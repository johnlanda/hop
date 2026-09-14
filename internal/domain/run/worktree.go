package run

import "github.com/johnlanda/hop/internal/domain/identity"

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
	Path         string
	Branch       string
	State        WorktreeState
}

// NewWorktree constructs a worktree in its active state.
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
