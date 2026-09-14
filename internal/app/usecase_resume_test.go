package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

func defaultResumeRequest(runID string) app.ResumeRequest {
	return app.ResumeRequest{RunID: runID, ControllerID: "controller-2", HOPPath: "/usr/local/bin/hop", StateRoot: "/state"}
}

func TestResume(t *testing.T) {
	t.Run("warm reattach: a matching occupant returns the attempt to running", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "/usr/bin/claude", Argv: []string{"claude", detail.AttemptID.String()}}}}, nil
		}

		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeWarmReattached {
			t.Fatalf("Outcome = %s, want %s", result.Outcome, app.ResumeWarmReattached)
		}

		updated, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.AttemptState != run.AttemptRunning {
			t.Fatalf("Attempt.State = %s, want %s", updated.AttemptState, run.AttemptRunning)
		}
	})

	t.Run("fail closed: an occupant that execed another executable keeping pid and marker is never adopted", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		// The recorded pid and the attempt marker both still match, but the
		// executable identity no longer equals the claim's: the pid+marker
		// shortcut must not adopt this occupant.
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "/bin/bash", Name: "bash", Argv: []string{"bash", detail.AttemptID.String()}}}}, nil
		}

		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeFailedClosed {
			t.Fatalf("Outcome = %s, want %s (executable replacement must fail closed)", result.Outcome, app.ResumeFailedClosed)
		}
		if got := tc.Store.Attempts[detail.AttemptID].value.State; got != run.AttemptReconciling {
			t.Fatalf("Attempt.State = %s, want %s", got, run.AttemptReconciling)
		}
	})

	t.Run("fail closed: a present occupant with no launch claim at all is never adopted", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "/usr/bin/claude", Argv: []string{"claude", detail.AttemptID.String()}}}}, nil
		}

		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeFailedClosed {
			t.Fatalf("Outcome = %s, want %s (no claim: adoption requires a settled claim)", result.Outcome, app.ResumeFailedClosed)
		}
	})

	t.Run("warm reattach settles an exec_pending claim through the ordinary fenced path", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "/usr/bin/claude", Argv: []string{"claude", detail.AttemptID.String()}}}}, nil
		}

		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeWarmReattached {
			t.Fatalf("Outcome = %s, want %s", result.Outcome, app.ResumeWarmReattached)
		}
		if got := tc.Store.LaunchClaims[detail.Binding.IncarnationID].State; got != app.LaunchClaimExeced {
			t.Fatalf("claim state = %s, want %s (settled through the ordinary path)", got, app.LaunchClaimExeced)
		}
		if got := tc.Store.Attempts[detail.AttemptID].value.State; got != run.AttemptRunning {
			t.Fatalf("Attempt.State = %s, want %s", got, run.AttemptRunning)
		}
	})

	t.Run("fail closed: a present, non-matching occupant with no positive evidence", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 9999, Argv0: "/bin/bash", Argv: []string{"bash"}}}}, nil
		}

		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeFailedClosed {
			t.Fatalf("Outcome = %s, want %s", result.Outcome, app.ResumeFailedClosed)
		}
		if result.ObservedPaneID == "" {
			t.Fatalf("a failed-closed report must name the observed pane")
		}
	})

	t.Run("cold relaunch: absence plus --confirm-absent authorizes a new session and supersedes the old intent", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound // positively gone by id
		}

		// Simulate the previous incarnation's pane.open operation left
		// unresolved by a crash (its outcome transaction never committed);
		// the SQLite adapter's pre-binding claim fallback matches
		// ClaimLaunch against the newest such pending operation, so a
		// zombie launcher for the old incarnation must never find it. The
		// fake store always round-trips Intent through JSON on commit (see
		// jsonRoundtripOperation), so this operation's Intent is already
		// the map[string]any shape a real store returns — the same shape
		// retirePendingLaunchIntents must decode via decodeOperationPayload.
		for id, op := range tc.Store.Operations {
			if op.Kind == app.OpPaneOpen {
				op.State = app.OperationPending
				tc.Store.Operations[id] = op
			}
		}

		req := defaultResumeRequest(detail.RunID.String())
		req.ConfirmAbsent = true
		tc.Clock.Advance(leaseTTL + time.Second)
		result, newHandle, err := tc.Controller.Resume(context.Background(), req)
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeColdRelaunched {
			t.Fatalf("Outcome = %s, want %s", result.Outcome, app.ResumeColdRelaunched)
		}
		if newHandle.RunID() != handle.RunID() {
			t.Fatalf("resume returned a handle for a different run")
		}

		updated, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.AttemptState != run.AttemptRelaunching && updated.AttemptState != run.AttemptRunning {
			t.Fatalf("Attempt.State = %s, want relaunching (or running once corroborated)", updated.AttemptState)
		}
		if updated.SessionID == detail.SessionID {
			t.Fatalf("cold relaunch did not bind a new session")
		}

		var reconcilingCount, otherCount int
		for _, op := range tc.Store.Operations {
			if op.Kind != app.OpPaneOpen {
				continue
			}
			if op.State == app.OperationReconciling {
				reconcilingCount++
			} else {
				otherCount++
			}
		}
		if reconcilingCount < 1 {
			t.Fatalf("expected the previous session's pane.open intent to be superseded (reconciling); found none among the operations")
		}
		if otherCount < 1 {
			t.Fatalf("expected a new pane.open operation for the relaunch; found none")
		}
	})

	t.Run("unsupported: cold resume is refused for a non-Claude harness", func(t *testing.T) {
		tc := newTestController(app.RunPolicy{CheckArgv: []string{"sh", "check.sh"}, Harness: "codex"})
		_, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}

		req := defaultResumeRequest(detail.RunID.String())
		req.ConfirmAbsent = true
		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), req)
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeUnsupported {
			t.Fatalf("Outcome = %s, want %s", result.Outcome, app.ResumeUnsupported)
		}
	})

	t.Run("reserved attempt: startup is continued with the launch intent and a fresh pane", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, _, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if err != nil {
			t.Fatalf("StartRun() error = %v", err)
		}
		runID := tc.onlyRunID(t)
		forceAttemptReserved(t, tc, runID)

		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(runID.String()))
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeStartupContinued {
			t.Fatalf("Outcome = %s, want %s", result.Outcome, app.ResumeStartupContinued)
		}
		updated, err := tc.Store.LoadRunStatus(context.Background(), runID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.AttemptState != run.AttemptLaunching {
			t.Fatalf("Attempt.State = %s, want %s (startup continued)", updated.AttemptState, run.AttemptLaunching)
		}
	})
}

// TestResumeOperationRecovery covers the section 4 decision table on
// takeover: interruption on either side of worktree.create and pane.open
// is recovered by provenance or creation-label adoption, bounded waits
// are persisted and enforced, and nothing is ever re-sent while an intent
// is unresolved.
func TestResumeOperationRecovery(t *testing.T) {
	t.Run("worktree crash before creation: bounded wait, then failed intent, then re-driven startup", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		tc.Runtime.CreateWorktreeErr = context.DeadlineExceeded
		if _, _, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest()); err == nil {
			t.Fatalf("StartRun() succeeded despite worktree.create failing")
		}
		runID := tc.onlyRunID(t)

		// First resume, within the bounded wait: no checkout answers for
		// the intended branch, so the intent stays open with a durable
		// wait record and nothing is re-driven.
		result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(runID.String()))
		if err != nil {
			t.Fatalf("first Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeReconciling {
			t.Fatalf("first Outcome = %s, want %s (within the bounded wait)", result.Outcome, app.ResumeReconciling)
		}
		if len(tc.Store.Worktrees) != 0 {
			t.Fatalf("a worktree row appeared during the bounded wait")
		}

		// Past the deadline the absent checkout settles the old intent as
		// failed and startup re-drives creation and the launch intent.
		tc.Runtime.CreateWorktreeErr = nil
		tc.Clock.Advance(3 * time.Minute)
		second, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(runID.String()))
		if err != nil {
			t.Fatalf("second Resume() error = %v", err)
		}
		if second.Outcome != app.ResumeStartupContinued {
			t.Fatalf("second Outcome = %s, want %s", second.Outcome, app.ResumeStartupContinued)
		}
		updated, err := tc.Store.LoadRunStatus(context.Background(), runID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.AttemptState != run.AttemptLaunching {
			t.Fatalf("Attempt.State = %s, want %s", updated.AttemptState, run.AttemptLaunching)
		}
		if updated.WorktreePath == "" {
			t.Fatalf("no worktree was created by the continued startup")
		}
		if updated.Binding == nil {
			t.Fatalf("no worker pane binding was recorded by the continued startup")
		}
	})

	t.Run("worktree crash after creation: the checkout is adopted by provenance, never re-created", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		tc.Runtime.CreateWorktreeErr = context.DeadlineExceeded
		if _, _, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest()); err == nil {
			t.Fatalf("StartRun() succeeded despite worktree.create failing")
		}
		runID := tc.onlyRunID(t)

		// The creation actually happened before the lost response: the
		// repository's worktree listing shows the intended branch.
		tc.Commands.Results["git -C /repo worktree list --porcelain"] = app.CommandResult{
			ExitCode: 0,
			Stdout:   []byte("worktree /repo\nHEAD cccccccccccccccccccccccccccccccccccccccc\nbranch refs/heads/main\n\nworktree /worktrees/recovered\nHEAD cccccccccccccccccccccccccccccccccccccccc\nbranch refs/heads/hop/run-1\n"),
		}

		created := false
		tc.Runtime.CreateWorktreeErr = nil
		tc.Runtime.CreateWorktreeFn = func(app.WorktreeRequest) (app.WorktreeInfo, error) {
			created = true
			return app.WorktreeInfo{}, context.DeadlineExceeded
		}
		result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(runID.String()))
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if created {
			t.Fatalf("CreateWorktree was re-driven despite an adoptable checkout")
		}
		updated, err := tc.Store.LoadRunStatus(context.Background(), runID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.WorktreePath != "/worktrees/recovered" {
			t.Fatalf("WorktreePath = %q, want the adopted checkout", updated.WorktreePath)
		}
		// The lost create response also lost the workspace placement: the
		// pane cannot be opened safely, so the run stays reconciling with
		// the reason named rather than opening a pane blindly.
		if result.Outcome != app.ResumeReconciling {
			t.Fatalf("Outcome = %s, want %s (workspace placement unrecorded)", result.Outcome, app.ResumeReconciling)
		}
	})

	t.Run("pane.open crash before creation: bounded wait persists, then reconciling, never a second create", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		tc.Store.Bindings = map[identity.SessionID][]run.RuntimeBinding{}
		var opID identity.OperationID
		for id, op := range tc.Store.Operations {
			if op.Kind == app.OpPaneOpen {
				op.State = app.OperationPending
				op.ActEvidence = nil
				tc.Store.Operations[id] = op
				opID = id
			}
		}
		var paneOpens int
		tc.Runtime.OpenWorkerPaneFn = func(app.WorkerPaneRequest) (app.PaneHandle, error) {
			paneOpens++
			return app.PaneHandle{}, context.DeadlineExceeded
		}

		tc.Clock.Advance(leaseTTL + time.Second)
		if _, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String())); err != nil {
			t.Fatalf("first Resume() error = %v", err)
		}
		if tc.Store.Operations[opID].State != app.OperationPending {
			t.Fatalf("operation state = %s within the bounded wait, want pending", tc.Store.Operations[opID].State)
		}
		if wait, ok := tc.Store.Operations[opID].ActEvidence.(map[string]any); !ok || wait["deadline"] == "" {
			t.Fatalf("bounded-wait evidence was not persisted: %+v", tc.Store.Operations[opID].ActEvidence)
		}

		tc.Clock.Advance(3 * time.Minute)
		if _, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String())); err != nil {
			t.Fatalf("second Resume() error = %v", err)
		}
		if got := tc.Store.Operations[opID].State; got != app.OperationReconciling {
			t.Fatalf("operation state = %s past the deadline, want reconciling", got)
		}
		if paneOpens != 0 {
			t.Fatalf("OpenWorkerPane was re-sent %d times; a lost create is never re-sent", paneOpens)
		}
	})

	t.Run("pane.open crash after creation: the pane is adopted by creation label on resume", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		tc.Store.Bindings = map[identity.SessionID][]run.RuntimeBinding{}
		var opID identity.OperationID
		for id, op := range tc.Store.Operations {
			if op.Kind == app.OpPaneOpen {
				op.State = app.OperationPending
				op.ActEvidence = nil
				tc.Store.Operations[id] = op
				opID = id
			}
		}
		tc.Runtime.FindPaneByLabelFn = func(label string) (app.PaneRef, bool, error) {
			if label != opID.String() {
				return app.PaneRef{}, false, nil
			}
			return app.PaneRef{WorkspaceID: "workspace-1", TabID: "tab-r", PaneID: "pane-r"}, true, nil
		}

		tc.Clock.Advance(leaseTTL + time.Second)
		if _, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String())); err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if got := tc.Store.Operations[opID].State; got != app.OperationSucceeded {
			t.Fatalf("operation state = %s, want succeeded after label adoption", got)
		}
		updated, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.Binding == nil || updated.Binding.PaneID != "pane-r" {
			t.Fatalf("the pane was not adopted by creation label: %+v", updated.Binding)
		}
	})
}

// TestResumeRounds proves resume is re-runnable and leaves the run usable
// (docs/plan/phase-2-design.md section 5): a fail-closed round followed by
// a warm round restores every entity, and a warm-reattached run carries a
// submission through a passing check to completion.
func TestResumeRounds(t *testing.T) {
	t.Run("fail-closed round then warm round restores Run, Task, Attempt and Session", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)

		// Round 1: the pane cannot be inspected conclusively and no
		// positive evidence exists; the run stays resuming/reconciling.
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 9999, Argv0: "/bin/bash", Argv: []string{"bash"}}}}, nil
		}
		tc.Clock.Advance(leaseTTL + time.Second)
		first, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
		if err != nil {
			t.Fatalf("first Resume() error = %v", err)
		}
		if first.Outcome != app.ResumeFailedClosed {
			t.Fatalf("first Outcome = %s, want %s", first.Outcome, app.ResumeFailedClosed)
		}

		// Round 2: the true worker is observable again.
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "/usr/bin/claude", Argv: []string{"claude", detail.AttemptID.String()}}}}, nil
		}
		tc.Clock.Advance(leaseTTL + time.Second)
		second, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
		if err != nil {
			t.Fatalf("second Resume() error = %v", err)
		}
		if second.Outcome != app.ResumeWarmReattached {
			t.Fatalf("second Outcome = %s, want %s", second.Outcome, app.ResumeWarmReattached)
		}

		updated, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.State != run.RunRunning {
			t.Fatalf("Run.State = %s, want %s (restored from resuming)", updated.State, run.RunRunning)
		}
		if updated.TaskState != run.TaskActive {
			t.Fatalf("Task.State = %s, want %s", updated.TaskState, run.TaskActive)
		}
		if updated.AttemptState != run.AttemptRunning {
			t.Fatalf("Attempt.State = %s, want %s", updated.AttemptState, run.AttemptRunning)
		}
		if got := tc.Store.Sessions[detail.SessionID].value.State; got != run.SessionActive {
			t.Fatalf("Session.State = %s, want %s", got, run.SessionActive)
		}
	})

	t.Run("warm reattach, then submission and passing check complete the run", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "/usr/bin/claude", Argv: []string{"claude", detail.AttemptID.String()}}}}, nil
		}
		tc.Clock.Advance(leaseTTL + time.Second)
		result, handle, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeWarmReattached {
			t.Fatalf("Outcome = %s, want %s", result.Outcome, app.ResumeWarmReattached)
		}

		if submitted, submitErr := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); submitErr != nil || submitted.Kind != string(app.SubmissionAccepted) {
			t.Fatalf("SubmitResult() = %+v, err %v; want accepted after warm reattach", submitted, submitErr)
		}
		report, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil)
		if err != nil {
			t.Fatalf("ClaimAndRunCheck() error = %v", err)
		}
		if !report.Ran || !report.Passed {
			t.Fatalf("report = %+v, want Ran/Passed after warm reattach", report)
		}

		updated, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.State != run.RunCompleted || updated.TaskState != run.TaskCompleted || updated.AttemptState != run.AttemptCompleted {
			t.Fatalf("Run/Task/Attempt = %s/%s/%s, want all completed", updated.State, updated.TaskState, updated.AttemptState)
		}
	})
}

// TestResumePositiveEvidenceRetirement is the complete section 5 case 2
// scenario: a restored occupant carrying the run's pre-assigned native
// session reference is recorded as an observed-restoration binding under
// a distinct observation incarnation, retired through the guarded
// pane.close operation, superseded on observed absence, and only then
// replaced by a cold relaunch that inherits the native lineage and
// workspace.
func TestResumePositiveEvidenceRetirement(t *testing.T) {
	tc := newTestController(defaultPolicy())
	_, detail := runningRun(t, tc)
	nativeRef := tc.Store.Sessions[detail.SessionID].value.NativeSessionRef
	if nativeRef == "" {
		t.Fatalf("no native session reference was pre-assigned")
	}
	originalIncarnation := detail.Binding.IncarnationID
	originalWorkspace := detail.Binding.WorkspaceID

	// Herdr's native restore replaced the worker: a new pid running
	// `claude --resume <native-ref>`, bypassing the launcher.
	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 7777, Argv0: "/usr/bin/claude", Argv: []string{"claude", "--resume", nativeRef}}}}, nil
	}

	tc.Clock.Advance(leaseTTL + time.Second)
	first, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
	if err != nil {
		t.Fatalf("first Resume() error = %v", err)
	}
	if first.Outcome != app.ResumeReconciling {
		t.Fatalf("first Outcome = %s, want %s (close dispatched, termination not yet observed)", first.Outcome, app.ResumeReconciling)
	}

	history := tc.Store.Bindings[detail.SessionID]
	if len(history) != 2 {
		t.Fatalf("binding history length = %d, want 2 (launch binding + observed restoration)", len(history))
	}
	if !history[0].Superseded {
		t.Fatalf("the launch binding was not superseded by the positive evidence")
	}
	observed := history[1]
	if observed.LaunchKind != run.LaunchRestoredObserved {
		t.Fatalf("observed binding launch kind = %s, want %s", observed.LaunchKind, run.LaunchRestoredObserved)
	}
	if observed.IncarnationID == originalIncarnation {
		t.Fatalf("the observed-restoration binding reused the launch incarnation; UNIQUE(session_id, incarnation_id) demands a distinct observation identity")
	}
	if observed.Occupant == nil || observed.Occupant.PID != 7777 || observed.Occupant.ArgvMarker != nativeRef {
		t.Fatalf("observed binding occupant = %+v, want the restored process evidence", observed.Occupant)
	}
	if len(tc.Runtime.ClosedPanes) != 1 {
		t.Fatalf("ClosePane calls = %d, want 1 guarded close against the observed target", len(tc.Runtime.ClosedPanes))
	}
	scrollbackCaptured := false
	for _, a := range tc.Store.Artifacts {
		if a.Kind == run.ArtifactPaneSnapshot {
			scrollbackCaptured = true
		}
	}
	if !scrollbackCaptured {
		t.Fatalf("pane scrollback was not captured before the retirement close")
	}

	// The occupant is observed gone on the next round: the retirement
	// completes and authorizes the cold relaunch — no attestation needed.
	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{}, app.ErrPaneNotFound
	}
	tc.Clock.Advance(leaseTTL + time.Second)
	second, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
	if err != nil {
		t.Fatalf("second Resume() error = %v", err)
	}
	if second.Outcome != app.ResumeColdRelaunched {
		t.Fatalf("second Outcome = %s, want %s", second.Outcome, app.ResumeColdRelaunched)
	}

	history = tc.Store.Bindings[detail.SessionID]
	if !history[1].Superseded {
		t.Fatalf("the observed-restoration binding was not superseded after confirmed retirement")
	}
	for id := range tc.Store.Operations {
		op := tc.Store.Operations[id]
		if op.Kind == app.OpPaneClose && op.State != app.OperationSucceeded {
			t.Fatalf("pane.close operation state = %s, want succeeded after observed absence", op.State)
		}
	}

	updated, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus() error = %v", err)
	}
	if updated.SessionID == detail.SessionID {
		t.Fatalf("cold relaunch did not bind a new session")
	}
	replacement := tc.Store.Sessions[updated.SessionID].value
	if replacement.NativeSessionRef != nativeRef {
		t.Fatalf("replacement session native ref = %q, want the inherited lineage %q", replacement.NativeSessionRef, nativeRef)
	}
	if oldSession := tc.Store.Sessions[detail.SessionID].value; oldSession.State != run.SessionLost {
		t.Fatalf("prior session state = %s, want %s", oldSession.State, run.SessionLost)
	}
	if updated.AttemptState != run.AttemptRelaunching {
		t.Fatalf("Attempt.State = %s, want %s", updated.AttemptState, run.AttemptRelaunching)
	}
	if updated.Binding == nil || updated.Binding.WorkspaceID != originalWorkspace {
		t.Fatalf("relaunch pane workspace = %+v, want the recorded placement %q", updated.Binding, originalWorkspace)
	}
}

// TestResumeAttestation covers section 5 item 5: --confirm-absent journals
// an absence.attested entry with the observed evidence, and authorizes a
// cold relaunch only in the positively established non-restart case —
// server continuity by recorded-vs-observed instance equality — never
// after a restart or on unknown continuity.
func TestResumeAttestation(t *testing.T) {
	attestations := func(tc *testController) []app.Operation {
		var out []app.Operation
		for id := range tc.Store.Operations {
			if tc.Store.Operations[id].Kind == app.OpAbsenceAttested {
				out = append(out, tc.Store.Operations[id])
			}
		}
		return out
	}

	t.Run("same server instance: attestation authorizes the cold relaunch", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}

		req := defaultResumeRequest(detail.RunID.String())
		req.ConfirmAbsent = true
		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), req)
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeColdRelaunched {
			t.Fatalf("Outcome = %s, want %s", result.Outcome, app.ResumeColdRelaunched)
		}
		recorded := attestations(tc)
		if len(recorded) != 1 {
			t.Fatalf("absence.attested operations = %d, want 1", len(recorded))
		}
		outcome, ok := recorded[0].Outcome.(map[string]any)
		if !ok {
			t.Fatalf("attestation outcome shape = %T, want a JSON object", recorded[0].Outcome)
		}
		if established, isBool := outcome["continuity_established"].(bool); !isBool || !established {
			t.Fatalf("attestation records continuity_established = %v, want true", outcome["continuity_established"])
		}
		if assertions, isList := outcome["assertions"].([]any); !isList || len(assertions) != 2 {
			t.Fatalf("attestation assertions = %v, want both required assertions", outcome["assertions"])
		}
	})

	t.Run("changed server instance: attestation is journaled but relaunch refused", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		// The server restarted between the launch and this resume.
		tc.Runtime.ServerInstanceValue = "peer-pid:2"

		req := defaultResumeRequest(detail.RunID.String())
		req.ConfirmAbsent = true
		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), req)
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeReconciling {
			t.Fatalf("Outcome = %s, want %s (post-restart attestation must refuse relaunch)", result.Outcome, app.ResumeReconciling)
		}
		if len(attestations(tc)) != 1 {
			t.Fatalf("the attestation was not journaled durably")
		}
		if updated := tc.Store.Sessions[detail.SessionID].value; updated.State == run.SessionLost {
			t.Fatalf("the session was retired despite the refused relaunch")
		}
	})

	t.Run("unknown server instance: attestation refused as ambiguous", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		tc.Runtime.ServerInstanceValue = "" // never observable
		_, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}

		req := defaultResumeRequest(detail.RunID.String())
		req.ConfirmAbsent = true
		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), req)
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeReconciling {
			t.Fatalf("Outcome = %s, want %s (unknown continuity is ambiguous)", result.Outcome, app.ResumeReconciling)
		}
	})

	t.Run("delayed restore after a refused attestation is retired by positive evidence", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		nativeRef := tc.Store.Sessions[detail.SessionID].value.NativeSessionRef

		// Round 1: post-restart, pane empty, attestation refused.
		tc.Runtime.ServerInstanceValue = "peer-pid:2"
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		req := defaultResumeRequest(detail.RunID.String())
		req.ConfirmAbsent = true
		tc.Clock.Advance(leaseTTL + time.Second)
		first, _, err := tc.Controller.Resume(context.Background(), req)
		if err != nil {
			t.Fatalf("first Resume() error = %v", err)
		}
		if first.Outcome != app.ResumeReconciling {
			t.Fatalf("first Outcome = %s, want %s", first.Outcome, app.ResumeReconciling)
		}

		// The deferred restore then fires: the restored occupant carries the
		// native reference and is retired by positive evidence (item 2).
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 8888, Argv0: "/usr/bin/claude", Argv: []string{"claude", "--resume", nativeRef}}}}, nil
		}
		tc.Clock.Advance(leaseTTL + time.Second)
		second, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
		if err != nil {
			t.Fatalf("second Resume() error = %v", err)
		}
		if second.Outcome != app.ResumeReconciling {
			t.Fatalf("second Outcome = %s, want %s (retirement close dispatched)", second.Outcome, app.ResumeReconciling)
		}

		// Once the retired occupant is observed gone, the relaunch proceeds
		// on the positive retirement evidence — no further attestation.
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		tc.Clock.Advance(leaseTTL + time.Second)
		third, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
		if err != nil {
			t.Fatalf("third Resume() error = %v", err)
		}
		if third.Outcome != app.ResumeColdRelaunched {
			t.Fatalf("third Outcome = %s, want %s", third.Outcome, app.ResumeColdRelaunched)
		}
	})
}

// TestPaneAbsenceRule proves absence is established only by a successful
// inspection showing no occupant AND no pane answering for the creation
// label: an inspection error is ambiguous with the error named, and an
// empty foreground with the label still answering never authorizes an
// attested relaunch or a stop's termination.
func TestPaneAbsenceRule(t *testing.T) {
	t.Run("inspection error is ambiguous: attestation refused, error named", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, context.DeadlineExceeded
		}
		// The label lookup would report no pane — irrelevant: an inspection
		// error alone keeps the observation ambiguous.
		tc.Runtime.FindPaneByLabelFn = func(string) (app.PaneRef, bool, error) {
			return app.PaneRef{}, false, nil
		}

		req := defaultResumeRequest(detail.RunID.String())
		req.ConfirmAbsent = true
		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), req)
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeReconciling {
			t.Fatalf("Outcome = %s, want %s (inspection error is never absence)", result.Outcome, app.ResumeReconciling)
		}
		if !strings.Contains(result.Detail, "pane inspection failed") || !strings.Contains(result.Detail, "context deadline exceeded") {
			t.Fatalf("Detail = %q, want the inspection error named", result.Detail)
		}
		if got := tc.Store.Sessions[detail.SessionID].value.State; got == run.SessionLost {
			t.Fatalf("the session was retired on an ambiguous observation")
		}
	})

	t.Run("a pane still answering by id with an empty foreground is not absence", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, nil // successful inspection: pane exists, no occupant
		}

		req := defaultResumeRequest(detail.RunID.String())
		req.ConfirmAbsent = true
		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), req)
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeReconciling {
			t.Fatalf("Outcome = %s, want %s (an empty foreground is never absence)", result.Outcome, app.ResumeReconciling)
		}
		if !strings.Contains(result.Detail, "still answers by id") {
			t.Fatalf("Detail = %q, want the pane-answers-by-id reason named", result.Detail)
		}
	})

	t.Run("gone by id but the label still answering is not absence", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		tc.Runtime.FindPaneByLabelFn = func(string) (app.PaneRef, bool, error) {
			return app.PaneRef{WorkspaceID: "workspace-1", TabID: "tab-1", PaneID: detail.Binding.PaneID}, true, nil
		}

		req := defaultResumeRequest(detail.RunID.String())
		req.ConfirmAbsent = true
		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), req)
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeReconciling {
			t.Fatalf("Outcome = %s, want %s (a pane still answering for the label is never absence)", result.Outcome, app.ResumeReconciling)
		}
		if !strings.Contains(result.Detail, "still answers for the creation label") {
			t.Fatalf("Detail = %q, want the label-presence reason named", result.Detail)
		}
	})
}

// TestCreationInstanceProvenance proves a binding recovered by label
// carries the server identity frozen into the pane.open intent at
// creation — never a recovery-time observation — so continuity across a
// server restart can never be fabricated by recovery.
func TestCreationInstanceProvenance(t *testing.T) {
	loseBinding := func(t *testing.T, tc *testController) {
		t.Helper()
		tc.Store.Bindings = map[identity.SessionID][]run.RuntimeBinding{}
		for id, op := range tc.Store.Operations {
			if op.Kind == app.OpPaneOpen {
				op.State = app.OperationPending
				op.ActEvidence = nil
				tc.Store.Operations[id] = op
			}
		}
	}

	t.Run("binding lost across a restart: recovery keeps the creation identity and attestation refuses", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc) // creation observed peer-pid:1 into the intent
		claimLaunch(t, tc, detail, 4242)
		loseBinding(t, tc)

		// Herdr restarted before the takeover; the restored pane still
		// carries its creation label.
		tc.Runtime.ServerInstanceValue = "peer-pid:2"
		var label string
		for id, op := range tc.Store.Operations {
			if op.Kind == app.OpPaneOpen {
				label = id.String()
			}
		}
		tc.Runtime.FindPaneByLabelFn = func(l string) (app.PaneRef, bool, error) {
			if l == label {
				return app.PaneRef{WorkspaceID: "workspace-1", TabID: "tab-9", PaneID: "pane-9"}, true, nil
			}
			return app.PaneRef{}, false, nil
		}

		tc.Clock.Advance(leaseTTL + time.Second)
		if _, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String())); err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		recovered, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if recovered.Binding == nil {
			t.Fatalf("the binding was not recovered by label")
		}
		if recovered.Binding.ServerInstance != "peer-pid:1" {
			t.Fatalf("recovered binding ServerInstance = %q, want the creation-time %q, never the recovery-time observation", recovered.Binding.ServerInstance, "peer-pid:1")
		}

		// The pane is later positively gone; the attestation must refuse
		// the relaunch because creation and current identities differ.
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		tc.Runtime.FindPaneByLabelFn = func(string) (app.PaneRef, bool, error) {
			return app.PaneRef{}, false, nil
		}
		req := defaultResumeRequest(detail.RunID.String())
		req.ConfirmAbsent = true
		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), req)
		if err != nil {
			t.Fatalf("attesting Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeReconciling {
			t.Fatalf("Outcome = %s, want %s (recovered creation identity differs from the current server)", result.Outcome, app.ResumeReconciling)
		}
	})

	t.Run("binding lost with the server unchanged: recovery preserves continuity and attestation authorizes", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		loseBinding(t, tc)
		var label string
		for id, op := range tc.Store.Operations {
			if op.Kind == app.OpPaneOpen {
				label = id.String()
			}
		}
		tc.Runtime.FindPaneByLabelFn = func(l string) (app.PaneRef, bool, error) {
			if l == label {
				return app.PaneRef{WorkspaceID: "workspace-1", TabID: "tab-9", PaneID: "pane-9"}, true, nil
			}
			return app.PaneRef{}, false, nil
		}
		tc.Clock.Advance(leaseTTL + time.Second)
		if _, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String())); err != nil {
			t.Fatalf("Resume() error = %v", err)
		}

		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		tc.Runtime.FindPaneByLabelFn = func(string) (app.PaneRef, bool, error) {
			return app.PaneRef{}, false, nil
		}
		req := defaultResumeRequest(detail.RunID.String())
		req.ConfirmAbsent = true
		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), req)
		if err != nil {
			t.Fatalf("attesting Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeColdRelaunched {
			t.Fatalf("Outcome = %s, want %s (creation identity equals the current server)", result.Outcome, app.ResumeColdRelaunched)
		}
	})

	t.Run("an undecodable pending launch payload blocks retirement", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		opID, err := identity.ParseOperationID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse operation id: %v", err)
		}
		tc.Store.Operations[opID] = app.Operation{
			ID: opID, RunID: detail.RunID, Generation: tc.Store.Leases[detail.RunID].lease.Generation,
			Kind: app.OpPaneOpen, State: app.OperationPending, Intent: "garbage, not an object",
			CreatedAt: tc.Clock.Now(), UpdatedAt: tc.Clock.Now(),
		}
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}

		req := defaultResumeRequest(detail.RunID.String())
		req.ConfirmAbsent = true
		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, resumeErr := tc.Controller.Resume(context.Background(), req)
		if resumeErr == nil && result.Outcome == app.ResumeColdRelaunched {
			t.Fatalf("Resume() relaunched despite an undecodable pending launch payload")
		}
		updated, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.AttemptState == run.AttemptRelaunching {
			t.Fatalf("Attempt relaunched despite the blocked retirement")
		}
	})
}
