package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// Worktree-retirement detection (docs/plan/phase-3-worktree-retirement.md
// sections 2 and 7): whether the run's validated integration head is
// contained in the target branch's tip, decided by one claimed
// `merge-base --is-ancestor` execution over two frozen object ids and
// journaled as an OpRetirementCheck operation.

// retirementActTimeout bounds each claimed retirement execution.
const retirementActTimeout = 5 * time.Minute

// ancestryUnknown is the settled result of a check whose exit status was
// never observed (a controller died while it ran); a fresh check is always
// safe, since the answer depends only on immutable objects.
const ancestryUnknown ancestryResult = "unknown"

// ancestryNeverExecuted settles a check that never reached its claim.
const ancestryNeverExecuted ancestryResult = "never-executed"

// retirementCheckIntent is the OpRetirementCheck intent payload: the two
// object ids the check compares, the target ref the tip was read from, and
// the frozen exec shape.
type retirementCheckIntent struct {
	worktreeRetirementExecIntent
	HeadOID   string   `json:"head_oid"`
	TargetRef string   `json:"target_ref"`
	TargetOID string   `json:"target_oid"`
	SpawnArgv []string `json:"spawn_argv"`
}

// retirementCheckOutcome is the OpRetirementCheck outcome payload.
type retirementCheckOutcome struct {
	Result   ancestryResult `json:"result"`
	ExitCode int            `json:"exit_code"`
	Detail   string         `json:"detail,omitempty"`
}

// retirementDetectionState is what detection concluded for the run.
type retirementDetectionState string

const (
	// detectionMerged: the integration head is contained in the target;
	// removal may proceed.
	detectionMerged retirementDetectionState = "merged"
	// detectionNotMerged: not contained (or the target branch is gone).
	detectionNotMerged retirementDetectionState = "not-merged"
	// detectionNothingIntegrated: the run integrated no new content; it
	// never retires automatically.
	detectionNothingIntegrated retirementDetectionState = "nothing-integrated"
	// detectionHistoryMissing: the frozen root does not hold the run's
	// integration head (a moved or replaced repository).
	detectionHistoryMissing retirementDetectionState = "history-missing"
	// detectionFailed: the check ran and failed, or an observation could
	// not be made; retried when the target moves.
	detectionFailed retirementDetectionState = "failed"
	// detectionBlocked: an earlier retirement execution is unresolved.
	detectionBlocked retirementDetectionState = "blocked"
	// detectionInterrupted: the check's dispatch or outcome was lost; the
	// operation is left for the next pass's recovery.
	detectionInterrupted retirementDetectionState = "interrupted"
)

// retirementDetection is one detection step's result.
type retirementDetection struct {
	State  retirementDetectionState
	Head   string
	Target string
	Detail string
}

// retirementPassOptions are the composition inputs a pass needs.
type retirementPassOptions struct {
	HOPPath string
	// SpawnEnv is the invoking process's environment, sanitized under the
	// run's frozen policy (CheckSpawnEnvironment).
	SpawnEnv []string
	// InspectPath resolves recorded paths for the removal step.
	InspectPath PathInspector
	// beforeFirstRemoval, when set, is called once, immediately before the
	// pass's first removal spawn; announced records that it was.
	beforeFirstRemoval func()
	announced          bool
}

// announceRemoval calls beforeFirstRemoval the first time a pass is about
// to spawn a removal act.
func (o *retirementPassOptions) announceRemoval() {
	if o.announced {
		return
	}
	o.announced = true
	if o.beforeFirstRemoval != nil {
		o.beforeFirstRemoval()
	}
}

// detectForRetirement is the pass's detection step. The run's validated
// head and the repository-identity check come before anything else: a run
// that integrated nothing new, or whose frozen root no longer holds its
// history (a moved or replaced repository), is left untouched — no
// recovery, no check, nothing journaled. Unresolved retirement operations
// from earlier passes are recovered next, and any blocking one ends
// detection for this pass; then detectRetirementMerge runs.
func (c *Controller) detectForRetirement(ctx context.Context, handle RunHandle, frozen *FrozenRun, opts *retirementPassOptions) (retirementDetection, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per pass.
	head, stop, err := c.retirementHead(ctx, handle, frozen)
	if err != nil || stop.State != "" {
		return stop, err
	}
	blocking, err := c.recoverRetirementOperations(ctx, handle, frozen, opts)
	if err != nil {
		return retirementDetection{}, err
	}
	if blocking != "" {
		return retirementDetection{State: detectionBlocked, Head: head, Detail: blocking}, nil
	}
	return c.detectRetirementMerge(ctx, handle, frozen, opts, head)
}

// retirementHead reads the run's validated integration head and confirms
// the frozen root still holds it. A non-empty stop.State ends the pass:
// ambiguous heads fail closed, a run with no new content never retires,
// and a root without the head is a moved or replaced repository.
func (c *Controller) retirementHead(ctx context.Context, handle RunHandle, frozen *FrozenRun) (head string, stop retirementDetection, err error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per pass.
	var hasContent, ambiguous bool
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "worktree retirement detection")
		if wfErr != nil {
			return wfErr
		}
		var readErr error
		head, hasContent, ambiguous, readErr = integratedHead(ctx, wf, handle.runID)
		return readErr
	}); err != nil {
		return "", retirementDetection{}, fmt.Errorf("app: read the run's integration head: %w", err)
	}
	switch {
	case ambiguous:
		return "", retirementDetection{State: detectionFailed, Detail: "two integrated rows claim the latest head; failing closed"}, nil
	case head == "" || !hasContent:
		return "", retirementDetection{State: detectionNothingIntegrated, Detail: "the run integrated no new content"}, nil
	}
	if identityCheck, gitErr := c.retirementGit(ctx, frozen.RepositoryRoot, "cat-file", "-e", head+"^{commit}"); gitErr != nil || identityCheck.ExitCode != 0 {
		return "", retirementDetection{State: detectionHistoryMissing, Head: head, Detail: "the repository root no longer holds this run's history"}, nil
	}
	return head, retirementDetection{}, nil
}

// integratedHead reads the run's validated integration head inside the
// caller's unit of work: the merge commit of the most recently created
// integrated row. hasContent is false when no integrated row added content
// (every one is a no-op, merge == pre-merge), and ambiguous is true when
// two integrated rows share the latest creation time with different merge
// commits — never a guess.
func integratedHead(ctx context.Context, wf WorkflowRepositories, runID identity.RunID) (head string, hasContent, ambiguous bool, err error) {
	tasks, err := wf.TaskIndex().ByRun(ctx, runID)
	if err != nil {
		return "", false, false, err
	}
	var latest time.Time
	for i := range tasks {
		integrations, intErr := wf.Integrations().ByTask(ctx, tasks[i].ID)
		if intErr != nil {
			return "", false, false, intErr
		}
		for j := range integrations {
			integ := &integrations[j]
			if integ.State != run.IntegrationIntegrated {
				continue
			}
			if integ.MergeCommitOID != integ.PremergeHeadOID {
				hasContent = true
			}
			switch {
			case head == "" || integ.CreatedAt.After(latest):
				head, latest, ambiguous = integ.MergeCommitOID, integ.CreatedAt, false
			case integ.CreatedAt.Equal(latest) && integ.MergeCommitOID != head:
				ambiguous = true
			}
		}
	}
	return head, hasContent, ambiguous, nil
}

// detectRetirementMerge runs the rest of the detection step for a run
// whose validated head the root holds: unless a settled check already
// answers, the target tip, then one claimed ancestry execution. A check
// already settled as merged ends detection for good; one settled
// not-merged or failed for the same head and tip is not repeated, so
// repeated passes over an unchanged run journal nothing.
func (c *Controller) detectRetirementMerge(ctx context.Context, handle RunHandle, frozen *FrozenRun, opts *retirementPassOptions, head string) (retirementDetection, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per pass.
	var settled []Operation
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		var readErr error
		settled, readErr = uow.Operations().ByKind(ctx, handle.runID, OpRetirementCheck)
		return readErr
	}); err != nil {
		return retirementDetection{}, fmt.Errorf("app: read the run's retirement checks: %w", err)
	}
	for i := range settled {
		if settled[i].State != OperationSucceeded {
			continue
		}
		if outcome, ok := decodeOperationPayload[retirementCheckOutcome](settled[i].Outcome); ok && outcome.Result == ancestryContained {
			return retirementDetection{State: detectionMerged, Head: head, Detail: "detected earlier"}, nil
		}
	}

	root := frozen.RepositoryRoot
	targetRef := frozen.Snapshot.Workflow.TargetBranch
	tip, err := c.retirementGit(ctx, root, "rev-parse", "--verify", "-q", targetRef+"^{commit}")
	switch {
	case err != nil:
		return retirementDetection{State: detectionFailed, Head: head, Detail: "the target branch could not be read"}, nil
	case tip.ExitCode == 1:
		return retirementDetection{State: detectionNotMerged, Head: head, Detail: "the target branch " + targetRef + " no longer exists"}, nil
	case tip.ExitCode != 0:
		return retirementDetection{State: detectionFailed, Head: head, Detail: "the target branch could not be read"}, nil
	}
	target := strings.TrimSpace(string(tip.Stdout))
	if !isObjectID(target) {
		return retirementDetection{State: detectionFailed, Head: head, Detail: "the target branch did not resolve to a commit"}, nil
	}
	for i := range settled {
		if settled[i].State != OperationSucceeded && settled[i].State != OperationFailed {
			continue
		}
		intent, intentOK := decodeOperationPayload[retirementCheckIntent](settled[i].Intent)
		outcome, outcomeOK := decodeOperationPayload[retirementCheckOutcome](settled[i].Outcome)
		if !intentOK || !outcomeOK || intent.HeadOID != head || intent.TargetOID != target {
			continue
		}
		switch outcome.Result {
		case ancestryNotContained:
			return retirementDetection{State: detectionNotMerged, Head: head, Target: target, Detail: "unchanged since the last check"}, nil
		case ancestryFailed:
			return retirementDetection{State: detectionFailed, Head: head, Target: target, Detail: "the last check of this head and tip failed"}, nil
		}
	}
	return c.runRetirementCheck(ctx, handle, frozen, opts, head, targetRef, target)
}

// runRetirementCheck journals and executes one claimed ancestry check.
func (c *Controller) runRetirementCheck(ctx context.Context, handle RunHandle, frozen *FrozenRun, opts *retirementPassOptions, head, targetRef, target string) (retirementDetection, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per check.
	if !filepath.IsAbs(opts.HOPPath) {
		return retirementDetection{}, errors.New("app: the hop executable path is not absolute")
	}
	opID, err := c.newOperationID()
	if err != nil {
		return retirementDetection{}, err
	}
	root := frozen.RepositoryRoot
	argv := worktreeRetirementCheckArgv(c.GitExecutable, root, head, target)
	intent := retirementCheckIntent{
		worktreeRetirementExecIntent: worktreeRetirementExecIntent{Argv: argv, Cwd: root},
		HeadOID:                      head, TargetRef: targetRef, TargetOID: target,
		SpawnArgv: append([]string{opts.HOPPath, "check-exec", "--op", opID.String(), "--"}, argv...),
	}
	now := c.Clock.Now()
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpRetirementCheck, State: OperationPending, Intent: intent,
			CreatedAt: now, UpdatedAt: now,
		})
	}); err != nil {
		return retirementDetection{}, fmt.Errorf("app: record retirement.check intent: %w", err)
	}
	result := retirementDetection{Head: head, Target: target}
	if err := c.revalidateRetirementDispatch(ctx, handle); err != nil {
		result.State, result.Detail = detectionInterrupted, "the pass lost its lease before the check was dispatched"
		return result, nil
	}

	actCtx, release := handle.actContext(ctx)
	boundedCtx, cancel := context.WithTimeout(actCtx, retirementActTimeout)
	cmdResult, runErr := c.Commands.Run(boundedCtx, Command{Argv: intent.SpawnArgv, Dir: root, Env: retirementSpawnEnv(opts.SpawnEnv, frozen.Snapshot.StateRoot)})
	cancel()
	release()

	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), checkPersistenceTimeout)
	defer persistCancel()
	if runErr != nil {
		if markErr := c.markOperationReconciling(persistCtx, handle, opID, "the check's exit status was not observed; the next pass recovers it"); markErr != nil {
			return retirementDetection{}, markErr
		}
		result.State, result.Detail = detectionInterrupted, "the check's exit status was not observed"
		return result, nil
	}
	outcome := retirementCheckOutcome{Result: classifyAncestry(cmdResult.ExitCode), ExitCode: cmdResult.ExitCode}
	state := OperationSucceeded
	switch outcome.Result {
	case ancestryContained:
		result.State = detectionMerged
	case ancestryNotContained:
		result.State = detectionNotMerged
	default:
		state = OperationFailed
		outcome.Detail = firstLine(cmdResult.Stderr)
		result.State, result.Detail = detectionFailed, fmt.Sprintf("the check exited %d", cmdResult.ExitCode)
	}
	if err := c.settleRetirementOperation(persistCtx, handle, opID, state, outcome); err != nil {
		result.State, result.Detail = detectionInterrupted, "the check's outcome could not be recorded"
		return result, nil
	}
	return result, nil
}

// retirementSpawnEnv is the complete environment a claimed retirement
// execution is spawned with: the sanitized spawn environment, the run's
// state root, and the retirement git environment the probe executed under.
func retirementSpawnEnv(spawnEnv []string, stateRoot string) []string {
	return append(withHOPStateDir(spawnEnv, stateRoot), retirementGitEnv()...)
}

// firstLine returns the first line of output, for bounded evidence.
func firstLine(output []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(output)), "\n")
	return line
}

// settleRetirementOperation settles a pending or reconciling retirement
// operation with a structured outcome.
func (c *Controller) settleRetirementOperation(ctx context.Context, handle RunHandle, opID identity.OperationID, state OperationState, outcome any) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
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

// recoverRetirementCheck applies the retirement.check decision-table row
// to an unresolved check left by an earlier pass
// (recoverRetirementOperations). The pass holds a newer
// lease generation than the operation, and ClaimCheckExec refuses any
// operation of another generation, so a missing claim is decisive: the
// check never ran and never will. A present claim's group is retired
// first; its exit status was never observed, so the check settles unknown
// and a fresh one may run. It returns a blocking detail while the group
// is not yet observed gone or does not match.
func (c *Controller) recoverRetirementCheck(ctx context.Context, handle RunHandle, op *Operation) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	if op.Generation >= handle.lease.Generation {
		return fmt.Sprintf("retirement.check %s belongs to this pass's generation; left for its own settlement", op.ID), nil
	}
	intent, ok := decodeOperationPayload[retirementCheckIntent](op.Intent)
	if !ok || len(intent.Argv) == 0 || len(intent.SpawnArgv) == 0 {
		if err := c.markOperationReconciling(ctx, handle, op.ID, "retirement.check intent could not be decoded; failing closed"); err != nil {
			return "", err
		}
		return fmt.Sprintf("retirement.check %s has an undecodable intent; failing closed", op.ID), nil
	}
	var (
		claim CheckExecClaim
		found bool
	)
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		var err error
		claim, found, err = uow.CheckExecClaims().Get(ctx, op.ID)
		return err
	}); err != nil {
		return "", fmt.Errorf("app: load retirement.check exec claim: %w", err)
	}
	if !found {
		return "", c.settleRetirementOperation(ctx, handle, op.ID, OperationFailed, retirementCheckOutcome{
			Result: ancestryNeverExecuted, ExitCode: -1, Detail: "no exec claim under a superseded generation; the check never ran",
		})
	}
	outcome, err := c.retireGroup(ctx, handle, claim.PID, [][]string{intent.Argv, intent.SpawnArgv})
	if err != nil {
		return "", err
	}
	switch outcome {
	case GroupEmpty:
		return "", c.settleRetirementOperation(ctx, handle, op.ID, OperationFailed, retirementCheckOutcome{
			Result: ancestryUnknown, ExitCode: -1, Detail: "the check's group is gone and its exit status was never observed",
		})
	case GroupMatched:
		return fmt.Sprintf("retirement.check group %d signaled; awaiting observed absence", claim.PID), nil
	case GroupMismatched:
		if err := c.markOperationReconciling(ctx, handle, op.ID, "retirement.check group members do not match the recorded argv; failing closed"); err != nil {
			return "", err
		}
		return fmt.Sprintf("retirement.check group %d does not match the recorded argv; failing closed", claim.PID), nil
	default:
		return fmt.Sprintf("retirement.check group %d could not be inspected; failing closed", claim.PID), nil
	}
}
