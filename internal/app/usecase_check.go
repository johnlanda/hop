package app

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// checkRunOutcome is the OpCheckRun operation's outcome payload.
type checkRunOutcome struct {
	ExitCode int  `json:"exit_code"`
	Unknown  bool `json:"unknown"` // the outcome could not be determined (crash recovery)
}

// CheckReport is one ClaimAndRunCheck round's outcome.
type CheckReport struct {
	Ran         bool // false when there was no pending check request to claim
	OperationID string
	Passed      bool
	Interrupted bool
	Unknown     bool // unrepeatable unknown outcome: the run is left actionable, not completed
}

// ClaimAndRunCheck claims the run's oldest pending check request, if any,
// and drives one check execution start to finish: a detached checkout of
// the accepted result's commit, hop check-exec spawned via CommandRunner,
// and the outcome transaction (docs/plan/phase-2-design.md section 7).
// spawnEnv is hop check-exec's complete spawned environment (composition
// builds it, ordinarily SanitizeEnvironment(os.Environ(), the frozen
// policy)) — this method only guarantees HOP_STATE_DIR is present,
// overriding any value spawnEnv already carries for it. hopPath is the
// absolute path to the running hop executable.
func (c *Controller) ClaimAndRunCheck(ctx context.Context, handle RunHandle, hopPath, repositoryRoot, stateRoot string, checkArgv []string, checkRepeatable bool, spawnEnv []string) (CheckReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; the controller's check-consuming loop calls this once per pending request.
	claimed, checkRequest, err := c.claimCheckRequest(ctx, handle)
	if err != nil {
		return CheckReport{}, err
	}
	if !claimed {
		return CheckReport{}, nil
	}

	commitOID, err := c.acceptedCommit(ctx, handle, checkRequest.AttemptID)
	if err != nil {
		return CheckReport{}, err
	}

	opID, err := c.newOperationID()
	if err != nil {
		return CheckReport{}, err
	}
	checkoutPath := filepath.Join(stateRoot, "runs", handle.runID.String(), "checks", opID.String(), "tree")
	intent := checkRunIntent{CheckoutPath: checkoutPath, CheckArgv: checkArgv}
	now := c.Clock.Now()

	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		r, rRev, getErr := uow.Runs().Get(ctx, handle.runID)
		if getErr != nil {
			return getErr
		}
		rFrom := r.State
		r, completeErr := r.EnterCompleting(now)
		if completeErr != nil {
			return completeErr
		}
		if _, saveErr := uow.Runs().Save(ctx, r, rRev); saveErr != nil {
			return saveErr
		}
		generation := gen(handle.lease.Generation)
		if transErr := recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(rFrom), string(r.State), "accepted result's check claimed", generation, now); transErr != nil {
			return transErr
		}
		return uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpCheckRun, State: OperationPending, Intent: intent,
			CreatedAt: now, UpdatedAt: now,
		})
	}); err != nil {
		return CheckReport{}, fmt.Errorf("app: record check.run intent: %w", err)
	}

	if err := c.materializeCheckout(ctx, repositoryRoot, checkoutPath, commitOID); err != nil {
		return c.recordCheckOutcome(ctx, handle, opID, checkRequest.AttemptID, checkRunOutcome{Unknown: true}, false, fmt.Errorf("materialize checkout: %w", err))
	}
	defer c.removeCheckout(ctx, repositoryRoot, checkoutPath)

	spawnEnv = withHOPStateDir(spawnEnv, stateRoot)
	spawnArgv := append([]string{hopPath, "check-exec", "--op", opID.String(), "--"}, checkArgv...)
	cmdResult, runErr := c.Commands.Run(ctx, Command{Argv: spawnArgv, Dir: checkoutPath, Env: spawnEnv})
	if runErr != nil {
		return c.recordCheckOutcome(ctx, handle, opID, checkRequest.AttemptID, checkRunOutcome{Unknown: true}, checkRepeatable, fmt.Errorf("spawn hop check-exec: %w", runErr))
	}

	outcome := checkRunOutcome{ExitCode: cmdResult.ExitCode}
	return c.recordCheckOutcome(ctx, handle, opID, checkRequest.AttemptID, outcome, checkRepeatable, nil)
}

// claimCheckRequest claims the run's oldest pending check request, marking
// it CheckRequestClaimed with the current generation.
func (c *Controller) claimCheckRequest(ctx context.Context, handle RunHandle) (bool, CheckRequest, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per claim attempt.
	var (
		claimed bool
		request CheckRequest
	)
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		pending, found, getErr := uow.CheckRequests().Pending(ctx, handle.runID)
		if getErr != nil {
			return getErr
		}
		if !found {
			return nil
		}
		generation := handle.lease.Generation
		pending.State = CheckRequestClaimed
		pending.ClaimedGeneration = &generation
		if saveErr := uow.CheckRequests().Save(ctx, pending); saveErr != nil {
			return saveErr
		}
		claimed = true
		request = pending
		return nil
	})
	return claimed, request, err
}

// acceptedCommit reads the accepted result's commit for attempt.
func (c *Controller) acceptedCommit(ctx context.Context, handle RunHandle, attempt identity.AttemptID) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per check claim.
	var commit string
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		result, getErr := uow.Results().Accepted(ctx, attempt)
		if getErr != nil {
			return getErr
		}
		if result == nil {
			return fmt.Errorf("app: attempt %s has no accepted result", attempt)
		}
		commit = result.CommitOID
		return nil
	})
	return commit, err
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
// outputs under the operation's own directory stay.
func (c *Controller) removeCheckout(ctx context.Context, repositoryRoot, checkoutPath string) {
	_, _ = c.runGit(ctx, repositoryRoot, "worktree", "remove", "--force", checkoutPath) //nolint:errcheck // best-effort cleanup; a leaked checkout is evidence, not corruption, since executions never share paths.
}

// recordCheckOutcome applies the section 7 outcome transaction: it
// re-reads the stop request and decides completion, failure, interruption
// or the unknown-outcome rule.
func (c *Controller) recordCheckOutcome(ctx context.Context, handle RunHandle, opID identity.OperationID, attempt identity.AttemptID, outcome checkRunOutcome, repeatable bool, actErr error) (CheckReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per check execution.
	now := c.Clock.Now()
	report := CheckReport{Ran: true, OperationID: opID.String()}

	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		op, getErr := uow.Operations().Get(ctx, opID)
		if getErr != nil {
			return getErr
		}
		op.UpdatedAt = now
		op.Outcome = outcome

		r, rRev, getErr := uow.Runs().Get(ctx, handle.runID)
		if getErr != nil {
			return getErr
		}
		a, aRev, getErr := uow.Attempts().Get(ctx, attempt)
		if getErr != nil {
			return getErr
		}
		t, tRev, getErr := uow.Tasks().Get(ctx, a.TaskID)
		if getErr != nil {
			return getErr
		}
		generation := gen(handle.lease.Generation)

		switch {
		case r.StopRequested:
			op.State = OperationSucceeded
			report.Interrupted = true
			return finishCheckOutcome(ctx, uow, &checkOutcomeArgs{r, rRev, a, aRev, t, tRev, generation, now, entityInterrupt, "stop precedence", op})
		case outcome.Unknown && repeatable:
			op.State = OperationFailed
			if saveErr := uow.Operations().Save(ctx, op); saveErr != nil {
				return saveErr
			}
			rFrom := r.State
			nextRun, runErr := r.MarkRunning(now)
			if runErr != nil {
				return runErr
			}
			if _, saveErr := uow.Runs().Save(ctx, nextRun, rRev); saveErr != nil {
				return saveErr
			}
			report.Unknown = true
			return recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(rFrom), string(nextRun.State), "repeatable check unknown outcome requeued", generation, now)
		case outcome.Unknown:
			op.State = OperationFailed
			report.Unknown = true
			return finishCheckOutcome(ctx, uow, &checkOutcomeArgs{r, rRev, a, aRev, t, tRev, generation, now, entityFail, "unrepeatable unknown check outcome", op})
		case actErr == nil && outcome.ExitCode == 0:
			op.State = OperationSucceeded
			report.Passed = true
			return finishCheckOutcome(ctx, uow, &checkOutcomeArgs{r, rRev, a, aRev, t, tRev, generation, now, entityComplete, "passing check receipt", op})
		default:
			op.State = OperationFailed
			return finishCheckOutcome(ctx, uow, &checkOutcomeArgs{r, rRev, a, aRev, t, tRev, generation, now, entityFail, "failing check", op})
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
	case entityInterrupt:
		if rNext, err = args.Run.MarkStopped(args.Now); err == nil {
			if tNext, err = args.Task.Interrupt(args.Now); err == nil {
				aNext, err = args.Attempt.Interrupt(args.Now)
			}
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
	if err := uow.Operations().Save(ctx, args.Operation); err != nil {
		return err
	}
	if err := recordTransition(ctx, uow, EntityRun, args.Run.ID.String(), string(args.Run.State), string(rNext.State), args.Reason, args.Generation, args.Now); err != nil {
		return err
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
