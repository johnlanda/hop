package app

import (
	"context"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
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
	// WorkflowTransient is the retryable outcome of a validated manager
	// whose run is not yet running (run.ErrRunNotYetRunning): nothing
	// changed, the receipt records "transient" outside the (run, verb,
	// request ID) acceptance key, and the same request may be retried.
	// Reason is empty; cmd/hop renders GrammarTransientRunNotRunningLine.
	WorkflowTransient WorkflowOutcomeKind = "transient"
)

// RunNotRunningDetail is the Detail every store sets on a WorkflowTransient
// or MessageTransient outcome: the run's state and nothing else, so the
// stderr line cmd/hop prints never echoes an identity, a path or an
// environment value.
func RunNotRunningDetail(state run.RunState) string {
	return "run is " + string(state) + ", not yet running"
}

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
// Reason is one of grammar.go's GrammarReason* tokens, set by the store at
// its exact decision point (never derived from Detail text downstream) on
// every WorkflowRefused or WorkflowMalformed outcome; empty on
// accepted/duplicate.
type TaskCreated struct {
	Outcome WorkflowOutcomeKind
	TaskID  identity.TaskID
	Seq     int
	Reason  string
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
// terminal, retry limit reached, non-manager caller). TaskSeq and
// AttemptNumber are the retried task's sequence and the reserved
// attempt's number, both set on accepted and on duplicate (the receipt
// replay reports the original acceptance's values), zero otherwise.
// Reason is one of grammar.go's GrammarReason* tokens, set by the store at
// its exact decision point, on every WorkflowRefused or WorkflowMalformed
// outcome; empty on accepted/duplicate.
type RetryAccepted struct {
	Outcome       WorkflowOutcomeKind
	TaskSeq       int
	AttemptNumber int
	Reason        string
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
// accepting). Reason is one of grammar.go's GrammarReason* tokens, set by
// the store at its exact decision point, on every WorkflowRefused or
// WorkflowMalformed outcome; empty on accepted/duplicate.
type PlanCloseResult struct {
	Outcome WorkflowOutcomeKind
	Reason  string
	Detail  string
}

// PlanStore holds the section 8 worker-authority plan writes: manager-only
// verbs, validated by session role and current incarnation inside each
// method's own transaction (never a controller lease), which then applies
// run.Run.CanAcceptManagerVerb so a create can never race completion:
// WorkflowTransient for a run that can still reach running, WorkflowRefused
// with GrammarReasonRunNotAccepting for one that never will. Every method accepts an optional
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
