package app_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// The feature bootstrap's fixed inputs. The fake git's update-ref
// reproduces the create-only compare-and-swap G1/G3 pinned under real git
// (test/integration TestSpikeUpdateRefCreateOnlySemantics: `update-ref
// <ref> <new> ""` succeeds only while the ref is absent and leaves an
// existing ref unchanged), and the fake runtime's workspaces carry the
// S8-pinned shape (TestSpikeWorkspaceCreateResponseShapeAndEnv: the
// workspace, its sole tab and that tab's sole root pane, the label naming
// the workspace).
const (
	featureRepo     = "/repo"
	featureHOP      = "/usr/local/bin/hop"
	featureState    = "/state"
	featureGit      = "/usr/bin/git"
	featureRef      = "refs/heads/hop/r1/integration"
	managerRoleSrc  = "/repo/.herdr-orchestrator/roles/manager.md"
	implRoleSrc     = "/repo/.herdr-orchestrator/roles/implementer.md"
	reviewerRoleSrc = "/repo/.herdr-orchestrator/roles/reviewer.md"
)

// featurePolicy is a SOLO-mode repository policy carrying the role keys:
// StartFeatureRun applies the --workflow feature override's shared
// defaults and requireds to it.
func featurePolicy() app.RunPolicy {
	return app.RunPolicy{
		CheckArgv: []string{"sh", "check.sh"}, Harness: "claude",
		ManagerRole: managerRoleSrc, ImplementerRole: implRoleSrc, ReviewerRole: reviewerRoleSrc,
	}
}

// featureStart drives StartFeatureRun (and ResumeFeature's continuation)
// against the fakes, logging every bootstrap step in order: the store's
// InitializeRun, each pre-dispatch revalidation heartbeat and each act.
type featureStart struct {
	t     *testing.T
	tc    *testController
	git   *fakeGitRepo
	base  string
	store *initHookStore

	mu     sync.Mutex
	events []string

	// Per-act hooks, run inside the act after it is logged; a hook
	// returning handled=true replaces the default act.
	onUpdateRef       func(cmd app.Command) (app.CommandResult, bool, error)
	onCreateWorkspace func(req app.WorkspaceRequest) (app.WorkspaceHandle, bool, error)
	onOpenPane        func(req app.WorkerPaneRequest) (app.PaneHandle, bool, error)
	onHeartbeat       func(n int)
	// onGit, when set, sees every git invocation first; handled=true
	// replaces the fake repository's answer.
	onGit func(cmd app.Command) (app.CommandResult, bool, error)

	heartbeats     int
	workspaceCalls []app.WorkspaceRequest
	paneCalls      []app.WorkerPaneRequest
	createdPanes   map[string]app.PaneRef
}

// initHookStore records InitializeRun calls and lets a test run code
// before each one reaches the fake.
type initHookStore struct {
	*fakeStore
	before func(call int, spec *app.NewRunSpec)
	after  func(err error)
	calls  int
}

func (s *initHookStore) InitializeRun(ctx context.Context, spec app.NewRunSpec) (identity.RunID, app.Lease, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	s.calls++
	if s.before != nil {
		s.before(s.calls, &spec)
	}
	runID, lease, err := s.fakeStore.InitializeRun(ctx, spec)
	if s.after != nil {
		s.after(err)
	}
	return runID, lease, err
}

func newFeatureStart(t *testing.T) *featureStart {
	t.Helper()
	tc := newTestController(featurePolicy())
	git := newFakeGitRepo(featureGit, featureHOP)
	base := git.newCommit("tree-base")
	if err := git.registerWorktree(featureRepo, base); err != nil {
		t.Fatalf("register repository HEAD: %v", err)
	}
	f := &featureStart{t: t, tc: tc, git: git, base: base, createdPanes: map[string]app.PaneRef{}}
	t.Cleanup(func() { git.requireNoDerefUpdates(t) })
	f.store = &initHookStore{fakeStore: tc.Store}
	tc.Controller.Store = f.store
	f.store.before = func(int, *app.NewRunSpec) { f.event("initialize") }

	for role, path := range map[string]string{"manager": managerRoleSrc, "implementer": implRoleSrc, "reviewer": reviewerRoleSrc} {
		tc.Artifacts.files[path] = []byte("# " + role + " role\n")
	}
	tc.Store.HeartbeatHook = func() {
		f.mu.Lock()
		f.heartbeats++
		n := f.heartbeats
		f.mu.Unlock()
		f.event("heartbeat")
		if f.onHeartbeat != nil {
			f.onHeartbeat(n)
		}
	}
	tc.Commands.RunHook = func(ctx context.Context, cmd app.Command) (app.CommandResult, bool, error) {
		if f.onGit != nil {
			if result, handled, err := f.onGit(cmd); handled {
				return result, true, err
			}
		}
		if len(cmd.Argv) >= 4 && cmd.Argv[3] == "update-ref" {
			f.event("update-ref")
			if f.onUpdateRef != nil {
				if result, handled, err := f.onUpdateRef(cmd); handled {
					return result, true, err
				}
			}
		}
		return git.Hook(ctx, cmd)
	}
	tc.Runtime.CreateWorkspaceFn = func(req app.WorkspaceRequest) (app.WorkspaceHandle, error) {
		f.event("workspace.create")
		f.workspaceCalls = append(f.workspaceCalls, req)
		if f.onCreateWorkspace != nil {
			if handle, handled, err := f.onCreateWorkspace(req); handled {
				return handle, err
			}
		}
		return f.createWorkspaceLocked(req), nil
	}
	tc.Runtime.OpenWorkerPaneFn = func(req app.WorkerPaneRequest) (app.PaneHandle, error) {
		f.event("pane.open")
		f.paneCalls = append(f.paneCalls, req)
		if f.onOpenPane != nil {
			if handle, handled, err := f.onOpenPane(req); handled {
				return handle, err
			}
		}
		return f.openPaneLocked(&req), nil
	}
	tc.Runtime.FindPaneByLabelFn = func(label string) (app.PaneRef, bool, error) {
		ref, ok := f.createdPanes[label]
		return ref, ok, nil
	}
	return f
}

// createWorkspaceLocked is the default workspace.create act (called with
// the fake runtime's lock held): an S8-shaped workspace remembered by
// label.
func (f *featureStart) createWorkspaceLocked(req app.WorkspaceRequest) app.WorkspaceHandle {
	r := f.tc.Runtime
	r.nextPaneN++
	handle := app.WorkspaceHandle{
		WorkspaceID: fmt.Sprintf("workspace-m%d", r.nextPaneN),
		TabID:       fmt.Sprintf("tab-m%d", r.nextPaneN),
		PaneID:      fmt.Sprintf("root-m%d", r.nextPaneN),
	}
	if r.Workspaces == nil {
		r.Workspaces = map[string]app.WorkspaceRef{}
	}
	r.Workspaces[req.Label] = app.WorkspaceRef(handle)
	return handle
}

// openPaneLocked is the default pane.open act (called with the fake
// runtime's lock held): a command pane remembered by creation label.
func (f *featureStart) openPaneLocked(req *app.WorkerPaneRequest) app.PaneHandle {
	r := f.tc.Runtime
	r.nextPaneN++
	handle := app.PaneHandle{WorkspaceID: req.WorkspaceID, TabID: fmt.Sprintf("tab-p%d", r.nextPaneN), PaneID: fmt.Sprintf("pane-p%d", r.nextPaneN)}
	f.createdPanes[req.Label] = app.PaneRef(handle)
	return handle
}

func (f *featureStart) event(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, name)
}

func (f *featureStart) eventLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func featureRequest() app.StartRunRequest {
	return app.StartRunRequest{
		RepositoryRoot: featureRepo, Brief: "build the feature", ControllerID: "controller-1",
		StateRoot: featureState, HOPPath: featureHOP,
	}
}

func (f *featureStart) start() (app.StartRunResult, app.RunHandle, error) {
	return f.tc.Controller.StartFeatureRun(context.Background(), featureRequest())
}

// mustStart runs StartFeatureRun and requires success.
func (f *featureStart) mustStart() (app.StartRunResult, app.RunHandle) {
	f.t.Helper()
	result, handle, err := f.start()
	if err != nil {
		f.t.Fatalf("StartFeatureRun() error = %v", err)
	}
	return result, handle
}

// resume runs ResumeFeature as a fresh controller once the dead
// controller's lease has expired.
func (f *featureStart) resume(controller string) (app.ResumeFeatureResult, app.RunHandle, error) {
	f.t.Helper()
	f.tc.Clock.Advance(leaseTTL + time.Second)
	return f.tc.Controller.ResumeFeature(context.Background(), app.ResumeFeatureRequest{
		RunID: f.runID().String(), ControllerID: controller, HOPPath: featureHOP, StateRoot: featureState,
	})
}

// mustResume requires ResumeFeature to succeed and releases the new lease
// afterward, as hop resume does on any path that leaves the loop.
func (f *featureStart) mustResume(controller string) app.ResumeFeatureResult {
	f.t.Helper()
	result, handle, err := f.resume(controller)
	if err != nil {
		f.t.Fatalf("ResumeFeature(%s) error = %v", controller, err)
	}
	_ = f.tc.Controller.Detach(context.Background(), handle) //nolint:errcheck // releasing the round's lease is best-effort here; later rounds acquire after expiry anyway.
	return result
}

// killController simulates the controller process dying at this instant:
// its lease is released under it, so every later commit or heartbeat it
// attempts is fenced.
func (f *featureStart) killController() {
	f.tc.Store.mu.Lock()
	defer f.tc.Store.mu.Unlock()
	for _, row := range f.tc.Store.Leases {
		row.held = false
	}
}

func (f *featureStart) runID() identity.RunID {
	f.t.Helper()
	f.tc.Store.mu.Lock()
	defer f.tc.Store.mu.Unlock()
	if len(f.tc.Store.Runs) != 1 {
		f.t.Fatalf("expected exactly one run, found %d", len(f.tc.Store.Runs))
	}
	for id := range f.tc.Store.Runs {
		return id
	}
	return ""
}

func (f *featureStart) runValue() run.Run {
	f.t.Helper()
	id := f.runID()
	f.tc.Store.mu.Lock()
	defer f.tc.Store.mu.Unlock()
	return f.tc.Store.Runs[id].value
}

func (f *featureStart) manager() run.Session {
	f.t.Helper()
	f.tc.Store.mu.Lock()
	defer f.tc.Store.mu.Unlock()
	var found []run.Session
	for _, row := range f.tc.Store.Sessions {
		if row.value.Role == run.RoleManager {
			found = append(found, row.value)
		}
	}
	if len(found) != 1 {
		f.t.Fatalf("expected exactly one manager session, found %d", len(found))
	}
	return found[0]
}

func (f *featureStart) ops(kind app.OperationKind) []app.Operation {
	f.tc.Store.mu.Lock()
	defer f.tc.Store.mu.Unlock()
	var out []app.Operation
	for id := range f.tc.Store.Operations {
		if f.tc.Store.Operations[id].Kind == kind {
			out = append(out, f.tc.Store.Operations[id])
		}
	}
	return out
}

func (f *featureStart) onlyOp(kind app.OperationKind) app.Operation {
	f.t.Helper()
	ops := f.ops(kind)
	if len(ops) != 1 {
		f.t.Fatalf("expected exactly one %s operation, found %d", kind, len(ops))
	}
	return ops[0]
}

func (f *featureStart) opState(kind app.OperationKind) app.OperationState {
	f.t.Helper()
	return f.onlyOp(kind).State
}

func (f *featureStart) managerBinding() (run.RuntimeBinding, bool) {
	manager := f.manager()
	f.tc.Store.mu.Lock()
	defer f.tc.Store.mu.Unlock()
	history := f.tc.Store.Bindings[manager.ID]
	if len(history) == 0 {
		return run.RuntimeBinding{}, false
	}
	return history[len(history)-1], true
}

// decodeInto round-trips a persisted payload into a test-local shape.
func decodeInto(t *testing.T, payload, target any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode payload %s: %v", raw, err)
	}
}

func digestOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// TestStartFeatureRun covers the feature bootstrap's live path
// (docs/plan/phase-3-design.md sections 1, 6, 9 and 10).
func TestStartFeatureRun(t *testing.T) {
	t.Run("journal order: freeze, initialize, branch CAS, workspace.create, pane.open — each act revalidated after its intent", func(t *testing.T) {
		f := newFeatureStart(t)
		var runID identity.RunID
		f.store.before = func(_ int, spec *app.NewRunSpec) {
			f.event("initialize")
			// Freeze precedes initialize: the role copies and the crib the
			// snapshot names already exist, with the frozen digests.
			wf := spec.Snapshot.Workflow
			for _, copyRef := range []struct{ path, digest string }{
				{wf.ManagerRolePath, wf.ManagerRoleDigest},
				{wf.ImplementerRolePath, wf.ImplementerRoleDigest},
				{wf.ReviewerRolePath, wf.ReviewerRoleDigest},
			} {
				content, ok := f.tc.Artifacts.files[copyRef.path]
				if !ok || digestOf(content) != copyRef.digest {
					t.Errorf("role copy %s absent or not at its frozen digest when InitializeRun ran", copyRef.path)
				}
			}
			if _, ok := f.tc.Artifacts.files[featureState+"/runs/"+spec.RunID.String()+"/artifacts/worker-protocol.md"]; !ok {
				t.Errorf("worker-protocol crib absent when InitializeRun ran")
			}
			runID = spec.RunID
		}
		f.onUpdateRef = func(app.Command) (app.CommandResult, bool, error) {
			if f.opState(app.OpIntegrationInit) != app.OperationPending {
				t.Errorf("integration.init act dispatched without a pending intent")
			}
			return app.CommandResult{}, false, nil
		}
		f.onCreateWorkspace = func(app.WorkspaceRequest) (app.WorkspaceHandle, bool, error) {
			if f.opState(app.OpIntegrationInit) != app.OperationSucceeded {
				t.Errorf("workspace.create dispatched before integration.init's outcome was recorded")
			}
			if f.opState(app.OpWorkspaceCreate) != app.OperationPending {
				t.Errorf("workspace.create act dispatched without a pending intent")
			}
			return app.WorkspaceHandle{}, false, nil
		}
		f.onOpenPane = func(app.WorkerPaneRequest) (app.PaneHandle, bool, error) {
			if f.opState(app.OpWorkspaceCreate) != app.OperationSucceeded {
				t.Errorf("pane.open dispatched before workspace.create's outcome was recorded")
			}
			if f.opState(app.OpPaneOpen) != app.OperationPending {
				t.Errorf("pane.open act dispatched without a pending intent")
			}
			if f.manager().State != run.SessionLaunching {
				t.Errorf("pane.open dispatched before the manager launch intent committed")
			}
			return app.PaneHandle{}, false, nil
		}

		result, handle := f.mustStart()
		want := []string{"initialize", "heartbeat", "update-ref", "heartbeat", "workspace.create", "heartbeat", "pane.open"}
		if got := f.eventLog(); !reflect.DeepEqual(got, want) {
			t.Fatalf("bootstrap journal order = %v, want %v", got, want)
		}
		if result.RunID != runID.String() || result.Sequence != 1 || handle.RunID() != result.RunID {
			t.Fatalf("StartFeatureRun() = %+v (handle %s), want run %s at sequence 1", result, handle.RunID(), runID)
		}
		for _, kind := range []app.OperationKind{app.OpIntegrationInit, app.OpWorkspaceCreate, app.OpPaneOpen} {
			if state := f.opState(kind); state != app.OperationSucceeded {
				t.Fatalf("%s state = %s, want succeeded", kind, state)
			}
		}
	})

	t.Run("happy path: the frozen feature snapshot, the branch at the base, the manager placed and launching", func(t *testing.T) {
		f := newFeatureStart(t)
		result, _ := f.mustStart()
		runID := f.runID()

		r := f.runValue()
		if r.State != run.RunLaunching {
			t.Fatalf("run state = %s, want launching (the manager's settled claim moves it to running)", r.State)
		}
		snapshot := f.tc.Store.Snapshots[runID]
		wf := snapshot.Workflow
		if !wf.Feature() || wf.MaxWorkers != app.DefaultMaxWorkers || wf.RetryLimit != app.DefaultRetryLimit ||
			wf.MessageAttention != app.DefaultMessageAttention || wf.MessageWait != app.DefaultMessageWait ||
			wf.ReviewerHarness != "claude" || wf.IntegrationBranch != "hop/r1/integration" || wf.BaseCommitOID != f.base {
			t.Fatalf("frozen workflow = %+v, want the feature defaults, hop/r1/integration and base %s", wf, f.base)
		}
		if got := f.git.ref(featureRef); got != f.base {
			t.Fatalf("integration ref = %q, want the frozen base %s", got, f.base)
		}
		if calls := f.git.UpdateRefCalls; len(calls) != 1 || calls[0] != featureRef+" "+f.base {
			t.Fatalf("update-ref calls = %q, want exactly one create-only CAS at the base", calls)
		}
		var initIntent struct {
			RepositoryRoot string `json:"repository_root"`
			Ref            string `json:"ref"`
			BaseOID        string `json:"base_oid"`
		}
		decodeInto(t, f.onlyOp(app.OpIntegrationInit).Intent, &initIntent)
		if initIntent.RepositoryRoot != featureRepo || initIntent.Ref != featureRef || initIntent.BaseOID != f.base {
			t.Fatalf("integration.init intent = %+v", initIntent)
		}

		assignment, ok := f.tc.Artifacts.files[snapshot.AssignmentPath]
		if !ok || digestOf(assignment) != snapshot.AssignmentDigest {
			t.Fatalf("manager assignment absent or not at its frozen digest")
		}
		for _, needle := range []string{"build the feature", wf.ManagerRolePath, featureHOP + " plan close"} {
			if !strings.Contains(string(assignment), needle) {
				t.Fatalf("manager assignment does not reference %q:\n%s", needle, assignment)
			}
		}
		recorded := false
		for _, a := range f.tc.Store.Artifacts {
			if a.RunID == runID && a.Kind == run.ArtifactAssignment && a.Path == snapshot.AssignmentPath && a.Digest == snapshot.AssignmentDigest {
				recorded = true
			}
		}
		if !recorded {
			t.Fatalf("the manager assignment artifact row was not recorded")
		}

		if len(f.workspaceCalls) != 1 {
			t.Fatalf("workspace.create calls = %d, want 1", len(f.workspaceCalls))
		}
		wsOp := f.onlyOp(app.OpWorkspaceCreate)
		wsReq := f.workspaceCalls[0]
		if wsReq.Cwd != featureRepo || wsReq.Label != wsOp.ID.String() || wsReq.Env != nil {
			t.Fatalf("workspace request = %+v, want cwd %s, label = operation %s, no env (the root pane is not a HOP pane)", wsReq, featureRepo, wsOp.ID)
		}
		var placed app.WorkspaceHandle
		decodeInto(t, wsOp.ActEvidence, &placed)

		manager := f.manager()
		if manager.State != run.SessionLaunching || manager.AttemptID != "" || manager.ParentSessionID != nil || manager.NativeSessionRef == "" {
			t.Fatalf("manager = %+v, want a launching attempt-less parentless session with a native reference", manager)
		}
		if len(f.paneCalls) != 1 {
			t.Fatalf("pane.open calls = %d, want 1", len(f.paneCalls))
		}
		paneOp := f.onlyOp(app.OpPaneOpen)
		paneReq := f.paneCalls[0]
		binding, bound := f.managerBinding()
		if !bound {
			t.Fatalf("no manager binding recorded")
		}
		wantArgv := []string{featureHOP, "launch", "--run", runID.String(), "--session", manager.ID.String()}
		wantEnv := map[string]string{
			"HOP_STATE_DIR": featureState, "HOP_RUN_ID": runID.String(), "HOP_SESSION_ID": manager.ID.String(),
			"HOP_ROLE": "manager", "HOP_INCARNATION_ID": binding.IncarnationID.String(),
		}
		if !reflect.DeepEqual(paneReq.Command, wantArgv) || paneReq.Cwd != featureRepo || paneReq.WorkspaceID != placed.WorkspaceID ||
			paneReq.Label != paneOp.ID.String() || !reflect.DeepEqual(paneReq.Env, wantEnv) {
			t.Fatalf("pane request = %+v, want argv %v, cwd %s, workspace %s, label %s, env %v", paneReq, wantArgv, featureRepo, placed.WorkspaceID, paneOp.ID, wantEnv)
		}
		var paneIntent struct {
			SessionID     string `json:"session_id"`
			IncarnationID string `json:"incarnation_id"`
			Cwd           string `json:"cwd"`
		}
		decodeInto(t, paneOp.Intent, &paneIntent)
		if paneIntent.SessionID != manager.ID.String() || paneIntent.IncarnationID != binding.IncarnationID.String() || paneIntent.Cwd != featureRepo {
			t.Fatalf("pane.open intent = %+v, want the manager session/incarnation the claim fallback matches", paneIntent)
		}
		if binding.PaneID == "" || binding.WorkspaceID != placed.WorkspaceID || binding.CreationLabel != paneOp.ID.String() || binding.ServerInstance != "peer-pid:1" {
			t.Fatalf("manager binding = %+v", binding)
		}
		if len(f.tc.Store.Tasks) != 0 || len(f.tc.Store.Attempts) != 0 || len(f.tc.Store.Worktrees) != 0 {
			t.Fatalf("the bootstrap created tasks, attempts or worktrees")
		}
		if result.Sequence != 1 {
			t.Fatalf("sequence = %d, want 1", result.Sequence)
		}
	})

	t.Run("an existing integration ref is refused before any side effect", func(t *testing.T) {
		for _, value := range []string{"base", "other"} {
			t.Run(value, func(t *testing.T) {
				f := newFeatureStart(t)
				existing := f.base
				if value == "other" {
					existing = f.git.newCommit("tree-foreign", f.base)
				}
				f.git.setRef(featureRef, existing)
				_, _, err := f.start()
				if !errors.Is(err, app.ErrStartRefused) || !errors.Is(err, app.ErrIntegrationBranchExists) {
					t.Fatalf("StartFeatureRun() error = %v, want ErrStartRefused wrapping ErrIntegrationBranchExists", err)
				}
				if !strings.Contains(err.Error(), featureRef) || !strings.Contains(err.Error(), "inspect it before removing or renaming it") {
					t.Fatalf("refusal %q does not name the ref and the inspect-first instruction", err)
				}
				assertNothingWritten(t, f)
				if got := f.git.ref(featureRef); got != existing {
					t.Fatalf("the refusal moved the existing ref to %s", got)
				}
			})
		}
	})

	t.Run("a ref created after the pre-check makes the CAS refuse: failed collision, even at the base", func(t *testing.T) {
		for _, value := range []string{"base", "other"} {
			t.Run(value, func(t *testing.T) {
				f := newFeatureStart(t)
				existing := f.base
				if value == "other" {
					existing = f.git.newCommit("tree-foreign", f.base)
				}
				f.onHeartbeat = func(n int) {
					if n == 1 {
						f.git.setRef(featureRef, existing)
					}
				}
				_, _, err := f.start()
				if !errors.Is(err, app.ErrIntegrationBranchExists) || errors.Is(err, app.ErrStartRefused) {
					t.Fatalf("StartFeatureRun() error = %v, want ErrIntegrationBranchExists after initialize (exit 1, not a usage refusal)", err)
				}
				if msg := err.Error(); !strings.Contains(msg, featureRef) || !strings.Contains(msg, "hop resume") || !strings.Contains(msg, "hop stop") || strings.Contains(msg, featureRepo) {
					t.Fatalf("collision %q must name the ref and both remedies, never a path", msg)
				}
				if state := f.opState(app.OpIntegrationInit); state != app.OperationFailed {
					t.Fatalf("integration.init state = %s, want failed", state)
				}
				if got := f.git.ref(featureRef); got != existing {
					t.Fatalf("the refused CAS moved the ref to %s", got)
				}
				assertBootstrapStoppedAfterInit(t, f)
			})
		}
	})

	t.Run("a refused CAS whose ref is not observed fails with git's refusal recorded", func(t *testing.T) {
		f := newFeatureStart(t)
		f.onUpdateRef = func(app.Command) (app.CommandResult, bool, error) {
			return app.CommandResult{ExitCode: 128, Stderr: []byte("fatal: cannot lock ref: unable to create lock file")}, true, nil
		}
		_, _, err := f.start()
		if err == nil || errors.Is(err, app.ErrIntegrationBranchExists) || !strings.Contains(err.Error(), "git refused to create the integration branch") {
			t.Fatalf("StartFeatureRun() error = %v, want git's refusal", err)
		}
		op := f.onlyOp(app.OpIntegrationInit)
		var evidence struct {
			ExitCode int    `json:"exit_code"`
			Stderr   string `json:"stderr"`
		}
		decodeInto(t, op.ActEvidence, &evidence)
		if op.State != app.OperationFailed || evidence.ExitCode != 128 || !strings.Contains(evidence.Stderr, "unable to create lock file") {
			t.Fatalf("integration.init = %s with evidence %+v, want failed with the stderr recorded", op.State, evidence)
		}
		assertBootstrapStoppedAfterInit(t, f)
	})

	t.Run("a runner failure leaves integration.init reconciling", func(t *testing.T) {
		f := newFeatureStart(t)
		f.onUpdateRef = func(app.Command) (app.CommandResult, bool, error) {
			return app.CommandResult{}, true, errors.New("spawn failed")
		}
		_, _, err := f.start()
		if err == nil || !strings.Contains(err.Error(), "unresolved") {
			t.Fatalf("StartFeatureRun() error = %v, want an unresolved integration.init", err)
		}
		if state := f.opState(app.OpIntegrationInit); state != app.OperationReconciling {
			t.Fatalf("integration.init state = %s, want reconciling", state)
		}
		assertBootstrapStoppedAfterInit(t, f)
	})

	t.Run("workspace.create failure with label recovery", func(t *testing.T) {
		t.Run("found: adopted, and the manager pane opens in the recovered workspace", func(t *testing.T) {
			f := newFeatureStart(t)
			f.onCreateWorkspace = func(req app.WorkspaceRequest) (app.WorkspaceHandle, bool, error) {
				f.createWorkspaceLocked(req)
				return app.WorkspaceHandle{}, true, errors.New("response lost")
			}
			f.mustStart()
			op := f.onlyOp(app.OpWorkspaceCreate)
			var placed app.WorkspaceHandle
			decodeInto(t, op.ActEvidence, &placed)
			if op.State != app.OperationSucceeded || placed.WorkspaceID == "" || placed.TabID == "" || placed.PaneID == "" {
				t.Fatalf("workspace.create = %s with evidence %+v, want succeeded with the S8 descent", op.State, placed)
			}
			if len(f.workspaceCalls) != 1 || len(f.paneCalls) != 1 || f.paneCalls[0].WorkspaceID != placed.WorkspaceID {
				t.Fatalf("workspace calls %d, pane calls %v; want one create and the pane in %s", len(f.workspaceCalls), f.paneCalls, placed.WorkspaceID)
			}
		})
		for _, tt := range []struct {
			name string
			find func(string) (app.WorkspaceRef, bool, error)
		}{
			{"absent: reconciling, no pane", func(string) (app.WorkspaceRef, bool, error) { return app.WorkspaceRef{}, false, nil }},
			{"ambiguous: reconciling, no pane", func(string) (app.WorkspaceRef, bool, error) {
				return app.WorkspaceRef{}, false, errors.New("2 workspaces carry this label, want at most one")
			}},
		} {
			t.Run(tt.name, func(t *testing.T) {
				f := newFeatureStart(t)
				f.onCreateWorkspace = func(app.WorkspaceRequest) (app.WorkspaceHandle, bool, error) {
					return app.WorkspaceHandle{}, true, errors.New("herdr socket /tmp/secret-socket: read workspace.create response: EOF")
				}
				f.tc.Runtime.FindWorkspaceByLabelFn = tt.find
				_, _, err := f.start()
				if err == nil || !strings.Contains(err.Error(), "workspace.create is unresolved") || strings.Contains(err.Error(), "secret-socket") {
					t.Fatalf("StartFeatureRun() error = %v, want an unresolved workspace.create that never echoes the transport error", err)
				}
				if state := f.opState(app.OpWorkspaceCreate); state != app.OperationReconciling {
					t.Fatalf("workspace.create state = %s, want reconciling", state)
				}
				if len(f.workspaceCalls) != 1 || len(f.paneCalls) != 0 || len(f.ops(app.OpPaneOpen)) != 0 {
					t.Fatalf("workspace calls %d, pane calls %d: want one create and no pane", len(f.workspaceCalls), len(f.paneCalls))
				}
				if f.manager().State != run.SessionReserved || f.runValue().State != run.RunCreated {
					t.Fatalf("manager %s, run %s; want reserved and created", f.manager().State, f.runValue().State)
				}
				assertLeaseReleased(t, f)
			})
		}
	})

	t.Run("pane.open failure with label recovery", func(t *testing.T) {
		t.Run("found: the recovered pane is bound", func(t *testing.T) {
			f := newFeatureStart(t)
			f.onOpenPane = func(req app.WorkerPaneRequest) (app.PaneHandle, bool, error) {
				f.openPaneLocked(&req)
				return app.PaneHandle{}, true, errors.New("response lost")
			}
			f.mustStart()
			binding, bound := f.managerBinding()
			paneOp := f.onlyOp(app.OpPaneOpen)
			if !bound || paneOp.State != app.OperationSucceeded || binding.CreationLabel != paneOp.ID.String() || binding.PaneID == "" {
				t.Fatalf("pane.open %s, binding %+v; want succeeded with the recovered pane bound", paneOp.State, binding)
			}
		})
		t.Run("absent: reconciling, the manager launching with no binding", func(t *testing.T) {
			f := newFeatureStart(t)
			f.onOpenPane = func(app.WorkerPaneRequest) (app.PaneHandle, bool, error) {
				return app.PaneHandle{}, true, errors.New("response lost")
			}
			_, _, err := f.start()
			if err == nil || !strings.Contains(err.Error(), "pane.open is unresolved") {
				t.Fatalf("StartFeatureRun() error = %v, want an unresolved pane.open", err)
			}
			if state := f.opState(app.OpPaneOpen); state != app.OperationReconciling {
				t.Fatalf("pane.open state = %s, want reconciling", state)
			}
			if _, bound := f.managerBinding(); bound {
				t.Fatalf("a binding was recorded for an unrecovered pane")
			}
			if f.manager().State != run.SessionLaunching || f.runValue().State != run.RunLaunching {
				t.Fatalf("manager %s, run %s; want launching and launching", f.manager().State, f.runValue().State)
			}
			assertLeaseReleased(t, f)
		})
	})

	t.Run("a stop request or a lost lease before each act dispatches nothing", func(t *testing.T) {
		acts := []struct {
			name      string
			heartbeat int
			kind      app.OperationKind
			dispatch  func(f *featureStart) int
		}{
			{"integration.init", 1, app.OpIntegrationInit, func(f *featureStart) int { return len(f.git.UpdateRefCalls) }},
			{"workspace.create", 2, app.OpWorkspaceCreate, func(f *featureStart) int { return len(f.workspaceCalls) }},
			{"pane.open", 3, app.OpPaneOpen, func(f *featureStart) int { return len(f.paneCalls) }},
		}
		for _, act := range acts {
			for _, cause := range []string{"stop request", "lost lease"} {
				t.Run(act.name+" / "+cause, func(t *testing.T) {
					f := newFeatureStart(t)
					f.onHeartbeat = func(n int) {
						if n != act.heartbeat {
							return
						}
						if cause == "stop request" {
							if err := f.tc.Store.RequestStop(context.Background(), f.runID()); err != nil {
								t.Errorf("RequestStop() error = %v", err)
							}
							return
						}
						f.killController()
					}
					_, _, err := f.start()
					switch cause {
					case "stop request":
						if !errors.Is(err, app.ErrStopRequested) {
							t.Fatalf("StartFeatureRun() error = %v, want ErrStopRequested", err)
						}
					default:
						if !errors.Is(err, app.ErrFenced) {
							t.Fatalf("StartFeatureRun() error = %v, want ErrFenced", err)
						}
					}
					if n := act.dispatch(f); n != 0 {
						t.Fatalf("%s was dispatched %d times after the refused revalidation", act.name, n)
					}
					if state := f.opState(act.kind); state != app.OperationPending {
						t.Fatalf("%s intent state = %s, want pending for recovery", act.name, state)
					}
				})
			}
		}
	})

	t.Run("policy and repository refusals write nothing", func(t *testing.T) {
		for _, tt := range []struct {
			name  string
			setup func(f *featureStart)
			want  string
		}{
			{"config cannot be loaded", func(f *featureStart) { f.tc.Config.Err = errors.New("config.toml: unknown key") }, "load repository policy"},
			{"no check command", func(f *featureStart) { f.tc.Config.Policy.CheckArgv = nil }, "no [check] command"},
			{"unsupported harness", func(f *featureStart) { f.tc.Config.Policy.Harness = "vim" }, "not one of claude, codex, opencode"},
			{"missing role key", func(f *featureStart) { f.tc.Config.Policy.ReviewerRole = "" }, "roles.reviewer.instructions is required"},
			{"invalid worker bound", func(f *featureStart) { f.tc.Config.Policy.MaxWorkers = -1 }, "workers.max"},
			{"unreadable role file", func(f *featureStart) { delete(f.tc.Artifacts.files, implRoleSrc) }, "roles.implementer.instructions file could not be read"},
			{"empty role file", func(f *featureStart) { f.tc.Artifacts.files[managerRoleSrc] = nil }, "roles.manager.instructions file is empty"},
			{"unsupported object format", func(f *featureStart) {
				f.tc.Commands.RunHook = func(ctx context.Context, cmd app.Command) (app.CommandResult, bool, error) {
					if strings.Contains(strings.Join(cmd.Argv, " "), "--show-object-format") {
						return app.CommandResult{Stdout: []byte("sha256\n")}, true, nil
					}
					return f.git.Hook(ctx, cmd)
				}
			}, "sha1 only"},
			{"HEAD is not a commit", func(f *featureStart) {
				f.tc.Commands.RunHook = func(ctx context.Context, cmd app.Command) (app.CommandResult, bool, error) {
					if strings.Contains(strings.Join(cmd.Argv, " "), "HEAD^{commit}") {
						return app.CommandResult{ExitCode: 128}, true, nil
					}
					return f.git.Hook(ctx, cmd)
				}
			}, "HEAD does not resolve to a commit"},
			{"git executable not absolute", func(f *featureStart) { f.tc.Controller.GitExecutable = "git" }, "git executable is not configured as an absolute path"},
			{"relative state root", func(*featureStart) {}, "is not absolute"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				f := newFeatureStart(t)
				tt.setup(f)
				req := featureRequest()
				if tt.name == "relative state root" {
					req.StateRoot = "state"
				}
				_, _, err := f.tc.Controller.StartFeatureRun(context.Background(), req)
				if !errors.Is(err, app.ErrStartRefused) || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("StartFeatureRun() error = %v, want ErrStartRefused mentioning %q", err, tt.want)
				}
				assertNothingWritten(t, f)
			})
		}
	})

	t.Run("an unloadable policy refuses with StartRun's exact text", func(t *testing.T) {
		loadErr := errors.New("config.toml: unknown key workers.maximum")
		f := newFeatureStart(t)
		f.tc.Config.Err = loadErr
		_, _, featureErr := f.start()
		solo := newTestController(defaultPolicy())
		solo.Config.Err = loadErr
		_, _, soloErr := solo.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if featureErr == nil || soloErr == nil || featureErr.Error() != soloErr.Error() {
			t.Fatalf("feature refusal %q, solo refusal %q; want identical text", featureErr, soloErr)
		}
	})

	t.Run("a sequence taken by another run re-freezes and initializes at the next sequence", func(t *testing.T) {
		f := newFeatureStart(t)
		competitor := newSoloSpecAt(t, f, featureRepo)
		f.store.before = func(call int, spec *app.NewRunSpec) {
			f.event("initialize")
			if call == 1 {
				if spec.Snapshot.Workflow.IntegrationBranch != "hop/r1/integration" {
					t.Errorf("first freeze predicted %s, want hop/r1/integration", spec.Snapshot.Workflow.IntegrationBranch)
				}
				// Another run of the same repository commits first.
				if _, _, err := f.tc.Store.InitializeRun(context.Background(), competitor); err != nil {
					t.Errorf("competing InitializeRun() error = %v", err)
				}
				// A role file changes before the re-freeze: the committed
				// digests must be the final freeze's.
				f.tc.Artifacts.files[reviewerRoleSrc] = []byte("# reviewer role, revised\n")
			}
		}
		result, _, err := f.start()
		if err != nil {
			t.Fatalf("StartFeatureRun() error = %v", err)
		}
		if f.store.calls != 2 || result.Sequence != 2 {
			t.Fatalf("InitializeRun calls = %d, sequence = %d; want 2 and 2", f.store.calls, result.Sequence)
		}
		runID := identity.RunID(result.RunID)
		wf := f.tc.Store.Snapshots[runID].Workflow
		if wf.IntegrationBranch != "hop/r2/integration" {
			t.Fatalf("frozen branch = %s, want hop/r2/integration", wf.IntegrationBranch)
		}
		for _, copyRef := range []struct{ path, digest string }{
			{wf.ManagerRolePath, wf.ManagerRoleDigest},
			{wf.ImplementerRolePath, wf.ImplementerRoleDigest},
			{wf.ReviewerRolePath, wf.ReviewerRoleDigest},
		} {
			if digestOf(f.tc.Artifacts.files[copyRef.path]) != copyRef.digest {
				t.Fatalf("role copy %s does not match its committed digest after the re-freeze", copyRef.path)
			}
		}
		if string(f.tc.Artifacts.files[wf.ReviewerRolePath]) != "# reviewer role, revised\n" {
			t.Fatalf("the reviewer copy is not the final freeze's content")
		}
		if got := f.git.ref("refs/heads/hop/r2/integration"); got != f.base {
			t.Fatalf("hop/r2/integration = %q, want the base", got)
		}
	})

	t.Run("exhausting the sequence retries fails with nothing committed for the run", func(t *testing.T) {
		f := newFeatureStart(t)
		var ours identity.RunID
		f.store.before = func(_ int, spec *app.NewRunSpec) {
			ours = spec.RunID
			competitor := newSoloSpecAt(t, f, featureRepo)
			if _, _, err := f.tc.Store.InitializeRun(context.Background(), competitor); err != nil {
				t.Errorf("competing InitializeRun() error = %v", err)
			}
		}
		_, _, err := f.start()
		if !errors.Is(err, app.ErrRunSequenceMismatch) || errors.Is(err, app.ErrStartRefused) {
			t.Fatalf("StartFeatureRun() error = %v, want ErrRunSequenceMismatch (exit 1)", err)
		}
		if f.store.calls != 3 {
			t.Fatalf("InitializeRun calls = %d, want 3", f.store.calls)
		}
		if _, ok := f.tc.Store.Runs[ours]; ok {
			t.Fatalf("the exhausted feature run was committed")
		}
		if len(f.tc.Store.Operations) != 0 || len(f.git.UpdateRefCalls) != 0 {
			t.Fatalf("an exhausted start journaled or acted")
		}
	})

	t.Run("a controller without a workspace runtime fails closed before any side effect", func(t *testing.T) {
		f := newFeatureStart(t)
		f.tc.Controller.Workspaces = nil
		_, _, err := f.start()
		if !errors.Is(err, app.ErrFeatureModeUnsupported) {
			t.Fatalf("StartFeatureRun() error = %v, want ErrFeatureModeUnsupported", err)
		}
		assertNothingWritten(t, f)
	})
}

// newSoloSpecAt builds a solo NewRunSpec for root with fresh identities,
// standing in for a competing hop run of the same repository.
func newSoloSpecAt(t *testing.T, f *featureStart, root string) app.NewRunSpec {
	t.Helper()
	id := func() string { return f.tc.IDs.NewID() }
	return app.NewRunSpec{
		RepositoryRoot: root,
		RunID:          identity.RunID(id()), TaskID: identity.TaskID(id()), AttemptID: identity.AttemptID(id()),
		SessionID: identity.SessionID(id()), WorktreeID: identity.WorktreeID(id()), IncarnationID: identity.IncarnationID(id()),
		Brief: "competing brief", BriefDigest: "d", InstructionsDigest: "i",
		Snapshot: app.RunSnapshot{StateRoot: featureState, Harness: "claude"},
		Harness:  run.HarnessClaude, ControllerID: "controller-other", Now: f.tc.Clock.Now(),
	}
}

// assertNothingWritten proves a refusal happened before any side effect:
// no run, lease or journal row, no artifact beyond the seeded role
// sources, and no git ref move.
func assertNothingWritten(t *testing.T, f *featureStart) {
	t.Helper()
	if len(f.tc.Store.Runs) != 0 || len(f.tc.Store.Leases) != 0 || len(f.tc.Store.Operations) != 0 || len(f.tc.Store.Sessions) != 0 {
		t.Fatalf("a refusal wrote store state: runs=%d leases=%d operations=%d sessions=%d",
			len(f.tc.Store.Runs), len(f.tc.Store.Leases), len(f.tc.Store.Operations), len(f.tc.Store.Sessions))
	}
	for path := range f.tc.Artifacts.files {
		if path != managerRoleSrc && path != implRoleSrc && path != reviewerRoleSrc {
			t.Fatalf("a refusal wrote artifact %s", path)
		}
	}
	if len(f.git.UpdateRefCalls) != 0 || len(f.workspaceCalls) != 0 || len(f.paneCalls) != 0 {
		t.Fatalf("a refusal acted: update-ref %v, workspace %d, pane %d", f.git.UpdateRefCalls, len(f.workspaceCalls), len(f.paneCalls))
	}
}

// assertBootstrapStoppedAfterInit proves a failed or unresolved
// integration.init ends the start there: the run stays created with its
// manager reserved, nothing is placed or launched, and the lease is
// released for hop resume or hop stop.
func assertBootstrapStoppedAfterInit(t *testing.T, f *featureStart) {
	t.Helper()
	if len(f.workspaceCalls) != 0 || len(f.paneCalls) != 0 || len(f.ops(app.OpWorkspaceCreate)) != 0 || len(f.ops(app.OpPaneOpen)) != 0 {
		t.Fatalf("the bootstrap continued past a failed integration.init")
	}
	if f.runValue().State != run.RunCreated || f.manager().State != run.SessionReserved {
		t.Fatalf("run %s, manager %s; want created and reserved", f.runValue().State, f.manager().State)
	}
	assertLeaseReleased(t, f)
}

func assertLeaseReleased(t *testing.T, f *featureStart) {
	t.Helper()
	f.tc.Store.mu.Lock()
	defer f.tc.Store.mu.Unlock()
	for id, row := range f.tc.Store.Leases {
		if row.held {
			t.Fatalf("run %s lease is still held after a failed start", id)
		}
	}
}

// TestResolveRunWorkflow pins hop run's dispatch rule: the --workflow
// override wins, the policy's mode decides without one (solo by
// default), an unloadable policy resolves to the solo path (which
// reports it), and an unknown override is refused.
func TestResolveRunWorkflow(t *testing.T) {
	for _, tt := range []struct {
		name     string
		mode     string
		loadErr  error
		override string
		want     string
		wantErr  bool
	}{
		{"no override, solo policy", "", nil, "", app.WorkflowModeSolo, false},
		{"no override, explicit solo", app.WorkflowModeSolo, nil, "", app.WorkflowModeSolo, false},
		{"no override, feature policy", app.WorkflowModeFeature, nil, "", app.WorkflowModeFeature, false},
		{"no override, unloadable policy", "", errors.New("bad"), "", app.WorkflowModeSolo, false},
		{"feature override on a solo policy", "", nil, app.WorkflowModeFeature, app.WorkflowModeFeature, false},
		{"solo override on a feature policy", app.WorkflowModeFeature, nil, app.WorkflowModeSolo, app.WorkflowModeSolo, false},
		{"feature override on an unloadable policy", "", errors.New("bad"), app.WorkflowModeFeature, app.WorkflowModeFeature, false},
		{"unknown override", "", nil, "parallel", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			policy := featurePolicy()
			policy.WorkflowMode = tt.mode
			tc := newTestController(policy)
			tc.Config.Err = tt.loadErr
			got, err := tc.Controller.ResolveRunWorkflow(context.Background(), featureRepo, tt.override)
			if tt.wantErr {
				if !errors.Is(err, app.ErrStartRefused) {
					t.Fatalf("ResolveRunWorkflow() error = %v, want ErrStartRefused", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ResolveRunWorkflow() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}
