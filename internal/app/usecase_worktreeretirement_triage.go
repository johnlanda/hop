package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// Lease-free worktree-retirement triage
// (docs/plan/phase-3-worktree-retirement.md section 3): before any pass,
// status and controller starts decide from a lease-free read and
// unclaimed, read-only git which runs have retirement work at all. Only
// those take a lease, so a repeated `hop status` over an unchanged,
// unmerged run takes no lease and writes nothing.

// RetirementCandidateRecord is one retirement-eligible run as the
// lease-free read reports it: a terminal feature run of the repository
// with a frozen target branch, its worktrees-retired fact unset, and at
// least one integrated row that added content.
type RetirementCandidateRecord struct {
	RunID    identity.RunID
	Sequence int
	// RepositoryRoot is the run's frozen repository root.
	RepositoryRoot string
	// TargetBranch is the frozen full target ref.
	TargetBranch string
	// Integrated are the run's integrated rows, the validated-head rule's
	// input.
	Integrated []run.Integration
	// Checks are the run's retirement.check operations in any state,
	// newest first.
	Checks []Operation
	// Unresolved reports a pending or reconciling retirement.check or
	// worktree.retire operation.
	Unresolved bool
}

// RetirementReadStore is the triage's lease-free read, declared
// separately from ReadStore under the additive packaging rule and reached
// by RequireRetirementReadStore.
type RetirementReadStore interface {
	// ListRetirementCandidates returns the repository's
	// retirement-eligible runs in sequence order. repositoryRoot is the
	// symlink-resolved absolute root, exactly as ListRuns takes it; an
	// unknown root has no candidates.
	ListRetirementCandidates(ctx context.Context, repositoryRoot string) ([]RetirementCandidateRecord, error)
}

// ErrRetirementReadStoreUnsupported reports a ReadStore that does not
// implement RetirementReadStore; triage fails closed with it.
var ErrRetirementReadStoreUnsupported = errors.New("app: read store does not implement RetirementReadStore")

// RequireRetirementReadStore asserts that read also implements
// RetirementReadStore, returning ErrRetirementReadStoreUnsupported
// (wrapped with reason) when it does not.
func RequireRetirementReadStore(read ReadStore, reason string) (RetirementReadStore, error) {
	store, ok := read.(RetirementReadStore)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrRetirementReadStoreUnsupported, reason)
	}
	return store, nil
}

// RetirementCandidate is one run's triage verdict.
type RetirementCandidate struct {
	RunID    string
	Sequence int
	// NeedsPass reports that a retirement pass has work for the run: an
	// unresolved operation to recover, a merged run to finish (its active
	// worktrees, or only its fact), or a head and target tip never
	// checked.
	NeedsPass bool
	// Disposition says why a run needs no pass: not-merged,
	// nothing-integrated, history-missing or check-failed. It is empty
	// when NeedsPass holds.
	Disposition WorktreeRetirementDisposition
}

// RetirementCandidates triages the repository's retirement-eligible runs
// without a lease, skipping excludeRunID (the invoking controller's own
// run; "" skips nothing). The rules are detection's own, applied in the
// same order: the validated head, the repository-identity check (a moved
// or replaced repository is history-missing and never leased), any
// unresolved operation, a settled merge, then the target tip against the
// settled answers. It never writes, never takes a lease and spawns
// nothing claimed.
func (c *Controller) RetirementCandidates(ctx context.Context, repositoryRoot, excludeRunID string) ([]RetirementCandidate, error) {
	store, err := RequireRetirementReadStore(c.Read, "worktree retirement triage")
	if err != nil {
		return nil, err
	}
	records, err := store.ListRetirementCandidates(ctx, repositoryRoot)
	if err != nil {
		return nil, fmt.Errorf("app: list worktree-retirement candidates: %w", err)
	}
	candidates := make([]RetirementCandidate, 0, len(records))
	for i := range records {
		if records[i].RunID.String() == excludeRunID {
			continue
		}
		candidates = append(candidates, c.triageRetirement(ctx, &records[i]))
	}
	return candidates, nil
}

// triageRetirement decides one record.
func (c *Controller) triageRetirement(ctx context.Context, record *RetirementCandidateRecord) RetirementCandidate {
	verdict := RetirementCandidate{RunID: record.RunID.String(), Sequence: record.Sequence}
	head, hasContent, ambiguous := latestIntegratedHead(record.Integrated)
	switch {
	case ambiguous:
		verdict.Disposition = RetirementCheckFailed
		return verdict
	case head == "" || !hasContent:
		verdict.Disposition = RetirementNothingIntegrated
		return verdict
	case !c.repositoryHoldsHead(ctx, record.RepositoryRoot, head):
		verdict.Disposition = RetirementHistoryMissing
		return verdict
	case record.Unresolved, settledMerged(record.Checks):
		verdict.NeedsPass = true
		return verdict
	}
	tip, gone, failure := c.targetTip(ctx, record.RepositoryRoot, record.TargetBranch)
	switch {
	case failure != "":
		verdict.Disposition = RetirementCheckFailed
		return verdict
	case gone:
		verdict.Disposition = RetirementNotMerged
		return verdict
	}
	switch answer, answered := settledAnswer(record.Checks, head, tip); {
	case answered && answer == ancestryNotContained:
		verdict.Disposition = RetirementNotMerged
	case answered:
		verdict.Disposition = RetirementCheckFailed
	default:
		verdict.NeedsPass = true
	}
	return verdict
}
