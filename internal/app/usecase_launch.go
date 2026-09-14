package app

import (
	"context"
	"fmt"

	"github.com/johnlanda/hop/internal/domain/run"
)

// LaunchProgress is CorroborateLaunch's report of one inspection round.
type LaunchProgress string

// Launch progress values.
const (
	// LaunchPending means the claim has not settled yet and the deadline
	// has not passed; the caller should call CorroborateLaunch again.
	LaunchPending LaunchProgress = "pending"
	// LaunchSettled means the claim settled and Run/Task/Attempt/Session
	// advanced accordingly.
	LaunchSettled LaunchProgress = "settled"
	// LaunchFailed means the claim settled exec_failed and the run, task
	// and attempt were moved to their terminal failure states.
	LaunchFailed LaunchProgress = "failed"
	// LaunchNeedsInteraction means the observation is the unsupported
	// forking-wrapper topology: the claim stays exec_pending and a human
	// decides.
	LaunchNeedsInteraction LaunchProgress = "needs-interaction"
	// LaunchAlreadySettled means a concurrent early-acceptance submission
	// already moved the attempt out of launching/relaunching; corroborating
	// the claim further is a no-op (docs/plan/phase-2-design.md section 7).
	LaunchAlreadySettled LaunchProgress = "already-settled"
)

// CorroborateLaunch performs one inspection round toward settling a launch
// claim (docs/plan/phase-2-design.md section 6). It never sleeps and never
// resends or recreates the pane; the caller is responsible for pacing
// repeated calls and for giving up only past LaunchClaimDeadline. marker is
// the identifier expected in the occupant's argv: the attempt id while the
// claim is unsettled (the launcher's own argv), or the harness's native
// session reference once execed — the caller supplies whichever is
// meaningful for the current claim state.
func (c *Controller) CorroborateLaunch(ctx context.Context, handle RunHandle, expectedExecutable, marker string) (LaunchProgress, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; corroboration polling calls this method, never a hot inner loop.
	detail, err := c.Read.LoadRunStatus(ctx, handle.runID)
	if err != nil {
		return "", fmt.Errorf("app: load run status: %w", err)
	}
	if detail.AttemptState != run.AttemptLaunching && detail.AttemptState != run.AttemptRelaunching {
		return LaunchAlreadySettled, nil
	}
	if detail.Claim == nil {
		return LaunchPending, nil
	}

	switch detail.Claim.State {
	case LaunchClaimExecFailed:
		return c.settleExecFailed(ctx, handle, *detail.Claim)
	case LaunchClaimExeced:
		return LaunchAlreadySettled, nil
	case LaunchClaimExecPending:
		// fall through to inspection below.
	}

	if detail.Binding.PaneID == "" {
		return LaunchPending, nil
	}
	pane, err := c.Runtime.InspectPane(ctx, detail.Binding.PaneID)
	if err != nil {
		return LaunchPending, nil //nolint:nilerr // an inspection failure leaves the claim ambiguous, not an error the caller need surface.
	}
	settlement := CorroborateSettlement(true, pane, expectedExecutable, marker, *detail.Claim)
	switch settlement {
	case SettlementSettled:
		return c.settleExeced(ctx, handle, *detail.Claim, pane, expectedExecutable, marker)
	case SettlementForkingWrapper:
		return LaunchNeedsInteraction, nil
	case SettlementUnresolved:
		return LaunchPending, nil
	}
	return LaunchPending, nil
}

// settleExeced commits the section 5 Run/Attempt/Session "claim settled
// execed" transitions, records the controller's LaunchClaims settlement,
// and records the observation as the current binding's occupant evidence.
func (c *Controller) settleExeced(ctx context.Context, handle RunHandle, claim LaunchClaim, pane PaneProcess, expectedExecutable, marker string) (LaunchProgress, error) { //nolint:gocritic // hugeParam: RunHandle and LaunchClaim carry design-fixed value shapes; this runs once per settlement, never a hot loop.
	now := c.Clock.Now()
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		r, rRev, getErr := uow.Runs().Get(ctx, handle.runID)
		if getErr != nil {
			return getErr
		}
		if r.State == run.RunLaunching {
			nextRun, launchErr := r.MarkRunning(now)
			if launchErr != nil {
				return launchErr
			}
			if _, saveErr := uow.Runs().Save(ctx, nextRun, rRev); saveErr != nil {
				return saveErr
			}
		}

		a, aRev, getErr := uow.Attempts().Get(ctx, claim.AttemptID)
		if getErr != nil {
			return getErr
		}
		aFrom := a.State
		a, runErr := a.MarkRunning(now)
		if runErr != nil {
			return runErr
		}
		if _, saveErr := uow.Attempts().Save(ctx, a, aRev); saveErr != nil {
			return saveErr
		}

		fg := firstForeground(pane)
		if settleErr := uow.LaunchClaims().Settle(ctx, claim.IncarnationID, LaunchClaimSettlement{
			State: LaunchClaimExeced, PID: fg.PID, Executable: expectedExecutable, ArgvMarker: marker, At: now,
		}); settleErr != nil {
			return settleErr
		}

		s, sRev, getErr := uow.Sessions().Current(ctx, claim.AttemptID)
		if getErr != nil {
			return getErr
		}
		binding, found, getErr := uow.Bindings().Current(ctx, s.ID)
		if getErr != nil {
			return getErr
		}
		if found {
			evidence := run.OccupantEvidence{Label: binding.CreationLabel, ArgvMarker: marker, PID: fg.PID}
			binding, runErr = binding.Observe(evidence, now)
			if runErr != nil {
				return runErr
			}
			if saveErr := uow.Bindings().Save(ctx, binding); saveErr != nil {
				return saveErr
			}
		}

		generation := gen(handle.lease.Generation)
		if s.State == run.SessionLaunching || s.State == run.SessionReconciling {
			sFrom := s.State
			s, runErr = s.ConfirmActive(now)
			if runErr != nil {
				return runErr
			}
			if _, saveErr := uow.Sessions().Save(ctx, s, sRev); saveErr != nil {
				return saveErr
			}
			if transErr := recordTransition(ctx, uow, EntitySession, s.ID.String(), string(sFrom), string(s.State), "launch claim settled execed", generation, now); transErr != nil {
				return transErr
			}
		}

		return recordTransition(ctx, uow, EntityAttempt, claim.AttemptID.String(), string(aFrom), string(a.State), "launch claim settled execed", generation, now)
	})
	if err != nil {
		return "", fmt.Errorf("app: settle launch claim: %w", err)
	}
	return LaunchSettled, nil
}

// firstForeground returns pane's first foreground process, or the zero
// ProcessInfo when none was observed.
func firstForeground(pane PaneProcess) ProcessInfo {
	if len(pane.Foreground) == 0 {
		return ProcessInfo{}
	}
	return pane.Foreground[0]
}

// settleExecFailed commits the section 5 "exec_failed claim" failure
// transitions for Attempt, Task and Run. The claim itself is already
// exec_failed by the launcher's own error-path write
// (SubmissionStore.SettleLaunchFailure); this only applies its
// consequences under the lease.
func (c *Controller) settleExecFailed(ctx context.Context, handle RunHandle, claim LaunchClaim) (LaunchProgress, error) { //nolint:gocritic // hugeParam: RunHandle and LaunchClaim carry design-fixed value shapes; this runs once per settlement, never a hot loop.
	now := c.Clock.Now()
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		a, aRev, getErr := uow.Attempts().Get(ctx, claim.AttemptID)
		if getErr != nil {
			return getErr
		}
		aFrom := a.State
		a, failErr := a.Fail(now)
		if failErr != nil {
			return failErr
		}
		if _, saveErr := uow.Attempts().Save(ctx, a, aRev); saveErr != nil {
			return saveErr
		}

		t, tRev, getErr := uow.Tasks().Get(ctx, a.TaskID)
		if getErr != nil {
			return getErr
		}
		tFrom := t.State
		t, failErr = t.Fail(now)
		if failErr != nil {
			return failErr
		}
		if _, saveErr := uow.Tasks().Save(ctx, t, tRev); saveErr != nil {
			return saveErr
		}

		r, rRev, getErr := uow.Runs().Get(ctx, handle.runID)
		if getErr != nil {
			return getErr
		}
		rFrom := r.State
		r, failErr = r.Fail(now)
		if failErr != nil {
			return failErr
		}
		if _, saveErr := uow.Runs().Save(ctx, r, rRev); saveErr != nil {
			return saveErr
		}

		generation := gen(handle.lease.Generation)
		if transErr := recordTransition(ctx, uow, EntityAttempt, claim.AttemptID.String(), string(aFrom), string(a.State), "exec_failed claim", generation, now); transErr != nil {
			return transErr
		}
		if transErr := recordTransition(ctx, uow, EntityTask, t.ID.String(), string(tFrom), string(t.State), "exec_failed claim", generation, now); transErr != nil {
			return transErr
		}
		return recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(rFrom), string(r.State), "exec_failed claim", generation, now)
	})
	if err != nil {
		return "", fmt.Errorf("app: settle exec_failed claim: %w", err)
	}
	return LaunchFailed, nil
}
