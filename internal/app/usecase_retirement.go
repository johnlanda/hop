package app

import (
	"context"
	"fmt"
	"sort"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// RetirementReport is one RetireSettledSessions round's outcome.
type RetirementReport struct {
	// Retired lists sessions whose termination was OBSERVED this round —
	// the only event that frees a bounded-concurrency slot.
	Retired []string
	// Outstanding names retirement work dispatched or ambiguous: a close
	// dispatched but absence not yet observed, a mismatched occupant, a
	// failed inspection. Never treated as absence.
	Outstanding []string
	// RunFailed is true when a failed task's terminal-failure settlement
	// completed this round: owned work observed terminated, ref intents
	// quiesced, and the run marked failed.
	RunFailed bool
}

// retirementCandidate is one child session due for retirement and the
// boundary that retired it.
type retirementCandidate struct {
	Session  run.Session
	Boundary string
}

// RetireSettledSessions is the section 6 per-attempt session retirement
// pass: every implementer or reviewer session whose attempt has settled
// — an implement attempt's ACCEPTED result (acceptance verified the
// mailbox clear and closed it, so nothing is owed to the retired
// session), a review attempt's accepted verdict, or any terminal attempt
// outcome — is retired under the Phase 2 close-rule procedure verbatim,
// journaled with the boundary as the reason. The slot frees only on the
// close's observed-absence outcome, never on dispatch. When a task has
// failed, the pass also drives the run's terminal failure: every child
// session retired, unresolved ref intents fenced, and the run marked
// failed only once its owned work's termination is observed.
func (c *Controller) RetireSettledSessions(ctx context.Context, handle RunHandle) (RetirementReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per scheduling pass.
	frozen, err := c.Read.LoadFrozenRun(ctx, handle.runID)
	if err != nil {
		return RetirementReport{}, fmt.Errorf("app: load frozen run: %w", err)
	}
	if !frozen.Snapshot.Workflow.Feature() {
		return RetirementReport{}, nil
	}
	detail, err := c.Read.LoadRunStatus(ctx, handle.runID)
	if err != nil {
		return RetirementReport{}, fmt.Errorf("app: load run status: %w", err)
	}

	candidates, anyTaskFailed, err := c.retirementCandidates(ctx, handle)
	if err != nil {
		return RetirementReport{}, err
	}

	var report RetirementReport
	for i := range candidates {
		retired, outstanding, err := c.retireChildSession(ctx, handle, detail, &candidates[i])
		if err != nil {
			return report, err
		}
		switch {
		case retired:
			report.Retired = append(report.Retired, candidates[i].Session.ID.String())
		case outstanding != "":
			report.Outstanding = append(report.Outstanding, outstanding)
		}
	}

	if anyTaskFailed && len(report.Outstanding) == 0 {
		failed, outstanding, err := c.driveFeatureTerminalFailure(ctx, handle, &frozen)
		if err != nil {
			return report, err
		}
		report.RunFailed = failed
		if outstanding != "" {
			report.Outstanding = append(report.Outstanding, outstanding)
		}
	}
	return report, nil
}

// retirementCandidates reads the run's child sessions and decides which
// have reached a retirement boundary.
func (c *Controller) retirementCandidates(ctx context.Context, handle RunHandle) ([]retirementCandidate, bool, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	var (
		candidates    []retirementCandidate
		anyTaskFailed bool
	)
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "RetireSettledSessions")
		if wfErr != nil {
			return wfErr
		}
		tasks, taskErr := wf.TaskIndex().ByRun(ctx, handle.runID)
		if taskErr != nil {
			return taskErr
		}
		taskByID := map[identity.TaskID]run.Task{}
		for i := range tasks {
			taskByID[tasks[i].ID] = tasks[i]
			if tasks[i].State == run.TaskFailed {
				anyTaskFailed = true
			}
		}
		sessions, sessErr := wf.SessionIndex().ByRun(ctx, handle.runID)
		if sessErr != nil {
			return sessErr
		}
		sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
		for i := range sessions {
			s := sessions[i]
			if s.Role != run.RoleImplementer && s.Role != run.RoleReviewer {
				continue
			}
			if s.State == run.SessionTerminated || s.State == run.SessionLost {
				continue
			}
			if s.AttemptID == "" {
				continue
			}
			attempt, _, attErr := uow.Attempts().Get(ctx, s.AttemptID)
			if attErr != nil {
				return attErr
			}
			boundary := ""
			switch attempt.State {
			case run.AttemptFailed:
				boundary = "terminal attempt outcome: failed"
			case run.AttemptInterrupted:
				boundary = "terminal attempt outcome: interrupted"
			case run.AttemptCompleted:
				if taskByID[attempt.TaskID].Kind == run.TaskKindReview {
					boundary = "review attempt's accepted verdict"
				} else {
					boundary = "implement attempt's accepted result"
				}
			case run.AttemptSubmitted, run.AttemptChecking:
				result, resErr := uow.Results().Accepted(ctx, s.AttemptID)
				if resErr != nil {
					return resErr
				}
				if result != nil {
					boundary = "implement attempt's accepted result"
				}
			}
			if boundary == "" && anyTaskFailed {
				boundary = "run terminal failure"
			}
			if boundary == "" {
				continue
			}
			candidates = append(candidates, retirementCandidate{Session: s, Boundary: boundary})
		}
		return nil
	})
	return candidates, anyTaskFailed, err
}

// sessionCloseEvidence resolves one session's recorded close-target
// evidence: its current binding, the claim keyed to that binding's
// incarnation, and the durable argv markers.
func (c *Controller) sessionCloseEvidence(ctx context.Context, handle RunHandle, session *run.Session) (binding run.RuntimeBinding, bindingFound bool, claim LaunchClaim, claimFound bool, markers []string, err error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	err = c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		var getErr error
		binding, bindingFound, getErr = uow.Bindings().Current(ctx, session.ID)
		if getErr != nil {
			return getErr
		}
		if bindingFound {
			claim, claimFound, getErr = uow.LaunchClaims().Get(ctx, binding.IncarnationID)
			if getErr != nil {
				return getErr
			}
		}
		return nil
	})
	if err != nil {
		return binding, bindingFound, claim, claimFound, nil, err
	}
	markers = []string{handle.runID.String()}
	if session.AttemptID != "" {
		markers = append(markers, session.AttemptID.String())
	}
	if bindingFound {
		markers = append(markers, binding.IncarnationID.String())
	}
	if session.NativeSessionRef != "" {
		markers = append(markers, session.NativeSessionRef)
	}
	if bindingFound && binding.Occupant != nil && binding.Occupant.ArgvMarker != "" {
		markers = append(markers, binding.Occupant.ArgvMarker)
	}
	return binding, bindingFound, claim, claimFound, markers, nil
}

// retireChildSession retires one settled child session under the shared
// close-rule procedure. retired is true only once termination has been
// observed and the session row is terminal.
func (c *Controller) retireChildSession(ctx context.Context, handle RunHandle, detail RunDetail, candidate *retirementCandidate) (bool, string, error) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; called once per candidate per round.
	session := candidate.Session
	reason := "per-attempt retirement: " + candidate.Boundary

	if session.State == run.SessionReserved {
		// A reserved session never launched: there is no process to
		// retire.
		return true, "", c.terminateRetiredSession(ctx, handle, session.ID, reason)
	}

	binding, bindingFound, claim, claimFound, markers, err := c.sessionCloseEvidence(ctx, handle, &session)
	if err != nil {
		return false, "", err
	}
	if claimFound && claim.State == LaunchClaimExecFailed {
		// The exec failed and the launcher exited; nothing is live for
		// this incarnation.
		return true, "", c.terminateRetiredSession(ctx, handle, session.ID, reason+" (exec failed; no process)")
	}
	if !bindingFound || binding.PaneID == "" {
		return false, fmt.Sprintf("session %s has no recorded placement to retire against; failing closed", session.ID), nil
	}
	if !claimFound {
		// No recorded occupant identity to close against: only observed
		// absence clears it.
		_, absent, ambiguous := c.observePaneAbsence(ctx, binding.PaneID, binding.CreationLabel)
		if ambiguous == "" && absent {
			return true, "", c.terminateRetiredSession(ctx, handle, session.ID, reason+" (pane observed absent)")
		}
		if ambiguous != "" {
			return false, fmt.Sprintf("session %s has no launch claim to retire against; %s", session.ID, ambiguous), nil
		}
		return false, fmt.Sprintf("session %s has no launch claim to retire against; failing closed", session.ID), nil
	}

	target := paneCloseTarget{
		PaneID:        binding.PaneID,
		Label:         binding.CreationLabel,
		SessionID:     session.ID,
		IncarnationID: binding.IncarnationID,
		PID:           claim.PID,
		Markers:       markers,
		Reason:        reason,
	}
	retired, outstanding, err := c.closePaneOperation(ctx, handle, detail, &target)
	if err != nil {
		return false, "", err
	}
	if !retired {
		return false, fmt.Sprintf("session %s: %s", session.ID, outstanding), nil
	}
	return true, "", c.terminateRetiredSession(ctx, handle, session.ID, reason)
}

// terminateRetiredSession moves a retired session's row to terminated
// (through stopping where the table requires it), recording the
// transition with the retirement boundary as the reason. Idempotent for
// an already-terminated session.
func (c *Controller) terminateRetiredSession(ctx context.Context, handle RunHandle, sessionID identity.SessionID, reason string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return terminateSession(ctx, uow, sessionID, reason, gen(handle.lease.Generation), now)
	})
}

// driveFeatureTerminalFailure finishes a feature run's terminal failure:
// a failed implement task can never satisfy readiness again, so once
// every child session is terminal, no check or merge execution is
// unresolved and every ref-move intent is retired (quiescence is
// REQUIRED before any terminal report), the manager is retired under the
// close rule and the run marked failed — with stop precedence: a held
// stop leaves the terminal state to stop handling.
func (c *Controller) driveFeatureTerminalFailure(ctx context.Context, handle RunHandle, frozen *FrozenRun) (bool, string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	detail, err := c.Read.LoadRunStatus(ctx, handle.runID)
	if err != nil {
		return false, "", err
	}
	if detail.StopRequested {
		return false, "", nil // stop precedence: stop handling owns the terminal state.
	}
	if detail.State != run.RunRunning && detail.State != run.RunResuming && detail.State != run.RunCompleting {
		return false, "", nil
	}
	for i := range detail.PendingOperations {
		switch detail.PendingOperations[i].Kind {
		case OpCheckRun, OpIntegrationMerge:
			return false, fmt.Sprintf("terminal failure blocked: execution %s is unresolved", detail.PendingOperations[i].ID), nil
		}
	}
	outstanding, err := c.retireUnresolvedRefIntents(ctx, handle, frozen)
	if err != nil {
		return false, "", err
	}
	if outstanding != "" {
		return false, "terminal failure blocked: " + outstanding, nil
	}

	childrenTerminal, manager, managerFound, err := c.featureSessionsTerminal(ctx, handle)
	if err != nil {
		return false, "", err
	}
	if !childrenTerminal {
		return false, "terminal failure blocked: a child session is not yet observed terminated", nil
	}
	if managerFound && manager.State != run.SessionTerminated && manager.State != run.SessionLost {
		retired, still, retErr := c.retireChildSession(ctx, handle, detail, &retirementCandidate{Session: manager, Boundary: "run terminal failure"})
		if retErr != nil {
			return false, "", retErr
		}
		if !retired {
			return false, "terminal failure blocked: " + still, nil
		}
	}

	now := c.Clock.Now()
	err = c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		r, rRev, getErr := uow.Runs().Get(ctx, handle.runID)
		if getErr != nil {
			return getErr
		}
		if r.StopRequested {
			return nil
		}
		if r.State != run.RunRunning && r.State != run.RunResuming && r.State != run.RunCompleting {
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
		return recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(rFrom), string(next.State), "task failure with owned-work termination observed", gen(handle.lease.Generation), now)
	})
	if err != nil {
		return false, "", err
	}
	return true, "", nil
}

// featureSessionsTerminal reports whether every child session is
// terminal, and returns the run's manager session row.
func (c *Controller) featureSessionsTerminal(ctx context.Context, handle RunHandle) (bool, run.Session, bool, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	var (
		childrenTerminal = true
		manager          run.Session
		managerFound     bool
	)
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "feature terminal failure")
		if wfErr != nil {
			return wfErr
		}
		sessions, sessErr := wf.SessionIndex().ByRun(ctx, handle.runID)
		if sessErr != nil {
			return sessErr
		}
		for i := range sessions {
			s := sessions[i]
			if s.Role == run.RoleManager {
				manager = s
				managerFound = true
				continue
			}
			if s.State != run.SessionTerminated && s.State != run.SessionLost {
				childrenTerminal = false
			}
		}
		return nil
	})
	return childrenTerminal, manager, managerFound, err
}
