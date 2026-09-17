package run

import (
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// Result is a submitted outcome for a particular task attempt. An attempt
// has at most one accepted result; an accepted receipt is immutable and is
// never replaced.
type Result struct {
	ID            identity.ResultID
	AttemptID     identity.AttemptID
	CommitOID     string
	Summary       string
	ContentDigest string
	Accepted      bool
	SubmittedAt   time.Time
}

// ResultSubmission is the application-validated content of one result
// submission: a pre-generated result identity, the candidate commit, its
// summary and its canonicalized content digest. The domain treats Digest as
// an opaque, already-validated string; it never computes or checks a hash
// (hashing is the application's job, docs/plan/phase-2-design.md section 7).
type ResultSubmission struct {
	ID        identity.ResultID
	CommitOID string
	Summary   string
	Digest    string
}

// AcceptanceContext is the complete, application-assembled context a result
// acceptance decision needs beyond the current Run, Task, Attempt and any
// prior accepted result: whether the submission's incarnation is the
// session's current, non-superseded binding; whether that incarnation's
// launch claim has settled to execed (meaningful only while the attempt is
// launching or relaunching, the early-submission case); and whether the
// task's mailbox is CLEAR — no queued or delivered-unacknowledged message
// addresses it (section 5's drain-then-submit contract; a solo task, which
// no message ever addresses, is clear). Every field's zero value refuses,
// so a caller that forgets one fails closed.
type AcceptanceContext struct {
	IncarnationCurrent bool
	LaunchClaimSettled bool
	MailboxClear       bool
}

// AcceptanceOutcome is the state AcceptResult decided: the result (existing,
// for a duplicate; newly accepted, otherwise) and the Run, Task and Attempt
// values after any transitions acceptance implies. On every error outcome,
// Run, Task and Attempt are returned unchanged and Result is the prior
// accepted result when one exists, the zero Result otherwise.
type AcceptanceOutcome struct {
	Result  Result
	Run     Run
	Task    Task
	Attempt Attempt
}

// AcceptResult decides the outcome of one result submission, in the
// validation order of section 7: resolve any prior accepted result before
// any state or incarnation precondition (an equal digest is an idempotent
// ErrDuplicateResult in every attempt state, including terminal ones; an
// unequal digest is ErrConflictingResult and never disturbs the accepted
// result); then, only for a first acceptance, eligibility — the
// submission's incarnation must be current (else ErrStaleSubmission), the
// run must have no stop request (else ErrStaleSubmission), the attempt
// must be running, or launching/relaunching with a settled launch claim for
// that incarnation (the early-submission case; an unsettled claim there is
// ErrTransientNotRunning) — any other attempt state is ErrStaleSubmission —
// and only then the task's mailbox must be clear (else ErrMailboxNotClear,
// the retryable "drain and resubmit" outcome). This is AcceptVerdict's
// order: a caller that can never be accepted is told so before it is told
// to drain, and a caller told to drain is one whose fetch the store serves.
// On acceptance, Attempt moves to submitted, Task to checking, and — when
// the run has not yet reached it — Run from launching to running, all as
// one atomic handoff.
func AcceptResult(run Run, task Task, attempt Attempt, prior *Result, ctx AcceptanceContext, submission ResultSubmission, now time.Time) (AcceptanceOutcome, error) { //nolint:gocritic // hugeParam: Run/Task/Attempt is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	unchanged := AcceptanceOutcome{Run: run, Task: task, Attempt: attempt}
	if prior != nil {
		unchanged.Result = *prior
		if prior.ContentDigest == submission.Digest {
			return unchanged, fmt.Errorf("%w: attempt %s: result %s", ErrDuplicateResult, attempt.ID, prior.ID)
		}
		return unchanged, fmt.Errorf("%w: attempt %s: result %s", ErrConflictingResult, attempt.ID, prior.ID)
	}

	if !ctx.IncarnationCurrent {
		return unchanged, fmt.Errorf("%w: attempt %s: incarnation is not current", ErrStaleSubmission, attempt.ID)
	}
	if run.StopRequested {
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
		return unchanged, fmt.Errorf("%w: attempt %s: state %s does not accept a result", ErrStaleSubmission, attempt.ID, attempt.State)
	}

	if !ctx.MailboxClear {
		return unchanged, fmt.Errorf("%w: task %s", ErrMailboxNotClear, task.ID)
	}

	nextAttempt, err := attempt.Submit(now)
	if err != nil {
		return unchanged, err
	}
	nextTask, err := task.EnterChecking(now)
	if err != nil {
		return unchanged, err
	}
	nextRun := run
	if run.State == RunLaunching {
		if nextRun, err = run.MarkRunning(now); err != nil {
			return unchanged, err
		}
	}

	result := Result{
		ID:            submission.ID,
		AttemptID:     attempt.ID,
		CommitOID:     submission.CommitOID,
		Summary:       submission.Summary,
		ContentDigest: submission.Digest,
		Accepted:      true,
		SubmittedAt:   now,
	}
	return AcceptanceOutcome{Result: result, Run: nextRun, Task: nextTask, Attempt: nextAttempt}, nil
}
