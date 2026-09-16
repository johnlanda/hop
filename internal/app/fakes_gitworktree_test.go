package app_test

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

// The fake git's linked attempt worktrees: the checkout conditions worktree
// retirement observes and removes, reproducing exactly what the executed
// process-adapter probe pinned (internal/adapters/process/
// gitworktree_probe_test.go) — the refusals, the texts, and the three
// hazards where a removal without force still deletes data. A force option
// against an attempt worktree is recorded as a violation that fails the
// test (requireNoForcedAttemptRemovals); retirement never forces.

// fakeAttemptWorktree is one linked attempt checkout's state.
type fakeAttemptWorktree struct {
	// Branch is the full checked-out ref, "" for a detached HEAD.
	Branch string
	Head   string
	// CommonDir is the git common directory the checkout answers with; ""
	// means the fake repository's own.
	CommonDir string
	// Present is whether the directory exists. A listed checkout that is
	// not present is prunable.
	Present    bool
	Locked     bool
	LockReason string
	// Listed is false for a directory git no longer lists.
	Unlisted bool

	Untracked       bool // `?? new.txt`
	Modified        bool // ` M f.txt`
	Staged          bool // `A  s.txt`
	NestedRepo      bool // `?? nested/`
	Ignored         bool // no status line; deleted by a removal
	AssumeUnchanged bool // `h f.txt`; the change is invisible to status
	SkipWorktree    bool // `S g.txt`; the change is invisible to status
	// HasSubmodule is an initialized submodule `sub`: git refuses to
	// remove the checkout at all; DirtySubmodule adds ` M sub` to status.
	HasSubmodule   bool
	DirtySubmodule bool
}

// fakeAttemptWorktrees is the fake repository's attempt-worktree state.
type fakeAttemptWorktrees struct {
	// root and commonDir name the repository the model answers for.
	root      string
	commonDir string
	// hideUntracked models a repository-local
	// status.showUntrackedFiles=no.
	hideUntracked bool
	// fsmonitorHook models a repository-local core.fsmonitor hook: every
	// index read and removal without `-c core.fsmonitor=false` runs it.
	fsmonitorHook bool
	byPath        map[string]*fakeAttemptWorktree

	// RemoveCalls records every attempt-worktree removal argv tail.
	RemoveCalls []string
	// ForcedRemovals records every attempt-worktree removal that carried a
	// force option.
	ForcedRemovals []string
	// DeletedHiddenData records every removal that deleted data git's own
	// check did not see (ignored files, untracked files hidden by config,
	// index-flag-hidden modifications).
	DeletedHiddenData []string
	// FsmonitorRuns records every invocation that ran the repository's
	// fsmonitor hook.
	FsmonitorRuns []string
}

const (
	fakeGitMainHead = "commit-main"
	// fakeDirtyRefusal, fakeLockedRefusal and fakeNotWorkingTree are the
	// probe-pinned stderr texts, %s the checkout path.
	fakeDirtyRefusal     = "fatal: '%s' contains modified or untracked files, use --force to delete it\n"
	fakeLockedRefusal    = "fatal: cannot remove a locked working tree;\nuse 'remove -f -f' to override or unlock first\n"
	fakeLockedReason     = "fatal: cannot remove a locked working tree, lock reason: %s\nuse 'remove -f -f' to override or unlock first\n"
	fakeNotWorkingTree   = "fatal: '%s' is not a working tree\n"
	fakeCannotChangeTo   = "fatal: cannot change to '%s': No such file or directory\n"
	fakeSubmoduleRefusal = "fatal: working trees containing submodules cannot be moved or removed\n"
	// fakeFsmonitorOff is the override that keeps the hook from running.
	fakeFsmonitorOff = "core.fsmonitor=false"
)

// setAttemptRepository names the repository root and common directory the
// attempt-worktree model answers for.
func (g *fakeGitRepo) setAttemptRepository(root, commonDir string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.attempt.root = root
	g.attempt.commonDir = commonDir
}

// addAttemptWorktree registers a linked attempt checkout at path.
func (g *fakeGitRepo) addAttemptWorktree(path string, w fakeAttemptWorktree) { //nolint:gocritic // hugeParam: test seeding takes the state by value so a caller's literal is copied, never aliased.
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.attempt.byPath == nil {
		g.attempt.byPath = map[string]*fakeAttemptWorktree{}
	}
	g.attempt.byPath[path] = &w
}

// setHideUntracked sets the repository-local status.showUntrackedFiles=no
// condition.
func (g *fakeGitRepo) setHideUntracked(hide bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.attempt.hideUntracked = hide
}

// setFsmonitorHook sets the repository-local core.fsmonitor hook
// condition.
func (g *fakeGitRepo) setFsmonitorHook(hook bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.attempt.fsmonitorHook = hook
}

// fsmonitorRuns returns a copy of the invocations that ran the hook.
func (g *fakeGitRepo) fsmonitorRuns() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.attempt.FsmonitorRuns)
}

// requireNoFsmonitorRuns fails t when any invocation ran the repository's
// fsmonitor hook.
func (g *fakeGitRepo) requireNoFsmonitorRuns(t *testing.T) {
	t.Helper()
	if runs := g.fsmonitorRuns(); len(runs) != 0 {
		t.Errorf("git invocations ran the repository's fsmonitor hook: %v", runs)
	}
}

// indexReadLocked records a hook run for an index read or removal made
// without the override. Callers hold g.mu.
func (g *fakeGitRepo) indexReadLocked(configs []string, invocation string) {
	if g.attempt.fsmonitorHook && !slices.Contains(configs, fakeFsmonitorOff) {
		g.attempt.FsmonitorRuns = append(g.attempt.FsmonitorRuns, invocation)
	}
}

// attemptWorktree returns a copy of the checkout at path.
func (g *fakeGitRepo) attemptWorktree(path string) (fakeAttemptWorktree, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w, ok := g.attempt.byPath[path]
	if !ok {
		return fakeAttemptWorktree{}, false
	}
	return *w, true
}

// attemptLog returns copies of the recorded removal evidence.
func (g *fakeGitRepo) attemptLog() (removes, forced, hiddenDeleted []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.attempt.RemoveCalls), slices.Clone(g.attempt.ForcedRemovals), slices.Clone(g.attempt.DeletedHiddenData)
}

// requireNoForcedAttemptRemovals fails t when any attempt-worktree removal
// carried a force option.
func (g *fakeGitRepo) requireNoForcedAttemptRemovals(t *testing.T) {
	t.Helper()
	if _, forced, _ := g.attemptLog(); len(forced) != 0 {
		t.Errorf("attempt worktrees were removed with a force option: %v", forced)
	}
}

// runAttemptWorktreeGitLocked answers the invocations the model owns;
// handled is false for everything else. Callers hold g.mu.
func (g *fakeGitRepo) runAttemptWorktreeGitLocked(dir string, configs []string, sub string, rest []string) (result app.CommandResult, handled bool) {
	m := &g.attempt
	if m.root == "" {
		return app.CommandResult{}, false
	}
	w, isAttempt := m.byPath[dir]
	switch {
	case sub == "worktree" && dir == m.root && slices.Equal(rest, []string{"list", "--porcelain", "-z"}):
		return g.attemptListLocked(), true
	case sub == "worktree" && dir == m.root && len(rest) >= 2 && rest[0] == "remove":
		target := rest[len(rest)-1]
		if _, ok := m.byPath[target]; !ok {
			return app.CommandResult{}, false
		}
		return g.attemptRemoveLocked(configs, rest[1:len(rest)-1], target), true
	case sub == "cat-file" && len(rest) == 2 && rest[0] == "-e":
		return g.catFileExistsLocked(dir, rest[1]), true
	case !isAttempt:
		return app.CommandResult{}, false
	case !w.Present:
		return app.CommandResult{ExitCode: 128, Stderr: fmt.Appendf(nil, fakeCannotChangeTo, dir)}, true
	case sub == "status" && slices.Equal(rest, []string{"--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none"}):
		g.indexReadLocked(configs, "status "+dir)
		return app.CommandResult{Stdout: []byte(attemptStatusLines(w, true))}, true
	case sub == "ls-files" && slices.Equal(rest, []string{"-v", "-z"}):
		g.indexReadLocked(configs, "ls-files "+dir)
		return app.CommandResult{Stdout: []byte(attemptIndexTags(w))}, true
	case sub == "rev-parse" && slices.Contains(rest, "--git-common-dir"):
		common := w.CommonDir
		if common == "" {
			common = m.commonDir
		}
		return app.CommandResult{Stdout: []byte(common + "\n")}, true
	default:
		return app.CommandResult{ExitCode: 2, Stderr: []byte("git: unhandled attempt-worktree invocation " + sub)}, true
	}
}

// attemptStatusLines renders `status --porcelain=v1` lines; untracked
// entries appear only when showUntracked holds.
func attemptStatusLines(w *fakeAttemptWorktree, showUntracked bool) string {
	var lines []string
	if w.Staged {
		lines = append(lines, "A  s.txt")
	}
	if w.Modified {
		lines = append(lines, " M f.txt")
	}
	if w.DirtySubmodule {
		lines = append(lines, " M sub")
	}
	if showUntracked && w.NestedRepo {
		lines = append(lines, "?? nested/")
	}
	if showUntracked && w.Untracked {
		lines = append(lines, "?? new.txt")
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// attemptIndexTags renders `ls-files -v -z` for the probe's three tracked
// files.
func attemptIndexTags(w *fakeAttemptWorktree) string {
	f, g := "H", "H"
	if w.AssumeUnchanged {
		f = "h"
	}
	if w.SkipWorktree {
		g = "S"
	}
	return "H .gitignore\x00" + f + " f.txt\x00" + g + " g.txt\x00"
}

// attemptListLocked renders the probe-pinned `worktree list --porcelain
// -z` records: the main checkout, then every listed attempt checkout in
// path order.
func (g *fakeGitRepo) attemptListLocked() app.CommandResult {
	m := &g.attempt
	var b strings.Builder
	fmt.Fprintf(&b, "worktree %s\x00HEAD %s\x00branch refs/heads/main\x00\x00", m.root, fakeGitMainHead)
	paths := make([]string, 0, len(m.byPath))
	for path, w := range m.byPath {
		if !w.Unlisted {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		w := m.byPath[path]
		fmt.Fprintf(&b, "worktree %s\x00HEAD %s\x00", path, w.Head)
		if w.Branch == "" {
			b.WriteString("detached\x00")
		} else {
			fmt.Fprintf(&b, "branch %s\x00", w.Branch)
		}
		switch {
		case w.Locked && w.LockReason != "":
			fmt.Fprintf(&b, "locked %s\x00", w.LockReason)
		case w.Locked:
			b.WriteString("locked\x00")
		}
		if !w.Present {
			b.WriteString("prunable gitdir file points to non-existent location\x00")
		}
		b.WriteString("\x00")
	}
	return app.CommandResult{Stdout: []byte(b.String())}
}

// attemptRemoveLocked applies `git worktree remove` to an attempt checkout
// with the probe-pinned outcomes, recording force options and hidden data
// loss.
func (g *fakeGitRepo) attemptRemoveLocked(configs, options []string, path string) app.CommandResult {
	m := &g.attempt
	w := m.byPath[path]
	m.RemoveCalls = append(m.RemoveCalls, strings.Join(append(append(slices.Clone(configs), options...), path), " "))
	for _, option := range options {
		if option == "-f" || option == "--force" {
			m.ForcedRemovals = append(m.ForcedRemovals, path)
			delete(m.byPath, path)
			return app.CommandResult{}
		}
	}
	if len(options) != 0 {
		return app.CommandResult{ExitCode: 129, Stderr: []byte("error: unknown option\n")}
	}
	if w.Unlisted {
		return app.CommandResult{ExitCode: 128, Stderr: fmt.Appendf(nil, fakeNotWorkingTree, path)}
	}
	switch {
	case w.Locked && w.LockReason != "":
		return app.CommandResult{ExitCode: 128, Stderr: fmt.Appendf(nil, fakeLockedReason, w.LockReason)}
	case w.Locked:
		return app.CommandResult{ExitCode: 128, Stderr: []byte(fakeLockedRefusal)}
	}
	g.indexReadLocked(configs, "worktree remove "+path)
	if w.Present && w.HasSubmodule {
		return app.CommandResult{ExitCode: 128, Stderr: []byte(fakeSubmoduleRefusal)}
	}
	if w.Present {
		showUntracked := !m.hideUntracked || slices.Contains(configs, "status.showUntrackedFiles=all")
		if attemptStatusLines(w, showUntracked) != "" {
			return app.CommandResult{ExitCode: 128, Stderr: fmt.Appendf(nil, fakeDirtyRefusal, path)}
		}
		if w.Ignored || w.AssumeUnchanged || w.SkipWorktree || w.Untracked || w.NestedRepo {
			m.DeletedHiddenData = append(m.DeletedHiddenData, path)
		}
	}
	delete(m.byPath, path)
	return app.CommandResult{}
}

// catFileExistsLocked answers `cat-file -e <object>`: a known commit
// (optionally with ^{commit}) at the model's root exits 0; anything else
// exits 128 with the probe-pinned text, and a root that is not the
// model's (moved away) cannot be entered.
func (g *fakeGitRepo) catFileExistsLocked(dir, object string) app.CommandResult {
	if dir != g.attempt.root {
		return app.CommandResult{ExitCode: 128, Stderr: fmt.Appendf(nil, fakeCannotChangeTo, dir)}
	}
	base := strings.TrimSuffix(object, "^{commit}")
	if _, ok := g.commits[base]; ok || base == fakeGitMainHead {
		return app.CommandResult{}
	}
	return app.CommandResult{ExitCode: 128, Stderr: []byte("fatal: Not a valid object name " + object + "\n")}
}

// TestFakeAttemptWorktreesReproduceTheProbe drives the model through the
// same invocations the process-adapter probe executed against real git
// and requires the same observable results, hazards included.
func TestFakeAttemptWorktreesReproduceTheProbe(t *testing.T) {
	const (
		root   = "/srv/repo"
		path   = "/worktrees/repo/hop-r1-t1a1"
		branch = "refs/heads/hop/r1/t1a1"
	)
	override := []string{"-c", "status.showUntrackedFiles=all"}
	precheck := []string{"status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none"}
	cases := []struct {
		name        string
		state       fakeAttemptWorktree
		hide        bool
		configs     []string
		wantStatus  string
		wantTags    string
		wantExit    int
		wantStderr  string
		wantRemoved bool
		wantHidden  bool
	}{
		{name: "clean", wantTags: "H .gitignore\x00H f.txt\x00H g.txt\x00", wantRemoved: true},
		{name: "untracked", state: fakeAttemptWorktree{Untracked: true}, wantStatus: "?? new.txt\n", wantExit: 128, wantStderr: fmt.Sprintf(fakeDirtyRefusal, path)},
		{name: "modified", state: fakeAttemptWorktree{Modified: true}, wantStatus: " M f.txt\n", wantExit: 128, wantStderr: fmt.Sprintf(fakeDirtyRefusal, path)},
		{name: "staged", state: fakeAttemptWorktree{Staged: true}, wantStatus: "A  s.txt\n", wantExit: 128, wantStderr: fmt.Sprintf(fakeDirtyRefusal, path)},
		{name: "nested repository", state: fakeAttemptWorktree{NestedRepo: true}, wantStatus: "?? nested/\n", wantExit: 128, wantStderr: fmt.Sprintf(fakeDirtyRefusal, path)},
		{name: "locked", state: fakeAttemptWorktree{Locked: true}, wantExit: 128, wantStderr: fakeLockedRefusal},
		{name: "locked with a reason", state: fakeAttemptWorktree{Locked: true, LockReason: "in use"}, wantExit: 128, wantStderr: fmt.Sprintf(fakeLockedReason, "in use")},
		{name: "HAZARD ignored", state: fakeAttemptWorktree{Ignored: true}, wantRemoved: true, wantHidden: true},
		{name: "HAZARD untracked hidden by config", state: fakeAttemptWorktree{Untracked: true}, hide: true, wantStatus: "?? new.txt\n", wantRemoved: true, wantHidden: true},
		{name: "untracked hidden by config, with the override", state: fakeAttemptWorktree{Untracked: true}, hide: true, configs: override, wantStatus: "?? new.txt\n", wantExit: 128, wantStderr: fmt.Sprintf(fakeDirtyRefusal, path)},
		{name: "HAZARD assume-unchanged", state: fakeAttemptWorktree{AssumeUnchanged: true}, wantTags: "H .gitignore\x00h f.txt\x00H g.txt\x00", wantRemoved: true, wantHidden: true},
		{name: "HAZARD skip-worktree", state: fakeAttemptWorktree{SkipWorktree: true}, wantTags: "H .gitignore\x00H f.txt\x00S g.txt\x00", wantRemoved: true, wantHidden: true},
		{name: "clean submodule", state: fakeAttemptWorktree{HasSubmodule: true}, wantExit: 128, wantStderr: fakeSubmoduleRefusal},
		{name: "dirty submodule", state: fakeAttemptWorktree{HasSubmodule: true, DirtySubmodule: true}, wantStatus: " M sub\n", wantExit: 128, wantStderr: fakeSubmoduleRefusal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newFakeGitRepo("/opt/homebrew/bin/git", "/opt/hop/bin/hop")
			g.setAttemptRepository(root, "/srv/repo/.git")
			g.setHideUntracked(tc.hide)
			state := tc.state
			state.Branch, state.Head, state.Present = branch, "commit-base", true
			g.addAttemptWorktree(path, state)

			status := g.runGitArgv(append([]string{"-C", path, "-c", fakeFsmonitorOff}, precheck...))
			if status.ExitCode != 0 || string(status.Stdout) != tc.wantStatus {
				t.Errorf("status = exit %d %q, want %q", status.ExitCode, status.Stdout, tc.wantStatus)
			}
			if tc.wantTags != "" {
				if tags := g.runGitArgv([]string{"-C", path, "-c", fakeFsmonitorOff, "ls-files", "-v", "-z"}); string(tags.Stdout) != tc.wantTags {
					t.Errorf("ls-files -v -z = %q, want %q", tags.Stdout, tc.wantTags)
				}
			}
			args := append(append([]string{"-C", root}, tc.configs...), "worktree", "remove", path)
			result := g.runGitArgv(args)
			if result.ExitCode != tc.wantExit || string(result.Stderr) != tc.wantStderr {
				t.Errorf("remove = exit %d %q, want exit %d %q", result.ExitCode, result.Stderr, tc.wantExit, tc.wantStderr)
			}
			_, present := g.attemptWorktree(path)
			if present == tc.wantRemoved {
				t.Errorf("checkout still modeled = %v, want %v", present, !tc.wantRemoved)
			}
			g.requireNoForcedAttemptRemovals(t)
			if _, _, hidden := g.attemptLog(); (len(hidden) != 0) != tc.wantHidden {
				t.Errorf("hidden-data deletions %v, want hidden=%v", hidden, tc.wantHidden)
			}
		})
	}

	t.Run("a repository-local fsmonitor hook runs on every index read and removal without the override", func(t *testing.T) {
		g := newFakeGitRepo("/opt/homebrew/bin/git", "/opt/hop/bin/hop")
		g.setAttemptRepository(root, "/srv/repo/.git")
		g.setFsmonitorHook(true)
		for _, wt := range []string{"/wt/plain", "/wt/guarded", "/wt/old"} {
			g.addAttemptWorktree(wt, fakeAttemptWorktree{Branch: branch, Head: "commit-base", Present: true})
		}
		g.runGitArgv(append([]string{"-C", "/wt/plain"}, precheck...))
		g.runGitArgv([]string{"-C", "/wt/plain", "ls-files", "-v", "-z"})
		g.runGitArgv([]string{"-C", root, "-c", "status.showUntrackedFiles=all", "worktree", "remove", "/wt/plain"})
		want := []string{"status /wt/plain", "ls-files /wt/plain", "worktree remove /wt/plain"}
		if runs := g.fsmonitorRuns(); !slices.Equal(runs, want) {
			t.Fatalf("hook runs without the override = %q, want %q", runs, want)
		}
		g.runGitArgv(append([]string{"-C", "/wt/guarded", "-c", fakeFsmonitorOff}, precheck...))
		g.runGitArgv([]string{"-C", "/wt/guarded", "-c", fakeFsmonitorOff, "ls-files", "-v", "-z"})
		if removed := g.runGitArgv([]string{"-C", root, "-c", "status.showUntrackedFiles=all", "-c", fakeFsmonitorOff, "worktree", "remove", "/wt/guarded"}); removed.ExitCode != 0 {
			t.Fatalf("guarded removal = exit %d", removed.ExitCode)
		}
		if runs := g.fsmonitorRuns(); !slices.Equal(runs, want) {
			t.Fatalf("the override still ran the hook: %q", runs)
		}
		if old := g.runGitArgv([]string{"-C", "/wt/old", "-c", fakeFsmonitorOff, "status", "--porcelain=v1", "--untracked-files=all"}); old.ExitCode != 2 {
			t.Fatalf("a pre-check without --ignore-submodules=none = exit %d, want the model's unhandled refusal", old.ExitCode)
		}
	})

	t.Run("listing, prunable removal, identity and a forced removal", func(t *testing.T) {
		g := newFakeGitRepo("/opt/homebrew/bin/git", "/opt/hop/bin/hop")
		g.setAttemptRepository(root, "/srv/repo/.git")
		head := g.newCommit("tree-head")
		g.addAttemptWorktree("/wt/a", fakeAttemptWorktree{Branch: "refs/heads/hop/r1/t1a1", Head: head, Present: true})
		g.addAttemptWorktree("/wt/b", fakeAttemptWorktree{Branch: "refs/heads/hop/r1/t2a1", Head: head, Present: true, Locked: true, LockReason: "in use"})
		g.addAttemptWorktree("/wt/c", fakeAttemptWorktree{Head: head, Present: true})
		g.addAttemptWorktree("/wt/d", fakeAttemptWorktree{Branch: "refs/heads/hop/r1/t4a1", Head: head})
		g.addAttemptWorktree("/wt/e", fakeAttemptWorktree{Branch: "refs/heads/hop/r1/t5a1", Head: head, Present: true, Unlisted: true})

		list := g.runGitArgv([]string{"-C", root, "worktree", "list", "--porcelain", "-z"})
		want := "worktree /srv/repo\x00HEAD commit-main\x00branch refs/heads/main\x00\x00" +
			"worktree /wt/a\x00HEAD " + head + "\x00branch refs/heads/hop/r1/t1a1\x00\x00" +
			"worktree /wt/b\x00HEAD " + head + "\x00branch refs/heads/hop/r1/t2a1\x00locked in use\x00\x00" +
			"worktree /wt/c\x00HEAD " + head + "\x00detached\x00\x00" +
			"worktree /wt/d\x00HEAD " + head + "\x00branch refs/heads/hop/r1/t4a1\x00prunable gitdir file points to non-existent location\x00\x00"
		if string(list.Stdout) != want {
			t.Errorf("list = %q, want %q", list.Stdout, want)
		}
		if missing := g.runGitArgv(append([]string{"-C", "/wt/d"}, precheck...)); missing.ExitCode != 128 || string(missing.Stderr) != fmt.Sprintf(fakeCannotChangeTo, "/wt/d") {
			t.Errorf("status in a missing checkout = exit %d %q", missing.ExitCode, missing.Stderr)
		}
		if removed := g.runGitArgv([]string{"-C", root, "-c", "status.showUntrackedFiles=all", "worktree", "remove", "/wt/d"}); removed.ExitCode != 0 {
			t.Errorf("remove of a prunable checkout = exit %d %q, want 0", removed.ExitCode, removed.Stderr)
		}
		if refused := g.runGitArgv([]string{"-C", root, "worktree", "remove", "/wt/e"}); refused.ExitCode != 128 || string(refused.Stderr) != fmt.Sprintf(fakeNotWorkingTree, "/wt/e") {
			t.Errorf("remove of an unlisted directory = exit %d %q", refused.ExitCode, refused.Stderr)
		}
		if common := g.runGitArgv([]string{"-C", "/wt/a", "rev-parse", "--path-format=absolute", "--git-common-dir"}); string(common.Stdout) != "/srv/repo/.git\n" {
			t.Errorf("common dir = %q", common.Stdout)
		}
		for object, wantExit := range map[string]int{head + "^{commit}": 0, "commit-9999^{commit}": 128} {
			if got := g.runGitArgv([]string{"-C", root, "cat-file", "-e", object}); got.ExitCode != wantExit {
				t.Errorf("cat-file -e %s = exit %d, want %d", object, got.ExitCode, wantExit)
			}
		}
		if moved := g.runGitArgv([]string{"-C", "/moved", "cat-file", "-e", head + "^{commit}"}); moved.ExitCode != 128 {
			t.Errorf("cat-file -e at a moved root = exit %d, want 128", moved.ExitCode)
		}

		g.runGitArgv([]string{"-C", root, "worktree", "remove", "--force", "/wt/a"})
		removes, forced, _ := g.attemptLog()
		if !slices.Equal(forced, []string{"/wt/a"}) {
			t.Errorf("forced removals = %v, want the one recorded violation", forced)
		}
		if want := []string{"status.showUntrackedFiles=all /wt/d", "/wt/e", "--force /wt/a"}; !slices.Equal(removes, want) {
			t.Errorf("removal calls = %q, want %q", removes, want)
		}
	})
}
