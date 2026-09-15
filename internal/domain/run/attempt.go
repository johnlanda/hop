package run

import (
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// AttemptState is one state of the Attempt state machine (section 5,
// "Attempt"). The same attempt is executed by a sequence of sessions: a
// cold relaunch (relaunching) binds a new session and incarnation to the
// same attempt rather than creating a new attempt, which Phase 2 reserves
// for a retry after a failed check (out of scope; Phase 3).
type AttemptState string

// Attempt states.
const (
	AttemptReserved    AttemptState = "reserved"
	AttemptLaunching   AttemptState = "launching"
	AttemptRunning     AttemptState = "running"
	AttemptSubmitted   AttemptState = "submitted"
	AttemptChecking    AttemptState = "checking"
	AttemptCompleted   AttemptState = "completed"
	AttemptFailed      AttemptState = "failed"
	AttemptInterrupted AttemptState = "interrupted"
	AttemptReconciling AttemptState = "reconciling"
	AttemptRelaunching AttemptState = "relaunching"
)

// reattachableAttemptStates are the only valid targets of Reattach: the
// states section 5 lists for reconciling → "returns to its prior state".
var reattachableAttemptStates = map[AttemptState]bool{ //nolint:gochecknoglobals // reattachableAttemptStates is the exhaustive, immutable set the section 5 table names for reconciling's reattach targets; it never mutates after init.
	AttemptRunning:   true,
	AttemptSubmitted: true,
	AttemptChecking:  true,
}

// attemptTransitions is the exhaustive Attempt state table of section 5,
// except for reconciling's three reattach targets, which Reattach validates
// on its own against reattachableAttemptStates: sharing this table with
// MarkRunning, Submit and EnterChecking would let those functions succeed
// from reconciling too, conflating "warm reattach verified" with their own,
// distinct causes.
var attemptTransitions = newTransitionTable(concatPairs( //nolint:gochecknoglobals // attemptTransitions is the exhaustive, immutable Attempt state table of section 5; it never mutates after init.
	fromAny(AttemptLaunching, AttemptReserved),
	fromAny(AttemptRunning, AttemptLaunching, AttemptRelaunching),
	fromAny(AttemptSubmitted, AttemptLaunching, AttemptRunning, AttemptRelaunching),
	fromAny(AttemptChecking, AttemptSubmitted),
	fromAny(AttemptCompleted, AttemptChecking),
	fromAny(AttemptFailed, AttemptLaunching, AttemptSubmitted, AttemptChecking, AttemptRelaunching),
	fromAny(AttemptInterrupted, AttemptReserved, AttemptLaunching, AttemptRunning, AttemptSubmitted, AttemptChecking, AttemptReconciling, AttemptRelaunching),
	fromAny(AttemptReconciling, AttemptLaunching, AttemptRunning, AttemptSubmitted, AttemptChecking, AttemptRelaunching),
	fromAny(AttemptRelaunching, AttemptReconciling),
))

// Attempt is one execution of a task; a retry creates a new attempt
// (Phase 3). Within Phase 2, one attempt may be executed by a sequence of
// sessions over time (the initial launch, then cold relaunches), with at
// most one non-terminated session and one current incarnation at any
// instant.
type Attempt struct {
	ID        identity.AttemptID
	TaskID    identity.TaskID
	Number    int
	State     AttemptState
	UpdatedAt time.Time
}

// NewAttempt constructs an attempt in its initial reserved state. Number is
// the attempt's 1-based position within its task; numbers are dense from 1,
// an invariant the application preserves by assigning consecutive numbers
// as it reserves attempts (the domain has no visibility into an attempt's
// siblings to verify density itself).
func NewAttempt(id identity.AttemptID, taskID identity.TaskID, number int, now time.Time) (Attempt, error) {
	if number < 1 {
		return Attempt{}, fmt.Errorf("%w: attempt %s: number %d is not dense from 1", ErrInvalidTransition, id, number)
	}
	return Attempt{ID: id, TaskID: taskID, Number: number, State: AttemptReserved, UpdatedAt: now}, nil
}

// terminalAttemptStates are the Attempt states a retry may follow: no
// verb ever revives a terminal attempt, so a retry always reserves a new
// one rather than reusing it.
var terminalAttemptStates = map[AttemptState]bool{ //nolint:gochecknoglobals // terminalAttemptStates is the exhaustive, immutable terminal subset of the Attempt state table; it never mutates after init.
	AttemptCompleted:   true,
	AttemptFailed:      true,
	AttemptInterrupted: true,
}

// NewRetryAttempt constructs attempt number prior.Number+1 for prior's
// task, reserving the retry a manager (implement) or the controller
// (review) requested. prior must have reached a terminal state
// (ErrRetryNotTerminal otherwise) and prior.Number must be below limit,
// the frozen per-task retry limit (ErrRetryLimit at or past it).
func NewRetryAttempt(id identity.AttemptID, prior Attempt, limit int, now time.Time) (Attempt, error) { //nolint:gocritic // hugeParam: Attempt is passed by value everywhere in this package; this constructor mirrors that convention.
	if !terminalAttemptStates[prior.State] {
		return Attempt{}, fmt.Errorf("%w: attempt %s: prior attempt %s is %s, not terminal", ErrRetryNotTerminal, id, prior.ID, prior.State)
	}
	if prior.Number >= limit {
		return Attempt{}, fmt.Errorf("%w: attempt %s: task %s has reached its retry limit %d", ErrRetryLimit, id, prior.TaskID, limit)
	}
	return NewAttempt(id, prior.TaskID, prior.Number+1, now)
}

// transition returns a with its state moved to to, or ErrInvalidTransition
// when the section 5 table does not list (a.State, to).
func (a Attempt) transition(to AttemptState, now time.Time) (Attempt, error) { //nolint:gocritic // hugeParam: Attempt is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	if !attemptTransitions.valid(a.State, to) {
		return a, fmt.Errorf("%w: attempt %s: %s to %s", ErrInvalidTransition, a.ID, a.State, to)
	}
	a.State = to
	a.UpdatedAt = now
	return a, nil
}

// Launch moves the attempt into launching: a launch intent has been
// recorded.
func (a Attempt) Launch(now time.Time) (Attempt, error) { return a.transition(AttemptLaunching, now) } //nolint:gocritic // hugeParam: Attempt is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// MarkRunning moves the attempt into running: its (or, after a cold
// relaunch, its new incarnation's) launch claim settled execed.
func (a Attempt) MarkRunning(now time.Time) (Attempt, error) { //nolint:gocritic // hugeParam: Attempt is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	return a.transition(AttemptRunning, now)
}

// Submit moves the attempt into submitted: a result was accepted, whether
// as an ordinary submission from running or an early submission accepted
// atomically alongside claim settlement from launching or relaunching.
func (a Attempt) Submit(now time.Time) (Attempt, error) { return a.transition(AttemptSubmitted, now) } //nolint:gocritic // hugeParam: Attempt is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// EnterChecking moves the attempt into checking: its check execution
// started.
func (a Attempt) EnterChecking(now time.Time) (Attempt, error) { //nolint:gocritic // hugeParam: Attempt is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	return a.transition(AttemptChecking, now)
}

// Complete moves the attempt into completed: its check passed.
func (a Attempt) Complete(now time.Time) (Attempt, error) { return a.transition(AttemptCompleted, now) } //nolint:gocritic // hugeParam: Attempt is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// Fail moves the attempt into failed: a conflicting or invalid candidate, a
// failed check, or an unrepeatable unknown check outcome left unresolved.
func (a Attempt) Fail(now time.Time) (Attempt, error) { return a.transition(AttemptFailed, now) } //nolint:gocritic // hugeParam: Attempt is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// Interrupt moves the attempt into interrupted: a stop, or an established
// worker loss with no resume path, from any non-terminal state the section
// 5 table allows.
func (a Attempt) Interrupt(now time.Time) (Attempt, error) { //nolint:gocritic // hugeParam: Attempt is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	return a.transition(AttemptInterrupted, now)
}

// Reconcile moves the attempt into reconciling: a takeover or an
// observation loss leaves its identity not yet established.
func (a Attempt) Reconcile(now time.Time) (Attempt, error) { //nolint:gocritic // hugeParam: Attempt is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	return a.transition(AttemptReconciling, now)
}

// Relaunch moves the attempt into relaunching: a cold relaunch was
// authorized (absence conclusively established, with supported resume
// semantics), binding a new session and incarnation to this same attempt.
func (a Attempt) Relaunch(now time.Time) (Attempt, error) { //nolint:gocritic // hugeParam: Attempt is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	return a.transition(AttemptRelaunching, now)
}

// CompleteReview moves a submitted review attempt into completed: the
// accepted verdict, applied atomically inside the verdict-acceptance
// transaction. This is a distinct, independently validated transition (the
// Reattach precedent) rather than a share of Complete's checking->completed
// row: a review attempt's settling evidence is the verdict row itself,
// there is no check phase, and reusing the implement path would make
// "check receipt" mean something other than a deterministic execution.
// kind must be TaskKindReview; any other kind, or any source state other
// than submitted, is ErrInvalidTransition.
func (a Attempt) CompleteReview(kind TaskKind, now time.Time) (Attempt, error) { //nolint:gocritic // hugeParam: Attempt is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	if kind != TaskKindReview || a.State != AttemptSubmitted {
		return a, fmt.Errorf("%w: attempt %s: %s to %s is not a valid review completion for kind %q", ErrInvalidTransition, a.ID, a.State, AttemptCompleted, kind)
	}
	a.State = AttemptCompleted
	a.UpdatedAt = now
	return a, nil
}

// Reattach moves a reconciling attempt back to to, its prior state, once
// warm reattach verifies the occupant against the current binding's claim
// evidence. Only a reconciling attempt reattaches, and only to running,
// submitted or checking — the states section 5 lists as valid reattach
// targets; any other source state or target is ErrInvalidTransition. This
// check does not share attemptTransitions with MarkRunning, Submit and
// EnterChecking, so reattachment stays reconciling's own, distinct cause.
func (a Attempt) Reattach(to AttemptState, now time.Time) (Attempt, error) { //nolint:gocritic // hugeParam: Attempt is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	if a.State != AttemptReconciling || !reattachableAttemptStates[to] {
		return a, fmt.Errorf("%w: attempt %s: %s to %s is not a valid reattach", ErrInvalidTransition, a.ID, a.State, to)
	}
	a.State = to
	a.UpdatedAt = now
	return a, nil
}
