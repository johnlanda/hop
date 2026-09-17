package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/johnlanda/hop/internal/app"
)

// defaultStatusTimeout bounds one hop status invocation.
const defaultStatusTimeout = 10 * time.Second

// safeRenderExternal renders a path or an opaque Herdr-assigned
// identifier for a status line: raw when it is valid UTF-8 with no
// control character (C0, DEL, C1), no double quote and no backslash —
// the shapes that could otherwise forge a line boundary (a newline
// inserting a fake protocol line), emit a terminal control sequence, or
// make the two rendering forms ambiguous. Otherwise it renders as Go's
// quoted-string form (strconv.Quote), which escapes exactly those bytes
// and always starts with a double quote — so a raw rendering never
// starts with one, and a reader can always tell which form a field
// took. Ordinary paths and identifiers are untouched, so every existing
// render table stays byte-identical. Applied to every path and every
// Herdr binding identifier hop status prints: these are operator- or
// principal-selected strings (a checkout location, a workspace/tab/pane
// id), never HOP-generated, and reach rendering unvalidated by the
// stores that accept and pass them through (design's "paths appear only
// where the design names them" invariant is about WHICH fields carry a
// path, not about what bytes those fields may contain).
func safeRenderExternal(s string) string {
	if isSafeExternalString(s) {
		return s
	}
	return strconv.Quote(s)
}

// isSafeExternalString reports whether s can render raw per
// safeRenderExternal's contract.
func isSafeExternalString(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			return false
		case r < 0x20 || r == 0x7f: // C0 controls and DEL
			return false
		case r >= 0x80 && r <= 0x9f: // C1 controls
			return false
		}
	}
	return true
}

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
	if r.NeedsAttention {
		markers = append(markers, app.GrammarAttentionMarker)
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
		lines = append(lines, "  worktree op:   "+op.OperationID+" "+safeRenderExternal(orUnset(op.Branch))+" ("+op.State+")",
			"    "+app.GrammarActionPrefix+"      "+op.Action)
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
			t.AttemptCount, safeRenderExternal(orUnset(t.WorktreePath)),
		))
	}

	if integ := detail.LatestIntegration; integ != nil {
		lines = append(lines, "  "+app.GrammarIntegrationLine(
			integ.ID, taskLabelFor(integ.TaskID, labels), integ.State,
			orUnset(integ.SourceCommitOID), orUnset(integ.PremergeHeadOID), orUnset(integ.MergeCommitOID),
		))
	}

	for _, s := range detail.GuardShortfalls {
		if s.Kind == app.GrammarShortfallVerdictRejected {
			lines = append(lines, "  "+app.GrammarVerdictRejectedLine(s.ReviewID, s.SubjectCommitOID, safeRenderExternal(s.ReasonsPath)))
			continue
		}
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
				action = app.GrammarAttentionActionSession(safeRenderExternal(sessionBindingFor(m.Address, detail.Sessions)))
			}
			lines = append(lines, "    "+app.GrammarActionPrefix+"      "+action)
		}
	}

	for _, q := range detail.PendingQuestions {
		lines = append(lines,
			"  "+app.GrammarQuestionLine(q.MessageID, q.Age, safeRenderExternal(q.BodyPath)),
			"    "+app.GrammarAnswerInvocationLine(q.MessageID),
		)
	}

	for _, s := range detail.Sessions {
		taskLabel := "(none)"
		if s.TaskID != "" {
			taskLabel = taskLabelFor(s.TaskID, labels)
		}
		lines = append(lines, "  "+app.GrammarSessionLine(s.SessionID, s.Role, s.State, taskLabel, s.AttemptNumber, safeRenderExternal(orUnset(s.BindingSummary))))
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
