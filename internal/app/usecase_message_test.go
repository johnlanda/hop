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

// plainReadStore implements exactly app.ReadStore over an inner
// *fakeStore, deliberately NOT forwarding app.WorkflowReadStore (no
// embedding, so no method promotion): a read store that predates Phase 3
// feature-mode support, for proving ShowMessage's fail-closed contract —
// the identical treatment plainStore/plainUnitOfWork (usecase_schedule_
// test.go) already give app.WorkflowRepositories.
type plainReadStore struct{ inner *fakeStore }

func (p plainReadStore) ListRuns(ctx context.Context, repositoryRoot string) ([]app.RunStatus, error) {
	return p.inner.ListRuns(ctx, repositoryRoot)
}

func (p plainReadStore) LoadRunStatus(ctx context.Context, runID identity.RunID) (app.RunDetail, error) {
	return p.inner.LoadRunStatus(ctx, runID)
}

func (p plainReadStore) LoadFrozenRun(ctx context.Context, runID identity.RunID) (app.FrozenRun, error) {
	return p.inner.LoadFrozenRun(ctx, runID)
}

func (p plainReadStore) LoadCheckExecutionContext(ctx context.Context, op identity.OperationID) (app.CheckExecutionContext, error) {
	return p.inner.LoadCheckExecutionContext(ctx, op)
}

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
		if result.Outcome != string(app.MessageRefused) || result.Reason != app.GrammarReasonUnauthorized {
			t.Fatalf("SendMessage() = %+v, want refused/%s (a worker may only send to manager)", result, app.GrammarReasonUnauthorized)
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
		if result.Outcome != string(app.MessageMailboxClose) || result.Reason != app.GrammarReasonMailboxClosed {
			t.Fatalf("SendMessage() = %+v, want refused-mailbox-closed/%s", result, app.GrammarReasonMailboxClosed)
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
		if result.Outcome != string(app.MessageMalformed) || result.Reason != app.GrammarReasonMalformed {
			t.Fatalf("SendMessage() = %+v, want malformed/%s", result, app.GrammarReasonMalformed)
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

	t.Run("a session claiming a different run is refused before the body is ever written", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr1 := seedFeatureRun(t, tc, 2)
		fr2 := seedFeatureRun(t, tc, 2)

		// fr1's manager session, but the request claims fr2 as its run —
		// the exact cross-run authentication gap the review found: the
		// driving use case must refuse this BEFORE writing the body
		// artifact or calling the store at all.
		result, err := tc.Controller.SendMessage(context.Background(), app.SendMessageRequest{
			RunID: fr2.RunID.String(), SessionID: fr1.ManagerID.String(), IncarnationID: fr1.ManagerIncarnation.String(),
			StateRoot: "/state", To: "human", Kind: "question", Body: []byte("cross-run?"),
		})
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}
		if result.Outcome != string(app.MessageRefused) || result.Reason != app.GrammarReasonUnauthorized {
			t.Fatalf("SendMessage(cross-run) = %+v, want refused/%s", result, app.GrammarReasonUnauthorized)
		}
		if len(tc.Store.Messages) != 0 {
			t.Fatalf("a cross-run send must not create a message")
		}
		if len(tc.Artifacts.files) != 0 {
			t.Fatalf("a cross-run send must not write the body artifact")
		}
	})
}

// TestFetchMessageCrossRunRefused proves the driving use case refuses a
// session claiming a different run than its own before ever calling the
// store — the same gap TestSendMessageValidation's cross-run subtest
// covers for SendMessage.
func TestFetchMessageCrossRunRefused(t *testing.T) {
	tc := newTestController(defaultPolicy())
	fr1 := seedFeatureRun(t, tc, 2)
	fr2 := seedFeatureRun(t, tc, 2)

	_, err := tc.Controller.FetchMessage(context.Background(), app.FetchMessageRequest{
		RunID: fr2.RunID.String(), SessionID: fr1.ManagerID.String(), IncarnationID: fr1.ManagerIncarnation.String(),
	})
	if !errors.Is(err, app.ErrMessagingUnauthorized) {
		t.Fatalf("FetchMessage(cross-run) error = %v, want ErrMessagingUnauthorized", err)
	}
}

// TestAckMessageCrossRunRefused proves the driving use case refuses a
// session claiming a different run than its own before ever calling the
// store, distinct from TestAckMessageRequiresOwnDelivery's own-run-but-
// never-delivered case.
func TestAckMessageCrossRunRefused(t *testing.T) {
	tc := newTestController(defaultPolicy())
	fr1 := seedFeatureRun(t, tc, 2)
	fr2 := seedFeatureRun(t, tc, 2)
	taskB := seedImplementTask(t, tc, fr1.RunID, 1, "B", false, run.TaskReady)
	workerID, workerIncarnation := seedWorkerSession(t, tc, fr1, taskB)
	ctx := context.Background()

	send, err := tc.Controller.SendMessage(ctx, app.SendMessageRequest{
		RunID: fr1.RunID.String(), SessionID: workerID.String(), IncarnationID: workerIncarnation.String(),
		StateRoot: "/state", To: "manager", Kind: "question", Body: []byte("q?"),
	})
	if err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
	if _, fetchErr := tc.Controller.FetchMessage(ctx, app.FetchMessageRequest{
		RunID: fr1.RunID.String(), SessionID: fr1.ManagerID.String(), IncarnationID: fr1.ManagerIncarnation.String(),
	}); fetchErr != nil {
		t.Fatalf("FetchMessage() error = %v", fetchErr)
	}

	// fr2's manager session, but the request claims fr1 (the message's
	// actual run) — an unrelated run's session acking someone else's
	// message.
	ack, err := tc.Controller.AckMessage(ctx, app.AckMessageRequest{
		RunID: fr1.RunID.String(), MessageID: send.MessageID, SessionID: fr2.ManagerID.String(), IncarnationID: fr2.ManagerIncarnation.String(),
	})
	if err != nil {
		t.Fatalf("AckMessage() error = %v", err)
	}
	if ack.Outcome != string(app.AckRefused) || ack.Reason != app.GrammarReasonUnauthorized {
		t.Fatalf("AckMessage(cross-run) = %+v, want refused/%s", ack, app.GrammarReasonUnauthorized)
	}
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
	if ack.Outcome != string(app.AckRefused) || ack.Reason != app.GrammarReasonNotDelivered {
		t.Fatalf("AckMessage() = %+v, want refused/%s (not yet delivered)", ack, app.GrammarReasonNotDelivered)
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

// TestAckMessageRequiresCurrentIncarnation is the successor/relaunch
// regression: a delivery served to a session's earlier, now-superseded
// incarnation must never authorize that same session's CURRENT incarnation
// to ack without itself being served — section 7's delivery fence is
// keyed on (session, incarnation), not session alone.
func TestAckMessageRequiresCurrentIncarnation(t *testing.T) {
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	taskB := seedImplementTask(t, tc, fr.RunID, 1, "B", false, run.TaskReady)
	workerID, firstIncarnation := seedWorkerSession(t, tc, fr, taskB)
	ctx := context.Background()

	send, err := tc.Controller.SendMessage(ctx, app.SendMessageRequest{
		RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
		StateRoot: "/state", To: "task:" + taskB.String(), Kind: "info", Body: []byte("update"),
	})
	if err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
	if _, fetchErr := tc.Controller.FetchMessage(ctx, app.FetchMessageRequest{
		RunID: fr.RunID.String(), SessionID: workerID.String(), IncarnationID: firstIncarnation.String(),
	}); fetchErr != nil {
		t.Fatalf("FetchMessage() at the first incarnation error = %v", fetchErr)
	}

	// A warm reattach supersedes the first incarnation with a second, same
	// session — seeded directly, mirroring how resume's own adoption path
	// mints a binding under a distinct incarnation (test-only state
	// seeding, bypassing lease fencing, per this file's established
	// convention).
	history := tc.Store.Bindings[workerID]
	if len(history) != 1 {
		t.Fatalf("Bindings[worker] = %+v, want exactly one binding before reattach", history)
	}
	history[0].Superseded = true
	secondIncarnation, err := identity.ParseIncarnationID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse incarnation id: %v", err)
	}
	second := run.NewRuntimeBinding(workerID, secondIncarnation, history[0].ServerSocketPath, history[0].ServerInstance, history[0].WorkspaceID, history[0].TabID, history[0].PaneID, history[0].CreationLabel, run.LaunchResume, tc.Clock.Now())
	tc.Store.Bindings[workerID] = append(history, second)

	// The current (second) incarnation was never itself served this
	// message — only the now-superseded first incarnation was. Acking
	// must be refused, not accepted on the strength of that stale
	// delivery.
	ack, err := tc.Controller.AckMessage(ctx, app.AckMessageRequest{
		RunID: fr.RunID.String(), MessageID: send.MessageID, SessionID: workerID.String(), IncarnationID: secondIncarnation.String(),
	})
	if err != nil {
		t.Fatalf("AckMessage() error = %v", err)
	}
	if ack.Outcome != string(app.AckRefused) || ack.Reason != app.GrammarReasonNotDelivered {
		t.Fatalf("AckMessage() at the current-but-never-served incarnation = %+v, want refused/%s", ack, app.GrammarReasonNotDelivered)
	}

	// Once the current incarnation is actually served (a re-serve of the
	// same still-in-flight message), it may ack.
	refetch, err := tc.Controller.FetchMessage(ctx, app.FetchMessageRequest{
		RunID: fr.RunID.String(), SessionID: workerID.String(), IncarnationID: secondIncarnation.String(),
	})
	if err != nil {
		t.Fatalf("FetchMessage() at the second incarnation error = %v", err)
	}
	if !refetch.Delivered || refetch.MessageID != send.MessageID {
		t.Fatalf("FetchMessage() at the second incarnation = %+v, want the same message re-served", refetch)
	}
	ack, err = tc.Controller.AckMessage(ctx, app.AckMessageRequest{
		RunID: fr.RunID.String(), MessageID: send.MessageID, SessionID: workerID.String(), IncarnationID: secondIncarnation.String(),
	})
	if err != nil {
		t.Fatalf("AckMessage() after re-serve error = %v", err)
	}
	if ack.Outcome != string(app.AckAccepted) {
		t.Fatalf("AckMessage() after re-serve to the current incarnation = %+v, want accepted", ack)
	}
}

func TestShowMessage(t *testing.T) {
	t.Run("renders the envelope plus full delivery/ack history", func(t *testing.T) {
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

		// A first fetch, then a re-serve (both re-servable while
		// unacknowledged): two delivery rows for the one message.
		if _, fetchErr := tc.Controller.FetchMessage(ctx, app.FetchMessageRequest{
			RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
		}); fetchErr != nil {
			t.Fatalf("FetchMessage() 1st error = %v", fetchErr)
		}
		tc.Clock.Advance(5 * time.Second)
		if _, fetchErr := tc.Controller.FetchMessage(ctx, app.FetchMessageRequest{
			RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
		}); fetchErr != nil {
			t.Fatalf("FetchMessage() re-serve error = %v", fetchErr)
		}

		show, err := tc.Controller.ShowMessage(ctx, app.ShowMessageRequest{RunID: fr.RunID.String(), MessageID: send.MessageID})
		if err != nil {
			t.Fatalf("ShowMessage() error = %v", err)
		}
		if !show.Found || show.MessageID != send.MessageID || show.Kind != "question" {
			t.Fatalf("ShowMessage() = %+v, want the found envelope", show)
		}
		if show.SenderKind != "session" || show.SenderSession != workerID.String() {
			t.Fatalf("ShowMessage().Sender = %s/%s, want session/%s", show.SenderKind, show.SenderSession, workerID)
		}
		if show.Recipient != "manager" {
			t.Fatalf("ShowMessage().Recipient = %s, want manager", show.Recipient)
		}
		if len(show.Deliveries) != 2 {
			t.Fatalf("ShowMessage().Deliveries = %+v, want two (fetch + re-serve)", show.Deliveries)
		}
		if show.Acknowledged {
			t.Fatalf("ShowMessage().Acknowledged = true before any ack")
		}

		if _, ackErr := tc.Controller.AckMessage(ctx, app.AckMessageRequest{
			RunID: fr.RunID.String(), MessageID: send.MessageID, SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
		}); ackErr != nil {
			t.Fatalf("AckMessage() error = %v", ackErr)
		}
		show, err = tc.Controller.ShowMessage(ctx, app.ShowMessageRequest{RunID: fr.RunID.String(), MessageID: send.MessageID})
		if err != nil {
			t.Fatalf("ShowMessage() after ack error = %v", err)
		}
		if !show.Acknowledged || show.AcknowledgedAt.IsZero() {
			t.Fatalf("ShowMessage() after ack = %+v, want an acknowledged time", show)
		}
	})

	t.Run("writes, delivers and acks nothing", func(t *testing.T) {
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
		before := len(tc.Store.MessageDeliveries[identity.MessageID(send.MessageID)])

		if _, err := tc.Controller.ShowMessage(ctx, app.ShowMessageRequest{RunID: fr.RunID.String(), MessageID: send.MessageID}); err != nil {
			t.Fatalf("ShowMessage() error = %v", err)
		}
		if _, err := tc.Controller.ShowMessage(ctx, app.ShowMessageRequest{RunID: fr.RunID.String(), MessageID: send.MessageID}); err != nil {
			t.Fatalf("ShowMessage() 2nd error = %v", err)
		}

		after := len(tc.Store.MessageDeliveries[identity.MessageID(send.MessageID)])
		if after != before {
			t.Fatalf("ShowMessage() must never deliver: deliveries before=%d after=%d", before, after)
		}
		if _, acked := tc.Store.MessageAcks[identity.MessageID(send.MessageID)]; acked {
			t.Fatalf("ShowMessage() must never ack")
		}
	})

	t.Run("an unknown message id is refused not-found", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)

		show, err := tc.Controller.ShowMessage(context.Background(), app.ShowMessageRequest{
			RunID: fr.RunID.String(), MessageID: "00000000-0000-4000-8000-000000000000",
		})
		if err != nil {
			t.Fatalf("ShowMessage() error = %v", err)
		}
		if show.Found {
			t.Fatalf("ShowMessage() = %+v, want not found", show)
		}
	})

	t.Run("a message from a different run is reported exactly like not-found", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr1 := seedFeatureRun(t, tc, 2)
		fr2 := seedFeatureRun(t, tc, 2)
		taskB := seedImplementTask(t, tc, fr1.RunID, 1, "B", false, run.TaskReady)
		workerID, workerIncarnation := seedWorkerSession(t, tc, fr1, taskB)

		send, err := tc.Controller.SendMessage(context.Background(), app.SendMessageRequest{
			RunID: fr1.RunID.String(), SessionID: workerID.String(), IncarnationID: workerIncarnation.String(),
			StateRoot: "/state", To: "manager", Kind: "question", Body: []byte("q?"),
		})
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}

		show, err := tc.Controller.ShowMessage(context.Background(), app.ShowMessageRequest{RunID: fr2.RunID.String(), MessageID: send.MessageID})
		if err != nil {
			t.Fatalf("ShowMessage() error = %v", err)
		}
		if show.Found {
			t.Fatalf("ShowMessage() across runs = %+v, want not found", show)
		}
	})

	t.Run("without a workflow-capable read store it fails closed", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		legacy := *tc.Controller
		legacy.Read = plainReadStore{inner: tc.Store}
		if _, err := legacy.ShowMessage(context.Background(), app.ShowMessageRequest{RunID: "r", MessageID: "m"}); !errors.Is(err, app.ErrWorkflowReadStoreUnsupported) {
			t.Fatalf("ShowMessage() over a plain ReadStore error = %v, want ErrWorkflowReadStoreUnsupported", err)
		}
	})
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

// TestMessageRefusalReasonsAlwaysSet drives every remaining SendMessage/
// Answer refusal shape not already asserted above (ruling B extended to
// messaging: a refused/malformed outcome with an empty Reason is a
// defect) and checks the exact grammar.go token each one sets, through
// the driving Controller exactly as cmd/hop renders it.
func TestMessageRefusalReasonsAlwaysSet(t *testing.T) {
	t.Run("a run that left running refuses a send", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		rRow := tc.Store.Runs[fr.RunID]
		completing, err := rRow.value.EnterCompleting(tc.Clock.Now())
		if err != nil {
			t.Fatalf("EnterCompleting() error = %v", err)
		}
		rRow.value = completing

		result, err := tc.Controller.SendMessage(context.Background(), app.SendMessageRequest{
			RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
			StateRoot: "/state", To: "human", Kind: "question", Body: []byte("q?"),
		})
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}
		if result.Outcome != string(app.MessageRunNotAccept) || result.Reason != app.GrammarReasonRunNotAccepting {
			t.Fatalf("SendMessage() over a non-running run = %+v, want refused-run-not-accepting/%s", result, app.GrammarReasonRunNotAccepting)
		}
	})

	t.Run("a stale sender incarnation is refused", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		history := tc.Store.Bindings[fr.ManagerID]
		history[len(history)-1].Superseded = true

		result, err := tc.Controller.SendMessage(context.Background(), app.SendMessageRequest{
			RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
			StateRoot: "/state", To: "human", Kind: "question", Body: []byte("q?"),
		})
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}
		if result.Outcome != string(app.MessageRefused) || result.Reason != app.GrammarReasonStale {
			t.Fatalf("SendMessage() with a stale incarnation = %+v, want refused/%s", result, app.GrammarReasonStale)
		}
	})

	t.Run("a conflicting request id is refused for send", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		first, err := tc.Controller.SendMessage(context.Background(), app.SendMessageRequest{
			RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
			StateRoot: "/state", To: "human", Kind: "question", Body: []byte("first"), RequestID: "shared-send-id",
		})
		if err != nil || first.Outcome != string(app.MessageAccepted) {
			t.Fatalf("SendMessage(first) = %+v, err = %v", first, err)
		}
		second, err := tc.Controller.SendMessage(context.Background(), app.SendMessageRequest{
			RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
			StateRoot: "/state", To: "human", Kind: "question", Body: []byte("different"), RequestID: "shared-send-id",
		})
		if err != nil {
			t.Fatalf("SendMessage(second) error = %v", err)
		}
		if second.Outcome != string(app.MessageRefused) || second.Reason != app.GrammarReasonConflicting {
			t.Fatalf("SendMessage() with a reused, conflicting request id = %+v, want refused/%s", second, app.GrammarReasonConflicting)
		}
	})

	t.Run("a session answer to an unknown question is malformed", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		unknown, err := identity.ParseMessageID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse message id: %v", err)
		}
		result, err := tc.Controller.SendMessage(context.Background(), app.SendMessageRequest{
			RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
			StateRoot: "/state", Kind: "answer", ReplyTo: unknown.String(), Body: []byte("a"), Inline: true,
		})
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}
		if result.Outcome != string(app.MessageMalformed) || result.Reason != app.GrammarReasonMalformed {
			t.Fatalf("SendMessage(session answer to unknown question) = %+v, want malformed/%s", result, app.GrammarReasonMalformed)
		}
	})

	t.Run("an answer to an unknown question is malformed", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		unknown, err := identity.ParseMessageID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse message id: %v", err)
		}
		result, err := tc.Controller.Answer(context.Background(), app.AnswerRequest{
			RunID: fr.RunID.String(), QuestionID: unknown.String(), StateRoot: "/state", Body: []byte("a"), Inline: true,
		})
		if err != nil {
			t.Fatalf("Answer() error = %v", err)
		}
		if result.Outcome != string(app.MessageMalformed) || result.Reason != app.GrammarReasonMalformed {
			t.Fatalf("Answer(unknown question) = %+v, want malformed/%s", result, app.GrammarReasonMalformed)
		}
	})

	t.Run("an answer to a non-human-addressed question is refused", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskB := seedImplementTask(t, tc, fr.RunID, 1, "B", false, run.TaskReady)
		workerID, workerIncarnation := seedWorkerSession(t, tc, fr, taskB)
		send, err := tc.Controller.SendMessage(context.Background(), app.SendMessageRequest{
			RunID: fr.RunID.String(), SessionID: workerID.String(), IncarnationID: workerIncarnation.String(),
			StateRoot: "/state", To: "manager", Kind: "question", Body: []byte("q?"),
		})
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}

		result, err := tc.Controller.Answer(context.Background(), app.AnswerRequest{
			RunID: fr.RunID.String(), QuestionID: send.MessageID, StateRoot: "/state", Body: []byte("a"), Inline: true,
		})
		if err != nil {
			t.Fatalf("Answer() error = %v", err)
		}
		if result.Outcome != string(app.MessageRefused) || result.Reason != app.GrammarReasonMalformed {
			t.Fatalf("Answer(non-human-addressed question) = %+v, want refused/%s", result, app.GrammarReasonMalformed)
		}
	})

	t.Run("a conflicting request id is refused for answer", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		send, err := tc.Controller.SendMessage(context.Background(), app.SendMessageRequest{
			RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
			StateRoot: "/state", To: "human", Kind: "question", Body: []byte("q?"),
		})
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}

		first, err := tc.Controller.Answer(context.Background(), app.AnswerRequest{
			RunID: fr.RunID.String(), QuestionID: send.MessageID, StateRoot: "/state", Body: []byte("first"), Inline: true, RequestID: "shared-answer-id",
		})
		if err != nil || first.Outcome != string(app.MessageAccepted) {
			t.Fatalf("Answer(first) = %+v, err = %v", first, err)
		}
		second, err := tc.Controller.Answer(context.Background(), app.AnswerRequest{
			RunID: fr.RunID.String(), QuestionID: send.MessageID, StateRoot: "/state", Body: []byte("different"), Inline: true, RequestID: "shared-answer-id",
		})
		if err != nil {
			t.Fatalf("Answer(second) error = %v", err)
		}
		if second.Outcome != string(app.MessageRefused) || second.Reason != app.GrammarReasonConflicting {
			t.Fatalf("Answer() with a reused, conflicting request id = %+v, want refused/%s", second, app.GrammarReasonConflicting)
		}
	})
}

// TestMessageWaitDefault covers ruling C: hop msg wait's default --timeout
// resolves from the run's own frozen [messages] wait_timeout when set,
// falling back to app.DefaultMessageWait only when the frozen value is
// zero (a solo run's zero WorkflowSnapshot, or a feature run whose policy
// never set one).
func TestMessageWaitDefault(t *testing.T) {
	t.Run("resolves the run's frozen wait_timeout", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		snap := tc.Store.Snapshots[fr.RunID]
		snap.Workflow.MessageWait = 17 * time.Second
		tc.Store.Snapshots[fr.RunID] = snap

		got, err := tc.Controller.MessageWaitDefault(context.Background(), fr.RunID.String())
		if err != nil {
			t.Fatalf("MessageWaitDefault() error = %v", err)
		}
		if got != 17*time.Second {
			t.Fatalf("MessageWaitDefault() = %s, want 17s", got)
		}
	})

	t.Run("falls back to the package default when the frozen value is zero", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		snap := tc.Store.Snapshots[fr.RunID]
		snap.Workflow.MessageWait = 0
		tc.Store.Snapshots[fr.RunID] = snap

		got, err := tc.Controller.MessageWaitDefault(context.Background(), fr.RunID.String())
		if err != nil {
			t.Fatalf("MessageWaitDefault() error = %v", err)
		}
		if got != app.DefaultMessageWait {
			t.Fatalf("MessageWaitDefault() = %s, want the package default %s", got, app.DefaultMessageWait)
		}
	})
}
