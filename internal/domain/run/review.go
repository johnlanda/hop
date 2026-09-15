package run

import (
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// Verdict is a reviewer's judgment on a review task's frozen subject.
type Verdict string

// Verdict values.
const (
	VerdictApprove Verdict = "approve"
	VerdictReject  Verdict = "reject"
)

// Review is an independent reviewer's immutable verdict on one review
// task's attempt. At most one accepted verdict exists per attempt; a
// verdict never mutates — a new candidate gets a new review task and a
// new attempt.
type Review struct {
	ID               identity.ReviewID
	RunID            identity.RunID
	TaskID           identity.TaskID
	AttemptID        identity.AttemptID
	SubjectCommitOID string
	SubjectTreeOID   string
	Verdict          Verdict
	ReasonsDigest    string
	SubmittedAt      time.Time
}

// ReviewSubmission is the application-validated content of one review
// submission: a pre-generated review identity, the submitted subject (the
// commit the reviewer reviewed; the application resolves its tree object
// ID against the recorded repository before the accepting transaction),
// the verdict token and the canonicalized reasons digest.
type ReviewSubmission struct {
	ID               identity.ReviewID
	SubjectCommitOID string
	SubjectTreeOID   string
	Verdict          Verdict
	ReasonsDigest    string
}

// ReviewAcceptanceContext is the complete, application-assembled context
// AcceptVerdict needs beyond the current Run, Task, Attempt and any prior
// accepted review: whether the submission's incarnation is the session's
// current, non-superseded binding; whether that incarnation's launch
// claim has settled (meaningful only while the attempt is launching or
// relaunching, the early-submission case); and whether the review task's
// mailbox is CLEAR — no queued or delivered-unacknowledged message
// addresses it (section 5's drain-then-submit contract).
type ReviewAcceptanceContext struct {
	IncarnationCurrent bool
	LaunchClaimSettled bool
	MailboxClear       bool
}

// VerdictOutcome is the state AcceptVerdict decided: the review (existing,
// for a duplicate/conflict; newly accepted, otherwise) and the Run, Task
// and Attempt values after any transitions acceptance implies. On every
// error outcome, Run, Task and Attempt are returned unchanged and Review
// is the prior accepted review when one exists, the zero Review
// otherwise.
type VerdictOutcome struct {
	Review  Review
	Run     Run
	Task    Task
	Attempt Attempt
}

// AcceptVerdict decides the outcome of one review submission, mirroring
// AcceptResult's receipt-before-eligibility order (section 8): resolve any
// prior accepted review for the attempt before any other check (equal
// verdict and reasons digest is the idempotent ErrDuplicateResult; any
// other prior review is ErrConflictingResult, and the accepted review is
// never disturbed); then eligibility — the submission's incarnation must
// be current (else ErrStaleSubmission), the run must have no stop request
// (else ErrStaleSubmission), the attempt must be running, or
// launching/relaunching with a settled launch claim for that incarnation
// (the early-submission case; an unsettled claim there is
// ErrTransientNotRunning) — any other attempt state is ErrStaleSubmission
// — the review task's mailbox must be clear (else ErrMailboxNotClear, the
// retryable "drain and resubmit" outcome), and the submitted subject must
// equal reviewTask's frozen subject (else ErrVerdictSubjectMismatch: the
// reviewer reviewed the wrong candidate). On acceptance: Attempt.Submit
// then the review-only Attempt.CompleteReview, Task.Complete (approve OR
// reject both complete the review task — the verdict's content gates run
// readiness, not the task state), and — when the run has not yet reached
// it — Run from launching to running, all atomically.
func AcceptVerdict(r Run, reviewTask Task, attempt Attempt, prior *Review, ctx ReviewAcceptanceContext, submission ReviewSubmission, now time.Time) (VerdictOutcome, error) { //nolint:gocritic // hugeParam: Run/Task/Attempt is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	unchanged := VerdictOutcome{Run: r, Task: reviewTask, Attempt: attempt}
	if prior != nil {
		unchanged.Review = *prior
		if prior.Verdict == submission.Verdict && prior.ReasonsDigest == submission.ReasonsDigest {
			return unchanged, fmt.Errorf("%w: attempt %s: review %s", ErrDuplicateResult, attempt.ID, prior.ID)
		}
		return unchanged, fmt.Errorf("%w: attempt %s: review %s", ErrConflictingResult, attempt.ID, prior.ID)
	}

	if !ctx.IncarnationCurrent {
		return unchanged, fmt.Errorf("%w: attempt %s: incarnation is not current", ErrStaleSubmission, attempt.ID)
	}
	if r.StopRequested {
		return unchanged, fmt.Errorf("%w: attempt %s: run has a stop request", ErrStaleSubmission, attempt.ID)
	}

	switch attempt.State {
	case AttemptRunning:
		// Eligible: the ordinary case.
	case AttemptLaunching, AttemptRelaunching:
		if !ctx.LaunchClaimSettled {
			return unchanged, fmt.Errorf("%w: attempt %s", ErrTransientNotRunning, attempt.ID)
		}
	default:
		return unchanged, fmt.Errorf("%w: attempt %s: state %s does not accept a verdict", ErrStaleSubmission, attempt.ID, attempt.State)
	}

	if !ctx.MailboxClear {
		return unchanged, fmt.Errorf("%w: task %s", ErrMailboxNotClear, reviewTask.ID)
	}
	if reviewTask.SubjectCommitOID != submission.SubjectCommitOID || reviewTask.SubjectTreeOID != submission.SubjectTreeOID {
		return unchanged, fmt.Errorf("%w: task %s", ErrVerdictSubjectMismatch, reviewTask.ID)
	}

	nextAttempt, err := attempt.Submit(now)
	if err != nil {
		return unchanged, err
	}
	nextAttempt, err = nextAttempt.CompleteReview(reviewTask.Kind, now)
	if err != nil {
		return unchanged, err
	}
	nextTask, err := reviewTask.Complete(now)
	if err != nil {
		return unchanged, err
	}
	nextRun := r
	if r.State == RunLaunching {
		if nextRun, err = r.MarkRunning(now); err != nil {
			return unchanged, err
		}
	}

	review := Review{
		ID:               submission.ID,
		RunID:            r.ID,
		TaskID:           reviewTask.ID,
		AttemptID:        attempt.ID,
		SubjectCommitOID: submission.SubjectCommitOID,
		SubjectTreeOID:   submission.SubjectTreeOID,
		Verdict:          submission.Verdict,
		ReasonsDigest:    submission.ReasonsDigest,
		SubmittedAt:      now,
	}
	return VerdictOutcome{Review: review, Run: nextRun, Task: nextTask, Attempt: nextAttempt}, nil
}
