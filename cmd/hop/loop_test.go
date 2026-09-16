package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

const testRunID = "11111111-1111-4111-8111-111111111111"

// detailStep scripts one Status reply for the loop.
func detailStep(state, attemptState string, stopRequested bool) app.StatusResult {
	return app.StatusResult{Detail: &app.RunDetailView{
		RunSummaryView: app.RunSummaryView{RunID: testRunID, Sequence: 1, State: state, StopRequested: stopRequested},
		AttemptState:   attemptState,
	}}
}

// scriptStatus returns a status fake serving steps in order, repeating the
// last one when calls outrun the script.
func scriptStatus(steps ...app.StatusResult) func(app.StatusRequest) (app.StatusResult, error) {
	i := 0
	return func(app.StatusRequest) (app.StatusResult, error) {
		if i >= len(steps) {
			return steps[len(steps)-1], nil
		}
		step := steps[i]
		i++
		return step, nil
	}
}

func TestRunHeartbeats(t *testing.T) {
	waits := 0
	d := &deps{wait: func(ctx context.Context, _ time.Duration) error {
		waits++
		if waits > 2 {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}}
	beats := 0
	ctrl := &fakeController{heartbeat: func() error {
		beats++
		if beats == 2 {
			return errors.New("lease is held by another controller")
		}
		return nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	failed := runHeartbeats(ctx, d, ctrl, app.RunHandle{})

	err := <-failed
	if err == nil || !strings.Contains(err.Error(), "lease is held") {
		t.Fatalf("heartbeat failure = %v", err)
	}
	if beats != 2 {
		t.Errorf("heartbeats = %d, want 2 (one success, one failure)", beats)
	}
}

func TestRunControllerLoop(t *testing.T) {
	t.Run("prints transitions, corroborates, runs checks and exits on the terminal state", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.corroborate = func() (app.LaunchProgress, error) { return app.LaunchSettled, nil }
		td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
		td.useCheckBarriers()
		// Explicit stepping, deterministic under any scheduler
		// interleaving: every poll wait consumes one posted-outcome
		// barrier token, so a round begins only once the previous round's
		// check outcome is already consumable — the loop can never outrun
		// the check goroutine. Call 1 reports nothing, call 2 is the
		// passing round; the status script turns the run completed only
		// once call 3 has begun (call 2's outcome was consumed, because a
		// new round starts only after consumption), and call 3 blocks
		// until the terminal drain cancels it.
		var checkCalls atomic.Int32
		ctrl.claimAndRunCheck = func(ctx context.Context, hopPath string, spawnEnv []string) (app.CheckReport, error) {
			if hopPath != "/opt/hop/bin/hop" {
				t.Errorf("hop path = %q", hopPath)
			}
			if len(spawnEnv) == 0 {
				t.Error("spawn env is empty; the sanitized environment must be passed through")
			}
			switch checkCalls.Add(1) {
			case 1:
				return app.CheckReport{}, nil
			case 2:
				return app.CheckReport{Ran: true, OperationID: "op-1", Passed: true}, nil
			default:
				// The final round blocks: its start signal releases the
				// wait barrier so the next status read can turn terminal,
				// and the terminal drain cancels this call.
				td.checkStarted <- struct{}{}
				<-ctx.Done()
				return app.CheckReport{}, nil
			}
		}
		statusCalls := 0
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			statusCalls++
			switch {
			case statusCalls == 1:
				return detailStep("launching", "launching", false), nil
			case checkCalls.Load() >= 3:
				return detailStep("completed", "completed", false), nil
			default:
				return detailStep("running", "running", false), nil
			}
		}
		var stdout bytes.Buffer

		result, err := runControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
		if err != nil {
			t.Fatalf("runControllerLoop: %v", err)
		}

		if result.Detached || result.FinalState != "completed" {
			t.Errorf("result = %+v", result)
		}
		out := stdout.String()
		for _, want := range []string{"run r1 launching\n", "launch settled\n", "run r1 running\n", "check op-1 passed\n", "run r1 completed\n"} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q; got:\n%s", want, out)
			}
		}
		if strings.Count(out, "launch settled") != 1 {
			t.Errorf("launch progress printed more than once on no change:\n%s", out)
		}
	})

	t.Run("the run-running line always prints even when a trivial check finishes before the next poll", func(t *testing.T) {
		// Reproduces the observed flake (TestRealProcessRunEndToEnd): a
		// tick corroborates the launch settled — moving the run to
		// running — and then, in that SAME tick, drives a check the
		// fixture worker already submitted for, which is claimed and
		// finishes before the loop's next poll. Without a mid-tick
		// re-read, the ordinary poll never observes "running" at all: it
		// jumps straight from "launching" to "completed". checkClaimed
		// gates the status fake exactly on the check being claimed (not
		// on any real elapsed time), and the check barrier makes the
		// claim happen-before the next tick's poll deterministically —
		// so this fails against the unfixed loop on every run, not just
		// under an unlucky interleaving.
		var (
			settled      atomic.Bool
			checkClaimed atomic.Bool
		)
		ctrl := &fakeController{}
		ctrl.corroborate = func() (app.LaunchProgress, error) {
			settled.Store(true)
			return app.LaunchSettled, nil
		}
		td := newTestDeps(ctrl, map[string]string{"PATH": "/bin"}, t.TempDir())
		td.useCheckBarriers()
		ctrl.claimAndRunCheck = func(_ context.Context, _ string, _ []string) (app.CheckReport, error) {
			checkClaimed.Store(true)
			td.checkStarted <- struct{}{}
			return app.CheckReport{Ran: true, OperationID: "op-1", Passed: true}, nil
		}
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			switch {
			case !settled.Load():
				return detailStep("launching", "launching", false), nil
			case !checkClaimed.Load():
				return detailStep("running", "running", false), nil
			default:
				return detailStep("completed", "completed", false), nil
			}
		}
		var stdout bytes.Buffer

		result, err := runControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
		if err != nil {
			t.Fatalf("runControllerLoop: %v", err)
		}

		if result.Detached || result.FinalState != "completed" {
			t.Errorf("result = %+v", result)
		}
		out := stdout.String()
		settledIdx := strings.Index(out, "launch settled\n")
		runningIdx := strings.Index(out, "run r1 running\n")
		completedIdx := strings.Index(out, "run r1 completed\n")
		if settledIdx == -1 || runningIdx == -1 || completedIdx == -1 {
			t.Fatalf("output missing an expected line; got:\n%s", out)
		}
		if settledIdx >= runningIdx || runningIdx >= completedIdx {
			t.Errorf("want \"launch settled\" < \"run r1 running\" < \"run r1 completed\"; got:\n%s", out)
		}
	})

	t.Run("a stop request interrupts a running check before the stop is driven", func(t *testing.T) {
		ctrl := &fakeController{}
		td := newTestDeps(ctrl, nil, t.TempDir())
		td.useCheckBarriers()
		var checkStarted atomic.Bool
		ctrl.claimAndRunCheck = func(ctx context.Context, _ string, _ []string) (app.CheckReport, error) {
			checkStarted.Store(true)
			td.checkStarted <- struct{}{} // barrier: the round after this one observes the stop
			// The check blocks until the loop cancels its context — the
			// long-check case; the app then records the outcome under stop
			// precedence and returns the interrupted report.
			<-ctx.Done()
			return app.CheckReport{Ran: true, OperationID: "op-9", Interrupted: true}, nil
		}
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			// The stop request appears only once the check is already
			// running: the loop must observe it mid-check.
			return detailStep("running", "running", checkStarted.Load()), nil
		}
		ctrl.driveStop = func() (app.StopReport, error) {
			return app.StopReport{RunState: "stopped", Terminated: true}, nil
		}
		var stdout bytes.Buffer

		result, err := runControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
		if err != nil {
			t.Fatalf("runControllerLoop: %v", err)
		}

		if result.FinalState != "stopped" {
			t.Errorf("result = %+v", result)
		}
		out := stdout.String()
		if !strings.Contains(out, "check op-9 interrupted\n") {
			t.Errorf("output lacks the interrupted check outcome:\n%s", out)
		}
		calls := ctrl.recorded()
		checkIdx, stopIdx := -1, -1
		for i, name := range calls {
			if name == "ClaimAndRunCheck" && checkIdx == -1 {
				checkIdx = i
			}
			if name == "DriveStop" && stopIdx == -1 {
				stopIdx = i
			}
		}
		if checkIdx == -1 || stopIdx == -1 || stopIdx < checkIdx {
			t.Errorf("calls = %v; the check must be interrupted before the stop rounds", calls)
		}
	})

	t.Run("a retention failure on an interrupted check surfaces, never silenced by the stop", func(t *testing.T) {
		ctrl := &fakeController{}
		td := newTestDeps(ctrl, nil, t.TempDir())
		td.useCheckBarriers()
		var checkStarted atomic.Bool
		ctrl.claimAndRunCheck = func(ctx context.Context, _ string, _ []string) (app.CheckReport, error) {
			checkStarted.Store(true)
			td.checkStarted <- struct{}{}
			<-ctx.Done()
			// The app canceled the command but could not retain its
			// output: a real failure, deliberately not a plain
			// cancellation shape.
			return app.CheckReport{Ran: true, OperationID: "op-9"}, errors.New("app: check output retention failed after an ambiguous execution (spawn error: killed): disk full")
		}
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return detailStep("running", "running", checkStarted.Load()), nil
		}
		var stdout bytes.Buffer

		_, err := runControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)

		if err == nil || !strings.Contains(err.Error(), "retention failed") {
			t.Fatalf("err = %v, want the retention failure surfaced instead of a silent stop", err)
		}
	})

	t.Run("a stop request routes to DriveStop until termination is observed", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.status = scriptStatus(detailStep("running", "running", true))
		stopRounds := 0
		ctrl.driveStop = func() (app.StopReport, error) {
			stopRounds++
			if stopRounds < 2 {
				return app.StopReport{RunState: "stopping", Outstanding: []string{"worker pane close dispatched"}}, nil
			}
			return app.StopReport{RunState: "stopped", Terminated: true}, nil
		}
		td := newTestDeps(ctrl, nil, t.TempDir())
		var stdout bytes.Buffer

		result, err := runControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
		if err != nil {
			t.Fatalf("runControllerLoop: %v", err)
		}

		if result.FinalState != "stopped" {
			t.Errorf("result = %+v", result)
		}
		if stopRounds != 2 {
			t.Errorf("stop rounds = %d, want 2", stopRounds)
		}
		for _, name := range ctrl.recorded() {
			if name == "ClaimAndRunCheck" || name == "CorroborateLaunch" {
				t.Errorf("%s dispatched while a stop request was pending", name)
			}
		}
		if !strings.Contains(stdout.String(), "run r1 stopped\n") {
			t.Errorf("output lacks the stopped transition:\n%s", stdout.String())
		}
	})

	t.Run("context cancellation detaches instead of stopping", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ctrl := &fakeController{}
		statusCalls := 0
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			statusCalls++
			if statusCalls == 2 {
				cancel()
			}
			return detailStep("running", "running", false), nil
		}
		td := newTestDeps(ctrl, nil, t.TempDir())
		var stdout bytes.Buffer

		result, err := runControllerLoop(ctx, td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)
		if err != nil {
			t.Fatalf("runControllerLoop: %v", err)
		}

		if !result.Detached {
			t.Errorf("result = %+v, want a detach", result)
		}
		for _, name := range ctrl.recorded() {
			if name == "DriveStop" || name == "RequestStop" {
				t.Errorf("%s called on detach; a signal never stops the run", name)
			}
		}
	})

	t.Run("a failed heartbeat ends the loop with the lease loss", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.status = scriptStatus(detailStep("running", "running", false))
		ctrl.heartbeat = func() error { return errors.New("fenced") }
		td := newTestDeps(ctrl, nil, t.TempDir())
		// Let the heartbeat goroutine run: its interval wait returns
		// immediately instead of blocking.
		td.deps.wait = func(ctx context.Context, _ time.Duration) error {
			return ctx.Err()
		}
		var stdout bytes.Buffer

		_, err := runControllerLoop(context.Background(), td.deps, ctrl, app.RunHandle{}, testRunID, "r1", "/opt/hop/bin/hop", &stdout)

		if err == nil || !strings.Contains(err.Error(), "heartbeat failed") {
			t.Fatalf("err = %v, want the heartbeat failure", err)
		}
	})
}

func TestNonCancellationCauses(t *testing.T) {
	retention := errors.New("retention failed: disk full")
	cases := []struct {
		name string
		err  error
		want string // "" means nil: the tree was entirely cancellation
	}{
		{name: "nil", err: nil, want: ""},
		{name: "bare cancellation", err: context.Canceled, want: ""},
		{name: "wrapped cancellation", err: fmt.Errorf("load run status: %w", context.Canceled), want: ""},
		{name: "bare failure survives", err: retention, want: "retention failed"},
		{name: "join keeps only survivors", err: errors.Join(context.Canceled, retention), want: "retention failed"},
		{name: "all-cancellation join", err: errors.Join(context.Canceled, fmt.Errorf("wait: %w", context.Canceled)), want: ""},
		{
			name: "wrapped mixed join keeps the wrapper whole",
			err:  fmt.Errorf("run check: %w", errors.Join(context.Canceled, retention)),
			want: "run check: ",
		},
		{
			name: "nested wrapped mixed join survives",
			err:  fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", errors.Join(context.Canceled, retention))),
			want: "outer: inner: ",
		},
		{
			name: "wrapped all-cancellation join is silent",
			err:  fmt.Errorf("outer: %w", errors.Join(context.Canceled, context.Canceled)),
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nonCancellationCauses(tc.err)

			if tc.want == "" {
				if got != nil {
					t.Fatalf("nonCancellationCauses(%v) = %v, want nil", tc.err, got)
				}
				return
			}
			if got == nil || !strings.Contains(got.Error(), tc.want) {
				t.Fatalf("nonCancellationCauses(%v) = %v, want it to contain %q", tc.err, got, tc.want)
			}
			if !strings.Contains(got.Error(), "retention failed") {
				t.Fatalf("nonCancellationCauses(%v) = %v; the surviving cause was lost", tc.err, got)
			}
		})
	}
}

func TestExitForRunState(t *testing.T) {
	cases := []struct {
		state string
		want  int
	}{
		{state: "completed", want: exitOK},
		{state: "failed", want: exitFailure},
		{state: "stopped", want: exitFailure},
		{state: "resuming", want: exitFailure},
	}
	for _, tc := range cases {
		if got := exitForRunState(tc.state); got != tc.want {
			t.Errorf("exitForRunState(%q) = %d, want %d", tc.state, got, tc.want)
		}
	}
}

func TestSeqLabels(t *testing.T) {
	if got := seqLabel(7); got != "r7" {
		t.Errorf("seqLabel(7) = %q", got)
	}
	for arg, want := range map[string]bool{
		"r1": true, "r42": true, "r": false, "run1": false,
		"11111111-1111-4111-8111-111111111111": false, "r1x": false, "": false,
	} {
		if got := isSeqLabel(arg); got != want {
			t.Errorf("isSeqLabel(%q) = %v, want %v", arg, got, want)
		}
	}
}

func TestResolveRunArg(t *testing.T) {
	ctrl := &fakeController{}
	ctrl.status = func(req app.StatusRequest) (app.StatusResult, error) {
		if req.RepositoryRoot == "" {
			t.Error("label resolution must list the repository's runs")
		}
		return app.StatusResult{Runs: []app.RunSummaryView{
			{RunID: testRunID, Sequence: 1, State: "completed"},
			{RunID: "22222222-2222-4222-8222-222222222222", Sequence: 2, State: "running"},
		}}, nil
	}

	if got, err := resolveRunArg(context.Background(), ctrl, "/repo", "r2"); err != nil || got != "22222222-2222-4222-8222-222222222222" {
		t.Errorf("resolveRunArg(r2) = %q, %v", got, err)
	}
	// A terminal run's label still resolves: the listing filter is a
	// rendering choice, never a resolution one.
	if got, err := resolveRunArg(context.Background(), ctrl, "/repo", "r1"); err != nil || got != testRunID {
		t.Errorf("resolveRunArg(r1) = %q, %v", got, err)
	}
	if got, err := resolveRunArg(context.Background(), ctrl, "/repo", testRunID); err != nil || got != testRunID {
		t.Errorf("resolveRunArg(uuid) = %q, %v (a UUID passes through untouched)", got, err)
	}
	if _, err := resolveRunArg(context.Background(), ctrl, "/repo", "r9"); err == nil {
		t.Error("an unknown label resolved")
	}
}
