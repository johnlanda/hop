package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"time"

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
// behind an in-memory store that happens to keep the exact value. A
// payload that cannot serialize is a faithful persistence FAILURE: the
// error propagates and the commit rejects atomically, exactly as a real
// JSON column write would.
func jsonRoundtripOperation(op app.Operation) (app.Operation, error) { //nolint:gocritic // hugeParam: mirrors Operation's own shape; called once per commit in tests, never a hot loop.
	var err error
	if op.Intent, err = jsonRoundtripAny(op.Intent); err != nil {
		return app.Operation{}, fmt.Errorf("operation %s intent does not serialize: %w", op.ID, err)
	}
	if op.ActEvidence, err = jsonRoundtripAny(op.ActEvidence); err != nil {
		return app.Operation{}, fmt.Errorf("operation %s act evidence does not serialize: %w", op.ID, err)
	}
	if op.Outcome, err = jsonRoundtripAny(op.Outcome); err != nil {
		return app.Operation{}, fmt.Errorf("operation %s outcome does not serialize: %w", op.ID, err)
	}
	return op, nil
}

func jsonRoundtripAny(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
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

// stagedRow is one transaction-local entity write: the value and revision
// it will hold after commit, plus the base revision the transaction first
// read. Commit re-checks baseRevision against the store so a concurrent
// writer's committed change is never silently overwritten, mirroring the
// SQLite adapter's `WHERE revision = ?` update contract.
type stagedRow[T any] struct {
	value        T
	revision     int64
	baseRevision int64
}

// stageSave applies one Save against a staged map plus its base map: a
// value already staged in this transaction re-saves against the staged
// revision; a first save validates expectedRevision against the base row.
func stageSave[K comparable, T any](staged map[K]stagedRow[T], base map[K]*entityRow[T], key K, v T, expectedRevision int64) (updated map[K]stagedRow[T], next int64, err error) {
	if prior, ok := staged[key]; ok {
		if prior.revision != expectedRevision {
			return staged, 0, app.ErrRevisionConflict
		}
		staged[key] = stagedRow[T]{value: v, revision: expectedRevision + 1, baseRevision: prior.baseRevision}
		return staged, expectedRevision + 1, nil
	}
	if baseRow, ok := base[key]; ok && baseRow.revision != expectedRevision {
		return staged, 0, app.ErrRevisionConflict
	}
	if staged == nil {
		staged = map[K]stagedRow[T]{}
	}
	staged[key] = stagedRow[T]{value: v, revision: expectedRevision + 1, baseRevision: expectedRevision}
	return staged, expectedRevision + 1, nil
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
	// ctx is the context the unit of work was begun under; its
	// cancellation before Commit fails the commit, as the real
	// transaction's does.
	ctx context.Context //nolint:containedctx // mirrors database/sql's Tx, which is bound to its BeginTx context until it ends.

	runs      map[identity.RunID]stagedRow[run.Run]
	tasks     map[identity.TaskID]stagedRow[run.Task]
	attempts  map[identity.AttemptID]stagedRow[run.Attempt]
	sessions  map[identity.SessionID]stagedRow[run.Session]
	worktrees map[identity.WorktreeID]stagedRow[run.Worktree]

	worktreeCreated []run.Worktree
	sessionCreated  []run.Session

	bindingCreated []run.RuntimeBinding
	bindingSaved   map[bindingKey]run.RuntimeBinding

	launchClaimSettled map[identity.IncarnationID]app.LaunchClaimSettlement

	opCreated map[identity.OperationID]app.Operation
	opSaved   map[identity.OperationID]app.Operation
	// opRoundtripped holds the serializability-validated JSON round trips
	// of every staged operation write, built during commit validation and
	// merged during apply.
	opRoundtripped map[identity.OperationID]app.Operation

	checkRequestSaved map[identity.ResultID]app.CheckRequest

	artifactsSaved []run.Artifact
	transitions    []app.Transition

	// Phase 3 feature-mode staged writes, additive to the Phase 2 shape
	// above: WorkflowRepositories methods stage here exactly as the
	// Phase 2 repositories stage above, merged into the store atomically
	// on Commit.
	attemptCreated     []run.Attempt
	taskCreated        []run.Task
	messagesCreated    []run.Message
	integrationCreated map[identity.IntegrationID]run.Integration
	integrationSaved   map[identity.IntegrationID]stagedRow[run.Integration]
	retryConsumed      map[identity.TaskID]int
	// worktreesRetired stages WorktreeRetirementRepositories'
	// set-once run facts.
	worktreesRetired map[identity.RunID]time.Time

	done bool
}

// dropBinding removes a session's binding rows: the shape of a launch
// whose pane.open outcome was never recorded.
func (s *fakeStore) dropBinding(sessionID identity.SessionID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Bindings, sessionID)
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

// Commit re-validates the lease against the store's current lease row and
// every staged write's preconditions, and only then merges the staged
// writes into the store in one critical section. Validation is complete
// before the first write is applied: a failed commit leaves base state
// exactly as it was, the way a real transaction's rollback would.
func (u *fakeUnitOfWork) Commit() error {
	u.ensureOpen()
	u.done = true
	s := u.store
	if s.CommitHook != nil {
		s.CommitHook(u)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.openUnitsOfWork--

	if u.ctx != nil && u.ctx.Err() != nil {
		return fmt.Errorf("app_test: commit unit of work: %w", u.ctx.Err())
	}
	if !s.matchesHeldLease(u.lease) {
		return app.ErrFenced
	}
	// Strict expiry: the lease is valid only while its expiry is strictly
	// after the transaction clock's reading; a commit at the expiry instant
	// is fenced, matching AcquireLease's takeover condition so an expiring
	// lease can never both commit and be taken over at the same instant.
	if row := s.Leases[u.lease.Run]; !row.lease.ExpiresAt.After(s.clock.Now()) {
		return app.ErrFenced
	}
	if err := u.validateStagedLocked(); err != nil {
		return err
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
	for id, row := range u.worktrees {
		s.Worktrees[id] = &entityRow[run.Worktree]{value: row.value, revision: row.revision}
	}
	for i := range u.worktreeCreated {
		w := u.worktreeCreated[i]
		s.Worktrees[w.ID] = &entityRow[run.Worktree]{value: w, revision: 1}
		s.worktreeInsertSeq++
		s.worktreeInsertOrder[w.ID] = s.worktreeInsertSeq
	}
	// sessionCreated merges BEFORE the staged sessions saves below: a
	// transaction that creates a session and then immediately transitions
	// it (Phase 3's assignment transaction — create, then Launch) stages
	// both the bare Create and a subsequent Save for the SAME id, and the
	// later logical write (the save) must win.
	for _, sess := range u.sessionCreated { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		s.Sessions[sess.ID] = &entityRow[run.Session]{value: sess, revision: 1}
		s.AttemptByRun[sess.RunID] = sess.AttemptID // Phase 2: one attempt per run
	}
	for id, row := range u.sessions {
		s.Sessions[id] = &entityRow[run.Session]{value: row.value, revision: row.revision}
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
		claim := s.LaunchClaims[incarnation]
		if claim.State == settlement.State {
			// Resettling to the same state is a no-op in the real store: the
			// first settlement's error and evidence stand.
			continue
		}
		claim.State = settlement.State
		claim.SettledAt = settlement.At
		claim.SettlementEvidence = fmt.Sprintf("pane=%s pid=%d exe=%s marker=%s", settlement.PaneID, settlement.PID, settlement.Executable, settlement.ArgvMarker)
		if settlement.Reason != "" {
			claim.Error = settlement.Reason
		}
		s.LaunchClaims[incarnation] = claim
	}
	maps.Copy(s.Operations, u.opRoundtripped)
	maps.Copy(s.CheckRequests, u.checkRequestSaved)
	s.Artifacts = append(s.Artifacts, u.artifactsSaved...)
	s.Transitions = append(s.Transitions, u.transitions...)

	for _, a := range u.attemptCreated {
		s.Attempts[a.ID] = &entityRow[run.Attempt]{value: a, revision: 1}
	}
	for _, t := range u.taskCreated { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		s.Tasks[t.ID] = &entityRow[run.Task]{value: t, revision: 1}
		// The controller computes a created task's seq from the run's
		// existing tasks; the store's own per-run seq counter (used by
		// PlanStore.CreateTask) advances past it so a later manager
		// create can never collide with a controller-created row.
		if s.taskSeqByRun[t.RunID] < t.Seq {
			s.taskSeqByRun[t.RunID] = t.Seq
		}
	}
	for _, m := range u.messagesCreated { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		// The adapter assigns the sequence inside its immediate
		// transaction, which no other writer interleaves; the fake
		// assigns it again here, under the store lock, in staging order.
		m.EnqueueSeq = nextEnqueueSeq(s, m.RunID, m.Recipient)
		s.Messages[m.ID] = m
	}
	for id, i := range u.integrationCreated { //nolint:gocritic // rangeValCopy: test fake; map iteration has no indexing alternative, and the domain snapshot is small and read-only here.
		s.Integrations[id] = &entityRow[run.Integration]{value: i, revision: 1}
	}
	for id, row := range u.integrationSaved {
		s.Integrations[id] = &entityRow[run.Integration]{value: row.value, revision: row.revision}
	}
	for runID, at := range u.worktreesRetired {
		if _, set := s.WorktreesRetiredAt[runID]; !set {
			s.WorktreesRetiredAt[runID] = at
		}
	}
	for taskID := range u.retryConsumed {
		// The attempt number was already recorded by
		// PlanStore.RequestRetry's own accepted receipt; consuming only
		// clears the pending bookkeeping row.
		delete(s.RetryRequests, taskID)
		s.RetryRequestStates[taskID] = app.RetryRequestConsumed
	}
	return nil
}

// validateOneManagerLocked mirrors the real schema's
// sessions_one_manager_per_run partial unique index: for every run this
// transaction writes a session of, the post-commit view (base sessions
// overlaid with this transaction's creates, then its saves) holds at most
// one manager-role session that is neither lost nor terminated. A cold
// relaunch that marks the predecessor lost and creates the successor in
// one transaction passes; a second live manager fails the whole commit.
func (u *fakeUnitOfWork) validateOneManagerLocked() error {
	s := u.store
	merged := map[identity.SessionID]run.Session{}
	touched := map[identity.RunID]bool{}
	for i := range u.sessionCreated {
		merged[u.sessionCreated[i].ID] = u.sessionCreated[i]
		touched[u.sessionCreated[i].RunID] = true
	}
	for id, row := range u.sessions {
		merged[id] = row.value
		touched[row.value.RunID] = true
	}
	live := map[identity.RunID]int{}
	count := func(v *run.Session) {
		if touched[v.RunID] && v.Role == run.RoleManager && v.State != run.SessionLost && v.State != run.SessionTerminated {
			live[v.RunID]++
		}
	}
	for id, base := range s.Sessions {
		if _, overlaid := merged[id]; overlaid {
			continue
		}
		count(&base.value)
	}
	for id := range merged {
		v := merged[id]
		count(&v)
	}
	for runID, n := range live {
		if n > 1 {
			return fmt.Errorf("app_test: UNIQUE sessions_one_manager_per_run violated: run %s would hold %d non-terminal manager sessions", runID, n)
		}
	}
	return nil
}

func (u *fakeUnitOfWork) Rollback() error {
	if !u.done {
		u.store.mu.Lock()
		u.store.openUnitsOfWork--
		u.store.mu.Unlock()
	}
	u.done = true
	return nil
}

// validateStagedLocked checks every staged write's precondition against the
// store's current base state, with no mutation: entity revisions still
// match what this transaction first read, created bindings respect
// UNIQUE(session_id, incarnation_id), and launch-claim settlements are
// legal from the claim's current state.
func (u *fakeUnitOfWork) validateStagedLocked() error {
	s := u.store
	for id, row := range u.runs {
		if base, ok := s.Runs[id]; ok && base.revision != row.baseRevision {
			return app.ErrRevisionConflict
		}
	}
	for id, row := range u.tasks {
		if base, ok := s.Tasks[id]; ok && base.revision != row.baseRevision {
			return app.ErrRevisionConflict
		}
	}
	for id, row := range u.attempts {
		if base, ok := s.Attempts[id]; ok && base.revision != row.baseRevision {
			return app.ErrRevisionConflict
		}
	}
	for id, row := range u.sessions {
		if base, ok := s.Sessions[id]; ok && base.revision != row.baseRevision {
			return app.ErrRevisionConflict
		}
	}
	for id, row := range u.worktrees {
		if base, ok := s.Worktrees[id]; ok && base.revision != row.baseRevision {
			return app.ErrRevisionConflict
		}
	}
	for id, row := range u.integrationSaved {
		if base, ok := s.Integrations[id]; ok && base.revision != row.baseRevision {
			return app.ErrRevisionConflict
		}
	}

	if err := u.validateOneManagerLocked(); err != nil {
		return err
	}

	stagedKeys := map[bindingKey]bool{}
	for i := range u.bindingCreated {
		key := bindingKey{session: u.bindingCreated[i].SessionID, incarnation: u.bindingCreated[i].IncarnationID}
		if stagedKeys[key] {
			return fmt.Errorf("app_test: binding key %s/%s staged twice in one transaction", key.session, key.incarnation)
		}
		stagedKeys[key] = true
		for _, existing := range s.Bindings[key.session] { //nolint:gocritic // rangeValCopy: test fake; the base history is small and read-only here.
			if existing.IncarnationID == key.incarnation {
				return fmt.Errorf("app_test: UNIQUE(session_id, incarnation_id) violated for binding %s/%s", key.session, key.incarnation)
			}
		}
	}

	for incarnation, settlement := range u.launchClaimSettled {
		claim, ok := s.LaunchClaims[incarnation]
		if !ok {
			return fmt.Errorf("%w: launch claim %s", app.ErrNotFound, incarnation)
		}
		// Settle is legal only from exec_pending, only to execed or
		// exec_failed; resettling to the same state is idempotent
		// (LaunchClaimRepository's documented contract, as the sqlite
		// adapter enforces it).
		if settlement.State != app.LaunchClaimExeced && settlement.State != app.LaunchClaimExecFailed {
			return fmt.Errorf("app_test: launch claim %s: %q is not a settlement state", incarnation, settlement.State)
		}
		if claim.State != app.LaunchClaimExecPending && claim.State != settlement.State {
			return fmt.Errorf("app_test: launch claim %s cannot settle from %s to %s", incarnation, claim.State, settlement.State)
		}
	}

	// Serializability is validated before anything merges: a payload that
	// does not survive the JSON round trip fails the whole commit.
	u.opRoundtripped = map[identity.OperationID]app.Operation{}
	for id, op := range u.opCreated { //nolint:gocritic // rangeValCopy: test fake; the staged journal is small and read once here.
		roundtripped, err := jsonRoundtripOperation(op)
		if err != nil {
			return fmt.Errorf("app_test: %w", err)
		}
		u.opRoundtripped[id] = roundtripped
	}
	for id, op := range u.opSaved { //nolint:gocritic // rangeValCopy: test fake; the staged journal is small and read once here.
		roundtripped, err := jsonRoundtripOperation(op)
		if err != nil {
			return fmt.Errorf("app_test: %w", err)
		}
		u.opRoundtripped[id] = roundtripped
	}
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
	staged, next, err := stageSave(r.u.runs, r.u.store.Runs, v.ID, v, expectedRevision)
	r.u.runs = staged
	return next, err
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
	staged, next, err := stageSave(r.u.tasks, r.u.store.Tasks, v.ID, v, expectedRevision)
	r.u.tasks = staged
	return next, err
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
	staged, next, err := stageSave(r.u.attempts, r.u.store.Attempts, v.ID, v, expectedRevision)
	r.u.attempts = staged
	return next, err
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

// sessionCurrent reports whether a session is the attempt's current one:
// neither terminated nor lost — a cold relaunch's replaced session is
// history, never current.
func sessionCurrent(s *run.Session) bool {
	return s.State != run.SessionTerminated && s.State != run.SessionLost
}

func (r fakeSessionRepo) Current(_ context.Context, attempt identity.AttemptID) (run.Session, int64, error) {
	for _, staged := range r.u.sessions {
		if staged.value.AttemptID == attempt && sessionCurrent(&staged.value) {
			return staged.value, staged.revision, nil
		}
	}
	for i := range r.u.sessionCreated {
		if r.u.sessionCreated[i].AttemptID == attempt && sessionCurrent(&r.u.sessionCreated[i]) {
			return r.u.sessionCreated[i], 1, nil
		}
	}
	for _, base := range r.u.store.Sessions {
		if base.value.AttemptID == attempt && sessionCurrent(&base.value) {
			if staged, ok := r.u.sessions[base.value.ID]; ok {
				return staged.value, staged.revision, nil
			}
			return base.value, base.revision, nil
		}
	}
	return run.Session{}, 0, fmt.Errorf("%w: no current session for attempt %s", app.ErrNotFound, attempt)
}

func (r fakeSessionRepo) Save(_ context.Context, v run.Session, expectedRevision int64) (int64, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	staged, next, err := stageSave(r.u.sessions, r.u.store.Sessions, v.ID, v, expectedRevision)
	r.u.sessions = staged
	return next, err
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
	for i := range r.u.worktreeCreated {
		if r.u.worktreeCreated[i].RunID == runID {
			return r.u.worktreeCreated[i], 1, nil
		}
	}
	for _, base := range r.u.store.Worktrees {
		if base.value.RunID == runID {
			return base.value, base.revision, nil
		}
	}
	return run.Worktree{}, 0, fmt.Errorf("%w: no worktree for run %s", app.ErrNotFound, runID)
}

// Create enforces the real store's insert contract (sqlite
// worktreeRepository.Create): the row belongs to the leased run, a linked
// attempt exists and belongs to that same run, and the path is unique
// across every worktree row (the schema's UNIQUE(path)).
func (r fakeWorktreeRepo) Create(_ context.Context, v run.Worktree) (int64, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	if v.RunID != r.u.lease.Run {
		return 0, fmt.Errorf("%w: worktree %s belongs to run %s, not the leased run %s", app.ErrFenced, v.ID, v.RunID, r.u.lease.Run)
	}
	if v.AttemptID != "" {
		owner, err := r.u.attemptOwnerRun(v.AttemptID)
		if err != nil {
			return 0, err
		}
		if owner != r.u.lease.Run {
			return 0, fmt.Errorf("%w: worktree %s's attempt %s belongs to run %s, not the leased run %s", app.ErrFenced, v.ID, v.AttemptID, owner, r.u.lease.Run)
		}
	}
	for i := range r.u.worktreeCreated {
		if r.u.worktreeCreated[i].Path == v.Path {
			return 0, fmt.Errorf("app_test: UNIQUE(path) violated for worktree %s", v.ID)
		}
	}
	for _, base := range r.u.store.Worktrees {
		if base.value.Path == v.Path {
			return 0, fmt.Errorf("app_test: UNIQUE(path) violated for worktree %s", v.ID)
		}
	}
	r.u.worktreeCreated = append(r.u.worktreeCreated, v)
	return 1, nil
}

// attemptOwnerRun resolves an attempt's owning run through its task, from
// this transaction's staged rows first and the store's base rows second —
// the fake's counterpart of the real store's runOfAttempt, including its
// ErrNotFound for an unknown attempt or task.
func (u *fakeUnitOfWork) attemptOwnerRun(id identity.AttemptID) (identity.RunID, error) {
	var (
		taskID identity.TaskID
		found  bool
	)
	if staged, ok := u.attempts[id]; ok {
		taskID, found = staged.value.TaskID, true
	}
	for i := range u.attemptCreated {
		if !found && u.attemptCreated[i].ID == id {
			taskID, found = u.attemptCreated[i].TaskID, true
		}
	}
	if base, ok := u.store.Attempts[id]; ok && !found {
		taskID, found = base.value.TaskID, true
	}
	if !found {
		return "", fmt.Errorf("%w: attempt %s", app.ErrNotFound, id)
	}
	if staged, ok := u.tasks[taskID]; ok {
		return staged.value.RunID, nil
	}
	for i := range u.taskCreated {
		if u.taskCreated[i].ID == taskID {
			return u.taskCreated[i].RunID, nil
		}
	}
	if base, ok := u.store.Tasks[taskID]; ok {
		return base.value.RunID, nil
	}
	return "", fmt.Errorf("%w: task %s of attempt %s", app.ErrNotFound, taskID, id)
}

func (r fakeWorktreeRepo) Save(_ context.Context, v run.Worktree, expectedRevision int64) (int64, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	staged, next, err := stageSave(r.u.worktrees, r.u.store.Worktrees, v.ID, v, expectedRevision)
	r.u.worktrees = staged
	return next, err
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
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (r fakeOperationRepo) ByKind(_ context.Context, runID identity.RunID, kind app.OperationKind) ([]app.Operation, error) {
	var out []app.Operation
	for _, op := range r.u.store.Operations { //nolint:gocritic // rangeValCopy: test fake; the domain snapshot is small and read-only here, and indexing would only obscure the loop.
		if op.RunID == runID && op.Kind == kind {
			out = append(out, op)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
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
