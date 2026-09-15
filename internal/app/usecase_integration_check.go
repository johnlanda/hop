package app

import (
	"context"
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// runIntegrationCheck drives one combined-candidate check execution: the
// generalized check pipeline's integration subject (docs/plan/
// phase-3-design.md section 8). Its own detached checkout under the
// check operation's directory — a scratch merge tree is never the check
// target — the frozen argv and timeout, mandatory evidence retention,
// and an outcome transaction dispatching by the integration/feature row
// of the outcome table. interrupted reports that a stop request
// prevented new work or claimed the recorded outcome.
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

	actCtx, release := handle.actContext(ctx)
	defer release()
	if revErr := c.revalidateForDispatch(ctx, handle, false); revErr != nil {
		return false, fmt.Errorf("app: revalidate before combined-check checkout: %w", revErr)
	}
	if matErr := c.materializeCheckout(actCtx, frozen.RepositoryRoot, checkoutPath, integ.MergeCommitOID); matErr != nil {
		// No execution was ever spawned: the operation settles failed and
		// a later round runs a fresh execution against the same candidate.
		if settleErr := c.settleOperation(ctx, handle, opID, OperationFailed, fmt.Sprintf("combined-check checkout could not be materialized: %v", matErr)); settleErr != nil {
			return false, settleErr
		}
		return false, fmt.Errorf("app: materialize combined-check checkout: %w", matErr)
	}

	if revErr := c.revalidateForDispatch(ctx, handle, false); revErr != nil {
		return false, fmt.Errorf("app: revalidate before combined-check spawn: %w", revErr)
	}
	timeout := frozen.Snapshot.CheckTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	boundedCtx, cancel := context.WithTimeout(actCtx, timeout)
	cmdResult, runErr := c.Commands.Run(boundedCtx, Command{Argv: intent.SpawnArgv, Dir: checkoutPath, Env: withHOPStateDir(spawnEnv, frozen.Snapshot.StateRoot)})
	cancel()

	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), checkPersistenceTimeout)
	defer persistCancel()

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
	if captureErr != nil {
		// Retention is mandatory: completion may never claim evidence that
		// was lost. The execution fails; the checkout stays inspectable.
		if settleErr := c.settleCombinedCheckOutcome(persistCtx, handle, opID, checkRunOutcome{ExitCode: cmdResult.ExitCode, Detail: fmt.Sprintf("evidence retention failed: %v", captureErr)}, evidence); settleErr != nil {
			return false, settleErr
		}
		return false, fmt.Errorf("app: combined-check output retention failed: %w", captureErr)
	}

	stopHeld, err := c.settleCombinedCheckRecorded(persistCtx, handle, opID, checkRunOutcome{ExitCode: cmdResult.ExitCode}, evidence)
	if err != nil {
		return false, err
	}
	if stopHeld {
		// Evidence recorded; the integration is the stop path's: the
		// candidate is rolled back, the task interrupted — never
		// integrated past a stop.
		return true, nil
	}

	if cmdResult.ExitCode == 0 {
		if err := c.settleIntegrationIntegrated(ctx, handle, frozen, integ, fmt.Sprintf("combined check passed (operation %s)", opID)); err != nil {
			return false, err
		}
		c.removeCheckout(ctx, handle, frozen.RepositoryRoot, checkoutPath)
		return false, nil
	}
	// Failing combined check: integration checking -> check-failed; the
	// journaled reset runs as its own operation on the next round.
	if err := c.failIntegrationCheck(persistCtx, handle, integ, fmt.Sprintf("combined check failed with exit %d (operation %s)", cmdResult.ExitCode, opID)); err != nil {
		return false, err
	}
	return false, nil
}

// settleCombinedCheckOutcome records a combined-check operation's outcome
// and evidence rows without touching the integration.
func (c *Controller) settleCombinedCheckOutcome(ctx context.Context, handle RunHandle, opID identity.OperationID, outcome checkRunOutcome, evidence []run.Artifact) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		for i := range evidence {
			if err := uow.Artifacts().Save(ctx, evidence[i]); err != nil {
				return err
			}
		}
		op, getErr := uow.Operations().Get(ctx, opID)
		if getErr != nil {
			return getErr
		}
		op.State = OperationFailed
		op.Outcome = outcome
		op.UpdatedAt = now
		return uow.Operations().Save(ctx, op)
	})
}

// settleCombinedCheckRecorded records a combined-check operation's
// observed outcome and evidence, reporting whether a stop request was
// held in that same transaction (in which case the integration is left
// untouched for the stop path).
func (c *Controller) settleCombinedCheckRecorded(ctx context.Context, handle RunHandle, opID identity.OperationID, outcome checkRunOutcome, evidence []run.Artifact) (bool, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	now := c.Clock.Now()
	stopHeld := false
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		for i := range evidence {
			if err := uow.Artifacts().Save(ctx, evidence[i]); err != nil {
				return err
			}
		}
		r, _, runErr := uow.Runs().Get(ctx, handle.runID)
		if runErr != nil {
			return runErr
		}
		stopHeld = r.StopRequested
		op, getErr := uow.Operations().Get(ctx, opID)
		if getErr != nil {
			return getErr
		}
		if outcome.ExitCode == 0 && !outcome.Unknown {
			op.State = OperationSucceeded
		} else {
			op.State = OperationFailed
		}
		op.Outcome = outcome
		op.UpdatedAt = now
		return uow.Operations().Save(ctx, op)
	})
	return stopHeld, err
}

// failIntegrationCheck moves the integration checking -> check-failed.
func (c *Controller) failIntegrationCheck(ctx context.Context, handle RunHandle, integ *run.Integration, reason string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "combined check failed")
		if wfErr != nil {
			return wfErr
		}
		latest, rev, getErr := wf.Integrations().Get(ctx, integ.ID)
		if getErr != nil {
			return getErr
		}
		if latest.State != run.IntegrationChecking {
			return nil
		}
		failed, trErr := latest.FailCheck(now)
		if trErr != nil {
			return trErr
		}
		if _, saveErr := wf.Integrations().Save(ctx, failed, rev); saveErr != nil {
			return saveErr
		}
		return recordTransition(ctx, uow, EntityTask, integ.TaskID.String(), string(run.TaskIntegrating), string(run.TaskIntegrating), reason, gen(handle.lease.Generation), now)
	})
}
