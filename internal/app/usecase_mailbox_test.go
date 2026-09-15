package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// mailboxFixture is a worker mid-task whose result submission and the
// manager's sends can race in either order, driven through the
// featureStore wrapper (the acceptance-side mailbox contract).
type mailboxFixture struct {
	tc     *testController
	fr     featureRun
	store  *featureStore
	TaskID identity.TaskID
	w      workerFixture
}

func newMailboxFixture(t *testing.T) *mailboxFixture {
	t.Helper()
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
	w := seedSettledWorker(t, tc, fr, taskID, 4711)
	// The attempt runs; a submission is legal from running.
	tc.Store.Attempts[w.AttemptID].value.State = run.AttemptRunning
	return &mailboxFixture{tc: tc, fr: fr, store: &featureStore{fakeStore: tc.Store}, TaskID: taskID, w: w}
}

func (f *mailboxFixture) send(t *testing.T, requestID string) app.MessageOutcome {
	t.Helper()
	outcome, err := f.tc.Store.SendMessage(context.Background(), app.MessageSend{
		ID: mintMessageID(t, f.tc), RunID: f.fr.RunID,
		Sender: run.SessionPrincipal(f.fr.ManagerID), SenderAddress: run.ManagerAddress(),
		IncarnationID: f.fr.ManagerIncarnation, Recipient: run.TaskAddress(f.TaskID),
		Kind: run.MessageInfo, BodyPath: "/state/b", BodyDigest: "d", BodyBytes: 4, RequestID: requestID,
	})
	if err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
	return outcome
}

func (f *mailboxFixture) submit(t *testing.T) app.SubmissionOutcome {
	t.Helper()
	resultID, err := identity.ParseResultID(f.tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse result id: %v", err)
	}
	outcome, err := f.store.SubmitResult(context.Background(), app.ResultSubmission{
		ID: resultID, RunID: f.fr.RunID, TaskID: f.TaskID, AttemptID: f.w.AttemptID,
		IncarnationID: f.w.IncarnationID, CommitOID: "c0mm17", Summary: "done", Digest: "d1",
	})
	if err != nil {
		t.Fatalf("SubmitResult() error = %v", err)
	}
	return outcome
}

func (f *mailboxFixture) drain(t *testing.T) {
	t.Helper()
	for {
		delivery, ok, err := f.tc.Store.FetchNextMessage(context.Background(), app.MessageFetch{
			RunID: f.fr.RunID, SessionID: f.w.SessionID, IncarnationID: f.w.IncarnationID, Address: run.TaskAddress(f.TaskID),
		})
		if err != nil {
			t.Fatalf("FetchNextMessage() error = %v", err)
		}
		if !ok {
			return
		}
		if outcome, err := f.tc.Store.AckMessage(context.Background(), app.MessageAck{
			RunID: f.fr.RunID, MessageID: delivery.Message.ID, SessionID: f.w.SessionID, IncarnationID: f.w.IncarnationID,
		}); err != nil || outcome.Kind != app.AckAccepted {
			t.Fatalf("AckMessage() = %+v err=%v", outcome, err)
		}
	}
}

func TestMailboxClosure(t *testing.T) {
	t.Run("send lands first: acceptance is refused transient until the worker drains, then closes", func(t *testing.T) {
		f := newMailboxFixture(t)
		if outcome := f.send(t, "mb-1"); outcome.Kind != app.MessageAccepted {
			t.Fatalf("send = %+v, want accepted while the mailbox is open", outcome)
		}
		outcome := f.submit(t)
		if outcome.Kind != app.SubmissionTransient {
			t.Fatalf("submission = %+v, want the transient undelivered-messages refusal", outcome)
		}
		if !strings.Contains(outcome.Detail, "drain with hop msg next") {
			t.Fatalf("transient detail = %q, want the drain instruction", outcome.Detail)
		}
		if got := f.tc.Store.Tasks[f.TaskID].value; got.MailboxClosed || got.State != run.TaskReady && got.State != run.TaskActive {
			t.Fatalf("task = %+v; a transient refusal changes no state", got.State)
		}

		f.drain(t)
		accepted := f.submit(t)
		if accepted.Kind != app.SubmissionAccepted {
			t.Fatalf("resubmission after drain = %+v, want accepted", accepted)
		}
		if !f.tc.Store.Tasks[f.TaskID].value.MailboxClosed {
			t.Fatalf("mailbox open after acceptance; acceptance closes it atomically")
		}
		// The closure lands first for any later send.
		if outcome := f.send(t, "mb-2"); outcome.Kind != app.MessageMailboxClose {
			t.Fatalf("send after closure = %+v, want refused-mailbox-closed", outcome)
		}
	})

	t.Run("closure lands first: the send is refused, no stranded row", func(t *testing.T) {
		f := newMailboxFixture(t)
		if outcome := f.submit(t); outcome.Kind != app.SubmissionAccepted {
			t.Fatalf("submission = %+v, want accepted over a clear mailbox", outcome)
		}
		messagesBefore := len(f.tc.Store.Messages)
		if outcome := f.send(t, "mb-1"); outcome.Kind != app.MessageMailboxClose {
			t.Fatalf("send = %+v, want refused-mailbox-closed", outcome)
		}
		if len(f.tc.Store.Messages) != messagesBefore {
			t.Fatalf("a refused send left a stranded message row")
		}
	})

	t.Run("a retry reopens a mailbox closed by acceptance", func(t *testing.T) {
		f := newMailboxFixture(t)
		if outcome := f.submit(t); outcome.Kind != app.SubmissionAccepted {
			t.Fatalf("submission = %+v, want accepted", outcome)
		}
		// The attempt later fails its check; the task needs rework.
		f.tc.Store.Attempts[f.w.AttemptID].value.State = run.AttemptFailed
		f.tc.Store.Tasks[f.TaskID].value.State = run.TaskNeedsRework

		accepted, err := f.tc.Store.RequestRetry(context.Background(), app.RetryRequest{
			TaskID: f.TaskID, RunID: f.fr.RunID, Session: f.fr.ManagerID,
			IncarnationID: f.fr.ManagerIncarnation, Reason: "rework", RequestID: "rt-1",
		})
		if err != nil || accepted.Outcome != app.WorkflowAccepted {
			t.Fatalf("RequestRetry() = %+v err=%v", accepted, err)
		}
		if f.tc.Store.Tasks[f.TaskID].value.MailboxClosed {
			t.Fatalf("mailbox still closed after the retry reservation; the successor attempt fetches the same address")
		}
		if outcome := f.send(t, "mb-3"); outcome.Kind != app.MessageAccepted {
			t.Fatalf("send after reopen = %+v, want accepted", outcome)
		}
	})

	t.Run("a mailbox closed by task failure is never reopened", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		f.git.MergeOutcome = "conflict"
		snap := f.tc.Store.Snapshots[f.fr.RunID]
		snap.Workflow.RetryLimit = 1
		f.tc.Store.Snapshots[f.fr.RunID] = snap

		f.drive(t) // claim
		f.drive(t) // conflict at the exhausted limit -> task failed, mailbox closed

		task := f.tc.Store.Tasks[f.taskID].value
		if task.State != run.TaskFailed || !task.MailboxClosed {
			t.Fatalf("task = %s closed=%v, want failed with the mailbox closed", task.State, task.MailboxClosed)
		}
		// A send after the failure closure is refused.
		outcome, err := f.tc.Store.SendMessage(context.Background(), app.MessageSend{
			ID: mintMessageID(t, f.tc), RunID: f.fr.RunID,
			Sender: run.SessionPrincipal(f.fr.ManagerID), SenderAddress: run.ManagerAddress(),
			IncarnationID: f.fr.ManagerIncarnation, Recipient: run.TaskAddress(f.taskID),
			Kind: run.MessageInfo, BodyPath: "/state/b", BodyDigest: "d", BodyBytes: 4, RequestID: "mb-4",
		})
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}
		if outcome.Kind != app.MessageMailboxClose {
			t.Fatalf("send after failure closure = %+v, want refused-mailbox-closed", outcome)
		}
		// No verb revives a failed task: the retry is refused, the mailbox
		// stays closed.
		retry, err := f.tc.Store.RequestRetry(context.Background(), app.RetryRequest{
			TaskID: f.taskID, RunID: f.fr.RunID, Session: f.fr.ManagerID,
			IncarnationID: f.fr.ManagerIncarnation, Reason: "revive", RequestID: "rt-2",
		})
		if err != nil {
			t.Fatalf("RequestRetry() error = %v", err)
		}
		if retry.Outcome == app.WorkflowAccepted {
			t.Fatalf("a retry revived a failed task")
		}
		if !f.tc.Store.Tasks[f.taskID].value.MailboxClosed {
			t.Fatalf("failure-closed mailbox reopened")
		}
	})

	t.Run("a send landing between the failure notice's snapshot and settlement forces the retry", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		f.git.MergeOutcome = "conflict"
		snap := f.tc.Store.Snapshots[f.fr.RunID]
		snap.Workflow.RetryLimit = 1
		f.tc.Store.Snapshots[f.fr.RunID] = snap
		f.drive(t) // claim

		// A send must land AFTER the failure settlement's obligation
		// snapshot and BEFORE its transaction. The settlement sequence's
		// commits are: ... evidence retention (stages artifact rows) →
		// the snapshot read (read-only) → the settlement transaction. The
		// hook waits for the evidence-retention commit, then injects the
		// send on the NEXT commit — the snapshot's own, which fires after
		// its reads — so the settlement's in-transaction re-read sees an
		// obligation the prepared notice missed and must roll back,
		// prepare a fresh notice and retry.
		var lateID identity.MessageID
		evidenceSeen := false
		injected := false
		f.tc.Store.CommitHook = func(u *fakeUnitOfWork) {
			if injected {
				return
			}
			if !evidenceSeen {
				if len(u.artifactsSaved) > 0 {
					evidenceSeen = true
				}
				return
			}
			injected = true
			id, err := identity.ParseMessageID(f.tc.IDs.NewID())
			if err != nil {
				return
			}
			lateID = id
			msg := run.NewInfo(id, f.fr.RunID, run.ControllerPrincipal(), run.TaskAddress(f.taskID), "", "/state/late", "d", 1, 99, f.tc.Clock.Now())
			f.tc.Store.mu.Lock()
			f.tc.Store.Messages[id] = msg
			f.tc.Store.mu.Unlock()
		}
		f.drive(t) // merge -> conflict -> settlement with one forced retry
		f.tc.Store.CommitHook = nil
		if !injected {
			t.Fatalf("the race window was never exercised; the injection hook did not fire")
		}

		task := f.tc.Store.Tasks[f.taskID].value
		if task.State != run.TaskFailed || !task.MailboxClosed {
			t.Fatalf("task = %s closed=%v, want failed with the mailbox closed", task.State, task.MailboxClosed)
		}
		notices := f.managerNotices()
		if len(notices) != 1 {
			t.Fatalf("manager notices = %d, want exactly one committed notice", len(notices))
		}
		body := string(f.tc.Artifacts.files[notices[0].BodyPath])
		if !strings.Contains(body, lateID.String()) {
			t.Fatalf("the committed notice %q does not name the late-landing obligation %s", body, lateID)
		}
		// The retry is visible: two prepared notice bodies exist on disk
		// (the stale one rolled back with its transaction), one envelope.
		prepared := 0
		for path := range f.tc.Artifacts.files {
			if strings.Contains(path, "/messages/") {
				prepared++
			}
		}
		if prepared != 2 {
			t.Fatalf("prepared notice bodies = %d, want the stale one plus the committed one", prepared)
		}
	})
}
