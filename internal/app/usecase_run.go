package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// StartRunRequest is hop run's input, exactly as parsed from CLI flags and
// resolved values: RepositoryRoot is already the symlink-resolved absolute
// repository path, StateRoot is the absolute state root cmd/hop's single
// resolver produced, and HOPPath is the absolute path to the running hop
// executable (used both as the pane command and in the assignment's submit
// instruction).
type StartRunRequest struct {
	RepositoryRoot string
	Brief          string
	ControllerID   string
	StateRoot      string
	HOPPath        string
	EnvPassthrough []string
}

// StartRunResult is StartRun's success value: the run's identity and
// repository-scoped sequence number, exactly what `hop run` prints as
// "run <seq-label> <uuid> started".
type StartRunResult struct {
	RunID    string
	Sequence int
}

// worktreeCreateIntent is the OpWorktreeCreate operation's intent payload.
type worktreeCreateIntent struct {
	RepositoryRoot string
	Branch         string
	BaseRef        string
}

// worktreeCreateOutcome is the OpWorktreeCreate operation's outcome
// evidence: what Herdr created plus the provenance CommandRunner resolved
// afterward — Herdr's response carries no commit, so the base commit an
// adoption decision validates against is resolved separately.
type worktreeCreateOutcome struct {
	Info       WorktreeInfo
	BaseCommit string
}

// paneOpenIntent is the OpPaneOpen operation's intent payload.
type paneOpenIntent struct {
	Command     []string
	Cwd         string
	WorkspaceID string
	Label       string
}

// StartRun freezes a new run and drives it through worktree creation and
// worker-pane opening: InitializeRun, then the assignment-artifact write,
// then worktree.create, then pane.open, each as its own record-intent /
// act / record-outcome unit (docs/plan/phase-2-design.md section 4). It
// returns once the pane has been opened; the launch claim it produces is
// still ambiguous at that point — CorroborateLaunch drives it toward
// running. Any failure after InitializeRun releases the lease before
// returning, since the process that owns it is about to exit.
func (c *Controller) StartRun(ctx context.Context, req StartRunRequest) (StartRunResult, RunHandle, error) { //nolint:gocritic // hugeParam: StartRunRequest is the driving DTO for hop run, called once per controller process; a pointer would only complicate composition's call site.
	if err := validateStartRunRequest(req); err != nil {
		return StartRunResult{}, RunHandle{}, err
	}

	policy, err := c.Config.Load(ctx, req.RepositoryRoot)
	if err != nil {
		return StartRunResult{}, RunHandle{}, fmt.Errorf("app: load repository policy: %w", err)
	}
	if len(policy.CheckArgv) == 0 {
		return StartRunResult{}, RunHandle{}, errors.New("app: repository policy has no [check] command")
	}
	harness, err := parseHarness(policy.Harness)
	if err != nil {
		return StartRunResult{}, RunHandle{}, err
	}

	ids, err := c.generateRunIdentities()
	if err != nil {
		return StartRunResult{}, RunHandle{}, err
	}
	now := c.Clock.Now()

	assignmentPath := filepath.Join(req.StateRoot, "runs", ids.Run.String(), "artifacts", "assignment.md")
	fields := assignmentFields{
		RunID:          ids.Run.String(),
		TaskID:         ids.Task.String(),
		AttemptID:      ids.Attempt.String(),
		Brief:          req.Brief,
		AssignmentPath: assignmentPath,
		HOPPath:        req.HOPPath,
	}
	content := renderAssignment(&fields)
	assignmentDigest := sha256Hex(content)

	checkTimeout := policy.CheckTimeout
	if checkTimeout <= 0 {
		checkTimeout = 10 * time.Minute
	}

	spec := NewRunSpec{
		RepositoryRoot:     req.RepositoryRoot,
		RunID:              ids.Run,
		TaskID:             ids.Task,
		AttemptID:          ids.Attempt,
		SessionID:          ids.Session,
		WorktreeID:         ids.Worktree,
		IncarnationID:      ids.Incarnation,
		Brief:              req.Brief,
		BriefDigest:        sha256Hex([]byte(req.Brief)),
		InstructionsDigest: assignmentDigest,
		Snapshot: RunSnapshot{
			CheckArgv:       policy.CheckArgv,
			CheckTimeout:    checkTimeout,
			CheckRepeatable: policy.CheckRepeatable,
			EnvPolicy: EnvPolicy{
				Version:     EnvPolicyVersion1,
				Harness:     policy.Harness,
				Strip:       policy.EnvStrip,
				Passthrough: mergeUnique(policy.EnvPassthrough, req.EnvPassthrough),
				ProfileDir:  policy.ProfileDir,
			},
			Harness:          policy.Harness,
			ProfileDir:       policy.ProfileDir,
			StateRoot:        req.StateRoot,
			AssignmentPath:   assignmentPath,
			AssignmentDigest: assignmentDigest,
		},
		Harness:      harness,
		ControllerID: req.ControllerID,
		Now:          now,
	}
	if harness == run.HarnessClaude {
		spec.NativeSessionRef = c.IDs.NewID()
	}

	runID, lease, err := c.Store.InitializeRun(ctx, spec)
	if err != nil {
		return StartRunResult{}, RunHandle{}, fmt.Errorf("app: initialize run: %w", err)
	}
	handle := RunHandle{runID: runID, lease: lease}

	detail, err := c.Read.LoadRunStatus(ctx, runID)
	if err != nil {
		c.abortStart(ctx, lease)
		return StartRunResult{}, RunHandle{}, fmt.Errorf("app: load run status after initialize: %w", err)
	}
	result := StartRunResult{RunID: runID.String(), Sequence: detail.Sequence}

	if writeErr := c.Artifacts.WriteArtifact(ctx, assignmentPath, content); writeErr != nil {
		c.abortStart(ctx, lease)
		return StartRunResult{}, RunHandle{}, fmt.Errorf("app: write assignment artifact: %w", writeErr)
	}
	if recordErr := c.recordAssignmentArtifact(ctx, lease, ids.Run, assignmentPath, assignmentDigest); recordErr != nil {
		c.abortStart(ctx, lease)
		return StartRunResult{}, RunHandle{}, recordErr
	}

	branch := fmt.Sprintf("hop/run-%d", detail.Sequence)
	worktreeInfo, err := c.createWorktree(ctx, handle, ids, req.RepositoryRoot, branch, now)
	if err != nil {
		c.abortStart(ctx, lease)
		return StartRunResult{}, RunHandle{}, err
	}

	if err := c.openWorkerPane(ctx, handle, ids, worktreeInfo, req.HOPPath, req.StateRoot); err != nil {
		c.abortStart(ctx, lease)
		return StartRunResult{}, RunHandle{}, err
	}

	return result, handle, nil
}

// resolveWorktreeProvenance resolves the base commit a freshly created
// worktree checked out, and validates it shares the repository at
// repositoryRoot: Herdr's worktree.create response carries no commit, so
// this is the provenance an adoption decision later validates against
// (docs/plan/phase-2-design.md section 4). git-common-dir ties the
// worktree back to its repository regardless of the worktree's own path.
func (c *Controller) resolveWorktreeProvenance(ctx context.Context, worktreePath, repositoryRoot string) (string, error) {
	commonDir, err := c.runGit(ctx, worktreePath, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("resolve worktree repository: %w", err)
	}
	if !strings.HasPrefix(commonDir, repositoryRoot) {
		return "", fmt.Errorf("worktree at %q shares repository %q, not %q", worktreePath, commonDir, repositoryRoot)
	}
	commit, err := c.runGit(ctx, worktreePath, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve worktree base commit: %w", err)
	}
	return commit, nil
}

// runGit runs one git subcommand in dir and returns its trimmed stdout.
func (c *Controller) runGit(ctx context.Context, dir string, args ...string) (string, error) {
	result, err := c.Commands.Run(ctx, Command{Argv: append([]string{"git"}, args...), Dir: dir})
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("git %s: exit %d: %s", strings.Join(args, " "), result.ExitCode, string(result.Stderr))
	}
	return strings.TrimSpace(string(result.Stdout)), nil
}

// abortStart releases the lease on a StartRun failure that happens after
// InitializeRun already committed: the calling process is about to exit,
// and an un-released lease would otherwise sit held until its heartbeat
// TTL expires before a future hop resume could take the run over.
func (c *Controller) abortStart(ctx context.Context, lease Lease) {
	_ = c.Store.ReleaseLease(ctx, lease) //nolint:errcheck // best-effort: the caller is already returning the causal error.
}

// recordAssignmentArtifact records the assignment artifact's row as the
// outcome of its write.
func (c *Controller) recordAssignmentArtifact(ctx context.Context, lease Lease, runID identity.RunID, path, digest string) error {
	return c.withUnitOfWork(ctx, lease, func(uow UnitOfWork) error {
		artifactID, err := identity.ParseArtifactID(c.IDs.NewID())
		if err != nil {
			return err
		}
		return uow.Artifacts().Save(ctx, run.NewArtifact(artifactID, runID, run.ArtifactAssignment, path, digest))
	})
}

// createWorktree drives the OpWorktreeCreate operation: intent, then the
// external call, then outcome. Worktree.create is never retried blindly
// while a prior intent is unresolved (the operation decision table); a
// controller crash recovery path that finds a pending worktree.create
// operation is resume's job (Advance), not StartRun's, since StartRun only
// ever runs at the moment a run is first created.
func (c *Controller) createWorktree(ctx context.Context, handle RunHandle, ids generatedIdentities, repositoryRoot, branch string, now time.Time) (WorktreeInfo, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design (see Lease's own doc comment); this path runs once per run start, never in a hot loop.
	opID, err := c.newOperationID()
	if err != nil {
		return WorktreeInfo{}, err
	}
	intent := worktreeCreateIntent{RepositoryRoot: repositoryRoot, Branch: branch, BaseRef: "HEAD"}
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpWorktreeCreate, State: OperationPending, Intent: intent,
			CreatedAt: now, UpdatedAt: now,
		})
	}); err != nil {
		return WorktreeInfo{}, fmt.Errorf("app: record worktree.create intent: %w", err)
	}

	info, actErr := c.Runtime.CreateWorktree(ctx, WorktreeRequest{
		RepositoryRoot: repositoryRoot, Branch: branch, BaseRef: intent.BaseRef,
	})
	var baseCommit string
	if actErr == nil {
		baseCommit, actErr = c.resolveWorktreeProvenance(ctx, info.Path, repositoryRoot)
	}

	outcomeErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		op, getErr := uow.Operations().Get(ctx, opID)
		if getErr != nil {
			return getErr
		}
		op.UpdatedAt = c.Clock.Now()
		if actErr != nil {
			op.State = OperationFailed
			op.Outcome = actErr.Error()
			return uow.Operations().Save(ctx, op)
		}
		r, _, runErr := uow.Runs().Get(ctx, handle.runID)
		if runErr != nil {
			return runErr
		}
		if _, createErr := uow.Worktrees().Create(ctx, run.NewWorktree(ids.Worktree, r.RepositoryID, handle.runID, info.Path, info.Branch)); createErr != nil {
			return createErr
		}
		op.State = OperationSucceeded
		op.ActEvidence = worktreeCreateOutcome{Info: info, BaseCommit: baseCommit}
		return uow.Operations().Save(ctx, op)
	})
	switch {
	case actErr != nil && outcomeErr != nil:
		return WorktreeInfo{}, fmt.Errorf("app: worktree.create failed (%w) and recording the outcome failed (%w)", actErr, outcomeErr)
	case actErr != nil:
		return WorktreeInfo{}, fmt.Errorf("app: worktree.create: %w", actErr)
	case outcomeErr != nil:
		return WorktreeInfo{}, fmt.Errorf("app: record worktree.create outcome: %w", outcomeErr)
	}
	return info, nil
}

// openWorkerPane drives the OpPaneOpen operation, which is also the single
// "launch intent" moment: Run, Task, Attempt and Session all transition
// together in the intent transaction (the section 5 reference traces'
// "intent → launching/active/launching/launching" line), before the pane is
// actually created.
func (c *Controller) openWorkerPane(ctx context.Context, handle RunHandle, ids generatedIdentities, worktree WorktreeInfo, hopPath, stateRoot string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; this path runs once per run start, never in a hot loop.
	opID, err := c.newOperationID()
	if err != nil {
		return err
	}
	argv := []string{hopPath, "launch", "--run", ids.Run.String(), "--attempt", ids.Attempt.String()}
	env := map[string]string{
		"HOP_STATE_DIR":      stateRoot,
		"HOP_RUN_ID":         ids.Run.String(),
		"HOP_TASK_ID":        ids.Task.String(),
		"HOP_ATTEMPT_ID":     ids.Attempt.String(),
		"HOP_INCARNATION_ID": ids.Incarnation.String(),
	}
	intent := paneOpenIntent{Command: argv, Cwd: worktree.Path, WorkspaceID: worktree.WorkspaceID, Label: opID.String()}
	now := c.Clock.Now()

	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return applyLaunchIntent(ctx, uow, handle, ids, opID, intent, now)
	}); err != nil {
		return fmt.Errorf("app: record pane.open intent: %w", err)
	}

	paneHandle, actErr := c.Runtime.OpenWorkerPane(ctx, WorkerPaneRequest{
		WorkspaceID: worktree.WorkspaceID, Cwd: worktree.Path, Command: argv, Env: env, Label: opID.String(),
	})
	if actErr != nil {
		if ref, found, findErr := c.Runtime.FindPaneByLabel(ctx, opID.String()); findErr == nil && found {
			paneHandle = PaneHandle(ref)
			actErr = nil
		}
	}

	outcomeErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		op, getErr := uow.Operations().Get(ctx, opID)
		if getErr != nil {
			return getErr
		}
		op.UpdatedAt = c.Clock.Now()
		if actErr != nil {
			// A commit here must succeed even though the pane.open act
			// itself failed: recording "reconciling" as the outcome is the
			// whole point, so the closure returns nil (a successful save)
			// and actErr is surfaced to the caller below, after commit.
			op.State = OperationReconciling
			op.Outcome = actErr.Error()
			return uow.Operations().Save(ctx, op)
		}
		binding := run.NewRuntimeBinding(ids.Session, ids.Incarnation, "", paneHandle.WorkspaceID, paneHandle.TabID, paneHandle.PaneID, opID.String(), run.LaunchInitial, op.UpdatedAt)
		if bindErr := uow.Bindings().Create(ctx, binding); bindErr != nil {
			return bindErr
		}
		op.State = OperationSucceeded
		op.ActEvidence = paneHandle
		return uow.Operations().Save(ctx, op)
	})
	if outcomeErr != nil {
		return fmt.Errorf("app: record pane.open outcome: %w", outcomeErr)
	}
	if actErr != nil {
		return fmt.Errorf("app: pane.open: %w (operation %s is reconciling)", actErr, opID)
	}
	return nil
}

// applyLaunchIntent commits the launch-intent transitions shared by a first
// launch (openWorkerPane) and a cold relaunch (resume item 3): Run, Task,
// Attempt and Session move together, and the pane.open operation is
// journaled in the same transaction.
func applyLaunchIntent(ctx context.Context, uow UnitOfWork, handle RunHandle, ids generatedIdentities, opID identity.OperationID, intent paneOpenIntent, now time.Time) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; this path runs once per launch, never in a hot loop.
	r, rRev, err := uow.Runs().Get(ctx, handle.runID)
	if err != nil {
		return err
	}
	if r, err = r.Launch(now); err != nil {
		return err
	}
	if _, saveErr := uow.Runs().Save(ctx, r, rRev); saveErr != nil {
		return saveErr
	}

	t, tRev, err := uow.Tasks().Get(ctx, ids.Task)
	if err != nil {
		return err
	}
	taskFrom := t.State
	if t, err = t.Activate(now); err != nil {
		return err
	}
	if _, saveErr := uow.Tasks().Save(ctx, t, tRev); saveErr != nil {
		return saveErr
	}

	a, aRev, err := uow.Attempts().Get(ctx, ids.Attempt)
	if err != nil {
		return err
	}
	attemptFrom := a.State
	if a, err = a.Launch(now); err != nil {
		return err
	}
	if _, saveErr := uow.Attempts().Save(ctx, a, aRev); saveErr != nil {
		return saveErr
	}

	s, sRev, err := uow.Sessions().Get(ctx, ids.Session)
	if err != nil {
		return err
	}
	sessionFrom := s.State
	if s, err = s.Launch(now); err != nil {
		return err
	}
	if _, saveErr := uow.Sessions().Save(ctx, s, sRev); saveErr != nil {
		return saveErr
	}

	generation := gen(handle.lease.Generation)
	if err := recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(run.RunCreated), string(r.State), "launch intent journaled", generation, now); err != nil {
		return err
	}
	if err := recordTransition(ctx, uow, EntityTask, ids.Task.String(), string(taskFrom), string(t.State), "attempt launched", generation, now); err != nil {
		return err
	}
	if err := recordTransition(ctx, uow, EntityAttempt, ids.Attempt.String(), string(attemptFrom), string(a.State), "launch intent", generation, now); err != nil {
		return err
	}
	if err := recordTransition(ctx, uow, EntitySession, ids.Session.String(), string(sessionFrom), string(s.State), "launch intent", generation, now); err != nil {
		return err
	}

	return uow.Operations().Create(ctx, Operation{
		ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
		Kind: OpPaneOpen, State: OperationPending, Intent: intent,
		CreatedAt: now, UpdatedAt: now,
	})
}

// validateStartRunRequest checks preconditions hop run refuses before any
// side effect (docs/plan/phase-2-design.md section 6).
func validateStartRunRequest(req StartRunRequest) error { //nolint:gocritic // hugeParam: StartRunRequest is the driving DTO for hop run, called once per controller process.
	if req.RepositoryRoot == "" {
		return errors.New("app: repository root is required")
	}
	if req.Brief == "" {
		return errors.New("app: brief is required")
	}
	if req.ControllerID == "" {
		return errors.New("app: controller id is required")
	}
	if !filepath.IsAbs(req.StateRoot) {
		return fmt.Errorf("app: state root %q is not absolute", req.StateRoot)
	}
	if !filepath.IsAbs(req.HOPPath) {
		return fmt.Errorf("app: hop executable path %q is not absolute", req.HOPPath)
	}
	if strings.ContainsRune(req.HOPPath, '\'') {
		return fmt.Errorf("app: hop executable path %q contains a single quote, which the launch-line grammar cannot escape", req.HOPPath)
	}
	if hasControlByte(req.HOPPath) {
		return fmt.Errorf("app: hop executable path %q contains a control character", req.HOPPath)
	}
	return nil
}

// hasControlByte reports whether s carries any byte below 0x20.
func hasControlByte(s string) bool {
	for i := range len(s) {
		if s[i] < 0x20 {
			return true
		}
	}
	return false
}

// parseHarness validates name as one of the harnesses the domain knows.
func parseHarness(name string) (run.Harness, error) {
	switch run.Harness(name) {
	case run.HarnessClaude, run.HarnessCodex, run.HarnessOpenCode:
		return run.Harness(name), nil
	default:
		return "", fmt.Errorf("app: harness %q is not one of claude, codex, opencode", name)
	}
}

// mergeUnique concatenates a and b, keeping only the first occurrence of
// each name in order.
func mergeUnique(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var merged []string
	for _, name := range append(append([]string{}, a...), b...) {
		if seen[name] {
			continue
		}
		seen[name] = true
		merged = append(merged, name)
	}
	return merged
}

// sha256Hex returns the lowercase hex SHA-256 digest of content.
func sha256Hex[T string | []byte](content T) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
