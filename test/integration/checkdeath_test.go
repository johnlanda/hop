package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// leaderExitCheckScriptName names a check whose own process (the group
// leader hop check-exec execs into) backgrounds a long-lived child and
// exits before that child does.
const leaderExitCheckScriptName = "check-leader-exit.sh"

// leaderExitCheckScriptSource backgrounds a sleep far longer than this
// scenario needs, then exits 0 immediately — the leader (this script's own
// process) is gone long before the backgrounded child would ever exit on
// its own. The backgrounded child inherits the leader's stdout/stderr, so
// those pipes never see EOF on their own once the leader exits: this is
// what actually produces the ambiguous, then unknown, disposition proven
// below — not the leader's own (irrelevant) exit code.
const leaderExitCheckScriptSource = "#!/bin/sh\nsleep 300 &\necho \"backgrounded $!\"\nexit 0\n"

// configWithTimeout renders a [check]/[worker] config.toml like
// fixtureConfigTOML but with an explicit timeout and repeatable setting,
// for scenarios that need a short check timeout (to bound how long a
// deliberately ambiguous check takes to resolve) or check.repeatable=true.
func configWithTimeout(checkCommand []string, timeout string, repeatable bool) string {
	quoted := make([]string, len(checkCommand))
	for i, arg := range checkCommand {
		quoted[i] = strconv.Quote(arg)
	}
	return "[check]\n" +
		"command = [" + strings.Join(quoted, ", ") + "]\n" +
		"timeout = " + strconv.Quote(timeout) + "\n" +
		"repeatable = " + strconv.FormatBool(repeatable) + "\n" +
		"\n[worker]\n" +
		"harness = \"claude\"\n"
}

// TestRealProcessCheckLeaderExitWithLiveChildren proves design section 9's
// "leader exit with live children" against a real check pipeline: the
// check backgrounds a child and exits, so the group's stdout/stderr pipes
// never see EOF on their own — CommandRunner's capture hangs until the
// frozen check timeout (set short here) forcibly kills the whole group,
// which internal/app then treats as an ambiguous execution: the operation
// goes reconciling with the timeout-driven cancellation named as evidence,
// and the timeout's own group kill leaves nothing running behind. This
// scenario asserts that fast, reliably-observable disposition; whether and
// when a live controller's own recovery later resolves a reconciling
// check execution to a settled unknown outcome on its own (with no further
// stop or resume) is not timed here — the bounded-wait windows involved
// (design section 7's unknown-outcome rule) are long enough that pinning
// the exact automatic resolution latency belongs to a scenario that drives
// it explicitly (see TestRealProcessCheckDeathUnknownOutcomeRepeatable,
// which kills the group outright rather than waiting out a timeout).
func TestRealProcessCheckLeaderExitWithLiveChildren(t *testing.T) {
	artifacts, server := newFixtureRunEnv(t)
	repo := newFixtureRepo(t, artifacts, server, "repo")
	repo.writeFile(t, leaderExitCheckScriptName, leaderExitCheckScriptSource, 0o755)
	repo.writeFile(t, configRelPath, configWithTimeout([]string{"sh", leaderExitCheckScriptName}, "5s", false), 0o644)
	repo.commit(t, "wire a leader-exits-with-live-children check")
	fx := startRun(t, artifacts, server, repo, "submit-valid")

	if !waitUntil(func() bool {
		row := querySQLite(t, fx.dbPath(), "SELECT count(*) FROM check_exec_claims;")
		return row != "0"
	}) {
		t.Fatalf("no check-exec claim ever appeared for run %s", fx.runID)
	}
	claimPID := querySQLite(t, fx.dbPath(), "SELECT pid FROM check_exec_claims;")

	// The 5s check timeout bounds how long the ambiguous cancellation takes
	// to land; poll for the operation to leave "pending" rather than
	// assuming a fixed delay.
	if !waitUntil(func() bool {
		state := querySQLite(t, fx.dbPath(), "SELECT state FROM operations WHERE kind = 'check.run';")
		return state == "reconciling"
	}) {
		state := querySQLite(t, fx.dbPath(), "SELECT state FROM operations WHERE kind = 'check.run';")
		t.Fatalf("check.run operation never reached reconciling; last observed state = %q", state)
	}

	outcome := querySQLite(t, fx.dbPath(), "SELECT outcome FROM operations WHERE kind = 'check.run';")
	if !strings.Contains(outcome, "context deadline exceeded") {
		t.Errorf("check.run operation outcome = %q, want it to name the timeout-driven cancellation", outcome)
	}

	result := runHop(t, fx.env, fx.repo.Root, "status", "-C", fx.repo.Root, "-run", fx.runID)
	detail := parseStatusDetail(result.Stdout)
	if detail["state"] != "completing" && !strings.Contains(detail["state"], "reconciling") {
		t.Errorf("run state = %q, want \"completing (reconciling)\"", detail["state"])
	}

	// The timeout-driven cancellation kills the whole group (including the
	// backgrounded child), so nothing is left running for this test to
	// clean up.
	if pid, err := strconv.Atoi(claimPID); err == nil {
		if !waitUntil(func() bool { return len(listGroupMembers(t, pid)) == 0 }) {
			t.Errorf("process group %d still has members after the timeout-driven kill: %+v", pid, listGroupMembers(t, pid))
		}
	}
}

// checkGateScriptName names a check that blocks before deferring to
// check.sh's own pass/fail contract (exec sh check.sh, the same hand-off
// gitDependentCheckScriptSource uses), so a scenario can hold a check
// execution open indefinitely and release it at a moment of its own
// choosing rather than a check that merely runs long enough to probably
// still be running. This hand-off shares a pre-existing, narrow race with
// gitDependentCheckScriptSource: a crash-recovery poll landing in the brief
// window between this script's own exec into check.sh and that process's
// exit sees a live group member whose argv no longer matches the claimed
// intent, classifying it as mismatched rather than empty.
const checkGateScriptName = "check-gate.sh"

// checkGateScriptSource renders checkGateScriptName's body: opening gatePath
// (a FIFO releaseCheckGate creates and later opens for writing) for reading
// blocks until a writer opens it — ordinary POSIX FIFO open semantics, so
// the check's own progress depends on that rendezvous, never a fixed sleep
// a hard kill could race.
func checkGateScriptSource(gatePath string) string {
	return "#!/bin/sh\nset -eu\ncat \"" + gatePath + "\" >/dev/null\nexec sh check.sh\n"
}

// withGatedCheck rewires repo's [check] command to checkGateScriptName and
// commits the change, mirroring stop_test.go's withSlowCheck: the caller
// gets a check execution it can be certain is still in flight until it
// calls releaseCheckGate, rather than one merely long enough that a hard
// kill would probably land while it is still running.
func withGatedCheck(t *testing.T, repo *fixtureRepo, gatePath, timeout string, repeatable bool) {
	t.Helper()
	if err := syscall.Mkfifo(gatePath, 0o600); err != nil {
		t.Fatalf("create check gate fifo %s: %v", gatePath, err)
	}
	repo.writeFile(t, checkGateScriptName, checkGateScriptSource(gatePath), 0o755)
	repo.writeFile(t, configRelPath, configWithTimeout([]string{"sh", checkGateScriptName}, timeout, repeatable), 0o644)
	repo.commit(t, "wire a gated check")
}

// releaseCheckGate opens gatePath for writing, unblocking a
// checkGateScriptName execution's read through a real rendezvous with that
// process rather than a guess at how long it needed to start. check.repeatable
// reruns the identical committed script for its automatic retry, so once the
// write-open call returns — meaning the reader has already resolved gatePath
// to the FIFO's own inode, since a FIFO open blocks only after path
// resolution — gatePath is immediately swapped for an empty regular file
// (renaming a path never disturbs a file descriptor already resolved to the
// old inode): a retry's own `cat gatePath` then finds an ordinary file and
// returns at once instead of waiting for a second release that never comes.
// Bounded by conditionTimeout so a check that never reaches the gate fails
// the test instead of hanging it.
func releaseCheckGate(t *testing.T, gatePath string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		gate, err := os.OpenFile(gatePath, os.O_WRONLY, 0) //nolint:gosec // G304: gatePath is the FIFO this test created under its own artifact directory.
		if err != nil {
			done <- err
			return
		}
		replacement := gatePath + ".released"
		if err := os.WriteFile(replacement, nil, 0o600); err != nil {
			done <- err
			return
		}
		if err := os.Rename(replacement, gatePath); err != nil {
			done <- err
			return
		}
		done <- gate.Close()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("release check gate %s: %v", gatePath, err)
		}
	case <-time.After(conditionTimeout):
		t.Fatalf("check gate %s was never opened for reading before the deadline", gatePath)
	}
}

// TestRealProcessCheckDeathUnknownOutcomeRepeatable proves the other half of
// design section 9's check.repeatable contract: with check.repeatable=true,
// a check execution that dies with no observable outcome settles unknown
// but is automatically requeued — a second, fresh execution then completes
// the run normally.
//
// Getting a genuinely UNKNOWN (rather than a definite failure) outcome
// needs the check's own process group to be gone by the time something
// inspects it, with no live watcher ever having captured what actually
// happened. Directly SIGKILLing the check's own process group under a live
// watching controller does NOT produce this: CommandRunner's Wait still
// observes a completed (if killed) command and hop check-exec's exit
// status. So the run's whole controller is crash-killed instead, leaving
// the check running unwatched. withGatedCheck holds the check open on a
// FIFO so this scenario can be certain the kill lands while it is still
// running rather than racing a fast, undelayed check that might already
// have finished and been observed under load; releaseCheckGate lets it
// proceed only once the controller is confirmed dead, and the lease's own
// TTL wait comfortably outlasts the quick check.sh run that follows, so by
// the time hop resume reconciles, the group is naturally empty and nothing
// ever recorded its outcome — exactly the unknowable case the design
// describes. check.repeatable=true then requeues it automatically against
// the same committed check command, so releaseCheckGate also swaps the FIFO
// for an empty regular file before returning: the automatic retry's own
// `cat gatePath` must see an ordinary, already-at-EOF file and proceed at
// once, never block on a second release this scenario never sends. The
// resumed (still-live) controller then watches that unblocked retry to a
// normal completion.
func TestRealProcessCheckDeathUnknownOutcomeRepeatable(t *testing.T) {
	artifacts, server := newFixtureRunEnv(t)
	repo := newFixtureRepo(t, artifacts, server, "repo")
	gatePath := filepath.Join(artifacts.dir(t, "gate"), "release")
	withGatedCheck(t, repo, gatePath, "30s", true)
	fx := startRun(t, artifacts, server, repo, "submit-valid")

	if !waitUntil(func() bool {
		row := querySQLite(t, fx.dbPath(), "SELECT count(*) FROM check_exec_claims;")
		return row != "0"
	}) {
		t.Fatalf("no check-exec claim ever appeared for run %s", fx.runID)
	}
	firstOpID := querySQLite(t, fx.dbPath(), "SELECT operation_id FROM check_exec_claims LIMIT 1;")

	// The check is still blocked on the gate here — nothing has opened it
	// for writing yet — so this kill is provably concurrent with a live
	// check execution, never a race against a check that already finished.
	killControllerLeader(t, fx.controller)
	releaseCheckGate(t, gatePath)
	waitForLeaseExpiry(t, fx.dbPath(), fx.runID)

	resumed := fx.server.startHopController(t, fx.stateDir, "resume", "resume", "-C", fx.repo.Root, fx.runID)
	fx.controller, fx.controllerName = resumed, "resume"

	fx.requireRunState(t, "completed")
	waitForControllerExit(t, fx.controller, 30*time.Second)

	firstOutcome := querySQLite(t, fx.dbPath(), fmt.Sprintf(
		"SELECT outcome FROM operations WHERE id = '%s';", firstOpID))
	if !strings.Contains(firstOutcome, `"unknown":true`) {
		t.Errorf("first (orphaned) check.run outcome = %q, want an unknown outcome", firstOutcome)
	}
	checkOpCount := querySQLite(t, fx.dbPath(), "SELECT count(*) FROM operations WHERE kind = 'check.run';")
	if checkOpCount != "2" {
		t.Errorf("check.run operation count = %s, want 2 (the orphaned execution plus its automatic requeue)", checkOpCount)
	}
	runState := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM runs WHERE id = '%s';", fx.runID))
	if runState != "completed" {
		t.Errorf("run state = %q, want \"completed\"", runState)
	}
}
