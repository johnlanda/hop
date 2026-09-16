package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/testsupport/hopfixtures"
)

// The real-binary green boundary of post-merge worktree retirement
// (docs/plan/phase-3-worktree-retirement.md section 9): a terminal feature
// run seeded through hopfixtures whose worktree rows point at real `git
// worktree add` checkouts of a real fixture repository, and the built hop
// binary's own `hop status` removing, keeping or releasing them — under
// isolatedHopEnv, whose Herdr canary fails the test if anything dials the
// server. Every path touched lives under this test's own temporary
// directories.

// retirementFixture is one seeded run and its repository.
type retirementFixture struct {
	t         *testing.T
	StateRoot string
	Repo      string
	Base      string
	RunID     string
	Checkouts []retirementCheckout
}

// retirementCheckout is one attempt's checkout: its requested branch and
// the path spelling the row records (through a symbolic link — git lists
// the canonical one).
type retirementCheckout struct {
	Branch string
	Path   string
}

// retirementSetup describes a fixture.
type retirementSetup struct {
	seed int
	// attempts is the number of attempt checkouts.
	attempts int
	// target is the frozen target branch ("" for a run frozen on a
	// detached HEAD).
	target string
	// merged fast-forwards main to the integration head.
	merged bool
	// parent, when set, holds the repository at parent/repo.
	parent string
}

// newRetirementFixture builds the repository, the attempt checkouts, the
// integration branch and the seeded terminal feature run.
func newRetirementFixture(t *testing.T, setup retirementSetup) *retirementFixture {
	t.Helper()
	parent := setup.parent
	if parent == "" {
		parent = realDir(t)
	}
	repo := filepath.Join(parent, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatalf("create fixture repository: %v", err)
	}
	runFixtureGit(t, repo, "init", "-q", "-b", "main")
	runFixtureGit(t, repo, "config", "commit.gpgsign", "false")
	writeFixtureFile(t, filepath.Join(repo, "README.md"), "hop fixture\n")
	writeFixtureFile(t, filepath.Join(repo, ".gitignore"), "build/\n")
	runFixtureGit(t, repo, "add", "README.md", ".gitignore")
	runFixtureGit(t, repo, "commit", "-q", "-m", "fixture base")
	base := runFixtureGit(t, repo, "rev-parse", "HEAD")

	// The checkouts are created and recorded through a symbolic link, so
	// the recorded spelling always differs from the canonical path git
	// lists (Herdr records the requested spelling; spike S9).
	checkoutParent := filepath.Join(realDir(t), "checkouts")
	if err := os.Symlink(realDir(t), checkoutParent); err != nil {
		t.Fatalf("link the checkout directory: %v", err)
	}
	f := &retirementFixture{t: t, Repo: repo, Base: base, StateRoot: freshStateDir(t)}
	var sources []string
	for i := 1; i <= setup.attempts; i++ {
		checkout := retirementCheckout{Branch: fmt.Sprintf("hop/r1/t%da1", i), Path: filepath.Join(checkoutParent, fmt.Sprintf("t%da1", i))}
		runFixtureGit(t, repo, "worktree", "add", "-q", "-b", checkout.Branch, checkout.Path, base)
		writeFixtureFile(t, filepath.Join(checkout.Path, fmt.Sprintf("work-%d.txt", i)), "attempt work\n")
		runFixtureGit(t, checkout.Path, "add", ".")
		runFixtureGit(t, checkout.Path, "commit", "-q", "-m", "attempt work")
		sources = append(sources, runFixtureGit(t, checkout.Path, "rev-parse", "HEAD"))
		f.Checkouts = append(f.Checkouts, checkout)
	}
	runFixtureGit(t, repo, "checkout", "-q", "-b", "hop/r1/integration")
	var heads [][2]string
	for _, checkout := range f.Checkouts {
		premerge := runFixtureGit(t, repo, "rev-parse", "HEAD")
		runFixtureGit(t, repo, "merge", "-q", "--no-ff", "-m", "integrate "+checkout.Branch, checkout.Branch)
		heads = append(heads, [2]string{premerge, runFixtureGit(t, repo, "rev-parse", "HEAD")})
	}
	runFixtureGit(t, repo, "checkout", "-q", "main")
	if setup.merged {
		runFixtureGit(t, repo, "merge", "-q", "--ff-only", "hop/r1/integration")
	}

	store := openFixtureStore(t, f.StateRoot)
	ctx := context.Background()
	now := time.Now().UTC()
	runBase, lease, err := hopfixtures.Initialize(ctx, store, f.StateRoot, repo, setup.seed, now)
	if err != nil {
		t.Fatalf("initialize run: %v", err)
	}
	f.RunID = runBase.RunID
	workflow := featureWorkflowSnapshot(f.RunID, defaultMessageWait)
	workflow.TargetBranch = setup.target
	workflow.BaseCommitOID = base
	freezeWorkflowSnapshot(t, f.StateRoot, f.RunID, workflow)
	managerID, _, err := hopfixtures.SeedManager(ctx, store, lease, f.RunID, setup.seed, now)
	if err != nil {
		t.Fatalf("seed manager: %v", err)
	}
	for i, checkout := range f.Checkouts {
		// Integrated rows are created a second apart: the latest one names
		// the validated head. The base run's own bootstrap task holds
		// sequence 1.
		at := now.Add(time.Duration(i) * time.Second)
		seed := setup.seed + 100*(i+1)
		_, attemptID, seedErr := hopfixtures.SeedIntegratedTask(ctx, store, lease, f.RunID, managerID, i+2, seed, sources[i], heads[i][0], heads[i][1], at)
		if seedErr != nil {
			t.Fatalf("seed integrated task %d: %v", i+1, seedErr)
		}
		if _, seedErr = hopfixtures.SeedAttemptWorktree(ctx, store, lease, f.RunID, attemptID, seed+50, repo, checkout.Path, checkout.Branch, base, at); seedErr != nil {
			t.Fatalf("seed attempt worktree %d: %v", i+1, seedErr)
		}
	}
	if err := hopfixtures.FinishRun(ctx, store, lease, f.RunID, runStateCompleted, now); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	return f
}

// writeFixtureFile writes one fixture file.
func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
}

// status runs the built hop status in dir with the fixture's state root.
func (f *retirementFixture) status(dir string, args ...string) hopResult {
	f.t.Helper()
	result := execHop(f.t, map[string]string{"HOP_STATE_DIR": f.StateRoot}, dir, append([]string{"status"}, args...)...)
	if result.ExitCode != exitOK {
		f.t.Fatalf("hop status exited %d:\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Stdout, result.Stderr)
	}
	return result
}

// journal reports the run's operation count and lease generation.
func (f *retirementFixture) journal() (operations, generation int) {
	f.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.StateRoot, "hop.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		f.t.Fatalf("open raw db: %v", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			f.t.Errorf("close raw db: %v", closeErr)
		}
	}()
	ctx := context.Background()
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM operations WHERE run_id = ?`, f.RunID).Scan(&operations); err != nil {
		f.t.Fatalf("count operations: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT generation FROM run_leases WHERE run_id = ?`, f.RunID).Scan(&generation); err != nil {
		f.t.Fatalf("read lease generation: %v", err)
	}
	return operations, generation
}

// exists reports whether path exists.
func (f *retirementFixture) exists(path string) bool {
	f.t.Helper()
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true
	case errors.Is(err, fs.ErrNotExist):
		return false
	default:
		f.t.Fatalf("stat %s: %v", path, err)
		return false
	}
}

// requireLines fails unless out carries every line, in order, as whole
// lines.
func requireLines(t *testing.T, out string, lines ...string) {
	t.Helper()
	rest := "\n" + out
	for _, line := range lines {
		i := strings.Index(rest, "\n"+line+"\n")
		if i < 0 {
			t.Fatalf("output lacks the line %q (in order):\n%s", line, out)
		}
		rest = rest[i+len(line)+1:]
	}
}

func removedLine(c retirementCheckout) string {
	return "r1 worktree " + c.Branch + " removed: " + c.Path + " (close its Herdr workspace if one is still open)"
}

// TestGrammarContractWorktreeRetirementMerged: the integration branch
// merged into main removes every clean checkout (ignored build output
// included) through the real binary, keeps the branches, sets the fact and
// prints section 6's lines; a second hop status does nothing at all.
func TestGrammarContractWorktreeRetirementMerged(t *testing.T) {
	f := newRetirementFixture(t, retirementSetup{seed: 21000, attempts: 2, target: "refs/heads/main", merged: true})
	writeFixtureFile(t, filepath.Join(f.Checkouts[1].Path, "build", "output.bin"), "ignored build output\n")
	// A repository-local fsmonitor hook would run on every index read and
	// removal hop makes; it must never run.
	hookDir := realDir(t)
	marker := filepath.Join(hookDir, "fsmonitor-ran")
	hook := filepath.Join(hookDir, "fsmonitor-hook.sh")
	writeFixtureFile(t, hook, "#!/bin/sh\necho ran >> '"+marker+"'\nexit 1\n")
	if err := os.Chmod(hook, 0o700); err != nil { //nolint:gosec // G302: the fixture's own hook must be executable.
		t.Fatal(err)
	}
	runFixtureGit(t, f.Repo, "config", "core.fsmonitor", hook)

	first := f.status(f.Repo)
	if f.exists(marker) {
		t.Fatal("hop status ran the repository's fsmonitor hook")
	}
	runFixtureGit(t, f.Repo, "config", "--unset", "core.fsmonitor")
	requireLines(t, first.Stdout,
		"retiring worktrees of r1…",
		"no runs (use -all to include finished runs)",
		"r1 worktrees retired: 2 removed, 0 already absent, 0 released (removal deletes ignored files such as build output)",
		removedLine(f.Checkouts[0]),
		removedLine(f.Checkouts[1]),
	)
	if first.Stderr != "" {
		t.Fatalf("stderr = %q", first.Stderr)
	}
	for _, checkout := range f.Checkouts {
		if f.exists(checkout.Path) {
			t.Fatalf("checkout %s still exists", checkout.Path)
		}
		if branch := runFixtureGit(t, f.Repo, "branch", "--list", checkout.Branch); branch == "" {
			t.Fatalf("branch %s was deleted", checkout.Branch)
		}
	}
	if listing := runFixtureGit(t, f.Repo, "worktree", "list", "--porcelain"); strings.Count(listing, "worktree ") != 1 {
		t.Fatalf("git still lists attempt worktrees:\n%s", listing)
	}
	if resolved, err := filepath.EvalSymlinks(filepath.Dir(f.Checkouts[0].Path)); err != nil || resolved == filepath.Dir(f.Checkouts[0].Path) {
		t.Fatalf("the recorded checkout spelling is canonical (%q, %v); the test must record a non-canonical one", resolved, err)
	}
	operations, generation := f.journal()

	detail := f.status(f.Repo, "-run", "r1")
	requireLines(t, detail.Stdout,
		"  target:        refs/heads/main",
		"  worktree:      "+f.Checkouts[0].Branch+" removed "+f.Checkouts[0].Path,
		"  worktree:      "+f.Checkouts[1].Branch+" removed "+f.Checkouts[1].Path,
	)
	if !strings.Contains(detail.Stdout, "  worktrees:     retired ") {
		t.Fatalf("the fact is not rendered:\n%s", detail.Stdout)
	}
	second := f.status(f.Repo)
	if second.Stdout != "no runs (use -all to include finished runs)\n" || second.Stderr != "" {
		t.Fatalf("a second hop status printed:\n%s%s", second.Stdout, second.Stderr)
	}
	if ops, gen := f.journal(); ops != operations || gen != generation {
		t.Fatalf("a later hop status journaled %d operations or took a lease (generation %d -> %d)", ops-operations, generation, gen)
	}
}

// TestGrammarContractWorktreeRetirementUnmerged: an unmerged run keeps
// every checkout; after its one check, repeated hop status calls take no
// lease and write nothing.
func TestGrammarContractWorktreeRetirementUnmerged(t *testing.T) {
	f := newRetirementFixture(t, retirementSetup{seed: 22000, attempts: 2, target: "refs/heads/main"})
	first := f.status(f.Repo)
	if first.Stdout != "no runs (use -all to include finished runs)\n" || first.Stderr != "" {
		t.Fatalf("an unmerged run printed:\n%s%s", first.Stdout, first.Stderr)
	}
	operations, generation := f.journal()
	for range 2 {
		if again := f.status(f.Repo); again.Stdout != first.Stdout || again.Stderr != "" {
			t.Fatalf("a repeated hop status printed:\n%s%s", again.Stdout, again.Stderr)
		}
	}
	if ops, gen := f.journal(); ops != operations || gen != generation {
		t.Fatalf("repeated hop status journaled %d operations or took a lease (generation %d -> %d)", ops-operations, generation, gen)
	}
	for _, checkout := range f.Checkouts {
		if !f.exists(checkout.Path) {
			t.Fatalf("an unmerged checkout %s was removed", checkout.Path)
		}
	}
	detail := f.status(f.Repo, "-run", "r1")
	requireLines(t, detail.Stdout,
		"  worktrees:     not retired (removed once the integration branch is merged into refs/heads/main; removal deletes ignored files such as build output; commit anything you want to keep)",
		"  worktree:      "+f.Checkouts[0].Branch+" active "+f.Checkouts[0].Path,
	)
}

// TestGrammarContractWorktreeRetirementKeepsChanges: a dirty checkout and
// one with a change hidden by assume-unchanged are retained with their
// actions while a clean sibling is removed; cleaning them lets the next
// hop status finish and set the fact.
func TestGrammarContractWorktreeRetirementKeepsChanges(t *testing.T) {
	f := newRetirementFixture(t, retirementSetup{seed: 23000, attempts: 3, target: "refs/heads/main", merged: true})
	dirty, hidden, clean := f.Checkouts[0], f.Checkouts[1], f.Checkouts[2]
	writeFixtureFile(t, filepath.Join(dirty.Path, "README.md"), "an uncommitted edit\n")
	runFixtureGit(t, hidden.Path, "update-index", "--assume-unchanged", "README.md")
	writeFixtureFile(t, filepath.Join(hidden.Path, "README.md"), "an edit git status hides\n")
	if status := runFixtureGit(t, hidden.Path, "status", "--porcelain"); status != "" {
		t.Fatalf("the hidden change is visible to git status: %q", status)
	}

	first := f.status(f.Repo)
	requireLines(t, first.Stdout,
		"r1 worktree "+dirty.Branch+" retained (uncommitted changes): "+dirty.Path,
		"  action: commit or discard the changes, then run hop status again",
		"r1 worktree "+hidden.Branch+" retained (hidden changes): "+hidden.Path,
		"  action: clear git update-index --no-assume-unchanged/--no-skip-worktree, then commit or discard, then run hop status again",
		removedLine(clean),
	)
	if strings.Contains(first.Stdout, "worktrees retired") {
		t.Fatalf("the fact was set with retained worktrees:\n%s", first.Stdout)
	}
	if !f.exists(dirty.Path) || !f.exists(hidden.Path) || f.exists(clean.Path) {
		t.Fatalf("checkouts after the first pass: dirty %v, hidden %v, clean %v", f.exists(dirty.Path), f.exists(hidden.Path), f.exists(clean.Path))
	}

	runFixtureGit(t, dirty.Path, "checkout", "--", "README.md")
	runFixtureGit(t, hidden.Path, "update-index", "--no-assume-unchanged", "README.md")
	runFixtureGit(t, hidden.Path, "checkout", "--", "README.md")
	second := f.status(f.Repo)
	requireLines(t, second.Stdout,
		"r1 worktrees retired: 3 removed, 0 already absent, 0 released (removal deletes ignored files such as build output)",
		removedLine(dirty),
		removedLine(hidden),
	)
	if f.exists(dirty.Path) || f.exists(hidden.Path) {
		t.Fatalf("cleaned checkouts were not removed")
	}
}

// TestGrammarContractWorktreeRetirementReleasesDetached: a checkout moved
// to a detached HEAD is released and left on disk; its sibling is removed
// and the run retires.
func TestGrammarContractWorktreeRetirementReleasesDetached(t *testing.T) {
	f := newRetirementFixture(t, retirementSetup{seed: 24000, attempts: 2, target: "refs/heads/main", merged: true})
	detached, sibling := f.Checkouts[0], f.Checkouts[1]
	runFixtureGit(t, detached.Path, "checkout", "-q", "--detach")

	first := f.status(f.Repo)
	requireLines(t, first.Stdout,
		"r1 worktrees retired: 1 removed, 0 already absent, 1 released (removal deletes ignored files such as build output)",
		"r1 worktree "+detached.Branch+" released (detached HEAD): "+detached.Path+"; HOP will not remove it",
		removedLine(sibling),
	)
	if !f.exists(detached.Path) || f.exists(sibling.Path) {
		t.Fatalf("detached kept %v, sibling removed %v", f.exists(detached.Path), !f.exists(sibling.Path))
	}
	detail := f.status(f.Repo, "-run", "r1")
	requireLines(t, detail.Stdout,
		"  worktree:      "+detached.Branch+" released (detached HEAD) "+detached.Path+"; left on disk; no longer managed by HOP",
	)
}

// TestGrammarContractWorktreeRetirementDetachedTarget: a run frozen on a
// detached HEAD never retires, even merged.
func TestGrammarContractWorktreeRetirementDetachedTarget(t *testing.T) {
	f := newRetirementFixture(t, retirementSetup{seed: 25000, attempts: 1, merged: true})
	operations, generation := f.journal()
	first := f.status(f.Repo)
	if first.Stdout != "no runs (use -all to include finished runs)\n" || first.Stderr != "" {
		t.Fatalf("a detached-target run printed:\n%s%s", first.Stdout, first.Stderr)
	}
	if ops, gen := f.journal(); ops != operations || gen != generation || !f.exists(f.Checkouts[0].Path) {
		t.Fatalf("a detached-target run was acted on")
	}
	requireLines(t, f.status(f.Repo, "-run", "r1").Stdout,
		"  target:        none (detached HEAD at freeze; worktrees are never retired automatically)",
		"  worktrees:     kept (no target branch)",
	)
}

// TestGrammarContractWorktreeRetirementMovedRepository: a merged run whose
// repository was moved away, with another directory now at its root, is
// skipped: no lease, no operation, nothing removed or released.
func TestGrammarContractWorktreeRetirementMovedRepository(t *testing.T) {
	for _, replacement := range []struct {
		name string
		make func(t *testing.T, root string)
	}{
		{"an empty directory", func(*testing.T, string) {}},
		{"a different git repository", func(t *testing.T, root string) {
			runFixtureGit(t, root, "init", "-q", "-b", "main")
		}},
	} {
		t.Run(replacement.name, func(t *testing.T) {
			parent := realDir(t)
			f := newRetirementFixture(t, retirementSetup{seed: 26000, attempts: 1, target: "refs/heads/main", merged: true, parent: parent})
			if err := os.Rename(f.Repo, filepath.Join(parent, "moved")); err != nil {
				t.Fatalf("move the repository: %v", err)
			}
			if err := os.Mkdir(f.Repo, 0o700); err != nil {
				t.Fatalf("create the replacement root: %v", err)
			}
			replacement.make(t, f.Repo)
			operations, generation := f.journal()

			result := f.status(f.Repo)
			requireLines(t, result.Stdout, "r1 worktree retirement skipped: the repository root no longer holds this run's history")
			if strings.Contains(result.Stdout, "retiring") || strings.Contains(result.Stdout, "removed") || result.Stderr != "" {
				t.Fatalf("a moved repository was acted on:\n%s%s", result.Stdout, result.Stderr)
			}
			if ops, gen := f.journal(); ops != operations || gen != generation {
				t.Fatalf("a moved repository journaled %d operations or took a lease (generation %d -> %d)", ops-operations, generation, gen)
			}
			if !f.exists(f.Checkouts[0].Path) {
				t.Fatalf("a checkout of the moved repository was removed")
			}
		})
	}
}
