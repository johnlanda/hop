package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// statusDetailValues returns every rendered value for a repeated
// "label:"-prefixed line across hop status -run's detail block
// (statuscmd.go's renderRunDetail repeats "artifact:" once per retained
// artifact and "evidence:" once per check evidence path); parseStatusDetail
// keeps only the last of a repeated key, so a scenario asserting on every
// occurrence scans the raw output directly instead.
func statusDetailValues(output, label string) []string {
	var values []string
	prefix := label + ":"
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, prefix); ok {
			values = append(values, strings.TrimSpace(rest))
		}
	}
	return values
}

// TestRealProcessFailedCheckRetainsArtifacts proves design section 7 step
// 4's failing-check contract: a deterministic check that exits non-zero
// against the worker's submitted commit ends the run `failed` (never
// `stopped` or left `completing`), and the check's stdout/stderr are
// retained on disk under the run's own checks directory and named by hop
// status as evidence, rather than discarded with the removed checkout.
func TestRealProcessFailedCheckRetainsArtifacts(t *testing.T) {
	artifacts, server := newFixtureRunEnv(t)
	repo := newFixtureRepo(t, artifacts, server, "repo")
	// The fixture worker's commitChange never touches CHECK_RESULT, so
	// whatever this second commit leaves committed is what check.sh sees
	// when it runs against the worker's submitted commit.
	repo.CommitCheckResult(t, "fail")
	fx := startRun(t, artifacts, server, repo, "submit-valid")

	fx.requireRunState(t, "failed")
	waitForControllerExit(t, fx.controller, 30*time.Second)

	result := runHop(t, fx.env, fx.repo.Root, "status", "-C", fx.repo.Root, "-run", fx.runID)
	if result.ExitCode != 0 {
		t.Fatalf("hop status exit=%d, want 0; stderr=%q", result.ExitCode, result.Stderr)
	}
	detail := parseStatusDetail(result.Stdout)
	if detail["state"] != "failed" {
		t.Fatalf("run state = %q, want \"failed\"; full detail:\n%s", detail["state"], result.Stdout)
	}
	if !strings.Contains(detail["last check"], "failed") {
		t.Errorf("last check summary = %q, want it to name a failed check; full detail:\n%s", detail["last check"], result.Stdout)
	}

	evidence := statusDetailValues(result.Stdout, "evidence")
	if len(evidence) == 0 {
		t.Fatalf("hop status names no retained check evidence; full detail:\n%s", result.Stdout)
	}
	sawStdout, sawStderr := false, false
	for _, path := range evidence {
		if !filepath.IsAbs(path) {
			t.Errorf("evidence path %q is not absolute", path)
		}
		content, err := os.ReadFile(path) //nolint:gosec // G304: a path this test read back from hop status's own rendering of its own state root.
		if err != nil {
			t.Errorf("read retained check evidence %s: %v", path, err)
			continue
		}
		switch filepath.Base(path) {
		case "stdout":
			// check.sh's failing branch (CHECK_RESULT != "pass") writes
			// nothing to stdout — only the stderr diagnostic below — so an
			// empty retained stdout file is the correct evidence here; its
			// presence (a real, readable file at the rendered path) is what
			// this case proves.
			sawStdout = true
		case "stderr":
			sawStderr = true
			if !strings.Contains(string(content), "not pass") {
				t.Errorf("retained stderr %s = %q, want it to name the non-pass CHECK_RESULT content", path, content)
			}
		}
	}
	if !sawStdout || !sawStderr {
		t.Errorf("retained evidence %v does not include both a stdout and a stderr file", evidence)
	}

	state := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM runs WHERE id = '%s';", fx.runID))
	if state != "failed" {
		t.Errorf("runs.state = %q, want \"failed\"", state)
	}
}

// TestRealProcessGitDependentCheckSucceedsInDetachedCheckout proves design
// section 7 step 1's candidate-isolation contract from the other side of
// TestRealProcessFailedCheckRetainsArtifacts: the check pipeline's
// `git worktree add --detach` materialization keeps real git metadata
// available, so a check that itself depends on running inside a git
// checkout (check-git.sh, newFixtureRepoGitDependentCheck) passes exactly
// like the plain deterministic check would.
func TestRealProcessGitDependentCheckSucceedsInDetachedCheckout(t *testing.T) {
	artifacts, server := newFixtureRunEnv(t)
	repo := newFixtureRepoGitDependentCheck(t, artifacts, server, "repo")
	fx := startRun(t, artifacts, server, repo, "submit-valid")

	fx.requireRunState(t, "completed")
	waitForControllerExit(t, fx.controller, 30*time.Second)

	result := runHop(t, fx.env, fx.repo.Root, "status", "-C", fx.repo.Root, "-run", fx.runID)
	if result.ExitCode != 0 {
		t.Fatalf("hop status exit=%d, want 0; stderr=%q", result.ExitCode, result.Stderr)
	}
	detail := parseStatusDetail(result.Stdout)
	if detail["state"] != "completed" {
		t.Fatalf("run state = %q, want \"completed\"; full detail:\n%s", detail["state"], result.Stdout)
	}

	state := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM runs WHERE id = '%s';", fx.runID))
	if state != "completed" {
		t.Errorf("runs.state = %q, want \"completed\"", state)
	}
}

// TestRealProcessSubmoduleRepositoryFailsClearly proves the "submodule
// repository fails clearly" scenario through the real check pipeline.
// Unlike a check script that itself notices a missing path,
// internal/app/usecase_check.go's rejectSubmoduleCandidate scans the
// submitted commit's tree for a gitlink entry BEFORE ever materializing a
// checkout or spawning the check argv (check-submodule.sh, wired as this
// repository's [check] command, never actually runs): the design's export
// contract ("submodules are unsupported") is enforced as its own named
// failure, not left to the check author to discover.
func TestRealProcessSubmoduleRepositoryFailsClearly(t *testing.T) {
	artifacts, server := newFixtureRunEnv(t)
	repo := newFixtureRepoWithSubmodule(t, artifacts, server, "repo")
	fx := startRun(t, artifacts, server, repo, "submit-valid")

	fx.requireRunState(t, "failed")
	waitForControllerExit(t, fx.controller, 30*time.Second)

	result := runHop(t, fx.env, fx.repo.Root, "status", "-C", fx.repo.Root, "-run", fx.runID)
	if result.ExitCode != 0 {
		t.Fatalf("hop status exit=%d, want 0; stderr=%q", result.ExitCode, result.Stderr)
	}
	detail := parseStatusDetail(result.Stdout)
	if detail["state"] != "failed" {
		t.Fatalf("run state = %q, want \"failed\"; full detail:\n%s", detail["state"], result.Stdout)
	}
	if !strings.Contains(detail["last check"], "failed") {
		t.Errorf("last check summary = %q, want it to name a failed check; full detail:\n%s", detail["last check"], result.Stdout)
	}
	if !strings.Contains(detail["detail"], "submodule") || !strings.Contains(detail["detail"], "unsupported") {
		t.Errorf("last check detail = %q, want it to clearly name the unsupported submodule; full detail:\n%s", detail["detail"], result.Stdout)
	}

	// The submodule rejection fires before any checkout is materialized or
	// check argv spawned, so check-submodule.sh never runs and retains no
	// stdout/stderr evidence — asserted explicitly so a regression that
	// starts running the check anyway (and merely happens to still fail) is
	// caught too.
	if evidence := statusDetailValues(result.Stdout, "evidence"); len(evidence) != 0 {
		t.Errorf("retained check evidence = %v, want none: the submodule rejection runs before check-submodule.sh is ever spawned", evidence)
	}

	state := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM runs WHERE id = '%s';", fx.runID))
	if state != "failed" {
		t.Errorf("runs.state = %q, want \"failed\"", state)
	}
}
