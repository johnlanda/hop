package app_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// launchEndedReason is the controller's fixed exec_failed settlement
// reason for a launch whose pane and claimed process were observed gone
// before corroboration.
const launchEndedReason = "launch ended before corroboration: pane absent by id and label; claimed process gone"

// launchEndedNotice is the manager notice reason line for a child whose
// launch ended before corroboration.
const launchEndedNotice = "the attempt's launch ended before it was corroborated (its pane and launched process were observed gone; claim settled exec_failed)"

// errInspectTransport is a pane inspection failure that is not the pinned
// not-found shape: a transport error, never absence.
var errInspectTransport = errors.New("inspect pane pane-x: herdr: connection reset by peer")

// vanishedPane answers InspectPane for paneID with the Herdr adapter's
// pinned not-found shape and every other pane from others (not found too
// when others is nil).
func vanishedPane(paneID string, others func(string) (app.PaneProcess, error)) func(string) (app.PaneProcess, error) {
	return func(id string) (app.PaneProcess, error) {
		if id == paneID || others == nil {
			return app.PaneProcess{}, pinnedPaneNotFound("inspect", id)
		}
		return others(id)
	}
}

// liveManagerOnly answers the seeded manager pane with a corroborated
// occupant at pid and every other pane as not found.
func liveManagerOnly(fr featureRun, pid int) func(string) (app.PaneProcess, error) { //nolint:gocritic // hugeParam: featureRun is a small test fixture value.
	return func(id string) (app.PaneProcess, error) {
		if id != "pane-mgr" {
			return app.PaneProcess{}, pinnedPaneNotFound("inspect", id)
		}
		return app.PaneProcess{
			ShellPID: pid, ForegroundGroupID: pid,
			Foreground: []app.ProcessInfo{{PID: pid, Argv: []string{"/usr/local/bin/claude"}, Cmdline: "claude " + fr.ManagerIncarnation.String()}},
		}, nil
	}
}

// seedLaunchingReviewer assigns a ready review task through the real
// scheduler and records the reviewer's launch claim in claimState.
func seedLaunchingReviewer(t *testing.T, tc *testController, fr featureRun, claimState app.LaunchClaimState) launchingChild { //nolint:gocritic // hugeParam: featureRun is a small test fixture value.
	t.Helper()
	implemented := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskIntegrated)
	reviewID := seedReadyReviewTask(t, tc, fr.RunID, 2, implemented)
	opts := defaultAssignmentOptions()
	opts.ReviewerHarness = run.HarnessClaude
	report, err := tc.Controller.AssignReadyTasks(context.Background(), fr.Handle, opts)
	if err != nil {
		t.Fatalf("AssignReadyTasks() error = %v", err)
	}
	if len(report.Assigned) != 1 || report.Assigned[0].Role != run.RoleReviewer {
		t.Fatalf("assigned = %+v, want the reviewer alone", report.Assigned)
	}
	sessionID := report.Assigned[0].SessionID
	binding, ok := tc.Store.currentBindingLocked(sessionID)
	if !ok {
		t.Fatalf("no binding for reviewer session %s", sessionID)
	}
	attemptID := tc.Store.Sessions[sessionID].value.AttemptID
	tc.Store.LaunchClaims[binding.IncarnationID] = app.LaunchClaim{
		IncarnationID: binding.IncarnationID, RunID: fr.RunID, SessionID: sessionID, AttemptID: attemptID,
		Executable: "/usr/local/bin/claude", ArgvDigest: "d", PID: childLaunchPID,
		State: claimState, ClaimedAt: tc.Clock.Now(),
	}
	return launchingChild{SessionID: sessionID, IncarnationID: binding.IncarnationID, AttemptID: attemptID, TaskID: reviewID, PaneID: binding.PaneID}
}

// requireLaunchEndedClaim asserts the claim settled exec_failed through
// the controller's launch-ended row, with the pane and pid as evidence.
func requireLaunchEndedClaim(t *testing.T, tc *testController, incarnation identity.IncarnationID, paneID string, pid int) {
	t.Helper()
	claim := tc.Store.LaunchClaims[incarnation]
	if claim.State != app.LaunchClaimExecFailed || claim.Error != launchEndedReason {
		t.Fatalf("claim = %s (%q), want exec_failed with the launch-ended reason", claim.State, claim.Error)
	}
	if !strings.Contains(claim.SettlementEvidence, "pane="+paneID+" ") || !strings.Contains(claim.SettlementEvidence, "pid="+strconv.Itoa(pid)+" ") {
		t.Errorf("settlement evidence = %q, want pane %s and pid %d", claim.SettlementEvidence, paneID, pid)
	}
}

// requireChildUnsettled asserts nothing moved for a launching child.
func requireChildUnsettled(t *testing.T, tc *testController, runID identity.RunID, child *launchingChild) {
	t.Helper()
	if got := tc.Store.LaunchClaims[child.IncarnationID]; got.State != app.LaunchClaimExecPending || got.Error != "" {
		t.Errorf("claim = %s (%q), want exec_pending", got.State, got.Error)
	}
	if got := tc.Store.Attempts[child.AttemptID].value.State; got != run.AttemptLaunching {
		t.Errorf("attempt state = %s, want launching", got)
	}
	if got := tc.Store.Sessions[child.SessionID].value.State; got != run.SessionLaunching {
		t.Errorf("session state = %s, want launching", got)
	}
	if got := tc.Store.Tasks[child.TaskID].value.State; got != run.TaskActive {
		t.Errorf("task state = %s, want active", got)
	}
	if n := len(controllerNoticesTo(tc, runID)); n != 0 {
		t.Errorf("manager notices = %d, want none", n)
	}
}

// TestLaunchEndedRowSettlesChildren proves the decision row for an
// exec_pending claim whose placed pane vanished: with the pane absent by
// id and label AND the claimed process gone from its own group, the
// controller settles the claim exec_failed and the child takes the exec
// failure's terminal attempt outcome — for an implementer and a reviewer
// alike — idempotently on later rounds.
func TestLaunchEndedRowSettlesChildren(t *testing.T) {
	for _, tt := range []struct {
		name     string
		reviewer bool
		stop     bool
		wantTask run.TaskState
		notice   string
	}{
		{name: "implementer: needs-rework with retries left", wantTask: run.TaskNeedsRework, notice: "task t1 needs-rework\nreason: " + launchEndedNotice + "\n"},
		{name: "implementer under a held stop: interrupted", stop: true, wantTask: run.TaskInterrupted, notice: "task t1 interrupted\nreason: " + launchEndedNotice + " while a stop was pending\n"},
		{name: "reviewer: needs-rework with retries left", reviewer: true, wantTask: run.TaskNeedsRework, notice: "task t2 needs-rework\nreason: " + launchEndedNotice + "\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tc := newTestController(defaultPolicy())
			fr := seedFeatureRun(t, tc, 2)
			var child launchingChild
			if tt.reviewer {
				child = seedLaunchingReviewer(t, tc, fr, app.LaunchClaimExecPending)
			} else {
				taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
				child = seedLaunchingChild(t, tc, fr, taskID, app.LaunchClaimExecPending)
			}
			if tt.stop {
				rRow := tc.Store.Runs[fr.RunID]
				rRow.value = rRow.value.RequestStop(tc.Clock.Now())
				rRow.revision++
			}
			tc.Runtime.InspectPaneFn = vanishedPane(child.PaneID, liveManagerOnly(fr, 900))

			reports, err := tc.Controller.CorroborateSessionLaunches(context.Background(), fr.Handle)
			if err != nil {
				t.Fatalf("CorroborateSessionLaunches() error = %v", err)
			}
			if len(reports) != 1 || reports[0].SessionID != child.SessionID.String() || reports[0].Progress != app.LaunchFailed {
				t.Fatalf("reports = %+v, want the child reported failed", reports)
			}
			if !slices.Contains(tc.Groups.Listed, childLaunchPID) {
				t.Errorf("listed groups = %v, want the claimed pid's own group inspected", tc.Groups.Listed)
			}
			requireLaunchEndedClaim(t, tc, child.IncarnationID, child.PaneID, childLaunchPID)
			if got := tc.Store.Attempts[child.AttemptID].value.State; got != run.AttemptFailed {
				t.Errorf("attempt state = %s, want failed", got)
			}
			if got := tc.Store.Tasks[child.TaskID].value.State; got != tt.wantTask {
				t.Errorf("task state = %s, want %s", got, tt.wantTask)
			}
			if got := tc.Store.Sessions[child.SessionID].value.State; got != run.SessionTerminated {
				t.Errorf("session state = %s, want terminated", got)
			}
			wantReason := "exec_failed claim: " + launchEndedReason
			if reason, ok := transitionReason(tc, app.EntityAttempt, child.AttemptID.String(), string(run.AttemptFailed)); !ok || reason != wantReason {
				t.Errorf("attempt transition reason = %q (found %t), want %q", reason, ok, wantReason)
			}
			if reason, ok := transitionReason(tc, app.EntitySession, child.SessionID.String(), string(run.SessionTerminated)); !ok || reason != wantReason {
				t.Errorf("session transition reason = %q (found %t), want %q", reason, ok, wantReason)
			}
			notices := controllerNoticesTo(tc, fr.RunID)
			if len(notices) != 1 {
				t.Fatalf("manager notices = %d, want exactly one", len(notices))
			}
			if body := string(tc.Artifacts.files[notices[0].BodyPath]); body != tt.notice {
				t.Errorf("notice body = %q, want %q", body, tt.notice)
			}

			// Idempotent: a later round finds nothing launching, the claim
			// keeps its settlement, and no second notice is committed.
			settledAt := tc.Store.LaunchClaims[child.IncarnationID].SettledAt
			tc.Clock.Advance(1)
			again, err := tc.Controller.CorroborateSessionLaunches(context.Background(), fr.Handle)
			if err != nil || len(again) != 0 {
				t.Fatalf("second corroboration = %+v, %v; want nothing left", again, err)
			}
			if got := tc.Store.LaunchClaims[child.IncarnationID].SettledAt; !got.Equal(settledAt) {
				t.Errorf("claim settled at %v after a later round, want the first settlement %v kept", got, settledAt)
			}
			if n := len(controllerNoticesTo(tc, fr.RunID)); n != 1 {
				t.Errorf("manager notices after a later round = %d, want still one", n)
			}
		})
	}

	t.Run("a crash after the claim settlement: the exec_failed row finishes it", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
		child := seedLaunchingChild(t, tc, fr, taskID, app.LaunchClaimExecPending)
		// The previous controller committed only the claim settlement.
		claim := tc.Store.LaunchClaims[child.IncarnationID]
		claim.State = app.LaunchClaimExecFailed
		claim.Error = launchEndedReason
		tc.Store.LaunchClaims[child.IncarnationID] = claim
		// Whatever the pane shows now, the settled claim decides.
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{}, errInspectTransport }

		reports, err := tc.Controller.CorroborateSessionLaunches(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		if len(reports) != 1 || reports[0].Progress != app.LaunchFailed {
			t.Fatalf("reports = %+v, want the child reported failed", reports)
		}
		if got := tc.Store.Tasks[taskID].value.State; got != run.TaskNeedsRework {
			t.Errorf("task state = %s, want needs-rework", got)
		}
		notices := controllerNoticesTo(tc, fr.RunID)
		if len(notices) != 1 || string(tc.Artifacts.files[notices[0].BodyPath]) != "task t1 needs-rework\nreason: "+launchEndedNotice+"\n" {
			t.Errorf("manager notices = %+v, want the launch-ended notice", notices)
		}
	})
}

// TestLaunchEndedRowResolvesDeferredClaimWindows exercises the two
// claim-protocol windows docs/plan/phase-3-design.md section 11 recorded
// as deferred, through their observable sequences: a launcher killed
// between its claim write and exec, and an exec failure whose exec_failed
// write never landed. Both leave an exec_pending claim behind a pane that
// closes with its process, and both now settle exec_failed once the pair
// is observed — never earlier.
func TestLaunchEndedRowResolvesDeferredClaimWindows(t *testing.T) {
	for _, tt := range []struct {
		name string
		// before is the pane's occupant while the launcher still lives.
		before app.ProcessInfo
	}{
		{
			name:   "launcher killed between the claim write and exec",
			before: app.ProcessInfo{PID: childLaunchPID, Name: "hop", Argv: []string{"/usr/local/bin/hop", "launch", "--run", "r", "--session", "s"}},
		},
		{
			name:   "exec failed and the exec_failed write was suppressed",
			before: app.ProcessInfo{PID: childLaunchPID, Name: "hop", Argv: []string{"/usr/local/bin/hop", "launch", "--run", "r", "--session", "s"}, Cmdline: "hop launch"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tc := newTestController(defaultPolicy())
			fr := seedFeatureRun(t, tc, 2)
			taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
			child := seedLaunchingChild(t, tc, fr, taskID, app.LaunchClaimExecPending)
			alive := true
			tc.Groups.liveLeader(childLaunchPID, tt.before.Argv...)
			tc.Runtime.InspectPaneFn = func(id string) (app.PaneProcess, error) {
				if id == child.PaneID && alive {
					return app.PaneProcess{ShellPID: childLaunchPID, ForegroundGroupID: childLaunchPID, Foreground: []app.ProcessInfo{tt.before}}, nil
				}
				return vanishedPane(child.PaneID, liveManagerOnly(fr, 900))(id)
			}

			// The launcher still occupies its pane: never settled.
			reports, err := tc.Controller.CorroborateSessionLaunches(context.Background(), fr.Handle)
			if err != nil || len(reports) != 1 || reports[0].Progress != app.LaunchPending {
				t.Fatalf("round 1 = %+v, %v; want pending", reports, err)
			}
			requireChildUnsettled(t, tc, fr.RunID, &child)

			// The launcher is gone and its pane closed with it.
			alive = false
			tc.Groups.Processes[childLaunchPID] = nil
			reports, err = tc.Controller.CorroborateSessionLaunches(context.Background(), fr.Handle)
			if err != nil || len(reports) != 1 || reports[0].Progress != app.LaunchFailed {
				t.Fatalf("round 2 = %+v, %v; want failed", reports, err)
			}
			requireLaunchEndedClaim(t, tc, child.IncarnationID, child.PaneID, childLaunchPID)
			if got := tc.Store.Tasks[taskID].value.State; got != run.TaskNeedsRework {
				t.Errorf("task state = %s, want needs-rework", got)
			}
		})
	}
}

// TestLaunchEndedRowStaysPending proves every observation short of the
// corroborated-absence pair leaves an exec_pending claim ambiguous:
// nothing settles, nothing is recorded, and the round reports pending.
func TestLaunchEndedRowStaysPending(t *testing.T) {
	for _, tt := range []struct {
		name string
		// arrange scripts the observation for the child's pane and pid.
		arrange func(tc *testController, fr featureRun, child *launchingChild)
		// wantListed is whether the claimed process group is inspected.
		wantListed bool
	}{
		{
			name: "pane absent, claimed process still leads its group",
			arrange: func(tc *testController, fr featureRun, child *launchingChild) {
				tc.Runtime.InspectPaneFn = vanishedPane(child.PaneID, liveManagerOnly(fr, 900))
				tc.Groups.liveLeader(childLaunchPID, "/usr/local/bin/claude", "--session-id", "ref")
			},
			wantListed: true,
		},
		{
			name: "pane absent, the claimed pid still listed among other members",
			arrange: func(tc *testController, fr featureRun, child *launchingChild) {
				tc.Runtime.InspectPaneFn = vanishedPane(child.PaneID, liveManagerOnly(fr, 900))
				tc.Groups.Processes[childLaunchPID] = []app.GroupProcess{
					{PID: childLaunchPID + 7, Argv: []string{"node", "mcp-server"}},
					{PID: childLaunchPID, Argv: []string{app.ArgvUnavailable}},
				}
			},
			wantListed: true,
		},
		{
			name: "pane absent, group listing fails",
			arrange: func(tc *testController, fr featureRun, child *launchingChild) {
				tc.Runtime.InspectPaneFn = vanishedPane(child.PaneID, liveManagerOnly(fr, 900))
				tc.Groups.ListErr[childLaunchPID] = errors.New("list the process table with ps: exit status 1")
			},
			wantListed: true,
		},
		{
			name: "pane absent, no process-group inspector",
			arrange: func(tc *testController, fr featureRun, child *launchingChild) {
				tc.Runtime.InspectPaneFn = vanishedPane(child.PaneID, liveManagerOnly(fr, 900))
				tc.Controller.Groups = nil
			},
		},
		{
			name: "pane absent, the claim records no inspectable pid",
			arrange: func(tc *testController, fr featureRun, child *launchingChild) {
				tc.Runtime.InspectPaneFn = vanishedPane(child.PaneID, liveManagerOnly(fr, 900))
				claim := tc.Store.LaunchClaims[child.IncarnationID]
				claim.PID = 1
				tc.Store.LaunchClaims[child.IncarnationID] = claim
			},
		},
		{
			name: "pane absent by id, but a pane answers for the creation label",
			arrange: func(tc *testController, fr featureRun, child *launchingChild) {
				tc.Runtime.InspectPaneFn = vanishedPane(child.PaneID, liveManagerOnly(fr, 900))
				tc.Runtime.FindPaneByLabelFn = func(string) (app.PaneRef, bool, error) {
					return app.PaneRef{WorkspaceID: "ws-restored", TabID: "tab-restored", PaneID: "pane-restored"}, true, nil
				}
			},
		},
		{
			name: "pane absent by id, the label lookup fails",
			arrange: func(tc *testController, fr featureRun, child *launchingChild) {
				tc.Runtime.InspectPaneFn = vanishedPane(child.PaneID, liveManagerOnly(fr, 900))
				tc.Runtime.FindPaneByLabelFn = func(string) (app.PaneRef, bool, error) {
					return app.PaneRef{}, false, errors.New("herdr: session.snapshot: connection reset by peer")
				}
			},
		},
		{
			// After a restart a renamed pane is restored under its new name, and
			// until its deferred native restore fires it has no runtime: it
			// answers not found by id and nothing by its creation label.
			name: "relabel plus restart: the renamed pane's native restore is still deferred",
			arrange: func(tc *testController, fr featureRun, child *launchingChild) {
				tc.Runtime.InspectPaneFn = vanishedPane(child.PaneID, liveManagerOnly(fr, 900))
				tc.Runtime.ServerInstanceValue = fakeServerToken(2)
			},
		},
		{
			name: "pane absent, the server lifetime is unknown",
			arrange: func(tc *testController, fr featureRun, child *launchingChild) {
				tc.Runtime.InspectPaneFn = vanishedPane(child.PaneID, liveManagerOnly(fr, 900))
				tc.Runtime.ServerInstanceErr = errors.New("socket gone")
			},
		},
		{
			name: "pane absent, the placement recorded no server lifetime",
			arrange: func(tc *testController, fr featureRun, child *launchingChild) {
				tc.Runtime.InspectPaneFn = vanishedPane(child.PaneID, liveManagerOnly(fr, 900))
				history := tc.Store.Bindings[child.SessionID]
				history[len(history)-1].ServerInstance = ""
			},
		},
		{
			name: "pane absent, the placement recorded a server identity in an older format",
			arrange: func(tc *testController, fr featureRun, child *launchingChild) {
				tc.Runtime.InspectPaneFn = vanishedPane(child.PaneID, liveManagerOnly(fr, 900))
				history := tc.Store.Bindings[child.SessionID]
				history[len(history)-1].ServerInstance = "peer-pid:41001"
			},
		},
		{
			name: "pane absent, the server restarts between the absence and the process observations",
			arrange: func(tc *testController, fr featureRun, child *launchingChild) {
				tc.Runtime.InspectPaneFn = vanishedPane(child.PaneID, liveManagerOnly(fr, 900))
				tc.Runtime.FindPaneByLabelFn = func(string) (app.PaneRef, bool, error) {
					tc.Runtime.ServerInstanceValue = fakeServerToken(2)
					return app.PaneRef{}, false, nil
				}
			},
			wantListed: true,
		},
		{
			name: "a pane inspection error other than not found",
			arrange: func(tc *testController, fr featureRun, child *launchingChild) {
				tc.Runtime.InspectPaneFn = func(id string) (app.PaneProcess, error) {
					if id == child.PaneID {
						return app.PaneProcess{}, errInspectTransport
					}
					return liveManagerOnly(fr, 900)(id)
				}
			},
		},
		{
			name: "the pane answers with no foreground occupant",
			arrange: func(tc *testController, fr featureRun, child *launchingChild) {
				tc.Runtime.InspectPaneFn = func(id string) (app.PaneProcess, error) {
					if id == child.PaneID {
						return app.PaneProcess{ShellPID: childLaunchPID}, nil
					}
					return liveManagerOnly(fr, 900)(id)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tc := newTestController(defaultPolicy())
			fr := seedFeatureRun(t, tc, 2)
			taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
			child := seedLaunchingChild(t, tc, fr, taskID, app.LaunchClaimExecPending)
			tt.arrange(tc, fr, &child)

			for round := range 2 {
				reports, err := tc.Controller.CorroborateSessionLaunches(context.Background(), fr.Handle)
				if err != nil {
					t.Fatalf("round %d: CorroborateSessionLaunches() error = %v", round, err)
				}
				if len(reports) != 1 || reports[0].Progress != app.LaunchPending {
					t.Fatalf("round %d: reports = %+v, want the child pending", round, reports)
				}
			}
			requireChildUnsettled(t, tc, fr.RunID, &child)
			if listed := slices.Contains(tc.Groups.Listed, childLaunchPID); listed != tt.wantListed {
				t.Errorf("claimed group listed = %t, want %t (listed %v)", listed, tt.wantListed, tc.Groups.Listed)
			}
		})
	}

	t.Run("a superseded binding is never the claim's current placement", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
		child := seedLaunchingChild(t, tc, fr, taskID, app.LaunchClaimExecPending)
		history := tc.Store.Bindings[child.SessionID]
		superseded, err := history[len(history)-1].Supersede("replaced", tc.Clock.Now())
		if err != nil {
			t.Fatalf("Supersede() error = %v", err)
		}
		history[len(history)-1] = superseded
		tc.Runtime.InspectPaneFn = vanishedPane(child.PaneID, liveManagerOnly(fr, 900))

		reports, err := tc.Controller.CorroborateSessionLaunches(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		if len(reports) != 1 || reports[0].Progress != app.LaunchPending {
			t.Fatalf("reports = %+v, want the child pending", reports)
		}
		requireChildUnsettled(t, tc, fr.RunID, &child)
		if len(tc.Groups.Listed) != 0 {
			t.Errorf("listed groups = %v, want none: a superseded placement is never observed", tc.Groups.Listed)
		}
	})
}

// TestLaunchEndedRowFailsTheRunForTheManager proves the manager's side of
// the row: a launching run whose manager pane and claimed process are
// both gone settles the manager's claim exec_failed and fails the run
// through the terminal-failure path, with the manager exec failure as the
// cause; a manager whose process still runs stays pending.
func TestLaunchEndedRowFailsTheRunForTheManager(t *testing.T) {
	t.Run("pane and claimed process gone: the run fails", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		runID, managerID, incarnation, handle := launchingFeatureRun(t, tc)
		tc.Runtime.InspectPaneFn = vanishedPane("pane-mgr", nil)

		reports, err := tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		if len(reports) != 1 || reports[0].Role != string(run.RoleManager) || reports[0].Progress != app.LaunchFailed {
			t.Fatalf("reports = %+v, want the manager reported failed", reports)
		}
		requireLaunchEndedClaim(t, tc, incarnation, "pane-mgr", launchClaimPID)
		if got := tc.Store.Sessions[managerID].value.State; got != run.SessionTerminated {
			t.Errorf("manager state = %s, want terminated", got)
		}
		if reason, ok := transitionReason(tc, app.EntitySession, managerID.String(), string(run.SessionTerminated)); !ok || reason != "exec_failed claim: "+launchEndedReason {
			t.Errorf("manager termination reason = %q (found %t)", reason, ok)
		}
		if got := tc.Store.Runs[runID].value.State; got != run.RunFailed {
			t.Fatalf("run state = %s, want failed", got)
		}
		if reason, ok := transitionReason(tc, app.EntityRun, runID.String(), string(run.RunFailed)); !ok || reason != managerLaunchFailure {
			t.Errorf("run failure reason = %q (found %t), want %q", reason, ok, managerLaunchFailure)
		}
		if again, err := tc.Controller.CorroborateSessionLaunches(context.Background(), handle); err != nil || len(again) != 0 {
			t.Fatalf("second corroboration = %+v, %v; want nothing left", again, err)
		}
	})

	t.Run("pane gone, claimed process alive: the run stays launching", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		runID, managerID, incarnation, handle := launchingFeatureRun(t, tc)
		tc.Runtime.InspectPaneFn = vanishedPane("pane-mgr", nil)
		tc.Groups.liveLeader(launchClaimPID, "/usr/bin/claude")

		reports, err := tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		if len(reports) != 1 || reports[0].Progress != app.LaunchPending {
			t.Fatalf("reports = %+v, want the manager pending", reports)
		}
		if got := tc.Store.LaunchClaims[incarnation].State; got != app.LaunchClaimExecPending {
			t.Errorf("claim state = %s, want exec_pending", got)
		}
		if got := tc.Store.Sessions[managerID].value.State; got != run.SessionLaunching {
			t.Errorf("manager state = %s, want launching", got)
		}
		if got := tc.Store.Runs[runID].value.State; got != run.RunLaunching {
			t.Errorf("run state = %s, want launching", got)
		}
	})
}

// TestLaunchEndedPredicateGovernsStopAndFailure proves stop and the
// terminal-failure shutdown retire an exec_pending session whose pane
// vanished only under the same corroborated-absence pair: with the claimed
// process gone the session is resolved; with it still running the session
// stays outstanding with the named human action, and resolves once the
// process is observed gone.
func TestLaunchEndedPredicateGovernsStopAndFailure(t *testing.T) {
	t.Run("stop", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
		child := seedLaunchingChild(t, tc, fr, taskID, app.LaunchClaimExecPending)
		rRow := tc.Store.Runs[fr.RunID]
		rRow.value = rRow.value.RequestStop(tc.Clock.Now())
		rRow.revision++
		tc.Runtime.InspectPaneFn = vanishedPane(child.PaneID, nil)
		tc.Groups.liveLeader(childLaunchPID, "/usr/local/bin/claude")

		report, err := tc.Controller.DriveFeatureStop(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("DriveFeatureStop() error = %v", err)
		}
		if report.Terminated {
			t.Fatalf("stop report = %+v, want outstanding while the claimed process runs", report)
		}
		want := "session " + child.SessionID.String() + ": the pane is gone but the claimed launch process (pid 4343) still runs; end that process, and a later round observes its exit"
		if !slices.Contains(report.Outstanding, want) {
			t.Fatalf("outstanding = %q, want %q", report.Outstanding, want)
		}
		if got := tc.Store.Sessions[child.SessionID].value.State; got == run.SessionTerminated {
			t.Fatalf("session terminated while its claimed process still runs")
		}
		if len(tc.Runtime.ClosedPanes) != 0 {
			t.Errorf("closed panes = %v, want none: the pane is already gone", tc.Runtime.ClosedPanes)
		}
		closeOp := paneCloseOperationFor(t, tc, fr.RunID, child.PaneID)
		if closeOp.State != app.OperationReconciling {
			t.Errorf("pane.close operation state = %s, want reconciling", closeOp.State)
		}

		// The process exits: the next round observes the pair and stops.
		tc.Groups.Processes[childLaunchPID] = nil
		report, err = tc.Controller.DriveFeatureStop(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("DriveFeatureStop() round 2 error = %v", err)
		}
		if !report.Terminated || report.RunState != string(run.RunStopped) {
			t.Fatalf("stop report = %+v, want stopped", report)
		}
		if got := tc.Store.Sessions[child.SessionID].value.State; got != run.SessionTerminated {
			t.Errorf("session state = %s, want terminated", got)
		}
		if got := tc.Store.Attempts[child.AttemptID].value.State; got != run.AttemptInterrupted {
			t.Errorf("attempt state = %s, want interrupted by stop", got)
		}
		if got := paneCloseOperationFor(t, tc, fr.RunID, child.PaneID).State; got != app.OperationSucceeded {
			t.Errorf("pane.close operation state = %s, want succeeded", got)
		}
	})

	t.Run("stop with the claimed process already gone resolves in one round", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
		child := seedLaunchingChild(t, tc, fr, taskID, app.LaunchClaimExecPending)
		rRow := tc.Store.Runs[fr.RunID]
		rRow.value = rRow.value.RequestStop(tc.Clock.Now())
		rRow.revision++
		tc.Runtime.InspectPaneFn = vanishedPane(child.PaneID, nil)

		report, err := tc.Controller.DriveFeatureStop(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("DriveFeatureStop() error = %v", err)
		}
		if !report.Terminated || report.RunState != string(run.RunStopped) {
			t.Fatalf("stop report = %+v, want stopped", report)
		}
		if !slices.Contains(tc.Groups.Listed, childLaunchPID) {
			t.Errorf("listed groups = %v, want the claimed group inspected before retiring", tc.Groups.Listed)
		}
	})

	t.Run("terminal failure", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
		child := seedLaunchingChild(t, tc, fr, taskID, app.LaunchClaimExecPending)
		// A task that failed after the child launched is the run's
		// terminal-failure cause.
		seedImplementTask(t, tc, fr.RunID, 2, "failed", false, run.TaskFailed)
		tc.Runtime.InspectPaneFn = allPanesAbsentPinned
		tc.Groups.liveLeader(childLaunchPID, "/usr/local/bin/claude")

		report, err := tc.Controller.RetireSettledSessions(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() error = %v", err)
		}
		if report.RunFailed || !report.RunFailing {
			t.Fatalf("round 1 = %+v, want RunFailing while the claimed process runs", report)
		}
		want := "session " + child.SessionID.String() + ": the pane is gone but the claimed launch process (pid 4343) still runs; end that process, and a later round observes its exit"
		if !slices.Contains(report.Outstanding, want) {
			t.Fatalf("outstanding = %q, want %q", report.Outstanding, want)
		}
		if got := tc.Store.Sessions[child.SessionID].value.State; got != run.SessionLaunching {
			t.Errorf("session state = %s, want launching", got)
		}

		tc.Groups.Processes[childLaunchPID] = nil
		report, err = tc.Controller.RetireSettledSessions(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() round 2 error = %v", err)
		}
		if !report.RunFailed {
			t.Fatalf("round 2 = %+v, want RunFailed", report)
		}
		if got := tc.Store.Sessions[child.SessionID].value.State; got != run.SessionTerminated {
			t.Errorf("session state = %s, want terminated", got)
		}
	})
}

// TestLaunchEndedRowAfterRestartHasNoAutomaticExit pins what the
// continuity conjunct leaves for a placed launch after a server restart:
// resume never settles it and names the rename-back action, and stop
// concludes no absence either and stays stopping with the same action,
// while the same observations under the placement's lifetime finish the
// stop.
func TestLaunchEndedRowAfterRestartHasNoAutomaticExit(t *testing.T) {
	t.Run("resume stays resuming with the rename-back action", func(t *testing.T) {
		f := newResumeFixture(t)
		binding := resumeChildClaim(t, f, app.LaunchClaimExecPending)
		f.tc.Runtime.InspectPaneFn = liveManagerOnly(f.fr, f.ManagerPID)
		f.tc.Runtime.ServerInstanceValue = fakeServerToken(2)

		result, _ := f.resume(t, "")
		if result.Outcome != "reconciling" {
			t.Fatalf("resume = %+v, want reconciling", result)
		}
		report := sessionReport(t, &result, f.ChildID.String())
		want := "launch claim not settled; corroboration continues (the pane is absent by id and by launch label " + binding.CreationLabel +
			", but server continuity since the placement is not established (the Herdr server may have restarted, and a pane renamed before a restart, or one awaiting a deferred restore, stays hidden from both), so no absence is concluded; if a pane of this run was renamed, rename it back to " +
			binding.CreationLabel + ")"
		if report.Disposition != app.SessionPending || report.Detail != want {
			t.Fatalf("child report = %+v, want pending with %q", report, want)
		}
		if got := f.tc.Store.LaunchClaims[binding.IncarnationID].State; got != app.LaunchClaimExecPending {
			t.Errorf("claim state = %s, want exec_pending", got)
		}
		if len(f.tc.Groups.Listed) != 0 {
			t.Errorf("listed groups = %v, want none without continuity", f.tc.Groups.Listed)
		}
		requireRunState(t, f, run.RunResuming)
	})

	t.Run("a placement that recorded no identity names its own action, not the rename-back one", func(t *testing.T) {
		f := newResumeFixture(t)
		binding := resumeChildClaim(t, f, app.LaunchClaimExecPending)
		// The server answers its ordinary lifetime; the placement simply
		// never recorded one, which is the shape of EVERY placement on a
		// platform with no server-lifetime identity.
		recordBindingServer(f, "")
		f.tc.Runtime.InspectPaneFn = liveManagerOnly(f.fr, f.ManagerPID)

		result, _ := f.resume(t, "")
		if result.Outcome != "reconciling" {
			t.Fatalf("resume = %+v, want reconciling", result)
		}
		report := sessionReport(t, &result, f.ChildID.String())
		want := "launch claim not settled; corroboration continues (" + placedUnrecordedLifetimeText(binding.CreationLabel) + ")"
		if report.Disposition != app.SessionPending || report.Detail != want {
			t.Fatalf("child report = %+v, want pending with %q", report, want)
		}
		if strings.Contains(report.Detail, "rename it back to") {
			t.Fatalf("child report = %+v, want no rename-back action: nothing was renamed", report)
		}
		if got := f.tc.Store.LaunchClaims[binding.IncarnationID].State; got != app.LaunchClaimExecPending {
			t.Errorf("claim state = %s, want exec_pending", got)
		}
		requireRunState(t, f, run.RunResuming)
	})

	stopWithChildPaneGone := func(t *testing.T, instance string) (app.StopReport, *testController, launchingChild) {
		t.Helper()
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
		child := seedLaunchingChild(t, tc, fr, taskID, app.LaunchClaimExecPending)
		rRow := tc.Store.Runs[fr.RunID]
		rRow.value = rRow.value.RequestStop(tc.Clock.Now())
		rRow.revision++
		tc.Runtime.InspectPaneFn = vanishedPane(child.PaneID, nil)
		tc.Runtime.ServerInstanceValue = instance

		report, err := tc.Controller.DriveFeatureStop(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("DriveFeatureStop() error = %v", err)
		}
		if got := tc.Store.LaunchClaims[child.IncarnationID]; got.State != app.LaunchClaimExecPending {
			t.Errorf("claim = %s (%q), want exec_pending: stop settles no launch-ended claim", got.State, got.Error)
		}
		return report, tc, child
	}

	t.Run("stop after a restart stays stopping with the rename-back action", func(t *testing.T) {
		report, tc, child := stopWithChildPaneGone(t, fakeServerToken(2))
		if report.Terminated || report.RunState != string(run.RunStopping) {
			t.Fatalf("stop report = %+v, want stopping: no absence is concluded without continuity", report)
		}
		binding, ok := tc.Store.currentBindingLocked(child.SessionID)
		if !ok {
			t.Fatalf("no binding for session %s", child.SessionID)
		}
		want := "session " + child.SessionID.String() + ": " + placedContinuityText(binding.CreationLabel)
		if !slices.Contains(report.Outstanding, want) {
			t.Fatalf("outstanding = %q, want %q", report.Outstanding, want)
		}
	})

	t.Run("stop with the pane really gone under one lifetime finishes", func(t *testing.T) {
		report, _, _ := stopWithChildPaneGone(t, fakeServerToken(1))
		if !report.Terminated || report.RunState != string(run.RunStopped) {
			t.Fatalf("stop report = %+v, want stopped", report)
		}
	})
}

// placedContinuityText is placedContinuityDetail's rendering for label.
func placedContinuityText(label string) string {
	return "the pane is absent by id and by launch label " + label +
		", but server continuity since the placement is not established (the Herdr server may have restarted, and a pane renamed before a restart, or one awaiting a deferred restore, stays hidden from both), so no absence is concluded; if a pane of this run was renamed, rename it back to " + label
}

// allPanesAbsentPinned answers every pane with the adapter's pinned
// not-found shape.
func allPanesAbsentPinned(id string) (app.PaneProcess, error) {
	return app.PaneProcess{}, pinnedPaneNotFound("inspect", id)
}

// paneCloseOperationFor returns the run's one pane.close operation for
// paneID.
func paneCloseOperationFor(t *testing.T, tc *testController, runID identity.RunID, paneID string) app.Operation {
	t.Helper()
	var found []app.Operation
	for _, op := range tc.Store.Operations { //nolint:gocritic // rangeValCopy: test helper over a small map.
		if op.RunID != runID || op.Kind != app.OpPaneClose {
			continue
		}
		if intent, ok := op.Intent.(map[string]any); ok && intent["pane_id"] == paneID {
			found = append(found, op)
		}
	}
	if len(found) != 1 {
		t.Fatalf("pane.close operations for %s = %d, want exactly one", paneID, len(found))
	}
	return found[0]
}

// TestResumeFeatureAppliesTheLaunchEndedRow proves resume's session
// reconciliation applies the same row: a child whose placed launch ended
// before corroboration is settled and retired instead of holding the run
// in reconciliation, a manager's fails the run through the loop, and a
// vanished pane whose claimed process still runs stays pending with the
// reason named.
func TestResumeFeatureAppliesTheLaunchEndedRow(t *testing.T) {
	t.Run("a child: settled, and the run resumes", func(t *testing.T) {
		f := newResumeFixture(t)
		tc := f.tc
		binding := resumeChildClaim(t, f, app.LaunchClaimExecPending)
		attemptID := tc.Store.Sessions[f.ChildID].value.AttemptID
		taskID := tc.Store.Attempts[attemptID].value.TaskID
		tc.Runtime.InspectPaneFn = liveManagerOnly(f.fr, f.ManagerPID)

		result, handle := f.resume(t, "")
		if result.Outcome != "resumed" {
			t.Fatalf("resume outcome = %s, want resumed; %+v", result.Outcome, result)
		}
		report := sessionReport(t, &result, f.ChildID.String())
		if report.Disposition != app.SessionRetiredNoProcess || report.Detail != launchEndedReason {
			t.Errorf("child report = %+v, want %s with the launch-ended reason", report, app.SessionRetiredNoProcess)
		}
		requireLaunchEndedClaim(t, tc, binding.IncarnationID, binding.PaneID, childLaunchPID)
		if got := tc.Store.Attempts[attemptID].value.State; got != run.AttemptFailed {
			t.Errorf("attempt state = %s, want failed", got)
		}
		if got := tc.Store.Tasks[taskID].value.State; got != run.TaskNeedsRework {
			t.Errorf("task state = %s, want needs-rework", got)
		}
		if got := tc.Store.Sessions[f.ChildID].value.State; got != run.SessionTerminated {
			t.Errorf("child session state = %s, want terminated", got)
		}
		notices := controllerNoticesTo(tc, f.fr.RunID)
		if len(notices) != 1 || string(tc.Artifacts.files[notices[0].BodyPath]) != "task t1 needs-rework\nreason: "+launchEndedNotice+"\n" {
			t.Fatalf("manager notices = %+v, want the launch-ended notice", notices)
		}

		// The loop finds nothing stranded, and a second resume is idempotent.
		if _, err := tc.Controller.CorroborateSessionLaunches(context.Background(), handle); err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		if n := len(controllerNoticesTo(tc, f.fr.RunID)); n != 1 {
			t.Errorf("manager notices after the loop = %d, want still one", n)
		}
	})

	t.Run("a child whose claimed process still runs stays pending", func(t *testing.T) {
		f := newResumeFixture(t)
		tc := f.tc
		resumeChildClaim(t, f, app.LaunchClaimExecPending)
		tc.Runtime.InspectPaneFn = liveManagerOnly(f.fr, f.ManagerPID)
		tc.Groups.liveLeader(childLaunchPID, "/usr/local/bin/claude")

		result, _ := f.resume(t, "")
		if result.Outcome != "reconciling" {
			t.Fatalf("resume outcome = %s, want reconciling; %+v", result.Outcome, result)
		}
		report := sessionReport(t, &result, f.ChildID.String())
		want := "launch claim not settled; corroboration continues (the pane is gone but the claimed launch process (pid 4343) still runs; end that process, and a later round observes its exit)"
		if report.Disposition != app.SessionPending || report.Detail != want {
			t.Errorf("child report = %+v, want pending with %q", report, want)
		}
		if got := tc.Store.Sessions[f.ChildID].value.State; got != run.SessionLaunching {
			t.Errorf("child session state = %s, want launching", got)
		}
	})

	t.Run("the manager: retired at resume, the run fails in the loop", func(t *testing.T) {
		f := newResumeFixture(t)
		tc := f.tc
		// The manager's launch was still unsettled when the controller died.
		mgrRow := tc.Store.Sessions[f.fr.ManagerID]
		mgrRow.value.State = run.SessionLaunching
		mgrClaim := tc.Store.LaunchClaims[f.fr.ManagerIncarnation]
		mgrClaim.State = app.LaunchClaimExecPending
		tc.Store.LaunchClaims[f.fr.ManagerIncarnation] = mgrClaim
		childBinding := resumeChildClaim(t, f, app.LaunchClaimExeced)
		childPane := func(id string) (app.PaneProcess, error) {
			if id != childBinding.PaneID {
				return app.PaneProcess{}, pinnedPaneNotFound("inspect", id)
			}
			return app.PaneProcess{
				ShellPID: childLaunchPID, ForegroundGroupID: childLaunchPID,
				Foreground: []app.ProcessInfo{{PID: childLaunchPID, Argv: []string{"/usr/local/bin/claude"}, Cmdline: "claude " + childBinding.IncarnationID.String()}},
			}, nil
		}
		tc.Runtime.InspectPaneFn = childPane

		result, handle := f.resume(t, "")
		if result.Outcome != "resumed" {
			t.Fatalf("resume outcome = %s, want resumed; %+v", result.Outcome, result)
		}
		if report := sessionReport(t, &result, f.fr.ManagerID.String()); report.Disposition != app.SessionRetiredNoProcess || report.Detail != launchEndedReason {
			t.Errorf("manager report = %+v, want %s with the launch-ended reason", report, app.SessionRetiredNoProcess)
		}
		requireLaunchEndedClaim(t, tc, f.fr.ManagerIncarnation, "pane-mgr", f.ManagerPID)
		if got := tc.Store.Sessions[f.fr.ManagerID].value.State; got != run.SessionTerminated {
			t.Fatalf("manager state = %s, want terminated", got)
		}
		if got := tc.Store.Runs[f.fr.RunID].value.State; got != run.RunLaunching {
			t.Fatalf("run state = %s, want launching (the bootstrap continuation returned it)", got)
		}

		// The loop's launching pass drives the terminal failure: the live
		// child blocks the first round, its observed absence the second.
		if _, err := tc.Controller.CorroborateSessionLaunches(context.Background(), handle); err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		if got := tc.Store.Runs[f.fr.RunID].value.State; got != run.RunLaunching {
			t.Fatalf("run state after round 1 = %s, want launching while the child runs", got)
		}
		tc.Runtime.InspectPaneFn = allPanesAbsentPinned
		if _, err := tc.Controller.CorroborateSessionLaunches(context.Background(), handle); err != nil {
			t.Fatalf("CorroborateSessionLaunches() round 2 error = %v", err)
		}
		if got := tc.Store.Runs[f.fr.RunID].value.State; got != run.RunFailed {
			t.Fatalf("run state = %s, want failed", got)
		}
		if reason, ok := transitionReason(tc, app.EntityRun, f.fr.RunID.String(), string(run.RunFailed)); !ok || reason != managerLaunchFailure {
			t.Errorf("run failure reason = %q (found %t), want %q", reason, ok, managerLaunchFailure)
		}
	})
}
