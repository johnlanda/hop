package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// installBrokenClaudeStub replaces the "claude" PATH stub with a plain
// executable-bit-set text file carrying no shebang line, so the kernel's
// execve refuses it (ENOEXEC) at the exec boundary itself — after
// hop launch's PrepareLaunchExec has already resolved it (a regular,
// executable file passes that check) and written the exec_pending claim,
// but before any harness process ever runs. This is a fresh, isolated
// server root per test, so nothing here needs restoring for other
// scenarios.
func installBrokenClaudeStub(t *testing.T, server *testServer) {
	t.Helper()
	path := filepath.Join(server.base, "bin", "claude")
	if err := os.WriteFile(path, []byte("this is not a valid executable format\n"), 0o755); err != nil { //nolint:gosec // G306: a deliberately broken stub, exec-bit required to reach the exec boundary at all.
		t.Fatalf("install broken claude stub: %v", err)
	}
}

// TestRealProcessExecFailureSettlesExecFailed proves design section 6 step 7
// directly: when the harness exec itself fails (never a lookup failure — a
// broken-format executable still passes PrepareLaunchExec's regular-file-
// and-executable-bit check), hop launch settles the claim exec_failed
// itself before exiting, and the controller's own CorroborateLaunch
// observes that settled claim and fails the run immediately — Attempt and
// Task follow, the run never reaches running, and a stop issued afterward
// is a clean no-op against an already-terminal run.
func TestRealProcessExecFailureSettlesExecFailed(t *testing.T) {
	artifacts, server := newFixtureRunEnv(t)
	installBrokenClaudeStub(t, server)
	repo := newFixtureRepo(t, artifacts, "repo")
	fx := startRun(t, artifacts, server, repo, "submit-valid")

	fx.requireRunState(t, "failed")
	waitForControllerExit(t, fx.controller, 30*time.Second)

	taskID, attemptID := fx.taskAndAttemptIDs(t)
	claimState := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM launch_claims WHERE attempt_id = '%s';", attemptID))
	if claimState != "exec_failed" {
		t.Errorf("launch claim state = %q, want \"exec_failed\"", claimState)
	}
	attemptState := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM attempts WHERE id = '%s';", attemptID))
	if attemptState != "failed" {
		t.Errorf("attempt state = %q, want \"failed\" (exec_failed is unrecoverable on a first launch)", attemptState)
	}
	taskState := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM tasks WHERE id = '%s';", taskID))
	if taskState != "failed" {
		t.Errorf("task state = %q, want \"failed\"", taskState)
	}
	runState := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM runs WHERE id = '%s';", fx.runID))
	if runState != "failed" {
		t.Errorf("run state = %q, want \"failed\"", runState)
	}
	// The run must never have passed through running: exec_failed on a
	// first launch is unrecoverable, not a detour through the ordinary
	// lifecycle.
	everRunning := querySQLite(t, fx.dbPath(), fmt.Sprintf(
		"SELECT count(*) FROM transitions WHERE entity_kind = 'run' AND entity_id = '%s' AND to_state = 'running';", fx.runID))
	if everRunning != "0" {
		t.Errorf("run %s transitioned to running %s time(s) before failing, want 0", fx.runID, everRunning)
	}

	// A clean no-op: hop stop's own lease-free DriveStop path reports the
	// run's actual (already-terminal) state rather than erroring; exit 1
	// here means only "stopped was not newly achieved", not a failure to
	// process the request.
	result := runHop(t, fx.env, fx.repo.Root, "stop", "-C", fx.repo.Root, fx.runID)
	if result.ExitCode != 1 {
		t.Errorf("hop stop against an already-failed run exit=%d, want 1; stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
	if result.FirstStdoutLine() != "failed" {
		t.Errorf("hop stop against an already-failed run stdout = %q, want it to report the run's actual state \"failed\"", result.Stdout)
	}
	runStateAfterStop := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM runs WHERE id = '%s';", fx.runID))
	if runStateAfterStop != "failed" {
		t.Errorf("run state after stopping an already-failed run = %q, want unchanged \"failed\"", runStateAfterStop)
	}
}
