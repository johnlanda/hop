package integration

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRealProcessReconcilingChildLaunchAcrossResume proves resume's
// in-flight rule for a child session already `reconciling` under the live
// wrapper reconciliation (docs/plan/phase-3-design.md section 5,
// `placedLaunchInFlight`'s `session.State == run.SessionReconciling &&
// wrapperReconciliation(...)` admission, `internal/app/usecase_featureresume.go`)
// against a real Herdr server and real pane processes: reusing
// wrappedlaunch_test.go's own stub and preconditions, a live controller
// first classifies the pane's foreground group as the forking-wrapper
// topology (the claimed pid, still the pane's own shell process, alongside
// a second, different-pid process also carrying the claim's executable
// identity and marker) and moves the session to `reconciling` with its
// claim still `exec_pending` — then, WHILE that topology still holds, the
// controller is hard-killed. Without resume re-inspecting a session already
// left reconciling by the live wrapper reconciliation, nothing else ever
// revisits it: the claim cannot advance on its own, and only a fresh hop
// resume round re-inspects the pane. hop resume must admit the run as
// resumed and running, reporting the child pending under resume's own
// in-flight detail, rather than treating an already-reconciling session as
// permanently unresumable. Releasing the second process afterward lets the
// resumed controller's own corroboration loop finish exactly what the
// original controller was still corroborating when it died.
func TestRealProcessReconcilingChildLaunchAcrossResume(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)

	worker := buildFixtureWorker(t)
	realWorkerPath := filepath.Join(artifacts.dir(t, "real-worker-bin"), "claude-real")
	copyExecutable(t, worker, realWorkerPath)
	installFixtureWorkerAsClaudeStub(t, server, buildWrappedLaunchStub(t, artifacts))

	gates := artifacts.dir(t, "reconciling-child-launch-gates")
	ready := filepath.Join(gates, "holder-ready")
	gate := filepath.Join(gates, "release")
	server.extraEnv = append(server.extraEnv,
		"HOP_WEDGE_WORKER="+realWorkerPath,
		"HOP_WEDGE_READY="+ready,
		"HOP_WEDGE_GATE="+gate)
	release := releaseWrappedLaunch(t, gate)
	// Registered before the server starts: every failure path lets the
	// other process leave on its own, and no pid is ever signaled.
	t.Cleanup(release)
	server.start(t)

	scratchDir := artifacts.dir(t, "fixture-scratch")
	repo := newFeatureFixtureRepo(t, artifacts, server, "reconciling-child-launch-repo", featureFixtureOptions{
		ScratchDir: scratchDir, ReviewerBehavior: "reviewer-approve",
		MaxWorkers: 1, RetryLimit: 3, MessageWaitTimeout: "3s", MessageAttentionAfter: "30s",
	})
	brief := fixtureManagerBrief(scratchDir,
		[]fixtureManagerTask{{Label: "t1", Title: "Implement t1", Behavior: "worker-implement"}}, nil, "")
	fx := startFeatureRun(t, artifacts, server, repo, scratchDir, brief)

	// The shared precondition: the live controller classifies the pane's
	// foreground group as the forking-wrapper topology and moves the
	// session to reconciling with its claim still exec_pending, both
	// matching-identity processes observably alive.
	t1 := requireWrappedTask(t, fx)
	attemptID, sessionID := requireWrappedAttempt(t, fx, t1)
	requireWrappedReconciling(t, fx, sessionID)
	requireWrappedGroup(t, fx, sessionID)

	paneID := fx.requirePane(t, sessionID)
	claimPID, ok := fx.launchClaimPID(t, sessionID)
	if !ok || claimPID <= 0 {
		t.Fatalf("no launch claim pid recorded for session %s", sessionID)
	}
	// The pane's own shell process is still the claimed pid throughout the
	// wrapper's whole life: the wrapper is exec'd in place (never forked)
	// and only forks the SECOND process, so it is this pid resume's own
	// pane inspection matches against — independently confirmed through
	// Herdr's own pane.process_info before the kill.
	info := fx.server.processInfo(t, paneID)
	if int(info.ShellPID) != claimPID {
		t.Fatalf("pane %s shell pid = %d, want the claimed pid %d", paneID, info.ShellPID, claimPID)
	}

	// The crash: the controller dies while the session is still reconciling
	// under the live wrapper reconciliation, the claim still exec_pending,
	// and both processes still alive — never having released the second
	// process itself.
	killControllerLeader(t, fx.controller)
	waitForLeaseExpiry(t, fx.dbPath(), fx.runID)
	if state := fx.sessionState(t, sessionID); state != "reconciling" {
		t.Fatalf("session state after the kill = %q, want it unchanged at reconciling", state)
	}
	if state := fx.claimState(t, sessionID); state != "exec_pending" {
		t.Fatalf("launch claim state after the kill = %q, want it unchanged at exec_pending", state)
	}
	info = fx.server.processInfo(t, paneID)
	if int(info.ShellPID) != claimPID {
		t.Fatalf("pane %s shell pid after the kill = %d, want the still-claimed pid %d unchanged", paneID, info.ShellPID, claimPID)
	}

	resumed := fx.server.startHopController(t, fx.stateDir, "resume", "resume", "-C", fx.repo.Root, fx.runID)
	fx.controller, fx.controllerName = resumed, "resume"

	// THE DECISIVE ASSERTION: hop resume admits the already-reconciling
	// session as in flight and hands the run back to a running controller
	// loop, rather than treating an already-reconciling session as
	// something no fresh resume round can ever re-inspect. Without that
	// admission this exact wait times out: resume instead fails the run
	// closed, releases the lease and exits 1 immediately, never becoming a
	// running controller at all.
	stdout := waitForControllerLog(t, artifacts, "resume", childLaunchInFlightDetail)
	if !strings.Contains(stdout, "resume resumed:") {
		t.Errorf("resume stdout never reports \"resumed\"; got:\n%s", stdout)
	}
	select {
	case <-resumed.leaderExited:
		t.Fatalf("hop resume exited instead of continuing as a running controller; stdout:\n%s", readControllerLog(t, artifacts, "resume"))
	default:
	}
	fx.requireRunState(t, "running")
	if state := fx.sessionState(t, sessionID); state != "reconciling" {
		t.Errorf("session state right after resume = %q, want unchanged reconciling: resume itself never settles it", state)
	}
	if state := fx.claimState(t, sessionID); state != "exec_pending" {
		t.Errorf("launch claim state right after resume = %q, want unchanged exec_pending: resume itself never settles it", state)
	}

	// Release the second process; the resumed controller's own
	// corroboration loop re-inspects the same reconciling session, settles
	// the claim once only the claimed process remains, and the run
	// completes exactly as the original, uninterrupted controller would
	// have finished it.
	release()
	if !waitUntilDeadline(featureRunTimeout, func() bool { return fx.claimState(t, sessionID) == "execed" }) {
		t.Fatalf("launch claim never settled after release; state = %q.\nsession transitions recorded:\n%s", fx.claimState(t, sessionID), sessionTransitionJournal(t, fx, sessionID))
	}
	fx.requireSessionState(t, sessionID, "active")
	fx.requireAttemptState(t, attemptID, "running", "checking", "integrating", "completed")
	fx.requireTaskState(t, t1, "integrated")
	reviewTaskID := fx.requireReviewTask(t)
	fx.requireTaskState(t, reviewTaskID, "completed")
	fx.requireRunState(t, "completed")
}
