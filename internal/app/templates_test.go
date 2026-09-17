package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// tmplArtifacts is a minimal in-memory ArtifactStore for the freeze test
// (this file is an in-package test; the shared fakes live in the external
// test package and are not reachable from here).
type tmplArtifacts struct {
	files    map[string][]byte
	writeErr error
}

func (a *tmplArtifacts) WriteArtifact(_ context.Context, path string, content []byte) error {
	if a.writeErr != nil {
		return a.writeErr
	}
	stored := make([]byte, len(content))
	copy(stored, content)
	a.files[path] = stored
	return nil
}

func (a *tmplArtifacts) ReadArtifact(_ context.Context, path string) ([]byte, error) {
	content, ok := a.files[path]
	if !ok {
		return nil, fmt.Errorf("app_test: no artifact at %s", path)
	}
	return content, nil
}

// TestCribRetryableLinesPerVerb pins which crib sections list which
// retryable first line: the run-not-running line under exactly the verbs
// whose accepting transaction checks the run state (task create, task
// retry, plan close, msg send), and nowhere else; the attempt-not-running
// and undelivered-messages lines, byte-identical to the grammar
// constants, under exactly the two submit verbs whose typed transient
// reason selects between them.
func TestCribRetryableLinesPerVerb(t *testing.T) {
	sections := map[string]string{}
	for _, part := range strings.Split(string(renderWorkerProtocolCrib()), "\n## hop ")[1:] {
		heading, body, _ := strings.Cut(part, "\n")
		sections[heading] = body
	}
	retryable := func(line string) string { return "Retryable: `" + line + "`" }
	want := map[string][]string{
		GrammarVerbResultSubmit:                             {retryable(GrammarTransientNotRunningLine), retryable(GrammarTransientUndeliveredLine)},
		GrammarVerbMsgNext + " / hop " + GrammarVerbMsgWait: nil,
		GrammarVerbMsgShow + " <message-uuid>":              nil,
		GrammarVerbMsgAck + " <message-uuid>":               nil,
		GrammarVerbMsgSend:                                  {retryable(GrammarTransientRunNotRunningLine)},
		GrammarVerbTaskCreate + " (manager only)":           {retryable(GrammarTransientRunNotRunningLine)},
		GrammarVerbTaskRetry + " (manager only)":            {retryable(GrammarTransientRunNotRunningLine)},
		GrammarVerbPlanClose + " (manager only)":            {retryable(GrammarTransientRunNotRunningLine)},
		GrammarVerbReviewSubmit + " (reviewer only)":        {retryable(GrammarTransientNotRunningLine), retryable(GrammarTransientUndeliveredLine)},
	}
	if len(sections) != len(want) {
		t.Fatalf("crib sections = %d, want %d", len(sections), len(want))
	}
	for heading, lines := range want {
		body, ok := sections[heading]
		if !ok {
			t.Errorf("crib has no section %q", heading)
			continue
		}
		var got []string
		for line := range strings.SplitSeq(body, "\n") {
			if rest, found := strings.CutPrefix(line, "Retryable: `"); found {
				quoted, _, _ := strings.Cut(rest, "`")
				got = append(got, retryable(quoted))
			}
		}
		if strings.Join(got, "\n") != strings.Join(lines, "\n") {
			t.Errorf("section %q retryable lines = %q, want %q", heading, got, lines)
		}
	}

	// hop result submit and hop review submit print the same two retry
	// lines for the same two typed reasons, so a worker reads the same
	// whole crib line, instruction included, under either verb.
	submitRetryable := "Retryable: `" + GrammarTransientNotRunningLine + "` — wait briefly, rerun the same command.\n" +
		"Retryable: `" + GrammarTransientUndeliveredLine + "`.\n"
	for _, heading := range []string{GrammarVerbResultSubmit, GrammarVerbReviewSubmit + " (reviewer only)"} {
		if !strings.Contains(sections[heading], submitRetryable) {
			t.Errorf("section %q = %q, want the retry lines %q verbatim", heading, sections[heading], submitRetryable)
		}
	}
}

// TestTemplatesQuoteGrammar is the template half of the golden-grammar
// countermeasure (design section 11, L1569): every protocol line or verb
// a template renders must be a QUOTATION of the grammar constant set, so
// this test asserts each rendered artifact contains the exact rendered
// grammar strings — a drift in either side breaks the containment.
func TestTemplatesQuoteGrammar(t *testing.T) {
	crib := string(renderWorkerProtocolCrib())
	cribQuotes := []string{
		GrammarRefusalLine("<reason-token>"),
		GrammarResultAcceptedLine("<result-uuid>"),
		GrammarResultDuplicateLine("<result-uuid>"),
		GrammarTransientNotRunningLine,
		GrammarTransientUndeliveredLine,
		GrammarTransientRunNotRunningLine,
		GrammarMsgNoneLine,
		GrammarMsgWaitNoneLine(50 * time.Second),
		GrammarMessageLine("<message-uuid>", "<kind>", "<sender>", "<reply-to-uuid>", "<relay-of-uuid>", "<origin-uuid>"),
		GrammarBodyLine("<absolute body path>"),
		GrammarAckHintLine("<message-uuid>"),
		GrammarMessageShowLine("<message-uuid>", "<kind>", "<sender>", "<address>", "<reply-to-uuid>", "<relay-of-uuid>", 0),
		GrammarRefusalLine(GrammarReasonNotFound),
		GrammarAckAcceptedLine("<message-uuid>"),
		GrammarAckDuplicateLine("<message-uuid>"),
		GrammarSentLine("<message-uuid>"),
		GrammarSendDuplicateLine("<message-uuid>"),
		GrammarTaskCreatedLine("<task-uuid>", 0),
		GrammarTaskCreateDuplicateLine("<task-uuid>", 0),
		GrammarRetryAcceptedLine(0, 0),
		GrammarRetryDuplicateLine(0, 0),
		GrammarPlanClosedLine,
		GrammarPlanCloseDuplicateLine,
		GrammarVerdictAcceptedLine("<review-uuid>"),
		GrammarVerdictDuplicateLine("<review-uuid>"),
		"## hop " + GrammarVerbResultSubmit,
		"## hop " + GrammarVerbMsgNext + " / hop " + GrammarVerbMsgWait,
		"## hop " + GrammarVerbMsgShow,
		"## hop " + GrammarVerbMsgAck,
		"## hop " + GrammarVerbMsgSend,
		"## hop " + GrammarVerbTaskCreate,
		"## hop " + GrammarVerbTaskRetry,
		"## hop " + GrammarVerbPlanClose,
		"## hop " + GrammarVerbReviewSubmit,
		GrammarReasonNotFound, GrammarReasonUnauthorized, GrammarReasonMalformed,
		GrammarReasonConflicting, GrammarReasonStale, GrammarReasonNotDelivered,
		GrammarReasonRunNotAccepting, GrammarReasonMailboxClosed, GrammarReasonNotManager,
		GrammarReasonNotReviewer, GrammarReasonDependencyCycle, GrammarReasonEmptyPlan,
		GrammarReasonRetryNotTerminal, GrammarReasonRetryLimit, GrammarReasonSubjectMismatch,
	}
	for _, quote := range cribQuotes {
		if !strings.Contains(crib, quote) {
			t.Errorf("crib does not quote %q", quote)
		}
	}

	manager := string(renderManagerAssignment(&managerAssignmentFields{
		RunID: "r", Brief: "b", AssignmentPath: "/a.md", RolePath: "/r.md", CribPath: "/c.md", HOPPath: "/hop",
		RepositoryRoot: "/repo",
	}))
	for _, quote := range []string{
		"/hop " + GrammarVerbTaskCreate + " --title",
		"/hop " + GrammarVerbPlanClose,
		"/hop " + GrammarVerbMsgWait,
		"/hop " + GrammarVerbMsgAck + " <message-uuid>",
		"/hop " + GrammarVerbMsgSend + " --kind answer",
		"/hop " + GrammarVerbTaskRetry + " <task-uuid>",
		GrammarRefusalLine("<reason-token>"),
		"'/hop' status -C '/repo' -run 'r'",
		GrammarShortfallVerdictRejected,
		"naming " + GrammarShortfallEvidenceInconsistent + " is never a rejection",
	} {
		if !strings.Contains(manager, quote) {
			t.Errorf("manager assignment does not quote %q", quote)
		}
	}

	task := string(renderTaskAssignment(&taskAssignmentFields{
		RunID: "r", TaskID: "t", AttemptID: "a", TaskSeq: 1, AttemptNumber: 1,
		Title: "T", InstructionsPath: "/i.md", RolePath: "/r.md", AssignmentPath: "/a.md", HOPPath: "/hop",
	}))
	if !strings.Contains(task, "/hop "+GrammarVerbResultSubmit+" --summary") {
		t.Errorf("task assignment does not quote the submit verb")
	}
	if !strings.Contains(task, GrammarRefusalLine("<reason-token>")) {
		t.Errorf("task assignment does not quote the refusal shape")
	}

	review := string(renderReviewAssignment(&reviewAssignmentFields{
		RunID: "r", TaskID: "t", AttemptID: "a", TaskSeq: 2, AttemptNumber: 1,
		SubjectCommitOID: "headoid", SubjectTreeOID: "treeoid", DiffBaseOID: "baseoid",
		RolePath: "/r.md", AssignmentPath: "/a.md", HOPPath: "/hop",
	}))
	for _, quote := range []string{
		"/hop " + GrammarVerbReviewSubmit + " --verdict <approve|reject> --subject headoid",
		GrammarRefusalLine(GrammarReasonSubjectMismatch),
		"Diff scope: baseoid..headoid",
	} {
		if !strings.Contains(review, quote) {
			t.Errorf("review assignment does not quote %q", quote)
		}
	}
}

// TestPosixShellQuoteRoundTrips proves posixShellQuote's output, fed back
// through a real POSIX shell's own word-splitting and expansion, always
// reproduces the original string byte for byte — the harmless recorder
// here is the trusted printf builtin/binary, exactly as the reviewer's
// own reproduction used printf in place of hop. Every hostile class the
// review named is covered, in both a repository-root-shaped and a
// hop-path-shaped string (posixShellQuote does not know or care which
// role its argument plays).
func TestPosixShellQuoteRoundTrips(t *testing.T) {
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("no /bin/sh available")
	}
	cases := []struct {
		name, value string
	}{
		{"plain", "/repo/checkout"},
		{"spaces (root-shaped)", "/repo with spaces/checkout"},
		{"spaces (hop-path-shaped)", "/opt/hop install/bin/hop"},
		{"single quote", "it's here"},
		{"double quote", `say "hi"`},
		{"dollar substitution", "$(printf INJECTED)"},
		{"backticks", "`printf INJECTED`"},
		{"semicolon", "a; printf INJECTED"},
		{"newline (root-shaped)", "/repo\nwith/a/newline"},
		{"newline (hop-path-shaped)", "/opt/hop\nbin/hop"},
		{"everything at once", "it's \"/repo\" $(printf INJECTED) `printf INJECTED`; printf INJECTED\ndone"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			quoted := posixShellQuote(tc.value)
			out, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", "printf '%s' "+quoted).Output() //nolint:gosec // the quoted value is exactly what is under test; no other input reaches this command.
			if err != nil {
				t.Fatalf("sh -c: %v", err)
			}
			if string(out) != tc.value {
				t.Errorf("round-trip = %q, want %q", out, tc.value)
			}
		})
	}
}

// TestManagerAssignmentVerdictCommandSurvivesShell renders the actual
// verdict-channel command line with a hostile repository root and a
// hostile (but filesystem-legal) hop executable path, then executes it
// for real against a harmless argv-recording script standing in for hop
// — proving the whole rendered line, not just the quoting primitive in
// isolation, tokenizes back into exactly the original arguments.
func TestManagerAssignmentVerdictCommandSurvivesShell(t *testing.T) {
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("no /bin/sh available")
	}
	dir := t.TempDir()
	recorderName := `it's "hop" $(printf INJECTED) ` + "`printf INJECTED`" + `; printf INJECTED bin`
	recorder := filepath.Join(dir, recorderName)
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\0' \"$a\"; done\n"
	if err := os.WriteFile(recorder, []byte(script), 0o755); err != nil { //nolint:gosec // a test fixture script, deliberately executable.
		t.Fatal(err)
	}

	// No embedded newline here: this test locates the rendered command by
	// splitting the assignment into text lines, which an argument's own
	// raw newline would legitimately spread across — TestPosixShellQuoteRoundTrips
	// already proves posixShellQuote handles a newline correctly in
	// isolation, for both a root-shaped and a hop-path-shaped string.
	root := `/repo with spaces/it's "quoted" $(printf INJECTED)/` + "`printf INJECTED`" + ";printf INJECTED"
	runID := "11111111-1111-4111-8111-111111111111"
	assignment := string(renderManagerAssignment(&managerAssignmentFields{
		HOPPath: recorder, RepositoryRoot: root, RunID: runID,
	}))
	var line string
	for _, l := range strings.Split(assignment, "\n") {
		if strings.Contains(l, " status -C ") {
			line = strings.TrimSpace(l)
		}
	}
	if line == "" {
		t.Fatalf("no status command line found in assignment:\n%s", assignment)
	}

	out, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", line).Output() //nolint:gosec // line is the exact rendered instruction under test.
	if err != nil {
		t.Fatalf("sh -c %q: %v", line, err)
	}
	got := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	want := []string{"status", "-C", root, "-run", runID}
	if !slices.Equal(got, want) {
		t.Errorf("recorded argv = %q, want %q", got, want)
	}
}

func TestFreezeWorkflowArtifacts(t *testing.T) {
	runID, err := identity.ParseRunID(ebRunID)
	if err != nil {
		t.Fatal(err)
	}
	validPolicy := func() RunPolicy {
		p := RunPolicy{
			Harness:      HarnessClaude,
			WorkflowMode: WorkflowModeFeature,
			ManagerRole:  "/repo/.herdr-orchestrator/roles/orchestrator.md",
			ImplementerRole: "/repo/.herdr-orchestrator/roles/" +
				"implementer.md",
			ReviewerRole: "/repo/.herdr-orchestrator/roles/reviewer.md",
		}
		ApplyWorkflowDefaults(&p)
		return p
	}
	seedRoleFiles := func() *tmplArtifacts {
		return &tmplArtifacts{files: map[string][]byte{
			"/repo/.herdr-orchestrator/roles/orchestrator.md": []byte("# manager role\n"),
			"/repo/.herdr-orchestrator/roles/implementer.md":  []byte("# implementer role\n"),
			"/repo/.herdr-orchestrator/roles/reviewer.md":     []byte("# reviewer role\n"),
		}}
	}

	t.Run("copies the role files with digests, writes the crib, derives the branch", func(t *testing.T) {
		artifacts := seedRoleFiles()
		c := &Controller{Artifacts: artifacts}
		req := WorkflowFreezeRequest{Policy: validPolicy(), RunID: runID, RunSeq: 7, StateRoot: "/state"}

		snapshot, err := c.FreezeWorkflowArtifacts(context.Background(), &req)
		if err != nil {
			t.Fatalf("FreezeWorkflowArtifacts: %v", err)
		}

		if snapshot.Mode != WorkflowModeFeature || snapshot.IntegrationBranch != "hop/r7/integration" {
			t.Errorf("snapshot = mode %q branch %q", snapshot.Mode, snapshot.IntegrationBranch)
		}
		if snapshot.MaxWorkers != 2 || snapshot.RetryLimit != 3 || snapshot.MessageAttention != 120*time.Second || snapshot.MessageWait != 50*time.Second {
			t.Errorf("snapshot policy fields = %+v", snapshot)
		}
		if snapshot.ReviewerHarness != HarnessClaude {
			t.Errorf("reviewer harness = %q", snapshot.ReviewerHarness)
		}
		roles := []struct{ name, path, digest, content string }{
			{"manager", snapshot.ManagerRolePath, snapshot.ManagerRoleDigest, "# manager role\n"},
			{"implementer", snapshot.ImplementerRolePath, snapshot.ImplementerRoleDigest, "# implementer role\n"},
			{"reviewer", snapshot.ReviewerRolePath, snapshot.ReviewerRoleDigest, "# reviewer role\n"},
		}
		for _, role := range roles {
			wantPath := "/state/runs/" + ebRunID + "/artifacts/roles/" + role.name + ".md"
			if role.path != wantPath {
				t.Errorf("%s path = %q, want %q", role.name, role.path, wantPath)
			}
			if got := artifacts.files[wantPath]; string(got) != role.content {
				t.Errorf("%s frozen copy = %q, want the source bytes", role.name, got)
			}
			if role.digest != sha256Hex(role.content) {
				t.Errorf("%s digest = %q", role.name, role.digest)
			}
		}
		crib := artifacts.files["/state/runs/"+ebRunID+"/artifacts/worker-protocol.md"]
		if !bytes.Equal(crib, renderWorkerProtocolCrib()) {
			t.Errorf("crib bytes differ from renderWorkerProtocolCrib()")
		}
	})

	refusals := []struct {
		name    string
		mutate  func(req *WorkflowFreezeRequest, artifacts *tmplArtifacts)
		wantErr string
	}{
		{
			name: "an invalid feature policy is refused by the shared validator",
			mutate: func(req *WorkflowFreezeRequest, _ *tmplArtifacts) {
				req.Policy.ReviewerRole = ""
			},
			wantErr: "roles.reviewer.instructions is required",
		},
		{
			name: "a relative state root is refused",
			mutate: func(req *WorkflowFreezeRequest, _ *tmplArtifacts) {
				req.StateRoot = "state"
			},
			wantErr: "state root is not absolute",
		},
		{
			name: "a missing role file is refused with the role named",
			mutate: func(_ *WorkflowFreezeRequest, artifacts *tmplArtifacts) {
				delete(artifacts.files, "/repo/.herdr-orchestrator/roles/implementer.md")
			},
			wantErr: "read the implementer role instructions",
		},
		{
			name: "an empty role file is refused",
			mutate: func(_ *WorkflowFreezeRequest, artifacts *tmplArtifacts) {
				artifacts.files["/repo/.herdr-orchestrator/roles/reviewer.md"] = nil
			},
			wantErr: "reviewer role instructions file is empty",
		},
		{
			name: "a failed freeze write is refused",
			mutate: func(_ *WorkflowFreezeRequest, artifacts *tmplArtifacts) {
				artifacts.writeErr = errors.New("disk full")
			},
			wantErr: "freeze the manager role artifact",
		},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			artifacts := seedRoleFiles()
			c := &Controller{Artifacts: artifacts}
			req := WorkflowFreezeRequest{Policy: validPolicy(), RunID: runID, RunSeq: 1, StateRoot: "/state"}
			tc.mutate(&req, artifacts)

			if _, err := c.FreezeWorkflowArtifacts(context.Background(), &req); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
