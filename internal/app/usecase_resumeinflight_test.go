package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// inFlightDetail is the resume report detail of a placed child launch the
// controller loop's corroboration finishes.
const inFlightDetail = "launch claim not settled; the placed launch is in flight, and the controller loop corroborates it"

// corroboratingChild is the resume fixture child's harness occupant at the
// claimed pid, carrying the incarnation marker.
func corroboratingChild(incarnation identity.IncarnationID) app.ProcessInfo {
	return app.ProcessInfo{PID: childLaunchPID, Argv: []string{"/usr/local/bin/claude"}, Cmdline: "claude " + incarnation.String()}
}

// withChildPane answers the resume fixture's manager pane with its
// corroborating occupant, paneID with child, and every other pane as
// positively absent.
func withChildPane(f *resumeFixture, paneID string, child app.PaneProcess) func(string) (app.PaneProcess, error) {
	manager := liveManagerPane(f, nil)
	return func(id string) (app.PaneProcess, error) {
		if id == paneID {
			return child, nil
		}
		return manager(id)
	}
}

// childPaneWith is a child pane whose own process is shellPID, with
// members as its foreground group.
func childPaneWith(shellPID int, members ...app.ProcessInfo) app.PaneProcess {
	return app.PaneProcess{ShellPID: shellPID, ForegroundGroupID: shellPID, Foreground: members}
}

// requireRunState asserts the fixture run's stored state.
func requireRunState(t *testing.T, f *resumeFixture, want run.RunState) {
	t.Helper()
	if got := f.tc.Store.Runs[f.fr.RunID].value.State; got != want {
		t.Fatalf("run state = %s, want %s", got, want)
	}
}

// TestResumeFeatureChildLaunchInFlight proves resume's in-flight rule for
// a child launch the lost controller placed but never settled: with the
// placement current, the claim exec_pending or absent, server continuity
// established and the recorded pane answering with the claimed process,
// the run resumes and the controller loop's corroboration finishes the
// launch; every weaker observation keeps the run resuming.
func TestResumeFeatureChildLaunchInFlight(t *testing.T) {
	t.Run("exec_pending with a live corroborating occupant: resumed, and the loop settles it", func(t *testing.T) {
		f := newResumeFixture(t)
		binding := resumeChildClaim(t, f, app.LaunchClaimExecPending)
		f.tc.Runtime.InspectPaneFn = withChildPane(f, binding.PaneID, childPaneWith(childLaunchPID, corroboratingChild(binding.IncarnationID)))

		result, handle := f.resume(t, "")
		if result.Outcome != "resumed" || result.RunState != string(run.RunRunning) {
			t.Fatalf("resume = %+v, want resumed and running", result)
		}
		if report := sessionReport(t, &result, f.ChildID.String()); report.Disposition != app.SessionPending || report.Detail != inFlightDetail {
			t.Fatalf("child report = %+v, want pending in flight", report)
		}
		requireRunState(t, f, run.RunRunning)
		if got := f.tc.Store.LaunchClaims[binding.IncarnationID].State; got != app.LaunchClaimExecPending {
			t.Fatalf("claim state after resume = %s, want exec_pending: resume never settles it", got)
		}

		reports, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		if len(reports) != 1 || reports[0].SessionID != f.ChildID.String() || reports[0].Progress != app.LaunchSettled {
			t.Fatalf("reports = %+v, want the child settled by the loop", reports)
		}
		attemptID := f.tc.Store.Sessions[f.ChildID].value.AttemptID
		if got := f.tc.Store.Attempts[attemptID].value.State; got != run.AttemptRunning {
			t.Errorf("attempt state = %s, want running", got)
		}
		if got := f.tc.Store.Sessions[f.ChildID].value.State; got != run.SessionActive {
			t.Errorf("child session state = %s, want active", got)
		}
	})

	t.Run("a forking-wrapper occupant: resumed, the loop fails it closed, and a later resume stays reconciling", func(t *testing.T) {
		f := newResumeFixture(t)
		binding := resumeChildClaim(t, f, app.LaunchClaimExecPending)
		wrapped := corroboratingChild(binding.IncarnationID)
		wrapper := wrapped
		wrapper.PID = childLaunchPID + 1
		f.tc.Runtime.InspectPaneFn = withChildPane(f, binding.PaneID, childPaneWith(childLaunchPID, wrapped, wrapper))

		result, handle := f.resume(t, "")
		if result.Outcome != "resumed" {
			t.Fatalf("resume = %+v, want resumed", result)
		}
		reports, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
		if err != nil || len(reports) != 1 || reports[0].Progress != app.LaunchNeedsInteraction {
			t.Fatalf("CorroborateSessionLaunches() = %+v, %v; want needs-interaction", reports, err)
		}
		if got := f.tc.Store.Sessions[f.ChildID].value.State; got != run.SessionReconciling {
			t.Fatalf("child session state = %s, want reconciling", got)
		}

		// The loop's controller dies too; the next resume fails closed.
		f.tc.Clock.Advance(leaseTTL + 1)
		again, _, err := f.tc.Controller.ResumeFeature(context.Background(), app.ResumeFeatureRequest{
			RunID: f.fr.RunID.String(), ControllerID: "controller-3", HOPPath: "/usr/local/bin/hop", StateRoot: "/state",
		})
		if err != nil {
			t.Fatalf("second ResumeFeature() error = %v", err)
		}
		if again.Outcome != "reconciling" {
			t.Fatalf("second resume = %+v, want reconciling", again)
		}
		report := sessionReport(t, &again, f.ChildID.String())
		if report.Disposition != app.SessionPending || !strings.Contains(report.Detail, "the session is reconciling, not launching") {
			t.Fatalf("child report = %+v, want pending, not in flight", report)
		}
		requireRunState(t, f, run.RunResuming)
	})

	t.Run("no claim yet with the launcher in its pane: resumed, and the loop settles the later claim", func(t *testing.T) {
		f := newResumeFixture(t)
		binding, _ := f.tc.Store.currentBindingLocked(f.ChildID)
		launcher := app.ProcessInfo{PID: childLaunchPID, Name: "hop", Argv: []string{"/usr/local/bin/hop", "launch", "--run", f.fr.RunID.String(), "--session", f.ChildID.String()}}
		f.tc.Runtime.InspectPaneFn = withChildPane(f, binding.PaneID, childPaneWith(childLaunchPID, launcher))

		result, handle := f.resume(t, "")
		if result.Outcome != "resumed" || result.RunState != string(run.RunRunning) {
			t.Fatalf("resume = %+v, want resumed and running", result)
		}
		if report := sessionReport(t, &result, f.ChildID.String()); report.Detail != inFlightDetail {
			t.Fatalf("child report = %+v, want in flight", report)
		}
		reports, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
		if err != nil || len(reports) != 1 || reports[0].Progress != app.LaunchPending {
			t.Fatalf("CorroborateSessionLaunches() = %+v, %v; want pending before the claim", reports, err)
		}

		// The launcher claims and execs in place.
		resumeChildClaim(t, f, app.LaunchClaimExecPending)
		f.tc.Runtime.InspectPaneFn = withChildPane(f, binding.PaneID, childPaneWith(childLaunchPID, corroboratingChild(binding.IncarnationID)))
		reports, err = f.tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
		if err != nil || len(reports) != 1 || reports[0].Progress != app.LaunchSettled {
			t.Fatalf("CorroborateSessionLaunches() = %+v, %v; want settled", reports, err)
		}
	})

	t.Run("the manager's own launch is unsettled: the run returns to launching, never running", func(t *testing.T) {
		f := newResumeFixture(t)
		mgrRow := f.tc.Store.Sessions[f.fr.ManagerID]
		mgrRow.value.State = run.SessionLaunching
		mgrClaim := f.tc.Store.LaunchClaims[f.fr.ManagerIncarnation]
		mgrClaim.State = app.LaunchClaimExecPending
		f.tc.Store.LaunchClaims[f.fr.ManagerIncarnation] = mgrClaim
		binding := resumeChildClaim(t, f, app.LaunchClaimExecPending)
		f.tc.Runtime.InspectPaneFn = withChildPane(f, binding.PaneID, childPaneWith(childLaunchPID, corroboratingChild(binding.IncarnationID)))

		result, handle := f.resume(t, "")
		if result.Outcome != "resumed" || result.RunState != string(run.RunLaunching) {
			t.Fatalf("resume = %+v, want resumed with the run launching", result)
		}
		requireRunState(t, f, run.RunLaunching)
		if reason, ok := transitionReason(f.tc, app.EntityRun, f.fr.RunID.String(), string(run.RunRunning)); ok {
			t.Fatalf("the run went running during resume (%q) while its manager was unsettled", reason)
		}

		// The loop's launching pass settles both; only the manager's own
		// settlement moves the run to running.
		if _, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), handle); err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		requireRunState(t, f, run.RunRunning)
		if reason, _ := transitionReason(f.tc, app.EntityRun, f.fr.RunID.String(), string(run.RunRunning)); reason != "manager launch claim settled execed" {
			t.Fatalf("run running reason = %q, want the manager's settlement", reason)
		}
	})

	t.Run("the manager is reconciling: the run stays resuming", func(t *testing.T) {
		f := newResumeFixture(t)
		binding := resumeChildClaim(t, f, app.LaunchClaimExecPending)
		child := childPaneWith(childLaunchPID, corroboratingChild(binding.IncarnationID))
		f.tc.Runtime.InspectPaneFn = func(id string) (app.PaneProcess, error) {
			if id == binding.PaneID {
				return child, nil
			}
			return app.PaneProcess{}, app.ErrPaneNotFound
		}

		result, _ := f.resume(t, "")
		if result.Outcome != "reconciling" {
			t.Fatalf("resume = %+v, want reconciling while the manager is unresolved", result)
		}
		if report := sessionReport(t, &result, f.ChildID.String()); report.Detail != inFlightDetail {
			t.Fatalf("child report = %+v, want in flight (the manager alone holds the run)", report)
		}
		requireRunState(t, f, run.RunResuming)
	})

	t.Run("a held stop routes to stop handling before any inspection", func(t *testing.T) {
		f := newResumeFixture(t)
		resumeChildClaim(t, f, app.LaunchClaimExecPending)
		row := f.tc.Store.Runs[f.fr.RunID]
		row.value = row.value.RequestStop(f.tc.Clock.Now())
		row.revision++
		inspected := false
		f.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			inspected = true
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		result, _ := f.resume(t, "")
		if result.Outcome != "stop-pending" || inspected {
			t.Fatalf("resume = %+v (inspected %t), want stop-pending with nothing inspected", result, inspected)
		}
	})

	for _, tt := range []struct {
		name string
		// arrange scripts the observation of the child's placed launch.
		arrange func(f *resumeFixture, binding run.RuntimeBinding)
		detail  string
	}{
		{
			name: "restart: the recorded id answers with a fresh occupant under a changed server instance",
			arrange: func(f *resumeFixture, binding run.RuntimeBinding) {
				f.tc.Runtime.ServerInstanceValue = "peer-pid:2"
				f.tc.Runtime.InspectPaneFn = withChildPane(f, binding.PaneID, childPaneWith(9191, app.ProcessInfo{PID: 9191, Name: "zsh", Argv: []string{"-zsh"}}))
			},
			detail: "server continuity since the placement is not established",
		},
		{
			name: "restart: even the claimed process answering proves nothing once the server changed",
			arrange: func(f *resumeFixture, binding run.RuntimeBinding) {
				f.tc.Runtime.ServerInstanceValue = "peer-pid:2"
				f.tc.Runtime.InspectPaneFn = withChildPane(f, binding.PaneID, childPaneWith(childLaunchPID, corroboratingChild(binding.IncarnationID)))
			},
			detail: "server continuity since the placement is not established",
		},
		{
			name: "an unknown server instance",
			arrange: func(f *resumeFixture, binding run.RuntimeBinding) {
				f.tc.Runtime.ServerInstanceErr = errors.New("socket gone")
				f.tc.Runtime.InspectPaneFn = withChildPane(f, binding.PaneID, childPaneWith(childLaunchPID, corroboratingChild(binding.IncarnationID)))
			},
			detail: "server continuity since the placement is not established",
		},
		{
			name: "the recorded id is gone and the creation label answers",
			arrange: func(f *resumeFixture, binding run.RuntimeBinding) {
				f.tc.Runtime.InspectPaneFn = liveManagerPane(f, nil)
				f.tc.Runtime.FindPaneByLabelFn = func(label string) (app.PaneRef, bool, error) {
					if label == binding.CreationLabel {
						return app.PaneRef{WorkspaceID: binding.WorkspaceID, TabID: "tab-moved", PaneID: "pane-moved"}, true, nil
					}
					return app.PaneRef{}, false, nil
				}
			},
			detail: "a pane still answers for the creation label",
		},
		{
			name: "same server, the pane's own process is not the claimed process",
			arrange: func(f *resumeFixture, binding run.RuntimeBinding) {
				f.tc.Runtime.InspectPaneFn = withChildPane(f, binding.PaneID, childPaneWith(7777, app.ProcessInfo{PID: 7777, Name: "zsh", Argv: []string{"-zsh"}}))
			},
			detail: "the recorded pane's process (pid 7777) is not the claimed launch process (pid 4343)",
		},
		{
			name: "the recorded pane answers with no foreground occupant",
			arrange: func(f *resumeFixture, binding run.RuntimeBinding) {
				f.tc.Runtime.InspectPaneFn = withChildPane(f, binding.PaneID, app.PaneProcess{ShellPID: childLaunchPID})
			},
			detail: "the pane still answers by id with no foreground occupant",
		},
		{
			name: "the placement's pane.open settled failed",
			arrange: func(f *resumeFixture, binding run.RuntimeBinding) {
				op := f.tc.Store.Operations[identity.OperationID(binding.CreationLabel)]
				op.State = app.OperationFailed
				f.tc.Store.Operations[op.ID] = op
				f.tc.Runtime.InspectPaneFn = withChildPane(f, binding.PaneID, childPaneWith(childLaunchPID, corroboratingChild(binding.IncarnationID)))
			},
			detail: "the placement's pane.open operation settled failed",
		},
	} {
		t.Run(tt.name+": the run stays resuming", func(t *testing.T) {
			f := newResumeFixture(t)
			binding := resumeChildClaim(t, f, app.LaunchClaimExecPending)
			tt.arrange(f, binding)

			result, _ := f.resume(t, "")
			if result.Outcome != "reconciling" {
				t.Fatalf("resume = %+v, want reconciling", result)
			}
			report := sessionReport(t, &result, f.ChildID.String())
			if report.Disposition != app.SessionPending || !strings.Contains(report.Detail, tt.detail) {
				t.Fatalf("child report = %+v, want pending naming %q", report, tt.detail)
			}
			requireRunState(t, f, run.RunResuming)
			if got := f.tc.Store.LaunchClaims[binding.IncarnationID].State; got != app.LaunchClaimExecPending {
				t.Errorf("claim state = %s, want exec_pending", got)
			}
			if got := f.tc.Store.Sessions[f.ChildID].value.State; got != run.SessionLaunching {
				t.Errorf("child session state = %s, want launching", got)
			}
		})
	}
}

// loseChildBinding simulates the resume fixture child's lost pane.open
// outcome: the binding is gone and the operation pending again.
func loseChildBinding(t *testing.T, f *resumeFixture) {
	t.Helper()
	binding, ok := f.tc.Store.currentBindingLocked(f.ChildID)
	if !ok {
		t.Fatalf("no binding for the fixture child")
	}
	delete(f.tc.Store.Bindings, f.ChildID)
	op := f.tc.Store.Operations[identity.OperationID(binding.CreationLabel)]
	op.State = app.OperationPending
	op.ActEvidence = nil
	f.tc.Store.Operations[op.ID] = op
}

// TestResumeFeatureChildUnplacedLaunch proves resume resolves a child
// launch whose pane.open outcome the lost controller never recorded: a
// pane answering for the creation label is adopted by it and then decided
// by the in-flight rule; with no pane answering and the claimed process
// gone, the label-only launch-ended row settles and retires it.
func TestResumeFeatureChildUnplacedLaunch(t *testing.T) {
	t.Run("the label answers: adopted, and the placed launch is in flight", func(t *testing.T) {
		f := newResumeFixture(t)
		binding := resumeChildClaim(t, f, app.LaunchClaimExecPending)
		loseChildBinding(t, f)
		f.tc.Runtime.FindPaneByLabelFn = func(label string) (app.PaneRef, bool, error) {
			if label == binding.CreationLabel {
				return app.PaneRef{WorkspaceID: binding.WorkspaceID, TabID: binding.TabID, PaneID: binding.PaneID}, true, nil
			}
			return app.PaneRef{}, false, nil
		}
		f.tc.Runtime.InspectPaneFn = withChildPane(f, binding.PaneID, childPaneWith(childLaunchPID, corroboratingChild(binding.IncarnationID)))

		result, _ := f.resume(t, "")
		if result.Outcome != "resumed" {
			t.Fatalf("resume = %+v, want resumed", result)
		}
		if report := sessionReport(t, &result, f.ChildID.String()); report.Detail != inFlightDetail {
			t.Fatalf("child report = %+v, want in flight once adopted by label", report)
		}
		adopted, ok := f.tc.Store.currentBindingLocked(f.ChildID)
		if !ok || adopted.PaneID != binding.PaneID || adopted.IncarnationID != binding.IncarnationID || adopted.ServerInstance != binding.ServerInstance {
			t.Fatalf("adopted binding = %+v (found %t), want the pane recovered by label with the intent's creation evidence", adopted, ok)
		}
		if op := f.tc.Store.Operations[identity.OperationID(binding.CreationLabel)]; op.State != app.OperationSucceeded {
			t.Fatalf("pane.open state = %s, want succeeded by the adoption", op.State)
		}
	})

	t.Run("no pane answers and the claimed process is gone: settled and retired", func(t *testing.T) {
		f := newResumeFixture(t)
		binding := resumeChildClaim(t, f, app.LaunchClaimExecPending)
		loseChildBinding(t, f)
		f.tc.Runtime.InspectPaneFn = liveManagerPane(f, nil)
		attemptID := f.tc.Store.Sessions[f.ChildID].value.AttemptID

		result, _ := f.resume(t, "")
		if result.Outcome != "resumed" {
			t.Fatalf("resume = %+v, want resumed", result)
		}
		report := sessionReport(t, &result, f.ChildID.String())
		if report.Disposition != app.SessionRetiredNoProcess || report.Detail != launchEndedUnplacedReason {
			t.Fatalf("child report = %+v, want %s with the label-only reason", report, app.SessionRetiredNoProcess)
		}
		requireUnplacedLaunchEnded(t, f.tc, binding.IncarnationID, childLaunchPID)
		if got := f.tc.Store.Attempts[attemptID].value.State; got != run.AttemptFailed {
			t.Errorf("attempt state = %s, want failed", got)
		}
		notices := controllerNoticesTo(f.tc, f.fr.RunID)
		if len(notices) != 1 || string(f.tc.Artifacts.files[notices[0].BodyPath]) != "task t1 needs-rework\nreason: "+launchEndedUnplacedNotice+"\n" {
			t.Fatalf("manager notices = %+v, want the label-only launch-ended notice", notices)
		}
	})

	t.Run("no pane answers and the claimed process runs: pending with the action named", func(t *testing.T) {
		f := newResumeFixture(t)
		binding := resumeChildClaim(t, f, app.LaunchClaimExecPending)
		loseChildBinding(t, f)
		f.tc.Runtime.InspectPaneFn = liveManagerPane(f, nil)
		f.tc.Groups.liveLeader(childLaunchPID, "/usr/local/bin/claude")

		result, _ := f.resume(t, "")
		if result.Outcome != "reconciling" {
			t.Fatalf("resume = %+v, want reconciling", result)
		}
		report := sessionReport(t, &result, f.ChildID.String())
		want := "no recorded placement; the launch may still be in flight (no pane answers for launch label " + binding.CreationLabel + " but the claimed launch process (pid 4343) still runs; end that process, and a later round observes its exit)"
		if report.Disposition != app.SessionPending || report.Detail != want {
			t.Fatalf("child report = %+v, want pending with %q", report, want)
		}
		requireRunState(t, f, run.RunResuming)
	})
}
