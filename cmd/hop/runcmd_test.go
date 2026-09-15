package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

func TestRunRun(t *testing.T) {
	env := map[string]string{"HOME": "/home/u", "HERDR_SOCKET_PATH": "/tmp/herdr.sock"}

	t.Run("starts, prints the start line and exits by the terminal state", func(t *testing.T) {
		ctrl := &fakeController{}
		var started app.StartRunRequest
		ctrl.startRun = func(req app.StartRunRequest) (app.StartRunResult, app.RunHandle, error) {
			started = req
			return app.StartRunResult{RunID: testRunID, Sequence: 3}, app.RunHandle{}, nil
		}
		ctrl.status = scriptStatus(
			detailStep("launching", "launching", false),
			detailStep("completed", "completed", false),
		)
		repo := t.TempDir()
		td := newTestDeps(ctrl, env, repo)
		var stdout, stderr bytes.Buffer

		code, err := runRun([]string{"-env-passthrough", "FOO", "-env-passthrough", "BAR", "my brief"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitOK {
			t.Errorf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
		}
		if !strings.Contains(stdout.String(), "run r3 "+testRunID+" started\n") {
			t.Errorf("output lacks the start line:\n%s", stdout.String())
		}
		if started.Brief != "my brief" || started.HOPPath != "/opt/hop/bin/hop" {
			t.Errorf("start request = %+v", started)
		}
		if len(started.EnvPassthrough) != 2 || started.EnvPassthrough[0] != "FOO" || started.EnvPassthrough[1] != "BAR" {
			t.Errorf("passthrough = %q", started.EnvPassthrough)
		}
		if started.StateRoot != "/home/u/.local/state/hop" {
			t.Errorf("state root = %q, want the resolver's default", started.StateRoot)
		}
		if started.ControllerID == "" {
			t.Error("controller id is empty")
		}
		if !strings.Contains(started.RepositoryRoot, "/") || strings.HasSuffix(started.RepositoryRoot, "/.") {
			t.Errorf("repository root = %q", started.RepositoryRoot)
		}
		if len(td.openCalls) != 1 || !td.openCalls[0].withRuntime || td.openCalls[0].socketPath != "/tmp/herdr.sock" {
			t.Errorf("open calls = %+v; hop run wires the runtime against the configured socket", td.openCalls)
		}
	})

	t.Run("a pre-side-effect refusal is a usage error", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.startRun = func(app.StartRunRequest) (app.StartRunResult, app.RunHandle, error) {
			return app.StartRunResult{}, app.RunHandle{}, fmt.Errorf("%w: repository policy has no [check] command", app.ErrStartRefused)
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runRun([]string{"brief"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})

	t.Run("a failure after the run started exits 1", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.startRun = func(app.StartRunRequest) (app.StartRunResult, app.RunHandle, error) {
			return app.StartRunResult{}, app.RunHandle{}, errors.New("app: pane.open: transport lost (operation op-1 is reconciling)")
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runRun([]string{"brief"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
	})

	t.Run("SIGINT detaches: the lease is released, the run is never stopped", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.startRun = func(app.StartRunRequest) (app.StartRunResult, app.RunHandle, error) {
			return app.StartRunResult{RunID: testRunID, Sequence: 1}, app.RunHandle{}, nil
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		statusCalls := 0
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			statusCalls++
			if statusCalls == 2 {
				td.signals <- syscall.SIGINT
			}
			return detailStep("running", "running", false), nil
		}
		var stdout, stderr bytes.Buffer

		code, err := runRun([]string{"brief"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitFailure {
			t.Errorf("exit code = %d, want %d (detach exits 1)", code, exitFailure)
		}
		if !strings.Contains(stdout.String(), "resume with: hop resume "+testRunID) {
			t.Errorf("output lacks the resume instruction:\n%s", stdout.String())
		}
		detached := false
		for _, call := range ctrl.recorded() {
			if call == "Detach" {
				detached = true
			}
			if call == "RequestStop" || call == "DriveStop" {
				t.Errorf("%s called on a signal; signals detach, they never stop", call)
			}
		}
		if !detached {
			t.Error("Detach was not called on the signal path")
		}
	})

	usage := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing brief", args: nil, want: "exactly one brief argument"},
		{name: "two positionals", args: []string{"a", "b"}, want: "exactly one brief argument"},
		{name: "unknown flag", args: []string{"-json", "brief"}, want: "flag provided but not defined"},
	}
	for _, tc := range usage {
		t.Run(tc.name, func(t *testing.T) {
			td := newTestDeps(&fakeController{}, env, t.TempDir())
			var stdout, stderr bytes.Buffer

			code, err := runRun(tc.args, &stdout, &stderr, td.deps)
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

	t.Run("a relative HOP_STATE_DIR override is refused", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, map[string]string{"HOME": "/home/u", "HOP_STATE_DIR": "rel"}, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runRun([]string{"brief"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
		if !strings.Contains(stderr.String(), "HOP_STATE_DIR is set to a relative path") {
			t.Errorf("stderr = %q", stderr.String())
		}
		if strings.Contains(stderr.String(), "rel") && strings.Contains(stderr.String(), `"rel"`) {
			t.Errorf("stderr echoes the refused value: %q", stderr.String())
		}
	})
}
