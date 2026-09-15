package integration

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSpikeIntegrationRefCompareAndSwap and
// TestSpikeDetachedMergeLeavesRefUntouched are PROVISIONAL git-level probes,
// requested by the Phase 3 design reviewer ahead of formal S-numbering, for
// the fenced-publish assumptions a revised integration design rests on: a
// compare-and-swap ref update for publishing a new integration head, and a
// side, detached merge that never touches that ref until explicitly
// published. Herdr itself has no ref-fencing surface (worktree.create only
// creates a checkout; merges and ref updates are plain `git` invocations
// HOP's own CommandRunner would issue against a checkout path, per design
// section 4's "Merges and resets run `git -C` with the absolute
// Controller.GitExecutable"), so both probes drive real `git` directly
// against a worktree.create'd checkout, exactly as production code would.
//
// The run-scoped ref name ("refs/heads/hop/r1/integration" here) is this
// probe's own stand-in for whatever ref shape the revised design ultimately
// adopts -- recorded as an open question in .bin/FINDINGS.md pending the
// design planner's own probe spec.

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

// runGit runs one git subcommand against dir, in the same hermetic git
// environment fixtureRepo itself uses (fixtureGitEnviron, no developer git
// configuration), returning its combined output and any error WITHOUT
// failing the test -- used where a call's failure is itself the assertion
// (a refused compare-and-swap update-ref).
func runGit(t *testing.T, dir, gitHome string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: git is resolved from PATH; args are fixed by this suite's own fixture construction.
	cmd.Env = fixtureGitEnviron(gitHome)
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
