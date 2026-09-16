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

// Interface conformance for the section 8 worker-authority review port.
var _ app.ReviewStore = (*Store)(nil)

// reviewReceipt is one review_submissions row: claimed identities as plain
// text with no foreign keys, evidence for every outcome exactly as
// result_submissions records results.
type reviewReceipt struct {
	runID, taskID, attemptID, incarnationID string
	subjectCommitOID, verdict               string
	reasonsDigest                           string
	reviewID                                string
	outcome                                 app.ReviewOutcomeKind
	detail                                  string
}

// insertReviewReceipt records one review-submission receipt at time at.
func insertReviewReceipt(ctx context.Context, q querier, r *reviewReceipt, at time.Time) error {
	id, err := newUUID()
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx,
		`INSERT INTO review_submissions (id, claimed_run_id, claimed_task_id, claimed_attempt_id, claimed_incarnation_id, claimed_subject_commit_oid, claimed_verdict, reasons_digest, review_id, outcome, detail, submitted_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, truncateClaim(r.runID), truncateClaim(r.taskID), truncateClaim(r.attemptID), truncateClaim(r.incarnationID),
		nullString(truncateClaim(r.subjectCommitOID)), nullString(r.verdict), nullString(r.reasonsDigest),
		nullString(r.reviewID), string(r.outcome), truncateClaim(r.detail), formatTime(at),
	); err != nil {
		return fmt.Errorf("sqlite: record review receipt: %w", err)
	}
	return nil
}

// SubmitReview applies the section 8 verdict order atomically inside one
// worker-authority transaction, mirroring SubmitResult: existence and
// agreement first (malformed on disagreement), any prior accepted verdict
// resolved before eligibility (duplicate or conflicting, the accepted
// review never disturbed), the reviewer-session eligibility check, then
// the domain's AcceptVerdict against the caller's current incarnation, the
// launch-claim settlement fact and the review task's mailbox. Acceptance
// persists the review row, the attempt/task (and, for an early
// acceptance, run) transitions with generation-NULL evidence, CLOSES the
// review task's mailbox in the same commit, and commits the controller's
// info notice to the manager — its body the reasons artifact — atomically
// with the acceptance. Every outcome leaves a receipt and returns a nil
// error; a non-nil error is an infrastructure failure with nothing
// recorded.
func (s *Store) SubmitReview(ctx context.Context, submission app.ReviewSubmission) (app.ReviewOutcome, error) { //nolint:gocritic // hugeParam: the port passes the submission by value; the adapter mirrors its signature.
	var outcome app.ReviewOutcome
	err := s.inWriteTx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		record := func(kind app.ReviewOutcomeKind, reviewID identity.ReviewID, detail string) error {
			outcome = app.ReviewOutcome{Kind: kind, ReviewID: reviewID, Detail: detail}
			return insertReviewReceipt(ctx, tx, &reviewReceipt{
				runID: submission.RunID.String(), taskID: submission.TaskID.String(),
				attemptID: submission.AttemptID.String(), incarnationID: submission.IncarnationID.String(),
				subjectCommitOID: submission.SubjectCommitOID, verdict: string(submission.Verdict),
				reasonsDigest: submission.ReasonsDigest, reviewID: reviewID.String(),
				outcome: kind, detail: detail,
			}, now)
		}

		runV, runRevision, err := getRun(ctx, tx, submission.RunID)
		if errors.Is(err, app.ErrNotFound) {
			return record(app.ReviewMalformed, "", "attempt/task/run do not agree")
		}
		if err != nil {
			return err
		}
		task, taskRevision, err := getTask(ctx, tx, submission.TaskID)
		if errors.Is(err, app.ErrNotFound) {
			return record(app.ReviewMalformed, "", "attempt/task/run do not agree")
		}
		if err != nil {
			return err
		}
		attempt, attemptRevision, err := getAttempt(ctx, tx, submission.AttemptID)
		if errors.Is(err, app.ErrNotFound) {
			return record(app.ReviewMalformed, "", "attempt/task/run do not agree")
		}
		if err != nil {
			return err
		}
		if attempt.TaskID != submission.TaskID || task.RunID != submission.RunID {
			return record(app.ReviewMalformed, "", "attempt/task/run do not agree")
		}

		prior, err := reviewByAttempt(ctx, tx, submission.AttemptID)
		if err != nil {
			return err
		}
		// Receipt before eligibility: a prior accepted verdict resolves as
		// duplicate or conflicting whatever the caller's current
		// eligibility; only a FIRST acceptance validates the reviewer
		// session.
		if prior == nil {
			session, sessionErr := reviewerSessionEligible(ctx, tx, &submission, &task)
			if sessionErr != nil {
				return sessionErr
			}
			if session != "" {
				return record(app.ReviewStale, "", session)
			}
		}

		binding, hasBinding, err := currentBinding(ctx, tx, submission.Session)
		if err != nil {
			return err
		}
		incarnationIsCurrent := hasBinding && binding.IncarnationID == submission.IncarnationID && !binding.Superseded
		claim, err := getLaunchClaim(ctx, tx, submission.IncarnationID)
		if err != nil {
			return err
		}
		boxClear, err := mailboxClear(ctx, tx, submission.TaskID)
		if err != nil {
			return err
		}

		acceptCtx := run.ReviewAcceptanceContext{
			IncarnationCurrent: incarnationIsCurrent,
			LaunchClaimSettled: claim != nil && claim.State == app.LaunchClaimExeced,
			MailboxClear:       boxClear,
		}
		outcomeVal, err := run.AcceptVerdict(runV, task, attempt, prior, acceptCtx, run.ReviewSubmission{
			ID: submission.ID, SubjectCommitOID: submission.SubjectCommitOID, SubjectTreeOID: submission.SubjectTreeOID,
			Verdict: submission.Verdict, ReasonsDigest: submission.ReasonsDigest,
		}, now)
		switch {
		case err == nil:
		case errors.Is(err, run.ErrDuplicateResult):
			return record(app.ReviewDuplicate, outcomeVal.Review.ID, err.Error())
		case errors.Is(err, run.ErrConflictingResult):
			return record(app.ReviewConflicting, outcomeVal.Review.ID, err.Error())
		case errors.Is(err, run.ErrTransientNotRunning), errors.Is(err, run.ErrMailboxNotClear):
			return record(app.ReviewTransient, "", err.Error())
		default:
			return record(app.ReviewStale, "", err.Error())
		}

		if err := persistVerdictAcceptance(ctx, tx, &submission, &outcomeVal, verdictRevisions{
			run: runRevision, task: taskRevision, attempt: attemptRevision,
		}, verdictStates{run: runV.State, task: task.State, attempt: attempt.State}, now); err != nil {
			return err
		}
		return record(app.ReviewAccepted, submission.ID, "")
	})
	if err != nil {
		return app.ReviewOutcome{}, err
	}
	return outcome, nil
}

// reviewerSessionEligible reports the section 8 caller check for a first
// acceptance: the submitting session must be THE reviewer session of the
// claimed attempt — a reviewer-role session of the submission's own run,
// bound to exactly the review attempt being settled, of a review task. A
// live reviewer from another run, or one assigned to a different review
// attempt of this run, is refused before any of ITS binding or launch
// claim ever reaches the acceptance context (a foreign session's currency
// must never vouch for this attempt's verdict). It returns the refusal
// detail ("" when eligible).
func reviewerSessionEligible(ctx context.Context, q querier, submission *app.ReviewSubmission, task *run.Task) (string, error) {
	const refusal = "caller is not the review task's reviewer session"
	session, _, err := getSession(ctx, q, submission.Session)
	if errors.Is(err, app.ErrNotFound) {
		return refusal, nil
	}
	if err != nil {
		return "", err
	}
	if session.Role != run.RoleReviewer || task.Kind != run.TaskKindReview ||
		session.RunID != submission.RunID || session.AttemptID != submission.AttemptID {
		return refusal, nil
	}
	return "", nil
}

// verdictRevisions carries the revisions the acceptance read for its
// entity updates; verdictStates the pre-acceptance states for evidence.
type verdictRevisions struct{ run, task, attempt int64 }

type verdictStates struct {
	run     run.RunState
	task    run.TaskState
	attempt run.AttemptState
}

// persistVerdictAcceptance writes the atomic verdict handoff: the review
// row, the attempt and task updates (the task's mailbox closed in the
// same commit), the run update when acceptance moved it, one
// generation-NULL transition row per changed entity, and the controller's
// reasons-bearing info notice to the manager.
func persistVerdictAcceptance(ctx context.Context, tx *sql.Tx, submission *app.ReviewSubmission, outcomeVal *run.VerdictOutcome, revisions verdictRevisions, before verdictStates, now time.Time) error {
	review := outcomeVal.Review
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO reviews (id, run_id, task_id, attempt_id, subject_commit_oid, subject_tree_oid, verdict, reasons_path, reasons_digest, submitted_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		review.ID.String(), review.RunID.String(), review.TaskID.String(), review.AttemptID.String(),
		review.SubjectCommitOID, review.SubjectTreeOID, string(review.Verdict),
		submission.ReasonsPath, review.ReasonsDigest, formatTime(review.SubmittedAt),
	); err != nil {
		return fmt.Errorf("sqlite: insert accepted review: %w", err)
	}
	at := formatTime(now)
	if result, err := tx.ExecContext(ctx,
		`UPDATE attempts SET state = ?, updated_at = ?, revision = revision + 1 WHERE id = ? AND revision = ?`,
		string(outcomeVal.Attempt.State), at, outcomeVal.Attempt.ID.String(), revisions.attempt,
	); err != nil {
		return fmt.Errorf("sqlite: apply verdict attempt handoff: %w", err)
	} else if err := requireCAS(result, fmt.Errorf("sqlite: attempt %s moved during verdict acceptance: %w", outcomeVal.Attempt.ID, app.ErrRevisionConflict)); err != nil {
		return err
	}
	if result, err := tx.ExecContext(ctx,
		`UPDATE tasks SET state = ?, mailbox_closed_at = COALESCE(mailbox_closed_at, ?), updated_at = ?, revision = revision + 1 WHERE id = ? AND revision = ?`,
		string(outcomeVal.Task.State), at, at, outcomeVal.Task.ID.String(), revisions.task,
	); err != nil {
		return fmt.Errorf("sqlite: apply verdict task handoff: %w", err)
	} else if err := requireCAS(result, fmt.Errorf("sqlite: task %s moved during verdict acceptance: %w", outcomeVal.Task.ID, app.ErrRevisionConflict)); err != nil {
		return err
	}
	transitions := []app.Transition{
		{EntityKind: app.EntityAttempt, EntityID: outcomeVal.Attempt.ID.String(), From: string(before.attempt), To: string(outcomeVal.Attempt.State), Reason: "verdict accepted", At: now},
		{EntityKind: app.EntityTask, EntityID: outcomeVal.Task.ID.String(), From: string(before.task), To: string(outcomeVal.Task.State), Reason: "verdict accepted", At: now},
	}
	if outcomeVal.Run.State != before.run {
		if result, err := tx.ExecContext(ctx,
			`UPDATE runs SET state = ?, updated_at = ?, revision = revision + 1 WHERE id = ? AND revision = ?`,
			string(outcomeVal.Run.State), at, outcomeVal.Run.ID.String(), revisions.run,
		); err != nil {
			return fmt.Errorf("sqlite: apply verdict run handoff: %w", err)
		} else if err := requireCAS(result, fmt.Errorf("sqlite: run %s moved during verdict acceptance: %w", outcomeVal.Run.ID, app.ErrRevisionConflict)); err != nil {
			return err
		}
		transitions = append(transitions, app.Transition{
			EntityKind: app.EntityRun, EntityID: outcomeVal.Run.ID.String(),
			From: string(before.run), To: string(outcomeVal.Run.State),
			Reason: "verdict accepted", At: now,
		})
	}
	for i := range transitions {
		if err := recordTransition(ctx, tx, &transitions[i]); err != nil {
			return err
		}
	}

	// The controller info notice to the manager commits with the
	// acceptance (section 8): its body is the reasons artifact the manager
	// acts on.
	noticeID, err := newUUID()
	if err != nil {
		return err
	}
	parsedNoticeID, err := identity.ParseMessageID(noticeID)
	if err != nil {
		return fmt.Errorf("sqlite: minted notice id: %w", err)
	}
	seq, err := nextEnqueueSeq(ctx, tx, review.RunID, run.ManagerAddress())
	if err != nil {
		return err
	}
	notice := run.NewInfo(parsedNoticeID, review.RunID, run.ControllerPrincipal(), run.ManagerAddress(), "", submission.ReasonsPath, review.ReasonsDigest, 0, seq, now)
	return insertMessage(ctx, tx, &notice)
}
