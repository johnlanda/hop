package app

import (
	"context"
	"fmt"
	"sort"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// CompletionReport is one DriveCompletion round's outcome.
type CompletionReport struct {
	// Ready reports EvaluateReadiness's verdict at this round's entry.
	Ready bool
	// Missing renders the guard shortfalls, one kind per entry.
	Missing []string
	// RunState is the run's state after the round.
	RunState string
	// Outstanding names completion retirement still awaiting observed
	// termination.
	Outstanding []string
	// Completed is true once the final transaction recorded completed.
	Completed bool
}

// guardEvidence is the application-assembled readiness evidence: the
// current integration head's object IDs resolved once, outside any
// transaction, so every in-transaction evaluation compares against the
// same candidate.
type guardEvidence struct {
	HeadCommitOID string
	HeadTreeOID   string
}

// resolveGuardHead resolves the current integration head's commit and
// tree object IDs: the commit is OBSERVED from the live integration ref
// (outside any transaction, like every git observation), then validated
// against the journal — the head is guard evidence only when an
// INTEGRATED integration row records it as its merge candidate. A no-op
// merge records the unchanged head as its candidate, so repeated no-ops
// at one head each vouch for it; any inference from the recorded chain
// alone would see such rows consume each other. A run whose ref cannot
// be read, or whose observed head no integrated row vouches for (a
// published-but-unsettled candidate, a half-done rollback), has no
// guard head and every head-bound guard reports its shortfall — the
// guards fail closed, never open. evaluateReadinessLocked re-verifies
// the row match inside each guard transaction.
func (c *Controller) resolveGuardHead(ctx context.Context, handle RunHandle, frozen *FrozenRun) (guardEvidence, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	observed, obsErr := c.runGit(ctx, frozen.RepositoryRoot, "rev-parse", "--verify", integrationRef(frozen.Snapshot.Workflow.IntegrationBranch))
	if obsErr != nil || observed == "" {
		return guardEvidence{}, nil // no readable head: head-bound guards report their shortfall.
	}
	var vouched bool
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "resolve guard head")
		if wfErr != nil {
			return wfErr
		}
		var vErr error
		vouched, vErr = integratedRowVouchesFor(ctx, wf, handle.runID, observed)
		return vErr
	}); err != nil {
		return guardEvidence{}, err
	}
	if !vouched {
		return guardEvidence{}, nil
	}
	tree, err := c.runGit(ctx, frozen.RepositoryRoot, "rev-parse", observed+"^{tree}")
	if err != nil {
		return guardEvidence{}, fmt.Errorf("app: resolve guard head tree: %w", err)
	}
	return guardEvidence{HeadCommitOID: observed, HeadTreeOID: tree}, nil
}

// integratedRowVouchesFor reports whether an INTEGRATED integration row
// of the run records head as its merge candidate — the journal-side
// half of guard-head resolution.
func integratedRowVouchesFor(ctx context.Context, wf WorkflowRepositories, runID identity.RunID, head string) (bool, error) {
	tasks, err := wf.TaskIndex().ByRun(ctx, runID)
	if err != nil {
		return false, err
	}
	for i := range tasks {
		integrations, intErr := wf.Integrations().ByTask(ctx, tasks[i].ID)
		if intErr != nil {
			return false, intErr
		}
		for j := range integrations {
			if integrations[j].State == run.IntegrationIntegrated && integrations[j].MergeCommitOID == head {
				return true, nil
			}
		}
	}
	return false, nil
}

// evaluateReadinessLocked assembles the GuardContext from the caller's
// transaction and evaluates it: the plan flag, every implement task, the
// pre-resolved head, the most recent combined-check receipt for that
// exact head, and the most recently accepted review verdict. Guards are
// application behavior over evidence rows only — no verb writes any of
// these except its own pipeline.
func evaluateReadinessLocked(ctx context.Context, uow UnitOfWork, wf WorkflowRepositories, handle RunHandle, head guardEvidence) (bool, []run.GuardShortfall, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	// Re-verify the pre-resolved head against THIS transaction's journal:
	// evidence rows may have moved since the caller observed the ref, and
	// a head no integrated row vouches for is no head at all.
	if head.HeadCommitOID != "" {
		vouched, vErr := integratedRowVouchesFor(ctx, wf, handle.runID, head.HeadCommitOID)
		if vErr != nil {
			return false, nil, vErr
		}
		if !vouched {
			head = guardEvidence{}
		}
	}
	r, _, err := uow.Runs().Get(ctx, handle.runID)
	if err != nil {
		return false, nil, err
	}
	tasks, err := wf.TaskIndex().ByRun(ctx, handle.runID)
	if err != nil {
		return false, nil, err
	}
	var implement []run.Task
	for i := range tasks {
		if tasks[i].Kind == run.TaskKindImplement {
			implement = append(implement, tasks[i])
		}
	}
	guardCtx := run.GuardContext{
		PlanClosed:     r.PlanClosed,
		ImplementTasks: implement,
		HeadCommitOID:  head.HeadCommitOID,
		HeadTreeOID:    head.HeadTreeOID,
	}
	if head.HeadCommitOID != "" {
		receipt, found, recErr := settledIntegrationCheckReceiptLocked(ctx, uow, handle, head.HeadCommitOID, head.HeadTreeOID)
		if recErr != nil {
			return false, nil, recErr
		}
		if found {
			guardCtx.LatestCheck = &receipt
		}
	}
	latest, revErr := wf.Reviews().Latest(ctx, handle.runID)
	if revErr != nil {
		return false, nil, revErr
	}
	guardCtx.LatestReview = latest
	ready, missing := run.EvaluateReadiness(guardCtx)
	return ready, missing, nil
}

// settledIntegrationCheckReceiptLocked is settledIntegrationCheckReceipt
// inside the caller's transaction.
func settledIntegrationCheckReceiptLocked(ctx context.Context, uow UnitOfWork, handle RunHandle, subjectCommit, subjectTree string) (run.CheckReceipt, bool, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	ops, err := uow.Operations().ByKind(ctx, handle.runID, OpCheckRun)
	if err != nil {
		return run.CheckReceipt{}, false, err
	}
	for i := range ops {
		if ops[i].State != OperationSucceeded && ops[i].State != OperationFailed {
			continue
		}
		intent, ok := decodeOperationPayload[integrationCheckIntent](ops[i].Intent)
		if !ok || intent.IntegrationID == "" {
			continue
		}
		if intent.SubjectCommitOID != subjectCommit || intent.SubjectTreeOID != subjectTree {
			continue
		}
		outcome, ok := decodeOperationPayload[checkRunOutcome](ops[i].Outcome)
		if !ok {
			continue
		}
		passed, usable := usableCheckReceipt(ops[i].State, &outcome)
		if !usable {
			continue
		}
		return run.CheckReceipt{Passed: passed, SubjectCommitOID: subjectCommit, SubjectTreeOID: subjectTree}, true, nil
	}
	return run.CheckReceipt{}, false, nil
}

// EvaluateRunReadiness evaluates the section 8 completion guard for
// handle's run without changing anything: the readiness verdict and the
// rendered shortfall list.
func (c *Controller) EvaluateRunReadiness(ctx context.Context, handle RunHandle) (bool, []string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	frozen, err := c.Read.LoadFrozenRun(ctx, handle.runID)
	if err != nil {
		return false, nil, fmt.Errorf("app: load frozen run: %w", err)
	}
	if !frozen.Snapshot.Workflow.Feature() {
		return false, nil, fmt.Errorf("app: run %s is not a feature-mode run", handle.runID)
	}
	head, err := c.resolveGuardHead(ctx, handle, &frozen)
	if err != nil {
		return false, nil, err
	}
	var (
		ready   bool
		missing []run.GuardShortfall
	)
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "EvaluateRunReadiness")
		if wfErr != nil {
			return wfErr
		}
		var evalErr error
		ready, missing, evalErr = evaluateReadinessLocked(ctx, uow, wf, handle, head)
		return evalErr
	}); err != nil {
		return false, nil, err
	}
	return ready, renderShortfalls(missing), nil
}

// renderShortfalls renders guard shortfalls as strings.
func renderShortfalls(missing []run.GuardShortfall) []string {
	var out []string
	for _, shortfall := range missing {
		s := string(shortfall.Kind)
		if shortfall.TaskID != "" {
			s += ":" + shortfall.TaskID.String()
		}
		out = append(out, s)
	}
	return out
}

// EnsureReviewTask creates the run's review task when it is due: plan
// closed, every implement task integrated, and no review task exists for
// the current head (section 8 — only the controller creates review
// tasks, with the frozen subject in the row; a rejected candidate's
// superseded head gets its own NEW review task once a fix task moves the
// head). created reports whether this call created one.
func (c *Controller) EnsureReviewTask(ctx context.Context, handle RunHandle) (bool, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per scheduling pass.
	frozen, err := c.Read.LoadFrozenRun(ctx, handle.runID)
	if err != nil {
		return false, fmt.Errorf("app: load frozen run: %w", err)
	}
	if !frozen.Snapshot.Workflow.Feature() {
		return false, nil
	}
	head, err := c.resolveGuardHead(ctx, handle, &frozen)
	if err != nil {
		return false, err
	}
	if head.HeadCommitOID == "" {
		return false, nil
	}
	taskID, err := identity.ParseTaskID(c.IDs.NewID())
	if err != nil {
		return false, fmt.Errorf("app: generate review task id: %w", err)
	}
	now := c.Clock.Now()
	created := false
	err = c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "EnsureReviewTask")
		if wfErr != nil {
			return wfErr
		}
		r, _, runErr := uow.Runs().Get(ctx, handle.runID)
		if runErr != nil {
			return runErr
		}
		if !r.PlanClosed || r.StopRequested || r.State != run.RunRunning {
			return nil
		}
		tasks, taskErr := wf.TaskIndex().ByRun(ctx, handle.runID)
		if taskErr != nil {
			return taskErr
		}
		maxSeq := 0
		for i := range tasks {
			if tasks[i].Seq > maxSeq {
				maxSeq = tasks[i].Seq
			}
			if tasks[i].Kind == run.TaskKindImplement && tasks[i].State != run.TaskIntegrated {
				return nil
			}
			if tasks[i].Kind == run.TaskKindReview && tasks[i].SubjectCommitOID == head.HeadCommitOID {
				return nil // a review task for the current head already exists, whatever its state.
			}
		}
		review := run.NewReviewTask(taskID, handle.runID, maxSeq+1, head.HeadCommitOID, head.HeadTreeOID, now)
		if _, createErr := wf.TaskIndex().Create(ctx, review); createErr != nil {
			return createErr
		}
		created = true
		return recordTransition(ctx, uow, EntityTask, taskID.String(), "", string(review.State), fmt.Sprintf("review task created for head %s", head.HeadCommitOID), gen(handle.lease.Generation), now)
	})
	return created, err
}

// DriveCompletion performs one round of the section 8 completion flow:
// the run leaves running for completing ONLY in the transaction that
// first establishes readiness; completion retirement then closes the
// manager and any straggling session under the close rule with
// scrollback captured first; and the final transaction RE-VALIDATES
// EvaluateReadiness against the then-current task set and plan flag
// before recording completed. Stop precedence applies in every
// transaction: a stop request yields to stop handling, never completed.
func (c *Controller) DriveCompletion(ctx context.Context, handle RunHandle) (CompletionReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per scheduling pass.
	frozen, err := c.Read.LoadFrozenRun(ctx, handle.runID)
	if err != nil {
		return CompletionReport{}, fmt.Errorf("app: load frozen run: %w", err)
	}
	if !frozen.Snapshot.Workflow.Feature() {
		return CompletionReport{}, nil
	}
	head, err := c.resolveGuardHead(ctx, handle, &frozen)
	if err != nil {
		return CompletionReport{}, err
	}

	report := CompletionReport{}
	// Entry transaction: establish readiness and enter completing.
	if entryErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "DriveCompletion")
		if wfErr != nil {
			return wfErr
		}
		ready, missing, evalErr := evaluateReadinessLocked(ctx, uow, wf, handle, head)
		if evalErr != nil {
			return evalErr
		}
		report.Ready = ready
		report.Missing = renderShortfalls(missing)
		r, rRev, runErr := uow.Runs().Get(ctx, handle.runID)
		if runErr != nil {
			return runErr
		}
		report.RunState = string(r.State)
		if !ready || r.State != run.RunRunning || r.StopRequested {
			return nil
		}
		rFrom := r.State
		next, trErr := r.EnterCompleting(c.Clock.Now())
		if trErr != nil {
			return trErr
		}
		if _, saveErr := uow.Runs().Save(ctx, next, rRev); saveErr != nil {
			return saveErr
		}
		report.RunState = string(next.State)
		return recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(rFrom), string(next.State), "readiness established; completion retirement begins", gen(handle.lease.Generation), c.Clock.Now())
	}); entryErr != nil {
		return report, entryErr
	}
	if report.RunState != string(run.RunCompleting) {
		return report, nil
	}

	// Completion retirement: the manager and any straggling session, under
	// the Phase 2 close rule with scrollback captured first.
	outstanding, err := c.retireRemainingSessions(ctx, handle)
	if err != nil {
		return report, err
	}
	if len(outstanding) > 0 {
		report.Outstanding = outstanding
		return report, nil
	}

	// Final transaction: re-validate readiness against the then-current
	// task set and plan flag, then record completed. Stop precedence:
	// never completed once a stop request is held (Run.Complete itself
	// also refuses).
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "completion re-validation")
		if wfErr != nil {
			return wfErr
		}
		r, rRev, runErr := uow.Runs().Get(ctx, handle.runID)
		if runErr != nil {
			return runErr
		}
		if r.State != run.RunCompleting {
			report.RunState = string(r.State)
			return nil
		}
		if r.StopRequested {
			report.RunState = string(r.State)
			return nil
		}
		ready, missing, evalErr := evaluateReadinessLocked(ctx, uow, wf, handle, head)
		if evalErr != nil {
			return evalErr
		}
		now := c.Clock.Now()
		if !ready {
			// Something un-satisfied the guard between the entry and the
			// final transaction: the run returns to running rather than
			// completing over stale evidence.
			report.Ready = false
			report.Missing = renderShortfalls(missing)
			rFrom := r.State
			next, trErr := r.MarkRunning(now)
			if trErr != nil {
				return trErr
			}
			if _, saveErr := uow.Runs().Save(ctx, next, rRev); saveErr != nil {
				return saveErr
			}
			report.RunState = string(next.State)
			return recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(rFrom), string(next.State), "completion re-validation failed; run returns to running", gen(handle.lease.Generation), now)
		}
		rFrom := r.State
		next, trErr := r.Complete(now)
		if trErr != nil {
			return trErr
		}
		if _, saveErr := uow.Runs().Save(ctx, next, rRev); saveErr != nil {
			return saveErr
		}
		report.RunState = string(next.State)
		report.Completed = true
		return recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(rFrom), string(next.State), "readiness re-validated after completion retirement", gen(handle.lease.Generation), now)
	}); err != nil {
		return report, err
	}
	return report, nil
}

// retireRemainingSessions retires every non-terminal session — the
// manager and any straggler — under the close rule, returning the
// outstanding items. Termination is observed, never declared.
func (c *Controller) retireRemainingSessions(ctx context.Context, handle RunHandle) ([]string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	detail, err := c.Read.LoadRunStatus(ctx, handle.runID)
	if err != nil {
		return nil, err
	}
	var live []run.Session
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "completion retirement")
		if wfErr != nil {
			return wfErr
		}
		sessions, sessErr := wf.SessionIndex().ByRun(ctx, handle.runID)
		if sessErr != nil {
			return sessErr
		}
		for i := range sessions {
			if sessions[i].State == run.SessionTerminated || sessions[i].State == run.SessionLost {
				continue
			}
			live = append(live, sessions[i])
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Slice(live, func(i, j int) bool { return live[i].ID < live[j].ID })

	var outstanding []string
	for i := range live {
		retired, still, err := c.retireChildSession(ctx, handle, detail, &retirementCandidate{Session: live[i], Boundary: "completion retirement"})
		if err != nil {
			return nil, err
		}
		if !retired {
			outstanding = append(outstanding, still)
		}
	}
	return outstanding, nil
}
