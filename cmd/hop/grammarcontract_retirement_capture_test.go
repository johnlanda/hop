package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Worktree retirement against git output larger than the process runner's
// default capture bound, through the built hop binary. The index scan and
// the worktree listing are read with a larger bound of their own, so such
// output is decided on in full; past that bound, a kept prefix ending
// exactly on a record boundary looks complete, and only the runner's
// truncation report keeps hop from deciding on it. Every path touched
// lives under the test's own temporary directories.

const (
	// retirementCaptureBytes is the process runner's default per-stream
	// capture bound (internal/adapters/process).
	retirementCaptureBytes = 1 << 20
	// retirementListingBytes is the capture bound of retirement's index
	// scan and worktree listing (internal/app).
	retirementListingBytes = 64 << 20
)

// fixtureGitBytes runs one git command in dir under the isolated fixture
// environment and returns its exact stdout.
func fixtureGitBytes(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: a fixed git invocation against this test's own throwaway repository.
	cmd.Dir = dir
	cmd.Env = isolatedTestEnv(t, "", fixtureGitVars())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.Bytes())
	}
	return out
}

const (
	// indexFillerStem is the fixed part of every filler path; with the tag,
	// its separator and the NUL, an entry is indexFillerStemBytes plus its
	// leaf's length.
	indexFillerStemBytes = len("H d/00000/") + 201 + 201 + len("\x00")
	indexFillerLeafMax   = 200
)

// writeIndexFiller writes clean files under dir/d whose `ls-files -v -z`
// entries — once added — total exactly n bytes, every one sorting before
// any path that starts with a letter after d. Paths stay far below the
// platform path limit.
func writeIndexFiller(t *testing.T, dir string, n int) {
	t.Helper()
	most, least := indexFillerStemBytes+indexFillerLeafMax, indexFillerStemBytes+1
	entries := (n + most - 1) / most
	if entries*least > n {
		t.Fatalf("%d bytes cannot be split into filler entries", n)
	}
	deficit := entries*most - n
	a, b := strings.Repeat("a", 200), strings.Repeat("b", 200)
	for i := range entries {
		size := most - min(deficit, most-least)
		deficit -= most - size
		path := filepath.Join(dir, "d", fmt.Sprintf("%05d", i), a, b, strings.Repeat("c", size-indexFillerStemBytes))
		writeFixtureFile(t, path, "filler\n")
	}
}

// TestGrammarContractWorktreeRetirementLargeIndex: a merged run's checkout
// whose `ls-files -v -z` output outgrows the default capture bound, with a
// modification hidden by assume-unchanged or skip-worktree in the entry
// right at that bound (or past it), is read in full and retained for its
// hidden change; the checkout and the change survive and the run is not
// retired. A read cut at the default bound would have removed it.
func TestGrammarContractWorktreeRetirementLargeIndex(t *testing.T) {
	cases := []struct {
		name string
		flag string
		tag  string
		// overshoot is how far the entries before the hidden one run past
		// the default bound: 0 puts the hidden entry exactly where a
		// default-bounded read would end.
		overshoot int
	}{
		{"assume-unchanged right at the default bound", "--assume-unchanged", "h", 0},
		{"skip-worktree right at the default bound", "--skip-worktree", "S", 0},
		{"assume-unchanged past the default bound", "--assume-unchanged", "h", 100},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetirementFixture(t, retirementSetup{seed: 34000 + 1000*i, attempts: 1, target: "refs/heads/main", merged: true})
			checkout := f.Checkouts[0]
			const hidden = "zz-hidden.txt"
			writeFixtureFile(t, filepath.Join(checkout.Path, hidden), "committed\n")
			runFixtureGit(t, checkout.Path, "add", hidden)
			runFixtureGit(t, checkout.Path, "commit", "-q", "-m", "the file a flag will hide")
			before := len(fixtureGitBytes(t, checkout.Path, "ls-files", "-v", "-z")) - len("H "+hidden+"\x00")
			writeIndexFiller(t, checkout.Path, retirementCaptureBytes+tc.overshoot-before)
			runFixtureGit(t, checkout.Path, "add", "d")
			runFixtureGit(t, checkout.Path, "commit", "-q", "-m", "a large index")
			runFixtureGit(t, checkout.Path, "update-index", tc.flag, hidden)
			const edit = "an uncommitted human edit\n"
			writeFixtureFile(t, filepath.Join(checkout.Path, hidden), edit)

			tags := fixtureGitBytes(t, checkout.Path, "ls-files", "-v", "-z")
			entry := "\x00" + tc.tag + " " + hidden + "\x00"
			if at := bytes.Index(tags, []byte(entry)) + 1; at != retirementCaptureBytes+tc.overshoot || !bytes.HasSuffix(tags, []byte(entry)) {
				t.Fatalf("the hidden entry starts at byte %d of %d, want %d and last", at, len(tags), retirementCaptureBytes+tc.overshoot)
			}
			if status := runFixtureGit(t, checkout.Path, "status", "--porcelain"); status != "" {
				t.Fatalf("the fixture checkout is not clean to git status: %q", status)
			}

			out := f.status(f.Repo)
			requireLines(t, out.Stdout,
				"r1 worktree "+checkout.Branch+" retained (hidden changes): "+checkout.Path,
				"  action: clear git update-index --no-assume-unchanged/--no-skip-worktree, then commit or discard, then run hop status again",
			)
			if strings.Contains(out.Stdout, "removed") || strings.Contains(out.Stdout, "worktrees retired") || out.Stderr != "" {
				t.Fatalf("hop status acted on a checkout with a hidden change:\n%s%s", out.Stdout, out.Stderr)
			}
			if got, err := os.ReadFile(filepath.Join(checkout.Path, hidden)); err != nil || string(got) != edit {
				t.Fatalf("DATA LOSS: the hidden change is %q (%v)", got, err)
			}
			if listing := runFixtureGit(t, f.Repo, "worktree", "list", "--porcelain"); strings.Count(listing, "worktree ") != 2 {
				t.Fatalf("git no longer lists the checkout:\n%s", listing)
			}
		})
	}
}

// lockSiblingToFill adds a detached sibling checkout that git's
// path-ordered listing names before the fixture's checkout, locked with a
// reason sized so the listing holds exactly n bytes before the checkout's
// record. It returns the sibling and its lock file.
func lockSiblingToFill(t *testing.T, f *retirementFixture, n int) (sibling, lockFile string) {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(f.Checkouts[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	sibling = filepath.Join(filepath.Dir(canonical), "a-sibling")
	runFixtureGit(t, f.Repo, "worktree", "add", "-q", "--detach", sibling)
	record := []byte("\x00\x00worktree " + canonical + "\x00")
	offset := bytes.Index(fixtureGitBytes(t, f.Repo, "worktree", "list", "--porcelain", "-z"), record) + 2
	if offset < 2 {
		t.Fatal("git does not list the checkout after the sibling")
	}
	// git prints the reason from the sibling's administrative lock file as
	// one `locked <reason>` attribute, inserted before the sibling's record
	// terminator.
	lockFile = filepath.Join(runFixtureGit(t, sibling, "rev-parse", "--absolute-git-dir"), "locked")
	writeFixtureFile(t, lockFile, strings.Repeat("r", n-offset-len("locked \x00")))
	if at := bytes.Index(fixtureGitBytes(t, f.Repo, "worktree", "list", "--porcelain", "-z"), record) + 2; at != n {
		t.Fatalf("the checkout's record starts at byte %d, want %d", at, n)
	}
	return sibling, lockFile
}

// TestGrammarContractWorktreeRetirementLargeListing: a repository whose
// `worktree list --porcelain -z` output reaches a capture bound exactly at
// the record boundary before a merged run's checkout — a sibling checkout,
// locked with a long reason, fills it. At the default bound the listing is
// still read in full and the checkout is removed; at the listing's own
// bound the checkout is retained as not fully inspected — never released
// as unregistered, never recorded absent — and the run is not retired.
// The sibling is never touched.
func TestGrammarContractWorktreeRetirementLargeListing(t *testing.T) {
	t.Run("past the default bound: read in full, the checkout removed", func(t *testing.T) {
		f := newRetirementFixture(t, retirementSetup{seed: 38000, attempts: 1, target: "refs/heads/main", merged: true})
		sibling, lockFile := lockSiblingToFill(t, f, retirementCaptureBytes)
		reason, err := os.ReadFile(lockFile) //nolint:gosec // G304: the sibling checkout's lock file under this test's own fixture repository.
		if err != nil {
			t.Fatal(err)
		}

		out := f.status(f.Repo)
		requireLines(t, out.Stdout,
			"r1 worktrees retired: 1 removed, 0 already absent, 0 released (removal deletes ignored files such as build output)",
			removedLine(f.Checkouts[0]),
		)
		if f.exists(f.Checkouts[0].Path) || !f.exists(sibling) {
			t.Fatalf("checkout present %v, sibling present %v; want the checkout removed and the sibling kept", f.exists(f.Checkouts[0].Path), f.exists(sibling))
		}
		if got, err := os.ReadFile(lockFile); err != nil || !bytes.Equal(got, reason) { //nolint:gosec // G304: as above.
			t.Fatalf("the sibling's lock changed (%v)", err)
		}
	})

	t.Run("cut at the listing's own bound: retained", func(t *testing.T) {
		f := newRetirementFixture(t, retirementSetup{seed: 39000, attempts: 1, target: "refs/heads/main", merged: true})
		checkout := f.Checkouts[0]
		sibling, _ := lockSiblingToFill(t, f, retirementListingBytes)

		out := f.status(f.Repo)
		requireLines(t, out.Stdout,
			"r1 worktree "+checkout.Branch+" retained (inspection failed): "+checkout.Path,
			"  action: the checkout could not be fully inspected; check it and its repository, then run hop status again",
		)
		for _, forbidden := range []string{"released", "already absent", "removed", "worktrees retired"} {
			if strings.Contains(out.Stdout, forbidden) {
				t.Fatalf("hop status decided on a cut worktree listing (%q):\n%s", forbidden, out.Stdout)
			}
		}
		if !f.exists(checkout.Path) || !f.exists(sibling) {
			t.Fatalf("checkout present %v, sibling present %v; want both kept", f.exists(checkout.Path), f.exists(sibling))
		}
		requireLines(t, f.status(f.Repo, "-run", "r1").Stdout, "  worktree:      "+checkout.Branch+" active "+checkout.Path)
	})
}
