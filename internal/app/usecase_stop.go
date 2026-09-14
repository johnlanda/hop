package app

import (
	"context"
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// RequestStop records run's monotonic stop request. It carries no lease:
// hop stop can always request a stop, whether or not it holds the run's
// controller lease.
func (c *Controller) RequestStop(ctx context.Context, runIDStr string) error {
	runID, err := identity.ParseRunID(runIDStr)
	if err != nil {
		return fmt.Errorf("app: parse run id: %w", err)
	}
	if err := c.Submissions.RequestStop(ctx, runID); err != nil {
		return fmt.Errorf("app: request stop: %w", err)
	}
	return nil
}

// CheckRunIntent is the OpCheckRun operation's intent payload: what the
// check use case recorded before spawning hop check-exec, and what stop
// and resume's group-retirement classification matches an inspected
// process group's argv against.
type CheckRunIntent struct {
	CheckoutPath string   `json:"checkout_path"`
	CheckArgv    []string `json:"check_argv"`
}

// StopReport is one DriveStop round's outcome.
type StopReport struct {
	RunState   string
	Terminated bool
}

// DriveStop performs one round of stop interruption against handle: it
// interrupts not-yet-acted work directly, retires a pre-exec launch claim
// or a running worker under the Runtime close rule, retires an orphaned
// check process group, and — once every piece of owned work is confirmed
// terminated — marks the run stopped. It is safe to call repeatedly: it
// never resends or recreates anything, and once the run is stopped every
// later call is a no-op reporting Terminated.
func (c *Controller) DriveStop(ctx context.Context, handle RunHandle) (StopReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; stop polling calls this method, never a hot inner loop.
	detail, err := c.Read.LoadRunStatus(ctx, handle.runID)
	if err != nil {
		return StopReport{}, fmt.Errorf("app: load run status: %w", err)
	}
	if detail.State == run.RunStopped {
		return StopReport{RunState: string(detail.State), Terminated: true}, nil
	}
	if detail.State != run.RunStopping {
		// A stop request was recorded but this run never reached a
		// stopping source state (RunRequestStop is monotonic but only
		// transitions state from a valid source); nothing to drive.
		return StopReport{RunState: string(detail.State)}, nil
	}

	switch detail.AttemptState {
	case run.AttemptReserved:
		return c.stopReserved(ctx, handle, detail)
	case run.AttemptLaunching, run.AttemptRelaunching:
		return c.stopLaunching(ctx, handle, detail)
	case run.AttemptRunning, run.AttemptSubmitted:
		return c.stopRunning(ctx, handle, detail)
	case run.AttemptChecking:
		return c.stopChecking(ctx, handle, detail)
	default:
		// interrupted, completed, failed or reconciling: nothing owned is
		// left to interrupt; only Run's own transition remains.
		return c.finishStop(ctx, handle, detail, "owned work already settled")
	}
}

// stopReserved interrupts an attempt that never launched: a reserved
// attempt is never launched (section 5), so there is no act to perform.
func (c *Controller) stopReserved(ctx context.Context, handle RunHandle, detail RunDetail) (StopReport, error) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; this runs once per stop round, never a hot loop.
	now := c.Clock.Now()
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		generation := gen(handle.lease.Generation)

		t, tRev, getErr := uow.Tasks().Get(ctx, detail.TaskID)
		if getErr != nil {
			return getErr
		}
		tFrom := t.State
		t, interruptErr := t.Interrupt(now)
		if interruptErr != nil {
			return interruptErr
		}
		if _, saveErr := uow.Tasks().Save(ctx, t, tRev); saveErr != nil {
			return saveErr
		}
		if transErr := recordTransition(ctx, uow, EntityTask, detail.TaskID.String(), string(tFrom), string(t.State), "stop before launch", generation, now); transErr != nil {
			return transErr
		}

		a, aRev, getErr := uow.Attempts().Get(ctx, detail.AttemptID)
		if getErr != nil {
			return getErr
		}
		aFrom := a.State
		a, interruptErr = a.Interrupt(now)
		if interruptErr != nil {
			return interruptErr
		}
		if _, saveErr := uow.Attempts().Save(ctx, a, aRev); saveErr != nil {
			return saveErr
		}
		if transErr := recordTransition(ctx, uow, EntityAttempt, detail.AttemptID.String(), string(aFrom), string(a.State), "stop before launch", generation, now); transErr != nil {
			return transErr
		}

		if detail.SessionID != "" {
			if transErr := terminateSession(ctx, uow, detail.SessionID, "stop before launch", generation, now); transErr != nil {
				return transErr
			}
		}

		return markRunStopped(ctx, uow, handle.runID, generation, now)
	})
	if err != nil {
		return StopReport{}, fmt.Errorf("app: stop reserved attempt: %w", err)
	}
	return StopReport{RunState: string(run.RunStopped), Terminated: true}, nil
}

// stopLaunching retires a pre-exec (or already-failed) launch claim under
// the close rule, then interrupts the attempt once retirement is
// confirmed. A nil claim, or one whose evidence fails to match, leaves the
// run reconciling rather than closing blindly.
func (c *Controller) stopLaunching(ctx context.Context, handle RunHandle, detail RunDetail) (StopReport, error) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; this runs once per stop round, never a hot loop.
	if detail.Claim == nil {
		return StopReport{RunState: string(run.RunStopping)}, nil
	}
	switch detail.Claim.State {
	case LaunchClaimExecFailed:
		return c.finishStop(ctx, handle, detail, "exec_failed claim; nothing to retire")
	case LaunchClaimExeced:
		return c.stopRunning(ctx, handle, detail)
	case LaunchClaimExecPending:
	}
	if detail.Binding == nil || detail.Binding.PaneID == "" {
		return StopReport{RunState: string(run.RunStopping)}, nil
	}
	evidence := run.OccupantEvidence{Label: detail.Binding.CreationLabel, ArgvMarker: detail.AttemptID.String(), PID: detail.Claim.PID}
	closed, err := c.closeUnderCloseRule(ctx, handle, detail.Binding.PaneID, evidence)
	if err != nil {
		return StopReport{RunState: string(run.RunStopping)}, fmt.Errorf("app: close launching pane: %w", err)
	}
	if !closed {
		return StopReport{RunState: string(run.RunStopping)}, nil
	}
	return c.finishStop(ctx, handle, detail, "stop during launch")
}

// stopRunning closes a running worker's pane under the close rule against
// the current binding's occupant evidence, or — when the occupant is
// already gone — treats absence as observed termination.
func (c *Controller) stopRunning(ctx context.Context, handle RunHandle, detail RunDetail) (StopReport, error) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; this runs once per stop round, never a hot loop.
	if detail.Binding == nil || detail.Binding.PaneID == "" || detail.Binding.Occupant == nil {
		return StopReport{RunState: string(run.RunStopping)}, nil
	}
	pane, err := c.Runtime.InspectPane(ctx, detail.Binding.PaneID)
	if err != nil {
		return StopReport{RunState: string(run.RunStopping)}, nil //nolint:nilerr // inspection failure fails closed: stays stopping, never closes blindly.
	}
	if !OccupantMatches(*detail.Binding.Occupant, pane) {
		return c.finishStop(ctx, handle, detail, "worker termination observed")
	}
	if err := c.revalidateForDispatch(ctx, handle, true); err != nil {
		return StopReport{RunState: string(run.RunStopping)}, err
	}
	if err := c.Runtime.ClosePane(ctx, detail.Binding.PaneID); err != nil {
		return StopReport{RunState: string(run.RunStopping)}, fmt.Errorf("app: close running pane: %w", err)
	}
	return StopReport{RunState: string(run.RunStopping)}, nil
}

// stopChecking retires the run's check-exec process group under the
// group-retirement rule and interrupts once retirement is confirmed.
func (c *Controller) stopChecking(ctx context.Context, handle RunHandle, detail RunDetail) (StopReport, error) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; this runs once per stop round, never a hot loop.
	var checkOp *Operation
	for i := range detail.PendingOperations {
		if detail.PendingOperations[i].Kind == OpCheckRun {
			checkOp = &detail.PendingOperations[i]
			break
		}
	}
	if checkOp == nil {
		return StopReport{RunState: string(run.RunStopping)}, nil
	}

	var claim CheckExecClaim
	var found bool
	getErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		var err error
		claim, found, err = uow.CheckExecClaims().Get(ctx, checkOp.ID)
		return err
	})
	if getErr != nil {
		return StopReport{RunState: string(run.RunStopping)}, fmt.Errorf("app: load check-exec claim: %w", getErr)
	}
	if !found {
		return StopReport{RunState: string(run.RunStopping)}, nil
	}

	intent, _ := decodeOperationPayload[CheckRunIntent](checkOp.Intent) // a decode failure leaves expectedArgv nil, which ClassifyGroupRetirement treats as never matching — fails closed, not a panic.
	outcome, retireErr := c.retireGroup(ctx, handle, claim.PID, intent.CheckArgv)
	if retireErr != nil {
		return StopReport{RunState: string(run.RunStopping)}, retireErr
	}
	switch outcome {
	case GroupEmpty, GroupMatched:
		return c.finishStop(ctx, handle, detail, "check process group retired")
	default:
		return StopReport{RunState: string(run.RunStopping)}, nil
	}
}

// closeUnderCloseRule applies the Runtime close rule: it revalidates the
// dispatch (heartbeat, generation), re-inspects the pane and matches the
// occupant against evidence immediately before closing; on mismatch,
// missing identity or inspection failure it fails closed (no close) and
// returns false.
func (c *Controller) closeUnderCloseRule(ctx context.Context, handle RunHandle, paneID string, evidence run.OccupantEvidence) (bool, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per close attempt.
	if err := c.revalidateForDispatch(ctx, handle, true); err != nil {
		return false, err
	}
	pane, err := c.Runtime.InspectPane(ctx, paneID)
	if err != nil {
		return false, nil //nolint:nilerr // inspection failure fails closed: no close, caller stays ambiguous.
	}
	if !OccupantMatches(evidence, pane) {
		return false, nil
	}
	if err := c.Runtime.ClosePane(ctx, paneID); err != nil {
		return false, err
	}
	return true, nil
}

// retireGroup lists pgid through ProcessGroupInspector and classifies it
// against expectedArgv, signaling the group only when matched, after
// pre-dispatch revalidation. A signal failure is returned, never
// discarded: the caller records it as reconciliation evidence.
func (c *Controller) retireGroup(ctx context.Context, handle RunHandle, pgid int, expectedArgv []string) (GroupRetirementOutcome, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per retirement round.
	processes, err := c.Groups.GroupProcesses(ctx, pgid)
	outcome := ClassifyGroupRetirement(processes, err, expectedArgv)
	if outcome != GroupMatched {
		return outcome, nil
	}
	if err := c.revalidateForDispatch(ctx, handle, true); err != nil {
		return outcome, err
	}
	if err := c.Groups.SignalGroup(ctx, pgid); err != nil {
		return outcome, fmt.Errorf("app: signal check process group %d: %w", pgid, err)
	}
	return outcome, nil
}

// finishStop interrupts the run's task, attempt and session (whichever are
// not already terminal) and marks the run stopped, in one transaction, once
// their owned work has been confirmed terminated by the caller.
func (c *Controller) finishStop(ctx context.Context, handle RunHandle, detail RunDetail, reason string) (StopReport, error) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; this runs once per stop completion.
	now := c.Clock.Now()
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		generation := gen(handle.lease.Generation)

		a, aRev, getErr := uow.Attempts().Get(ctx, detail.AttemptID)
		if getErr != nil {
			return getErr
		}
		if a.State != run.AttemptInterrupted && a.State != run.AttemptFailed && a.State != run.AttemptCompleted {
			aFrom := a.State
			nextAttempt, interruptErr := a.Interrupt(now)
			if interruptErr != nil {
				return interruptErr
			}
			if _, saveErr := uow.Attempts().Save(ctx, nextAttempt, aRev); saveErr != nil {
				return saveErr
			}
			if transErr := recordTransition(ctx, uow, EntityAttempt, detail.AttemptID.String(), string(aFrom), string(nextAttempt.State), reason, generation, now); transErr != nil {
				return transErr
			}
		}

		t, tRev, getErr := uow.Tasks().Get(ctx, detail.TaskID)
		if getErr != nil {
			return getErr
		}
		if t.State != run.TaskInterrupted && t.State != run.TaskFailed && t.State != run.TaskCompleted {
			tFrom := t.State
			nextTask, interruptErr := t.Interrupt(now)
			if interruptErr != nil {
				return interruptErr
			}
			if _, saveErr := uow.Tasks().Save(ctx, nextTask, tRev); saveErr != nil {
				return saveErr
			}
			if transErr := recordTransition(ctx, uow, EntityTask, detail.TaskID.String(), string(tFrom), string(nextTask.State), reason, generation, now); transErr != nil {
				return transErr
			}
		}

		if detail.SessionID != "" {
			if transErr := terminateSession(ctx, uow, detail.SessionID, reason, generation, now); transErr != nil {
				return transErr
			}
		}

		return markRunStopped(ctx, uow, handle.runID, generation, now)
	})
	if err != nil {
		return StopReport{}, fmt.Errorf("app: finish stop: %w", err)
	}
	return StopReport{RunState: string(run.RunStopped), Terminated: true}, nil
}

// markRunStopped moves run to stopped and records the transition.
func markRunStopped(ctx context.Context, uow UnitOfWork, runID identity.RunID, generation *int64, now time.Time) error {
	r, rRev, err := uow.Runs().Get(ctx, runID)
	if err != nil {
		return err
	}
	rFrom := r.State
	r, err = r.MarkStopped(now)
	if err != nil {
		return err
	}
	if _, err := uow.Runs().Save(ctx, r, rRev); err != nil {
		return err
	}
	return recordTransition(ctx, uow, EntityRun, runID.String(), string(rFrom), string(r.State), "owned work terminated", generation, now)
}

// terminateSession moves session to terminated when it is not already, and
// records the transition.
func terminateSession(ctx context.Context, uow UnitOfWork, sessionID identity.SessionID, reason string, generation *int64, now time.Time) error {
	s, sRev, err := uow.Sessions().Get(ctx, sessionID)
	if err != nil {
		return err
	}
	if s.State == run.SessionTerminated {
		return nil
	}
	// Terminate is invalid directly from active (section 5: "a stop against
	// a corroborated live process goes through launching → stopping" first).
	// reserved, launching, stopping and reconciling all terminate directly.
	if s.State == run.SessionActive {
		stoppingFrom := s.State
		s, err = s.Stop(now)
		if err != nil {
			return err
		}
		sRev, err = uow.Sessions().Save(ctx, s, sRev)
		if err != nil {
			return err
		}
		if transErr := recordTransition(ctx, uow, EntitySession, sessionID.String(), string(stoppingFrom), string(s.State), reason, generation, now); transErr != nil {
			return transErr
		}
	}

	sFrom := s.State
	s, err = s.Terminate(now)
	if err != nil {
		return err
	}
	if _, err := uow.Sessions().Save(ctx, s, sRev); err != nil {
		return err
	}
	return recordTransition(ctx, uow, EntitySession, sessionID.String(), string(sFrom), string(s.State), reason, generation, now)
}
