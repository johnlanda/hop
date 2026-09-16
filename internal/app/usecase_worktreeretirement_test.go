package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

const retirementTarget = "refs/heads/main"

// seedRetirableRun seeds a feature run in state with the given frozen
// target and its lease released, as a finished controller leaves it.
func seedRetirableRun(t *testing.T, tc *testController, state run.RunState, target string) featureRun {
	t.Helper()
	fr := seedFeatureRun(t, tc, 2)
	row := tc.Store.Runs[fr.RunID]
	row.value.State = state
	snapshot := tc.Store.Snapshots[fr.RunID]
	snapshot.Workflow.TargetBranch = target
	tc.Store.Snapshots[fr.RunID] = snapshot
	tc.Store.Leases[fr.RunID].held = false
	return fr
}

// TestAcquireForRetirement covers the pass's lease discipline: an eligible
// terminal feature run is acquired under a fresh generation and released
// without any journal entry; every ineligible run and a live controller's
// lease are refused before the lease moves; and a stale lease-free read is
// caught under the lease, which is released again.
func TestAcquireForRetirement(t *testing.T) {
	for _, state := range []run.RunState{run.RunCompleted, run.RunFailed, run.RunStopped} {
		t.Run("eligible "+string(state)+" run: acquired, then released without a journal entry", func(t *testing.T) {
			tc := newTestController(defaultPolicy())
			fr := seedRetirableRun(t, tc, state, retirementTarget)
			transitions, operations := len(tc.Store.Transitions), len(tc.Store.Operations)

			handle, err := tc.Controller.AcquireForRetirement(context.Background(), fr.RunID.String(), "status-7")
			if err != nil {
				t.Fatalf("AcquireForRetirement() error = %v", err)
			}
			lease := tc.Store.Leases[fr.RunID]
			if handle.RunID() != fr.RunID.String() || !lease.held || lease.lease.Generation != 2 || lease.lease.ControllerID != "status-7" {
				t.Fatalf("lease after acquisition = %+v (handle %s), want held by status-7 at generation 2", lease, handle.RunID())
			}
			if err := tc.Controller.ReleaseRetirement(context.Background(), handle); err != nil {
				t.Fatalf("ReleaseRetirement() error = %v", err)
			}
			if lease := tc.Store.Leases[fr.RunID]; lease.held || lease.lease.Generation != 2 {
				t.Fatalf("lease after release = %+v, want released with the generation preserved", lease)
			}
			if len(tc.Store.Transitions) != transitions || len(tc.Store.Operations) != operations {
				t.Fatalf("the pass journaled %d transitions and %d operations, want none", len(tc.Store.Transitions)-transitions, len(tc.Store.Operations)-operations)
			}
		})
	}

	refusals := []struct {
		name  string
		setup func(t *testing.T, tc *testController) identity.RunID
	}{
		{"a running feature run", func(t *testing.T, tc *testController) identity.RunID {
			return seedRetirableRun(t, tc, run.RunRunning, retirementTarget).RunID
		}},
		{"a run frozen on a detached HEAD", func(t *testing.T, tc *testController) identity.RunID {
			return seedRetirableRun(t, tc, run.RunCompleted, "").RunID
		}},
		{"a run already retired", func(t *testing.T, tc *testController) identity.RunID {
			fr := seedRetirableRun(t, tc, run.RunCompleted, retirementTarget)
			tc.Store.WorktreesRetiredAt[fr.RunID] = time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
			return fr.RunID
		}},
		{"a solo run", func(t *testing.T, tc *testController) identity.RunID {
			fr := seedRetirableRun(t, tc, run.RunCompleted, retirementTarget)
			tc.Store.Snapshots[fr.RunID] = app.RunSnapshot{StateRoot: "/state"}
			return fr.RunID
		}},
	}
	for _, tc := range refusals {
		t.Run("refused before the lease moves, however often: "+tc.name, func(t *testing.T) {
			ctrl := newTestController(defaultPolicy())
			runID := tc.setup(t, ctrl)
			before := *ctrl.Store.Leases[runID]
			operations, transitions := len(ctrl.Store.Operations), len(ctrl.Store.Transitions)
			for range 3 {
				_, err := ctrl.Controller.AcquireForRetirement(context.Background(), runID.String(), "status-7")
				if !errors.Is(err, app.ErrRetirementNotEligible) {
					t.Fatalf("AcquireForRetirement() error = %v, want ErrRetirementNotEligible", err)
				}
			}
			if after := *ctrl.Store.Leases[runID]; after != before {
				t.Fatalf("the refusal moved the lease: %+v -> %+v", before, after)
			}
			if len(ctrl.Store.Operations) != operations || len(ctrl.Store.Transitions) != transitions {
				t.Fatalf("repeated refusals journaled %d operations and %d transitions, want none",
					len(ctrl.Store.Operations)-operations, len(ctrl.Store.Transitions)-transitions)
			}
		})
	}

	t.Run("a lease held by a live controller defers the run", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedRetirableRun(t, tc, run.RunCompleted, retirementTarget)
		tc.Store.Leases[fr.RunID].held = true
		before := *tc.Store.Leases[fr.RunID]
		_, err := tc.Controller.AcquireForRetirement(context.Background(), fr.RunID.String(), "status-7")
		if !errors.Is(err, app.ErrLeaseHeld) {
			t.Fatalf("AcquireForRetirement() error = %v, want ErrLeaseHeld", err)
		}
		if after := *tc.Store.Leases[fr.RunID]; after != before {
			t.Fatalf("the deferral moved the lease: %+v -> %+v", before, after)
		}
	})

	t.Run("a stale lease-free read is caught under the lease, which is released", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedRetirableRun(t, tc, run.RunRunning, retirementTarget)
		stale := *tc.Controller
		stale.Read = &staleTerminalReadStore{fakeStore: tc.Store}
		_, err := stale.AcquireForRetirement(context.Background(), fr.RunID.String(), "status-7")
		if !errors.Is(err, app.ErrRetirementNotEligible) {
			t.Fatalf("AcquireForRetirement() error = %v, want ErrRetirementNotEligible from the in-lease recheck", err)
		}
		if lease := tc.Store.Leases[fr.RunID]; lease.held || lease.lease.Generation != 2 {
			t.Fatalf("lease after the recheck = %+v, want acquired then released", lease)
		}
	})

	t.Run("malformed inputs are refused", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedRetirableRun(t, tc, run.RunCompleted, retirementTarget)
		if _, err := tc.Controller.AcquireForRetirement(context.Background(), "not-a-uuid", "status-7"); err == nil {
			t.Error("a malformed run id was accepted")
		}
		if _, err := tc.Controller.AcquireForRetirement(context.Background(), fr.RunID.String(), ""); err == nil {
			t.Error("an empty controller id was accepted")
		}
	})
}

// staleTerminalReadStore reports every run as completed, whatever the
// store holds: a lease-free read that went stale before acquisition.
type staleTerminalReadStore struct{ *fakeStore }

func (s *staleTerminalReadStore) LoadRunStatus(ctx context.Context, runID identity.RunID) (app.RunDetail, error) {
	detail, err := s.fakeStore.LoadRunStatus(ctx, runID)
	detail.State = run.RunCompleted
	return detail, err
}
