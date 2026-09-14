package app_test

import (
	"context"
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
			return app.PaneProcess{}, nil // no live process: genuinely absent
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
			return app.PaneProcess{}, nil
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
