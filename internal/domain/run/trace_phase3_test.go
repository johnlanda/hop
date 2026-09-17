package run_test

import (
	"errors"
	"testing"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// TestReferenceTraceFeatureHappyPathDependencyRelease is Phase 3 reference
// trace 1 (docs/plan/phase-3-design.md section 5): the manager plans two
// dependent implement tasks and a review; dependency release gates B on
// A's integration, not merely A's completion; the run completes only once
// EvaluateReadiness holds inside the completion transaction.
func TestReferenceTraceFeatureHappyPathDependencyRelease(t *testing.T) {
	r := run.NewRun(testRunID, testRepositoryID, 1, "brief-digest", epoch())
	manager := run.NewManagerSession(testManagerSessionID, testRunID, run.HarnessClaude, epoch())

	// Manager launched (Phase 2 launch trace verbatim for the manager
	// session).
	r, err := r.Launch(epoch())
	mustNoError(t, err)
	manager, err = manager.Launch(epoch())
	mustNoError(t, err)
	r, err = r.MarkRunning(later())
	mustNoError(t, err)
	manager, err = manager.ConfirmActive(later())
	mustNoError(t, err)
	mustState(t, "run", string(r.State), string(run.RunRunning))

	// Manager CreateTask A (no deps -> ready) and B (depends on A ->
	// pending), then ClosePlan (plan closed).
	taskA := run.NewImplementTask(testTaskID, testRunID, 1, "A", "digest-a", false, later())
	taskB := run.NewImplementTask(testSecondTaskID, testRunID, 2, "B", "digest-b", true, later())
	dep, err := run.NewTaskDependency(taskB.ID, taskA.ID, later())
	mustNoError(t, err)
	mustNoError(t, run.ValidateAcyclic(nil, dep))
	mustState(t, "taskA", string(taskA.State), string(run.TaskReady))
	mustState(t, "taskB", string(taskB.State), string(run.TaskPending))
	r, err = r.ClosePlan(true, later())
	mustNoError(t, err)
	if !r.PlanClosed {
		t.Fatal("ClosePlan: PlanClosed = false, want true")
	}

	// Controller assigns A (Task A ready->active, Attempt A1
	// reserved->launching, Session reserved->launching, atomically).
	attemptA, err := run.NewAttempt(testAttemptID, taskA.ID, 1, later())
	mustNoError(t, err)
	taskA, err = taskA.Activate(later())
	mustNoError(t, err)
	attemptA, err = attemptA.Launch(later())
	mustNoError(t, err)
	implA, err := run.NewChildSession(testSessionID, testRunID, attemptA.ID, run.RoleImplementer, manager, run.HarnessClaude, later())
	mustNoError(t, err)
	implA, err = implA.Launch(later())
	mustNoError(t, err)
	attemptA, err = attemptA.MarkRunning(later())
	mustNoError(t, err)
	implA, err = implA.ConfirmActive(later())
	mustNoError(t, err)

	// A submits, and the acceptance commits with it the retirement
	// intent for A's session.
	submissionA := run.ResultSubmission{ID: testResultID, CommitOID: "a-commit", Summary: "A done", Digest: "digest-a1"}
	resultOutcome, err := run.AcceptResult(r, taskA, attemptA, nil, run.AcceptanceContext{IncarnationCurrent: true, MailboxClear: true}, submissionA, later())
	mustNoError(t, err)
	r, taskA, attemptA = resultOutcome.Run, resultOutcome.Task, resultOutcome.Attempt
	mustState(t, "taskA", string(taskA.State), string(run.TaskChecking))
	implA, err = implA.Stop(later())
	mustNoError(t, err)
	implA, err = implA.Terminate(later())
	mustNoError(t, err)
	mustState(t, "implA", string(implA.State), string(run.SessionTerminated))

	// Per-task check passes (Phase 2 trace) -> A completed.
	attemptA, err = attemptA.EnterChecking(later())
	mustNoError(t, err)
	attemptA, err = attemptA.Complete(later())
	mustNoError(t, err)
	taskA, err = taskA.Complete(later())
	mustNoError(t, err)
	mustState(t, "taskA", string(taskA.State), string(run.TaskCompleted))

	// Integration A claimed -> A integrating, scratch merge produced
	// and published (CAS) -> integration checking, combined check
	// passes -> A integrated AND — same transaction — B
	// pending->ready and the controller info message to the manager
	// commits with it.
	taskA, err = taskA.EnterIntegrating(later())
	mustNoError(t, err)
	integrationA := run.NewIntegration(testIntegrationID, testRunID, taskA.ID, testResultID, "a-commit", "base-oid", later())
	integrationA, err = integrationA.EnterChecking("a-merge-oid", later())
	mustNoError(t, err)
	mustState(t, "integrationA", string(integrationA.State), string(run.IntegrationChecking))
	integrationA, err = integrationA.Integrate(later())
	mustNoError(t, err)
	taskA, err = taskA.Integrate(later())
	mustNoError(t, err)
	mustState(t, "taskA", string(taskA.State), string(run.TaskIntegrated))

	if !run.ReleaseEligible(taskB, []run.TaskDependency{dep}, []run.Task{taskA}) {
		t.Fatal("ReleaseEligible(taskB): want true once taskA is integrated")
	}
	taskB, err = taskB.Release([]run.TaskDependency{dep}, []run.Task{taskA}, later())
	mustNoError(t, err)
	mustState(t, "taskB", string(taskB.State), string(run.TaskReady))
	infoAIntegrated := run.NewInfo(testMessageID, testRunID, run.ControllerPrincipal(), run.ManagerAddress(), "", "/notice-a-integrated", "digest", 32, 1, later())
	mustState(t, "infoAIntegrated", string(infoAIntegrated.State), string(run.MessageQueued))

	// B assigned and completes the same way.
	attemptB, err := run.NewAttempt(testSecondAttemptID, taskB.ID, 1, later())
	mustNoError(t, err)
	taskB, err = taskB.Activate(later())
	mustNoError(t, err)
	attemptB, err = attemptB.Launch(later())
	mustNoError(t, err)
	implB, err := run.NewChildSession(testSecondSessionID, testRunID, attemptB.ID, run.RoleImplementer, manager, run.HarnessClaude, later())
	mustNoError(t, err)
	implB, err = implB.Launch(later())
	mustNoError(t, err)
	attemptB, err = attemptB.MarkRunning(later())
	mustNoError(t, err)
	implB, err = implB.ConfirmActive(later())
	mustNoError(t, err)

	submissionB := run.ResultSubmission{ID: identityOtherResultID, CommitOID: "b-commit", Summary: "B done", Digest: "digest-b1"}
	resultOutcome, err = run.AcceptResult(r, taskB, attemptB, nil, run.AcceptanceContext{IncarnationCurrent: true, MailboxClear: true}, submissionB, later())
	mustNoError(t, err)
	r, taskB, attemptB = resultOutcome.Run, resultOutcome.Task, resultOutcome.Attempt
	implB, err = implB.Stop(later())
	mustNoError(t, err)
	implB, err = implB.Terminate(later())
	mustNoError(t, err)
	mustState(t, "implB", string(implB.State), string(run.SessionTerminated))

	attemptB, err = attemptB.EnterChecking(later())
	mustNoError(t, err)
	attemptB, err = attemptB.Complete(later())
	mustNoError(t, err)
	taskB, err = taskB.Complete(later())
	mustNoError(t, err)
	taskB, err = taskB.EnterIntegrating(later())
	mustNoError(t, err)
	integrationB := run.NewIntegration(testSecondIntegrationID, testRunID, taskB.ID, identityOtherResultID, "b-commit", "a-merge-oid", later())
	integrationB, err = integrationB.EnterChecking("b-merge-oid", later())
	mustNoError(t, err)
	integrationB, err = integrationB.Integrate(later())
	mustNoError(t, err)
	mustState(t, "integrationB", string(integrationB.State), string(run.IntegrationIntegrated))
	taskB, err = taskB.Integrate(later())
	mustNoError(t, err)
	mustState(t, "taskB", string(taskB.State), string(run.TaskIntegrated))

	// Review task R created ready (plan closed, all implement tasks
	// integrated) -> reviewer assigned -> verdict approve accepted (R
	// active->completed, reviewer session retired the same way) ->
	// EvaluateReadiness re-validated inside the completion transaction
	// -> Run completing->completed after manager retirement.
	reviewTask := run.NewReviewTask(testReviewTaskID, testRunID, 3, "b-merge-oid", "head-tree", later())
	mustState(t, "reviewTask", string(reviewTask.State), string(run.TaskReady))
	reviewAttempt, err := run.NewAttempt(testReviewAttemptID, reviewTask.ID, 1, later())
	mustNoError(t, err)
	reviewTask, err = reviewTask.Activate(later())
	mustNoError(t, err)
	reviewAttempt, err = reviewAttempt.Launch(later())
	mustNoError(t, err)
	reviewer, err := run.NewChildSession(testReviewerSessionID, testRunID, reviewAttempt.ID, run.RoleReviewer, manager, run.HarnessClaude, later())
	mustNoError(t, err)
	reviewer, err = reviewer.Launch(later())
	mustNoError(t, err)
	reviewAttempt, err = reviewAttempt.MarkRunning(later())
	mustNoError(t, err)
	reviewer, err = reviewer.ConfirmActive(later())
	mustNoError(t, err)

	verdictSubmission := run.ReviewSubmission{ID: testReviewID, SubjectCommitOID: "b-merge-oid", SubjectTreeOID: "head-tree", Verdict: run.VerdictApprove, ReasonsDigest: "reasons-v1"}
	verdictOutcome, err := run.AcceptVerdict(r, reviewTask, reviewAttempt, nil, run.ReviewAcceptanceContext{IncarnationCurrent: true, MailboxClear: true}, verdictSubmission, later())
	mustNoError(t, err)
	r, reviewTask, reviewAttempt = verdictOutcome.Run, verdictOutcome.Task, verdictOutcome.Attempt
	mustState(t, "reviewAttempt", string(reviewAttempt.State), string(run.AttemptCompleted))
	mustState(t, "reviewTask", string(reviewTask.State), string(run.TaskCompleted))
	reviewer, err = reviewer.Stop(later())
	mustNoError(t, err)
	reviewer, err = reviewer.Terminate(later())
	mustNoError(t, err)
	mustState(t, "reviewer", string(reviewer.State), string(run.SessionTerminated))

	guardCtx := run.GuardContext{
		PlanClosed:     r.PlanClosed,
		ImplementTasks: []run.Task{taskA, taskB},
		HeadCommitOID:  "b-merge-oid",
		HeadTreeOID:    "head-tree",
		LatestCheck:    &run.CheckReceipt{Passed: true, SubjectCommitOID: "b-merge-oid", SubjectTreeOID: "head-tree"},
		LatestReview:   &verdictOutcome.Review,
	}
	ready, missing := run.EvaluateReadiness(guardCtx)
	if !ready {
		t.Fatalf("EvaluateReadiness: ready = false, missing = %+v", missing)
	}
	r, err = r.EnterCompleting(later())
	mustNoError(t, err)

	manager, err = manager.Stop(later())
	mustNoError(t, err)
	manager, err = manager.Terminate(later())
	mustNoError(t, err)
	mustState(t, "manager", string(manager.State), string(run.SessionTerminated))

	r, err = r.Complete(later())
	mustNoError(t, err)
	mustState(t, "run", string(r.State), string(run.RunCompleted))
}

// TestReferenceTraceRelayedQuestion is Phase 3 reference trace 2: a
// worker's question relayed by the manager to the human and answered,
// forwarded back to the worker. No Run/Task/Attempt/Session state changes
// anywhere in the trace — messaging is orthogonal to the lifecycle
// machines.
func TestReferenceTraceRelayedQuestion(t *testing.T) {
	workerAddress := run.TaskAddress(testSecondTaskID) // task B's mailbox

	// Worker (task B) sends question q1 to manager (queued).
	q1 := run.NewQuestion(testMessageID, testRunID, run.SessionPrincipal(testSecondSessionID), run.ManagerAddress(), nil, "req-q1", "/q1", "digest-q1", 10, 1, epoch())
	mustNoError(t, run.ValidateSendAddressing(workerAddress, q1.Kind, q1.Recipient))
	mustState(t, "q1", string(q1.State), string(run.MessageQueued))

	// Manager fetch (q1 delivered).
	q1, err := q1.Deliver()
	mustNoError(t, err)
	mustState(t, "q1", string(q1.State), string(run.MessageDelivered))

	// Manager sends question q2 to human with --relay-of q1
	// (relayed_from recorded) and acks q1.
	q1ID := q1.ID
	q2 := run.NewQuestion(testSecondMessageID, testRunID, run.SessionPrincipal(testManagerSessionID), run.HumanAddress(), &q1ID, "req-q2", "/q2", "digest-q2", 12, 1, later())
	mustNoError(t, run.ValidateSendAddressing(run.ManagerAddress(), q2.Kind, q2.Recipient))
	if q2.RelayedFrom == nil || *q2.RelayedFrom != q1.ID {
		t.Fatalf("q2.RelayedFrom = %v, want %s", q2.RelayedFrom, q1.ID)
	}
	q1AckOutcome, err := run.AcceptAck(q1, nil, run.AckContext{DeliveredToSession: true, IncarnationCurrent: true}, run.Ack{SessionID: testManagerSessionID, IncarnationID: testIncarnation}, later())
	mustNoError(t, err)
	q1 = q1AckOutcome.Message
	mustState(t, "q1", string(q1.State), string(run.MessageAcknowledged))

	// Human hop answer on q2 (answer a2 accepted, q2 acknowledged,
	// atomically).
	a2Submission := run.AnswerSubmission{ID: testThirdMessageID, BodyPath: "/a2", BodyDigest: "digest-a2", BodyBytes: 8}
	a2Outcome, err := run.AcceptAnswer(q2, nil, run.AnswerContext{AnswererAddress: run.HumanAddress()}, run.ManagerAddress(), run.HumanPrincipal(), a2Submission, 1, later())
	mustNoError(t, err)
	q2, a2 := a2Outcome.Question, a2Outcome.Answer
	mustState(t, "q2", string(q2.State), string(run.MessageAcknowledged))
	mustState(t, "a2", string(a2.State), string(run.MessageQueued))

	// Manager fetch — a2's envelope carries origin=q1 resolved
	// server-side.
	a2, err = a2.Deliver()
	mustNoError(t, err)
	if origin := run.ResolveOrigin(q2); origin != q1.ID {
		t.Fatalf("ResolveOrigin(q2) = %s, want %s (q1)", origin, q1.ID)
	}

	// Manager FORWARDS first (answer a1, reply-to q1, destination
	// DERIVED as task:B from q1's sender), THEN acks a2 (forward-
	// before-ack, so a manager crash between the two re-serves a2 and
	// the forward's request ID makes the redo idempotent).
	a1Submission := run.AnswerSubmission{ID: testFourthMessageID, BodyPath: "/a1", BodyDigest: "digest-a1", BodyBytes: 8}
	a1Outcome, err := run.AcceptAnswer(q1, nil, run.AnswerContext{AnswererAddress: run.ManagerAddress()}, workerAddress, run.SessionPrincipal(testManagerSessionID), a1Submission, 2, later())
	mustNoError(t, err)
	q1Reconfirmed, a1 := a1Outcome.Question, a1Outcome.Answer
	// q1 was not human-addressed, so answering it does not itself
	// re-acknowledge it — it was already acknowledged above, by the
	// manager's own explicit ack, independently of this answer.
	mustState(t, "q1 (after forwarding its answer)", string(q1Reconfirmed.State), string(run.MessageAcknowledged))
	if a1.Recipient != workerAddress || a1.ReplyTo == nil || *a1.ReplyTo != q1.ID {
		t.Fatalf("a1 = %+v, want recipient=%v reply-to=%s", a1, workerAddress, q1.ID)
	}

	a2AckOutcome, err := run.AcceptAck(a2, nil, run.AckContext{DeliveredToSession: true, IncarnationCurrent: true}, run.Ack{SessionID: testManagerSessionID, IncarnationID: testIncarnation}, later())
	mustNoError(t, err)
	a2 = a2AckOutcome.Message
	mustState(t, "a2", string(a2.State), string(run.MessageAcknowledged))

	// Worker fetch (a1 delivered), worker acks, continues.
	a1, err = a1.Deliver()
	mustNoError(t, err)
	a1AckOutcome, err := run.AcceptAck(a1, nil, run.AckContext{DeliveredToSession: true, IncarnationCurrent: true}, run.Ack{SessionID: testSecondSessionID, IncarnationID: testIncarnation}, later())
	mustNoError(t, err)
	a1 = a1AckOutcome.Message
	mustState(t, "a1", string(a1.State), string(run.MessageAcknowledged))
}

// TestReferenceTraceDuplicateAndAmbiguousDelivery is Phase 3 reference
// trace 3: a worker killed between fetch and ack, a cold relaunch, and the
// at-least-once re-serve and stale/duplicate ack rules that follow.
func TestReferenceTraceDuplicateAndAmbiguousDelivery(t *testing.T) {
	m1 := run.NewInfo(testMessageID, testRunID, run.ControllerPrincipal(), run.ManagerAddress(), "", "/m1", "digest-m1", 8, 1, epoch())

	// Worker fetches m1 (delivered); worker killed before ack. The
	// first delivery's incarnation (testIncarnation) is now superseded.
	m1, err := m1.Deliver()
	mustNoError(t, err)

	// Cold relaunch (Phase 2 relaunch trace; new session, new
	// incarnation — testSecondIncarnation — same task address). New
	// worker fetches -> the SAME m1 re-served (second delivery row).
	m1, err = m1.Deliver()
	mustNoError(t, err)
	mustState(t, "m1", string(m1.State), string(run.MessageDelivered))

	// Old incarnation's late ack refused (stale, receipt recorded).
	_, err = run.AcceptAck(m1, nil, run.AckContext{DeliveredToSession: true, IncarnationCurrent: false}, run.Ack{SessionID: testSessionID, IncarnationID: testIncarnation}, later())
	if !errors.Is(err, run.ErrStaleAck) {
		t.Fatalf("late ack from the superseded incarnation: error = %v, want ErrStaleAck", err)
	}

	// New worker acks (acknowledged).
	ackOutcome, err := run.AcceptAck(m1, nil, run.AckContext{DeliveredToSession: true, IncarnationCurrent: true}, run.Ack{SessionID: testSessionID, IncarnationID: testSecondIncarnation}, later())
	mustNoError(t, err)
	m1 = ackOutcome.Message
	ack := ackOutcome.Ack
	mustState(t, "m1", string(m1.State), string(run.MessageAcknowledged))

	// Second ack of m1 from anyone -> duplicate, idempotent success.
	dupOutcome, err := run.AcceptAck(m1, &ack, run.AckContext{}, run.Ack{SessionID: testSecondSessionID, IncarnationID: testSecondIncarnation}, later())
	mustNoError(t, err)
	if dupOutcome.Ack != ack {
		t.Fatalf("duplicate ack = %+v, want the original ack unchanged: %+v", dupOutcome.Ack, ack)
	}

	// Next fetch serves m2: NextDeliverable never offers an
	// acknowledged message again.
	m2 := run.Message{ID: testSecondMessageID, EnqueueSeq: 2, State: run.MessageQueued}
	next, ok := run.NextDeliverable([]run.Message{m1, m2})
	if !ok || next.ID != m2.ID {
		t.Fatalf("NextDeliverable = (%+v, %v), want m2 %s", next, ok, m2.ID)
	}
}

// TestReferenceTraceWorkerInterruptionRetryProvenance is Phase 3 reference
// trace 4: a worker killed mid-attempt is reconciled, retried with full
// provenance across both attempts; the exhaustion variant moves the task
// directly to failed instead of parking it needs-rework.
func TestReferenceTraceWorkerInterruptionRetryProvenance(t *testing.T) {
	t.Run("retried below the limit", func(t *testing.T) {
		taskB := run.NewImplementTask(testSecondTaskID, testRunID, 2, "B", "digest-b", true, epoch())
		bEdges := []run.TaskDependency{{TaskID: taskB.ID, PrerequisiteID: testTaskID}}
		bPrereqs := []run.Task{{ID: testTaskID, State: run.TaskIntegrated}}
		taskB, err := taskB.Release(bEdges, bPrereqs, epoch())
		mustNoError(t, err)
		taskB, err = taskB.Activate(later())
		mustNoError(t, err)

		attemptB1, err := run.NewAttempt(testAttemptID, taskB.ID, 1, later())
		mustNoError(t, err)
		attemptB1, err = attemptB1.Launch(later())
		mustNoError(t, err)

		worker1 := run.NewSession(testSessionID, testRunID, attemptB1.ID, run.HarnessClaude, later())
		worker1, err = worker1.Launch(later())
		mustNoError(t, err)
		attemptB1, err = attemptB1.MarkRunning(later())
		mustNoError(t, err)
		worker1, err = worker1.ConfirmActive(later())
		mustNoError(t, err)

		// Worker killed; reconciliation establishes absence (Phase 2
		// evidence rules) -> Attempt B1 interrupted, Session
		// lost/terminated, Task B needs-rework, controller info
		// message to the manager — one transaction.
		worker1, err = worker1.Reconcile(later())
		mustNoError(t, err)
		worker1, err = worker1.MarkLost(later())
		mustNoError(t, err)
		attemptB1, err = attemptB1.Interrupt(later())
		mustNoError(t, err)
		taskB, err = taskB.NeedsRework(later())
		mustNoError(t, err)
		mustState(t, "taskB", string(taskB.State), string(run.TaskNeedsRework))
		mustState(t, "worker1", string(worker1.State), string(run.SessionLost))
		mustState(t, "attemptB1", string(attemptB1.State), string(run.AttemptInterrupted))

		// Manager RequestRetry; the controller consumes it: Attempt
		// B2 reserved (number 2), fresh worktree from the current
		// integration head, new session — Task B ready->active; B2
		// completes; the store shows both attempts, both worktrees,
		// both session lineages.
		attemptB2, err := run.NewRetryAttempt(testSecondAttemptID, attemptB1, 3, later())
		mustNoError(t, err)
		if attemptB2.Number != 2 {
			t.Fatalf("retry attempt Number = %d, want 2", attemptB2.Number)
		}
		taskB, err = taskB.Reopen(later())
		mustNoError(t, err)
		mustState(t, "taskB", string(taskB.State), string(run.TaskReady))
		taskB, err = taskB.Activate(later())
		mustNoError(t, err)
		attemptB2, err = attemptB2.Launch(later())
		mustNoError(t, err)
		worktree2 := run.NewWorktree(testWorktreeID, testRepositoryID, testRunID, "/worktrees/b2", "hop/r1/t2a2")
		mustState(t, "worktree2", string(worktree2.State), string(run.WorktreeActive))
		worker2 := run.NewSession(testSecondSessionID, testRunID, attemptB2.ID, run.HarnessClaude, later())
		worker2, err = worker2.Launch(later())
		mustNoError(t, err)
		attemptB2, err = attemptB2.MarkRunning(later())
		mustNoError(t, err)
		worker2, err = worker2.ConfirmActive(later())
		mustNoError(t, err)
		mustState(t, "worker2", string(worker2.State), string(run.SessionActive))

		submission := run.ResultSubmission{ID: testResultID, CommitOID: "b2-commit", Summary: "B retried", Digest: "digest-b2"}
		r := run.Run{ID: testRunID, State: run.RunRunning}
		outcome, err := run.AcceptResult(r, taskB, attemptB2, nil, run.AcceptanceContext{IncarnationCurrent: true, MailboxClear: true}, submission, later())
		mustNoError(t, err)
		attemptB2, taskB = outcome.Attempt, outcome.Task
		attemptB2, err = attemptB2.EnterChecking(later())
		mustNoError(t, err)
		attemptB2, err = attemptB2.Complete(later())
		mustNoError(t, err)
		taskB, err = taskB.Complete(later())
		mustNoError(t, err)
		mustState(t, "attemptB2", string(attemptB2.State), string(run.AttemptCompleted))
		mustState(t, "taskB", string(taskB.State), string(run.TaskCompleted))

		if attemptB1.Number == attemptB2.Number {
			t.Fatal("both attempts share a number; want distinct provenance")
		}
	})

	t.Run("exhaustion moves the task directly to failed", func(t *testing.T) {
		taskB := run.NewImplementTask(testSecondTaskID, testRunID, 2, "B", "digest-b", false, epoch())
		taskB, err := taskB.Activate(later())
		mustNoError(t, err)

		// Attempt 3 (the limit) is the one that gets interrupted; no
		// retry remains.
		attemptB3 := run.Attempt{ID: testAttemptID, TaskID: taskB.ID, Number: 3, State: run.AttemptInterrupted}
		if _, retryErr := run.NewRetryAttempt(testSecondAttemptID, attemptB3, 3, later()); !errors.Is(retryErr, run.ErrRetryLimit) {
			t.Fatalf("NewRetryAttempt at the limit: error = %v, want ErrRetryLimit", retryErr)
		}

		taskB, err = taskB.Fail(later())
		mustNoError(t, err)
		mustState(t, "taskB", string(taskB.State), string(run.TaskFailed))

		r := run.Run{ID: testRunID, State: run.RunRunning}
		r, err = r.Fail(later())
		mustNoError(t, err)
		mustState(t, "run", string(r.State), string(run.RunFailed))
	})
}

// TestReferenceTraceReviewerRejectionAndReReview is Phase 3 reference
// trace 5: a reject verdict blocks completion; a fix task's integration
// produces a new head; the stale verdict can never satisfy the guard
// again; a fresh approval on the new head completes the run.
func TestReferenceTraceReviewerRejectionAndReReview(t *testing.T) {
	r := run.Run{ID: testRunID, RepositoryID: testRepositoryID, Sequence: 1, State: run.RunRunning, PlanClosed: true}
	reviewTask1 := run.NewReviewTask(testReviewTaskID, testRunID, 3, "head-v1-commit", "head-v1-tree", epoch())
	reviewAttempt1, err := run.NewAttempt(testReviewAttemptID, reviewTask1.ID, 1, epoch())
	mustNoError(t, err)
	reviewTask1, err = reviewTask1.Activate(later())
	mustNoError(t, err)
	reviewAttempt1, err = reviewAttempt1.Launch(later())
	mustNoError(t, err)
	reviewAttempt1, err = reviewAttempt1.MarkRunning(later())
	mustNoError(t, err)

	// Review R1 verdict reject accepted (R1 completed;
	// EvaluateReadiness false — shortfall names the reject).
	rejectSubmission := run.ReviewSubmission{ID: testReviewID, SubjectCommitOID: "head-v1-commit", SubjectTreeOID: "head-v1-tree", Verdict: run.VerdictReject, ReasonsDigest: "reasons-reject"}
	verdictOutcome, err := run.AcceptVerdict(r, reviewTask1, reviewAttempt1, nil, run.ReviewAcceptanceContext{IncarnationCurrent: true, MailboxClear: true}, rejectSubmission, later())
	mustNoError(t, err)
	r, reviewTask1, reviewAttempt1 = verdictOutcome.Run, verdictOutcome.Task, verdictOutcome.Attempt
	review1 := verdictOutcome.Review
	mustState(t, "reviewTask1", string(reviewTask1.State), string(run.TaskCompleted))
	mustState(t, "reviewAttempt1", string(reviewAttempt1.State), string(run.AttemptCompleted))

	ready, missing := run.EvaluateReadiness(run.GuardContext{
		PlanClosed: true, HeadCommitOID: "head-v1-commit", HeadTreeOID: "head-v1-tree",
		LatestCheck:  &run.CheckReceipt{Passed: true, SubjectCommitOID: "head-v1-commit", SubjectTreeOID: "head-v1-tree"},
		LatestReview: &review1,
	})
	if ready {
		t.Fatal("EvaluateReadiness after a reject: ready = true, want false")
	}
	if !hasShortfall(missing, run.ShortfallVerdictRejected) {
		t.Fatalf("EvaluateReadiness after a reject: missing = %+v, want ShortfallVerdictRejected naming it", missing)
	}

	// Controller info to manager with the reasons artifact path;
	// manager creates fix task F; F completes and integrates -> new
	// head H2.
	fixTask := run.NewImplementTask(testFixTaskID, testRunID, 4, "fix", "digest-fix", false, later())
	fixTask, err = fixTask.Activate(later())
	mustNoError(t, err)
	fixAttempt, err := run.NewAttempt(testFixAttemptID, fixTask.ID, 1, later())
	mustNoError(t, err)
	fixAttempt, err = fixAttempt.Launch(later())
	mustNoError(t, err)
	fixAttempt, err = fixAttempt.MarkRunning(later())
	mustNoError(t, err)
	fixSubmission := run.ResultSubmission{ID: testResultID, CommitOID: "fix-commit", Summary: "fix", Digest: "digest-fix-1"}
	resultOutcome, err := run.AcceptResult(r, fixTask, fixAttempt, nil, run.AcceptanceContext{IncarnationCurrent: true, MailboxClear: true}, fixSubmission, later())
	mustNoError(t, err)
	r, fixTask, fixAttempt = resultOutcome.Run, resultOutcome.Task, resultOutcome.Attempt
	fixAttempt, err = fixAttempt.EnterChecking(later())
	mustNoError(t, err)
	fixAttempt, err = fixAttempt.Complete(later())
	mustNoError(t, err)
	mustState(t, "fixAttempt", string(fixAttempt.State), string(run.AttemptCompleted))
	fixTask, err = fixTask.Complete(later())
	mustNoError(t, err)
	fixTask, err = fixTask.EnterIntegrating(later())
	mustNoError(t, err)
	fixIntegration := run.NewIntegration(testFixIntegrationID, testRunID, fixTask.ID, testResultID, "fix-commit", "head-v1-commit", later())
	fixIntegration, err = fixIntegration.EnterChecking("head-v2-commit", later())
	mustNoError(t, err)
	fixIntegration, err = fixIntegration.Integrate(later())
	mustNoError(t, err)
	mustState(t, "fixIntegration", string(fixIntegration.State), string(run.IntegrationIntegrated))
	fixTask, err = fixTask.Integrate(later())
	mustNoError(t, err)
	mustState(t, "fixTask", string(fixTask.State), string(run.TaskIntegrated))

	// Controller creates review task R2 with subject H2; R1's verdict
	// is now bound to a stale subject by construction (subject != head)
	// and can never satisfy the guard — whether reported as the reject
	// it always was, or as a stale subject had it been an approve
	// (TestEvaluateReadinessStaleSubjectApprove pins that comparison
	// directly), it is never ready.
	reviewTask2 := run.NewReviewTask(testSecondReviewTaskID, testRunID, 5, "head-v2-commit", "head-v2-tree", later())
	staleReady, staleMissing := run.EvaluateReadiness(run.GuardContext{
		PlanClosed: true, HeadCommitOID: "head-v2-commit", HeadTreeOID: "head-v2-tree",
		LatestCheck:  &run.CheckReceipt{Passed: true, SubjectCommitOID: "head-v2-commit", SubjectTreeOID: "head-v2-tree"},
		LatestReview: &review1,
	})
	if staleReady {
		t.Fatalf("EvaluateReadiness with R1's stale verdict against the new head: ready = true, missing = %+v, want not ready", staleMissing)
	}

	// R2 approve on H2 -> run completes.
	reviewAttempt2, err := run.NewAttempt(identity.AttemptID("44444444-4444-4444-8444-444444444448"), reviewTask2.ID, 1, later())
	mustNoError(t, err)
	reviewTask2, err = reviewTask2.Activate(later())
	mustNoError(t, err)
	reviewAttempt2, err = reviewAttempt2.Launch(later())
	mustNoError(t, err)
	reviewAttempt2, err = reviewAttempt2.MarkRunning(later())
	mustNoError(t, err)
	approveSubmission := run.ReviewSubmission{ID: testSecondReviewID, SubjectCommitOID: "head-v2-commit", SubjectTreeOID: "head-v2-tree", Verdict: run.VerdictApprove, ReasonsDigest: "reasons-approve"}
	verdictOutcome2, err := run.AcceptVerdict(r, reviewTask2, reviewAttempt2, nil, run.ReviewAcceptanceContext{IncarnationCurrent: true, MailboxClear: true}, approveSubmission, later())
	mustNoError(t, err)
	r = verdictOutcome2.Run
	review2 := verdictOutcome2.Review

	finalReady, finalMissing := run.EvaluateReadiness(run.GuardContext{
		PlanClosed: true, HeadCommitOID: "head-v2-commit", HeadTreeOID: "head-v2-tree",
		LatestCheck:  &run.CheckReceipt{Passed: true, SubjectCommitOID: "head-v2-commit", SubjectTreeOID: "head-v2-tree"},
		LatestReview: &review2,
	})
	if !finalReady {
		t.Fatalf("EvaluateReadiness after R2 approves H2: ready = false, missing = %+v", finalMissing)
	}
	r, err = r.EnterCompleting(later())
	mustNoError(t, err)
	r, err = r.Complete(later())
	mustNoError(t, err)
	mustState(t, "run", string(r.State), string(run.RunCompleted))
}

// TestReferenceTraceStopPrecedenceAcrossNewMachinery is Phase 3 reference
// trace 6: stop precedence during integration, in both the merging and
// checking windows.
func TestReferenceTraceStopPrecedenceAcrossNewMachinery(t *testing.T) {
	t.Run("stop in the merging window: interrupted, never integrated", func(t *testing.T) {
		integration := run.NewIntegration(testIntegrationID, testRunID, testTaskID, testResultID, "source-oid", "premerge-oid", epoch())
		task := run.NewImplementTask(testTaskID, testRunID, 1, "A", "digest-a", false, epoch())
		task, err := task.Activate(later())
		mustNoError(t, err)
		task, err = task.EnterChecking(later())
		mustNoError(t, err)
		task, err = task.Complete(later())
		mustNoError(t, err)
		task, err = task.EnterIntegrating(later())
		mustNoError(t, err)

		r := run.Run{ID: testRunID, State: run.RunRunning}
		r = r.RequestStop(later())
		mustState(t, "run", string(r.State), string(run.RunStopping))

		// The merge command's group is canceled and retired; the
		// integration settles interrupted (no candidate was ever
		// published).
		integration, err = integration.Interrupt(later())
		mustNoError(t, err)
		task, err = task.Interrupt(later())
		mustNoError(t, err)
		mustState(t, "integration", string(integration.State), string(run.IntegrationInterrupted))
		mustState(t, "task", string(task.State), string(run.TaskInterrupted))

		// A passing combined check that lands after the stop request
		// is evidence, never a route back to integrated.
		if _, err := integration.Integrate(later()); !errors.Is(err, run.ErrInvalidTransition) {
			t.Fatalf("Integrate after interrupted: error = %v, want ErrInvalidTransition", err)
		}
	})

	t.Run("stop in the checking window with a published candidate: rolled back", func(t *testing.T) {
		integration := run.NewIntegration(testIntegrationID, testRunID, testTaskID, testResultID, "source-oid", "premerge-oid", epoch())
		integration, err := integration.EnterChecking("merge-oid", later())
		mustNoError(t, err)
		task := run.NewImplementTask(testTaskID, testRunID, 1, "A", "digest-a", false, epoch())
		task, err = task.Activate(later())
		mustNoError(t, err)
		task, err = task.EnterChecking(later())
		mustNoError(t, err)
		task, err = task.Complete(later())
		mustNoError(t, err)
		task, err = task.EnterIntegrating(later())
		mustNoError(t, err)

		r := run.Run{ID: testRunID, State: run.RunRunning}
		r = r.RequestStop(later())
		mustState(t, "run", string(r.State), string(run.RunStopping))

		// DriveStop drives the reset (rollback commit, CAS) so the
		// integration ref never rests on an unvalidated candidate in
		// a stopped run; the integration settles rolled-back and the
		// task interrupted by stop precedence.
		integration, err = integration.RollBack(later())
		mustNoError(t, err)
		task, err = task.Interrupt(later())
		mustNoError(t, err)
		mustState(t, "integration", string(integration.State), string(run.IntegrationRolledBack))
		mustState(t, "task", string(task.State), string(run.TaskInterrupted))

		// needs-rework, not interrupted's usual sibling, is never the
		// outcome under stop precedence: the task settled interrupted
		// directly, above, and this is not a valid second move.
		if _, err := task.NeedsRework(later()); !errors.Is(err, run.ErrInvalidTransition) {
			t.Fatalf("NeedsRework after an already-interrupted task: error = %v, want ErrInvalidTransition", err)
		}
	})
}
