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

// renamePaneAndRestart has a human rename u's pane and Herdr restart: the
// pane survives under its new name only, and the server lifetime changes.
func renamePaneAndRestart(u *unplacedLaunch) {
	ref := u.panes[u.label]
	delete(u.panes, u.label)
	u.panes["human-renamed"] = ref
	u.tc.Runtime.ServerInstanceValue = fakeServerToken(2)
}

// restoredPane answers u's surviving pane id with restored and every other
// pane as positively absent.
func restoredPane(u *unplacedLaunch, restored app.PaneProcess) func(string) (app.PaneProcess, error) {
	return func(id string) (app.PaneProcess, error) {
		if id != u.paneID {
			return app.PaneProcess{}, pinnedPaneNotFound("inspect", id)
		}
		return restored, nil
	}
}

// recordIntentServer rewrites the server identity u's unresolved pane.open
// intent recorded immediately before the pane was created.
func recordIntentServer(t *testing.T, u *unplacedLaunch, token string) {
	t.Helper()
	intent := pendingIntentPayload(t, u)
	intent["server_instance"] = token
}

// pendingIntentPayload returns the committed JSON payload of u's
// unresolved pane.open intent, for in-place edits.
func pendingIntentPayload(t *testing.T, u *unplacedLaunch) map[string]any {
	t.Helper()
	op, ok := u.tc.Store.Operations[identity.OperationID(u.label)]
	if !ok {
		t.Fatalf("no pane.open operation %s", u.label)
	}
	intent, ok := op.Intent.(map[string]any)
	if !ok {
		t.Fatalf("pane.open intent = %T, want the committed JSON map", op.Intent)
	}
	return intent
}

// TestUnplacedLaunchEndedRowStaysPending proves every observation short of
// the label-only pair under established server continuity leaves an
// unplaced exec_pending claim ambiguous: nothing settles, the pane.open
// stays unresolved, and the round reports pending. A renamed pane restored
// by a server restart — with a fresh shell, a restored harness, or its
// native restore still deferred — is the shape only the continuity
// conjunct excludes.
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
			// Herdr restores a renamed pane under its new name, with a fresh
			// occupant: the creation label answers nothing, the claimed
			// process is gone, and the pane still exists.
			name: "relabel plus restart: the renamed pane is restored with a fresh shell",
			arrange: func(u *unplacedLaunch) {
				renamePaneAndRestart(u)
				u.tc.Runtime.InspectPaneFn = restoredPane(u, childPaneWith(9191, app.ProcessInfo{PID: 9191, Name: "zsh", Argv: []string{"-zsh"}}))
			},
		},
		{
			name: "relabel plus restart: the renamed pane is restored running the harness's native resume",
			arrange: func(u *unplacedLaunch) {
				renamePaneAndRestart(u)
				u.tc.Runtime.InspectPaneFn = restoredPane(u, childPaneWith(9191, app.ProcessInfo{PID: 9191, Name: "claude", Argv: []string{"claude", "--resume", "native-ref"}}))
			},
		},
		{
			// A pane awaiting a deferred native restore has no runtime yet:
			// every inspection answers not found until the restore fires.
			name: "relabel plus restart: the renamed pane's native restore is still deferred",
			arrange: func(u *unplacedLaunch) {
				renamePaneAndRestart(u)
				u.tc.Runtime.InspectPaneFn = allPanesAbsentPinned
			},
		},
		{
			name: "a restart with no pane answering the label",
			arrange: func(u *unplacedLaunch) {
				delete(u.panes, u.label)
				u.tc.Runtime.ServerInstanceValue = fakeServerToken(2)
			},
		},
		{
			name: "the server lifetime is unknown",
			arrange: func(u *unplacedLaunch) {
				delete(u.panes, u.label)
				u.tc.Runtime.ServerInstanceErr = errors.New("socket gone")
			},
		},
		{
			name: "the launch recorded no server lifetime",
			arrange: func(u *unplacedLaunch) {
				delete(u.panes, u.label)
				recordIntentServer(t, u, "")
			},
		},
		{
			name: "the launch recorded a server identity in an older format",
			arrange: func(u *unplacedLaunch) {
				delete(u.panes, u.label)
				recordIntentServer(t, u, "peer-pid:41001")
			},
		},
		{
			// Each round's label recovery looks the label up first; the row's
			// own lookup is the second of the round.
			name: "the server restarts between the label lookup and the process observation",
			arrange: func(u *unplacedLaunch) {
				delete(u.panes, u.label)
				lookups := 0
				u.tc.Runtime.FindPaneByLabelFn = func(string) (app.PaneRef, bool, error) {
					lookups++
					if lookups == 2 {
						u.tc.Runtime.ServerInstanceValue = fakeServerToken(2)
					}
					return app.PaneRef{}, false, nil
				}
			},
			wantListed: true,
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
					u.sessionID, u.incarnation, "", fakeServerToken(1), "workspace-1", "tab-w", "pane-late", u.label, run.LaunchInitial, u.tc.Clock.Now()))
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

// unplacedContinuityText is the rename-back action an unplaced launch
// reports while its label answers nothing without server continuity.
func unplacedContinuityText(renderedLabel string) string {
	return "no pane answers for launch label " + renderedLabel + ", but server continuity since the launch is not established (the Herdr server may have restarted, and a pane renamed before a restart keeps its new name), so the launch may still run; if a pane of this run was renamed, rename it back to " + renderedLabel + " and a later round adopts it by its label"
}

// TestUnplacedLaunchAfterRestartHasNoAutomaticExit pins the residual the
// continuity conjunct leaves on purpose: an unplaced launch whose creation
// label answers nothing after a server restart — its pane renamed, or
// really gone — is never settled. Stop keeps the run stopping and resume
// keeps it resuming, each naming the rename-back action; a renamed pane
// renamed back is adopted by its label, while a pane really gone has no
// automatic or attested exit yet.
func TestUnplacedLaunchAfterRestartHasNoAutomaticExit(t *testing.T) {
	t.Run("stop stays stopping with the rename-back action, and a pane renamed back is adopted", func(t *testing.T) {
		u := unplacedWorker(t, true)
		renamePaneAndRestart(u)
		u.tc.Runtime.InspectPaneFn = allPanesAbsentPinned
		report := u.driveStop(t, 3)
		if report.Terminated || report.RunState != string(run.RunStopping) {
			t.Fatalf("DriveFeatureStop() = %+v, want still stopping", report)
		}
		want := "session " + u.sessionID.String() + ": " + unplacedContinuityText(u.label) + "; failing closed"
		if !slices.Contains(report.Outstanding, want) {
			t.Fatalf("outstanding = %q, want %q", report.Outstanding, want)
		}
		requireUnplacedChildUnsettled(t, u)
		if len(u.tc.Groups.Listed) != 0 || len(u.tc.Runtime.ClosedPanes) != 0 {
			t.Fatalf("listed %v, closed %v; nothing is observed or closed without continuity", u.tc.Groups.Listed, u.tc.Runtime.ClosedPanes)
		}

		// The human renames the pane back: the next round adopts it by its
		// label under the intent's recorded creation evidence.
		u.panes[u.label] = u.panes["human-renamed"]
		u.driveStop(t, 1)
		history := u.tc.Store.Bindings[u.sessionID]
		if len(history) != 1 || history[0].PaneID != u.paneID || history[0].ServerInstance != fakeServerToken(1) {
			t.Fatalf("bindings = %+v, want the renamed-back pane adopted with the creation-time server identity", history)
		}
	})

	t.Run("resume stays resuming with the rename-back action", func(t *testing.T) {
		f := newResumeFixture(t)
		binding := resumeChildClaim(t, f, app.LaunchClaimExecPending)
		loseChildBinding(t, f)
		f.tc.Runtime.ServerInstanceValue = fakeServerToken(2)
		f.tc.Runtime.InspectPaneFn = liveManagerPane(f, nil)

		result, _ := f.resume(t, "")
		if result.Outcome != "reconciling" {
			t.Fatalf("resume = %+v, want reconciling", result)
		}
		report := sessionReport(t, &result, f.ChildID.String())
		want := "no recorded placement; the launch may still be in flight (" + unplacedContinuityText(binding.CreationLabel) + ")"
		if report.Disposition != app.SessionPending || report.Detail != want {
			t.Fatalf("child report = %+v, want pending with %q", report, want)
		}
		requireRunState(t, f, run.RunResuming)
		if got := f.tc.Store.LaunchClaims[binding.IncarnationID].State; got != app.LaunchClaimExecPending {
			t.Errorf("claim state = %s, want exec_pending", got)
		}
		if op := f.tc.Store.Operations[identity.OperationID(binding.CreationLabel)]; op.State != app.OperationPending {
			t.Errorf("pane.open state = %s, want still pending", op.State)
		}
		if len(f.tc.Groups.Listed) != 0 {
			t.Errorf("listed groups = %v, want none without continuity", f.tc.Groups.Listed)
		}
	})

	t.Run("a hostile creation label renders escaped", func(t *testing.T) {
		const hostile = "label\x1b[2J\nsession forged: stopped \"ok\""
		u := unplacedWorker(t, true)
		delete(u.panes, u.label)
		pendingIntentPayload(t, u)["label"] = hostile
		u.tc.Runtime.ServerInstanceValue = fakeServerToken(2)
		report := u.driveStop(t, 1)
		want := "session " + u.sessionID.String() + ": " + unplacedContinuityText(strconv.Quote(hostile)) + "; failing closed"
		if !slices.Contains(report.Outstanding, want) {
			t.Fatalf("outstanding = %q, want %q", report.Outstanding, want)
		}
		for _, entry := range report.Outstanding {
			if strings.ContainsAny(entry, "\x1b\n") {
				t.Fatalf("outstanding entry %q carries a raw control byte or line break", entry)
			}
		}
	})

	t.Run("a hostile creation label renders escaped in the live-process action too", func(t *testing.T) {
		const hostile = "label\x1b[2J\nsession forged"
		u := unplacedWorker(t, true)
		delete(u.panes, u.label)
		pendingIntentPayload(t, u)["label"] = hostile
		u.tc.Groups.liveLeader(unplacedPID, "/usr/local/bin/claude")
		report := u.driveStop(t, 1)
		want := "session " + u.sessionID.String() + ": no pane answers for launch label " + strconv.Quote(hostile) + " but the claimed launch process (pid 4711) still runs; end that process, and a later round observes its exit; failing closed"
		if !slices.Contains(report.Outstanding, want) {
			t.Fatalf("outstanding = %q, want %q", report.Outstanding, want)
		}
	})
}
