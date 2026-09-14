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

// cleanupGroup registers a guarded best-effort group kill so a failing test
// never leaves fixture processes behind.
func cleanupGroup(t *testing.T, pgid int) {
	t.Helper()
	t.Cleanup(func() {
		if pgid <= 1 || pgid == syscall.Getpgrp() {
			return
		}
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Logf("cleanup kill of group %d: %v", pgid, err)
		}
	})
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
	}()

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
	cleanupGroup(t, pgid)
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
	cleanupGroup(t, pgid)

	inspector := process.GroupInspector{}
	members, err := inspector.GroupProcesses(t.Context(), pgid)
	if err != nil {
		t.Fatalf("GroupProcesses: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("live group members = %+v, want exactly the sleeper child", members)
	}
	if members[0].PID != childPid {
		t.Errorf("member pid = %d, want %d", members[0].PID, childPid)
	}
	wantArgv := []string{exe, spaceArg, tabArg}
	if !slices.Equal(members[0].Argv, wantArgv) {
		t.Errorf("member argv = %q, want the exact vector %q", members[0].Argv, wantArgv)
	}

	if signalErr := inspector.SignalGroup(t.Context(), pgid); signalErr != nil {
		t.Fatalf("SignalGroup: %v", signalErr)
	}
	waitUntil(t, "the retired group to empty", func() bool { return groupGone(pgid) })
	members, err = inspector.GroupProcesses(t.Context(), pgid)
	if err != nil {
		t.Fatalf("GroupProcesses after retirement: %v", err)
	}
	if len(members) != 0 {
		t.Errorf("retired group members = %+v, want none", members)
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
