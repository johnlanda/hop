package app

import (
	"context"
	"fmt"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// Recovery of unresolved worktree-retirement operations
// (docs/plan/phase-3-worktree-retirement.md section 7), the first step of
// every pass. The pass holds a newer lease generation than any operation
// an earlier pass left behind, and ClaimCheckExec refuses a claim for an
// operation of another generation, so a missing claim is decisive: that
// execution never ran and never will.

// recoverRetirementOperations recovers every unresolved retirement.check
// and worktree.retire operation of the run, oldest first within each kind.
// It returns the first blocking detail — an operation that could not be
// settled, which blocks the rest of the pass — after attempting them all.
func (c *Controller) recoverRetirementOperations(ctx context.Context, handle RunHandle, frozen *FrozenRun, opts *retirementPassOptions) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per pass.
	var checks, retires []Operation
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		var err error
		if checks, err = unresolvedOperations(ctx, uow, handle.runID, OpRetirementCheck); err != nil {
			return err
		}
		retires, err = unresolvedOperations(ctx, uow, handle.runID, OpWorktreeRetire)
		return err
	}); err != nil {
		return "", fmt.Errorf("app: read unresolved retirement operations: %w", err)
	}
	blocking := ""
	for i := range checks {
		detail, err := c.recoverRetirementCheck(ctx, handle, &checks[i])
		if err != nil {
			return "", err
		}
		if blocking == "" {
			blocking = detail
		}
	}
	for i := range retires {
		detail, err := c.recoverWorktreeRetire(ctx, handle, frozen, opts, &retires[i])
		if err != nil {
			return "", err
		}
		if blocking == "" {
			blocking = detail
		}
	}
	return blocking, nil
}

// unresolvedOperations lists the run's pending or reconciling operations
// of kind, oldest first.
func unresolvedOperations(ctx context.Context, uow UnitOfWork, runID identity.RunID, kind OperationKind) ([]Operation, error) {
	ops, err := uow.Operations().ByKind(ctx, runID, kind)
	if err != nil {
		return nil, err
	}
	var unresolved []Operation
	for i := len(ops) - 1; i >= 0; i-- {
		if ops[i].State == OperationPending || ops[i].State == OperationReconciling {
			unresolved = append(unresolved, ops[i])
		}
	}
	return unresolved, nil
}

// recoverWorktreeRetire applies the worktree.retire decision-table row to
// an unresolved removal left by an earlier pass:
//   - no claim: settled failed, never executed; the row is unchanged and
//     re-observed from scratch;
//   - a claim: the claimed group is retired first (a surviving `git
//     worktree remove` is killed mid-delete), and once it is observed gone
//     the checkout is observed again — unlisted and absent adopts the
//     removal (the row final); listed, present or not, settles failed with
//     result interrupted (the row active); unlisted but present releases
//     the row, left on disk;
//   - a group still running, a mismatched group, an uninspectable group or
//     an unobservable checkout blocks the pass, the operation never
//     re-dispatched.
//
// It returns the blocking detail, or "" once the operation is settled.
func (c *Controller) recoverWorktreeRetire(ctx context.Context, handle RunHandle, frozen *FrozenRun, opts *retirementPassOptions, op *Operation) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	if op.Generation >= handle.lease.Generation {
		return fmt.Sprintf("worktree.retire %s belongs to this pass's generation; left for its own settlement", op.ID), nil
	}
	intent, ok := decodeOperationPayload[worktreeRetireIntent](op.Intent)
	if !ok || intent.Decision != retireDecisionRemove || len(intent.Argv) == 0 || len(intent.SpawnArgv) == 0 ||
		intent.WorktreeID == "" || intent.RecordedPath == "" || intent.RepositoryRoot != frozen.RepositoryRoot {
		if err := c.markOperationReconciling(ctx, handle, op.ID, "worktree.retire intent is undecodable or names another repository root; failing closed"); err != nil {
			return "", err
		}
		return fmt.Sprintf("worktree.retire %s has an undecodable intent or names another repository root; failing closed", op.ID), nil
	}
	var (
		claim CheckExecClaim
		found bool
	)
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		var err error
		claim, found, err = uow.CheckExecClaims().Get(ctx, op.ID)
		return err
	}); err != nil {
		return "", fmt.Errorf("app: load worktree.retire exec claim: %w", err)
	}
	if !found {
		return "", c.settleRetireAct(ctx, handle, op.ID, intent.WorktreeID, OperationFailed, "", &worktreeRetireOutcome{
			Result: retireResultNeverExecuted, ExitCode: -1, Detail: "no exec claim under a superseded generation; the removal never ran",
		})
	}
	group, err := c.retireGroup(ctx, handle, claim.PID, [][]string{intent.Argv, intent.SpawnArgv})
	if err != nil {
		return "", err
	}
	switch group {
	case GroupEmpty:
	case GroupMatched:
		return fmt.Sprintf("worktree.retire group %d signaled; awaiting observed absence", claim.PID), nil
	case GroupMismatched:
		if err := c.markOperationReconciling(ctx, handle, op.ID, "worktree.retire group members do not match the recorded argv; failing closed"); err != nil {
			return "", err
		}
		return fmt.Sprintf("worktree.retire group %d does not match the recorded argv; failing closed", claim.PID), nil
	default:
		return fmt.Sprintf("worktree.retire group %d could not be inspected; failing closed", claim.PID), nil
	}

	candidate := attemptCheckout{RepositoryRoot: intent.RepositoryRoot, Path: intent.RecordedPath}
	listed, exists, observed := c.observeCheckout(ctx, &candidate, opts.InspectPath)
	if !observed {
		if err := c.markOperationReconciling(ctx, handle, op.ID, "the removal's group is gone but the checkout could not be observed; failing closed"); err != nil {
			return "", err
		}
		return fmt.Sprintf("worktree.retire %s: the checkout could not be observed after its group was retired", op.ID), nil
	}
	outcome := worktreeRetireOutcome{ExitCode: -1}
	state, target := OperationFailed, run.WorktreeState("")
	switch {
	case !listed && !exists:
		state, target, outcome.Result = OperationSucceeded, run.WorktreeRemoved, retireResultRemoved
		if intent.WasAbsent {
			target, outcome.Result = run.WorktreeAbsent, retireResultAbsent
		}
		outcome.Detail = "the interrupted removal completed; its exit status was never observed"
	case listed:
		outcome.Result = retireResultInterrupted
		outcome.Detail = "the removal was interrupted; git still lists the worktree"
	default:
		target = run.WorktreeReleased
		outcome.Result, outcome.Released = retireResultReleased, ReleasedNotRegistered
		outcome.Detail = "the interrupted removal left the directory on disk after git dropped the worktree"
	}
	return "", c.settleRetireAct(ctx, handle, op.ID, intent.WorktreeID, state, target, &outcome)
}
