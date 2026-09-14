// Package integration proves HOP against a real, disposable Herdr server.
// Every test spawns its own named-session server on temporary config, state
// and runtime roots with a test-only config path and no inherited HERDR_*
// environment, so the developer's live session is never addressed. The suite
// skips with an explicit reason when no herdr binary is installed; a skip is
// never a pass, and CI runners without the binary do not run it.
package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/herdr"
)

// callTimeout bounds one request against the test server.
const callTimeout = 30 * time.Second

// pollInterval is the pace of bounded condition polls.
const pollInterval = 25 * time.Millisecond

// conditionTimeout bounds a polled wait for a live condition (a plugin log
// completing, a pane or sidebar rendering the expected text).
const conditionTimeout = 60 * time.Second

// sessionName is the disposable server's named session. It stays one
// character so the socket path fits the platform's Unix socket length cap.
const sessionName = "s"

// requireHerdr returns the herdr binary to test against, or skips: hosted CI
// runners have no herdr installation, and the guide documents that the
// real-process suite did not run there. HOP_TEST_HERDR_BIN pins a binary.
func requireHerdr(t *testing.T) string {
	t.Helper()
	if pinned := os.Getenv("HOP_TEST_HERDR_BIN"); pinned != "" {
		if _, err := os.Stat(pinned); err != nil { //nolint:gosec // G703: the operator chose this path to pin the binary under test.
			t.Fatalf("HOP_TEST_HERDR_BIN: %v", err)
		}
		return pinned
	}
	path, err := exec.LookPath("herdr")
	if err != nil {
		t.Skipf("real-process suite skipped, not run: no herdr binary on PATH and HOP_TEST_HERDR_BIN is unset (%v)", err)
	}
	return path
}

// moduleRoot locates the repository root by walking up to go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test working directory")
		}
		dir = parent
	}
}

// artifactDir collects evidence for one test: server output, plugin logs and
// pane snapshots. It is deleted on success and retained on failure.
type artifactDir struct {
	path string
}

// newArtifactDir creates the evidence directory for one test.
func newArtifactDir(t *testing.T) *artifactDir {
	t.Helper()
	base := filepath.Join(os.TempDir(), "hop-integration")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := os.MkdirTemp(base, sanitizeName(t.Name())+"-")
	if err != nil {
		t.Fatal(err)
	}
	artifacts := &artifactDir{path: path}
	t.Cleanup(func() {
		if retainArtifacts(t.Failed()) {
			t.Logf("artifacts retained at %s", path)
			return
		}
		removeArtifactDir(t, path)
	})
	return artifacts
}

// retainArtifacts decides whether a test's evidence directory is kept: a
// failing run retains it for inspection, a passing run leaves nothing.
func retainArtifacts(failed bool) bool {
	return failed
}

// removeArtifactDir removes a passing test's artifact directory, retrying
// briefly. Server teardown reaps the process group before this runs, but a
// descendant that is a hair slow to exit could still hold a log file for a
// moment; a bounded retry closes that window so a passing run leaves nothing.
func removeArtifactDir(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := os.RemoveAll(path)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("remove artifacts %s: %v", path, err)
			return
		}
		time.Sleep(pollInterval)
	}
}

// sanitizeName makes a test name usable as a directory component.
func sanitizeName(name string) string {
	sanitized := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			sanitized = append(sanitized, r)
		default:
			sanitized = append(sanitized, '_')
		}
	}
	return string(sanitized)
}

// save writes one evidence file.
func (a *artifactDir) save(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(a.path, name), []byte(content), 0o600); err != nil {
		t.Logf("save artifact %s: %v", name, err)
	}
}

// create opens one evidence file for streaming writes.
func (a *artifactDir) create(t *testing.T, name string) *os.File {
	t.Helper()
	file, err := os.Create(filepath.Join(a.path, name)) //nolint:gosec // G304: the path is inside this test's own artifact directory.
	if err != nil {
		t.Fatal(err)
	}
	return file
}

// testServer is one disposable named-session Herdr server on temporary
// roots, plus the environment every subprocess addressing it must use.
type testServer struct {
	herdrBin   string
	base       string
	configHome string
	socketPath string
	config     string
	// shell overrides the SHELL every subprocess (and therefore every pane
	// login shell) uses; empty means the default /bin/sh. The spike tests set
	// it to exercise zsh login-shell behavior.
	shell string
	// running holds every server process the suite has launched on these
	// roots, newest last. A restart (S3) launches a second one; each is
	// reaped exactly once, by restart or by cleanup, so a graceful restart
	// never leaves a stray server or a doubly-signaled group.
	running   []*serverProcess
	client    *herdr.Client
	artifacts *artifactDir
}

// serverProcess is one launched herdr server plus a group anchor. The leader
// is a group leader (Setpgid, pgid == leader pid); the anchor is a tiny
// long-lived process launched into that SAME group and kept unreaped until
// retirement.
//
// The anchor is what makes group signaling safe. A process group's id stays
// reserved (its number is not recycled as a pid, and no new group is assigned
// that id) as long as the group has an unreaped member. So while the anchor
// lives unreaped, kill(-pgid, …) provably targets only this server's group,
// even after the leader itself has been reaped. That decouples reaping from
// signaling: the leader is reaped by exactly one owner — a single goroutine's
// leaderCmd.Wait, whose result closes leaderExited — at any time, with no
// recycled-pgid window, because the anchor holds the group. The anchor is
// reaped exactly once, by retireGroup, and only after the group's single
// SIGKILL has been sent.
type serverProcess struct {
	leaderCmd      *exec.Cmd
	anchorCmd      *exec.Cmd
	pgid           int
	stdout, stderr *os.File

	leaderExited  chan struct{} // closed once the single leader Wait has returned
	leaderWaitErr error         // valid once leaderExited is closed

	mu       sync.Mutex
	torndown bool // teardown (retire + close logs) has completed
}

// takeTeardown returns true exactly once per server: the caller that gets true
// owns teardown, later callers get false and skip.
func (sp *serverProcess) takeTeardown() bool {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.torndown {
		return false
	}
	sp.torndown = true
	return true
}

// testConfig is the test-only configuration: no onboarding, fixed headless
// geometry for repeatable pane snapshots, nesting allowed so a client may
// attach inside the test server, and an Agent sidebar layout that renders
// HOP's own metadata tokens so the PTY smoke can assert row rendering. The
// sidebar rows affect only an attached client's rendering, so they are inert
// for the API-only tests. Missing token values simply disappear.
const testConfig = "onboarding = false\n" +
	"\n[server]\nheadless_cols = 100\nheadless_rows = 30\n" +
	"\n[experimental]\nallow_nested = true\n" +
	"\n[ui.sidebar.agents]\nrows = [[\"state_icon\", \"agent\", \"$hop_role\"], [\"$hop_run\", \"$hop_task\"]]\n"

// newServerRoots prepares the temporary config/state/runtime roots and the
// test-only config file. The base directory is created outside t.TempDir
// because macOS caps Unix socket paths at 104 bytes.
func newServerRoots(t *testing.T) (base string) {
	t.Helper()
	base, err := os.MkdirTemp("", "hop-it")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Logf("remove server roots: %v", err)
		}
	})
	for _, dir := range []string{"c/herdr", "r", "st", "work", "home", "bin", "cache", "data"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "c", "herdr", "config.toml"), []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	writeHarnessStubs(t, filepath.Join(base, "bin"))
	return base
}

// writeHarnessStubs installs harmless stub executables named for the native
// harnesses in a directory that is prepended to the fixture PATH, so a plugin
// action that probes `claude`, `codex` or `opencode` resolves these instead of
// the developer's real, credential-bearing harness binaries. The stubs just
// print a recognizable fake version.
func writeHarnessStubs(t *testing.T, binDir string) {
	t.Helper()
	for _, name := range []string{"claude", "codex", "opencode"} {
		script := "#!/bin/sh\necho \"" + name + " 0.0.0-stub\"\n"
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil { //nolint:gosec // G306: a stub must be executable; it lives in this test's private roots.
			t.Fatal(err)
		}
	}
}

// environ builds the hermetic environment for one subprocess: temporary
// roots, the test-only config path, and no inherited HERDR_* values at all.
// The allowlist is built from scratch, so socket, session and caller
// variables from the developer's live session cannot leak in. HOME is a
// directory inside the temp roots, not the developer's real home, so the
// server and the login shells Herdr opens in panes source only the temp
// home's startup files and cannot reach the developer's native-harness
// state or shell profile.
func (s *testServer) environ() []string {
	// The stub-harness directory is first on PATH so a probe of claude/codex/
	// opencode resolves the harmless stubs, never the developer's real
	// harness binaries; herdr, go and the shell resolve from the inherited
	// PATH after it.
	shell := s.shell
	if shell == "" {
		shell = "/bin/sh"
	}
	return []string{
		"PATH=" + filepath.Join(s.base, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + s.homeDir(),
		"TMPDIR=" + os.Getenv("TMPDIR"),
		"SHELL=" + shell,
		"XDG_CONFIG_HOME=" + filepath.Join(s.base, "c"),
		"XDG_STATE_HOME=" + filepath.Join(s.base, "st"),
		"XDG_RUNTIME_DIR=" + filepath.Join(s.base, "r"),
		"XDG_CACHE_HOME=" + filepath.Join(s.base, "cache"),
		"XDG_DATA_HOME=" + filepath.Join(s.base, "data"),
		"HERDR_CONFIG_PATH=" + s.config,
	}
}

// homeDir is the temp home the server and its pane shells use.
func (s *testServer) homeDir() string {
	return filepath.Join(s.base, "home")
}

// prepareServer lays out roots and the command for one disposable server
// without starting it, so registry writes can happen while it is down.
func prepareServer(t *testing.T, artifacts *artifactDir) *testServer {
	t.Helper()
	herdrBin := requireHerdr(t)
	base := newServerRoots(t)
	configHome := filepath.Join(base, "c")
	server := &testServer{
		herdrBin:   herdrBin,
		base:       base,
		configHome: configHome,
		socketPath: filepath.Join(configHome, "herdr", "sessions", sessionName, "herdr.sock"),
		config:     filepath.Join(configHome, "herdr", "config.toml"),
		artifacts:  artifacts,
	}
	server.client = herdr.NewClient(server.socketPath)
	return server
}

// start launches the named-session server headless and waits for its socket
// to answer ping, with a bounded deadline and no fixed sleeps.
func (s *testServer) start(t *testing.T) {
	t.Helper()
	stdoutName, stderrName := "server-stdout.log", "server-stderr.log"
	if len(s.running) > 0 {
		stdoutName = fmt.Sprintf("server-stdout-%d.log", len(s.running))
		stderrName = fmt.Sprintf("server-stderr-%d.log", len(s.running))
	}
	stdout := s.artifacts.create(t, stdoutName)
	stderr := s.artifacts.create(t, stderrName)
	cmd := exec.CommandContext(context.Background(), s.herdrBin, "--session", sessionName, "server") //nolint:gosec // G204: the binary is the pinned or PATH-resolved herdr under test. The context is deliberately unbounded: stop owns the shutdown through the API and the process handle.
	cmd.Env = s.environ()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = nil
	// Put the server in its own process group so its pane-shell descendants,
	// which inherit the server-log file descriptors in the artifact
	// directory, can be reaped as a group on stop. An orphaned shell that
	// outlived a bare server kill would keep those files open and could
	// leave the artifact directory behind on cleanup.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		// The log files were created before the failed Start; close them here so
		// a start failure does not leak descriptors (no cleanup is registered
		// yet at this point).
		closeLogs(t, stdout, stderr)
		t.Fatalf("start herdr server: %v", err)
	}
	// Setpgid made the server a group leader, so its pgid equals its pid; it is
	// captured once, now, while the pid is certainly the live server. Launch an
	// anchor into that group immediately so an unreaped member of ours pins the
	// pgid until retirement; if the anchor cannot join, the group is already
	// gone (the server died at once) and the launch is inconclusive.
	anchor, err := startAnchor(cmd.Process.Pid)
	if err != nil {
		closeLogs(t, stdout, stderr)
		t.Fatalf("anchor could not join the server's process group %d (server exited at once?): %v", cmd.Process.Pid, err)
	}
	sp := newServerProcess(cmd, anchor, stdout, stderr)
	s.running = append(s.running, sp)
	t.Cleanup(func() { s.reapServer(t, sp) })
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		var pong struct {
			Version string `json:"version"`
		}
		err := s.client.Call(ctx, "ping", nil, &pong)
		cancel()
		if err == nil && pong.Version != "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("herdr server socket %s did not answer ping before the deadline; last error: %v", s.socketPath, err)
		}
		time.Sleep(pollInterval)
	}
}

// gracefulStopTimeout bounds how long a graceful restart waits for the server
// to actually exit after server.stop. The shutdown save of a small session is
// fast; a server that has not exited by this deadline is treated as an
// inconclusive restart, not a successful one.
const gracefulStopTimeout = 20 * time.Second

// startAnchor launches a tiny long-lived process into the existing process
// group pgid, so an unreaped member of ours pins the pgid until retirement.
// setpgid into an existing group fails (EPERM/ESRCH) once that group is empty —
// the leader already exited — which the caller treats as an inconclusive
// launch. The anchor gets no stdio, so it never inherits the artifact-log fds.
func startAnchor(pgid int) (*exec.Cmd, error) {
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		return nil, fmt.Errorf("resolve sleep for the group anchor: %w", err)
	}
	anchor := exec.CommandContext(context.Background(), sleepBin, "100000") //nolint:gosec // G204: a fixed sleep binary; the anchor only pins the process group.
	anchor.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := anchor.Start(); err != nil {
		return nil, err
	}
	return anchor, nil
}

// newServerProcess wraps an already-started leader and its group anchor, and
// starts the single goroutine that owns the leader's one Wait — its result
// closes leaderExited. Nothing else waits on the leader pid; the anchor keeps
// the pgid pinned, so this reap may complete at any time without opening a
// recycled-pgid window.
func newServerProcess(leaderCmd, anchorCmd *exec.Cmd, stdout, stderr *os.File) *serverProcess {
	sp := &serverProcess{
		leaderCmd:    leaderCmd,
		anchorCmd:    anchorCmd,
		pgid:         leaderCmd.Process.Pid,
		stdout:       stdout,
		stderr:       stderr,
		leaderExited: make(chan struct{}),
	}
	go func() {
		sp.leaderWaitErr = sp.leaderCmd.Wait()
		close(sp.leaderExited)
	}()
	return sp
}

// closeLogs closes a server's log files, tolerating nil (a failed start).
func closeLogs(t *testing.T, stdout, stderr *os.File) {
	t.Helper()
	if stdout != nil {
		if err := stdout.Close(); err != nil {
			t.Logf("close server stdout: %v", err)
		}
	}
	if stderr != nil {
		if err := stderr.Close(); err != nil {
			t.Logf("close server stderr: %v", err)
		}
	}
}

// restart gracefully stops the current server and launches a fresh one on the
// same roots, so persisted session state survives across the restart. The
// graceful save runs only as the run loop exits (save_session_on_shutdown), so
// restart waits for the leader to ACTUALLY exit — never merely for ping to
// fail, which Herdr returns while it is still shutting down and before it
// saves. If the server does not exit within the deadline the restart is
// inconclusive: the old server is force-killed and the test fails rather than
// treating a truncated save as restored state.
func (s *testServer) restart(t *testing.T) {
	t.Helper()
	if len(s.running) == 0 {
		t.Fatal("restart called before start")
	}
	current := s.running[len(s.running)-1]
	if !current.takeTeardown() {
		t.Fatal("restart called on an already-torn-down server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := s.client.Call(ctx, "server.stop", nil, nil); err != nil {
		t.Logf("server.stop before restart: %v", err)
	}
	cancel()
	// Wait for the leader to ACTUALLY exit — never merely for a failed ping,
	// which Herdr returns while it is still shutting down and before the run
	// loop's save. The save runs only as the leader exits.
	select {
	case <-current.leaderExited:
	case <-time.After(gracefulStopTimeout):
		// The leader did not exit on its own; it is still running (and the anchor
		// still pins the group), so retire the group and fail — a forced
		// shutdown's save may be incomplete, never treat it as restored state.
		s.retireGroup(t, current)
		closeLogs(t, current.stdout, current.stderr)
		t.Fatal("herdr did not exit within the graceful-stop deadline; restore evidence would be inconclusive")
	}
	// The leader exited on its own, so the shutdown save completed. Verify it
	// was a clean exit (code 0), not a signal or an error, before trusting the
	// restored state.
	status := current.leaderCmd.ProcessState
	switch {
	case current.leaderWaitErr != nil && status == nil:
		s.retireGroup(t, current)
		closeLogs(t, current.stdout, current.stderr)
		t.Fatalf("herdr leader wait failed during graceful restart (%v); inconclusive", current.leaderWaitErr)
	case !status.Exited():
		s.retireGroup(t, current)
		closeLogs(t, current.stdout, current.stderr)
		t.Fatalf("herdr exited via signal during graceful restart (%v); the save may be incomplete (inconclusive)", status)
	case status.ExitCode() != 0:
		s.retireGroup(t, current)
		closeLogs(t, current.stdout, current.stderr)
		t.Fatalf("herdr graceful exit code %d (nonzero); the save may be incomplete (inconclusive)", status.ExitCode())
	}
	// Retire the still-anchored group: the anchor pins the pgid, so the single
	// SIGKILL that clears any lingering pane shells is safe even though the
	// leader is already reaped.
	s.retireGroup(t, current)
	closeLogs(t, current.stdout, current.stderr)
	s.start(t)
}

// reapServer is the cleanup teardown, run at most once per server, so restart
// and the stacked cleanups never tear one down twice.
func (s *testServer) reapServer(t *testing.T, sp *serverProcess) {
	t.Helper()
	if !sp.takeTeardown() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := s.client.Call(ctx, "server.stop", nil, nil); err != nil {
		t.Logf("server.stop: %v (falling back to the process group)", err)
	}
	cancel()
	s.retireGroup(t, sp)
	closeLogs(t, sp.stdout, sp.stderr)
}

// retireGroup tears the server's whole process group down safely. The single
// group SIGKILL is sent while the anchor is provably unreaped, so it targets
// only this group's members and never a recycled pgid; only afterward are the
// anchor and leader reaped (each by exactly one owner). It must run at most
// once per server (its callers hold takeTeardown).
func (s *testServer) retireGroup(t *testing.T, sp *serverProcess) {
	t.Helper()
	// One SIGKILL to the whole group. The anchor is still unreaped here, so the
	// pgid is pinned to our group; this kills the leader (if alive), the anchor,
	// and every descendant.
	if safeToSignalGroup(sp.pgid, syscall.Getpgrp()) {
		if err := syscall.Kill(-sp.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Logf("kill process group %d: %v", sp.pgid, err)
		}
	} else {
		t.Errorf("refusing to signal unsafe process group %d", sp.pgid)
	}
	// Reap the leader (its single owning goroutine) and the anchor (here, its
	// single owner). No group signal is sent after this point.
	<-sp.leaderExited
	if err := sp.anchorCmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) { // a SIGKILLed anchor reports an ExitError; anything else is unexpected
			t.Logf("reap group anchor %d: %v", sp.pgid, err)
		}
	}
	// Confirm the group is empty: signal 0 only reads existence, so even if the
	// now-unpinned pgid were recycled this cannot signal an unrelated process;
	// it just waits until no member with that pgid remains.
	s.awaitGroupGone(t, sp.pgid)
}

// awaitGroupGone waits until no process remains in the group: signal 0 returns
// ESRCH only once every member, including reparented descendants, has exited
// and been reaped. A timeout means a lingering writer could still hold artifact
// files, so it fails the test rather than leaving that unguaranteed.
func (*testServer) awaitGroupGone(t *testing.T, pgid int) {
	t.Helper()
	if !waitUntil(func() bool { return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) }) {
		t.Errorf("process group %d still had members after the deadline; a descendant may still hold artifact files", pgid)
	}
}

// killProcessGroupThenReap SIGKILLs the process group identified by pgid,
// reaps the leader with Wait, and then waits until the whole group is gone.
// The order matters: the leader is signaled while it is still unreaped, so
// pgid cannot have been recycled onto an unrelated process, and only the group
// the suite owns is ever signaled. Waiting for the group to empty matters for
// cleanup: a pane-shell descendant, reparented to init after its SIGKILL,
// still holds inherited file descriptors on the server logs in the artifact
// directory until init reaps it, so removing that directory before the group
// is empty could race those writers.
func killProcessGroupThenReap(t *testing.T, cmd *exec.Cmd, pgid, ownGroup int) {
	t.Helper()
	if !safeToSignalGroup(pgid, ownGroup) {
		t.Errorf("refusing to signal unsafe process group %d", pgid)
		return
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Logf("kill process group %d: %v", pgid, err)
	}
	// Reap the leader. A SIGKILLed process reports a signal error, expected.
	if err := cmd.Wait(); err != nil {
		t.Logf("herdr server exited: %v", err)
	}
	// Wait until no process remains in the group: signal 0 to the group
	// returns ESRCH only once every member, including reparented descendants,
	// has exited and been reaped. A timeout here means a lingering writer
	// could still hold artifact files, so it fails the test rather than
	// leaving a passing run's emptiness unguaranteed.
	if !waitUntil(func() bool { return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) }) {
		t.Errorf("process group %d still had members after the deadline; a descendant may still hold artifact files", pgid)
	}
}

// safeToSignalGroup reports whether pgid is a real, specific process group the
// suite owns and may signal. It rejects any non-positive value, 1 (init), and
// the caller's own process group, so a missing, bogus or reused pgid can never
// turn into a broadcast, a signal to init, or a signal to the test runner's
// own group.
func safeToSignalGroup(pgid, ownGroup int) bool {
	return pgid > 1 && pgid != ownGroup
}

// runCLI runs one herdr CLI command against the test roots and returns its
// combined output. The command addresses only the disposable session.
func (s *testServer) runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.herdrBin, append([]string{"--session", sessionName}, args...)...) //nolint:gosec // G204: the binary is the pinned or PATH-resolved herdr under test and the arguments are chosen by this suite.
	cmd.Env = s.environ()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// stagePlugin builds the hop binary and copies the repository's manifest
// into a temporary plugin directory shaped like the linked working tree:
// the manifest at the root and the binary at .bin/hop.
func stagePlugin(t *testing.T) string {
	t.Helper()
	root := moduleRoot(t)
	stage, err := os.MkdirTemp("", "hop-plug")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if removeErr := os.RemoveAll(stage); removeErr != nil {
			t.Logf("remove staged plugin: %v", removeErr)
		}
	})
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go tool is required to build the plugin binary: %v", err)
	}
	buildCtx, cancelBuild := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, goBin, "build", "-o", filepath.Join(stage, ".bin", "hop"), "./cmd/hop") //nolint:gosec // G204: the go tool builds this repository's own command.
	build.Dir = root
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("go build ./cmd/hop: %v\n%s", buildErr, out)
	}
	copyFile(t, filepath.Join(root, "herdr-plugin.toml"), filepath.Join(stage, "herdr-plugin.toml"))
	return stage
}

// copyFile copies one regular file.
func copyFile(t *testing.T, from, to string) {
	t.Helper()
	source, err := os.Open(from) //nolint:gosec // G304: both paths are chosen by this suite.
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := source.Close(); closeErr != nil {
			t.Errorf("close %s: %v", from, closeErr)
		}
	}()
	destination, err := os.Create(to) //nolint:gosec // G304: both paths are chosen by this suite.
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(destination, source); err != nil {
		t.Fatal(err)
	}
	if err := destination.Close(); err != nil {
		t.Fatal(err)
	}
}

// userGlobalRegistries lists the real user-global plugin registry files the
// suite must never touch. Reading them is the leak check; they are never
// written.
func userGlobalRegistries() []string {
	home := os.Getenv("HOME")
	return []string{
		filepath.Join(home, ".config", "herdr", "plugins.json"),
		filepath.Join(home, ".config", "herdr-dev", "plugins.json"),
	}
}

// registryState is the observed state of one registry file. exists separates
// an absent file from a present one, so absence cannot be confused with a file
// whose contents happen to equal any sentinel string.
type registryState struct {
	exists  bool
	content []byte
}

// registrySnapshot records each user-global registry's state, distinguishing
// absence from emptiness with an explicit exists flag rather than a sentinel
// string that file bytes could collide with.
func registrySnapshot(t *testing.T) map[string]registryState {
	t.Helper()
	snapshot := map[string]registryState{}
	for _, path := range userGlobalRegistries() {
		content, err := os.ReadFile(path) //nolint:gosec // G304: fixed well-known paths under the user's home, read-only.
		if err != nil {
			if !os.IsNotExist(err) {
				t.Fatalf("read %s: %v", path, err)
			}
			snapshot[path] = registryState{exists: false}
			continue
		}
		snapshot[path] = registryState{exists: true, content: content}
	}
	return snapshot
}

// assertNoRegistryLeak fails when any user-global registry changed while the
// test ran: registration is user-global by default in Herdr, so this is the
// proof that temporary roots confined it.
func assertNoRegistryLeak(t *testing.T, before map[string]registryState) {
	t.Helper()
	after := registrySnapshot(t)
	for path, want := range before {
		got := after[path]
		if got.exists != want.exists || !bytes.Equal(got.content, want.content) {
			t.Errorf("user-global registry %s changed during the test; the test registration leaked", path)
		}
	}
}

// waitUntil polls a condition until conditionTimeout, with no fixed sleeps
// beyond the poll interval. It reports whether the condition became true.
func waitUntil(condition func() bool) bool {
	deadline := time.Now().Add(conditionTimeout)
	for {
		if condition() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(pollInterval)
	}
}

// testContext returns a bounded context for adapter calls a test drives
// directly (the presentation and observer adapters take a context). It is
// canceled when the test ends.
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	t.Cleanup(cancel)
	return ctx
}

// call issues one request against the test server, each with its own
// bounded deadline, and fails the test on any error.
func (s *testServer) call(t *testing.T, method string, params, result any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	defer cancel()
	if err := s.client.Call(ctx, method, params, result); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

// pluginLogs fetches the hop plugin's command log records.
func (s *testServer) pluginLogs(t *testing.T) []pluginLogRecord {
	t.Helper()
	var result struct {
		Logs []pluginLogRecord `json:"logs"`
	}
	s.call(t, "plugin.log.list", map[string]any{"plugin_id": "hop", "limit": 50}, &result)
	return result.Logs
}

// pluginLogRecord is the subset of Herdr's plugin command log the suite
// asserts on.
type pluginLogRecord struct {
	LogID    string `json:"log_id"`
	PluginID string `json:"plugin_id"`
	ActionID string `json:"action_id"`
	Event    string `json:"event"`
	Status   string `json:"status"`
	ExitCode *int   `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// waitForLog waits until the identified log record leaves the running state
// and returns it, saving the full log set as evidence on failure.
func (s *testServer) waitForLog(t *testing.T, match func(*pluginLogRecord) bool) pluginLogRecord {
	t.Helper()
	var found pluginLogRecord
	completed := waitUntil(func() bool {
		for _, record := range s.pluginLogs(t) {
			if match(&record) && record.Status != "running" {
				found = record
				return true
			}
		}
		return false
	})
	if !completed {
		logs := s.pluginLogs(t)
		s.artifacts.save(t, "plugin-logs.txt", fmt.Sprintf("%+v", logs))
		t.Fatalf("no matching plugin command completed before the deadline; %d records saved to artifacts", len(logs))
	}
	return found
}
