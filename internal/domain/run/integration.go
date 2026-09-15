package run

import (
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// IntegrationState is one state of the Integration state machine (section
// 5, "Integration").
type IntegrationState string

// Integration states.
const (
	IntegrationMerging     IntegrationState = "merging"
	IntegrationChecking    IntegrationState = "checking"
	IntegrationIntegrated  IntegrationState = "integrated"
	IntegrationConflicted  IntegrationState = "conflicted"
	IntegrationCheckFailed IntegrationState = "check-failed"
	IntegrationRolledBack  IntegrationState = "rolled-back"
	IntegrationInterrupted IntegrationState = "interrupted"
)

// integrationTransitions is the exhaustive Integration state table of
// section 5. checking->rolled-back and check-failed->rolled-back are both
// listed rows with distinct causes (a published-but-unsettled candidate
// stopped directly, versus a pending reset completed under stop) that
// share this one target and are not otherwise distinguished by the state
// machine; the causal detail belongs to the application's operation
// journal, matching the Run/Task/Attempt/Session convention elsewhere in
// this package.
var integrationTransitions = newTransitionTable(concatPairs( //nolint:gochecknoglobals // integrationTransitions is the exhaustive, immutable Integration state table of section 5; it never mutates after init.
	fromAny(IntegrationChecking, IntegrationMerging),
	fromAny(IntegrationConflicted, IntegrationMerging),
	fromAny(IntegrationIntegrated, IntegrationChecking),
	fromAny(IntegrationCheckFailed, IntegrationChecking),
	fromAny(IntegrationRolledBack, IntegrationChecking, IntegrationCheckFailed),
	fromAny(IntegrationInterrupted, IntegrationMerging),
))

// Integration is one serial attempt to merge a task's accepted result into
// the run's integration branch and validate the combined candidate. One
// integration exists per (task, attempt-that-produced-the-result); at most
// one non-terminal integration exists per run (application/store
// enforced). Every state change carries recorded object IDs, never branch
// names alone.
type Integration struct {
	ID              identity.IntegrationID
	RunID           identity.RunID
	TaskID          identity.TaskID
	ResultID        identity.ResultID
	SourceCommitOID string
	PremergeHeadOID string
	MergeCommitOID  string
	State           IntegrationState
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// NewIntegration constructs an integration in its initial merging state:
// the source commit (the task's accepted result) will be merged onto
// premergeHeadOID, the integration branch's head at claim time.
func NewIntegration(id identity.IntegrationID, runID identity.RunID, taskID identity.TaskID, resultID identity.ResultID, sourceCommitOID, premergeHeadOID string, now time.Time) Integration {
	return Integration{
		ID: id, RunID: runID, TaskID: taskID, ResultID: resultID,
		SourceCommitOID: sourceCommitOID, PremergeHeadOID: premergeHeadOID,
		State: IntegrationMerging, CreatedAt: now, UpdatedAt: now,
	}
}

// transition returns i with its state moved to to, or ErrInvalidTransition
// when the section 5 table does not list (i.State, to).
func (i Integration) transition(to IntegrationState, now time.Time) (Integration, error) { //nolint:gocritic // hugeParam: Integration is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	if !integrationTransitions.valid(i.State, to) {
		return i, fmt.Errorf("%w: integration %s: %s to %s", ErrInvalidTransition, i.ID, i.State, to)
	}
	i.State = to
	i.UpdatedAt = now
	return i, nil
}

// EnterChecking moves the integration into checking: a merge commit M was
// produced in the scratch tree and published to the integration ref by
// compare-and-swap, or the ancestor/no-op outcome (the source commit is
// already an ancestor of, or equal to, the pre-merge head; no publish
// runs, and mergeCommitOID is the unchanged head — the existing head
// object ID, not a fresh commit). Either way mergeCommitOID is the
// candidate the combined check now targets.
func (i Integration) EnterChecking(mergeCommitOID string, now time.Time) (Integration, error) { //nolint:gocritic // hugeParam: Integration is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	next, err := i.transition(IntegrationChecking, now)
	if err != nil {
		return i, err
	}
	next.MergeCommitOID = mergeCommitOID
	return next, nil
}

// Conflict moves the integration into conflicted: a merge conflict was
// observed (non-zero exit); the conflicted scratch tree is retained as
// evidence until settlement.
func (i Integration) Conflict(now time.Time) (Integration, error) { //nolint:gocritic // hugeParam: Integration is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	return i.transition(IntegrationConflicted, now)
}

// Integrate moves the integration into integrated: a passing combined-
// candidate check receipt for MergeCommitOID.
func (i Integration) Integrate(now time.Time) (Integration, error) { //nolint:gocritic // hugeParam: Integration is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	return i.transition(IntegrationIntegrated, now)
}

// FailCheck moves the integration into check-failed: a failing combined
// check; triggers the integration.reset operation.
func (i Integration) FailCheck(now time.Time) (Integration, error) { //nolint:gocritic // hugeParam: Integration is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	return i.transition(IntegrationCheckFailed, now)
}

// RollBack moves the integration into rolled-back: the reset outcome
// observed (from check-failed), or a published-but-unsettled candidate
// retired directly under stop precedence (from checking) — the ref moved
// to the fresh rollback commit R, which carries the pre-merge content and
// keeps the rejected merge commit reachable as its parent.
func (i Integration) RollBack(now time.Time) (Integration, error) { //nolint:gocritic // hugeParam: Integration is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	return i.transition(IntegrationRolledBack, now)
}

// Interrupt moves the integration into interrupted: stop before a
// candidate was published — the merge's claimed group is retired under
// the group rule, and any unresolved publish intent is retired by the
// ref-fencing rule.
func (i Integration) Interrupt(now time.Time) (Integration, error) { //nolint:gocritic // hugeParam: Integration is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	return i.transition(IntegrationInterrupted, now)
}
