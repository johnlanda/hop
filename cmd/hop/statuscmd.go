package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// defaultStatusTimeout bounds one hop status invocation.
const defaultStatusTimeout = 10 * time.Second

// runStatus implements `hop status`: without -run one line per run of the
// repository (non-terminal runs by default; -all includes completed,
// failed and stopped), with -run the full detail block. Before rendering,
// both forms run the repository's worktree-retirement pass under its own
// bound — a first SIGINT/SIGTERM cancels the pass, a second exits — and its
// lines follow the rendered output. The pass never dials Herdr and never
// changes the exit code. State is data, not an exit code: rendering
// success exits 0.
func runStatus(args []string, stdout, stderr io.Writer, d *deps) (int, error) {
	diagnostics := &recordingWriter{w: stderr}
	flags := flag.NewFlagSet("hop status", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	repoDir := flags.String("C", "", "repository directory (default: the working directory)")
	runArg := flags.String("run", "", "render one run's full detail block (UUID or r<seq> label)")
	all := flags.Bool("all", false, "include completed, failed and stopped runs in the listing")
	if err := flags.Parse(args); err != nil {
		return exitUsage, diagnostics.err
	}
	if flags.NArg() > 0 {
		_, err := fmt.Fprintf(stderr, "hop status: unexpected argument %q\n", flags.Arg(0))
		return exitUsage, err
	}

	repoRoot, err := resolveRepositoryRoot(d, *repoDir)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop status: %v\n", err)
		return exitUsage, werr
	}
	stateRoot, _, err := resolveStateRoot(d.getenv)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop status: %v\n", err)
		return exitUsage, werr
	}

	openCtx, cancelOpen := context.WithTimeout(context.Background(), defaultStatusTimeout)
	defer cancelOpen()
	ctrl, closeStore, err := d.openController(openCtx, controllerConfig{stateRoot: stateRoot})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop status: %s\n", describeStoreOpenFailure(err, "the resolved state root"))
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // the store closes on process exit either way; commands report command errors, not pool teardown.

	passLines, err := statusRetirementPass(d, ctrl, repoRoot, stdout, stderr)
	if err != nil {
		return exitFailure, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultStatusTimeout)
	defer cancel()
	code, err := renderStatus(ctx, ctrl, repoRoot, *runArg, *all, stdout, stderr)
	if err != nil || code != exitOK {
		return code, err
	}
	if err := printLines(stdout, passLines); err != nil {
		return exitFailure, err
	}
	return exitOK, nil
}

// statusRetirementPass runs hop status's worktree-retirement pass under
// its own bound: the first SIGINT/SIGTERM cancels it (a running removal is
// killed and recovered by a later pass), a second exits immediately. A
// hop binary that cannot be located skips the pass with one line.
func statusRetirementPass(d *deps, ctrl controllerAPI, repoRoot string, stdout, stderr io.Writer) ([]string, error) {
	hopPath, err := hopExecutablePath(d)
	if err != nil {
		_, werr := fmt.Fprintln(stderr, "hop status: worktree retirement skipped: the hop executable could not be located")
		return nil, werr
	}
	ctx, cancel := context.WithTimeout(context.Background(), retirementPassTimeout)
	defer cancel()
	stopSignals := watchDetachSignals(d, cancel)
	defer stopSignals()
	return runWorktreeRetirement(ctx, d, ctrl, repoRoot, "", hopPath, stdout, stderr, "hop status")
}

// renderStatus renders the listing, or one run's detail block.
func renderStatus(ctx context.Context, ctrl controllerAPI, repoRoot, runArg string, all bool, stdout, stderr io.Writer) (int, error) {
	if runArg == "" {
		result, listErr := ctrl.Status(ctx, app.StatusRequest{RepositoryRoot: repoRoot})
		if listErr != nil {
			_, werr := fmt.Fprintf(stderr, "hop status: %v\n", listErr)
			return exitFailure, werr
		}
		return renderRunListing(stdout, result.Runs, all)
	}

	runID, err := resolveRunArg(ctx, ctrl, repoRoot, runArg)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop status: %v\n", err)
		return exitUsage, werr
	}
	result, err := ctrl.Status(ctx, app.StatusRequest{RunID: runID})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop status: %v\n", err)
		return exitFailure, werr
	}
	if result.Detail == nil {
		_, werr := fmt.Fprintf(stderr, "hop status: run %s has no detail\n", runID)
		return exitFailure, werr
	}
	return renderRunDetail(stdout, result.Detail)
}

// renderRunListing prints one deterministic line per run.
func renderRunListing(w io.Writer, runs []app.RunSummaryView, includeTerminal bool) (int, error) {
	shown := 0
	for _, r := range runs {
		if !includeTerminal && isTerminalRunState(r.State) {
			continue
		}
		if _, err := fmt.Fprintf(w, "%s %s %s%s\n", seqLabel(r.Sequence), r.RunID, r.State, listingMarkers(&r)); err != nil {
			return exitFailure, err
		}
		shown++
	}
	if shown == 0 {
		suffix := " (use -all to include finished runs)"
		if includeTerminal {
			suffix = ""
		}
		if _, err := fmt.Fprintf(w, "no runs%s\n", suffix); err != nil {
			return exitFailure, err
		}
	}
	return exitOK, nil
}

// listingMarkers renders a listing line's condition markers.
func listingMarkers(r *app.RunSummaryView) string {
	var markers []string
	if r.StopRequested && !isTerminalRunState(r.State) {
		markers = append(markers, "stop requested")
	}
	if r.Reconciling {
		markers = append(markers, "reconciling")
	}
	if len(markers) == 0 {
		return ""
	}
	return " (" + strings.Join(markers, ", ") + ")"
}

// renderRunDetail prints the full detail block: states, a feature run's
// retirement target and fact, the worktree (one line per row for a feature
// run), binding, claim, pending operations (each unresolved feature
// worktree creation with its human action), last submission, artifacts and
// the last check
// execution — including the human's options for an unrepeatable unknown
// outcome.
func renderRunDetail(w io.Writer, detail *app.RunDetailView) (int, error) {
	lines := []string{
		fmt.Sprintf("run %s %s", seqLabel(detail.Sequence), detail.RunID),
		"  state:         " + detail.State + listingMarkers(&detail.RunSummaryView),
		"  workflow:      " + workflowLabel(detail.Mode),
	}
	if isFeatureMode(detail.Mode) {
		lines = append(lines,
			"  target:        "+targetBranchLabel(detail.TargetBranch),
			"  worktrees:     "+worktreeRetirementLabel(detail.TargetBranch, detail.WorktreesRetiredAt))
	}
	lines = append(lines,
		"  task:          "+orUnset(detail.TaskState),
		"  attempt:       "+orUnset(detail.AttemptState))
	if isFeatureMode(detail.Mode) {
		lines = append(lines, worktreeDetailLines(detail.Worktrees)...)
	} else {
		lines = append(lines, "  worktree:      "+orUnset(detail.WorktreePath))
	}
	lines = append(lines,
		"  binding:       "+orUnset(detail.BindingSummary),
		"  launch claim:  "+orUnset(detail.ClaimState),
		"  trust seed:    "+orUnset(detail.SeedEvidence),
		fmt.Sprintf("  pending ops:   %d", detail.PendingOps),
		"  last submit:   "+orUnset(detail.LastSubmission),
	)
	for _, op := range detail.WorktreeOperations {
		lines = append(lines, "  worktree op:   "+op.OperationID+" "+orUnset(op.Branch)+" ("+op.State+")",
			"    action:      "+op.Action)
	}
	for _, artifact := range detail.Artifacts {
		lines = append(lines, "  artifact:      "+artifact)
	}
	if detail.LastCheckOperation != "" {
		lines = append(lines,
			"  last check:    "+detail.LastCheckOperation+" ("+detail.LastCheckState+")",
			"    detail:      "+orUnset(detail.LastCheckDetail))
		for _, path := range detail.LastCheckEvidence {
			lines = append(lines, "    evidence:    "+path)
		}
		if detail.LastCheckUnknown {
			lines = append(lines, "    unknown outcome — options: "+detail.LastCheckOptions)
		}
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return exitFailure, err
		}
	}
	return exitOK, nil
}

// targetBranchLabel renders a feature run's frozen worktree-retirement
// target (docs/plan/phase-3-worktree-retirement.md section 6).
func targetBranchLabel(target string) string {
	if target == "" {
		return "none (detached HEAD at freeze; worktrees are never retired automatically)"
	}
	return target
}

// worktreeRetirementLabel renders a feature run's worktrees-retired fact,
// and before it is set, when and how retirement will happen — including
// the plain warning that removal deletes ignored files.
func worktreeRetirementLabel(target string, retiredAt *time.Time) string {
	switch {
	case retiredAt != nil:
		return "retired " + retiredAt.UTC().Format(time.RFC3339)
	case target == "":
		return "kept (no target branch)"
	default:
		return "not retired (removed once the integration branch is merged into " + target +
			"; removal deletes ignored files such as build output; commit anything you want to keep)"
	}
}

// isFeatureMode reports whether mode (RunDetail.Mode/RunDetailView.Mode)
// names a feature-mode run: "" means solo (app.WorkflowSnapshot's own
// zero-value convention), matched against app.WorkflowModeFeature rather
// than a repeated literal.
func isFeatureMode(mode string) bool { return mode == app.WorkflowModeFeature }

// workflowLabel renders a run's workflow mode for human display: "solo"
// for the zero value, "feature" otherwise.
func workflowLabel(mode string) string {
	if isFeatureMode(mode) {
		return app.WorkflowModeFeature
	}
	return app.WorkflowModeSolo
}

// orUnset renders an explicit marker for a value the store has not
// populated yet, so output stays deterministic and comparable.
func orUnset(value string) string {
	if value == "" {
		return "(none)"
	}
	return value
}
