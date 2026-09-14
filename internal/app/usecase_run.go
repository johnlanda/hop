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
// BaseRef is the intended base commit resolved to an immutable object ID
// BEFORE the intent commits — never a mutable ref name — so takeover
// validation compares the candidate against exactly what was intended.
type worktreeCreateIntent struct {
	RepositoryRoot string `json:"repository_root"`
	Branch         string `json:"branch"`
	BaseRef        string `json:"base_ref"`
}

// worktreeCreateOutcome is the OpWorktreeCreate operation's outcome
// evidence: what Herdr created plus the provenance CommandRunner resolved
// afterward — Herdr's response carries no commit, so the base commit an
// adoption decision validates against is resolved separately.
type worktreeCreateOutcome struct {
	Info       WorktreeInfo `json:"info"`
	BaseCommit string       `json:"base_commit"`
}

// paneOpenIntent is the OpPaneOpen operation's intent payload.
type paneOpenIntent struct {
	Command []string `json:"command"`
	Cwd     string   `json:"cwd"`
	// WorkspaceID is the workspace the pane joins.
	WorkspaceID string `json:"workspace_id"`
	// Label is the pane's unique creation label (the operation ID).
	Label string `json:"label"`
	// IncarnationID and SessionID are what SubmissionStore.ClaimLaunch
	// validates a pre-binding launcher against: json_extract on the newest
	// pending pane.open operation for the current attempt binds the claim
	// to both, before falling back to the binding once one exists.
	IncarnationID identity.IncarnationID `json:"incarnation_id"`
	SessionID     identity.SessionID     `json:"session_id"`
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
	// SHA-256-object-format repositories are out of Phase 2 scope and are
	// refused here, before any side effect: result submission validates
	// 40-hex object ids and the check pipeline assumes them.
	objectFormat, err := c.runGit(ctx, req.RepositoryRoot, "rev-parse", "--show-object-format")
	if err != nil {
		return StartRunResult{}, RunHandle{}, fmt.Errorf("app: resolve repository object format: %w", err)
	}
	if objectFormat != "sha1" {
		return StartRunResult{}, RunHandle{}, fmt.Errorf("app: repository object format %q is unsupported in Phase 2 (sha1 only)", objectFormat)
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
	handle := newRunHandle(runID, lease)

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

// worktreeProvenance classifies a candidate checkout against the intended
// repository and frozen base commit (docs/plan/phase-2-design.md
// section 4, worktree.create adoption rule).
type worktreeProvenance int

const (
	// worktreeValid: the candidate shares the intended repository's
	// canonical common directory and its HEAD is the frozen base commit.
	worktreeValid worktreeProvenance = iota
	// worktreeAbsent: no git checkout answers at the candidate path.
	worktreeAbsent
	// worktreeUnrelated: a checkout of a different repository, or of the
	// intended repository at a different commit — a failure, not adoption.
	worktreeUnrelated
	// worktreeAmbiguous: the inspection itself could not be made; never
	// treated as absence or as failure.
	worktreeAmbiguous
)

// classifyWorktreeProvenance establishes whether the checkout at
// candidatePath is the intended one: canonical absolute git common
// directories of the intended repository and the candidate must be EQUAL
// (a path prefix is never provenance), and the candidate's HEAD commit
// must equal the frozen base object ID.
func (c *Controller) classifyWorktreeProvenance(ctx context.Context, candidatePath, repositoryRoot, expectedBaseOID string) (provenance worktreeProvenance, detail string) {
	repoCommon, repoStatus := c.gitCommonDir(ctx, repositoryRoot)
	if repoStatus != gitOK {
		return worktreeAmbiguous, fmt.Sprintf("intended repository %q common directory could not be resolved", repositoryRoot)
	}
	candCommon, candStatus := c.gitCommonDir(ctx, candidatePath)
	switch candStatus {
	case gitTransportError:
		return worktreeAmbiguous, fmt.Sprintf("candidate %q could not be inspected", candidatePath)
	case gitNonZero:
		return worktreeAbsent, fmt.Sprintf("no git checkout answers at %q", candidatePath)
	case gitOK:
	}
	if repoCommon != candCommon {
		return worktreeUnrelated, fmt.Sprintf("candidate at %q shares repository %q, not %q", candidatePath, candCommon, repoCommon)
	}

	head, headStatus := c.gitOutput(ctx, candidatePath, "rev-parse", "HEAD^{commit}")
	if headStatus != gitOK {
		return worktreeAmbiguous, fmt.Sprintf("candidate %q HEAD could not be resolved", candidatePath)
	}
	if head != expectedBaseOID {
		return worktreeUnrelated, fmt.Sprintf("candidate at %q is at commit %s, not the frozen base %s", candidatePath, head, expectedBaseOID)
	}
	return worktreeValid, ""
}

// gitStatus classifies one git invocation's result.
type gitStatus int

const (
	gitOK gitStatus = iota
	gitNonZero
	gitTransportError
)

// gitOutput runs one git subcommand against dir (via `git -C`, so the
// invocation is fully identified by its argv) and returns its trimmed
// stdout with a typed status.
func (c *Controller) gitOutput(ctx context.Context, dir string, args ...string) (string, gitStatus) {
	result, err := c.Commands.Run(ctx, Command{Argv: append([]string{"git", "-C", dir}, args...)})
	if err != nil {
		return "", gitTransportError
	}
	if result.ExitCode != 0 {
		return "", gitNonZero
	}
	return strings.TrimSpace(string(result.Stdout)), gitOK
}

// gitCommonDir resolves dir's canonical absolute git common directory. A
// relative answer (an older git ignoring --path-format) is resolved
// against dir before comparison.
func (c *Controller) gitCommonDir(ctx context.Context, dir string) (string, gitStatus) {
	out, status := c.gitOutput(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if status != gitOK {
		return "", status
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(dir, out)
	}
	return filepath.Clean(out), gitOK
}

// runGit runs one git subcommand against dir and returns its trimmed
// stdout, folding a non-zero exit into the error.
func (c *Controller) runGit(ctx context.Context, dir string, args ...string) (string, error) {
	result, err := c.Commands.Run(ctx, Command{Argv: append([]string{"git", "-C", dir}, args...)})
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("git -C %s %s: exit %d: %s", dir, strings.Join(args, " "), result.ExitCode, string(result.Stderr))
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
// controller crash that leaves a pending worktree.create operation is
// recovered by Resume (recoverWorktreeCreate), not StartRun, since
// StartRun only ever runs at the moment a run is first created.
func (c *Controller) createWorktree(ctx context.Context, handle RunHandle, ids generatedIdentities, repositoryRoot, branch string, now time.Time) (WorktreeInfo, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design (see Lease's own doc comment); this path runs once per run start, never in a hot loop.
	opID, err := c.newOperationID()
	if err != nil {
		return WorktreeInfo{}, err
	}
	baseOID, err := c.runGit(ctx, repositoryRoot, "rev-parse", "HEAD^{commit}")
	if err != nil {
		return WorktreeInfo{}, fmt.Errorf("app: resolve intended base commit: %w", err)
	}
	intent := worktreeCreateIntent{RepositoryRoot: repositoryRoot, Branch: branch, BaseRef: baseOID}
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpWorktreeCreate, State: OperationPending, Intent: intent,
			CreatedAt: now, UpdatedAt: now,
		})
	}); err != nil {
		return WorktreeInfo{}, fmt.Errorf("app: record worktree.create intent: %w", err)
	}

	if err := c.revalidateForDispatch(ctx, handle, false); err != nil {
		return WorktreeInfo{}, fmt.Errorf("app: revalidate before worktree.create: %w", err)
	}
	actCtx, release := handle.actContext(ctx)
	info, actErr := c.Runtime.CreateWorktree(actCtx, WorktreeRequest{
		RepositoryRoot: repositoryRoot, Branch: branch, BaseRef: baseOID,
	})
	provenance := worktreeAmbiguous
	provenanceDetail := ""
	if actErr == nil {
		provenance, provenanceDetail = c.classifyWorktreeProvenance(actCtx, info.Path, repositoryRoot, baseOID)
	}
	release()

	outcomeErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		op, getErr := uow.Operations().Get(ctx, opID)
		if getErr != nil {
			return getErr
		}
		op.UpdatedAt = c.Clock.Now()
		switch {
		case actErr != nil:
			// A transport error is ambiguous, never failure: the creation
			// may still have happened, and recovery validates provenance.
			op.State = OperationReconciling
			op.Outcome = actErr.Error()
			return uow.Operations().Save(ctx, op)
		case provenance == worktreeValid:
			r, _, runErr := uow.Runs().Get(ctx, handle.runID)
			if runErr != nil {
				return runErr
			}
			if _, createErr := uow.Worktrees().Create(ctx, run.NewWorktree(ids.Worktree, r.RepositoryID, handle.runID, info.Path, info.Branch)); createErr != nil {
				return createErr
			}
			op.State = OperationSucceeded
			op.ActEvidence = worktreeCreateOutcome{Info: info, BaseCommit: baseOID}
			return uow.Operations().Save(ctx, op)
		case provenance == worktreeUnrelated:
			op.State = OperationFailed
			op.Outcome = provenanceDetail
			return uow.Operations().Save(ctx, op)
		default:
			// Absent or ambiguous immediately after a successful create
			// response: reconciling, never a second create.
			op.State = OperationReconciling
			op.Outcome = provenanceDetail
			return uow.Operations().Save(ctx, op)
		}
	})
	var resultErr error
	switch {
	case actErr != nil:
		resultErr = fmt.Errorf("app: worktree.create: %w (operation %s is reconciling)", actErr, opID)
	case provenance == worktreeUnrelated:
		resultErr = fmt.Errorf("app: worktree.create provenance: %s", provenanceDetail)
	case provenance != worktreeValid:
		resultErr = fmt.Errorf("app: worktree.create provenance ambiguous: %s (operation %s is reconciling)", provenanceDetail, opID)
	}
	switch {
	case resultErr != nil && outcomeErr != nil:
		return WorktreeInfo{}, fmt.Errorf("%w; recording the outcome also failed: %w", resultErr, outcomeErr)
	case outcomeErr != nil:
		return WorktreeInfo{}, fmt.Errorf("app: record worktree.create outcome: %w", outcomeErr)
	case resultErr != nil:
		return WorktreeInfo{}, resultErr
	}
	return info, nil
}

// openWorkerPane drives the OpPaneOpen operation for a run's first launch,
// which is also the single "launch intent" moment: Run, Task, Attempt and
// Session all transition together in the intent transaction (the section 5
// reference traces' "intent → launching/active/launching/launching" line),
// before the pane is actually created.
func (c *Controller) openWorkerPane(ctx context.Context, handle RunHandle, ids generatedIdentities, worktree WorktreeInfo, hopPath, stateRoot string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; this path runs once per run start, never in a hot loop.
	return c.openPane(ctx, handle, ids, worktree, hopPath, stateRoot, run.LaunchInitial)
}

// openRelaunchPane drives the OpPaneOpen operation for a cold relaunch. The
// resume use case's own transaction already applied the Run/Attempt/Session
// transitions the relaunch needs (Run.Launch, Attempt.Relaunch, the new
// Session's own Launch), so this only journals the operation intent —
// applying them again here would be a second, invalid transition on values
// already at their target state.
func (c *Controller) openRelaunchPane(ctx context.Context, handle RunHandle, ids generatedIdentities, worktree WorktreeInfo, hopPath, stateRoot string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; this path runs once per relaunch.
	return c.openPane(ctx, handle, ids, worktree, hopPath, stateRoot, run.LaunchResume)
}

func (c *Controller) openPane(ctx context.Context, handle RunHandle, ids generatedIdentities, worktree WorktreeInfo, hopPath, stateRoot string, kind run.LaunchKind) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; this path runs once per launch or relaunch.
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
	intent := paneOpenIntent{Command: argv, Cwd: worktree.Path, WorkspaceID: worktree.WorkspaceID, Label: opID.String(), IncarnationID: ids.Incarnation, SessionID: ids.Session}
	now := c.Clock.Now()

	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		if kind == run.LaunchInitial {
			return applyLaunchIntent(ctx, uow, handle, ids, opID, intent, now)
		}
		return uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpPaneOpen, State: OperationPending, Intent: intent,
			CreatedAt: now, UpdatedAt: now,
		})
	}); err != nil {
		return fmt.Errorf("app: record pane.open intent: %w", err)
	}

	if err := c.revalidateForDispatch(ctx, handle, false); err != nil {
		return fmt.Errorf("app: revalidate before pane.open: %w", err)
	}
	// The server-process identity is observed immediately before the pane
	// is created and recorded in the creation binding: resume compares it
	// against a fresh observation to establish server continuity.
	serverInstance := c.observeServerInstance(ctx)
	actCtx, release := handle.actContext(ctx)
	paneHandle, actErr := c.Runtime.OpenWorkerPane(actCtx, WorkerPaneRequest{
		WorkspaceID: worktree.WorkspaceID, Cwd: worktree.Path, Command: argv, Env: env, Label: opID.String(),
	})
	if actErr != nil {
		if ref, found, findErr := c.Runtime.FindPaneByLabel(actCtx, opID.String()); findErr == nil && found {
			paneHandle = PaneHandle(ref)
			actErr = nil
		}
	}
	release()

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
		binding := run.NewRuntimeBinding(ids.Session, ids.Incarnation, "", serverInstance, paneHandle.WorkspaceID, paneHandle.TabID, paneHandle.PaneID, opID.String(), kind, op.UpdatedAt)
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
	runFrom := r.State
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
	if err := recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(runFrom), string(r.State), "launch intent journaled", generation, now); err != nil {
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
