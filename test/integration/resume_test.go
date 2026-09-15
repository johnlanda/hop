package integration

import (
	"fmt"
	"strings"
	"syscall"
	"testing"
)

// TestRealProcessControllerKillResumeWarmReattach proves design section 5
// resume case 1 (warm reattach) against a real hard-killed controller: the
// worker's pane and process are left entirely untouched, only the
// controller dies. Once the abandoned lease has expired, a fresh
// `hop resume` observes the same live occupant satisfying the section 6
// corroboration predicate and reattaches — the same worker pid, the same
// incarnation, no new session — and continues driving the run.
func TestRealProcessControllerKillResumeWarmReattach(t *testing.T) {
	fx := startFixtureRun(t, "") // idling worker: stays put so the pid is stable across the kill.
	fields := fx.requireRunState(t, "running")
	paneID := paneIDFromBinding(fields["binding"])

	before := fx.server.processInfo(t, paneID)
	if len(before.ForegroundProcesses) == 0 {
		t.Fatalf("pane %s reports no foreground worker process before the controller is killed", paneID)
	}
	workerPID := before.ForegroundProcesses[0].PID
	_, attemptID := fx.taskAndAttemptIDs(t)
	incarnationBefore := fx.currentIncarnationID(t, attemptID)

	killControllerLeader(t, fx.controller)
	waitForLeaseExpiry(t, fx.dbPath(), fx.runID)

	resumed := fx.server.startHopController(t, fx.stateDir, "resume", "resume", "-C", fx.repo.Root, fx.runID)
	stdout := waitForControllerLog(t, fx.artifacts, "resume", "resume warm-reattached")
	if !strings.Contains(stdout, "resume warm-reattached") {
		t.Fatalf("hop resume did not report warm-reattached; stdout:\n%s", stdout)
	}
	fx.controller, fx.controllerName = resumed, "resume"

	fx.requireRunState(t, "running")

	after := fx.server.processInfo(t, paneID)
	if len(after.ForegroundProcesses) == 0 {
		t.Fatalf("pane %s reports no foreground worker process after warm reattach", paneID)
	}
	if got := after.ForegroundProcesses[0].PID; got != workerPID {
		t.Errorf("worker pid after warm reattach = %d, want unchanged %d (warm reattach rebinds, it never relaunches)", got, workerPID)
	}
	if got := fx.currentIncarnationID(t, attemptID); got != incarnationBefore {
		t.Errorf("incarnation after warm reattach = %s, want unchanged %s (warm reattach keeps the incarnation)", got, incarnationBefore)
	}
	sessionCount := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT count(*) FROM sessions WHERE attempt_id = '%s';", attemptID))
	if sessionCount != "1" {
		t.Errorf("session count for attempt %s after warm reattach = %s, want 1 (no new session)", attemptID, sessionCount)
	}
}

// coldRelaunchAfterCrash builds a fixture run, settles its first
// incarnation, then crash-simulates total loss of both the worker and the
// controller: it SIGKILLs the worker's own foreground process (a
// layout.apply command pane has no shell, so the pane closes itself once
// its command exits, S6 — giving hop resume's absence observation both
// required conjuncts: gone by id, gone by creation label, no ambiguity) and
// SIGKILLs the hop run controller (never released its lease). Once the
// abandoned lease has expired, it drives `hop resume --confirm-absent` to a
// cold relaunch (design section 5 resume case 3/5: absence conclusively
// established by positive observation, continuity established because the
// herdr server itself never restarted) and waits for the new incarnation to
// itself settle, returning both incarnation ids and the shared task/attempt
// ids for the caller's own assertions.
func coldRelaunchAfterCrash(t *testing.T, fx *fixtureRun) (oldIncarnationID, newIncarnationID, taskID, attemptID string) {
	t.Helper()
	fields := fx.requireRunState(t, "running")
	paneID := paneIDFromBinding(fields["binding"])

	taskID, attemptID = fx.taskAndAttemptIDs(t)
	oldIncarnationID = fx.currentIncarnationID(t, attemptID)

	info := fx.server.processInfo(t, paneID)
	if len(info.ForegroundProcesses) == 0 {
		t.Fatalf("pane %s reports no foreground worker process before the crash", paneID)
	}
	workerPID := int(info.ForegroundProcesses[0].PID)
	if err := syscall.Kill(workerPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill fixture worker pid %d: %v", workerPID, err)
	}
	if !waitUntil(func() bool { return !fx.server.paneExists(t, paneID) }) {
		t.Fatalf("pane %s still exists after its foreground worker was killed; a layout.apply command pane must close itself (S6)", paneID)
	}

	killControllerLeader(t, fx.controller)
	waitForLeaseExpiry(t, fx.dbPath(), fx.runID)

	resumed := fx.server.startHopController(t, fx.stateDir, "resume", "resume", "-C", fx.repo.Root, "--confirm-absent", fx.runID)
	stdout := waitForControllerLog(t, fx.artifacts, "resume", "resume cold-relaunched")
	if !strings.Contains(stdout, "resume cold-relaunched") {
		t.Fatalf("hop resume --confirm-absent did not report cold-relaunched; stdout:\n%s", stdout)
	}
	fx.controller, fx.controllerName = resumed, "resume"

	fx.requireRunState(t, "running")
	newIncarnationID = fx.currentIncarnationID(t, attemptID)
	if newIncarnationID == oldIncarnationID {
		t.Fatalf("incarnation after cold relaunch = %s, want a new incarnation distinct from %s", newIncarnationID, oldIncarnationID)
	}
	return oldIncarnationID, newIncarnationID, taskID, attemptID
}

// TestRealProcessConfirmAbsentColdRelaunchNonRestart proves design section 5
// resume case 5's non-restart litmus test directly: with the same herdr
// server process up throughout (server-continuity established), a positively
// observed absent worker plus `--confirm-absent` authorizes a cold relaunch,
// journals the human attestation, and binds a fresh session and incarnation
// to the same attempt.
func TestRealProcessConfirmAbsentColdRelaunchNonRestart(t *testing.T) {
	fx := startFixtureRun(t, "")
	oldIncarnationID, newIncarnationID, _, attemptID := coldRelaunchAfterCrash(t, fx)

	if oldIncarnationID == newIncarnationID {
		t.Fatal("cold relaunch did not mint a new incarnation")
	}
	sessionCount := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT count(*) FROM sessions WHERE attempt_id = '%s';", attemptID))
	if sessionCount != "2" {
		t.Errorf("session count for attempt %s after one cold relaunch = %s, want 2 (the original, now lost, plus the relaunch's)", attemptID, sessionCount)
	}
	lostSessions := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT count(*) FROM sessions WHERE attempt_id = '%s' AND state = 'lost';", attemptID))
	if lostSessions != "1" {
		t.Errorf("lost-session count for attempt %s = %s, want 1 (the original, retired incarnation)", attemptID, lostSessions)
	}
	attestations := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT count(*) FROM operations WHERE run_id = '%s' AND kind = 'absence.attested';", fx.runID))
	if attestations == "0" {
		t.Errorf("no absence.attested operation recorded for run %s after --confirm-absent", fx.runID)
	}
}

// TestRealProcessStaleSubmissionFromRetiredIncarnation proves design section
// 7 step 4's stale-incarnation rule directly: once a cold relaunch has
// superseded a session's binding, a hop result submit carrying the RETIRED
// incarnation id — with no corresponding OS process anywhere, since the
// eligibility check is a pure store read of the attempt's current binding —
// is rejected `stale`, never accepted or treated as a duplicate.
func TestRealProcessStaleSubmissionFromRetiredIncarnation(t *testing.T) {
	fx := startFixtureRun(t, "")
	oldIncarnationID, _, taskID, attemptID := coldRelaunchAfterCrash(t, fx)

	env := append(append([]string{}, fx.env...), "HOP_INCARNATION_ID="+oldIncarnationID)
	result := runHop(t, env, fx.stateDir, "result", "submit",
		"--summary", "stale submission from a retired incarnation",
		"--commit", strings.Repeat("a", 40),
		"--run", fx.runID, "--task", taskID, "--attempt", attemptID)
	if result.ExitCode != 1 {
		t.Fatalf("hop result submit (stale) exit=%d, want 1; stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
	if !strings.HasPrefix(result.FirstStdoutLine(), "stale") {
		t.Errorf("hop result submit (stale) first line = %q, want it to begin with \"stale\"", result.FirstStdoutLine())
	}

	outcome := querySQLite(t, fx.dbPath(), fmt.Sprintf(
		"SELECT outcome FROM result_submissions WHERE claimed_incarnation_id = '%s' AND claimed_attempt_id = '%s' ORDER BY submitted_at DESC LIMIT 1;",
		oldIncarnationID, attemptID))
	if outcome != "stale" {
		t.Errorf("result_submissions outcome for the retired incarnation = %q, want \"stale\"", outcome)
	}

	// The retired incarnation's stale receipt must never touch the still-open
	// attempt the new (current) incarnation owns.
	state := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM attempts WHERE id = '%s';", attemptID))
	if state == "failed" || state == "interrupted" {
		t.Errorf("attempt state after the stale submission = %q, want the current incarnation's attempt left unaffected", state)
	}
}
