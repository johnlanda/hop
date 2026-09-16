package app

import (
	"context"
	"fmt"

	"github.com/johnlanda/hop/internal/domain/run"
)

// The worktree-retirement pass driver
// (docs/plan/phase-3-worktree-retirement.md section 3), called by cmd/hop
// between AcquireForRetirement and ReleaseRetirement while composition
// heartbeats the handle.

// RetireWorktreesOptions are RetireWorktrees' composition inputs.
type RetireWorktreesOptions struct {
	// HOPPath is the absolute hop executable every claimed act is spawned
	// through (`hop check-exec`).
	HOPPath string
	// Environ is the invoking process's environment; the pass sanitizes it
	// under the run's frozen policy before any spawn.
	Environ []string
	// InspectPath resolves recorded paths to their canonical form and
	// reports whether they exist.
	InspectPath PathInspector
	// BeforeFirstRemoval, when set, is called once, immediately before the
	// pass spawns its first removal act.
	BeforeFirstRemoval func()
}

// WorktreeRetirementDisposition is what one pass concluded for a run.
type WorktreeRetirementDisposition string

// The pass dispositions.
const (
	// RetirementRetired: every worktree row is final and the run's
	// worktrees-retired fact was set by this pass.
	RetirementRetired WorktreeRetirementDisposition = "retired"
	// RetirementInProgress: the run is merged, but some worktree is still
	// retained or awaiting a later pass.
	RetirementInProgress WorktreeRetirementDisposition = "in-progress"
	// RetirementNotMerged: the integration head is not in the target (or
	// the target branch is gone).
	RetirementNotMerged WorktreeRetirementDisposition = "not-merged"
	// RetirementNothingIntegrated: the run integrated no new content and
	// never retires automatically.
	RetirementNothingIntegrated WorktreeRetirementDisposition = "nothing-integrated"
	// RetirementHistoryMissing: the frozen root no longer holds the run's
	// history (a moved or replaced repository); nothing was touched.
	RetirementHistoryMissing WorktreeRetirementDisposition = "history-missing"
	// RetirementBlocked: an earlier retirement execution could not be
	// verified gone or settled.
	RetirementBlocked WorktreeRetirementDisposition = "blocked"
	// RetirementCheckFailed: the detection check failed or an observation
	// it needs could not be made.
	RetirementCheckFailed WorktreeRetirementDisposition = "check-failed"
	// RetirementInterrupted: the pass lost its lease or an outcome before it
	// finished; the next pass continues.
	RetirementInterrupted WorktreeRetirementDisposition = "interrupted"
)

// WorktreeRetirementOutcome is what a pass did with one worktree.
type WorktreeRetirementOutcome string

// The per-worktree outcomes.
const (
	WorktreeOutcomeRemoved       WorktreeRetirementOutcome = "removed"
	WorktreeOutcomeAbsent        WorktreeRetirementOutcome = "absent"
	WorktreeOutcomeReleased      WorktreeRetirementOutcome = "released"
	WorktreeOutcomeRetained      WorktreeRetirementOutcome = "retained"
	WorktreeOutcomeIncomplete    WorktreeRetirementOutcome = "incomplete"
	WorktreeOutcomeUnresolved    WorktreeRetirementOutcome = "unresolved"
	WorktreeOutcomeNotDispatched WorktreeRetirementOutcome = "not-dispatched"
)

// WorktreeRetirementLine is one worktree's line in a pass report.
type WorktreeRetirementLine struct {
	// Branch and Path are the worktree row's recorded branch name and
	// path.
	Branch  string
	Path    string
	Outcome WorktreeRetirementOutcome
	// Retained is the category of a retained worktree.
	Retained WorktreeRetainedCategory
	// Released is the reason of a released worktree.
	Released WorktreeReleaseReason
	// LeftOnDisk marks a worktree released after a removal act that left
	// its directory on disk; HOP no longer manages it.
	LeftOnDisk bool
	// EvidencePath names a failed removal act's retained stderr.
	EvidencePath string
	// ExitCode is a removal act's observed exit status.
	ExitCode int
}

// WorktreeRetirementReport is one pass's result for one run.
type WorktreeRetirementReport struct {
	RunID       string
	Sequence    int
	Disposition WorktreeRetirementDisposition
	// Removed, Absent and Released count the run's worktree rows in each
	// final state once the pass finished.
	Removed  int
	Absent   int
	Released int
	// Worktrees are this pass's per-worktree lines: every row it examined,
	// oldest first. Rows already final before the pass are not repeated.
	Worktrees []WorktreeRetirementLine
}

// RetireWorktrees runs one worktree-retirement pass over the run whose
// retirement lease handle holds (AcquireForRetirement): the frozen run
// and the sanitized spawn environment, detection (the head, the
// repository identity, recovery of earlier passes' operations, then the
// ancestry check), the removal step for a merged run, and — once every
// worktree row is final — the run's worktrees-retired fact at the clock's
// current time. It changes no run state, and an error names no path or
// environment value.
func (c *Controller) RetireWorktrees(ctx context.Context, handle RunHandle, opts RetireWorktreesOptions) (WorktreeRetirementReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; RetireWorktreesOptions is a per-pass value; called once per pass.
	detail, err := c.Read.LoadRunStatus(ctx, handle.runID)
	if err != nil {
		return WorktreeRetirementReport{}, fmt.Errorf("app: load run status: %w", err)
	}
	report := WorktreeRetirementReport{RunID: handle.runID.String(), Sequence: detail.Sequence}
	frozen, err := c.Read.LoadFrozenRun(ctx, handle.runID)
	if err != nil {
		return report, fmt.Errorf("app: load frozen run: %w", err)
	}
	if !frozen.Snapshot.Workflow.Feature() || frozen.Snapshot.Workflow.TargetBranch == "" {
		return report, fmt.Errorf("%w: not a feature run with a target branch", ErrRetirementNotEligible)
	}
	spawnEnv, err := c.CheckSpawnEnvironment(ctx, handle, opts.Environ)
	if err != nil {
		return report, err
	}
	passOpts := &retirementPassOptions{
		HOPPath: opts.HOPPath, SpawnEnv: spawnEnv, InspectPath: opts.InspectPath,
		beforeFirstRemoval: opts.BeforeFirstRemoval,
	}

	detection, err := c.detectForRetirement(ctx, handle, &frozen, passOpts)
	if err != nil {
		return report, err
	}
	switch detection.State {
	case detectionMerged:
	case detectionNotMerged:
		report.Disposition = RetirementNotMerged
		return report, nil
	case detectionNothingIntegrated:
		report.Disposition = RetirementNothingIntegrated
		return report, nil
	case detectionHistoryMissing:
		report.Disposition = RetirementHistoryMissing
		return report, nil
	case detectionBlocked:
		report.Disposition = RetirementBlocked
		return report, nil
	case detectionFailed:
		report.Disposition = RetirementCheckFailed
		return report, nil
	case detectionInterrupted:
		report.Disposition = RetirementInterrupted
		return report, nil
	default:
		return report, fmt.Errorf("app: unknown retirement detection state %q", detection.State)
	}

	rows, err := c.removeRunWorktrees(ctx, handle, &frozen, passOpts)
	for i := range rows {
		report.Worktrees = append(report.Worktrees, rows[i].line())
	}
	if err != nil {
		return report, err
	}
	for i := range rows {
		if rows[i].Outcome == rowNotDispatched {
			report.Disposition = RetirementInterrupted
			return report, nil
		}
	}
	retired, err := c.markWorktreesRetiredWhenFinal(ctx, handle, &report)
	if err != nil {
		return report, err
	}
	report.Disposition = RetirementInProgress
	if retired {
		report.Disposition = RetirementRetired
	}
	return report, nil
}

// line renders a row report as its exported line.
func (r *retirementRowReport) line() WorktreeRetirementLine {
	return WorktreeRetirementLine{
		Branch: r.Branch, Path: r.Path, Outcome: WorktreeRetirementOutcome(r.Outcome),
		Retained: r.Retained, Released: r.Released, LeftOnDisk: r.LeftOnDisk,
		EvidencePath: r.EvidencePath, ExitCode: r.ExitCode,
	}
}

// markWorktreesRetiredWhenFinal counts the run's final worktree rows into
// report and, when every row is final, sets the run's worktrees-retired
// fact, all in one unit of work. It reports whether the fact is set.
func (c *Controller) markWorktreesRetiredWhenFinal(ctx context.Context, handle RunHandle, report *WorktreeRetirementReport) (bool, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per pass.
	retired := false
	now := c.Clock.Now()
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		repos, err := RequireWorktreeRetirementRepositories(uow, "worktree retirement")
		if err != nil {
			return err
		}
		rows, err := repos.WorktreesForRetirement(ctx, handle.runID)
		if err != nil {
			return err
		}
		report.Removed, report.Absent, report.Released = 0, 0, 0
		active := 0
		for i := range rows {
			switch rows[i].Worktree.State {
			case run.WorktreeRemoved:
				report.Removed++
			case run.WorktreeAbsent:
				report.Absent++
			case run.WorktreeReleased:
				report.Released++
			default:
				active++
			}
		}
		if active > 0 {
			return nil
		}
		retired = true
		return repos.MarkWorktreesRetired(ctx, handle.runID, now)
	})
	if err != nil {
		return false, fmt.Errorf("app: record the worktrees-retired fact: %w", err)
	}
	return retired, nil
}
