package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// TestRunFeatureControllerLoop proves the section 6 scheduling pass runs
// in the design's deterministic order once per tick, that check-driving
// runs asynchronously after the pass, and that the loop reports
// transitions and exits on the terminal state — the loop-ordering
// contract slice 6 owes for the feature-mode loop.
func TestRunFeatureControllerLoop(t *testing.T) {
	t.Run("runs the scheduling pass in order, corroborates and drives checks, then exits terminal", func(t *testing.T) {
		ctrl := &fakeController{}
		td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
		td.useCheckBarriers()

		var checkCalls atomic.Int32
		ctrl.driveFeatureChecks = func(ctx context.Context, hopPath string, spawnEnv []string) (app.FeatureCheckReport, error) {
			if hopPath != "/opt/hop/bin/hop" {
				t.Errorf("hop path = %q", hopPath)
			}
			if len(spawnEnv) == 0 {
				t.Error("spawn env is empty; the sanitized environment must be passed through")
			}
			if checkCalls.Add(1) == 1 {
				td.checkStarted <- struct{}{}
			}
			<-ctx.Done()
			return app.FeatureCheckReport{}, nil
		}
		statusCalls := 0
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			statusCalls++
			if statusCalls == 1 {
				return detailStep("running", "", false), nil
			}
			return detailStep("completed", "", false), nil
		}
		var stdout bytes.Buffer

		result, err := runFeatureControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
		if err != nil {
			t.Fatalf("runFeatureControllerLoop: %v", err)
		}
		if result.Detached || result.FinalState != "completed" {
			t.Errorf("result = %+v", result)
		}
		if got := stdout.String(); got != "run r1 running\nrun r1 completed\n" {
			t.Errorf("stdout = %q", got)
		}

		calls := ctrl.recorded()
		firstIndex := map[string]int{}
		for i, name := range calls {
			if _, seen := firstIndex[name]; !seen {
				firstIndex[name] = i
			}
		}
		orderedSteps := []string{
			"RetireSettledSessions", "RecomputeReleases", "DriveIntegration",
			"EnsureReviewTask", "DriveCompletion", "AssignReadyTasks",
			"CorroborateSessionLaunches", "DriveFeatureChecks",
		}
		for _, name := range orderedSteps {
			if _, ok := firstIndex[name]; !ok {
				t.Fatalf("%s was never called; calls = %v", name, calls)
			}
		}
		for i := 1; i < len(orderedSteps); i++ {
			prev, cur := orderedSteps[i-1], orderedSteps[i]
			if firstIndex[prev] >= firstIndex[cur] {
				t.Errorf("%s (at %d) did not run before %s (at %d); calls = %v", prev, firstIndex[prev], cur, firstIndex[cur], calls)
			}
		}
	})

	t.Run("stop takes precedence and interrupts an in-flight check", func(t *testing.T) {
		ctrl := &fakeController{}
		td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
		td.useCheckBarriers()

		ctrl.driveFeatureChecks = func(ctx context.Context, _ string, _ []string) (app.FeatureCheckReport, error) {
			td.checkStarted <- struct{}{}
			<-ctx.Done()
			return app.FeatureCheckReport{Interrupted: true}, nil
		}
		statusCalls := 0
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			statusCalls++
			if statusCalls == 1 {
				return detailStep("running", "", false), nil
			}
			return detailStep("stopping", "", true), nil
		}
		ctrl.driveFeatureStop = func() (app.StopReport, error) {
			return app.StopReport{RunState: "stopped", Terminated: true}, nil
		}
		var stdout bytes.Buffer

		result, err := runFeatureControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
		if err != nil {
			t.Fatalf("runFeatureControllerLoop: %v", err)
		}
		if result.FinalState != "stopped" {
			t.Errorf("result = %+v", result)
		}
		calls := ctrl.recorded()
		stopAt := -1
		for i, name := range calls {
			if name == "DriveFeatureStop" {
				stopAt = i
				break
			}
		}
		if stopAt == -1 {
			t.Fatalf("DriveFeatureStop was never called; calls = %v", calls)
		}
		for _, name := range calls[stopAt+1:] {
			if name == "AssignReadyTasks" {
				t.Errorf("AssignReadyTasks ran after a stop was observed; the scheduling pass must not run once stopping: calls = %v", calls)
			}
		}
	})

	t.Run("a heartbeat failure cancels in-flight work and reports the error", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.status = scriptStatus(detailStep("running", "", false))
		ctrl.heartbeat = func() error { return errors.New("fenced") }
		td := newTestDeps(ctrl, nil, t.TempDir())
		// Let the heartbeat goroutine run: its interval wait returns
		// immediately instead of blocking, mirroring
		// TestRunControllerLoop's identical solo-loop scenario.
		td.deps.wait = func(ctx context.Context, _ time.Duration) error {
			return ctx.Err()
		}
		var stdout bytes.Buffer

		_, err := runFeatureControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
		if err == nil || !strings.Contains(err.Error(), "heartbeat failed") {
			t.Fatalf("err = %v, want the heartbeat failure", err)
		}
	})

	t.Run("context cancellation detaches", func(t *testing.T) {
		ctrl := &fakeController{}
		td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return detailStep("running", "", false), nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var stdout bytes.Buffer

		result, err := runFeatureControllerLoop(ctx, td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
		if err != nil {
			t.Fatalf("runFeatureControllerLoop: %v", err)
		}
		if !result.Detached {
			t.Errorf("result = %+v, want Detached", result)
		}
	})
}

// TestRunFeatureControllerLoopWhileLaunching proves the reported defect
// fix: while the run is still launching, the scheduling pass runs only
// CorroborateSessionLaunches and skips every other step, since none of
// them has a settled manager session to work from yet.
func TestRunFeatureControllerLoopWhileLaunching(t *testing.T) {
	ctrl := &fakeController{}
	td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
	statusCalls := 0
	ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
		statusCalls++
		if statusCalls == 1 {
			return detailStep("launching", "", false), nil
		}
		return detailStep("completed", "", false), nil
	}
	var stdout bytes.Buffer

	result, err := runFeatureControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
	if err != nil {
		t.Fatalf("runFeatureControllerLoop: %v", err)
	}
	if result.FinalState != "completed" {
		t.Errorf("result = %+v", result)
	}

	calls := ctrl.recorded()
	seen := map[string]bool{}
	for _, name := range calls {
		seen[name] = true
	}
	if !seen["CorroborateSessionLaunches"] {
		t.Errorf("CorroborateSessionLaunches was never called; calls = %v", calls)
	}
	skipped := []string{
		"RetireSettledSessions", "RecomputeReleases", "DriveIntegration",
		"EnsureReviewTask", "DriveCompletion", "AssignReadyTasks",
		"AssignmentDefaults", "ResolveIntegrationHead", "PublishRunPresentation",
	}
	for _, name := range skipped {
		if seen[name] {
			t.Errorf("%s ran while the run was still launching; calls = %v", name, calls)
		}
	}
}

// TestRunFeatureSchedulingPassPopulatesAssignmentOptions proves the
// scheduling pass never calls AssignReadyTasks with a zero
// app.AssignmentOptions{}: MaxWorkers/Harness/ReviewerHarness and the
// frozen RepositoryRoot/StateRoot come from AssignmentDefaults, HOPPath is
// the running binary, and IntegrationHeadCommitOID comes from
// ResolveIntegrationHead, every pass.
func TestRunFeatureSchedulingPassPopulatesAssignmentOptions(t *testing.T) {
	ctrl := &fakeController{frozenRepositoryRoot: "/frozen/repo", frozenStateRoot: "/frozen/state-root"}
	ctrl.assignmentDefaults = func() (app.AssignmentOptions, error) {
		return app.AssignmentOptions{
			MaxWorkers: 3, Harness: "claude", ReviewerHarness: "codex",
			RepositoryRoot: "/frozen/repo", StateRoot: "/frozen/state-root",
		}, nil
	}
	ctrl.resolveIntegrationHead = func() (string, error) {
		return "cccccccccccccccccccccccccccccccccccccccc", nil
	}
	var gotOpts app.AssignmentOptions
	ctrl.assignReadyTasks = func(opts app.AssignmentOptions) (app.AssignmentReport, error) {
		gotOpts = opts
		return app.AssignmentReport{}, nil
	}

	pass, err := runFeatureSchedulingPass(context.Background(), ctrl, app.RunHandle{}, "running", "/opt/hop/bin/hop", []string{"KEY=value"}, false)
	if err != nil {
		t.Fatalf("runFeatureSchedulingPass() error = %v", err)
	}
	if pass.halted {
		t.Errorf("pass = %+v, want the full pass on a running run", pass)
	}

	want := app.AssignmentOptions{
		MaxWorkers: 3, Harness: "claude", ReviewerHarness: "codex",
		RepositoryRoot: "/frozen/repo", HOPPath: "/opt/hop/bin/hop", StateRoot: "/frozen/state-root",
		IntegrationHeadCommitOID: "cccccccccccccccccccccccccccccccccccccccc",
	}
	if gotOpts != want {
		t.Errorf("AssignReadyTasks options = %+v, want %+v", gotOpts, want)
	}
}

// TestFinishFeatureControllerLoop proves the exit-code mapping mirrors
// finishControllerLoop's solo shape: completed exits 0, everything else 1,
// a detach prints the resume instruction.
func TestFinishFeatureControllerLoop(t *testing.T) {
	t.Run("completed exits 0", func(t *testing.T) {
		ctrl := &fakeController{}
		td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return detailStep("completed", "", false), nil
		}
		var stdout, stderr bytes.Buffer

		code, err := finishFeatureControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout, &stderr, "hop run")
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK {
			t.Errorf("exit code = %d, want %d", code, exitOK)
		}
	})

	t.Run("failed exits 1", func(t *testing.T) {
		ctrl := &fakeController{}
		td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return detailStep("failed", "", false), nil
		}
		var stdout, stderr bytes.Buffer

		code, err := finishFeatureControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout, &stderr, "hop run")
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
	})
}

// statusFromFakeRunState serves the fake's own durable run state, so the
// loop observes exactly the state the scripted use cases moved the run to.
func statusFromFakeRunState(ctrl *fakeController) func(app.StatusRequest) (app.StatusResult, error) {
	return func(app.StatusRequest) (app.StatusResult, error) {
		return detailStep(ctrl.currentRunState(), "", false), nil
	}
}

// callsAfterFirst returns the calls recorded after the first call named
// name, and whether that call happened at all.
func callsAfterFirst(calls []string, name string) ([]string, bool) {
	for i, call := range calls {
		if call == name {
			return calls[i+1:], true
		}
	}
	return nil, false
}

// countCalls counts the recorded calls named name.
func countCalls(calls []string, name string) int {
	n := 0
	for _, call := range calls {
		if call == name {
			n++
		}
	}
	return n
}

// requireOnlyCalls fails the test when calls holds anything outside
// allowed.
func requireOnlyCalls(t *testing.T, calls []string, allowed ...string) {
	t.Helper()
	permitted := map[string]bool{}
	for _, name := range allowed {
		permitted[name] = true
	}
	for _, call := range calls {
		if !permitted[call] {
			t.Errorf("unexpected %s; calls = %v", call, calls)
		}
	}
}

// TestFeatureLoopHonorsRetirementAndCompletionReports proves the pass
// stops as soon as retirement or completion reports that the run left
// ordinary scheduling, against a fake whose AssignReadyTasks refuses every
// state but running exactly as the real use case does: completion (with
// retirement outstanding or in the same tick), a retirement round that
// fails the run, and a terminal failure that is due but blocked on owned
// work. In every case the loop observes the terminal state and maps it to
// the normal exit code, with nothing on stderr.
func TestFeatureLoopHonorsRetirementAndCompletionReports(t *testing.T) {
	t.Run("completion with retirement outstanding drives only completion on later ticks, then exits 0", func(t *testing.T) {
		ctrl := &fakeController{}
		td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
		ctrl.status = statusFromFakeRunState(ctrl)
		rounds := 0
		ctrl.driveCompletion = func() (app.CompletionReport, error) {
			rounds++
			switch rounds {
			case 1:
				ctrl.setRunState("completing")
				return app.CompletionReport{Ready: true, RunState: "completing", Outstanding: []string{"session m: close dispatched"}}, nil
			case 2:
				return app.CompletionReport{Ready: true, RunState: "completing", Outstanding: []string{"session m: close dispatched"}}, nil
			default:
				ctrl.setRunState("completed")
				return app.CompletionReport{Ready: true, RunState: "completed", Completed: true}, nil
			}
		}
		var stdout, stderr bytes.Buffer

		code, err := finishFeatureControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout, &stderr, "hop resume")
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK || stderr.Len() != 0 {
			t.Fatalf("exit code = %d, stderr = %q; want 0 and nothing", code, stderr.String())
		}
		if got := stdout.String(); got != "run r1 running\nrun r1 completing\nrun r1 completed\n" {
			t.Errorf("stdout = %q", got)
		}
		if rounds != 3 {
			t.Errorf("DriveCompletion rounds = %d, want 3 (entry, outstanding retirement, completed)", rounds)
		}
		calls := ctrl.recorded()
		after, ok := callsAfterFirst(calls, "DriveCompletion")
		if !ok {
			t.Fatalf("DriveCompletion never ran; calls = %v", calls)
		}
		requireOnlyCalls(t, after, "Status", "DriveCompletion", "Heartbeat", "Detach")
		if countCalls(calls, "DriveFeatureChecks") != 0 {
			t.Errorf("a check round was dispatched after the run left running; calls = %v", calls)
		}
	})

	t.Run("completion recorded in the same tick stops the pass and exits 0", func(t *testing.T) {
		ctrl := &fakeController{}
		td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
		ctrl.status = statusFromFakeRunState(ctrl)
		ctrl.driveCompletion = func() (app.CompletionReport, error) {
			ctrl.setRunState("completed")
			return app.CompletionReport{Ready: true, RunState: "completed", Completed: true}, nil
		}
		var stdout, stderr bytes.Buffer

		code, err := finishFeatureControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout, &stderr, "hop resume")
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK || stderr.Len() != 0 {
			t.Fatalf("exit code = %d, stderr = %q; want 0 and nothing", code, stderr.String())
		}
		if got := stdout.String(); got != "run r1 running\nrun r1 completed\n" {
			t.Errorf("stdout = %q", got)
		}
		calls := ctrl.recorded()
		after, _ := callsAfterFirst(calls, "DriveCompletion")
		requireOnlyCalls(t, after, "Status", "Heartbeat", "Detach")
	})

	t.Run("a retirement round that fails the run stops the pass and exits 1", func(t *testing.T) {
		ctrl := &fakeController{}
		td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
		ctrl.status = statusFromFakeRunState(ctrl)
		ctrl.retireSettledSessions = func() (app.RetirementReport, error) {
			ctrl.setRunState("failed")
			return app.RetirementReport{Retired: []string{"session-1"}, RunFailed: true}, nil
		}
		var stdout, stderr bytes.Buffer

		code, err := finishFeatureControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout, &stderr, "hop resume")
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure || stderr.Len() != 0 {
			t.Fatalf("exit code = %d, stderr = %q; want 1 and nothing (a failed run is a state, not an error)", code, stderr.String())
		}
		if got := stdout.String(); got != "run r1 running\nrun r1 failed\n" {
			t.Errorf("stdout = %q", got)
		}
		calls := ctrl.recorded()
		after, _ := callsAfterFirst(calls, "RetireSettledSessions")
		requireOnlyCalls(t, after, "Status", "Heartbeat", "Detach")
	})

	t.Run("a due-but-blocked terminal failure never claims an integration or assigns a task, then exits 1", func(t *testing.T) {
		ctrl := &fakeController{}
		td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
		ctrl.status = statusFromFakeRunState(ctrl)
		rounds := 0
		ctrl.retireSettledSessions = func() (app.RetirementReport, error) {
			rounds++
			if rounds < 3 {
				return app.RetirementReport{Outstanding: []string{"session-1: close dispatched"}, RunFailing: true}, nil
			}
			ctrl.setRunState("failed")
			return app.RetirementReport{Retired: []string{"session-1"}, RunFailed: true}, nil
		}
		ctrl.driveIntegration = func(context.Context, string, []string) (app.IntegrationReport, error) {
			t.Error("DriveIntegration ran while a terminal failure was due")
			return app.IntegrationReport{}, nil
		}
		ctrl.assignReadyTasks = func(app.AssignmentOptions) (app.AssignmentReport, error) {
			t.Error("AssignReadyTasks ran while a terminal failure was due")
			return app.AssignmentReport{}, nil
		}
		var stdout, stderr bytes.Buffer

		code, err := finishFeatureControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout, &stderr, "hop resume")
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure || stderr.Len() != 0 {
			t.Fatalf("exit code = %d, stderr = %q; want 1 and nothing", code, stderr.String())
		}
		if got := stdout.String(); got != "run r1 running\nrun r1 failed\n" {
			t.Errorf("stdout = %q", got)
		}
		if rounds != 3 {
			t.Errorf("retirement rounds = %d, want 3 (two blocked, one settling)", rounds)
		}
		requireOnlyCalls(t, ctrl.recorded(), "Status", "CheckSpawnEnvironment", "RetireSettledSessions", "Heartbeat", "Detach")
	})
}

// TestFeatureLoopManagerExecFailure proves the loop's side of the manager
// exec-failure rule: the failed manager launch prints the solo loop's own
// `launch failed` line — the fixed category, no path, no environment value,
// nothing on stderr — and the loop then observes the failed run and exits
// 1. A child's failed launch prints nothing: it settles as a task
// consequence, never a run transition.
func TestFeatureLoopManagerExecFailure(t *testing.T) {
	ctrl := &fakeController{runState: "launching"}
	td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
	ctrl.status = statusFromFakeRunState(ctrl)
	ctrl.corroborateSessions = func() ([]app.SessionLaunchProgress, error) {
		ctrl.setRunState("failed")
		return []app.SessionLaunchProgress{
			{SessionID: "child-session", Role: "implementer", Progress: app.LaunchFailed},
			{SessionID: "manager-session", Role: "manager", Progress: app.LaunchFailed},
		}, nil
	}
	var stdout, stderr bytes.Buffer

	code, err := finishFeatureControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout, &stderr, "hop resume")
	if err != nil {
		t.Fatalf("write error: %v", err)
	}
	if code != exitFailure || stderr.Len() != 0 {
		t.Fatalf("exit code = %d, stderr = %q; want 1 and nothing", code, stderr.String())
	}
	want := "run r1 launching\nlaunch " + describeLaunchProgress(app.LaunchFailed) + "\nrun r1 failed\n"
	if got := stdout.String(); got != want || want != "run r1 launching\nlaunch failed\nrun r1 failed\n" {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

// TestFeatureLoopKeepsRunningOverUnresolvedLaunches proves an attempt
// launch the pass could not complete — a worktree act error, a stop
// refusing a dispatch, an unattributable worktree operation — never ends
// the loop: each is printed as the app's fixed-text line, the next tick
// runs the pass again (where the app recovers the launch), and only a
// genuine error from AssignReadyTasks still ends the loop.
func TestFeatureLoopKeepsRunningOverUnresolvedLaunches(t *testing.T) {
	t.Run("reported launches are printed and later passes continue", func(t *testing.T) {
		ctrl := &fakeController{runState: runStateRunning}
		td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
		passes := 0
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			if passes >= 3 {
				ctrl.setRunState("completed")
			}
			return detailStep(ctrl.currentRunState(), "", false), nil
		}
		ctrl.assignReadyTasks = func(app.AssignmentOptions) (app.AssignmentReport, error) {
			passes++
			switch passes {
			case 1:
				return app.AssignmentReport{
					Launches: []app.AttemptLaunchCondition{{TaskSeq: 1, AttemptNumber: 1, Disposition: app.AttemptLaunchReconciling, Detail: "worktree creation did not complete"}},
					Blocked:  []string{"operation x names no attempt"},
				}, nil
			case 2:
				return app.AssignmentReport{Launches: []app.AttemptLaunchCondition{
					{TaskSeq: 1, AttemptNumber: 1, Disposition: app.AttemptLaunchOpened, Detail: "the attempt's pane was opened"},
					{TaskSeq: 2, AttemptNumber: 1, Disposition: app.AttemptLaunchStopRequested, Detail: "a stop request refused the worktree creation"},
				}}, nil
			default:
				return app.AssignmentReport{}, nil
			}
		}
		var stdout bytes.Buffer

		result, err := runFeatureControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
		if err != nil {
			t.Fatalf("runFeatureControllerLoop: %v", err)
		}
		if result.FinalState != "completed" || passes != 3 {
			t.Fatalf("result = %+v after %d passes, want completed after 3", result, passes)
		}
		want := "run r1 running\n" +
			"attempt t1a1 reconciling: worktree creation did not complete\n" +
			"worktree blocked: operation x names no attempt\n" +
			"attempt t1a1 opened: the attempt's pane was opened\n" +
			"attempt t2a1 stop-requested: a stop request refused the worktree creation\n" +
			"run r1 completed\n"
		if got := stdout.String(); got != want {
			t.Errorf("stdout = %q, want %q", got, want)
		}
	})

	t.Run("a store or lease error from assignment still ends the loop, after the report lines", func(t *testing.T) {
		ctrl := &fakeController{runState: runStateRunning}
		td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
		ctrl.status = statusFromFakeRunState(ctrl)
		ctrl.assignReadyTasks = func(app.AssignmentOptions) (app.AssignmentReport, error) {
			return app.AssignmentReport{
				Launches: []app.AttemptLaunchCondition{{TaskSeq: 1, AttemptNumber: 1, Disposition: app.AttemptLaunchSettled, Detail: "worktree creation failed"}},
			}, errors.New("app: lease fenced")
		}
		var stdout bytes.Buffer

		_, err := runFeatureControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
		if err == nil || !strings.Contains(err.Error(), "assign ready tasks: app: lease fenced") {
			t.Fatalf("err = %v, want the assignment error", err)
		}
		if got := stdout.String(); got != "run r1 running\nattempt t1a1 settled: worktree creation failed\n" {
			t.Errorf("stdout = %q", got)
		}
	})
}

// TestFeatureLoopContinuesPastAVanishedPane proves the loop keeps running
// when a pane vanishes: the presentation round reports the pane skipped
// (the app's tolerance of a pane the server no longer has), later passes
// run as usual — here the vanished child's launch settles failed — and
// the loop ends only on the run's own terminal state.
func TestFeatureLoopContinuesPastAVanishedPane(t *testing.T) {
	ctrl := &fakeController{runState: runStateRunning}
	td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
	passes := 0
	ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
		if passes >= 3 {
			ctrl.setRunState("completed")
		}
		return detailStep(ctrl.currentRunState(), "", false), nil
	}
	ctrl.corroborateSessions = func() ([]app.SessionLaunchProgress, error) {
		if passes == 2 {
			return []app.SessionLaunchProgress{{SessionID: "child", Role: "implementer", Progress: app.LaunchFailed}}, nil
		}
		return []app.SessionLaunchProgress{{SessionID: "child", Role: "implementer", Progress: app.LaunchPending}}, nil
	}
	ctrl.publishPresentation = func() (app.PresentationReport, error) {
		passes++
		if passes <= 2 {
			return app.PresentationReport{Published: []string{"w1:p1"}, Skipped: []string{"w2:p2"}}, nil
		}
		return app.PresentationReport{Published: []string{"w1:p1"}}, nil
	}
	var stdout bytes.Buffer

	result, err := runFeatureControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
	if err != nil {
		t.Fatalf("runFeatureControllerLoop: %v", err)
	}
	if result.FinalState != "completed" || passes != 3 {
		t.Fatalf("result = %+v after %d passes, want completed after 3", result, passes)
	}
	if got := countCalls(ctrl.recorded(), "PublishRunPresentation"); got != 3 {
		t.Errorf("PublishRunPresentation calls = %d, want one per pass", got)
	}
	if got, want := stdout.String(), "run r1 running\nrun r1 completed\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}
