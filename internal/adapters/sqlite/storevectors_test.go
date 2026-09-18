package sqlite_test

import (
	"errors"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
	"github.com/johnlanda/hop/internal/testsupport/storevectors"
)

// TestStoreVectors drives every internal/testsupport/storevectors vector
// against the REAL store through the ordinary PlanStore/MessagingStore
// ports — the real-adapter half of the shared refused-input contract
// (docs/plan/phase-3-design.md section 11's countermeasure): the exact
// request internal/app's TestStoreVectors drives against fakeStore is
// refused identically here.
func TestStoreVectors(t *testing.T) {
	t.Run("TaskCreateSelfDependency", func(t *testing.T) {
		f := newFeatureFixture(t)
		got, err := f.store.CreateTask(t.Context(), storevectors.TaskCreateSelfDependency(f.spec.RunID, f.ManagerID, f.ManagerIncarnation, identity.TaskID(uid(8001))))
		if err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if got.Outcome != app.WorkflowRefused || got.Reason != storevectors.TaskCreateSelfDependencyReason {
			t.Fatalf("CreateTask(self-dependency) = %+v, want refused/%s", got, storevectors.TaskCreateSelfDependencyReason)
		}
	})

	t.Run("TaskCreateRequestIDConflict", func(t *testing.T) {
		f := newFeatureFixture(t)
		const requestID = "shared-request-id"
		first, err := f.store.CreateTask(t.Context(), storevectors.TaskCreateRequestIDConflictFirst(f.spec.RunID, f.ManagerID, f.ManagerIncarnation, identity.TaskID(uid(8002)), requestID))
		if err != nil {
			t.Fatalf("CreateTask(first) error = %v", err)
		}
		if first.Outcome != app.WorkflowAccepted {
			t.Fatalf("CreateTask(first) = %+v, want accepted", first)
		}
		second, err := f.store.CreateTask(t.Context(), storevectors.TaskCreateRequestIDConflictSecond(f.spec.RunID, f.ManagerID, f.ManagerIncarnation, identity.TaskID(uid(8003)), requestID))
		if err != nil {
			t.Fatalf("CreateTask(second) error = %v", err)
		}
		if second.Outcome != app.WorkflowRefused || second.Reason != storevectors.TaskCreateRequestIDConflictReason {
			t.Fatalf("CreateTask(second, conflicting request id) = %+v, want refused/%s", second, storevectors.TaskCreateRequestIDConflictReason)
		}
	})

	t.Run("TaskCreateNonManagerCaller", func(t *testing.T) {
		f := newFeatureFixture(t)
		taskA := f.createFeatureTask(t, 8004, 2, run.TaskActive)
		workerID, workerIncarnation := f.createWorkerSession(t, taskA, run.RoleImplementer, 8005)
		got, err := f.store.CreateTask(t.Context(), storevectors.TaskCreateNonManagerCaller(f.spec.RunID, workerID, workerIncarnation, identity.TaskID(uid(8008))))
		if err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if got.Outcome != app.WorkflowRefused || got.Reason != storevectors.TaskCreateNonManagerCallerReason {
			t.Fatalf("CreateTask(non-manager caller) = %+v, want refused/%s", got, storevectors.TaskCreateNonManagerCallerReason)
		}
	})

	t.Run("TaskCreateOversizedTitle", func(t *testing.T) {
		f := newFeatureFixture(t)
		got, err := f.store.CreateTask(t.Context(), storevectors.TaskCreateOversizedTitle(f.spec.RunID, f.ManagerID, f.ManagerIncarnation, identity.TaskID(uid(8009))))
		if err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if got.Outcome != app.WorkflowMalformed || got.Reason != storevectors.TaskCreateOversizedTitleReason {
			t.Fatalf("CreateTask(oversized title) = %+v, want malformed/%s", got, storevectors.TaskCreateOversizedTitleReason)
		}
	})

	t.Run("AckMessageStaleIncarnation", func(t *testing.T) {
		f := newMessagingFixture(t)
		send := f.workerSend(8011, run.MessageQuestion, "")
		if outcome, err := f.store.SendMessage(t.Context(), send); err != nil || outcome.Kind != app.MessageAccepted {
			t.Fatalf("seed question: %+v, %v", outcome, err)
		}
		if _, served, err := f.store.FetchNextMessage(t.Context(), f.managerFetch()); err != nil || !served {
			t.Fatalf("serve question: %t, %v", served, err)
		}
		// Supersede the manager's incarnation that actually received the
		// delivery, with no replacement: AckMessageStaleIncarnation's
		// claimed incarnation (f.ManagerIncarnation) then satisfies the
		// delivery check but fails currentBinding's currency check —
		// ErrStaleAck, not ErrNotDelivered (which an incarnation never
		// delivered to at all would hit first).
		f.inUOW(t, func(uow app.UnitOfWork) {
			binding, ok, err := uow.Bindings().Current(t.Context(), f.ManagerID)
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
		got, err := f.store.AckMessage(t.Context(), storevectors.AckMessageStaleIncarnation(f.spec.RunID, send.ID, f.ManagerID, f.ManagerIncarnation))
		if err != nil {
			t.Fatalf("AckMessage() error = %v", err)
		}
		if got.Kind != app.AckRefused || got.Reason != storevectors.AckMessageStaleIncarnationReason {
			t.Fatalf("AckMessage(stale incarnation) = %+v, want refused/%s", got, storevectors.AckMessageStaleIncarnationReason)
		}
	})

	t.Run("AckMessageUnknownMessage", func(t *testing.T) {
		f := newMessagingFixture(t)
		got, err := f.store.AckMessage(t.Context(), storevectors.AckMessageUnknownMessage(f.spec.RunID, identity.MessageID(uid(8018)), f.ManagerID, f.ManagerIncarnation))
		if err != nil {
			t.Fatalf("AckMessage() error = %v", err)
		}
		if got.Kind != app.AckRefused || got.Reason != storevectors.AckMessageUnknownMessageReason {
			t.Fatalf("AckMessage(unknown message) = %+v, want refused/%s", got, storevectors.AckMessageUnknownMessageReason)
		}
	})

	t.Run("AckMessageNotDelivered", func(t *testing.T) {
		f := newMessagingFixture(t)
		send := f.workerSend(8019, run.MessageQuestion, "")
		if outcome, err := f.store.SendMessage(t.Context(), send); err != nil || outcome.Kind != app.MessageAccepted {
			t.Fatalf("seed question: %+v, %v", outcome, err)
		}
		got, err := f.store.AckMessage(t.Context(), storevectors.AckMessageNotDelivered(f.spec.RunID, send.ID, f.ManagerID, f.ManagerIncarnation))
		if err != nil {
			t.Fatalf("AckMessage() error = %v", err)
		}
		if got.Kind != app.AckRefused || got.Reason != storevectors.AckMessageNotDeliveredReason {
			t.Fatalf("AckMessage(not delivered) = %+v, want refused/%s", got, storevectors.AckMessageNotDeliveredReason)
		}
	})

	t.Run("MessageSendAnswerUnknownQuestion", func(t *testing.T) {
		f := newFeatureFixture(t)
		got, err := f.store.SendMessage(t.Context(), storevectors.MessageSendAnswerUnknownQuestion(
			f.spec.RunID, f.ManagerID, run.ManagerAddress(), f.ManagerIncarnation,
			identity.MessageID(uid(8013)), identity.MessageID(uid(8014)), "/state/body.md", "digest", 3,
		))
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}
		if got.Kind != app.MessageMalformed || got.Reason != storevectors.MessageSendAnswerUnknownQuestionReason {
			t.Fatalf("SendMessage(answer to unknown question) = %+v, want malformed/%s", got, storevectors.MessageSendAnswerUnknownQuestionReason)
		}
	})

	t.Run("MessageSendAnswerNotRecipient", func(t *testing.T) {
		f := newMessagingFixture(t)
		humanQuestion := seedHumanQuestion(t, f.store, f, 8060)
		answerID := identity.MessageID(uid(8062))

		// The worker is current and in the run, but the question is the
		// human's to answer.
		got, err := f.store.SendMessage(t.Context(), storevectors.MessageSendAnswerNotRecipient(
			f.spec.RunID, f.WorkerID, run.TaskAddress(f.TaskB), f.WorkerIncarnation, answerID, humanQuestion, "/state/body.md", "digest", 3,
		))
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}
		if got.Kind != app.MessageRefused || got.Reason != storevectors.MessageSendAnswerNotRecipientReason || got.Detail != storevectors.MessageSendAnswerNotRecipientDetail {
			t.Fatalf("SendMessage(answer by a non-recipient) = %+v, want refused/%s %q", got, storevectors.MessageSendAnswerNotRecipientReason, storevectors.MessageSendAnswerNotRecipientDetail)
		}
		if n := countRows(t, f.store, `SELECT COUNT(*) FROM messages WHERE id = ? OR reply_to = ?`, answerID.String(), humanQuestion.String()); n != 0 {
			t.Fatalf("refused answer left %d envelope(s); want none", n)
		}
		if n := countRows(t, f.store, `SELECT COUNT(*) FROM message_acks WHERE message_id = ?`, humanQuestion.String()); n != 0 {
			t.Fatalf("refused answer acknowledged the human question (%d ack rows); want none", n)
		}
	})

	t.Run("MessageSendAnswerMailboxClosed", func(t *testing.T) {
		clock := newFakeClock()
		store := openStoreAt(t, t.TempDir(), clock)
		f := newMailboxFixtureAt(t, store, clock)
		question := app.MessageSend{
			ID: identity.MessageID(uid(8063)), RunID: f.spec.RunID,
			Sender: run.SessionPrincipal(f.WorkerID), SenderAddress: run.TaskAddress(f.TaskB),
			IncarnationID: f.WorkerIncarnation, Recipient: run.ManagerAddress(), Kind: run.MessageQuestion,
			BodyPath: "/state/q.md", BodyDigest: "digest-q", BodyBytes: 1,
		}
		if outcome, err := store.SendMessage(t.Context(), question); err != nil || outcome.Kind != app.MessageAccepted {
			t.Fatalf("seed worker question: %+v, %v", outcome, err)
		}
		// The worker's own mailbox is empty, so its result is accepted and
		// the task mailbox closes in the same commit.
		if outcome, err := store.SubmitResult(t.Context(), f.resultFor(8064)); err != nil || outcome.Kind != app.SubmissionAccepted {
			t.Fatalf("SubmitResult() = %+v, %v; want accepted", outcome, err)
		}
		answerID := identity.MessageID(uid(8065))

		got, err := store.SendMessage(t.Context(), storevectors.MessageSendAnswerMailboxClosed(
			f.spec.RunID, f.ManagerID, run.ManagerAddress(), f.ManagerIncarnation, answerID, question.ID, "/state/a.md", "digest-a", 1,
		))
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}
		if got.Kind != storevectors.MessageSendAnswerMailboxClosedKind || got.Reason != storevectors.MessageSendAnswerMailboxClosedReason {
			t.Fatalf("SendMessage(answer into a closed mailbox) = %+v, want %s/%s", got, storevectors.MessageSendAnswerMailboxClosedKind, storevectors.MessageSendAnswerMailboxClosedReason)
		}
		if n := countRows(t, store, `SELECT COUNT(*) FROM messages WHERE id = ? OR recipient_address = ?`, answerID.String(), app.AddressString(run.TaskAddress(f.TaskB))); n != 0 {
			t.Fatalf("refused answer left %d envelope(s) for the closed task; want none", n)
		}
	})

	t.Run("MessageSendClaimedAddressMismatch", func(t *testing.T) {
		f := newMessagingFixture(t)
		taskC := f.createFeatureTask(t, 8070, 3, run.TaskActive)
		workerC, workerCIncarnation := f.createWorkerSession(t, taskC, run.RoleImplementer, 8071)
		fromC := app.MessageSend{
			ID: identity.MessageID(uid(8075)), RunID: f.spec.RunID,
			Sender: run.SessionPrincipal(workerC), SenderAddress: run.TaskAddress(taskC),
			IncarnationID: workerCIncarnation, Recipient: run.ManagerAddress(), Kind: run.MessageQuestion,
			BodyPath: "/state/q.md", BodyDigest: "digest-q", BodyBytes: 1,
		}
		if outcome, err := f.store.SendMessage(t.Context(), fromC); err != nil || outcome.Kind != app.MessageAccepted {
			t.Fatalf("seed task C question: %+v, %v", outcome, err)
		}
		cases := []struct {
			name    string
			n       int
			claimed run.Address
			kind    run.MessageKind
			to      run.Address
			replyTo *identity.MessageID
		}{
			{"a worker claiming manager sends info to another task", 8076, run.ManagerAddress(), run.MessageInfo, run.TaskAddress(taskC), nil},
			{"a worker claiming manager asks the human", 8077, run.ManagerAddress(), run.MessageQuestion, run.HumanAddress(), nil},
			{"a worker claiming manager answers a manager-addressed question", 8078, run.ManagerAddress(), run.MessageAnswer, run.Address{}, &fromC.ID},
			{"a worker claiming another task asks the manager", 8079, run.TaskAddress(taskC), run.MessageQuestion, run.ManagerAddress(), nil},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				messageID := identity.MessageID(uid(tc.n))
				before := messageReceiptCount(t, f.store, f.spec.RunID)
				got, err := f.store.SendMessage(t.Context(), storevectors.MessageSendClaimedAddressMismatch(
					f.spec.RunID, f.WorkerID, tc.claimed, f.WorkerIncarnation, messageID, tc.kind, tc.to, tc.replyTo, "/state/body.md", "digest", 3,
				))
				if err != nil {
					t.Fatalf("SendMessage() error = %v", err)
				}
				if got.Kind != app.MessageRefused || got.Reason != storevectors.MessageSendClaimedAddressMismatchReason || got.Detail != storevectors.MessageSendClaimedAddressMismatchDetail {
					t.Fatalf("SendMessage(claimed address) = %+v, want refused/%s %q", got, storevectors.MessageSendClaimedAddressMismatchReason, storevectors.MessageSendClaimedAddressMismatchDetail)
				}
				if n := countRows(t, f.store, `SELECT COUNT(*) FROM messages WHERE id = ?`, messageID.String()); n != 0 {
					t.Fatal("the refused send created an envelope; want none")
				}
				if after := messageReceiptCount(t, f.store, f.spec.RunID); after != before+1 {
					t.Fatalf("the refusal left %d receipts, want exactly one more than %d", after, before)
				}
			})
		}
		if n := countRows(t, f.store, `SELECT COUNT(*) FROM messages WHERE kind = 'answer' AND reply_to = ?`, fromC.ID.String()); n != 0 {
			t.Errorf("task C's question has %d answers after the refused claim; want none", n)
		}

		t.Run("a session with no messaging role claims a task address", func(t *testing.T) {
			solo := newFixture(t)
			solo.createBinding(t)
			messageID := identity.MessageID(uid(8080))
			got, err := solo.store.SendMessage(t.Context(), storevectors.MessageSendClaimedAddressMismatch(
				solo.spec.RunID, solo.spec.SessionID, run.TaskAddress(solo.spec.TaskID), solo.spec.IncarnationID, messageID,
				run.MessageQuestion, run.ManagerAddress(), nil, "/state/body.md", "digest", 3,
			))
			if err != nil {
				t.Fatalf("SendMessage() error = %v", err)
			}
			if got.Kind != app.MessageRefused || got.Reason != storevectors.MessageSendClaimedAddressMismatchReason || got.Detail != storevectors.MessageSendClaimedAddressMismatchDetail {
				t.Fatalf("SendMessage(solo session) = %+v, want refused/%s %q", got, storevectors.MessageSendClaimedAddressMismatchReason, storevectors.MessageSendClaimedAddressMismatchDetail)
			}
			if n := countRows(t, solo.store, `SELECT COUNT(*) FROM messages`); n != 0 {
				t.Fatalf("the refused send left %d envelopes; want none", n)
			}
		})
	})

	t.Run("MessageSendClaimedAddressReplay", func(t *testing.T) {
		f := newMessagingFixture(t)
		taskC := f.createFeatureTask(t, 8100, 3, run.TaskActive)
		workerC, workerCIncarnation := f.createWorkerSession(t, taskC, run.RoleImplementer, 8101)
		question := f.workerSend(8105, run.MessageQuestion, "q-1")
		requireSendOutcome(t, f.store, "task B question", question, app.MessageAccepted, "")
		requireSendOutcome(t, f.store, "manager answer", f.managerAnswer(8106, question.ID, "fwd-1", "secret-body"), app.MessageAccepted, "")
		info := app.MessageSend{
			ID: identity.MessageID(uid(8107)), RunID: f.spec.RunID,
			Sender: run.SessionPrincipal(f.ManagerID), SenderAddress: run.ManagerAddress(),
			IncarnationID: f.ManagerIncarnation, Recipient: run.TaskAddress(f.TaskB), Kind: run.MessageInfo,
			RequestID: "info-1", BodyPath: "/state/info.md", BodyDigest: "info-body", BodyBytes: 4,
		}
		requireSendOutcome(t, f.store, "manager info to task B", info, app.MessageAccepted, "")

		cases := []struct {
			name      string
			n         int
			claimed   run.Address
			kind      run.MessageKind
			to        run.Address
			replyTo   *identity.MessageID
			requestID string
			digest    string
		}{
			{"the manager's answer, same body", 8110, run.ManagerAddress(), run.MessageAnswer, run.Address{}, &question.ID, "fwd-1", "secret-body"},
			{"the manager's answer, another body", 8111, run.ManagerAddress(), run.MessageAnswer, run.Address{}, &question.ID, "fwd-1", "guess-body"},
			{"the manager's info, same body", 8112, run.ManagerAddress(), run.MessageInfo, run.TaskAddress(f.TaskB), nil, "info-1", "info-body"},
			{"the manager's info, another body", 8113, run.ManagerAddress(), run.MessageInfo, run.TaskAddress(f.TaskB), nil, "info-1", "guess-body"},
			{"task B's question, same body", 8114, run.TaskAddress(f.TaskB), run.MessageQuestion, run.ManagerAddress(), nil, "q-1", question.BodyDigest},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				messageID := identity.MessageID(uid(tc.n))
				before := messageReceiptCount(t, f.store, f.spec.RunID)
				got, err := f.store.SendMessage(t.Context(), storevectors.MessageSendClaimedAddressReplay(
					f.spec.RunID, workerC, tc.claimed, workerCIncarnation, messageID, tc.kind, tc.to, tc.replyTo, tc.requestID, "/state/body.md", tc.digest, 3,
				))
				if err != nil {
					t.Fatalf("SendMessage() error = %v", err)
				}
				if got.Kind != app.MessageRefused || got.Reason != storevectors.MessageSendClaimedAddressReplayReason ||
					got.Detail != storevectors.MessageSendClaimedAddressReplayDetail || got.MessageID != "" {
					t.Fatalf("SendMessage(claimed-address replay) = %+v, want refused/%s %q naming no message", got, storevectors.MessageSendClaimedAddressReplayReason, storevectors.MessageSendClaimedAddressReplayDetail)
				}
				if n := countRows(t, f.store, `SELECT COUNT(*) FROM messages WHERE id = ?`, messageID.String()); n != 0 {
					t.Fatal("the refused replay created an envelope; want none")
				}
				if after := messageReceiptCount(t, f.store, f.spec.RunID); after != before+1 {
					t.Fatalf("the refusal left %d receipts, want exactly one more than %d", after, before)
				}
			})
		}
		if n := countRows(t, f.store, `SELECT COUNT(*) FROM messages WHERE reply_to = ? OR recipient_address = ?`, question.ID.String(), app.AddressString(run.TaskAddress(f.TaskB))); n != 2 {
			t.Errorf("envelopes answering task B's question or addressed to task B = %d; want the manager's answer and info only", n)
		}
	})

	t.Run("MessageSendCrossRun", func(t *testing.T) {
		clock := newFakeClock()
		store := openStoreAt(t, t.TempDir(), clock)
		fr1 := buildMessagingFixtureAt(t, store, clock)
		fr2 := buildSecondMessagingFixtureAt(t, store, clock)
		messageID := identity.MessageID(uid(8015))

		// fr1's manager session, but the request claims fr2 as its run.
		got, err := store.SendMessage(t.Context(), storevectors.MessageSendCrossRun(
			fr2.spec.RunID, fr1.ManagerID, run.ManagerAddress(), fr1.ManagerIncarnation, messageID, run.HumanAddress(), "/state/body.md", "digest", 3,
		))
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}
		if got.Kind != app.MessageRefused || got.Reason != storevectors.MessageSendCrossRunReason {
			t.Fatalf("SendMessage(cross-run) = %+v, want refused/%s", got, storevectors.MessageSendCrossRunReason)
		}
		if n := countRows(t, store, `SELECT COUNT(*) FROM messages WHERE id = ?`, messageID.String()); n != 0 {
			t.Fatal("SendMessage(cross-run) must not create a message")
		}
	})

	t.Run("MessageFetchCrossRun", func(t *testing.T) {
		clock := newFakeClock()
		store := openStoreAt(t, t.TempDir(), clock)
		fr1 := buildMessagingFixtureAt(t, store, clock)
		fr2 := buildSecondMessagingFixtureAt(t, store, clock)

		_, ok, err := store.FetchNextMessage(t.Context(), storevectors.MessageFetchCrossRun(
			fr2.spec.RunID, fr1.ManagerID, fr1.ManagerIncarnation, run.ManagerAddress(),
		))
		if ok {
			t.Fatal("FetchNextMessage(cross-run) reported a delivery; want refused")
		}
		if !errors.Is(err, app.ErrMessagingUnauthorized) {
			t.Fatalf("FetchNextMessage(cross-run) error = %v, want ErrMessagingUnauthorized", err)
		}
	})

	t.Run("MessageFetchSupersededAttempt", func(t *testing.T) {
		f := newTaskLineageFixture(t)
		f.retireAndRetry(t)
		message := queueTaskInfo(t, f.featureFixture, f.Task, 8090)
		requireFetchRefused(t, f.store, ptr(storevectors.MessageFetchSupersededAttempt(f.spec.RunID, f.OldSession, f.OldIncarnation, f.Task)), storevectors.MessageFetchSupersededAttemptDetail)
		if delivery, served, err := f.store.FetchNextMessage(t.Context(), f.fetch(f.NewSession, f.NewIncarnation)); err != nil || !served || delivery.Message.ID != message {
			t.Fatalf("successor fetch = %+v, %t, %v; want the message", delivery, served, err)
		}
		if n := countRows(t, f.store, `SELECT COUNT(*) FROM message_deliveries WHERE message_id = ?`, message.String()); n != 1 {
			t.Fatalf("delivery rows = %d, want the successor's first delivery only", n)
		}
	})

	t.Run("AckMessageSupersededAttempt", func(t *testing.T) {
		f := newTaskLineageFixture(t)
		message := queueTaskInfo(t, f.featureFixture, f.Task, 8091)
		if _, served, err := f.store.FetchNextMessage(t.Context(), f.fetch(f.OldSession, f.OldIncarnation)); err != nil || !served {
			t.Fatalf("fetch while current = %t, %v", served, err)
		}
		f.retireAndRetry(t)
		got, err := f.store.AckMessage(t.Context(), storevectors.AckMessageSupersededAttempt(f.spec.RunID, message, f.OldSession, f.OldIncarnation))
		if err != nil || got.Kind != app.AckRefused || got.Reason != storevectors.AckMessageSupersededAttemptReason {
			t.Fatalf("AckMessage(superseded attempt) = %+v, %v; want refused/%s", got, err, storevectors.AckMessageSupersededAttemptReason)
		}
		if n := countRows(t, f.store, `SELECT COUNT(*) FROM message_acks`); n != 0 {
			t.Fatalf("ack rows = %d, want none", n)
		}
		if delivery, served, err := f.store.FetchNextMessage(t.Context(), f.fetch(f.NewSession, f.NewIncarnation)); err != nil || !served || delivery.Message.ID != message {
			t.Fatalf("successor fetch = %+v, %t, %v; want the message re-served", delivery, served, err)
		}
		if ack, err := f.store.AckMessage(t.Context(), f.ack(message, f.NewSession, f.NewIncarnation)); err != nil || ack.Kind != app.AckAccepted {
			t.Fatalf("successor ack = %+v, %v; want accepted", ack, err)
		}
	})

	t.Run("MessageFetchEndedManager", func(t *testing.T) {
		f := newMessagingFixture(t)
		send := f.workerSend(8092, run.MessageQuestion, "")
		if outcome, err := f.store.SendMessage(t.Context(), send); err != nil || outcome.Kind != app.MessageAccepted {
			t.Fatalf("seed question: %+v, %v", outcome, err)
		}
		successor, successorIncarnation := endManagerWithSuccessor(t, f.featureFixture, 8093)
		requireFetchRefused(t, f.store, ptr(storevectors.MessageFetchEndedManager(f.spec.RunID, f.ManagerID, f.ManagerIncarnation)), storevectors.MessageFetchEndedManagerDetail)
		fetch := app.MessageFetch{RunID: f.spec.RunID, SessionID: successor, IncarnationID: successorIncarnation, Address: run.ManagerAddress()}
		if delivery, served, err := f.store.FetchNextMessage(t.Context(), fetch); err != nil || !served || delivery.Message.ID != send.ID {
			t.Fatalf("successor manager fetch = %+v, %t, %v; want the question", delivery, served, err)
		}
	})

	t.Run("AckMessageEndedManager", func(t *testing.T) {
		f := newMessagingFixture(t)
		send := f.workerSend(8096, run.MessageQuestion, "")
		if outcome, err := f.store.SendMessage(t.Context(), send); err != nil || outcome.Kind != app.MessageAccepted {
			t.Fatalf("seed question: %+v, %v", outcome, err)
		}
		if _, served, err := f.store.FetchNextMessage(t.Context(), f.managerFetch()); err != nil || !served {
			t.Fatalf("manager fetch while current = %t, %v", served, err)
		}
		endManagerWithSuccessor(t, f.featureFixture, 8097)
		got, err := f.store.AckMessage(t.Context(), storevectors.AckMessageEndedManager(f.spec.RunID, send.ID, f.ManagerID, f.ManagerIncarnation))
		if err != nil || got.Kind != app.AckRefused || got.Reason != storevectors.AckMessageEndedManagerReason {
			t.Fatalf("AckMessage(ended manager) = %+v, %v; want refused/%s", got, err, storevectors.AckMessageEndedManagerReason)
		}
		if n := countRows(t, f.store, `SELECT COUNT(*) FROM message_acks`); n != 0 {
			t.Fatalf("ack rows = %d, want none", n)
		}
	})

	t.Run("MessageSendSupersededAttempt", func(t *testing.T) {
		f := newTaskLineageFixture(t)
		question := queueTaskQuestion(t, f.featureFixture, f.Task, 8120)
		if _, served, err := f.store.FetchNextMessage(t.Context(), f.fetch(f.OldSession, f.OldIncarnation)); err != nil || !served {
			t.Fatalf("fetch while current = %t, %v", served, err)
		}
		f.retireAndRetry(t)
		answerID := identity.MessageID(uid(8121))
		got, err := f.store.SendMessage(t.Context(), storevectors.MessageSendSupersededAttempt(f.spec.RunID, f.OldSession, f.Task, f.OldIncarnation, answerID, question, "/state/a.md", "stale-answer", 5))
		if err != nil || got.Kind != app.MessageRefused || got.Reason != storevectors.MessageSendSupersededAttemptReason || got.Detail != storevectors.MessageSendSupersededAttemptDetail {
			t.Fatalf("SendMessage(superseded attempt) = %+v, %v; want refused/%s %q", got, err, storevectors.MessageSendSupersededAttemptReason, storevectors.MessageSendSupersededAttemptDetail)
		}
		if n := countRows(t, f.store, `SELECT COUNT(*) FROM messages WHERE id = ? OR reply_to = ?`, answerID.String(), question.String()); n != 0 {
			t.Fatalf("the refused answer left %d envelope(s); want none", n)
		}
		if delivery, served, err := f.store.FetchNextMessage(t.Context(), f.fetch(f.NewSession, f.NewIncarnation)); err != nil || !served || delivery.Message.ID != question {
			t.Fatalf("successor fetch = %+v, %t, %v; want the question re-served", delivery, served, err)
		}
		requireSendOutcome(t, f.store, "successor's answer",
			sessionAnswer(f.spec.RunID, 8122, f.NewSession, run.TaskAddress(f.Task), f.NewIncarnation, question, "", "fresh-answer"),
			app.MessageAccepted, "")
	})

	t.Run("MessageSendEndedManager", func(t *testing.T) {
		f := newMessagingFixture(t)
		successor, successorIncarnation := endManagerWithSuccessor(t, f.featureFixture, 8123)
		messageID := identity.MessageID(uid(8126))
		got, err := f.store.SendMessage(t.Context(), storevectors.MessageSendEndedManager(f.spec.RunID, f.ManagerID, f.ManagerIncarnation, messageID, f.TaskB, "/state/i.md", "ended-info", 4))
		if err != nil || got.Kind != app.MessageRefused || got.Reason != storevectors.MessageSendEndedManagerReason || got.Detail != storevectors.MessageSendEndedManagerDetail {
			t.Fatalf("SendMessage(ended manager) = %+v, %v; want refused/%s %q", got, err, storevectors.MessageSendEndedManagerReason, storevectors.MessageSendEndedManagerDetail)
		}
		if n := countRows(t, f.store, `SELECT COUNT(*) FROM messages`); n != 0 {
			t.Fatalf("the refused send left %d envelope(s); want none", n)
		}
		requireSendOutcome(t, f.store, "successor manager's info",
			storevectors.MessageSendEndedManager(f.spec.RunID, successor, successorIncarnation, identity.MessageID(uid(8127)), f.TaskB, "/state/i.md", "successor-info", 4),
			app.MessageAccepted, "")
	})

	t.Run("AckMessageCrossRun", func(t *testing.T) {
		clock := newFakeClock()
		store := openStoreAt(t, t.TempDir(), clock)
		fr1 := buildMessagingFixtureAt(t, store, clock)
		fr2 := buildSecondMessagingFixtureAt(t, store, clock)
		send := fr1.workerSend(8016, run.MessageQuestion, "")
		if outcome, err := store.SendMessage(t.Context(), send); err != nil || outcome.Kind != app.MessageAccepted {
			t.Fatalf("seed question: %+v, %v", outcome, err)
		}
		// A delivery row for fr2's manager, seeded directly rather than
		// through FetchNextMessage (whose own cross-run refusal — the
		// MessageFetchCrossRun vector above — would otherwise make this row
		// impossible to create honestly): this isolates the ack-side
		// cross-run check from the ALSO-true never-delivered refusal,
		// defense in depth against exactly this kind of already-corrupt row.
		rawExec(t, store, `INSERT INTO message_deliveries (id, message_id, session_id, incarnation_id, delivered_at) VALUES (?, ?, ?, ?, '2026-09-14T10:00:00.000000000Z')`,
			uid(8017), send.ID.String(), fr2.ManagerID.String(), fr2.ManagerIncarnation.String())

		// fr2's manager session, but the request claims fr1 (the message's
		// actual run) — a cross-run ack attempt.
		got, err := store.AckMessage(t.Context(), storevectors.AckMessageCrossRun(fr1.spec.RunID, send.ID, fr2.ManagerID, fr2.ManagerIncarnation))
		if err != nil {
			t.Fatalf("AckMessage() error = %v", err)
		}
		if got.Kind != app.AckRefused || got.Reason != storevectors.AckMessageCrossRunReason {
			t.Fatalf("AckMessage(cross-run) = %+v, want refused/%s", got, storevectors.AckMessageCrossRunReason)
		}
	})

	t.Run("TaskRetry", func(t *testing.T) {
		f := newFeatureFixture(t)
		taskID := f.createFeatureTask(t, 8020, 4, run.TaskNeedsRework)
		seedTerminalAttempt(t, f, taskID, 8021, 1)

		request := storevectors.TaskRetry(f.spec.RunID, f.ManagerID, f.ManagerIncarnation, taskID, "retry-vector")
		for _, want := range []app.WorkflowOutcomeKind{app.WorkflowAccepted, app.WorkflowDuplicate} {
			got, err := f.store.RequestRetry(t.Context(), request)
			if err != nil {
				t.Fatalf("RequestRetry() error = %v", err)
			}
			if got.Outcome != want || got.TaskSeq != 4 || got.AttemptNumber != 2 || got.Reason != storevectors.TaskRetryReason {
				t.Fatalf("RequestRetry() = %+v, want %s t4 attempt 2", got, want)
			}
		}
	})

	t.Run("WorktreeCreateUnknownAttempt", func(t *testing.T) {
		f := newFeatureFixture(t)
		vector := storevectors.WorktreeCreateUnknownAttempt(
			identity.WorktreeID(uid(8030)), f.repositoryID(t), f.spec.RunID, identity.AttemptID(uid(8031)),
		)
		assertWorktreeVectorRefused(t, f, vector, app.ErrNotFound)
	})

	t.Run("WorktreeCreateForeignAttempt", func(t *testing.T) {
		f := newFeatureFixture(t)
		// The second run's worker attempt is uid 7454.
		buildSecondMessagingFixtureAt(t, f.store, f.clock)
		vector := storevectors.WorktreeCreateForeignAttempt(
			identity.WorktreeID(uid(8032)), f.repositoryID(t), f.spec.RunID, identity.AttemptID(uid(7454)),
		)
		assertWorktreeVectorRefused(t, f, vector, app.ErrFenced)
	})

	t.Run("WorktreeLookupByAttempt", func(t *testing.T) {
		f := newFeatureFixture(t)
		task := f.createFeatureTask(t, 8040, 4, run.TaskNeedsRework)
		for i := range 3 {
			seedTerminalAttempt(t, f, task, 8041+i, i+1)
		}
		rowIDs := [4]identity.WorktreeID{
			identity.WorktreeID(uid(8045)), identity.WorktreeID(uid(8046)), identity.WorktreeID(uid(8047)), identity.WorktreeID(uid(8048)),
		}
		vector := storevectors.WorktreeLookupByAttempt(f.repositoryID(t), f.spec.RunID, rowIDs,
			identity.AttemptID(uid(8041)), identity.AttemptID(uid(8042)), identity.AttemptID(uid(8043)))
		for i := range vector.Rows {
			f.inUOW(t, func(uow app.UnitOfWork) {
				if _, err := uow.Worktrees().Create(t.Context(), vector.Rows[i]); err != nil {
					t.Fatalf("Worktrees().Create(row %d): %v", i, err)
				}
			})
		}
		f.inUOW(t, func(uow app.UnitOfWork) {
			wf := workflowRepos(t, uow)
			for attempt, wantPath := range vector.Answers {
				got, revision, err := wf.WorktreeIndex().ByAttempt(t.Context(), attempt)
				if err != nil || got.Path != wantPath || got.AttemptID != attempt || got.BaseCommit != storevectors.WorktreeVectorBaseCommit || revision != 1 {
					t.Errorf("ByAttempt(%s) = %+v rev %d, %v; want the row at %s, revision 1", attempt, got, revision, err, wantPath)
				}
			}
			for _, attempt := range vector.Unanswered {
				if got, _, err := wf.WorktreeIndex().ByAttempt(t.Context(), attempt); !errors.Is(err, app.ErrNotFound) {
					t.Errorf("ByAttempt(%q) = %+v, %v; want ErrNotFound", attempt, got, err)
				}
			}
		})
	})
}

// TestStoreVectorsEndedSession is the real-store half of internal/app's
// TestStoreVectors subtests of the same names: the two caller rules that
// decide liveness from a session row rather than from an address. Each
// case asserts the placement is still current first, so only the
// session's own state can be the refusal's cause.
func TestStoreVectorsEndedSession(t *testing.T) {
	t.Run("TaskCreateEndedManager", func(t *testing.T) {
		f := newFeatureFixture(t)
		endManagerWithSuccessor(t, f, 8301)
		requirePlacementCurrent(t, f, f.ManagerID, f.ManagerIncarnation)
		taskID := identity.TaskID(uid(8302))

		got, err := f.store.CreateTask(t.Context(), storevectors.TaskCreateEndedManager(f.spec.RunID, f.ManagerID, f.ManagerIncarnation, taskID))
		if err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if got.Outcome != app.WorkflowRefused || got.Reason != storevectors.TaskCreateEndedManagerReason || got.Detail != storevectors.TaskCreateEndedManagerDetail {
			t.Fatalf("CreateTask(ended manager) = %+v, want refused/%s %q", got, storevectors.TaskCreateEndedManagerReason, storevectors.TaskCreateEndedManagerDetail)
		}
		if n := countRows(t, f.store, `SELECT COUNT(*) FROM tasks WHERE id = ?`, taskID.String()); n != 0 {
			t.Fatalf("task rows after the refusal = %d, want none", n)
		}
		// The same caller rule gates the other two plan verbs.
		if retry, err := f.store.RequestRetry(t.Context(), app.RetryRequest{
			TaskID: taskID, RunID: f.spec.RunID, Session: f.ManagerID, IncarnationID: f.ManagerIncarnation, Reason: "retry",
		}); err != nil || retry.Outcome != app.WorkflowRefused || retry.Reason != storevectors.TaskCreateEndedManagerReason {
			t.Fatalf("RequestRetry(ended manager) = %+v, %v; want refused/%s", retry, err, storevectors.TaskCreateEndedManagerReason)
		}
		if closed, err := f.store.ClosePlan(t.Context(), app.PlanClose{
			RunID: f.spec.RunID, Session: f.ManagerID, IncarnationID: f.ManagerIncarnation,
		}); err != nil || closed.Outcome != app.WorkflowRefused || closed.Reason != storevectors.TaskCreateEndedManagerReason {
			t.Fatalf("ClosePlan(ended manager) = %+v, %v; want refused/%s", closed, err, storevectors.TaskCreateEndedManagerReason)
		}
	})

	t.Run("ReviewSubmitEndedReviewer", func(t *testing.T) {
		f := newReviewFixture(t)
		endSession(t, f.featureFixture, f.ReviewerID, run.SessionTerminated)
		requirePlacementCurrent(t, f.featureFixture, f.ReviewerID, f.ReviewerIncarnation)

		got, err := f.store.SubmitReview(t.Context(), storevectors.ReviewSubmitEndedReviewer(
			f.spec.RunID, f.ReviewTask, f.AttemptID, f.ReviewerID, f.ReviewerIncarnation,
			identity.ReviewID(uid(8311)), "commit-head", "tree-head", "/state/reasons/8311.md", "reasons-8311",
		))
		if err != nil {
			t.Fatalf("SubmitReview() error = %v", err)
		}
		if got.Kind != app.ReviewStale || got.Reason != storevectors.ReviewSubmitEndedReviewerReason || got.Detail != storevectors.ReviewSubmitEndedReviewerDetail {
			t.Fatalf("SubmitReview(ended reviewer) = %+v, want stale/%s %q", got, storevectors.ReviewSubmitEndedReviewerReason, storevectors.ReviewSubmitEndedReviewerDetail)
		}
		if n := countRows(t, f.store, `SELECT COUNT(*) FROM reviews`); n != 0 {
			t.Fatalf("review rows after the refusal = %d, want none", n)
		}
		f.inUOW(t, func(uow app.UnitOfWork) {
			attempt, _, attErr := uow.Attempts().Get(t.Context(), f.AttemptID)
			if attErr != nil || attempt.State != run.AttemptRunning {
				t.Fatalf("attempt after the refusal = %+v, %v; want still running", attempt, attErr)
			}
		})
	})
}

// assertWorktreeVectorRefused drives one worktree vector through a unit of
// work under the fixture's own lease: Create refuses with want, and
// committing the same unit of work afterwards writes no row.
func assertWorktreeVectorRefused(t *testing.T, f *featureFixture, vector run.Worktree, want error) { //nolint:gocritic // hugeParam: the vector is the port's by-value argument, passed once per subtest.
	t.Helper()
	f.inUOW(t, func(uow app.UnitOfWork) {
		if _, err := uow.Worktrees().Create(t.Context(), vector); !errors.Is(err, want) {
			t.Fatalf("Worktrees().Create() error = %v, want %v", err, want)
		}
	})
	var rows int
	if err := writeDBRow(t, f, `SELECT COUNT(*) FROM worktrees WHERE id = ?`, vector.ID.String()).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("worktree rows after a refused create = %d, %v; want 0", rows, err)
	}
}
