package app_test

import (
	"context"
	"errors"
	"fmt"
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

	repoByRoot map[string]identity.RepositoryID
	seqByRepo  map[identity.RepositoryID]int

	Runs      map[identity.RunID]*entityRow[run.Run]
	Snapshots map[identity.RunID]app.RunSnapshot
	Tasks     map[identity.TaskID]*entityRow[run.Task]
	Attempts  map[identity.AttemptID]*entityRow[run.Attempt]
	Sessions  map[identity.SessionID]*entityRow[run.Session]
	Worktrees map[identity.WorktreeID]*entityRow[run.Worktree]
	Bindings  map[identity.SessionID][]run.RuntimeBinding
	Results   map[identity.AttemptID]run.Result
	Artifacts []run.Artifact

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
}

func newFakeStore(clock interface{ Now() time.Time }) *fakeStore {
	return &fakeStore{
		clock:           clock,
		repoByRoot:      map[string]identity.RepositoryID{},
		seqByRepo:       map[identity.RepositoryID]int{},
		Runs:            map[identity.RunID]*entityRow[run.Run]{},
		Snapshots:       map[identity.RunID]app.RunSnapshot{},
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
	}
}

// --- StateStore ---

func (s *fakeStore) InitializeRun(_ context.Context, spec app.NewRunSpec) (identity.RunID, app.Lease, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	s.mu.Lock()
	defer s.mu.Unlock()

	repoID, ok := s.repoByRoot[spec.RepositoryRoot]
	if !ok {
		repoID = identity.RepositoryID(fmt.Sprintf("repo-%d", len(s.repoByRoot)+1))
		s.repoByRoot[spec.RepositoryRoot] = repoID
	}
	s.seqByRepo[repoID]++
	seq := s.seqByRepo[repoID]

	r := run.NewRun(spec.RunID, repoID, seq, spec.BriefDigest, spec.Now)
	s.Runs[spec.RunID] = &entityRow[run.Run]{value: r, revision: 1}
	s.Snapshots[spec.RunID] = spec.Snapshot

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

func (s *fakeStore) Heartbeat(_ context.Context, lease app.Lease) error {
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
	return &fakeUnitOfWork{store: s, lease: lease}, nil
}

// --- ReadStore ---

func (s *fakeStore) ListRuns(_ context.Context, repositoryRoot string) ([]app.RunStatus, error) {
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
	return app.RunStatus{RunID: runID, Sequence: r.Sequence, State: r.State, Reconciling: reconciling, UpdatedAt: r.UpdatedAt}
}

func (s *fakeStore) LoadRunStatus(_ context.Context, runID identity.RunID) (app.RunDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.Runs[runID]; !ok {
		return app.RunDetail{}, fmt.Errorf("%w: run %s", app.ErrNotFound, runID)
	}
	taskID := s.TaskByRun[runID]
	attemptID := s.AttemptByRun[runID]
	detail := app.RunDetail{
		RunStatus: s.runStatusLocked(runID),
		TaskID:    taskID,
		AttemptID: attemptID,
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
	if sessionID, binding, ok := s.currentBindingByAttemptLocked(attemptID); ok {
		detail.SessionID = sessionID
		b := binding
		detail.Binding = &b
		if claim, ok := s.LaunchClaims[binding.IncarnationID]; ok {
			c := claim
			detail.Claim = &c
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
	if len(s.Submissions) > 0 {
		last := s.Submissions[len(s.Submissions)-1]
		detail.LastSubmission = &last
	}
	return detail, nil
}

func (s *fakeStore) worktreeByRunLocked(runID identity.RunID) (run.Worktree, bool) {
	for _, w := range s.Worktrees {
		if w.value.RunID == runID {
			return w.value, true
		}
	}
	return run.Worktree{}, false
}

func (s *fakeStore) currentBindingByAttemptLocked(attemptID identity.AttemptID) (identity.SessionID, run.RuntimeBinding, bool) {
	for sessionID, history := range s.Bindings {
		sess, ok := s.Sessions[sessionID]
		if !ok || sess.value.AttemptID != attemptID {
			continue
		}
		// Phase 2: at most one non-terminated session per attempt at any
		// instant; a lost or terminated session's binding is history, not
		// the attempt's current one, even though the binding row itself
		// was never marked superseded (only the session ended).
		if sess.value.State == run.SessionLost || sess.value.State == run.SessionTerminated {
			continue
		}
		for i := len(history) - 1; i >= 0; i-- {
			if !history[i].Superseded {
				return sessionID, history[i], true
			}
		}
	}
	return "", run.RuntimeBinding{}, false
}

func (*fakeStore) LoadLaunchContext(context.Context, identity.RunID, identity.AttemptID) (app.LaunchContext, error) {
	return app.LaunchContext{}, fmt.Errorf("app_test: LoadLaunchContext is not exercised by the controller (hop launch's own port)")
}

func (*fakeStore) LoadCheckExecutionContext(context.Context, identity.OperationID) (app.CheckExecutionContext, error) {
	return app.CheckExecutionContext{}, fmt.Errorf("app_test: LoadCheckExecutionContext is not exercised by the controller (hop check-exec's own port)")
}

// --- SubmissionStore ---

func (s *fakeStore) ClaimLaunch(_ context.Context, claim app.LaunchClaim) error { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.LaunchClaims[claim.IncarnationID]; ok && existing.PID != claim.PID {
		return fmt.Errorf("app_test: launch claim for incarnation %s already exists with a different pid", claim.IncarnationID)
	}
	s.LaunchClaims[claim.IncarnationID] = claim
	return nil
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
	row.value = row.value.RequestStop(s.clock.Now())
	return nil
}

func (s *fakeStore) SubmitResult(_ context.Context, submission app.ResultSubmission) (app.SubmissionOutcome, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()

	if prior, ok := s.Results[submission.AttemptID]; ok {
		outcome := app.SubmissionOutcome{ResultID: prior.ID}
		if prior.ContentDigest == submission.Digest {
			outcome.Kind = app.SubmissionDuplicate
		} else {
			outcome.Kind = app.SubmissionConflicting
		}
		s.Submissions = append(s.Submissions, outcome)
		return outcome, nil
	}

	aRow, ok := s.Attempts[submission.AttemptID]
	if !ok {
		outcome := app.SubmissionOutcome{Kind: app.SubmissionMalformed, Detail: "unknown attempt"}
		s.Submissions = append(s.Submissions, outcome)
		return outcome, nil
	}
	tRow := s.Tasks[aRow.value.TaskID]
	rRow := s.Runs[submission.RunID]
	if tRow == nil || rRow == nil || tRow.value.RunID != submission.RunID {
		outcome := app.SubmissionOutcome{Kind: app.SubmissionMalformed, Detail: "attempt/task/run do not agree"}
		s.Submissions = append(s.Submissions, outcome)
		return outcome, nil
	}

	sessionID, binding, hasBinding := s.currentBindingByAttemptLocked(submission.AttemptID)
	_ = sessionID
	incarnationCurrent := hasBinding && binding.IncarnationID == submission.IncarnationID && !binding.Superseded
	claim, hasClaim := s.LaunchClaims[submission.IncarnationID]
	launchClaimSettled := hasClaim && claim.State == app.LaunchClaimExeced

	ctx := run.AcceptanceContext{IncarnationCurrent: incarnationCurrent, LaunchClaimSettled: launchClaimSettled}
	outcomeVal, err := run.AcceptResult(rRow.value, tRow.value, aRow.value, nil, ctx, run.ResultSubmission{
		ID: submission.ID, CommitOID: submission.CommitOID, Summary: submission.Summary, Digest: submission.Digest,
	}, now)

	var outcome app.SubmissionOutcome
	switch {
	case err == nil:
		s.Results[submission.AttemptID] = outcomeVal.Result
		rRow.value = outcomeVal.Run
		tRow.value = outcomeVal.Task
		aRow.value = outcomeVal.Attempt
		s.CheckRequests[submission.ID] = app.CheckRequest{
			ResultID: submission.ID, AttemptID: submission.AttemptID,
			State: app.CheckRequestRequested, CreatedAt: now,
		}
		outcome = app.SubmissionOutcome{Kind: app.SubmissionAccepted, ResultID: submission.ID}
	case errors.Is(err, run.ErrTransientNotRunning):
		outcome = app.SubmissionOutcome{Kind: app.SubmissionTransient, Detail: "attempt not yet running; retry"}
	default:
		outcome = app.SubmissionOutcome{Kind: app.SubmissionStale, Detail: err.Error()}
	}
	s.Submissions = append(s.Submissions, outcome)
	return outcome, nil
}

func (s *fakeStore) RecordMalformed(_ context.Context, claimed app.ClaimedSubmission) (app.SubmissionOutcome, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	s.mu.Lock()
	defer s.mu.Unlock()
	outcome := app.SubmissionOutcome{Kind: app.SubmissionMalformed, Detail: claimed.Detail}
	s.Submissions = append(s.Submissions, outcome)
	return outcome, nil
}
