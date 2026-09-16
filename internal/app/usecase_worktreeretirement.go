package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// The post-merge worktree retirement pass
// (docs/plan/phase-3-worktree-retirement.md section 3): a controller
// start or `hop status` takes each eligible run's lease for one pass,
// heartbeats it from composition, and releases it again without
// journaling anything of its own.

// ErrRetirementNotEligible reports a run the worktree-retirement pass
// never acts on: not a terminal feature run with a frozen target branch,
// or one whose worktrees are already retired.
var ErrRetirementNotEligible = errors.New("app: run is not eligible for worktree retirement")

// isTerminalRunState reports the three states a run never leaves.
func isTerminalRunState(state run.RunState) bool {
	switch state {
	case run.RunCompleted, run.RunFailed, run.RunStopped:
		return true
	default:
		return false
	}
}

// AcquireForRetirement takes runID's controller lease for one
// worktree-retirement pass. A run that is not eligible — not a feature
// run, frozen without a target branch, not terminal, or already retired —
// is refused with ErrRetirementNotEligible before the lease is touched. A
// lease held by a live controller passes ErrLeaseHeld through: the pass
// defers that run. Eligibility is re-checked under the acquired lease,
// since the lease-free read may be stale, and a refusal there releases
// the lease again.
func (c *Controller) AcquireForRetirement(ctx context.Context, runIDArg, controllerID string) (RunHandle, error) {
	runID, err := identity.ParseRunID(runIDArg)
	if err != nil {
		return RunHandle{}, fmt.Errorf("app: parse run id: %w", err)
	}
	if controllerID == "" {
		return RunHandle{}, errors.New("app: a controller id is required to acquire a run lease")
	}
	if eligibleErr := c.retirementEligible(ctx, runID); eligibleErr != nil {
		return RunHandle{}, eligibleErr
	}
	lease, err := c.Store.AcquireLease(ctx, runID, controllerID)
	if err != nil {
		return RunHandle{}, fmt.Errorf("app: acquire the run lease for worktree retirement: %w", err)
	}
	handle := newRunHandle(runID, lease)
	recheck := c.withUnitOfWork(ctx, lease, func(uow UnitOfWork) error {
		r, _, getErr := uow.Runs().Get(ctx, runID)
		if getErr != nil {
			return getErr
		}
		if !isTerminalRunState(r.State) {
			return fmt.Errorf("%w: the run is %s, not terminal", ErrRetirementNotEligible, r.State)
		}
		return nil
	})
	if recheck != nil {
		if releaseErr := c.ReleaseRetirement(ctx, handle); releaseErr != nil {
			return RunHandle{}, errors.Join(recheck, releaseErr)
		}
		return RunHandle{}, recheck
	}
	return handle, nil
}

// retirementEligible is AcquireForRetirement's lease-free eligibility
// read: the frozen snapshot (immutable) and the run's detail.
func (c *Controller) retirementEligible(ctx context.Context, runID identity.RunID) error {
	frozen, err := c.Read.LoadFrozenRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("app: load frozen run: %w", err)
	}
	switch {
	case !frozen.Snapshot.Workflow.Feature():
		return fmt.Errorf("%w: a solo run has no integration branch", ErrRetirementNotEligible)
	case frozen.Snapshot.Workflow.TargetBranch == "":
		return fmt.Errorf("%w: the run was frozen without a target branch", ErrRetirementNotEligible)
	}
	detail, err := c.Read.LoadRunStatus(ctx, runID)
	if err != nil {
		return fmt.Errorf("app: load run status: %w", err)
	}
	switch {
	case detail.WorktreesRetiredAt != nil:
		return fmt.Errorf("%w: the run's worktrees are already retired", ErrRetirementNotEligible)
	case !isTerminalRunState(detail.State):
		return fmt.Errorf("%w: the run is %s, not terminal", ErrRetirementNotEligible, detail.State)
	}
	return nil
}

// ReleaseRetirement ends a worktree-retirement pass: it cancels the
// handle's in-flight acts and releases the lease, journaling nothing — a
// pass leaves no detach transition behind, so repeated passes do not grow
// the store.
func (c *Controller) ReleaseRetirement(ctx context.Context, handle RunHandle) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per pass.
	handle.cancelDispatch()
	if err := c.Store.ReleaseLease(ctx, handle.lease); err != nil {
		return fmt.Errorf("app: release the worktree-retirement lease: %w", err)
	}
	return nil
}
