package app

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// RequestStop records run's monotonic stop request. It carries no lease:
// hop stop can always request a stop, whether or not it holds the run's
// controller lease.
func (c *Controller) RequestStop(ctx context.Context, runIDStr string) error {
	runID, err := identity.ParseRunID(runIDStr)
	if err != nil {
		return fmt.Errorf("app: parse run id: %w", err)
	}
	if err := c.Submissions.RequestStop(ctx, runID); err != nil {
		return fmt.Errorf("app: request stop: %w", err)
	}
	return nil
}

// CheckRunIntent is the OpCheckRun operation's intent payload: what the
// check use case recorded before spawning hop check-exec, and what stop
// and resume's group-retirement classification matches an inspected
// process group's argv against — both the frozen check argv and the
// check-exec invocation that execs it, so a paused pre-exec boundary is
// retirable too.
type CheckRunIntent struct {
	CheckoutPath string   `json:"checkout_path"`
	CheckArgv    []string `json:"check_argv"`
	SpawnArgv    []string `json:"spawn_argv"`
}

// paneCloseIntent is the OpPaneClose operation's intent payload: the exact
// target the close was authorized against — pane placement, creation
// label, occupant pid and the durable argv markers — plus the reason the
// close was requested. A recovery round re-acts only against this same,
// positively re-matched target.
type paneCloseIntent struct {
	PaneID        string                 `json:"pane_id"`
	Label         string                 `json:"label"`
	SessionID     identity.SessionID     `json:"close_session_id"`
	IncarnationID identity.IncarnationID `json:"close_incarnation_id"`
	PID           int                    `json:"pid"`
	ArgvMarkers   []string               `json:"argv_markers"`
	Reason        string                 `json:"reason"`
}

// paneCloseOutcome is the OpPaneClose operation's outcome payload.
type paneCloseOutcome struct {
	// AbsenceObserved is true when the target was observed absent — the
	// retirement is complete — whether the close acted or the target was
	// already gone.
	AbsenceObserved bool   `json:"absence_observed"`
	Detail          string `json:"detail"`
}

// Close reasons a pane.close intent records.
const (
	// closeReasonStop interrupts the session's own corroborated worker.
	closeReasonStop = "stop"
	// closeReasonRetirement retires a positively identified restored
	// occupant — a foreign process, never the session's own worker.
	closeReasonRetirement = "positive-evidence retirement"
)

// paneCloseTarget is the recorded evidence one pane close is authorized
// against; it is frozen into the operation intent before any act.
type paneCloseTarget struct {
	PaneID        string
	Label         string
	SessionID     identity.SessionID
	IncarnationID identity.IncarnationID
	PID           int
	Markers       []string
	Reason        string
}

// matchesCloseTarget applies the close rule's occupant match: the observed
// foreground pid equals the recorded pid AND the argv carries one of the
// recorded durable markers. A pid is never evidence alone.
func matchesCloseTarget(target *paneCloseTarget, pane PaneProcess) bool {
	if len(pane.Foreground) == 0 {
		return false
	}
	return pane.Foreground[0].PID == target.PID && FirstMarkerMatch(pane, target.Markers) != ""
}

// StopReport is one DriveStop round's outcome.
type StopReport struct {
	RunState   string
	Terminated bool
	// Outstanding names the owned work not yet observed terminated:
	// dispatched closes/signals awaiting absence, or ambiguous targets the
	// round refused to act on.
	Outstanding []string
}

// DriveStop performs one round of stop interruption against handle: it
// interrupts not-yet-acted work directly, retires the recorded worker
// under the pane.close operation procedure and every check execution's
// process group under the group-retirement rule, and marks the run
// stopped ONLY once the worker and every check group have been observed
// absent — a dispatched close or signal is never itself termination.
// Mismatched occupants and failed inspections stay outstanding
// (reconciling), never absent. It is safe to call repeatedly: it never
// resends or recreates anything, and once the run is stopped every later
// call is a no-op reporting Terminated.
func (c *Controller) DriveStop(ctx context.Context, handle RunHandle) (StopReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; stop polling calls this method, never a hot inner loop.
	detail, err := c.Read.LoadRunStatus(ctx, handle.runID)
	if err != nil {
		return StopReport{}, fmt.Errorf("app: load run status: %w", err)
	}
	if detail.State == run.RunStopped {
		return StopReport{RunState: string(detail.State), Terminated: true}, nil
	}
	if detail.State != run.RunStopping {
		// A stop request was recorded but this run never reached a
		// stopping source state (RunRequestStop is monotonic but only
		// transitions state from a valid source); nothing to drive.
		return StopReport{RunState: string(detail.State)}, nil
	}

	if detail.AttemptState == run.AttemptReserved {
		return c.stopReserved(ctx, handle, detail)
	}

	outstanding, err := c.retireOwnedWork(ctx, handle, detail)
	if err != nil {
		return StopReport{RunState: string(run.RunStopping)}, err
	}
	if len(outstanding) > 0 {
		return StopReport{RunState: string(run.RunStopping), Outstanding: outstanding}, nil
	}
	return c.finishStop(ctx, handle, detail, "termination of owned work observed")
}

// retireOwnedWork drives one retirement round over everything the run
// owns: every check execution's process group, then the recorded worker.
// It returns the pieces still outstanding — dispatched but not yet
// observed absent, or ambiguous.
func (c *Controller) retireOwnedWork(ctx context.Context, handle RunHandle, detail RunDetail) ([]string, error) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; this runs once per stop round.
	var outstanding []string
	for i := range detail.PendingOperations {
		if detail.PendingOperations[i].Kind != OpCheckRun {
			continue
		}
		still, err := c.retireCheckOperation(ctx, handle, &detail.PendingOperations[i])
		if err != nil {
			return nil, err
		}
		if still != "" {
			outstanding = append(outstanding, still)
		}
	}

	still, err := c.retireWorker(ctx, handle, detail)
	if err != nil {
		return nil, err
	}
	if still != "" {
		outstanding = append(outstanding, still)
	}
	return outstanding, nil
}

// retireCheckOperation retires one pending check execution's process group
// under the section 7 group-retirement rule. An observed-empty group
// settles the operation (outcome unknown, interrupted by stop); a
// signaled or unmatched group stays outstanding.
func (c *Controller) retireCheckOperation(ctx context.Context, handle RunHandle, checkOp *Operation) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per pending check operation per round.
	var (
		claim CheckExecClaim
		found bool
	)
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		var err error
		claim, found, err = uow.CheckExecClaims().Get(ctx, checkOp.ID)
		return err
	}); err != nil {
		return "", fmt.Errorf("app: load check-exec claim: %w", err)
	}
	if !found {
		// No claim: nothing spawned, or the child died before its pre-exec
		// write. Ambiguous, never absence (the decision table's bounded
		// wait); the run stays stopping and this round reports it.
		return fmt.Sprintf("check execution %s has no claim yet; ambiguous, never absence", checkOp.ID), nil
	}

	intent, _ := decodeOperationPayload[CheckRunIntent](checkOp.Intent) // a decode failure leaves both argvs nil, which ClassifyGroupRetirement treats as never matching — fails closed, not a panic.
	outcome, retireErr := c.retireGroup(ctx, handle, claim.PID, [][]string{intent.CheckArgv, intent.SpawnArgv})
	if retireErr != nil {
		return "", retireErr
	}
	switch outcome {
	case GroupEmpty:
		if err := c.settleInterruptedCheck(ctx, handle, checkOp.ID); err != nil {
			return "", err
		}
		return "", nil
	case GroupMatched:
		return fmt.Sprintf("check group %d signaled; awaiting observed absence", claim.PID), nil
	case GroupMismatched:
		if err := c.markOperationReconciling(ctx, handle, checkOp.ID, "check group members do not match the recorded argv; failing closed"); err != nil {
			return "", err
		}
		return fmt.Sprintf("check group %d does not match the recorded argv; failing closed", claim.PID), nil
	default: // GroupInspectionFailed
		return fmt.Sprintf("check group %d could not be inspected; failing closed", claim.PID), nil
	}
}

// settleInterruptedCheck records a stop-retired check execution's outcome:
// the operation failed with an unknown result after its group was observed
// absent under a stop.
func (c *Controller) settleInterruptedCheck(ctx context.Context, handle RunHandle, opID identity.OperationID) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per retired check execution.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		op, err := uow.Operations().Get(ctx, opID)
		if err != nil {
			return err
		}
		if op.State != OperationPending && op.State != OperationReconciling {
			return nil
		}
		op.State = OperationFailed
		op.Outcome = checkRunOutcome{Unknown: true}
		op.UpdatedAt = now
		return uow.Operations().Save(ctx, op)
	})
}

// markOperationReconciling moves an operation to reconciling with detail
// as its outcome evidence, idempotently.
func (c *Controller) markOperationReconciling(ctx context.Context, handle RunHandle, opID identity.OperationID, detail string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per escalation.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		op, err := uow.Operations().Get(ctx, opID)
		if err != nil {
			return err
		}
		if op.State != OperationPending && op.State != OperationReconciling {
			return nil
		}
		op.State = OperationReconciling
		op.Outcome = detail
		op.UpdatedAt = now
		return uow.Operations().Save(ctx, op)
	})
}

// retireWorker retires the run's recorded worker through the pane.close
// operation procedure. It returns "" once the worker is observed absent
// (or there is provably nothing to retire), or the outstanding detail.
func (c *Controller) retireWorker(ctx context.Context, handle RunHandle, detail RunDetail) (string, error) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; this runs once per stop round.
	if detail.Binding == nil || detail.Binding.PaneID == "" {
		return "", nil
	}
	if detail.Claim == nil {
		// A binding with no claim: the pane may hold a pre-claim launcher.
		// There is no recorded identity to close against, so nothing is
		// acted on; only observed absence clears it.
		_, absent, observed := c.observePane(ctx, detail.Binding)
		if observed && absent {
			return "", nil
		}
		return "worker pane has no launch claim to retire against; failing closed", nil
	}
	if detail.Claim.State == LaunchClaimExecFailed {
		// The exec failed and the launcher exited; there is no process to
		// retire for this incarnation.
		return "", nil
	}

	markers, err := c.launchMarkers(ctx, handle, detail)
	if err != nil {
		return "", err
	}
	if detail.Binding.Occupant != nil && detail.Binding.Occupant.ArgvMarker != "" {
		markers = append(markers, detail.Binding.Occupant.ArgvMarker)
	}
	target := paneCloseTarget{
		PaneID:        detail.Binding.PaneID,
		Label:         detail.Binding.CreationLabel,
		SessionID:     detail.SessionID,
		IncarnationID: detail.Binding.IncarnationID,
		PID:           detail.Claim.PID,
		Markers:       markers,
		Reason:        closeReasonStop,
	}
	retired, outstanding, err := c.closePaneOperation(ctx, handle, detail, &target)
	if err != nil {
		return "", err
	}
	if retired {
		return "", nil
	}
	return outstanding, nil
}

// paneScrollbackLines bounds the ReadPane evidence captured before a close.
const paneScrollbackLines = 500

// closePaneOperation drives one OpPaneClose operation: commit the intent
// naming the exact target evidence, revalidate, re-inspect and match under
// the close rule, capture pane scrollback into the ArtifactStore, close
// outside any transaction, then persist the observed outcome. A recovery
// round (a pending OpPaneClose already journaled for the same target)
// adopts an already-gone target, re-acts only against the same positively
// matched target, and stays reconciling on mismatch. retired is true only
// once the target has been OBSERVED absent; a dispatched close is not
// termination. On confirmed retirement the target's current binding is
// superseded with the observed evidence.
func (c *Controller) closePaneOperation(ctx context.Context, handle RunHandle, detail RunDetail, target *paneCloseTarget) (retired bool, outstanding string, err error) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; this runs once per close round.
	opID, err := c.findOrCreateCloseOperation(ctx, handle, detail, target)
	if err != nil {
		return false, "", err
	}

	if err := c.revalidateForDispatch(ctx, handle, true); err != nil {
		return false, "", err
	}

	pane, inspectErr := c.Runtime.InspectPane(ctx, target.PaneID)
	if inspectErr != nil {
		if target.Label != "" {
			if _, found, findErr := c.Runtime.FindPaneByLabel(ctx, target.Label); findErr == nil && !found {
				if err := c.recordCloseOutcome(ctx, handle, opID, target, "pane absent by id and by label"); err != nil {
					return false, "", err
				}
				return true, "", nil
			}
		}
		return false, "pane inspection failed; absence is never assumed", nil
	}
	if len(pane.Foreground) == 0 {
		if err := c.recordCloseOutcome(ctx, handle, opID, target, "pane has no foreground occupant"); err != nil {
			return false, "", err
		}
		return true, "", nil
	}
	if !matchesCloseTarget(target, pane) {
		if err := c.markOperationReconciling(ctx, handle, opID, "occupant does not match the recorded close target; failing closed"); err != nil {
			return false, "", err
		}
		return false, "pane occupant does not match the recorded close target; failing closed", nil
	}

	// The occupant is the recorded target: capture scrollback evidence (it
	// vanishes with the pane), then close, then record the dispatch.
	c.capturePaneScrollback(ctx, handle, detail, opID, target.PaneID)
	if closeErr := c.Runtime.ClosePane(ctx, target.PaneID); closeErr != nil {
		return false, "", fmt.Errorf("app: close pane %s: %w", target.PaneID, closeErr)
	}
	if err := c.recordCloseDispatched(ctx, handle, opID, target); err != nil {
		return false, "", err
	}

	// One immediate re-observation: the pane may already be gone.
	after, afterErr := c.Runtime.InspectPane(ctx, target.PaneID)
	if afterErr == nil && len(after.Foreground) == 0 {
		if err := c.recordCloseOutcome(ctx, handle, opID, target, "occupant absent after close"); err != nil {
			return false, "", err
		}
		return true, "", nil
	}
	return false, "close dispatched; awaiting observed termination", nil
}

// findOrCreateCloseOperation reuses the pending OpPaneClose operation for
// the same target when one exists — a close is never re-journaled while
// unresolved — and otherwise commits a fresh intent.
func (c *Controller) findOrCreateCloseOperation(ctx context.Context, handle RunHandle, detail RunDetail, target *paneCloseTarget) (identity.OperationID, error) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; this runs once per close round.
	for i := range detail.PendingOperations {
		op := &detail.PendingOperations[i]
		if op.Kind != OpPaneClose {
			continue
		}
		intent, ok := decodeOperationPayload[paneCloseIntent](op.Intent)
		if !ok {
			continue
		}
		if intent.PaneID == target.PaneID && intent.IncarnationID == target.IncarnationID {
			return op.ID, nil
		}
	}

	opID, err := c.newOperationID()
	if err != nil {
		return "", err
	}
	now := c.Clock.Now()
	intent := paneCloseIntent{
		PaneID: target.PaneID, Label: target.Label,
		SessionID: target.SessionID, IncarnationID: target.IncarnationID,
		PID: target.PID, ArgvMarkers: target.Markers, Reason: target.Reason,
	}
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpPaneClose, State: OperationPending, Intent: intent,
			CreatedAt: now, UpdatedAt: now,
		})
	}); err != nil {
		return "", fmt.Errorf("app: record pane.close intent: %w", err)
	}
	return opID, nil
}

// capturePaneScrollback reads the pane's scrollback and stores it as a run
// artifact before a close: scrollback vanishes with the pane. Capture is
// evidence, not a gate — a failed read or write never blocks the close,
// which the stop still owes the run.
func (c *Controller) capturePaneScrollback(ctx context.Context, handle RunHandle, detail RunDetail, opID identity.OperationID, paneID string) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; called once per close.
	content, readErr := c.Runtime.ReadPane(ctx, paneID, paneScrollbackLines)
	if readErr != nil || detail.StateRoot == "" {
		return
	}
	path := filepath.Join(detail.StateRoot, "runs", handle.runID.String(), "artifacts", fmt.Sprintf("pane-scrollback-%s.txt", opID))
	if writeErr := c.Artifacts.WriteArtifact(ctx, path, []byte(content)); writeErr != nil {
		return
	}
	_ = c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error { //nolint:errcheck // best-effort evidence row; the capture itself succeeded and the close proceeds either way.
		artifactID, err := identity.ParseArtifactID(c.IDs.NewID())
		if err != nil {
			return err
		}
		return uow.Artifacts().Save(ctx, run.NewArtifact(artifactID, handle.runID, run.ArtifactPaneSnapshot, path, sha256Hex(content)))
	})
}

// recordCloseDispatched journals the close dispatch as act evidence and —
// for a stop, whose target is the session's own corroborated worker —
// moves the session to stopping: the interrupt has been dispatched;
// termination is observed later, never declared here. A retirement close
// targets a foreign restored occupant, so the session is left for the
// relaunch decision to settle (lost, with the retirement as evidence).
func (c *Controller) recordCloseDispatched(ctx context.Context, handle RunHandle, opID identity.OperationID, target *paneCloseTarget) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per dispatched close.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		op, err := uow.Operations().Get(ctx, opID)
		if err != nil {
			return err
		}
		op.ActEvidence = fmt.Sprintf("close dispatched at %s against pid %d", now.UTC().Format(time.RFC3339Nano), target.PID)
		op.UpdatedAt = now
		if saveErr := uow.Operations().Save(ctx, op); saveErr != nil {
			return saveErr
		}

		if target.SessionID == "" || target.Reason != closeReasonStop {
			return nil
		}
		s, sRev, err := uow.Sessions().Get(ctx, target.SessionID)
		if err != nil {
			return err
		}
		if s.State != run.SessionActive && s.State != run.SessionLaunching && s.State != run.SessionReconciling {
			return nil
		}
		sFrom := s.State
		next, stopErr := s.Stop(now)
		if stopErr != nil {
			return stopErr
		}
		if _, saveErr := uow.Sessions().Save(ctx, next, sRev); saveErr != nil {
			return saveErr
		}
		return recordTransition(ctx, uow, EntitySession, target.SessionID.String(), string(sFrom), string(next.State), "interrupt dispatched", gen(handle.lease.Generation), now)
	})
}

// recordCloseOutcome settles an OpPaneClose operation once the target has
// been observed absent, and supersedes the target's current binding with
// the observed retirement evidence.
func (c *Controller) recordCloseOutcome(ctx context.Context, handle RunHandle, opID identity.OperationID, target *paneCloseTarget, detail string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per confirmed retirement.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		op, err := uow.Operations().Get(ctx, opID)
		if err != nil {
			return err
		}
		if op.State == OperationPending || op.State == OperationReconciling {
			op.State = OperationSucceeded
			op.Outcome = paneCloseOutcome{AbsenceObserved: true, Detail: detail}
			op.UpdatedAt = now
			if saveErr := uow.Operations().Save(ctx, op); saveErr != nil {
				return saveErr
			}
		}

		if target.SessionID == "" {
			return nil
		}
		binding, found, err := uow.Bindings().Current(ctx, target.SessionID)
		if err != nil {
			return err
		}
		if !found || binding.IncarnationID != target.IncarnationID {
			return nil
		}
		superseded, supersedeErr := binding.Supersede("pane close retirement: "+detail, now)
		if supersedeErr != nil {
			return supersedeErr
		}
		return uow.Bindings().Save(ctx, superseded)
	})
}

// stopReserved interrupts an attempt that never launched: a reserved
// attempt is never launched (section 5), so there is no act to perform.
func (c *Controller) stopReserved(ctx context.Context, handle RunHandle, detail RunDetail) (StopReport, error) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; this runs once per stop round, never a hot loop.
	now := c.Clock.Now()
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		generation := gen(handle.lease.Generation)

		t, tRev, getErr := uow.Tasks().Get(ctx, detail.TaskID)
		if getErr != nil {
			return getErr
		}
		tFrom := t.State
		t, interruptErr := t.Interrupt(now)
		if interruptErr != nil {
			return interruptErr
		}
		if _, saveErr := uow.Tasks().Save(ctx, t, tRev); saveErr != nil {
			return saveErr
		}
		if transErr := recordTransition(ctx, uow, EntityTask, detail.TaskID.String(), string(tFrom), string(t.State), "stop before launch", generation, now); transErr != nil {
			return transErr
		}

		a, aRev, getErr := uow.Attempts().Get(ctx, detail.AttemptID)
		if getErr != nil {
			return getErr
		}
		aFrom := a.State
		a, interruptErr = a.Interrupt(now)
		if interruptErr != nil {
			return interruptErr
		}
		if _, saveErr := uow.Attempts().Save(ctx, a, aRev); saveErr != nil {
			return saveErr
		}
		if transErr := recordTransition(ctx, uow, EntityAttempt, detail.AttemptID.String(), string(aFrom), string(a.State), "stop before launch", generation, now); transErr != nil {
			return transErr
		}

		if detail.SessionID != "" {
			if transErr := terminateSession(ctx, uow, detail.SessionID, "stop before launch", generation, now); transErr != nil {
				return transErr
			}
		}

		return markRunStopped(ctx, uow, handle.runID, generation, now)
	})
	if err != nil {
		return StopReport{}, fmt.Errorf("app: stop reserved attempt: %w", err)
	}
	return StopReport{RunState: string(run.RunStopped), Terminated: true}, nil
}

// retireGroup lists pgid through ProcessGroupInspector and classifies it
// against the expected argvs, signaling the group only when matched, after
// pre-dispatch revalidation. A signal failure is returned, never
// discarded: the caller records it as reconciliation evidence.
func (c *Controller) retireGroup(ctx context.Context, handle RunHandle, pgid int, expectedArgvs [][]string) (GroupRetirementOutcome, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per retirement round.
	processes, err := c.Groups.GroupProcesses(ctx, pgid)
	outcome := ClassifyGroupRetirement(processes, err, expectedArgvs)
	if outcome != GroupMatched {
		return outcome, nil
	}
	if err := c.revalidateForDispatch(ctx, handle, true); err != nil {
		return outcome, err
	}
	if err := c.Groups.SignalGroup(ctx, pgid); err != nil {
		return outcome, fmt.Errorf("app: signal check process group %d: %w", pgid, err)
	}
	return outcome, nil
}

// finishStop interrupts the run's task, attempt and session (whichever are
// not already terminal) and marks the run stopped, in one transaction, once
// their owned work has been confirmed terminated by the caller.
func (c *Controller) finishStop(ctx context.Context, handle RunHandle, detail RunDetail, reason string) (StopReport, error) { //nolint:gocritic // hugeParam: RunHandle and RunDetail are per-call DTOs; this runs once per stop completion.
	now := c.Clock.Now()
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		generation := gen(handle.lease.Generation)

		a, aRev, getErr := uow.Attempts().Get(ctx, detail.AttemptID)
		if getErr != nil {
			return getErr
		}
		if a.State != run.AttemptInterrupted && a.State != run.AttemptFailed && a.State != run.AttemptCompleted {
			aFrom := a.State
			nextAttempt, interruptErr := a.Interrupt(now)
			if interruptErr != nil {
				return interruptErr
			}
			if _, saveErr := uow.Attempts().Save(ctx, nextAttempt, aRev); saveErr != nil {
				return saveErr
			}
			if transErr := recordTransition(ctx, uow, EntityAttempt, detail.AttemptID.String(), string(aFrom), string(nextAttempt.State), reason, generation, now); transErr != nil {
				return transErr
			}
		}

		t, tRev, getErr := uow.Tasks().Get(ctx, detail.TaskID)
		if getErr != nil {
			return getErr
		}
		if t.State != run.TaskInterrupted && t.State != run.TaskFailed && t.State != run.TaskCompleted {
			tFrom := t.State
			nextTask, interruptErr := t.Interrupt(now)
			if interruptErr != nil {
				return interruptErr
			}
			if _, saveErr := uow.Tasks().Save(ctx, nextTask, tRev); saveErr != nil {
				return saveErr
			}
			if transErr := recordTransition(ctx, uow, EntityTask, detail.TaskID.String(), string(tFrom), string(nextTask.State), reason, generation, now); transErr != nil {
				return transErr
			}
		}

		if detail.SessionID != "" {
			if transErr := terminateSession(ctx, uow, detail.SessionID, reason, generation, now); transErr != nil {
				return transErr
			}
		}

		return markRunStopped(ctx, uow, handle.runID, generation, now)
	})
	if err != nil {
		return StopReport{}, fmt.Errorf("app: finish stop: %w", err)
	}
	return StopReport{RunState: string(run.RunStopped), Terminated: true}, nil
}

// markRunStopped moves run to stopped and records the transition.
func markRunStopped(ctx context.Context, uow UnitOfWork, runID identity.RunID, generation *int64, now time.Time) error {
	r, rRev, err := uow.Runs().Get(ctx, runID)
	if err != nil {
		return err
	}
	rFrom := r.State
	r, err = r.MarkStopped(now)
	if err != nil {
		return err
	}
	if _, err := uow.Runs().Save(ctx, r, rRev); err != nil {
		return err
	}
	return recordTransition(ctx, uow, EntityRun, runID.String(), string(rFrom), string(r.State), "owned work terminated", generation, now)
}

// terminateSession moves session to terminated when it is not already, and
// records the transition.
func terminateSession(ctx context.Context, uow UnitOfWork, sessionID identity.SessionID, reason string, generation *int64, now time.Time) error {
	s, sRev, err := uow.Sessions().Get(ctx, sessionID)
	if err != nil {
		return err
	}
	if s.State == run.SessionTerminated {
		return nil
	}
	// Terminate is invalid directly from active (section 5: "a stop against
	// a corroborated live process goes through launching → stopping" first).
	// reserved, launching, stopping and reconciling all terminate directly.
	if s.State == run.SessionActive {
		stoppingFrom := s.State
		s, err = s.Stop(now)
		if err != nil {
			return err
		}
		sRev, err = uow.Sessions().Save(ctx, s, sRev)
		if err != nil {
			return err
		}
		if transErr := recordTransition(ctx, uow, EntitySession, sessionID.String(), string(stoppingFrom), string(s.State), reason, generation, now); transErr != nil {
			return transErr
		}
	}

	sFrom := s.State
	s, err = s.Terminate(now)
	if err != nil {
		return err
	}
	if _, err := uow.Sessions().Save(ctx, s, sRev); err != nil {
		return err
	}
	return recordTransition(ctx, uow, EntitySession, sessionID.String(), string(sFrom), string(s.State), reason, generation, now)
}
