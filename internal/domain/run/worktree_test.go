package run_test

import (
	"errors"
	"testing"

	"github.com/johnlanda/hop/internal/domain/identity"
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
	if w.AttemptID != "" || w.BaseCommit != "" {
		t.Fatalf("NewWorktree linked an attempt or base commit: %+v", w)
	}
}

func TestNewAttemptWorktree(t *testing.T) {
	const base = "cccccccccccccccccccccccccccccccccccccccc"

	t.Run("links the attempt and base commit", func(t *testing.T) {
		w, err := run.NewAttemptWorktree(testWorktreeID, testRepositoryID, testRunID, testAttemptID, base, "/tmp/hop/r1/t1a1", "hop/r1/t1a1")
		if err != nil {
			t.Fatalf("NewAttemptWorktree: %v", err)
		}
		want := run.Worktree{
			ID: testWorktreeID, RepositoryID: testRepositoryID, RunID: testRunID,
			AttemptID: testAttemptID, BaseCommit: base,
			Path: "/tmp/hop/r1/t1a1", Branch: "hop/r1/t1a1", State: run.WorktreeActive,
		}
		if w != want {
			t.Fatalf("NewAttemptWorktree = %+v, want %+v", w, want)
		}
	})

	for _, tc := range []struct {
		name       string
		attemptID  identity.AttemptID
		baseCommit string
	}{
		{"missing attempt", "", base},
		{"missing base commit", testAttemptID, ""},
		{"missing both", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, err := run.NewAttemptWorktree(testWorktreeID, testRepositoryID, testRunID, tc.attemptID, tc.baseCommit, "/tmp/hop/r1/t1a1", "hop/r1/t1a1")
			if !errors.Is(err, run.ErrInvalidTransition) {
				t.Fatalf("NewAttemptWorktree error = %v, want ErrInvalidTransition", err)
			}
			if w != (run.Worktree{}) {
				t.Fatalf("NewAttemptWorktree returned a value on refusal: %+v", w)
			}
		})
	}
}
