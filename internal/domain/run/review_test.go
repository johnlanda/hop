package run_test

import (
	"errors"
	"testing"

	"github.com/johnlanda/hop/internal/domain/run"
)

// baseReviewTask is a review task frozen against a fixed subject, in
// state, ready for AcceptVerdict's own transition to completed.
func baseReviewTask(state run.TaskState) run.Task {
	return run.Task{
		ID: testReviewTaskID, RunID: testRunID, Kind: run.TaskKindReview,
		SubjectCommitOID: "head-commit", SubjectTreeOID: "head-tree", State: state,
	}
}

func approvedSubmission() run.ReviewSubmission {
	return run.ReviewSubmission{ID: testReviewID, SubjectCommitOID: "head-commit", SubjectTreeOID: "head-tree", Verdict: run.VerdictApprove, ReasonsDigest: "reasons-v1"}
}

// TestAcceptVerdictOrdinaryAcceptance proves the atomic handoff: the
// attempt moves to completed through CompleteReview (never checking), and
// the review task to completed, for BOTH verdict values — the verdict's
// content gates readiness, not the task state.
func TestAcceptVerdictOrdinaryAcceptance(t *testing.T) {
	for _, verdict := range []run.Verdict{run.VerdictApprove, run.VerdictReject} {
		t.Run(string(verdict), func(t *testing.T) {
			r := baseRun(run.RunRunning, false)
			task := baseReviewTask(run.TaskActive)
			attempt := baseAttempt(run.AttemptRunning)
			submission := approvedSubmission()
			submission.Verdict = verdict
			ctx := run.ReviewAcceptanceContext{IncarnationCurrent: true, MailboxClear: true}

			outcome, err := run.AcceptVerdict(r, task, attempt, nil, ctx, submission, epoch())
			if err != nil {
				t.Fatalf("AcceptVerdict: unexpected error: %v", err)
			}
			if outcome.Attempt.State != run.AttemptCompleted {
				t.Fatalf("Attempt.State = %s, want completed", outcome.Attempt.State)
			}
			if outcome.Task.State != run.TaskCompleted {
				t.Fatalf("Task.State = %s, want completed", outcome.Task.State)
			}
			if outcome.Run.State != run.RunRunning {
				t.Fatalf("Run.State = %s, want unchanged running", outcome.Run.State)
			}
			if outcome.Review.Verdict != verdict || !outcome.Review.SubmittedAt.Equal(epoch()) {
				t.Fatalf("Review = %+v, want verdict %s at %v", outcome.Review, verdict, epoch())
			}
		})
	}
}

// TestAcceptVerdictEarlySubmission proves the early-submission handoff
// applies to reviewers too: a settled claim lets a launching or
// relaunching attempt accept a verdict, moving Run launching to running.
func TestAcceptVerdictEarlySubmission(t *testing.T) {
	for _, attemptState := range []run.AttemptState{run.AttemptLaunching, run.AttemptRelaunching} {
		t.Run(string(attemptState), func(t *testing.T) {
			r := baseRun(run.RunLaunching, false)
			task := baseReviewTask(run.TaskActive)
			attempt := baseAttempt(attemptState)
			ctx := run.ReviewAcceptanceContext{IncarnationCurrent: true, LaunchClaimSettled: true, MailboxClear: true}

			outcome, err := run.AcceptVerdict(r, task, attempt, nil, ctx, approvedSubmission(), epoch())
			if err != nil {
				t.Fatalf("AcceptVerdict: unexpected error: %v", err)
			}
			if outcome.Run.State != run.RunRunning {
				t.Fatalf("Run.State = %s, want running", outcome.Run.State)
			}
		})
	}
}

func TestAcceptVerdictTransient(t *testing.T) {
	r := baseRun(run.RunLaunching, false)
	task := baseReviewTask(run.TaskActive)
	attempt := baseAttempt(run.AttemptLaunching)
	ctx := run.ReviewAcceptanceContext{IncarnationCurrent: true, LaunchClaimSettled: false, MailboxClear: true}

	outcome, err := run.AcceptVerdict(r, task, attempt, nil, ctx, approvedSubmission(), epoch())

	if !errors.Is(err, run.ErrTransientNotRunning) {
		t.Fatalf("AcceptVerdict: error = %v, want ErrTransientNotRunning", err)
	}
	if outcome.Run != r || outcome.Task != task || outcome.Attempt != attempt {
		t.Fatalf("AcceptVerdict mutated its inputs on a transient outcome: %+v", outcome)
	}
}

func TestAcceptVerdictStaleByIncarnation(t *testing.T) {
	r := baseRun(run.RunRunning, false)
	task := baseReviewTask(run.TaskActive)
	attempt := baseAttempt(run.AttemptRunning)
	ctx := run.ReviewAcceptanceContext{IncarnationCurrent: false, MailboxClear: true}

	_, err := run.AcceptVerdict(r, task, attempt, nil, ctx, approvedSubmission(), epoch())
	if !errors.Is(err, run.ErrStaleSubmission) {
		t.Fatalf("AcceptVerdict: error = %v, want ErrStaleSubmission", err)
	}
}

func TestAcceptVerdictStaleByStopRequest(t *testing.T) {
	r := baseRun(run.RunStopping, true)
	task := baseReviewTask(run.TaskActive)
	attempt := baseAttempt(run.AttemptRunning)
	ctx := run.ReviewAcceptanceContext{IncarnationCurrent: true, MailboxClear: true}

	_, err := run.AcceptVerdict(r, task, attempt, nil, ctx, approvedSubmission(), epoch())
	if !errors.Is(err, run.ErrStaleSubmission) {
		t.Fatalf("AcceptVerdict: error = %v, want ErrStaleSubmission", err)
	}
}

func TestAcceptVerdictStaleByAttemptState(t *testing.T) {
	ineligible := []run.AttemptState{
		run.AttemptReserved, run.AttemptSubmitted, run.AttemptChecking,
		run.AttemptCompleted, run.AttemptFailed, run.AttemptInterrupted, run.AttemptReconciling,
	}
	for _, state := range ineligible {
		t.Run(string(state), func(t *testing.T) {
			r := baseRun(run.RunRunning, false)
			task := baseReviewTask(run.TaskActive)
			attempt := baseAttempt(state)
			ctx := run.ReviewAcceptanceContext{IncarnationCurrent: true, LaunchClaimSettled: true, MailboxClear: true}

			_, err := run.AcceptVerdict(r, task, attempt, nil, ctx, approvedSubmission(), epoch())
			if !errors.Is(err, run.ErrStaleSubmission) {
				t.Fatalf("AcceptVerdict from %s: error = %v, want ErrStaleSubmission", state, err)
			}
		})
	}
}

func TestAcceptVerdictMailboxNotClear(t *testing.T) {
	r := baseRun(run.RunRunning, false)
	task := baseReviewTask(run.TaskActive)
	attempt := baseAttempt(run.AttemptRunning)
	ctx := run.ReviewAcceptanceContext{IncarnationCurrent: true, MailboxClear: false}

	_, err := run.AcceptVerdict(r, task, attempt, nil, ctx, approvedSubmission(), epoch())
	if !errors.Is(err, run.ErrMailboxNotClear) {
		t.Fatalf("AcceptVerdict: error = %v, want ErrMailboxNotClear", err)
	}
}

func TestAcceptVerdictSubjectMismatch(t *testing.T) {
	r := baseRun(run.RunRunning, false)
	task := baseReviewTask(run.TaskActive)
	attempt := baseAttempt(run.AttemptRunning)
	ctx := run.ReviewAcceptanceContext{IncarnationCurrent: true, MailboxClear: true}
	mismatched := approvedSubmission()
	mismatched.SubjectCommitOID = "wrong-commit"

	_, err := run.AcceptVerdict(r, task, attempt, nil, ctx, mismatched, epoch())
	if !errors.Is(err, run.ErrVerdictSubjectMismatch) {
		t.Fatalf("AcceptVerdict: error = %v, want ErrVerdictSubjectMismatch", err)
	}
}

func TestAcceptVerdictDuplicateIsIdempotent(t *testing.T) {
	prior := run.Review{ID: testReviewID, AttemptID: testAttemptID, Verdict: run.VerdictApprove, ReasonsDigest: "reasons-v1"}
	r := baseRun(run.RunRunning, false)
	task := baseReviewTask(run.TaskCompleted)
	attempt := baseAttempt(run.AttemptCompleted)
	ctx := run.ReviewAcceptanceContext{IncarnationCurrent: true, MailboxClear: true}

	outcome, err := run.AcceptVerdict(r, task, attempt, &prior, ctx, approvedSubmission(), later())
	if !errors.Is(err, run.ErrDuplicateResult) {
		t.Fatalf("AcceptVerdict: error = %v, want ErrDuplicateResult", err)
	}
	if outcome.Review != prior || outcome.Run != r || outcome.Task != task || outcome.Attempt != attempt {
		t.Fatalf("AcceptVerdict mutated its inputs on a duplicate outcome: %+v", outcome)
	}
}

func TestAcceptVerdictConflictingNeverDisturbsAccepted(t *testing.T) {
	prior := run.Review{ID: testReviewID, AttemptID: testAttemptID, Verdict: run.VerdictReject, ReasonsDigest: "reasons-v1"}
	r := baseRun(run.RunRunning, false)
	task := baseReviewTask(run.TaskCompleted)
	attempt := baseAttempt(run.AttemptCompleted)
	ctx := run.ReviewAcceptanceContext{IncarnationCurrent: true, MailboxClear: true}

	outcome, err := run.AcceptVerdict(r, task, attempt, &prior, ctx, approvedSubmission(), later())
	if !errors.Is(err, run.ErrConflictingResult) {
		t.Fatalf("AcceptVerdict: error = %v, want ErrConflictingResult", err)
	}
	if outcome.Review != prior {
		t.Fatalf("AcceptVerdict: Review = %+v, want the unchanged, still-accepted prior %+v", outcome.Review, prior)
	}
}
