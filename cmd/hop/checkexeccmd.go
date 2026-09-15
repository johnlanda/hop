package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// defaultCheckExecPrepTimeout bounds hop check-exec's pre-exec store work.
// The check command itself replaces the process; the controller bounds it
// with the frozen check timeout by canceling the whole process group.
const defaultCheckExecPrepTimeout = 30 * time.Second

// runCheckExec implements `hop check-exec --op <uuid> -- <check argv...>`,
// the check exec boundary the controller spawns as a process-group leader
// (design section 7): it requires the controller-provided absolute
// HOP_STATE_DIR, verifies it leads its own process group, durably records
// its pid as the check-exec claim BEFORE exec — refusing to run when that
// write fails or the operation is not a current pending check — then execs
// the frozen check argv verbatim (its running argv is what group
// retirement matches) with the sanitized environment through the process
// adapter. Exit status propagates through the exec: the check's exit
// status IS this process's. Every pre-exec failure exits 1 with one stderr
// line.
func runCheckExec(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	_ = stdout // hop check-exec's success is an exec; it writes only diagnostics.
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop check-exec", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	opID := flags.String("op", "", "check execution operation id (required)")
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if *opID == "" {
		_, err := fmt.Fprintln(stderr, "hop check-exec: --op is required")
		return exitUsage, err
	}
	if flags.NArg() == 0 {
		_, err := fmt.Fprintln(stderr, "hop check-exec: the check argv is required after --")
		return exitUsage, err
	}

	stateRoot, err := requireWorkerStateRoot(d.getenv)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop check-exec: %v\n", err)
		return exitFailure, werr
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultCheckExecPrepTimeout)
	defer cancel()
	ctrl, closeStore, err := d.openController(ctx, controllerConfig{stateRoot: stateRoot})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop check-exec: %v\n", err)
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // an exec success never reaches this; on failure the store closes on process exit either way.

	plan, err := ctrl.PrepareCheckExec(ctx, app.CheckExecRequest{
		OperationID:       *opID,
		CheckArgv:         flags.Args(),
		Environ:           d.environ(),
		PID:               d.getpid(),
		LeadsProcessGroup: d.leadsGroup(),
		LookupExecutable:  lookupExecutable,
	})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop check-exec: %v\n", err)
		return exitFailure, werr
	}

	execErr := d.execResolved(plan.ExecPath, plan.Argv, plan.Env)
	_, werr := fmt.Fprintf(stderr, "hop check-exec: %v\n", execErr)
	return exitFailure, werr
}
