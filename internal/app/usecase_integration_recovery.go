package app

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// recoverIntegrationOperations resolves every unresolved integration
// operation — merge, publish, reset, fence and integration-subject check
// executions — per the section 4 decision-table rows, BEFORE any new
// integration work. blocked names an operation still unresolved after
// the round: new integration work is refused while it stands (the
// serial index already prevents a second integration; an unresolved
// intent blocks even the current one's next step).
func (c *Controller) recoverIntegrationOperations(ctx context.Context, handle RunHandle, frozen *FrozenRun, hopPath string, spawnEnv []string) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per integration round.
	var unresolved []Operation
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		ops, opErr := uow.Operations().Pending(ctx, handle.runID)
		if opErr != nil {
			return opErr
		}
		for i := range ops {
			switch ops[i].Kind {
			case OpIntegrationMerge, OpIntegrationPublish, OpIntegrationReset, OpIntegrationFence:
				unresolved = append(unresolved, ops[i])
			case OpCheckRun:
				if intent, ok := decodeOperationPayload[integrationCheckIntent](ops[i].Intent); ok && intent.IntegrationID != "" {
					unresolved = append(unresolved, ops[i])
				}
			}
		}
		return nil
	}); err != nil {
		return "", err
	}

	blocked := ""
	note := func(still string) {
		if still != "" && blocked == "" {
			blocked = still
		}
	}
	for i := range unresolved {
		// Re-read at each turn: recovering an earlier operation can
		// settle a later one (a fence settles the publish it retires),
		// and a stale pending snapshot must never re-drive it.
		op, opErr := c.currentOperation(ctx, handle, unresolved[i].ID)
		if opErr != nil {
			return "", opErr
		}
		if op.State != OperationPending && op.State != OperationReconciling {
			continue
		}
		var (
			still string
			err   error
		)
		switch op.Kind {
		case OpIntegrationMerge:
			still, err = c.recoverIntegrationMerge(ctx, handle, &op)
		case OpIntegrationPublish:
			still, err = c.recoverIntegrationPublish(ctx, handle, frozen, &op)
		case OpIntegrationReset:
			still, err = c.recoverIntegrationReset(ctx, handle, frozen, &op)
		case OpIntegrationFence:
			still, err = c.recoverIntegrationFence(ctx, handle, frozen, &op)
		case OpCheckRun:
			still, err = c.recoverIntegrationCheck(ctx, handle, frozen, &op)
		}
		if err != nil {
			return "", err
		}
		note(still)
	}
	_ = hopPath
	_ = spawnEnv
	return blocked, nil
}

// recoverIntegrationMerge resolves one unresolved integration.merge per
// its decision-table row. With no claim: a directory without the
// recorded materialization-complete evidence settles the operation
// failed (the D2 barrier — the ambiguous directory is abandoned, its
// possible surviving preparer confined to it, and a re-act is a NEW
// operation with a fresh path); with the evidence recorded, the missing
// claim is ambiguous for the bounded wait, then reconciling — never
// absence. With a claim: the group is retired FIRST, the operation
// settles with the retirement evidence, THEN the scratch HEAD decides —
// adopt M (parents exactly {pre-merge head, source}), adopt the no-op,
// or settle failed so a fresh operation re-acts.
func (c *Controller) recoverIntegrationMerge(ctx context.Context, handle RunHandle, op *Operation) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per unresolved merge.
	intent, ok := decodeOperationPayload[integrationMergeIntent](op.Intent)
	if !ok {
		if err := c.markOperationReconciling(ctx, handle, op.ID, "integration.merge intent could not be decoded; failing closed"); err != nil {
			return "", err
		}
		return fmt.Sprintf("integration.merge %s has an undecodable intent; failing closed", op.ID), nil
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
		return "", fmt.Errorf("app: load merge exec claim: %w", err)
	}

	if !found {
		materialized, hasEvidence := decodeOperationPayload[integrationMergeMaterialized](op.ActEvidence)
		if !hasEvidence || materialized.TreePath == "" {
			// The D2 materialization barrier: HEAD may already read
			// correctly in a directory a surviving preparer is still
			// writing, so without the recorded completion evidence the
			// directory is never adopted and never entered.
			if err := c.settleOperation(ctx, handle, op.ID, OperationFailed, "materialization completion was never recorded; the directory is abandoned and a fresh operation re-acts"); err != nil {
				return "", err
			}
			return "", nil
		}
		// The bounded wait is enforced against the intent's durable
		// CreatedAt; recording it as act evidence (the Phase 2 rule for
		// worktree/pane rows) would clobber the materialization-complete
		// record, which IS this operation's act evidence.
		if c.Clock.Now().Sub(op.CreatedAt) <= CheckClaimDeadline {
			return fmt.Sprintf("integration.merge %s has no claim yet; within the bounded wait", op.ID), nil
		}
		if err := c.markOperationReconciling(ctx, handle, op.ID, "no merge exec claim within the bounded wait; ambiguous, never absence"); err != nil {
			return "", err
		}
		return fmt.Sprintf("integration.merge %s has no claim past the bounded wait; reconciling", op.ID), nil
	}

	outcome, retireErr := c.retireGroup(ctx, handle, claim.PID, [][]string{intent.MergeArgv, intent.SpawnArgv})
	if retireErr != nil {
		return "", retireErr
	}
	switch outcome {
	case GroupEmpty:
		return c.adoptSettledMergeTree(ctx, handle, op, &intent)
	case GroupMatched:
		return fmt.Sprintf("merge group %d signaled; awaiting observed absence", claim.PID), nil
	case GroupMismatched:
		if err := c.markOperationReconciling(ctx, handle, op.ID, "merge group members do not match the recorded argv; failing closed"); err != nil {
			return "", err
		}
		return fmt.Sprintf("merge group %d does not match the recorded argv; failing closed", claim.PID), nil
	default: // GroupInspectionFailed
		return fmt.Sprintf("merge group %d could not be inspected; failing closed", claim.PID), nil
	}
}

// adoptSettledMergeTree settles a retired merge operation by reading the
// scratch tree: the operation's outcome records what the tree proves,
// and a tree proving nothing settles the operation failed so a NEW
// operation (fresh ID, claim, argv record and tree path) re-acts.
func (c *Controller) adoptSettledMergeTree(ctx context.Context, handle RunHandle, op *Operation, intent *integrationMergeIntent) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	classified, classifyErr := c.classifyMergeSuccess(ctx, intent.TreePath, intent.PremergeOID, intent.SourceOID)
	if classifyErr != nil {
		// The inspection itself failed: ambiguous, never a settlement.
		return fmt.Sprintf("integration.merge %s group retired but the scratch tree could not be inspected: %v", op.ID, classifyErr), nil
	}
	now := c.Clock.Now()
	switch classified.Result {
	case mergeResultMerged:
		return "", c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
			latest, getErr := uow.Operations().Get(ctx, op.ID)
			if getErr != nil {
				return getErr
			}
			if latest.State != OperationPending && latest.State != OperationReconciling {
				return nil
			}
			latest.State = OperationSucceeded
			latest.Outcome = classified
			latest.UpdatedAt = now
			return uow.Operations().Save(ctx, latest)
		})
	case mergeResultNoOp:
		return "", c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
			wf, wfErr := RequireWorkflowRepositories(uow, "merge no-op adoption")
			if wfErr != nil {
				return wfErr
			}
			latest, getErr := uow.Operations().Get(ctx, op.ID)
			if getErr != nil {
				return getErr
			}
			if latest.State != OperationPending && latest.State != OperationReconciling {
				return nil
			}
			latest.State = OperationSucceeded
			latest.Outcome = classified
			latest.UpdatedAt = now
			if saveErr := uow.Operations().Save(ctx, latest); saveErr != nil {
				return saveErr
			}
			integ, rev, getErr := wf.Integrations().Get(ctx, identity.IntegrationID(intent.IntegrationID))
			if getErr != nil {
				return getErr
			}
			if integ.State != run.IntegrationMerging {
				return nil
			}
			checking, trErr := integ.EnterChecking(intent.PremergeOID, now)
			if trErr != nil {
				return trErr
			}
			_, saveErr := wf.Integrations().Save(ctx, checking, rev)
			return saveErr
		})
	default:
		return "", c.settleOperation(ctx, handle, op.ID, OperationFailed, "retired merge tree proves no adoptable outcome: "+classified.Detail)
	}
}

// recoverIntegrationPublish resolves one unresolved integration.publish
// per its row: ref == the candidate → adopt; ref == the expected old
// value → re-act the SAME operation (retry-idempotent CAS); any other
// value → reconciling with the observed ref as evidence (the ref-fencing
// rule retires it before any terminal report).
func (c *Controller) recoverIntegrationPublish(ctx context.Context, handle RunHandle, frozen *FrozenRun, op *Operation) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	intent, ok := decodeOperationPayload[integrationPublishIntent](op.Intent)
	if !ok {
		if err := c.markOperationReconciling(ctx, handle, op.ID, "integration.publish intent could not be decoded; failing closed"); err != nil {
			return "", err
		}
		return fmt.Sprintf("integration.publish %s has an undecodable intent; failing closed", op.ID), nil
	}
	observed, obsErr := c.runGit(ctx, frozen.RepositoryRoot, "rev-parse", "--verify", intent.Ref)
	if obsErr != nil {
		return fmt.Sprintf("integration.publish %s: the ref could not be observed: %v", op.ID, obsErr), nil
	}
	switch observed {
	case intent.NewOID:
		return "", c.adoptPublishOutcome(ctx, handle, op.ID, &intent)
	case intent.ExpectedOldOID:
		return "", c.actAndSettlePublish(ctx, handle, frozen, op.ID, &intent)
	default:
		if err := c.markOperationReconciling(ctx, handle, op.ID, fmt.Sprintf("ref rests at %s, neither the candidate nor the expected old value", observed)); err != nil {
			return "", err
		}
		return fmt.Sprintf("integration.publish %s: ref rests at %s; reconciling", op.ID, observed), nil
	}
}

// adoptPublishOutcome settles a publish whose CAS is observed to have
// landed: operation succeeded, integration into checking.
func (c *Controller) adoptPublishOutcome(ctx context.Context, handle RunHandle, opID identity.OperationID, intent *integrationPublishIntent) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "publish adoption")
		if wfErr != nil {
			return wfErr
		}
		op, getErr := uow.Operations().Get(ctx, opID)
		if getErr != nil {
			return getErr
		}
		if op.State == OperationPending || op.State == OperationReconciling {
			op.State = OperationSucceeded
			op.Outcome = "published " + intent.NewOID + " (adopted by observation)"
			op.UpdatedAt = now
			if saveErr := uow.Operations().Save(ctx, op); saveErr != nil {
				return saveErr
			}
		}
		integ, rev, getErr := wf.Integrations().Get(ctx, identity.IntegrationID(intent.IntegrationID))
		if getErr != nil {
			return getErr
		}
		if integ.State != run.IntegrationMerging {
			return nil
		}
		checking, trErr := integ.EnterChecking(intent.NewOID, now)
		if trErr != nil {
			return trErr
		}
		_, saveErr := wf.Integrations().Save(ctx, checking, rev)
		return saveErr
	})
}

// recoverIntegrationReset resolves one unresolved integration.reset per
// its row: R recorded and ref == R → adopt; R recorded and ref == the
// rejected candidate → re-act step (ii) only; no R recorded and ref ==
// the rejected candidate → re-act from step (i); anything else →
// reconciling (the ref-fencing retirement completes it before any
// terminal report).
func (c *Controller) recoverIntegrationReset(ctx context.Context, handle RunHandle, frozen *FrozenRun, op *Operation) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	intent, ok := decodeOperationPayload[integrationResetIntent](op.Intent)
	if !ok {
		if err := c.markOperationReconciling(ctx, handle, op.ID, "integration.reset intent could not be decoded; failing closed"); err != nil {
			return "", err
		}
		return fmt.Sprintf("integration.reset %s has an undecodable intent; failing closed", op.ID), nil
	}
	evidence, _ := decodeOperationPayload[integrationResetEvidence](op.ActEvidence)
	observed, obsErr := c.runGit(ctx, frozen.RepositoryRoot, "rev-parse", "--verify", intent.Ref)
	if obsErr != nil {
		return fmt.Sprintf("integration.reset %s: the ref could not be observed: %v", op.ID, obsErr), nil
	}
	switch {
	case evidence.RollbackOID != "" && observed == evidence.RollbackOID:
		return "", c.settleIntegrationTerminal(ctx, handle, frozen, identity.IntegrationID(intent.IntegrationID), integrationSettlement{
			OpID: op.ID, OpState: OperationSucceeded,
			OpOutcome:   fmt.Sprintf("ref reset to rollback commit %s (adopted by observation)", evidence.RollbackOID),
			TargetState: run.IntegrationRolledBack,
			Reason:      intent.Reason,
		})
	case evidence.RollbackOID != "" && observed == intent.RejectedOID:
		return c.actAndSettleReset(ctx, handle, frozen, op.ID, &intent, op.CreatedAt, intent.RejectedOID, evidence.RollbackOID)
	case evidence.RollbackOID == "" && observed == intent.RejectedOID:
		// A prior orphaned commit-tree object is unreferenced and
		// harmless; re-act from step (i).
		return c.actAndSettleReset(ctx, handle, frozen, op.ID, &intent, op.CreatedAt, intent.RejectedOID, "")
	default:
		if err := c.markOperationReconciling(ctx, handle, op.ID, fmt.Sprintf("ref rests at %s; reset recovery is not decidable in place", observed)); err != nil {
			return "", err
		}
		return fmt.Sprintf("integration.reset %s: ref rests at %s; reconciling", op.ID, observed), nil
	}
}

// recoverIntegrationFence resolves one unresolved integration.fence per
// its own row (the reset row's shape): ref == the persisted F → adopt;
// ref == the recorded head → re-act; else reconciling.
func (c *Controller) recoverIntegrationFence(ctx context.Context, handle RunHandle, frozen *FrozenRun, op *Operation) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	intent, ok := decodeOperationPayload[integrationFenceIntent](op.Intent)
	if !ok {
		if err := c.markOperationReconciling(ctx, handle, op.ID, "integration.fence intent could not be decoded; failing closed"); err != nil {
			return "", err
		}
		return fmt.Sprintf("integration.fence %s has an undecodable intent; failing closed", op.ID), nil
	}
	evidence, _ := decodeOperationPayload[integrationFenceEvidence](op.ActEvidence)
	observed, obsErr := c.runGit(ctx, frozen.RepositoryRoot, "rev-parse", "--verify", intent.Ref)
	if obsErr != nil {
		return fmt.Sprintf("integration.fence %s: the ref could not be observed: %v", op.ID, obsErr), nil
	}
	switch {
	case evidence.FenceOID != "" && observed == evidence.FenceOID:
		return "", c.settleFenceOutcome(ctx, handle, op.ID, &intent, evidence.FenceOID)
	case observed == intent.ObservedHeadOID:
		return c.actAndSettleFence(ctx, handle, frozen, op.ID, &intent, op.CreatedAt, evidence.FenceOID)
	default:
		// The losing-fence race: the zombie the fence meant to retire won
		// instead — the ref reads as the RETIRED PUBLISH intent's own
		// candidate. The fence settles failed through its own row (its
		// purpose is moot: the expected-old it wanted to burn is gone) and
		// the publish is adopted by observation, handing the published
		// candidate to the standard rollback path. Each operation keeps
		// its own evidence.
		if resolved, zombieErr := c.adoptZombiePublishAfterFence(ctx, handle, op.ID, &intent, observed); zombieErr != nil {
			return "", zombieErr
		} else if resolved {
			return "", nil
		}
		if err := c.markOperationReconciling(ctx, handle, op.ID, fmt.Sprintf("ref rests at %s, neither the fence nor the recorded head", observed)); err != nil {
			return "", err
		}
		return fmt.Sprintf("integration.fence %s: ref rests at %s; reconciling", op.ID, observed), nil
	}
}

// adoptZombiePublishAfterFence resolves the losing-fence race: when the
// observed ref equals the retired publish intent's candidate, the zombie
// CAS won — the fence settles failed and the publish is adopted, so the
// integration enters checking on the published candidate and the
// shutdown paths roll it back through the standard reset. resolved is
// false when the observed ref is not the retired intent's candidate.
func (c *Controller) adoptZombiePublishAfterFence(ctx context.Context, handle RunHandle, fenceOpID identity.OperationID, intent *integrationFenceIntent, observed string) (bool, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	if intent.RetiredOperationID == "" {
		return false, nil
	}
	var retiredIntent integrationPublishIntent
	decoded := false
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		retired, getErr := uow.Operations().Get(ctx, identity.OperationID(intent.RetiredOperationID))
		if getErr != nil {
			return getErr
		}
		if retired.Kind != OpIntegrationPublish {
			return nil
		}
		retiredIntent, decoded = decodeOperationPayload[integrationPublishIntent](retired.Intent)
		return nil
	}); err != nil {
		return false, err
	}
	if !decoded || retiredIntent.NewOID != observed {
		return false, nil
	}
	if err := c.settleOperation(ctx, handle, fenceOpID, OperationFailed, fmt.Sprintf("the zombie publish won the race to %s; the fence's expected-old is gone and the standard rollback path retires the published candidate", observed)); err != nil {
		return false, err
	}
	if err := c.adoptPublishOutcome(ctx, handle, identity.OperationID(intent.RetiredOperationID), &retiredIntent); err != nil {
		return false, err
	}
	return true, nil
}

// recoverIntegrationCheck resolves one unresolved combined-check
// execution: claim absent → bounded wait then reconciling; claim present
// → group retirement, and on confirmed absence the unknown-outcome rule
// at integration scope — repeatable requeues a fresh execution against
// the same candidate head; unrepeatable settles the integration
// check-failed with the outcome unknown, so the reset runs and an
// unvalidated candidate never stays published. Stop precedence leaves
// the integration to the stop path.
func (c *Controller) recoverIntegrationCheck(ctx context.Context, handle RunHandle, frozen *FrozenRun, op *Operation) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	intent, ok := decodeOperationPayload[integrationCheckIntent](op.Intent)
	if !ok {
		if err := c.markOperationReconciling(ctx, handle, op.ID, "combined-check intent could not be decoded; failing closed"); err != nil {
			return "", err
		}
		return fmt.Sprintf("combined check %s has an undecodable intent; failing closed", op.ID), nil
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
		return "", fmt.Errorf("app: load combined-check exec claim: %w", err)
	}
	if !found {
		if c.Clock.Now().Sub(op.CreatedAt) <= CheckClaimDeadline {
			if err := c.recordCombinedCheckClaimWait(ctx, handle, op); err != nil {
				return "", err
			}
			return fmt.Sprintf("combined check %s has no claim yet; within the bounded wait", op.ID), nil
		}
		if err := c.markOperationReconciling(ctx, handle, op.ID, "no check-exec claim within the bounded wait; ambiguous, never absence"); err != nil {
			return "", err
		}
		return fmt.Sprintf("combined check %s has no claim past the bounded wait; reconciling", op.ID), nil
	}
	outcome, retireErr := c.retireGroup(ctx, handle, claim.PID, [][]string{intent.CheckArgv, intent.SpawnArgv})
	if retireErr != nil {
		return "", retireErr
	}
	switch outcome {
	case GroupEmpty:
		return "", c.applyIntegrationCheckUnknown(ctx, handle, frozen, op, &intent)
	case GroupMatched:
		return fmt.Sprintf("combined-check group %d signaled; awaiting observed absence", claim.PID), nil
	case GroupMismatched:
		if err := c.markOperationReconciling(ctx, handle, op.ID, "combined-check group members do not match the recorded argv; failing closed"); err != nil {
			return "", err
		}
		return fmt.Sprintf("combined-check group %d does not match the recorded argv; failing closed", claim.PID), nil
	default: // GroupInspectionFailed
		return fmt.Sprintf("combined-check group %d could not be inspected; failing closed", claim.PID), nil
	}
}

// applyIntegrationCheckUnknown settles a retired combined-check execution
// under the unknown-outcome rule at integration scope.
func (c *Controller) applyIntegrationCheckUnknown(ctx context.Context, handle RunHandle, frozen *FrozenRun, op *Operation, intent *integrationCheckIntent) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	now := c.Clock.Now()
	failIntegration := false
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "combined-check unknown outcome")
		if wfErr != nil {
			return wfErr
		}
		latest, getErr := uow.Operations().Get(ctx, op.ID)
		if getErr != nil {
			return getErr
		}
		if latest.State != OperationPending && latest.State != OperationReconciling {
			return nil
		}
		latest.State = OperationFailed
		latest.Outcome = checkRunOutcome{Unknown: true, Detail: "process group retired after takeover; result unknown"}
		latest.UpdatedAt = now
		if saveErr := uow.Operations().Save(ctx, latest); saveErr != nil {
			return saveErr
		}
		r, _, runErr := uow.Runs().Get(ctx, handle.runID)
		if runErr != nil {
			return runErr
		}
		if r.StopRequested || frozen.Snapshot.CheckRepeatable {
			// Repeatable: a fresh execution against the same candidate on
			// the next round. Stop: the published candidate is the stop
			// path's to roll back.
			return nil
		}
		integ, rev, getErr := wf.Integrations().Get(ctx, identity.IntegrationID(intent.IntegrationID))
		if getErr != nil {
			return getErr
		}
		if integ.State != run.IntegrationChecking {
			return nil
		}
		failed, trErr := integ.FailCheck(now)
		if trErr != nil {
			return trErr
		}
		if _, saveErr := wf.Integrations().Save(ctx, failed, rev); saveErr != nil {
			return saveErr
		}
		failIntegration = true
		return nil
	})
	_ = failIntegration
	return err
}

// retireUnresolvedRefIntents applies the ref-fencing rule: before stop, a
// takeover or a terminal-failure settlement reports any terminal state
// while an UNRESOLVED ref-move intent exists, it first retires that
// intent by moving the ref itself. Retirement distinguishes the intent
// kind — the invariant is "the ref rests on the last VALIDATED tree",
// not "the ref moved". Dependencies recover FIRST: every unresolved
// fence row (each one names the publish it retires) is recovered before
// any publish is visited, so a transiently failed fence resumes its own
// journal row rather than a second fence being allocated for the same
// intent — one fence identity per retired operation. Each operation is
// re-read at its turn, because recovering an earlier intent settles
// dependents (a landed fence settles its publish) and a stale pending
// snapshot must never re-drive a settled operation. outstanding is
// non-empty while any ref-move intent stays unresolved; a run never
// reports stopped or failed over it.
func (c *Controller) retireUnresolvedRefIntents(ctx context.Context, handle RunHandle, frozen *FrozenRun) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per stop or terminal-failure round.
	var refOps []Operation
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		ops, opErr := uow.Operations().Pending(ctx, handle.runID)
		if opErr != nil {
			return opErr
		}
		for i := range ops {
			switch ops[i].Kind {
			case OpIntegrationPublish, OpIntegrationReset, OpIntegrationFence:
				refOps = append(refOps, ops[i])
			}
		}
		return nil
	}); err != nil {
		return "", err
	}
	rank := map[OperationKind]int{OpIntegrationFence: 0, OpIntegrationReset: 1, OpIntegrationPublish: 2}
	slices.SortStableFunc(refOps, func(a, b Operation) int { return rank[a.Kind] - rank[b.Kind] })

	outstanding := ""
	for i := range refOps {
		op, opErr := c.currentOperation(ctx, handle, refOps[i].ID)
		if opErr != nil {
			return "", opErr
		}
		if op.State != OperationPending && op.State != OperationReconciling {
			continue // settled by an earlier intent's recovery this round.
		}
		var (
			still string
			err   error
		)
		switch op.Kind {
		case OpIntegrationPublish:
			still, err = c.retirePublishIntent(ctx, handle, frozen, &op)
		case OpIntegrationReset:
			still, err = c.retireResetIntent(ctx, handle, frozen, &op)
		case OpIntegrationFence:
			still, err = c.recoverIntegrationFence(ctx, handle, frozen, &op)
		}
		if err != nil {
			return "", err
		}
		if still != "" && outstanding == "" {
			outstanding = still
		}
	}
	return outstanding, nil
}

// currentOperation re-reads one operation row: dispatch loops that hold
// a snapshot re-read each row at its turn so a settlement made earlier
// in the round is seen, never re-driven.
func (c *Controller) currentOperation(ctx context.Context, handle RunHandle, opID identity.OperationID) (Operation, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	var op Operation
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		var getErr error
		op, getErr = uow.Operations().Get(ctx, opID)
		return getErr
	})
	return op, err
}

// unresolvedFenceFor finds the unresolved integration.fence operation
// retiring publish opID, if one exists — the oldest such row, so
// recovery always resumes the same journal identity.
func (c *Controller) unresolvedFenceFor(ctx context.Context, handle RunHandle, publishOpID identity.OperationID) (Operation, bool, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	var (
		fence Operation
		found bool
	)
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		ops, opErr := uow.Operations().ByKind(ctx, handle.runID, OpIntegrationFence)
		if opErr != nil {
			return opErr
		}
		for i := range ops {
			if ops[i].State != OperationPending && ops[i].State != OperationReconciling {
				continue
			}
			intent, ok := decodeOperationPayload[integrationFenceIntent](ops[i].Intent)
			if !ok || intent.RetiredOperationID != publishOpID.String() {
				continue
			}
			if !found || ops[i].CreatedAt.Before(fence.CreatedAt) {
				fence = ops[i]
				found = true
			}
		}
		return nil
	})
	return fence, found, err
}

// retirePublishIntent retires one unresolved publish intent. Head still
// at the expected old value (the paused-publish window the CAS cannot
// see): publish a fencing commit F carrying the CURRENT head's tree with
// the head as parent — content unchanged, expected-old value burned —
// through integration.fence's own journaled operation. Head at the
// candidate: the zombie landed; the outcome is adopted and the standard
// rollback path retires the now-published candidate. Head anywhere else:
// the expected-old is stale, the CAS can never land, and the intent
// settles by that observation.
func (c *Controller) retirePublishIntent(ctx context.Context, handle RunHandle, frozen *FrozenRun, op *Operation) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	intent, ok := decodeOperationPayload[integrationPublishIntent](op.Intent)
	if !ok {
		return fmt.Sprintf("integration.publish %s has an undecodable intent; failing closed", op.ID), nil
	}
	observed, obsErr := c.runGit(ctx, frozen.RepositoryRoot, "rev-parse", "--verify", intent.Ref)
	if obsErr != nil {
		return fmt.Sprintf("integration.publish %s: the ref could not be observed: %v", op.ID, obsErr), nil
	}
	switch observed {
	case intent.NewOID:
		// The zombie won the race: the ref reads as its target. Adopt the
		// outcome; the caller's stop/rollback path retires the published
		// candidate through the standard reset.
		return "", c.adoptPublishOutcome(ctx, handle, op.ID, &intent)
	case intent.ExpectedOldOID:
		// Idempotence per retired operation: an unresolved fence already
		// standing for THIS publish (a transient commit-tree or CAS
		// failure left it journaled) is recovered through its own row and
		// persisted OID — a second fence is never allocated while one
		// stands, or the abandoned one could stay reconciling forever
		// against a ref the duplicate moved.
		existing, found, fenceErr := c.unresolvedFenceFor(ctx, handle, op.ID)
		if fenceErr != nil {
			return "", fenceErr
		}
		if found {
			return c.recoverIntegrationFence(ctx, handle, frozen, &existing)
		}
		return c.fencePublishIntent(ctx, handle, frozen, op, &intent, observed)
	default:
		return "", c.settleOperation(ctx, handle, op.ID, OperationFailed, fmt.Sprintf("expected-old %s is no longer current (ref at %s); the CAS is dead and the intent retired by observation", intent.ExpectedOldOID, observed))
	}
}

// fencePublishIntent journals and drives one integration.fence operation
// retiring a publish intent whose expected-old value is still current.
// outstanding is non-empty while the fence has not settled.
func (c *Controller) fencePublishIntent(ctx context.Context, handle RunHandle, frozen *FrozenRun, publishOp *Operation, publishIntent *integrationPublishIntent, observedHead string) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	fenceOpID, err := c.newOperationID()
	if err != nil {
		return "", err
	}
	intent := integrationFenceIntent{
		Ref: publishIntent.Ref, ObservedHeadOID: observedHead,
		RetiredOperationID: publishOp.ID.String(), RetiredOperationKind: string(publishOp.Kind),
	}
	now := c.Clock.Now()
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return uow.Operations().Create(ctx, Operation{
			ID: fenceOpID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpIntegrationFence, State: OperationPending, Intent: intent,
			CreatedAt: now, UpdatedAt: now,
		})
	}); err != nil {
		return "", fmt.Errorf("app: record integration.fence intent: %w", err)
	}
	return c.actAndSettleFence(ctx, handle, frozen, fenceOpID, &intent, now, "")
}

// actAndSettleFence performs a fence intent's two-step act — create F
// carrying the observed head's tree with the head as parent, persist its
// OID BEFORE the CAS (never only implied), then CAS — and settles the
// fence and the intent it retires. persistedF, when non-empty, is an
// already-recorded fence OID whose creation step is skipped. outstanding
// is non-empty when the fence was journaled reconciling instead of
// settling — a LOSING fence CAS is an unsettled result the caller must
// carry, never quiescence: the ref then holds whatever won the race
// (ordinarily the zombie's unvalidated candidate), and the fence's own
// recovery row resolves it on the next round.
func (c *Controller) actAndSettleFence(ctx context.Context, handle RunHandle, frozen *FrozenRun, opID identity.OperationID, intent *integrationFenceIntent, intentTime time.Time, persistedF string) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	fenceOID := persistedF
	if fenceOID == "" {
		if err := c.revalidateForDispatch(ctx, handle, true); err != nil {
			return "", fmt.Errorf("app: revalidate before fence commit: %w", err)
		}
		actCtx, release := handle.actContext(ctx)
		created, commitErr := c.runGitEnv(actCtx, frozen.RepositoryRoot, gitDeterministicCommitEnv(intentTime),
			"commit-tree", intent.ObservedHeadOID+"^{tree}", "-p", intent.ObservedHeadOID, "-m", "hop fence "+opID.String())
		release()
		if commitErr != nil {
			detail := fmt.Sprintf("fence commit could not be created: %v", commitErr)
			if err := c.markOperationReconciling(ctx, handle, opID, detail); err != nil {
				return "", err
			}
			return fmt.Sprintf("integration.fence %s: %s", opID, detail), nil
		}
		fenceOID = created
		if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
			op, getErr := uow.Operations().Get(ctx, opID)
			if getErr != nil {
				return getErr
			}
			op.ActEvidence = integrationFenceEvidence{FenceOID: fenceOID}
			op.UpdatedAt = c.Clock.Now()
			return uow.Operations().Save(ctx, op)
		}); err != nil {
			return "", fmt.Errorf("app: persist fence commit OID: %w", err)
		}
	}

	if err := c.revalidateForDispatch(ctx, handle, true); err != nil {
		return "", fmt.Errorf("app: revalidate before fence CAS: %w", err)
	}
	actCtx, release := handle.actContext(ctx)
	_, casErr := c.runGit(actCtx, frozen.RepositoryRoot, "update-ref", intent.Ref, fenceOID, intent.ObservedHeadOID)
	release()
	if casErr != nil {
		observed, obsErr := c.runGit(ctx, frozen.RepositoryRoot, "rev-parse", "--verify", intent.Ref)
		if obsErr != nil || observed != fenceOID {
			detail := fmt.Sprintf("fence CAS refused (%v); observed ref %q", casErr, observed)
			if err := c.markOperationReconciling(ctx, handle, opID, detail); err != nil {
				return "", err
			}
			return fmt.Sprintf("integration.fence %s: %s", opID, detail), nil
		}
	}
	return "", c.settleFenceOutcome(ctx, handle, opID, intent, fenceOID)
}

// settleFenceOutcome settles a landed fence and the ref-move intent it
// retires: the stale intent's expected-old value is burned, so its
// zombie CAS is dead at the ref store.
func (c *Controller) settleFenceOutcome(ctx context.Context, handle RunHandle, opID identity.OperationID, intent *integrationFenceIntent, fenceOID string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		op, getErr := uow.Operations().Get(ctx, opID)
		if getErr != nil {
			return getErr
		}
		if op.State == OperationPending || op.State == OperationReconciling {
			op.State = OperationSucceeded
			op.Outcome = fmt.Sprintf("ref fenced to %s; retired %s %s", fenceOID, intent.RetiredOperationKind, intent.RetiredOperationID)
			op.UpdatedAt = now
			if saveErr := uow.Operations().Save(ctx, op); saveErr != nil {
				return saveErr
			}
		}
		if intent.RetiredOperationID == "" {
			return nil
		}
		retired, getErr := uow.Operations().Get(ctx, identity.OperationID(intent.RetiredOperationID))
		if getErr != nil {
			return getErr
		}
		if retired.State != OperationPending && retired.State != OperationReconciling {
			return nil
		}
		retired.State = OperationFailed
		retired.Outcome = fmt.Sprintf("retired by integration.fence %s: the expected-old value was burned before any terminal report", opID)
		retired.UpdatedAt = now
		return uow.Operations().Save(ctx, retired)
	})
}

// retireResetIntent COMPLETES an unresolved reset rather than fencing
// over it: fencing with the rejected candidate's tree would PRESERVE the
// rejected content. The persisted R is adopted or its CAS re-driven when
// R was recorded and the ref cooperates; when the ref rests elsewhere,
// the retirement commit is rebuilt from the RECORDED VALIDATED PRE-MERGE
// TREE with the observed current head as parent, its OID persisted, then
// the CAS — the original reset operation settles adopted/completed by
// this recovery, never fenced over.
func (c *Controller) retireResetIntent(ctx context.Context, handle RunHandle, frozen *FrozenRun, op *Operation) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	intent, ok := decodeOperationPayload[integrationResetIntent](op.Intent)
	if !ok {
		return fmt.Sprintf("integration.reset %s has an undecodable intent; failing closed", op.ID), nil
	}
	evidence, _ := decodeOperationPayload[integrationResetEvidence](op.ActEvidence)
	observed, obsErr := c.runGit(ctx, frozen.RepositoryRoot, "rev-parse", "--verify", intent.Ref)
	if obsErr != nil {
		return fmt.Sprintf("integration.reset %s: the ref could not be observed: %v", op.ID, obsErr), nil
	}
	switch {
	case evidence.RollbackOID != "" && observed == evidence.RollbackOID:
		return "", c.settleIntegrationTerminal(ctx, handle, frozen, identity.IntegrationID(intent.IntegrationID), integrationSettlement{
			OpID: op.ID, OpState: OperationSucceeded,
			OpOutcome:   fmt.Sprintf("ref reset to rollback commit %s (adopted by observation)", evidence.RollbackOID),
			TargetState: run.IntegrationRolledBack,
			Reason:      intent.Reason,
		})
	case evidence.RollbackOID != "" && observed == intent.RejectedOID:
		return c.actAndSettleReset(ctx, handle, frozen, op.ID, &intent, op.CreatedAt, intent.RejectedOID, evidence.RollbackOID)
	case observed == intent.RejectedOID:
		return c.actAndSettleReset(ctx, handle, frozen, op.ID, &intent, op.CreatedAt, intent.RejectedOID, "")
	default:
		// The ref rests neither on the rejected candidate nor on the
		// recorded R: rebuild the retirement commit against the observed
		// head so the pre-merge content still wins.
		return c.actAndSettleReset(ctx, handle, frozen, op.ID, &intent, op.CreatedAt, observed, "")
	}
}

// recordCombinedCheckClaimWait persists a combined-check execution's
// bounded-wait window as act evidence, once — the Phase 2
// recordBoundedWait behavior, restated here for the operations this file
// owns.
func (c *Controller) recordCombinedCheckClaimWait(ctx context.Context, handle RunHandle, op *Operation) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per ambiguous round.
	if recorded, ok := decodeOperationPayload[boundedWaitEvidence](op.ActEvidence); ok && recorded.WaitingSince != "" {
		return nil
	}
	now := c.Clock.Now()
	evidence := boundedWaitEvidence{
		WaitingSince: op.CreatedAt.UTC().Format(time.RFC3339Nano),
		Deadline:     op.CreatedAt.Add(CheckClaimDeadline).UTC().Format(time.RFC3339Nano),
		Detail:       "no check-exec claim yet; the child may not have reached its pre-exec write",
	}
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		latest, getErr := uow.Operations().Get(ctx, op.ID)
		if getErr != nil {
			return getErr
		}
		if latest.State != OperationPending && latest.State != OperationReconciling {
			return nil
		}
		latest.ActEvidence = evidence
		latest.UpdatedAt = now
		return uow.Operations().Save(ctx, latest)
	})
}
