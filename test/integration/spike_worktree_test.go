package integration

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// worktreeAttempt is one worktree.create call's request shape plus its
// eventual response, for TestSpikeConcurrentWorktreeCreate's slice-driven
// full-family scenario.
type worktreeAttempt struct {
	name       string // "t1a1", "t2a1", "t3a1" -- the attempt's own family suffix
	branch     string
	base       string
	path       string
	label      string
	wantIgnore bool // true for the pre-existing branch: base must be IGNORED
	response   worktreeCreatedResponse
}

// TestSpikeConcurrentWorktreeCreate is spike item S9: three worktree.create
// calls -- implement task 1, implement task 2 and the REVIEW task, all
// sharing the ordinary `hop/r1/t<t>a1` naming with no separate review-branch
// scheme -- issued CONCURRENTLY against one repository, prove per-attempt
// worktree creation under the shapes Phase 3's worker/reviewer launch path
// depends on, and cover BOTH `base` cases side by side: t1a1/t2a1 are
// genuinely new branches (base honored), t3a1 reuses a branch created ahead
// of time (base silently ignored -- see also
// TestSpikeWorktreeCreateExistingBranchIgnoresBase, which isolates that
// case on its own).
//
// worktree.create's response is the same {workspace, tab, root_pane} triple
// as workspace.create (S8) and tab.create (S7), plus a fourth `worktree`
// object (repos/herdr/src/api/schema/response.rs ResponseResult::
// WorktreeCreated); the new workspace's own `worktree` field (WorkspaceInfo.
// worktree, repos/herdr/src/app/creation.rs workspace_info) carries the
// repo_key/repo_root identity that lets Herdr and HOP group sibling
// checkouts of one repository by adjacency, independent of workspace
// numbering.
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

	// The review task's branch is created AHEAD of time, at repo.Base, so its
	// worktree.create call below reuses an existing branch -- the "base
	// ignored" case -- rather than creating a new one.
	const reviewBranch = "hop/r1/t3a1"
	repo.git(t, "branch", reviewBranch, repo.Base)

	// Manager placement (design section 6): a plain workspace.create at the
	// repository root. This is the workspace worktree.create's own
	// parent-grouping mechanism (find_parent_workspace_for_space,
	// repos/herdr/src/app/api/worktrees.rs) later attaches worktree_space
	// membership to, once a worktree.create call resolves it as the source.
	var manager workspaceCreatedResponse
	server.call(t, "workspace.create", map[string]any{"cwd": repo.Root, "focus": true}, &manager)

	attempts := []*worktreeAttempt{
		{
			name: "t1a1", branch: "hop/r1/t1a1", base: repo.Base,
			path: filepath.Join(artifacts.dir(t, "worktrees"), "attempt-t1a1"), label: "hop-spike-wt-t1a1",
		},
		{
			name: "t2a1", branch: "hop/r1/t2a1", base: secondOID,
			path: filepath.Join(artifacts.dir(t, "worktrees"), "attempt-t2a1"), label: "hop-spike-wt-t2a1",
		},
		{
			// The review task: an EXISTING branch, given a base that differs
			// from its own current tip so an honored base would be
			// distinguishable from an ignored one.
			name: "t3a1", branch: reviewBranch, base: secondOID,
			path: filepath.Join(artifacts.dir(t, "worktrees"), "attempt-t3a1"), label: "hop-spike-wt-t3a1",
			wantIgnore: true,
		},
	}

	results := make([]<-chan worktreeCreateAsyncResult, len(attempts))
	for i, attempt := range attempts {
		results[i] = server.callWorktreeCreateAsync(map[string]any{
			"cwd": repo.Root, "branch": attempt.branch, "base": attempt.base, "path": attempt.path,
			"label": attempt.label, "focus": false,
		})
	}
	for i, ch := range results {
		result := <-ch
		if result.err != nil {
			t.Fatalf("worktree.create (%s): %v", attempts[i].name, result.err)
		}
		attempts[i].response = result.response
	}

	// Response shape, and identity distinct per call and from the manager.
	workspaceIDs := map[string]string{"manager": manager.Workspace.WorkspaceID}
	for _, attempt := range attempts {
		resp := attempt.response
		if resp.Type != "worktree_created" {
			t.Errorf("worktree.create (%s) result type = %q, want worktree_created", attempt.name, resp.Type)
		}
		if !resp.Worktree.IsLinkedWorktree {
			t.Errorf("worktree.create (%s) worktree.is_linked_worktree = false, want true for a created attempt checkout", attempt.name)
		}
		if resp.Workspace.Worktree == nil {
			t.Fatalf("worktree.create (%s) workspace carries no worktree membership", attempt.name)
		}
		if resp.Worktree.Branch == nil || *resp.Worktree.Branch != attempt.branch {
			t.Errorf("worktree.create (%s) worktree.branch = %v, want %q", attempt.name, resp.Worktree.Branch, attempt.branch)
		}
		if !samePath(t, resp.Worktree.Path, attempt.path) {
			t.Errorf("worktree.create (%s) worktree.path = %q, want %q", attempt.name, resp.Worktree.Path, attempt.path)
		}
		if resp.RootPane.WorkspaceID != resp.Workspace.WorkspaceID || resp.RootPane.TabID != resp.Tab.TabID {
			t.Errorf("worktree.create (%s) root pane %+v does not cross-reference its own workspace/tab", attempt.name, resp.RootPane)
		}
		if existing, ok := workspaceIDs[resp.Workspace.WorkspaceID]; ok {
			t.Fatalf("worktree.create (%s) returned workspace id %s, already used by %s", attempt.name, resp.Workspace.WorkspaceID, existing)
		}
		workspaceIDs[resp.Workspace.WorkspaceID] = attempt.name
	}

	// Base-commit provenance, BOTH cases side by side: t1a1/t2a1's new
	// branches honor the requested base; t3a1's pre-existing branch ignores
	// it, checked out at its own tip (repo.Base) instead of the requested
	// secondOID.
	for _, attempt := range attempts {
		got := worktreeHead(t, repo, attempt.path)
		want := attempt.base
		if attempt.wantIgnore {
			want = repo.Base
		}
		if got != want {
			verb := "honors"
			if attempt.wantIgnore {
				verb = "ignores (reused an existing branch)"
			}
			t.Errorf("attempt %s worktree HEAD = %s, want %s (base %s %s)", attempt.name, got, want, attempt.base, verb)
		}
	}

	// Grouping with the parent workspace: every checkout, AND the manager's
	// own workspace (now the resolved source), carries the SAME
	// repo_key/repo_root -- the mechanism Herdr and HOP use to associate
	// sibling checkouts of one repository, independent of workspace order.
	repoKey := attempts[0].response.Workspace.Worktree.RepoKey
	for _, attempt := range attempts[1:] {
		if attempt.response.Workspace.Worktree.RepoKey != repoKey {
			t.Errorf("attempt %s repo_key = %q, want %q (every attempt is a worktree of the same repository)",
				attempt.name, attempt.response.Workspace.Worktree.RepoKey, repoKey)
		}
	}
	managerInfo := server.workspaceGet(t, manager.Workspace.WorkspaceID)
	if managerInfo.Worktree == nil {
		t.Fatal("the manager's own workspace carries no worktree membership after worktree.create resolved it as the parent")
	}
	if managerInfo.Worktree.RepoKey != repoKey {
		t.Errorf("manager workspace repo_key %q does not match the created worktrees' repo_key %q", managerInfo.Worktree.RepoKey, repoKey)
	}
	if managerInfo.Worktree.IsLinkedWorktree {
		t.Errorf("the manager's own (parent, non-worktree) workspace reports is_linked_worktree=true")
	}

	// Creation-label round trip through session.snapshot, per call (S8's
	// recovery pattern): worktree.create's label names the new WORKSPACE, not
	// a pane, exactly like workspace.create.
	for _, attempt := range attempts {
		if got := server.snapshotWorkspaceByLabel(t, attempt.label); got != attempt.response.Workspace.WorkspaceID {
			t.Errorf("session.snapshot recovers workspace %q by label %q, want %q", got, attempt.label, attempt.response.Workspace.WorkspaceID)
		}
	}

	// Coexistence with the S6 command-pane launch mechanism: each freshly
	// created worktree workspace accepts a layout.apply command pane exactly
	// like any other workspace, running at the worktree's own checkout path.
	for _, attempt := range attempts {
		runID := newSpikeUUID(t)
		var applied workerPaneAppliedResponse
		server.call(t, "layout.apply", map[string]any{
			"workspace_id": attempt.response.Workspace.WorkspaceID,
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
			t.Fatalf("layout.apply into worktree workspace %s (%s) returned no pane id", attempt.response.Workspace.WorkspaceID, attempt.name)
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

// TestSpikeWorktreeCreateExistingBranchIgnoresBase is S9's existing-branch
// case, isolated: when the requested branch already exists (and is not
// checked out anywhere else,
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
		t.Fatalf("worktree.create honored base %s for an already-existing branch %s; expected it to be ignored", secondOID, branch)
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
