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

func TestRunStop(t *testing.T) {
	env := map[string]string{"HOME": "/home/u"}

	t.Run("requests, acquires the free lease and drives to stopped", func(t *testing.T) {
		ctrl := &fakeController{}
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
}
