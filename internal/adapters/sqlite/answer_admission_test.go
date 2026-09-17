package sqlite_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// sessionAnswer builds a session's answer to question, sent by session at
// its true logical address.
func sessionAnswer(runID identity.RunID, n int, session identity.SessionID, address run.Address, incarnation identity.IncarnationID, question identity.MessageID, requestID, bodyDigest string) app.MessageSend {
	replyTo := question
	return app.MessageSend{
		ID: identity.MessageID(uid(n)), RunID: runID,
		Sender: run.SessionPrincipal(session), SenderAddress: address, IncarnationID: incarnation,
		Kind: run.MessageAnswer, ReplyTo: &replyTo, RequestID: requestID,
		BodyPath: "/state/bodies/" + uid(n) + ".md", BodyDigest: bodyDigest, BodyBytes: 6,
	}
}

// managerAnswer is the fixture manager's answer to question.
func (f *featureFixture) managerAnswer(n int, question identity.MessageID, requestID, bodyDigest string) app.MessageSend {
	return sessionAnswer(f.spec.RunID, n, f.ManagerID, run.ManagerAddress(), f.ManagerIncarnation, question, requestID, bodyDigest)
}

// requireSendOutcome sends one request through store and fails unless it
// decides kind with reason; it returns the outcome.
func requireSendOutcome(t *testing.T, store *sqlite.Store, label string, send app.MessageSend, kind app.MessageOutcomeKind, reason string) app.MessageOutcome { //nolint:gocritic // hugeParam: the port passes the send value; the helper mirrors it.
	t.Helper()
	outcome, err := store.SendMessage(t.Context(), send)
	if err != nil {
		t.Fatalf("%s: SendMessage() error = %v", label, err)
	}
	if outcome.Kind != kind || outcome.Reason != reason {
		t.Fatalf("%s: SendMessage() = %+v, want %s/%q", label, outcome, kind, reason)
	}
	return outcome
}

// requireNoAnswer fails when any answer envelope replies to question.
func requireNoAnswer(t *testing.T, store *sqlite.Store, label string, question identity.MessageID) {
	t.Helper()
	if n := countRows(t, store, `SELECT COUNT(*) FROM messages WHERE kind = 'answer' AND reply_to = ?`, question.String()); n != 0 {
		t.Fatalf("%s: %d answer envelope(s) reply to %s; want none", label, n, question)
	}
}

// TestAnswerFromNonRecipientRefused proves section 7's answer authority at
// the real store for human-addressed questions: a current worker session
// in the run cannot answer a question addressed to the human — refused
// unauthorized with a receipt, no envelope and the question left
// unacknowledged, a same-request-id retry decided afresh — so the human's
// own hop answer is then accepted (bundling the question's ack), and on a
// relay the manager still forwards it to the worker.
func TestAnswerFromNonRecipientRefused(t *testing.T) {
	t.Run("the manager's own human question", func(t *testing.T) {
		f := newMessagingFixture(t)
		question := app.MessageSend{
			ID: identity.MessageID(uid(9711)), RunID: f.spec.RunID,
			Sender: run.SessionPrincipal(f.ManagerID), SenderAddress: run.ManagerAddress(),
			IncarnationID: f.ManagerIncarnation, Recipient: run.HumanAddress(), Kind: run.MessageQuestion,
			BodyPath: "/state/q", BodyDigest: "question", BodyBytes: 8,
		}
		requireSendOutcome(t, f.store, "manager question to human", question, app.MessageAccepted, "")

		stolen := sessionAnswer(f.spec.RunID, 9712, f.WorkerID, run.TaskAddress(f.TaskB), f.WorkerIncarnation, question.ID, "worker-answer", "wrong-answer")
		for _, attempt := range []string{"first", "same request id retried"} {
			before := messageReceiptCount(t, f.store, f.spec.RunID)
			requireSendOutcome(t, f.store, "worker answer ("+attempt+")", stolen, app.MessageRefused, app.GrammarReasonUnauthorized)
			if after := messageReceiptCount(t, f.store, f.spec.RunID); after != before+1 {
				t.Fatalf("worker answer (%s) left %d receipts, want exactly one more than %d", attempt, after, before)
			}
		}
		requireNoAnswer(t, f.store, "after the refused worker answer", question.ID)
		if n := countRows(t, f.store, `SELECT COUNT(*) FROM message_acks WHERE message_id = ?`, question.ID.String()); n != 0 {
			t.Fatalf("human question ack rows = %d after the refused worker answer, want 0", n)
		}
		if n := countRows(t, f.store, `SELECT COUNT(*) FROM message_receipts WHERE op = 'msg-send' AND outcome = 'refused' AND claimed_message_id = ? AND request_id = 'worker-answer'`, question.ID.String()); n != 2 {
			t.Fatalf("refusal receipts naming the question = %d, want 2", n)
		}

		human := app.HumanAnswer{
			ID: identity.MessageID(uid(9713)), RunID: f.spec.RunID, QuestionID: question.ID,
			BodyPath: "/state/a", BodyDigest: "right-answer", BodyBytes: 12,
		}
		if outcome, err := f.store.AnswerQuestion(t.Context(), human); err != nil || outcome.Kind != app.MessageAccepted || outcome.MessageID != human.ID {
			t.Fatalf("AnswerQuestion() = %+v, %v; want the human's answer accepted", outcome, err)
		}
		if n := countRows(t, f.store, `SELECT COUNT(*) FROM message_acks WHERE message_id = ? AND session_id IS NULL`, question.ID.String()); n != 1 {
			t.Fatalf("bundled human-question acks = %d, want 1", n)
		}
		delivery, served, err := f.store.FetchNextMessage(t.Context(), f.managerFetch())
		if err != nil || !served || delivery.Message.ID != human.ID || delivery.Origin == nil || *delivery.Origin != question.ID {
			t.Fatalf("manager fetch = %+v served=%t err=%v; want the human's answer with origin = its own question", delivery, served, err)
		}
	})

	t.Run("a manager relay of the worker's own question", func(t *testing.T) {
		f := newMessagingFixture(t)
		relayID := seedHumanQuestion(t, f.store, f, 9714)
		original := identity.MessageID(uid(9714))

		requireSendOutcome(t, f.store, "worker answer to the relay",
			sessionAnswer(f.spec.RunID, 9716, f.WorkerID, run.TaskAddress(f.TaskB), f.WorkerIncarnation, relayID, "", "self-answer"),
			app.MessageRefused, app.GrammarReasonUnauthorized)
		requireSendOutcome(t, f.store, "manager answer to its own relay",
			f.managerAnswer(9717, relayID, "", "self-answer"),
			app.MessageRefused, app.GrammarReasonUnauthorized)
		requireNoAnswer(t, f.store, "after the refused session answers", relayID)

		human := app.HumanAnswer{
			ID: identity.MessageID(uid(9718)), RunID: f.spec.RunID, QuestionID: relayID,
			BodyPath: "/state/a", BodyDigest: "human-answer", BodyBytes: 12,
		}
		if outcome, err := f.store.AnswerQuestion(t.Context(), human); err != nil || outcome.Kind != app.MessageAccepted {
			t.Fatalf("AnswerQuestion(relay) = %+v, %v; want accepted", outcome, err)
		}
		var recipient string
		if err := sqlite.WriteDB(f.store).QueryRowContext(t.Context(), `SELECT recipient_address FROM messages WHERE id = ?`, human.ID.String()).Scan(&recipient); err != nil || recipient != "manager" {
			t.Fatalf("human answer recipient = %q, %v; want manager", recipient, err)
		}
		// The manager serves the worker question first (FIFO), then the
		// human's answer carrying the original question as origin.
		for _, want := range []identity.MessageID{original, human.ID} {
			delivery, served, err := f.store.FetchNextMessage(t.Context(), f.managerFetch())
			if err != nil || !served || delivery.Message.ID != want {
				t.Fatalf("manager fetch = %s served=%t err=%v; want %s", delivery.Message.ID, served, err, want)
			}
			if outcome, ackErr := f.store.AckMessage(t.Context(), app.MessageAck{RunID: f.spec.RunID, MessageID: delivery.Message.ID, SessionID: f.ManagerID, IncarnationID: f.ManagerIncarnation}); ackErr != nil || outcome.Kind != app.AckAccepted {
				t.Fatalf("manager ack = %+v, %v", outcome, ackErr)
			}
			if want == human.ID && (delivery.Origin == nil || *delivery.Origin != original) {
				t.Fatalf("human answer origin = %v, want the original worker question %s", delivery.Origin, original)
			}
		}
		requireSendOutcome(t, f.store, "manager forward to the worker", f.managerAnswer(9719, original, "forward", "human-answer"), app.MessageAccepted, "")
	})
}

// TestAnswerToAnotherAddressRefused proves no session answers a question
// addressed to another logical address: another task's worker, the
// manager answering a task-addressed question, and a worker answering its
// own manager-addressed question are each refused unauthorized with no
// envelope, while the addressed recipient's answer is accepted with its
// destination derived from the originator.
func TestAnswerToAnotherAddressRefused(t *testing.T) {
	f := newMessagingFixture(t)
	taskC := f.createFeatureTask(t, 9721, 3, run.TaskActive)
	workerC, workerCIncarnation := f.createWorkerSession(t, taskC, run.RoleImplementer, 9722)
	answerByB := func(n int, question identity.MessageID) app.MessageSend {
		return sessionAnswer(f.spec.RunID, n, f.WorkerID, run.TaskAddress(f.TaskB), f.WorkerIncarnation, question, "", "digest-"+uid(n))
	}
	answerByC := func(n int, question identity.MessageID) app.MessageSend {
		return sessionAnswer(f.spec.RunID, n, workerC, run.TaskAddress(taskC), workerCIncarnation, question, "", "digest-"+uid(n))
	}

	t.Run("a manager question to task B", func(t *testing.T) {
		toB := app.MessageSend{
			ID: identity.MessageID(uid(9730)), RunID: f.spec.RunID,
			Sender: run.SessionPrincipal(f.ManagerID), SenderAddress: run.ManagerAddress(),
			IncarnationID: f.ManagerIncarnation, Recipient: run.TaskAddress(f.TaskB), Kind: run.MessageQuestion,
			BodyPath: "/state/q-b", BodyDigest: "q-b", BodyBytes: 3,
		}
		requireSendOutcome(t, f.store, "manager question to task B", toB, app.MessageAccepted, "")
		requireSendOutcome(t, f.store, "task C answering task B's question", answerByC(9731, toB.ID), app.MessageRefused, app.GrammarReasonUnauthorized)
		requireSendOutcome(t, f.store, "the manager answering its own question to task B", f.managerAnswer(9732, toB.ID, "", "digest-m"), app.MessageRefused, app.GrammarReasonUnauthorized)
		requireNoAnswer(t, f.store, "after the refused answers", toB.ID)

		accepted := requireSendOutcome(t, f.store, "task B answering", answerByB(9733, toB.ID), app.MessageAccepted, "")
		var recipient string
		if err := sqlite.WriteDB(f.store).QueryRowContext(t.Context(), `SELECT recipient_address FROM messages WHERE id = ?`, accepted.MessageID.String()).Scan(&recipient); err != nil || recipient != "manager" {
			t.Fatalf("task B's answer recipient = %q, %v; want manager", recipient, err)
		}
	})

	t.Run("task B's question to the manager", func(t *testing.T) {
		fromB := f.workerSend(9740, run.MessageQuestion, "")
		requireSendOutcome(t, f.store, "task B question to the manager", fromB, app.MessageAccepted, "")
		requireSendOutcome(t, f.store, "task C answering task B's question", answerByC(9741, fromB.ID), app.MessageRefused, app.GrammarReasonUnauthorized)
		requireSendOutcome(t, f.store, "task B answering its own question", answerByB(9742, fromB.ID), app.MessageRefused, app.GrammarReasonUnauthorized)
		requireNoAnswer(t, f.store, "after the refused answers", fromB.ID)

		accepted := requireSendOutcome(t, f.store, "the manager answering", f.managerAnswer(9743, fromB.ID, "", "digest-m"), app.MessageAccepted, "")
		var (
			recipient string
			seq       int
		)
		if err := sqlite.WriteDB(f.store).QueryRowContext(t.Context(), `SELECT recipient_address, enqueue_seq FROM messages WHERE id = ?`, accepted.MessageID.String()).Scan(&recipient, &seq); err != nil ||
			recipient != app.AddressString(run.TaskAddress(f.TaskB)) || seq != 2 {
			t.Fatalf("the manager's answer = recipient %q seq %d, %v; want task B at seq 2", recipient, seq, err)
		}
	})
}

// TestAnswerRetryPrecedence pins the order a retried or competing answer is
// decided in: the (run, verb, request ID) receipt first — so the
// recipient's identical retry is duplicate and a reused id with other
// content (including a non-recipient's, whose digest names a different
// sender address) is refused conflicting — then recipient authority, so a
// non-recipient never reads a duplicate or conflicting verdict about the
// accepted answer, then the prior answer's content. Every decision leaves
// one receipt, and one answer envelope exists throughout.
func TestAnswerRetryPrecedence(t *testing.T) {
	f := newMessagingFixture(t)
	taskC := f.createFeatureTask(t, 9745, 3, run.TaskActive)
	workerC, workerCIncarnation := f.createWorkerSession(t, taskC, run.RoleImplementer, 9746)
	byC := func(n int, question identity.MessageID, requestID, digest string) app.MessageSend {
		return sessionAnswer(f.spec.RunID, n, workerC, run.TaskAddress(taskC), workerCIncarnation, question, requestID, digest)
	}
	question := f.workerSend(9750, run.MessageQuestion, "")
	requireSendOutcome(t, f.store, "task B question", question, app.MessageAccepted, "")
	accepted := requireSendOutcome(t, f.store, "manager answer", f.managerAnswer(9751, question.ID, "fwd-1", "digest-fwd"), app.MessageAccepted, "")

	steps := []struct {
		name   string
		send   app.MessageSend
		kind   app.MessageOutcomeKind
		reason string
	}{
		{"the same request id and body", f.managerAnswer(9752, question.ID, "fwd-1", "digest-fwd"), app.MessageDuplicate, ""},
		{"the same request id with another body", f.managerAnswer(9753, question.ID, "fwd-1", "digest-other"), app.MessageRefused, app.GrammarReasonConflicting},
		{"a fresh request id with the same body", f.managerAnswer(9754, question.ID, "fwd-2", "digest-fwd"), app.MessageDuplicate, ""},
		{"no request id with the same body", f.managerAnswer(9755, question.ID, "", "digest-fwd"), app.MessageDuplicate, ""},
		{"a fresh request id with another body", f.managerAnswer(9756, question.ID, "fwd-3", "digest-other"), app.MessageConflicting, app.GrammarReasonConflicting},
		{"a non-recipient reusing the recipient's request id", byC(9757, question.ID, "fwd-1", "digest-fwd"), app.MessageRefused, app.GrammarReasonConflicting},
		{"a non-recipient with a fresh request id and the accepted body", byC(9758, question.ID, "c-1", "digest-fwd"), app.MessageRefused, app.GrammarReasonUnauthorized},
		{"a non-recipient with no request id and the accepted body", byC(9759, question.ID, "", "digest-fwd"), app.MessageRefused, app.GrammarReasonUnauthorized},
		{"a non-recipient with another body", byC(9760, question.ID, "c-2", "digest-other"), app.MessageRefused, app.GrammarReasonUnauthorized},
		{"the question's own sender", sessionAnswer(f.spec.RunID, 9761, f.WorkerID, run.TaskAddress(f.TaskB), f.WorkerIncarnation, question.ID, "b-1", "digest-fwd"), app.MessageRefused, app.GrammarReasonUnauthorized},
	}
	for _, step := range steps {
		before := messageReceiptCount(t, f.store, f.spec.RunID)
		got := requireSendOutcome(t, f.store, step.name, step.send, step.kind, step.reason)
		if step.kind == app.MessageDuplicate && got.MessageID != accepted.MessageID {
			t.Fatalf("%s: duplicate names %s, want the accepted answer %s", step.name, got.MessageID, accepted.MessageID)
		}
		if step.kind != app.MessageDuplicate && got.MessageID != "" {
			t.Fatalf("%s: refusal names message %s, want none", step.name, got.MessageID)
		}
		if after := messageReceiptCount(t, f.store, f.spec.RunID); after != before+1 {
			t.Fatalf("%s: %d receipts, want exactly one more than %d", step.name, after, before)
		}
	}
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM messages WHERE kind = 'answer' AND reply_to = ?`, question.ID.String()); n != 1 {
		t.Fatalf("answer envelopes = %d, want the one accepted answer", n)
	}
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM message_receipts WHERE op = 'msg-send' AND outcome = 'accepted' AND claimed_message_id = ?`, question.ID.String()); n != 1 {
		t.Fatalf("accepted answer receipts = %d, want 1", n)
	}
}

// mailboxWorkerQuestion builds a question from the mailbox fixture's worker
// to the manager.
func (f *mailboxFixture) mailboxWorkerQuestion(n int) app.MessageSend {
	return app.MessageSend{
		ID: identity.MessageID(uid(n)), RunID: f.spec.RunID,
		Sender: run.SessionPrincipal(f.WorkerID), SenderAddress: run.TaskAddress(f.TaskB),
		IncarnationID: f.WorkerIncarnation, Recipient: run.ManagerAddress(), Kind: run.MessageQuestion,
		BodyPath: "/state/bodies/" + uid(n) + ".md", BodyDigest: "question-" + uid(n), BodyBytes: 8,
	}
}

// taskEnvelopes counts the envelopes addressed to the fixture's task.
func (f *mailboxFixture) taskEnvelopes(t *testing.T, store *sqlite.Store) int {
	t.Helper()
	return countRows(t, store, `SELECT COUNT(*) FROM messages WHERE run_id = ? AND recipient_address = ?`, f.spec.RunID.String(), app.AddressString(run.TaskAddress(f.TaskB)))
}

// TestAnswerAcceptanceBothOrders drives section 5's admission rule for
// answers in both commit orders from separate handles, with no run-state
// gate involved: an answer that commits BEFORE the result acceptance is an
// undelivered message, so the acceptance is transient until the worker
// drains it; once the acceptance has closed the mailbox, an answer to the
// worker's other question is refused mailbox-closed with a receipt and no
// envelope — and a retry of the answer accepted before the closure still
// replays as duplicate, while the refused one stays refused.
func TestAnswerAcceptanceBothOrders(t *testing.T) {
	clock := newFakeClock()
	root := t.TempDir()
	storeA := openStoreAt(t, root, clock)
	storeB := openStoreAt(t, root, clock)
	f := newMailboxFixtureAt(t, storeA, clock)
	q1, q2 := f.mailboxWorkerQuestion(9701), f.mailboxWorkerQuestion(9702)
	requireSendOutcome(t, storeA, "worker question 1", q1, app.MessageAccepted, "")
	requireSendOutcome(t, storeA, "worker question 2", q2, app.MessageAccepted, "")

	// Order 1: the answer commits first.
	early := requireSendOutcome(t, storeB, "answer before acceptance", f.managerAnswer(9703, q1.ID, "early", "answer-1"), app.MessageAccepted, "")
	transient, err := storeA.SubmitResult(t.Context(), f.resultFor(9704))
	if err != nil || transient.Kind != app.SubmissionTransient || !strings.Contains(transient.Detail, "drain with hop msg next") {
		t.Fatalf("SubmitResult(undelivered answer) = %+v, %v; want the transient drain outcome", transient, err)
	}
	fetch := app.MessageFetch{RunID: f.spec.RunID, SessionID: f.WorkerID, IncarnationID: f.WorkerIncarnation, Address: run.TaskAddress(f.TaskB)}
	delivery, served, err := storeA.FetchNextMessage(t.Context(), fetch)
	if err != nil || !served || delivery.Message.ID != early.MessageID {
		t.Fatalf("worker drain fetch = %+v served=%t err=%v; want the early answer", delivery.Message, served, err)
	}
	if outcome, ackErr := storeA.AckMessage(t.Context(), app.MessageAck{RunID: f.spec.RunID, MessageID: early.MessageID, SessionID: f.WorkerID, IncarnationID: f.WorkerIncarnation}); ackErr != nil || outcome.Kind != app.AckAccepted {
		t.Fatalf("worker drain ack = %+v, %v", outcome, ackErr)
	}
	if accepted, submitErr := storeA.SubmitResult(t.Context(), f.resultFor(9704)); submitErr != nil || accepted.Kind != app.SubmissionAccepted {
		t.Fatalf("SubmitResult(after drain) = %+v, %v; want accepted", accepted, submitErr)
	}
	if n := countRows(t, storeA, `SELECT COUNT(*) FROM tasks WHERE id = ? AND mailbox_closed_at IS NOT NULL`, f.TaskB.String()); n != 1 {
		t.Fatalf("closed task mailboxes = %d after acceptance, want 1", n)
	}

	// Order 2: the acceptance committed first.
	late := f.managerAnswer(9705, q2.ID, "late", "answer-2")
	for _, attempt := range []string{"first", "same request id retried"} {
		before := messageReceiptCount(t, storeA, f.spec.RunID)
		requireSendOutcome(t, storeB, "answer after acceptance ("+attempt+")", late, app.MessageMailboxClose, app.GrammarReasonMailboxClosed)
		if after := messageReceiptCount(t, storeA, f.spec.RunID); after != before+1 {
			t.Fatalf("answer after acceptance (%s) left %d receipts, want exactly one more than %d", attempt, after, before)
		}
	}
	requireNoAnswer(t, storeA, "after the closure", q2.ID)
	if n := countRows(t, storeA, `SELECT COUNT(*) FROM message_receipts WHERE op = 'msg-send' AND outcome = 'refused-mailbox-closed' AND claimed_message_id = ?`, q2.ID.String()); n != 2 {
		t.Fatalf("mailbox-closed receipts naming question 2 = %d, want 2", n)
	}

	// The answer accepted before the closure replays unchanged.
	for _, retry := range []app.MessageSend{
		f.managerAnswer(9706, q1.ID, "early", "answer-1"),
		f.managerAnswer(9707, q1.ID, "", "answer-1"),
	} {
		got := requireSendOutcome(t, storeB, "retry of the early answer", retry, app.MessageDuplicate, "")
		if got.MessageID != early.MessageID {
			t.Fatalf("early-answer retry names %s, want %s", got.MessageID, early.MessageID)
		}
	}
	requireSendOutcome(t, storeB, "a different early answer", f.managerAnswer(9708, q1.ID, "", "answer-other"), app.MessageConflicting, app.GrammarReasonConflicting)
	if n := f.taskEnvelopes(t, storeA); n != 1 {
		t.Fatalf("envelopes addressed to the task = %d, want only the early answer", n)
	}
}

// TestAnswerAcceptanceRaced races the manager's answer against the worker's
// result acceptance from separate handles: SQLite serializes the pair, so
// exactly one serialization is observed each round — the answer lands and
// the acceptance is transient, or the acceptance lands and the answer is
// refused mailbox-closed with no envelope. Never both accepted, never an
// answer stranded in a closed mailbox.
func TestAnswerAcceptanceRaced(t *testing.T) {
	for round := range 6 {
		clock := newFakeClock()
		root := t.TempDir()
		storeA := openStoreAt(t, root, clock)
		storeB := openStoreAt(t, root, clock)
		f := newMailboxFixtureAt(t, storeA, clock)
		question := f.mailboxWorkerQuestion(9711)
		requireSendOutcome(t, storeA, "worker question", question, app.MessageAccepted, "")

		var (
			start      sync.WaitGroup
			done       sync.WaitGroup
			answer     app.MessageOutcome
			submission app.SubmissionOutcome
			answerErr  error
			submitErr  error
		)
		start.Add(1)
		done.Add(2)
		go func() {
			defer done.Done()
			start.Wait()
			answer, answerErr = storeB.SendMessage(t.Context(), f.managerAnswer(9712, question.ID, "raced", "answer"))
		}()
		go func() {
			defer done.Done()
			start.Wait()
			submission, submitErr = storeA.SubmitResult(t.Context(), f.resultFor(9713))
		}()
		start.Done()
		done.Wait()

		if answerErr != nil || submitErr != nil {
			t.Fatalf("round %d: raced writes errored: %v, %v", round, answerErr, submitErr)
		}
		t.Logf("round %d: answer %s, submission %s", round, answer.Kind, submission.Kind)
		switch {
		case answer.Kind == app.MessageAccepted && submission.Kind == app.SubmissionTransient:
			if n := f.taskEnvelopes(t, storeA); n != 1 {
				t.Fatalf("round %d: answer landed first, but the task has %d envelopes", round, n)
			}
			if n := countRows(t, storeA, `SELECT COUNT(*) FROM tasks WHERE id = ? AND mailbox_closed_at IS NULL`, f.TaskB.String()); n != 1 {
				t.Fatalf("round %d: the transient acceptance closed the mailbox", round)
			}
		case answer.Kind == app.MessageMailboxClose && answer.Reason == app.GrammarReasonMailboxClosed && submission.Kind == app.SubmissionAccepted:
			if n := f.taskEnvelopes(t, storeA); n != 0 {
				t.Fatalf("round %d: the refused answer left %d envelopes in the closed mailbox", round, n)
			}
		default:
			t.Fatalf("round %d: answer = %+v, submission = %+v; want exactly one serialization", round, answer, submission)
		}
	}
}

// TestAnswerAfterFailureClosureRefused proves the same admission rule for
// the other closure cause: a task that settled failed closed its mailbox,
// so the manager's answer to that worker's question is refused
// mailbox-closed with no envelope.
func TestAnswerAfterFailureClosureRefused(t *testing.T) {
	clock := newFakeClock()
	store := openStoreAt(t, t.TempDir(), clock)
	f := newMailboxFixtureAt(t, store, clock)
	question := f.mailboxWorkerQuestion(9721)
	requireSendOutcome(t, store, "worker question", question, app.MessageAccepted, "")
	f.inUOW(t, func(uow app.UnitOfWork) {
		task, revision, err := uow.Tasks().Get(t.Context(), f.TaskB)
		if err != nil {
			t.Fatalf("load task: %v", err)
		}
		failed, err := task.Fail(clock.Now())
		if err != nil {
			t.Fatalf("fail task: %v", err)
		}
		if _, err := uow.Tasks().Save(t.Context(), failed.CloseMailbox(clock.Now()), revision); err != nil {
			t.Fatalf("save failed task: %v", err)
		}
	})

	requireSendOutcome(t, store, "answer after failure closure", f.managerAnswer(9722, question.ID, "", "answer"), app.MessageMailboxClose, app.GrammarReasonMailboxClosed)
	requireNoAnswer(t, store, "after the failure closure", question.ID)
}

// TestHumanAnswerAfterOriginClosure proves hop answer never lands in a task
// mailbox: a human answer to the manager's relay is addressed to the
// manager and accepted even after the relayed worker's task mailbox has
// closed (no run-state or task gate applies to it), while the manager's
// forward of that answer to the closed task is refused mailbox-closed.
func TestHumanAnswerAfterOriginClosure(t *testing.T) {
	clock := newFakeClock()
	store := openStoreAt(t, t.TempDir(), clock)
	f := newMailboxFixtureAt(t, store, clock)
	question := f.mailboxWorkerQuestion(9731)
	requireSendOutcome(t, store, "worker question", question, app.MessageAccepted, "")
	relayOf := question.ID
	relay := app.MessageSend{
		ID: identity.MessageID(uid(9732)), RunID: f.spec.RunID,
		Sender: run.SessionPrincipal(f.ManagerID), SenderAddress: run.ManagerAddress(),
		IncarnationID: f.ManagerIncarnation, Recipient: run.HumanAddress(), Kind: run.MessageQuestion,
		RelayedFrom: &relayOf, BodyPath: "/state/relay", BodyDigest: "relay", BodyBytes: 5,
	}
	requireSendOutcome(t, store, "manager relay", relay, app.MessageAccepted, "")
	if outcome, err := store.SubmitResult(t.Context(), f.resultFor(9733)); err != nil || outcome.Kind != app.SubmissionAccepted {
		t.Fatalf("SubmitResult() = %+v, %v; want accepted", outcome, err)
	}

	human := app.HumanAnswer{
		ID: identity.MessageID(uid(9734)), RunID: f.spec.RunID, QuestionID: relay.ID,
		BodyPath: "/state/human", BodyDigest: "human", BodyBytes: 5,
	}
	if outcome, err := store.AnswerQuestion(t.Context(), human); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("AnswerQuestion() = %+v, %v; want accepted to the manager", outcome, err)
	}
	if n := countRows(t, store, `SELECT COUNT(*) FROM messages WHERE id = ? AND recipient_address = 'manager'`, human.ID.String()); n != 1 {
		t.Fatalf("human answer addressed to the manager = %d rows, want 1", n)
	}
	requireSendOutcome(t, store, "manager forward to the closed task", f.managerAnswer(9735, question.ID, "forward", "human"), app.MessageMailboxClose, app.GrammarReasonMailboxClosed)
	if n := f.taskEnvelopes(t, store); n != 0 {
		t.Fatalf("envelopes addressed to the closed task = %d, want 0", n)
	}
}
