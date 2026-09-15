package integration

import (
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRealProcessDetachDistinctFromStop proves the design's "Controller
// signals (detach, distinct from stop)" contract directly: a SIGINT to a
// live `hop run` controller is a detach, never a stop. It releases the
// lease at once (never waiting out the lease TTL), prints the resume
// instruction and exits 1, but disturbs neither the run's state nor the
// worker: the pane's foreground process (the fixture worker, still idling)
// is untouched, and the run never records stopping or stopped.
func TestRealProcessDetachDistinctFromStop(t *testing.T) {
	fx := startFixtureRun(t, "") // empty behavior: the worker settles, then idles without ever submitting.
	fields := fx.requireRunState(t, runEndToEndTimeout, "running")

	paneID := paneIDFromBinding(fields["binding"])
	before := fx.server.processInfo(t, paneID)
	if len(before.ForegroundProcesses) == 0 {
		t.Fatalf("pane %s reports no foreground worker process before detach", paneID)
	}
	workerPID := before.ForegroundProcesses[0].PID

	if err := fx.controller.leaderCmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("send SIGINT to the hop run controller: %v", err)
	}
	status := waitForControllerExit(t, fx.controller, 10*time.Second)
	if !status.Exited() || status.ExitCode() != 1 {
		t.Errorf("hop run exit status after detach = %v, want a clean exit(1)", status)
	}

	stdout := readControllerLog(t, fx.artifacts, "run", "stdout")
	if want := "detached; the run keeps its state and the worker keeps running — resume with: hop resume " + fx.runID; !strings.Contains(stdout, want) {
		t.Errorf("controller stdout missing the detach resume instruction %q; got:\n%s", want, stdout)
	}
	if strings.Contains(stdout, "run "+fx.label+" stopping") || strings.Contains(stdout, "run "+fx.label+" stopped") {
		t.Errorf("controller stdout shows a stop transition after a detach signal; got:\n%s", stdout)
	}

	state := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM runs WHERE id = '%s';", fx.runID))
	if state != "running" {
		t.Errorf("run state after detach = %q, want unchanged \"running\"", state)
	}
	stopRequestedAt := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT ifnull(stop_requested_at, '') FROM runs WHERE id = '%s';", fx.runID))
	if stopRequestedAt != "" {
		t.Errorf("run stop_requested_at after detach = %q, want empty (never requested)", stopRequestedAt)
	}

	leaseState := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM run_leases WHERE run_id = '%s';", fx.runID))
	if leaseState != "released" {
		t.Errorf("lease state immediately after detach = %q, want \"released\" (detach releases at once, never waiting out the TTL)", leaseState)
	}

	after := fx.server.processInfo(t, paneID)
	if len(after.ForegroundProcesses) == 0 {
		t.Fatalf("pane %s reports no foreground worker process after detach; the worker must keep running", paneID)
	}
	if got := after.ForegroundProcesses[0].PID; got != workerPID {
		t.Errorf("worker pid after detach = %d, want unchanged %d (the worker must keep running, untouched by detach)", got, workerPID)
	}
}
