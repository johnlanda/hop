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

// terminalCauseDetail is the fixed detail a launch suppressed by a
// terminal-failure cause reports.
const terminalCauseDetail = "terminal-failure cause"

// childSessionsOf counts task's child sessions.
func (f *launchFixture) childSessionsOf(task identity.TaskID) int {
	n := 0
	for _, row := range f.tc.Store.Sessions {
		if attempt, ok := f.tc.Store.Attempts[row.value.AttemptID]; ok && row.value.AttemptID != "" && attempt.value.TaskID == task {
			n++
		}
	}
	return n
}

// worktreeOpsFor lists the worktree.create operations whose intent names
// attempt.
func (f *launchFixture) worktreeOpsFor(t *testing.T, attempt identity.AttemptID) []app.Operation {
	t.Helper()
	var out []app.Operation
	for _, op := range f.ops(app.OpWorktreeCreate) { //nolint:gocritic // rangeValCopy: test helper over a small slice.
		var intent struct {
			AttemptID string `json:"attempt_id"`
		}
		decodeInto(t, op.Intent, &intent)
		if intent.AttemptID == attempt.String() {
			out = append(out, op)
		}
	}
	return out
}

// failSuccessorManager replaces the fixture's live manager with a
// launching successor whose launcher recorded exec_failed: the manager
// lineage's terminal-failure cause.
func (f *launchFixture) failSuccessorManager(t *testing.T) {
	t.Helper()
	f.tc.Store.Sessions[f.fr.ManagerID].value.State = run.SessionLost
	seedManagerMember(t, f.tc, f.fr.RunID, run.SessionLaunching, app.LaunchClaimExecFailed)
}

// crashBeforeWorktreeIntent makes the next worktree.create intent commit
// fenced: the controller dies between an assignment commit and its intent.
func (f *launchFixture) crashBeforeWorktreeIntent(t *testing.T) {
	t.Helper()
	f.tc.Store.CommitHook = func(u *fakeUnitOfWork) {
		for id := range u.opCreated {
			if u.opCreated[id].Kind == app.OpWorktreeCreate {
				f.crash()
			}
		}
	}
	if _, err := f.tc.Controller.AssignReadyTasks(context.Background(), f.handle, launchOptions()); !errors.Is(err, app.ErrFenced) {
		t.Fatalf("AssignReadyTasks() error = %v, want the fenced intent", err)
	}
	f.tc.Store.CommitHook = nil
}

// failThroughLoop runs what the controller loop runs for a resumed run
// carrying a terminal-failure cause — its retirement pass, which halts the
// scheduling pass — with every pane observed gone, until the run fails,
// and proves no stop was ever requested and nothing more was launched.
func (f *launchFixture) failThroughLoop(t *testing.T) {
	t.Helper()
	panes := len(f.ops(app.OpPaneOpen))
	f.tc.Runtime.InspectPaneFn = allPanesAbsent
	for round := 1; round <= 3; round++ {
		report, err := f.tc.Controller.RetireSettledSessions(context.Background(), f.handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() round %d error = %v", round, err)
		}
		if report.RunFailed {
			break
		}
		if !report.RunFailing {
			t.Fatalf("retirement round %d = %+v, want the failing run the loop halts on", round, report)
		}
	}
	r := f.tc.Store.Runs[f.fr.RunID].value
	if r.State != run.RunFailed || r.StopRequested {
		t.Fatalf("run = %s (stop requested %t), want failed with no stop", r.State, r.StopRequested)
	}
	if n := len(f.ops(app.OpPaneOpen)); n != panes {
		t.Fatalf("pane intents %d → %d during the failure cleanup, want none added", panes, n)
	}
	for _, row := range f.tc.Store.Sessions {
		if row.value.State != run.SessionTerminated && row.value.State != run.SessionLost {
			t.Fatalf("session %s = %s after the failure, want terminal", row.value.ID, row.value.State)
		}
	}
}

// TestAttemptLaunchFailurePrecedence proves a terminal-failure cause stops
// every later launch step — a worktree intent, an assignment artifact or a
// pane — including a cause created earlier in the same round, while
// operations keep resolving.
func TestAttemptLaunchFailurePrecedence(t *testing.T) {
	t.Run("an exhausted recovery settlement stops the pass's fresh assignment", func(t *testing.T) {
		f := newLaunchFixture(t, 2, 1)
		f.tc.Runtime.CreateWorktreeErr = errors.New("herdr: timeout")
		f.assign(t)
		f.tc.Runtime.CreateWorktreeErr = nil
		f.showNoCheckout()
		f.wait(t, 3*time.Minute)

		report := f.assign(t)
		if launch := launchFor(t, &report, 1); launch.Disposition != app.AttemptLaunchSettled {
			t.Fatalf("t1 launch = %+v, want settled", launch)
		}
		f.assertSettled(t, f.tasks[0], run.TaskFailed, "no checkout of the attempt's branch surfaced")
		if len(report.Assigned) != 0 || len(report.Launches) != 1 || f.childSessionsOf(f.tasks[1]) != 0 {
			t.Fatalf("report = %+v with %d t2 sessions; want nothing assigned into the failing run", report, f.childSessionsOf(f.tasks[1]))
		}
		if n := len(f.tc.Runtime.CreateWorktreeRequests); n != 1 || len(f.ops(app.OpWorktreeCreate)) != 1 || len(f.ops(app.OpPaneOpen)) != 0 {
			t.Fatalf("creates %d, worktree ops %d, pane ops %d; want only t1's one create", n, len(f.ops(app.OpWorktreeCreate)), len(f.ops(app.OpPaneOpen)))
		}
		if got := f.tc.Store.Tasks[f.tasks[1]].value.State; got != run.TaskReady {
			t.Fatalf("t2 = %s, want still ready", got)
		}
	})

	t.Run("an exhausted fresh collision stops the pass's next assignment", func(t *testing.T) {
		f := newLaunchFixture(t, 2, 1)
		f.tc.Commands.Results["/usr/bin/git -C /repo rev-parse --verify refs/heads/hop/r1/t1a1"] = app.CommandResult{Stdout: []byte(fakeHeadCommitOID + "\n")}
		report := f.assign(t)
		f.assertSettled(t, f.tasks[0], run.TaskFailed, "refs/heads/hop/r1/t1a1 already exists")
		if len(report.Assigned) != 0 || f.childSessionsOf(f.tasks[1]) != 0 || len(f.ops(app.OpWorktreeCreate)) != 0 {
			t.Fatalf("report = %+v; want no t2 session and no worktree intent after t1 failed", report)
		}
	})

	t.Run("a manager exec failure recorded before the create suppresses the worktree intent", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 3)
		f.tc.Commands.RunHook = func(_ context.Context, cmd app.Command) (app.CommandResult, bool, error) {
			if len(cmd.Argv) > 3 && cmd.Argv[3] == "symbolic-ref" {
				f.tc.Store.mu.Lock()
				claim := f.tc.Store.LaunchClaims[f.fr.ManagerIncarnation]
				claim.State = app.LaunchClaimExecFailed
				f.tc.Store.LaunchClaims[f.fr.ManagerIncarnation] = claim
				f.tc.Store.mu.Unlock()
			}
			return app.CommandResult{}, false, nil
		}
		report := f.assign(t)
		launch := launchFor(t, &report, 1)
		if launch.Disposition != app.AttemptLaunchReconciling || !strings.Contains(launch.Detail, terminalCauseDetail) {
			t.Fatalf("launch = %+v, want suppressed by the terminal-failure cause", launch)
		}
		if len(f.ops(app.OpWorktreeCreate)) != 0 || len(f.tc.Runtime.CreateWorktreeRequests) != 0 {
			t.Fatalf("a worktree intent was recorded after the manager's exec failure")
		}
	})

	t.Run("resume: once a settlement fails a task, a sibling's failed launch is only resolved, never settled", func(t *testing.T) {
		f := newLaunchFixture(t, 2, 1)
		opts := launchOptions()
		opts.MaxWorkers = 2
		f.tc.Runtime.CreateWorktreeErr = errors.New("herdr: timeout")
		if _, err := f.tc.Controller.AssignReadyTasks(context.Background(), f.handle, opts); err != nil {
			t.Fatalf("AssignReadyTasks() error = %v", err)
		}
		f.showNoCheckout()
		f.tc.Clock.Advance(3 * time.Minute)
		result := f.resume(t)

		first, second := f.child(t, f.tasks[0]), f.child(t, f.tasks[1])
		ops := f.ops(app.OpWorktreeCreate)
		for i := range ops {
			if ops[i].State != app.OperationFailed {
				t.Fatalf("worktree.create %s = %s, want both resolved failed", ops[i].ID, ops[i].State)
			}
		}
		if got := f.tc.Store.Tasks[f.tasks[0]].value.State; got != run.TaskFailed {
			t.Fatalf("t1 = %s, want failed by the first settlement", got)
		}
		if f.tc.Store.Attempts[second.AttemptID].value.State != run.AttemptLaunching || f.tc.Store.Tasks[f.tasks[1]].value.State != run.TaskActive {
			t.Fatalf("t2 attempt %s task %s; want untouched once the run carries a cause", f.tc.Store.Attempts[second.AttemptID].value.State, f.tc.Store.Tasks[f.tasks[1]].value.State)
		}
		if n := len(controllerNoticesTo(f.tc, f.fr.RunID)); n != 1 || first.State != run.SessionTerminated {
			t.Fatalf("notices = %d, t1 session %s; want exactly the first settlement", n, first.State)
		}
		if report := sessionReport(t, &result, second.ID.String()); !strings.Contains(report.Detail, terminalCauseDetail) {
			t.Fatalf("t2 report = %+v, want the suppression named", report)
		}
	})

	for _, tt := range []struct {
		name    string
		failing int // index of the task whose launch settles at the retry limit
	}{
		{"resume: an earlier target's exhausted settlement stops its sibling's continuation", 0},
		{"resume: a later target's exhausted settlement stops its sibling's continuation too", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newLaunchFixture(t, 2, 1)
			opts := launchOptions()
			opts.MaxWorkers = 2
			f.tc.Runtime.CreateWorktreeFn = func(req app.WorktreeRequest) (app.WorktreeInfo, error) {
				f.tc.Runtime.nameWorktreeWorkspaceLocked(req.Label, "workspace-"+strings.ReplaceAll(req.Branch, "/", "-"))
				return app.WorktreeInfo{}, errors.New("herdr: response lost")
			}
			if _, err := f.tc.Controller.AssignReadyTasks(context.Background(), f.handle, opts); err != nil {
				t.Fatalf("AssignReadyTasks() error = %v", err)
			}
			sibling := f.child(t, f.tasks[1-tt.failing])
			siblingBranch := []string{"hop/r1/t1a1", "hop/r1/t2a1"}[1-tt.failing]
			f.tc.Commands.Results[worktreeListing] = app.CommandResult{Stdout: []byte(
				"worktree /worktrees/sibling\nHEAD " + fakeHeadCommitOID + "\nbranch refs/heads/" + siblingBranch + "\n",
			)}
			f.tc.Clock.Advance(3 * time.Minute)
			result := f.resume(t)

			if got := f.tc.Store.Tasks[f.tasks[tt.failing]].value.State; got != run.TaskFailed {
				t.Fatalf("failing task = %s, want failed", got)
			}
			if n := len(f.paneOpensFor(t, sibling.ID)); n != 0 {
				t.Fatalf("resume opened %d sibling panes after a task failed", n)
			}
			if _, ok := f.tc.Store.newestAttemptWorktreeLocked(sibling.AttemptID); !ok {
				t.Fatalf("the sibling's checkout was not adopted; operations still resolve under the cause")
			}
			report := sessionReport(t, &result, sibling.ID.String())
			if result.Outcome != "resumed" || report.Disposition != app.SessionLaunchSuppressed {
				t.Fatalf("resume = %s, sibling report %+v; want resumed with the sibling handed to the failure cleanup", result.Outcome, report)
			}
			f.failThroughLoop(t)
		})
	}
}

// TestAttemptLaunchManagerFailurePrecedence proves resume reads the
// manager lineage's exec failure while the run is resuming: a failed
// successor manager suppresses a child's continuation and a child's
// re-drive alike, while a historical failed member does not.
func TestAttemptLaunchManagerFailurePrecedence(t *testing.T) {
	t.Run("a child whose worktree is recorded is not continued", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 3)
		f.tc.Artifacts.WriteErr = errors.New("disk full")
		f.assign(t)
		f.tc.Artifacts.WriteErr = nil
		child := f.child(t, f.tasks[0])
		artifactsBefore := len(f.tc.Artifacts.files)
		f.failSuccessorManager(t)
		f.crash()
		result := f.resume(t)
		if n := len(f.paneOpensFor(t, child.ID)); n != 0 {
			t.Fatalf("resume opened %d child panes under the manager's exec failure", n)
		}
		if len(f.tc.Runtime.CreateWorktreeRequests) != 1 || len(f.ops(app.OpWorktreeCreate)) != 1 || len(f.tc.Artifacts.files) != artifactsBefore {
			t.Fatalf("resume created intents or artifacts under the manager's exec failure")
		}
		if report := sessionReport(t, &result, child.ID.String()); result.Outcome != "resumed" || report.Disposition != app.SessionLaunchSuppressed {
			t.Fatalf("resume = %s, child report %+v; want resumed with the child handed to the failure cleanup", result.Outcome, report)
		}
		f.failThroughLoop(t)
	})

	t.Run("a child with no intent is not re-driven", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 3)
		f.tc.Commands.Results["/usr/bin/git -C /repo rev-parse --verify refs/heads/hop/r1/integration"] = app.CommandResult{Stdout: []byte(fakeHeadCommitOID + "\n")}
		f.crashBeforeWorktreeIntent(t)
		child := f.child(t, f.tasks[0])
		f.failSuccessorManager(t)
		result := f.resume(t)
		if len(f.worktreeOpsFor(t, child.AttemptID)) != 0 || len(f.tc.Runtime.CreateWorktreeRequests) != 0 || len(f.paneOpensFor(t, child.ID)) != 0 {
			t.Fatalf("resume re-drove the child under the manager's exec failure")
		}
		if report := sessionReport(t, &result, child.ID.String()); result.Outcome != "resumed" || report.Disposition != app.SessionLaunchSuppressed {
			t.Fatalf("resume = %s, child report %+v; want the child handed to the failure cleanup", result.Outcome, report)
		}
		f.failThroughLoop(t)
	})

	t.Run("historical failed and relaunched-away members before a working manager do not suppress the continuation", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 3)
		f.tc.Artifacts.WriteErr = errors.New("disk full")
		f.assign(t)
		f.tc.Artifacts.WriteErr = nil
		child := f.child(t, f.tasks[0])
		seedManagerMember(t, f.tc, f.fr.RunID, run.SessionLost, app.LaunchClaimExecFailed)
		seedManagerMember(t, f.tc, f.fr.RunID, run.SessionLost, app.LaunchClaimExeced)
		f.crash()
		if result := f.resume(t); result.Outcome != "resumed" {
			t.Fatalf("resume = %+v, want resumed", result)
		}
		f.assertLaunchedFrom(t, child.ID, "/worktrees/w")
	})
}

// TestAttemptLaunchFailureHandoff proves a resumed run whose remaining
// unplaced launches are suppressed by a terminal-failure cause is handed to
// the controller loop's failure cleanup rather than left reconciling:
// every resume — the first, and one after a controller that died before
// its loop ran — reports the run resumed with the child launch-suppressed,
// and the loop's retirement pass fails the run with no stop request. An
// unplaced launch whose worktree work is still unresolved keeps the run
// reconciling instead.
func TestAttemptLaunchFailureHandoff(t *testing.T) {
	t.Run("repeated resumes reach failed through the loop without a stop", func(t *testing.T) {
		f := newLaunchFixture(t, 2, 1)
		opts := launchOptions()
		opts.MaxWorkers = 2
		f.tc.Runtime.CreateWorktreeErr = errors.New("herdr: timeout")
		if _, err := f.tc.Controller.AssignReadyTasks(context.Background(), f.handle, opts); err != nil {
			t.Fatalf("AssignReadyTasks() error = %v", err)
		}
		f.showNoCheckout()
		f.tc.Clock.Advance(3 * time.Minute)
		suppressed := f.child(t, f.tasks[1])
		for round := 1; round <= 2; round++ {
			result := f.resume(t)
			report := sessionReport(t, &result, suppressed.ID.String())
			if result.Outcome != "resumed" || result.RunState != string(run.RunRunning) || report.Disposition != app.SessionLaunchSuppressed {
				t.Fatalf("resume round %d = %s/%s, report %+v; want resumed with the child launch-suppressed", round, result.Outcome, result.RunState, report)
			}
			assertPathFree(t, report.Detail)
			f.crash() // the controller dies before its loop runs
		}
		f.resume(t)
		f.failThroughLoop(t)
		if got := f.tc.Store.Attempts[suppressed.AttemptID].value.State; got == run.AttemptRunning {
			t.Fatalf("the suppressed attempt reached running")
		}
	})

	t.Run("an unplaced launch with unresolved worktree work keeps the run reconciling", func(t *testing.T) {
		f := newLaunchFixture(t, 2, 1)
		opts := launchOptions()
		opts.MaxWorkers = 2
		f.tc.Runtime.CreateWorktreeErr = errors.New("herdr: timeout")
		if _, err := f.tc.Controller.AssignReadyTasks(context.Background(), f.handle, opts); err != nil {
			t.Fatalf("AssignReadyTasks() error = %v", err)
		}
		// The second task's checkout is there but unverifiable: its
		// operation stays reconciling under the Phase 2 row.
		f.tc.Commands.Results[worktreeListing] = app.CommandResult{Stdout: []byte(
			"worktree /worktrees/broken\nHEAD " + fakeHeadCommitOID + "\nbranch refs/heads/hop/r1/t2a1\n",
		)}
		f.tc.Commands.Errs["/usr/bin/git -C /worktrees/broken rev-parse HEAD^{commit}"] = errors.New("git: transport")
		f.tc.Clock.Advance(3 * time.Minute)
		result := f.resume(t)
		if got := f.tc.Store.Tasks[f.tasks[0]].value.State; got != run.TaskFailed {
			t.Fatalf("t1 = %s, want failed", got)
		}
		report := sessionReport(t, &result, f.child(t, f.tasks[1]).ID.String())
		if result.Outcome != "reconciling" || report.Disposition != app.SessionPending {
			t.Fatalf("resume = %s, report %+v; want reconciling while the checkout is unverifiable", result.Outcome, report)
		}
	})
}

// TestAttemptLaunchFailureAtDispatch proves the feature-child dispatch
// revalidation reads the terminal-failure cause with the lease and the
// stop flag: a successor manager's exec_pending claim settling exec_failed
// during a child's pre-dispatch heartbeat refuses the dispatch — the
// worktree.create's or the pane.open's — which is recorded failed as never
// dispatched, and the loop's failure cleanup then retires the child and
// fails the run without waiting on it.
func TestAttemptLaunchFailureAtDispatch(t *testing.T) {
	for _, tt := range []struct {
		name      string
		heartbeat int
		refused   app.OperationKind
	}{
		{"at the worktree.create dispatch", 1, app.OpWorktreeCreate},
		{"at the pane.open dispatch", 2, app.OpPaneOpen},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newLaunchFixture(t, 1, 3)
			f.tc.Store.Sessions[f.fr.ManagerID].value.State = run.SessionLost
			manager := seedManagerMember(t, f.tc, f.fr.RunID, run.SessionLaunching, app.LaunchClaimExecPending)
			binding, ok := f.tc.Store.currentBindingLocked(manager)
			if !ok {
				t.Fatalf("the successor manager has no binding")
			}
			heartbeats := 0
			f.tc.Store.HeartbeatHook = func() {
				heartbeats++
				if heartbeats != tt.heartbeat {
					return
				}
				f.tc.Store.mu.Lock()
				defer f.tc.Store.mu.Unlock()
				claim := f.tc.Store.LaunchClaims[binding.IncarnationID]
				claim.State = app.LaunchClaimExecFailed
				f.tc.Store.LaunchClaims[binding.IncarnationID] = claim
			}
			report := f.assign(t)
			f.tc.Store.HeartbeatHook = nil
			launch := launchFor(t, &report, 1)
			if launch.Disposition != app.AttemptLaunchReconciling || !strings.Contains(launch.Detail, terminalCauseDetail) {
				t.Fatalf("launch = %+v, want refused by the terminal-failure cause", launch)
			}
			child := f.child(t, f.tasks[0])
			if _, bound := f.tc.Store.currentBindingLocked(child.ID); bound {
				t.Fatalf("the child acquired a pane binding after the cause was recorded")
			}
			refused := f.ops(tt.refused)
			if len(refused) != 1 || refused[0].State != app.OperationFailed {
				t.Fatalf("%s = %+v, want one recorded failed before dispatch", tt.refused, refused)
			}
			wantCreates := 0
			if tt.refused == app.OpPaneOpen {
				wantCreates = 1
			}
			if n := len(f.tc.Runtime.CreateWorktreeRequests); n != wantCreates {
				t.Fatalf("creates = %d, want %d", n, wantCreates)
			}
			if tt.refused == app.OpPaneOpen {
				// The refused pane.open is never dispatched: past the
				// launch-claim deadline corroboration leaves it settled, and a
				// resume hands the child to the failure cleanup rather than
				// waiting on it.
				f.wait(t, 3*time.Minute)
				if _, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), f.handle); err != nil {
					t.Fatalf("CorroborateSessionLaunches() error = %v", err)
				}
				if got := f.tc.Store.Operations[refused[0].ID].State; got != app.OperationFailed {
					t.Fatalf("refused pane.open = %s after the deadline, want still failed", got)
				}
				f.crash()
				result := f.resume(t)
				if report := sessionReport(t, &result, child.ID.String()); result.Outcome != "resumed" || report.Disposition != app.SessionLaunchSuppressed {
					t.Fatalf("resume = %s, child report %+v; want the child handed to the failure cleanup", result.Outcome, report)
				}
			}
			f.failThroughLoop(t)
		})
	}
}
