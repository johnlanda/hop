package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain lets this package share process-lifetime state across tests — the
// single ./cmd/hop build cached below, and the fixture builds cached in their
// own files — and cleans all three up once every test in this binary run has
// finished. The plugin-linking tests still get a fresh plugin-shaped staging
// directory each, but the executable inside it is a link to the shared build
// rather than a build of its own (stagePlugin).
//
// Before that cleanup it checks that no shared binary changed after it was
// built: each one is reachable through paths that link to it, so a writer
// that replaced such a path in place rather than unlinking it first would
// have rewritten the shared file, and the cleanup would then destroy the
// evidence. See replaceHarnessStub for the invariant itself.
func TestMain(m *testing.M) {
	code := m.Run()
	// Checked before the cleanup below, which would destroy the evidence: a
	// shared binary that changed size was written through a stub path that
	// links to it, and the alternative symptom is an exec failure in some
	// unrelated, order-dependent test.
	for _, violation := range []string{
		checkSharedBinaryIntact("fixture worker", fixtureWorkerBinary.path, fixtureWorkerBinary.fingerprint),
		checkSharedBinaryIntact("fake hop", fakeHopBinary.path, fakeHopBinary.fingerprint),
		checkSharedBinaryIntact("hop command", hopBinary.path, hopBinary.fingerprint),
	} {
		if violation == "" {
			continue
		}
		fmt.Fprintln(os.Stderr, violation)
		if code == 0 {
			code = 1
		}
	}
	// Removing these process-scoped MkdirTemp directories is this process
	// tidying up after itself; it is not, and must never become, a precedent
	// for removing a retained failure artifact.
	for _, dir := range []string{hopBinary.dir, fixtureWorkerBinary.dir, fakeHopBinary.dir} {
		if dir != "" {
			_ = os.RemoveAll(dir) //nolint:errcheck // best-effort process-exit cleanup; a leftover temp dir is not a test failure.
		}
	}
	os.Exit(code)
}

// hopBinary caches the composition-root build buildHopBinary produces, so
// every test in one `go test` process shares a single ./cmd/hop build
// instead of paying the build cost per scenario. It is a narrow,
// process-lifetime mutable global guarded by sync.Once and cleaned up by
// TestMain; there is no other way to share build state across independent
// Test functions.
var hopBinary struct { //nolint:gochecknoglobals // process-lifetime build cache guarded by sync.Once; see hopBinary's own doc comment.
	once        sync.Once
	dir         string
	path        string
	err         error
	fingerprint sharedBinaryFingerprint
}

// buildHopBinary builds ./cmd/hop once for every test in this binary run and
// returns the built executable's absolute path. The build itself always
// succeeds throughout task 6b's two phases — cmd/hop is valid Go both before
// and after task 6a lands run/status/stop/resume/result/launch/check-exec —
// so a build failure here is a hard test failure; a subcommand task 6a has
// not landed yet is detected per invocation by requireHopCommand, not here.
func buildHopBinary(t *testing.T) string {
	t.Helper()
	hopBinary.once.Do(func() {
		dir, err := os.MkdirTemp("", "hop-cmd")
		if err != nil {
			hopBinary.err = err
			return
		}
		hopBinary.dir = dir
		goBin, err := exec.LookPath("go")
		if err != nil {
			hopBinary.err = fmt.Errorf("the go tool is required to build cmd/hop: %w", err)
			return
		}
		out := filepath.Join(dir, "hop")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		build := exec.CommandContext(ctx, goBin, "build", "-o", out, "./cmd/hop") //nolint:gosec // G204: the go tool builds this repository's own command with a fixed argument list.
		build.Dir = moduleRoot(t)
		if combined, buildErr := build.CombinedOutput(); buildErr != nil {
			hopBinary.err = fmt.Errorf("go build ./cmd/hop: %w\n%s", buildErr, combined)
			return
		}
		// An empty argv reaches dispatch's usage branch, which writes to
		// stderr and returns at once, touching no state and spawning
		// nothing. Warmed here, with the build, so the first execution is
		// paid once for the process and outside every deadline — the same
		// reason the fixture binaries are warmed where they are built.
		warmExecutable(t, out)
		fingerprint, statErr := fingerprintSharedBinary(out)
		if statErr != nil {
			hopBinary.err = statErr
			return
		}
		hopBinary.fingerprint = fingerprint
		hopBinary.path = out
	})
	if hopBinary.err != nil {
		t.Fatalf("build hop binary: %v", hopBinary.err)
	}
	return hopBinary.path
}

// hopExitUsage mirrors cmd/hop's unexported exitUsage (2): a usage error,
// including the "unknown command" response a not-yet-landed subcommand gets
// before task 6a lands it (cmd/hop/main.go's dispatch).
const hopExitUsage = 2

// hopResult captures one bounded hop subcommand invocation.
type hopResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// FirstStdoutLine returns Stdout's first line (without its terminator), the
// value section 7's transient/stale/duplicate/conflicting outcomes and the
// assignment template's retry instruction key on.
func (r hopResult) FirstStdoutLine() string {
	line, _, _ := strings.Cut(r.Stdout, "\n")
	return line
}

// runHop runs the built hop binary with args and dir as its working
// directory, bounded by callTimeout, and captures stdout/stderr separately
// plus the exit code. It never fails the test on a non-zero exit — the
// caller decides what a given code means for the command it ran — but
// failing to even start the process is a hard test failure.
func runHop(t *testing.T, env []string, dir string, args ...string) hopResult {
	t.Helper()
	hopPath := buildHopBinary(t)
	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, hopPath, args...) //nolint:gosec // G204: the executable is this suite's own build of cmd/hop; args are chosen by the calling test.
	cmd.Env = env
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := hopResult{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		result.ExitCode = exitErr.ExitCode()
	default:
		t.Fatalf("run hop %s: %v", strings.Join(args, " "), err)
	}
	return result
}

// requireHopCommand skips the test with a clear reason when result is
// cmd/hop's usage response for command being unimplemented — task 6a has
// not landed run/status/stop/resume/result/launch/check-exec yet. Any other
// outcome (including a different usage error from an implemented command)
// is left for the caller to assert on; this only recognizes the exact
// "unknown command" shape cmd/hop/main.go's dispatch prints.
func requireHopCommand(t *testing.T, result hopResult, command string) {
	t.Helper()
	if result.ExitCode == hopExitUsage && strings.Contains(result.Stderr, fmt.Sprintf("hop: unknown command %q", command)) {
		t.Skipf("cmd/hop does not implement %q yet; task 6a lands it separately (docs/plan/phase-2-design.md section 10)", command)
	}
}

// hopEnviron builds the environment a hop subcommand runs with: this
// suite's hermetic, built-from-scratch base (environ — no inherited
// HERDR_* or credential variables), an isolated absolute HOP_STATE_DIR
// (section 4: worker-context commands require it provided at pane creation
// and never fall back to a resolved default), and this disposable server's
// own socket and binary path so the command addresses this test server and
// never the developer's live Herdr session.
func (s *testServer) hopEnviron(stateDir string) []string {
	return append(s.environ(),
		"HOP_STATE_DIR="+stateDir,
		"HERDR_SOCKET_PATH="+s.socketPath,
		"HERDR_BIN_PATH="+s.herdrBin,
	)
}

// startHopController starts a long-running hop subcommand (hop run, hop
// resume) as its own anchored process group, capturing stdout/stderr to
// named log files under the test's artifact directory exactly as the herdr
// server does (start), and returns the serverProcess so callers reuse the
// existing process-group helpers (retireGroup, killProcessGroupThenReap,
// groupIsGone, listGroupMembers) to signal, kill or inspect it. Cleanup
// retires the group and closes the logs at most once; driving a graceful
// end first — hop's own `hop stop`, or a detach signal — is the caller's
// responsibility before that forced fallback fires.
func (s *testServer) startHopController(t *testing.T, stateDir, name string, args ...string) *serverProcess {
	t.Helper()
	hopPath := buildHopBinary(t)
	stdout := s.artifacts.create(t, name+"-stdout.log")
	stderr := s.artifacts.create(t, name+"-stderr.log")
	cmd := exec.CommandContext(context.Background(), hopPath, args...) //nolint:gosec // G204: the executable is this suite's own build of cmd/hop; args are chosen by the calling test. The context is deliberately unbounded: the test drives this controller's lifetime explicitly (hop stop, a detach signal, or group teardown), exactly like the server leader above.
	cmd.Env = s.hopEnviron(stateDir)
	cmd.Dir = stateDir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	sp := startAnchoredLeader(t, cmd, stdout, stderr)
	t.Cleanup(func() {
		if sp.takeTeardown() {
			if err := sp.retireGroup(); err != nil {
				t.Errorf("retire hop controller %q group on cleanup: %v", name, err)
			}
			closeLogs(t, sp.stdout, sp.stderr)
		}
	})
	return sp
}

// groupMember is one live process-group member as this suite's own,
// independently implemented inspection sees it. This is deliberately a
// SEPARATE observation path from internal/adapters/process's GroupInspector
// — test/integration may not import internal/adapters/process at all (see
// internal/arch_test.go's rule for this package) — so a scenario proving
// group retirement is evidence against HOP's actual effect on the OS
// process table, never an echo of the port under test sharing its bugs.
type groupMember struct {
	PID     int
	Command string // ps's command column: sufficient for substring assertions, not the frozen-argv authority the real inspector reads exactly.
}

// listGroupMembers independently lists pgid's live members with one ps
// invocation, mirroring the platform tool the real ProcessGroupInspector
// uses but reading the coarser whitespace-joined command column, which is
// sufficient for test assertions on which processes are running. An empty
// result means the group has no member left; a failure to run or parse ps
// is a hard test failure, never silently treated as absence.
func listGroupMembers(t *testing.T, pgid int) []groupMember {
	t.Helper()
	if pgid <= 1 {
		t.Fatalf("process group id %d is not inspectable", pgid)
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,pgid=,command=").Output()
	if err != nil {
		t.Fatalf("list process table: %v", err)
	}
	var members []groupMember
	for line := range strings.Lines(string(out)) {
		trimmed := strings.TrimRight(line, "\n")
		if strings.TrimSpace(trimmed) == "" {
			continue
		}
		pid, rowPgid, command, parseErr := parseGroupMember(trimmed)
		if parseErr != nil {
			t.Fatalf("parse ps row: %v", parseErr)
		}
		if rowPgid == pgid {
			members = append(members, groupMember{PID: pid, Command: command})
		}
	}
	return members
}

// parseGroupMember parses one "ps -A -o pid=,pgid=,command=" row into a pid,
// pgid and command, tolerating ps's padded columns while preserving the
// command field's own internal spacing exactly (strings.Fields would
// collapse repeated spaces inside it).
func parseGroupMember(line string) (pid, pgid int, command string, err error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, 0, "", fmt.Errorf("unparseable ps row %q: want at least a pid and a pgid", line)
	}
	pid, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, "", fmt.Errorf("unparseable pid in ps row %q: %w", line, err)
	}
	pgid, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, "", fmt.Errorf("unparseable pgid in ps row %q: %w", line, err)
	}
	rest := strings.TrimLeft(line, " ")
	rest = strings.TrimPrefix(rest, fields[0])
	rest = strings.TrimLeft(rest, " ")
	rest = strings.TrimPrefix(rest, fields[1])
	command = strings.TrimLeft(rest, " ")
	return pid, pgid, command, nil
}

// TestBuildAndRunHopBinary proves buildHopBinary and runHop against the
// commands cmd/hop already implements (Phase 1's version and doctor), and
// requireHopCommand's skip detection against a command name that will never
// exist, independent of whichever real subcommands task 6a lands.
func TestBuildAndRunHopBinary(t *testing.T) {
	result := runHop(t, os.Environ(), t.TempDir(), "version")
	if result.ExitCode != 0 {
		t.Fatalf("hop version exit=%d, want 0; stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
	if strings.TrimSpace(result.Stdout) == "" {
		t.Error("hop version printed nothing")
	}

	t.Run("an unimplemented command is detected and skipped", func(t *testing.T) {
		result := runHop(t, os.Environ(), t.TempDir(), "obviously-not-a-real-hop-command")
		requireHopCommand(t, result, "obviously-not-a-real-hop-command")
		t.Fatal("requireHopCommand should have skipped this subtest for an unknown command")
	})
}

// TestHopEnviron proves hopEnviron's isolation contract directly, with no
// process execution: the isolated state dir and this disposable server's
// own socket and binary path are present, and the base environment stays
// exactly this suite's hermetic one (no HERDR_* leaked from the current
// process, no provider-credential variable ever present to strip).
func TestHopEnviron(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts) // does not start the server; only resolves paths.
	stateDir := filepath.Join(t.TempDir(), "state")

	env := server.hopEnviron(stateDir)
	want := map[string]string{
		"HOP_STATE_DIR":     stateDir,
		"HERDR_SOCKET_PATH": server.socketPath,
		"HERDR_BIN_PATH":    server.herdrBin,
	}
	for key, value := range want {
		if !slices.Contains(env, key+"="+value) {
			t.Errorf("hopEnviron missing %s=%s; got %v", key, value, env)
		}
	}
	for _, forbidden := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR", "OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_HOME"} {
		for _, entry := range env {
			if strings.HasPrefix(entry, forbidden+"=") {
				t.Errorf("hopEnviron carries the provider-credential variable %s", forbidden)
			}
		}
	}
}

// TestStartHopController proves the anchored-leader mechanics against a
// short-lived Phase 1 command (hop version stands in for a real long-running
// controller, which does not exist until task 6a lands): the process starts
// as its own anchored group, exits cleanly on its own, is reaped without a
// forced kill, and its output is captured to the named log file under the
// test's artifact directory.
func TestStartHopController(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	stateDir := artifacts.dir(t, "state")

	sp := server.startHopController(t, stateDir, "version-check", "version")
	select {
	case <-sp.leaderExited:
	case <-time.After(10 * time.Second):
		t.Fatal("hop version did not exit before the deadline")
	}
	if status := sp.leaderCmd.ProcessState; status == nil || !status.Exited() || status.ExitCode() != 0 {
		t.Errorf("hop version exit status = %v, want a clean exit(0)", status)
	}
	// The leader exiting does not empty the group on its own: the anchor
	// still pins it (that pinning is the whole point — see startAnchor).
	// Retire it explicitly, exactly as the registered cleanup would, before
	// asserting emptiness.
	if sp.takeTeardown() {
		if err := sp.retireGroup(); err != nil {
			t.Errorf("retire hop controller group: %v", err)
		}
		closeLogs(t, sp.stdout, sp.stderr)
	}
	if !sp.groupGone(t) {
		t.Error("hop version's process group is not gone after retirement")
	}

	stdoutPath := filepath.Join(artifacts.path, "version-check-stdout.log")
	content, err := os.ReadFile(stdoutPath) //nolint:gosec // G304: a path this test constructed itself, under its own artifact directory.
	if err != nil {
		t.Fatalf("read captured hop version stdout log: %v", err)
	}
	if strings.TrimSpace(string(content)) == "" {
		t.Error("captured hop version stdout log is empty")
	}
}

// TestListGroupMembers proves this suite's independent process-group
// listing against a fixture leader with a known backgrounded child (no herdr
// binary needed): both appear while alive, with the child's command naming
// its own program, and the group is empty after retirement.
func TestListGroupMembers(t *testing.T) {
	leader := startFixtureLeader(t, "sleep 300 & wait")

	var members []groupMember
	ready := waitUntil(func() bool {
		members = listGroupMembers(t, leader.pgid)
		return len(members) >= 2
	})
	if !ready {
		t.Fatalf("expected at least 2 group members (leader + child), got %d: %+v", len(members), members)
	}
	foundSleep := false
	for _, m := range members {
		if strings.Contains(m.Command, "sleep") {
			foundSleep = true
		}
	}
	if !foundSleep {
		t.Errorf("no group member's command mentions sleep; members=%+v", members)
	}

	if leader.takeTeardown() {
		retireInTest(t, leader)
	}
	if got := listGroupMembers(t, leader.pgid); len(got) != 0 {
		t.Errorf("group members after retirement = %+v, want none", got)
	}
}

// waitUntilDeadline polls condition like waitUntil, but with a caller-chosen
// bound instead of the suite's standard conditionTimeout: a full
// run/launch/corroborate/check round trip against a real herdr server
// legitimately needs longer than most bounded waits in this suite.
func waitUntilDeadline(deadline time.Duration, condition func() bool) bool {
	end := time.Now().Add(deadline)
	for {
		if condition() {
			return true
		}
		if time.Now().After(end) {
			return false
		}
		time.Sleep(pollInterval)
	}
}

// installFixtureWorkerAsClaudeStub replaces the server's stub "claude" on
// its fixture PATH (writeHarnessStubs's harmless echo script, installed by
// prepareServer) with the compiled fixture worker, so hop launch's
// PATH-based harness resolution (section 6) execs the real fixture worker
// instead of the stub. Safe to call any time after prepareServer: PATH
// resolution happens at each launch, not once at server start.
func installFixtureWorkerAsClaudeStub(t *testing.T, server *testServer, workerPath string) {
	t.Helper()
	linkHarnessStub(t, filepath.Join(server.base, "bin", "claude"), workerPath)
}

// extractRunID parses a controller's captured stdout for its first
// "run <label> <uuid> started" line (runcmd.go's runRun) and returns the
// label and run id.
func extractRunID(t *testing.T, content string) (label, runID string) {
	t.Helper()
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 && fields[0] == "run" && fields[3] == "started" {
			return fields[1], fields[2]
		}
	}
	t.Fatalf("controller output does not contain a \"run <label> <uuid> started\" line:\n%s", content)
	return "", ""
}

// parseStatusDetail parses hop status -run's rendered detail block
// (statuscmd.go's renderRunDetail) into field-name -> value, keyed by the
// label preceding each line's first colon, trimmed. It is a plain-text
// scrape of a deliberately human-oriented rendering, not a stable wire
// format, but the rendering is a table-tested, stable command contract, and
// scraping it directly is independent evidence — this suite is not
// importing the DTOs it renders from.
func parseStatusDetail(output string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return fields
}

// paneIDFromBinding extracts the pane id from hop status's rendered
// binding summary, formatted "<workspace>/<workspace>:<tab>/<workspace>:
// <pane>" (e.g. "w2/w2:t2/w2:p2"): the pane id is the last "/"-separated
// segment. A binding with no "/" is returned unchanged.
func paneIDFromBinding(binding string) string {
	if idx := strings.LastIndex(binding, "/"); idx >= 0 {
		return binding[idx+1:]
	}
	return binding
}

// waitForRunState polls `hop status -C <repoRoot> -run <runID>` until the
// reported state is one of want, bounded by deadline, and returns the last
// parsed detail fields plus whether one of the wanted states was reached.
// It never fails the test itself: a caller that wants pane or process
// evidence captured before reporting a failure — scrollback is ephemeral,
// gone with the pane, so it must be captured before any t.Fatal — needs the
// chance to do that first with the returned fields.
func waitForRunState(t *testing.T, env []string, repoRoot, runID string, deadline time.Duration, want ...string) (fields map[string]string, reached bool) {
	t.Helper()
	reached = waitUntilDeadline(deadline, func() bool {
		result := runHop(t, env, repoRoot, "status", "-C", repoRoot, "-run", runID)
		fields = parseStatusDetail(result.Stdout)
		return slices.Contains(want, fields["state"])
	})
	return fields, reached
}

// querySQLite runs one read-only query against a test's own throwaway
// sqlite state database via the sqlite3 CLI — a standard tool on this
// suite's supported platforms, shelled out to the same way this suite
// already uses git and ps, so no new Go dependency is needed for direct
// evidence a rendered `hop status` block does not carry (row history, not
// just current state). dbPath is always this test's own isolated
// HOP_STATE_DIR/hop.db, never a real one. `-cmd ".timeout 5000"` gives
// this one-shot CLI connection the SAME busy-timeout SQLite itself already
// applies to every production connection (internal/adapters/sqlite/
// sqlite.go's dsn: `busy_timeout(5000)`) — SQLite's own blocking retry
// against a transient WAL lock (most likely to appear in the brief window
// right after a scenario SIGKILLs the store's own writer mid-transaction),
// deterministic and bounded, rather than this test package re-detecting
// and re-implementing the same behavior by matching "database is locked"
// in the CLI's own error text. Returns trimmed combined output; fails the
// test on any error.
func querySQLite(t *testing.T, dbPath, query string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sqlite3", "-cmd", ".timeout 5000", dbPath, query).CombinedOutput() //nolint:gosec // G204: fixed sqlite3 invocation against this test's own database path and a query it composed itself.
	if err != nil {
		t.Fatalf("sqlite3 %s %q: %v\n%s", dbPath, query, err, out)
	}
	return strings.TrimSpace(string(out))
}

// readControllerLog reads a hop controller's captured stdout log (as
// started by startHopController) from the test's artifact directory. Every
// current scenario reads the controller's own transition/report lines; a
// scenario needing its stderr instead reads
// "<artifacts.path>/<name>-stderr.log" directly.
func readControllerLog(t *testing.T, artifacts *artifactDir, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(artifacts.path, name+"-stdout.log")) //nolint:gosec // G304: a path this test constructed itself, under its own artifact directory.
	if err != nil {
		t.Fatalf("read %s stdout log: %v", name, err)
	}
	return string(content)
}

// controllerLogTimeout bounds every current waitForControllerLog wait: the
// controller's own next transition/report line, never a full run round trip
// (callers waiting on a terminal run state poll hop status instead, via
// waitForRunState/requireRunState, since the controller's stdout is not a
// reliable real-time mirror of it — see runEndToEndTimeout's doc comment).
const controllerLogTimeout = 30 * time.Second

// waitForControllerLog polls a hop controller's captured stdout log until
// its content contains want, bounded by controllerLogTimeout, and returns
// the content. It fails the test with the accumulated content on timeout.
func waitForControllerLog(t *testing.T, artifacts *artifactDir, name, want string) string {
	t.Helper()
	var content string
	found := waitUntilDeadline(controllerLogTimeout, func() bool {
		content = readControllerLog(t, artifacts, name)
		return strings.Contains(content, want)
	})
	if !found {
		t.Fatalf("%s stdout log never contained %q; content:\n%s", name, want, content)
	}
	return content
}

// waitForControllerExit waits for a hop controller leader to exit, bounded
// by deadline, and returns its ProcessState. It fails the test on timeout.
func waitForControllerExit(t *testing.T, sp *serverProcess, deadline time.Duration) *os.ProcessState {
	t.Helper()
	select {
	case <-sp.leaderExited:
		return sp.leaderCmd.ProcessState
	case <-time.After(deadline):
		t.Fatalf("hop controller did not exit within %s", deadline)
		return nil
	}
}
