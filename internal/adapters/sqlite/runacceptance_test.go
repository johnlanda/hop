package sqlite_test

import (
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// runAcceptance is the outcome one run state gives every run-gated verb.
type runAcceptance string

const (
	acceptedInState  runAcceptance = "accepted"
	transientInState runAcceptance = "transient"
	refusedInState   runAcceptance = "refused"
)

// gatedRequests are one fixture's run-gated requests, each with a
// request ID so its receipts can be counted: task create, task retry,
// plan close, the manager's relay of a worker question to the human, and
// the manager's info notice to a task.
type gatedRequests struct {
	create app.TaskCreate
	retry  app.RetryRequest
	close  app.PlanClose
	relay  app.MessageSend
	notice app.MessageSend
}

// newGatedRequests seeds what the requests need — a needs-rework task
// with a terminal attempt, and a worker question the relay names — while
// the fixture run is still running.
func newGatedRequests(t *testing.T, f *messagingFixture) gatedRequests {
	t.Helper()
	reworkTask := f.createFeatureTask(t, 8601, 3, run.TaskNeedsRework)
	seedTerminalAttempt(t, f.featureFixture, reworkTask, 8602, 1)
	question, err := f.store.SendMessage(t.Context(), f.workerSend(8603, run.MessageQuestion, "worker-q"))
	if err != nil || question.Kind != app.MessageAccepted {
		t.Fatalf("SendMessage(worker question) = %+v, %v; want accepted", question, err)
	}
	relayOf := question.MessageID
	return gatedRequests{
		create: taskCreate(f.featureFixture, 8611, "gated task", "gated-create"),
		retry: app.RetryRequest{
			TaskID: reworkTask, RunID: f.spec.RunID, Session: f.ManagerID, IncarnationID: f.ManagerIncarnation,
			Reason: "flaky", RequestID: "gated-retry",
		},
		close: app.PlanClose{RunID: f.spec.RunID, Session: f.ManagerID, IncarnationID: f.ManagerIncarnation, RequestID: "gated-close"},
		relay: app.MessageSend{
			ID: identity.MessageID(uid(8621)), RunID: f.spec.RunID,
			Sender: run.SessionPrincipal(f.ManagerID), SenderAddress: run.ManagerAddress(), IncarnationID: f.ManagerIncarnation,
			Recipient: run.HumanAddress(), Kind: run.MessageQuestion, RelayedFrom: &relayOf,
			RequestID: "gated-relay", BodyPath: "/state/bodies/relay.md", BodyDigest: "digest-relay", BodyBytes: 8,
		},
		notice: app.MessageSend{
			ID: identity.MessageID(uid(8622)), RunID: f.spec.RunID,
			Sender: run.SessionPrincipal(f.ManagerID), SenderAddress: run.ManagerAddress(), IncarnationID: f.ManagerIncarnation,
			Recipient: run.TaskAddress(f.TaskB), Kind: run.MessageInfo,
			RequestID: "gated-notice", BodyPath: "/state/bodies/notice.md", BodyDigest: "digest-notice", BodyBytes: 8,
		},
	}
}

// setRunState writes the fixture run's persisted state and stop flag
// directly: the accepting transactions decide on exactly these columns.
func setRunState(t *testing.T, f *messagingFixture, state run.RunState, stopRequested bool) {
	t.Helper()
	var stopAt any
	if stopRequested {
		stopAt = "2026-09-14T09:00:00.000000000Z"
	}
	rawExec(t, f.store, `UPDATE runs SET state = ?, stop_requested_at = ? WHERE id = ?`, string(state), stopAt, f.spec.RunID.String())
}

// gatedOutcome is every run-gated verb's recorded outcome.
type gatedOutcome struct {
	create app.TaskCreated
	retry  app.RetryAccepted
	close  app.PlanCloseResult
	relay  app.MessageOutcome
	notice app.MessageOutcome
}

func issueGated(t *testing.T, f *messagingFixture, req *gatedRequests) gatedOutcome {
	t.Helper()
	var (
		out gatedOutcome
		err error
	)
	if out.create, err = f.store.CreateTask(t.Context(), req.create); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if out.retry, err = f.store.RequestRetry(t.Context(), req.retry); err != nil {
		t.Fatalf("RequestRetry: %v", err)
	}
	if out.close, err = f.store.ClosePlan(t.Context(), req.close); err != nil {
		t.Fatalf("ClosePlan: %v", err)
	}
	if out.relay, err = f.store.SendMessage(t.Context(), req.relay); err != nil {
		t.Fatalf("SendMessage(relay): %v", err)
	}
	if out.notice, err = f.store.SendMessage(t.Context(), req.notice); err != nil {
		t.Fatalf("SendMessage(notice): %v", err)
	}
	return out
}

// workflowReceiptCount and sendReceiptCount count one request key's
// receipts with one outcome.
func workflowReceiptCount(t *testing.T, f *messagingFixture, op, requestID, outcome string) int {
	t.Helper()
	return countRows(t, f.store, `SELECT COUNT(*) FROM workflow_receipts WHERE run_id = ? AND op = ? AND request_id = ? AND outcome = ?`,
		f.spec.RunID.String(), op, requestID, outcome)
}

func sendReceiptCount(t *testing.T, f *messagingFixture, requestID, outcome string) int {
	t.Helper()
	return countRows(t, f.store, `SELECT COUNT(*) FROM message_receipts WHERE run_id = ? AND op = 'msg-send' AND request_id = ? AND outcome = ?`,
		f.spec.RunID.String(), requestID, outcome)
}

// TestRunGatedVerbsAcrossRunStates drives every run-gated worker-authority
// verb — task create, task retry, plan close, and an ordinary send (a
// relayed question and a task notice) — against every persisted run state
// with and without a stop request, and pins the outcome the split
// acceptance check gives it: accepted only while running; transient while
// created, launching, resuming or completing, with a "transient" receipt
// outside the acceptance key and nothing else changed; refused
// run-not-accepting once completed, failed, stopping or stopped, or with
// a stop request. A transient request retried under the SAME request ID
// once the run is running is accepted exactly once, and its next retry is
// a duplicate. Fetch, ack and a session answer carry no run-state gate and
// proceed in every state.
func TestRunGatedVerbsAcrossRunStates(t *testing.T) {
	cases := []struct {
		state run.RunState
		stop  bool
		want  runAcceptance
	}{
		{run.RunCreated, false, transientInState},
		{run.RunLaunching, false, transientInState},
		{run.RunResuming, false, transientInState},
		{run.RunCompleting, false, transientInState},
		{run.RunRunning, false, acceptedInState},
		{run.RunCompleted, false, refusedInState},
		{run.RunFailed, false, refusedInState},
		{run.RunStopping, true, refusedInState},
		{run.RunStopped, true, refusedInState},
		{run.RunResuming, true, refusedInState},
		{run.RunLaunching, true, refusedInState},
	}
	for _, tc := range cases {
		name := string(tc.state)
		if tc.stop {
			name += "_stop_requested"
		}
		t.Run(name, func(t *testing.T) {
			f := newMessagingFixture(t)
			req := newGatedRequests(t, f)
			setRunState(t, f, tc.state, tc.stop)
			tasksBefore := countRows(t, f.store, `SELECT COUNT(*) FROM tasks`)
			attemptsBefore := countRows(t, f.store, `SELECT COUNT(*) FROM attempts`)
			messagesBefore := countRows(t, f.store, `SELECT COUNT(*) FROM messages`)
			revisionBefore := countRows(t, f.store, `SELECT revision FROM runs WHERE id = ?`, f.spec.RunID.String())

			out := issueGated(t, f, &req)

			switch tc.want {
			case acceptedInState:
				assertGatedKinds(t, &out, app.WorkflowAccepted, app.MessageAccepted, "", "")
			case transientInState:
				detail := "run is " + string(tc.state) + ", not yet running"
				assertGatedKinds(t, &out, app.WorkflowTransient, app.MessageTransient, "", detail)
				assertNothingChanged(t, f, tasksBefore, attemptsBefore, messagesBefore, revisionBefore)
				assertReceipts(t, f, "transient", 1, 0)
				if task, _, err := getTaskValue(t, f, req.retry.TaskID); err != nil || task.State != run.TaskNeedsRework {
					t.Fatalf("retried task = %+v, %v; want unchanged needs-rework", task, err)
				}

				// The same requests, once the run is running: accepted once,
				// then duplicate, each key holding one accepted receipt beside
				// its transient one.
				setRunState(t, f, run.RunRunning, false)
				accepted := issueGated(t, f, &req)
				assertGatedKinds(t, &accepted, app.WorkflowAccepted, app.MessageAccepted, "", "")
				duplicate := issueGated(t, f, &req)
				assertGatedKinds(t, &duplicate, app.WorkflowDuplicate, app.MessageDuplicate, "", "")
				if duplicate.create.TaskID != accepted.create.TaskID || duplicate.create.Seq != accepted.create.Seq ||
					duplicate.retry.AttemptNumber != accepted.retry.AttemptNumber || duplicate.retry.AttemptNumber != 2 ||
					duplicate.relay.MessageID != accepted.relay.MessageID || duplicate.notice.MessageID != accepted.notice.MessageID {
					t.Fatalf("duplicates = %+v, want the accepted entities %+v", duplicate, accepted)
				}
				assertReceipts(t, f, "transient", 1, 1)
				if n := countRows(t, f.store, `SELECT COUNT(*) FROM tasks`); n != tasksBefore+1 {
					t.Fatalf("tasks = %d, want exactly one created (%d)", n, tasksBefore+1)
				}
				if n := countRows(t, f.store, `SELECT COUNT(*) FROM messages`); n != messagesBefore+2 {
					t.Fatalf("messages = %d, want exactly the relay and the notice (%d)", n, messagesBefore+2)
				}
			case refusedInState:
				assertGatedKinds(t, &out, app.WorkflowRefused, app.MessageRunNotAccept, app.GrammarReasonRunNotAccepting, "")
				assertNothingChanged(t, f, tasksBefore, attemptsBefore, messagesBefore, revisionBefore)
				assertReceipts(t, f, "refused", 1, 0)
			}

			assertUngatedVerbsProceed(t, f)
		})
	}
}

// assertGatedKinds checks every gated outcome's kind and reason, and —
// when detail is set — its detail.
func assertGatedKinds(t *testing.T, out *gatedOutcome, workflow app.WorkflowOutcomeKind, message app.MessageOutcomeKind, reason, detail string) {
	t.Helper()
	type observed struct {
		verb, kind, reason, detail string
	}
	for _, o := range []observed{
		{"create", string(out.create.Outcome), out.create.Reason, out.create.Detail},
		{"retry", string(out.retry.Outcome), out.retry.Reason, out.retry.Detail},
		{"close", string(out.close.Outcome), out.close.Reason, out.close.Detail},
		{"relay", string(out.relay.Kind), out.relay.Reason, out.relay.Detail},
		{"notice", string(out.notice.Kind), out.notice.Reason, out.notice.Detail},
	} {
		want := string(workflow)
		if o.verb == "relay" || o.verb == "notice" {
			want = string(message)
		}
		if o.kind != want || o.reason != reason || (detail != "" && o.detail != detail) {
			t.Errorf("%s outcome = (%s, %q, %q), want (%s, %q, %q)", o.verb, o.kind, o.reason, o.detail, want, reason, detail)
		}
	}
}

// assertNothingChanged checks a non-accepted round created no task,
// attempt or message and left the run row's revision alone.
func assertNothingChanged(t *testing.T, f *messagingFixture, tasks, attempts, messages, revision int) {
	t.Helper()
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM tasks`); n != tasks {
		t.Errorf("tasks = %d, want unchanged %d", n, tasks)
	}
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM attempts`); n != attempts {
		t.Errorf("attempts = %d, want unchanged %d", n, attempts)
	}
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM messages`); n != messages {
		t.Errorf("messages = %d, want unchanged %d", n, messages)
	}
	if n := countRows(t, f.store, `SELECT revision FROM runs WHERE id = ?`, f.spec.RunID.String()); n != revision {
		t.Errorf("run revision = %d, want unchanged %d", n, revision)
	}
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM retry_requests`); n != 0 {
		t.Errorf("retry requests = %d, want none", n)
	}
}

// assertReceipts checks every gated request key holds exactly nonAccepted
// receipts of the given outcome family and exactly accepted acceptances.
// The message refusal kind is its own outcome string.
func assertReceipts(t *testing.T, f *messagingFixture, outcome string, nonAccepted, accepted int) {
	t.Helper()
	messageOutcome := outcome
	if outcome == "refused" {
		messageOutcome = string(app.MessageRunNotAccept)
	}
	for _, key := range []struct{ op, requestID string }{
		{"task-create", "gated-create"}, {"task-retry", "gated-retry"}, {"plan-close", "gated-close"},
	} {
		if n := workflowReceiptCount(t, f, key.op, key.requestID, outcome); n != nonAccepted {
			t.Errorf("%s %s receipts = %d, want %d", key.op, outcome, n, nonAccepted)
		}
		if n := workflowReceiptCount(t, f, key.op, key.requestID, "accepted"); n != accepted {
			t.Errorf("%s accepted receipts = %d, want %d", key.op, n, accepted)
		}
	}
	for _, requestID := range []string{"gated-relay", "gated-notice"} {
		if n := sendReceiptCount(t, f, requestID, messageOutcome); n != nonAccepted {
			t.Errorf("msg-send %s %s receipts = %d, want %d", requestID, messageOutcome, n, nonAccepted)
		}
		if n := sendReceiptCount(t, f, requestID, "accepted"); n != accepted {
			t.Errorf("msg-send %s accepted receipts = %d, want %d", requestID, n, accepted)
		}
	}
}

// assertUngatedVerbsProceed checks the verbs with no run-state gate in the
// run's current state: the manager fetches the worker question, acks it,
// and answers it, and the worker fetches the answer.
func assertUngatedVerbsProceed(t *testing.T, f *messagingFixture) {
	t.Helper()
	delivery, served, err := f.store.FetchNextMessage(t.Context(), f.managerFetch())
	if err != nil || !served || delivery.Message.Kind != run.MessageQuestion {
		t.Fatalf("manager fetch = %+v, %t, %v; want the worker question", delivery, served, err)
	}
	questionID := delivery.Message.ID
	ack, err := f.store.AckMessage(t.Context(), app.MessageAck{RunID: f.spec.RunID, MessageID: questionID, SessionID: f.ManagerID, IncarnationID: f.ManagerIncarnation})
	if err != nil || ack.Kind != app.AckAccepted {
		t.Fatalf("manager ack = %+v, %v; want accepted", ack, err)
	}
	answer, err := f.store.SendMessage(t.Context(), app.MessageSend{
		ID: identity.MessageID(uid(8631)), RunID: f.spec.RunID,
		Sender: run.SessionPrincipal(f.ManagerID), SenderAddress: run.ManagerAddress(), IncarnationID: f.ManagerIncarnation,
		Kind: run.MessageAnswer, ReplyTo: &questionID, RequestID: "ungated-answer",
		BodyPath: "/state/bodies/answer.md", BodyDigest: "digest-answer", BodyBytes: 8,
	})
	if err != nil || answer.Kind != app.MessageAccepted {
		t.Fatalf("manager answer = %+v, %v; want accepted", answer, err)
	}
	workerDelivery, served, err := f.store.FetchNextMessage(t.Context(), app.MessageFetch{
		RunID: f.spec.RunID, SessionID: f.WorkerID, IncarnationID: f.WorkerIncarnation, Address: run.TaskAddress(f.TaskB),
	})
	if err != nil || !served || workerDelivery.Message.Kind == "" {
		t.Fatalf("worker fetch = %+v, %t, %v; want a served message", workerDelivery, served, err)
	}
}

// getTaskValue loads one task row through a unit of work.
func getTaskValue(t *testing.T, f *messagingFixture, id identity.TaskID) (run.Task, int64, error) {
	t.Helper()
	var (
		task     run.Task
		revision int64
		getErr   error
	)
	f.inUOW(t, func(uow app.UnitOfWork) {
		task, revision, getErr = uow.Tasks().Get(t.Context(), id)
	})
	return task, revision, getErr
}
