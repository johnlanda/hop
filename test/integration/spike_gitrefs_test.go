package integration

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file's git-level probes (G1, G3) establish the fenced-publish
// assumptions the integration design rests on. Herdr itself has no
// ref-fencing surface (worktree.create only creates a checkout; merges and
// ref updates are plain `git` invocations HOP's own CommandRunner would
// issue against a checkout path, per design section 4's "Merges and resets
// run `git -C` with the absolute Controller.GitExecutable"), so every probe
// here drives real `git` directly against a worktree.create'd checkout,
// exactly as production code would.
//
// The run-scoped ref name is `refs/heads/hop/r<seq>/integration` (r1 in
// every probe below): it disambiguates the integration ref from the
// `hop/r<seq>/t<t>a<n>` attempt branches sharing its `hop/r<seq>/` prefix.
// TestSpikeBareRunBranchCollidesWithAttemptBranches proves why that
// disambiguation is required -- a bare `hop/r<seq>` branch name cannot
// coexist with any `hop/r<seq>/t<t>a<n>` attempt branch at all.

// TestSpikeIntegrationRefCompareAndSwap proves `git update-ref <ref> <new>
// <old>` is a true compare-and-swap when run against a worktree.create'd
// checkout: it succeeds and moves the ref when <old> still matches the ref's
// actual current value, and it FAILS -- leaving the ref exactly where it
// was, not partially updated -- when <old> is stale.
func TestSpikeIntegrationRefCompareAndSwap(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)
	repo := newFixtureRepo(t, artifacts, "repo")

	const integrationRef = "refs/heads/hop/r1/integration"
	mustRunGit(t, repo.Root, repo.gitHome, "update-ref", integrationRef, repo.Base)

	attemptPath := filepath.Join(artifacts.dir(t, "worktrees"), "attempt")
	var created worktreeCreatedResponse
	server.call(t, "worktree.create", map[string]any{
		"cwd": repo.Root, "branch": "hop/r1/t1a1", "base": repo.Base, "path": attemptPath,
	}, &created)

	// The worker's own worktree produces the candidate commit; the
	// controller CAS-publishes it onto the integration ref FROM that same
	// checkout -- exactly the access pattern a revised design would use.
	mustRunGit(t, attemptPath, repo.gitHome, "commit", "--allow-empty", "-m", "candidate 1")
	candidate := strings.TrimSpace(mustRunGit(t, attemptPath, repo.gitHome, "rev-parse", "HEAD"))

	if out, err := runGit(t, attemptPath, repo.gitHome, "update-ref", integrationRef, candidate, repo.Base); err != nil {
		t.Fatalf("update-ref with the CURRENT old value %s failed: %v\n%s", repo.Base, err, out)
	}
	if got := readRef(t, attemptPath, repo.gitHome, integrationRef); got != candidate {
		t.Fatalf("integration ref = %s after a successful CAS update, want %s", got, candidate)
	}

	// A second candidate, proposed with the NOW-STALE old value (repo.Base):
	// the update must be refused, and the ref must remain exactly at the
	// first candidate -- no silent overwrite, no partial state.
	mustRunGit(t, attemptPath, repo.gitHome, "commit", "--allow-empty", "-m", "candidate 2 (would race)")
	rogue := strings.TrimSpace(mustRunGit(t, attemptPath, repo.gitHome, "rev-parse", "HEAD"))
	out, err := runGit(t, attemptPath, repo.gitHome, "update-ref", integrationRef, rogue, repo.Base)
	if err == nil {
		t.Fatalf("update-ref with a STALE old value %s unexpectedly succeeded:\n%s", repo.Base, out)
	}
	if got := readRef(t, attemptPath, repo.gitHome, integrationRef); got != candidate {
		t.Errorf("integration ref = %s after a refused stale-old-value update, want it unchanged at %s", got, candidate)
	}
}

// TestSpikeDetachedMergeLeavesRefUntouched proves `git merge --no-ff` run in
// a DETACHED scratch checkout (never the integration branch's own checkout)
// computes a candidate merge commit as a side effect -- a new, otherwise
// unreferenced object -- without moving refs/heads/hop/r1/integration at
// all. Publishing that merge commit onto the ref is a separate, explicit
// step (TestSpikeIntegrationRefCompareAndSwap's update-ref), never an
// automatic consequence of computing it.
func TestSpikeDetachedMergeLeavesRefUntouched(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)
	repo := newFixtureRepo(t, artifacts, "repo")

	const integrationRef = "refs/heads/hop/r1/integration"
	mustRunGit(t, repo.Root, repo.gitHome, "update-ref", integrationRef, repo.Base)

	// The merge SOURCE: a real per-attempt checkout (worktree.create'd),
	// diverging from base with its own commit.
	attemptPath := filepath.Join(artifacts.dir(t, "worktrees"), "attempt")
	var created worktreeCreatedResponse
	server.call(t, "worktree.create", map[string]any{
		"cwd": repo.Root, "branch": "hop/r1/t1a1", "base": repo.Base, "path": attemptPath,
	}, &created)
	mustRunGit(t, attemptPath, repo.gitHome, "commit", "--allow-empty", "-m", "attempt change")
	sourceOID := strings.TrimSpace(mustRunGit(t, attemptPath, repo.gitHome, "rev-parse", "HEAD"))

	// The merge itself runs in a SEPARATE, plain detached checkout of the
	// base commit -- deliberately not on the integration branch -- so
	// refs/heads/hop/r1/integration cannot be touched by construction.
	scratchPath := artifacts.dir(t, "integration-scratch")
	mustRunGit(t, repo.Root, repo.gitHome, "worktree", "add", "--detach", scratchPath, repo.Base)
	t.Cleanup(func() {
		if out, err := runGit(t, repo.Root, repo.gitHome, "worktree", "remove", "--force", scratchPath); err != nil {
			t.Logf("remove detached scratch checkout: %v\n%s", err, out)
		}
	})

	mustRunGit(t, scratchPath, repo.gitHome, "merge", "--no-ff", "-m", "candidate merge", sourceOID)
	mergeOID := strings.TrimSpace(mustRunGit(t, scratchPath, repo.gitHome, "rev-parse", "HEAD"))
	if mergeOID == repo.Base || mergeOID == sourceOID {
		t.Fatal("the detached merge did not produce a new commit distinct from base/source")
	}

	if got := readRef(t, scratchPath, repo.gitHome, integrationRef); got != repo.Base {
		t.Errorf("refs/heads/hop/r1/integration = %s after a detached-checkout merge, want it untouched at %s", got, repo.Base)
	}
	// The scratch checkout's own (detached) HEAD moved to the merge commit;
	// no branch ref tracks it until explicitly published.
	if head := strings.TrimSpace(mustRunGit(t, scratchPath, repo.gitHome, "rev-parse", "HEAD")); head != mergeOID {
		t.Errorf("scratch checkout HEAD = %s, want the merge commit %s", head, mergeOID)
	}
}

// TestSpikeRunScopedRefFamilyCoexists is G1: every ref a Phase 3 run creates
// -- the integration ref plus one attempt branch per role, including the
// review task's (no separate review-branch scheme: it gets an ordinary
// `hop/r<seq>/t<t>a<n>` name like any implement task) -- coexists in one
// repository. None is a path-prefix of another (see
// TestSpikeBareRunBranchCollidesWithAttemptBranches for the case where one
// is), so git's ref storage never has to treat the same path both as a leaf
// ref and as a directory.
func TestSpikeRunScopedRefFamilyCoexists(t *testing.T) {
	artifacts := newArtifactDir(t)
	repo := newFixtureRepo(t, artifacts, "repo")

	family := []string{
		"refs/heads/hop/r1/integration",
		"refs/heads/hop/r1/t1a1", // implement task 1, attempt 1
		"refs/heads/hop/r1/t2a1", // implement task 2, attempt 1
		"refs/heads/hop/r1/t3a1", // the REVIEW task's branch -- same naming, no special case
	}
	for _, ref := range family {
		mustRunGit(t, repo.Root, repo.gitHome, "update-ref", ref, repo.Base)
	}
	for _, ref := range family {
		if got := readRef(t, repo.Root, repo.gitHome, ref); got != repo.Base {
			t.Errorf("ref %s = %s after the whole family was created, want %s", ref, got, repo.Base)
		}
	}
	listed := mustRunGit(t, repo.Root, repo.gitHome, "for-each-ref", "--format=%(refname)", "refs/heads/hop/r1")
	for _, ref := range family {
		if !strings.Contains(listed, ref) {
			t.Errorf("git for-each-ref does not list %s among the coexisting family:\n%s", ref, listed)
		}
	}
}

// TestSpikeBareRunBranchCollidesWithAttemptBranches is the NEGATIVE control
// proving why the run-scoped ref needs its own "/integration" leaf: git's
// ref storage is a filesystem-like path namespace, so a ref named exactly
// "hop/r1" and a ref named "hop/r1/t1a1" cannot coexist -- the first
// requires "hop/r1" to be a FILE, the second requires it to be a DIRECTORY.
// This is exactly the collision `hop/r<seq>/integration` (G1) avoids.
func TestSpikeBareRunBranchCollidesWithAttemptBranches(t *testing.T) {
	artifacts := newArtifactDir(t)
	repo := newFixtureRepo(t, artifacts, "repo")

	mustRunGit(t, repo.Root, repo.gitHome, "update-ref", "refs/heads/hop/r1", repo.Base)
	out, err := runGit(t, repo.Root, repo.gitHome, "update-ref", "refs/heads/hop/r1/t1a1", repo.Base)
	if err == nil {
		t.Fatalf("creating hop/r1/t1a1 alongside a bare hop/r1 branch unexpectedly succeeded (expected a path-collision refusal):\n%s", out)
	}
	t.Logf("expected path collision confirmed: %v\n%s", err, out)
}

// TestSpikeUpdateRefCreateOnlySemantics is G1/G3's create-only case: `git
// update-ref <ref> <new> ""` (an EMPTY expected-old value) means "the ref
// must not currently exist." It succeeds for a ref that was never created
// or checked out before, and is refused -- leaving the ref exactly at its
// existing value -- once that ref exists.
func TestSpikeUpdateRefCreateOnlySemantics(t *testing.T) {
	artifacts := newArtifactDir(t)
	repo := newFixtureRepo(t, artifacts, "repo")

	const ref = "refs/heads/hop/r1/integration"
	if out, err := runGit(t, repo.Root, repo.gitHome, "update-ref", ref, repo.Base, ""); err != nil {
		t.Fatalf("create-only update-ref on a ref that was never checked out failed: %v\n%s", err, out)
	}
	if got := readRef(t, repo.Root, repo.gitHome, ref); got != repo.Base {
		t.Fatalf("ref %s = %s after create-only creation, want %s", ref, got, repo.Base)
	}

	repo.writeFile(t, "second.txt", "second\n", 0o644)
	second := repo.commit(t, "second commit")
	out, err := runGit(t, repo.Root, repo.gitHome, "update-ref", ref, second, "")
	if err == nil {
		t.Fatalf("create-only update-ref on an ALREADY-EXISTING ref unexpectedly succeeded:\n%s", out)
	}
	if got := readRef(t, repo.Root, repo.gitHome, ref); got != repo.Base {
		t.Errorf("ref %s = %s after a refused create-only update, want it unchanged at %s", ref, got, repo.Base)
	}
}

// TestSpikeIntegrationRollbackPreservesRejectedMergeReachable is G3's
// rollback construction: when a published merge commit is rejected (the
// combined check failed against it), the ref is rolled back with a NEW
// commit built by `git commit-tree <premerge-tree> -p <rejected-merge>`,
// CAS-published in turn -- never a destructive `git reset --hard` +
// `git branch -f`. The rollback commit's content matches the pre-merge
// state, but the rejected merge stays REACHABLE as its parent: `git log`
// against the ref after rollback still shows it, so nothing here needs to
// race garbage collection to remain inspectable evidence.
func TestSpikeIntegrationRollbackPreservesRejectedMergeReachable(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)
	repo := newFixtureRepo(t, artifacts, "repo")

	const ref = "refs/heads/hop/r1/integration"
	mustRunGit(t, repo.Root, repo.gitHome, "update-ref", ref, repo.Base, "")
	preMergeTree := strings.TrimSpace(mustRunGit(t, repo.Root, repo.gitHome, "rev-parse", repo.Base+"^{tree}"))

	attemptPath := filepath.Join(artifacts.dir(t, "worktrees"), "attempt")
	var created worktreeCreatedResponse
	server.call(t, "worktree.create", map[string]any{
		"cwd": repo.Root, "branch": "hop/r1/t1a1", "base": repo.Base, "path": attemptPath,
	}, &created)
	mustRunGit(t, attemptPath, repo.gitHome, "commit", "--allow-empty", "-m", "candidate")
	sourceOID := strings.TrimSpace(mustRunGit(t, attemptPath, repo.gitHome, "rev-parse", "HEAD"))

	// The merge is computed detached (never touching the ref, per
	// TestSpikeDetachedMergeLeavesRefUntouched), then CAS-published as if the
	// combined check were about to run against it.
	scratchPath := artifacts.dir(t, "integration-scratch")
	mustRunGit(t, repo.Root, repo.gitHome, "worktree", "add", "--detach", scratchPath, repo.Base)
	t.Cleanup(func() {
		if out, err := runGit(t, repo.Root, repo.gitHome, "worktree", "remove", "--force", scratchPath); err != nil {
			t.Logf("remove detached scratch checkout: %v\n%s", err, out)
		}
	})
	mustRunGit(t, scratchPath, repo.gitHome, "merge", "--no-ff", "-m", "candidate merge", sourceOID)
	mergeOID := strings.TrimSpace(mustRunGit(t, scratchPath, repo.gitHome, "rev-parse", "HEAD"))
	mustRunGit(t, scratchPath, repo.gitHome, "update-ref", ref, mergeOID, repo.Base)

	// The combined check against mergeOID is now imagined to have failed:
	// construct the rollback commit (pre-merge tree, parent = the rejected
	// merge) and CAS-publish it in place of the merge.
	rollback := strings.TrimSpace(mustRunGit(t, scratchPath, repo.gitHome,
		"commit-tree", preMergeTree, "-p", mergeOID, "-m", "rollback: combined check failed"))
	mustRunGit(t, scratchPath, repo.gitHome, "update-ref", ref, rollback, mergeOID)

	if got := readRef(t, scratchPath, repo.gitHome, ref); got != rollback {
		t.Fatalf("ref %s = %s after the rollback publish, want the rollback commit %s", ref, got, rollback)
	}
	if got := strings.TrimSpace(mustRunGit(t, scratchPath, repo.gitHome, "rev-parse", ref+"^{tree}")); got != preMergeTree {
		t.Errorf("rollback commit tree = %s, want the pre-merge tree %s (content must revert)", got, preMergeTree)
	}
	if _, err := runGit(t, scratchPath, repo.gitHome, "merge-base", "--is-ancestor", mergeOID, ref); err != nil {
		t.Errorf("the rejected merge %s is NOT an ancestor of the rollback ref %s; it is unreachable, not preserved as evidence", mergeOID, ref)
	}
	history := mustRunGit(t, scratchPath, repo.gitHome, "log", "--format=%H", ref)
	if !strings.Contains(history, mergeOID) {
		t.Errorf("git log on the rolled-back ref does not include the rejected merge %s:\n%s", mergeOID, history)
	}
}

// runGit runs one git subcommand against dir, in the same hermetic git
// environment fixtureRepo itself uses (fixtureGitEnviron, no developer git
// configuration), returning its combined output and any error WITHOUT
// failing the test -- used where a call's failure is itself the assertion
// (a refused compare-and-swap update-ref).
func runGit(t *testing.T, dir, gitHome string, args ...string) (string, error) {
	t.Helper()
	return runGitWithEnv(t, dir, fixtureGitEnviron(gitHome), args...)
}

// runGitWithEnv is runGit against an explicit environment, for probes (G2's
// merge matrix) that need extra variables (GIT_TERMINAL_PROMPT=0) beyond
// fixtureGitEnviron's hermetic base.
func runGitWithEnv(t *testing.T, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: git is resolved from PATH; args are fixed by this suite's own fixture construction.
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// mustRunGit is runGit but fails the test on any error, for setup and
// assertion steps whose success is a precondition rather than the thing
// under test.
func mustRunGit(t *testing.T, dir, gitHome string, args ...string) string {
	t.Helper()
	out, err := runGit(t, dir, gitHome, args...)
	if err != nil {
		t.Fatalf("git -C %s %v: %v\n%s", dir, args, err, out)
	}
	return out
}

// readRef resolves a fully-qualified ref to its current object id via `git
// rev-parse`, failing the test if it cannot be resolved.
func readRef(t *testing.T, dir, gitHome, ref string) string {
	t.Helper()
	return strings.TrimSpace(mustRunGit(t, dir, gitHome, "rev-parse", ref))
}
