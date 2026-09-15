package integration

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A fixtureRepo is a real, temporary SHA-1 git repository the real-process
// suite drives `hop run`/`hop check-exec` against end to end: a known base
// commit, a trivial source file, a deterministic check the caller toggles
// pass/fail by choosing which commit it submits (CommitCheckResult), a
// git-dependent check variant that only succeeds inside a real checkout
// (newFixtureRepoGitDependentCheck), and a submodule variant for the
// "submodule repository fails clearly" scenario
// (newFixtureRepoWithSubmodule). SHA-1 object format only — Phase 2 rejects
// any other (usecase_run.go's `git rev-parse --show-object-format` check).

// configRelPath is the policy file's fixed location under a repository
// root, matching internal/adapters/config's own configRelPath constant.
const configRelPath = ".herdr-orchestrator/config.toml"

// Fixture file names committed at the repository root.
const (
	checkScriptName             = "check.sh"
	gitDependentCheckScriptName = "check-git.sh"
	submoduleCheckScriptName    = "check-submodule.sh"
	checkResultFile             = "CHECK_RESULT"
	submoduleDir                = "vendor/inner"
)

// trivialSourceFile is committed at the repository root as the "trivial
// source file" the design's fixture generator calls for; nothing builds or
// imports it, it only needs to exist and be editable by the fixture worker.
const trivialSourceFile = `package main

func main() {}
`

// checkScriptSource is the deterministic check: it exits 0 exactly when
// CHECK_RESULT (committed alongside it) reads "pass", and non-zero
// otherwise, naming the actual content on failure. The caller controls the
// outcome "on demand" by choosing which commit — and therefore which
// CHECK_RESULT content — it submits; nothing here depends on an environment
// variable or other runtime state the sanitized exec boundary could strip.
const checkScriptSource = `#!/bin/sh
set -eu
result=$(cat CHECK_RESULT)
if [ "$result" = "pass" ]; then
  echo "check: CHECK_RESULT is pass"
  exit 0
fi
echo "check: CHECK_RESULT is $result, not pass" >&2
exit 1
`

// gitDependentCheckScriptSource only succeeds inside a real git checkout:
// it requires git metadata to resolve and requires HEAD to track this very
// script, which is true of a `git worktree add` checkout (the check
// pipeline's candidate isolation, section 7) and false of a tree exported by
// something like `git archive`, which strips .git entirely. It otherwise
// defers to check.sh's CHECK_RESULT contract.
const gitDependentCheckScriptSource = `#!/bin/sh
set -eu
git rev-parse --is-inside-work-tree >/dev/null 2>&1 || {
  echo "check-git: not inside a git checkout" >&2
  exit 1
}
git cat-file -e HEAD:check-git.sh 2>/dev/null || {
  echo "check-git: HEAD does not track check-git.sh" >&2
  exit 1
}
exec sh check.sh
`

// submoduleCheckScriptSource fails clearly when the referenced submodule was
// not checked out: a detached `git worktree add` (section 7's candidate
// isolation) does not initialize submodules, and Phase 2 leaves them
// unsupported by design (docs/plan/phase-2-design.md section 7's export
// contract) — a check that depends on submodule content simply fails, with
// the missing path named.
const submoduleCheckScriptSource = `#!/bin/sh
set -eu
if [ ! -f ` + submoduleDir + `/marker.txt ]; then
  echo "check: submodule ` + submoduleDir + ` is not checked out; submodule repositories are unsupported in Phase 2" >&2
  exit 1
fi
exit 0
`

// fixtureRepo is one temporary git repository built for a single test. Root
// is the repository's working tree — the "repository root" hop run and
// HOP's own git invocations use; Base is the initial commit's object id,
// matching what StartRun resolves as `git rev-parse HEAD^{commit}` at that
// point.
type fixtureRepo struct {
	Root    string
	Base    string
	gitHome string // isolated HOME for this repository's own git invocations; see fixtureGitEnviron.
}

// fixtureGitEnviron builds a hermetic environment for git commands that
// build or extend a fixture repository from this test process: an isolated
// HOME with GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM both pointed at
// /dev/null, so nothing from the developer's real git configuration
// (signing keys, hooks path, aliases) affects fixture construction. The
// repositories this builds carry their own local user identity and disable
// commit signing (initFixtureRepo), so every later git operation against
// them — by hop's own subprocesses, or by the fixture worker committing
// inside a launched pane, regardless of what environment survives
// sanitization — is self-contained and does not depend on this build-time
// environment at all.
func fixtureGitEnviron(home string) []string {
	return []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	}
}

// git runs one git subcommand against the repository and returns its
// trimmed combined output, failing the test on a non-zero exit.
func (r *fixtureRepo) git(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.Root}, args...)...) //nolint:gosec // G204: git is resolved from PATH; args are fixed by this suite's own fixture construction.
	cmd.Env = fixtureGitEnviron(r.gitHome)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s %s: %v\n%s", r.Root, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeFile writes one file inside the repository's working tree, creating
// parent directories as needed.
func (r *fixtureRepo) writeFile(t *testing.T, relPath, content string, mode os.FileMode) {
	t.Helper()
	full := filepath.Join(r.Root, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// commit stages every change in the working tree and commits it, returning
// the new commit's object id.
func (r *fixtureRepo) commit(t *testing.T, message string) string {
	t.Helper()
	r.git(t, "add", "-A")
	r.git(t, "commit", "-m", message)
	return r.git(t, "rev-parse", "HEAD^{commit}")
}

// CommitCheckResult writes result to CHECK_RESULT and commits it, returning
// the new commit's object id. This is the "on demand" toggle: the caller
// picks which commit to submit, deterministically choosing whether
// check.sh (and check-git.sh, which defers to it) passes or fails.
func (r *fixtureRepo) CommitCheckResult(t *testing.T, result string) string {
	t.Helper()
	r.writeFile(t, checkResultFile, result+"\n", 0o644)
	return r.commit(t, "check result: "+result)
}

// fixtureConfigTOML renders a minimal .herdr-orchestrator/config.toml per
// the design's section 3 example: the given [check] command, a short
// timeout so a hung check fails scenarios fast instead of waiting out the
// design's 10m default, and the claude harness (Phase 2 launches only
// claude).
func fixtureConfigTOML(checkCommand []string) string {
	quoted := make([]string, len(checkCommand))
	for i, arg := range checkCommand {
		quoted[i] = strconv.Quote(arg)
	}
	return "[check]\n" +
		"command = [" + strings.Join(quoted, ", ") + "]\n" +
		"timeout = \"30s\"\n" +
		"\n[worker]\n" +
		"harness = \"claude\"\n"
}

// initFixtureRepo creates an empty SHA-1 repository at
// <artifacts.path>/<name> with a deterministic local identity and commit
// signing disabled, isolated from any developer git configuration
// (fixtureGitEnviron). The initial branch is fixed to "main" so the
// repository's default branch name never depends on the host's git version
// or configuration.
func initFixtureRepo(t *testing.T, artifacts *artifactDir, name string) *fixtureRepo {
	t.Helper()
	repo := &fixtureRepo{
		Root:    artifacts.dir(t, name),
		gitHome: artifacts.dir(t, name+"-git-home"),
	}
	repo.git(t, "init", "--object-format=sha1", "--initial-branch=main")
	repo.git(t, "config", "user.name", "hop-fixture")
	repo.git(t, "config", "user.email", "hop-fixture@example.invalid")
	repo.git(t, "config", "commit.gpgsign", "false")
	return repo
}

// newFixtureRepoWithCheck is the shared constructor behind newFixtureRepo
// and newFixtureRepoGitDependentCheck: same repository shape, differing
// only in which committed script is wired as the [check] command.
func newFixtureRepoWithCheck(t *testing.T, artifacts *artifactDir, name, checkCommand string) *fixtureRepo {
	t.Helper()
	repo := initFixtureRepo(t, artifacts, name)
	repo.writeFile(t, "hello.go", trivialSourceFile, 0o644)
	repo.writeFile(t, checkScriptName, checkScriptSource, 0o755)
	repo.writeFile(t, gitDependentCheckScriptName, gitDependentCheckScriptSource, 0o755)
	repo.writeFile(t, checkResultFile, "pass\n", 0o644)
	repo.writeFile(t, configRelPath, fixtureConfigTOML([]string{"sh", checkCommand}), 0o644)
	repo.Base = repo.commit(t, "initial commit")
	return repo
}

// newFixtureRepo creates a temporary SHA-1 git repository under the test's
// own artifact directory (so it is retained for inspection on failure, like
// every other evidence this suite produces): a trivial source file, the
// deterministic check.sh (starting at CHECK_RESULT=pass) wired as the
// repository's [check] command, and .herdr-orchestrator/config.toml per the
// section 3 example. It returns after the initial commit, whose object id
// is fixtureRepo.Base — the base commit hop run resolves at StartRun.
func newFixtureRepo(t *testing.T, artifacts *artifactDir, name string) *fixtureRepo { //nolint:unparam // every current call site names its one repository "repo"; name exists so a scenario needing two concurrent plain fixture repositories (as newFixtureRepoWithSubmodule already needs internally for its inner/outer pair) can avoid an artifact-directory collision.
	t.Helper()
	return newFixtureRepoWithCheck(t, artifacts, name, checkScriptName)
}

// newFixtureRepoGitDependentCheck is newFixtureRepo with check-git.sh wired
// as the [check] command instead: it only succeeds inside a real git
// checkout, proving the check pipeline's candidate isolation is a detached
// `git worktree add` (which keeps .git metadata) rather than an archive
// export (which would strip it).
func newFixtureRepoGitDependentCheck(t *testing.T, artifacts *artifactDir, name string) *fixtureRepo {
	t.Helper()
	return newFixtureRepoWithCheck(t, artifacts, name, gitDependentCheckScriptName)
}

// newFixtureRepoWithSubmodule creates a fixture repository whose working
// tree references a real git submodule at vendor/inner: a detached `git
// worktree add` (the check pipeline's candidate isolation, section 7) does
// not initialize submodules, so check-submodule.sh — wired as this
// repository's [check] command — fails clearly with the missing path named,
// proving the "submodule repository fails clearly" scenario.
func newFixtureRepoWithSubmodule(t *testing.T, artifacts *artifactDir, name string) *fixtureRepo {
	t.Helper()
	inner := initFixtureRepo(t, artifacts, name+"-submodule")
	inner.writeFile(t, "marker.txt", "submodule-content\n", 0o644)
	inner.commit(t, "submodule initial commit")

	outer := initFixtureRepo(t, artifacts, name)
	outer.writeFile(t, "hello.go", trivialSourceFile, 0o644)
	outer.writeFile(t, submoduleCheckScriptName, submoduleCheckScriptSource, 0o755)
	outer.writeFile(t, checkResultFile, "pass\n", 0o644)
	outer.writeFile(t, configRelPath, fixtureConfigTOML([]string{"sh", submoduleCheckScriptName}), 0o644)
	// protocol.file.allow=always: git 2.38+ refuses a local file:// submodule
	// URL by default (CVE-2022-39253's mitigation); this repository and its
	// submodule are both this test's own disposable fixtures on local disk,
	// so the restriction has nothing to protect here.
	outer.git(t, "-c", "protocol.file.allow=always", "submodule", "add", "--", inner.Root, submoduleDir)
	outer.Base = outer.commit(t, "initial commit with submodule reference")
	return outer
}

// runShellScript runs "sh <script>" in dir and returns its combined output,
// exit code and any start failure. It never fails the test itself — the
// caller asserts on the outcome, matching how the real check pipeline
// treats a check command's exit code as data, not a harness error.
func runShellScript(t *testing.T, dir, script string) (output string, exitCode int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", script) //nolint:gosec // G204: fixed interpreter and a script this suite committed itself.
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		exitCode = 0
	case errors.As(err, &exitErr):
		exitCode = exitErr.ExitCode()
	default:
		t.Fatalf("run sh %s in %s: %v", script, dir, err)
	}
	return string(out), exitCode
}

// TestFixtureRepoDeterministicCheck proves check.sh's "on demand" pass/fail
// contract directly, with no herdr or hop binary involved: the repository
// starts passing, and CommitCheckResult flips it deterministically.
func TestFixtureRepoDeterministicCheck(t *testing.T) {
	artifacts := newArtifactDir(t)
	repo := newFixtureRepo(t, artifacts, "repo")

	if len(repo.Base) != 40 {
		t.Fatalf("base commit %q is not a 40-hex sha1 object id", repo.Base)
	}
	if out, code := runShellScript(t, repo.Root, checkScriptName); code != 0 {
		t.Fatalf("check.sh at the base commit (CHECK_RESULT=pass) exit=%d, want 0:\n%s", code, out)
	}

	failing := repo.CommitCheckResult(t, "fail")
	if failing == repo.Base {
		t.Fatal("CommitCheckResult did not produce a new commit")
	}
	if out, code := runShellScript(t, repo.Root, checkScriptName); code == 0 {
		t.Fatalf("check.sh after CommitCheckResult(fail) exit=0, want non-zero:\n%s", out)
	} else if !strings.Contains(out, "fail") {
		t.Errorf("check.sh failure output %q does not name the CHECK_RESULT content", out)
	}

	repo.CommitCheckResult(t, "pass")
	if out, code := runShellScript(t, repo.Root, checkScriptName); code != 0 {
		t.Fatalf("check.sh after CommitCheckResult(pass) exit=%d, want 0:\n%s", code, out)
	}
}

// TestFixtureRepoGitDependentCheck proves check-git.sh only succeeds inside
// a real git checkout: it passes in the fixture repository itself, and
// fails once the same files are copied to a plain, non-git directory —
// exactly the difference between the check pipeline's detached `git
// worktree add` checkout (git metadata present) and an archive export
// (metadata stripped).
func TestFixtureRepoGitDependentCheck(t *testing.T) {
	artifacts := newArtifactDir(t)
	repo := newFixtureRepoGitDependentCheck(t, artifacts, "repo")

	if out, code := runShellScript(t, repo.Root, gitDependentCheckScriptName); code != 0 {
		t.Fatalf("check-git.sh inside the real checkout exit=%d, want 0:\n%s", code, out)
	}

	plain := artifacts.dir(t, "repo-plain-copy")
	for _, name := range []string{gitDependentCheckScriptName, checkScriptName, checkResultFile} {
		copyFile(t, filepath.Join(repo.Root, name), filepath.Join(plain, name))
	}
	if err := os.Chmod(filepath.Join(plain, gitDependentCheckScriptName), 0o755); err != nil { //nolint:gosec // G302: the script must stay executable after the copy.
		t.Fatal(err)
	}
	out, code := runShellScript(t, plain, gitDependentCheckScriptName)
	if code == 0 {
		t.Fatalf("check-git.sh outside any git checkout exit=0, want non-zero:\n%s", out)
	}
	if !strings.Contains(out, "not inside a git checkout") {
		t.Errorf("check-git.sh failure output %q does not name the missing git checkout", out)
	}
}

// TestFixtureRepoSubmoduleFailsClearly proves the "submodule repository
// fails clearly" scenario at the git level, independent of hop's own check
// pipeline: materializing a detached `git worktree add` checkout of the
// submodule-referencing fixture — exactly the mechanism section 7 uses —
// leaves the submodule directory empty, so check-submodule.sh fails with
// the missing path named; populating that directory makes it pass, proving
// the script's failure is specifically about the submodule content.
func TestFixtureRepoSubmoduleFailsClearly(t *testing.T) {
	artifacts := newArtifactDir(t)
	repo := newFixtureRepoWithSubmodule(t, artifacts, "repo")

	checkout := artifacts.dir(t, "repo-detached-checkout")
	if err := os.RemoveAll(checkout); err != nil {
		t.Fatal(err)
	}
	repo.git(t, "worktree", "add", "--detach", checkout, repo.Base)
	t.Cleanup(func() {
		repo.git(t, "worktree", "remove", "--force", checkout)
	})

	out, code := runShellScript(t, checkout, submoduleCheckScriptName)
	if code == 0 {
		t.Fatalf("check-submodule.sh against an uninitialized submodule exit=0, want non-zero:\n%s", out)
	}
	if !strings.Contains(out, submoduleDir) || !strings.Contains(out, "not checked out") {
		t.Errorf("check-submodule.sh failure output %q does not clearly name the uninitialized submodule", out)
	}

	if err := os.MkdirAll(filepath.Join(checkout, submoduleDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, submoduleDir, "marker.txt"), []byte("submodule-content\n"), 0o644); err != nil { //nolint:gosec // G306: test-owned fixture content.
		t.Fatal(err)
	}
	if out, code := runShellScript(t, checkout, submoduleCheckScriptName); code != 0 {
		t.Fatalf("check-submodule.sh with the submodule content present exit=%d, want 0:\n%s", code, out)
	}
}
