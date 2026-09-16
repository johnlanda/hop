package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// Attempt launch dispositions (AttemptLaunchCondition.Disposition).
const (
	// AttemptLaunchOpened: the attempt's worktree exists and its pane was
	// opened this round with a fresh incarnation; launch corroboration
	// continues from here.
	AttemptLaunchOpened = "opened"
	// AttemptLaunchSettled: the attempt settled as a terminal launch
	// failure — attempt failed, the budgeted task consequence, the manager
	// notice, the session terminated — which frees its slot.
	AttemptLaunchSettled = "settled"
	// AttemptLaunchWaiting: the attempt's worktree creation may still be in
	// flight; the bounded wait anchored at the operation's creation runs.
	AttemptLaunchWaiting = "waiting"
	// AttemptLaunchReconciling: the launch cannot advance this round;
	// nothing was dispatched. Detail names what a later round or a human
	// resolves.
	AttemptLaunchReconciling = "reconciling"
	// AttemptLaunchStopRequested: a held stop refused the next act; stop
	// handling resolves what the round left unresolved.
	AttemptLaunchStopRequested = "stop-requested"
)

// AttemptLaunchCondition is one feature attempt launch a round advanced or
// left unresolved: an assigned attempt between its assignment commit and
// its pane.open intent. Detail is fixed text naming the condition and the
// action that resolves it — never a path, an environment value or an
// adapter error; the raw cause stays in the operation journal.
type AttemptLaunchCondition struct {
	TaskSeq       int
	AttemptNumber int
	AttemptID     string
	SessionID     string
	Disposition   string
	Detail        string
}

// Per-attempt worktree.create condition tokens: the persisted outcome of
// an operation left reconciling or failed (attemptWorktreeCondition), so
// the status surface can name the human action without reading a cause.
const (
	worktreeConditionActError   = "act-error"
	worktreeConditionUnverified = "provenance-unverified"
	worktreeConditionListing    = "listing-unavailable"
	worktreeConditionIntent     = "intent-unreadable"
	worktreeConditionNoCheckout = "no-checkout"
	worktreeConditionUnrelated  = "unrelated-checkout"
)

// attemptWorktreeCondition is the outcome payload of a per-attempt
// worktree.create left reconciling or settled failed: a fixed condition
// token and the raw cause, which may name paths and stays in the journal.
type attemptWorktreeCondition struct {
	Condition string `json:"condition"`
	Cause     string `json:"cause,omitempty"`
}

// attemptWorktreeOutcome is createAttemptWorktree's disposition of its own
// operation.
type attemptWorktreeOutcome int

const (
	// attemptWorktreeCreated: provenance validated, row and outcome
	// recorded.
	attemptWorktreeCreated attemptWorktreeOutcome = iota
	// attemptWorktreeUnresolved: the operation is reconciling (an act error,
	// or provenance absent or ambiguous); recovery owns it.
	attemptWorktreeUnresolved
	// attemptWorktreeFailed: the created checkout is unrelated; the
	// operation settled failed.
	attemptWorktreeFailed
	// attemptWorktreeStopRequested: a held stop refused the dispatch; the
	// intent stays pending for stop handling.
	attemptWorktreeStopRequested
)

// paneOpenOutcome is openChildPane's disposition of its own operation.
type paneOpenOutcome int

const (
	// paneOpened: the pane and its binding are recorded.
	paneOpened paneOpenOutcome = iota
	// paneOpenUnresolved: the act failed and no pane answered for the label;
	// the operation is reconciling and launch corroboration recovers it by
	// label and deadline.
	paneOpenUnresolved
	// paneOpenStopRequested: a held stop refused the dispatch.
	paneOpenStopRequested
)

// attemptLaunchTarget is one launching child attempt whose launch has not
// reached its pane.open intent.
type attemptLaunchTarget struct {
	runSeq  int
	task    run.Task
	attempt run.Attempt
	session run.Session
	// facts are the assignment artifact's inputs the assignment
	// transaction already gathered; nil means a recovery round reads them.
	facts *attemptAssignmentFacts
}

// attemptAssignmentFacts are what an attempt's assignment artifact renders
// beyond the frozen run: a retry attempt's prior-attempt feedback and a
// review task's diff-scope base.
type attemptAssignmentFacts struct {
	prior      *priorAttemptFeedback
	reviewBase string
}

// branch is the attempt's worktree branch: one scheme for implement and
// review tasks (design section 6).
func (t *attemptLaunchTarget) branch() string {
	return fmt.Sprintf("hop/r%d/t%da%d", t.runSeq, t.task.Seq, t.attempt.Number)
}

// report renders one condition for the target.
func (t *attemptLaunchTarget) report(disposition, detail string) AttemptLaunchCondition {
	return AttemptLaunchCondition{
		TaskSeq: t.task.Seq, AttemptNumber: t.attempt.Number,
		AttemptID: t.attempt.ID.String(), SessionID: t.session.ID.String(),
		Disposition: disposition, Detail: detail,
	}
}

// attemptLaunchInputs are the run-fixed values a launch needs beyond its
// target.
type attemptLaunchInputs struct {
	frozen    *FrozenRun
	hopPath   string
	stateRoot string
	// implementerBase resolves the base a fresh implementer worktree starts
	// from — the current integration head — only when a create is due.
	implementerBase func(context.Context) (string, error)
}

// attemptLaunchMode selects how far a recovery round may drive.
type attemptLaunchMode int

const (
	// attemptLaunchContinue adopts, settles, re-drives and continues to the
	// pane: the scheduling pass and resume.
	attemptLaunchContinue attemptLaunchMode = iota
	// attemptLaunchResolveOnly adopts and records, or settles failed after
	// the bounded wait, and nothing more — no create, no assignment write,
	// no pane: stop and the terminal-failure settlement.
	attemptLaunchResolveOnly
)

// attemptLaunchRound is one recovery round's result.
type attemptLaunchRound struct {
	conditions []AttemptLaunchCondition
	// outstanding names every per-attempt worktree.create operation still
	// unresolved, in fixed text: stop and terminal failure never reach
	// their terminal transaction while one stands.
	outstanding []string
	// blocked names unresolved worktree.create operations whose intent
	// cannot be attributed to an attempt; nothing settles them
	// automatically.
	blocked []string
	// opened are the sessions whose pane this round opened.
	opened map[identity.SessionID]bool
	// unresolved maps a session whose launch this round left unresolved to
	// its condition.
	unresolved map[identity.SessionID]AttemptLaunchCondition
}

// decodeAttemptWorktreeIntent reads a per-attempt worktree.create intent,
// requiring every field recovery acts on: the frozen repository root, the
// branch, the base and the owning attempt. The Phase 2 solo shape carries
// no attempt and is refused here.
func decodeAttemptWorktreeIntent(op *Operation, repositoryRoot string) (attemptWorktreeCreateIntent, bool) {
	intent, ok := decodeOperationPayload[attemptWorktreeCreateIntent](op.Intent)
	if !ok || intent.Branch == "" || intent.BaseRef == "" || intent.RepositoryRoot != repositoryRoot {
		return attemptWorktreeCreateIntent{}, false
	}
	if _, err := identity.ParseAttemptID(intent.AttemptID.String()); err != nil {
		return attemptWorktreeCreateIntent{}, false
	}
	return intent, true
}

// recoverAttemptLaunches is the feature-mode recovery of per-attempt
// launches a lost or failing act left between the assignment commit and
// the pane.open intent (design section 4, the per-attempt worktree.create
// row). Every unresolved worktree.create is resolved first, in both modes:
// a row already linked to the attempt settles it; otherwise the intended
// repository's worktree listing is searched for the intent's branch — a
// found checkout is adopted only by provenance against the intent's base
// (the row created with run.NewAttemptWorktree, the workspace taken from
// the act's recorded response or recovered by the operation's creation
// label), an unrelated one settles the operation failed, an unverifiable
// one stays reconciling; no checkout within the bounded wait anchored at
// the operation's creation stays in flight, past it the operation settles
// failed. Never a second create while one may be in flight.
//
// In continue mode each launching attempt with no binding and no launch
// intent then advances: an existing row continues the launch (assignment
// artifact, then the pane with a fresh incarnation, only in a recorded
// workspace); a failed operation settles the attempt as a terminal launch
// failure; no operation at all re-drives the create (refusing an existing
// branch); an unresolved operation waits.
func (c *Controller) recoverAttemptLaunches(ctx context.Context, handle RunHandle, mode attemptLaunchMode, in *attemptLaunchInputs) (attemptLaunchRound, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per round.
	round := attemptLaunchRound{
		opened:     map[identity.SessionID]bool{},
		unresolved: map[identity.SessionID]AttemptLaunchCondition{},
	}
	var pending []Operation
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		ops, err := uow.Operations().Pending(ctx, handle.runID)
		for i := range ops {
			if ops[i].Kind == OpWorktreeCreate {
				pending = append(pending, ops[i])
			}
		}
		return err
	}); err != nil {
		return round, err
	}

	byAttempt := map[identity.AttemptID]string{}
	for i := range pending {
		op := &pending[i]
		intent, ok := decodeAttemptWorktreeIntent(op, in.frozen.RepositoryRoot)
		if !ok {
			if err := c.markAttemptWorktreeReconciling(ctx, handle, op.ID, attemptWorktreeCondition{Condition: worktreeConditionIntent}); err != nil {
				return round, err
			}
			still := fmt.Sprintf("operation %s (worktree.create) has an intent naming no attempt of this run; it is never settled automatically", op.ID)
			round.blocked = append(round.blocked, still)
			round.outstanding = append(round.outstanding, still)
			continue
		}
		still, err := c.resolveAttemptWorktreeCreate(ctx, handle, op, &intent)
		if err != nil {
			return round, err
		}
		if still != "" {
			byAttempt[intent.AttemptID] = still
			round.outstanding = append(round.outstanding, fmt.Sprintf("operation %s (worktree.create, branch %s): %s", op.ID, intent.Branch, still))
		}
	}
	if mode == attemptLaunchResolveOnly {
		return round, nil
	}

	targets, launchBlocked, err := c.unresolvedAttemptLaunches(ctx, handle)
	if err != nil {
		return round, err
	}
	states := make([]attemptLaunchState, len(targets))
	for i := range targets {
		if states[i], err = c.readAttemptLaunchState(ctx, handle, &targets[i]); err != nil {
			return round, err
		}
	}
	// Settlements run before any launch step: a task one of them fails
	// becomes a terminal-failure cause that suppresses every launch after
	// it in this round, whatever the targets' order.
	order := make([]int, len(targets))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return states[order[a]].settles() && !states[order[b]].settles() })
	for _, i := range order {
		target := &targets[i]
		var condition AttemptLaunchCondition
		switch {
		case launchBlocked != "":
			condition = target.report(AttemptLaunchReconciling, launchBlocked)
		case byAttempt[target.attempt.ID] != "":
			disposition := AttemptLaunchReconciling
			if byAttempt[target.attempt.ID] == worktreeInFlightDetail {
				disposition = AttemptLaunchWaiting
			}
			condition = target.report(disposition, byAttempt[target.attempt.ID])
		default:
			disposition, detail, suppressErr := c.launchSuppression(ctx, handle)
			if suppressErr != nil {
				return round, suppressErr
			}
			if disposition != "" {
				condition = target.report(disposition, detail)
				break
			}
			condition, err = c.advanceAttemptLaunch(ctx, handle, in, target, &states[i], len(round.blocked) > 0)
			if err != nil {
				return round, err
			}
		}
		round.conditions = append(round.conditions, condition)
		switch condition.Disposition {
		case AttemptLaunchOpened:
			round.opened[target.session.ID] = true
		case AttemptLaunchSettled:
		default:
			round.unresolved[target.session.ID] = condition
		}
	}
	return round, nil
}

// worktreeInFlightDetail is the condition detail while a worktree creation
// may still be in flight.
const worktreeInFlightDetail = "worktree creation may still be in flight; it settles once the bounded wait has passed"

// resolveAttemptWorktreeCreate resolves one unresolved per-attempt
// worktree.create operation, returning "" once it is settled (adopted or
// failed) or the fixed-text reason it stays unresolved.
func (c *Controller) resolveAttemptWorktreeCreate(ctx context.Context, handle RunHandle, op *Operation, intent *attemptWorktreeCreateIntent) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per unresolved operation.
	adopted := false
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, err := RequireWorkflowRepositories(uow, "worktree.create recovery")
		if err != nil {
			return err
		}
		_, _, err = wf.WorktreeIndex().ByAttempt(ctx, intent.AttemptID)
		switch {
		case err == nil:
			adopted = true
			return nil
		case errors.Is(err, ErrNotFound):
			return nil
		default:
			return err
		}
	}); err != nil {
		return "", err
	}
	if adopted {
		return "", c.settleOperation(ctx, handle, op.ID, OperationSucceeded, "worktree already adopted")
	}

	listing, status := c.gitOutput(ctx, intent.RepositoryRoot, "worktree", "list", "--porcelain")
	if status != gitOK {
		if err := c.markAttemptWorktreeReconciling(ctx, handle, op.ID, attemptWorktreeCondition{Condition: worktreeConditionListing}); err != nil {
			return "", err
		}
		return "the repository's worktree listing could not be read; fix the repository's git state, then rerun", nil
	}
	candidate, found := worktreeForBranch(listing, intent.Branch)
	if !found {
		if c.Clock.Now().Sub(op.CreatedAt) <= worktreeRecoveryDeadline {
			// The act's own recorded response (an unverifiable create) is never
			// overwritten: the deadline is enforced against the intent's
			// durable creation time either way.
			if op.ActEvidence == nil {
				if err := c.recordBoundedWait(ctx, handle, op, worktreeRecoveryDeadline, "no checkout for the intended branch yet; creation may be in flight"); err != nil {
					return "", err
				}
			}
			return worktreeInFlightDetail, nil
		}
		return "", c.settleAttemptWorktree(ctx, handle, op.ID, OperationFailed, attemptWorktreeCondition{
			Condition: worktreeConditionNoCheckout,
			Cause:     "no checkout for the intended branch surfaced within the bounded wait",
		})
	}

	provenance, detail := c.classifyWorktreeProvenance(ctx, candidate, intent.RepositoryRoot, intent.BaseRef)
	switch provenance {
	case worktreeValid:
		return "", c.adoptAttemptWorktree(ctx, handle, op, intent, candidate)
	case worktreeUnrelated:
		return "", c.settleAttemptWorktree(ctx, handle, op.ID, OperationFailed, attemptWorktreeCondition{Condition: worktreeConditionUnrelated, Cause: detail})
	default:
		if err := c.markAttemptWorktreeReconciling(ctx, handle, op.ID, attemptWorktreeCondition{Condition: worktreeConditionUnverified, Cause: detail}); err != nil {
			return "", err
		}
		return worktreeUnverifiedDetail, nil
	}
}

// worktreeUnverifiedDetail is the condition detail for a checkout of the
// attempt's branch whose provenance cannot be established.
const worktreeUnverifiedDetail = "the checkout of the attempt's branch could not be verified; inspect it (git worktree list), repair or remove it, then rerun"

// adoptAttemptWorktree records a provenance-valid checkout: the row linked
// to its attempt and the intent's base, and the operation succeeded with
// the placement a pane may join — the workspace the act's own response
// recorded for this branch, else the workspace the operation's creation
// label still names (S9's per-call label round trip, pinned through the
// adapter by TestRealProcessHerdrAdapterWorkspaceAndWorktreeLabels). An
// unlabeled intent, or a label that resolves nothing, records no workspace.
func (c *Controller) adoptAttemptWorktree(ctx context.Context, handle RunHandle, op *Operation, intent *attemptWorktreeCreateIntent, candidate string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per adoption.
	workspace := ""
	if recorded, ok := decodeOperationPayload[worktreeCreateOutcome](op.ActEvidence); ok && recorded.Info.Branch == intent.Branch {
		workspace = recorded.Info.WorkspaceID
	}
	if workspace == "" && intent.Label != "" && c.Workspaces != nil {
		if ref, found, err := c.Workspaces.FindWorkspaceByLabel(ctx, intent.Label); err == nil && found {
			workspace = ref.WorkspaceID
		}
	}
	worktreeID, err := identity.ParseWorktreeID(c.IDs.NewID())
	if err != nil {
		return fmt.Errorf("app: generate worktree id: %w", err)
	}
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "worktree.create adoption")
		if wfErr != nil {
			return wfErr
		}
		latest, getErr := uow.Operations().Get(ctx, op.ID)
		if getErr != nil {
			return getErr
		}
		if latest.State != OperationPending && latest.State != OperationReconciling {
			return nil
		}
		if _, _, existsErr := wf.WorktreeIndex().ByAttempt(ctx, intent.AttemptID); existsErr == nil {
			return fmt.Errorf("app: attempt %s gained a worktree row during adoption; failing closed", intent.AttemptID)
		} else if !errors.Is(existsErr, ErrNotFound) {
			return existsErr
		}
		r, _, runErr := uow.Runs().Get(ctx, handle.runID)
		if runErr != nil {
			return runErr
		}
		worktree, wtErr := run.NewAttemptWorktree(worktreeID, r.RepositoryID, handle.runID, intent.AttemptID, intent.BaseRef, candidate, intent.Branch)
		if wtErr != nil {
			return wtErr
		}
		if _, createErr := uow.Worktrees().Create(ctx, worktree); createErr != nil {
			return createErr
		}
		latest.State = OperationSucceeded
		latest.Outcome = worktreeCreateOutcome{Info: WorktreeInfo{WorkspaceID: workspace, Path: candidate, Branch: intent.Branch}, BaseCommit: intent.BaseRef}
		latest.UpdatedAt = c.Clock.Now()
		return uow.Operations().Save(ctx, latest)
	})
}

// markAttemptWorktreeReconciling moves an unresolved per-attempt
// worktree.create to reconciling with its condition, keeping any recorded
// act evidence.
func (c *Controller) markAttemptWorktreeReconciling(ctx context.Context, handle RunHandle, opID identity.OperationID, condition attemptWorktreeCondition) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	return c.settleAttemptWorktree(ctx, handle, opID, OperationReconciling, condition)
}

// settleAttemptWorktree moves an unresolved per-attempt worktree.create to
// state with its condition as the outcome, idempotently for one already
// settled.
func (c *Controller) settleAttemptWorktree(ctx context.Context, handle RunHandle, opID identity.OperationID, state OperationState, condition attemptWorktreeCondition) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		op, err := uow.Operations().Get(ctx, opID)
		if err != nil {
			return err
		}
		if op.State != OperationPending && op.State != OperationReconciling {
			return nil
		}
		op.State = state
		op.Outcome = condition
		op.UpdatedAt = now
		return uow.Operations().Save(ctx, op)
	})
}

// unresolvedAttemptLaunches lists the run's launching child attempts whose
// launch has not reached a pane: the session is launching, is its
// launching attempt's only session (a cold-relaunch successor is the
// relaunch path's), has no binding, and no pane.open or launch.send
// operation names it in any state — a launch intent, once journaled, is
// the launch machinery's and is never opened twice. blocked is non-empty,
// and names why, when an undecodable launch intent might name any of them.
func (c *Controller) unresolvedAttemptLaunches(ctx context.Context, handle RunHandle) ([]attemptLaunchTarget, string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	var (
		targets []attemptLaunchTarget
		blocked string
	)
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, err := RequireWorkflowRepositories(uow, "attempt launch recovery")
		if err != nil {
			return err
		}
		r, _, err := uow.Runs().Get(ctx, handle.runID)
		if err != nil {
			return err
		}
		launchIntents := map[identity.SessionID]bool{}
		for _, kind := range []OperationKind{OpPaneOpen, OpLaunchSend} {
			ops, opErr := uow.Operations().ByKind(ctx, handle.runID, kind)
			if opErr != nil {
				return opErr
			}
			for i := range ops {
				intent, ok := decodeOperationPayload[paneOpenIntent](ops[i].Intent)
				if !ok || intent.SessionID == "" {
					blocked = fmt.Sprintf("launch operation %s has an unreadable intent that may name this session; nothing is launched while it stands", ops[i].ID)
					continue
				}
				launchIntents[intent.SessionID] = true
			}
		}
		sessions, err := wf.SessionIndex().ByRun(ctx, handle.runID)
		if err != nil {
			return err
		}
		perAttempt := map[identity.AttemptID]int{}
		for i := range sessions {
			if sessions[i].AttemptID != "" {
				perAttempt[sessions[i].AttemptID]++
			}
		}
		sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
		for i := range sessions {
			s := sessions[i]
			if (s.Role != run.RoleImplementer && s.Role != run.RoleReviewer) || s.State != run.SessionLaunching ||
				s.AttemptID == "" || perAttempt[s.AttemptID] != 1 || launchIntents[s.ID] {
				continue
			}
			if _, bound, bindErr := uow.Bindings().Current(ctx, s.ID); bindErr != nil {
				return bindErr
			} else if bound {
				continue
			}
			attempt, _, attErr := uow.Attempts().Get(ctx, s.AttemptID)
			if attErr != nil {
				return attErr
			}
			if attempt.State != run.AttemptLaunching {
				continue
			}
			task, _, taskErr := uow.Tasks().Get(ctx, attempt.TaskID)
			if taskErr != nil {
				return taskErr
			}
			targets = append(targets, attemptLaunchTarget{runSeq: r.Sequence, task: task, attempt: attempt, session: s})
		}
		return nil
	})
	return targets, blocked, err
}

// attemptLaunchState is what one unresolved launch's next step depends
// on: the attempt's worktree row, its newest worktree.create and the
// workspace a succeeded one recorded.
type attemptLaunchState struct {
	row       run.Worktree
	hasRow    bool
	newest    *Operation
	workspace string
}

// settles reports whether the launch's next step is a launch-failure
// settlement: a failed worktree.create and no row.
func (s *attemptLaunchState) settles() bool {
	return !s.hasRow && s.newest != nil && s.newest.State == OperationFailed
}

// readAttemptLaunchState reads one target's attemptLaunchState.
func (c *Controller) readAttemptLaunchState(ctx context.Context, handle RunHandle, target *attemptLaunchTarget) (attemptLaunchState, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per target.
	var state attemptLaunchState
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, err := RequireWorkflowRepositories(uow, "attempt launch recovery")
		if err != nil {
			return err
		}
		w, _, err := wf.WorktreeIndex().ByAttempt(ctx, target.attempt.ID)
		switch {
		case err == nil:
			state.row, state.hasRow = w, true
		case !errors.Is(err, ErrNotFound):
			return err
		}
		ops, err := uow.Operations().ByKind(ctx, handle.runID, OpWorktreeCreate)
		if err != nil {
			return err
		}
		for i := range ops { // newest first
			intent, ok := decodeOperationPayload[attemptWorktreeCreateIntent](ops[i].Intent)
			if !ok || intent.AttemptID != target.attempt.ID {
				continue
			}
			if state.newest == nil {
				op := ops[i]
				state.newest = &op
			}
			if ops[i].State == OperationSucceeded && state.workspace == "" {
				state.workspace = recordedWorkspace(&ops[i], intent.Branch)
			}
		}
		return nil
	})
	return state, err
}

// advanceAttemptLaunch advances one unresolved launch whose own
// worktree.create is not unresolved: an existing row continues the launch,
// a failed operation settles the attempt, no operation re-drives the
// create — unless an unattributable operation might be this attempt's.
func (c *Controller) advanceAttemptLaunch(ctx context.Context, handle RunHandle, in *attemptLaunchInputs, target *attemptLaunchTarget, state *attemptLaunchState, unattributed bool) (AttemptLaunchCondition, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per target.
	switch {
	case state.hasRow:
		return c.continueAttemptLaunch(ctx, handle, in, target, WorktreeInfo{WorkspaceID: state.workspace, Path: state.row.Path, Branch: state.row.Branch}, "")
	case state.newest == nil:
		if unattributed {
			return target.report(AttemptLaunchReconciling, "an unattributable worktree operation may be this attempt's; nothing is created while it stands"), nil
		}
		worktreeID, err := identity.ParseWorktreeID(c.IDs.NewID())
		if err != nil {
			return AttemptLaunchCondition{}, fmt.Errorf("app: generate worktree id: %w", err)
		}
		condition, _, err := c.launchAttempt(ctx, handle, in, target, worktreeID, "")
		return condition, err
	case state.newest.State == OperationFailed:
		return c.settleAttemptLaunchFailure(ctx, handle, in.frozen, target, failedWorktreeReason(state.newest))
	default:
		return target.report(AttemptLaunchReconciling, "the attempt's worktree operation succeeded but its row is missing; failing closed"), nil
	}
}

// recordedWorkspace returns the workspace a succeeded worktree.create
// recorded for branch, from its act evidence (a create) or its outcome (an
// adoption); "" when neither records one.
func recordedWorkspace(op *Operation, branch string) string {
	for _, payload := range []any{op.ActEvidence, op.Outcome} {
		if recorded, ok := decodeOperationPayload[worktreeCreateOutcome](payload); ok && recorded.Info.Branch == branch && recorded.Info.WorkspaceID != "" {
			return recorded.Info.WorkspaceID
		}
	}
	return ""
}

// failedWorktreeReason renders a settled-failed worktree operation's
// condition as the attempt's fixed-text failure reason.
func failedWorktreeReason(op *Operation) string {
	condition, _ := decodeOperationPayload[attemptWorktreeCondition](op.Outcome)
	switch condition.Condition {
	case worktreeConditionNoCheckout:
		return "worktree creation failed: no checkout of the attempt's branch surfaced within the bounded wait"
	case worktreeConditionUnrelated:
		return "worktree creation failed: the checkout of the attempt's branch is not the intended repository at the attempt's base"
	default:
		return fmt.Sprintf("worktree creation failed: operation %s settled failed", op.ID)
	}
}

// launchAttempt drives a launching attempt that has no worktree.create
// intent yet — a fresh assignment, or one whose controller died between
// the assignment commit and the intent: the section 6 refuse-if-exists
// check of the attempt's branch (S9: an existing branch is checked out at
// its own tip and the requested base silently ignored), the worktree.create
// operation, then the continuation. incarnationID may be empty, in which
// case the continuation mints a fresh one.
func (c *Controller) launchAttempt(ctx context.Context, handle RunHandle, in *attemptLaunchInputs, target *attemptLaunchTarget, worktreeID identity.WorktreeID, incarnationID identity.IncarnationID) (AttemptLaunchCondition, WorktreeInfo, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per launch.
	base := target.task.SubjectCommitOID
	if target.task.Kind != run.TaskKindReview {
		head, err := in.implementerBase(ctx)
		if err != nil || head == "" {
			return target.report(AttemptLaunchReconciling, "the integration head could not be resolved; the worktree is created on a later round"), WorktreeInfo{}, nil //nolint:nilerr // an unresolvable head defers the create; the cause is git's, never a store failure.
		}
		base = head
	}
	branch := target.branch()
	ref := "refs/heads/" + branch
	observed := c.observeRef(ctx, in.frozen.RepositoryRoot, ref)
	switch {
	case !observed.observed:
		return target.report(AttemptLaunchReconciling, "the attempt's branch could not be checked for an existing ref; the worktree is created on a later round"), WorktreeInfo{}, nil
	case observed.symbolic || observed.value != "":
		condition, err := c.settleAttemptLaunchFailure(ctx, handle, in.frozen, target, fmt.Sprintf("worktree creation refused: %s already exists, and an existing branch would ignore the attempt's base", ref))
		return condition, WorktreeInfo{}, err
	}
	if disposition, detail, err := c.launchSuppression(ctx, handle); err != nil || disposition != "" {
		return target.report(disposition, detail), WorktreeInfo{}, err
	}

	info, outcome, err := c.createAttemptWorktree(ctx, handle, worktreeID, target.attempt.ID, in.frozen.RepositoryRoot, branch, base, c.Clock.Now())
	if err != nil {
		return AttemptLaunchCondition{}, WorktreeInfo{}, err
	}
	switch outcome {
	case attemptWorktreeStopRequested:
		return target.report(AttemptLaunchStopRequested, "a stop request refused the worktree creation; stop handling resolves its pending intent"), WorktreeInfo{}, nil
	case attemptWorktreeUnresolved:
		return target.report(AttemptLaunchReconciling, "worktree creation did not complete; later rounds recover it by provenance or settle it after the bounded wait"), WorktreeInfo{}, nil
	case attemptWorktreeFailed:
		condition, settleErr := c.settleAttemptLaunchFailure(ctx, handle, in.frozen, target, "worktree creation failed: the created checkout is not the intended repository at the attempt's base")
		return condition, WorktreeInfo{}, settleErr
	}
	condition, err := c.continueAttemptLaunch(ctx, handle, in, target, info, incarnationID)
	return condition, info, err
}

// continueAttemptLaunch finishes a launch whose worktree exists: the
// attempt's assignment artifact (prior feedback and the review diff base
// read as the assignment path reads them), then the child pane with a
// fresh incarnation — only in a recorded workspace, never a guessed one.
func (c *Controller) continueAttemptLaunch(ctx context.Context, handle RunHandle, in *attemptLaunchInputs, target *attemptLaunchTarget, worktree WorktreeInfo, incarnationID identity.IncarnationID) (AttemptLaunchCondition, error) { //nolint:gocritic // hugeParam: RunHandle and WorktreeInfo are per-call values; called once per launch.
	if worktree.WorkspaceID == "" {
		return target.report(AttemptLaunchReconciling, "the attempt's worktree was adopted but its workspace placement is unrecorded; the pane is never opened in a guessed workspace — hop stop retires the run"), nil
	}
	if !filepath.IsAbs(in.hopPath) {
		return target.report(AttemptLaunchReconciling, "the hop executable path is unknown to this round; the pane opens on a later round"), nil
	}
	facts := target.facts
	if facts == nil {
		read, err := c.attemptAssignmentInputs(ctx, handle, in.frozen, target)
		if err != nil {
			return AttemptLaunchCondition{}, err
		}
		facts = &read
	}
	if target.task.Kind == run.TaskKindReview && facts.reviewBase == "" {
		return target.report(AttemptLaunchReconciling, "no integration is recorded for the run; the review assignment's diff scope cannot be rendered"), nil
	}
	if disposition, detail, err := c.launchSuppression(ctx, handle); err != nil || disposition != "" {
		return target.report(disposition, detail), err
	}
	if err := c.writeAttemptAssignment(ctx, in.frozen, &target.task, &target.attempt, target.session.Role, in.hopPath, facts.prior, facts.reviewBase); err != nil {
		return target.report(AttemptLaunchReconciling, "the attempt's assignment artifact could not be written; a later round writes it before the pane opens"), nil //nolint:nilerr // a failed artifact write defers the pane; the next round rewrites it.
	}
	if incarnationID == "" {
		fresh, err := identity.ParseIncarnationID(c.IDs.NewID())
		if err != nil {
			return AttemptLaunchCondition{}, fmt.Errorf("app: generate incarnation id: %w", err)
		}
		incarnationID = fresh
	}
	if disposition, detail, err := c.launchSuppression(ctx, handle); err != nil || disposition != "" {
		return target.report(disposition, detail), err
	}
	outcome, err := c.openChildPane(ctx, handle, target.task.ID, target.attempt.ID, target.session.ID, incarnationID, target.session.Role, worktree, in.hopPath, in.stateRoot, c.Clock.Now())
	if err != nil {
		return AttemptLaunchCondition{}, err
	}
	switch outcome {
	case paneOpenStopRequested:
		return target.report(AttemptLaunchStopRequested, "a stop request refused the pane creation; stop handling resolves its pending intent"), nil
	case paneOpenUnresolved:
		return target.report(AttemptLaunchReconciling, "pane creation did not complete; launch corroboration recovers the pane by its creation label"), nil
	}
	return target.report(AttemptLaunchOpened, "the attempt's pane was opened; launch corroboration continues"), nil
}

// attemptAssignmentInputs reads an attempt's assignment facts inside a
// unit of work, as the assignment transaction gathers them.
func (c *Controller) attemptAssignmentInputs(ctx context.Context, handle RunHandle, frozen *FrozenRun, target *attemptLaunchTarget) (attemptAssignmentFacts, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	var facts attemptAssignmentFacts
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, err := RequireWorkflowRepositories(uow, "attempt assignment")
		if err != nil {
			return err
		}
		if target.task.Kind == run.TaskKindReview {
			tasks, taskErr := wf.TaskIndex().ByRun(ctx, handle.runID)
			if taskErr != nil {
				return taskErr
			}
			facts.reviewBase, err = earliestIntegrationBase(ctx, wf, tasks)
			return err
		}
		if target.attempt.Number <= 1 {
			return nil
		}
		attempts, err := wf.AttemptIndex().ByTask(ctx, target.task.ID)
		if err != nil {
			return err
		}
		facts.prior, err = collectPriorAttemptFeedback(ctx, uow, wf, handle.runID, frozen.Snapshot.StateRoot, attempts, target.attempt.Number)
		return err
	})
	return facts, err
}

// workerLaunchFailure is a terminal launch outcome before any process
// existed: the attempt's worktree could not be created or verified. The
// attempt fails, whatever the task consequence.
func workerLaunchFailure(reason string) workerTermination {
	return workerTermination{
		kind:          "launch failure",
		failAttempt:   true,
		reason:        reason,
		sessionReason: reason + "; no process was launched",
		noticeReason: func(consequence taskConsequence) string {
			if consequence == taskConsequenceInterrupted {
				return "the attempt failed to launch (" + reason + "; no process) while a stop was pending"
			}
			return "the attempt failed to launch (" + reason + "; no process)"
		},
	}
}

// settleAttemptLaunchFailure settles one unresolved launch as a terminal
// launch failure through the shared worker settlement: attempt failed,
// the budgeted task consequence, mailbox closure at the limit, the manager
// notice and the session terminated, in one transaction.
func (c *Controller) settleAttemptLaunchFailure(ctx context.Context, handle RunHandle, frozen *FrozenRun, target *attemptLaunchTarget, reason string) (AttemptLaunchCondition, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per settled launch.
	if err := c.settleWorkerTermination(ctx, handle, frozen, &target.session, workerLaunchFailure(reason)); err != nil {
		return AttemptLaunchCondition{}, err
	}
	return target.report(AttemptLaunchSettled, reason), nil
}

// featureFailureCauseLocked reports, inside the caller's transaction,
// whether the run carries a durable terminal-failure cause (a failed task,
// or the manager lineage's exec failure) — the causes RetireSettledSessions
// fails the run for. Nothing new is launched while one stands. The
// manager-lineage cause is read while the run is resuming too, since
// resume recovers launches after it has entered resuming; when the
// retirement pass fails the run is unchanged (managerLaunchFailedLocked).
func featureFailureCauseLocked(ctx context.Context, uow UnitOfWork, wf WorkflowRepositories, runID identity.RunID) (bool, error) {
	tasks, err := wf.TaskIndex().ByRun(ctx, runID)
	if err != nil {
		return false, err
	}
	for i := range tasks {
		if tasks[i].State == run.TaskFailed {
			return true, nil
		}
	}
	return managerLineageFailedLocked(ctx, uow, wf, runID, run.RunLaunching, run.RunRunning, run.RunResuming)
}

// launchSuppression re-reads, in its own unit of work, what forbids a new
// launch step — a worktree intent, an assignment artifact or a pane — for
// an assigned attempt: a held stop, or a durable terminal-failure cause.
// It returns the disposition and fixed detail to report, "" when the
// launch may proceed. Checked immediately before each such step, so a
// cause a settlement created earlier in the same round, or a manager
// exec failure recorded meanwhile, suppresses every later launch.
func (c *Controller) launchSuppression(ctx context.Context, handle RunHandle) (disposition, detail string, err error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per launch step.
	err = c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "attempt launch")
		if wfErr != nil {
			return wfErr
		}
		r, _, runErr := uow.Runs().Get(ctx, handle.runID)
		if runErr != nil {
			return runErr
		}
		if r.StopRequested {
			disposition, detail = AttemptLaunchStopRequested, "a stop is requested; nothing more is launched, and stop handling resolves this launch"
			return nil
		}
		failing, causeErr := featureFailureCauseLocked(ctx, uow, wf, handle.runID)
		if failing {
			disposition, detail = AttemptLaunchReconciling, "the run carries a terminal-failure cause; nothing more is launched, and the failure settlement retires this session"
		}
		return causeErr
	})
	return disposition, detail, err
}

// resolveAttemptWorktreesForShutdown resolves every unresolved per-attempt
// worktree.create before a stopped or failed report — adopted and
// recorded, or settled failed after the bounded wait — returning what
// still stands.
func (c *Controller) resolveAttemptWorktreesForShutdown(ctx context.Context, handle RunHandle, frozen *FrozenRun) ([]string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	round, err := c.recoverAttemptLaunches(ctx, handle, attemptLaunchResolveOnly, &attemptLaunchInputs{frozen: frozen})
	return round.outstanding, err
}

// worktreeOperationAction renders the status surface's human action for
// one unresolved per-attempt worktree.create operation.
func worktreeOperationAction(op *Operation) string {
	condition, _ := decodeOperationPayload[attemptWorktreeCondition](op.Outcome)
	switch condition.Condition {
	case worktreeConditionIntent:
		return "the operation's intent names no attempt; HOP never settles it automatically — inspect the run's operation journal"
	case worktreeConditionListing:
		return "the repository's worktree listing could not be read; fix the repository's git state, then rerun hop resume or hop stop"
	case worktreeConditionUnverified:
		return "the checkout of this branch could not be verified; inspect it (git worktree list), repair or remove it, then rerun hop resume or hop stop"
	default:
		return "worktree creation may still be in flight; the controller settles it after " + op.CreatedAt.Add(worktreeRecoveryDeadline).UTC().Format(time.RFC3339) + " (a running controller, hop resume or hop stop drives it)"
	}
}
