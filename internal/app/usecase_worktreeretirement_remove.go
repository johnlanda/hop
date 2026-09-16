package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// Worktree removal (docs/plan/phase-3-worktree-retirement.md sections 4
// and 7, decisions in section 13): for each active worktree row of a
// merged run, provenance first, then the read-only pre-act inspection,
// then either a final decision journaled once as a settled OpWorktreeRetire
// operation, a retained report journaling nothing, or one claimed
// `git worktree remove` execution without force.

// worktreeRetireDecision is what an OpWorktreeRetire operation records
// having decided before any act.
type worktreeRetireDecision string

const (
	// retireDecisionRemove is a claimed removal act.
	retireDecisionRemove worktreeRetireDecision = "remove"
	// retireDecisionAbsent settles a checkout already gone, with no act.
	retireDecisionAbsent worktreeRetireDecision = "absent"
	// retireDecisionReleased settles a checkout HOP relinquishes, with no
	// act.
	retireDecisionReleased worktreeRetireDecision = "released"
)

// worktreeRetireResult is an OpWorktreeRetire outcome's result.
type worktreeRetireResult string

const (
	retireResultRemoved       worktreeRetireResult = "removed"
	retireResultAbsent        worktreeRetireResult = "absent"
	retireResultReleased      worktreeRetireResult = "released"
	retireResultRefused       worktreeRetireResult = "refused"
	retireResultIncomplete    worktreeRetireResult = "incomplete"
	retireResultInterrupted   worktreeRetireResult = "interrupted"
	retireResultNeverExecuted worktreeRetireResult = "never-executed"
)

// worktreeRetireIntent is the OpWorktreeRetire intent payload. A removal
// act carries the frozen exec shape (argv, cwd) and its hop check-exec
// invocation; a settled decision carries neither and is never claimed.
type worktreeRetireIntent struct {
	worktreeRetirementExecIntent
	Decision       worktreeRetireDecision `json:"decision"`
	WorktreeID     identity.WorktreeID    `json:"worktree_id"`
	AttemptID      identity.AttemptID     `json:"attempt_id,omitempty"`
	RepositoryRoot string                 `json:"repository_root"`
	RecordedPath   string                 `json:"recorded_path"`
	ListedPath     string                 `json:"listed_path,omitempty"`
	// Branch is the row's recorded branch name, as stored.
	Branch  string `json:"branch,omitempty"`
	BaseOID string `json:"base_oid,omitempty"`
	// WasAbsent records a listed checkout whose directory was already gone
	// before the act: a completed removal settles absent.
	WasAbsent bool `json:"was_absent,omitempty"`
	// PreCheck is the pre-act inspection's deciding observation.
	PreCheck  string   `json:"pre_check"`
	SpawnArgv []string `json:"spawn_argv,omitempty"`
}

// worktreeRetireOutcome is the OpWorktreeRetire outcome payload.
type worktreeRetireOutcome struct {
	Result   worktreeRetireResult     `json:"result"`
	Released WorktreeReleaseReason    `json:"released_reason,omitempty"`
	Retained WorktreeRetainedCategory `json:"retained_category,omitempty"`
	ExitCode int                      `json:"exit_code"`
	Detail   string                   `json:"detail,omitempty"`
	// StdoutPath and StderrPath name the act's retained output, when a
	// failed act retained it.
	StdoutPath string `json:"stdout_path,omitempty"`
	StderrPath string `json:"stderr_path,omitempty"`
}

// retirementRowOutcome is what one pass concluded for one worktree row.
type retirementRowOutcome string

const (
	// rowRemoved, rowAbsent and rowReleased moved the row to that final
	// state in this pass.
	rowRemoved  retirementRowOutcome = "removed"
	rowAbsent   retirementRowOutcome = "absent"
	rowReleased retirementRowOutcome = "released"
	// rowRetained keeps the checkout for now; it is examined again next
	// pass.
	rowRetained retirementRowOutcome = "retained"
	// rowIncomplete: the act left git's entry behind with the directory
	// gone; the next pass completes the removal.
	rowIncomplete retirementRowOutcome = "incomplete"
	// rowUnresolved: the act's outcome was not observed; the next pass's
	// recovery settles it.
	rowUnresolved retirementRowOutcome = "unresolved"
	// rowNotDispatched: the pass lost its lease or the run's eligibility
	// before the act; nothing was spawned.
	rowNotDispatched retirementRowOutcome = "not-dispatched"
)

// retirementRowReport is one row's line in a pass report.
type retirementRowReport struct {
	WorktreeID identity.WorktreeID
	Branch     string
	Path       string
	Outcome    retirementRowOutcome
	Retained   WorktreeRetainedCategory
	Released   WorktreeReleaseReason
	// LeftOnDisk marks a release after an act that did not remove the
	// directory: it stays on disk, no longer managed by HOP.
	LeftOnDisk bool
	// EvidencePath names a failed act's retained stderr.
	EvidencePath string
	ExitCode     int
	Detail       string
}

// errRetirementPassHalted stops a removal pass after an act could not be
// dispatched: the pass lost its lease or the run its eligibility, and
// every later act would be refused the same way.
var errRetirementPassHalted = errors.New("app: the worktree-retirement pass halted before an act")

// retirementRows reads the pass's inputs under the lease: every worktree
// row of the run, the run's worktree.create operations, and its
// worktree.retire operations.
func (c *Controller) retirementRows(ctx context.Context, handle RunHandle) (rows []RetirementWorktree, creates, retires []Operation, err error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per pass step.
	err = c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		repos, reqErr := RequireWorktreeRetirementRepositories(uow, "worktree retirement")
		if reqErr != nil {
			return reqErr
		}
		var readErr error
		if rows, readErr = repos.WorktreesForRetirement(ctx, handle.runID); readErr != nil {
			return readErr
		}
		if creates, readErr = uow.Operations().ByKind(ctx, handle.runID, OpWorktreeCreate); readErr != nil {
			return readErr
		}
		retires, readErr = uow.Operations().ByKind(ctx, handle.runID, OpWorktreeRetire)
		return readErr
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("app: read the run's worktrees for retirement: %w", err)
	}
	return rows, creates, retires, nil
}

// removeRunWorktrees runs the removal step over every active worktree row
// of a merged run, oldest first. A row whose act could not be dispatched
// halts the pass: its report says so and the remaining rows are left for
// the next pass.
func (c *Controller) removeRunWorktrees(ctx context.Context, handle RunHandle, frozen *FrozenRun, opts *retirementPassOptions) ([]retirementRowReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per pass.
	rows, creates, retires, err := c.retirementRows(ctx, handle)
	if err != nil {
		return nil, err
	}
	var reports []retirementRowReport
	for i := range rows {
		if rows[i].Worktree.State != run.WorktreeActive {
			continue
		}
		report, rowErr := c.retireWorktreeRow(ctx, handle, frozen, opts, &rows[i].Worktree, creates, retires)
		reports = append(reports, report)
		switch {
		case errors.Is(rowErr, errRetirementPassHalted):
			return reports, nil
		case rowErr != nil:
			return reports, rowErr
		}
	}
	return reports, nil
}

// retireWorktreeRow decides one active row.
func (c *Controller) retireWorktreeRow(ctx context.Context, handle RunHandle, frozen *FrozenRun, opts *retirementPassOptions, row *run.Worktree, creates, retires []Operation) (retirementRowReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per row.
	report := retirementRowReport{WorktreeID: row.ID, Branch: row.Branch, Path: row.Path}
	candidate, unverified, ok := verifyRetirementCandidate(row, creates, frozen.RepositoryRoot)
	if !ok {
		intent := retireDecisionIntent(row, frozen.RepositoryRoot, retireDecisionReleased, "", unverified)
		outcome := worktreeRetireOutcome{Result: retireResultReleased, Released: ReleasedUnverified, ExitCode: -1, Detail: unverified}
		return c.settleRetireDecision(ctx, handle, row.ID, &intent, &outcome, report)
	}

	verdict := c.inspectAttemptCheckout(ctx, &candidate, opts.InspectPath)
	switch verdict.Disposition {
	case checkoutAbsent:
		intent := retireDecisionIntent(row, frozen.RepositoryRoot, retireDecisionAbsent, verdict.ListedPath, verdict.Detail)
		outcome := worktreeRetireOutcome{Result: retireResultAbsent, ExitCode: -1, Detail: verdict.Detail}
		return c.settleRetireDecision(ctx, handle, row.ID, &intent, &outcome, report)
	case checkoutReleased:
		intent := retireDecisionIntent(row, frozen.RepositoryRoot, retireDecisionReleased, verdict.ListedPath, verdict.Detail)
		outcome := worktreeRetireOutcome{Result: retireResultReleased, Released: verdict.Released, ExitCode: -1, Detail: verdict.Detail}
		return c.settleRetireDecision(ctx, handle, row.ID, &intent, &outcome, report)
	case checkoutRetained:
		report.Outcome, report.Retained, report.Detail = rowRetained, verdict.Retained, verdict.Detail
		if verdict.Retained == RetainedUncommittedChanges && latestRetireInterrupted(retires, row.ID) {
			report.Retained = RetainedInterruptedRemoval
		}
		return report, nil
	case checkoutRemovable:
		return c.removeAttemptCheckout(ctx, handle, frozen, opts, row, &candidate, &verdict)
	}
	return report, fmt.Errorf("app: unknown checkout disposition %q", verdict.Disposition)
}

// retireDecisionIntent is a settled decision's intent: the row's recorded
// facts and the deciding observation, with no exec shape.
func retireDecisionIntent(row *run.Worktree, repositoryRoot string, decision worktreeRetireDecision, listedPath, preCheck string) worktreeRetireIntent {
	return worktreeRetireIntent{
		Decision: decision, WorktreeID: row.ID, AttemptID: row.AttemptID,
		RepositoryRoot: repositoryRoot, RecordedPath: row.Path, ListedPath: listedPath,
		Branch: row.Branch, BaseOID: row.BaseCommit, PreCheck: preCheck,
	}
}

// latestRetireInterrupted reports whether the row's latest settled
// worktree.retire operation — the newest generation, then the newest
// creation — recorded an interrupted act.
func latestRetireInterrupted(retires []Operation, worktreeID identity.WorktreeID) bool {
	var latest *Operation
	for i := range retires {
		op := &retires[i]
		if op.State != OperationSucceeded && op.State != OperationFailed {
			continue
		}
		intent, ok := decodeOperationPayload[worktreeRetireIntent](op.Intent)
		if !ok || intent.WorktreeID != worktreeID {
			continue
		}
		if latest == nil || op.Generation > latest.Generation ||
			(op.Generation == latest.Generation && op.CreatedAt.After(latest.CreatedAt)) {
			latest = op
		}
	}
	if latest == nil {
		return false
	}
	outcome, ok := decodeOperationPayload[worktreeRetireOutcome](latest.Outcome)
	return ok && outcome.Result == retireResultInterrupted
}

// settleRetireDecision journals a final decision as one settled
// worktree.retire operation and moves the row to the matching final state
// in the same unit of work.
func (c *Controller) settleRetireDecision(ctx context.Context, handle RunHandle, worktreeID identity.WorktreeID, intent *worktreeRetireIntent, outcome *worktreeRetireOutcome, report retirementRowReport) (retirementRowReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; report is a small per-row value.
	opID, err := c.newOperationID()
	if err != nil {
		return report, err
	}
	target, rowOutcome := run.WorktreeReleased, rowReleased
	if outcome.Result == retireResultAbsent {
		target, rowOutcome = run.WorktreeAbsent, rowAbsent
	}
	now := c.Clock.Now()
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		if saveErr := retireRowLocked(ctx, uow, worktreeID, target); saveErr != nil {
			return saveErr
		}
		return uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpWorktreeRetire, State: OperationSucceeded, Intent: *intent, Outcome: *outcome,
			CreatedAt: now, UpdatedAt: now,
		})
	}); err != nil {
		return report, fmt.Errorf("app: record the worktree.retire decision: %w", err)
	}
	report.Outcome, report.Released, report.Detail = rowOutcome, outcome.Released, outcome.Detail
	return report, nil
}

// retireRowLocked moves an active worktree row to a final state inside the
// caller's unit of work.
func retireRowLocked(ctx context.Context, uow UnitOfWork, worktreeID identity.WorktreeID, target run.WorktreeState) error {
	row, revision, err := uow.Worktrees().Get(ctx, worktreeID)
	if err != nil {
		return err
	}
	retired, err := row.Retire(target)
	if err != nil {
		return err
	}
	_, err = uow.Worktrees().Save(ctx, retired, revision)
	return err
}

// removeAttemptCheckout performs one claimed removal act: the intent, the
// dispatch revalidation, the hop check-exec spawn of the frozen no-force
// `git worktree remove`, then the post-act observation deciding the
// outcome.
func (c *Controller) removeAttemptCheckout(ctx context.Context, handle RunHandle, frozen *FrozenRun, opts *retirementPassOptions, row *run.Worktree, candidate *attemptCheckout, verdict *checkoutVerdict) (retirementRowReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per act.
	report := retirementRowReport{WorktreeID: row.ID, Branch: row.Branch, Path: row.Path}
	if !filepath.IsAbs(opts.HOPPath) {
		return report, errors.New("app: the hop executable path is not absolute")
	}
	if !filepath.IsAbs(c.GitExecutable) {
		return report, errRetirementGitUnconfigured
	}
	opID, err := c.newOperationID()
	if err != nil {
		return report, err
	}
	root := frozen.RepositoryRoot
	argv := worktreeRetireArgv(c.GitExecutable, root, verdict.ListedPath)
	intent := retireDecisionIntent(row, root, retireDecisionRemove, verdict.ListedPath, verdict.Detail)
	intent.worktreeRetirementExecIntent = worktreeRetirementExecIntent{Argv: argv, Cwd: root}
	intent.WasAbsent = verdict.WasAbsent
	intent.SpawnArgv = append([]string{opts.HOPPath, "check-exec", "--op", opID.String(), "--"}, argv...)
	now := c.Clock.Now()
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpWorktreeRetire, State: OperationPending, Intent: intent,
			CreatedAt: now, UpdatedAt: now,
		})
	}); err != nil {
		return report, fmt.Errorf("app: record worktree.retire intent: %w", err)
	}
	if err := c.revalidateRetirementDispatch(ctx, handle); err != nil {
		report.Outcome, report.Detail = rowNotDispatched, "the pass lost its lease or the run's eligibility before the removal was dispatched"
		return report, errRetirementPassHalted
	}

	actCtx, release := handle.actContext(ctx)
	boundedCtx, cancel := context.WithTimeout(actCtx, retirementActTimeout)
	cmdResult, runErr := c.Commands.Run(boundedCtx, Command{Argv: intent.SpawnArgv, Dir: root, Env: retirementSpawnEnv(opts.SpawnEnv, frozen.Snapshot.StateRoot)})
	cancel()
	release()

	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), checkPersistenceTimeout)
	defer persistCancel()
	if runErr != nil {
		if markErr := c.markOperationReconciling(persistCtx, handle, opID, "the removal's exit status was not observed; the next pass recovers it"); markErr != nil {
			return report, markErr
		}
		report.Outcome, report.Detail = rowUnresolved, "the removal's exit status was not observed; the next pass recovers it"
		return report, nil
	}
	listed, exists, observed := c.observeCheckout(persistCtx, candidate, opts.InspectPath)
	if !observed {
		if markErr := c.markOperationReconciling(persistCtx, handle, opID, "the checkout could not be observed after the removal; the next pass recovers it"); markErr != nil {
			return report, markErr
		}
		report.Outcome, report.Detail, report.ExitCode = rowUnresolved, "the checkout could not be observed after the removal; the next pass recovers it", cmdResult.ExitCode
		return report, nil
	}
	outcome := worktreeRetireOutcome{ExitCode: cmdResult.ExitCode}
	state, target := OperationFailed, run.WorktreeState("")
	switch {
	case !listed && !exists:
		state, target = OperationSucceeded, run.WorktreeRemoved
		outcome.Result, report.Outcome = retireResultRemoved, rowRemoved
		if verdict.WasAbsent {
			target, outcome.Result, report.Outcome = run.WorktreeAbsent, retireResultAbsent, rowAbsent
		}
	case listed && exists:
		category := RetainedRemoveRefused
		switch again := c.inspectAttemptCheckout(persistCtx, candidate, opts.InspectPath); again.Retained {
		case RetainedUncommittedChanges, RetainedHiddenChanges, RetainedLocked:
			category = again.Retained
		default:
		}
		outcome.Result, outcome.Retained = retireResultRefused, category
		outcome.Detail = fmt.Sprintf("git worktree remove exited %d and the checkout is still listed", cmdResult.ExitCode)
		report.Outcome, report.Retained = rowRetained, category
	case listed:
		outcome.Result, report.Outcome = retireResultIncomplete, rowIncomplete
		outcome.Detail = "the directory is gone but git still lists the worktree; the next pass completes the removal"
	default:
		target = run.WorktreeReleased
		outcome.Result, outcome.Released = retireResultReleased, ReleasedNotRegistered
		outcome.Detail = "git no longer lists the worktree but the directory is still on disk"
		report.Outcome, report.Released, report.LeftOnDisk = rowReleased, ReleasedNotRegistered, true
	}
	if state != OperationSucceeded {
		stdoutPath, stderrPath, retainErr := c.retainRetireOutput(persistCtx, frozen.Snapshot.StateRoot, handle.runID, opID, cmdResult)
		outcome.StdoutPath, outcome.StderrPath = stdoutPath, stderrPath
		if retainErr != nil {
			outcome.Detail += "; the removal's output could not be retained"
		}
		report.EvidencePath = stderrPath
	}
	report.ExitCode, report.Detail = cmdResult.ExitCode, outcome.Detail
	if err := c.settleRetireAct(persistCtx, handle, opID, row.ID, state, target, &outcome); err != nil {
		report.Outcome, report.Detail = rowUnresolved, "the removal's outcome could not be recorded; the next pass recovers it"
		return report, err
	}
	return report, nil
}

// retirementOpDir is a worktree-retirement operation's evidence directory.
func retirementOpDir(stateRoot string, runID identity.RunID, opID identity.OperationID) string {
	return filepath.Join(stateRoot, "runs", runID.String(), "retirement", opID.String())
}

// retainRetireOutput writes a failed removal act's stdout and stderr under
// the operation's evidence directory, returning the paths it wrote.
func (c *Controller) retainRetireOutput(ctx context.Context, stateRoot string, runID identity.RunID, opID identity.OperationID, result CommandResult) (stdoutPath, stderrPath string, err error) {
	base := retirementOpDir(stateRoot, runID, opID)
	var errs []error
	if writeErr := c.Artifacts.WriteArtifact(ctx, filepath.Join(base, "stdout"), result.Stdout); writeErr != nil {
		errs = append(errs, writeErr)
	} else {
		stdoutPath = filepath.Join(base, "stdout")
	}
	if writeErr := c.Artifacts.WriteArtifact(ctx, filepath.Join(base, "stderr"), result.Stderr); writeErr != nil {
		errs = append(errs, writeErr)
	} else {
		stderrPath = filepath.Join(base, "stderr")
	}
	return stdoutPath, stderrPath, errors.Join(errs...)
}

// settleRetireAct records a removal act's outcome and, when target names a
// final state, moves the row there in the same unit of work. An operation
// already settled is left alone.
func (c *Controller) settleRetireAct(ctx context.Context, handle RunHandle, opID identity.OperationID, worktreeID identity.WorktreeID, state OperationState, target run.WorktreeState, outcome *worktreeRetireOutcome) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per act.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		op, err := uow.Operations().Get(ctx, opID)
		if err != nil {
			return err
		}
		if op.State != OperationPending && op.State != OperationReconciling {
			return nil
		}
		if target != "" {
			if saveErr := retireRowLocked(ctx, uow, worktreeID, target); saveErr != nil {
				return saveErr
			}
		}
		op.State = state
		op.Outcome = *outcome
		op.UpdatedAt = now
		return uow.Operations().Save(ctx, op)
	})
}
