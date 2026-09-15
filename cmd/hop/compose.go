package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/johnlanda/hop/internal/adapters/config"
	"github.com/johnlanda/hop/internal/adapters/herdr"
	"github.com/johnlanda/hop/internal/adapters/process"
	"github.com/johnlanda/hop/internal/adapters/sqlite"
	"github.com/johnlanda/hop/internal/adapters/system"
	"github.com/johnlanda/hop/internal/app"
)

// The composition root wires the Herdr runtime adapter into the app.Runtime
// port; the assertion pins that contract at compile time.
var _ app.Runtime = (*herdr.Runtime)(nil)

// controllerAPI is the slice of *app.Controller the commands drive. The
// commands depend on this interface so their tests substitute a scripted
// fake for the loop-driving methods, or wire a real *app.Controller over
// handwritten port fakes for the exec boundaries.
type controllerAPI interface {
	StartRun(ctx context.Context, req app.StartRunRequest) (app.StartRunResult, app.RunHandle, error)
	Resume(ctx context.Context, req app.ResumeRequest) (app.ResumeResult, app.RunHandle, error)
	Status(ctx context.Context, req app.StatusRequest) (app.StatusResult, error)
	RequestStop(ctx context.Context, runID string) error
	DriveStop(ctx context.Context, handle app.RunHandle) (app.StopReport, error)
	Heartbeat(ctx context.Context, handle app.RunHandle) error
	Detach(ctx context.Context, handle app.RunHandle) error
	CorroborateLaunch(ctx context.Context, handle app.RunHandle) (app.LaunchProgress, error)
	ClaimAndRunCheck(ctx context.Context, handle app.RunHandle, hopPath string, spawnEnv []string) (app.CheckReport, error)
	CheckSpawnEnvironment(ctx context.Context, handle app.RunHandle, environ []string) ([]string, error)
	SubmitResult(ctx context.Context, req app.SubmitResultRequest) (app.SubmitResultResult, error)
	PrepareLaunchExec(ctx context.Context, req app.LaunchExecRequest) (app.LaunchExecPlan, error)
	FailLaunchExec(ctx context.Context, incarnationID, reason string) error
	PrepareCheckExec(ctx context.Context, req app.CheckExecRequest) (app.CheckExecPlan, error)
}

var _ controllerAPI = (*app.Controller)(nil)

// controllerConfig selects what one command's controller needs.
type controllerConfig struct {
	// stateRoot is the absolute state root the store opens under.
	stateRoot string
	// socketPath selects the Herdr server socket; consulted only when
	// withRuntime is set.
	socketPath string
	// withRuntime wires the Herdr runtime adapter. Commands that never act
	// on panes or worktrees (status, result submit, the exec boundaries)
	// leave it unset and the Runtime port nil.
	withRuntime bool
}

// deps carries every effectful constructor and process fact the Phase 2
// commands consume, so command tests substitute fakes without opening real
// adapters or replacing the test process. defaultDeps builds the
// production wiring; tests build the struct directly.
type deps struct {
	getenv         func(string) string
	environ        func() []string
	getwd          func() (string, error)
	executable     func() (string, error)
	getpid         func() int
	leadsGroup     func() bool
	now            func() time.Time
	wait           func(ctx context.Context, d time.Duration) error
	notifySignals  func() (stream <-chan os.Signal, stop func())
	openController func(ctx context.Context, cfg controllerConfig) (controllerAPI, func() error, error)
	exec           func(argv, env []string) error
	execResolved   func(path string, argv, env []string) error
	newID          func() string
	// forceExit ends the process immediately: the second SIGINT/SIGTERM
	// during a detach shutdown exits without waiting for the release.
	forceExit func()
	// checkOutcomePosted, when non-nil, is called by the loop's check
	// driver after an asynchronous check outcome has been posted for
	// consumption. Production wiring leaves it nil; loop tests use it as
	// the channel barrier that makes their pacing deterministic under any
	// scheduler interleaving.
	checkOutcomePosted func()
}

// defaultDeps is the production wiring of deps.
func defaultDeps() *deps {
	return &deps{
		getenv:     os.Getenv,
		environ:    os.Environ,
		getwd:      os.Getwd,
		executable: os.Executable,
		getpid:     os.Getpid,
		leadsGroup: func() bool { return syscall.Getpgrp() == os.Getpid() },
		now:        time.Now,
		wait:       waitInterval,
		notifySignals: func() (<-chan os.Signal, func()) {
			ch := make(chan os.Signal, 2)
			signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
			return ch, func() { signal.Stop(ch) }
		},
		openController: openController,
		exec:           process.Exec,
		execResolved:   process.ExecResolved,
		newID:          system.IDGenerator{}.NewID,
		forceExit:      func() { os.Exit(exitFailure) },
	}
}

// openController opens the SQLite store under cfg.stateRoot (creating the
// state root with restrictive permissions on first use) and constructs the
// application controller exactly once for the calling command, wiring the
// concrete adapters into their ports. The returned closer releases the
// store's connection pools.
func openController(ctx context.Context, cfg controllerConfig) (controllerAPI, func() error, error) {
	// The controller's own git binary is resolved once here, against the
	// controller process's own PATH (os.Getenv("PATH") — never the
	// sanitized worker environment a launched harness or check runs under):
	// CommandRunner.Run requires an absolute argv[0], and a bare "git"
	// never pins which binary runs.
	gitExecutable, err := lookupExecutable("git", os.Getenv("PATH"))
	if err != nil {
		return nil, nil, errors.New("git executable not found on PATH")
	}
	store, err := sqlite.Open(ctx, cfg.stateRoot, sqlite.Options{})
	if err != nil {
		return nil, nil, fmt.Errorf("open state store: %w", err)
	}
	controller := &app.Controller{
		Store:         store,
		Read:          store,
		Submissions:   store,
		Artifacts:     system.ArtifactStore{},
		Clock:         system.Clock{},
		IDs:           system.IDGenerator{},
		Commands:      process.Runner{},
		Groups:        process.GroupInspector{},
		Config:        config.Source{},
		GitExecutable: gitExecutable,
	}
	if cfg.withRuntime {
		controller.Runtime = herdr.NewRuntime(cfg.socketPath)
	}
	return controller, store.Close, nil
}

// describeStoreOpenFailure classifies a store-open failure into a fixed,
// value-free diagnostic: the raw error chain carries the state-root path —
// in a worker context, the complete HOP_STATE_DIR value — so the wrapped
// error is never printed. location names where the root came from
// ("HOP_STATE_DIR" for worker commands; "the resolved state root" for
// controller commands, whose path hop doctor prints by design). Only
// error CATEGORIES established through errors.Is are surfaced.
func describeStoreOpenFailure(err error, location string) string {
	category := "the state root could not be created or the database could not be opened"
	switch {
	case errors.Is(err, syscall.ENOTDIR):
		category = "a path element is not a directory"
	case errors.Is(err, os.ErrPermission):
		category = "permission denied creating or opening the store"
	case errors.Is(err, os.ErrNotExist):
		category = "a required path element does not exist"
	}
	return fmt.Sprintf("cannot open the state store under %s: %s (the configured value is never echoed; run hop doctor to inspect the resolved state root)", location, category)
}

// waitInterval blocks for d or until ctx is done, returning ctx's error in
// that case. It is the production pacing primitive of the controller loop;
// tests substitute a fake that advances a fake clock instead of sleeping.
func waitInterval(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// lookupExecutable resolves name to the absolute path of the binary that
// would run, implementing app.ExecutableLookup for the exec-boundary
// commands: a name containing a path separator is resolved against the
// working directory and verified directly; a bare name is searched through
// pathValue — the SANITIZED environment's PATH, exactly as the launched
// process would resolve it, with an empty PATH element meaning the working
// directory as execvp does. A candidate must be a regular file with an
// execute bit.
func lookupExecutable(name, pathValue string) (string, error) {
	if name == "" {
		return "", errors.New("executable name is empty")
	}
	if strings.ContainsRune(name, os.PathSeparator) {
		abs, err := filepath.Abs(name)
		if err != nil {
			return "", fmt.Errorf("resolve %q: %w", name, err)
		}
		if err := verifyExecutable(abs); err != nil {
			return "", err
		}
		return abs, nil
	}
	for _, dir := range strings.Split(pathValue, string(os.PathListSeparator)) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, name)
		if abs, err := filepath.Abs(candidate); err == nil && verifyExecutable(abs) == nil {
			return abs, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in the sanitized PATH", name)
}

// verifyExecutable reports whether path names an executable regular file.
func verifyExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() || !info.Mode().IsRegular() {
		return fmt.Errorf("%q is not a regular file", path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%q is not executable", path)
	}
	return nil
}
