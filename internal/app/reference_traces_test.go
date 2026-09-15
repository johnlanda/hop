package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// entityStates snapshots the four entities a reference trace asserts.
type entityStates struct {
	Run     run.RunState
	Task    run.TaskState
	Attempt run.AttemptState
	Session run.SessionState
}

// states reads the current four-entity snapshot for the run, using the
// given session id (the traces distinguish the original session from a
// cold relaunch's replacement).
func states(t *testing.T, tc *testController, runID identity.RunID, sessionID identity.SessionID) entityStates {
	t.Helper()
	return statesCtx(context.Background(), t, tc, runID, sessionID)
}

func statesCtx(ctx context.Context, t *testing.T, tc *testController, runID identity.RunID, sessionID identity.SessionID) entityStates {
	t.Helper()
	detail, err := tc.Store.LoadRunStatus(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRunStatus() error = %v", err)
	}
	sessionRow, ok := tc.Store.Sessions[sessionID]
	if !ok {
		t.Fatalf("session %s not found", sessionID)
	}
	return entityStates{Run: detail.State, Task: detail.TaskState, Attempt: detail.AttemptState, Session: sessionRow.value.State}
}

// expect asserts one trace step's four-entity snapshot.
func expect(t *testing.T, step string, got, want entityStates) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: Run/Task/Attempt/Session = %s/%s/%s/%s, want %s/%s/%s/%s",
			step, got.Run, got.Task, got.Attempt, got.Session, want.Run, want.Task, want.Attempt, want.Session)
	}
}

// TestReferenceTraceSubmitBeforeRunning is section 5 reference trace 1: a
// fixture worker (name never detected) submits before running. The
// transient rejection changes nothing; the claim settles via the
// inspection predicate; the resubmission lands the acceptance; the check
// claim moves the run to completing.
func TestReferenceTraceSubmitBeforeRunning(t *testing.T) {
	tc := newTestController(defaultPolicy())
	handle, detail := startedRun(t, tc)
	expect(t, "intent", states(t, tc, detail.RunID, detail.SessionID),
		entityStates{run.RunLaunching, run.TaskActive, run.AttemptLaunching, run.SessionLaunching})

	claimLaunch(t, tc, detail, 4242)
	expect(t, "claim exec_pending", states(t, tc, detail.RunID, detail.SessionID),
		entityStates{run.RunLaunching, run.TaskActive, run.AttemptLaunching, run.SessionLaunching})

	// Early submission with a still-unsettled claim: transient, no
	// transitions.
	if result, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil || result.Kind != string(app.SubmissionTransient) {
		t.Fatalf("SubmitResult() = %+v, err %v; want transient", result, err)
	}
	expect(t, "transient submission", states(t, tc, detail.RunID, detail.SessionID),
		entityStates{run.RunLaunching, run.TaskActive, run.AttemptLaunching, run.SessionLaunching})

	// The claim settles through the inspection predicate.
	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "claude", Name: "claude", Argv: []string{"/usr/bin/claude", detail.AttemptID.String()}}}}, nil
	}
	if progress, err := tc.Controller.CorroborateLaunch(context.Background(), handle); err != nil || progress != app.LaunchSettled {
		t.Fatalf("CorroborateLaunch() = %s, err %v; want settled", progress, err)
	}
	expect(t, "claim settled execed", states(t, tc, detail.RunID, detail.SessionID),
		entityStates{run.RunRunning, run.TaskActive, run.AttemptRunning, run.SessionActive})

	// The retried submission is accepted: Run stays running (idempotent).
	if result, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil || result.Kind != string(app.SubmissionAccepted) {
		t.Fatalf("SubmitResult() = %+v, err %v; want accepted", result, err)
	}
	expect(t, "resubmission accepted", states(t, tc, detail.RunID, detail.SessionID),
		entityStates{run.RunRunning, run.TaskChecking, run.AttemptSubmitted, run.SessionActive})

	// The check claim moves Run to completing and Attempt to checking; the
	// hook observes the claimed states while the execution runs.
	var during entityStates
	tc.Commands.CheckExecFn = func(ctx context.Context, _ app.Command) (app.CommandResult, error) {
		during = statesCtx(ctx, t, tc, detail.RunID, detail.SessionID)
		return app.CommandResult{ExitCode: 0}, nil
	}
	if report, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil); err != nil || !report.Passed {
		t.Fatalf("ClaimAndRunCheck() = %+v, err %v; want passing", report, err)
	}
	expect(t, "check claimed", during,
		entityStates{run.RunCompleting, run.TaskChecking, run.AttemptChecking, run.SessionActive})
}

// TestReferenceTraceEarlyAcceptanceWinsFirst is trace 1's alternative
// order: the claim settles and the acceptance wins before the controller
// applies the lifecycle transitions — the acceptance transaction itself
// moves Run launching→running, Task active→checking and Attempt
// launching→submitted atomically, and the controller's later observation
// of the settled claim only activates the session.
func TestReferenceTraceEarlyAcceptanceWinsFirst(t *testing.T) {
	tc := newTestController(defaultPolicy())
	handle, detail := startedRun(t, tc)
	claimLaunch(t, tc, detail, 4242)
	claim := tc.Store.LaunchClaims[detail.Binding.IncarnationID]
	claim.State = app.LaunchClaimExeced
	tc.Store.LaunchClaims[detail.Binding.IncarnationID] = claim

	if result, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil || result.Kind != string(app.SubmissionAccepted) {
		t.Fatalf("SubmitResult() = %+v, err %v; want the early acceptance", result, err)
	}
	expect(t, "atomic handoff", states(t, tc, detail.RunID, detail.SessionID),
		entityStates{run.RunRunning, run.TaskChecking, run.AttemptSubmitted, run.SessionLaunching})

	if progress, err := tc.Controller.CorroborateLaunch(context.Background(), handle); err != nil || progress != app.LaunchAlreadySettled {
		t.Fatalf("CorroborateLaunch() = %s, err %v; want already-settled (a no-op beyond session activation)", progress, err)
	}
	expect(t, "controller observation", states(t, tc, detail.RunID, detail.SessionID),
		entityStates{run.RunRunning, run.TaskChecking, run.AttemptSubmitted, run.SessionActive})
}

// relaunchedRun drives a run to an authorized cold relaunch (trace 2's
// first line) and returns the handle, the original detail and the
// replacement session id.
func relaunchedRun(t *testing.T, tc *testController) (app.RunHandle, app.RunDetail, identity.SessionID) {
	t.Helper()
	_, detail := startedRun(t, tc)
	claimLaunch(t, tc, detail, 4242)
	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{}, app.ErrPaneNotFound // positively gone by id
	}
	req := defaultResumeRequest(detail.RunID.String())
	req.ConfirmAbsent = true
	tc.Clock.Advance(leaseTTL + time.Second)
	result, handle, err := tc.Controller.Resume(context.Background(), req)
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if result.Outcome != app.ResumeColdRelaunched {
		t.Fatalf("Outcome = %s, want %s", result.Outcome, app.ResumeColdRelaunched)
	}
	updated, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus() error = %v", err)
	}
	if updated.SessionID == detail.SessionID {
		t.Fatalf("cold relaunch did not bind a replacement session")
	}
	return handle, detail, updated.SessionID
}

// TestReferenceTraceColdRelaunchSubmitBeforeRunning is section 5
// reference trace 2: a cold relaunch is authorized, the new incarnation's
// claim settles, and the submission lands the atomic handoff before the
// controller applies lifecycle; the new session activates on the settled
// claim.
func TestReferenceTraceColdRelaunchSubmitBeforeRunning(t *testing.T) {
	tc := newTestController(defaultPolicy())
	handle, detail, newSessionID := relaunchedRun(t, tc)
	expect(t, "relaunch authorized", states(t, tc, detail.RunID, newSessionID),
		entityStates{run.RunLaunching, run.TaskActive, run.AttemptRelaunching, run.SessionLaunching})
	if got := tc.Store.Sessions[detail.SessionID].value.State; got != run.SessionLost {
		t.Fatalf("original session state = %s, want %s", got, run.SessionLost)
	}

	updated, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus() error = %v", err)
	}
	newIncarnation := updated.Binding.IncarnationID
	if newIncarnation == detail.Binding.IncarnationID {
		t.Fatalf("cold relaunch did not mint a new incarnation")
	}
	if err := tc.Store.ClaimLaunch(context.Background(), app.LaunchClaim{
		IncarnationID: newIncarnation, RunID: detail.RunID, AttemptID: detail.AttemptID,
		Executable: "/usr/bin/claude", PID: 5252, State: app.LaunchClaimExeced,
	}); err != nil {
		t.Fatalf("ClaimLaunch() error = %v", err)
	}

	submit := defaultSubmitRequest(detail)
	submit.IncarnationID = newIncarnation.String()
	if result, submitErr := tc.Controller.SubmitResult(context.Background(), submit); submitErr != nil || result.Kind != string(app.SubmissionAccepted) {
		t.Fatalf("SubmitResult() = %+v, err %v; want the atomic handoff", result, submitErr)
	}
	expect(t, "atomic handoff", states(t, tc, detail.RunID, newSessionID),
		entityStates{run.RunRunning, run.TaskChecking, run.AttemptSubmitted, run.SessionLaunching})

	if progress, corrErr := tc.Controller.CorroborateLaunch(context.Background(), handle); corrErr != nil || progress != app.LaunchAlreadySettled {
		t.Fatalf("CorroborateLaunch() = %s, err %v; want already-settled", progress, corrErr)
	}
	expect(t, "session active on the settled claim", states(t, tc, detail.RunID, newSessionID),
		entityStates{run.RunRunning, run.TaskChecking, run.AttemptSubmitted, run.SessionActive})
}

// TestReferenceTraceExecFailureAfterRelaunch is section 5 reference
// trace 3: as trace 2 up to the claim, which settles exec_failed — the
// attempt fails, the replacement session terminates, and task and run
// fail with it.
func TestReferenceTraceExecFailureAfterRelaunch(t *testing.T) {
	tc := newTestController(defaultPolicy())
	handle, detail, newSessionID := relaunchedRun(t, tc)

	updated, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus() error = %v", err)
	}
	newIncarnation := updated.Binding.IncarnationID
	if err := tc.Store.ClaimLaunch(context.Background(), app.LaunchClaim{
		IncarnationID: newIncarnation, RunID: detail.RunID, AttemptID: detail.AttemptID,
		Executable: "/usr/bin/claude", PID: 5252, State: app.LaunchClaimExecPending,
	}); err != nil {
		t.Fatalf("ClaimLaunch() error = %v", err)
	}
	if err := tc.Store.SettleLaunchFailure(context.Background(), newIncarnation, "exec: no such file"); err != nil {
		t.Fatalf("SettleLaunchFailure() error = %v", err)
	}

	if progress, corrErr := tc.Controller.CorroborateLaunch(context.Background(), handle); corrErr != nil || progress != app.LaunchFailed {
		t.Fatalf("CorroborateLaunch() = %s, err %v; want failed", progress, corrErr)
	}
	expect(t, "exec_failed claim", states(t, tc, detail.RunID, newSessionID),
		entityStates{run.RunFailed, run.TaskFailed, run.AttemptFailed, run.SessionTerminated})
}

// TestReferenceTraceStopDuringLaunching is section 5 reference trace 4:
// stop during launching with a corroborated live process. The interrupt
// is dispatched under the close rule (session launching→stopping), and
// only observed termination settles session, attempt, task and run.
func TestReferenceTraceStopDuringLaunching(t *testing.T) {
	tc := newTestController(defaultPolicy())
	handle, detail := startedRun(t, tc)
	claimLaunch(t, tc, detail, 4242)
	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "hop", Name: "hop", Argv: []string{"/usr/bin/hop", "launch", "--attempt", detail.AttemptID.String()}}}}, nil
	}

	if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
		t.Fatalf("RequestStop() error = %v", err)
	}
	expect(t, "stop requested", states(t, tc, detail.RunID, detail.SessionID),
		entityStates{run.RunStopping, run.TaskActive, run.AttemptLaunching, run.SessionLaunching})

	report, err := tc.Controller.DriveStop(context.Background(), handle)
	if err != nil {
		t.Fatalf("DriveStop() error = %v", err)
	}
	if report.Terminated {
		t.Fatalf("report = %+v; a dispatched interrupt is not observed termination", report)
	}
	expect(t, "interrupt dispatched under the close rule", states(t, tc, detail.RunID, detail.SessionID),
		entityStates{run.RunStopping, run.TaskActive, run.AttemptLaunching, run.SessionStopping})

	// Termination observed on a later round.
	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{}, app.ErrPaneNotFound
	}
	final, err := tc.Controller.DriveStop(context.Background(), handle)
	if err != nil {
		t.Fatalf("second DriveStop() error = %v", err)
	}
	if !final.Terminated {
		t.Fatalf("final report = %+v, want terminated", final)
	}
	expect(t, "termination observed", states(t, tc, detail.RunID, detail.SessionID),
		entityStates{run.RunStopped, run.TaskInterrupted, run.AttemptInterrupted, run.SessionTerminated})
}
