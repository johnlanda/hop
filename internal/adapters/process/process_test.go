package process_test

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
	"syscall"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/process"
	"github.com/johnlanda/hop/internal/app"
)

// helperModeVar selects the fixture behavior when the test binary re-runs
// itself as a child process. Every fixture child is the test executable
// itself, so the fixtures are built by the tests by construction.
const helperModeVar = "HOP_PROCESS_HELPER"

// TestMain dispatches fixture-child modes before any test runs. A process
// started without the mode variable runs the tests normally.
func TestMain(m *testing.M) {
	switch mode := os.Getenv(helperModeVar); mode {
	case "":
		os.Exit(m.Run())
	case "envdump":
		helperEnvDump()
	case "sleeper":
		helperSleeper()
	case "spawner":
		helperSpawner()
	case "exec":
		helperExec()
	case "execresolved":
		helperExecResolved()
	case "flood":
		helperFlood()
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(97)
	}
}

// helperEnvDump prints its pid, working directory and complete environment,
// optionally floods stdout, writes a marker to stderr, and exits with the
// requested code.
func helperEnvDump() {
	fmt.Printf("PID %d\n", os.Getpid())
	for _, arg := range os.Args {
		fmt.Printf("ARG %s\n", arg)
	}
	if wd, err := os.Getwd(); err == nil {
		fmt.Printf("CWD %s\n", wd)
	}
	for _, entry := range os.Environ() {
		fmt.Printf("ENV %s\n", entry)
	}
	if raw := os.Getenv("HOP_HELPER_STDOUT_BYTES"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bad HOP_HELPER_STDOUT_BYTES:", err)
			os.Exit(96)
		}
		chunk := bytes.Repeat([]byte{'x'}, 64*1024)
		for written := 0; written < n; {
			m := min(len(chunk), n-written)
			if _, err := os.Stdout.Write(chunk[:m]); err != nil {
				os.Exit(95)
			}
			written += m
		}
	}
	fmt.Fprintln(os.Stderr, "envdump stderr marker")
	code := 0
	if raw := os.Getenv("HOP_HELPER_EXIT"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bad HOP_HELPER_EXIT:", err)
			os.Exit(96)
		}
		code = parsed
	}
	os.Exit(code)
}

// helperFlood writes exactly HOP_HELPER_STDOUT_BYTES bytes to stdout and
// HOP_HELPER_STDERR_BYTES bytes to stderr, each in HOP_HELPER_CHUNK_BYTES
// writes (default 64 KiB), and nothing else, then exits with
// HOP_HELPER_EXIT.
func helperFlood() {
	chunkSize := helperInt("HOP_HELPER_CHUNK_BYTES", 64*1024)
	for _, stream := range []struct {
		out *os.File
		n   int
	}{{os.Stdout, helperInt("HOP_HELPER_STDOUT_BYTES", 0)}, {os.Stderr, helperInt("HOP_HELPER_STDERR_BYTES", 0)}} {
		chunk := bytes.Repeat([]byte{'y'}, chunkSize)
		for written := 0; written < stream.n; {
			m := min(len(chunk), stream.n-written)
			if _, err := stream.out.Write(chunk[:m]); err != nil {
				os.Exit(95)
			}
			written += m
		}
	}
	os.Exit(helperInt("HOP_HELPER_EXIT", 0))
}

// helperInt reads a non-negative integer helper variable, or def when it is
// unset.
func helperInt(name string, def int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		fmt.Fprintln(os.Stderr, "bad", name)
		os.Exit(96)
	}
	return n
}

// helperSleeper announces itself through its pid file, prints a readiness
// marker, and then sleeps until killed.
func helperSleeper() {
	fmt.Println("sleeper-ready")
	writePidFile(os.Getenv("HOP_HELPER_PIDFILE"))
	sleepForever()
}

// helperSpawner starts one sleeper child in its own (inherited) process
// group with the argv the parent test requested, then either exits leaving
// the child behind or announces itself and sleeps.
func helperSpawner() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve executable:", err)
		os.Exit(94)
	}
	var childArgs []string
	if arg := os.Getenv("HOP_HELPER_CHILD_ARG1"); arg != "" {
		childArgs = append(childArgs, arg)
	}
	if arg := os.Getenv("HOP_HELPER_CHILD_ARG2"); arg != "" {
		childArgs = append(childArgs, arg)
	}
	child := exec.CommandContext(context.Background(), exe, childArgs...) //nolint:gosec // G204: the executable is this test binary re-run as a fixture child with test-chosen arguments. The context is deliberately unbounded: the child must outlive this exiting helper.
	child.Env = []string{
		helperModeVar + "=sleeper",
		"HOP_HELPER_PIDFILE=" + os.Getenv("HOP_HELPER_CHILD_PIDFILE"),
	}
	if err := child.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start sleeper child:", err)
		os.Exit(93)
	}
	if os.Getenv("HOP_HELPER_SPAWNER_EXITS") == "1" {
		os.Exit(0) // the unwaited child stays in the group
	}
	fmt.Println("spawner-ready")
	writePidFile(os.Getenv("HOP_HELPER_PIDFILE"))
	sleepForever()
}

// helperExec replaces itself with the test binary in envdump mode through
// process.Exec. Reaching the code after Exec means the replacement failed.
func helperExec() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve executable:", err)
		os.Exit(94)
	}
	execErr := process.Exec(
		[]string{exe, "argument-after-exec"},
		[]string{helperModeVar + "=envdump", "HOP_HELPER_MARKER=exec-marker"},
	)
	fmt.Fprintln(os.Stderr, "exec returned:", execErr)
	os.Exit(3)
}

// helperExecResolved replaces itself with the test binary in envdump mode
// through process.ExecResolved, deliberately renaming argv[0]: the dump must
// then report the renamed argv, proving the executed path and the reported
// argv are independent. Reaching the code after ExecResolved means the
// replacement failed.
func helperExecResolved() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve executable:", err)
		os.Exit(94)
	}
	execErr := process.ExecResolved(
		exe,
		[]string{"renamed-argv0", "frozen-tail"},
		[]string{helperModeVar + "=envdump"},
	)
	fmt.Fprintln(os.Stderr, "exec returned:", execErr)
	os.Exit(3)
}

// writePidFile publishes the helper's pid with a write-then-rename so the
// polling test never reads a partial file.
func writePidFile(path string) {
	if path == "" {
		fmt.Fprintln(os.Stderr, "no pid file path")
		os.Exit(92)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil { //nolint:gosec // G703: the path is chosen by the parent test inside its own temporary directory.
		fmt.Fprintln(os.Stderr, "write pid file:", err)
		os.Exit(92)
	}
	if err := os.Rename(tmp, path); err != nil { //nolint:gosec // G703: both paths live inside the parent test's own temporary directory.
		fmt.Fprintln(os.Stderr, "publish pid file:", err)
		os.Exit(92)
	}
}

// sleepForever blocks the helper until it is killed.
func sleepForever() {
	for {
		time.Sleep(time.Hour)
	}
}

// testExecutable returns the absolute path of the test binary.
func testExecutable(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// waitUntil polls a condition with a bounded deadline and no fixed sleeps
// beyond the poll pace.
func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// readPid reads a helper's published pid file.
func readPid(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a pid file inside this test's own temporary directory.
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil {
		t.Fatalf("pid file %s: %v", path, err)
	}
	return pid
}

// groupGone reports whether no process remains in the group; signal 0 only
// reads existence.
func groupGone(pgid int) bool {
	return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}

func TestRunnerExitCodeMapping(t *testing.T) {
	exe := testExecutable(t)
	cases := []struct {
		name string
		exit string
		want int
	}{
		{name: "success is exit 0", exit: "", want: 0},
		{name: "failure code 7", exit: "7", want: 7},
		{name: "failure code 42", exit: "42", want: 42},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := []string{helperModeVar + "=envdump"}
			if tc.exit != "" {
				env = append(env, "HOP_HELPER_EXIT="+tc.exit)
			}

			result, err := process.Runner{}.Run(t.Context(), app.Command{Argv: []string{exe}, Env: env})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			if result.ExitCode != tc.want {
				t.Errorf("ExitCode = %d, want %d", result.ExitCode, tc.want)
			}
			if result.Duration <= 0 {
				t.Errorf("Duration = %v, want > 0", result.Duration)
			}
		})
	}
}

func TestRunnerCapturesOutputAndDeliversExactEnvironment(t *testing.T) {
	exe := testExecutable(t)
	env := []string{
		helperModeVar + "=envdump",
		"HOP_TEST_MARKER_A=alpha",
		"HOP_TEST_MARKER_B=beta with spaces",
	}

	result, err := process.Runner{}.Run(t.Context(), app.Command{Argv: []string{exe}, Env: env})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, stderr: %s", result.ExitCode, result.Stderr)
	}
	var dumped []string
	for line := range strings.Lines(string(result.Stdout)) {
		if entry, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "ENV "); ok {
			dumped = append(dumped, entry)
		}
	}
	slices.Sort(dumped)
	want := slices.Sorted(slices.Values(env))
	if !slices.Equal(dumped, want) {
		t.Errorf("child environment = %q, want exactly %q", dumped, want)
	}
	if !strings.Contains(string(result.Stderr), "envdump stderr marker") {
		t.Errorf("stderr %q does not carry the marker", result.Stderr)
	}
}

func TestRunnerNilEnvRunsEmptyEnvironment(t *testing.T) {
	result, err := process.Runner{}.Run(t.Context(), app.Command{Argv: []string{"/usr/bin/env"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, stderr: %s", result.ExitCode, result.Stderr)
	}
	if got := strings.TrimSpace(string(result.Stdout)); got != "" {
		t.Errorf("environment of a nil-Env command = %q, want empty", got)
	}
}

func TestRunnerRunsInDir(t *testing.T) {
	exe := testExecutable(t)
	dir := t.TempDir()

	result, err := process.Runner{}.Run(t.Context(), app.Command{
		Argv: []string{exe},
		Dir:  dir,
		Env:  []string{helperModeVar + "=envdump"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var reported string
	for line := range strings.Lines(string(result.Stdout)) {
		if cwd, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "CWD "); ok {
			reported = cwd
		}
	}
	wantDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	gotDir, err := filepath.EvalSymlinks(reported)
	if err != nil {
		t.Fatalf("child cwd %q: %v", reported, err)
	}
	if gotDir != wantDir {
		t.Errorf("child cwd = %s, want %s", gotDir, wantDir)
	}
}

func TestRunnerBoundsCapturedOutput(t *testing.T) {
	exe := testExecutable(t)
	const captureLimit = 1 << 20

	result, err := process.Runner{}.Run(t.Context(), app.Command{
		Argv: []string{exe},
		Env:  []string{helperModeVar + "=envdump", "HOP_HELPER_STDOUT_BYTES=" + strconv.Itoa(3*captureLimit)},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d; the child must keep running with its pipe drained past the cap", result.ExitCode)
	}
	if len(result.Stdout) != captureLimit {
		t.Errorf("len(Stdout) = %d, want exactly the %d-byte cap", len(result.Stdout), captureLimit)
	}
	if !result.StdoutTruncated || result.StderrTruncated {
		t.Errorf("StdoutTruncated = %v, StderrTruncated = %v; want only stdout reported truncated", result.StdoutTruncated, result.StderrTruncated)
	}
}

// TestRunnerReportsTruncation pins the capture bound and the truncation
// flags through the real Runner: a stream is reported truncated exactly
// when the child wrote more than the bound — 1 MiB, or the command's own
// MaxOutputBytes, applied to each stream separately — whatever the exit
// status and wherever the child's writes fall, and each flag describes only
// its own stream. Callers deciding on a whole stream rely on this.
func TestRunnerReportsTruncation(t *testing.T) {
	exe := testExecutable(t)
	const captureLimit = 1 << 20
	cases := []struct {
		name                     string
		stdout, stderr, chunk    int
		exit                     int
		limit                    int
		wantStdout, wantStderr   int
		stdoutTrunc, stderrTrunc bool
	}{
		{name: "small output", stdout: 10, stderr: 5, wantStdout: 10, wantStderr: 5},
		{name: "empty output", wantStdout: 0, wantStderr: 0},
		{name: "exactly the bound is complete", stdout: captureLimit, wantStdout: captureLimit},
		{name: "exactly the bound in one write is complete", stdout: captureLimit, chunk: captureLimit, wantStdout: captureLimit},
		{name: "one byte past the bound", stdout: captureLimit + 1, wantStdout: captureLimit, stdoutTrunc: true},
		{name: "the bound falls exactly between two writes", stdout: 2 * captureLimit, chunk: captureLimit / 4, wantStdout: captureLimit, stdoutTrunc: true},
		{name: "the bound falls inside a write", stdout: captureLimit + 100, chunk: 3000, wantStdout: captureLimit, stdoutTrunc: true},
		{name: "truncated on a successful exit", stdout: 3 * captureLimit, wantStdout: captureLimit, stdoutTrunc: true},
		{name: "truncated on a failing exit", stdout: 3 * captureLimit, exit: 3, wantStdout: captureLimit, stdoutTrunc: true},
		{name: "stderr alone", stdout: 7, stderr: captureLimit + 1, wantStdout: 7, wantStderr: captureLimit, stderrTrunc: true},
		{name: "both streams", stdout: captureLimit + 9, stderr: 2 * captureLimit, wantStdout: captureLimit, wantStderr: captureLimit, stdoutTrunc: true, stderrTrunc: true},
		{name: "a larger per-call bound keeps output past the default", stdout: 2*captureLimit + 1, limit: 3 * captureLimit, wantStdout: 2*captureLimit + 1},
		{name: "exactly a per-call bound is complete", stdout: 2 * captureLimit, limit: 2 * captureLimit, wantStdout: 2 * captureLimit},
		{name: "one byte past a per-call bound", stdout: 2*captureLimit + 1, limit: 2 * captureLimit, wantStdout: 2 * captureLimit, stdoutTrunc: true},
		{name: "a per-call bound applies to each stream", stdout: 3 * captureLimit, stderr: 2 * captureLimit, limit: 2 * captureLimit, wantStdout: 2 * captureLimit, wantStderr: 2 * captureLimit, stdoutTrunc: true},
		{name: "a per-call bound below the default", stdout: 101, stderr: 100, limit: 100, wantStdout: 100, wantStderr: 100, stdoutTrunc: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := []string{
				helperModeVar + "=flood",
				"HOP_HELPER_STDOUT_BYTES=" + strconv.Itoa(tc.stdout),
				"HOP_HELPER_STDERR_BYTES=" + strconv.Itoa(tc.stderr),
				"HOP_HELPER_EXIT=" + strconv.Itoa(tc.exit),
			}
			if tc.chunk != 0 {
				env = append(env, "HOP_HELPER_CHUNK_BYTES="+strconv.Itoa(tc.chunk))
			}

			result, err := process.Runner{}.Run(t.Context(), app.Command{Argv: []string{exe}, Env: env, MaxOutputBytes: tc.limit})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			if result.ExitCode != tc.exit {
				t.Errorf("ExitCode = %d, want %d", result.ExitCode, tc.exit)
			}
			if len(result.Stdout) != tc.wantStdout || len(result.Stderr) != tc.wantStderr {
				t.Errorf("captured %d stdout and %d stderr bytes, want %d and %d", len(result.Stdout), len(result.Stderr), tc.wantStdout, tc.wantStderr)
			}
			if result.StdoutTruncated != tc.stdoutTrunc || result.StderrTruncated != tc.stderrTrunc {
				t.Errorf("StdoutTruncated = %v, StderrTruncated = %v; want %v, %v", result.StdoutTruncated, result.StderrTruncated, tc.stdoutTrunc, tc.stderrTrunc)
			}
		})
	}
}

func TestRunnerRejectsInvalidArgv(t *testing.T) {
	cases := []struct {
		name string
		argv []string
	}{
		{name: "empty argv", argv: nil},
		{name: "bare name", argv: []string{"sh"}},
		{name: "relative path", argv: []string{"./sh"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := process.Runner{}.Run(t.Context(), app.Command{Argv: tc.argv})

			if err == nil {
				t.Fatalf("Run(%q) succeeded, want rejection", tc.argv)
			}
		})
	}

	t.Run("negative output bound", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "started")
		_, err := process.Runner{}.Run(t.Context(), app.Command{Argv: []string{"/usr/bin/touch", marker}, MaxOutputBytes: -1})
		if err == nil {
			t.Fatal("Run with a negative output bound succeeded, want rejection")
		}
		if _, statErr := os.Lstat(marker); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("the command started despite the rejection (%v)", statErr)
		}
	})
}

func TestRunnerCancellationKillsWholeGroupWithTypedResult(t *testing.T) {
	exe := testExecutable(t)
	dir := t.TempDir()
	leaderPidFile := filepath.Join(dir, "leader.pid")
	childPidFile := filepath.Join(dir, "child.pid")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	type runOutcome struct {
		result app.CommandResult
		err    error
	}
	done := make(chan runOutcome, 1)
	finished := make(chan struct{})
	go func() {
		result, err := process.Runner{}.Run(ctx, app.Command{
			Argv: []string{exe},
			Env: []string{
				helperModeVar + "=spawner",
				"HOP_HELPER_PIDFILE=" + leaderPidFile,
				"HOP_HELPER_CHILD_PIDFILE=" + childPidFile,
			},
		})
		done <- runOutcome{result: result, err: err}
		close(finished)
	}()
	// Cleanup owns no group signal of its own: it cancels the run and waits
	// for Run to return, so the runner's anchored retirement — the only
	// owner of this group's teardown — has finished before the test ends.
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(30 * time.Second):
			t.Error("Run did not return after the cleanup cancellation")
		}
	})

	waitUntil(t, "both fixture pid files", func() bool {
		_, leaderErr := os.Stat(leaderPidFile)
		_, childErr := os.Stat(childPidFile)
		return leaderErr == nil && childErr == nil
	})
	leaderPid := readPid(t, leaderPidFile)
	childPid := readPid(t, childPidFile)
	pgid, err := syscall.Getpgid(childPid)
	if err != nil {
		t.Fatalf("getpgid(%d): %v", childPid, err)
	}
	if pgid != leaderPid {
		t.Fatalf("child pgid = %d, want the leader pid %d: the leader must lead a fresh group", pgid, leaderPid)
	}

	cancel()
	outcome := <-done

	var cancellation *process.CancellationError
	if !errors.As(outcome.err, &cancellation) {
		t.Fatalf("Run error = %v, want a *process.CancellationError", outcome.err)
	}
	if !errors.Is(outcome.err, context.Canceled) {
		t.Errorf("cancellation error %v does not wrap context.Canceled", outcome.err)
	}
	if !cancellation.GroupEmptied {
		t.Errorf("GroupEmptied = false, want the whole group observed gone")
	}
	if outcome.result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 for the SIGKILLed leader", outcome.result.ExitCode)
	}
	if !strings.Contains(string(outcome.result.Stdout), "spawner-ready") {
		t.Errorf("Stdout %q does not carry the output captured before cancellation", outcome.result.Stdout)
	}
	if !groupGone(pgid) {
		t.Errorf("process group %d still has members after cancellation", pgid)
	}
	if err := syscall.Kill(childPid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("descendant %d still exists after group kill (signal 0: %v)", childPid, err)
	}
}

func TestGroupInspectorAgainstLiveGroupThenRetirement(t *testing.T) {
	exe := testExecutable(t)
	dir := t.TempDir()
	childPidFile := filepath.Join(dir, "child.pid")
	const spaceArg = "argument with spaces"
	tabArg := "argument\twith\ttabs"

	// The leader exits immediately, leaving its sleeper descendant alive in
	// the recorded group: the leader-exit-with-children retirement case.
	result, err := process.Runner{}.Run(t.Context(), app.Command{
		Argv: []string{exe},
		Env: []string{
			helperModeVar + "=spawner",
			"HOP_HELPER_SPAWNER_EXITS=1",
			"HOP_HELPER_CHILD_PIDFILE=" + childPidFile,
			"HOP_HELPER_CHILD_ARG1=" + spaceArg,
			"HOP_HELPER_CHILD_ARG2=" + tabArg,
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("spawner ExitCode = %d, stderr: %s", result.ExitCode, result.Stderr)
	}

	waitUntil(t, "the sleeper child pid file", func() bool {
		_, statErr := os.Stat(childPidFile)
		return statErr == nil
	})
	childPid := readPid(t, childPidFile)
	pgid, err := syscall.Getpgid(childPid)
	if err != nil {
		t.Fatalf("getpgid(%d): %v", childPid, err)
	}
	anchor, disarm := armGroupRetirement(t, pgid)

	inspector := process.GroupInspector{}
	members, err := inspector.GroupProcesses(t.Context(), pgid)
	if err != nil {
		t.Fatalf("GroupProcesses: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("live group members = %+v, want the sleeper child and the test anchor", members)
	}
	var sleeper *app.GroupProcess
	for i := range members {
		switch members[i].PID {
		case childPid:
			sleeper = &members[i]
		case anchor.Process.Pid:
		default:
			t.Errorf("unexpected group member %+v", members[i])
		}
	}
	if sleeper == nil {
		t.Fatalf("members = %+v do not include the sleeper child %d", members, childPid)
	}
	wantArgv := []string{exe, spaceArg, tabArg}
	if !slices.Equal(sleeper.Argv, wantArgv) {
		t.Errorf("sleeper argv = %q, want the exact vector %q", sleeper.Argv, wantArgv)
	}

	if signalErr := inspector.SignalGroup(t.Context(), pgid); signalErr != nil {
		t.Fatalf("SignalGroup: %v", signalErr)
	}
	// The retirement under test has been delivered to every member, the
	// test anchor included; reap the anchor (releasing the identity pin)
	// and disarm the cleanup so no signal is ever sent after the release.
	disarm()
	waitUntil(t, "the retired group to empty", func() bool { return groupGone(pgid) })
	members, err = inspector.GroupProcesses(t.Context(), pgid)
	if err != nil {
		t.Fatalf("GroupProcesses after retirement: %v", err)
	}
	if len(members) != 0 {
		t.Errorf("retired group members = %+v, want none", members)
	}
}

// armGroupRetirement joins a test-owned sleep anchor into the group and
// registers an idempotent retirement owner: while the anchor is unreaped it
// provably pins the group id, so the failure-path cleanup may signal the
// group; disarm reaps the anchor and marks the group retired, after which
// cleanup sends no signal — a signal is never issued after the identity pin
// is released.
func armGroupRetirement(t *testing.T, pgid int) (anchor *exec.Cmd, disarm func()) {
	t.Helper()
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	anchor = exec.CommandContext(context.Background(), sleepBin, "100000") //nolint:gosec // G204: a fixed sleep binary; the anchor only pins the process group id for this test's cleanup.
	anchor.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := anchor.Start(); err != nil {
		t.Fatalf("join the test anchor into group %d: %v", pgid, err)
	}
	retired := false
	reapAnchor := func() {
		if killErr := anchor.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			t.Errorf("kill test anchor: %v", killErr)
		}
		if waitErr := anchor.Wait(); waitErr != nil {
			var exitErr *exec.ExitError
			if !errors.As(waitErr, &exitErr) {
				t.Errorf("reap test anchor: %v", waitErr)
			}
		}
	}
	t.Cleanup(func() {
		if retired {
			return
		}
		retired = true
		// The anchor is still unreaped here, pinning pgid, so this
		// failure-path group kill cannot reach a recycled group.
		if killErr := syscall.Kill(-pgid, syscall.SIGKILL); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
			t.Logf("cleanup kill of group %d: %v", pgid, killErr)
		}
		reapAnchor()
	})
	return anchor, func() {
		if retired {
			return
		}
		retired = true
		reapAnchor()
	}
}

func TestGroupProcessesRejectsUninspectableIds(t *testing.T) {
	for _, pgid := range []int{-3, 0, 1} {
		t.Run(strconv.Itoa(pgid), func(t *testing.T) {
			_, err := process.GroupInspector{}.GroupProcesses(t.Context(), pgid)

			if err == nil {
				t.Fatalf("GroupProcesses(%d) succeeded, want an error", pgid)
			}
		})
	}
}

func TestGroupProcessesEmptyForExitedGroup(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "/usr/bin/env")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}

	members, err := process.GroupInspector{}.GroupProcesses(t.Context(), pgid)
	if err != nil {
		t.Fatalf("GroupProcesses: %v", err)
	}
	if len(members) != 0 {
		t.Errorf("members of the exited group = %+v, want none", members)
	}
}

func TestSignalGroupRefusesUnsafeIds(t *testing.T) {
	cases := []struct {
		name string
		pgid int
	}{
		{name: "zero", pgid: 0},
		{name: "init", pgid: 1},
		{name: "negative", pgid: -9},
		{name: "own group", pgid: syscall.Getpgrp()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := process.GroupInspector{}.SignalGroup(t.Context(), tc.pgid)

			if err == nil {
				t.Fatalf("SignalGroup(%d) succeeded, want a refusal", tc.pgid)
			}
		})
	}
}

func TestExecReplacesProcessImage(t *testing.T) {
	exe := testExecutable(t)
	cmd := exec.CommandContext(t.Context(), exe) //nolint:gosec // G204: the executable is this test binary re-run as a fixture child.
	cmd.Env = []string{helperModeVar + "=exec"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	spawnedPid := cmd.Process.Pid

	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper failed: %v\nstderr: %s", err, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, fmt.Sprintf("PID %d\n", spawnedPid)) {
		t.Errorf("replaced image reports a different pid; execve must preserve it\noutput: %s", out)
	}
	if !strings.Contains(out, "ENV HOP_HELPER_MARKER=exec-marker") {
		t.Errorf("replaced image does not carry the exec-provided environment\noutput: %s", out)
	}
	if strings.Contains(out, "ENV "+helperModeVar+"=exec") {
		t.Errorf("replaced image inherited the pre-exec environment; Exec must pass env verbatim\noutput: %s", out)
	}
}

func TestExecResolvedPreservesArgvVerbatim(t *testing.T) {
	exe := testExecutable(t)
	cmd := exec.CommandContext(t.Context(), exe) //nolint:gosec // G204: the executable is this test binary re-run as a fixture child.
	cmd.Env = []string{helperModeVar + "=execresolved"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	spawnedPid := cmd.Process.Pid

	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper failed: %v\nstderr: %s", err, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, fmt.Sprintf("PID %d\n", spawnedPid)) {
		t.Errorf("replaced image reports a different pid; execve must preserve it\noutput: %s", out)
	}
	if !strings.Contains(out, "ARG renamed-argv0\n") || !strings.Contains(out, "ARG frozen-tail\n") {
		t.Errorf("replaced image does not report the caller's verbatim argv\noutput: %s", out)
	}
}

func TestExecResolvedRejectsNonAbsolutePathsAndEmptyArgv(t *testing.T) {
	cases := []struct {
		name string
		path string
		argv []string
	}{
		{name: "empty argv", path: "/bin/sh", argv: nil},
		{name: "bare path", path: "sh", argv: []string{"sh"}},
		{name: "relative path", path: "./sh", argv: []string{"sh"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := process.ExecResolved(tc.path, tc.argv, nil)

			if err == nil {
				t.Fatalf("ExecResolved(%q, %q) reported success; it must never succeed by returning", tc.path, tc.argv)
			}
		})
	}
}

func TestExecRejectsNonAbsoluteExecutables(t *testing.T) {
	cases := []struct {
		name string
		argv []string
	}{
		{name: "empty argv", argv: nil},
		{name: "bare name", argv: []string{"sh"}},
		{name: "relative path", argv: []string{"./sh"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := process.Exec(tc.argv, nil)

			if err == nil {
				t.Fatalf("Exec(%q) reported success; Exec must never succeed by returning", tc.argv)
			}
		})
	}
}

func TestExecReturnsOnFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-binary")

	err := process.Exec([]string{missing}, nil)

	if err == nil {
		t.Fatal("Exec of a missing binary reported success")
	}
	if !errors.Is(err, syscall.ENOENT) {
		t.Errorf("Exec error = %v, want it to wrap ENOENT", err)
	}
}
