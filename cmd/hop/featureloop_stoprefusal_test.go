package main

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// TestFeatureLoopStopRefusalEndsThePass proves a stop that refuses one of
// the integration step's own dispatches mid-pass ends that pass, not the
// loop: nothing later in the pass runs, and the next tick drives the stop.
func TestFeatureLoopStopRefusalEndsThePass(t *testing.T) {
	ctrl := &fakeController{}
	td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
	ctrl.driveIntegration = func(context.Context, string, []string) (app.IntegrationReport, error) {
		return app.IntegrationReport{}, fmt.Errorf("app: revalidate before merge spawn: %w", app.ErrStopRequested)
	}
	ctrl.status = scriptStatus(detailStep("running", "", false), detailStep("stopping", "", true))
	ctrl.driveFeatureStop = func() (app.StopReport, error) {
		return app.StopReport{RunState: "stopped", Terminated: true}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout bytes.Buffer
	result, err := runFeatureControllerLoop(ctx, td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
	if err != nil {
		t.Fatalf("runFeatureControllerLoop: %v, want the stop refusal to end the pass, not the loop", err)
	}
	if result.FinalState != "stopped" {
		t.Fatalf("result = %+v, want stopped", result)
	}
	calls := ctrl.recorded()
	after, ok := callsAfterFirst(calls, "DriveIntegration")
	if !ok {
		t.Fatalf("DriveIntegration never ran; calls = %v", calls)
	}
	for _, name := range []string{"EnsureReviewTask", "DriveCompletion", "AssignReadyTasks", "CorroborateSessionLaunches", "DriveFeatureChecks"} {
		if countCalls(after, name) != 0 {
			t.Errorf("%s ran after the stop refusal ended the pass; calls = %v", name, calls)
		}
	}
	if countCalls(after, "DriveFeatureStop") != 1 {
		t.Errorf("the next tick did not drive the stop; calls = %v", calls)
	}
}
