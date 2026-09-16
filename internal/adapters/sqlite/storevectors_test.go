package sqlite_test

import (
	"errors"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
	"github.com/johnlanda/hop/internal/testsupport/storevectors"
)

// TestStoreVectors drives every internal/testsupport/storevectors vector
// against the REAL store through the ordinary PlanStore/MessagingStore
// ports — the real-adapter half of the shared refused-input contract
// (docs/plan/phase-3-design.md section 11's countermeasure): the exact
// request internal/app's TestStoreVectors drives against fakeStore is
// refused identically here.
func TestStoreVectors(t *testing.T) {
	t.Run("TaskCreateSelfDependency", func(t *testing.T) {
		f := newFeatureFixture(t)
		got, err := f.store.CreateTask(t.Context(), storevectors.TaskCreateSelfDependency(f.spec.RunID, f.ManagerID, f.ManagerIncarnation, identity.TaskID(uid(8001))))
		if err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if got.Outcome != app.WorkflowRefused || got.Reason != storevectors.TaskCreateSelfDependencyReason {
			t.Fatalf("CreateTask(self-dependency) = %+v, want refused/%s", got, storevectors.TaskCreateSelfDependencyReason)
		}
	})

	t.Run("TaskCreateRequestIDConflict", func(t *testing.T) {
		f := newFeatureFixture(t)
		const requestID = "shared-request-id"
		first, err := f.store.CreateTask(t.Context(), storevectors.TaskCreateRequestIDConflictFirst(f.spec.RunID, f.ManagerID, f.ManagerIncarnation, identity.TaskID(uid(8002)), requestID))
		if err != nil {
			t.Fatalf("CreateTask(first) error = %v", err)
		}
		if first.Outcome != app.WorkflowAccepted {
			t.Fatalf("CreateTask(first) = %+v, want accepted", first)
		}
		second, err := f.store.CreateTask(t.Context(), storevectors.TaskCreateRequestIDConflictSecond(f.spec.RunID, f.ManagerID, f.ManagerIncarnation, identity.TaskID(uid(8003)), requestID))
		if err != nil {
			t.Fatalf("CreateTask(second) error = %v", err)
		}
		if second.Outcome != app.WorkflowRefused || second.Reason != storevectors.TaskCreateRequestIDConflictReason {
			t.Fatalf("CreateTask(second, conflicting request id) = %+v, want refused/%s", second, storevectors.TaskCreateRequestIDConflictReason)
		}
	})

	t.Run("TaskCreateNonManagerCaller", func(t *testing.T) {
		f := newFeatureFixture(t)
		taskA := f.createFeatureTask(t, 8004, 2, run.TaskActive)
		workerID, workerIncarnation := f.createWorkerSession(t, taskA, run.RoleImplementer, 8005)
		got, err := f.store.CreateTask(t.Context(), storevectors.TaskCreateNonManagerCaller(f.spec.RunID, workerID, workerIncarnation, identity.TaskID(uid(8008))))
		if err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if got.Outcome != app.WorkflowRefused || got.Reason != storevectors.TaskCreateNonManagerCallerReason {
			t.Fatalf("CreateTask(non-manager caller) = %+v, want refused/%s", got, storevectors.TaskCreateNonManagerCallerReason)
		}
	})

	t.Run("TaskCreateOversizedTitle", func(t *testing.T) {
		f := newFeatureFixture(t)
		got, err := f.store.CreateTask(t.Context(), storevectors.TaskCreateOversizedTitle(f.spec.RunID, f.ManagerID, f.ManagerIncarnation, identity.TaskID(uid(8009))))
		if err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if got.Outcome != app.WorkflowMalformed || got.Reason != storevectors.TaskCreateOversizedTitleReason {
			t.Fatalf("CreateTask(oversized title) = %+v, want malformed/%s", got, storevectors.TaskCreateOversizedTitleReason)
		}
	})

	t.Run("AckMessageStaleIncarnation", func(t *testing.T) {
		f := newMessagingFixture(t)
		send := f.workerSend(8011, run.MessageQuestion, "")
		if outcome, err := f.store.SendMessage(t.Context(), send); err != nil || outcome.Kind != app.MessageAccepted {
			t.Fatalf("seed question: %+v, %v", outcome, err)
		}
		if _, served, err := f.store.FetchNextMessage(t.Context(), f.managerFetch()); err != nil || !served {
			t.Fatalf("serve question: %t, %v", served, err)
		}
		// Supersede the manager's incarnation that actually received the
		// delivery, with no replacement: AckMessageStaleIncarnation's
		// claimed incarnation (f.ManagerIncarnation) then satisfies the
		// delivery check but fails currentBinding's currency check —
		// ErrStaleAck, not ErrNotDelivered (which an incarnation never
		// delivered to at all would hit first).
		f.inUOW(t, func(uow app.UnitOfWork) {
			binding, ok, err := uow.Bindings().Current(t.Context(), f.ManagerID)
			if err != nil || !ok {
				t.Fatalf("current binding: %v (found %t)", err, ok)
			}
			superseded, err := binding.Supersede("observed replacement occupant", f.clock.Now())
			if err != nil {
				t.Fatalf("supersede: %v", err)
			}
			if err := uow.Bindings().Save(t.Context(), superseded); err != nil {
				t.Fatalf("save superseded binding: %v", err)
			}
		})
		got, err := f.store.AckMessage(t.Context(), storevectors.AckMessageStaleIncarnation(f.spec.RunID, send.ID, f.ManagerID, f.ManagerIncarnation))
		if err != nil {
			t.Fatalf("AckMessage() error = %v", err)
		}
		if got.Kind != app.AckRefused || got.Reason != storevectors.AckMessageStaleIncarnationReason {
			t.Fatalf("AckMessage(stale incarnation) = %+v, want refused/%s", got, storevectors.AckMessageStaleIncarnationReason)
		}
	})

	t.Run("AckMessageUnknownMessage", func(t *testing.T) {
		f := newMessagingFixture(t)
		got, err := f.store.AckMessage(t.Context(), storevectors.AckMessageUnknownMessage(f.spec.RunID, identity.MessageID(uid(8018)), f.ManagerID, f.ManagerIncarnation))
		if err != nil {
			t.Fatalf("AckMessage() error = %v", err)
		}
		if got.Kind != app.AckRefused || got.Reason != storevectors.AckMessageUnknownMessageReason {
			t.Fatalf("AckMessage(unknown message) = %+v, want refused/%s", got, storevectors.AckMessageUnknownMessageReason)
		}
	})

	t.Run("AckMessageNotDelivered", func(t *testing.T) {
		f := newMessagingFixture(t)
		send := f.workerSend(8019, run.MessageQuestion, "")
		if outcome, err := f.store.SendMessage(t.Context(), send); err != nil || outcome.Kind != app.MessageAccepted {
			t.Fatalf("seed question: %+v, %v", outcome, err)
		}
		got, err := f.store.AckMessage(t.Context(), storevectors.AckMessageNotDelivered(f.spec.RunID, send.ID, f.ManagerID, f.ManagerIncarnation))
		if err != nil {
			t.Fatalf("AckMessage() error = %v", err)
		}
		if got.Kind != app.AckRefused || got.Reason != storevectors.AckMessageNotDeliveredReason {
			t.Fatalf("AckMessage(not delivered) = %+v, want refused/%s", got, storevectors.AckMessageNotDeliveredReason)
		}
	})

	t.Run("MessageSendAnswerUnknownQuestion", func(t *testing.T) {
		f := newFeatureFixture(t)
		got, err := f.store.SendMessage(t.Context(), storevectors.MessageSendAnswerUnknownQuestion(
			f.spec.RunID, f.ManagerID, run.ManagerAddress(), f.ManagerIncarnation,
			identity.MessageID(uid(8013)), identity.MessageID(uid(8014)), "/state/body.md", "digest", 3,
		))
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}
		if got.Kind != app.MessageMalformed || got.Reason != storevectors.MessageSendAnswerUnknownQuestionReason {
			t.Fatalf("SendMessage(answer to unknown question) = %+v, want malformed/%s", got, storevectors.MessageSendAnswerUnknownQuestionReason)
		}
	})

	t.Run("MessageSendCrossRun", func(t *testing.T) {
		clock := newFakeClock()
		store := openStoreAt(t, t.TempDir(), clock)
		fr1 := buildMessagingFixtureAt(t, store, clock)
		fr2 := buildSecondMessagingFixtureAt(t, store, clock)
		messageID := identity.MessageID(uid(8015))

		// fr1's manager session, but the request claims fr2 as its run.
		got, err := store.SendMessage(t.Context(), storevectors.MessageSendCrossRun(
			fr2.spec.RunID, fr1.ManagerID, run.ManagerAddress(), fr1.ManagerIncarnation, messageID, run.HumanAddress(), "/state/body.md", "digest", 3,
		))
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}
		if got.Kind != app.MessageRefused || got.Reason != storevectors.MessageSendCrossRunReason {
			t.Fatalf("SendMessage(cross-run) = %+v, want refused/%s", got, storevectors.MessageSendCrossRunReason)
		}
		if n := countRows(t, store, `SELECT COUNT(*) FROM messages WHERE id = ?`, messageID.String()); n != 0 {
			t.Fatal("SendMessage(cross-run) must not create a message")
		}
	})

	t.Run("MessageFetchCrossRun", func(t *testing.T) {
		clock := newFakeClock()
		store := openStoreAt(t, t.TempDir(), clock)
		fr1 := buildMessagingFixtureAt(t, store, clock)
		fr2 := buildSecondMessagingFixtureAt(t, store, clock)

		_, ok, err := store.FetchNextMessage(t.Context(), storevectors.MessageFetchCrossRun(
			fr2.spec.RunID, fr1.ManagerID, fr1.ManagerIncarnation, run.ManagerAddress(),
		))
		if ok {
			t.Fatal("FetchNextMessage(cross-run) reported a delivery; want refused")
		}
		if !errors.Is(err, app.ErrMessagingUnauthorized) {
			t.Fatalf("FetchNextMessage(cross-run) error = %v, want ErrMessagingUnauthorized", err)
		}
	})

	t.Run("AckMessageCrossRun", func(t *testing.T) {
		clock := newFakeClock()
		store := openStoreAt(t, t.TempDir(), clock)
		fr1 := buildMessagingFixtureAt(t, store, clock)
		fr2 := buildSecondMessagingFixtureAt(t, store, clock)
		send := fr1.workerSend(8016, run.MessageQuestion, "")
		if outcome, err := store.SendMessage(t.Context(), send); err != nil || outcome.Kind != app.MessageAccepted {
			t.Fatalf("seed question: %+v, %v", outcome, err)
		}
		// A delivery row for fr2's manager, seeded directly rather than
		// through FetchNextMessage (whose own cross-run refusal — the
		// MessageFetchCrossRun vector above — would otherwise make this row
		// impossible to create honestly): this isolates the ack-side
		// cross-run check from the ALSO-true never-delivered refusal,
		// defense in depth against exactly this kind of already-corrupt row.
		rawExec(t, store, `INSERT INTO message_deliveries (id, message_id, session_id, incarnation_id, delivered_at) VALUES (?, ?, ?, ?, '2026-09-14T10:00:00.000000000Z')`,
			uid(8017), send.ID.String(), fr2.ManagerID.String(), fr2.ManagerIncarnation.String())

		// fr2's manager session, but the request claims fr1 (the message's
		// actual run) — a cross-run ack attempt.
		got, err := store.AckMessage(t.Context(), storevectors.AckMessageCrossRun(fr1.spec.RunID, send.ID, fr2.ManagerID, fr2.ManagerIncarnation))
		if err != nil {
			t.Fatalf("AckMessage() error = %v", err)
		}
		if got.Kind != app.AckRefused || got.Reason != storevectors.AckMessageCrossRunReason {
			t.Fatalf("AckMessage(cross-run) = %+v, want refused/%s", got, storevectors.AckMessageCrossRunReason)
		}
	})
}
