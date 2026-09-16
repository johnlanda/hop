package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// TestRunFeatureControllerLoop proves the section 6 scheduling pass runs
// in the design's deterministic order once per tick, that check-driving
// runs asynchronously after the pass, and that the loop reports
// transitions and exits on the terminal state — the loop-ordering
// contract slice 6 owes for the feature-mode loop.
func TestRunFeatureControllerLoop(t *testing.T) {
	t.Run("runs the scheduling pass in order, corroborates and drives checks, then exits terminal", func(t *testing.T) {
		ctrl := &fakeController{}
		td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
		td.useCheckBarriers()

		var checkCalls atomic.Int32
		ctrl.driveFeatureChecks = func(ctx context.Context, hopPath string, spawnEnv []string) (app.FeatureCheckReport, error) {
			if hopPath != "/opt/hop/bin/hop" {
				t.Errorf("hop path = %q", hopPath)
			}
			if len(spawnEnv) == 0 {
				t.Error("spawn env is empty; the sanitized environment must be passed through")
			}
			if checkCalls.Add(1) == 1 {
				td.checkStarted <- struct{}{}
			}
			<-ctx.Done()
			return app.FeatureCheckReport{}, nil
		}
		statusCalls := 0
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			statusCalls++
			if statusCalls == 1 {
				return detailStep("running", "", false), nil
			}
			return detailStep("completed", "", false), nil
		}
		var stdout bytes.Buffer

		result, err := runFeatureControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
		if err != nil {
			t.Fatalf("runFeatureControllerLoop: %v", err)
		}
		if result.Detached || result.FinalState != "completed" {
			t.Errorf("result = %+v", result)
		}
		if got := stdout.String(); got != "run r1 running\nrun r1 completed\n" {
			t.Errorf("stdout = %q", got)
		}

		calls := ctrl.recorded()
		firstIndex := map[string]int{}
		for i, name := range calls {
			if _, seen := firstIndex[name]; !seen {
				firstIndex[name] = i
			}
		}
		orderedSteps := []string{
			"RetireSettledSessions", "RecomputeReleases", "DriveIntegration",
			"EnsureReviewTask", "DriveCompletion", "AssignReadyTasks",
			"CorroborateSessionLaunches", "DriveFeatureChecks",
		}
		for _, name := range orderedSteps {
			if _, ok := firstIndex[name]; !ok {
				t.Fatalf("%s was never called; calls = %v", name, calls)
			}
		}
		for i := 1; i < len(orderedSteps); i++ {
			prev, cur := orderedSteps[i-1], orderedSteps[i]
			if firstIndex[prev] >= firstIndex[cur] {
				t.Errorf("%s (at %d) did not run before %s (at %d); calls = %v", prev, firstIndex[prev], cur, firstIndex[cur], calls)
			}
		}
	})

	t.Run("stop takes precedence and interrupts an in-flight check", func(t *testing.T) {
		ctrl := &fakeController{}
		td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
		td.useCheckBarriers()

		ctrl.driveFeatureChecks = func(ctx context.Context, _ string, _ []string) (app.FeatureCheckReport, error) {
			td.checkStarted <- struct{}{}
			<-ctx.Done()
			return app.FeatureCheckReport{Interrupted: true}, nil
		}
		statusCalls := 0
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			statusCalls++
			if statusCalls == 1 {
				return detailStep("running", "", false), nil
			}
			return detailStep("stopping", "", true), nil
		}
		ctrl.driveFeatureStop = func() (app.StopReport, error) {
			return app.StopReport{RunState: "stopped", Terminated: true}, nil
		}
		var stdout bytes.Buffer

		result, err := runFeatureControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
		if err != nil {
			t.Fatalf("runFeatureControllerLoop: %v", err)
		}
		if result.FinalState != "stopped" {
			t.Errorf("result = %+v", result)
		}
		calls := ctrl.recorded()
		stopAt := -1
		for i, name := range calls {
			if name == "DriveFeatureStop" {
				stopAt = i
				break
			}
		}
		if stopAt == -1 {
			t.Fatalf("DriveFeatureStop was never called; calls = %v", calls)
		}
		for _, name := range calls[stopAt+1:] {
			if name == "AssignReadyTasks" {
				t.Errorf("AssignReadyTasks ran after a stop was observed; the scheduling pass must not run once stopping: calls = %v", calls)
			}
		}
	})

	t.Run("a heartbeat failure cancels in-flight work and reports the error", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.status = scriptStatus(detailStep("running", "", false))
		ctrl.heartbeat = func() error { return errors.New("fenced") }
		td := newTestDeps(ctrl, nil, t.TempDir())
		// Let the heartbeat goroutine run: its interval wait returns
		// immediately instead of blocking, mirroring
		// TestRunControllerLoop's identical solo-loop scenario.
		td.deps.wait = func(ctx context.Context, _ time.Duration) error {
			return ctx.Err()
		}
		var stdout bytes.Buffer

		_, err := runFeatureControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
		if err == nil || !strings.Contains(err.Error(), "heartbeat failed") {
			t.Fatalf("err = %v, want the heartbeat failure", err)
		}
	})

	t.Run("context cancellation detaches", func(t *testing.T) {
		ctrl := &fakeController{}
		td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return detailStep("running", "", false), nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var stdout bytes.Buffer

		result, err := runFeatureControllerLoop(ctx, td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
		if err != nil {
			t.Fatalf("runFeatureControllerLoop: %v", err)
		}
		if !result.Detached {
			t.Errorf("result = %+v, want Detached", result)
		}
	})
}

// TestFinishFeatureControllerLoop proves the exit-code mapping mirrors
// finishControllerLoop's solo shape: completed exits 0, everything else 1,
// a detach prints the resume instruction.
func TestFinishFeatureControllerLoop(t *testing.T) {
	t.Run("completed exits 0", func(t *testing.T) {
		ctrl := &fakeController{}
		td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return detailStep("completed", "", false), nil
		}
		var stdout, stderr bytes.Buffer

		code, err := finishFeatureControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout, &stderr, "hop run")
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK {
			t.Errorf("exit code = %d, want %d", code, exitOK)
		}
	})

	t.Run("failed exits 1", func(t *testing.T) {
		ctrl := &fakeController{}
		td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return detailStep("failed", "", false), nil
		}
		var stdout, stderr bytes.Buffer

		code, err := finishFeatureControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout, &stderr, "hop run")
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
	})
}
