package app_test

import (
	"context"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/run"
)

func TestClaimAndRunCheck(t *testing.T) {
	checkArgv := []string{"sh", "check.sh"}

	t.Run("no pending check request: a no-op", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, _ := runningRun(t, tc)
		report, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", "/repo", "/state", checkArgv, false, nil)
		if err != nil {
			t.Fatalf("ClaimAndRunCheck() error = %v", err)
		}
		if report.Ran {
			t.Fatalf("report = %+v, want Ran=false with no pending request", report)
		}
	})

	t.Run("passing check completes Run, Task and Attempt", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}

		report, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", "/repo", "/state", checkArgv, false, nil)
		if err != nil {
			t.Fatalf("ClaimAndRunCheck() error = %v", err)
		}
		if !report.Ran || !report.Passed {
			t.Fatalf("report = %+v, want Ran/Passed", report)
		}

		updated, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.State != run.RunCompleted || updated.TaskState != run.TaskCompleted || updated.AttemptState != run.AttemptCompleted {
			t.Fatalf("Run/Task/Attempt = %s/%s/%s, want all completed", updated.State, updated.TaskState, updated.AttemptState)
		}
	})

	t.Run("failing check fails Run, Task and Attempt, never completes", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		tc.Commands.CheckExecExitCode = 1

		report, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", "/repo", "/state", checkArgv, false, nil)
		if err != nil {
			t.Fatalf("ClaimAndRunCheck() error = %v", err)
		}
		if !report.Ran || report.Passed {
			t.Fatalf("report = %+v, want Ran and not Passed", report)
		}

		updated, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.State != run.RunFailed || updated.TaskState != run.TaskFailed || updated.AttemptState != run.AttemptFailed {
			t.Fatalf("Run/Task/Attempt = %s/%s/%s, want all failed", updated.State, updated.TaskState, updated.AttemptState)
		}
	})

	t.Run("unrepeatable unknown outcome never completes and leaves the run actionable", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		tc.Commands.CheckExecErr = context.DeadlineExceeded

		report, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", "/repo", "/state", checkArgv, false, nil)
		if err == nil {
			t.Fatalf("ClaimAndRunCheck() succeeded despite an unknown outcome")
		}
		if !report.Ran || !report.Unknown {
			t.Fatalf("report = %+v, want Ran and Unknown", report)
		}

		updated, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.State != run.RunFailed || updated.AttemptState != run.AttemptFailed {
			t.Fatalf("Run/Attempt = %s/%s, an unrepeatable unknown outcome must never complete", updated.State, updated.AttemptState)
		}
	})

	t.Run("repeatable unknown outcome requeues a fresh execution from completing back to running", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		tc.Commands.CheckExecErr = context.DeadlineExceeded

		report, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", "/repo", "/state", checkArgv, true, nil)
		if err == nil {
			t.Fatalf("ClaimAndRunCheck() succeeded despite an unknown outcome")
		}
		if !report.Ran || !report.Unknown {
			t.Fatalf("report = %+v, want Ran and Unknown", report)
		}

		updated, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.State != run.RunRunning {
			t.Fatalf("Run.State = %s, want %s (repeatable unknown outcome requeues)", updated.State, run.RunRunning)
		}
	})

	t.Run("stop precedence: a stop requested during the check interrupts rather than completes", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		// The stop request arrives while the check command is executing: the
		// outcome transaction re-reads the stop flag and applies precedence.
		tc.Commands.CheckExecFn = func(ctx context.Context, _ app.Command) (app.CommandResult, error) {
			if err := tc.Controller.RequestStop(ctx, detail.RunID.String()); err != nil {
				t.Errorf("RequestStop() during check error = %v", err)
			}
			return app.CommandResult{ExitCode: 0}, nil
		}

		report, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", "/repo", "/state", checkArgv, false, nil)
		if err != nil {
			t.Fatalf("ClaimAndRunCheck() error = %v", err)
		}
		if !report.Ran || !report.Interrupted || report.Passed {
			t.Fatalf("report = %+v, want Ran/Interrupted and not Passed (stop precedence)", report)
		}

		updated, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.State != run.RunStopping {
			t.Fatalf("Run.State = %s, want %s: the check outcome never declares stopped while the worker may be live", updated.State, run.RunStopping)
		}
		if updated.AttemptState != run.AttemptInterrupted || updated.TaskState != run.TaskInterrupted {
			t.Fatalf("Attempt/Task = %s/%s, want both interrupted", updated.AttemptState, updated.TaskState)
		}

		// DriveStop then observes the worker gone and completes the stop.
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, nil
		}
		final, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if !final.Terminated || final.RunState != string(run.RunStopped) {
			t.Fatalf("final report = %+v, want terminated/stopped", final)
		}
	})

	t.Run("a stop request already recorded refuses to start unstarted check work", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}

		report, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", "/repo", "/state", checkArgv, false, nil)
		if err != nil {
			t.Fatalf("ClaimAndRunCheck() error = %v", err)
		}
		if report.Ran {
			t.Fatalf("report = %+v, want Ran=false: unstarted work is never started after stop", report)
		}
		for _, cmd := range tc.Commands.Calls {
			if len(cmd.Argv) >= 2 && cmd.Argv[1] == "check-exec" {
				t.Fatalf("hop check-exec was spawned despite the stop request")
			}
		}
		for _, cr := range tc.Store.CheckRequests {
			if cr.State != app.CheckRequestRequested {
				t.Fatalf("check request state = %s, want requested (left for stop handling)", cr.State)
			}
		}
	})
}
