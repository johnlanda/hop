package run

import (
	"fmt"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// WorktreeState is a worktree's lifecycle state. Phase 2 never removes a
// worktree: it is created before launch and preserved by both stop and
// failure, so active is the only state this phase's domain code reaches.
type WorktreeState string

// WorktreeActive is a worktree's only state in Phase 2.
const WorktreeActive WorktreeState = "active"

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
