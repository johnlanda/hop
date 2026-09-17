package integration

import (
	"testing"
)

// workerInterruptionNoticeFirstLine is the EXACT first line
// internal/app/usecase_featurecheck.go's renderTaskNotice renders for a
// live controller's observed self-exit settlement (usecase_retirement.go's
// settleWorkerInterruption -> workerSelfExit's noticeReason), retyped here
// — never derived — so a drift in either renderer fails this scenario
// directly, per the manager directive: assert the exact rendered line
// read from the store, not merely that the fixture's own looser parse
// found a label in it.
const workerInterruptionNoticeFirstLine = "task t1 needs-rework"

// TestRealProcessWorkerInterruption is design section 11 scenario 4
// (reference trace 4): a worker killed mid-attempt is reconciled by the
// LIVE controller (Phase 2 evidence rules — no controller crash, no
// resume involved), the manager receives the controller's info notice and
// retries the task, and attempt 2 completes with full provenance for both
// attempts (worktrees with their attempt link, sessions, launch claims,
// results).
//
// Both attempts use worker-hold so the kill point is fully deterministic
// (a worker-hold worker blocks indefinitely on its own barrier's answer,
// so it can be killed at will without racing its own submission) and so
// attempt 2 — which reuses the SAME task instructions and therefore the
// SAME scripted behavior — is released and completes the same
// message-barrier way FeatureRunEndToEnd already proves.
func TestRealProcessWorkerInterruption(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	worker := buildFixtureWorker(t, artifacts)
	installFixtureWorkerAsClaudeStub(t, server, worker)
	server.start(t)

	scratchDir := artifacts.dir(t, "fixture-scratch")
	repo := newFeatureFixtureRepo(t, artifacts, server, "repo", featureFixtureOptions{
		ScratchDir: scratchDir, ReviewerBehavior: "reviewer-approve",
		MaxWorkers: 1, RetryLimit: 3, MessageWaitTimeout: "3s", MessageAttentionAfter: "30s",
	})

	brief := fixtureManagerBrief(scratchDir,
		[]fixtureManagerTask{{Label: "t1", Title: "Implement t1", Behavior: "worker-hold"}},
		[]fixtureManagerAnswer{{Match: fixtureHoldMarker, Action: "relay"}},
		"",
	)
	fx := startFeatureRun(t, artifacts, server, repo, scratchDir, brief)

	t1 := fx.requireTaskBySeq(t, 1)
	fx.requireTaskState(t, t1, "active")

	attempt1ID, attempt1Number := fx.currentAttempt(t, t1)
	if attempt1Number != 1 {
		t.Fatalf("first attempt number = %d, want 1", attempt1Number)
	}
	session1ID := fx.sessionForAttempt(t, attempt1ID)
	worktree1Path, worktree1Branch, worktree1Base := fx.requireWorktreeForAttempt(t, attempt1ID)
	if worktree1Path == "" || worktree1Branch == "" {
		t.Fatalf("attempt 1's worktree row is incomplete: path=%q branch=%q", worktree1Path, worktree1Branch)
	}
	if worktree1Base != repo.Base {
		t.Errorf("attempt 1's worktree base = %s, want the frozen base commit %s", worktree1Base, repo.Base)
	}

	// Design section 11 scenario 4 is "a worker killed mid-attempt" —
	// AFTER its own launch settled, never a race against the controller's
	// own launch corroboration (that race is LAUNCH-2's own coverage,
	// deliberately kept out of this scenario: a worker-hold worker reaches
	// its message barrier within about a second of exec, well inside the
	// controller's 2s corroboration poll interval, so killing immediately
	// after the checks above could catch it before settlement). Wait for,
	// and explicitly assert, the settled precondition first.
	fx.requireAttemptState(t, attempt1ID, "running")
	fx.requireSessionState(t, session1ID, "active")
	if state := fx.claimState(t, session1ID); state != "execed" {
		t.Fatalf("attempt 1's launch claim state = %q before the kill, want execed (settled)", state)
	}

	// Kill the worker mid-attempt (blocked on its own barrier, holding no
	// accepted result, its own launch already settled) — the live
	// controller, never a crashed one, must reconcile this on its own
	// next scheduling pass.
	fx.killSession(t, session1ID, attempt1ID)

	// Attempt 1 interrupts, task moves to needs-rework, session terminates
	// — all in the settlement transaction — and the manager receives the
	// notice with the EXACT rendered first line. Asserted as the DURABLE
	// transition into needs-rework (never a live task-state poll): the
	// scripted manager reads the notice and requests retry immediately
	// (fixtureworker_test.go's handleManagerMessage), so the controller
	// can move the task on to ready/active again before a snapshot poll
	// ever observes it sitting at needs-rework (Astra review finding).
	fx.requireTransitionAt(t, "task", t1, "needs-rework")
	fx.requireSessionState(t, session1ID, "terminated")
	fx.requireAttemptState(t, attempt1ID, "interrupted")
	// Exactly one running->interrupted transition — the settlement never
	// double-fires (e.g. a raced second reconciliation pass).
	if n := fx.transitionCount(t, "attempt", attempt1ID, "running", "interrupted"); n != 1 {
		t.Errorf("attempt %s running->interrupted transition count = %d, want 1", attempt1ID, n)
	}
	noticeID := fx.requireManagerNoticeFirstLine(t, featureRunTimeout, workerInterruptionNoticeFirstLine)
	if noticeID == "" {
		t.Fatal("requireManagerNoticeFirstLine returned an empty message id")
	}

	// The scripted manager retries automatically on the notice; a fresh
	// attempt 2 is reserved with a fresh worktree from the current
	// integration head (repo.Base — no task has integrated yet).
	fx.requireTaskState(t, t1, "active")
	attempt2ID, attempt2Number := fx.currentAttempt(t, t1)
	if attempt2Number != 2 {
		t.Fatalf("second attempt number = %d, want 2", attempt2Number)
	}
	if attempt2ID == attempt1ID {
		t.Fatal("the retried attempt has the same id as the interrupted one")
	}
	session2ID := fx.sessionForAttempt(t, attempt2ID)
	if session2ID == session1ID {
		t.Fatal("the retried attempt's session is the same as the interrupted one's")
	}
	// Attempt 2's own launch settles (session active) before anything
	// else is asserted about it — the same explicit precondition attempt
	// 1 was held to before its own kill, restated here for the retried
	// attempt.
	fx.requireSessionState(t, session2ID, "active")
	worktree2Path, worktree2Branch, worktree2Base := fx.requireWorktreeForAttempt(t, attempt2ID)
	if worktree2Path == "" || worktree2Branch == "" {
		t.Fatalf("attempt 2's worktree row is incomplete: path=%q branch=%q", worktree2Path, worktree2Branch)
	}
	if worktree2Path == worktree1Path || worktree2Branch == worktree1Branch {
		t.Errorf("attempt 2 reused attempt 1's worktree (path=%q branch=%q); want a fresh one", worktree2Path, worktree2Branch)
	}
	if worktree2Base != repo.Base {
		t.Errorf("attempt 2's worktree base = %s, want the current integration head %s (no task has integrated yet)", worktree2Base, repo.Base)
	}

	// Release attempt 2's own barrier (the same behavior the task
	// instructions still name) and let it complete normally.
	question2ID := fx.relayedQuestionFor(t, session2ID, featureRunTimeout)
	fx.answerHuman(t, question2ID, "release the retried attempt")
	fx.requireSessionState(t, session2ID, "terminated")

	fx.requireTaskState(t, t1, "integrated")
	reviewTaskID := fx.requireReviewTask(t)
	fx.requireTaskState(t, reviewTaskID, "completed")
	if verdict, ok := fx.reviewVerdict(t, reviewTaskID); !ok || verdict != "approve" {
		t.Errorf("review verdict = %q (found=%v), want approve", verdict, ok)
	}
	fx.requireRunState(t, "completed")

	// Full provenance for BOTH attempts: worktrees (already asserted
	// above), sessions, launch claims and results — attempt 1 never
	// submitted, attempt 2 did.
	if pid, ok := fx.launchClaimPID(t, session1ID); !ok || pid <= 0 {
		t.Errorf("attempt 1's session %s has no recorded launch claim pid (ok=%v pid=%d)", session1ID, ok, pid)
	}
	if pid, ok := fx.launchClaimPID(t, session2ID); !ok || pid <= 0 {
		t.Errorf("attempt 2's session %s has no recorded launch claim pid (ok=%v pid=%d)", session2ID, ok, pid)
	}
	if n := fx.resultCount(t, attempt1ID); n != 0 {
		t.Errorf("attempt 1 (interrupted before ever submitting) has %d result rows, want 0", n)
	}
	if n := fx.resultCount(t, attempt2ID); n != 1 {
		t.Errorf("attempt 2 (completed) has %d result rows, want 1", n)
	}
	if fx.attemptCount(t, t1) != 2 {
		t.Errorf("task t1 has %d recorded attempts, want exactly 2", fx.attemptCount(t, t1))
	}
}
