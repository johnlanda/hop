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

// receipt is one result_submissions row: the durable evidence every
// submission attempt leaves, whatever its outcome. Claimed identities are
// stored as plain text with no foreign keys.
type receipt struct {
	runID, taskID, attemptID, incarnationID string
	commitOID, summary                      string
	digest                                  string // "" stores NULL
	resultID                                string // "" stores NULL
	outcome                                 app.SubmissionOutcomeKind
	detail                                  string
}

// insertReceipt records one submission receipt at time at. Free-text fields
// are truncated to ClaimedSubmissionFieldLimit bytes so a receipt is
// evidence, never an unbounded copy of worker input.
func insertReceipt(ctx context.Context, q querier, r *receipt, at time.Time) error {
	id, err := newUUID()
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx,
		`INSERT INTO result_submissions (id, claimed_run_id, claimed_task_id, claimed_attempt_id, claimed_incarnation_id, claimed_commit_oid, claimed_summary, content_digest, result_id, outcome, detail, submitted_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, truncateClaim(r.runID), truncateClaim(r.taskID), truncateClaim(r.attemptID), truncateClaim(r.incarnationID),
		nullString(truncateClaim(r.commitOID)), nullString(truncateClaim(r.summary)), nullString(r.digest),
		nullString(r.resultID), string(r.outcome), truncateClaim(r.detail), formatTime(at),
	); err != nil {
		return fmt.Errorf("sqlite: record submission receipt: %w", err)
	}
	return nil
}

// truncateClaim bounds one claimed field to ClaimedSubmissionFieldLimit
// bytes.
func truncateClaim(v string) string {
	if len(v) > app.ClaimedSubmissionFieldLimit {
		return v[:app.ClaimedSubmissionFieldLimit]
	}
	return v
}

// RecordMalformed records a submission that failed application-side step 1
// parsing or bounds checks, with the claimed values as received.
func (s *Store) RecordMalformed(ctx context.Context, claimed app.ClaimedSubmission) (app.SubmissionOutcome, error) { //nolint:gocritic // hugeParam: the port passes the claimed values by value; the adapter mirrors its signature.
	outcome := app.SubmissionOutcome{Kind: app.SubmissionMalformed, Detail: truncateClaim(claimed.Detail)}
	err := s.inWriteTx(ctx, func(tx *sql.Tx) error {
		return insertReceipt(ctx, tx, &receipt{
			runID:         claimed.RunID,
			taskID:        claimed.TaskID,
			attemptID:     claimed.AttemptID,
			incarnationID: claimed.IncarnationID,
			commitOID:     claimed.CommitOID,
			summary:       claimed.Summary,
			outcome:       app.SubmissionMalformed,
			detail:        claimed.Detail,
		}, s.now())
	})
	if err != nil {
		return app.SubmissionOutcome{}, err
	}
	return outcome, nil
}

// SubmitResult applies the section 7 validation order atomically inside
// one write transaction: existence and agreement (step 2, malformed on
// disagreement), the prior accepted result before any state or incarnation
// precondition (duplicate or conflicting), then eligibility and acceptance
// through the domain's AcceptResult, persisting on acceptance the result,
// the receipt, the unique check request and every implied transition with
// its evidence rows — all in the same transaction. Every recorded outcome
// returns a nil error; a non-nil error is an infrastructure failure with
// nothing recorded.
func (s *Store) SubmitResult(ctx context.Context, submission app.ResultSubmission) (app.SubmissionOutcome, error) { //nolint:gocritic // hugeParam: the port passes the submission by value; the adapter mirrors its signature.
	var outcome app.SubmissionOutcome
	err := s.inWriteTx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		malformed := func(detail string) error {
			outcome = app.SubmissionOutcome{Kind: app.SubmissionMalformed, Detail: detail}
			return insertReceipt(ctx, tx, submissionReceipt(&submission, app.SubmissionMalformed, "", detail), now)
		}

		runV, runRevision, err := getRun(ctx, tx, submission.RunID)
		if errors.Is(err, app.ErrNotFound) {
			return malformed(fmt.Sprintf("run %s does not exist", submission.RunID))
		}
		if err != nil {
			return err
		}
		task, taskRevision, err := getTask(ctx, tx, submission.TaskID)
		if errors.Is(err, app.ErrNotFound) {
			return malformed(fmt.Sprintf("task %s does not exist", submission.TaskID))
		}
		if err != nil {
			return err
		}
		if task.RunID != submission.RunID {
			return malformed(fmt.Sprintf("task %s does not belong to run %s", submission.TaskID, submission.RunID))
		}
		attempt, attemptRevision, err := getAttempt(ctx, tx, submission.AttemptID)
		if errors.Is(err, app.ErrNotFound) {
			return malformed(fmt.Sprintf("attempt %s does not exist", submission.AttemptID))
		}
		if err != nil {
			return err
		}
		if attempt.TaskID != submission.TaskID {
			return malformed(fmt.Sprintf("attempt %s does not belong to task %s", submission.AttemptID, submission.TaskID))
		}

		prior, err := acceptedResult(ctx, tx, submission.AttemptID)
		if err != nil {
			return err
		}
		incarnationIsCurrent, err := incarnationCurrent(ctx, tx, submission.AttemptID, submission.IncarnationID)
		if err != nil {
			return err
		}
		claim, err := getLaunchClaim(ctx, tx, submission.IncarnationID)
		if err != nil {
			return err
		}
		acceptance, err := run.AcceptResult(runV, task, attempt, prior,
			run.AcceptanceContext{
				IncarnationCurrent: incarnationIsCurrent,
				LaunchClaimSettled: claim != nil && claim.State == app.LaunchClaimExeced,
			},
			run.ResultSubmission{ID: submission.ID, CommitOID: submission.CommitOID, Summary: submission.Summary, Digest: submission.Digest},
			now,
		)
		switch {
		case err == nil:
		case errors.Is(err, run.ErrDuplicateResult):
			outcome = app.SubmissionOutcome{Kind: app.SubmissionDuplicate, ResultID: acceptance.Result.ID, Detail: err.Error()}
			return insertReceipt(ctx, tx, submissionReceipt(&submission, app.SubmissionDuplicate, acceptance.Result.ID.String(), err.Error()), now)
		case errors.Is(err, run.ErrConflictingResult):
			outcome = app.SubmissionOutcome{Kind: app.SubmissionConflicting, Detail: err.Error()}
			return insertReceipt(ctx, tx, submissionReceipt(&submission, app.SubmissionConflicting, "", err.Error()), now)
		case errors.Is(err, run.ErrStaleSubmission):
			outcome = app.SubmissionOutcome{Kind: app.SubmissionStale, Detail: err.Error()}
			return insertReceipt(ctx, tx, submissionReceipt(&submission, app.SubmissionStale, "", err.Error()), now)
		case errors.Is(err, run.ErrTransientNotRunning):
			outcome = app.SubmissionOutcome{Kind: app.SubmissionTransient, Detail: err.Error()}
			return insertReceipt(ctx, tx, submissionReceipt(&submission, app.SubmissionTransient, "", err.Error()), now)
		default:
			return fmt.Errorf("sqlite: acceptance decision: %w", err)
		}

		if err := persistAcceptance(ctx, tx, acceptance, acceptanceRevisions{
			run:     runRevision,
			task:    taskRevision,
			attempt: attemptRevision,
		}, entityStates{run: runV.State, task: task.State, attempt: attempt.State}, now); err != nil {
			return err
		}
		outcome = app.SubmissionOutcome{Kind: app.SubmissionAccepted, ResultID: acceptance.Result.ID}
		return insertReceipt(ctx, tx, submissionReceipt(&submission, app.SubmissionAccepted, acceptance.Result.ID.String(), ""), now)
	})
	if err != nil {
		return app.SubmissionOutcome{}, err
	}
	return outcome, nil
}

// submissionReceipt maps a typed submission onto its receipt row.
func submissionReceipt(submission *app.ResultSubmission, outcome app.SubmissionOutcomeKind, resultID, detail string) *receipt {
	return &receipt{
		runID:         submission.RunID.String(),
		taskID:        submission.TaskID.String(),
		attemptID:     submission.AttemptID.String(),
		incarnationID: submission.IncarnationID.String(),
		commitOID:     submission.CommitOID,
		summary:       submission.Summary,
		digest:        submission.Digest,
		resultID:      resultID,
		outcome:       outcome,
		detail:        detail,
	}
}

// acceptanceRevisions carries the revisions the acceptance transaction read
// for its three entity updates.
type acceptanceRevisions struct {
	run, task, attempt int64
}

// entityStates carries the pre-acceptance states for transition evidence.
type entityStates struct {
	run     run.RunState
	task    run.TaskState
	attempt run.AttemptState
}

// persistAcceptance writes the atomic handoff: the accepted result, the
// attempt, task and (when it moved) run updates, the unique check request,
// and one transition-evidence row per changed entity, generation NULL —
// this is a worker-authority transaction.
func persistAcceptance(ctx context.Context, tx *sql.Tx, acceptance run.AcceptanceOutcome, revisions acceptanceRevisions, before entityStates, now time.Time) error { //nolint:gocritic // hugeParam: the acceptance outcome is the domain's immutable decision; by-value keeps it aliasing-free.
	result := acceptance.Result
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO results (id, attempt_id, commit_oid, summary, content_digest, accepted, submitted_at) VALUES (?, ?, ?, ?, ?, 1, ?)`,
		result.ID.String(), result.AttemptID.String(), result.CommitOID, result.Summary, result.ContentDigest, formatTime(result.SubmittedAt),
	); err != nil {
		return fmt.Errorf("sqlite: insert accepted result: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO check_requests (result_id, attempt_id, state, created_at, claimed_generation) VALUES (?, ?, ?, ?, NULL)`,
		result.ID.String(), result.AttemptID.String(), string(app.CheckRequestRequested), formatTime(now),
	); err != nil {
		return fmt.Errorf("sqlite: insert check request: %w", err)
	}
	at := formatTime(now)
	if result, err := tx.ExecContext(ctx,
		`UPDATE attempts SET state = ?, updated_at = ?, revision = revision + 1 WHERE id = ? AND revision = ?`,
		string(acceptance.Attempt.State), at, acceptance.Attempt.ID.String(), revisions.attempt,
	); err != nil {
		return fmt.Errorf("sqlite: apply attempt handoff: %w", err)
	} else if err := requireCAS(result, fmt.Errorf("sqlite: attempt %s moved during acceptance: %w", acceptance.Attempt.ID, app.ErrRevisionConflict)); err != nil {
		return err
	}
	if result, err := tx.ExecContext(ctx,
		`UPDATE tasks SET state = ?, updated_at = ?, revision = revision + 1 WHERE id = ? AND revision = ?`,
		string(acceptance.Task.State), at, acceptance.Task.ID.String(), revisions.task,
	); err != nil {
		return fmt.Errorf("sqlite: apply task handoff: %w", err)
	} else if err := requireCAS(result, fmt.Errorf("sqlite: task %s moved during acceptance: %w", acceptance.Task.ID, app.ErrRevisionConflict)); err != nil {
		return err
	}
	transitions := []app.Transition{
		{EntityKind: app.EntityAttempt, EntityID: acceptance.Attempt.ID.String(), From: string(before.attempt), To: string(acceptance.Attempt.State), Reason: "result accepted", At: now},
		{EntityKind: app.EntityTask, EntityID: acceptance.Task.ID.String(), From: string(before.task), To: string(acceptance.Task.State), Reason: "result accepted", At: now},
	}
	if acceptance.Run.State != before.run {
		if result, err := tx.ExecContext(ctx,
			`UPDATE runs SET state = ?, updated_at = ?, revision = revision + 1 WHERE id = ? AND revision = ?`,
			string(acceptance.Run.State), at, acceptance.Run.ID.String(), revisions.run,
		); err != nil {
			return fmt.Errorf("sqlite: apply run handoff: %w", err)
		} else if err := requireCAS(result, fmt.Errorf("sqlite: run %s moved during acceptance: %w", acceptance.Run.ID, app.ErrRevisionConflict)); err != nil {
			return err
		}
		transitions = append(transitions, app.Transition{
			EntityKind: app.EntityRun, EntityID: acceptance.Run.ID.String(),
			From: string(before.run), To: string(acceptance.Run.State),
			Reason: "early submission accepted", At: now,
		})
	}
	for i := range transitions {
		if err := recordTransition(ctx, tx, &transitions[i]); err != nil {
			return err
		}
	}
	return nil
}

// ClaimLaunch records hop launch's durable pre-exec claim. Agreement is
// validated first — the claimed attempt's persisted owning run must be the
// claimed run, so a mixed tuple can never have its stop or currency
// questions answered against the wrong run. It then fails — and the caller
// must not exec — when the run is stopping or stopped, the incarnation is
// not the attempt's current identity, or a claim for this incarnation
// already exists with a different run, attempt or pid; a rewrite by the
// same pid on the same tuple is idempotent. Currency follows
// launchIncarnationCurrent: the
// current binding decides when one exists, and before any binding row the
// authority is the newest pending launch operation's intent JSON
// ("incarnation_id"), so a launcher racing the controller's pane.open
// outcome write is admitted while a retired incarnation's launcher never
// is.
func (s *Store) ClaimLaunch(ctx context.Context, claim app.LaunchClaim) error { //nolint:gocritic // hugeParam: the port passes the claim value; the adapter mirrors its signature.
	return s.inWriteTx(ctx, func(tx *sql.Tx) error {
		runV, _, err := getRun(ctx, tx, claim.RunID)
		if err != nil {
			return err
		}
		// Agreement precedes every other precondition: a mixed tuple must
		// never have its stop or currency questions answered against the
		// wrong run's state.
		attemptOwner, err := runOfAttempt(ctx, tx, claim.AttemptID)
		if err != nil {
			return err
		}
		if attemptOwner != claim.RunID {
			return fmt.Errorf("sqlite: attempt %s belongs to run %s, not the claimed run %s; launch claim refused", claim.AttemptID, attemptOwner, claim.RunID)
		}
		if runV.StopRequested || runV.State == run.RunStopping || runV.State == run.RunStopped {
			return fmt.Errorf("sqlite: run %s is stopping or stopped; launch claim refused", claim.RunID)
		}
		isCurrent, err := launchIncarnationCurrent(ctx, tx, claim.RunID, claim.AttemptID, claim.IncarnationID)
		if err != nil {
			return err
		}
		if !isCurrent {
			return fmt.Errorf("sqlite: incarnation %s is not attempt %s's current identity; launch claim refused", claim.IncarnationID, claim.AttemptID)
		}
		existing, err := getLaunchClaim(ctx, tx, claim.IncarnationID)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.RunID != claim.RunID || existing.AttemptID != claim.AttemptID {
				return fmt.Errorf("sqlite: incarnation %s's existing claim is for run %s attempt %s; a claim for run %s attempt %s is refused", claim.IncarnationID, existing.RunID, existing.AttemptID, claim.RunID, claim.AttemptID)
			}
			if existing.PID != claim.PID {
				return fmt.Errorf("sqlite: incarnation %s already has a launch claim by pid %d; a claim by pid %d is refused", claim.IncarnationID, existing.PID, claim.PID)
			}
			if existing.State != app.LaunchClaimExecPending {
				return fmt.Errorf("sqlite: incarnation %s's launch claim is already settled %s; a launcher retry never rewrites settled history", claim.IncarnationID, existing.State)
			}
			if existing.Executable != claim.Executable || existing.ArgvDigest != claim.ArgvDigest {
				return fmt.Errorf("sqlite: incarnation %s's existing claim records a different executable or argv; an incompatible retry is refused", claim.IncarnationID)
			}
			// A same-pid retry re-applies the seed, so the row's evidence
			// tracks the newest application (healing NULL rows written
			// before the seed-evidence migration); every invocation
			// identity field stays exactly as first claimed.
			if existing.SeedEvidence != claim.SeedEvidence {
				if _, err := tx.ExecContext(ctx,
					`UPDATE launch_claims SET seed_evidence = ? WHERE incarnation_id = ?`,
					sql.NullString{String: claim.SeedEvidence, Valid: claim.SeedEvidence != ""}, claim.IncarnationID.String(),
				); err != nil {
					return fmt.Errorf("sqlite: refresh launch claim seed evidence: %w", err)
				}
			}
			return nil
		}
		claimedAt := claim.ClaimedAt
		if claimedAt.IsZero() {
			claimedAt = s.now()
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO launch_claims (incarnation_id, run_id, attempt_id, executable, argv_digest, pid, state, error, claimed_at, settled_at, settlement_evidence, seed_evidence)
			 VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, NULL, NULL, ?)`,
			claim.IncarnationID.String(), claim.RunID.String(), claim.AttemptID.String(), claim.Executable, claim.ArgvDigest,
			claim.PID, string(app.LaunchClaimExecPending), formatTime(claimedAt),
			sql.NullString{String: claim.SeedEvidence, Valid: claim.SeedEvidence != ""},
		); err != nil {
			return fmt.Errorf("sqlite: insert launch claim: %w", err)
		}
		return nil
	})
}

// SettleLaunchFailure records exec_failed on the launcher's own error path.
// Settling an already exec_failed claim is idempotent; an execed claim can
// no longer fail this way.
func (s *Store) SettleLaunchFailure(ctx context.Context, incarnation identity.IncarnationID, reason string) error {
	return s.inWriteTx(ctx, func(tx *sql.Tx) error {
		claim, err := getLaunchClaim(ctx, tx, incarnation)
		if err != nil {
			return err
		}
		if claim == nil {
			return fmt.Errorf("sqlite: launch claim of incarnation %s: %w", incarnation, app.ErrNotFound)
		}
		switch claim.State {
		case app.LaunchClaimExecFailed:
			return nil
		case app.LaunchClaimExeced:
			return fmt.Errorf("sqlite: launch claim %s already settled execed; exec failure refused", incarnation)
		case app.LaunchClaimExecPending:
		default:
			return fmt.Errorf("sqlite: launch claim %s has unknown state %q", incarnation, claim.State)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE launch_claims SET state = ?, error = ?, settled_at = ? WHERE incarnation_id = ?`,
			string(app.LaunchClaimExecFailed), reason, formatTime(s.now()), incarnation.String(),
		); err != nil {
			return fmt.Errorf("sqlite: settle launch failure: %w", err)
		}
		return nil
	})
}

// ClaimCheckExec records hop check-exec's durable pre-exec identity: its
// own pid, which is its process-group id. It fails when the operation is
// not a pending check execution of the run's current lease generation; a
// rewrite by the same pid is idempotent. The operation ID is the only
// claimed identity, so there is no cross-run tuple to disagree: the owning
// run, its lease generation and the operation's kind and state all resolve
// from the persisted operation row, never from caller input.
func (s *Store) ClaimCheckExec(ctx context.Context, opID identity.OperationID, pid int) error {
	return s.inWriteTx(ctx, func(tx *sql.Tx) error {
		op, err := getOperation(ctx, tx, opID)
		if err != nil {
			return err
		}
		if op.Kind != app.OpCheckRun || op.State != app.OperationPending {
			return fmt.Errorf("sqlite: operation %s is %s %q, not a pending check execution", opID, op.State, op.Kind)
		}
		var generation int64
		err = tx.QueryRowContext(ctx, `SELECT generation FROM run_leases WHERE run_id = ?`, op.RunID.String()).Scan(&generation)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: lease of run %s: %w", op.RunID, app.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("sqlite: read lease generation of run %s: %w", op.RunID, err)
		}
		if op.Generation != generation {
			return fmt.Errorf("sqlite: operation %s is generation %d but the run's current generation is %d; check-exec claim refused", opID, op.Generation, generation)
		}
		var existingPID int64
		err = tx.QueryRowContext(ctx, `SELECT pid FROM check_exec_claims WHERE operation_id = ?`, opID.String()).Scan(&existingPID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("sqlite: read check-exec claim of operation %s: %w", opID, err)
		case int(existingPID) == pid:
			return nil
		default:
			return fmt.Errorf("sqlite: operation %s already has a check-exec claim by pid %d; a claim by pid %d is refused", opID, existingPID, pid)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO check_exec_claims (operation_id, pid, claimed_at) VALUES (?, ?, ?)`,
			opID.String(), pid, formatTime(s.now()),
		); err != nil {
			return fmt.Errorf("sqlite: insert check-exec claim: %w", err)
		}
		return nil
	})
}

// RequestStop sets the run's monotonic stop request without a lease. A
// repeated request is idempotent; when the run's state permits it also
// moves to stopping, with transition evidence recorded generation NULL.
func (s *Store) RequestStop(ctx context.Context, runID identity.RunID) error {
	return s.inWriteTx(ctx, func(tx *sql.Tx) error {
		runV, revision, err := getRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if runV.StopRequested {
			return nil
		}
		now := s.now()
		next := runV.RequestStop(now)
		at := formatTime(now)
		result, err := tx.ExecContext(ctx,
			`UPDATE runs SET state = ?, stop_requested_at = ?, updated_at = ?, revision = revision + 1 WHERE id = ? AND revision = ?`,
			string(next.State), at, at, runID.String(), revision,
		)
		if err != nil {
			return fmt.Errorf("sqlite: record stop request: %w", err)
		}
		if err := requireCAS(result, fmt.Errorf("sqlite: run %s moved while requesting stop: %w", runID, app.ErrRevisionConflict)); err != nil {
			return err
		}
		if next.State != runV.State {
			return recordTransition(ctx, tx, &app.Transition{
				EntityKind: app.EntityRun,
				EntityID:   runID.String(),
				From:       string(runV.State),
				To:         string(next.State),
				Reason:     "stop requested",
				At:         now,
			})
		}
		return nil
	})
}
