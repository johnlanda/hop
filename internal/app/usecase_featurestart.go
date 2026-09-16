package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// Feature-run bootstrap operation kinds (docs/plan/phase-3-design.md
// sections 4, 6 and 9). Declared here, with the operations they journal,
// as usecase_integration.go declares its own.
const (
	// OpIntegrationInit is the create-only compare-and-swap that creates
	// the run's integration branch at the frozen base commit
	// (`git update-ref <ref> <base> ""`). It needs no fencing: its
	// expected-old value is the never-again-current empty value.
	OpIntegrationInit OperationKind = "integration.init"
	// OpWorkspaceCreate is the manager's placement: a plain workspace at
	// the repository root whose creation label is the operation ID.
	OpWorkspaceCreate OperationKind = "workspace.create"
)

// ErrIntegrationBranchExists reports that the run's integration branch
// already exists in the repository and was not created by this run: the
// create-only compare-and-swap was refused (or, before any side effect,
// the advisory pre-check found the ref).
var ErrIntegrationBranchExists = errors.New("app: the integration branch already exists")

// Bootstrap act outcomes that are RECORDED states, not failures to record:
// the operation journal already holds the reconciling or failed outcome
// the error names, so a continuation reports them rather than aborting.
var (
	errIntegrationInitUnresolved  = errors.New("app: creating the integration branch is unresolved")
	errIntegrationInitRefused     = errors.New("app: git refused to create the integration branch")
	errManagerWorkspaceUnresolved = errors.New("app: the manager's workspace.create is unresolved")
	errManagerPaneUnresolved      = errors.New("app: the manager's pane.open is unresolved")
)

// maxFeatureFreezeAttempts bounds StartFeatureRun's freeze-and-initialize
// retries when another run takes the predicted sequence first.
const maxFeatureFreezeAttempts = 3

// workspaceRecoveryDeadline bounds the wait for a lost workspace.create
// response to surface by label: within it an absent label is an in-flight
// request; past it the operation is reconciling. A second create is never
// dispatched either way.
const workspaceRecoveryDeadline = 120 * time.Second

// integrationInitIntent is the OpIntegrationInit intent payload.
type integrationInitIntent struct {
	RepositoryRoot string `json:"repository_root"`
	Ref            string `json:"ref"`
	BaseOID        string `json:"base_oid"`
}

// integrationInitEvidence is the OpIntegrationInit act evidence: the
// create-only update-ref's exit status and stderr, and the ref value read
// back afterward when it could be observed.
type integrationInitEvidence struct {
	ExitCode    int    `json:"exit_code"`
	Stderr      string `json:"stderr,omitempty"`
	ObservedRef string `json:"observed_ref,omitempty"`
	SymbolicRef bool   `json:"symbolic_ref,omitempty"`
	Adopted     bool   `json:"adopted,omitempty"`
}

// workspaceCreateIntent is the OpWorkspaceCreate intent payload. The
// placed session is recorded under its own key, never "session_id", which
// the launch-claim fallback reads from pane.open intents only.
type workspaceCreateIntent struct {
	Cwd              string `json:"cwd"`
	Label            string `json:"label"`
	ManagerSessionID string `json:"manager_session_id"`
}

// ResolveRunWorkflow decides which start use case `hop run` drives
// (docs/plan/phase-3-design.md section 10): an explicit override ("solo"
// or "feature", the --workflow flag) wins; without one the repository
// policy's [workflow] mode decides, solo by default. A policy that cannot
// be loaded resolves to solo, whose own load reports the refusal exactly
// as Phase 2 does. Any other override is refused before any side effect.
func (c *Controller) ResolveRunWorkflow(ctx context.Context, repositoryRoot, override string) (string, error) {
	switch override {
	case WorkflowModeSolo, WorkflowModeFeature:
		return override, nil
	case "":
	default:
		return "", fmt.Errorf("%w: --workflow must be %q or %q", ErrStartRefused, WorkflowModeSolo, WorkflowModeFeature)
	}
	policy, err := c.Config.Load(ctx, repositoryRoot)
	if err != nil {
		return WorkflowModeSolo, nil //nolint:nilerr // the solo path reloads the policy and reports this failure with Phase 2's exact refusal.
	}
	if policy.WorkflowMode == WorkflowModeFeature {
		return WorkflowModeFeature, nil
	}
	return WorkflowModeSolo, nil
}

// StartFeatureRun is the feature-mode counterpart of StartRun
// (docs/plan/phase-3-design.md sections 1, 6, 9 and 10): it validates the
// request and the repository policy (the shared feature defaults and
// requireds applied as the --workflow feature override), resolves the
// repository HEAD to the frozen base commit, predicts the run sequence
// and refuses an integration branch that already exists — every refusal
// so far wraps ErrStartRefused, before any side effect — then freezes the
// role artifacts and the workflow snapshot, and commits InitializeRun's
// feature shape (run, snapshot, manager session, lease), re-freezing
// against a fresh prediction when another run took the predicted
// sequence. After InitializeRun it writes and records the manager's brief
// assignment, then drives integration.init, workspace.create and the
// manager's pane.open, each as record-intent, revalidate, act,
// record-outcome. Any failure after InitializeRun releases the lease and
// leaves the run for hop resume or hop stop. Errors never echo
// environment values or paths.
func (c *Controller) StartFeatureRun(ctx context.Context, req StartRunRequest) (StartRunResult, RunHandle, error) { //nolint:gocritic // hugeParam: StartRunRequest is the driving DTO for hop run, called once per controller process.
	if err := validateStartRunRequest(req); err != nil {
		return StartRunResult{}, RunHandle{}, fmt.Errorf("%w: %w", ErrStartRefused, err)
	}
	if c.Workspaces == nil {
		return StartRunResult{}, RunHandle{}, fmt.Errorf("%w: StartFeatureRun has no workspace runtime", ErrFeatureModeUnsupported)
	}
	policy, harness, err := c.loadFeaturePolicy(ctx, req.RepositoryRoot)
	if err != nil {
		return StartRunResult{}, RunHandle{}, err
	}
	baseOID, err := c.resolveFeatureBase(ctx, req.RepositoryRoot)
	if err != nil {
		return StartRunResult{}, RunHandle{}, err
	}

	runID, err := identity.ParseRunID(c.IDs.NewID())
	if err != nil {
		return StartRunResult{}, RunHandle{}, fmt.Errorf("app: generate run id: %w", err)
	}
	managerID, err := identity.ParseSessionID(c.IDs.NewID())
	if err != nil {
		return StartRunResult{}, RunHandle{}, fmt.Errorf("app: generate manager session id: %w", err)
	}
	now := c.Clock.Now()

	assignmentPath := filepath.Join(req.StateRoot, "runs", runID.String(), "artifacts", "assignment.md")
	content := renderManagerAssignment(&managerAssignmentFields{
		RunID:          runID.String(),
		Brief:          req.Brief,
		AssignmentPath: assignmentPath,
		RolePath:       roleArtifactPath(req.StateRoot, runID, "manager"),
		CribPath:       workerProtocolCribPath(req.StateRoot, runID),
		HOPPath:        req.HOPPath,
	})
	assignmentDigest := sha256Hex(content)

	checkTimeout := policy.CheckTimeout
	if checkTimeout <= 0 {
		checkTimeout = 10 * time.Minute
	}
	spec := NewRunSpec{
		RepositoryRoot: req.RepositoryRoot,
		RunID:          runID,
		SessionID:      managerID,
		Brief:          req.Brief,
		BriefDigest:    sha256Hex([]byte(req.Brief)),
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

	lease, err := c.freezeAndInitialize(ctx, &spec, &policy, baseOID)
	if err != nil {
		return StartRunResult{}, RunHandle{}, err
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
		return StartRunResult{}, RunHandle{}, fmt.Errorf("app: the manager assignment artifact could not be written for run %s", runID)
	}
	if recordErr := c.recordAssignmentArtifact(ctx, lease, runID, assignmentPath, assignmentDigest); recordErr != nil {
		c.abortStart(ctx, lease)
		return StartRunResult{}, RunHandle{}, recordErr
	}

	intent := integrationInitIntent{
		RepositoryRoot: req.RepositoryRoot,
		Ref:            integrationRef(spec.Snapshot.Workflow.IntegrationBranch),
		BaseOID:        baseOID,
	}
	if initErr := c.initIntegrationBranch(ctx, handle, &intent); initErr != nil {
		c.abortStart(ctx, lease)
		return StartRunResult{}, RunHandle{}, initErr
	}
	workspaceID, err := c.placeManagerWorkspace(ctx, handle, managerID, req.RepositoryRoot)
	if err != nil {
		c.abortStart(ctx, lease)
		return StartRunResult{}, RunHandle{}, err
	}
	if err := c.openManagerPane(ctx, handle, managerID, workspaceID, req.RepositoryRoot, req.HOPPath, req.StateRoot); err != nil {
		c.abortStart(ctx, lease)
		return StartRunResult{}, RunHandle{}, err
	}
	return result, handle, nil
}

// loadFeaturePolicy loads the repository policy for a feature run and
// applies the --workflow feature override through the shared helpers,
// then pre-reads every role instructions file so an unreadable or empty
// one is refused before the freeze writes anything. The policy-load and
// [check] refusals keep StartRun's exact text; every refusal wraps
// ErrStartRefused. Role-file errors name the policy key, never the path.
func (c *Controller) loadFeaturePolicy(ctx context.Context, repositoryRoot string) (RunPolicy, run.Harness, error) {
	policy, err := c.Config.Load(ctx, repositoryRoot)
	if err != nil {
		return RunPolicy{}, "", fmt.Errorf("%w: load repository policy: %w", ErrStartRefused, err)
	}
	if len(policy.CheckArgv) == 0 {
		return RunPolicy{}, "", fmt.Errorf("%w: repository policy has no [check] command", ErrStartRefused)
	}
	harness, err := parseHarness(policy.Harness)
	if err != nil {
		return RunPolicy{}, "", fmt.Errorf("%w: %w", ErrStartRefused, err)
	}
	policy.WorkflowMode = WorkflowModeFeature
	ApplyWorkflowDefaults(&policy)
	if err := ValidateFeaturePolicy(&policy); err != nil {
		return RunPolicy{}, "", fmt.Errorf("%w: %w", ErrStartRefused, err)
	}
	for _, role := range []struct{ key, path string }{
		{"roles.manager.instructions", policy.ManagerRole},
		{"roles.implementer.instructions", policy.ImplementerRole},
		{"roles.reviewer.instructions", policy.ReviewerRole},
	} {
		roleContent, readErr := c.Artifacts.ReadArtifact(ctx, role.path)
		if readErr != nil {
			return RunPolicy{}, "", fmt.Errorf("%w: the %s file could not be read", ErrStartRefused, role.key)
		}
		if len(roleContent) == 0 {
			return RunPolicy{}, "", fmt.Errorf("%w: the %s file is empty; an empty role artifact cannot instruct a session", ErrStartRefused, role.key)
		}
	}
	return policy, harness, nil
}

// resolveFeatureBase refuses an unsupported object format and resolves
// the repository HEAD to the commit object ID the integration branch is
// created at, before any side effect.
func (c *Controller) resolveFeatureBase(ctx context.Context, repositoryRoot string) (string, error) {
	if !filepath.IsAbs(c.GitExecutable) {
		return "", fmt.Errorf("%w: the git executable is not configured as an absolute path", ErrStartRefused)
	}
	objectFormat, status := c.gitOutput(ctx, repositoryRoot, "rev-parse", "--show-object-format")
	if status != gitOK {
		return "", fmt.Errorf("%w: the repository object format could not be resolved", ErrStartRefused)
	}
	if objectFormat != "sha1" {
		return "", fmt.Errorf("%w: repository object format %q is unsupported (sha1 only)", ErrStartRefused, objectFormat)
	}
	baseOID, status := c.gitOutput(ctx, repositoryRoot, "rev-parse", "HEAD^{commit}")
	if status != gitOK || !isObjectID(baseOID) {
		return "", fmt.Errorf("%w: the repository HEAD does not resolve to a commit; a feature run's integration branch needs a base commit", ErrStartRefused)
	}
	return baseOID, nil
}

// isObjectID reports whether s is a 40-hex lowercase SHA-1 object ID.
func isObjectID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := range len(s) {
		if (s[i] < '0' || s[i] > '9') && (s[i] < 'a' || s[i] > 'f') {
			return false
		}
	}
	return true
}

// freezeAndInitialize predicts the run's sequence, refuses a predicted
// integration branch that already exists (advisory: the create-only CAS
// stays the authority), freezes the workflow artifacts against the
// prediction and commits InitializeRun's feature shape. When another run
// took the predicted sequence first (ErrRunSequenceMismatch, nothing
// committed) it re-predicts and re-freezes under the same run ID —
// the role copies are rewritten by temp-file-then-rename, so the digests
// the committed snapshot carries are the final freeze's — at most
// maxFeatureFreezeAttempts times; exhaustion is an ordinary failure with
// nothing committed. Artifacts a refused freeze wrote stay in place,
// exactly as Phase 2 leaves a failed start's files.
func (c *Controller) freezeAndInitialize(ctx context.Context, spec *NewRunSpec, policy *RunPolicy, baseOID string) (Lease, error) {
	for attempt := 1; ; attempt++ {
		seq, err := c.predictRunSequence(ctx, spec.RepositoryRoot)
		if err != nil {
			return Lease{}, err
		}
		branch := IntegrationBranchName(seq)
		if seen := c.observeRef(ctx, spec.RepositoryRoot, integrationRef(branch)); seen.symbolic || seen.value != "" {
			return Lease{}, fmt.Errorf("%w: %w", ErrStartRefused, integrationBranchCollision(integrationRef(branch)))
		}
		workflow, err := c.FreezeWorkflowArtifacts(ctx, &WorkflowFreezeRequest{
			Policy: *policy, RunID: spec.RunID, RunSeq: seq, StateRoot: spec.Snapshot.StateRoot,
		})
		if err != nil {
			return Lease{}, fmt.Errorf("app: the workflow artifacts could not be frozen for run %s", spec.RunID)
		}
		workflow.BaseCommitOID = baseOID
		spec.Snapshot.Workflow = workflow
		_, lease, err := c.Store.InitializeRun(ctx, *spec)
		if errors.Is(err, ErrRunSequenceMismatch) && attempt < maxFeatureFreezeAttempts {
			continue
		}
		if err != nil {
			return Lease{}, fmt.Errorf("app: initialize run: %w", err)
		}
		return lease, nil
	}
}

// predictRunSequence is the sequence InitializeRun will assign the
// repository's next run if no other run commits first.
func (c *Controller) predictRunSequence(ctx context.Context, repositoryRoot string) (int, error) {
	runs, err := c.Read.ListRuns(ctx, repositoryRoot)
	if err != nil {
		return 0, fmt.Errorf("app: list the repository's runs: %w", err)
	}
	highest := 0
	for i := range runs {
		highest = max(highest, runs[i].Sequence)
	}
	return highest + 1, nil
}

// refObservation is one read of an integration ref.
type refObservation struct {
	// observed is false when either read could not be made at all.
	observed bool
	// symbolic is true for a symbolic ref, dangling or not: never HOP's,
	// always a collision.
	symbolic bool
	// value is a direct ref's object ID; "" when the ref was not observed
	// present.
	value string
}

// observeRef reads ref in two pinned steps (the process adapter's real-git
// probe TestGitRefSemanticsThroughRunner): `git symbolic-ref -q <ref>`
// exits 0 with the target for a symbolic ref (dangling included) and 1
// with no output for a direct or absent ref — any other status is an
// unobservable read; then `git rev-parse --verify <ref>`, exactly as the
// integration operations read the live ref, returns an existing direct
// ref's object ID (G1/G3). A non-zero rev-parse is "not observed present":
// never proof of absence, only permission to attempt the create-only
// --no-deref CAS, which is itself the authoritative existence test.
func (c *Controller) observeRef(ctx context.Context, repositoryRoot, ref string) refObservation {
	if !filepath.IsAbs(c.GitExecutable) {
		return refObservation{}
	}
	symbolic, err := c.Commands.Run(ctx, Command{Argv: []string{c.GitExecutable, "-C", repositoryRoot, "symbolic-ref", "-q", ref}})
	switch {
	case err != nil:
		return refObservation{}
	case symbolic.ExitCode == 0:
		return refObservation{observed: true, symbolic: true}
	case symbolic.ExitCode != 1:
		return refObservation{}
	}
	out, status := c.gitOutput(ctx, repositoryRoot, "rev-parse", "--verify", ref)
	switch status {
	case gitOK:
		return refObservation{observed: true, value: out}
	case gitNonZero:
		return refObservation{observed: true}
	default:
		return refObservation{}
	}
}

// integrationBranchCollision renders the collision refusal: the ref by
// name, and the instruction to inspect it before removing or renaming it,
// since it may hold someone's work. No path is echoed.
func integrationBranchCollision(ref string) error {
	return fmt.Errorf("%w: %s is already present in the repository; inspect it before removing or renaming it, since it may hold someone's work", ErrIntegrationBranchExists, ref)
}

// initIntegrationBranch journals a fresh integration.init intent and acts
// on it. A fresh intent means nothing of this operation was ever
// dispatched, so any ref the refused CAS finds present is foreign — even
// one at the base commit.
func (c *Controller) initIntegrationBranch(ctx context.Context, handle RunHandle, intent *integrationInitIntent) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per bootstrap.
	opID, err := c.newOperationID()
	if err != nil {
		return err
	}
	now := c.Clock.Now()
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpIntegrationInit, State: OperationPending, Intent: *intent,
			CreatedAt: now, UpdatedAt: now,
		})
	}); err != nil {
		return fmt.Errorf("app: record integration.init intent: %w", err)
	}
	return c.actIntegrationInit(ctx, handle, opID, intent, false, false)
}

// actIntegrationInit revalidates, runs the create-only CAS
// `git update-ref --no-deref <ref> <base> ""` and records the outcome
// (G1/G3-pinned: it succeeds only when the ref does not exist and leaves
// an existing ref unchanged; --no-deref makes an existing symbolic ref,
// dangling or not, refuse instead of creating its target). A symbolic ref
// read back is always a collision, never adopted. Exit 0 succeeds. A refused CAS reads the
// ref back: present at the base commit is adopted only when adoptAtBase
// (a recovered intent whose earlier dispatch may have landed), any other
// present value is a collision and fails; a ref still not observed fails a
// fresh intent (nothing was created) and leaves a recovered one
// reconciling. A runner failure is ambiguous: reconciling. stopping marks
// the stop path's completing act, which revalidates the lease but not the
// stop flag.
func (c *Controller) actIntegrationInit(ctx context.Context, handle RunHandle, opID identity.OperationID, intent *integrationInitIntent, adoptAtBase, stopping bool) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per act.
	if err := c.revalidateForDispatch(ctx, handle, stopping); err != nil {
		return fmt.Errorf("app: revalidate before integration.init: %w", err)
	}
	if !filepath.IsAbs(c.GitExecutable) {
		return errors.New("app: git executable is not configured as an absolute path")
	}
	actCtx, release := handle.actContext(ctx)
	result, runErr := c.Commands.Run(actCtx, Command{Argv: []string{c.GitExecutable, "-C", intent.RepositoryRoot, "update-ref", "--no-deref", intent.Ref, intent.BaseOID, ""}})
	var seen refObservation
	if runErr == nil {
		seen = c.observeRef(actCtx, intent.RepositoryRoot, intent.Ref)
	}
	release()
	readback, observed := seen.value, seen.observed

	evidence := integrationInitEvidence{ExitCode: result.ExitCode, Stderr: string(result.Stderr), ObservedRef: readback, SymbolicRef: seen.symbolic}
	state := OperationReconciling
	outcome := ""
	var resultErr error
	switch {
	case runErr != nil:
		outcome = "the create-only update-ref could not be run; it may or may not have landed"
		resultErr = fmt.Errorf("%w: %s (operation %s is reconciling); run hop resume %s", errIntegrationInitUnresolved, intent.Ref, opID, handle.runID)
	case result.ExitCode == 0 && !seen.symbolic:
		state = OperationSucceeded
		outcome = "integration branch created at the frozen base commit"
	case seen.symbolic:
		state = OperationFailed
		outcome = "integration branch collision: the ref is a symbolic ref, never HOP's"
		resultErr = fmt.Errorf("%w; then run hop resume %s, or hop stop %s", integrationBranchCollision(intent.Ref), handle.runID, handle.runID)
	case !observed:
		outcome = "the create-only update-ref was refused and the ref could not be read back"
		resultErr = fmt.Errorf("%w: %s (operation %s is reconciling); run hop resume %s", errIntegrationInitUnresolved, intent.Ref, opID, handle.runID)
	case readback != "" && adoptAtBase && readback == intent.BaseOID:
		state = OperationSucceeded
		evidence.Adopted = true
		outcome = "an earlier dispatch of this operation created the branch at the frozen base commit; adopted"
	case readback != "":
		state = OperationFailed
		outcome = "integration branch collision: the ref exists and was not created by this run"
		resultErr = fmt.Errorf("%w; then run hop resume %s, or hop stop %s", integrationBranchCollision(intent.Ref), handle.runID, handle.runID)
	case adoptAtBase:
		outcome = "the create-only update-ref was refused but the ref is not observed present"
		resultErr = fmt.Errorf("%w: %s (operation %s is reconciling); run hop resume %s", errIntegrationInitUnresolved, intent.Ref, opID, handle.runID)
	default:
		state = OperationFailed
		outcome = "git refused to create the integration branch; its stderr is recorded as act evidence"
		resultErr = fmt.Errorf("%w: %s (recorded in operation %s); inspect the repository, then run hop resume %s, or hop stop %s", errIntegrationInitRefused, intent.Ref, opID, handle.runID, handle.runID)
	}

	outcomeErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		op, getErr := uow.Operations().Get(ctx, opID)
		if getErr != nil {
			return getErr
		}
		if op.State != OperationPending && op.State != OperationReconciling {
			return nil
		}
		op.State = state
		op.ActEvidence = evidence
		op.Outcome = outcome
		op.UpdatedAt = c.Clock.Now()
		return uow.Operations().Save(ctx, op)
	})
	switch {
	case resultErr != nil && outcomeErr != nil:
		return fmt.Errorf("%w; recording the outcome also failed: %w", resultErr, outcomeErr)
	case outcomeErr != nil:
		return fmt.Errorf("app: record integration.init outcome: %w", outcomeErr)
	}
	return resultErr
}

// placeManagerWorkspace drives the manager's workspace.create
// (docs/plan/phase-3-design.md section 4 row): intent (the label is the
// operation ID), revalidate, CreateWorkspace at the repository root with
// no env — the root pane is a login shell, never a HOP pane — then, when
// the response is lost, recovery by the creation label (the S8-pinned
// sole-tab/sole-root-pane descent, TestSpikeWorkspaceCreateResponseShapeAndEnv):
// found is adopted; absent or a failed or ambiguous lookup leaves the
// operation reconciling. A second create is never dispatched.
func (c *Controller) placeManagerWorkspace(ctx context.Context, handle RunHandle, managerID identity.SessionID, repositoryRoot string) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per bootstrap.
	opID, err := c.newOperationID()
	if err != nil {
		return "", err
	}
	intent := workspaceCreateIntent{Cwd: repositoryRoot, Label: opID.String(), ManagerSessionID: managerID.String()}
	now := c.Clock.Now()
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpWorkspaceCreate, State: OperationPending, Intent: intent,
			CreatedAt: now, UpdatedAt: now,
		})
	}); err != nil {
		return "", fmt.Errorf("app: record workspace.create intent: %w", err)
	}

	if err := c.revalidateForDispatch(ctx, handle, false); err != nil {
		return "", fmt.Errorf("app: revalidate before workspace.create: %w", err)
	}
	actCtx, release := handle.actContext(ctx)
	created, actErr := c.Workspaces.CreateWorkspace(actCtx, WorkspaceRequest{Cwd: repositoryRoot, Label: intent.Label})
	outcome := ""
	if actErr != nil {
		ref, found, findErr := c.Workspaces.FindWorkspaceByLabel(actCtx, intent.Label)
		switch {
		case findErr != nil:
			outcome = "workspace.create failed and the label lookup failed or was ambiguous: " + actErr.Error() + "; " + findErr.Error()
		case found:
			created = WorkspaceHandle(ref)
			actErr = nil
		default:
			outcome = "workspace.create failed and no workspace carries the creation label: " + actErr.Error()
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
			op.State = OperationReconciling
			op.Outcome = outcome
			return uow.Operations().Save(ctx, op)
		}
		op.State = OperationSucceeded
		op.ActEvidence = created
		return uow.Operations().Save(ctx, op)
	})
	if outcomeErr != nil {
		return "", fmt.Errorf("app: record workspace.create outcome: %w", outcomeErr)
	}
	if actErr != nil {
		return "", fmt.Errorf("%w (operation %s is reconciling; the transport error is recorded there); run hop resume %s", errManagerWorkspaceUnresolved, opID, handle.runID)
	}
	return created.WorkspaceID, nil
}

// openManagerPane drives the manager's pane.open (docs/plan/phase-3-design.md
// section 6 role table): the intent transaction moves the run to
// launching (from created, or resuming for a continued bootstrap) and the
// manager session reserved -> launching, journaling the operation with
// the pane argv `<hop> launch --run <run> --session <manager>`, the
// repository root as cwd and a fresh incarnation; then revalidation,
// OpenWorkerPane into the manager's workspace with the manager env
// (HOP_STATE_DIR, HOP_RUN_ID, HOP_SESSION_ID, HOP_ROLE=manager,
// HOP_INCARNATION_ID), recovery by the pane's creation label when the
// response is lost, and the outcome (binding and success, or
// reconciling). The launcher it starts runs the unchanged sanitized
// launch pipeline.
func (c *Controller) openManagerPane(ctx context.Context, handle RunHandle, managerID identity.SessionID, workspaceID, repositoryRoot, hopPath, stateRoot string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per bootstrap.
	opID, err := c.newOperationID()
	if err != nil {
		return err
	}
	incarnationID, err := identity.ParseIncarnationID(c.IDs.NewID())
	if err != nil {
		return fmt.Errorf("app: generate incarnation id: %w", err)
	}
	argv := []string{hopPath, "launch", "--run", handle.runID.String(), "--session", managerID.String()}
	env := map[string]string{
		"HOP_STATE_DIR":      stateRoot,
		"HOP_RUN_ID":         handle.runID.String(),
		"HOP_SESSION_ID":     managerID.String(),
		"HOP_ROLE":           string(run.RoleManager),
		"HOP_INCARNATION_ID": incarnationID.String(),
	}
	serverInstance := c.observeServerInstance(ctx)
	intent := paneOpenIntent{Command: argv, Cwd: repositoryRoot, WorkspaceID: workspaceID, Label: opID.String(), IncarnationID: incarnationID, SessionID: managerID, ServerInstance: serverInstance}
	now := c.Clock.Now()

	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return applyManagerLaunchIntent(ctx, uow, handle, managerID, opID, intent, now)
	}); err != nil {
		return fmt.Errorf("app: record manager pane.open intent: %w", err)
	}

	if err := c.revalidateForDispatch(ctx, handle, false); err != nil {
		return fmt.Errorf("app: revalidate before manager pane.open: %w", err)
	}
	actCtx, release := handle.actContext(ctx)
	paneHandle, actErr := c.Runtime.OpenWorkerPane(actCtx, WorkerPaneRequest{
		WorkspaceID: workspaceID, Cwd: repositoryRoot, Command: argv, Env: env, Label: opID.String(),
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
			op.State = OperationReconciling
			op.Outcome = actErr.Error()
			return uow.Operations().Save(ctx, op)
		}
		binding := run.NewRuntimeBinding(managerID, incarnationID, "", serverInstance, paneHandle.WorkspaceID, paneHandle.TabID, paneHandle.PaneID, opID.String(), run.LaunchInitial, op.UpdatedAt)
		if bindErr := uow.Bindings().Create(ctx, binding); bindErr != nil {
			return bindErr
		}
		op.State = OperationSucceeded
		op.ActEvidence = paneHandle
		return uow.Operations().Save(ctx, op)
	})
	if outcomeErr != nil {
		return fmt.Errorf("app: record manager pane.open outcome: %w", outcomeErr)
	}
	if actErr != nil {
		return fmt.Errorf("%w (operation %s is reconciling; the transport error is recorded there); run hop resume %s", errManagerPaneUnresolved, opID, handle.runID)
	}
	return nil
}

// applyManagerLaunchIntent commits the manager's launch-intent
// transitions with the pane.open operation: the run moves to launching
// (from created on a first start, or from resuming when hop resume
// continues the bootstrap) and the manager session reserved -> launching.
// A held stop request refuses the intent before anything is staged.
func applyManagerLaunchIntent(ctx context.Context, uow UnitOfWork, handle RunHandle, managerID identity.SessionID, opID identity.OperationID, intent paneOpenIntent, now time.Time) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per manager launch.
	r, rRev, err := uow.Runs().Get(ctx, handle.runID)
	if err != nil {
		return err
	}
	if r.StopRequested {
		return fmt.Errorf("%w: run %s", ErrStopRequested, handle.runID)
	}
	generation := gen(handle.lease.Generation)
	if r.State == run.RunCreated || r.State == run.RunResuming {
		runFrom := r.State
		launched, launchErr := r.Launch(now)
		if launchErr != nil {
			return launchErr
		}
		if _, saveErr := uow.Runs().Save(ctx, launched, rRev); saveErr != nil {
			return saveErr
		}
		if transErr := recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(runFrom), string(launched.State), "manager launch intent journaled", generation, now); transErr != nil {
			return transErr
		}
	}

	s, sRev, err := uow.Sessions().Get(ctx, managerID)
	if err != nil {
		return err
	}
	if s.Role != run.RoleManager {
		return fmt.Errorf("app: session %s is not a manager session; the manager pane is never opened for it", managerID)
	}
	sessionFrom := s.State
	launching, err := s.Launch(now)
	if err != nil {
		return err
	}
	if _, saveErr := uow.Sessions().Save(ctx, launching, sRev); saveErr != nil {
		return saveErr
	}
	if err := recordTransition(ctx, uow, EntitySession, managerID.String(), string(sessionFrom), string(launching.State), "manager launch intent", generation, now); err != nil {
		return err
	}
	return uow.Operations().Create(ctx, Operation{
		ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
		Kind: OpPaneOpen, State: OperationPending, Intent: intent,
		CreatedAt: now, UpdatedAt: now,
	})
}

// featureBootstrapProgress is one continueFeatureBootstrap round's result.
type featureBootstrapProgress struct {
	// Blocked names what keeps the bootstrap from completing this round;
	// empty when nothing does.
	Blocked string
	// ManagerLaunching is true while the manager session's first launch is
	// in flight — its pane.open journaled, its launch claim not settled —
	// which the controller loop's launch corroboration settles, including
	// the session-keyed label recovery of a lost pane.open outcome.
	ManagerLaunching bool
	// ManagerSessionID is the run's current manager session, when one is
	// live.
	ManagerSessionID identity.SessionID
}

// bootstrapJournal is the feature bootstrap's durable progress, read in
// one unit of work: the newest integration.init operation, the newest
// workspace.create operation placing the current manager, and that
// manager session.
type bootstrapJournal struct {
	init           *Operation
	workspace      *Operation
	manager        run.Session
	managerPresent bool
}

// continueFeatureBootstrap is hop resume's feature startup continuation
// (the feature counterpart of Phase 2's continueStartup): it finishes whatever of the
// bootstrap a lost controller left undone, idempotently across repeated
// resumes, before any session is reconciled. While the manager is still
// reserved, integration.init is re-driven fresh when no intent exists or
// the newest settled failed, and recovered per its decision row when one
// is unresolved; then the manager is placed (workspace.create recovered
// by label per the design row, never created twice) and its pane opened
// after its frozen artifacts are verified. A launching manager — the
// bootstrap's, or a relaunched successor — with no binding goes through
// the session-keyed label recovery, and with no settled claim returns the
// run to launching for the loop's corroboration. Any other manager state
// means the bootstrap is complete. Recorded outcomes that stop the bootstrap (a collision,
// an unresolved act) are reported as Blocked; only failures to read or
// record anything are errors.
func (c *Controller) continueFeatureBootstrap(ctx context.Context, handle RunHandle, frozen *FrozenRun, req *ResumeFeatureRequest) (featureBootstrapProgress, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per resume round.
	journal, err := c.loadBootstrapJournal(ctx, handle)
	if err != nil {
		return featureBootstrapProgress{}, err
	}
	if !journal.managerPresent {
		return featureBootstrapProgress{}, nil
	}
	progress := featureBootstrapProgress{ManagerSessionID: journal.manager.ID}

	switch journal.manager.State {
	case run.SessionReserved:
		// A manager pane opens only after integration.init succeeded, so the
		// init step is owed exactly while the manager was never launched.
		if journal.init == nil || journal.init.State != OperationSucceeded {
			blocked, initErr := c.continueIntegrationInit(ctx, handle, frozen, journal.init)
			if initErr != nil || blocked != "" {
				progress.Blocked = blocked
				return progress, initErr
			}
		}
		workspaceID, blocked, placeErr := c.continueManagerWorkspace(ctx, handle, frozen, journal.manager.ID, journal.workspace)
		if placeErr != nil || blocked != "" {
			progress.Blocked = blocked
			return progress, placeErr
		}
		if err := c.ensureManagerArtifacts(ctx, handle, frozen, req.HOPPath); err != nil {
			return progress, err
		}
		stateRoot := req.StateRoot
		if stateRoot == "" {
			stateRoot = frozen.Snapshot.StateRoot
		}
		if err := c.openManagerPane(ctx, handle, journal.manager.ID, workspaceID, frozen.RepositoryRoot, req.HOPPath, stateRoot); err != nil && !errors.Is(err, errManagerPaneUnresolved) {
			return progress, err
		}
		progress.ManagerLaunching = true
	case run.SessionLaunching:
		manager := journal.manager
		binding, bindingFound, claim, claimFound, _, evidenceErr := c.sessionCloseEvidence(ctx, handle, &manager)
		if evidenceErr != nil {
			return progress, evidenceErr
		}
		if !bindingFound || binding.PaneID == "" {
			if err := c.recoverSessionBindingByLabel(ctx, handle, &manager, bindingFound, binding); err != nil {
				return progress, err
			}
		}
		if claimFound && claim.State != LaunchClaimExecPending {
			return progress, nil
		}
		progress.ManagerLaunching = true
		if err := c.returnRunToLaunching(ctx, handle); err != nil {
			return progress, err
		}
	}
	return progress, nil
}

// loadBootstrapJournal reads the bootstrap's durable progress.
func (c *Controller) loadBootstrapJournal(ctx context.Context, handle RunHandle) (bootstrapJournal, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	var journal bootstrapJournal
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "feature bootstrap continuation")
		if wfErr != nil {
			return wfErr
		}
		manager, _, mgrErr := wf.ManagerSession(ctx, handle.runID)
		switch {
		case errors.Is(mgrErr, ErrNotFound):
			return nil
		case mgrErr != nil:
			return mgrErr
		}
		journal.manager = manager
		journal.managerPresent = true

		inits, opErr := uow.Operations().ByKind(ctx, handle.runID, OpIntegrationInit)
		if opErr != nil {
			return opErr
		}
		if len(inits) > 0 {
			journal.init = &inits[0]
		}
		workspaces, opErr := uow.Operations().ByKind(ctx, handle.runID, OpWorkspaceCreate)
		if opErr != nil {
			return opErr
		}
		for i := range workspaces {
			intent, ok := decodeOperationPayload[workspaceCreateIntent](workspaces[i].Intent)
			if ok && intent.ManagerSessionID == manager.ID.String() {
				journal.workspace = &workspaces[i]
				break
			}
		}
		return nil
	})
	return journal, err
}

// bootstrapOutcomeBlocked reports a bootstrap act's error as a Blocked
// detail when it names a recorded outcome, or passes it through as a
// failure otherwise.
func bootstrapOutcomeBlocked(err error) (string, error) {
	switch {
	case err == nil:
		return "", nil
	case errors.Is(err, ErrIntegrationBranchExists), errors.Is(err, errIntegrationInitRefused),
		errors.Is(err, errIntegrationInitUnresolved), errors.Is(err, errManagerWorkspaceUnresolved):
		return err.Error(), nil
	default:
		return "", err
	}
}

// continueIntegrationInit drives the integration.init decision row for a
// continued bootstrap: no intent, or a newest intent that settled failed
// (nothing of it was ever created), journals a fresh intent from the
// frozen snapshot — any ref then present is foreign, even at the base
// commit; an unresolved intent is recovered.
func (c *Controller) continueIntegrationInit(ctx context.Context, handle RunHandle, frozen *FrozenRun, op *Operation) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	if op != nil && (op.State == OperationPending || op.State == OperationReconciling) {
		return c.recoverIntegrationInit(ctx, handle, op, false)
	}
	wf := frozen.Snapshot.Workflow
	if wf.IntegrationBranch == "" || !isObjectID(wf.BaseCommitOID) {
		return "the frozen snapshot records no integration branch or base commit; the bootstrap cannot continue", nil
	}
	intent := integrationInitIntent{
		RepositoryRoot: frozen.RepositoryRoot,
		Ref:            integrationRef(wf.IntegrationBranch),
		BaseOID:        wf.BaseCommitOID,
	}
	return bootstrapOutcomeBlocked(c.initIntegrationBranch(ctx, handle, &intent))
}

// recoverIntegrationInit resolves an unresolved integration.init per its
// decision row: a symbolic ref (dangling or not) fails the operation as a
// collision and is never dereferenced; the ref at the intent's base commit
// is adopted; any other present value fails as a collision; a ref not observed
// present re-acts the SAME operation (a dead controller's surviving
// dispatch can only write the identical value, and the create-only CAS
// lets exactly one land); a read that cannot be made leaves the operation
// reconciling, never treated as absence. stopping selects the stop path's
// completing act.
func (c *Controller) recoverIntegrationInit(ctx context.Context, handle RunHandle, op *Operation, stopping bool) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	intent, ok := decodeOperationPayload[integrationInitIntent](op.Intent)
	if !ok || intent.RepositoryRoot == "" || intent.Ref == "" || !isObjectID(intent.BaseOID) {
		detail := fmt.Sprintf("integration.init %s has an undecodable intent; failing closed", op.ID)
		return detail, c.markOperationReconciling(ctx, handle, op.ID, detail)
	}
	seen := c.observeRef(ctx, intent.RepositoryRoot, intent.Ref)
	value := seen.value
	switch {
	case !seen.observed:
		detail := fmt.Sprintf("integration.init %s: the ref could not be observed; it stays reconciling", op.ID)
		return detail, c.markOperationReconciling(ctx, handle, op.ID, "the ref could not be observed; never treated as absence")
	case seen.symbolic:
		collision := fmt.Errorf("%w; then run hop resume %s, or hop stop %s", integrationBranchCollision(intent.Ref), handle.runID, handle.runID)
		return collision.Error(), c.settleOperation(ctx, handle, op.ID, OperationFailed, "integration branch collision: the ref is a symbolic ref, never HOP's")
	case value == intent.BaseOID:
		return "", c.settleOperation(ctx, handle, op.ID, OperationSucceeded, "adopted: the ref is at the frozen base commit")
	case value != "":
		collision := fmt.Errorf("%w; then run hop resume %s, or hop stop %s", integrationBranchCollision(intent.Ref), handle.runID, handle.runID)
		return collision.Error(), c.settleOperation(ctx, handle, op.ID, OperationFailed, "integration branch collision: the ref exists at another commit")
	default:
		return bootstrapOutcomeBlocked(c.actIntegrationInit(ctx, handle, op.ID, &intent, true, stopping))
	}
}

// continueManagerWorkspace resolves the reserved manager's placement: a
// succeeded workspace.create's recorded workspace; an unresolved one
// recovered by label; none (or a settled failure, which created nothing)
// placed fresh.
func (c *Controller) continueManagerWorkspace(ctx context.Context, handle RunHandle, frozen *FrozenRun, managerID identity.SessionID, op *Operation) (workspaceID, blocked string, err error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	switch {
	case op == nil || op.State == OperationFailed:
		workspaceID, err = c.placeManagerWorkspace(ctx, handle, managerID, frozen.RepositoryRoot)
		if errors.Is(err, errManagerWorkspaceUnresolved) {
			return "", err.Error(), nil
		}
		return workspaceID, "", err
	case op.State == OperationSucceeded:
		created, ok := decodeOperationPayload[WorkspaceHandle](op.ActEvidence)
		if !ok || created.WorkspaceID == "" {
			return "", fmt.Sprintf("workspace.create %s succeeded without a recorded workspace; the manager pane cannot be placed safely", op.ID), nil
		}
		return created.WorkspaceID, "", nil
	default:
		return c.recoverManagerWorkspace(ctx, handle, op)
	}
}

// recoverManagerWorkspace resolves an unresolved workspace.create exactly
// per the design's decision row: the workspace found by its creation label
// (descended to its sole tab and sole root pane, S8) is adopted; an absent
// label within the bounded wait is an in-flight request, the wait
// persisted as act evidence; past it, or on a failed or ambiguous lookup,
// the operation is reconciling. It never creates a second workspace.
func (c *Controller) recoverManagerWorkspace(ctx context.Context, handle RunHandle, op *Operation) (workspaceID, blocked string, err error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	intent, ok := decodeOperationPayload[workspaceCreateIntent](op.Intent)
	if !ok || intent.Label == "" {
		detail := fmt.Sprintf("workspace.create %s has an undecodable intent; failing closed", op.ID)
		return "", detail, c.markOperationReconciling(ctx, handle, op.ID, detail)
	}
	ref, found, findErr := c.Workspaces.FindWorkspaceByLabel(ctx, intent.Label)
	switch {
	case findErr != nil:
		detail := fmt.Sprintf("workspace.create %s: the label lookup failed or was ambiguous; it stays reconciling", op.ID)
		return "", detail, c.markOperationReconciling(ctx, handle, op.ID, "the creation-label lookup failed or was ambiguous: "+findErr.Error())
	case found:
		now := c.Clock.Now()
		err = c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
			latest, getErr := uow.Operations().Get(ctx, op.ID)
			if getErr != nil {
				return getErr
			}
			if latest.State != OperationPending && latest.State != OperationReconciling {
				return nil
			}
			latest.State = OperationSucceeded
			latest.ActEvidence = WorkspaceHandle(ref)
			latest.Outcome = "adopted by creation label"
			latest.UpdatedAt = now
			return uow.Operations().Save(ctx, latest)
		})
		if err != nil {
			return "", "", err
		}
		return ref.WorkspaceID, "", nil
	case c.Clock.Now().Sub(op.CreatedAt) <= workspaceRecoveryDeadline:
		detail := fmt.Sprintf("workspace.create %s: no workspace carries the creation label yet; the request may still be in flight", op.ID)
		return "", detail, c.recordBoundedWait(ctx, handle, op, workspaceRecoveryDeadline, "no workspace carries the creation label yet; the create may be in flight")
	default:
		detail := fmt.Sprintf("workspace.create %s: no workspace surfaced for the creation label within the bounded wait; never re-created", op.ID)
		return "", detail, c.markOperationReconciling(ctx, handle, op.ID, "no workspace surfaced for the creation label within the bounded wait; never re-created")
	}
}

// ensureManagerArtifacts verifies the frozen artifacts the manager's
// launch prompt references before its pane opens: the frozen manager role
// copy must match its frozen digest (it cannot be recreated — the
// repository file may have changed since the freeze); the worker-protocol
// crib is rewritten from the grammar constants when missing or altered;
// and the brief assignment must match its frozen digest, recreated
// byte-identically from the frozen inputs when missing (a recreation that
// does not reproduce the digest — a moved hop executable — fails closed).
func (c *Controller) ensureManagerArtifacts(ctx context.Context, handle RunHandle, frozen *FrozenRun, hopPath string) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	wf := frozen.Snapshot.Workflow
	role, err := c.Artifacts.ReadArtifact(ctx, wf.ManagerRolePath)
	if err != nil || sha256Hex(role) != wf.ManagerRoleDigest {
		return errors.New("app: the frozen manager role artifact is missing or does not match its frozen digest; failing closed")
	}
	stateRoot := frozen.Snapshot.StateRoot
	cribPath := workerProtocolCribPath(stateRoot, handle.runID)
	crib := renderWorkerProtocolCrib()
	if existing, readErr := c.Artifacts.ReadArtifact(ctx, cribPath); readErr != nil || sha256Hex(existing) != sha256Hex(crib) {
		if writeErr := c.Artifacts.WriteArtifact(ctx, cribPath, crib); writeErr != nil {
			return errors.New("app: the worker protocol reference could not be rewritten")
		}
	}

	path := frozen.Snapshot.AssignmentPath
	if path == "" {
		return errors.New("app: the frozen snapshot records no assignment path")
	}
	if content, readErr := c.Artifacts.ReadArtifact(ctx, path); readErr == nil {
		if sha256Hex(content) != frozen.Snapshot.AssignmentDigest {
			return errors.New("app: the manager assignment artifact does not match its frozen digest; failing closed")
		}
		return nil
	}
	content := renderManagerAssignment(&managerAssignmentFields{
		RunID:          handle.runID.String(),
		Brief:          frozen.Brief,
		AssignmentPath: path,
		RolePath:       wf.ManagerRolePath,
		CribPath:       cribPath,
		HOPPath:        hopPath,
	})
	if sha256Hex(content) != frozen.Snapshot.AssignmentDigest {
		return errors.New("app: the manager assignment artifact could not be recreated identically from the frozen inputs (digest mismatch; has the hop executable path changed?); failing closed")
	}
	if err := c.Artifacts.WriteArtifact(ctx, path, content); err != nil {
		return errors.New("app: the manager assignment artifact could not be recreated")
	}
	return c.recordAssignmentArtifact(ctx, handle.lease, handle.runID, path, frozen.Snapshot.AssignmentDigest)
}

// returnRunToLaunching moves a resuming run back to launching while its
// manager's first launch is in flight (the section 5 resuming ->
// launching row), so the controller loop's launch corroboration — not a
// resume round — settles it. Idempotent; a run in any other state, or one
// holding a stop request, is left alone.
func (c *Controller) returnRunToLaunching(ctx context.Context, handle RunHandle) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		r, rRev, err := uow.Runs().Get(ctx, handle.runID)
		if err != nil {
			return err
		}
		if r.State != run.RunResuming || r.StopRequested {
			return nil
		}
		next, err := r.Launch(now)
		if err != nil {
			return err
		}
		if _, err := uow.Runs().Save(ctx, next, rRev); err != nil {
			return err
		}
		return recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(r.State), string(next.State), "feature bootstrap continued: the manager's launch is in flight", gen(handle.lease.Generation), now)
	})
}

// settleIntegrationInitForShutdown resolves every unresolved
// integration.init before a stopped or failed report (design section 4: a
// run never reports a terminal state with a ref-move intent unresolved).
// Each follows its recovery row with the stop path's completing act, a
// retirement that revalidates the lease but not the stop flag: the ref at
// the intent's base is adopted; another value fails the operation as a
// collision; a ref not observed present is completed by the same
// create-only CAS — a zombie of the dead controller landing first is
// adopted — so nothing can create the ref after the terminal report; a
// read that cannot be made stays outstanding, never absence.
func (c *Controller) settleIntegrationInitForShutdown(ctx context.Context, handle RunHandle) ([]string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per stop or terminal-failure round.
	var unresolved []identity.OperationID
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		pending, pendErr := uow.Operations().Pending(ctx, handle.runID)
		if pendErr != nil {
			return pendErr
		}
		for i := range pending {
			if pending[i].Kind == OpIntegrationInit {
				unresolved = append(unresolved, pending[i].ID)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	var outstanding []string
	for _, id := range unresolved {
		op, err := c.currentOperation(ctx, handle, id)
		if err != nil {
			return nil, err
		}
		if op.State != OperationPending && op.State != OperationReconciling {
			continue
		}
		detail, err := c.recoverIntegrationInit(ctx, handle, &op, true)
		if err != nil {
			return nil, err
		}
		settled, err := c.currentOperation(ctx, handle, id)
		if err != nil {
			return nil, err
		}
		if settled.State == OperationPending || settled.State == OperationReconciling {
			if detail == "" {
				detail = fmt.Sprintf("integration.init %s is unresolved", id)
			}
			outstanding = append(outstanding, detail)
		}
	}
	return outstanding, nil
}
