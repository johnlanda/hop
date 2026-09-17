package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// unplacedLaunch is resolveUnplacedLaunch's verdict for a session with no
// recorded placement.
type unplacedLaunch int

const (
	// unplacedNothingLive: no unresolved launch names the session, or its
	// launch claim settled exec_failed — nothing live to retire.
	unplacedNothingLive unplacedLaunch = iota
	// unplacedBound: the launched pane was recovered by its creation label
	// and its binding committed; the caller retires it through the bound
	// close path.
	unplacedBound
	// unplacedOutstanding: the launch may still be live and cannot be
	// retired this round; the detail names why.
	unplacedOutstanding
)

// unplacedLaunchFacts is one session's unresolved launch journal, read in
// one unit of work: every unresolved pane.open or launch.send intent
// naming it, the newest of them with that intent's own claim, the
// unresolved launch rows whose identity is unusable (they could name any
// session), and whether the session has a current binding.
type unplacedLaunchFacts struct {
	ours      []identity.OperationID
	newest    Operation
	intent    paneOpenIntent
	hasNewest bool
	unusable  []string
	claim     LaunchClaim
	claimed   bool
	bound     bool
}

// readUnplacedLaunchLocked reads sessionID's unplacedLaunchFacts inside
// the caller's transaction.
func readUnplacedLaunchLocked(ctx context.Context, uow UnitOfWork, runID identity.RunID, sessionID identity.SessionID) (unplacedLaunchFacts, error) {
	var facts unplacedLaunchFacts
	_, bound, err := uow.Bindings().Current(ctx, sessionID)
	if err != nil {
		return facts, err
	}
	facts.bound = bound
	pending, err := uow.Operations().Pending(ctx, runID)
	if err != nil {
		return facts, err
	}
	for i := range pending {
		if pending[i].Kind != OpPaneOpen && pending[i].Kind != OpLaunchSend {
			continue
		}
		decoded, ok := decodeOperationPayload[paneOpenIntent](pending[i].Intent)
		switch {
		case !ok:
			facts.unusable = append(facts.unusable, fmt.Sprintf("launch operation %s has an undecodable intent", pending[i].ID))
			continue
		case decoded.SessionID != sessionID:
			continue
		case decoded.Label == "":
			facts.unusable = append(facts.unusable, fmt.Sprintf("launch operation %s has a label-less intent", pending[i].ID))
			continue
		}
		facts.ours = append(facts.ours, pending[i].ID)
		if !facts.hasNewest || pending[i].CreatedAt.After(facts.newest.CreatedAt) {
			facts.newest, facts.intent, facts.hasNewest = pending[i], decoded, true
		}
	}
	if !facts.hasNewest {
		return facts, nil
	}
	facts.claim, facts.claimed, err = uow.LaunchClaims().Get(ctx, facts.intent.IncarnationID)
	return facts, err
}

// resolveUnplacedLaunch is Phase 2's retireUnboundLaunch rule, keyed to
// one session (feature mode runs several launches at once): the
// session's unresolved pane.open (or launch.send) intent and the launch
// claim of its incarnation decide. An undecodable intent — which could
// name any session — or a label-less one naming this session is
// outstanding, never skipped; so is more than one unresolved launch for
// the session, and an unclaimed launch (the pre-claim window may still be
// in flight). A claimed launch recovers its pane by the creation label
// through the session-keyed label recovery (recoverSessionBindingByLabel);
// a failed or ambiguous lookup stays outstanding, never treated as
// absence. A successful lookup that finds nothing hands an exec_pending
// claim to the label-only launch-ended row (settleIfUnplacedLaunchEnded):
// settled, the launch has nothing live; otherwise — the claimed process
// still runs, its observation is ambiguous, or server continuity since the
// launch is not established — it stays outstanding with the reason and
// the human action named.
func (c *Controller) resolveUnplacedLaunch(ctx context.Context, handle RunHandle, session *run.Session) (unplacedLaunch, string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per unplaced session per round.
	var facts unplacedLaunchFacts
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		var readErr error
		facts, readErr = readUnplacedLaunchLocked(ctx, uow, handle.runID, session.ID)
		return readErr
	})
	if err != nil {
		return unplacedOutstanding, "", err
	}
	switch {
	case len(facts.unusable) > 0:
		return unplacedOutstanding, "unresolved launch rows with unusable identity; failing closed: " + strings.Join(facts.unusable, "; "), nil
	case len(facts.ours) == 0:
		return unplacedNothingLive, "", nil
	case len(facts.ours) > 1:
		return unplacedOutstanding, fmt.Sprintf("%d unresolved launch operations name the session; the newest alone cannot prove the others started nothing — failing closed", len(facts.ours)), nil
	case !facts.claimed:
		return unplacedOutstanding, fmt.Sprintf("launch operation %s is unresolved with no claim; the launch may still be in flight — failing closed", facts.newest.ID), nil
	case facts.claim.State == LaunchClaimExecFailed:
		return unplacedNothingLive, "", nil
	}

	_, found, lookupErr := c.Runtime.FindPaneByLabel(ctx, facts.intent.Label)
	switch {
	case lookupErr != nil:
		return unplacedOutstanding, fmt.Sprintf("the pane label lookup for launch operation %s failed or was ambiguous; failing closed", facts.newest.ID), nil
	case !found:
		current, still, settleErr := c.settleIfUnplacedLaunchEnded(ctx, handle, session)
		if settleErr != nil {
			return unplacedOutstanding, "", settleErr
		}
		if current.State == LaunchClaimExecFailed {
			return unplacedNothingLive, "", nil
		}
		if still == "" {
			still = fmt.Sprintf("no pane answers for launch label %s while claim pid %d is unobserved", RenderExternal(facts.intent.Label), facts.claim.PID)
		}
		return unplacedOutstanding, still + "; failing closed", nil
	}
	if err := c.recoverSessionBindingByLabel(ctx, handle, session, false, run.RuntimeBinding{}); err != nil {
		return unplacedOutstanding, "", err
	}
	var bound bool
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		binding, found, getErr := uow.Bindings().Current(ctx, session.ID)
		bound = found && binding.PaneID != "" && binding.IncarnationID == facts.intent.IncarnationID
		return getErr
	}); err != nil {
		return unplacedOutstanding, "", err
	}
	if !bound {
		return unplacedOutstanding, fmt.Sprintf("the pane for launch label %s could not be bound; failing closed", RenderExternal(facts.intent.Label)), nil
	}
	return unplacedBound, "", nil
}
