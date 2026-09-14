package run_test

import (
	"testing"

	"github.com/johnlanda/hop/internal/domain/run"
)

func TestNewWorktree(t *testing.T) {
	w := run.NewWorktree(testWorktreeID, testRepositoryID, testRunID, "/tmp/hop/run/worktree", "hop/run-1")

	if w.State != run.WorktreeActive {
		t.Fatalf("NewWorktree State = %s, want active", w.State)
	}
	if w.ID != testWorktreeID || w.RepositoryID != testRepositoryID || w.RunID != testRunID {
		t.Fatalf("NewWorktree did not preserve its identity inputs: %+v", w)
	}
	if w.Path != "/tmp/hop/run/worktree" || w.Branch != "hop/run-1" {
		t.Fatalf("NewWorktree did not preserve its path/branch inputs: %+v", w)
	}
}
