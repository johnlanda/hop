package main

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// fakeController is a scripted controllerAPI: each behavior field replaces
// one method, and a nil field reports an unexpected call. Calls are
// recorded under a mutex because Heartbeat arrives from the loop's
// concurrent heartbeat goroutine.
type fakeController struct {
	mu    sync.Mutex
	calls []string
	// lastStatusCtx is the context of the newest Status call, so a
	// scripted status fake can observe the foreground cancellation the
	// command created (the fake signature itself carries no context).
	lastStatusCtx context.Context //nolint:containedctx // test-only capture of the call's context for deterministic cancellation scripting.

	startRun         func(req app.StartRunRequest) (app.StartRunResult, app.RunHandle, error)
	resume           func(req app.ResumeRequest) (app.ResumeResult, app.RunHandle, error)
	status           func(req app.StatusRequest) (app.StatusResult, error)
	requestStop      func(runID string) error
	driveStop        func() (app.StopReport, error)
	heartbeat        func() error
	detach           func() error
	corroborate      func() (app.LaunchProgress, error)
	claimAndRunCheck func(ctx context.Context, hopPath string, spawnEnv []string) (app.CheckReport, error)
	checkSpawnEnv    func(environ []string) ([]string, error)
	submitResult     func(req app.SubmitResultRequest) (app.SubmitResultResult, error)
	prepareLaunch    func(req app.LaunchExecRequest) (app.LaunchExecPlan, error)
	failLaunch       func(incarnationID, reason string) error
	prepareCheck     func(req app.CheckExecRequest) (app.CheckExecPlan, error)
}

func (f *fakeController) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

// recorded returns a snapshot of the calls seen so far.
func (f *fakeController) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeController) StartRun(_ context.Context, req app.StartRunRequest) (app.StartRunResult, app.RunHandle, error) { //nolint:gocritic // hugeParam: the fake mirrors the controllerAPI signature.
	f.record("StartRun")
	if f.startRun == nil {
		return app.StartRunResult{}, app.RunHandle{}, errors.New("unexpected StartRun")
	}
	return f.startRun(req)
}

func (f *fakeController) Resume(_ context.Context, req app.ResumeRequest) (app.ResumeResult, app.RunHandle, error) {
	f.record("Resume")
	if f.resume == nil {
		return app.ResumeResult{}, app.RunHandle{}, errors.New("unexpected Resume")
	}
	return f.resume(req)
}

func (f *fakeController) Status(ctx context.Context, req app.StatusRequest) (app.StatusResult, error) {
	f.mu.Lock()
	f.lastStatusCtx = ctx
	f.mu.Unlock()
	f.record("Status")
	if f.status == nil {
		return app.StatusResult{}, errors.New("unexpected Status")
	}
	return f.status(req)
}

// statusCtx returns the newest Status call's context.
func (f *fakeController) statusCtx() context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastStatusCtx
}

func (f *fakeController) RequestStop(_ context.Context, runID string) error {
	f.record("RequestStop")
	if f.requestStop == nil {
		return errors.New("unexpected RequestStop")
	}
	return f.requestStop(runID)
}

func (f *fakeController) DriveStop(_ context.Context, _ app.RunHandle) (app.StopReport, error) { //nolint:gocritic // hugeParam: the fake mirrors the controllerAPI signature.
	f.record("DriveStop")
	if f.driveStop == nil {
		return app.StopReport{}, errors.New("unexpected DriveStop")
	}
	return f.driveStop()
}

func (f *fakeController) Heartbeat(_ context.Context, _ app.RunHandle) error { //nolint:gocritic // hugeParam: the fake mirrors the controllerAPI signature.
	f.record("Heartbeat")
	if f.heartbeat == nil {
		return nil
	}
	return f.heartbeat()
}

func (f *fakeController) Detach(_ context.Context, _ app.RunHandle) error { //nolint:gocritic // hugeParam: the fake mirrors the controllerAPI signature.
	f.record("Detach")
	if f.detach == nil {
		return nil
	}
	return f.detach()
}

func (f *fakeController) CorroborateLaunch(_ context.Context, _ app.RunHandle) (app.LaunchProgress, error) { //nolint:gocritic // hugeParam: the fake mirrors the controllerAPI signature.
	f.record("CorroborateLaunch")
	if f.corroborate == nil {
		return app.LaunchPending, nil
	}
	return f.corroborate()
}

func (f *fakeController) ClaimAndRunCheck(ctx context.Context, _ app.RunHandle, hopPath string, spawnEnv []string) (app.CheckReport, error) { //nolint:gocritic // hugeParam: the fake mirrors the controllerAPI signature.
	f.record("ClaimAndRunCheck")
	if f.claimAndRunCheck == nil {
		return app.CheckReport{}, nil
	}
	return f.claimAndRunCheck(ctx, hopPath, spawnEnv)
}

func (f *fakeController) CheckSpawnEnvironment(_ context.Context, _ app.RunHandle, environ []string) ([]string, error) { //nolint:gocritic // hugeParam: the fake mirrors the controllerAPI signature.
	f.record("CheckSpawnEnvironment")
	if f.checkSpawnEnv == nil {
		return environ, nil
	}
	return f.checkSpawnEnv(environ)
}

func (f *fakeController) SubmitResult(_ context.Context, req app.SubmitResultRequest) (app.SubmitResultResult, error) { //nolint:gocritic // hugeParam: the fake mirrors the controllerAPI signature.
	f.record("SubmitResult")
	if f.submitResult == nil {
		return app.SubmitResultResult{}, errors.New("unexpected SubmitResult")
	}
	return f.submitResult(req)
}

func (f *fakeController) PrepareLaunchExec(_ context.Context, req app.LaunchExecRequest) (app.LaunchExecPlan, error) { //nolint:gocritic // hugeParam: the fake mirrors the controllerAPI signature.
	f.record("PrepareLaunchExec")
	if f.prepareLaunch == nil {
		return app.LaunchExecPlan{}, errors.New("unexpected PrepareLaunchExec")
	}
	return f.prepareLaunch(req)
}

func (f *fakeController) FailLaunchExec(_ context.Context, incarnationID, reason string) error {
	f.record("FailLaunchExec")
	if f.failLaunch == nil {
		return errors.New("unexpected FailLaunchExec")
	}
	return f.failLaunch(incarnationID, reason)
}

func (f *fakeController) PrepareCheckExec(_ context.Context, req app.CheckExecRequest) (app.CheckExecPlan, error) { //nolint:gocritic // hugeParam: the fake mirrors the controllerAPI signature.
	f.record("PrepareCheckExec")
	if f.prepareCheck == nil {
		return app.CheckExecPlan{}, errors.New("unexpected PrepareCheckExec")
	}
	return f.prepareCheck(req)
}

// execCall records one exec-seam invocation.
type execCall struct {
	Path string
	Argv []string
	Env  []string
}

// testDeps builds a deps whose every seam is a fake: the clock is fixed
// and advanced only by wait, wait never sleeps (a heartbeat-interval wait
// blocks until the context ends so the concurrent heartbeat goroutine
// stays quiet unless a test drives it), and exec calls are recorded
// instead of replacing the process.
type testDeps struct {
	deps *deps
	ctrl *fakeController

	mu        sync.Mutex
	now       time.Time
	waits     int
	execs     []execCall
	openErr   error
	closed    int
	signals   chan os.Signal
	openCalls []controllerConfig

	// checkPosted receives one token per asynchronous check outcome the
	// loop's driver has posted (via deps.checkOutcomePosted), and
	// checkStarted receives tokens a test's check fake sends on entry.
	// useCheckBarriers switches the poll wait to consume these tokens, so
	// the loop advances one round per observable event instead of
	// free-running against the scheduler.
	checkPosted  chan struct{}
	checkStarted chan struct{}
}

// newTestDeps wires fakes around ctrl. env supplies getenv; the working
// directory defaults to dir.
func newTestDeps(ctrl *fakeController, env map[string]string, dir string) *testDeps {
	td := &testDeps{
		ctrl:         ctrl,
		now:          time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC),
		signals:      make(chan os.Signal, 2),
		checkPosted:  make(chan struct{}, 64),
		checkStarted: make(chan struct{}, 64),
	}
	td.deps = &deps{
		getenv:     mapGetenv(env),
		environ:    func() []string { return environFromMap(env) },
		getwd:      func() (string, error) { return dir, nil },
		executable: func() (string, error) { return "/opt/hop/bin/hop", nil },
		getpid:     func() int { return 4242 },
		leadsGroup: func() bool { return true },
		now: func() time.Time {
			td.mu.Lock()
			defer td.mu.Unlock()
			return td.now
		},
		wait: func(ctx context.Context, d time.Duration) error {
			if d == heartbeatInterval {
				<-ctx.Done()
				return ctx.Err()
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			td.mu.Lock()
			td.now = td.now.Add(d)
			td.waits++
			td.mu.Unlock()
			return nil
		},
		checkOutcomePosted: func() { td.checkPosted <- struct{}{} },
		notifySignals: func() (<-chan os.Signal, func()) {
			return td.signals, func() {}
		},
		openController: func(_ context.Context, cfg controllerConfig) (controllerAPI, func() error, error) {
			td.mu.Lock()
			td.openCalls = append(td.openCalls, cfg)
			td.mu.Unlock()
			if td.openErr != nil {
				return nil, nil, td.openErr
			}
			return ctrl, func() error {
				td.mu.Lock()
				defer td.mu.Unlock()
				td.closed++
				return nil
			}, nil
		},
		exec: func(argv, env []string) error {
			td.mu.Lock()
			defer td.mu.Unlock()
			td.execs = append(td.execs, execCall{Argv: argv, Env: env})
			return errors.New("exec recorded by the test seam; nothing replaced")
		},
		execResolved: func(path string, argv, env []string) error {
			td.mu.Lock()
			defer td.mu.Unlock()
			td.execs = append(td.execs, execCall{Path: path, Argv: argv, Env: env})
			return errors.New("exec recorded by the test seam; nothing replaced")
		},
		newID: func() string { return "aaaaaaaa-0000-4000-8000-000000000001" },
		forceExit: func() {
			panic("unexpected force exit")
		},
	}
	return td
}

// useCheckBarriers replaces the poll wait with a channel barrier: each
// poll-interval wait consumes one observable event — a posted check
// outcome, a check fake's start signal, or the context's end — so a test
// scenario advances by explicit steps and can never outrun the check
// goroutine, under any scheduler interleaving or shuffle seed. Heartbeat
// waits keep blocking on the context.
func (td *testDeps) useCheckBarriers() {
	td.deps.wait = func(ctx context.Context, d time.Duration) error {
		if d == heartbeatInterval {
			<-ctx.Done()
			return ctx.Err()
		}
		select {
		case <-td.checkPosted:
		case <-td.checkStarted:
		case <-ctx.Done():
			return ctx.Err()
		}
		td.mu.Lock()
		td.now = td.now.Add(d)
		td.waits++
		td.mu.Unlock()
		return nil
	}
}

// recordedExecs snapshots the exec-seam calls.
func (td *testDeps) recordedExecs() []execCall {
	td.mu.Lock()
	defer td.mu.Unlock()
	return append([]execCall(nil), td.execs...)
}

// environFromMap renders env as NAME=value entries in unspecified order.
func environFromMap(env map[string]string) []string {
	entries := make([]string, 0, len(env))
	for name, value := range env {
		entries = append(entries, name+"="+value)
	}
	return entries
}
