package app_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// launchManagerPID is the pid the launch fixture's manager claim records.
const launchManagerPID = 900

// worktreeListing is the fake repository root's `git worktree list
// --porcelain` argv, as the controller runs it.
const worktreeListing = "/usr/bin/git -C /repo worktree list --porcelain"

// launchFixture is a running feature run with a settled, corroborating
// manager and ready implement tasks: the per-attempt launch recovery
// scenarios assign through the real scheduler, then break one act.
type launchFixture struct {
	tc     *testController
	fr     featureRun
	handle app.RunHandle
	tasks  []identity.TaskID
}

// newLaunchFixture seeds the run with taskCount ready implement tasks, a
// single worker slot and retryLimit.
func newLaunchFixture(t *testing.T, taskCount, retryLimit int) *launchFixture {
	t.Helper()
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 1)
	freezeLaunchPolicy(tc, fr.RunID)
	snapshot := tc.Store.Snapshots[fr.RunID]
	snapshot.Workflow.RetryLimit = retryLimit
	tc.Store.Snapshots[fr.RunID] = snapshot
	now := tc.Clock.Now()

	mgrRow := tc.Store.Sessions[fr.ManagerID]
	withRef, err := mgrRow.value.AssignNativeRef("manager-native-ref", run.NativeRefAssigned, now)
	if err != nil {
		t.Fatalf("AssignNativeRef() error = %v", err)
	}
	mgrRow.value = withRef
	mgrRow.revision++
	tc.Store.LaunchClaims[fr.ManagerIncarnation] = app.LaunchClaim{
		IncarnationID: fr.ManagerIncarnation, RunID: fr.RunID, SessionID: fr.ManagerID,
		Executable: "/usr/local/bin/claude", ArgvDigest: "d", PID: launchManagerPID,
		State: app.LaunchClaimExeced, ClaimedAt: now,
	}
	tc.Runtime.InspectPaneFn = func(paneID string) (app.PaneProcess, error) {
		if paneID != "pane-mgr" {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		return app.PaneProcess{
			ShellPID: 1, ForegroundGroupID: launchManagerPID,
			Foreground: []app.ProcessInfo{{PID: launchManagerPID, Argv: []string{"/usr/local/bin/claude"}, Cmdline: "claude " + fr.ManagerIncarnation.String()}},
		}, nil
	}

	f := &launchFixture{tc: tc, fr: fr, handle: fr.Handle}
	for i := range taskCount {
		f.tasks = append(f.tasks, seedImplementTask(t, tc, fr.RunID, i+1, "task", false, run.TaskReady))
	}
	return f
}

// launchOptions are the fixture's assignment options: its one worker slot.
func launchOptions() app.AssignmentOptions {
	opts := defaultAssignmentOptions()
	opts.MaxWorkers = 1
	return opts
}

// assign runs one AssignReadyTasks pass, which must not fail.
func (f *launchFixture) assign(t *testing.T) app.AssignmentReport {
	t.Helper()
	report, err := f.tc.Controller.AssignReadyTasks(context.Background(), f.handle, launchOptions())
	if err != nil {
		t.Fatalf("AssignReadyTasks() error = %v", err)
	}
	return report
}

// wait advances the clock by d the way a live controller lives through
// it: heartbeating every 10s, so its lease stays current.
func (f *launchFixture) wait(t *testing.T, d time.Duration) {
	t.Helper()
	for d > 0 {
		step := min(d, 10*time.Second)
		f.tc.Clock.Advance(step)
		d -= step
		if err := f.tc.Controller.Heartbeat(context.Background(), f.handle); err != nil {
			t.Fatalf("Heartbeat() error = %v", err)
		}
	}
}

// crash models the controller's death: its lease expires, so its next
// commit is fenced.
func (f *launchFixture) crash() { f.tc.Clock.Advance(leaseTTL + time.Second) }

// loseLease models a controller whose lease was released under it: its
// next heartbeat is fenced. Called from a store hook, outside the store's
// lock.
func (f *launchFixture) loseLease() {
	f.tc.Store.mu.Lock()
	defer f.tc.Store.mu.Unlock()
	f.tc.Store.Leases[f.fr.RunID].held = false
}

// resume runs ResumeFeature as a fresh controller and adopts its handle.
func (f *launchFixture) resume(t *testing.T) app.ResumeFeatureResult {
	t.Helper()
	result, handle, err := f.tc.Controller.ResumeFeature(context.Background(), app.ResumeFeatureRequest{
		RunID: f.fr.RunID.String(), ControllerID: "controller-2",
		HOPPath: "/usr/local/bin/hop", StateRoot: "/state",
	})
	if err != nil {
		t.Fatalf("ResumeFeature() error = %v", err)
	}
	f.handle = handle
	return result
}

// child returns the one child session of task.
func (f *launchFixture) child(t *testing.T, task identity.TaskID) run.Session {
	t.Helper()
	var found []run.Session
	for _, row := range f.tc.Store.Sessions {
		if row.value.AttemptID == "" {
			continue
		}
		if attempt, ok := f.tc.Store.Attempts[row.value.AttemptID]; ok && attempt.value.TaskID == task {
			found = append(found, row.value)
		}
	}
	if len(found) != 1 {
		t.Fatalf("task %s has %d child sessions, want 1", task, len(found))
	}
	return found[0]
}

// ops lists the run's operations of kind, oldest first.
func (f *launchFixture) ops(kind app.OperationKind) []app.Operation {
	var out []app.Operation
	for _, op := range f.tc.Store.Operations { //nolint:gocritic // rangeValCopy: test helper over a small map.
		if op.RunID == f.fr.RunID && op.Kind == kind {
			out = append(out, op)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// onlyWorktreeOp returns the run's single worktree.create operation.
func (f *launchFixture) onlyWorktreeOp(t *testing.T) app.Operation {
	t.Helper()
	ops := f.ops(app.OpWorktreeCreate)
	if len(ops) != 1 {
		t.Fatalf("worktree.create operations = %d, want 1: %+v", len(ops), ops)
	}
	return ops[0]
}

// paneOpensFor lists the pane.open operations naming session.
func (f *launchFixture) paneOpensFor(t *testing.T, session identity.SessionID) []app.Operation {
	t.Helper()
	var out []app.Operation
	for _, op := range f.ops(app.OpPaneOpen) { //nolint:gocritic // rangeValCopy: test helper over a small slice.
		var intent struct {
			SessionID string `json:"session_id"`
		}
		decodeInto(t, op.Intent, &intent)
		if intent.SessionID == session.String() {
			out = append(out, op)
		}
	}
	return out
}

// showCheckout scripts the repository's worktree listing with one checkout
// of the first task's first-attempt branch at path beside the main
// checkout.
func (f *launchFixture) showCheckout(path string) {
	f.tc.Commands.Results[worktreeListing] = app.CommandResult{Stdout: []byte(
		"worktree /repo\nHEAD " + fakeHeadCommitOID + "\nbranch refs/heads/main\n\n" +
			"worktree " + path + "\nHEAD " + fakeHeadCommitOID + "\nbranch refs/heads/hop/r1/t1a1\n",
	)}
}

// showNoCheckout scripts the listing with the main checkout alone.
func (f *launchFixture) showNoCheckout() {
	f.tc.Commands.Results[worktreeListing] = app.CommandResult{Stdout: []byte(
		"worktree /repo\nHEAD " + fakeHeadCommitOID + "\nbranch refs/heads/main\n",
	)}
}

// launching reports one condition for task's attempt from report.
func launchFor(t *testing.T, report *app.AssignmentReport, taskSeq int) app.AttemptLaunchCondition {
	t.Helper()
	var found []app.AttemptLaunchCondition
	for _, l := range report.Launches {
		if l.TaskSeq == taskSeq {
			found = append(found, l)
		}
	}
	if len(found) != 1 {
		t.Fatalf("launch conditions for t%d = %+v, want exactly one (report %+v)", taskSeq, found, report)
	}
	return found[0]
}

// assertPathFree fails when text echoes a filesystem path the fixture
// uses: report and status text is fixed, never a path or a raw cause.
func assertPathFree(t *testing.T, text string) {
	t.Helper()
	for _, fragment := range []string{"/repo", "/worktrees", "/state", "/usr/"} {
		if strings.Contains(text, fragment) {
			t.Fatalf("text %q echoes the path fragment %q", text, fragment)
		}
	}
}

// assertLaunchedFrom proves session's recovered launch is accepted by hop
// launch from path, the attempt's recorded worktree, under a fresh
// incarnation no earlier pane intent carried.
func (f *launchFixture) assertLaunchedFrom(t *testing.T, session identity.SessionID, path string) {
	t.Helper()
	ctx := context.Background()
	opens := f.paneOpensFor(t, session)
	if len(opens) != 1 || opens[0].State != app.OperationSucceeded {
		t.Fatalf("pane.open operations for %s = %+v, want exactly one succeeded", session, opens)
	}
	slc, err := f.tc.Store.LoadSessionLaunchContext(ctx, f.fr.RunID, session)
	if err != nil {
		t.Fatalf("LoadSessionLaunchContext() error = %v", err)
	}
	if slc.WorktreePath != path || slc.Relaunch {
		t.Fatalf("launch context worktree = %q relaunch %t, want %q first launch", slc.WorktreePath, slc.Relaunch, path)
	}
	for _, claim := range f.tc.Store.LaunchClaims { //nolint:gocritic // rangeValCopy: test helper over a small map.
		if claim.IncarnationID == slc.IncarnationID {
			t.Fatalf("incarnation %s already carries a claim; the recovered launch must mint a fresh one", slc.IncarnationID)
		}
	}
	plan, err := f.tc.Controller.PrepareSessionLaunchExec(ctx, sessionLaunchRequest(&slc, f.fr.RunID, path, 7101))
	if err != nil {
		t.Fatalf("PrepareSessionLaunchExec() from the recorded worktree error = %v", err)
	}
	if len(plan.Argv) < 3 || plan.Argv[1] != "--session-id" {
		t.Fatalf("launch argv = %q, want a first launch", plan.Argv)
	}
}

// assertSettled proves the launch failure settlement: attempt failed, the
// task's budgeted consequence, the session terminated and one manager
// notice carrying reason.
func (f *launchFixture) assertSettled(t *testing.T, task identity.TaskID, wantTask run.TaskState, reason string) {
	t.Helper()
	session := f.child(t, task)
	if got := f.tc.Store.Attempts[session.AttemptID].value.State; got != run.AttemptFailed {
		t.Errorf("attempt state = %s, want failed", got)
	}
	taskRow := f.tc.Store.Tasks[task].value
	if taskRow.State != wantTask || taskRow.MailboxClosed != (wantTask == run.TaskFailed) {
		t.Errorf("task = %s (mailbox closed %t), want %s", taskRow.State, taskRow.MailboxClosed, wantTask)
	}
	if session.State != run.SessionTerminated {
		t.Errorf("session state = %s, want terminated", session.State)
	}
	notices := controllerNoticesTo(f.tc, f.fr.RunID)
	if len(notices) != 1 {
		t.Fatalf("manager notices = %d, want 1", len(notices))
	}
	body := string(f.tc.Artifacts.files[notices[0].BodyPath])
	if !strings.Contains(body, "task t1 "+string(wantTask)+"\n") || !strings.Contains(body, reason) {
		t.Errorf("notice body = %q, want t1 %s with %q", body, wantTask, reason)
	}
	assertPathFree(t, body)
	if reasonText, ok := transitionReason(f.tc, app.EntityAttempt, session.AttemptID.String(), string(run.AttemptFailed)); !ok || !strings.Contains(reasonText, reason) {
		t.Errorf("attempt failure reason = %q (found %t), want %q", reasonText, ok, reason)
	}
}

// TestAttemptLaunchLoopActError proves a worktree.create act failure no
// longer ends the scheduling pass: the launch is reported, later passes
// recover it by provenance or settle it after the bounded wait, and
// nothing is created twice while the first create may be in flight.
func TestAttemptLaunchLoopActError(t *testing.T) {
	t.Run("a later pass adopts the checkout the lost response created and launches it", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 3)
		var createdLabel string
		f.tc.Runtime.CreateWorktreeFn = func(req app.WorktreeRequest) (app.WorktreeInfo, error) {
			// Herdr created the worktree and its labeled workspace; the
			// response was lost.
			createdLabel = req.Label
			f.tc.Runtime.nameWorktreeWorkspaceLocked(req.Label, "workspace-lost")
			return app.WorktreeInfo{}, errors.New("herdr: response lost at /worktrees/lost")
		}
		report := f.assign(t)
		launch := launchFor(t, &report, 1)
		if launch.Disposition != app.AttemptLaunchReconciling || len(report.Assigned) != 0 {
			t.Fatalf("first pass = %+v, want the launch reported reconciling and nothing assigned", report)
		}
		assertPathFree(t, launch.Detail)
		op := f.onlyWorktreeOp(t)
		if op.State != app.OperationReconciling || createdLabel != op.ID.String() {
			t.Fatalf("worktree.create = %s with label %q, want reconciling and labeled with its operation id %s", op.State, createdLabel, op.ID)
		}
		session := f.child(t, f.tasks[0])
		if session.State != run.SessionLaunching || len(f.paneOpensFor(t, session.ID)) != 0 {
			t.Fatalf("session = %s with pane intents, want launching with none", session.State)
		}

		f.showCheckout("/worktrees/lost")
		f.tc.Runtime.CreateWorktreeFn = func(app.WorktreeRequest) (app.WorktreeInfo, error) {
			t.Fatalf("CreateWorktree re-driven while the first create's checkout is adoptable")
			return app.WorktreeInfo{}, nil
		}
		report = f.assign(t)
		if launch := launchFor(t, &report, 1); launch.Disposition != app.AttemptLaunchOpened {
			t.Fatalf("second pass launch = %+v, want opened", launch)
		}
		op = f.onlyWorktreeOp(t)
		if op.State != app.OperationSucceeded {
			t.Fatalf("worktree.create = %s, want succeeded by adoption", op.State)
		}
		row, ok := f.tc.Store.newestAttemptWorktreeLocked(session.AttemptID)
		if !ok || row.Path != "/worktrees/lost" || row.BaseCommit != fakeHeadCommitOID || row.Branch != "hop/r1/t1a1" {
			t.Fatalf("adopted row = %+v (found %t), want the lost checkout linked at the intent's base", row, ok)
		}
		opens := f.paneOpensFor(t, session.ID)
		var intent struct {
			WorkspaceID string `json:"workspace_id"`
			Cwd         string `json:"cwd"`
		}
		if len(opens) == 1 {
			decodeInto(t, opens[0].Intent, &intent)
		}
		if intent.WorkspaceID != "workspace-lost" || intent.Cwd != "/worktrees/lost" {
			t.Fatalf("pane intent = %+v, want the labeled workspace and the adopted checkout", intent)
		}
		if _, ok := f.tc.Artifacts.files["/state/runs/"+f.fr.RunID.String()+"/attempts/"+session.AttemptID.String()+"/assignment.md"]; !ok {
			t.Fatalf("assignment artifact not written before the pane opened; files = %v", keysOf(f.tc.Artifacts.files))
		}
		f.assertLaunchedFrom(t, session.ID, "/worktrees/lost")
	})

	t.Run("within the bounded wait nothing is created twice; past it the attempt settles and frees the slot", func(t *testing.T) {
		f := newLaunchFixture(t, 2, 3)
		f.tc.Runtime.CreateWorktreeErr = errors.New("herdr: refused")
		f.assign(t)
		op := f.onlyWorktreeOp(t)

		f.showNoCheckout()
		f.tc.Runtime.CreateWorktreeErr = nil
		f.wait(t, time.Minute)
		report := f.assign(t)
		if launch := launchFor(t, &report, 1); launch.Disposition != app.AttemptLaunchWaiting {
			t.Fatalf("launch within the wait = %+v, want waiting", launch)
		}
		if len(report.Assigned) != 0 || !report.SlotsFull || len(f.tc.Runtime.CreateWorktreeRequests) != 1 {
			t.Fatalf("report = %+v with %d creates, want the slot still held and one create", report, len(f.tc.Runtime.CreateWorktreeRequests))
		}

		f.wait(t, 90*time.Second)
		report = f.assign(t)
		launch := launchFor(t, &report, 1)
		if launch.Disposition != app.AttemptLaunchSettled {
			t.Fatalf("launch past the wait = %+v, want settled", launch)
		}
		assertPathFree(t, launch.Detail)
		if got := f.tc.Store.Operations[op.ID].State; got != app.OperationFailed {
			t.Fatalf("worktree.create = %s, want failed", got)
		}
		f.assertSettled(t, f.tasks[0], run.TaskNeedsRework, "no checkout of the attempt's branch surfaced within the bounded wait")
		if len(report.Assigned) != 1 || report.Assigned[0].TaskID != f.tasks[1] {
			t.Fatalf("Assigned = %+v, want t2 launched into the freed slot in the same pass", report.Assigned)
		}
	})

	t.Run("an unrelated created checkout settles the attempt in the same pass", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 3)
		f.tc.Commands.Results["/usr/bin/git -C /worktrees/w rev-parse --path-format=absolute --git-common-dir"] = app.CommandResult{Stdout: []byte("/other/.git\n")}
		report := f.assign(t)
		if launch := launchFor(t, &report, 1); launch.Disposition != app.AttemptLaunchSettled {
			t.Fatalf("launch = %+v, want settled", launch)
		}
		if op := f.onlyWorktreeOp(t); op.State != app.OperationFailed {
			t.Fatalf("worktree.create = %s, want failed", op.State)
		}
		f.assertSettled(t, f.tasks[0], run.TaskNeedsRework, "the created checkout is not the intended repository")
	})

	t.Run("an unrelated checkout found by recovery settles the attempt at the retry limit", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 1)
		f.tc.Runtime.CreateWorktreeErr = errors.New("herdr: timeout")
		f.assign(t)
		f.showCheckout("/worktrees/foreign")
		f.tc.Commands.Results["/usr/bin/git -C /worktrees/foreign rev-parse HEAD^{commit}"] = app.CommandResult{Stdout: []byte("dddddddddddddddddddddddddddddddddddddddd\n")}
		report := f.assign(t)
		if launch := launchFor(t, &report, 1); launch.Disposition != app.AttemptLaunchSettled {
			t.Fatalf("launch = %+v, want settled", launch)
		}
		f.assertSettled(t, f.tasks[0], run.TaskFailed, "not the intended repository at the attempt's base")
		if n := len(f.tc.Store.Worktrees); n != 0 {
			t.Fatalf("worktree rows = %d, want none for an unrelated checkout", n)
		}
	})

	t.Run("an unverifiable created checkout keeps the act's workspace and launches there once verified", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 3)
		headArgv := "/usr/bin/git -C /worktrees/w rev-parse HEAD^{commit}"
		f.tc.Commands.Errs[headArgv] = errors.New("git: transport")
		report := f.assign(t)
		if launch := launchFor(t, &report, 1); launch.Disposition != app.AttemptLaunchReconciling {
			t.Fatalf("launch = %+v, want reconciling", launch)
		}
		op := f.onlyWorktreeOp(t)
		var evidence struct {
			Info struct {
				WorkspaceID string `json:"WorkspaceID"`
				Branch      string `json:"Branch"`
			} `json:"info"`
		}
		decodeInto(t, op.ActEvidence, &evidence)
		if op.State != app.OperationReconciling || evidence.Info.WorkspaceID == "" || evidence.Info.Branch != "hop/r1/t1a1" {
			t.Fatalf("worktree.create = %s with evidence %+v, want reconciling with the act's response recorded", op.State, evidence)
		}

		f.showCheckout("/worktrees/w")
		report = f.assign(t)
		if launch := launchFor(t, &report, 1); launch.Disposition != app.AttemptLaunchReconciling {
			t.Fatalf("launch while still unverifiable = %+v, want reconciling", launch)
		}
		status, err := f.tc.Controller.Status(context.Background(), app.StatusRequest{RunID: f.fr.RunID.String()})
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		if ops := status.Detail.WorktreeOperations; len(ops) != 1 || ops[0].Branch != "hop/r1/t1a1" || ops[0].State != string(app.OperationReconciling) ||
			!strings.Contains(ops[0].Action, "repair or remove it") {
			t.Fatalf("status worktree operations = %+v, want the unverifiable checkout with its human action", ops)
		} else {
			assertPathFree(t, ops[0].Action)
		}

		delete(f.tc.Commands.Errs, headArgv)
		report = f.assign(t)
		if launch := launchFor(t, &report, 1); launch.Disposition != app.AttemptLaunchOpened {
			t.Fatalf("launch once verified = %+v, want opened", launch)
		}
		session := f.child(t, f.tasks[0])
		var intent struct {
			WorkspaceID string `json:"workspace_id"`
		}
		decodeInto(t, f.paneOpensFor(t, session.ID)[0].Intent, &intent)
		if intent.WorkspaceID != evidence.Info.WorkspaceID {
			t.Fatalf("pane workspace = %q, want the act's recorded %q", intent.WorkspaceID, evidence.Info.WorkspaceID)
		}
		f.assertLaunchedFrom(t, session.ID, "/worktrees/w")
	})

	t.Run("an adopted checkout with no recorded placement stays reconciling and opens nothing", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 3)
		f.tc.Runtime.CreateWorktreeFn = func(app.WorktreeRequest) (app.WorktreeInfo, error) {
			return app.WorktreeInfo{}, errors.New("herdr: timeout")
		}
		f.assign(t)
		f.showCheckout("/worktrees/unplaced")
		for range 2 {
			report := f.assign(t)
			launch := launchFor(t, &report, 1)
			if launch.Disposition != app.AttemptLaunchReconciling || !strings.Contains(launch.Detail, "placement is unrecorded") {
				t.Fatalf("launch = %+v, want reconciling on the unrecorded placement", launch)
			}
		}
		session := f.child(t, f.tasks[0])
		if _, ok := f.tc.Store.newestAttemptWorktreeLocked(session.AttemptID); !ok {
			t.Fatalf("the adopted checkout has no row")
		}
		if n := len(f.paneOpensFor(t, session.ID)); n != 0 {
			t.Fatalf("pane intents = %d, want none in an unrecorded workspace", n)
		}
	})

	t.Run("an unreadable listing stays reconciling past the wait and names the action", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 3)
		f.tc.Runtime.CreateWorktreeErr = errors.New("herdr: timeout")
		f.assign(t)
		f.tc.Commands.Errs[worktreeListing] = errors.New("git: transport")
		f.wait(t, 5*time.Minute)
		report := f.assign(t)
		if launch := launchFor(t, &report, 1); launch.Disposition != app.AttemptLaunchReconciling || !strings.Contains(launch.Detail, "listing could not be read") {
			t.Fatalf("launch = %+v, want reconciling on the unreadable listing", launch)
		}
		if op := f.onlyWorktreeOp(t); op.State != app.OperationReconciling {
			t.Fatalf("worktree.create = %s, want reconciling", op.State)
		}
	})
}

// keysOf lists a map's keys for diagnostics.
func keysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestAttemptLaunchAssignmentPath proves the assignment path's own launch
// refusals and failures are reported, never pass failures: the section 6
// refuse-if-exists check before the worktree.create intent, a deferred
// branch check, an unwritable assignment artifact and a pane act error.
func TestAttemptLaunchAssignmentPath(t *testing.T) {
	t.Run("an existing attempt branch settles the attempt, naming the ref and no path", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 3)
		f.tc.Commands.Results["/usr/bin/git -C /repo rev-parse --verify refs/heads/hop/r1/t1a1"] = app.CommandResult{Stdout: []byte(fakeHeadCommitOID + "\n")}
		report := f.assign(t)
		launch := launchFor(t, &report, 1)
		if launch.Disposition != app.AttemptLaunchSettled || !strings.Contains(launch.Detail, "refs/heads/hop/r1/t1a1 already exists") {
			t.Fatalf("launch = %+v, want settled with the collision named", launch)
		}
		assertPathFree(t, launch.Detail)
		if n := len(f.ops(app.OpWorktreeCreate)); n != 0 || len(f.tc.Runtime.CreateWorktreeRequests) != 0 {
			t.Fatalf("worktree.create operations = %d, creates = %d; want none after the refusal", n, len(f.tc.Runtime.CreateWorktreeRequests))
		}
		f.assertSettled(t, f.tasks[0], run.TaskNeedsRework, "refs/heads/hop/r1/t1a1 already exists")
	})

	t.Run("a symbolic attempt branch is a collision too", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 3)
		f.tc.Commands.Results["/usr/bin/git -C /repo symbolic-ref -q refs/heads/hop/r1/t1a1"] = app.CommandResult{Stdout: []byte("refs/heads/foreign\n")}
		report := f.assign(t)
		if launch := launchFor(t, &report, 1); launch.Disposition != app.AttemptLaunchSettled {
			t.Fatalf("launch = %+v, want settled", launch)
		}
		if len(f.tc.Runtime.CreateWorktreeRequests) != 0 {
			t.Fatalf("CreateWorktree called over a symbolic branch")
		}
	})

	t.Run("an unobservable branch check defers the create to a later pass", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 3)
		verify := "/usr/bin/git -C /repo rev-parse --verify refs/heads/hop/r1/t1a1"
		f.tc.Commands.Errs[verify] = errors.New("git: transport")
		report := f.assign(t)
		if launch := launchFor(t, &report, 1); launch.Disposition != app.AttemptLaunchReconciling {
			t.Fatalf("launch = %+v, want reconciling", launch)
		}
		if n := len(f.ops(app.OpWorktreeCreate)); n != 0 {
			t.Fatalf("worktree.create operations = %d, want none before the branch is checked", n)
		}
		delete(f.tc.Commands.Errs, verify)
		report = f.assign(t)
		if launch := launchFor(t, &report, 1); launch.Disposition != app.AttemptLaunchOpened {
			t.Fatalf("re-driven launch = %+v, want opened", launch)
		}
		f.assertLaunchedFrom(t, f.child(t, f.tasks[0]).ID, "/worktrees/w")
	})

	t.Run("an unwritable assignment artifact is reported; the next pass writes it and opens the pane", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 3)
		f.tc.Artifacts.WriteErr = errors.New("disk full at /state")
		report := f.assign(t)
		launch := launchFor(t, &report, 1)
		if launch.Disposition != app.AttemptLaunchReconciling || !strings.Contains(launch.Detail, "assignment artifact") {
			t.Fatalf("launch = %+v, want reconciling on the artifact", launch)
		}
		assertPathFree(t, launch.Detail)
		session := f.child(t, f.tasks[0])
		if op := f.onlyWorktreeOp(t); op.State != app.OperationSucceeded || len(f.paneOpensFor(t, session.ID)) != 0 {
			t.Fatalf("worktree.create = %s with pane intents; want the worktree recorded and no pane", op.State)
		}
		f.tc.Artifacts.WriteErr = nil
		report = f.assign(t)
		if launch := launchFor(t, &report, 1); launch.Disposition != app.AttemptLaunchOpened {
			t.Fatalf("continued launch = %+v, want opened", launch)
		}
		if len(f.tc.Runtime.CreateWorktreeRequests) != 1 {
			t.Fatalf("creates = %d, want the recorded worktree reused", len(f.tc.Runtime.CreateWorktreeRequests))
		}
		f.assertLaunchedFrom(t, session.ID, "/worktrees/w")
	})

	t.Run("a pane act error is reported; launch corroboration recovers the pane by its label", func(t *testing.T) {
		f := newLaunchFixture(t, 1, 3)
		f.tc.Runtime.OpenWorkerPaneErr = errors.New("herdr: response lost")
		report := f.assign(t)
		if launch := launchFor(t, &report, 1); launch.Disposition != app.AttemptLaunchReconciling || !strings.Contains(launch.Detail, "creation label") {
			t.Fatalf("launch = %+v, want reconciling pending label recovery", launch)
		}
		session := f.child(t, f.tasks[0])
		opens := f.paneOpensFor(t, session.ID)
		if len(opens) != 1 || opens[0].State != app.OperationReconciling {
			t.Fatalf("pane.open = %+v, want one reconciling", opens)
		}
		label := opens[0].ID.String()

		// The next pass never opens a second pane; corroboration finds the
		// created pane by its label and binds it.
		f.tc.Runtime.OpenWorkerPaneErr = nil
		f.tc.Runtime.FindPaneByLabelFn = func(l string) (app.PaneRef, bool, error) {
			if l == label {
				return app.PaneRef{WorkspaceID: "workspace-1", TabID: "tab-late", PaneID: "pane-late"}, true, nil
			}
			return app.PaneRef{}, false, nil
		}
		report = f.assign(t)
		if len(report.Launches) != 0 || len(f.paneOpensFor(t, session.ID)) != 1 {
			t.Fatalf("second pass = %+v with %d pane intents, want nothing re-driven", report, len(f.paneOpensFor(t, session.ID)))
		}
		if _, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), f.handle); err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		binding, ok := f.tc.Store.currentBindingLocked(session.ID)
		if !ok || binding.PaneID != "pane-late" {
			t.Fatalf("binding = %+v (found %t), want the pane recovered by label", binding, ok)
		}
	})
}
