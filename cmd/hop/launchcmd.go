package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"syscall"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// defaultLaunchPrepTimeout bounds hop launch's pre-exec store work. The
// exec itself replaces the process and is not bounded by any context.
const defaultLaunchPrepTimeout = 30 * time.Second

// runLaunch implements `hop launch --run <uuid> --attempt <uuid>` (the
// permanent solo shim, slice 4) and `hop launch --run <uuid> --session
// <uuid>` (every feature-mode pane), the worker exec boundary
// (docs/plan/phase-3-design.md section 6, generalizing
// docs/plan/phase-2-design.md section 6): it requires the launch-provided
// absolute HOP_STATE_DIR (never falling back to the default resolution),
// resolves its own working directory to the symlink-resolved worktree (or,
// for the manager, repository root) path the workspace-trust seed keys on,
// prepares the exec through the application — session context load,
// environment validation, sanitization under the frozen policy, per-role
// argv composition, executable resolution, the workspace-trust pre-seed
// with its recorded evidence, the session-keyed exec_pending claim — and
// execs through the process adapter. The two forms are mutually exclusive;
// solo-mode panes keep the Phase 2 argv byte-identical. On success it
// never returns. Every failure exits 1 with one stderr line that never
// echoes an environment value; a failure after the claim (the exec itself)
// settles the claim exec_failed first.
func runLaunch(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	_ = stdout // hop launch's success is an exec; it writes only diagnostics.
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop launch", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	runID := flags.String("run", "", "run id (required)")
	attemptID := flags.String("attempt", "", "attempt id (the permanent solo shim; mutually exclusive with --session)")
	sessionID := flags.String("session", "", "session id (every feature-mode pane; mutually exclusive with --attempt)")
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() > 0 {
		_, err := fmt.Fprintf(stderr, "hop launch: unexpected argument %q\n", flags.Arg(0))
		return exitUsage, err
	}
	if *runID == "" {
		_, err := fmt.Fprintln(stderr, "hop launch: --run is required")
		return exitUsage, err
	}
	if (*attemptID == "") == (*sessionID == "") {
		_, err := fmt.Fprintln(stderr, "hop launch: exactly one of --attempt and --session is required")
		return exitUsage, err
	}

	stateRoot, err := requireWorkerStateRoot(d.getenv)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop launch: %v\n", err)
		return exitFailure, werr
	}
	hopPath, err := hopExecutablePath(d)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop launch: %v\n", err)
		return exitFailure, werr
	}
	workerDir, err := launcherWorkerDir(d)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop launch: %v\n", err)
		return exitFailure, werr
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultLaunchPrepTimeout)
	defer cancel()
	ctrl, closeStore, err := d.openController(ctx, controllerConfig{stateRoot: stateRoot})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop launch: %s\n", describeStoreOpenFailure(err, "HOP_STATE_DIR"))
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // an exec success never reaches this; on failure the store closes on process exit either way.

	plan, err := ctrl.PrepareSessionLaunchExec(ctx, app.SessionLaunchExecRequest{
		RunID:            *runID,
		AttemptID:        *attemptID,
		SessionID:        *sessionID,
		HOPPath:          hopPath,
		WorkerDir:        workerDir,
		Environ:          d.environ(),
		PID:              d.getpid(),
		ResolvePath:      resolveCanonicalPath,
		LookupExecutable: lookupExecutable,
	})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop launch: %v\n", err)
		return exitFailure, werr
	}

	// From here the claim exists: any failure settles it exec_failed so
	// recovery sees a dead incarnation, not an ambiguous one.
	execErr := d.exec(plan.Argv, plan.Env)
	if settleErr := ctrl.FailLaunchExec(ctx, plan.IncarnationID, execErr.Error()); settleErr != nil {
		// The settlement write itself was lost: the claim stays
		// exec_pending and recovery treats it as ambiguous — reported, not
		// hidden.
		_, werr := fmt.Fprintf(stderr, "hop launch: exec failed and the exec_failed settlement could not be recorded (%v); the claim stays exec_pending and recovery treats it as ambiguous: %v\n", settleErr, execErr)
		return exitFailure, werr
	}
	_, werr := fmt.Fprintf(stderr, "hop launch: %v\n", execErr)
	return exitFailure, werr
}

// launcherWorkerDir resolves the launcher's own working directory — the
// attempt worktree its pane was created at — to the symlink-resolved
// absolute path the launched harness will observe as its cwd (execve
// preserves the working directory). That resolved form is the
// workspace-trust seed's exact projects key: macOS resolves /var to
// /private/var, and Claude Code records trust under the resolved path.
// Errors carry a fixed operation and errors.Is category only — the raw
// chain includes the directory itself, which is never rendered.
func launcherWorkerDir(d *deps) (string, error) {
	wd, err := d.getwd()
	if err != nil {
		return "", fmt.Errorf("the working directory could not be determined: %s (the directory is never echoed)", pathErrorCategory(err))
	}
	resolved, err := resolveCanonicalPath(wd)
	if err != nil {
		return "", fmt.Errorf("the working directory could not be canonically resolved: %s (the directory is never echoed)", pathErrorCategory(err))
	}
	return resolved, nil
}

// pathErrorCategory classifies a filesystem error into a fixed value-free
// category through errors.Is alone, the same way describeStoreOpenFailure
// classifies store-open failures: the raw chain commonly carries the
// complete path (os.PathError) and is never rendered.
func pathErrorCategory(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "a path element does not exist"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, syscall.ENOTDIR):
		return "a path element is not a directory"
	case errors.Is(err, syscall.ELOOP):
		return "too many levels of symbolic links"
	default:
		return "i/o failure"
	}
}

// resolveCanonicalPath resolves a path's symlinks to its canonical
// absolute form. It implements app.LaunchExecRequest.ResolvePath, the
// seam PrepareLaunchExec uses to compare the launcher's cwd with the
// recorded worktree path; the application never echoes its errors, which
// may carry the path.
func resolveCanonicalPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(resolved) {
		return "", fmt.Errorf("the path did not resolve to an absolute form")
	}
	return resolved, nil
}
