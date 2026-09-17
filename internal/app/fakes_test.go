package app_test

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// leaseTTL is the fake store's fixed heartbeat TTL, matching the design's
// stated 30s (docs/plan/phase-2-design.md section 4); tests use a fake
// clock, so the literal duration only has to be internally consistent.
const leaseTTL = 30 * time.Second

// fakeClock is a deterministic, manually advanced Clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeIDs is a deterministic IDGenerator: a fixed sequence of canonical
// UUIDs, panicking if exhausted so a test's identity budget is explicit.
type fakeIDs struct {
	mu   sync.Mutex
	next int
}

func newFakeIDs() *fakeIDs { return &fakeIDs{} }

func (g *fakeIDs) NewID() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next++
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", g.next, g.next)
}

// leaseRow is the fake store's lease bookkeeping for one run.
type leaseRow struct {
	lease   app.Lease
	held    bool
	repoID  identity.RepositoryID
	created bool
}

// entityRow pairs a stored domain value with its revision.
type entityRow[T any] struct {
	value    T
	revision int64
}

// fakeStore is a handwritten, in-memory implementation of StateStore,
// ReadStore and SubmissionStore over shared state, mirroring how one
// SQLite database backs all three in production. Exported fields let
// tests seed or inspect state directly, bypassing lease fencing, to set up
// scenarios without replaying an entire lifecycle through the use cases.
type fakeStore struct {
	mu sync.Mutex

	clock interface{ Now() time.Time }

	// HeartbeatHook, when set, runs at the start of every Heartbeat call,
	// outside the store lock: a test can take the lease over from another
	// controller at exactly the pre-dispatch revalidation point.
	HeartbeatHook func()

	// CommitHook, when set, runs at the start of every unit-of-work
	// Commit, outside the store lock, with the committing unit of work: a
	// test can race a concurrent write against exactly one transaction's
	// commit window.
	CommitHook func(u *fakeUnitOfWork)

	// LoadRunStatusHook, when set, runs after every LoadRunStatus returns
	// (outside the store lock): a test can land a write in exactly the
	// window between a lease-free entry read and the entry transaction.
	LoadRunStatusHook func()

	// openUnitsOfWork counts units of work begun but not yet committed or
	// rolled back. The port fakes consult it through
	// refuseInsideTransaction: an external call made while a store
	// transaction is open violates the section 4 transaction rule and
	// fails the call.
	openUnitsOfWork int

	repoByRoot map[string]identity.RepositoryID
	seqByRepo  map[identity.RepositoryID]int

	Runs      map[identity.RunID]*entityRow[run.Run]
	Snapshots map[identity.RunID]app.RunSnapshot
	Briefs    map[identity.RunID]string
	Tasks     map[identity.TaskID]*entityRow[run.Task]
	Attempts  map[identity.AttemptID]*entityRow[run.Attempt]
	Sessions  map[identity.SessionID]*entityRow[run.Session]
	Worktrees map[identity.WorktreeID]*entityRow[run.Worktree]
	Bindings  map[identity.SessionID][]run.RuntimeBinding
	Results   map[identity.AttemptID]run.Result
	Artifacts []run.Artifact

	// worktreeInsertOrder records each committed worktree row's insertion
	// sequence (worktreeInsertSeq is the last one assigned): the fake's
	// counterpart of the real store's rowid, which the by-attempt worktree
	// lookup orders by. A row seeded directly into Worktrees has none and
	// sorts oldest.
	worktreeInsertOrder map[identity.WorktreeID]int
	worktreeInsertSeq   int

	LaunchClaims    map[identity.IncarnationID]app.LaunchClaim
	CheckExecClaims map[identity.OperationID]app.CheckExecClaim
	Operations      map[identity.OperationID]app.Operation
	CheckRequests   map[identity.ResultID]app.CheckRequest
	Transitions     []app.Transition
	Submissions     []app.SubmissionOutcome

	Leases map[identity.RunID]*leaseRow

	// TaskByRun and AttemptByTask let the fake resolve Phase 2's
	// one-task/one-attempt-per-run invariant without a dedicated port.
	TaskByRun    map[identity.RunID]identity.TaskID
	AttemptByRun map[identity.RunID]identity.AttemptID

	// WorktreeCreateErr, when set, is returned by the next CreateWorktree
	// call and then cleared. Runtime hooks like this live on fakeRuntime;
	// this one lives here only for tests that assert on store-recorded
	// operation outcomes without a full fakeRuntime wiring.

	// Phase 3 feature-mode state, additive to the Phase 2 shape above.
	// TaskDependencies is every persisted edge, immutable once appended.
	TaskDependencies []run.TaskDependency
	// Messages is every immutable envelope by id; MessageDeliveries and
	// MessageAcks are its append-only delivery/ack rows.
	Messages          map[identity.MessageID]run.Message
	MessageDeliveries map[identity.MessageID][]run.Delivery
	MessageAcks       map[identity.MessageID]run.Ack

	// Reviews is the run's accepted verdicts, at most one per attempt.
	Reviews map[identity.AttemptID]run.Review

	// Integrations is the controller's own integration journal.
	Integrations map[identity.IntegrationID]*entityRow[run.Integration]

	// RetryRequests holds pending and consumed retry requests, keyed by
	// task (UNIQUE(task_id) WHERE state='pending' in the real schema, so
	// a task's later retry after consumption reuses the same key).
	RetryRequests map[identity.TaskID]app.RetryRequestRecord
	// RetryRequestStates tracks each request's own small lifecycle,
	// separately from RetryRequestRecord (a plain read value).
	RetryRequestStates map[identity.TaskID]app.RetryRequestState
	// taskSeqByRun assigns dense per-run task sequence numbers, mirroring
	// seqByRepo's role for run sequences.
	taskSeqByRun map[identity.RunID]int

	// WorktreesRetiredAt is the per-run worktrees-retired fact (migration
	// 004's runs.worktrees_retired_at), absent until set, set once.
	WorktreesRetiredAt map[identity.RunID]time.Time

	// RequestReceipts is the shared (run, verb, requestID) acceptance-key
	// idempotency store for every request-ID-bearing verb (send, answer,
	// task-create, retry, plan-close): the digest of the accepted
	// request's content and its outcome, so an identical retry (same ID,
	// same digest) returns the ORIGINAL outcome and a reused ID with
	// different content is refused. Ack has no request-ID (its own
	// message-id-keyed duplicate-ack rule already covers idempotency);
	// review submission has its own per-attempt digest idempotency
	// (ReviewStore.SubmitReview, no requestID field).
	RequestReceipts map[requestReceiptKey]requestReceipt
}

// requestReceiptKey identifies one (run, verb, requestID) acceptance key.
type requestReceiptKey struct {
	run       identity.RunID
	verb      string
	requestID string
}

// requestReceipt is one accepted request-ID-bearing verb's remembered
// digest and outcome.
type requestReceipt struct {
	digest  string
	outcome any
}

func newFakeStore(clock interface{ Now() time.Time }) *fakeStore {
	return &fakeStore{
		clock:           clock,
		repoByRoot:      map[string]identity.RepositoryID{},
		seqByRepo:       map[identity.RepositoryID]int{},
		Runs:            map[identity.RunID]*entityRow[run.Run]{},
		Snapshots:       map[identity.RunID]app.RunSnapshot{},
		Briefs:          map[identity.RunID]string{},
		Tasks:           map[identity.TaskID]*entityRow[run.Task]{},
		Attempts:        map[identity.AttemptID]*entityRow[run.Attempt]{},
		Sessions:        map[identity.SessionID]*entityRow[run.Session]{},
		Worktrees:       map[identity.WorktreeID]*entityRow[run.Worktree]{},
		Bindings:        map[identity.SessionID][]run.RuntimeBinding{},
		Results:         map[identity.AttemptID]run.Result{},
		LaunchClaims:    map[identity.IncarnationID]app.LaunchClaim{},
		CheckExecClaims: map[identity.OperationID]app.CheckExecClaim{},
		Operations:      map[identity.OperationID]app.Operation{},
		CheckRequests:   map[identity.ResultID]app.CheckRequest{},
		Leases:          map[identity.RunID]*leaseRow{},
		TaskByRun:       map[identity.RunID]identity.TaskID{},
		AttemptByRun:    map[identity.RunID]identity.AttemptID{},

		Messages:           map[identity.MessageID]run.Message{},
		MessageDeliveries:  map[identity.MessageID][]run.Delivery{},
		MessageAcks:        map[identity.MessageID]run.Ack{},
		Reviews:            map[identity.AttemptID]run.Review{},
		Integrations:       map[identity.IntegrationID]*entityRow[run.Integration]{},
		RetryRequests:      map[identity.TaskID]app.RetryRequestRecord{},
		RetryRequestStates: map[identity.TaskID]app.RetryRequestState{},
		RequestReceipts:    map[requestReceiptKey]requestReceipt{},
		taskSeqByRun:       map[identity.RunID]int{},
		WorktreesRetiredAt: map[identity.RunID]time.Time{},

		worktreeInsertOrder: map[identity.WorktreeID]int{},
	}
}

// nextTaskSeqLocked assigns the next dense per-run task sequence number.
// Callers hold s.mu.
func (s *fakeStore) nextTaskSeqLocked(runID identity.RunID) int {
	s.taskSeqByRun[runID]++
	return s.taskSeqByRun[runID]
}

// nextEnqueueSeq is the next durable per-(run, recipient address) message
// sequence number — the FIFO authority (section 7) — computed exactly as
// the adapter does: one more than the highest sequence any stored message
// to that address carries, or any of staged (a unit of work's own
// uncommitted messages). Reading it consumes nothing, so a decision that
// refuses leaves no gap, and a message seeded directly into s.Messages
// counts like any other. Callers hold s.mu.
func nextEnqueueSeq(s *fakeStore, runID identity.RunID, address run.Address, staged ...run.Message) int {
	highest := 0
	consider := func(m *run.Message) {
		if m.RunID == runID && m.Recipient.Equal(address) && m.EnqueueSeq > highest {
			highest = m.EnqueueSeq
		}
	}
	for id := range s.Messages {
		m := s.Messages[id]
		consider(&m)
	}
	for i := range staged {
		consider(&staged[i])
	}
	return highest + 1
}

// --- StateStore ---

// InitializeRun mirrors the SQLite adapter's bootstrap transaction. A
// feature-mode spec reproduces the real store's feature shape — run,
// snapshot, MANAGER session (no attempt, no parent, the pre-assigned
// native reference, reserved) and lease, with no task, attempt or
// worktree — and its two refusals: app.ValidateFeatureRunSpec before
// anything, and app.RequireIntegrationBranchForSequence against the
// sequence this call would assign, with nothing (not even the repository
// row or the sequence counter) mutated on either refusal, exactly as the
// real transaction rolls back.
func (s *fakeStore) InitializeRun(_ context.Context, spec app.NewRunSpec) (identity.RunID, app.Lease, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	feature := spec.Snapshot.Workflow.Feature()
	if feature {
		if err := app.ValidateFeatureRunSpec(&spec); err != nil {
			return "", app.Lease{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	repoID, ok := s.repoByRoot[spec.RepositoryRoot]
	if !ok {
		repoID = identity.RepositoryID(fmt.Sprintf("repo-%d", len(s.repoByRoot)+1))
	}
	seq := s.seqByRepo[repoID] + 1
	if feature {
		if err := app.RequireIntegrationBranchForSequence(&spec.Snapshot.Workflow, seq); err != nil {
			return "", app.Lease{}, err
		}
	}
	s.repoByRoot[spec.RepositoryRoot] = repoID
	s.seqByRepo[repoID] = seq

	r := run.NewRun(spec.RunID, repoID, seq, spec.BriefDigest, spec.Now)
	s.Runs[spec.RunID] = &entityRow[run.Run]{value: r, revision: 1}
	s.Snapshots[spec.RunID] = spec.Snapshot
	s.Briefs[spec.RunID] = spec.Brief

	if feature {
		manager := run.NewManagerSession(spec.SessionID, spec.RunID, spec.Harness, spec.Now)
		if spec.NativeSessionRef != "" {
			var assignErr error
			manager, assignErr = manager.AssignNativeRef(spec.NativeSessionRef, run.NativeRefAssigned, spec.Now)
			if assignErr != nil {
				return "", app.Lease{}, assignErr
			}
		}
		s.Sessions[spec.SessionID] = &entityRow[run.Session]{value: manager, revision: 1}
		lease := app.Lease{Run: spec.RunID, ControllerID: spec.ControllerID, Generation: 1, ExpiresAt: spec.Now.Add(leaseTTL)}
		s.Leases[spec.RunID] = &leaseRow{lease: lease, held: true, repoID: repoID, created: true}
		return spec.RunID, lease, nil
	}

	t := run.NewTask(spec.TaskID, spec.RunID, spec.InstructionsDigest, spec.Now)
	s.Tasks[spec.TaskID] = &entityRow[run.Task]{value: t, revision: 1}
	s.TaskByRun[spec.RunID] = spec.TaskID

	a, err := run.NewAttempt(spec.AttemptID, spec.TaskID, 1, spec.Now)
	if err != nil {
		return "", app.Lease{}, err
	}
	s.Attempts[spec.AttemptID] = &entityRow[run.Attempt]{value: a, revision: 1}
	s.AttemptByRun[spec.RunID] = spec.AttemptID

	sess := run.NewSession(spec.SessionID, spec.RunID, spec.AttemptID, spec.Harness, spec.Now)
	if spec.NativeSessionRef != "" {
		var assignErr error
		sess, assignErr = sess.AssignNativeRef(spec.NativeSessionRef, run.NativeRefAssigned, spec.Now)
		if assignErr != nil {
			return "", app.Lease{}, assignErr
		}
	}
	s.Sessions[spec.SessionID] = &entityRow[run.Session]{value: sess, revision: 1}

	lease := app.Lease{Run: spec.RunID, ControllerID: spec.ControllerID, Generation: 1, ExpiresAt: spec.Now.Add(leaseTTL)}
	s.Leases[spec.RunID] = &leaseRow{lease: lease, held: true, repoID: repoID, created: true}

	return spec.RunID, lease, nil
}

func (s *fakeStore) AcquireLease(_ context.Context, runID identity.RunID, controllerID string) (app.Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.Leases[runID]
	if !ok {
		return app.Lease{}, fmt.Errorf("%w: run %s has no lease row", app.ErrNotFound, runID)
	}
	now := s.clock.Now()
	if row.held && row.lease.ExpiresAt.After(now) {
		return app.Lease{}, app.ErrLeaseHeld
	}
	row.lease = app.Lease{Run: runID, ControllerID: controllerID, Generation: row.lease.Generation + 1, ExpiresAt: now.Add(leaseTTL)}
	row.held = true
	return row.lease, nil
}

func (s *fakeStore) matchesHeldLease(lease app.Lease) bool {
	row, ok := s.Leases[lease.Run]
	if !ok || !row.held {
		return false
	}
	return row.lease.ControllerID == lease.ControllerID && row.lease.Generation == lease.Generation
}

func (s *fakeStore) Heartbeat(ctx context.Context, lease app.Lease) error {
	if s.HeartbeatHook != nil {
		s.HeartbeatHook()
	}
	// The real heartbeat is a write transaction bound to ctx.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("app_test: heartbeat: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.matchesHeldLease(lease) {
		return app.ErrFenced
	}
	row := s.Leases[lease.Run]
	row.lease.ExpiresAt = s.clock.Now().Add(leaseTTL)
	return nil
}

func (s *fakeStore) ReleaseLease(_ context.Context, lease app.Lease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.matchesHeldLease(lease) {
		return app.ErrFenced
	}
	s.Leases[lease.Run].held = false
	return nil
}

func (s *fakeStore) Begin(_ context.Context, lease app.Lease) (app.UnitOfWork, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.openUnitsOfWork++
	return &fakeUnitOfWork{store: s, lease: lease}, nil
}

// refuseInsideTransaction fails a port call made while any unit of work is
// open: external calls never happen inside a store transaction
// (docs/plan/phase-2-design.md section 4, the transaction rule).
func (s *fakeStore) refuseInsideTransaction(port string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.openUnitsOfWork > 0 {
		return fmt.Errorf("app_test: %s called while a unit of work is open; external calls are forbidden inside store transactions", port)
	}
	return nil
}

// --- ReadStore ---

func (s *fakeStore) ListRuns(_ context.Context, repositoryRoot string) ([]app.RunStatus, error) {
	if err := s.refuseInsideTransaction("ReadStore.ListRuns"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	repoID, ok := s.repoByRoot[repositoryRoot]
	if !ok {
		return nil, nil
	}
	var out []app.RunStatus
	for id, row := range s.Runs {
		if row.value.RepositoryID != repoID {
			continue
		}
		out = append(out, s.runStatusLocked(id))
	}
	return out, nil
}

func (s *fakeStore) runStatusLocked(runID identity.RunID) app.RunStatus {
	r := s.Runs[runID].value
	reconciling := false
	for _, op := range s.Operations { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		if op.RunID == runID && op.State == app.OperationReconciling {
			reconciling = true
			break
		}
	}
	return app.RunStatus{
		RunID: runID, Sequence: r.Sequence, State: r.State, StopRequested: r.StopRequested,
		Reconciling: reconciling, UpdatedAt: r.UpdatedAt, NeedsAttention: s.runNeedsAttentionLocked(runID),
	}
}

// runNeedsAttentionLocked mirrors the real sqlite adapter's
// runNeedsAttentionLocked (Astra F4): false for a solo run, else the
// OR-reduction of the SAME mailboxesLocked call featureRunDetail's own
// Mailboxes population uses — one implementation of the attention
// threshold rule, so ListRuns and LoadRunStatus can never disagree
// about one run. Callers hold s.mu.
func (s *fakeStore) runNeedsAttentionLocked(runID identity.RunID) bool {
	if !s.Snapshots[runID].Workflow.Feature() {
		return false
	}
	for _, m := range s.mailboxesLocked(runID, s.clock.Now()) {
		if m.Attention {
			return true
		}
	}
	return false
}

func (s *fakeStore) LoadRunStatus(_ context.Context, runID identity.RunID) (app.RunDetail, error) {
	if hook := s.LoadRunStatusHook; hook != nil {
		defer hook() // registered before the unlock defer, so it runs after the lock is released.
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.Runs[runID]; !ok {
		return app.RunDetail{}, fmt.Errorf("%w: run %s", app.ErrNotFound, runID)
	}
	taskID := s.TaskByRun[runID]
	attemptID := s.AttemptByRun[runID]
	detail := app.RunDetail{
		RunStatus:    s.runStatusLocked(runID),
		Mode:         s.Snapshots[runID].Workflow.Mode,
		TargetBranch: s.Snapshots[runID].Workflow.TargetBranch,
		TaskID:       taskID,
		AttemptID:    attemptID,
		StateRoot:    s.Snapshots[runID].StateRoot,
	}
	if at, retired := s.WorktreesRetiredAt[runID]; retired {
		detail.WorktreesRetiredAt = &at
	}
	if t, ok := s.Tasks[taskID]; ok {
		detail.TaskState = t.value.State
	}
	if a, ok := s.Attempts[attemptID]; ok {
		detail.AttemptState = a.value.State
	}
	if w, ok := s.worktreeByRunLocked(runID); ok {
		detail.WorktreePath = w.Path
	}
	// The real store's session surface (sqlite LoadRunStatus): a feature
	// run's current manager session, a solo run's attempt's current
	// non-terminated session — each resolved within this run and
	// independent of any binding — that session's current binding, and the
	// claim of the incarnation its launch context resolves
	// (sessionLaunchIncarnationLocked).
	sessionID, hasSession := s.currentManagerOfRunLocked(runID)
	if !s.Snapshots[runID].Workflow.Feature() {
		sessionID, hasSession = "", false
		if attemptID != "" {
			sessionID, hasSession = s.currentSessionOfAttemptLocked(attemptID)
		}
	}
	if hasSession {
		detail.SessionID = sessionID
		if binding, ok := s.currentBindingLocked(sessionID); ok {
			b := binding
			detail.Binding = &b
		}
		if incarnation, ok := s.sessionLaunchIncarnationLocked(sessionID); ok {
			if claim, ok := s.LaunchClaims[incarnation]; ok {
				c := claim
				detail.Claim = &c
			}
		}
	}
	for _, op := range s.Operations { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		if op.RunID == runID && (op.State == app.OperationPending || op.State == app.OperationReconciling) {
			detail.PendingOperations = append(detail.PendingOperations, op)
		}
	}
	for _, artifact := range s.Artifacts {
		if artifact.RunID == runID {
			detail.Artifacts = append(detail.Artifacts, artifact)
		}
	}
	var newestCheck *app.Operation
	for id := range s.Operations {
		op := s.Operations[id]
		if op.RunID != runID || op.Kind != app.OpCheckRun {
			continue
		}
		if newestCheck == nil || op.CreatedAt.After(newestCheck.CreatedAt) {
			c := op
			newestCheck = &c
		}
	}
	if newestCheck != nil {
		summary := app.CheckExecutionSummary{OperationID: newestCheck.ID, State: newestCheck.State}
		if outcome, ok := decodeCheckOutcome(newestCheck.Outcome); ok {
			summary.Unknown = outcome.Unknown
			summary.Detail = outcome.Detail
		}
		for _, artifact := range detail.Artifacts {
			if artifact.Kind == run.ArtifactCheckStdout || artifact.Kind == run.ArtifactCheckStderr || artifact.Kind == run.ArtifactPaneSnapshot {
				summary.EvidencePaths = append(summary.EvidencePaths, artifact.Path)
			}
		}
		detail.LastCheck = &summary
	}
	if len(s.Submissions) > 0 {
		last := s.Submissions[len(s.Submissions)-1]
		detail.LastSubmission = &last
	}

	detail.Tasks = s.tasksSummaryLocked(runID)
	if integration, ok := s.latestIntegrationLocked(runID); ok {
		detail.LatestIntegration = &app.IntegrationSummary{
			ID: integration.ID, TaskID: integration.TaskID, SourceCommitOID: integration.SourceCommitOID,
			PremergeHeadOID: integration.PremergeHeadOID, MergeCommitOID: integration.MergeCommitOID, State: integration.State,
		}
	}
	if s.Snapshots[runID].Workflow.Feature() {
		detail.Worktrees, detail.WorktreeRetirements = s.featureWorktreesLocked(runID)
	}
	detail.GuardShortfalls = s.guardShortfallsLocked(runID)
	detail.Mailboxes = s.mailboxesLocked(runID, s.clock.Now())
	detail.PendingQuestions = s.pendingQuestionsLocked(runID, s.clock.Now())
	detail.Sessions = s.sessionsLocked(runID)

	return detail, nil
}

// featureWorktreesLocked mirrors the real store's feature detail
// (featureWorktreeDetail): every worktree row of the run in insertion
// order — a seeded row with no insertion sequence first, by id — as a
// non-nil list, and the run's worktree.retire operations newest first.
// Callers hold s.mu.
func (s *fakeStore) featureWorktreesLocked(runID identity.RunID) ([]run.Worktree, []app.Operation) {
	rows := []run.Worktree{}
	for _, row := range s.Worktrees {
		if row.value.RunID == runID {
			rows = append(rows, row.value)
		}
	}
	slices.SortFunc(rows, func(a, b run.Worktree) int {
		return cmp.Or(cmp.Compare(s.worktreeInsertOrder[a.ID], s.worktreeInsertOrder[b.ID]), cmp.Compare(a.ID, b.ID))
	})
	var retires []app.Operation
	for id := range s.Operations {
		if op := s.Operations[id]; op.RunID == runID && op.Kind == app.OpWorktreeRetire {
			retires = append(retires, op)
		}
	}
	slices.SortFunc(retires, func(a, b app.Operation) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return rows, retires
}

func (s *fakeStore) worktreeByRunLocked(runID identity.RunID) (run.Worktree, bool) {
	for _, w := range s.Worktrees {
		if w.value.RunID == runID {
			return w.value, true
		}
	}
	return run.Worktree{}, false
}

// newestAttemptWorktreeLocked returns the most recently inserted worktree
// row linked to attemptID, mirroring the real store's `WHERE attempt_id =
// ? ORDER BY rowid DESC LIMIT 1`; an empty attemptID matches nothing, as
// SQL's comparison with a NULL column never does. Callers hold s.mu.
func (s *fakeStore) newestAttemptWorktreeLocked(attemptID identity.AttemptID) (run.Worktree, bool) {
	var (
		newest run.Worktree
		order  int
		found  bool
	)
	if attemptID == "" {
		return run.Worktree{}, false
	}
	for id, row := range s.Worktrees {
		if row.value.AttemptID != attemptID {
			continue
		}
		if seq := s.worktreeInsertOrder[id]; !found || seq > order {
			newest, order, found = row.value, seq, true
		}
	}
	return newest, found
}

// worktreePathForAttemptLocked mirrors the real store's launch-boundary
// lookup (sqlite worktreePathForAttempt): the attempt's own linked row
// when one exists; else the run's only worktree row, and only while that
// row is unlinked (the solo shape); else "" — a row linked to another
// attempt is never served, and several rows, linked or not, are never
// guessed among. Callers hold s.mu.
func (s *fakeStore) worktreePathForAttemptLocked(runID identity.RunID, attemptID identity.AttemptID) string {
	if w, ok := s.newestAttemptWorktreeLocked(attemptID); ok {
		return w.Path
	}
	var rows []run.Worktree
	for _, row := range s.Worktrees {
		if row.value.RunID == runID {
			rows = append(rows, row.value)
		}
	}
	if len(rows) == 1 && rows[0].AttemptID == "" {
		return rows[0].Path
	}
	return ""
}

func (s *fakeStore) LoadFrozenRun(_ context.Context, runID identity.RunID) (app.FrozenRun, error) {
	if err := s.refuseInsideTransaction("ReadStore.LoadFrozenRun"); err != nil {
		return app.FrozenRun{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.Runs[runID]
	if !ok {
		return app.FrozenRun{}, fmt.Errorf("%w: run %s", app.ErrNotFound, runID)
	}
	root := ""
	for candidate, id := range s.repoByRoot {
		if id == row.value.RepositoryID {
			root = candidate
		}
	}
	return app.FrozenRun{Snapshot: s.Snapshots[runID], RepositoryRoot: root, Brief: s.Briefs[runID]}, nil
}

func (*fakeStore) LoadCheckExecutionContext(context.Context, identity.OperationID) (app.CheckExecutionContext, error) {
	return app.CheckExecutionContext{}, fmt.Errorf("app_test: LoadCheckExecutionContext is not exercised by the controller (hop check-exec's own port)")
}

// --- SubmissionStore ---

func (s *fakeStore) ClaimLaunch(_ context.Context, claim app.LaunchClaim) error { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.LaunchClaims[claim.IncarnationID]; ok {
		if existing.PID != claim.PID {
			return fmt.Errorf("app_test: launch claim for incarnation %s already exists with a different pid", claim.IncarnationID)
		}
		if existing.State != app.LaunchClaimExecPending {
			return fmt.Errorf("app_test: launch claim for incarnation %s is already settled; a retry never rewrites settled history", claim.IncarnationID)
		}
		if existing.Executable != claim.Executable || existing.ArgvDigest != claim.ArgvDigest {
			return fmt.Errorf("app_test: launch claim for incarnation %s records a different executable or argv; an incompatible retry is refused", claim.IncarnationID)
		}
		// A same-pid retry refreshes the seed evidence only; every
		// invocation identity field stays as first claimed.
		existing.SeedEvidence = claim.SeedEvidence
		s.LaunchClaims[claim.IncarnationID] = existing
		return nil
	}
	rRow, ok := s.Runs[claim.RunID]
	if !ok {
		return fmt.Errorf("%w: run %s", app.ErrNotFound, claim.RunID)
	}
	if rRow.value.StopRequested || rRow.value.State == run.RunStopping || rRow.value.State == run.RunStopped {
		return fmt.Errorf("app_test: run %s is stopping or stopped; launch claim refused", claim.RunID)
	}
	sessionID := claim.SessionID
	if sessionID == "" {
		current, ok := s.currentSessionOfAttemptLocked(claim.AttemptID)
		if !ok {
			return fmt.Errorf("app_test: incarnation %s is not attempt %s's current identity; launch claim refused", claim.IncarnationID, claim.AttemptID)
		}
		sessionID = current
	}
	if !s.sessionIncarnationCurrentLocked(sessionID, claim.IncarnationID) {
		return fmt.Errorf("app_test: incarnation %s is not session %s's current identity; launch claim refused", claim.IncarnationID, sessionID)
	}
	s.LaunchClaims[claim.IncarnationID] = claim
	return nil
}

// currentManagerOfRunLocked resolves the run's non-terminated manager
// session, as the real store's managerSession does.
func (s *fakeStore) currentManagerOfRunLocked(runID identity.RunID) (identity.SessionID, bool) {
	for id, row := range s.Sessions {
		if row.value.RunID == runID && row.value.Role == run.RoleManager && sessionCurrent(&row.value) {
			return id, true
		}
	}
	return "", false
}

// currentSessionOfAttemptLocked resolves the attempt's non-terminated
// session, as the real store's currentSession does.
func (s *fakeStore) currentSessionOfAttemptLocked(attemptID identity.AttemptID) (identity.SessionID, bool) {
	for id, row := range s.Sessions {
		if row.value.AttemptID == attemptID && row.value.State != run.SessionLost && row.value.State != run.SessionTerminated {
			return id, true
		}
	}
	return "", false
}

// sessionIncarnationCurrentLocked mirrors the real store's one
// principal-incarnation rule (sqlite sessionIncarnationCurrent), which
// ClaimLaunch, the plan verbs, message send, fetch and ack, review
// submission and result submission all decide through: the session's
// current binding must carry exactly incarnation, with no pending launch
// intent of the session — any of them — naming a different one or none
// usable (pendingSessionIntentDisagreesLocked); with no binding row at
// all, the session's newest PENDING pane.open/launch.send intent must name
// it; any binding row (a superseded one included) disables that fallback.
func (s *fakeStore) sessionIncarnationCurrentLocked(sessionID identity.SessionID, incarnation identity.IncarnationID) bool {
	if binding, ok := s.currentBindingLocked(sessionID); ok {
		if s.pendingSessionIntentDisagreesLocked(sessionID, binding.IncarnationID) {
			return false
		}
		return binding.IncarnationID == incarnation
	}
	if len(s.Bindings[sessionID]) > 0 {
		return false
	}
	intent, hasIntent := s.pendingSessionIntentLocked(sessionID)
	return hasIntent && intent == incarnation
}

// pendingSessionIntentDisagreesLocked mirrors the real store's
// pendingLaunchIntentDisagrees: whether ANY pending pane.open/launch.send
// operation whose intent's session_id is the session fails to carry
// exactly incarnation as a string incarnation_id — a different one, or an
// absent, null or non-string one. The intent is read as generic JSON so an
// unusable incarnation_id is seen, never skipped by a typed decode.
func (s *fakeStore) pendingSessionIntentDisagreesLocked(sessionID identity.SessionID, incarnation identity.IncarnationID) bool {
	for _, op := range s.Operations { //nolint:gocritic // rangeValCopy: test fake; the journal is small and read-only here.
		if op.State != app.OperationPending || (op.Kind != app.OpPaneOpen && op.Kind != app.OpLaunchSend) {
			continue
		}
		raw, err := json.Marshal(op.Intent)
		if err != nil {
			continue
		}
		var fields map[string]any
		if json.Unmarshal(raw, &fields) != nil {
			continue
		}
		if session, ok := fields["session_id"].(string); !ok || session != sessionID.String() {
			continue
		}
		if named, ok := fields["incarnation_id"].(string); !ok || named != incarnation.String() {
			return true
		}
	}
	return false
}

// pendingSessionIntentLocked resolves the incarnation named by the
// session's newest PENDING pane.open/launch.send operation intent, keyed
// by the intent's own session_id (the real store's
// pendingLaunchIntentOfSession).
func (s *fakeStore) pendingSessionIntentLocked(sessionID identity.SessionID) (identity.IncarnationID, bool) {
	var (
		newest    time.Time
		newestInc identity.IncarnationID
		found     bool
	)
	for _, op := range s.Operations { //nolint:gocritic // rangeValCopy: test fake; the journal is small and read-only here.
		if op.State != app.OperationPending || (op.Kind != app.OpPaneOpen && op.Kind != app.OpLaunchSend) {
			continue
		}
		intent, ok := decodePaneOpenIntent(op.Intent)
		if !ok || intent.SessionID != sessionID.String() {
			continue
		}
		if !found || op.CreatedAt.After(newest) {
			newest = op.CreatedAt
			newestInc = identity.IncarnationID(intent.IncarnationID)
			found = true
		}
	}
	return newestInc, found
}

// sessionLaunchIncarnationLocked mirrors the real store's one
// launch-identity resolution (sqlite sessionLaunchIncarnation), which the
// launch context and the status read model share: the session's current
// binding's incarnation, unless any pending launch intent of the session
// names another or none usable; else its newest pending launch intent's; a
// disagreement or a malformed intent identity resolves nothing.
func (s *fakeStore) sessionLaunchIncarnationLocked(sessionID identity.SessionID) (identity.IncarnationID, bool) {
	if binding, hasBinding := s.currentBindingLocked(sessionID); hasBinding {
		if s.pendingSessionIntentDisagreesLocked(sessionID, binding.IncarnationID) {
			return "", false
		}
		return binding.IncarnationID, true
	}
	intent, hasIntent := s.pendingSessionIntentLocked(sessionID)
	if !hasIntent {
		return "", false
	}
	if _, err := identity.ParseIncarnationID(intent.String()); err != nil {
		return "", false
	}
	return intent, true
}

// checkOutcomeFields are the check outcome JSON keys the fake's status
// read model surfaces.
type checkOutcomeFields struct {
	Unknown bool   `json:"unknown"`
	Detail  string `json:"detail"`
}

// decodeCheckOutcome reads a persisted check outcome payload generically.
func decodeCheckOutcome(payload any) (checkOutcomeFields, bool) {
	if payload == nil {
		return checkOutcomeFields{}, false
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return checkOutcomeFields{}, false
	}
	var fields checkOutcomeFields
	if err := json.Unmarshal(raw, &fields); err != nil {
		return checkOutcomeFields{}, false
	}
	return fields, true
}

// paneOpenIntentFields are the stable pane.open intent JSON keys the store
// contract reads (`incarnation_id`, `session_id`).
type paneOpenIntentFields struct {
	IncarnationID string `json:"incarnation_id"`
	SessionID     string `json:"session_id"`
}

// decodePaneOpenIntent reads the stable pane.open intent keys from a
// persisted payload, which after the fake's commit-time JSON round trip is
// a map[string]any, never the original Go struct.
func decodePaneOpenIntent(payload any) (paneOpenIntentFields, bool) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return paneOpenIntentFields{}, false
	}
	var fields paneOpenIntentFields
	if err := json.Unmarshal(raw, &fields); err != nil {
		return paneOpenIntentFields{}, false
	}
	return fields, true
}

func (s *fakeStore) SettleLaunchFailure(_ context.Context, incarnation identity.IncarnationID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	claim, ok := s.LaunchClaims[incarnation]
	if !ok {
		return fmt.Errorf("%w: launch claim %s", app.ErrNotFound, incarnation)
	}
	claim.State = app.LaunchClaimExecFailed
	claim.Error = reason
	s.LaunchClaims[incarnation] = claim
	return nil
}

func (s *fakeStore) ClaimCheckExec(_ context.Context, op identity.OperationID, pid int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	operation, ok := s.Operations[op]
	// The real store's contract (design section 3, generalized by the
	// worktree-retirement kinds): any pending exec-claimable operation of
	// the current generation, decided by the same OperationKind predicate
	// the real store uses. Publish, reset, fence and every Herdr act are
	// executed directly by the controller and are never claimable.
	if !ok || !operation.Kind.ExecClaimable() || operation.State != app.OperationPending {
		return fmt.Errorf("app_test: operation %s is not a pending exec-claimable execution; check-exec claim refused", op)
	}
	if row, ok := s.Leases[operation.RunID]; !ok || operation.Generation != row.lease.Generation {
		return fmt.Errorf("app_test: operation %s is not of the current generation; check-exec claim refused", op)
	}
	// The real store's pid-before-exec contract (sqlite submission.go's
	// ClaimCheckExec): one row, one pid, never re-armed or overwritten — a
	// same-pid retry is an idempotent no-op that preserves the original
	// row, and a different pid is refused.
	if existing, claimed := s.CheckExecClaims[op]; claimed {
		if existing.PID != pid {
			return fmt.Errorf("app_test: operation %s already has a check-exec claim by another pid; a claim is never overwritten", op)
		}
		return nil
	}
	s.CheckExecClaims[op] = app.CheckExecClaim{OperationID: op, PID: pid, ClaimedAt: s.clock.Now()}
	return nil
}

func (s *fakeStore) RequestStop(_ context.Context, runID identity.RunID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.Runs[runID]
	if !ok {
		return fmt.Errorf("%w: run %s", app.ErrNotFound, runID)
	}
	// The real store's single-row stop write moves the run's revision: a
	// controller unit of work that staged the pre-stop run must conflict
	// at commit instead of silently overwriting the stop flag.
	row.value = row.value.RequestStop(s.clock.Now())
	row.revision++
	return nil
}

func (s *fakeStore) SubmitResult(_ context.Context, submission app.ResultSubmission) (app.SubmissionOutcome, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.submitResultLocked(&submission, nil), nil
}

// submitResultLocked decides and records one result submission. onAccept,
// when non-nil, runs after an acceptance is applied and before the lock is
// released, as the real store's accepting transaction commits its own side
// effects. Callers hold s.mu.
func (s *fakeStore) submitResultLocked(submission *app.ResultSubmission, onAccept func()) app.SubmissionOutcome {
	now := s.clock.Now()

	// Section 7 step 2 first: existence and agreement (attempt belongs to
	// the claimed task, task to the claimed run) precede everything,
	// including duplicate-receipt resolution — a receipt is resolved only
	// for a submission whose identities actually agree.
	aRow, ok := s.Attempts[submission.AttemptID]
	if !ok {
		outcome := app.SubmissionOutcome{Kind: app.SubmissionMalformed, Detail: "unknown attempt"}
		s.Submissions = append(s.Submissions, outcome)
		return outcome
	}
	tRow := s.Tasks[submission.TaskID]
	rRow := s.Runs[submission.RunID]
	if tRow == nil || rRow == nil || aRow.value.TaskID != submission.TaskID || tRow.value.RunID != submission.RunID {
		outcome := app.SubmissionOutcome{Kind: app.SubmissionMalformed, Detail: "attempt/task/run do not agree"}
		s.Submissions = append(s.Submissions, outcome)
		return outcome
	}

	// Step 3 onward is the domain's AcceptResult, handed any prior
	// accepted result so receipts resolve before state or incarnation
	// preconditions.
	var prior *run.Result
	if existing, ok := s.Results[submission.AttemptID]; ok {
		p := existing
		prior = &p
	}

	incarnationCurrent := false
	if sessionID, ok := s.currentSessionOfAttemptLocked(submission.AttemptID); ok {
		incarnationCurrent = s.sessionIncarnationCurrentLocked(sessionID, submission.IncarnationID)
	}
	claim, hasClaim := s.LaunchClaims[submission.IncarnationID]
	launchClaimSettled := hasClaim && claim.State == app.LaunchClaimExeced
	// The real store re-reads the task's mailbox inside the accepting
	// transaction and hands it to AcceptResult, which consults it last; a
	// solo task, which no message ever addresses, is clear.
	mailboxClear := s.mailboxClearLocked(submission.TaskID)

	ctx := run.AcceptanceContext{IncarnationCurrent: incarnationCurrent, LaunchClaimSettled: launchClaimSettled, MailboxClear: mailboxClear}
	outcomeVal, err := run.AcceptResult(rRow.value, tRow.value, aRow.value, prior, ctx, run.ResultSubmission{
		ID: submission.ID, CommitOID: submission.CommitOID, Summary: submission.Summary, Digest: submission.Digest,
	}, now)

	var outcome app.SubmissionOutcome
	switch {
	case err == nil:
		// The acceptance transaction's entity transitions move the same
		// row revisions the real store does, so a stale controller unit of
		// work conflicts at commit rather than overwriting the handoff.
		s.Results[submission.AttemptID] = outcomeVal.Result
		rRow.value = outcomeVal.Run
		rRow.revision++
		tRow.value = outcomeVal.Task
		tRow.revision++
		aRow.value = outcomeVal.Attempt
		aRow.revision++
		s.CheckRequests[submission.ID] = app.CheckRequest{
			ResultID: submission.ID, AttemptID: submission.AttemptID,
			State: app.CheckRequestRequested, CreatedAt: now,
		}
		outcome = app.SubmissionOutcome{Kind: app.SubmissionAccepted, ResultID: submission.ID}
		if onAccept != nil {
			onAccept()
		}
	case errors.Is(err, run.ErrDuplicateResult):
		outcome = app.SubmissionOutcome{Kind: app.SubmissionDuplicate, ResultID: outcomeVal.Result.ID}
	case errors.Is(err, run.ErrConflictingResult):
		outcome = app.SubmissionOutcome{Kind: app.SubmissionConflicting, ResultID: outcomeVal.Result.ID}
	case errors.Is(err, run.ErrTransientNotRunning), errors.Is(err, run.ErrMailboxNotClear):
		reason, _ := app.TransientReasonOf(err)
		// The real store's details: the drain line itself for a pending
		// mailbox, the domain error's own text otherwise.
		detail := err.Error()
		if reason == app.TransientUndeliveredMessages {
			detail = app.GrammarTransientUndeliveredLine
		}
		outcome = app.SubmissionOutcome{Kind: app.SubmissionTransient, Detail: detail, Transient: reason}
	default:
		outcome = app.SubmissionOutcome{Kind: app.SubmissionStale, Detail: err.Error()}
	}
	s.Submissions = append(s.Submissions, outcome)
	return outcome
}

func (s *fakeStore) RecordMalformed(_ context.Context, claimed app.ClaimedSubmission) (app.SubmissionOutcome, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	s.mu.Lock()
	defer s.mu.Unlock()
	outcome := app.SubmissionOutcome{Kind: app.SubmissionMalformed, Detail: claimed.Detail}
	s.Submissions = append(s.Submissions, outcome)
	return outcome, nil
}
