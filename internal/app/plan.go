package app

import (
	"context"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// Task title/instructions bounds. The design names an explicit 64 KiB/4
// KiB split only for message bodies (section 13, open question 3); task
// titles and instructions are artifact-file content of the same shape, so
// this reuses those exact numbers rather than inventing an unrelated one.
const (
	TaskTitleLimit        = MessageBodyInlineLimit
	TaskInstructionsLimit = MessageBodyFileLimit
)

// WorkflowOutcomeKind is the outcome of one manager plan verb
// (task-create, retry, plan-close), the workflow_receipts analog of
// MessageOutcomeKind.
type WorkflowOutcomeKind string

// Workflow outcome kinds.
const (
	WorkflowAccepted    WorkflowOutcomeKind = "accepted"
	WorkflowDuplicate   WorkflowOutcomeKind = "duplicate"
	WorkflowConflicting WorkflowOutcomeKind = "conflicting"
	WorkflowRefused     WorkflowOutcomeKind = "refused"
	WorkflowMalformed   WorkflowOutcomeKind = "malformed"
)

// TaskCreate is the application-validated content of one `hop task
// create`: identities already parsed, the instructions artifact already
// written durably (temp-file-then-rename, digest computed) before this
// call, and DependsOn already bound-checked (a caller-supplied list, not
// yet validated against the persisted graph — CreateTask does that inside
// its own transaction, atomically with the acyclicity check).
type TaskCreate struct {
	ID                 identity.TaskID
	RunID              identity.RunID
	Session            identity.SessionID
	IncarnationID      identity.IncarnationID
	Title              string
	InstructionsPath   string
	InstructionsDigest string
	DependsOn          []identity.TaskID
	RequestID          string
}

// TaskCreated is CreateTask's outcome: `task <uuid> t<seq> created` /
// `duplicate <uuid> t<seq>` (an identical request-ID retry) / a refusal
// (non-manager caller, cross-run or cyclic dependency, run not accepting,
// kind=review is never requested here since this verb never creates one).
type TaskCreated struct {
	Outcome WorkflowOutcomeKind
	TaskID  identity.TaskID
	Seq     int
	Detail  string
}

// RetryRequest is the application-validated content of one `hop task
// retry`: legal only against a terminal attempt of a needs-rework task
// below the frozen retry limit.
type RetryRequest struct {
	TaskID        identity.TaskID
	RunID         identity.RunID
	Session       identity.SessionID
	IncarnationID identity.IncarnationID
	Reason        string
	RequestID     string
}

// RetryAccepted is RequestRetry's outcome: `retry accepted t<seq> attempt
// <n>` / `duplicate t<seq> attempt <n>` / a refusal (not needs-rework, not
// terminal, retry limit reached, non-manager caller).
type RetryAccepted struct {
	Outcome       WorkflowOutcomeKind
	AttemptNumber int
	Detail        string
}

// PlanClose is the application-validated content of one `hop plan close`.
type PlanClose struct {
	RunID         identity.RunID
	Session       identity.SessionID
	IncarnationID identity.IncarnationID
	RequestID     string
}

// PlanCloseResult is ClosePlan's outcome: `plan closed` / `duplicate plan
// closed` / a refusal (zero implement tasks, non-manager caller, run not
// accepting).
type PlanCloseResult struct {
	Outcome WorkflowOutcomeKind
	Detail  string
}

// PlanStore holds the section 8 worker-authority plan writes: manager-only
// verbs, validated by session role and current incarnation inside each
// method's own transaction (never a controller lease), which also
// validates the run is running (ErrRunNotAccepting otherwise) so a create
// can never race completion. Every method accepts an optional
// caller-stable RequestID: an identical retry returns the original
// outcome and entity, surviving consumption, relaunch, retirement and
// manager succession; a reused ID with different content is refused.
type PlanStore interface {
	// CreateTask validates title/instructions bounds and the dependency
	// edges against the persisted graph (acyclic, same run), writes the
	// task with its edges and instructions-artifact reference atomically,
	// and clears the run's plan flag (ReopenPlan) in the same commit.
	CreateTask(ctx context.Context, req TaskCreate) (TaskCreated, error)
	// RequestRetry reserves attempt n+1 for a terminal attempt of a
	// needs-rework task below the frozen retry limit.
	RequestRetry(ctx context.Context, req RetryRequest) (RetryAccepted, error)
	// ClosePlan sets the plan flag; a plan with zero implement tasks is
	// refused (ErrEmptyPlan).
	ClosePlan(ctx context.Context, req PlanClose) (PlanCloseResult, error)
}
