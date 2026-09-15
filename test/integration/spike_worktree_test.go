package integration

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestSpikeConcurrentWorktreeCreate is spike item S9: two worktree.create
// calls issued concurrently against one repository, each with its own
// explicit branch and `base` object id, prove per-attempt worktree creation
// under the shapes Phase 3's worker/reviewer launch path depends on.
//
// worktree.create's response is the same {workspace, tab, root_pane} triple
// as workspace.create (S8) and tab.create (S7), plus a fourth `worktree`
// object (repos/herdr/src/api/schema/response.rs ResponseResult::
// WorktreeCreated); the new workspace's own `worktree` field (WorkspaceInfo.
// worktree, repos/herdr/src/app/creation.rs workspace_info) carries the
// repo_key/repo_root identity that lets Herdr and HOP group sibling
// checkouts of one repository by adjacency, independent of workspace
// numbering.
//
// `--base <oid>` is honored (git worktree add -b <branch> <path> <base>,
// repos/herdr/src/worktree.rs build_worktree_add_new_branch_command) only
// when the requested branch does not already exist; the existing-branch case
// is pinned separately by TestSpikeWorktreeCreateExistingBranchIgnoresBase as
// a FINDING, since it silently ignores base.
func TestSpikeConcurrentWorktreeCreate(t *testing.T) {
	fixtures := buildSpikeFixtures(t)
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)

	repo := newFixtureRepo(t, artifacts, "repo")
	repo.writeFile(t, "SECOND_MARKER", "second\n", 0o644)
	secondOID := repo.commit(t, "second commit")
	if secondOID == repo.Base {
		t.Fatal("the second commit did not change HEAD; the two bases below would be indistinguishable")
	}

	// Manager placement (design section 6): a plain workspace.create at the
	// repository root. This is the workspace worktree.create's own
	// parent-grouping mechanism (find_parent_workspace_for_space,
	// repos/herdr/src/app/api/worktrees.rs) later attaches worktree_space
	// membership to, once a worktree.create call resolves it as the source.
	var manager workspaceCreatedResponse
	server.call(t, "workspace.create", map[string]any{"cwd": repo.Root, "focus": true}, &manager)

	branchA, branchB := "hop/r1/t1a1", "hop/r1/t2a1"
	pathA := filepath.Join(artifacts.dir(t, "worktrees"), "attempt-a")
	pathB := filepath.Join(artifacts.dir(t, "worktrees"), "attempt-b")
	markerA, markerB := "hop-spike-wt-a", "hop-spike-wt-b"

	resultA := server.callWorktreeCreateAsync(map[string]any{
		"cwd": repo.Root, "branch": branchA, "base": repo.Base, "path": pathA, "label": markerA, "focus": false,
	})
	resultB := server.callWorktreeCreateAsync(map[string]any{
		"cwd": repo.Root, "branch": branchB, "base": secondOID, "path": pathB, "label": markerB, "focus": false,
	})
	a := <-resultA
	b := <-resultB
	if a.err != nil {
		t.Fatalf("worktree.create (attempt a): %v", a.err)
	}
	if b.err != nil {
		t.Fatalf("worktree.create (attempt b): %v", b.err)
	}

	// Response shape, and identity distinct per call.
	for name, resp := range map[string]worktreeCreatedResponse{"a": a.response, "b": b.response} {
		if resp.Type != "worktree_created" {
			t.Errorf("worktree.create (%s) result type = %q, want worktree_created", name, resp.Type)
		}
		if resp.Worktree.IsLinkedWorktree != true {
			t.Errorf("worktree.create (%s) worktree.is_linked_worktree = false, want true for a created attempt checkout", name)
		}
		if resp.Workspace.Worktree == nil {
			t.Fatalf("worktree.create (%s) workspace carries no worktree membership", name)
		}
	}
	if a.response.Workspace.WorkspaceID == b.response.Workspace.WorkspaceID {
		t.Fatal("two worktree.create calls returned the same workspace id")
	}
	if a.response.Workspace.WorkspaceID == manager.Workspace.WorkspaceID || b.response.Workspace.WorkspaceID == manager.Workspace.WorkspaceID {
		t.Fatal("a worktree.create call reused the manager's own workspace id")
	}
	if a.response.Worktree.Branch == nil || *a.response.Worktree.Branch != branchA {
		t.Errorf("worktree.create (a) worktree.branch = %v, want %q", a.response.Worktree.Branch, branchA)
	}
	if b.response.Worktree.Branch == nil || *b.response.Worktree.Branch != branchB {
		t.Errorf("worktree.create (b) worktree.branch = %v, want %q", b.response.Worktree.Branch, branchB)
	}
	if !samePath(t, a.response.Worktree.Path, pathA) {
		t.Errorf("worktree.create (a) worktree.path = %q, want %q", a.response.Worktree.Path, pathA)
	}
	if !samePath(t, b.response.Worktree.Path, pathB) {
		t.Errorf("worktree.create (b) worktree.path = %q, want %q", b.response.Worktree.Path, pathB)
	}
	if a.response.RootPane.WorkspaceID != a.response.Workspace.WorkspaceID || a.response.RootPane.TabID != a.response.Tab.TabID {
		t.Errorf("worktree.create (a) root pane %+v does not cross-reference its own workspace/tab", a.response.RootPane)
	}
	if b.response.RootPane.WorkspaceID != b.response.Workspace.WorkspaceID || b.response.RootPane.TabID != b.response.Tab.TabID {
		t.Errorf("worktree.create (b) root pane %+v does not cross-reference its own workspace/tab", b.response.RootPane)
	}

	// Base-commit provenance: each new branch's HEAD equals the base it was
	// given, proving --base <oid> is honored for a genuinely new branch.
	if got := worktreeHead(t, repo, pathA); got != repo.Base {
		t.Errorf("attempt a worktree HEAD = %s, want its requested base %s", got, repo.Base)
	}
	if got := worktreeHead(t, repo, pathB); got != secondOID {
		t.Errorf("attempt b worktree HEAD = %s, want its requested base %s", got, secondOID)
	}

	// Grouping with the parent workspace: both new checkouts, AND the
	// manager's own workspace (now the resolved source), carry the SAME
	// repo_key/repo_root -- the mechanism Herdr and HOP use to associate
	// sibling checkouts of one repository, independent of workspace order.
	if a.response.Workspace.Worktree.RepoKey != b.response.Workspace.Worktree.RepoKey {
		t.Errorf("attempt a/b repo_key differ (%q vs %q); both are worktrees of the same repository",
			a.response.Workspace.Worktree.RepoKey, b.response.Workspace.Worktree.RepoKey)
	}
	managerInfo := server.workspaceGet(t, manager.Workspace.WorkspaceID)
	if managerInfo.Worktree == nil {
		t.Fatal("the manager's own workspace carries no worktree membership after worktree.create resolved it as the parent")
	}
	if managerInfo.Worktree.RepoKey != a.response.Workspace.Worktree.RepoKey {
		t.Errorf("manager workspace repo_key %q does not match the created worktrees' repo_key %q",
			managerInfo.Worktree.RepoKey, a.response.Workspace.Worktree.RepoKey)
	}
	if managerInfo.Worktree.IsLinkedWorktree {
		t.Errorf("the manager's own (parent, non-worktree) workspace reports is_linked_worktree=true")
	}

	// Creation-label round trip through session.snapshot, per call (S8's
	// recovery pattern): worktree.create's label names the new WORKSPACE, not
	// a pane, exactly like workspace.create.
	if got := server.snapshotWorkspaceByLabel(t, markerA); got != a.response.Workspace.WorkspaceID {
		t.Errorf("session.snapshot recovers workspace %q by label %q, want %q", got, markerA, a.response.Workspace.WorkspaceID)
	}
	if got := server.snapshotWorkspaceByLabel(t, markerB); got != b.response.Workspace.WorkspaceID {
		t.Errorf("session.snapshot recovers workspace %q by label %q, want %q", got, markerB, b.response.Workspace.WorkspaceID)
	}

	// Coexistence with the S6 command-pane launch mechanism: each freshly
	// created worktree workspace accepts a layout.apply command pane exactly
	// like any other workspace, running at the worktree's own checkout path.
	for _, attempt := range []struct {
		name        string
		workspaceID string
		path        string
	}{
		{"a", a.response.Workspace.WorkspaceID, pathA},
		{"b", b.response.Workspace.WorkspaceID, pathB},
	} {
		runID := newSpikeUUID(t)
		var applied workerPaneAppliedResponse
		server.call(t, "layout.apply", map[string]any{
			"workspace_id": attempt.workspaceID,
			"focus":        false,
			"root": map[string]any{
				"type":    "pane",
				"label":   "hop-spike-wt-cmd-" + attempt.name,
				"cwd":     attempt.path,
				"command": []string{fixtures.claude, "--run", runID},
				"env":     map[string]string{"HOP_RUN_ID": runID},
			},
		}, &applied)
		pane := applied.Layout.Root.PaneID
		if pane == "" {
			t.Fatalf("layout.apply into worktree workspace %s (%s) returned no pane id", attempt.workspaceID, attempt.name)
		}
		server.waitForPaneText(t, pane, "SPIKE-HARNESS-STARTED name=[claude]")
		info := server.waitForForegroundProcess(t, pane)
		process := foregroundClaudeProcess(info)
		if !samePath(t, process.Cwd, attempt.path) {
			t.Errorf("command pane in worktree %s cwd = %q, want the worktree checkout %q", attempt.name, process.Cwd, attempt.path)
		}
		server.waitForAgent(t, pane, "claude")
	}
}

// TestSpikeWorktreeCreateExistingBranchIgnoresBase is the S9 FINDING: when
// the requested branch already exists (and is not checked out anywhere else,
// so git does not additionally refuse it), Herdr checks it out at its OWN
// current tip (`git worktree add <path> <branch>`,
// repos/herdr/src/worktree.rs build_worktree_add_existing_branch_command) and
// silently ignores the request's `base` entirely -- base is only threaded
// into the `-b <branch> <base>` form used for a genuinely new branch
// (build_worktree_add_new_branch_command). Phase 3's branch names always
// carry a unique attempt number (design section 6, `hop/r<seq>/t<tseq>a<n>`),
// so production code should never hit this path, but a base-commit
// provenance rule that assumes base is unconditionally honored is wrong for
// any accidental branch-name reuse.
func TestSpikeWorktreeCreateExistingBranchIgnoresBase(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)

	repo := newFixtureRepo(t, artifacts, "repo")
	const branch = "existing-branch"
	// Created but never checked out anywhere, so `git worktree add` does not
	// separately refuse it as already in use.
	repo.git(t, "branch", branch, repo.Base)

	repo.writeFile(t, "SECOND_MARKER", "second\n", 0o644)
	secondOID := repo.commit(t, "second commit")
	if secondOID == repo.Base {
		t.Fatal("the second commit did not change HEAD")
	}

	path := filepath.Join(artifacts.dir(t, "worktrees"), "existing-branch")
	var created worktreeCreatedResponse
	server.call(t, "worktree.create", map[string]any{
		"cwd": repo.Root, "branch": branch, "base": secondOID, "path": path,
	}, &created)
	if created.Worktree.Branch == nil || *created.Worktree.Branch != branch {
		t.Errorf("worktree.create worktree.branch = %v, want %q", created.Worktree.Branch, branch)
	}

	got := worktreeHead(t, repo, path)
	if got == secondOID {
		t.Fatalf("worktree.create honored base %s for an already-existing branch %s; expected it to be ignored (FINDING)", secondOID, branch)
	}
	if got != repo.Base {
		t.Errorf("worktree.create checked out %s at %s, want its own existing tip %s (base %s must be ignored)", branch, got, repo.Base, secondOID)
	}
}

// worktreeCreatedResponse is the worktree.create result reduced to the
// fields this spike asserts on: the same {workspace, tab, root_pane} triple
// as workspace.create, plus the created worktree's own path/branch/kind.
type worktreeCreatedResponse struct {
	Type      string `json:"type"`
	Workspace struct {
		WorkspaceID string                   `json:"workspace_id"`
		Label       string                   `json:"label"`
		Worktree    *workspaceWorktreeFields `json:"worktree"`
	} `json:"workspace"`
	Tab struct {
		TabID       string `json:"tab_id"`
		WorkspaceID string `json:"workspace_id"`
	} `json:"tab"`
	RootPane struct {
		PaneID      string `json:"pane_id"`
		WorkspaceID string `json:"workspace_id"`
		TabID       string `json:"tab_id"`
	} `json:"root_pane"`
	Worktree struct {
		Path             string  `json:"path"`
		Branch           *string `json:"branch"`
		IsDetached       bool    `json:"is_detached"`
		IsLinkedWorktree bool    `json:"is_linked_worktree"`
	} `json:"worktree"`
}

// workerPaneAppliedResponse is layout.apply's result reduced to the root
// pane id it created (S6's response shape, repeated here for the
// coexistence check).
type workerPaneAppliedResponse struct {
	Layout struct {
		Root struct {
			PaneID string `json:"pane_id"`
		} `json:"root"`
	} `json:"layout"`
}

// workspaceWorktreeFields is WorkspaceInfo.worktree
// (repos/herdr/src/api/schema/workspaces.rs WorkspaceWorktreeInfo): the
// repository-grouping identity a workspace carries once worktree.create (or
// worktree.open) establishes membership on it.
type workspaceWorktreeFields struct {
	RepoKey          string `json:"repo_key"`
	RepoName         string `json:"repo_name"`
	RepoRoot         string `json:"repo_root"`
	CheckoutPath     string `json:"checkout_path"`
	IsLinkedWorktree bool   `json:"is_linked_worktree"`
}

// workspaceGetResponse is workspace.get's result reduced to the fields this
// spike reads back after worktree.create has run.
type workspaceGetResponse struct {
	WorkspaceID string                   `json:"workspace_id"`
	Worktree    *workspaceWorktreeFields `json:"worktree"`
}

// workspaceGet reads one workspace's current state directly, used to observe
// membership worktree.create attached to a workspace OTHER than the one
// named in its own response (the resolved parent).
func (s *testServer) workspaceGet(t *testing.T, workspaceID string) workspaceGetResponse {
	t.Helper()
	var result struct {
		Workspace workspaceGetResponse `json:"workspace"`
	}
	s.call(t, "workspace.get", map[string]any{"workspace_id": workspaceID}, &result)
	return result.Workspace
}

// worktreeCreateAsyncResult is one worktree.create call's outcome, delivered
// over a channel so concurrent calls can be issued from a test's main
// goroutine without calling t.Fatal from another goroutine.
type worktreeCreateAsyncResult struct {
	response worktreeCreatedResponse
	err      error
}

// callWorktreeCreateAsync issues one worktree.create call in its own
// goroutine and returns a channel carrying its outcome, so a test can start
// two or more calls without either blocking on the other -- the concurrency
// S9 is required to exercise.
func (s *testServer) callWorktreeCreateAsync(params map[string]any) <-chan worktreeCreateAsyncResult {
	out := make(chan worktreeCreateAsyncResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		var response worktreeCreatedResponse
		err := s.client.Call(ctx, "worktree.create", params, &response)
		out <- worktreeCreateAsyncResult{response: response, err: err}
	}()
	return out
}

// worktreeHead runs `git rev-parse HEAD` against a worktree checkout path on
// disk, in the same hermetic git environment fixtureRepo itself uses (no
// developer git configuration), and returns the commit object id.
func worktreeHead(t *testing.T, repo *fixtureRepo, path string) string {
	t.Helper()
	return strings.TrimSpace(mustRunGit(t, path, repo.gitHome, "rev-parse", "HEAD^{commit}"))
}
