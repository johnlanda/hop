package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

const (
	testAttemptID     = "33333333-3333-4333-8333-333333333333"
	testIncarnationID = "44444444-4444-4444-8444-444444444444"
	testOperationID   = "66666666-6666-4666-8666-666666666666"
)

func TestRunLaunch(t *testing.T) {
	workerEnv := map[string]string{
		"HOP_STATE_DIR":      "/state/root",
		"HOP_INCARNATION_ID": testIncarnationID,
		"PATH":               "/bin",
	}

	t.Run("prepares, claims and execs the plan; a failed exec settles exec_failed", func(t *testing.T) {
		ctrl := &fakeController{}
		var prepared app.SessionLaunchExecRequest
		ctrl.prepareSessionLaunch = func(req app.SessionLaunchExecRequest) (app.LaunchExecPlan, error) {
			prepared = req
			return app.LaunchExecPlan{
				Argv:          []string{"/resolved/claude", "--session-id", "ref", "prompt"},
				Env:           []string{"PATH=/bin"},
				IncarnationID: testIncarnationID,
			}, nil
		}
		var settledIncarnation, settledReason string
		ctrl.failLaunch = func(incarnationID, reason string) error {
			settledIncarnation, settledReason = incarnationID, reason
			return nil
		}
		dir := t.TempDir()
		td := newTestDeps(ctrl, workerEnv, dir)
		var stdout, stderr bytes.Buffer

		code, err := runLaunch([]string{"--run", testRunID, "--attempt", testAttemptID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		// The fake exec records and fails, so the command exits 1 with the
		// claim settled exec_failed — the design's post-claim failure path.
		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		if prepared.RunID != testRunID || prepared.AttemptID != testAttemptID {
			t.Errorf("prepared ids = %q/%q", prepared.RunID, prepared.AttemptID)
		}
		if prepared.PID != 4242 || prepared.HOPPath != "/opt/hop/bin/hop" || prepared.LookupExecutable == nil {
			t.Errorf("prepared request = %+v", prepared)
		}
		resolvedDir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		if prepared.WorkerDir != resolvedDir {
			t.Errorf("prepared worker dir = %q, want the symlink-resolved working directory %q", prepared.WorkerDir, resolvedDir)
		}
		if prepared.ResolvePath == nil {
			t.Error("no path resolver was wired into the request")
		}
		if len(prepared.Environ) == 0 {
			t.Error("the launcher's inherited environment was not passed for validation and sanitization")
		}
		execs := td.recordedExecs()
		if len(execs) != 1 || execs[0].Argv[0] != "/resolved/claude" || len(execs[0].Env) != 1 {
			t.Fatalf("execs = %+v", execs)
		}
		if settledIncarnation != testIncarnationID || !strings.Contains(settledReason, "exec recorded") {
			t.Errorf("settlement = %q %q; the exec failure must settle exec_failed", settledIncarnation, settledReason)
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout = %q, want empty (diagnostics go to stderr)", stdout.String())
		}
	})

	t.Run("a refused preparation never execs and never settles", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.prepareSessionLaunch = func(app.SessionLaunchExecRequest) (app.LaunchExecPlan, error) {
			return app.LaunchExecPlan{}, errors.New("app: HOP_INCARNATION_ID does not match")
		}
		td := newTestDeps(ctrl, workerEnv, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runLaunch([]string{"--run", testRunID, "--attempt", testAttemptID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		if execs := td.recordedExecs(); len(execs) != 0 {
			t.Errorf("a refused launch reached the exec seam: %+v", execs)
		}
		for _, call := range ctrl.recorded() {
			if call == "FailLaunchExec" {
				t.Error("a pre-claim refusal settled a claim that does not exist")
			}
		}
		if got := strings.Count(stderr.String(), "\n"); got != 1 {
			t.Errorf("stderr lines = %d, want exactly one diagnostic line:\n%s", got, stderr.String())
		}
	})

	t.Run("a relative HOP_STATE_DIR names the variable, never the value", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, map[string]string{"HOP_STATE_DIR": "s3kr3t-pasted/rel"}, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runLaunch([]string{"--run", testRunID, "--attempt", testAttemptID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		if !strings.Contains(stderr.String(), "HOP_STATE_DIR") || strings.Contains(stderr.String(), "s3kr3t") {
			t.Errorf("stderr = %q; the diagnostic must name the variable and never its value", stderr.String())
		}
		if len(td.openCalls) != 0 {
			t.Error("the store was opened with a refused state root")
		}
	})

	t.Run("a failed getwd is a diagnostic failure that never echoes the raw error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, workerEnv, t.TempDir())
		td.deps.getwd = func() (string, error) { return "", errors.New("getwd: s3kr3t-cwd vanished") }
		var stdout, stderr bytes.Buffer

		code, err := runLaunch([]string{"--run", testRunID, "--attempt", testAttemptID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		if !strings.Contains(stderr.String(), "working directory could not be determined") {
			t.Errorf("stderr = %q, want the fixed working-directory diagnostic", stderr.String())
		}
		if strings.Contains(stderr.String(), "s3kr3t") {
			t.Errorf("stderr echoes the raw getwd error: %q", stderr.String())
		}
		if len(td.openCalls) != 0 {
			t.Error("the store was opened although the worker directory never resolved")
		}
	})

	t.Run("a path-bearing symlink-resolution failure names a category, never the path", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "sensitive-workspace-does-not-exist")
		td := newTestDeps(&fakeController{}, workerEnv, t.TempDir())
		td.deps.getwd = func() (string, error) { return missing, nil }
		var stdout, stderr bytes.Buffer

		code, err := runLaunch([]string{"--run", testRunID, "--attempt", testAttemptID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		out := stderr.String()
		if !strings.Contains(out, "could not be canonically resolved") || !strings.Contains(out, "a path element does not exist") {
			t.Errorf("stderr = %q, want the fixed resolution diagnostic with its category", out)
		}
		if strings.Contains(out, "sensitive-workspace") || strings.Contains(out, missing) || strings.Contains(out, filepath.Dir(missing)) {
			t.Errorf("stderr echoes the failing path or a component of it: %q", out)
		}
		if len(td.openCalls) != 0 {
			t.Error("the store was opened although the worker directory never resolved")
		}
	})

	t.Run("missing HOP_STATE_DIR is a diagnostic failure before any store access", func(t *testing.T) {
		ctrl := &fakeController{}
		td := newTestDeps(ctrl, map[string]string{"HOME": "/home/u"}, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runLaunch([]string{"--run", testRunID, "--attempt", testAttemptID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		if !strings.Contains(stderr.String(), "HOP_STATE_DIR is not set") {
			t.Errorf("stderr = %q, want the lost-variable diagnostic", stderr.String())
		}
		if len(td.openCalls) != 0 {
			t.Error("the store was opened without a worker state root; a worker context never falls back")
		}
	})

	usage := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing flags", args: nil, want: "--run is required"},
		{name: "missing address", args: []string{"--run", testRunID}, want: "exactly one of --attempt and --session is required"},
		{name: "both addresses", args: []string{"--run", testRunID, "--attempt", testAttemptID, "--session", testIncarnationID}, want: "exactly one of --attempt and --session is required"},
		{name: "unexpected positional", args: []string{"--run", testRunID, "--attempt", testAttemptID, "extra"}, want: "unexpected argument"},
		{name: "unknown flag", args: []string{"-json"}, want: "flag provided but not defined"},
	}
	for _, tc := range usage {
		t.Run(tc.name, func(t *testing.T) {
			td := newTestDeps(&fakeController{}, workerEnv, t.TempDir())
			var stdout, stderr bytes.Buffer

			code, err := runLaunch(tc.args, &stdout, &stderr, td.deps)
			if err != nil {
				t.Fatalf("write error: %v", err)
			}

			if code != exitUsage {
				t.Errorf("exit code = %d, want %d", code, exitUsage)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.want)
			}
		})
	}
}

func TestRunCheckExec(t *testing.T) {
	workerEnv := map[string]string{"HOP_STATE_DIR": "/state/root", "PATH": "/bin"}

	t.Run("claims through preparation, then execs the frozen argv with the resolved path", func(t *testing.T) {
		ctrl := &fakeController{}
		var prepared app.CheckExecRequest
		ctrl.prepareCheck = func(req app.CheckExecRequest) (app.CheckExecPlan, error) {
			prepared = req
			return app.CheckExecPlan{ExecPath: "/bin/sh", Argv: []string{"sh", "check.sh"}, Env: []string{"PATH=/bin"}}, nil
		}
		td := newTestDeps(ctrl, workerEnv, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runCheckExec([]string{"--op", testOperationID, "--", "sh", "check.sh"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitFailure { // the fake exec records and returns
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		if prepared.OperationID != testOperationID || prepared.PID != 4242 || !prepared.LeadsProcessGroup {
			t.Errorf("prepared = %+v", prepared)
		}
		if len(prepared.CheckArgv) != 2 || prepared.CheckArgv[0] != "sh" || prepared.CheckArgv[1] != "check.sh" {
			t.Errorf("check argv = %q", prepared.CheckArgv)
		}
		execs := td.recordedExecs()
		if len(execs) != 1 || execs[0].Path != "/bin/sh" || execs[0].Argv[0] != "sh" {
			t.Fatalf("execs = %+v; the frozen argv must stay verbatim with the path resolved separately", execs)
		}
		calls := ctrl.recorded()
		if len(calls) == 0 || calls[len(calls)-1] != "PrepareCheckExec" {
			t.Errorf("calls = %v; preparation (which claims before exec) must be the only controller call", calls)
		}
	})

	t.Run("a supervisor that does not lead its group is refused by preparation, no exec", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.prepareCheck = func(req app.CheckExecRequest) (app.CheckExecPlan, error) {
			if req.LeadsProcessGroup {
				t.Error("the command must report the real group-leadership fact")
			}
			return app.CheckExecPlan{}, errors.New("app: hop check-exec does not lead its own process group")
		}
		td := newTestDeps(ctrl, workerEnv, t.TempDir())
		td.deps.leadsGroup = func() bool { return false }
		var stdout, stderr bytes.Buffer

		code, err := runCheckExec([]string{"--op", testOperationID, "--", "sh", "check.sh"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		if execs := td.recordedExecs(); len(execs) != 0 {
			t.Errorf("a refused supervisor reached the exec seam: %+v", execs)
		}
	})

	t.Run("missing HOP_STATE_DIR is refused before any store access", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, map[string]string{"HOME": "/home/u"}, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runCheckExec([]string{"--op", testOperationID, "--", "sh"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		if len(td.openCalls) != 0 {
			t.Error("the store was opened without a worker state root")
		}
	})

	usage := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing op", args: []string{"--", "sh"}, want: "--op is required"},
		{name: "missing argv", args: []string{"--op", testOperationID}, want: "the check argv is required after --"},
	}
	for _, tc := range usage {
		t.Run(tc.name, func(t *testing.T) {
			td := newTestDeps(&fakeController{}, workerEnv, t.TempDir())
			var stdout, stderr bytes.Buffer

			code, err := runCheckExec(tc.args, &stdout, &stderr, td.deps)
			if err != nil {
				t.Fatalf("write error: %v", err)
			}

			if code != exitUsage {
				t.Errorf("exit code = %d, want %d", code, exitUsage)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.want)
			}
		})
	}
}
