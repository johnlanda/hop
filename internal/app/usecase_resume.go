package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// ResumeRequest is hop resume's input. HOPPath and StateRoot mirror
// StartRunRequest's: the absolute hop executable path and state root
// cmd/hop's single resolver produced, needed only if reconciliation
// authorizes a cold relaunch.
type ResumeRequest struct {
	RunID         string
	ControllerID  string
	ConfirmAbsent bool
	HOPPath       string
	StateRoot     string
}

// ResumeOutcome is one reconciliation round's disposition (section 5).
type ResumeOutcome string

// Resume outcomes.
const (
	// ResumeWarmReattached means the current binding's occupant matched the
	// settled claim or recorded evidence (case 1): the run continues.
	ResumeWarmReattached ResumeOutcome = "warm-reattached"
	// ResumeColdRelaunched means absence was conclusively established (case
	// 3) and a new session/incarnation was launched.
	ResumeColdRelaunched ResumeOutcome = "cold-relaunched"
	// ResumeFailedClosed means a present occupant failed to match with no
	// positive retirement evidence (case 2, no evidence branch): a human
	// must inspect and act.
	ResumeFailedClosed ResumeOutcome = "failed-closed"
	// ResumeReconciling means neither warm reattach nor cold relaunch could
	// be established this round; the run stays resuming/reconciling.
	ResumeReconciling ResumeOutcome = "reconciling"
	// ResumeUnsupported means cold relaunch would be required but the
	// harness has no supported resume semantics (Codex, opencode).
	ResumeUnsupported ResumeOutcome = "unsupported"
	// ResumeNothingToDo means the attempt was never launched, or is already
	// terminal; resume only needed to acquire the lease.
	ResumeNothingToDo ResumeOutcome = "nothing-to-do"
)

// ResumeResult is one Resume call's outcome.
type ResumeResult struct {
	Outcome        ResumeOutcome
	ObservedPaneID string // set on ResumeFailedClosed: the pane a human should inspect
	Detail         string
}

// Resume acquires run's controller lease (a new fencing generation) and
// performs one round of the section 5 reconciliation. It never sleeps or
// resends; the caller is responsible for driving the returned RunHandle
// further (CorroborateLaunch, DriveStop, ClaimAndRunCheck) once
// reconciliation lands on a continuable outcome.
func (c *Controller) Resume(ctx context.Context, req ResumeRequest) (ResumeResult, RunHandle, error) {
	runID, err := identity.ParseRunID(req.RunID)
	if err != nil {
		return ResumeResult{}, RunHandle{}, fmt.Errorf("app: parse run id: %w", err)
	}
	lease, err := c.Store.AcquireLease(ctx, runID, req.ControllerID)
	if err != nil {
		return ResumeResult{}, RunHandle{}, fmt.Errorf("app: acquire lease: %w", err)
	}
	handle := newRunHandle(runID, lease)
	now := c.Clock.Now()

	if enterErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		r, rRev, getErr := uow.Runs().Get(ctx, runID)
		if getErr != nil {
			return getErr
		}
		rFrom := r.State
		r, resumeErr := r.EnterResuming(now)
		if resumeErr != nil {
			return resumeErr
		}
		if _, saveErr := uow.Runs().Save(ctx, r, rRev); saveErr != nil {
			return saveErr
		}
		return recordTransition(ctx, uow, EntityRun, runID.String(), string(rFrom), string(r.State), "hop resume acquired the lease", gen(handle.lease.Generation), now)
	}); enterErr != nil {
		return ResumeResult{}, handle, fmt.Errorf("app: enter resuming: %w", enterErr)
	}

	detail, err := c.Read.LoadRunStatus(ctx, runID)
	if err != nil {
		return ResumeResult{}, handle, fmt.Errorf("app: load run status: %w", err)
	}

	result, err := c.reconcile(ctx, handle, detail, req)
	return result, handle, err
}

// reconcile dispatches by the attempt's state, per the section 5 tables.
func (c *Controller) reconcile(ctx context.Context, handle RunHandle, detail RunDetail, req ResumeRequest) (ResumeResult, error) { //nolint:gocritic // hugeParam: RunHandle, RunDetail and ResumeRequest are per-call DTOs; this runs once per resume round.
	switch detail.AttemptState {
	case run.AttemptReserved:
		return ResumeResult{Outcome: ResumeNothingToDo, Detail: "attempt was never launched"}, nil
	case run.AttemptRunning, run.AttemptSubmitted, run.AttemptChecking, run.AttemptReconciling, run.AttemptLaunching, run.AttemptRelaunching:
		return c.reconcileActive(ctx, handle, detail, req)
	default:
		return ResumeResult{Outcome: ResumeNothingToDo, Detail: "attempt is already terminal"}, nil
	}
}

// reconcileActive handles every attempt state a live worker could still
// occupy a pane for. A launching/relaunching attempt corroborates through
// the ordinary fenced settlement path first — its state is never hidden
// from CorroborateLaunch — and only an ambiguous round moves it to
// reconciling. The reconciling decision is then: warm adoption of a
// settled claim under the one corroboration predicate, positive-evidence
// retirement plus cold relaunch, fail-closed on an unidentified occupant,
// or — with conclusively established absence — cold relaunch.
func (c *Controller) reconcileActive(ctx context.Context, handle RunHandle, detail RunDetail, req ResumeRequest) (ResumeResult, error) { //nolint:gocritic // hugeParam: RunHandle, RunDetail and ResumeRequest are per-call DTOs; this runs once per resume round.
	launchingState := detail.AttemptState == run.AttemptLaunching || detail.AttemptState == run.AttemptRelaunching
	execFailed := detail.Claim != nil && detail.Claim.State == LaunchClaimExecFailed
	if launchingState && !execFailed {
		progress, err := c.CorroborateLaunch(ctx, handle)
		if err != nil {
			return ResumeResult{}, err
		}
		switch progress {
		case LaunchSettled, LaunchAlreadySettled:
			return ResumeResult{Outcome: ResumeWarmReattached, Detail: "launch claim corroborated on resume"}, nil
		case LaunchNeedsInteraction:
			paneID := ""
			if detail.Binding != nil {
				paneID = detail.Binding.PaneID
			}
			return ResumeResult{
				Outcome: ResumeFailedClosed, ObservedPaneID: paneID,
				Detail: "the occupant matches the claim's executable identity and marker but not its pid: the unsupported forking-wrapper topology; inspect the pane, then close it or hop stop the run",
			}, nil
		case LaunchFailed, LaunchPending:
			// LaunchPending: still ambiguous — enter reconciling below.
		}
	}

	now := c.Clock.Now()
	priorAttemptState := detail.AttemptState
	if detail.AttemptState != run.AttemptReconciling {
		if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
			return enterReconciling(ctx, uow, detail, gen(handle.lease.Generation), now)
		}); err != nil {
			return ResumeResult{}, fmt.Errorf("app: enter reconciling: %w", err)
		}
	}

	if detail.Binding == nil || detail.Binding.PaneID == "" {
		return ResumeResult{Outcome: ResumeReconciling, Detail: "no runtime binding recorded yet"}, nil
	}
	pane, occupantAbsent, observed := c.observePane(ctx, detail.Binding)
	if !observed {
		// Inspection failed with no positive absence evidence: ambiguous,
		// never absence (docs/plan/phase-2-design.md section 5).
		return ResumeResult{Outcome: ResumeReconciling, Detail: "pane inspection failed; absence is never assumed from an inspection error"}, nil
	}

	if !occupantAbsent {
		// Warm adoption goes through the one corroboration predicate,
		// against a settled claim only; an exec_pending claim was already
		// routed through the ordinary settlement path above.
		if detail.Claim != nil && detail.Claim.State == LaunchClaimExeced {
			markers, markerErr := c.launchMarkers(ctx, handle, detail)
			if markerErr != nil {
				return ResumeResult{}, markerErr
			}
			paneMatches := detail.Binding.IncarnationID == detail.Claim.IncarnationID && !detail.Binding.Superseded
			if CorroborateSettlement(paneMatches, pane, markers, *detail.Claim) == SettlementSettled {
				return c.warmReattach(ctx, handle, detail, priorAttemptState, pane, FirstMarkerMatch(pane, markers))
			}
		}

		// A different, present occupant. Positive evidence (the run's
		// pre-assigned native session reference in its argv) authorizes
		// guarded retirement and cold relaunch; anything else fails closed.
		nativeRef, nativeErr := c.sessionNativeRef(ctx, handle, detail.SessionID)
		if nativeErr == nil && nativeRef != "" && paneCarriesMarker(pane, nativeRef) {
			return c.retireAndRelaunch(ctx, handle, detail, req, pane, nativeRef)
		}
		return ResumeResult{
			Outcome: ResumeFailedClosed, ObservedPaneID: detail.Binding.PaneID,
			Detail: "a present occupant does not match this run's claim identity and carries no positive evidence tying it to this run; inspect the pane, then close it or hop stop the run, and rerun hop resume, or attest absence with --confirm-absent",
		}, nil
	}

	if execFailed || req.ConfirmAbsent {
		return c.coldRelaunch(ctx, handle, detail, req)
	}
	return ResumeResult{Outcome: ResumeReconciling, Detail: "no live process observed; rerun with --confirm-absent once no worker for this run is running anywhere"}, nil
}

// observePane inspects a binding's pane and classifies the observation.
// absent is true only on positive evidence: the pane exists with no
// foreground occupant, or no pane carries the binding's creation label.
// observed is false when inspection failed and no positive absence could
// be established — ambiguous, never absence.
func (c *Controller) observePane(ctx context.Context, binding *run.RuntimeBinding) (pane PaneProcess, absent, observed bool) {
	inspected, err := c.Runtime.InspectPane(ctx, binding.PaneID)
	if err == nil {
		return inspected, len(inspected.Foreground) == 0, true
	}
	if binding.CreationLabel != "" {
		if _, found, findErr := c.Runtime.FindPaneByLabel(ctx, binding.CreationLabel); findErr == nil && !found {
			return PaneProcess{}, true, true
		}
	}
	return PaneProcess{}, false, false
}

// warmReattach adopts a verified occupant: the attempt returns to its
// prior state (derived from durable evidence when the attempt entered this
// round already reconciling), the session is confirmed active, and the
// binding's occupant evidence is refreshed with the marker that actually
// corroborated — never a synthesized one.
func (c *Controller) warmReattach(ctx context.Context, handle RunHandle, detail RunDetail, priorState run.AttemptState, pane PaneProcess, marker string) (ResumeResult, error) { //nolint:gocritic // hugeParam: RunHandle, RunDetail and PaneProcess are per-call values; this runs once per resume round.
	target := priorState
	if target == run.AttemptReconciling {
		derived, deriveErr := c.reattachTarget(ctx, handle, detail)
		if deriveErr != nil {
			return ResumeResult{}, deriveErr
		}
		target = derived
	}

	now := c.Clock.Now()
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		a, aRev, getErr := uow.Attempts().Get(ctx, detail.AttemptID)
		if getErr != nil {
			return getErr
		}
		nextAttempt, reattachErr := a.Reattach(target, now)
		if reattachErr != nil {
			return reattachErr
		}
		if _, saveErr := uow.Attempts().Save(ctx, nextAttempt, aRev); saveErr != nil {
			return saveErr
		}

		s, sRev, getErr := uow.Sessions().Get(ctx, detail.SessionID)
		if getErr != nil {
			return getErr
		}
		sFrom := s.State
		nextSession, confirmErr := s.ConfirmActive(now)
		if confirmErr != nil {
			return confirmErr
		}
		if _, saveErr := uow.Sessions().Save(ctx, nextSession, sRev); saveErr != nil {
			return saveErr
		}

		binding, found, getErr := uow.Bindings().Current(ctx, detail.SessionID)
		if getErr != nil {
			return getErr
		}
		if found && marker != "" {
			evidence := run.OccupantEvidence{Label: binding.CreationLabel, ArgvMarker: marker, PID: firstForeground(pane).PID}
			nextBinding, observeErr := binding.Observe(evidence, now)
			if observeErr != nil {
				return observeErr
			}
			if saveErr := uow.Bindings().Save(ctx, nextBinding); saveErr != nil {
				return saveErr
			}
		}

		generation := gen(handle.lease.Generation)
		if transErr := recordTransition(ctx, uow, EntityAttempt, detail.AttemptID.String(), string(run.AttemptReconciling), string(nextAttempt.State), "warm reattach verified", generation, now); transErr != nil {
			return transErr
		}
		return recordTransition(ctx, uow, EntitySession, detail.SessionID.String(), string(sFrom), string(nextSession.State), "warm reattach verified", generation, now)
	})
	if err != nil {
		return ResumeResult{}, fmt.Errorf("app: warm reattach: %w", err)
	}
	return ResumeResult{Outcome: ResumeWarmReattached}, nil
}

// reattachTarget derives, from durable evidence, the state a reconciling
// attempt returns to on verified warm reattach: an accepted result whose
// check request is claimed means checking, an accepted result otherwise
// means submitted, and no accepted result means running.
func (c *Controller) reattachTarget(ctx context.Context, handle RunHandle, detail RunDetail) (run.AttemptState, error) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call values; called once per warm reattach.
	target := run.AttemptRunning
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		result, getErr := uow.Results().Accepted(ctx, detail.AttemptID)
		if getErr != nil {
			return getErr
		}
		if result == nil {
			return nil
		}
		target = run.AttemptSubmitted
		request, getErr := uow.CheckRequests().Get(ctx, result.ID)
		if getErr != nil {
			if errors.Is(getErr, ErrNotFound) {
				return nil
			}
			return getErr
		}
		if request.State == CheckRequestClaimed {
			target = run.AttemptChecking
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("app: derive reattach target: %w", err)
	}
	return target, nil
}

// retireAndRelaunch records the positively identified restored occupant as
// an observed-restoration binding under a freshly minted observation
// incarnation — the store's UNIQUE(session_id, incarnation_id) binding key
// is never reused — supersedes the launch binding with that evidence,
// retires the occupant through the shared pane.close operation procedure,
// and proceeds to cold relaunch only once its termination was observed.
func (c *Controller) retireAndRelaunch(ctx context.Context, handle RunHandle, detail RunDetail, req ResumeRequest, pane PaneProcess, nativeRef string) (ResumeResult, error) { //nolint:gocritic // hugeParam: RunHandle, RunDetail, ResumeRequest and PaneProcess are per-call values; this runs once per resume round.
	now := c.Clock.Now()
	evidence := fmt.Sprintf("observed process argv carries native session reference %s", nativeRef)
	observationIncarnation, err := identity.ParseIncarnationID(c.IDs.NewID())
	if err != nil {
		return ResumeResult{}, fmt.Errorf("app: generate observation incarnation id: %w", err)
	}

	// The current binding may already be the recorded observation from a
	// previous round; only a launch binding is superseded and re-recorded.
	var target paneCloseTarget
	uowErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		binding, found, getErr := uow.Bindings().Current(ctx, detail.SessionID)
		if getErr != nil {
			return getErr
		}
		if !found {
			return fmt.Errorf("app: no current binding to supersede for session %s", detail.SessionID)
		}
		target = paneCloseTarget{
			PaneID: binding.PaneID, Label: binding.CreationLabel,
			SessionID: detail.SessionID, IncarnationID: binding.IncarnationID,
			PID: firstForeground(pane).PID, Markers: []string{nativeRef},
			Reason: "positive-evidence retirement",
		}
		if binding.LaunchKind == run.LaunchRestoredObserved {
			return nil
		}
		nextBinding, supersedeErr := binding.Supersede(evidence, now)
		if supersedeErr != nil {
			return supersedeErr
		}
		if saveErr := uow.Bindings().Save(ctx, nextBinding); saveErr != nil {
			return saveErr
		}
		observed := run.NewRuntimeBinding(detail.SessionID, observationIncarnation, binding.ServerSocketPath, binding.WorkspaceID, binding.TabID, binding.PaneID, binding.CreationLabel, run.LaunchRestoredObserved, now)
		observedEvidence := run.OccupantEvidence{Label: binding.CreationLabel, ArgvMarker: nativeRef, PID: firstForeground(pane).PID}
		observed, observeErr := observed.Observe(observedEvidence, now)
		if observeErr != nil {
			return observeErr
		}
		target.IncarnationID = observationIncarnation
		return uow.Bindings().Create(ctx, observed)
	})
	if uowErr != nil {
		return ResumeResult{}, fmt.Errorf("app: record restored-observed binding: %w", uowErr)
	}

	retired, outstanding, closeErr := c.closePaneOperation(ctx, handle, detail, &target)
	if closeErr != nil {
		return ResumeResult{}, closeErr
	}
	if !retired {
		return ResumeResult{Outcome: ResumeReconciling, Detail: "positive-evidence occupant recorded; " + outstanding}, nil
	}
	return c.coldRelaunch(ctx, handle, detail, req)
}

// coldRelaunch authorizes a cold relaunch: a new session and incarnation
// bound to the same attempt, a fresh pane in the same worktree. Only
// Claude's resume semantics are supported in Phase 2.
func (c *Controller) coldRelaunch(ctx context.Context, handle RunHandle, detail RunDetail, req ResumeRequest) (ResumeResult, error) { //nolint:gocritic // hugeParam: RunHandle, RunDetail and ResumeRequest are per-call DTOs; this runs once per resume round.
	harness, err := c.sessionHarness(ctx, handle, detail.SessionID)
	if err != nil {
		return ResumeResult{}, err
	}
	if harness != run.HarnessClaude {
		return ResumeResult{Outcome: ResumeUnsupported, Detail: fmt.Sprintf("cold resume is not supported for harness %q in Phase 2", harness)}, nil
	}

	sessionID, err := identity.ParseSessionID(c.IDs.NewID())
	if err != nil {
		return ResumeResult{}, fmt.Errorf("app: generate session id: %w", err)
	}
	incarnationID, err := identity.ParseIncarnationID(c.IDs.NewID())
	if err != nil {
		return ResumeResult{}, fmt.Errorf("app: generate incarnation id: %w", err)
	}
	now := c.Clock.Now()

	err = c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		a, aRev, getErr := uow.Attempts().Get(ctx, detail.AttemptID)
		if getErr != nil {
			return getErr
		}
		nextAttempt, relaunchErr := a.Relaunch(now)
		if relaunchErr != nil {
			return relaunchErr
		}
		if _, saveErr := uow.Attempts().Save(ctx, nextAttempt, aRev); saveErr != nil {
			return saveErr
		}

		r, rRev, getErr := uow.Runs().Get(ctx, handle.runID)
		if getErr != nil {
			return getErr
		}
		rFrom := r.State
		nextRun, launchErr := r.Launch(now)
		if launchErr != nil {
			return launchErr
		}
		if _, saveErr := uow.Runs().Save(ctx, nextRun, rRev); saveErr != nil {
			return saveErr
		}

		if retireErr := retirePendingLaunchIntents(ctx, uow, handle.runID, detail.SessionID, now); retireErr != nil {
			return retireErr
		}

		// The old session is never revived: a cold relaunch binds a new
		// session and incarnation to the same attempt (section 5).
		oldSession, oldRev, getErr := uow.Sessions().Get(ctx, detail.SessionID)
		if getErr != nil {
			return getErr
		}
		oldFrom := oldSession.State
		oldSession, lostErr := oldSession.MarkLost(now)
		if lostErr != nil {
			return lostErr
		}
		if _, saveErr := uow.Sessions().Save(ctx, oldSession, oldRev); saveErr != nil {
			return saveErr
		}
		if transErr := recordTransition(ctx, uow, EntitySession, detail.SessionID.String(), string(oldFrom), string(oldSession.State), "cold relaunch authorized", gen(handle.lease.Generation), now); transErr != nil {
			return transErr
		}

		newSession := run.NewSession(sessionID, handle.runID, detail.AttemptID, harness, now)
		newSession, launchSessErr := newSession.Launch(now)
		if launchSessErr != nil {
			return launchSessErr
		}
		if _, createErr := uow.Sessions().Create(ctx, newSession); createErr != nil {
			return createErr
		}

		generation := gen(handle.lease.Generation)
		if transErr := recordTransition(ctx, uow, EntityAttempt, detail.AttemptID.String(), string(run.AttemptReconciling), string(nextAttempt.State), "cold relaunch authorized", generation, now); transErr != nil {
			return transErr
		}
		if transErr := recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(rFrom), string(nextRun.State), "cold relaunch authorized", generation, now); transErr != nil {
			return transErr
		}
		return recordTransition(ctx, uow, EntitySession, sessionID.String(), string(run.SessionReserved), string(newSession.State), "cold relaunch authorized", generation, now)
	})
	if err != nil {
		return ResumeResult{}, fmt.Errorf("app: authorize cold relaunch: %w", err)
	}

	worktree, err := c.currentWorktree(ctx, handle)
	if err != nil {
		return ResumeResult{}, err
	}
	if req.HOPPath == "" || req.StateRoot == "" {
		return ResumeResult{}, fmt.Errorf("app: resume requires HOPPath and StateRoot to open the relaunch pane")
	}
	ids := generatedIdentities{Run: handle.runID, Task: detail.TaskID, Attempt: detail.AttemptID, Session: sessionID, Incarnation: incarnationID}
	if err := c.openRelaunchPane(ctx, handle, ids, worktree, req.HOPPath, req.StateRoot); err != nil {
		return ResumeResult{}, err
	}
	return ResumeResult{Outcome: ResumeColdRelaunched}, nil
}

// currentWorktree loads the run's single Phase 2 worktree as WorktreeInfo.
func (c *Controller) currentWorktree(ctx context.Context, handle RunHandle) (WorktreeInfo, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per relaunch.
	var info WorktreeInfo
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		w, _, getErr := uow.Worktrees().ByRun(ctx, handle.runID)
		if getErr != nil {
			return getErr
		}
		info = WorktreeInfo{Path: w.Path, Branch: w.Branch}
		return nil
	})
	return info, err
}

// sessionHarness reads a session's harness.
func (c *Controller) sessionHarness(ctx context.Context, handle RunHandle, sessionID identity.SessionID) (run.Harness, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per relaunch decision.
	var harness run.Harness
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		s, _, getErr := uow.Sessions().Get(ctx, sessionID)
		if getErr != nil {
			return getErr
		}
		harness = s.Harness
		return nil
	})
	return harness, err
}

// sessionNativeRef reads a session's native reference, "" when unassigned.
func (c *Controller) sessionNativeRef(ctx context.Context, handle RunHandle, sessionID identity.SessionID) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per reconciliation round.
	var ref string
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		s, _, getErr := uow.Sessions().Get(ctx, sessionID)
		if getErr != nil {
			return getErr
		}
		ref = s.NativeSessionRef
		return nil
	})
	return ref, err
}

// enterReconciling moves Attempt and, when it exists, the attempt's current
// Session into reconciling, recording both transitions.
// retirePendingLaunchIntents marks every still-pending pane.open operation
// whose intent named previousSession as superseded, in the same
// transaction that creates a cold relaunch's replacement session — before
// that new session's own intent is committed — so a retired incarnation's
// launcher can never claim against a stale intent (SubmissionStore's
// pre-binding fallback matches ClaimLaunch against the newest pending
// pane.open intent for the attempt; an old one left pending would still be
// "newest" until this runs).
func retirePendingLaunchIntents(ctx context.Context, uow UnitOfWork, runID identity.RunID, previousSession identity.SessionID, now time.Time) error {
	pending, err := uow.Operations().Pending(ctx, runID)
	if err != nil {
		return err
	}
	for _, op := range pending { //nolint:gocritic // rangeValCopy: Operation is a small per-run journal row; copying it to classify and possibly resave is clearer than indexing.
		if op.Kind != OpPaneOpen && op.Kind != OpLaunchSend {
			continue
		}
		intent, ok := decodeOperationPayload[paneOpenIntent](op.Intent)
		if !ok || intent.SessionID != previousSession {
			continue
		}
		op.State = OperationReconciling
		op.Outcome = "superseded: retired for a cold relaunch"
		op.UpdatedAt = now
		if saveErr := uow.Operations().Save(ctx, op); saveErr != nil {
			return saveErr
		}
	}
	return nil
}

func enterReconciling(ctx context.Context, uow UnitOfWork, detail RunDetail, generation *int64, now time.Time) error { //nolint:gocritic // hugeParam: RunDetail is a per-call DTO; this runs once per resume round.
	a, aRev, err := uow.Attempts().Get(ctx, detail.AttemptID)
	if err != nil {
		return err
	}
	aFrom := a.State
	a, err = a.Reconcile(now)
	if err != nil {
		return err
	}
	if _, saveErr := uow.Attempts().Save(ctx, a, aRev); saveErr != nil {
		return saveErr
	}
	if transErr := recordTransition(ctx, uow, EntityAttempt, detail.AttemptID.String(), string(aFrom), string(a.State), "takeover", generation, now); transErr != nil {
		return transErr
	}

	if detail.SessionID == "" {
		return nil
	}
	s, sRev, err := uow.Sessions().Get(ctx, detail.SessionID)
	if err != nil {
		return err
	}
	if s.State != run.SessionLaunching && s.State != run.SessionActive {
		return nil
	}
	sFrom := s.State
	s, err = s.Reconcile(now)
	if err != nil {
		return err
	}
	if _, err := uow.Sessions().Save(ctx, s, sRev); err != nil {
		return err
	}
	return recordTransition(ctx, uow, EntitySession, detail.SessionID.String(), string(sFrom), string(s.State), "takeover", generation, now)
}

// paneCarriesMarker reports whether pane's foreground process argv or
// cmdline carries marker.
func paneCarriesMarker(pane PaneProcess, marker string) bool {
	return FirstMarkerMatch(pane, []string{marker}) != ""
}
