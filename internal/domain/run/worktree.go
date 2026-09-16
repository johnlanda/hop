package run

import (
	"fmt"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// WorktreeState is a worktree's lifecycle state. A worktree is created
// active before launch and stays active through stop and failure; only
// post-merge worktree retirement moves it on, to one of three final
// states.
type WorktreeState string

// Worktree states.
const (
	// WorktreeActive is a worktree HOP still owns: its checkout may exist
	// on disk and a retirement pass may still act on it.
	WorktreeActive WorktreeState = "active"
	// WorktreeRemoved is a checkout the retirement pass removed.
	WorktreeRemoved WorktreeState = "removed"
	// WorktreeAbsent is a checkout the retirement pass found already gone.
	WorktreeAbsent WorktreeState = "absent"
	// WorktreeReleased is a checkout HOP relinquished because it could no
	// longer prove the checkout is its own; HOP never touches it again.
	WorktreeReleased WorktreeState = "released"
)

// worktreeTransitions is the exhaustive Worktree state table: each final
// state is reached only from active, and none is ever left.
var worktreeTransitions = newTransitionTable(concatPairs( //nolint:gochecknoglobals // worktreeTransitions is the exhaustive, immutable Worktree state table; it never mutates after init.
	fromAny(WorktreeRemoved, WorktreeActive),
	fromAny(WorktreeAbsent, WorktreeActive),
	fromAny(WorktreeReleased, WorktreeActive),
))

// Worktree is checkout provenance used by one or more sessions: a path and
// branch created before launch in the run's repository.
type Worktree struct {
	ID           identity.WorktreeID
	RepositoryID identity.RepositoryID
	RunID        identity.RunID
	// AttemptID is the attempt a feature-mode worktree was created for;
	// empty for a solo run's single, unlinked worktree.
	AttemptID identity.AttemptID
	// BaseCommit is the object ID the attempt's worktree was created from
	// and verified against; empty for a solo worktree, whose base lives
	// only in its worktree.create operation intent.
	BaseCommit string
	Path       string
	Branch     string
	State      WorktreeState
}

// NewWorktree constructs an unlinked worktree (no attempt, no base
// commit) in its active state: the solo run's single worktree.
func NewWorktree(id identity.WorktreeID, repositoryID identity.RepositoryID, runID identity.RunID, path, branch string) Worktree {
	return Worktree{
		ID:           id,
		RepositoryID: repositoryID,
		RunID:        runID,
		Path:         path,
		Branch:       branch,
		State:        WorktreeActive,
	}
}

// NewAttemptWorktree constructs a feature-mode worktree linked to the
// attempt it was created for and the base commit it was verified against,
// in its active state. Both links are required (ErrInvalidTransition
// otherwise): an attempt worktree without them is indistinguishable from
// a solo row, and the launch boundary could not find it by attempt.
func NewAttemptWorktree(id identity.WorktreeID, repositoryID identity.RepositoryID, runID identity.RunID, attemptID identity.AttemptID, baseCommit, path, branch string) (Worktree, error) {
	if attemptID == "" || baseCommit == "" {
		return Worktree{}, fmt.Errorf("%w: worktree %s: an attempt worktree names its attempt and its base commit", ErrInvalidTransition, id)
	}
	w := NewWorktree(id, repositoryID, runID, path, branch)
	w.AttemptID = attemptID
	w.BaseCommit = baseCommit
	return w, nil
}

// Retire returns w moved to one of the final retirement states (removed,
// absent or released), or ErrInvalidTransition when w is not active or to
// is not a final state.
func (w Worktree) Retire(to WorktreeState) (Worktree, error) { //nolint:gocritic // hugeParam: Worktree is an immutable domain value returned by its transition; a pointer receiver would let a caller's original be mutated through it.
	if !worktreeTransitions.valid(w.State, to) {
		return w, fmt.Errorf("%w: worktree %s: %s to %s", ErrInvalidTransition, w.ID, w.State, to)
	}
	w.State = to
	return w, nil
}
