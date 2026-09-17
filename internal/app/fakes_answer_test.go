package app_test

import (
	"context"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// This file is fakeStore's half of the answer authority and admission
// scenarios internal/adapters/sqlite's answer_admission_test.go drives
// against the real store: the same requests, the same outcomes, tokens and
// state.

// answerSender is one session's messaging identity at its true logical
// address.
type answerSender struct {
	session     identity.SessionID
	address     run.Address
	incarnation identity.IncarnationID
}

// answer builds this sender's answer to question.
func (a answerSender) answer(t *testing.T, tc *testController, runID identity.RunID, question identity.MessageID, requestID, bodyDigest string) app.MessageSend {
	t.Helper()
	replyTo := question
	return app.MessageSend{
		ID: mintMessageID(t, tc), RunID: runID,
		Sender: run.SessionPrincipal(a.session), SenderAddress: a.address, IncarnationID: a.incarnation,
		Kind: run.MessageAnswer, ReplyTo: &replyTo, RequestID: requestID,
		BodyPath: "/state/bodies/answer.md", BodyDigest: bodyDigest, BodyBytes: 6,
	}
}

// question builds this sender's question to recipient.
func (a answerSender) question(t *testing.T, tc *testController, runID identity.RunID, recipient run.Address, relayOf *identity.MessageID) app.MessageSend {
	t.Helper()
	return app.MessageSend{
		ID: mintMessageID(t, tc), RunID: runID,
		Sender: run.SessionPrincipal(a.session), SenderAddress: a.address, IncarnationID: a.incarnation,
		Recipient: recipient, Kind: run.MessageQuestion, RelayedFrom: relayOf,
		BodyPath: "/state/bodies/question.md", BodyDigest: "question", BodyBytes: 8,
	}
}

// managerSender is fr's manager as an answerSender.
func managerSender(fr featureRun) answerSender { //nolint:gocritic // hugeParam: featureRun is a small test fixture value passed once per call, never a hot loop.
	return answerSender{session: fr.ManagerID, address: run.ManagerAddress(), incarnation: fr.ManagerIncarnation}
}

// requireFakeSend sends one request through tc's fakeStore and fails unless
// it decides kind with reason; it returns the outcome.
func requireFakeSend(t *testing.T, tc *testController, label string, send app.MessageSend, kind app.MessageOutcomeKind, reason string) app.MessageOutcome { //nolint:gocritic // hugeParam: the port passes the send value; the helper mirrors it.
	t.Helper()
	outcome, err := tc.Store.SendMessage(context.Background(), send)
	if err != nil {
		t.Fatalf("%s: SendMessage() error = %v", label, err)
	}
	if outcome.Kind != kind || outcome.Reason != reason {
		t.Fatalf("%s: SendMessage() = %+v, want %s/%q", label, outcome, kind, reason)
	}
	return outcome
}

// fakeAnswersTo counts the answer envelopes replying to question.
func fakeAnswersTo(tc *testController, question identity.MessageID) int {
	n := 0
	for id := range tc.Store.Messages {
		m := tc.Store.Messages[id]
		if m.Kind == run.MessageAnswer && m.ReplyTo != nil && *m.ReplyTo == question {
			n++
		}
	}
	return n
}

// fakeEnvelopesFor counts the envelopes addressed to address.
func fakeEnvelopesFor(tc *testController, address run.Address) int {
	n := 0
	for id := range tc.Store.Messages {
		if tc.Store.Messages[id].Recipient.Equal(address) {
			n++
		}
	}
	return n
}

// seedTwoWorkers seeds a feature run with two assigned implement tasks, B
// and C, and returns their workers.
func seedTwoWorkers(t *testing.T, tc *testController) (fr featureRun, workerB, workerC answerSender) {
	t.Helper()
	fr = seedFeatureRun(t, tc, 2)
	taskB := seedImplementTask(t, tc, fr.RunID, 1, "B", false, run.TaskReady)
	bSession, bIncarnation := seedWorkerSession(t, tc, fr, taskB)
	taskC := seedImplementTask(t, tc, fr.RunID, 2, "C", false, run.TaskReady)
	cSession, cIncarnation := seedWorkerSession(t, tc, fr, taskC)
	return fr,
		answerSender{session: bSession, address: run.TaskAddress(taskB), incarnation: bIncarnation},
		answerSender{session: cSession, address: run.TaskAddress(taskC), incarnation: cIncarnation}
}

// TestFakeAnswerRecipientAuthority mirrors TestAnswerFromNonRecipientRefused
// and TestAnswerToAnotherAddressRefused: only the question's addressed
// recipient may answer it.
func TestFakeAnswerRecipientAuthority(t *testing.T) {
	t.Run("the manager's own human question", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr, workerB, _ := seedTwoWorkers(t, tc)
		question := managerSender(fr).question(t, tc, fr.RunID, run.HumanAddress(), nil)
		requireFakeSend(t, tc, "manager question to human", question, app.MessageAccepted, "")

		stolen := workerB.answer(t, tc, fr.RunID, question.ID, "worker-answer", "wrong-answer")
		requireFakeSend(t, tc, "worker answer", stolen, app.MessageRefused, app.GrammarReasonUnauthorized)
		requireFakeSend(t, tc, "worker answer retried", stolen, app.MessageRefused, app.GrammarReasonUnauthorized)
		if n := fakeAnswersTo(tc, question.ID); n != 0 {
			t.Fatalf("answers after the refused worker answer = %d, want 0", n)
		}
		if _, acked := tc.Store.MessageAcks[question.ID]; acked {
			t.Fatal("the refused worker answer acknowledged the human question")
		}

		human, err := tc.Store.AnswerQuestion(context.Background(), app.HumanAnswer{
			ID: mintMessageID(t, tc), RunID: fr.RunID, QuestionID: question.ID,
			BodyPath: "/state/a", BodyDigest: "right-answer", BodyBytes: 12,
		})
		if err != nil || human.Kind != app.MessageAccepted {
			t.Fatalf("AnswerQuestion() = %+v, %v; want accepted", human, err)
		}
		if ack, acked := tc.Store.MessageAcks[question.ID]; !acked || ack.SessionID != "" {
			t.Fatalf("bundled human-question ack = %+v (present %t), want one session-less ack", ack, acked)
		}
	})

	t.Run("a manager relay of the worker's own question", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr, workerB, _ := seedTwoWorkers(t, tc)
		manager := managerSender(fr)
		original := workerB.question(t, tc, fr.RunID, run.ManagerAddress(), nil)
		requireFakeSend(t, tc, "worker question", original, app.MessageAccepted, "")
		relay := manager.question(t, tc, fr.RunID, run.HumanAddress(), &original.ID)
		requireFakeSend(t, tc, "manager relay", relay, app.MessageAccepted, "")

		requireFakeSend(t, tc, "worker answer to the relay", workerB.answer(t, tc, fr.RunID, relay.ID, "", "self-answer"), app.MessageRefused, app.GrammarReasonUnauthorized)
		requireFakeSend(t, tc, "manager answer to its own relay", manager.answer(t, tc, fr.RunID, relay.ID, "", "self-answer"), app.MessageRefused, app.GrammarReasonUnauthorized)
		if n := fakeAnswersTo(tc, relay.ID); n != 0 {
			t.Fatalf("answers to the relay = %d, want 0", n)
		}

		humanID := mintMessageID(t, tc)
		human, err := tc.Store.AnswerQuestion(context.Background(), app.HumanAnswer{
			ID: humanID, RunID: fr.RunID, QuestionID: relay.ID,
			BodyPath: "/state/a", BodyDigest: "human-answer", BodyBytes: 12,
		})
		if err != nil || human.Kind != app.MessageAccepted {
			t.Fatalf("AnswerQuestion(relay) = %+v, %v; want accepted", human, err)
		}
		if got := tc.Store.Messages[humanID].Recipient; !got.Equal(run.ManagerAddress()) {
			t.Fatalf("human answer recipient = %+v, want manager", got)
		}
		requireFakeSend(t, tc, "manager forward to the worker", manager.answer(t, tc, fr.RunID, original.ID, "forward", "human-answer"), app.MessageAccepted, "")
	})

	t.Run("questions addressed to another address", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr, workerB, workerC := seedTwoWorkers(t, tc)
		manager := managerSender(fr)

		toB := manager.question(t, tc, fr.RunID, workerB.address, nil)
		requireFakeSend(t, tc, "manager question to task B", toB, app.MessageAccepted, "")
		requireFakeSend(t, tc, "task C answering task B's question", workerC.answer(t, tc, fr.RunID, toB.ID, "", "c"), app.MessageRefused, app.GrammarReasonUnauthorized)
		requireFakeSend(t, tc, "the manager answering its own question to task B", manager.answer(t, tc, fr.RunID, toB.ID, "", "m"), app.MessageRefused, app.GrammarReasonUnauthorized)
		byB := requireFakeSend(t, tc, "task B answering", workerB.answer(t, tc, fr.RunID, toB.ID, "", "b"), app.MessageAccepted, "")
		if got := tc.Store.Messages[byB.MessageID].Recipient; !got.Equal(run.ManagerAddress()) {
			t.Fatalf("task B's answer recipient = %+v, want manager", got)
		}

		fromB := workerB.question(t, tc, fr.RunID, run.ManagerAddress(), nil)
		requireFakeSend(t, tc, "task B question to the manager", fromB, app.MessageAccepted, "")
		requireFakeSend(t, tc, "task C answering task B's question", workerC.answer(t, tc, fr.RunID, fromB.ID, "", "c"), app.MessageRefused, app.GrammarReasonUnauthorized)
		requireFakeSend(t, tc, "task B answering its own question", workerB.answer(t, tc, fr.RunID, fromB.ID, "", "b"), app.MessageRefused, app.GrammarReasonUnauthorized)
		byManager := requireFakeSend(t, tc, "the manager answering", manager.answer(t, tc, fr.RunID, fromB.ID, "", "m"), app.MessageAccepted, "")
		answer := tc.Store.Messages[byManager.MessageID]
		// The refusals above consumed no sequence number: the answer is the
		// second envelope task B's mailbox has ever admitted, exactly the
		// real store's MAX+1.
		if !answer.Recipient.Equal(workerB.address) || answer.EnqueueSeq != 2 {
			t.Fatalf("the manager's answer = recipient %+v seq %d, want task B at seq 2", answer.Recipient, answer.EnqueueSeq)
		}
	})
}

// TestFakeAnswerRetryPrecedence mirrors TestAnswerRetryPrecedence: the
// request-ID receipt first, then recipient authority, then the prior
// answer's content.
func TestFakeAnswerRetryPrecedence(t *testing.T) {
	tc := newTestController(defaultPolicy())
	fr, workerB, workerC := seedTwoWorkers(t, tc)
	manager := managerSender(fr)
	question := workerB.question(t, tc, fr.RunID, run.ManagerAddress(), nil)
	requireFakeSend(t, tc, "task B question", question, app.MessageAccepted, "")
	accepted := requireFakeSend(t, tc, "manager answer", manager.answer(t, tc, fr.RunID, question.ID, "fwd-1", "digest-fwd"), app.MessageAccepted, "")

	steps := []struct {
		name   string
		send   app.MessageSend
		kind   app.MessageOutcomeKind
		reason string
	}{
		{"the same request id and body", manager.answer(t, tc, fr.RunID, question.ID, "fwd-1", "digest-fwd"), app.MessageDuplicate, ""},
		{"the same request id with another body", manager.answer(t, tc, fr.RunID, question.ID, "fwd-1", "digest-other"), app.MessageRefused, app.GrammarReasonConflicting},
		{"a fresh request id with the same body", manager.answer(t, tc, fr.RunID, question.ID, "fwd-2", "digest-fwd"), app.MessageDuplicate, ""},
		{"no request id with the same body", manager.answer(t, tc, fr.RunID, question.ID, "", "digest-fwd"), app.MessageDuplicate, ""},
		{"a fresh request id with another body", manager.answer(t, tc, fr.RunID, question.ID, "fwd-3", "digest-other"), app.MessageConflicting, app.GrammarReasonConflicting},
		{"a non-recipient reusing the recipient's request id", workerC.answer(t, tc, fr.RunID, question.ID, "fwd-1", "digest-fwd"), app.MessageRefused, app.GrammarReasonConflicting},
		{"a non-recipient with a fresh request id and the accepted body", workerC.answer(t, tc, fr.RunID, question.ID, "c-1", "digest-fwd"), app.MessageRefused, app.GrammarReasonUnauthorized},
		{"a non-recipient with no request id and the accepted body", workerC.answer(t, tc, fr.RunID, question.ID, "", "digest-fwd"), app.MessageRefused, app.GrammarReasonUnauthorized},
		{"a non-recipient with another body", workerC.answer(t, tc, fr.RunID, question.ID, "c-2", "digest-other"), app.MessageRefused, app.GrammarReasonUnauthorized},
		{"the question's own sender", workerB.answer(t, tc, fr.RunID, question.ID, "b-1", "digest-fwd"), app.MessageRefused, app.GrammarReasonUnauthorized},
	}
	for _, step := range steps {
		got := requireFakeSend(t, tc, step.name, step.send, step.kind, step.reason)
		if step.kind == app.MessageDuplicate && got.MessageID != accepted.MessageID {
			t.Fatalf("%s: duplicate names %s, want the accepted answer %s", step.name, got.MessageID, accepted.MessageID)
		}
		if step.kind != app.MessageDuplicate && got.MessageID != "" {
			t.Fatalf("%s: refusal names message %s, want none", step.name, got.MessageID)
		}
	}
	if n := fakeAnswersTo(tc, question.ID); n != 1 {
		t.Fatalf("answer envelopes = %d, want the one accepted answer", n)
	}
}

// TestFakeAnswerMailboxAdmission mirrors TestAnswerAcceptanceBothOrders,
// TestAnswerAfterFailureClosureRefused and TestHumanAnswerAfterOriginClosure.
func TestFakeAnswerMailboxAdmission(t *testing.T) {
	t.Run("both commit orders around result acceptance", func(t *testing.T) {
		f := newMailboxFixture(t)
		worker := answerSender{session: f.w.SessionID, address: run.TaskAddress(f.TaskID), incarnation: f.w.IncarnationID}
		manager := managerSender(f.fr)
		q1 := worker.question(t, f.tc, f.fr.RunID, run.ManagerAddress(), nil)
		q2 := worker.question(t, f.tc, f.fr.RunID, run.ManagerAddress(), nil)
		requireFakeSend(t, f.tc, "worker question 1", q1, app.MessageAccepted, "")
		requireFakeSend(t, f.tc, "worker question 2", q2, app.MessageAccepted, "")

		early := requireFakeSend(t, f.tc, "answer before acceptance", manager.answer(t, f.tc, f.fr.RunID, q1.ID, "early", "answer-1"), app.MessageAccepted, "")
		if outcome := f.submit(t); outcome.Kind != app.SubmissionTransient {
			t.Fatalf("submission with the answer undelivered = %+v, want transient", outcome)
		}
		f.drain(t)
		if outcome := f.submit(t); outcome.Kind != app.SubmissionAccepted {
			t.Fatalf("submission after drain = %+v, want accepted", outcome)
		}

		late := manager.answer(t, f.tc, f.fr.RunID, q2.ID, "late", "answer-2")
		requireFakeSend(t, f.tc, "answer after acceptance", late, app.MessageMailboxClose, app.GrammarReasonMailboxClosed)
		requireFakeSend(t, f.tc, "answer after acceptance retried", late, app.MessageMailboxClose, app.GrammarReasonMailboxClosed)
		if n := fakeAnswersTo(f.tc, q2.ID); n != 0 {
			t.Fatalf("answers to question 2 = %d after the closure, want 0", n)
		}
		for _, retry := range []app.MessageSend{
			manager.answer(t, f.tc, f.fr.RunID, q1.ID, "early", "answer-1"),
			manager.answer(t, f.tc, f.fr.RunID, q1.ID, "", "answer-1"),
		} {
			if got := requireFakeSend(t, f.tc, "retry of the early answer", retry, app.MessageDuplicate, ""); got.MessageID != early.MessageID {
				t.Fatalf("early-answer retry names %s, want %s", got.MessageID, early.MessageID)
			}
		}
		requireFakeSend(t, f.tc, "a different early answer", manager.answer(t, f.tc, f.fr.RunID, q1.ID, "", "answer-other"), app.MessageConflicting, app.GrammarReasonConflicting)
		if n := fakeEnvelopesFor(f.tc, worker.address); n != 1 {
			t.Fatalf("envelopes addressed to the task = %d, want only the early answer", n)
		}
	})

	t.Run("failure closure", func(t *testing.T) {
		f := newMailboxFixture(t)
		worker := answerSender{session: f.w.SessionID, address: run.TaskAddress(f.TaskID), incarnation: f.w.IncarnationID}
		question := worker.question(t, f.tc, f.fr.RunID, run.ManagerAddress(), nil)
		requireFakeSend(t, f.tc, "worker question", question, app.MessageAccepted, "")
		row := f.tc.Store.Tasks[f.TaskID]
		failed, err := row.value.Fail(f.tc.Clock.Now())
		if err != nil {
			t.Fatalf("fail task: %v", err)
		}
		row.value = failed.CloseMailbox(f.tc.Clock.Now())

		requireFakeSend(t, f.tc, "answer after failure closure", managerSender(f.fr).answer(t, f.tc, f.fr.RunID, question.ID, "", "answer"), app.MessageMailboxClose, app.GrammarReasonMailboxClosed)
		if n := fakeAnswersTo(f.tc, question.ID); n != 0 {
			t.Fatalf("answers after the failure closure = %d, want 0", n)
		}
	})

	t.Run("a human answer after the origin's closure", func(t *testing.T) {
		f := newMailboxFixture(t)
		worker := answerSender{session: f.w.SessionID, address: run.TaskAddress(f.TaskID), incarnation: f.w.IncarnationID}
		manager := managerSender(f.fr)
		question := worker.question(t, f.tc, f.fr.RunID, run.ManagerAddress(), nil)
		requireFakeSend(t, f.tc, "worker question", question, app.MessageAccepted, "")
		relay := manager.question(t, f.tc, f.fr.RunID, run.HumanAddress(), &question.ID)
		requireFakeSend(t, f.tc, "manager relay", relay, app.MessageAccepted, "")
		if outcome := f.submit(t); outcome.Kind != app.SubmissionAccepted {
			t.Fatalf("submission = %+v, want accepted", outcome)
		}

		humanID := mintMessageID(t, f.tc)
		human, err := f.tc.Store.AnswerQuestion(context.Background(), app.HumanAnswer{
			ID: humanID, RunID: f.fr.RunID, QuestionID: relay.ID,
			BodyPath: "/state/human", BodyDigest: "human", BodyBytes: 5,
		})
		if err != nil || human.Kind != app.MessageAccepted || !f.tc.Store.Messages[humanID].Recipient.Equal(run.ManagerAddress()) {
			t.Fatalf("AnswerQuestion() = %+v, %v; want accepted to the manager", human, err)
		}
		requireFakeSend(t, f.tc, "manager forward to the closed task", manager.answer(t, f.tc, f.fr.RunID, question.ID, "forward", "human"), app.MessageMailboxClose, app.GrammarReasonMailboxClosed)
		if n := fakeEnvelopesFor(f.tc, worker.address); n != 0 {
			t.Fatalf("envelopes addressed to the closed task = %d, want 0", n)
		}
	})
}
