package app_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// jsonRoundtripOperation mimics what a real JSON-backed store returns for
// an operation's Intent/ActEvidence/Outcome payloads: each decoded
// generically (map[string]any for a struct, a scalar for a string or
// number), never the original Go type a caller constructed. Application
// code must never rely on Go type identity for a persisted payload
// (decodeOperationPayload is the one place that reads one back), and this
// keeps the fakes exercising that same decode path rather than hiding it
// behind an in-memory store that happens to keep the exact value.
func jsonRoundtripOperation(op app.Operation) app.Operation { //nolint:gocritic // hugeParam: mirrors Operation's own shape; called once per commit in tests, never a hot loop.
	op.Intent = jsonRoundtripAny(op.Intent)
	op.ActEvidence = jsonRoundtripAny(op.ActEvidence)
	op.Outcome = jsonRoundtripAny(op.Outcome)
	return op
}

func jsonRoundtripAny(v any) any {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return v
	}
	return decoded
}

// currentBindingLocked returns sessionID's current (non-superseded)
// binding from base state alone, ignoring any transaction overlay.
func (s *fakeStore) currentBindingLocked(sessionID identity.SessionID) (run.RuntimeBinding, bool) {
	history := s.Bindings[sessionID]
	for i := len(history) - 1; i >= 0; i-- {
		if !history[i].Superseded {
			return history[i], true
		}
	}
	return run.RuntimeBinding{}, false
}

// bindingKey identifies one RuntimeBinding row: UNIQUE(session_id,
// incarnation_id) per the schema.
type bindingKey struct {
	session     identity.SessionID
	incarnation identity.IncarnationID
}

// fakeUnitOfWork is a handwritten UnitOfWork: every read merges base state
// with this transaction's own staged writes, and Commit re-validates the
// lease before merging the overlay into the store atomically. This makes
// the takeover barrier testable: a unit of work begun under a lease that
// has since moved fails to commit even though its reads and in-memory
// writes already happened, matching "fencing the outcome commit does not
// fence the act itself" (docs/plan/phase-2-design.md section 4).
type fakeUnitOfWork struct {
	store *fakeStore
	lease app.Lease

	runs      map[identity.RunID]entityRow[run.Run]
	tasks     map[identity.TaskID]entityRow[run.Task]
	attempts  map[identity.AttemptID]entityRow[run.Attempt]
	sessions  map[identity.SessionID]entityRow[run.Session]
	worktrees map[identity.WorktreeID]entityRow[run.Worktree]

	worktreeCreated []run.Worktree
	sessionCreated  []run.Session

	bindingCreated []run.RuntimeBinding
	bindingSaved   map[bindingKey]run.RuntimeBinding

	launchClaimSettled map[identity.IncarnationID]app.LaunchClaimSettlement

	opCreated map[identity.OperationID]app.Operation
	opSaved   map[identity.OperationID]app.Operation

	checkRequestSaved map[identity.ResultID]app.CheckRequest

	artifactsSaved []run.Artifact
	transitions    []app.Transition

	done bool
}

func (u *fakeUnitOfWork) ensureOpen() {
	if u.done {
		panic("app_test: unit of work used after Commit or Rollback")
	}
}

func (u *fakeUnitOfWork) Runs() app.RunRepository           { return fakeRunRepo{u} }
func (u *fakeUnitOfWork) Tasks() app.TaskRepository         { return fakeTaskRepo{u} }
func (u *fakeUnitOfWork) Attempts() app.AttemptRepository   { return fakeAttemptRepo{u} }
func (u *fakeUnitOfWork) Sessions() app.SessionRepository   { return fakeSessionRepo{u} }
func (u *fakeUnitOfWork) Worktrees() app.WorktreeRepository { return fakeWorktreeRepo{u} }
func (u *fakeUnitOfWork) Results() app.ResultRepository     { return fakeResultRepo{u} }
func (u *fakeUnitOfWork) Artifacts() app.ArtifactRepository { return fakeArtifactRepo{u} }
func (u *fakeUnitOfWork) Bindings() app.BindingRepository   { return fakeBindingRepo{u} }
func (u *fakeUnitOfWork) LaunchClaims() app.LaunchClaimRepository {
	return fakeLaunchClaimRepo{u}
}

func (u *fakeUnitOfWork) CheckExecClaims() app.CheckExecClaimRepository {
	return fakeCheckExecClaimRepo{u}
}
func (u *fakeUnitOfWork) Operations() app.OperationRepository       { return fakeOperationRepo{u} }
func (u *fakeUnitOfWork) Transitions() app.TransitionRepository     { return fakeTransitionRepo{u} }
func (u *fakeUnitOfWork) CheckRequests() app.CheckRequestRepository { return fakeCheckRequestRepo{u} }

// Commit re-validates the lease against the store's current lease row and,
// only if it still matches and is unexpired, merges every staged write
// into the store in one critical section.
func (u *fakeUnitOfWork) Commit() error {
	u.ensureOpen()
	u.done = true
	s := u.store
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.matchesHeldLease(u.lease) {
		return app.ErrFenced
	}
	if row := s.Leases[u.lease.Run]; row.lease.ExpiresAt.Before(s.clock.Now()) {
		return app.ErrFenced
	}

	for id, row := range u.runs {
		s.Runs[id] = &entityRow[run.Run]{value: row.value, revision: row.revision}
	}
	for id, row := range u.tasks {
		s.Tasks[id] = &entityRow[run.Task]{value: row.value, revision: row.revision}
	}
	for id, row := range u.attempts {
		s.Attempts[id] = &entityRow[run.Attempt]{value: row.value, revision: row.revision}
	}
	for id, row := range u.sessions {
		s.Sessions[id] = &entityRow[run.Session]{value: row.value, revision: row.revision}
	}
	for id, row := range u.worktrees {
		s.Worktrees[id] = &entityRow[run.Worktree]{value: row.value, revision: row.revision}
	}
	for _, w := range u.worktreeCreated {
		s.Worktrees[w.ID] = &entityRow[run.Worktree]{value: w, revision: 1}
	}
	for _, sess := range u.sessionCreated { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		s.Sessions[sess.ID] = &entityRow[run.Session]{value: sess, revision: 1}
		s.AttemptByRun[sess.RunID] = sess.AttemptID // Phase 2: one attempt per run
	}
	for _, b := range u.bindingCreated { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		s.Bindings[b.SessionID] = append(s.Bindings[b.SessionID], b)
	}
	for key, b := range u.bindingSaved { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		history := s.Bindings[key.session]
		replaced := false
		for i := range history {
			if history[i].IncarnationID == key.incarnation {
				history[i] = b
				replaced = true
			}
		}
		if !replaced {
			history = append(history, b)
		}
		s.Bindings[key.session] = history
	}
	for incarnation, settlement := range u.launchClaimSettled {
		claim, ok := s.LaunchClaims[incarnation]
		if !ok {
			return fmt.Errorf("%w: launch claim %s", app.ErrNotFound, incarnation)
		}
		claim.State = settlement.State
		claim.SettledAt = settlement.At
		claim.SettlementEvidence = fmt.Sprintf("pane=%s pid=%d exe=%s marker=%s", settlement.PaneID, settlement.PID, settlement.Executable, settlement.ArgvMarker)
		if settlement.Reason != "" {
			claim.Error = settlement.Reason
		}
		s.LaunchClaims[incarnation] = claim
	}
	for id, op := range u.opCreated { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		s.Operations[id] = jsonRoundtripOperation(op)
	}
	for id, op := range u.opSaved { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		s.Operations[id] = jsonRoundtripOperation(op)
	}
	for id, cr := range u.checkRequestSaved {
		s.CheckRequests[id] = cr
	}
	s.Artifacts = append(s.Artifacts, u.artifactsSaved...)
	s.Transitions = append(s.Transitions, u.transitions...)
	return nil
}

func (u *fakeUnitOfWork) Rollback() error {
	u.done = true
	return nil
}

// --- Runs ---

type fakeRunRepo struct{ u *fakeUnitOfWork }

func (r fakeRunRepo) Get(_ context.Context, id identity.RunID) (run.Run, int64, error) {
	if staged, ok := r.u.runs[id]; ok {
		return staged.value, staged.revision, nil
	}
	base, ok := r.u.store.Runs[id]
	if !ok {
		return run.Run{}, 0, fmt.Errorf("%w: run %s", app.ErrNotFound, id)
	}
	return base.value, base.revision, nil
}

func (r fakeRunRepo) Save(_ context.Context, v run.Run, expectedRevision int64) (int64, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	if base, ok := r.u.store.Runs[v.ID]; ok {
		if _, staged := r.u.runs[v.ID]; !staged && base.revision != expectedRevision {
			return 0, app.ErrRevisionConflict
		}
	}
	if r.u.runs == nil {
		r.u.runs = map[identity.RunID]entityRow[run.Run]{}
	}
	next := expectedRevision + 1
	r.u.runs[v.ID] = entityRow[run.Run]{value: v, revision: next}
	return next, nil
}

// --- Tasks ---

type fakeTaskRepo struct{ u *fakeUnitOfWork }

func (r fakeTaskRepo) Get(_ context.Context, id identity.TaskID) (run.Task, int64, error) {
	if staged, ok := r.u.tasks[id]; ok {
		return staged.value, staged.revision, nil
	}
	base, ok := r.u.store.Tasks[id]
	if !ok {
		return run.Task{}, 0, fmt.Errorf("%w: task %s", app.ErrNotFound, id)
	}
	return base.value, base.revision, nil
}

func (r fakeTaskRepo) Save(_ context.Context, v run.Task, expectedRevision int64) (int64, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	if base, ok := r.u.store.Tasks[v.ID]; ok {
		if _, staged := r.u.tasks[v.ID]; !staged && base.revision != expectedRevision {
			return 0, app.ErrRevisionConflict
		}
	}
	if r.u.tasks == nil {
		r.u.tasks = map[identity.TaskID]entityRow[run.Task]{}
	}
	next := expectedRevision + 1
	r.u.tasks[v.ID] = entityRow[run.Task]{value: v, revision: next}
	return next, nil
}

// --- Attempts ---

type fakeAttemptRepo struct{ u *fakeUnitOfWork }

func (r fakeAttemptRepo) Get(_ context.Context, id identity.AttemptID) (run.Attempt, int64, error) {
	if staged, ok := r.u.attempts[id]; ok {
		return staged.value, staged.revision, nil
	}
	base, ok := r.u.store.Attempts[id]
	if !ok {
		return run.Attempt{}, 0, fmt.Errorf("%w: attempt %s", app.ErrNotFound, id)
	}
	return base.value, base.revision, nil
}

func (r fakeAttemptRepo) Save(_ context.Context, v run.Attempt, expectedRevision int64) (int64, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	if base, ok := r.u.store.Attempts[v.ID]; ok {
		if _, staged := r.u.attempts[v.ID]; !staged && base.revision != expectedRevision {
			return 0, app.ErrRevisionConflict
		}
	}
	if r.u.attempts == nil {
		r.u.attempts = map[identity.AttemptID]entityRow[run.Attempt]{}
	}
	next := expectedRevision + 1
	r.u.attempts[v.ID] = entityRow[run.Attempt]{value: v, revision: next}
	return next, nil
}

// --- Sessions ---

type fakeSessionRepo struct{ u *fakeUnitOfWork }

func (r fakeSessionRepo) Get(_ context.Context, id identity.SessionID) (run.Session, int64, error) {
	if staged, ok := r.u.sessions[id]; ok {
		return staged.value, staged.revision, nil
	}
	base, ok := r.u.store.Sessions[id]
	if !ok {
		return run.Session{}, 0, fmt.Errorf("%w: session %s", app.ErrNotFound, id)
	}
	return base.value, base.revision, nil
}

func (r fakeSessionRepo) Current(_ context.Context, attempt identity.AttemptID) (run.Session, int64, error) {
	for _, staged := range r.u.sessions {
		if staged.value.AttemptID == attempt && staged.value.State != run.SessionTerminated {
			return staged.value, staged.revision, nil
		}
	}
	for _, sess := range r.u.sessionCreated { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		if sess.AttemptID == attempt && sess.State != run.SessionTerminated {
			return sess, 1, nil
		}
	}
	for _, base := range r.u.store.Sessions {
		if base.value.AttemptID == attempt && base.value.State != run.SessionTerminated {
			if staged, ok := r.u.sessions[base.value.ID]; ok {
				return staged.value, staged.revision, nil
			}
			return base.value, base.revision, nil
		}
	}
	return run.Session{}, 0, fmt.Errorf("%w: no current session for attempt %s", app.ErrNotFound, attempt)
}

func (r fakeSessionRepo) Save(_ context.Context, v run.Session, expectedRevision int64) (int64, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	if base, ok := r.u.store.Sessions[v.ID]; ok {
		if _, staged := r.u.sessions[v.ID]; !staged && base.revision != expectedRevision {
			return 0, app.ErrRevisionConflict
		}
	}
	if r.u.sessions == nil {
		r.u.sessions = map[identity.SessionID]entityRow[run.Session]{}
	}
	next := expectedRevision + 1
	r.u.sessions[v.ID] = entityRow[run.Session]{value: v, revision: next}
	return next, nil
}

func (r fakeSessionRepo) Create(_ context.Context, v run.Session) (int64, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	r.u.sessionCreated = append(r.u.sessionCreated, v)
	return 1, nil
}

// --- Worktrees ---

type fakeWorktreeRepo struct{ u *fakeUnitOfWork }

func (r fakeWorktreeRepo) Get(_ context.Context, id identity.WorktreeID) (run.Worktree, int64, error) {
	if staged, ok := r.u.worktrees[id]; ok {
		return staged.value, staged.revision, nil
	}
	base, ok := r.u.store.Worktrees[id]
	if !ok {
		return run.Worktree{}, 0, fmt.Errorf("%w: worktree %s", app.ErrNotFound, id)
	}
	return base.value, base.revision, nil
}

func (r fakeWorktreeRepo) ByRun(_ context.Context, runID identity.RunID) (run.Worktree, int64, error) {
	for _, w := range r.u.worktreeCreated {
		if w.RunID == runID {
			return w, 1, nil
		}
	}
	for _, base := range r.u.store.Worktrees {
		if base.value.RunID == runID {
			return base.value, base.revision, nil
		}
	}
	return run.Worktree{}, 0, fmt.Errorf("%w: no worktree for run %s", app.ErrNotFound, runID)
}

func (r fakeWorktreeRepo) Create(_ context.Context, v run.Worktree) (int64, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.

	r.u.worktreeCreated = append(r.u.worktreeCreated, v)
	return 1, nil
}

func (r fakeWorktreeRepo) Save(_ context.Context, v run.Worktree, expectedRevision int64) (int64, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.

	if r.u.worktrees == nil {
		r.u.worktrees = map[identity.WorktreeID]entityRow[run.Worktree]{}
	}
	next := expectedRevision + 1
	r.u.worktrees[v.ID] = entityRow[run.Worktree]{value: v, revision: next}
	return next, nil
}

// --- Results (read-only) ---

type fakeResultRepo struct{ u *fakeUnitOfWork }

func (r fakeResultRepo) Accepted(_ context.Context, attempt identity.AttemptID) (*run.Result, error) {
	result, ok := r.u.store.Results[attempt]
	if !ok {
		return nil, nil //nolint:nilnil // "no accepted result yet" is a valid, common outcome, not an error.
	}
	return &result, nil
}

// --- Artifacts ---

type fakeArtifactRepo struct{ u *fakeUnitOfWork }

func (r fakeArtifactRepo) Save(_ context.Context, v run.Artifact) error { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.

	r.u.artifactsSaved = append(r.u.artifactsSaved, v)
	return nil
}

// --- Bindings ---

type fakeBindingRepo struct{ u *fakeUnitOfWork }

func (r fakeBindingRepo) Current(_ context.Context, session identity.SessionID) (run.RuntimeBinding, bool, error) {
	for i := len(r.u.bindingCreated) - 1; i >= 0; i-- {
		if r.u.bindingCreated[i].SessionID == session && !r.u.bindingCreated[i].Superseded {
			return r.u.bindingCreated[i], true, nil
		}
	}
	for key, b := range r.u.bindingSaved { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		if key.session == session && !b.Superseded {
			return b, true, nil
		}
	}
	base, ok := r.u.store.currentBindingLocked(session)
	return base, ok, nil
}

func (r fakeBindingRepo) Create(_ context.Context, v run.RuntimeBinding) error { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.

	r.u.bindingCreated = append(r.u.bindingCreated, v)
	return nil
}

func (r fakeBindingRepo) Save(_ context.Context, v run.RuntimeBinding) error { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.

	if r.u.bindingSaved == nil {
		r.u.bindingSaved = map[bindingKey]run.RuntimeBinding{}
	}
	r.u.bindingSaved[bindingKey{session: v.SessionID, incarnation: v.IncarnationID}] = v
	return nil
}

// --- LaunchClaims ---

type fakeLaunchClaimRepo struct{ u *fakeUnitOfWork }

func (r fakeLaunchClaimRepo) Get(_ context.Context, incarnation identity.IncarnationID) (app.LaunchClaim, bool, error) {
	claim, ok := r.u.store.LaunchClaims[incarnation]
	return claim, ok, nil
}

func (r fakeLaunchClaimRepo) Pending(_ context.Context, runID identity.RunID) ([]app.LaunchClaim, error) {
	var out []app.LaunchClaim
	for _, claim := range r.u.store.LaunchClaims { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		if claim.RunID == runID && claim.State == app.LaunchClaimExecPending {
			out = append(out, claim)
		}
	}
	return out, nil
}

func (r fakeLaunchClaimRepo) Settle(_ context.Context, incarnation identity.IncarnationID, settlement app.LaunchClaimSettlement) error { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.

	if r.u.launchClaimSettled == nil {
		r.u.launchClaimSettled = map[identity.IncarnationID]app.LaunchClaimSettlement{}
	}
	r.u.launchClaimSettled[incarnation] = settlement
	return nil
}

// --- CheckExecClaims (read-only) ---

type fakeCheckExecClaimRepo struct{ u *fakeUnitOfWork }

func (r fakeCheckExecClaimRepo) Get(_ context.Context, op identity.OperationID) (app.CheckExecClaim, bool, error) {
	claim, ok := r.u.store.CheckExecClaims[op]
	return claim, ok, nil
}

// --- Operations ---

type fakeOperationRepo struct{ u *fakeUnitOfWork }

func (r fakeOperationRepo) Create(_ context.Context, op app.Operation) error { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.

	if r.u.opCreated == nil {
		r.u.opCreated = map[identity.OperationID]app.Operation{}
	}
	r.u.opCreated[op.ID] = op
	return nil
}

func (r fakeOperationRepo) Get(_ context.Context, id identity.OperationID) (app.Operation, error) {
	if op, ok := r.u.opSaved[id]; ok {
		return op, nil
	}
	if op, ok := r.u.opCreated[id]; ok {
		return op, nil
	}
	op, ok := r.u.store.Operations[id]
	if !ok {
		return app.Operation{}, fmt.Errorf("%w: operation %s", app.ErrNotFound, id)
	}
	return op, nil
}

func (r fakeOperationRepo) Save(_ context.Context, op app.Operation) error { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.

	if r.u.opSaved == nil {
		r.u.opSaved = map[identity.OperationID]app.Operation{}
	}
	r.u.opSaved[op.ID] = op
	return nil
}

func (r fakeOperationRepo) Pending(_ context.Context, runID identity.RunID) ([]app.Operation, error) {
	var out []app.Operation
	for _, op := range r.u.store.Operations { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		if op.RunID == runID && (op.State == app.OperationPending || op.State == app.OperationReconciling) {
			out = append(out, op)
		}
	}
	return out, nil
}

// --- Transitions ---

type fakeTransitionRepo struct{ u *fakeUnitOfWork }

func (r fakeTransitionRepo) Record(_ context.Context, t app.Transition) error { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.

	r.u.transitions = append(r.u.transitions, t)
	return nil
}

// --- CheckRequests ---

type fakeCheckRequestRepo struct{ u *fakeUnitOfWork }

func (r fakeCheckRequestRepo) Get(_ context.Context, resultID identity.ResultID) (app.CheckRequest, error) {
	if cr, ok := r.u.checkRequestSaved[resultID]; ok {
		return cr, nil
	}
	cr, ok := r.u.store.CheckRequests[resultID]
	if !ok {
		return app.CheckRequest{}, fmt.Errorf("%w: check request %s", app.ErrNotFound, resultID)
	}
	return cr, nil
}

func (r fakeCheckRequestRepo) Pending(_ context.Context, runID identity.RunID) (app.CheckRequest, bool, error) {
	for id, cr := range r.u.store.CheckRequests {
		if cr.State != app.CheckRequestRequested {
			continue
		}
		attempt, ok := r.u.store.AttemptByRun[runID]
		if !ok || cr.AttemptID != attempt {
			continue
		}
		_ = id
		return cr, true, nil
	}
	return app.CheckRequest{}, false, nil
}

func (r fakeCheckRequestRepo) Save(_ context.Context, cr app.CheckRequest) error { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.

	if r.u.checkRequestSaved == nil {
		r.u.checkRequestSaved = map[identity.ResultID]app.CheckRequest{}
	}
	r.u.checkRequestSaved[cr.ResultID] = cr
	return nil
}
