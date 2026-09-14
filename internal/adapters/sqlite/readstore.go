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
			`SELECT id, seq, state, updated_at FROM runs WHERE repository_id = ? ORDER BY seq`, repositoryID,
		)
		if err != nil {
			return fmt.Errorf("sqlite: list runs: %w", err)
		}
		defer rows.Close() //nolint:errcheck // the deferred close of a fully-iterated read cursor has no failure the rows.Err check below misses.
		for rows.Next() {
			var (
				id, state, updatedAt string
				seq                  int64
			)
			if scanErr := rows.Scan(&id, &seq, &state, &updatedAt); scanErr != nil {
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
				RunID:     runID,
				Sequence:  int(seq),
				State:     run.RunState(state),
				UpdatedAt: updated,
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

// LoadRunStatus assembles the full detail block for one run.
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
		task, _, err := taskByRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		attempt, _, err := latestAttempt(ctx, tx, task.ID)
		if err != nil {
			return err
		}
		detail = app.RunDetail{
			RunStatus: app.RunStatus{
				RunID:       runID,
				Sequence:    runV.Sequence,
				State:       runV.State,
				Reconciling: reconciling,
				UpdatedAt:   runV.UpdatedAt,
			},
			TaskID:       task.ID,
			AttemptID:    attempt.ID,
			TaskState:    task.State,
			AttemptState: attempt.State,
		}
		worktree, _, err := getWorktree(ctx, tx, "run_id", runID.String())
		switch {
		case err == nil:
			detail.WorktreePath = worktree.Path
		case errors.Is(err, app.ErrNotFound):
		default:
			return err
		}
		session, _, ok, err := currentSession(ctx, tx, attempt.ID)
		if err != nil {
			return err
		}
		if ok {
			detail.SessionID = session.ID
			binding, hasBinding, bindingErr := currentBinding(ctx, tx, session.ID)
			if bindingErr != nil {
				return bindingErr
			}
			if hasBinding {
				detail.Binding = &binding
				if detail.Claim, bindingErr = getLaunchClaim(ctx, tx, binding.IncarnationID); bindingErr != nil {
					return bindingErr
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
		return nil
	})
	if err != nil {
		return app.RunDetail{}, err
	}
	return detail, nil
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
		profileDir                                                                 sql.NullString
		checkTimeoutMS, checkRepeatable                                            int64
	)
	err := q.QueryRowContext(ctx,
		`SELECT check_argv, check_timeout_ms, check_repeatable, env_policy, harness, profile_dir, state_root, assignment_path, assignment_digest FROM run_snapshots WHERE run_id = ?`,
		runID.String(),
	).Scan(&checkArgv, &checkTimeoutMS, &checkRepeatable, &envPolicy, &harness, &profileDir, &stateRoot, &assignmentPath, &assignmentDigest)
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
	return snapshot, nil
}

// LoadLaunchContext loads what the launch exec boundary needs, without any
// lease: the frozen snapshot, the attempt, the session, the stop state,
// and — when the controller has already recorded them — the session's
// current binding and that incarnation's launch-claim state. It succeeds
// as soon as InitializeRun's rows exist: the launcher starts as the pane's
// own command and may load before the binding row is committed, in which
// case Binding is the zero value and Claim is nil.
func (s *Store) LoadLaunchContext(ctx context.Context, runID identity.RunID, attemptID identity.AttemptID) (app.LaunchContext, error) {
	var launchContext app.LaunchContext
	err := s.inReadTx(ctx, func(tx *sql.Tx) error {
		runV, _, err := getRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		attempt, _, err := getAttempt(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		task, _, err := getTask(ctx, tx, attempt.TaskID)
		if err != nil {
			return err
		}
		if task.RunID != runID {
			return fmt.Errorf("sqlite: attempt %s does not belong to run %s: %w", attemptID, runID, app.ErrNotFound)
		}
		snapshot, err := loadSnapshot(ctx, tx, runID)
		if err != nil {
			return err
		}
		session, _, ok, err := currentSession(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("sqlite: current session of attempt %s: %w", attemptID, app.ErrNotFound)
		}
		// The launcher is the pane's own command, so it can run before the
		// controller has recorded the binding row as the pane.open outcome.
		// A missing binding therefore never fails this load: Binding stays
		// its zero value and Claim nil — the launcher does not need pane
		// identifiers, and a claim is keyed by the binding's incarnation.
		binding, hasBinding, err := currentBinding(ctx, tx, session.ID)
		if err != nil {
			return err
		}
		var claim *app.LaunchClaim
		if hasBinding {
			if claim, err = getLaunchClaim(ctx, tx, binding.IncarnationID); err != nil {
				return err
			}
		}
		launchContext = app.LaunchContext{
			Snapshot:      snapshot,
			Harness:       session.Harness,
			Attempt:       attempt,
			Session:       session,
			Binding:       binding,
			Claim:         claim,
			StopRequested: runV.StopRequested || runV.State == run.RunStopping || runV.State == run.RunStopped,
		}
		return nil
	})
	if err != nil {
		return app.LaunchContext{}, err
	}
	return launchContext, nil
}

// LoadCheckExecutionContext loads what the check exec boundary needs,
// addressed by the check execution's operation ID. The candidate checkout
// path is the design's fixed layout under the frozen state root:
// runs/<run>/checks/<operation>/tree.
func (s *Store) LoadCheckExecutionContext(ctx context.Context, opID identity.OperationID) (app.CheckExecutionContext, error) {
	var executionContext app.CheckExecutionContext
	err := s.inReadTx(ctx, func(tx *sql.Tx) error {
		op, err := getOperation(ctx, tx, opID)
		if err != nil {
			return err
		}
		if op.Kind != app.OpCheckRun {
			return fmt.Errorf("sqlite: operation %s is %q, not a check execution", opID, op.Kind)
		}
		snapshot, err := loadSnapshot(ctx, tx, op.RunID)
		if err != nil {
			return err
		}
		executionContext = app.CheckExecutionContext{
			EnvPolicy:    snapshot.EnvPolicy,
			StateRoot:    snapshot.StateRoot,
			CheckoutPath: filepath.Join(snapshot.StateRoot, "runs", op.RunID.String(), "checks", opID.String(), "tree"),
			CheckArgv:    snapshot.CheckArgv,
		}
		return nil
	})
	if err != nil {
		return app.CheckExecutionContext{}, err
	}
	return executionContext, nil
}
