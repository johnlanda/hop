package sqlite_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// mailboxFixture is a feature fixture with an implementer whose attempt is
// running, so both the acceptance and settlement transactions have a live
// submission target.
type mailboxFixture struct {
	*featureFixture
	TaskB             identity.TaskID
	AttemptB          identity.AttemptID
	WorkerID          identity.SessionID
	WorkerIncarnation identity.IncarnationID
}

func newMailboxFixtureAt(t *testing.T, store *sqlite.Store, clock *fakeClock) *mailboxFixture {
	t.Helper()
	spec := newSpec("/repos/mailbox", specStride, clock.Now())
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
	taskB := f.createFeatureTask(t, 7801, 2, run.TaskActive)
	workerID, workerIncarnation := f.createWorkerSession(t, taskB, run.RoleImplementer, 7802)
	attemptB := identity.AttemptID(uid(7802))
	now := clock.Now()
	f.inUOW(t, func(uow app.UnitOfWork) {
		saveAttempt(t, uow, attemptB, func(v run.Attempt) (run.Attempt, error) { return v.Launch(now) })
		saveAttempt(t, uow, attemptB, func(v run.Attempt) (run.Attempt, error) { return v.MarkRunning(now) })
	})
	return &mailboxFixture{featureFixture: f, TaskB: taskB, AttemptB: attemptB, WorkerID: workerID, WorkerIncarnation: workerIncarnation}
}

// managerTaskSend builds a manager→task question.
func (f *mailboxFixture) managerTaskSend(n int) app.MessageSend {
	return app.MessageSend{
		ID: identity.MessageID(uid(n)), RunID: f.spec.RunID,
		Sender: run.SessionPrincipal(f.ManagerID), SenderAddress: run.ManagerAddress(),
		IncarnationID: f.ManagerIncarnation, Recipient: run.TaskAddress(f.TaskB), Kind: run.MessageQuestion,
		BodyPath: "/state/bodies/" + uid(n) + ".md", BodyDigest: "digest-" + uid(n), BodyBytes: 4,
	}
}

// resultFor builds a submission for the implementer attempt.
func (f *mailboxFixture) resultFor(n int) app.ResultSubmission {
	return app.ResultSubmission{
		ID: identity.ResultID(uid(n)), RunID: f.spec.RunID, TaskID: f.TaskB, AttemptID: f.AttemptB,
		IncarnationID: f.WorkerIncarnation, CommitOID: strings.Repeat("b", 40),
		Summary: "implemented", Digest: "digest-" + uid(n),
	}
}

// TestMailboxSendAcceptRaceBothOrders drives the send/accept race in both
// commit orders from separate handles: a send that lands first forces the
// acceptance transient (the worker drains and resubmits); the acceptance
// landing first closes the mailbox and the late send is refused.
func TestMailboxSendAcceptRaceBothOrders(t *testing.T) {
	clock := newFakeClock()
	root := t.TempDir()
	storeA := openStoreAt(t, root, clock)
	storeB := openStoreAt(t, root, clock)
	f := newMailboxFixtureAt(t, storeA, clock)

	// Order 1: the send commits first — the acceptance is refused
	// transient until the worker drains.
	send := f.managerTaskSend(7811)
	if outcome, err := storeB.SendMessage(t.Context(), send); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("send before acceptance = %+v, %v", outcome, err)
	}
	transient, err := storeA.SubmitResult(t.Context(), f.resultFor(7812))
	if err != nil || transient.Kind != app.SubmissionTransient || !strings.Contains(transient.Detail, "drain with hop msg next") {
		t.Fatalf("SubmitResult(pending mailbox) = %+v, %v; want the transient drain outcome", transient, err)
	}
	fetch := app.MessageFetch{RunID: f.spec.RunID, SessionID: f.WorkerID, IncarnationID: f.WorkerIncarnation, Address: run.TaskAddress(f.TaskB)}
	if _, served, fetchErr := storeA.FetchNextMessage(t.Context(), fetch); fetchErr != nil || !served {
		t.Fatalf("drain fetch = %t, %v", served, fetchErr)
	}
	if outcome, ackErr := storeA.AckMessage(t.Context(), app.MessageAck{RunID: f.spec.RunID, MessageID: send.ID, SessionID: f.WorkerID, IncarnationID: f.WorkerIncarnation}); ackErr != nil || outcome.Kind != app.AckAccepted {
		t.Fatalf("drain ack = %+v, %v", outcome, ackErr)
	}
	accepted, err := storeA.SubmitResult(t.Context(), f.resultFor(7812))
	if err != nil || accepted.Kind != app.SubmissionAccepted {
		t.Fatalf("resubmit after drain = %+v, %v; want accepted", accepted, err)
	}

	// Order 2: the acceptance closed the mailbox in the same commit — a
	// late send is refused, with a receipt, and no envelope lands.
	late, err := storeB.SendMessage(t.Context(), f.managerTaskSend(7813))
	if err != nil || late.Kind != app.MessageMailboxClose {
		t.Fatalf("send after acceptance = %+v, %v; want refused-mailbox-closed", late, err)
	}
	if n := countRows(t, storeA, `SELECT COUNT(*) FROM messages WHERE id = ?`, uid(7813)); n != 0 {
		t.Fatalf("refused send left an envelope; want none")
	}
}

// TestMailboxSendFailureSettlementRace drives the failure-closure
// snapshot-equality contract from separate handles, in both orders: a
// send committed BEFORE the settlement's snapshot preparation appears in
// the prepared set directly, and a send committed AFTER the preparation
// but BEFORE the first settlement transaction forces that transaction to
// retry on the snapshot mismatch — the eventual committed notice includes
// the raced message id, and the mailbox closes exactly once.
func TestMailboxSendFailureSettlementRace(t *testing.T) {
	clock := newFakeClock()
	root := t.TempDir()
	storeA := openStoreAt(t, root, clock)
	storeB := openStoreAt(t, root, clock)
	f := newMailboxFixtureAt(t, storeA, clock)

	// Order 1: a message committed before any settlement work begins.
	early := f.managerTaskSend(7821)
	if outcome, err := storeB.SendMessage(t.Context(), early); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("early send = %+v, %v", outcome, err)
	}

	// The settlement loop the application runs (usecase_featuresettle):
	// prepare the obligation snapshot, write the notice content outside
	// any transaction, then re-read the pending set INSIDE the settlement
	// transaction and retry from preparation on any mismatch.
	prepare := func() []identity.MessageID {
		uow, err := storeA.Begin(t.Context(), f.lease)
		if err != nil {
			t.Fatalf("Begin(prepare): %v", err)
		}
		defer uow.Rollback() //nolint:errcheck // read-only preparation.
		wf := workflowRepos(t, uow)
		pending, err := wf.Messages().PendingByAddress(t.Context(), f.spec.RunID, run.TaskAddress(f.TaskB))
		if err != nil {
			t.Fatalf("prepare snapshot: %v", err)
		}
		return pending
	}
	settle := func(snapshot []identity.MessageID, noticeN int) (committed bool) {
		uow, err := storeA.Begin(t.Context(), f.lease)
		if err != nil {
			t.Fatalf("Begin(settle): %v", err)
		}
		defer uow.Rollback() //nolint:errcheck // rollback after commit is a documented no-op.
		wf := workflowRepos(t, uow)
		pending, err := wf.Messages().PendingByAddress(t.Context(), f.spec.RunID, run.TaskAddress(f.TaskB))
		if err != nil {
			t.Fatalf("re-read pending set: %v", err)
		}
		if !slices.Equal(pending, snapshot) {
			// The obligation set moved between preparation and the
			// transaction: the settlement retries from preparation.
			return false
		}
		task, revision, err := uow.Tasks().Get(t.Context(), f.TaskB)
		if err != nil {
			t.Fatalf("load task: %v", err)
		}
		failed, err := task.Fail(f.clock.Now())
		if err != nil {
			t.Fatalf("fail task: %v", err)
		}
		failed = failed.CloseMailbox(f.clock.Now())
		if _, err := uow.Tasks().Save(t.Context(), failed, revision); err != nil {
			t.Fatalf("save failed task: %v", err)
		}
		ids := make([]string, len(snapshot))
		for i, id := range snapshot {
			ids[i] = id.String()
		}
		notice := run.NewInfo(identity.MessageID(uid(noticeN)), f.spec.RunID, run.ControllerPrincipal(), run.ManagerAddress(),
			"", "/state/notices/orphaned-"+strings.Join(ids, "+")+".md", "notice-digest", 0, 0, f.clock.Now())
		if _, err := wf.Messages().Create(t.Context(), notice); err != nil {
			t.Fatalf("create notice: %v", err)
		}
		if err := uow.Commit(); err != nil {
			t.Fatalf("commit settlement: %v", err)
		}
		return true
	}

	// Order 2's window: the snapshot is prepared, THEN a send commits from
	// the other handle before the settlement transaction begins.
	snapshot := prepare()
	if len(snapshot) != 1 || snapshot[0] != early.ID {
		t.Fatalf("prepared snapshot = %v, want the early message alone", snapshot)
	}
	raced := f.managerTaskSend(7822)
	if outcome, err := storeB.SendMessage(t.Context(), raced); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("raced send = %+v, %v; the mailbox is still open", outcome, err)
	}

	if settle(snapshot, 7823) {
		t.Fatal("the first settlement committed against a stale snapshot; the re-read must force a retry")
	}
	// The retry re-prepares and commits with the raced message included.
	snapshot = prepare()
	if len(snapshot) != 2 {
		t.Fatalf("re-prepared snapshot = %v, want both pending messages", snapshot)
	}
	if !settle(snapshot, 7824) {
		t.Fatal("the second settlement did not commit against the fresh snapshot")
	}

	// The committed notice names the raced message id.
	var noticeBody string
	if err := sqlite.WriteDB(storeA).QueryRowContext(t.Context(),
		`SELECT body_path FROM messages WHERE id = ?`, uid(7824),
	).Scan(&noticeBody); err != nil {
		t.Fatalf("read committed notice: %v", err)
	}
	if !strings.Contains(noticeBody, raced.ID.String()) || !strings.Contains(noticeBody, early.ID.String()) {
		t.Fatalf("notice = %q, want both orphaned message ids incl. the raced one", noticeBody)
	}

	// The mailbox is durably closed: a further send is refused.
	f.inUOW(t, func(uow app.UnitOfWork) {
		task, _, err := uow.Tasks().Get(t.Context(), f.TaskB)
		if err != nil || !task.MailboxClosed || task.State != run.TaskFailed {
			t.Fatalf("task after settlement = %+v, %v; want failed with the mailbox closed", task, err)
		}
	})
	if outcome, err := storeB.SendMessage(t.Context(), f.managerTaskSend(7825)); err != nil || outcome.Kind != app.MessageMailboxClose {
		t.Fatalf("send after failure closure = %+v, %v; want refused-mailbox-closed", outcome, err)
	}
}
