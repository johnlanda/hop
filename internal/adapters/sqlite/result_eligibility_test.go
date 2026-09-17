package sqlite_test

import (
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// queueTaskInfo sends task one manager info and requires its acceptance.
func queueTaskInfo(t *testing.T, f *featureFixture, task identity.TaskID, n int) identity.MessageID {
	t.Helper()
	send := app.MessageSend{
		ID: identity.MessageID(uid(n)), RunID: f.spec.RunID,
		Sender: run.SessionPrincipal(f.ManagerID), SenderAddress: run.ManagerAddress(),
		IncarnationID: f.ManagerIncarnation, Recipient: run.TaskAddress(task), Kind: run.MessageInfo,
		BodyPath: "/state/bodies/" + uid(n) + ".md", BodyDigest: "digest-" + uid(n), BodyBytes: 4,
	}
	if outcome, err := f.store.SendMessage(t.Context(), send); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("queue info to task %s: %+v, %v", task, outcome, err)
	}
	return send.ID
}

// supersedeWithoutSuccessor supersedes session's current binding with no
// successor.
func supersedeWithoutSuccessor(t *testing.T, f *featureFixture, session identity.SessionID) {
	t.Helper()
	f.inUOW(t, func(uow app.UnitOfWork) {
		binding, ok, err := uow.Bindings().Current(t.Context(), session)
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
}

// launchingWorker creates an implement task whose attempt is launching
// behind a bound, active session with an exec_pending launch claim: the
// early-submission window with the binding already recorded.
func launchingWorker(t *testing.T, f *featureFixture, n int) (task identity.TaskID, attempt identity.AttemptID, session identity.SessionID, incarnation identity.IncarnationID) {
	t.Helper()
	task = f.createFeatureTask(t, n, 3, run.TaskActive)
	session, incarnation = f.createWorkerSession(t, task, run.RoleImplementer, n+1)
	attempt = identity.AttemptID(uid(n + 1))
	now := f.clock.Now()
	f.inUOW(t, func(uow app.UnitOfWork) {
		saveAttempt(t, uow, attempt, func(v run.Attempt) (run.Attempt, error) { return v.Launch(now) })
	})
	if err := f.store.ClaimLaunch(t.Context(), claimFor(f, incarnation, session, attempt, 4242)); err != nil {
		t.Fatalf("ClaimLaunch() = %v", err)
	}
	return task, attempt, session, incarnation
}

// TestSubmitResultEligibilityBeforeMailbox pins AcceptVerdict's order for
// results on the real store. With a message queued to the task, a result
// from a caller that can never be accepted — a superseded incarnation, an
// incarnation a relaunch replaced, a run with a stop request, an attempt
// already terminal — is stale, and one from an attempt still launching
// with an unsettled claim is attempt-not-running. Only an otherwise
// eligible caller is told to drain. Each refusal leaves exactly its
// receipt and no result row, and the mailbox refusal keeps its detail.
func TestSubmitResultEligibilityBeforeMailbox(t *testing.T) {
	cases := []struct {
		name       string
		arrange    func(t *testing.T, f *mailboxFixture) app.ResultSubmission
		wantKind   app.SubmissionOutcomeKind
		wantReason app.TransientReason
	}{
		{
			name: "a superseded binding with no successor",
			arrange: func(t *testing.T, f *mailboxFixture) app.ResultSubmission {
				queueTaskInfo(t, f.featureFixture, f.TaskB, 8201)
				supersedeWithoutSuccessor(t, f.featureFixture, f.WorkerID)
				return f.resultFor(8202)
			},
			wantKind: app.SubmissionStale,
		},
		{
			name: "an old incarnation whose session a relaunch rebound",
			arrange: func(t *testing.T, f *mailboxFixture) app.ResultSubmission {
				queueTaskInfo(t, f.featureFixture, f.TaskB, 8211)
				supersedeWithoutSuccessor(t, f.featureFixture, f.WorkerID)
				f.inUOW(t, func(uow app.UnitOfWork) {
					next := run.NewRuntimeBinding(f.WorkerID, identity.IncarnationID(uid(8212)), "/tmp/herdr.sock", "server-instance-1", "ws", "tab", "pane-8213", uid(8213), run.LaunchResume, f.clock.Now())
					if err := uow.Bindings().Create(t.Context(), next); err != nil {
						t.Fatalf("create successor binding: %v", err)
					}
				})
				return f.resultFor(8214)
			},
			wantKind: app.SubmissionStale,
		},
		{
			name: "a run with a stop request",
			arrange: func(t *testing.T, f *mailboxFixture) app.ResultSubmission {
				queueTaskInfo(t, f.featureFixture, f.TaskB, 8221)
				if err := f.store.RequestStop(t.Context(), f.spec.RunID); err != nil {
					t.Fatalf("RequestStop() = %v", err)
				}
				return f.resultFor(8222)
			},
			wantKind: app.SubmissionStale,
		},
		{
			name: "an interrupted attempt whose session is still bound",
			arrange: func(t *testing.T, f *mailboxFixture) app.ResultSubmission {
				queueTaskInfo(t, f.featureFixture, f.TaskB, 8231)
				now := f.clock.Now()
				f.inUOW(t, func(uow app.UnitOfWork) {
					saveAttempt(t, uow, f.AttemptB, func(v run.Attempt) (run.Attempt, error) { return v.Interrupt(now) })
				})
				return f.resultFor(8232)
			},
			wantKind: app.SubmissionStale,
		},
		{
			name: "a launching attempt with an unsettled claim",
			arrange: func(t *testing.T, f *mailboxFixture) app.ResultSubmission {
				task, attempt, _, incarnation := launchingWorker(t, f.featureFixture, 8240)
				queueTaskInfo(t, f.featureFixture, task, 8246)
				return app.ResultSubmission{
					ID: identity.ResultID(uid(8247)), RunID: f.spec.RunID, TaskID: task, AttemptID: attempt,
					IncarnationID: incarnation, CommitOID: strings.Repeat("b", 40), Summary: "early", Digest: "digest-8247",
				}
			},
			wantKind: app.SubmissionTransient, wantReason: app.TransientAttemptNotRunning,
		},
		{
			name: "a running attempt, the only caller told to drain",
			arrange: func(t *testing.T, f *mailboxFixture) app.ResultSubmission {
				queueTaskInfo(t, f.featureFixture, f.TaskB, 8251)
				return f.resultFor(8252)
			},
			wantKind: app.SubmissionTransient, wantReason: app.TransientUndeliveredMessages,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			store := openStoreAt(t, t.TempDir(), clock)
			f := newMailboxFixtureAt(t, store, clock)
			submission := tc.arrange(t, f)

			for _, round := range []string{"first", "retried"} {
				before := receiptCount(t, store, f.spec.RunID, tc.wantKind)
				outcome, err := store.SubmitResult(t.Context(), submission)
				if err != nil || outcome.Kind != tc.wantKind || outcome.Transient != tc.wantReason {
					t.Fatalf("%s SubmitResult() = %+v, %v; want %s/%q", round, outcome, err, tc.wantKind, tc.wantReason)
				}
				kind, detail, _ := lastReceipt(t, store, f.spec.RunID)
				if kind != string(tc.wantKind) || detail != outcome.Detail {
					t.Fatalf("%s last receipt = %s %q, want %s %q", round, kind, detail, tc.wantKind, outcome.Detail)
				}
				const drain = "transient: undelivered messages; drain with hop msg next, ack, then resubmit"
				if tc.wantReason == app.TransientUndeliveredMessages && detail != drain {
					t.Fatalf("%s detail = %q, want the drain detail unchanged", round, detail)
				}
				if after := receiptCount(t, store, f.spec.RunID, tc.wantKind); after != before+1 {
					t.Fatalf("%s %s receipts = %d, want %d", round, tc.wantKind, after, before+1)
				}
			}
			if n := countRows(t, store, `SELECT COUNT(*) FROM results`); n != 0 {
				t.Fatalf("result rows after refusals = %d, want 0", n)
			}
		})
	}
}

// TestSubmitResultOldIncarnationStaysStaleWhileTheSuccessorDrains proves
// the relaunch shape end to end: the replaced incarnation is stale before
// and after the drain and can never fetch, while the successor incarnation
// of the same session fetches, acks and is accepted.
func TestSubmitResultOldIncarnationStaysStaleWhileTheSuccessorDrains(t *testing.T) {
	clock := newFakeClock()
	store := openStoreAt(t, t.TempDir(), clock)
	f := newMailboxFixtureAt(t, store, clock)
	message := queueTaskInfo(t, f.featureFixture, f.TaskB, 8261)
	successor := identity.IncarnationID(uid(8262))
	supersedeWithoutSuccessor(t, f.featureFixture, f.WorkerID)
	f.inUOW(t, func(uow app.UnitOfWork) {
		next := run.NewRuntimeBinding(f.WorkerID, successor, "/tmp/herdr.sock", "server-instance-1", "ws", "tab", "pane-8263", uid(8263), run.LaunchResume, clock.Now())
		if err := uow.Bindings().Create(t.Context(), next); err != nil {
			t.Fatalf("create successor binding: %v", err)
		}
	})

	if outcome, err := store.SubmitResult(t.Context(), f.resultFor(8264)); err != nil || outcome.Kind != app.SubmissionStale {
		t.Fatalf("old incarnation SubmitResult() = %+v, %v; want stale", outcome, err)
	}
	fetch := app.MessageFetch{RunID: f.spec.RunID, SessionID: f.WorkerID, IncarnationID: successor, Address: run.TaskAddress(f.TaskB)}
	if delivery, served, err := store.FetchNextMessage(t.Context(), fetch); err != nil || !served || delivery.Message.ID != message {
		t.Fatalf("successor fetch = %+v, %t, %v; want the queued message", delivery, served, err)
	}
	if ack, err := store.AckMessage(t.Context(), app.MessageAck{RunID: f.spec.RunID, MessageID: message, SessionID: f.WorkerID, IncarnationID: successor}); err != nil || ack.Kind != app.AckAccepted {
		t.Fatalf("successor ack = %+v, %v; want accepted", ack, err)
	}
	if outcome, err := store.SubmitResult(t.Context(), f.resultFor(8264)); err != nil || outcome.Kind != app.SubmissionStale {
		t.Fatalf("old incarnation SubmitResult() after the drain = %+v, %v; want stale", outcome, err)
	}
	accepted := f.resultFor(8265)
	accepted.IncarnationID = successor
	if outcome, err := store.SubmitResult(t.Context(), accepted); err != nil || outcome.Kind != app.SubmissionAccepted {
		t.Fatalf("successor SubmitResult() = %+v, %v; want accepted", outcome, err)
	}
}
