package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// FeatureCheckReport is one DriveFeatureChecks round's outcome.
type FeatureCheckReport struct {
	Ran         bool
	TaskID      string
	Passed      bool
	Interrupted bool
	// Blocked names an unresolved prior execution refusing new check work.
	Blocked string
}

// DriveFeatureChecks drives the per-task (result-subject) check pipeline
// for a feature-mode run: the generalized pipeline's `result, feature`
// outcome row (docs/plan/phase-3-design.md section 8) — a passing check
// completes the attempt and task while the RUN STAYS RUNNING
// (integration is a separate later claim); a failing check fails the
// attempt and moves the task needs-rework or, at the exhausted retry
// limit, directly to failed with its mailbox closed; never Run.Complete
// or Run.Fail directly from a check. Exec, capture, evidence retention,
// the frozen timeout and the unknown-outcome rule are shared verbatim
// with the solo pipeline; only the outcome dispatch differs by mode.
func (c *Controller) DriveFeatureChecks(ctx context.Context, handle RunHandle, hopPath string, spawnEnv []string) (FeatureCheckReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per scheduling pass.
	frozen, err := c.Read.LoadFrozenRun(ctx, handle.runID)
	if err != nil {
		return FeatureCheckReport{}, fmt.Errorf("app: load frozen run: %w", err)
	}
	if !frozen.Snapshot.Workflow.Feature() {
		return FeatureCheckReport{}, nil
	}

	blocked, err := c.recoverFeatureCheckExecutions(ctx, handle, &frozen)
	if err != nil {
		return FeatureCheckReport{}, err
	}
	if blocked != "" {
		return FeatureCheckReport{Blocked: blocked}, nil
	}

	task, attempt, result, found, err := c.nextCheckingTask(ctx, handle)
	if err != nil || !found {
		return FeatureCheckReport{}, err
	}
	report := FeatureCheckReport{TaskID: task.ID.String()}

	treeOID, err := c.runGit(ctx, frozen.RepositoryRoot, "rev-parse", result.CommitOID+"^{tree}")
	if err != nil {
		return report, fmt.Errorf("app: resolve candidate tree: %w", err)
	}
	claimed, opID, err := c.claimFeatureCheck(ctx, handle, &frozen, hopPath, &attempt, result, treeOID)
	if err != nil || !claimed {
		return report, err
	}
	report.Ran = true

	checkoutPath := checkExecutionCheckoutPath(frozen.Snapshot.StateRoot, handle.runID, opID)
	actCtx, release := handle.actContext(ctx)
	defer release()
	if err := c.revalidateForDispatch(ctx, handle, false); err != nil {
		return report, fmt.Errorf("app: revalidate before feature-check checkout: %w", err)
	}
	if rejectDetail, rejectErr := c.rejectSubmoduleCandidate(actCtx, frozen.RepositoryRoot, result.CommitOID); rejectErr != nil {
		return report, c.settleFeatureCheckOutcome(ctx, handle, &frozen, opID, &task, &attempt, result.ID, checkRunOutcome{Unknown: true, Detail: rejectErr.Error()}, nil)
	} else if rejectDetail != "" {
		return report, c.settleFeatureCheckOutcome(ctx, handle, &frozen, opID, &task, &attempt, result.ID, checkRunOutcome{ExitCode: 1, Detail: rejectDetail}, nil)
	}
	if err := c.revalidateForDispatch(ctx, handle, false); err != nil {
		return report, fmt.Errorf("app: revalidate before feature-check materialization: %w", err)
	}
	if err := c.materializeCheckout(actCtx, frozen.RepositoryRoot, checkoutPath, result.CommitOID); err != nil {
		return report, c.settleFeatureCheckOutcome(ctx, handle, &frozen, opID, &task, &attempt, result.ID, checkRunOutcome{Unknown: true, Detail: err.Error()}, nil)
	}

	if err := c.revalidateForDispatch(ctx, handle, false); err != nil {
		return report, fmt.Errorf("app: revalidate before feature-check spawn: %w", err)
	}
	timeout := frozen.Snapshot.CheckTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	boundedCtx, cancel := context.WithTimeout(actCtx, timeout)
	cmdResult, runErr := c.Commands.Run(boundedCtx, Command{Argv: checkSpawnArgv(hopPath, opID, frozen.Snapshot.CheckArgv), Dir: checkoutPath, Env: withHOPStateDir(spawnEnv, frozen.Snapshot.StateRoot)})
	cancel()

	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), checkPersistenceTimeout)
	defer persistCancel()

	evidence, captureErr := c.captureCheckOutputs(persistCtx, handle, &frozen, opID, result.ID, cmdResult)
	if runErr != nil {
		if len(evidence) > 0 {
			if saveErr := c.saveEvidenceRows(persistCtx, handle, evidence); saveErr != nil {
				return report, saveErr
			}
		}
		if markErr := c.markOperationReconciling(persistCtx, handle, opID, fmt.Sprintf("feature-check spawn returned an error before an outcome was observed: %v", runErr)); markErr != nil {
			return report, markErr
		}
		return report, fmt.Errorf("app: feature check execution ambiguous: %w", runErr)
	}
	if captureErr != nil {
		// Retention is mandatory: the execution fails rather than letting
		// completion claim lost evidence.
		return report, c.settleFeatureCheckOutcome(persistCtx, handle, &frozen, opID, &task, &attempt, result.ID, checkRunOutcome{ExitCode: cmdResult.ExitCode, Detail: fmt.Sprintf("evidence retention failed: %v", captureErr)}, evidence)
	}

	if err := c.settleFeatureCheckOutcome(persistCtx, handle, &frozen, opID, &task, &attempt, result.ID, checkRunOutcome{ExitCode: cmdResult.ExitCode}, evidence); err != nil {
		return report, err
	}
	report.Passed = cmdResult.ExitCode == 0
	if report.Passed {
		c.removeCheckout(ctx, handle, frozen.RepositoryRoot, checkoutPath)
	}
	return report, nil
}

// nextCheckingTask finds the lowest-seq implement task in checking with a
// requested check for its newest accepted result.
func (c *Controller) nextCheckingTask(ctx context.Context, handle RunHandle) (run.Task, run.Attempt, *run.Result, bool, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	var (
		task    run.Task
		attempt run.Attempt
		result  *run.Result
		found   bool
	)
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "DriveFeatureChecks")
		if wfErr != nil {
			return wfErr
		}
		tasks, taskErr := wf.TaskIndex().ByRun(ctx, handle.runID)
		if taskErr != nil {
			return taskErr
		}
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].Seq < tasks[j].Seq })
		for i := range tasks {
			if tasks[i].Kind != run.TaskKindImplement || tasks[i].State != run.TaskChecking {
				continue
			}
			a, res, resErr := newestAcceptedResult(ctx, uow, wf, tasks[i].ID)
			if resErr != nil {
				return resErr
			}
			if res == nil {
				continue
			}
			request, reqErr := uow.CheckRequests().Get(ctx, res.ID)
			if reqErr != nil {
				if errors.Is(reqErr, ErrNotFound) {
					continue
				}
				return reqErr
			}
			if request.State != CheckRequestRequested {
				continue
			}
			task = tasks[i]
			attempt = a
			result = res
			found = true
			return nil
		}
		return nil
	})
	return task, attempt, result, found, err
}

// claimFeatureCheck atomically claims the request together with its
// execution intent and the attempt's checking transition — the RUN is
// untouched: in feature mode an individual task's check never moves it.
func (c *Controller) claimFeatureCheck(ctx context.Context, handle RunHandle, frozen *FrozenRun, hopPath string, attempt *run.Attempt, result *run.Result, treeOID string) (bool, identity.OperationID, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	opID, err := c.newOperationID()
	if err != nil {
		return false, "", err
	}
	now := c.Clock.Now()
	claimed := false
	err = c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		r, _, getErr := uow.Runs().Get(ctx, handle.runID)
		if getErr != nil {
			return getErr
		}
		if r.StopRequested {
			return nil // unstarted work is never started after stop.
		}
		request, getErr := uow.CheckRequests().Get(ctx, result.ID)
		if getErr != nil {
			return getErr
		}
		if request.State != CheckRequestRequested {
			return nil
		}
		generation := handle.lease.Generation
		request.State = CheckRequestClaimed
		request.ClaimedGeneration = &generation
		if saveErr := uow.CheckRequests().Save(ctx, request); saveErr != nil {
			return saveErr
		}
		a, aRev, getErr := uow.Attempts().Get(ctx, attempt.ID)
		if getErr != nil {
			return getErr
		}
		if a.State == run.AttemptSubmitted {
			aFrom := a.State
			checking, trErr := a.EnterChecking(now)
			if trErr != nil {
				return trErr
			}
			if _, saveErr := uow.Attempts().Save(ctx, checking, aRev); saveErr != nil {
				return saveErr
			}
			if transErr := recordTransition(ctx, uow, EntityAttempt, a.ID.String(), string(aFrom), string(checking.State), "feature check execution started", gen(generation), now); transErr != nil {
				return transErr
			}
		}
		checkoutPath := checkExecutionCheckoutPath(frozen.Snapshot.StateRoot, handle.runID, opID)
		intent := CheckRunIntent{
			ResultID:     result.ID.String(),
			TreeOID:      treeOID,
			CheckoutPath: checkoutPath,
			CheckArgv:    frozen.Snapshot.CheckArgv,
			SpawnArgv:    checkSpawnArgv(hopPath, opID, frozen.Snapshot.CheckArgv),
		}
		if opErr := uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: generation,
			Kind: OpCheckRun, State: OperationPending, Intent: intent,
			CreatedAt: now, UpdatedAt: now,
		}); opErr != nil {
			return opErr
		}
		claimed = true
		return nil
	})
	if err != nil {
		return false, "", fmt.Errorf("app: claim feature check: %w", err)
	}
	return claimed, opID, nil
}

// settleFeatureCheckOutcome applies the `result, feature` outcome row.
func (c *Controller) settleFeatureCheckOutcome(ctx context.Context, handle RunHandle, frozen *FrozenRun, opID identity.OperationID, task *run.Task, attempt *run.Attempt, resultID identity.ResultID, outcome checkRunOutcome, evidence []run.Artifact) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per execution.
	// The unknown-plus-repeatable case requeues without touching the
	// entities; everything else settles through the task consequence.
	if outcome.Unknown && frozen.Snapshot.CheckRepeatable {
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
			op.UpdatedAt = c.Clock.Now()
			if saveErr := uow.Operations().Save(ctx, op); saveErr != nil {
				return saveErr
			}
			request, getErr := uow.CheckRequests().Get(ctx, resultID)
			if getErr != nil {
				return getErr
			}
			request.State = CheckRequestRequested
			request.ClaimedGeneration = nil
			return uow.CheckRequests().Save(ctx, request)
		})
	}

	for round := 0; round < settlementNoticeRetries; round++ {
		prediction, obligations, err := c.predictTaskConsequence(ctx, handle, frozen, task.ID, outcome.ExitCode == 0 && !outcome.Unknown)
		if err != nil {
			return err
		}
		var notice controllerNotice
		if prediction != "" {
			body := renderTaskNotice(task, prediction, describeCheckOutcome(&outcome), obligations)
			notice, err = c.prepareControllerNotice(ctx, handle, frozen.Snapshot.StateRoot, body)
			if err != nil {
				return err
			}
		}
		err = c.applyFeatureCheckSettlement(ctx, handle, frozen, opID, task, attempt, resultID, &outcome, evidence, prediction, obligations, notice)
		if errors.Is(err, errSettlementRetry) {
			continue
		}
		return err
	}
	return fmt.Errorf("app: task %s check settlement kept racing concurrent sends after %d attempts", task.ID, settlementNoticeRetries)
}

// describeCheckOutcome renders a check outcome for the manager notice.
func describeCheckOutcome(outcome *checkRunOutcome) string {
	switch {
	case outcome.Unknown:
		return "check outcome unknown: " + outcome.Detail
	case outcome.ExitCode == 0:
		return "check passed"
	default:
		reason := fmt.Sprintf("check failed with exit %d", outcome.ExitCode)
		if outcome.Detail != "" {
			reason += ": " + outcome.Detail
		}
		return reason
	}
}

// renderTaskNotice renders a task settlement's manager notice body.
func renderTaskNotice(task *run.Task, consequence taskConsequence, reason string, obligations []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "task t%d %s\n", task.Seq, consequence)
	fmt.Fprintf(&b, "reason: %s\n", reason)
	if consequence == taskConsequenceFailed {
		if len(obligations) == 0 {
			b.WriteString("orphaned obligations: none\n")
		} else {
			fmt.Fprintf(&b, "orphaned obligations: %s\n", strings.Join(obligations, " "))
		}
	}
	return b.String()
}

// predictTaskConsequence decides the task consequence a settlement's
// notice is composed for; "" for a passing outcome (no notice).
func (c *Controller) predictTaskConsequence(ctx context.Context, handle RunHandle, frozen *FrozenRun, taskID identity.TaskID, passed bool) (taskConsequence, []string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	if passed {
		return "", nil, nil
	}
	var (
		prediction  taskConsequence
		obligations []string
	)
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "predict task consequence")
		if wfErr != nil {
			return wfErr
		}
		r, _, runErr := uow.Runs().Get(ctx, handle.runID)
		if runErr != nil {
			return runErr
		}
		attempts, attErr := wf.AttemptIndex().ByTask(ctx, taskID)
		if attErr != nil {
			return attErr
		}
		prediction = decideTaskConsequence(r.StopRequested, len(attempts), retryLimitFor(&frozen.Snapshot))
		if prediction == taskConsequenceFailed {
			var oblErr error
			obligations, oblErr = c.pendingTaskObligations(ctx, wf, handle.runID, taskID)
			return oblErr
		}
		return nil
	})
	return prediction, obligations, err
}

// applyFeatureCheckSettlement is one settlement transaction attempt for a
// result-subject feature check.
func (c *Controller) applyFeatureCheckSettlement(ctx context.Context, handle RunHandle, frozen *FrozenRun, opID identity.OperationID, task *run.Task, attempt *run.Attempt, resultID identity.ResultID, outcome *checkRunOutcome, evidence []run.Artifact, prediction taskConsequence, obligations []string, notice controllerNotice) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "feature check settlement")
		if wfErr != nil {
			return wfErr
		}
		for i := range evidence {
			if err := uow.Artifacts().Save(ctx, evidence[i]); err != nil {
				return err
			}
		}
		op, getErr := uow.Operations().Get(ctx, opID)
		if getErr != nil {
			return getErr
		}
		if outcome.ExitCode == 0 && !outcome.Unknown {
			op.State = OperationSucceeded
		} else {
			op.State = OperationFailed
		}
		op.Outcome = *outcome
		op.UpdatedAt = now
		if saveErr := uow.Operations().Save(ctx, op); saveErr != nil {
			return saveErr
		}
		request, getErr := uow.CheckRequests().Get(ctx, resultID)
		if getErr != nil {
			return getErr
		}
		request.State = CheckRequestSettled
		if saveErr := uow.CheckRequests().Save(ctx, request); saveErr != nil {
			return saveErr
		}

		r, _, runErr := uow.Runs().Get(ctx, handle.runID)
		if runErr != nil {
			return runErr
		}
		t, tRev, taskErr := uow.Tasks().Get(ctx, task.ID)
		if taskErr != nil {
			return taskErr
		}
		a, aRev, attErr := uow.Attempts().Get(ctx, attempt.ID)
		if attErr != nil {
			return attErr
		}
		generation := gen(handle.lease.Generation)

		if r.StopRequested {
			// Stop precedence: evidence recorded; attempt and task
			// interrupted; the run is stop handling's.
			tFrom, aFrom := t.State, a.State
			tNext, trErr := t.Interrupt(now)
			if trErr != nil {
				return trErr
			}
			aNext, trErr := a.Interrupt(now)
			if trErr != nil {
				return trErr
			}
			if _, saveErr := uow.Tasks().Save(ctx, tNext, tRev); saveErr != nil {
				return saveErr
			}
			if _, saveErr := uow.Attempts().Save(ctx, aNext, aRev); saveErr != nil {
				return saveErr
			}
			if err := recordTransition(ctx, uow, EntityTask, t.ID.String(), string(tFrom), string(tNext.State), "stop precedence over the check outcome", generation, now); err != nil {
				return err
			}
			return recordTransition(ctx, uow, EntityAttempt, a.ID.String(), string(aFrom), string(aNext.State), "stop precedence over the check outcome", generation, now)
		}

		if outcome.ExitCode == 0 && !outcome.Unknown {
			// Pass: attempt completed, task completed — the run stays
			// running; integration is a separate later claim.
			tFrom, aFrom := t.State, a.State
			aNext, trErr := a.Complete(now)
			if trErr != nil {
				return trErr
			}
			tNext, trErr := t.Complete(now)
			if trErr != nil {
				return trErr
			}
			if _, saveErr := uow.Attempts().Save(ctx, aNext, aRev); saveErr != nil {
				return saveErr
			}
			if _, saveErr := uow.Tasks().Save(ctx, tNext, tRev); saveErr != nil {
				return saveErr
			}
			if err := recordTransition(ctx, uow, EntityAttempt, a.ID.String(), string(aFrom), string(aNext.State), "passing per-task check", generation, now); err != nil {
				return err
			}
			return recordTransition(ctx, uow, EntityTask, t.ID.String(), string(tFrom), string(tNext.State), "passing per-task check", generation, now)
		}

		// Fail or unrepeatable unknown: the attempt fails; the task follows
		// the budgeted consequence, with failure closing the mailbox under
		// the snapshot-equality contract.
		attempts, attListErr := wf.AttemptIndex().ByTask(ctx, task.ID)
		if attListErr != nil {
			return attListErr
		}
		consequence := decideTaskConsequence(r.StopRequested, len(attempts), retryLimitFor(&frozen.Snapshot))
		if consequence != prediction {
			return errSettlementRetry
		}
		tFrom, aFrom := t.State, a.State
		aNext, trErr := a.Fail(now)
		if trErr != nil {
			return trErr
		}
		var tNext run.Task
		if consequence == taskConsequenceFailed {
			tNext, trErr = t.Fail(now)
			if trErr != nil {
				return trErr
			}
			current, oblErr := c.pendingTaskObligations(ctx, wf, handle.runID, task.ID)
			if oblErr != nil {
				return oblErr
			}
			if !slices.Equal(current, obligations) {
				return errSettlementRetry
			}
			tNext = tNext.CloseMailbox(now)
		} else {
			tNext, trErr = t.NeedsRework(now)
			if trErr != nil {
				return trErr
			}
		}
		if _, saveErr := uow.Attempts().Save(ctx, aNext, aRev); saveErr != nil {
			return saveErr
		}
		if _, saveErr := uow.Tasks().Save(ctx, tNext, tRev); saveErr != nil {
			return saveErr
		}
		reason := describeCheckOutcome(outcome)
		if err := recordTransition(ctx, uow, EntityAttempt, a.ID.String(), string(aFrom), string(aNext.State), reason, generation, now); err != nil {
			return err
		}
		if err := recordTransition(ctx, uow, EntityTask, t.ID.String(), string(tFrom), string(tNext.State), reason, generation, now); err != nil {
			return err
		}
		return commitControllerNotice(ctx, wf, handle.runID, notice, now)
	})
}

// recoverFeatureCheckExecutions resolves unresolved RESULT-subject check
// executions (integration-subject ones belong to the integration
// recovery): claim absent → bounded wait then reconciling; claim present
// → group retirement, then the unknown-outcome rule at task scope.
func (c *Controller) recoverFeatureCheckExecutions(ctx context.Context, handle RunHandle, frozen *FrozenRun) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	var unresolved []Operation
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		ops, opErr := uow.Operations().Pending(ctx, handle.runID)
		if opErr != nil {
			return opErr
		}
		for i := range ops {
			if ops[i].Kind != OpCheckRun {
				continue
			}
			if intent, ok := decodeOperationPayload[integrationCheckIntent](ops[i].Intent); ok && intent.IntegrationID != "" {
				continue
			}
			unresolved = append(unresolved, ops[i])
		}
		return nil
	}); err != nil {
		return "", err
	}

	blocked := ""
	for i := range unresolved {
		op := unresolved[i]
		still, err := c.recoverFeatureCheckExecution(ctx, handle, frozen, &op)
		if err != nil {
			return "", err
		}
		if still != "" && blocked == "" {
			blocked = still
		}
	}
	return blocked, nil
}

// recoverFeatureCheckExecution resolves one unresolved result-subject
// execution.
func (c *Controller) recoverFeatureCheckExecution(ctx context.Context, handle RunHandle, frozen *FrozenRun, op *Operation) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	intent, ok := decodeOperationPayload[CheckRunIntent](op.Intent)
	if !ok || intent.ResultID == "" {
		if err := c.markOperationReconciling(ctx, handle, op.ID, "feature check intent could not be decoded; failing closed"); err != nil {
			return "", err
		}
		return fmt.Sprintf("feature check %s has an undecodable intent; failing closed", op.ID), nil
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
		return "", fmt.Errorf("app: load feature check claim: %w", err)
	}
	if !found {
		if c.Clock.Now().Sub(op.CreatedAt) <= CheckClaimDeadline {
			if err := c.recordCombinedCheckClaimWait(ctx, handle, op); err != nil {
				return "", err
			}
			return fmt.Sprintf("feature check %s has no claim yet; within the bounded wait", op.ID), nil
		}
		if err := c.markOperationReconciling(ctx, handle, op.ID, "no check-exec claim within the bounded wait; ambiguous, never absence"); err != nil {
			return "", err
		}
		return fmt.Sprintf("feature check %s has no claim past the bounded wait; reconciling", op.ID), nil
	}
	outcome, retireErr := c.retireGroup(ctx, handle, claim.PID, [][]string{intent.CheckArgv, intent.SpawnArgv})
	if retireErr != nil {
		return "", retireErr
	}
	switch outcome {
	case GroupEmpty:
		return "", c.applyFeatureCheckUnknown(ctx, handle, frozen, op, &intent)
	case GroupMatched:
		return fmt.Sprintf("feature check group %d signaled; awaiting observed absence", claim.PID), nil
	case GroupMismatched:
		if err := c.markOperationReconciling(ctx, handle, op.ID, "check group members do not match the recorded argv; failing closed"); err != nil {
			return "", err
		}
		return fmt.Sprintf("feature check group %d does not match the recorded argv; failing closed", claim.PID), nil
	default: // GroupInspectionFailed
		return fmt.Sprintf("feature check group %d could not be inspected; failing closed", claim.PID), nil
	}
}

// applyFeatureCheckUnknown settles a retired result-subject execution
// under the unknown-outcome rule at task scope: repeatable requeues the
// request for a fresh execution from the same checking attempt;
// unrepeatable fails the attempt and settles the task by budget —
// evidence retained, never completed on unknown.
func (c *Controller) applyFeatureCheckUnknown(ctx context.Context, handle RunHandle, frozen *FrozenRun, op *Operation, intent *CheckRunIntent) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	resultID := identity.ResultID(intent.ResultID)
	var (
		task    run.Task
		attempt run.Attempt
	)
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		request, getErr := uow.CheckRequests().Get(ctx, resultID)
		if getErr != nil {
			return getErr
		}
		a, _, attErr := uow.Attempts().Get(ctx, request.AttemptID)
		if attErr != nil {
			return attErr
		}
		t, _, taskErr := uow.Tasks().Get(ctx, a.TaskID)
		if taskErr != nil {
			return taskErr
		}
		task = t
		attempt = a
		return nil
	}); err != nil {
		return err
	}
	return c.settleFeatureCheckOutcome(ctx, handle, frozen, op.ID, &task, &attempt, resultID, checkRunOutcome{Unknown: true, Detail: "process group retired after takeover; result unknown"}, nil)
}
