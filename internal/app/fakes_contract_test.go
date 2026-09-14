package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
)

// TestFakeStoreContracts proves the handwritten fakes enforce the store
// contracts the scenarios rely on (docs/plan/phase-2-design.md sections 4
// and 7): validation happens before any write is applied, so a failed
// commit or refused claim leaves base state unchanged.
func TestFakeStoreContracts(t *testing.T) {
	t.Run("commit rejects a duplicate runtime binding key and applies nothing", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		lease := tc.Store.Leases[detail.RunID].lease
		before := len(tc.Store.Bindings[detail.SessionID])
		transitionsBefore := len(tc.Store.Transitions)

		uow, err := tc.Store.Begin(context.Background(), lease)
		if err != nil {
			t.Fatalf("Begin() error = %v", err)
		}
		// The same (session, incarnation) key the pane.open outcome already
		// recorded, staged alongside an unrelated write.
		if err := uow.Bindings().Create(context.Background(), *detail.Binding); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		if err := uow.Transitions().Record(context.Background(), app.Transition{EntityKind: app.EntityRun, EntityID: detail.RunID.String()}); err != nil {
			t.Fatalf("Record() error = %v", err)
		}
		if err := uow.Commit(); err == nil {
			t.Fatalf("Commit() accepted a duplicate UNIQUE(session_id, incarnation_id) binding key")
		}
		if got := len(tc.Store.Bindings[detail.SessionID]); got != before {
			t.Fatalf("binding history length = %d after failed commit, want %d (nothing applied)", got, before)
		}
		if got := len(tc.Store.Transitions); got != transitionsBefore {
			t.Fatalf("transitions length = %d after failed commit, want %d (nothing applied)", got, transitionsBefore)
		}
	})

	t.Run("commit rechecks revisions read at begin against the store's current rows", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		lease := tc.Store.Leases[detail.RunID].lease

		uowA, err := tc.Store.Begin(context.Background(), lease)
		if err != nil {
			t.Fatalf("Begin() (A) error = %v", err)
		}
		rA, revA, err := uowA.Runs().Get(context.Background(), detail.RunID)
		if err != nil {
			t.Fatalf("Get() (A) error = %v", err)
		}
		if _, saveErr := uowA.Runs().Save(context.Background(), rA, revA); saveErr != nil {
			t.Fatalf("Save() (A) error = %v", saveErr)
		}

		// B reads and commits the same row first.
		uowB, err := tc.Store.Begin(context.Background(), lease)
		if err != nil {
			t.Fatalf("Begin() (B) error = %v", err)
		}
		rB, revB, err := uowB.Runs().Get(context.Background(), detail.RunID)
		if err != nil {
			t.Fatalf("Get() (B) error = %v", err)
		}
		if _, err := uowB.Runs().Save(context.Background(), rB, revB); err != nil {
			t.Fatalf("Save() (B) error = %v", err)
		}
		if err := uowB.Commit(); err != nil {
			t.Fatalf("Commit() (B) error = %v", err)
		}
		movedRevision := tc.Store.Runs[detail.RunID].revision

		if err := uowA.Commit(); !errors.Is(err, app.ErrRevisionConflict) {
			t.Fatalf("Commit() (A) error = %v, want ErrRevisionConflict (row moved since A's read)", err)
		}
		if got := tc.Store.Runs[detail.RunID].revision; got != movedRevision {
			t.Fatalf("run revision = %d after A's failed commit, want %d (B's write intact)", got, movedRevision)
		}
	})

	t.Run("commit at the lease expiry instant is fenced (strict expiry)", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		lease := tc.Store.Leases[detail.RunID].lease

		uow, err := tc.Store.Begin(context.Background(), lease)
		if err != nil {
			t.Fatalf("Begin() error = %v", err)
		}
		tc.Clock.Advance(lease.ExpiresAt.Sub(tc.Clock.Now())) // exactly the expiry instant
		if err := uow.Commit(); !errors.Is(err, app.ErrFenced) {
			t.Fatalf("Commit() at the expiry instant error = %v, want ErrFenced", err)
		}
	})

	t.Run("ClaimLaunch refuses a run with a stop request", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
		err := tc.Store.ClaimLaunch(context.Background(), app.LaunchClaim{
			IncarnationID: detail.Binding.IncarnationID, RunID: detail.RunID, AttemptID: detail.AttemptID,
			Executable: "/usr/bin/claude", PID: 4242, State: app.LaunchClaimExecPending,
		})
		if err == nil {
			t.Fatalf("ClaimLaunch() accepted a claim for a stopping run")
		}
	})

	t.Run("ClaimLaunch refuses a non-current incarnation", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		stale, err := identity.ParseIncarnationID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse incarnation id: %v", err)
		}
		claimErr := tc.Store.ClaimLaunch(context.Background(), app.LaunchClaim{
			IncarnationID: stale, RunID: detail.RunID, AttemptID: detail.AttemptID,
			Executable: "/usr/bin/claude", PID: 4242, State: app.LaunchClaimExecPending,
		})
		if claimErr == nil {
			t.Fatalf("ClaimLaunch() accepted a claim for a non-current incarnation")
		}
	})

	t.Run("ClaimLaunch same-pid rewrite is idempotent; different pid is refused", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		claimLaunch(t, tc, detail, 4242) // idempotent rewrite
		err := tc.Store.ClaimLaunch(context.Background(), app.LaunchClaim{
			IncarnationID: detail.Binding.IncarnationID, RunID: detail.RunID, AttemptID: detail.AttemptID,
			Executable: "/usr/bin/claude", PID: 9999, State: app.LaunchClaimExecPending,
		})
		if err == nil {
			t.Fatalf("ClaimLaunch() accepted a second claim with a different pid")
		}
	})

	t.Run("ClaimCheckExec refuses a non-pending operation and a prior generation", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)

		settled := seedPendingCheckOperation(t, tc, detail.RunID, []string{"sh", "check.sh"})
		op := tc.Store.Operations[settled]
		op.State = app.OperationSucceeded
		tc.Store.Operations[settled] = op
		if err := tc.Store.ClaimCheckExec(context.Background(), settled, 5150); err == nil {
			t.Fatalf("ClaimCheckExec() accepted a settled operation")
		}

		priorGen := seedPendingCheckOperation(t, tc, detail.RunID, []string{"sh", "check.sh"})
		op = tc.Store.Operations[priorGen]
		op.Generation--
		tc.Store.Operations[priorGen] = op
		if err := tc.Store.ClaimCheckExec(context.Background(), priorGen, 5150); err == nil {
			t.Fatalf("ClaimCheckExec() accepted an operation of a prior generation")
		}
	})

	t.Run("SubmitResult checks existence and agreement before duplicate receipts", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		req := defaultSubmitRequest(detail)
		if _, err := tc.Controller.SubmitResult(context.Background(), req); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}

		// The identical content, resubmitted under a task id the attempt
		// does not belong to: agreement fails before the receipt lookup, so
		// the outcome is malformed, never an idempotent duplicate.
		wrongTask, err := identity.ParseTaskID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse task id: %v", err)
		}
		disagreeing := req
		disagreeing.TaskID = wrongTask.String()
		result, err := tc.Controller.SubmitResult(context.Background(), disagreeing)
		if err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		if result.Kind != string(app.SubmissionMalformed) {
			t.Fatalf("Kind = %s, want %s (agreement precedes receipts)", result.Kind, app.SubmissionMalformed)
		}
	})

	t.Run("expired lease still refuses acquisition before its expiry instant", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		tc.Clock.Advance(leaseTTL - time.Second)
		if _, err := tc.Store.AcquireLease(context.Background(), detail.RunID, "controller-B"); !errors.Is(err, app.ErrLeaseHeld) {
			t.Fatalf("AcquireLease() error = %v, want ErrLeaseHeld while the lease is unexpired", err)
		}
	})
}
