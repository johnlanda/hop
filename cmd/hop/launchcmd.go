package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// defaultLaunchPrepTimeout bounds hop launch's pre-exec store work. The
// exec itself replaces the process and is not bounded by any context.
const defaultLaunchPrepTimeout = 30 * time.Second

// runLaunch implements `hop launch --run <uuid> --attempt <uuid>`, the
// worker exec boundary (design section 6): it requires the launch-provided
// absolute HOP_STATE_DIR (never falling back to the default resolution),
// prepares the exec through the application — environment validation
// against the launch context, sanitization under the frozen policy,
// per-harness argv composition, executable resolution, the exec_pending
// claim — and execs through the process adapter. On success it never
// returns. Every failure exits 1 with one stderr line that never echoes an
// environment value; a failure after the claim (the exec itself) settles
// the claim exec_failed first.
func runLaunch(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	_ = stdout // hop launch's success is an exec; it writes only diagnostics.
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop launch", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	runID := flags.String("run", "", "run id (required)")
	attemptID := flags.String("attempt", "", "attempt id (required)")
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() > 0 {
		_, err := fmt.Fprintf(stderr, "hop launch: unexpected argument %q\n", flags.Arg(0))
		return exitUsage, err
	}
	if *runID == "" || *attemptID == "" {
		_, err := fmt.Fprintln(stderr, "hop launch: --run and --attempt are required")
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

	ctx, cancel := context.WithTimeout(context.Background(), defaultLaunchPrepTimeout)
	defer cancel()
	ctrl, closeStore, err := d.openController(ctx, controllerConfig{stateRoot: stateRoot})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop launch: %s\n", describeStoreOpenFailure(err, "HOP_STATE_DIR"))
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // an exec success never reaches this; on failure the store closes on process exit either way.

	plan, err := ctrl.PrepareLaunchExec(ctx, app.LaunchExecRequest{
		RunID:            *runID,
		AttemptID:        *attemptID,
		HOPPath:          hopPath,
		Environ:          d.environ(),
		PID:              d.getpid(),
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
