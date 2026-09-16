package app

import (
	"context"
	"fmt"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// SessionLaunchProgress is one CorroborateSessionLaunches round's report
// for one session, mirroring LaunchProgress's vocabulary.
type SessionLaunchProgress struct {
	SessionID string
	Role      string
	Progress  LaunchProgress
}

// CorroborateSessionLaunches performs one inspection round for every
// non-terminal session of a feature-mode run currently in SessionLaunching
// state — manager, implementers and reviewer alike
// (docs/plan/phase-3-design.md section 6, "corroborate launches" in the
// extended scheduling pass, L796-798). It is the per-session sibling of
// Phase 2's CorroborateLaunch: orchestration only, every occupant decision
// reuses CorroborateSettlement verbatim, and Phase 2's CorroborateLaunch
// and its RunDetail-bound helpers (driveLaunchDeadline,
// recoverBindingByLabel) are untouched — the solo path stays exactly as it
// was.
func (c *Controller) CorroborateSessionLaunches(ctx context.Context, handle RunHandle) ([]SessionLaunchProgress, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per scheduling pass.
	sessions, err := c.featureRunSessions(ctx, handle)
	if err != nil {
		return nil, err
	}
	var reports []SessionLaunchProgress
	for i := range sessions {
		session := sessions[i]
		if session.State != run.SessionLaunching {
			continue
		}
		progress, corrErr := c.corroborateSessionLaunch(ctx, handle, &session)
		if corrErr != nil {
			return reports, corrErr
		}
		reports = append(reports, SessionLaunchProgress{SessionID: session.ID.String(), Role: string(session.Role), Progress: progress})
	}
	return reports, nil
}

// corroborateSessionLaunch performs one inspection round for one session,
// mirroring CorroborateLaunch's branches against session-keyed context
// (sessionCloseEvidence) instead of RunDetail's single attempt/claim/
// binding fields.
func (c *Controller) corroborateSessionLaunch(ctx context.Context, handle RunHandle, session *run.Session) (LaunchProgress, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per session per scheduling pass.
	binding, bindingFound, claim, claimFound, markers, err := c.sessionCloseEvidence(ctx, handle, session)
	if err != nil {
		return "", err
	}
	if !claimFound {
		if recoverErr := c.recoverSessionBindingByLabel(ctx, handle, session, bindingFound, binding); recoverErr != nil {
			return "", recoverErr
		}
		overdue, deadlineErr := c.driveSessionLaunchDeadline(ctx, handle, session, bindingFound, binding)
		if deadlineErr != nil {
			return "", deadlineErr
		}
		if overdue {
			return LaunchOverdue, nil
		}
		return LaunchPending, nil
	}

	switch claim.State {
	case LaunchClaimExecFailed:
		if termErr := c.terminateRetiredSession(ctx, handle, session.ID, "exec_failed claim"); termErr != nil {
			return "", termErr
		}
		return LaunchFailed, nil
	case LaunchClaimExeced:
		// Already settled (an adoption path, or a prior round's settlement
		// whose session activation was lost): apply what is still missing,
		// idempotently. No fresh occupant evidence is available here.
		if confirmErr := c.confirmSessionActive(ctx, handle, session.ID, "launch claim settled execed"); confirmErr != nil {
			return "", confirmErr
		}
		return LaunchSettled, nil
	case LaunchClaimExecPending:
		// fall through to inspection below.
	}

	if !bindingFound || binding.PaneID == "" {
		if recoverErr := c.recoverSessionBindingByLabel(ctx, handle, session, bindingFound, binding); recoverErr != nil {
			return "", recoverErr
		}
		return LaunchPending, nil
	}
	pane, err := c.Runtime.InspectPane(ctx, binding.PaneID)
	if err != nil {
		return LaunchPending, nil //nolint:nilerr // an inspection failure leaves the claim ambiguous, not an error the caller need surface.
	}
	// binding was resolved as the claim's OWN incarnation (sessionCloseEvidence
	// looks the claim up by binding.IncarnationID), so only supersession is
	// left to check.
	paneMatches := !binding.Superseded
	settlement, occupant := CorroborateSettlement(paneMatches, pane, markers, claim)
	switch settlement {
	case SettlementSettled:
		if err := c.settleSessionExeced(ctx, handle, session, &claim, &occupant); err != nil {
			return "", err
		}
		return LaunchSettled, nil
	case SettlementForkingWrapper:
		if err := c.markSessionReconciling(ctx, handle, session.ID); err != nil {
			return "", err
		}
		return LaunchNeedsInteraction, nil
	case SettlementUnresolved:
		return LaunchPending, nil
	}
	return LaunchPending, nil
}

// settleSessionExeced records one feature-mode session's launch-claim
// settlement: the claim's exec_pending -> execed transition with the
// corroborated occupant's pid and marker as evidence (never an arbitrary
// foreground member), that evidence also recorded on the session's current
// binding, its bound attempt's launching/relaunching -> running transition
// when it has one (the manager has none), and the session's own
// activation — mirroring Phase 2's settleExeced without the Run-level
// transition, which belongs to the scheduling pass driving the run
// overall, never to one session's settlement.
func (c *Controller) settleSessionExeced(ctx context.Context, handle RunHandle, session *run.Session, claim *LaunchClaim, occupant *SettlementEvidence) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per session settlement.
	now := c.Clock.Now()
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		if claim.State == LaunchClaimExecPending {
			if settleErr := uow.LaunchClaims().Settle(ctx, claim.IncarnationID, LaunchClaimSettlement{
				State: LaunchClaimExeced, PID: occupant.Occupant.PID, Executable: claim.Executable, ArgvMarker: occupant.Marker, At: now,
			}); settleErr != nil {
				return settleErr
			}
		}

		if session.AttemptID != "" {
			a, aRev, getErr := uow.Attempts().Get(ctx, session.AttemptID)
			if getErr != nil {
				return getErr
			}
			if a.State == run.AttemptLaunching || a.State == run.AttemptRelaunching {
				aFrom := a.State
				next, runErr := a.MarkRunning(now)
				if runErr != nil {
					return runErr
				}
				if _, saveErr := uow.Attempts().Save(ctx, next, aRev); saveErr != nil {
					return saveErr
				}
				if transErr := recordTransition(ctx, uow, EntityAttempt, session.AttemptID.String(), string(aFrom), string(next.State), "launch claim settled execed", gen(handle.lease.Generation), now); transErr != nil {
					return transErr
				}
			}
		}

		if occupant.Marker != "" {
			binding, found, getErr := uow.Bindings().Current(ctx, session.ID)
			if getErr != nil {
				return getErr
			}
			if found {
				evidence := run.OccupantEvidence{Label: binding.CreationLabel, ArgvMarker: occupant.Marker, PID: occupant.Occupant.PID}
				observed, observeErr := binding.Observe(evidence, now)
				if observeErr != nil {
					return observeErr
				}
				if saveErr := uow.Bindings().Save(ctx, observed); saveErr != nil {
					return saveErr
				}
			}
		}

		s, sRev, getErr := uow.Sessions().Get(ctx, session.ID)
		if getErr != nil {
			return getErr
		}
		if s.State != run.SessionLaunching && s.State != run.SessionReconciling {
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
		return recordTransition(ctx, uow, EntitySession, session.ID.String(), string(sFrom), string(next.State), "launch claim settled execed", gen(handle.lease.Generation), now)
	})
	if err != nil {
		return fmt.Errorf("app: settle session launch claim: %w", err)
	}
	return nil
}

// recoverSessionBindingByLabel is recoverBindingByLabel's session-keyed
// sibling: it looks up ONE session's newest pending pane.open/launch.send
// operation — by the intent's own session_id, since several sessions can
// have concurrent pending pane-opens in feature mode — by its creation
// label and, when found, commits the runtime binding and the operation's
// success. Not finding the label is not an error: the create may still be
// in flight, and the operation is never re-sent.
func (c *Controller) recoverSessionBindingByLabel(ctx context.Context, handle RunHandle, session *run.Session, bindingFound bool, binding run.RuntimeBinding) error { //nolint:gocritic // hugeParam: RunHandle and RuntimeBinding are per-call values; called once per recovery round.
	if bindingFound && binding.PaneID != "" {
		return nil
	}
	var (
		op     Operation
		intent paneOpenIntent
		found  bool
	)
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		pending, pendErr := uow.Operations().Pending(ctx, handle.runID)
		if pendErr != nil {
			return pendErr
		}
		op, intent, found = newestPendingPaneOpenForSession(pending, session.ID)
		return nil
	}); err != nil {
		return err
	}
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
		// Creation evidence only: the server instance frozen into the
		// intent before the pane was created, never a fresh observation.
		newBinding := run.NewRuntimeBinding(intent.SessionID, intent.IncarnationID, "", intent.ServerInstance, ref.WorkspaceID, ref.TabID, ref.PaneID, intent.Label, run.LaunchInitial, now)
		if bindErr := uow.Bindings().Create(ctx, newBinding); bindErr != nil {
			return bindErr
		}
		latest.State = OperationSucceeded
		latest.ActEvidence = PaneHandle(ref)
		latest.UpdatedAt = now
		return uow.Operations().Save(ctx, latest)
	})
}

// newestPendingPaneOpenForSession is newestPendingPaneOpen's session-keyed
// sibling: several sessions can have concurrent pending pane-opens in
// feature mode, so recovery must select the one addressed to sessionID
// rather than the run's newest of any session.
func newestPendingPaneOpenForSession(ops []Operation, sessionID identity.SessionID) (Operation, paneOpenIntent, bool) {
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
		if !ok || decoded.Label == "" || decoded.SessionID != sessionID {
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

// driveSessionLaunchDeadline is driveLaunchDeadline's session-keyed
// sibling: once LaunchClaimDeadline has passed since ONE session's
// pane.open/launch.send operation was journaled with no claim row
// appearing, that operation goes reconciling with a pane snapshot as
// evidence — never re-created, never re-sent. Human interaction time after
// a claim exists is unbounded; this bounds only the mechanical launcher
// start.
func (c *Controller) driveSessionLaunchDeadline(ctx context.Context, handle RunHandle, session *run.Session, bindingFound bool, binding run.RuntimeBinding) (bool, error) { //nolint:gocritic // hugeParam: RunHandle and RuntimeBinding are per-call values; called once per corroboration round.
	var newest *Operation
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		for _, kind := range []OperationKind{OpPaneOpen, OpLaunchSend} {
			ops, opErr := uow.Operations().ByKind(ctx, handle.runID, kind)
			if opErr != nil {
				return opErr
			}
			for i := range ops {
				decoded, ok := decodeOperationPayload[paneOpenIntent](ops[i].Intent)
				if !ok || decoded.SessionID != session.ID {
					continue
				}
				if newest == nil || ops[i].CreatedAt.After(newest.CreatedAt) {
					op := ops[i]
					newest = &op
				}
			}
		}
		return nil
	}); err != nil {
		return false, err
	}
	if newest == nil || !LaunchDeadlineExpired(newest.CreatedAt, c.Clock.Now()) {
		return false, nil
	}
	if newest.State == OperationReconciling {
		return true, nil
	}

	snapshot := ""
	if bindingFound && binding.PaneID != "" {
		if content, readErr := c.Runtime.ReadPane(ctx, binding.PaneID, paneScrollbackLines); readErr == nil {
			snapshot = content
		}
	}
	now := c.Clock.Now()
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		op, getErr := uow.Operations().Get(ctx, newest.ID)
		if getErr != nil {
			return getErr
		}
		if op.State == OperationReconciling {
			return nil
		}
		op.State = OperationReconciling
		op.Outcome = "no launch claim appeared within the launch-claim deadline; reconciling, never re-sent"
		if snapshot != "" {
			op.ActEvidence = map[string]string{"pane_snapshot": snapshot}
		}
		op.UpdatedAt = now
		return uow.Operations().Save(ctx, op)
	})
	if err != nil {
		return false, fmt.Errorf("app: record session launch deadline: %w", err)
	}
	return true, nil
}
