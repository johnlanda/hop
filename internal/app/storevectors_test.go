package app_test

import (
	"context"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
	"github.com/johnlanda/hop/internal/testsupport/storevectors"
)

// TestStoreVectors drives every internal/testsupport/storevectors vector
// against fakeStore through the ordinary PlanStore/MessagingStore ports —
// the internal/app half of the shared refused-input contract; a future
// internal/adapters/sqlite vector-contract test drives the identical
// vectors against the real store (docs/plan/phase-3-design.md section 11).
func TestStoreVectors(t *testing.T) {
	t.Run("TaskCreateSelfDependency", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskID, err := identity.ParseTaskID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse task id: %v", err)
		}

		got, err := tc.Controller.Plan.CreateTask(context.Background(), storevectors.TaskCreateSelfDependency(fr.RunID, fr.ManagerID, fr.ManagerIncarnation, taskID))
		if err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if got.Outcome != app.WorkflowRefused {
			t.Fatalf("CreateTask(self-dependency) = %+v, want refused", got)
		}
	})

	t.Run("TaskCreateRequestIDConflict", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		firstID, err := identity.ParseTaskID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse task id: %v", err)
		}
		secondID, err := identity.ParseTaskID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse task id: %v", err)
		}
		const requestID = "shared-request-id"

		first, err := tc.Controller.Plan.CreateTask(context.Background(), storevectors.TaskCreateRequestIDConflictFirst(fr.RunID, fr.ManagerID, fr.ManagerIncarnation, firstID, requestID))
		if err != nil {
			t.Fatalf("CreateTask(first) error = %v", err)
		}
		if first.Outcome != app.WorkflowAccepted {
			t.Fatalf("CreateTask(first) = %+v, want accepted", first)
		}

		second, err := tc.Controller.Plan.CreateTask(context.Background(), storevectors.TaskCreateRequestIDConflictSecond(fr.RunID, fr.ManagerID, fr.ManagerIncarnation, secondID, requestID))
		if err != nil {
			t.Fatalf("CreateTask(second) error = %v", err)
		}
		if second.Outcome != app.WorkflowRefused {
			t.Fatalf("CreateTask(second, conflicting request id) = %+v, want refused", second)
		}
	})

	t.Run("TaskCreateNonManagerCaller", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskA := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
		workerID, workerIncarnation := seedWorkerSession(t, tc, fr, taskA)
		taskID, err := identity.ParseTaskID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse task id: %v", err)
		}

		got, err := tc.Controller.Plan.CreateTask(context.Background(), storevectors.TaskCreateNonManagerCaller(fr.RunID, workerID, workerIncarnation, taskID))
		if err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if got.Outcome != app.WorkflowRefused {
			t.Fatalf("CreateTask(non-manager caller) = %+v, want refused", got)
		}
	})

	t.Run("TaskCreateOversizedTitle", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskID, err := identity.ParseTaskID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse task id: %v", err)
		}

		got, err := tc.Controller.Plan.CreateTask(context.Background(), storevectors.TaskCreateOversizedTitle(fr.RunID, fr.ManagerID, fr.ManagerIncarnation, taskID))
		if err != nil {
			t.Fatalf("CreateTask() error = %v", err)
		}
		if got.Outcome != app.WorkflowMalformed {
			t.Fatalf("CreateTask(oversized title) = %+v, want malformed", got)
		}
	})

	t.Run("AckMessageStaleIncarnation", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskB := seedImplementTask(t, tc, fr.RunID, 1, "B", false, run.TaskReady)
		workerID, workerIncarnation := seedWorkerSession(t, tc, fr, taskB)

		send, err := tc.Controller.SendMessage(context.Background(), app.SendMessageRequest{
			RunID: fr.RunID.String(), SessionID: workerID.String(), IncarnationID: workerIncarnation.String(),
			StateRoot: "/state", To: "manager", Kind: "question", Body: []byte("q?"),
		})
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}
		if _, fetchErr := tc.Controller.FetchMessage(context.Background(), app.FetchMessageRequest{
			RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
		}); fetchErr != nil {
			t.Fatalf("FetchMessage() error = %v", fetchErr)
		}
		staleIncarnation, err := identity.ParseIncarnationID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse incarnation id: %v", err)
		}
		messageID, err := identity.ParseMessageID(send.MessageID)
		if err != nil {
			t.Fatalf("parse message id: %v", err)
		}

		got, err := tc.Controller.Messages.AckMessage(context.Background(), storevectors.AckMessageStaleIncarnation(fr.RunID, messageID, fr.ManagerID, staleIncarnation))
		if err != nil {
			t.Fatalf("AckMessage() error = %v", err)
		}
		if got.Kind != app.AckRefused {
			t.Fatalf("AckMessage(stale incarnation) = %+v, want refused", got)
		}
	})

	t.Run("MessageSendAnswerUnknownQuestion", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		answerID, err := identity.ParseMessageID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse message id: %v", err)
		}
		unknownQuestionID, err := identity.ParseMessageID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse message id: %v", err)
		}

		got, err := tc.Controller.Messages.SendMessage(context.Background(), storevectors.MessageSendAnswerUnknownQuestion(
			fr.RunID, fr.ManagerID, run.ManagerAddress(), fr.ManagerIncarnation, answerID, unknownQuestionID, "/state/body.md", "digest", 3,
		))
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}
		if got.Kind != app.MessageMalformed {
			t.Fatalf("SendMessage(answer to unknown question) = %+v, want malformed", got)
		}
	})
}
