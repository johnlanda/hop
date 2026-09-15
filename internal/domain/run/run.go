// Package run holds HOP's run domain module: the Run, Task, Attempt,
// Session, RuntimeBinding, Worktree, Result and Artifact entities, their
// pure state transitions and the result-acceptance rule of
// docs/plan/phase-2-design.md sections 2 and 5. Domain code never reads a
// clock, generates an identity or computes a digest: every transition takes
// its now and every identity/digest as an explicit input.
package run

import (
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// RunState is one state of the Run state machine (docs/plan/phase-2-design.md
// section 5, "Run"). reconciling is not a Run state: recovery is the
// resuming state with at least one reconciling operation in the
// application's operation journal.
type RunState string //nolint:revive // stutters deliberately: TaskState, AttemptState and SessionState name the same section 5 tables and keep the symmetry.

// Run states.
const (
	RunCreated    RunState = "created"
	RunLaunching  RunState = "launching"
	RunRunning    RunState = "running"
	RunCompleting RunState = "completing"
	RunCompleted  RunState = "completed"
	RunStopping   RunState = "stopping"
	RunStopped    RunState = "stopped"
	RunResuming   RunState = "resuming"
	RunFailed     RunState = "failed"
)

// runTransitions is the exhaustive Run state table of section 5. A (from,
// to) pair it does not list is invalid.
var runTransitions = newTransitionTable(concatPairs( //nolint:gochecknoglobals // runTransitions is the exhaustive, immutable Run state table of section 5; it never mutates after init.
	fromAny(RunLaunching, RunCreated, RunResuming),
	fromAny(RunRunning, RunLaunching, RunResuming, RunCompleting),
	fromAny(RunCompleting, RunRunning, RunResuming),
	fromAny(RunCompleted, RunCompleting),
	fromAny(RunStopping, RunCreated, RunLaunching, RunRunning, RunCompleting, RunResuming),
	fromAny(RunStopped, RunStopping),
	fromAny(RunFailed, RunLaunching, RunRunning, RunCompleting, RunResuming),
	fromAny(RunResuming, RunCreated, RunLaunching, RunRunning, RunCompleting, RunStopping),
))

// concatPairs flattens any number of pair slices into one, so a table can be
// assembled from several fromAny calls without an intermediate variable per
// call.
func concatPairs[S comparable](groups ...[][2]S) [][2]S {
	var all [][2]S
	for _, g := range groups {
		all = append(all, g...)
	}
	return all
}

// Run is one orchestration run: one task brief executed under an immutable
// effective configuration. Phase 2 gives a run exactly one task; feature
// mode (Phase 3) gains PlanClosed, the durable statement that the
// manager's plan is finished. Solo runs never close a plan and never
// consult the flag.
type Run struct {
	ID            identity.RunID
	RepositoryID  identity.RepositoryID
	Sequence      int
	BriefDigest   string
	State         RunState
	StopRequested bool
	PlanClosed    bool
	UpdatedAt     time.Time
}

// NewRun constructs a run in its initial created state. Sequence is the
// run's 1-based position within its repository, assigned by the
// application inside the same transaction that reserves it.
func NewRun(id identity.RunID, repository identity.RepositoryID, sequence int, briefDigest string, now time.Time) Run {
	return Run{
		ID:           id,
		RepositoryID: repository,
		Sequence:     sequence,
		BriefDigest:  briefDigest,
		State:        RunCreated,
		UpdatedAt:    now,
	}
}

// transition returns r with its state moved to to, or ErrInvalidTransition
// when the section 5 table does not list (r.State, to).
func (r Run) transition(to RunState, now time.Time) (Run, error) { //nolint:gocritic // hugeParam: Run is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	if !runTransitions.valid(r.State, to) {
		return r, fmt.Errorf("%w: run %s: %s to %s", ErrInvalidTransition, r.ID, r.State, to)
	}
	r.State = to
	r.UpdatedAt = now
	return r, nil
}

// Launch moves the run into launching: a launch intent has been journaled,
// either the run's first launch (from created) or a reconciliation outcome
// after resume authorized a cold relaunch (from resuming).
func (r Run) Launch(now time.Time) (Run, error) { return r.transition(RunLaunching, now) } //nolint:gocritic // hugeParam: Run is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// MarkRunning moves the run into running: its launch claim settled execed,
// resume reconciliation found it already running, or a repeatable check's
// unknown outcome requeued a new execution from completing.
func (r Run) MarkRunning(now time.Time) (Run, error) { return r.transition(RunRunning, now) } //nolint:gocritic // hugeParam: Run is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// EnterCompleting moves the run into completing: the accepted result's
// check has been claimed.
func (r Run) EnterCompleting(now time.Time) (Run, error) { return r.transition(RunCompleting, now) } //nolint:gocritic // hugeParam: Run is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// Complete moves the run into completed: a passing check receipt with no
// stop requested. Stop takes precedence over completion in every settling
// transition, so Complete refuses once StopRequested is set — the caller
// re-reads the stop request immediately before calling Complete, and a true
// value means the outcome is interrupted, never completed.
func (r Run) Complete(now time.Time) (Run, error) { //nolint:gocritic // hugeParam: Run is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	if r.StopRequested {
		return r, fmt.Errorf("%w: run %s: completion is superseded by a stop request", ErrInvalidTransition, r.ID)
	}
	return r.transition(RunCompleted, now)
}

// Fail moves the run into failed: a terminal failure (a rejected
// candidate, a failed check, an unresumable loss, or an unrepeatable
// unknown check outcome after its worker stopped).
func (r Run) Fail(now time.Time) (Run, error) { return r.transition(RunFailed, now) } //nolint:gocritic // hugeParam: Run is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// RequestStop records the run's stop request. The request is monotonic and
// can never be withdrawn, so a repeated or late request is idempotent. When
// the run is in a state the section 5 table lists as a valid source for
// stopping, the run also transitions to stopping; from any other state
// (including one already stopping or terminal) the request is recorded
// without changing the run's state.
func (r Run) RequestStop(now time.Time) Run { //nolint:gocritic // hugeParam: Run is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	r.StopRequested = true
	if runTransitions.valid(r.State, RunStopping) {
		r.State = RunStopping
		r.UpdatedAt = now
	}
	return r
}

// MarkStopped moves the run into stopped: termination of every piece of
// owned work — the worker and any running check — has been observed.
func (r Run) MarkStopped(now time.Time) (Run, error) { return r.transition(RunStopped, now) } //nolint:gocritic // hugeParam: Run is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// EnterResuming moves the run into resuming: hop resume acquired the
// controller lease and begins reconciliation.
func (r Run) EnterResuming(now time.Time) (Run, error) { return r.transition(RunResuming, now) } //nolint:gocritic // hugeParam: Run is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// CanAcceptManagerVerb reports whether r currently accepts a manager verb
// (CreateTask, RequestRetry, ClosePlan): only while running.
// ErrRunNotAccepting otherwise, the fate of a late request racing
// completion — validated inside the same accepting transaction so nothing
// can race it.
func (r Run) CanAcceptManagerVerb() error { //nolint:gocritic // hugeParam: Run is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	if r.State != RunRunning {
		return fmt.Errorf("%w: run %s: state %s", ErrRunNotAccepting, r.ID, r.State)
	}
	return nil
}

// ClosePlan sets the plan flag: the manager has finished submitting its
// plan. hasImplementTask must be true — a plan with zero implement tasks
// is ErrEmptyPlan, a usage error surfaced to the manager rather than a
// silently-vacuous readiness. Also validates CanAcceptManagerVerb.
func (r Run) ClosePlan(hasImplementTask bool, now time.Time) (Run, error) { //nolint:gocritic // hugeParam: Run is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	if err := r.CanAcceptManagerVerb(); err != nil {
		return r, err
	}
	if !hasImplementTask {
		return r, fmt.Errorf("%w: run %s", ErrEmptyPlan, r.ID)
	}
	r.PlanClosed = true
	r.UpdatedAt = now
	return r, nil
}

// ReopenPlan clears the plan flag: an accepted CreateTask reopens planning
// even after a prior close, inside the same transaction that accepts the
// new task.
func (r Run) ReopenPlan(now time.Time) Run { //nolint:gocritic // hugeParam: Run is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	r.PlanClosed = false
	r.UpdatedAt = now
	return r
}
