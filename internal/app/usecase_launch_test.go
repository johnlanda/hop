package app_test

import (
	"context"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/run"
)

// startedRun drives StartRun to completion and returns the handle plus the
// binding it produced, for tests that need to corroborate, stop or check
// against an already-launching attempt.
func startedRun(t *testing.T, tc *testController) (app.RunHandle, app.RunDetail) {
	t.Helper()
	_, handle, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
	if err != nil {
		t.Fatalf("StartRun() error = %v", err)
	}
	detail, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
	if err != nil {
		t.Fatalf("LoadRunStatus() error = %v", err)
	}
	if detail.Binding == nil {
		t.Fatalf("StartRun did not record a runtime binding")
	}
	return handle, detail
}

// claim writes a launch claim for the started run's incarnation, as hop
// launch would before exec.
func claimLaunch(t *testing.T, tc *testController, detail app.RunDetail, pid int) { //nolint:gocritic,unparam // hugeParam: detail is the test's already-loaded RunDetail, passed once per call in test setup, never a hot path. unparam: every current test scripts the same launcher pid, but the parameter documents that InspectPaneFn stubs must match whatever pid is claimed here.
	t.Helper()
	if err := tc.Store.ClaimLaunch(context.Background(), app.LaunchClaim{
		IncarnationID: detail.Binding.IncarnationID, RunID: detail.RunID, AttemptID: detail.AttemptID,
		Executable: "/usr/bin/claude", PID: pid, State: app.LaunchClaimExecPending,
	}); err != nil {
		t.Fatalf("ClaimLaunch() error = %v", err)
	}
}

func TestCorroborateLaunch(t *testing.T) {
	const marker = "attempt-marker"

	t.Run("pending: no claim written yet", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, _ := startedRun(t, tc)
		progress, err := tc.Controller.CorroborateLaunch(context.Background(), handle, "/usr/bin/claude", marker)
		if err != nil {
			t.Fatalf("CorroborateLaunch() error = %v", err)
		}
		if progress != app.LaunchPending {
			t.Fatalf("progress = %s, want %s", progress, app.LaunchPending)
		}
	})

	t.Run("settled: matching inspection moves Run/Attempt to running", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "/usr/bin/claude", Argv: []string{"claude", marker}}}}, nil
		}

		progress, err := tc.Controller.CorroborateLaunch(context.Background(), handle, "/usr/bin/claude", marker)
		if err != nil {
			t.Fatalf("CorroborateLaunch() error = %v", err)
		}
		if progress != app.LaunchSettled {
			t.Fatalf("progress = %s, want %s", progress, app.LaunchSettled)
		}

		updated, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.State != run.RunRunning {
			t.Fatalf("Run.State = %s, want %s", updated.State, run.RunRunning)
		}
		if updated.AttemptState != run.AttemptRunning {
			t.Fatalf("Attempt.State = %s, want %s", updated.AttemptState, run.AttemptRunning)
		}
		if tc.Store.LaunchClaims[detail.Binding.IncarnationID].State != app.LaunchClaimExeced {
			t.Fatalf("launch claim was not settled to execed")
		}
	})

	t.Run("needs interaction: forking-wrapper topology never settles", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 9999, Argv0: "/usr/bin/claude", Argv: []string{"claude", marker}}}}, nil
		}

		progress, err := tc.Controller.CorroborateLaunch(context.Background(), handle, "/usr/bin/claude", marker)
		if err != nil {
			t.Fatalf("CorroborateLaunch() error = %v", err)
		}
		if progress != app.LaunchNeedsInteraction {
			t.Fatalf("progress = %s, want %s", progress, app.LaunchNeedsInteraction)
		}
		if tc.Store.LaunchClaims[detail.Binding.IncarnationID].State != app.LaunchClaimExecPending {
			t.Fatalf("a forking-wrapper observation must never settle the claim")
		}
	})

	t.Run("failed: exec_failed claim fails Attempt, Task and Run", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		if err := tc.Store.SettleLaunchFailure(context.Background(), detail.Binding.IncarnationID, "exec: no such file"); err != nil {
			t.Fatalf("SettleLaunchFailure() error = %v", err)
		}

		progress, err := tc.Controller.CorroborateLaunch(context.Background(), handle, "/usr/bin/claude", marker)
		if err != nil {
			t.Fatalf("CorroborateLaunch() error = %v", err)
		}
		if progress != app.LaunchFailed {
			t.Fatalf("progress = %s, want %s", progress, app.LaunchFailed)
		}

		updated, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.State != run.RunFailed || updated.TaskState != run.TaskFailed || updated.AttemptState != run.AttemptFailed {
			t.Fatalf("Run/Task/Attempt = %s/%s/%s, want all failed", updated.State, updated.TaskState, updated.AttemptState)
		}
	})
}
