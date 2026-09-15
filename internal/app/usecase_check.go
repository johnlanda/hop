package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// CheckClaimDeadline bounds the ambiguous window between a check
// execution's committed intent and its check-exec claim appearing: within
// it an absent claim may still be a child that has not reached its
// pre-exec write; past it, with no claim, the operation escalates to
// reconciling — never to assumed absence.
const CheckClaimDeadline = 120 * time.Second

// checkRunOutcome is the OpCheckRun operation's outcome payload.
type checkRunOutcome struct {
	ExitCode int    `json:"exit_code"`
	Unknown  bool   `json:"unknown"` // the outcome could not be determined (crash recovery)
	Detail   string `json:"detail,omitempty"`
}

// CheckReport is one ClaimAndRunCheck round's outcome.
type CheckReport struct {
	Ran         bool // false when there was no pending check request to claim (or recovery still blocks new work)
	OperationID string
	Passed      bool
	Interrupted bool
	Unknown     bool // unrepeatable unknown outcome: the run is left actionable, not completed
	// Blocked names an unresolved prior execution that still prevents new
	// check work (the decision table's unresolved-intent rule).
	Blocked string
}

// ClaimAndRunCheck first recovers any unresolved prior check execution
// (docs/plan/phase-2-design.md section 4, check.run row, and the section 7
// unknown-outcome rule), then claims the run's oldest pending check
// request and drives one execution start to finish: a detached checkout of
// the accepted result's commit, hop check-exec spawned via CommandRunner
// bounded by the frozen check timeout, and the outcome transaction.
// Execution inputs — check argv, timeout, repeatability, repository root
// and state root — are loaded from the frozen run, never from the caller.
// spawnEnv is hop check-exec's complete spawned environment (composition
// builds it, ordinarily SanitizeEnvironment(os.Environ(), the frozen
// policy)) — this method only guarantees HOP_STATE_DIR is present,
// overriding any value spawnEnv already carries for it. hopPath is the
// absolute path to the running hop executable.
func (c *Controller) ClaimAndRunCheck(ctx context.Context, handle RunHandle, hopPath string, spawnEnv []string) (CheckReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; the controller's check-consuming loop calls this once per pending request.
	frozen, err := c.Read.LoadFrozenRun(ctx, handle.runID)
	if err != nil {
		return CheckReport{}, fmt.Errorf("app: load frozen run: %w", err)
	}

	blocked, err := c.recoverCheckState(ctx, handle, &frozen)
	if err != nil {
		return CheckReport{}, err
	}
	if blocked != "" {
		return CheckReport{Blocked: blocked}, nil
	}

	commitOID, hasResult, err := c.acceptedCommit(ctx, handle)
	if err != nil {
		return CheckReport{}, err
	}
	if !hasResult {
		return CheckReport{}, nil
	}
	// The materialized tree object id is resolved before the intent
	// commits and frozen into it: takeover validation and evidence both
	// name exactly the tree the check ran against.
	treeOID, err := c.runGit(ctx, frozen.RepositoryRoot, "rev-parse", commitOID+"^{tree}")
	if err != nil {
		return CheckReport{}, fmt.Errorf("app: resolve candidate tree: %w", err)
	}

	claimed, checkRequest, opID, err := c.claimCheckRequest(ctx, handle, &frozen, hopPath, commitOID, treeOID)
	if err != nil {
		return CheckReport{}, err
	}
	if !claimed {
		return CheckReport{}, nil
	}
	checkoutPath := checkExecutionCheckoutPath(frozen.Snapshot.StateRoot, handle.runID, opID)

	// Every external act from here on — candidate inspection, checkout
	// materialization, the spawn and any cleanup — runs under the handle's
	// cancelable act context, so a failed heartbeat or detach cancels it,
	// with revalidation immediately before each mutation.
	actCtx, release := handle.actContext(ctx)
	defer release()
	if err := c.revalidateForDispatch(ctx, handle, false); err != nil {
		return CheckReport{}, fmt.Errorf("app: revalidate before check checkout: %w", err)
	}
	if rejectDetail, rejectErr := c.rejectSubmoduleCandidate(actCtx, frozen.RepositoryRoot, commitOID); rejectErr != nil {
		return c.recordCheckOutcome(ctx, handle, opID, checkRequest, checkRunOutcome{Unknown: true, Detail: rejectErr.Error()}, false, rejectErr)
	} else if rejectDetail != "" {
		return c.recordCheckOutcome(ctx, handle, opID, checkRequest, checkRunOutcome{ExitCode: 1, Detail: rejectDetail}, false, nil)
	}
	// The candidate inspection above takes time: revalidate again
	// immediately before the materialization mutation, so a controller
	// fenced or stopped during inspection never materializes.
	if err := c.revalidateForDispatch(ctx, handle, false); err != nil {
		return CheckReport{}, fmt.Errorf("app: revalidate before checkout materialization: %w", err)
	}
	if err := c.materializeCheckout(actCtx, frozen.RepositoryRoot, checkoutPath, commitOID); err != nil {
		return c.recordCheckOutcome(ctx, handle, opID, checkRequest, checkRunOutcome{Unknown: true, Detail: err.Error()}, false, fmt.Errorf("materialize checkout: %w", err))
	}

	spawnEnv = withHOPStateDir(spawnEnv, frozen.Snapshot.StateRoot)
	spawnArgv := checkSpawnArgv(hopPath, opID, frozen.Snapshot.CheckArgv)
	if err := c.revalidateForDispatch(ctx, handle, false); err != nil {
		return CheckReport{}, fmt.Errorf("app: revalidate before check spawn: %w", err)
	}
	timeout := frozen.Snapshot.CheckTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	boundedCtx, cancel := context.WithTimeout(actCtx, timeout)
	cmdResult, runErr := c.Commands.Run(boundedCtx, Command{Argv: spawnArgv, Dir: checkoutPath, Env: spawnEnv})
	cancel()

	// Evidence retention before any cleanup or outcome, on EVERY command
	// result — a lost execution's partial output is evidence too: stdout
	// and stderr are written under the execution's own directory and
	// recorded as result-linked artifact rows with digests.
	evidence, captureErr := c.captureCheckOutputs(ctx, handle, &frozen, opID, checkRequest.ResultID, cmdResult)

	if runErr != nil {
		// The spawn's result is unknowable here: the child may never have
		// started, or may have started and been lost. Ambiguous — the
		// operation goes reconciling with the error as evidence, whatever
		// output was returned is preserved, the checkout is kept for
		// inspection, and recovery resolves it through the claim table,
		// never by guessing.
		if len(evidence) > 0 {
			if saveErr := c.saveEvidenceRows(ctx, handle, evidence); saveErr != nil {
				return CheckReport{}, saveErr
			}
		}
		detail := fmt.Sprintf("check spawn returned an error before an outcome was observed: %v", runErr)
		if captureErr != nil {
			detail += fmt.Sprintf("; output retention also failed: %v", captureErr)
		}
		if markErr := c.markOperationReconciling(ctx, handle, opID, detail); markErr != nil {
			return CheckReport{}, markErr
		}
		return CheckReport{Ran: true, OperationID: opID.String()}, fmt.Errorf("app: check execution ambiguous: %w", runErr)
	}
	if captureErr != nil {
		// Completion may never claim retained evidence that was lost: the
		// execution fails with the retention failure named, and the
		// checkout is kept so the outputs remain inspectable in place.
		outcome := checkRunOutcome{ExitCode: cmdResult.ExitCode, Detail: fmt.Sprintf("evidence retention failed: %v", captureErr)}
		return c.recordCheckOutcome(ctx, handle, opID, checkRequest, outcome, false, captureErr, evidence...)
	}

	outcome := checkRunOutcome{ExitCode: cmdResult.ExitCode}
	report, outcomeErr := c.recordCheckOutcome(ctx, handle, opID, checkRequest, outcome, frozen.Snapshot.CheckRepeatable, nil, evidence...)
	// Cleanup only after evidence retention and the recorded outcome; the
	// retained outputs stay under the execution's own directory.
	c.removeCheckout(ctx, handle, frozen.RepositoryRoot, checkoutPath)
	return report, outcomeErr
}

// saveEvidenceRows persists retained-output artifact rows outside the
// outcome transaction, for executions whose outcome stays unresolved.
func (c *Controller) saveEvidenceRows(ctx context.Context, handle RunHandle, evidence []run.Artifact) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per ambiguous execution.
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		for i := range evidence {
			if err := uow.Artifacts().Save(ctx, evidence[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

// acceptedCommit reads the run's accepted result commit, if one exists.
func (c *Controller) acceptedCommit(ctx context.Context, handle RunHandle) (string, bool, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per check round.
	var (
		commit string
		has    bool
	)
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		detail, loadErr := c.Read.LoadRunStatus(ctx, handle.runID)
		if loadErr != nil {
			return loadErr
		}
		result, getErr := uow.Results().Accepted(ctx, detail.AttemptID)
		if getErr != nil || result == nil {
			return getErr
		}
		commit = result.CommitOID
		has = true
		return nil
	})
	return commit, has, err
}

// captureCheckOutputs writes the execution's stdout and stderr through the
// ArtifactStore under the execution's own directory and returns the
// result-linked artifact rows to persist. Retention is mandatory
// evidence: any failed write or row construction is returned, alongside
// whichever rows did land, so the caller can refuse to complete over
// evidence it claims but lost.
func (c *Controller) captureCheckOutputs(ctx context.Context, handle RunHandle, frozen *FrozenRun, opID identity.OperationID, resultID identity.ResultID, cmdResult CommandResult) ([]run.Artifact, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per execution.
	base := filepath.Join(frozen.Snapshot.StateRoot, "runs", handle.runID.String(), "checks", opID.String())
	var (
		rows       []run.Artifact
		captureErr error
	)
	streams := []struct {
		name    string
		kind    run.ArtifactKind
		content []byte
	}{
		{"stdout", run.ArtifactCheckStdout, cmdResult.Stdout},
		{"stderr", run.ArtifactCheckStderr, cmdResult.Stderr},
	}
	for _, stream := range streams {
		path := filepath.Join(base, stream.name)
		if err := c.Artifacts.WriteArtifact(ctx, path, stream.content); err != nil {
			captureErr = errors.Join(captureErr, fmt.Errorf("retain check %s: %w", stream.name, err))
			continue
		}
		artifactID, err := identity.ParseArtifactID(c.IDs.NewID())
		if err != nil {
			captureErr = errors.Join(captureErr, fmt.Errorf("retain check %s: %w", stream.name, err))
			continue
		}
		rows = append(rows, run.NewResultArtifact(artifactID, handle.runID, resultID, stream.kind, path, sha256Hex(stream.content)))
	}
	return rows, captureErr
}

// checkExecutionCheckoutPath is the per-execution detached checkout path.
func checkExecutionCheckoutPath(stateRoot string, runID identity.RunID, opID identity.OperationID) string {
	return filepath.Join(stateRoot, "runs", runID.String(), "checks", opID.String(), "tree")
}

// checkSpawnArgv is the hop check-exec invocation for one execution.
func checkSpawnArgv(hopPath string, opID identity.OperationID, checkArgv []string) []string {
	return append([]string{hopPath, "check-exec", "--op", opID.String(), "--"}, checkArgv...)
}

// recoverCheckState resolves every unresolved prior check execution before
// new check work, and reopens a check request a dead generation claimed
// but never executed. blocked names an execution that is still ambiguous
// or reconciling: new check work is refused while it stands (the decision
// table's unresolved-intent rule).
func (c *Controller) recoverCheckState(ctx context.Context, handle RunHandle, frozen *FrozenRun) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per check round.
	var unresolved []Operation
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		ops, opErr := uow.Operations().ByKind(ctx, handle.runID, OpCheckRun)
		if opErr != nil {
			return opErr
		}
		for i := range ops {
			if ops[i].State == OperationPending || ops[i].State == OperationReconciling {
				unresolved = append(unresolved, ops[i])
			}
		}
		return nil
	}); err != nil {
		return "", err
	}

	blocked := ""
	for i := range unresolved {
		still, err := c.recoverCheckExecution(ctx, handle, &unresolved[i], frozen)
		if err != nil {
			return "", err
		}
		if still != "" && blocked == "" {
			blocked = still
		}
	}
	if blocked != "" {
		return blocked, nil
	}
	if err := c.driveTerminalUnknownFailure(ctx, handle); err != nil {
		return "", err
	}
	return "", c.reopenOrphanedCheckRequest(ctx, handle)
}

// recoverCheckExecution resolves one unresolved check execution per the
// decision table: with a claim, its process group is retired under the
// group-retirement rule and, once observed absent, the unknown-outcome
// rule applies; without a claim, the pre-write window stays ambiguous for
// the bounded claim wait and then escalates to reconciling.
func (c *Controller) recoverCheckExecution(ctx context.Context, handle RunHandle, op *Operation, frozen *FrozenRun) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per unresolved execution.
	var (
		claim CheckExecClaim
		found bool
	)
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		var err error
		claim, found, err = uow.CheckExecClaims().Get(ctx, op.ID)
		return err
	}); err != nil {
		return "", fmt.Errorf("app: load check-exec claim: %w", err)
	}

	if !found {
		now := c.Clock.Now()
		if now.Sub(op.CreatedAt) <= CheckClaimDeadline {
			if err := c.recordBoundedWait(ctx, handle, op, CheckClaimDeadline, "no check-exec claim yet; the child may not have reached its pre-exec write"); err != nil {
				return "", err
			}
			return fmt.Sprintf("check execution %s has no claim yet; within the bounded wait", op.ID), nil
		}
		if err := c.markOperationReconciling(ctx, handle, op.ID, "no check-exec claim within the bounded wait; ambiguous, never absence"); err != nil {
			return "", err
		}
		return fmt.Sprintf("check execution %s has no claim past the bounded wait; reconciling", op.ID), nil
	}

	intent, _ := decodeOperationPayload[CheckRunIntent](op.Intent) // a decode failure leaves both argvs nil, which ClassifyGroupRetirement treats as never matching — fails closed, not a panic.
	outcome, retireErr := c.retireGroup(ctx, handle, claim.PID, [][]string{intent.CheckArgv, intent.SpawnArgv})
	if retireErr != nil {
		return "", retireErr
	}
	switch outcome {
	case GroupEmpty:
		// Confirmed absence: the unknown-outcome rule applies.
		return "", c.applyUnknownOutcome(ctx, handle, op, frozen)
	case GroupMatched:
		return fmt.Sprintf("check group %d signaled; awaiting observed absence", claim.PID), nil
	case GroupMismatched:
		if err := c.markOperationReconciling(ctx, handle, op.ID, "check group members do not match the recorded argv; failing closed"); err != nil {
			return "", err
		}
		return fmt.Sprintf("check group %d does not match the recorded argv; failing closed", claim.PID), nil
	default: // GroupInspectionFailed
		return fmt.Sprintf("check group %d could not be inspected; failing closed", claim.PID), nil
	}
}

// applyUnknownOutcome settles a retired check execution under the
// section 7 unknown-outcome rule with stop precedence applied INSIDE the
// transaction: with a stop request held, the execution settles and the
// task and attempt are interrupted, never terminally failed, and the run
// is left for stop handling. With `check.repeatable = true` the request
// returns to requested for a fresh execution (a new operation, a fresh
// checkout) against the same accepted result, and a completing run
// returns to running. Otherwise the unknown outcome is terminal for
// automation: the task and attempt fail with the execution named, and the
// run fails only after its worker's termination has been observed
// (driveTerminalUnknownFailure) — never while the worker may be live.
func (c *Controller) applyUnknownOutcome(ctx context.Context, handle RunHandle, op *Operation, frozen *FrozenRun) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per retired execution.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		latest, err := uow.Operations().Get(ctx, op.ID)
		if err != nil {
			return err
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

		intent, ok := decodeOperationPayload[CheckRunIntent](latest.Intent)
		if !ok || intent.ResultID == "" {
			return fmt.Errorf("app: check execution %s records no result id; cannot apply the unknown-outcome rule", latest.ID)
		}
		resultID := identity.ResultID(intent.ResultID)
		request, err := uow.CheckRequests().Get(ctx, resultID)
		if err != nil {
			return err
		}
		generation := gen(handle.lease.Generation)

		r, rRev, getErr := uow.Runs().Get(ctx, handle.runID)
		if getErr != nil {
			return getErr
		}
		if r.StopRequested {
			// Stop precedence: the execution settles as unknown and the
			// task and attempt are interrupted; the run belongs to stop
			// handling, which retires the worker and observes termination.
			request.State = CheckRequestSettled
			if err := uow.CheckRequests().Save(ctx, request); err != nil {
				return err
			}
			a, aRev, aErr := uow.Attempts().Get(ctx, request.AttemptID)
			if aErr != nil {
				return aErr
			}
			t, tRev, tErr := uow.Tasks().Get(ctx, a.TaskID)
			if tErr != nil {
				return tErr
			}
			return finishCheckEntities(ctx, uow, &checkOutcomeArgs{
				Run: r, RunRevision: rRev, Attempt: a, AttemptRev: aRev, Task: t, TaskRev: tRev,
				Generation: generation, Now: now, Kind: entityInterrupt,
				Reason: "stop precedence over an unknown check outcome",
			})
		}

		if frozen.Snapshot.CheckRepeatable {
			request.State = CheckRequestRequested
			request.ClaimedGeneration = nil
			if err := uow.CheckRequests().Save(ctx, request); err != nil {
				return err
			}
			if r.State == run.RunCompleting {
				rFrom := r.State
				next, runErr := r.MarkRunning(now)
				if runErr != nil {
					return runErr
				}
				if _, saveErr := uow.Runs().Save(ctx, next, rRev); saveErr != nil {
					return saveErr
				}
				return recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(rFrom), string(next.State), "repeatable check unknown outcome requeued", generation, now)
			}
			return nil
		}

		request.State = CheckRequestSettled
		if err := uow.CheckRequests().Save(ctx, request); err != nil {
			return err
		}
		a, aRev, getErr := uow.Attempts().Get(ctx, request.AttemptID)
		if getErr != nil {
			return getErr
		}
		t, tRev, getErr := uow.Tasks().Get(ctx, a.TaskID)
		if getErr != nil {
			return getErr
		}
		return finishCheckEntities(ctx, uow, &checkOutcomeArgs{
			Run: r, RunRevision: rRev, Attempt: a, AttemptRev: aRev, Task: t, TaskRev: tRev,
			Generation: generation, Now: now, Kind: entityFailKeepRun,
			Reason: fmt.Sprintf("unrepeatable unknown outcome of check execution %s", latest.ID),
		})
	})
}

// driveTerminalUnknownFailure finishes a terminal unknown outcome: with
// the task and attempt already failed and the run not yet terminal, the
// worker is retired through the shared close procedure and the run fails
// only once its termination has been observed — section 5's run table
// permits the failure only "after stop of its worker".
func (c *Controller) driveTerminalUnknownFailure(ctx context.Context, handle RunHandle) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per check round.
	detail, err := c.Read.LoadRunStatus(ctx, handle.runID)
	if err != nil {
		return err
	}
	if detail.AttemptState != run.AttemptFailed {
		return nil
	}
	if detail.State != run.RunCompleting && detail.State != run.RunResuming && detail.State != run.RunRunning {
		return nil
	}
	if detail.LastCheck == nil || !detail.LastCheck.Unknown {
		return nil
	}
	outstanding, err := c.retireWorker(ctx, handle, detail)
	if err != nil {
		return err
	}
	if outstanding != "" {
		return nil // termination not yet observed; the run stays actionable.
	}
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		r, rRev, getErr := uow.Runs().Get(ctx, handle.runID)
		if getErr != nil {
			return getErr
		}
		if r.State != run.RunCompleting && r.State != run.RunResuming && r.State != run.RunRunning {
			return nil
		}
		rFrom := r.State
		next, failErr := r.Fail(now)
		if failErr != nil {
			return failErr
		}
		if _, saveErr := uow.Runs().Save(ctx, next, rRev); saveErr != nil {
			return saveErr
		}
		if detail.SessionID != "" {
			if termErr := terminateSession(ctx, uow, detail.SessionID, "unrepeatable unknown check outcome; worker retired", gen(handle.lease.Generation), now); termErr != nil {
				return termErr
			}
		}
		return recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(rFrom), string(next.State), "unrepeatable unknown check outcome after worker retirement", gen(handle.lease.Generation), now)
	})
}

// reopenOrphanedCheckRequest returns a check request a prior generation
// claimed but never turned into an execution back to requested: with the
// atomic claim-plus-intent transaction this window no longer opens, but a
// store written by an older controller can still carry one.
func (c *Controller) reopenOrphanedCheckRequest(ctx context.Context, handle RunHandle) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per check round.
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		detail, err := c.Read.LoadRunStatus(ctx, handle.runID)
		if err != nil {
			return err
		}
		result, err := uow.Results().Accepted(ctx, detail.AttemptID)
		if err != nil || result == nil {
			return err
		}
		request, err := uow.CheckRequests().Get(ctx, result.ID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return err
		}
		if request.State != CheckRequestClaimed {
			return nil
		}
		if request.ClaimedGeneration != nil && *request.ClaimedGeneration == handle.lease.Generation {
			return nil
		}
		ops, err := uow.Operations().ByKind(ctx, handle.runID, OpCheckRun)
		if err != nil {
			return err
		}
		for i := range ops {
			intent, ok := decodeOperationPayload[CheckRunIntent](ops[i].Intent)
			if ok && intent.ResultID == result.ID.String() {
				return nil // an execution exists for this request; recovery owns it.
			}
		}
		request.State = CheckRequestRequested
		request.ClaimedGeneration = nil
		return uow.CheckRequests().Save(ctx, request)
	})
}

// claimCheckRequest atomically claims the run's oldest pending check
// request together with its execution intent and the lifecycle
// transitions it implies — one transaction, so no crash window can leave
// a claimed request without a recoverable operation.
func (c *Controller) claimCheckRequest(ctx context.Context, handle RunHandle, frozen *FrozenRun, hopPath, commitOID, treeOID string) (bool, CheckRequest, identity.OperationID, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per claim attempt.
	opID, err := c.newOperationID()
	if err != nil {
		return false, CheckRequest{}, "", err
	}
	var (
		claimed bool
		request CheckRequest
	)
	now := c.Clock.Now()
	err = c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		r, rRev, getErr := uow.Runs().Get(ctx, handle.runID)
		if getErr != nil {
			return getErr
		}
		if r.StopRequested {
			// Unstarted work is never started after stop: the pending
			// request stays requested; stop handling interrupts it.
			return nil
		}
		pending, found, getErr := uow.CheckRequests().Pending(ctx, handle.runID)
		if getErr != nil {
			return getErr
		}
		if !found {
			return nil
		}
		result, getErr := uow.Results().Accepted(ctx, pending.AttemptID)
		if getErr != nil {
			return getErr
		}
		if result == nil {
			return fmt.Errorf("app: attempt %s has no accepted result", pending.AttemptID)
		}
		if result.CommitOID != commitOID {
			return fmt.Errorf("app: accepted result commit changed between read and claim (%s != %s)", result.CommitOID, commitOID)
		}

		generation := handle.lease.Generation
		pending.State = CheckRequestClaimed
		pending.ClaimedGeneration = &generation
		if saveErr := uow.CheckRequests().Save(ctx, pending); saveErr != nil {
			return saveErr
		}

		gp := gen(generation)
		if r.State == run.RunRunning {
			rFrom := r.State
			nextRun, completeErr := r.EnterCompleting(now)
			if completeErr != nil {
				return completeErr
			}
			if _, saveErr := uow.Runs().Save(ctx, nextRun, rRev); saveErr != nil {
				return saveErr
			}
			if transErr := recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(rFrom), string(nextRun.State), "accepted result's check claimed", gp, now); transErr != nil {
				return transErr
			}
		}

		a, aRev, getErr := uow.Attempts().Get(ctx, pending.AttemptID)
		if getErr != nil {
			return getErr
		}
		if a.State == run.AttemptSubmitted {
			aFrom := a.State
			checking, checkingErr := a.EnterChecking(now)
			if checkingErr != nil {
				return checkingErr
			}
			if _, saveErr := uow.Attempts().Save(ctx, checking, aRev); saveErr != nil {
				return saveErr
			}
			if transErr := recordTransition(ctx, uow, EntityAttempt, pending.AttemptID.String(), string(aFrom), string(checking.State), "check execution started", gp, now); transErr != nil {
				return transErr
			}
		}
		// An attempt already checking supports a fresh execution (a
		// repeatable requeue) without a second EnterChecking.

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
		request = pending
		return nil
	})
	if err != nil {
		return false, CheckRequest{}, "", fmt.Errorf("app: claim check request: %w", err)
	}
	return claimed, request, opID, nil
}

// rejectSubmoduleCandidate scans the candidate commit's tree for gitlink
// (submodule) entries: the Phase 2 check must be self-contained in its
// detached checkout, and a submodule tree would silently omit required
// content, so it fails clearly before any spawn. The returned detail is
// non-empty when the candidate is rejected; the error reports an
// inspection that could not be made.
func (c *Controller) rejectSubmoduleCandidate(ctx context.Context, repositoryRoot, commitOID string) (string, error) {
	listing, err := c.runGit(ctx, repositoryRoot, "ls-tree", "-r", commitOID)
	if err != nil {
		return "", fmt.Errorf("inspect candidate tree for submodules: %w", err)
	}
	for line := range strings.SplitSeq(listing, "\n") {
		if strings.HasPrefix(line, "160000 ") {
			return fmt.Sprintf("candidate %s contains a submodule (gitlink) entry: submodules are unsupported and the checkout would be incomplete", commitOID), nil
		}
	}
	return "", nil
}

// materializeCheckout validates the commit and creates the detached
// worktree checkout via CommandRunner (docs/plan/phase-2-design.md
// section 7): the check never runs in the live worktree.
func (c *Controller) materializeCheckout(ctx context.Context, repositoryRoot, checkoutPath, commitOID string) error {
	kind, err := c.runGit(ctx, repositoryRoot, "cat-file", "-t", commitOID)
	if err != nil {
		return err
	}
	if kind != "commit" {
		return fmt.Errorf("object %s is a %s, not a commit", commitOID, kind)
	}
	_, err = c.runGit(ctx, repositoryRoot, "worktree", "add", "--detach", checkoutPath, commitOID)
	return err
}

// removeCheckout removes the detached checkout after evidence capture;
// outputs under the operation's own directory stay. Cleanup is itself a
// mutation: it revalidates the dispatch first and never runs after a
// fenced, expired or stop-requested lease — a leaked checkout is
// evidence-safe, an unauthorized mutation is not.
func (c *Controller) removeCheckout(ctx context.Context, handle RunHandle, repositoryRoot, checkoutPath string) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per execution.
	if err := c.revalidateForDispatch(ctx, handle, false); err != nil {
		return
	}
	actCtx, release := handle.actContext(ctx)
	defer release()
	_, _ = c.runGit(actCtx, repositoryRoot, "worktree", "remove", "--force", checkoutPath) //nolint:errcheck // best-effort cleanup; a leaked checkout is evidence, not corruption, since executions never share paths.
}

// recordCheckOutcome applies the section 7 outcome transaction: it
// re-reads the stop request and decides completion, failure, interruption
// or the unknown-outcome rule, settling the check request accordingly —
// settled on every final outcome, requested again on an authorized
// repetition.
func (c *Controller) recordCheckOutcome(ctx context.Context, handle RunHandle, opID identity.OperationID, request CheckRequest, outcome checkRunOutcome, repeatable bool, actErr error, evidence ...run.Artifact) (CheckReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per check execution.
	now := c.Clock.Now()
	report := CheckReport{Ran: true, OperationID: opID.String()}

	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		for i := range evidence {
			if saveErr := uow.Artifacts().Save(ctx, evidence[i]); saveErr != nil {
				return saveErr
			}
		}
		op, getErr := uow.Operations().Get(ctx, opID)
		if getErr != nil {
			return getErr
		}
		op.UpdatedAt = now
		op.Outcome = outcome

		latestRequest, getErr := uow.CheckRequests().Get(ctx, request.ResultID)
		if getErr != nil {
			return getErr
		}

		r, rRev, getErr := uow.Runs().Get(ctx, handle.runID)
		if getErr != nil {
			return getErr
		}
		a, aRev, getErr := uow.Attempts().Get(ctx, request.AttemptID)
		if getErr != nil {
			return getErr
		}
		t, tRev, getErr := uow.Tasks().Get(ctx, a.TaskID)
		if getErr != nil {
			return getErr
		}
		generation := gen(handle.lease.Generation)

		settleRequest := func(state CheckRequestState) error {
			latestRequest.State = state
			if state == CheckRequestRequested {
				latestRequest.ClaimedGeneration = nil
			}
			return uow.CheckRequests().Save(ctx, latestRequest)
		}

		switch {
		case r.StopRequested:
			op.State = OperationSucceeded
			report.Interrupted = true
			if err := settleRequest(CheckRequestSettled); err != nil {
				return err
			}
			return finishCheckOutcome(ctx, uow, &checkOutcomeArgs{r, rRev, a, aRev, t, tRev, generation, now, entityInterrupt, "stop precedence", op})
		case outcome.Unknown && repeatable:
			op.State = OperationFailed
			if saveErr := uow.Operations().Save(ctx, op); saveErr != nil {
				return saveErr
			}
			if err := settleRequest(CheckRequestRequested); err != nil {
				return err
			}
			if r.State == run.RunCompleting {
				rFrom := r.State
				nextRun, runErr := r.MarkRunning(now)
				if runErr != nil {
					return runErr
				}
				if _, saveErr := uow.Runs().Save(ctx, nextRun, rRev); saveErr != nil {
					return saveErr
				}
				if transErr := recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(rFrom), string(nextRun.State), "repeatable check unknown outcome requeued", generation, now); transErr != nil {
					return transErr
				}
			}
			report.Unknown = true
			return nil
		case outcome.Unknown:
			op.State = OperationFailed
			report.Unknown = true
			if err := settleRequest(CheckRequestSettled); err != nil {
				return err
			}
			return finishCheckOutcome(ctx, uow, &checkOutcomeArgs{r, rRev, a, aRev, t, tRev, generation, now, entityFailKeepRun, "unrepeatable unknown check outcome", op})
		case actErr == nil && outcome.ExitCode == 0:
			op.State = OperationSucceeded
			report.Passed = true
			if err := settleRequest(CheckRequestSettled); err != nil {
				return err
			}
			return finishCheckOutcome(ctx, uow, &checkOutcomeArgs{r, rRev, a, aRev, t, tRev, generation, now, entityComplete, "passing check receipt", op})
		default:
			op.State = OperationFailed
			if err := settleRequest(CheckRequestSettled); err != nil {
				return err
			}
			reason := "failing check"
			if outcome.Detail != "" {
				reason = outcome.Detail
			}
			return finishCheckOutcome(ctx, uow, &checkOutcomeArgs{r, rRev, a, aRev, t, tRev, generation, now, entityFail, reason, op})
		}
	})
	if err != nil {
		return CheckReport{}, fmt.Errorf("app: record check outcome: %w", err)
	}
	if actErr != nil {
		return report, fmt.Errorf("app: check execution: %w", actErr)
	}
	return report, nil
}

// entityOutcomeKind names which family of transitions finishCheckOutcome
// applies to Run, Task and Attempt.
type entityOutcomeKind int

const (
	entityComplete entityOutcomeKind = iota
	entityFail
	// entityFailKeepRun fails the task and attempt but leaves the run
	// untouched: a terminal unknown outcome fails the run only after its
	// worker's termination has been observed.
	entityFailKeepRun
	entityInterrupt
)

// checkOutcomeArgs bundles finishCheckOutcome's per-entity inputs; it exists
// so callers do not have to name ten positional arguments at each call site.
type checkOutcomeArgs struct {
	Run         run.Run
	RunRevision int64
	Attempt     run.Attempt
	AttemptRev  int64
	Task        run.Task
	TaskRev     int64
	Generation  *int64
	Now         time.Time
	Kind        entityOutcomeKind
	Reason      string
	Operation   Operation
}

// finishCheckOutcome applies the same transition kind to Run, Task and
// Attempt and saves the operation, all within the caller's transaction.
func finishCheckOutcome(ctx context.Context, uow UnitOfWork, args *checkOutcomeArgs) error {
	if err := uow.Operations().Save(ctx, args.Operation); err != nil {
		return err
	}
	return finishCheckEntities(ctx, uow, args)
}

// finishCheckEntities applies the same transition kind to Run, Task and
// Attempt with their transition-evidence rows, within the caller's
// transaction. Stop precedence never declares the run stopped here: the
// worker may still be live, so only DriveStop marks stopped once
// termination of every piece of owned work has been observed.
func finishCheckEntities(ctx context.Context, uow UnitOfWork, args *checkOutcomeArgs) error {
	var (
		rNext run.Run
		tNext run.Task
		aNext run.Attempt
		err   error
	)
	switch args.Kind {
	case entityComplete:
		if rNext, err = args.Run.Complete(args.Now); err == nil {
			if tNext, err = args.Task.Complete(args.Now); err == nil {
				aNext, err = args.Attempt.Complete(args.Now)
			}
		}
	case entityFail:
		if rNext, err = args.Run.Fail(args.Now); err == nil {
			if tNext, err = args.Task.Fail(args.Now); err == nil {
				aNext, err = args.Attempt.Fail(args.Now)
			}
		}
	case entityFailKeepRun:
		rNext = args.Run
		if tNext, err = args.Task.Fail(args.Now); err == nil {
			aNext, err = args.Attempt.Fail(args.Now)
		}
	case entityInterrupt:
		rNext = args.Run
		if tNext, err = args.Task.Interrupt(args.Now); err == nil {
			aNext, err = args.Attempt.Interrupt(args.Now)
		}
	}
	if err != nil {
		return err
	}
	if _, err := uow.Runs().Save(ctx, rNext, args.RunRevision); err != nil {
		return err
	}
	if _, err := uow.Tasks().Save(ctx, tNext, args.TaskRev); err != nil {
		return err
	}
	if _, err := uow.Attempts().Save(ctx, aNext, args.AttemptRev); err != nil {
		return err
	}
	if rNext.State != args.Run.State {
		if err := recordTransition(ctx, uow, EntityRun, args.Run.ID.String(), string(args.Run.State), string(rNext.State), args.Reason, args.Generation, args.Now); err != nil {
			return err
		}
	}
	if err := recordTransition(ctx, uow, EntityTask, args.Task.ID.String(), string(args.Task.State), string(tNext.State), args.Reason, args.Generation, args.Now); err != nil {
		return err
	}
	return recordTransition(ctx, uow, EntityAttempt, args.Attempt.ID.String(), string(args.Attempt.State), string(aNext.State), args.Reason, args.Generation, args.Now)
}

// withHOPStateDir returns env with HOP_STATE_DIR present and set to
// stateRoot, overriding any value env already carries for it.
func withHOPStateDir(env []string, stateRoot string) []string {
	const name = "HOP_STATE_DIR="
	filtered := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if len(entry) >= len(name) && entry[:len(name)] == name {
			continue
		}
		filtered = append(filtered, entry)
	}
	return append(filtered, name+stateRoot)
}
