package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// vanishOnceThenRealWorkerScript is the "claude" PATH stub this scenario
// installs: the manager role always execs the real fixture worker
// (realWorkerPath) — the manager itself must behave normally so it can
// plan and observe the run. Every OTHER role (implementer, reviewer)
// vanishes exactly like launchvanish_test.go's vanishingClaudeStub — its
// shebang process exits at once, before any corroboration could ever
// settle it — on its FIRST invocation only: markerPath records that the
// vanish has already happened, so the retried attempt this scenario
// drives to completion gets the real fixture worker instead of vanishing
// forever (which would exhaust the retry budget and fail the run, never
// reaching "the controller keeps running").
func vanishOnceThenRealWorkerScript(markerPath, realWorkerPath string) string {
	return "#!/bin/sh\n" +
		"if [ \"$HOP_ROLE\" = \"manager\" ]; then\n" +
		"  exec \"" + realWorkerPath + "\" \"$@\"\n" +
		"fi\n" +
		"if [ -f \"" + markerPath + "\" ]; then\n" +
		"  exec \"" + realWorkerPath + "\" \"$@\"\n" +
		"fi\n" +
		": > \"" + markerPath + "\"\n" +
		"exit 0\n"
}

// installVanishOnceThenRealWorkerStub installs vanishOnceThenRealWorkerScript
// as the server's "claude" PATH stub, replacing prepareServer's harmless
// echo stub. The real fixture worker binary is written alongside it (never
// installed as "claude" directly, unlike installFixtureWorkerAsClaudeStub)
// so the stub script can exec it by its own separate path.
func installVanishOnceThenRealWorkerStub(t *testing.T, server *testServer, realWorkerPath, markerPath string) {
	t.Helper()
	stub := filepath.Join(server.base, "bin", "claude")
	if err := os.WriteFile(stub, []byte(vanishOnceThenRealWorkerScript(markerPath, realWorkerPath)), 0o755); err != nil { //nolint:gosec // G306: the stub must be executable to reach the exec boundary.
		t.Fatalf("install vanish-once-then-real claude stub: %v", err)
	}
}

// TestRealProcessWorkerLaunchEndsBeforeSettlement is the registry's own
// 7c addition (LAUNCH-2, design section 4's launch-ended row, section 11
// "Layers"): a worker's harness ends right after exec — its pane closing
// with it before any corroboration can settle the claim — using the same
// technique as TestRealProcessManagerLaunchVanishesBeforeCorroboration
// (a shebang stub whose process is /bin/sh reading the script, so its
// argv can never carry the claim's executable and corroboration could
// never settle it regardless of timing), applied to the CHILD instead of
// the manager. Asserts the claim row's own state — never timing — proves
// the exec_failed path (the launch-ended row), not the interruption path;
// the task takes the budgeted needs-rework consequence; the manager is
// notified (the exact rendered notice line) and retries automatically;
// and the controller keeps running throughout, proved by driving the
// retried attempt (now the real fixture worker, the vanish stub's own
// one-shot marker already spent) all the way to a completed run.
func TestRealProcessWorkerLaunchEndsBeforeSettlement(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	realWorker := buildFixtureWorker(t, artifacts)
	markerPath := filepath.Join(artifacts.path, "worker-vanish-spent")
	installVanishOnceThenRealWorkerStub(t, server, realWorker, markerPath)
	server.start(t)

	scratchDir := artifacts.dir(t, "fixture-scratch")
	repo := newFeatureFixtureRepo(t, artifacts, server, "repo", featureFixtureOptions{
		ScratchDir: scratchDir, ReviewerBehavior: "reviewer-approve",
		MaxWorkers: 1, RetryLimit: 3, MessageWaitTimeout: "3s", MessageAttentionAfter: "30s",
	})

	brief := fixtureManagerBrief(scratchDir,
		[]fixtureManagerTask{{Label: "t1", Title: "Implement t1", Behavior: "worker-implement"}},
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
	// the vanish is deterministic (the process is gone before any
	// inspection could ever observe it), but this scenario still asserts
	// the settled state directly rather than inferring it from how long
	// anything took.
	var claimState string
	if !waitUntil(func() bool {
		claimState = fx.claimState(t, session1ID)
		return claimState == "exec_failed"
	}) {
		t.Fatalf("session %s launch claim never settled exec_failed; last observed state %q", session1ID, claimState)
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

	fx.requireTaskState(t, t1, "active")
	attempt2ID, attempt2Number := fx.currentAttempt(t, t1)
	if attempt2Number != 2 {
		t.Fatalf("retried attempt number = %d, want 2", attempt2Number)
	}
	if attempt2ID == attempt1ID {
		t.Fatal("the retried attempt has the same id as the vanished one")
	}

	// The retried attempt's own worker is the real fixture worker (the
	// vanish stub's one-shot marker is already spent): it implements and
	// submits normally, and the run completes — the strongest possible
	// confirmation that the controller kept running throughout.
	fx.requireTaskState(t, t1, "integrated")
	reviewTaskID := fx.requireReviewTask(t)
	fx.requireTaskState(t, reviewTaskID, "completed")
	if verdict, ok := fx.reviewVerdict(t, reviewTaskID); !ok || verdict != "approve" {
		t.Errorf("review verdict = %q (found=%v), want approve", verdict, ok)
	}
	fx.requireRunState(t, "completed")

	if fx.attemptCount(t, t1) != 2 {
		t.Errorf("task t1 has %d recorded attempts, want exactly 2", fx.attemptCount(t, t1))
	}
}
