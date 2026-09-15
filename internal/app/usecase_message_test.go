package app_test

import (
	"context"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// seedWorkerSession assigns taskID via the scheduler (proving the fixture
// through code already tested, rather than hand-rolling attempt/session
// rows) and returns the resulting worker session and its current
// incarnation.
func seedWorkerSession(t *testing.T, tc *testController, fr featureRun, taskID identity.TaskID) (identity.SessionID, identity.IncarnationID) { //nolint:gocritic // hugeParam: featureRun is a small test fixture value passed once per call, never a hot loop.
	t.Helper()
	report, err := tc.Controller.AssignReadyTasks(context.Background(), fr.Handle, defaultAssignmentOptions())
	if err != nil {
		t.Fatalf("AssignReadyTasks() error = %v", err)
	}
	var assigned *app.AssignedTask
	for i := range report.Assigned {
		if report.Assigned[i].TaskID == taskID {
			assigned = &report.Assigned[i]
		}
	}
	if assigned == nil {
		t.Fatalf("task %s was not assigned; report = %+v", taskID, report)
	}
	binding, ok := tc.Store.currentBindingLocked(assigned.SessionID)
	if !ok {
		t.Fatalf("no binding recorded for assigned session %s", assigned.SessionID)
	}
	return assigned.SessionID, binding.IncarnationID
}

func TestMessagingReferenceTraceRelayedQuestion(t *testing.T) {
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	taskB := seedImplementTask(t, tc, fr.RunID, 1, "B", false, run.TaskReady)
	workerID, workerIncarnation := seedWorkerSession(t, tc, fr, taskB)

	ctx := context.Background()

	// 1. Worker (task B) sends question q1 to manager.
	sendQ1, err := tc.Controller.SendMessage(ctx, app.SendMessageRequest{
		RunID: fr.RunID.String(), SessionID: workerID.String(), IncarnationID: workerIncarnation.String(),
		StateRoot: "/state", To: "manager", Kind: "question", Body: []byte("what should I do about X?"),
		RequestID: "worker-q1",
	})
	if err != nil {
		t.Fatalf("SendMessage(q1) error = %v", err)
	}
	if sendQ1.Outcome != string(app.MessageAccepted) {
		t.Fatalf("SendMessage(q1) = %+v, want accepted", sendQ1)
	}

	// 2. Manager fetches q1 (delivered).
	fetchQ1, err := tc.Controller.FetchMessage(ctx, app.FetchMessageRequest{
		RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
	})
	if err != nil {
		t.Fatalf("FetchMessage(manager, 1st) error = %v", err)
	}
	if !fetchQ1.Delivered || fetchQ1.MessageID != sendQ1.MessageID || fetchQ1.Kind != "question" {
		t.Fatalf("FetchMessage(manager, 1st) = %+v, want q1 delivered", fetchQ1)
	}

	// 3. Manager sends question q2 to human with --relay-of q1, then acks q1.
	sendQ2, err := tc.Controller.SendMessage(ctx, app.SendMessageRequest{
		RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
		StateRoot: "/state", To: "human", Kind: "question", RelayOf: sendQ1.MessageID,
		Body: []byte("worker needs a decision about X, please advise"), RequestID: "manager-q2",
	})
	if err != nil {
		t.Fatalf("SendMessage(q2) error = %v", err)
	}
	if sendQ2.Outcome != string(app.MessageAccepted) {
		t.Fatalf("SendMessage(q2) = %+v, want accepted", sendQ2)
	}
	ackQ1, err := tc.Controller.AckMessage(ctx, app.AckMessageRequest{
		RunID: fr.RunID.String(), MessageID: sendQ1.MessageID, SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
	})
	if err != nil {
		t.Fatalf("AckMessage(q1) error = %v", err)
	}
	if ackQ1.Outcome != string(app.AckAccepted) {
		t.Fatalf("AckMessage(q1) = %+v, want accepted", ackQ1)
	}

	// 4. Human answers q2 (a2 accepted, q2 acknowledged atomically).
	answerA2, err := tc.Controller.Answer(ctx, app.AnswerRequest{
		RunID: fr.RunID.String(), QuestionID: sendQ2.MessageID, StateRoot: "/state",
		Body: []byte("tell the worker to do Y"), RequestID: "human-a2",
	})
	if err != nil {
		t.Fatalf("Answer(a2) error = %v", err)
	}
	if answerA2.Outcome != string(app.MessageAccepted) {
		t.Fatalf("Answer(a2) = %+v, want accepted", answerA2)
	}

	// 5. Manager fetches: a2's envelope carries origin=q1 resolved
	// server-side.
	fetchA2, err := tc.Controller.FetchMessage(ctx, app.FetchMessageRequest{
		RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
	})
	if err != nil {
		t.Fatalf("FetchMessage(manager, 2nd) error = %v", err)
	}
	if !fetchA2.Delivered || fetchA2.MessageID != answerA2.MessageID || fetchA2.Kind != "answer" {
		t.Fatalf("FetchMessage(manager, 2nd) = %+v, want a2 delivered", fetchA2)
	}
	if fetchA2.SenderKind != "human" {
		t.Fatalf("FetchMessage(manager, 2nd).SenderKind = %s, want human", fetchA2.SenderKind)
	}
	if fetchA2.Origin != sendQ1.MessageID {
		t.Fatalf("FetchMessage(manager, 2nd).Origin = %s, want q1 %s", fetchA2.Origin, sendQ1.MessageID)
	}

	// 6. Manager FORWARDS first (answer a1, reply-to q1, destination
	// DERIVED as task:B from q1's sender), THEN acks a2.
	sendA1, err := tc.Controller.SendMessage(ctx, app.SendMessageRequest{
		RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
		StateRoot: "/state", Kind: "answer", ReplyTo: fetchA2.Origin,
		Body: []byte("do Y"), RequestID: "manager-forward-a1",
	})
	if err != nil {
		t.Fatalf("SendMessage(a1 forward) error = %v", err)
	}
	if sendA1.Outcome != string(app.MessageAccepted) {
		t.Fatalf("SendMessage(a1 forward) = %+v, want accepted", sendA1)
	}
	ackA2, err := tc.Controller.AckMessage(ctx, app.AckMessageRequest{
		RunID: fr.RunID.String(), MessageID: answerA2.MessageID, SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
	})
	if err != nil {
		t.Fatalf("AckMessage(a2) error = %v", err)
	}
	if ackA2.Outcome != string(app.AckAccepted) {
		t.Fatalf("AckMessage(a2) = %+v, want accepted", ackA2)
	}

	// 7. Worker fetches a1 at its task address, then acks.
	fetchA1, err := tc.Controller.FetchMessage(ctx, app.FetchMessageRequest{
		RunID: fr.RunID.String(), SessionID: workerID.String(), IncarnationID: workerIncarnation.String(),
	})
	if err != nil {
		t.Fatalf("FetchMessage(worker) error = %v", err)
	}
	if !fetchA1.Delivered || fetchA1.MessageID != sendA1.MessageID || fetchA1.Kind != "answer" {
		t.Fatalf("FetchMessage(worker) = %+v, want a1 delivered", fetchA1)
	}
	ackA1, err := tc.Controller.AckMessage(ctx, app.AckMessageRequest{
		RunID: fr.RunID.String(), MessageID: sendA1.MessageID, SessionID: workerID.String(), IncarnationID: workerIncarnation.String(),
	})
	if err != nil {
		t.Fatalf("AckMessage(a1) error = %v", err)
	}
	if ackA1.Outcome != string(app.AckAccepted) {
		t.Fatalf("AckMessage(a1) = %+v, want accepted", ackA1)
	}

	// Forward-before-ack redo idempotency: retrying the forward under the
	// same request ID after a1 has already been acked returns duplicate,
	// never a second message.
	sendA1Retry, err := tc.Controller.SendMessage(ctx, app.SendMessageRequest{
		RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
		StateRoot: "/state", Kind: "answer", ReplyTo: fetchA2.Origin,
		Body: []byte("do Y"), RequestID: "manager-forward-a1",
	})
	if err != nil {
		t.Fatalf("SendMessage(a1 retry) error = %v", err)
	}
	if sendA1Retry.Outcome != string(app.MessageDuplicate) || sendA1Retry.MessageID != sendA1.MessageID {
		t.Fatalf("SendMessage(a1 retry) = %+v, want duplicate of %s", sendA1Retry, sendA1.MessageID)
	}
}

func TestSendMessageValidation(t *testing.T) {
	t.Run("a worker cannot send info to a task address (kind/address legality)", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskB := seedImplementTask(t, tc, fr.RunID, 1, "B", false, run.TaskReady)
		workerID, workerIncarnation := seedWorkerSession(t, tc, fr, taskB)

		result, err := tc.Controller.SendMessage(context.Background(), app.SendMessageRequest{
			RunID: fr.RunID.String(), SessionID: workerID.String(), IncarnationID: workerIncarnation.String(),
			StateRoot: "/state", To: "task:" + taskB.String(), Kind: "info", Body: []byte("hi"),
		})
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}
		if result.Outcome != string(app.MessageRefused) {
			t.Fatalf("SendMessage() = %+v, want refused (a worker may only send to manager)", result)
		}
	})

	t.Run("a send to a closed mailbox is refused", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskB := seedImplementTask(t, tc, fr.RunID, 1, "B", false, run.TaskReady)
		seedWorkerSession(t, tc, fr, taskB)
		closed := tc.Store.Tasks[taskB].value
		closed = closed.CloseMailbox(tc.Clock.Now())
		tc.Store.Tasks[taskB].value = closed

		result, err := tc.Controller.SendMessage(context.Background(), app.SendMessageRequest{
			RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
			StateRoot: "/state", To: "task:" + taskB.String(), Kind: "info", Body: []byte("update"),
		})
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}
		if result.Outcome != string(app.MessageMailboxClose) {
			t.Fatalf("SendMessage() = %+v, want refused-mailbox-closed", result)
		}
	})

	t.Run("a body exceeding the inline limit is malformed before any store call", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)

		oversized := make([]byte, app.MessageBodyInlineLimit+1)
		result, err := tc.Controller.SendMessage(context.Background(), app.SendMessageRequest{
			RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
			StateRoot: "/state", To: "human", Kind: "question", Body: oversized, Inline: true,
		})
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}
		if result.Outcome != string(app.MessageMalformed) {
			t.Fatalf("SendMessage() = %+v, want malformed", result)
		}
		if len(tc.Store.Messages) != 0 {
			t.Fatalf("a malformed oversized body must not be written or recorded")
		}
	})

	t.Run("SendMessage without a feature-mode port fails closed", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		solo := *tc.Controller
		solo.Messages = nil
		_, err := solo.SendMessage(context.Background(), app.SendMessageRequest{})
		if err == nil {
			t.Fatalf("SendMessage() over a solo Controller succeeded; want ErrFeatureModeUnsupported")
		}
	})
}

func TestAckMessageRequiresOwnDelivery(t *testing.T) {
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	taskB := seedImplementTask(t, tc, fr.RunID, 1, "B", false, run.TaskReady)
	workerID, workerIncarnation := seedWorkerSession(t, tc, fr, taskB)
	ctx := context.Background()

	send, err := tc.Controller.SendMessage(ctx, app.SendMessageRequest{
		RunID: fr.RunID.String(), SessionID: workerID.String(), IncarnationID: workerIncarnation.String(),
		StateRoot: "/state", To: "manager", Kind: "question", Body: []byte("q?"),
	})
	if err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}

	// Acking before any fetch: never delivered to this session.
	ack, err := tc.Controller.AckMessage(ctx, app.AckMessageRequest{
		RunID: fr.RunID.String(), MessageID: send.MessageID, SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
	})
	if err != nil {
		t.Fatalf("AckMessage() error = %v", err)
	}
	if ack.Outcome != string(app.AckRefused) {
		t.Fatalf("AckMessage() = %+v, want refused (not yet delivered)", ack)
	}

	if _, fetchErr := tc.Controller.FetchMessage(ctx, app.FetchMessageRequest{
		RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
	}); fetchErr != nil {
		t.Fatalf("FetchMessage() error = %v", fetchErr)
	}

	ack, err = tc.Controller.AckMessage(ctx, app.AckMessageRequest{
		RunID: fr.RunID.String(), MessageID: send.MessageID, SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
	})
	if err != nil {
		t.Fatalf("AckMessage() error = %v", err)
	}
	if ack.Outcome != string(app.AckAccepted) {
		t.Fatalf("AckMessage() after fetch = %+v, want accepted", ack)
	}

	// A repeat ack is idempotent.
	ack, err = tc.Controller.AckMessage(ctx, app.AckMessageRequest{
		RunID: fr.RunID.String(), MessageID: send.MessageID, SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
	})
	if err != nil {
		t.Fatalf("AckMessage() error = %v", err)
	}
	if ack.Outcome != string(app.AckDuplicate) {
		t.Fatalf("repeat AckMessage() = %+v, want duplicate", ack)
	}
}

func TestFetchMessageEmptyQueueCommitsNothing(t *testing.T) {
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)

	result, err := tc.Controller.FetchMessage(context.Background(), app.FetchMessageRequest{
		RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
	})
	if err != nil {
		t.Fatalf("FetchMessage() error = %v", err)
	}
	if result.Delivered {
		t.Fatalf("FetchMessage() = %+v, want an empty (undelivered) result", result)
	}
	if len(tc.Store.Messages) != 0 || len(tc.Store.MessageDeliveries) != 0 {
		t.Fatalf("an empty fetch must commit nothing: messages=%d deliveries=%d", len(tc.Store.Messages), len(tc.Store.MessageDeliveries))
	}
}
