package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// TestAttemptLaunchStopRefusal proves a stop refusing a child's dispatch
// ends the pass without an error and records the intent as refused before
// dispatch — for the worktree.create and for the pane.open — so stop
// resolves it as never dispatched and reaches stopped in its first round,
// with no bounded wait and no outstanding launch.
func TestAttemptLaunchStopRefusal(t *testing.T) {
	for _, tt := range []struct {
		name string
		kind app.OperationKind
		// heartbeat is the dispatch revalidation's heartbeat the stop lands
		// in: the first is the worktree.create's, the second the pane.open's.
		heartbeat int
	}{
		{"the worktree.create dispatch", app.OpWorktreeCreate, 1},
		{"the pane.open dispatch", app.OpPaneOpen, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newLaunchFixture(t, 2, 3)
			heartbeats := 0
			f.tc.Store.HeartbeatHook = func() {
				heartbeats++
				if heartbeats == tt.heartbeat {
					f.tc.Store.mu.Lock()
					rRow := f.tc.Store.Runs[f.fr.RunID]
					rRow.value = rRow.value.RequestStop(f.tc.Clock.Now())
					rRow.revision++
					f.tc.Store.mu.Unlock()
				}
			}
			report := f.assign(t)
			f.tc.Store.HeartbeatHook = nil
			launch := launchFor(t, &report, 1)
			if launch.Disposition != app.AttemptLaunchStopRequested || len(report.Launches) != 1 {
				t.Fatalf("report = %+v, want t1 stop-requested and nothing further", report)
			}
			assertPathFree(t, launch.Detail)
			ops := f.ops(tt.kind)
			if len(ops) != 1 || ops[0].State != app.OperationFailed {
				t.Fatalf("%s operations = %+v, want one recorded failed", tt.kind, ops)
			}
			var refusal struct {
				Condition             string `json:"condition"`
				RefusedBeforeDispatch bool   `json:"refused_before_dispatch"`
			}
			decodeInto(t, ops[0].Outcome, &refusal)
			if refusal.Condition != "refused-before-dispatch" && !refusal.RefusedBeforeDispatch {
				t.Fatalf("%s outcome = %+v, want refused before dispatch", tt.kind, ops[0].Outcome)
			}
			session := f.child(t, f.tasks[0])
			if tt.kind == app.OpWorktreeCreate && len(f.tc.Runtime.CreateWorktreeRequests) != 0 {
				t.Fatalf("the refused worktree.create was dispatched")
			}
			if _, bound := f.tc.Store.currentBindingLocked(session.ID); bound {
				t.Fatalf("the refused pane.open left a binding")
			}

			f.tc.Runtime.InspectPaneFn = allPanesAbsent
			stop, err := f.tc.Controller.DriveFeatureStop(context.Background(), f.handle)
			if err != nil {
				t.Fatalf("DriveFeatureStop() error = %v", err)
			}
			if !stop.Terminated || stop.RunState != string(run.RunStopped) {
				t.Fatalf("stop = %+v, want stopped in its first round", stop)
			}
			session = f.child(t, f.tasks[0])
			if session.State != run.SessionTerminated || f.tc.Store.Attempts[session.AttemptID].value.State != run.AttemptInterrupted {
				t.Fatalf("session %s, attempt %s; want terminated and interrupted", session.State, f.tc.Store.Attempts[session.AttemptID].value.State)
			}
			if len(f.tc.Runtime.ClosedPanes) != 0 && tt.kind == app.OpPaneOpen {
				// Only the manager's pane may be closed; the child never had one.
				for _, pane := range f.tc.Runtime.ClosedPanes {
					if pane != "pane-mgr" {
						t.Fatalf("stop closed pane %s; the refused child pane never existed", pane)
					}
				}
			}
		})
	}
}

// TestAttemptLaunchCrashRecovery proves resume recovers every crash point
// of a per-attempt launch before reconciling sessions.
func TestAttemptLaunchCrashRecovery(t *testing.T) {
	t.Run("after the intent, before the act: resume waits, then settles the attempt", func(t *testing.T) {
		for _, tt := range []struct {
			name       string
			retryLimit int
			wantTask   run.TaskState
		}{
			{"retry budget left: needs-rework", 3, run.TaskNeedsRework},
			{"retry budget exhausted: failed, and the run fails on retirement", 1, run.TaskFailed},
		} {
			t.Run(tt.name, func(t *testing.T) {
				f := newLaunchFixture(t, 1, tt.retryLimit)
				// The controller dies at the dispatch revalidation: the intent
				// is committed, the act never dispatched.
				f.tc.Store.HeartbeatHook = f.loseLease
				if _, err := f.tc.Controller.AssignReadyTasks(context.Background(), f.handle, launchOptions()); !errors.Is(err, app.ErrFenced) {
					t.Fatalf("AssignReadyTasks() error = %v, want the lost lease", err)
				}
				f.tc.Store.HeartbeatHook = nil
				op := f.onlyWorktreeOp(t)
				if op.State != app.OperationPending || len(f.tc.Runtime.CreateWorktreeRequests) != 0 {
					t.Fatalf("worktree.create = %s, want pending and never dispatched", op.State)
				}
				session := f.child(t, f.tasks[0])

				f.showNoCheckout()
				result := f.resume(t)
				if result.Outcome != "reconciling" {
					t.Fatalf("resume within the wait = %+v, want reconciling", result)
				}
				if report := sessionReport(t, &result, session.ID.String()); report.Disposition != app.SessionPending || !strings.Contains(report.Detail, "in flight") {
					t.Fatalf("child report = %+v, want pending with the in-flight worktree named", report)
				}
				var wait struct {
					WaitingSince string `json:"waiting_since"`
				}
				decodeInto(t, f.tc.Store.Operations[op.ID].ActEvidence, &wait)
				if wait.WaitingSince != op.CreatedAt.UTC().Format(time.RFC3339Nano) {
					t.Fatalf("bounded wait anchored at %q, want the operation's creation %s", wait.WaitingSince, op.CreatedAt)
				}

				f.tc.Clock.Advance(3 * time.Minute)
				result = f.resume(t)
				if result.Outcome != "resumed" {
					t.Fatalf("resume past the wait = %+v, want resumed", result)
				}
				if got := f.tc.Store.Operations[op.ID].State; got != app.OperationFailed {
					t.Fatalf("worktree.create = %s, want failed", got)
				}
				f.assertSettled(t, f.tasks[0], tt.wantTask, "no checkout of the attempt's branch surfaced within the bounded wait")
				if len(f.tc.Runtime.CreateWorktreeRequests) != 0 {
					t.Fatalf("CreateWorktree dispatched %d times, want never", len(f.tc.Runtime.CreateWorktreeRequests))
				}
				if tt.wantTask != run.TaskFailed {
					return
				}
				retirement, err := f.tc.Controller.RetireSettledSessions(context.Background(), f.handle)
				if err != nil {
					t.Fatalf("RetireSettledSessions() error = %v", err)
				}
				if !retirement.RunFailing {
					t.Fatalf("retirement = %+v, want the failing run (the manager is alive)", retirement)
				}
			})
		}
	})

	t.Run("after the act, before the outcome: resume adopts, links the row and launches with a fresh incarnation", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 3)
		f.tc.Runtime.CreateWorktreeFn = func(req app.WorktreeRequest) (app.WorktreeInfo, error) {
			f.tc.Runtime.nameWorktreeWorkspaceLocked(req.Label, "workspace-created")
			f.crash() // the outcome commit is fenced
			return app.WorktreeInfo{WorkspaceID: "workspace-created", Path: "/worktrees/created", Branch: req.Branch}, nil
		}
		if _, err := f.tc.Controller.AssignReadyTasks(context.Background(), f.handle, launchOptions()); !errors.Is(err, app.ErrFenced) {
			t.Fatalf("AssignReadyTasks() error = %v, want the fenced outcome", err)
		}
		f.tc.Runtime.CreateWorktreeFn = func(app.WorktreeRequest) (app.WorktreeInfo, error) {
			t.Fatalf("CreateWorktree re-driven by resume")
			return app.WorktreeInfo{}, nil
		}
		op := f.onlyWorktreeOp(t)
		session := f.child(t, f.tasks[0])
		if op.State != app.OperationPending || len(f.tc.Store.Worktrees) != 0 {
			t.Fatalf("after the crash: worktree.create %s, %d rows; want pending with no row", op.State, len(f.tc.Store.Worktrees))
		}

		f.showCheckout("/worktrees/created")
		result := f.resume(t)
		if result.Outcome != "resumed" || result.RunState != string(run.RunRunning) {
			t.Fatalf("resume = %+v, want resumed and running", result)
		}
		if report := sessionReport(t, &result, session.ID.String()); report.Disposition != app.SessionPending {
			t.Fatalf("child report = %+v, want pending (its launch is in flight)", report)
		}
		if got := f.tc.Store.Operations[op.ID].State; got != app.OperationSucceeded {
			t.Fatalf("worktree.create = %s, want succeeded by adoption", got)
		}
		row, ok := f.tc.Store.newestAttemptWorktreeLocked(session.AttemptID)
		if !ok || row.AttemptID != session.AttemptID || row.BaseCommit != fakeHeadCommitOID || row.Path != "/worktrees/created" {
			t.Fatalf("row = %+v (found %t), want the checkout linked to its attempt and base", row, ok)
		}
		f.assertLaunchedFrom(t, session.ID, "/worktrees/created")

		// The loop's corroboration takes it from here.
		if _, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), f.handle); err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
	})

	t.Run("after the worktree, before the pane intent: resume writes the assignment and opens the pane", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 3)
		f.tc.Artifacts.WriteErr = errors.New("disk full")
		f.assign(t)
		f.tc.Artifacts.WriteErr = nil
		f.crash()
		session := f.child(t, f.tasks[0])
		result := f.resume(t)
		if result.Outcome != "resumed" {
			t.Fatalf("resume = %+v, want resumed", result)
		}
		f.assertLaunchedFrom(t, session.ID, "/worktrees/w")
	})

	t.Run("between the assignment commit and the intent: resume re-drives the create", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 3)
		f.tc.Commands.Results["/usr/bin/git -C /repo rev-parse --verify refs/heads/hop/r1/integration"] = app.CommandResult{Stdout: []byte(fakeHeadCommitOID + "\n")}
		f.tc.Store.CommitHook = func(u *fakeUnitOfWork) {
			for _, op := range u.opCreated {
				if op.Kind == app.OpWorktreeCreate {
					f.crash() // the intent commit is fenced
				}
			}
		}
		if _, err := f.tc.Controller.AssignReadyTasks(context.Background(), f.handle, launchOptions()); !errors.Is(err, app.ErrFenced) {
			t.Fatalf("AssignReadyTasks() error = %v, want the fenced intent", err)
		}
		f.tc.Store.CommitHook = nil
		session := f.child(t, f.tasks[0])
		if n := len(f.ops(app.OpWorktreeCreate)); n != 0 || session.State != run.SessionLaunching {
			t.Fatalf("after the crash: %d worktree ops, session %s; want none and launching", n, session.State)
		}
		result := f.resume(t)
		if result.Outcome != "resumed" {
			t.Fatalf("resume = %+v, want resumed", result)
		}
		op := f.onlyWorktreeOp(t)
		var intent struct {
			BaseRef   string `json:"base_ref"`
			AttemptID string `json:"attempt_id"`
			Branch    string `json:"branch"`
		}
		decodeInto(t, op.Intent, &intent)
		if op.State != app.OperationSucceeded || intent.BaseRef != fakeHeadCommitOID || intent.AttemptID != session.AttemptID.String() || intent.Branch != "hop/r1/t1a1" {
			t.Fatalf("re-driven worktree.create = %s %+v, want succeeded for the attempt at the integration head", op.State, intent)
		}
		f.assertLaunchedFrom(t, session.ID, "/worktrees/w")
	})

	t.Run("between the assignment commit and the intent: an existing branch settles the attempt", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 3)
		f.tc.Commands.Results["/usr/bin/git -C /repo rev-parse --verify refs/heads/hop/r1/integration"] = app.CommandResult{Stdout: []byte(fakeHeadCommitOID + "\n")}
		f.tc.Store.CommitHook = func(u *fakeUnitOfWork) {
			for _, op := range u.opCreated {
				if op.Kind == app.OpWorktreeCreate {
					f.crash()
				}
			}
		}
		if _, err := f.tc.Controller.AssignReadyTasks(context.Background(), f.handle, launchOptions()); !errors.Is(err, app.ErrFenced) {
			t.Fatalf("AssignReadyTasks() error = %v, want the fenced intent", err)
		}
		f.tc.Store.CommitHook = nil
		f.tc.Commands.Results["/usr/bin/git -C /repo rev-parse --verify refs/heads/hop/r1/t1a1"] = app.CommandResult{Stdout: []byte("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee\n")}
		result := f.resume(t)
		if result.Outcome != "resumed" {
			t.Fatalf("resume = %+v, want resumed", result)
		}
		f.assertSettled(t, f.tasks[0], run.TaskNeedsRework, "refs/heads/hop/r1/t1a1 already exists")
		if n := len(f.ops(app.OpWorktreeCreate)); n != 0 {
			t.Fatalf("worktree ops = %d, want none", n)
		}
	})

	t.Run("a sibling attempt's row never counts as adoption", func(t *testing.T) {
		f := newLaunchFixture(t, 2, 3)
		snapshot := f.tc.Store.Snapshots[f.fr.RunID]
		snapshot.Workflow.MaxWorkers = 2
		f.tc.Store.Snapshots[f.fr.RunID] = snapshot
		opts := defaultAssignmentOptions()
		opts.MaxWorkers = 2
		created := 0
		f.tc.Runtime.CreateWorktreeFn = func(req app.WorktreeRequest) (app.WorktreeInfo, error) {
			created++
			if req.Branch == "hop/r1/t2a1" {
				return app.WorktreeInfo{}, errors.New("herdr: timeout")
			}
			return app.WorktreeInfo{WorkspaceID: "workspace-t1", Path: "/worktrees/t1", Branch: req.Branch}, nil
		}
		report, err := f.tc.Controller.AssignReadyTasks(context.Background(), f.handle, opts)
		if err != nil {
			t.Fatalf("AssignReadyTasks() error = %v", err)
		}
		if len(report.Assigned) != 1 || launchFor(t, &report, 2).Disposition != app.AttemptLaunchReconciling {
			t.Fatalf("report = %+v, want t1 launched and t2 reconciling", report)
		}
		sibling := f.child(t, f.tasks[0])
		target := f.child(t, f.tasks[1])
		// The listing shows only the sibling's checkout.
		f.showCheckout("/worktrees/t1")
		f.crash()
		result := f.resume(t)
		if result.Outcome != "reconciling" {
			t.Fatalf("resume = %+v, want reconciling", result)
		}
		if _, ok := f.tc.Store.newestAttemptWorktreeLocked(target.AttemptID); ok {
			t.Fatalf("the target attempt gained a row from its sibling's checkout")
		}
		ops := f.ops(app.OpWorktreeCreate)
		for i := range ops {
			var intent struct {
				AttemptID string `json:"attempt_id"`
			}
			decodeInto(t, ops[i].Intent, &intent)
			if intent.AttemptID == target.AttemptID.String() && ops[i].State != app.OperationReconciling {
				t.Fatalf("target worktree.create = %s, want still reconciling", ops[i].State)
			}
		}
		f.tc.Clock.Advance(3 * time.Minute)
		f.resume(t)
		if got := f.tc.Store.Attempts[target.AttemptID].value.State; got != run.AttemptFailed {
			t.Fatalf("target attempt = %s, want failed past the wait", got)
		}
		row, ok := f.tc.Store.newestAttemptWorktreeLocked(sibling.AttemptID)
		if !ok || row.Path != "/worktrees/t1" || created != 2 {
			t.Fatalf("sibling row = %+v (found %t) after %d creates, want it untouched and no re-create", row, ok, created)
		}
	})
}

// TestAttemptLaunchShutdownResolvesWorktree proves stop and the terminal
// failure settlement resolve an unresolved worktree.create — adopted and
// recorded, or settled failed after the wait — before their terminal
// report, and never launch anything.
func TestAttemptLaunchShutdownResolvesWorktree(t *testing.T) {
	requestStop := func(f *launchFixture) {
		rRow := f.tc.Store.Runs[f.fr.RunID]
		rRow.value = rRow.value.RequestStop(f.tc.Clock.Now())
		rRow.revision++
	}

	t.Run("stop adopts and records a checkout that surfaces, and opens nothing", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 3)
		f.tc.Runtime.CreateWorktreeFn = func(req app.WorktreeRequest) (app.WorktreeInfo, error) {
			f.tc.Runtime.nameWorktreeWorkspaceLocked(req.Label, "workspace-late")
			return app.WorktreeInfo{}, errors.New("herdr: timeout")
		}
		f.assign(t)
		session := f.child(t, f.tasks[0])
		requestStop(f)
		f.showCheckout("/worktrees/late")
		f.tc.Runtime.InspectPaneFn = allPanesAbsent
		stop, err := f.tc.Controller.DriveFeatureStop(context.Background(), f.handle)
		if err != nil {
			t.Fatalf("DriveFeatureStop() error = %v", err)
		}
		if !stop.Terminated {
			t.Fatalf("stop = %+v, want stopped", stop)
		}
		if op := f.onlyWorktreeOp(t); op.State != app.OperationSucceeded {
			t.Fatalf("worktree.create = %s, want adopted before the stopped report", op.State)
		}
		if _, ok := f.tc.Store.newestAttemptWorktreeLocked(session.AttemptID); !ok {
			t.Fatalf("the adopted checkout has no row")
		}
		if n := len(f.paneOpensFor(t, session.ID)); n != 0 {
			t.Fatalf("stop opened %d panes", n)
		}
	})

	t.Run("stop never reports stopped over an unverifiable checkout", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 3)
		f.tc.Runtime.CreateWorktreeErr = errors.New("herdr: timeout")
		f.assign(t)
		requestStop(f)
		f.showCheckout("/worktrees/broken")
		f.tc.Commands.Errs["/usr/bin/git -C /worktrees/broken rev-parse HEAD^{commit}"] = errors.New("git: transport")
		f.tc.Runtime.InspectPaneFn = allPanesAbsent
		f.wait(t, 10*time.Minute)
		for range 2 {
			stop, err := f.tc.Controller.DriveFeatureStop(context.Background(), f.handle)
			if err != nil {
				t.Fatalf("DriveFeatureStop() error = %v", err)
			}
			if stop.Terminated || !strings.Contains(strings.Join(stop.Outstanding, "\n"), "could not be verified") {
				t.Fatalf("stop = %+v, want outstanding on the unverifiable checkout", stop)
			}
			assertPathFree(t, strings.Join(stop.Outstanding, "\n"))
		}
	})

	t.Run("a failed task's run fails only once the worktree.create is resolved", func(t *testing.T) {
		f := newLaunchFixture(t, 2, 3)
		f.tc.Runtime.CreateWorktreeErr = errors.New("herdr: timeout")
		f.assign(t)
		op := f.onlyWorktreeOp(t)
		// Another task has failed: the run carries a terminal-failure cause.
		failed := f.tc.Store.Tasks[f.tasks[1]]
		failed.value.State = run.TaskFailed
		f.showNoCheckout()
		f.tc.Runtime.InspectPaneFn = allPanesAbsent

		report, err := f.tc.Controller.RetireSettledSessions(context.Background(), f.handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() error = %v", err)
		}
		if report.RunFailed || !report.RunFailing || !strings.Contains(strings.Join(report.Outstanding, "\n"), op.ID.String()) {
			t.Fatalf("retirement within the wait = %+v, want failing with the worktree operation outstanding", report)
		}
		if got := f.tc.Store.Runs[f.fr.RunID].value.State; got != run.RunRunning {
			t.Fatalf("run = %s, want still running", got)
		}
		f.wait(t, 3*time.Minute)
		report, err = f.tc.Controller.RetireSettledSessions(context.Background(), f.handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() error = %v", err)
		}
		if !report.RunFailed {
			t.Fatalf("retirement past the wait = %+v, want RunFailed", report)
		}
		if got := f.tc.Store.Operations[op.ID].State; got != app.OperationFailed {
			t.Fatalf("worktree.create = %s, want failed before the failed report", got)
		}
		if len(f.tc.Runtime.CreateWorktreeRequests) != 1 {
			t.Fatalf("creates = %d, want the one original", len(f.tc.Runtime.CreateWorktreeRequests))
		}
	})

	t.Run("resume of a failing run resolves the operation but launches nothing", func(t *testing.T) {
		f := newLaunchFixture(t, 2, 3)
		f.tc.Runtime.CreateWorktreeFn = func(req app.WorktreeRequest) (app.WorktreeInfo, error) {
			f.tc.Runtime.nameWorktreeWorkspaceLocked(req.Label, "workspace-late")
			return app.WorktreeInfo{}, errors.New("herdr: timeout")
		}
		f.assign(t)
		session := f.child(t, f.tasks[0])
		f.tc.Store.Tasks[f.tasks[1]].value.State = run.TaskFailed
		f.showCheckout("/worktrees/late")
		f.crash()
		f.resume(t)
		if op := f.onlyWorktreeOp(t); op.State != app.OperationSucceeded {
			t.Fatalf("worktree.create = %s, want adopted", op.State)
		}
		if n := len(f.paneOpensFor(t, session.ID)); n != 0 {
			t.Fatalf("resume launched %d panes into a failing run", n)
		}
	})
}

// TestAttemptLaunchUnattributableIntent proves a worktree.create intent
// naming no attempt is never settled automatically: it blocks the
// re-drive, resume reports it, and stop stays outstanding.
func TestAttemptLaunchUnattributableIntent(t *testing.T) {
	f := newLaunchFixture(t, 1, 3)
	f.tc.Store.CommitHook = func(u *fakeUnitOfWork) {
		for _, op := range u.opCreated {
			if op.Kind == app.OpWorktreeCreate {
				f.crash()
			}
		}
	}
	if _, err := f.tc.Controller.AssignReadyTasks(context.Background(), f.handle, launchOptions()); !errors.Is(err, app.ErrFenced) {
		t.Fatalf("AssignReadyTasks() error = %v, want the fenced intent", err)
	}
	f.tc.Store.CommitHook = nil
	opID := identity.OperationID(f.tc.IDs.NewID())
	now := f.tc.Clock.Now()
	f.tc.Store.Operations[opID] = app.Operation{
		ID: opID, RunID: f.fr.RunID, Generation: 1, Kind: app.OpWorktreeCreate, State: app.OperationPending,
		Intent:    map[string]any{"repository_root": "/repo", "branch": "hop/run-1", "base_ref": fakeHeadCommitOID},
		CreatedAt: now, UpdatedAt: now,
	}
	f.tc.Commands.Results["/usr/bin/git -C /repo rev-parse --verify refs/heads/hop/r1/integration"] = app.CommandResult{Stdout: []byte(fakeHeadCommitOID + "\n")}

	result := f.resume(t)
	if result.Outcome != "reconciling" || !strings.Contains(strings.Join(result.Blocked, "\n"), opID.String()) {
		t.Fatalf("resume = %+v, want reconciling with the operation blocked", result)
	}
	if len(f.tc.Runtime.CreateWorktreeRequests) != 0 || len(f.ops(app.OpWorktreeCreate)) != 1 {
		t.Fatalf("a create was re-driven beside an unattributable operation")
	}
	if got := f.tc.Store.Operations[opID].State; got != app.OperationReconciling {
		t.Fatalf("unattributable operation = %s, want reconciling", got)
	}
	status, err := f.tc.Controller.Status(context.Background(), app.StatusRequest{RunID: f.fr.RunID.String()})
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if ops := status.Detail.WorktreeOperations; len(ops) != 1 || !strings.Contains(ops[0].Action, "never settles it automatically") {
		t.Fatalf("status worktree operations = %+v, want the unreadable intent's action", ops)
	}

	rRow := f.tc.Store.Runs[f.fr.RunID]
	rRow.value = rRow.value.RequestStop(f.tc.Clock.Now())
	rRow.revision++
	f.wait(t, 10*time.Minute)
	stop, err := f.tc.Controller.DriveFeatureStop(context.Background(), f.handle)
	if err != nil {
		t.Fatalf("DriveFeatureStop() error = %v", err)
	}
	if stop.Terminated || !strings.Contains(strings.Join(stop.Outstanding, "\n"), opID.String()) {
		t.Fatalf("stop = %+v, want outstanding naming %s", stop, opID)
	}
}

// TestStatusWorktreeOperations proves hop status names every unresolved
// per-attempt worktree.create with a path-free human action, and nothing
// for a solo run.
func TestStatusWorktreeOperations(t *testing.T) {
	f := newLaunchFixture(t, 1, 3)
	f.tc.Runtime.CreateWorktreeErr = errors.New("herdr: timeout at /worktrees/x")
	f.assign(t)
	op := f.onlyWorktreeOp(t)
	status, err := f.tc.Controller.Status(context.Background(), app.StatusRequest{RunID: f.fr.RunID.String()})
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	ops := status.Detail.WorktreeOperations
	if len(ops) != 1 || ops[0].OperationID != op.ID.String() || ops[0].Branch != "hop/r1/t1a1" || ops[0].State != string(app.OperationReconciling) ||
		!strings.Contains(ops[0].Action, "in flight") || !strings.Contains(ops[0].Action, op.CreatedAt.Add(2*time.Minute).UTC().Format(time.RFC3339)) {
		t.Fatalf("status worktree operations = %+v, want the in-flight operation with its settle time", ops)
	}
	assertPathFree(t, ops[0].Action)

	solo := newTestController(defaultPolicy())
	_, detail := startedRun(t, solo)
	soloStatus, err := solo.Controller.Status(context.Background(), app.StatusRequest{RunID: detail.RunID.String()})
	if err != nil {
		t.Fatalf("Status(solo) error = %v", err)
	}
	if soloStatus.Detail.WorktreeOperations != nil {
		t.Fatalf("solo worktree operations = %+v, want none", soloStatus.Detail.WorktreeOperations)
	}
}
