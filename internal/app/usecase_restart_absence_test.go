package app_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// restartedAfterRename scripts the pinned shapes of a pane a human renamed
// before a Herdr restart whose native restore is still deferred: the
// server lifetime differs from the one the placement recorded, the pane
// answers inspection with the adapter's pinned not-found shape, and its
// creation label answers nothing (the fake's label lookup finds no pane
// unless one is scripted).
func restartedAfterRename(tc *testController) {
	tc.Runtime.ServerInstanceValue = fakeServerToken(2)
	tc.Runtime.InspectPaneFn = allPanesAbsentPinned
}

// requireOutstandingContinuity requires one outstanding entry carrying the
// placed-continuity action for label.
func requireOutstandingContinuity(t *testing.T, outstanding []string, label string) {
	t.Helper()
	want := placedContinuityText(label)
	if !slices.ContainsFunc(outstanding, func(entry string) bool { return strings.Contains(entry, want) }) {
		t.Fatalf("outstanding = %q, want an entry carrying %q", outstanding, want)
	}
}

// retirementWorker seeds a feature run whose child reached a retirement
// boundary (its result accepted) with a settled launch claim.
func retirementWorker(t *testing.T) (*testController, featureRun, workerFixture, identity.TaskID, run.RuntimeBinding) {
	t.Helper()
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
	w := seedSettledWorker(t, tc, fr, taskID, 4711)
	binding, ok := tc.Store.currentBindingLocked(w.SessionID)
	if !ok {
		t.Fatalf("no binding for session %s", w.SessionID)
	}
	return tc, fr, w, taskID, binding
}

// TestStopAfterRestartConcludesNoAbsence pins stop's absence rule for a
// settled session against a renamed pane restored after a Herdr restart:
// absence by id and creation label proves nothing without the placement's
// server lifetime, so the stop stays stopping with the rename-back action,
// while the same observations under that lifetime finish it.
func TestStopAfterRestartConcludesNoAbsence(t *testing.T) {
	t.Run("solo stop of a settled worker", func(t *testing.T) {
		for _, restarted := range []bool{true, false} {
			tc := newTestController(defaultPolicy())
			handle, detail := runningRun(t, tc)
			if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
				t.Fatalf("RequestStop() error = %v", err)
			}
			tc.Runtime.InspectPaneFn = allPanesAbsentPinned
			if restarted {
				restartedAfterRename(tc)
			}
			report, err := tc.Controller.DriveStop(context.Background(), handle)
			if err != nil {
				t.Fatalf("DriveStop() error = %v", err)
			}
			if !restarted {
				if !report.Terminated {
					t.Fatalf("stop under the placement's lifetime = %+v, want terminated", report)
				}
				continue
			}
			if report.Terminated || report.RunState != string(run.RunStopping) {
				t.Fatalf("stop after a restart = %+v, want stopping", report)
			}
			requireOutstandingContinuity(t, report.Outstanding, detail.Binding.CreationLabel)
			if len(tc.Runtime.ClosedPanes) != 0 {
				t.Fatalf("ClosePane dispatched %v against an unresolved absence", tc.Runtime.ClosedPanes)
			}
		}
	})

	t.Run("solo stop of a bound worker with no claim", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := startedRun(t, tc)
		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
		restartedAfterRename(tc)
		report, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if report.Terminated {
			t.Fatalf("stop after a restart = %+v, want stopping", report)
		}
		requireOutstandingContinuity(t, report.Outstanding, detail.Binding.CreationLabel)
	})

	t.Run("feature stop of a settled child and of a claimless one", func(t *testing.T) {
		for _, restarted := range []bool{true, false} {
			tc, fr, w, _, binding := retirementWorker(t)
			claimless := seedImplementTask(t, tc, fr.RunID, 2, "B", false, run.TaskReady)
			claimlessSession, _ := seedWorkerSession(t, tc, fr, claimless)
			claimlessBinding, ok := tc.Store.currentBindingLocked(claimlessSession)
			if !ok {
				t.Fatalf("no binding for session %s", claimlessSession)
			}
			requestRunStop(tc, fr.RunID)
			tc.Runtime.InspectPaneFn = allPanesAbsentPinned
			if restarted {
				restartedAfterRename(tc)
			}
			report, err := tc.Controller.DriveFeatureStop(context.Background(), fr.Handle)
			if err != nil {
				t.Fatalf("DriveFeatureStop() error = %v", err)
			}
			if !restarted {
				if !report.Terminated {
					t.Fatalf("feature stop under the placement's lifetime = %+v, want stopped", report)
				}
				continue
			}
			if report.Terminated || report.RunState != string(run.RunStopping) {
				t.Fatalf("feature stop after a restart = %+v, want stopping", report)
			}
			requireOutstandingContinuity(t, report.Outstanding, binding.CreationLabel)
			requireOutstandingContinuity(t, report.Outstanding, claimlessBinding.CreationLabel)
			for _, id := range []identity.SessionID{w.SessionID, claimlessSession} {
				if got := tc.Store.Sessions[id].value.State; got == run.SessionTerminated {
					t.Fatalf("session %s terminated on an unresolved absence", id)
				}
			}
		}
	})
}

// TestRetirementAfterRestartConcludesNoAbsence pins the per-attempt
// retirement against the same shapes: an accepted result's session is not
// retired, and its slot not freed, on an absence observed without the
// placement's server lifetime.
func TestRetirementAfterRestartConcludesNoAbsence(t *testing.T) {
	for _, restarted := range []bool{true, false} {
		tc, fr, w, taskID, binding := retirementWorker(t)
		resultID, err := identity.ParseResultID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse result id: %v", err)
		}
		tc.Store.Results[w.AttemptID] = run.Result{ID: resultID, AttemptID: w.AttemptID, CommitOID: "c", Accepted: true, SubmittedAt: tc.Clock.Now()}
		tc.Store.Attempts[w.AttemptID].value.State = run.AttemptSubmitted
		tc.Store.Tasks[taskID].value.State = run.TaskChecking
		tc.Runtime.InspectPaneFn = allPanesAbsentPinned
		if restarted {
			restartedAfterRename(tc)
		}

		report, err := tc.Controller.RetireSettledSessions(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() error = %v", err)
		}
		if !restarted {
			if len(report.Retired) != 1 {
				t.Fatalf("retirement under the placement's lifetime = %+v, want the worker retired", report)
			}
			continue
		}
		if len(report.Retired) != 0 {
			t.Fatalf("retirement after a restart = %+v, want nothing retired", report)
		}
		requireOutstandingContinuity(t, report.Outstanding, binding.CreationLabel)
		if got := tc.Store.Sessions[w.SessionID].value.State; got == run.SessionTerminated {
			t.Fatalf("session terminated on an unresolved absence")
		}
	}
}

// TestWorkerExitAfterRestartIsNotObserved pins the live controller's
// self-exit observation against the same shapes: an in-flight settled
// worker whose pane is absent without the placement's server lifetime has
// not exited — nothing is interrupted and the manager is not notified.
func TestWorkerExitAfterRestartIsNotObserved(t *testing.T) {
	for _, restarted := range []bool{true, false} {
		tc, fr, w, taskID, _ := retirementWorker(t)
		tc.Store.Attempts[w.AttemptID].value.State = run.AttemptRunning
		tc.Store.Tasks[taskID].value.State = run.TaskActive
		tc.Runtime.InspectPaneFn = allPanesAbsentPinned
		if restarted {
			restartedAfterRename(tc)
		}

		report, err := tc.Controller.RetireSettledSessions(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() error = %v", err)
		}
		attemptState := tc.Store.Attempts[w.AttemptID].value.State
		if !restarted {
			if attemptState != run.AttemptInterrupted {
				t.Fatalf("attempt state under the placement's lifetime = %s, want the self-exit observed", attemptState)
			}
			continue
		}
		if len(report.Retired) != 0 || attemptState != run.AttemptRunning {
			t.Fatalf("after a restart: report %+v, attempt %s; want no self-exit observed", report, attemptState)
		}
		if got := tc.Store.Sessions[w.SessionID].value.State; got != run.SessionActive {
			t.Fatalf("session state = %s, want active", got)
		}
		if n := len(controllerNoticesTo(tc, fr.RunID)); n != 0 {
			t.Fatalf("manager notices = %d, want none", n)
		}
	}
}
