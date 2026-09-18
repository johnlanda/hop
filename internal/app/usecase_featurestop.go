package app

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// errRunNotQuiesced marks a stopped/failed commit refused because owned
// work surfaced between the caller's outstanding checks and the terminal
// transaction — the round reports outstanding work instead of a terminal
// state.
var errRunNotQuiesced = errors.New("run has unquiesced owned work")

// assertRunQuiescedLocked re-validates, INSIDE the terminal transaction,
// that no execution-bearing operation — nor an unresolved worktree.create,
// whose checkout would otherwise be abandoned without a row — is pending
// and no integration occupies the serial slot. The caller's own outstanding checks read
// state that may have moved (a losing fence CAS journals reconciling and
// returns unsettled; a racing zombie publish lands between rounds), so
// the terminal commit re-derives quiescence from the transaction's own
// repositories rather than trusting the earlier reads.
func assertRunQuiescedLocked(ctx context.Context, uow UnitOfWork, wf WorkflowRepositories, runID identity.RunID) error {
	ops, err := uow.Operations().Pending(ctx, runID)
	if err != nil {
		return err
	}
	for i := range ops {
		switch ops[i].Kind {
		case OpCheckRun, OpIntegrationMerge, OpIntegrationPublish, OpIntegrationReset, OpIntegrationFence, OpIntegrationInit, OpWorktreeCreate:
			return fmt.Errorf("%w: operation %s (%s) is pending", errRunNotQuiesced, ops[i].ID, ops[i].Kind)
		}
	}
	if _, _, occupied, curErr := wf.Integrations().Current(ctx, runID); curErr != nil {
		return curErr
	} else if occupied {
		return fmt.Errorf("%w: an integration occupies the serial slot", errRunNotQuiesced)
	}
	return nil
}

// DriveFeatureStop performs one round of stop interruption for a
// feature-mode run: it retires every check and merge execution's process
// group under the group-retirement rule, resolves every unresolved
// per-attempt worktree.create (adopted and recorded, or settled failed
// after its bounded wait), drives the reset for a
// published-but-unsettled candidate so the integration ref never rests
// on an unvalidated candidate in a stopped run, retires every unresolved
// ref-move intent under the ref-fencing rule (quiescence is REQUIRED
// before the terminal report), closes every owned session — manager,
// implementers, reviewer — under the shared close-rule procedure, and
// marks the run stopped ONLY once every piece of owned work has been
// observed absent. Safe to call repeatedly; it never resends anything.
func (c *Controller) DriveFeatureStop(ctx context.Context, handle RunHandle) (StopReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; stop polling calls this method, never a hot inner loop.
	frozen, err := c.Read.LoadFrozenRun(ctx, handle.runID)
	if err != nil {
		return StopReport{}, fmt.Errorf("app: load frozen run: %w", err)
	}
	if !frozen.Snapshot.Workflow.Feature() {
		return c.DriveStop(ctx, handle)
	}
	detail, err := c.Read.LoadRunStatus(ctx, handle.runID)
	if err != nil {
		return StopReport{}, fmt.Errorf("app: load run status: %w", err)
	}
	if detail.State == run.RunStopped {
		return StopReport{RunState: string(detail.State), Terminated: true}, nil
	}
	if detail.State != run.RunStopping {
		return StopReport{RunState: string(detail.State)}, nil
	}

	var outstanding []string
	note := func(still string) {
		if still != "" {
			outstanding = append(outstanding, still)
		}
	}

	// A stop that began before a Herdr restart, or spanned one, can never
	// conclude a session's absence on its own: the placement's lifetime is
	// gone and no recorded occupant can be matched again. The
	// restart-lifecycle step closes what HOP owns first — retire-only here,
	// since a stopping run never relaunches — so the retirement below
	// decides against panes that are provably gone. No launch parameters
	// are supplied for that reason.
	restart, err := c.ReconcileServerRestart(ctx, handle, RestartOptions{})
	if err != nil {
		return StopReport{RunState: string(run.RunStopping)}, err
	}
	for _, still := range restart.Outstanding {
		note(still)
	}
	if len(restart.Closed) > 0 {
		// The closes changed what the run owns; re-read before retiring.
		if detail, err = c.Read.LoadRunStatus(ctx, handle.runID); err != nil {
			return StopReport{RunState: string(run.RunStopping)}, fmt.Errorf("app: load run status: %w", err)
		}
	}

	// Check and merge executions: retire each claimed group; ambiguity is
	// never absence.
	for i := range detail.PendingOperations {
		op := detail.PendingOperations[i]
		switch op.Kind {
		case OpCheckRun:
			still, retireErr := c.retireCheckOperation(ctx, handle, &op)
			if retireErr != nil {
				return StopReport{RunState: string(run.RunStopping)}, retireErr
			}
			note(still)
		case OpIntegrationMerge:
			still, retireErr := c.recoverIntegrationMerge(ctx, handle, &op)
			if retireErr != nil {
				return StopReport{RunState: string(run.RunStopping)}, retireErr
			}
			note(still)
		}
	}

	// The ref-fencing rule runs BEFORE the integration settles: a zombie
	// publish that LANDED is adopted here (the integration enters
	// checking on its published candidate), so the stop reset below rolls
	// the candidate back rather than leaving the ref on an unvalidated
	// commit; an unmoved-head publish intent is fenced, burning its
	// expected-old value. No terminal report while a ref-move intent is
	// unresolved.
	refsOutstanding, refErr := c.retireUnresolvedRefIntents(ctx, handle, &frozen)
	if refErr != nil {
		return StopReport{RunState: string(run.RunStopping)}, refErr
	}
	note(refsOutstanding)
	// integration.init is a ref-move intent too: it is resolved (adopted,
	// failed, or completed by its own create-only CAS) before any terminal
	// report, so nothing can create the ref afterward.
	initOutstanding, initErr := c.settleIntegrationInitForShutdown(ctx, handle)
	if initErr != nil {
		return StopReport{RunState: string(run.RunStopping)}, initErr
	}
	outstanding = append(outstanding, initOutstanding...)
	// An unresolved per-attempt worktree.create is never abandoned: it is
	// adopted and recorded, or settled failed after its bounded wait,
	// before any terminal report.
	worktreeOutstanding, wtErr := c.resolveAttemptWorktreesForShutdown(ctx, handle, &frozen)
	if wtErr != nil {
		return StopReport{RunState: string(run.RunStopping)}, wtErr
	}
	outstanding = append(outstanding, worktreeOutstanding...)

	// The integration itself: a merging integration with its merge
	// settled interrupts; a published-but-unsettled candidate (checking)
	// is rolled back by the reset; a check-failed one completes its
	// pending rollback — a half-published rejected candidate is retired,
	// not abandoned. A reset that journals without settling is
	// outstanding work, never quiescence.
	if len(outstanding) == 0 && refsOutstanding == "" {
		still, settleErr := c.settleIntegrationForStop(ctx, handle, &frozen)
		if settleErr != nil {
			return StopReport{RunState: string(run.RunStopping)}, settleErr
		}
		note(still)
	}

	// Every owned session, manager included, under the close rule.
	sessionOutstanding, err := c.stopFeatureSessions(ctx, handle, detail)
	if err != nil {
		return StopReport{RunState: string(run.RunStopping)}, err
	}
	outstanding = append(outstanding, sessionOutstanding...)

	if len(outstanding) > 0 {
		return StopReport{RunState: string(run.RunStopping), Outstanding: outstanding}, nil
	}
	return c.finishFeatureStop(ctx, handle)
}

// settleIntegrationForStop settles the run's current integration under
// stop precedence through the shared shutdown procedure.
func (c *Controller) settleIntegrationForStop(ctx context.Context, handle RunHandle, frozen *FrozenRun) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	return c.settleIntegrationForShutdown(ctx, handle, frozen, "stop")
}

// settleIntegrationForShutdown settles the run's current integration on
// a shutdown boundary (stop, or terminal failure): merging (merge
// already settled or never executed) → interrupted; checking → the
// reset rolls the published candidate back; check-failed → the reset
// completes. cause names the boundary in the recorded reasons.
// outstanding is non-empty while a reset journaled without settling.
func (c *Controller) settleIntegrationForShutdown(ctx context.Context, handle RunHandle, frozen *FrozenRun, cause string) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	integ, exists, err := c.currentIntegration(ctx, handle)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", nil
	}
	switch integ.State {
	case run.IntegrationMerging:
		return "", c.settleIntegrationTerminal(ctx, handle, frozen, integ.ID, integrationSettlement{
			OpID: "", OpState: "", OpOutcome: nil,
			TargetState: run.IntegrationInterrupted,
			Reason:      cause + " before a candidate was published",
		})
	case run.IntegrationChecking:
		return c.driveIntegrationReset(ctx, handle, frozen, &integ, cause+" with a published-but-unsettled candidate")
	case run.IntegrationCheckFailed:
		return c.driveIntegrationReset(ctx, handle, frozen, &integ, cause+" completing a pending rollback")
	default:
		return "", nil
	}
}

// stopFeatureSessions drives one close round over every non-terminal
// session, stop-reason targets, and terminates each session once its
// absence is observed.
func (c *Controller) stopFeatureSessions(ctx context.Context, handle RunHandle, detail RunDetail) ([]string, error) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; called once per stop round.
	var live []run.Session
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "feature stop")
		if wfErr != nil {
			return wfErr
		}
		sessions, sessErr := wf.SessionIndex().ByRun(ctx, handle.runID)
		if sessErr != nil {
			return sessErr
		}
		for i := range sessions {
			if sessions[i].State == run.SessionTerminated || sessions[i].State == run.SessionLost {
				continue
			}
			live = append(live, sessions[i])
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Slice(live, func(i, j int) bool { return live[i].ID < live[j].ID })

	var outstanding []string
	for i := range live {
		session := live[i]
		if session.State == run.SessionReserved {
			if err := c.terminateRetiredSession(ctx, handle, session.ID, "stop before launch"); err != nil {
				return nil, err
			}
			continue
		}
		binding, bindingFound, claim, claimFound, markers, err := c.sessionCloseEvidence(ctx, handle, &session)
		if err != nil {
			return nil, err
		}
		if claimFound && claim.State == LaunchClaimExecFailed {
			if termErr := c.terminateRetiredSession(ctx, handle, session.ID, "stop: exec failed, no process"); termErr != nil {
				return nil, termErr
			}
			continue
		}
		if !bindingFound || binding.PaneID == "" {
			// A launch whose binding was never committed: Phase 2's
			// unbound-launch rule, session-keyed.
			verdict, still, resolveErr := c.resolveUnplacedLaunch(ctx, handle, &session)
			if resolveErr != nil {
				return nil, resolveErr
			}
			switch verdict {
			case unplacedNothingLive:
				if termErr := c.terminateRetiredSession(ctx, handle, session.ID, "stop: no launch live"); termErr != nil {
					return nil, termErr
				}
				continue
			case unplacedOutstanding:
				outstanding = append(outstanding, fmt.Sprintf("session %s: %s", session.ID, still))
				continue
			case unplacedBound:
			}
			binding, bindingFound, claim, claimFound, markers, err = c.sessionCloseEvidence(ctx, handle, &session)
			if err != nil {
				return nil, err
			}
			if !bindingFound || binding.PaneID == "" {
				outstanding = append(outstanding, fmt.Sprintf("session %s: the recovered placement could not be re-read; failing closed", session.ID))
				continue
			}
		}
		if !claimFound {
			_, absent, ambiguous := c.observePlacedPaneAbsence(ctx, binding.ServerInstance, binding.PaneID, binding.CreationLabel)
			if ambiguous == "" && absent {
				if termErr := c.terminateRetiredSession(ctx, handle, session.ID, "stop: pane observed absent with no claim"); termErr != nil {
					return nil, termErr
				}
				continue
			}
			if ambiguous != "" {
				outstanding = append(outstanding, fmt.Sprintf("session %s has no launch claim to retire against; %s", session.ID, ambiguous))
				continue
			}
			outstanding = append(outstanding, fmt.Sprintf("session %s has no launch claim to retire against; failing closed", session.ID))
			continue
		}
		target := paneCloseTarget{
			PaneID:         binding.PaneID,
			Label:          binding.CreationLabel,
			SessionID:      session.ID,
			IncarnationID:  binding.IncarnationID,
			PID:            claim.PID,
			Markers:        markers,
			Reason:         closeReasonStop,
			ServerInstance: binding.ServerInstance,
			// An unsettled launch is observed terminated only as the
			// corroborated-absence pair: pane absent and claimed process gone.
			requireProcessGone: claim.State == LaunchClaimExecPending,
		}
		retired, still, closeErr := c.closePaneOperation(ctx, handle, detail, &target)
		if closeErr != nil {
			return nil, closeErr
		}
		if !retired {
			outstanding = append(outstanding, fmt.Sprintf("session %s: %s", session.ID, still))
			continue
		}
		if termErr := c.terminateRetiredSession(ctx, handle, session.ID, "stop: termination observed"); termErr != nil {
			return nil, termErr
		}
	}
	return outstanding, nil
}

// finishFeatureStop interrupts every non-terminal task and attempt and
// marks the run stopped, in one transaction, once every piece of owned
// work has been confirmed terminated by the caller.
func (c *Controller) finishFeatureStop(ctx context.Context, handle RunHandle) (StopReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per stop completion.
	now := c.Clock.Now()
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "finish feature stop")
		if wfErr != nil {
			return wfErr
		}
		if quiesceErr := assertRunQuiescedLocked(ctx, uow, wf, handle.runID); quiesceErr != nil {
			return quiesceErr
		}
		generation := gen(handle.lease.Generation)
		tasks, taskErr := wf.TaskIndex().ByRun(ctx, handle.runID)
		if taskErr != nil {
			return taskErr
		}
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].Seq < tasks[j].Seq })
		for i := range tasks {
			t := tasks[i]
			switch t.State {
			case run.TaskCompleted, run.TaskIntegrated, run.TaskFailed, run.TaskInterrupted:
				continue
			}
			_, rev, getErr := uow.Tasks().Get(ctx, t.ID)
			if getErr != nil {
				return getErr
			}
			tFrom := t.State
			next, trErr := t.Interrupt(now)
			if trErr != nil {
				return trErr
			}
			if _, saveErr := uow.Tasks().Save(ctx, next, rev); saveErr != nil {
				return saveErr
			}
			if err := recordTransition(ctx, uow, EntityTask, t.ID.String(), string(tFrom), string(next.State), "stop", generation, now); err != nil {
				return err
			}
			attempts, attErr := wf.AttemptIndex().ByTask(ctx, t.ID)
			if attErr != nil {
				return attErr
			}
			for j := range attempts {
				a := attempts[j]
				switch a.State {
				case run.AttemptCompleted, run.AttemptFailed, run.AttemptInterrupted:
					continue
				}
				_, aRev, getErr := uow.Attempts().Get(ctx, a.ID)
				if getErr != nil {
					return getErr
				}
				aFrom := a.State
				nextAttempt, trErr := a.Interrupt(now)
				if trErr != nil {
					return trErr
				}
				if _, saveErr := uow.Attempts().Save(ctx, nextAttempt, aRev); saveErr != nil {
					return saveErr
				}
				if err := recordTransition(ctx, uow, EntityAttempt, a.ID.String(), string(aFrom), string(nextAttempt.State), "stop", generation, now); err != nil {
					return err
				}
			}
		}
		return markRunStopped(ctx, uow, handle.runID, generation, now)
	})
	if errors.Is(err, errRunNotQuiesced) {
		return StopReport{RunState: string(run.RunStopping), Outstanding: []string{err.Error()}}, nil
	}
	if err != nil {
		return StopReport{}, fmt.Errorf("app: finish feature stop: %w", err)
	}
	return StopReport{RunState: string(run.RunStopped), Terminated: true}, nil
}
