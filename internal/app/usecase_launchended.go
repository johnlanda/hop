package app

import (
	"context"
	"fmt"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// launchEndedReason is the fixed exec_failed settlement reason the
// controller records for an unsettled launch claim whose incarnation is
// observed dead before corroboration (settleIfLaunchEnded). The claim's
// recorded error equal to it is how every later reader tells a
// controller-settled launch death from the launcher's own exec failure.
const launchEndedReason = "launch ended before corroboration: pane absent by id and label; claimed process gone"

// launchEndedUnplacedReason is the label-only variant's fixed exec_failed
// settlement reason (settleIfUnplacedLaunchEnded): the launch's placement
// was never recorded, so absence rests on the creation label alone.
const launchEndedUnplacedReason = "launch ended before its placement was recorded: no pane answers for the creation label; claimed process gone"

// launchEndedByController reports whether claim is exec_failed through
// one of the controller's launch-ended settlements rather than the
// launcher's own error path.
func launchEndedByController(claim *LaunchClaim) bool {
	return claim.State == LaunchClaimExecFailed && (claim.Error == launchEndedReason || claim.Error == launchEndedUnplacedReason)
}

// observeClaimedProcessGone is the process half of the corroborated-absence
// predicate: whether the process a launch claim recorded is gone, as a
// successful observation. The claimed pid leads its own process group for
// its whole life: hop launch execs the harness in place (the pid is kept),
// and Herdr spawns a command pane's process through portable-pty, which
// calls setsid() in the child, so the launcher is a session leader, and a
// session leader can never change its process group (setpgid refuses it).
// The relation is pinned by the executed probe
// test/integration/spike_panevanish_test.go
// (TestSpikeVanishedPaneShapes: shell pid = foreground group id = the
// command pid, listed by the production GroupInspector while alive and
// absent from it after exit). A living claimed process is therefore
// always a member of group claim pid, and a successful listing without
// that pid means the process is gone. A member still carrying the pid (a
// zombie not yet reaped, or a recycled pid that leads a group of its own)
// keeps the process live — the predicate fails closed, never open.
// ambiguous is "" exactly when the observation is conclusive; a missing
// inspector, an uninspectable pid and a failed listing are ambiguous, and
// no detail echoes the inspector's own error text.
func (c *Controller) observeClaimedProcessGone(ctx context.Context, pid int) (gone bool, ambiguous string) {
	if c.Groups == nil {
		return false, "no process-group inspector is configured, so the claimed process cannot be observed gone"
	}
	if pid <= 1 {
		return false, "the launch claim records no inspectable process id"
	}
	members, err := c.Groups.GroupProcesses(ctx, pid)
	if err != nil {
		return false, "the claimed process's group listing failed; absence is never assumed from an inspection error"
	}
	for _, member := range members {
		if member.PID == pid {
			return false, ""
		}
	}
	return true, ""
}

// claimedProcessLiveDetail is the value-free outstanding detail, with the
// human action, for a pane observed absent while the claimed process
// still runs.
func claimedProcessLiveDetail(pid int) string {
	return fmt.Sprintf("the pane is gone but the claimed launch process (pid %d) still runs; end that process, and a later round observes its exit", pid)
}

// observeLaunchEnded applies the corroborated-absence predicate to an
// unsettled launch: binding is the claim's own current incarnation and
// not superseded, the pane is positively absent under the one absence
// rule (observePaneAbsence: absent by id AND by creation label, each a
// successful observation) and the claimed process is gone
// (observeClaimedProcessGone). docs/plan/phase-2-design.md section 5
// defines a worker's exit as exactly this pair: pane absence together
// with the launch claim's process being gone. ended is false whenever any
// conjunct fails, and no conjunct's failure is ever absence; detail names
// what is still unestablished, and is empty for the ordinary launch in
// flight — a pane that still has a foreground occupant.
func (c *Controller) observeLaunchEnded(ctx context.Context, binding *run.RuntimeBinding, claim *LaunchClaim) (ended bool, detail string) {
	switch {
	case claim.State != LaunchClaimExecPending:
		return false, fmt.Sprintf("the launch claim is %s, not exec_pending", claim.State)
	case binding.PaneID == "" || binding.IncarnationID != claim.IncarnationID:
		return false, "the current binding is not the claim's own placed incarnation"
	case binding.Superseded:
		return false, "the claim's binding is superseded; its observations are ignored"
	}
	_, absent, ambiguous := c.observePaneAbsence(ctx, binding.PaneID, binding.CreationLabel)
	switch {
	case ambiguous != "":
		return false, ambiguous
	case !absent:
		return false, ""
	}
	gone, ambiguous := c.observeClaimedProcessGone(ctx, claim.PID)
	switch {
	case ambiguous != "":
		return false, "the pane is absent, but " + ambiguous
	case !gone:
		return false, claimedProcessLiveDetail(claim.PID)
	}
	return true, ""
}

// settleIfLaunchEnded is the decision row for an exec_pending claim whose
// placed pane was observed absent: once observeLaunchEnded holds, the
// incarnation is dead before any corroboration could settle it, and the
// controller settles the claim exec_failed under the lease with the fixed
// launchEndedReason and the pane and pid as evidence. From then on the
// exec_failed row decides everywhere (docs/plan/phase-2-design.md section
// 4: "Claim exec_failed → the incarnation is dead"), which makes the
// settlement idempotent across passes, crashes and resume. The claim and
// binding are re-read inside the settling transaction: a claim some other
// writer already settled is left as it is, and a binding no longer the
// claim's current, unsuperseded incarnation settles nothing. current is
// the claim as it stands after the round; detail names what kept an
// unsettled claim pending.
func (c *Controller) settleIfLaunchEnded(ctx context.Context, handle RunHandle, binding *run.RuntimeBinding, claim *LaunchClaim) (current LaunchClaim, detail string, err error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per unsettled placed launch per round.
	current = *claim
	ended, detail := c.observeLaunchEnded(ctx, binding, claim)
	if !ended {
		return current, detail, nil
	}
	now := c.Clock.Now()
	err = c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		latest, found, getErr := uow.LaunchClaims().Get(ctx, claim.IncarnationID)
		if getErr != nil {
			return getErr
		}
		if !found {
			return fmt.Errorf("app: launch claim of incarnation %s: %w", claim.IncarnationID, ErrNotFound)
		}
		current = latest
		if latest.State != LaunchClaimExecPending {
			return nil
		}
		bound, boundFound, getErr := uow.Bindings().Current(ctx, binding.SessionID)
		if getErr != nil {
			return getErr
		}
		if !boundFound || bound.IncarnationID != claim.IncarnationID || bound.Superseded || bound.PaneID != binding.PaneID {
			detail = "the claim's binding changed before the settlement; nothing settled"
			return nil
		}
		if settleErr := uow.LaunchClaims().Settle(ctx, claim.IncarnationID, LaunchClaimSettlement{
			State: LaunchClaimExecFailed, PaneID: binding.PaneID, PID: latest.PID, Executable: latest.Executable,
			Reason: launchEndedReason, At: now,
		}); settleErr != nil {
			return settleErr
		}
		current.State = LaunchClaimExecFailed
		current.Error = launchEndedReason
		current.SettledAt = now
		return nil
	})
	if err != nil {
		return *claim, "", fmt.Errorf("app: settle ended launch claim: %w", err)
	}
	return current, detail, nil
}

// unplacedLaunchEndedOutcome is the outcome a pane.open operation records
// when the label-only launch-ended row resolves it
// (settleIfUnplacedLaunchEnded): the operation was dispatched — its pane's
// own process wrote the launch claim — and the pane is gone, so it settles
// failed, never refused before dispatch. Its payload is how
// sessionLaunchClaimLocked still finds the settled claim once no
// unresolved intent names the incarnation.
type unplacedLaunchEndedOutcome struct {
	LaunchEnded        bool                   `json:"launch_ended"`
	Dispatched         bool                   `json:"dispatched"`
	PaneAbsentByLabel  bool                   `json:"pane_absent_by_label"`
	ClaimedProcessGone bool                   `json:"claimed_process_gone"`
	IncarnationID      identity.IncarnationID `json:"incarnation_id"`
	PID                int                    `json:"pid"`
	Reason             string                 `json:"reason"`
}

// launchEndedResolution decodes op as a pane.open the label-only row
// resolved for intent's incarnation.
func launchEndedResolution(op *Operation, intent *paneOpenIntent) bool {
	if op.Kind != OpPaneOpen || op.State != OperationFailed {
		return false
	}
	outcome, ok := decodeOperationPayload[unplacedLaunchEndedOutcome](op.Outcome)
	return ok && outcome.LaunchEnded && outcome.Dispatched && outcome.IncarnationID != "" && outcome.IncarnationID == intent.IncarnationID
}

// launchEndedIntentLocked resolves, inside the caller's transaction, the
// incarnation of sessionID's newest pane.open when the label-only row
// resolved it; found is false for any other newest pane.open, or none.
func launchEndedIntentLocked(ctx context.Context, uow UnitOfWork, runID identity.RunID, sessionID identity.SessionID) (identity.IncarnationID, bool, error) {
	ops, err := uow.Operations().ByKind(ctx, runID, OpPaneOpen)
	if err != nil {
		return "", false, err
	}
	for i := range ops { // newest first
		intent, ok := decodeOperationPayload[paneOpenIntent](ops[i].Intent)
		if !ok || intent.SessionID != sessionID {
			continue
		}
		if !launchEndedResolution(&ops[i], &intent) {
			return "", false, nil
		}
		return intent.IncarnationID, true, nil
	}
	return "", false, nil
}

// unplacedLaunchLiveDetail is the value-free outstanding detail, with the
// human action, for an unplaced launch whose creation label answers
// nothing while its claimed process still runs.
func unplacedLaunchLiveDetail(label string, pid int) string {
	return fmt.Sprintf("no pane answers for launch label %s but the claimed launch process (pid %d) still runs; end that process, and a later round observes its exit", label, pid)
}

// observeUnplacedLaunchEnded applies the label-only variant of the
// corroborated-absence predicate to one session's unplaced launch facts:
// no committed binding, exactly one unresolved, decodable, labeled
// pane.open intent naming the session and nothing unusable that might,
// that intent's own exec_pending claim, a successful creation-label lookup
// that finds nothing, and the claimed process gone
// (observeClaimedProcessGone). The label conjunct stands in for the
// missing pane id: a claim exists only once the pane's own process ran
// hop launch, so the pane existed at claim time, and Herdr attaches a
// pane's creation label in the very request that creates it, keeps it for
// the pane's whole life — across a graceful restart too — and drops it
// with the pane (pinned by test/integration/spike_panevanish_test.go:
// TestSpikeVanishedPaneShapes and TestSpikeLabelSurvivesRestart). A pane a
// human relabels while it lives keeps its process, which the process
// conjunct still observes. ended is false whenever any conjunct fails;
// detail names what is still unestablished ("" when the predicate simply
// does not apply, or a pane answers for the label).
func (c *Controller) observeUnplacedLaunchEnded(ctx context.Context, facts *unplacedLaunchFacts) (ended bool, detail string) {
	switch {
	case facts.bound:
		return false, ""
	case len(facts.unusable) > 0:
		return false, "unresolved launch rows with unusable identity"
	case len(facts.ours) != 1:
		return false, ""
	case facts.newest.Kind != OpPaneOpen:
		return false, "the unresolved launch is not a pane.open"
	case !facts.claimed:
		return false, ""
	case facts.claim.State != LaunchClaimExecPending:
		return false, ""
	case facts.claim.IncarnationID != facts.intent.IncarnationID:
		return false, "the launch claim is not the unresolved intent's own incarnation"
	}
	_, found, err := c.Runtime.FindPaneByLabel(ctx, facts.intent.Label)
	switch {
	case err != nil:
		return false, "the pane label lookup failed; absence is never assumed from a lookup error"
	case found:
		return false, ""
	}
	gone, ambiguous := c.observeClaimedProcessGone(ctx, facts.claim.PID)
	switch {
	case ambiguous != "":
		return false, fmt.Sprintf("no pane answers for launch label %s, but %s", facts.intent.Label, ambiguous)
	case !gone:
		return false, unplacedLaunchLiveDetail(facts.intent.Label, facts.claim.PID)
	}
	return true, ""
}

// settleIfUnplacedLaunchEnded is the label-only variant of the
// launch-ended decision row, for an exec_pending claim whose pane.open
// outcome — and so its binding — was never recorded: once
// observeUnplacedLaunchEnded holds, one lease-fenced transaction settles
// the claim exec_failed with the fixed launchEndedUnplacedReason (the pid
// as evidence; no pane id was ever recorded) and resolves the pane.open
// operation failed with unplacedLaunchEndedOutcome, so nothing stays
// unresolved. The facts are re-read inside that transaction and must be
// unchanged — a claim some other writer settled, a binding committed
// meanwhile, or a different unresolved intent settles nothing. From then
// on the exec_failed row decides everywhere, through
// sessionLaunchClaimLocked's fallback to the resolved intent, which makes
// the settlement idempotent across passes, crashes and resume. current is
// the session's claim after the round (exec_failed once settled), and
// detail names what kept an unplaced claim pending.
func (c *Controller) settleIfUnplacedLaunchEnded(ctx context.Context, handle RunHandle, session *run.Session) (current LaunchClaim, detail string, err error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per unplaced session per round.
	var facts unplacedLaunchFacts
	readErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		var factsErr error
		facts, factsErr = readUnplacedLaunchLocked(ctx, uow, handle.runID, session.ID)
		return factsErr
	})
	if readErr != nil {
		return LaunchClaim{}, "", readErr
	}
	current = facts.claim
	ended, detail := c.observeUnplacedLaunchEnded(ctx, &facts)
	if !ended {
		return current, detail, nil
	}
	now := c.Clock.Now()
	err = c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		latest, readErr := readUnplacedLaunchLocked(ctx, uow, handle.runID, session.ID)
		if readErr != nil {
			return readErr
		}
		if latest.bound || len(latest.unusable) > 0 || len(latest.ours) != 1 || latest.newest.ID != facts.newest.ID ||
			!latest.claimed || latest.claim.PID != facts.claim.PID || latest.claim.State != LaunchClaimExecPending {
			current = latest.claim
			detail = "the unplaced launch changed before the settlement; nothing settled"
			return nil
		}
		if settleErr := uow.LaunchClaims().Settle(ctx, latest.claim.IncarnationID, LaunchClaimSettlement{
			State: LaunchClaimExecFailed, PID: latest.claim.PID, Executable: latest.claim.Executable,
			Reason: launchEndedUnplacedReason, At: now,
		}); settleErr != nil {
			return settleErr
		}
		op := latest.newest
		op.State = OperationFailed
		op.Outcome = unplacedLaunchEndedOutcome{
			LaunchEnded: true, Dispatched: true, PaneAbsentByLabel: true, ClaimedProcessGone: true,
			IncarnationID: latest.claim.IncarnationID, PID: latest.claim.PID, Reason: launchEndedUnplacedReason,
		}
		op.UpdatedAt = now
		if saveErr := uow.Operations().Save(ctx, op); saveErr != nil {
			return saveErr
		}
		current = latest.claim
		current.State = LaunchClaimExecFailed
		current.Error = launchEndedUnplacedReason
		current.SettledAt = now
		return nil
	})
	if err != nil {
		return facts.claim, "", fmt.Errorf("app: settle ended unplaced launch claim: %w", err)
	}
	return current, detail, nil
}
