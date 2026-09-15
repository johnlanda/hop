package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mergeIdentityArgs are the frozen non-interactive argv prefix flags the
// design's merge invocations use (G2): an inline `-c user.name=... -c
// user.email=...` identity so a merge never depends on ambient git
// configuration existing at the checkout, plus `-c commit.gpgsign=false -c
// merge.verifysignatures=false` so a repository with signing turned on
// locally (commit.gpgsign=true) never blocks an automated merge -- these
// two `-c` flags always win over any config file, including the checkout's
// own local config, because command-line `-c` has the highest git config
// precedence.
func mergeIdentityArgs() []string {
	return []string{
		"-c", "user.name=hop-integration",
		"-c", "user.email=hop-integration@example.invalid",
		"-c", "commit.gpgsign=false",
		"-c", "merge.verifysignatures=false",
	}
}

// mergeEnviron is the frozen non-interactive environment (G2): fixtureRepo's
// hermetic git environment (fixtureGitEnviron, which already sets
// GIT_CONFIG_GLOBAL=/dev/null and GIT_CONFIG_SYSTEM=/dev/null -- no developer
// git configuration) plus GIT_TERMINAL_PROMPT=0, so a merge that would
// otherwise need a credential or host-key prompt fails fast instead of
// hanging -- nothing in these probes needs real network access, but the
// frozen argv/env pins the exact invocation production code would use. This
// suppresses only GLOBAL/SYSTEM config, never a repository's own LOCAL
// config or its .git/hooks -- see TestSpikeMergeRepoLocalHooksFire and
// TestSpikeMergeRepoLocalSigningConfigDoesNotBlock.
func mergeEnviron(gitHome string) []string {
	return append(fixtureGitEnviron(gitHome), "GIT_TERMINAL_PROMPT=0")
}

// runMerge runs `git -c user.name=... -c user.email=... merge --no-ff
// --no-edit <source>` in dir under mergeEnviron, returning combined output
// and any error without failing the test.
func runMerge(t *testing.T, dir, gitHome, source string) (string, error) {
	t.Helper()
	args := append(mergeIdentityArgs(), "merge", "--no-ff", "--no-edit", source)
	return runGitWithEnv(t, dir, mergeEnviron(gitHome), args...)
}

// isUnorderedPair reports whether {parents[0], parents[1]} equals {a, b} in
// either order.
func isUnorderedPair(parents []string, a, b string) bool {
	return (parents[0] == a && parents[1] == b) || (parents[0] == b && parents[1] == a)
}

// detachedScratch creates a plain `git worktree add --detach` checkout of
// at, registered for removal on test cleanup, standing in for the
// controller-owned scratch checkout every G2 case merges into. It never
// touches any branch ref by construction (detached HEAD).
func detachedScratch(t *testing.T, repo *fixtureRepo, artifacts *artifactDir, name, at string) string {
	t.Helper()
	path := artifacts.dir(t, name)
	mustRunGit(t, repo.Root, repo.gitHome, "worktree", "add", "--detach", path, at)
	t.Cleanup(func() {
		if out, err := runGit(t, repo.Root, repo.gitHome, "worktree", "remove", "--force", path); err != nil {
			t.Logf("remove detached scratch checkout %s: %v\n%s", name, err, out)
		}
	})
	return path
}

// TestSpikeMergeTwoParentCommit is G2's first matrix case: a genuine
// two-parent merge of two commits that diverged from a common ancestor,
// under the frozen non-interactive argv/env, in a detached scratch checkout
// that leaves the run-scoped integration ref untouched (the property
// TestSpikeDetachedMergeLeavesRefUntouched already established; reasserted
// here as part of the matrix rather than assumed).
func TestSpikeMergeTwoParentCommit(t *testing.T) {
	artifacts := newArtifactDir(t)
	repo := newFixtureRepo(t, artifacts, "repo")
	const ref = "refs/heads/hop/r1/integration"
	mustRunGit(t, repo.Root, repo.gitHome, "update-ref", ref, repo.Base)

	repo.writeFile(t, "source.txt", "source change\n", 0o644)
	source := repo.commit(t, "source change")

	scratch := detachedScratch(t, repo, artifacts, "scratch-two-parent", repo.Base)
	if out, err := runMerge(t, scratch, repo.gitHome, source); err != nil {
		t.Fatalf("git merge --no-ff --no-edit %s: %v\n%s", source, err, out)
	}
	mergeOID := strings.TrimSpace(mustRunGit(t, scratch, repo.gitHome, "rev-parse", "HEAD"))
	parents := strings.Fields(mustRunGit(t, scratch, repo.gitHome, "log", "-1", "--format=%P", mergeOID))
	if len(parents) != 2 {
		t.Fatalf("merge commit %s has %d parents %v, want exactly 2", mergeOID, len(parents), parents)
	}
	if !isUnorderedPair(parents, repo.Base, source) {
		t.Errorf("merge commit parents = %v, want exactly {%s, %s}", parents, repo.Base, source)
	}
	if got := readRef(t, scratch, repo.gitHome, ref); got != repo.Base {
		t.Errorf("integration ref = %s after the detached merge, want it untouched at %s", got, repo.Base)
	}
}

// TestSpikeMergeConflictExitCodeAndIndexState is G2's conflict case: two
// commits that diverge from a common ancestor with CONFLICTING changes to
// the same file. `git merge --no-ff --no-edit` exits non-zero, leaves the
// index with unmerged ("UU") entries, and -- once aborted, as design section
// 4's decision table specifies for a real conflict -- the integration ref
// stays untouched throughout.
func TestSpikeMergeConflictExitCodeAndIndexState(t *testing.T) {
	artifacts := newArtifactDir(t)
	repo := newFixtureRepo(t, artifacts, "repo")
	const ref = "refs/heads/hop/r1/integration"

	repo.writeFile(t, "contested.txt", "base content\n", 0o644)
	commonAncestor := repo.commit(t, "common ancestor for the conflict case")
	mustRunGit(t, repo.Root, repo.gitHome, "update-ref", ref, commonAncestor)

	repo.writeFile(t, "contested.txt", "head branch content\n", 0o644)
	headSide := repo.commit(t, "head side change")
	mustRunGit(t, repo.Root, repo.gitHome, "reset", "--hard", commonAncestor)
	repo.writeFile(t, "contested.txt", "source branch content\n", 0o644)
	sourceSide := repo.commit(t, "source side change")
	mustRunGit(t, repo.Root, repo.gitHome, "reset", "--hard", headSide) // restore repo.Root's own working branch

	scratch := detachedScratch(t, repo, artifacts, "scratch-conflict", headSide)
	out, err := runMerge(t, scratch, repo.gitHome, sourceSide)
	if err == nil {
		t.Fatalf("git merge --no-ff --no-edit of a genuinely conflicting source unexpectedly succeeded:\n%s", out)
	}
	t.Logf("expected conflict output:\n%s", out)

	status := mustRunGit(t, scratch, repo.gitHome, "status", "--porcelain")
	if !strings.Contains(status, "UU contested.txt") {
		t.Errorf("git status --porcelain after the conflict = %q, want an unmerged (UU) entry for contested.txt", status)
	}
	unmerged := mustRunGit(t, scratch, repo.gitHome, "diff", "--name-only", "--diff-filter=U")
	if strings.TrimSpace(unmerged) != "contested.txt" {
		t.Errorf("git diff --diff-filter=U names %q, want exactly contested.txt", strings.TrimSpace(unmerged))
	}

	if got := readRef(t, repo.Root, repo.gitHome, ref); got != commonAncestor {
		t.Errorf("integration ref = %s during an unresolved conflict, want it untouched at %s", got, commonAncestor)
	}

	// Design section 4's decision-table row: conflict -> abort, evidence,
	// `conflicted`. Aborting must cleanly restore the scratch checkout and
	// must not touch the ref either.
	mustRunGit(t, scratch, repo.gitHome, "merge", "--abort")
	if head := strings.TrimSpace(mustRunGit(t, scratch, repo.gitHome, "rev-parse", "HEAD")); head != headSide {
		t.Errorf("scratch checkout HEAD after merge --abort = %s, want it restored to %s", head, headSide)
	}
	if got := readRef(t, repo.Root, repo.gitHome, ref); got != commonAncestor {
		t.Errorf("integration ref = %s after merge --abort, want it still untouched at %s", got, commonAncestor)
	}
}

// TestSpikeMergeAlreadyUpToDateNoOp is G2's ancestor case: merging a commit
// that is already an ancestor of the checkout's HEAD is a no-op -- "Already
// up to date.", exit 0, HEAD (and therefore the ref) unchanged, no new
// commit created.
func TestSpikeMergeAlreadyUpToDateNoOp(t *testing.T) {
	artifacts := newArtifactDir(t)
	repo := newFixtureRepo(t, artifacts, "repo")
	const ref = "refs/heads/hop/r1/integration"

	repo.writeFile(t, "advance.txt", "advance\n", 0o644)
	head := repo.commit(t, "advance past base")
	mustRunGit(t, repo.Root, repo.gitHome, "update-ref", ref, head)

	scratch := detachedScratch(t, repo, artifacts, "scratch-up-to-date", head)
	out, err := runMerge(t, scratch, repo.gitHome, repo.Base) // repo.Base is an ancestor of head
	if err != nil {
		t.Fatalf("git merge --no-ff --no-edit of an ancestor unexpectedly failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Already up to date") {
		t.Errorf("merge-of-an-ancestor output = %q, want it to report already up to date", out)
	}
	if got := strings.TrimSpace(mustRunGit(t, scratch, repo.gitHome, "rev-parse", "HEAD")); got != head {
		t.Errorf("scratch checkout HEAD after the no-op merge = %s, want it unchanged at %s", got, head)
	}
	if got := readRef(t, repo.Root, repo.gitHome, ref); got != head {
		t.Errorf("integration ref = %s after the no-op merge, want it unchanged at %s", got, head)
	}
}

// TestSpikeMergeNoFFForcesCommitOnFastForwardableHead is G2's --no-ff-forces
// case: when the checkout's HEAD is a strict ancestor of the source (a plain
// `git merge` would just fast-forward), `--no-ff` still produces a genuine
// two-parent merge commit rather than moving HEAD directly to source.
func TestSpikeMergeNoFFForcesCommitOnFastForwardableHead(t *testing.T) {
	artifacts := newArtifactDir(t)
	repo := newFixtureRepo(t, artifacts, "repo")
	const ref = "refs/heads/hop/r1/integration"
	mustRunGit(t, repo.Root, repo.gitHome, "update-ref", ref, repo.Base)

	repo.writeFile(t, "descendant.txt", "descendant\n", 0o644)
	source := repo.commit(t, "a direct descendant of base") // repo.Base is a strict ancestor of source

	scratch := detachedScratch(t, repo, artifacts, "scratch-no-ff-forced", repo.Base)
	if out, err := runMerge(t, scratch, repo.gitHome, source); err != nil {
		t.Fatalf("git merge --no-ff --no-edit %s: %v\n%s", source, err, out)
	}
	mergeOID := strings.TrimSpace(mustRunGit(t, scratch, repo.gitHome, "rev-parse", "HEAD"))
	if mergeOID == source {
		t.Fatalf("--no-ff fast-forwarded to %s instead of producing a new merge commit", source)
	}
	parents := strings.Fields(mustRunGit(t, scratch, repo.gitHome, "log", "-1", "--format=%P", mergeOID))
	if len(parents) != 2 {
		t.Fatalf("merge commit %s has %d parents %v, want exactly 2 even though a fast-forward was possible", mergeOID, len(parents), parents)
	}
	if !isUnorderedPair(parents, repo.Base, source) {
		t.Errorf("merge commit parents = %v, want exactly {%s, %s}", parents, repo.Base, source)
	}
	if got := readRef(t, repo.Root, repo.gitHome, ref); got != repo.Base {
		t.Errorf("integration ref = %s after the forced merge, want it untouched at %s", got, repo.Base)
	}
}

// TestSpikeMergeRepoLocalHooksFire is G2's hook-configured-repository case:
// mergeEnviron only suppresses GLOBAL and SYSTEM git configuration
// (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM=/dev/null); it does nothing to a
// repository's own LOCAL .git/hooks. Neither `pre-merge-commit` nor
// `post-merge` is skipped by the frozen argv (there is no `--no-verify` in
// it), so a repo-local hook script fires during an automated merge --
// arbitrary repository-supplied code executes with the same privileges as
// the merge itself. See TestSpikeMergeNoVerifySkipsPreMergeCommitOnly for
// what adding `--no-verify` does and does not suppress, and
// TestSpikeMergeHooksPathSuppressesRepoLocalHooks for the option that
// suppresses both.
func TestSpikeMergeRepoLocalHooksFire(t *testing.T) {
	artifacts := newArtifactDir(t)
	repo := newFixtureRepo(t, artifacts, "repo")

	hooksDir := filepath.Join(repo.Root, ".git", "hooks")
	preMergeCommitMarker := filepath.Join(artifacts.dir(t, "hook-evidence"), "pre-merge-commit-fired")
	postMergeMarker := filepath.Join(artifacts.dir(t, "hook-evidence"), "post-merge-fired")
	writeHookScript(t, hooksDir, "pre-merge-commit", preMergeCommitMarker)
	writeHookScript(t, hooksDir, "post-merge", postMergeMarker)

	repo.writeFile(t, "source.txt", "source change\n", 0o644)
	source := repo.commit(t, "source change")
	scratch := detachedScratch(t, repo, artifacts, "scratch-hooks", repo.Base)
	// Hooks live in the shared .git/hooks (common to every linked worktree of
	// this repository), so they apply to a merge run in the detached scratch
	// checkout exactly as they would in the main checkout.
	if out, err := runMerge(t, scratch, repo.gitHome, source); err != nil {
		t.Fatalf("git merge --no-ff --no-edit %s: %v\n%s", source, err, out)
	}

	if _, err := os.Stat(preMergeCommitMarker); err != nil {
		t.Errorf("pre-merge-commit hook did not fire under the frozen argv: %v", err)
	}
	if _, err := os.Stat(postMergeMarker); err != nil {
		t.Errorf("post-merge hook did not fire under the frozen argv: %v", err)
	}
}

// TestSpikeMergeNoVerifySkipsPreMergeCommitOnly is G2's `--no-verify`
// control: adding `--no-verify` to the frozen argv skips `pre-merge-commit`
// (git's own documented behavior for `merge --no-verify`) but has no effect
// on `post-merge`, which fires unconditionally after a successful merge and
// has no suppression flag of its own.
func TestSpikeMergeNoVerifySkipsPreMergeCommitOnly(t *testing.T) {
	artifacts := newArtifactDir(t)
	repo := newFixtureRepo(t, artifacts, "repo")

	hooksDir := filepath.Join(repo.Root, ".git", "hooks")
	preMergeCommitMarker := filepath.Join(artifacts.dir(t, "hook-evidence"), "pre-merge-commit-fired")
	postMergeMarker := filepath.Join(artifacts.dir(t, "hook-evidence"), "post-merge-fired")
	writeHookScript(t, hooksDir, "pre-merge-commit", preMergeCommitMarker)
	writeHookScript(t, hooksDir, "post-merge", postMergeMarker)

	repo.writeFile(t, "source.txt", "source change\n", 0o644)
	source := repo.commit(t, "source change")
	scratch := detachedScratch(t, repo, artifacts, "scratch-no-verify", repo.Base)

	args := append(append([]string{}, mergeIdentityArgs()...), "merge", "--no-ff", "--no-edit", "--no-verify", source)
	if out, err := runGitWithEnv(t, scratch, mergeEnviron(repo.gitHome), args...); err != nil {
		t.Fatalf("git merge --no-ff --no-edit --no-verify %s: %v\n%s", source, err, out)
	}

	if _, err := os.Stat(preMergeCommitMarker); err == nil {
		t.Error("pre-merge-commit hook fired despite --no-verify")
	}
	if _, err := os.Stat(postMergeMarker); err != nil {
		t.Errorf("post-merge hook did not fire despite --no-verify (it has no suppression flag of its own): %v", err)
	}
}

// TestSpikeMergeHooksPathSuppressesRepoLocalHooks is G2's hook-suppression
// verification: `-c core.hooksPath=<dir>` redirects git's hook lookup away
// from the repository's own .git/hooks entirely, so a hostile or
// accidental repo-local pre-merge-commit/post-merge script never runs --
// confirmed for both an EMPTY existing temp directory and a directory path
// that does not exist at all (git treats a missing hooksPath as "no hooks
// configured there", not an error).
func TestSpikeMergeHooksPathSuppressesRepoLocalHooks(t *testing.T) {
	artifacts := newArtifactDir(t)
	repo := newFixtureRepo(t, artifacts, "repo")

	hooksDir := filepath.Join(repo.Root, ".git", "hooks")
	preMergeCommitMarker := filepath.Join(artifacts.dir(t, "hook-evidence"), "pre-merge-commit-fired")
	postMergeMarker := filepath.Join(artifacts.dir(t, "hook-evidence"), "post-merge-fired")
	writeHookScript(t, hooksDir, "pre-merge-commit", preMergeCommitMarker)
	writeHookScript(t, hooksDir, "post-merge", postMergeMarker)

	repo.writeFile(t, "source.txt", "source change\n", 0o644)
	source := repo.commit(t, "source change")

	emptyHooksDir := artifacts.dir(t, "empty-hooks-path")
	missingHooksDir := filepath.Join(artifacts.dir(t, "hooks-path-parent"), "does-not-exist")

	for _, hooksPath := range []string{emptyHooksDir, missingHooksDir} {
		scratch := detachedScratch(t, repo, artifacts, "scratch-hookspath-"+filepath.Base(hooksPath), repo.Base)
		args := append(append([]string{}, mergeIdentityArgs()...), "-c", "core.hooksPath="+hooksPath,
			"merge", "--no-ff", "--no-edit", source)
		if out, err := runGitWithEnv(t, scratch, mergeEnviron(repo.gitHome), args...); err != nil {
			t.Fatalf("git merge --no-ff --no-edit %s with core.hooksPath=%s: %v\n%s", source, hooksPath, err, out)
		}
		if _, err := os.Stat(preMergeCommitMarker); err == nil {
			t.Errorf("pre-merge-commit hook fired despite core.hooksPath=%s", hooksPath)
		}
		if _, err := os.Stat(postMergeMarker); err == nil {
			t.Errorf("post-merge hook fired despite core.hooksPath=%s", hooksPath)
		}
		t.Logf("core.hooksPath=%s (exists=%v) suppressed both hooks", hooksPath, hooksPath == emptyHooksDir)
	}
}

// writeHookScript installs an executable git hook at hooksDir/name that
// touches markerPath when it runs, so a test can observe whether git invoked
// it.
func writeHookScript(t *testing.T, hooksDir, name, markerPath string) {
	t.Helper()
	script := "#!/bin/sh\ntouch " + markerPath + "\n"
	path := filepath.Join(hooksDir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // G306: a hook must be executable; it lives in this test's own throwaway fixture repository.
		t.Fatalf("write hook %s: %v", name, err)
	}
}

// TestSpikeMergeRepoLocalSigningConfigDoesNotBlock is G2's signing-configured
// repository case: even with commit.gpgsign=true set in the repository's own
// LOCAL config (mergeEnviron does not suppress local config, only
// global/system), the frozen argv's inline `-c commit.gpgsign=false` --
// command-line -c always wins over every config file -- lets the merge
// succeed without ever attempting to sign, in an environment with no GPG
// agent or key configured at all (a signing attempt here would fail loudly).
func TestSpikeMergeRepoLocalSigningConfigDoesNotBlock(t *testing.T) {
	artifacts := newArtifactDir(t)
	repo := newFixtureRepo(t, artifacts, "repo")

	// The diverging source commit is built BEFORE turning signing on: this
	// fixture's own commit path (repo.commit) is a plain `git commit` with no
	// -c override, so it would fail to sign too once commit.gpgsign=true is
	// set -- only the MERGE under test needs to survive that config.
	repo.writeFile(t, "source.txt", "source change\n", 0o644)
	source := repo.commit(t, "source change")
	mustRunGit(t, repo.Root, repo.gitHome, "config", "commit.gpgsign", "true")
	scratch := detachedScratch(t, repo, artifacts, "scratch-signing", repo.Base)

	out, err := runMerge(t, scratch, repo.gitHome, source)
	if err != nil {
		t.Fatalf("git merge --no-ff --no-edit %s failed with commit.gpgsign=true locally configured: %v\n%s", source, err, out)
	}
	mergeOID := strings.TrimSpace(mustRunGit(t, scratch, repo.gitHome, "rev-parse", "HEAD"))
	signature := mustRunGit(t, scratch, repo.gitHome, "log", "-1", "--format=%G?", mergeOID)
	if strings.TrimSpace(signature) != "N" {
		t.Errorf("merge commit %%G? = %q, want %q (N = no signature at all)", strings.TrimSpace(signature), "N")
	}
}

// TestSpikeMergeNonConflictFailureIsDistinctFromConflict is G2's non-conflict
// merge failure case: an UNWRITABLE object database (the shared
// repo.Root/.git/objects every linked worktree writes into) makes `git
// merge` fail on an I/O error while attempting to write objects -- a failure
// classified distinctly from a content conflict: no CONFLICT marker in the
// output and no unmerged (UU) index entries. Unlike that difference, `git
// merge --abort` DOES still succeed here (MERGE_HEAD is recorded before the
// tree-write step that actually fails), cleanly restoring the scratch
// checkout to its pre-merge HEAD -- so a controller cannot distinguish "a
// real conflict" from "an unwritable object database" by abortability
// alone; the CONFLICT marker/UU-entries check above is the real
// discriminator.
func TestSpikeMergeNonConflictFailureIsDistinctFromConflict(t *testing.T) {
	artifacts := newArtifactDir(t)
	repo := newFixtureRepo(t, artifacts, "repo")

	repo.writeFile(t, "source.txt", "source change\n", 0o644)
	source := repo.commit(t, "source change")
	scratch := detachedScratch(t, repo, artifacts, "scratch-unwritable", repo.Base)

	// A new loose object lands one level under objectsDir (objects/<xx>/<rest>);
	// writing into an EXISTING <xx> subdirectory needs write permission on
	// that subdirectory alone, not on objectsDir itself. Locking only
	// objectsDir therefore still allows the merge to succeed whenever its new
	// object hashes happen to collide with a two-hex prefix the fixture
	// commits already created -- lock every existing subdirectory too so the
	// write failure is deterministic regardless of hash prefix.
	objectsDir := filepath.Join(repo.Root, ".git", "objects")
	entries, err := os.ReadDir(objectsDir)
	if err != nil {
		t.Fatalf("read object database: %v", err)
	}
	lockPaths := []string{objectsDir}
	for _, e := range entries {
		if e.IsDir() {
			lockPaths = append(lockPaths, filepath.Join(objectsDir, e.Name()))
		}
	}
	for _, p := range lockPaths {
		if chmodErr := os.Chmod(p, 0o500); chmodErr != nil { //nolint:gosec // G302: deliberately read-only to force a git write failure; restored below.
			t.Fatalf("chmod object database read-only: %v", chmodErr)
		}
	}
	t.Cleanup(func() {
		for _, p := range lockPaths {
			if chmodErr := os.Chmod(p, 0o700); chmodErr != nil { //nolint:gosec // G302: restoring this test's own throwaway object database to owner rwx so cleanup can remove it; not a security-sensitive permission.
				t.Errorf("restore object database permissions: %v", chmodErr)
			}
		}
	})

	out, err := runMerge(t, scratch, repo.gitHome, source)
	if err == nil {
		t.Fatalf("git merge --no-ff --no-edit against an unwritable object database unexpectedly succeeded:\n%s", out)
	}
	t.Logf("expected non-conflict failure output:\n%s", out)
	if strings.Contains(out, "CONFLICT") {
		t.Errorf("an unwritable-object-database failure unexpectedly looks like a content conflict:\n%s", out)
	}
	status := mustRunGit(t, scratch, repo.gitHome, "status", "--porcelain")
	if strings.Contains(status, "UU ") {
		t.Errorf("git status --porcelain shows an unmerged (UU) entry for a non-conflict failure, want none:\n%s", status)
	}
	if out, err := runGit(t, scratch, repo.gitHome, "merge", "--abort"); err != nil {
		t.Errorf("git merge --abort failed after the non-conflict failure: %v\n%s", err, out)
	}
	if head := strings.TrimSpace(mustRunGit(t, scratch, repo.gitHome, "rev-parse", "HEAD")); head != repo.Base {
		t.Errorf("scratch checkout HEAD after merge --abort = %s, want it restored to %s", head, repo.Base)
	}
}

// TestSpikeCommitTreeIsDeterministic is G2's deterministic rollback
// construction case: `git commit-tree <tree> -p <parent> -m <message>`,
// given the SAME tree, parent, author/committer identity AND dates, and
// message, produces the IDENTICAL commit object id every time -- the
// property TestSpikeIntegrationRollbackPreservesRejectedMergeReachable's
// rollback commit depends on for a retried rollback to be a safe no-op
// (the same construction republished, never a second distinct object).
// Git commit objects hash their author/committer timestamps, so this only
// holds when those are pinned explicitly rather than left to wall-clock
// "now".
func TestSpikeCommitTreeIsDeterministic(t *testing.T) {
	artifacts := newArtifactDir(t)
	repo := newFixtureRepo(t, artifacts, "repo")

	tree := strings.TrimSpace(mustRunGit(t, repo.Root, repo.gitHome, "rev-parse", repo.Base+"^{tree}"))
	env := append(fixtureGitEnviron(repo.gitHome),
		"GIT_AUTHOR_NAME=hop-integration", "GIT_AUTHOR_EMAIL=hop-integration@example.invalid",
		"GIT_AUTHOR_DATE=2024-01-01T00:00:00+0000",
		"GIT_COMMITTER_NAME=hop-integration", "GIT_COMMITTER_EMAIL=hop-integration@example.invalid",
		"GIT_COMMITTER_DATE=2024-01-01T00:00:00+0000",
	)
	args := []string{"commit-tree", tree, "-p", repo.Base, "-m", "deterministic rollback"}

	first, err := runGitWithEnv(t, repo.Root, env, args...)
	if err != nil {
		t.Fatalf("first commit-tree: %v\n%s", err, first)
	}
	second, err := runGitWithEnv(t, repo.Root, env, args...)
	if err != nil {
		t.Fatalf("second commit-tree: %v\n%s", err, second)
	}
	firstOID, secondOID := strings.TrimSpace(first), strings.TrimSpace(second)
	if firstOID != secondOID {
		t.Errorf("commit-tree with identical tree/parent/identity/dates/message produced DIFFERENT oids: %s vs %s", firstOID, secondOID)
	} else {
		t.Logf("confirmed deterministic: both invocations produced %s", firstOID)
	}
}
