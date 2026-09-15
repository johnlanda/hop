package integration

import (
	"fmt"
	"strconv"
	"strings"
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
	repo := newFixtureRepo(t, artifacts, "repo")
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
// check.sh (an ordinary, fast-passing check with no artificial delay)
// running unwatched; the lease's own TTL wait comfortably outlasts it, so
// by the time hop resume reconciles, the group is naturally empty and
// nothing ever recorded its outcome — exactly the unknowable case the
// design describes. check.repeatable=true then requeues it automatically,
// and the resumed (still-live) controller watches the fresh retry to a
// normal completion.
func TestRealProcessCheckDeathUnknownOutcomeRepeatable(t *testing.T) {
	artifacts, server := newFixtureRunEnv(t)
	repo := newFixtureRepo(t, artifacts, "repo")
	repo.writeFile(t, configRelPath, configWithTimeout([]string{"sh", checkScriptName}, "30s", true), 0o644)
	repo.commit(t, "wire a repeatable check")
	fx := startRun(t, artifacts, server, repo, "submit-valid")

	if !waitUntil(func() bool {
		row := querySQLite(t, fx.dbPath(), "SELECT count(*) FROM check_exec_claims;")
		return row != "0"
	}) {
		t.Fatalf("no check-exec claim ever appeared for run %s", fx.runID)
	}
	firstOpID := querySQLite(t, fx.dbPath(), "SELECT operation_id FROM check_exec_claims LIMIT 1;")

	killControllerLeader(t, fx.controller)
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
