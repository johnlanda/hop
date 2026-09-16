package process_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/process"
	"github.com/johnlanda/hop/internal/app"
)

// The worktree-retirement git probe (docs/plan/phase-3-worktree-retirement.md):
// every exit status, output shape and on-disk effect the retirement decision
// rules consume, executed under the production transport — process.Runner,
// an absolute git executable and the retirement git environment — against
// throwaway repositories under t.TempDir. Nothing outside the test's own
// temporary directory is read or written.

// retirementGitEnv is the complete environment every retirement git
// invocation runs with: inherited global and system configuration
// suppressed, terminal prompts off, nothing else (the runner inherits
// nothing).
func retirementGitEnv() []string {
	return []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0"}
}

// worktreeProbe is one throwaway repository plus the absolute git it runs.
type worktreeProbe struct {
	t    *testing.T
	git  string
	root string // symlink-resolved temporary root
	repo string // root/repo, the main checkout
	base string // the repository's second commit
}

func newWorktreeProbe(t *testing.T) *worktreeProbe {
	t.Helper()
	found, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git probe skipped, not run: no git executable on PATH (%v)", err)
	}
	git, err := filepath.Abs(found)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &worktreeProbe{t: t, git: git, root: root, repo: filepath.Join(root, "repo")}
	p.must(root, "init", "-q", "--object-format=sha1", "-b", "main", "repo")
	p.commitAll("empty base", true)
	p.writeFile(filepath.Join(p.repo, ".gitignore"), "ignored.txt\nbuild/\n")
	p.writeFile(filepath.Join(p.repo, "f.txt"), "f\n")
	p.writeFile(filepath.Join(p.repo, "g.txt"), "g\n")
	p.base = p.commitAll("tracked files", false)
	return p
}

// run executes `git -C dir args...` through the production runner.
func (p *worktreeProbe) run(dir string, args ...string) app.CommandResult {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := process.Runner{}.Run(ctx, app.Command{
		Argv: append([]string{p.git, "-C", dir}, args...),
		Env:  retirementGitEnv(),
	})
	if err != nil {
		p.t.Fatalf("git %s: runner error: %v", strings.Join(args, " "), err)
	}
	return result
}

// must runs git and fails the test on a non-zero exit, returning trimmed
// stdout.
func (p *worktreeProbe) must(dir string, args ...string) string {
	p.t.Helper()
	result := p.run(dir, args...)
	if result.ExitCode != 0 {
		p.t.Fatalf("git %s: exit %d: %s", strings.Join(args, " "), result.ExitCode, result.Stderr)
	}
	return strings.TrimSpace(string(result.Stdout))
}

func (p *worktreeProbe) commitAll(message string, allowEmpty bool) string {
	p.t.Helper()
	p.must(p.repo, "add", "-A")
	args := []string{"-c", "user.name=hop-probe", "-c", "user.email=probe@invalid", "commit", "-q", "-m", message}
	if allowEmpty {
		args = append(args, "--allow-empty")
	}
	p.must(p.repo, args...)
	return p.must(p.repo, "rev-parse", "HEAD^{commit}")
}

func (p *worktreeProbe) writeFile(path, content string) {
	p.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		p.t.Fatal(err)
	}
}

// addWorktree creates a linked checkout on a new attempt-shaped branch at
// the probe's base commit and returns its canonical path.
func (p *worktreeProbe) addWorktree(name string) (path, branch string) {
	p.t.Helper()
	path = filepath.Join(p.root, "wt-"+name)
	branch = "hop/r1/" + name
	p.must(p.repo, "worktree", "add", "-q", "-b", branch, path, p.base)
	return path, branch
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true
	case os.IsNotExist(err):
		return false
	default:
		t.Fatalf("lstat: %v", err)
		return false
	}
}

// worktreeRecord is one `git worktree list --porcelain -z` record: its
// NUL-terminated attribute lines in order.
type worktreeRecord []string

// parseWorktreeListZ splits `git worktree list --porcelain -z` output: each
// attribute is NUL-terminated and each record ends with one extra NUL.
func parseWorktreeListZ(t *testing.T, out []byte) []worktreeRecord {
	t.Helper()
	if len(out) == 0 {
		return nil
	}
	if !bytes.HasSuffix(out, []byte{0, 0}) {
		t.Fatalf("worktree list -z output does not end with a record terminator: %q", out)
	}
	var records []worktreeRecord
	for raw := range bytes.SplitSeq(bytes.TrimSuffix(out, []byte{0, 0}), []byte{0, 0}) {
		records = append(records, strings.Split(string(raw), "\x00"))
	}
	return records
}

func (p *worktreeProbe) listRecord(path string) (worktreeRecord, bool) {
	p.t.Helper()
	result := p.run(p.repo, "worktree", "list", "--porcelain", "-z")
	if result.ExitCode != 0 {
		p.t.Fatalf("worktree list: exit %d: %s", result.ExitCode, result.Stderr)
	}
	for _, record := range parseWorktreeListZ(p.t, result.Stdout) {
		if record[0] == "worktree "+path {
			return record, true
		}
	}
	return nil, false
}

// TestGitProbeWorktreeRemoveOutcomes pins `git worktree remove <path>`
// WITHOUT any force option across every checkout condition the retirement
// rules classify: the exit status, the exact refusal text, whether the
// checkout and its administrative entry survive, and the pre-check
// observations (`status --porcelain=v1 --untracked-files=all` and
// `ls-files -v`) the rules read beforehand. The hazard rows are the reason
// the pre-check and the `-c status.showUntrackedFiles=all` override exist:
// git's own cleanliness check removes them.
func TestGitProbeWorktreeRemoveOutcomes(t *testing.T) {
	const dirtyRefusal = "contains modified or untracked files, use --force to delete it"
	cases := []struct {
		name string
		// setup dirties the checkout; it runs after the worktree exists.
		setup func(p *worktreeProbe, wt string)
		// removeOverride are `-c` pairs placed before the subcommand.
		removeOverride []string
		wantStatus     string // exact trimmed porcelain output of the pre-check
		wantFlags      string // exact trimmed `ls-files -v` output, "" to skip
		wantExit       int
		// wantStderr is the exact stderr with %s replaced by the checkout path;
		// "" means empty.
		wantStderr  string
		wantRemoved bool
	}{
		{
			name:        "clean",
			setup:       func(*worktreeProbe, string) {},
			wantFlags:   "H .gitignore\nH f.txt\nH g.txt",
			wantRemoved: true,
		},
		{
			name:       "untracked",
			setup:      func(p *worktreeProbe, wt string) { p.writeFile(filepath.Join(wt, "new.txt"), "x\n") },
			wantStatus: "?? new.txt",
			wantExit:   128, wantStderr: "fatal: '%s' " + dirtyRefusal + "\n",
		},
		{
			name:       "modified",
			setup:      func(p *worktreeProbe, wt string) { p.writeFile(filepath.Join(wt, "f.txt"), "changed\n") },
			wantStatus: "M f.txt",
			wantExit:   128, wantStderr: "fatal: '%s' " + dirtyRefusal + "\n",
		},
		{
			name: "staged",
			setup: func(p *worktreeProbe, wt string) {
				p.writeFile(filepath.Join(wt, "s.txt"), "s\n")
				p.must(wt, "add", "s.txt")
			},
			wantStatus: "A  s.txt",
			wantExit:   128, wantStderr: "fatal: '%s' " + dirtyRefusal + "\n",
		},
		{
			name:  "nested untracked repository",
			setup: func(p *worktreeProbe, wt string) { p.must(wt, "init", "-q", "nested") },
			// The nested repository is reported as one untracked directory.
			wantStatus: "?? nested/",
			wantExit:   128, wantStderr: "fatal: '%s' " + dirtyRefusal + "\n",
		},
		{
			name:     "locked",
			setup:    func(p *worktreeProbe, wt string) { p.must(p.repo, "worktree", "lock", wt) },
			wantExit: 128, wantStderr: "fatal: cannot remove a locked working tree;\nuse 'remove -f -f' to override or unlock first\n",
		},
		{
			name:     "locked with a reason",
			setup:    func(p *worktreeProbe, wt string) { p.must(p.repo, "worktree", "lock", "--reason", "in use", wt) },
			wantExit: 128, wantStderr: "fatal: cannot remove a locked working tree, lock reason: in use\nuse 'remove -f -f' to override or unlock first\n",
		},
		{
			name:        "empty directory only",
			setup:       func(p *worktreeProbe, wt string) { mkdirAll(p.t, filepath.Join(wt, "emptydir")) },
			wantRemoved: true,
		},
		// Hazard: ignored files are not uncommitted changes to git; the
		// checkout is removed and they are deleted with it.
		{
			name:        "HAZARD ignored file",
			setup:       func(p *worktreeProbe, wt string) { p.writeFile(filepath.Join(wt, "ignored.txt"), "i\n") },
			wantRemoved: true,
		},
		{
			name:        "HAZARD ignored directory",
			setup:       func(p *worktreeProbe, wt string) { p.writeFile(filepath.Join(wt, "build", "out"), "o\n") },
			wantRemoved: true,
		},
		// Hazard: a repository-local status.showUntrackedFiles=no hides the
		// untracked file from git's own check, and the remove deletes it; the
		// pre-check's explicit --untracked-files=all still sees it.
		{
			name: "HAZARD untracked hidden by status.showUntrackedFiles=no",
			setup: func(p *worktreeProbe, wt string) {
				p.must(p.repo, "config", "status.showUntrackedFiles", "no")
				p.writeFile(filepath.Join(wt, "u.txt"), "u\n")
			},
			wantStatus:  "?? u.txt",
			wantRemoved: true,
		},
		{
			name: "untracked hidden by config, remove with -c status.showUntrackedFiles=all",
			setup: func(p *worktreeProbe, wt string) {
				p.must(p.repo, "config", "status.showUntrackedFiles", "no")
				p.writeFile(filepath.Join(wt, "u.txt"), "u\n")
			},
			removeOverride: []string{"-c", "status.showUntrackedFiles=all"},
			wantStatus:     "?? u.txt",
			wantExit:       128, wantStderr: "fatal: '%s' " + dirtyRefusal + "\n",
		},
		// Hazard: index flags hide a modification from status entirely; the
		// only observation is the `ls-files -v` tag (lowercase for
		// assume-unchanged, S for skip-worktree).
		{
			name: "HAZARD modification hidden by assume-unchanged",
			setup: func(p *worktreeProbe, wt string) {
				p.must(wt, "update-index", "--assume-unchanged", "f.txt")
				p.writeFile(filepath.Join(wt, "f.txt"), "changed\n")
			},
			wantFlags:   "H .gitignore\nh f.txt\nH g.txt",
			wantRemoved: true,
		},
		{
			name: "HAZARD modification hidden by skip-worktree",
			setup: func(p *worktreeProbe, wt string) {
				p.must(wt, "update-index", "--skip-worktree", "g.txt")
				p.writeFile(filepath.Join(wt, "g.txt"), "changed\n")
			},
			wantFlags:   "H .gitignore\nH f.txt\nS g.txt",
			wantRemoved: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newWorktreeProbe(t)
			wt, branch := p.addWorktree("t1a1")
			tc.setup(p, wt)

			status := p.run(wt, "status", "--porcelain=v1", "--untracked-files=all")
			if status.ExitCode != 0 {
				t.Fatalf("pre-check status: exit %d: %s", status.ExitCode, status.Stderr)
			}
			if got := strings.TrimSpace(string(status.Stdout)); got != strings.TrimSpace(tc.wantStatus) {
				t.Errorf("pre-check status = %q, want %q", got, tc.wantStatus)
			}
			if tc.wantFlags != "" {
				if got := p.must(wt, "ls-files", "-v"); got != tc.wantFlags {
					t.Errorf("ls-files -v = %q, want %q", got, tc.wantFlags)
				}
			}

			args := append(append([]string{}, tc.removeOverride...), "worktree", "remove", wt)
			result := p.run(p.repo, args...)
			if result.ExitCode != tc.wantExit {
				t.Errorf("worktree remove exit = %d, want %d (stderr %q)", result.ExitCode, tc.wantExit, result.Stderr)
			}
			wantStderr := ""
			if tc.wantStderr != "" {
				wantStderr = strings.ReplaceAll(tc.wantStderr, "%s", wt)
			}
			if got := string(result.Stderr); got != wantStderr {
				t.Errorf("worktree remove stderr = %q, want %q", got, wantStderr)
			}
			if len(result.Stdout) != 0 {
				t.Errorf("worktree remove stdout = %q, want empty", result.Stdout)
			}
			_, listed := p.listRecord(wt)
			if present := exists(t, wt); present == tc.wantRemoved {
				t.Errorf("checkout present after remove = %v, want %v", present, !tc.wantRemoved)
			}
			if listed == tc.wantRemoved {
				t.Errorf("checkout listed after remove = %v, want %v", listed, !tc.wantRemoved)
			}
			// The branch is never deleted by a worktree removal.
			if got := p.must(p.repo, "rev-parse", "--verify", "-q", "refs/heads/"+branch+"^{commit}"); got != p.base {
				t.Errorf("branch %s after remove = %q, want %s", branch, got, p.base)
			}
		})
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestGitProbeWorktreeRemoveMissingAndSymlinkedPaths pins the two path
// shapes that are not a present, canonically spelled checkout: a checkout
// whose directory is already gone (listed as prunable) is removed from the
// listing with exit 0, and a symlinked spelling of a present checkout's path
// removes it; the listing always reports the symlink-resolved path.
func TestGitProbeWorktreeRemoveMissingAndSymlinkedPaths(t *testing.T) {
	t.Run("missing directory", func(t *testing.T) {
		p := newWorktreeProbe(t)
		wt, _ := p.addWorktree("t1a1")
		if err := os.RemoveAll(wt); err != nil {
			t.Fatal(err)
		}
		record, listed := p.listRecord(wt)
		if !listed {
			t.Fatal("a checkout whose directory was deleted is no longer listed; want it listed as prunable")
		}
		if want := "prunable gitdir file points to non-existent location"; record[len(record)-1] != want {
			t.Errorf("listing attribute = %q, want %q (record %q)", record[len(record)-1], want, record)
		}
		status := p.run(wt, "status", "--porcelain=v1", "--untracked-files=all")
		if status.ExitCode != 128 || !strings.HasPrefix(string(status.Stderr), "fatal: cannot change to '"+wt+"'") {
			t.Errorf("status in a missing checkout: exit %d stderr %q, want exit 128 and a cannot-change-to error", status.ExitCode, status.Stderr)
		}
		result := p.run(p.repo, "worktree", "remove", wt)
		if result.ExitCode != 0 || len(result.Stderr) != 0 {
			t.Errorf("remove of a missing checkout: exit %d stderr %q, want exit 0 and no output", result.ExitCode, result.Stderr)
		}
		if _, listed := p.listRecord(wt); listed {
			t.Error("the missing checkout is still listed after remove")
		}
	})
	t.Run("symlinked spelling", func(t *testing.T) {
		p := newWorktreeProbe(t)
		link := filepath.Join(p.root, "link")
		if err := os.Symlink(p.root, link); err != nil {
			t.Fatal(err)
		}
		spelled := filepath.Join(link, "wt-sym")
		p.must(p.repo, "worktree", "add", "-q", "-b", "hop/r1/sym", spelled, p.base)
		canonical := filepath.Join(p.root, "wt-sym")
		if _, listed := p.listRecord(canonical); !listed {
			t.Fatalf("the listing does not report the symlink-resolved path %s", canonical)
		}
		if _, listed := p.listRecord(spelled); listed {
			t.Errorf("the listing reports the symlinked spelling %s", spelled)
		}
		result := p.run(p.repo, "worktree", "remove", spelled)
		if result.ExitCode != 0 {
			t.Errorf("remove by symlinked spelling: exit %d stderr %q", result.ExitCode, result.Stderr)
		}
		if exists(t, canonical) {
			t.Error("the checkout survived removal by its symlinked spelling")
		}
	})
}

// TestGitProbeWorktreeListPorcelainZ pins the `worktree list --porcelain -z`
// record shapes the retirement provenance check parses: the main checkout
// first, then per linked checkout `worktree <canonical path>`, `HEAD <oid>`,
// then `branch <full ref>` or `detached`, then optional `locked[ <reason>]`
// and `prunable <reason>` attributes.
func TestGitProbeWorktreeListPorcelainZ(t *testing.T) {
	p := newWorktreeProbe(t)
	onBranch, onBranchRef := p.addWorktree("t1a1")
	locked, lockedRef := p.addWorktree("t2a1")
	p.must(p.repo, "worktree", "lock", "--reason", "in use", locked)
	lockedBare, lockedBareRef := p.addWorktree("t3a1")
	p.must(p.repo, "worktree", "lock", lockedBare)
	detached := filepath.Join(p.root, "wt-detached")
	p.must(p.repo, "worktree", "add", "-q", "--detach", detached, p.base)
	missing, missingRef := p.addWorktree("t4a1")
	if err := os.RemoveAll(missing); err != nil {
		t.Fatal(err)
	}

	result := p.run(p.repo, "worktree", "list", "--porcelain", "-z")
	if result.ExitCode != 0 {
		t.Fatalf("worktree list: exit %d: %s", result.ExitCode, result.Stderr)
	}
	records := parseWorktreeListZ(t, result.Stdout)
	mainHead := p.must(p.repo, "rev-parse", "HEAD^{commit}")
	if len(records) == 0 || strings.Join(records[0], "|") != strings.Join([]string{"worktree " + p.repo, "HEAD " + mainHead, "branch refs/heads/main"}, "|") {
		t.Fatalf("first record = %q, want the main checkout on refs/heads/main", records)
	}
	want := map[string]worktreeRecord{
		onBranch:   {"worktree " + onBranch, "HEAD " + p.base, "branch refs/heads/" + onBranchRef},
		locked:     {"worktree " + locked, "HEAD " + p.base, "branch refs/heads/" + lockedRef, "locked in use"},
		lockedBare: {"worktree " + lockedBare, "HEAD " + p.base, "branch refs/heads/" + lockedBareRef, "locked"},
		detached:   {"worktree " + detached, "HEAD " + p.base, "detached"},
		missing:    {"worktree " + missing, "HEAD " + p.base, "branch refs/heads/" + missingRef, "prunable gitdir file points to non-existent location"},
	}
	if len(records) != len(want)+1 {
		t.Errorf("record count = %d, want %d: %q", len(records), len(want)+1, records)
	}
	for _, record := range records[1:] {
		path := strings.TrimPrefix(record[0], "worktree ")
		expected, ok := want[path]
		if !ok {
			t.Errorf("unexpected record %q", record)
			continue
		}
		if strings.Join(record, "|") != strings.Join(expected, "|") {
			t.Errorf("record for %s = %q, want %q", path, record, expected)
		}
	}

	// A linked checkout resolves the repository's own common directory, the
	// provenance equality the retirement rules reuse.
	common := p.must(onBranch, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if want := filepath.Join(p.repo, ".git"); common != want {
		t.Errorf("linked checkout common dir = %q, want %q", common, want)
	}
	if got := p.must(onBranch, "symbolic-ref", "-q", "HEAD"); got != "refs/heads/"+onBranchRef {
		t.Errorf("linked checkout symbolic-ref = %q, want refs/heads/%s", got, onBranchRef)
	}
}

// TestGitProbeTargetAndAncestry pins the freeze-time target capture
// (`symbolic-ref -q HEAD`), target resolution (`rev-parse --verify -q
// <ref>^{commit}`) and the detection predicate (`merge-base --is-ancestor
// <integration head> <target oid>`): exit 0 is contained (including a
// commit with itself), 1 is not contained, and 128 is an unknown object.
func TestGitProbeTargetAndAncestry(t *testing.T) {
	p := newWorktreeProbe(t)
	first := p.must(p.repo, "rev-parse", "HEAD~1")

	sym := p.run(p.repo, "symbolic-ref", "-q", "HEAD")
	if sym.ExitCode != 0 || string(sym.Stdout) != "refs/heads/main\n" || len(sym.Stderr) != 0 {
		t.Errorf("symbolic-ref on a branch: exit %d stdout %q stderr %q, want 0 and refs/heads/main", sym.ExitCode, sym.Stdout, sym.Stderr)
	}
	verify := p.run(p.repo, "rev-parse", "--verify", "-q", "refs/heads/main^{commit}")
	if verify.ExitCode != 0 || string(verify.Stdout) != p.base+"\n" {
		t.Errorf("rev-parse --verify main: exit %d stdout %q, want 0 and %s", verify.ExitCode, verify.Stdout, p.base)
	}
	missing := p.run(p.repo, "rev-parse", "--verify", "-q", "refs/heads/no-such-branch^{commit}")
	if missing.ExitCode != 1 || len(missing.Stdout) != 0 || len(missing.Stderr) != 0 {
		t.Errorf("rev-parse --verify of a missing branch: exit %d stdout %q stderr %q, want 1 and no output", missing.ExitCode, missing.Stdout, missing.Stderr)
	}

	ancestry := []struct {
		name, ancestor, descendant string
		wantExit                   int
		wantStderr                 string
	}{
		{"older commit is contained", first, p.base, 0, ""},
		{"a commit contains itself", p.base, p.base, 0, ""},
		{"newer commit is not contained", p.base, first, 1, ""},
		{"unknown object", "0123456789012345678901234567890123456789", p.base, 128, "fatal: Not a valid commit name 0123456789012345678901234567890123456789\n"},
	}
	for _, tc := range ancestry {
		result := p.run(p.repo, "merge-base", "--is-ancestor", tc.ancestor, tc.descendant)
		if result.ExitCode != tc.wantExit || string(result.Stderr) != tc.wantStderr || len(result.Stdout) != 0 {
			t.Errorf("%s: exit %d stdout %q stderr %q, want exit %d stderr %q", tc.name, result.ExitCode, result.Stdout, result.Stderr, tc.wantExit, tc.wantStderr)
		}
	}

	p.must(p.repo, "checkout", "-q", "--detach")
	detached := p.run(p.repo, "symbolic-ref", "-q", "HEAD")
	if detached.ExitCode != 1 || len(detached.Stdout) != 0 || len(detached.Stderr) != 0 {
		t.Errorf("symbolic-ref on a detached HEAD: exit %d stdout %q stderr %q, want 1 and no output", detached.ExitCode, detached.Stdout, detached.Stderr)
	}
}
