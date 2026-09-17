package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
	"github.com/johnlanda/hop/internal/testsupport/storevectors"
)

// fakeChild is one child session of a feature fixture, bound to its own
// attempt.
type fakeChild struct {
	Session     identity.SessionID
	Attempt     identity.AttemptID
	Incarnation identity.IncarnationID
}

// seedBoundChild creates attempt number of task in attemptState and a
// child session of the fixture manager in sessionState, bound by a current
// binding, directly in the fake.
func seedBoundChild(t *testing.T, tc *testController, fr featureRun, task identity.TaskID, number int, attemptState run.AttemptState, sessionState run.SessionState) fakeChild { //nolint:gocritic // hugeParam: featureRun is a small test fixture value passed once per call.
	t.Helper()
	now := tc.Clock.Now()
	attemptID, err := identity.ParseAttemptID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse attempt id: %v", err)
	}
	sessionID, err := identity.ParseSessionID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse session id: %v", err)
	}
	incarnationID, err := identity.ParseIncarnationID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse incarnation id: %v", err)
	}
	attempt, err := run.NewAttempt(attemptID, task, number, now)
	if err != nil {
		t.Fatalf("NewAttempt() error = %v", err)
	}
	attempt.State = attemptState
	tc.Store.Attempts[attemptID] = &entityRow[run.Attempt]{value: attempt, revision: 1}
	session, err := run.NewChildSession(sessionID, fr.RunID, attemptID, run.RoleImplementer, tc.Store.Sessions[fr.ManagerID].value, run.HarnessClaude, now)
	if err != nil {
		t.Fatalf("NewChildSession() error = %v", err)
	}
	session.State = sessionState
	tc.Store.Sessions[sessionID] = &entityRow[run.Session]{value: session, revision: 1}
	tc.Store.Bindings[sessionID] = append(tc.Store.Bindings[sessionID],
		run.NewRuntimeBinding(sessionID, incarnationID, "", fakeServerToken(1), "ws", "tab", "pane-"+sessionID.String(), "label-"+sessionID.String(), run.LaunchInitial, now))
	return fakeChild{Session: sessionID, Attempt: attemptID, Incarnation: incarnationID}
}

// fakeLineage is the fake half of the sqlite adapter's taskLineageFixture:
// one implement task whose attempt 1 runs behind a bound, active session.
type fakeLineage struct {
	tc       *testController
	fr       featureRun
	Task     identity.TaskID
	Old, New fakeChild
}

func newFakeLineage(t *testing.T) *fakeLineage {
	t.Helper()
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	task := seedImplementTask(t, tc, fr.RunID, 1, "B", false, run.TaskActive)
	old := seedBoundChild(t, tc, fr, task, 1, run.AttemptRunning, run.SessionActive)
	return &fakeLineage{tc: tc, fr: fr, Task: task, Old: old}
}

// retireAndRetry interrupts attempt 1, terminates its session with the
// binding untouched, and runs attempt 2 behind its own bound session.
func (f *fakeLineage) retireAndRetry(t *testing.T) {
	t.Helper()
	f.tc.Store.Attempts[f.Old.Attempt].value.State = run.AttemptInterrupted
	f.tc.Store.Sessions[f.Old.Session].value.State = run.SessionTerminated
	f.New = seedBoundChild(t, f.tc, f.fr, f.Task, 2, run.AttemptRunning, run.SessionActive)
}

func (f *fakeLineage) queue(t *testing.T) identity.MessageID {
	t.Helper()
	id := mintMessageID(t, f.tc)
	outcome, err := f.tc.Store.SendMessage(context.Background(), app.MessageSend{
		ID: id, RunID: f.fr.RunID, Sender: run.SessionPrincipal(f.fr.ManagerID), SenderAddress: run.ManagerAddress(),
		IncarnationID: f.fr.ManagerIncarnation, Recipient: run.TaskAddress(f.Task), Kind: run.MessageInfo,
		BodyPath: "/state/b", BodyDigest: "d-" + id.String(), BodyBytes: 1,
	})
	if err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("queue info = %+v, %v", outcome, err)
	}
	return id
}

func (f *fakeLineage) fetch(child fakeChild) (app.MessageDelivery, bool, error) {
	return f.tc.Store.FetchNextMessage(context.Background(), app.MessageFetch{RunID: f.fr.RunID, SessionID: child.Session, IncarnationID: child.Incarnation, Address: run.TaskAddress(f.Task)})
}

func (f *fakeLineage) ack(t *testing.T, message identity.MessageID, child fakeChild) app.MessageAckOutcome {
	t.Helper()
	outcome, err := f.tc.Store.AckMessage(context.Background(), app.MessageAck{RunID: f.fr.RunID, MessageID: message, SessionID: child.Session, IncarnationID: child.Incarnation})
	if err != nil {
		t.Fatalf("AckMessage() error = %v", err)
	}
	return outcome
}

// fakePtr returns a pointer to a copy of v.
func fakePtr[T any](v T) *T { return &v }

// requireFakeFetchRefused asserts the fake refused fetch as not the
// address's current session, with nothing served.
func requireFakeFetchRefused(t *testing.T, tc *testController, fetch *app.MessageFetch, detail string) {
	t.Helper()
	deliveries := 0
	for _, rows := range tc.Store.MessageDeliveries {
		deliveries += len(rows)
	}
	delivery, served, err := tc.Store.FetchNextMessage(context.Background(), *fetch)
	if served || delivery.Message.ID != "" || !errors.Is(err, app.ErrMessagingUnauthorized) || !strings.Contains(err.Error(), detail) {
		t.Fatalf("FetchNextMessage = %+v, %t, %v; want ErrMessagingUnauthorized %q and nothing served", delivery, served, err, detail)
	}
	if strings.Contains(err.Error(), fetch.SessionID.String()) || strings.Contains(err.Error(), fetch.IncarnationID.String()) {
		t.Fatalf("refusal %q echoes the caller's session or incarnation", err)
	}
	after := 0
	for _, rows := range tc.Store.MessageDeliveries {
		after += len(rows)
	}
	if after != deliveries {
		t.Fatalf("a refused fetch recorded %d deliveries", after-deliveries)
	}
}

// TestFakeFetchRequiresTheTaskCurrentAttemptSession is the fake half of
// the sqlite adapter's TestFetchRequiresTheTaskCurrentAttemptSession,
// shape for shape.
func TestFakeFetchRequiresTheTaskCurrentAttemptSession(t *testing.T) {
	refused := func(f *fakeLineage) app.MessageFetch {
		return storevectors.MessageFetchSupersededAttempt(f.fr.RunID, f.Old.Session, f.Old.Incarnation, f.Task)
	}

	t.Run("a retired attempt's session after a retry; the successor is served", func(t *testing.T) {
		f := newFakeLineage(t)
		f.retireAndRetry(t)
		message := f.queue(t)
		requireFakeFetchRefused(t, f.tc, fakePtr(refused(f)), storevectors.MessageFetchSupersededAttemptDetail)
		if delivery, served, err := f.fetch(f.New); err != nil || !served || delivery.Message.ID != message {
			t.Fatalf("successor fetch = %+v, %t, %v; want the queued message", delivery, served, err)
		}
		if n := len(f.tc.Store.MessageDeliveries[message]); n != 1 {
			t.Fatalf("delivery rows = %d, want only the successor's", n)
		}
		if ack := f.ack(t, message, f.New); ack.Kind != app.AckAccepted {
			t.Fatalf("successor ack = %+v, want accepted", ack)
		}
	})

	t.Run("an interrupted attempt whose session is still active and bound", func(t *testing.T) {
		f := newFakeLineage(t)
		f.tc.Store.Attempts[f.Old.Attempt].value.State = run.AttemptInterrupted
		f.queue(t)
		requireFakeFetchRefused(t, f.tc, fakePtr(refused(f)), storevectors.MessageFetchSupersededAttemptDetail)
	})

	t.Run("a terminated session on a live, newest attempt", func(t *testing.T) {
		f := newFakeLineage(t)
		f.tc.Store.Sessions[f.Old.Session].value.State = run.SessionTerminated
		f.queue(t)
		requireFakeFetchRefused(t, f.tc, fakePtr(refused(f)), storevectors.MessageFetchSupersededAttemptDetail)
	})

	t.Run("a live session on an attempt that is not its task's newest", func(t *testing.T) {
		f := newFakeLineage(t)
		newerID, err := identity.ParseAttemptID(f.tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse attempt id: %v", err)
		}
		newer, err := run.NewAttempt(newerID, f.Task, 2, f.tc.Clock.Now())
		if err != nil {
			t.Fatalf("NewAttempt() error = %v", err)
		}
		newer.State = run.AttemptInterrupted
		f.tc.Store.Attempts[newerID] = &entityRow[run.Attempt]{value: newer, revision: 1}
		f.queue(t)
		requireFakeFetchRefused(t, f.tc, fakePtr(refused(f)), storevectors.MessageFetchSupersededAttemptDetail)
	})

	t.Run("the current session is served", func(t *testing.T) {
		f := newFakeLineage(t)
		message := f.queue(t)
		if delivery, served, err := f.fetch(f.Old); err != nil || !served || delivery.Message.ID != message {
			t.Fatalf("current fetch = %+v, %t, %v; want the queued message", delivery, served, err)
		}
	})
}

// TestFakeAckRequiresTheTaskCurrentAttemptSession is the fake half of the
// sqlite adapter's TestAckRequiresTheTaskCurrentAttemptSession.
func TestFakeAckRequiresTheTaskCurrentAttemptSession(t *testing.T) {
	f := newFakeLineage(t)
	first := f.queue(t)
	second := f.queue(t)
	if delivery, served, err := f.fetch(f.Old); err != nil || !served || delivery.Message.ID != first {
		t.Fatalf("fetch while current = %+v, %t, %v; want the first message", delivery, served, err)
	}
	f.retireAndRetry(t)

	stale, err := f.tc.Store.AckMessage(context.Background(), storevectors.AckMessageSupersededAttempt(f.fr.RunID, first, f.Old.Session, f.Old.Incarnation))
	if err != nil || stale.Kind != app.AckRefused || stale.Reason != storevectors.AckMessageSupersededAttemptReason {
		t.Fatalf("retired session's ack = %+v, %v; want refused %s", stale, err, storevectors.AckMessageSupersededAttemptReason)
	}
	if _, acked := f.tc.Store.MessageAcks[first]; acked {
		t.Fatal("a stale ack recorded an ack")
	}
	if notServed := f.ack(t, second, f.Old); notServed.Kind != app.AckRefused || notServed.Reason != app.GrammarReasonNotDelivered {
		t.Fatalf("retired session's ack of an unserved message = %+v; want refused not-delivered", notServed)
	}
	if delivery, served, err := f.fetch(f.New); err != nil || !served || delivery.Message.ID != first {
		t.Fatalf("successor fetch = %+v, %t, %v; want the in-flight first message re-served", delivery, served, err)
	}
	if ack := f.ack(t, first, f.New); ack.Kind != app.AckAccepted {
		t.Fatalf("successor ack = %+v; want accepted", ack)
	}
	if recorded := f.tc.Store.MessageAcks[first]; recorded.SessionID != f.New.Session {
		t.Fatalf("first message acked by %s, want the successor %s", recorded.SessionID, f.New.Session)
	}
	if dup := f.ack(t, first, f.Old); dup.Kind != app.AckDuplicate {
		t.Fatalf("repeat ack from the retired session = %+v; want duplicate", dup)
	}
	if delivery, served, err := f.fetch(f.New); err != nil || !served || delivery.Message.ID != second {
		t.Fatalf("successor's next fetch = %+v, %t, %v; want the second message", delivery, served, err)
	}
}

// endFakeManagerWithSuccessor terminates the fixture manager with its
// binding still current and seeds an active, bound successor manager.
func endFakeManagerWithSuccessor(t *testing.T, tc *testController, fr featureRun) fakeChild { //nolint:gocritic // hugeParam: featureRun is a small test fixture value passed once per call.
	t.Helper()
	now := tc.Clock.Now()
	tc.Store.Sessions[fr.ManagerID].value.State = run.SessionTerminated
	successorID, err := identity.ParseSessionID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse session id: %v", err)
	}
	incarnationID, err := identity.ParseIncarnationID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse incarnation id: %v", err)
	}
	manager := run.NewManagerSession(successorID, fr.RunID, run.HarnessClaude, now)
	manager.State = run.SessionActive
	tc.Store.Sessions[successorID] = &entityRow[run.Session]{value: manager, revision: 1}
	tc.Store.Bindings[successorID] = append(tc.Store.Bindings[successorID],
		run.NewRuntimeBinding(successorID, incarnationID, "", fakeServerToken(1), "ws-m", "tab-m", "pane-"+successorID.String(), "label-"+successorID.String(), run.LaunchResume, now))
	return fakeChild{Session: successorID, Incarnation: incarnationID}
}

// TestFakeManagerMessagingRequiresAManagerSessionThatHasNotEnded is the fake
// half of the sqlite adapter's
// TestManagerMessagingRequiresAManagerSessionThatHasNotEnded.
func TestFakeManagerMessagingRequiresAManagerSessionThatHasNotEnded(t *testing.T) {
	f := newFakeLineage(t)
	send := func() identity.MessageID {
		id := mintMessageID(t, f.tc)
		outcome, err := f.tc.Store.SendMessage(context.Background(), app.MessageSend{
			ID: id, RunID: f.fr.RunID, Sender: run.SessionPrincipal(f.Old.Session), SenderAddress: run.TaskAddress(f.Task),
			IncarnationID: f.Old.Incarnation, Recipient: run.ManagerAddress(), Kind: run.MessageQuestion,
			BodyPath: "/state/q", BodyDigest: "q-" + id.String(), BodyBytes: 1,
		})
		if err != nil || outcome.Kind != app.MessageAccepted {
			t.Fatalf("seed worker question = %+v, %v", outcome, err)
		}
		return id
	}
	first, second := send(), send()
	managerFetch := func(session identity.SessionID, incarnation identity.IncarnationID) app.MessageFetch {
		return app.MessageFetch{RunID: f.fr.RunID, SessionID: session, IncarnationID: incarnation, Address: run.ManagerAddress()}
	}
	if delivery, served, err := f.tc.Store.FetchNextMessage(context.Background(), managerFetch(f.fr.ManagerID, f.fr.ManagerIncarnation)); err != nil || !served || delivery.Message.ID != first {
		t.Fatalf("manager fetch while current = %+v, %t, %v", delivery, served, err)
	}
	successor := endFakeManagerWithSuccessor(t, f.tc, f.fr)

	requireFakeFetchRefused(t, f.tc, fakePtr(storevectors.MessageFetchEndedManager(f.fr.RunID, f.fr.ManagerID, f.fr.ManagerIncarnation)), storevectors.MessageFetchEndedManagerDetail)
	stale, err := f.tc.Store.AckMessage(context.Background(), storevectors.AckMessageEndedManager(f.fr.RunID, first, f.fr.ManagerID, f.fr.ManagerIncarnation))
	if err != nil || stale.Kind != app.AckRefused || stale.Reason != storevectors.AckMessageEndedManagerReason {
		t.Fatalf("ended manager's ack = %+v, %v; want refused %s", stale, err, storevectors.AckMessageEndedManagerReason)
	}
	if delivery, served, fetchErr := f.tc.Store.FetchNextMessage(context.Background(), managerFetch(successor.Session, successor.Incarnation)); fetchErr != nil || !served || delivery.Message.ID != first {
		t.Fatalf("successor manager fetch = %+v, %t, %v; want the in-flight message re-served", delivery, served, fetchErr)
	}
	ack, err := f.tc.Store.AckMessage(context.Background(), app.MessageAck{RunID: f.fr.RunID, MessageID: first, SessionID: successor.Session, IncarnationID: successor.Incarnation})
	if err != nil || ack.Kind != app.AckAccepted {
		t.Fatalf("successor manager ack = %+v, %v; want accepted", ack, err)
	}
	if delivery, served, fetchErr := f.tc.Store.FetchNextMessage(context.Background(), managerFetch(successor.Session, successor.Incarnation)); fetchErr != nil || !served || delivery.Message.ID != second {
		t.Fatalf("successor manager's next fetch = %+v, %t, %v; want the second message", delivery, served, fetchErr)
	}
}

// queueQuestion sends the task one manager question and requires its
// acceptance.
func (f *fakeLineage) queueQuestion(t *testing.T) identity.MessageID {
	t.Helper()
	question := managerSender(f.fr).question(t, f.tc, f.fr.RunID, run.TaskAddress(f.Task), nil)
	requireFakeSend(t, f.tc, "manager question to the task", question, app.MessageAccepted, "")
	return question.ID
}

// sender is child's messaging identity at the task's lineage address.
func (f *fakeLineage) sender(child fakeChild) answerSender {
	return answerSender{session: child.Session, address: run.TaskAddress(f.Task), incarnation: child.Incarnation}
}

// requireFakeSendStale sends one request the fake must refuse as not its
// address's current session: stale with the value-free detail and no
// envelope.
func requireFakeSendStale(t *testing.T, tc *testController, label string, send app.MessageSend, detail string) { //nolint:gocritic // hugeParam: the port passes the send value; the helper mirrors it.
	t.Helper()
	got := requireFakeSend(t, tc, label, send, app.MessageRefused, app.GrammarReasonStale)
	if got.Detail != detail || got.MessageID != "" {
		t.Fatalf("%s: SendMessage() = %+v, want detail %q naming no message", label, got, detail)
	}
	if _, exists := tc.Store.Messages[send.ID]; exists {
		t.Fatalf("%s: the refused send created an envelope", label)
	}
}

// TestFakeSendRequiresTheAddressCurrentSession is the fake half of the
// sqlite adapter's TestSendRequiresTheAddressCurrentSession.
func TestFakeSendRequiresTheAddressCurrentSession(t *testing.T) {
	infoToManager := func(t *testing.T, f *fakeLineage, child fakeChild) app.MessageSend {
		t.Helper()
		send := f.sender(child).question(t, f.tc, f.fr.RunID, run.ManagerAddress(), nil)
		send.Kind, send.BodyDigest = run.MessageInfo, "info-"+send.ID.String()
		return send
	}

	t.Run("a retired attempt's session after a retry; the successor answers", func(t *testing.T) {
		f := newFakeLineage(t)
		question := f.queueQuestion(t)
		if delivery, served, err := f.fetch(f.Old); err != nil || !served || delivery.Message.ID != question {
			t.Fatalf("fetch while current = %+v, %t, %v; want the question", delivery, served, err)
		}
		f.retireAndRetry(t)

		requireFakeSendStale(t, f.tc, "retired session's answer",
			storevectors.MessageSendSupersededAttempt(f.fr.RunID, f.Old.Session, f.Task, f.Old.Incarnation, mintMessageID(t, f.tc), question, "/state/a.md", "stale-answer", 5),
			storevectors.MessageSendSupersededAttemptDetail)
		requireFakeSendStale(t, f.tc, "retired session's info", infoToManager(t, f, f.Old), storevectors.MessageSendSupersededAttemptDetail)
		requireFakeSendStale(t, f.tc, "retired session's question", f.sender(f.Old).question(t, f.tc, f.fr.RunID, run.ManagerAddress(), nil), storevectors.MessageSendSupersededAttemptDetail)
		if n := fakeAnswersTo(f.tc, question); n != 0 {
			t.Fatalf("answers after the retired session's answer = %d, want 0", n)
		}

		if delivery, served, err := f.fetch(f.New); err != nil || !served || delivery.Message.ID != question {
			t.Fatalf("successor fetch = %+v, %t, %v; want the question re-served", delivery, served, err)
		}
		accepted := requireFakeSend(t, f.tc, "successor's answer", f.sender(f.New).answer(t, f.tc, f.fr.RunID, question, "", "fresh-answer"), app.MessageAccepted, "")
		if seq := f.tc.Store.Messages[accepted.MessageID].EnqueueSeq; seq != 1 {
			t.Fatalf("successor's answer = seq %d; want the manager's first message", seq)
		}
		requireFakeSend(t, f.tc, "successor's info", infoToManager(t, f, f.New), app.MessageAccepted, "")
	})

	t.Run("an interrupted attempt whose session is stopping and bound", func(t *testing.T) {
		f := newFakeLineage(t)
		question := f.queueQuestion(t)
		if _, served, err := f.fetch(f.Old); err != nil || !served {
			t.Fatalf("fetch while current = %t, %v", served, err)
		}
		f.tc.Store.Attempts[f.Old.Attempt].value.State = run.AttemptInterrupted
		f.tc.Store.Sessions[f.Old.Session].value.State = run.SessionStopping
		requireFakeSendStale(t, f.tc, "stopping session's answer", f.sender(f.Old).answer(t, f.tc, f.fr.RunID, question, "", "late-answer"), storevectors.MessageSendSupersededAttemptDetail)
		if n := fakeAnswersTo(f.tc, question); n != 0 {
			t.Fatalf("answers after the stopping session's answer = %d, want 0", n)
		}
	})

	t.Run("a terminated session on a live, newest attempt", func(t *testing.T) {
		f := newFakeLineage(t)
		f.tc.Store.Sessions[f.Old.Session].value.State = run.SessionTerminated
		requireFakeSendStale(t, f.tc, "terminated session's info", infoToManager(t, f, f.Old), storevectors.MessageSendSupersededAttemptDetail)
	})

	t.Run("receipt first, then the incarnation, then currency", func(t *testing.T) {
		f := newFakeLineage(t)
		question := f.queueQuestion(t)
		answer := f.sender(f.Old).answer(t, f.tc, f.fr.RunID, question, "s1-answer", "s1-body")
		accepted := requireFakeSend(t, f.tc, "answer while current", answer, app.MessageAccepted, "")
		f.retireAndRetry(t)

		if replay := requireFakeSend(t, f.tc, "the retired session's identical retry", answer, app.MessageDuplicate, ""); replay.MessageID != accepted.MessageID {
			t.Fatalf("duplicate names %s, want the accepted answer %s", replay.MessageID, accepted.MessageID)
		}
		requireFakeSend(t, f.tc, "the retired session reusing its request id with another body",
			f.sender(f.Old).answer(t, f.tc, f.fr.RunID, question, "s1-answer", "other-body"), app.MessageRefused, app.GrammarReasonConflicting)
		requireFakeSendStale(t, f.tc, "the retired session's fresh request id with the same body",
			f.sender(f.Old).answer(t, f.tc, f.fr.RunID, question, "s1-again", "s1-body"), storevectors.MessageSendSupersededAttemptDetail)
		other := infoToManager(t, f, f.Old)
		other.IncarnationID = identity.IncarnationID(mintMessageID(t, f.tc).String())
		if got := requireFakeSend(t, f.tc, "the retired session at another incarnation", other, app.MessageRefused, app.GrammarReasonStale); got.Detail != "incarnation is not current" {
			t.Fatalf("SendMessage() detail = %q, want the incarnation refusal first", got.Detail)
		}
	})

	t.Run("an ended manager beside its successor", func(t *testing.T) {
		f := newFakeLineage(t)
		successor := endFakeManagerWithSuccessor(t, f.tc, f.fr)
		requireFakeSendStale(t, f.tc, "ended manager's info",
			storevectors.MessageSendEndedManager(f.fr.RunID, f.fr.ManagerID, f.fr.ManagerIncarnation, mintMessageID(t, f.tc), f.Task, "/state/i.md", "ended-info", 4),
			storevectors.MessageSendEndedManagerDetail)
		requireFakeSend(t, f.tc, "successor manager's info",
			storevectors.MessageSendEndedManager(f.fr.RunID, successor.Session, successor.Incarnation, mintMessageID(t, f.tc), f.Task, "/state/i.md", "successor-info", 4),
			app.MessageAccepted, "")
	})
}
