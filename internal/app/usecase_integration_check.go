package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// DriveIntegrationCheck runs one combined-candidate check round for the
// run's current integration: the long external act of the serial
// integration step, which DriveIntegration reports CheckDue and never
// runs itself, so a caller can run it under a context it cancels. A
// canceled round kills the check's process group through the runner and
// journals the execution reconciling (spawned) or failed as never spawned
// (canceled before its spawn), which DriveFeatureStop then retires or
// passes over. The round acts only on an integration still in checking
// with no unresolved integration-step operation: it adopts a settled
// receipt for the candidate or runs a fresh execution, and never recovers,
// publishes or resets anything. It does nothing, reporting InFlight, while
// a DriveIntegration call of the same handle is running.
func (c *Controller) DriveIntegrationCheck(ctx context.Context, handle RunHandle, hopPath string, spawnEnv []string) (IntegrationReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per combined-check round.
	release, owned := handle.claimIntegrationStep()
	if !owned {
		return IntegrationReport{InFlight: true}, nil
	}
	defer release()
	frozen, err := c.Read.LoadFrozenRun(ctx, handle.runID)
	if err != nil {
		return IntegrationReport{}, fmt.Errorf("app: load frozen run: %w", err)
	}
	if !frozen.Snapshot.Workflow.Feature() {
		return IntegrationReport{}, nil
	}
	blocked, err := c.unresolvedIntegrationStepOperation(ctx, handle)
	if err != nil {
		return IntegrationReport{}, err
	}
	if blocked != "" {
		return IntegrationReport{Blocked: blocked}, nil
	}
	integ, exists, err := c.currentIntegration(ctx, handle)
	if err != nil || !exists {
		return IntegrationReport{}, err
	}
	if integ.State != run.IntegrationChecking {
		return IntegrationReport{IntegrationID: integ.ID.String(), State: string(integ.State)}, nil
	}
	return c.advanceChecking(ctx, handle, &frozen, hopPath, spawnEnv, &integ, true)
}

// isIntegrationStepOperation reports whether op belongs to the serial
// integration step: every kind the step journals, and a check execution
// whose intent names an integration.
func isIntegrationStepOperation(op *Operation) bool {
	switch op.Kind {
	case OpIntegrationMerge, OpIntegrationPublish, OpIntegrationReset, OpIntegrationFence:
		return true
	case OpCheckRun:
		intent, ok := decodeOperationPayload[integrationCheckIntent](op.Intent)
		return ok && intent.IntegrationID != ""
	default:
		return false
	}
}

// unresolvedIntegrationStepOperation names an unresolved integration-step
// operation of the run, "" when none stands: DriveIntegration's recovery
// owns every such operation, so a combined-check round starts nothing
// while one exists.
func (c *Controller) unresolvedIntegrationStepOperation(ctx context.Context, handle RunHandle) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	blocked := ""
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		ops, opErr := uow.Operations().Pending(ctx, handle.runID)
		if opErr != nil {
			return opErr
		}
		for i := range ops {
			if isIntegrationStepOperation(&ops[i]) {
				blocked = fmt.Sprintf("%s %s is unresolved; the integration step recovers it first", ops[i].Kind, ops[i].ID)
				return nil
			}
		}
		return nil
	})
	return blocked, err
}

// combinedCheckSettlement is what one combined-check outcome transaction
// did.
type combinedCheckSettlement int

const (
	// combinedCheckIntegrated: the passing candidate was integrated.
	combinedCheckIntegrated combinedCheckSettlement = iota
	// combinedCheckFailed: the integration moved to check-failed.
	combinedCheckFailed
	// combinedCheckStopHeld: the outcome was recorded under a held stop
	// and the integration left to the stop path.
	combinedCheckStopHeld
	// combinedCheckSettledElsewhere: the execution was already settled
	// (a shutdown retired it), so nothing was written.
	combinedCheckSettledElsewhere
)

// runIntegrationCheck drives one combined-candidate check execution: the
// generalized check pipeline's integration subject (docs/plan/
// phase-3-design.md section 8). Its own detached checkout under the
// check operation's directory — a scratch merge tree is never the check
// target — the frozen argv and timeout, mandatory evidence retention,
// and an outcome transaction dispatching by the integration/feature row
// of the outcome table. The intent is refused under a held stop or a
// terminal-failure cause (unstarted work is never started into a
// stopping or failing run), and a round that ends before its spawn
// settles the operation failed as never spawned, since no process can
// have claimed it. The outcome, its evidence and the integration's
// consequence commit in one transaction, and only while the operation is
// still unresolved: a shutdown that settled the execution first owns the
// integration. interrupted reports that a stop request or failure cause
// prevented new work, that a stop claimed the recorded outcome, or that a
// shutdown settled the execution.
func (c *Controller) runIntegrationCheck(ctx context.Context, handle RunHandle, frozen *FrozenRun, integ *run.Integration, subjectTree, hopPath string, spawnEnv []string) (bool, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per combined-check execution.
	opID, err := c.newOperationID()
	if err != nil {
		return false, err
	}
	checkoutPath := checkExecutionCheckoutPath(frozen.Snapshot.StateRoot, handle.runID, opID)
	intent := integrationCheckIntent{
		IntegrationID:    integ.ID.String(),
		SubjectCommitOID: integ.MergeCommitOID,
		SubjectTreeOID:   subjectTree,
		ResultID:         integ.ResultID.String(),
		CheckoutPath:     checkoutPath,
		CheckArgv:        frozen.Snapshot.CheckArgv,
		SpawnArgv:        checkSpawnArgv(hopPath, opID, frozen.Snapshot.CheckArgv),
	}
	now := c.Clock.Now()
	interrupted := false
	if intentErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "combined check")
		if wfErr != nil {
			return wfErr
		}
		r, _, runErr := uow.Runs().Get(ctx, handle.runID)
		if runErr != nil {
			return runErr
		}
		if r.StopRequested {
			// Unstarted work is never started after stop: the published
			// candidate belongs to the stop path's reset.
			interrupted = true
			return nil
		}
		failing, causeErr := featureFailureCauseLocked(ctx, uow, wf, handle.runID)
		if causeErr != nil {
			return causeErr
		}
		if failing {
			// Nor into a failing run: the candidate belongs to the
			// terminal-failure shutdown's reset.
			interrupted = true
			return nil
		}
		return uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpCheckRun, State: OperationPending, Intent: intent,
			CreatedAt: now, UpdatedAt: now,
		})
	}); intentErr != nil {
		return false, fmt.Errorf("app: record combined-check intent: %w", intentErr)
	}
	if interrupted {
		return true, nil
	}

	// Settlements outlive a canceled round: an interrupted execution is
	// still journaled, never left for recovery to guess at.
	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), checkPersistenceTimeout)
	defer persistCancel()
	actCtx, release := handle.actContext(ctx)
	defer release()
	if revErr := c.revalidateForDispatch(ctx, handle, false); revErr != nil {
		return false, c.settleUnspawnedCheck(persistCtx, handle, opID, fmt.Errorf("app: revalidate before combined-check checkout: %w", revErr))
	}
	if matErr := c.materializeCheckout(actCtx, frozen.RepositoryRoot, checkoutPath, integ.MergeCommitOID); matErr != nil {
		// No execution was ever spawned: the operation settles failed and
		// a later round runs a fresh execution against the same candidate.
		return false, c.settleUnspawnedCheck(persistCtx, handle, opID, fmt.Errorf("app: materialize combined-check checkout: %w", matErr))
	}

	if revErr := c.revalidateForDispatch(ctx, handle, false); revErr != nil {
		return false, c.settleUnspawnedCheck(persistCtx, handle, opID, fmt.Errorf("app: revalidate before combined-check spawn: %w", revErr))
	}
	timeout := frozen.Snapshot.CheckTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	boundedCtx, cancel := context.WithTimeout(actCtx, timeout)
	cmdResult, runErr := c.Commands.Run(boundedCtx, Command{Argv: intent.SpawnArgv, Dir: checkoutPath, Env: withHOPStateDir(spawnEnv, frozen.Snapshot.StateRoot)})
	cancel()

	evidence, captureErr := c.captureCheckOutputs(persistCtx, handle, frozen, opID, integ.ResultID, cmdResult)
	if runErr != nil {
		if len(evidence) > 0 {
			if saveErr := c.saveEvidenceRows(persistCtx, handle, evidence); saveErr != nil {
				return false, saveErr
			}
		}
		detail := fmt.Sprintf("combined-check spawn returned an error before an outcome was observed: %v", runErr)
		if captureErr != nil {
			detail += fmt.Sprintf("; output retention also failed: %v", captureErr)
		}
		if markErr := c.markOperationReconciling(persistCtx, handle, opID, detail); markErr != nil {
			return false, markErr
		}
		return false, fmt.Errorf("app: combined-check execution ambiguous: %w", runErr)
	}

	outcome := checkRunOutcome{ExitCode: cmdResult.ExitCode}
	retained := captureErr == nil
	if !retained {
		// Retention is mandatory: completion may never claim evidence that
		// was lost, whatever the exit code says. The execution fails — the
		// operation settles failed with the real exit recorded, the
		// integration moves check-failed so the reset retires the
		// candidate, and the checkout stays inspectable. The receipt
		// readers additionally refuse a failed-state zero-exit row, so
		// this shape can never be adopted as a passing receipt.
		outcome.Detail = fmt.Sprintf("evidence retention failed: %v", captureErr)
	}
	// A held stop is read once here, before the notice body is written.
	// The settling transaction's own read below is still the authority —
	// it decides, under the lease, whether the integration happens — and
	// this one only avoids writing an artifact for a notice that read is
	// about to discard. Preparing the body first is otherwise harmless
	// (the mailbox is the store, and only commitControllerNotice makes a
	// notice a message), but it leaves a file under the run's messages
	// directory with no row naming it.
	stopHeld, stopErr := c.runStopRequested(persistCtx, handle)
	if stopErr != nil {
		return false, stopErr
	}
	var notice controllerNotice
	if retained && cmdResult.ExitCode == 0 && !stopHeld {
		prepared, noticeErr := c.prepareIntegratedNotice(persistCtx, handle, frozen, integ, fmt.Sprintf("combined check passed (operation %s)", opID))
		if noticeErr != nil {
			return false, noticeErr
		}
		notice = prepared
	}
	settled, err := c.settleCombinedCheck(persistCtx, handle, integ, opID, outcome, evidence, retained, notice)
	if err != nil {
		return false, err
	}
	switch {
	case !retained:
		return false, fmt.Errorf("app: combined-check output retention failed: %w", captureErr)
	case settled == combinedCheckIntegrated:
		c.removeCheckout(ctx, handle, frozen.RepositoryRoot, checkoutPath)
		return false, nil
	case settled == combinedCheckFailed:
		// Failing combined check: the journaled reset runs as its own
		// operation on the next round.
		return false, nil
	default:
		// Evidence recorded under a held stop, or the execution settled by
		// a shutdown: the integration is that path's — the candidate is
		// rolled back, never integrated past it.
		return true, nil
	}
}

// settleUnspawnedCheck settles a check execution whose round ended before
// its spawn was dispatched — no process can have claimed it — failed as
// never spawned, and returns cause joined with any write failure. A
// settlement the store refuses (a lost lease) leaves the intent pending
// for its recovery row.
func (c *Controller) settleUnspawnedCheck(ctx context.Context, handle RunHandle, opID identity.OperationID, cause error) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per unspawned execution.
	if err := c.settleOperation(ctx, handle, opID, OperationFailed, fmt.Sprintf("never spawned: %v", cause)); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// runStopRequested reads the run's held-stop flag on its own, outside any
// settling transaction: a read-only look, never the authority for a
// decision. Every path that ACTS on a stop re-reads it inside the
// transaction that commits the act.
func (c *Controller) runStopRequested(ctx context.Context, handle RunHandle) (bool, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	stopRequested := false
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		r, _, runErr := uow.Runs().Get(ctx, handle.runID)
		if runErr != nil {
			return runErr
		}
		stopRequested = r.StopRequested
		return nil
	}); err != nil {
		return false, fmt.Errorf("app: read the run's stop request: %w", err)
	}
	return stopRequested, nil
}

// settleCombinedCheck records a combined-check execution's observed
// outcome, its evidence rows and the integration's consequence in one
// transaction, while the operation is still unresolved. An execution
// whose output was not retained fails the check whatever its exit code;
// otherwise a held stop leaves the integration to the stop path, a clean
// exit integrates the candidate (notice already prepared) and any other
// exit moves the integration to check-failed.
func (c *Controller) settleCombinedCheck(ctx context.Context, handle RunHandle, integ *run.Integration, opID identity.OperationID, outcome checkRunOutcome, evidence []run.Artifact, retained bool, notice controllerNotice) (combinedCheckSettlement, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per combined-check execution.
	now := c.Clock.Now()
	var settled combinedCheckSettlement
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "combined check outcome")
		if wfErr != nil {
			return wfErr
		}
		op, getErr := uow.Operations().Get(ctx, opID)
		if getErr != nil {
			return getErr
		}
		if op.State != OperationPending && op.State != OperationReconciling {
			settled = combinedCheckSettledElsewhere
			return nil
		}
		for i := range evidence {
			if err := uow.Artifacts().Save(ctx, evidence[i]); err != nil {
				return err
			}
		}
		passed := retained && outcome.ExitCode == 0 && !outcome.Unknown
		op.State = OperationFailed
		if passed {
			op.State = OperationSucceeded
		}
		op.Outcome = outcome
		op.UpdatedAt = now
		if err := uow.Operations().Save(ctx, op); err != nil {
			return err
		}
		if !retained {
			settled = combinedCheckFailed
			return failCheckLocked(ctx, uow, wf, handle, integ, fmt.Sprintf("combined-check output retention failed (operation %s)", opID), now)
		}
		r, _, runErr := uow.Runs().Get(ctx, handle.runID)
		if runErr != nil {
			return runErr
		}
		if r.StopRequested {
			settled = combinedCheckStopHeld
			return nil
		}
		if passed {
			settled = combinedCheckIntegrated
			return integrateLocked(ctx, uow, wf, handle, integ, notice, now)
		}
		settled = combinedCheckFailed
		return failCheckLocked(ctx, uow, wf, handle, integ, fmt.Sprintf("combined check failed with exit %d (operation %s)", outcome.ExitCode, opID), now)
	})
	if err != nil {
		return 0, fmt.Errorf("app: record combined-check outcome: %w", err)
	}
	return settled, nil
}

// failIntegrationCheck moves the integration checking -> check-failed.
func (c *Controller) failIntegrationCheck(ctx context.Context, handle RunHandle, integ *run.Integration, reason string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "combined check failed")
		if wfErr != nil {
			return wfErr
		}
		return failCheckLocked(ctx, uow, wf, handle, integ, reason, now)
	})
}

// failCheckLocked moves the integration checking -> check-failed inside
// the caller's transaction; an integration no longer checking is left
// as it is.
func failCheckLocked(ctx context.Context, uow UnitOfWork, wf WorkflowRepositories, handle RunHandle, integ *run.Integration, reason string, now time.Time) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	latest, rev, err := wf.Integrations().Get(ctx, integ.ID)
	if err != nil {
		return err
	}
	if latest.State != run.IntegrationChecking {
		return nil
	}
	failed, err := latest.FailCheck(now)
	if err != nil {
		return err
	}
	if _, err := wf.Integrations().Save(ctx, failed, rev); err != nil {
		return err
	}
	return recordTransition(ctx, uow, EntityTask, integ.TaskID.String(), string(run.TaskIntegrating), string(run.TaskIntegrating), reason, gen(handle.lease.Generation), now)
}
