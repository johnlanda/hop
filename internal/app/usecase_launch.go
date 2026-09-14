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
	// LaunchFailed means the claim settled exec_failed and the run, task,
	// attempt and session were moved to their terminal failure states.
	LaunchFailed LaunchProgress = "failed"
	// LaunchNeedsInteraction means the observation is the unsupported
	// forking-wrapper topology: the claim stays exec_pending and a human
	// decides.
	LaunchNeedsInteraction LaunchProgress = "needs-interaction"
	// LaunchAlreadySettled means a concurrent early-acceptance submission
	// already moved the attempt out of launching/relaunching; corroborating
	// the claim further only applies any lifecycle still missing — a
	// still-launching session is activated — and is otherwise a no-op
	// (docs/plan/phase-2-design.md section 7).
	LaunchAlreadySettled LaunchProgress = "already-settled"
)

// CorroborateLaunch performs one inspection round toward settling a launch
// claim (docs/plan/phase-2-design.md section 6). It never sleeps and never
// resends or recreates the pane; the caller is responsible for pacing
// repeated calls. The expected executable identity comes from the claim
// itself, and the accepted argv markers are derived from durable
// launch/binding context (run, attempt and incarnation identifiers plus
// the session's native reference) — never from caller-selected values.
func (c *Controller) CorroborateLaunch(ctx context.Context, handle RunHandle) (LaunchProgress, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; corroboration polling calls this method, never a hot inner loop.
	detail, err := c.Read.LoadRunStatus(ctx, handle.runID)
	if err != nil {
		return "", fmt.Errorf("app: load run status: %w", err)
	}
	if detail.AttemptState != run.AttemptLaunching && detail.AttemptState != run.AttemptRelaunching {
		return c.settleRemainingLifecycle(ctx, handle, detail)
	}
	if detail.Claim == nil {
		if recoverErr := c.recoverBindingByLabel(ctx, handle, detail); recoverErr != nil {
			return "", recoverErr
		}
		return LaunchPending, nil
	}

	switch detail.Claim.State {
	case LaunchClaimExecFailed:
		return c.settleExecFailed(ctx, handle, *detail.Claim)
	case LaunchClaimExeced:
		// The claim already settled (an adoption path, or a prior
		// generation's settlement whose lifecycle consequences were lost):
		// apply the remaining transitions idempotently.
		return c.settleExeced(ctx, handle, *detail.Claim, nil, "")
	case LaunchClaimExecPending:
		// fall through to inspection below.
	}

	if detail.Binding == nil || detail.Binding.PaneID == "" {
		// The claim exists but the pane.open outcome (and so the binding)
		// was never recorded: recover the pane by its creation label; the
		// next round inspects it.
		if recoverErr := c.recoverBindingByLabel(ctx, handle, detail); recoverErr != nil {
			return "", recoverErr
		}
		return LaunchPending, nil
	}
	pane, err := c.Runtime.InspectPane(ctx, detail.Binding.PaneID)
	if err != nil {
		return LaunchPending, nil //nolint:nilerr // an inspection failure leaves the claim ambiguous, not an error the caller need surface.
	}
	markers, err := c.launchMarkers(ctx, handle, detail)
	if err != nil {
		return "", err
	}
	paneMatches := detail.Binding.IncarnationID == detail.Claim.IncarnationID && !detail.Binding.Superseded
	settlement := CorroborateSettlement(paneMatches, pane, markers, *detail.Claim)
	switch settlement {
	case SettlementSettled:
		return c.settleExeced(ctx, handle, *detail.Claim, &pane, FirstMarkerMatch(pane, markers))
	case SettlementForkingWrapper:
		return LaunchNeedsInteraction, nil
	case SettlementUnresolved:
		return LaunchPending, nil
	}
	return LaunchPending, nil
}

// launchMarkers derives the argv markers the corroboration predicate
// accepts, from durable context only: the run, attempt and (when a claim
// exists) incarnation identifiers, plus the session's native reference —
// the marker a cold `claude --resume <native-ref>` argv carries.
func (c *Controller) launchMarkers(ctx context.Context, handle RunHandle, detail RunDetail) ([]string, error) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call values; called once per corroboration round.
	markers := []string{detail.AttemptID.String(), handle.runID.String()}
	if detail.Claim != nil {
		markers = append(markers, detail.Claim.IncarnationID.String())
	}
	if detail.SessionID != "" {
		ref, err := c.sessionNativeRef(ctx, handle, detail.SessionID)
		if err != nil {
			return nil, err
		}
		if ref != "" {
			markers = append(markers, ref)
		}
	}
	return markers, nil
}

// recoverBindingByLabel looks the newest pending pane.open operation's pane
// up by its creation label and, when found, commits the runtime binding and
// the operation's success — the decision-table recovery for a lost create
// response. Not finding the label is not an error: the create may still be
// in flight, and the operation is never re-sent.
func (c *Controller) recoverBindingByLabel(ctx context.Context, handle RunHandle, detail RunDetail) error { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call values; called once per recovery round.
	if detail.Binding != nil && detail.Binding.PaneID != "" {
		return nil
	}
	op, intent, found := newestPendingPaneOpen(detail.PendingOperations)
	if !found {
		return nil
	}
	ref, paneFound, err := c.Runtime.FindPaneByLabel(ctx, intent.Label)
	if err != nil || !paneFound {
		return nil //nolint:nilerr // a failed or empty label lookup leaves the operation pending; recovery retries on a later round.
	}
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		latest, getErr := uow.Operations().Get(ctx, op.ID)
		if getErr != nil {
			return getErr
		}
		if latest.State != OperationPending && latest.State != OperationReconciling {
			return nil
		}
		binding := run.NewRuntimeBinding(intent.SessionID, intent.IncarnationID, "", ref.WorkspaceID, ref.TabID, ref.PaneID, intent.Label, run.LaunchInitial, now)
		if bindErr := uow.Bindings().Create(ctx, binding); bindErr != nil {
			return bindErr
		}
		latest.State = OperationSucceeded
		latest.ActEvidence = PaneHandle(ref)
		latest.UpdatedAt = now
		return uow.Operations().Save(ctx, latest)
	})
}

// newestPendingPaneOpen returns the newest pending or reconciling pane.open
// (or launch.send) operation and its decoded intent.
func newestPendingPaneOpen(ops []Operation) (Operation, paneOpenIntent, bool) {
	var (
		newest Operation
		intent paneOpenIntent
		found  bool
	)
	for i := range ops {
		if ops[i].Kind != OpPaneOpen && ops[i].Kind != OpLaunchSend {
			continue
		}
		decoded, ok := decodeOperationPayload[paneOpenIntent](ops[i].Intent)
		if !ok || decoded.Label == "" {
			continue
		}
		if !found || ops[i].CreatedAt.After(newest.CreatedAt) {
			newest = ops[i]
			intent = decoded
			found = true
		}
	}
	return newest, intent, found
}

// settleRemainingLifecycle handles corroboration once the attempt has
// already moved past launching/relaunching (the early-acceptance handoff,
// docs/plan/phase-2-design.md section 7): the run/task/attempt transitions
// are the acceptance transaction's, but a still-launching session is
// activated here once the claim is settled — its activation is not part of
// the acceptance transaction.
func (c *Controller) settleRemainingLifecycle(ctx context.Context, handle RunHandle, detail RunDetail) (LaunchProgress, error) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call values; called once per corroboration round.
	if detail.SessionID == "" || detail.Claim == nil || detail.Claim.State != LaunchClaimExeced {
		return LaunchAlreadySettled, nil
	}
	now := c.Clock.Now()
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		s, sRev, getErr := uow.Sessions().Get(ctx, detail.SessionID)
		if getErr != nil {
			return getErr
		}
		if s.State != run.SessionLaunching {
			return nil
		}
		sFrom := s.State
		next, confirmErr := s.ConfirmActive(now)
		if confirmErr != nil {
			return confirmErr
		}
		if _, saveErr := uow.Sessions().Save(ctx, next, sRev); saveErr != nil {
			return saveErr
		}
		return recordTransition(ctx, uow, EntitySession, detail.SessionID.String(), string(sFrom), string(next.State), "launch claim settled execed", gen(handle.lease.Generation), now)
	})
	if err != nil {
		return "", fmt.Errorf("app: activate early-accepted session: %w", err)
	}
	return LaunchAlreadySettled, nil
}

// settleExeced commits the section 5 Run/Attempt/Session "claim settled
// execed" transitions, records the controller's LaunchClaims settlement,
// and — when a fresh observation is supplied — records it as the current
// binding's occupant evidence. A nil pane applies only the lifecycle
// transitions still missing for an already-settled claim, idempotently.
func (c *Controller) settleExeced(ctx context.Context, handle RunHandle, claim LaunchClaim, pane *PaneProcess, marker string) (LaunchProgress, error) { //nolint:gocritic // hugeParam: RunHandle and LaunchClaim carry design-fixed value shapes; this runs once per settlement, never a hot loop.
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
		attemptMoved := false
		if a.State == run.AttemptLaunching || a.State == run.AttemptRelaunching {
			var runErr error
			if a, runErr = a.MarkRunning(now); runErr != nil {
				return runErr
			}
			if _, saveErr := uow.Attempts().Save(ctx, a, aRev); saveErr != nil {
				return saveErr
			}
			attemptMoved = true
		}

		if claim.State == LaunchClaimExecPending {
			fgPID := claim.PID
			if pane != nil {
				fgPID = firstForeground(*pane).PID
			}
			if settleErr := uow.LaunchClaims().Settle(ctx, claim.IncarnationID, LaunchClaimSettlement{
				State: LaunchClaimExeced, PID: fgPID, Executable: claim.Executable, ArgvMarker: marker, At: now,
			}); settleErr != nil {
				return settleErr
			}
		}

		s, sRev, getErr := uow.Sessions().Current(ctx, claim.AttemptID)
		if getErr != nil {
			return getErr
		}
		if pane != nil && marker != "" {
			binding, found, getErr := uow.Bindings().Current(ctx, s.ID)
			if getErr != nil {
				return getErr
			}
			if found {
				evidence := run.OccupantEvidence{Label: binding.CreationLabel, ArgvMarker: marker, PID: firstForeground(*pane).PID}
				observed, observeErr := binding.Observe(evidence, now)
				if observeErr != nil {
					return observeErr
				}
				if saveErr := uow.Bindings().Save(ctx, observed); saveErr != nil {
					return saveErr
				}
			}
		}

		generation := gen(handle.lease.Generation)
		if s.State == run.SessionLaunching || s.State == run.SessionReconciling {
			sFrom := s.State
			var runErr error
			if s, runErr = s.ConfirmActive(now); runErr != nil {
				return runErr
			}
			if _, saveErr := uow.Sessions().Save(ctx, s, sRev); saveErr != nil {
				return saveErr
			}
			if transErr := recordTransition(ctx, uow, EntitySession, s.ID.String(), string(sFrom), string(s.State), "launch claim settled execed", generation, now); transErr != nil {
				return transErr
			}
		}

		if attemptMoved {
			return recordTransition(ctx, uow, EntityAttempt, claim.AttemptID.String(), string(aFrom), string(a.State), "launch claim settled execed", generation, now)
		}
		return nil
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
// transitions for Attempt, Task, Run and Session (reference trace 3:
// Session' launching→terminated). The claim itself is already exec_failed
// by the launcher's own error-path write
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
		if s, _, sessErr := uow.Sessions().Current(ctx, claim.AttemptID); sessErr == nil {
			if termErr := terminateSession(ctx, uow, s.ID, "exec_failed claim", generation, now); termErr != nil {
				return termErr
			}
		}

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
