package app_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// unplacedLaunch is a stopping feature run holding one session whose
// pane.open act landed but whose outcome (and so its binding) was lost.
type unplacedLaunch struct {
	tc          *testController
	runID       identity.RunID
	sessionID   identity.SessionID
	incarnation identity.IncarnationID
	label       string
	paneID      string
	handle      app.RunHandle
	// panes is the runtime's label → pane view for the unplaced launch.
	panes map[string]app.PaneRef
}

const unplacedPID = 4711

// launchEndedUnplacedReason is the controller's fixed exec_failed
// settlement reason for a launch whose placement was never recorded, whose
// creation label answers nothing and whose claimed process is gone.
const launchEndedUnplacedReason = "launch ended before its placement was recorded: no pane answers for the creation label; claimed process gone"

// unplacedLiveDetail is the outstanding detail for u's launch while its
// label answers nothing and its claimed process still runs.
func unplacedLiveDetail(u *unplacedLaunch) string {
	return "no pane answers for launch label " + u.label + " but the claimed launch process (pid 4711) still runs; end that process, and a later round observes its exit"
}

// requireUnplacedLaunchEnded asserts the label-only launch-ended row
// settled incarnation's claim exec_failed with pid as its evidence (no
// pane id was ever recorded) and resolved the pane.open naming it failed,
// as dispatched, with the typed launch-ended outcome.
func requireUnplacedLaunchEnded(t *testing.T, tc *testController, incarnation identity.IncarnationID, pid int) {
	t.Helper()
	tc.Store.mu.Lock()
	defer tc.Store.mu.Unlock()
	claim := tc.Store.LaunchClaims[incarnation]
	if claim.State != app.LaunchClaimExecFailed || claim.Error != launchEndedUnplacedReason {
		t.Fatalf("claim = %s (%q), want exec_failed with the label-only launch-ended reason", claim.State, claim.Error)
	}
	if !strings.HasPrefix(claim.SettlementEvidence, "pane= pid="+strconv.Itoa(pid)+" ") {
		t.Errorf("settlement evidence = %q, want no pane and pid %d", claim.SettlementEvidence, pid)
	}
	var resolved []app.Operation
	for id := range tc.Store.Operations {
		op := tc.Store.Operations[id]
		var intent struct {
			IncarnationID string `json:"incarnation_id"`
		}
		decodeInto(t, op.Intent, &intent)
		if op.Kind == app.OpPaneOpen && intent.IncarnationID == incarnation.String() {
			resolved = append(resolved, op)
		}
	}
	if len(resolved) != 1 || resolved[0].State != app.OperationFailed {
		t.Fatalf("pane.open operations for %s = %+v, want exactly one, failed", incarnation, resolved)
	}
	var outcome struct {
		LaunchEnded        bool   `json:"launch_ended"`
		Dispatched         bool   `json:"dispatched"`
		PaneAbsentByLabel  bool   `json:"pane_absent_by_label"`
		ClaimedProcessGone bool   `json:"claimed_process_gone"`
		IncarnationID      string `json:"incarnation_id"`
		PID                int    `json:"pid"`
		Reason             string `json:"reason"`
		Refused            bool   `json:"refused_before_dispatch"`
	}
	decodeInto(t, resolved[0].Outcome, &outcome)
	if !outcome.LaunchEnded || !outcome.Dispatched || !outcome.PaneAbsentByLabel || !outcome.ClaimedProcessGone ||
		outcome.IncarnationID != incarnation.String() || outcome.PID != pid || outcome.Reason != launchEndedUnplacedReason || outcome.Refused {
		t.Errorf("pane.open outcome = %+v, want the dispatched launch-ended outcome", outcome)
	}
}

// pendingPaneIntent decodes the unresolved pane.open intent of session.
func pendingPaneIntent(t *testing.T, tc *testController, session identity.SessionID) (label string, incarnation identity.IncarnationID) {
	t.Helper()
	for id := range tc.Store.Operations {
		op := tc.Store.Operations[id]
		if op.Kind != app.OpPaneOpen || op.State != app.OperationPending {
			continue
		}
		var intent struct {
			Label         string `json:"label"`
			SessionID     string `json:"session_id"`
			IncarnationID string `json:"incarnation_id"`
		}
		decodeInto(t, op.Intent, &intent)
		if intent.SessionID == session.String() {
			return intent.Label, identity.IncarnationID(intent.IncarnationID)
		}
	}
	t.Fatalf("no pending pane.open intent for session %s", session)
	return "", ""
}

// finishUnplaced records the pre-binding claim (when claimed), requests the
// stop (unless the caller retires instead) and takes the lease as the
// successor controller.
func finishUnplaced(t *testing.T, u *unplacedLaunch, claimed bool) {
	finishUnplacedWith(t, u, claimed, true)
}

func finishUnplacedWith(t *testing.T, u *unplacedLaunch, claimed, stop bool) {
	t.Helper()
	u.label, u.incarnation = pendingPaneIntent(t, u.tc, u.sessionID)
	u.paneID = u.panes[u.label].PaneID
	if claimed {
		attemptID := u.tc.Store.Sessions[u.sessionID].value.AttemptID
		u.tc.Store.LaunchClaims[u.incarnation] = app.LaunchClaim{
			IncarnationID: u.incarnation, RunID: u.runID, SessionID: u.sessionID, AttemptID: attemptID,
			Executable: "/usr/local/bin/claude", ArgvDigest: "d", PID: unplacedPID,
			State: app.LaunchClaimExecPending, ClaimedAt: u.tc.Clock.Now(),
		}
	}
	if stop {
		if err := u.tc.Controller.RequestStop(context.Background(), u.runID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
	}
	lease, err := u.tc.Store.AcquireLease(context.Background(), u.runID, "controller-stop")
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	u.handle = app.NewRunHandleForTest(u.runID, lease)
	// The launched occupant idles until its pane is closed; after that the
	// pane is positively gone by id and by label.
	u.tc.Runtime.InspectPaneFn = func(paneID string) (app.PaneProcess, error) {
		closed := false
		for _, p := range u.tc.Runtime.ClosedPanes {
			closed = closed || p == paneID
		}
		if paneID != u.paneID || closed {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		return app.PaneProcess{ShellPID: 1, ForegroundGroupID: unplacedPID, Foreground: []app.ProcessInfo{{
			PID: unplacedPID, Argv0: "claude", Name: "claude",
			Argv:    []string{"/usr/local/bin/claude", "--session-id", "ref", "run " + u.runID.String() + " incarnation " + u.incarnation.String()},
			Cmdline: "claude --session-id ref run " + u.runID.String() + " incarnation " + u.incarnation.String(),
		}}}, nil
	}
	u.tc.Runtime.FindPaneByLabelFn = func(label string) (app.PaneRef, bool, error) {
		ref, ok := u.panes[label]
		if !ok {
			return app.PaneRef{}, false, nil
		}
		for _, p := range u.tc.Runtime.ClosedPanes {
			if p == ref.PaneID {
				return app.PaneRef{}, false, nil
			}
		}
		return ref, true, nil
	}
}

// unplacedManager is the bootstrap manager whose pane.open outcome was
// lost.
func unplacedManager(t *testing.T, claimed bool) *unplacedLaunch {
	t.Helper()
	f := newFeatureStart(t)
	f.onOpenPane = func(req app.WorkerPaneRequest) (app.PaneHandle, bool, error) {
		handle := f.openPaneLocked(&req)
		f.killController()
		return handle, true, nil
	}
	f.crash()
	u := &unplacedLaunch{tc: f.tc, runID: f.runID(), sessionID: f.manager().ID, panes: f.createdPanes}
	finishUnplaced(t, u, claimed)
	return u
}

// unplacedWorker is an assigned implementer whose pane.open outcome was
// lost; the run's manager is already gone.
func unplacedWorker(t *testing.T, claimed bool) *unplacedLaunch {
	t.Helper()
	return unplacedWorkerWith(t, claimed, true)
}

func unplacedWorkerWith(t *testing.T, claimed, stop bool) *unplacedLaunch {
	t.Helper()
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
	panes := map[string]app.PaneRef{}
	tc.Runtime.OpenWorkerPaneFn = func(req app.WorkerPaneRequest) (app.PaneHandle, error) {
		handle := app.PaneHandle{WorkspaceID: req.WorkspaceID, TabID: "tab-w", PaneID: "pane-w"}
		panes[req.Label] = app.PaneRef(handle)
		tc.Store.mu.Lock()
		tc.Store.Leases[fr.RunID].held = false
		tc.Store.mu.Unlock()
		return handle, nil
	}
	if _, err := tc.Controller.AssignReadyTasks(context.Background(), fr.Handle, defaultAssignmentOptions()); err == nil {
		t.Fatalf("AssignReadyTasks() succeeded despite the lost controller")
	}
	tc.Runtime.OpenWorkerPaneFn = nil
	var worker identity.SessionID
	for id, row := range tc.Store.Sessions {
		if row.value.Role == run.RoleImplementer {
			worker = id
		}
	}
	u := &unplacedLaunch{tc: tc, runID: fr.RunID, sessionID: worker, panes: panes}
	finishUnplacedWith(t, u, claimed, stop)
	return u
}

// driveStop runs DriveFeatureStop rounds until the run is stopped or the
// round budget is spent.
func (u *unplacedLaunch) driveStop(t *testing.T, rounds int) app.StopReport {
	t.Helper()
	var report app.StopReport
	for range rounds {
		var err error
		report, err = u.tc.Controller.DriveFeatureStop(context.Background(), u.handle)
		if err != nil {
			t.Fatalf("DriveFeatureStop() error = %v", err)
		}
		if report.Terminated {
			return report
		}
	}
	return report
}

// TestFeatureStopUnplacedLaunch applies Phase 2's unbound-launch rule,
// session-keyed, to a stop over a lost placement, for the manager and a
// worker alike.
func TestFeatureStopUnplacedLaunch(t *testing.T) {
	roles := []struct {
		name  string
		build func(t *testing.T, claimed bool) *unplacedLaunch
	}{
		{"manager", unplacedManager},
		{"worker", unplacedWorker},
	}
	for _, role := range roles {
		t.Run(role.name, func(t *testing.T) {
			t.Run("found: the pane is bound by label, closed under the close rule, and the run stops", func(t *testing.T) {
				u := role.build(t, true)
				report := u.driveStop(t, 3)
				if !report.Terminated || report.RunState != string(run.RunStopped) {
					t.Fatalf("DriveFeatureStop() = %+v, want stopped", report)
				}
				if len(u.tc.Runtime.ClosedPanes) != 1 || u.tc.Runtime.ClosedPanes[0] != u.paneID {
					t.Fatalf("ClosedPanes = %v, want the recovered pane %s closed once", u.tc.Runtime.ClosedPanes, u.paneID)
				}
				history := u.tc.Store.Bindings[u.sessionID]
				if len(history) != 1 || history[0].CreationLabel != u.label || history[0].IncarnationID != u.incarnation {
					t.Fatalf("bindings = %+v, want the pane bound by its creation label", history)
				}
				if got := u.tc.Store.Sessions[u.sessionID].value.State; got != run.SessionTerminated {
					t.Fatalf("session state = %s, want terminated", got)
				}
			})

			t.Run("ended: no pane answers and the claimed process is gone, so the launch settles and the run stops", func(t *testing.T) {
				u := role.build(t, true)
				delete(u.panes, u.label)
				report := u.driveStop(t, 2)
				if !report.Terminated || report.RunState != string(run.RunStopped) {
					t.Fatalf("DriveFeatureStop() = %+v, want stopped", report)
				}
				requireUnplacedLaunchEnded(t, u.tc, u.incarnation, unplacedPID)
				if got := u.tc.Store.Sessions[u.sessionID].value.State; got != run.SessionTerminated {
					t.Fatalf("session state = %s, want terminated", got)
				}
				if len(u.tc.Runtime.ClosedPanes) != 0 {
					t.Fatalf("ClosedPanes = %v; no pane answers, so nothing is closed", u.tc.Runtime.ClosedPanes)
				}
				if len(u.tc.Store.Bindings[u.sessionID]) != 0 {
					t.Fatalf("bindings = %+v, want none for a pane that no longer exists", u.tc.Store.Bindings[u.sessionID])
				}
			})

			for _, tt := range []struct {
				name    string
				claimed bool
				setup   func(u *unplacedLaunch)
				want    string
			}{
				{"absent: a live claimed process with no pane answering stays outstanding", true, func(u *unplacedLaunch) {
					delete(u.panes, u.label)
					u.tc.Groups.liveLeader(unplacedPID, "/usr/local/bin/claude")
				}, "but the claimed launch process (pid 4711) still runs; end that process, and a later round observes its exit"},
				{"absent: a claimed process that cannot be listed stays outstanding", true, func(u *unplacedLaunch) {
					delete(u.panes, u.label)
					u.tc.Groups.ListErr[unplacedPID] = errors.New("list the process table with ps: exit status 1")
				}, "the claimed process's group listing failed"},
				{"ambiguous: a failed label lookup stays outstanding", true, func(u *unplacedLaunch) {
					u.tc.Runtime.FindPaneByLabelFn = func(string) (app.PaneRef, bool, error) {
						return app.PaneRef{}, false, errors.New("2 panes carry this label, want at most one")
					}
				}, "failed or was ambiguous"},
				{"undecodable: an unusable launch row is named, never skipped", true, func(u *unplacedLaunch) {
					opID := identity.OperationID(u.tc.IDs.NewID())
					u.tc.Store.Operations[opID] = app.Operation{
						ID: opID, RunID: u.runID, Generation: 1, Kind: app.OpPaneOpen, State: app.OperationPending,
						Intent: "not an intent", CreatedAt: u.tc.Clock.Now(), UpdatedAt: u.tc.Clock.Now(),
					}
				}, "has an undecodable intent"},
				{"unclaimed: the pre-claim window stays outstanding", false, func(*unplacedLaunch) {}, "no claim; the launch may still be in flight"},
			} {
				t.Run(tt.name, func(t *testing.T) {
					u := role.build(t, tt.claimed)
					tt.setup(u)
					report := u.driveStop(t, 2)
					if report.Terminated || report.RunState != string(run.RunStopping) {
						t.Fatalf("DriveFeatureStop() = %+v, want still stopping", report)
					}
					if joined := strings.Join(report.Outstanding, "\n"); !strings.Contains(joined, tt.want) || !strings.Contains(joined, u.sessionID.String()) {
						t.Fatalf("outstanding = %q, want the session named with %q", joined, tt.want)
					}
					if len(u.tc.Runtime.ClosedPanes) != 0 {
						t.Fatalf("ClosedPanes = %v; nothing is closed without a recovered, matched pane", u.tc.Runtime.ClosedPanes)
					}
					if got := u.tc.Store.Sessions[u.sessionID].value.State; got == run.SessionTerminated || got == run.SessionLost {
						t.Fatalf("session state = %s; an unresolved launch is never absence", got)
					}
				})
			}
		})
	}

	t.Run("an exec-failed claim has nothing live: the session terminates and the run stops", func(t *testing.T) {
		u := unplacedWorker(t, true)
		claim := u.tc.Store.LaunchClaims[u.incarnation]
		claim.State = app.LaunchClaimExecFailed
		u.tc.Store.LaunchClaims[u.incarnation] = claim
		report := u.driveStop(t, 2)
		if !report.Terminated || len(u.tc.Runtime.ClosedPanes) != 0 {
			t.Fatalf("DriveFeatureStop() = %+v (closed %v), want stopped with nothing closed", report, u.tc.Runtime.ClosedPanes)
		}
	})
}

// TestFeatureStopResolvesIntegrationInit proves a stop never reports
// stopped over an unresolved integration.init (design section 4): the ref
// at the base is adopted, another value fails as a collision, an absent
// ref is completed by the same create-only CAS (a zombie landing first is
// adopted), and a read that cannot be made stays outstanding.
func TestFeatureStopResolvesIntegrationInit(t *testing.T) {
	// stoppingWithPendingInit crashes the bootstrap before the init act and
	// hands the run to a stopping controller.
	stoppingWithPendingInit := func(t *testing.T) (*featureStart, app.RunHandle) {
		t.Helper()
		f := newFeatureStart(t)
		f.onHeartbeat = func(n int) {
			if n == 1 {
				f.killController()
			}
		}
		f.crash()
		f.onHeartbeat = nil
		if f.opState(app.OpIntegrationInit) != app.OperationPending {
			t.Fatalf("integration.init is not pending after the crash")
		}
		runID := f.runID()
		if err := f.tc.Controller.RequestStop(context.Background(), runID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
		lease, err := f.tc.Store.AcquireLease(context.Background(), runID, "controller-stop")
		if err != nil {
			t.Fatalf("AcquireLease() error = %v", err)
		}
		return f, app.NewRunHandleForTest(runID, lease)
	}
	stop := func(t *testing.T, f *featureStart, handle app.RunHandle) app.StopReport {
		t.Helper()
		report, err := f.tc.Controller.DriveFeatureStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveFeatureStop() error = %v", err)
		}
		return report
	}

	for _, tt := range []struct {
		name        string
		setup       func(f *featureStart) string
		wantState   app.OperationState
		wantUpdates int
	}{
		{"the ref at the base is adopted", func(f *featureStart) string {
			f.git.setRef(featureRef, f.base)
			return f.base
		}, app.OperationSucceeded, 0},
		{"another value fails as a collision", func(f *featureStart) string {
			foreign := f.git.newCommit("tree-foreign", f.base)
			f.git.setRef(featureRef, foreign)
			return foreign
		}, app.OperationFailed, 0},
		{"an absent ref is completed by the create-only CAS", func(f *featureStart) string {
			return f.base
		}, app.OperationSucceeded, 1},
		{"a zombie landing between the read and the completing act is adopted", func(f *featureStart) string {
			f.onUpdateRef = func(app.Command) (app.CommandResult, bool, error) {
				f.git.setRef(featureRef, f.base)
				return app.CommandResult{ExitCode: 128, Stderr: []byte("fatal: ref exists")}, true, nil
			}
			return f.base
		}, app.OperationSucceeded, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, handle := stoppingWithPendingInit(t)
			wantRef := tt.setup(f)
			report := stop(t, f, handle)
			if !report.Terminated || report.RunState != string(run.RunStopped) {
				t.Fatalf("DriveFeatureStop() = %+v, want stopped", report)
			}
			if state := f.opState(app.OpIntegrationInit); state != tt.wantState {
				t.Fatalf("integration.init state = %s, want %s", state, tt.wantState)
			}
			if got := f.git.ref(featureRef); got != wantRef {
				t.Fatalf("integration ref = %q, want %q", got, wantRef)
			}
			if n := len(f.git.UpdateRefCalls); n != tt.wantUpdates {
				t.Fatalf("update-ref ran %d times, want %d", n, tt.wantUpdates)
			}
			if f.manager().State != run.SessionTerminated {
				t.Fatalf("manager state = %s, want terminated", f.manager().State)
			}
		})
	}

	t.Run("a read error stays outstanding until the ref can be observed", func(t *testing.T) {
		f, handle := stoppingWithPendingInit(t)
		f.onGit = refReadFails
		report := stop(t, f, handle)
		if report.Terminated || !strings.Contains(strings.Join(report.Outstanding, "\n"), "could not be observed") {
			t.Fatalf("DriveFeatureStop() = %+v, want outstanding on the unobservable ref", report)
		}
		if state := f.opState(app.OpIntegrationInit); state != app.OperationReconciling {
			t.Fatalf("integration.init state = %s, want reconciling", state)
		}
		if len(f.git.UpdateRefCalls) != 0 {
			t.Fatalf("the stop acted on an unobserved ref")
		}
		f.onGit = nil
		report = stop(t, f, handle)
		if !report.Terminated || f.opState(app.OpIntegrationInit) != app.OperationSucceeded {
			t.Fatalf("DriveFeatureStop() = %+v, init %s; want stopped once the ref is observable", report, f.opState(app.OpIntegrationInit))
		}
	})

	t.Run("a refused completing act with the ref still unobserved stays outstanding", func(t *testing.T) {
		f, handle := stoppingWithPendingInit(t)
		f.onUpdateRef = func(app.Command) (app.CommandResult, bool, error) {
			return app.CommandResult{ExitCode: 128, Stderr: []byte("fatal: cannot lock ref")}, true, nil
		}
		report := stop(t, f, handle)
		if report.Terminated || len(report.Outstanding) == 0 {
			t.Fatalf("DriveFeatureStop() = %+v, want outstanding", report)
		}
		if state := f.opState(app.OpIntegrationInit); state != app.OperationReconciling {
			t.Fatalf("integration.init state = %s, want reconciling", state)
		}
	})
}

// TestRetirementUnplacedLaunch drives the same session-keyed rule through
// per-attempt retirement: a terminal attempt whose worker's pane.open
// outcome was lost is retired once its pane is recovered by label, and
// stays outstanding while no pane answers.
func TestRetirementUnplacedLaunch(t *testing.T) {
	retire := func(t *testing.T, u *unplacedLaunch) app.RetirementReport {
		t.Helper()
		attemptID := u.tc.Store.Sessions[u.sessionID].value.AttemptID
		u.tc.Store.Attempts[attemptID].value.State = run.AttemptInterrupted
		report, err := u.tc.Controller.RetireSettledSessions(context.Background(), u.handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() error = %v", err)
		}
		return report
	}
	t.Run("found: recovered, closed and retired", func(t *testing.T) {
		u := unplacedWorkerWith(t, true, false)
		report := retire(t, u)
		if len(u.tc.Runtime.ClosedPanes) != 1 || u.tc.Runtime.ClosedPanes[0] != u.paneID {
			t.Fatalf("report %+v, ClosedPanes %v; want the recovered pane closed", report, u.tc.Runtime.ClosedPanes)
		}
		if got := u.tc.Store.Sessions[u.sessionID].value.State; got != run.SessionTerminated {
			t.Fatalf("session state = %s, want terminated once absence is observed", got)
		}
	})
	t.Run("absent with the claimed process alive: outstanding, never retired", func(t *testing.T) {
		u := unplacedWorkerWith(t, true, false)
		delete(u.panes, u.label)
		u.tc.Groups.liveLeader(unplacedPID, "/usr/local/bin/claude")
		report := retire(t, u)
		if len(report.Outstanding) == 0 || !strings.Contains(strings.Join(report.Outstanding, "\n"), unplacedLiveDetail(u)) {
			t.Fatalf("report = %+v, want the unanswered label outstanding with the live process named", report)
		}
		if got := u.tc.Store.Sessions[u.sessionID].value.State; got == run.SessionTerminated {
			t.Fatalf("an unresolved launch was retired")
		}
		if got := u.tc.Store.LaunchClaims[u.incarnation].State; got != app.LaunchClaimExecPending {
			t.Fatalf("claim state = %s, want exec_pending while its process runs", got)
		}
	})
	t.Run("absent with the claimed process gone: settled and retired", func(t *testing.T) {
		u := unplacedWorkerWith(t, true, false)
		delete(u.panes, u.label)
		report := retire(t, u)
		if len(report.Outstanding) != 0 {
			t.Fatalf("report = %+v, want nothing outstanding once the launch is observed ended", report)
		}
		requireUnplacedLaunchEnded(t, u.tc, u.incarnation, unplacedPID)
		if got := u.tc.Store.Sessions[u.sessionID].value.State; got != run.SessionTerminated {
			t.Fatalf("session state = %s, want terminated", got)
		}
	})
}
