package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/johnlanda/hop/internal/app"
)

// stringList is a repeatable string flag value.
type stringList []string

func (l *stringList) String() string { return fmt.Sprint([]string(*l)) }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

// resolveRepositoryRoot canonicalizes dir — the -C value, or the working
// directory when empty — into the design's repository identity: the
// symlink-resolved absolute path of the repository root.
func resolveRepositoryRoot(d *deps, dir string) (string, error) {
	if dir == "" {
		wd, err := d.getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working directory: %w", err)
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve repository path %q: %w", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve repository path %q: %w", dir, err)
	}
	return resolved, nil
}

// hopExecutablePath resolves the running hop binary's absolute path: the
// value frozen into pane commands and rendered into the assignment.
func hopExecutablePath(d *deps) (string, error) {
	path, err := d.executable()
	if err != nil {
		return "", fmt.Errorf("resolve the hop executable path: %w", err)
	}
	if !filepath.IsAbs(path) {
		abs, absErr := filepath.Abs(path)
		if absErr != nil {
			return "", fmt.Errorf("resolve the hop executable path: %w", absErr)
		}
		path = abs
	}
	return path, nil
}

// watchDetachSignals cancels cancel on the first SIGINT/SIGTERM — detach,
// never stop — and force-exits on a second signal during shutdown. The
// returned stop function releases the signal registration.
func watchDetachSignals(d *deps, cancel context.CancelFunc) (stop func()) {
	signals, stopNotify := d.notifySignals()
	go func() {
		if _, ok := <-signals; !ok {
			return
		}
		cancel()
		if _, ok := <-signals; !ok {
			return
		}
		d.forceExit()
	}()
	return stopNotify
}

// runRun implements `hop run "<brief>"`: it freezes and starts a run
// through Controller.StartRun, prints "run <seq-label> <uuid> started",
// then stays in the foreground controller loop until the run is terminal
// or a signal detaches. Exit 0 only on completed; 1 on failed, stopped,
// detach or error; 2 on usage, including every StartRun refusal that
// happens before any side effect.
func runRun(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop run", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	repoDir := flags.String("C", "", "repository directory (default: the working directory)")
	socketPath := flags.String("socket", "", "herdr server socket (default $HERDR_SOCKET_PATH)")
	// -herdr is accepted for parity with doctor's flag surface; the run
	// controller itself talks only to the server socket.
	_ = flags.String("herdr", "", "herdr binary (accepted as in doctor; unused by run)")
	var passthrough stringList
	flags.Var(&passthrough, "env-passthrough", "environment variable kept for the worker (repeatable; merged into the frozen policy)")
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() != 1 {
		_, err := fmt.Fprintln(stderr, "hop run: exactly one brief argument is required")
		return exitUsage, err
	}
	brief := flags.Arg(0)
	if *socketPath == "" {
		*socketPath = d.getenv("HERDR_SOCKET_PATH")
	}

	repoRoot, err := resolveRepositoryRoot(d, *repoDir)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop run: %v\n", err)
		return exitUsage, werr
	}
	stateRoot, _, err := resolveStateRoot(d.getenv)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop run: %v\n", err)
		return exitUsage, werr
	}
	hopPath, err := hopExecutablePath(d)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop run: %v\n", err)
		return exitFailure, werr
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopSignals := watchDetachSignals(d, cancel)
	defer stopSignals()

	ctrl, closeStore, err := d.openController(ctx, controllerConfig{stateRoot: stateRoot, socketPath: *socketPath, withRuntime: true})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop run: %s\n", describeStoreOpenFailure(err, "the resolved state root"))
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // the store closes on process exit either way; commands report command errors, not pool teardown.

	result, handle, err := ctrl.StartRun(ctx, app.StartRunRequest{
		RepositoryRoot: repoRoot,
		Brief:          brief,
		ControllerID:   d.newID(),
		StateRoot:      stateRoot,
		HOPPath:        hopPath,
		EnvPassthrough: passthrough,
	})
	if err != nil {
		code := exitFailure
		if errors.Is(err, app.ErrStartRefused) {
			code = exitUsage
		}
		_, werr := fmt.Fprintf(stderr, "hop run: %v\n", err)
		return code, werr
	}
	label := seqLabel(result.Sequence)
	if _, err := fmt.Fprintf(stdout, "run %s %s started\n", label, result.RunID); err != nil {
		return exitFailure, err
	}

	return finishControllerLoop(ctx, d, ctrl, handle, result.RunID, label, hopPath, stdout, stderr, "hop run")
}

// finishControllerLoop runs the shared foreground loop and maps its ending
// to the design's exit codes: the terminal state's code, or a detach (exit
// 1) that releases the lease and prints the resume instruction. A canceled
// foreground signal context is classified BEFORE ordinary errors: the
// cancellation surfaces through whichever app call was in flight (status,
// corroboration, a check), and that is still the detach path — the resume
// instruction is owed either way. Ordinary failures (a heartbeat fencing
// loss included) keep their error report when the foreground context is
// live.
func finishControllerLoop(ctx context.Context, d *deps, ctrl controllerAPI, handle app.RunHandle, runID, label, hopPath string, stdout, stderr io.Writer, command string) (int, error) { //nolint:gocritic // hugeParam: RunHandle is the app-defined opaque token, passed by value as every Controller method takes it.
	result, err := runControllerLoop(ctx, d, ctrl, handle, runID, label, hopPath, stdout)
	if err != nil {
		if ctx.Err() != nil {
			// The detach path still owes any non-cancellation failure the
			// loop carried out — a retention or recording error is never
			// silenced by the signal — before the resume instruction.
			if !errors.Is(err, context.Canceled) {
				if _, werr := fmt.Fprintf(stderr, "%s: %v\n", command, err); werr != nil {
					return exitFailure, werr
				}
			}
			return exitFailure, detachAndReport(ctx, ctrl, handle, runID, stdout)
		}
		releaseQuietly(ctx, ctrl, handle)
		_, werr := fmt.Fprintf(stderr, "%s: %v\n", command, err)
		return exitFailure, werr
	}
	if result.Detached {
		return exitFailure, detachAndReport(ctx, ctrl, handle, runID, stdout)
	}
	releaseQuietly(ctx, ctrl, handle)
	return exitForRunState(result.FinalState), nil
}
