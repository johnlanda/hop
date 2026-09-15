package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// ResumeRequest is hop resume's input. HOPPath and StateRoot mirror
// StartRunRequest's: the absolute hop executable path and state root
// cmd/hop's single resolver produced, needed only if reconciliation
// authorizes a cold relaunch.
type ResumeRequest struct {
	RunID         string
	ControllerID  string
	ConfirmAbsent bool
	HOPPath       string
	StateRoot     string
}

// ResumeOutcome is one reconciliation round's disposition (section 5).
type ResumeOutcome string

// Resume outcomes.
const (
	// ResumeWarmReattached means the current binding's occupant matched the
	// settled claim or recorded evidence (case 1): the run continues.
	ResumeWarmReattached ResumeOutcome = "warm-reattached"
	// ResumeColdRelaunched means absence was conclusively established (case
	// 3) and a new session/incarnation was launched.
	ResumeColdRelaunched ResumeOutcome = "cold-relaunched"
	// ResumeFailedClosed means a present occupant failed to match with no
	// positive retirement evidence (case 2, no evidence branch): a human
	// must inspect and act.
	ResumeFailedClosed ResumeOutcome = "failed-closed"
	// ResumeReconciling means neither warm reattach nor cold relaunch could
	// be established this round; the run stays resuming/reconciling.
	ResumeReconciling ResumeOutcome = "reconciling"
	// ResumeUnsupported means cold relaunch would be required but the
	// harness has no supported resume semantics (Codex, opencode).
	ResumeUnsupported ResumeOutcome = "unsupported"
	// ResumeNothingToDo means the run is already terminal, or the attempt
	// is; resume only needed to acquire the lease.
	ResumeNothingToDo ResumeOutcome = "nothing-to-do"
	// ResumeStartupContinued means a crash during initial startup was
	// recovered: the worktree was adopted or re-created and the worker pane
	// was opened, continuing the original launch.
	ResumeStartupContinued ResumeOutcome = "startup-continued"
	// ResumeStopPending means the run holds a stop request: resume routes
	// it back to stop handling (hop stop / DriveStop) before any worker
	// adoption or new dispatch, and never restores it to running.
	ResumeStopPending ResumeOutcome = "stop-pending"
)

// ResumeResult is one Resume call's outcome.
type ResumeResult struct {
	Outcome        ResumeOutcome
	ObservedPaneID string // set on ResumeFailedClosed: the pane a human should inspect
	Detail         string
}

// Resume acquires run's controller lease (a new fencing generation),
// resolves pending or ambiguous operations from previous generations per
// the section 4 decision table, and performs one round of the section 5
// reconciliation. It never sleeps or resends; the caller is responsible
// for driving the returned RunHandle further (CorroborateLaunch,
// DriveStop, ClaimAndRunCheck) once reconciliation lands on a continuable
// outcome. Entry is idempotent: a run already resuming is not
// re-transitioned, and a terminal run reports NothingToDo without any
// state change.
func (c *Controller) Resume(ctx context.Context, req ResumeRequest) (ResumeResult, RunHandle, error) {
	runID, err := identity.ParseRunID(req.RunID)
	if err != nil {
		return ResumeResult{}, RunHandle{}, fmt.Errorf("app: parse run id: %w", err)
	}
	lease, err := c.Store.AcquireLease(ctx, runID, req.ControllerID)
	if err != nil {
		return ResumeResult{}, RunHandle{}, fmt.Errorf("app: acquire lease: %w", err)
	}
	handle := newRunHandle(runID, lease)
	now := c.Clock.Now()

	entry, err := c.Read.LoadRunStatus(ctx, runID)
	if err != nil {
		return ResumeResult{}, handle, fmt.Errorf("app: load run status: %w", err)
	}
	switch entry.State {
	case run.RunCompleted, run.RunFailed, run.RunStopped:
		return ResumeResult{Outcome: ResumeNothingToDo, Detail: fmt.Sprintf("run is already %s", entry.State)}, handle, nil
	case run.RunStopping:
		// A stopping run is never turned back into a resuming one: recover
		// pending operations (retirement acts are stop-compatible) and
		// route the caller to stop handling, which observes termination.
		if _, recoverErr := c.recoverPendingOperations(ctx, handle, entry); recoverErr != nil {
			return ResumeResult{}, handle, recoverErr
		}
		return ResumeResult{Outcome: ResumeStopPending, Detail: "the run is stopping; drive hop stop to observe termination"}, handle, nil
	case run.RunResuming:
		// A previous resume round already entered resuming; entry is
		// idempotent and reconciliation just continues.
	case run.RunCreated, run.RunLaunching, run.RunRunning, run.RunCompleting:
		stopObserved := false
		if enterErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
			r, rRev, getErr := uow.Runs().Get(ctx, runID)
			if getErr != nil {
				return getErr
			}
			// The lease-free entry read races the worker-authority stop
			// write: re-check under the transaction, and never move a
			// stopping (or stop-requested) run toward resuming.
			if r.State == run.RunStopping || r.StopRequested {
				stopObserved = true
				return nil
			}
			rFrom := r.State
			r, resumeErr := r.EnterResuming(now)
			if resumeErr != nil {
				return resumeErr
			}
			if _, saveErr := uow.Runs().Save(ctx, r, rRev); saveErr != nil {
				return saveErr
			}
			return recordTransition(ctx, uow, EntityRun, runID.String(), string(rFrom), string(r.State), "hop resume acquired the lease", gen(handle.lease.Generation), now)
		}); enterErr != nil {
			return ResumeResult{}, handle, fmt.Errorf("app: enter resuming: %w", enterErr)
		}
		if stopObserved {
			if _, recoverErr := c.recoverPendingOperations(ctx, handle, entry); recoverErr != nil {
				return ResumeResult{}, handle, recoverErr
			}
			return ResumeResult{Outcome: ResumeStopPending, Detail: "a stop request raced resume's entry; drive hop stop to observe termination"}, handle, nil
		}
	default:
		return ResumeResult{}, handle, fmt.Errorf("app: run %s is in unknown state %q; failing closed", runID, entry.State)
	}

	blocked, recoverErr := c.recoverPendingOperations(ctx, handle, entry)
	if recoverErr != nil {
		return ResumeResult{}, handle, recoverErr
	}
	// A terminal unknown check outcome settled by recovery still owes the
	// run its worker retirement (the run fails only after termination is
	// observed): drive it here, before the terminal-attempt shortcut, so
	// no separate check round is needed to discover the work.
	retirementOutstanding, retireErr := c.driveTerminalUnknownFailure(ctx, handle)
	if retireErr != nil {
		return ResumeResult{}, handle, retireErr
	}

	detail, err := c.Read.LoadRunStatus(ctx, runID)
	if err != nil {
		return ResumeResult{}, handle, fmt.Errorf("app: load run status: %w", err)
	}

	result, err := c.reconcile(ctx, handle, detail, req, blocked, retirementOutstanding)
	return result, handle, err
}

// reconcile dispatches by the attempt's state, per the section 5 tables.
// An unknown attempt state fails closed rather than passing as terminal.
func (c *Controller) reconcile(ctx context.Context, handle RunHandle, detail RunDetail, req ResumeRequest, blocked []string, retirementOutstanding string) (ResumeResult, error) { //nolint:gocritic // hugeParam: RunHandle, RunDetail and ResumeRequest are per-call DTOs; this runs once per resume round.
	if detail.StopRequested {
		// A held stop request wins over adoption and every new dispatch:
		// durably restore the stopping state under the lease (the section 5
		// resuming→stopping row, cause "stop requested") before handing the
		// run to stop handling — DriveStop drives only a stopping run.
		now := c.Clock.Now()
		if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
			r, rRev, getErr := uow.Runs().Get(ctx, handle.runID)
			if getErr != nil {
				return getErr
			}
			rFrom := r.State
			next := r.RequestStop(now)
			if next.State == rFrom {
				return nil
			}
			if _, saveErr := uow.Runs().Save(ctx, next, rRev); saveErr != nil {
				return saveErr
			}
			return recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(rFrom), string(next.State), "stop requested", gen(handle.lease.Generation), now)
		}); err != nil {
			return ResumeResult{}, fmt.Errorf("app: restore stopping for the held stop request: %w", err)
		}
		return ResumeResult{Outcome: ResumeStopPending, Detail: "the run holds a stop request; drive hop stop to retire its work and observe termination"}, nil
	}
	switch detail.AttemptState {
	case run.AttemptReserved:
		return c.continueStartup(ctx, handle, detail, req, blocked)
	case run.AttemptRunning, run.AttemptSubmitted, run.AttemptChecking, run.AttemptReconciling, run.AttemptLaunching, run.AttemptRelaunching:
		return c.reconcileActive(ctx, handle, detail, req, blocked)
	case run.AttemptCompleted, run.AttemptFailed, run.AttemptInterrupted:
		// A terminal attempt over a non-terminal run still owes work — a
		// live worker awaiting retirement is never NothingToDo.
		if detail.State != run.RunCompleted && detail.State != run.RunFailed && detail.State != run.RunStopped {
			if retirementOutstanding != "" {
				return ResumeResult{Outcome: ResumeReconciling, Detail: retirementOutstanding + "; rerun hop resume once the worker is observed gone"}, nil
			}
			return ResumeResult{Outcome: ResumeReconciling, Detail: "attempt is terminal but the run's finalization is not complete; rerun hop resume"}, nil
		}
		return ResumeResult{Outcome: ResumeNothingToDo, Detail: "attempt is already terminal"}, nil
	default:
		return ResumeResult{}, fmt.Errorf("app: attempt %s is in unknown state %q; failing closed", detail.AttemptID, detail.AttemptState)
	}
}

// worktreeRecoveryDeadline bounds the wait for an in-flight worktree
// creation to surface after a crash between intent and act: within it, an
// absent checkout is ambiguous (the create may still be in flight); past
// it, an absent checkout settles the old intent as failed and startup
// re-drives creation.
const worktreeRecoveryDeadline = 120 * time.Second

// boundedWaitEvidence is the act-evidence payload recording an ambiguous
// operation's bounded wait window.
type boundedWaitEvidence struct {
	WaitingSince string `json:"waiting_since"`
	Deadline     string `json:"deadline"`
	Detail       string `json:"detail"`
}

// recoverPendingOperations resolves pending and reconciling operations
// from previous generations per the section 4 decision table, oldest
// first, before any new act: worktree.create by provenance-validated
// adoption, pane.open/launch.send by creation-label lookup with a bounded
// wait, check.run by claim-and-group retirement (recoverCheckExecution),
// and unknown operation kinds fail closed into reconciling. pane.close
// operations are resolved by the shared close procedure their own drivers
// re-enter (DriveStop and positive-evidence retirement), and attestations
// are journal-only.
func (c *Controller) recoverPendingOperations(ctx context.Context, handle RunHandle, detail RunDetail) ([]string, error) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; this runs once per resume.
	var (
		frozen  *FrozenRun
		blocked []string
	)
	for i := range detail.PendingOperations {
		op := &detail.PendingOperations[i]
		var err error
		switch op.Kind {
		case OpWorktreeCreate:
			if _, ok := decodeOperationPayload[worktreeCreateIntent](op.Intent); !ok {
				err = c.markOperationReconciling(ctx, handle, op.ID, "worktree.create intent could not be decoded; failing closed")
				blocked = append(blocked, fmt.Sprintf("operation %s (worktree.create) has an undecodable intent", op.ID))
				break
			}
			err = c.recoverWorktreeCreate(ctx, handle, op)
		case OpPaneOpen, OpLaunchSend:
			if _, ok := decodeOperationPayload[paneOpenIntent](op.Intent); !ok {
				err = c.markOperationReconciling(ctx, handle, op.ID, "pane.open intent could not be decoded; failing closed")
				blocked = append(blocked, fmt.Sprintf("operation %s (%s) has an undecodable intent", op.ID, op.Kind))
				break
			}
			err = c.recoverPaneOpen(ctx, handle, op)
		case OpCheckRun:
			if frozen == nil {
				loaded, loadErr := c.Read.LoadFrozenRun(ctx, handle.runID)
				if loadErr != nil {
					return nil, fmt.Errorf("app: load frozen run: %w", loadErr)
				}
				frozen = &loaded
			}
			// Takeover reads the check-exec claim and retires its process
			// group; confirmed absence applies the unknown-outcome rule. A
			// still-unresolved execution blocks new acts: an uninspectable
			// or live old check group must never coexist with a newly
			// authorized cold worker launch.
			var still string
			still, err = c.recoverCheckExecution(ctx, handle, op, frozen)
			if still != "" {
				blocked = append(blocked, still)
			}
		case OpPaneClose, OpAbsenceAttested:
			// Resolved by their own drivers; attestations are journal-only.
		default:
			err = c.markOperationReconciling(ctx, handle, op.ID, fmt.Sprintf("unknown operation kind %q; failing closed", op.Kind))
			blocked = append(blocked, fmt.Sprintf("operation %s has unknown kind %q", op.ID, op.Kind))
		}
		if err != nil {
			return nil, err
		}
	}
	return blocked, nil
}

// recoverWorktreeCreate resolves a pending worktree.create operation: the
// intended repository's worktree listing is searched for the intent's
// branch, and a found candidate is validated by provenance — adoption
// requires provenance, never path existence. No candidate within the
// bounded wait stays ambiguous; past the deadline the intent settles
// failed so startup may re-drive creation. A worktree row that already
// exists settles the operation as already adopted.
func (c *Controller) recoverWorktreeCreate(ctx context.Context, handle RunHandle, op *Operation) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per pending operation.
	intent, ok := decodeOperationPayload[worktreeCreateIntent](op.Intent)
	if !ok || intent.RepositoryRoot == "" || intent.Branch == "" || intent.BaseRef == "" {
		return c.markOperationReconciling(ctx, handle, op.ID, "worktree.create intent could not be decoded; failing closed")
	}

	already := false
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		_, _, getErr := uow.Worktrees().ByRun(ctx, handle.runID)
		if getErr == nil {
			already = true
			return nil
		}
		if errors.Is(getErr, ErrNotFound) {
			return nil
		}
		return getErr
	}); err != nil {
		return err
	}
	if already {
		return c.settleOperation(ctx, handle, op.ID, OperationSucceeded, "worktree already adopted")
	}

	listing, status := c.gitOutput(ctx, intent.RepositoryRoot, "worktree", "list", "--porcelain")
	if status != gitOK {
		return nil // the listing itself could not be made: ambiguous, retry on a later round.
	}
	candidate, found := worktreeForBranch(listing, intent.Branch)
	if !found {
		now := c.Clock.Now()
		if now.Sub(op.CreatedAt) <= worktreeRecoveryDeadline {
			return c.recordBoundedWait(ctx, handle, op, worktreeRecoveryDeadline, "no checkout for the intended branch yet; creation may be in flight")
		}
		return c.settleOperation(ctx, handle, op.ID, OperationFailed, "no checkout for the intended branch surfaced within the bounded wait")
	}

	provenance, provenanceDetail := c.classifyWorktreeProvenance(ctx, candidate, intent.RepositoryRoot, intent.BaseRef)
	switch provenance {
	case worktreeValid:
		worktreeID, err := identity.ParseWorktreeID(c.IDs.NewID())
		if err != nil {
			return fmt.Errorf("app: generate worktree id: %w", err)
		}
		return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
			r, _, getErr := uow.Runs().Get(ctx, handle.runID)
			if getErr != nil {
				return getErr
			}
			if _, createErr := uow.Worktrees().Create(ctx, run.NewWorktree(worktreeID, r.RepositoryID, handle.runID, candidate, intent.Branch)); createErr != nil {
				return createErr
			}
			latest, getErr := uow.Operations().Get(ctx, op.ID)
			if getErr != nil {
				return getErr
			}
			latest.State = OperationSucceeded
			latest.Outcome = worktreeCreateOutcome{Info: WorktreeInfo{Path: candidate, Branch: intent.Branch}, BaseCommit: intent.BaseRef}
			latest.UpdatedAt = c.Clock.Now()
			return uow.Operations().Save(ctx, latest)
		})
	case worktreeUnrelated:
		return c.settleOperation(ctx, handle, op.ID, OperationFailed, provenanceDetail)
	case worktreeAbsent, worktreeAmbiguous:
		return nil
	}
	return nil
}

// worktreeForBranch finds the checkout path for branch in a
// `git worktree list --porcelain` listing.
func worktreeForBranch(listing, branch string) (string, bool) {
	var path string
	for line := range strings.SplitSeq(listing, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch refs/heads/"):
			if strings.TrimPrefix(line, "branch refs/heads/") == branch && path != "" {
				return path, true
			}
		}
	}
	return "", false
}

// recoverPaneOpen resolves a pending pane.open (or launch.send) operation
// whose outcome was lost: the pane is looked up by its unique creation
// label; found, the binding and success commit — pane creation is never
// repeated. Not found within the launch-claim deadline is ambiguous (the
// create may still be in flight); past it the operation goes reconciling
// with a bounded-wait record, never a second create.
func (c *Controller) recoverPaneOpen(ctx context.Context, handle RunHandle, op *Operation) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per pending operation.
	intent, ok := decodeOperationPayload[paneOpenIntent](op.Intent)
	if !ok || intent.Label == "" {
		return c.markOperationReconciling(ctx, handle, op.ID, "pane.open intent could not be decoded; failing closed")
	}

	bindingExists := false
	if uowErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		_, found, bErr := uow.Bindings().Current(ctx, intent.SessionID)
		bindingExists = found
		return bErr
	}); uowErr != nil {
		return uowErr
	}
	if bindingExists {
		// The pane's binding is already recorded; whatever state the
		// operation reached (including a launch-deadline reconciliation)
		// stands — there is nothing left to recover here.
		return nil
	}

	ref, found, err := c.Runtime.FindPaneByLabel(ctx, intent.Label)
	if err != nil {
		return nil //nolint:nilerr // a failed lookup is ambiguous; recovery retries on a later round.
	}
	if !found {
		now := c.Clock.Now()
		if !LaunchDeadlineExpired(op.CreatedAt, now) {
			return c.recordBoundedWait(ctx, handle, op, LaunchClaimDeadline, "no pane carries the creation label yet; the create may be in flight")
		}
		return c.markOperationReconciling(ctx, handle, op.ID, "no pane surfaced for the creation label within the launch deadline; never re-created")
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
		if _, foundBinding, bErr := uow.Bindings().Current(ctx, intent.SessionID); bErr != nil {
			return bErr
		} else if !foundBinding {
			kind := run.LaunchInitial
			if op.Kind == OpLaunchSend {
				kind = run.LaunchResume
			}
			// The binding's server instance is CREATION evidence: it comes
			// from the value frozen into the pane.open intent before the
			// pane was created, never from a fresh observation — a binding
			// recovered after a server restart must not masquerade as
			// continuous. An intent without one records unknown, which
			// fails the continuity check closed.
			binding := run.NewRuntimeBinding(intent.SessionID, intent.IncarnationID, "", intent.ServerInstance, ref.WorkspaceID, ref.TabID, ref.PaneID, intent.Label, kind, now)
			if bindErr := uow.Bindings().Create(ctx, binding); bindErr != nil {
				return bindErr
			}
		}
		latest.State = OperationSucceeded
		latest.ActEvidence = PaneHandle(ref)
		latest.UpdatedAt = now
		return uow.Operations().Save(ctx, latest)
	})
}

// recordBoundedWait persists the ambiguous operation's wait window as act
// evidence, once, so the deadline a later round enforces is durable.
func (c *Controller) recordBoundedWait(ctx context.Context, handle RunHandle, op *Operation, window time.Duration, detail string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per ambiguous round.
	if recorded, ok := decodeOperationPayload[boundedWaitEvidence](op.ActEvidence); ok && recorded.WaitingSince != "" {
		return nil
	}
	now := c.Clock.Now()
	evidence := boundedWaitEvidence{
		WaitingSince: op.CreatedAt.UTC().Format(time.RFC3339Nano),
		Deadline:     op.CreatedAt.Add(window).UTC().Format(time.RFC3339Nano),
		Detail:       detail,
	}
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		latest, getErr := uow.Operations().Get(ctx, op.ID)
		if getErr != nil {
			return getErr
		}
		if latest.State != OperationPending && latest.State != OperationReconciling {
			return nil
		}
		latest.ActEvidence = evidence
		latest.UpdatedAt = now
		return uow.Operations().Save(ctx, latest)
	})
}

// settleOperation moves an operation to state with outcome, idempotently.
func (c *Controller) settleOperation(ctx context.Context, handle RunHandle, opID identity.OperationID, state OperationState, outcome string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per settlement.
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
		op.Outcome = outcome
		op.UpdatedAt = now
		return uow.Operations().Save(ctx, op)
	})
}

// continueStartup resumes a run whose attempt is still reserved: the
// crash happened during (or before) worktree creation, before the launch
// intent. Startup identity comes from the frozen run — repository root,
// state root and brief — never from journal archeology, so even a crash
// immediately after InitializeRun is recoverable. Once a provenance-valid
// worktree exists — adopted by recovery or freshly created here — the
// assignment artifact is verified (and recreated byte-identically from
// the frozen inputs when its file is lost) and the original startup
// continues by opening the worker pane with a fresh incarnation. The pane
// joins the workspace the worktree.create outcome recorded; without that
// recorded placement the run stays reconciling rather than opening a pane
// in an unknown workspace. Unresolved blocking work refuses the new acts.
func (c *Controller) continueStartup(ctx context.Context, handle RunHandle, detail RunDetail, req ResumeRequest, blocked []string) (ResumeResult, error) { //nolint:gocritic // hugeParam: RunHandle, RunDetail and ResumeRequest are per-call DTOs; this runs once per resume round.
	if req.HOPPath == "" || req.StateRoot == "" {
		return ResumeResult{}, errors.New("app: resume requires HOPPath and StateRoot to continue startup")
	}
	if len(blocked) > 0 {
		return ResumeResult{Outcome: ResumeReconciling, Detail: "unresolved work blocks the startup continuation: " + strings.Join(blocked, "; ")}, nil
	}
	for i := range detail.PendingOperations {
		if detail.PendingOperations[i].Kind == OpWorktreeCreate {
			return ResumeResult{Outcome: ResumeReconciling, Detail: "worktree creation is still unresolved; rerun hop resume"}, nil
		}
	}

	frozen, err := c.Read.LoadFrozenRun(ctx, handle.runID)
	if err != nil {
		return ResumeResult{}, fmt.Errorf("app: load frozen run: %w", err)
	}

	var (
		worktree  run.Worktree
		hasRow    bool
		workspace string
	)
	if uowErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		w, _, getErr := uow.Worktrees().ByRun(ctx, handle.runID)
		if getErr == nil {
			worktree = w
			hasRow = true
		} else if !errors.Is(getErr, ErrNotFound) {
			return getErr
		}
		ops, opErr := uow.Operations().ByKind(ctx, handle.runID, OpWorktreeCreate)
		if opErr != nil {
			return opErr
		}
		for i := range ops {
			if outcome, ok := decodeOperationPayload[worktreeCreateOutcome](ops[i].ActEvidence); ok && outcome.Info.WorkspaceID != "" {
				workspace = outcome.Info.WorkspaceID
				break
			}
			if outcome, ok := decodeOperationPayload[worktreeCreateOutcome](ops[i].Outcome); ok && outcome.Info.WorkspaceID != "" {
				workspace = outcome.Info.WorkspaceID
				break
			}
		}
		return nil
	}); uowErr != nil {
		return ResumeResult{}, uowErr
	}

	info := WorktreeInfo{WorkspaceID: workspace, Path: worktree.Path, Branch: worktree.Branch}
	if !hasRow {
		// Nothing exists yet (the pre-act crash window, past its bounded
		// wait, or a crash before the first worktree intent): re-drive
		// creation from the frozen repository root.
		var seq int
		if statusDetail, statusErr := c.Read.LoadRunStatus(ctx, handle.runID); statusErr == nil {
			seq = statusDetail.Sequence
		}
		branch := fmt.Sprintf("hop/run-%d", seq)
		ids := generatedIdentities{Run: handle.runID, Task: detail.TaskID, Attempt: detail.AttemptID, Session: detail.SessionID}
		var worktreeID identity.WorktreeID
		if worktreeID, err = identity.ParseWorktreeID(c.IDs.NewID()); err != nil {
			return ResumeResult{}, fmt.Errorf("app: generate worktree id: %w", err)
		}
		ids.Worktree = worktreeID
		created, createErr := c.createWorktree(ctx, handle, ids, frozen.RepositoryRoot, branch, c.Clock.Now())
		if createErr != nil {
			return ResumeResult{}, createErr
		}
		info = created
	}
	if info.WorkspaceID == "" {
		return ResumeResult{Outcome: ResumeReconciling, Detail: "worktree adopted but its workspace placement is unrecorded; the worker pane cannot be opened safely"}, nil
	}

	if assignErr := c.ensureAssignment(ctx, handle, &frozen, detail, req.HOPPath); assignErr != nil {
		return ResumeResult{}, assignErr
	}

	incarnationID, err := identity.ParseIncarnationID(c.IDs.NewID())
	if err != nil {
		return ResumeResult{}, fmt.Errorf("app: generate incarnation id: %w", err)
	}
	ids := generatedIdentities{Run: handle.runID, Task: detail.TaskID, Attempt: detail.AttemptID, Session: detail.SessionID, Incarnation: incarnationID}
	if err := c.openWorkerPane(ctx, handle, ids, info, req.HOPPath, req.StateRoot); err != nil {
		return ResumeResult{}, err
	}
	return ResumeResult{Outcome: ResumeStartupContinued, Detail: "startup continued: worker pane opened"}, nil
}

// ensureAssignment verifies the frozen assignment artifact before a worker
// is launched against it: an existing file must match the frozen digest,
// and a missing file is recreated byte-identically from the frozen inputs
// (brief, identities, paths). A recreation whose digest does not match the
// freeze — a moved hop executable, for example — fails closed rather than
// launching a worker against unverified instructions.
func (c *Controller) ensureAssignment(ctx context.Context, handle RunHandle, frozen *FrozenRun, detail RunDetail, hopPath string) error { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; called once per startup continuation.
	path := frozen.Snapshot.AssignmentPath
	if path == "" {
		return errors.New("app: the frozen snapshot records no assignment path")
	}
	if content, readErr := c.Artifacts.ReadArtifact(ctx, path); readErr == nil {
		if sha256Hex(content) != frozen.Snapshot.AssignmentDigest {
			return fmt.Errorf("app: assignment artifact at %s does not match its frozen digest; failing closed", path)
		}
		return nil
	}

	fields := assignmentFields{
		RunID:          handle.runID.String(),
		TaskID:         detail.TaskID.String(),
		AttemptID:      detail.AttemptID.String(),
		Brief:          frozen.Brief,
		AssignmentPath: path,
		HOPPath:        hopPath,
	}
	content := renderAssignment(&fields)
	if sha256Hex(content) != frozen.Snapshot.AssignmentDigest {
		return fmt.Errorf("app: the assignment artifact could not be recreated identically from the frozen inputs (digest mismatch; has the hop executable path changed?); failing closed")
	}
	if err := c.Artifacts.WriteArtifact(ctx, path, content); err != nil {
		return fmt.Errorf("app: recreate assignment artifact: %w", err)
	}
	return c.recordAssignmentArtifact(ctx, handle.lease, handle.runID, path, frozen.Snapshot.AssignmentDigest)
}

// reconcileActive handles every attempt state a live worker could still
// occupy a pane for. A launching/relaunching attempt corroborates through
// the ordinary fenced settlement path first — its state is never hidden
// from CorroborateLaunch — and only an ambiguous round moves it to
// reconciling. The reconciling decision is then: warm adoption of a
// settled claim under the one corroboration predicate, positive-evidence
// retirement plus cold relaunch, fail-closed on an unidentified occupant,
// or — with conclusively established absence — cold relaunch.
func (c *Controller) reconcileActive(ctx context.Context, handle RunHandle, detail RunDetail, req ResumeRequest, blocked []string) (ResumeResult, error) { //nolint:gocritic // hugeParam: RunHandle, RunDetail and ResumeRequest are per-call DTOs; this runs once per resume round.
	launchingState := detail.AttemptState == run.AttemptLaunching || detail.AttemptState == run.AttemptRelaunching
	execFailed := detail.Claim != nil && detail.Claim.State == LaunchClaimExecFailed
	if launchingState && !execFailed {
		progress, err := c.CorroborateLaunch(ctx, handle)
		if err != nil {
			return ResumeResult{}, err
		}
		switch progress {
		case LaunchSettled, LaunchAlreadySettled:
			if restoreErr := c.restoreRunAfterAdoption(ctx, handle); restoreErr != nil {
				return ResumeResult{}, restoreErr
			}
			// Applying a settled claim's lifecycle consequences is
			// historical catch-up, never evidence of a currently surviving
			// worker: reload and decide again with the attempt out of
			// launching, so the occupant is verified under the one
			// corroboration predicate — absent, replaced or unobservable
			// occupants fail closed — before any warm report.
			reloaded, loadErr := c.Read.LoadRunStatus(ctx, handle.runID)
			if loadErr != nil {
				return ResumeResult{}, fmt.Errorf("app: load run status: %w", loadErr)
			}
			return c.reconcileActive(ctx, handle, reloaded, req, blocked)
		case LaunchNeedsInteraction:
			paneID := ""
			if detail.Binding != nil {
				paneID = detail.Binding.PaneID
			}
			return forkingWrapperFailedClosed(paneID), nil
		case LaunchFailed, LaunchPending:
			// LaunchPending: still ambiguous — enter reconciling below.
		}
	}

	now := c.Clock.Now()
	priorAttemptState := detail.AttemptState
	if detail.AttemptState != run.AttemptReconciling {
		if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
			return enterReconciling(ctx, uow, detail, gen(handle.lease.Generation), now)
		}); err != nil {
			return ResumeResult{}, fmt.Errorf("app: enter reconciling: %w", err)
		}
	}

	if detail.Binding == nil || detail.Binding.PaneID == "" {
		// A crash between a confirmed positive-evidence retirement (close
		// succeeded, observation binding superseded) and the relaunch it
		// authorized leaves no current binding; the durable close outcome
		// still finishes the recovery.
		done, doneErr := c.completedRetirement(ctx, handle, detail)
		if doneErr != nil {
			return ResumeResult{}, doneErr
		}
		if done {
			if len(blocked) > 0 {
				return ResumeResult{Outcome: ResumeReconciling, Detail: "unresolved work blocks the relaunch: " + strings.Join(blocked, "; ")}, nil
			}
			return c.coldRelaunch(ctx, handle, detail, req)
		}
		return ResumeResult{Outcome: ResumeReconciling, Detail: "no runtime binding recorded yet"}, nil
	}
	pane, occupantAbsent, ambiguous := c.observePaneAbsence(ctx, detail.Binding.PaneID, detail.Binding.CreationLabel)
	if ambiguous != "" {
		// Ambiguous observation: never absence, never adoption
		// (docs/plan/phase-2-design.md section 5).
		return ResumeResult{Outcome: ResumeReconciling, Detail: ambiguous}, nil
	}

	if !occupantAbsent {
		// An exec_pending claim on an already-reconciling attempt settles
		// here, under the same inspected predicate — the ordinary
		// launching-state path no longer applies once the attempt entered
		// reconciling, and a live matching worker must not stay stuck.
		if detail.Claim != nil && detail.Claim.State == LaunchClaimExecPending && detail.AttemptState == run.AttemptReconciling {
			markers, markerErr := c.launchMarkers(ctx, handle, detail)
			if markerErr != nil {
				return ResumeResult{}, markerErr
			}
			paneMatches := detail.Binding.IncarnationID == detail.Claim.IncarnationID && !detail.Binding.Superseded
			settlement, occupant := CorroborateSettlement(paneMatches, pane, markers, *detail.Claim)
			switch settlement {
			case SettlementSettled:
				now := c.Clock.Now()
				if settleErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
					return uow.LaunchClaims().Settle(ctx, detail.Claim.IncarnationID, LaunchClaimSettlement{
						State: LaunchClaimExeced, PaneID: detail.Binding.PaneID, PID: occupant.Occupant.PID,
						Executable: detail.Claim.Executable, ArgvMarker: occupant.Marker, At: now,
					})
				}); settleErr != nil {
					return ResumeResult{}, fmt.Errorf("app: settle launch claim from reconciliation: %w", settleErr)
				}
				return c.warmReattach(ctx, handle, detail, priorAttemptState, occupant)
			case SettlementForkingWrapper:
				// The unsettled claim's wrapper topology fails closed exactly
				// as on the launching path: the claim stays exec_pending and
				// nothing is retired — the human decides.
				return forkingWrapperFailedClosed(detail.Binding.PaneID), nil
			case SettlementUnresolved:
			}
		}

		// Warm adoption goes through the one corroboration predicate,
		// against a settled claim only; an exec_pending claim was already
		// routed through the ordinary settlement path above.
		if detail.Claim != nil && detail.Claim.State == LaunchClaimExeced {
			markers, markerErr := c.launchMarkers(ctx, handle, detail)
			if markerErr != nil {
				return ResumeResult{}, markerErr
			}
			paneMatches := detail.Binding.IncarnationID == detail.Claim.IncarnationID && !detail.Binding.Superseded
			settlement, occupant := CorroborateSettlement(paneMatches, pane, markers, *detail.Claim)
			switch settlement {
			case SettlementSettled:
				return c.warmReattach(ctx, handle, detail, priorAttemptState, occupant)
			case SettlementForkingWrapper:
				// A matching member under a different pid while the claimed
				// process itself still matches is the refused wrapper
				// topology: rejected without retirement. With the claimed
				// process gone, the different-pid match is the restored
				// occupant case, which only the restored-harness predicate
				// below may authorize.
				if ClaimProcessMatches(pane, markers, *detail.Claim) {
					return forkingWrapperFailedClosed(detail.Binding.PaneID), nil
				}
			case SettlementUnresolved:
			}
		}

		// A different, present occupant. Positive evidence authorizes guarded
		// retirement and cold relaunch only through the restored-harness
		// predicate: exactly one foreground member that is the session
		// harness's native restore invocation for the durable native
		// reference, with executable identity and the exact resume argument
		// on that same member (the restored occupant spawns its own MCP
		// children into its own group, so it holds no particular index).
		// Retirement targets that member; more than one candidate, or none,
		// fails closed with nothing closed or superseded.
		harness, harnessErr := c.sessionHarness(ctx, handle, detail.SessionID)
		nativeRef, nativeErr := c.sessionNativeRef(ctx, handle, detail.SessionID)
		if harnessErr == nil && nativeErr == nil {
			switch outcome, candidates := MatchRestoredHarness(pane, harness, nativeRef); outcome {
			case RestoredHarnessMatched:
				if len(blocked) > 0 {
					return ResumeResult{Outcome: ResumeReconciling, Detail: "unresolved work blocks the retirement: " + strings.Join(blocked, "; ")}, nil
				}
				return c.retireAndRelaunch(ctx, handle, detail, req, candidates[0], nativeRef)
			case RestoredHarnessAmbiguous:
				pids := make([]string, 0, len(candidates))
				for _, candidate := range candidates {
					pids = append(pids, strconv.Itoa(candidate.PID))
				}
				return ResumeResult{
					Outcome: ResumeFailedClosed, ObservedPaneID: detail.Binding.PaneID,
					Detail: fmt.Sprintf("more than one foreground member (pids %s) matches the restored harness invocation for this run's native session; no single occupant is identified, so nothing is retired; inspect the pane, then close it or hop stop the run, and rerun hop resume", strings.Join(pids, ", ")),
				}, nil
			case RestoredHarnessNone, RestoredHarnessUnsupported:
			}
		}
		return ResumeResult{
			Outcome: ResumeFailedClosed, ObservedPaneID: detail.Binding.PaneID,
			Detail: "a present occupant does not match this run's claim identity and carries no positive evidence tying it to this run; inspect the pane, then close it or hop stop the run, and rerun hop resume, or attest absence with --confirm-absent",
		}, nil
	}

	// A pending positive-evidence retirement completes on observed absence:
	// the recorded close target is adopted and relaunch is authorized by
	// the retirement evidence itself, no attestation needed.
	retired, retireErr := c.completePendingRetirement(ctx, handle, detail)
	if retireErr != nil {
		return ResumeResult{}, retireErr
	}
	if retired || execFailed || req.ConfirmAbsent {
		if len(blocked) > 0 {
			return ResumeResult{Outcome: ResumeReconciling, Detail: "unresolved work blocks the relaunch: " + strings.Join(blocked, "; ")}, nil
		}
	}
	if retired {
		return c.coldRelaunch(ctx, handle, detail, req)
	}

	if execFailed {
		// A settled exec_failed claim is conclusive: the incarnation is
		// dead and never started a native transcript a restore could replay.
		return c.coldRelaunch(ctx, handle, detail, req)
	}
	if req.ConfirmAbsent {
		return c.attestAbsence(ctx, handle, detail, req)
	}
	return ResumeResult{Outcome: ResumeReconciling, Detail: "no live process observed; rerun with --confirm-absent once no worker for this run is running anywhere"}, nil
}

// forkingWrapperFailedClosed is the fail-closed report for the unsupported
// forking-wrapper topology, on every resume path that observes it: nothing
// is settled, adopted or retired, and the human decides.
func forkingWrapperFailedClosed(paneID string) ResumeResult {
	return ResumeResult{
		Outcome: ResumeFailedClosed, ObservedPaneID: paneID,
		Detail: "the occupant matches the claim's executable identity and marker but not its pid: the unsupported forking-wrapper topology; inspect the pane, then close it or hop stop the run",
	}
}

// absenceAttestationRecord is the absence.attested journal entry's payload:
// the two assertions the human's attestation covers, the observed absence
// evidence, and the server-continuity observation that decides whether the
// attestation may authorize a cold relaunch (section 5, item 5).
type absenceAttestationRecord struct {
	Assertions            []string `json:"assertions"`
	ObservedEvidence      string   `json:"observed_evidence"`
	RecordedInstance      string   `json:"recorded_instance"`
	ObservedInstance      string   `json:"observed_instance"`
	ContinuityEstablished bool     `json:"continuity_established"`
}

// attestAbsence records the human's --confirm-absent attestation as an
// absence.attested journal entry and decides whether it authorizes the
// item-3 cold relaunch: only in the established non-restart case — the
// recorded and observed server instances prove continuity — with the pane
// already observed absent and HOP's own pending launch claims and intents
// retired by the relaunch itself. Unknown continuity or a changed
// instance keeps the run resuming with the item-2 report: after a server
// restart a deferred native restore may still fire, and a truthful
// present-tense attestation cannot retire it.
func (c *Controller) attestAbsence(ctx context.Context, handle RunHandle, detail RunDetail, req ResumeRequest) (ResumeResult, error) { //nolint:gocritic // hugeParam: RunHandle, RunDetail and ResumeRequest are per-call DTOs; this runs once per attestation.
	recorded := ""
	if detail.Binding != nil {
		recorded = detail.Binding.ServerInstance
	}
	observed := c.observeServerInstance(ctx)
	continuity := ServerContinuityEstablished(recorded, observed)

	record := absenceAttestationRecord{
		Assertions: []string{
			"no worker for this run is currently running anywhere",
			"every outstanding mechanism that could still start one has been retired",
		},
		ObservedEvidence:      "recorded pane positively absent by id (pane not found) and by creation label, each a successful observation",
		RecordedInstance:      recorded,
		ObservedInstance:      observed,
		ContinuityEstablished: continuity,
	}
	opID, err := c.newOperationID()
	if err != nil {
		return ResumeResult{}, err
	}
	now := c.Clock.Now()
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpAbsenceAttested, State: OperationSucceeded, Intent: record, Outcome: record,
			CreatedAt: now, UpdatedAt: now,
		})
	}); err != nil {
		return ResumeResult{}, fmt.Errorf("app: journal absence attestation: %w", err)
	}

	if !continuity {
		reason := "the server-process identity at pane creation is unknown"
		if recorded != "" && observed != "" {
			reason = fmt.Sprintf("the server process changed (recorded %q, observed %q): a deferred native restore may still fire", recorded, observed)
		}
		return ResumeResult{
			Outcome: ResumeReconciling,
			Detail:  "attestation recorded, but cold relaunch is refused: " + reason + "; the run stays reconciling until the restored occupant appears and is retired by positive evidence, or hop stop ends the run",
		}, nil
	}
	return c.coldRelaunch(ctx, handle, detail, req)
}

// completePendingRetirement finds an unresolved positive-evidence
// pane.close operation targeting the current binding and re-drives it:
// with the target now observed absent, the retirement completes and
// authorizes the cold relaunch it was recorded for.
func (c *Controller) completePendingRetirement(ctx context.Context, handle RunHandle, detail RunDetail) (bool, error) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; this runs once per resume round.
	if detail.Binding == nil {
		return false, nil
	}
	for i := range detail.PendingOperations {
		op := &detail.PendingOperations[i]
		if op.Kind != OpPaneClose {
			continue
		}
		intent, ok := decodeOperationPayload[paneCloseIntent](op.Intent)
		if !ok || intent.Reason != closeReasonRetirement || intent.PaneID != detail.Binding.PaneID {
			continue
		}
		target := paneCloseTarget{
			PaneID: intent.PaneID, Label: intent.Label,
			SessionID: intent.SessionID, IncarnationID: intent.IncarnationID,
			PID: intent.PID, Markers: intent.ArgvMarkers, Reason: intent.Reason,
		}
		retired, _, err := c.closePaneOperation(ctx, handle, detail, &target)
		return retired, err
	}
	return false, nil
}

// observePaneAbsence applies the one absence rule shared by resume and
// the pane.close procedure: absence is established ONLY by the pane being
// POSITIVELY absent by id (InspectPane reporting the typed
// ErrPaneNotFound result) AND positively absent by creation label (a
// successful lookup finding no pane). Each conjunct must be a successful
// observation: any other inspection or lookup error is ambiguous — never
// absence — with the error named in ambiguous, and a pane that still
// answers by id (even with an empty foreground) or by label keeps the
// observation ambiguous (a restored pane keeps its label; S3's phantom
// rule). ambiguous is "" exactly when the observation is conclusive: an
// occupant was observed (absent false) or absence was established
// (absent true).
func (c *Controller) observePaneAbsence(ctx context.Context, paneID, label string) (pane PaneProcess, absent bool, ambiguous string) {
	inspected, err := c.Runtime.InspectPane(ctx, paneID)
	switch {
	case err == nil:
		if len(inspected.Foreground) > 0 {
			return inspected, false, ""
		}
		return inspected, false, "the pane still answers by id with no foreground occupant; absence not established"
	case errors.Is(err, ErrPaneNotFound):
		// Positively absent by id; the label conjunct decides below.
	default:
		return PaneProcess{}, false, fmt.Sprintf("pane inspection failed (%v); absence is never assumed from an inspection error", err)
	}
	if label == "" {
		return PaneProcess{}, false, "pane absent by id, but the binding has no creation label to confirm absence independently"
	}
	if _, found, findErr := c.Runtime.FindPaneByLabel(ctx, label); findErr != nil {
		return PaneProcess{}, false, fmt.Sprintf("pane label lookup failed (%v); absence is never assumed from an inspection error", findErr)
	} else if found {
		return PaneProcess{}, false, "pane absent by id, but a pane still answers for the creation label; absence not established"
	}
	return PaneProcess{}, true, ""
}

// warmReattach adopts a verified occupant: the attempt returns to its
// prior state (derived from durable evidence when the attempt entered this
// round already reconciling), the session is confirmed active, and the
// binding's occupant evidence is refreshed with the corroborated member's
// own pid and the marker that actually corroborated it — never a
// synthesized marker, and never the pid of whichever member the foreground
// listing happened to report first.
func (c *Controller) warmReattach(ctx context.Context, handle RunHandle, detail RunDetail, priorState run.AttemptState, occupant SettlementEvidence) (ResumeResult, error) { //nolint:gocritic // hugeParam: RunHandle, RunDetail and SettlementEvidence are per-call values; this runs once per resume round.
	target := priorState
	if target == run.AttemptReconciling {
		derived, deriveErr := c.reattachTarget(ctx, handle, detail)
		if deriveErr != nil {
			return ResumeResult{}, deriveErr
		}
		target = derived
	}

	now := c.Clock.Now()
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		a, aRev, getErr := uow.Attempts().Get(ctx, detail.AttemptID)
		if getErr != nil {
			return getErr
		}
		nextAttempt, reattachErr := a.Reattach(target, now)
		if reattachErr != nil {
			return reattachErr
		}
		if _, saveErr := uow.Attempts().Save(ctx, nextAttempt, aRev); saveErr != nil {
			return saveErr
		}

		if restoreErr := restoreRunForAttemptState(ctx, uow, handle, target, now); restoreErr != nil {
			return restoreErr
		}

		s, sRev, getErr := uow.Sessions().Get(ctx, detail.SessionID)
		if getErr != nil {
			return getErr
		}
		sFrom := s.State
		nextSession, confirmErr := s.ConfirmActive(now)
		if confirmErr != nil {
			return confirmErr
		}
		if _, saveErr := uow.Sessions().Save(ctx, nextSession, sRev); saveErr != nil {
			return saveErr
		}

		binding, found, getErr := uow.Bindings().Current(ctx, detail.SessionID)
		if getErr != nil {
			return getErr
		}
		if found && occupant.Marker != "" {
			evidence := run.OccupantEvidence{Label: binding.CreationLabel, ArgvMarker: occupant.Marker, PID: occupant.Occupant.PID}
			nextBinding, observeErr := binding.Observe(evidence, now)
			if observeErr != nil {
				return observeErr
			}
			if saveErr := uow.Bindings().Save(ctx, nextBinding); saveErr != nil {
				return saveErr
			}
		}

		generation := gen(handle.lease.Generation)
		if transErr := recordTransition(ctx, uow, EntityAttempt, detail.AttemptID.String(), string(run.AttemptReconciling), string(nextAttempt.State), "warm reattach verified", generation, now); transErr != nil {
			return transErr
		}
		return recordTransition(ctx, uow, EntitySession, detail.SessionID.String(), string(sFrom), string(nextSession.State), "warm reattach verified", generation, now)
	})
	if err != nil {
		return ResumeResult{}, fmt.Errorf("app: warm reattach: %w", err)
	}
	return ResumeResult{Outcome: ResumeWarmReattached}, nil
}

// reattachTarget derives, from durable evidence, the state a reconciling
// attempt returns to on verified warm reattach: an accepted result whose
// check request is claimed means checking, an accepted result otherwise
// means submitted, and no accepted result means running.
func (c *Controller) reattachTarget(ctx context.Context, handle RunHandle, detail RunDetail) (run.AttemptState, error) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call values; called once per warm reattach.
	target := run.AttemptRunning
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		result, getErr := uow.Results().Accepted(ctx, detail.AttemptID)
		if getErr != nil {
			return getErr
		}
		if result == nil {
			return nil
		}
		target = run.AttemptSubmitted
		request, getErr := uow.CheckRequests().Get(ctx, result.ID)
		if getErr != nil {
			if errors.Is(getErr, ErrNotFound) {
				return nil
			}
			return getErr
		}
		if request.State == CheckRequestClaimed {
			target = run.AttemptChecking
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("app: derive reattach target: %w", err)
	}
	return target, nil
}

// restoreRunAfterAdoption restores a resuming run to the state its
// attempt implies after a verified adoption: checking implies completing,
// every other adopted state implies running. A run not resuming is left
// untouched (the settlement transaction already moved it).
func (c *Controller) restoreRunAfterAdoption(ctx context.Context, handle RunHandle) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per adoption.
	detail, err := c.Read.LoadRunStatus(ctx, handle.runID)
	if err != nil {
		return fmt.Errorf("app: load run status: %w", err)
	}
	if detail.State != run.RunResuming {
		return nil
	}
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return restoreRunForAttemptState(ctx, uow, handle, detail.AttemptState, now)
	})
}

// restoreRunForAttemptState moves a resuming run to running (or, for a
// checking attempt, completing) with its transition evidence, within the
// caller's transaction; any other run state is left untouched.
func restoreRunForAttemptState(ctx context.Context, uow UnitOfWork, handle RunHandle, attemptState run.AttemptState, now time.Time) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per adoption.
	r, rRev, err := uow.Runs().Get(ctx, handle.runID)
	if err != nil {
		return err
	}
	if r.State != run.RunResuming {
		return nil
	}
	rFrom := r.State
	var next run.Run
	if attemptState == run.AttemptChecking {
		if next, err = r.EnterCompleting(now); err != nil {
			return err
		}
	} else {
		if next, err = r.MarkRunning(now); err != nil {
			return err
		}
	}
	if _, err := uow.Runs().Save(ctx, next, rRev); err != nil {
		return err
	}
	return recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(rFrom), string(next.State), "warm reattach verified", gen(handle.lease.Generation), now)
}

// completedRetirement reports whether a positive-evidence retirement for
// the session already completed durably — its pane.close operation
// succeeded with observed absence — even though the binding it superseded
// is no longer current: the recorded outcome, not a live binding, is the
// recovery evidence.
func (c *Controller) completedRetirement(ctx context.Context, handle RunHandle, detail RunDetail) (bool, error) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; this runs once per resume round.
	if detail.SessionID == "" {
		return false, nil
	}
	done := false
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		ops, opErr := uow.Operations().ByKind(ctx, handle.runID, OpPaneClose)
		if opErr != nil {
			return opErr
		}
		for i := range ops {
			if ops[i].State != OperationSucceeded {
				continue
			}
			intent, ok := decodeOperationPayload[paneCloseIntent](ops[i].Intent)
			if !ok || intent.Reason != closeReasonRetirement || intent.SessionID != detail.SessionID {
				continue
			}
			outcome, ok := decodeOperationPayload[paneCloseOutcome](ops[i].Outcome)
			if ok && outcome.AbsenceObserved {
				done = true
				return nil
			}
		}
		return nil
	})
	return done, err
}

// retireAndRelaunch records the positively identified restored occupant as
// an observed-restoration binding under a freshly minted observation
// incarnation — the store's UNIQUE(session_id, incarnation_id) binding key
// is never reused — supersedes the launch binding with that evidence,
// retires the occupant through the shared pane.close operation procedure,
// and proceeds to cold relaunch only once its termination was observed.
// occupant is the one foreground member MatchRestoredHarness identified as
// the restored harness for nativeRef: the close target records THAT
// member's pid, never the pid of whichever member the listing reported
// first.
func (c *Controller) retireAndRelaunch(ctx context.Context, handle RunHandle, detail RunDetail, req ResumeRequest, occupant ProcessInfo, nativeRef string) (ResumeResult, error) { //nolint:gocritic // hugeParam: RunHandle, RunDetail, ResumeRequest and ProcessInfo are per-call values; this runs once per resume round.
	now := c.Clock.Now()
	evidence := fmt.Sprintf("observed process argv carries native session reference %s", nativeRef)
	observedInstance := c.observeServerInstance(ctx)
	observationIncarnation, err := identity.ParseIncarnationID(c.IDs.NewID())
	if err != nil {
		return ResumeResult{}, fmt.Errorf("app: generate observation incarnation id: %w", err)
	}

	// The current binding may already be the recorded observation from a
	// previous round; only a launch binding is superseded and re-recorded.
	var target paneCloseTarget
	uowErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		binding, found, getErr := uow.Bindings().Current(ctx, detail.SessionID)
		if getErr != nil {
			return getErr
		}
		if !found {
			return fmt.Errorf("app: no current binding to supersede for session %s", detail.SessionID)
		}
		target = paneCloseTarget{
			PaneID: binding.PaneID, Label: binding.CreationLabel,
			SessionID: detail.SessionID, IncarnationID: binding.IncarnationID,
			PID: occupant.PID, Markers: []string{nativeRef},
			Reason: closeReasonRetirement,
		}
		if binding.LaunchKind == run.LaunchRestoredObserved {
			return nil
		}
		nextBinding, supersedeErr := binding.Supersede(evidence, now)
		if supersedeErr != nil {
			return supersedeErr
		}
		if saveErr := uow.Bindings().Save(ctx, nextBinding); saveErr != nil {
			return saveErr
		}
		observed := run.NewRuntimeBinding(detail.SessionID, observationIncarnation, binding.ServerSocketPath, observedInstance, binding.WorkspaceID, binding.TabID, binding.PaneID, binding.CreationLabel, run.LaunchRestoredObserved, now)
		observedEvidence := run.OccupantEvidence{Label: binding.CreationLabel, ArgvMarker: nativeRef, PID: occupant.PID}
		observed, observeErr := observed.Observe(observedEvidence, now)
		if observeErr != nil {
			return observeErr
		}
		target.IncarnationID = observationIncarnation
		return uow.Bindings().Create(ctx, observed)
	})
	if uowErr != nil {
		return ResumeResult{}, fmt.Errorf("app: record restored-observed binding: %w", uowErr)
	}

	retired, outstanding, closeErr := c.closePaneOperation(ctx, handle, detail, &target)
	if closeErr != nil {
		return ResumeResult{}, closeErr
	}
	if !retired {
		return ResumeResult{Outcome: ResumeReconciling, Detail: "positive-evidence occupant recorded; " + outstanding}, nil
	}
	return c.coldRelaunch(ctx, handle, detail, req)
}

// coldRelaunch authorizes a cold relaunch: a new session and incarnation
// bound to the same attempt, a fresh pane in the same worktree and
// workspace. The replacement session inherits the prior session's
// immutable native reference — a real launcher cannot render
// `claude --resume <native-ref>` without it — so a missing reference or
// unrecorded workspace placement refuses the relaunch before any
// transition commits. Only Claude's resume semantics are supported in
// Phase 2.
func (c *Controller) coldRelaunch(ctx context.Context, handle RunHandle, detail RunDetail, req ResumeRequest) (ResumeResult, error) { //nolint:gocritic // hugeParam: RunHandle, RunDetail and ResumeRequest are per-call DTOs; this runs once per resume round.
	harness, err := c.sessionHarness(ctx, handle, detail.SessionID)
	if err != nil {
		return ResumeResult{}, err
	}
	if harness != run.HarnessClaude {
		return ResumeResult{Outcome: ResumeUnsupported, Detail: fmt.Sprintf("cold resume is not supported for harness %q in Phase 2", harness)}, nil
	}
	if req.HOPPath == "" || req.StateRoot == "" {
		return ResumeResult{}, errors.New("app: resume requires HOPPath and StateRoot to open the relaunch pane")
	}

	// Validate every relaunch input before any transition commits.
	priorRef, priorSource, err := c.sessionNativeLineage(ctx, handle, detail.SessionID)
	if err != nil {
		return ResumeResult{}, err
	}
	if priorRef == "" {
		return ResumeResult{}, fmt.Errorf("app: session %s has no native session reference; a Claude cold relaunch cannot be rendered", detail.SessionID)
	}
	worktree, err := c.currentWorktree(ctx, handle)
	if err != nil {
		return ResumeResult{}, err
	}
	worktree.WorkspaceID = c.relaunchWorkspace(ctx, handle, detail)
	if worktree.WorkspaceID == "" {
		return ResumeResult{Outcome: ResumeReconciling, Detail: "no recorded workspace placement for the relaunch pane; failing closed"}, nil
	}

	sessionID, err := identity.ParseSessionID(c.IDs.NewID())
	if err != nil {
		return ResumeResult{}, fmt.Errorf("app: generate session id: %w", err)
	}
	incarnationID, err := identity.ParseIncarnationID(c.IDs.NewID())
	if err != nil {
		return ResumeResult{}, fmt.Errorf("app: generate incarnation id: %w", err)
	}
	now := c.Clock.Now()

	err = c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		a, aRev, getErr := uow.Attempts().Get(ctx, detail.AttemptID)
		if getErr != nil {
			return getErr
		}
		nextAttempt, relaunchErr := a.Relaunch(now)
		if relaunchErr != nil {
			return relaunchErr
		}
		if _, saveErr := uow.Attempts().Save(ctx, nextAttempt, aRev); saveErr != nil {
			return saveErr
		}

		r, rRev, getErr := uow.Runs().Get(ctx, handle.runID)
		if getErr != nil {
			return getErr
		}
		rFrom := r.State
		nextRun, launchErr := r.Launch(now)
		if launchErr != nil {
			return launchErr
		}
		if _, saveErr := uow.Runs().Save(ctx, nextRun, rRev); saveErr != nil {
			return saveErr
		}

		if retireErr := retirePendingLaunchIntents(ctx, uow, handle.runID, detail.SessionID, now); retireErr != nil {
			return retireErr
		}

		// The old session is never revived: a cold relaunch binds a new
		// session and incarnation to the same attempt (section 5).
		oldSession, oldRev, getErr := uow.Sessions().Get(ctx, detail.SessionID)
		if getErr != nil {
			return getErr
		}
		oldFrom := oldSession.State
		oldSession, lostErr := oldSession.MarkLost(now)
		if lostErr != nil {
			return lostErr
		}
		if _, saveErr := uow.Sessions().Save(ctx, oldSession, oldRev); saveErr != nil {
			return saveErr
		}
		if transErr := recordTransition(ctx, uow, EntitySession, detail.SessionID.String(), string(oldFrom), string(oldSession.State), "cold relaunch authorized", gen(handle.lease.Generation), now); transErr != nil {
			return transErr
		}

		newSession := run.NewSession(sessionID, handle.runID, detail.AttemptID, harness, now)
		newSession, assignErr := newSession.AssignNativeRef(priorRef, priorSource, now)
		if assignErr != nil {
			return assignErr
		}
		var launchSessErr error
		newSession, launchSessErr = newSession.Launch(now)
		if launchSessErr != nil {
			return launchSessErr
		}
		if _, createErr := uow.Sessions().Create(ctx, newSession); createErr != nil {
			return createErr
		}

		generation := gen(handle.lease.Generation)
		if transErr := recordTransition(ctx, uow, EntityAttempt, detail.AttemptID.String(), string(run.AttemptReconciling), string(nextAttempt.State), "cold relaunch authorized", generation, now); transErr != nil {
			return transErr
		}
		if transErr := recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(rFrom), string(nextRun.State), "cold relaunch authorized", generation, now); transErr != nil {
			return transErr
		}
		return recordTransition(ctx, uow, EntitySession, sessionID.String(), string(run.SessionReserved), string(newSession.State), "cold relaunch authorized", generation, now)
	})
	if err != nil {
		return ResumeResult{}, fmt.Errorf("app: authorize cold relaunch: %w", err)
	}

	ids := generatedIdentities{Run: handle.runID, Task: detail.TaskID, Attempt: detail.AttemptID, Session: sessionID, Incarnation: incarnationID}
	if err := c.openRelaunchPane(ctx, handle, ids, worktree, req.HOPPath, req.StateRoot); err != nil {
		return ResumeResult{}, err
	}
	return ResumeResult{Outcome: ResumeColdRelaunched}, nil
}

// currentWorktree loads the run's single Phase 2 worktree as WorktreeInfo.
func (c *Controller) currentWorktree(ctx context.Context, handle RunHandle) (WorktreeInfo, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per relaunch.
	var info WorktreeInfo
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		w, _, getErr := uow.Worktrees().ByRun(ctx, handle.runID)
		if getErr != nil {
			return getErr
		}
		info = WorktreeInfo{Path: w.Path, Branch: w.Branch}
		return nil
	})
	return info, err
}

// sessionHarness reads a session's harness.
func (c *Controller) sessionHarness(ctx context.Context, handle RunHandle, sessionID identity.SessionID) (run.Harness, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per relaunch decision.
	var harness run.Harness
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		s, _, getErr := uow.Sessions().Get(ctx, sessionID)
		if getErr != nil {
			return getErr
		}
		harness = s.Harness
		return nil
	})
	return harness, err
}

// sessionNativeLineage reads a session's immutable native reference and
// its source, "" when unassigned.
func (c *Controller) sessionNativeLineage(ctx context.Context, handle RunHandle, sessionID identity.SessionID) (string, run.NativeRefSource, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per relaunch.
	var (
		ref    string
		source run.NativeRefSource
	)
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		s, _, getErr := uow.Sessions().Get(ctx, sessionID)
		if getErr != nil {
			return getErr
		}
		ref = s.NativeSessionRef
		source = s.NativeRefSource
		return nil
	})
	return ref, source, err
}

// relaunchWorkspace recovers the workspace the relaunch pane must join
// from recorded placement: the run detail's binding (captured before any
// retirement superseded it), or the worktree.create outcome's workspace.
func (c *Controller) relaunchWorkspace(ctx context.Context, handle RunHandle, detail RunDetail) string { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; called once per relaunch.
	if detail.Binding != nil && detail.Binding.WorkspaceID != "" {
		return detail.Binding.WorkspaceID
	}
	var workspace string
	_ = c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error { //nolint:errcheck // a failed read leaves workspace empty and the caller fails closed.
		ops, opErr := uow.Operations().ByKind(ctx, handle.runID, OpWorktreeCreate)
		if opErr != nil {
			return opErr
		}
		for i := range ops {
			if outcome, ok := decodeOperationPayload[worktreeCreateOutcome](ops[i].ActEvidence); ok && outcome.Info.WorkspaceID != "" {
				workspace = outcome.Info.WorkspaceID
				return nil
			}
			if outcome, ok := decodeOperationPayload[worktreeCreateOutcome](ops[i].Outcome); ok && outcome.Info.WorkspaceID != "" {
				workspace = outcome.Info.WorkspaceID
				return nil
			}
		}
		return nil
	})
	return workspace
}

// sessionNativeRef reads a session's native reference, "" when unassigned.
func (c *Controller) sessionNativeRef(ctx context.Context, handle RunHandle, sessionID identity.SessionID) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per reconciliation round.
	var ref string
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		s, _, getErr := uow.Sessions().Get(ctx, sessionID)
		if getErr != nil {
			return getErr
		}
		ref = s.NativeSessionRef
		return nil
	})
	return ref, err
}

// retirePendingLaunchIntents marks every still-pending pane.open operation
// whose intent named previousSession as superseded, in the same
// transaction that creates a cold relaunch's replacement session — before
// that new session's own intent is committed — so a retired incarnation's
// launcher can never claim against a stale intent (SubmissionStore's
// pre-binding fallback matches ClaimLaunch against the newest pending
// pane.open intent for the attempt; an old one left pending would still be
// "newest" until this runs).
func retirePendingLaunchIntents(ctx context.Context, uow UnitOfWork, runID identity.RunID, previousSession identity.SessionID, now time.Time) error {
	pending, err := uow.Operations().Pending(ctx, runID)
	if err != nil {
		return err
	}
	for _, op := range pending { //nolint:gocritic // rangeValCopy: Operation is a small per-run journal row; copying it to classify and possibly resave is clearer than indexing.
		if op.Kind != OpPaneOpen && op.Kind != OpLaunchSend {
			continue
		}
		intent, ok := decodeOperationPayload[paneOpenIntent](op.Intent)
		if !ok {
			// An undecodable launch payload cannot be proven retired: it
			// blocks the relaunch rather than being silently skipped.
			return fmt.Errorf("app: pending launch operation %s has an undecodable intent; retirement blocked", op.ID)
		}
		if intent.SessionID != previousSession {
			continue
		}
		op.State = OperationReconciling
		op.Outcome = "superseded: retired for a cold relaunch"
		op.UpdatedAt = now
		if saveErr := uow.Operations().Save(ctx, op); saveErr != nil {
			return saveErr
		}
	}
	return nil
}

// enterReconciling moves Attempt and, when it exists, the attempt's current
// Session into reconciling, recording both transitions.
func enterReconciling(ctx context.Context, uow UnitOfWork, detail RunDetail, generation *int64, now time.Time) error { //nolint:gocritic // hugeParam: RunDetail is a per-call DTO; this runs once per resume round.
	a, aRev, err := uow.Attempts().Get(ctx, detail.AttemptID)
	if err != nil {
		return err
	}
	aFrom := a.State
	a, err = a.Reconcile(now)
	if err != nil {
		return err
	}
	if _, saveErr := uow.Attempts().Save(ctx, a, aRev); saveErr != nil {
		return saveErr
	}
	if transErr := recordTransition(ctx, uow, EntityAttempt, detail.AttemptID.String(), string(aFrom), string(a.State), "takeover", generation, now); transErr != nil {
		return transErr
	}

	if detail.SessionID == "" {
		return nil
	}
	s, sRev, err := uow.Sessions().Get(ctx, detail.SessionID)
	if err != nil {
		return err
	}
	if s.State != run.SessionLaunching && s.State != run.SessionActive {
		return nil
	}
	sFrom := s.State
	s, err = s.Reconcile(now)
	if err != nil {
		return err
	}
	if _, err := uow.Sessions().Save(ctx, s, sRev); err != nil {
		return err
	}
	return recordTransition(ctx, uow, EntitySession, detail.SessionID.String(), string(sFrom), string(s.State), "takeover", generation, now)
}
