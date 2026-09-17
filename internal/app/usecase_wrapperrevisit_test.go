package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// The two transition reasons a reconciling feature session can carry,
// retyped from internal/app's own constants (sessionReconcileResumeAmbiguous
// and TransitionReasonLaunchCorroboration) rather than derived from them, so
// either side drifting fails here.
const (
	liveCorroborationReason = "launch corroboration: another process on the pane carries the launch identity; re-inspected every pass"
	resumeAmbiguousReason   = "resume: evidence ambiguous"
)

// wrapperPane is a foreground group in the shape a same-image child in its
// fork-before-exec window produces (pinned under the production transport by
// test/integration/spike_forkwindow_test.go's
// TestSpikeForkWindowSameImageChild): the pane's own process is the claimed
// one and matches every conjunct of the predicate itself, and one other
// member carries the identical executable identity and marker under a
// different pid.
func wrapperPane(incarnation identity.IncarnationID) app.PaneProcess {
	claimed := corroboratingChild(incarnation)
	forked := claimed
	forked.PID = childLaunchPID + 1
	return childPaneWith(childLaunchPID, claimed, forked)
}

// cleanPane is the same pane one sample later, the forked child having
// exec'd something that carries neither the executable identity nor a
// marker.
func cleanPane(incarnation identity.IncarnationID) app.PaneProcess {
	standIn := app.ProcessInfo{PID: childLaunchPID + 1, Name: "sleep", Argv0: "mcp-stand-in", Argv: []string{"mcp-stand-in", "600"}}
	return childPaneWith(childLaunchPID, corroboratingChild(incarnation), standIn)
}

// wedgeWrappedChildLaunch drives one child launch to the live wrapper
// reconciliation and returns the fixture, the child's binding and the
// loop's handle: resume hands the placed launch to the loop, and the
// loop's first round finds another process carrying the launch identity
// and fails closed.
func wedgeWrappedChildLaunch(t *testing.T) (*resumeFixture, run.RuntimeBinding, app.RunHandle) {
	t.Helper()
	f := newResumeFixture(t)
	binding := resumeChildClaim(t, f, app.LaunchClaimExecPending)
	f.tc.Runtime.InspectPaneFn = withChildPane(f, binding.PaneID, wrapperPane(binding.IncarnationID))
	_, handle := f.resume(t, "")

	reports, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
	if err != nil {
		t.Fatalf("CorroborateSessionLaunches() error = %v", err)
	}
	if len(reports) != 1 || reports[0].SessionID != f.ChildID.String() || reports[0].Progress != app.LaunchNeedsInteraction {
		t.Fatalf("first round = %+v, want the child failing closed with needs-interaction", reports)
	}
	if got := f.tc.Store.Sessions[f.ChildID].value.State; got != run.SessionReconciling {
		t.Fatalf("child session state = %s, want reconciling", got)
	}
	return f, binding, handle
}

// sessionTransitions counts the transitions recorded for one session.
func sessionTransitions(tc *testController, sessionID identity.SessionID) int {
	n := 0
	for _, tr := range tc.Store.Transitions {
		if tr.EntityKind == app.EntitySession && tr.EntityID == sessionID.String() {
			n++
		}
	}
	return n
}

// reconcilingSessionReason is the reason of the newest recorded
// transition INTO reconciling for one session.
func reconcilingSessionReason(t *testing.T, tc *testController, sessionID identity.SessionID) string {
	t.Helper()
	reason, ok := transitionReason(tc, app.EntitySession, sessionID.String(), string(run.SessionReconciling))
	if !ok {
		t.Fatalf("no transition into reconciling was recorded for session %s", sessionID)
	}
	return reason
}

// TestLaunchCorroborationRevisitsItsOwnReconciliation proves the live
// launch corroboration's revisit rule against a child launch whose
// foreground group holds a same-image child in its fork window: the round
// fails closed to reconciling under its OWN value-free reason, and every
// later round re-inspects that session — settling it once one observation
// is clean, writing nothing while the other process is still there, and
// taking the launch-ended row once the pane is gone.
func TestLaunchCorroborationRevisitsItsOwnReconciliation(t *testing.T) {
	t.Run("the live path records its own value-free reason, never resume's", func(t *testing.T) {
		f, _, _ := wedgeWrappedChildLaunch(t)
		if got := reconcilingSessionReason(t, f.tc, f.ChildID); got != liveCorroborationReason {
			t.Fatalf("reconciling reason = %q, want the live corroboration's own %q", got, liveCorroborationReason)
		}
	})

	t.Run("a later clean observation settles the reconciling session", func(t *testing.T) {
		f, binding, handle := wedgeWrappedChildLaunch(t)
		f.tc.Runtime.InspectPaneFn = withChildPane(f, binding.PaneID, cleanPane(binding.IncarnationID))

		reports, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
		if err != nil {
			t.Fatalf("second CorroborateSessionLaunches() error = %v", err)
		}
		if len(reports) != 1 || reports[0].Progress != app.LaunchSettled {
			t.Fatalf("second round = %+v, want the reconciling session settled", reports)
		}
		if got := f.tc.Store.LaunchClaims[binding.IncarnationID].State; got != app.LaunchClaimExeced {
			t.Fatalf("claim state = %s, want execed", got)
		}
		if got := f.tc.Store.Sessions[f.ChildID].value.State; got != run.SessionActive {
			t.Fatalf("child session state = %s, want active", got)
		}
		attemptID := f.tc.Store.Sessions[f.ChildID].value.AttemptID
		if got := f.tc.Store.Attempts[attemptID].value.State; got != run.AttemptRunning {
			t.Fatalf("attempt state = %s, want running", got)
		}
	})

	t.Run("a persistent other process stays reconciling and writes nothing", func(t *testing.T) {
		f, binding, handle := wedgeWrappedChildLaunch(t)
		transitions := sessionTransitions(f.tc, f.ChildID)

		for round := range 3 {
			reports, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
			if err != nil {
				t.Fatalf("round %d: CorroborateSessionLaunches() error = %v", round, err)
			}
			if len(reports) != 1 || reports[0].Progress != app.LaunchNeedsInteraction {
				t.Fatalf("round %d = %+v, want needs-interaction again", round, reports)
			}
		}
		if got := sessionTransitions(f.tc, f.ChildID); got != transitions {
			t.Fatalf("session transitions = %d, want the original %d: a persistent wrapper writes nothing", got, transitions)
		}
		if got := f.tc.Store.LaunchClaims[binding.IncarnationID].State; got != app.LaunchClaimExecPending {
			t.Fatalf("claim state = %s, want exec_pending", got)
		}
		if got := f.tc.Store.Sessions[f.ChildID].value.State; got != run.SessionReconciling {
			t.Fatalf("child session state = %s, want reconciling", got)
		}
	})

	t.Run("a gone pane takes the launch-ended row and the exec-failure row", func(t *testing.T) {
		f, binding, handle := wedgeWrappedChildLaunch(t)
		// The pane is positively gone by id, its creation label answers
		// nothing, the claimed process is not in its group and the server
		// lifetime that placed it still answers: the launch ended.
		f.tc.Runtime.InspectPaneFn = liveManagerPane(f, nil)

		reports, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		if len(reports) != 1 || reports[0].Progress != app.LaunchFailed {
			t.Fatalf("round = %+v, want the launch settled failed", reports)
		}
		if got := f.tc.Store.LaunchClaims[binding.IncarnationID].State; got != app.LaunchClaimExecFailed {
			t.Fatalf("claim state = %s, want exec_failed", got)
		}
		if got := f.tc.Store.Sessions[f.ChildID].value.State; got != run.SessionTerminated {
			t.Fatalf("child session state = %s, want terminated", got)
		}
		attemptID := f.tc.Store.Sessions[f.ChildID].value.AttemptID
		if got := f.tc.Store.Attempts[attemptID].value.State; got != run.AttemptFailed {
			t.Fatalf("attempt state = %s, want failed", got)
		}
	})
}

// TestReconcilingSessionWithASettledClaimIsNeverReInspected pins the
// structural identification from the other side: a session a RESUME path
// marked reconciling holds a settled claim, so no corroboration round
// touches it — not its pane, not its row. This is what makes the live
// reconciliation identifiable without a reason or marker of its own.
func TestReconcilingSessionWithASettledClaimIsNeverReInspected(t *testing.T) {
	f := newResumeFixture(t)
	binding := resumeChildClaim(t, f, app.LaunchClaimExeced)
	// A settled claim whose occupant does not corroborate: resume fails
	// closed to reconciling.
	foreign := app.ProcessInfo{PID: childLaunchPID, Name: "zsh", Argv: []string{"-zsh"}}
	inspected := map[string]int{}
	f.tc.Runtime.InspectPaneFn = func(id string) (app.PaneProcess, error) {
		inspected[id]++
		if id == binding.PaneID {
			return childPaneWith(childLaunchPID, foreign), nil
		}
		return liveManagerPane(f, nil)(id)
	}

	result, handle := f.resume(t, "")
	if report := sessionReport(t, &result, f.ChildID.String()); report.Disposition != app.SessionReconciling {
		t.Fatalf("child report = %+v, want reconciling on ambiguous evidence", report)
	}
	if got := reconcilingSessionReason(t, f.tc, f.ChildID); got != resumeAmbiguousReason {
		t.Fatalf("reconciling reason = %q, want resume's own %q", got, resumeAmbiguousReason)
	}

	transitions := sessionTransitions(f.tc, f.ChildID)
	inspected = map[string]int{}
	reports, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
	if err != nil {
		t.Fatalf("CorroborateSessionLaunches() error = %v", err)
	}
	if len(reports) != 0 {
		t.Fatalf("reports = %+v, want none: a resume-marked reconciling session is not the loop's to inspect", reports)
	}
	if got := inspected[binding.PaneID]; got != 0 {
		t.Fatalf("the child's pane was inspected %d times, want none", got)
	}
	if got := sessionTransitions(f.tc, f.ChildID); got != transitions {
		t.Fatalf("session transitions = %d, want the original %d", got, transitions)
	}
	if got := f.tc.Store.Sessions[f.ChildID].value.State; got != run.SessionReconciling {
		t.Fatalf("child session state = %s, want reconciling, exactly as resume left it", got)
	}
}

// TestNoResumePathProducesAnUnsettledReconcilingSession is the invariant
// markSessionReconciling's doc comment states, driven rather than argued:
// across every resume path that fails closed to reconciling, the session's
// current placement's launch claim is already SETTLED. A new resume path
// that could mark a session reconciling while its claim is still
// exec_pending would make the live reconciliation unidentifiable, and
// fails here.
func TestNoResumePathProducesAnUnsettledReconcilingSession(t *testing.T) {
	for _, tt := range []struct {
		name          string
		confirmAbsent bool
		arrange       func(f *resumeFixture, binding run.RuntimeBinding)
	}{
		{
			name: "the occupant does not corroborate",
			arrange: func(f *resumeFixture, binding run.RuntimeBinding) {
				f.tc.Runtime.InspectPaneFn = withChildPane(f, binding.PaneID,
					childPaneWith(childLaunchPID, app.ProcessInfo{PID: childLaunchPID, Name: "zsh", Argv: []string{"-zsh"}}))
			},
		},
		{
			name: "the occupant is a forking wrapper",
			arrange: func(f *resumeFixture, binding run.RuntimeBinding) {
				f.tc.Runtime.InspectPaneFn = withChildPane(f, binding.PaneID, wrapperPane(binding.IncarnationID))
			},
		},
		{
			name: "the pane is gone but its creation label still answers",
			arrange: func(f *resumeFixture, binding run.RuntimeBinding) {
				f.tc.Runtime.InspectPaneFn = liveManagerPane(f, nil)
				f.tc.Runtime.FindPaneByLabelFn = func(label string) (app.PaneRef, bool, error) {
					if label == binding.CreationLabel {
						return app.PaneRef{WorkspaceID: binding.WorkspaceID, TabID: "tab-moved", PaneID: "pane-moved"}, true, nil
					}
					return app.PaneRef{}, false, nil
				}
			},
		},
		{
			name: "the pane is absent and no attestation was given",
			arrange: func(f *resumeFixture, _ run.RuntimeBinding) {
				f.tc.Runtime.InspectPaneFn = liveManagerPane(f, nil)
			},
		},
		{
			name:          "the pane is absent, attested, and the server restarted",
			confirmAbsent: true,
			arrange: func(f *resumeFixture, _ run.RuntimeBinding) {
				f.tc.Runtime.InspectPaneFn = liveManagerPane(f, nil)
				f.tc.Runtime.ServerInstanceValue = fakeServerToken(2)
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newResumeFixture(t)
			// A settled claim is what every one of these paths reads; the
			// assertion below is that none of them can be reached with an
			// unsettled one.
			binding := resumeChildClaim(t, f, app.LaunchClaimExeced)
			tt.arrange(f, binding)
			confirm := ""
			if tt.confirmAbsent {
				confirm = f.ChildID.String()
			}

			result, _ := f.resume(t, confirm)
			if report := sessionReport(t, &result, f.ChildID.String()); report.Disposition != app.SessionReconciling {
				t.Fatalf("child report = %+v, want reconciling", report)
			}
			for id, row := range f.tc.Store.Sessions {
				if row.value.State != run.SessionReconciling {
					continue
				}
				current, ok := f.tc.Store.currentBindingLocked(id)
				if !ok || current.PaneID == "" {
					continue
				}
				claim, claimFound := f.tc.Store.LaunchClaims[current.IncarnationID]
				if claimFound && claim.State == app.LaunchClaimExecPending {
					t.Fatalf("session %s is reconciling with an exec_pending claim under a current placement: a resume path produced the one shape that identifies the LIVE launch corroboration's reconciliation", id)
				}
			}
		})
	}
}

// TestColdRelaunchLeavesAReconcilingSessionLost proves the cold-relaunch
// path is untouched by the revisit rule: a session resume left reconciling
// is attested absent and goes reconciling -> lost with its binding
// superseded, and no corroboration round ever re-inspects it — only its
// successor's fresh launch.
func TestColdRelaunchLeavesAReconcilingSessionLost(t *testing.T) {
	f := newResumeFixture(t)
	inspected := map[string]int{}
	f.tc.Runtime.InspectPaneFn = func(id string) (app.PaneProcess, error) {
		inspected[id]++
		return app.PaneProcess{}, app.ErrPaneNotFound
	}

	// Round one: absent with no attestation, so the manager stays
	// reconciling with a settled claim.
	first, handle := f.resume(t, "")
	if mgr := sessionReport(t, &first, f.fr.ManagerID.String()); mgr.Disposition != app.SessionReconciling {
		t.Fatalf("manager report = %+v, want reconciling without an attestation", mgr)
	}
	managerPane, ok := f.tc.Store.currentBindingLocked(f.fr.ManagerID)
	if !ok {
		t.Fatalf("no binding for the fixture manager %s", f.fr.ManagerID)
	}
	inspected = map[string]int{}
	if _, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), handle); err != nil {
		t.Fatalf("CorroborateSessionLaunches() error = %v", err)
	}
	if got := inspected[managerPane.PaneID]; got != 0 {
		t.Fatalf("the reconciling manager's pane was inspected %d times, want none", got)
	}

	// Round two, a fresh controller: the attestation relaunches the
	// lineage, and the reconciling predecessor is lost.
	f.tc.Clock.Advance(leaseTTL + 1)
	second, _, err := f.tc.Controller.ResumeFeature(context.Background(), app.ResumeFeatureRequest{
		RunID: f.fr.RunID.String(), ControllerID: "controller-3",
		ConfirmAbsentSession: f.fr.ManagerID.String(),
		HOPPath:              "/usr/local/bin/hop", StateRoot: "/state",
	})
	if err != nil {
		t.Fatalf("second ResumeFeature() error = %v", err)
	}
	if mgr := sessionReport(t, &second, f.fr.ManagerID.String()); mgr.Disposition != app.SessionRelaunched {
		t.Fatalf("manager report = %+v, want relaunched", mgr)
	}
	if got := f.tc.Store.Sessions[f.fr.ManagerID].value.State; got != run.SessionLost {
		t.Fatalf("prior manager state = %s, want lost", got)
	}
	reason, found := transitionReason(f.tc, app.EntitySession, f.fr.ManagerID.String(), string(run.SessionLost))
	if !found || !strings.Contains(reason, "cold relaunch authorized") {
		t.Fatalf("lost transition reason = %q (found %t), want the attested cold relaunch", reason, found)
	}
	if current, stillCurrent := f.tc.Store.currentBindingLocked(f.fr.ManagerID); stillCurrent && current.PaneID == managerPane.PaneID {
		t.Fatalf("the lost manager still has its placement as a current binding (%s); it must be superseded", current.PaneID)
	}
}

// wedgeWrappedManagerLaunch puts the resume fixture's MANAGER into the
// live wrapper reconciliation — its placement current, its claim that
// placement's own exec_pending one, the session reconciling — and returns
// its binding. This is the one reconciling state the controller loop
// re-inspects, so the bootstrap continuation hands it back to the loop
// rather than holding the run in reconciliation.
func wedgeWrappedManagerLaunch(t *testing.T, f *resumeFixture) run.RuntimeBinding {
	t.Helper()
	binding, ok := f.tc.Store.currentBindingLocked(f.fr.ManagerID)
	if !ok {
		t.Fatalf("no binding for the fixture manager %s", f.fr.ManagerID)
	}
	claim := f.tc.Store.LaunchClaims[f.fr.ManagerIncarnation]
	claim.State = app.LaunchClaimExecPending
	claim.PID = f.ManagerPID
	f.tc.Store.LaunchClaims[f.fr.ManagerIncarnation] = claim

	row := f.tc.Store.Sessions[f.fr.ManagerID]
	reconciling, err := row.value.Reconcile(f.tc.Clock.Now())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	row.value = reconciling
	row.revision++
	return binding
}

// claimedManagerPane is the manager's pane answering with the claimed
// process as its own: the launch the loop still corroborates.
func claimedManagerPane(f *resumeFixture) app.PaneProcess {
	return app.PaneProcess{
		ShellPID: f.ManagerPID, ForegroundGroupID: f.ManagerPID,
		Foreground: []app.ProcessInfo{{PID: f.ManagerPID, Argv: []string{"/usr/local/bin/claude"}, Cmdline: "claude " + f.fr.ManagerIncarnation.String()}},
	}
}

// TestManagerBootstrapContinuesTheWrapperReconciliation is the manager arm
// of resume parity. The bootstrap continuation returns a run to launching
// for a manager in the live wrapper reconciliation — the same state the
// loop re-inspects — but ONLY under the positive observations resume's
// in-flight rule requires of a child's placed launch: the inspection
// answered by the lifetime the placement recorded, and the pane's own
// process being the claimed one. Under anything weaker the run stays
// resuming and nothing is handed back.
func TestManagerBootstrapContinuesTheWrapperReconciliation(t *testing.T) {
	for _, tt := range []struct {
		name string
		// arrange scripts the manager pane's observation.
		arrange func(f *resumeFixture) app.PaneProcess
		want    run.RunState
	}{
		{
			name:    "the claimed process answers under the placement's own lifetime",
			arrange: claimedManagerPane,
			want:    run.RunLaunching,
		},
		{
			name: "the server restarted, so the inspection is no evidence of the launch",
			arrange: func(f *resumeFixture) app.PaneProcess {
				f.tc.Runtime.ServerInstanceValue = fakeServerToken(2)
				return claimedManagerPane(f)
			},
			want: run.RunResuming,
		},
		{
			name: "the pane's own process is not the claimed launch process",
			arrange: func(*resumeFixture) app.PaneProcess {
				return app.PaneProcess{
					ShellPID: 7777, ForegroundGroupID: 7777,
					Foreground: []app.ProcessInfo{{PID: 7777, Name: "zsh", Argv: []string{"-zsh"}}},
				}
			},
			want: run.RunResuming,
		},
		{
			name: "the recorded pane answers with no foreground occupant",
			arrange: func(f *resumeFixture) app.PaneProcess {
				return app.PaneProcess{ShellPID: f.ManagerPID, ForegroundGroupID: f.ManagerPID}
			},
			want: run.RunResuming,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newResumeFixture(t)
			managerBinding := wedgeWrappedManagerLaunch(t, f)
			childBinding := resumeChildClaim(t, f, app.LaunchClaimExecPending)
			managerPane := tt.arrange(f)
			f.tc.Runtime.InspectPaneFn = func(id string) (app.PaneProcess, error) {
				switch id {
				case managerBinding.PaneID:
					return managerPane, nil
				case childBinding.PaneID:
					return childPaneWith(childLaunchPID, corroboratingChild(childBinding.IncarnationID)), nil
				}
				return app.PaneProcess{}, app.ErrPaneNotFound
			}

			f.resume(t, "")
			requireRunState(t, f, tt.want)
			// Whatever the observation, resume never settles the manager's
			// claim itself: that stays the loop's to do.
			if got := f.tc.Store.LaunchClaims[f.fr.ManagerIncarnation].State; got != app.LaunchClaimExecPending {
				t.Fatalf("manager claim state = %s, want exec_pending", got)
			}
			if got := f.tc.Store.Sessions[f.fr.ManagerID].value.State; got != run.SessionReconciling {
				t.Fatalf("manager session state = %s, want reconciling", got)
			}
		})
	}
}

// TestManagerBootstrapIgnoresAReconciliationWithASettledClaim is the other
// side of the manager arm, where wrapperReconciliation is the
// DISCRIMINATOR rather than a passenger: every row of the table above
// holds the claim at exec_pending, so the conjunct is true throughout and
// deleting it changes nothing there.
//
// A settled claim under a reconciling session is the shape EVERY resume
// path leaves behind, and the loop does not re-inspect it
// (sessionUnderLaunchCorroboration skips it for the same reason). So the
// bootstrap must not hand the run back to launching for it, even when the
// pane answers with the claimed process under the placement's own
// lifetime — every positive observation LAUNCH-5 requires. Handing that
// session to a loop that will never look at it is the wedge this slice
// exists to prevent, arriving through a different door.
func TestManagerBootstrapIgnoresAReconciliationWithASettledClaim(t *testing.T) {
	f := newResumeFixture(t)
	managerBinding := wedgeWrappedManagerLaunch(t, f)
	claim := f.tc.Store.LaunchClaims[f.fr.ManagerIncarnation]
	claim.State = app.LaunchClaimExeced
	f.tc.Store.LaunchClaims[f.fr.ManagerIncarnation] = claim

	childBinding := resumeChildClaim(t, f, app.LaunchClaimExecPending)
	managerPane := claimedManagerPane(f)
	f.tc.Runtime.InspectPaneFn = func(id string) (app.PaneProcess, error) {
		switch id {
		case managerBinding.PaneID:
			return managerPane, nil
		case childBinding.PaneID:
			return childPaneWith(childLaunchPID, corroboratingChild(childBinding.IncarnationID)), nil
		}
		return app.PaneProcess{}, app.ErrPaneNotFound
	}

	result, _ := f.resume(t, "")
	if got := f.tc.Store.Runs[f.fr.RunID].value.State; got == run.RunLaunching {
		t.Fatalf("run state = %s: the bootstrap returned the run to launching for a reconciliation whose claim is already settled", got)
	}
	// What happens instead is the ordinary resume: the settled claim's own
	// occupant corroborates, so the manager warm-reattaches and the run
	// resumes to running. The launch was never the loop's to finish.
	if mgr := sessionReport(t, &result, f.fr.ManagerID.String()); mgr.Disposition != app.SessionWarm {
		t.Fatalf("manager report = %+v, want warm: its settled claim's occupant corroborates here, not in the loop", mgr)
	}
	if got := f.tc.Store.Sessions[f.fr.ManagerID].value.State; got != run.SessionActive {
		t.Fatalf("manager session state = %s, want active", got)
	}
	requireRunState(t, f, run.RunRunning)
	if result.Outcome != "resumed" {
		t.Fatalf("resume = %+v, want resumed", result)
	}
}
