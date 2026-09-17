package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testLaunchArgvDigest reproduces internal/app/usecase_execboundary.go's
// launchArgvDigest byte for byte (unexported there, like the prompt
// templates this package already mirrors): the "hop-argv-v1" tag, each
// argv element as `<decimal byte length>:<raw bytes>`, SHA-256 lowercase
// hex. It lets a real-process scenario verify the exact argv a launch
// claim recorded from durable evidence alone.
func testLaunchArgvDigest(argv []string) string {
	var b strings.Builder
	b.WriteString("hop-argv-v1")
	for _, arg := range argv {
		fmt.Fprintf(&b, "%d:%s", len(arg), arg)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

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

	workerPID := fx.workerForegroundPID(t, paneID)
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

	// The same worker pid must still be among the pane's foreground members
	// (warm reattach rebinds, it never relaunches); it is located by the
	// claim's pid, never by listing index — the worker's MCP stand-in child
	// shares its group and holds no fixed position.
	if got := fx.workerForegroundPID(t, paneID); got != workerPID {
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

// startSelfKillableFixtureRun starts a solo fixture run whose worker is
// reachable through the fixture self-kill channel: "idle-self-kill" is
// registered in scratchDirRequiringBehaviors (fixtureworker_test.go), but
// fixtureWorkerBrief's solo directive line carries no separate scratch-dir
// argument of its own, so the scratch directory is appended directly into
// the behavior string it renders verbatim ("FIXTURE-BEHAVIOR: <behavior>
// <scratchDir>\n") — parseBehavior then splits it out as behaviorArgs[0],
// exactly the shape requireScratchDir expects. Returns the run plus the
// scratch directory endWithSelfKill needs to address this worker's control
// file.
func startSelfKillableFixtureRun(t *testing.T) (fx *fixtureRun, scratchDir string) {
	t.Helper()
	artifacts, server := newFixtureRunEnv(t)
	repo := newFixtureRepo(t, artifacts, server, "repo")
	scratchDir = artifacts.dir(t, "fixture-scratch")
	fx = startRun(t, artifacts, server, repo, "idle-self-kill "+scratchDir)
	return fx, scratchDir
}

// endWithSelfKill asks attemptID's own worker to end itself via the
// fixture's self-kill control file — never a raw OS signal to a pid this
// harness only OBSERVED via pane inspection: the OS could recycle that
// pid between observation and signal, since this harness never owns the
// worker's Wait/reap lifecycle; only Herdr does.
// Waits for the pane to close itself (a layout.apply command pane has no
// shell, S6), giving hop resume's absence observation both required
// conjuncts.
func endWithSelfKill(t *testing.T, fx *fixtureRun, scratchDir, paneID, attemptID string) {
	t.Helper()
	controlPath := filepath.Join(scratchDir, "self-kill-"+attemptID)
	tmp := controlPath + ".tmp"
	if err := os.WriteFile(tmp, []byte("FIXTURE-SELF-KILL\n"), 0o600); err != nil {
		t.Fatalf("write self-kill control file for attempt %s: %v", attemptID, err)
	}
	if err := os.Rename(tmp, controlPath); err != nil {
		t.Fatalf("rename self-kill control file into place for attempt %s: %v", attemptID, err)
	}
	if !waitUntil(func() bool { return !fx.server.paneExists(t, paneID) }) {
		t.Fatalf("pane %s still exists after attempt %s's self-kill control file was written", paneID, attemptID)
	}
	// Remove this test's own control file now that the pane is gone and
	// before any relaunch: a relaunched incarnation of the SAME attempt
	// runs the identical frozen "idle-self-kill" brief, so a control file
	// still on disk from this kill would make it SIGKILL itself the
	// instant it starts, before it ever becomes observable.
	if err := os.Remove(controlPath); err != nil {
		t.Fatalf("remove self-kill control file for attempt %s: %v", attemptID, err)
	}
}

// coldRelaunchAfterCrash builds a self-killable fixture run, settles its
// first incarnation, then crash-simulates total loss of both the worker
// and the controller: it asks the worker to end ITSELF via the fixture
// self-kill channel (a layout.apply command pane has no shell, so the pane
// closes itself once its command exits, S6 — giving hop resume's absence
// observation both required conjuncts: gone by id, gone by creation label,
// no ambiguity) and SIGKILLs the hop run controller through its own owned
// process handle (never released its lease). Once the abandoned lease has
// expired, it drives `hop resume --confirm-absent` to a cold relaunch
// (design section 5 resume case 3/5: absence conclusively established by
// positive observation, continuity established because the herdr server
// itself never restarted) and waits for the new incarnation to itself
// settle, returning both incarnation ids and the shared task/attempt ids
// for the caller's own assertions.
func coldRelaunchAfterCrash(t *testing.T, fx *fixtureRun, scratchDir string) (oldIncarnationID, newIncarnationID, taskID, attemptID string) {
	t.Helper()
	fields := fx.requireRunState(t, "running")
	paneID := paneIDFromBinding(fields["binding"])

	taskID, attemptID = fx.taskAndAttemptIDs(t)
	oldIncarnationID = fx.currentIncarnationID(t, attemptID)

	endWithSelfKill(t, fx, scratchDir, paneID, attemptID)

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

// waitForWorkerObservation polls the scratch-dir observation dump at path
// until it carries env:HOP_INCARNATION_ID=incarnationID, bounded. The dump
// name is keyed by attempt id, not incarnation id (fixtureworker_test.go's
// runWorker), so a cold relaunch's worker overwrites the SAME file its
// predecessor already wrote; reading it without this wait could observe
// the retired incarnation's own stale content instead of the relaunched
// worker's.
func waitForWorkerObservation(t *testing.T, path, incarnationID string) workerObservation {
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
		t.Fatalf("worker observation %s never carried %s", path, want)
	}
	return obs
}

// TestRealProcessConfirmAbsentColdRelaunchNonRestart proves design section 5
// resume case 5's non-restart litmus test directly: with the same herdr
// server process up throughout (server-continuity established), a positively
// observed absent worker plus `--confirm-absent` authorizes a cold relaunch,
// journals the human attestation, and binds a fresh session and incarnation
// to the same attempt.
func TestRealProcessConfirmAbsentColdRelaunchNonRestart(t *testing.T) {
	fx, scratchDir := startSelfKillableFixtureRun(t)
	oldIncarnationID, newIncarnationID, _, attemptID := coldRelaunchAfterCrash(t, fx, scratchDir)

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

	// The relaunched claim's argv carries the continuation prompt: recompute
	// the claim's canonical argv digest from durable facts alone — the
	// claim's own recorded executable, the adjacent `--resume <native-ref>`
	// pair, and the byte-for-byte continuation prompt built from the run's
	// assignment path and the hop path the relaunched worker itself observed
	// in that prompt — and require it to equal the recorded digest. A
	// prompt-less relaunch argv (the pre-fix shape) fails this equality.
	newClaimExecutable := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT executable FROM launch_claims WHERE incarnation_id = '%s';", newIncarnationID))
	newClaimDigest := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT argv_digest FROM launch_claims WHERE incarnation_id = '%s';", newIncarnationID))
	if newClaimExecutable == "" || newClaimDigest == "" {
		t.Fatalf("no launch claim recorded for the relaunched incarnation %s", newIncarnationID)
	}
	nativeRef := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT DISTINCT native_session_ref FROM sessions WHERE attempt_id = '%s';", attemptID))
	if nativeRef == "" || strings.Contains(nativeRef, "\n") {
		t.Fatalf("attempt %s does not carry one shared native session reference; got %q", attemptID, nativeRef)
	}
	assignmentPath := filepath.Join(fx.stateDir, "runs", fx.runID, "artifacts", "assignment.md")
	obs := waitForWorkerObservation(t, filepath.Join(scratchDir, "worker-observed-"+attemptID+".txt"), newIncarnationID)
	hopPath := obs.Fields["hop_path"]
	if hopPath == "" {
		t.Fatalf("the relaunched worker's observation dump records no hop_path (parsed from the continuation prompt)")
	}
	wantArgv := []string{newClaimExecutable, "--resume", nativeRef, testContinuationPrompt(assignmentPath, hopPath)}
	if got := testLaunchArgvDigest(wantArgv); got != newClaimDigest {
		t.Errorf("relaunched claim argv digest = %s, want %s for %q — the claim's argv must be exactly `<claude> --resume <native-ref> <continuation prompt>`", newClaimDigest, got, wantArgv)
	}
}

// TestRealProcessStaleSubmissionFromRetiredIncarnation proves design section
// 7 step 4's stale-incarnation rule directly: once a cold relaunch has
// superseded a session's binding, a hop result submit carrying the RETIRED
// incarnation id — with no corresponding OS process anywhere, since the
// eligibility check is a pure store read of the attempt's current binding —
// is rejected `stale`, never accepted or treated as a duplicate.
func TestRealProcessStaleSubmissionFromRetiredIncarnation(t *testing.T) {
	fx, scratchDir := startSelfKillableFixtureRun(t)
	oldIncarnationID, _, taskID, attemptID := coldRelaunchAfterCrash(t, fx, scratchDir)

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
	// attempt the new (current) incarnation owns. A single snapshot read
	// here would be a race, not a proof: the controller's own next
	// scheduling pass could still settle the attempt failed after this
	// read happened to land first. The wait for "running" independently
	// confirms the current incarnation is genuinely healthy, and the
	// transitions journal (append-only, so a later pass can only ADD to
	// it) is checked directly for the one outcome that must never appear.
	if !waitUntil(func() bool {
		return querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM attempts WHERE id = '%s';", attemptID)) == "running"
	}) {
		t.Fatalf("attempt %s never confirmed running after the stale submission", attemptID)
	}
	if n := querySQLite(t, fx.dbPath(), fmt.Sprintf(
		"SELECT count(*) FROM transitions WHERE entity_kind = 'attempt' AND entity_id = '%s' AND to_state IN ('failed', 'interrupted');",
		attemptID)); n != "0" {
		t.Errorf("attempt %s recorded %s failed/interrupted transition(s), want none: a stale submission from a retired incarnation must never affect it", attemptID, n)
	}
}

// TestRealProcessConfirmAbsentRefusedAfterServerRestart proves the negative
// half of design section 5 resume case 5's litmus test: identical crash
// (worker and controller both killed, pane conclusively gone) except that
// the herdr SERVER ITSELF also restarts before hop resume --confirm-absent
// runs. Server continuity is no longer established (the creation-time
// recorded server instance token can never equal a freshly observed one
// from a different server process), so the attestation is still journaled
// but the cold relaunch is refused — the run stays resuming/reconciling,
// never relaunching, distinct from the non-restart case this task also
// proves.
func TestRealProcessConfirmAbsentRefusedAfterServerRestart(t *testing.T) {
	fx, scratchDir := startSelfKillableFixtureRun(t)
	fields := fx.requireRunState(t, "running")
	paneID := paneIDFromBinding(fields["binding"])
	_, attemptID := fx.taskAndAttemptIDs(t)
	oldIncarnationID := fx.currentIncarnationID(t, attemptID)

	endWithSelfKill(t, fx, scratchDir, paneID, attemptID)
	killControllerLeader(t, fx.controller)
	waitForLeaseExpiry(t, fx.dbPath(), fx.runID)

	// The restart itself is unrelated to the run's own lease (a pure HOP-side,
	// sqlite-recorded concept independent of herdr): only the server-instance
	// continuity token changes.
	fx.server.restart(t)

	result := runHop(t, fx.env, fx.repo.Root, "resume", "-C", fx.repo.Root, "--confirm-absent", fx.runID)
	if result.ExitCode != 1 {
		t.Fatalf("hop resume --confirm-absent after a server restart exit=%d, want 1 (refused, not a cold relaunch); stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "resume reconciling") {
		t.Errorf("hop resume --confirm-absent after a server restart stdout = %q, want it to report reconciling, not cold-relaunched", result.Stdout)
	}
	if strings.Contains(result.Stdout, "cold-relaunched") {
		t.Errorf("hop resume --confirm-absent after a server restart reported cold-relaunched; want it refused: %q", result.Stdout)
	}

	runState := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM runs WHERE id = '%s';", fx.runID))
	if runState != "resuming" {
		t.Errorf("run state after the refused attestation = %q, want \"resuming\" (never relaunching)", runState)
	}
	attestations := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT count(*) FROM operations WHERE run_id = '%s' AND kind = 'absence.attested';", fx.runID))
	if attestations == "0" {
		t.Errorf("no absence.attested operation recorded for run %s; the attestation is journaled even when refused", fx.runID)
	}
	currentIncarnation := fx.currentIncarnationID(t, attemptID)
	if currentIncarnation != oldIncarnationID {
		t.Errorf("current incarnation after the refused attestation = %s, want unchanged %s (no relaunch happened)", currentIncarnation, oldIncarnationID)
	}
}
