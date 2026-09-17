package integration

import (
	"fmt"
	"strings"
	"testing"
)

// verdictRejectedShortfallToken mirrors the identically named constant
// inside fixtureWorkerSource (the embedded fixture manager's own
// hop-status-polling detection, per design section 8/defect STATUS-1):
// run.ShortfallVerdictRejected's kind token, "verdict-rejected". Retyped
// here, never derived, so this scenario's own assertion of hop status's
// rendering matches literally the same token the fixture manager itself
// matches.
const verdictRejectedShortfallToken = "verdict-rejected"

// TestRealProcessReviewerRejection is design section 11 scenario 5
// (reference trace 5): a reject verdict blocks completion; the manager
// plans a fix task; the new integration head gets its own new review
// task; approval on the new head completes the run. R1's stale verdict
// (bound to a superseded head) can never satisfy the guard, by
// construction — the guard compares object IDs, so staleness is computed,
// never stored.
//
// Blocked on defect STATUS-1 (reported and confirmed): cmd/hop's `hop
// status -run` does not yet render EvaluateReadiness's guard shortfalls
// at all, and no notice body or other channel names a reject verdict
// either (the acceptance notice's body is only the reviewer's own raw
// reasons text) — the fixture manager's standing instruction to check hop
// status for the verdict-rejected shortfall therefore has nothing to
// read yet. This scenario is written and ready; STATUS-1 lands on a
// separate branch, after which this test is unskipped with no other
// change (the fixture manager's own detection, statusReportsVerdictRejected,
// already matches the kind token this scenario asserts hop status renders).
func TestRealProcessReviewerRejection(t *testing.T) {
	t.Skip("STATUS-1: hop status does not render guard shortfalls yet")

	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	worker := buildFixtureWorker(t, artifacts)
	installFixtureWorkerAsClaudeStub(t, server, worker)
	server.start(t)

	scratchDir := artifacts.dir(t, "fixture-scratch")
	repo := newFeatureFixtureRepo(t, artifacts, server, "repo", featureFixtureOptions{
		ScratchDir: scratchDir, ReviewerBehavior: "reviewer-reject-once",
		MaxWorkers: 2, RetryLimit: 3, MessageWaitTimeout: "3s", MessageAttentionAfter: "30s",
	})

	brief := fixtureManagerBrief(scratchDir,
		[]fixtureManagerTask{{Label: "t1", Title: "Implement t1", Behavior: "worker-implement"}},
		[]fixtureManagerAnswer{{Match: fixtureHoldMarker, Action: "relay"}},
		"worker-hold",
	)
	fx := startFeatureRun(t, artifacts, server, repo, scratchDir, brief)

	t1 := fx.requireTaskBySeq(t, 1)
	fx.requireTaskState(t, t1, "integrated")
	headAfterT1 := fx.integrationHead(t)

	r1 := fx.requireReviewTask(t)
	fx.requireTaskState(t, r1, "completed")
	verdict1, ok := fx.reviewVerdict(t, r1)
	if !ok || verdict1 != "reject" {
		t.Fatalf("first review verdict = %q (found=%v), want reject", verdict1, ok)
	}
	if subject1 := fx.scalar(t, fmt.Sprintf("SELECT subject_commit_oid FROM reviews WHERE task_id = '%s';", r1)); subject1 != headAfterT1 {
		t.Errorf("first review subject = %s, want the head at the time it was created %s", subject1, headAfterT1)
	}

	// A reject verdict completes the review task but must never complete
	// the run.
	if state := fx.runState(t); state == "completed" {
		t.Fatal("run completed despite a reject verdict")
	}

	// STATUS-1: hop status now renders the guard shortfall the fixture
	// manager's own standing instruction reads.
	result := runHop(t, fx.env, repo.Root, "status", "-C", repo.Root, "-run", fx.runID)
	if result.ExitCode != 0 {
		t.Fatalf("hop status -run %s exit=%d stdout=%q stderr=%q", fx.runID, result.ExitCode, result.Stdout, result.Stderr)
	}
	if !strings.Contains(result.Stdout, verdictRejectedShortfallToken) {
		t.Errorf("hop status -run %s does not name the %q guard shortfall; stdout:\n%s", fx.runID, verdictRejectedShortfallToken, result.Stdout)
	}

	// The manager plans a fix task. Task seq numbers are shared with
	// review tasks (EnsureReviewTask mints maxSeq+1), so R1 itself is
	// seq 2 — requireTaskBySeq(t, 2) would select R1, not the fix task.
	// Select the fix task by KIND (implement) and identity: created
	// after R1, and distinct from it.
	fixTaskID := fx.requireImplementTaskAfter(t, r1)
	if fixTaskID == r1 {
		t.Fatalf("selected fix task %s is R1 itself", fixTaskID)
	}
	fx.requireTaskState(t, fixTaskID, "active")
	fixAttemptID, _ := fx.currentAttempt(t, fixTaskID)
	fixSessionID := fx.sessionForAttempt(t, fixAttemptID)

	// Hold the fix worker at its own barrier while re-asserting the
	// rejection/head facts — proving the guard genuinely still blocks
	// completion at this exact point (the fix task not yet integrated),
	// not merely "eventually" once everything has already settled.
	fixQuestionID := fx.relayedQuestionFor(t, fixSessionID, featureRunTimeout)
	if state := fx.runState(t); state == "completed" {
		t.Fatal("run completed while the fix task's own worker is still held at its barrier")
	}
	if headNow := fx.integrationHead(t); headNow != headAfterT1 {
		t.Errorf("integration head = %s before the fix task integrated, want it still %s", headNow, headAfterT1)
	}
	fx.answerHuman(t, fixQuestionID, "release the fix worker")

	fx.requireTaskState(t, fixTaskID, "integrated")
	headAfterFix := fx.integrationHead(t)
	if headAfterFix == headAfterT1 {
		t.Fatal("the fix task's integration did not move the integration head")
	}

	// A new review task is created for the new head; approval completes
	// the run.
	r2 := fx.requireReviewTaskOtherThan(t, r1)
	fx.requireTaskState(t, r2, "completed")
	verdict2, ok := fx.reviewVerdict(t, r2)
	if !ok || verdict2 != "approve" {
		t.Errorf("second review verdict = %q (found=%v), want approve", verdict2, ok)
	}
	if subject2 := fx.scalar(t, fmt.Sprintf("SELECT subject_commit_oid FROM reviews WHERE task_id = '%s';", r2)); subject2 != headAfterFix {
		t.Errorf("second review subject = %s, want the new integration head %s", subject2, headAfterFix)
	}

	fx.requireRunState(t, "completed")

	// R1's stale verdict, bound to a superseded head, can never satisfy
	// the guard: the final head differs from what R1 reviewed.
	if headAfterFix == headAfterT1 {
		t.Error("the final integration head equals the head R1 (rejected) reviewed; the guard would be satisfied by a stale verdict")
	}
}
