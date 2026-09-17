package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// indexOf returns the index of the first recorded call named name, or -1.
func indexOf(calls []string, name string) int {
	for i, call := range calls {
		if call == name {
			return i
		}
	}
	return -1
}

// TestFeatureLoopStopInterruptsAnInFlightCombinedCheck proves a stop is
// never held behind the integration step's combined check: the check runs
// as an asynchronous round (the fake holds it open the way the runner
// holds a running check-exec child, until its context ends), the loop
// keeps ticking meanwhile without re-entering the integration step, and
// the stop branch interrupts and awaits the round before DriveFeatureStop.
func TestFeatureLoopStopInterruptsAnInFlightCombinedCheck(t *testing.T) {
	ctrl := &fakeController{}
	td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
	td.useCheckBarriers()

	var started atomic.Bool
	ctrl.driveIntegration = func(context.Context, string, []string) (app.IntegrationReport, error) {
		return app.IntegrationReport{IntegrationID: "integration-1", State: "checking", CheckDue: true}, nil
	}
	ctrl.driveIntegrationCheck = func(ctx context.Context, hopPath string, spawnEnv []string) (app.IntegrationReport, error) {
		if hopPath != "/opt/hop/bin/hop" || len(spawnEnv) == 0 {
			t.Errorf("combined-check round got hop path %q and spawn env %v; want the loop's own", hopPath, spawnEnv)
		}
		started.Store(true)
		td.checkStarted <- struct{}{}
		<-ctx.Done()
		ctrl.record("combined check canceled")
		return app.IntegrationReport{}, fmt.Errorf("app: combined-check execution ambiguous: %w", context.Cause(ctx))
	}
	ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
		if started.Load() {
			return detailStep("stopping", "", true), nil
		}
		return detailStep("running", "", false), nil
	}
	ctrl.driveFeatureStop = func() (app.StopReport, error) {
		return app.StopReport{RunState: "stopped", Terminated: true}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout bytes.Buffer
	result, err := runFeatureControllerLoop(ctx, td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
	if err != nil {
		t.Fatalf("runFeatureControllerLoop: %v (the round's cancellation is the loop's own doing and never reported)", err)
	}
	if result.Detached {
		t.Fatalf("the loop never drove the stop while the combined check was in flight")
	}
	if result.FinalState != "stopped" {
		t.Fatalf("result = %+v, want stopped", result)
	}

	calls := ctrl.recorded()
	canceledAt, stopAt := indexOf(calls, "combined check canceled"), indexOf(calls, "DriveFeatureStop")
	if canceledAt == -1 || stopAt == -1 || canceledAt > stopAt {
		t.Fatalf("the combined check was not interrupted and awaited before DriveFeatureStop; calls = %v", calls)
	}
	if got := countCalls(calls, "DriveIntegrationCheck"); got != 1 {
		t.Errorf("DriveIntegrationCheck ran %d times, want one round; calls = %v", got, calls)
	}
	if got := countCalls(calls, "DriveIntegration"); got != 1 {
		t.Errorf("DriveIntegration ran %d times, want once: never while the round owned the step; calls = %v", got, calls)
	}
}

// TestFeatureLoopRunsTheCombinedCheckAsARound proves the scheduling pass
// hands a due combined check to an asynchronous round, leaves the
// integration step alone for as long as that round runs — however many
// ticks — and resumes the step on the tick that consumes the round.
func TestFeatureLoopRunsTheCombinedCheckAsARound(t *testing.T) {
	ctrl := &fakeController{}
	td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
	td.useCheckBarriers()

	var (
		scripted  atomic.Int32
		releaseMu sync.Mutex
		release   = make(chan struct{})
		released  bool
	)
	ctrl.driveIntegration = func(context.Context, string, []string) (app.IntegrationReport, error) {
		if scripted.Add(1) == 1 {
			return app.IntegrationReport{IntegrationID: "integration-1", State: "checking", CheckDue: true}, nil
		}
		return app.IntegrationReport{IntegrationID: "integration-1", State: "rolled-back"}, nil
	}
	ctrl.driveIntegrationCheck = func(context.Context, string, []string) (app.IntegrationReport, error) {
		td.checkStarted <- struct{}{}
		<-release
		ctrl.record("combined check returned")
		return app.IntegrationReport{IntegrationID: "integration-1", State: "check-failed"}, nil
	}
	ticks := 0
	ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
		ticks++
		if ticks == 4 {
			releaseMu.Lock()
			if !released {
				close(release)
				released = true
			}
			releaseMu.Unlock()
		}
		if scripted.Load() >= 2 {
			return detailStep("completed", "", false), nil
		}
		return detailStep("running", "", false), nil
	}
	t.Cleanup(func() {
		releaseMu.Lock()
		defer releaseMu.Unlock()
		if !released {
			close(release)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout bytes.Buffer
	result, err := runFeatureControllerLoop(ctx, td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
	if err != nil {
		t.Fatalf("runFeatureControllerLoop: %v", err)
	}
	if result.FinalState != "completed" {
		t.Fatalf("result = %+v, want completed", result)
	}

	calls := ctrl.recorded()
	if recorded := countCalls(calls, "DriveIntegration"); recorded != int(scripted.Load()) {
		t.Fatalf("DriveIntegration was called %d times but acted %d times: the loop entered the step while the round held it; calls = %v", recorded, scripted.Load(), calls)
	}
	checkAt, returnedAt := indexOf(calls, "DriveIntegrationCheck"), indexOf(calls, "combined check returned")
	if checkAt == -1 || returnedAt == -1 {
		t.Fatalf("the combined-check round never ran to completion; calls = %v", calls)
	}
	if got := countCalls(calls[checkAt:returnedAt], "DriveIntegration"); got != 0 {
		t.Errorf("DriveIntegration ran %d time(s) while the round was in flight; calls = %v", got, calls)
	}
	if got := countCalls(calls[checkAt:returnedAt], "AssignReadyTasks"); got < 2 {
		t.Errorf("the pass ran %d time(s) while the round was in flight, want it to keep ticking; calls = %v", got, calls)
	}
	if countCalls(calls[returnedAt:], "DriveIntegration") != 1 {
		t.Errorf("the tick that consumed the round did not resume the integration step; calls = %v", calls)
	}
	if got := countCalls(calls, "DriveIntegrationCheck"); got != 1 {
		t.Errorf("DriveIntegrationCheck ran %d times, want exactly the one due round; calls = %v", got, calls)
	}
}

// TestFeatureLoopReportsACombinedCheckFailure proves a combined-check
// round's own error — neither a cancellation nor a stop refusal — ends the
// loop with the error named, as the synchronous step's error did.
func TestFeatureLoopReportsACombinedCheckFailure(t *testing.T) {
	ctrl := &fakeController{}
	td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
	td.useCheckBarriers()
	ctrl.status = scriptStatus(detailStep("running", "", false))
	ctrl.driveIntegration = func(context.Context, string, []string) (app.IntegrationReport, error) {
		return app.IntegrationReport{State: "checking", CheckDue: true}, nil
	}
	ctrl.driveIntegrationCheck = func(context.Context, string, []string) (app.IntegrationReport, error) {
		return app.IntegrationReport{}, errors.New("combined-check output retention failed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout bytes.Buffer
	_, err := runFeatureControllerLoop(ctx, td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
	if err == nil || !strings.Contains(err.Error(), "run the combined integration check: combined-check output retention failed") {
		t.Fatalf("err = %v, want the round's failure reported", err)
	}
}

// TestFeatureLoopDoesNotEndOnACombinedCheckStopRefusal pins the other leg
// of roundError's skip. A held stop can refuse one of the combined-check
// round's OWN dispatches, and the round then returns an
// ErrStopRequested-wrapping error. That refusal is the loop's own doing,
// exactly as a cancellation is, so it must not end the loop: the pass
// ends, the stop is driven, and the run finishes normally. Only the
// cancellation leg was exercised before, so deleting the
// ErrStopRequested arm of the skip left this package green.
func TestFeatureLoopDoesNotEndOnACombinedCheckStopRefusal(t *testing.T) {
	ctrl := &fakeController{}
	td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
	td.useCheckBarriers()

	var refused atomic.Bool
	ctrl.driveIntegration = func(context.Context, string, []string) (app.IntegrationReport, error) {
		return app.IntegrationReport{IntegrationID: "integration-1", State: "checking", CheckDue: true}, nil
	}
	ctrl.driveIntegrationCheck = func(context.Context, string, []string) (app.IntegrationReport, error) {
		td.checkStarted <- struct{}{}
		refused.Store(true)
		return app.IntegrationReport{}, fmt.Errorf("app: revalidate before the merge spawn: %w", app.ErrStopRequested)
	}
	ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
		if refused.Load() {
			return detailStep("stopping", "", true), nil
		}
		return detailStep("running", "", false), nil
	}
	ctrl.driveFeatureStop = func() (app.StopReport, error) {
		return app.StopReport{RunState: "stopped", Terminated: true}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout bytes.Buffer
	result, err := runFeatureControllerLoop(ctx, td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
	if err != nil {
		t.Fatalf("runFeatureControllerLoop: %v; a round's stop refusal is the loop's own doing and is never reported as a failure", err)
	}
	if result.FinalState != "stopped" {
		t.Fatalf("result = %+v, want the loop to have driven the stop to stopped", result)
	}
	if got := countCalls(ctrl.recorded(), "DriveFeatureStop"); got == 0 {
		t.Fatalf("the stop was never driven after the round's refusal; calls = %v", ctrl.recorded())
	}
}
