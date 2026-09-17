package integration

import (
	"fmt"
	"testing"
)

// TestRealProcessWorkerLaunchEndsBeforeSettlement proves design section
// 4's launch-ended row for a worker (section 11's "Layers" table): a
// worker's harness ends right after exec — its pane closing
// with it before any corroboration can settle the claim. Unlike
// TestRealProcessManagerLaunchVanishesBeforeCorroboration's shebang stub
// (a process that is /bin/sh for its whole life, so its argv can never
// carry the claim's executable), this scenario needs the retried attempt
// to run the REAL fixture worker afterward, and a stub that exec-chains
// into a separately pathed binary makes the pane's observed foreground
// identity depend on when corroboration happens to sample the exec — the
// same executable and marker under a momentarily different pid is
// section 6's forking-wrapper topology, which fails closed to
// reconciling. So this installs the compiled fixture worker directly as
// "claude" (installFixtureWorkerAsClaudeStub, no exec chain, exactly like
// every other real-process scenario) and drives its own "worker-vanish-
// once" behavior instead: the fixture binary itself exits at once on the
// task's first attempt, keyed by a test-owned scratch marker, and
// behaves like worker-implement on the retried one. Asserts the claim
// row's own state — never timing — proves the exec_failed path (the
// launch-ended row), not the interruption path; the task takes the
// budgeted needs-rework consequence; the manager is notified (the exact
// rendered notice line) and retries automatically; no session in the run
// ever reconciles; and the controller keeps running throughout, proved
// by driving the retried attempt all the way to a completed run.
func TestRealProcessWorkerLaunchEndsBeforeSettlement(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	worker := buildFixtureWorker(t, artifacts)
	installFixtureWorkerAsClaudeStub(t, server, worker)
	server.start(t)

	scratchDir := artifacts.dir(t, "fixture-scratch")
	repo := newFeatureFixtureRepo(t, artifacts, server, "repo", featureFixtureOptions{
		ScratchDir: scratchDir, ReviewerBehavior: "reviewer-approve",
		MaxWorkers: 1, RetryLimit: 3, MessageWaitTimeout: "3s", MessageAttentionAfter: "30s",
	})

	brief := fixtureManagerBrief(scratchDir,
		[]fixtureManagerTask{{Label: "t1", Title: "Implement t1", Behavior: "worker-vanish-once"}},
		nil, "")
	fx := startFeatureRun(t, artifacts, server, repo, scratchDir, brief)

	t1 := fx.requireTaskBySeq(t, 1)
	if !waitUntil(func() bool { return fx.attemptCount(t, t1) > 0 }) {
		t.Fatalf("no attempt ever reserved for task %s", t1)
	}
	attempt1ID, attempt1Number := fx.currentAttempt(t, t1)
	if attempt1Number != 1 {
		t.Fatalf("first attempt number = %d, want 1", attempt1Number)
	}
	session1ID := fx.sessionForAttempt(t, attempt1ID)

	// The claim row's own state, not timing, proves the exec_failed path:
	// this process is claude-identified for a few milliseconds before its
	// own os.Exit (it spawns the MCP stand-in and reads its instructions
	// file first), so a corroboration pass landing in that narrow window
	// could in principle settle it execed instead — a flake, not a false
	// pass. The same window exposes the MCP stand-in's own fork-before-
	// exec instant, which can present the forking-wrapper topology
	// section 6 fails closed on; the reconciling guard below catches that
	// early rather than reporting a generic "last observed state
	// exec_pending" after the full poll bound.
	var claimState, reconciledSession string
	if !waitUntil(func() bool {
		if reconciledSession == "" {
			reconciledSession = fx.reconciledSessionID(t)
		}
		claimState = fx.claimState(t, session1ID)
		return claimState == "exec_failed" || reconciledSession != ""
	}) {
		t.Fatalf("session %s launch claim never settled exec_failed; last observed state %q", session1ID, claimState)
	}
	if reconciledSession != "" {
		t.Fatalf("session %s entered reconciling before the vanished launch could settle exec_failed; last observed claim state %q", reconciledSession, claimState)
	}
	claimError := fx.scalar(t, fmt.Sprintf("SELECT error FROM launch_claims WHERE session_id = '%s';", session1ID))
	if claimError != launchEndedClaimReason {
		t.Errorf("session %s launch claim error = %q, want the launch-ended reason %q", session1ID, claimError, launchEndedClaimReason)
	}

	// The attempt settles as a terminal, non-completed outcome (never
	// merely "interrupted", which is stop's own cause) and the task takes
	// the budgeted needs-rework consequence.
	fx.requireAttemptState(t, attempt1ID, "failed")
	fx.requireTransitionAt(t, "task", t1, "needs-rework")

	// The manager is notified with the exact rendered notice line
	// (workerinterruption_test.go's own constant: renderTaskNotice is one
	// shared template regardless of WHY a task reached needs-rework) and
	// retries automatically.
	noticeID := fx.requireManagerNoticeFirstLine(t, featureRunTimeout, workerInterruptionNoticeFirstLine)
	if noticeID == "" {
		t.Fatal("requireManagerNoticeFirstLine returned an empty message id")
	}

	// The controller kept running through all of this: its own process
	// never exited, checked directly right after the retry is observed —
	// not merely inferred from the run eventually completing below.
	select {
	case <-fx.controller.leaderExited:
		t.Fatal("the controller exited after the worker's launch vanished; want it to keep running")
	default:
	}

	fx.requireTaskStateNeverReconciling(t, t1, "active")
	attempt2ID, attempt2Number := fx.currentAttempt(t, t1)
	if attempt2Number != 2 {
		t.Fatalf("retried attempt number = %d, want 2", attempt2Number)
	}
	if attempt2ID == attempt1ID {
		t.Fatal("the retried attempt has the same id as the vanished one")
	}

	// The retried attempt's own worker is the same "claude"-installed
	// fixture binary (worker-vanish-once's marker is already spent for
	// this task): it implements and submits normally, and the run
	// completes — the strongest possible confirmation that the controller
	// kept running throughout. Every wait below also fails at once, never
	// only after the full featureRunTimeout, if any session in the run
	// goes reconciling.
	fx.requireTaskStateNeverReconciling(t, t1, "integrated")
	reviewTaskID := fx.requireReviewTask(t)
	fx.requireTaskStateNeverReconciling(t, reviewTaskID, "completed")
	if verdict, ok := fx.reviewVerdict(t, reviewTaskID); !ok || verdict != "approve" {
		t.Errorf("review verdict = %q (found=%v), want approve", verdict, ok)
	}
	fx.requireRunState(t, "completed")

	if fx.attemptCount(t, t1) != 2 {
		t.Errorf("task t1 has %d recorded attempts, want exactly 2", fx.attemptCount(t, t1))
	}
}
