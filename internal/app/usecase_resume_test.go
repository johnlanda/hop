package app_test

import (
	"context"
	"encoding/json"
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
		// The pane reports the pinned multi-member shape: the worker's own
		// MCP-server children share its process group and are listed before
		// it, so adoption must corroborate the worker from index 1+ and
		// record THAT member's pid as occupant evidence.
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return mcpGroupPane(4242, "/usr/bin/claude", detail.AttemptID.String()), nil
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
		bindings := tc.Store.Bindings[detail.SessionID]
		if n := len(bindings); n == 0 || bindings[n-1].Occupant == nil || bindings[n-1].Occupant.PID != 4242 {
			t.Fatalf("binding occupant evidence = %+v, want the corroborated member's pid 4242, never an MCP sibling's", bindings)
		}
	})

	t.Run("fail closed: an occupant that execed another executable keeping pid and marker is never adopted", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		// The recorded pid and the attempt marker both still match, but the
		// executable identity no longer equals the claim's: the pid+marker
		// shortcut must not adopt this occupant.
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "bash", Name: "bash", Argv: []string{"/bin/bash", detail.AttemptID.String()}}}}, nil
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
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "claude", Name: "claude", Argv: []string{"/usr/bin/claude", detail.AttemptID.String()}}}}, nil
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
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "claude", Name: "claude", Argv: []string{"/usr/bin/claude", detail.AttemptID.String()}}}}, nil
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
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 9999, Argv0: "bash", Name: "bash", Argv: []string{"/bin/bash"}}}}, nil
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
		tc.Commands.Results["/usr/bin/git -C /repo worktree list --porcelain"] = app.CommandResult{
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
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 9999, Argv0: "bash", Name: "bash", Argv: []string{"/bin/bash"}}}}, nil
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
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "claude", Name: "claude", Argv: []string{"/usr/bin/claude", detail.AttemptID.String()}}}}, nil
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
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "claude", Name: "claude", Argv: []string{"/usr/bin/claude", detail.AttemptID.String()}}}}, nil
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
	// `claude --resume <native-ref>`, bypassing the launcher. The restored
	// harness spawned its own MCP-server child into its process group, and
	// the raw listing reports that child FIRST — the positive evidence and
	// the recorded close target must both come from the member that
	// actually carries the native reference, never from index 0.
	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{Foreground: []app.ProcessInfo{
			{PID: 7840, Argv0: "npm", Name: "npm", Argv: []string{"npm", "exec", "@executeautomation/playwright-mcp-server"}},
			{PID: 7777, Argv0: "claude", Name: "claude", Argv: []string{"/usr/bin/claude", "--resume", nativeRef}},
		}}, nil
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
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 8888, Argv0: "claude", Name: "claude", Argv: []string{"/usr/bin/claude", "--resume", nativeRef}}}}, nil
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

// TestSettledClaimIsNotLiveAdoption proves lifecycle catch-up for an
// already-execed claim never doubles as warm adoption: the occupant must
// still be verified under the corroboration predicate, and an absent or
// replaced pane fails closed.
func TestSettledClaimIsNotLiveAdoption(t *testing.T) {
	settledStuckLaunch := func(t *testing.T, tc *testController) app.RunDetail {
		t.Helper()
		_, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		// A prior generation settled the claim but its lifecycle was lost.
		claim := tc.Store.LaunchClaims[detail.Binding.IncarnationID]
		claim.State = app.LaunchClaimExeced
		tc.Store.LaunchClaims[detail.Binding.IncarnationID] = claim
		return detail
	}

	t.Run("absent pane: catch-up applies but the outcome is never warm", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		detail := settledStuckLaunch(t, tc)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}

		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome == app.ResumeWarmReattached {
			t.Fatalf("Outcome = %s; an absent worker must never be adopted through lifecycle catch-up", result.Outcome)
		}
		if got := tc.Store.Attempts[detail.AttemptID].value.State; got != run.AttemptReconciling {
			t.Fatalf("Attempt.State = %s, want %s (caught up, then reconciling)", got, run.AttemptReconciling)
		}
	})

	t.Run("replaced occupant: catch-up applies and the outcome fails closed", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		detail := settledStuckLaunch(t, tc)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 9999, Argv0: "bash", Name: "bash", Argv: []string{"/bin/bash"}}}}, nil
		}

		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeFailedClosed {
			t.Fatalf("Outcome = %s, want %s (a replaced occupant is never adopted)", result.Outcome, app.ResumeFailedClosed)
		}
	})
}

// TestRecoveryBlockingDispositions covers the remaining M1 sequences:
// unknown unresolved work blocks new acts; a durably completed retirement
// finishes cold recovery on a later round even after its binding was
// superseded; and startup after a crash immediately following
// InitializeRun rebuilds worktree and assignment from the frozen run.
func TestRecoveryBlockingDispositions(t *testing.T) {
	t.Run("an unknown unresolved operation blocks the attested relaunch", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		opID, err := identity.ParseOperationID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse operation id: %v", err)
		}
		tc.Store.Operations[opID] = app.Operation{
			ID: opID, RunID: detail.RunID, Generation: tc.Store.Leases[detail.RunID].lease.Generation,
			Kind: app.OperationKind("bogus.kind"), State: app.OperationPending,
			CreatedAt: tc.Clock.Now(), UpdatedAt: tc.Clock.Now(),
		}
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
			t.Fatalf("Outcome = %s, want %s (unknown unresolved work blocks new acts)", result.Outcome, app.ResumeReconciling)
		}
		if !strings.Contains(result.Detail, "bogus.kind") {
			t.Fatalf("Detail = %q, want the blocking operation named", result.Detail)
		}
		if got := tc.Store.Operations[opID].State; got != app.OperationReconciling {
			t.Fatalf("unknown operation state = %s, want reconciling", got)
		}
	})

	t.Run("a completed retirement finishes cold recovery after binding supersession", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		nativeRef := tc.Store.Sessions[detail.SessionID].value.NativeSessionRef

		// Round 1 records the observed restoration and dispatches the
		// guarded close.
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 7777, Argv0: "claude", Name: "claude", Argv: []string{"/usr/bin/claude", "--resume", nativeRef}}}}, nil
		}
		tc.Clock.Advance(leaseTTL + time.Second)
		if _, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String())); err != nil {
			t.Fatalf("first Resume() error = %v", err)
		}

		// The crashed controller had already committed the close outcome
		// and superseded the observation binding, but died before the
		// relaunch it authorized.
		now := tc.Clock.Now()
		for id, op := range tc.Store.Operations {
			if op.Kind != app.OpPaneClose {
				continue
			}
			op.State = app.OperationSucceeded
			op.Outcome = map[string]any{"absence_observed": true, "detail": "occupant absent after close"}
			tc.Store.Operations[id] = op
		}
		history := tc.Store.Bindings[detail.SessionID]
		for i := range history {
			if !history[i].Superseded {
				superseded, err := history[i].Supersede("pane close retirement: occupant absent after close", now)
				if err != nil {
					t.Fatalf("Supersede() error = %v", err)
				}
				history[i] = superseded
			}
		}
		tc.Store.Bindings[detail.SessionID] = history

		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
		if err != nil {
			t.Fatalf("second Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeColdRelaunched {
			t.Fatalf("Outcome = %s, want %s (the durable retirement outcome finishes the recovery)", result.Outcome, app.ResumeColdRelaunched)
		}
	})

	t.Run("crash right after InitializeRun: startup rebuilds from the frozen run, assignment recreated", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, _, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if err != nil {
			t.Fatalf("StartRun() error = %v", err)
		}
		runID := tc.onlyRunID(t)
		forceAttemptReserved(t, tc, runID)
		// Erase everything the crash would have prevented: operations,
		// worktree rows, bindings and the assignment file itself.
		tc.Store.Operations = map[identity.OperationID]app.Operation{}
		tc.Store.Worktrees = map[identity.WorktreeID]*entityRow[run.Worktree]{}
		tc.Store.Bindings = map[identity.SessionID][]run.RuntimeBinding{}
		assignmentPath := tc.Store.Snapshots[runID].AssignmentPath
		delete(tc.Artifacts.files, assignmentPath)

		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(runID.String()))
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeStartupContinued {
			t.Fatalf("Outcome = %s, want %s", result.Outcome, app.ResumeStartupContinued)
		}
		content, readErr := tc.Artifacts.ReadArtifact(context.Background(), assignmentPath)
		if readErr != nil {
			t.Fatalf("the assignment artifact was not recreated: %v", readErr)
		}
		if got := len(content); got == 0 {
			t.Fatalf("recreated assignment is empty")
		}
		updated, err := tc.Store.LoadRunStatus(context.Background(), runID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.WorktreePath == "" || updated.Binding == nil {
			t.Fatalf("startup did not rebuild worktree and worker pane: path=%q binding=%v", updated.WorktreePath, updated.Binding)
		}
	})
}

// TestRetirementTargetImmutability is the M2 second-round scenario: an
// unresolved positive-evidence close is never retargeted — a restored
// occupant whose pid changed while it still carries the native marker is
// not closed under the old row.
func TestRetirementTargetImmutability(t *testing.T) {
	tc := newTestController(defaultPolicy())
	_, detail := runningRun(t, tc)
	nativeRef := tc.Store.Sessions[detail.SessionID].value.NativeSessionRef

	// Round 1: the restored occupant (pid 7777) is recorded and its
	// guarded close dispatched; it stays unresolved.
	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 7777, Argv0: "claude", Name: "claude", Argv: []string{"/usr/bin/claude", "--resume", nativeRef}}}}, nil
	}
	tc.Clock.Advance(leaseTTL + time.Second)
	if _, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String())); err != nil {
		t.Fatalf("first Resume() error = %v", err)
	}
	if len(tc.Runtime.ClosedPanes) != 1 {
		t.Fatalf("ClosePane calls after round 1 = %d, want 1", len(tc.Runtime.ClosedPanes))
	}

	// Round 2: a DIFFERENT process (pid 8888) now occupies the pane, still
	// carrying the native marker. The persisted target names 7777; the new
	// occupant must not be closed under that row.
	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 8888, Argv0: "claude", Name: "claude", Argv: []string{"/usr/bin/claude", "--resume", nativeRef}}}}, nil
	}
	tc.Clock.Advance(leaseTTL + time.Second)
	result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
	if err != nil {
		t.Fatalf("second Resume() error = %v", err)
	}
	if result.Outcome == app.ResumeColdRelaunched {
		t.Fatalf("Outcome = %s; the changed-pid occupant must not be retired under the old intent", result.Outcome)
	}
	if len(tc.Runtime.ClosedPanes) != 1 {
		t.Fatalf("ClosePane calls after round 2 = %d, want still 1 (no close against the new pid under the old row)", len(tc.Runtime.ClosedPanes))
	}
	reconciling := false
	for id := range tc.Store.Operations {
		op := tc.Store.Operations[id]
		if op.Kind == app.OpPaneClose && op.State == app.OperationReconciling {
			reconciling = true
		}
	}
	if !reconciling {
		t.Fatalf("the unresolved close was not marked reconciling on the occupant mismatch")
	}
}

// assertNothingRetired fails unless a resume round left the session's
// pane, binding and lineage untouched: no ClosePane call, no pane.close
// operation journaled, the launch binding neither superseded nor joined by
// an observed-restoration binding, and no replacement session.
func assertNothingRetired(t *testing.T, tc *testController, detail app.RunDetail) { //nolint:gocritic // hugeParam: detail is the test's already-loaded RunDetail, passed once per assertion.
	t.Helper()
	if n := len(tc.Runtime.ClosedPanes); n != 0 {
		t.Fatalf("ClosePane calls = %d, want 0 (nothing may be retired)", n)
	}
	for id := range tc.Store.Operations {
		if tc.Store.Operations[id].Kind == app.OpPaneClose {
			t.Fatalf("a pane.close operation was journaled; nothing may be retired")
		}
	}
	history := tc.Store.Bindings[detail.SessionID]
	if len(history) != 1 || history[0].Superseded || history[0].IncarnationID != detail.Binding.IncarnationID {
		t.Fatalf("binding history = %+v, want the one launch binding, unsuperseded", history)
	}
	updated, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus() error = %v", err)
	}
	if updated.SessionID != detail.SessionID {
		t.Fatalf("a replacement session %s was bound; no relaunch may follow", updated.SessionID)
	}
	if got := tc.Store.Sessions[detail.SessionID].value.State; got == run.SessionLost {
		t.Fatalf("Session.State = %s; the session was retired", got)
	}
}

// TestResumeRestoredHarnessPredicate proves positive-evidence retirement is
// authorized only by the restored-harness predicate: exactly one
// foreground member that is Claude Code's native restore invocation for the
// session's durable native reference, harness identity and the exact
// `--resume <ref>` argv elements on that same member. Every other shape —
// an unrelated member whose argument merely embeds the reference, a foreign
// executable carrying the exact pair, two competing candidates — fails
// closed with nothing closed, superseded or relaunched, behind a
// marker-free foreign first member.
func TestResumeRestoredHarnessPredicate(t *testing.T) {
	shell := app.ProcessInfo{PID: 9001, Argv0: "bash", Name: "bash", Argv: []string{"/bin/bash"}, Cmdline: "/bin/bash"}

	failClosed := map[string]struct {
		members    func(nativeRef string) []app.ProcessInfo
		wantDetail []string
	}{
		"a marker-bearing unrelated child whose argument embeds the reference": {
			members: func(nativeRef string) []app.ProcessInfo {
				path := "/logs/" + nativeRef + ".jsonl"
				return []app.ProcessInfo{shell, {PID: 9002, Argv0: "node", Name: "node", Argv: []string{"node", "viewer.js", path}, Cmdline: "node viewer.js " + path}}
			},
			wantDetail: []string{"no positive evidence"},
		},
		"a foreign executable carrying the reference as the exact resume argument": {
			members: func(nativeRef string) []app.ProcessInfo {
				return []app.ProcessInfo{shell, {PID: 9002, Argv0: "viewer", Name: "viewer", Argv: []string{"/usr/local/bin/viewer", "--resume", nativeRef}, Cmdline: "/usr/local/bin/viewer --resume " + nativeRef}}
			},
			wantDetail: []string{"no positive evidence"},
		},
		"the reference in a cmdline substring while argv lacks the resume pair": {
			members: func(nativeRef string) []app.ProcessInfo {
				return []app.ProcessInfo{shell, {PID: 7777, Argv0: "claude", Name: "claude", Argv: []string{"claude", "--continue"}, Cmdline: "claude --resume " + nativeRef}}
			},
			wantDetail: []string{"no positive evidence"},
		},
		"two restored-harness candidates are ambiguous": {
			members: func(nativeRef string) []app.ProcessInfo {
				return []app.ProcessInfo{
					shell,
					{PID: 7777, Argv0: "claude", Name: "claude", Argv: []string{"claude", "--resume", nativeRef}},
					{PID: 7778, Argv0: "claude", Name: "claude", Argv: []string{"/usr/bin/claude", "--resume", nativeRef}},
				}
			},
			wantDetail: []string{"more than one", "7777", "7778"},
		},
	}
	for name, vector := range failClosed {
		t.Run("fail closed: "+name, func(t *testing.T) {
			tc := newTestController(defaultPolicy())
			_, detail := runningRun(t, tc)
			nativeRef := tc.Store.Sessions[detail.SessionID].value.NativeSessionRef
			members := vector.members(nativeRef)
			tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
				return app.PaneProcess{ForegroundGroupID: 9001, Foreground: members}, nil
			}

			tc.Clock.Advance(leaseTTL + time.Second)
			result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
			if err != nil {
				t.Fatalf("Resume() error = %v", err)
			}
			if result.Outcome != app.ResumeFailedClosed {
				t.Fatalf("Outcome = %s, want %s", result.Outcome, app.ResumeFailedClosed)
			}
			if result.ObservedPaneID != detail.Binding.PaneID {
				t.Fatalf("ObservedPaneID = %q, want the bound pane %q", result.ObservedPaneID, detail.Binding.PaneID)
			}
			for _, want := range vector.wantDetail {
				if !strings.Contains(result.Detail, want) {
					t.Fatalf("Detail = %q, want it to name %q", result.Detail, want)
				}
			}
			assertNothingRetired(t, tc, detail)
		})
	}

	retired := map[string]func(nativeRef string) []app.ProcessInfo{
		"the restored harness listed ahead of its MCP child": func(nativeRef string) []app.ProcessInfo {
			return []app.ProcessInfo{
				{PID: 7777, Argv0: "claude", Name: "claude", Argv: []string{"claude", "--resume", nativeRef}, Cmdline: "claude --resume " + nativeRef},
				{PID: 7840, Argv0: "npm", Name: "npm", Argv: []string{"npm", "exec", "@executeautomation/playwright-mcp-server"}},
			}
		},
		"argv-absent fallback: identity by argv0/name, the token-bounded resume pair in cmdline": func(nativeRef string) []app.ProcessInfo {
			return []app.ProcessInfo{
				{PID: 7840, Name: "npm", Cmdline: "npm exec @executeautomation/playwright-mcp-server"},
				{PID: 7777, Argv0: "claude", Name: "claude", Cmdline: "claude --resume " + nativeRef},
			}
		},
	}
	for name, members := range retired {
		t.Run("retired: "+name, func(t *testing.T) {
			tc := newTestController(defaultPolicy())
			_, detail := runningRun(t, tc)
			nativeRef := tc.Store.Sessions[detail.SessionID].value.NativeSessionRef
			observed := members(nativeRef)
			tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
				return app.PaneProcess{ForegroundGroupID: 7777, Foreground: observed}, nil
			}

			tc.Clock.Advance(leaseTTL + time.Second)
			result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
			if err != nil {
				t.Fatalf("Resume() error = %v", err)
			}
			if result.Outcome != app.ResumeReconciling {
				t.Fatalf("Outcome = %s (%s), want %s (close dispatched, termination not yet observed)", result.Outcome, result.Detail, app.ResumeReconciling)
			}
			if n := len(tc.Runtime.ClosedPanes); n != 1 {
				t.Fatalf("ClosePane calls = %d, want 1 guarded close of the restored harness", n)
			}
			history := tc.Store.Bindings[detail.SessionID]
			if len(history) != 2 || history[1].Occupant == nil || history[1].Occupant.PID != 7777 || history[1].Occupant.ArgvMarker != nativeRef {
				t.Fatalf("binding history = %+v, want the observed restoration of pid 7777", history)
			}
		})
	}

	t.Run("fail closed: the close-time recheck refuses a retirement target no longer the restored harness", func(t *testing.T) {
		// The restored harness authorizes the retirement, but by the time
		// the close procedure re-inspects, its pid is held by an unrelated
		// process whose argument merely embeds the reference. The recorded
		// pid plus a cmdline substring must never pass the recheck.
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		nativeRef := tc.Store.Sessions[detail.SessionID].value.NativeSessionRef
		path := "/logs/" + nativeRef + ".jsonl"
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			tc.Store.mu.Lock()
			recorded := len(tc.Store.Bindings[detail.SessionID]) == 2
			tc.Store.mu.Unlock()
			if recorded {
				return app.PaneProcess{Foreground: []app.ProcessInfo{shell, {PID: 7777, Argv0: "node", Name: "node", Argv: []string{"node", "viewer.js", path}, Cmdline: "node viewer.js " + path}}}, nil
			}
			return app.PaneProcess{Foreground: []app.ProcessInfo{shell, {PID: 7777, Argv0: "claude", Name: "claude", Argv: []string{"claude", "--resume", nativeRef}}}}, nil
		}

		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
		if err != nil {
			t.Fatalf("Resume() error = %v", err)
		}
		if result.Outcome == app.ResumeColdRelaunched {
			t.Fatalf("Outcome = %s; a changed occupant must never be retired", result.Outcome)
		}
		if n := len(tc.Runtime.ClosedPanes); n != 0 {
			t.Fatalf("ClosePane calls = %d, want 0 (the recheck must fail closed)", n)
		}
		reconciling := false
		for id := range tc.Store.Operations {
			if op := tc.Store.Operations[id]; op.Kind == app.OpPaneClose && op.State == app.OperationReconciling {
				reconciling = true
			}
		}
		if !reconciling {
			t.Fatalf("the retirement close was not marked reconciling on the recheck mismatch")
		}
	})
}

// retirementCloseOperation returns the run's one pane.close operation and
// the pid its persisted intent records, failing unless exactly one exists.
func retirementCloseOperation(t *testing.T, tc *testController) (op app.Operation, pid int) {
	t.Helper()
	var found []app.Operation
	for id := range tc.Store.Operations {
		if candidate := tc.Store.Operations[id]; candidate.Kind == app.OpPaneClose {
			found = append(found, candidate)
		}
	}
	if len(found) != 1 {
		t.Fatalf("pane.close operations = %d, want exactly 1", len(found))
	}
	raw, err := json.Marshal(found[0].Intent)
	if err != nil {
		t.Fatalf("marshal pane.close intent: %v", err)
	}
	var intent struct {
		PID    int    `json:"pid"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &intent); err != nil {
		t.Fatalf("unmarshal pane.close intent: %v", err)
	}
	if intent.Reason != "positive-evidence retirement" {
		t.Fatalf("pane.close intent reason = %q, want the positive-evidence retirement", intent.Reason)
	}
	return found[0], intent.PID
}

// TestRetirementRecheckRequiresUniqueCandidate proves the close-time
// recheck of a positive-evidence retirement target re-establishes the
// uniqueness that authorized it, on the very observation the close would
// act on: the recorded restored harness A still matching is not enough
// once a second restored-harness candidate B shares the group. The close
// is never dispatched, the operation stays reconciling with its recorded
// intent and act evidence intact, the report names the pane and both
// candidate pids, and nothing is relaunched — in the authorizing round
// (both member orders) and when the persisted pending retirement is
// replayed by a later resume round or by stop.
func TestRetirementRecheckRequiresUniqueCandidate(t *testing.T) {
	restored := func(pid int, nativeRef string) app.ProcessInfo {
		return app.ProcessInfo{PID: pid, Argv0: "claude", Name: "claude", Argv: []string{"claude", "--resume", nativeRef}}
	}
	mcpChild := app.ProcessInfo{PID: 7840, Argv0: "npm", Name: "npm", Argv: []string{"npm", "exec", "@executeautomation/playwright-mcp-server"}}
	const recheckRefusal = "the retirement close is not dispatched"

	assertAmbiguityReported := func(t *testing.T, text, paneID string) {
		t.Helper()
		for _, want := range []string{recheckRefusal, paneID, "7777", "7778"} {
			if !strings.Contains(text, want) {
				t.Fatalf("report %q, want it to name %q", text, want)
			}
		}
	}
	assertTargetDurable := func(t *testing.T, tc *testController, detail app.RunDetail, wantActEvidence bool) {
		t.Helper()
		op, pid := retirementCloseOperation(t, tc)
		if op.State != app.OperationReconciling {
			t.Fatalf("pane.close operation state = %s, want %s", op.State, app.OperationReconciling)
		}
		if pid != 7777 {
			t.Fatalf("persisted retirement target pid = %d, want the recorded 7777 (never retargeted)", pid)
		}
		if wantActEvidence {
			if evidence, ok := op.ActEvidence.(string); !ok || !strings.Contains(evidence, "pid 7777") {
				t.Fatalf("pane.close act evidence = %v, want the recorded dispatch against pid 7777 kept", op.ActEvidence)
			}
		}
		history := tc.Store.Bindings[detail.SessionID]
		if len(history) != 2 || history[1].Superseded || history[1].Occupant == nil || history[1].Occupant.PID != 7777 {
			t.Fatalf("binding history = %+v, want the observed restoration of pid 7777, unsuperseded", history)
		}
		updated, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.SessionID != detail.SessionID {
			t.Fatalf("a replacement session %s was bound; no relaunch may follow", updated.SessionID)
		}
	}

	orders := map[string]func(a, b app.ProcessInfo) []app.ProcessInfo{
		"second candidate listed after the recorded member": func(a, b app.ProcessInfo) []app.ProcessInfo {
			return []app.ProcessInfo{a, mcpChild, b}
		},
		"second candidate listed before the recorded member": func(a, b app.ProcessInfo) []app.ProcessInfo {
			return []app.ProcessInfo{b, mcpChild, a}
		},
	}
	for name, order := range orders {
		t.Run("authorizing round: "+name, func(t *testing.T) {
			tc := newTestController(defaultPolicy())
			_, detail := runningRun(t, tc)
			nativeRef := tc.Store.Sessions[detail.SessionID].value.NativeSessionRef
			a, b := restored(7777, nativeRef), restored(7778, nativeRef)
			// A alone authorizes the retirement; once its observation binding
			// is recorded, the close procedure's fresh inspection shows B too.
			tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
				tc.Store.mu.Lock()
				recorded := len(tc.Store.Bindings[detail.SessionID]) == 2
				tc.Store.mu.Unlock()
				if recorded {
					return app.PaneProcess{ForegroundGroupID: 7777, Foreground: order(a, b)}, nil
				}
				return app.PaneProcess{ForegroundGroupID: 7777, Foreground: []app.ProcessInfo{a, mcpChild}}, nil
			}

			tc.Clock.Advance(leaseTTL + time.Second)
			result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
			if err != nil {
				t.Fatalf("Resume() error = %v", err)
			}
			if result.Outcome != app.ResumeReconciling {
				t.Fatalf("Outcome = %s (%s), want %s", result.Outcome, result.Detail, app.ResumeReconciling)
			}
			if n := len(tc.Runtime.ClosedPanes); n != 0 {
				t.Fatalf("ClosePane calls = %d, want 0 (an ambiguous recheck never closes)", n)
			}
			assertAmbiguityReported(t, result.Detail, detail.Binding.PaneID)
			assertTargetDurable(t, tc, detail, false)
		})
	}

	// dispatchedRetirement runs the authorizing round against A alone: the
	// close is dispatched but termination is not yet observed, leaving the
	// retirement operation pending with its recorded act evidence.
	dispatchedRetirement := func(t *testing.T, tc *testController) (app.RunDetail, string) {
		t.Helper()
		_, detail := runningRun(t, tc)
		nativeRef := tc.Store.Sessions[detail.SessionID].value.NativeSessionRef
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{ForegroundGroupID: 7777, Foreground: []app.ProcessInfo{restored(7777, nativeRef), mcpChild}}, nil
		}
		tc.Clock.Advance(leaseTTL + time.Second)
		if _, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String())); err != nil {
			t.Fatalf("first Resume() error = %v", err)
		}
		if n := len(tc.Runtime.ClosedPanes); n != 1 {
			t.Fatalf("ClosePane calls after the authorizing round = %d, want 1", n)
		}
		if op, _ := retirementCloseOperation(t, tc); op.State != app.OperationPending {
			t.Fatalf("pane.close operation state = %s, want pending (dispatched, termination unobserved)", op.State)
		}
		return detail, nativeRef
	}

	t.Run("resume replay of the pending retirement", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		detail, nativeRef := dispatchedRetirement(t, tc)
		a, b := restored(7777, nativeRef), restored(7778, nativeRef)
		// The replaying round's own observation still shows A alone; B joins
		// the group from the close procedure's pre-dispatch revalidation on,
		// so the ambiguity is present in the inspection the recheck acts on.
		joined := false
		tc.Store.HeartbeatHook = func() { joined = true }
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			if joined {
				return app.PaneProcess{ForegroundGroupID: 7777, Foreground: []app.ProcessInfo{a, mcpChild, b}}, nil
			}
			return app.PaneProcess{ForegroundGroupID: 7777, Foreground: []app.ProcessInfo{a, mcpChild}}, nil
		}

		tc.Clock.Advance(leaseTTL + time.Second)
		result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
		if err != nil {
			t.Fatalf("second Resume() error = %v", err)
		}
		if result.Outcome != app.ResumeReconciling {
			t.Fatalf("Outcome = %s (%s), want %s", result.Outcome, result.Detail, app.ResumeReconciling)
		}
		if n := len(tc.Runtime.ClosedPanes); n != 1 {
			t.Fatalf("ClosePane calls = %d, want still 1 (no close re-dispatched on an ambiguous recheck)", n)
		}
		assertAmbiguityReported(t, result.Detail, detail.Binding.PaneID)
		assertTargetDurable(t, tc, detail, true)
	})

	t.Run("stop replay of the pending retirement", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		detail, nativeRef := dispatchedRetirement(t, tc)
		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
		tc.Clock.Advance(leaseTTL + time.Second)
		routed, handle, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
		if err != nil {
			t.Fatalf("second Resume() error = %v", err)
		}
		if routed.Outcome != app.ResumeStopPending {
			t.Fatalf("Outcome = %s (%s), want %s", routed.Outcome, routed.Detail, app.ResumeStopPending)
		}

		// Stop faces a group that now holds both A and B. The current binding
		// is the observed restoration, which carries no launch claim, so stop
		// fails closed on its own rule before any close: the pending
		// retirement is neither re-dispatched nor retargeted, and its
		// recorded target and act evidence stay as persisted.
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{ForegroundGroupID: 7777, Foreground: []app.ProcessInfo{restored(7778, nativeRef), mcpChild, restored(7777, nativeRef)}}, nil
		}
		report, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if report.Terminated || len(report.Outstanding) == 0 {
			t.Fatalf("report = %+v; an unretired occupant is never termination", report)
		}
		if n := len(tc.Runtime.ClosedPanes); n != 1 {
			t.Fatalf("ClosePane calls = %d, want still 1 (no close re-dispatched)", n)
		}
		op, pid := retirementCloseOperation(t, tc)
		if op.State != app.OperationPending || pid != 7777 {
			t.Fatalf("pane.close operation = state %s pid %d, want the persisted pending target for pid 7777", op.State, pid)
		}
		if evidence, ok := op.ActEvidence.(string); !ok || !strings.Contains(evidence, "pid 7777") {
			t.Fatalf("pane.close act evidence = %v, want the recorded dispatch against pid 7777 kept", op.ActEvidence)
		}
	})
}

// TestResumeForkingWrapperNeverRetires proves resume preserves the
// forking-wrapper classification on both reconciliation paths instead of
// falling through to positive-evidence retirement: an exec_pending claim
// on a reconciling attempt with ANY wrapper topology, and a settled claim
// whose group holds both the matching claimed process and a matching
// different-pid process, each fail closed with nothing closed, superseded
// or relaunched — in every member order.
func TestResumeForkingWrapperNeverRetires(t *testing.T) {
	claimMember := func(detail app.RunDetail, nativeRef string) app.ProcessInfo {
		return app.ProcessInfo{PID: 4242, Argv0: "claude", Name: "claude", Argv: []string{"/usr/bin/claude", "--session-id", nativeRef, detail.AttemptID.String()}}
	}
	resumeMember := func(nativeRef string) app.ProcessInfo {
		return app.ProcessInfo{PID: 5151, Argv0: "claude", Name: "claude", Argv: []string{"/usr/bin/claude", "--resume", nativeRef}}
	}
	mcpMember := app.ProcessInfo{PID: 4305, Argv0: "npm", Name: "npm", Argv: []string{"npm", "exec", "@executeautomation/playwright-mcp-server"}}

	orders := map[string]func(claim, other app.ProcessInfo) []app.ProcessInfo{
		"claimed process first": func(claim, other app.ProcessInfo) []app.ProcessInfo {
			return []app.ProcessInfo{claim, other}
		},
		"different-pid process first": func(claim, other app.ProcessInfo) []app.ProcessInfo {
			return []app.ProcessInfo{other, claim}
		},
		"both behind an MCP member": func(claim, other app.ProcessInfo) []app.ProcessInfo {
			return []app.ProcessInfo{mcpMember, other, claim}
		},
	}

	assertWrapperFailedClosed := func(t *testing.T, result app.ResumeResult, detail app.RunDetail) {
		t.Helper()
		if result.Outcome != app.ResumeFailedClosed {
			t.Fatalf("Outcome = %s (%s), want %s", result.Outcome, result.Detail, app.ResumeFailedClosed)
		}
		if !strings.Contains(result.Detail, "forking-wrapper") || result.ObservedPaneID != detail.Binding.PaneID {
			t.Fatalf("result = %+v, want the forking-wrapper report naming pane %q", result, detail.Binding.PaneID)
		}
	}

	for name, order := range orders {
		t.Run("settled claim: claimed process and a matching different-pid process, "+name, func(t *testing.T) {
			tc := newTestController(defaultPolicy())
			_, detail := runningRun(t, tc)
			nativeRef := tc.Store.Sessions[detail.SessionID].value.NativeSessionRef
			members := order(claimMember(detail, nativeRef), resumeMember(nativeRef))
			tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
				return app.PaneProcess{ForegroundGroupID: 4242, Foreground: members}, nil
			}

			tc.Clock.Advance(leaseTTL + time.Second)
			result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
			if err != nil {
				t.Fatalf("Resume() error = %v", err)
			}
			assertWrapperFailedClosed(t, result, detail)
			assertNothingRetired(t, tc, detail)
			if got := tc.Store.LaunchClaims[detail.Binding.IncarnationID].State; got != app.LaunchClaimExeced {
				t.Fatalf("claim state = %s, want %s unchanged", got, app.LaunchClaimExeced)
			}
			if got := tc.Store.Attempts[detail.AttemptID].value.State; got != run.AttemptReconciling {
				t.Fatalf("Attempt.State = %s, want %s (never warm-adopted)", got, run.AttemptReconciling)
			}
		})
	}

	// execPendingOnReconciling drives a launch whose claim was never written
	// into reconciling (an unidentified pre-exec occupant fails closed), then
	// writes the exec_pending claim, as TestReconciliationClaimSettlement does.
	execPendingOnReconciling := func(t *testing.T, tc *testController) app.RunDetail {
		t.Helper()
		_, detail := startedRun(t, tc)
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "hop", Name: "hop", Argv: []string{"/usr/bin/hop", "launch", "--attempt", detail.AttemptID.String()}}}}, nil
		}
		tc.Clock.Advance(leaseTTL + time.Second)
		if _, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String())); err != nil {
			t.Fatalf("first Resume() error = %v", err)
		}
		if got := tc.Store.Attempts[detail.AttemptID].value.State; got != run.AttemptReconciling {
			t.Fatalf("Attempt.State = %s, want %s", got, run.AttemptReconciling)
		}
		claimLaunch(t, tc, detail, 4242)
		return detail
	}
	execPendingCases := map[string]func(detail app.RunDetail, nativeRef string) []app.ProcessInfo{}
	for name, order := range orders {
		execPendingCases["claimed process and a matching different-pid process, "+name] = func(detail app.RunDetail, nativeRef string) []app.ProcessInfo {
			return order(claimMember(detail, nativeRef), resumeMember(nativeRef))
		}
	}
	execPendingCases["only a matching different-pid process"] = func(_ app.RunDetail, nativeRef string) []app.ProcessInfo {
		return []app.ProcessInfo{mcpMember, resumeMember(nativeRef)}
	}
	for name, members := range execPendingCases {
		t.Run("exec_pending claim on a reconciling attempt: "+name, func(t *testing.T) {
			tc := newTestController(defaultPolicy())
			detail := execPendingOnReconciling(t, tc)
			nativeRef := tc.Store.Sessions[detail.SessionID].value.NativeSessionRef
			observed := members(detail, nativeRef)
			tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
				return app.PaneProcess{ForegroundGroupID: 4242, Foreground: observed}, nil
			}

			tc.Clock.Advance(leaseTTL + time.Second)
			result, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
			if err != nil {
				t.Fatalf("second Resume() error = %v", err)
			}
			assertWrapperFailedClosed(t, result, detail)
			assertNothingRetired(t, tc, detail)
			if got := tc.Store.LaunchClaims[detail.Binding.IncarnationID].State; got != app.LaunchClaimExecPending {
				t.Fatalf("claim state = %s, want %s (the wrapper topology never settles)", got, app.LaunchClaimExecPending)
			}
			if got := tc.Store.Attempts[detail.AttemptID].value.State; got != run.AttemptReconciling {
				t.Fatalf("Attempt.State = %s, want %s", got, run.AttemptReconciling)
			}
		})
	}
}

// TestReconciliationClaimSettlement is the M4 pending→reconciling→
// claim-appears trace: a launch that went ambiguous (attempt reconciling)
// is settled once the claim appears and its worker corroborates under the
// inspected predicate, instead of staying stuck or being mistaken for a
// restoration.
func TestReconciliationClaimSettlement(t *testing.T) {
	tc := newTestController(defaultPolicy())
	_, detail := startedRun(t, tc)

	// Round 1: an unidentified occupant with no claim fails closed and the
	// attempt enters reconciling.
	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "hop", Name: "hop", Argv: []string{"/usr/bin/hop", "launch", "--attempt", detail.AttemptID.String()}}}}, nil
	}
	tc.Clock.Advance(leaseTTL + time.Second)
	first, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
	if err != nil {
		t.Fatalf("first Resume() error = %v", err)
	}
	if first.Outcome != app.ResumeFailedClosed {
		t.Fatalf("first Outcome = %s, want %s", first.Outcome, app.ResumeFailedClosed)
	}
	if got := tc.Store.Attempts[detail.AttemptID].value.State; got != run.AttemptReconciling {
		t.Fatalf("Attempt.State = %s, want %s", got, run.AttemptReconciling)
	}

	// The launcher then writes its claim and execs; the live worker — with
	// its MCP-server children already spawned into its own process group,
	// so it is NOT index 0 of the raw listing — now corroborates under the
	// predicate on the next round.
	claimLaunch(t, tc, detail, 4242)
	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return mcpGroupPane(4242, "/usr/bin/claude", detail.AttemptID.String()), nil
	}
	tc.Clock.Advance(leaseTTL + time.Second)
	second, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
	if err != nil {
		t.Fatalf("second Resume() error = %v", err)
	}
	if second.Outcome != app.ResumeWarmReattached {
		t.Fatalf("second Outcome = %s, want %s", second.Outcome, app.ResumeWarmReattached)
	}
	claim := tc.Store.LaunchClaims[detail.Binding.IncarnationID]
	if claim.State != app.LaunchClaimExeced {
		t.Fatalf("claim state = %s, want %s (settled from reconciliation)", claim.State, app.LaunchClaimExeced)
	}
	if !strings.Contains(claim.SettlementEvidence, "pid=4242") {
		t.Fatalf("settlement evidence %q must record the corroborated member's pid 4242, never an MCP sibling's", claim.SettlementEvidence)
	}
	updated, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus() error = %v", err)
	}
	if updated.State != run.RunRunning || updated.AttemptState != run.AttemptRunning {
		t.Fatalf("Run/Attempt = %s/%s, want running/running", updated.State, updated.AttemptState)
	}
}

// TestResumeRoutesHeldStop is the M4 stop-requested→Resume→DriveStop
// trace: a run holding a stop request is never restored to running by
// resume; it is routed to stop handling, which retires the worker and
// observes termination.
func TestResumeRoutesHeldStop(t *testing.T) {
	tc := newTestController(defaultPolicy())
	_, detail := runningRun(t, tc)
	if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
		t.Fatalf("RequestStop() error = %v", err)
	}

	tc.Clock.Advance(leaseTTL + time.Second)
	result, handle, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if result.Outcome != app.ResumeStopPending {
		t.Fatalf("Outcome = %s, want %s", result.Outcome, app.ResumeStopPending)
	}
	if got := tc.Store.Runs[detail.RunID].value.State; got != run.RunStopping {
		t.Fatalf("Run.State = %s, want %s (never turned back into resuming)", got, run.RunStopping)
	}

	// Stop handling then retires the worker and observes termination.
	report, err := tc.Controller.DriveStop(context.Background(), handle)
	if err != nil {
		t.Fatalf("DriveStop() error = %v", err)
	}
	if report.Terminated {
		t.Fatalf("report = %+v; the dispatched close is not yet observed termination", report)
	}
	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{}, app.ErrPaneNotFound
	}
	final, err := tc.Controller.DriveStop(context.Background(), handle)
	if err != nil {
		t.Fatalf("second DriveStop() error = %v", err)
	}
	if !final.Terminated || final.RunState != string(run.RunStopped) {
		t.Fatalf("final report = %+v, want terminated/stopped", final)
	}
}

// TestStopRacingResumeEntry proves the entry barrier: a stop request
// recorded between resume's lease-free entry read and its entry
// transaction is observed under the transaction, the run is never moved
// toward resuming, and Resume→DriveStop completes with no further user
// stop request.
func TestStopRacingResumeEntry(t *testing.T) {
	tc := newTestController(defaultPolicy())
	_, detail := runningRun(t, tc)

	fired := false
	tc.Store.LoadRunStatusHook = func() {
		if fired {
			return
		}
		fired = true
		// The worker-authority stop lands right after the entry read.
		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Errorf("RequestStop() error = %v", err)
		}
	}

	tc.Clock.Advance(leaseTTL + time.Second)
	result, handle, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if !fired {
		t.Fatalf("the race hook never fired; the scenario did not exercise the entry window")
	}
	if result.Outcome != app.ResumeStopPending {
		t.Fatalf("Outcome = %s, want %s", result.Outcome, app.ResumeStopPending)
	}
	if got := tc.Store.Runs[detail.RunID].value.State; got != run.RunStopping {
		t.Fatalf("Run.State = %s, want %s (never moved toward resuming over the raced stop)", got, run.RunStopping)
	}

	// DriveStop completes with no further user stop request.
	report, err := tc.Controller.DriveStop(context.Background(), handle)
	if err != nil {
		t.Fatalf("DriveStop() error = %v", err)
	}
	if report.Terminated {
		t.Fatalf("report = %+v; the dispatched close is not yet observed termination", report)
	}
	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{}, app.ErrPaneNotFound
	}
	final, err := tc.Controller.DriveStop(context.Background(), handle)
	if err != nil {
		t.Fatalf("second DriveStop() error = %v", err)
	}
	if !final.Terminated || final.RunState != string(run.RunStopped) {
		t.Fatalf("final report = %+v, want terminated/stopped", final)
	}
}

// TestResumeDrivesTerminalUnknownRetirement is the Resume-only trace for
// an unrepeatable lost check: takeover recovery settles the unknown
// execution and fails the attempt, the FIRST resume round retires the
// worker without terminal run failure and reports the outstanding
// retirement (never NothingToDo over a live worker), and the next round
// fails the run on observed absence — no ClaimAndRunCheck call involved.
func TestResumeDrivesTerminalUnknownRetirement(t *testing.T) {
	tc := newTestController(defaultPolicy())
	handle, detail := runningRun(t, tc)
	if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
		t.Fatalf("SubmitResult() error = %v", err)
	}
	// Controller A claims the check, spawns, the child writes its claim,
	// and A dies with the execution unresolved.
	tc.Commands.CheckExecFn = func(ctx context.Context, cmd app.Command) (app.CommandResult, error) {
		if err := tc.Store.ClaimCheckExec(ctx, checkExecOpID(t, cmd), 5150); err != nil {
			t.Errorf("ClaimCheckExec() error = %v", err)
		}
		return app.CommandResult{}, context.DeadlineExceeded
	}
	if _, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil); err == nil {
		t.Fatalf("ClaimAndRunCheck() succeeded despite the lost execution")
	}
	tc.Commands.CheckExecFn = nil

	// Controller B's FIRST resume round: recovery settles the unknown
	// execution (group observed empty), fails task and attempt, and the
	// live worker's retirement is dispatched — the run stays non-terminal
	// and the outcome names the outstanding retirement.
	tc.Clock.Advance(leaseTTL + time.Second)
	first, _, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
	if err != nil {
		t.Fatalf("first Resume() error = %v", err)
	}
	if first.Outcome == app.ResumeNothingToDo {
		t.Fatalf("first Outcome = %s; never NothingToDo over a live worker", first.Outcome)
	}
	if first.Outcome != app.ResumeReconciling {
		t.Fatalf("first Outcome = %s, want %s naming the outstanding retirement", first.Outcome, app.ResumeReconciling)
	}
	updated, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus() error = %v", err)
	}
	if updated.AttemptState != run.AttemptFailed {
		t.Fatalf("Attempt.State = %s, want %s", updated.AttemptState, run.AttemptFailed)
	}
	if updated.State == run.RunFailed {
		t.Fatalf("Run.State = %s; the run must not fail before its worker's observed termination", updated.State)
	}
	if len(tc.Runtime.ClosedPanes) != 1 {
		t.Fatalf("ClosePane calls = %d, want the worker retirement dispatched once", len(tc.Runtime.ClosedPanes))
	}

	// The next round observes the worker gone and fails the run.
	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{}, app.ErrPaneNotFound
	}
	tc.Clock.Advance(leaseTTL + time.Second)
	if _, _, secondErr := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String())); secondErr != nil {
		t.Fatalf("second Resume() error = %v", secondErr)
	}
	final, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus() error = %v", err)
	}
	if final.State != run.RunFailed {
		t.Fatalf("Run.State = %s, want %s after the worker's observed termination", final.State, run.RunFailed)
	}
	if got := tc.Store.Sessions[detail.SessionID].value.State; got != run.SessionTerminated {
		t.Fatalf("Session.State = %s, want %s", got, run.SessionTerminated)
	}
}
