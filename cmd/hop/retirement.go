package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// The worktree-retirement pass as the commands run it
// (docs/plan/phase-3-worktree-retirement.md sections 3 and 6): lease-free
// triage for the repository, then, for each run with work, the retirement
// lease, concurrent heartbeats, one pass and a plain release. The pass
// never changes a command's exit code and never dials Herdr.

const (
	// retirementPassTimeout bounds one command's whole retirement pass,
	// every run included; each removal act is bounded by the app's own
	// five-minute act timeout within it.
	retirementPassTimeout = 15 * time.Minute
	// retirementReleaseTimeout bounds the release after a pass, detached
	// from the pass's own (possibly canceled) context.
	retirementReleaseTimeout = 5 * time.Second
)

// runWorktreeRetirement runs the pass for the repository at repoRoot,
// skipping excludeRunID (the invoking controller's own run), and returns
// the section 6 lines to print. It prints `retiring worktrees of r<seq>…`
// to stdout immediately before a run's first removal act. A failure is
// reported to stderr as one value-free line prefixed with command, and the
// remaining runs are still attempted.
func runWorktreeRetirement(ctx context.Context, d *deps, ctrl controllerAPI, repoRoot, excludeRunID, hopPath string, stdout, stderr io.Writer, command string) ([]string, error) {
	candidates, err := ctrl.RetirementCandidates(ctx, repoRoot, excludeRunID)
	if err != nil {
		_, werr := fmt.Fprintf(stderr, "%s: worktree retirement could not list this repository's runs; the next hop status retries\n", command)
		return nil, werr
	}
	var lines []string
	for _, candidate := range candidates {
		label := seqLabel(candidate.Sequence)
		if !candidate.NeedsPass {
			lines = append(lines, dispositionLines(label, candidate.Disposition)...)
			continue
		}
		handle, acquireErr := ctrl.AcquireForRetirement(ctx, candidate.RunID, d.newID())
		switch {
		case errors.Is(acquireErr, app.ErrLeaseHeld):
			lines = append(lines, label+" worktree retirement deferred: the run is held by another controller")
			continue
		case errors.Is(acquireErr, app.ErrRetirementNotEligible):
			continue
		case acquireErr != nil:
			if _, werr := fmt.Fprintf(stderr, "%s: worktree retirement of %s could not take the run's lease; the next hop status retries\n", command, label); werr != nil {
				return lines, werr
			}
			continue
		}
		report, passErr := retireOneRun(ctx, d, ctrl, handle, label, hopPath, stdout)
		lines = append(lines, reportLines(label, &report)...)
		if passErr != nil {
			if _, werr := fmt.Fprintf(stderr, "%s: worktree retirement of %s stopped early; the next hop status continues it\n", command, label); werr != nil {
				return lines, werr
			}
		}
	}
	return lines, nil
}

// retireOneRun runs one pass under handle's lease with concurrent
// heartbeats, then releases the lease; a lost heartbeat is a pass error.
func retireOneRun(ctx context.Context, d *deps, ctrl controllerAPI, handle app.RunHandle, label, hopPath string, stdout io.Writer) (app.WorktreeRetirementReport, error) { //nolint:gocritic // hugeParam: RunHandle is the app-defined opaque token, passed by value as every Controller method takes it.
	passCtx, stopHeartbeats := context.WithCancel(ctx)
	heartbeatFailed := runHeartbeats(passCtx, d, ctrl, handle)
	var announceErr error
	report, passErr := ctrl.RetireWorktrees(passCtx, handle, app.RetireWorktreesOptions{
		HOPPath:     hopPath,
		Environ:     d.environ(),
		InspectPath: d.inspectPath,
		BeforeFirstRemoval: func() {
			_, announceErr = fmt.Fprintf(stdout, "retiring worktrees of %s…\n", label)
		},
	})
	stopHeartbeats()
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), retirementReleaseTimeout)
	defer cancel()
	releaseErr := ctrl.ReleaseRetirement(releaseCtx, handle)
	var heartbeatErr error
	select {
	case heartbeatErr = <-heartbeatFailed:
	default:
	}
	return report, errors.Join(passErr, heartbeatErr, releaseErr, announceErr)
}

// dispositionLines renders a run triage left alone; only a skipped
// repository and an undecidable check are worth a line.
func dispositionLines(label string, disposition app.WorktreeRetirementDisposition) []string {
	switch disposition {
	case app.RetirementHistoryMissing:
		return []string{label + " worktree retirement skipped: the repository root no longer holds this run's history"}
	case app.RetirementCheckFailed:
		return []string{label + " worktree retirement check failed: whether the run is merged could not be decided"}
	case app.RetirementBlocked:
		return []string{label + " worktree retirement blocked: an earlier retirement process could not be verified gone"}
	case app.RetirementInterrupted:
		return []string{label + " worktree retirement interrupted: hop status will continue it"}
	default:
		return nil
	}
}

// reportLines renders one pass report as section 6's fixed lines: the
// retired summary first, then one line per worktree the pass examined
// (and a retained worktree's action), then the run-level disposition.
func reportLines(label string, report *app.WorktreeRetirementReport) []string {
	var lines []string
	if report.Disposition == app.RetirementRetired {
		lines = append(lines, fmt.Sprintf("%s worktrees retired: %d removed, %d already absent, %d released (removal deletes ignored files such as build output)",
			label, report.Removed, report.Absent, report.Released))
	}
	for i := range report.Worktrees {
		lines = append(lines, worktreeOutcomeLines(label, &report.Worktrees[i])...)
	}
	if report.Disposition != app.RetirementRetired {
		lines = append(lines, dispositionLines(label, report.Disposition)...)
	}
	return lines
}

// worktreeOutcomeLines renders one worktree's pass outcome. Branch and
// path are operator-selected strings the app layer passes through
// unvalidated; safeRenderExternal renders each raw only when it cannot
// forge a line boundary or emit a terminal control sequence.
func worktreeOutcomeLines(label string, w *app.WorktreeRetirementLine) []string {
	prefix := label + " worktree " + safeRenderExternal(w.Branch)
	path := safeRenderExternal(w.Path)
	switch w.Outcome {
	case app.WorktreeOutcomeRemoved:
		return []string{fmt.Sprintf("%s removed: %s (close its Herdr workspace if one is still open)", prefix, path)}
	case app.WorktreeOutcomeAbsent:
		return []string{fmt.Sprintf("%s already absent: %s", prefix, path)}
	case app.WorktreeOutcomeReleased:
		if w.LeftOnDisk {
			return []string{fmt.Sprintf("%s released (%s): %s; left on disk; no longer managed by HOP", prefix, releasedLabel(w.Released), path)}
		}
		return []string{fmt.Sprintf("%s released (%s): %s; HOP will not remove it", prefix, releasedLabel(w.Released), path)}
	case app.WorktreeOutcomeRetained:
		category := retainedLabel(w.Retained)
		if w.Retained == app.RetainedRemoveRefused {
			category = fmt.Sprintf("%s, exit %d", category, w.ExitCode)
		}
		return []string{
			fmt.Sprintf("%s retained (%s): %s", prefix, category, path),
			"  " + app.GrammarActionPrefix + " " + retainedAction(w.Retained, w.EvidencePath),
		}
	case app.WorktreeOutcomeIncomplete:
		return []string{fmt.Sprintf("%s removal incomplete: %s; hop status will finish the removal", prefix, path)}
	case app.WorktreeOutcomeUnresolved:
		return []string{fmt.Sprintf("%s removal unresolved: %s; hop status will finish the removal", prefix, path)}
	case app.WorktreeOutcomeNotDispatched:
		return []string{fmt.Sprintf("%s not removed yet: %s; hop status will retry", prefix, path)}
	default:
		return []string{fmt.Sprintf("%s %s: %s", prefix, w.Outcome, path)}
	}
}

// worktreeDetailLines renders hop status -run's per-row lines for a
// feature run (section 6), each non-final row with its human action.
// Branch and path render through safeRenderExternal, exactly like
// worktreeOutcomeLines.
func worktreeDetailLines(views []app.WorktreeView) []string {
	var lines []string
	for i := range views {
		w := &views[i]
		prefix := "  worktree:      " + safeRenderExternal(w.Branch) + " "
		path := safeRenderExternal(w.Path)
		switch {
		case w.State == "released":
			lines = append(lines, prefix+"released ("+releasedLabel(w.Released)+") "+path+"; left on disk; no longer managed by HOP")
		case w.State != "active":
			lines = append(lines, prefix+w.State+" "+path)
		case w.Removal != "":
			lines = append(lines, prefix+"removal "+w.Removal+" "+path+"; hop status will finish the removal")
		case w.Retained != "":
			lines = append(lines,
				prefix+"retained ("+retainedLabel(w.Retained)+") "+path,
				"    "+app.GrammarActionPrefix+"      "+retainedAction(w.Retained, w.EvidencePath))
		default:
			lines = append(lines, prefix+"active "+path)
		}
	}
	return lines
}

// retainedLabel is a retained category's human wording.
func retainedLabel(category app.WorktreeRetainedCategory) string {
	switch category {
	case app.RetainedUncommittedChanges:
		return "uncommitted changes"
	case app.RetainedHiddenChanges:
		return "hidden changes"
	case app.RetainedLocked:
		return "locked"
	case app.RetainedInterruptedRemoval:
		return "interrupted removal"
	case app.RetainedRemoveRefused:
		return "removal refused"
	case app.RetainedInspectionFailed:
		return "inspection failed"
	default:
		return string(category)
	}
}

// retainedAction is a retained category's human action (section 6); each
// ends ", then run hop status again".
func retainedAction(category app.WorktreeRetainedCategory, evidencePath string) string {
	action := "check the checkout"
	switch category {
	case app.RetainedUncommittedChanges:
		action = "commit or discard the changes"
	case app.RetainedHiddenChanges:
		action = "clear git update-index --no-assume-unchanged/--no-skip-worktree, then commit or discard"
	case app.RetainedLocked:
		action = "git worktree unlock"
	case app.RetainedInterruptedRemoval:
		action = "inspect; restore (git checkout -- .) or remove it yourself"
	case app.RetainedRemoveRefused:
		action = "inspect the retained evidence"
		if evidencePath != "" {
			action += " at " + safeRenderExternal(evidencePath)
		}
	case app.RetainedInspectionFailed:
		action = "the checkout could not be fully inspected; check it and its repository"
	}
	return action + ", then run hop status again"
}

// releasedLabel is a release reason's human wording.
func releasedLabel(reason app.WorktreeReleaseReason) string {
	switch reason {
	case app.ReleasedRepositoryRoot:
		return "the repository root"
	case app.ReleasedNotRegistered:
		return "not a registered worktree"
	case app.ReleasedOtherBranch:
		return "checked out on another branch"
	case app.ReleasedDetached:
		return "detached HEAD"
	case app.ReleasedOtherRepository:
		return "belongs to another repository"
	case app.ReleasedBaseNotAncestor:
		return "no longer descends from its base"
	case app.ReleasedUnverified:
		return "not verifiably HOP's"
	default:
		return string(reason)
	}
}

// printLines writes lines to w, one per line.
func printLines(w io.Writer, lines []string) error {
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}
