package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// defaultSubmitTimeout bounds one hop result submit invocation.
const defaultSubmitTimeout = 30 * time.Second

// runResult dispatches the `hop result` subcommands; submit is the only
// one.
func runResult(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	if len(args) == 0 || args[0] != "submit" {
		_, err := fmt.Fprintln(stderr, "hop result: usage: hop result submit --summary \"<text>\" --commit <oid> [--run ID --task ID --attempt ID]")
		return exitUsage, err
	}
	return runResultSubmit(args[1:], stdout, stderr, d)
}

// runResultSubmit implements `hop result submit` (design section 7): ID
// flags default from the launch-provided HOP_RUN_ID, HOP_TASK_ID and
// HOP_ATTEMPT_ID; the incarnation is read from HOP_INCARNATION_ID (no
// flag); the absolute HOP_STATE_DIR is required as in every worker
// context. Parse-don't-validate: the flag values — empty ones included —
// are handed to the application verbatim, whose section 7 step 1 records
// an invalid submission as `malformed` through the protocol; the command
// never pre-judges a value. The submission outcome is printed as the
// first output line —
// verbatim for `transient`, whose first line is the worker's retry
// signal. Exit 0 for accepted and duplicate; 1 for transient, stale,
// conflicting and malformed; 2 on usage.
func runResultSubmit(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop result submit", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	summary := flags.String("summary", "", "one-line result summary (required; the value itself is judged by the submission protocol)")
	commit := flags.String("commit", "", "submitted commit object id (required; the value itself is judged by the submission protocol)")
	runID := flags.String("run", "", "run id (default $HOP_RUN_ID)")
	taskID := flags.String("task", "", "task id (default $HOP_TASK_ID)")
	attemptID := flags.String("attempt", "", "attempt id (default $HOP_ATTEMPT_ID)")
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() > 0 {
		_, err := fmt.Fprintf(stderr, "hop result submit: unexpected argument %q\n", flags.Arg(0))
		return exitUsage, err
	}
	// Flag PRESENCE decides usage; flag VALUES are the protocol's to judge.
	// An omitted required flag is a usage error, and an omitted id flag
	// falls back to its launch-provided default — but a flag the worker
	// explicitly supplied is forwarded verbatim, empty included, so the
	// application records an invalid value as malformed with a receipt
	// instead of the CLI discarding it.
	supplied := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { supplied[f.Name] = true })
	if !supplied["summary"] || !supplied["commit"] {
		_, err := fmt.Fprintln(stderr, "hop result submit: --summary and --commit are required")
		return exitUsage, err
	}
	if !supplied["run"] {
		*runID = d.getenv("HOP_RUN_ID")
	}
	if !supplied["task"] {
		*taskID = d.getenv("HOP_TASK_ID")
	}
	if !supplied["attempt"] {
		*attemptID = d.getenv("HOP_ATTEMPT_ID")
	}

	stateRoot, err := requireWorkerStateRoot(d.getenv)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop result submit: %v\n", err)
		return exitFailure, werr
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultSubmitTimeout)
	defer cancel()
	ctrl, closeStore, err := d.openController(ctx, controllerConfig{stateRoot: stateRoot})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop result submit: %s\n", describeStoreOpenFailure(err, "HOP_STATE_DIR"))
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // the store closes on process exit either way; commands report command errors, not pool teardown.

	result, err := ctrl.SubmitResult(ctx, app.SubmitResultRequest{
		RunID:         *runID,
		TaskID:        *taskID,
		AttemptID:     *attemptID,
		IncarnationID: d.getenv("HOP_INCARNATION_ID"),
		CommitOID:     *commit,
		Summary:       *summary,
	})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop result submit: %v\n", err)
		return exitFailure, werr
	}
	if _, err := fmt.Fprintln(stdout, submissionLine(&result)); err != nil {
		return exitFailure, err
	}
	switch result.Kind {
	case string(app.SubmissionAccepted), string(app.SubmissionDuplicate):
		return exitOK, nil
	default:
		return exitFailure, nil
	}
}

// submissionLine renders one submission outcome as the command's first
// output line. A transient outcome's line is the store's Detail verbatim
// (`transient: attempt not yet running; retry` — the worker's parseable
// retry signal); accepted and duplicate name the result id.
func submissionLine(result *app.SubmitResultResult) string {
	switch result.Kind {
	case string(app.SubmissionTransient):
		return result.Detail
	case string(app.SubmissionAccepted), string(app.SubmissionDuplicate):
		return result.Kind + " " + result.ResultID
	default:
		if result.Detail != "" {
			return result.Kind + ": " + result.Detail
		}
		return result.Kind
	}
}
