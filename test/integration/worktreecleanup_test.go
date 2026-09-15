package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// registerWorktreeCleanup registers a t.Cleanup that tears down every git
// worktree the real herdr pipeline may have created against repo through
// server (cleanupServerWorktrees), if server is non-nil. A pure git-level
// scenario that never touches a herdr server cannot have created a real
// worktree through Herdr's own worktree.create in the first place, so nil
// is a deliberate no-op, not an oversight.
//
// Cleanup functions run in LIFO order, and every caller here creates repo
// strictly after server (prepareServer/newFixtureRunEnv always precede
// newFixtureRepo), so this cleanup always runs — and therefore removes
// every worktree through git — before newServerRoots' own blanket
// os.RemoveAll(base) deletes the directory tree out from under it.
func registerWorktreeCleanup(t *testing.T, server *testServer, repo *fixtureRepo) {
	t.Helper()
	if server == nil {
		return
	}
	t.Cleanup(func() { cleanupServerWorktrees(t, server, repo) })
}

// cleanupServerWorktrees tears down every git worktree the real herdr
// pipeline may have created against repo through server. Every repository
// this suite hands to `hop run`/`hop launch`, or drives directly through
// `worktree.create`, can accumulate real, disposable `git worktree add`
// checkouts, and this proves none is left behind: inside the test's own
// scratch roots (belt-and-suspenders on top of newServerRoots' blanket
// os.RemoveAll — TestRealProcessWorktreeCleanedUpAfterRun asserts the
// git-level teardown itself works, independent of that later blanket
// removal) or, in the pre-fix leak case this mechanism guards against,
// outside them (evidence (b): a live run whose HOME pointed at the
// operator's actual home directory left
// `~/.herdr/worktrees/repo/hop-run-1` behind, twice).
//
// Every worktree path this removes comes from `git -C repo.Root worktree
// list --porcelain` — repo's own administrative metadata — never from
// globbing `~/.herdr` or any other path outside the test's own roots, so
// this can never discover, and therefore never touch, a directory unrelated
// to this test's own fixture repository. The main checkout is always
// skipped, so repo's own branches (hop/run-1 and friends) stay reachable in
// whatever of the repository is retained on failure.
func cleanupServerWorktrees(t *testing.T, server *testServer, repo *fixtureRepo) {
	t.Helper()
	scratchRoots := serverScratchRoots(t, server)
	for _, path := range parseWorktreeListPaths(repo.git(t, "worktree", "list", "--porcelain")) {
		if samePath(t, path, repo.Root) {
			continue // the main checkout: never removed.
		}
		removeGitWorktree(t, repo, path, scratchRoots)
	}

	// Second pass: a checkout directory worktree.create started but never
	// finished registering with git (e.g. a crash between mkdir and `git
	// worktree add`) would not appear in the porcelain listing above at
	// all. The configured worktrees directory is created solely for this
	// one server (newServerRoots' per-test MkdirTemp base), so anything
	// left under it at this point belongs to this test and nothing else.
	configuredDir := filepath.Join(server.base, "worktrees")
	entries, err := os.ReadDir(configuredDir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Errorf("read configured worktrees directory %s: %v", configuredDir, err)
		return
	}
	for _, entry := range entries {
		leftover := filepath.Join(configuredDir, entry.Name())
		if err := os.RemoveAll(leftover); err != nil {
			t.Errorf("remove leftover worktree directory %s: %v", leftover, err)
			continue
		}
		t.Logf("removed leftover worktree directory %s (never registered with git)", leftover)
	}
}

// serverScratchRoots resolves the symlinked absolute form of every root this
// test's own worktrees may legitimately live under: the server's own
// temporary base (the configured [worktrees] directory, harness_test.go's
// testConfig) and its artifact directory (a handful of spike scenarios pass
// worktree.create an explicit path under artifacts.dir(t, "worktrees")
// instead of relying on the configured default).
func serverScratchRoots(t *testing.T, server *testServer) []string {
	t.Helper()
	roots := make([]string, 0, 2)
	for _, root := range []string{server.base, server.artifacts.path} {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			t.Fatalf("resolve test scratch root %q: %v", root, err)
		}
		roots = append(roots, resolved)
	}
	return roots
}

// removeGitWorktree removes one linked worktree git reported for repo. A
// path outside every known scratch root is the pre-fix leak case: before
// ever touching it, this independently verifies — never merely trusting the
// porcelain listing — that the directory's own ".git" file resolves back
// into repo's own .git/worktrees administrative directory. A path that
// fails that check is left alone, and fails the test with the exact
// location, rather than being removed on the strength of git's report
// alone.
func removeGitWorktree(t *testing.T, repo *fixtureRepo, path string, scratchRoots []string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			repo.git(t, "worktree", "prune")
			t.Logf("pruned stale worktree entry %s (directory already gone) for repo %s", path, repo.Root)
			return
		}
		t.Fatalf("stat worktree %s: %v", path, err)
	}
	resolved := canonicalPath(t, path)
	if !underAnyRoot(resolved, scratchRoots) && !worktreeBelongsToRepo(path, repo) {
		t.Fatalf("worktree %q reported by `git -C %s worktree list` is outside this test's scratch roots (%v) and its .git does not point back at this fixture repository; refusing to remove it automatically — investigate by hand", path, repo.Root, scratchRoots)
	}
	repo.git(t, "worktree", "remove", "--force", path)
	repo.git(t, "worktree", "prune")
	t.Logf("removed worktree %s (repo %s)", path, repo.Root)
}

// underAnyRoot reports whether resolved is at or under any of roots.
func underAnyRoot(resolved string, roots []string) bool {
	for _, root := range roots {
		if resolved == root || strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// worktreeBelongsToRepo independently confirms worktreePath is a linked
// worktree of repo, without trusting `git worktree list`'s report alone: a
// linked worktree's own ".git" is a single-line file ("gitdir:
// <repo>/.git/worktrees/<id>"), never a real repository, and that line must
// resolve inside repo's own .git/worktrees directory.
func worktreeBelongsToRepo(worktreePath string, repo *fixtureRepo) bool {
	content, err := os.ReadFile(filepath.Join(worktreePath, ".git")) //nolint:gosec // G304: worktreePath is one entry from `git worktree list` against this test's own fixture repo; this reads it only to independently confirm that report before any removal.
	if err != nil {
		return false
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(content)), "gitdir: ")
	if !ok {
		return false
	}
	repoWorktreesDir, err := filepath.EvalSymlinks(filepath.Join(repo.Root, ".git", "worktrees"))
	if err != nil {
		return false
	}
	target := gitdir
	if resolved, err := filepath.EvalSymlinks(gitdir); err == nil {
		target = resolved
	}
	return target == repoWorktreesDir || strings.HasPrefix(target, repoWorktreesDir+string(os.PathSeparator))
}

// parseWorktreeListPaths extracts every "worktree <path>" line from `git
// worktree list --porcelain` output, in the order git reports them (the main
// checkout first, per git's own documented behavior).
func parseWorktreeListPaths(porcelain string) []string {
	var paths []string
	for _, line := range strings.Split(porcelain, "\n") {
		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			paths = append(paths, path)
		}
	}
	return paths
}

// TestRealProcessWorktreeCleanedUpAfterRun proves cleanupServerWorktrees
// itself against a worktree the real pipeline created: a real hop run
// creates its attempt worktree under the configured [worktrees] directory
// (harness_test.go's testConfig — confirming the pin from evidence (b) is
// effective for an ordinary run, not only the live one), and the worktree
// is still present on disk once the run completes (worktrees are preserved,
// never removed by the run itself — stop_test.go's
// TestRealProcessStopWithTerminationObserved already establishes this for a
// stopped run; this is the same fact for a completed one).
//
// cleanupServerWorktrees is invoked directly here, rather than only relying
// on the t.Cleanup registerWorktreeCleanup already attached when the
// fixture repository was created, specifically so this test can assert the
// postcondition — the worktree directory gone, `git worktree list` down to
// the main checkout alone — from inside its own body. The later automatic
// t.Cleanup call is then a harmless no-op against an already-clean repo.
func TestRealProcessWorktreeCleanedUpAfterRun(t *testing.T) {
	fx := startFixtureRun(t, "submit-valid")
	fx.requireRunState(t, "completed")
	waitForControllerExit(t, fx.controller, 30*time.Second)

	worktreePath := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT path FROM worktrees WHERE run_id = '%s';", fx.runID))
	if worktreePath == "" {
		t.Fatalf("run %s has no recorded worktree path", fx.runID)
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("worktree %s missing before cleanup even runs: %v", worktreePath, err)
	}
	scratchRoot := canonicalPath(t, fx.server.base)
	if resolved := canonicalPath(t, worktreePath); !strings.HasPrefix(resolved, scratchRoot+string(os.PathSeparator)) {
		t.Fatalf("worktree %s is not under the test's scratch root %s; the [worktrees] directory pin may not be effective", resolved, scratchRoot)
	}

	cleanupServerWorktrees(t, fx.server, fx.repo)

	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still exists after cleanup (stat err: %v)", worktreePath, err)
	}
	remaining := parseWorktreeListPaths(fx.repo.git(t, "worktree", "list", "--porcelain"))
	if len(remaining) != 1 || !samePath(t, remaining[0], fx.repo.Root) {
		t.Fatalf("`git worktree list` after cleanup = %v, want only the main checkout %s", remaining, fx.repo.Root)
	}
}
