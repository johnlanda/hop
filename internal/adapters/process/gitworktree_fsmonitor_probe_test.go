package process_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The worktree-retirement git probe's repository-code rows
// (docs/plan/phase-3-worktree-retirement.md sections 4 and 10): a
// repository-local core.fsmonitor hook runs on every index read git makes
// — the pre-check's status and ls-files and the removal's own clean check
// — unless the invocation carries `-c core.fsmonitor=false`, which child
// git processes (submodule recursion, the removal's inner status) inherit.
// A repository-local diff.ignoreSubmodules hides a dirty submodule from
// status unless `--ignore-submodules=none` is explicit, and git refuses to
// remove a checkout that contains a submodule. Every file lives under the
// test's own temporary directory; the hook only appends to a marker file
// there.

// fsmonitorHook writes an executable hook under the probe root that
// appends one line to marker per run, using shell builtins only (the
// production runner passes no PATH).
func (p *worktreeProbe) fsmonitorHook() (hook, marker string) {
	p.t.Helper()
	hook = filepath.Join(p.root, "fsmonitor-hook.sh")
	marker = filepath.Join(p.root, "fsmonitor-ran")
	p.writeFile(hook, "#!/bin/sh\necho ran >> '"+marker+"'\nexit 1\n")
	if err := os.Chmod(hook, 0o700); err != nil { //nolint:gosec // G302: the probe's own hook must be executable.
		p.t.Fatal(err)
	}
	return hook, marker
}

// hookRan reports whether the hook ran since the last call, clearing the
// marker.
func hookRan(t *testing.T, marker string) bool {
	t.Helper()
	ran := exists(t, marker)
	if err := os.RemoveAll(marker); err != nil {
		t.Fatal(err)
	}
	return ran
}

// fsmonitorOverride is the configuration pair every retirement read of a
// checkout's index and the removal carry.
func fsmonitorOverride() []string { return []string{"-c", "core.fsmonitor=false"} }

// TestGitProbeFsmonitorOverride proves the hazard first — each plain
// invocation runs a repository-local fsmonitor hook — then the protection:
// the same invocation with `-c core.fsmonitor=false` never runs it, with
// the pre-check output unchanged and the removal still succeeding.
func TestGitProbeFsmonitorOverride(t *testing.T) {
	p := newWorktreeProbe(t)
	checkout, _ := p.addWorktree("t1a1")
	plainRemoval, _ := p.addWorktree("t2a1")
	overrideRemoval, _ := p.addWorktree("t3a1")
	hook, marker := p.fsmonitorHook()
	p.must(p.repo, "config", "core.fsmonitor", hook)
	hookRan(t, marker)

	reads := []struct {
		name string
		args []string
		want string
	}{
		{"status pre-check", []string{"status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none"}, ""},
		{"index flags", []string{"ls-files", "-v", "-z"}, "H .gitignore\x00H f.txt\x00H g.txt\x00"},
	}
	for _, read := range reads {
		plain := p.run(checkout, read.args...)
		if plain.ExitCode != 0 || !hookRan(t, marker) {
			t.Errorf("%s without the override: exit %d, hook ran %v; the hazard needs the hook to run", read.name, plain.ExitCode, exists(t, marker))
		}
		guarded := p.run(checkout, append(fsmonitorOverride(), read.args...)...)
		if guarded.ExitCode != 0 || string(guarded.Stdout) != read.want || len(guarded.Stderr) != 0 {
			t.Errorf("%s with the override: exit %d stdout %q stderr %q, want exit 0 and %q", read.name, guarded.ExitCode, guarded.Stdout, guarded.Stderr, read.want)
		}
		if hookRan(t, marker) {
			t.Errorf("%s with the override ran the repository's fsmonitor hook", read.name)
		}
	}

	removal := []string{"-c", "status.showUntrackedFiles=all", "worktree", "remove"}
	plain := p.run(p.repo, append(removal, plainRemoval)...)
	if plain.ExitCode != 0 || exists(t, plainRemoval) || !hookRan(t, marker) {
		t.Errorf("worktree remove without the override: exit %d, removed %v, hook ran; want exit 0, removed and the hook run (the hazard)", plain.ExitCode, !exists(t, plainRemoval))
	}
	guarded := p.run(p.repo, append(append(fsmonitorOverride(), removal...), overrideRemoval)...)
	if guarded.ExitCode != 0 || exists(t, overrideRemoval) || len(guarded.Stderr) != 0 {
		t.Errorf("worktree remove with the override: exit %d stderr %q, removed %v; want exit 0 and removed", guarded.ExitCode, guarded.Stderr, !exists(t, overrideRemoval))
	}
	if hookRan(t, marker) {
		t.Error("worktree remove with the override ran the repository's fsmonitor hook")
	}
}

// TestGitProbeSubmoduleCheckouts pins what a checkout holding a submodule
// shows the pre-check and the removal: status recurses into the submodule
// (its own fsmonitor hook runs unless the inherited override is set), a
// repository-local diff.ignoreSubmodules=all hides the submodule's changes
// unless `--ignore-submodules=none` is explicit, and `git worktree remove`
// refuses the checkout outright, clean or not.
func TestGitProbeSubmoduleCheckouts(t *testing.T) {
	p := newWorktreeProbe(t)
	module := filepath.Join(p.root, "module")
	p.must(p.root, "init", "-q", "--object-format=sha1", "-b", "main", "module")
	p.writeFile(filepath.Join(module, "m.txt"), "m\n")
	p.must(module, "add", "m.txt")
	p.must(module, "-c", "user.name=hop-probe", "-c", "user.email=probe@invalid", "commit", "-q", "-m", "module")
	p.must(p.repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", module, "sub")
	p.base = p.commitAll("add the submodule", false)
	checkout, _ := p.addWorktree("t1a1")
	p.must(checkout, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
	if !exists(t, filepath.Join(checkout, "sub", "m.txt")) {
		t.Fatal("the submodule was not checked out in the linked checkout")
	}

	precheck := []string{"status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none"}
	hook, marker := p.fsmonitorHook()
	p.must(filepath.Join(checkout, "sub"), "config", "core.fsmonitor", hook)
	hookRan(t, marker)
	if plain := p.run(checkout, precheck...); plain.ExitCode != 0 || !hookRan(t, marker) {
		t.Errorf("status without the override: exit %d; want the submodule's own hook to run (the hazard)", plain.ExitCode)
	}
	guarded := p.run(checkout, append(fsmonitorOverride(), precheck...)...)
	if guarded.ExitCode != 0 || len(guarded.Stdout) != 0 {
		t.Errorf("status with the override: exit %d stdout %q, want a clean checkout", guarded.ExitCode, guarded.Stdout)
	}
	if hookRan(t, marker) {
		t.Error("the override did not reach the submodule: its fsmonitor hook ran")
	}
	p.must(filepath.Join(checkout, "sub"), "config", "--unset", "core.fsmonitor")

	p.must(p.repo, "config", "diff.ignoreSubmodules", "all")
	p.writeFile(filepath.Join(checkout, "sub", "m.txt"), "changed\n")
	defaulted := p.run(checkout, append(fsmonitorOverride(), "status", "--porcelain=v1", "--untracked-files=all")...)
	if defaulted.ExitCode != 0 || len(defaulted.Stdout) != 0 {
		t.Errorf("status honoring diff.ignoreSubmodules=all: exit %d stdout %q, want the dirty submodule hidden (the hazard)", defaulted.ExitCode, defaulted.Stdout)
	}
	explicit := p.run(checkout, append(fsmonitorOverride(), precheck...)...)
	if explicit.ExitCode != 0 || strings.TrimSpace(string(explicit.Stdout)) != "M sub" {
		t.Errorf("status with --ignore-submodules=none: exit %d stdout %q, want the dirty submodule reported", explicit.ExitCode, explicit.Stdout)
	}

	removal := append(fsmonitorOverride(), "-c", "status.showUntrackedFiles=all", "worktree", "remove", checkout)
	for _, state := range []string{"dirty", "clean"} {
		if state == "clean" {
			p.must(filepath.Join(checkout, "sub"), "checkout", "-q", "--", "m.txt")
		}
		result := p.run(p.repo, removal...)
		if result.ExitCode != 128 || string(result.Stderr) != "fatal: working trees containing submodules cannot be moved or removed\n" {
			t.Errorf("worktree remove of a %s checkout with a submodule: exit %d stderr %q, want 128 and the submodule refusal", state, result.ExitCode, result.Stderr)
		}
		if _, listed := p.listRecord(checkout); !listed || !exists(t, filepath.Join(checkout, "sub", "m.txt")) {
			t.Errorf("a refused %s removal changed the checkout", state)
		}
	}
}
