package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"

	"github.com/johnlanda/hop/internal/app"
)

// confirmAbsentValue implements flag.Value plus the standard library's
// IsBoolFlag() extension so `--confirm-absent` parses as a bare boolean
// (Phase 2's exact solo grammar, `-confirm-absent` alone) while still
// accepting an explicit value via `-confirm-absent=<value>` — the only
// way `--confirm-absent=<session-id>` (feature mode's required form,
// design section 10) can share one flag registration with the solo
// boolean without breaking either surface. set reports whether the flag
// was given at all; value is the raw string Set received ("true" for the
// bare form, since the flag package calls Set("true") when a bool-shaped
// flag appears with no "=value").
type confirmAbsentValue struct {
	set   bool
	value string
}

func (v *confirmAbsentValue) String() string { return v.value }

func (v *confirmAbsentValue) Set(s string) error {
	v.set = true
	v.value = s
	return nil
}

func (*confirmAbsentValue) IsBoolFlag() bool { return true }

// runResume implements `hop resume <run-id>`: after the repository's
// worktree-retirement pass (excluding this run), it loads the run's frozen
// mode via Status BEFORE acquiring any lease (the two forms of
// --confirm-absent can only be validated once mode is known), then
// acquires the lease (a new fencing generation) and reconciles through
// exactly one pair — ResumeFeature for a feature-mode run, Resume for
// solo — never a fallback between them. Continuable outcomes stay in the
// matching foreground controller loop; a fail-closed or unsupported
// report releases the lease and exits 1 so the human can act and rerun
// hop resume; a pending stop routes to stop driving. Exit codes as hop
// run.
//
// --confirm-absent: a solo run takes the bare Phase 2 boolean form
// unchanged (`--confirm-absent`, or an explicit `--confirm-absent=true|false`
// once parsed as a bool; any other value is a usage error). A feature-mode
// run requires an explicit session id (`--confirm-absent=<session-id>`);
// omitting the flag entirely is valid (no attestation attempted), but a
// bare or empty flag is a usage error naming the required form. A value
// that matches no currently-absent session is not a usage error: the
// per-session disposition rendered below names exactly the id that
// session still requires.
func runResume(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop resume", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	repoDir := flags.String("C", "", "repository directory (default: the working directory)")
	socketPath := flags.String("socket", "", "herdr server socket (default $HERDR_SOCKET_PATH)")
	var confirmAbsent confirmAbsentValue
	flags.Var(&confirmAbsent, "confirm-absent", "solo: bare boolean attestation (Phase 2, unchanged); feature mode: --confirm-absent=<session-id>, the per-session absence attestation")
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
		_, werr := fmt.Fprintf(stderr, "hop resume: %s\n", describeStoreOpenFailure(err, "the resolved state root"))
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // the store closes on process exit either way; commands report command errors, not pool teardown.

	runID, err := resolveRunArg(ctx, ctrl, repoRoot, flags.Arg(0))
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop resume: %v\n", err)
		return exitUsage, werr
	}
	// The repository's worktree-retirement pass runs before this
	// controller takes its own run's lease, excluding that run.
	passLines, err := runWorktreeRetirement(ctx, d, ctrl, repoRoot, runID, hopPath, stdout, stderr, "hop resume")
	if err != nil {
		return exitFailure, err
	}
	if printErr := printLines(stdout, passLines); printErr != nil {
		return exitFailure, printErr
	}
	status, err := ctrl.Status(ctx, app.StatusRequest{RunID: runID})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop resume: %v\n", err)
		return exitFailure, werr
	}
	if status.Detail == nil {
		_, werr := fmt.Fprintf(stderr, "hop resume: run %s has no status detail\n", runID)
		return exitFailure, werr
	}

	if isFeatureMode(status.Detail.Mode) {
		sessionID := ""
		if confirmAbsent.set {
			if confirmAbsent.value == "" || confirmAbsent.value == "true" {
				_, werr := fmt.Fprintln(stderr, "hop resume: --confirm-absent requires a session id in feature mode: --confirm-absent=<session-id>")
				return exitUsage, werr
			}
			sessionID = confirmAbsent.value
		}
		return runResumeFeature(ctx, d, ctrl, runID, sessionID, hopPath, stateRoot, stdout, stderr)
	}

	confirmBool := false
	if confirmAbsent.set {
		confirmBool, err = strconv.ParseBool(confirmAbsent.value)
		if err != nil {
			_, werr := fmt.Fprintf(stderr, "hop resume: --confirm-absent takes no value on a solo run (got %q)\n", confirmAbsent.value)
			return exitUsage, werr
		}
	}

	result, handle, err := ctrl.Resume(ctx, app.ResumeRequest{
		RunID:         runID,
		ControllerID:  d.newID(),
		ConfirmAbsent: confirmBool,
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

// runResumeFeature is runResume's feature-mode branch: ResumeFeature,
// rendering every session's disposition (a non-matching --confirm-absent
// value is runtime state, not a usage error — the printed line names
// exactly the id that session still requires), then dispatching to the
// feature-mode loop or a status-based exit exactly like the solo branch.
// The caller's directory only scopes an r<seq> lookup: the loop assigns in
// the run's frozen repository, whichever directory hop resume ran in.
func runResumeFeature(ctx context.Context, d *deps, ctrl controllerAPI, runID, confirmAbsentSession, hopPath, stateRoot string, stdout, stderr io.Writer) (int, error) {
	result, handle, err := ctrl.ResumeFeature(ctx, app.ResumeFeatureRequest{
		RunID:                runID,
		ControllerID:         d.newID(),
		ConfirmAbsentSession: confirmAbsentSession,
		HOPPath:              hopPath,
		StateRoot:            stateRoot,
	})
	if err != nil {
		releaseQuietly(ctx, ctrl, handle)
		_, werr := fmt.Fprintf(stderr, "hop resume: %v\n", err)
		return exitFailure, werr
	}
	if _, werr := fmt.Fprintf(stdout, "resume %s: %s\n", result.Outcome, result.RunState); werr != nil {
		return exitFailure, werr
	}
	for _, line := range resumeFeatureSessionLines(&result) {
		if _, werr := fmt.Fprintln(stdout, line); werr != nil {
			return exitFailure, werr
		}
	}

	label, err := runLabel(ctx, ctrl, runID)
	if err != nil {
		releaseQuietly(ctx, ctrl, handle)
		_, werr := fmt.Fprintf(stderr, "hop resume: %v\n", err)
		return exitFailure, werr
	}

	switch result.Outcome {
	case "resumed", "stop-pending":
		return finishFeatureControllerLoop(ctx, d, ctrl, handle, runID, label, hopPath, stdout, stderr, "hop resume")
	case "nothing-to-do":
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
		// "reconciling": the per-session lines above already name what is
		// outstanding and, for a session still needing attestation, the
		// exact id --confirm-absent must carry.
		releaseQuietly(ctx, ctrl, handle)
		return exitFailure, nil
	}
}

// resumeFeatureSessionLines renders one line per session's disposition
// and one per blocked integration operation, in that order.
func resumeFeatureSessionLines(result *app.ResumeFeatureResult) []string {
	lines := make([]string, 0, len(result.Sessions)+len(result.Blocked))
	for _, s := range result.Sessions {
		line := fmt.Sprintf("  session %s (%s): %s", s.SessionID, s.Role, s.Disposition)
		if s.Detail != "" {
			line += " - " + s.Detail
		}
		lines = append(lines, line)
	}
	for _, b := range result.Blocked {
		lines = append(lines, "  blocked: "+b)
	}
	return lines
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
