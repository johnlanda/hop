package run

import (
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// TaskState is one state of the Task state machine (section 5, "Task
// (kind = implement)" and "Task (kind = review)"). The two kinds share one
// state set but different valid transitions; Task.transition dispatches on
// Kind to pick the table that applies.
type TaskState string

// Task states.
const (
	TaskPending     TaskState = "pending"
	TaskReady       TaskState = "ready"
	TaskActive      TaskState = "active"
	TaskChecking    TaskState = "checking"
	TaskCompleted   TaskState = "completed"
	TaskIntegrating TaskState = "integrating"
	TaskIntegrated  TaskState = "integrated"
	TaskNeedsRework TaskState = "needs-rework"
	TaskFailed      TaskState = "failed"
	TaskInterrupted TaskState = "interrupted"
)

// TaskKind distinguishes a task's role in the workflow. The zero value
// behaves as TaskKindImplement: every Phase 2 task predates this field and
// is never explicitly kind=implement, so the implement table is the
// default table, not a table conditioned on a non-empty kind string.
type TaskKind string

// Task kinds.
const (
	TaskKindImplement TaskKind = "implement"
	TaskKindReview    TaskKind = "review"
)

// taskTransitions is the exhaustive Task (kind = implement) state table of
// section 5, EXCEPT the two rows whose legality depends on a fact beyond
// the bare (from, to) pair: pending->active (legal only for a task with no
// dependencies; Activate validates it directly) and pending->ready (legal
// only once every prerequisite is integrated; Release validates it
// directly against an application-assembled eligibility fact). Both are
// omitted here so an unlisted pair stays exhaustively invalid rather than
// silently bypassing those checks through the generic table.
var taskTransitions = newTransitionTable(concatPairs( //nolint:gochecknoglobals // taskTransitions is the exhaustive, immutable Task (kind = implement) state table of section 5 (less the two context-dependent rows above); it never mutates after init.
	fromAny(TaskActive, TaskReady),
	fromAny(TaskChecking, TaskActive),
	fromAny(TaskCompleted, TaskChecking),
	fromAny(TaskIntegrating, TaskCompleted),
	fromAny(TaskIntegrated, TaskIntegrating),
	fromAny(TaskNeedsRework, TaskIntegrating, TaskActive, TaskChecking),
	fromAny(TaskReady, TaskNeedsRework),
	fromAny(TaskFailed, TaskActive, TaskChecking, TaskIntegrating, TaskNeedsRework),
	fromAny(TaskInterrupted, TaskPending, TaskReady, TaskActive, TaskChecking, TaskCompleted, TaskIntegrating, TaskNeedsRework),
))

// reviewTaskTransitions is the exhaustive Task (kind = review) state table
// of section 5. A review task is always created ready (it has no
// dependencies), so pending never appears here.
var reviewTaskTransitions = newTransitionTable(concatPairs( //nolint:gochecknoglobals // reviewTaskTransitions is the exhaustive, immutable Task (kind = review) state table of section 5; it never mutates after init.
	fromAny(TaskActive, TaskReady),
	fromAny(TaskCompleted, TaskActive),
	fromAny(TaskNeedsRework, TaskActive),
	fromAny(TaskReady, TaskNeedsRework),
	fromAny(TaskFailed, TaskActive, TaskNeedsRework),
	fromAny(TaskInterrupted, TaskReady, TaskActive, TaskNeedsRework),
))

// Task is a scoped unit of work with acceptance criteria and dependencies.
// Kind, Seq, Title and HasDependencies are fixed at creation.
// HasDependencies is a plain, comparable flag Activate dispatches on
// (zero-dependency tasks activate directly from pending); it is NOT the
// ground truth Release verifies against — that is the task's persisted
// TaskDependency edges, supplied explicitly to Release/ReleaseEligible
// (never stored on Task itself: a []identity.TaskID field would make Task
// incomparable, breaking every Phase 2 test's `==`/`!=` use of it).
// SubjectCommitOID and SubjectTreeOID are set only for a review task,
// frozen to the integration head it was created against.
type Task struct {
	ID                 identity.TaskID
	RunID              identity.RunID
	Kind               TaskKind
	Seq                int
	Title              string
	InstructionsDigest string
	HasDependencies    bool
	SubjectCommitOID   string
	SubjectTreeOID     string
	MailboxClosed      bool
	State              TaskState
	UpdatedAt          time.Time
}

// NewTask constructs a task in its initial pending state. InstructionsDigest
// is an opaque digest of the task's frozen instructions, computed by the
// application. This is the Phase 2 constructor: it never sets Kind, Seq,
// Title or HasDependencies, so the task it returns activates directly from
// pending (the zero-dependency path Activate grants every Kind=="" task).
func NewTask(id identity.TaskID, runID identity.RunID, instructionsDigest string, now time.Time) Task {
	return Task{
		ID:                 id,
		RunID:              runID,
		InstructionsDigest: instructionsDigest,
		State:              TaskPending,
		UpdatedAt:          now,
	}
}

// NewImplementTask constructs an implement task: ready immediately when
// hasDependencies is false (a task with no prerequisites is created
// ready), pending otherwise — Release then gates its move to ready on the
// task's actual persisted TaskDependency edges (see Release), never on
// this flag.
func NewImplementTask(id identity.TaskID, runID identity.RunID, seq int, title, instructionsDigest string, hasDependencies bool, now time.Time) Task {
	state := TaskPending
	if !hasDependencies {
		state = TaskReady
	}
	return Task{
		ID:                 id,
		RunID:              runID,
		Kind:               TaskKindImplement,
		Seq:                seq,
		Title:              title,
		InstructionsDigest: instructionsDigest,
		HasDependencies:    hasDependencies,
		State:              state,
		UpdatedAt:          now,
	}
}

// NewReviewTask constructs a review task in its initial ready state, with
// its subject (the integration head it reviews) frozen at creation. A
// review task has no dependencies and never enters integrating/integrated.
func NewReviewTask(id identity.TaskID, runID identity.RunID, seq int, subjectCommitOID, subjectTreeOID string, now time.Time) Task {
	return Task{
		ID:               id,
		RunID:            runID,
		Kind:             TaskKindReview,
		Seq:              seq,
		SubjectCommitOID: subjectCommitOID,
		SubjectTreeOID:   subjectTreeOID,
		State:            TaskReady,
		UpdatedAt:        now,
	}
}

// table returns t's applicable transition table: reviewTaskTransitions for
// Kind == TaskKindReview, taskTransitions (the implement table) for every
// other Kind including the Phase 2 zero value.
func (t Task) table() transitionTable[TaskState] { //nolint:gocritic // hugeParam: Task is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	if t.Kind == TaskKindReview {
		return reviewTaskTransitions
	}
	return taskTransitions
}

// transition returns t with its state moved to to, or ErrInvalidTransition
// when t's applicable table does not list (t.State, to).
func (t Task) transition(to TaskState, now time.Time) (Task, error) { //nolint:gocritic // hugeParam: Task is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	if !t.table().valid(t.State, to) {
		return t, fmt.Errorf("%w: task %s: %s to %s", ErrInvalidTransition, t.ID, t.State, to)
	}
	t.State = to
	t.UpdatedAt = now
	return t, nil
}

// Activate moves the task into active: from ready (the assignment
// transaction, after any required release), or, for a task with no
// dependencies, directly from pending — the Phase 2/solo legacy path. A
// task WITH dependencies attempted directly from pending is refused
// (ErrTaskNotReleased): it must Release to ready first.
func (t Task) Activate(now time.Time) (Task, error) { //nolint:gocritic // hugeParam: Task is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	if t.State == TaskPending && t.Kind != TaskKindReview {
		if t.HasDependencies {
			return t, fmt.Errorf("%w: task %s", ErrTaskNotReleased, t.ID)
		}
		t.State = TaskActive
		t.UpdatedAt = now
		return t, nil
	}
	return t.transition(TaskActive, now)
}

// Release moves a dependent task from pending to ready. edges must be
// EXACTLY t's own persisted TaskDependency set (every row naming t.ID as
// the dependent task; a caller-supplied edge naming a different task is
// rejected, never silently ignored) and prerequisites the current state
// of every task edges names. Release computes eligibility itself via
// ReleaseEligible — it never accepts a bare caller-asserted boolean, which
// is exactly what would let an incomplete or empty evidence set slip
// through unnoticed. Checked in order: any source state other than
// pending is ErrInvalidTransition; a mismatch between t.HasDependencies
// and whether edges is non-empty is ErrDependencyEvidenceMissing (a
// dependent task given zero edges, or a zero-dependency task given any —
// the shape-consistency check that keeps an empty edge set from being
// vacuous ground truth); otherwise ErrDependencyNotIntegrated when the
// (shape-consistent) evidence is incomplete, contains a foreign or
// duplicate prerequisite, or any named prerequisite has not reached
// integrated. A task with no dependencies is created ready directly and
// never calls Release in practice.
func (t Task) Release(edges []TaskDependency, prerequisites []Task, now time.Time) (Task, error) { //nolint:gocritic // hugeParam: Task is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	if t.State != TaskPending || t.Kind == TaskKindReview {
		return t, fmt.Errorf("%w: task %s: %s to %s", ErrInvalidTransition, t.ID, t.State, TaskReady)
	}
	if t.HasDependencies != (len(edges) > 0) {
		return t, fmt.Errorf("%w: task %s", ErrDependencyEvidenceMissing, t.ID)
	}
	if !ReleaseEligible(t, edges, prerequisites) {
		return t, fmt.Errorf("%w: task %s", ErrDependencyNotIntegrated, t.ID)
	}
	t.State = TaskReady
	t.UpdatedAt = now
	return t, nil
}

// Reopen moves a task from needs-rework back to ready: a retry was
// consumed (implement) or the controller re-armed a reviewer retry
// (review).
func (t Task) Reopen(now time.Time) (Task, error) { return t.transition(TaskReady, now) } //nolint:gocritic // hugeParam: Task is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// EnterChecking moves the task into checking: a result has been accepted.
// Valid only for an implement task (a review task's table has no entry
// reaching checking).
func (t Task) EnterChecking(now time.Time) (Task, error) { return t.transition(TaskChecking, now) } //nolint:gocritic // hugeParam: Task is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// Complete moves the task into completed: its per-task check passed
// (implement, from checking) or its review verdict was accepted (review,
// from active).
func (t Task) Complete(now time.Time) (Task, error) { return t.transition(TaskCompleted, now) } //nolint:gocritic // hugeParam: Task is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// EnterIntegrating moves an implement task into integrating: its
// integration was claimed (serial slot free, dependency order).
func (t Task) EnterIntegrating(now time.Time) (Task, error) { //nolint:gocritic // hugeParam: Task is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	return t.transition(TaskIntegrating, now)
}

// Integrate moves an implement task into integrated: its merge committed
// and the combined-candidate check passed.
func (t Task) Integrate(now time.Time) (Task, error) { return t.transition(TaskIntegrated, now) } //nolint:gocritic // hugeParam: Task is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// NeedsRework moves the task into needs-rework: a merge conflict or failed
// combined check with room in the retry budget (implement, from
// integrating, active or checking); or an interrupted/failed reviewer
// attempt with a controller-issued retry (review, from active). The
// caller has already confirmed the retry budget has room — at the
// exhausted limit the settling transaction calls Fail directly instead.
func (t Task) NeedsRework(now time.Time) (Task, error) { return t.transition(TaskNeedsRework, now) } //nolint:gocritic // hugeParam: Task is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// Fail moves the task into failed: retry limit reached, or a terminal
// attempt failure with no retry path. A failed implement task fails the
// run (section 5); no verb revives a failed task.
func (t Task) Fail(now time.Time) (Task, error) { return t.transition(TaskFailed, now) } //nolint:gocritic // hugeParam: Task is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// Interrupt moves the task into interrupted: a stop, from any non-terminal
// state each kind's table allows.
func (t Task) Interrupt(now time.Time) (Task, error) { return t.transition(TaskInterrupted, now) } //nolint:gocritic // hugeParam: Task is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.

// CloseMailbox closes t's mailbox: no further message may be sent to
// task:<t.ID>. An accepted result/verdict's acceptance transaction and a
// task settling failed both close admission in the same commit (section
// 5); this method only records the flag, since the transactional
// snapshot-equality contract around a failure's orphaned obligations is
// the application's concern.
func (t Task) CloseMailbox(now time.Time) Task { //nolint:gocritic // hugeParam: Task is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	t.MailboxClosed = true
	t.UpdatedAt = now
	return t
}

// ReopenMailbox reopens t's mailbox: a retry's successor attempt fetches
// the same task address again. The application never calls this for a
// task closed by failure — no verb revives a failed task — only for one
// re-armed by a consumed retry.
func (t Task) ReopenMailbox(now time.Time) Task { //nolint:gocritic // hugeParam: Task is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	t.MailboxClosed = false
	t.UpdatedAt = now
	return t
}
