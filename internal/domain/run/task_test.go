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
		run.TaskPending, run.TaskActive, run.TaskChecking,
		run.TaskCompleted, run.TaskFailed, run.TaskInterrupted,
	}
}

// taskValidTransitions is section 5's Task table, transcribed independently
// of the production transitionTable.
func taskValidTransitions() map[[2]run.TaskState]bool {
	return map[[2]run.TaskState]bool{
		{run.TaskPending, run.TaskActive}:       true,
		{run.TaskActive, run.TaskChecking}:      true,
		{run.TaskChecking, run.TaskCompleted}:   true,
		{run.TaskActive, run.TaskFailed}:        true,
		{run.TaskChecking, run.TaskFailed}:      true,
		{run.TaskPending, run.TaskInterrupted}:  true,
		{run.TaskActive, run.TaskInterrupted}:   true,
		{run.TaskChecking, run.TaskInterrupted}: true,
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
}
