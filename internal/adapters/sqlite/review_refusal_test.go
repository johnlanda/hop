package sqlite_test

import (
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// TestSubmitReviewRefusalReasons pins the real store's typed review refusal
// reasons (internal/app's fakes_review_refusal_test.go is the fake's half):
// every refused verdict names its grammar reason token at the store's own
// decision point — subject-mismatch for another candidate, not-reviewer for
// a caller that is not the attempt's reviewer, stale for a superseded
// reviewer, malformed for disagreeing identities, conflicting for a
// different verdict after an acceptance — with its receipt and no review
// row, and accepted and duplicate outcomes carry none.
func TestSubmitReviewRefusalReasons(t *testing.T) {
	cases := []struct {
		name       string
		submission func(f *reviewFixture) app.ReviewSubmission
		setup      func(t *testing.T, f *reviewFixture)
		kind       app.ReviewOutcomeKind
		reason     string
		detail     string
	}{
		{
			name: "another candidate is subject-mismatch",
			submission: func(f *reviewFixture) app.ReviewSubmission {
				s := f.submission(8130, run.VerdictApprove)
				s.SubjectCommitOID, s.SubjectTreeOID = "commit-other", "tree-other"
				return s
			},
			kind: app.ReviewStale, reason: app.GrammarReasonSubjectMismatch,
		},
		{
			name: "the manager's session is not-reviewer",
			submission: func(f *reviewFixture) app.ReviewSubmission {
				s := f.submission(8131, run.VerdictApprove)
				s.Session, s.IncarnationID = f.ManagerID, f.ManagerIncarnation
				return s
			},
			kind: app.ReviewStale, reason: app.GrammarReasonNotReviewer, detail: app.ReviewNotReviewerDetail,
		},
		{
			name:       "a superseded reviewer is stale",
			submission: func(f *reviewFixture) app.ReviewSubmission { return f.submission(8132, run.VerdictApprove) },
			setup:      func(t *testing.T, f *reviewFixture) { supersedeWithoutSuccessor(t, f.featureFixture, f.ReviewerID) },
			kind:       app.ReviewStale, reason: app.GrammarReasonStale,
		},
		{
			name: "an unknown task is malformed",
			submission: func(f *reviewFixture) app.ReviewSubmission {
				s := f.submission(8133, run.VerdictApprove)
				s.TaskID = identity.TaskID(uid(8134))
				return s
			},
			kind: app.ReviewMalformed, reason: app.GrammarReasonMalformed, detail: "attempt/task/run do not agree",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			outcome, err := f.store.SubmitReview(t.Context(), tc.submission(f))
			if err != nil || outcome.Kind != tc.kind || outcome.Reason != tc.reason || outcome.ReviewID != "" {
				t.Fatalf("SubmitReview() = %+v, %v; want %s/%s naming no review", outcome, err, tc.kind, tc.reason)
			}
			if tc.detail != "" && outcome.Detail != tc.detail {
				t.Fatalf("Detail = %q, want %q", outcome.Detail, tc.detail)
			}
			if n := countRows(t, f.store, `SELECT COUNT(*) FROM review_submissions WHERE outcome = ?`, string(tc.kind)); n != 1 {
				t.Fatalf("%s review receipts = %d, want 1", tc.kind, n)
			}
			if n := countRows(t, f.store, `SELECT COUNT(*) FROM reviews`); n != 0 {
				t.Fatalf("review rows after a refusal = %d, want 0", n)
			}
		})
	}

	t.Run("accepted and duplicate carry no reason; a different verdict is conflicting", func(t *testing.T) {
		f := newReviewFixture(t)
		approve := f.submission(8135, run.VerdictApprove)
		for _, want := range []app.ReviewOutcomeKind{app.ReviewAccepted, app.ReviewDuplicate} {
			if outcome, err := f.store.SubmitReview(t.Context(), approve); err != nil || outcome.Kind != want || outcome.Reason != "" {
				t.Fatalf("SubmitReview(approve) = %+v, %v; want %s with no reason", outcome, err, want)
			}
		}
		reject := f.submission(8136, run.VerdictReject)
		reject.ReasonsDigest = approve.ReasonsDigest
		if outcome, err := f.store.SubmitReview(t.Context(), reject); err != nil || outcome.Kind != app.ReviewConflicting || outcome.Reason != app.GrammarReasonConflicting {
			t.Fatalf("SubmitReview(reject) = %+v, %v; want conflicting/%s", outcome, err, app.GrammarReasonConflicting)
		}
	})
}
