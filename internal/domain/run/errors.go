package run

import "errors"

// ErrInvalidTransition reports that an entity cannot move from its current
// state through the requested transition. The section 5 state tables are
// exhaustive: a (from, to) pair the tables do not list is invalid.
var ErrInvalidTransition = errors.New("run: invalid transition")

// ErrStaleSubmission reports that a result submission targets an attempt
// that is no longer eligible to accept new content: its incarnation is not
// the session's current binding, the run is stopping or stopped, or the
// attempt is in a state that never accepts a first result.
var ErrStaleSubmission = errors.New("run: stale submission")

// ErrConflictingResult reports that a result submission's digest differs
// from the attempt's already-accepted result. The accepted result is never
// replaced; the conflicting submission is recorded and rejected.
var ErrConflictingResult = errors.New("run: conflicting result")

// ErrDuplicateResult distinguishes the idempotent case: a submission whose
// digest matches the attempt's already-accepted result. Callers treat this
// as success, not failure; it is returned as an error only so acceptance
// has one uniform (outcome, error) shape.
var ErrDuplicateResult = errors.New("run: duplicate result")

// ErrTransientNotRunning reports that a result submission arrived for an
// attempt that is launching or relaunching with a launch claim that has not
// yet settled: the submission may become valid once the claim settles, and
// the caller is expected to retry.
var ErrTransientNotRunning = errors.New("run: attempt not yet running")
