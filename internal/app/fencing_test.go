package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// TestLeaseFencing proves the takeover barrier directly against the store:
// a unit of work opened under a lease that a later AcquireLease has since
// superseded cannot commit, and a stale controller can neither heartbeat
// nor release a lease a successor now holds. "Fencing the outcome commit
// does not fence the act itself" (docs/plan/phase-2-design.md section 4) —
// this is the mechanism the decision table relies on.
func TestLeaseFencing(t *testing.T) {
	t.Run("takeover barrier: A's unit of work cannot commit once B has taken over", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, _, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if err != nil {
			t.Fatalf("StartRun() error = %v", err)
		}
		runID := tc.onlyRunID(t)
		leaseA := tc.Store.Leases[runID].lease

		uowA, err := tc.Store.Begin(context.Background(), leaseA)
		if err != nil {
			t.Fatalf("Begin() (A) error = %v", err)
		}

		// A crashed; its lease expires, and B takes over with a new
		// generation while A's unit of work is still open.
		tc.Clock.Advance(leaseTTL + time.Second)
		leaseB, err := tc.Store.AcquireLease(context.Background(), runID, "controller-B")
		if err != nil {
			t.Fatalf("AcquireLease() (B) error = %v", err)
		}
		if leaseB.Generation != leaseA.Generation+1 {
			t.Fatalf("B's generation = %d, want %d", leaseB.Generation, leaseA.Generation+1)
		}

		// A, unaware it was superseded, tries to commit its already-open
		// unit of work: its act may have already run (fencing the outcome
		// commit does not fence the act), but the commit itself must fail.
		if commitErr := uowA.Commit(); !errors.Is(commitErr, app.ErrFenced) {
			t.Fatalf("A's Commit() error = %v, want ErrFenced", commitErr)
		}

		// B, the legitimate holder, can still open and commit its own work.
		uowB, err := tc.Store.Begin(context.Background(), leaseB)
		if err != nil {
			t.Fatalf("Begin() (B) error = %v", err)
		}
		if err := uowB.Commit(); err != nil {
			t.Fatalf("B's Commit() error = %v, want nil", err)
		}
	})

	t.Run("heartbeat and release refuse a stale controller", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, _, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if err != nil {
			t.Fatalf("StartRun() error = %v", err)
		}
		runID := tc.onlyRunID(t)
		leaseA := tc.Store.Leases[runID].lease

		tc.Clock.Advance(leaseTTL + time.Second)
		leaseB, err := tc.Store.AcquireLease(context.Background(), runID, "controller-B")
		if err != nil {
			t.Fatalf("AcquireLease() (B) error = %v", err)
		}

		if err := tc.Store.Heartbeat(context.Background(), leaseA); !errors.Is(err, app.ErrFenced) {
			t.Fatalf("A's Heartbeat() error = %v, want ErrFenced", err)
		}
		if err := tc.Store.ReleaseLease(context.Background(), leaseA); !errors.Is(err, app.ErrFenced) {
			t.Fatalf("A's ReleaseLease() error = %v, want ErrFenced", err)
		}

		if err := tc.Store.Heartbeat(context.Background(), leaseB); err != nil {
			t.Fatalf("B's Heartbeat() error = %v, want nil", err)
		}
		if err := tc.Store.ReleaseLease(context.Background(), leaseB); err != nil {
			t.Fatalf("B's ReleaseLease() error = %v, want nil", err)
		}
	})

	t.Run("AcquireLease refuses to take over a live, unexpired lease", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, _, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if err != nil {
			t.Fatalf("StartRun() error = %v", err)
		}
		runID := tc.onlyRunID(t)

		if _, err := tc.Store.AcquireLease(context.Background(), runID, "controller-B"); !errors.Is(err, app.ErrLeaseHeld) {
			t.Fatalf("AcquireLease() error = %v, want ErrLeaseHeld", err)
		}
	})

	t.Run("AcquireLease succeeds once released, generation still monotonic", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, _, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if err != nil {
			t.Fatalf("StartRun() error = %v", err)
		}
		runID := tc.onlyRunID(t)
		leaseA := tc.Store.Leases[runID].lease

		if releaseErr := tc.Store.ReleaseLease(context.Background(), leaseA); releaseErr != nil {
			t.Fatalf("ReleaseLease() error = %v", releaseErr)
		}
		leaseB, err := tc.Store.AcquireLease(context.Background(), runID, "controller-B")
		if err != nil {
			t.Fatalf("AcquireLease() error = %v", err)
		}
		if leaseB.Generation != leaseA.Generation+1 {
			t.Fatalf("generation = %d, want %d (monotonic across release)", leaseB.Generation, leaseA.Generation+1)
		}
	})
}
