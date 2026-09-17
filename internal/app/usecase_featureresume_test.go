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

// resumeFixture is a feature run whose controller has died: the lease is
// expired, the manager holds a settled claim, and a child session
// records its historical parent.
type resumeFixture struct {
	tc         *testController
	fr         featureRun
	ManagerPID int
	ChildID    identity.SessionID
}

func newResumeFixture(t *testing.T) *resumeFixture {
	t.Helper()
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	tc.Store.repoByRoot["/repo"] = tc.Store.Runs[fr.RunID].value.RepositoryID
	now := tc.Clock.Now()

	// The manager carries its native reference (a cold relaunch cannot be
	// rendered without it) and a settled launch claim.
	mgrRow := tc.Store.Sessions[fr.ManagerID]
	withRef, err := mgrRow.value.AssignNativeRef("manager-native-ref", run.NativeRefAssigned, now)
	if err != nil {
		t.Fatalf("AssignNativeRef() error = %v", err)
	}
	mgrRow.value = withRef
	mgrRow.revision++
	tc.Store.LaunchClaims[fr.ManagerIncarnation] = app.LaunchClaim{
		IncarnationID: fr.ManagerIncarnation, RunID: fr.RunID, SessionID: fr.ManagerID,
		Executable: "/usr/local/bin/claude", ArgvDigest: "d", PID: 900,
		State: app.LaunchClaimExeced, ClaimedAt: now,
	}

	// A child implementer session bound to the manager that was current
	// at its creation.
	taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
	childID, _ := seedWorkerSession(t, tc, fr, taskID)

	// The controller died: its lease expires.
	tc.Clock.Advance(leaseTTL + 1)
	return &resumeFixture{tc: tc, fr: fr, ManagerPID: 900, ChildID: childID}
}

// resume runs ResumeFeature as a fresh controller.
func (f *resumeFixture) resume(t *testing.T, confirmAbsent string) (app.ResumeFeatureResult, app.RunHandle) {
	t.Helper()
	result, handle, err := f.tc.Controller.ResumeFeature(context.Background(), app.ResumeFeatureRequest{
		RunID: f.fr.RunID.String(), ControllerID: "controller-2",
		ConfirmAbsentSession: confirmAbsent,
		HOPPath:              "/usr/local/bin/hop", StateRoot: "/state",
	})
	if err != nil {
		t.Fatalf("ResumeFeature() error = %v", err)
	}
	return result, handle
}

// sessionReport finds one session's disposition in the result.
func sessionReport(t *testing.T, result *app.ResumeFeatureResult, sessionID string) app.FeatureSessionReport {
	t.Helper()
	for _, s := range result.Sessions {
		if s.SessionID == sessionID {
			return s
		}
	}
	t.Fatalf("session %s not in the resume report %+v", sessionID, result.Sessions)
	return app.FeatureSessionReport{}
}

// attestationOps lists the run's absence.attested journal entries.
func attestationOps(tc *testController, runID identity.RunID) []app.Operation {
	var out []app.Operation
	for _, op := range tc.Store.Operations { //nolint:gocritic // rangeValCopy: test helper over a small map.
		if op.RunID == runID && op.Kind == app.OpAbsenceAttested {
			out = append(out, op)
		}
	}
	return out
}

func TestResumeFeature(t *testing.T) {
	t.Run("warm reattach: the occupant corroborates under the one predicate", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			// childPane is what the pre-claim child's recorded pane answers.
			childPane func(f *resumeFixture) (app.PaneProcess, error)
			outcome   string
			detail    string
		}{
			{
				// The child is still pre-claim with its launcher in its pane:
				// its placed launch is in flight, so the run resumes and the
				// loop's corroboration waits for the claim.
				name: "a pre-claim child's launcher occupies its pane: resumed",
				childPane: func(f *resumeFixture) (app.PaneProcess, error) {
					return app.PaneProcess{ShellPID: 5151, ForegroundGroupID: 5151, Foreground: []app.ProcessInfo{{
						PID: 5151, Name: "hop", Argv: []string{"/usr/local/bin/hop", "launch", "--run", f.fr.RunID.String(), "--session", f.ChildID.String()},
					}}}, nil
				},
				outcome: "resumed",
				detail:  "launch claim not settled; the placed launch is in flight, and the controller loop corroborates it",
			},
			{
				// A pre-claim child whose recorded pane no longer answers is
				// not in flight: the run stays in reconciliation.
				name:      "a pre-claim child's recorded pane is gone: reconciling",
				childPane: func(*resumeFixture) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound },
				outcome:   "reconciling",
				detail:    "launch claim not settled; corroboration continues (the recorded pane does not answer by id)",
			},
		} {
			t.Run(tt.name, func(t *testing.T) {
				f := newResumeFixture(t)
				f.tc.Runtime.InspectPaneFn = func(paneID string) (app.PaneProcess, error) {
					if paneID == "pane-mgr" {
						return app.PaneProcess{
							ShellPID: 1, ForegroundGroupID: f.ManagerPID,
							Foreground: []app.ProcessInfo{{PID: f.ManagerPID, Argv: []string{"/usr/local/bin/claude"}, Cmdline: "claude " + f.fr.ManagerIncarnation.String()}},
						}, nil
					}
					return tt.childPane(f)
				}
				result, _ := f.resume(t, "")
				mgr := sessionReport(t, &result, f.fr.ManagerID.String())
				if mgr.Disposition != app.SessionWarm {
					t.Fatalf("manager disposition = %+v, want warm", mgr)
				}
				child := sessionReport(t, &result, f.ChildID.String())
				if child.Disposition != app.SessionPending || child.Detail != tt.detail {
					t.Fatalf("child report = %+v, want pending with %q", child, tt.detail)
				}
				if result.Outcome != tt.outcome {
					t.Fatalf("outcome = %s, want %s", result.Outcome, tt.outcome)
				}
				if got := f.tc.Store.Sessions[f.fr.ManagerID].value.State; got != run.SessionActive {
					t.Fatalf("manager state = %s, want active", got)
				}
			})
		}
	})

	t.Run("a second concurrent resume loses the lease CAS", func(t *testing.T) {
		f := newResumeFixture(t)
		f.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound }
		f.resume(t, "")
		_, _, err := f.tc.Controller.ResumeFeature(context.Background(), app.ResumeFeatureRequest{
			RunID: f.fr.RunID.String(), ControllerID: "controller-3",
		})
		if !errors.Is(err, app.ErrLeaseHeld) {
			t.Fatalf("second resume error = %v, want ErrLeaseHeld", err)
		}
	})

	t.Run("a held stop routes to stop handling before any adoption", func(t *testing.T) {
		f := newResumeFixture(t)
		row := f.tc.Store.Runs[f.fr.RunID]
		row.value = row.value.RequestStop(f.tc.Clock.Now())
		row.revision++
		inspected := false
		f.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			inspected = true
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		result, _ := f.resume(t, "")
		if result.Outcome != "stop-pending" {
			t.Fatalf("outcome = %s, want stop-pending", result.Outcome)
		}
		if inspected {
			t.Fatalf("a session was inspected under a held stop; no adoption or new dispatch happens before stop handling")
		}
	})

	t.Run("pane absent without attestation stays reconciling", func(t *testing.T) {
		f := newResumeFixture(t)
		f.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound }
		result, _ := f.resume(t, "")
		mgr := sessionReport(t, &result, f.fr.ManagerID.String())
		if mgr.Disposition != app.SessionReconciling {
			t.Fatalf("manager disposition = %+v, want reconciling without --confirm-absent", mgr)
		}
		if !strings.Contains(mgr.Detail, "--confirm-absent "+f.fr.ManagerID.String()) {
			t.Fatalf("detail %q does not name the per-session flag", mgr.Detail)
		}
		if got := len(attestationOps(f.tc, f.fr.RunID)); got != 0 {
			t.Fatalf("attestation entries = %d, want none without the flag", got)
		}
	})

	t.Run("per-session attestation with continuity cold-relaunches the manager lineage", func(t *testing.T) {
		f := newResumeFixture(t)
		f.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound }
		result, _ := f.resume(t, f.fr.ManagerID.String())
		mgr := sessionReport(t, &result, f.fr.ManagerID.String())
		if mgr.Disposition != app.SessionRelaunched {
			t.Fatalf("manager disposition = %+v, want relaunched", mgr)
		}

		// The attestation is journaled per session with its continuity
		// evidence.
		attested := attestationOps(f.tc, f.fr.RunID)
		if len(attested) != 1 {
			t.Fatalf("attestation entries = %d, want exactly one", len(attested))
		}
		outcome := attested[0].Outcome
		if payloadField(t, outcome, "session_id") != f.fr.ManagerID.String() {
			t.Fatalf("attestation session = %v, want the manager", outcome)
		}
		for _, field := range []string{"attested_not_restored", "attested_no_other_client", "pane_absent_by_id", "pane_absent_by_label"} {
			if payloadField(t, outcome, field) != "true" {
				t.Fatalf("attestation %s = %v, want true", field, outcome)
			}
		}
		if payloadField(t, outcome, "recorded_server_instance") != fakeServerToken(1) || payloadField(t, outcome, "observed_server_instance") != fakeServerToken(1) {
			t.Fatalf("attestation continuity evidence = %v, want both instance tokens recorded", outcome)
		}

		// The predecessor is terminal; the successor is the run's sole
		// current manager, bound to the SAME native reference.
		if got := f.tc.Store.Sessions[f.fr.ManagerID].value.State; got != run.SessionLost {
			t.Fatalf("prior manager state = %s, want lost", got)
		}
		var successor run.Session
		successorFound := false
		for id, row := range f.tc.Store.Sessions {
			if id == f.fr.ManagerID || row.value.Role != run.RoleManager {
				continue
			}
			successor = row.value
			successorFound = true
		}
		if !successorFound {
			t.Fatalf("no successor manager session")
		}
		if successor.NativeSessionRef != "manager-native-ref" {
			t.Fatalf("successor native ref = %q, want the SAME reference (the conversation continues)", successor.NativeSessionRef)
		}
		if successor.State != run.SessionLaunching {
			t.Fatalf("successor state = %s, want launching", successor.State)
		}

		// Historical parents are preserved: the child still names the
		// manager session that was current AT ITS CREATION.
		child := f.tc.Store.Sessions[f.ChildID].value
		if child.ParentSessionID == nil || *child.ParentSessionID != f.fr.ManagerID {
			t.Fatalf("child parent = %v, want the historical manager %s", child.ParentSessionID, f.fr.ManagerID)
		}

		// The relaunch pane was opened with a fresh incarnation, the
		// manager role env and the recorded workspace.
		lastCall := f.tc.Runtime.ClosedPanes // unused; keep the runtime assertions on the binding instead.
		_ = lastCall
		binding, ok := f.tc.Store.currentBindingLocked(successor.ID)
		if !ok {
			t.Fatalf("no binding recorded for the successor")
		}
		if binding.LaunchKind != run.LaunchResume {
			t.Fatalf("successor binding kind = %s, want resume", binding.LaunchKind)
		}
		if binding.IncarnationID == f.fr.ManagerIncarnation {
			t.Fatalf("successor reused the prior incarnation; the UNIQUE(session, incarnation) key is never reused")
		}

		// The stale manager's verbs fail the ordinary incarnation-currency
		// checks; nothing special is added for it.
		created, err := f.tc.Store.CreateTask(context.Background(), app.TaskCreate{
			ID: mintTaskID(t, f.tc), RunID: f.fr.RunID, Session: f.fr.ManagerID,
			IncarnationID: f.fr.ManagerIncarnation, Title: "stale", InstructionsDigest: "d", RequestID: "stale-1",
		})
		if err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if created.Outcome == app.WorkflowAccepted {
			t.Fatalf("a retired manager incarnation's verb was accepted")
		}
	})

	t.Run("a changed server instance refuses the relaunch after journaling the attestation", func(t *testing.T) {
		f := newResumeFixture(t)
		f.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound }
		f.tc.Runtime.ServerInstanceValue = fakeServerToken(2)
		result, _ := f.resume(t, f.fr.ManagerID.String())
		mgr := sessionReport(t, &result, f.fr.ManagerID.String())
		if mgr.Disposition != app.SessionReconciling {
			t.Fatalf("manager disposition = %+v, want reconciling on a changed instance", mgr)
		}
		if got := len(attestationOps(f.tc, f.fr.RunID)); got != 1 {
			t.Fatalf("attestation entries = %d, want the attestation journaled even when refused", got)
		}
		if got := f.tc.Store.Sessions[f.fr.ManagerID].value.State; got == run.SessionLost || got == run.SessionTerminated {
			t.Fatalf("manager state = %s; a refused attestation must not retire the session", got)
		}
	})

	t.Run("an unknown server instance refuses the relaunch", func(t *testing.T) {
		f := newResumeFixture(t)
		f.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound }
		f.tc.Runtime.ServerInstanceErr = errors.New("socket gone")
		result, _ := f.resume(t, f.fr.ManagerID.String())
		mgr := sessionReport(t, &result, f.fr.ManagerID.String())
		if mgr.Disposition != app.SessionReconciling {
			t.Fatalf("manager disposition = %+v, want reconciling on unknown continuity", mgr)
		}
	})

	t.Run("cold resume is Claude-only", func(t *testing.T) {
		f := newResumeFixture(t)
		mgrRow := f.tc.Store.Sessions[f.fr.ManagerID]
		mgrRow.value.Harness = run.HarnessCodex
		f.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound }
		result, _ := f.resume(t, f.fr.ManagerID.String())
		mgr := sessionReport(t, &result, f.fr.ManagerID.String())
		if mgr.Disposition != app.SessionRelaunchUnsupported {
			t.Fatalf("manager disposition = %+v, want relaunch-unsupported for a non-Claude harness", mgr)
		}
	})
}
