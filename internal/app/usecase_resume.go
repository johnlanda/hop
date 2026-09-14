package app

import (
	"context"
	"errors"
	"fmt"
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
	case run.RunResuming:
		// A previous resume round already entered resuming; entry is
		// idempotent and reconciliation just continues.
	case run.RunCreated, run.RunLaunching, run.RunRunning, run.RunCompleting, run.RunStopping:
		if enterErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
			r, rRev, getErr := uow.Runs().Get(ctx, runID)
			if getErr != nil {
				return getErr
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
	default:
		return ResumeResult{}, handle, fmt.Errorf("app: run %s is in unknown state %q; failing closed", runID, entry.State)
	}

	if recoverErr := c.recoverPendingOperations(ctx, handle, entry); recoverErr != nil {
		return ResumeResult{}, handle, recoverErr
	}

	detail, err := c.Read.LoadRunStatus(ctx, runID)
	if err != nil {
		return ResumeResult{}, handle, fmt.Errorf("app: load run status: %w", err)
	}

	result, err := c.reconcile(ctx, handle, detail, req)
	return result, handle, err
}

// reconcile dispatches by the attempt's state, per the section 5 tables.
// An unknown attempt state fails closed rather than passing as terminal.
func (c *Controller) reconcile(ctx context.Context, handle RunHandle, detail RunDetail, req ResumeRequest) (ResumeResult, error) { //nolint:gocritic // hugeParam: RunHandle, RunDetail and ResumeRequest are per-call DTOs; this runs once per resume round.
	switch detail.AttemptState {
	case run.AttemptReserved:
		return c.continueStartup(ctx, handle, detail, req)
	case run.AttemptRunning, run.AttemptSubmitted, run.AttemptChecking, run.AttemptReconciling, run.AttemptLaunching, run.AttemptRelaunching:
		return c.reconcileActive(ctx, handle, detail, req)
	case run.AttemptCompleted, run.AttemptFailed, run.AttemptInterrupted:
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
// wait, and unknown operation kinds fail closed into reconciling.
// pane.close and check.run operations are resolved by their own drivers
// (DriveStop, retirement and the check use case), never here.
func (c *Controller) recoverPendingOperations(ctx context.Context, handle RunHandle, detail RunDetail) error { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; this runs once per resume.
	var frozen *FrozenRun
	for i := range detail.PendingOperations {
		op := &detail.PendingOperations[i]
		var err error
		switch op.Kind {
		case OpWorktreeCreate:
			err = c.recoverWorktreeCreate(ctx, handle, op)
		case OpPaneOpen, OpLaunchSend:
			err = c.recoverPaneOpen(ctx, handle, op)
		case OpCheckRun:
			if frozen == nil {
				loaded, loadErr := c.Read.LoadFrozenRun(ctx, handle.runID)
				if loadErr != nil {
					return fmt.Errorf("app: load frozen run: %w", loadErr)
				}
				frozen = &loaded
			}
			// Takeover reads the check-exec claim and retires its process
			// group; confirmed absence applies the unknown-outcome rule.
			_, err = c.recoverCheckExecution(ctx, handle, op, frozen)
		case OpPaneClose, OpAbsenceAttested:
			// Resolved by their own drivers; attestations are journal-only.
		default:
			err = c.markOperationReconciling(ctx, handle, op.ID, fmt.Sprintf("unknown operation kind %q; failing closed", op.Kind))
		}
		if err != nil {
			return err
		}
	}
	return nil
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
			binding := run.NewRuntimeBinding(intent.SessionID, intent.IncarnationID, "", c.observeServerInstance(ctx), ref.WorkspaceID, ref.TabID, ref.PaneID, intent.Label, kind, now)
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
// intent. Once a provenance-valid worktree exists — adopted by recovery
// or freshly created here — the original startup continues by opening the
// worker pane with a fresh incarnation. The pane joins the workspace the
// worktree.create outcome recorded; without that recorded placement the
// run stays reconciling rather than opening a pane in an unknown
// workspace.
func (c *Controller) continueStartup(ctx context.Context, handle RunHandle, detail RunDetail, req ResumeRequest) (ResumeResult, error) { //nolint:gocritic // hugeParam: RunHandle, RunDetail and ResumeRequest are per-call DTOs; this runs once per resume round.
	if req.HOPPath == "" || req.StateRoot == "" {
		return ResumeResult{}, errors.New("app: resume requires HOPPath and StateRoot to continue startup")
	}
	for i := range detail.PendingOperations {
		if detail.PendingOperations[i].Kind == OpWorktreeCreate {
			return ResumeResult{Outcome: ResumeReconciling, Detail: "worktree creation is still unresolved; rerun hop resume"}, nil
		}
	}

	var (
		worktree  run.Worktree
		hasRow    bool
		snapshot  RunSnapshot
		workspace string
	)
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
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
	}); err != nil {
		return ResumeResult{}, err
	}
	_ = snapshot

	repositoryRoot, err := c.runRepositoryRoot(ctx, handle)
	if err != nil {
		return ResumeResult{}, err
	}

	info := WorktreeInfo{WorkspaceID: workspace, Path: worktree.Path, Branch: worktree.Branch}
	if !hasRow {
		// Nothing exists yet (the pre-act crash window, past its bounded
		// wait): re-drive creation itself; the settled prior intent no
		// longer blocks it.
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
		created, createErr := c.createWorktree(ctx, handle, ids, repositoryRoot, branch, c.Clock.Now())
		if createErr != nil {
			return ResumeResult{}, createErr
		}
		info = created
	}
	if info.WorkspaceID == "" {
		return ResumeResult{Outcome: ResumeReconciling, Detail: "worktree adopted but its workspace placement is unrecorded; the worker pane cannot be opened safely"}, nil
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

// runRepositoryRoot resolves the run's repository root through ReadStore's
// run listing scope: the worktree.create intent recorded it durably, so it
// is read back from the newest such operation.
func (c *Controller) runRepositoryRoot(ctx context.Context, handle RunHandle) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per startup continuation.
	var root string
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		ops, opErr := uow.Operations().ByKind(ctx, handle.runID, OpWorktreeCreate)
		if opErr != nil {
			return opErr
		}
		for i := range ops {
			if intent, ok := decodeOperationPayload[worktreeCreateIntent](ops[i].Intent); ok && intent.RepositoryRoot != "" {
				root = intent.RepositoryRoot
				return nil
			}
		}
		return fmt.Errorf("%w: no worktree.create intent records the repository root for run %s", ErrNotFound, handle.runID)
	})
	return root, err
}

// reconcileActive handles every attempt state a live worker could still
// occupy a pane for. A launching/relaunching attempt corroborates through
// the ordinary fenced settlement path first — its state is never hidden
// from CorroborateLaunch — and only an ambiguous round moves it to
// reconciling. The reconciling decision is then: warm adoption of a
// settled claim under the one corroboration predicate, positive-evidence
// retirement plus cold relaunch, fail-closed on an unidentified occupant,
// or — with conclusively established absence — cold relaunch.
func (c *Controller) reconcileActive(ctx context.Context, handle RunHandle, detail RunDetail, req ResumeRequest) (ResumeResult, error) { //nolint:gocritic // hugeParam: RunHandle, RunDetail and ResumeRequest are per-call DTOs; this runs once per resume round.
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
			return ResumeResult{Outcome: ResumeWarmReattached, Detail: "launch claim corroborated on resume"}, nil
		case LaunchNeedsInteraction:
			paneID := ""
			if detail.Binding != nil {
				paneID = detail.Binding.PaneID
			}
			return ResumeResult{
				Outcome: ResumeFailedClosed, ObservedPaneID: paneID,
				Detail: "the occupant matches the claim's executable identity and marker but not its pid: the unsupported forking-wrapper topology; inspect the pane, then close it or hop stop the run",
			}, nil
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
		return ResumeResult{Outcome: ResumeReconciling, Detail: "no runtime binding recorded yet"}, nil
	}
	pane, occupantAbsent, observed := c.observePane(ctx, detail.Binding)
	if !observed {
		// Inspection failed with no positive absence evidence: ambiguous,
		// never absence (docs/plan/phase-2-design.md section 5).
		return ResumeResult{Outcome: ResumeReconciling, Detail: "pane inspection failed; absence is never assumed from an inspection error"}, nil
	}

	if !occupantAbsent {
		// Warm adoption goes through the one corroboration predicate,
		// against a settled claim only; an exec_pending claim was already
		// routed through the ordinary settlement path above.
		if detail.Claim != nil && detail.Claim.State == LaunchClaimExeced {
			markers, markerErr := c.launchMarkers(ctx, handle, detail)
			if markerErr != nil {
				return ResumeResult{}, markerErr
			}
			paneMatches := detail.Binding.IncarnationID == detail.Claim.IncarnationID && !detail.Binding.Superseded
			if CorroborateSettlement(paneMatches, pane, markers, *detail.Claim) == SettlementSettled {
				return c.warmReattach(ctx, handle, detail, priorAttemptState, pane, FirstMarkerMatch(pane, markers))
			}
		}

		// A different, present occupant. Positive evidence (the run's
		// pre-assigned native session reference in its argv) authorizes
		// guarded retirement and cold relaunch; anything else fails closed.
		nativeRef, nativeErr := c.sessionNativeRef(ctx, handle, detail.SessionID)
		if nativeErr == nil && nativeRef != "" && paneCarriesMarker(pane, nativeRef) {
			return c.retireAndRelaunch(ctx, handle, detail, req, pane, nativeRef)
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
	recorded := ServerEvidence{}
	if detail.Binding != nil {
		recorded = ServerEvidence{SocketPath: detail.Binding.ServerSocketPath, Instance: detail.Binding.ServerInstance}
	}
	observed := ServerEvidence{SocketPath: recorded.SocketPath, Instance: c.observeServerInstance(ctx)}
	continuity := ServerContinuityEstablished(recorded, observed)

	record := absenceAttestationRecord{
		Assertions: []string{
			"no worker for this run is currently running anywhere",
			"every outstanding mechanism that could still start one has been retired",
		},
		ObservedEvidence:      "recorded pane observed absent by inspection and by creation label",
		RecordedInstance:      recorded.Instance,
		ObservedInstance:      observed.Instance,
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
		if recorded.Instance != "" && observed.Instance != "" {
			reason = fmt.Sprintf("the server process changed (recorded %q, observed %q): a deferred native restore may still fire", recorded.Instance, observed.Instance)
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

// observePane inspects a binding's pane and classifies the observation.
// absent is true only on positive evidence: the pane exists with no
// foreground occupant, or no pane carries the binding's creation label.
// observed is false when inspection failed and no positive absence could
// be established — ambiguous, never absence.
func (c *Controller) observePane(ctx context.Context, binding *run.RuntimeBinding) (pane PaneProcess, absent, observed bool) {
	inspected, err := c.Runtime.InspectPane(ctx, binding.PaneID)
	if err == nil {
		return inspected, len(inspected.Foreground) == 0, true
	}
	if binding.CreationLabel != "" {
		if _, found, findErr := c.Runtime.FindPaneByLabel(ctx, binding.CreationLabel); findErr == nil && !found {
			return PaneProcess{}, true, true
		}
	}
	return PaneProcess{}, false, false
}

// warmReattach adopts a verified occupant: the attempt returns to its
// prior state (derived from durable evidence when the attempt entered this
// round already reconciling), the session is confirmed active, and the
// binding's occupant evidence is refreshed with the marker that actually
// corroborated — never a synthesized one.
func (c *Controller) warmReattach(ctx context.Context, handle RunHandle, detail RunDetail, priorState run.AttemptState, pane PaneProcess, marker string) (ResumeResult, error) { //nolint:gocritic // hugeParam: RunHandle, RunDetail and PaneProcess are per-call values; this runs once per resume round.
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
		if found && marker != "" {
			evidence := run.OccupantEvidence{Label: binding.CreationLabel, ArgvMarker: marker, PID: firstForeground(pane).PID}
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

// retireAndRelaunch records the positively identified restored occupant as
// an observed-restoration binding under a freshly minted observation
// incarnation — the store's UNIQUE(session_id, incarnation_id) binding key
// is never reused — supersedes the launch binding with that evidence,
// retires the occupant through the shared pane.close operation procedure,
// and proceeds to cold relaunch only once its termination was observed.
func (c *Controller) retireAndRelaunch(ctx context.Context, handle RunHandle, detail RunDetail, req ResumeRequest, pane PaneProcess, nativeRef string) (ResumeResult, error) { //nolint:gocritic // hugeParam: RunHandle, RunDetail, ResumeRequest and PaneProcess are per-call values; this runs once per resume round.
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
			PID: firstForeground(pane).PID, Markers: []string{nativeRef},
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
		observedEvidence := run.OccupantEvidence{Label: binding.CreationLabel, ArgvMarker: nativeRef, PID: firstForeground(pane).PID}
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

// enterReconciling moves Attempt and, when it exists, the attempt's current
// Session into reconciling, recording both transitions.
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
		if !ok || intent.SessionID != previousSession {
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

// paneCarriesMarker reports whether pane's foreground process argv or
// cmdline carries marker.
func paneCarriesMarker(pane PaneProcess, marker string) bool {
	return FirstMarkerMatch(pane, []string{marker}) != ""
}
