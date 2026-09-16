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

// resolveUnplacedLaunch is Phase 2's retireUnboundLaunch rule, keyed to
// one session (feature mode runs several launches at once): the
// session's unresolved pane.open (or launch.send) intent and the launch
// claim of its incarnation decide. An undecodable intent — which could
// name any session — or a label-less one naming this session is
// outstanding, never skipped; so is more than one unresolved launch for
// the session, and an unclaimed launch (the pre-claim window may still be
// in flight). A claimed launch recovers its pane by the creation label
// through the session-keyed label recovery (recoverSessionBindingByLabel);
// a failed or ambiguous lookup, or a claimed process with no pane
// answering for the label, stays outstanding — never treated as absence.
func (c *Controller) resolveUnplacedLaunch(ctx context.Context, handle RunHandle, session *run.Session) (unplacedLaunch, string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per unplaced session per round.
	var (
		ours      []identity.OperationID
		newest    Operation
		intent    paneOpenIntent
		unusable  []string
		claim     LaunchClaim
		claimed   bool
		hasNewest bool
	)
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		pending, pendErr := uow.Operations().Pending(ctx, handle.runID)
		if pendErr != nil {
			return pendErr
		}
		for i := range pending {
			if pending[i].Kind != OpPaneOpen && pending[i].Kind != OpLaunchSend {
				continue
			}
			decoded, ok := decodeOperationPayload[paneOpenIntent](pending[i].Intent)
			switch {
			case !ok:
				unusable = append(unusable, fmt.Sprintf("launch operation %s has an undecodable intent", pending[i].ID))
				continue
			case decoded.SessionID != session.ID:
				continue
			case decoded.Label == "":
				unusable = append(unusable, fmt.Sprintf("launch operation %s has a label-less intent", pending[i].ID))
				continue
			}
			ours = append(ours, pending[i].ID)
			if !hasNewest || pending[i].CreatedAt.After(newest.CreatedAt) {
				newest, intent, hasNewest = pending[i], decoded, true
			}
		}
		if !hasNewest {
			return nil
		}
		var getErr error
		claim, claimed, getErr = uow.LaunchClaims().Get(ctx, intent.IncarnationID)
		return getErr
	})
	if err != nil {
		return unplacedOutstanding, "", err
	}
	switch {
	case len(unusable) > 0:
		return unplacedOutstanding, "unresolved launch rows with unusable identity; failing closed: " + strings.Join(unusable, "; "), nil
	case len(ours) == 0:
		return unplacedNothingLive, "", nil
	case len(ours) > 1:
		return unplacedOutstanding, fmt.Sprintf("%d unresolved launch operations name the session; the newest alone cannot prove the others started nothing — failing closed", len(ours)), nil
	case !claimed:
		return unplacedOutstanding, fmt.Sprintf("launch operation %s is unresolved with no claim; the launch may still be in flight — failing closed", newest.ID), nil
	case claim.State == LaunchClaimExecFailed:
		return unplacedNothingLive, "", nil
	}

	_, found, lookupErr := c.Runtime.FindPaneByLabel(ctx, intent.Label)
	switch {
	case lookupErr != nil:
		return unplacedOutstanding, fmt.Sprintf("the pane label lookup for launch operation %s failed or was ambiguous; failing closed", newest.ID), nil
	case !found:
		return unplacedOutstanding, fmt.Sprintf("no pane answers for launch label %s while claim pid %d is unobserved; failing closed", intent.Label, claim.PID), nil
	}
	if err := c.recoverSessionBindingByLabel(ctx, handle, session, false, run.RuntimeBinding{}); err != nil {
		return unplacedOutstanding, "", err
	}
	var bound bool
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		binding, found, getErr := uow.Bindings().Current(ctx, session.ID)
		bound = found && binding.PaneID != "" && binding.IncarnationID == intent.IncarnationID
		return getErr
	}); err != nil {
		return unplacedOutstanding, "", err
	}
	if !bound {
		return unplacedOutstanding, fmt.Sprintf("the pane for launch label %s could not be bound; failing closed", intent.Label), nil
	}
	return unplacedBound, "", nil
}
