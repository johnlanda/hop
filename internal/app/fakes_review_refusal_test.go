package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// This file is the fake-side half of the typed review refusal reasons
// internal/adapters/sqlite's review_refusal_test.go pins against the real
// store: every refused verdict names its grammar reason token at the
// store's own decision point, in the real store's order, and the driving
// use case refuses a store outcome whose reason its kind does not admit.

// storeSubmission is the fixture reviewer's matching-subject submission,
// as the use case would hand it to the store.
func (f *reviewFixture) storeSubmission(t *testing.T) app.ReviewSubmission {
	t.Helper()
	id, err := identity.ParseReviewID(f.tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse review id: %v", err)
	}
	return app.ReviewSubmission{
		ID: id, RunID: f.fr.RunID, TaskID: f.TaskID, AttemptID: f.AttemptID,
		Session: f.SessionID, IncarnationID: f.IncarnationID,
		SubjectCommitOID: f.SubjectCommit, SubjectTreeOID: fakeSubjectTree,
		Verdict: run.VerdictApprove, ReasonsPath: "/state/reasons", ReasonsDigest: "reasons",
	}
}

// TestFakeSubmitReviewRefusalReasons drives every refused verdict through
// Controller.SubmitReviewVerdict over the featureStore wrapper, each
// carrying the reason the real store sets and recording no review.
func TestFakeSubmitReviewRefusalReasons(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(t *testing.T, f *reviewFixture)
		subject func(f *reviewFixture) string
		reason  string
		detail  string
	}{
		{
			name:    "another candidate is subject-mismatch",
			subject: func(*reviewFixture) string { return "0th3rc0mm17" },
			reason:  app.GrammarReasonSubjectMismatch,
		},
		{
			name: "the manager's session is not-reviewer",
			mutate: func(_ *testing.T, f *reviewFixture) {
				f.SessionID, f.IncarnationID = f.fr.ManagerID, f.fr.ManagerIncarnation
			},
			reason: app.GrammarReasonNotReviewer, detail: app.ReviewNotReviewerDetail,
		},
		{
			name: "an incarnation that is not the reviewer's is stale",
			mutate: func(t *testing.T, f *reviewFixture) {
				other, err := identity.ParseIncarnationID(f.tc.IDs.NewID())
				if err != nil {
					t.Fatalf("parse incarnation id: %v", err)
				}
				f.IncarnationID = other
			},
			reason: app.GrammarReasonStale,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t)
			if tc.mutate != nil {
				tc.mutate(t, f)
			}
			subject := f.SubjectCommit
			if tc.subject != nil {
				subject = tc.subject(f)
			}
			result := f.submit(t, "approve", subject, []byte("ok"))
			if result.Outcome != string(app.ReviewStale) || result.Reason != tc.reason || result.ReviewID != "" {
				t.Fatalf("SubmitReviewVerdict() = %+v, want stale/%s naming no review", result, tc.reason)
			}
			if tc.detail != "" && result.Detail != tc.detail {
				t.Fatalf("Detail = %q, want %q", result.Detail, tc.detail)
			}
			if len(f.tc.Store.Reviews) != 0 {
				t.Fatal("a refused verdict recorded a review row")
			}
		})
	}

	t.Run("accepted and duplicate carry no reason; a different verdict is conflicting", func(t *testing.T) {
		f := newReviewFixture(t)
		for _, want := range []app.ReviewOutcomeKind{app.ReviewAccepted, app.ReviewDuplicate} {
			if result := f.submit(t, "approve", f.SubjectCommit, []byte("ok")); result.Outcome != string(want) || result.Reason != "" {
				t.Fatalf("SubmitReviewVerdict(approve) = %+v, want %s with no reason", result, want)
			}
		}
		if result := f.submit(t, "reject", f.SubjectCommit, []byte("ok")); result.Outcome != string(app.ReviewConflicting) || result.Reason != app.GrammarReasonConflicting {
			t.Fatalf("SubmitReviewVerdict(reject) = %+v, want conflicting/%s", result, app.GrammarReasonConflicting)
		}
	})
}

// TestFakeSubmitReviewOrder pins the featureStore wrapper to the real
// store's order: existence and agreement first — malformed even for a
// caller that is not the reviewer — then any prior verdict, whatever the
// caller, then the reviewer session.
func TestFakeSubmitReviewOrder(t *testing.T) {
	notReviewer := func(f *reviewFixture, s *app.ReviewSubmission) {
		s.Session, s.IncarnationID = f.fr.ManagerID, f.fr.ManagerIncarnation
	}
	malformed := []struct {
		name   string
		mutate func(t *testing.T, f *reviewFixture, s *app.ReviewSubmission)
	}{
		{"an unknown task", func(t *testing.T, f *reviewFixture, s *app.ReviewSubmission) {
			t.Helper()
			id, err := identity.ParseTaskID(f.tc.IDs.NewID())
			if err != nil {
				t.Fatalf("parse task id: %v", err)
			}
			s.TaskID = id
		}},
		{"an unknown run", func(t *testing.T, f *reviewFixture, s *app.ReviewSubmission) {
			t.Helper()
			id, err := identity.ParseRunID(f.tc.IDs.NewID())
			if err != nil {
				t.Fatalf("parse run id: %v", err)
			}
			s.RunID = id
		}},
		{"an attempt of another task", func(t *testing.T, f *reviewFixture, s *app.ReviewSubmission) {
			t.Helper()
			task := seedImplementTask(t, f.tc, f.fr.RunID, 1, "B", false, run.TaskActive)
			other := seedBoundChild(t, f.tc, f.fr, task, 1, run.AttemptRunning, run.SessionActive)
			s.AttemptID = other.Attempt
		}},
	}
	for _, tc := range malformed {
		t.Run(tc.name+" is malformed before the reviewer check", func(t *testing.T) {
			f := newReviewFixture(t)
			submission := f.storeSubmission(t)
			tc.mutate(t, f, &submission)
			notReviewer(f, &submission)
			outcome, err := f.tc.Controller.Reviews.SubmitReview(context.Background(), submission)
			if err != nil || outcome.Kind != app.ReviewMalformed || outcome.Reason != app.GrammarReasonMalformed {
				t.Fatalf("SubmitReview() = %+v, %v; want malformed/%s", outcome, err, app.GrammarReasonMalformed)
			}
		})
	}

	t.Run("a prior verdict answers before the reviewer check", func(t *testing.T) {
		f := newReviewFixture(t)
		submission := f.storeSubmission(t)
		if outcome, err := f.tc.Controller.Reviews.SubmitReview(context.Background(), submission); err != nil || outcome.Kind != app.ReviewAccepted {
			t.Fatalf("SubmitReview() = %+v, %v; want accepted", outcome, err)
		}
		if !f.tc.Store.Tasks[f.TaskID].value.MailboxClosed {
			t.Fatal("the acceptance left the review task's mailbox open")
		}
		notReviewer(f, &submission)
		if outcome, err := f.tc.Controller.Reviews.SubmitReview(context.Background(), submission); err != nil || outcome.Kind != app.ReviewDuplicate {
			t.Fatalf("SubmitReview(not the reviewer, after acceptance) = %+v, %v; want duplicate", outcome, err)
		}
	})
}

// TestReviewRefusalReasonContract proves SubmitReviewVerdict refuses a store
// outcome whose reason its kind does not admit, so hop review submit never
// has to guess a refusal token.
func TestReviewRefusalReasonContract(t *testing.T) {
	invalid := []struct {
		kind   app.ReviewOutcomeKind
		reason string
	}{
		{app.ReviewStale, ""},
		{app.ReviewStale, app.GrammarReasonConflicting},
		{app.ReviewMalformed, ""},
		{app.ReviewMalformed, app.GrammarReasonStale},
		{app.ReviewConflicting, app.GrammarReasonStale},
		{app.ReviewAccepted, app.GrammarReasonStale},
		{app.ReviewDuplicate, app.GrammarReasonNotReviewer},
	}
	for _, c := range invalid {
		t.Run(string(c.kind)+" with reason "+c.reason, func(t *testing.T) {
			f := newReviewFixture(t)
			f.tc.Controller.Reviews = fixedReviewStore{outcome: app.ReviewOutcome{Kind: c.kind, Reason: c.reason}}
			result, err := f.tc.Controller.SubmitReviewVerdict(context.Background(), app.SubmitReviewRequest{
				RunID: f.fr.RunID.String(), TaskID: f.TaskID.String(), AttemptID: f.AttemptID.String(),
				SessionID: f.SessionID.String(), IncarnationID: f.IncarnationID.String(),
				Verdict: "approve", SubjectCommitOID: f.SubjectCommit, ReasonsBody: []byte("ok"),
			})
			if !errors.Is(err, app.ErrReviewReasonInvalid) || result != (app.SubmitReviewResult{}) {
				t.Fatalf("SubmitReviewVerdict() = %+v, %v; want ErrReviewReasonInvalid and no result", result, err)
			}
		})
	}
	t.Run("a transient outcome with a refusal reason", func(t *testing.T) {
		f := newReviewFixture(t)
		f.tc.Controller.Reviews = fixedReviewStore{outcome: app.ReviewOutcome{Kind: app.ReviewTransient, Transient: app.TransientAttemptNotRunning, Reason: app.GrammarReasonStale}}
		if _, err := f.submitErr(); !errors.Is(err, app.ErrReviewReasonInvalid) {
			t.Fatalf("SubmitReviewVerdict() error = %v, want ErrReviewReasonInvalid", err)
		}
	})
}

// submitErr submits the fixture reviewer's approve and returns the use
// case's own result and error unchecked.
func (f *reviewFixture) submitErr() (app.SubmitReviewResult, error) {
	return f.tc.Controller.SubmitReviewVerdict(context.Background(), app.SubmitReviewRequest{
		RunID: f.fr.RunID.String(), TaskID: f.TaskID.String(), AttemptID: f.AttemptID.String(),
		SessionID: f.SessionID.String(), IncarnationID: f.IncarnationID.String(),
		Verdict: "approve", SubjectCommitOID: f.SubjectCommit, ReasonsBody: []byte("ok"),
	})
}
