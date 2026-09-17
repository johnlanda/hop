package app

import (
	"context"
	"errors"
	"fmt"
	"time"

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

// TransitionReasonLaunchCorroboration is the reason the LIVE launch
// corroboration records when it fails closed on a foreground group that
// holds another process carrying the launch identity. It is value-free,
// and it is deliberately not the resume reason: this reconciliation is
// re-inspected by every later round, and a later clean observation of the
// same pane settles it.
//
// Exported because the transition journal is read outside this package:
// a fixture that reproduces this state has to write the SAME reason the
// live path writes, and a guard over reconciling transitions tells this
// one from every other by it.
const TransitionReasonLaunchCorroboration = "launch corroboration: another process on the pane carries the launch identity; re-inspected every pass"

// CorroborateSessionLaunches performs one inspection round for every
// non-terminal session of a feature-mode run currently in SessionLaunching
// state — manager, implementers and reviewer alike
// (docs/plan/phase-3-design.md section 6, "corroborate launches" in the
// extended scheduling pass, L796-798) — and for the one reconciliation
// this step itself produces, identified structurally by
// wrapperReconciliation. It is the per-session sibling of
// Phase 2's CorroborateLaunch: orchestration only, every occupant decision
// reuses CorroborateSettlement verbatim, and Phase 2's CorroborateLaunch
// and its RunDetail-bound helpers (driveLaunchDeadline,
// recoverBindingByLabel) are untouched — the solo path stays exactly as it
// was.
//
// An exec_failed claim is a terminal launch outcome. A child's settles
// through settleChildExecFailure (attempt failed, budgeted task
// consequence, mailbox closure, manager notice, session terminated, one
// transaction). A manager's fails the run: its session is terminated
// here, and while the run is launching — the scheduling pass runs nothing
// but this step then — the feature terminal failure is driven from here
// on this and every later round until it settles (RetireSettledSessions
// drives it for a running run).
func (c *Controller) CorroborateSessionLaunches(ctx context.Context, handle RunHandle) ([]SessionLaunchProgress, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per scheduling pass.
	frozen, err := c.Read.LoadFrozenRun(ctx, handle.runID)
	if err != nil {
		return nil, fmt.Errorf("app: load frozen run: %w", err)
	}
	sessions, err := c.featureRunSessions(ctx, handle)
	if err != nil {
		return nil, err
	}
	var reports []SessionLaunchProgress
	for i := range sessions {
		session := sessions[i]
		inspect, inspectErr := c.sessionUnderLaunchCorroboration(ctx, handle, &session)
		if inspectErr != nil {
			return reports, inspectErr
		}
		if !inspect {
			continue
		}
		progress, corrErr := c.corroborateSessionLaunch(ctx, handle, &frozen, &session)
		if corrErr != nil {
			return reports, corrErr
		}
		reports = append(reports, SessionLaunchProgress{SessionID: session.ID.String(), Role: string(session.Role), Progress: progress})
	}
	if err := c.driveLaunchingRunFailure(ctx, handle); err != nil {
		return reports, err
	}
	return reports, nil
}

// sessionUnderLaunchCorroboration reports whether one session is this
// step's to inspect: every launching session, plus the live wrapper
// reconciliation this step itself produces (wrapperReconciliation). A
// reconciling session in any other shape — every resume-marked one, whose
// claim is settled — is left exactly as it was.
func (c *Controller) sessionUnderLaunchCorroboration(ctx context.Context, handle RunHandle, session *run.Session) (bool, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per session per scheduling pass.
	switch session.State {
	case run.SessionLaunching:
		return true, nil
	case run.SessionReconciling:
		binding, _, claim, claimFound, _, err := c.sessionCloseEvidence(ctx, handle, session)
		if err != nil {
			return false, err
		}
		return wrapperReconciliation(&binding, claimFound, &claim), nil
	default:
		return false, nil
	}
}

// wrapperReconciliation is the STRUCTURAL identification of the live
// wrapper reconciliation, used wherever that state has to be told apart
// from every other reconciling session: a current, unsuperseded, placed
// binding whose own incarnation's launch claim is still exec_pending. The
// caller has already established that the session is reconciling. A
// binding that was not found is the zero value, whose empty pane id fails
// the first conjunct.
//
// It needs no recorded reason or marker of its own because
// markSessionReconciling's invariant makes the shape exclusive: every
// resume path marks a session reconciling only after reading its claim as
// settled, so an exec_pending claim under a live placement can only be a
// launch this step is still corroborating.
//
// Two of the four conjuncts are defensive rather than discriminating as
// this is called today: every caller sources binding and claim from
// sessionCloseEvidence, which reads Bindings().Current — already
// unsuperseded, since the store selects on it — and then keys the claim by
// that binding's own incarnation. So !Superseded and the incarnation
// equality cannot be false through any current path, and no test can make
// them false without a caller that sources the pair some other way. They
// are kept because this predicate states the whole shape it identifies,
// and the sqlite read model's twin (launchCorroborationPending) rests on
// the same currentBinding property rather than re-deriving it.
func wrapperReconciliation(binding *run.RuntimeBinding, claimFound bool, claim *LaunchClaim) bool {
	return binding.PaneID != "" && !binding.Superseded &&
		claimFound && claim.State == LaunchClaimExecPending && claim.IncarnationID == binding.IncarnationID
}

// driveLaunchingRunFailure drives the feature terminal failure of a
// launching run whose manager lineage ended in an exec failure, through
// RetireSettledSessions — the one routine that retires every live child
// and group, quiesces ref moves and then marks the run failed with stop
// precedence. Anything else is left to the ordinary pass.
func (c *Controller) driveLaunchingRunFailure(ctx context.Context, handle RunHandle) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per scheduling pass.
	due := false
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "CorroborateSessionLaunches")
		if wfErr != nil {
			return wfErr
		}
		r, _, runErr := uow.Runs().Get(ctx, handle.runID)
		if runErr != nil {
			return runErr
		}
		if r.State != run.RunLaunching {
			return nil
		}
		var failedErr error
		due, failedErr = managerLaunchFailedLocked(ctx, uow, wf, handle.runID)
		return failedErr
	}); err != nil {
		return err
	}
	if !due {
		return nil
	}
	if _, err := c.RetireSettledSessions(ctx, handle); err != nil {
		return fmt.Errorf("app: fail the run after the manager's exec failure: %w", err)
	}
	return nil
}

// corroborateSessionLaunch performs one inspection round for one session,
// mirroring CorroborateLaunch's branches against session-keyed context
// (sessionCloseEvidence) instead of RunDetail's single attempt/claim/
// binding fields.
func (c *Controller) corroborateSessionLaunch(ctx context.Context, handle RunHandle, frozen *FrozenRun, session *run.Session) (LaunchProgress, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per session per scheduling pass.
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
		return c.settleSessionExecFailure(ctx, handle, frozen, session, &claim)
	case LaunchClaimExeced:
		// Already settled (an adoption path, or a prior round's settlement
		// whose session activation — or, for the manager, the run's own
		// launching->running consequence — was lost): apply what is still
		// missing, idempotently. No fresh occupant evidence is available
		// here. Settlement happens only against a bound pane, so a
		// settled claim with no binding waits for the label recovery.
		if !bindingFound {
			if recoverErr := c.recoverSessionBindingByLabel(ctx, handle, session, bindingFound, binding); recoverErr != nil {
				return "", recoverErr
			}
			return LaunchPending, nil
		}
		if runErr := c.settleManagerRunRunningIfNeeded(ctx, handle, session); runErr != nil {
			return "", runErr
		}
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
		if bindingFound {
			return LaunchPending, nil
		}
		// The label-only variant of the launch-ended row: a launch whose
		// placement was never recorded, whose creation label answers
		// nothing and whose claimed process is gone, is settled exec_failed.
		current, _, endErr := c.settleIfUnplacedLaunchEnded(ctx, handle, session)
		if endErr != nil {
			return "", endErr
		}
		if current.State != LaunchClaimExecFailed {
			return LaunchPending, nil
		}
		return c.settleSessionExecFailure(ctx, handle, frozen, session, &current)
	}
	pane, err := c.Runtime.InspectPane(ctx, binding.PaneID)
	if errors.Is(err, ErrPaneNotFound) {
		// The one decision row for a placed, unsettled launch whose pane
		// is gone: settled exec_failed only once the corroborated-absence
		// predicate holds, pending otherwise.
		current, _, endErr := c.settleIfLaunchEnded(ctx, handle, &binding, &claim)
		if endErr != nil {
			return "", endErr
		}
		if current.State != LaunchClaimExecFailed {
			return LaunchPending, nil
		}
		return c.settleSessionExecFailure(ctx, handle, frozen, session, &current)
	}
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
		if err := c.markSessionReconciling(ctx, handle, session.ID, TransitionReasonLaunchCorroboration); err != nil {
			return "", err
		}
		return LaunchNeedsInteraction, nil
	case SettlementUnresolved:
		return LaunchPending, nil
	}
	return LaunchPending, nil
}

// settleSessionExecFailure applies the exec_failed row to one session
// whose current launch claim is exec_failed, whether the launcher wrote
// it or the controller settled a launch that ended before corroboration
// (settleIfLaunchEnded). A child settles through settleChildExecFailure:
// docs/plan/phase-2-design.md section 5's Attempt row "launching → failed:
// exec_failed claim, unrecoverable", with the docs/plan/phase-3-design.md
// section 5 budgeted task consequence ("active, checking → needs-rework:
// … exec failure" below the retry limit, "→ failed" at it, interrupted
// under a held stop). The manager has no attempt to settle: its session
// is terminated and the manager-lineage failure cause, read from the same
// exec_failed claim, fails the run.
func (c *Controller) settleSessionExecFailure(ctx context.Context, handle RunHandle, frozen *FrozenRun, session *run.Session, claim *LaunchClaim) (LaunchProgress, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per exec-failed session per round.
	if session.Role != run.RoleManager {
		if err := c.settleChildExecFailure(ctx, handle, frozen, session); err != nil {
			return "", err
		}
		return LaunchFailed, nil
	}
	reason := "exec_failed claim"
	if launchEndedByController(claim) {
		reason = workerLaunchEnded(claim.Error).sessionReason
	}
	if err := c.terminateRetiredSession(ctx, handle, session.ID, reason); err != nil {
		return "", err
	}
	return LaunchFailed, nil
}

// settleSessionExeced records one feature-mode session's launch-claim
// settlement: the run's own launching -> running transition (never
// resuming, ResumeFeature's own gate — see applyManagerRunRunning) when
// session is the manager (design L560 — "launching -> running is the
// manager's settled launch claim"; worker and reviewer sessions launch
// only once AssignReadyTasks/EnsureReviewTask have already observed the
// run running, so applying the check for every role stays idempotent but
// only the manager's own settlement can ever find the run still
// launching), the claim's exec_pending -> execed transition with the
// corroborated occupant's pid and marker as evidence (never an arbitrary
// foreground member), that evidence also recorded on the session's current
// binding, its bound attempt's launching/relaunching -> running transition
// when it has one (the manager has none), and the session's own
// activation — mirroring Phase 2's settleExeced, whose own Run-level
// transition this now applies identically for the manager alone.
func (c *Controller) settleSessionExeced(ctx context.Context, handle RunHandle, session *run.Session, claim *LaunchClaim, occupant *SettlementEvidence) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per session settlement.
	now := c.Clock.Now()
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		if runErr := applyManagerRunRunning(ctx, uow, handle, session, now); runErr != nil {
			return runErr
		}
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

// applyManagerRunRunning moves handle's run from launching to running, in
// uow, when session is the run's manager — the design L560 consequence
// ("launching -> running is the manager's settled launch claim") that
// neither the solo-mode settleExeced's mirror nor any step of the
// feature-mode scheduling pass otherwise applies. Deliberately NEVER
// resuming: resuming->running is ResumeFeature's own gate
// (usecase_featureresume.go, markFeatureRunning) — every session warm,
// relaunched or retired, and no blocked integration operation — and a
// manager settlement alone must never bypass it, unlike solo's
// settleExeced, which may accept a resuming run only because a solo run
// has exactly one session. A no-op for every other role and for a run
// already running, so calling it unconditionally from every settlement
// path stays idempotent.
func applyManagerRunRunning(ctx context.Context, uow UnitOfWork, handle RunHandle, session *run.Session, now time.Time) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per session settlement.
	if session.Role != run.RoleManager {
		return nil
	}
	r, rRev, err := uow.Runs().Get(ctx, handle.runID)
	if err != nil {
		return err
	}
	if r.State != run.RunLaunching {
		return nil
	}
	rFrom := r.State
	next, err := r.MarkRunning(now)
	if err != nil {
		return err
	}
	if _, err := uow.Runs().Save(ctx, next, rRev); err != nil {
		return err
	}
	return recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(rFrom), string(next.State), "manager launch claim settled execed", gen(handle.lease.Generation), now)
}

// settleManagerRunRunningIfNeeded applies applyManagerRunRunning's run
// transition in its own unit of work, for the "claim already execed"
// corroboration path: a prior round settled the claim through some other
// route without ever finding the run launching (for example, a crash
// between committing that settlement and this consequence), so it is
// re-applied here, idempotently, exactly like confirmSessionActive's own
// catch-up role in the same branch.
func (c *Controller) settleManagerRunRunningIfNeeded(ctx context.Context, handle RunHandle, session *run.Session) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per corroboration round.
	if session.Role != run.RoleManager {
		return nil
	}
	now := c.Clock.Now()
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return applyManagerRunRunning(ctx, uow, handle, session, now)
	})
	if err != nil {
		return fmt.Errorf("app: settle manager run running: %w", err)
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
	// A launch the dispatch revalidation refused was never dispatched: it
	// has no launcher to wait for and is never reopened.
	if newest == nil || refusedBeforeDispatch(newest) || !LaunchDeadlineExpired(newest.CreatedAt, c.Clock.Now()) {
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
	claimed := false
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		op, getErr := uow.Operations().Get(ctx, newest.ID)
		if getErr != nil {
			return getErr
		}
		if op.State == OperationReconciling {
			return nil
		}
		var claimErr error
		if claimed, claimErr = launchClaimedLocked(ctx, uow, &op); claimErr != nil || claimed {
			return claimErr
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
	return !claimed, nil
}
