package app_test

import (
	"reflect"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/run"
)

// TestStatusGuardShortfallsHeadTreeRule pins the status read model's
// head-tree rule directly: the head tree is the one recorded for exactly
// the head commit, never the commit id; subjects for other commits and
// subjects with no tree record nothing; an unrecorded tree reads the
// latest review as stale and a check as missing, never current; and
// disagreeing trees replace the check and verdict shortfalls with
// evidence-inconsistent while the plan and task guards stay.
func TestStatusGuardShortfallsHeadTreeRule(t *testing.T) {
	openTask := run.Task{ID: "task-open", Kind: run.TaskKindImplement, State: run.TaskActive}
	review := func(verdict run.Verdict, commit, tree string) *run.Review {
		return &run.Review{ID: "review-1", SubjectCommitOID: commit, SubjectTreeOID: tree, Verdict: verdict}
	}
	headCtx := func(latest *run.Review) run.GuardContext {
		return run.GuardContext{PlanClosed: true, HeadCommitOID: "commit-head", LatestReview: latest}
	}
	checkMissing := run.GuardShortfall{Kind: run.ShortfallCheckMissing}
	stale := run.GuardShortfall{Kind: run.ShortfallVerdictStaleSubject}
	rejected := run.GuardShortfall{Kind: run.ShortfallVerdictRejected, ReviewID: "review-1", SubjectCommitOID: "commit-head"}
	inconsistent := run.GuardShortfall{Kind: app.ShortfallEvidenceInconsistent}

	cases := []struct {
		name     string
		guard    run.GuardContext
		recorded []app.RecordedSubject
		want     []run.GuardShortfall
	}{
		{
			name:     "a reject of the head, its tree recorded by the review itself",
			guard:    headCtx(review(run.VerdictReject, "commit-head", "tree-head")),
			recorded: []app.RecordedSubject{{CommitOID: "commit-head", TreeOID: "tree-head"}},
			want:     []run.GuardShortfall{checkMissing, rejected},
		},
		{
			name:  "an approve of the head, recorded by its review task and review",
			guard: headCtx(review(run.VerdictApprove, "commit-head", "tree-head")),
			recorded: []app.RecordedSubject{
				{CommitOID: "commit-head", TreeOID: "tree-head"},
				{CommitOID: "commit-head", TreeOID: "tree-head"},
			},
			want: []run.GuardShortfall{checkMissing},
		},
		{
			name: "a caller's head tree is ignored, even when it is the commit id",
			guard: func() run.GuardContext {
				g := headCtx(review(run.VerdictApprove, "commit-head", "commit-head"))
				g.HeadTreeOID = "commit-head"
				return g
			}(),
			want: []run.GuardShortfall{checkMissing, stale},
		},
		{
			name:  "subjects of other commits and subjects without a tree record nothing",
			guard: headCtx(review(run.VerdictReject, "commit-old", "tree-old")),
			recorded: []app.RecordedSubject{
				{CommitOID: "commit-old", TreeOID: "tree-other"},
				{CommitOID: "commit-head", TreeOID: ""},
			},
			want: []run.GuardShortfall{checkMissing, stale},
		},
		{
			name: "a passing check for the head with no recorded tree is not current",
			guard: func() run.GuardContext {
				g := headCtx(nil)
				g.LatestCheck = &run.CheckReceipt{Passed: true, SubjectCommitOID: "commit-head", SubjectTreeOID: "tree-head"}
				return g
			}(),
			want: []run.GuardShortfall{checkMissing, {Kind: run.ShortfallVerdictMissing}},
		},
		{
			name: "disagreeing trees keep the plan and task guards and replace the rest",
			guard: func() run.GuardContext {
				g := headCtx(review(run.VerdictReject, "commit-head", "tree-head"))
				g.PlanClosed = false
				g.ImplementTasks = []run.Task{openTask}
				return g
			}(),
			recorded: []app.RecordedSubject{
				{CommitOID: "commit-head", TreeOID: "tree-head"},
				{CommitOID: "commit-head", TreeOID: "tree-other"},
			},
			want: []run.GuardShortfall{
				{Kind: run.ShortfallPlanOpen},
				{Kind: run.ShortfallTaskNotIntegrated, TaskID: "task-open"},
				inconsistent,
			},
		},
		{
			name:     "no integrated head ignores the recorded subjects",
			guard:    run.GuardContext{PlanClosed: true, LatestReview: review(run.VerdictApprove, "commit-head", "tree-head")},
			recorded: []app.RecordedSubject{{CommitOID: "commit-head", TreeOID: "tree-other"}},
			want:     []run.GuardShortfall{checkMissing, stale},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := app.StatusGuardShortfalls(tc.guard, tc.recorded); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("StatusGuardShortfalls() = %+v, want %+v", got, tc.want)
			}
		})
	}
	if string(app.ShortfallEvidenceInconsistent) != "evidence-inconsistent" {
		t.Fatalf("ShortfallEvidenceInconsistent = %q, want the grammar token", app.ShortfallEvidenceInconsistent)
	}
}
