package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// RestartReport is one ReconcileServerRestart round's outcome.
type RestartReport struct {
	// Closed names the sessions whose recorded pane was closed under the
	// restart rule and whose absence was then OBSERVED this round. A
	// dispatched close is never listed here.
	Closed []string
	// Relaunched names the sessions cold-relaunched from their recorded
	// native session reference after that close.
	Relaunched []string
	// ManagerRelaunched is true when the run's manager was one of them:
	// the relaunch a human most needs to know happened, since it moves the
	// run's manager lineage.
	ManagerRelaunched bool
	// Outstanding names sessions a changed server lifetime was observed
	// for and nothing was concluded about — each with the value-free
	// reason and, where one exists, the human action.
	Outstanding []string
}

// RestartOptions parameterizes a relaunch's pane argv and environment, the
// same two values every other launch path takes from its caller: the
// absolute path of the running hop binary and the run's state root.
type RestartOptions struct {
	HOPPath   string
	StateRoot string
}

// restartCloseReasonSuffix is the fixed, value-free tail of every reason
// this rule journals: it names the cause without naming a value, so a
// reader can tell a restart close from a stop's or a retirement's in the
// journal without the journal carrying a token, a pid or a path.
const restartCloseReasonSuffix = "the Herdr server restarted, so the recorded pane's process is gone and the recorded pane was closed"

// Reasons this rule journals. Each is distinct from every other close and
// termination reason so the journal tells them apart.
const (
	// restartSessionReason is the session-termination reason for a session
	// the restart rule closed and did not relaunch.
	restartSessionReason = "server-lifetime change: " + restartCloseReasonSuffix
	// restartRelaunchReason is the predecessor session's termination reason
	// when the rule relaunched it from its recorded native reference.
	restartRelaunchReason = "server-lifetime change: " + restartCloseReasonSuffix + "; the session was relaunched from its recorded native reference"
	// launchEndedRestartReason is the exec_failed settlement reason for a
	// launch the restart ended before it was ever corroborated. It is a
	// THIRD recognized launch-ended reason beside launchEndedReason and
	// launchEndedUnplacedReason, and it is distinct because its evidence is
	// distinct: not "pane and process observed gone under one lifetime" but
	// "the lifetime itself changed, and the pane HOP recorded was closed".
	launchEndedRestartReason = "launch ended at a server restart: the server lifetime changed before the launch was corroborated; the recorded pane was closed"
)

// The fixed, value-free dispositions `hop status` renders for a session
// the restart-lifecycle step acted on. They are DERIVED from the session's
// recorded transition reason rather than stored a second time, so the
// journal stays the single record of what happened.
const (
	// RestartDispositionClosed: the session's pane was closed after a
	// server restart and the session was not relaunched.
	RestartDispositionClosed = "closed after a server restart"
	// RestartDispositionRelaunched: closed, and relaunched from its
	// recorded native session reference.
	RestartDispositionRelaunched = "relaunched after a server restart"
)

// RestartDispositionFor reduces a session's newest recorded transition
// reason to one of the fixed dispositions above, or "" when the restart
// step did not act on it. It is the ONE place the journal's reasons are
// mapped to the status surface, so a reason can never leak into a rendering
// by accident.
func RestartDispositionFor(reason string) string {
	switch reason {
	case restartRelaunchReason:
		return RestartDispositionRelaunched
	case restartSessionReason:
		return RestartDispositionClosed
	default:
		return ""
	}
}

// restartIdentity is how a pane answering a recorded id was identified as
// the session's OWN after a restart. A recorded pane id is not a durable
// address across a restart — a workspace closed before one leaves its id
// free, the restarted server reissues it, and the pane id composed from it
// answers for somebody else's pane (pinned by test/integration's
// TestSpikeRecordedPaneIDCanAddressADifferentPane, which observed exactly
// that collision) — so the id alone never authorizes a close.
type restartIdentity string

// Restart identifications.
const (
	// restartIdentifiedByLabel: the session's creation label, a UUID HOP
	// minted, resolves to exactly the recorded pane id. This is the rung
	// that works in BOTH restore windows, because a label is a layout
	// attribute that needs no terminal runtime.
	restartIdentifiedByLabel restartIdentity = "creation label"
	// restartIdentifiedByHarness: the pane holds exactly one foreground
	// member running the harness's native restore invocation for THIS
	// session's own native reference (MatchRestoredHarness). This is the
	// rung that survives a human's pane.rename, which defeats the label.
	restartIdentifiedByHarness restartIdentity = "restored harness occupant"
	// restartUnidentified: neither rung speaks. Nothing is closed.
	restartUnidentified restartIdentity = ""
)

// restartDisposition is what happens to a session once its pane has been
// closed and its absence observed.
type restartDisposition string

// Restart dispositions.
const (
	// restartRetire: terminate the session and free its slot. A held stop
	// or a terminal failure takes this whatever the session was doing —
	// relaunching an agent into a stopping run would be exactly wrong — and
	// so does a session whose attempt has already settled.
	restartRetire restartDisposition = "retire"
	// restartSettleLaunchFailed: the launch was never corroborated, so the
	// harness never started and its PRE-ASSIGNED native reference names no
	// transcript (test/integration's TestSpikeClaudePreassignedSessionID
	// pins that a --resume of an unknown id is not found). Relaunching
	// would start a harness that immediately fails, so the claim settles
	// exec_failed and the ordinary exec-failure consequences follow.
	restartSettleLaunchFailed restartDisposition = "settle-launch-failed"
	// restartRelaunch: cold relaunch from the recorded native reference.
	restartRelaunch restartDisposition = "relaunch"
	// restartInterrupt: the session's work could continue but this harness
	// cannot be resumed (cold resume is Claude-only, or no native reference
	// was recorded), so the attempt is interrupted and the manager decides.
	restartInterrupt restartDisposition = "interrupt"
)

// ReconcileServerRestart is the section 6 restart-lifecycle step: on a
// CHANGED server lifetime, HOP closes the panes it owns and cold-relaunches
// those sessions from their recorded native session references.
//
// A changed lifetime is POSITIVE evidence, not an absence of evidence: a
// socket path is served by one server at a time and lifetimes are
// contiguous, so a recorded, non-empty token that differs from a fresh,
// non-empty one proves the server behind the placement is gone — and with
// it every pane process it spawned. It is also permanent: the placement's
// own continuity can never be established again, so without a positive act
// the session stays outstanding forever. Without this step stop, feature
// stop and the per-attempt retirement cannot conclude absence after a
// restart at all: the run stays stopping, the slot is never freed and
// observeWorkerExit never fires.
//
// What is closed is not a stray process. Herdr rebuilds a restored pane's
// agent from its own persisted plan about a second and a half after the
// restart, with no client attached and nobody watching
// (test/integration's TestSpikeRestoredPaneCloseDiscardsResume measured
// it), and that agent resumes its own conversation: reading its
// assignment, editing the worktree, running commands, while HOP believes
// the session ended. Closing the pane discards the pending plan at the
// root — the same probe observed no resume for a closed pane across a
// client attach and a second restart that demonstrably re-armed an
// unclosed pane's plan.
//
// The step runs before every path that decides a placed session's absence,
// so those paths need no restart case of their own: after it, a session is
// either closed (and retired or relaunched) or unchanged and outstanding
// with its reason named. Sessions are reconciled children first and the
// MANAGER LAST, so a round that fails partway has not moved the run's
// manager lineage.
//
// Scope: a feature run takes every disposition; a SOLO run takes this step
// only under a held stop or a terminal failure, where the whole action is
// to close and conclude absence. A running solo run keeps today's
// fail-closed behavior and its existing exit, because solo cold relaunch
// is exclusively a `hop resume` recovery action (docs/plan/phase-3-design.md
// section 5) and this step does not amend that. Off darwin no lifetime is
// ever recorded, so the verdict is always unknown and nothing here applies.
func (c *Controller) ReconcileServerRestart(ctx context.Context, handle RunHandle, opts RestartOptions) (RestartReport, error) { //nolint:gocritic // hugeParam: RunHandle and RestartOptions are per-call DTOs; called once per scheduling, stop or resume round.
	detail, err := c.Read.LoadRunStatus(ctx, handle.runID)
	if err != nil {
		return RestartReport{}, fmt.Errorf("app: load run status: %w", err)
	}
	switch detail.State {
	case run.RunStopped, run.RunCompleted, run.RunFailed:
		return RestartReport{}, nil
	}
	frozen, err := c.Read.LoadFrozenRun(ctx, handle.runID)
	if err != nil {
		return RestartReport{}, fmt.Errorf("app: load frozen run: %w", err)
	}
	sessions, err := c.restartCandidateSessions(ctx, handle, &detail)
	if err != nil {
		return RestartReport{}, err
	}

	var report RestartReport
	for i := range sessions {
		session := sessions[i]
		outcome, line, sessErr := c.reconcileRestartedSession(ctx, handle, &frozen, detail, opts, &session)
		if sessErr != nil {
			return report, sessErr
		}
		switch outcome {
		case restartRetire, restartSettleLaunchFailed, restartInterrupt:
			report.Closed = append(report.Closed, session.ID.String())
		case restartRelaunch:
			report.Closed = append(report.Closed, session.ID.String())
			report.Relaunched = append(report.Relaunched, session.ID.String())
			report.ManagerRelaunched = report.ManagerRelaunched || session.Role == run.RoleManager
		default:
			if line != "" {
				report.Outstanding = append(report.Outstanding, fmt.Sprintf("session %s: %s", session.ID, line))
			}
		}
	}
	return report, nil
}

// restartCandidateSessions lists the run's non-terminal sessions in the
// order the step reconciles them: children first (by id), the MANAGER
// LAST. A solo run has no session index to read; its single session is the
// run status's own, and it is a candidate only under a held stop or a
// terminal failure (see ReconcileServerRestart's scope).
func (c *Controller) restartCandidateSessions(ctx context.Context, handle RunHandle, detail *RunDetail) ([]run.Session, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	var sessions []run.Session
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "ReconcileServerRestart")
		if wfErr != nil {
			return wfErr
		}
		all, sessErr := wf.SessionIndex().ByRun(ctx, handle.runID)
		if sessErr != nil {
			return sessErr
		}
		for i := range all {
			if all[i].State == run.SessionTerminated || all[i].State == run.SessionLost || all[i].State == run.SessionReserved {
				continue
			}
			sessions = append(sessions, all[i])
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrWorkflowRepositoriesUnsupported) {
		return nil, err
	}
	if !restartFeatureRun(detail) {
		// Solo: only a stop or a terminal failure, whose whole action is to
		// close and conclude absence, and only the run's own session.
		if !restartStopHeld(detail) {
			return nil, nil
		}
		sessions = soloRestartSession(sessions, detail)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if (sessions[i].Role == run.RoleManager) != (sessions[j].Role == run.RoleManager) {
			return sessions[j].Role == run.RoleManager
		}
		return sessions[i].ID < sessions[j].ID
	})
	return sessions, nil
}

// restartFeatureRun reports whether detail is a feature-mode run.
func restartFeatureRun(detail *RunDetail) bool { return detail.Mode == string(WorkflowModeFeature) }

// restartStopHeld reports whether the run is stopping or holds a stop
// request — the state in which the restart rule never relaunches anything.
func restartStopHeld(detail *RunDetail) bool {
	return detail.StopRequested || detail.State == run.RunStopping
}

// soloRestartSession narrows a solo run's candidates to the one session
// the run status names, which is the only session a solo run has.
func soloRestartSession(sessions []run.Session, detail *RunDetail) []run.Session {
	for i := range sessions {
		if sessions[i].ID == detail.SessionID {
			return sessions[i : i+1]
		}
	}
	return nil
}

// reconcileRestartedSession applies the rule to ONE session. It returns the
// disposition taken, or "" with the value-free reason nothing was
// concluded.
func (c *Controller) reconcileRestartedSession(ctx context.Context, handle RunHandle, frozen *FrozenRun, detail RunDetail, opts RestartOptions, session *run.Session) (restartDisposition, string, error) { //nolint:gocritic // hugeParam: RunHandle, RunDetail and RestartOptions are per-call DTOs; called once per session per round.
	binding, bindingFound, claim, claimFound, markers, err := c.sessionCloseEvidence(ctx, handle, session)
	if err != nil {
		return "", "", err
	}
	if !bindingFound || binding.PaneID == "" || binding.Superseded {
		// No placement of our own to close: the unplaced-launch rules own
		// this session, restart or not.
		return "", "", nil
	}

	// A close this rule already journaled for this placement is re-driven
	// against its persisted target — never re-identified, since the
	// identification was made when the intent was committed and the pane
	// may since have stopped answering precisely because that close worked.
	if op, intent, found := findRestartClose(&detail, binding.PaneID, binding.IncarnationID); found {
		return c.finishRestartClose(ctx, handle, frozen, detail, opts, session, &binding, claimFound, &claim, op, intent)
	}

	observed := c.observeServerInstance(ctx)
	if ClassifyServerLifetime(binding.ServerInstance, observed) != LifetimeChanged {
		// Continuity, or unknown: both are the existing rules' business and
		// neither licenses this act.
		return "", "", nil
	}

	identified, detailLine := c.identifyRestartedPane(ctx, &binding, session)
	if identified == restartUnidentified {
		return "", detailLine, nil
	}
	if pid := restartRecordedPID(claimFound, &claim, &binding); pid > 0 {
		gone, ambiguous := c.observeClaimedProcessGone(ctx, pid)
		switch {
		case ambiguous != "":
			return "", "the server lifetime changed, but " + ambiguous, nil
		case !gone:
			return "", claimedProcessLiveDetail(pid), nil
		}
	}
	// The closing bracket: one lifetime served every observation above, and
	// it is still not the placement's. A restart DURING the observations
	// concludes nothing this round.
	if after := c.observeServerInstance(ctx); after != observed || ClassifyServerLifetime(binding.ServerInstance, after) != LifetimeChanged {
		return "", "the server lifetime changed again while this session was being observed; nothing is concluded from observations that span a restart", nil
	}

	opID, intent, err := c.openRestartClose(ctx, handle, session, &binding, claimFound, &claim, markers, observed, identified)
	if err != nil {
		return "", "", err
	}
	if err := c.dispatchRestartClose(ctx, handle, detail, opID, &intent); err != nil {
		return "", "", err
	}
	return c.finishRestartClose(ctx, handle, frozen, detail, opts, session, &binding, claimFound, &claim, opID, intent)
}

// identifyRestartedPane applies the identification ladder: the pane the
// recorded id answers for is this session's own only when its own creation
// label resolves to exactly that id, or when the pane holds exactly one
// foreground member running the harness's restore invocation for this
// session's own native reference.
//
// The label rung is first deliberately: a label is a layout attribute, so
// it answers whether or not the deferred restore has fired, while an
// occupant can only speak once it has. The harness rung covers what the
// label cannot — a pane a human renamed, which Herdr restores under its
// NEW name.
//
// MatchRestoredHarness's None outcome is NOT suspicious and never
// fail-closed on its own: it is the ordinary answer for a pane whose
// restore has not fired and for any plain cold-restored pane. It means
// only that this rung does not identify.
func (c *Controller) identifyRestartedPane(ctx context.Context, binding *run.RuntimeBinding, session *run.Session) (identified restartIdentity, unidentifiedDetail string) {
	if binding.CreationLabel != "" {
		ref, found, err := c.Runtime.FindPaneByLabel(ctx, binding.CreationLabel)
		switch {
		case err != nil:
			return restartUnidentified, "the server lifetime changed, but the creation-label lookup failed, so the recorded pane is not identified as this session's; nothing is closed"
		case found && ref.PaneID == binding.PaneID:
			return restartIdentifiedByLabel, ""
		case found:
			return restartUnidentified, "the server lifetime changed, and this session's creation label answers for a DIFFERENT pane than the one recorded, so the recorded id is not this session's; nothing is closed"
		}
	}
	pane, err := c.Runtime.InspectPane(ctx, binding.PaneID)
	switch {
	case errors.Is(err, ErrPaneNotFound):
		// THE FIRST OF TWO NOT-FOUNDS, and the one that is never absence.
		// Nothing has vouched for this pane being ours: the id answers
		// nothing and the label found nothing, which is exactly what a pane
		// AWAITING ITS DEFERRED RESTORE looks like — it has no terminal
		// runtime for about a second and a half after a restart — and
		// equally what a renamed one looks like, since the rename took its
		// label. A pane that still exists, and will resume an agent
		// shortly, presents this pair. Concluding absence here is precisely
		// the mistake the continuity conjunct existed to prevent, so this
		// rung identifies nothing and the round ends; once the restore
		// fires, the harness rung speaks and the close proceeds.
		return restartUnidentified, "the server lifetime changed, and neither this session's creation label nor its recorded pane id answers; a pane awaiting its deferred restore answers neither, so nothing is concluded yet — if a pane of this run was renamed, rename it back to " + RenderExternal(binding.CreationLabel)
	case err != nil:
		return restartUnidentified, "the server lifetime changed, but the recorded pane could not be inspected; absence is never assumed from an inspection error"
	}
	outcome, candidates := MatchRestoredHarness(pane, session.Harness, session.NativeSessionRef)
	switch outcome {
	case RestoredHarnessMatched:
		return restartIdentifiedByHarness, ""
	case RestoredHarnessAmbiguous:
		pids := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			pids = append(pids, strconv.Itoa(candidate.PID))
		}
		return restartUnidentified, "the server lifetime changed, and the recorded pane holds more than one member (pids " + strings.Join(pids, ", ") + ") running this session's restore invocation; no single occupant identifies it, so nothing is closed"
	default:
		return restartUnidentified, "the server lifetime changed, and the recorded pane answers for an occupant this session cannot claim: its creation label is not on it and nothing on it is this session's own resumed agent; nothing is closed"
	}
}

// restartRecordedPID is the process the rule must observe gone before it
// concludes anything: the launch claim's, else the corroborated occupant's.
// Zero means the placement recorded no process at all, which happens only
// before `hop launch` wrote its claim — and since that claim is written
// BEFORE the harness is exec'd, a placement with no claim never started an
// agent, so there is no process of ours to outlive the server.
func restartRecordedPID(claimFound bool, claim *LaunchClaim, binding *run.RuntimeBinding) int {
	if claimFound && claim.PID > 0 {
		return claim.PID
	}
	if binding.Occupant != nil {
		return binding.Occupant.PID
	}
	return 0
}

// findRestartClose returns this rule's own unresolved pane.close operation
// for a pane and incarnation, if one exists.
func findRestartClose(detail *RunDetail, paneID string, incarnation identity.IncarnationID) (identity.OperationID, paneCloseIntent, bool) {
	for i := range detail.PendingOperations {
		op := &detail.PendingOperations[i]
		if op.Kind != OpPaneClose {
			continue
		}
		intent, ok := decodeOperationPayload[paneCloseIntent](op.Intent)
		if !ok || intent.Reason != closeReasonRestart {
			continue
		}
		if intent.PaneID == paneID && intent.IncarnationID == incarnation {
			return op.ID, intent, true
		}
	}
	return "", paneCloseIntent{}, false
}

// openRestartClose commits the close intent, freezing the OBSERVING
// lifetime rather than the placement's: the placement's lifetime is gone,
// and absence after this close is decided against the lifetime that serves
// the socket now. The identification is recorded with it, so a later round
// re-drives the close it authorized rather than re-deciding it.
func (c *Controller) openRestartClose(ctx context.Context, handle RunHandle, session *run.Session, binding *run.RuntimeBinding, claimFound bool, claim *LaunchClaim, markers []string, observed string, by restartIdentity) (identity.OperationID, paneCloseIntent, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per restart close.
	opID, err := c.newOperationID()
	if err != nil {
		return "", paneCloseIntent{}, err
	}
	intent := paneCloseIntent{
		PaneID:                 binding.PaneID,
		Label:                  binding.CreationLabel,
		SessionID:              session.ID,
		IncarnationID:          binding.IncarnationID,
		PID:                    restartRecordedPID(claimFound, claim, binding),
		ArgvMarkers:            markers,
		Reason:                 closeReasonRestart,
		ServerInstance:         observed,
		RecordedServerInstance: binding.ServerInstance,
		IdentifiedBy:           string(by),
	}
	now := c.Clock.Now()
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpPaneClose, State: OperationPending, Intent: intent,
			CreatedAt: now, UpdatedAt: now,
		})
	}); err != nil {
		return "", paneCloseIntent{}, fmt.Errorf("app: record restart pane.close intent: %w", err)
	}
	return opID, intent, nil
}

// dispatchRestartClose captures the pane's scrollback (it vanishes with the
// pane), revalidates immediately before the mutation, and closes. A pane
// that answers pane_not_found at the close vanished between the
// identification and the act, which the absence confirmation then settles.
func (c *Controller) dispatchRestartClose(ctx context.Context, handle RunHandle, detail RunDetail, opID identity.OperationID, intent *paneCloseIntent) error { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; called once per restart close.
	if err := c.capturePaneScrollback(ctx, handle, detail, opID, intent.PaneID); err != nil {
		return err
	}
	if err := c.revalidateForDispatch(ctx, handle, true); err != nil {
		return err
	}
	actCtx, release := handle.actContext(ctx)
	closeErr := c.Runtime.ClosePane(actCtx, intent.PaneID)
	release()
	// THE SECOND OF TWO NOT-FOUNDS, and this one is tolerated — for one
	// reason only: IDENTIFICATION ALREADY SUCCEEDED. Something vouched for
	// this pane being this session's before the intent was committed, so a
	// pane that has gone away between then and now went away as ours. That
	// is what makes it different from the not-found the ladder refuses,
	// where nothing had vouched for the pane at all. Absence is still not
	// assumed from it: the confirmation below observes it.
	if closeErr != nil && !errors.Is(closeErr, ErrPaneNotFound) {
		return fmt.Errorf("app: close pane %s after a server restart: %w", intent.PaneID, closeErr)
	}
	return nil
}

// finishRestartClose confirms the close's OBSERVED absence — bracketed
// against the lifetime that answered the close, never the placement's —
// and, once absent, records the outcome, supersedes the binding and applies
// the session's disposition. A dispatched close is never termination.
func (c *Controller) finishRestartClose(ctx context.Context, handle RunHandle, frozen *FrozenRun, detail RunDetail, opts RestartOptions, session *run.Session, binding *run.RuntimeBinding, claimFound bool, claim *LaunchClaim, opID identity.OperationID, intent paneCloseIntent) (restartDisposition, string, error) { //nolint:gocritic // hugeParam: RunHandle, RunDetail, RestartOptions and paneCloseIntent are per-call DTOs; called once per restart close per round.
	_, absent, ambiguous := c.observePlacedPaneAbsence(ctx, intent.ServerInstance, intent.PaneID, intent.Label)
	switch {
	case ambiguous != "":
		return "", "the recorded pane was closed after the server restart, but its absence is not yet established: " + ambiguous, nil
	case !absent:
		return "", "the recorded pane was closed after the server restart; awaiting its observed absence", nil
	}
	target := paneCloseTarget{
		PaneID: intent.PaneID, Label: intent.Label,
		SessionID: intent.SessionID, IncarnationID: intent.IncarnationID,
		PID: intent.PID, Markers: intent.ArgvMarkers, Reason: intent.Reason,
		ServerInstance: intent.ServerInstance,
	}
	if err := c.recordCloseOutcome(ctx, handle, opID, &target, restartCloseReasonSuffix); err != nil {
		return "", "", err
	}
	disposition := restartDispositionFor(&detail, session, claimFound, claim)
	return disposition, "", c.applyRestartDisposition(ctx, handle, frozen, opts, session, binding, disposition)
}

// restartDispositionFor decides what becomes of a session whose pane the
// restart rule has closed.
func restartDispositionFor(detail *RunDetail, session *run.Session, claimFound bool, claim *LaunchClaim) restartDisposition {
	switch {
	case restartStopHeld(detail) || !restartFeatureRun(detail):
		// A stopping run never relaunches, and a solo run reaches this step
		// only under a stop.
		return restartRetire
	case !claimFound || claim.State != LaunchClaimExeced:
		return restartSettleLaunchFailed
	case session.Harness == run.HarnessClaude && session.NativeSessionRef != "":
		return restartRelaunch
	default:
		return restartInterrupt
	}
}

// applyRestartDisposition carries out one disposition through the SAME
// settlement machinery every other cause uses: no new transition, no second
// relaunch path.
func (c *Controller) applyRestartDisposition(ctx context.Context, handle RunHandle, frozen *FrozenRun, opts RestartOptions, session *run.Session, binding *run.RuntimeBinding, disposition restartDisposition) error { //nolint:gocritic // hugeParam: RunHandle and RestartOptions are per-call DTOs; called once per closed session.
	switch disposition {
	case restartRetire:
		return c.terminateRetiredSession(ctx, handle, session.ID, restartSessionReason)
	case restartSettleLaunchFailed:
		if err := c.settleRestartLaunchEnded(ctx, handle, binding); err != nil {
			return err
		}
		if session.Role == run.RoleManager {
			return c.terminateRetiredSession(ctx, handle, session.ID, restartSessionReason)
		}
		// The cause is passed rather than rediscovered: this rule settled
		// the claim itself and has already superseded the binding the
		// rediscovery would read it through.
		ended := workerLaunchEnded(launchEndedRestartReason)
		return c.settleChildExecFailure(ctx, handle, frozen, session, &ended)
	case restartInterrupt:
		if session.Role == run.RoleManager {
			return c.terminateRetiredSession(ctx, handle, session.ID, restartSessionReason)
		}
		return c.settleWorkerTermination(ctx, handle, frozen, session, restartInterruption())
	default: // restartRelaunch
		return c.coldRelaunchFeatureSession(ctx, handle, frozen, &ResumeFeatureRequest{
			RunID: handle.runID.String(), HOPPath: opts.HOPPath, StateRoot: opts.StateRoot,
		}, session, binding, restartRelaunchReason)
	}
}

// restartInterruption is the terminal attempt outcome for a session the
// restart ended and HOP cannot resume: the attempt is interrupted, the
// manager is notified, and retrying is the manager's judgment — the
// ordinary interruption settlement, under this cause's own reasons.
func restartInterruption() workerTermination {
	return workerTermination{
		kind:          "interruption",
		reason:        "the Herdr server restarted and this session's harness cannot be resumed",
		sessionReason: restartSessionReason + "; the harness has no cold resume",
		noticeReason: func(taskConsequence) string {
			return "the Herdr server restarted, ending this attempt's agent; the harness has no cold resume, so the attempt was interrupted"
		},
	}
}

// settleRestartLaunchEnded settles an unsettled launch claim exec_failed
// under this rule's own reason, so every later reader — the child's
// settlement, the manager-lineage failure cause, stop and retirement —
// decides from the same durable evidence. A claim another writer already
// settled is left as it is.
func (c *Controller) settleRestartLaunchEnded(ctx context.Context, handle RunHandle, binding *run.RuntimeBinding) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per settled launch.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		latest, found, getErr := uow.LaunchClaims().Get(ctx, binding.IncarnationID)
		if getErr != nil || !found {
			return getErr
		}
		if latest.State != LaunchClaimExecPending {
			return nil
		}
		return uow.LaunchClaims().Settle(ctx, binding.IncarnationID, LaunchClaimSettlement{
			State: LaunchClaimExecFailed, PaneID: binding.PaneID, PID: latest.PID,
			Executable: latest.Executable, Reason: launchEndedRestartReason, At: now,
		})
	})
}
