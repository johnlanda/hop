package integration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/herdr"
)

// Worktree-retirement transport probe (provisional numbering R1; docs/plan/
// phase-3-worktree-retirement.md): the observations that decide how HOP
// removes a merged run's attempt worktrees. Herdr 0.9.0 offers
// `worktree.remove`, addressed by WORKSPACE id; the alternative is a plain
// `git worktree remove` of the recorded path. These tests pin, against a
// disposable test-owned server:
//
//   - worktree.remove's response shape, its dirty-checkout refusal without
//     force, and its effect on the grouped workspace and the workspace's
//     shells (TestSpikeWorktreeRemoveHerdrShapes);
//   - that it needs the workspace still open in Herdr
//     (TestSpikeWorktreeRemoveNeedsOpenWorkspace);
//   - that a workspace id is not a durable address across a server restart
//     (TestSpikeWorkspaceIDReissuedAfterRestart);
//   - what a git-side removal leaves behind in Herdr
//     (TestSpikeGitWorktreeRemoveLeavesHerdrWorkspace).
//
// Every checkout lives under the test's own artifact directory; nothing
// outside the test's temporary roots is created, moved or removed.

// worktreeRemovedResult is the exact worktree.remove success result.
type worktreeRemovedResult struct {
	Type        string `json:"type"`
	WorkspaceID string `json:"workspace_id"`
	Path        string `json:"path"`
	Forced      bool   `json:"forced"`
}

// worktreeListResult is worktree.list's result reduced to the entries.
type worktreeListResult struct {
	Type      string `json:"type"`
	Worktrees []struct {
		Path             string  `json:"path"`
		Branch           *string `json:"branch"`
		IsPrunable       bool    `json:"is_prunable"`
		IsLinkedWorktree bool    `json:"is_linked_worktree"`
		OpenWorkspaceID  *string `json:"open_workspace_id"`
	} `json:"worktrees"`
}

// callRaw issues one request and returns the raw result object and the
// error, without failing the test, so a refusal can be the assertion.
func (s *testServer) callRaw(t *testing.T, method string, params any) (json.RawMessage, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	defer cancel()
	var raw json.RawMessage
	err := s.client.Call(ctx, method, params, &raw)
	return raw, err
}

// requireAPIError asserts err is a Herdr API error with the given code and
// returns its message.
func requireAPIError(t *testing.T, err error, code string) string {
	t.Helper()
	var apiErr *herdr.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want a Herdr API error with code %q", err, code)
	}
	if apiErr.Code != code {
		t.Fatalf("API error code = %q (message %q), want %q", apiErr.Code, apiErr.Message, code)
	}
	return apiErr.Message
}

// createAttemptWorktree creates one attempt-shaped checkout through
// worktree.create beside a manager-shaped parent workspace at the
// repository root, the production placement.
func createAttemptWorktree(t *testing.T, server *testServer, repo *fixtureRepo, artifacts *artifactDir, name string) worktreeCreatedResponse {
	t.Helper()
	var created worktreeCreatedResponse
	server.call(t, "worktree.create", map[string]any{
		"cwd": repo.Root, "branch": "hop/r1/" + name, "base": repo.Base,
		"path":  filepath.Join(artifacts.dir(t, "worktrees"), "attempt-"+name),
		"label": "hop-spike-remove-" + name, "focus": false,
	}, &created)
	if created.Type != "worktree_created" || created.Workspace.WorkspaceID == "" || created.RootPane.PaneID == "" {
		t.Fatalf("worktree.create returned %+v", created)
	}
	return created
}

func (s *testServer) worktreeList(t *testing.T, repoRoot string) worktreeListResult {
	t.Helper()
	var result worktreeListResult
	s.call(t, "worktree.list", map[string]any{"cwd": repoRoot}, &result)
	if result.Type != "worktree_list" {
		t.Fatalf("worktree.list type = %q", result.Type)
	}
	return result
}

// listedByHerdr reports whether worktree.list names the checkout (by
// canonical path) and the entry.
func (s *testServer) listedByHerdr(t *testing.T, repoRoot, path string) (bool, *string, bool) {
	t.Helper()
	for _, entry := range s.worktreeList(t, repoRoot).Worktrees {
		if sameMaybeMissingPath(t, entry.Path, path) {
			return true, entry.OpenWorkspaceID, entry.IsPrunable
		}
	}
	return false, nil, false
}

// listedByGit reports whether the repository's own worktree list names the
// checkout.
func listedByGit(t *testing.T, repo *fixtureRepo, path string) bool {
	t.Helper()
	out := mustRunGit(t, repo.Root, repo.gitHome, "worktree", "list", "--porcelain")
	for line := range strings.SplitSeq(out, "\n") {
		if listed, ok := strings.CutPrefix(line, "worktree "); ok && sameMaybeMissingPath(t, listed, path) {
			return true
		}
	}
	return false
}

// canonicalMaybeMissing resolves symbolic links in path; a path that no
// longer exists is resolved through its parent directory instead.
func canonicalMaybeMissing(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved
	}
	if !os.IsNotExist(err) {
		t.Fatalf("resolve %s: %v", path, err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		t.Fatalf("resolve the parent of %s: %v", path, err)
	}
	return filepath.Join(parent, filepath.Base(path))
}

// sameMaybeMissingPath reports whether two paths name the same location,
// either of which may no longer exist.
func sameMaybeMissingPath(t *testing.T, a, b string) bool {
	t.Helper()
	return canonicalMaybeMissing(t, a) == canonicalMaybeMissing(t, b)
}

// pidAlive reports whether a process with pid exists (signal 0).
func pidAlive(pid uint32) bool {
	err := syscall.Kill(int(pid), 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func pathExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("lstat: %v", err)
	}
	return err == nil
}

func (s *testServer) workspaceExists(t *testing.T, workspaceID string) bool {
	t.Helper()
	_, err := s.callRaw(t, "workspace.get", map[string]any{"workspace_id": workspaceID})
	if err == nil {
		return true
	}
	requireAPIError(t, err, "workspace_not_found")
	return false
}

// TestSpikeWorktreeRemoveHerdrShapes pins Herdr's own removal: without
// force a checkout holding an untracked file is refused with
// dirty_worktree_requires_force and git's own message, leaving checkout,
// workspace and shell untouched; a clean checkout is removed with the
// exact {type, workspace_id, path, forced} result, the branch kept, the
// linked workspace CLOSED and its root shell terminated, the parent
// workspace kept.
func TestSpikeWorktreeRemoveHerdrShapes(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)
	repo := newFixtureRepo(t, artifacts, server, "repo")
	var manager workspaceCreatedResponse
	server.call(t, "workspace.create", map[string]any{"cwd": repo.Root, "focus": true}, &manager)

	created := createAttemptWorktree(t, server, repo, artifacts, "t1a1")
	path := created.Worktree.Path
	requested := filepath.Join(artifacts.dir(t, "worktrees"), "attempt-t1a1")
	canonical := canonicalPath(t, requested)
	t.Logf("worktree.create path %q (requested %q, canonical %q)", path, requested, canonical)
	if path != requested {
		t.Errorf("worktree.create reported path %q, want the requested spelling %q", path, requested)
	}
	if created.Workspace.Worktree == nil || created.Workspace.Worktree.CheckoutPath != path {
		t.Errorf("workspace membership checkout_path = %+v, want %q", created.Workspace.Worktree, path)
	}
	shell := server.processInfo(t, created.RootPane.PaneID).ShellPID
	if shell == 0 || !pidAlive(shell) {
		t.Fatalf("root pane shell pid %d is not a live process", shell)
	}

	// Dirty: an untracked file. Refused without force; nothing changes.
	untracked := filepath.Join(path, "untracked.txt")
	if err := os.WriteFile(untracked, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := server.callRaw(t, "worktree.remove", map[string]any{"workspace_id": created.Workspace.WorkspaceID, "force": false})
	message := requireAPIError(t, err, "dirty_worktree_requires_force")
	if want := "fatal: '" + path + "' contains modified or untracked files, use --force to delete it"; message != want {
		t.Errorf("dirty refusal message = %q, want %q", message, want)
	}
	if !pathExists(t, untracked) || !listedByGit(t, repo, path) {
		t.Fatal("a refused removal changed the checkout")
	}
	if !server.workspaceExists(t, created.Workspace.WorkspaceID) || !server.paneExists(t, created.RootPane.PaneID) || !pidAlive(shell) {
		t.Fatal("a refused removal closed the workspace, its pane or its shell")
	}

	// Clean: removed.
	if err := os.Remove(untracked); err != nil {
		t.Fatal(err)
	}
	raw, err := server.callRaw(t, "worktree.remove", map[string]any{"workspace_id": created.Workspace.WorkspaceID, "force": false})
	if err != nil {
		t.Fatalf("worktree.remove of a clean checkout: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	gotKeys := make([]string, 0, len(keys))
	for k := range keys {
		gotKeys = append(gotKeys, k)
	}
	slices.Sort(gotKeys)
	if want := []string{"forced", "path", "type", "workspace_id"}; !slices.Equal(gotKeys, want) {
		t.Errorf("worktree.remove result keys = %v, want %v (raw %s)", gotKeys, want, raw)
	}
	var removed worktreeRemovedResult
	if err := json.Unmarshal(raw, &removed); err != nil {
		t.Fatal(err)
	}
	want := worktreeRemovedResult{Type: "worktree_removed", WorkspaceID: created.Workspace.WorkspaceID, Path: path, Forced: false}
	if removed != want {
		t.Errorf("worktree.remove result = %+v, want %+v", removed, want)
	}
	if pathExists(t, path) || listedByGit(t, repo, path) {
		t.Error("the checkout survived a successful worktree.remove")
	}
	if listed, _, _ := server.listedByHerdr(t, repo.Root, path); listed {
		t.Error("worktree.list still names the removed checkout")
	}
	if got := mustRunGit(t, repo.Root, repo.gitHome, "rev-parse", "--verify", "-q", "refs/heads/hop/r1/t1a1^{commit}"); strings.TrimSpace(got) != repo.Base {
		t.Errorf("attempt branch after removal = %q, want %s (never deleted)", got, repo.Base)
	}
	if server.workspaceExists(t, created.Workspace.WorkspaceID) {
		t.Error("the linked workspace is still open after worktree.remove; want it closed")
	}
	if server.paneExists(t, created.RootPane.PaneID) {
		t.Error("the linked workspace's root pane survived worktree.remove")
	}
	if !waitUntil(func() bool { return !pidAlive(shell) }) {
		t.Errorf("the linked workspace's root shell (pid %d) is still running after worktree.remove", shell)
	}
	if !server.workspaceExists(t, manager.Workspace.WorkspaceID) {
		t.Error("the parent workspace was closed by removing one linked worktree")
	}
}

// TestSpikeWorktreeRemoveNeedsOpenWorkspace pins that worktree.remove is
// addressed only through an OPEN workspace: once the linked workspace is
// closed (Herdr state only — the checkout stays on disk), the removal is
// refused with workspace_not_found and the checkout survives, still listed
// by git and by worktree.list with no open workspace.
func TestSpikeWorktreeRemoveNeedsOpenWorkspace(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)
	repo := newFixtureRepo(t, artifacts, server, "repo")
	var manager workspaceCreatedResponse
	server.call(t, "workspace.create", map[string]any{"cwd": repo.Root, "focus": true}, &manager)
	created := createAttemptWorktree(t, server, repo, artifacts, "t1a1")
	path := created.Worktree.Path

	var closed struct {
		Type string `json:"type"`
	}
	server.call(t, "workspace.close", map[string]any{"workspace_id": created.Workspace.WorkspaceID}, &closed)
	t.Logf("workspace.close result type %q", closed.Type)
	if server.workspaceExists(t, created.Workspace.WorkspaceID) {
		t.Fatal("workspace.close left the linked workspace open")
	}
	if !pathExists(t, path) || !listedByGit(t, repo, path) {
		t.Fatal("workspace.close removed the checkout; want Herdr state only")
	}

	_, err := server.callRaw(t, "worktree.remove", map[string]any{"workspace_id": created.Workspace.WorkspaceID, "force": false})
	message := requireAPIError(t, err, "workspace_not_found")
	if want := "workspace " + created.Workspace.WorkspaceID + " not found"; message != want {
		t.Errorf("refusal message = %q, want %q", message, want)
	}
	if !pathExists(t, path) || !listedByGit(t, repo, path) {
		t.Error("a refused removal changed the checkout")
	}
	listed, open, prunable := server.listedByHerdr(t, repo.Root, path)
	if !listed || open != nil || prunable {
		t.Errorf("worktree.list entry: listed=%v open_workspace_id=%v prunable=%v, want listed, no open workspace, not prunable", listed, open, prunable)
	}
}

// TestSpikeWorkspaceIDReissuedAfterRestart pins that a Herdr workspace id
// is not a durable address: ids are `w<n>` from a per-process counter that
// a restarted server re-seeds from the highest RESTORED id, so the id of a
// workspace closed before the restart is issued again to the next new
// workspace afterwards.
func TestSpikeWorkspaceIDReissuedAfterRestart(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)
	dirA := artifacts.dir(t, "a")
	dirB := artifacts.dir(t, "b")
	dirC := artifacts.dir(t, "c")

	var a, b workspaceCreatedResponse
	server.call(t, "workspace.create", map[string]any{"cwd": dirA, "focus": true, "label": "hop-spike-id-a"}, &a)
	server.call(t, "workspace.create", map[string]any{"cwd": dirB, "focus": false, "label": "hop-spike-id-b"}, &b)
	t.Logf("before restart: a=%s b=%s", a.Workspace.WorkspaceID, b.Workspace.WorkspaceID)
	server.call(t, "workspace.close", map[string]any{"workspace_id": b.Workspace.WorkspaceID}, nil)

	server.restart(t)
	if got := server.snapshotWorkspaceByLabel(t, "hop-spike-id-a"); got != a.Workspace.WorkspaceID {
		t.Fatalf("restored workspace a has id %q, want %q (restore evidence inconclusive)", got, a.Workspace.WorkspaceID)
	}
	var c workspaceCreatedResponse
	server.call(t, "workspace.create", map[string]any{"cwd": dirC, "focus": false, "label": "hop-spike-id-c"}, &c)
	t.Logf("after restart: a=%s c=%s (closed b was %s)", a.Workspace.WorkspaceID, c.Workspace.WorkspaceID, b.Workspace.WorkspaceID)
	if c.Workspace.WorkspaceID != b.Workspace.WorkspaceID {
		t.Errorf("new workspace after restart has id %q; want the closed workspace's id %q reissued", c.Workspace.WorkspaceID, b.Workspace.WorkspaceID)
	}
}

// TestSpikeGitWorktreeRemoveLeavesHerdrWorkspace pins what a git-side
// removal (`git worktree remove <path>`, no force, run outside Herdr)
// leaves in Herdr: the linked workspace stays open with its membership
// still naming the removed checkout, its root shell keeps running, and
// worktree.list no longer names the checkout. worktree.remove against that
// stale workspace, and workspace.close as the manual tidy step, are pinned
// too.
func TestSpikeGitWorktreeRemoveLeavesHerdrWorkspace(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)
	repo := newFixtureRepo(t, artifacts, server, "repo")
	var manager workspaceCreatedResponse
	server.call(t, "workspace.create", map[string]any{"cwd": repo.Root, "focus": true}, &manager)
	created := createAttemptWorktree(t, server, repo, artifacts, "t1a1")
	path := created.Worktree.Path
	shell := server.processInfo(t, created.RootPane.PaneID).ShellPID

	if out, err := runGit(t, repo.Root, repo.gitHome, "worktree", "remove", path); err != nil {
		t.Fatalf("git worktree remove: %v\n%s", err, out)
	}
	if pathExists(t, path) || listedByGit(t, repo, path) {
		t.Fatal("git worktree remove left the checkout")
	}

	info := server.workspaceGet(t, created.Workspace.WorkspaceID)
	t.Logf("workspace after git removal: %+v membership %+v", info, info.Worktree)
	if info.Worktree == nil || info.Worktree.CheckoutPath != path || !info.Worktree.IsLinkedWorktree {
		t.Errorf("stale workspace membership = %+v, want it still naming %q as a linked worktree", info.Worktree, path)
	}
	if !server.paneExists(t, created.RootPane.PaneID) || !pidAlive(shell) {
		t.Error("git-side removal closed the linked workspace's pane or shell; want both left running")
	}
	if listed, _, _ := server.listedByHerdr(t, repo.Root, path); listed {
		t.Error("worktree.list still names the git-removed checkout")
	}

	_, err := server.callRaw(t, "worktree.remove", map[string]any{"workspace_id": created.Workspace.WorkspaceID, "force": false})
	var apiErr *herdr.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("worktree.remove of a stale workspace: %v, want an API error", err)
	}
	t.Logf("worktree.remove of the stale workspace: code %q message %q", apiErr.Code, apiErr.Message)
	if apiErr.Code != "worktree_remove_failed" {
		t.Errorf("stale-workspace worktree.remove code = %q, want worktree_remove_failed", apiErr.Code)
	}
	if !server.workspaceExists(t, created.Workspace.WorkspaceID) {
		t.Error("a failed worktree.remove closed the stale workspace")
	}

	server.call(t, "workspace.close", map[string]any{"workspace_id": created.Workspace.WorkspaceID}, nil)
	if server.workspaceExists(t, created.Workspace.WorkspaceID) {
		t.Error("workspace.close left the stale workspace open")
	}
	if !waitUntil(func() bool { return !pidAlive(shell) }) {
		t.Errorf("the stale workspace's shell (pid %d) survived workspace.close", shell)
	}
	if !server.workspaceExists(t, manager.Workspace.WorkspaceID) {
		t.Error("closing the stale linked workspace closed its parent")
	}
}
