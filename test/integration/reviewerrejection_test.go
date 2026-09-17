package integration

import (
	"fmt"
	"strconv"
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

// verdictStaleSubjectShortfallToken mirrors run.ShortfallVerdictStaleSubject:
// once the fix task's integration moves the head, R1's own rejection is no
// longer of the CURRENT candidate — EvaluateReadiness checks subject
// currency before the verdict value, so a superseded review (of either
// verdict) reports this token instead, never verdict-rejected again.
const verdictStaleSubjectShortfallToken = "verdict-stale-subject" //nolint:gosec // G101: a fixed shortfall kind token, not a credential.

// verdictRejectedLinePrefix mirrors the fixed text
// app.GrammarVerdictRejectedLine renders before its review= field
// (internal/app/grammar.go), so this scenario's own parsing fails loudly
// on any drift rather than matching a bare substring anywhere in the
// output.
const verdictRejectedLinePrefix = "shortfall: " + verdictRejectedShortfallToken + " review="

// verdictRejectedLine is one parsed "shortfall: verdict-rejected
// review=<id> subject=<oid> reasons=<path>" line from a real hop status
// rendering (app.GrammarVerdictRejectedLine, STATUS-1): the review's own
// identity, its subject commit and its reasons artifact path.
type verdictRejectedLine struct {
	reviewID, subjectCommitOID, reasonsPath string
}

// parseVerdictRejectedLine extracts the SOLE verdict-rejected shortfall
// line from one "hop status -run" rendering, failing the test outright if
// none or more than one is present, and unquotes a reasons path
// cmd/hop's safeRenderExternal rendered through strconv.Quote (a raw path
// never begins with a double quote — that shape is reserved for the
// quoted form, which always starts with one, so a leading quote
// unambiguously means the field must be unquoted).
func parseVerdictRejectedLine(t *testing.T, statusOutput string) verdictRejectedLine {
	t.Helper()
	var found []verdictRejectedLine
	for _, line := range strings.Split(statusOutput, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), verdictRejectedLinePrefix)
		if !ok {
			continue
		}
		reviewID, rest, ok := strings.Cut(rest, " subject=")
		if !ok || reviewID == "" {
			t.Fatalf("malformed verdict-rejected shortfall line: %q", line)
		}
		subjectCommitOID, rawReasons, ok := strings.Cut(rest, " reasons=")
		if !ok || subjectCommitOID == "" {
			t.Fatalf("malformed verdict-rejected shortfall line: %q", line)
		}
		reasonsPath := rawReasons
		if strings.HasPrefix(reasonsPath, "\"") {
			unquoted, err := strconv.Unquote(reasonsPath)
			if err != nil {
				t.Fatalf("could not unquote status reasons path %q: %v", reasonsPath, err)
			}
			reasonsPath = unquoted
		}
		found = append(found, verdictRejectedLine{reviewID: reviewID, subjectCommitOID: subjectCommitOID, reasonsPath: reasonsPath})
	}
	if len(found) != 1 {
		t.Fatalf("hop status -run output carries %d verdict-rejected shortfall lines, want exactly 1; stdout:\n%s", len(found), statusOutput)
	}
	return found[0]
}

// TestRealProcessReviewerRejection is design section 11 scenario 5
// (reference trace 5): a reject verdict blocks completion; the manager
// plans a fix task; the new integration head gets its own new review
// task; approval on the new head completes the run. R1's stale verdict
// (bound to a superseded head) can never satisfy the guard, by
// construction — the guard compares object IDs, so staleness is computed,
// never stored.
func TestRealProcessReviewerRejection(t *testing.T) {
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

	// hop status renders the guard shortfall the fixture manager's own
	// standing instruction reads (STATUS-1's manager verdict channel):
	// asserted here against the STORE ITSELF, not just the bare kind
	// token — the review id, the subject commit and the reasons path must
	// name R1's own accepted review row exactly (reviews.id, distinct
	// from R1's own task id, r1), since that identity is the whole point
	// of the correlation rule the manager applies to decide whether a
	// given notice IS this review's own rejection.
	result := runHop(t, fx.env, repo.Root, "status", "-C", repo.Root, "-run", fx.runID)
	if result.ExitCode != 0 {
		t.Fatalf("hop status -run %s exit=%d stdout=%q stderr=%q", fx.runID, result.ExitCode, result.Stdout, result.Stderr)
	}
	rejected := parseVerdictRejectedLine(t, result.Stdout)
	wantReviewID := fx.scalar(t, fmt.Sprintf("SELECT id FROM reviews WHERE task_id = '%s';", r1))
	if rejected.reviewID != wantReviewID {
		t.Errorf("verdict-rejected shortfall names review %s, want %s (R1's own accepted review row)", rejected.reviewID, wantReviewID)
	}
	if rejected.subjectCommitOID != headAfterT1 {
		t.Errorf("verdict-rejected shortfall subject = %s, want %s (the head R1 reviewed)", rejected.subjectCommitOID, headAfterT1)
	}
	wantReasonsPath := fx.scalar(t, fmt.Sprintf("SELECT reasons_path FROM reviews WHERE task_id = '%s';", r1))
	if rejected.reasonsPath != wantReasonsPath {
		t.Errorf("verdict-rejected shortfall reasons path = %s, want %s (the store's own recorded reasons_path for R1)", rejected.reasonsPath, wantReasonsPath)
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
	fixQuestionID := fx.relayedQuestionFor(t, fixSessionID)
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

	// R1's rejection is no longer of the CURRENT head once the fix
	// integrated: EvaluateReadiness checks subject currency before the
	// verdict value, so R1 now reports verdict-stale-subject (a superseded
	// review of either verdict), or no verdict shortfall at all if a fast
	// new review has already been accepted for the new head by the time
	// this polls — but never verdict-rejected again, which would mean a
	// fixed defect's own reject resurfacing after it was already addressed.
	afterFixResult := runHop(t, fx.env, repo.Root, "status", "-C", repo.Root, "-run", fx.runID)
	if afterFixResult.ExitCode != 0 {
		t.Fatalf("hop status -run %s exit=%d stdout=%q stderr=%q", fx.runID, afterFixResult.ExitCode, afterFixResult.Stdout, afterFixResult.Stderr)
	}
	switch {
	case strings.Contains(afterFixResult.Stdout, "shortfall: "+verdictRejectedShortfallToken):
		t.Errorf("hop status -run %s still names verdict-rejected after the fix integrated a new head; stdout:\n%s", fx.runID, afterFixResult.Stdout)
	case strings.Contains(afterFixResult.Stdout, "shortfall: "+verdictStaleSubjectShortfallToken):
		t.Logf("hop status -run %s reports verdict-stale-subject for R1 after the fix integrated, as expected before a new review is accepted", fx.runID)
	default:
		t.Logf("hop status -run %s reports no verdict shortfall after the fix integrated (a new review was already accepted for the new head)", fx.runID)
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
