package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	}))
	for _, quote := range []string{
		"/hop " + GrammarVerbTaskCreate + " --title",
		"/hop " + GrammarVerbPlanClose,
		"/hop " + GrammarVerbMsgWait,
		"/hop " + GrammarVerbMsgAck + " <message-uuid>",
		"/hop " + GrammarVerbMsgSend + " --kind answer",
		"/hop " + GrammarVerbTaskRetry + " <task-uuid>",
		GrammarRefusalLine("<reason-token>"),
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
