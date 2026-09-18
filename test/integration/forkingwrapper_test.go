package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// forkingWrapperSource stands in for "a wrapper that forks the real harness
// after exec" (design section 6's exec-boundary test list): exec'd as the
// resolved "claude" path (preserving the launch claim's pid), it spawns the
// real fixture worker as a CHILD carrying the identical argv and its own
// argv[0] forced to the wrapper's own invocation name (so the child's own
// observed executable identity also reads "claude"), then blocks forever
// itself rather than exiting — the parent (the claimed pid) and the child
// (a different pid, same identity and markers) are both live at once.
const forkingWrapperSource = `package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func diag(format string, args ...any) {
	dir := os.Getenv("HOP_FIXTURE_DIAG_DIR")
	if dir == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "wrapper-diag.txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, format+"\n", args...)
}

func main() {
	real := os.Getenv("HOP_FIXTURE_REAL_WORKER")
	diag("wrapper pid=%d real=%q argv=%v", os.Getpid(), real, os.Args)
	cmd := exec.Command(real, os.Args[1:]...)
	cmd.Args[0] = os.Args[0]
	cmd.Env = os.Environ()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		diag("child start failed: %v", err)
		os.Exit(1)
	}
	diag("child started pid=%d", cmd.Process.Pid)
	// A bare "select {}" here would trip Go's own deadlock detector (no
	// other goroutine could ever wake it) and crash the wrapper — waiting
	// on the child is both correct (keeps the parent alive exactly as long
	// as the child lives, the fixture worker's own idle() loop with no
	// natural exit) and realistic wrapper behavior.
	err := cmd.Wait()
	diag("child exited: %v", err)
}
`

// buildForkingWrapper compiles forkingWrapperSource for the calling test.
func buildForkingWrapper(t *testing.T, artifacts *artifactDir) string {
	t.Helper()
	src := artifacts.dir(t, "forking-wrapper-src")
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module forkingwrapper\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(forkingWrapperSource), 0o600); err != nil {
		t.Fatal(err)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go tool is required to build the forking wrapper: %v", err)
	}
	binDir := artifacts.dir(t, "forking-wrapper-bin")
	out := filepath.Join(binDir, "wrapper")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, goBin, "build", "-o", out, ".") //nolint:gosec // G204: the go tool builds this test's own generated fixture module.
	build.Dir = src
	if combined, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("go build forking wrapper: %v\n%s", buildErr, combined)
	}
	return out
}

// TestRealProcessForkingWrapperAfterExec proves design section 6's
// forking-wrapper fail-closed rule: forkingWrapperSource is installed as
// the "claude" stub, execs preserving the launch claim's pid, then forks
// the real fixture worker as a child carrying the identical argv and its
// own argv[0] forced to the wrapper's own invocation name. Herdr's
// pane.process_info reports the CHILD as foreground[0] — a different pid
// than the claim, despite matching executable identity and marker — which
// is exactly the executable-identity-and-marker-match-but-pid-differs
// shape CorroborateSettlement classifies SettlementForkingWrapper: the
// claim never settles, and CorroborateLaunch reports LaunchNeedsInteraction.
func TestRealProcessForkingWrapperAfterExec(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	worker := buildFixtureWorker(t)
	realWorkerPath := filepath.Join(artifacts.dir(t, "real-worker-bin"), "claude-real")
	copyExecutable(t, worker, realWorkerPath)
	wrapper := buildForkingWrapper(t, artifacts)
	installFixtureWorkerAsClaudeStub(t, server, wrapper)
	diagDir := artifacts.dir(t, "wrapper-diag")
	server.extraEnv = append(server.extraEnv,
		"HOP_FIXTURE_REAL_WORKER="+realWorkerPath,
		"HOP_FIXTURE_DIAG_DIR="+diagDir)
	server.start(t)

	repo := newFixtureRepo(t, artifacts, server, "repo")
	fx := startRun(t, artifacts, server, repo, "")

	if !waitUntil(func() bool {
		row := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT count(*) FROM launch_claims WHERE run_id = '%s';", fx.runID))
		return row != "0"
	}) {
		t.Fatalf("no launch claim ever appeared for run %s", fx.runID)
	}
	claimPID := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT pid FROM launch_claims WHERE run_id = '%s';", fx.runID))

	// Let the controller run several corroboration rounds (loopPollInterval
	// 2s in production) against the live fork before asserting: a single
	// round settling exec_pending would be unremarkable (ambiguous is the
	// default), but the claim STAYING exec_pending across repeated rounds
	// while both the parent and a matching-identity child are observably
	// alive is the fail-closed signature this scenario exists to prove.
	// waitUntilDeadline returning false here (never leaving exec_pending) is
	// the expected, asserted-below outcome, not a timeout failure.
	waitUntilDeadline(10*time.Second, func() bool {
		state := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM launch_claims WHERE run_id = '%s';", fx.runID))
		return state != "exec_pending"
	})
	claimState := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM launch_claims WHERE run_id = '%s';", fx.runID))
	if claimState != "exec_pending" {
		t.Fatalf("launch claim state = %q, want it to stay \"exec_pending\" (fail-closed against the forking-wrapper topology); diagnostic:\n%s", claimState, mustReadFile(t, filepath.Join(diagDir, "wrapper-diag.txt")))
	}

	result := runHop(t, fx.env, fx.repo.Root, "status", "-C", fx.repo.Root, "-run", fx.runID)
	detail := parseStatusDetail(result.Stdout)
	if detail["state"] != "launching" {
		t.Errorf("run state = %q, want unchanged \"launching\" (never settled)", detail["state"])
	}
	if detail["launch claim"] != "exec_pending" {
		t.Errorf("hop status launch claim = %q, want \"exec_pending\"", detail["launch claim"])
	}

	stdout := readControllerLog(t, fx.artifacts, "run")
	if !strings.Contains(stdout, "needs interaction") {
		t.Errorf("controller stdout never reports needs interaction; got:\n%s", stdout)
	}

	// Independent confirmation of WHY: Herdr's own foreground[0] for this
	// pane is the forked child, a different pid than the claim, even though
	// its executable identity and marker both match.
	info := fx.server.processInfo(t, paneIDFromBinding(detail["binding"]))
	if len(info.ForegroundProcesses) == 0 {
		t.Fatalf("pane reports no foreground process at all")
	}
	fg := info.ForegroundProcesses[0]
	if fmt.Sprint(fg.PID) == claimPID {
		t.Errorf("foreground[0] pid = %d, want it to differ from the claim pid %s (the forked child, not the claimed parent)", fg.PID, claimPID)
	}
	if fg.Argv0 != "claude" {
		t.Errorf("foreground[0] argv0 = %q, want \"claude\" (the spoofed identity the predicate matches on)", fg.Argv0)
	}
}

// mustReadFile reads path for a diagnostic message, tolerating absence.
func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path) //nolint:gosec // G304: a path this test constructed itself, for failure diagnostics only.
	if err != nil {
		return fmt.Sprintf("(unavailable: %v)", err)
	}
	return string(content)
}
