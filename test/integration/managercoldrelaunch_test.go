package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// waitForManagerObservation polls the scratch-dir manager-observed.txt dump
// until it carries env:HOP_INCARNATION_ID=incarnationID, bounded.
// runManager (fixtureworker_test.go) writes this SAME fixed-name file on
// every incarnation (unlike the worker's attempt-keyed dump, since the
// manager has no attempt id), so a relaunched manager overwrites its
// predecessor's own content; reading it without this wait could observe
// the retired incarnation's stale content instead of the relaunched
// manager's.
func waitForManagerObservation(t *testing.T, path, incarnationID string) workerObservation {
	t.Helper()
	want := "env:HOP_INCARNATION_ID=" + incarnationID
	var obs workerObservation
	if !waitUntil(func() bool {
		raw, err := os.ReadFile(path) //nolint:gosec // G304: a path this test constructed itself, under its own scratch directory.
		if err != nil || !strings.Contains(string(raw), want) {
			return false
		}
		obs = readWorkerObservation(t, path)
		return true
	}) {
		t.Fatalf("manager observation %s never carried %s", path, want)
	}
	return obs
}

// TestRealProcessManagerColdRelaunch proves design section 8's row: the
// manager is killed together with the controller; hop resume
// --confirm-absent=<manager-session> cold-relaunches the manager lineage
// via HOP's one supported --resume argv shape, while the still-live
// worker warmly reattaches untouched by the same round. Built entirely on
// 7b's mechanisms — the manager's own self-kill watcher
// (fixtureworker_test.go's runManager) and its resume-skips-replanning
// guard — never a raw signal to an observed pid.
//
// A single worker-hold task keeps its worker blocked at a barrier for the
// whole window: task t1 reaches "active" (and therefore its attempt,
// session and worktree intent are already committed) before the worker
// process itself has done anything else, so killing the manager at that
// point guarantees the worker's own barrier question is still unsent —
// the relaunched manager, not the killed one, is what relays it and later
// forwards the human's answer back.
func TestRealProcessManagerColdRelaunch(t *testing.T) {
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
		[]fixtureManagerTask{{Label: "t1", Title: "Implement t1", Behavior: "worker-hold"}},
		[]fixtureManagerAnswer{{Match: fixtureHoldMarker, Action: "relay"}},
		"",
	)
	fx := startFeatureRun(t, artifacts, server, repo, scratchDir, brief)

	t1 := fx.requireTaskBySeq(t, 1)
	fx.requireTaskState(t, t1, "active")
	attempt1ID, attempt1Number := fx.currentAttempt(t, t1)
	if attempt1Number != 1 {
		t.Fatalf("first attempt number = %d, want 1", attempt1Number)
	}
	worker1SessionID := fx.sessionForAttempt(t, attempt1ID)
	worktreePath, worktreeBranch, worktreeBase := fx.requireWorktreeForAttempt(t, attempt1ID)

	managerSessionID := fx.managerSessionID(t)
	managerPaneID := fx.requirePane(t, managerSessionID)

	// Snapshot every row a cold relaunch of the MANAGER must never
	// disturb, taken before either kill: the task and attempt rows (byte
	// for byte), the run's task count (no duplicate plan) and the plan
	// flag's own timestamp (no second close). Nothing about t1 or its
	// attempt can legitimately change while the manager is down — the
	// worker stays blocked on its own barrier the entire time — so an
	// exact string comparison after the relaunch is not a race.
	beforeTaskCount := fx.scalar(t, fmt.Sprintf("SELECT count(*) FROM tasks WHERE run_id = '%s';", fx.runID))
	beforeTaskRow := fx.scalar(t, fmt.Sprintf("SELECT * FROM tasks WHERE id = '%s';", t1))
	beforeAttemptRow := fx.scalar(t, fmt.Sprintf("SELECT * FROM attempts WHERE id = '%s';", attempt1ID))
	beforePlanClosedAt := fx.scalar(t, fmt.Sprintf("SELECT plan_closed_at FROM runs WHERE id = '%s';", fx.runID))
	if beforePlanClosedAt == "" {
		t.Fatalf("plan_closed_at is empty before the kill; the manager never closed its plan")
	}

	// The manager dies by its own hand through the fixture's self-kill
	// control channel — never a raw signal to an observed pid — exactly
	// resume_test.go's endWithSelfKill pattern, keyed by the manager's own
	// session id (the manager has no attempt id; fixtureworker_test.go's
	// runManager names its control file self-kill-<HOP_SESSION_ID>).
	// Waits for the pane to close itself (a layout.apply command pane has
	// no shell, S6), giving hop resume's absence observation the by-id
	// conjunct, then removes the control file: a relaunched incarnation
	// reads the identical frozen assignment and would SIGKILL itself the
	// instant it started if the file were still in place.
	managerSelfKillControlPath := filepath.Join(scratchDir, "self-kill-"+managerSessionID)
	tmp := managerSelfKillControlPath + ".tmp"
	if err := os.WriteFile(tmp, []byte("FIXTURE-SELF-KILL\n"), 0o600); err != nil {
		t.Fatalf("write manager self-kill control file: %v", err)
	}
	if err := os.Rename(tmp, managerSelfKillControlPath); err != nil {
		t.Fatalf("rename manager self-kill control file into place: %v", err)
	}
	if !waitUntil(func() bool { return !fx.server.paneExists(t, managerPaneID) }) {
		t.Fatalf("manager pane %s still exists after its self-kill control file was written", managerPaneID)
	}
	if err := os.Remove(managerSelfKillControlPath); err != nil {
		t.Fatalf("remove manager self-kill control file: %v", err)
	}

	killControllerLeader(t, fx.controller)
	waitForLeaseExpiry(t, fx.dbPath(), fx.runID)

	resumed := fx.server.startHopController(t, fx.stateDir, "resume", "resume", "-C", fx.repo.Root, "--confirm-absent="+managerSessionID, fx.runID)
	stdout := waitForControllerLog(t, fx.artifacts, "resume", "resume resumed")
	if !strings.Contains(stdout, "session "+managerSessionID+" (manager): relaunched") {
		t.Fatalf("hop resume did not report the manager session relaunched; stdout:\n%s", stdout)
	}
	if !strings.Contains(stdout, "session "+worker1SessionID+" (implementer): warm") {
		t.Fatalf("hop resume did not report the worker session warm-reattached; stdout:\n%s", stdout)
	}
	fx.controller, fx.controllerName = resumed, "resume"

	// Task/attempt state must be exactly what it was before either kill —
	// no re-plan, no duplicate task, no new attempt — asserted from the
	// store, never from a log line, and read back BEFORE the worker's own
	// barrier is released so nothing the worker legitimately does
	// afterward could be mistaken for something the relaunch itself
	// added.
	if got := fx.scalar(t, fmt.Sprintf("SELECT count(*) FROM tasks WHERE run_id = '%s';", fx.runID)); got != beforeTaskCount {
		t.Errorf("task count for run %s after the manager's cold relaunch = %s, want unchanged %s (no re-plan)", fx.runID, got, beforeTaskCount)
	}
	if got := fx.scalar(t, fmt.Sprintf("SELECT * FROM tasks WHERE id = '%s';", t1)); got != beforeTaskRow {
		t.Errorf("task %s row after the manager's cold relaunch =\n%s\nwant unchanged\n%s", t1, got, beforeTaskRow)
	}
	if got := fx.scalar(t, fmt.Sprintf("SELECT * FROM attempts WHERE id = '%s';", attempt1ID)); got != beforeAttemptRow {
		t.Errorf("attempt %s row after the manager's cold relaunch =\n%s\nwant unchanged\n%s", attempt1ID, got, beforeAttemptRow)
	}
	if got := fx.attemptCount(t, t1); got != 1 {
		t.Errorf("attempt count for task %s after the manager's cold relaunch = %d, want 1 (no new attempt: only the manager was lost, not the worker)", t1, got)
	}
	if got := fx.scalar(t, fmt.Sprintf("SELECT plan_closed_at FROM runs WHERE id = '%s';", fx.runID)); got != beforePlanClosedAt {
		t.Errorf("plan_closed_at for run %s after the manager's cold relaunch = %q, want unchanged %q (no second plan close)", fx.runID, got, beforePlanClosedAt)
	}
	if gotPath, gotBranch, gotBase, _ := fx.worktreeForAttempt(t, attempt1ID); gotPath != worktreePath || gotBranch != worktreeBranch || gotBase != worktreeBase {
		t.Errorf("worktree for attempt %s after the manager's cold relaunch = (%s, %s, %s), want unchanged (%s, %s, %s)", attempt1ID, gotPath, gotBranch, gotBase, worktreePath, worktreeBranch, worktreeBase)
	}

	// The worker's own session and lineage are untouched: only the
	// manager was lost, so the worker warmly reattaches (same session,
	// never a successor) exactly as
	// TestRealProcessControllerKillResumeWarmReattach proves for a plain
	// controller kill.
	if got := fx.sessionForAttempt(t, attempt1ID); got != worker1SessionID {
		t.Errorf("worker session for attempt %s after the manager's cold relaunch = %s, want unchanged %s (warm reattach, not a relaunch)", attempt1ID, got, worker1SessionID)
	}
	if got := fx.scalar(t, fmt.Sprintf("SELECT count(*) FROM sessions WHERE attempt_id = '%s';", attempt1ID)); got != "1" {
		t.Errorf("session count for attempt %s after the manager's cold relaunch = %s, want 1 (the worker was never relaunched)", attempt1ID, got)
	}

	// Exactly one predecessor manager session, now lost, plus its
	// successor: the manager-uniqueness index admits the successor only
	// once the predecessor is terminal
	// (usecase_featureresume.go's coldRelaunchFeatureSession).
	if got := fx.scalar(t, fmt.Sprintf("SELECT count(*) FROM sessions WHERE run_id = '%s' AND role = 'manager';", fx.runID)); got != "2" {
		t.Errorf("manager session count for run %s after one cold relaunch = %s, want 2 (the original, now lost, plus the relaunch's)", fx.runID, got)
	}
	if got := fx.scalar(t, fmt.Sprintf("SELECT count(*) FROM sessions WHERE run_id = '%s' AND role = 'manager' AND state = 'lost';", fx.runID)); got != "1" {
		t.Errorf("lost manager-session count for run %s = %s, want 1 (the original, retired incarnation)", fx.runID, got)
	}
	if got := fx.scalar(t, fmt.Sprintf("SELECT count(*) FROM operations WHERE run_id = '%s' AND kind = 'absence.attested';", fx.runID)); got == "0" {
		t.Errorf("no absence.attested operation recorded for run %s after --confirm-absent=%s", fx.runID, managerSessionID)
	}

	newManagerSessionID := fx.managerSessionID(t)
	if newManagerSessionID == managerSessionID {
		t.Fatalf("manager session after the cold relaunch = %s, want a new session distinct from the killed %s", newManagerSessionID, managerSessionID)
	}

	// The relaunched claim's argv carries the manager's continuation
	// prompt: recompute the claim's canonical hop-argv-v1 digest from
	// durable facts alone — the claim's own recorded executable, the
	// adjacent --resume <native-ref> pair, and the byte-for-byte manager
	// continuation prompt built from the run's frozen assignment, role
	// and worker-protocol-crib artifact paths and the hop path the
	// relaunched manager itself observed in that prompt — and require it
	// to equal the recorded digest, exactly as
	// TestRealProcessConfirmAbsentColdRelaunchNonRestart does for a
	// worker. A prompt-less or reshaped relaunch argv fails this equality
	// (and fails requireResumeShape's own strict validation inside the
	// fixture before ever reaching this point, shared by every role).
	newClaimExecutable := fx.scalar(t, fmt.Sprintf("SELECT executable FROM launch_claims WHERE session_id = '%s';", newManagerSessionID))
	newClaimDigest := fx.scalar(t, fmt.Sprintf("SELECT argv_digest FROM launch_claims WHERE session_id = '%s';", newManagerSessionID))
	if newClaimExecutable == "" || newClaimDigest == "" {
		t.Fatalf("no launch claim recorded for the relaunched manager session %s", newManagerSessionID)
	}
	nativeRef := fx.scalar(t, fmt.Sprintf("SELECT DISTINCT native_session_ref FROM sessions WHERE run_id = '%s' AND role = 'manager';", fx.runID))
	if nativeRef == "" || strings.Contains(nativeRef, "\n") {
		t.Fatalf("the manager lineage for run %s does not carry one shared native session reference; got %q", fx.runID, nativeRef)
	}
	newIncarnationID := fx.scalar(t, fmt.Sprintf("SELECT incarnation_id FROM runtime_bindings WHERE session_id = '%s' ORDER BY observed_at DESC LIMIT 1;", newManagerSessionID))
	if newIncarnationID == "" {
		t.Fatalf("no runtime binding recorded for the relaunched manager session %s", newManagerSessionID)
	}

	managerObs := waitForManagerObservation(t, filepath.Join(scratchDir, "manager-observed.txt"), newIncarnationID)
	hopPath := managerObs.Fields["hop_path"]
	if hopPath == "" {
		t.Fatalf("the relaunched manager's observation dump records no hop_path (parsed from the continuation prompt)")
	}
	assignmentPath := filepath.Join(fx.stateDir, "runs", fx.runID, "artifacts", "assignment.md")
	rolePath := filepath.Join(fx.stateDir, "runs", fx.runID, "artifacts", "roles", "manager.md")
	cribPath := filepath.Join(fx.stateDir, "runs", fx.runID, "artifacts", "worker-protocol.md")
	wantArgv := []string{newClaimExecutable, "--resume", nativeRef, testManagerContinuationPrompt(assignmentPath, rolePath, cribPath, hopPath)}
	if got := testLaunchArgvDigest(wantArgv); got != newClaimDigest {
		t.Errorf("relaunched manager claim argv digest = %s, want %s for %q — the claim's argv must be exactly `<claude> --resume <native-ref> <continuation prompt>`", newClaimDigest, got, wantArgv)
	}

	// The relaunched manager resumes its section 7 duties: it relays the
	// worker's own barrier question — still pending, since the manager
	// was killed before the worker ever reached its barrier — to the
	// human, and, once answered, forwards the answer back so the worker
	// releases and submits. A manager that comes back mute would leave
	// the relayed question, and therefore the whole run, stuck here
	// forever, failing this wait rather than any later one.
	fx.requireTaskStateNeverReconciling(t, t1, "active", "checking", "completed", "integrating", "integrated")
	question := fx.relayedQuestionFor(t, worker1SessionID)
	if got := fx.messageBodyContent(t, question); got != fixtureHoldMarker+"\n" {
		t.Errorf("relayed question %s body = %q, want the barrier marker unchanged", question, got)
	}
	fx.answerHuman(t, question, "released")

	fx.requireTaskStateNeverReconciling(t, t1, "completed", "integrating", "integrated")
	fx.requireIntegrationState(t, t1, "integrated")
	reviewTask := fx.requireReviewTask(t)
	fx.requireTaskState(t, reviewTask, "completed")
	if verdict, ok := fx.reviewVerdict(t, reviewTask); !ok || verdict != "approve" {
		t.Errorf("review verdict for task %s = %q (ok=%v), want \"approve\"", reviewTask, verdict, ok)
	}

	fx.requireRunState(t, "completed")
	fx.requireSessionState(t, newManagerSessionID, "terminated")
}
