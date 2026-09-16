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

// worktreeStates enumerates every Worktree state.
func worktreeStates() []run.WorktreeState {
	return []run.WorktreeState{run.WorktreeActive, run.WorktreeRemoved, run.WorktreeAbsent, run.WorktreeReleased}
}

// worktreeValidTransitions is the Worktree retirement table, transcribed
// independently of the production transitionTable: each final state only
// from active, and nothing out of a final state.
func worktreeValidTransitions() map[[2]run.WorktreeState]bool {
	return map[[2]run.WorktreeState]bool{
		{run.WorktreeActive, run.WorktreeRemoved}:  true,
		{run.WorktreeActive, run.WorktreeAbsent}:   true,
		{run.WorktreeActive, run.WorktreeReleased}: true,
	}
}

func TestWorktreeRetireTransitions(t *testing.T) {
	valid := worktreeValidTransitions()
	targets := append(worktreeStates(), run.WorktreeState("deleted"), "")
	for _, from := range worktreeStates() {
		for _, to := range targets {
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				w := run.NewWorktree(testWorktreeID, testRepositoryID, testRunID, "/tmp/hop/r1/t1a1", "hop/r1/t1a1")
				w.State = from

				got, err := w.Retire(to)

				if valid[[2]run.WorktreeState{from, to}] {
					if err != nil {
						t.Fatalf("Retire(%s) from %s: unexpected error: %v", to, from, err)
					}
					want := w
					want.State = to
					if got != want {
						t.Fatalf("Retire(%s) from %s = %+v, want %+v", to, from, got, want)
					}
					if w.State != from {
						t.Fatalf("Retire mutated its receiver: State = %s, want %s", w.State, from)
					}
					return
				}
				if !errors.Is(err, run.ErrInvalidTransition) {
					t.Fatalf("Retire(%s) from %s: error = %v, want ErrInvalidTransition", to, from, err)
				}
				if got != w {
					t.Fatalf("Retire(%s) from %s changed the value on refusal: %+v", to, from, got)
				}
			})
		}
	}
}
