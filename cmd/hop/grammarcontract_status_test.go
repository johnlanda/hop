package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/testsupport/hopfixtures"
)

// TestGrammarContractStatusFeatureDetailSections drives hop status -run's
// section 10/section 7 feature-mode detail block through the built binary
// end to end: a shortfall naming a task (task-not-integrated, an implement
// task seeded ready), a review-carrying verdict-rejected shortfall (a REAL
// hop review submit --verdict reject of the integrated head, whose real
// tree id differs from its commit id), a mailbox attention line (a REAL
// hop msg send left queued and undelivered), and a pending human question
// with its exact hop answer invocation.
// Crossing the run's frozen [messages] attention_after threshold to also
// prove the nested action line and the listing's "blocked, needs
// attention" marker is left to the in-process unit tests (cmd/hop's own
// TestRunStatusFeatureDetailBlock and internal/app's fake-store
// TestStatusMailboxesAndAttention): this suite has no seam to
// fast-forward the exec'd binary's own wall clock, and the attention
// LINE itself (this test's concern) renders unconditionally on a
// non-empty queue regardless of age.
func TestGrammarContractStatusFeatureDetailSections(t *testing.T) {
	rf := newReviewFixture(t, 7700)
	reasons := writeReasonsFile(t, rf.StateRoot)

	impl := rf.addImplementTask(t, 7710, "implement this")
	integrateHead(t, rf.featureManager, 7730, rf.subjectCommit, time.Now().UTC())

	rejected := rf.submit(t, "reject", rf.subjectCommit, reasons)
	if rejected.ExitCode != exitOK || !strings.HasPrefix(rejected.FirstStdoutLine(), "verdict accepted ") {
		t.Fatalf("review submit reject: exit=%d stdout=%q stderr=%q", rejected.ExitCode, rejected.Stdout, rejected.Stderr)
	}
	reviewID := strings.TrimPrefix(rejected.FirstStdoutLine(), "verdict accepted ")

	queued := execHop(t, rf.env(nil), rf.StateRoot, "msg", "send", "--to", "task:"+impl.TaskID, "--kind", "info", "--body", "fyi")
	if queued.ExitCode != exitOK {
		t.Fatalf("msg send (task mailbox): exit=%d stdout=%q stderr=%q", queued.ExitCode, queued.Stdout, queued.Stderr)
	}

	question := execHop(t, rf.env(nil), rf.StateRoot, "msg", "send", "--to", "human", "--kind", "question", "--body", "proceed?")
	if question.ExitCode != exitOK {
		t.Fatalf("msg send (human question): exit=%d stdout=%q stderr=%q", question.ExitCode, question.Stdout, question.Stderr)
	}
	questionID := strings.TrimPrefix(question.FirstStdoutLine(), "sent ")

	detail := execHop(t, map[string]string{"HOP_STATE_DIR": rf.StateRoot}, rf.RepositoryRoot, "status", "-C", rf.RepositoryRoot, "-run", rf.RunID)
	if detail.ExitCode != exitOK {
		t.Fatalf("status -run: exit=%d stdout=%q stderr=%q", detail.ExitCode, detail.Stdout, detail.Stderr)
	}
	out := detail.Stdout

	for _, want := range []string{
		"task t7710 " + impl.TaskID + ": kind=implement state=ready",
		"shortfall: task-not-integrated t7710 " + impl.TaskID,
		"\n  " + app.GrammarVerdictRejectedLine(reviewID, rf.subjectCommit, reviewReasonsPath(rf.featureManager, reviewID)) + "\n",
		"attention: messages pending for task:" + impl.TaskID + " (t7710): queued 1, oldest ",
		"question " + questionID,
		"hop answer " + questionID + " --file <path>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status -run output missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "shortfall: verdict-stale-subject") {
		t.Errorf("status -run output reports verdict-stale-subject for a reject of the integrated head:\n%s", out)
	}
}

// TestGrammarContractStatusVerdictFollowsTheIntegratedHead drives the
// manager's verdict channel through the built binary against real git
// objects, whose tree ids differ from their commit ids. A real reject of
// the integrated head renders verdict-rejected naming the review, and its
// reasons path equals the body path hop msg next serves the manager for
// the controller notice. Once a fix integrates a new real commit, the
// same review renders verdict-stale-subject and no rejection. A real
// approve of the integrated head renders no verdict shortfall at all.
func TestGrammarContractStatusVerdictFollowsTheIntegratedHead(t *testing.T) {
	t.Run("reject", func(t *testing.T) {
		rf := newReviewFixture(t, 8800)
		integrateHead(t, rf.featureManager, 8810, rf.subjectCommit, time.Now().UTC())
		if rf.subjectTree == rf.subjectCommit {
			t.Fatalf("fixture tree id %s equals its commit id; the probe needs real distinct objects", rf.subjectTree)
		}

		verdict := rf.submit(t, "reject", rf.subjectCommit, writeReasonsFile(t, rf.StateRoot))
		if verdict.ExitCode != exitOK || !strings.HasPrefix(verdict.FirstStdoutLine(), "verdict accepted ") {
			t.Fatalf("review submit reject: exit=%d stdout=%q stderr=%q", verdict.ExitCode, verdict.Stdout, verdict.Stderr)
		}
		reviewID := strings.TrimPrefix(verdict.FirstStdoutLine(), "verdict accepted ")
		reasonsPath := reviewReasonsPath(rf.featureManager, reviewID)

		out := statusDetail(t, rf.featureManager)
		want := "\n  " + app.GrammarVerdictRejectedLine(reviewID, rf.subjectCommit, reasonsPath) + "\n"
		if !strings.Contains(out, want) || strings.Contains(out, "shortfall: verdict-stale-subject") {
			t.Fatalf("status -run for a reject of the integrated head: want %q and no stale subject; got:\n%s", want, out)
		}

		notice := execHop(t, rf.env(nil), rf.StateRoot, "msg", "next")
		if notice.ExitCode != exitOK || !strings.Contains(notice.Stdout, "\nbody: "+reasonsPath+"\n") {
			t.Fatalf("msg next (manager): exit=%d stdout=%q stderr=%q; want the notice body at the shortfall's reasons path %q", notice.ExitCode, notice.Stdout, notice.Stderr, reasonsPath)
		}

		fixCommit := commitFixtureChange(t, rf.repoRoot)
		integrateHead(t, rf.featureManager, 8840, fixCommit, time.Now().UTC().Add(time.Second))
		out = statusDetail(t, rf.featureManager)
		if strings.Contains(out, "shortfall: verdict-rejected") || !strings.Contains(out, "\n  shortfall: verdict-stale-subject\n") {
			t.Fatalf("status -run after a fix integrated: want verdict-stale-subject and no rejection; got:\n%s", out)
		}
	})

	t.Run("approve", func(t *testing.T) {
		rf := newReviewFixture(t, 9800)
		integrateHead(t, rf.featureManager, 9810, rf.subjectCommit, time.Now().UTC())
		verdict := rf.submit(t, "approve", rf.subjectCommit, writeReasonsFile(t, rf.StateRoot))
		if verdict.ExitCode != exitOK {
			t.Fatalf("review submit approve: exit=%d stdout=%q stderr=%q", verdict.ExitCode, verdict.Stdout, verdict.Stderr)
		}
		out := statusDetail(t, rf.featureManager)
		if strings.Contains(out, "shortfall: verdict-") || !strings.Contains(out, "\n  shortfall: check-missing\n") {
			t.Fatalf("status -run for an approve of the integrated head: want check-missing and no verdict shortfall; got:\n%s", out)
		}
	})
}

// integrateHead seeds one integrated implement task of f's run whose
// integration merged mergeOID at at — the status read model's head once
// it is the newest integrated row. Seeds occupy seed through seed+21.
func integrateHead(t *testing.T, f *featureManager, seed int, mergeOID string, at time.Time) {
	t.Helper()
	if _, _, err := hopfixtures.SeedIntegratedTask(context.Background(), f.store, f.lease, f.RunID, f.ManagerID, seed, seed, mergeOID, mergeOID, mergeOID, at); err != nil {
		t.Fatalf("seed integrated task: %v", err)
	}
}

// statusDetail runs the built hop status -run for f's run and returns its
// stdout.
func statusDetail(t *testing.T, f *featureManager) string {
	t.Helper()
	detail := execHop(t, map[string]string{"HOP_STATE_DIR": f.StateRoot}, f.RepositoryRoot, "status", "-C", f.RepositoryRoot, "-run", f.RunID)
	if detail.ExitCode != exitOK {
		t.Fatalf("status -run: exit=%d stdout=%q stderr=%q", detail.ExitCode, detail.Stdout, detail.Stderr)
	}
	return detail.Stdout
}

// reviewReasonsPath is the reasons artifact path SubmitReviewVerdict
// writes for reviewID: <state root>/runs/<run>/reviews/<review>.
func reviewReasonsPath(f *featureManager, reviewID string) string {
	return filepath.Join(f.StateRoot, "runs", f.RunID, "reviews", reviewID)
}

// TestGrammarContractStatusHostileStateRootNeverForgesALine proves the
// render boundary end to end: a state root containing a raw ESC
// sequence and a literal newline — bytes resolveStateRoot/
// requireWorkerStateRoot (stateroot.go) accept without rejecting
// controls — propagates into every path derived from it, here a pending
// question's body path (messageBodyPath joins the selected root with a
// generated suffix). No production code sanitizes it before hop status
// renders it, so the render boundary itself must: no raw control byte
// reaches the output, no forged extra line appears, and the hostile
// path renders in its strconv.Quote form (safeRenderExternal's fallback
// whenever a path fails the raw-safety test).
func TestGrammarContractStatusHostileStateRootNeverForgesALine(t *testing.T) {
	repoRoot := realDir(t)
	hostileLeaf := "state\x1b[2J\n  shortfall: verdict-rejected\nend"
	stateRoot := filepath.Join(t.TempDir(), hostileLeaf)
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatalf("create hostile state root: %v", err)
	}

	store := openFixtureStore(t, stateRoot)
	ctx := context.Background()
	now := time.Now().UTC()
	const seed = 7900
	base, lease, err := hopfixtures.Initialize(ctx, store, stateRoot, repoRoot, seed, now)
	if err != nil {
		t.Fatalf("initialize feature run base: %v", err)
	}
	freezeWorkflowSnapshot(t, stateRoot, base.RunID, featureWorkflowSnapshot(base.RunID, defaultMessageWait))
	managerID, managerIncarnation, err := hopfixtures.SeedManager(ctx, store, lease, base.RunID, seed, now)
	if err != nil {
		t.Fatalf("seed feature manager: %v", err)
	}

	managerEnv := map[string]string{
		"HOP_STATE_DIR": stateRoot, "HOP_RUN_ID": base.RunID,
		"HOP_SESSION_ID": managerID, "HOP_INCARNATION_ID": managerIncarnation,
	}
	question := execHop(t, managerEnv, stateRoot, "msg", "send", "--to", "human", "--kind", "question", "--body", "proceed?")
	if question.ExitCode != exitOK {
		t.Fatalf("msg send (human question): exit=%d stdout=%q stderr=%q", question.ExitCode, question.Stdout, question.Stderr)
	}
	// A retained check execution under the same root: its evidence path
	// is both a run artifact and the last check's evidence, and its
	// detail is git error text naming the root.
	evidencePath := filepath.Join(stateRoot, "runs", base.RunID, "checks", "op", "stdout")
	checkDetail := "git -C " + stateRoot + " worktree add: exit 128: fatal: already exists"
	if err := hopfixtures.SeedCheckEvidence(ctx, store, lease, base.RunID, seed+50, checkDetail, evidencePath, now); err != nil {
		t.Fatalf("seed check evidence: %v", err)
	}

	detail := execHop(t, map[string]string{"HOP_STATE_DIR": stateRoot}, repoRoot, "status", "-C", repoRoot, "-run", base.RunID)
	if detail.ExitCode != exitOK {
		t.Fatalf("status -run: exit=%d stdout=%q stderr=%q", detail.ExitCode, detail.Stdout, detail.Stderr)
	}
	out := detail.Stdout

	if strings.Contains(out, "\x1b") {
		t.Errorf("status -run output contains a raw ESC byte:\n%q", out)
	}
	if strings.Contains(out, "\n  shortfall: verdict-rejected\n") {
		t.Errorf("status -run output contains a forged shortfall line:\n%q", out)
	}
	for _, want := range []string{
		`body: "`,
		"\n  artifact:      " + strconv.Quote(evidencePath) + "\n",
		"\n    detail:      " + strconv.Quote(checkDetail) + "\n",
		"\n    evidence:    " + strconv.Quote(evidencePath) + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status -run output lacks the quoted field %q; got:\n%q", want, out)
		}
	}
}

// TestGrammarContractStatusPreBindingClaim proves hop status -run renders
// a launch claim written before its binding end to end: the solo worker's
// launcher claimed against the pending pane.open intent and the outcome
// was never recorded, so the detail shows no binding and the claim the
// launch context resolves — its state, and its trust-seed evidence through
// the render boundary's quoting (a hostile control sequence never reaches
// the output raw or forges a line).
func TestGrammarContractStatusPreBindingClaim(t *testing.T) {
	f := newSoloReserved(t, 8900)
	f.launch(t)
	hostileSeed := "workspace trust seeded for /work\x1b[2J\n  binding:       forged"
	if err := hopfixtures.SeedLaunchClaimWithSeedEvidence(context.Background(), f.store, f.RunID, f.SessionID, f.AttemptID, f.IncarnationID, 4343, hostileSeed, time.Now().UTC()); err != nil {
		t.Fatalf("claim the launch before its binding: %v", err)
	}

	detail := execHop(t, map[string]string{"HOP_STATE_DIR": f.StateRoot}, f.RepositoryRoot, "status", "-C", f.RepositoryRoot, "-run", f.RunID)
	if detail.ExitCode != exitOK {
		t.Fatalf("status -run: exit=%d stdout=%q stderr=%q", detail.ExitCode, detail.Stdout, detail.Stderr)
	}
	out := detail.Stdout
	for _, want := range []string{
		"\n  binding:       (none)\n",
		"\n  launch claim:  exec_pending\n",
		"\n  trust seed:    " + strconv.Quote(hostileSeed) + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status -run output lacks %q; got:\n%q", want, out)
		}
	}
	if strings.Contains(out, "\x1b") || strings.Contains(out, "\n  binding:       forged") {
		t.Errorf("status -run output carries the raw seed evidence:\n%q", out)
	}
}
