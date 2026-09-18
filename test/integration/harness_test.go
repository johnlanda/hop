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
	"strings"
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

// dir returns a subdirectory of this test's evidence root, creating it if
// needed, for content that is not a single named file — a fixture
// repository's working tree, a built binary's own directory. It shares the
// artifact directory's retention lifecycle (kept on failure, removed on
// success) instead of t.TempDir's unconditional removal, so a scenario
// failure leaves the repository state behind for inspection too.
func (a *artifactDir) dir(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(a.path, name)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
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
	// extraEnv is appended to environ()'s hermetic base. It exists only for
	// task 6b's exec-boundary tests, which deliberately seed a known, fake
	// credential-shaped variable into the server's own environment (never a
	// real credential) to prove the launcher's strip matrix removes it even
	// when the value IS present in the inherited environment, matching
	// section 6's "Exec-boundary tests" contract exactly.
	extraEnv []string
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
//
// Documented limitations:
//
// Cleanup targets the owned process group. A descendant that deliberately
// leaves that group is outside this harness's retirement guarantee; a retained
// output descriptor causes bounded capture failure, not proof that the escaped
// process was retired.
//
// A teardown deadline reports retirement as inconclusive. If the operating
// system does not complete a signaled process's exit, its sole Wait owner may
// remain pending; the harness neither claims successful cleanup nor signals a
// potentially recycled group to force completion.
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

// testConfig renders the test-only configuration for a server rooted at
// base: no onboarding, fixed headless geometry for repeatable pane
// snapshots, nesting allowed so a client may attach inside the test server,
// an Agent sidebar layout that renders HOP's own metadata tokens so the PTY
// smoke can assert row rendering, and an explicit `[worktrees] directory`
// pinned under base. The sidebar rows affect only an attached client's
// rendering, so they are inert for the API-only tests. Missing token values
// simply disappear.
//
// The worktrees pin matters for every server, not only the live test:
// Herdr's own default (`~/.herdr/worktrees`,
// repos/herdr/src/config/model.rs's `impl Default for WorktreesConfig`)
// expands against HOME, and the live scenario deliberately sets HOME to the
// operator's real home directory (see the live-scenario notes in
// docs/architecture/native-harness-compat.md and TestLiveClaudeDefaultProfileRun),
// so an unpinned config would create real worktrees under the operator's
// actual ~/.herdr/worktrees. base is an absolute path, so Herdr's own tilde
// expansion (`expand_tilde_absolute_path`, repos/herdr/src/worktree.rs)
// returns it unchanged regardless of HOME.
func testConfig(base string) string {
	return "onboarding = false\n" +
		"\n[server]\nheadless_cols = 100\nheadless_rows = 30\n" +
		"\n[experimental]\nallow_nested = true\n" +
		"\n[ui.sidebar.agents]\nrows = [[\"state_icon\", \"agent\", \"$hop_role\"], [\"$hop_run\", \"$hop_task\"]]\n" +
		"\n[worktrees]\ndirectory = \"" + filepath.Join(base, "worktrees") + "\"\n"
}

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
	for _, dir := range []string{"c/herdr", "r", "st", "work", "home", "bin", "cache", "data", "worktrees"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "c", "herdr", "config.toml"), []byte(testConfig(base)), 0o600); err != nil {
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
		writeHarnessStub(t, filepath.Join(binDir, name), []byte(script))
	}
}

// replaceHarnessStub removes whatever currently occupies a harness stub path,
// so its caller can create the path afresh rather than write into it.
//
// Every writer of a stub path in this package goes through here, and the
// invariant that requires it is this: NO PATH THAT LINKS TO A SHARED BINARY
// IS EVER WRITTEN IN PLACE. A stub path may be a hard link to a binary shared
// by every test in the process — linkHarnessStub installs the fixture worker
// that way, because a link shares the target's image and costs what running
// the target again costs, while a copy is a new image and pays a full first
// execution. Writing such a path in place writes THROUGH the link to the
// shared file, and every later test in the process then executes whatever the
// writer left behind. Removing the name first breaks the link, never the
// file; installRealClaudeStub's symlink relies on the same property.
func replaceHarnessStub(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove existing %s stub: %v", filepath.Base(path), err)
	}
}

// writeHarnessStub installs content as an executable stub, replacing whatever
// occupied the path.
func writeHarnessStub(t *testing.T, path string, content []byte) {
	t.Helper()
	replaceHarnessStub(t, path)
	if err := os.WriteFile(path, content, 0o755); err != nil { //nolint:gosec // G306: a stub must be executable; it lives in this test's private roots.
		t.Fatalf("install %s stub: %v", filepath.Base(path), err)
	}
}

// linkSharedBinary makes path another name for target's image rather than a
// duplicate of it, replacing whatever occupied path.
//
// What the link buys depends on the target, not on this call. Where target is
// one of the binaries built and warmed once per process, every name linked to
// it executes that already-evaluated image and costs a small fraction of a
// first execution. Where target is a per-test build — a caller staging its own
// wrapper as the harness — there is no warmth to inherit and the first
// execution is paid once regardless; linking there only avoids copying the
// image.
//
// Either way the name becomes another entry for one file, which is why
// replaceHarnessStub exists: a later writer must unlink such a path, never
// write through it.
//
// It falls back to a copy for one expected reason only — target living on
// another filesystem, where no hard link can exist — and fails on anything
// else. A copy is correct but pays a full first execution, so a blanket
// fallback would quietly restore the per-test cost this linking exists to
// remove and leave no signal that the mechanism had stopped working. A link
// this machine cannot make is worth learning about once, loudly.
func linkSharedBinary(t *testing.T, path, target string) {
	t.Helper()
	replaceHarnessStub(t, path)
	err := os.Link(target, path)
	switch {
	case err == nil:
		return
	case errors.Is(err, syscall.EXDEV):
		copyExecutable(t, target, path)
	default:
		t.Fatalf("link %s to %s: %v", filepath.Base(path), filepath.Base(target), err)
	}
}

// linkHarnessStub points a harness stub path at target, sharing its image.
func linkHarnessStub(t *testing.T, path, target string) {
	t.Helper()
	linkSharedBinary(t, path, target)
}

// sharedBinaryFingerprint is what a shared binary looked like the instant it
// was built and warmed.
type sharedBinaryFingerprint struct {
	size    int64
	modTime time.Time
}

// fingerprintSharedBinary records path's current size and modification time.
func fingerprintSharedBinary(path string) (sharedBinaryFingerprint, error) {
	info, err := os.Stat(path)
	if err != nil {
		return sharedBinaryFingerprint{}, err
	}
	return sharedBinaryFingerprint{size: info.Size(), modTime: info.ModTime()}, nil
}

// checkSharedBinaryIntact returns a description of a violated invariant, or
// the empty string. The invariant is the one every stub writer depends on: no
// path that links to a shared binary is ever written in place. A stub path may
// be a hard link to this file, so a writer that truncated its own path instead
// of unlinking it first would have rewritten THIS file, and the symptom would
// otherwise be an exec failure in whichever unrelated test happened to run
// afterwards — an order-dependent mystery rather than a diagnosis.
//
// It must run before the shared directories are removed, since removing them
// destroys the one piece of evidence worth having.
func checkSharedBinaryIntact(name, path string, want sharedBinaryFingerprint) string {
	if path == "" {
		return ""
	}
	got, err := fingerprintSharedBinary(path)
	if err != nil {
		return fmt.Sprintf("the shared %s binary can no longer be read (%v); a stub path linking to it was replaced in place. The invariant: no path that links to a shared binary is ever written in place — stub writers must go through replaceHarnessStub", name, err)
	}
	if got.size != want.size || !got.modTime.Equal(want.modTime) {
		return fmt.Sprintf("the shared %s binary changed after it was built (size %d then %d): a stub path linking to it was written in place. The invariant: no path that links to a shared binary is ever written in place — stub writers must go through replaceHarnessStub", name, want.size, got.size)
	}
	return ""
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
	base := []string{
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
	return append(base, s.extraEnv...)
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
	sp := startAnchoredLeader(t, cmd, stdout, stderr)
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

// startAnchoredLeader starts cmd as its own process-group leader and anchors
// that group immediately (startAnchor), wrapping both as a serverProcess —
// the ownership discipline every owned process-group leader in this suite
// shares, whether it fronts the herdr server, a spike fixture leader, or a
// hop subcommand under test. It registers no cleanup itself; the caller
// decides its own teardown (the server's graceful server.stop-then-retire, a
// bare retire, or a signal-driven test).
func startAnchoredLeader(t *testing.T, cmd *exec.Cmd, stdout, stderr *os.File) *serverProcess {
	t.Helper()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		// The log files, if any, were created before the failed Start; close
		// them here so a start failure does not leak descriptors (no cleanup is
		// registered yet at this point).
		closeLogs(t, stdout, stderr)
		t.Fatalf("start %s: %v", cmd.Path, err)
	}
	// Setpgid made the leader a group leader, so its pgid equals its pid; it is
	// captured once, now, while the pid is certainly the live leader. Launch an
	// anchor into that group immediately so an unreaped member of ours pins the
	// pgid until retirement; if the anchor cannot join, the group is already
	// gone (the leader died at once) and the launch is inconclusive.
	anchor, err := startAnchor(cmd.Process.Pid)
	if err != nil {
		// The leader started but could not be anchored (e.g. sleep unresolved);
		// it is not necessarily dead and has no Wait owner or cleanup registered
		// yet, so retire and reap it here before failing — no live leader, no
		// unreaped pid. Do not infer leader exit from an anchor error.
		retireUnanchoredLeader(t, cmd)
		closeLogs(t, stdout, stderr)
		t.Fatalf("anchor could not join %s's process group %d: %v", cmd.Path, cmd.Process.Pid, err)
	}
	return newServerProcess(cmd, anchor, stdout, stderr)
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
		s.failRestart(t, current, "herdr did not exit within the graceful-stop deadline; restore evidence would be inconclusive")
	}
	// The leader exited on its own, so the shutdown save completed. Verify it
	// was a clean exit (code 0), not a signal or an error, before trusting the
	// restored state.
	status := current.leaderCmd.ProcessState
	switch {
	case current.leaderWaitErr != nil && status == nil:
		s.failRestart(t, current, fmt.Sprintf("herdr leader wait failed during graceful restart (%v); inconclusive", current.leaderWaitErr))
	case !status.Exited():
		s.failRestart(t, current, fmt.Sprintf("herdr exited via signal during graceful restart (%v); the save may be incomplete (inconclusive)", status))
	case status.ExitCode() != 0:
		s.failRestart(t, current, fmt.Sprintf("herdr graceful exit code %d (nonzero); the save may be incomplete (inconclusive)", status.ExitCode()))
	}
	// Retire the still-anchored group: the anchor pins the pgid, so the single
	// SIGKILL that clears any lingering pane shells is safe even though the
	// leader is already reaped. A failed retirement is fatal — the old group
	// may still hold artifact files or a live process, so never relaunch.
	if err := current.retireGroup(); err != nil {
		closeLogs(t, current.stdout, current.stderr)
		t.Fatalf("old server group did not retire after graceful exit (%v); restore evidence would be inconclusive", err)
	}
	closeLogs(t, current.stdout, current.stderr)
	s.start(t)
}

// failRestart retires the old group and fails the restart as inconclusive; the
// retirement error, if any, is folded into the message so a lingering group is
// never silently followed by a relaunch.
func (*testServer) failRestart(t *testing.T, sp *serverProcess, reason string) {
	t.Helper()
	retireErr := sp.retireGroup()
	closeLogs(t, sp.stdout, sp.stderr)
	if retireErr != nil {
		t.Fatalf("%s; retirement also failed: %v", reason, retireErr)
	}
	t.Fatal(reason)
}

// reapServer is the cleanup teardown, run at most once per server, so restart
// and the stacked cleanups never tear one down twice. As a cleanup it reports a
// retirement failure but does not stop other cleanups.
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
	if err := sp.retireGroup(); err != nil {
		t.Errorf("retire server group on cleanup: %v", err)
	}
	closeLogs(t, sp.stdout, sp.stderr)
}

// retireGroup tears the server's whole process group down safely and reports
// the first failure it hit. The single group SIGKILL is sent while the anchor
// is provably unreaped, so it targets only this group's members and never a
// recycled pgid; only afterward are the anchor and leader reaped (each by
// exactly one owner). It must run at most once per server (its callers hold
// takeTeardown). A non-nil error means retirement could not be established;
// restart treats that as inconclusive and must not relaunch.
func (sp *serverProcess) retireGroup() error {
	var firstErr error
	record := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	// One SIGKILL to the whole group. The anchor is still unreaped here, so the
	// pgid is pinned to our group; this kills the leader (if alive), the anchor,
	// and every descendant.
	if safeToSignalGroup(sp.pgid, syscall.Getpgrp()) {
		if err := syscall.Kill(-sp.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			record(fmt.Errorf("kill process group %d: %w", sp.pgid, err))
		}
	} else {
		record(fmt.Errorf("refusing to signal unsafe process group %d", sp.pgid))
	}
	// Observe the leader's exit (reaped by its single owning goroutine) and reap
	// the anchor (its single owner here), each bounded so a stuck reap cannot
	// hang teardown. On a timeout the sole Wait owner is left in place — no
	// second waiter, no post-anchor-reap group signal — and retirement is
	// reported inconclusive. No group signal is sent after this point.
	select {
	case <-sp.leaderExited:
	case <-time.After(ownedGroupDrainTimeout):
		record(fmt.Errorf("leader %d did not exit within the teardown deadline (inconclusive)", sp.pgid))
	}
	if err := reapCmdBounded(sp.anchorCmd, ownedGroupDrainTimeout); err != nil {
		record(fmt.Errorf("reap group anchor %d: %w", sp.pgid, err))
	}
	// Confirm the group is empty: signal 0 only reads existence, so even if the
	// now-unpinned pgid were recycled this cannot signal an unrelated process;
	// it just waits until no member with that pgid remains.
	if !groupIsGone(sp.pgid) {
		record(fmt.Errorf("process group %d still had members after the deadline; a descendant may still hold artifact files", sp.pgid))
	}
	return firstErr
}

// retireUnanchoredLeader kills and reaps a just-started leader whose group
// could not be anchored. The leader has no Wait owner yet and is still
// unreaped, so signaling its own pgid is safe; this leaves no live process and
// no unreaped pid before the caller fails. Its single Wait is bounded, so this
// cleanup path cannot hang either.
func retireUnanchoredLeader(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	pgid := cmd.Process.Pid
	if safeToSignalGroup(pgid, syscall.Getpgrp()) {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Logf("kill unanchored leader group %d: %v", pgid, err)
		}
	} else {
		t.Errorf("refusing to signal unsafe process group %d", pgid)
	}
	if err := reapCmdBounded(cmd, ownedGroupDrainTimeout); err != nil {
		t.Errorf("retire unanchored leader %d: %v", pgid, err)
	}
	if !groupIsGone(pgid) {
		t.Errorf("unanchored leader group %d still had members after teardown", pgid)
	}
}

// groupIsGone reports whether no process remains in the group: signal 0 returns
// ESRCH only once every member, including reparented descendants, has exited
// and been reaped. It polls until the bounded deadline. Signal 0 sends nothing,
// so even if the now-unpinned pgid were recycled this only reads existence.
func groupIsGone(pgid int) bool {
	return waitUntil(func() bool { return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) })
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
	// The staging directory must be fresh per test — that is the shape these
	// scenarios register with herdr — but the executable inside it need not
	// be: it is byte-for-byte the build buildHopBinary already made and
	// warmed, so linking shares that image instead of producing a new one
	// herdr would then execute for the first time. Nothing writes to this
	// path afterwards, and the per-test cleanup below unlinks the name
	// without touching the shared file.
	binDir := filepath.Join(stage, ".bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkSharedBinary(t, filepath.Join(binDir, "hop"), buildHopBinary(t))
	copyFile(t, filepath.Join(root, "herdr-plugin.toml"), filepath.Join(stage, "herdr-plugin.toml"))
	return stage
}

// warmExecutable runs a just-built binary once, for the execution itself
// rather than for anything it does, and discards the outcome.
//
// The first execution of a newly written executable costs far more than
// any later execution of that same file: the operating system evaluates
// the new image once, through a single machine-wide service, so the cost
// rises with how many processes anywhere are starting never-before-seen
// binaries at that moment. Copying a warmed binary does not inherit the
// warmth — a copy is a new image and pays in full — but a hard link to it
// does, being the same image under another name.
//
// Without this call the whole of that cost lands on whichever run executes
// the binary first, inside whatever budget that run is being held to, and
// a test measures the loader instead of the fixture. Warming here spends it
// once, deliberately, where no deadline is running.
//
// The environment is emptied so no behavior keyed off HOP_* or FAKE_HOP_*
// can observe this invocation, and args must select a mode that returns
// immediately without spawning children.
func warmExecutable(t *testing.T, path string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	warm := exec.CommandContext(ctx, path, args...) //nolint:gosec // G204: a binary this suite just built, with arguments this suite chose.
	warm.Env = []string{}
	// Reaching EOF on stdin is what ends the stand-in mode the fixture
	// worker is warmed through; an empty reader gives it one immediately.
	warm.Stdin = strings.NewReader("")
	_ = warm.Run() //nolint:errcheck // only the exec matters here: the fake hop stub refuses an empty argv by design, and that refusal carries no information this warm-up wants.
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
