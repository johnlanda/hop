package app_test

import (
	"context"
	"errors"
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
		if got.Outcome != app.WorkflowRefused || got.Reason != storevectors.TaskCreateSelfDependencyReason {
			t.Fatalf("CreateTask(self-dependency) = %+v, want refused/%s", got, storevectors.TaskCreateSelfDependencyReason)
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
		if second.Outcome != app.WorkflowRefused || second.Reason != storevectors.TaskCreateRequestIDConflictReason {
			t.Fatalf("CreateTask(second, conflicting request id) = %+v, want refused/%s", second, storevectors.TaskCreateRequestIDConflictReason)
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
		if got.Outcome != app.WorkflowRefused || got.Reason != storevectors.TaskCreateNonManagerCallerReason {
			t.Fatalf("CreateTask(non-manager caller) = %+v, want refused/%s", got, storevectors.TaskCreateNonManagerCallerReason)
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
		if got.Outcome != app.WorkflowMalformed || got.Reason != storevectors.TaskCreateOversizedTitleReason {
			t.Fatalf("CreateTask(oversized title) = %+v, want malformed/%s", got, storevectors.TaskCreateOversizedTitleReason)
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
		messageID, err := identity.ParseMessageID(send.MessageID)
		if err != nil {
			t.Fatalf("parse message id: %v", err)
		}

		// Supersede the manager's incarnation that actually received the
		// delivery with a fresh one — mirroring
		// TestAckMessageRequiresCurrentIncarnation's reattach seeding — so
		// AckMessageStaleIncarnation's claimed incarnation (the delivered,
		// now-superseded one) satisfies the delivery check and fails only
		// the currency check.
		history := tc.Store.Bindings[fr.ManagerID]
		history[len(history)-1].Superseded = true
		freshIncarnation, err := identity.ParseIncarnationID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse incarnation id: %v", err)
		}
		last := history[len(history)-1]
		fresh := run.NewRuntimeBinding(fr.ManagerID, freshIncarnation, last.ServerSocketPath, last.ServerInstance, last.WorkspaceID, last.TabID, last.PaneID, last.CreationLabel, run.LaunchResume, tc.Clock.Now())
		tc.Store.Bindings[fr.ManagerID] = append(history, fresh)

		got, err := tc.Controller.Messages.AckMessage(context.Background(), storevectors.AckMessageStaleIncarnation(fr.RunID, messageID, fr.ManagerID, fr.ManagerIncarnation))
		if err != nil {
			t.Fatalf("AckMessage() error = %v", err)
		}
		if got.Kind != app.AckRefused || got.Reason != storevectors.AckMessageStaleIncarnationReason {
			t.Fatalf("AckMessage(stale incarnation) = %+v, want refused/%s", got, storevectors.AckMessageStaleIncarnationReason)
		}
	})

	t.Run("AckMessageUnknownMessage", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		unknownMessageID, err := identity.ParseMessageID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse message id: %v", err)
		}

		got, err := tc.Controller.Messages.AckMessage(context.Background(), storevectors.AckMessageUnknownMessage(fr.RunID, unknownMessageID, fr.ManagerID, fr.ManagerIncarnation))
		if err != nil {
			t.Fatalf("AckMessage() error = %v", err)
		}
		if got.Kind != app.AckRefused || got.Reason != storevectors.AckMessageUnknownMessageReason {
			t.Fatalf("AckMessage(unknown message) = %+v, want refused/%s", got, storevectors.AckMessageUnknownMessageReason)
		}
	})

	t.Run("AckMessageNotDelivered", func(t *testing.T) {
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
		messageID, err := identity.ParseMessageID(send.MessageID)
		if err != nil {
			t.Fatalf("parse message id: %v", err)
		}

		got, err := tc.Controller.Messages.AckMessage(context.Background(), storevectors.AckMessageNotDelivered(fr.RunID, messageID, fr.ManagerID, fr.ManagerIncarnation))
		if err != nil {
			t.Fatalf("AckMessage() error = %v", err)
		}
		if got.Kind != app.AckRefused || got.Reason != storevectors.AckMessageNotDeliveredReason {
			t.Fatalf("AckMessage(not delivered) = %+v, want refused/%s", got, storevectors.AckMessageNotDeliveredReason)
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
		if got.Kind != app.MessageMalformed || got.Reason != storevectors.MessageSendAnswerUnknownQuestionReason {
			t.Fatalf("SendMessage(answer to unknown question) = %+v, want malformed/%s", got, storevectors.MessageSendAnswerUnknownQuestionReason)
		}
	})

	t.Run("MessageSendCrossRun", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr1 := seedFeatureRun(t, tc, 2)
		fr2 := seedFeatureRun(t, tc, 2)
		messageID, err := identity.ParseMessageID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse message id: %v", err)
		}

		// fr1's manager session, but the request claims fr2 as its run.
		got, err := tc.Controller.Messages.SendMessage(context.Background(), storevectors.MessageSendCrossRun(
			fr2.RunID, fr1.ManagerID, run.ManagerAddress(), fr1.ManagerIncarnation, messageID, run.HumanAddress(), "/state/body.md", "digest", 3,
		))
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}
		if got.Kind != app.MessageRefused || got.Reason != storevectors.MessageSendCrossRunReason {
			t.Fatalf("SendMessage(cross-run) = %+v, want refused/%s", got, storevectors.MessageSendCrossRunReason)
		}
		if _, exists := tc.Store.Messages[messageID]; exists {
			t.Fatalf("SendMessage(cross-run) must not create a message")
		}
	})

	t.Run("MessageFetchCrossRun", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr1 := seedFeatureRun(t, tc, 2)
		fr2 := seedFeatureRun(t, tc, 2)

		// fr1's manager session, but the request claims fr2 as its run.
		_, ok, err := tc.Controller.Messages.FetchNextMessage(context.Background(), storevectors.MessageFetchCrossRun(
			fr2.RunID, fr1.ManagerID, fr1.ManagerIncarnation, run.ManagerAddress(),
		))
		if ok {
			t.Fatalf("FetchNextMessage(cross-run) reported a delivery; want refused")
		}
		if !errors.Is(err, app.ErrMessagingUnauthorized) {
			t.Fatalf("FetchNextMessage(cross-run) error = %v, want ErrMessagingUnauthorized", err)
		}
	})

	t.Run("AckMessageCrossRun", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr1 := seedFeatureRun(t, tc, 2)
		fr2 := seedFeatureRun(t, tc, 2)
		taskB := seedImplementTask(t, tc, fr1.RunID, 1, "B", false, run.TaskReady)
		workerID, workerIncarnation := seedWorkerSession(t, tc, fr1, taskB)

		send, err := tc.Controller.SendMessage(context.Background(), app.SendMessageRequest{
			RunID: fr1.RunID.String(), SessionID: workerID.String(), IncarnationID: workerIncarnation.String(),
			StateRoot: "/state", To: "manager", Kind: "question", Body: []byte("q?"),
		})
		if err != nil {
			t.Fatalf("SendMessage() error = %v", err)
		}
		messageID, err := identity.ParseMessageID(send.MessageID)
		if err != nil {
			t.Fatalf("parse message id: %v", err)
		}

		// A delivery row for fr2's manager, seeded directly rather than
		// through FetchNextMessage (whose own cross-run refusal — the
		// MessageFetchCrossRun vector above — would otherwise make this
		// row impossible to create honestly): this isolates the ack-side
		// cross-run check from the ALSO-true "never delivered to this
		// session" refusal (TestAckMessageRequiresOwnDelivery) that would
		// otherwise produce the identical outcome for an unrelated
		// reason, defense in depth against exactly this kind of
		// already-corrupt row.
		tc.Store.MessageDeliveries[messageID] = append(tc.Store.MessageDeliveries[messageID], run.Delivery{
			MessageID: messageID, SessionID: fr2.ManagerID, IncarnationID: fr2.ManagerIncarnation, At: tc.Clock.Now(),
		})

		// fr2's manager session, but the request claims fr1 (the
		// message's actual run) — a cross-run ack attempt.
		got, err := tc.Controller.Messages.AckMessage(context.Background(), storevectors.AckMessageCrossRun(fr1.RunID, messageID, fr2.ManagerID, fr2.ManagerIncarnation))
		if err != nil {
			t.Fatalf("AckMessage() error = %v", err)
		}
		if got.Kind != app.AckRefused || got.Reason != storevectors.AckMessageCrossRunReason {
			t.Fatalf("AckMessage(cross-run) = %+v, want refused/%s", got, storevectors.AckMessageCrossRunReason)
		}
	})

	t.Run("ReviewSubmitForeignReviewer", func(t *testing.T) {
		// Both foreign shapes drive BOTH fake layers — the featureStore
		// wrapper the controller wires and the base fakeStore beneath it —
		// so a direct caller of either is refused exactly like the real
		// store refuses the identical vector, with no review row and the
		// target attempt untouched.
		refuse := func(t *testing.T, rf *reviewFixture, foreignSession identity.SessionID, foreignIncarnation identity.IncarnationID) {
			t.Helper()
			reviewID, err := identity.ParseReviewID(rf.tc.IDs.NewID())
			if err != nil {
				t.Fatalf("parse review id: %v", err)
			}
			vector := storevectors.ReviewSubmitForeignReviewer(
				rf.fr.RunID, rf.TaskID, rf.AttemptID, foreignSession, foreignIncarnation,
				reviewID, rf.SubjectCommit, fakeSubjectTree, "/state/reasons.md", "reasons-digest",
			)
			for name, store := range map[string]app.ReviewStore{
				"featureStore": rf.tc.Controller.Reviews,
				"fakeStore":    rf.tc.Store,
			} {
				got, err := store.SubmitReview(context.Background(), vector)
				if err != nil {
					t.Fatalf("%s.SubmitReview() error = %v", name, err)
				}
				if got.Kind != app.ReviewStale {
					t.Fatalf("%s.SubmitReview(foreign reviewer) = %+v, want stale refusal", name, got)
				}
				if _, exists := rf.tc.Store.Reviews[rf.AttemptID]; exists {
					t.Fatalf("%s accepted a foreign reviewer's verdict: review row exists", name)
				}
				if attempt := rf.tc.Store.Attempts[rf.AttemptID]; attempt.value.State != run.AttemptRunning {
					t.Fatalf("%s moved the attempt to %s; want untouched", name, attempt.value.State)
				}
			}
		}

		t.Run("reviewer of another run", func(t *testing.T) {
			rf := newReviewFixture(t)
			fr2 := seedFeatureRun(t, rf.tc, 2)
			foreignSession, foreignIncarnation := seedForeignReviewer(t, rf.tc, fr2, 2)
			refuse(t, rf, foreignSession, foreignIncarnation)
		})

		t.Run("reviewer of a different review attempt in the same run", func(t *testing.T) {
			rf := newReviewFixture(t)
			foreignSession, foreignIncarnation := seedForeignReviewer(t, rf.tc, rf.fr, 3)
			refuse(t, rf, foreignSession, foreignIncarnation)
		})
	})
	t.Run("TaskRetry", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskID := seedImplementTask(t, tc, fr.RunID, 4, "D", false, run.TaskReady)
		workerID, _ := seedWorkerSession(t, tc, fr, taskID)
		attemptID := tc.Store.Sessions[workerID].value.AttemptID
		interrupted, err := tc.Store.Attempts[attemptID].value.Interrupt(tc.Clock.Now())
		if err != nil {
			t.Fatalf("Interrupt() error = %v", err)
		}
		tc.Store.Attempts[attemptID].value = interrupted
		needsRework, err := tc.Store.Tasks[taskID].value.NeedsRework(tc.Clock.Now())
		if err != nil {
			t.Fatalf("NeedsRework() error = %v", err)
		}
		tc.Store.Tasks[taskID].value = needsRework

		request := storevectors.TaskRetry(fr.RunID, fr.ManagerID, fr.ManagerIncarnation, taskID, "retry-vector")
		for _, want := range []app.WorkflowOutcomeKind{app.WorkflowAccepted, app.WorkflowDuplicate} {
			got, err := tc.Controller.Plan.RequestRetry(context.Background(), request)
			if err != nil {
				t.Fatalf("RequestRetry() error = %v", err)
			}
			if got.Outcome != want || got.TaskSeq != 4 || got.AttemptNumber != 2 || got.Reason != storevectors.TaskRetryReason {
				t.Fatalf("RequestRetry() = %+v, want %s t4 attempt 2", got, want)
			}
		}
	})
}

// seedForeignReviewer builds a second review task inside fr with its own
// running attempt and a live, currently-bound reviewer session — a
// legitimate reviewer that is simply not the target attempt's — and
// returns that session's identities.
func seedForeignReviewer(t *testing.T, tc *testController, fr featureRun, seq int) (identity.SessionID, identity.IncarnationID) { //nolint:gocritic // hugeParam: featureRun is a small test fixture value passed once per call, never a hot loop.
	t.Helper()
	now := tc.Clock.Now()
	taskID, err := identity.ParseTaskID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse task id: %v", err)
	}
	review := run.NewReviewTask(taskID, fr.RunID, seq, "other-subject-commit", fakeSubjectTree, now)
	review.State = run.TaskActive
	tc.Store.Tasks[taskID] = &entityRow[run.Task]{value: review, revision: 1}

	attemptID, err := identity.ParseAttemptID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse attempt id: %v", err)
	}
	attempt, err := run.NewAttempt(attemptID, taskID, 1, now)
	if err != nil {
		t.Fatalf("NewAttempt() error = %v", err)
	}
	attempt.State = run.AttemptRunning
	tc.Store.Attempts[attemptID] = &entityRow[run.Attempt]{value: attempt, revision: 1}

	sessionID, err := identity.ParseSessionID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse session id: %v", err)
	}
	reviewer, err := run.NewChildSession(sessionID, fr.RunID, attemptID, run.RoleReviewer, tc.Store.Sessions[fr.ManagerID].value, run.HarnessClaude, now)
	if err != nil {
		t.Fatalf("NewChildSession() error = %v", err)
	}
	if reviewer, err = reviewer.Launch(now); err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	if reviewer, err = reviewer.ConfirmActive(now); err != nil {
		t.Fatalf("ConfirmActive() error = %v", err)
	}
	tc.Store.Sessions[sessionID] = &entityRow[run.Session]{value: reviewer, revision: 1}

	incarnationID, err := identity.ParseIncarnationID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse incarnation id: %v", err)
	}
	binding := run.NewRuntimeBinding(sessionID, incarnationID, "", "peer-pid:2", "ws-f", "tab-f", "pane-f", "label-f", run.LaunchInitial, now)
	tc.Store.Bindings[sessionID] = append(tc.Store.Bindings[sessionID], binding)
	tc.Store.LaunchClaims[incarnationID] = app.LaunchClaim{
		IncarnationID: incarnationID, RunID: fr.RunID, SessionID: sessionID, AttemptID: attemptID,
		Executable: "/usr/local/bin/claude", PID: 556, State: app.LaunchClaimExeced, ClaimedAt: now,
	}
	return sessionID, incarnationID
}

// validFeatureRunSpec is a feature-mode NewRunSpec any conforming store
// accepts as the first run of root: the manager session identity, no solo
// bootstrap identity, and an integration branch naming sequence 1.
func validFeatureRunSpec(t *testing.T, tc *testController, root string) app.NewRunSpec {
	t.Helper()
	runID, err := identity.ParseRunID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse run id: %v", err)
	}
	managerID, err := identity.ParseSessionID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse session id: %v", err)
	}
	return app.NewRunSpec{
		RepositoryRoot: root, RunID: runID, SessionID: managerID,
		Brief: "feature brief", BriefDigest: "brief-digest",
		Snapshot: app.RunSnapshot{
			StateRoot: "/state", AssignmentPath: "/state/runs/" + runID.String() + "/artifacts/assignment.md", AssignmentDigest: "assignment-digest",
			Harness: "claude",
			Workflow: app.WorkflowSnapshot{
				Mode: app.WorkflowModeFeature, MaxWorkers: 2, RetryLimit: 3,
				ManagerRolePath: "/state/roles/manager.md", ManagerRoleDigest: "manager-digest",
				ImplementerRolePath: "/state/roles/implementer.md", ImplementerRoleDigest: "implementer-digest",
				ReviewerRolePath: "/state/roles/reviewer.md", ReviewerRoleDigest: "reviewer-digest",
				ReviewerHarness: "claude", MessageAttention: app.DefaultMessageAttention, MessageWait: app.DefaultMessageWait,
				IntegrationBranch: app.IntegrationBranchName(1), BaseCommitOID: "cccccccccccccccccccccccccccccccccccccccc",
			},
		},
		Harness: run.HarnessClaude, NativeSessionRef: "native-ref-manager",
		ControllerID: "controller-1", Now: tc.Clock.Now(),
	}
}

// TestStoreVectorsFeatureBootstrap drives the feature-run bootstrap
// vectors against fakeStore.InitializeRun: every refusal carries its
// typed sentinel and leaves the store exactly as it was — no repository
// row, no sequence consumed, no run, session or lease — and the valid spec
// resubmitted afterward initializes at sequence 1.
func TestStoreVectorsFeatureBootstrap(t *testing.T) {
	const root = "/repo"
	cases := []struct {
		name   string
		bend   func(tc *testController, valid app.NewRunSpec) app.NewRunSpec
		target error
	}{
		{"FeatureRunSpecWithTask", func(tc *testController, v app.NewRunSpec) app.NewRunSpec {
			return storevectors.FeatureRunSpecWithTask(v, identity.TaskID(tc.IDs.NewID()))
		}, app.ErrFeatureRunSpecInvalid},
		{"FeatureRunSpecWithAttempt", func(tc *testController, v app.NewRunSpec) app.NewRunSpec {
			return storevectors.FeatureRunSpecWithAttempt(v, identity.AttemptID(tc.IDs.NewID()))
		}, app.ErrFeatureRunSpecInvalid},
		{"FeatureRunSpecWithWorktree", func(tc *testController, v app.NewRunSpec) app.NewRunSpec {
			return storevectors.FeatureRunSpecWithWorktree(v, identity.WorktreeID(tc.IDs.NewID()))
		}, app.ErrFeatureRunSpecInvalid},
		{"FeatureRunSpecWithoutManager", func(_ *testController, v app.NewRunSpec) app.NewRunSpec {
			return storevectors.FeatureRunSpecWithoutManager(v)
		}, app.ErrFeatureRunSpecInvalid},
		{"FeatureRunSpecSequenceMismatch", func(_ *testController, v app.NewRunSpec) app.NewRunSpec {
			return storevectors.FeatureRunSpecSequenceMismatch(v, 1)
		}, app.ErrRunSequenceMismatch},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			tc := newTestController(defaultPolicy())
			valid := validFeatureRunSpec(t, tc, root)

			_, _, err := tc.Store.InitializeRun(context.Background(), tt.bend(tc, valid))
			if !errors.Is(err, tt.target) {
				t.Fatalf("InitializeRun(%s) error = %v, want %v", tt.name, err, tt.target)
			}
			if len(tc.Store.Runs) != 0 || len(tc.Store.Sessions) != 0 || len(tc.Store.Leases) != 0 || len(tc.Store.Snapshots) != 0 || len(tc.Store.repoByRoot) != 0 || len(tc.Store.seqByRepo) != 0 {
				t.Fatalf("a refused InitializeRun left state behind: runs=%d sessions=%d leases=%d snapshots=%d repos=%d seqs=%d",
					len(tc.Store.Runs), len(tc.Store.Sessions), len(tc.Store.Leases), len(tc.Store.Snapshots), len(tc.Store.repoByRoot), len(tc.Store.seqByRepo))
			}

			if _, _, err := tc.Store.InitializeRun(context.Background(), valid); err != nil {
				t.Fatalf("InitializeRun(valid) after the refusal error = %v", err)
			}
			if got := tc.Store.Runs[valid.RunID].value.Sequence; got != 1 {
				t.Fatalf("valid run sequence = %d, want 1: the refused attempt must consume nothing", got)
			}
		})
	}
}
