package app_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// launchEndedUnplacedNotice is the manager notice reason line for a child
// whose unplaced launch ended.
const launchEndedUnplacedNotice = "the attempt's launch ended before its placement was recorded (no pane answered for its creation label and its launched process was observed gone; claim settled exec_failed)"

// requireUnplacedChildUnsettled asserts nothing moved for u's unplaced
// launch: the claim pending, the pane.open unresolved, no binding, the
// attempt, session and task where the launch left them, no notice.
func requireUnplacedChildUnsettled(t *testing.T, u *unplacedLaunch) {
	t.Helper()
	if got := u.tc.Store.LaunchClaims[u.incarnation]; got.State != app.LaunchClaimExecPending || got.Error != "" {
		t.Errorf("claim = %s (%q), want exec_pending", got.State, got.Error)
	}
	if op := u.tc.Store.Operations[identity.OperationID(u.label)]; op.State != app.OperationPending {
		t.Errorf("pane.open %s state = %s, want still pending", u.label, op.State)
	}
	session := u.tc.Store.Sessions[u.sessionID].value
	if session.State != run.SessionLaunching {
		t.Errorf("session state = %s, want launching", session.State)
	}
	if got := u.tc.Store.Attempts[session.AttemptID].value.State; got != run.AttemptLaunching {
		t.Errorf("attempt state = %s, want launching", got)
	}
	if n := len(controllerNoticesTo(u.tc, u.runID)); n != 0 {
		t.Errorf("manager notices = %d, want none", n)
	}
}

// TestUnplacedLaunchEndedRowSettlesChildren proves the label-only variant
// of the launch-ended row on the controller loop's corroboration: a child
// whose pane.open outcome was lost, whose creation label answers nothing
// and whose claimed process is gone settles exec_failed with the distinct
// reason, its pane.open resolved failed as dispatched, and the child takes
// the exec failure's terminal attempt outcome — idempotently on later
// rounds, and finished from the durable settlement after a crash.
func TestUnplacedLaunchEndedRowSettlesChildren(t *testing.T) {
	t.Run("settled and the child failed", func(t *testing.T) {
		u := unplacedWorkerWith(t, true, false)
		delete(u.panes, u.label)
		attemptID := u.tc.Store.Sessions[u.sessionID].value.AttemptID
		taskID := u.tc.Store.Attempts[attemptID].value.TaskID

		reports, err := u.tc.Controller.CorroborateSessionLaunches(context.Background(), u.handle)
		if err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		if len(reports) != 1 || reports[0].SessionID != u.sessionID.String() || reports[0].Progress != app.LaunchFailed {
			t.Fatalf("reports = %+v, want the child reported failed", reports)
		}
		if !slices.Contains(u.tc.Groups.Listed, unplacedPID) {
			t.Errorf("listed groups = %v, want the claimed pid's own group inspected", u.tc.Groups.Listed)
		}
		requireUnplacedLaunchEnded(t, u.tc, u.incarnation, unplacedPID)
		if got := u.tc.Store.Attempts[attemptID].value.State; got != run.AttemptFailed {
			t.Errorf("attempt state = %s, want failed", got)
		}
		if got := u.tc.Store.Tasks[taskID].value.State; got != run.TaskNeedsRework {
			t.Errorf("task state = %s, want needs-rework", got)
		}
		if got := u.tc.Store.Sessions[u.sessionID].value.State; got != run.SessionTerminated {
			t.Errorf("session state = %s, want terminated", got)
		}
		wantReason := "exec_failed claim: " + launchEndedUnplacedReason
		if reason, ok := transitionReason(u.tc, app.EntityAttempt, attemptID.String(), string(run.AttemptFailed)); !ok || reason != wantReason {
			t.Errorf("attempt transition reason = %q (found %t), want %q", reason, ok, wantReason)
		}
		notices := controllerNoticesTo(u.tc, u.runID)
		if len(notices) != 1 || string(u.tc.Artifacts.files[notices[0].BodyPath]) != "task t1 needs-rework\nreason: "+launchEndedUnplacedNotice+"\n" {
			t.Fatalf("manager notices = %+v, want the label-only launch-ended notice", notices)
		}
		if len(u.tc.Store.Bindings[u.sessionID]) != 0 || len(u.tc.Runtime.ClosedPanes) != 0 {
			t.Errorf("bindings %v, closed panes %v; want none for a pane that no longer exists", u.tc.Store.Bindings[u.sessionID], u.tc.Runtime.ClosedPanes)
		}

		// Idempotent: nothing is launching any more, the settlement stands,
		// and no second notice is committed.
		settledAt := u.tc.Store.LaunchClaims[u.incarnation].SettledAt
		u.tc.Clock.Advance(1)
		again, err := u.tc.Controller.CorroborateSessionLaunches(context.Background(), u.handle)
		if err != nil || len(again) != 0 {
			t.Fatalf("second corroboration = %+v, %v; want nothing left", again, err)
		}
		if got := u.tc.Store.LaunchClaims[u.incarnation].SettledAt; !got.Equal(settledAt) {
			t.Errorf("claim settled at %v after a later round, want %v kept", got, settledAt)
		}
		if n := len(controllerNoticesTo(u.tc, u.runID)); n != 1 {
			t.Errorf("manager notices after a later round = %d, want still one", n)
		}
	})

	t.Run("a crash after the settlement: the resolved intent still names the claim", func(t *testing.T) {
		u := unplacedWorkerWith(t, true, false)
		delete(u.panes, u.label)
		// The previous controller committed only the row's own transaction.
		settleOnlyTheUnplacedRow(t, u)
		// Whatever the runtime shows now, the settled claim decides.
		u.tc.Runtime.FindPaneByLabelFn = func(string) (app.PaneRef, bool, error) {
			return app.PaneRef{}, false, errors.New("herdr: session.snapshot: connection reset by peer")
		}

		reports, err := u.tc.Controller.CorroborateSessionLaunches(context.Background(), u.handle)
		if err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		if len(reports) != 1 || reports[0].Progress != app.LaunchFailed {
			t.Fatalf("reports = %+v, want the child reported failed", reports)
		}
		notices := controllerNoticesTo(u.tc, u.runID)
		if len(notices) != 1 || string(u.tc.Artifacts.files[notices[0].BodyPath]) != "task t1 needs-rework\nreason: "+launchEndedUnplacedNotice+"\n" {
			t.Fatalf("manager notices = %+v, want the label-only launch-ended notice", notices)
		}
	})
}

// settleOnlyTheUnplacedRow writes the durable state the row's own
// settling transaction leaves for u — the claim exec_failed with the
// label-only reason, the pane.open resolved failed with the launch-ended
// outcome — and nothing else, as a controller that crashed before the
// child's settlement would leave it.
func settleOnlyTheUnplacedRow(t *testing.T, u *unplacedLaunch) {
	t.Helper()
	u.tc.Store.mu.Lock()
	defer u.tc.Store.mu.Unlock()
	claim := u.tc.Store.LaunchClaims[u.incarnation]
	claim.State = app.LaunchClaimExecFailed
	claim.Error = launchEndedUnplacedReason
	claim.SettledAt = u.tc.Clock.Now()
	claim.SettlementEvidence = "pane= pid=4711 exe=/usr/local/bin/claude marker="
	u.tc.Store.LaunchClaims[u.incarnation] = claim
	for id := range u.tc.Store.Operations {
		op := u.tc.Store.Operations[id]
		if op.Kind != app.OpPaneOpen || op.State != app.OperationPending {
			continue
		}
		op.State = app.OperationFailed
		op.Outcome = map[string]any{
			"launch_ended": true, "dispatched": true, "pane_absent_by_label": true, "claimed_process_gone": true,
			"incarnation_id": u.incarnation.String(), "pid": unplacedPID, "reason": launchEndedUnplacedReason,
		}
		u.tc.Store.Operations[id] = op
	}
}

// TestUnplacedLaunchEndedRowStaysPending proves every observation short of
// the label-only pair leaves an unplaced exec_pending claim ambiguous:
// nothing settles, the pane.open stays unresolved, and the round reports
// pending.
func TestUnplacedLaunchEndedRowStaysPending(t *testing.T) {
	for _, tt := range []struct {
		name       string
		arrange    func(u *unplacedLaunch)
		wantListed bool
	}{
		{
			name: "the claimed process still leads its group",
			arrange: func(u *unplacedLaunch) {
				delete(u.panes, u.label)
				u.tc.Groups.liveLeader(unplacedPID, "/usr/local/bin/claude")
			},
			wantListed: true,
		},
		{
			name: "the claimed pid is still listed among other members",
			arrange: func(u *unplacedLaunch) {
				delete(u.panes, u.label)
				u.tc.Groups.Processes[unplacedPID] = []app.GroupProcess{
					{PID: unplacedPID + 7, Argv: []string{"node", "mcp-server"}},
					{PID: unplacedPID, Argv: []string{app.ArgvUnavailable}},
				}
			},
			wantListed: true,
		},
		{
			name: "the group listing fails",
			arrange: func(u *unplacedLaunch) {
				delete(u.panes, u.label)
				u.tc.Groups.ListErr[unplacedPID] = errors.New("list the process table with ps: exit status 1")
			},
			wantListed: true,
		},
		{
			name: "no process-group inspector",
			arrange: func(u *unplacedLaunch) {
				delete(u.panes, u.label)
				u.tc.Controller.Groups = nil
			},
		},
		{
			name: "the claim records no inspectable pid",
			arrange: func(u *unplacedLaunch) {
				delete(u.panes, u.label)
				claim := u.tc.Store.LaunchClaims[u.incarnation]
				claim.PID = 1
				u.tc.Store.LaunchClaims[u.incarnation] = claim
			},
		},
		{
			name: "the label lookup fails",
			arrange: func(u *unplacedLaunch) {
				u.tc.Runtime.FindPaneByLabelFn = func(string) (app.PaneRef, bool, error) {
					return app.PaneRef{}, false, errors.New("herdr: session.snapshot: connection reset by peer")
				}
			},
		},
		{
			name: "a second unresolved launch names the session",
			arrange: func(u *unplacedLaunch) {
				delete(u.panes, u.label)
				seedPendingLaunchIntent(t, u.tc, u.runID, u.sessionID, identity.IncarnationID(u.tc.IDs.NewID()))
			},
		},
		{
			name: "an undecodable unresolved launch row might name the session",
			arrange: func(u *unplacedLaunch) {
				delete(u.panes, u.label)
				opID := identity.OperationID(u.tc.IDs.NewID())
				u.tc.Store.Operations[opID] = app.Operation{
					ID: opID, RunID: u.runID, Generation: 1, Kind: app.OpPaneOpen, State: app.OperationPending,
					Intent: "not an intent", CreatedAt: u.tc.Clock.Now(), UpdatedAt: u.tc.Clock.Now(),
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			u := unplacedWorkerWith(t, true, false)
			tt.arrange(u)
			for round := range 2 {
				reports, err := u.tc.Controller.CorroborateSessionLaunches(context.Background(), u.handle)
				if err != nil {
					t.Fatalf("round %d: CorroborateSessionLaunches() error = %v", round, err)
				}
				if len(reports) != 1 || reports[0].Progress != app.LaunchPending {
					t.Fatalf("round %d: reports = %+v, want the child pending", round, reports)
				}
			}
			requireUnplacedChildUnsettled(t, u)
			if listed := slices.Contains(u.tc.Groups.Listed, unplacedPID); listed != tt.wantListed {
				t.Errorf("claimed group listed = %t, want %t (listed %v)", listed, tt.wantListed, u.tc.Groups.Listed)
			}
		})
	}

	t.Run("a pane answers for the label: it is bound, never settled", func(t *testing.T) {
		u := unplacedWorkerWith(t, true, false)
		reports, err := u.tc.Controller.CorroborateSessionLaunches(context.Background(), u.handle)
		if err != nil || len(reports) != 1 || reports[0].Progress != app.LaunchPending {
			t.Fatalf("CorroborateSessionLaunches() = %+v, %v; want pending", reports, err)
		}
		if got := u.tc.Store.LaunchClaims[u.incarnation].State; got != app.LaunchClaimExecPending {
			t.Errorf("claim state = %s, want exec_pending", got)
		}
		history := u.tc.Store.Bindings[u.sessionID]
		if len(history) != 1 || history[0].CreationLabel != u.label {
			t.Errorf("bindings = %+v, want the pane recovered by its label", history)
		}
		if len(u.tc.Groups.Listed) != 0 {
			t.Errorf("listed groups = %v, want none: an answering label is never absence", u.tc.Groups.Listed)
		}
	})

	t.Run("a binding committed between the observation and the settlement settles nothing", func(t *testing.T) {
		u := unplacedWorkerWith(t, true, false)
		delete(u.panes, u.label)
		// The label recovery and the row's observation see no pane; a
		// concurrent writer binds the launch right after the row observed it.
		lookups := 0
		u.tc.Runtime.FindPaneByLabelFn = func(string) (app.PaneRef, bool, error) {
			lookups++
			if lookups == 2 {
				u.tc.Store.mu.Lock()
				u.tc.Store.Bindings[u.sessionID] = append(u.tc.Store.Bindings[u.sessionID], run.NewRuntimeBinding(
					u.sessionID, u.incarnation, "", "peer-pid:1", "workspace-1", "tab-w", "pane-late", u.label, run.LaunchInitial, u.tc.Clock.Now()))
				u.tc.Store.mu.Unlock()
			}
			return app.PaneRef{}, false, nil
		}
		reports, err := u.tc.Controller.CorroborateSessionLaunches(context.Background(), u.handle)
		if err != nil || len(reports) != 1 || reports[0].Progress != app.LaunchPending {
			t.Fatalf("CorroborateSessionLaunches() = %+v, %v; want pending", reports, err)
		}
		if got := u.tc.Store.LaunchClaims[u.incarnation].State; got != app.LaunchClaimExecPending {
			t.Errorf("claim state = %s, want exec_pending: the launch was placed meanwhile", got)
		}
	})
}
