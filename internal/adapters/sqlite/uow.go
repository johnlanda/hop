package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// unitOfWork is one controller transaction, fenced by the lease it was
// begun with. Every repository it hands out writes through the same
// immediate transaction; nothing is visible to other connections until
// Commit re-validates the lease and commits.
type unitOfWork struct {
	store *Store
	tx    *sql.Tx
	ctx   context.Context
	lease app.Lease
	done  bool
}

var _ app.UnitOfWork = (*unitOfWork)(nil)

func (u *unitOfWork) Runs() app.RunRepository                 { return runRepository{u} }
func (u *unitOfWork) Tasks() app.TaskRepository               { return taskRepository{u} }
func (u *unitOfWork) Attempts() app.AttemptRepository         { return attemptRepository{u} }
func (u *unitOfWork) Sessions() app.SessionRepository         { return sessionRepository{u} }
func (u *unitOfWork) Worktrees() app.WorktreeRepository       { return worktreeRepository{u} }
func (u *unitOfWork) Results() app.ResultRepository           { return resultRepository{u} }
func (u *unitOfWork) Artifacts() app.ArtifactRepository       { return artifactRepository{u} }
func (u *unitOfWork) Bindings() app.BindingRepository         { return bindingRepository{u} }
func (u *unitOfWork) LaunchClaims() app.LaunchClaimRepository { return launchClaimRepository{u} }
func (u *unitOfWork) Operations() app.OperationRepository     { return operationRepository{u} }
func (u *unitOfWork) Transitions() app.TransitionRepository   { return transitionRepository{u} }
func (u *unitOfWork) CheckRequests() app.CheckRequestRepository {
	return checkRequestRepository{u}
}

func (u *unitOfWork) CheckExecClaims() app.CheckExecClaimRepository {
	return checkExecClaimRepository{u}
}

// Commit re-reads the lease inside the transaction and fails with
// ErrFenced unless it still matches (run, controller, generation, held)
// and is unexpired at the commit-time clock reading; only then does the
// transaction commit.
func (u *unitOfWork) Commit() error {
	if u.done {
		return errors.New("sqlite: unit of work is already settled")
	}
	if err := validateLease(u.ctx, u.tx, u.lease, u.store.now()); err != nil {
		u.done = true
		if rbErr := u.tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, rbErr)
		}
		return err
	}
	u.done = true
	if err := u.tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit unit of work: %w", err)
	}
	return nil
}

// Rollback discards the unit of work. After a settled Commit or Rollback it
// is a no-op, so a deferred Rollback is always safe.
func (u *unitOfWork) Rollback() error {
	if u.done {
		return nil
	}
	u.done = true
	if err := u.tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("sqlite: rollback unit of work: %w", err)
	}
	return nil
}

// requireLeasedRun refuses a mutation whose target belongs to a run other
// than the leased one: a unit of work is one run's exclusive write
// authority, never a general database handle, so every repository write
// resolves its target's persisted owning run and compares it to the
// lease's run before any SQL executes. The refusal is ErrFenced — the
// caller holds no authority over that run.
func (u *unitOfWork) requireLeasedRun(owner identity.RunID, kind, id string) error {
	if owner != u.lease.Run {
		return fmt.Errorf("sqlite: %s %s belongs to run %s, not the leased run %s: %w", kind, id, owner, u.lease.Run, app.ErrFenced)
	}
	return nil
}

// saveOwnerLookup adapts a revision-bearing Save's ownership lookup: a
// missing row is reported as ErrRevisionConflict, exactly as the update's
// zero affected rows would have reported it before the ownership check ran
// first (design section 4's contract for revision-bearing updates). Every
// other lookup error passes through unchanged.
func saveOwnerLookup(err error, kind, id string, expectedRevision int64) error {
	if errors.Is(err, app.ErrNotFound) {
		return fmt.Errorf("sqlite: %s %s at revision %d: %w", kind, id, expectedRevision, app.ErrRevisionConflict)
	}
	return err
}

// saveEntity runs one optimistic-concurrency update: zero affected rows is
// ErrRevisionConflict, and the entity's revision advances by one.
func saveEntity(result sql.Result, execErr error, kind, id string, expectedRevision int64) (int64, error) {
	if execErr != nil {
		return 0, fmt.Errorf("sqlite: save %s %s: %w", kind, id, execErr)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: save %s %s rows affected: %w", kind, id, err)
	}
	if affected == 0 {
		return 0, fmt.Errorf("sqlite: %s %s at revision %d: %w", kind, id, expectedRevision, app.ErrRevisionConflict)
	}
	return expectedRevision + 1, nil
}

// runRepository loads and saves Run values inside the unit of work.
type runRepository struct{ u *unitOfWork }

func (r runRepository) Get(ctx context.Context, id identity.RunID) (run.Run, int64, error) {
	return getRun(ctx, r.u.tx, id)
}

// Save persists the run's mutable columns. The stop request is monotonic:
// stop_requested_at is set once, to the run's update time, and never
// cleared or moved by a later save.
func (r runRepository) Save(ctx context.Context, v run.Run, expectedRevision int64) (int64, error) { //nolint:gocritic // hugeParam: the port passes domain values by value; the repository mirrors its signature.
	if err := r.u.requireLeasedRun(v.ID, "run", v.ID.String()); err != nil {
		return 0, err
	}
	at := formatTime(v.UpdatedAt)
	result, err := r.u.tx.ExecContext(ctx,
		`UPDATE runs SET state = ?, stop_requested_at = CASE WHEN ? = 1 THEN COALESCE(stop_requested_at, ?) ELSE stop_requested_at END, updated_at = ?, revision = revision + 1
		 WHERE id = ? AND revision = ?`,
		string(v.State), boolToInt(v.StopRequested), at, at, v.ID.String(), expectedRevision,
	)
	return saveEntity(result, err, "run", v.ID.String(), expectedRevision)
}

// taskRepository loads and saves Task values inside the unit of work.
type taskRepository struct{ u *unitOfWork }

func (r taskRepository) Get(ctx context.Context, id identity.TaskID) (run.Task, int64, error) {
	return getTask(ctx, r.u.tx, id)
}

// Save persists the task's mutable columns. The mailbox flag is persisted
// both ways under the ordinary revision discipline: a controller
// settlement closes it (Task.CloseMailbox + Save, keeping the original
// closure time on a re-save), and only a value loaded at the current
// revision can write — a retry's reopen goes through
// PlanStore.RequestRetry's own transaction, never a stale Save.
func (r taskRepository) Save(ctx context.Context, v run.Task, expectedRevision int64) (int64, error) { //nolint:gocritic // hugeParam: the port passes domain values by value; the repository mirrors its signature.
	owner, err := runOfTask(ctx, r.u.tx, v.ID)
	if err != nil {
		return 0, saveOwnerLookup(err, "task", v.ID.String(), expectedRevision)
	}
	if scopeErr := r.u.requireLeasedRun(owner, "task", v.ID.String()); scopeErr != nil {
		return 0, scopeErr
	}
	at := formatTime(v.UpdatedAt)
	result, err := r.u.tx.ExecContext(ctx,
		`UPDATE tasks SET state = ?, mailbox_closed_at = CASE WHEN ? = 1 THEN COALESCE(mailbox_closed_at, ?) ELSE NULL END, updated_at = ?, revision = revision + 1 WHERE id = ? AND revision = ?`,
		string(v.State), boolToInt(v.MailboxClosed), at, at, v.ID.String(), expectedRevision,
	)
	return saveEntity(result, err, "task", v.ID.String(), expectedRevision)
}

// attemptRepository loads and saves Attempt values inside the unit of work.
type attemptRepository struct{ u *unitOfWork }

func (r attemptRepository) Get(ctx context.Context, id identity.AttemptID) (run.Attempt, int64, error) {
	return getAttempt(ctx, r.u.tx, id)
}

func (r attemptRepository) Save(ctx context.Context, v run.Attempt, expectedRevision int64) (int64, error) { //nolint:gocritic // hugeParam: the port passes domain values by value; the repository mirrors its signature.
	owner, err := runOfAttempt(ctx, r.u.tx, v.ID)
	if err != nil {
		return 0, saveOwnerLookup(err, "attempt", v.ID.String(), expectedRevision)
	}
	if scopeErr := r.u.requireLeasedRun(owner, "attempt", v.ID.String()); scopeErr != nil {
		return 0, scopeErr
	}
	result, err := r.u.tx.ExecContext(ctx,
		`UPDATE attempts SET state = ?, updated_at = ?, revision = revision + 1 WHERE id = ? AND revision = ?`,
		string(v.State), formatTime(v.UpdatedAt), v.ID.String(), expectedRevision,
	)
	return saveEntity(result, err, "attempt", v.ID.String(), expectedRevision)
}

// sessionRepository loads, creates and saves Session values inside the unit
// of work.
type sessionRepository struct{ u *unitOfWork }

func (r sessionRepository) Get(ctx context.Context, id identity.SessionID) (run.Session, int64, error) {
	return getSession(ctx, r.u.tx, id)
}

func (r sessionRepository) Current(ctx context.Context, attempt identity.AttemptID) (run.Session, int64, error) {
	s, revision, ok, err := currentSession(ctx, r.u.tx, attempt)
	if err != nil {
		return run.Session{}, 0, err
	}
	if !ok {
		return run.Session{}, 0, fmt.Errorf("sqlite: current session of attempt %s: %w", attempt, app.ErrNotFound)
	}
	return s, revision, nil
}

// Save persists the session's mutable columns. The native reference is
// immutable once set: a stored reference and its source are never
// overwritten, mirroring the domain's assignment rule.
func (r sessionRepository) Save(ctx context.Context, v run.Session, expectedRevision int64) (int64, error) { //nolint:gocritic // hugeParam: the port passes domain values by value; the repository mirrors its signature.
	owner, err := runOfSession(ctx, r.u.tx, v.ID)
	if err != nil {
		return 0, saveOwnerLookup(err, "session", v.ID.String(), expectedRevision)
	}
	if scopeErr := r.u.requireLeasedRun(owner, "session", v.ID.String()); scopeErr != nil {
		return 0, scopeErr
	}
	result, err := r.u.tx.ExecContext(ctx,
		`UPDATE sessions SET state = ?, native_session_ref = COALESCE(native_session_ref, ?), native_ref_source = COALESCE(native_ref_source, ?), updated_at = ?, revision = revision + 1
		 WHERE id = ? AND revision = ?`,
		string(v.State), nullString(v.NativeSessionRef), nullString(string(v.NativeRefSource)),
		formatTime(v.UpdatedAt), v.ID.String(), expectedRevision,
	)
	return saveEntity(result, err, "session", v.ID.String(), expectedRevision)
}

// Create inserts a session row. AttemptID is optional — NULL for a manager
// session, the only role with none — and a non-empty one must belong to
// the leased run. ParentSessionID is persisted as given: the one-level
// delegation rule (parent is the run's manager and has no parent itself)
// is validated by the application transaction that constructs the child
// (run.NewChildSession), the same division Phase 2 used for the DAG rule;
// the store only requires a named parent to be a same-run session row.
func (r sessionRepository) Create(ctx context.Context, v run.Session) (int64, error) { //nolint:gocritic // hugeParam: the port passes domain values by value; the repository mirrors its signature.
	if err := r.u.requireLeasedRun(v.RunID, "session", v.ID.String()); err != nil {
		return 0, err
	}
	if v.AttemptID != "" {
		attemptOwner, err := runOfAttempt(ctx, r.u.tx, v.AttemptID)
		if err != nil {
			return 0, err
		}
		if scopeErr := r.u.requireLeasedRun(attemptOwner, "session's attempt", v.AttemptID.String()); scopeErr != nil {
			return 0, scopeErr
		}
	}
	var parentID any
	if v.ParentSessionID != nil {
		parentOwner, err := runOfSession(ctx, r.u.tx, *v.ParentSessionID)
		if err != nil {
			return 0, err
		}
		if scopeErr := r.u.requireLeasedRun(parentOwner, "session's parent", v.ParentSessionID.String()); scopeErr != nil {
			return 0, scopeErr
		}
		parentID = v.ParentSessionID.String()
	}
	if _, err := r.u.tx.ExecContext(ctx,
		`INSERT INTO sessions (id, run_id, attempt_id, parent_session_id, role, harness, native_session_ref, native_ref_source, state, revision, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
		v.ID.String(), v.RunID.String(), nullString(v.AttemptID.String()), parentID, string(v.Role), string(v.Harness),
		nullString(v.NativeSessionRef), nullString(string(v.NativeRefSource)), string(v.State), formatTime(v.UpdatedAt),
	); err != nil {
		return 0, fmt.Errorf("sqlite: create session %s: %w", v.ID, err)
	}
	return 1, nil
}

// worktreeRepository loads, creates and saves the run's worktree inside the
// unit of work.
type worktreeRepository struct{ u *unitOfWork }

func (r worktreeRepository) Get(ctx context.Context, id identity.WorktreeID) (run.Worktree, int64, error) {
	return getWorktree(ctx, r.u.tx, "id", id.String())
}

func (r worktreeRepository) ByRun(ctx context.Context, runID identity.RunID) (run.Worktree, int64, error) {
	return getWorktree(ctx, r.u.tx, "run_id", runID.String())
}

// Create inserts a worktree row. A feature-mode row names its attempt,
// which must belong to the same leased run, and its verified base commit;
// a solo row leaves both NULL.
func (r worktreeRepository) Create(ctx context.Context, v run.Worktree) (int64, error) { //nolint:gocritic // hugeParam: the port passes domain values by value; the repository mirrors its signature.
	if err := r.u.requireLeasedRun(v.RunID, "worktree", v.ID.String()); err != nil {
		return 0, err
	}
	if v.AttemptID != "" {
		attemptOwner, err := runOfAttempt(ctx, r.u.tx, v.AttemptID)
		if err != nil {
			return 0, err
		}
		if scopeErr := r.u.requireLeasedRun(attemptOwner, "worktree's attempt", v.AttemptID.String()); scopeErr != nil {
			return 0, scopeErr
		}
	}
	if _, err := r.u.tx.ExecContext(ctx,
		`INSERT INTO worktrees (id, repository_id, run_id, path, branch, state, revision, created_at, attempt_id, base_commit) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
		v.ID.String(), v.RepositoryID.String(), v.RunID.String(), v.Path, v.Branch, string(v.State), formatTime(r.u.store.now()),
		nullString(v.AttemptID.String()), nullString(v.BaseCommit),
	); err != nil {
		return 0, fmt.Errorf("sqlite: create worktree %s: %w", v.ID, err)
	}
	return 1, nil
}

func (r worktreeRepository) Save(ctx context.Context, v run.Worktree, expectedRevision int64) (int64, error) { //nolint:gocritic // hugeParam: the port passes domain values by value; the repository mirrors its signature.
	owner, err := runOfWorktree(ctx, r.u.tx, v.ID)
	if err != nil {
		return 0, saveOwnerLookup(err, "worktree", v.ID.String(), expectedRevision)
	}
	if scopeErr := r.u.requireLeasedRun(owner, "worktree", v.ID.String()); scopeErr != nil {
		return 0, scopeErr
	}
	result, err := r.u.tx.ExecContext(ctx,
		`UPDATE worktrees SET state = ?, revision = revision + 1 WHERE id = ? AND revision = ?`,
		string(v.State), v.ID.String(), expectedRevision,
	)
	return saveEntity(result, err, "worktree", v.ID.String(), expectedRevision)
}

// resultRepository reads accepted results inside the unit of work; results
// are written only by SubmissionStore.SubmitResult.
type resultRepository struct{ u *unitOfWork }

func (r resultRepository) Accepted(ctx context.Context, attempt identity.AttemptID) (*run.Result, error) {
	return acceptedResult(ctx, r.u.tx, attempt)
}

// artifactRepository records artifact references inside the unit of work.
type artifactRepository struct{ u *unitOfWork }

func (r artifactRepository) Save(ctx context.Context, a run.Artifact) error { //nolint:gocritic // hugeParam: the port passes domain values by value; the repository mirrors its signature.
	if err := r.u.requireLeasedRun(a.RunID, "artifact", a.ID.String()); err != nil {
		return err
	}
	var resultID any
	if a.ResultID != nil {
		owner, err := runOfResult(ctx, r.u.tx, *a.ResultID)
		if err != nil {
			return err
		}
		if err := r.u.requireLeasedRun(owner, "artifact's result", a.ResultID.String()); err != nil {
			return err
		}
		resultID = a.ResultID.String()
	}
	if _, err := r.u.tx.ExecContext(ctx,
		`INSERT INTO artifacts (id, run_id, result_id, kind, path, digest, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.ID.String(), a.RunID.String(), resultID, string(a.Kind), a.Path, a.Digest, formatTime(r.u.store.now()),
	); err != nil {
		return fmt.Errorf("sqlite: save artifact %s: %w", a.ID, err)
	}
	return nil
}

// bindingRepository reads and appends RuntimeBinding history inside the
// unit of work.
type bindingRepository struct{ u *unitOfWork }

func (r bindingRepository) Current(ctx context.Context, session identity.SessionID) (run.RuntimeBinding, bool, error) {
	return currentBinding(ctx, r.u.tx, session)
}

func (r bindingRepository) Create(ctx context.Context, b run.RuntimeBinding) error { //nolint:gocritic // hugeParam: the port passes domain values by value; the repository mirrors its signature.
	owner, err := runOfSession(ctx, r.u.tx, b.SessionID)
	if err != nil {
		return err
	}
	if scopeErr := r.u.requireLeasedRun(owner, "binding's session", b.SessionID.String()); scopeErr != nil {
		return scopeErr
	}
	id, err := newUUID()
	if err != nil {
		return err
	}
	occupant, err := encodeOccupant(b.Occupant)
	if err != nil {
		return err
	}
	if _, err := r.u.tx.ExecContext(ctx,
		`INSERT INTO runtime_bindings (id, session_id, incarnation_id, server_socket_path, server_instance, workspace_id, tab_id, pane_id, creation_label, launch_kind, occupant_evidence, observed_at, superseded, superseded_at, superseded_evidence)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, b.SessionID.String(), b.IncarnationID.String(), b.ServerSocketPath, nullString(b.ServerInstance),
		b.WorkspaceID, b.TabID, b.PaneID,
		b.CreationLabel, string(b.LaunchKind), occupant, formatTime(b.ObservedAt),
		boolToInt(b.Superseded), nullTime(b.SupersededAt), nullString(b.SupersededEvidence),
	); err != nil {
		return fmt.Errorf("sqlite: create binding %s/%s: %w", b.SessionID, b.IncarnationID, err)
	}
	return nil
}

func (r bindingRepository) Save(ctx context.Context, b run.RuntimeBinding) error { //nolint:gocritic // hugeParam: the port passes domain values by value; the repository mirrors its signature.
	owner, err := runOfSession(ctx, r.u.tx, b.SessionID)
	if err != nil {
		return err
	}
	if scopeErr := r.u.requireLeasedRun(owner, "binding's session", b.SessionID.String()); scopeErr != nil {
		return scopeErr
	}
	occupant, err := encodeOccupant(b.Occupant)
	if err != nil {
		return err
	}
	result, err := r.u.tx.ExecContext(ctx,
		`UPDATE runtime_bindings SET occupant_evidence = ?, observed_at = ?, superseded = ?, superseded_at = ?, superseded_evidence = ?
		 WHERE session_id = ? AND incarnation_id = ?`,
		occupant, formatTime(b.ObservedAt), boolToInt(b.Superseded), nullTime(b.SupersededAt), nullString(b.SupersededEvidence),
		b.SessionID.String(), b.IncarnationID.String(),
	)
	if err != nil {
		return fmt.Errorf("sqlite: save binding %s/%s: %w", b.SessionID, b.IncarnationID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: save binding rows affected: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("sqlite: binding %s/%s: %w", b.SessionID, b.IncarnationID, app.ErrNotFound)
	}
	return nil
}

// encodeOccupant renders occupant evidence as its stored JSON, or NULL.
func encodeOccupant(o *run.OccupantEvidence) (any, error) {
	if o == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(occupantJSON{Label: o.Label, ArgvMarker: o.ArgvMarker, PID: o.PID})
	if err != nil {
		return nil, fmt.Errorf("sqlite: encode occupant evidence: %w", err)
	}
	return string(encoded), nil
}

// launchClaimRepository reads launch claims and records controller-side
// settlement inside the unit of work. hop launch's own writes go through
// SubmissionStore.
type launchClaimRepository struct{ u *unitOfWork }

func (r launchClaimRepository) Get(ctx context.Context, incarnation identity.IncarnationID) (app.LaunchClaim, bool, error) {
	claim, err := getLaunchClaim(ctx, r.u.tx, incarnation)
	if err != nil {
		return app.LaunchClaim{}, false, err
	}
	if claim == nil {
		return app.LaunchClaim{}, false, nil
	}
	return *claim, true, nil
}

func (r launchClaimRepository) Pending(ctx context.Context, runID identity.RunID) ([]app.LaunchClaim, error) {
	rows, err := r.u.tx.QueryContext(ctx,
		`SELECT incarnation_id FROM launch_claims WHERE run_id = ? AND state = ? ORDER BY claimed_at, rowid`,
		runID.String(), string(app.LaunchClaimExecPending),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list pending launch claims of run %s: %w", runID, err)
	}
	defer rows.Close() //nolint:errcheck // the deferred close of a fully-iterated read cursor has no failure the rows.Err check below misses.
	var incarnations []identity.IncarnationID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("sqlite: scan pending launch claim: %w", err)
		}
		incarnation, err := identity.ParseIncarnationID(raw)
		if err != nil {
			return nil, fmt.Errorf("sqlite: pending launch claim incarnation id: %w", err)
		}
		incarnations = append(incarnations, incarnation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate pending launch claims: %w", err)
	}
	claims := make([]app.LaunchClaim, 0, len(incarnations))
	for _, incarnation := range incarnations {
		claim, err := getLaunchClaim(ctx, r.u.tx, incarnation)
		if err != nil {
			return nil, err
		}
		if claim != nil {
			claims = append(claims, *claim)
		}
	}
	return claims, nil
}

// settlementEvidenceJSON is the stored form of a settlement's corroborating
// observation.
type settlementEvidenceJSON struct {
	PaneID     string `json:"pane_id"`
	PID        int    `json:"pid"`
	Executable string `json:"executable"`
	ArgvMarker string `json:"argv_marker"`
}

// Settle moves a claim to settlement.State: execed or exec_failed, each
// legal only from exec_pending. Resettling to the same state is idempotent;
// any other transition, and a missing claim, is an error.
func (r launchClaimRepository) Settle(ctx context.Context, incarnation identity.IncarnationID, settlement app.LaunchClaimSettlement) error { //nolint:gocritic // hugeParam: the port passes the settlement value; the repository mirrors its signature.
	if settlement.State != app.LaunchClaimExeced && settlement.State != app.LaunchClaimExecFailed {
		return fmt.Errorf("sqlite: launch claim %s: %q is not a settlement state", incarnation, settlement.State)
	}
	claim, err := getLaunchClaim(ctx, r.u.tx, incarnation)
	if err != nil {
		return err
	}
	if claim == nil {
		return fmt.Errorf("sqlite: launch claim of incarnation %s: %w", incarnation, app.ErrNotFound)
	}
	if scopeErr := r.u.requireLeasedRun(claim.RunID, "launch claim", incarnation.String()); scopeErr != nil {
		return scopeErr
	}
	if claim.State == settlement.State {
		return nil
	}
	if claim.State != app.LaunchClaimExecPending {
		return fmt.Errorf("sqlite: launch claim %s is %s and cannot settle to %s", incarnation, claim.State, settlement.State)
	}
	evidence, err := json.Marshal(settlementEvidenceJSON{
		PaneID:     settlement.PaneID,
		PID:        settlement.PID,
		Executable: settlement.Executable,
		ArgvMarker: settlement.ArgvMarker,
	})
	if err != nil {
		return fmt.Errorf("sqlite: encode settlement evidence: %w", err)
	}
	if _, err := r.u.tx.ExecContext(ctx,
		`UPDATE launch_claims SET state = ?, error = ?, settled_at = ?, settlement_evidence = ? WHERE incarnation_id = ?`,
		string(settlement.State), nullString(settlement.Reason), formatTime(settlement.At), string(evidence), incarnation.String(),
	); err != nil {
		return fmt.Errorf("sqlite: settle launch claim %s: %w", incarnation, err)
	}
	return nil
}

// operationRepository records and updates the operation journal inside the
// unit of work.
type operationRepository struct{ u *unitOfWork }

// encodeJournalValue renders a journal payload as stored JSON; nil is NULL.
func encodeJournalValue(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("sqlite: encode journal payload: %w", err)
	}
	return string(encoded), nil
}

// decodeJournalValue parses a stored journal payload; NULL is nil. The
// store persists payloads without interpreting them, so a read-back value
// is the generic decoding of the JSON, not the Go type that produced it.
func decodeJournalValue(v sql.NullString) (any, error) {
	if !v.Valid {
		return nil, nil
	}
	var out any
	if err := json.Unmarshal([]byte(v.String), &out); err != nil {
		return nil, fmt.Errorf("sqlite: decode journal payload: %w", err)
	}
	return out, nil
}

func (r operationRepository) Create(ctx context.Context, op app.Operation) error { //nolint:gocritic // hugeParam: the port passes journal rows by value; the repository mirrors its signature.
	if err := r.u.requireLeasedRun(op.RunID, "operation", op.ID.String()); err != nil {
		return err
	}
	intent, err := encodeJournalValue(op.Intent)
	if err != nil {
		return err
	}
	if intent == nil {
		intent = "null"
	}
	actEvidence, err := encodeJournalValue(op.ActEvidence)
	if err != nil {
		return err
	}
	outcome, err := encodeJournalValue(op.Outcome)
	if err != nil {
		return err
	}
	if _, err := r.u.tx.ExecContext(ctx,
		`INSERT INTO operations (id, run_id, generation, kind, state, intent, act_evidence, outcome, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		op.ID.String(), op.RunID.String(), op.Generation, string(op.Kind), string(op.State),
		intent, actEvidence, outcome, formatTime(op.CreatedAt), formatTime(op.UpdatedAt),
	); err != nil {
		return fmt.Errorf("sqlite: create operation %s: %w", op.ID, err)
	}
	return nil
}

const selectOperationColumns = `SELECT id, run_id, generation, kind, state, intent, act_evidence, outcome, created_at, updated_at FROM operations`

// scanOperation maps one operations row from a row scanner.
func scanOperation(scan func(dest ...any) error) (app.Operation, error) {
	var (
		id, runID, kind, state, intent, createdAt, updatedAt string
		actEvidence, outcome                                 sql.NullString
		generation                                           int64
	)
	if err := scan(&id, &runID, &generation, &kind, &state, &intent, &actEvidence, &outcome, &createdAt, &updatedAt); err != nil {
		return app.Operation{}, err
	}
	operationID, err := identity.ParseOperationID(id)
	if err != nil {
		return app.Operation{}, fmt.Errorf("sqlite: operation id: %w", err)
	}
	parsedRunID, err := identity.ParseRunID(runID)
	if err != nil {
		return app.Operation{}, fmt.Errorf("sqlite: operation %s run id: %w", id, err)
	}
	decodedIntent, err := decodeJournalValue(sql.NullString{String: intent, Valid: true})
	if err != nil {
		return app.Operation{}, err
	}
	decodedAct, err := decodeJournalValue(actEvidence)
	if err != nil {
		return app.Operation{}, err
	}
	decodedOutcome, err := decodeJournalValue(outcome)
	if err != nil {
		return app.Operation{}, err
	}
	created, err := parseTime(createdAt)
	if err != nil {
		return app.Operation{}, err
	}
	updated, err := parseTime(updatedAt)
	if err != nil {
		return app.Operation{}, err
	}
	return app.Operation{
		ID:          operationID,
		RunID:       parsedRunID,
		Generation:  generation,
		Kind:        app.OperationKind(kind),
		State:       app.OperationState(state),
		Intent:      decodedIntent,
		ActEvidence: decodedAct,
		Outcome:     decodedOutcome,
		CreatedAt:   created,
		UpdatedAt:   updated,
	}, nil
}

// getOperation loads one operation row through any querier.
func getOperation(ctx context.Context, q querier, id identity.OperationID) (app.Operation, error) {
	op, err := scanOperation(q.QueryRowContext(ctx, selectOperationColumns+` WHERE id = ?`, id.String()).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return app.Operation{}, fmt.Errorf("sqlite: operation %s: %w", id, app.ErrNotFound)
	}
	if err != nil {
		return app.Operation{}, fmt.Errorf("sqlite: load operation %s: %w", id, err)
	}
	return op, nil
}

func (r operationRepository) Get(ctx context.Context, id identity.OperationID) (app.Operation, error) {
	return getOperation(ctx, r.u.tx, id)
}

// Save updates the operation's mutable columns: its journal state, act
// evidence and outcome. Identity, generation, kind and intent are fixed at
// Create.
func (r operationRepository) Save(ctx context.Context, op app.Operation) error { //nolint:gocritic // hugeParam: the port passes journal rows by value; the repository mirrors its signature.
	owner, err := runOfOperationRow(ctx, r.u.tx, op.ID)
	if err != nil {
		return err
	}
	if scopeErr := r.u.requireLeasedRun(owner, "operation", op.ID.String()); scopeErr != nil {
		return scopeErr
	}
	actEvidence, err := encodeJournalValue(op.ActEvidence)
	if err != nil {
		return err
	}
	outcome, err := encodeJournalValue(op.Outcome)
	if err != nil {
		return err
	}
	result, err := r.u.tx.ExecContext(ctx,
		`UPDATE operations SET state = ?, act_evidence = ?, outcome = ?, updated_at = ? WHERE id = ?`,
		string(op.State), actEvidence, outcome, formatTime(op.UpdatedAt), op.ID.String(),
	)
	if err != nil {
		return fmt.Errorf("sqlite: save operation %s: %w", op.ID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: save operation rows affected: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("sqlite: operation %s: %w", op.ID, app.ErrNotFound)
	}
	return nil
}

// collectOperations drains one operations query into decoded rows.
func collectOperations(rows *sql.Rows) ([]app.Operation, error) {
	defer rows.Close() //nolint:errcheck // the deferred close of a fully-iterated read cursor has no failure the rows.Err check below misses.
	var operations []app.Operation
	for rows.Next() {
		op, err := scanOperation(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan operation row: %w", err)
		}
		operations = append(operations, op)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate operation rows: %w", err)
	}
	return operations, nil
}

// pendingOperations lists a run's open operations (pending or reconciling),
// oldest first, through any querier.
func pendingOperations(ctx context.Context, q querier, runID identity.RunID) ([]app.Operation, error) {
	rows, err := q.QueryContext(ctx,
		selectOperationColumns+` WHERE run_id = ? AND state IN (?, ?) ORDER BY created_at, rowid`,
		runID.String(), string(app.OperationPending), string(app.OperationReconciling),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list open operations of run %s: %w", runID, err)
	}
	return collectOperations(rows)
}

func (r operationRepository) Pending(ctx context.Context, runID identity.RunID) ([]app.Operation, error) {
	return pendingOperations(ctx, r.u.tx, runID)
}

// ByKind lists every operation of one kind for a run, whatever its state,
// newest first.
func (r operationRepository) ByKind(ctx context.Context, runID identity.RunID, kind app.OperationKind) ([]app.Operation, error) {
	return operationsByKind(ctx, r.u.tx, runID, kind)
}

// operationsByKind lists every operation of one kind for a run, whatever
// its state, newest first, through any querier.
func operationsByKind(ctx context.Context, q querier, runID identity.RunID, kind app.OperationKind) ([]app.Operation, error) {
	rows, err := q.QueryContext(ctx,
		selectOperationColumns+` WHERE run_id = ? AND kind = ? ORDER BY created_at DESC, rowid DESC`,
		runID.String(), string(kind),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list %s operations of run %s: %w", kind, runID, err)
	}
	return collectOperations(rows)
}

// transitionRepository records append-only transition evidence inside the
// unit of work.
type transitionRepository struct{ u *unitOfWork }

func (r transitionRepository) Record(ctx context.Context, t app.Transition) error { //nolint:gocritic // hugeParam: the port passes evidence rows by value; the repository mirrors its signature.
	owner, err := r.u.runOfTransitionTarget(ctx, &t)
	if err != nil {
		return err
	}
	if scopeErr := r.u.requireLeasedRun(owner, "transition target", t.EntityID); scopeErr != nil {
		return scopeErr
	}
	return recordTransition(ctx, r.u.tx, &t)
}

// runOfTransitionTarget resolves the owning run of a transition's target
// entity through its persisted ancestry.
func (u *unitOfWork) runOfTransitionTarget(ctx context.Context, t *app.Transition) (identity.RunID, error) {
	switch t.EntityKind {
	case app.EntityRun:
		runID, err := identity.ParseRunID(t.EntityID)
		if err != nil {
			return "", fmt.Errorf("sqlite: transition run id: %w", err)
		}
		return runID, nil
	case app.EntityTask:
		taskID, err := identity.ParseTaskID(t.EntityID)
		if err != nil {
			return "", fmt.Errorf("sqlite: transition task id: %w", err)
		}
		return runOfTask(ctx, u.tx, taskID)
	case app.EntityAttempt:
		attemptID, err := identity.ParseAttemptID(t.EntityID)
		if err != nil {
			return "", fmt.Errorf("sqlite: transition attempt id: %w", err)
		}
		return runOfAttempt(ctx, u.tx, attemptID)
	case app.EntitySession:
		sessionID, err := identity.ParseSessionID(t.EntityID)
		if err != nil {
			return "", fmt.Errorf("sqlite: transition session id: %w", err)
		}
		return runOfSession(ctx, u.tx, sessionID)
	default:
		return "", fmt.Errorf("sqlite: transition entity kind %q has no owning run", t.EntityKind)
	}
}

// recordTransition appends one transition-evidence row through any querier.
func recordTransition(ctx context.Context, q querier, t *app.Transition) error {
	id, err := newUUID()
	if err != nil {
		return err
	}
	var generation any
	if t.Generation != nil {
		generation = *t.Generation
	}
	if _, err := q.ExecContext(ctx,
		`INSERT INTO transitions (id, entity_kind, entity_id, from_state, to_state, reason, generation, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, string(t.EntityKind), t.EntityID, t.From, t.To, t.Reason, generation, formatTime(t.At),
	); err != nil {
		return fmt.Errorf("sqlite: record transition of %s %s: %w", t.EntityKind, t.EntityID, err)
	}
	return nil
}

// checkRequestRepository reads and updates check requests inside the unit
// of work; new requests are inserted only by SubmitResult's acceptance.
type checkRequestRepository struct{ u *unitOfWork }

// scanCheckRequest maps one check_requests row.
func scanCheckRequest(row *sql.Row) (app.CheckRequest, error) {
	var (
		resultID, attemptID, state, createdAt string
		claimedGeneration                     sql.NullInt64
	)
	if err := row.Scan(&resultID, &attemptID, &state, &createdAt, &claimedGeneration); err != nil {
		return app.CheckRequest{}, err
	}
	parsedResultID, err := identity.ParseResultID(resultID)
	if err != nil {
		return app.CheckRequest{}, fmt.Errorf("sqlite: check request result id: %w", err)
	}
	parsedAttemptID, err := identity.ParseAttemptID(attemptID)
	if err != nil {
		return app.CheckRequest{}, fmt.Errorf("sqlite: check request %s attempt id: %w", resultID, err)
	}
	created, err := parseTime(createdAt)
	if err != nil {
		return app.CheckRequest{}, err
	}
	request := app.CheckRequest{
		ResultID:  parsedResultID,
		AttemptID: parsedAttemptID,
		State:     app.CheckRequestState(state),
		CreatedAt: created,
	}
	if claimedGeneration.Valid {
		request.ClaimedGeneration = &claimedGeneration.Int64
	}
	return request, nil
}

const selectCheckRequestColumns = `SELECT result_id, attempt_id, state, created_at, claimed_generation FROM check_requests`

func (r checkRequestRepository) Get(ctx context.Context, resultID identity.ResultID) (app.CheckRequest, error) {
	request, err := scanCheckRequest(r.u.tx.QueryRowContext(ctx, selectCheckRequestColumns+` WHERE result_id = ?`, resultID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return app.CheckRequest{}, fmt.Errorf("sqlite: check request of result %s: %w", resultID, app.ErrNotFound)
	}
	if err != nil {
		return app.CheckRequest{}, fmt.Errorf("sqlite: load check request of result %s: %w", resultID, err)
	}
	return request, nil
}

// Pending returns the run's oldest still-requested check request, if any.
func (r checkRequestRepository) Pending(ctx context.Context, runID identity.RunID) (app.CheckRequest, bool, error) {
	request, err := scanCheckRequest(r.u.tx.QueryRowContext(ctx,
		selectCheckRequestColumns+` WHERE state = ? AND attempt_id IN (
			SELECT a.id FROM attempts a JOIN tasks t ON a.task_id = t.id WHERE t.run_id = ?
		) ORDER BY created_at, rowid LIMIT 1`,
		string(app.CheckRequestRequested), runID.String(),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return app.CheckRequest{}, false, nil
	}
	if err != nil {
		return app.CheckRequest{}, false, fmt.Errorf("sqlite: load pending check request of run %s: %w", runID, err)
	}
	return request, true, nil
}

func (r checkRequestRepository) Save(ctx context.Context, cr app.CheckRequest) error { //nolint:gocritic // hugeParam: the port passes check requests by value; the repository mirrors its signature.
	owner, err := runOfResult(ctx, r.u.tx, cr.ResultID)
	if err != nil {
		return err
	}
	if scopeErr := r.u.requireLeasedRun(owner, "check request", cr.ResultID.String()); scopeErr != nil {
		return scopeErr
	}
	var claimedGeneration any
	if cr.ClaimedGeneration != nil {
		claimedGeneration = *cr.ClaimedGeneration
	}
	result, err := r.u.tx.ExecContext(ctx,
		`UPDATE check_requests SET state = ?, claimed_generation = ? WHERE result_id = ?`,
		string(cr.State), claimedGeneration, cr.ResultID.String(),
	)
	if err != nil {
		return fmt.Errorf("sqlite: save check request of result %s: %w", cr.ResultID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: save check request rows affected: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("sqlite: check request of result %s: %w", cr.ResultID, app.ErrNotFound)
	}
	return nil
}

// checkExecClaimRepository reads check-exec claims inside the unit of work;
// SubmissionStore.ClaimCheckExec remains the only writer.
type checkExecClaimRepository struct{ u *unitOfWork }

func (r checkExecClaimRepository) Get(ctx context.Context, op identity.OperationID) (app.CheckExecClaim, bool, error) {
	var (
		pid       int64
		claimedAt string
	)
	err := r.u.tx.QueryRowContext(ctx,
		`SELECT pid, claimed_at FROM check_exec_claims WHERE operation_id = ?`, op.String(),
	).Scan(&pid, &claimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return app.CheckExecClaim{}, false, nil
	}
	if err != nil {
		return app.CheckExecClaim{}, false, fmt.Errorf("sqlite: load check-exec claim of operation %s: %w", op, err)
	}
	claimed, err := parseTime(claimedAt)
	if err != nil {
		return app.CheckExecClaim{}, false, err
	}
	return app.CheckExecClaim{OperationID: op, PID: int(pid), ClaimedAt: claimed}, true, nil
}
