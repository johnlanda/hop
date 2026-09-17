package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

func TestRunStop(t *testing.T) {
	env := map[string]string{"HOME": "/home/u"}

	t.Run("requests, acquires the free lease and drives to stopped", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return detailStep("running", "running", false), nil
		}
		requested := ""
		ctrl.requestStop = func(runID string) error { requested = runID; return nil }
		ctrl.resume = func(app.ResumeRequest) (app.ResumeResult, app.RunHandle, error) {
			return app.ResumeResult{Outcome: app.ResumeStopPending}, app.RunHandle{}, nil
		}
		rounds := 0
		ctrl.driveStop = func() (app.StopReport, error) {
			rounds++
			if rounds < 2 {
				return app.StopReport{RunState: "stopping"}, nil
			}
			return app.StopReport{RunState: "stopped", Terminated: true}, nil
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runStop([]string{testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitOK {
			t.Errorf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
		}
		if requested != testRunID {
			t.Errorf("stop requested for %q", requested)
		}
		out := stdout.String()
		if !strings.Contains(out, "stopping\n") || !strings.Contains(out, "stopped\n") {
			t.Errorf("output = %q, want stopping then stopped", out)
		}
	})

	t.Run("a held lease reports and observes the live controller's stop", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.requestStop = func(string) error { return nil }
		ctrl.resume = func(app.ResumeRequest) (app.ResumeResult, app.RunHandle, error) {
			return app.ResumeResult{}, app.RunHandle{}, app.ErrLeaseHeld
		}
		statusCalls := 0
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			statusCalls++
			if statusCalls < 2 {
				return detailStep("stopping", "running", true), nil
			}
			return detailStep("stopped", "interrupted", true), nil
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runStop([]string{testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitOK {
			t.Errorf("exit code = %d, want %d", code, exitOK)
		}
		if !strings.Contains(stdout.String(), "a live controller holds the lease") {
			t.Errorf("output = %q", stdout.String())
		}
		for _, call := range ctrl.recorded() {
			if call == "DriveStop" {
				t.Error("DriveStop called without the lease")
			}
		}
	})

	t.Run("a deadline that leaves the run stopping exits 1 and says it is rerunnable", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return detailStep("running", "running", false), nil
		}
		ctrl.requestStop = func(string) error { return nil }
		ctrl.resume = func(app.ResumeRequest) (app.ResumeResult, app.RunHandle, error) {
			return app.ResumeResult{Outcome: app.ResumeStopPending}, app.RunHandle{}, nil
		}
		ctrl.driveStop = func() (app.StopReport, error) {
			return app.StopReport{RunState: "stopping", Outstanding: []string{"check group 41337 signaled, absence not yet observed"}}, nil
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		// The fake wait reports the deadline after three stop rounds,
		// standing in for the command's expired context; heartbeat waits
		// keep blocking on the context (the heartbeat goroutine shares
		// this seam), and the counter is atomic for the same reason.
		var polls atomic.Int32
		td.deps.wait = func(ctx context.Context, d time.Duration) error {
			if d == heartbeatInterval {
				<-ctx.Done()
				return ctx.Err()
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if polls.Add(1) >= 3 {
				return context.DeadlineExceeded
			}
			return nil
		}
		var stdout, stderr bytes.Buffer

		code, err := runStop([]string{testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		if !strings.Contains(stdout.String(), "rerun hop stop") || !strings.Contains(stdout.String(), "check group 41337") {
			t.Errorf("output = %q", stdout.String())
		}
	})

	t.Run("usage requires exactly one run id", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, env, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runStop(nil, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitUsage || !strings.Contains(stderr.String(), "exactly one run-id") {
			t.Errorf("code = %d, stderr = %q", code, stderr.String())
		}
	})

	t.Run("a feature-mode run drives through ResumeFeature/DriveFeatureStop, never the solo pair", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return app.StatusResult{Detail: &app.RunDetailView{
				RunSummaryView: app.RunSummaryView{RunID: testRunID, Sequence: 1, State: "running"},
				Mode:           "feature",
			}}, nil
		}
		ctrl.requestStop = func(string) error { return nil }
		ctrl.resumeFeature = func(app.ResumeFeatureRequest) (app.ResumeFeatureResult, app.RunHandle, error) {
			return app.ResumeFeatureResult{Outcome: "stop-pending"}, app.RunHandle{}, nil
		}
		rounds := 0
		ctrl.driveFeatureStop = func() (app.StopReport, error) {
			rounds++
			if rounds < 2 {
				return app.StopReport{RunState: "stopping"}, nil
			}
			return app.StopReport{RunState: "stopped", Terminated: true}, nil
		}
		ctrl.resume = func(app.ResumeRequest) (app.ResumeResult, app.RunHandle, error) {
			t.Fatal("the solo Resume was called for a feature-mode run")
			return app.ResumeResult{}, app.RunHandle{}, nil
		}
		ctrl.driveStop = func() (app.StopReport, error) {
			t.Fatal("the solo DriveStop was called for a feature-mode run")
			return app.StopReport{}, nil
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runStop([]string{testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK {
			t.Errorf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
		}
		if !strings.Contains(stdout.String(), "stopped\n") {
			t.Errorf("output = %q, want it to reach stopped", stdout.String())
		}
	})

	t.Run("a feature-mode run on a controller missing the feature ports fails closed, never falls back to solo", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return app.StatusResult{Detail: &app.RunDetailView{
				RunSummaryView: app.RunSummaryView{RunID: testRunID, Sequence: 1, State: "running"},
				Mode:           "feature",
			}}, nil
		}
		ctrl.requestStop = func(string) error { return nil }
		ctrl.resumeFeature = func(app.ResumeFeatureRequest) (app.ResumeFeatureResult, app.RunHandle, error) {
			return app.ResumeFeatureResult{}, app.RunHandle{}, fmt.Errorf("app: %w", app.ErrFeatureModeUnsupported)
		}
		ctrl.resume = func(app.ResumeRequest) (app.ResumeResult, app.RunHandle, error) {
			t.Fatal("the solo Resume was called as a fallback for an unsupported feature-mode run")
			return app.ResumeResult{}, app.RunHandle{}, nil
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runStop([]string{testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		if !strings.Contains(stderr.String(), app.ErrFeatureModeUnsupported.Error()) {
			t.Errorf("stderr = %q, want the fail-closed sentinel surfaced", stderr.String())
		}
	})
}

func TestRunResume(t *testing.T) {
	env := map[string]string{"HOME": "/home/u"}

	newResumeController := func(outcome app.ResumeOutcome, detail string) *fakeController {
		ctrl := &fakeController{}
		ctrl.resume = func(app.ResumeRequest) (app.ResumeResult, app.RunHandle, error) {
			return app.ResumeResult{Outcome: outcome, Detail: detail}, app.RunHandle{}, nil
		}
		return ctrl
	}

	t.Run("warm reattach continues as the foreground controller to completion", func(t *testing.T) {
		ctrl := newResumeController(app.ResumeWarmReattached, "occupant matched the settled claim")
		var resumed app.ResumeRequest
		ctrl.resume = func(req app.ResumeRequest) (app.ResumeResult, app.RunHandle, error) {
			resumed = req
			return app.ResumeResult{Outcome: app.ResumeWarmReattached, Detail: "occupant matched the settled claim"}, app.RunHandle{}, nil
		}
		ctrl.status = scriptStatus(
			detailStep("running", "running", false),
			detailStep("completed", "completed", false),
		)
		td := newTestDeps(ctrl, env, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runResume([]string{"--confirm-absent", testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitOK {
			t.Errorf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
		}
		if !resumed.ConfirmAbsent {
			t.Error("--confirm-absent was not passed through")
		}
		if resumed.RunID != testRunID || resumed.HOPPath != "/opt/hop/bin/hop" || resumed.StateRoot != "/home/u/.local/state/hop" {
			t.Errorf("resume request = %+v", resumed)
		}
		if !strings.Contains(stdout.String(), "resume warm-reattached: occupant matched the settled claim\n") {
			t.Errorf("output = %q", stdout.String())
		}
	})

	t.Run("fail-closed reports the pane and the human action, releases and exits 1", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return detailStep("running", "running", false), nil
		}
		ctrl.resume = func(app.ResumeRequest) (app.ResumeResult, app.RunHandle, error) {
			return app.ResumeResult{
				Outcome:        app.ResumeFailedClosed,
				ObservedPaneID: "pane-9",
				Detail:         "occupant does not match; inspect the pane, close it or hop stop the run, then rerun hop resume or attest absence",
			}, app.RunHandle{}, nil
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runResume([]string{testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		out := stdout.String()
		if !strings.Contains(out, "failed-closed") || !strings.Contains(out, "(pane pane-9)") || !strings.Contains(out, "rerun hop resume") {
			t.Errorf("output = %q", out)
		}
		detached := false
		for _, call := range ctrl.recorded() {
			if call == "Detach" {
				detached = true
			}
		}
		if !detached {
			t.Error("the lease was not released on the fail-closed exit")
		}
	})

	t.Run("unsupported cold resume exits 1 with the report", func(t *testing.T) {
		ctrl := newResumeController(app.ResumeUnsupported, "codex cold resume is out of Phase 2 scope")
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return detailStep("running", "running", false), nil
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runResume([]string{testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitFailure || !strings.Contains(stdout.String(), "unsupported") {
			t.Errorf("code = %d, output = %q", code, stdout.String())
		}
	})

	t.Run("nothing-to-do exits by the run's terminal state", func(t *testing.T) {
		ctrl := newResumeController(app.ResumeNothingToDo, "run is already completed")
		ctrl.status = scriptStatus(detailStep("completed", "completed", false))
		td := newTestDeps(ctrl, env, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runResume([]string{testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitOK {
			t.Errorf("exit code = %d, want %d for a completed run", code, exitOK)
		}
	})

	t.Run("a resume error releases and exits 1", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return detailStep("running", "running", false), nil
		}
		ctrl.resume = func(app.ResumeRequest) (app.ResumeResult, app.RunHandle, error) {
			return app.ResumeResult{}, app.RunHandle{}, errors.New("app: acquire lease: app: lease is held")
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runResume([]string{testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitFailure || !strings.Contains(stderr.String(), "lease is held") {
			t.Errorf("code = %d, stderr = %q", code, stderr.String())
		}
	})

	t.Run("usage requires exactly one run id", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, env, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runResume(nil, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitUsage || !strings.Contains(stderr.String(), "exactly one run-id") {
			t.Errorf("code = %d, stderr = %q", code, stderr.String())
		}
	})

	t.Run("a non-boolean --confirm-absent value on a solo run is a usage error", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return detailStep("running", "running", false), nil
		}
		ctrl.resume = func(app.ResumeRequest) (app.ResumeResult, app.RunHandle, error) {
			t.Fatal("Resume was called despite the malformed flag value")
			return app.ResumeResult{}, app.RunHandle{}, nil
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runResume([]string{"--confirm-absent=some-session-id", testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
		if !strings.Contains(stderr.String(), "--confirm-absent takes no value on a solo run") {
			t.Errorf("stderr = %q", stderr.String())
		}
	})

	t.Run("--confirm-absent=false on a solo run is accepted and passed through as false", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return detailStep("running", "running", false), nil
		}
		var resumed app.ResumeRequest
		ctrl.resume = func(req app.ResumeRequest) (app.ResumeResult, app.RunHandle, error) {
			resumed = req
			return app.ResumeResult{Outcome: app.ResumeReconciling, Detail: "still reconciling"}, app.RunHandle{}, nil
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runResume([]string{"--confirm-absent=false", testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		if resumed.ConfirmAbsent {
			t.Error("ConfirmAbsent = true, want false")
		}
	})

	t.Run("a feature-mode run requires --confirm-absent=<session-id>; bare or empty is usage", func(t *testing.T) {
		for _, args := range [][]string{
			{"--confirm-absent", testRunID},
			{"--confirm-absent=", testRunID},
		} {
			ctrl := &fakeController{}
			ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
				return app.StatusResult{Detail: &app.RunDetailView{
					RunSummaryView: app.RunSummaryView{RunID: testRunID, Sequence: 1, State: "running"},
					Mode:           "feature",
				}}, nil
			}
			ctrl.resumeFeature = func(app.ResumeFeatureRequest) (app.ResumeFeatureResult, app.RunHandle, error) {
				t.Fatal("ResumeFeature was called despite the malformed --confirm-absent")
				return app.ResumeFeatureResult{}, app.RunHandle{}, nil
			}
			td := newTestDeps(ctrl, env, t.TempDir())
			var stdout, stderr bytes.Buffer

			code, err := runResume(args, &stdout, &stderr, td.deps)
			if err != nil {
				t.Fatalf("write error: %v", err)
			}
			if code != exitUsage {
				t.Errorf("args = %v: exit code = %d, want %d", args, code, exitUsage)
			}
			if !strings.Contains(stderr.String(), "requires a session id in feature mode") {
				t.Errorf("args = %v: stderr = %q", args, stderr.String())
			}
		}
	})

	t.Run("a feature-mode run dispatches through ResumeFeature, passing the session id verbatim", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return app.StatusResult{Detail: &app.RunDetailView{
				RunSummaryView: app.RunSummaryView{RunID: testRunID, Sequence: 1, State: "running"},
				Mode:           "feature",
			}}, nil
		}
		var resumed app.ResumeFeatureRequest
		ctrl.resumeFeature = func(req app.ResumeFeatureRequest) (app.ResumeFeatureResult, app.RunHandle, error) {
			resumed = req
			return app.ResumeFeatureResult{
				Outcome:  "reconciling",
				RunState: "resuming",
				Sessions: []app.FeatureSessionReport{
					{SessionID: "worker-session-1", Role: "implementer", Disposition: "reconciling", Detail: "pane absent; cold relaunch requires --confirm-absent worker-session-1"},
				},
			}, app.RunHandle{}, nil
		}
		ctrl.resume = func(app.ResumeRequest) (app.ResumeResult, app.RunHandle, error) {
			t.Fatal("the solo Resume was called for a feature-mode run")
			return app.ResumeResult{}, app.RunHandle{}, nil
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runResume([]string{"--confirm-absent=other-session-2", testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure {
			t.Errorf("exit code = %d, want %d (a value matching no absent session stays reconciling)", code, exitFailure)
		}
		if resumed.ConfirmAbsentSession != "other-session-2" {
			t.Errorf("ConfirmAbsentSession = %q, want the value passed through verbatim", resumed.ConfirmAbsentSession)
		}
		out := stdout.String()
		if !strings.Contains(out, "resume reconciling: resuming") {
			t.Errorf("output = %q", out)
		}
		if !strings.Contains(out, "worker-session-1") || !strings.Contains(out, "--confirm-absent worker-session-1") {
			t.Errorf("output does not name the reconciling session's required id: %q", out)
		}
	})

	t.Run("a resumed feature run with a launch-suppressed child reaches failed through the loop, never a stop", func(t *testing.T) {
		ctrl := &fakeController{runState: runStateRunning}
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return app.StatusResult{Detail: &app.RunDetailView{
				RunSummaryView: app.RunSummaryView{RunID: testRunID, Sequence: 1, State: ctrl.currentRunState()},
				Mode:           "feature",
			}}, nil
		}
		ctrl.resumeFeature = func(app.ResumeFeatureRequest) (app.ResumeFeatureResult, app.RunHandle, error) {
			return app.ResumeFeatureResult{
				Outcome:  "resumed",
				RunState: "running",
				Sessions: []app.FeatureSessionReport{
					{SessionID: "manager-session", Role: "manager", Disposition: app.SessionWarm},
					{SessionID: "worker-session-2", Role: "implementer", Disposition: app.SessionLaunchSuppressed, Detail: "its launch never reached a pane and the run carries a terminal-failure cause; the controller loop's failure cleanup retires it"},
				},
			}, app.RunHandle{}, nil
		}
		ctrl.requestStop = func(string) error {
			t.Fatal("a stop was requested for a run the failure cleanup settles")
			return nil
		}
		rounds := 0
		ctrl.retireSettledSessions = func() (app.RetirementReport, error) {
			rounds++
			if rounds < 2 {
				return app.RetirementReport{RunFailing: true, Outstanding: []string{"session manager-session: close dispatched"}}, nil
			}
			ctrl.setRunState("failed")
			return app.RetirementReport{RunFailed: true, Retired: []string{"worker-session-2", "manager-session"}}, nil
		}
		ctrl.assignReadyTasks = func(app.AssignmentOptions) (app.AssignmentReport, error) {
			t.Fatal("AssignReadyTasks ran in a failing run")
			return app.AssignmentReport{}, nil
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runResume([]string{testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure || rounds != 2 {
			t.Errorf("exit code = %d after %d retirement rounds, want %d after 2 (stderr: %s)", code, rounds, exitFailure, stderr.String())
		}
		out := stdout.String()
		for _, want := range []string{
			"resume resumed: running",
			"session worker-session-2 (implementer): launch-suppressed",
			"run r1 running",
			"run r1 failed",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("a resumed feature run with a child launch in flight enters the loop, whose corroboration settles it", func(t *testing.T) {
		ctrl := &fakeController{runState: runStateRunning}
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return app.StatusResult{Detail: &app.RunDetailView{
				RunSummaryView: app.RunSummaryView{RunID: testRunID, Sequence: 1, State: ctrl.currentRunState()},
				Mode:           "feature",
			}}, nil
		}
		const inFlight = "launch claim not settled; the placed launch is in flight, and the controller loop corroborates it"
		ctrl.resumeFeature = func(app.ResumeFeatureRequest) (app.ResumeFeatureResult, app.RunHandle, error) {
			return app.ResumeFeatureResult{
				Outcome:  "resumed",
				RunState: "running",
				Sessions: []app.FeatureSessionReport{
					{SessionID: "manager-session", Role: "manager", Disposition: app.SessionWarm},
					{SessionID: "worker-session-3", Role: "implementer", Disposition: app.SessionPending, Detail: inFlight},
				},
			}, app.RunHandle{}, nil
		}
		ctrl.requestStop = func(string) error {
			t.Fatal("a stop was requested for a run whose launch the loop corroborates")
			return nil
		}
		corroborations := 0
		ctrl.corroborateSessions = func() ([]app.SessionLaunchProgress, error) {
			corroborations++
			// The loop's corroboration settles the in-flight launch; the run
			// then completes on its own.
			ctrl.setRunState("completed")
			return []app.SessionLaunchProgress{{SessionID: "worker-session-3", Role: "implementer", Progress: app.LaunchSettled}}, nil
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runResume([]string{testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK || corroborations == 0 {
			t.Errorf("exit code = %d after %d corroboration rounds, want %d after at least one (stderr: %s)", code, corroborations, exitOK, stderr.String())
		}
		out := stdout.String()
		for _, want := range []string{
			"resume resumed: running",
			"session worker-session-3 (implementer): pending - " + inFlight,
			"run r1 running",
			"run r1 completed",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("a feature-mode run with no --confirm-absent omits the attestation and can still resume", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return app.StatusResult{Detail: &app.RunDetailView{
				RunSummaryView: app.RunSummaryView{RunID: testRunID, Sequence: 1, State: "completed"},
				Mode:           "feature",
			}}, nil
		}
		var resumed app.ResumeFeatureRequest
		ctrl.resumeFeature = func(req app.ResumeFeatureRequest) (app.ResumeFeatureResult, app.RunHandle, error) {
			resumed = req
			return app.ResumeFeatureResult{Outcome: "nothing-to-do", RunState: "completed"}, app.RunHandle{}, nil
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runResume([]string{testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK {
			t.Errorf("exit code = %d, want %d", code, exitOK)
		}
		if resumed.ConfirmAbsentSession != "" {
			t.Errorf("ConfirmAbsentSession = %q, want empty when the flag was never given", resumed.ConfirmAbsentSession)
		}
	})

	t.Run("resuming a feature run by UUID from another directory assigns in the frozen repository", func(t *testing.T) {
		otherDir := t.TempDir()
		for _, args := range [][]string{
			{testRunID},
			{"-C", otherDir, testRunID},
		} {
			ctrl := &fakeController{}
			statusCalls := 0
			ctrl.status = func(req app.StatusRequest) (app.StatusResult, error) {
				if req.RunID != testRunID {
					t.Errorf("status request = %+v, want the UUID looked up directly", req)
				}
				statusCalls++
				state := "running"
				if statusCalls > 3 { // resume's mode load, the label, the first tick
					state = "completed"
				}
				return app.StatusResult{Detail: &app.RunDetailView{
					RunSummaryView: app.RunSummaryView{RunID: testRunID, Sequence: 1, State: state},
					Mode:           "feature",
				}}, nil
			}
			ctrl.resumeFeature = func(app.ResumeFeatureRequest) (app.ResumeFeatureResult, app.RunHandle, error) {
				return app.ResumeFeatureResult{Outcome: "resumed", RunState: "running"}, app.RunHandle{}, nil
			}
			var assigned []app.AssignmentOptions
			ctrl.assignReadyTasks = func(opts app.AssignmentOptions) (app.AssignmentReport, error) {
				assigned = append(assigned, opts)
				return app.AssignmentReport{}, nil
			}
			td := newTestDeps(ctrl, env, otherDir)
			var stdout, stderr bytes.Buffer

			code, err := runResume(args, &stdout, &stderr, td.deps)
			if err != nil {
				t.Fatalf("write error: %v", err)
			}
			if code != exitOK {
				t.Fatalf("args = %v: exit code = %d, want %d (stderr: %s)", args, code, exitOK, stderr.String())
			}
			if len(assigned) != 1 {
				t.Fatalf("args = %v: AssignReadyTasks calls = %+v, want exactly one", args, assigned)
			}
			if assigned[0].RepositoryRoot != fakeFrozenRepositoryRoot || assigned[0].StateRoot != fakeFrozenStateRoot {
				t.Errorf("args = %v: assignment roots = %q/%q, want the frozen %q/%q (never the caller's %q)",
					args, assigned[0].RepositoryRoot, assigned[0].StateRoot, fakeFrozenRepositoryRoot, fakeFrozenStateRoot, otherDir)
			}
		}
	})

	t.Run("a feature-mode run on a controller missing the feature ports fails closed, never falls back to solo", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return app.StatusResult{Detail: &app.RunDetailView{
				RunSummaryView: app.RunSummaryView{RunID: testRunID, Sequence: 1, State: "running"},
				Mode:           "feature",
			}}, nil
		}
		ctrl.resumeFeature = func(app.ResumeFeatureRequest) (app.ResumeFeatureResult, app.RunHandle, error) {
			return app.ResumeFeatureResult{}, app.RunHandle{}, fmt.Errorf("app: %w", app.ErrFeatureModeUnsupported)
		}
		ctrl.resume = func(app.ResumeRequest) (app.ResumeResult, app.RunHandle, error) {
			t.Fatal("the solo Resume was called as a fallback for an unsupported feature-mode run")
			return app.ResumeResult{}, app.RunHandle{}, nil
		}
		td := newTestDeps(ctrl, env, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runResume([]string{testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		if !strings.Contains(stderr.String(), app.ErrFeatureModeUnsupported.Error()) {
			t.Errorf("stderr = %q, want the fail-closed sentinel surfaced", stderr.String())
		}
	})
}
