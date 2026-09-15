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

// TestReleaseEligible exercises every bypass vector Terra's round-1 review
// named: missing, foreign and duplicate prerequisites, a foreign edge (one
// naming a different task entirely), and a non-integrated prerequisite —
// each must refuse, not vacuously pass through an incomplete or
// mismatched evidence set.
func TestReleaseEligible(t *testing.T) {
	task := run.Task{ID: testTaskID, RunID: testRunID, HasDependencies: true}
	edges := []run.TaskDependency{
		{TaskID: testTaskID, PrerequisiteID: testSecondTaskID},
		{TaskID: testTaskID, PrerequisiteID: testReviewTaskID},
	}

	t.Run("no dependencies is vacuously eligible", func(t *testing.T) {
		noDeps := run.Task{ID: testTaskID, RunID: testRunID, HasDependencies: false}
		if !run.ReleaseEligible(noDeps, nil, nil) {
			t.Fatal("ReleaseEligible(no dependencies) = false, want true")
		}
	})

	// Round-2 review's required vectors: edges must agree with
	// HasDependencies, or an empty edge set is vacuous ground truth for a
	// task that actually has real dependencies.

	t.Run("dependent task with zero edges is never vacuously eligible", func(t *testing.T) {
		if run.ReleaseEligible(task, nil, nil) {
			t.Fatal("ReleaseEligible(dependent, zero edges) = true, want false")
		}
	})

	t.Run("zero-dependency task with a non-empty edge set is refused", func(t *testing.T) {
		noDeps := run.Task{ID: testTaskID, RunID: testRunID, HasDependencies: false}
		prereqs := []run.Task{
			{ID: testSecondTaskID, State: run.TaskIntegrated},
			{ID: testReviewTaskID, State: run.TaskIntegrated},
		}
		if run.ReleaseEligible(noDeps, edges, prereqs) {
			t.Fatal("ReleaseEligible(zero-dependency, non-empty edges) = true, want false")
		}
	})

	t.Run("every named prerequisite integrated", func(t *testing.T) {
		prereqs := []run.Task{
			{ID: testSecondTaskID, State: run.TaskIntegrated},
			{ID: testReviewTaskID, State: run.TaskIntegrated},
		}
		if !run.ReleaseEligible(task, edges, prereqs) {
			t.Fatal("ReleaseEligible(all integrated) = false, want true")
		}
	})

	t.Run("one named prerequisite not integrated", func(t *testing.T) {
		prereqs := []run.Task{
			{ID: testSecondTaskID, State: run.TaskIntegrated},
			{ID: testReviewTaskID, State: run.TaskCompleted},
		}
		if run.ReleaseEligible(task, edges, prereqs) {
			t.Fatal("ReleaseEligible(one not integrated) = true, want false")
		}
	})

	t.Run("missing prerequisite: fewer than the edges name", func(t *testing.T) {
		prereqs := []run.Task{
			{ID: testSecondTaskID, State: run.TaskIntegrated},
		}
		if run.ReleaseEligible(task, edges, prereqs) {
			t.Fatal("ReleaseEligible(missing prerequisite) = true, want false")
		}
	})

	t.Run("empty prerequisites against a dependent task never vacuously passes", func(t *testing.T) {
		// The exact bypass the round-1 review found: an empty evidence
		// slice must never look like "nothing to check" when edges
		// name real prerequisites.
		if run.ReleaseEligible(task, edges, nil) {
			t.Fatal("ReleaseEligible(empty prerequisites, real edges) = true, want false")
		}
	})

	t.Run("foreign prerequisite: not among the named edges", func(t *testing.T) {
		prereqs := []run.Task{
			{ID: testSecondTaskID, State: run.TaskIntegrated},
			{ID: testFixTaskID, State: run.TaskIntegrated}, // never named by edges
		}
		if run.ReleaseEligible(task, edges, prereqs) {
			t.Fatal("ReleaseEligible(foreign prerequisite) = true, want false")
		}
	})

	t.Run("duplicate prerequisite masking a missing one", func(t *testing.T) {
		prereqs := []run.Task{
			{ID: testSecondTaskID, State: run.TaskIntegrated},
			{ID: testSecondTaskID, State: run.TaskIntegrated}, // duplicate, not testReviewTaskID
		}
		if run.ReleaseEligible(task, edges, prereqs) {
			t.Fatal("ReleaseEligible(duplicate prerequisite) = true, want false")
		}
	})

	t.Run("foreign edge: naming a different task is rejected outright", func(t *testing.T) {
		foreignEdges := []run.TaskDependency{
			{TaskID: testSecondTaskID, PrerequisiteID: testReviewTaskID}, // not task.ID
		}
		prereqs := []run.Task{{ID: testReviewTaskID, State: run.TaskIntegrated}}
		if run.ReleaseEligible(task, foreignEdges, prereqs) {
			t.Fatal("ReleaseEligible(foreign edge) = true, want false")
		}
	})
}
