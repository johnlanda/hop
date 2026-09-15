package run_test

import (
	"testing"

	"github.com/johnlanda/hop/internal/domain/run"
)

// readyContext is a fully satisfied GuardContext: one integrated implement
// task, a passing check for the head, and an approve verdict bound to it.
func readyContext() run.GuardContext {
	return run.GuardContext{
		PlanClosed:     true,
		ImplementTasks: []run.Task{{ID: testTaskID, State: run.TaskIntegrated}},
		HeadCommitOID:  "head-commit",
		HeadTreeOID:    "head-tree",
		LatestCheck: &run.CheckReceipt{
			Passed: true, SubjectCommitOID: "head-commit", SubjectTreeOID: "head-tree",
		},
		LatestReview: &run.Review{
			ID: testReviewID, Verdict: run.VerdictApprove,
			SubjectCommitOID: "head-commit", SubjectTreeOID: "head-tree",
		},
	}
}

func hasShortfall(missing []run.GuardShortfall, kind run.ShortfallKind) bool {
	for _, m := range missing {
		if m.Kind == kind {
			return true
		}
	}
	return false
}

func TestEvaluateReadinessReady(t *testing.T) {
	ready, missing := run.EvaluateReadiness(readyContext())
	if !ready {
		t.Fatalf("EvaluateReadiness(fully satisfied) ready = false, missing = %+v", missing)
	}
	if len(missing) != 0 {
		t.Fatalf("EvaluateReadiness(fully satisfied) missing = %+v, want empty", missing)
	}
}

func TestEvaluateReadinessPlanOpen(t *testing.T) {
	ctx := readyContext()
	ctx.PlanClosed = false

	ready, missing := run.EvaluateReadiness(ctx)
	if ready {
		t.Fatal("EvaluateReadiness(plan open) ready = true, want false")
	}
	if !hasShortfall(missing, run.ShortfallPlanOpen) {
		t.Fatalf("EvaluateReadiness(plan open) missing = %+v, want ShortfallPlanOpen", missing)
	}
}

// TestEvaluateReadinessMissingIntegration is the "missing integration"
// vector: an implement task that has not reached integrated.
func TestEvaluateReadinessMissingIntegration(t *testing.T) {
	ctx := readyContext()
	ctx.ImplementTasks = []run.Task{
		{ID: testTaskID, State: run.TaskIntegrated},
		{ID: testSecondTaskID, State: run.TaskCompleted},
	}

	ready, missing := run.EvaluateReadiness(ctx)
	if ready {
		t.Fatal("EvaluateReadiness(missing integration) ready = true, want false")
	}
	found := false
	for _, m := range missing {
		if m.Kind == run.ShortfallTaskNotIntegrated && m.TaskID == testSecondTaskID {
			found = true
		}
	}
	if !found {
		t.Fatalf("EvaluateReadiness(missing integration) missing = %+v, want ShortfallTaskNotIntegrated naming %s", missing, testSecondTaskID)
	}
}

func TestEvaluateReadinessCheckMissing(t *testing.T) {
	ctx := readyContext()
	ctx.LatestCheck = nil

	ready, missing := run.EvaluateReadiness(ctx)
	if ready {
		t.Fatal("EvaluateReadiness(check missing) ready = true, want false")
	}
	if !hasShortfall(missing, run.ShortfallCheckMissing) {
		t.Fatalf("EvaluateReadiness(check missing) missing = %+v, want ShortfallCheckMissing", missing)
	}
}

// TestEvaluateReadinessCheckFailed proves a recorded but failing receipt
// for the current head is exactly as unready as no receipt at all.
func TestEvaluateReadinessCheckFailed(t *testing.T) {
	ctx := readyContext()
	ctx.LatestCheck = &run.CheckReceipt{Passed: false, SubjectCommitOID: "head-commit", SubjectTreeOID: "head-tree"}

	ready, missing := run.EvaluateReadiness(ctx)
	if ready {
		t.Fatal("EvaluateReadiness(check failed) ready = true, want false")
	}
	if !hasShortfall(missing, run.ShortfallCheckMissing) {
		t.Fatalf("EvaluateReadiness(check failed) missing = %+v, want ShortfallCheckMissing", missing)
	}
}

// TestEvaluateReadinessStaleCheckHeadMoved proves that a passing receipt
// for an OLDER integration head, alongside an otherwise-valid approve
// verdict for the CURRENT head, must still report ShortfallCheckMissing —
// a caller cannot satisfy the check guard by asserting a bare "passed"
// flag once the head has moved.
func TestEvaluateReadinessStaleCheckHeadMoved(t *testing.T) {
	ctx := readyContext()
	ctx.LatestCheck = &run.CheckReceipt{Passed: true, SubjectCommitOID: "old-commit", SubjectTreeOID: "old-tree"}

	ready, missing := run.EvaluateReadiness(ctx)
	if ready {
		t.Fatal("EvaluateReadiness(stale check, head moved) ready = true, want false")
	}
	if !hasShortfall(missing, run.ShortfallCheckMissing) {
		t.Fatalf("EvaluateReadiness(stale check, head moved) missing = %+v, want ShortfallCheckMissing", missing)
	}
}

func TestEvaluateReadinessVerdictMissing(t *testing.T) {
	ctx := readyContext()
	ctx.LatestReview = nil

	ready, missing := run.EvaluateReadiness(ctx)
	if ready {
		t.Fatal("EvaluateReadiness(verdict missing) ready = true, want false")
	}
	if !hasShortfall(missing, run.ShortfallVerdictMissing) {
		t.Fatalf("EvaluateReadiness(verdict missing) missing = %+v, want ShortfallVerdictMissing", missing)
	}
}

// TestEvaluateReadinessRejectPresent is the "reject present" vector.
func TestEvaluateReadinessRejectPresent(t *testing.T) {
	ctx := readyContext()
	ctx.LatestReview = &run.Review{ID: testReviewID, Verdict: run.VerdictReject, SubjectCommitOID: "head-commit", SubjectTreeOID: "head-tree"}

	ready, missing := run.EvaluateReadiness(ctx)
	if ready {
		t.Fatal("EvaluateReadiness(reject present) ready = true, want false")
	}
	if !hasShortfall(missing, run.ShortfallVerdictRejected) {
		t.Fatalf("EvaluateReadiness(reject present) missing = %+v, want ShortfallVerdictRejected", missing)
	}
}

// TestEvaluateReadinessStaleSubjectApprove is the "stale-subject approve"
// vector (also "head moved"): an approve verdict bound to a superseded
// head never satisfies the guard, since the comparison is computed here,
// not trusted from a stored flag.
func TestEvaluateReadinessStaleSubjectApprove(t *testing.T) {
	ctx := readyContext()
	ctx.LatestReview = &run.Review{ID: testReviewID, Verdict: run.VerdictApprove, SubjectCommitOID: "old-commit", SubjectTreeOID: "old-tree"}

	ready, missing := run.EvaluateReadiness(ctx)
	if ready {
		t.Fatal("EvaluateReadiness(stale-subject approve) ready = true, want false")
	}
	if !hasShortfall(missing, run.ShortfallVerdictStaleSubject) {
		t.Fatalf("EvaluateReadiness(stale-subject approve) missing = %+v, want ShortfallVerdictStaleSubject", missing)
	}
}

// TestEvaluateReadinessReportsEveryShortfall proves missing is not just
// the first failing guard: every unsatisfied guard is reported together.
func TestEvaluateReadinessReportsEveryShortfall(t *testing.T) {
	ctx := run.GuardContext{
		PlanClosed:     false,
		ImplementTasks: []run.Task{{ID: testTaskID, State: run.TaskCompleted}},
		LatestCheck:    nil,
		LatestReview:   nil,
	}

	ready, missing := run.EvaluateReadiness(ctx)
	if ready {
		t.Fatal("EvaluateReadiness(everything missing) ready = true, want false")
	}
	for _, kind := range []run.ShortfallKind{
		run.ShortfallPlanOpen, run.ShortfallTaskNotIntegrated, run.ShortfallCheckMissing, run.ShortfallVerdictMissing,
	} {
		if !hasShortfall(missing, kind) {
			t.Fatalf("EvaluateReadiness(everything missing) missing = %+v, want %s among them", missing, kind)
		}
	}
}
