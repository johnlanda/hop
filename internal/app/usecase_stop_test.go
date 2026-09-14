package app_test

import (
	"context"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/run"
)

func TestDriveStop(t *testing.T) {
	t.Run("reserved: an attempt never launched is interrupted directly, run stops immediately", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		// A run whose worktree.create/pane.open never ran: force the
		// attempt back to reserved to simulate a stop requested before
		// StartRun's launch intent committed.
		_, handle, startErr := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if startErr != nil {
			t.Fatalf("StartRun() error = %v", startErr)
		}
		runID := tc.onlyRunID(t)
		forceAttemptReserved(t, tc, runID)

		if stopErr := tc.Controller.RequestStop(context.Background(), runID.String()); stopErr != nil {
			t.Fatalf("RequestStop() error = %v", stopErr)
		}
		report, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if !report.Terminated || report.RunState != string(run.RunStopped) {
			t.Fatalf("report = %+v, want terminated/stopped", report)
		}

		detail, err := tc.Store.LoadRunStatus(context.Background(), runID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if detail.AttemptState != run.AttemptInterrupted || detail.TaskState != run.TaskInterrupted {
			t.Fatalf("Attempt/Task = %s/%s, want both interrupted", detail.AttemptState, detail.TaskState)
		}
	})

	t.Run("launching: a pre-exec claim is retired under the close rule", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "/usr/bin/hop", Argv: []string{"hop", "launch", "--attempt", detail.AttemptID.String()}}}}, nil
		}

		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
		report, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if !report.Terminated {
			t.Fatalf("report = %+v, want terminated after retiring the pre-exec claim", report)
		}
		if len(tc.Runtime.ClosedPanes) != 1 {
			t.Fatalf("ClosePane was called %d times, want 1", len(tc.Runtime.ClosedPanes))
		}
	})

	t.Run("launching: mismatched occupant fails closed, never closes", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 9999, Argv0: "/bin/bash", Argv: []string{"bash"}}}}, nil
		}

		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
		report, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if report.Terminated {
			t.Fatalf("report = %+v, must not terminate on a mismatched occupant", report)
		}
		if len(tc.Runtime.ClosedPanes) != 0 {
			t.Fatalf("ClosePane was called despite a mismatched occupant: %v", tc.Runtime.ClosedPanes)
		}
	})

	t.Run("running: the worker pane is closed under the close rule", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)

		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "/usr/bin/claude", Argv: []string{"claude", detail.AttemptID.String()}}}}, nil
		}
		report, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if report.Terminated {
			t.Fatalf("closing does not itself prove termination; report = %+v", report)
		}
		if len(tc.Runtime.ClosedPanes) != 1 {
			t.Fatalf("ClosePane was called %d times, want 1", len(tc.Runtime.ClosedPanes))
		}

		// A later round observes the pane gone and finalizes.
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

	t.Run("checking: an orphaned check-exec group is retired via ProcessGroupInspector", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		forceAttemptChecking(t, tc, detail)

		opID := seedPendingCheckOperation(t, tc, detail.RunID, []string{"sh", "check.sh"})
		if err := tc.Store.ClaimCheckExec(context.Background(), opID, 5150); err != nil {
			t.Fatalf("ClaimCheckExec() error = %v", err)
		}
		tc.Groups.Processes[5150] = []app.GroupProcess{{PID: 5150, Argv: []string{"sh", "check.sh"}}}

		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
		report, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if !report.Terminated {
			t.Fatalf("report = %+v, want terminated after retiring the matched check group", report)
		}
		if len(tc.Groups.Signaled) != 1 || tc.Groups.Signaled[0] != 5150 {
			t.Fatalf("Signaled = %v, want [5150]", tc.Groups.Signaled)
		}
	})

	t.Run("idempotent: driving stop again after the run already stopped is a no-op", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, handle, startErr := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if startErr != nil {
			t.Fatalf("StartRun() error = %v", startErr)
		}
		runID := tc.onlyRunID(t)
		forceAttemptReserved(t, tc, runID)
		if stopErr := tc.Controller.RequestStop(context.Background(), runID.String()); stopErr != nil {
			t.Fatalf("RequestStop() error = %v", stopErr)
		}
		if _, driveErr := tc.Controller.DriveStop(context.Background(), handle); driveErr != nil {
			t.Fatalf("first DriveStop() error = %v", driveErr)
		}
		report, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("second DriveStop() error = %v", err)
		}
		if !report.Terminated {
			t.Fatalf("repeated DriveStop() must stay terminated: %+v", report)
		}
	})
}
