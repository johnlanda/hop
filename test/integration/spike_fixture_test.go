package integration

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The TestSpike* tests are the Phase 2 capability spike: they establish, with
// executed evidence against a disposable server, the Herdr capabilities the
// Phase 2 design depends on. Each test states the spike item it answers; the
// verdicts and evidence index live in FINDINGS.md at the repository root.

// spikeFixtureSource is the fixture program the spike compiles. One binary
// serves every role, switching on its own basename:
//
//   - launch-standin: stands in for the future `hop launch` sanitizing
//     launcher. It takes --harness <path> and replaces itself with that
//     program via execve, passing the remaining arguments through.
//   - any other name (installed as `claude` and as `standin-harness`): stands
//     in for a native harness process. It prints marker lines carrying its
//     name, pid, argv and the HOP_* variables it inherited, then writes its
//     complete environment atomically (write-temp-then-rename) to a pid-named
//     file in its working directory and to HOP_SPIKE_ENV_FILE when set, prints
//     SPIKE-ENV-WRITTEN once the dump is durably in place, and reads stdin
//     until SPIKE-QUIT / SPIKE-QUIT-FAIL / EOF so it stays the pane's live
//     foreground process until the test ends it. The rename makes a reader
//     that keys on the marker see a complete file, never a partial write.
//
// A copy installed as `claude` matches Herdr's process-name detection for the
// claude agent kind; `standin-harness` is the control that an unrecognized
// name is never detected. Neither is a real harness; no network or credential
// is involved. The recognized-name copy is invoked by absolute path, except
// where a test deliberately puts its directory on a pane shell's PATH so a
// bare `claude` (as agent.start or Herdr's own resume command types it)
// resolves to the fixture.
const spikeFixtureSource = `package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

func main() {
	if filepath.Base(os.Args[0]) == "launch-standin" {
		launchStandin()
		return
	}
	harness()
}

func launchStandin() {
	args := os.Args[1:]
	harnessPath := ""
	passthrough := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--harness" && i+1 < len(args) {
			harnessPath = args[i+1]
			i++
			continue
		}
		passthrough = append(passthrough, args[i])
	}
	if harnessPath == "" {
		fmt.Fprintln(os.Stderr, "launch-standin: --harness is required")
		os.Exit(2)
	}
	argv := append([]string{harnessPath}, passthrough...)
	if err := syscall.Exec(harnessPath, argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "launch-standin: exec %s: %v\n", harnessPath, err)
		os.Exit(1)
	}
}

func writeEnvDumpAtomic(path string, dump []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, dump, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func harness() {
	name := filepath.Base(os.Args[0])
	fmt.Printf("SPIKE-HARNESS-STARTED name=[%s] pid=[%d]\n", name, os.Getpid())
	fmt.Printf("SPIKE-ARGS=[%s]\n", strings.Join(os.Args[1:], " "))
	for _, key := range []string{"HOP_RUN_ID", "HOP_ATTEMPT_ID", "HOP_ROLE"} {
		fmt.Printf("SPIKE-%s=[%s]\n", key, os.Getenv(key))
	}
	environ := os.Environ()
	sort.Strings(environ)
	dump := []byte(strings.Join(environ, "\n") + "\n")
	// Dump the environment atomically to a pid-named file in the working
	// directory, so a test can inspect the environment of a process it did not
	// itself launch (the restore path relaunches the harness) and correlate it
	// by pid. Honor an explicit path too. The SPIKE-ENV-WRITTEN marker is
	// printed only after the rename, so a reader that waits for it sees the
	// complete dump.
	if cwd, err := os.Getwd(); err == nil {
		if err := writeEnvDumpAtomic(filepath.Join(cwd, fmt.Sprintf("spike-env-%d.txt", os.Getpid())), dump); err != nil {
			fmt.Printf("SPIKE-ENV-ERROR=[%v]\n", err)
		}
	}
	if envFile := os.Getenv("HOP_SPIKE_ENV_FILE"); envFile != "" {
		if err := writeEnvDumpAtomic(envFile, dump); err != nil {
			fmt.Printf("SPIKE-ENV-ERROR=[%v]\n", err)
		}
	}
	fmt.Printf("SPIKE-ENV-WRITTEN pid=[%d]\n", os.Getpid())
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		switch strings.TrimSpace(scanner.Text()) {
		case "SPIKE-QUIT":
			return
		case "SPIKE-QUIT-FAIL":
			os.Exit(3)
		}
	}
}
`

// spikeFixtures holds the absolute paths of the compiled fixture binaries.
type spikeFixtures struct {
	launchStandin  string // the sanitizing-launcher stand-in
	claude         string // the harness stand-in under a detection-recognized name
	standinHarness string // the harness stand-in under an unrecognized name
}

// buildSpikeFixtures compiles the fixture program once into a temporary
// module and installs the binary under each fixture name. Tests invoke the
// binaries by absolute path; a test that needs a bare `claude` to resolve to
// the fixture (S3's Herdr resume command, S5's agent.start) puts the fixture
// directory on that pane shell's PATH through the test's own scratch rc files.
func buildSpikeFixtures(t *testing.T) spikeFixtures {
	t.Helper()
	dir, err := os.MkdirTemp("", "hop-spike")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			t.Logf("remove spike fixtures: %v", removeErr)
		}
	})
	src := filepath.Join(dir, "src")
	if err = os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(src, "go.mod"), []byte("module spikefixture\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(src, "main.go"), []byte(spikeFixtureSource), 0o600); err != nil {
		t.Fatal(err)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go tool is required to build the spike fixture: %v", err)
	}
	built := filepath.Join(dir, "launch-standin")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, goBin, "build", "-o", built, ".") //nolint:gosec // G204: the go tool builds this test's own generated fixture module.
	build.Dir = src
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("go build spike fixture: %v\n%s", buildErr, out)
	}
	fixtures := spikeFixtures{
		launchStandin:  built,
		claude:         filepath.Join(dir, "claude"),
		standinHarness: filepath.Join(dir, "standin-harness"),
	}
	copyExecutable(t, built, fixtures.claude)
	copyExecutable(t, built, fixtures.standinHarness)
	return fixtures
}

// copyExecutable copies a built binary to another name, keeping it runnable.
func copyExecutable(t *testing.T, from, to string) {
	t.Helper()
	copyFile(t, from, to)
	if err := os.Chmod(to, 0o755); err != nil { //nolint:gosec // G302: the fixture must be executable; it lives in this test's private directory.
		t.Fatal(err)
	}
}

// runInOwnedGroup runs a command as its own process-group leader with a
// bounded lifecycle, and after it returns confirms the whole group is gone. It
// gives a real external binary (S4's claude, and the self-test's fixture
// leader) an owned teardown: on the context deadline cmd.Cancel SIGKILLs the
// whole group so a descendant cannot outlive the leader or hold the output
// pipe open, and WaitDelay bounds pipe draining so CombinedOutput cannot block
// indefinitely. It returns the combined output, the run error, and whether the
// deadline fired.
func runInOwnedGroup(t *testing.T, timeout time.Duration, name string, env []string, dir string, args ...string) (out []byte, timedOut bool, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: the binary and arguments are chosen by this suite.
	cmd.Env = env
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 10 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Setpgid made the child a group leader (pgid == pid); signal the whole
		// group so descendants die with it. The leader is still unreaped here
		// (Wait has not returned), so the pgid cannot have been recycled.
		if killErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
			return killErr
		}
		return nil
	}
	out, err = cmd.CombinedOutput()
	timedOut = ctx.Err() != nil
	// After CombinedOutput the leader is reaped; confirm no descendant (which
	// would have been reparented to init) survives holding the group open.
	if cmd.Process != nil {
		pgid := cmd.Process.Pid
		if safeToSignalGroup(pgid, syscall.Getpgrp()) {
			if !waitUntil(func() bool { return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) }) {
				t.Errorf("owned process group %d still had members after teardown", pgid)
			}
		}
	}
	return out, timedOut, err
}

// requireShell skips the calling test with an explicit reason when the given
// shell is not an executable file on this host, so a capability case for a
// shell that is not installed (for example /bin/zsh on a minimal Linux runner)
// is reported as not-run rather than failing.
func requireShell(t *testing.T, shell string) {
	t.Helper()
	info, err := os.Stat(shell)
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		t.Skipf("shell capability skipped, not run: %s is not an executable shell on this host (%v)", shell, err)
	}
}

// newSpikeUUID returns a random canonical UUIDv4 string, so spike launches
// carry unique run/attempt identities without any new dependency.
func newSpikeUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// spikeProcessInfo is the pane.process_info result: the pane's shell pid,
// foreground process-group id, tty and the foreground processes with their
// identity fields. This is the complete field set the API exposes; note the
// absence of any process start time.
type spikeProcessInfo struct {
	PaneID                 string                `json:"pane_id"`
	ShellPID               uint32                `json:"shell_pid"`
	ForegroundProcessGroup uint32                `json:"foreground_process_group_id"`
	TTY                    string                `json:"tty"`
	ForegroundProcesses    []spikeProcessDetails `json:"foreground_processes"`
}

// spikeProcessDetails is one foreground process record.
type spikeProcessDetails struct {
	PID     uint32   `json:"pid"`
	Name    string   `json:"name"`
	Argv0   string   `json:"argv0"`
	Argv    []string `json:"argv"`
	Cmdline string   `json:"cmdline"`
	Cwd     string   `json:"cwd"`
}

// processInfo reads pane.process_info for one pane.
func (s *testServer) processInfo(t *testing.T, paneID string) spikeProcessInfo {
	t.Helper()
	var result struct {
		ProcessInfo spikeProcessInfo `json:"process_info"`
	}
	s.call(t, "pane.process_info", map[string]any{"pane_id": paneID}, &result)
	return result.ProcessInfo
}

// tryProcessInfo reads pane.process_info without failing the test, so a caller
// can inspect a pane that may have no live runtime yet (the delayed-restore
// window, where the pane exists in the snapshot but its shell is not spawned).
func (s *testServer) tryProcessInfo(t *testing.T, paneID string) (spikeProcessInfo, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	defer cancel()
	var result struct {
		ProcessInfo spikeProcessInfo `json:"process_info"`
	}
	err := s.client.Call(ctx, "pane.process_info", map[string]any{"pane_id": paneID}, &result)
	return result.ProcessInfo, err
}

// envDumpForPID reads and parses one fixture env dump (spike-env-<pid>.txt)
// into a name→value map, reporting whether the file exists. The fixture writes
// the file atomically, so a present file is complete.
func envDumpForPID(t *testing.T, dir string, pid uint32) (map[string]string, bool) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("spike-env-%d.txt", pid))) //nolint:gosec // G304: a pid-named file under this test's own worktree.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false
		}
		t.Fatalf("read env dump for pid %d: %v", pid, err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(content), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			env[name] = value
		}
	}
	return env, true
}

// waitForEnvDump waits until the fixture's atomic env dump for pid exists and
// returns its parsed contents.
func (*testServer) waitForEnvDump(t *testing.T, dir string, pid uint32) map[string]string {
	t.Helper()
	var env map[string]string
	if !waitUntil(func() bool {
		var ok bool
		env, ok = envDumpForPID(t, dir, pid)
		return ok
	}) {
		t.Fatalf("fixture env dump for pid %d never appeared in %s", pid, dir)
	}
	return env
}

// snapshotAgentPane returns the pane id that session.snapshot lists with the
// given agent label, and whether exactly one such pane exists.
func (s *testServer) snapshotAgentPane(t *testing.T, agent string) (string, bool) {
	t.Helper()
	var result struct {
		Snapshot struct {
			Agents []struct {
				PaneID string `json:"pane_id"`
				Agent  string `json:"agent"`
			} `json:"agents"`
		} `json:"snapshot"`
	}
	s.call(t, "session.snapshot", nil, &result)
	found := ""
	for _, a := range result.Snapshot.Agents {
		if a.Agent == agent {
			if found != "" {
				return "", false
			}
			found = a.PaneID
		}
	}
	return found, found != ""
}

// renderProcessInfo formats a process-info result for evidence files.
func renderProcessInfo(info spikeProcessInfo) string {
	var out strings.Builder
	fmt.Fprintf(&out, "pane=%s shell_pid=%d foreground_pgid=%d tty=%s\n",
		info.PaneID, info.ShellPID, info.ForegroundProcessGroup, info.TTY)
	for _, p := range info.ForegroundProcesses {
		fmt.Fprintf(&out, "  pid=%d name=%q argv0=%q argv=%q cmdline=%q cwd=%q\n",
			p.PID, p.Name, p.Argv0, p.Argv, p.Cmdline, p.Cwd)
	}
	return out.String()
}

// foregroundClaudeProcess returns the pane's foreground process whose name is
// the recognized "claude" harness name, or nil. Every spike that inspects a
// foreground process is looking for the fixture harness under that name.
func foregroundClaudeProcess(info spikeProcessInfo) *spikeProcessDetails {
	for i := range info.ForegroundProcesses {
		if info.ForegroundProcesses[i].Name == "claude" {
			return &info.ForegroundProcesses[i]
		}
	}
	return nil
}

// spikeAgentRecord is the subset of an agent.list record the spike asserts
// on.
type spikeAgentRecord struct {
	PaneID           string `json:"pane_id"`
	Agent            string `json:"agent"`
	AgentStatus      string `json:"agent_status"`
	LaunchPending    bool   `json:"launch_pending"`
	InteractiveReady bool   `json:"interactive_ready"`
}

// agentRecords returns agent.list keyed by pane id.
func (s *testServer) agentRecords(t *testing.T) map[string]spikeAgentRecord {
	t.Helper()
	var result struct {
		Agents []spikeAgentRecord `json:"agents"`
	}
	s.call(t, "agent.list", nil, &result)
	records := map[string]spikeAgentRecord{}
	for _, agent := range result.Agents {
		records[agent.PaneID] = agent
	}
	return records
}

// waitForAgent polls agent.list until the pane appears with the wanted agent
// label and returns the record; it fails on timeout with the last state.
func (s *testServer) waitForAgent(t *testing.T, paneID, agent string) spikeAgentRecord {
	t.Helper()
	var last map[string]spikeAgentRecord
	found := waitUntil(func() bool {
		last = s.agentRecords(t)
		record, ok := last[paneID]
		return ok && record.Agent == agent
	})
	if !found {
		t.Fatalf("pane %s was never listed as agent %q; last agent.list: %+v", paneID, agent, last)
	}
	return last[paneID]
}

// waitForShellReady polls pane.process_info until the pane's own login shell
// is observed as the sole foreground process (nonzero shell pid, foreground
// group equal to it, exactly one foreground process whose pid is the shell),
// which is the condition Herdr's agent.start requires to treat the pane as an
// available shell (repos/herdr/src/platform/mod.rs:301
// available_pane_shell_from_job). Inspection is not atomic with the later
// agent.start, so this is a readiness observation, not a guarantee. It fails
// on timeout with the last info.
func (s *testServer) waitForShellReady(t *testing.T, paneID string) {
	t.Helper()
	var last spikeProcessInfo
	ready := waitUntil(func() bool {
		last = s.processInfo(t, paneID)
		if last.ShellPID == 0 || last.ForegroundProcessGroup != last.ShellPID {
			return false
		}
		if len(last.ForegroundProcesses) != 1 {
			return false
		}
		return last.ForegroundProcesses[0].PID == last.ShellPID
	})
	if !ready {
		t.Fatalf("pane %s shell never became the sole idle foreground process; last:\n%s",
			paneID, renderProcessInfo(last))
	}
}

// negativeObservationWindow is how long a not-detected assertion keeps
// polling before concluding absence. Detection is a periodic poll inside
// Herdr, so a short bounded window is required to observe a negative; this
// is an observation window for absence evidence, not a synchronization
// sleep.
const negativeObservationWindow = 5 * time.Second

// timeAfterConditionTimeout returns a channel firing after the suite's
// standard bounded-condition timeout, for select-based waits on streams.
func timeAfterConditionTimeout() <-chan time.Time {
	return time.After(conditionTimeout)
}

// assertNeverListedAsAgent polls agent.list for the whole negative window
// and fails if the pane is ever listed with any agent label.
func (s *testServer) assertNeverListedAsAgent(t *testing.T, paneID string) {
	t.Helper()
	deadline := time.Now().Add(negativeObservationWindow)
	for time.Now().Before(deadline) {
		if record, ok := s.agentRecords(t)[paneID]; ok && record.Agent != "" {
			t.Fatalf("pane %s was listed as agent %q; an unrecognized process name must not be detected", paneID, record.Agent)
		}
		time.Sleep(pollInterval)
	}
}
