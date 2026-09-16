package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// Interface conformance for the section 8 worker-authority plan port.
var _ app.PlanStore = (*Store)(nil)

// Workflow-receipt verbs: the op column of workflow_receipts and the verb
// half of the (run, verb, request ID) acceptance key, matching
// internal/app's fakeStore digest recipes exactly.
const (
	taskCreateVerb = "task-create"
	taskRetryVerb  = "task-retry"
	planCloseVerb  = "plan-close"
)

// workflowReceipt is one workflow_receipts row.
type workflowReceipt struct {
	runID, op                        string
	sessionID, incarnationID, taskID string
	requestID, requestDigest         string
	outcome, createdEntityID, detail string
	createdEntitySeq                 *int
}

// insertWorkflowReceipt records one plan-verb receipt at time at.
func insertWorkflowReceipt(ctx context.Context, q querier, r *workflowReceipt, at time.Time) error {
	id, err := newUUID()
	if err != nil {
		return err
	}
	var seq any
	if r.createdEntitySeq != nil {
		seq = *r.createdEntitySeq
	}
	if _, err := q.ExecContext(ctx,
		`INSERT INTO workflow_receipts (id, run_id, op, request_id, request_digest, claimed_session_id, claimed_incarnation_id, claimed_task_id, outcome, created_entity_id, created_entity_seq, detail, at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, truncateClaim(r.runID), r.op, nullString(r.requestID), nullString(r.requestDigest),
		nullString(truncateClaim(r.sessionID)), nullString(truncateClaim(r.incarnationID)), nullString(truncateClaim(r.taskID)),
		r.outcome, nullString(r.createdEntityID), seq, truncateClaim(r.detail), formatTime(at),
	); err != nil {
		return fmt.Errorf("sqlite: record workflow receipt: %w", err)
	}
	return nil
}

// acceptedWorkflowReceipt reads the ONE authoritative acceptance for the
// (run, verb, request ID) key, if any.
func acceptedWorkflowReceipt(ctx context.Context, q querier, runID, op, requestID string) (digest, createdEntityID string, createdEntitySeq int, ok bool, err error) {
	var (
		storedDigest, createdEntity sql.NullString
		storedSeq                   sql.NullInt64
	)
	err = q.QueryRowContext(ctx,
		`SELECT request_digest, created_entity_id, created_entity_seq FROM workflow_receipts WHERE run_id = ? AND op = ? AND request_id = ? AND outcome = 'accepted'`,
		runID, op, requestID,
	).Scan(&storedDigest, &createdEntity, &storedSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", 0, false, nil
	}
	if err != nil {
		return "", "", 0, false, fmt.Errorf("sqlite: read accepted %s receipt: %w", op, err)
	}
	return storedDigest.String, createdEntity.String, int(storedSeq.Int64), true, nil
}

// requireManagerCaller resolves the manager-verb caller checks shared by
// every PlanStore method: the caller session must be the run's manager and
// its claimed incarnation must be the session's current, non-superseded
// binding. It returns the refusal's grammar reason token and detail ("",
// "" when the caller is legitimate) — set directly at each decision point,
// never derived from the detail text downstream.
func requireManagerCaller(ctx context.Context, q querier, runID identity.RunID, sessionID identity.SessionID, incarnationID identity.IncarnationID) (reason, detail string, err error) {
	session, _, err := getSession(ctx, q, sessionID)
	if errors.Is(err, app.ErrNotFound) || (err == nil && (session.Role != run.RoleManager || session.RunID != runID)) {
		return app.GrammarReasonNotManager, "caller is not the run's manager", nil
	}
	if err != nil {
		return "", "", err
	}
	binding, hasBinding, err := currentBinding(ctx, q, sessionID)
	if err != nil {
		return "", "", err
	}
	if !hasBinding || binding.IncarnationID != incarnationID || binding.Superseded {
		return app.GrammarReasonStale, "incarnation is not current", nil
	}
	return "", "", nil
}

// CreateTask validates title/instructions bounds and the dependency edges
// against the persisted graph (acyclic, same run — every edge points at an
// already-existing task, so the persisted graph stays a DAG by
// construction), writes the task with its edges and instructions-artifact
// reference atomically, and clears the run's plan flag (ReopenPlan) in the
// same commit. Section 8's validation order mirrors internal/app's
// fakeStore: the request-ID receipt first, then the manager caller, the
// current incarnation, the run's manager-verb acceptance, bounds, and the
// dependency set.
func (s *Store) CreateTask(ctx context.Context, req app.TaskCreate) (app.TaskCreated, error) { //nolint:gocritic // hugeParam: the port passes the request value; the adapter mirrors its signature.
	var outcome app.TaskCreated
	err := s.inWriteTx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		depIDs := make([]string, len(req.DependsOn))
		for i, dep := range req.DependsOn {
			depIDs[i] = dep.String()
		}
		sort.Strings(depIDs)
		payload := append([]string{req.Title, req.InstructionsDigest}, depIDs...)
		digest := app.ComputeRequestDigest(taskCreateVerb, req.RunID.String(), app.AddressString(run.ManagerAddress()), payload...)
		record := func(kind app.WorkflowOutcomeKind, taskID identity.TaskID, seq int, reason, detail string) error {
			outcome = app.TaskCreated{Outcome: kind, TaskID: taskID, Seq: seq, Reason: reason, Detail: detail}
			receipt := &workflowReceipt{
				runID: req.RunID.String(), op: taskCreateVerb,
				sessionID: req.Session.String(), incarnationID: req.IncarnationID.String(),
				taskID:    req.ID.String(),
				requestID: req.RequestID, requestDigest: digest,
				outcome: string(kind), detail: detail,
			}
			if kind == app.WorkflowAccepted || kind == app.WorkflowDuplicate {
				receipt.createdEntityID = taskID.String()
				receipt.createdEntitySeq = &seq
			}
			return insertWorkflowReceipt(ctx, tx, receipt, now)
		}

		if req.RequestID != "" {
			priorDigest, createdEntity, createdSeq, ok, err := acceptedWorkflowReceipt(ctx, tx, req.RunID.String(), taskCreateVerb, req.RequestID)
			if err != nil {
				return err
			}
			if ok {
				if priorDigest == digest {
					created, parseErr := identity.ParseTaskID(createdEntity)
					if parseErr != nil {
						return fmt.Errorf("sqlite: accepted task-create receipt entity id: %w", parseErr)
					}
					return record(app.WorkflowDuplicate, created, createdSeq, "", "")
				}
				return record(app.WorkflowRefused, "", 0, app.GrammarReasonConflicting, "request id reused with different content")
			}
		}

		reason, refusal, err := requireManagerCaller(ctx, tx, req.RunID, req.Session, req.IncarnationID)
		if err != nil {
			return err
		}
		if refusal != "" {
			return record(app.WorkflowRefused, "", 0, reason, refusal)
		}
		runV, runRevision, err := getRun(ctx, tx, req.RunID)
		if errors.Is(err, app.ErrNotFound) {
			return record(app.WorkflowMalformed, "", 0, app.GrammarReasonMalformed, "unknown run")
		}
		if err != nil {
			return err
		}
		if acceptErr := runV.CanAcceptManagerVerb(); acceptErr != nil {
			acceptReason := app.GrammarReasonUnauthorized
			if errors.Is(acceptErr, run.ErrRunNotAccepting) {
				acceptReason = app.GrammarReasonRunNotAccepting
			}
			return record(app.WorkflowRefused, "", 0, acceptReason, acceptErr.Error())
		}
		if req.Title == "" || len(req.Title) > app.TaskTitleLimit || req.InstructionsDigest == "" {
			return record(app.WorkflowMalformed, "", 0, app.GrammarReasonMalformed, "title is empty or exceeds the size bound, or the instructions digest is empty")
		}

		existingEdges, edgesErr := taskDependenciesByRun(ctx, tx, req.RunID)
		if edgesErr != nil {
			return edgesErr
		}
		edges := make([]run.TaskDependency, 0, len(req.DependsOn))
		for _, dep := range req.DependsOn {
			depTask, _, depErr := getTask(ctx, tx, dep)
			if errors.Is(depErr, app.ErrNotFound) || (depErr == nil && depTask.RunID != req.RunID) {
				return record(app.WorkflowRefused, "", 0, app.GrammarReasonDependencyCycle, "dependency is not in this run")
			}
			if depErr != nil {
				return depErr
			}
			edge, edgeErr := run.NewTaskDependency(req.ID, dep, now)
			if edgeErr != nil {
				return record(app.WorkflowRefused, "", 0, app.GrammarReasonDependencyCycle, edgeErr.Error())
			}
			if cycleErr := run.ValidateAcyclic(existingEdges, edge); cycleErr != nil {
				return record(app.WorkflowRefused, "", 0, app.GrammarReasonDependencyCycle, cycleErr.Error())
			}
			edges = append(edges, edge)
		}

		var seq int64
		if seqErr := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(seq), 0) + 1 FROM tasks WHERE run_id = ?`, req.RunID.String(),
		).Scan(&seq); seqErr != nil {
			return fmt.Errorf("sqlite: next task sequence of run %s: %w", req.RunID, seqErr)
		}
		task := run.NewImplementTask(req.ID, req.RunID, int(seq), req.Title, req.InstructionsDigest, len(req.DependsOn) > 0, now)
		if _, insertErr := tx.ExecContext(ctx,
			`INSERT INTO tasks (id, run_id, kind, seq, title, instructions_path, instructions_digest, retry_count, subject_commit_oid, subject_tree_oid, mailbox_closed_at, state, revision, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, 0, NULL, NULL, NULL, ?, 1, ?, ?)`,
			task.ID.String(), task.RunID.String(), string(task.Kind), task.Seq, task.Title,
			req.InstructionsPath, task.InstructionsDigest, string(task.State), formatTime(now), formatTime(now),
		); insertErr != nil {
			return fmt.Errorf("sqlite: insert manager task: %w", insertErr)
		}
		for i := range edges {
			if _, insertErr := tx.ExecContext(ctx,
				`INSERT INTO task_dependencies (task_id, prerequisite_id, created_at) VALUES (?, ?, ?)`,
				edges[i].TaskID.String(), edges[i].PrerequisiteID.String(), formatTime(edges[i].CreatedAt),
			); insertErr != nil {
				return fmt.Errorf("sqlite: insert dependency edge: %w", insertErr)
			}
		}
		// An accepted CreateTask reopens planning even after a prior close,
		// in the same commit.
		reopened := runV.ReopenPlan(now)
		result, err := tx.ExecContext(ctx,
			`UPDATE runs SET plan_closed_at = NULL, updated_at = ?, revision = revision + 1 WHERE id = ? AND revision = ?`,
			formatTime(reopened.UpdatedAt), req.RunID.String(), runRevision,
		)
		if err != nil {
			return fmt.Errorf("sqlite: reopen plan: %w", err)
		}
		if err := requireCAS(result, fmt.Errorf("sqlite: run %s moved during task creation: %w", req.RunID, app.ErrRevisionConflict)); err != nil {
			return err
		}
		return record(app.WorkflowAccepted, task.ID, task.Seq, "", "")
	})
	if err != nil {
		return app.TaskCreated{}, err
	}
	return outcome, nil
}

// RequestRetry reserves attempt n+1 for a terminal attempt of a
// needs-rework task below the frozen retry limit, IMMEDIATELY inside this
// worker-authority transaction (the CLI grammar names the attempt number
// in the same response), reopens the task and its mailbox, and records the
// pending retry_requests bookkeeping row the controller's assignment pass
// later consumes.
func (s *Store) RequestRetry(ctx context.Context, req app.RetryRequest) (app.RetryAccepted, error) { //nolint:gocritic // hugeParam: the port passes the request value; the adapter mirrors its signature.
	var outcome app.RetryAccepted
	err := s.inWriteTx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		digest := app.ComputeRequestDigest(taskRetryVerb, req.RunID.String(), app.AddressString(run.ManagerAddress()), req.TaskID.String(), req.Reason)
		record := func(kind app.WorkflowOutcomeKind, attemptID string, attemptNumber int, reason, detail string) error {
			outcome = app.RetryAccepted{Outcome: kind, AttemptNumber: attemptNumber, Reason: reason, Detail: detail}
			receipt := &workflowReceipt{
				runID: req.RunID.String(), op: taskRetryVerb,
				sessionID: req.Session.String(), incarnationID: req.IncarnationID.String(),
				taskID:    req.TaskID.String(),
				requestID: req.RequestID, requestDigest: digest,
				outcome: string(kind), detail: detail,
			}
			if kind == app.WorkflowAccepted || kind == app.WorkflowDuplicate {
				receipt.createdEntityID = attemptID
				receipt.createdEntitySeq = &attemptNumber
			}
			return insertWorkflowReceipt(ctx, tx, receipt, now)
		}

		if req.RequestID != "" {
			priorDigest, createdEntity, createdSeq, ok, err := acceptedWorkflowReceipt(ctx, tx, req.RunID.String(), taskRetryVerb, req.RequestID)
			if err != nil {
				return err
			}
			if ok {
				if priorDigest == digest {
					// The original acceptance survives consumption, relaunch
					// and manager succession: an identical retry reads the
					// attempt number back from this receipt, never a second
					// pending row.
					return record(app.WorkflowDuplicate, createdEntity, createdSeq, "", "")
				}
				return record(app.WorkflowRefused, "", 0, app.GrammarReasonConflicting, "request id reused with different content")
			}
		}

		reason, refusal, err := requireManagerCaller(ctx, tx, req.RunID, req.Session, req.IncarnationID)
		if err != nil {
			return err
		}
		if refusal != "" {
			return record(app.WorkflowRefused, "", 0, reason, refusal)
		}
		runV, _, err := getRun(ctx, tx, req.RunID)
		if errors.Is(err, app.ErrNotFound) {
			return record(app.WorkflowMalformed, "", 0, app.GrammarReasonMalformed, "unknown run")
		}
		if err != nil {
			return err
		}
		if acceptErr := runV.CanAcceptManagerVerb(); acceptErr != nil {
			acceptReason := app.GrammarReasonUnauthorized
			if errors.Is(acceptErr, run.ErrRunNotAccepting) {
				acceptReason = app.GrammarReasonRunNotAccepting
			}
			return record(app.WorkflowRefused, "", 0, acceptReason, acceptErr.Error())
		}
		task, taskRevision, err := getTask(ctx, tx, req.TaskID)
		if errors.Is(err, app.ErrNotFound) || (err == nil && task.RunID != req.RunID) {
			return record(app.WorkflowMalformed, "", 0, app.GrammarReasonMalformed, "unknown task")
		}
		if err != nil {
			return err
		}
		if task.State != run.TaskNeedsRework {
			return record(app.WorkflowRefused, "", 0, app.GrammarReasonRetryNotTerminal, "task is not needs-rework")
		}
		var pendingExists bool
		if scanErr := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM retry_requests WHERE task_id = ? AND state = ?)`,
			req.TaskID.String(), string(app.RetryRequestPending),
		).Scan(&pendingExists); scanErr != nil {
			return fmt.Errorf("sqlite: read pending retry request of task %s: %w", req.TaskID, scanErr)
		}
		if pendingExists {
			return record(app.WorkflowRefused, "", 0, app.GrammarReasonConflicting, "a retry request is already pending for this task")
		}

		attempts, attemptsErr := attemptsByTask(ctx, tx, req.TaskID)
		if attemptsErr != nil {
			return attemptsErr
		}
		if len(attempts) == 0 {
			return record(app.WorkflowMalformed, "", 0, app.GrammarReasonMalformed, "task has no attempts")
		}
		prior := attempts[len(attempts)-1]
		limit := 3
		snapshot, snapshotErr := loadSnapshot(ctx, tx, req.RunID)
		if snapshotErr != nil {
			return snapshotErr
		}
		if snapshot.Workflow.RetryLimit > 0 {
			limit = snapshot.Workflow.RetryLimit
		}
		attemptID, err := newUUID()
		if err != nil {
			return err
		}
		parsedAttemptID, err := identity.ParseAttemptID(attemptID)
		if err != nil {
			return fmt.Errorf("sqlite: minted attempt id: %w", err)
		}
		next, err := run.NewRetryAttempt(parsedAttemptID, prior, limit, now)
		switch {
		case err == nil:
		case errors.Is(err, run.ErrRetryLimit):
			return record(app.WorkflowRefused, "", 0, app.GrammarReasonRetryLimit, "retry limit reached")
		case errors.Is(err, run.ErrRetryNotTerminal):
			return record(app.WorkflowRefused, "", 0, app.GrammarReasonRetryNotTerminal, "prior attempt is not terminal")
		default:
			return fmt.Errorf("sqlite: reserve retry attempt: %w", err)
		}
		if _, insertErr := tx.ExecContext(ctx,
			`INSERT INTO attempts (id, task_id, number, state, revision, updated_at) VALUES (?, ?, ?, ?, 1, ?)`,
			next.ID.String(), next.TaskID.String(), next.Number, string(next.State), formatTime(now),
		); insertErr != nil {
			return fmt.Errorf("sqlite: insert retry attempt: %w", insertErr)
		}

		reopened, reopenErr := task.Reopen(now)
		if reopenErr != nil {
			return fmt.Errorf("sqlite: reopen task for retry: %w", reopenErr)
		}
		reopened = reopened.ReopenMailbox(now)
		result, updateErr := tx.ExecContext(ctx,
			`UPDATE tasks SET state = ?, mailbox_closed_at = NULL, retry_count = retry_count + 1, updated_at = ?, revision = revision + 1 WHERE id = ? AND revision = ?`,
			string(reopened.State), formatTime(now), req.TaskID.String(), taskRevision,
		)
		if updateErr != nil {
			return fmt.Errorf("sqlite: reopen task row: %w", updateErr)
		}
		if casErr := requireCAS(result, fmt.Errorf("sqlite: task %s moved during retry: %w", req.TaskID, app.ErrRevisionConflict)); casErr != nil {
			return casErr
		}
		if evidenceErr := recordTransition(ctx, tx, &app.Transition{
			EntityKind: app.EntityTask, EntityID: req.TaskID.String(),
			From: string(task.State), To: string(reopened.State), Reason: "retry accepted", At: now,
		}); evidenceErr != nil {
			return evidenceErr
		}

		requestRowID, err := newUUID()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO retry_requests (id, task_id, requested_by_session, request_id, reason, state, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			requestRowID, req.TaskID.String(), req.Session.String(), nullString(req.RequestID), req.Reason,
			string(app.RetryRequestPending), formatTime(now),
		); err != nil {
			return fmt.Errorf("sqlite: insert retry request: %w", err)
		}
		return record(app.WorkflowAccepted, next.ID.String(), next.Number, "", "")
	})
	if err != nil {
		return app.RetryAccepted{}, err
	}
	return outcome, nil
}

// ClosePlan sets the run's durable plan flag; a plan with zero implement
// tasks is refused, and only a running run accepts the verb.
func (s *Store) ClosePlan(ctx context.Context, req app.PlanClose) (app.PlanCloseResult, error) {
	var outcome app.PlanCloseResult
	err := s.inWriteTx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		digest := app.ComputeRequestDigest(planCloseVerb, req.RunID.String(), app.AddressString(run.ManagerAddress()))
		record := func(kind app.WorkflowOutcomeKind, reason, detail string) error {
			outcome = app.PlanCloseResult{Outcome: kind, Reason: reason, Detail: detail}
			return insertWorkflowReceipt(ctx, tx, &workflowReceipt{
				runID: req.RunID.String(), op: planCloseVerb,
				sessionID: req.Session.String(), incarnationID: req.IncarnationID.String(),
				requestID: req.RequestID, requestDigest: digest,
				outcome: string(kind), detail: detail,
			}, now)
		}

		if req.RequestID != "" {
			priorDigest, _, _, ok, err := acceptedWorkflowReceipt(ctx, tx, req.RunID.String(), planCloseVerb, req.RequestID)
			if err != nil {
				return err
			}
			if ok {
				if priorDigest == digest {
					return record(app.WorkflowDuplicate, "", "")
				}
				return record(app.WorkflowRefused, app.GrammarReasonConflicting, "request id reused with different content")
			}
		}

		reason, refusal, err := requireManagerCaller(ctx, tx, req.RunID, req.Session, req.IncarnationID)
		if err != nil {
			return err
		}
		if refusal != "" {
			return record(app.WorkflowRefused, reason, refusal)
		}
		runV, runRevision, err := getRun(ctx, tx, req.RunID)
		if errors.Is(err, app.ErrNotFound) {
			return record(app.WorkflowMalformed, app.GrammarReasonMalformed, "unknown run")
		}
		if err != nil {
			return err
		}
		var hasImplementTask bool
		if scanErr := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM tasks WHERE run_id = ? AND kind = ?)`,
			req.RunID.String(), string(run.TaskKindImplement),
		).Scan(&hasImplementTask); scanErr != nil {
			return fmt.Errorf("sqlite: read implement tasks of run %s: %w", req.RunID, scanErr)
		}
		closed, closeErr := runV.ClosePlan(hasImplementTask, now)
		switch {
		case closeErr == nil:
		case errors.Is(closeErr, run.ErrEmptyPlan):
			return record(app.WorkflowRefused, app.GrammarReasonEmptyPlan, "plan has no implement task")
		case errors.Is(closeErr, run.ErrRunNotAccepting):
			return record(app.WorkflowRefused, app.GrammarReasonRunNotAccepting, closeErr.Error())
		default:
			return record(app.WorkflowRefused, app.GrammarReasonUnauthorized, closeErr.Error())
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE runs SET plan_closed_at = ?, updated_at = ?, revision = revision + 1 WHERE id = ? AND revision = ?`,
			formatTime(now), formatTime(closed.UpdatedAt), req.RunID.String(), runRevision,
		)
		if err != nil {
			return fmt.Errorf("sqlite: close plan: %w", err)
		}
		if err := requireCAS(result, fmt.Errorf("sqlite: run %s moved during plan close: %w", req.RunID, app.ErrRevisionConflict)); err != nil {
			return err
		}
		return record(app.WorkflowAccepted, "", "")
	})
	if err != nil {
		return app.PlanCloseResult{}, err
	}
	return outcome, nil
}
