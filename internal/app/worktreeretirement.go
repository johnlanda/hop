package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// The post-merge worktree retirement operation kinds and their frozen
// exec shapes (docs/plan/phase-3-worktree-retirement.md section 7). Both
// are exec-claimable: hop check-exec claims either exactly as it claims a
// check.run or integration.merge execution, and the store resolves the
// frozen argv and spawn directory from the intent's
// worktreeRetirementExecIntent members.
const (
	// OpRetirementCheck is the detection execution: whether the run's
	// validated integration head is contained in the target branch's tip,
	// both frozen into the intent as object ids.
	OpRetirementCheck OperationKind = "retirement.check"
	// OpWorktreeRetire is one attempt worktree's removal execution, a
	// `git worktree remove` that never carries a force option.
	OpWorktreeRetire OperationKind = "worktree.retire"
)

// ExecClaimable reports whether hop check-exec may claim an operation of
// this kind: the check execution, the scratch merge and the two
// retirement executions. Every other kind is an act the controller
// performs itself (a ref move, a Herdr call) and never accepts a claim.
// The real store and the fakes decide claimability through this one
// predicate.
func (k OperationKind) ExecClaimable() bool {
	switch k {
	case OpCheckRun, OpIntegrationMerge, OpRetirementCheck, OpWorktreeRetire:
		return true
	default:
		return false
	}
}

// worktreeRetirementExecIntent is the exec-boundary part of both retirement
// intents: the argv hop check-exec runs byte for byte and the directory
// the spawn runs in. The JSON keys "argv" and "cwd" are the store's
// documented contract (LoadCheckExecutionContext reads them for both
// retirement kinds).
type worktreeRetirementExecIntent struct {
	Argv []string `json:"argv"`
	Cwd  string   `json:"cwd"`
}

// worktreeRetirementCheckArgv is the frozen detection argv: exit 0 when headOID
// is contained in targetOID (a commit contains itself), 1 when it is not,
// 128 for an unknown object (probe-pinned). Both revisions are object
// ids, never ref names, so the answer depends only on the immutable
// object graph.
func worktreeRetirementCheckArgv(git, repositoryRoot, headOID, targetOID string) []string {
	return []string{git, "-C", repositoryRoot, "merge-base", "--is-ancestor", headOID, targetOID}
}

// worktreeRetireArgv is the frozen removal argv. It never carries a force
// option, so git's own refusal of a checkout with modified, staged or
// untracked files stays in force; the `-c status.showUntrackedFiles=all`
// override keeps untracked files visible to that refusal whatever the
// repository's own configuration says (probe-pinned: a repository-local
// `no` otherwise lets the removal delete them).
func worktreeRetireArgv(git, repositoryRoot, path string) []string {
	return []string{git, "-C", repositoryRoot, "-c", "status.showUntrackedFiles=all", "worktree", "remove", path}
}

// WorktreeRetirementRepositories is the controller-transaction surface
// worktree retirement adds to a unit of work. It is declared separately
// from UnitOfWork and WorkflowRepositories under the additive packaging
// rule, and reached through RequireWorktreeRetirementRepositories.
type WorktreeRetirementRepositories interface {
	// WorktreesForRetirement returns every worktree row of the leased run,
	// in any state, oldest first, each with the revision a state save
	// expects (Worktrees().Save). A run other than the unit of work's
	// leased run is refused with ErrFenced; a run without rows returns
	// none.
	WorktreesForRetirement(ctx context.Context, runID identity.RunID) ([]RetirementWorktree, error)
	// WorktreesRetiredAt reads the leased run's worktrees-retired fact
	// inside the unit of work, this transaction's own mark included: nil
	// while it is unset. A run other than the unit of work's leased run is
	// refused with ErrFenced, and an unknown run with ErrNotFound.
	WorktreesRetiredAt(ctx context.Context, runID identity.RunID) (*time.Time, error)
	// MarkWorktreesRetired sets the run's worktrees-retired fact to at when
	// it is unset; a fact already set keeps its first value and the call
	// succeeds. A run other than the unit of work's leased run is refused
	// with ErrFenced before anything is written, and an unknown run with
	// ErrNotFound.
	MarkWorktreesRetired(ctx context.Context, runID identity.RunID, at time.Time) error
}

// RetirementWorktree is one worktree row as the retirement pass reads it
// under its lease: the domain value, with its attempt link and verified
// base commit, and its current revision.
type RetirementWorktree struct {
	Worktree run.Worktree
	Revision int64
}

// ErrWorktreeRetirementUnsupported reports that a unit of work does not
// implement WorktreeRetirementRepositories: the retirement pass fails
// closed before any side effect, never a nil-interface panic.
var ErrWorktreeRetirementUnsupported = errors.New("app: unit of work does not implement WorktreeRetirementRepositories")

// RequireWorktreeRetirementRepositories asserts that uow also implements
// WorktreeRetirementRepositories, returning
// ErrWorktreeRetirementUnsupported (wrapped with reason) when it does not.
func RequireWorktreeRetirementRepositories(uow UnitOfWork, reason string) (WorktreeRetirementRepositories, error) {
	repos, ok := uow.(WorktreeRetirementRepositories)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrWorktreeRetirementUnsupported, reason)
	}
	return repos, nil
}
