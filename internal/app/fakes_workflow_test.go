package app_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// Compile-time proof that the fakes implement the Phase 3 capability
// surface, mirroring what the real SQLite adapter proves once slice 3
// lands (app_test.HANDOFF: `var _ app.WorkflowRepositories =
// (*unitOfWork)(nil)`).
var (
	_ app.WorkflowRepositories = (*fakeUnitOfWork)(nil)
	_ app.WorkflowReadStore    = (*fakeStore)(nil)
	_ app.MessagingStore       = (*fakeStore)(nil)
	_ app.PlanStore            = (*fakeStore)(nil)
	_ app.ReviewStore          = (*fakeStore)(nil)
)

// This file extends the Phase 2 fakes (fakes_test.go, fakes_uow_test.go,
// fakes_ports_test.go) with the Phase 3 feature-mode capabilities:
// fakeUnitOfWork implements app.WorkflowRepositories (so
// app.RequireWorkflowRepositories succeeds against it, exactly as the real
// SQLite adapter will once slice 3 lands) and fakeStore implements
// app.MessagingStore, app.PlanStore, app.ReviewStore and
// app.WorkflowReadStore. Every method mirrors the real adapter's intended
// contract by calling the SAME domain functions the design names, never
// re-implementing their rules (docs/plan/phase-3-design.md section 7-8).

// receiptOutcome recovers a receipt's typed outcome value, reporting false
// on the impossible case of a mistyped receipt (a bug in this test fake,
// never a reachable production condition, but checked rather than
// discarded).
func receiptOutcome[T any](r requestReceipt) (T, bool) {
	v, ok := r.outcome.(T)
	return v, ok
}

// --- fakeUnitOfWork: app.WorkflowRepositories ---

func (u *fakeUnitOfWork) TaskDependencies() app.TaskDependencyRepository {
	return fakeTaskDependencyRepo{u}
}

func (u *fakeUnitOfWork) TaskIndex() app.TaskIndexRepository { return fakeTaskIndexRepo{u} }

func (u *fakeUnitOfWork) AttemptIndex() app.AttemptIndexRepository { return fakeAttemptIndexRepo{u} }

func (u *fakeUnitOfWork) SessionIndex() app.SessionIndexRepository { return fakeSessionIndexRepo{u} }

func (u *fakeUnitOfWork) Messages() app.MessageRepository { return fakeMessageRepo{u} }

func (u *fakeUnitOfWork) Reviews() app.ReviewRepository { return fakeReviewRepo{u} }

func (u *fakeUnitOfWork) Integrations() app.IntegrationRepository { return fakeIntegrationRepo{u} }

func (u *fakeUnitOfWork) RetryRequests() app.RetryRequestRepository {
	return fakeRetryRequestRepo{u}
}

func (u *fakeUnitOfWork) ManagerSession(_ context.Context, runID identity.RunID) (run.Session, int64, error) {
	for id, staged := range u.sessions {
		if staged.value.RunID == runID && staged.value.Role == run.RoleManager && sessionCurrent(&staged.value) {
			_ = id
			return staged.value, staged.revision, nil
		}
	}
	for i := range u.sessionCreated {
		if u.sessionCreated[i].RunID == runID && u.sessionCreated[i].Role == run.RoleManager && sessionCurrent(&u.sessionCreated[i]) {
			return u.sessionCreated[i], 1, nil
		}
	}
	for id, base := range u.store.Sessions {
		if base.value.RunID != runID || base.value.Role != run.RoleManager || !sessionCurrent(&base.value) {
			continue
		}
		if staged, ok := u.sessions[id]; ok {
			return staged.value, staged.revision, nil
		}
		return base.value, base.revision, nil
	}
	return run.Session{}, 0, fmt.Errorf("%w: no manager session for run %s", app.ErrNotFound, runID)
}

// --- TaskDependencies ---

type fakeTaskDependencyRepo struct{ u *fakeUnitOfWork }

func (r fakeTaskDependencyRepo) ByRun(_ context.Context, runID identity.RunID) ([]run.TaskDependency, error) {
	var out []run.TaskDependency
	for _, e := range r.u.store.TaskDependencies {
		if base, ok := r.u.store.Tasks[e.TaskID]; ok && base.value.RunID == runID {
			out = append(out, e)
		}
	}
	return out, nil
}

// --- TaskIndex ---

type fakeTaskIndexRepo struct{ u *fakeUnitOfWork }

func (r fakeTaskIndexRepo) ByRun(_ context.Context, runID identity.RunID) ([]run.Task, error) {
	seen := map[identity.TaskID]bool{}
	var out []run.Task
	for id, staged := range r.u.tasks {
		if staged.value.RunID != runID {
			continue
		}
		out = append(out, staged.value)
		seen[id] = true
	}
	for i := range r.u.taskCreated {
		t := r.u.taskCreated[i]
		if t.RunID != runID || seen[t.ID] {
			continue
		}
		out = append(out, t)
		seen[t.ID] = true
	}
	for id, base := range r.u.store.Tasks {
		if seen[id] || base.value.RunID != runID {
			continue
		}
		out = append(out, base.value)
	}
	return out, nil
}

func (r fakeTaskIndexRepo) Create(_ context.Context, t run.Task) (int64, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	r.u.taskCreated = append(r.u.taskCreated, t)
	return 1, nil
}

// --- AttemptIndex ---

type fakeAttemptIndexRepo struct{ u *fakeUnitOfWork }

func (r fakeAttemptIndexRepo) ByTask(_ context.Context, task identity.TaskID) ([]run.Attempt, error) {
	seen := map[identity.AttemptID]bool{}
	var out []run.Attempt
	for id, staged := range r.u.attempts {
		if staged.value.TaskID != task {
			continue
		}
		out = append(out, staged.value)
		seen[id] = true
	}
	for i := range r.u.attemptCreated {
		a := r.u.attemptCreated[i]
		if a.TaskID != task || seen[a.ID] {
			continue
		}
		out = append(out, a)
		seen[a.ID] = true
	}
	for id, base := range r.u.store.Attempts {
		if seen[id] || base.value.TaskID != task {
			continue
		}
		out = append(out, base.value)
	}
	return out, nil
}

func (r fakeAttemptIndexRepo) Create(_ context.Context, a run.Attempt) (int64, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	r.u.attemptCreated = append(r.u.attemptCreated, a)
	return 1, nil
}

// --- SessionIndex ---

type fakeSessionIndexRepo struct{ u *fakeUnitOfWork }

func (r fakeSessionIndexRepo) ByRun(_ context.Context, runID identity.RunID) ([]run.Session, error) {
	seen := map[identity.SessionID]bool{}
	var out []run.Session
	for id, staged := range r.u.sessions {
		if staged.value.RunID != runID {
			continue
		}
		out = append(out, staged.value)
		seen[id] = true
	}
	for i := range r.u.sessionCreated {
		s := r.u.sessionCreated[i]
		if s.RunID != runID || seen[s.ID] {
			continue
		}
		out = append(out, s)
		seen[s.ID] = true
	}
	for id, base := range r.u.store.Sessions {
		if seen[id] || base.value.RunID != runID {
			continue
		}
		out = append(out, base.value)
	}
	return out, nil
}

// --- Messages ---

type fakeMessageRepo struct{ u *fakeUnitOfWork }

func (r fakeMessageRepo) Create(_ context.Context, m run.Message) (run.Message, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	m.EnqueueSeq = nextEnqueueSeq(r.u.store, m.RunID, m.Recipient)
	r.u.messagesCreated = append(r.u.messagesCreated, m)
	return m, nil
}

func (r fakeMessageRepo) Get(_ context.Context, id identity.MessageID) (run.Message, error) {
	for i := range r.u.messagesCreated {
		if r.u.messagesCreated[i].ID == id {
			return r.u.messagesCreated[i], nil
		}
	}
	m, ok := r.u.store.Messages[id]
	if !ok {
		return run.Message{}, fmt.Errorf("%w: message %s", app.ErrNotFound, id)
	}
	return m, nil
}

func (r fakeMessageRepo) ByAddress(_ context.Context, runID identity.RunID, address run.Address) ([]run.Message, error) {
	var out []run.Message
	for _, m := range r.u.store.Messages { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		if m.RunID == runID && m.Recipient.Equal(address) {
			out = append(out, m)
		}
	}
	for i := range r.u.messagesCreated {
		m := r.u.messagesCreated[i]
		if m.RunID == runID && m.Recipient.Equal(address) {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EnqueueSeq < out[j].EnqueueSeq })
	return out, nil
}

// --- Reviews ---

type fakeReviewRepo struct{ u *fakeUnitOfWork }

func (r fakeReviewRepo) Latest(_ context.Context, runID identity.RunID) (*run.Review, error) {
	var latest *run.Review
	for _, rev := range r.u.store.Reviews { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		if rev.RunID != runID {
			continue
		}
		if latest == nil || rev.SubmittedAt.After(latest.SubmittedAt) {
			v := rev
			latest = &v
		}
	}
	return latest, nil
}

func (r fakeReviewRepo) ByAttempt(_ context.Context, attempt identity.AttemptID) (*run.Review, error) {
	if rev, ok := r.u.store.Reviews[attempt]; ok {
		return &rev, nil
	}
	return nil, nil //nolint:nilnil // "no accepted review yet" is a valid, common outcome, not an error.
}

// --- Integrations ---

// terminalIntegrationStates is the exhaustive terminal subset of the
// Integration state machine (internal/domain/run/integration.go);
// Current's serial-slot check treats any other state as occupying it.
var terminalIntegrationStates = map[run.IntegrationState]bool{ //nolint:gochecknoglobals // fixed, immutable lookup table for the test fake.
	run.IntegrationIntegrated:  true,
	run.IntegrationConflicted:  true,
	run.IntegrationRolledBack:  true,
	run.IntegrationInterrupted: true,
}

type fakeIntegrationRepo struct{ u *fakeUnitOfWork }

func (r fakeIntegrationRepo) Get(_ context.Context, id identity.IntegrationID) (run.Integration, int64, error) {
	if i, ok := r.u.integrationCreated[id]; ok {
		return i, 1, nil
	}
	if staged, ok := r.u.integrationSaved[id]; ok {
		return staged.value, staged.revision, nil
	}
	base, ok := r.u.store.Integrations[id]
	if !ok {
		return run.Integration{}, 0, fmt.Errorf("%w: integration %s", app.ErrNotFound, id)
	}
	return base.value, base.revision, nil
}

func (r fakeIntegrationRepo) Create(_ context.Context, i run.Integration) (int64, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	if r.u.integrationCreated == nil {
		r.u.integrationCreated = map[identity.IntegrationID]run.Integration{}
	}
	r.u.integrationCreated[i.ID] = i
	return 1, nil
}

func (r fakeIntegrationRepo) Save(_ context.Context, i run.Integration, expectedRevision int64) (int64, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	staged, next, err := stageSave(r.u.integrationSaved, r.u.store.Integrations, i.ID, i, expectedRevision)
	r.u.integrationSaved = staged
	return next, err
}

func (r fakeIntegrationRepo) Current(_ context.Context, runID identity.RunID) (integration run.Integration, revision int64, ok bool, err error) {
	for _, i := range r.u.integrationCreated { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		if i.RunID == runID && !terminalIntegrationStates[i.State] {
			return i, 1, true, nil
		}
	}
	for _, row := range r.u.integrationSaved {
		if row.value.RunID == runID && !terminalIntegrationStates[row.value.State] {
			return row.value, row.revision, true, nil
		}
	}
	for _, base := range r.u.store.Integrations {
		if base.value.RunID == runID && !terminalIntegrationStates[base.value.State] {
			return base.value, base.revision, true, nil
		}
	}
	return run.Integration{}, 0, false, nil
}

func (r fakeIntegrationRepo) ByTask(_ context.Context, task identity.TaskID) ([]run.Integration, error) {
	var out []run.Integration
	for _, base := range r.u.store.Integrations {
		if base.value.TaskID == task {
			out = append(out, base.value)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// --- RetryRequests ---

type fakeRetryRequestRepo struct{ u *fakeUnitOfWork }

func (r fakeRetryRequestRepo) Pending(_ context.Context, runID identity.RunID) ([]app.RetryRequestRecord, error) {
	var out []app.RetryRequestRecord
	for taskID, rec := range r.u.store.RetryRequests {
		if r.u.store.RetryRequestStates[taskID] != app.RetryRequestPending {
			continue
		}
		taskRow, ok := r.u.store.Tasks[taskID]
		if !ok || taskRow.value.RunID != runID {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (r fakeRetryRequestRepo) MarkConsumed(_ context.Context, task identity.TaskID, attemptNumber int) error {
	if r.u.retryConsumed == nil {
		r.u.retryConsumed = map[identity.TaskID]int{}
	}
	r.u.retryConsumed[task] = attemptNumber
	return nil
}

// --- fakeStore: app.MessagingStore ---

// resolveSessionAddressLocked resolves session's logical address: manager
// for a manager session, task:<id> for an implementer/reviewer via its
// attempt's task — lineage-based, so it resolves identically for any
// session ever bound to that task or run, current or historical. Callers
// hold s.mu.
func (s *fakeStore) resolveSessionAddressLocked(sessionID identity.SessionID) (run.Address, bool) {
	sRow, ok := s.Sessions[sessionID]
	if !ok {
		return run.Address{}, false
	}
	switch sRow.value.Role {
	case run.RoleManager:
		return run.ManagerAddress(), true
	case run.RoleImplementer, run.RoleReviewer:
		aRow, ok := s.Attempts[sRow.value.AttemptID]
		if !ok {
			return run.Address{}, false
		}
		return run.TaskAddress(aRow.value.TaskID), true
	default:
		return run.Address{}, false
	}
}

func (s *fakeStore) SendMessage(_ context.Context, send app.MessageSend) (app.MessageOutcome, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()
	const verb = "msg-send"

	digest := sendRequestDigest(send)
	if send.RequestID != "" {
		key := requestReceiptKey{run: send.RunID, verb: verb, requestID: send.RequestID}
		if prior, ok := s.RequestReceipts[key]; ok {
			out, ok := receiptOutcome[app.MessageOutcome](prior)
			if !ok {
				return app.MessageOutcome{Kind: app.MessageMalformed, Detail: "corrupt receipt"}, nil
			}
			if prior.digest == digest {
				// The grammar reports an identical request-ID retry as
				// "duplicate", distinct from the original "accepted" line
				// (docs/plan/phase-3-design.md section 7's worker protocol
				// grammar), even though the underlying entity is unchanged.
				return app.MessageOutcome{Kind: app.MessageDuplicate, MessageID: out.MessageID}, nil
			}
			return app.MessageOutcome{Kind: app.MessageRefused, Detail: "request id reused with different content"}, nil
		}
	}

	rRow, ok := s.Runs[send.RunID]
	if !ok {
		return app.MessageOutcome{Kind: app.MessageMalformed, Detail: "unknown run"}, nil
	}
	// The sender session's OWN run is the only authoritative source for
	// which run it may act in — send.RunID is a caller-supplied field,
	// never trusted on its own (a current session for run A must not be
	// able to supply run B and act against B's messages).
	senderRow, ok := s.Sessions[send.Sender.SessionID]
	if !ok || senderRow.value.RunID != send.RunID {
		return app.MessageOutcome{Kind: app.MessageRefused, Detail: "session does not belong to this run"}, nil
	}
	binding, hasBinding := s.currentBindingLocked(send.Sender.SessionID)
	if !hasBinding || binding.IncarnationID != send.IncarnationID || binding.Superseded {
		return app.MessageOutcome{Kind: app.MessageRefused, Detail: "incarnation is not current"}, nil
	}

	var outcome app.MessageOutcome
	switch send.Kind {
	case run.MessageAnswer:
		outcome = s.acceptSessionAnswerLocked(send, now)
	default:
		if err := run.ValidateSendAddressing(send.SenderAddress, send.Kind, send.Recipient); err != nil {
			outcome = app.MessageOutcome{Kind: app.MessageRefused, Detail: err.Error()}
			break
		}
		if rRow.value.State != run.RunRunning {
			outcome = app.MessageOutcome{Kind: app.MessageRunNotAccept, Detail: "run is not accepting messages"}
			break
		}
		if send.Recipient.Kind == run.AddressTask {
			tRow, ok := s.Tasks[send.Recipient.TaskID]
			if !ok || tRow.value.RunID != send.RunID {
				outcome = app.MessageOutcome{Kind: app.MessageMalformed, Detail: "unknown task"}
				break
			}
			if tRow.value.MailboxClosed {
				outcome = app.MessageOutcome{Kind: app.MessageMailboxClose, Detail: "mailbox is closed"}
				break
			}
		}
		seq := nextEnqueueSeq(s, send.RunID, send.Recipient)
		var msg run.Message
		if send.Kind == run.MessageQuestion {
			msg = run.NewQuestion(send.ID, send.RunID, send.Sender, send.Recipient, send.RelayedFrom, send.RequestID, send.BodyPath, send.BodyDigest, send.BodyBytes, seq, now)
		} else {
			msg = run.NewInfo(send.ID, send.RunID, send.Sender, send.Recipient, send.RequestID, send.BodyPath, send.BodyDigest, send.BodyBytes, seq, now)
		}
		s.Messages[msg.ID] = msg
		outcome = app.MessageOutcome{Kind: app.MessageAccepted, MessageID: msg.ID}
	}

	if send.RequestID != "" && outcome.Kind == app.MessageAccepted {
		s.RequestReceipts[requestReceiptKey{run: send.RunID, verb: verb, requestID: send.RequestID}] = requestReceipt{digest: digest, outcome: outcome}
	}
	return outcome, nil
}

// acceptSessionAnswerLocked handles a session's answer to a question
// addressed to its own logical address (e.g. the manager forwarding an
// answer to a worker's question), via run.AcceptAnswer. destination is
// derived from the question's own sender, never from send.Recipient (an
// answer's destination is never caller-chosen). Callers hold s.mu.
func (s *fakeStore) acceptSessionAnswerLocked(send app.MessageSend, now time.Time) app.MessageOutcome { //nolint:gocritic // hugeParam: MessageSend is a per-call DTO; mirrors the port method's own convention.
	if send.ReplyTo == nil {
		return app.MessageOutcome{Kind: app.MessageMalformed, Detail: "answer requires reply-to"}
	}
	question, ok := s.Messages[*send.ReplyTo]
	if !ok || question.RunID != send.RunID {
		return app.MessageOutcome{Kind: app.MessageMalformed, Detail: "unknown question"}
	}
	destination, ok := s.answerDestinationLocked(question)
	if !ok {
		return app.MessageOutcome{Kind: app.MessageMalformed, Detail: "question originator is not resolvable"}
	}

	var prior *run.Message
	for _, m := range s.Messages { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		if m.Kind == run.MessageAnswer && m.ReplyTo != nil && *m.ReplyTo == question.ID {
			p := m
			prior = &p
			break
		}
	}

	seq := nextEnqueueSeq(s, send.RunID, destination)
	outcomeVal, err := run.AcceptAnswer(question, prior, destination, send.Sender, run.AnswerSubmission{
		ID: send.ID, BodyPath: send.BodyPath, BodyDigest: send.BodyDigest, BodyBytes: send.BodyBytes,
	}, seq, now)
	switch {
	case err == nil:
		s.Messages[outcomeVal.Question.ID] = outcomeVal.Question
		s.Messages[outcomeVal.Answer.ID] = outcomeVal.Answer
		return app.MessageOutcome{Kind: app.MessageAccepted, MessageID: outcomeVal.Answer.ID}
	case errors.Is(err, run.ErrDuplicateAnswer):
		return app.MessageOutcome{Kind: app.MessageDuplicate, MessageID: outcomeVal.Answer.ID}
	case errors.Is(err, run.ErrConflictingAnswer):
		return app.MessageOutcome{Kind: app.MessageConflicting, Detail: err.Error()}
	default:
		return app.MessageOutcome{Kind: app.MessageRefused, Detail: err.Error()}
	}
}

// answerDestinationLocked resolves an answer's destination from the
// referenced question's own sender (never caller-chosen): the sender's
// logical address for a session, or human for a human-authored question —
// though a human never sends a question in this design, so only the
// session case is reachable in practice. Callers hold s.mu.
func (s *fakeStore) answerDestinationLocked(question run.Message) (run.Address, bool) { //nolint:gocritic // hugeParam: Message is passed by value everywhere in this package; mirrors that convention.
	if question.Sender.Kind != run.PrincipalSession {
		return run.Address{}, false
	}
	return s.resolveSessionAddressLocked(question.Sender.SessionID)
}

// reconstructMessageStateLocked returns m with State recomputed from the
// delivery/ack rows: Message.State is not a persisted envelope field
// (internal/domain/run/message.go). Callers hold s.mu.
func (s *fakeStore) reconstructMessageStateLocked(m run.Message) run.Message { //nolint:gocritic // hugeParam: Message is passed by value everywhere in this package; mirrors that convention.
	switch {
	case func() bool { _, ok := s.MessageAcks[m.ID]; return ok }():
		m.State = run.MessageAcknowledged
	case len(s.MessageDeliveries[m.ID]) > 0:
		m.State = run.MessageDelivered
	default:
		m.State = run.MessageQueued
	}
	return m
}

func (s *fakeStore) FetchNextMessage(_ context.Context, fetch app.MessageFetch) (app.MessageDelivery, bool, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()

	// FetchNextMessage has no outcome-kind field to carry a business
	// refusal (an empty fetch's ok-false already means "nothing queued"),
	// so every one of the four caller-supplied identity fields is
	// independently re-derived from the session row itself and compared —
	// never trusted at face value, exactly like SendMessage/AckMessage
	// (ErrMessagingUnauthorized's own doc comment).
	sessionRow, ok := s.Sessions[fetch.SessionID]
	if !ok || sessionRow.value.RunID != fetch.RunID {
		return app.MessageDelivery{}, false, fmt.Errorf("%w: session %s does not belong to run %s", app.ErrMessagingUnauthorized, fetch.SessionID, fetch.RunID)
	}
	binding, hasBinding := s.currentBindingLocked(fetch.SessionID)
	if !hasBinding || binding.IncarnationID != fetch.IncarnationID || binding.Superseded {
		return app.MessageDelivery{}, false, fmt.Errorf("%w: session %s incarnation %s is not current", app.ErrMessagingUnauthorized, fetch.SessionID, fetch.IncarnationID)
	}
	resolvedAddress, ok := s.resolveSessionAddressLocked(fetch.SessionID)
	if !ok || !resolvedAddress.Equal(fetch.Address) {
		return app.MessageDelivery{}, false, fmt.Errorf("%w: session %s does not resolve to address %s", app.ErrMessagingUnauthorized, fetch.SessionID, app.AddressString(fetch.Address))
	}

	var queue []run.Message
	for _, m := range s.Messages { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		if m.RunID == fetch.RunID && m.Recipient.Equal(fetch.Address) {
			queue = append(queue, s.reconstructMessageStateLocked(m))
		}
	}
	next, ok := run.NextDeliverable(queue)
	if !ok {
		// A poll loop must not grow the store: no delivery row, no receipt.
		return app.MessageDelivery{}, false, nil
	}

	s.MessageDeliveries[next.ID] = append(s.MessageDeliveries[next.ID], run.Delivery{
		MessageID: next.ID, SessionID: fetch.SessionID, IncarnationID: fetch.IncarnationID, At: now,
	})

	var origin *identity.MessageID
	if next.Kind == run.MessageAnswer && next.ReplyTo != nil {
		if q, ok := s.Messages[*next.ReplyTo]; ok {
			o := run.ResolveOrigin(q)
			origin = &o
		}
	}
	return app.MessageDelivery{Message: next, Origin: origin}, true, nil
}

func (s *fakeStore) AckMessage(_ context.Context, ack app.MessageAck) (app.MessageAckOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()

	msg, ok := s.Messages[ack.MessageID]
	if !ok || msg.RunID != ack.RunID {
		return app.MessageAckOutcome{Kind: app.AckRefused, Detail: "unknown message"}, nil
	}
	// The acking session's OWN run is the only authoritative source for
	// which run it may act in — ack.RunID is a caller-supplied field,
	// never trusted on its own.
	ackerRow, ok := s.Sessions[ack.SessionID]
	if !ok || ackerRow.value.RunID != ack.RunID {
		return app.MessageAckOutcome{Kind: app.AckRefused, Detail: "session does not belong to this run"}, nil
	}
	msg = s.reconstructMessageStateLocked(msg)

	var priorAck *run.Ack
	if existing, ok := s.MessageAcks[ack.MessageID]; ok {
		p := existing
		priorAck = &p
	}
	// Section 7: a first ack requires a delivery row for the ACKING
	// SESSION ITSELF at its CURRENT incarnation — a delivery served to an
	// earlier, now-superseded incarnation of this same session proves
	// nothing about what the CURRENT incarnation actually saw, exactly
	// like a predecessor session's delivery proves nothing about a
	// successor's. Matching SessionID alone (accepting any historical
	// incarnation) would let a warm-reattached successor incarnation ack
	// a message only a stale incarnation was ever served.
	deliveredToSession := false
	for _, d := range s.MessageDeliveries[ack.MessageID] {
		if d.SessionID == ack.SessionID && d.IncarnationID == ack.IncarnationID {
			deliveredToSession = true
			break
		}
	}
	binding, hasBinding := s.currentBindingLocked(ack.SessionID)
	incarnationCurrent := hasBinding && binding.IncarnationID == ack.IncarnationID && !binding.Superseded

	outcomeVal, err := run.AcceptAck(msg, priorAck, run.AckContext{DeliveredToSession: deliveredToSession, IncarnationCurrent: incarnationCurrent}, run.Ack{SessionID: ack.SessionID, IncarnationID: ack.IncarnationID}, now)
	switch {
	case err != nil:
		return app.MessageAckOutcome{Kind: app.AckRefused, Detail: err.Error()}, nil
	case priorAck != nil:
		return app.MessageAckOutcome{Kind: app.AckDuplicate}, nil
	default:
		s.MessageAcks[ack.MessageID] = outcomeVal.Ack
		return app.MessageAckOutcome{Kind: app.AckAccepted}, nil
	}
}

func (s *fakeStore) AnswerQuestion(_ context.Context, answer app.HumanAnswer) (app.MessageOutcome, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()
	const verb = "answer"

	digest := app.ComputeRequestDigest(verb, answer.RunID.String(), app.AddressString(run.HumanAddress()), answer.QuestionID.String(), answer.BodyDigest)
	if answer.RequestID != "" {
		key := requestReceiptKey{run: answer.RunID, verb: verb, requestID: answer.RequestID}
		if prior, ok := s.RequestReceipts[key]; ok {
			out, ok := receiptOutcome[app.MessageOutcome](prior)
			if !ok {
				return app.MessageOutcome{Kind: app.MessageMalformed, Detail: "corrupt receipt"}, nil
			}
			if prior.digest == digest {
				return app.MessageOutcome{Kind: app.MessageDuplicate, MessageID: out.MessageID}, nil
			}
			return app.MessageOutcome{Kind: app.MessageRefused, Detail: "request id reused with different content"}, nil
		}
	}

	question, ok := s.Messages[answer.QuestionID]
	if !ok || question.RunID != answer.RunID {
		return app.MessageOutcome{Kind: app.MessageMalformed, Detail: "unknown question"}, nil
	}
	if question.Recipient.Kind != run.AddressHuman {
		return app.MessageOutcome{Kind: app.MessageRefused, Detail: "question is not human-addressed"}, nil
	}
	destination, ok := s.answerDestinationLocked(question)
	if !ok {
		return app.MessageOutcome{Kind: app.MessageMalformed, Detail: "question originator is not resolvable"}, nil
	}
	var prior *run.Message
	for _, m := range s.Messages { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		if m.Kind == run.MessageAnswer && m.ReplyTo != nil && *m.ReplyTo == question.ID {
			p := m
			prior = &p
			break
		}
	}

	seq := nextEnqueueSeq(s, answer.RunID, destination)
	outcomeVal, err := run.AcceptAnswer(question, prior, destination, run.HumanPrincipal(), run.AnswerSubmission{
		ID: answer.ID, BodyPath: answer.BodyPath, BodyDigest: answer.BodyDigest, BodyBytes: answer.BodyBytes,
	}, seq, now)
	var outcome app.MessageOutcome
	switch {
	case err == nil:
		s.Messages[outcomeVal.Question.ID] = outcomeVal.Question
		s.Messages[outcomeVal.Answer.ID] = outcomeVal.Answer
		// A human question is never fetched, so its own ack is bundled
		// atomically into its answer's acceptance (section 5/7).
		s.MessageAcks[question.ID] = run.Ack{MessageID: question.ID, At: now}
		outcome = app.MessageOutcome{Kind: app.MessageAccepted, MessageID: outcomeVal.Answer.ID}
	case errors.Is(err, run.ErrDuplicateAnswer):
		outcome = app.MessageOutcome{Kind: app.MessageDuplicate, MessageID: outcomeVal.Answer.ID}
	case errors.Is(err, run.ErrConflictingAnswer):
		outcome = app.MessageOutcome{Kind: app.MessageConflicting, Detail: err.Error()}
	default:
		outcome = app.MessageOutcome{Kind: app.MessageRefused, Detail: err.Error()}
	}
	if answer.RequestID != "" && outcome.Kind == app.MessageAccepted {
		s.RequestReceipts[requestReceiptKey{run: answer.RunID, verb: verb, requestID: answer.RequestID}] = requestReceipt{digest: digest, outcome: outcome}
	}
	return outcome, nil
}

// --- fakeStore: app.PlanStore ---

func (s *fakeStore) CreateTask(_ context.Context, req app.TaskCreate) (app.TaskCreated, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()
	const verb = "task-create"

	depIDs := make([]string, len(req.DependsOn))
	for i, d := range req.DependsOn {
		depIDs[i] = d.String()
	}
	sort.Strings(depIDs)
	payload := append([]string{req.Title, req.InstructionsDigest}, depIDs...)
	digest := app.ComputeRequestDigest(verb, req.RunID.String(), app.AddressString(run.ManagerAddress()), payload...)

	if req.RequestID != "" {
		key := requestReceiptKey{run: req.RunID, verb: verb, requestID: req.RequestID}
		if prior, ok := s.RequestReceipts[key]; ok {
			out, ok := receiptOutcome[app.TaskCreated](prior)
			if !ok {
				return app.TaskCreated{Outcome: app.WorkflowMalformed, Detail: "corrupt receipt"}, nil
			}
			if prior.digest == digest {
				return app.TaskCreated{Outcome: app.WorkflowDuplicate, TaskID: out.TaskID, Seq: out.Seq}, nil
			}
			return app.TaskCreated{Outcome: app.WorkflowRefused, Detail: "request id reused with different content"}, nil
		}
	}

	sRow, ok := s.Sessions[req.Session]
	if !ok || sRow.value.Role != run.RoleManager || sRow.value.RunID != req.RunID {
		return app.TaskCreated{Outcome: app.WorkflowRefused, Detail: "caller is not the run's manager"}, nil
	}
	binding, hasBinding := s.currentBindingLocked(req.Session)
	if !hasBinding || binding.IncarnationID != req.IncarnationID || binding.Superseded {
		return app.TaskCreated{Outcome: app.WorkflowRefused, Detail: "incarnation is not current"}, nil
	}
	rRow, ok := s.Runs[req.RunID]
	if !ok {
		return app.TaskCreated{Outcome: app.WorkflowMalformed, Detail: "unknown run"}, nil
	}
	if err := rRow.value.CanAcceptManagerVerb(); err != nil {
		return app.TaskCreated{Outcome: app.WorkflowRefused, Detail: err.Error()}, nil
	}
	if req.Title == "" || len(req.Title) > app.TaskTitleLimit || req.InstructionsDigest == "" {
		return app.TaskCreated{Outcome: app.WorkflowMalformed, Detail: "invalid title or instructions"}, nil
	}

	edges := make([]run.TaskDependency, 0, len(req.DependsOn))
	for _, dep := range req.DependsOn {
		depRow, ok := s.Tasks[dep]
		if !ok || depRow.value.RunID != req.RunID {
			return app.TaskCreated{Outcome: app.WorkflowRefused, Detail: "dependency is not in this run"}, nil
		}
		edge, err := run.NewTaskDependency(req.ID, dep, now)
		if err != nil {
			return app.TaskCreated{Outcome: app.WorkflowRefused, Detail: err.Error()}, nil
		}
		if err := run.ValidateAcyclic(s.TaskDependencies, edge); err != nil {
			return app.TaskCreated{Outcome: app.WorkflowRefused, Detail: err.Error()}, nil
		}
		edges = append(edges, edge)
	}

	seq := s.nextTaskSeqLocked(req.RunID)
	t := run.NewImplementTask(req.ID, req.RunID, seq, req.Title, req.InstructionsDigest, len(req.DependsOn) > 0, now)
	s.Tasks[req.ID] = &entityRow[run.Task]{value: t, revision: 1}
	s.TaskDependencies = append(s.TaskDependencies, edges...)
	rRow.value = rRow.value.ReopenPlan(now)
	rRow.revision++

	outcome := app.TaskCreated{Outcome: app.WorkflowAccepted, TaskID: req.ID, Seq: seq}
	if req.RequestID != "" {
		s.RequestReceipts[requestReceiptKey{run: req.RunID, verb: verb, requestID: req.RequestID}] = requestReceipt{digest: digest, outcome: outcome}
	}
	return outcome, nil
}

func (s *fakeStore) RequestRetry(_ context.Context, req app.RetryRequest) (app.RetryAccepted, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()
	const verb = "task-retry"

	digest := app.ComputeRequestDigest(verb, req.RunID.String(), app.AddressString(run.ManagerAddress()), req.TaskID.String(), req.Reason)
	if req.RequestID != "" {
		key := requestReceiptKey{run: req.RunID, verb: verb, requestID: req.RequestID}
		if prior, ok := s.RequestReceipts[key]; ok {
			out, ok := receiptOutcome[app.RetryAccepted](prior)
			if !ok {
				return app.RetryAccepted{Outcome: app.WorkflowMalformed, Detail: "corrupt receipt"}, nil
			}
			if prior.digest == digest {
				return app.RetryAccepted{Outcome: app.WorkflowDuplicate, AttemptNumber: out.AttemptNumber}, nil
			}
			return app.RetryAccepted{Outcome: app.WorkflowRefused, Detail: "request id reused with different content"}, nil
		}
	}

	sRow, ok := s.Sessions[req.Session]
	if !ok || sRow.value.Role != run.RoleManager || sRow.value.RunID != req.RunID {
		return app.RetryAccepted{Outcome: app.WorkflowRefused, Detail: "caller is not the run's manager"}, nil
	}
	binding, hasBinding := s.currentBindingLocked(req.Session)
	if !hasBinding || binding.IncarnationID != req.IncarnationID || binding.Superseded {
		return app.RetryAccepted{Outcome: app.WorkflowRefused, Detail: "incarnation is not current"}, nil
	}
	rRow, ok := s.Runs[req.RunID]
	if !ok {
		return app.RetryAccepted{Outcome: app.WorkflowMalformed, Detail: "unknown run"}, nil
	}
	if err := rRow.value.CanAcceptManagerVerb(); err != nil {
		return app.RetryAccepted{Outcome: app.WorkflowRefused, Detail: err.Error()}, nil
	}
	tRow, ok := s.Tasks[req.TaskID]
	if !ok || tRow.value.RunID != req.RunID {
		return app.RetryAccepted{Outcome: app.WorkflowMalformed, Detail: "unknown task"}, nil
	}
	if tRow.value.State != run.TaskNeedsRework {
		return app.RetryAccepted{Outcome: app.WorkflowRefused, Detail: "task is not needs-rework"}, nil
	}
	if s.RetryRequestStates[req.TaskID] == app.RetryRequestPending {
		return app.RetryAccepted{Outcome: app.WorkflowRefused, Detail: "a retry request is already pending for this task"}, nil
	}

	var attempts []run.Attempt
	for _, a := range s.Attempts {
		if a.value.TaskID == req.TaskID {
			attempts = append(attempts, a.value)
		}
	}
	if len(attempts) == 0 {
		return app.RetryAccepted{Outcome: app.WorkflowMalformed, Detail: "task has no attempts"}, nil
	}
	prior := attempts[0]
	for _, a := range attempts[1:] {
		if a.Number > prior.Number {
			prior = a
		}
	}

	limit := 3
	if snap, ok := s.Snapshots[req.RunID]; ok && snap.Workflow.RetryLimit > 0 {
		limit = snap.Workflow.RetryLimit
	}
	nextID := identity.AttemptID(fmt.Sprintf("retry-%s-%d", req.TaskID, prior.Number+1))
	next, err := run.NewRetryAttempt(nextID, prior, limit, now)
	if err != nil {
		reason := "retry limit reached"
		if errors.Is(err, run.ErrRetryNotTerminal) {
			reason = "prior attempt is not terminal"
		}
		return app.RetryAccepted{Outcome: app.WorkflowRefused, Detail: reason}, nil
	}
	s.Attempts[next.ID] = &entityRow[run.Attempt]{value: next, revision: 1}

	taskFrom := tRow.value.State
	reopened, err := tRow.value.Reopen(now)
	if err != nil {
		return app.RetryAccepted{}, err
	}
	reopened = reopened.ReopenMailbox(now)
	tRow.value = reopened
	tRow.revision++
	s.Transitions = append(s.Transitions, app.Transition{
		EntityKind: app.EntityTask, EntityID: req.TaskID.String(),
		From: string(taskFrom), To: string(reopened.State), Reason: "retry accepted", At: now,
	})

	s.RetryRequests[req.TaskID] = app.RetryRequestRecord{
		TaskID: req.TaskID, RequestedBy: req.Session, Reason: req.Reason, RequestID: req.RequestID, CreatedAt: now,
	}
	s.RetryRequestStates[req.TaskID] = app.RetryRequestPending

	outcome := app.RetryAccepted{Outcome: app.WorkflowAccepted, AttemptNumber: next.Number}
	if req.RequestID != "" {
		s.RequestReceipts[requestReceiptKey{run: req.RunID, verb: verb, requestID: req.RequestID}] = requestReceipt{digest: digest, outcome: outcome}
	}
	return outcome, nil
}

func (s *fakeStore) ClosePlan(_ context.Context, req app.PlanClose) (app.PlanCloseResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()
	const verb = "plan-close"

	digest := app.ComputeRequestDigest(verb, req.RunID.String(), app.AddressString(run.ManagerAddress()))
	if req.RequestID != "" {
		key := requestReceiptKey{run: req.RunID, verb: verb, requestID: req.RequestID}
		if prior, ok := s.RequestReceipts[key]; ok {
			if prior.digest == digest {
				return app.PlanCloseResult{Outcome: app.WorkflowDuplicate}, nil
			}
			return app.PlanCloseResult{Outcome: app.WorkflowRefused, Detail: "request id reused with different content"}, nil
		}
	}

	sRow, ok := s.Sessions[req.Session]
	if !ok || sRow.value.Role != run.RoleManager || sRow.value.RunID != req.RunID {
		return app.PlanCloseResult{Outcome: app.WorkflowRefused, Detail: "caller is not the run's manager"}, nil
	}
	binding, hasBinding := s.currentBindingLocked(req.Session)
	if !hasBinding || binding.IncarnationID != req.IncarnationID || binding.Superseded {
		return app.PlanCloseResult{Outcome: app.WorkflowRefused, Detail: "incarnation is not current"}, nil
	}
	rRow, ok := s.Runs[req.RunID]
	if !ok {
		return app.PlanCloseResult{Outcome: app.WorkflowMalformed, Detail: "unknown run"}, nil
	}
	hasImplementTask := false
	for _, t := range s.Tasks {
		if t.value.RunID == req.RunID && t.value.Kind == run.TaskKindImplement {
			hasImplementTask = true
			break
		}
	}
	closed, err := rRow.value.ClosePlan(hasImplementTask, now)
	if err != nil {
		reason := err.Error()
		if errors.Is(err, run.ErrEmptyPlan) {
			reason = "plan has no implement task"
		}
		return app.PlanCloseResult{Outcome: app.WorkflowRefused, Detail: reason}, nil
	}
	rRow.value = closed
	rRow.revision++

	outcome := app.PlanCloseResult{Outcome: app.WorkflowAccepted}
	if req.RequestID != "" {
		s.RequestReceipts[requestReceiptKey{run: req.RunID, verb: verb, requestID: req.RequestID}] = requestReceipt{digest: digest, outcome: outcome}
	}
	return outcome, nil
}

// --- fakeStore: app.ReviewStore ---

func (s *fakeStore) SubmitReview(_ context.Context, submission app.ReviewSubmission) (app.ReviewOutcome, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()

	rRow, ok := s.Runs[submission.RunID]
	tRow := s.Tasks[submission.TaskID]
	aRow, aok := s.Attempts[submission.AttemptID]
	if !ok || tRow == nil || !aok || aRow.value.TaskID != submission.TaskID || tRow.value.RunID != submission.RunID {
		return app.ReviewOutcome{Kind: app.ReviewMalformed, Detail: "attempt/task/run do not agree"}, nil
	}

	var prior *run.Review
	if existing, ok := s.Reviews[submission.AttemptID]; ok {
		p := existing
		prior = &p
	}

	binding, hasBinding := s.currentBindingLocked(submission.Session)
	incarnationCurrent := hasBinding && binding.IncarnationID == submission.IncarnationID && !binding.Superseded
	claim, hasClaim := s.LaunchClaims[submission.IncarnationID]
	launchClaimSettled := hasClaim && claim.State == app.LaunchClaimExeced
	mailboxClear := s.mailboxClearLocked(submission.TaskID)

	ctx := run.ReviewAcceptanceContext{IncarnationCurrent: incarnationCurrent, LaunchClaimSettled: launchClaimSettled, MailboxClear: mailboxClear}
	outcomeVal, err := run.AcceptVerdict(rRow.value, tRow.value, aRow.value, prior, ctx, run.ReviewSubmission{
		ID: submission.ID, SubjectCommitOID: submission.SubjectCommitOID, SubjectTreeOID: submission.SubjectTreeOID,
		Verdict: submission.Verdict, ReasonsDigest: submission.ReasonsDigest,
	}, now)

	switch {
	case err == nil:
		s.Reviews[submission.AttemptID] = outcomeVal.Review
		rRow.value = outcomeVal.Run
		rRow.revision++
		tRow.value = outcomeVal.Task
		tRow.revision++
		aRow.value = outcomeVal.Attempt
		aRow.revision++
		return app.ReviewOutcome{Kind: app.ReviewAccepted, ReviewID: submission.ID}, nil
	case errors.Is(err, run.ErrDuplicateResult):
		return app.ReviewOutcome{Kind: app.ReviewDuplicate, ReviewID: outcomeVal.Review.ID}, nil
	case errors.Is(err, run.ErrConflictingResult):
		return app.ReviewOutcome{Kind: app.ReviewConflicting, ReviewID: outcomeVal.Review.ID}, nil
	case errors.Is(err, run.ErrTransientNotRunning), errors.Is(err, run.ErrMailboxNotClear):
		return app.ReviewOutcome{Kind: app.ReviewTransient, Detail: err.Error()}, nil
	default:
		return app.ReviewOutcome{Kind: app.ReviewStale, Detail: err.Error()}, nil
	}
}

// mailboxClearLocked reports whether task's mailbox has no queued or
// delivered-unacknowledged message: the section 5 drain-then-submit
// contract. Callers hold s.mu.
func (s *fakeStore) mailboxClearLocked(task identity.TaskID) bool {
	address := run.TaskAddress(task)
	for _, m := range s.Messages { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		if !m.Recipient.Equal(address) {
			continue
		}
		if _, acked := s.MessageAcks[m.ID]; acked {
			continue
		}
		return false
	}
	return true
}

// --- fakeStore: app.WorkflowReadStore ---

func (s *fakeStore) LoadSessionLaunchContext(_ context.Context, runID identity.RunID, session identity.SessionID) (app.SessionLaunchContext, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sRow, ok := s.Sessions[session]
	if !ok || sRow.value.RunID != runID {
		return app.SessionLaunchContext{}, fmt.Errorf("%w: session %s", app.ErrNotFound, session)
	}
	rRow, ok := s.Runs[runID]
	if !ok {
		return app.SessionLaunchContext{}, fmt.Errorf("%w: run %s", app.ErrNotFound, runID)
	}
	out := app.SessionLaunchContext{
		Snapshot: s.Snapshots[runID], Harness: sRow.value.Harness, Session: sRow.value,
		AttemptID: sRow.value.AttemptID, StopRequested: rRow.value.StopRequested,
	}
	if binding, ok := s.currentBindingLocked(session); ok {
		out.IncarnationID = binding.IncarnationID
		if claim, ok := s.LaunchClaims[binding.IncarnationID]; ok {
			c := claim
			out.Claim = &c
		}
	} else {
		for _, op := range s.Operations { //nolint:gocritic // rangeValCopy: test fake; the journal is small and read-only here.
			if op.State != app.OperationPending || op.Kind != app.OpPaneOpen {
				continue
			}
			intent, ok := decodePaneOpenIntent(op.Intent)
			if !ok || identity.SessionID(intent.SessionID) != session {
				continue
			}
			out.IncarnationID = identity.IncarnationID(intent.IncarnationID)
			if claim, ok := s.LaunchClaims[out.IncarnationID]; ok {
				c := claim
				out.Claim = &c
			}
		}
	}
	return out, nil
}

func (s *fakeStore) LoadMessagingContext(_ context.Context, session identity.SessionID) (app.MessagingContext, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sRow, ok := s.Sessions[session]
	if !ok {
		return app.MessagingContext{}, fmt.Errorf("%w: session %s", app.ErrNotFound, session)
	}
	address, ok := s.resolveSessionAddressLocked(session)
	if !ok {
		return app.MessagingContext{}, fmt.Errorf("app_test: session %s has no resolvable address", session)
	}
	rRow, ok := s.Runs[sRow.value.RunID]
	if !ok {
		return app.MessagingContext{}, fmt.Errorf("%w: run %s", app.ErrNotFound, sRow.value.RunID)
	}
	out := app.MessagingContext{RunID: sRow.value.RunID, Address: address, StopRequested: rRow.value.StopRequested}
	if binding, ok := s.currentBindingLocked(session); ok {
		out.IncarnationID = binding.IncarnationID
	}
	return out, nil
}

func (s *fakeStore) LoadMessageDetail(_ context.Context, runID identity.RunID, messageID identity.MessageID) (app.MessageDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg, ok := s.Messages[messageID]
	if !ok || msg.RunID != runID {
		return app.MessageDetail{}, fmt.Errorf("%w: message %s", app.ErrNotFound, messageID)
	}
	detail := app.MessageDetail{Message: s.reconstructMessageStateLocked(msg)}
	detail.Deliveries = append(detail.Deliveries, s.MessageDeliveries[messageID]...)
	if ack, ok := s.MessageAcks[messageID]; ok {
		a := ack
		detail.Ack = &a
	}
	return detail, nil
}

// --- fakeStore: RunDetail's Phase 3 status-surface assembly (section 3/7) ---

// tasksSummaryLocked returns runID's feature-mode task table, unsorted;
// callers sort as their rendering requires. WorktreePath is always "":
// Worktree (internal/domain/run) is still the Phase 2 one-row-per-run
// shape (no attempt linkage), so a per-attempt worktree path is not yet
// resolvable from any repository this slice owns. Per the manager: slice
// 3's migration adds attempt_id and base_commit to worktrees, at which
// point this field resolves for real — left "" here deliberately rather
// than guessed. Callers hold s.mu.
func (s *fakeStore) tasksSummaryLocked(runID identity.RunID) []app.TaskSummary {
	var out []app.TaskSummary
	for id, row := range s.Tasks {
		if row.value.RunID != runID {
			continue
		}
		summary := app.TaskSummary{TaskID: id, Seq: row.value.Seq, Kind: row.value.Kind, State: row.value.State}
		for _, dep := range s.TaskDependencies {
			if dep.TaskID == id {
				summary.DependsOn = append(summary.DependsOn, dep.PrerequisiteID)
			}
		}
		for _, a := range s.Attempts {
			if a.value.TaskID == id {
				summary.AttemptCount++
			}
		}
		out = append(out, summary)
	}
	return out
}

// latestIntegrationLocked returns runID's most recently CREATED
// integration row, if any — "the integration head" hop status renders
// (section 3); reading exactly what is recorded, never resolving the live
// git ref (2b's integration-operations concern). Callers hold s.mu.
func (s *fakeStore) latestIntegrationLocked(runID identity.RunID) (run.Integration, bool) {
	var latest *run.Integration
	for _, row := range s.Integrations {
		if row.value.RunID != runID {
			continue
		}
		if latest == nil || row.value.CreatedAt.After(latest.CreatedAt) {
			v := row.value
			latest = &v
		}
	}
	if latest == nil {
		return run.Integration{}, false
	}
	return *latest, true
}

// guardShortfallsLocked assembles a GuardContext from exactly the evidence
// this fake tracks and evaluates it against EvaluateReadiness: PlanClosed
// and every implement task are real; the head object IDs come from the
// most recently INTEGRATED integration row (a conflicted, checking or
// merging one has not moved the branch) — both HeadCommitOID and
// HeadTreeOID collapse to the same MergeCommitOID, since Integration
// carries no separate tree object id and none of this fake's tests need
// that distinction; LatestCheck is always nil, since no Phase "2a" port
// tracks a combined-candidate check receipt yet — so ShortfallCheckMissing
// is reported whenever a head exists, honestly reflecting that no such
// evidence has been recorded rather than guessing one way or the other.
// Returns nil for a solo run, matching RunDetail.GuardShortfalls' doc.
// Callers hold s.mu.
func (s *fakeStore) guardShortfallsLocked(runID identity.RunID) []run.GuardShortfall {
	if !s.Snapshots[runID].Workflow.Feature() {
		return nil
	}
	rRow, ok := s.Runs[runID]
	if !ok {
		return nil
	}
	var implement []run.Task
	for _, row := range s.Tasks {
		if row.value.RunID == runID && row.value.Kind == run.TaskKindImplement {
			implement = append(implement, row.value)
		}
	}
	ctx := run.GuardContext{PlanClosed: rRow.value.PlanClosed, ImplementTasks: implement}

	var latestIntegrated *run.Integration
	for _, row := range s.Integrations {
		if row.value.RunID != runID || row.value.State != run.IntegrationIntegrated {
			continue
		}
		if latestIntegrated == nil || row.value.UpdatedAt.After(latestIntegrated.UpdatedAt) {
			v := row.value
			latestIntegrated = &v
		}
	}
	if latestIntegrated != nil {
		ctx.HeadCommitOID = latestIntegrated.MergeCommitOID
		ctx.HeadTreeOID = latestIntegrated.MergeCommitOID
	}

	var latestReview *run.Review
	for _, r := range s.Reviews { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here.
		if r.RunID != runID {
			continue
		}
		if latestReview == nil || r.SubmittedAt.After(latestReview.SubmittedAt) {
			v := r
			latestReview = &v
		}
	}
	ctx.LatestReview = latestReview

	_, missing := run.EvaluateReadiness(ctx)
	return missing
}

// addressLiveLocked reports whether address's own session is currently
// non-terminal: the run's current manager session for AddressManager, the
// address's task's most recent attempt's current session for AddressTask,
// and always true for AddressHuman (no session to go stale) — section 7's
// "address's session is live" qualifier on the attention condition.
// Callers hold s.mu.
func (s *fakeStore) addressLiveLocked(runID identity.RunID, address run.Address) bool {
	switch address.Kind {
	case run.AddressHuman:
		return true
	case run.AddressManager:
		for _, row := range s.Sessions {
			if row.value.RunID == runID && row.value.Role == run.RoleManager &&
				row.value.State != run.SessionTerminated && row.value.State != run.SessionLost {
				return true
			}
		}
		return false
	case run.AddressTask:
		var latest *run.Attempt
		for _, row := range s.Attempts {
			if row.value.TaskID != address.TaskID {
				continue
			}
			if latest == nil || row.value.Number > latest.Number {
				v := row.value
				latest = &v
			}
		}
		if latest == nil {
			return false
		}
		for _, row := range s.Sessions {
			if row.value.AttemptID == latest.ID &&
				row.value.State != run.SessionTerminated && row.value.State != run.SessionLost {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// mailboxesLocked assembles section 7's per-address status surface: one
// entry per address holding a non-empty queue or an unacknowledged
// in-flight message, ages computed against now. Attention is gated on the
// run's frozen [messages] attention_after threshold (a solo run's zero
// WorkflowSnapshot has a zero threshold, so Attention is always false) and
// on the single oldest pending item at that address, whichever of the
// in-flight age or the oldest queued age is larger. Callers hold s.mu.
func (s *fakeStore) mailboxesLocked(runID identity.RunID, now time.Time) []app.MailboxStatus {
	byAddress := map[string][]run.Message{}
	addressOf := map[string]run.Address{}
	var order []string
	for _, m := range s.Messages { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here.
		if m.RunID != runID {
			continue
		}
		key := app.AddressString(m.Recipient)
		if _, seen := addressOf[key]; !seen {
			order = append(order, key)
			addressOf[key] = m.Recipient
		}
		byAddress[key] = append(byAddress[key], s.reconstructMessageStateLocked(m))
	}
	sort.Strings(order)
	threshold := s.Snapshots[runID].Workflow.MessageAttention

	var out []app.MailboxStatus
	for _, key := range order {
		address := addressOf[key]
		status := app.MailboxStatus{Address: address, AddressLive: s.addressLiveLocked(runID, address)}
		var oldestQueued time.Time
		for _, m := range byAddress[key] { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here.
			switch m.State {
			case run.MessageDelivered:
				deliveries := s.MessageDeliveries[m.ID]
				if len(deliveries) == 0 {
					continue
				}
				latest := deliveries[0].At
				for _, d := range deliveries[1:] {
					if d.At.After(latest) {
						latest = d.At
					}
				}
				status.InFlight = &app.InFlightMessage{MessageID: m.ID, Age: now.Sub(latest)}
			case run.MessageQueued:
				status.QueuedCount++
				if oldestQueued.IsZero() || m.CreatedAt.Before(oldestQueued) {
					oldestQueued = m.CreatedAt
				}
			}
		}
		if status.InFlight == nil && status.QueuedCount == 0 {
			continue
		}
		if status.QueuedCount > 0 {
			status.OldestQueuedAge = now.Sub(oldestQueued)
		}
		pendingAge := status.OldestQueuedAge
		if status.InFlight != nil && status.InFlight.Age > pendingAge {
			pendingAge = status.InFlight.Age
		}
		status.Attention = status.AddressLive && threshold > 0 && pendingAge > threshold
		out = append(out, status)
	}
	return out
}

// pendingQuestionsLocked returns every unanswered human-addressed
// question for runID, oldest first. A human question's own ack is bundled
// atomically into its answer's acceptance (AnswerQuestion above), so
// s.MessageAcks alone decides "answered". Callers hold s.mu.
func (s *fakeStore) pendingQuestionsLocked(runID identity.RunID, now time.Time) []app.PendingQuestion {
	var questions []run.Message
	for _, m := range s.Messages { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here.
		if m.RunID == runID && m.Recipient.Kind == run.AddressHuman && m.Kind == run.MessageQuestion {
			questions = append(questions, m)
		}
	}
	sort.Slice(questions, func(i, j int) bool { return questions[i].EnqueueSeq < questions[j].EnqueueSeq })
	var out []app.PendingQuestion
	for _, q := range questions { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here.
		if _, answered := s.MessageAcks[q.ID]; answered {
			continue
		}
		out = append(out, app.PendingQuestion{MessageID: q.ID, BodyPath: q.BodyPath, Age: now.Sub(q.CreatedAt)})
	}
	return out
}

// sendRequestDigest computes the section 7 request digest for a
// hop msg send, normalizing an answer's payload (question uuid + body
// digest) separately from an ordinary question/info send.
func sendRequestDigest(send app.MessageSend) string { //nolint:gocritic // hugeParam: MessageSend is a per-call DTO; this helper runs once per SendMessage call, never a hot loop.
	senderAddr := app.AddressString(send.SenderAddress)
	if send.Kind == run.MessageAnswer {
		replyTo := ""
		if send.ReplyTo != nil {
			replyTo = send.ReplyTo.String()
		}
		return app.ComputeRequestDigest("msg-send", send.RunID.String(), senderAddr, replyTo, send.BodyDigest)
	}
	replyTo, relayOf := "", ""
	if send.ReplyTo != nil {
		replyTo = send.ReplyTo.String()
	}
	if send.RelayedFrom != nil {
		relayOf = send.RelayedFrom.String()
	}
	return app.ComputeRequestDigest("msg-send", send.RunID.String(), senderAddr, app.AddressString(send.Recipient), string(send.Kind), replyTo, relayOf, send.BodyDigest)
}
