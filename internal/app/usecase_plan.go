package app

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// CreateTaskRequest is `hop task create`'s CLI-facing input, exactly as
// parsed from flags and the manager's HOP_* environment: identities as
// plain strings (composition never imports domain or identity types),
// and InstructionsBody already read from --file by the caller.
type CreateTaskRequest struct {
	RunID            string
	SessionID        string
	IncarnationID    string
	StateRoot        string
	Title            string
	InstructionsBody []byte
	DependsOn        []string
	RequestID        string
}

// CreateTaskResult is CreateTask's outcome, string-only per the driving
// API convention. Reason is one of grammar.go's GrammarReason* tokens on
// every refused/malformed outcome (cmd/hop renders it directly, never
// re-deriving one from Detail text); empty on accepted/duplicate.
type CreateTaskResult struct {
	Outcome string
	TaskID  string
	Seq     int
	Reason  string
	Detail  string
}

// CreateTask is `hop task create`'s driving use case: bounds and writes
// the instructions body durably (file-first protocol, digest computed
// here, before any store call), parses the dependency list, then
// delegates validation (manager-only, run-accepting, acyclic, same-run
// dependencies) and acceptance to PlanStore.CreateTask, which owns the
// request-ID receipt and the plan-flag reopen.
func (c *Controller) CreateTask(ctx context.Context, req CreateTaskRequest) (CreateTaskResult, error) { //nolint:gocritic // hugeParam: CreateTaskRequest is the driving DTO for hop task create, carrying an instructions body; a pointer would only complicate composition's call site.
	if c.Plan == nil {
		return CreateTaskResult{}, fmt.Errorf("%w: CreateTask", ErrFeatureModeUnsupported)
	}
	runID, err := identity.ParseRunID(req.RunID)
	if err != nil {
		return CreateTaskResult{}, fmt.Errorf("app: parse run id: %w", err)
	}
	sessionID, err := identity.ParseSessionID(req.SessionID)
	if err != nil {
		return CreateTaskResult{}, fmt.Errorf("app: parse session id: %w", err)
	}
	incarnationID, err := identity.ParseIncarnationID(req.IncarnationID)
	if err != nil {
		return CreateTaskResult{}, fmt.Errorf("app: parse incarnation id: %w", err)
	}
	if req.Title == "" || len(req.Title) > TaskTitleLimit {
		return CreateTaskResult{Outcome: string(WorkflowMalformed), Reason: GrammarReasonMalformed, Detail: "title is empty or exceeds the size bound"}, nil
	}
	if len(req.InstructionsBody) == 0 || len(req.InstructionsBody) > TaskInstructionsLimit {
		return CreateTaskResult{Outcome: string(WorkflowMalformed), Reason: GrammarReasonMalformed, Detail: "instructions are empty or exceed the size bound"}, nil
	}
	dependsOn := make([]identity.TaskID, len(req.DependsOn))
	for i, raw := range req.DependsOn {
		id, parseErr := identity.ParseTaskID(raw)
		if parseErr != nil {
			return CreateTaskResult{Outcome: string(WorkflowMalformed), Reason: GrammarReasonMalformed, Detail: fmt.Sprintf("invalid dependency %q", raw)}, nil
		}
		dependsOn[i] = id
	}

	taskID, err := identity.ParseTaskID(c.IDs.NewID())
	if err != nil {
		return CreateTaskResult{}, fmt.Errorf("app: generate task id: %w", err)
	}
	instructionsPath := taskInstructionsPath(req.StateRoot, runID, taskID)
	instructionsDigest := sha256Hex(req.InstructionsBody)
	if err = c.Artifacts.WriteArtifact(ctx, instructionsPath, req.InstructionsBody); err != nil {
		return CreateTaskResult{}, fmt.Errorf("app: write task instructions: %w", err)
	}

	outcome, err := c.Plan.CreateTask(ctx, TaskCreate{
		ID: taskID, RunID: runID, Session: sessionID, IncarnationID: incarnationID,
		Title: req.Title, InstructionsPath: instructionsPath, InstructionsDigest: instructionsDigest,
		DependsOn: dependsOn, RequestID: req.RequestID,
	})
	if err != nil {
		return CreateTaskResult{}, fmt.Errorf("app: create task: %w", err)
	}
	return CreateTaskResult{Outcome: string(outcome.Outcome), TaskID: outcome.TaskID.String(), Seq: outcome.Seq, Reason: outcome.Reason, Detail: outcome.Detail}, nil
}

// RequestRetryRequest is `hop task retry`'s driving input.
type RequestRetryRequest struct {
	RunID         string
	SessionID     string
	IncarnationID string
	TaskID        string
	Reason        string
	RequestID     string
}

// RequestRetryResult is RequestRetry's outcome. TaskSeq (the retried
// task's t<seq>) and AttemptNumber name the grammar's accepted and
// duplicate lines. Reason is one of grammar.go's GrammarReason* tokens on
// every refused/malformed outcome; empty on accepted/duplicate.
type RequestRetryResult struct {
	Outcome       string
	TaskSeq       int
	AttemptNumber int
	Reason        string
	Detail        string
}

// RequestRetry is `hop task retry`'s driving use case: delegates directly
// to PlanStore.RequestRetry, which validates the caller is the current
// manager, the task is needs-rework with a terminal prior attempt below
// the frozen retry limit, and reserves the new attempt immediately (the
// grammar's `retry accepted t<seq> attempt <n>` names the number in this
// same response — never deferred to a later controller step).
func (c *Controller) RequestRetry(ctx context.Context, req RequestRetryRequest) (RequestRetryResult, error) { //nolint:gocritic // hugeParam: RequestRetryRequest is the driving DTO for hop task retry, called once per invocation.
	if c.Plan == nil {
		return RequestRetryResult{}, fmt.Errorf("%w: RequestRetry", ErrFeatureModeUnsupported)
	}
	runID, err := identity.ParseRunID(req.RunID)
	if err != nil {
		return RequestRetryResult{}, fmt.Errorf("app: parse run id: %w", err)
	}
	sessionID, err := identity.ParseSessionID(req.SessionID)
	if err != nil {
		return RequestRetryResult{}, fmt.Errorf("app: parse session id: %w", err)
	}
	incarnationID, err := identity.ParseIncarnationID(req.IncarnationID)
	if err != nil {
		return RequestRetryResult{}, fmt.Errorf("app: parse incarnation id: %w", err)
	}
	taskID, err := identity.ParseTaskID(req.TaskID)
	if err != nil {
		return RequestRetryResult{}, fmt.Errorf("app: parse task id: %w", err)
	}
	outcome, err := c.Plan.RequestRetry(ctx, RetryRequest{
		TaskID: taskID, RunID: runID, Session: sessionID, IncarnationID: incarnationID, Reason: req.Reason, RequestID: req.RequestID,
	})
	if err != nil {
		return RequestRetryResult{}, fmt.Errorf("app: request retry: %w", err)
	}
	return RequestRetryResult{Outcome: string(outcome.Outcome), TaskSeq: outcome.TaskSeq, AttemptNumber: outcome.AttemptNumber, Reason: outcome.Reason, Detail: outcome.Detail}, nil
}

// ClosePlanRequest is `hop plan close`'s driving input.
type ClosePlanRequest struct {
	RunID         string
	SessionID     string
	IncarnationID string
	RequestID     string
}

// ClosePlanResult is ClosePlan's outcome. Reason is one of grammar.go's
// GrammarReason* tokens on every refused/malformed outcome; empty on
// accepted/duplicate.
type ClosePlanResult struct {
	Outcome string
	Reason  string
	Detail  string
}

// ClosePlan is `hop plan close`'s driving use case: delegates directly to
// PlanStore.ClosePlan, which refuses a plan with zero implement tasks
// (ErrEmptyPlan).
func (c *Controller) ClosePlan(ctx context.Context, req ClosePlanRequest) (ClosePlanResult, error) {
	if c.Plan == nil {
		return ClosePlanResult{}, fmt.Errorf("%w: ClosePlan", ErrFeatureModeUnsupported)
	}
	runID, err := identity.ParseRunID(req.RunID)
	if err != nil {
		return ClosePlanResult{}, fmt.Errorf("app: parse run id: %w", err)
	}
	sessionID, err := identity.ParseSessionID(req.SessionID)
	if err != nil {
		return ClosePlanResult{}, fmt.Errorf("app: parse session id: %w", err)
	}
	incarnationID, err := identity.ParseIncarnationID(req.IncarnationID)
	if err != nil {
		return ClosePlanResult{}, fmt.Errorf("app: parse incarnation id: %w", err)
	}
	outcome, err := c.Plan.ClosePlan(ctx, PlanClose{RunID: runID, Session: sessionID, IncarnationID: incarnationID, RequestID: req.RequestID})
	if err != nil {
		return ClosePlanResult{}, fmt.Errorf("app: close plan: %w", err)
	}
	return ClosePlanResult{Outcome: string(outcome.Outcome), Reason: outcome.Reason, Detail: outcome.Detail}, nil
}

// taskInstructionsPath is the deterministic artifact path for one task's
// frozen instructions, under the run's artifact directory.
func taskInstructionsPath(stateRoot string, runID identity.RunID, taskID identity.TaskID) string {
	return filepath.Join(stateRoot, "runs", runID.String(), "tasks", taskID.String()+".md")
}
