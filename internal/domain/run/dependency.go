package run

import (
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// TaskDependency is an edge from a task to a same-run prerequisite it
// depends on. Immutable once created: a task's dependency set is fixed at
// creation.
type TaskDependency struct {
	TaskID         identity.TaskID
	PrerequisiteID identity.TaskID
	CreatedAt      time.Time
}

// NewTaskDependency constructs one dependency edge. A task depending on
// itself is never valid (a single-node cycle); ValidateAcyclic also
// rejects it, but the constructor fails closed immediately since no graph
// context is even needed to see it.
func NewTaskDependency(taskID, prerequisiteID identity.TaskID, now time.Time) (TaskDependency, error) {
	if taskID == prerequisiteID {
		return TaskDependency{}, fmt.Errorf("%w: task %s: cannot depend on itself", ErrDependencyCycle, taskID)
	}
	return TaskDependency{TaskID: taskID, PrerequisiteID: prerequisiteID, CreatedAt: now}, nil
}

// ValidateAcyclic reports whether adding candidate to existing (the run's
// currently persisted edge set) keeps the dependency graph acyclic.
// ErrDependencyCycle otherwise. The application calls this against the
// persisted edge set inside the same transaction that creates candidate's
// task and its edges, per section 5's acyclicity validation rule.
func ValidateAcyclic(existing []TaskDependency, candidate TaskDependency) error {
	// A path exists from candidate.PrerequisiteID back to candidate.TaskID
	// through existing edges exactly when adding candidate would close a
	// cycle: candidate itself supplies the closing edge
	// (TaskID -> PrerequisiteID), so a cycle exists iff the prerequisite
	// can already reach the task without it.
	adjacency := make(map[identity.TaskID][]identity.TaskID, len(existing))
	for _, e := range existing {
		adjacency[e.TaskID] = append(adjacency[e.TaskID], e.PrerequisiteID)
	}

	visited := make(map[identity.TaskID]bool, len(existing))
	var stack []identity.TaskID
	stack = append(stack, candidate.PrerequisiteID)
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if node == candidate.TaskID {
			return fmt.Errorf("%w: task %s: prerequisite %s already depends on it (directly or transitively)", ErrDependencyCycle, candidate.TaskID, candidate.PrerequisiteID)
		}
		if visited[node] {
			continue
		}
		visited[node] = true
		stack = append(stack, adjacency[node]...)
	}
	return nil
}

// ReleaseEligible reports whether task is eligible to move pending->ready:
// every one of prerequisites has reached integrated. A task with no
// dependencies is vacuously eligible (it is created ready directly and
// never actually calls Release, but the predicate stays true for it by
// definition).
func ReleaseEligible(task Task, prerequisites []Task) bool { //nolint:gocritic // hugeParam: Task is passed by value everywhere in this package; this predicate mirrors that convention.
	if !task.HasDependencies {
		return true
	}
	for _, p := range prerequisites {
		if p.State != TaskIntegrated {
			return false
		}
	}
	return true
}
