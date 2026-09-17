package integration

import (
	"fmt"
	"testing"
)

// TestRealProcessIntegrationConflict is the Layers row's own merge-
// conflict scenario: two independent tasks (no dependency between them,
// so both are eligible and launch concurrently under MaxWorkers=2) each
// use worker-conflict, which writes DIFFERENT content (its own task id)
// to the SAME shared file from its own worktree — both worktrees branched
// from the same, still-unmodified integration head, since attempt
// creation for both happens before either has integrated.
//
// Production does not guarantee WHICH of the two integrates first:
// claimNextIntegration claims the lowest-seq task already completed, a
// scheduling round starts at most once per pass, and both workers retry
// their own transient submit on the same cadence, so which one's submit
// and check round land first is effectively a coin flip. This scenario
// is order-agnostic: whichever task's integration row settles
// "integrated" first is treated as the clean one, and the other — whose
// own merge against the now-advanced head genuinely conflicts on the
// shared file — is the one every conflict/retry assertion below runs
// against.
//
// Asserts: the conflicting task moves integrating -> needs-rework (the
// durable transition, never a live state poll — the scripted manager
// retries automatically on the notice, exactly as workerinterruption_
// test.go's own comment explains); its first integration row (read by
// row order, never the newest — a retry's own fresh row must never be
// mistaken for the conflict outcome) is "conflicted"; the retried
// attempt's fresh worktree bases from the NEW integration head (the
// clean task's own integrated merge commit), verified by object id,
// distinct from the original frozen base attempt 1 used; the final head
// carries a persisted, successful combined-check operation of its own
// (never merely inferred from integration/task states); and the run
// completes (the retry, based on the current head, is a same-side
// change with nothing to conflict against, integrates cleanly, and
// review approves the final head).
func TestRealProcessIntegrationConflict(t *testing.T) {
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
			{Label: "t1", Title: "Implement t1", Behavior: "worker-conflict"},
			{Label: "t2", Title: "Implement t2", Behavior: "worker-conflict"},
		},
		nil, "")
	fx := startFeatureRun(t, artifacts, server, repo, scratchDir, brief)

	t1 := fx.requireTaskBySeq(t, 1)
	t2 := fx.requireTaskBySeq(t, 2)
	// worker-conflict submits at once (no barrier), so both can already be
	// well past "active" by the first poll; wait for the attempt ROW
	// itself rather than racing a specific task state.
	if !waitUntil(func() bool { return fx.attemptCount(t, t1) > 0 && fx.attemptCount(t, t2) > 0 }) {
		t.Fatalf("no attempt ever reserved for both tasks %s, %s", t1, t2)
	}

	// Determine which task integrates first from the store, not from task
	// seq order (production does not enforce it — see the doc comment
	// above).
	var firstTaskID string
	if !waitUntil(func() bool {
		firstTaskID = fx.scalar(t, fmt.Sprintf(
			"SELECT task_id FROM integrations WHERE run_id = '%s' AND state = 'integrated' ORDER BY updated_at ASC, rowid ASC LIMIT 1;", fx.runID))
		return firstTaskID != ""
	}) {
		t.Fatalf("neither task ever integrated for run %s", fx.runID)
	}
	conflictTaskID := t1
	if firstTaskID == t1 {
		conflictTaskID = t2
	}

	// The conflicting task's first attempt worktree was branched from the
	// original frozen base — its own row was written before either task
	// had integrated, so reading it now (after the fact) still reports
	// that original state.
	attempt1ID, attempt1Number := fx.currentAttempt(t, conflictTaskID)
	if attempt1Number != 1 {
		t.Fatalf("the conflicting task's first attempt number = %d, want 1", attempt1Number)
	}
	worktree1Path, worktree1Branch, worktree1Base := fx.requireWorktreeForAttempt(t, attempt1ID)
	if worktree1Path == "" || worktree1Branch == "" {
		t.Fatalf("the conflicting task's attempt 1 worktree row is incomplete: path=%q branch=%q", worktree1Path, worktree1Branch)
	}
	if worktree1Base != repo.Base {
		t.Errorf("the conflicting task's attempt 1 worktree base = %s, want the frozen base commit %s (created before either task integrated)", worktree1Base, repo.Base)
	}

	// The first task, with no competing change yet published, integrated
	// cleanly.
	fx.requireTaskState(t, firstTaskID, "integrated")
	firstMergeOID := fx.scalar(t, fmt.Sprintf("SELECT merge_commit_oid FROM integrations WHERE run_id = '%s' AND task_id = '%s';", fx.runID, firstTaskID))
	if firstMergeOID == "" {
		t.Fatalf("task %s integrated with no recorded merge_commit_oid", firstTaskID)
	}

	// The conflicting task's own merge against the now-advanced head
	// conflicts on the shared file; the durable transition, never a live
	// poll, since the scripted manager retries automatically on the
	// notice (workerinterruption_test.go's own reasoning applies
	// identically here).
	fx.requireTransitionAt(t, "task", conflictTaskID, "needs-rework")
	// The FIRST integration row by row order, never the newest: by the
	// time this reads, the retry the notice above triggers may already
	// have claimed a fresh integration row for the same task, and that
	// row is never "conflicted".
	conflictedState := fx.scalar(t, fmt.Sprintf(
		"SELECT state FROM integrations WHERE run_id = '%s' AND task_id = '%s' ORDER BY rowid ASC LIMIT 1;", fx.runID, conflictTaskID))
	if conflictedState != "conflicted" {
		t.Errorf("%s's first integration state = %q, want \"conflicted\"", conflictTaskID, conflictedState)
	}

	// The retry's fresh worktree bases from the NEW integration head —
	// the first task's own merge commit — verified by object id,
	// distinct from the original frozen base attempt 1 used.
	fx.requireTaskState(t, conflictTaskID, "active")
	attempt2ID, attempt2Number := fx.currentAttempt(t, conflictTaskID)
	if attempt2Number != 2 {
		t.Fatalf("the conflicting task's retried attempt number = %d, want 2", attempt2Number)
	}
	if attempt2ID == attempt1ID {
		t.Fatal("the conflicting task's retried attempt has the same id as the conflicted one")
	}
	worktree2Path, worktree2Branch, worktree2Base := fx.requireWorktreeForAttempt(t, attempt2ID)
	if worktree2Path == worktree1Path || worktree2Branch == worktree1Branch {
		t.Errorf("the conflicting task's attempt 2 reused attempt 1's worktree (path=%q branch=%q); want a fresh one", worktree2Path, worktree2Branch)
	}
	if worktree2Base != firstMergeOID {
		t.Errorf("the conflicting task's attempt 2 worktree base = %s, want the current integration head %s (the first task's own integrated merge commit)", worktree2Base, firstMergeOID)
	}
	if worktree2Base == repo.Base {
		t.Error("the conflicting task's attempt 2 worktree base is still the original frozen base; want the NEW head after the first task integrated")
	}

	// The retry, a same-side change against the head it branched from, has
	// nothing left to conflict against and integrates cleanly; the run
	// completes.
	fx.requireTaskState(t, conflictTaskID, "integrated")
	finalMergeOID := fx.scalar(t, fmt.Sprintf(
		"SELECT merge_commit_oid FROM integrations WHERE run_id = '%s' AND task_id = '%s' ORDER BY rowid DESC LIMIT 1;", fx.runID, conflictTaskID))
	if finalMergeOID == "" {
		t.Fatalf("the conflicting task's final integration has no recorded merge_commit_oid")
	}
	fx.requireCombinedCheckPassed(t, finalMergeOID)
	reviewTaskID := fx.requireReviewTask(t)
	fx.requireTaskState(t, reviewTaskID, "completed")
	if verdict, ok := fx.reviewVerdict(t, reviewTaskID); !ok || verdict != "approve" {
		t.Errorf("review verdict = %q (found=%v), want approve", verdict, ok)
	}
	fx.requireRunState(t, "completed")

	if fx.attemptCount(t, conflictTaskID) != 2 {
		t.Errorf("the conflicting task has %d recorded attempts, want exactly 2", fx.attemptCount(t, conflictTaskID))
	}
}
