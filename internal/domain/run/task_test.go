package run_test

import (
	"errors"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/domain/run"
)

// taskStates enumerates every Task state. It is a function, not a
// package-level slice, only to keep this shared fixture out of the
// global-variable set the project's lint configuration restricts.
func taskStates() []run.TaskState {
	return []run.TaskState{
		run.TaskPending, run.TaskReady, run.TaskActive, run.TaskChecking,
		run.TaskCompleted, run.TaskIntegrating, run.TaskIntegrated,
		run.TaskNeedsRework, run.TaskFailed, run.TaskInterrupted,
	}
}

// taskValidTransitions is section 5's Task (kind = implement) table,
// transcribed independently of the production transitionTable, for a task
// built with the zero-value Kind and HasDependencies == false — the Phase
// 2/solo legacy shape, which is exactly what every case below constructs.
// pending->active is included here (not exercised by TestTaskRelease):
// with no dependencies it is Activate's legacy path, not Release's.
func taskValidTransitions() map[[2]run.TaskState]bool {
	return map[[2]run.TaskState]bool{
		{run.TaskPending, run.TaskActive}:          true,
		{run.TaskReady, run.TaskActive}:            true,
		{run.TaskActive, run.TaskChecking}:         true,
		{run.TaskChecking, run.TaskCompleted}:      true,
		{run.TaskCompleted, run.TaskIntegrating}:   true,
		{run.TaskIntegrating, run.TaskIntegrated}:  true,
		{run.TaskIntegrating, run.TaskNeedsRework}: true,
		{run.TaskActive, run.TaskNeedsRework}:      true,
		{run.TaskChecking, run.TaskNeedsRework}:    true,
		{run.TaskNeedsRework, run.TaskReady}:       true,
		{run.TaskActive, run.TaskFailed}:           true,
		{run.TaskChecking, run.TaskFailed}:         true,
		{run.TaskIntegrating, run.TaskFailed}:      true,
		{run.TaskNeedsRework, run.TaskFailed}:      true,
		{run.TaskPending, run.TaskInterrupted}:     true,
		{run.TaskReady, run.TaskInterrupted}:       true,
		{run.TaskActive, run.TaskInterrupted}:      true,
		{run.TaskChecking, run.TaskInterrupted}:    true,
		{run.TaskCompleted, run.TaskInterrupted}:   true,
		{run.TaskIntegrating, run.TaskInterrupted}: true,
		{run.TaskNeedsRework, run.TaskInterrupted}: true,
	}
}

func TestTaskTransitions(t *testing.T) {
	steps := []struct {
		name string
		to   run.TaskState
		fn   func(run.Task, time.Time) (run.Task, error)
	}{
		{"Activate", run.TaskActive, run.Task.Activate},
		{"EnterChecking", run.TaskChecking, run.Task.EnterChecking},
		{"Complete", run.TaskCompleted, run.Task.Complete},
		{"EnterIntegrating", run.TaskIntegrating, run.Task.EnterIntegrating},
		{"Integrate", run.TaskIntegrated, run.Task.Integrate},
		{"NeedsRework", run.TaskNeedsRework, run.Task.NeedsRework},
		{"Reopen", run.TaskReady, run.Task.Reopen},
		{"Fail", run.TaskFailed, run.Task.Fail},
		{"Interrupt", run.TaskInterrupted, run.Task.Interrupt},
	}

	valid := taskValidTransitions()
	for _, step := range steps {
		for _, from := range taskStates() {
			t.Run(step.name+"/"+string(from)+"_to_"+string(step.to), func(t *testing.T) {
				task := run.Task{ID: testTaskID, RunID: testRunID, State: from}

				got, err := step.fn(task, epoch())

				if valid[[2]run.TaskState{from, step.to}] {
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

// reviewTaskValidTransitions is section 5's Task (kind = review) table,
// transcribed independently of the production transitionTable.
func reviewTaskValidTransitions() map[[2]run.TaskState]bool {
	return map[[2]run.TaskState]bool{
		{run.TaskReady, run.TaskActive}:            true,
		{run.TaskActive, run.TaskCompleted}:        true,
		{run.TaskActive, run.TaskNeedsRework}:      true,
		{run.TaskNeedsRework, run.TaskReady}:       true,
		{run.TaskActive, run.TaskFailed}:           true,
		{run.TaskNeedsRework, run.TaskFailed}:      true,
		{run.TaskReady, run.TaskInterrupted}:       true,
		{run.TaskActive, run.TaskInterrupted}:      true,
		{run.TaskNeedsRework, run.TaskInterrupted}: true,
	}
}

// TestReviewTaskTransitions is the review-kind counterpart of
// TestTaskTransitions: the same TaskState enum and the same shared
// transition functions, but every constructed task carries
// Kind == TaskKindReview, so it resolves against reviewTaskTransitions
// instead — proving the two kinds' tables are actually independent (for
// example, checking is never reachable for a review task, and pending is
// never a legal source at all, unlike the implement table's legacy path).
func TestReviewTaskTransitions(t *testing.T) {
	steps := []struct {
		name string
		to   run.TaskState
		fn   func(run.Task, time.Time) (run.Task, error)
	}{
		{"Activate", run.TaskActive, run.Task.Activate},
		{"Complete", run.TaskCompleted, run.Task.Complete},
		{"NeedsRework", run.TaskNeedsRework, run.Task.NeedsRework},
		{"Reopen", run.TaskReady, run.Task.Reopen},
		{"Fail", run.TaskFailed, run.Task.Fail},
		{"Interrupt", run.TaskInterrupted, run.Task.Interrupt},
	}

	valid := reviewTaskValidTransitions()
	for _, step := range steps {
		for _, from := range taskStates() {
			t.Run(step.name+"/"+string(from)+"_to_"+string(step.to), func(t *testing.T) {
				task := run.Task{ID: testTaskID, RunID: testRunID, Kind: run.TaskKindReview, State: from}

				got, err := step.fn(task, epoch())

				if valid[[2]run.TaskState{from, step.to}] {
					if err != nil {
						t.Fatalf("%s from %s: unexpected error: %v", step.name, from, err)
					}
					if got.State != step.to {
						t.Fatalf("%s from %s: State = %s, want %s", step.name, from, got.State, step.to)
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

// TestTaskActivateDependentRefusesDirectFromPending proves that a task WITH
// dependencies cannot activate directly from pending: it must Release to
// ready first. A zero-dependency task is exempt (TestTaskTransitions's
// pending->active case).
func TestTaskActivateDependentRefusesDirectFromPending(t *testing.T) {
	task := run.Task{ID: testTaskID, RunID: testRunID, Kind: run.TaskKindImplement, HasDependencies: true, State: run.TaskPending}

	got, err := task.Activate(epoch())

	if !errors.Is(err, run.ErrTaskNotReleased) {
		t.Fatalf("Activate a dependent pending task: error = %v, want ErrTaskNotReleased", err)
	}
	if got.State != run.TaskPending {
		t.Fatalf("Activate a dependent pending task: State = %s, want unchanged", got.State)
	}
}

// TestTaskRelease proves Release's own rule: only from pending, and only
// when the actual edge/prerequisite evidence establishes eligibility —
// never a bare caller assertion.
func TestTaskRelease(t *testing.T) {
	edges := []run.TaskDependency{{TaskID: testTaskID, PrerequisiteID: testSecondTaskID}}
	integratedPrereqs := []run.Task{{ID: testSecondTaskID, State: run.TaskIntegrated}}
	incompletePrereqs := []run.Task{{ID: testSecondTaskID, State: run.TaskCompleted}}

	t.Run("eligible", func(t *testing.T) {
		task := run.Task{ID: testTaskID, RunID: testRunID, Kind: run.TaskKindImplement, HasDependencies: true, State: run.TaskPending}

		got, err := task.Release(edges, integratedPrereqs, epoch())
		if err != nil {
			t.Fatalf("Release(eligible): unexpected error: %v", err)
		}
		if got.State != run.TaskReady {
			t.Fatalf("Release(eligible): State = %s, want ready", got.State)
		}
	})

	t.Run("not eligible", func(t *testing.T) {
		task := run.Task{ID: testTaskID, RunID: testRunID, Kind: run.TaskKindImplement, HasDependencies: true, State: run.TaskPending}

		got, err := task.Release(edges, incompletePrereqs, epoch())

		if !errors.Is(err, run.ErrDependencyNotIntegrated) {
			t.Fatalf("Release(not eligible): error = %v, want ErrDependencyNotIntegrated", err)
		}
		if got.State != run.TaskPending {
			t.Fatalf("Release(not eligible): State = %s, want unchanged", got.State)
		}
	})

	t.Run("empty prerequisites against real edges is refused, never a bare assertion", func(t *testing.T) {
		task := run.Task{ID: testTaskID, RunID: testRunID, Kind: run.TaskKindImplement, HasDependencies: true, State: run.TaskPending}

		got, err := task.Release(edges, nil, epoch())

		if !errors.Is(err, run.ErrDependencyNotIntegrated) {
			t.Fatalf("Release(empty prerequisites): error = %v, want ErrDependencyNotIntegrated", err)
		}
		if got.State != run.TaskPending {
			t.Fatalf("Release(empty prerequisites): State = %s, want unchanged", got.State)
		}
	})

	// The following two prove edges disagreeing with HasDependencies is
	// refused BEFORE ReleaseEligible ever runs: an empty edge set against
	// a dependent task must never look like vacuous ground truth.

	t.Run("dependent task with zero edges is refused", func(t *testing.T) {
		task := run.Task{ID: testTaskID, RunID: testRunID, Kind: run.TaskKindImplement, HasDependencies: true, State: run.TaskPending}

		got, err := task.Release(nil, nil, epoch())

		if !errors.Is(err, run.ErrDependencyEvidenceMissing) {
			t.Fatalf("Release(dependent, zero edges): error = %v, want ErrDependencyEvidenceMissing", err)
		}
		if got.State != run.TaskPending {
			t.Fatalf("Release(dependent, zero edges): State = %s, want unchanged", got.State)
		}
	})

	t.Run("zero-dependency task with a non-empty edge set is refused", func(t *testing.T) {
		// HasDependencies false but somehow parked in pending with real
		// edges supplied: the mismatch itself is refused, regardless of
		// how such a task came to exist.
		task := run.Task{ID: testTaskID, RunID: testRunID, Kind: run.TaskKindImplement, HasDependencies: false, State: run.TaskPending}

		got, err := task.Release(edges, integratedPrereqs, epoch())

		if !errors.Is(err, run.ErrDependencyEvidenceMissing) {
			t.Fatalf("Release(zero-dependency, non-empty edges): error = %v, want ErrDependencyEvidenceMissing", err)
		}
		if got.State != run.TaskPending {
			t.Fatalf("Release(zero-dependency, non-empty edges): State = %s, want unchanged", got.State)
		}
	})

	t.Run("wrong source state", func(t *testing.T) {
		task := run.Task{ID: testTaskID, RunID: testRunID, Kind: run.TaskKindImplement, State: run.TaskReady}

		got, err := task.Release(edges, integratedPrereqs, epoch())

		if !errors.Is(err, run.ErrInvalidTransition) {
			t.Fatalf("Release from ready: error = %v, want ErrInvalidTransition", err)
		}
		if got.State != run.TaskReady {
			t.Fatalf("Release from ready: State = %s, want unchanged", got.State)
		}
	})

	t.Run("review task refused regardless of eligibility", func(t *testing.T) {
		task := run.Task{ID: testTaskID, RunID: testRunID, Kind: run.TaskKindReview, State: run.TaskPending}

		got, err := task.Release(nil, nil, epoch())

		if !errors.Is(err, run.ErrInvalidTransition) {
			t.Fatalf("Release a review task: error = %v, want ErrInvalidTransition", err)
		}
		if got.State != run.TaskPending {
			t.Fatalf("Release a review task: State = %s, want unchanged", got.State)
		}
	})
}

func TestNewTask(t *testing.T) {
	task := run.NewTask(testTaskID, testRunID, "instructions-digest", epoch())

	if task.State != run.TaskPending {
		t.Fatalf("NewTask State = %s, want pending", task.State)
	}
	if task.ID != testTaskID || task.RunID != testRunID || task.InstructionsDigest != "instructions-digest" {
		t.Fatalf("NewTask did not preserve its inputs: %+v", task)
	}
	if !task.UpdatedAt.Equal(epoch()) {
		t.Fatalf("NewTask UpdatedAt = %v, want %v", task.UpdatedAt, epoch())
	}

	// A Phase 2 task, activated directly from pending: the legacy path
	// this constructor exists to preserve.
	activated, err := task.Activate(later())
	if err != nil {
		t.Fatalf("Activate a NewTask task: unexpected error: %v", err)
	}
	if activated.State != run.TaskActive {
		t.Fatalf("Activate a NewTask task: State = %s, want active", activated.State)
	}
}

func TestNewImplementTask(t *testing.T) {
	t.Run("no dependencies is created ready", func(t *testing.T) {
		task := run.NewImplementTask(testTaskID, testRunID, 1, "title", "instructions-digest", false, epoch())

		if task.State != run.TaskReady {
			t.Fatalf("NewImplementTask(hasDependencies=false) State = %s, want ready", task.State)
		}
		if task.Kind != run.TaskKindImplement || task.Seq != 1 || task.Title != "title" || task.HasDependencies {
			t.Fatalf("NewImplementTask did not preserve its inputs: %+v", task)
		}
	})

	t.Run("with dependencies is created pending", func(t *testing.T) {
		task := run.NewImplementTask(testTaskID, testRunID, 2, "title", "instructions-digest", true, epoch())

		if task.State != run.TaskPending {
			t.Fatalf("NewImplementTask(hasDependencies=true) State = %s, want pending", task.State)
		}
		if !task.HasDependencies {
			t.Fatal("NewImplementTask(hasDependencies=true) HasDependencies = false, want true")
		}
	})
}

func TestNewReviewTask(t *testing.T) {
	task := run.NewReviewTask(testTaskID, testRunID, 5, "commit-oid", "tree-oid", epoch())

	if task.State != run.TaskReady {
		t.Fatalf("NewReviewTask State = %s, want ready", task.State)
	}
	if task.Kind != run.TaskKindReview || task.Seq != 5 || task.SubjectCommitOID != "commit-oid" || task.SubjectTreeOID != "tree-oid" {
		t.Fatalf("NewReviewTask did not preserve its inputs: %+v", task)
	}
}

// TestTaskMailbox proves the mailbox flag's basic open/close/reopen shape.
func TestTaskMailbox(t *testing.T) {
	task := run.NewImplementTask(testTaskID, testRunID, 1, "title", "digest", false, epoch())
	if task.MailboxClosed {
		t.Fatal("NewImplementTask MailboxClosed = true, want false")
	}

	closed := task.CloseMailbox(later())
	if !closed.MailboxClosed {
		t.Fatal("CloseMailbox: MailboxClosed = false, want true")
	}

	reopened := closed.ReopenMailbox(later())
	if reopened.MailboxClosed {
		t.Fatal("ReopenMailbox: MailboxClosed = true, want false")
	}
}
