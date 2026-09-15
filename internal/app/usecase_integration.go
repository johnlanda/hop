package app

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// Phase 3 integration operation kinds (docs/plan/phase-3-design.md
// section 4). Additive constants on the Phase 2 OperationKind type;
// declared here, with the operations they journal, rather than in
// store.go, exactly as usecase_schedule.go declares its own intent
// shapes.
const (
	// OpIntegrationMerge is the scratch merge: one merge EXECUTION per
	// operation, the operation ID fixing the exec claim, the frozen merge
	// argv and the private tree path immutably. It never touches the ref.
	OpIntegrationMerge OperationKind = "integration.merge"
	// OpIntegrationPublish is the compare-and-swap publish of the merge
	// commit to the integration ref, executed directly by the controller.
	OpIntegrationPublish OperationKind = "integration.publish"
	// OpIntegrationReset is the combined-check failure rollback (also the
	// stop path's retirement of a published-but-unsettled candidate): a
	// fresh rollback commit R, its OID persisted BEFORE the CAS.
	OpIntegrationReset OperationKind = "integration.reset"
	// OpIntegrationFence is the ref-fencing rule's first-class operation:
	// a fencing commit F carrying the current head's tree with the head
	// as parent, burning a stale publish intent's expected-old value.
	OpIntegrationFence OperationKind = "integration.fence"
)

// integrationMergeIntent is the OpIntegrationMerge intent payload: the
// operation ID fixes all three execution identities immutably — the exec
// claim, the frozen merge argv and the private tree path.
type integrationMergeIntent struct {
	IntegrationID string   `json:"integration_id"`
	PremergeOID   string   `json:"premerge_oid"`
	SourceOID     string   `json:"source_oid"`
	TreePath      string   `json:"tree_path"`
	HooksPath     string   `json:"hooks_path"`
	MergeArgv     []string `json:"merge_argv"`
	SpawnArgv     []string `json:"spawn_argv"`
}

// integrationMergeMaterialized is the act-evidence record of the merge's
// step (i): the detached scratch tree was materialized to OBSERVED
// completion and its HEAD verified against the recorded pre-merge head.
// Adoption of a materialized directory requires exactly this record —
// never a HEAD observation alone (git worktree add writes HEAD before it
// finishes populating the checkout).
type integrationMergeMaterialized struct {
	TreePath     string `json:"tree_path"`
	VerifiedHead string `json:"verified_head"`
}

// Merge outcome results.
const (
	mergeResultMerged   = "merged"
	mergeResultNoOp     = "no-op"
	mergeResultConflict = "conflict"
	mergeResultFailed   = "failed"
)

// integrationMergeOutcome is the OpIntegrationMerge outcome payload.
type integrationMergeOutcome struct {
	Result         string `json:"result"`
	MergeCommitOID string `json:"merge_commit_oid,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

// integrationPublishIntent is the OpIntegrationPublish intent payload.
type integrationPublishIntent struct {
	IntegrationID  string `json:"integration_id"`
	Ref            string `json:"ref"`
	NewOID         string `json:"new_oid"`
	ExpectedOldOID string `json:"expected_old_oid"`
}

// integrationResetIntent is the OpIntegrationReset intent payload. The
// rollback commit is built from the RECORDED VALIDATED PRE-MERGE TREE;
// RejectedOID is the published candidate the CAS expects as old value,
// and the rollback commit's parent, so the rejected merge stays
// reachable.
type integrationResetIntent struct {
	IntegrationID string `json:"integration_id"`
	Ref           string `json:"ref"`
	RejectedOID   string `json:"rejected_oid"`
	PremergeOID   string `json:"premerge_oid"`
	Reason        string `json:"reason"`
}

// integrationResetEvidence is the OpIntegrationReset act evidence: the
// rollback commit OID, persisted BEFORE the CAS so recovery is decidable
// in every window. Recovery compares the PERSISTED OID only, never a
// recomputation. ParentOID records the observed head the commit was
// built against (ordinarily the rejected candidate; the fencing rule's
// reset completion may build against a later observed head).
type integrationResetEvidence struct {
	RollbackOID string `json:"rollback_oid"`
	ParentOID   string `json:"parent_oid"`
}

// integrationFenceIntent is the OpIntegrationFence intent payload: the
// observed head the fence preserves and the ref-move intent it retires.
type integrationFenceIntent struct {
	Ref                  string `json:"ref"`
	ObservedHeadOID      string `json:"observed_head_oid"`
	RetiredOperationID   string `json:"retired_operation_id"`
	RetiredOperationKind string `json:"retired_operation_kind"`
}

// integrationFenceEvidence is the OpIntegrationFence act evidence: the
// fencing commit OID, persisted before the CAS.
type integrationFenceEvidence struct {
	FenceOID string `json:"fence_oid"`
}

// integrationCheckIntent is the combined-candidate check execution's
// intent payload: an OpCheckRun with an integration subject rather than a
// result subject (the generalized pipeline's subject_kind, expressed in
// the intent until migration 002 lands the typed request table). The
// JSON keys for the argvs and checkout path match CheckRunIntent's, so
// the shared group-retirement decode reads either shape.
type integrationCheckIntent struct {
	IntegrationID    string   `json:"integration_id"`
	SubjectCommitOID string   `json:"subject_commit_oid"`
	SubjectTreeOID   string   `json:"subject_tree_oid"`
	ResultID         string   `json:"result_id"`
	CheckoutPath     string   `json:"checkout_path"`
	CheckArgv        []string `json:"check_argv"`
	SpawnArgv        []string `json:"spawn_argv"`
}

// IntegrationReport is one DriveIntegration round's outcome.
type IntegrationReport struct {
	// Claimed is true when this round claimed a fresh integration.
	Claimed bool
	// IntegrationID names the run's current (or just-settled) integration.
	IntegrationID string
	// State is that integration's state after the round; "" when the run
	// has none and nothing was claimable.
	State string
	// Blocked names an unresolved integration operation that refuses new
	// integration work (the decision table's unresolved-intent rule).
	Blocked string
	// Interrupted is true when a stop request prevented new work.
	Interrupted bool
}

// integrationRef renders the integration branch's full ref name.
func integrationRef(branch string) string { return "refs/heads/" + branch }

// integrationOpDir is the per-operation private directory under the run's
// state root: scratch trees, hooks dir and retained outputs all live
// under it, and no two operations ever share a path.
func integrationOpDir(stateRoot string, runID identity.RunID, opID identity.OperationID) string {
	return filepath.Join(stateRoot, "runs", runID.String(), "integrations", opID.String())
}

// integrationMergeArgv is the frozen, fully noninteractive merge argv
// (design section 8, human decision Q5(a) 2026-09-15): identity fixed,
// editor disabled, signing and signature verification off, and
// repository-local hooks suppressed by pointing core.hooksPath at an
// empty directory under the operation's artifact dir.
func integrationMergeArgv(gitExecutable, treePath, hooksPath, sourceOID string) []string {
	return []string{
		gitExecutable, "-C", treePath,
		"-c", "user.name=hop", "-c", "user.email=hop@invalid",
		"-c", "core.editor=true",
		"-c", "commit.gpgsign=false", "-c", "merge.verifysignatures=false",
		"-c", "core.hooksPath=" + hooksPath,
		"merge", "--no-ff", "--no-edit", sourceOID,
	}
}

// integrationMergeSpawnEnv extends the sanitized spawn environment with
// the frozen noninteractive git environment (design section 8):
// terminal prompts off, editor neutralized, inherited global and system
// configuration suppressed.
func integrationMergeSpawnEnv(spawnEnv []string, stateRoot string) []string {
	env := withHOPStateDir(spawnEnv, stateRoot)
	return append(env,
		"GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
}

// gitDeterministicCommitEnv is the byte-deterministic commit-tree
// environment for rollback and fencing commits: fixed identity, both
// dates pinned to the operation intent's timestamp (design section 4).
func gitDeterministicCommitEnv(intentTime time.Time) []string {
	stamp := intentTime.UTC().Format(time.RFC3339)
	return []string{
		"GIT_AUTHOR_NAME=hop", "GIT_AUTHOR_EMAIL=hop@invalid",
		"GIT_COMMITTER_NAME=hop", "GIT_COMMITTER_EMAIL=hop@invalid",
		"GIT_AUTHOR_DATE=" + stamp, "GIT_COMMITTER_DATE=" + stamp,
	}
}

// runGitEnv runs one git subcommand against dir with extra environment
// entries, mirroring runGit's contract (absolute GitExecutable required;
// a non-zero exit folds into the error).
func (c *Controller) runGitEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	if !filepath.IsAbs(c.GitExecutable) {
		return "", fmt.Errorf("app: git executable is not configured as an absolute path")
	}
	result, err := c.Commands.Run(ctx, Command{Argv: append([]string{c.GitExecutable, "-C", dir}, args...), Env: env})
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("git -C %s %s: exit %d: %s", dir, strings.Join(args, " "), result.ExitCode, string(result.Stderr))
	}
	return strings.TrimSpace(string(result.Stdout)), nil
}

// ResolveIntegrationHead reads the current integration branch head for
// handle's run — the worktree base every implement assignment consumes
// (AssignmentOptions.IntegrationHeadCommitOID) and the guard's head
// commit. It is a read of the live ref, never of integration rows.
func (c *Controller) ResolveIntegrationHead(ctx context.Context, handle RunHandle) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per scheduling pass.
	frozen, err := c.Read.LoadFrozenRun(ctx, handle.runID)
	if err != nil {
		return "", fmt.Errorf("app: load frozen run: %w", err)
	}
	if !frozen.Snapshot.Workflow.Feature() {
		return "", fmt.Errorf("app: run %s is not a feature-mode run", handle.runID)
	}
	head, err := c.runGit(ctx, frozen.RepositoryRoot, "rev-parse", "--verify", integrationRef(frozen.Snapshot.Workflow.IntegrationBranch))
	if err != nil {
		return "", fmt.Errorf("app: resolve integration head: %w", err)
	}
	return head, nil
}

// DriveIntegration performs one round of the serial-integration step:
// recover any unresolved integration operation first (nothing new starts
// while one stands), then advance the run's current integration one
// journaled operation — merge, publish, combined check or reset — or
// claim the next completed implement task into the serial slot. Callers
// (the controller loop's scheduling pass) invoke it repeatedly; every
// external act is record-intent → act → record-outcome with pre-dispatch
// revalidation.
func (c *Controller) DriveIntegration(ctx context.Context, handle RunHandle, hopPath string, spawnEnv []string) (IntegrationReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per scheduling pass.
	frozen, err := c.Read.LoadFrozenRun(ctx, handle.runID)
	if err != nil {
		return IntegrationReport{}, fmt.Errorf("app: load frozen run: %w", err)
	}
	if !frozen.Snapshot.Workflow.Feature() {
		return IntegrationReport{}, nil
	}

	blocked, err := c.recoverIntegrationOperations(ctx, handle, &frozen, hopPath, spawnEnv)
	if err != nil {
		return IntegrationReport{}, err
	}
	if blocked != "" {
		return IntegrationReport{Blocked: blocked}, nil
	}

	integ, exists, err := c.currentIntegration(ctx, handle)
	if err != nil {
		return IntegrationReport{}, err
	}
	if !exists {
		return c.claimNextIntegration(ctx, handle, &frozen)
	}

	report := IntegrationReport{IntegrationID: integ.ID.String(), State: string(integ.State)}
	switch integ.State {
	case run.IntegrationMerging:
		return c.advanceMerging(ctx, handle, &frozen, hopPath, spawnEnv, &integ)
	case run.IntegrationChecking:
		return c.advanceChecking(ctx, handle, &frozen, hopPath, spawnEnv, &integ)
	case run.IntegrationCheckFailed:
		if err := c.driveIntegrationReset(ctx, handle, &frozen, &integ, "combined check failed"); err != nil {
			return report, err
		}
		return c.reportCurrentIntegrationState(ctx, handle, integ.ID)
	default:
		return report, nil
	}
}

// currentIntegration reads the run's non-terminal integration, if any.
func (c *Controller) currentIntegration(ctx context.Context, handle RunHandle) (run.Integration, bool, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	var (
		integ  run.Integration
		exists bool
	)
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "DriveIntegration")
		if wfErr != nil {
			return wfErr
		}
		var getErr error
		integ, _, exists, getErr = wf.Integrations().Current(ctx, handle.runID)
		return getErr
	})
	return integ, exists, err
}

// reportCurrentIntegrationState re-reads one integration for the round
// report.
func (c *Controller) reportCurrentIntegrationState(ctx context.Context, handle RunHandle, id identity.IntegrationID) (IntegrationReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	report := IntegrationReport{IntegrationID: id.String()}
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "DriveIntegration")
		if wfErr != nil {
			return wfErr
		}
		integ, _, getErr := wf.Integrations().Get(ctx, id)
		if getErr != nil {
			return getErr
		}
		report.State = string(integ.State)
		return nil
	})
	return report, err
}

// claimNextIntegration claims the lowest-seq completed implement task
// into the serial integration slot: the pre-merge head resolved from the
// live ref before the transaction, then one controller transaction
// moving the task completed → integrating and creating the integration
// row in merging. Unstarted work is never started after a stop request.
func (c *Controller) claimNextIntegration(ctx context.Context, handle RunHandle, frozen *FrozenRun) (IntegrationReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per scheduling pass.
	premerge, headErr := c.runGit(ctx, frozen.RepositoryRoot, "rev-parse", "--verify", integrationRef(frozen.Snapshot.Workflow.IntegrationBranch))
	integrationID, idErr := identity.ParseIntegrationID(c.IDs.NewID())
	if idErr != nil {
		return IntegrationReport{}, fmt.Errorf("app: generate integration id: %w", idErr)
	}
	now := c.Clock.Now()
	var report IntegrationReport
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "claim integration")
		if wfErr != nil {
			return wfErr
		}
		r, _, runErr := uow.Runs().Get(ctx, handle.runID)
		if runErr != nil {
			return runErr
		}
		if r.StopRequested {
			report.Interrupted = true
			return nil
		}
		if _, _, occupied, curErr := wf.Integrations().Current(ctx, handle.runID); curErr != nil {
			return curErr
		} else if occupied {
			return nil // the serial slot filled since the caller looked; nothing to claim.
		}
		tasks, taskErr := wf.TaskIndex().ByRun(ctx, handle.runID)
		if taskErr != nil {
			return taskErr
		}
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].Seq < tasks[j].Seq })
		var task run.Task
		found := false
		for i := range tasks {
			if tasks[i].Kind == run.TaskKindImplement && tasks[i].State == run.TaskCompleted {
				task = tasks[i]
				found = true
				break
			}
		}
		if !found {
			return nil
		}
		if headErr != nil {
			return fmt.Errorf("app: resolve pre-merge integration head: %w", headErr)
		}

		attempt, result, resErr := newestAcceptedResult(ctx, uow, wf, task.ID)
		if resErr != nil {
			return resErr
		}
		if result == nil {
			return fmt.Errorf("app: task %s is completed but has no accepted result; cannot integrate", task.ID)
		}

		_, taskRev, getErr := uow.Tasks().Get(ctx, task.ID)
		if getErr != nil {
			return getErr
		}
		taskFrom := task.State
		integrating, trErr := task.EnterIntegrating(now)
		if trErr != nil {
			return trErr
		}
		if _, saveErr := uow.Tasks().Save(ctx, integrating, taskRev); saveErr != nil {
			return saveErr
		}
		integ := run.NewIntegration(integrationID, handle.runID, task.ID, result.ID, result.CommitOID, premerge, now)
		if _, createErr := wf.Integrations().Create(ctx, integ); createErr != nil {
			return createErr
		}
		generation := gen(handle.lease.Generation)
		if transErr := recordTransition(ctx, uow, EntityTask, task.ID.String(), string(taskFrom), string(integrating.State), "integration claimed", generation, now); transErr != nil {
			return transErr
		}
		report = IntegrationReport{Claimed: true, IntegrationID: integrationID.String(), State: string(run.IntegrationMerging)}
		_ = attempt
		return nil
	})
	return report, err
}

// newestAcceptedResult resolves a task's newest attempt that has an
// accepted result, and that result.
func newestAcceptedResult(ctx context.Context, uow UnitOfWork, wf WorkflowRepositories, taskID identity.TaskID) (run.Attempt, *run.Result, error) {
	attempts, err := wf.AttemptIndex().ByTask(ctx, taskID)
	if err != nil {
		return run.Attempt{}, nil, err
	}
	sort.Slice(attempts, func(i, j int) bool { return attempts[i].Number > attempts[j].Number })
	for i := range attempts {
		result, resErr := uow.Results().Accepted(ctx, attempts[i].ID)
		if resErr != nil {
			return run.Attempt{}, nil, resErr
		}
		if result != nil {
			return attempts[i], result, nil
		}
	}
	return run.Attempt{}, nil, nil
}

// advanceMerging advances an integration in merging: run the scratch
// merge when none has been executed, or publish an already-produced
// merge commit.
func (c *Controller) advanceMerging(ctx context.Context, handle RunHandle, frozen *FrozenRun, hopPath string, spawnEnv []string, integ *run.Integration) (IntegrationReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	report := IntegrationReport{IntegrationID: integ.ID.String(), State: string(integ.State)}
	mergeOp, outcome, found, err := c.newestSettledMergeOutcome(ctx, handle, integ.ID)
	if err != nil {
		return report, err
	}
	if found && outcome.Result == mergeResultMerged && outcome.MergeCommitOID != "" {
		if err := c.runIntegrationPublish(ctx, handle, frozen, integ, outcome.MergeCommitOID); err != nil {
			return report, err
		}
		return c.reportCurrentIntegrationState(ctx, handle, integ.ID)
	}
	_ = mergeOp
	if err := c.runIntegrationMerge(ctx, handle, frozen, integ, hopPath, spawnEnv); err != nil {
		return report, err
	}
	return c.reportCurrentIntegrationState(ctx, handle, integ.ID)
}

// newestSettledMergeOutcome reads the newest SETTLED integration.merge
// operation for one integration, decoded. found is false when none
// exists or the newest settled one did not produce a usable outcome.
func (c *Controller) newestSettledMergeOutcome(ctx context.Context, handle RunHandle, integrationID identity.IntegrationID) (Operation, integrationMergeOutcome, bool, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	var (
		newest  Operation
		decoded integrationMergeOutcome
		found   bool
	)
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		ops, opErr := uow.Operations().ByKind(ctx, handle.runID, OpIntegrationMerge)
		if opErr != nil {
			return opErr
		}
		for i := range ops {
			intent, ok := decodeOperationPayload[integrationMergeIntent](ops[i].Intent)
			if !ok || intent.IntegrationID != integrationID.String() {
				continue
			}
			if ops[i].State != OperationSucceeded && ops[i].State != OperationFailed {
				continue
			}
			outcome, ok := decodeOperationPayload[integrationMergeOutcome](ops[i].Outcome)
			if !ok {
				continue
			}
			newest = ops[i]
			decoded = outcome
			found = true
			return nil // ByKind is newest-first: the first settled match is the newest.
		}
		return nil
	})
	return newest, decoded, found, err
}

// runIntegrationMerge drives one OpIntegrationMerge operation end to end:
// intent (fresh operation ID, tree path, hooks dir, frozen argvs), act
// step (i) MATERIALIZE to observed completion with the HEAD verified and
// the completion recorded as act evidence, act step (ii) SPAWN through
// the generalized exec boundary bounded by the frozen check timeout, and
// the outcome transaction classifying merged / no-op / conflict /
// failed. A conflict settles the integration and applies the task
// consequence in the same transaction.
func (c *Controller) runIntegrationMerge(ctx context.Context, handle RunHandle, frozen *FrozenRun, integ *run.Integration, hopPath string, spawnEnv []string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per merge execution.
	opID, err := c.newOperationID()
	if err != nil {
		return err
	}
	opDir := integrationOpDir(frozen.Snapshot.StateRoot, handle.runID, opID)
	treePath := filepath.Join(opDir, "tree")
	hooksPath := filepath.Join(opDir, "hooks")
	mergeArgv := integrationMergeArgv(c.GitExecutable, treePath, hooksPath, integ.SourceCommitOID)
	spawnArgv := append([]string{hopPath, "check-exec", "--op", opID.String(), "--"}, mergeArgv...)
	intent := integrationMergeIntent{
		IntegrationID: integ.ID.String(), PremergeOID: integ.PremergeHeadOID, SourceOID: integ.SourceCommitOID,
		TreePath: treePath, HooksPath: hooksPath, MergeArgv: mergeArgv, SpawnArgv: spawnArgv,
	}
	now := c.Clock.Now()
	if intentErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpIntegrationMerge, State: OperationPending, Intent: intent,
			CreatedAt: now, UpdatedAt: now,
		})
	}); intentErr != nil {
		return fmt.Errorf("app: record integration.merge intent: %w", intentErr)
	}

	actCtx, release := handle.actContext(ctx)
	defer release()

	// Step (i): materialize the detached scratch tree to observed
	// completion, verify its HEAD, and only then record the completion
	// evidence. Any failure before the record settles the operation
	// failed: the directory is abandoned (never entered again, its
	// possible surviving preparer confined to it) and a re-act is a NEW
	// operation with a fresh path.
	if revErr := c.revalidateForDispatch(ctx, handle, false); revErr != nil {
		return fmt.Errorf("app: revalidate before merge materialization: %w", revErr)
	}
	// The empty hooks directory exists before the spawn: core.hooksPath
	// points at it so no repository-local hook runs during the merge.
	if hooksErr := c.Artifacts.WriteArtifact(actCtx, filepath.Join(hooksPath, ".keep"), nil); hooksErr != nil {
		return c.settleOperation(ctx, handle, opID, OperationFailed, fmt.Sprintf("hooks directory could not be created: %v", hooksErr))
	}
	if _, addErr := c.runGit(actCtx, frozen.RepositoryRoot, "worktree", "add", "--detach", treePath, integ.PremergeHeadOID); addErr != nil {
		return c.settleOperation(ctx, handle, opID, OperationFailed, fmt.Sprintf("materialization incomplete; directory abandoned: %v", addErr))
	}
	head, headErr := c.runGit(actCtx, treePath, "rev-parse", "HEAD^{commit}")
	if headErr != nil {
		return c.settleOperation(ctx, handle, opID, OperationFailed, fmt.Sprintf("materialized tree HEAD could not be verified; directory abandoned: %v", headErr))
	}
	if head != integ.PremergeHeadOID {
		return c.settleOperation(ctx, handle, opID, OperationFailed, fmt.Sprintf("materialized tree HEAD %s is not the recorded pre-merge head %s; directory abandoned", head, integ.PremergeHeadOID))
	}
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		op, getErr := uow.Operations().Get(ctx, opID)
		if getErr != nil {
			return getErr
		}
		op.ActEvidence = integrationMergeMaterialized{TreePath: treePath, VerifiedHead: head}
		op.UpdatedAt = c.Clock.Now()
		return uow.Operations().Save(ctx, op)
	}); err != nil {
		return fmt.Errorf("app: record merge materialization evidence: %w", err)
	}

	// Step (ii): spawn the merge through the generalized exec boundary so
	// a durable pre-exec group claim exists, bounded by the frozen check
	// timeout.
	if err := c.revalidateForDispatch(ctx, handle, false); err != nil {
		return fmt.Errorf("app: revalidate before merge spawn: %w", err)
	}
	timeout := frozen.Snapshot.CheckTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	boundedCtx, cancel := context.WithTimeout(actCtx, timeout)
	cmdResult, runErr := c.Commands.Run(boundedCtx, Command{Argv: spawnArgv, Dir: treePath, Env: integrationMergeSpawnEnv(spawnEnv, frozen.Snapshot.StateRoot)})
	cancel()

	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), checkPersistenceTimeout)
	defer persistCancel()

	if runErr != nil {
		// The spawn's result is unknowable: ambiguous, resolved through
		// the claim table by recovery, never by guessing.
		if markErr := c.markOperationReconciling(persistCtx, handle, opID, fmt.Sprintf("merge spawn returned an error before an outcome was observed: %v", runErr)); markErr != nil {
			return markErr
		}
		return fmt.Errorf("app: integration merge execution ambiguous: %w", runErr)
	}

	if cmdResult.ExitCode != 0 {
		// Conflict classification: retain the full output as evidence and
		// settle the integration with the task consequence in the same
		// transaction. The conflicted scratch tree is retained until
		// settlement (no abort step exists; the tree is never reused).
		evidence, captureErr := c.captureIntegrationOutputs(persistCtx, handle, frozen, opID, integ.ResultID, cmdResult)
		detail := fmt.Sprintf("merge exited %d", cmdResult.ExitCode)
		if captureErr != nil {
			detail += fmt.Sprintf("; output retention incomplete: %v", captureErr)
		}
		return c.settleIntegrationTerminal(persistCtx, handle, frozen, integ.ID, integrationSettlement{
			OpID: opID, OpState: OperationFailed,
			OpOutcome:   integrationMergeOutcome{Result: mergeResultConflict, Detail: detail},
			TargetState: run.IntegrationConflicted,
			Reason:      "merge conflict: " + detail,
			Evidence:    evidence,
		})
	}

	// Exit 0: classify merged vs the ancestor/no-op outcome by inspecting
	// the scratch tree.
	classified, classifyErr := c.classifyMergeSuccess(ctx, treePath, integ.PremergeHeadOID, integ.SourceCommitOID)
	if classifyErr != nil {
		if markErr := c.markOperationReconciling(persistCtx, handle, opID, fmt.Sprintf("merge exited 0 but the scratch tree could not be inspected: %v", classifyErr)); markErr != nil {
			return markErr
		}
		return fmt.Errorf("app: classify merge result: %w", classifyErr)
	}
	now = c.Clock.Now()
	switch classified.Result {
	case mergeResultMerged:
		return c.withUnitOfWork(persistCtx, handle.lease, func(uow UnitOfWork) error {
			op, getErr := uow.Operations().Get(ctx, opID)
			if getErr != nil {
				return getErr
			}
			op.State = OperationSucceeded
			op.Outcome = classified
			op.UpdatedAt = now
			return uow.Operations().Save(ctx, op)
		})
	case mergeResultNoOp:
		// No publish runs: the ref is already the candidate. The check
		// subject is the unchanged head; the integration enters checking
		// in the same transaction that settles the operation.
		return c.withUnitOfWork(persistCtx, handle.lease, func(uow UnitOfWork) error {
			wf, wfErr := RequireWorkflowRepositories(uow, "merge no-op outcome")
			if wfErr != nil {
				return wfErr
			}
			op, getErr := uow.Operations().Get(ctx, opID)
			if getErr != nil {
				return getErr
			}
			op.State = OperationSucceeded
			op.Outcome = classified
			op.UpdatedAt = now
			if saveErr := uow.Operations().Save(ctx, op); saveErr != nil {
				return saveErr
			}
			latest, rev, getErr := wf.Integrations().Get(ctx, integ.ID)
			if getErr != nil {
				return getErr
			}
			checking, trErr := latest.EnterChecking(integ.PremergeHeadOID, now)
			if trErr != nil {
				return trErr
			}
			if _, saveErr := wf.Integrations().Save(ctx, checking, rev); saveErr != nil {
				return saveErr
			}
			return recordTransition(ctx, uow, EntityTask, integ.TaskID.String(), string(run.TaskIntegrating), string(run.TaskIntegrating), "merge no-op: source already reachable from the head", gen(handle.lease.Generation), now)
		})
	default:
		return c.settleOperation(persistCtx, handle, opID, OperationFailed, classified.Detail)
	}
}

// classifyMergeSuccess inspects the scratch tree after a zero-exit merge:
// a two-parent merge commit whose parents are exactly {pre-merge head,
// source} is merged; an unchanged HEAD with the source already an
// ancestor of the head is the legitimate no-op; anything else fails the
// operation.
func (c *Controller) classifyMergeSuccess(ctx context.Context, treePath, premergeOID, sourceOID string) (integrationMergeOutcome, error) {
	head, err := c.runGit(ctx, treePath, "rev-parse", "HEAD^{commit}")
	if err != nil {
		return integrationMergeOutcome{}, err
	}
	if head == premergeOID {
		if _, ancestorErr := c.runGit(ctx, treePath, "merge-base", "--is-ancestor", sourceOID, premergeOID); ancestorErr != nil {
			return integrationMergeOutcome{Result: mergeResultFailed, Detail: fmt.Sprintf("merge exited 0 leaving HEAD at the pre-merge head, but %s is not an ancestor of it", sourceOID)}, nil
		}
		return integrationMergeOutcome{Result: mergeResultNoOp, MergeCommitOID: premergeOID}, nil
	}
	parentsLine, err := c.runGit(ctx, treePath, "rev-list", "--parents", "-n", "1", "HEAD")
	if err != nil {
		return integrationMergeOutcome{}, err
	}
	fields := strings.Fields(parentsLine)
	if len(fields) != 3 || fields[0] != head {
		return integrationMergeOutcome{Result: mergeResultFailed, Detail: fmt.Sprintf("merge produced %q, not a two-parent merge commit", parentsLine)}, nil
	}
	parents := []string{fields[1], fields[2]}
	want := []string{premergeOID, sourceOID}
	slices.Sort(parents)
	slices.Sort(want)
	if !slices.Equal(parents, want) {
		return integrationMergeOutcome{Result: mergeResultFailed, Detail: fmt.Sprintf("merge commit %s has parents %v, not the recorded {pre-merge head, source}", head, fields[1:])}, nil
	}
	return integrationMergeOutcome{Result: mergeResultMerged, MergeCommitOID: head}, nil
}

// captureIntegrationOutputs retains a merge execution's stdout and stderr
// under the operation's own directory as result-linked artifact rows,
// mirroring captureCheckOutputs.
func (c *Controller) captureIntegrationOutputs(ctx context.Context, handle RunHandle, frozen *FrozenRun, opID identity.OperationID, resultID identity.ResultID, cmdResult CommandResult) ([]string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per settled merge.
	base := integrationOpDir(frozen.Snapshot.StateRoot, handle.runID, opID)
	var (
		paths      []string
		captureErr error
		rows       []run.Artifact
	)
	streams := []struct {
		name    string
		kind    run.ArtifactKind
		content []byte
	}{
		{"stdout", run.ArtifactCheckStdout, cmdResult.Stdout},
		{"stderr", run.ArtifactCheckStderr, cmdResult.Stderr},
	}
	for _, stream := range streams {
		path := filepath.Join(base, stream.name)
		if err := c.Artifacts.WriteArtifact(ctx, path, stream.content); err != nil {
			captureErr = fmt.Errorf("retain merge %s: %w", stream.name, err)
			continue
		}
		artifactID, err := identity.ParseArtifactID(c.IDs.NewID())
		if err != nil {
			captureErr = fmt.Errorf("retain merge %s: %w", stream.name, err)
			continue
		}
		rows = append(rows, run.NewResultArtifact(artifactID, handle.runID, resultID, stream.kind, path, sha256Hex(stream.content)))
		paths = append(paths, path)
	}
	if len(rows) > 0 {
		if err := c.saveEvidenceRows(ctx, handle, rows); err != nil {
			return paths, err
		}
	}
	return paths, captureErr
}

// runIntegrationPublish drives one OpIntegrationPublish operation: the
// compare-and-swap update-ref moving the integration branch to the merge
// commit, with the Phase 2 revalidation immediately before the dispatch
// and a lease-fenced outcome transaction that re-reads stop. A landed
// publish is always recorded (the candidate is published); a held stop
// leaves the published candidate to the stop/rollback path.
func (c *Controller) runIntegrationPublish(ctx context.Context, handle RunHandle, frozen *FrozenRun, integ *run.Integration, mergeOID string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per publish.
	// A publish intent is never re-journaled while one is unresolved:
	// recovery owns unresolved intents (recoverIntegrationOperations runs
	// before any new work in DriveIntegration).
	opID, err := c.newOperationID()
	if err != nil {
		return err
	}
	ref := integrationRef(frozen.Snapshot.Workflow.IntegrationBranch)
	intent := integrationPublishIntent{
		IntegrationID: integ.ID.String(), Ref: ref,
		NewOID: mergeOID, ExpectedOldOID: integ.PremergeHeadOID,
	}
	now := c.Clock.Now()
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpIntegrationPublish, State: OperationPending, Intent: intent,
			CreatedAt: now, UpdatedAt: now,
		})
	}); err != nil {
		return fmt.Errorf("app: record integration.publish intent: %w", err)
	}
	return c.actAndSettlePublish(ctx, handle, frozen, opID, &intent)
}

// actAndSettlePublish performs a publish intent's CAS act and outcome
// transaction; recovery's re-act path shares it, so a retried publish is
// always the SAME operation re-acted, never a second intent.
func (c *Controller) actAndSettlePublish(ctx context.Context, handle RunHandle, frozen *FrozenRun, opID identity.OperationID, intent *integrationPublishIntent) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	// Layer (i): the revalidation runs immediately before the update-ref
	// dispatch specifically.
	if err := c.revalidateForDispatch(ctx, handle, false); err != nil {
		return fmt.Errorf("app: revalidate before publish CAS: %w", err)
	}
	actCtx, release := handle.actContext(ctx)
	_, casErr := c.runGit(actCtx, frozen.RepositoryRoot, "update-ref", intent.Ref, intent.NewOID, intent.ExpectedOldOID)
	release()

	if casErr != nil {
		observed, obsErr := c.runGit(ctx, frozen.RepositoryRoot, "rev-parse", "--verify", intent.Ref)
		if obsErr != nil {
			return c.markOperationReconciling(ctx, handle, opID, fmt.Sprintf("publish CAS failed (%v) and the ref could not be observed (%v)", casErr, obsErr))
		}
		if observed != intent.NewOID {
			// The CAS lost: the ref is not the candidate. The head either
			// moved (a fence or takeover retired this intent) or the store
			// refused; either way this operation settles by observation.
			if markErr := c.markOperationReconciling(ctx, handle, opID, fmt.Sprintf("publish CAS refused; observed ref %s", observed)); markErr != nil {
				return markErr
			}
			return fmt.Errorf("app: publish CAS refused; ref is at %s, not %s", observed, intent.NewOID)
		}
		// The ref reads as the candidate despite the reported failure:
		// adopt below.
	}

	// Layer (ii): the outcome transaction is lease-fenced and re-reads
	// stop. The publish landed either way; a held stop only means the
	// candidate now belongs to the stop/rollback path, so the integration
	// still enters checking (published, unvalidated) and stop handling
	// drives the reset before the run ever reports stopped.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "publish outcome")
		if wfErr != nil {
			return wfErr
		}
		op, getErr := uow.Operations().Get(ctx, opID)
		if getErr != nil {
			return getErr
		}
		if op.State == OperationSucceeded {
			return nil
		}
		op.State = OperationSucceeded
		op.Outcome = "published " + intent.NewOID
		op.UpdatedAt = now
		if saveErr := uow.Operations().Save(ctx, op); saveErr != nil {
			return saveErr
		}
		integrationID := identity.IntegrationID(intent.IntegrationID)
		latest, rev, getErr := wf.Integrations().Get(ctx, integrationID)
		if getErr != nil {
			return getErr
		}
		if latest.State != run.IntegrationMerging {
			return nil
		}
		checking, trErr := latest.EnterChecking(intent.NewOID, now)
		if trErr != nil {
			return trErr
		}
		if _, saveErr := wf.Integrations().Save(ctx, checking, rev); saveErr != nil {
			return saveErr
		}
		return recordTransition(ctx, uow, EntityTask, latest.TaskID.String(), string(run.TaskIntegrating), string(run.TaskIntegrating), "candidate published to "+intent.Ref, gen(handle.lease.Generation), now)
	})
}

// advanceChecking advances an integration in checking: adopt an existing
// settled passing receipt for exactly this candidate, or run a fresh
// combined-check execution.
func (c *Controller) advanceChecking(ctx context.Context, handle RunHandle, frozen *FrozenRun, hopPath string, spawnEnv []string, integ *run.Integration) (IntegrationReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	report := IntegrationReport{IntegrationID: integ.ID.String(), State: string(integ.State)}
	subjectTree, err := c.runGit(ctx, frozen.RepositoryRoot, "rev-parse", integ.MergeCommitOID+"^{tree}")
	if err != nil {
		return report, fmt.Errorf("app: resolve candidate tree: %w", err)
	}
	receipt, found, err := c.settledIntegrationCheckReceipt(ctx, handle, integ.MergeCommitOID, subjectTree)
	if err != nil {
		return report, err
	}
	if found && receipt.Passed {
		if settleErr := c.settleIntegrationIntegrated(ctx, handle, frozen, integ, "existing settled passing receipt for this exact head"); settleErr != nil {
			return report, settleErr
		}
		return c.reportCurrentIntegrationState(ctx, handle, integ.ID)
	}
	if found {
		// A settled FAILING receipt for this exact candidate: the crash
		// window between the outcome record and the integration's
		// check-failed transition — apply the transition, never a fresh
		// execution against an already-failed candidate.
		if failErr := c.failIntegrationCheck(ctx, handle, integ, "settled failing receipt for this exact head"); failErr != nil {
			return report, failErr
		}
		return c.reportCurrentIntegrationState(ctx, handle, integ.ID)
	}
	interrupted, err := c.runIntegrationCheck(ctx, handle, frozen, integ, subjectTree, hopPath, spawnEnv)
	if err != nil {
		return report, err
	}
	report.Interrupted = interrupted
	return c.reportCurrentIntegrationState(ctx, handle, integ.ID)
}

// settledIntegrationCheckReceipt scans settled combined-check executions
// for one whose subject is EXACTLY (commit, tree), returning its receipt.
func (c *Controller) settledIntegrationCheckReceipt(ctx context.Context, handle RunHandle, subjectCommit, subjectTree string) (run.CheckReceipt, bool, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	var (
		receipt run.CheckReceipt
		found   bool
	)
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		ops, opErr := uow.Operations().ByKind(ctx, handle.runID, OpCheckRun)
		if opErr != nil {
			return opErr
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
			if !ok || outcome.Unknown {
				continue
			}
			receipt = run.CheckReceipt{Passed: outcome.ExitCode == 0, SubjectCommitOID: subjectCommit, SubjectTreeOID: subjectTree}
			found = true
			return nil
		}
		return nil
	})
	return receipt, found, err
}

// driveIntegrationReset drives one OpIntegrationReset operation for the
// current candidate: rollback commit R created from the recorded
// validated pre-merge tree with the rejected candidate as parent, R's
// OID persisted BEFORE the CAS, then the compare-and-swap ref move and
// the settling transaction (integration rolled-back, task consequence,
// manager notice).
func (c *Controller) driveIntegrationReset(ctx context.Context, handle RunHandle, frozen *FrozenRun, integ *run.Integration, reason string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per reset.
	opID, err := c.newOperationID()
	if err != nil {
		return err
	}
	ref := integrationRef(frozen.Snapshot.Workflow.IntegrationBranch)
	intent := integrationResetIntent{
		IntegrationID: integ.ID.String(), Ref: ref,
		RejectedOID: integ.MergeCommitOID, PremergeOID: integ.PremergeHeadOID,
		Reason: reason,
	}
	now := c.Clock.Now()
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpIntegrationReset, State: OperationPending, Intent: intent,
			CreatedAt: now, UpdatedAt: now,
		})
	}); err != nil {
		return fmt.Errorf("app: record integration.reset intent: %w", err)
	}
	return c.actAndSettleReset(ctx, handle, frozen, opID, &intent, now, integ.MergeCommitOID, "")
}

// actAndSettleReset performs a reset intent's two-step act — create and
// persist the rollback commit, then CAS — and its settling transaction.
// expectedOld is the CAS expected-old value and the rollback commit's
// parent (the rejected candidate ordinarily; the fencing rule's reset
// completion passes a later observed head). persistedR, when non-empty,
// is an already-recorded rollback OID whose creation step is skipped
// (the recovery rows "R recorded" cases).
func (c *Controller) actAndSettleReset(ctx context.Context, handle RunHandle, frozen *FrozenRun, opID identity.OperationID, intent *integrationResetIntent, intentTime time.Time, expectedOld, persistedR string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	rollbackOID := persistedR
	if rollbackOID == "" {
		// Step (i): create R and persist its OID in act evidence — a plain
		// store write — BEFORE any ref move. R carries the pre-merge
		// content and keeps the rejected candidate reachable as its
		// parent.
		if err := c.revalidateForDispatch(ctx, handle, true); err != nil {
			return fmt.Errorf("app: revalidate before rollback commit: %w", err)
		}
		actCtx, release := handle.actContext(ctx)
		created, commitErr := c.runGitEnv(actCtx, frozen.RepositoryRoot, gitDeterministicCommitEnv(intentTime),
			"commit-tree", intent.PremergeOID+"^{tree}", "-p", expectedOld, "-m", "hop rollback "+opID.String())
		release()
		if commitErr != nil {
			return c.markOperationReconciling(ctx, handle, opID, fmt.Sprintf("rollback commit could not be created: %v", commitErr))
		}
		rollbackOID = created
		if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
			op, getErr := uow.Operations().Get(ctx, opID)
			if getErr != nil {
				return getErr
			}
			op.ActEvidence = integrationResetEvidence{RollbackOID: rollbackOID, ParentOID: expectedOld}
			op.UpdatedAt = c.Clock.Now()
			return uow.Operations().Save(ctx, op)
		}); err != nil {
			return fmt.Errorf("app: persist rollback commit OID: %w", err)
		}
	}

	// Step (ii): the compare-and-swap ref move.
	if err := c.revalidateForDispatch(ctx, handle, true); err != nil {
		return fmt.Errorf("app: revalidate before reset CAS: %w", err)
	}
	actCtx, release := handle.actContext(ctx)
	_, casErr := c.runGit(actCtx, frozen.RepositoryRoot, "update-ref", intent.Ref, rollbackOID, expectedOld)
	release()
	if casErr != nil {
		observed, obsErr := c.runGit(ctx, frozen.RepositoryRoot, "rev-parse", "--verify", intent.Ref)
		if obsErr != nil || observed != rollbackOID {
			return c.markOperationReconciling(ctx, handle, opID, fmt.Sprintf("reset CAS refused (%v); observed ref %q", casErr, observed))
		}
	}

	return c.settleIntegrationTerminal(ctx, handle, frozen, identity.IntegrationID(intent.IntegrationID), integrationSettlement{
		OpID: opID, OpState: OperationSucceeded,
		OpOutcome:   fmt.Sprintf("ref reset to rollback commit %s", rollbackOID),
		TargetState: run.IntegrationRolledBack,
		Reason:      intent.Reason,
	})
}

// settleIntegrationIntegrated settles a passing combined candidate:
// integration integrated, task integrated, dependents released, and the
// controller info message to the manager — one transaction (the
// reference trace 1 commit).
func (c *Controller) settleIntegrationIntegrated(ctx context.Context, handle RunHandle, frozen *FrozenRun, integ *run.Integration, evidence string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per integration.
	noticeBody := fmt.Sprintf("integration %s integrated\ncandidate %s\n%s\n", integ.ID, integ.MergeCommitOID, evidence)
	notice, err := c.prepareControllerNotice(ctx, handle, frozen.Snapshot.StateRoot, noticeBody)
	if err != nil {
		return err
	}
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "integration integrated")
		if wfErr != nil {
			return wfErr
		}
		r, _, runErr := uow.Runs().Get(ctx, handle.runID)
		if runErr != nil {
			return runErr
		}
		if r.StopRequested {
			// Stop precedence: a passing combined check that lands after
			// the stop request records evidence and yields interrupted,
			// never integrated. The caller's outcome recording already
			// preserved the evidence; the candidate belongs to the stop
			// path's reset.
			return fmt.Errorf("%w: run %s", ErrStopRequested, handle.runID)
		}
		latest, rev, getErr := wf.Integrations().Get(ctx, integ.ID)
		if getErr != nil {
			return getErr
		}
		integrated, trErr := latest.Integrate(now)
		if trErr != nil {
			return trErr
		}
		if _, saveErr := wf.Integrations().Save(ctx, integrated, rev); saveErr != nil {
			return saveErr
		}
		task, taskRev, getErr := uow.Tasks().Get(ctx, integ.TaskID)
		if getErr != nil {
			return getErr
		}
		taskFrom := task.State
		taskIntegrated, trErr := task.Integrate(now)
		if trErr != nil {
			return trErr
		}
		if _, saveErr := uow.Tasks().Save(ctx, taskIntegrated, taskRev); saveErr != nil {
			return saveErr
		}
		generation := gen(handle.lease.Generation)
		if err := recordTransition(ctx, uow, EntityTask, integ.TaskID.String(), string(taskFrom), string(taskIntegrated.State), "combined candidate integrated", generation, now); err != nil {
			return err
		}
		// Dependents release in the same transaction.
		if err := releaseEligibleTasks(ctx, uow, wf, handle.runID, generation, now); err != nil {
			return err
		}
		return commitControllerNotice(ctx, wf, handle.runID, notice, now)
	})
}

// releaseEligibleTasks releases every pending dependent task whose
// prerequisites are all integrated, inside the caller's transaction —
// the dependency-release step run atomically with the integration that
// satisfied it.
func releaseEligibleTasks(ctx context.Context, uow UnitOfWork, wf WorkflowRepositories, runID identity.RunID, generation *int64, now time.Time) error {
	tasks, err := wf.TaskIndex().ByRun(ctx, runID)
	if err != nil {
		return err
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Seq < tasks[j].Seq })
	byID := make(map[identity.TaskID]run.Task, len(tasks))
	for i := range tasks {
		byID[tasks[i].ID] = tasks[i]
	}
	edges, err := wf.TaskDependencies().ByRun(ctx, runID)
	if err != nil {
		return err
	}
	edgesByTask := map[identity.TaskID][]run.TaskDependency{}
	for _, e := range edges {
		edgesByTask[e.TaskID] = append(edgesByTask[e.TaskID], e)
	}
	for i := range tasks {
		t := tasks[i]
		if t.State != run.TaskPending || t.Kind == run.TaskKindReview || !t.HasDependencies {
			continue
		}
		taskEdges := edgesByTask[t.ID]
		prereqs := make([]run.Task, 0, len(taskEdges))
		complete := true
		for _, e := range taskEdges {
			p, found := byID[e.PrerequisiteID]
			if !found {
				complete = false
				break
			}
			prereqs = append(prereqs, p)
		}
		if !complete || !run.ReleaseEligible(t, taskEdges, prereqs) {
			continue
		}
		_, rev, err := uow.Tasks().Get(ctx, t.ID)
		if err != nil {
			return err
		}
		released, err := t.Release(taskEdges, prereqs, now)
		if err != nil {
			return err
		}
		if _, err := uow.Tasks().Save(ctx, released, rev); err != nil {
			return err
		}
		byID[t.ID] = released
		if err := recordTransition(ctx, uow, EntityTask, t.ID.String(), string(run.TaskPending), string(run.TaskReady), "dependency release", generation, now); err != nil {
			return err
		}
	}
	return nil
}
