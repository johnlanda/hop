package integration

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// slowCheckScriptName names a check command that runs long enough for a
// scenario to observe it mid-execution before it would exit on its own.
const slowCheckScriptName = "check-slow.sh"

// slowCheckScriptSource sleeps far longer than any scenario needs it to:
// the run either kills its process group (a stop) or observes it pass on
// its own timeline, never by waiting out the sleep.
const slowCheckScriptSource = "#!/bin/sh\nsleep 20\nexit 0\n"

// withSlowCheck rewires repo's [check] command to slowCheckScriptSource and
// commits the change, so a caller starting a run against the returned
// commit gets a check execution long-lived enough to interrupt mid-flight.
func withSlowCheck(t *testing.T, repo *fixtureRepo) {
	t.Helper()
	repo.writeFile(t, slowCheckScriptName, slowCheckScriptSource, 0o755)
	repo.writeFile(t, configRelPath, fixtureConfigTOML([]string{"sh", slowCheckScriptName}), 0o644)
	repo.commit(t, "wire a slow check")
}

// TestRealProcessStopWithTerminationObserved proves design section 5's
// "Stop" contract directly against a live controller and a real herdr
// server: `hop stop` (run from a separate one-shot process, observing since
// the live `hop run` controller holds the lease and drives the stop itself)
// reports termination once observed, the run ends `stopped`, the worker's
// pane is closed, and the worktree is preserved rather than removed.
func TestRealProcessStopWithTerminationObserved(t *testing.T) {
	fx := startFixtureRun(t, "") // idling worker: stays put until stopped.
	fields := fx.requireRunState(t, "running")
	paneID := paneIDFromBinding(fields["binding"])
	worktree := fields["worktree"]
	if worktree == "" {
		t.Fatal("hop status reports no worktree before stop")
	}

	result := runHop(t, fx.env, fx.repo.Root, "stop", "-C", fx.repo.Root, fx.runID)
	if result.ExitCode != 0 {
		t.Fatalf("hop stop exit=%d, want 0; stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "stopped") {
		t.Errorf("hop stop stdout = %q, want it to report stopped", result.Stdout)
	}

	status := waitForControllerExit(t, fx.controller, 30*time.Second)
	if !status.Exited() || status.ExitCode() != 1 {
		t.Errorf("hop run exit status after being stopped = %v, want exit(1) (stopped is not completed)", status)
	}

	state := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM runs WHERE id = '%s';", fx.runID))
	if state != "stopped" {
		t.Errorf("runs.state = %q, want \"stopped\"", state)
	}
	if fx.server.paneExists(t, paneID) {
		t.Errorf("pane %s still exists after stop; the worker must be terminated", paneID)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Errorf("worktree %s missing after stop; worktrees are preserved, never removed by a stop: %v", worktree, err)
	}
}

// TestRealProcessStopInterruptsRunningCheckGroup extends the stop scenario
// with a check execution in flight: `hop stop` interrupts it before it can
// exit on its own, its process group is retired, and the run still ends
// `stopped` — stop precedence, never a completed or failed run from a check
// that was killed rather than observed to a real outcome.
func TestRealProcessStopInterruptsRunningCheckGroup(t *testing.T) {
	artifacts, server := newFixtureRunEnv(t)
	repo := newFixtureRepo(t, artifacts, "repo")
	withSlowCheck(t, repo)
	fx := startRun(t, artifacts, server, repo, "submit-valid")

	// The worker submits promptly (submit-valid); wait for its accepted
	// result to enqueue the check, then for the controller to actually claim
	// and spawn it, before stopping — otherwise stop could race ahead of the
	// check ever starting.
	fx.requireRunState(t, "completing")
	if !waitUntil(func() bool {
		lastCheck := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT count(*) FROM check_exec_claims c JOIN operations o ON o.id = c.operation_id WHERE o.run_id = '%s';", fx.runID))
		return lastCheck != "0"
	}) {
		t.Fatalf("no check-exec claim ever appeared for run %s before stopping", fx.runID)
	}

	result := runHop(t, fx.env, fx.repo.Root, "stop", "-C", fx.repo.Root, fx.runID)
	if result.ExitCode != 0 {
		t.Fatalf("hop stop exit=%d, want 0; stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "stopped") {
		t.Errorf("hop stop stdout = %q, want it to report stopped", result.Stdout)
	}

	status := waitForControllerExit(t, fx.controller, 30*time.Second)
	if !status.Exited() || status.ExitCode() != 1 {
		t.Errorf("hop run exit status after being stopped = %v, want exit(1)", status)
	}

	// The loop's own interrupt() cancels the check's context first, which
	// CommandRunner turns into a real process-group kill; ClaimAndRunCheck
	// then sees a canceled spawn and marks the operation reconciling with a
	// pure-cancellation error, which the loop's nonCancellationCauses filter
	// never prints (it is not a reportable failure). DriveStop's own retry
	// of that now-reconciling operation finds the group already gone
	// (GroupEmpty) and settles it — honestly, from that code path's own
	// perspective, as UNKNOWN rather than a distinct "interrupted" outcome,
	// since discovering an empty group cannot by itself distinguish "killed
	// by the interrupt a moment ago" from any other cause of absence. Stop
	// precedence is still what actually decided the task/attempt/run
	// outcome: both move to interrupted (never failed), and the run reaches
	// stopped, which the assertions below confirm directly against the
	// store rather than a controller stdout line that this path never prints.
	taskState := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM tasks WHERE run_id = '%s';", fx.runID))
	if taskState != "interrupted" {
		t.Errorf("task state after stop interrupted its check = %q, want \"interrupted\" (never failed)", taskState)
	}
	checkOutcome := querySQLite(t, fx.dbPath(), "SELECT outcome FROM operations WHERE kind = 'check.run';")
	if !strings.Contains(checkOutcome, `"unknown":true`) {
		t.Errorf("check.run operation outcome = %q, want an unknown outcome (the retired group cannot itself prove pass/fail)", checkOutcome)
	}

	state := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM runs WHERE id = '%s';", fx.runID))
	if state != "stopped" {
		t.Errorf("runs.state = %q, want \"stopped\" (stop precedence over a killed check's non-outcome)", state)
	}
}
