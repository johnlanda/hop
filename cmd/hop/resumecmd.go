package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/johnlanda/hop/internal/app"
)

// runResume implements `hop resume <run-id>`: it acquires the lease (a new
// fencing generation), reconciles per the design's section 5 and prints
// what it established. Continuable outcomes (warm reattach, cold relaunch,
// continued startup, one more reconciliation pass) stay in the foreground
// controller loop; a fail-closed or unsupported report releases the lease
// and exits 1 so the human can act and rerun hop resume; a pending stop
// routes to stop driving. --confirm-absent records the human's absence
// attestation (the design enables cold relaunch only in the non-restart,
// server-continuity-established case). Exit codes as hop run.
func runResume(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop resume", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	repoDir := flags.String("C", "", "repository directory (default: the working directory)")
	socketPath := flags.String("socket", "", "herdr server socket (default $HERDR_SOCKET_PATH)")
	confirmAbsent := flags.Bool("confirm-absent", false, "record a human attestation that no worker for this run is running anywhere and every mechanism that could still start one has been retired")
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() != 1 {
		_, err := fmt.Fprintln(stderr, "hop resume: exactly one run-id argument is required")
		return exitUsage, err
	}
	if *socketPath == "" {
		*socketPath = d.getenv("HERDR_SOCKET_PATH")
	}

	repoRoot, err := resolveRepositoryRoot(d, *repoDir)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop resume: %v\n", err)
		return exitUsage, werr
	}
	stateRoot, _, err := resolveStateRoot(d.getenv)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop resume: %v\n", err)
		return exitUsage, werr
	}
	hopPath, err := hopExecutablePath(d)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop resume: %v\n", err)
		return exitFailure, werr
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopSignals := watchDetachSignals(d, cancel)
	defer stopSignals()

	ctrl, closeStore, err := d.openController(ctx, controllerConfig{stateRoot: stateRoot, socketPath: *socketPath, withRuntime: true})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop resume: %v\n", err)
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // the store closes on process exit either way; commands report command errors, not pool teardown.

	runID, err := resolveRunArg(ctx, ctrl, repoRoot, flags.Arg(0))
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop resume: %v\n", err)
		return exitUsage, werr
	}

	result, handle, err := ctrl.Resume(ctx, app.ResumeRequest{
		RunID:         runID,
		ControllerID:  d.newID(),
		ConfirmAbsent: *confirmAbsent,
		HOPPath:       hopPath,
		StateRoot:     stateRoot,
	})
	if err != nil {
		releaseQuietly(ctx, ctrl, handle)
		_, werr := fmt.Fprintf(stderr, "hop resume: %v\n", err)
		return exitFailure, werr
	}
	if _, werr := fmt.Fprintf(stdout, "resume %s: %s\n", result.Outcome, resumeDetailLine(&result)); werr != nil {
		return exitFailure, werr
	}

	label, err := runLabel(ctx, ctrl, runID)
	if err != nil {
		releaseQuietly(ctx, ctrl, handle)
		_, werr := fmt.Fprintf(stderr, "hop resume: %v\n", err)
		return exitFailure, werr
	}

	switch result.Outcome {
	case app.ResumeWarmReattached, app.ResumeColdRelaunched, app.ResumeStartupContinued, app.ResumeStopPending:
		return finishControllerLoop(ctx, d, ctrl, handle, runID, label, hopPath, stdout, stderr, "hop resume")
	case app.ResumeNothingToDo:
		status, statusErr := ctrl.Status(ctx, app.StatusRequest{RunID: runID})
		releaseQuietly(ctx, ctrl, handle)
		if statusErr != nil {
			_, werr := fmt.Fprintf(stderr, "hop resume: %v\n", statusErr)
			return exitFailure, werr
		}
		if status.Detail != nil {
			return exitForRunState(status.Detail.State), nil
		}
		return exitFailure, nil
	default:
		// Failed closed, still reconciling, or unsupported: the report
		// names what was observed and the exact human action. The lease is
		// released so the human can act and rerun hop resume (or attest
		// absence, or hop stop the run); further reconciliation rounds run
		// through a fresh hop resume, never a blind wait.
		releaseQuietly(ctx, ctrl, handle)
		return exitFailure, nil
	}
}

// resumeDetailLine renders a ResumeResult's report, naming the observed
// pane on a fail-closed outcome.
func resumeDetailLine(result *app.ResumeResult) string {
	if result.ObservedPaneID != "" {
		return fmt.Sprintf("%s (pane %s)", result.Detail, result.ObservedPaneID)
	}
	return result.Detail
}

// runLabel loads the run's repository-scoped r<seq> label for the loop's
// transition lines.
func runLabel(ctx context.Context, ctrl controllerAPI, runID string) (string, error) {
	status, err := ctrl.Status(ctx, app.StatusRequest{RunID: runID})
	if err != nil {
		return "", fmt.Errorf("load run status: %w", err)
	}
	if status.Detail == nil {
		return "", fmt.Errorf("run %s has no status detail", runID)
	}
	return seqLabel(status.Detail.Sequence), nil
}
