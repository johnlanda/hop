package app_test

import (
	"context"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// mustTaskID parses s as a TaskID or fails the test.
func mustTaskID(t *testing.T, s string) identity.TaskID {
	t.Helper()
	id, err := identity.ParseTaskID(s)
	if err != nil {
		t.Fatalf("parse task id %q: %v", s, err)
	}
	return id
}

func defaultCreateTaskRequest(fr featureRun, title string, dependsOn ...string) app.CreateTaskRequest { //nolint:gocritic // hugeParam: featureRun is a small test fixture value passed once per call, never a hot loop.
	return app.CreateTaskRequest{
		RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
		StateRoot: "/state", Title: title, InstructionsBody: []byte("do the thing"), DependsOn: dependsOn,
	}
}

func TestCreateTask(t *testing.T) {
	t.Run("a manager creates a zero-dependency task", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)

		result, err := tc.Controller.CreateTask(context.Background(), defaultCreateTaskRequest(fr, "A"))
		if err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if result.Outcome != string(app.WorkflowAccepted) || result.Seq != 1 {
			t.Fatalf("CreateTask() = %+v, want accepted seq 1", result)
		}
		taskID := mustTaskID(t, result.TaskID)
		if got := tc.Store.Tasks[taskID].value.State; got != run.TaskReady {
			t.Fatalf("task state = %s, want ready", got)
		}
		if len(tc.Artifacts.files) != 1 {
			t.Fatalf("instructions artifact was not written: %d files", len(tc.Artifacts.files))
		}
	})

	t.Run("chained dependencies are accepted and a task cannot depend on itself", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)

		a, err := tc.Controller.CreateTask(context.Background(), defaultCreateTaskRequest(fr, "A"))
		if err != nil || a.Outcome != string(app.WorkflowAccepted) {
			t.Fatalf("CreateTask(A) = %+v, err = %v", a, err)
		}
		// A task's dependency set is fixed at creation and may only name
		// already-existing tasks, so a genuine multi-task cycle cannot be
		// constructed through CreateTask alone (each new node can only
		// point backwards in creation order) — the one cycle CreateTask
		// can ever attempt is direct self-reference, which
		// NewTaskDependency refuses before ValidateAcyclic is even
		// reached.
		b, err := tc.Controller.CreateTask(context.Background(), defaultCreateTaskRequest(fr, "B", a.TaskID))
		if err != nil || b.Outcome != string(app.WorkflowAccepted) {
			t.Fatalf("CreateTask(B) depending on A = %+v, err = %v", b, err)
		}

		selfID := mustTaskID(t, b.TaskID)
		if _, err := run.NewTaskDependency(selfID, selfID, tc.Clock.Now()); err == nil {
			t.Fatalf("NewTaskDependency(self, self) succeeded, want ErrDependencyCycle")
		}
	})

	t.Run("a dependency in another run is refused", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		other := seedFeatureRun(t, tc, 2)
		foreignTask := seedImplementTask(t, tc, other.RunID, 1, "foreign", false, run.TaskReady)

		result, err := tc.Controller.CreateTask(context.Background(), defaultCreateTaskRequest(fr, "A", foreignTask.String()))
		if err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if result.Outcome != string(app.WorkflowRefused) {
			t.Fatalf("CreateTask() with a cross-run dependency = %+v, want refused", result)
		}
	})

	t.Run("a non-manager caller is refused", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskB := seedImplementTask(t, tc, fr.RunID, 1, "B", false, run.TaskReady)
		workerID, workerIncarnation := seedWorkerSession(t, tc, fr, taskB)

		req := defaultCreateTaskRequest(fr, "C")
		req.SessionID = workerID.String()
		req.IncarnationID = workerIncarnation.String()
		result, err := tc.Controller.CreateTask(context.Background(), req)
		if err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if result.Outcome != string(app.WorkflowRefused) {
			t.Fatalf("CreateTask() from a worker session = %+v, want refused", result)
		}
	})

	t.Run("an identical request-ID retry returns the original acceptance", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		req := defaultCreateTaskRequest(fr, "A")
		req.RequestID = "task-a-1"

		first, err := tc.Controller.CreateTask(context.Background(), req)
		if err != nil || first.Outcome != string(app.WorkflowAccepted) {
			t.Fatalf("CreateTask() first = %+v, err = %v", first, err)
		}
		second, err := tc.Controller.CreateTask(context.Background(), req)
		if err != nil {
			t.Fatalf("CreateTask() retry error = %v", err)
		}
		if second.Outcome != string(app.WorkflowDuplicate) || second.TaskID != first.TaskID || second.Seq != first.Seq {
			t.Fatalf("CreateTask() retry = %+v, want duplicate of %+v", second, first)
		}
	})

	t.Run("an oversized title is malformed before any side effect", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		req := defaultCreateTaskRequest(fr, string(make([]byte, app.TaskTitleLimit+1)))

		result, err := tc.Controller.CreateTask(context.Background(), req)
		if err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if result.Outcome != string(app.WorkflowMalformed) {
			t.Fatalf("CreateTask() = %+v, want malformed", result)
		}
		if len(tc.Artifacts.files) != 0 {
			t.Fatalf("a malformed title must not write an instructions artifact")
		}
	})

	t.Run("CreateTask without a feature-mode port fails closed", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		solo := *tc.Controller
		solo.Plan = nil
		if _, err := solo.CreateTask(context.Background(), app.CreateTaskRequest{}); err == nil {
			t.Fatalf("CreateTask() over a solo Controller succeeded; want ErrFeatureModeUnsupported")
		}
	})
}

func TestClosePlanReopenOnCreate(t *testing.T) {
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)

	created, err := tc.Controller.CreateTask(context.Background(), defaultCreateTaskRequest(fr, "A"))
	if err != nil || created.Outcome != string(app.WorkflowAccepted) {
		t.Fatalf("CreateTask() = %+v, err = %v", created, err)
	}

	closeResult, err := tc.Controller.ClosePlan(context.Background(), app.ClosePlanRequest{
		RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
	})
	if err != nil {
		t.Fatalf("ClosePlan() error = %v", err)
	}
	if closeResult.Outcome != string(app.WorkflowAccepted) {
		t.Fatalf("ClosePlan() = %+v, want accepted", closeResult)
	}
	if !tc.Store.Runs[fr.RunID].value.PlanClosed {
		t.Fatalf("run.PlanClosed = false after ClosePlan, want true")
	}

	// An accepted CreateTask reopens the plan in the same transaction.
	second, err := tc.Controller.CreateTask(context.Background(), defaultCreateTaskRequest(fr, "B"))
	if err != nil || second.Outcome != string(app.WorkflowAccepted) {
		t.Fatalf("CreateTask(B) = %+v, err = %v", second, err)
	}
	if tc.Store.Runs[fr.RunID].value.PlanClosed {
		t.Fatalf("run.PlanClosed = true after an accepted CreateTask, want false (reopened)")
	}
}

func TestClosePlanRefusesEmptyPlan(t *testing.T) {
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)

	result, err := tc.Controller.ClosePlan(context.Background(), app.ClosePlanRequest{
		RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
	})
	if err != nil {
		t.Fatalf("ClosePlan() error = %v", err)
	}
	if result.Outcome != string(app.WorkflowRefused) {
		t.Fatalf("ClosePlan() over an empty plan = %+v, want refused", result)
	}
}

func TestRequestRetry(t *testing.T) {
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	taskB := seedImplementTask(t, tc, fr.RunID, 1, "B", false, run.TaskReady)
	workerID, _ := seedWorkerSession(t, tc, fr, taskB)

	// Establish a terminal first attempt: interrupt the launched one.
	attemptID := tc.Store.Sessions[workerID].value.AttemptID
	attempt := tc.Store.Attempts[attemptID].value
	launched, err := attempt.MarkRunning(tc.Clock.Now())
	if err != nil {
		t.Fatalf("MarkRunning() error = %v", err)
	}
	interrupted, err := launched.Interrupt(tc.Clock.Now())
	if err != nil {
		t.Fatalf("Interrupt() error = %v", err)
	}
	tc.Store.Attempts[attemptID].value = interrupted
	needsRework, err := tc.Store.Tasks[taskB].value.NeedsRework(tc.Clock.Now())
	if err != nil {
		t.Fatalf("NeedsRework() error = %v", err)
	}
	tc.Store.Tasks[taskB].value = needsRework

	result, err := tc.Controller.RequestRetry(context.Background(), app.RequestRetryRequest{
		RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
		TaskID: taskB.String(), Reason: "flaky", RequestID: "retry-b-1",
	})
	if err != nil {
		t.Fatalf("RequestRetry() error = %v", err)
	}
	if result.Outcome != string(app.WorkflowAccepted) || result.AttemptNumber != 2 {
		t.Fatalf("RequestRetry() = %+v, want accepted attempt 2", result)
	}
	if got := tc.Store.Tasks[taskB].value.State; got != run.TaskReady {
		t.Fatalf("task state after retry = %s, want ready", got)
	}

	// An identical retry returns the original acceptance.
	again, err := tc.Controller.RequestRetry(context.Background(), app.RequestRetryRequest{
		RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
		TaskID: taskB.String(), Reason: "flaky", RequestID: "retry-b-1",
	})
	if err != nil {
		t.Fatalf("RequestRetry() retry error = %v", err)
	}
	if again.Outcome != string(app.WorkflowDuplicate) || again.AttemptNumber != 2 {
		t.Fatalf("RequestRetry() retry = %+v, want duplicate attempt 2", again)
	}
}
