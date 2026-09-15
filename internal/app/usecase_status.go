package app

import (
	"context"
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// StatusRequest is hop status's input. An empty RunID lists every run of
// the repository; a non-empty one renders that run's full detail block.
type StatusRequest struct {
	RepositoryRoot string
	RunID          string
}

// RunSummaryView is one rendered line of `hop status` without -run.
type RunSummaryView struct {
	RunID    string
	Sequence int
	State    string
	// StopRequested mirrors the run's monotonic stop flag, so a foreground
	// controller can route to stop handling when hop stop was requested
	// elsewhere.
	StopRequested bool
	Reconciling   bool
	UpdatedAt     time.Time
}

// RunDetailView is the full detail block `hop status -run` renders. Every
// field the underlying store may not have populated yet renders as its
// zero value ("" or 0), never a guess.
type RunDetailView struct {
	RunSummaryView
	TaskState      string
	AttemptState   string
	WorktreePath   string
	BindingSummary string // "workspace/tab/pane"; "" when no current binding
	ClaimState     string // "" when no launch claim exists yet
	PendingOps     int
	LastSubmission string // the last submission outcome's kind; "" when none
	Artifacts      []string
	// LastCheckOperation names the newest check execution; "" when none ran.
	LastCheckOperation string
	LastCheckState     string
	LastCheckUnknown   bool
	LastCheckDetail    string
	LastCheckEvidence  []string
	// LastCheckOptions spells the human's options for an unknown outcome
	// (docs/plan/phase-2-design.md sections 7-8): rerun after inspection is
	// a human decision, never an automatic one.
	LastCheckOptions string
}

// StatusResult is Status's success value: exactly one of Runs (the -run-less
// listing) or Detail (a single run's detail) is populated.
type StatusResult struct {
	Runs   []RunSummaryView
	Detail *RunDetailView
}

// Status renders hop status's output from ReadStore, without a lease.
func (c *Controller) Status(ctx context.Context, req StatusRequest) (StatusResult, error) {
	if req.RunID == "" {
		statuses, err := c.Read.ListRuns(ctx, req.RepositoryRoot)
		if err != nil {
			return StatusResult{}, fmt.Errorf("app: list runs: %w", err)
		}
		views := make([]RunSummaryView, len(statuses))
		for i, s := range statuses {
			views[i] = runSummaryView(s)
		}
		return StatusResult{Runs: views}, nil
	}

	runID, err := identity.ParseRunID(req.RunID)
	if err != nil {
		return StatusResult{}, fmt.Errorf("app: parse run id: %w", err)
	}
	detail, err := c.Read.LoadRunStatus(ctx, runID)
	if err != nil {
		return StatusResult{}, fmt.Errorf("app: load run status: %w", err)
	}
	view := runDetailView(detail)
	return StatusResult{Detail: &view}, nil
}

func runSummaryView(s RunStatus) RunSummaryView {
	return RunSummaryView{
		RunID: s.RunID.String(), Sequence: s.Sequence, State: string(s.State),
		StopRequested: s.StopRequested, Reconciling: s.Reconciling, UpdatedAt: s.UpdatedAt,
	}
}

func runDetailView(d RunDetail) RunDetailView { //nolint:gocritic // hugeParam: RunDetail is a ReadStore DTO rendered at most once per hop status -run call.
	view := RunDetailView{
		RunSummaryView: runSummaryView(d.RunStatus),
		TaskState:      string(d.TaskState),
		AttemptState:   string(d.AttemptState),
		WorktreePath:   d.WorktreePath,
		PendingOps:     len(d.PendingOperations),
	}
	if d.Binding != nil {
		view.BindingSummary = fmt.Sprintf("%s/%s/%s", d.Binding.WorkspaceID, d.Binding.TabID, d.Binding.PaneID)
	}
	if d.Claim != nil {
		view.ClaimState = string(d.Claim.State)
	}
	if d.LastSubmission != nil {
		view.LastSubmission = string(d.LastSubmission.Kind)
	}
	if d.LastCheck != nil {
		view.LastCheckOperation = d.LastCheck.OperationID.String()
		view.LastCheckState = string(d.LastCheck.State)
		view.LastCheckUnknown = d.LastCheck.Unknown
		view.LastCheckDetail = d.LastCheck.Detail
		view.LastCheckEvidence = d.LastCheck.EvidencePaths
		if d.LastCheck.Unknown {
			view.LastCheckOptions = "inspect the retained evidence at the listed paths; automated retry is not available for this outcome — after inspection, start a new run for further work, or use hop stop if this run is still active"
		}
	}
	for _, a := range d.Artifacts {
		view.Artifacts = append(view.Artifacts, a.Path)
	}
	return view
}
