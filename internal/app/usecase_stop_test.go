package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

func TestDriveStop(t *testing.T) {
	t.Run("reserved: an attempt never launched is interrupted directly, run stops immediately", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		// A run whose worktree.create/pane.open never ran: force the
		// attempt back to reserved to simulate a stop requested before
		// StartRun's launch intent committed.
		_, handle, startErr := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if startErr != nil {
			t.Fatalf("StartRun() error = %v", startErr)
		}
		runID := tc.onlyRunID(t)
		forceAttemptReserved(t, tc, runID)

		if stopErr := tc.Controller.RequestStop(context.Background(), runID.String()); stopErr != nil {
			t.Fatalf("RequestStop() error = %v", stopErr)
		}
		report, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if !report.Terminated || report.RunState != string(run.RunStopped) {
			t.Fatalf("report = %+v, want terminated/stopped", report)
		}

		detail, err := tc.Store.LoadRunStatus(context.Background(), runID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if detail.AttemptState != run.AttemptInterrupted || detail.TaskState != run.TaskInterrupted {
			t.Fatalf("Attempt/Task = %s/%s, want both interrupted", detail.AttemptState, detail.TaskState)
		}
	})

	t.Run("launching: a pre-exec claim is retired under the pane.close operation, termination observed", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "hop", Name: "hop", Argv: []string{"/usr/bin/hop", "launch", "--attempt", detail.AttemptID.String()}}}}, nil
		}

		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
		report, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if report.Terminated {
			t.Fatalf("report = %+v; a dispatched close is never itself termination", report)
		}
		if len(tc.Runtime.ClosedPanes) != 1 {
			t.Fatalf("ClosePane was called %d times, want 1", len(tc.Runtime.ClosedPanes))
		}
		var closeOp *app.Operation
		for id := range tc.Store.Operations {
			op := tc.Store.Operations[id]
			if op.Kind == app.OpPaneClose {
				closeOp = &op
			}
		}
		if closeOp == nil {
			t.Fatalf("no pane.close operation was journaled for the retirement")
		}
		if closeOp.State != app.OperationPending {
			t.Fatalf("pane.close operation state = %s, want pending until absence is observed", closeOp.State)
		}

		// The launcher is observed gone on a later round: the pending
		// pane.close operation resolves and the stop completes.
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		final, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("second DriveStop() error = %v", err)
		}
		if !final.Terminated || final.RunState != string(run.RunStopped) {
			t.Fatalf("final report = %+v, want terminated/stopped", final)
		}
		if len(tc.Runtime.ClosedPanes) != 1 {
			t.Fatalf("ClosePane was re-sent on the recovery round: %v", tc.Runtime.ClosedPanes)
		}
		for id := range tc.Store.Operations {
			op := tc.Store.Operations[id]
			if op.Kind == app.OpPaneClose && op.State != app.OperationSucceeded {
				t.Fatalf("pane.close operation state = %s after observed absence, want succeeded", op.State)
			}
		}
	})

	t.Run("launching: mismatched occupant fails closed, never closes", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 9999, Argv0: "bash", Name: "bash", Argv: []string{"/bin/bash"}}}}, nil
		}

		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
		report, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if report.Terminated {
			t.Fatalf("report = %+v, must not terminate on a mismatched occupant", report)
		}
		if len(tc.Runtime.ClosedPanes) != 0 {
			t.Fatalf("ClosePane was called despite a mismatched occupant: %v", tc.Runtime.ClosedPanes)
		}
	})

	t.Run("running: the worker pane is closed under the close rule", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)

		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
		// The worker's own MCP-server children share its process group and
		// list before it: the close-target match must find the recorded pid
		// among the members, not require it at index 0.
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return mcpGroupPane(4242, "/usr/bin/claude", detail.AttemptID.String()), nil
		}
		report, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if report.Terminated {
			t.Fatalf("closing does not itself prove termination; report = %+v", report)
		}
		if len(tc.Runtime.ClosedPanes) != 1 {
			t.Fatalf("ClosePane was called %d times, want 1", len(tc.Runtime.ClosedPanes))
		}

		// A later round observes the pane gone and finalizes.
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		final, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if !final.Terminated || final.RunState != string(run.RunStopped) {
			t.Fatalf("final report = %+v, want terminated/stopped", final)
		}
	})

	t.Run("running: a foreground group of foreign members only never matches the close target", func(t *testing.T) {
		// The recorded pid appears on no member, and a marker carried by a
		// foreign member (a same-group process quoting the attempt id in
		// its own argv) is never combined with another member's pid: the
		// close-target match is per member, so nothing is closed.
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)

		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{
				{PID: 9999, Argv0: "bash", Name: "bash", Argv: []string{"/bin/bash"}},
				{PID: 9998, Argv0: "npm", Name: "npm", Argv: []string{"npm", "exec", detail.AttemptID.String()}},
			}}, nil
		}
		report, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if report.Terminated {
			t.Fatalf("report = %+v, must not terminate on a foreign-members-only group", report)
		}
		if len(tc.Runtime.ClosedPanes) != 0 {
			t.Fatalf("ClosePane was called despite no member matching the close target: %v", tc.Runtime.ClosedPanes)
		}
	})

	t.Run("checking: a check group and the worker are both retired, absence observed before stopped", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		forceAttemptChecking(t, tc, detail)

		opID := seedPendingCheckOperation(t, tc, detail.RunID, []string{"sh", "check.sh"})
		if err := tc.Store.ClaimCheckExec(context.Background(), opID, 5150); err != nil {
			t.Fatalf("ClaimCheckExec() error = %v", err)
		}
		tc.Groups.Processes[5150] = []app.GroupProcess{{PID: 5150, Argv: []string{"sh", "check.sh"}}}

		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
		report, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if report.Terminated {
			t.Fatalf("report = %+v; signaled group and dispatched close are not yet observed termination", report)
		}
		if len(tc.Groups.Signaled) != 1 || tc.Groups.Signaled[0] != 5150 {
			t.Fatalf("Signaled = %v, want [5150]", tc.Groups.Signaled)
		}

		// A later round observes both the group and the worker gone.
		tc.Groups.Processes[5150] = nil
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		final, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("second DriveStop() error = %v", err)
		}
		if !final.Terminated || final.RunState != string(run.RunStopped) {
			t.Fatalf("final report = %+v, want terminated/stopped", final)
		}
	})

	t.Run("checking: a mismatched check group is never signaled and the run stays stopping", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		forceAttemptChecking(t, tc, detail)
		opID := seedPendingCheckOperation(t, tc, detail.RunID, []string{"sh", "check.sh"})
		if err := tc.Store.ClaimCheckExec(context.Background(), opID, 5150); err != nil {
			t.Fatalf("ClaimCheckExec() error = %v", err)
		}
		tc.Groups.Processes[5150] = []app.GroupProcess{{PID: 6000, Argv: []string{"unrelated"}}}
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}

		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
		report, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if report.Terminated {
			t.Fatalf("report = %+v; a mismatched group must never count as absence", report)
		}
		if len(tc.Groups.Signaled) != 0 {
			t.Fatalf("Signaled = %v; a mismatched group is never signaled", tc.Groups.Signaled)
		}
		if tc.Store.Operations[opID].State != app.OperationReconciling {
			t.Fatalf("check operation state = %s, want reconciling with the listing as evidence", tc.Store.Operations[opID].State)
		}
	})

	t.Run("idempotent: driving stop again after the run already stopped is a no-op", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, handle, startErr := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if startErr != nil {
			t.Fatalf("StartRun() error = %v", startErr)
		}
		runID := tc.onlyRunID(t)
		forceAttemptReserved(t, tc, runID)
		if stopErr := tc.Controller.RequestStop(context.Background(), runID.String()); stopErr != nil {
			t.Fatalf("RequestStop() error = %v", stopErr)
		}
		if _, driveErr := tc.Controller.DriveStop(context.Background(), handle); driveErr != nil {
			t.Fatalf("first DriveStop() error = %v", driveErr)
		}
		report, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("second DriveStop() error = %v", err)
		}
		if !report.Terminated {
			t.Fatalf("repeated DriveStop() must stay terminated: %+v", report)
		}
	})
}

// TestPaneCloseInterruption covers the pane.close decision-table row: a
// close intent committed by a controller that crashed before (or after)
// the act is recovered by a later round — an already-gone target is
// adopted, a still-matching target is re-acted against exactly once, and a
// replaced occupant stays reconciling.
func TestPaneCloseInterruption(t *testing.T) {
	seedPendingClose := func(t *testing.T, tc *testController, detail app.RunDetail, pid int) identity.OperationID {
		t.Helper()
		opID, err := identity.ParseOperationID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse operation id: %v", err)
		}
		tc.Store.Operations[opID] = app.Operation{
			ID: opID, RunID: detail.RunID, Generation: tc.Store.Leases[detail.RunID].lease.Generation,
			Kind: app.OpPaneClose, State: app.OperationPending,
			Intent: map[string]any{
				"pane_id": detail.Binding.PaneID, "label": detail.Binding.CreationLabel,
				"close_session_id": detail.SessionID.String(), "close_incarnation_id": detail.Binding.IncarnationID.String(),
				"pid": pid, "argv_markers": []any{detail.AttemptID.String()}, "reason": "stop",
			},
			CreatedAt: tc.Clock.Now(), UpdatedAt: tc.Clock.Now(),
		}
		return opID
	}

	t.Run("crash between intent and act: recovery re-acts once against the same matched target", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		opID := seedPendingClose(t, tc, detail, 4242)

		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
		report, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if report.Terminated {
			t.Fatalf("report = %+v; dispatch is not termination", report)
		}
		if len(tc.Runtime.ClosedPanes) != 1 {
			t.Fatalf("ClosePane calls = %d, want exactly 1 re-act against the recovered intent", len(tc.Runtime.ClosedPanes))
		}
		var closeOps int
		for id := range tc.Store.Operations {
			if tc.Store.Operations[id].Kind == app.OpPaneClose {
				closeOps++
			}
		}
		if closeOps != 1 {
			t.Fatalf("pane.close operations = %d, want 1 (the recovered intent, never a second)", closeOps)
		}
		_ = opID
	})

	t.Run("crash between act and outcome: an already-gone target is adopted, close never re-sent", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		seedPendingClose(t, tc, detail, 4242)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound // the crashed controller's close already landed
		}

		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
		report, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if !report.Terminated {
			t.Fatalf("report = %+v, want terminated: the target was observed absent", report)
		}
		if len(tc.Runtime.ClosedPanes) != 0 {
			t.Fatalf("ClosePane was re-sent against an absent target: %v", tc.Runtime.ClosedPanes)
		}
	})

	t.Run("replaced occupant: recovery stays reconciling and never closes", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		opID := seedPendingClose(t, tc, detail, 4242)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 9999, Argv0: "bash", Name: "bash", Argv: []string{"/bin/bash"}}}}, nil
		}

		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
		report, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if report.Terminated {
			t.Fatalf("report = %+v; a replaced occupant is never absence", report)
		}
		if len(tc.Runtime.ClosedPanes) != 0 {
			t.Fatalf("ClosePane was sent against a mismatched occupant: %v", tc.Runtime.ClosedPanes)
		}
		if got := tc.Store.Operations[opID].State; got != app.OperationReconciling {
			t.Fatalf("pane.close operation state = %s, want reconciling", got)
		}
	})
}

// TestStopAbsenceRule proves the pane.close procedure never counts an
// empty-foreground pane that still answers for its creation label — or an
// inspection error — as observed termination.
func TestStopAbsenceRule(t *testing.T) {
	tc := newTestController(defaultPolicy())
	handle, detail := runningRun(t, tc)
	if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
		t.Fatalf("RequestStop() error = %v", err)
	}
	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{}, nil // the pane still answers by id, foreground empty
	}

	report, err := tc.Controller.DriveStop(context.Background(), handle)
	if err != nil {
		t.Fatalf("DriveStop() error = %v", err)
	}
	if report.Terminated {
		t.Fatalf("report = %+v; a pane still answering by id is never observed termination", report)
	}

	// Positively gone by id, but a pane still answers for the label.
	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{}, app.ErrPaneNotFound
	}
	tc.Runtime.FindPaneByLabelFn = func(string) (app.PaneRef, bool, error) {
		return app.PaneRef{WorkspaceID: "workspace-1", TabID: "tab-1", PaneID: detail.Binding.PaneID}, true, nil
	}
	report, err = tc.Controller.DriveStop(context.Background(), handle)
	if err != nil {
		t.Fatalf("label-present DriveStop() error = %v", err)
	}
	if report.Terminated {
		t.Fatalf("report = %+v; a pane still answering for its label is never observed termination", report)
	}

	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{}, context.DeadlineExceeded
	}
	report, err = tc.Controller.DriveStop(context.Background(), handle)
	if err != nil {
		t.Fatalf("second DriveStop() error = %v", err)
	}
	if report.Terminated {
		t.Fatalf("report = %+v; an inspection error is never observed termination", report)
	}

	// Established absence: positively gone by id and by label.
	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{}, app.ErrPaneNotFound
	}
	tc.Runtime.FindPaneByLabelFn = func(string) (app.PaneRef, bool, error) {
		return app.PaneRef{}, false, nil
	}
	final, err := tc.Controller.DriveStop(context.Background(), handle)
	if err != nil {
		t.Fatalf("final DriveStop() error = %v", err)
	}
	if !final.Terminated {
		t.Fatalf("final report = %+v, want terminated once absence is established", final)
	}
}

// TestStopUnboundLaunch covers the crash window where OpenWorkerPane
// created a worker pane but the binding commit was lost: stop must
// recover the pane by its creation label and retire the claimed process,
// never reporting stopped while it lives; without a claim the unresolved
// launch window stays outstanding.
func TestStopUnboundLaunch(t *testing.T) {
	loseBinding := func(t *testing.T, tc *testController) string {
		t.Helper()
		tc.Store.Bindings = map[identity.SessionID][]run.RuntimeBinding{}
		label := ""
		for id, op := range tc.Store.Operations {
			if op.Kind == app.OpPaneOpen {
				op.State = app.OperationPending
				op.ActEvidence = nil
				tc.Store.Operations[id] = op
				label = id.String()
			}
		}
		return label
	}

	t.Run("live pre-binding claim: recovered by label, closed, stopped only on observed absence", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, started := startedRun(t, tc)
		claimLaunch(t, tc, started, 4242)
		label := loseBinding(t, tc)
		detail, err := tc.Store.LoadRunStatus(context.Background(), started.RunID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if detail.Binding != nil || detail.Claim == nil {
			t.Fatalf("fixture: want a pre-binding claim with no binding, got binding=%v claim=%v", detail.Binding, detail.Claim)
		}
		tc.Runtime.FindPaneByLabelFn = func(l string) (app.PaneRef, bool, error) {
			if l == label {
				return app.PaneRef{WorkspaceID: "workspace-1", TabID: "tab-9", PaneID: "pane-9"}, true, nil
			}
			return app.PaneRef{}, false, nil
		}
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "hop", Name: "hop", Argv: []string{"/usr/bin/hop", "launch", "--attempt", started.AttemptID.String()}}}}, nil
		}

		if stopErr := tc.Controller.RequestStop(context.Background(), started.RunID.String()); stopErr != nil {
			t.Fatalf("RequestStop() error = %v", stopErr)
		}
		report, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if report.Terminated {
			t.Fatalf("report = %+v; the run must not report stopped while the unbound worker lives", report)
		}
		if len(tc.Runtime.ClosedPanes) != 1 || tc.Runtime.ClosedPanes[0] != "pane-9" {
			t.Fatalf("ClosedPanes = %v, want the label-recovered pane closed once", tc.Runtime.ClosedPanes)
		}

		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		tc.Runtime.FindPaneByLabelFn = func(string) (app.PaneRef, bool, error) {
			return app.PaneRef{}, false, nil
		}
		final, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("second DriveStop() error = %v", err)
		}
		if !final.Terminated || final.RunState != string(run.RunStopped) {
			t.Fatalf("final report = %+v, want terminated/stopped after observed absence", final)
		}
	})

	t.Run("unresolved launch with no claim: outstanding, never closed, never stopped", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, started := startedRun(t, tc)
		loseBinding(t, tc)

		if stopErr := tc.Controller.RequestStop(context.Background(), started.RunID.String()); stopErr != nil {
			t.Fatalf("RequestStop() error = %v", stopErr)
		}
		report, driveErr := tc.Controller.DriveStop(context.Background(), handle)
		if driveErr != nil {
			t.Fatalf("DriveStop() error = %v", driveErr)
		}
		if report.Terminated {
			t.Fatalf("report = %+v; an unobserved pre-claim launch window is never absence", report)
		}
		if len(report.Outstanding) == 0 {
			t.Fatalf("report = %+v, want the unresolved launch named outstanding", report)
		}
		if len(tc.Runtime.ClosedPanes) != 0 {
			t.Fatalf("ClosePane was dispatched with no recorded identity: %v", tc.Runtime.ClosedPanes)
		}
	})
}

// TestStopUnboundLaunchUnusableIdentity proves an unresolved launch row
// whose persisted identity is unusable — undecodable, or label-less — is
// named outstanding and never skipped into a stopped report.
func TestStopUnboundLaunchUnusableIdentity(t *testing.T) {
	seed := func(t *testing.T, tc *testController, runID identity.RunID, intent any) identity.OperationID {
		t.Helper()
		tc.Store.Bindings = map[identity.SessionID][]run.RuntimeBinding{}
		for id, op := range tc.Store.Operations {
			if op.Kind == app.OpPaneOpen {
				delete(tc.Store.Operations, id)
			}
		}
		opID, err := identity.ParseOperationID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse operation id: %v", err)
		}
		tc.Store.Operations[opID] = app.Operation{
			ID: opID, RunID: runID, Generation: tc.Store.Leases[runID].lease.Generation,
			Kind: app.OpPaneOpen, State: app.OperationPending, Intent: intent,
			CreatedAt: tc.Clock.Now(), UpdatedAt: tc.Clock.Now(),
		}
		return opID
	}

	cases := map[string]any{
		"undecodable persisted intent": "garbage, not an object",
		"label-less persisted intent":  map[string]any{"incarnation_id": "44444444-4444-4444-8444-444444444444", "session_id": "55555555-5555-4555-8555-555555555555"},
	}
	for name, intent := range cases {
		t.Run(name, func(t *testing.T) {
			tc := newTestController(defaultPolicy())
			handle, started := startedRun(t, tc)
			opID := seed(t, tc, started.RunID, intent)

			if err := tc.Controller.RequestStop(context.Background(), started.RunID.String()); err != nil {
				t.Fatalf("RequestStop() error = %v", err)
			}
			report, err := tc.Controller.DriveStop(context.Background(), handle)
			if err != nil {
				t.Fatalf("DriveStop() error = %v", err)
			}
			if report.Terminated {
				t.Fatalf("report = %+v; an unusable launch identity must never be skipped into stopped", report)
			}
			named := false
			for _, line := range report.Outstanding {
				if strings.Contains(line, opID.String()) {
					named = true
				}
			}
			if !named {
				t.Fatalf("Outstanding = %v, want the unusable launch operation named", report.Outstanding)
			}
			if got := tc.Store.Runs[started.RunID].value.State; got != run.RunStopping {
				t.Fatalf("Run.State = %s, want %s", got, run.RunStopping)
			}
		})
	}
}
