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
// failed and stopped), with -run the full detail block. State is data, not
// an exit code: rendering success exits 0.
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

	ctx, cancel := context.WithTimeout(context.Background(), defaultStatusTimeout)
	defer cancel()
	ctrl, closeStore, err := d.openController(ctx, controllerConfig{stateRoot: stateRoot})
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "hop status: %s\n", describeStoreOpenFailure(err, "the resolved state root"))
		return exitFailure, werr
	}
	defer closeStore() //nolint:errcheck // the store closes on process exit either way; commands report command errors, not pool teardown.

	if *runArg == "" {
		result, listErr := ctrl.Status(ctx, app.StatusRequest{RepositoryRoot: repoRoot})
		if listErr != nil {
			_, werr := fmt.Fprintf(stderr, "hop status: %v\n", listErr)
			return exitFailure, werr
		}
		return renderRunListing(stdout, result.Runs, *all)
	}

	runID, err := resolveRunArg(ctx, ctrl, repoRoot, *runArg)
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
	if r.NeedsAttention {
		markers = append(markers, app.GrammarAttentionMarker)
	}
	if len(markers) == 0 {
		return ""
	}
	return " (" + strings.Join(markers, ", ") + ")"
}

// renderRunDetail prints the full detail block: states, worktree, binding,
// claim, pending operations (each unresolved feature worktree operation
// with its human action), last submission, artifacts and the last check
// execution — including the human's options for an unrepeatable unknown
// outcome.
func renderRunDetail(w io.Writer, detail *app.RunDetailView) (int, error) {
	lines := []string{
		fmt.Sprintf("run %s %s", seqLabel(detail.Sequence), detail.RunID),
		"  state:         " + detail.State + listingMarkers(&detail.RunSummaryView),
		"  workflow:      " + workflowLabel(detail.Mode),
		"  task:          " + orUnset(detail.TaskState),
		"  attempt:       " + orUnset(detail.AttemptState),
		"  worktree:      " + orUnset(detail.WorktreePath),
		"  binding:       " + orUnset(detail.BindingSummary),
		"  launch claim:  " + orUnset(detail.ClaimState),
		"  trust seed:    " + orUnset(detail.SeedEvidence),
		fmt.Sprintf("  pending ops:   %d", detail.PendingOps),
		"  last submit:   " + orUnset(detail.LastSubmission),
	}
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
	lines = append(lines, featureDetailLines(detail)...)
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return exitFailure, err
		}
	}
	return exitOK, nil
}

// featureDetailLines renders the section 10 feature-mode detail block:
// the task table, the latest integration, EvaluateReadiness's guard
// shortfalls verbatim, the section 7 per-mailbox attention lines, the
// pending human questions and the per-session roles/bindings listing. A
// solo run (detail.Mode == "") renders none of this, so solo output stays
// byte-identical to Phase 2's. Every fixed line's text comes from
// internal/app/grammar.go; this function only resolves task-uuid-to-label
// lookups and assembles the block's indentation.
func featureDetailLines(detail *app.RunDetailView) []string {
	if !isFeatureMode(detail.Mode) {
		return nil
	}
	labels := taskLabelsByID(detail.Tasks)
	var lines []string

	for _, t := range detail.Tasks {
		lines = append(lines, "  "+app.GrammarTaskLine(
			app.GrammarTaskLabel(t.Seq), t.TaskID, t.Kind, t.State, depLabels(t.DependsOn, labels),
			t.AttemptCount, orUnset(t.WorktreePath),
		))
	}

	if integ := detail.LatestIntegration; integ != nil {
		lines = append(lines, "  "+app.GrammarIntegrationLine(
			integ.ID, taskLabelFor(integ.TaskID, labels), integ.State,
			orUnset(integ.SourceCommitOID), orUnset(integ.PremergeHeadOID), orUnset(integ.MergeCommitOID),
		))
	}

	for _, s := range detail.GuardShortfalls {
		taskLabel := ""
		if s.TaskID != "" {
			taskLabel = taskLabelFor(s.TaskID, labels)
		}
		lines = append(lines, "  "+app.GrammarShortfallLine(s.Kind, taskLabel, s.TaskID))
	}

	for _, m := range detail.Mailboxes {
		address := m.Address
		if strings.HasPrefix(address, "task:") {
			taskID := strings.TrimPrefix(address, "task:")
			address = app.GrammarTaskAddress(taskID, taskLabelFor(taskID, labels))
		}
		lines = append(lines, "  "+app.GrammarAttentionLine(address, m.InFlightMessageID, m.InFlightAge, m.QueuedCount, m.OldestQueuedAge))
		if m.Attention {
			action := app.GrammarAttentionActionHuman
			if m.Address != "human" {
				action = app.GrammarAttentionActionSession(sessionBindingFor(m.Address, detail.Sessions))
			}
			lines = append(lines, "    action:      "+action)
		}
	}

	for _, q := range detail.PendingQuestions {
		lines = append(lines,
			"  "+app.GrammarQuestionLine(q.MessageID, q.Age, q.BodyPath),
			"    "+app.GrammarAnswerInvocationLine(q.MessageID),
		)
	}

	for _, s := range detail.Sessions {
		taskLabel := "(none)"
		if s.TaskID != "" {
			taskLabel = taskLabelFor(s.TaskID, labels)
		}
		lines = append(lines, "  "+app.GrammarSessionLine(s.SessionID, s.Role, s.State, taskLabel, s.AttemptNumber, orUnset(s.BindingSummary)))
	}

	return lines
}

// taskLabelsByID maps a feature run's task uuids to their stable t<seq>
// display sequence, for resolving a uuid reference (a dependency edge, a
// guard shortfall's task, an integration's task, a mailbox address) to its
// label without a second store round trip.
func taskLabelsByID(tasks []app.TaskSummaryView) map[string]int {
	labels := make(map[string]int, len(tasks))
	for _, t := range tasks {
		labels[t.TaskID] = t.Seq
	}
	return labels
}

// taskLabelFor resolves one task uuid to its "t<seq>" label.
func taskLabelFor(taskID string, labels map[string]int) string {
	return app.GrammarTaskLabel(labels[taskID])
}

// depLabels renders a task's dependency edges as comma-separated t<seq>
// labels, or "(none)" when it depends on nothing.
func depLabels(dependsOn []string, labels map[string]int) string {
	if len(dependsOn) == 0 {
		return "(none)"
	}
	rendered := make([]string, len(dependsOn))
	for i, id := range dependsOn {
		rendered[i] = taskLabelFor(id, labels)
	}
	return strings.Join(rendered, ",")
}

// sessionBindingFor resolves a mailbox address ("manager" or
// "task:<uuid>") to its live session's current binding summary, the most
// recently created matching session in sessions (oldest first) — the
// manager's own succession, or a task's latest attempt — or "" when that
// session currently has none. The human address is never passed here: it
// has no session to bind.
func sessionBindingFor(address string, sessions []app.SessionView) string {
	taskID, isTask := strings.CutPrefix(address, "task:")
	var binding string
	for _, s := range sessions {
		switch {
		case isTask && s.TaskID == taskID:
			binding = s.BindingSummary
		case !isTask && address == "manager" && s.Role == "manager":
			binding = s.BindingSummary
		}
	}
	return binding
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
