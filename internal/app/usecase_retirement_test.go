package app_test

import (
	"context"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// workerFixture is one assigned worker with a settled launch claim: the
// shape per-attempt retirement operates on.
type workerFixture struct {
	SessionID     identity.SessionID
	IncarnationID identity.IncarnationID
	AttemptID     identity.AttemptID
	PaneID        string
	PID           int
}

// seedSettledWorker assigns taskID through the real scheduler, then
// settles its launch: claim execed at pid, session active.
func seedSettledWorker(t *testing.T, tc *testController, fr featureRun, taskID identity.TaskID, pid int) workerFixture { //nolint:gocritic,unparam // hugeParam: featureRun is a small test fixture value; unparam: every current scenario uses one worker pid, but the pid is the fixture's point.
	t.Helper()
	sessionID, incarnationID := seedWorkerSession(t, tc, fr, taskID)
	sessRow := tc.Store.Sessions[sessionID]
	attemptID := sessRow.value.AttemptID
	binding, ok := tc.Store.currentBindingLocked(sessionID)
	if !ok {
		t.Fatalf("no binding for session %s", sessionID)
	}
	tc.Store.LaunchClaims[incarnationID] = app.LaunchClaim{
		IncarnationID: incarnationID, RunID: fr.RunID, SessionID: sessionID, AttemptID: attemptID,
		Executable: "/usr/local/bin/claude", ArgvDigest: "d", PID: pid,
		State: app.LaunchClaimExeced, ClaimedAt: tc.Clock.Now(),
	}
	active, err := sessRow.value.ConfirmActive(tc.Clock.Now())
	if err != nil {
		t.Fatalf("ConfirmActive() error = %v", err)
	}
	sessRow.value = active
	sessRow.revision++
	return workerFixture{SessionID: sessionID, IncarnationID: incarnationID, AttemptID: attemptID, PaneID: binding.PaneID, PID: pid}
}

// composerOccupant makes the worker's pane report the settled occupant
// still alive at its composer: the retirement suite's fixture principal
// contract — the process does NOT exit on its own.
func composerOccupant(tc *testController, w workerFixture) {
	tc.Runtime.InspectPaneFn = func(paneID string) (app.PaneProcess, error) {
		if paneID == w.PaneID {
			return app.PaneProcess{
				ShellPID: 1, ForegroundGroupID: w.PID,
				Foreground: []app.ProcessInfo{{PID: w.PID, Argv: []string{"/usr/local/bin/claude", "--session-id", "ref"}, Cmdline: "claude " + w.IncarnationID.String()}},
			}, nil
		}
		return app.PaneProcess{}, app.ErrPaneNotFound
	}
}

func TestRetireSettledSessions(t *testing.T) {
	t.Run("an accepted result retires the worker; the slot frees only on observed absence", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
		w := seedSettledWorker(t, tc, fr, taskID, 4711)

		// The result accepted: attempt submitted, task checking.
		resultID, err := identity.ParseResultID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse result id: %v", err)
		}
		tc.Store.Results[w.AttemptID] = run.Result{ID: resultID, AttemptID: w.AttemptID, CommitOID: "c", Accepted: true, SubmittedAt: tc.Clock.Now()}
		tc.Store.Attempts[w.AttemptID].value.State = run.AttemptSubmitted
		tc.Store.Tasks[taskID].value.State = run.TaskChecking

		// Round 1: the fixture principal idles at its composer — the close
		// is dispatched, absence is NOT observed, the slot stays occupied.
		composerOccupant(tc, w)
		report, err := tc.Controller.RetireSettledSessions(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() error = %v", err)
		}
		if len(report.Retired) != 0 || len(report.Outstanding) == 0 {
			t.Fatalf("round 1 = %+v, want outstanding, nothing retired on dispatch alone", report)
		}
		if len(tc.Runtime.ClosedPanes) != 1 || tc.Runtime.ClosedPanes[0] != w.PaneID {
			t.Fatalf("ClosedPanes = %v, want the worker pane closed once", tc.Runtime.ClosedPanes)
		}
		if got := tc.Store.Sessions[w.SessionID].value.State; got == run.SessionTerminated || got == run.SessionLost {
			t.Fatalf("session state = %s after a dispatched close; termination is observed, never declared", got)
		}

		// Round 2: the pane is now positively absent.
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound }
		report, err = tc.Controller.RetireSettledSessions(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() round 2 error = %v", err)
		}
		if len(report.Retired) != 1 || report.Retired[0] != w.SessionID.String() {
			t.Fatalf("round 2 = %+v, want the worker retired", report)
		}
		if got := tc.Store.Sessions[w.SessionID].value.State; got != run.SessionTerminated {
			t.Fatalf("session state = %s, want terminated on observed absence", got)
		}
		// The retirement is journaled with the boundary as the reason.
		found := false
		for _, tr := range tc.Store.Transitions {
			if tr.EntityKind == app.EntitySession && tr.EntityID == w.SessionID.String() && tr.To == string(run.SessionTerminated) {
				if tr.Reason != "per-attempt retirement: implement attempt's accepted result" {
					t.Fatalf("termination reason = %q, want the boundary", tr.Reason)
				}
				found = true
			}
		}
		if !found {
			t.Fatalf("no termination transition journaled")
		}
		// The freed slot admits new work.
		taskC := seedImplementTask(t, tc, fr.RunID, 3, "C", false, run.TaskReady)
		assignReport, err := tc.Controller.AssignReadyTasks(context.Background(), fr.Handle, defaultAssignmentOptions())
		if err != nil {
			t.Fatalf("AssignReadyTasks() error = %v", err)
		}
		if len(assignReport.Assigned) == 0 {
			t.Fatalf("the freed slot did not admit task %s", taskC)
		}
	})

	t.Run("a mismatched occupant stays outstanding, never absent", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
		w := seedSettledWorker(t, tc, fr, taskID, 4711)
		tc.Store.Attempts[w.AttemptID].value.State = run.AttemptFailed

		// A different pid under a foreign argv answers in the pane.
		tc.Runtime.InspectPaneFn = func(paneID string) (app.PaneProcess, error) {
			if paneID == w.PaneID {
				return app.PaneProcess{ShellPID: 1, Foreground: []app.ProcessInfo{{PID: 9999, Argv: []string{"/bin/other"}, Cmdline: "other"}}}, nil
			}
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		report, err := tc.Controller.RetireSettledSessions(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() error = %v", err)
		}
		if len(report.Retired) != 0 || len(report.Outstanding) == 0 {
			t.Fatalf("report = %+v, want outstanding on the mismatch", report)
		}
		if len(tc.Runtime.ClosedPanes) != 0 {
			t.Fatalf("ClosedPanes = %v; a mismatched occupant is never closed", tc.Runtime.ClosedPanes)
		}
	})

	t.Run("an exec-failed claim retires without a process", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
		w := seedSettledWorker(t, tc, fr, taskID, 4711)
		claim := tc.Store.LaunchClaims[w.IncarnationID]
		claim.State = app.LaunchClaimExecFailed
		tc.Store.LaunchClaims[w.IncarnationID] = claim
		tc.Store.Attempts[w.AttemptID].value.State = run.AttemptFailed

		report, err := tc.Controller.RetireSettledSessions(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() error = %v", err)
		}
		if len(report.Retired) != 1 {
			t.Fatalf("report = %+v, want the exec-failed session retired directly", report)
		}
		if got := tc.Store.Sessions[w.SessionID].value.State; got != run.SessionTerminated {
			t.Fatalf("session state = %s, want terminated", got)
		}
	})

	t.Run("an in-flight attempt is never retired", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
		w := seedSettledWorker(t, tc, fr, taskID, 4711)
		composerOccupant(tc, w)

		report, err := tc.Controller.RetireSettledSessions(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() error = %v", err)
		}
		if len(report.Retired) != 0 || len(report.Outstanding) != 0 || len(tc.Runtime.ClosedPanes) != 0 {
			t.Fatalf("report = %+v closes %v; a running attempt has reached no boundary", report, tc.Runtime.ClosedPanes)
		}
	})

	t.Run("a failed task fails the run only after owned-work termination is observed", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
		w := seedSettledWorker(t, tc, fr, taskID, 4711)
		tc.Store.Tasks[taskID].value.State = run.TaskFailed
		tc.Store.Attempts[w.AttemptID].value.State = run.AttemptFailed

		// Round 1: the worker still idles; the run must not fail yet.
		composerOccupant(tc, w)
		report, err := tc.Controller.RetireSettledSessions(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() error = %v", err)
		}
		if report.RunFailed {
			t.Fatalf("run failed while the worker was still observed alive")
		}
		if got := tc.Store.Runs[fr.RunID].value.State; got != run.RunRunning {
			t.Fatalf("run state = %s, want still running", got)
		}

		// Round 2: everything absent — children and manager retire, the
		// run fails.
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound }
		report, err = tc.Controller.RetireSettledSessions(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() round 2 error = %v", err)
		}
		if !report.RunFailed {
			t.Fatalf("report = %+v, want RunFailed once termination was observed", report)
		}
		if got := tc.Store.Runs[fr.RunID].value.State; got != run.RunFailed {
			t.Fatalf("run state = %s, want failed", got)
		}
		if got := tc.Store.Sessions[fr.ManagerID].value.State; got != run.SessionTerminated {
			t.Fatalf("manager state = %s, want terminated before the failed report", got)
		}

		// Stop precedence variant is structural: with a stop held the
		// terminal state belongs to stop handling.
		if report.RunFailed {
			rRow := tc.Store.Runs[fr.RunID]
			_ = rRow
		}
	})

	t.Run("stop precedence: a held stop leaves the terminal state to stop handling", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
		w := seedSettledWorker(t, tc, fr, taskID, 4711)
		tc.Store.Tasks[taskID].value.State = run.TaskFailed
		tc.Store.Attempts[w.AttemptID].value.State = run.AttemptFailed
		rRow := tc.Store.Runs[fr.RunID]
		rRow.value = rRow.value.RequestStop(tc.Clock.Now())
		rRow.revision++
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound }

		report, err := tc.Controller.RetireSettledSessions(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() error = %v", err)
		}
		if report.RunFailed {
			t.Fatalf("run failed under a held stop; stop handling owns the terminal state")
		}
		if got := tc.Store.Runs[fr.RunID].value.State; got != run.RunStopping {
			t.Fatalf("run state = %s, want stopping", got)
		}
	})
}
