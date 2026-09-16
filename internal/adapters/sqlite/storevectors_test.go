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
		if got.Outcome != app.WorkflowRefused {
			t.Fatalf("CreateTask(self-dependency) = %+v, want refused", got)
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
		if second.Outcome != app.WorkflowRefused {
			t.Fatalf("CreateTask(second, conflicting request id) = %+v, want refused", second)
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
		if got.Outcome != app.WorkflowRefused {
			t.Fatalf("CreateTask(non-manager caller) = %+v, want refused", got)
		}
	})

	t.Run("TaskCreateOversizedTitle", func(t *testing.T) {
		f := newFeatureFixture(t)
		got, err := f.store.CreateTask(t.Context(), storevectors.TaskCreateOversizedTitle(f.spec.RunID, f.ManagerID, f.ManagerIncarnation, identity.TaskID(uid(8009))))
		if err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if got.Outcome != app.WorkflowMalformed {
			t.Fatalf("CreateTask(oversized title) = %+v, want malformed", got)
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
		got, err := f.store.AckMessage(t.Context(), storevectors.AckMessageStaleIncarnation(f.spec.RunID, send.ID, f.ManagerID, identity.IncarnationID(uid(8012))))
		if err != nil {
			t.Fatalf("AckMessage() error = %v", err)
		}
		if got.Kind != app.AckRefused {
			t.Fatalf("AckMessage(stale incarnation) = %+v, want refused", got)
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
		if got.Kind != app.MessageMalformed {
			t.Fatalf("SendMessage(answer to unknown question) = %+v, want malformed", got)
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
		if got.Kind != app.MessageRefused {
			t.Fatalf("SendMessage(cross-run) = %+v, want refused", got)
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
		if got.Kind != app.AckRefused {
			t.Fatalf("AckMessage(cross-run) = %+v, want refused", got)
		}
	})
}
