package sqlite_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// messagingFixture is a feature fixture with one implementer bound to a
// second task — the worker whose task address the messaging suites
// exercise.
type messagingFixture struct {
	*featureFixture
	TaskB             identity.TaskID
	WorkerID          identity.SessionID
	WorkerIncarnation identity.IncarnationID
}

func newMessagingFixture(t *testing.T) *messagingFixture {
	t.Helper()
	f := newFeatureFixture(t)
	taskB := f.createFeatureTask(t, 7301, 2, run.TaskActive)
	workerID, workerIncarnation := f.createWorkerSession(t, taskB, run.RoleImplementer, 7302)
	return &messagingFixture{featureFixture: f, TaskB: taskB, WorkerID: workerID, WorkerIncarnation: workerIncarnation}
}

// workerSend builds a question/info send from the fixture worker to the
// manager.
func (f *messagingFixture) workerSend(n int, kind run.MessageKind, requestID string) app.MessageSend {
	return app.MessageSend{
		ID: identity.MessageID(uid(n)), RunID: f.spec.RunID,
		Sender: run.SessionPrincipal(f.WorkerID), SenderAddress: run.TaskAddress(f.TaskB),
		IncarnationID: f.WorkerIncarnation, Recipient: run.ManagerAddress(), Kind: kind,
		RequestID: requestID, BodyPath: "/state/bodies/" + uid(n) + ".md", BodyDigest: "digest-" + uid(n), BodyBytes: 8,
	}
}

// managerFetch builds the manager's fetch request.
func (f *messagingFixture) managerFetch() app.MessageFetch {
	return app.MessageFetch{RunID: f.spec.RunID, SessionID: f.ManagerID, IncarnationID: f.ManagerIncarnation, Address: run.ManagerAddress()}
}

// workerFetch builds the worker's fetch request at its task address.
func (f *messagingFixture) workerFetch() app.MessageFetch {
	return app.MessageFetch{RunID: f.spec.RunID, SessionID: f.WorkerID, IncarnationID: f.WorkerIncarnation, Address: run.TaskAddress(f.TaskB)}
}

// messageReceiptCount counts every message_receipts row in the store —
// receipts key by the CLAIMED run id, so an unknown-run refusal's evidence
// lands under the claimed id, not the fixture's.
func messageReceiptCount(t *testing.T, store *sqlite.Store, _ identity.RunID) int {
	t.Helper()
	return countRows(t, store, `SELECT COUNT(*) FROM message_receipts`)
}

// TestMessagingLifecycle drives send → fetch → ack end to end: the
// acceptance leaves an envelope and a receipt, the serve leaves a delivery
// row and NO receipt, the ack leaves the single ack row, and an empty
// fetch afterwards commits neither a delivery nor a receipt.
func TestMessagingLifecycle(t *testing.T) {
	f := newMessagingFixture(t)

	send := f.workerSend(7311, run.MessageQuestion, "")
	outcome, err := f.store.SendMessage(t.Context(), send)
	if err != nil || outcome.Kind != app.MessageAccepted || outcome.MessageID != send.ID {
		t.Fatalf("SendMessage() = %+v, %v; want accepted", outcome, err)
	}
	if n := messageReceiptCount(t, f.store, f.spec.RunID); n != 1 {
		t.Fatalf("receipts after send = %d, want the acceptance receipt", n)
	}

	delivery, served, err := f.store.FetchNextMessage(t.Context(), f.managerFetch())
	if err != nil || !served {
		t.Fatalf("FetchNextMessage() = served %t, %v; want the question served", served, err)
	}
	// The served envelope is returned as read at serve time (queued on a
	// first serve, delivered on a re-serve), exactly as internal/app's
	// fakeStore returns it; the delivery row is the durable serve fact.
	if delivery.Message.ID != send.ID || delivery.Origin != nil {
		t.Fatalf("delivery = %+v, want the question with no origin", delivery)
	}
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM message_deliveries WHERE message_id = ?`, send.ID.String()); n != 1 {
		t.Fatalf("delivery rows = %d, want 1", n)
	}
	if n := messageReceiptCount(t, f.store, f.spec.RunID); n != 1 {
		t.Fatalf("receipts after serve = %d; a successful serve's evidence is its delivery row, never a receipt", n)
	}

	ack := app.MessageAck{RunID: f.spec.RunID, MessageID: send.ID, SessionID: f.ManagerID, IncarnationID: f.ManagerIncarnation}
	ackOutcome, err := f.store.AckMessage(t.Context(), ack)
	if err != nil || ackOutcome.Kind != app.AckAccepted {
		t.Fatalf("AckMessage() = %+v, %v; want accepted", ackOutcome, err)
	}
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM message_acks WHERE message_id = ?`, send.ID.String()); n != 1 {
		t.Fatalf("ack rows = %d, want 1", n)
	}

	// Idempotent duplicate ack.
	dup, err := f.store.AckMessage(t.Context(), ack)
	if err != nil || dup.Kind != app.AckDuplicate {
		t.Fatalf("repeat AckMessage() = %+v, %v; want duplicate", dup, err)
	}

	// An empty fetch commits nothing: no delivery row, no receipt.
	before := messageReceiptCount(t, f.store, f.spec.RunID)
	deliveriesBefore := countRows(t, f.store, `SELECT COUNT(*) FROM message_deliveries`)
	if _, served, err := f.store.FetchNextMessage(t.Context(), f.managerFetch()); err != nil || served {
		t.Fatalf("empty FetchNextMessage() = served %t, %v; want nothing", served, err)
	}
	if after := messageReceiptCount(t, f.store, f.spec.RunID); after != before {
		t.Fatalf("empty fetch grew receipts %d -> %d; a poll loop must not grow the store", before, after)
	}
	if after := countRows(t, f.store, `SELECT COUNT(*) FROM message_deliveries`); after != deliveriesBefore {
		t.Fatalf("empty fetch grew deliveries %d -> %d", deliveriesBefore, after)
	}
}

// TestSendRefusalMatrix drives every send refusal path and asserts each
// leaves a receipt.
func TestSendRefusalMatrix(t *testing.T) {
	f := newMessagingFixture(t)
	cases := []struct {
		name string
		send func() app.MessageSend
		want app.MessageOutcomeKind
	}{
		{
			name: "unknown run is malformed",
			send: func() app.MessageSend {
				s := f.workerSend(7321, run.MessageQuestion, "")
				s.RunID = identity.RunID(uid(9990))
				return s
			},
			want: app.MessageMalformed,
		},
		{
			name: "stale incarnation is refused",
			send: func() app.MessageSend {
				s := f.workerSend(7322, run.MessageQuestion, "")
				s.IncarnationID = identity.IncarnationID(uid(9991))
				return s
			},
			want: app.MessageRefused,
		},
		{
			name: "worker to human violates addressing",
			send: func() app.MessageSend {
				s := f.workerSend(7323, run.MessageQuestion, "")
				s.Recipient = run.HumanAddress()
				return s
			},
			want: app.MessageRefused,
		},
		{
			name: "info to human is refused even from the manager",
			send: func() app.MessageSend {
				return app.MessageSend{
					ID: identity.MessageID(uid(7324)), RunID: f.spec.RunID,
					Sender: run.SessionPrincipal(f.ManagerID), SenderAddress: run.ManagerAddress(),
					IncarnationID: f.ManagerIncarnation, Recipient: run.HumanAddress(), Kind: run.MessageInfo,
					BodyPath: "/state/bodies/x.md", BodyDigest: "digest-x", BodyBytes: 1,
				}
			},
			want: app.MessageRefused,
		},
		{
			name: "unknown task recipient is malformed",
			send: func() app.MessageSend {
				return app.MessageSend{
					ID: identity.MessageID(uid(7325)), RunID: f.spec.RunID,
					Sender: run.SessionPrincipal(f.ManagerID), SenderAddress: run.ManagerAddress(),
					IncarnationID: f.ManagerIncarnation, Recipient: run.TaskAddress(identity.TaskID(uid(9992))), Kind: run.MessageInfo,
					BodyPath: "/state/bodies/y.md", BodyDigest: "digest-y", BodyBytes: 1,
				}
			},
			want: app.MessageMalformed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := messageReceiptCount(t, f.store, f.spec.RunID)
			outcome, err := f.store.SendMessage(t.Context(), tc.send())
			if err != nil {
				t.Fatalf("SendMessage() error = %v", err)
			}
			if outcome.Kind != tc.want {
				t.Fatalf("SendMessage() = %+v, want %s", outcome, tc.want)
			}
			if after := messageReceiptCount(t, f.store, f.spec.RunID); after != before+1 {
				t.Fatalf("refusal left %d receipts, want exactly one more than %d", after, before)
			}
		})
	}

	t.Run("run not running refuses an ordinary send", func(t *testing.T) {
		if err := f.store.RequestStop(t.Context(), f.spec.RunID); err != nil {
			t.Fatalf("RequestStop: %v", err)
		}
		outcome, err := f.store.SendMessage(t.Context(), f.workerSend(7326, run.MessageQuestion, ""))
		if err != nil || outcome.Kind != app.MessageRunNotAccept {
			t.Fatalf("SendMessage(stopping run) = %+v, %v; want refused-run-not-accepting", outcome, err)
		}
	})
}

// TestUnauthorizedFetchLeavesReceipt proves a refused fetch is the one
// fetch shape that leaves evidence.
func TestUnauthorizedFetchLeavesReceipt(t *testing.T) {
	f := newMessagingFixture(t)
	fetch := f.managerFetch()
	fetch.IncarnationID = identity.IncarnationID(uid(9993)) // stale

	_, served, err := f.store.FetchNextMessage(t.Context(), fetch)
	if served || !errors.Is(err, app.ErrMessagingUnauthorized) {
		t.Fatalf("FetchNextMessage(stale incarnation) = served %t, %v; want ErrMessagingUnauthorized", served, err)
	}
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM message_receipts WHERE run_id = ? AND op = 'msg-fetch' AND outcome = 'refused'`, f.spec.RunID.String()); n != 1 {
		t.Fatalf("refused fetch receipts = %d, want 1", n)
	}
}

// TestRelayedAnswerLineage drives the section 7 relay chain at store
// level: worker question → manager relay to the human (relayed-from set)
// → human answer (question acked in the same acceptance) → the manager's
// fetch of the answer carries the ORIGINAL question as origin → the
// manager forwards the answer to the worker → the worker's fetch carries
// its own question as origin — lineage recovered from CLI-returned data
// alone.
func TestRelayedAnswerLineage(t *testing.T) {
	f := newMessagingFixture(t)

	q1 := f.workerSend(7331, run.MessageQuestion, "")
	if outcome, err := f.store.SendMessage(t.Context(), q1); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("send q1 = %+v, %v", outcome, err)
	}
	if _, served, err := f.store.FetchNextMessage(t.Context(), f.managerFetch()); err != nil || !served {
		t.Fatalf("manager fetch q1 = %t, %v", served, err)
	}
	if outcome, err := f.store.AckMessage(t.Context(), app.MessageAck{RunID: f.spec.RunID, MessageID: q1.ID, SessionID: f.ManagerID, IncarnationID: f.ManagerIncarnation}); err != nil || outcome.Kind != app.AckAccepted {
		t.Fatalf("manager ack q1 = %+v, %v", outcome, err)
	}

	relayID := identity.MessageID(uid(7332))
	q1ID := q1.ID
	relay := app.MessageSend{
		ID: relayID, RunID: f.spec.RunID,
		Sender: run.SessionPrincipal(f.ManagerID), SenderAddress: run.ManagerAddress(),
		IncarnationID: f.ManagerIncarnation, Recipient: run.HumanAddress(), Kind: run.MessageQuestion,
		RelayedFrom: &q1ID, BodyPath: "/state/bodies/relay.md", BodyDigest: "digest-relay", BodyBytes: 8,
	}
	if outcome, err := f.store.SendMessage(t.Context(), relay); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("send relay = %+v, %v", outcome, err)
	}

	answer := app.HumanAnswer{
		ID: identity.MessageID(uid(7333)), RunID: f.spec.RunID, QuestionID: relayID,
		BodyPath: "/state/bodies/answer.md", BodyDigest: "digest-answer", BodyBytes: 8,
	}
	if outcome, err := f.store.AnswerQuestion(t.Context(), answer); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("AnswerQuestion() = %+v, %v; want accepted", outcome, err)
	}
	// The human question's ack is bundled into the answer's acceptance.
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM message_acks WHERE message_id = ? AND session_id IS NULL`, relayID.String()); n != 1 {
		t.Fatalf("bundled question acks = %d, want 1 session-less row", n)
	}

	// The manager fetches the answer: origin is the ORIGINAL worker
	// question, resolved server-side from the relay's provenance.
	delivery, served, err := f.store.FetchNextMessage(t.Context(), f.managerFetch())
	if err != nil || !served {
		t.Fatalf("manager fetch answer = %t, %v", served, err)
	}
	if delivery.Message.ID != answer.ID || delivery.Origin == nil || *delivery.Origin != q1.ID {
		t.Fatalf("answer delivery = %+v origin %v, want origin = the original worker question", delivery.Message, delivery.Origin)
	}
	if outcome, ackErr := f.store.AckMessage(t.Context(), app.MessageAck{RunID: f.spec.RunID, MessageID: answer.ID, SessionID: f.ManagerID, IncarnationID: f.ManagerIncarnation}); ackErr != nil || outcome.Kind != app.AckAccepted {
		t.Fatalf("manager ack answer = %+v, %v", outcome, ackErr)
	}

	// The manager forwards the answer to the worker using only the origin.
	forward := app.MessageSend{
		ID: identity.MessageID(uid(7334)), RunID: f.spec.RunID,
		Sender: run.SessionPrincipal(f.ManagerID), SenderAddress: run.ManagerAddress(),
		IncarnationID: f.ManagerIncarnation, Kind: run.MessageAnswer, ReplyTo: delivery.Origin,
		BodyPath: "/state/bodies/answer.md", BodyDigest: "digest-answer", BodyBytes: 8,
	}
	if outcome, sendErr := f.store.SendMessage(t.Context(), forward); sendErr != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("forward answer = %+v, %v; want accepted", outcome, sendErr)
	}

	// The worker fetches at its lineage address; the forwarded answer's
	// origin is its own question.
	workerDelivery, served, err := f.store.FetchNextMessage(t.Context(), f.workerFetch())
	if err != nil || !served {
		t.Fatalf("worker fetch answer = %t, %v", served, err)
	}
	if workerDelivery.Message.ID != forward.ID || workerDelivery.Origin == nil || *workerDelivery.Origin != q1.ID {
		t.Fatalf("worker delivery = %+v origin %v, want the forwarded answer with the original question as origin", workerDelivery.Message, workerDelivery.Origin)
	}
	if outcome, err := f.store.AckMessage(t.Context(), app.MessageAck{RunID: f.spec.RunID, MessageID: forward.ID, SessionID: f.WorkerID, IncarnationID: f.WorkerIncarnation}); err != nil || outcome.Kind != app.AckAccepted {
		t.Fatalf("worker ack answer = %+v, %v", outcome, err)
	}
}

// TestFetchFetchRaced races two handles fetching the manager's queue:
// per-recipient serialization keeps exactly ONE in-flight message — both
// serves return the SAME envelope (the second is the at-least-once
// re-serve) and each successful serve leaves its own delivery row.
func TestFetchFetchRaced(t *testing.T) {
	clock := newFakeClock()
	root := t.TempDir()
	storeA := openStoreAt(t, root, clock)
	storeB := openStoreAt(t, root, clock)
	f := buildMessagingFixtureAt(t, storeA, clock)
	second := f.workerSend(7341, run.MessageQuestion, "")
	if outcome, err := storeA.SendMessage(t.Context(), f.workerSend(7340, run.MessageQuestion, "")); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("seed first message: %+v, %v", outcome, err)
	}
	if outcome, err := storeA.SendMessage(t.Context(), second); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("seed second message: %+v, %v", outcome, err)
	}

	var (
		start      sync.WaitGroup
		done       sync.WaitGroup
		deliveries [2]app.MessageDelivery
		served     [2]bool
		errs       [2]error
	)
	start.Add(1)
	done.Add(2)
	go func() {
		defer done.Done()
		start.Wait()
		deliveries[0], served[0], errs[0] = storeA.FetchNextMessage(t.Context(), f.managerFetch())
	}()
	go func() {
		defer done.Done()
		start.Wait()
		deliveries[1], served[1], errs[1] = storeB.FetchNextMessage(t.Context(), f.managerFetch())
	}()
	start.Done()
	done.Wait()

	if errs[0] != nil || errs[1] != nil || !served[0] || !served[1] {
		t.Fatalf("raced fetches = %v %v (served %t %t), want both served", errs[0], errs[1], served[0], served[1])
	}
	if deliveries[0].Message.ID != deliveries[1].Message.ID {
		t.Fatalf("raced fetches served %s and %s; per-recipient serialization keeps exactly one in-flight message", deliveries[0].Message.ID, deliveries[1].Message.ID)
	}
	if n := countRows(t, storeA, `SELECT COUNT(*) FROM message_deliveries WHERE message_id = ?`, deliveries[0].Message.ID.String()); n != 2 {
		t.Fatalf("delivery rows for the raced serves = %d, want one per successful serve", n)
	}
}

// TestAckAckRaced races two handles acknowledging the same delivered
// message: one accepted, one idempotent duplicate, one ack row.
func TestAckAckRaced(t *testing.T) {
	clock := newFakeClock()
	root := t.TempDir()
	storeA := openStoreAt(t, root, clock)
	storeB := openStoreAt(t, root, clock)
	f := buildMessagingFixtureAt(t, storeA, clock)
	send := f.workerSend(7351, run.MessageQuestion, "")
	if outcome, err := storeA.SendMessage(t.Context(), send); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("seed message: %+v, %v", outcome, err)
	}
	if _, served, err := storeA.FetchNextMessage(t.Context(), f.managerFetch()); err != nil || !served {
		t.Fatalf("serve message: %t, %v", served, err)
	}
	ack := app.MessageAck{RunID: f.spec.RunID, MessageID: send.ID, SessionID: f.ManagerID, IncarnationID: f.ManagerIncarnation}

	var (
		start    sync.WaitGroup
		done     sync.WaitGroup
		outcomes [2]app.MessageAckOutcome
		errs     [2]error
	)
	start.Add(1)
	done.Add(2)
	go func() { defer done.Done(); start.Wait(); outcomes[0], errs[0] = storeA.AckMessage(t.Context(), ack) }()
	go func() { defer done.Done(); start.Wait(); outcomes[1], errs[1] = storeB.AckMessage(t.Context(), ack) }()
	start.Done()
	done.Wait()

	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("raced acks errored: %v %v", errs[0], errs[1])
	}
	accepted, duplicate := 0, 0
	for _, o := range outcomes {
		switch o.Kind {
		case app.AckAccepted:
			accepted++
		case app.AckDuplicate:
			duplicate++
		default:
			t.Fatalf("raced ack outcome = %+v", o)
		}
	}
	if accepted != 1 || duplicate != 1 {
		t.Fatalf("raced acks = %d accepted, %d duplicate; want exactly one of each", accepted, duplicate)
	}
	if n := countRows(t, storeA, `SELECT COUNT(*) FROM message_acks WHERE message_id = ?`, send.ID.String()); n != 1 {
		t.Fatalf("ack rows after the race = %d, want 1", n)
	}
}

// TestAnswerAnswerRaced races two handles answering the same human
// question with the same body: one accepted, the loser reads the
// idempotent duplicate; one answer envelope exists.
func TestAnswerAnswerRaced(t *testing.T) {
	clock := newFakeClock()
	root := t.TempDir()
	storeA := openStoreAt(t, root, clock)
	storeB := openStoreAt(t, root, clock)
	f := buildMessagingFixtureAt(t, storeA, clock)
	relayID := seedHumanQuestion(t, storeA, f, 7361)

	answer := func(n int) app.HumanAnswer {
		return app.HumanAnswer{
			ID: identity.MessageID(uid(n)), RunID: f.spec.RunID, QuestionID: relayID,
			BodyPath: "/state/bodies/answer.md", BodyDigest: "digest-same", BodyBytes: 8,
		}
	}
	var (
		start    sync.WaitGroup
		done     sync.WaitGroup
		outcomes [2]app.MessageOutcome
		errs     [2]error
	)
	start.Add(1)
	done.Add(2)
	go func() {
		defer done.Done()
		start.Wait()
		outcomes[0], errs[0] = storeA.AnswerQuestion(t.Context(), answer(7365))
	}()
	go func() {
		defer done.Done()
		start.Wait()
		outcomes[1], errs[1] = storeB.AnswerQuestion(t.Context(), answer(7366))
	}()
	start.Done()
	done.Wait()

	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("raced answers errored: %v %v", errs[0], errs[1])
	}
	accepted, duplicate := 0, 0
	var acceptedID identity.MessageID
	for _, o := range outcomes {
		switch o.Kind {
		case app.MessageAccepted:
			accepted++
			acceptedID = o.MessageID
		case app.MessageDuplicate:
			duplicate++
		default:
			t.Fatalf("raced answer outcome = %+v", o)
		}
	}
	if accepted != 1 || duplicate != 1 {
		t.Fatalf("raced answers = %d accepted, %d duplicate; want exactly one accepted", accepted, duplicate)
	}
	for _, o := range outcomes {
		if o.Kind == app.MessageDuplicate && o.MessageID != acceptedID {
			t.Fatalf("duplicate answer returned %s, want the winner's answer %s", o.MessageID, acceptedID)
		}
	}
	if n := countRows(t, storeA, `SELECT COUNT(*) FROM messages WHERE kind = 'answer' AND reply_to = ?`, relayID.String()); n != 1 {
		t.Fatalf("answer envelopes after the race = %d, want 1", n)
	}
}

// TestRequestIDReuseRaced races two handles sending with the SAME request
// ID and identical content but distinct message ids: one entity is
// created, and the loser reads the winner's receipt back as a duplicate
// naming the winner's message.
func TestRequestIDReuseRaced(t *testing.T) {
	clock := newFakeClock()
	root := t.TempDir()
	storeA := openStoreAt(t, root, clock)
	storeB := openStoreAt(t, root, clock)
	f := buildMessagingFixtureAt(t, storeA, clock)
	const requestID = "raced-request"

	send := func(n int) app.MessageSend {
		s := f.workerSend(n, run.MessageQuestion, requestID)
		// Identical content: the digest covers the body digest and
		// addressing, never the fresh envelope id.
		s.BodyPath, s.BodyDigest, s.BodyBytes = "/state/bodies/shared.md", "digest-shared", 8
		return s
	}
	var (
		start    sync.WaitGroup
		done     sync.WaitGroup
		outcomes [2]app.MessageOutcome
		errs     [2]error
	)
	start.Add(1)
	done.Add(2)
	go func() {
		defer done.Done()
		start.Wait()
		outcomes[0], errs[0] = storeA.SendMessage(t.Context(), send(7371))
	}()
	go func() {
		defer done.Done()
		start.Wait()
		outcomes[1], errs[1] = storeB.SendMessage(t.Context(), send(7372))
	}()
	start.Done()
	done.Wait()

	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("raced sends errored: %v %v", errs[0], errs[1])
	}
	accepted, duplicate := -1, -1
	for i, o := range outcomes {
		switch o.Kind {
		case app.MessageAccepted:
			accepted = i
		case app.MessageDuplicate:
			duplicate = i
		default:
			t.Fatalf("raced send outcome = %+v", o)
		}
	}
	if accepted < 0 || duplicate < 0 {
		t.Fatalf("raced sends = %+v, want one accepted and one duplicate", outcomes)
	}
	winner, loser := outcomes[accepted], outcomes[duplicate] //nolint:gosec // G602: both indexes are proven in [0,1] by the check above.
	if loser.MessageID != winner.MessageID {
		t.Fatalf("loser read %s, want the winner's receipt entity %s", loser.MessageID, winner.MessageID)
	}
	if n := countRows(t, storeA, `SELECT COUNT(*) FROM messages WHERE run_id = ? AND kind = 'question'`, f.spec.RunID.String()); n != 1 {
		t.Fatalf("question envelopes after the race = %d, want the one created entity", n)
	}
}

// TestEnqueueSequenceFIFO proves the durable per-address sequence is
// commit order, never caller clocks: a later writer whose wall clock runs
// EARLIER cannot jump the queue.
func TestEnqueueSequenceFIFO(t *testing.T) {
	clock := newFakeClock()
	root := t.TempDir()
	storeA := openStoreAt(t, root, clock)
	storeB := openStoreAt(t, root, clock)
	f := buildMessagingFixtureAt(t, storeA, clock)

	first := f.workerSend(7381, run.MessageQuestion, "")
	if outcome, err := storeA.SendMessage(t.Context(), first); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("send first: %+v, %v", outcome, err)
	}
	// The second writer's clock is turned BACK an hour: its envelope
	// timestamp precedes the first message's, its enqueue sequence must
	// not.
	clock.Advance(-time.Hour)
	second := f.workerSend(7382, run.MessageInfo, "")
	if outcome, err := storeB.SendMessage(t.Context(), second); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("send second: %+v, %v", outcome, err)
	}

	var firstSeq, secondSeq int
	if err := sqlite.WriteDB(storeA).QueryRowContext(t.Context(), `SELECT enqueue_seq FROM messages WHERE id = ?`, first.ID.String()).Scan(&firstSeq); err != nil {
		t.Fatal(err)
	}
	if err := sqlite.WriteDB(storeA).QueryRowContext(t.Context(), `SELECT enqueue_seq FROM messages WHERE id = ?`, second.ID.String()).Scan(&secondSeq); err != nil {
		t.Fatal(err)
	}
	if firstSeq != 1 || secondSeq != 2 {
		t.Fatalf("enqueue sequences = %d, %d; want commit order 1, 2 despite the inverted timestamp", firstSeq, secondSeq)
	}
	delivery, served, err := storeA.FetchNextMessage(t.Context(), f.managerFetch())
	if err != nil || !served || delivery.Message.ID != first.ID {
		t.Fatalf("first fetch served %v (%t, %v), want the first-committed message", delivery.Message.ID, served, err)
	}
}

// TestReceiptAcceptanceKey proves the (run, verb, request ID) key's scope:
// the same request ID is accepted independently across two RUNS and
// across two VERBS, an identical retry is a duplicate, and a reused ID
// with a different body or question is refused.
func TestReceiptAcceptanceKey(t *testing.T) {
	clock := newFakeClock()
	root := t.TempDir()
	store := openStoreAt(t, root, clock)
	f := buildMessagingFixtureAt(t, store, clock)
	const requestID = "shared-id"

	// Accepted under the send verb.
	send := f.workerSend(7391, run.MessageQuestion, requestID)
	if outcome, err := store.SendMessage(t.Context(), send); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("send = %+v, %v", outcome, err)
	}
	// An identical retry (fresh envelope id, same content) is a duplicate
	// returning the original entity.
	retry := f.workerSend(7392, run.MessageQuestion, requestID)
	retry.BodyPath, retry.BodyDigest, retry.BodyBytes = send.BodyPath, send.BodyDigest, send.BodyBytes
	if outcome, err := store.SendMessage(t.Context(), retry); err != nil || outcome.Kind != app.MessageDuplicate || outcome.MessageID != send.ID {
		t.Fatalf("identical retry = %+v, %v; want duplicate of %s", outcome, err, send.ID)
	}
	// A reused ID with a different body is refused.
	conflicting := f.workerSend(7393, run.MessageQuestion, requestID)
	if outcome, err := store.SendMessage(t.Context(), conflicting); err != nil || outcome.Kind != app.MessageRefused {
		t.Fatalf("conflicting reuse = %+v, %v; want refused", outcome, err)
	}

	// The SAME ID under the answer verb is an independent acceptance.
	questionID := seedHumanQuestion(t, store, f, 7394)
	answer := app.HumanAnswer{
		ID: identity.MessageID(uid(7397)), RunID: f.spec.RunID, QuestionID: questionID,
		RequestID: requestID, BodyPath: "/state/bodies/a.md", BodyDigest: "digest-a", BodyBytes: 4,
	}
	if outcome, err := store.AnswerQuestion(t.Context(), answer); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("answer under the shared id = %+v, %v; want an independent acceptance", outcome, err)
	}

	// The SAME ID in another RUN is an independent acceptance.
	other := buildSecondMessagingFixtureAt(t, store, clock)
	otherSend := other.workerSend(7396, run.MessageQuestion, requestID)
	if outcome, err := store.SendMessage(t.Context(), otherSend); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("send in the second run = %+v, %v; want an independent acceptance", outcome, err)
	}
}

// TestHumanAnswerDigestVectors proves the question UUID is load-bearing in
// the answer digest: identical bodies to two questions are two requests —
// the same request ID across them is a conflicting reuse, and a fresh ID
// accepts.
func TestHumanAnswerDigestVectors(t *testing.T) {
	f := newMessagingFixture(t)
	question1 := seedHumanQuestion(t, f.store, f, 7401)
	question2 := seedHumanQuestion(t, f.store, f, 7403)
	const requestID = "answer-request"

	first := app.HumanAnswer{
		ID: identity.MessageID(uid(7405)), RunID: f.spec.RunID, QuestionID: question1,
		RequestID: requestID, BodyPath: "/state/bodies/same.md", BodyDigest: "digest-same", BodyBytes: 4,
	}
	if outcome, err := f.store.AnswerQuestion(t.Context(), first); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("first answer = %+v, %v", outcome, err)
	}
	// The identical body addressed to a DIFFERENT question under the same
	// request ID is a different request: refused as a conflicting reuse,
	// never a duplicate.
	crossQuestion := app.HumanAnswer{
		ID: identity.MessageID(uid(7406)), RunID: f.spec.RunID, QuestionID: question2,
		RequestID: requestID, BodyPath: "/state/bodies/same.md", BodyDigest: "digest-same", BodyBytes: 4,
	}
	if outcome, err := f.store.AnswerQuestion(t.Context(), crossQuestion); err != nil || outcome.Kind != app.MessageRefused {
		t.Fatalf("cross-question reuse = %+v, %v; want refused", outcome, err)
	}
	// An identical retry against the SAME question is the idempotent
	// duplicate.
	retry := first
	retry.ID = identity.MessageID(uid(7407))
	if outcome, err := f.store.AnswerQuestion(t.Context(), retry); err != nil || outcome.Kind != app.MessageDuplicate || outcome.MessageID != first.ID {
		t.Fatalf("identical answer retry = %+v, %v; want duplicate of %s", outcome, err, first.ID)
	}
	// A fresh request ID answers the second question independently.
	second := app.HumanAnswer{
		ID: identity.MessageID(uid(7408)), RunID: f.spec.RunID, QuestionID: question2,
		RequestID: "another-request", BodyPath: "/state/bodies/same.md", BodyDigest: "digest-same", BodyBytes: 4,
	}
	if outcome, err := f.store.AnswerQuestion(t.Context(), second); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("second question's answer = %+v, %v; want accepted", outcome, err)
	}
}

// buildMessagingFixtureAt builds the standard messaging fixture against an
// already-open store handle (for raced suites sharing a root).
func buildMessagingFixtureAt(t *testing.T, store *sqlite.Store, clock *fakeClock) *messagingFixture {
	t.Helper()
	spec := newSpec("/repos/feature", specStride, clock.Now())
	spec.Snapshot.Workflow = featureWorkflow()
	_, lease, err := store.InitializeRun(t.Context(), spec)
	if err != nil {
		t.Fatalf("InitializeRun: %v", err)
	}
	f := &featureFixture{
		fixture:            &fixture{store: store, clock: clock, spec: spec, lease: lease},
		ManagerID:          identity.SessionID(uid(offManager)),
		ManagerIncarnation: identity.IncarnationID(uid(offManagerInc)),
	}
	seedFeatureRunState(t, f)
	taskB := f.createFeatureTask(t, 7301, 2, run.TaskActive)
	workerID, workerIncarnation := f.createWorkerSession(t, taskB, run.RoleImplementer, 7302)
	return &messagingFixture{featureFixture: f, TaskB: taskB, WorkerID: workerID, WorkerIncarnation: workerIncarnation}
}

// buildSecondMessagingFixtureAt initializes an INDEPENDENT second run on
// the same store, with its own manager and worker, for cross-run vectors.
func buildSecondMessagingFixtureAt(t *testing.T, store *sqlite.Store, clock *fakeClock) *messagingFixture {
	t.Helper()
	spec := newSpec("/repos/feature-second", 3*specStride, clock.Now())
	spec.Snapshot.Workflow = featureWorkflow()
	_, lease, err := store.InitializeRun(t.Context(), spec)
	if err != nil {
		t.Fatalf("InitializeRun second: %v", err)
	}
	f := &featureFixture{
		fixture:            &fixture{store: store, clock: clock, spec: spec, lease: lease},
		ManagerID:          identity.SessionID(uid(7451)),
		ManagerIncarnation: identity.IncarnationID(uid(7452)),
	}
	seedFeatureRunState(t, f)
	taskB := f.createFeatureTask(t, 7453, 2, run.TaskActive)
	workerID, workerIncarnation := f.createWorkerSession(t, taskB, run.RoleImplementer, 7454)
	return &messagingFixture{featureFixture: f, TaskB: taskB, WorkerID: workerID, WorkerIncarnation: workerIncarnation}
}

// seedFeatureRunState drives a freshly initialized feature run to running
// with a bound manager, exactly as newFeatureFixture does.
func seedFeatureRunState(t *testing.T, f *featureFixture) {
	t.Helper()
	now := f.clock.Now()
	f.inUOW(t, func(uow app.UnitOfWork) {
		saveRun(t, uow, f.spec.RunID, func(v run.Run) (run.Run, error) { return v.Launch(now) })
		saveRun(t, uow, f.spec.RunID, func(v run.Run) (run.Run, error) { return v.MarkRunning(now) })
		manager := run.NewManagerSession(f.ManagerID, f.spec.RunID, run.HarnessClaude, now)
		manager, launchErr := manager.Launch(now)
		if launchErr != nil {
			t.Fatalf("launch manager session: %v", launchErr)
		}
		if manager, launchErr = manager.ConfirmActive(now); launchErr != nil {
			t.Fatalf("activate manager session: %v", launchErr)
		}
		if _, err := uow.Sessions().Create(t.Context(), manager); err != nil {
			t.Fatalf("create manager session: %v", err)
		}
		binding := run.NewRuntimeBinding(
			f.ManagerID, f.ManagerIncarnation,
			"/tmp/herdr.sock", "server-instance-1", "workspace-m", "tab-m", "pane-m",
			"label-"+f.ManagerID.String(), run.LaunchInitial, now,
		)
		if err := uow.Bindings().Create(t.Context(), binding); err != nil {
			t.Fatalf("create manager binding: %v", err)
		}
	})
}

// seedHumanQuestion relays a fresh worker question to the human and
// returns the human-addressed relay's id, ready for hop answer.
func seedHumanQuestion(t *testing.T, store *sqlite.Store, f *messagingFixture, n int) identity.MessageID {
	t.Helper()
	q := f.workerSend(n, run.MessageQuestion, "")
	if outcome, err := store.SendMessage(t.Context(), q); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("seed worker question: %+v, %v", outcome, err)
	}
	qID := q.ID
	relay := app.MessageSend{
		ID: identity.MessageID(uid(n + 1)), RunID: f.spec.RunID,
		Sender: run.SessionPrincipal(f.ManagerID), SenderAddress: run.ManagerAddress(),
		IncarnationID: f.ManagerIncarnation, Recipient: run.HumanAddress(), Kind: run.MessageQuestion,
		RelayedFrom: &qID, BodyPath: "/state/bodies/" + uid(n+1) + ".md", BodyDigest: "digest-" + uid(n+1), BodyBytes: 8,
	}
	if outcome, err := store.SendMessage(t.Context(), relay); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("seed relay question: %+v, %v", outcome, err)
	}
	return relay.ID
}
