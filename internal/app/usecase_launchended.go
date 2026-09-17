package app

import (
	"context"
	"fmt"

	"github.com/johnlanda/hop/internal/domain/run"
)

// launchEndedReason is the fixed exec_failed settlement reason the
// controller records for an unsettled launch claim whose incarnation is
// observed dead before corroboration (settleIfLaunchEnded). The claim's
// recorded error equal to it is how every later reader tells a
// controller-settled launch death from the launcher's own exec failure.
const launchEndedReason = "launch ended before corroboration: pane absent by id and label; claimed process gone"

// launchEndedByController reports whether claim is exec_failed through
// the controller's launch-ended settlement rather than the launcher's own
// error path.
func launchEndedByController(claim *LaunchClaim) bool {
	return claim.State == LaunchClaimExecFailed && claim.Error == launchEndedReason
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
