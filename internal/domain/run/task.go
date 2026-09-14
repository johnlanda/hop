package run

import (
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// TaskState is one state of the Task state machine (section 5, "Task").
type TaskState string

// Task states.
const (
	TaskPending     TaskState = "pending"
	TaskActive      TaskState = "active"
	TaskChecking    TaskState = "checking"
	TaskCompleted   TaskState = "completed"
	TaskFailed      TaskState = "failed"
	TaskInterrupted TaskState = "interrupted"
)

// taskTransitions is the exhaustive Task state table of section 5.
var taskTransitions = newTransitionTable(concatPairs( //nolint:gochecknoglobals // taskTransitions is the exhaustive, immutable Task state table of section 5; it never mutates after init.
	fromAny(TaskActive, TaskPending),
	fromAny(TaskChecking, TaskActive),
	fromAny(TaskCompleted, TaskChecking),
	fromAny(TaskFailed, TaskActive, TaskChecking),
	fromAny(TaskInterrupted, TaskPending, TaskActive, TaskChecking),
))

// Task is a scoped unit of work with acceptance criteria and dependencies.
// Phase 2 gives every run exactly one task.
type Task struct {
	ID                 identity.TaskID
	RunID              identity.RunID
	InstructionsDigest string
	State              TaskState
	UpdatedAt          time.Time
}

// NewTask constructs a task in its initial pending state. InstructionsDigest
// is an opaque digest of the task's frozen instructions, computed by the
// application.
func NewTask(id identity.TaskID, runID identity.RunID, instructionsDigest string, now time.Time) Task {
	return Task{
		ID:                 id,
		RunID:              runID,
		InstructionsDigest: instructionsDigest,
		State:              TaskPending,
		UpdatedAt:          now,
	}
}

// transition returns t with its state moved to to, or ErrInvalidTransition
// when the section 5 table does not list (t.State, to).
func (t Task) transition(to TaskState, now time.Time) (Task, error) { //nolint:gocritic // hugeParam: Task is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	if !taskTransitions.valid(t.State, to) {
		return t, fmt.Errorf("%w: task %s: %s to %s", ErrInvalidTransition, t.ID, t.State, to)
	}
	t.State = to
	t.UpdatedAt = now
	return t, nil
}

// Activate moves the task into active: its attempt has launched.
func (t Task) Activate(now time.Time) (Task, error) { return t.transition(TaskActive, now) } //nolint:gocritic // hugeParam: Task is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// EnterChecking moves the task into checking: a result has been accepted.
func (t Task) EnterChecking(now time.Time) (Task, error) { return t.transition(TaskChecking, now) } //nolint:gocritic // hugeParam: Task is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// Complete moves the task into completed: its check passed.
func (t Task) Complete(now time.Time) (Task, error) { return t.transition(TaskCompleted, now) } //nolint:gocritic // hugeParam: Task is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// Fail moves the task into failed: a rejected candidate or a failed check.
func (t Task) Fail(now time.Time) (Task, error) { return t.transition(TaskFailed, now) } //nolint:gocritic // hugeParam: Task is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// Interrupt moves the task into interrupted: a stop, whether before launch
// (from pending) or during execution (from active or checking).
func (t Task) Interrupt(now time.Time) (Task, error) { return t.transition(TaskInterrupted, now) } //nolint:gocritic // hugeParam: Task is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
