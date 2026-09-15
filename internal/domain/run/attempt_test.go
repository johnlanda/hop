package run_test

import (
	"errors"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/domain/run"
)

// attemptStates enumerates every Attempt state. It is a function, not a
// package-level slice, only to keep this shared fixture out of the
// global-variable set the project's lint configuration restricts.
func attemptStates() []run.AttemptState {
	return []run.AttemptState{
		run.AttemptReserved, run.AttemptLaunching, run.AttemptRunning, run.AttemptSubmitted,
		run.AttemptChecking, run.AttemptCompleted, run.AttemptFailed, run.AttemptInterrupted,
		run.AttemptReconciling, run.AttemptRelaunching,
	}
}

// attemptValidTransitions is section 5's Attempt table, transcribed
// independently of the production transitionTable. It excludes the three
// reconciling reattach rows, which TestAttemptReattach checks on its own:
// Reattach is a distinct operation from MarkRunning, Submit and
// EnterChecking even where it lands on the same target state.
func attemptValidTransitions() map[[2]run.AttemptState]bool {
	return map[[2]run.AttemptState]bool{
		{run.AttemptReserved, run.AttemptLaunching}:      true,
		{run.AttemptLaunching, run.AttemptRunning}:       true,
		{run.AttemptRelaunching, run.AttemptRunning}:     true,
		{run.AttemptLaunching, run.AttemptSubmitted}:     true,
		{run.AttemptRunning, run.AttemptSubmitted}:       true,
		{run.AttemptRelaunching, run.AttemptSubmitted}:   true,
		{run.AttemptSubmitted, run.AttemptChecking}:      true,
		{run.AttemptChecking, run.AttemptCompleted}:      true,
		{run.AttemptLaunching, run.AttemptFailed}:        true,
		{run.AttemptSubmitted, run.AttemptFailed}:        true,
		{run.AttemptChecking, run.AttemptFailed}:         true,
		{run.AttemptRelaunching, run.AttemptFailed}:      true,
		{run.AttemptReserved, run.AttemptInterrupted}:    true,
		{run.AttemptLaunching, run.AttemptInterrupted}:   true,
		{run.AttemptRunning, run.AttemptInterrupted}:     true,
		{run.AttemptSubmitted, run.AttemptInterrupted}:   true,
		{run.AttemptChecking, run.AttemptInterrupted}:    true,
		{run.AttemptReconciling, run.AttemptInterrupted}: true,
		{run.AttemptRelaunching, run.AttemptInterrupted}: true,
		{run.AttemptLaunching, run.AttemptReconciling}:   true,
		{run.AttemptRunning, run.AttemptReconciling}:     true,
		{run.AttemptSubmitted, run.AttemptReconciling}:   true,
		{run.AttemptChecking, run.AttemptReconciling}:    true,
		{run.AttemptRelaunching, run.AttemptReconciling}: true,
		{run.AttemptReconciling, run.AttemptRelaunching}: true,
	}
}

func TestAttemptTransitions(t *testing.T) {
	steps := []struct {
		name string
		to   run.AttemptState
		fn   func(run.Attempt, time.Time) (run.Attempt, error)
	}{
		{"Launch", run.AttemptLaunching, run.Attempt.Launch},
		{"MarkRunning", run.AttemptRunning, run.Attempt.MarkRunning},
		{"Submit", run.AttemptSubmitted, run.Attempt.Submit},
		{"EnterChecking", run.AttemptChecking, run.Attempt.EnterChecking},
		{"Complete", run.AttemptCompleted, run.Attempt.Complete},
		{"Fail", run.AttemptFailed, run.Attempt.Fail},
		{"Interrupt", run.AttemptInterrupted, run.Attempt.Interrupt},
		{"Reconcile", run.AttemptReconciling, run.Attempt.Reconcile},
		{"Relaunch", run.AttemptRelaunching, run.Attempt.Relaunch},
	}

	valid := attemptValidTransitions()
	for _, step := range steps {
		for _, from := range attemptStates() {
			t.Run(step.name+"/"+string(from)+"_to_"+string(step.to), func(t *testing.T) {
				attempt := run.Attempt{ID: testAttemptID, TaskID: testTaskID, Number: 1, State: from}

				got, err := step.fn(attempt, epoch())

				if valid[[2]run.AttemptState{from, step.to}] {
					if err != nil {
						t.Fatalf("%s from %s: unexpected error: %v", step.name, from, err)
					}
					if got.State != step.to {
						t.Fatalf("%s from %s: State = %s, want %s", step.name, from, got.State, step.to)
					}
					if !got.UpdatedAt.Equal(epoch()) {
						t.Fatalf("%s from %s: UpdatedAt = %v, want %v", step.name, from, got.UpdatedAt, epoch())
					}
					return
				}
				if err == nil {
					t.Fatalf("%s from %s: got %+v, want ErrInvalidTransition", step.name, from, got)
				}
				if !errors.Is(err, run.ErrInvalidTransition) {
					t.Fatalf("%s from %s: error = %v, want wrapping ErrInvalidTransition", step.name, from, err)
				}
				if got.State != from {
					t.Fatalf("%s from %s: State = %s, want unchanged", step.name, from, got.State)
				}
			})
		}
	}
}

// TestAttemptReattach exhaustively checks Reattach: only a reconciling
// attempt reattaches, and only to running, submitted or checking.
func TestAttemptReattach(t *testing.T) {
	targets := append(append([]run.AttemptState{}, attemptStates()...), "made-up-state")

	for _, from := range attemptStates() {
		for _, to := range targets {
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				attempt := run.Attempt{ID: testAttemptID, TaskID: testTaskID, Number: 1, State: from}

				got, err := attempt.Reattach(to, epoch())

				wantValid := from == run.AttemptReconciling && (to == run.AttemptRunning || to == run.AttemptSubmitted || to == run.AttemptChecking)
				if wantValid {
					if err != nil {
						t.Fatalf("Reattach(%s) from %s: unexpected error: %v", to, from, err)
					}
					if got.State != to {
						t.Fatalf("Reattach(%s) from %s: State = %s, want %s", to, from, got.State, to)
					}
					if !got.UpdatedAt.Equal(epoch()) {
						t.Fatalf("Reattach(%s) from %s: UpdatedAt = %v, want %v", to, from, got.UpdatedAt, epoch())
					}
					return
				}
				if err == nil {
					t.Fatalf("Reattach(%s) from %s: got %+v, want ErrInvalidTransition", to, from, got)
				}
				if !errors.Is(err, run.ErrInvalidTransition) {
					t.Fatalf("Reattach(%s) from %s: error = %v, want wrapping ErrInvalidTransition", to, from, err)
				}
				if got.State != from {
					t.Fatalf("Reattach(%s) from %s: State = %s, want unchanged", to, from, got.State)
				}
			})
		}
	}
}

func TestNewAttempt(t *testing.T) {
	t.Run("valid number", func(t *testing.T) {
		attempt, err := run.NewAttempt(testAttemptID, testTaskID, 1, epoch())
		if err != nil {
			t.Fatalf("NewAttempt: unexpected error: %v", err)
		}
		if attempt.State != run.AttemptReserved {
			t.Fatalf("NewAttempt State = %s, want reserved", attempt.State)
		}
		if attempt.ID != testAttemptID || attempt.TaskID != testTaskID || attempt.Number != 1 {
			t.Fatalf("NewAttempt did not preserve its inputs: %+v", attempt)
		}
		if !attempt.UpdatedAt.Equal(epoch()) {
			t.Fatalf("NewAttempt UpdatedAt = %v, want %v", attempt.UpdatedAt, epoch())
		}
	})

	t.Run("number not dense from 1", func(t *testing.T) {
		_, err := run.NewAttempt(testAttemptID, testTaskID, 0, epoch())
		if !errors.Is(err, run.ErrInvalidTransition) {
			t.Fatalf("NewAttempt(number=0): error = %v, want ErrInvalidTransition", err)
		}

		_, err = run.NewAttempt(testAttemptID, testTaskID, -1, epoch())
		if !errors.Is(err, run.ErrInvalidTransition) {
			t.Fatalf("NewAttempt(number=-1): error = %v, want ErrInvalidTransition", err)
		}
	})
}

// TestAttemptCompleteReview exhaustively checks CompleteReview: only a
// submitted review-kind attempt reaches completed through it; every other
// (kind, source state) combination is ErrInvalidTransition and leaves the
// attempt unchanged.
func TestAttemptCompleteReview(t *testing.T) {
	kinds := []run.TaskKind{run.TaskKindImplement, run.TaskKindReview, ""}

	for _, kind := range kinds {
		for _, from := range attemptStates() {
			t.Run(string(kind)+"/"+string(from), func(t *testing.T) {
				attempt := run.Attempt{ID: testAttemptID, TaskID: testTaskID, Number: 1, State: from}

				got, err := attempt.CompleteReview(kind, epoch())

				wantValid := kind == run.TaskKindReview && from == run.AttemptSubmitted
				if wantValid {
					if err != nil {
						t.Fatalf("CompleteReview(%s) from %s: unexpected error: %v", kind, from, err)
					}
					if got.State != run.AttemptCompleted {
						t.Fatalf("CompleteReview(%s) from %s: State = %s, want completed", kind, from, got.State)
					}
					if !got.UpdatedAt.Equal(epoch()) {
						t.Fatalf("CompleteReview(%s) from %s: UpdatedAt = %v, want %v", kind, from, got.UpdatedAt, epoch())
					}
					return
				}
				if !errors.Is(err, run.ErrInvalidTransition) {
					t.Fatalf("CompleteReview(%s) from %s: error = %v, want ErrInvalidTransition", kind, from, err)
				}
				if got.State != from {
					t.Fatalf("CompleteReview(%s) from %s: State = %s, want unchanged", kind, from, got.State)
				}
			})
		}
	}
}

// TestNewRetryAttempt proves the retry-reservation rule: a new attempt is
// reserved only against a terminal prior attempt, and only below the
// frozen retry limit.
func TestNewRetryAttempt(t *testing.T) {
	t.Run("terminal prior, below limit", func(t *testing.T) {
		prior := run.Attempt{ID: testAttemptID, TaskID: testTaskID, Number: 1, State: run.AttemptFailed}

		got, err := run.NewRetryAttempt(testSecondAttemptID, prior, 3, epoch())
		if err != nil {
			t.Fatalf("NewRetryAttempt: unexpected error: %v", err)
		}
		if got.Number != 2 || got.TaskID != testTaskID || got.State != run.AttemptReserved {
			t.Fatalf("NewRetryAttempt = %+v, want number 2, task %s, reserved", got, testTaskID)
		}
	})

	t.Run("prior not terminal", func(t *testing.T) {
		for _, from := range attemptStates() {
			if from == run.AttemptCompleted || from == run.AttemptFailed || from == run.AttemptInterrupted {
				continue
			}
			prior := run.Attempt{ID: testAttemptID, TaskID: testTaskID, Number: 1, State: from}

			_, err := run.NewRetryAttempt(testSecondAttemptID, prior, 3, epoch())
			if !errors.Is(err, run.ErrRetryNotTerminal) {
				t.Fatalf("NewRetryAttempt with prior %s: error = %v, want ErrRetryNotTerminal", from, err)
			}
		}
	})

	t.Run("retry limit reached", func(t *testing.T) {
		prior := run.Attempt{ID: testAttemptID, TaskID: testTaskID, Number: 3, State: run.AttemptFailed}

		_, err := run.NewRetryAttempt(testSecondAttemptID, prior, 3, epoch())
		if !errors.Is(err, run.ErrRetryLimit) {
			t.Fatalf("NewRetryAttempt at the limit: error = %v, want ErrRetryLimit", err)
		}
	})
}
