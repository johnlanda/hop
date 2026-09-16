package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// The Phase 3 capability surface: the same fenced unit of work also
// implements app.WorkflowRepositories, so app.RequireWorkflowRepositories
// succeeds against this adapter (docs/plan/phase-3-design.md section 3's
// additive packaging rule; slice 6 may later fold the getters into
// UnitOfWork proper).
var _ app.WorkflowRepositories = (*unitOfWork)(nil)

func (u *unitOfWork) TaskDependencies() app.TaskDependencyRepository {
	return taskDependencyRepository{u}
}

func (u *unitOfWork) TaskIndex() app.TaskIndexRepository { return taskIndexRepository{u} }

func (u *unitOfWork) AttemptIndex() app.AttemptIndexRepository { return attemptIndexRepository{u} }

func (u *unitOfWork) SessionIndex() app.SessionIndexRepository { return sessionIndexRepository{u} }

func (u *unitOfWork) Messages() app.MessageRepository { return messageRepository{u} }

func (u *unitOfWork) Reviews() app.ReviewRepository { return reviewRepository{u} }

func (u *unitOfWork) Integrations() app.IntegrationRepository { return integrationRepository{u} }

func (u *unitOfWork) RetryRequests() app.RetryRequestRepository { return retryRequestRepository{u} }

// ManagerSession returns the run's current (non-terminal) manager session
// — well defined by the sessions_one_manager_per_run partial unique index
// — or ErrNotFound when none exists.
func (u *unitOfWork) ManagerSession(ctx context.Context, runID identity.RunID) (run.Session, int64, error) {
	session, revision, ok, err := managerSession(ctx, u.tx, runID)
	if err != nil {
		return run.Session{}, 0, err
	}
	if !ok {
		return run.Session{}, 0, fmt.Errorf("sqlite: manager session of run %s: %w", runID, app.ErrNotFound)
	}
	return session, revision, nil
}

// managerSession resolves the run's current manager session through any
// querier; ok is false when none exists.
func managerSession(ctx context.Context, q querier, runID identity.RunID) (session run.Session, revision int64, ok bool, err error) {
	var id string
	err = q.QueryRowContext(ctx,
		`SELECT id FROM sessions WHERE run_id = ? AND role = ? AND state NOT IN `+terminalSessionStates,
		runID.String(), string(run.RoleManager),
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return run.Session{}, 0, false, nil
	}
	if err != nil {
		return run.Session{}, 0, false, fmt.Errorf("sqlite: load manager session of run %s: %w", runID, err)
	}
	sessionID, err := identity.ParseSessionID(id)
	if err != nil {
		return run.Session{}, 0, false, fmt.Errorf("sqlite: manager session id of run %s: %w", runID, err)
	}
	session, revision, err = getSession(ctx, q, sessionID)
	if err != nil {
		return run.Session{}, 0, false, err
	}
	return session, revision, true, nil
}

// taskDependencyRepository reads a run's persisted dependency edges inside
// the unit of work; edges are written only by PlanStore.CreateTask.
type taskDependencyRepository struct{ u *unitOfWork }

func (r taskDependencyRepository) ByRun(ctx context.Context, runID identity.RunID) ([]run.TaskDependency, error) {
	return taskDependenciesByRun(ctx, r.u.tx, runID)
}

// taskDependenciesByRun lists a run's dependency edges through any querier.
func taskDependenciesByRun(ctx context.Context, q querier, runID identity.RunID) ([]run.TaskDependency, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT td.task_id, td.prerequisite_id, td.created_at FROM task_dependencies td
		 JOIN tasks t ON td.task_id = t.id WHERE t.run_id = ? ORDER BY td.rowid`,
		runID.String(),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list dependency edges of run %s: %w", runID, err)
	}
	defer rows.Close() //nolint:errcheck // the deferred close of a fully-iterated read cursor has no failure the rows.Err check below misses.
	var edges []run.TaskDependency
	for rows.Next() {
		var taskID, prerequisiteID, createdAt string
		if err := rows.Scan(&taskID, &prerequisiteID, &createdAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan dependency edge: %w", err)
		}
		parsedTaskID, err := identity.ParseTaskID(taskID)
		if err != nil {
			return nil, fmt.Errorf("sqlite: dependency task id: %w", err)
		}
		parsedPrerequisiteID, err := identity.ParseTaskID(prerequisiteID)
		if err != nil {
			return nil, fmt.Errorf("sqlite: dependency prerequisite id: %w", err)
		}
		created, err := parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		edges = append(edges, run.TaskDependency{TaskID: parsedTaskID, PrerequisiteID: parsedPrerequisiteID, CreatedAt: created})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate dependency edges: %w", err)
	}
	return edges, nil
}

// taskIndexRepository lists a run's tasks in bulk and creates
// controller-authored rows (review tasks) under the lease fence.
type taskIndexRepository struct{ u *unitOfWork }

func (r taskIndexRepository) ByRun(ctx context.Context, runID identity.RunID) ([]run.Task, error) {
	return tasksByRun(ctx, r.u.tx, runID)
}

// tasksByRun lists a run's tasks through any querier, task seq order.
func tasksByRun(ctx context.Context, q querier, runID identity.RunID) ([]run.Task, error) {
	rows, err := q.QueryContext(ctx, selectTaskColumns+` WHERE t.run_id = ? ORDER BY t.seq`, runID.String())
	if err != nil {
		return nil, fmt.Errorf("sqlite: list tasks of run %s: %w", runID, err)
	}
	defer rows.Close() //nolint:errcheck // the deferred close of a fully-iterated read cursor has no failure the rows.Err check below misses.
	var tasks []run.Task
	for rows.Next() {
		task, _, err := scanTask(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan task row: %w", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate task rows: %w", err)
	}
	return tasks, nil
}

// Create inserts a controller-created task row (section 8: only the
// controller creates review tasks, with the frozen subject in the row and
// an empty instructions path — a review task's instructions are its role
// template plus the frozen subject, never a per-task artifact file).
func (r taskIndexRepository) Create(ctx context.Context, t run.Task) (int64, error) { //nolint:gocritic // hugeParam: the port passes domain values by value; the repository mirrors its signature.
	if err := r.u.requireLeasedRun(t.RunID, "task", t.ID.String()); err != nil {
		return 0, err
	}
	var mailboxClosedAt any
	if t.MailboxClosed {
		mailboxClosedAt = formatTime(t.UpdatedAt)
	}
	if _, err := r.u.tx.ExecContext(ctx,
		`INSERT INTO tasks (id, run_id, kind, seq, title, instructions_path, instructions_digest, retry_count, subject_commit_oid, subject_tree_oid, mailbox_closed_at, state, revision, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, '', ?, 0, ?, ?, ?, ?, 1, ?, ?)`,
		t.ID.String(), t.RunID.String(), string(t.Kind), t.Seq, t.Title, t.InstructionsDigest,
		nullString(t.SubjectCommitOID), nullString(t.SubjectTreeOID), mailboxClosedAt,
		string(t.State), formatTime(t.UpdatedAt), formatTime(t.UpdatedAt),
	); err != nil {
		return 0, fmt.Errorf("sqlite: create task %s: %w", t.ID, err)
	}
	return 1, nil
}

// attemptIndexRepository lists a task's attempts in bulk and reserves new
// ones under the lease fence (a task's FIRST attempt at assignment time; a
// retry's successor is reserved by PlanStore.RequestRetry instead).
type attemptIndexRepository struct{ u *unitOfWork }

func (r attemptIndexRepository) ByTask(ctx context.Context, task identity.TaskID) ([]run.Attempt, error) {
	return attemptsByTask(ctx, r.u.tx, task)
}

// attemptsByTask lists a task's attempts in reservation order (ascending
// number) through any querier.
func attemptsByTask(ctx context.Context, q querier, taskID identity.TaskID) ([]run.Attempt, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, number, state, updated_at FROM attempts WHERE task_id = ? ORDER BY number`,
		taskID.String(),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list attempts of task %s: %w", taskID, err)
	}
	defer rows.Close() //nolint:errcheck // the deferred close of a fully-iterated read cursor has no failure the rows.Err check below misses.
	var attempts []run.Attempt
	for rows.Next() {
		var (
			id, state, updatedAt string
			number               int64
		)
		if err := rows.Scan(&id, &number, &state, &updatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan attempt row: %w", err)
		}
		attemptID, err := identity.ParseAttemptID(id)
		if err != nil {
			return nil, fmt.Errorf("sqlite: attempt id: %w", err)
		}
		updated, err := parseTime(updatedAt)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, run.Attempt{ID: attemptID, TaskID: taskID, Number: int(number), State: run.AttemptState(state), UpdatedAt: updated})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate attempt rows: %w", err)
	}
	return attempts, nil
}

func (r attemptIndexRepository) Create(ctx context.Context, a run.Attempt) (int64, error) { //nolint:gocritic // hugeParam: the port passes domain values by value; the repository mirrors its signature.
	owner, err := runOfTask(ctx, r.u.tx, a.TaskID)
	if err != nil {
		return 0, err
	}
	if scopeErr := r.u.requireLeasedRun(owner, "attempt's task", a.TaskID.String()); scopeErr != nil {
		return 0, scopeErr
	}
	if _, err := r.u.tx.ExecContext(ctx,
		`INSERT INTO attempts (id, task_id, number, state, revision, updated_at) VALUES (?, ?, ?, ?, 1, ?)`,
		a.ID.String(), a.TaskID.String(), a.Number, string(a.State), formatTime(a.UpdatedAt),
	); err != nil {
		return 0, fmt.Errorf("sqlite: create attempt %s: %w", a.ID, err)
	}
	return 1, nil
}

// sessionIndexRepository lists a run's sessions in bulk.
type sessionIndexRepository struct{ u *unitOfWork }

func (r sessionIndexRepository) ByRun(ctx context.Context, runID identity.RunID) ([]run.Session, error) {
	rows, err := r.u.tx.QueryContext(ctx,
		`SELECT id FROM sessions WHERE run_id = ? ORDER BY rowid`, runID.String(),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list sessions of run %s: %w", runID, err)
	}
	defer rows.Close() //nolint:errcheck // the deferred close of a fully-iterated read cursor has no failure the rows.Err check below misses.
	var ids []identity.SessionID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("sqlite: scan session row: %w", err)
		}
		id, err := identity.ParseSessionID(raw)
		if err != nil {
			return nil, fmt.Errorf("sqlite: listed session id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate session rows: %w", err)
	}
	sessions := make([]run.Session, 0, len(ids))
	for _, id := range ids {
		session, _, err := getSession(ctx, r.u.tx, id)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, nil
}

// messageRepository is the controller-side view of the message journal:
// envelopes only — deliveries and acks are MessagingStore's own concern.
type messageRepository struct{ u *unitOfWork }

// Create inserts one immutable controller-transaction envelope (a
// controller info notice, committed inside the same transaction as the
// transition it reports), the store assigning EnqueueSeq as the next value
// for (RunID, Recipient).
func (r messageRepository) Create(ctx context.Context, m run.Message) (run.Message, error) { //nolint:gocritic // hugeParam: the port passes domain values by value; the repository mirrors its signature.
	if err := r.u.requireLeasedRun(m.RunID, "message", m.ID.String()); err != nil {
		return run.Message{}, err
	}
	seq, err := nextEnqueueSeq(ctx, r.u.tx, m.RunID, m.Recipient)
	if err != nil {
		return run.Message{}, err
	}
	m.EnqueueSeq = seq
	if err := insertMessage(ctx, r.u.tx, &m); err != nil {
		return run.Message{}, err
	}
	return m, nil
}

func (r messageRepository) Get(ctx context.Context, id identity.MessageID) (run.Message, error) {
	return getMessage(ctx, r.u.tx, id)
}

func (r messageRepository) ByAddress(ctx context.Context, runID identity.RunID, address run.Address) ([]run.Message, error) {
	return messagesByAddress(ctx, r.u.tx, runID, address)
}

// PendingByAddress returns the queued and delivered-unacknowledged message
// IDs for one recipient address, sorted lexically: the mailbox-closure
// snapshot-equality set, read INSIDE the caller's transaction so a
// settlement's obligation snapshot and its final equality check never
// escape the unit of work.
func (r messageRepository) PendingByAddress(ctx context.Context, runID identity.RunID, address run.Address) ([]identity.MessageID, error) {
	rows, err := r.u.tx.QueryContext(ctx,
		`SELECT m.id FROM messages m
		 WHERE m.run_id = ? AND m.recipient_address = ?
		   AND NOT EXISTS (SELECT 1 FROM message_acks a WHERE a.message_id = m.id)
		 ORDER BY m.id`,
		runID.String(), app.AddressString(address),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list pending messages of %s at %s: %w", runID, app.AddressString(address), err)
	}
	defer rows.Close() //nolint:errcheck // the deferred close of a fully-iterated read cursor has no failure the rows.Err check below misses.
	var ids []identity.MessageID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("sqlite: scan pending message id: %w", err)
		}
		id, err := identity.ParseMessageID(raw)
		if err != nil {
			return nil, fmt.Errorf("sqlite: pending message id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate pending message ids: %w", err)
	}
	return ids, nil
}

// reviewRepository is the controller-side read of accepted verdicts;
// writes happen only through ReviewStore.SubmitReview.
type reviewRepository struct{ u *unitOfWork }

const selectReviewColumns = `SELECT id, run_id, task_id, attempt_id, subject_commit_oid, subject_tree_oid, verdict, reasons_digest, submitted_at FROM reviews`

// scanReview maps one reviews row.
func scanReview(scan func(dest ...any) error) (run.Review, error) {
	var id, runID, taskID, attemptID, subjectCommit, subjectTree, verdict, reasonsDigest, submittedAt string
	if err := scan(&id, &runID, &taskID, &attemptID, &subjectCommit, &subjectTree, &verdict, &reasonsDigest, &submittedAt); err != nil {
		return run.Review{}, err
	}
	reviewID, err := identity.ParseReviewID(id)
	if err != nil {
		return run.Review{}, fmt.Errorf("sqlite: review id: %w", err)
	}
	parsedRunID, err := identity.ParseRunID(runID)
	if err != nil {
		return run.Review{}, fmt.Errorf("sqlite: review %s run id: %w", id, err)
	}
	parsedTaskID, err := identity.ParseTaskID(taskID)
	if err != nil {
		return run.Review{}, fmt.Errorf("sqlite: review %s task id: %w", id, err)
	}
	parsedAttemptID, err := identity.ParseAttemptID(attemptID)
	if err != nil {
		return run.Review{}, fmt.Errorf("sqlite: review %s attempt id: %w", id, err)
	}
	submitted, err := parseTime(submittedAt)
	if err != nil {
		return run.Review{}, err
	}
	return run.Review{
		ID: reviewID, RunID: parsedRunID, TaskID: parsedTaskID, AttemptID: parsedAttemptID,
		SubjectCommitOID: subjectCommit, SubjectTreeOID: subjectTree,
		Verdict: run.Verdict(verdict), ReasonsDigest: reasonsDigest, SubmittedAt: submitted,
	}, nil
}

// Latest returns the run's most recently accepted review, or nil.
func (r reviewRepository) Latest(ctx context.Context, runID identity.RunID) (*run.Review, error) {
	return latestReview(ctx, r.u.tx, runID)
}

// latestReview resolves the run's newest accepted review through any
// querier, or nil when none exists.
func latestReview(ctx context.Context, q querier, runID identity.RunID) (*run.Review, error) {
	review, err := scanReview(q.QueryRowContext(ctx,
		selectReviewColumns+` WHERE run_id = ? ORDER BY submitted_at DESC, rowid DESC LIMIT 1`, runID.String(),
	).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // a nil review with a nil error is the documented "no accepted review yet" value.
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: load latest review of run %s: %w", runID, err)
	}
	return &review, nil
}

// ByAttempt returns the accepted review for one attempt, or nil.
func (r reviewRepository) ByAttempt(ctx context.Context, attempt identity.AttemptID) (*run.Review, error) {
	return reviewByAttempt(ctx, r.u.tx, attempt)
}

// reviewByAttempt resolves one attempt's accepted review through any
// querier, or nil when none exists.
func reviewByAttempt(ctx context.Context, q querier, attempt identity.AttemptID) (*run.Review, error) {
	review, err := scanReview(q.QueryRowContext(ctx,
		selectReviewColumns+` WHERE attempt_id = ?`, attempt.String(),
	).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // a nil review with a nil error is the documented "no accepted review yet" value.
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: load review of attempt %s: %w", attempt, err)
	}
	return &review, nil
}

// integrationRepository loads and saves Integration rows: the controller's
// serial merge journal, its one-non-terminal-per-run invariant enforced by
// the integrations_one_current_per_run partial unique index.
type integrationRepository struct{ u *unitOfWork }

const selectIntegrationColumns = `SELECT id, run_id, task_id, result_id, source_commit_oid, premerge_head_oid, merge_commit_oid, state, revision, created_at, updated_at FROM integrations`

// scanIntegration maps one integrations row and its revision.
func scanIntegration(scan func(dest ...any) error) (run.Integration, int64, error) {
	var (
		id, runID, taskID, resultID, sourceCommit, premergeHead, state, createdAt, updatedAt string
		mergeCommit                                                                          sql.NullString
		revision                                                                             int64
	)
	if err := scan(&id, &runID, &taskID, &resultID, &sourceCommit, &premergeHead, &mergeCommit, &state, &revision, &createdAt, &updatedAt); err != nil {
		return run.Integration{}, 0, err
	}
	integrationID, err := identity.ParseIntegrationID(id)
	if err != nil {
		return run.Integration{}, 0, fmt.Errorf("sqlite: integration id: %w", err)
	}
	parsedRunID, err := identity.ParseRunID(runID)
	if err != nil {
		return run.Integration{}, 0, fmt.Errorf("sqlite: integration %s run id: %w", id, err)
	}
	parsedTaskID, err := identity.ParseTaskID(taskID)
	if err != nil {
		return run.Integration{}, 0, fmt.Errorf("sqlite: integration %s task id: %w", id, err)
	}
	parsedResultID, err := identity.ParseResultID(resultID)
	if err != nil {
		return run.Integration{}, 0, fmt.Errorf("sqlite: integration %s result id: %w", id, err)
	}
	created, err := parseTime(createdAt)
	if err != nil {
		return run.Integration{}, 0, err
	}
	updated, err := parseTime(updatedAt)
	if err != nil {
		return run.Integration{}, 0, err
	}
	return run.Integration{
		ID: integrationID, RunID: parsedRunID, TaskID: parsedTaskID, ResultID: parsedResultID,
		SourceCommitOID: sourceCommit, PremergeHeadOID: premergeHead, MergeCommitOID: mergeCommit.String,
		State: run.IntegrationState(state), CreatedAt: created, UpdatedAt: updated,
	}, revision, nil
}

func (r integrationRepository) Get(ctx context.Context, id identity.IntegrationID) (run.Integration, int64, error) {
	integration, revision, err := scanIntegration(r.u.tx.QueryRowContext(ctx,
		selectIntegrationColumns+` WHERE id = ?`, id.String(),
	).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return run.Integration{}, 0, fmt.Errorf("sqlite: integration %s: %w", id, app.ErrNotFound)
	}
	if err != nil {
		return run.Integration{}, 0, fmt.Errorf("sqlite: load integration %s: %w", id, err)
	}
	return integration, revision, nil
}

func (r integrationRepository) Create(ctx context.Context, i run.Integration) (int64, error) { //nolint:gocritic // hugeParam: the port passes domain values by value; the repository mirrors its signature.
	if err := r.u.requireLeasedRun(i.RunID, "integration", i.ID.String()); err != nil {
		return 0, err
	}
	if _, err := r.u.tx.ExecContext(ctx,
		`INSERT INTO integrations (id, run_id, task_id, result_id, source_commit_oid, premerge_head_oid, merge_commit_oid, state, operation_id, revision, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, 1, ?, ?)`,
		i.ID.String(), i.RunID.String(), i.TaskID.String(), i.ResultID.String(),
		i.SourceCommitOID, i.PremergeHeadOID, nullString(i.MergeCommitOID), string(i.State),
		formatTime(i.CreatedAt), formatTime(i.UpdatedAt),
	); err != nil {
		return 0, fmt.Errorf("sqlite: create integration %s: %w", i.ID, err)
	}
	return 1, nil
}

func (r integrationRepository) Save(ctx context.Context, i run.Integration, expectedRevision int64) (int64, error) { //nolint:gocritic // hugeParam: the port passes domain values by value; the repository mirrors its signature.
	if err := r.u.requireLeasedRun(i.RunID, "integration", i.ID.String()); err != nil {
		return 0, err
	}
	result, err := r.u.tx.ExecContext(ctx,
		`UPDATE integrations SET state = ?, merge_commit_oid = ?, updated_at = ?, revision = revision + 1 WHERE id = ? AND revision = ?`,
		string(i.State), nullString(i.MergeCommitOID), formatTime(i.UpdatedAt), i.ID.String(), expectedRevision,
	)
	return saveEntity(result, err, "integration", i.ID.String(), expectedRevision)
}

// Current returns the run's non-terminal integration, if any: the
// store-enforced serial slot.
func (r integrationRepository) Current(ctx context.Context, runID identity.RunID) (integration run.Integration, revision int64, ok bool, err error) {
	integration, revision, err = scanIntegration(r.u.tx.QueryRowContext(ctx,
		selectIntegrationColumns+` WHERE run_id = ? AND state IN ('merging', 'checking', 'check-failed')`,
		runID.String(),
	).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return run.Integration{}, 0, false, nil
	}
	if err != nil {
		return run.Integration{}, 0, false, fmt.Errorf("sqlite: load current integration of run %s: %w", runID, err)
	}
	return integration, revision, true, nil
}

// ByTask returns every integration recorded for task, newest first.
func (r integrationRepository) ByTask(ctx context.Context, task identity.TaskID) ([]run.Integration, error) {
	rows, err := r.u.tx.QueryContext(ctx,
		selectIntegrationColumns+` WHERE task_id = ? ORDER BY created_at DESC, rowid DESC`, task.String(),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list integrations of task %s: %w", task, err)
	}
	defer rows.Close() //nolint:errcheck // the deferred close of a fully-iterated read cursor has no failure the rows.Err check below misses.
	var integrations []run.Integration
	for rows.Next() {
		integration, _, err := scanIntegration(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan integration row: %w", err)
		}
		integrations = append(integrations, integration)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate integration rows: %w", err)
	}
	return integrations, nil
}

// retryRequestRepository reads and consumes the manager's pending retry
// bookkeeping rows; PlanStore.RequestRetry is their only writer.
type retryRequestRepository struct{ u *unitOfWork }

// Pending returns the run's pending retry requests, oldest first.
func (r retryRequestRepository) Pending(ctx context.Context, runID identity.RunID) ([]app.RetryRequestRecord, error) {
	rows, err := r.u.tx.QueryContext(ctx,
		`SELECT rr.task_id, rr.requested_by_session, rr.reason, rr.request_id, rr.created_at
		 FROM retry_requests rr JOIN tasks t ON rr.task_id = t.id
		 WHERE t.run_id = ? AND rr.state = ? ORDER BY rr.created_at, rr.rowid`,
		runID.String(), string(app.RetryRequestPending),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list pending retry requests of run %s: %w", runID, err)
	}
	defer rows.Close() //nolint:errcheck // the deferred close of a fully-iterated read cursor has no failure the rows.Err check below misses.
	var records []app.RetryRequestRecord
	for rows.Next() {
		var (
			taskID, requestedBy, reason, createdAt string
			requestID                              sql.NullString
		)
		if err := rows.Scan(&taskID, &requestedBy, &reason, &requestID, &createdAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan retry request row: %w", err)
		}
		parsedTaskID, err := identity.ParseTaskID(taskID)
		if err != nil {
			return nil, fmt.Errorf("sqlite: retry request task id: %w", err)
		}
		parsedSessionID, err := identity.ParseSessionID(requestedBy)
		if err != nil {
			return nil, fmt.Errorf("sqlite: retry request session id: %w", err)
		}
		var created time.Time
		if created, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		records = append(records, app.RetryRequestRecord{
			TaskID: parsedTaskID, RequestedBy: parsedSessionID, Reason: reason,
			RequestID: requestID.String, CreatedAt: created,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate retry request rows: %w", err)
	}
	return records, nil
}

// MarkConsumed marks task's pending request consumed. The launched attempt
// number is already recorded on the acceptance receipt
// (workflow_receipts.created_entity_seq, written by RequestRetry), which
// is what an identical request-ID retry reads back after consumption; the
// argument re-states it so the caller's transaction says what it launched.
func (r retryRequestRepository) MarkConsumed(ctx context.Context, task identity.TaskID, attemptNumber int) error {
	owner, err := runOfTask(ctx, r.u.tx, task)
	if err != nil {
		return err
	}
	if scopeErr := r.u.requireLeasedRun(owner, "retry request's task", task.String()); scopeErr != nil {
		return scopeErr
	}
	result, err := r.u.tx.ExecContext(ctx,
		`UPDATE retry_requests SET state = ? WHERE task_id = ? AND state = ?`,
		string(app.RetryRequestConsumed), task.String(), string(app.RetryRequestPending),
	)
	if err != nil {
		return fmt.Errorf("sqlite: consume retry request of task %s (attempt %d): %w", task, attemptNumber, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: consume retry request rows affected: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("sqlite: pending retry request of task %s: %w", task, app.ErrNotFound)
	}
	return nil
}
