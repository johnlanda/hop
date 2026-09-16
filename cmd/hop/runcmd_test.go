package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
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

	t.Run("a signal during an in-flight app call still prints the resume instruction", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.startRun = func(app.StartRunRequest) (app.StartRunResult, app.RunHandle, error) {
			return app.StartRunResult{RunID: testRunID, Sequence: 1}, app.RunHandle{}, nil
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		statusCalls := 0
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			statusCalls++
			if statusCalls == 2 {
				// The signal lands while this Status call is in flight; the
				// call observes its own context's cancellation and returns
				// the canceled error, exactly as the real store would.
				td.signals <- syscall.SIGINT
				<-ctrl.statusCtx().Done()
				return app.StatusResult{}, fmt.Errorf("load run status: %w", ctrl.statusCtx().Err())
			}
			return detailStep("running", "running", false), nil
		}
		var stdout, stderr bytes.Buffer

		code, err := runRun([]string{"brief"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		if !strings.Contains(stdout.String(), "resume with: hop resume "+testRunID) {
			t.Errorf("output lacks the resume instruction:\n%s", stdout.String())
		}
		if strings.Contains(stderr.String(), "context canceled") {
			t.Errorf("the detach was misclassified as an ordinary failure:\n%s", stderr.String())
		}
		detached := false
		for _, call := range ctrl.recorded() {
			if call == "Detach" {
				detached = true
			}
		}
		if !detached {
			t.Error("Detach was not called on the in-flight cancellation path")
		}
	})

	t.Run("a detach carrying a joined retention failure reports it before the resume instruction", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.startRun = func(app.StartRunRequest) (app.StartRunResult, app.RunHandle, error) {
			return app.StartRunResult{RunID: testRunID, Sequence: 1}, app.RunHandle{}, nil
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		td.useCheckBarriers()
		var checkStarted atomic.Bool
		ctrl.claimAndRunCheck = func(ctx context.Context, _ string, _ []string) (app.CheckReport, error) {
			checkStarted.Store(true)
			td.checkStarted <- struct{}{}
			<-ctx.Done()
			// The interrupted round could not retain its output: a real
			// failure that the signal must not silence.
			return app.CheckReport{}, errors.New("app: check output retention failed after an ambiguous execution (spawn error: killed): disk full")
		}
		statusCalls := 0
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			statusCalls++
			if checkStarted.Load() && statusCalls > 1 {
				// The signal lands while this Status call is in flight and
				// its own context observes the cancellation, joining the
				// canceled status error with the drain's retention failure.
				td.signals <- syscall.SIGINT
				<-ctrl.statusCtx().Done()
				return app.StatusResult{}, fmt.Errorf("load run status: %w", ctrl.statusCtx().Err())
			}
			return detailStep("running", "running", false), nil
		}
		var stdout, stderr bytes.Buffer

		code, err := runRun([]string{"brief"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		if !strings.Contains(stderr.String(), "retention failed") {
			t.Errorf("stderr = %q; the joined retention failure was silenced by the cancellation", stderr.String())
		}
		if strings.Contains(stderr.String(), "context canceled") {
			t.Errorf("stderr = %q; the pure cancellation cause must stay out of the report", stderr.String())
		}
		if !strings.Contains(stdout.String(), "resume with: hop resume "+testRunID) {
			t.Errorf("output lacks the resume instruction:\n%s", stdout.String())
		}
	})

	t.Run("a detach carrying a WRAPPED mixed join still reports the retention failure", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.startRun = func(app.StartRunRequest) (app.StartRunResult, app.RunHandle, error) {
			return app.StartRunResult{RunID: testRunID, Sequence: 1}, app.RunHandle{}, nil
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		td.useCheckBarriers()
		var checkStarted atomic.Bool
		ctrl.claimAndRunCheck = func(ctx context.Context, _ string, _ []string) (app.CheckReport, error) {
			checkStarted.Store(true)
			td.checkStarted <- struct{}{}
			<-ctx.Done()
			// The reviewer's probe shape: the retention failure arrives
			// wrapped AROUND a join that also carries the cancellation, so
			// a whole-tree errors.Is reads it as canceled.
			return app.CheckReport{}, fmt.Errorf("run check cleanup: %w", errors.Join(context.Canceled, errors.New("retention failed: disk full")))
		}
		statusCalls := 0
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			statusCalls++
			if checkStarted.Load() && statusCalls > 1 {
				td.signals <- syscall.SIGINT
				<-ctrl.statusCtx().Done()
				return app.StatusResult{}, fmt.Errorf("load run status: %w", ctrl.statusCtx().Err())
			}
			return detailStep("running", "running", false), nil
		}
		var stdout, stderr bytes.Buffer

		code, err := runRun([]string{"brief"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		if !strings.Contains(stderr.String(), "retention failed") {
			t.Errorf("stderr = %q; the wrapped joined retention failure was discarded as a plain cancellation", stderr.String())
		}
		if !strings.Contains(stdout.String(), "resume with: hop resume "+testRunID) {
			t.Errorf("output lacks the resume instruction:\n%s", stdout.String())
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

// TestRunRunWorkflowDispatch pins hop run's --workflow surface (design
// section 10): the resolved workflow selects StartRun and the Phase 2
// loop, or StartFeatureRun and the feature-mode loop, and the exit codes
// keep the Phase 2 discipline for both.
func TestRunRunWorkflowDispatch(t *testing.T) {
	env := map[string]string{"HOME": "/home/u", "PATH": "/bin"}
	started := func(seq int) func(app.StartRunRequest) (app.StartRunResult, app.RunHandle, error) {
		return func(app.StartRunRequest) (app.StartRunResult, app.RunHandle, error) {
			return app.StartRunResult{RunID: testRunID, Sequence: seq}, app.RunHandle{}, nil
		}
	}
	contains := func(calls []string, name string) bool {
		for _, call := range calls {
			if call == name {
				return true
			}
		}
		return false
	}

	for _, tt := range []struct {
		name         string
		args         []string
		resolved     string
		wantOverride string
		wantFeature  bool
	}{
		{"no flag, solo policy: StartRun and the solo loop", []string{"brief"}, app.WorkflowModeSolo, "", false},
		{"no flag, feature policy: StartFeatureRun and the feature loop", []string{"brief"}, app.WorkflowModeFeature, "", true},
		{"--workflow feature overrides the policy", []string{"-workflow", "feature", "brief"}, app.WorkflowModeFeature, app.WorkflowModeFeature, true},
		{"--workflow solo overrides the policy", []string{"--workflow=solo", "brief"}, app.WorkflowModeSolo, app.WorkflowModeSolo, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := &fakeController{}
			var gotOverride, gotRoot string
			ctrl.resolveRunWorkflow = func(root, override string) (string, error) {
				gotRoot, gotOverride = root, override
				return tt.resolved, nil
			}
			var request app.StartRunRequest
			start := func(req app.StartRunRequest) (app.StartRunResult, app.RunHandle, error) {
				request = req
				return started(4)(req)
			}
			if tt.wantFeature {
				ctrl.startFeatureRun = start
			} else {
				ctrl.startRun = start
			}
			ctrl.status = scriptStatus(detailStep("running", "running", false), detailStep("completed", "completed", false))
			repo := t.TempDir()
			td := newTestDeps(ctrl, env, repo)
			var stdout, stderr bytes.Buffer

			code, err := runRun(tt.args, &stdout, &stderr, td.deps)
			if err != nil {
				t.Fatalf("write error: %v", err)
			}
			if code != exitOK {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
			}
			if gotOverride != tt.wantOverride || gotRoot != request.RepositoryRoot || gotRoot == "" {
				t.Fatalf("ResolveRunWorkflow(%q, %q), start root %q; want the override %q and the resolved repository root", gotRoot, gotOverride, request.RepositoryRoot, tt.wantOverride)
			}
			if request.Brief != "brief" || request.HOPPath != "/opt/hop/bin/hop" || request.StateRoot != "/home/u/.local/state/hop" || request.ControllerID == "" {
				t.Fatalf("start request = %+v", request)
			}
			if !strings.Contains(stdout.String(), "run r4 "+testRunID+" started\n") {
				t.Fatalf("stdout lacks the start line:\n%s", stdout.String())
			}
			calls := ctrl.recorded()
			if contains(calls, "StartRun") == tt.wantFeature || contains(calls, "StartFeatureRun") != tt.wantFeature {
				t.Fatalf("calls = %v; want exactly the %v start", calls, map[bool]string{true: "feature", false: "solo"}[tt.wantFeature])
			}
			if contains(calls, "CorroborateSessionLaunches") != tt.wantFeature || contains(calls, "ClaimAndRunCheck") == tt.wantFeature {
				t.Fatalf("calls = %v; want the %s loop", calls, map[bool]string{true: "feature", false: "solo"}[tt.wantFeature])
			}
		})
	}

	for _, tt := range []struct {
		name     string
		args     []string
		resolve  func(string, string) (string, error)
		start    func(app.StartRunRequest) (app.StartRunResult, app.RunHandle, error)
		status   []app.StatusResult
		wantCode int
		wantErr  string
		wantOpen bool
	}{
		{
			name: "an unknown --workflow is a usage error before the store opens",
			args: []string{"-workflow", "parallel", "brief"}, wantCode: exitUsage,
			wantErr: "--workflow must be solo or feature",
		},
		{
			name:     "a refused resolution is a usage error",
			args:     []string{"brief"},
			resolve:  func(string, string) (string, error) { return "", fmt.Errorf("%w: bad", app.ErrStartRefused) },
			wantCode: exitUsage, wantErr: "hop run:", wantOpen: true,
		},
		{
			name:    "a feature refusal before any side effect is a usage error",
			args:    []string{"-workflow", "feature", "brief"},
			resolve: func(_, o string) (string, error) { return o, nil },
			start: func(app.StartRunRequest) (app.StartRunResult, app.RunHandle, error) {
				return app.StartRunResult{}, app.RunHandle{}, fmt.Errorf("%w: load repository policy: bad key", app.ErrStartRefused)
			},
			wantCode: exitUsage, wantErr: "hop run: app: run refused before any side effect: load repository policy: bad key", wantOpen: true,
		},
		{
			name:    "a feature failure after InitializeRun exits 1",
			args:    []string{"-workflow", "feature", "brief"},
			resolve: func(_, o string) (string, error) { return o, nil },
			start: func(app.StartRunRequest) (app.StartRunResult, app.RunHandle, error) {
				return app.StartRunResult{}, app.RunHandle{}, fmt.Errorf("%w: refs/heads/hop/r1/integration is already present", app.ErrIntegrationBranchExists)
			},
			wantCode: exitFailure, wantErr: "refs/heads/hop/r1/integration", wantOpen: true,
		},
		{
			name:     "a failed feature run exits 1",
			args:     []string{"-workflow", "feature", "brief"},
			resolve:  func(_, o string) (string, error) { return o, nil },
			start:    started(1),
			status:   []app.StatusResult{detailStep("launching", "", false), detailStep("failed", "", false)},
			wantCode: exitFailure, wantOpen: true,
		},
		{
			name:     "a stopped feature run exits 1",
			args:     []string{"-workflow", "feature", "brief"},
			resolve:  func(_, o string) (string, error) { return o, nil },
			start:    started(1),
			status:   []app.StatusResult{detailStep("stopped", "", false)},
			wantCode: exitFailure, wantOpen: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := &fakeController{resolveRunWorkflow: tt.resolve, startFeatureRun: tt.start}
			if len(tt.status) > 0 {
				ctrl.status = scriptStatus(tt.status...)
			}
			td := newTestDeps(ctrl, env, t.TempDir())
			var stdout, stderr bytes.Buffer

			code, err := runRun(tt.args, &stdout, &stderr, td.deps)
			if err != nil {
				t.Fatalf("write error: %v", err)
			}
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), tt.wantErr)
			}
			if opened := len(td.openCalls) > 0; opened != tt.wantOpen {
				t.Fatalf("store opened = %v, want %v", opened, tt.wantOpen)
			}
			if tt.wantOpen && !td.openCalls[0].withRuntime {
				t.Fatalf("hop run must wire the runtime for either workflow")
			}
		})
	}

	t.Run("SIGINT detaches a feature run without stopping it", func(t *testing.T) {
		ctrl := &fakeController{startFeatureRun: started(2)}
		td := newTestDeps(ctrl, env, t.TempDir())
		statusCalls := 0
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			statusCalls++
			if statusCalls == 2 {
				td.signals <- syscall.SIGINT
			}
			return detailStep("launching", "", false), nil
		}
		var stdout, stderr bytes.Buffer

		code, err := runRun([]string{"-workflow", "feature", "brief"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure || !strings.Contains(stdout.String(), "resume with: hop resume "+testRunID) {
			t.Fatalf("exit %d, stdout %q; want the detach exit and the resume instruction", code, stdout.String())
		}
		calls := ctrl.recorded()
		if !contains(calls, "Detach") || contains(calls, "RequestStop") || contains(calls, "DriveFeatureStop") {
			t.Fatalf("calls = %v; a signal detaches and never stops", calls)
		}
	})
}
