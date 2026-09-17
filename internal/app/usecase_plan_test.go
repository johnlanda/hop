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
		if result.Outcome != string(app.WorkflowRefused) || result.Reason != app.GrammarReasonDependencyCycle {
			t.Fatalf("CreateTask() with a cross-run dependency = %+v, want refused/%s", result, app.GrammarReasonDependencyCycle)
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
		if result.Outcome != string(app.WorkflowRefused) || result.Reason != app.GrammarReasonNotManager {
			t.Fatalf("CreateTask() from a worker session = %+v, want refused/%s", result, app.GrammarReasonNotManager)
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
		if result.Outcome != string(app.WorkflowMalformed) || result.Reason != app.GrammarReasonMalformed {
			t.Fatalf("CreateTask() = %+v, want malformed/%s", result, app.GrammarReasonMalformed)
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
	if result.Outcome != string(app.WorkflowRefused) || result.Reason != app.GrammarReasonEmptyPlan {
		t.Fatalf("ClosePlan() over an empty plan = %+v, want refused/%s", result, app.GrammarReasonEmptyPlan)
	}
}

// seedNeedsReworkTask seeds an implement task whose launched first
// attempt was interrupted and which is now needs-rework: the one shape
// RequestRetry accepts.
func seedNeedsReworkTask(t *testing.T, tc *testController, fr featureRun, seq int, title string) identity.TaskID { //nolint:gocritic // hugeParam: featureRun is a small test fixture value passed once per call, never a hot loop.
	t.Helper()
	taskID := seedImplementTask(t, tc, fr.RunID, seq, title, false, run.TaskReady)
	workerID, _ := seedWorkerSession(t, tc, fr, taskID)

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
	needsRework, err := tc.Store.Tasks[taskID].value.NeedsRework(tc.Clock.Now())
	if err != nil {
		t.Fatalf("NeedsRework() error = %v", err)
	}
	tc.Store.Tasks[taskID].value = needsRework
	return taskID
}

// TestPlanVerbsTransientUntilRunning proves the retryable half of the
// run-acceptance split through the driving Controller: while the run is
// launching, resuming or completing, every plan verb from the validated
// manager is transient (empty reason, the state-only detail, nothing
// changed), and the SAME request retried once the run is running is
// accepted, then reported duplicate.
func TestPlanVerbsTransientUntilRunning(t *testing.T) {
	for _, state := range []run.RunState{run.RunLaunching, run.RunResuming, run.RunCompleting} {
		t.Run(string(state), func(t *testing.T) {
			tc := newTestController(defaultPolicy())
			fr := seedFeatureRun(t, tc, 2)
			taskB := seedNeedsReworkTask(t, tc, fr, 1, "B")
			rRow := tc.Store.Runs[fr.RunID]
			rRow.value.State = state
			revision := rRow.revision
			tasksBefore, attemptsBefore := len(tc.Store.Tasks), len(tc.Store.Attempts)

			create := defaultCreateTaskRequest(fr, "A")
			create.RequestID = "create-a"
			retry := app.RequestRetryRequest{
				RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
				TaskID: taskB.String(), Reason: "flaky", RequestID: "retry-b",
			}
			closePlan := app.ClosePlanRequest{
				RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
				RequestID: "close",
			}
			wantDetail := "run is " + string(state) + ", not yet running"

			createResult, err := tc.Controller.CreateTask(context.Background(), create)
			if err != nil || createResult.Outcome != string(app.WorkflowTransient) || createResult.Reason != "" || createResult.Detail != wantDetail || createResult.TaskID != "" {
				t.Fatalf("CreateTask() = %+v, %v; want transient with detail %q", createResult, err, wantDetail)
			}
			retryResult, err := tc.Controller.RequestRetry(context.Background(), retry)
			if err != nil || retryResult.Outcome != string(app.WorkflowTransient) || retryResult.Reason != "" || retryResult.Detail != wantDetail || retryResult.AttemptNumber != 0 {
				t.Fatalf("RequestRetry() = %+v, %v; want transient with detail %q", retryResult, err, wantDetail)
			}
			closeResult, err := tc.Controller.ClosePlan(context.Background(), closePlan)
			if err != nil || closeResult.Outcome != string(app.WorkflowTransient) || closeResult.Reason != "" || closeResult.Detail != wantDetail {
				t.Fatalf("ClosePlan() = %+v, %v; want transient with detail %q", closeResult, err, wantDetail)
			}
			if len(tc.Store.Tasks) != tasksBefore || len(tc.Store.Attempts) != attemptsBefore {
				t.Fatalf("tasks/attempts = %d/%d, want unchanged %d/%d", len(tc.Store.Tasks), len(tc.Store.Attempts), tasksBefore, attemptsBefore)
			}
			if rRow.revision != revision || rRow.value.PlanClosed || rRow.value.State != state {
				t.Fatalf("run row = %+v (revision %d), want unchanged at revision %d", rRow.value, rRow.revision, revision)
			}
			if got := tc.Store.Tasks[taskB].value.State; got != run.TaskNeedsRework {
				t.Fatalf("retried task state = %s, want unchanged needs-rework", got)
			}

			rRow.value.State = run.RunRunning
			var created app.CreateTaskResult
			for _, pass := range []app.WorkflowOutcomeKind{app.WorkflowAccepted, app.WorkflowDuplicate} {
				createResult, err = tc.Controller.CreateTask(context.Background(), create)
				if err != nil || createResult.Outcome != string(pass) || createResult.TaskID == "" {
					t.Fatalf("CreateTask() once running = %+v, %v; want %s", createResult, err, pass)
				}
				if pass == app.WorkflowAccepted {
					created = createResult
				} else if createResult.TaskID != created.TaskID || createResult.Seq != created.Seq {
					t.Fatalf("CreateTask() duplicate = %+v, want the accepted task %s t%d", createResult, created.TaskID, created.Seq)
				}
				retryResult, err = tc.Controller.RequestRetry(context.Background(), retry)
				if err != nil || retryResult.Outcome != string(pass) || retryResult.AttemptNumber != 2 {
					t.Fatalf("RequestRetry() once running = %+v, %v; want %s attempt 2", retryResult, err, pass)
				}
				closeResult, err = tc.Controller.ClosePlan(context.Background(), closePlan)
				if err != nil || closeResult.Outcome != string(pass) {
					t.Fatalf("ClosePlan() once running = %+v, %v; want %s", closeResult, err, pass)
				}
			}
		})
	}

	t.Run("a resuming run with a stop request refuses", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		rRow := tc.Store.Runs[fr.RunID]
		rRow.value = rRow.value.RequestStop(tc.Clock.Now())
		resuming, err := rRow.value.EnterResuming(tc.Clock.Now())
		if err != nil {
			t.Fatalf("EnterResuming() error = %v", err)
		}
		rRow.value = resuming

		result, err := tc.Controller.CreateTask(context.Background(), defaultCreateTaskRequest(fr, "A"))
		if err != nil || result.Outcome != string(app.WorkflowRefused) || result.Reason != app.GrammarReasonRunNotAccepting {
			t.Fatalf("CreateTask() over a stop-requested resuming run = %+v, %v; want refused/%s", result, err, app.GrammarReasonRunNotAccepting)
		}
	})
}

func TestRequestRetry(t *testing.T) {
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	taskB := seedNeedsReworkTask(t, tc, fr, 1, "B")

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

// TestPlanRefusalReasonsAlwaysSet drives every remaining
// CreateTask/RequestRetry/ClosePlan refusal shape not already asserted
// above (ruling B: a refused/malformed outcome with an empty Reason is a
// defect) and checks the exact grammar.go token each one sets — through
// the driving Controller, exactly as cmd/hop renders it.
func TestPlanRefusalReasonsAlwaysSet(t *testing.T) {
	t.Run("a stale incarnation is refused", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		stale, err := identity.ParseIncarnationID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse incarnation id: %v", err)
		}
		req := defaultCreateTaskRequest(fr, "A")
		req.IncarnationID = stale.String()

		result, err := tc.Controller.CreateTask(context.Background(), req)
		if err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if result.Outcome != string(app.WorkflowRefused) || result.Reason != app.GrammarReasonStale {
			t.Fatalf("CreateTask() with a stale incarnation = %+v, want refused/%s", result, app.GrammarReasonStale)
		}
	})

	t.Run("a run that can never accept refuses every plan verb", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		rRow := tc.Store.Runs[fr.RunID]
		completing, err := rRow.value.EnterCompleting(tc.Clock.Now())
		if err != nil {
			t.Fatalf("EnterCompleting() error = %v", err)
		}
		completed, err := completing.Complete(tc.Clock.Now())
		if err != nil {
			t.Fatalf("Complete() error = %v", err)
		}
		rRow.value = completed

		createResult, err := tc.Controller.CreateTask(context.Background(), defaultCreateTaskRequest(fr, "A"))
		if err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if createResult.Outcome != string(app.WorkflowRefused) || createResult.Reason != app.GrammarReasonRunNotAccepting {
			t.Fatalf("CreateTask() over a non-running run = %+v, want refused/%s", createResult, app.GrammarReasonRunNotAccepting)
		}

		retryResult, err := tc.Controller.RequestRetry(context.Background(), app.RequestRetryRequest{
			RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
			TaskID: mustTaskID(t, tc.IDs.NewID()).String(), Reason: "flaky",
		})
		if err != nil {
			t.Fatalf("RequestRetry() error = %v", err)
		}
		if retryResult.Outcome != string(app.WorkflowRefused) || retryResult.Reason != app.GrammarReasonRunNotAccepting {
			t.Fatalf("RequestRetry() over a non-running run = %+v, want refused/%s", retryResult, app.GrammarReasonRunNotAccepting)
		}

		closeResult, err := tc.Controller.ClosePlan(context.Background(), app.ClosePlanRequest{
			RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
		})
		if err != nil {
			t.Fatalf("ClosePlan() error = %v", err)
		}
		if closeResult.Outcome != string(app.WorkflowRefused) || closeResult.Reason != app.GrammarReasonRunNotAccepting {
			t.Fatalf("ClosePlan() over a non-running run = %+v, want refused/%s", closeResult, app.GrammarReasonRunNotAccepting)
		}
	})

	t.Run("retrying a task that is not needs-rework is refused", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskA := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)

		result, err := tc.Controller.RequestRetry(context.Background(), app.RequestRetryRequest{
			RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
			TaskID: taskA.String(), Reason: "flaky",
		})
		if err != nil {
			t.Fatalf("RequestRetry() error = %v", err)
		}
		if result.Outcome != string(app.WorkflowRefused) || result.Reason != app.GrammarReasonRetryNotTerminal {
			t.Fatalf("RequestRetry() of a ready task = %+v, want refused/%s", result, app.GrammarReasonRetryNotTerminal)
		}
	})

	t.Run("a retry against a task at its frozen limit is refused", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskB := seedImplementTask(t, tc, fr.RunID, 1, "B", false, run.TaskReady)
		workerID, _ := seedWorkerSession(t, tc, fr, taskB)

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
		// The snapshot's frozen retry limit is 3 (seedFeatureRun): pinning
		// this terminal attempt's own number AT the limit is the cheapest
		// way to reach NewRetryAttempt's ErrRetryLimit branch directly,
		// without driving three real retry cycles.
		interrupted.Number = 3
		tc.Store.Attempts[attemptID].value = interrupted
		needsRework, err := tc.Store.Tasks[taskB].value.NeedsRework(tc.Clock.Now())
		if err != nil {
			t.Fatalf("NeedsRework() error = %v", err)
		}
		tc.Store.Tasks[taskB].value = needsRework

		result, err := tc.Controller.RequestRetry(context.Background(), app.RequestRetryRequest{
			RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
			TaskID: taskB.String(), Reason: "flaky",
		})
		if err != nil {
			t.Fatalf("RequestRetry() error = %v", err)
		}
		if result.Outcome != string(app.WorkflowRefused) || result.Reason != app.GrammarReasonRetryLimit {
			t.Fatalf("RequestRetry() at the retry limit = %+v, want refused/%s", result, app.GrammarReasonRetryLimit)
		}
	})

	t.Run("a retry request while one is already pending is refused", func(t *testing.T) {
		// This shape (a task still needs-rework with an UNCONSUMED pending
		// retry_requests row already recorded for it) is not reachable
		// through two ordinary RequestRetry calls: acceptance itself moves
		// the task out of needs-rework in the same transaction that records
		// the pending row, so a second ordinary call hits "task is not
		// needs-rework" first. The check exists as defense in depth (the
		// bookkeeping row and the task's own state could disagree after a
		// future change); seeded directly, mirroring
		// TestStoreVectors/AckMessageCrossRun's already-corrupt-row pattern.
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskB := seedImplementTask(t, tc, fr.RunID, 1, "B", false, run.TaskReady)
		workerID, _ := seedWorkerSession(t, tc, fr, taskB)

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
		tc.Store.RetryRequestStates[taskB] = app.RetryRequestPending

		result, err := tc.Controller.RequestRetry(context.Background(), app.RequestRetryRequest{
			RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
			TaskID: taskB.String(), Reason: "flaky",
		})
		if err != nil {
			t.Fatalf("RequestRetry() error = %v", err)
		}
		if result.Outcome != string(app.WorkflowRefused) || result.Reason != app.GrammarReasonConflicting {
			t.Fatalf("RequestRetry() with a pending retry already reserved = %+v, want refused/%s", result, app.GrammarReasonConflicting)
		}
	})

	t.Run("a conflicting request id is refused for retry and close", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskB := seedImplementTask(t, tc, fr.RunID, 1, "B", false, run.TaskReady)
		workerID, _ := seedWorkerSession(t, tc, fr, taskB)
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

		firstRetry, err := tc.Controller.RequestRetry(context.Background(), app.RequestRetryRequest{
			RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
			TaskID: taskB.String(), Reason: "flaky", RequestID: "shared-retry-id",
		})
		if err != nil || firstRetry.Outcome != string(app.WorkflowAccepted) {
			t.Fatalf("RequestRetry(first) = %+v, err = %v", firstRetry, err)
		}
		conflictingRetry, err := tc.Controller.RequestRetry(context.Background(), app.RequestRetryRequest{
			RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
			TaskID: taskB.String(), Reason: "a different reason entirely", RequestID: "shared-retry-id",
		})
		if err != nil {
			t.Fatalf("RequestRetry(conflicting) error = %v", err)
		}
		if conflictingRetry.Outcome != string(app.WorkflowRefused) || conflictingRetry.Reason != app.GrammarReasonConflicting {
			t.Fatalf("RequestRetry() with a reused, conflicting request id = %+v, want refused/%s", conflictingRetry, app.GrammarReasonConflicting)
		}

		firstClose, err := tc.Controller.ClosePlan(context.Background(), app.ClosePlanRequest{
			RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(), RequestID: "shared-close-id",
		})
		if err != nil {
			t.Fatalf("ClosePlan(first) error = %v", err)
		}
		// The plan still has zero implement tasks in run.ClosePlan's own
		// terms (taskB is needs-rework, never an accepted implement task
		// count check — ClosePlan only checks task KIND) — seedImplementTask
		// already inserted an implement-kind row, so this closes cleanly.
		if firstClose.Outcome != string(app.WorkflowAccepted) {
			t.Fatalf("ClosePlan(first) = %+v, want accepted", firstClose)
		}
		conflictingClose, err := tc.Controller.ClosePlan(context.Background(), app.ClosePlanRequest{
			RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(), RequestID: "shared-close-id-2",
		})
		if err != nil {
			t.Fatalf("ClosePlan(second, different id) error = %v", err)
		}
		if conflictingClose.Outcome != string(app.WorkflowAccepted) {
			t.Fatalf("ClosePlan(second, different id) = %+v, want accepted (a fresh close after reopen has no relation to the first)", conflictingClose)
		}
	})

	t.Run("an unknown run is malformed", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		now := tc.Clock.Now()
		unknownRunID, err := identity.ParseRunID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse run id: %v", err)
		}
		taskID, err := identity.ParseTaskID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse task id: %v", err)
		}
		// requireManagerCaller refuses "caller is not the run's manager"
		// before CreateTask ever reaches the unknown-run lookup, so a
		// legitimate manager session and current binding are seeded for a
		// run that is never inserted into tc.Store.Runs at all — the only
		// way to reach the unknown-run branch itself (unreachable through
		// the driving Controller, whose own identity parsing never proves a
		// run exists, but reachable directly against the store, exactly as
		// internal/adapters/sqlite's storevectors test drives the identical
		// shape against the real store).
		managerID, err := identity.ParseSessionID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse session id: %v", err)
		}
		manager := run.NewManagerSession(managerID, unknownRunID, run.HarnessClaude, now)
		if manager, err = manager.Launch(now); err != nil {
			t.Fatalf("launch manager: %v", err)
		}
		if manager, err = manager.ConfirmActive(now); err != nil {
			t.Fatalf("activate manager: %v", err)
		}
		tc.Store.Sessions[managerID] = &entityRow[run.Session]{value: manager, revision: 1}
		incarnationID, err := identity.ParseIncarnationID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse incarnation id: %v", err)
		}
		tc.Store.Bindings[managerID] = append(tc.Store.Bindings[managerID],
			run.NewRuntimeBinding(managerID, incarnationID, "", "peer-pid:9", "ws", "tab", "pane", "label", run.LaunchInitial, now))

		got, err := tc.Store.CreateTask(context.Background(), app.TaskCreate{
			ID: taskID, RunID: unknownRunID, Session: managerID, IncarnationID: incarnationID,
			Title: "orphaned", InstructionsPath: "/state/instructions.md", InstructionsDigest: "digest",
		})
		if err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if got.Outcome != app.WorkflowMalformed || got.Reason != app.GrammarReasonMalformed {
			t.Fatalf("CreateTask() against an unknown run = %+v, want malformed/%s", got, app.GrammarReasonMalformed)
		}
	})
}
