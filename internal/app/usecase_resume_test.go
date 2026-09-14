package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/run"
)

func defaultResumeRequest(runID string) app.ResumeRequest {
	return app.ResumeRequest{RunID: runID, ControllerID: "controller-2", HOPPath: "/usr/local/bin/hop", StateRoot: "/state"}
}

func TestResume(t *testing.T) {
	t.Run("warm reattach: a matching occupant returns the attempt to running", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "/usr/bin/claude", Argv: []string{"claude", detail.AttemptID.String()}}}}, nil
		}

		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeWarmReattached {
			t.Fatalf("Outcome = %s, want %s", result.Outcome, app.ResumeWarmReattached)
		}

		updated, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.AttemptState != run.AttemptRunning {
			t.Fatalf("Attempt.State = %s, want %s", updated.AttemptState, run.AttemptRunning)
		}
	})

	t.Run("fail closed: a present, non-matching occupant with no positive evidence", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 9999, Argv0: "/bin/bash", Argv: []string{"bash"}}}}, nil
		}

		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeFailedClosed {
			t.Fatalf("Outcome = %s, want %s", result.Outcome, app.ResumeFailedClosed)
		}
		if result.ObservedPaneID == "" {
			t.Fatalf("a failed-closed report must name the observed pane")
		}
	})

	t.Run("cold relaunch: absence plus --confirm-absent authorizes a new session and supersedes the old intent", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, nil // no live process: genuinely absent
		}

		// Simulate the previous incarnation's pane.open operation left
		// unresolved by a crash (its outcome transaction never committed);
		// the SQLite adapter's pre-binding claim fallback matches
		// ClaimLaunch against the newest such pending operation, so a
		// zombie launcher for the old incarnation must never find it.
		for id, op := range tc.Store.Operations {
			if op.Kind == app.OpPaneOpen {
				op.State = app.OperationPending
				tc.Store.Operations[id] = op
			}
		}

		req := defaultResumeRequest(detail.RunID.String())
		req.ConfirmAbsent = true
		tc.Clock.Advance(leaseTTL + time.Second)
		result, newHandle, err := tc.Controller.Resume(context.Background(), req)
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeColdRelaunched {
			t.Fatalf("Outcome = %s, want %s", result.Outcome, app.ResumeColdRelaunched)
		}
		if newHandle.RunID() != handle.RunID() {
			t.Fatalf("resume returned a handle for a different run")
		}

		updated, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.AttemptState != run.AttemptRelaunching && updated.AttemptState != run.AttemptRunning {
			t.Fatalf("Attempt.State = %s, want relaunching (or running once corroborated)", updated.AttemptState)
		}
		if updated.SessionID == detail.SessionID {
			t.Fatalf("cold relaunch did not bind a new session")
		}

		var reconcilingCount, otherCount int
		for _, op := range tc.Store.Operations {
			if op.Kind != app.OpPaneOpen {
				continue
			}
			if op.State == app.OperationReconciling {
				reconcilingCount++
			} else {
				otherCount++
			}
		}
		if reconcilingCount < 1 {
			t.Fatalf("expected the previous session's pane.open intent to be superseded (reconciling); found none among the operations")
		}
		if otherCount < 1 {
			t.Fatalf("expected a new pane.open operation for the relaunch; found none")
		}
	})

	t.Run("unsupported: cold resume is refused for a non-Claude harness", func(t *testing.T) {
		tc := newTestController(app.RunPolicy{CheckArgv: []string{"sh", "check.sh"}, Harness: "codex"})
		_, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, nil
		}

		req := defaultResumeRequest(detail.RunID.String())
		req.ConfirmAbsent = true
		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), req)
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeUnsupported {
			t.Fatalf("Outcome = %s, want %s", result.Outcome, app.ResumeUnsupported)
		}
	})

	t.Run("reserved attempt: nothing to reconcile", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, _, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if err != nil {
			t.Fatalf("StartRun() error = %v", err)
		}
		runID := tc.onlyRunID(t)
		forceAttemptReserved(t, tc, runID)

		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(runID.String()))
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeNothingToDo {
			t.Fatalf("Outcome = %s, want %s", result.Outcome, app.ResumeNothingToDo)
		}
	})
}
