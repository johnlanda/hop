package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// defaultStopTimeout bounds one hop stop invocation. A deadline that
// leaves the run stopping is rerunnable: hop stop re-drives it.
const defaultStopTimeout = 30 * time.Second

// stopPollInterval paces the stop-driving and stop-observation rounds.
const stopPollInterval = 2 * time.Second

// runStop implements `hop stop <run-id>`: it records the monotonic stop
// request, then drives the stop directly when the lease is free (acquiring
// it with a new generation through Resume/ResumeFeature, which routes a
// stopping run to stop handling) or reports and observes when a live
// controller holds the lease. The run's mode (loaded once via Status
// before any lease acquisition) selects the driving pair: a feature-mode
// run always drives through ResumeFeature/DriveFeatureStop, a solo run
// through Resume/DriveStop — never a fallback between them, so a
// feature-mode run against a Controller missing the feature ports
// surfaces app.ErrFeatureModeUnsupported rather than silently running
// under the solo procedure. Exit 0 when stopped was reached, 1 when the
// deadline left it stopping (rerunnable) or on error, 2 on usage.
func runStop(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop stop", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	repoDir := flags.String("C", "", "repository directory (default: the working directory)")
	timeout := flags.Duration("timeout", defaultStopTimeout, "deadline for observing termination; hop stop re-drives an unfinished stop when rerun")
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() != 1 {
		_, err := fmt.Fprintln(stderr, "hop stop: exactly one run-id argument is required")
		return exitUsage, err
	}

	repoRoot, err := resolveRepositoryRoot(d, *repoDir)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop stop: %v\n", err)
		return exitUsage, werr
	}
	stateRoot, _, err := resolveStateRoot(d.getenv)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop stop: %v\n", err)
		return exitUsage, werr
	}
	hopPath, err := hopExecutablePath(d)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop stop: %v\n", err)
		return exitFailure, werr
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	ctrl, closeStore, err := d.openController(ctx, controllerConfig{stateRoot: stateRoot, socketPath: d.getenv("HERDR_SOCKET_PATH"), withRuntime: true})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop stop: %s\n", describeStoreOpenFailure(err, "the resolved state root"))
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // the store closes on process exit either way; commands report command errors, not pool teardown.

	runID, err := resolveRunArg(ctx, ctrl, repoRoot, flags.Arg(0))
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop stop: %v\n", err)
		return exitUsage, werr
	}
	status, err := ctrl.Status(ctx, app.StatusRequest{RunID: runID})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop stop: %v\n", err)
		return exitFailure, werr
	}
	if status.Detail == nil {
		_, werr := fmt.Fprintf(stderr, "hop stop: run %s has no status detail\n", runID)
		return exitFailure, werr
	}
	feature := isFeatureMode(status.Detail.Mode)

	if stopErr := ctrl.RequestStop(ctx, runID); stopErr != nil {
		_, werr := fmt.Fprintf(stderr, "hop stop: %v\n", stopErr)
		return exitFailure, werr
	}

	if feature {
		result, handle, featureErr := ctrl.ResumeFeature(ctx, app.ResumeFeatureRequest{
			RunID:        runID,
			ControllerID: d.newID(),
			HOPPath:      hopPath,
			StateRoot:    stateRoot,
		})
		return dispatchStopResume(ctx, d, ctrl, handle, featureErr, result.Outcome == "nothing-to-do", ctrl.DriveFeatureStop, runID, stdout, stderr)
	}
	result, handle, err := ctrl.Resume(ctx, app.ResumeRequest{
		RunID:        runID,
		ControllerID: d.newID(),
		HOPPath:      hopPath,
		StateRoot:    stateRoot,
	})
	return dispatchStopResume(ctx, d, ctrl, handle, err, result.Outcome == app.ResumeNothingToDo, ctrl.DriveStop, runID, stdout, stderr)
}

// dispatchStopResume is the shared tail of runStop's two mode branches:
// resumeErr and nothingToDo are the ResumeFeature/Resume call's own
// outcome, driveStop is the matching DriveFeatureStop/DriveStop method
// value — never crossed with the other mode's Resume/DriveStop pair.
func dispatchStopResume(ctx context.Context, d *deps, ctrl controllerAPI, handle app.RunHandle, resumeErr error, nothingToDo bool, driveStop func(context.Context, app.RunHandle) (app.StopReport, error), runID string, stdout, stderr io.Writer) (int, error) { //nolint:gocritic // hugeParam: RunHandle is the app-defined opaque token, passed by value as every Controller method takes it.
	switch {
	case resumeErr == nil:
		return driveStopRounds(ctx, d, ctrl, handle, nothingToDo, driveStop, stdout, stderr)
	case errors.Is(resumeErr, app.ErrLeaseHeld):
		// A live controller holds the lease; it observes the stop request
		// on its next round and drives the stop itself. Observe until
		// stopped or the deadline.
		if _, werr := fmt.Fprintln(stdout, "stopping (a live controller holds the lease and drives the stop)"); werr != nil {
			return exitFailure, werr
		}
		return observeStop(ctx, d, ctrl, runID, stdout, stderr)
	default:
		releaseQuietly(ctx, ctrl, handle)
		_, werr := fmt.Fprintf(stderr, "hop stop: %v\n", resumeErr)
		return exitFailure, werr
	}
}

// driveStopRounds drives driveStop (DriveStop or DriveFeatureStop, per the
// caller's mode) with the held lease until termination is observed or the
// deadline leaves the run stopping (rerunnable).
func driveStopRounds(ctx context.Context, d *deps, ctrl controllerAPI, handle app.RunHandle, nothingToDo bool, driveStop func(context.Context, app.RunHandle) (app.StopReport, error), stdout, stderr io.Writer) (int, error) { //nolint:gocritic // hugeParam: RunHandle is the app-defined opaque token, passed by value as every Controller method takes it.
	defer releaseQuietly(ctx, ctrl, handle)
	if nothingToDo {
		report, err := driveStop(ctx, handle)
		if err != nil {
			_, werr := fmt.Fprintf(stderr, "hop stop: %v\n", err)
			return exitFailure, werr
		}
		if _, werr := fmt.Fprintf(stdout, "%s\n", report.RunState); werr != nil {
			return exitFailure, werr
		}
		if report.RunState == runStateStopped {
			return exitOK, nil
		}
		return exitFailure, nil
	}
	heartbeatFailed := runHeartbeats(ctx, d, ctrl, handle)
	printed := false
	for {
		select {
		case err := <-heartbeatFailed:
			_, werr := fmt.Fprintf(stderr, "hop stop: heartbeat failed, the lease is lost: %v\n", err)
			return exitFailure, werr
		default:
		}
		report, err := driveStop(ctx, handle)
		if err != nil {
			_, werr := fmt.Fprintf(stderr, "hop stop: %v\n", err)
			return exitFailure, werr
		}
		if report.Terminated {
			if _, werr := fmt.Fprintln(stdout, "stopped"); werr != nil {
				return exitFailure, werr
			}
			return exitOK, nil
		}
		if !printed {
			if _, werr := fmt.Fprintln(stdout, "stopping"); werr != nil {
				return exitFailure, werr
			}
			printed = true
		}
		if waitErr := d.wait(ctx, stopPollInterval); waitErr != nil {
			_, werr := fmt.Fprintf(stdout, "still stopping: %s; rerun hop stop to keep driving it\n", outstandingSummary(report.Outstanding))
			return exitFailure, werr
		}
	}
}

// observeStop polls run status until stopped or the deadline, without a
// lease: the live controller does the driving.
func observeStop(ctx context.Context, d *deps, ctrl controllerAPI, runID string, stdout, stderr io.Writer) (int, error) {
	for {
		status, err := ctrl.Status(ctx, app.StatusRequest{RunID: runID})
		if err != nil {
			_, werr := fmt.Fprintf(stderr, "hop stop: %v\n", err)
			return exitFailure, werr
		}
		if status.Detail != nil && status.Detail.State == runStateStopped {
			if _, werr := fmt.Fprintln(stdout, "stopped"); werr != nil {
				return exitFailure, werr
			}
			return exitOK, nil
		}
		if err := d.wait(ctx, stopPollInterval); err != nil {
			_, werr := fmt.Fprintln(stdout, "still stopping; rerun hop stop to keep driving it")
			return exitFailure, werr
		}
	}
}

// outstandingSummary names the owned work a stop round is still waiting on.
func outstandingSummary(outstanding []string) string {
	if len(outstanding) == 0 {
		return "termination not yet observed"
	}
	summary := outstanding[0]
	for _, item := range outstanding[1:] {
		summary += "; " + item
	}
	return summary
}
