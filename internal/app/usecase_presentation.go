package app

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/johnlanda/hop/internal/domain/run"
)

// paneTokenValueLimit is Herdr's 80-character token value cap (section
// 9): hop_task carries the task title truncated to it.
const paneTokenValueLimit = 80

// PresentationReport is one PublishRunPresentation round's outcome.
type PresentationReport struct {
	// Published lists the pane IDs whose metadata was reported, in the
	// deterministic publish order.
	Published []string
	// Skipped lists the pane IDs the server reported as not found this
	// round, in the same order: nothing was published to them and nothing
	// else was recorded.
	Skipped []string
}

// PublishRunPresentation publishes the run's live state tokens for every
// session with a current binding (docs/plan/phase-3-design.md section
// 9): hop_run, hop_run_order, hop_order (manager first), hop_role,
// hop_task, hop_parent and hop_state, through the Phase 1
// AgentPresentation port's full-replacement semantics — an unset
// optional token is cleared, never left stale. The controller calls it
// on every transition that changes a session's tokens and again on
// resume: token metadata is not cold-restored, so rehydration is simply
// a fresh full publication against the current bindings. A superseded
// binding is never published to.
//
// A pane can vanish at any moment — its process exits, or a human closes
// it — so a pane the server reports as not found (ErrPaneNotFound) is
// skipped for this round: nothing is published to it, nothing is
// recorded, and the session's fate stays with launch corroboration,
// retirement and resume, which decide absence under their own evidence
// rules. Every other publication error is returned.
func (c *Controller) PublishRunPresentation(ctx context.Context, handle RunHandle) (PresentationReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per transition batch.
	if c.Presentation == nil {
		return PresentationReport{}, fmt.Errorf("%w: presentation publication", ErrFeatureModeUnsupported)
	}
	displays, err := c.assembleRunDisplays(ctx, handle)
	if err != nil {
		return PresentationReport{}, err
	}
	presenter := &Presenter{Presentation: c.Presentation}
	report := PresentationReport{}
	for i := range displays {
		if err := presenter.Publish(ctx, &displays[i]); err != nil {
			if errors.Is(err, ErrPaneNotFound) {
				report.Skipped = append(report.Skipped, displays[i].PaneID)
				continue
			}
			return report, fmt.Errorf("app: publish presentation for pane %s: %w", displays[i].PaneID, err)
		}
		report.Published = append(report.Published, displays[i].PaneID)
	}
	return report, nil
}

// assembleRunDisplays reads the run's sessions and builds their displays,
// manager-first.
func (c *Controller) assembleRunDisplays(ctx context.Context, handle RunHandle) ([]AgentDisplay, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	var displays []AgentDisplay
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "PublishRunPresentation")
		if wfErr != nil {
			return wfErr
		}
		r, _, runErr := uow.Runs().Get(ctx, handle.runID)
		if runErr != nil {
			return runErr
		}
		sessions, sessErr := wf.SessionIndex().ByRun(ctx, handle.runID)
		if sessErr != nil {
			return sessErr
		}
		sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
		for i := range sessions {
			s := sessions[i]
			if s.State == run.SessionTerminated || s.State == run.SessionLost {
				continue
			}
			binding, found, bindErr := uow.Bindings().Current(ctx, s.ID)
			if bindErr != nil {
				return bindErr
			}
			if !found || binding.Superseded || binding.PaneID == "" {
				continue
			}
			display, ok, displayErr := sessionDisplay(ctx, uow, &r, &s, binding.PaneID)
			if displayErr != nil {
				return displayErr
			}
			if ok {
				displays = append(displays, display)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return SortDisplays(displays), nil
}

// sessionDisplay builds one session's display: role, ordering, task
// label, parent label and the HOP condition label.
func sessionDisplay(ctx context.Context, uow UnitOfWork, r *run.Run, s *run.Session, paneID string) (AgentDisplay, bool, error) {
	display := AgentDisplay{
		PaneID:      paneID,
		Run:         fmt.Sprintf("r%d", r.Sequence),
		RunSequence: r.Sequence,
	}
	switch s.Role {
	case run.RoleManager:
		display.Role = RoleManager
		display.WorkerSequence = 0
		if r.PlanClosed {
			display.State = "working"
		} else {
			display.State = "planning"
		}
		return display, true, nil
	case run.RoleImplementer, run.RoleReviewer:
		if s.Role == run.RoleReviewer {
			display.Role = RoleReviewer
		} else {
			display.Role = RoleImplementer
		}
		display.ParentLabel = fmt.Sprintf("manager-r%d", r.Sequence)
		task, taskErr := sessionTaskOf(ctx, uow, s)
		if taskErr != nil {
			return AgentDisplay{}, false, taskErr
		}
		display.WorkerSequence = 10 + 10*task.Seq
		display.Task = truncateToken(task.Title)
		display.State = taskConditionLabel(&task, s.Role)
		return display, true, nil
	default:
		// The Phase 2 solo worker keeps the Phase 1/2 presentation path.
		return AgentDisplay{}, false, nil
	}
}

// sessionTask resolves the task a child session's attempt executes.
func sessionTaskOf(ctx context.Context, uow UnitOfWork, s *run.Session) (run.Task, error) {
	if s.AttemptID == "" {
		return run.Task{}, fmt.Errorf("app: child session %s has no attempt", s.ID)
	}
	attempt, _, err := uow.Attempts().Get(ctx, s.AttemptID)
	if err != nil {
		return run.Task{}, err
	}
	task, _, err := uow.Tasks().Get(ctx, attempt.TaskID)
	if err != nil {
		return run.Task{}, err
	}
	return task, nil
}

// taskConditionLabel maps a child session's task state to the section 9
// hop_state condition label.
func taskConditionLabel(task *run.Task, role run.Role) string {
	if role == run.RoleReviewer {
		return "review"
	}
	switch task.State {
	case run.TaskChecking:
		return "awaiting checks"
	case run.TaskIntegrating:
		return "integrating"
	case run.TaskNeedsRework, run.TaskFailed:
		return "needs attention"
	default:
		return "working"
	}
}

// truncateToken bounds a token value to Herdr's 80-character cap.
func truncateToken(value string) string {
	if len(value) <= paneTokenValueLimit {
		return value
	}
	return value[:paneTokenValueLimit]
}
