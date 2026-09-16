package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

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
	// RunFailing is true when a terminal-failure cause is durable but its
	// settlement has not completed this round (owned work still
	// outstanding): the run is still running, yet it will never accept
	// new work again, so the caller schedules nothing more until the
	// failure settles.
	RunFailing bool
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

	candidates, inFlight, failureCause, err := c.retirementCandidates(ctx, handle)
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

	// A LIVE controller that establishes an in-flight worker's absence
	// (the strict positive rule) with no accepted result marks the
	// attempt interrupted and notifies the manager: a self-exiting worker
	// is a behavioral failure, and retrying it is manager judgment, never
	// an automatic same-attempt relaunch (section 5). Ambiguity changes
	// nothing.
	for i := range inFlight {
		exited, err := c.observeWorkerExit(ctx, handle, &frozen, &inFlight[i].Session)
		if err != nil {
			return report, err
		}
		if exited {
			report.Retired = append(report.Retired, inFlight[i].Session.ID.String())
		}
	}

	if failureCause != "" && len(report.Outstanding) == 0 {
		failed, outstanding, err := c.driveFeatureTerminalFailure(ctx, handle, &frozen, failureCause)
		if err != nil {
			return report, err
		}
		report.RunFailed = failed
		if outstanding != "" {
			report.Outstanding = append(report.Outstanding, outstanding)
		}
	}
	report.RunFailing = failureCause != "" && !report.RunFailed
	return report, nil
}

// The durable terminal-failure causes a feature run can carry, recorded as
// the failing run transition's reason.
const (
	// taskFailureCause: a task reached failed, so readiness can never hold
	// again.
	taskFailureCause = "task failure with owned-work termination observed"
	// managerLaunchFailureCause: the manager lineage's most recent session
	// failed to exec. No design row covers it; like a solo exec failure in
	// Phase 2, it fails the run.
	managerLaunchFailureCause = "manager launch exec failed (exec_failed claim; no process) with owned-work termination observed"
)

// retirementCandidates reads the run's child sessions and decides which
// have reached a retirement boundary; inFlight lists sessions whose
// attempt is still in flight under a settled claim — the self-exit
// observation probes them for positive absence. failureCause is the run's
// durable terminal-failure cause, "" when it has none: a failed task
// (taskFailureCause) or, failing that, a manager lineage that ended in an
// exec failure (managerLaunchFailureCause). While a cause stands, every
// live child is retired with the "run terminal failure" boundary.
func (c *Controller) retirementCandidates(ctx context.Context, handle RunHandle) ([]retirementCandidate, []retirementCandidate, string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	var (
		candidates   []retirementCandidate
		inFlight     []retirementCandidate
		failureCause string
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
				failureCause = taskFailureCause
			}
		}
		if failureCause == "" {
			managerFailed, mgrErr := managerLaunchFailedLocked(ctx, uow, wf, handle.runID)
			if mgrErr != nil {
				return mgrErr
			}
			if managerFailed {
				failureCause = managerLaunchFailureCause
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
			if boundary == "" && failureCause != "" {
				boundary = "run terminal failure"
			}
			if boundary == "" {
				if attempt.State == run.AttemptLaunching || attempt.State == run.AttemptRunning {
					inFlight = append(inFlight, retirementCandidate{Session: s})
				}
				continue
			}
			candidates = append(candidates, retirementCandidate{Session: s, Boundary: boundary})
		}
		return nil
	})
	return candidates, inFlight, failureCause, err
}

// managerLaunchFailedLocked reports, inside the caller's transaction,
// whether handle's run carries the manager exec-failure cause: the run is
// launching or running, and the MOST RECENT session of its manager
// lineage has a current binding whose launch claim is exec_failed. The
// most recent session is the live manager, else the lineage's one member
// never marked lost — a cold relaunch marks every predecessor lost in its
// successor's creating transaction, so an earlier exec-failed member
// followed by a working successor never counts, and neither does a
// manager retired at completion, at stop or by an earlier terminal
// failure (its claim settled execed, or the run is no longer launching or
// running). Any other lineage shape — no manager, or more than one member
// never marked lost — is no cause.
func managerLaunchFailedLocked(ctx context.Context, uow UnitOfWork, wf WorkflowRepositories, runID identity.RunID) (bool, error) {
	r, _, err := uow.Runs().Get(ctx, runID)
	if err != nil {
		return false, err
	}
	if r.State != run.RunLaunching && r.State != run.RunRunning {
		return false, nil
	}
	head, found, err := latestManagerSessionLocked(ctx, wf, runID)
	if err != nil || !found {
		return false, err
	}
	binding, bindingFound, err := uow.Bindings().Current(ctx, head.ID)
	if err != nil || !bindingFound {
		return false, err
	}
	claim, claimFound, err := uow.LaunchClaims().Get(ctx, binding.IncarnationID)
	if err != nil {
		return false, err
	}
	return claimFound && claim.State == LaunchClaimExecFailed, nil
}

// latestManagerSessionLocked resolves the most recent session of runID's
// manager lineage (see managerLaunchFailedLocked); found is false when no
// single most recent member exists.
func latestManagerSessionLocked(ctx context.Context, wf WorkflowRepositories, runID identity.RunID) (run.Session, bool, error) {
	live, _, err := wf.ManagerSession(ctx, runID)
	switch {
	case err == nil:
		return live, true, nil
	case !errors.Is(err, ErrNotFound):
		return run.Session{}, false, err
	}
	sessions, err := wf.SessionIndex().ByRun(ctx, runID)
	if err != nil {
		return run.Session{}, false, err
	}
	var (
		head  run.Session
		heads int
	)
	for i := range sessions {
		if sessions[i].Role == run.RoleManager && sessions[i].State != run.SessionLost {
			head = sessions[i]
			heads++
		}
	}
	return head, heads == 1, nil
}

// observeWorkerExit probes one in-flight worker for a self-exit: only a
// POSITIVELY absent pane (by id and by label, under a settled claim)
// establishes it; ambiguity and a live occupant change nothing. On
// establishment, one transaction interrupts the attempt, settles the
// task by budget (a failure closure journals the orphaned obligations),
// terminates the session and commits the manager notice.
func (c *Controller) observeWorkerExit(ctx context.Context, handle RunHandle, frozen *FrozenRun, session *run.Session) (bool, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per in-flight session per round.
	binding, bindingFound, claim, claimFound, _, err := c.sessionCloseEvidence(ctx, handle, session)
	if err != nil {
		return false, err
	}
	if !bindingFound || binding.PaneID == "" || !claimFound || claim.State != LaunchClaimExeced {
		return false, nil // pre-claim launches stay the launch machinery's.
	}
	_, absent, ambiguous := c.observePaneAbsence(ctx, binding.PaneID, binding.CreationLabel)
	if ambiguous != "" || !absent {
		return false, nil
	}
	return true, c.settleWorkerInterruption(ctx, handle, frozen, session)
}

// workerTermination describes one terminal attempt outcome a live
// controller settles for a child session that has no accepted result:
// the attempt's own terminal state and the reasons the settlement
// records. The task consequence is never part of it — the section 5
// budget rule decides that inside the settling transaction.
type workerTermination struct {
	// kind names the settlement in errors and the transaction label.
	kind string
	// failAttempt moves the attempt to failed; otherwise it is interrupted.
	failAttempt bool
	// reason is the attempt and task transition reason.
	reason string
	// sessionReason is the session termination reason.
	sessionReason string
	// noticeReason renders the manager notice's reason line for the
	// decided task consequence.
	noticeReason func(taskConsequence) string
}

// workerSelfExit is an observed self-exit: the attempt is interrupted.
func workerSelfExit() workerTermination {
	return workerTermination{
		kind:          "interruption",
		reason:        "worker exited without an accepted result",
		sessionReason: "worker exited without an accepted result; observed absent",
		noticeReason: func(taskConsequence) string {
			return "worker exited without an accepted result (observed absent under its settled claim)"
		},
	}
}

// workerExecFailure is an exec_failed launch claim (design section 5's
// "exec failure" terminal attempt outcome): the attempt failed, whatever
// the task consequence — a held stop interrupts the task, never the
// observed outcome.
func workerExecFailure() workerTermination {
	return workerTermination{
		kind:          "exec failure",
		failAttempt:   true,
		reason:        "exec_failed claim",
		sessionReason: "exec_failed claim; no process",
		noticeReason: func(consequence taskConsequence) string {
			if consequence == taskConsequenceInterrupted {
				return "the attempt failed to launch (exec_failed claim; no process) while a stop was pending"
			}
			return "the attempt failed to launch (exec_failed claim; no process)"
		},
	}
}

// settleWorkerInterruption settles an observed self-exit.
func (c *Controller) settleWorkerInterruption(ctx context.Context, handle RunHandle, frozen *FrozenRun, session *run.Session) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per established self-exit.
	return c.settleWorkerTermination(ctx, handle, frozen, session, workerSelfExit())
}

// settleChildExecFailure settles one implementer or reviewer session
// whose current launch claim is exec_failed, as the terminal attempt
// outcome design section 5 names: in one transaction the attempt fails,
// the task takes the budgeted consequence (needs-rework with retries
// left, failed at the limit with its mailbox closed, interrupted under a
// held stop), the session is terminated and the manager notice commits.
// An attempt already past launching or relaunching carries no launch
// outcome to settle; only the session is terminated. It assumes nothing
// about its caller beyond a held lease: the scheduling pass's launch
// corroboration and resume's reconciliation both settle through it.
func (c *Controller) settleChildExecFailure(ctx context.Context, handle RunHandle, frozen *FrozenRun, session *run.Session) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per exec-failed child session.
	if session.AttemptID == "" {
		return fmt.Errorf("app: session %s has no attempt; an exec-failed manager fails the run instead", session.ID)
	}
	var attemptState run.AttemptState
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		a, _, err := uow.Attempts().Get(ctx, session.AttemptID)
		attemptState = a.State
		return err
	}); err != nil {
		return err
	}
	if attemptState != run.AttemptLaunching && attemptState != run.AttemptRelaunching {
		return c.terminateRetiredSession(ctx, handle, session.ID, workerExecFailure().sessionReason)
	}
	return c.settleWorkerTermination(ctx, handle, frozen, session, workerExecFailure())
}

// settleWorkerTermination settles one child session's terminal attempt
// outcome: the prediction, the file-first manager notice and the settling
// transaction, retried while concurrent sends move the mailbox.
func (c *Controller) settleWorkerTermination(ctx context.Context, handle RunHandle, frozen *FrozenRun, session *run.Session, outcome workerTermination) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per settled child session.
	var task run.Task
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		attempt, _, attErr := uow.Attempts().Get(ctx, session.AttemptID)
		if attErr != nil {
			return attErr
		}
		t, _, taskErr := uow.Tasks().Get(ctx, attempt.TaskID)
		if taskErr != nil {
			return taskErr
		}
		task = t
		return nil
	}); err != nil {
		return err
	}

	for round := 0; round < settlementNoticeRetries; round++ {
		prediction, obligations, err := c.predictTaskConsequence(ctx, handle, frozen, task.ID, false)
		if err != nil {
			return err
		}
		body := renderTaskNotice(&task, prediction, outcome.noticeReason(prediction), obligations)
		notice, err := c.prepareControllerNotice(ctx, handle, frozen.Snapshot.StateRoot, body)
		if err != nil {
			return err
		}
		err = c.applyWorkerTermination(ctx, handle, frozen, session, &task, &outcome, prediction, obligations, notice)
		if errors.Is(err, errSettlementRetry) {
			continue
		}
		return err
	}
	return fmt.Errorf("app: worker %s %s settlement kept racing concurrent sends", session.ID, outcome.kind)
}

// applyWorkerTermination is one worker-termination settlement transaction.
func (c *Controller) applyWorkerTermination(ctx context.Context, handle RunHandle, frozen *FrozenRun, session *run.Session, task *run.Task, outcome *workerTermination, prediction taskConsequence, obligations []string, notice controllerNotice) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "worker "+outcome.kind)
		if wfErr != nil {
			return wfErr
		}
		r, _, runErr := uow.Runs().Get(ctx, handle.runID)
		if runErr != nil {
			return runErr
		}
		a, aRev, attErr := uow.Attempts().Get(ctx, session.AttemptID)
		if attErr != nil {
			return attErr
		}
		generation := gen(handle.lease.Generation)
		if outcome.failAttempt && a.State != run.AttemptLaunching && a.State != run.AttemptRelaunching {
			// Another settlement moved the attempt since the prediction:
			// nothing launch-shaped is left to settle but the session.
			return terminateSession(ctx, uow, session.ID, outcome.sessionReason, generation, now)
		}
		t, tRev, taskErr := uow.Tasks().Get(ctx, task.ID)
		if taskErr != nil {
			return taskErr
		}
		attempts, listErr := wf.AttemptIndex().ByTask(ctx, task.ID)
		if listErr != nil {
			return listErr
		}
		consequence := decideTaskConsequence(r.StopRequested, len(attempts), retryLimitFor(&frozen.Snapshot))
		if consequence != prediction {
			return errSettlementRetry
		}

		aFrom := a.State
		var aNext run.Attempt
		var trErr error
		if outcome.failAttempt {
			aNext, trErr = a.Fail(now)
		} else {
			aNext, trErr = a.Interrupt(now)
		}
		if trErr != nil {
			return trErr
		}
		if _, saveErr := uow.Attempts().Save(ctx, aNext, aRev); saveErr != nil {
			return saveErr
		}
		if err := recordTransition(ctx, uow, EntityAttempt, a.ID.String(), string(aFrom), string(aNext.State), outcome.reason, generation, now); err != nil {
			return err
		}

		tFrom := t.State
		var tNext run.Task
		switch consequence {
		case taskConsequenceInterrupted:
			tNext, trErr = t.Interrupt(now)
		case taskConsequenceFailed:
			tNext, trErr = t.Fail(now)
		default:
			tNext, trErr = t.NeedsRework(now)
		}
		if trErr != nil {
			return trErr
		}
		if consequence == taskConsequenceFailed {
			current, oblErr := pendingTaskObligations(ctx, wf, handle.runID, task.ID)
			if oblErr != nil {
				return oblErr
			}
			if !slices.Equal(current, obligations) {
				return errSettlementRetry
			}
			tNext = tNext.CloseMailbox(now)
		}
		if _, saveErr := uow.Tasks().Save(ctx, tNext, tRev); saveErr != nil {
			return saveErr
		}
		if err := recordTransition(ctx, uow, EntityTask, t.ID.String(), string(tFrom), string(tNext.State), outcome.reason, generation, now); err != nil {
			return err
		}

		if err := terminateSession(ctx, uow, session.ID, outcome.sessionReason, generation, now); err != nil {
			return err
		}
		return commitControllerNotice(ctx, wf, handle.runID, notice, now)
	})
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
// a failed implement task can never satisfy readiness again, so the
// shutdown mirrors stop — every check and merge execution's process
// group is ACTIVELY retired under the group-retirement rule, every
// ref-move intent is retired under the ref-fencing rule, and the
// current integration settles through the shared shutdown procedure (a
// published-but-unsettled candidate is rolled back, never left on the
// ref of a failed run). Only then, once every child session is
// terminal, is the manager retired under the close rule and the run
// marked failed — with stop precedence: a held stop leaves the terminal
// state to stop handling. cause is the durable failure cause, recorded as
// the run transition's reason. A launching run fails the same way: a
// manager whose launch exec failed leaves a launching run nothing else to
// reach.
func (c *Controller) driveFeatureTerminalFailure(ctx context.Context, handle RunHandle, frozen *FrozenRun, cause string) (bool, string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	detail, err := c.Read.LoadRunStatus(ctx, handle.runID)
	if err != nil {
		return false, "", err
	}
	if detail.StopRequested {
		return false, "", nil // stop precedence: stop handling owns the terminal state.
	}
	if !terminalFailureAccepts(detail.State) {
		return false, "", nil
	}
	var stillOutstanding []string
	for i := range detail.PendingOperations {
		op := detail.PendingOperations[i]
		switch op.Kind {
		case OpCheckRun:
			still, retireErr := c.retireCheckOperation(ctx, handle, &op)
			if retireErr != nil {
				return false, "", retireErr
			}
			if still != "" {
				stillOutstanding = append(stillOutstanding, still)
			}
		case OpIntegrationMerge:
			still, retireErr := c.recoverIntegrationMerge(ctx, handle, &op)
			if retireErr != nil {
				return false, "", retireErr
			}
			if still != "" {
				stillOutstanding = append(stillOutstanding, still)
			}
		}
	}
	if len(stillOutstanding) > 0 {
		return false, "terminal failure blocked: " + strings.Join(stillOutstanding, "; "), nil
	}
	outstanding, err := c.retireUnresolvedRefIntents(ctx, handle, frozen)
	if err != nil {
		return false, "", err
	}
	if outstanding != "" {
		return false, "terminal failure blocked: " + outstanding, nil
	}
	stillIntegration, err := c.settleIntegrationForShutdown(ctx, handle, frozen, "run terminal failure")
	if err != nil {
		return false, "", err
	}
	if stillIntegration != "" {
		return false, "terminal failure blocked: " + stillIntegration, nil
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
		wf, wfErr := RequireWorkflowRepositories(uow, "feature terminal failure")
		if wfErr != nil {
			return wfErr
		}
		if quiesceErr := assertRunQuiescedLocked(ctx, uow, wf, handle.runID); quiesceErr != nil {
			return quiesceErr
		}
		r, rRev, getErr := uow.Runs().Get(ctx, handle.runID)
		if getErr != nil {
			return getErr
		}
		if r.StopRequested {
			return nil
		}
		if !terminalFailureAccepts(r.State) {
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
		return recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(rFrom), string(next.State), cause, gen(handle.lease.Generation), now)
	})
	if errors.Is(err, errRunNotQuiesced) {
		return false, "terminal failure blocked: " + err.Error(), nil
	}
	if err != nil {
		return false, "", err
	}
	return true, "", nil
}

// terminalFailureAccepts reports whether a run in state may take the
// feature terminal failure: every non-terminal, non-stopping state that
// can reach failed.
func terminalFailureAccepts(state run.RunState) bool {
	switch state {
	case run.RunLaunching, run.RunRunning, run.RunResuming, run.RunCompleting:
		return true
	default:
		return false
	}
}

// featureSessionsTerminal reports whether every session OTHER than the
// run's current manager is terminal, and returns that manager row. The
// current manager resolves through the ManagerSession port (ErrNotFound
// = no live manager) — never by scanning for the role, which would let
// a historical manager row from a cold relaunch shadow the live one in
// whatever order the index returns rows. Historical manager rows are
// ordinary sessions here: they must be terminal like any other.
func (c *Controller) featureSessionsTerminal(ctx context.Context, handle RunHandle) (bool, run.Session, bool, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	var (
		othersTerminal = true
		manager        run.Session
		managerFound   bool
	)
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "feature terminal failure")
		if wfErr != nil {
			return wfErr
		}
		current, _, mgrErr := wf.ManagerSession(ctx, handle.runID)
		switch {
		case mgrErr == nil:
			manager = current
			managerFound = true
		case errors.Is(mgrErr, ErrNotFound):
			// No live manager: every session below must be terminal.
		default:
			return mgrErr
		}
		sessions, sessErr := wf.SessionIndex().ByRun(ctx, handle.runID)
		if sessErr != nil {
			return sessErr
		}
		for i := range sessions {
			s := sessions[i]
			if managerFound && s.ID == manager.ID {
				continue
			}
			if s.State != run.SessionTerminated && s.State != run.SessionLost {
				othersTerminal = false
			}
		}
		return nil
	})
	return othersTerminal, manager, managerFound, err
}
