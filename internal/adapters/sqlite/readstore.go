package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// inReadTx runs fn inside one deferred read transaction on the read pool,
// so a multi-query load observes a single WAL snapshot.
func (s *Store) inReadTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.reads.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin read transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("sqlite: rollback read transaction: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit read transaction: %w", err)
	}
	return nil
}

// runReconciling reports whether the run has at least one reconciling
// operation.
func runReconciling(ctx context.Context, q querier, runID identity.RunID) (bool, error) {
	var exists int64
	err := q.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM operations WHERE run_id = ? AND state = ?)`,
		runID.String(), string(app.OperationReconciling),
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("sqlite: reconciling condition of run %s: %w", runID, err)
	}
	return exists != 0, nil
}

// ListRuns lists the repository's runs in sequence order. repositoryRoot is
// the symlink-resolved absolute repository root; an unknown root has no
// runs and returns none, never an error.
func (s *Store) ListRuns(ctx context.Context, repositoryRoot string) ([]app.RunStatus, error) {
	var statuses []app.RunStatus
	err := s.inReadTx(ctx, func(tx *sql.Tx) error {
		var repositoryID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM repositories WHERE root_path = ?`, repositoryRoot).Scan(&repositoryID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("sqlite: resolve repository root: %w", err)
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT id, seq, state, stop_requested_at, updated_at FROM runs WHERE repository_id = ? ORDER BY seq`, repositoryID,
		)
		if err != nil {
			return fmt.Errorf("sqlite: list runs: %w", err)
		}
		defer rows.Close() //nolint:errcheck // the deferred close of a fully-iterated read cursor has no failure the rows.Err check below misses.
		for rows.Next() {
			var (
				id, state, updatedAt string
				stopRequestedAt      sql.NullString
				seq                  int64
			)
			if scanErr := rows.Scan(&id, &seq, &state, &stopRequestedAt, &updatedAt); scanErr != nil {
				return fmt.Errorf("sqlite: scan run row: %w", scanErr)
			}
			runID, parseErr := identity.ParseRunID(id)
			if parseErr != nil {
				return fmt.Errorf("sqlite: listed run id: %w", parseErr)
			}
			updated, timeErr := parseTime(updatedAt)
			if timeErr != nil {
				return timeErr
			}
			statuses = append(statuses, app.RunStatus{
				RunID:         runID,
				Sequence:      int(seq),
				State:         run.RunState(state),
				StopRequested: stopRequestedAt.Valid,
				UpdatedAt:     updated,
			})
		}
		if iterErr := rows.Err(); iterErr != nil {
			return fmt.Errorf("sqlite: iterate runs: %w", iterErr)
		}
		for i := range statuses {
			if statuses[i].Reconciling, err = runReconciling(ctx, tx, statuses[i].RunID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return statuses, nil
}

// LoadRunStatus assembles the full detail block for one run. A solo run
// keeps the exact Phase 2 single-task shape; a feature run (a non-NULL
// frozen workflow) has no single task or attempt to name — its TaskID/
// AttemptID stay zero, SessionID is the current manager session, and the
// Phase 3 extensions (task table, latest integration, guard shortfalls,
// mailboxes, pending questions) are populated instead.
func (s *Store) LoadRunStatus(ctx context.Context, runID identity.RunID) (app.RunDetail, error) {
	var detail app.RunDetail
	err := s.inReadTx(ctx, func(tx *sql.Tx) error {
		runV, _, err := getRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		reconciling, err := runReconciling(ctx, tx, runID)
		if err != nil {
			return err
		}
		snapshot, err := loadSnapshot(ctx, tx, runID)
		if err != nil {
			return err
		}
		retiredAt, err := runWorktreesRetiredAt(ctx, tx, runID)
		if err != nil {
			return err
		}
		detail = app.RunDetail{
			RunStatus: app.RunStatus{
				RunID:         runID,
				Sequence:      runV.Sequence,
				State:         runV.State,
				StopRequested: runV.StopRequested,
				Reconciling:   reconciling,
				UpdatedAt:     runV.UpdatedAt,
			},
			Mode:               snapshot.Workflow.Mode,
			TargetBranch:       snapshot.Workflow.TargetBranch,
			WorktreesRetiredAt: retiredAt,
			StateRoot:          snapshot.StateRoot,
		}
		if snapshot.Workflow.Feature() {
			// The current manager session is the run-level session identity
			// a feature run has; its binding and claim surface exactly like
			// the solo worker's.
			manager, _, ok, managerErr := managerSession(ctx, tx, runID)
			if managerErr != nil {
				return managerErr
			}
			if ok {
				detail.SessionID = manager.ID
				if bindErr := attachSessionBinding(ctx, tx, &detail, manager.ID); bindErr != nil {
					return bindErr
				}
			}
			if featureErr := featureRunDetail(ctx, tx, &detail, &snapshot, s.now()); featureErr != nil {
				return featureErr
			}
			if worktreeErr := featureWorktreeDetail(ctx, tx, &detail); worktreeErr != nil {
				return worktreeErr
			}
		} else {
			task, _, taskErr := taskByRun(ctx, tx, runID)
			if taskErr != nil {
				return taskErr
			}
			attempt, _, attemptErr := latestAttempt(ctx, tx, task.ID)
			if attemptErr != nil {
				return attemptErr
			}
			detail.TaskID = task.ID
			detail.AttemptID = attempt.ID
			detail.TaskState = task.State
			detail.AttemptState = attempt.State
			worktree, _, wtErr := getWorktree(ctx, tx, "run_id", runID.String())
			switch {
			case wtErr == nil:
				detail.WorktreePath = worktree.Path
			case errors.Is(wtErr, app.ErrNotFound):
			default:
				return wtErr
			}
			session, _, ok, sessionErr := currentSession(ctx, tx, attempt.ID)
			if sessionErr != nil {
				return sessionErr
			}
			if ok {
				detail.SessionID = session.ID
				if bindErr := attachSessionBinding(ctx, tx, &detail, session.ID); bindErr != nil {
					return bindErr
				}
			}
		}
		if detail.PendingOperations, err = pendingOperations(ctx, tx, runID); err != nil {
			return err
		}
		if detail.LastSubmission, err = lastSubmission(ctx, tx, runID); err != nil {
			return err
		}
		if detail.Artifacts, err = runArtifacts(ctx, tx, runID); err != nil {
			return err
		}
		if detail.LastCheck, err = lastCheckSummary(ctx, tx, runID, detail.Artifacts); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return app.RunDetail{}, err
	}
	return detail, nil
}

// featureWorktreeDetail surfaces a feature run's worktree rows, oldest
// first, and its worktree.retire operations, newest first, for the
// per-row status lines; a run without rows gets an empty, non-nil list.
func featureWorktreeDetail(ctx context.Context, q querier, detail *app.RunDetail) error {
	rows, err := runWorktrees(ctx, q, detail.RunID)
	if err != nil {
		return err
	}
	detail.Worktrees = make([]run.Worktree, 0, len(rows))
	for i := range rows {
		detail.Worktrees = append(detail.Worktrees, rows[i].Worktree)
	}
	detail.WorktreeRetirements, err = operationsByKind(ctx, q, detail.RunID, app.OpWorktreeRetire)
	return err
}

// attachSessionBinding surfaces the session's current binding and its
// incarnation's claim on the detail block, when a binding exists.
func attachSessionBinding(ctx context.Context, q querier, detail *app.RunDetail, sessionID identity.SessionID) error {
	binding, hasBinding, err := currentBinding(ctx, q, sessionID)
	if err != nil {
		return err
	}
	if !hasBinding {
		return nil
	}
	detail.Binding = &binding
	if detail.Claim, err = getLaunchClaim(ctx, q, binding.IncarnationID); err != nil {
		return err
	}
	return nil
}

// lastCheckSummary summarizes the run's newest check execution, whatever
// its journal state — a pending execution and a settled unknown one are
// equally visible. Evidence paths are the run's retained check-stdout,
// check-stderr and pane-snapshot artifacts. The outcome payload is opaque
// journal JSON the store never interprets as authority, so the read model
// reads only the "unknown" (bool) and "detail" (string) members it
// presents, with checked type assertions on the decoded map: an absent,
// null or differently typed member yields its zero value and never fails
// the status load.
func lastCheckSummary(ctx context.Context, q querier, runID identity.RunID, artifacts []run.Artifact) (*app.CheckExecutionSummary, error) {
	op, err := scanOperation(q.QueryRowContext(ctx,
		selectOperationColumns+` WHERE run_id = ? AND kind = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`,
		runID.String(), string(app.OpCheckRun),
	).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // a nil summary with a nil error is the documented "no check execution yet" value.
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: load newest check execution of run %s: %w", runID, err)
	}
	summary := app.CheckExecutionSummary{OperationID: op.ID, State: op.State}
	if fields, ok := op.Outcome.(map[string]any); ok {
		if unknown, ok := fields["unknown"].(bool); ok {
			summary.Unknown = unknown
		}
		if detail, ok := fields["detail"].(string); ok {
			summary.Detail = detail
		}
	}
	for _, artifact := range artifacts {
		switch artifact.Kind {
		case run.ArtifactCheckStdout, run.ArtifactCheckStderr, run.ArtifactPaneSnapshot:
			summary.EvidencePaths = append(summary.EvidencePaths, artifact.Path)
		}
	}
	return &summary, nil
}

// lastSubmission loads the newest submission receipt claiming the run, or
// nil when none exists.
func lastSubmission(ctx context.Context, q querier, runID identity.RunID) (*app.SubmissionOutcome, error) {
	var (
		outcome, detail string
		resultID        sql.NullString
	)
	err := q.QueryRowContext(ctx,
		`SELECT outcome, result_id, detail FROM result_submissions WHERE claimed_run_id = ? ORDER BY submitted_at DESC, rowid DESC LIMIT 1`,
		runID.String(),
	).Scan(&outcome, &resultID, &detail)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // a nil outcome with a nil error is the documented "no submission yet" value.
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: load last submission of run %s: %w", runID, err)
	}
	submission := app.SubmissionOutcome{Kind: app.SubmissionOutcomeKind(outcome), Detail: detail}
	if resultID.Valid {
		parsed, err := identity.ParseResultID(resultID.String)
		if err != nil {
			return nil, fmt.Errorf("sqlite: last submission result id: %w", err)
		}
		submission.ResultID = parsed
	}
	return &submission, nil
}

// runArtifacts lists the run's artifact references, oldest first.
func runArtifacts(ctx context.Context, q querier, runID identity.RunID) ([]run.Artifact, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, result_id, kind, path, digest FROM artifacts WHERE run_id = ? ORDER BY created_at, rowid`,
		runID.String(),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list artifacts of run %s: %w", runID, err)
	}
	defer rows.Close() //nolint:errcheck // the deferred close of a fully-iterated read cursor has no failure the rows.Err check below misses.
	var artifacts []run.Artifact
	for rows.Next() {
		var (
			id, kind, path, digest string
			resultID               sql.NullString
		)
		if err := rows.Scan(&id, &resultID, &kind, &path, &digest); err != nil {
			return nil, fmt.Errorf("sqlite: scan artifact row: %w", err)
		}
		artifactID, err := identity.ParseArtifactID(id)
		if err != nil {
			return nil, fmt.Errorf("sqlite: artifact id: %w", err)
		}
		artifact := run.Artifact{ID: artifactID, RunID: runID, Kind: run.ArtifactKind(kind), Path: path, Digest: digest}
		if resultID.Valid {
			parsed, err := identity.ParseResultID(resultID.String)
			if err != nil {
				return nil, fmt.Errorf("sqlite: artifact %s result id: %w", id, err)
			}
			artifact.ResultID = &parsed
		}
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate artifacts: %w", err)
	}
	return artifacts, nil
}

// loadSnapshot reads one run's frozen snapshot.
func loadSnapshot(ctx context.Context, q querier, runID identity.RunID) (app.RunSnapshot, error) {
	var (
		checkArgv, envPolicy, harness, stateRoot, assignmentPath, assignmentDigest string
		profileDir, workflow                                                       sql.NullString
		checkTimeoutMS, checkRepeatable                                            int64
	)
	err := q.QueryRowContext(ctx,
		`SELECT check_argv, check_timeout_ms, check_repeatable, env_policy, harness, profile_dir, state_root, assignment_path, assignment_digest, workflow FROM run_snapshots WHERE run_id = ?`,
		runID.String(),
	).Scan(&checkArgv, &checkTimeoutMS, &checkRepeatable, &envPolicy, &harness, &profileDir, &stateRoot, &assignmentPath, &assignmentDigest, &workflow)
	if errors.Is(err, sql.ErrNoRows) {
		return app.RunSnapshot{}, fmt.Errorf("sqlite: snapshot of run %s: %w", runID, app.ErrNotFound)
	}
	if err != nil {
		return app.RunSnapshot{}, fmt.Errorf("sqlite: load snapshot of run %s: %w", runID, err)
	}
	snapshot := app.RunSnapshot{
		CheckTimeout:     time.Duration(checkTimeoutMS) * time.Millisecond,
		CheckRepeatable:  checkRepeatable != 0,
		Harness:          harness,
		ProfileDir:       profileDir.String,
		StateRoot:        stateRoot,
		AssignmentPath:   assignmentPath,
		AssignmentDigest: assignmentDigest,
	}
	if err := json.Unmarshal([]byte(checkArgv), &snapshot.CheckArgv); err != nil {
		return app.RunSnapshot{}, fmt.Errorf("sqlite: decode check argv of run %s: %w", runID, err)
	}
	if err := json.Unmarshal([]byte(envPolicy), &snapshot.EnvPolicy); err != nil {
		return app.RunSnapshot{}, fmt.Errorf("sqlite: decode env policy of run %s: %w", runID, err)
	}
	// NULL means solo (pre-Phase 3 rows and every solo freeze); the zero
	// WorkflowSnapshot already says exactly that.
	if workflow.Valid {
		if err := json.Unmarshal([]byte(workflow.String), &snapshot.Workflow); err != nil {
			return app.RunSnapshot{}, fmt.Errorf("sqlite: decode workflow snapshot of run %s: %w", runID, err)
		}
	}
	return snapshot, nil
}

// LoadFrozenRun returns the run's frozen execution inputs, lease-free: the
// immutable snapshot, the repository root it belongs to, and the frozen
// brief text — the check use case reads its argv, timeout, repeatability
// and state root from here, never from caller arguments.
func (s *Store) LoadFrozenRun(ctx context.Context, runID identity.RunID) (app.FrozenRun, error) {
	var frozen app.FrozenRun
	err := s.inReadTx(ctx, func(tx *sql.Tx) error {
		snapshot, err := loadSnapshot(ctx, tx, runID)
		if err != nil {
			return err
		}
		var repositoryID, brief string
		err = tx.QueryRowContext(ctx,
			`SELECT repository_id, brief FROM runs WHERE id = ?`, runID.String(),
		).Scan(&repositoryID, &brief)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: run %s: %w", runID, app.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("sqlite: load frozen run %s: %w", runID, err)
		}
		var repositoryRoot string
		if err := tx.QueryRowContext(ctx,
			`SELECT root_path FROM repositories WHERE id = ?`, repositoryID,
		).Scan(&repositoryRoot); err != nil {
			return fmt.Errorf("sqlite: resolve repository root of run %s: %w", runID, err)
		}
		frozen = app.FrozenRun{Snapshot: snapshot, RepositoryRoot: repositoryRoot, Brief: brief}
		return nil
	})
	if err != nil {
		return app.FrozenRun{}, err
	}
	return frozen, nil
}

// LoadCheckExecutionContext loads what the generalized check exec boundary
// needs, addressed by the execution's operation ID, dispatching the frozen
// argv by operation kind (the agreed slice-3/slice-4 contract — no
// signature change): a check.run returns the snapshot's frozen CheckArgv
// with the design's fixed checkout layout under the state root
// (runs/<run>/checks/<operation>/tree); an integration.merge returns the
// intent's own frozen merge_argv with the intent's scratch tree path; a
// retirement.check or worktree.retire returns the intent's frozen argv
// with its spawn directory (the "argv" and "cwd" members). A malformed
// intent fails closed rather than hand the boundary an argv nothing
// froze.
func (s *Store) LoadCheckExecutionContext(ctx context.Context, opID identity.OperationID) (app.CheckExecutionContext, error) {
	var executionContext app.CheckExecutionContext
	err := s.inReadTx(ctx, func(tx *sql.Tx) error {
		op, err := getOperation(ctx, tx, opID)
		if err != nil {
			return err
		}
		snapshot, err := loadSnapshot(ctx, tx, op.RunID)
		if err != nil {
			return err
		}
		executionContext = app.CheckExecutionContext{
			EnvPolicy: snapshot.EnvPolicy,
			StateRoot: snapshot.StateRoot,
		}
		switch op.Kind {
		case app.OpCheckRun:
			executionContext.CheckoutPath = filepath.Join(snapshot.StateRoot, "runs", op.RunID.String(), "checks", opID.String(), "tree")
			executionContext.CheckArgv = snapshot.CheckArgv
			return nil
		case app.OpIntegrationMerge:
			argv, treePath, intentErr := mergeIntentExecution(op.Intent)
			if intentErr != nil {
				return fmt.Errorf("sqlite: operation %s: %w", opID, intentErr)
			}
			executionContext.CheckoutPath = treePath
			executionContext.CheckArgv = argv
			return nil
		case app.OpRetirementCheck, app.OpWorktreeRetire:
			argv, cwd, intentErr := retirementIntentExecution(op.Intent)
			if intentErr != nil {
				return fmt.Errorf("sqlite: operation %s: %w", opID, intentErr)
			}
			executionContext.CheckoutPath = cwd
			executionContext.CheckArgv = argv
			return nil
		default:
			return fmt.Errorf("sqlite: operation %s is %q, not an exec-claimable execution", opID, op.Kind)
		}
	})
	if err != nil {
		return app.CheckExecutionContext{}, err
	}
	return executionContext, nil
}

// retirementIntentExecution reads the frozen argv and spawn directory
// from a persisted worktree-retirement intent payload — the documented
// worktreeRetirementExecIntent JSON keys "argv" and "cwd"
// (internal/app/worktreeretirement.go) — failing closed on any missing or
// mistyped member, exactly as mergeIntentExecution does.
func retirementIntentExecution(intent any) (argv []string, cwd string, err error) {
	fields, ok := intent.(map[string]any)
	if !ok {
		return nil, "", errors.New("retirement intent payload is not a JSON object")
	}
	rawArgv, ok := fields["argv"].([]any)
	if !ok || len(rawArgv) == 0 {
		return nil, "", errors.New("retirement intent carries no frozen argv")
	}
	argv = make([]string, len(rawArgv))
	for i, element := range rawArgv {
		text, isString := element.(string)
		if !isString {
			return nil, "", errors.New("retirement intent argv carries a non-string element")
		}
		argv[i] = text
	}
	cwd, ok = fields["cwd"].(string)
	if !ok || cwd == "" {
		return nil, "", errors.New("retirement intent carries no spawn directory")
	}
	return argv, cwd, nil
}

// mergeIntentExecution reads the frozen merge argv and scratch tree path
// from a persisted integration.merge intent payload — the documented
// integrationMergeIntent JSON keys "merge_argv" and "tree_path"
// (internal/app/usecase_integration.go). The store persists intents
// without interpreting them, so this reads the generic decoding, failing
// closed on any missing or mistyped member.
func mergeIntentExecution(intent any) (argv []string, treePath string, err error) {
	fields, ok := intent.(map[string]any)
	if !ok {
		return nil, "", errors.New("merge intent payload is not a JSON object")
	}
	rawArgv, ok := fields["merge_argv"].([]any)
	if !ok || len(rawArgv) == 0 {
		return nil, "", errors.New("merge intent carries no frozen merge argv")
	}
	argv = make([]string, len(rawArgv))
	for i, element := range rawArgv {
		text, isString := element.(string)
		if !isString {
			return nil, "", errors.New("merge intent argv carries a non-string element")
		}
		argv[i] = text
	}
	treePath, ok = fields["tree_path"].(string)
	if !ok || treePath == "" {
		return nil, "", errors.New("merge intent carries no scratch tree path")
	}
	return argv, treePath, nil
}
