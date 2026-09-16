package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Worktree retirement of checkouts whose recorded spelling only the
// filesystem can resolve, through the built hop binary: `..` after a
// symbolic link, and a link on the way that no longer resolves. Every path
// touched lives under the test's own temporary directories.

// respellRecordedPath records spelling as the first checkout's path, in
// its worktree row and in its worktree.create operation's outcome alike,
// so provenance still agrees.
func (f *retirementFixture) respellRecordedPath(spelling string) {
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
	branch := f.Checkouts[0].Branch
	if _, err := db.ExecContext(ctx, `UPDATE worktrees SET path = ? WHERE run_id = ? AND branch = ?`, spelling, f.RunID, branch); err != nil {
		f.t.Fatalf("respell the worktree row: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE operations SET act_evidence = json_set(act_evidence, '$.info.Path', ?)
		WHERE run_id = ? AND kind = 'worktree.create' AND json_extract(act_evidence, '$.info.Branch') = ?`, spelling, f.RunID, branch); err != nil {
		f.t.Fatalf("respell the worktree.create outcome: %v", err)
	}
	f.Checkouts[0].Path = spelling
}

// symlinkedParentSpelling makes a fresh directory holding link, a symbolic
// link to a sibling directory "sub" beside the first checkout, and returns
// <dir>/link/../<checkout>: the filesystem resolves it to the checkout
// itself, while collapsing the spelling first names <dir>/<checkout>,
// which does not exist.
func (f *retirementFixture) symlinkedParentSpelling() (spelling, canonical string) {
	f.t.Helper()
	canonical, err := filepath.EvalSymlinks(f.Checkouts[0].Path)
	if err != nil {
		f.t.Fatal(err)
	}
	sub := filepath.Join(filepath.Dir(canonical), "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		f.t.Fatal(err)
	}
	alias := realDir(f.t)
	if err := os.Symlink(sub, filepath.Join(alias, "link")); err != nil {
		f.t.Fatal(err)
	}
	spelling = alias + "/link/../" + filepath.Base(canonical)
	if resolved, err := filepath.EvalSymlinks(spelling); err != nil || resolved != canonical {
		f.t.Fatalf("the spelling resolves to %q (%v), want the checkout", resolved, err)
	}
	if f.exists(filepath.Join(alias, filepath.Base(canonical))) {
		f.t.Fatal("the collapsed spelling names an existing path")
	}
	return spelling, canonical
}

// TestGrammarContractWorktreeRetirementSymlinkParentSpelling: a checkout
// recorded as <dir>/link/../<checkout> is the checkout the filesystem
// resolves that spelling to. Existing and clean, it is removed; deleted
// while git still lists it, git's entry is pruned and the row settles
// absent. Neither is mistaken for an unlisted path.
func TestGrammarContractWorktreeRetirementSymlinkParentSpelling(t *testing.T) {
	t.Run("an existing checkout is removed", func(t *testing.T) {
		f := newRetirementFixture(t, retirementSetup{seed: 40000, attempts: 1, target: "refs/heads/main", merged: true})
		spelling, canonical := f.symlinkedParentSpelling()
		f.respellRecordedPath(spelling)

		out := f.status(f.Repo)
		requireLines(t, out.Stdout,
			"r1 worktrees retired: 1 removed, 0 already absent, 0 released (removal deletes ignored files such as build output)",
			removedLine(f.Checkouts[0]),
		)
		if f.exists(canonical) {
			t.Fatalf("the checkout was not removed:\n%s", out.Stdout)
		}
		if listing := runFixtureGit(t, f.Repo, "worktree", "list", "--porcelain"); strings.Count(listing, "worktree ") != 1 {
			t.Fatalf("git still lists the checkout:\n%s", listing)
		}
	})

	t.Run("a deleted checkout git still lists is pruned and settles absent", func(t *testing.T) {
		f := newRetirementFixture(t, retirementSetup{seed: 41000, attempts: 1, target: "refs/heads/main", merged: true})
		spelling, canonical := f.symlinkedParentSpelling()
		f.respellRecordedPath(spelling)
		if err := os.RemoveAll(canonical); err != nil {
			t.Fatal(err)
		}
		if listing := runFixtureGit(t, f.Repo, "worktree", "list", "--porcelain"); !strings.Contains(listing, "worktree "+canonical+"\n") {
			t.Fatalf("git no longer lists the deleted checkout:\n%s", listing)
		}

		out := f.status(f.Repo)
		requireLines(t, out.Stdout,
			"r1 worktrees retired: 0 removed, 1 already absent, 0 released (removal deletes ignored files such as build output)",
			"r1 worktree "+f.Checkouts[0].Branch+" already absent: "+spelling,
		)
		if listing := runFixtureGit(t, f.Repo, "worktree", "list", "--porcelain"); strings.Count(listing, "worktree ") != 1 {
			t.Fatalf("git still lists the deleted checkout; its entry was never pruned:\n%s", listing)
		}
	})
}

// TestGrammarContractWorktreeRetirementDanglingLinkSpelling: a checkout
// recorded through a symbolic link that no longer resolves (an unmounted
// volume, say) cannot be located, so it is retained as not fully
// inspected — never recorded absent — and the checkout stays.
func TestGrammarContractWorktreeRetirementDanglingLinkSpelling(t *testing.T) {
	f := newRetirementFixture(t, retirementSetup{seed: 42000, attempts: 1, target: "refs/heads/main", merged: true})
	canonical, err := filepath.EvalSymlinks(f.Checkouts[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	alias := realDir(t)
	link := filepath.Join(alias, "volume")
	if err := os.Symlink(filepath.Dir(canonical), link); err != nil {
		t.Fatal(err)
	}
	spelling := filepath.Join(link, filepath.Base(canonical))
	f.respellRecordedPath(spelling)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(alias, "unmounted", "volume"), link); err != nil {
		t.Fatal(err)
	}

	out := f.status(f.Repo)
	requireLines(t, out.Stdout,
		"r1 worktree "+f.Checkouts[0].Branch+" retained (inspection failed): "+spelling,
		"  action: the checkout could not be fully inspected; check it and its repository, then run hop status again",
	)
	for _, forbidden := range []string{"already absent", "released", "removed", "worktrees retired"} {
		if strings.Contains(out.Stdout, forbidden) {
			t.Fatalf("hop status decided on an unresolvable spelling (%q):\n%s", forbidden, out.Stdout)
		}
	}
	if strings.Contains(out.Stderr, alias) {
		t.Fatalf("stderr echoes the path:\n%s", out.Stderr)
	}
	if !f.exists(canonical) {
		t.Fatal("the checkout was removed")
	}
	requireLines(t, f.status(f.Repo, "-run", "r1").Stdout, "  worktree:      "+f.Checkouts[0].Branch+" active "+spelling)
}
