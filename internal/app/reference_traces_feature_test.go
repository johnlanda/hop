package app_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// The section 5 reference traces at app level (docs/plan/phase-3-design.md,
// "Reference traces"). Trace 2 (relayed question, no lifecycle movement)
// is pinned end to end by TestMessagingReferenceTraceRelayedQuestion
// (usecase_message_test.go, slice 2a) and is not duplicated here.

// traceHarness bundles the fixture pieces the trace flows share.
type traceHarness struct {
	*integrationFixture
	store *featureStore
}

func newTraceHarness(t *testing.T) *traceHarness {
	t.Helper()
	f := newIntegrationFixture(t, false)
	store := &featureStore{fakeStore: f.tc.Store}
	f.tc.Controller.Reviews = store
	// Runtime-created worktrees materialize in the fake repository at the
	// requested base, so the Phase 2 provenance rule (common-directory
	// equality plus frozen-base HEAD) holds for legitimate creates.
	worktreeN := 0
	f.tc.Runtime.CreateWorktreeFn = func(req app.WorktreeRequest) (app.WorktreeInfo, error) {
		worktreeN++
		path := fmt.Sprintf("/worktrees/wt-%d", worktreeN)
		if err := f.git.registerWorktree(path, req.BaseRef); err != nil {
			return app.WorktreeInfo{}, err
		}
		return app.WorktreeInfo{WorkspaceID: fmt.Sprintf("ws-wt-%d", worktreeN), Path: path, Branch: req.Branch}, nil
	}
	return &traceHarness{integrationFixture: f, store: store}
}

// tick advances the deterministic clock one second (so ordering by
// UpdatedAt is decidable across steps) and extends the lease so the
// advance never expires it mid-trace.
func (h *traceHarness) tick(t *testing.T) {
	t.Helper()
	h.tc.Clock.Advance(1_000_000_000)
	if err := h.tc.Controller.Heartbeat(context.Background(), h.fr.Handle); err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}
}

// createTask drives the manager's real task-create verb.
func (h *traceHarness) createTask(t *testing.T, title, requestID string, deps ...identity.TaskID) identity.TaskID {
	t.Helper()
	h.tick(t)
	created, err := h.tc.Store.CreateTask(context.Background(), app.TaskCreate{
		ID: mintTaskID(t, h.tc), RunID: h.fr.RunID, Session: h.fr.ManagerID,
		IncarnationID: h.fr.ManagerIncarnation, Title: title, InstructionsDigest: "d",
		DependsOn: deps, RequestID: requestID,
	})
	if err != nil || created.Outcome != app.WorkflowAccepted {
		t.Fatalf("CreateTask(%s) = %+v err=%v", title, created, err)
	}
	return created.TaskID
}

// settleAssigned settles one just-assigned worker's launch claim and
// activates its session, returning the worker fixture.
func (h *traceHarness) settleAssigned(t *testing.T, assigned *app.AssignedTask, pid int) workerFixture {
	t.Helper()
	binding, ok := h.tc.Store.currentBindingLocked(assigned.SessionID)
	if !ok {
		t.Fatalf("no binding for assigned session %s", assigned.SessionID)
	}
	h.tc.Store.LaunchClaims[binding.IncarnationID] = app.LaunchClaim{
		IncarnationID: binding.IncarnationID, RunID: h.fr.RunID, SessionID: assigned.SessionID,
		AttemptID: assigned.AttemptID, Executable: "/usr/local/bin/claude", ArgvDigest: "d",
		PID: pid, State: app.LaunchClaimExeced, ClaimedAt: h.tc.Clock.Now(),
	}
	sessRow := h.tc.Store.Sessions[assigned.SessionID]
	active, err := sessRow.value.ConfirmActive(h.tc.Clock.Now())
	if err != nil {
		t.Fatalf("ConfirmActive() error = %v", err)
	}
	sessRow.value = active
	sessRow.revision++
	return workerFixture{SessionID: assigned.SessionID, IncarnationID: binding.IncarnationID, AttemptID: assigned.AttemptID, PaneID: binding.PaneID, PID: pid}
}

// assignOne assigns exactly one ready task and settles its launch.
func (h *traceHarness) assignOne(t *testing.T, taskID identity.TaskID, pid int) workerFixture {
	t.Helper()
	h.tick(t)
	head, err := h.tc.Controller.ResolveIntegrationHead(context.Background(), h.fr.Handle)
	if err != nil {
		t.Fatalf("ResolveIntegrationHead() error = %v", err)
	}
	opts := defaultAssignmentOptions()
	opts.IntegrationHeadCommitOID = head
	report, err := h.tc.Controller.AssignReadyTasks(context.Background(), h.fr.Handle, opts)
	if err != nil {
		t.Fatalf("AssignReadyTasks() error = %v", err)
	}
	for i := range report.Assigned {
		if report.Assigned[i].TaskID == taskID {
			return h.settleAssigned(t, &report.Assigned[i], pid)
		}
	}
	t.Fatalf("task %s was not assigned; report = %+v", taskID, report)
	return workerFixture{}
}

// submitResult submits one worker result through the acceptance wrapper.
func (h *traceHarness) submitResult(t *testing.T, w workerFixture, taskID identity.TaskID, commit string) {
	t.Helper()
	h.tick(t)
	resultID, err := identity.ParseResultID(h.tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse result id: %v", err)
	}
	outcome, err := h.store.SubmitResult(context.Background(), app.ResultSubmission{
		ID: resultID, RunID: h.fr.RunID, TaskID: taskID, AttemptID: w.AttemptID,
		IncarnationID: w.IncarnationID, CommitOID: commit, Summary: "done", Digest: "digest-" + commit,
	})
	if err != nil || outcome.Kind != app.SubmissionAccepted {
		t.Fatalf("SubmitResult(%s) = %+v err=%v", commit, outcome, err)
	}
}

// retireAbsent retires settled sessions with every pane observed absent.
func (h *traceHarness) retireAbsent(t *testing.T) app.RetirementReport {
	t.Helper()
	h.tick(t)
	h.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound }
	report, err := h.tc.Controller.RetireSettledSessions(context.Background(), h.fr.Handle)
	if err != nil {
		t.Fatalf("RetireSettledSessions() error = %v", err)
	}
	return report
}

// checkPasses drives the per-task feature check to a pass for taskID.
func (h *traceHarness) checkPasses(t *testing.T, taskID identity.TaskID) {
	t.Helper()
	h.tick(t)
	report, err := h.tc.Controller.DriveFeatureChecks(context.Background(), h.fr.Handle, integrationHopPath, nil)
	if err != nil {
		t.Fatalf("DriveFeatureChecks() error = %v", err)
	}
	if !report.Ran || !report.Passed || report.TaskID != taskID.String() {
		t.Fatalf("DriveFeatureChecks() = %+v, want a passing run for %s", report, taskID)
	}
}

// completeTaskThroughIntegration takes a ready task through worker,
// submission, retirement, per-task check and serial integration.
func (h *traceHarness) completeTaskThroughIntegration(t *testing.T, taskID identity.TaskID, pid int) string {
	t.Helper()
	h.tick(t)
	w := h.assignOne(t, taskID, pid)
	head, err := h.tc.Controller.ResolveIntegrationHead(context.Background(), h.fr.Handle)
	if err != nil {
		t.Fatalf("ResolveIntegrationHead() error = %v", err)
	}
	src := h.git.newCommit("tree-src-"+taskID.String()[:8], head)
	h.submitResult(t, w, taskID, src)
	h.retireAbsent(t)
	h.checkPasses(t, taskID)
	h.driveUntil(t, string(run.IntegrationIntegrated), 6)
	if got := h.tc.Store.Tasks[taskID].value.State; got != run.TaskIntegrated {
		t.Fatalf("task %s state = %s, want integrated", taskID, got)
	}
	newHead, err := h.tc.Controller.ResolveIntegrationHead(context.Background(), h.fr.Handle)
	if err != nil {
		t.Fatalf("ResolveIntegrationHead() error = %v", err)
	}
	return newHead
}

// approveThroughReview creates the review task, assigns a reviewer and
// accepts a verdict against the current head.
func (h *traceHarness) approveThroughReview(t *testing.T, verdict string, pid int) (identity.TaskID, app.SubmitReviewResult) {
	t.Helper()
	h.tick(t)
	created, err := h.tc.Controller.EnsureReviewTask(context.Background(), h.fr.Handle)
	if err != nil || !created {
		t.Fatalf("EnsureReviewTask() = %v err=%v", created, err)
	}
	reviewTaskID, ok := findReviewTask(h.tc, h.fr.RunID)
	if !ok {
		t.Fatalf("no review task")
	}
	// A completed review task from an earlier head may exist; find the
	// READY one.
	for id, row := range h.tc.Store.Tasks {
		if row.value.RunID == h.fr.RunID && row.value.Kind == run.TaskKindReview && row.value.State == run.TaskReady {
			reviewTaskID = id
		}
	}
	h.tc.Runtime.InspectPaneFn = nil
	reviewer := h.assignOne(t, reviewTaskID, pid)
	subject := h.tc.Store.Tasks[reviewTaskID].value.SubjectCommitOID
	result, err := h.tc.Controller.SubmitReviewVerdict(context.Background(), app.SubmitReviewRequest{
		RunID: h.fr.RunID.String(), TaskID: reviewTaskID.String(), AttemptID: reviewer.AttemptID.String(),
		SessionID: reviewer.SessionID.String(), IncarnationID: reviewer.IncarnationID.String(),
		Verdict: verdict, SubjectCommitOID: subject, ReasonsBody: []byte("reviewed: " + verdict),
	})
	if err != nil || result.Outcome != string(app.ReviewAccepted) {
		t.Fatalf("SubmitReviewVerdict(%s) = %+v err=%v", verdict, result, err)
	}
	return reviewTaskID, result
}

func TestFeatureTraceHappyPathDependencyRelease(t *testing.T) {
	h := newTraceHarness(t)
	// The fixture's pre-seeded completed task is not part of this trace.
	delete(h.tc.Store.Tasks, h.taskID)
	delete(h.tc.Store.Attempts, h.attemptID)
	delete(h.tc.Store.Results, h.attemptID)

	// Manager: CreateTask A (no deps -> ready), B (depends on A ->
	// pending), then ClosePlan.
	taskA := h.createTask(t, "A", "tr1-a")
	taskB := h.createTask(t, "B", "tr1-b", taskA)
	if got := h.tc.Store.Tasks[taskA].value.State; got != run.TaskReady {
		t.Fatalf("task A state = %s, want ready at creation", got)
	}
	if got := h.tc.Store.Tasks[taskB].value.State; got != run.TaskPending {
		t.Fatalf("task B state = %s, want pending behind its dependency", got)
	}
	closed, err := h.tc.Store.ClosePlan(context.Background(), app.PlanClose{
		RunID: h.fr.RunID, Session: h.fr.ManagerID, IncarnationID: h.fr.ManagerIncarnation, RequestID: "tr1-close",
	})
	if err != nil || closed.Outcome != app.WorkflowAccepted {
		t.Fatalf("ClosePlan() = %+v err=%v", closed, err)
	}

	// A assigned, works, submits; the acceptance closes its mailbox and
	// the retirement frees the slot only on observed termination — the
	// fixture principal stays alive at its composer first.
	wA := h.assignOne(t, taskA, 6001)
	srcA := h.git.newCommit("tree-srcA", h.base)
	h.submitResult(t, wA, taskA, srcA)
	composerOccupant(h.tc, wA)
	report, err := h.tc.Controller.RetireSettledSessions(context.Background(), h.fr.Handle)
	if err != nil {
		t.Fatalf("RetireSettledSessions() error = %v", err)
	}
	if len(report.Retired) != 0 {
		t.Fatalf("the composer-idling worker was declared retired on dispatch alone")
	}
	if got := h.retireAbsent(t); len(got.Retired) != 1 {
		t.Fatalf("retirement after observed absence = %+v", got)
	}

	// Per-task check passes; integration A publishes and the combined
	// check passes; B releases IN the integrating transaction.
	h.checkPasses(t, taskA)
	h.driveUntil(t, string(run.IntegrationIntegrated), 6)
	if got := h.tc.Store.Tasks[taskB].value.State; got != run.TaskReady {
		t.Fatalf("task B state = %s, want released with A's integration", got)
	}
	headAfterA, err := h.tc.Controller.ResolveIntegrationHead(context.Background(), h.fr.Handle)
	if err != nil {
		t.Fatalf("ResolveIntegrationHead() error = %v", err)
	}
	if headAfterA == h.base {
		t.Fatalf("integration head never moved for A")
	}

	// B completes the same way.
	h.tc.Runtime.InspectPaneFn = nil
	wB := h.assignOne(t, taskB, 6002)
	srcB := h.git.newCommit("tree-srcB", headAfterA)
	h.submitResult(t, wB, taskB, srcB)
	h.retireAbsent(t)
	h.checkPasses(t, taskB)
	h.driveUntil(t, string(run.IntegrationIntegrated), 6)

	// Review task created ready once the plan is closed and every
	// implement task integrated; reviewer approves; reviewer retired the
	// same way; completion re-validates and completes.
	_, _ = h.approveThroughReview(t, "approve", 6003)
	if got := h.retireAbsent(t); len(got.Retired) == 0 {
		t.Fatalf("the reviewer was not retired on its accepted verdict")
	}
	completion, err := h.tc.Controller.DriveCompletion(context.Background(), h.fr.Handle)
	if err != nil {
		t.Fatalf("DriveCompletion() error = %v", err)
	}
	if !completion.Completed {
		t.Fatalf("DriveCompletion() = %+v, want completed", completion)
	}

	// Every guard row is present in the store; nothing was typed into a
	// pane anywhere in the trace.
	integrated := 0
	for _, row := range h.tc.Store.Integrations {
		if row.value.State == run.IntegrationIntegrated {
			integrated++
		}
	}
	if integrated != 2 {
		t.Fatalf("integrated integrations = %d, want A and B", integrated)
	}
	if len(h.tc.Store.Reviews) != 1 {
		t.Fatalf("review rows = %d, want the approve verdict", len(h.tc.Store.Reviews))
	}
	if len(h.tc.Runtime.SentText) != 0 {
		t.Fatalf("SendText was called; the feature workflow must never type into a pane")
	}
	if got := h.tc.Store.Runs[h.fr.RunID].value.State; got != run.RunCompleted {
		t.Fatalf("run state = %s, want completed", got)
	}
}

func TestFeatureTraceDuplicateAndAmbiguousDelivery(t *testing.T) {
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	taskB := seedImplementTask(t, tc, fr.RunID, 1, "B", false, run.TaskReady)
	workerID, workerIncarnation := seedWorkerSession(t, tc, fr, taskB)
	ctx := context.Background()

	// m1 and m2 queued to task B.
	var messageIDs []identity.MessageID
	for i, requestID := range []string{"tr3-m1", "tr3-m2"} {
		outcome, err := tc.Store.SendMessage(ctx, app.MessageSend{
			ID: mintMessageID(t, tc), RunID: fr.RunID,
			Sender: run.SessionPrincipal(fr.ManagerID), SenderAddress: run.ManagerAddress(),
			IncarnationID: fr.ManagerIncarnation, Recipient: run.TaskAddress(taskB),
			Kind: run.MessageInfo, BodyPath: "/state/b", BodyDigest: "d", BodyBytes: int64(i + 1), RequestID: requestID,
		})
		if err != nil || outcome.Kind != app.MessageAccepted {
			t.Fatalf("SendMessage(%s) = %+v err=%v", requestID, outcome, err)
		}
		messageIDs = append(messageIDs, outcome.MessageID)
	}

	// The worker fetches m1 (delivered), then dies before acking.
	first, ok, err := tc.Store.FetchNextMessage(ctx, app.MessageFetch{
		RunID: fr.RunID, SessionID: workerID, IncarnationID: workerIncarnation, Address: run.TaskAddress(taskB),
	})
	if err != nil || !ok || first.Message.ID != messageIDs[0] {
		t.Fatalf("first fetch = %+v ok=%v err=%v, want m1", first, ok, err)
	}

	// Cold relaunch: a successor session on the same attempt (same task
	// address), a fresh incarnation; the predecessor's binding is
	// superseded.
	history := tc.Store.Bindings[workerID]
	superseded, err := history[len(history)-1].Supersede("worker killed; cold relaunch", tc.Clock.Now())
	if err != nil {
		t.Fatalf("Supersede() error = %v", err)
	}
	history[len(history)-1] = superseded
	tc.Store.Bindings[workerID] = history
	oldSession := tc.Store.Sessions[workerID]
	lost, err := oldSession.value.Reconcile(tc.Clock.Now())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if lost, err = lost.MarkLost(tc.Clock.Now()); err != nil {
		t.Fatalf("MarkLost() error = %v", err)
	}
	oldSession.value = lost
	oldSession.revision++

	successorID, err := identity.ParseSessionID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse session id: %v", err)
	}
	attemptID := tc.Store.Sessions[workerID].value.AttemptID
	successor, err := run.NewChildSession(successorID, fr.RunID, attemptID, run.RoleImplementer, tc.Store.Sessions[fr.ManagerID].value, run.HarnessClaude, tc.Clock.Now())
	if err != nil {
		t.Fatalf("NewChildSession() error = %v", err)
	}
	if successor, err = successor.Launch(tc.Clock.Now()); err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	if successor, err = successor.ConfirmActive(tc.Clock.Now()); err != nil {
		t.Fatalf("ConfirmActive() error = %v", err)
	}
	tc.Store.Sessions[successorID] = &entityRow[run.Session]{value: successor, revision: 1}
	newIncarnation, err := identity.ParseIncarnationID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse incarnation id: %v", err)
	}
	tc.Store.Bindings[successorID] = append(tc.Store.Bindings[successorID], run.NewRuntimeBinding(successorID, newIncarnation, "", "peer-pid:1", "ws-w", "tab-w", "pane-w2", "label-w2", run.LaunchResume, tc.Clock.Now()))

	// The new worker's fetch re-serves the SAME m1: a second delivery row.
	reserved, ok, err := tc.Store.FetchNextMessage(ctx, app.MessageFetch{
		RunID: fr.RunID, SessionID: successorID, IncarnationID: newIncarnation, Address: run.TaskAddress(taskB),
	})
	if err != nil || !ok || reserved.Message.ID != messageIDs[0] {
		t.Fatalf("re-serve fetch = %+v ok=%v err=%v, want m1 again", reserved, ok, err)
	}
	if got := len(tc.Store.MessageDeliveries[messageIDs[0]]); got != 2 {
		t.Fatalf("delivery rows for m1 = %d, want the at-least-once pair", got)
	}

	// The old incarnation's late ack is refused stale; the new worker's
	// ack is accepted; a second ack is duplicate; the next fetch serves m2.
	late, err := tc.Store.AckMessage(ctx, app.MessageAck{RunID: fr.RunID, MessageID: messageIDs[0], SessionID: workerID, IncarnationID: workerIncarnation})
	if err != nil || late.Kind != app.AckRefused {
		t.Fatalf("late ack = %+v err=%v, want refused", late, err)
	}
	accepted, err := tc.Store.AckMessage(ctx, app.MessageAck{RunID: fr.RunID, MessageID: messageIDs[0], SessionID: successorID, IncarnationID: newIncarnation})
	if err != nil || accepted.Kind != app.AckAccepted {
		t.Fatalf("new ack = %+v err=%v, want accepted", accepted, err)
	}
	dup, err := tc.Store.AckMessage(ctx, app.MessageAck{RunID: fr.RunID, MessageID: messageIDs[0], SessionID: successorID, IncarnationID: newIncarnation})
	if err != nil || dup.Kind != app.AckDuplicate {
		t.Fatalf("second ack = %+v err=%v, want duplicate", dup, err)
	}
	next, ok, err := tc.Store.FetchNextMessage(ctx, app.MessageFetch{
		RunID: fr.RunID, SessionID: successorID, IncarnationID: newIncarnation, Address: run.TaskAddress(taskB),
	})
	if err != nil || !ok || next.Message.ID != messageIDs[1] {
		t.Fatalf("next fetch = %+v ok=%v err=%v, want m2", next, ok, err)
	}
}

func TestFeatureTraceWorkerInterruptionAndRetry(t *testing.T) {
	t.Run("interruption, retry provenance across both attempts", func(t *testing.T) {
		h := newTraceHarness(t)
		delete(h.tc.Store.Tasks, h.taskID)
		delete(h.tc.Store.Attempts, h.attemptID)
		delete(h.tc.Store.Results, h.attemptID)
		taskB := h.createTask(t, "B", "tr4-b")
		w1 := h.assignOne(t, taskB, 7001)

		// The worker is killed: reconciliation establishes absence and, in
		// one transaction, interrupts the attempt, reworks the task,
		// terminates the session and notifies the manager.
		report := h.retireAbsent(t)
		if len(report.Retired) != 1 {
			t.Fatalf("interruption report = %+v", report)
		}
		if got := h.tc.Store.Attempts[w1.AttemptID].value.State; got != run.AttemptInterrupted {
			t.Fatalf("attempt 1 state = %s, want interrupted", got)
		}
		if got := h.tc.Store.Tasks[taskB].value.State; got != run.TaskNeedsRework {
			t.Fatalf("task state = %s, want needs-rework with budget room", got)
		}
		if got := h.tc.Store.Sessions[w1.SessionID].value.State; got != run.SessionTerminated {
			t.Fatalf("session 1 state = %s, want terminated", got)
		}
		if len(h.managerNotices()) == 0 {
			t.Fatalf("no manager notice for the interruption")
		}

		// Manager retries; the retry reserves attempt 2 immediately, the
		// assignment launches it; B2 completes; both attempts' provenance
		// stands.
		accepted, err := h.tc.Store.RequestRetry(context.Background(), app.RetryRequest{
			TaskID: taskB, RunID: h.fr.RunID, Session: h.fr.ManagerID,
			IncarnationID: h.fr.ManagerIncarnation, Reason: "worker died", RequestID: "tr4-retry",
		})
		if err != nil || accepted.Outcome != app.WorkflowAccepted || accepted.AttemptNumber != 2 {
			t.Fatalf("RequestRetry() = %+v err=%v, want attempt 2 reserved", accepted, err)
		}
		h.tc.Runtime.InspectPaneFn = nil
		w2 := h.assignOne(t, taskB, 7002)
		if w2.AttemptID == w1.AttemptID {
			t.Fatalf("the retry reused attempt 1; provenance requires a fresh attempt")
		}
		src := h.git.newCommit("tree-src-b2", h.base)
		h.submitResult(t, w2, taskB, src)
		h.retireAbsent(t)
		h.checkPasses(t, taskB)
		if got := h.tc.Store.Tasks[taskB].value.State; got != run.TaskCompleted {
			t.Fatalf("task state = %s, want completed on attempt 2", got)
		}
		if got := h.tc.Store.Sessions[w1.SessionID].value.State; got != run.SessionTerminated {
			t.Fatalf("attempt 1's session lineage was disturbed: %s", got)
		}
		if got := h.tc.Store.Attempts[w1.AttemptID].value.State; got != run.AttemptInterrupted {
			t.Fatalf("attempt 1's provenance was disturbed: %s", got)
		}
	})

	t.Run("exhaustion variant: the settling transaction moves the task directly to failed", func(t *testing.T) {
		h := newTraceHarness(t)
		delete(h.tc.Store.Tasks, h.taskID)
		delete(h.tc.Store.Attempts, h.attemptID)
		delete(h.tc.Store.Results, h.attemptID)
		snap := h.tc.Store.Snapshots[h.fr.RunID]
		snap.Workflow.RetryLimit = 1
		h.tc.Store.Snapshots[h.fr.RunID] = snap
		taskB := h.createTask(t, "B", "tr4x-b")
		h.assignOne(t, taskB, 7003)

		report := h.retireAbsent(t)
		if len(report.Retired) != 1 {
			t.Fatalf("interruption report = %+v", report)
		}
		task := h.tc.Store.Tasks[taskB].value
		if task.State != run.TaskFailed {
			t.Fatalf("task state = %s, want failed directly at the exhausted limit (never parked needs-rework)", task.State)
		}
		if !task.MailboxClosed {
			t.Fatalf("failed task's mailbox open; failure closes admission in the same commit")
		}
		// The run fails once owned-work termination is observed.
		report, err := h.tc.Controller.RetireSettledSessions(context.Background(), h.fr.Handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() error = %v", err)
		}
		if !report.RunFailed {
			t.Fatalf("report = %+v, want the run failed after termination observed", report)
		}
		if got := h.tc.Store.Runs[h.fr.RunID].value.State; got != run.RunFailed {
			t.Fatalf("run state = %s, want failed", got)
		}
	})
}

func TestFeatureTraceReviewerRejectionAndReReview(t *testing.T) {
	h := newTraceHarness(t)
	closePlanDirectly(t, h.tc, h.fr.RunID)
	h.driveUntil(t, string(run.IntegrationIntegrated), 6)
	headH1, err := h.tc.Controller.ResolveIntegrationHead(context.Background(), h.fr.Handle)
	if err != nil {
		t.Fatalf("ResolveIntegrationHead() error = %v", err)
	}

	// R1 rejects: the review task completes, readiness fails with the
	// reject named, the notice carries the reasons artifact path.
	r1Task, r1 := h.approveThroughReview(t, "reject", 8001)
	if got := h.tc.Store.Tasks[r1Task].value.State; got != run.TaskCompleted {
		t.Fatalf("review task state = %s, want completed on reject too", got)
	}
	ready, missing, err := h.tc.Controller.EvaluateRunReadiness(context.Background(), h.fr.Handle)
	if err != nil || ready {
		t.Fatalf("EvaluateRunReadiness() = %v %v err=%v, want the reject shortfall", ready, missing, err)
	}
	if !strings.Contains(strings.Join(missing, " "), string(run.ShortfallVerdictRejected)) {
		t.Fatalf("shortfalls = %v, want verdict-rejected", missing)
	}
	reasonsPath := "/state/runs/" + h.fr.RunID.String() + "/reviews/" + r1.ReviewID
	noticeFound := false
	for _, m := range h.tc.Store.Messages {
		if m.Sender.Kind == run.PrincipalController && m.Recipient.Kind == run.AddressManager && m.BodyPath == reasonsPath {
			noticeFound = true
		}
	}
	if !noticeFound {
		t.Fatalf("no controller notice with the reasons artifact path")
	}

	// The manager creates fix task F (reopening the plan), F completes and
	// integrates to a new head H2, the plan closes again.
	h.tc.Runtime.InspectPaneFn = nil
	h.retireAbsent(t) // the rejecting reviewer retires on its verdict boundary
	h.tc.Runtime.InspectPaneFn = nil
	fixTask := h.createTask(t, "F", "tr5-f")
	if h.tc.Store.Runs[h.fr.RunID].value.PlanClosed {
		t.Fatalf("plan still closed after CreateTask; creation reopens it")
	}
	closePlanDirectly(t, h.tc, h.fr.RunID)
	headH2 := h.completeTaskThroughIntegration(t, fixTask, 8002)
	if headH2 == headH1 {
		t.Fatalf("the fix task's integration did not move the head")
	}

	// R2 reviews the NEW head; R1's verdict is stale by construction and
	// can never satisfy the guard.
	r2Task, _ := h.approveThroughReview(t, "approve", 8003)
	if r2Task == r1Task {
		t.Fatalf("no new review task was created for the new head")
	}
	if got := h.tc.Store.Tasks[r2Task].value.SubjectCommitOID; got != headH2 {
		t.Fatalf("R2 subject = %s, want the new head %s", got, headH2)
	}
	h.retireAbsent(t)
	completion, err := h.tc.Controller.DriveCompletion(context.Background(), h.fr.Handle)
	if err != nil || !completion.Completed {
		t.Fatalf("DriveCompletion() = %+v err=%v, want completed on R2's approve", completion, err)
	}
}

func TestFeatureTraceStopPrecedenceAcrossIntegration(t *testing.T) {
	// A passing combined check that lands after the stop request records
	// evidence and yields interrupted, never integrated; DriveFeatureStop
	// then drives the reset so the ref never rests on the unvalidated
	// candidate in a stopped run.
	h := newTraceHarness(t)
	closePlanDirectly(t, h.tc, h.fr.RunID)
	h.drive(t) // claim
	h.drive(t) // merge
	h.drive(t) // publish -> checking

	// The stop request lands while the combined check runs.
	h.git.OnCheckExec = func(string, []string) {
		row := h.tc.Store.Runs[h.fr.RunID]
		row.value = row.value.RequestStop(h.tc.Clock.Now())
		row.revision++
	}
	report := h.drive(t)
	if !report.Interrupted {
		t.Fatalf("DriveIntegration() = %+v, want the outcome claimed by stop precedence", report)
	}
	integ := h.currentIntegrationRow(t)
	if integ.State != run.IntegrationChecking {
		t.Fatalf("integration state = %s, want left checking for the stop path", integ.State)
	}

	// The stop path rolls the published candidate back before stopped.
	h.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound }
	stopReport, err := h.tc.Controller.DriveFeatureStop(context.Background(), h.fr.Handle)
	if err != nil {
		t.Fatalf("DriveFeatureStop() error = %v", err)
	}
	if !stopReport.Terminated {
		t.Fatalf("stop = %+v, want terminated with panes absent", stopReport)
	}
	integ = h.currentIntegrationRow(t)
	if integ.State != run.IntegrationRolledBack {
		t.Fatalf("integration state = %s, want rolled-back — never integrated past a stop", integ.State)
	}
	head := h.git.ref(integrationRefName)
	if head == integ.MergeCommitOID {
		t.Fatalf("the ref rests on the unvalidated candidate in a stopped run")
	}
	if got := h.tc.Store.Tasks[h.taskID].value.State; got != run.TaskInterrupted {
		t.Fatalf("task state = %s, want interrupted by stop precedence", got)
	}
	if got := h.tc.Store.Sessions[h.fr.ManagerID].value.State; got != run.SessionTerminated {
		t.Fatalf("manager state = %s, want closed under the close rule", got)
	}
	if got := h.tc.Store.Runs[h.fr.RunID].value.State; got != run.RunStopped {
		t.Fatalf("run state = %s, want stopped only on observed absence", got)
	}
	// The check's evidence was recorded even though the outcome yielded to
	// stop.
	evidence := 0
	for _, a := range h.tc.Store.Artifacts {
		if a.Kind == run.ArtifactCheckStdout || a.Kind == run.ArtifactCheckStderr {
			evidence++
		}
	}
	if evidence < 2 {
		t.Fatalf("retained evidence rows = %d, want the interrupted check's output kept", evidence)
	}
}
