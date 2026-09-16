package integration

import (
	"fmt"
	"strings"
	"testing"
)

// TestRealProcessFeatureRunEndToEnd is design section 11 scenario 1 (the
// section 1 evidence row): a fixture manager plans four tasks — t1, t3, t4
// initially eligible, t2 depending on t1 — under MaxWorkers=2, with
// worker-hold barriers keeping t1's and t3's sessions alive at will. It
// asserts, separately: t1 and t3 launch immediately (the intended
// two-worker concurrency); t4, eligible throughout, launches only after a
// barrier is released and that session's retirement is OBSERVED (the slot
// assertion, decoupled from process-exit assumptions); t2 launches only
// after t1 is integrated (the dependency assertion, distinct from mere
// completion); serial integration order; the plan closed before the
// review task appears; review approval; completion with the manager
// retired; every guard row present in the store; and token metadata
// published manager-first (agent.list).
func TestRealProcessFeatureRunEndToEnd(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	worker := buildFixtureWorker(t, artifacts)
	installFixtureWorkerAsClaudeStub(t, server, worker)
	server.start(t)

	scratchDir := artifacts.dir(t, "fixture-scratch")
	repo := newFeatureFixtureRepo(t, artifacts, server, "repo", featureFixtureOptions{
		ScratchDir: scratchDir, ReviewerBehavior: "reviewer-approve",
		MaxWorkers: 2, RetryLimit: 3, MessageWaitTimeout: "3s", MessageAttentionAfter: "30s",
	})

	brief := fixtureManagerBrief(scratchDir,
		[]fixtureManagerTask{
			{Label: "t1", Title: "Implement t1", Behavior: "worker-hold"},
			{Label: "t2", Title: "Implement t2", Behavior: "worker-implement", DependsOn: []string{"t1"}},
			{Label: "t3", Title: "Implement t3", Behavior: "worker-hold"},
			{Label: "t4", Title: "Implement t4", Behavior: "worker-implement"},
		},
		[]fixtureManagerAnswer{{Match: fixtureHoldMarker, Action: "relay"}},
		"",
	)
	fx := startFeatureRun(t, artifacts, server, repo, scratchDir, brief)

	t1 := fx.requireTaskBySeq(t, 1)
	t2 := fx.requireTaskBySeq(t, 2)
	t3 := fx.requireTaskBySeq(t, 3)
	t4 := fx.requireTaskBySeq(t, 4)

	// t1 and t3 launch immediately: a worker-hold task can never progress
	// past "active" until its barrier is released, so observing "active"
	// here is exact, not merely a forward-looking superset check.
	fx.requireTaskState(t, t1, "active")
	fx.requireTaskState(t, t3, "active")
	// With both slots occupied, t4 (eligible from the start) and t2
	// (blocked on t1's dependency) cannot yet be assigned — a hard
	// guarantee from the store's own bounded-concurrency transaction, not
	// a timing race.
	if state := fx.taskState(t, t4); state != "ready" {
		t.Errorf("task t4 state = %q with both worker slots held by t1/t3, want \"ready\" (eligible but slot-blocked)", state)
	}
	if state := fx.taskState(t, t2); state != "pending" {
		t.Errorf("task t2 state = %q before t1 integrated, want \"pending\" (dependency not yet released)", state)
	}

	// Token metadata published manager-first while the manager and both
	// held workers are all live (agent.list, as in Phase 1).
	managerSessionID := fx.managerSessionID(t)
	managerPaneID := fx.requirePane(t, managerSessionID)
	assertManagerFirst(t, server.agentTokens(t), managerPaneID)

	t1AttemptID, _ := fx.currentAttempt(t, t1)
	t1SessionID := fx.sessionForAttempt(t, t1AttemptID)
	t3AttemptID, _ := fx.currentAttempt(t, t3)
	t3SessionID := fx.sessionForAttempt(t, t3AttemptID)

	// Release t1's barrier — matched by ITS OWN relay chain (directive:
	// never by queue order), since t3's barrier is held concurrently and
	// both relay through the same manager to the same human address. The
	// relayed body carries the ORIGINAL barrier content unchanged (the
	// manager relays, never rewrites).
	t1QuestionID := fx.relayedQuestionFor(t, t1SessionID, featureRunTimeout)
	if body := fx.messageBodyContent(t, t1QuestionID); !strings.Contains(body, fixtureHoldMarker) {
		t.Errorf("relayed question %s body = %q, want it to carry %q unchanged", t1QuestionID, body, fixtureHoldMarker)
	}
	fx.answerHuman(t, t1QuestionID, "release t1")
	fx.requireSessionState(t, t1SessionID, "terminated")
	t1SessionTerminatedAt := fx.requireTransitionAt(t, "session", t1SessionID, "terminated")

	// t4 launches only after the barrier is released AND that session's
	// retirement is OBSERVED: its own session must not have begun
	// launching any earlier than t1's session was recorded terminated.
	fx.requireTaskState(t, t4, "active", "checking", "completed", "integrating", "integrated")
	t4AttemptID, _ := fx.currentAttempt(t, t4)
	t4SessionID := fx.sessionForAttempt(t, t4AttemptID)
	t4LaunchingAt := fx.requireTransitionAt(t, "session", t4SessionID, "launching")
	if t4LaunchingAt < t1SessionTerminatedAt {
		t.Errorf("task t4's session %s began launching at %s, before task t1's session %s was observed terminated at %s (slot freed before retirement observed)",
			t4SessionID, t4LaunchingAt, t1SessionID, t1SessionTerminatedAt)
	}

	// t1 integrates; t2's dependency releases in the SAME transaction —
	// the dependency assertion, distinct from t4's slot-wait assertion:
	// t2 must become ready no earlier than t1's own integrated transition.
	fx.requireTaskState(t, t1, "integrated")
	t1IntegratedAt := fx.requireTransitionAt(t, "task", t1, "integrated")
	t2ReadyAt := fx.requireTransitionAt(t, "task", t2, "ready")
	if t2ReadyAt < t1IntegratedAt {
		t.Errorf("task t2 became ready at %s, before task t1 integrated at %s", t2ReadyAt, t1IntegratedAt)
	}
	fx.requireTaskState(t, t2, "active", "checking", "completed", "integrating", "integrated")

	// Release t3's barrier the same way, matched by its own relay chain.
	t3QuestionID := fx.relayedQuestionFor(t, t3SessionID, featureRunTimeout)
	fx.answerHuman(t, t3QuestionID, "release t3")
	fx.requireSessionState(t, t3SessionID, "terminated")

	// Every implement task integrates, in serial order (no two
	// integrations ever overlap, proved empirically from the journal).
	fx.requireTaskState(t, t2, "integrated")
	fx.requireTaskState(t, t3, "integrated")
	fx.requireTaskState(t, t4, "integrated")
	fx.requireSerialIntegrationOrder(t)

	// Plan closed before the review task appears (the manager closes it
	// immediately after creating all four tasks, long before any of them
	// completes, so this is already true by construction — asserted
	// directly against the store rather than assumed).
	reviewTaskID := fx.requireReviewTask(t)
	if !fx.planClosed(t) {
		t.Error("the review task exists but the run's plan flag is not set")
	}

	// Review approves against the current integration head.
	fx.requireTaskState(t, reviewTaskID, "completed")
	verdict, ok := fx.reviewVerdict(t, reviewTaskID)
	if !ok || verdict != "approve" {
		t.Errorf("review verdict = %q (found=%v), want approve", verdict, ok)
	}
	head := fx.integrationHead(t)
	if subject := fx.scalar(t, fmt.Sprintf("SELECT subject_commit_oid FROM reviews WHERE task_id = '%s';", reviewTaskID)); subject != head {
		t.Errorf("review subject commit = %s, want the current integration head %s", subject, head)
	}

	// Completion with the manager retired.
	fx.requireRunState(t, "completed")
	fx.requireSessionState(t, managerSessionID, "terminated")

	// Every guard row present in the store (design section 8): the plan
	// closed, every implement task integrated (a passing combined check
	// is exactly what integration.state=="integrated" means), and the
	// approve verdict bound to the current head — all already asserted
	// above; the plan flag is re-checked here at the run's own terminal
	// state.
	if !fx.planClosed(t) {
		t.Error("run completed with the plan flag not set")
	}
	for _, taskID := range []string{t1, t2, t3, t4} {
		fx.requireIntegrationState(t, taskID, "integrated")
		if state := fx.taskState(t, taskID); state != "integrated" {
			t.Errorf("task %s state at completion = %q, want integrated", taskID, state)
		}
	}
}
