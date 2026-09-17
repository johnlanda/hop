package sqlite_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
	"github.com/johnlanda/hop/internal/testsupport/storevectors"
)

// taskLineageFixture is one implement task whose attempt 1 runs behind a
// bound, active session; retireAndRetry settles that attempt the way the
// live controller's worker termination does and runs the retry's attempt 2
// behind its own bound session.
type taskLineageFixture struct {
	*featureFixture
	Task                           identity.TaskID
	OldAttempt, NewAttempt         identity.AttemptID
	OldSession, NewSession         identity.SessionID
	OldIncarnation, NewIncarnation identity.IncarnationID
}

func newTaskLineageFixture(t *testing.T) *taskLineageFixture {
	t.Helper()
	f := newFeatureFixture(t)
	task := f.createFeatureTask(t, 9401, 2, run.TaskActive)
	session, incarnation := f.createWorkerSession(t, task, run.RoleImplementer, 9402)
	attempt := identity.AttemptID(uid(9402))
	now := f.clock.Now()
	f.inUOW(t, func(uow app.UnitOfWork) {
		saveAttempt(t, uow, attempt, func(v run.Attempt) (run.Attempt, error) { return v.Launch(now) })
		saveAttempt(t, uow, attempt, func(v run.Attempt) (run.Attempt, error) { return v.MarkRunning(now) })
	})
	return &taskLineageFixture{featureFixture: f, Task: task, OldAttempt: attempt, OldSession: session, OldIncarnation: incarnation}
}

// interruptOldAttempt interrupts attempt 1 and moves the task to
// needs-rework, leaving its session and binding untouched.
func (f *taskLineageFixture) interruptOldAttempt(t *testing.T) {
	t.Helper()
	now := f.clock.Now()
	f.inUOW(t, func(uow app.UnitOfWork) {
		saveAttempt(t, uow, f.OldAttempt, func(v run.Attempt) (run.Attempt, error) { return v.Interrupt(now) })
		saveTask(t, uow, f.Task, func(v run.Task) (run.Task, error) { return v.NeedsRework(now) })
	})
}

// terminateOldSession stops and terminates attempt 1's session without
// superseding its binding (terminateSession's shape).
func (f *taskLineageFixture) terminateOldSession(t *testing.T) {
	t.Helper()
	now := f.clock.Now()
	f.inUOW(t, func(uow app.UnitOfWork) {
		saveSession(t, uow, f.OldSession, func(v run.Session) (run.Session, error) { return v.Stop(now) })
		saveSession(t, uow, f.OldSession, func(v run.Session) (run.Session, error) { return v.Terminate(now) })
	})
}

// retireAndRetry is the whole worker-termination settlement followed by a
// consumed retry: attempt 2 running behind its own bound session.
func (f *taskLineageFixture) retireAndRetry(t *testing.T) {
	t.Helper()
	f.interruptOldAttempt(t)
	f.terminateOldSession(t)
	now := f.clock.Now()
	f.inUOW(t, func(uow app.UnitOfWork) {
		saveTask(t, uow, f.Task, func(v run.Task) (run.Task, error) { return v.Reopen(now) })
		saveTask(t, uow, f.Task, func(v run.Task) (run.Task, error) { return v.Activate(now) })
	})
	f.NewSession, f.NewIncarnation = f.createWorkerSession(t, f.Task, run.RoleImplementer, 9410)
	f.NewAttempt = identity.AttemptID(uid(9410))
	f.inUOW(t, func(uow app.UnitOfWork) {
		saveAttempt(t, uow, f.NewAttempt, func(v run.Attempt) (run.Attempt, error) { return v.Launch(now) })
		saveAttempt(t, uow, f.NewAttempt, func(v run.Attempt) (run.Attempt, error) { return v.MarkRunning(now) })
	})
}

func (f *taskLineageFixture) fetch(session identity.SessionID, incarnation identity.IncarnationID) app.MessageFetch {
	return app.MessageFetch{RunID: f.spec.RunID, SessionID: session, IncarnationID: incarnation, Address: run.TaskAddress(f.Task)}
}

func (f *taskLineageFixture) ack(message identity.MessageID, session identity.SessionID, incarnation identity.IncarnationID) app.MessageAck {
	return app.MessageAck{RunID: f.spec.RunID, MessageID: message, SessionID: session, IncarnationID: incarnation}
}

// lastMessageReceipt reads the newest message_receipts row of op.
func lastMessageReceipt(t *testing.T, store *sqlite.Store, op string) (outcome, detail string) {
	t.Helper()
	if err := sqlite.WriteDB(store).QueryRowContext(t.Context(),
		`SELECT outcome, detail FROM message_receipts WHERE op = ? ORDER BY at DESC, rowid DESC LIMIT 1`, op,
	).Scan(&outcome, &detail); err != nil {
		t.Fatalf("read last %s receipt: %v", op, err)
	}
	return outcome, detail
}

// ptr returns a pointer to a copy of v.
func ptr[T any](v T) *T { return &v }

// requireFetchRefused asserts a fetch refused as not the address's current
// session: the unauthorized error with the value-free detail, its refusal
// receipt, and no delivery row written.
func requireFetchRefused(t *testing.T, store *sqlite.Store, fetch *app.MessageFetch, detail string) {
	t.Helper()
	before := countRows(t, store, `SELECT COUNT(*) FROM message_deliveries`)
	delivery, served, err := store.FetchNextMessage(t.Context(), *fetch)
	if served || delivery.Message.ID != "" || !errors.Is(err, app.ErrMessagingUnauthorized) || !strings.Contains(err.Error(), detail) {
		t.Fatalf("FetchNextMessage = %+v, %t, %v; want ErrMessagingUnauthorized %q and nothing served", delivery, served, err, detail)
	}
	if strings.Contains(err.Error(), fetch.SessionID.String()) || strings.Contains(err.Error(), fetch.IncarnationID.String()) {
		t.Fatalf("refusal %q echoes the caller's session or incarnation", err)
	}
	if outcome, got := lastMessageReceipt(t, store, "msg-fetch"); outcome != string(app.MessageRefused) || got != detail {
		t.Fatalf("fetch receipt = %s %q, want refused %q", outcome, got, detail)
	}
	if after := countRows(t, store, `SELECT COUNT(*) FROM message_deliveries`); after != before {
		t.Fatalf("a refused fetch wrote %d delivery rows", after-before)
	}
}

// TestFetchRequiresTheTaskCurrentAttemptSession pins section 7's fetch
// authority on the real store: only the current session of the task's
// newest, non-terminal attempt is served the task address. A retired
// attempt's session — terminated with its binding still current, or still
// active on an interrupted attempt, or ended on an attempt that is still
// live, or live on an attempt that is no longer the newest — is refused
// with a receipt and nothing served.
func TestFetchRequiresTheTaskCurrentAttemptSession(t *testing.T) {
	t.Run("a retired attempt's session after a retry; the successor is served", func(t *testing.T) {
		f := newTaskLineageFixture(t)
		f.retireAndRetry(t)
		message := queueTaskInfo(t, f.featureFixture, f.Task, 9420)

		requireFetchRefused(t, f.store, ptr(storevectors.MessageFetchSupersededAttempt(f.spec.RunID, f.OldSession, f.OldIncarnation, f.Task)), storevectors.MessageFetchSupersededAttemptDetail)
		delivery, served, err := f.store.FetchNextMessage(t.Context(), f.fetch(f.NewSession, f.NewIncarnation))
		if err != nil || !served || delivery.Message.ID != message {
			t.Fatalf("successor fetch = %+v, %t, %v; want the queued message", delivery, served, err)
		}
		if n := countRows(t, f.store, `SELECT COUNT(*) FROM message_deliveries WHERE message_id = ?`, message.String()); n != 1 {
			t.Fatalf("delivery rows = %d, want only the successor's", n)
		}
		if ack, err := f.store.AckMessage(t.Context(), f.ack(message, f.NewSession, f.NewIncarnation)); err != nil || ack.Kind != app.AckAccepted {
			t.Fatalf("successor ack = %+v, %v; want accepted", ack, err)
		}
	})

	t.Run("an interrupted attempt whose session is still active and bound", func(t *testing.T) {
		f := newTaskLineageFixture(t)
		f.interruptOldAttempt(t)
		queueTaskInfo(t, f.featureFixture, f.Task, 9430)
		requireFetchRefused(t, f.store, ptr(f.fetch(f.OldSession, f.OldIncarnation)), storevectors.MessageFetchSupersededAttemptDetail)
	})

	t.Run("a terminated session on a live, newest attempt", func(t *testing.T) {
		f := newTaskLineageFixture(t)
		f.terminateOldSession(t)
		queueTaskInfo(t, f.featureFixture, f.Task, 9440)
		requireFetchRefused(t, f.store, ptr(f.fetch(f.OldSession, f.OldIncarnation)), storevectors.MessageFetchSupersededAttemptDetail)
	})

	t.Run("a live session on an attempt that is not its task's newest", func(t *testing.T) {
		f := newTaskLineageFixture(t)
		// A higher-numbered attempt of the same task, recorded terminal (the
		// only shape beside a live attempt the one-active-attempt index
		// admits): the store's own numbering decides which attempt is newest.
		now := f.clock.Now()
		f.inUOW(t, func(uow app.UnitOfWork) {
			newer, err := run.NewAttempt(identity.AttemptID(uid(9450)), f.Task, 2, now)
			if err != nil {
				t.Fatalf("new attempt: %v", err)
			}
			if newer, err = newer.Interrupt(now); err != nil {
				t.Fatalf("interrupt attempt: %v", err)
			}
			if _, err := workflowRepos(t, uow).AttemptIndex().Create(t.Context(), newer); err != nil {
				t.Fatalf("create attempt 2: %v", err)
			}
		})
		queueTaskInfo(t, f.featureFixture, f.Task, 9451)
		requireFetchRefused(t, f.store, ptr(f.fetch(f.OldSession, f.OldIncarnation)), storevectors.MessageFetchSupersededAttemptDetail)
	})

	t.Run("the current session is served", func(t *testing.T) {
		f := newTaskLineageFixture(t)
		message := queueTaskInfo(t, f.featureFixture, f.Task, 9460)
		if delivery, served, err := f.store.FetchNextMessage(t.Context(), f.fetch(f.OldSession, f.OldIncarnation)); err != nil || !served || delivery.Message.ID != message {
			t.Fatalf("current fetch = %+v, %t, %v; want the queued message", delivery, served, err)
		}
	})
}

// TestAckRequiresTheTaskCurrentAttemptSession pins section 7's ack order on
// the real store: a retired attempt's session acking a message it was
// served while current is refused stale with a receipt and no ack row, so
// the message stays in flight and is re-served to the successor, whose own
// ack is accepted; acking a message it was never served is not-delivered;
// and once the message is acknowledged, its repeat is an idempotent
// duplicate whoever sends it.
func TestAckRequiresTheTaskCurrentAttemptSession(t *testing.T) {
	f := newTaskLineageFixture(t)
	first := queueTaskInfo(t, f.featureFixture, f.Task, 9470)
	second := queueTaskInfo(t, f.featureFixture, f.Task, 9471)
	if delivery, served, err := f.store.FetchNextMessage(t.Context(), f.fetch(f.OldSession, f.OldIncarnation)); err != nil || !served || delivery.Message.ID != first {
		t.Fatalf("fetch while current = %+v, %t, %v; want the first message", delivery, served, err)
	}
	f.retireAndRetry(t)

	stale, err := f.store.AckMessage(t.Context(), storevectors.AckMessageSupersededAttempt(f.spec.RunID, first, f.OldSession, f.OldIncarnation))
	if err != nil || stale.Kind != app.AckRefused || stale.Reason != storevectors.AckMessageSupersededAttemptReason {
		t.Fatalf("retired session's ack = %+v, %v; want refused %s", stale, err, storevectors.AckMessageSupersededAttemptReason)
	}
	if strings.Contains(stale.Detail, f.OldSession.String()) || strings.Contains(stale.Detail, f.OldIncarnation.String()) {
		t.Fatalf("stale detail %q echoes the caller's session or incarnation", stale.Detail)
	}
	if outcome, _ := lastMessageReceipt(t, f.store, "msg-ack"); outcome != string(app.AckRefused) {
		t.Fatalf("ack receipt = %s, want refused", outcome)
	}
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM message_acks`); n != 0 {
		t.Fatalf("ack rows after a stale ack = %d, want 0", n)
	}
	notServed, err := f.store.AckMessage(t.Context(), f.ack(second, f.OldSession, f.OldIncarnation))
	if err != nil || notServed.Kind != app.AckRefused || notServed.Reason != app.GrammarReasonNotDelivered {
		t.Fatalf("retired session's ack of an unserved message = %+v, %v; want refused not-delivered", notServed, err)
	}

	delivery, served, err := f.store.FetchNextMessage(t.Context(), f.fetch(f.NewSession, f.NewIncarnation))
	if err != nil || !served || delivery.Message.ID != first {
		t.Fatalf("successor fetch = %+v, %t, %v; want the in-flight first message re-served", delivery, served, err)
	}
	if ack, err := f.store.AckMessage(t.Context(), f.ack(first, f.NewSession, f.NewIncarnation)); err != nil || ack.Kind != app.AckAccepted {
		t.Fatalf("successor ack = %+v, %v; want accepted", ack, err)
	}
	var ackedBy string
	if err := sqlite.WriteDB(f.store).QueryRowContext(t.Context(), `SELECT session_id FROM message_acks WHERE message_id = ?`, first.String()).Scan(&ackedBy); err != nil || ackedBy != f.NewSession.String() {
		t.Fatalf("first message acked by %q (%v), want the successor %s", ackedBy, err, f.NewSession)
	}
	if dup, err := f.store.AckMessage(t.Context(), f.ack(first, f.OldSession, f.OldIncarnation)); err != nil || dup.Kind != app.AckDuplicate {
		t.Fatalf("repeat ack from the retired session = %+v, %v; want duplicate", dup, err)
	}
	if delivery, served, err := f.store.FetchNextMessage(t.Context(), f.fetch(f.NewSession, f.NewIncarnation)); err != nil || !served || delivery.Message.ID != second {
		t.Fatalf("successor's next fetch = %+v, %t, %v; want the second message", delivery, served, err)
	}
}

// endManagerWithSuccessor terminates the fixture manager with its binding
// still current and creates an active, bound successor manager session
// (identities n and n+1, label n+2), returning the successor.
func endManagerWithSuccessor(t *testing.T, f *featureFixture, n int) (identity.SessionID, identity.IncarnationID) {
	t.Helper()
	successor := identity.SessionID(uid(n))
	successorIncarnation := identity.IncarnationID(uid(n + 1))
	now := f.clock.Now()
	f.inUOW(t, func(uow app.UnitOfWork) {
		saveSession(t, uow, f.ManagerID, func(v run.Session) (run.Session, error) { return v.Stop(now) })
		saveSession(t, uow, f.ManagerID, func(v run.Session) (run.Session, error) { return v.Terminate(now) })
		manager := run.NewManagerSession(successor, f.spec.RunID, run.HarnessClaude, now)
		manager, err := manager.Launch(now)
		if err != nil {
			t.Fatalf("launch successor manager: %v", err)
		}
		if manager, err = manager.ConfirmActive(now); err != nil {
			t.Fatalf("activate successor manager: %v", err)
		}
		if _, err := uow.Sessions().Create(t.Context(), manager); err != nil {
			t.Fatalf("create successor manager: %v", err)
		}
		binding := run.NewRuntimeBinding(successor, successorIncarnation, "/tmp/herdr.sock", "server-instance-1", "ws-m", "tab-m", "pane-"+uid(n+2), uid(n+2), run.LaunchResume, now)
		if err := uow.Bindings().Create(t.Context(), binding); err != nil {
			t.Fatalf("create successor binding: %v", err)
		}
	})
	return successor, successorIncarnation
}

// TestManagerMessagingRequiresAManagerSessionThatHasNotEnded pins the same
// authority on the manager address: a manager session that has ended while
// its binding is still current is neither served nor able to ack a
// message it was served, so it cannot consume its successor's queue; the
// successor manager is re-served and acks.
func TestManagerMessagingRequiresAManagerSessionThatHasNotEnded(t *testing.T) {
	f := newMessagingFixture(t)
	first := f.workerSend(9480, run.MessageQuestion, "")
	second := f.workerSend(9481, run.MessageInfo, "")
	for _, send := range []app.MessageSend{first, second} {
		if outcome, err := f.store.SendMessage(t.Context(), send); err != nil || outcome.Kind != app.MessageAccepted {
			t.Fatalf("seed worker message: %+v, %v", outcome, err)
		}
	}
	if delivery, served, err := f.store.FetchNextMessage(t.Context(), f.managerFetch()); err != nil || !served || delivery.Message.ID != first.ID {
		t.Fatalf("manager fetch while current = %+v, %t, %v", delivery, served, err)
	}

	successor, successorIncarnation := endManagerWithSuccessor(t, f.featureFixture, 9482)

	requireFetchRefused(t, f.store, ptr(storevectors.MessageFetchEndedManager(f.spec.RunID, f.ManagerID, f.ManagerIncarnation)), storevectors.MessageFetchEndedManagerDetail)
	stale, err := f.store.AckMessage(t.Context(), storevectors.AckMessageEndedManager(f.spec.RunID, first.ID, f.ManagerID, f.ManagerIncarnation))
	if err != nil || stale.Kind != app.AckRefused || stale.Reason != storevectors.AckMessageEndedManagerReason {
		t.Fatalf("ended manager's ack = %+v, %v; want refused %s", stale, err, storevectors.AckMessageEndedManagerReason)
	}

	fetch := app.MessageFetch{RunID: f.spec.RunID, SessionID: successor, IncarnationID: successorIncarnation, Address: run.ManagerAddress()}
	if delivery, served, err := f.store.FetchNextMessage(t.Context(), fetch); err != nil || !served || delivery.Message.ID != first.ID {
		t.Fatalf("successor manager fetch = %+v, %t, %v; want the in-flight message re-served", delivery, served, err)
	}
	if ack, err := f.store.AckMessage(t.Context(), app.MessageAck{RunID: f.spec.RunID, MessageID: first.ID, SessionID: successor, IncarnationID: successorIncarnation}); err != nil || ack.Kind != app.AckAccepted {
		t.Fatalf("successor manager ack = %+v, %v; want accepted", ack, err)
	}
	if delivery, served, err := f.store.FetchNextMessage(t.Context(), fetch); err != nil || !served || delivery.Message.ID != second.ID {
		t.Fatalf("successor manager's next fetch = %+v, %t, %v; want the second message", delivery, served, err)
	}
}

// queueTaskQuestion sends task one manager question and requires its
// acceptance.
func queueTaskQuestion(t *testing.T, f *featureFixture, task identity.TaskID, n int) identity.MessageID {
	t.Helper()
	send := app.MessageSend{
		ID: identity.MessageID(uid(n)), RunID: f.spec.RunID,
		Sender: run.SessionPrincipal(f.ManagerID), SenderAddress: run.ManagerAddress(),
		IncarnationID: f.ManagerIncarnation, Recipient: run.TaskAddress(task), Kind: run.MessageQuestion,
		BodyPath: "/state/bodies/" + uid(n) + ".md", BodyDigest: "digest-" + uid(n), BodyBytes: 4,
	}
	requireSendOutcome(t, f.store, "manager question to the task", send, app.MessageAccepted, "")
	return send.ID
}

// requireSendStale sends one request that must be refused as not its
// address's current session: stale with the value-free detail, a refused
// receipt carrying it, and no envelope.
func requireSendStale(t *testing.T, store *sqlite.Store, label string, send app.MessageSend, detail string) { //nolint:gocritic // hugeParam: the port passes the send value; the helper mirrors it.
	t.Helper()
	got := requireSendOutcome(t, store, label, send, app.MessageRefused, app.GrammarReasonStale)
	if got.Detail != detail || got.MessageID != "" {
		t.Fatalf("%s: SendMessage() = %+v, want detail %q naming no message", label, got, detail)
	}
	if strings.Contains(got.Detail, send.Sender.SessionID.String()) || strings.Contains(got.Detail, send.IncarnationID.String()) {
		t.Fatalf("%s: detail %q echoes the caller's session or incarnation", label, got.Detail)
	}
	if outcome, recorded := lastMessageReceipt(t, store, "msg-send"); outcome != string(app.MessageRefused) || recorded != detail {
		t.Fatalf("%s: send receipt = %s %q, want refused %q", label, outcome, recorded, detail)
	}
	if n := countRows(t, store, `SELECT COUNT(*) FROM messages WHERE id = ?`, send.ID.String()); n != 0 {
		t.Fatalf("%s: the refused send created an envelope", label)
	}
}

// TestSendRequiresTheAddressCurrentSession pins section 7's one currency
// rule for sends on the real store: a session that is no longer its
// address's current session sends nothing — no answer, question or info —
// and is refused stale with a value-free detail, after the request-ID
// receipt (its own accepted request still replays as duplicate) and after
// the incarnation; the current session is re-served the question and its
// answer is accepted.
func TestSendRequiresTheAddressCurrentSession(t *testing.T) {
	infoToManager := func(f *taskLineageFixture, n int, session identity.SessionID, incarnation identity.IncarnationID) app.MessageSend {
		return app.MessageSend{
			ID: identity.MessageID(uid(n)), RunID: f.spec.RunID,
			Sender: run.SessionPrincipal(session), SenderAddress: run.TaskAddress(f.Task),
			IncarnationID: incarnation, Recipient: run.ManagerAddress(), Kind: run.MessageInfo,
			BodyPath: "/state/bodies/" + uid(n) + ".md", BodyDigest: "digest-" + uid(n), BodyBytes: 4,
		}
	}

	t.Run("a retired attempt's session after a retry; the successor answers", func(t *testing.T) {
		f := newTaskLineageFixture(t)
		question := queueTaskQuestion(t, f.featureFixture, f.Task, 9901)
		if delivery, served, err := f.store.FetchNextMessage(t.Context(), f.fetch(f.OldSession, f.OldIncarnation)); err != nil || !served || delivery.Message.ID != question {
			t.Fatalf("fetch while current = %+v, %t, %v; want the question", delivery, served, err)
		}
		f.retireAndRetry(t)

		requireSendStale(t, f.store, "retired session's answer",
			storevectors.MessageSendSupersededAttempt(f.spec.RunID, f.OldSession, f.Task, f.OldIncarnation, identity.MessageID(uid(9902)), question, "/state/a.md", "stale-answer", 5),
			storevectors.MessageSendSupersededAttemptDetail)
		stale := infoToManager(f, 9903, f.OldSession, f.OldIncarnation)
		requireSendStale(t, f.store, "retired session's info", stale, storevectors.MessageSendSupersededAttemptDetail)
		stale.Kind, stale.ID = run.MessageQuestion, identity.MessageID(uid(9904))
		requireSendStale(t, f.store, "retired session's question", stale, storevectors.MessageSendSupersededAttemptDetail)
		requireNoAnswer(t, f.store, "after the retired session's answer", question)

		if delivery, served, err := f.store.FetchNextMessage(t.Context(), f.fetch(f.NewSession, f.NewIncarnation)); err != nil || !served || delivery.Message.ID != question {
			t.Fatalf("successor fetch = %+v, %t, %v; want the question re-served", delivery, served, err)
		}
		accepted := requireSendOutcome(t, f.store, "successor's answer",
			sessionAnswer(f.spec.RunID, 9905, f.NewSession, run.TaskAddress(f.Task), f.NewIncarnation, question, "", "fresh-answer"),
			app.MessageAccepted, "")
		var seq int
		if err := sqlite.WriteDB(f.store).QueryRowContext(t.Context(), `SELECT enqueue_seq FROM messages WHERE id = ? AND recipient_address = 'manager'`, accepted.MessageID.String()).Scan(&seq); err != nil || seq != 1 {
			t.Fatalf("successor's answer = seq %d, %v; want the manager's first message", seq, err)
		}
		requireSendOutcome(t, f.store, "successor's info", infoToManager(f, 9906, f.NewSession, f.NewIncarnation), app.MessageAccepted, "")
	})

	t.Run("an interrupted attempt whose session is stopping and bound", func(t *testing.T) {
		f := newTaskLineageFixture(t)
		question := queueTaskQuestion(t, f.featureFixture, f.Task, 9911)
		if _, served, err := f.store.FetchNextMessage(t.Context(), f.fetch(f.OldSession, f.OldIncarnation)); err != nil || !served {
			t.Fatalf("fetch while current = %t, %v", served, err)
		}
		now := f.clock.Now()
		f.inUOW(t, func(uow app.UnitOfWork) {
			saveAttempt(t, uow, f.OldAttempt, func(v run.Attempt) (run.Attempt, error) { return v.Interrupt(now) })
			saveSession(t, uow, f.OldSession, func(v run.Session) (run.Session, error) { return v.Stop(now) })
		})
		requireSendStale(t, f.store, "stopping session's answer",
			sessionAnswer(f.spec.RunID, 9912, f.OldSession, run.TaskAddress(f.Task), f.OldIncarnation, question, "", "late-answer"),
			storevectors.MessageSendSupersededAttemptDetail)
		requireNoAnswer(t, f.store, "after the stopping session's answer", question)
	})

	t.Run("a terminated session on a live, newest attempt", func(t *testing.T) {
		f := newTaskLineageFixture(t)
		f.terminateOldSession(t)
		requireSendStale(t, f.store, "terminated session's info", infoToManager(f, 9921, f.OldSession, f.OldIncarnation), storevectors.MessageSendSupersededAttemptDetail)
	})

	t.Run("receipt first, then the incarnation, then currency", func(t *testing.T) {
		f := newTaskLineageFixture(t)
		question := queueTaskQuestion(t, f.featureFixture, f.Task, 9931)
		answer := sessionAnswer(f.spec.RunID, 9932, f.OldSession, run.TaskAddress(f.Task), f.OldIncarnation, question, "s1-answer", "s1-body")
		accepted := requireSendOutcome(t, f.store, "answer while current", answer, app.MessageAccepted, "")
		f.retireAndRetry(t)

		if replay := requireSendOutcome(t, f.store, "the retired session's identical retry", answer, app.MessageDuplicate, ""); replay.MessageID != accepted.MessageID {
			t.Fatalf("duplicate names %s, want the accepted answer %s", replay.MessageID, accepted.MessageID)
		}
		requireSendOutcome(t, f.store, "the retired session reusing its request id with another body",
			sessionAnswer(f.spec.RunID, 9933, f.OldSession, run.TaskAddress(f.Task), f.OldIncarnation, question, "s1-answer", "other-body"),
			app.MessageRefused, app.GrammarReasonConflicting)
		requireSendStale(t, f.store, "the retired session's fresh request id with the same body",
			sessionAnswer(f.spec.RunID, 9934, f.OldSession, run.TaskAddress(f.Task), f.OldIncarnation, question, "s1-again", "s1-body"),
			storevectors.MessageSendSupersededAttemptDetail)
		unknown := infoToManager(f, 9935, f.OldSession, identity.IncarnationID(uid(9936)))
		if got := requireSendOutcome(t, f.store, "the retired session at another incarnation", unknown, app.MessageRefused, app.GrammarReasonStale); got.Detail != "incarnation is not current" {
			t.Fatalf("SendMessage() detail = %q, want the incarnation refusal first", got.Detail)
		}
	})

	t.Run("an ended manager beside its successor", func(t *testing.T) {
		f := newTaskLineageFixture(t)
		successor, successorIncarnation := endManagerWithSuccessor(t, f.featureFixture, 9941)
		requireSendStale(t, f.store, "ended manager's info",
			storevectors.MessageSendEndedManager(f.spec.RunID, f.ManagerID, f.ManagerIncarnation, identity.MessageID(uid(9944)), f.Task, "/state/i.md", "ended-info", 4),
			storevectors.MessageSendEndedManagerDetail)
		requireSendOutcome(t, f.store, "successor manager's info",
			storevectors.MessageSendEndedManager(f.spec.RunID, successor, successorIncarnation, identity.MessageID(uid(9945)), f.Task, "/state/i.md", "successor-info", 4),
			app.MessageAccepted, "")
	})
}
