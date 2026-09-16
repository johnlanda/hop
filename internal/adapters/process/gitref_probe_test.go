package process_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/process"
	"github.com/johnlanda/hop/internal/app"
)

// gitProbe runs real git through the production Runner — the transport
// every app git invocation uses — against one isolated repository.
type gitProbe struct {
	t    *testing.T
	git  string
	repo string
	env  []string
}

func newGitProbe(t *testing.T) *gitProbe {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("real-git probe skipped, not run: no git executable on PATH")
	}
	gitPath, err = filepath.Abs(gitPath)
	if err != nil {
		t.Fatalf("resolve git: %v", err)
	}
	home := t.TempDir()
	p := &gitProbe{t: t, git: gitPath, repo: filepath.Join(home, "repo"), env: []string{
		"HOME=" + home, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=probe", "GIT_AUTHOR_EMAIL=probe@invalid", "GIT_COMMITTER_NAME=probe", "GIT_COMMITTER_EMAIL=probe@invalid",
	}}
	if result := p.runIn(home, "init", "-q", "--object-format=sha1", p.repo); result.ExitCode != 0 {
		t.Fatalf("git init: exit %d: %s", result.ExitCode, result.Stderr)
	}
	return p
}

func (p *gitProbe) runIn(dir string, args ...string) app.CommandResult {
	p.t.Helper()
	result, err := process.Runner{}.Run(p.t.Context(), app.Command{Argv: append([]string{p.git, "-C", dir}, args...), Env: p.env})
	if err != nil {
		p.t.Fatalf("git %s: runner error: %v", strings.Join(args, " "), err)
	}
	return result
}

// run runs one git subcommand in the probe repository, logging the full
// observation.
func (p *gitProbe) run(args ...string) app.CommandResult {
	p.t.Helper()
	result := p.runIn(p.repo, args...)
	p.t.Logf("git %s -> exit %d stdout %q stderr %q", strings.Join(args, " "), result.ExitCode, result.Stdout, result.Stderr)
	return result
}

// out requires success and returns trimmed stdout.
func (p *gitProbe) out(args ...string) string {
	p.t.Helper()
	result := p.run(args...)
	if result.ExitCode != 0 {
		p.t.Fatalf("git %s: exit %d", strings.Join(args, " "), result.ExitCode)
	}
	return strings.TrimSpace(string(result.Stdout))
}

// TestGitRefSemanticsThroughRunner is the executed probe (law 6BA4DBBF)
// behind internal/app's integration-ref rules, under the production
// transport: the symbolic-ref read internal/app's observeRef decides on,
// the create-only `update-ref --no-deref <ref> <new> ""` of
// integration.init, and the `--no-deref` compare-and-swap of publish,
// reset and fence. internal/app's fake git reproduces exactly these
// observations.
func TestGitRefSemanticsThroughRunner(t *testing.T) {
	p := newGitProbe(t)
	tree := p.out("mktree")
	base := p.out("commit-tree", tree, "-m", "base")
	next := p.out("commit-tree", tree, "-p", base, "-m", "next")
	p.out("update-ref", "refs/heads/direct", base)
	p.out("update-ref", "refs/heads/foreign", next)
	p.out("symbolic-ref", "refs/heads/hop/sym", "refs/heads/foreign")
	p.out("symbolic-ref", "refs/heads/hop/dangling", "refs/heads/foreign-unborn")

	symbolic := func(ref string) (int, string) {
		t.Helper()
		result := p.run("symbolic-ref", "-q", ref)
		return result.ExitCode, strings.TrimSpace(string(result.Stdout))
	}

	t.Run("symbolic-ref -q: 0 with the target for any symref, 1 with no output otherwise", func(t *testing.T) {
		for _, tt := range []struct {
			ref, target string
			exit        int
		}{
			{"refs/heads/direct", "", 1},
			{"refs/heads/hop/sym", "refs/heads/foreign", 0},
			{"refs/heads/hop/dangling", "refs/heads/foreign-unborn", 0},
			{"refs/heads/absent", "", 1},
		} {
			if exit, target := symbolic(tt.ref); exit != tt.exit || target != tt.target {
				t.Errorf("symbolic-ref -q %s = exit %d %q, want exit %d %q", tt.ref, exit, target, tt.exit, tt.target)
			}
		}
		// The rev-parse read that follows never sees a dangling symref as a
		// ref: it fails exactly as an absent ref does, which is why the
		// symbolic read comes first.
		if result := p.run("rev-parse", "--verify", "refs/heads/hop/dangling"); result.ExitCode == 0 {
			t.Errorf("rev-parse --verify on a dangling symref succeeded")
		}
		if got := p.out("rev-parse", "--verify", "refs/heads/hop/sym"); got != next {
			t.Errorf("rev-parse --verify on a symref = %s, want its target's %s", got, next)
		}
	})

	t.Run("create-only --no-deref is refused on a symref, dangling or not, which stays unchanged", func(t *testing.T) {
		for _, tt := range []struct{ ref, target string }{
			{"refs/heads/hop/dangling", "refs/heads/foreign-unborn"},
			{"refs/heads/hop/sym", "refs/heads/foreign"},
		} {
			if result := p.run("update-ref", "--no-deref", tt.ref, base, ""); result.ExitCode == 0 {
				t.Errorf("create-only --no-deref on %s succeeded", tt.ref)
			}
			if exit, target := symbolic(tt.ref); exit != 0 || target != tt.target {
				t.Errorf("%s after the refused create = exit %d %q, want the symref to %s intact", tt.ref, exit, target, tt.target)
			}
		}
		if result := p.run("rev-parse", "--verify", "refs/heads/foreign-unborn"); result.ExitCode == 0 {
			t.Errorf("the dangling symref's target was created")
		}
		if got := p.out("rev-parse", "--verify", "refs/heads/foreign"); got != next {
			t.Errorf("the symref's target moved to %s", got)
		}
	})

	t.Run("without --no-deref the create-only update writes through a dangling symref (the defect)", func(t *testing.T) {
		p.out("symbolic-ref", "refs/heads/hop/followed", "refs/heads/followed-unborn")
		if result := p.run("update-ref", "refs/heads/hop/followed", base, ""); result.ExitCode != 0 {
			t.Fatalf("the dereferencing create-only update was refused; the defect no longer reproduces")
		}
		if got := p.out("rev-parse", "--verify", "refs/heads/followed-unborn"); got != base {
			t.Errorf("the dereferenced target = %s, want %s", got, base)
		}
	})

	t.Run("create-only --no-deref creates an absent ref", func(t *testing.T) {
		if result := p.run("update-ref", "--no-deref", "refs/heads/hop/new", base, ""); result.ExitCode != 0 {
			t.Fatalf("create-only --no-deref on an absent ref: exit %d", result.ExitCode)
		}
		if got := p.out("rev-parse", "--verify", "refs/heads/hop/new"); got != base {
			t.Errorf("created ref = %s, want %s", got, base)
		}
		if exit, _ := symbolic("refs/heads/hop/new"); exit != 1 {
			t.Errorf("the created ref reads as symbolic")
		}
	})

	t.Run("--no-deref CAS on a direct ref: moves with the current old value, refused with a stale one", func(t *testing.T) {
		if result := p.run("update-ref", "--no-deref", "refs/heads/direct", next, base); result.ExitCode != 0 {
			t.Fatalf("CAS with the current old value: exit %d", result.ExitCode)
		}
		if result := p.run("update-ref", "--no-deref", "refs/heads/direct", base, base); result.ExitCode == 0 {
			t.Fatalf("CAS with a stale old value succeeded")
		}
		if got := p.out("rev-parse", "--verify", "refs/heads/direct"); got != next {
			t.Errorf("direct ref = %s, want %s", got, next)
		}
	})

	t.Run("--no-deref CAS on a symref replaces the symref itself and never moves its target", func(t *testing.T) {
		p.out("symbolic-ref", "refs/heads/hop/cas", "refs/heads/foreign")
		result := p.run("update-ref", "--no-deref", "refs/heads/hop/cas", base, next)
		if result.ExitCode != 0 {
			t.Fatalf("CAS on a symref with its target's value: exit %d (the recorded behavior changed)", result.ExitCode)
		}
		if exit, _ := symbolic("refs/heads/hop/cas"); exit != 1 {
			t.Errorf("the symref survived the --no-deref CAS")
		}
		if got := p.out("rev-parse", "--verify", "refs/heads/hop/cas"); got != base {
			t.Errorf("replaced ref = %s, want %s", got, base)
		}
		if got := p.out("rev-parse", "--verify", "refs/heads/foreign"); got != next {
			t.Errorf("the symref's foreign target moved to %s", got)
		}
	})
}
