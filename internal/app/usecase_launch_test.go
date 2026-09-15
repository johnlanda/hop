package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// startedRun drives StartRun to completion and returns the handle plus the
// binding it produced, for tests that need to corroborate, stop or check
// against an already-launching attempt.
func startedRun(t *testing.T, tc *testController) (app.RunHandle, app.RunDetail) {
	t.Helper()
	_, handle, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
	if err != nil {
		t.Fatalf("StartRun() error = %v", err)
	}
	detail, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
	if err != nil {
		t.Fatalf("LoadRunStatus() error = %v", err)
	}
	if detail.Binding == nil {
		t.Fatalf("StartRun did not record a runtime binding")
	}
	return handle, detail
}

// claim writes a launch claim for the started run's incarnation, as hop
// launch would before exec.
func claimLaunch(t *testing.T, tc *testController, detail app.RunDetail, pid int) { //nolint:gocritic,unparam // hugeParam: detail is the test's already-loaded RunDetail, passed once per call in test setup, never a hot path. unparam: every current test scripts the same launcher pid, but the parameter documents that InspectPaneFn stubs must match whatever pid is claimed here.
	t.Helper()
	if err := tc.Store.ClaimLaunch(context.Background(), app.LaunchClaim{
		IncarnationID: detail.Binding.IncarnationID, RunID: detail.RunID, AttemptID: detail.AttemptID,
		Executable: "/usr/bin/claude", PID: pid, State: app.LaunchClaimExecPending,
	}); err != nil {
		t.Fatalf("ClaimLaunch() error = %v", err)
	}
}

func TestCorroborateLaunch(t *testing.T) {
	t.Run("pending: no claim written yet", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, _ := startedRun(t, tc)
		progress, err := tc.Controller.CorroborateLaunch(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateLaunch() error = %v", err)
		}
		if progress != app.LaunchPending {
			t.Fatalf("progress = %s, want %s", progress, app.LaunchPending)
		}
	})

	t.Run("settled: matching inspection moves Run/Attempt to running", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "/usr/bin/claude", Argv: []string{"claude", detail.AttemptID.String()}}}}, nil
		}

		progress, err := tc.Controller.CorroborateLaunch(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateLaunch() error = %v", err)
		}
		if progress != app.LaunchSettled {
			t.Fatalf("progress = %s, want %s", progress, app.LaunchSettled)
		}

		updated, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.State != run.RunRunning {
			t.Fatalf("Run.State = %s, want %s", updated.State, run.RunRunning)
		}
		if updated.AttemptState != run.AttemptRunning {
			t.Fatalf("Attempt.State = %s, want %s", updated.AttemptState, run.AttemptRunning)
		}
		if tc.Store.LaunchClaims[detail.Binding.IncarnationID].State != app.LaunchClaimExeced {
			t.Fatalf("launch claim was not settled to execed")
		}
	})

	t.Run("needs interaction: forking-wrapper topology never settles", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 9999, Argv0: "/usr/bin/claude", Argv: []string{"claude", detail.AttemptID.String()}}}}, nil
		}

		progress, err := tc.Controller.CorroborateLaunch(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateLaunch() error = %v", err)
		}
		if progress != app.LaunchNeedsInteraction {
			t.Fatalf("progress = %s, want %s", progress, app.LaunchNeedsInteraction)
		}
		if tc.Store.LaunchClaims[detail.Binding.IncarnationID].State != app.LaunchClaimExecPending {
			t.Fatalf("a forking-wrapper observation must never settle the claim")
		}
	})

	t.Run("failed: exec_failed claim fails Attempt, Task and Run", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		if err := tc.Store.SettleLaunchFailure(context.Background(), detail.Binding.IncarnationID, "exec: no such file"); err != nil {
			t.Fatalf("SettleLaunchFailure() error = %v", err)
		}

		progress, err := tc.Controller.CorroborateLaunch(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateLaunch() error = %v", err)
		}
		if progress != app.LaunchFailed {
			t.Fatalf("progress = %s, want %s", progress, app.LaunchFailed)
		}

		updated, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.State != run.RunFailed || updated.TaskState != run.TaskFailed || updated.AttemptState != run.AttemptFailed {
			t.Fatalf("Run/Task/Attempt = %s/%s/%s, want all failed", updated.State, updated.TaskState, updated.AttemptState)
		}
		session := tc.Store.Sessions[detail.SessionID]
		if session.value.State != run.SessionTerminated {
			t.Fatalf("Session.State = %s, want %s (reference trace 3 terminates the session)", session.value.State, run.SessionTerminated)
		}
	})

	t.Run("settled: the native session reference marker corroborates a resume-shaped argv", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		nativeRef := tc.Store.Sessions[detail.SessionID].value.NativeSessionRef
		if nativeRef == "" {
			t.Fatalf("no native session reference was pre-assigned for the claude harness")
		}
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "/usr/bin/claude", Argv: []string{"claude", "--resume", nativeRef}}}}, nil
		}

		progress, err := tc.Controller.CorroborateLaunch(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateLaunch() error = %v", err)
		}
		if progress != app.LaunchSettled {
			t.Fatalf("progress = %s, want %s (native reference is a durable marker)", progress, app.LaunchSettled)
		}
	})

	t.Run("pre-binding claim: a lost pane.open outcome is recovered by label, never dereferenced nil", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)

		// Simulate the pane.open outcome transaction having been lost: the
		// binding is gone and the operation is pending again, while the
		// launcher's claim (written against the intent) survives.
		tc.Store.Bindings = map[identity.SessionID][]run.RuntimeBinding{}
		var label string
		for id, op := range tc.Store.Operations {
			if op.Kind == app.OpPaneOpen {
				op.State = app.OperationPending
				op.ActEvidence = nil
				tc.Store.Operations[id] = op
				label = id.String()
			}
		}
		tc.Runtime.FindPaneByLabelFn = func(l string) (app.PaneRef, bool, error) {
			if l != label {
				t.Errorf("FindPaneByLabel(%q), want the pane.open operation label %q", l, label)
			}
			return app.PaneRef{WorkspaceID: "workspace-1", TabID: "tab-9", PaneID: "pane-9"}, true, nil
		}

		progress, err := tc.Controller.CorroborateLaunch(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateLaunch() error = %v", err)
		}
		if progress != app.LaunchPending {
			t.Fatalf("progress = %s, want %s (binding recovered; settlement is a later round)", progress, app.LaunchPending)
		}
		updated, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.Binding == nil || updated.Binding.PaneID != "pane-9" {
			t.Fatalf("binding was not recovered by label: %+v", updated.Binding)
		}
	})

	t.Run("early acceptance: corroboration activates a still-launching session and is otherwise a no-op", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		// A prior generation settled the claim; its lifecycle landed, but the
		// worker submitted first (early acceptance), so run/task/attempt come
		// from the acceptance transaction while the session never activated.
		claim := tc.Store.LaunchClaims[detail.Binding.IncarnationID]
		claim.State = app.LaunchClaimExeced
		tc.Store.LaunchClaims[detail.Binding.IncarnationID] = claim

		if result, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil || result.Kind != string(app.SubmissionAccepted) {
			t.Fatalf("SubmitResult() = %+v, err %v; want early acceptance", result, err)
		}
		if got := tc.Store.Sessions[detail.SessionID].value.State; got != run.SessionLaunching {
			t.Fatalf("Session.State = %s before corroboration, want %s", got, run.SessionLaunching)
		}

		progress, err := tc.Controller.CorroborateLaunch(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateLaunch() error = %v", err)
		}
		if progress != app.LaunchAlreadySettled {
			t.Fatalf("progress = %s, want %s", progress, app.LaunchAlreadySettled)
		}
		if got := tc.Store.Sessions[detail.SessionID].value.State; got != run.SessionActive {
			t.Fatalf("Session.State = %s after corroboration, want %s (early-accepted session activated)", got, run.SessionActive)
		}
	})
}

// TestLaunchClaimDeadline proves the section 6 mechanical launch bound: no
// claim within LaunchClaimDeadline of the journaled launch moves the
// operation to reconciling with a pane snapshot as evidence, and the pane
// is never re-created or re-sent.
func TestLaunchClaimDeadline(t *testing.T) {
	tc := newTestController(defaultPolicy())
	handle, detail := startedRun(t, tc)
	tc.Runtime.PaneContents[detail.Binding.PaneID] = "shell prompt with no launcher output"

	// Within the deadline the claim is simply pending.
	progress, err := tc.Controller.CorroborateLaunch(context.Background(), handle)
	if err != nil || progress != app.LaunchPending {
		t.Fatalf("CorroborateLaunch() = %s, err %v; want pending within the deadline", progress, err)
	}

	// The controller keeps heartbeating on the design's 10s interval while
	// it waits, so the lease stays live as the deadline passes.
	for elapsed := time.Duration(0); elapsed < 3*time.Minute; elapsed += 10 * time.Second {
		tc.Clock.Advance(10 * time.Second)
		if hbErr := tc.Controller.Heartbeat(context.Background(), handle); hbErr != nil {
			t.Fatalf("Heartbeat() error = %v", hbErr)
		}
	}

	progress, err = tc.Controller.CorroborateLaunch(context.Background(), handle)
	if err != nil {
		t.Fatalf("CorroborateLaunch() error = %v", err)
	}
	if progress != app.LaunchOverdue {
		t.Fatalf("progress = %s, want %s past the deadline", progress, app.LaunchOverdue)
	}
	var op app.Operation
	for id := range tc.Store.Operations {
		if tc.Store.Operations[id].Kind == app.OpPaneOpen {
			op = tc.Store.Operations[id]
		}
	}
	if op.State != app.OperationReconciling {
		t.Fatalf("pane.open operation state = %s, want reconciling with the pane snapshot as evidence", op.State)
	}
	evidence, isMap := op.ActEvidence.(map[string]any)
	if !isMap || evidence["pane_snapshot"] != "shell prompt with no launcher output" {
		t.Fatalf("operation evidence = %+v, want the captured pane snapshot", op.ActEvidence)
	}
}
