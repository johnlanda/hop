package integration

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The TestSpike* tests are the Phase 2 capability spike: they establish, with
// executed evidence against a disposable server, the Herdr capabilities the
// Phase 2 design depends on but nobody had verified. Each test states the
// spike item it answers; the verdicts are collected in FINDINGS.md at the
// repository root while the spike branch is under review.

// spikeFixtureSource is the fixture program the spike compiles. One binary
// serves every role, switching on its own basename:
//
//   - launch-standin: the stand-in for the future `hop launch` sanitizing
//     launcher. It takes --harness <path> and replaces itself with that
//     program via execve, passing the remaining arguments through, exactly
//     as the real launcher will replace itself with the harness.
//   - any other name (the spike installs it as `claude` and as
//     `standin-harness`): the stand-in for a native harness process. It
//     prints marker lines carrying its name, pid, argv and the HOP_*
//     variables it inherited, optionally dumps its complete environment to
//     the file named by HOP_SPIKE_ENV_FILE, and then reads stdin line by
//     line until the line SPIKE-QUIT or end of input, so it stays the pane's
//     live foreground process until the test ends it.
//
// A copy installed as `claude` matches Herdr's process-name detection for
// the claude agent kind; the `standin-harness` copy is the control that no
// unrecognized name is ever detected. Neither is the real harness and no
// network or credential is involved.
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
	// Always dump the environment to a pid-named file in the working
	// directory, so a test can inspect the environment of a process it did
	// not itself launch (the restore path relaunches the harness). Also honor
	// an explicit path when set.
	if cwd, err := os.Getwd(); err == nil {
		_ = os.WriteFile(filepath.Join(cwd, fmt.Sprintf("spike-env-%d.txt", os.Getpid())), dump, 0o600)
	}
	if envFile := os.Getenv("HOP_SPIKE_ENV_FILE"); envFile != "" {
		_ = os.WriteFile(envFile, dump, 0o600)
	}
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
// module and installs the binary under each fixture name. The binaries are
// invoked by absolute path only; the recognized-name copy is never placed on
// any PATH.
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

// foregroundProcessNamed returns the foreground process with the given name,
// or nil.
func foregroundProcessNamed(info spikeProcessInfo, name string) *spikeProcessDetails {
	for i := range info.ForegroundProcesses {
		if info.ForegroundProcesses[i].Name == name {
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
