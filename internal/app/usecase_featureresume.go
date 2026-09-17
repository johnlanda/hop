package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// ResumeFeatureRequest is the string-facing input behind `hop resume` for
// a feature-mode run: composition passes raw strings.
type ResumeFeatureRequest struct {
	RunID        string
	ControllerID string
	// ConfirmAbsentSession carries `--confirm-absent <session-id>`: the
	// per-session human attestation (both required assertions implied by
	// the flag), journaled with its own continuity evidence. Empty means
	// no attestation was given.
	ConfirmAbsentSession string
	// HOPPath and StateRoot parameterize a cold relaunch's pane argv/env.
	HOPPath   string
	StateRoot string
}

// Feature session dispositions.
const (
	// SessionWarm: the occupant corroborated under the one predicate; the
	// session is (or now is) active.
	SessionWarm = "warm"
	// SessionPending: the launch claim is not settled; corroboration
	// continues on later rounds.
	SessionPending = "pending"
	// SessionReconciling: ambiguous evidence; nothing was acted on.
	SessionReconciling = "reconciling"
	// SessionRelaunched: the session was attested absent and its
	// successor launched (cold relaunch).
	SessionRelaunched = "relaunched"
	// SessionRetiredNoProcess: an exec-failed claim; nothing live.
	SessionRetiredNoProcess = "retired-no-process"
	// SessionRelaunchUnsupported: cold resume is Claude-only.
	SessionRelaunchUnsupported = "relaunch-unsupported"
	// SessionLaunchSuppressed: an assigned child whose launch never reached
	// a pane, with no worktree work unresolved, in a run carrying a
	// terminal-failure cause: nothing is launched for it, and the
	// controller loop's failure cleanup retires it.
	SessionLaunchSuppressed = "launch-suppressed"
)

// FeatureSessionReport is one session's reconciliation disposition.
type FeatureSessionReport struct {
	SessionID   string
	Role        string
	Disposition string
	Detail      string
}

// ResumeFeatureResult is one ResumeFeature round's outcome.
type ResumeFeatureResult struct {
	// Outcome: "resumed" (every session warm or terminal, its launch in
	// flight, or launch-suppressed for the loop's failure cleanup),
	// "reconciling",
	// "stop-pending" (a held stop routes to stop handling before any
	// adoption or new dispatch) or "nothing-to-do" (a terminal run).
	Outcome  string
	RunState string
	Sessions []FeatureSessionReport
	// Blocked names unresolved operations refusing continuation.
	Blocked []string
}

// absenceAttestationOutcome is the OpAbsenceAttested journal payload for
// a feature-mode per-session attestation.
type absenceAttestationOutcome struct {
	SessionID              string `json:"session_id"`
	AttestedNotRestored    bool   `json:"attested_not_restored"`
	AttestedNoOtherClient  bool   `json:"attested_no_other_client"`
	RecordedServerInstance string `json:"recorded_server_instance"`
	ObservedServerInstance string `json:"observed_server_instance"`
	PaneAbsentByID         bool   `json:"pane_absent_by_id"`
	PaneAbsentByLabel      bool   `json:"pane_absent_by_label"`
}

// ResumeFeature reconciles a feature-mode run after a controller loss:
// it acquires the lease (the takeover CAS — a concurrent resume loses it
// and exits reporting the holder), routes a held stop back to stop
// handling before any adoption or new dispatch, recovers unresolved
// integration operations per their decision-table rows, and reconciles
// EVERY session — manager, implementers, reviewer — under the one
// corroboration predicate. Cold relaunch is authorized per session, only
// through `--confirm-absent <session-id>`: the attestation is journaled
// with its own continuity evidence, and relaunch proceeds only with the
// pane positively absent by id and by label AND server continuity
// established; unknown continuity, inspection errors and a changed
// instance keep the session reconciling. A manager relaunch creates the
// SUCCESSOR manager session bound to the same native reference — the
// conversation continues, children keep their historical parent.
func (c *Controller) ResumeFeature(ctx context.Context, req ResumeFeatureRequest) (ResumeFeatureResult, RunHandle, error) { //nolint:gocritic // hugeParam: ResumeFeatureRequest is the driving DTO for hop resume, called once per invocation.
	runID, err := identity.ParseRunID(req.RunID)
	if err != nil {
		return ResumeFeatureResult{}, RunHandle{}, fmt.Errorf("app: parse run id: %w", err)
	}
	lease, err := c.Store.AcquireLease(ctx, runID, req.ControllerID)
	if err != nil {
		return ResumeFeatureResult{}, RunHandle{}, fmt.Errorf("app: acquire lease: %w", err)
	}
	handle := newRunHandle(runID, lease)

	frozen, err := c.Read.LoadFrozenRun(ctx, runID)
	if err != nil {
		return ResumeFeatureResult{}, handle, fmt.Errorf("app: load frozen run: %w", err)
	}
	if !frozen.Snapshot.Workflow.Feature() {
		return ResumeFeatureResult{}, handle, fmt.Errorf("app: run %s is not a feature-mode run; hop resume's solo path owns it", runID)
	}

	result := ResumeFeatureResult{}
	entryState, stopRequested, err := c.enterFeatureResuming(ctx, handle)
	if err != nil {
		return result, handle, err
	}
	result.RunState = entryState
	switch {
	case entryState == string(run.RunStopped) || entryState == string(run.RunCompleted) || entryState == string(run.RunFailed):
		result.Outcome = "nothing-to-do"
		return result, handle, nil
	case stopRequested:
		// A held stop always routes back to stop handling before any
		// adoption or new dispatch: a stopping run is never turned back
		// toward running.
		result.Outcome = "stop-pending"
		return result, handle, nil
	}

	blocked, err := c.recoverIntegrationOperations(ctx, handle, &frozen, req.HOPPath, nil)
	if err != nil {
		return result, handle, err
	}
	if blocked != "" {
		result.Blocked = append(result.Blocked, blocked)
	}

	// The feature startup continuation finishes a bootstrap the lost
	// controller left undone (integration.init, the manager's placement
	// and pane) before any session is reconciled.
	bootstrap, err := c.continueFeatureBootstrap(ctx, handle, &frozen, &req)
	if errors.Is(err, ErrStopRequested) {
		result.Outcome = "stop-pending"
		return result, handle, nil
	}
	if err != nil {
		return result, handle, err
	}
	if bootstrap.Blocked != "" {
		result.Blocked = append(result.Blocked, bootstrap.Blocked)
	}

	// Assigned attempts whose launch the lost controller left before its
	// pane.open intent are recovered next, before any session is
	// reconciled: adopted or re-driven and launched, or settled as launch
	// failures (resolve-only while a terminal-failure cause stands).
	launches, err := c.resumeAttemptLaunches(ctx, handle, &frozen, &req)
	if err != nil {
		return result, handle, err
	}
	result.Blocked = append(result.Blocked, launches.blocked...)
	if stopRefused(launches.conditions) {
		result.Outcome = "stop-pending"
		return result, handle, nil
	}

	sessions, err := c.featureRunSessions(ctx, handle)
	if err != nil {
		return result, handle, err
	}
	allSettled := true
	for i := range sessions {
		report, placedInFlight, reconErr := c.reconcileFeatureSession(ctx, handle, &frozen, &req, &sessions[i])
		if reconErr != nil {
			return result, handle, reconErr
		}
		switch condition, unresolved := launches.unresolved[sessions[i].ID]; {
		case report.Disposition != SessionPending:
		case launches.retirable[sessions[i].ID]:
			// No pane was ever requested for it and none can be: the run
			// goes back to running so the controller loop's retirement pass
			// — which reads the failure cause there — retires it and fails
			// the run.
			report.Disposition = SessionLaunchSuppressed
			report.Detail = "its launch never reached a pane and the run carries a terminal-failure cause; the controller loop's failure cleanup retires it"
		case unresolved:
			report.Detail = "attempt launch unresolved: " + condition.Detail
		}
		result.Sessions = append(result.Sessions, report)
		// A launch in flight — the manager's, a child's whose pane this
		// round opened, or a child's placed launch the lost controller left
		// unsettled (placedLaunchInFlight) — reports pending under the one
		// predicate; its settlement belongs to the loop's corroboration, so
		// it does not hold the run in reconciliation. An ambiguous launch
		// blocks its own relaunch, never the loop that corroborates it
		// (docs/plan/phase-2-design.md section 4, the launch row's takeover
		// column).
		launchInFlight := report.Disposition == SessionPending &&
			((bootstrap.ManagerLaunching && report.SessionID == bootstrap.ManagerSessionID.String()) || launches.opened[sessions[i].ID] || placedInFlight)
		if report.Disposition != SessionWarm && report.Disposition != SessionRelaunched && report.Disposition != SessionRetiredNoProcess &&
			report.Disposition != SessionLaunchSuppressed && !launchInFlight {
			allSettled = false
		}
	}

	if allSettled && len(result.Blocked) == 0 {
		if err := c.markFeatureRunning(ctx, handle); err != nil {
			return result, handle, err
		}
		result.Outcome = "resumed"
		result.RunState = string(run.RunRunning)
		if bootstrap.ManagerLaunching {
			result.RunState = string(run.RunLaunching)
		}
		return result, handle, nil
	}
	result.Outcome = "reconciling"
	result.RunState = string(run.RunResuming)
	return result, handle, nil
}

// resumeAttemptLaunches runs the per-attempt launch recovery for a resume
// round: continue mode, or resolve-only while the run carries a durable
// terminal-failure cause (nothing new is launched into a failing run).
// Once a cause stands after the round — at entry, or created by the
// round's own settlements — every unplaced launch whose worktree work is
// resolved is marked retirable, unless a launch or worktree intent is
// unattributable.
func (c *Controller) resumeAttemptLaunches(ctx context.Context, handle RunHandle, frozen *FrozenRun, req *ResumeFeatureRequest) (attemptLaunchRound, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per resume round.
	mode := attemptLaunchContinue
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, err := RequireWorkflowRepositories(uow, "ResumeFeature")
		if err != nil {
			return err
		}
		failing, err := featureFailureCauseLocked(ctx, uow, wf, handle.runID)
		if failing {
			mode = attemptLaunchResolveOnly
		}
		return err
	}); err != nil {
		return attemptLaunchRound{}, err
	}
	stateRoot := req.StateRoot
	if stateRoot == "" {
		stateRoot = frozen.Snapshot.StateRoot
	}
	round, err := c.recoverAttemptLaunches(ctx, handle, mode, &attemptLaunchInputs{
		frozen: frozen, hopPath: req.HOPPath, stateRoot: stateRoot,
		implementerBase: func(ctx context.Context) (string, error) { return c.ResolveIntegrationHead(ctx, handle) },
	})
	if err != nil || len(round.blocked) > 0 {
		return round, err
	}
	failing := false
	if readErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "ResumeFeature")
		if wfErr != nil {
			return wfErr
		}
		var causeErr error
		failing, causeErr = featureFailureCauseLocked(ctx, uow, wf, handle.runID)
		return causeErr
	}); readErr != nil || !failing {
		return round, readErr
	}
	targets, launchBlocked, err := c.unresolvedAttemptLaunches(ctx, handle)
	if err != nil || launchBlocked != "" {
		return round, err
	}
	for i := range targets {
		if round.pending[targets[i].attempt.ID] == "" {
			round.retirable[targets[i].session.ID] = true
		}
	}
	return round, nil
}

// enterFeatureResuming moves the run into resuming (idempotently for an
// already-resuming run; a terminal run is untouched) and reports whether
// a stop request is held.
func (c *Controller) enterFeatureResuming(ctx context.Context, handle RunHandle) (string, bool, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	now := c.Clock.Now()
	var (
		state         string
		stopRequested bool
	)
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		r, rRev, getErr := uow.Runs().Get(ctx, handle.runID)
		if getErr != nil {
			return getErr
		}
		stopRequested = r.StopRequested
		state = string(r.State)
		switch r.State {
		case run.RunStopped, run.RunCompleted, run.RunFailed, run.RunResuming, run.RunStopping:
			return nil
		}
		if stopRequested {
			return nil
		}
		rFrom := r.State
		next, trErr := r.EnterResuming(now)
		if trErr != nil {
			return trErr
		}
		if _, saveErr := uow.Runs().Save(ctx, next, rRev); saveErr != nil {
			return saveErr
		}
		state = string(next.State)
		return recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(rFrom), string(next.State), "feature resume entered", gen(handle.lease.Generation), now)
	})
	return state, stopRequested, err
}

// featureRunSessions lists the run's non-terminal sessions, sorted.
func (c *Controller) featureRunSessions(ctx context.Context, handle RunHandle) ([]run.Session, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	var live []run.Session
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "ResumeFeature")
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
	})
	sort.Slice(live, func(i, j int) bool { return live[i].ID < live[j].ID })
	return live, err
}

// reconcileFeatureSession reconciles one session under the one predicate.
// inFlight reports a pending session whose placed launch the controller
// loop's corroboration finishes (placedLaunchInFlight), so it does not
// hold the run in reconciliation.
func (c *Controller) reconcileFeatureSession(ctx context.Context, handle RunHandle, frozen *FrozenRun, req *ResumeFeatureRequest, session *run.Session) (report FeatureSessionReport, inFlight bool, err error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per session per resume round.
	report = FeatureSessionReport{SessionID: session.ID.String(), Role: string(session.Role)}
	binding, bindingFound, claim, claimFound, markers, err := c.sessionCloseEvidence(ctx, handle, session)
	if err != nil {
		return report, false, err
	}
	if !bindingFound && (!claimFound || claim.State != LaunchClaimExecFailed) {
		// A takeover adopts an unresolved pane.open by its unique creation
		// label (the decision table's pane.open row) before deciding: a
		// launch whose outcome the lost controller never recorded is
		// placed here, never left for a loop that would not run.
		if recoverErr := c.recoverSessionBindingByLabel(ctx, handle, session, bindingFound, binding); recoverErr != nil {
			return report, false, recoverErr
		}
		if binding, bindingFound, claim, claimFound, markers, err = c.sessionCloseEvidence(ctx, handle, session); err != nil {
			return report, false, err
		}
	}
	pendingDetail := ""
	switch {
	case claimFound && claim.State == LaunchClaimExecPending && bindingFound && binding.PaneID != "":
		// The launch-ended row, exactly as the loop's corroboration applies
		// it: a placed, unsettled launch whose pane and claimed process are
		// both observed gone is settled exec_failed, and the exec_failed
		// branch below then retires it.
		current, detail, endErr := c.settleIfLaunchEnded(ctx, handle, &binding, &claim)
		if endErr != nil {
			return report, false, endErr
		}
		claim = current
		pendingDetail = detail
	case claimFound && claim.State == LaunchClaimExecPending && !bindingFound:
		// Its label-only variant, for a launch whose placement was never
		// recorded and whose creation label answers nothing.
		current, detail, endErr := c.settleIfUnplacedLaunchEnded(ctx, handle, session)
		if endErr != nil {
			return report, false, endErr
		}
		claim = current
		pendingDetail = detail
	}
	if claimFound && claim.State == LaunchClaimExecFailed {
		// Nothing is live for this incarnation, placed or not (a launcher
		// whose exec failed has exited, closing its pane). A child's exec
		// failure is a terminal attempt outcome settled exactly as launch
		// corroboration settles it (attempt failed, budgeted task
		// consequence, mailbox closure, manager notice, session terminated);
		// the attempt-less manager's session is only terminated, and the
		// manager-lineage failure cause then fails the run on the next
		// retirement pass.
		managerReason := "resume: exec failed, no process"
		if launchEndedByController(&claim) {
			report.Detail = claim.Error
			managerReason = "resume: " + workerLaunchEnded(claim.Error).sessionReason
		}
		if session.Role != run.RoleManager {
			if err := c.settleChildExecFailure(ctx, handle, frozen, session); err != nil {
				return report, false, err
			}
		} else if err := c.terminateRetiredSession(ctx, handle, session.ID, managerReason); err != nil {
			return report, false, err
		}
		report.Disposition = SessionRetiredNoProcess
		return report, false, nil
	}
	if !bindingFound || binding.PaneID == "" {
		report.Disposition = SessionPending
		report.Detail = "no recorded placement; the launch may still be in flight"
		if pendingDetail != "" {
			report.Detail += " (" + pendingDetail + ")"
		}
		return report, false, nil
	}
	if !claimFound || claim.State != LaunchClaimExeced {
		report.Disposition = SessionPending
		if pendingDetail == "" {
			var flightErr error
			inFlight, pendingDetail, flightErr = c.placedLaunchInFlight(ctx, handle, session.ID, &binding, claimFound, &claim)
			if flightErr != nil {
				return report, false, flightErr
			}
		}
		report.Detail = "launch claim not settled; corroboration continues"
		switch {
		case inFlight:
			report.Detail = "launch claim not settled; the placed launch is in flight, and the controller loop corroborates it"
		case pendingDetail != "":
			report.Detail += " (" + pendingDetail + ")"
		}
		return report, inFlight, nil
	}

	pane, inspectErr := c.Runtime.InspectPane(ctx, binding.PaneID)
	if inspectErr == nil {
		settlement, _ := CorroborateSettlement(true, pane, markers, claim)
		if settlement == SettlementSettled {
			if err := c.confirmSessionActive(ctx, handle, session.ID, "warm reattach: occupant corroborated under the one predicate"); err != nil {
				return report, false, err
			}
			report.Disposition = SessionWarm
			return report, false, nil
		}
		report.Disposition = SessionReconciling
		report.Detail = fmt.Sprintf("occupant did not corroborate (%s); failing closed", settlement)
		if err := c.markSessionReconciling(ctx, handle, session.ID, sessionReconcileResumeAmbiguous); err != nil {
			return report, false, err
		}
		return report, false, nil
	}
	if !errors.Is(inspectErr, ErrPaneNotFound) {
		report.Disposition = SessionReconciling
		report.Detail = "pane inspection failed; ambiguous, never absence"
		return report, false, nil
	}

	// The pane is positively gone by id; absence also requires the label
	// lookup to succeed and find nothing.
	_, absent, ambiguous := c.observePaneAbsence(ctx, binding.PaneID, binding.CreationLabel)
	if ambiguous != "" || !absent {
		report.Disposition = SessionReconciling
		report.Detail = "absence not established: " + ambiguous
		if err := c.markSessionReconciling(ctx, handle, session.ID, sessionReconcileResumeAmbiguous); err != nil {
			return report, false, err
		}
		return report, false, nil
	}

	if req.ConfirmAbsentSession != session.ID.String() {
		report.Disposition = SessionReconciling
		report.Detail = "pane absent; cold relaunch requires --confirm-absent " + session.ID.String()
		if err := c.markSessionReconciling(ctx, handle, session.ID, sessionReconcileResumeAmbiguous); err != nil {
			return report, false, err
		}
		return report, false, nil
	}

	// The per-session attestation: journaled with its own continuity
	// evidence, authorizing relaunch only when both instance tokens are
	// non-empty and equal.
	observedInstance := c.observeServerInstance(ctx)
	if err := c.journalAttestation(ctx, handle, session.ID, binding.ServerInstance, observedInstance); err != nil {
		return report, false, err
	}
	if !ServerContinuityEstablished(binding.ServerInstance, observedInstance) {
		report.Disposition = SessionReconciling
		report.Detail = fmt.Sprintf("attestation recorded, but server continuity is not established (recorded %q, observed %q); a deferred native restore may still fire", binding.ServerInstance, observedInstance)
		if err := c.markSessionReconciling(ctx, handle, session.ID, sessionReconcileResumeAmbiguous); err != nil {
			return report, false, err
		}
		return report, false, nil
	}
	if session.Harness != run.HarnessClaude {
		report.Disposition = SessionRelaunchUnsupported
		report.Detail = "cold resume is Claude-only; recover this session by retiring the attempt and retrying the task"
		return report, false, nil
	}
	if session.NativeSessionRef == "" {
		report.Disposition = SessionReconciling
		report.Detail = "no native reference recorded; a cold relaunch cannot be rendered"
		return report, false, nil
	}
	if err := c.coldRelaunchFeatureSession(ctx, handle, frozen, req, session, &binding); err != nil {
		return report, false, err
	}
	report.Disposition = SessionRelaunched
	return report, false, nil
}

// confirmSessionActive moves a launching or reconciling session to
// active, idempotently for one already active.
func (c *Controller) confirmSessionActive(ctx context.Context, handle RunHandle, sessionID identity.SessionID, reason string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		s, rev, getErr := uow.Sessions().Get(ctx, sessionID)
		if getErr != nil {
			return getErr
		}
		if s.State == run.SessionActive {
			return nil
		}
		sFrom := s.State
		next, trErr := s.ConfirmActive(now)
		if trErr != nil {
			return trErr
		}
		if _, saveErr := uow.Sessions().Save(ctx, next, rev); saveErr != nil {
			return saveErr
		}
		return recordTransition(ctx, uow, EntitySession, sessionID.String(), string(sFrom), string(next.State), reason, gen(handle.lease.Generation), now)
	})
}

// sessionReconcileResumeAmbiguous is the reason every RESUME path records
// when it fails closed on a session whose evidence it cannot resolve. Each
// such path has already read the session's launch claim as SETTLED, which
// is what makes the reason below a structural fact and not a convention.
const sessionReconcileResumeAmbiguous = "resume: evidence ambiguous"

// markSessionReconciling moves an active or launching session to
// reconciling with reason as its recorded transition reason, idempotently.
//
// INVARIANT, on which the live launch corroboration's re-inspection rests:
// the only reconciling session whose current placement's launch claim is
// still exec_pending is the live wrapper reconciliation — the corroboration
// round's own SettlementForkingWrapper branch. Every resume path reaches
// this function only after reading that session's claim as settled
// (execed), so a resume-marked reconciling session always has a settled
// claim, and the structural identification "reconciling, with a current
// unsuperseded placed binding whose incarnation's claim is exec_pending"
// names the live reconciliation alone. A new caller that can mark a
// session reconciling while its placement's claim is still exec_pending
// breaks that identification and must be weighed against it.
func (c *Controller) markSessionReconciling(ctx context.Context, handle RunHandle, sessionID identity.SessionID, reason string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		s, rev, getErr := uow.Sessions().Get(ctx, sessionID)
		if getErr != nil {
			return getErr
		}
		if s.State == run.SessionReconciling {
			return nil
		}
		if s.State != run.SessionActive && s.State != run.SessionLaunching {
			return nil
		}
		sFrom := s.State
		next, trErr := s.Reconcile(now)
		if trErr != nil {
			return trErr
		}
		if _, saveErr := uow.Sessions().Save(ctx, next, rev); saveErr != nil {
			return saveErr
		}
		return recordTransition(ctx, uow, EntitySession, sessionID.String(), string(sFrom), string(next.State), reason, gen(handle.lease.Generation), now)
	})
}

// journalAttestation records the per-session `--confirm-absent`
// attestation as its own journal entry: both required assertions plus
// the recorded and observed server-continuity evidence.
func (c *Controller) journalAttestation(ctx context.Context, handle RunHandle, sessionID identity.SessionID, recordedInstance, observedInstance string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per attestation.
	opID, err := c.newOperationID()
	if err != nil {
		return err
	}
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpAbsenceAttested, State: OperationSucceeded,
			Outcome: absenceAttestationOutcome{
				SessionID:              sessionID.String(),
				AttestedNotRestored:    true,
				AttestedNoOtherClient:  true,
				RecordedServerInstance: recordedInstance,
				ObservedServerInstance: observedInstance,
				PaneAbsentByID:         true,
				PaneAbsentByLabel:      true,
			},
			CreatedAt: now, UpdatedAt: now,
		})
	})
}

// coldRelaunchFeatureSession creates the attested-absent session's
// successor and opens its pane: for the manager, the SUCCESSOR manager
// session bound to the same native reference (the conversation
// continues; the manager-uniqueness index admits it only once the
// predecessor is terminal, so the predecessor is marked lost in the same
// transaction); for a child, a successor session on the same attempt.
// Children keep their historical parent_session_id — provenance is never
// rewritten.
func (c *Controller) coldRelaunchFeatureSession(ctx context.Context, handle RunHandle, frozen *FrozenRun, req *ResumeFeatureRequest, prior *run.Session, priorBinding *run.RuntimeBinding) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per relaunch.
	successorID, err := identity.ParseSessionID(c.IDs.NewID())
	if err != nil {
		return fmt.Errorf("app: generate successor session id: %w", err)
	}
	incarnationID, err := identity.ParseIncarnationID(c.IDs.NewID())
	if err != nil {
		return fmt.Errorf("app: generate incarnation id: %w", err)
	}
	now := c.Clock.Now()

	// The relaunch pane reuses the session's recorded placement and cwd:
	// the newest pane.open intent for the prior session is the durable
	// source (never a fresh guess).
	cwd, err := c.priorPanePlacement(ctx, handle, prior.ID)
	if err != nil {
		return err
	}
	taskID, err := c.sessionTaskID(ctx, handle, prior)
	if err != nil {
		return err
	}
	if cwd == "" {
		if prior.Role != run.RoleManager {
			return fmt.Errorf("app: session %s has no recorded pane placement; a relaunch pane's cwd would be a guess", prior.ID)
		}
		cwd = frozen.RepositoryRoot
	}

	if txErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		generation := gen(handle.lease.Generation)
		s, rev, getErr := uow.Sessions().Get(ctx, prior.ID)
		if getErr != nil {
			return getErr
		}
		sFrom := s.State
		if s.State == run.SessionActive || s.State == run.SessionLaunching {
			reconciling, trErr := s.Reconcile(now)
			if trErr != nil {
				return trErr
			}
			if rev, err = uow.Sessions().Save(ctx, reconciling, rev); err != nil {
				return err
			}
			s = reconciling
		}
		lost, trErr := s.MarkLost(now)
		if trErr != nil {
			return trErr
		}
		if _, saveErr := uow.Sessions().Save(ctx, lost, rev); saveErr != nil {
			return saveErr
		}
		if err := recordTransition(ctx, uow, EntitySession, prior.ID.String(), string(sFrom), string(lost.State), "attested absent with server continuity; cold relaunch authorized", generation, now); err != nil {
			return err
		}
		// The current binding is superseded with the attestation evidence.
		current, found, bindErr := uow.Bindings().Current(ctx, prior.ID)
		if bindErr != nil {
			return bindErr
		}
		if found && current.IncarnationID == priorBinding.IncarnationID {
			superseded, supErr := current.Supersede("attested absent; cold relaunch", now)
			if supErr != nil {
				return supErr
			}
			if saveErr := uow.Bindings().Save(ctx, superseded); saveErr != nil {
				return saveErr
			}
		}

		var successor run.Session
		if prior.Role == run.RoleManager {
			successor = run.NewManagerSession(successorID, handle.runID, prior.Harness, now)
		} else {
			wf, wfErr := RequireWorkflowRepositories(uow, "cold relaunch")
			if wfErr != nil {
				return wfErr
			}
			manager, _, mgrErr := wf.ManagerSession(ctx, handle.runID)
			if mgrErr != nil {
				return mgrErr
			}
			child, childErr := run.NewChildSession(successorID, handle.runID, prior.AttemptID, prior.Role, manager, prior.Harness, now)
			if childErr != nil {
				return childErr
			}
			successor = child
		}
		successor, trErr = successor.AssignNativeRef(prior.NativeSessionRef, run.NativeRefAssigned, now)
		if trErr != nil {
			return trErr
		}
		if _, createErr := uow.Sessions().Create(ctx, successor); createErr != nil {
			return createErr
		}
		launching, trErr := successor.Launch(now)
		if trErr != nil {
			return trErr
		}
		if _, saveErr := uow.Sessions().Save(ctx, launching, 1); saveErr != nil {
			return saveErr
		}
		return recordTransition(ctx, uow, EntitySession, successorID.String(), string(run.SessionReserved), string(launching.State), "successor of "+prior.ID.String()+" (same native reference)", generation, now)
	}); txErr != nil {
		return txErr
	}

	return c.openRelaunchFeaturePane(ctx, handle, frozen, req, prior, successorID, incarnationID, taskID, cwd, priorBinding.WorkspaceID)
}

// priorPanePlacement recovers a session's recorded pane cwd from its
// newest pane.open intent — the durable placement source.
func (c *Controller) priorPanePlacement(ctx context.Context, handle RunHandle, sessionID identity.SessionID) (cwd string, err error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	err = c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		ops, opErr := uow.Operations().ByKind(ctx, handle.runID, OpPaneOpen)
		if opErr != nil {
			return opErr
		}
		for i := range ops {
			intent, ok := decodeOperationPayload[paneOpenIntent](ops[i].Intent)
			if !ok || string(intent.SessionID) != sessionID.String() {
				continue
			}
			cwd = intent.Cwd
			return nil
		}
		return nil
	})
	return cwd, err
}

// openRelaunchFeaturePane opens the successor session's pane: the Phase 2
// pane.open operation with LaunchRelaunch, a fresh incarnation, and the
// role-appropriate env.
func (c *Controller) openRelaunchFeaturePane(ctx context.Context, handle RunHandle, frozen *FrozenRun, req *ResumeFeatureRequest, prior *run.Session, successorID identity.SessionID, incarnationID identity.IncarnationID, taskID, cwd, workspaceID string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per relaunch.
	opID, err := c.newOperationID()
	if err != nil {
		return err
	}
	stateRoot := req.StateRoot
	if stateRoot == "" {
		stateRoot = frozen.Snapshot.StateRoot
	}
	argv := []string{req.HOPPath, "launch", "--run", handle.runID.String(), "--session", successorID.String()}
	env := map[string]string{
		"HOP_STATE_DIR":      stateRoot,
		"HOP_RUN_ID":         handle.runID.String(),
		"HOP_INCARNATION_ID": incarnationID.String(),
		"HOP_SESSION_ID":     successorID.String(),
		"HOP_ROLE":           string(prior.Role),
	}
	if taskID != "" {
		env["HOP_TASK_ID"] = taskID
	}
	if prior.AttemptID != "" {
		env["HOP_ATTEMPT_ID"] = prior.AttemptID.String()
	}
	serverInstance := c.observeServerInstance(ctx)
	intent := paneOpenIntent{Command: argv, Cwd: cwd, WorkspaceID: workspaceID, Label: opID.String(), IncarnationID: incarnationID, SessionID: successorID, ServerInstance: serverInstance}
	now := c.Clock.Now()

	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpPaneOpen, State: OperationPending, Intent: intent,
			CreatedAt: now, UpdatedAt: now,
		})
	}); err != nil {
		return fmt.Errorf("app: record relaunch pane.open intent: %w", err)
	}
	if err := c.revalidateForDispatch(ctx, handle, false); err != nil {
		return fmt.Errorf("app: revalidate before relaunch pane.open: %w", err)
	}
	actCtx, release := handle.actContext(ctx)
	paneHandle, actErr := c.Runtime.OpenWorkerPane(actCtx, WorkerPaneRequest{
		WorkspaceID: workspaceID, Cwd: cwd, Command: argv, Env: env, Label: opID.String(),
	})
	if actErr != nil {
		if ref, found, findErr := c.Runtime.FindPaneByLabel(actCtx, opID.String()); findErr == nil && found {
			paneHandle = PaneHandle(ref)
			actErr = nil
		}
	}
	release()

	outcomeErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		op, getErr := uow.Operations().Get(ctx, opID)
		if getErr != nil {
			return getErr
		}
		op.UpdatedAt = c.Clock.Now()
		if actErr != nil {
			op.State = OperationReconciling
			op.Outcome = actErr.Error()
			return uow.Operations().Save(ctx, op)
		}
		binding := run.NewRuntimeBinding(successorID, incarnationID, "", serverInstance, paneHandle.WorkspaceID, paneHandle.TabID, paneHandle.PaneID, opID.String(), run.LaunchResume, op.UpdatedAt)
		if bindErr := uow.Bindings().Create(ctx, binding); bindErr != nil {
			return bindErr
		}
		op.State = OperationSucceeded
		op.ActEvidence = paneHandle
		return uow.Operations().Save(ctx, op)
	})
	if outcomeErr != nil {
		return fmt.Errorf("app: record relaunch pane.open outcome: %w", outcomeErr)
	}
	if actErr != nil {
		return fmt.Errorf("app: relaunch pane.open: %w (operation %s is reconciling)", actErr, opID)
	}
	return nil
}

// markFeatureRunning returns a fully reconciled run from resuming to
// running.
func (c *Controller) markFeatureRunning(ctx context.Context, handle RunHandle) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		r, rRev, getErr := uow.Runs().Get(ctx, handle.runID)
		if getErr != nil {
			return getErr
		}
		if r.State != run.RunResuming || r.StopRequested {
			return nil
		}
		rFrom := r.State
		next, trErr := r.MarkRunning(now)
		if trErr != nil {
			return trErr
		}
		if _, saveErr := uow.Runs().Save(ctx, next, rRev); saveErr != nil {
			return saveErr
		}
		return recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(rFrom), string(next.State), "every session reconciled", gen(handle.lease.Generation), now)
	})
}

// sessionTaskID resolves the task a session's attempt belongs to, "" for
// an attempt-less session (the manager).
func (c *Controller) sessionTaskID(ctx context.Context, handle RunHandle, session *run.Session) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	if session.AttemptID == "" {
		return "", nil
	}
	var taskID string
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		attempt, _, getErr := uow.Attempts().Get(ctx, session.AttemptID)
		if getErr != nil {
			return getErr
		}
		taskID = attempt.TaskID.String()
		return nil
	})
	return taskID, err
}

// placedLaunchInFlight is resume's in-flight rule for a child launch the
// lost controller placed but never settled (docs/plan/phase-3-design.md
// section 5): the run resumes and the controller loop's corroboration
// finishes it — settlement, the forking-wrapper reconcile, the
// launch-ended row, or the pre-claim launch deadline — exactly as a live
// controller would have, since an ambiguous launch blocks its own
// relaunch, not the loop. It holds only when every conjunct is a positive
// observation:
//
//   - the session is an implementer or reviewer (the manager's launch is
//     the bootstrap continuation's) and is either launching or the live
//     launch corroboration's own reconciliation (wrapperReconciliation) —
//     the two states that step re-inspects. Every OTHER reconciling
//     session keeps failing closed, since nothing revisits it;
//   - its binding is committed, current and not superseded, and its claim
//     is absent or that placement's own exec_pending claim;
//   - the pane.open operation that recorded the placement is not failed;
//   - the recorded pane answers by id, and that very inspection was
//     answered by the server lifetime the placement recorded
//     (PaneProcess.ServerInstance, established on the inspection's own
//     connection): a graceful restart keeps public pane ids (pinned by
//     test/integration/spike_panevanish_test.go
//     TestSpikeLabelSurvivesRestart), so a recorded id answering under any
//     other or an unknown lifetime is no evidence of the original launch —
//     including a restart between an earlier identity read and the
//     inspection itself;
//   - the pane has a foreground occupant, and its own process is the
//     launch's: with a claim, the claimed process; with none yet, HOP's
//     own launcher for exactly this session, its argv equal to the
//     placement's frozen `hop launch --run <run> --session <session>`
//     command (pinnedLauncherOccupies). Under one server lifetime a pane id
//     names one pane and a command pane keeps its process for its whole
//     life, so that process is the one the placement spawned with this
//     incarnation's environment.
//
// Anything else is not in flight, and detail names why.
func (c *Controller) placedLaunchInFlight(ctx context.Context, handle RunHandle, sessionID identity.SessionID, binding *run.RuntimeBinding, claimFound bool, claim *LaunchClaim) (inFlight bool, detail string, err error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per pending session per resume round.
	var (
		session      run.Session
		placed       *Operation
		placedIntent paneOpenIntent
	)
	err = c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		s, _, getErr := uow.Sessions().Get(ctx, sessionID)
		if getErr != nil {
			return getErr
		}
		session = s
		ops, opErr := uow.Operations().ByKind(ctx, handle.runID, OpPaneOpen)
		if opErr != nil {
			return opErr
		}
		for i := range ops { // newest first
			intent, ok := decodeOperationPayload[paneOpenIntent](ops[i].Intent)
			if ok && intent.SessionID == sessionID && intent.IncarnationID == binding.IncarnationID {
				op := ops[i]
				placed, placedIntent = &op, intent
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return false, "", err
	}
	// The two session states the controller loop's corroboration step
	// re-inspects, and therefore the two it can finish.
	corroborated := session.State == run.SessionLaunching ||
		(session.State == run.SessionReconciling && wrapperReconciliation(binding, claimFound, claim))
	switch {
	case session.Role != run.RoleImplementer && session.Role != run.RoleReviewer:
		return false, "", nil
	case !corroborated:
		return false, fmt.Sprintf("the session is %s, and not a launch the controller loop still corroborates", session.State), nil
	case binding.PaneID == "" || binding.Superseded:
		return false, "the session has no current placement", nil
	case claimFound && (claim.State != LaunchClaimExecPending || claim.IncarnationID != binding.IncarnationID):
		return false, "the launch claim is not the placement's own unsettled claim", nil
	case placed == nil:
		return false, "no pane.open operation records the placement", nil
	case placed.State == OperationFailed:
		return false, "the placement's pane.open operation settled failed", nil
	}
	observed, detail := c.placedLaunchProcessObserved(ctx, binding, claimFound, claim, placedIntent.Command, handle.runID, sessionID)
	return observed, detail, nil
}

// placedLaunchProcessObserved is the positive-observation half of resume's
// in-flight rule (the last two conjuncts of placedLaunchInFlight's doc
// comment), shared verbatim by the manager bootstrap's own continuation of
// a launch the loop still corroborates. pinned is the placement's frozen
// pane.open command, read only where no claim exists yet.
func (c *Controller) placedLaunchProcessObserved(ctx context.Context, binding *run.RuntimeBinding, claimFound bool, claim *LaunchClaim, pinned []string, runID identity.RunID, sessionID identity.SessionID) (observed bool, detail string) {
	pane, inspectErr := c.Runtime.InspectPane(ctx, binding.PaneID)
	switch {
	case errors.Is(inspectErr, ErrPaneNotFound):
		return false, "the recorded pane does not answer by id"
	case inspectErr != nil:
		return false, "the recorded pane could not be inspected; ambiguous, never in flight"
	case !ServerContinuityEstablished(binding.ServerInstance, pane.ServerInstance):
		return false, "server continuity since the placement is not established for the inspection (the Herdr server may have restarted), so the recorded pane is no evidence of the launch"
	case len(pane.Foreground) == 0:
		return false, "the recorded pane answers with no foreground occupant"
	case claimFound && pane.ShellPID != claim.PID:
		return false, fmt.Sprintf("the recorded pane's process (pid %d) is not the claimed launch process (pid %d)", pane.ShellPID, claim.PID)
	case !claimFound && !pinnedLauncherOccupies(&pane, pinned, runID, sessionID):
		return false, "no launch claim yet, and the recorded pane's own process is not this session's hop launch invocation"
	}
	return true, ""
}

// pinnedLauncherOccupies reports whether the pane's own process is HOP's
// launcher for sessionID of runID: pinned must be the frozen session
// launch command — exactly [<absolute hop path> launch --run <runID>
// --session <sessionID>], a launcher invocation (isLauncherInvocation) —
// and the foreground member whose pid is the pane's own process (its shell
// pid) must report exactly that argv. Any other occupant, a pane with no
// inspectable process, and a command of any other shape report false.
func pinnedLauncherOccupies(pane *PaneProcess, pinned []string, runID identity.RunID, sessionID identity.SessionID) bool {
	if len(pinned) != 6 || !filepath.IsAbs(pinned[0]) || !isLauncherInvocation(pinned) ||
		pinned[2] != "--run" || pinned[3] != runID.String() || pinned[4] != "--session" || pinned[5] != sessionID.String() {
		return false
	}
	if pane.ShellPID <= 1 {
		return false
	}
	for _, fg := range pane.Foreground {
		if fg.PID == pane.ShellPID && slices.Equal(fg.Argv, pinned) {
			return true
		}
	}
	return false
}
