package app_test

import (
	"context"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/run"
)

// statusSessionView finds one session's row in a rendered run detail.
func statusSessionView(t *testing.T, detail *app.RunDetailView, sessionID string) app.SessionView {
	t.Helper()
	if detail == nil {
		t.Fatalf("hop status rendered no run detail")
	}
	for _, s := range detail.Sessions {
		if s.SessionID == sessionID {
			return s
		}
	}
	t.Fatalf("session %s is not in the rendered detail %+v", sessionID, detail.Sessions)
	return app.SessionView{}
}

// renderedSessions renders hop status's detail for the fixture run.
func renderedSessions(t *testing.T, f *resumeFixture) *app.RunDetailView {
	t.Helper()
	status, err := f.tc.Controller.Status(context.Background(), app.StatusRequest{RunID: f.fr.RunID.String()})
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	return status.Detail
}

// TestStatusRendersTheLaunchCorroborationAction pins the condition `hop
// status` renders a session's own action line under, through the view the
// command renders from: it holds while the live launch corroboration is
// still failing closed on that session, and only then. Once the launch
// settles it is gone, and a session a RESUME path left reconciling — whose
// claim is already settled — never carries it, since nothing re-inspects
// that one and no wrapper is what put it there.
func TestStatusRendersTheLaunchCorroborationAction(t *testing.T) {
	t.Run("a wrapped launch renders it, and settling clears it", func(t *testing.T) {
		f, binding, handle := wedgeWrappedChildLaunch(t)
		child := statusSessionView(t, renderedSessions(t, f), f.ChildID.String())
		if child.State != string(run.SessionReconciling) || !child.LaunchCorroborationPending {
			t.Fatalf("session view = %+v, want reconciling with its launch corroboration still pending", child)
		}

		f.tc.Runtime.InspectPaneFn = withChildPane(f, binding.PaneID, cleanPane(binding.IncarnationID))
		if _, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), handle); err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		child = statusSessionView(t, renderedSessions(t, f), f.ChildID.String())
		if child.State != string(run.SessionActive) || child.LaunchCorroborationPending {
			t.Fatalf("session view after the settlement = %+v, want active with no action owed", child)
		}
	})

	t.Run("a resume-marked reconciling session never renders it", func(t *testing.T) {
		f := newResumeFixture(t)
		binding := resumeChildClaim(t, f, app.LaunchClaimExeced)
		f.tc.Runtime.InspectPaneFn = withChildPane(f, binding.PaneID,
			childPaneWith(childLaunchPID, app.ProcessInfo{PID: childLaunchPID, Name: "zsh", Argv: []string{"-zsh"}}))

		result, _ := f.resume(t, "")
		if report := sessionReport(t, &result, f.ChildID.String()); report.Disposition != app.SessionReconciling {
			t.Fatalf("child report = %+v, want reconciling on ambiguous evidence", report)
		}
		child := statusSessionView(t, renderedSessions(t, f), f.ChildID.String())
		if child.State != string(run.SessionReconciling) {
			t.Fatalf("session view = %+v, want reconciling", child)
		}
		if child.LaunchCorroborationPending {
			t.Fatalf("session view = %+v, want no action owed: its claim is already settled, and nothing re-inspects it", child)
		}
	})
}
