package run_test

import (
	"errors"
	"testing"

	"github.com/johnlanda/hop/internal/domain/run"
)

func TestNewTaskDependency(t *testing.T) {
	t.Run("valid edge", func(t *testing.T) {
		dep, err := run.NewTaskDependency(testTaskID, testSecondTaskID, epoch())
		if err != nil {
			t.Fatalf("NewTaskDependency: unexpected error: %v", err)
		}
		if dep.TaskID != testTaskID || dep.PrerequisiteID != testSecondTaskID || !dep.CreatedAt.Equal(epoch()) {
			t.Fatalf("NewTaskDependency did not preserve its inputs: %+v", dep)
		}
	})

	t.Run("self dependency", func(t *testing.T) {
		_, err := run.NewTaskDependency(testTaskID, testTaskID, epoch())
		if !errors.Is(err, run.ErrDependencyCycle) {
			t.Fatalf("NewTaskDependency(self): error = %v, want ErrDependencyCycle", err)
		}
	})
}

func TestValidateAcyclic(t *testing.T) {
	t.Run("no existing edges", func(t *testing.T) {
		candidate, err := run.NewTaskDependency(testTaskID, testSecondTaskID, epoch())
		if err != nil {
			t.Fatalf("NewTaskDependency: %v", err)
		}
		if err := run.ValidateAcyclic(nil, candidate); err != nil {
			t.Fatalf("ValidateAcyclic(no existing edges): unexpected error: %v", err)
		}
	})

	t.Run("direct cycle", func(t *testing.T) {
		// testTaskID already depends on testSecondTaskID; adding the
		// reverse edge would close a two-node cycle.
		existing := []run.TaskDependency{{TaskID: testTaskID, PrerequisiteID: testSecondTaskID}}
		candidate := run.TaskDependency{TaskID: testSecondTaskID, PrerequisiteID: testTaskID}

		if err := run.ValidateAcyclic(existing, candidate); !errors.Is(err, run.ErrDependencyCycle) {
			t.Fatalf("ValidateAcyclic(direct cycle): error = %v, want ErrDependencyCycle", err)
		}
	})

	t.Run("transitive cycle", func(t *testing.T) {
		// A depends on B, B depends on C; adding C -> A would close a
		// three-node cycle.
		a, b, c := testTaskID, testSecondTaskID, testReviewTaskID
		existing := []run.TaskDependency{
			{TaskID: a, PrerequisiteID: b},
			{TaskID: b, PrerequisiteID: c},
		}
		candidate := run.TaskDependency{TaskID: c, PrerequisiteID: a}

		if err := run.ValidateAcyclic(existing, candidate); !errors.Is(err, run.ErrDependencyCycle) {
			t.Fatalf("ValidateAcyclic(transitive cycle): error = %v, want ErrDependencyCycle", err)
		}
	})

	t.Run("no cycle among unrelated edges", func(t *testing.T) {
		a, b, c := testTaskID, testSecondTaskID, testReviewTaskID
		existing := []run.TaskDependency{
			{TaskID: b, PrerequisiteID: c},
		}
		candidate := run.TaskDependency{TaskID: a, PrerequisiteID: b}

		if err := run.ValidateAcyclic(existing, candidate); err != nil {
			t.Fatalf("ValidateAcyclic(no cycle): unexpected error: %v", err)
		}
	})
}

func TestReleaseEligible(t *testing.T) {
	t.Run("no dependencies is vacuously eligible", func(t *testing.T) {
		task := run.Task{ID: testTaskID, RunID: testRunID, HasDependencies: false}
		if !run.ReleaseEligible(task, nil) {
			t.Fatal("ReleaseEligible(no dependencies) = false, want true")
		}
	})

	t.Run("every prerequisite integrated", func(t *testing.T) {
		task := run.Task{ID: testTaskID, RunID: testRunID, HasDependencies: true}
		prereqs := []run.Task{
			{ID: testSecondTaskID, State: run.TaskIntegrated},
			{ID: testReviewTaskID, State: run.TaskIntegrated},
		}
		if !run.ReleaseEligible(task, prereqs) {
			t.Fatal("ReleaseEligible(all integrated) = false, want true")
		}
	})

	t.Run("one prerequisite not integrated", func(t *testing.T) {
		task := run.Task{ID: testTaskID, RunID: testRunID, HasDependencies: true}
		prereqs := []run.Task{
			{ID: testSecondTaskID, State: run.TaskIntegrated},
			{ID: testReviewTaskID, State: run.TaskCompleted},
		}
		if run.ReleaseEligible(task, prereqs) {
			t.Fatal("ReleaseEligible(one not integrated) = true, want false")
		}
	})
}
