package app_test

import (
	"context"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// childLaunchPID is the pid every launching-child fixture's claim records.
const childLaunchPID = 4343

// launchingChild is one child session assigned through the real
// scheduler, still launching, with a launch claim in a chosen state.
type launchingChild struct {
	SessionID     identity.SessionID
	IncarnationID identity.IncarnationID
	AttemptID     identity.AttemptID
	TaskID        identity.TaskID
	PaneID        string
}

// seedLaunchingChild assigns taskID through the real scheduler — attempt
// and session launching, task active — and records a launch claim in
// claimState, without settling anything.
func seedLaunchingChild(t *testing.T, tc *testController, fr featureRun, taskID identity.TaskID, claimState app.LaunchClaimState) launchingChild { //nolint:gocritic // hugeParam: featureRun is a small test fixture value.
	t.Helper()
	sessionID, incarnationID := seedWorkerSession(t, tc, fr, taskID)
	attemptID := tc.Store.Sessions[sessionID].value.AttemptID
	binding, ok := tc.Store.currentBindingLocked(sessionID)
	if !ok {
		t.Fatalf("no binding for session %s", sessionID)
	}
	tc.Store.LaunchClaims[incarnationID] = app.LaunchClaim{
		IncarnationID: incarnationID, RunID: fr.RunID, SessionID: sessionID, AttemptID: attemptID,
		Executable: "/usr/local/bin/claude", ArgvDigest: "d", PID: childLaunchPID,
		State: claimState, ClaimedAt: tc.Clock.Now(),
	}
	if got := tc.Store.Attempts[attemptID].value.State; got != run.AttemptLaunching {
		t.Fatalf("fixture attempt state = %s, want launching", got)
	}
	return launchingChild{SessionID: sessionID, IncarnationID: incarnationID, AttemptID: attemptID, TaskID: taskID, PaneID: binding.PaneID}
}

// controllerNoticesTo lists the run's controller info messages to the
// manager address.
func controllerNoticesTo(tc *testController, runID identity.RunID) []run.Message {
	var out []run.Message
	for _, m := range tc.Store.Messages { //nolint:gocritic // rangeValCopy: test helper over a small map.
		if m.RunID == runID && m.Sender.Kind == run.PrincipalController && m.Recipient.Kind == run.AddressManager {
			out = append(out, m)
		}
	}
	return out
}

// transitionReason returns the reason of the recorded transition of one
// entity to state, and whether it exists.
func transitionReason(tc *testController, kind app.EntityKind, entityID, to string) (string, bool) {
	for _, tr := range tc.Store.Transitions {
		if tr.EntityKind == kind && tr.EntityID == entityID && tr.To == to {
			return tr.Reason, true
		}
	}
	return "", false
}

// seedManagerMember adds one manager session to runID in state, with a
// current binding and a launch claim in claimState: a member of the run's
// manager lineage, for shapes seedFeatureRun alone cannot build.
func seedManagerMember(t *testing.T, tc *testController, runID identity.RunID, state run.SessionState, claimState app.LaunchClaimState) identity.SessionID {
	t.Helper()
	now := tc.Clock.Now()
	id, err := identity.ParseSessionID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse session id: %v", err)
	}
	session := run.NewManagerSession(id, runID, run.HarnessClaude, now)
	session.State = state
	tc.Store.Sessions[id] = &entityRow[run.Session]{value: session, revision: 1}
	incarnation, err := identity.ParseIncarnationID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse incarnation id: %v", err)
	}
	binding := run.NewRuntimeBinding(id, incarnation, "", fakeServerToken(1), "workspace-mgr", "tab-mgr", "pane-"+id.String(), "label-"+id.String(), run.LaunchInitial, now)
	tc.Store.Bindings[id] = append(tc.Store.Bindings[id], binding)
	tc.Store.LaunchClaims[incarnation] = app.LaunchClaim{
		IncarnationID: incarnation, RunID: runID, SessionID: id,
		Executable: "/usr/bin/claude", PID: 5151, State: claimState, ClaimedAt: now,
	}
	return id
}

// TestCorroborateSessionLaunchesSettlesChildExecFailure proves a child's
// exec_failed claim is a terminal attempt outcome settled in one
// transaction — attempt failed, the budgeted task consequence, mailbox
// closure at the limit, the manager notice and the session terminated —
// starting from a still-launching attempt, never a hand-failed one.
func TestCorroborateSessionLaunchesSettlesChildExecFailure(t *testing.T) {
	for _, tt := range []struct {
		name        string
		retryLimit  int
		stop        bool
		obligation  bool
		wantTask    run.TaskState
		wantMailbox bool
		wantNotice  string
	}{
		{
			name: "retry budget left: needs-rework", retryLimit: 3,
			wantTask:   run.TaskNeedsRework,
			wantNotice: "task t1 needs-rework\nreason: the attempt failed to launch (exec_failed claim; no process)\n",
		},
		{
			name: "at the retry limit: failed, mailbox closed with its obligations", retryLimit: 1, obligation: true,
			wantTask: run.TaskFailed, wantMailbox: true,
			wantNotice: "task t1 failed\nreason: the attempt failed to launch (exec_failed claim; no process)\norphaned obligations: ",
		},
		{
			name: "under a held stop: interrupted", retryLimit: 3, stop: true,
			wantTask:   run.TaskInterrupted,
			wantNotice: "task t1 interrupted\nreason: the attempt failed to launch (exec_failed claim; no process) while a stop was pending\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tc := newTestController(defaultPolicy())
			fr := seedFeatureRun(t, tc, 2)
			snapshot := tc.Store.Snapshots[fr.RunID]
			snapshot.Workflow.RetryLimit = tt.retryLimit
			tc.Store.Snapshots[fr.RunID] = snapshot
			taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
			child := seedLaunchingChild(t, tc, fr, taskID, app.LaunchClaimExecFailed)

			var obligationID identity.MessageID
			if tt.obligation {
				id, err := identity.ParseMessageID(tc.IDs.NewID())
				if err != nil {
					t.Fatalf("parse message id: %v", err)
				}
				obligationID = id
				tc.Store.Messages[id] = run.NewInfo(id, fr.RunID, run.ControllerPrincipal(), run.TaskAddress(taskID), "", "/state/m", "d", 1, 1, tc.Clock.Now())
			}
			if tt.stop {
				rRow := tc.Store.Runs[fr.RunID]
				rRow.value = rRow.value.RequestStop(tc.Clock.Now())
				rRow.revision++
			}

			reports, err := tc.Controller.CorroborateSessionLaunches(context.Background(), fr.Handle)
			if err != nil {
				t.Fatalf("CorroborateSessionLaunches() error = %v", err)
			}
			if len(reports) != 1 || reports[0].SessionID != child.SessionID.String() || reports[0].Progress != app.LaunchFailed {
				t.Fatalf("reports = %+v, want the child reported failed", reports)
			}

			if got := tc.Store.Attempts[child.AttemptID].value.State; got != run.AttemptFailed {
				t.Errorf("attempt state = %s, want failed (the observed outcome, stop or not)", got)
			}
			task := tc.Store.Tasks[taskID].value
			if task.State != tt.wantTask {
				t.Errorf("task state = %s, want %s", task.State, tt.wantTask)
			}
			if task.MailboxClosed != tt.wantMailbox {
				t.Errorf("mailbox closed = %t, want %t", task.MailboxClosed, tt.wantMailbox)
			}
			if got := tc.Store.Sessions[child.SessionID].value.State; got != run.SessionTerminated {
				t.Errorf("session state = %s, want terminated", got)
			}
			for entity, id := range map[app.EntityKind]string{app.EntityAttempt: child.AttemptID.String(), app.EntityTask: taskID.String()} {
				to := string(run.AttemptFailed)
				if entity == app.EntityTask {
					to = string(tt.wantTask)
				}
				if reason, ok := transitionReason(tc, entity, id, to); !ok || reason != "exec_failed claim" {
					t.Errorf("%s transition to %s = %q (found %t), want the exec_failed claim reason", entity, to, reason, ok)
				}
			}
			if reason, ok := transitionReason(tc, app.EntitySession, child.SessionID.String(), string(run.SessionTerminated)); !ok || reason != "exec_failed claim; no process" {
				t.Errorf("session termination reason = %q (found %t)", reason, ok)
			}

			notices := controllerNoticesTo(tc, fr.RunID)
			if len(notices) != 1 {
				t.Fatalf("manager notices = %d, want exactly one", len(notices))
			}
			body := string(tc.Artifacts.files[notices[0].BodyPath])
			want := tt.wantNotice
			if tt.obligation {
				want += obligationID.String() + "\n"
			}
			if body != want {
				t.Errorf("notice body = %q, want %q", body, want)
			}

			// Nothing is stranded: a later retirement round finds nothing
			// left to settle for this child.
			if !tt.stop && tt.wantTask == run.TaskNeedsRework {
				if _, err := tc.Controller.RetireSettledSessions(context.Background(), fr.Handle); err != nil {
					t.Fatalf("RetireSettledSessions() error = %v", err)
				}
				if got := tc.Store.Tasks[taskID].value.State; got != run.TaskNeedsRework {
					t.Errorf("task state after a retirement round = %s, want needs-rework", got)
				}
				if again, err := tc.Controller.CorroborateSessionLaunches(context.Background(), fr.Handle); err != nil || len(again) != 0 {
					t.Errorf("second corroboration = %+v, %v; want nothing left", again, err)
				}
			}
		})
	}

	t.Run("pending and already-execed child claims keep their branches", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		pendingTask := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
		pending := seedLaunchingChild(t, tc, fr, pendingTask, app.LaunchClaimExecPending)
		execedTask := seedImplementTask(t, tc, fr.RunID, 2, "B", false, run.TaskReady)
		execed := seedLaunchingChild(t, tc, fr, execedTask, app.LaunchClaimExeced)
		// The pending child's pane shows nothing that corroborates the claim.
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{ShellPID: 1}, nil
		}

		reports, err := tc.Controller.CorroborateSessionLaunches(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		progress := map[string]app.LaunchProgress{}
		for _, r := range reports {
			progress[r.SessionID] = r.Progress
		}
		if progress[pending.SessionID.String()] != app.LaunchPending || progress[execed.SessionID.String()] != app.LaunchSettled {
			t.Fatalf("reports = %+v, want pending and settled", reports)
		}
		if got := tc.Store.Attempts[pending.AttemptID].value.State; got != run.AttemptLaunching {
			t.Errorf("pending attempt state = %s, want launching", got)
		}
		if got := tc.Store.Sessions[pending.SessionID].value.State; got != run.SessionLaunching {
			t.Errorf("pending session state = %s, want launching", got)
		}
		if got := tc.Store.Sessions[execed.SessionID].value.State; got != run.SessionActive {
			t.Errorf("execed session state = %s, want active", got)
		}
		for _, id := range []identity.TaskID{pendingTask, execedTask} {
			if got := tc.Store.Tasks[id].value.State; got != run.TaskActive {
				t.Errorf("task %s state = %s, want active", id, got)
			}
		}
		if n := len(controllerNoticesTo(tc, fr.RunID)); n != 0 {
			t.Errorf("manager notices = %d, want none", n)
		}
	})
}

// TestManagerExecFailureFailsTheRun proves the manager exec-failure rule:
// the run fails through the feature terminal-failure path with the exec
// failure recorded as the cause — from launch corroboration while the run
// is launching, from retirement while it is running — and the disposition
// is durable, so a later round finishes what an earlier one began.
func TestManagerExecFailureFailsTheRun(t *testing.T) {
	const cause = "manager launch exec failed (exec_failed claim; no process) with owned-work termination observed"

	t.Run("a launching run fails from corroboration", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		runID, managerID, incarnation, handle := launchingFeatureRun(t, tc)
		claim := tc.Store.LaunchClaims[incarnation]
		claim.State = app.LaunchClaimExecFailed
		tc.Store.LaunchClaims[incarnation] = claim

		reports, err := tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		if len(reports) != 1 || reports[0].Role != string(run.RoleManager) || reports[0].Progress != app.LaunchFailed {
			t.Fatalf("reports = %+v, want the manager reported failed", reports)
		}
		if got := tc.Store.Sessions[managerID].value.State; got != run.SessionTerminated {
			t.Errorf("manager state = %s, want terminated", got)
		}
		if got := tc.Store.Runs[runID].value.State; got != run.RunFailed {
			t.Fatalf("run state = %s, want failed", got)
		}
		if reason, ok := transitionReason(tc, app.EntityRun, runID.String(), string(run.RunFailed)); !ok || reason != cause {
			t.Errorf("run failure reason = %q (found %t), want %q", reason, ok, cause)
		}

		again, err := tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
		if err != nil || len(again) != 0 {
			t.Fatalf("second corroboration = %+v, %v; want nothing left", again, err)
		}
	})

	t.Run("a launching run whose exec-failed manager was already terminated fails on the next round", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		runID, managerID, incarnation, handle := launchingFeatureRun(t, tc)
		claim := tc.Store.LaunchClaims[incarnation]
		claim.State = app.LaunchClaimExecFailed
		tc.Store.LaunchClaims[incarnation] = claim
		tc.Store.Sessions[managerID].value.State = run.SessionTerminated

		reports, err := tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		if len(reports) != 0 {
			t.Errorf("reports = %+v, want none (no launching session is left)", reports)
		}
		if got := tc.Store.Runs[runID].value.State; got != run.RunFailed {
			t.Fatalf("run state = %s, want failed from the durable disposition", got)
		}
	})

	t.Run("a running run's exec-failed successor manager fails the run once its children are retired", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
		w := seedSettledWorker(t, tc, fr, taskID, 4711)
		composerOccupant(tc, w)
		// The original manager was relaunched away; its successor's exec
		// failed.
		tc.Store.Sessions[fr.ManagerID].value.State = run.SessionLost
		successor := seedManagerMember(t, tc, fr.RunID, run.SessionLaunching, app.LaunchClaimExecFailed)

		report, err := tc.Controller.RetireSettledSessions(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() error = %v", err)
		}
		if !report.RunFailing || report.RunFailed {
			t.Fatalf("round 1 = %+v, want RunFailing while the child still idles", report)
		}
		if len(tc.Runtime.ClosedPanes) != 1 || tc.Runtime.ClosedPanes[0] != w.PaneID {
			t.Fatalf("ClosedPanes = %v, want the child retired for the run's terminal failure", tc.Runtime.ClosedPanes)
		}

		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound }
		report, err = tc.Controller.RetireSettledSessions(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() round 2 error = %v", err)
		}
		if !report.RunFailed || report.RunFailing {
			t.Fatalf("round 2 = %+v, want RunFailed", report)
		}
		if got := tc.Store.Sessions[successor].value.State; got != run.SessionTerminated {
			t.Errorf("successor manager state = %s, want terminated", got)
		}
		if reason, ok := transitionReason(tc, app.EntityRun, fr.RunID.String(), string(run.RunFailed)); !ok || reason != cause {
			t.Errorf("run failure reason = %q (found %t), want %q", reason, ok, cause)
		}
	})
}

// TestManagerExecFailureDispositionFalsePositives proves the manager
// exec-failure cause keys on the lineage's most recent session and fires
// only while the run is launching or running: an earlier exec-failed
// member followed by a working successor never fails the run, and neither
// does a lineage whose members are all legitimately terminal at
// completion or stop.
func TestManagerExecFailureDispositionFalsePositives(t *testing.T) {
	t.Run("an earlier exec-failed member followed by a working successor", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		seedManagerMember(t, tc, fr.RunID, run.SessionLost, app.LaunchClaimExecFailed)
		tc.Store.LaunchClaims[fr.ManagerIncarnation] = app.LaunchClaim{
			IncarnationID: fr.ManagerIncarnation, RunID: fr.RunID, SessionID: fr.ManagerID,
			Executable: "/usr/bin/claude", PID: 5252, State: app.LaunchClaimExeced,
		}

		report, err := tc.Controller.RetireSettledSessions(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() error = %v", err)
		}
		if report.RunFailing || report.RunFailed {
			t.Fatalf("report = %+v, want no failure cause", report)
		}
		if _, err := tc.Controller.CorroborateSessionLaunches(context.Background(), fr.Handle); err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		if got := tc.Store.Runs[fr.RunID].value.State; got != run.RunRunning {
			t.Fatalf("run state = %s, want running", got)
		}
	})

	for _, tt := range []struct {
		name       string
		runState   run.RunState
		stop       bool
		headClaim  app.LaunchClaimState
		wantFailed bool
	}{
		{name: "every manager terminal at completion", runState: run.RunCompleting, headClaim: app.LaunchClaimExeced},
		{name: "every manager terminal at stop", runState: run.RunStopping, stop: true, headClaim: app.LaunchClaimExeced},
		{name: "every manager terminal after completion re-validation returned the run to running", runState: run.RunRunning, headClaim: app.LaunchClaimExeced},
		{name: "a completing run is never failed by the disposition, whatever the head's claim", runState: run.RunCompleting, headClaim: app.LaunchClaimExecFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tc := newTestController(defaultPolicy())
			fr := seedFeatureRun(t, tc, 2)
			seedManagerMember(t, tc, fr.RunID, run.SessionLost, app.LaunchClaimExecFailed)
			tc.Store.Sessions[fr.ManagerID].value.State = run.SessionTerminated
			tc.Store.LaunchClaims[fr.ManagerIncarnation] = app.LaunchClaim{
				IncarnationID: fr.ManagerIncarnation, RunID: fr.RunID, SessionID: fr.ManagerID,
				Executable: "/usr/bin/claude", PID: 5252, State: tt.headClaim,
			}
			rRow := tc.Store.Runs[fr.RunID]
			if tt.stop {
				rRow.value = rRow.value.RequestStop(tc.Clock.Now())
			}
			rRow.value.State = tt.runState

			report, err := tc.Controller.RetireSettledSessions(context.Background(), fr.Handle)
			if err != nil {
				t.Fatalf("RetireSettledSessions() error = %v", err)
			}
			if report.RunFailing || report.RunFailed {
				t.Fatalf("report = %+v, want no failure cause", report)
			}
			if got := tc.Store.Runs[fr.RunID].value.State; got != tt.runState {
				t.Fatalf("run state = %s, want %s untouched", got, tt.runState)
			}
		})
	}
}

// resumeChildClaim records the resume fixture child's launch claim in
// state under childLaunchPID, returning the child's current binding.
func resumeChildClaim(t *testing.T, f *resumeFixture, state app.LaunchClaimState) run.RuntimeBinding {
	t.Helper()
	binding, ok := f.tc.Store.currentBindingLocked(f.ChildID)
	if !ok {
		t.Fatalf("no binding for the fixture child %s", f.ChildID)
	}
	f.tc.Store.LaunchClaims[binding.IncarnationID] = app.LaunchClaim{
		IncarnationID: binding.IncarnationID, RunID: f.fr.RunID, SessionID: f.ChildID,
		AttemptID:  f.tc.Store.Sessions[f.ChildID].value.AttemptID,
		Executable: "/usr/local/bin/claude", ArgvDigest: "d", PID: childLaunchPID,
		State: state, ClaimedAt: f.tc.Clock.Now(),
	}
	return binding
}

// liveManagerPane answers the resume fixture's manager pane with its
// corroborating occupant and every other pane as positively absent;
// onManager, when non-nil, runs on each manager inspection.
func liveManagerPane(f *resumeFixture, onManager func()) func(string) (app.PaneProcess, error) {
	return func(paneID string) (app.PaneProcess, error) {
		if paneID != "pane-mgr" {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		if onManager != nil {
			onManager()
		}
		return app.PaneProcess{
			ShellPID: 1, ForegroundGroupID: f.ManagerPID,
			Foreground: []app.ProcessInfo{{PID: f.ManagerPID, Argv: []string{"/usr/local/bin/claude"}, Cmdline: "claude " + f.fr.ManagerIncarnation.String()}},
		}, nil
	}
}

// allPanesAbsent answers every pane as positively absent.
func allPanesAbsent(string) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound }

// TestResumeFeatureSettlesChildExecFailure proves a child whose launch
// claim is exec_failed and whose controller died before corroboration is
// settled by resume's reconciliation, never merely retired: the attempt
// fails and the task takes the budgeted consequence, with the mailbox
// closure and the manager notice — and a held stop keeps precedence.
func TestResumeFeatureSettlesChildExecFailure(t *testing.T) {
	const failureCause = "task failure with owned-work termination observed"

	for _, tt := range []struct {
		name        string
		retryLimit  int
		wantTask    run.TaskState
		wantMailbox bool
		wantNotice  string
	}{
		{name: "retry budget left: needs-rework", retryLimit: 3, wantTask: run.TaskNeedsRework, wantNotice: "task t1 needs-rework\nreason: the attempt failed to launch (exec_failed claim; no process)\n"},
		{name: "retry budget exhausted: failed, mailbox closed, the run fails on retirement", retryLimit: 1, wantTask: run.TaskFailed, wantMailbox: true, wantNotice: "task t1 failed\nreason: the attempt failed to launch (exec_failed claim; no process)\norphaned obligations: none\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newResumeFixture(t)
			tc := f.tc
			snapshot := tc.Store.Snapshots[f.fr.RunID]
			snapshot.Workflow.RetryLimit = tt.retryLimit
			tc.Store.Snapshots[f.fr.RunID] = snapshot
			resumeChildClaim(t, f, app.LaunchClaimExecFailed)
			attemptID := tc.Store.Sessions[f.ChildID].value.AttemptID
			taskID := tc.Store.Attempts[attemptID].value.TaskID
			tc.Runtime.InspectPaneFn = liveManagerPane(f, nil)

			result, handle := f.resume(t, "")
			if result.Outcome != "resumed" {
				t.Fatalf("resume outcome = %s, want resumed; %+v", result.Outcome, result)
			}
			if report := sessionReport(t, &result, f.ChildID.String()); report.Disposition != app.SessionRetiredNoProcess {
				t.Errorf("child disposition = %s, want %s", report.Disposition, app.SessionRetiredNoProcess)
			}
			if got := tc.Store.Attempts[attemptID].value.State; got != run.AttemptFailed {
				t.Errorf("attempt state = %s, want failed", got)
			}
			task := tc.Store.Tasks[taskID].value
			if task.State != tt.wantTask || task.MailboxClosed != tt.wantMailbox {
				t.Errorf("task = %s (mailbox closed %t), want %s (%t)", task.State, task.MailboxClosed, tt.wantTask, tt.wantMailbox)
			}
			if got := tc.Store.Sessions[f.ChildID].value.State; got != run.SessionTerminated {
				t.Errorf("child session state = %s, want terminated", got)
			}
			notices := controllerNoticesTo(tc, f.fr.RunID)
			if len(notices) != 1 || string(tc.Artifacts.files[notices[0].BodyPath]) != tt.wantNotice {
				t.Fatalf("manager notices = %+v, want exactly one with body %q", notices, tt.wantNotice)
			}

			// The ordinary pass finds nothing stranded.
			if _, err := tc.Controller.CorroborateSessionLaunches(context.Background(), handle); err != nil {
				t.Fatalf("CorroborateSessionLaunches() error = %v", err)
			}
			report, err := tc.Controller.RetireSettledSessions(context.Background(), handle)
			if err != nil {
				t.Fatalf("RetireSettledSessions() error = %v", err)
			}
			if tt.wantTask != run.TaskFailed {
				if report.RunFailing || report.RunFailed || tc.Store.Tasks[taskID].value.State != tt.wantTask {
					t.Errorf("retirement = %+v, task = %s; want nothing further", report, tc.Store.Tasks[taskID].value.State)
				}
				return
			}
			// The failed task fails the run once owned work is observed
			// terminated: the live manager blocks the first round.
			if !report.RunFailing || report.RunFailed {
				t.Fatalf("retirement round 1 = %+v, want RunFailing while the manager is alive", report)
			}
			tc.Runtime.InspectPaneFn = allPanesAbsent
			if report, err = tc.Controller.RetireSettledSessions(context.Background(), handle); err != nil || !report.RunFailed {
				t.Fatalf("retirement round 2 = %+v, %v; want RunFailed", report, err)
			}
			if reason, ok := transitionReason(tc, app.EntityRun, f.fr.RunID.String(), string(run.RunFailed)); !ok || reason != failureCause {
				t.Errorf("run failure reason = %q (found %t), want %q", reason, ok, failureCause)
			}
		})
	}

	t.Run("a stop held before resume routes to stop handling, which interrupts the child", func(t *testing.T) {
		f := newResumeFixture(t)
		tc := f.tc
		resumeChildClaim(t, f, app.LaunchClaimExecFailed)
		attemptID := tc.Store.Sessions[f.ChildID].value.AttemptID
		taskID := tc.Store.Attempts[attemptID].value.TaskID
		rRow := tc.Store.Runs[f.fr.RunID]
		rRow.value = rRow.value.RequestStop(tc.Clock.Now())
		rRow.revision++
		tc.Runtime.InspectPaneFn = allPanesAbsent

		result, handle := f.resume(t, "")
		if result.Outcome != "stop-pending" {
			t.Fatalf("resume outcome = %s, want stop-pending", result.Outcome)
		}
		if got := tc.Store.Attempts[attemptID].value.State; got != run.AttemptLaunching {
			t.Errorf("attempt state after a stop-pending resume = %s, want launching (resume adopts nothing under a stop)", got)
		}
		if n := len(controllerNoticesTo(tc, f.fr.RunID)); n != 0 {
			t.Errorf("manager notices = %d, want none (stop handling owns the terminal state)", n)
		}

		stop, err := tc.Controller.DriveFeatureStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveFeatureStop() error = %v", err)
		}
		if !stop.Terminated || stop.RunState != string(run.RunStopped) {
			t.Fatalf("stop report = %+v, want stopped", stop)
		}
		if got := tc.Store.Attempts[attemptID].value.State; got != run.AttemptInterrupted {
			t.Errorf("attempt state = %s, want interrupted by stop", got)
		}
		if got := tc.Store.Tasks[taskID].value.State; got != run.TaskInterrupted {
			t.Errorf("task state = %s, want interrupted by stop", got)
		}
		if got := tc.Store.Sessions[f.ChildID].value.State; got != run.SessionTerminated {
			t.Errorf("child session state = %s, want terminated", got)
		}
	})

	t.Run("a stop landing during resume interrupts the task in the settlement itself", func(t *testing.T) {
		f := newResumeFixture(t)
		tc := f.tc
		resumeChildClaim(t, f, app.LaunchClaimExecFailed)
		attemptID := tc.Store.Sessions[f.ChildID].value.AttemptID
		taskID := tc.Store.Attempts[attemptID].value.TaskID
		stopped := false
		// The manager reconciles first (sessions reconcile in id order);
		// its pane inspection is the window between resume's stop check
		// and the child's settlement.
		tc.Runtime.InspectPaneFn = liveManagerPane(f, func() {
			if stopped {
				return
			}
			stopped = true
			rRow := tc.Store.Runs[f.fr.RunID]
			rRow.value = rRow.value.RequestStop(tc.Clock.Now())
			rRow.revision++
		})

		result, _ := f.resume(t, "")
		if !stopped {
			t.Fatalf("the manager was never inspected; resume = %+v", result)
		}
		if got := tc.Store.Attempts[attemptID].value.State; got != run.AttemptFailed {
			t.Errorf("attempt state = %s, want failed (the observed outcome)", got)
		}
		if got := tc.Store.Tasks[taskID].value.State; got != run.TaskInterrupted {
			t.Errorf("task state = %s, want interrupted under the stop", got)
		}
		if got := tc.Store.Runs[f.fr.RunID].value.State; got != run.RunStopping {
			t.Errorf("run state = %s, want stopping (never turned back toward running)", got)
		}
		notices := controllerNoticesTo(tc, f.fr.RunID)
		want := "task t1 interrupted\nreason: the attempt failed to launch (exec_failed claim; no process) while a stop was pending\n"
		if len(notices) != 1 || string(tc.Artifacts.files[notices[0].BodyPath]) != want {
			t.Errorf("manager notices = %+v, want exactly one with body %q", notices, want)
		}
	})
}

// TestResumeFeatureManagerExecFailureFailsTheRunOnRetirement proves the
// resume path's manager branch: an exec-failed manager is only retired
// (no attempt to settle), resume completes over the warm child, and the
// manager-lineage failure cause fails the run on the following
// retirement passes, once the child is observed terminated.
func TestResumeFeatureManagerExecFailureFailsTheRunOnRetirement(t *testing.T) {
	const cause = "manager launch exec failed (exec_failed claim; no process) with owned-work termination observed"
	f := newResumeFixture(t)
	tc := f.tc
	managerClaim := tc.Store.LaunchClaims[f.fr.ManagerIncarnation]
	managerClaim.State = app.LaunchClaimExecFailed
	tc.Store.LaunchClaims[f.fr.ManagerIncarnation] = managerClaim
	childBinding := resumeChildClaim(t, f, app.LaunchClaimExeced)
	childPane := func(paneID string) (app.PaneProcess, error) {
		if paneID != childBinding.PaneID {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		return app.PaneProcess{
			ShellPID: 1, ForegroundGroupID: childLaunchPID,
			Foreground: []app.ProcessInfo{{PID: childLaunchPID, Argv: []string{"/usr/local/bin/claude"}, Cmdline: "claude " + childBinding.IncarnationID.String()}},
		}, nil
	}
	tc.Runtime.InspectPaneFn = childPane

	result, handle := f.resume(t, "")
	if result.Outcome != "resumed" {
		t.Fatalf("resume outcome = %s, want resumed; %+v", result.Outcome, result)
	}
	if report := sessionReport(t, &result, f.fr.ManagerID.String()); report.Disposition != app.SessionRetiredNoProcess {
		t.Errorf("manager disposition = %s, want %s", report.Disposition, app.SessionRetiredNoProcess)
	}
	if got := tc.Store.Sessions[f.fr.ManagerID].value.State; got != run.SessionTerminated {
		t.Fatalf("manager state = %s, want terminated", got)
	}

	report, err := tc.Controller.RetireSettledSessions(context.Background(), handle)
	if err != nil {
		t.Fatalf("RetireSettledSessions() error = %v", err)
	}
	if !report.RunFailing || report.RunFailed {
		t.Fatalf("retirement round 1 = %+v, want RunFailing while the child still runs", report)
	}
	tc.Runtime.InspectPaneFn = allPanesAbsent
	if report, err = tc.Controller.RetireSettledSessions(context.Background(), handle); err != nil || !report.RunFailed {
		t.Fatalf("retirement round 2 = %+v, %v; want RunFailed", report, err)
	}
	if reason, ok := transitionReason(tc, app.EntityRun, f.fr.RunID.String(), string(run.RunFailed)); !ok || reason != cause {
		t.Errorf("run failure reason = %q (found %t), want %q", reason, ok, cause)
	}
}
