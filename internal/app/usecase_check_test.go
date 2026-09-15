package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// checkExecOpID extracts the --op operation id from a hop check-exec argv.
func checkExecOpID(t *testing.T, cmd app.Command) identity.OperationID {
	t.Helper()
	for i, tok := range cmd.Argv {
		if tok == "--op" && i+1 < len(cmd.Argv) {
			opID, err := identity.ParseOperationID(cmd.Argv[i+1])
			if err != nil {
				t.Fatalf("parse --op value %q: %v", cmd.Argv[i+1], err)
			}
			return opID
		}
	}
	t.Fatalf("no --op flag in check-exec argv %v", cmd.Argv)
	return ""
}

func TestClaimAndRunCheck(t *testing.T) {
	t.Run("no pending check request: a no-op", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, _ := runningRun(t, tc)
		report, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil)
		if err != nil {
			t.Fatalf("ClaimAndRunCheck() error = %v", err)
		}
		if report.Ran {
			t.Fatalf("report = %+v, want Ran=false with no pending request", report)
		}
	})

	t.Run("passing check completes Run, Task and Attempt", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}

		report, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil)
		if err != nil {
			t.Fatalf("ClaimAndRunCheck() error = %v", err)
		}
		if !report.Ran || !report.Passed {
			t.Fatalf("report = %+v, want Ran/Passed", report)
		}

		updated, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.State != run.RunCompleted || updated.TaskState != run.TaskCompleted || updated.AttemptState != run.AttemptCompleted {
			t.Fatalf("Run/Task/Attempt = %s/%s/%s, want all completed", updated.State, updated.TaskState, updated.AttemptState)
		}
	})

	t.Run("failing check fails Run, Task and Attempt, never completes, and retains its evidence", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		tc.Commands.CheckExecFn = func(_ context.Context, _ app.Command) (app.CommandResult, error) {
			return app.CommandResult{ExitCode: 1, Stdout: []byte("check output"), Stderr: []byte("check failure")}, nil
		}

		report, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil)
		if err != nil {
			t.Fatalf("ClaimAndRunCheck() error = %v", err)
		}
		if !report.Ran || report.Passed {
			t.Fatalf("report = %+v, want Ran and not Passed", report)
		}

		updated, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.State != run.RunFailed || updated.TaskState != run.TaskFailed || updated.AttemptState != run.AttemptFailed {
			t.Fatalf("Run/Task/Attempt = %s/%s/%s, want all failed", updated.State, updated.TaskState, updated.AttemptState)
		}

		// The stdout/stderr evidence outlives the failure: files written
		// through the ArtifactStore, result-linked rows with digests, and a
		// status summary naming the execution and its evidence.
		var stdoutRows, stderrRows int
		for _, artifact := range updated.Artifacts {
			switch artifact.Kind {
			case run.ArtifactCheckStdout:
				stdoutRows++
				if artifact.ResultID == nil || artifact.Digest == "" {
					t.Fatalf("stdout artifact is not result-linked with a digest: %+v", artifact)
				}
				content, readErr := tc.Artifacts.ReadArtifact(context.Background(), artifact.Path)
				if readErr != nil || string(content) != "check output" {
					t.Fatalf("stdout evidence = %q (err %v), want the captured output", content, readErr)
				}
			case run.ArtifactCheckStderr:
				stderrRows++
			}
		}
		if stdoutRows != 1 || stderrRows != 1 {
			t.Fatalf("stdout/stderr artifact rows = %d/%d, want 1/1", stdoutRows, stderrRows)
		}
		if updated.LastCheck == nil || updated.LastCheck.OperationID.String() != report.OperationID {
			t.Fatalf("status does not name the check execution: %+v", updated.LastCheck)
		}
		if len(updated.LastCheck.EvidencePaths) < 2 {
			t.Fatalf("status names %d evidence paths, want the retained stdout and stderr", len(updated.LastCheck.EvidencePaths))
		}

		view, err := tc.Controller.Status(context.Background(), app.StatusRequest{RepositoryRoot: "/repo", RunID: detail.RunID.String()})
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		if view.Detail == nil || view.Detail.LastCheckOperation != report.OperationID {
			t.Fatalf("status view does not name the check execution")
		}
	})

	t.Run("the tree object id is frozen into the check intent", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		if _, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil); err != nil {
			t.Fatalf("ClaimAndRunCheck() error = %v", err)
		}
		found := false
		for id := range tc.Store.Operations {
			op := tc.Store.Operations[id]
			if op.Kind != app.OpCheckRun {
				continue
			}
			intent, isMap := op.Intent.(map[string]any)
			if !isMap {
				t.Fatalf("check intent shape = %T, want a JSON object", op.Intent)
			}
			if tree, isString := intent["tree_oid"].(string); !isString || tree != "tttttttttttttttttttttttttttttttttttttttt" {
				t.Fatalf("intent tree_oid = %v, want the resolved candidate tree", intent["tree_oid"])
			}
			found = true
		}
		if !found {
			t.Fatalf("no check operation was journaled")
		}
	})

	t.Run("spawn error is ambiguous: reconciling, recovered as unknown only after the group is observed absent", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		// The child reached its pre-exec claim write before the controller
		// lost it (as the real hop check-exec does before exec).
		tc.Commands.CheckExecFn = func(ctx context.Context, cmd app.Command) (app.CommandResult, error) {
			opID := checkExecOpID(t, cmd)
			if err := tc.Store.ClaimCheckExec(ctx, opID, 5150); err != nil {
				t.Errorf("ClaimCheckExec() error = %v", err)
			}
			return app.CommandResult{}, context.DeadlineExceeded
		}

		report, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil)
		if err == nil {
			t.Fatalf("ClaimAndRunCheck() succeeded despite an ambiguous spawn error")
		}
		if !report.Ran || report.Unknown {
			t.Fatalf("report = %+v, want Ran and not (yet) Unknown: a spawn error is ambiguous, never an immediate unknown", report)
		}
		if updated, loadErr := tc.Store.LoadRunStatus(context.Background(), detail.RunID); loadErr != nil || updated.State != run.RunCompleting {
			t.Fatalf("Run.State = %s (err %v), want %s (no unknown outcome was declared)", updated.State, loadErr, run.RunCompleting)
		}

		// The next round retires the claimed group; its observed absence
		// (the default empty listing) permits the unknown-outcome rule,
		// which fails the task and attempt — but the run fails only after
		// its worker's termination is observed, so this round dispatches
		// the worker close and leaves the run untouched.
		tc.Commands.CheckExecFn = nil
		blockedReport, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil)
		if err != nil {
			t.Fatalf("second ClaimAndRunCheck() error = %v", err)
		}
		if blockedReport.Ran {
			t.Fatalf("report = %+v, want no new work after the terminal unknown outcome", blockedReport)
		}
		updated, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.AttemptState != run.AttemptFailed {
			t.Fatalf("Attempt.State = %s, want %s", updated.AttemptState, run.AttemptFailed)
		}
		if updated.State == run.RunFailed || updated.State == run.RunCompleted {
			t.Fatalf("Run.State = %s; the run must not go terminal while its worker may be live", updated.State)
		}
		if len(tc.Runtime.ClosedPanes) != 1 {
			t.Fatalf("ClosePane calls = %d, want the worker close dispatched once", len(tc.Runtime.ClosedPanes))
		}
		for _, cr := range tc.Store.CheckRequests {
			if cr.State != app.CheckRequestSettled {
				t.Fatalf("check request state = %s, want settled after the terminal unknown outcome", cr.State)
			}
		}

		// Once the worker's termination is observed, the run fails.
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		if _, thirdErr := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil); thirdErr != nil {
			t.Fatalf("third ClaimAndRunCheck() error = %v", thirdErr)
		}
		final, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if final.State != run.RunFailed {
			t.Fatalf("Run.State = %s, want %s after the worker's observed termination", final.State, run.RunFailed)
		}
	})

	t.Run("repeatable unknown outcome requeues and a second execution completes the run", func(t *testing.T) {
		policy := defaultPolicy()
		policy.CheckRepeatable = true
		tc := newTestController(policy)
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		tc.Commands.CheckExecFn = func(ctx context.Context, cmd app.Command) (app.CommandResult, error) {
			opID := checkExecOpID(t, cmd)
			if err := tc.Store.ClaimCheckExec(ctx, opID, 5150); err != nil {
				t.Errorf("ClaimCheckExec() error = %v", err)
			}
			return app.CommandResult{}, context.DeadlineExceeded
		}
		if _, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil); err == nil {
			t.Fatalf("ClaimAndRunCheck() succeeded despite an ambiguous spawn error")
		}

		// The next round retires the lost group (observed empty), requeues
		// the request under the frozen repeatable contract, and runs the
		// fresh execution to a passing completion in the same call.
		tc.Commands.CheckExecFn = nil
		report, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil)
		if err != nil {
			t.Fatalf("second ClaimAndRunCheck() error = %v", err)
		}
		if !report.Ran || !report.Passed {
			t.Fatalf("report = %+v, want a fresh passing execution after the requeue", report)
		}
		var checkOps, failedUnknown int
		for id := range tc.Store.Operations {
			op := tc.Store.Operations[id]
			if op.Kind != app.OpCheckRun {
				continue
			}
			checkOps++
			if op.State == app.OperationFailed {
				failedUnknown++
			}
		}
		if checkOps != 2 || failedUnknown != 1 {
			t.Fatalf("check operations = %d (failed %d), want two executions with the first settled unknown", checkOps, failedUnknown)
		}
		updated, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.State != run.RunCompleted {
			t.Fatalf("Run.State = %s, want %s", updated.State, run.RunCompleted)
		}
	})

	t.Run("frozen inputs: the spawn uses the frozen argv and the frozen timeout bounds the command", func(t *testing.T) {
		policy := defaultPolicy()
		policy.CheckTimeout = 7 * time.Minute
		tc := newTestController(policy)
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		var deadline time.Time
		var spawned []string
		tc.Commands.CheckExecFn = func(ctx context.Context, cmd app.Command) (app.CommandResult, error) {
			if d, ok := ctx.Deadline(); ok {
				deadline = d
			}
			spawned = cmd.Argv
			return app.CommandResult{ExitCode: 0}, nil
		}

		if _, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil); err != nil {
			t.Fatalf("ClaimAndRunCheck() error = %v", err)
		}
		if deadline.IsZero() {
			t.Fatalf("the check command context carries no deadline; the frozen timeout was not propagated")
		}
		// context.WithTimeout runs on the wall clock; the deadline must sit
		// within the frozen 7m window of the real time of the call.
		if remaining := time.Until(deadline); remaining > 7*time.Minute || remaining <= 0 {
			t.Fatalf("deadline in %v, want within the frozen 7m timeout", remaining)
		}
		wantSuffix := []string{"--", "sh", "check.sh"}
		if len(spawned) < len(wantSuffix) {
			t.Fatalf("spawn argv = %v, want the frozen check argv", spawned)
		}
		for i, tok := range wantSuffix {
			if spawned[len(spawned)-len(wantSuffix)+i] != tok {
				t.Fatalf("spawn argv = %v, want it to end with the frozen %v", spawned, wantSuffix)
			}
		}
	})

	t.Run("a submodule candidate is rejected before any spawn", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		tc.Commands.Results["git -C /repo ls-tree -r cccccccccccccccccccccccccccccccccccccccc"] = app.CommandResult{
			ExitCode: 0,
			Stdout:   []byte("100644 blob aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\tmain.go\n160000 commit bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\tvendor/dep\n"),
		}

		report, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil)
		if err != nil {
			t.Fatalf("ClaimAndRunCheck() error = %v", err)
		}
		if !report.Ran || report.Passed {
			t.Fatalf("report = %+v, want a clear failure for the submodule candidate", report)
		}
		for _, cmd := range tc.Commands.Calls {
			if len(cmd.Argv) >= 2 && cmd.Argv[1] == "check-exec" {
				t.Fatalf("hop check-exec was spawned despite the submodule rejection")
			}
		}
		updated, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.State != run.RunFailed {
			t.Fatalf("Run.State = %s, want %s (a submodule tree fails clearly)", updated.State, run.RunFailed)
		}
	})

	t.Run("stop precedence: a stop requested during the check interrupts rather than completes", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		// The stop request arrives while the check command is executing: the
		// outcome transaction re-reads the stop flag and applies precedence.
		tc.Commands.CheckExecFn = func(ctx context.Context, _ app.Command) (app.CommandResult, error) {
			if err := tc.Controller.RequestStop(ctx, detail.RunID.String()); err != nil {
				t.Errorf("RequestStop() during check error = %v", err)
			}
			return app.CommandResult{ExitCode: 0}, nil
		}

		report, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil)
		if err != nil {
			t.Fatalf("ClaimAndRunCheck() error = %v", err)
		}
		if !report.Ran || !report.Interrupted || report.Passed {
			t.Fatalf("report = %+v, want Ran/Interrupted and not Passed (stop precedence)", report)
		}

		updated, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.State != run.RunStopping {
			t.Fatalf("Run.State = %s, want %s: the check outcome never declares stopped while the worker may be live", updated.State, run.RunStopping)
		}
		if updated.AttemptState != run.AttemptInterrupted || updated.TaskState != run.TaskInterrupted {
			t.Fatalf("Attempt/Task = %s/%s, want both interrupted", updated.AttemptState, updated.TaskState)
		}

		// DriveStop then observes the worker gone and completes the stop.
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		final, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("DriveStop() error = %v", err)
		}
		if !final.Terminated || final.RunState != string(run.RunStopped) {
			t.Fatalf("final report = %+v, want terminated/stopped", final)
		}
	})

	t.Run("a stop request already recorded refuses to start unstarted check work", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}

		report, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil)
		if err != nil {
			t.Fatalf("ClaimAndRunCheck() error = %v", err)
		}
		if report.Ran {
			t.Fatalf("report = %+v, want Ran=false: unstarted work is never started after stop", report)
		}
		for _, cmd := range tc.Commands.Calls {
			if len(cmd.Argv) >= 2 && cmd.Argv[1] == "check-exec" {
				t.Fatalf("hop check-exec was spawned despite the stop request")
			}
		}
		for _, cr := range tc.Store.CheckRequests {
			if cr.State != app.CheckRequestRequested {
				t.Fatalf("check request state = %s, want requested (left for stop handling)", cr.State)
			}
		}
	})
}

// TestResumeDuringCheck is the section 9 resume-during-check takeover
// scenario (gap c): a controller dies while a claimed check execution
// runs; the successor's resume reads the check-exec claim, retires the
// group under the group-retirement rule, warm-reattaches the worker, and
// — with the repeatable contract — requeues and completes a fresh
// execution against the same accepted result.
func TestResumeDuringCheck(t *testing.T) {
	policy := defaultPolicy()
	policy.CheckRepeatable = true
	tc := newTestController(policy)
	handle, detail := runningRun(t, tc)
	if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
		t.Fatalf("SubmitResult() error = %v", err)
	}

	// Controller A claims the request, journals the execution and spawns
	// hop check-exec, which writes its claim; A then dies with the check
	// still running.
	tc.Commands.CheckExecFn = func(ctx context.Context, cmd app.Command) (app.CommandResult, error) {
		opID := checkExecOpID(t, cmd)
		if err := tc.Store.ClaimCheckExec(ctx, opID, 5150); err != nil {
			t.Errorf("ClaimCheckExec() error = %v", err)
		}
		return app.CommandResult{}, context.DeadlineExceeded
	}
	if _, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil); err == nil {
		t.Fatalf("ClaimAndRunCheck() succeeded despite the lost execution")
	}
	tc.Commands.CheckExecFn = nil
	tc.Groups.Processes[5150] = []app.GroupProcess{{PID: 5150, Argv: []string{"sh", "check.sh"}}}

	// Controller B resumes: recovery reads the check-exec claim and
	// signals the still-matching group; the worker warm-reattaches.
	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 4242, Argv0: "/usr/bin/claude", Argv: []string{"claude", detail.AttemptID.String()}}}}, nil
	}
	tc.Clock.Advance(leaseTTL + time.Second)
	result, newHandle, err := tc.Controller.Resume(context.Background(), defaultResumeRequest(detail.RunID.String()))
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if result.Outcome != app.ResumeWarmReattached {
		t.Fatalf("Outcome = %s, want %s", result.Outcome, app.ResumeWarmReattached)
	}
	if len(tc.Groups.Signaled) != 1 || tc.Groups.Signaled[0] != 5150 {
		t.Fatalf("Signaled = %v, want [5150]: resume must retire the claimed check group", tc.Groups.Signaled)
	}

	// The group is observed gone: the unknown-outcome rule requeues the
	// repeatable request and a fresh execution completes the run.
	tc.Groups.Processes[5150] = nil
	report, err := tc.Controller.ClaimAndRunCheck(context.Background(), newHandle, "/usr/local/bin/hop", nil)
	if err != nil {
		t.Fatalf("ClaimAndRunCheck() after takeover error = %v", err)
	}
	if !report.Ran || !report.Passed {
		t.Fatalf("report = %+v, want a fresh passing execution after the requeue", report)
	}
	updated, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus() error = %v", err)
	}
	if updated.State != run.RunCompleted {
		t.Fatalf("Run.State = %s, want %s", updated.State, run.RunCompleted)
	}
	for _, cr := range tc.Store.CheckRequests {
		if cr.State != app.CheckRequestSettled {
			t.Fatalf("check request state = %s, want settled", cr.State)
		}
	}
}

// TestOrphanedClaimedCheckRequest proves a request a dead generation
// claimed without a recoverable execution — the crash window the atomic
// claim-plus-intent transaction has since closed — is reopened.
func TestOrphanedClaimedCheckRequest(t *testing.T) {
	tc := newTestController(defaultPolicy())
	handle, detail := runningRun(t, tc)
	if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
		t.Fatalf("SubmitResult() error = %v", err)
	}
	// A prior generation claimed the request but died before journaling
	// the execution (an older controller's non-atomic window).
	priorGeneration := tc.Store.Leases[detail.RunID].lease.Generation - 1
	for id, cr := range tc.Store.CheckRequests {
		cr.State = app.CheckRequestClaimed
		cr.ClaimedGeneration = &priorGeneration
		tc.Store.CheckRequests[id] = cr
	}

	report, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil)
	if err != nil {
		t.Fatalf("ClaimAndRunCheck() error = %v", err)
	}
	if !report.Ran || !report.Passed {
		t.Fatalf("report = %+v, want the reopened request claimed and run to completion", report)
	}
}

// TestStopPrecedenceInUnknownRecovery covers M7's stop-side windows: a
// stop request held during unknown-outcome recovery interrupts rather
// than terminally fails, and a stop-retired check execution settles its
// request.
func TestStopPrecedenceInUnknownRecovery(t *testing.T) {
	t.Run("stopped during unknown recovery: interrupted, request settled, stop completes", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
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
		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}

		// Recovery under the held stop: the group's observed absence
		// settles the execution as unknown, interrupts task and attempt,
		// and leaves the run to stop handling.
		if _, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil); err != nil {
			t.Fatalf("recovery ClaimAndRunCheck() error = %v", err)
		}
		updated, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.AttemptState != run.AttemptInterrupted || updated.TaskState != run.TaskInterrupted {
			t.Fatalf("Attempt/Task = %s/%s, want both interrupted (stop precedence)", updated.AttemptState, updated.TaskState)
		}
		if updated.State != run.RunStopping {
			t.Fatalf("Run.State = %s, want %s (left for stop handling)", updated.State, run.RunStopping)
		}
		for _, cr := range tc.Store.CheckRequests {
			if cr.State != app.CheckRequestSettled {
				t.Fatalf("check request state = %s, want settled", cr.State)
			}
		}

		// Stop handling then retires the worker and observes termination.
		if _, driveErr := tc.Controller.DriveStop(context.Background(), handle); driveErr != nil {
			t.Fatalf("DriveStop() error = %v", driveErr)
		}
		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		final, err := tc.Controller.DriveStop(context.Background(), handle)
		if err != nil {
			t.Fatalf("final DriveStop() error = %v", err)
		}
		if !final.Terminated {
			t.Fatalf("final report = %+v, want terminated", final)
		}
	})

	t.Run("a stop-retired check settles its request through DriveStop", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		tc.Commands.CheckExecFn = func(ctx context.Context, cmd app.Command) (app.CommandResult, error) {
			if err := tc.Store.ClaimCheckExec(ctx, checkExecOpID(t, cmd), 5150); err != nil {
				t.Errorf("ClaimCheckExec() error = %v", err)
			}
			return app.CommandResult{}, context.DeadlineExceeded
		}
		if _, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil); err == nil {
			t.Fatalf("ClaimAndRunCheck() succeeded despite the lost execution")
		}
		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}

		// DriveStop retires the claimed group (observed empty) and settles
		// both the operation and its request.
		if _, driveErr := tc.Controller.DriveStop(context.Background(), handle); driveErr != nil {
			t.Fatalf("DriveStop() error = %v", driveErr)
		}
		for _, cr := range tc.Store.CheckRequests {
			if cr.State != app.CheckRequestSettled {
				t.Fatalf("check request state = %s, want settled by the stop-retired execution", cr.State)
			}
		}
	})
}

// TestBlockedCheckRefusesColdRelaunch is the M7 cold branch: an
// uninspectable or foreign old check group blocks a newly authorized cold
// worker launch.
func TestBlockedCheckRefusesColdRelaunch(t *testing.T) {
	tc := newTestController(defaultPolicy())
	handle, detail := runningRun(t, tc)
	if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
		t.Fatalf("SubmitResult() error = %v", err)
	}
	tc.Commands.CheckExecFn = func(ctx context.Context, cmd app.Command) (app.CommandResult, error) {
		if err := tc.Store.ClaimCheckExec(ctx, checkExecOpID(t, cmd), 5150); err != nil {
			t.Errorf("ClaimCheckExec() error = %v", err)
		}
		return app.CommandResult{}, context.DeadlineExceeded
	}
	if _, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", nil); err == nil {
		t.Fatalf("ClaimAndRunCheck() succeeded despite the lost execution")
	}
	// The recorded group now holds a foreign process: retirement stays
	// blocked, never signaled.
	tc.Groups.Processes[5150] = []app.GroupProcess{{PID: 6000, Argv: []string{"unrelated"}}}
	// The worker pane is positively gone and the human attests absence.
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
		t.Fatalf("Outcome = %s, want %s (an unresolved check group blocks the cold relaunch)", result.Outcome, app.ResumeReconciling)
	}
	if got := tc.Store.Attempts[detail.AttemptID].value.State; got == run.AttemptRelaunching {
		t.Fatalf("the attempt relaunched over an unresolved check group")
	}
}
