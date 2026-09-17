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
// creation for both happens before either has integrated. Task seq order
// (design section 6: "assign released tasks into free slots (task seq
// order)" and serial integration claiming the same order with no
// dependency to reorder it) makes t1 integrate first, cleanly (only one
// side has diverged from the frozen base); t2's own merge against the
// NOW-ADVANCED head then genuinely conflicts on that same file.
//
// Asserts: t2 moves integrating -> needs-rework (the durable transition,
// never a live state poll — the scripted manager retries automatically on
// the notice, exactly as workerinterruption_test.go's own comment
// explains); the retried attempt's fresh worktree bases from the NEW
// integration head (t1's own integrated merge commit), verified by object
// id, distinct from the original frozen base attempt 1 used; and the run
// completes (t2's retry, based on the current head, is a same-side change
// with nothing to conflict against, integrates cleanly, and review
// approves the final head).
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

	attempt1ID, attempt1Number := fx.currentAttempt(t, t2)
	if attempt1Number != 1 {
		t.Fatalf("t2's first attempt number = %d, want 1", attempt1Number)
	}
	worktree1Path, worktree1Branch, worktree1Base := fx.requireWorktreeForAttempt(t, attempt1ID)
	if worktree1Path == "" || worktree1Branch == "" {
		t.Fatalf("t2 attempt 1's worktree row is incomplete: path=%q branch=%q", worktree1Path, worktree1Branch)
	}
	if worktree1Base != repo.Base {
		t.Errorf("t2 attempt 1's worktree base = %s, want the frozen base commit %s (created before either task integrated)", worktree1Base, repo.Base)
	}

	// t1, with no competing change yet published, integrates cleanly first
	// (task seq order, no dependency to reorder it).
	fx.requireTaskState(t, t1, "integrated")
	t1MergeOID := fx.scalar(t, fmt.Sprintf("SELECT merge_commit_oid FROM integrations WHERE run_id = '%s' AND task_id = '%s';", fx.runID, t1))
	if t1MergeOID == "" {
		t.Fatalf("task %s integrated with no recorded merge_commit_oid", t1)
	}

	// t2's own merge against the now-advanced head conflicts on the shared
	// file; the durable transition, never a live poll, since the scripted
	// manager retries automatically on the notice (workerinterruption_
	// test.go's own reasoning applies identically here).
	fx.requireTransitionAt(t, "task", t2, "needs-rework")
	integrationState, _, ok := fx.integrationForTask(t, t2)
	if !ok || integrationState != "conflicted" {
		t.Errorf("t2's first integration state = %q (found=%v), want \"conflicted\"", integrationState, ok)
	}

	// The retry's fresh worktree bases from the NEW integration head —
	// t1's own merge commit — verified by object id, distinct from the
	// original frozen base attempt 1 used.
	fx.requireTaskState(t, t2, "active")
	attempt2ID, attempt2Number := fx.currentAttempt(t, t2)
	if attempt2Number != 2 {
		t.Fatalf("t2's retried attempt number = %d, want 2", attempt2Number)
	}
	if attempt2ID == attempt1ID {
		t.Fatal("t2's retried attempt has the same id as the conflicted one")
	}
	worktree2Path, worktree2Branch, worktree2Base := fx.requireWorktreeForAttempt(t, attempt2ID)
	if worktree2Path == worktree1Path || worktree2Branch == worktree1Branch {
		t.Errorf("t2 attempt 2 reused attempt 1's worktree (path=%q branch=%q); want a fresh one", worktree2Path, worktree2Branch)
	}
	if worktree2Base != t1MergeOID {
		t.Errorf("t2 attempt 2's worktree base = %s, want the current integration head %s (t1's own integrated merge commit)", worktree2Base, t1MergeOID)
	}
	if worktree2Base == repo.Base {
		t.Error("t2 attempt 2's worktree base is still the original frozen base; want the NEW head after t1 integrated")
	}

	// The retry, a same-side change against the head it branched from, has
	// nothing left to conflict against and integrates cleanly; the run
	// completes.
	fx.requireTaskState(t, t2, "integrated")
	reviewTaskID := fx.requireReviewTask(t)
	fx.requireTaskState(t, reviewTaskID, "completed")
	if verdict, ok := fx.reviewVerdict(t, reviewTaskID); !ok || verdict != "approve" {
		t.Errorf("review verdict = %q (found=%v), want approve", verdict, ok)
	}
	fx.requireRunState(t, "completed")

	if fx.attemptCount(t, t2) != 2 {
		t.Errorf("task t2 has %d recorded attempts, want exactly 2", fx.attemptCount(t, t2))
	}
}
