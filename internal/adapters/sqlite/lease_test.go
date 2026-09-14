package sqlite_test

import (
	"errors"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
	"github.com/johnlanda/hop/internal/app"
)

// leaseTTL mirrors the store's default heartbeat TTL.
const leaseTTL = 30 * time.Second

// TestInitializeRunCreatesInitialLease proves the run's first lease is
// created by InitializeRun at generation 1, held, and immediately usable.
func TestInitializeRunCreatesInitialLease(t *testing.T) {
	f := newFixture(t)

	if f.lease.Generation != 1 {
		t.Fatalf("initial lease generation = %d, want 1", f.lease.Generation)
	}
	if err := f.store.Heartbeat(t.Context(), f.lease); err != nil {
		t.Fatalf("heartbeat on the initial lease: %v", err)
	}
}

// TestAcquireLeaseRefusedWhileHeld proves acquisition fails with
// ErrLeaseHeld while a live (unexpired, held) lease exists.
func TestAcquireLeaseRefusedWhileHeld(t *testing.T) {
	f := newFixture(t)

	_, err := f.store.AcquireLease(t.Context(), f.spec.RunID, "controller-b")

	if !errors.Is(err, app.ErrLeaseHeld) {
		t.Fatalf("AcquireLease on a held lease = %v, want ErrLeaseHeld", err)
	}
}

// TestLeaseTakeoverAfterExpiry proves a second controller, on its own
// handle, takes over once the holder's lease expires, and that the stale
// holder can then neither heartbeat, release nor begin.
func TestLeaseTakeoverAfterExpiry(t *testing.T) {
	f := newFixture(t)

	f.clock.Advance(leaseTTL + time.Second)
	takeover, err := f.store.AcquireLease(t.Context(), f.spec.RunID, "controller-b")
	if err != nil {
		t.Fatalf("takeover after expiry: %v", err)
	}

	if takeover.Generation != 2 {
		t.Fatalf("takeover generation = %d, want 2", takeover.Generation)
	}
	if err := f.store.Heartbeat(t.Context(), f.lease); !errors.Is(err, app.ErrFenced) {
		t.Fatalf("stale holder heartbeat = %v, want ErrFenced", err)
	}
	if err := f.store.ReleaseLease(t.Context(), f.lease); !errors.Is(err, app.ErrFenced) {
		t.Fatalf("stale holder release = %v, want ErrFenced", err)
	}
	if _, err := f.store.Begin(t.Context(), f.lease); !errors.Is(err, app.ErrFenced) {
		t.Fatalf("stale holder begin = %v, want ErrFenced", err)
	}
}

// TestStaleHeartbeatFromSecondHandle proves the CAS refusal across store
// handles standing in for separate processes: controller A's handle cannot
// extend the lease its successor holds.
func TestStaleHeartbeatFromSecondHandle(t *testing.T) {
	clock := newFakeClock()
	root := t.TempDir()
	storeA := openStoreAt(t, root, clock)
	storeB := openStoreAt(t, root, clock)
	spec := newSpec("/repos/alpha", specStride, clock.Now())
	_, leaseA, err := storeA.InitializeRun(t.Context(), spec)
	if err != nil {
		t.Fatalf("InitializeRun: %v", err)
	}
	clock.Advance(leaseTTL + time.Second)
	if _, takeoverErr := storeB.AcquireLease(t.Context(), spec.RunID, "controller-b"); takeoverErr != nil {
		t.Fatalf("takeover from second handle: %v", takeoverErr)
	}

	err = storeA.Heartbeat(t.Context(), leaseA)

	if !errors.Is(err, app.ErrFenced) {
		t.Fatalf("stale heartbeat from first handle = %v, want ErrFenced", err)
	}
}

// TestReleasePreservesGenerationAndRow proves release marks the row
// released without deleting it or touching the generation, and that
// reacquisition continues the monotonic generation sequence.
func TestReleasePreservesGenerationAndRow(t *testing.T) {
	f := newFixture(t)

	if err := f.store.ReleaseLease(t.Context(), f.lease); err != nil {
		t.Fatalf("release: %v", err)
	}

	var (
		state      string
		generation int64
	)
	err := sqlite.WriteDB(f.store).QueryRowContext(t.Context(),
		`SELECT state, generation FROM run_leases WHERE run_id = ?`, f.spec.RunID.String(),
	).Scan(&state, &generation)
	if err != nil {
		t.Fatalf("released lease row is gone: %v", err)
	}
	if state != "released" || generation != 1 {
		t.Fatalf("released row is (%s, generation %d), want (released, 1)", state, generation)
	}
	reacquired, err := f.store.AcquireLease(t.Context(), f.spec.RunID, "controller-b")
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if reacquired.Generation != 2 {
		t.Fatalf("generation after release and reacquire = %d, want 2", reacquired.Generation)
	}
}

// TestReleaseIsCASNotIdempotent proves a second release of the same lease
// value is fenced: the row no longer matches (held).
func TestReleaseIsCASNotIdempotent(t *testing.T) {
	f := newFixture(t)
	if err := f.store.ReleaseLease(t.Context(), f.lease); err != nil {
		t.Fatalf("first release: %v", err)
	}

	err := f.store.ReleaseLease(t.Context(), f.lease)

	if !errors.Is(err, app.ErrFenced) {
		t.Fatalf("second release = %v, want ErrFenced", err)
	}
}

// TestCommitRejectedAfterRelease proves Commit's re-read rejects a lease
// that is no longer held at commit time. The release is written inside the
// open unit of work's own transaction (via the test hook), because every
// other writer serializes behind the open immediate transaction — this is
// exactly the re-read window the contract fences.
func TestCommitRejectedAfterRelease(t *testing.T) {
	f := newFixture(t)
	uow, err := f.store.Begin(t.Context(), f.lease)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, execErr := sqlite.TxOf(uow).ExecContext(t.Context(),
		`UPDATE run_leases SET state = 'released' WHERE run_id = ?`, f.spec.RunID.String(),
	); execErr != nil {
		t.Fatalf("release inside the transaction: %v", execErr)
	}

	err = uow.Commit()

	if !errors.Is(err, app.ErrFenced) {
		t.Fatalf("commit after release = %v, want ErrFenced", err)
	}
}

// TestCommitRejectedAtExpiry proves an open unit of work whose lease
// expires before commit is fenced at commit time by the parsed expiry
// against the commit-time clock reading.
func TestCommitRejectedAtExpiry(t *testing.T) {
	f := newFixture(t)
	uow, err := f.store.Begin(t.Context(), f.lease)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	f.clock.Advance(leaseTTL + time.Second)

	err = uow.Commit()

	if !errors.Is(err, app.ErrFenced) {
		t.Fatalf("commit after expiry = %v, want ErrFenced", err)
	}
}

// TestCommitSucceedsBeforeExpiry proves the same open unit of work commits
// while the lease is live, so the expiry rejection above is about time, not
// about the transaction shape.
func TestCommitSucceedsBeforeExpiry(t *testing.T) {
	f := newFixture(t)
	uow, err := f.store.Begin(t.Context(), f.lease)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	f.clock.Advance(leaseTTL - time.Second)

	if err := uow.Commit(); err != nil {
		t.Fatalf("commit before expiry: %v", err)
	}
}

// TestHeartbeatExtendsExpiry proves a heartbeat advances expires_at, so a
// heartbeating controller's lease never lapses.
func TestHeartbeatExtendsExpiry(t *testing.T) {
	f := newFixture(t)
	f.clock.Advance(leaseTTL - time.Second)
	if err := f.store.Heartbeat(t.Context(), f.lease); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	f.clock.Advance(leaseTTL - time.Second)

	_, err := f.store.AcquireLease(t.Context(), f.spec.RunID, "controller-b")

	if !errors.Is(err, app.ErrLeaseHeld) {
		t.Fatalf("acquisition after a fresh heartbeat = %v, want ErrLeaseHeld", err)
	}
}
