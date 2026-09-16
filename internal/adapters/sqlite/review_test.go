package sqlite_test

import (
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// reviewFixture is a feature fixture with a review task, its reviewer
// session and the reviewer's attempt driven to running.
type reviewFixture struct {
	*featureFixture
	ReviewTask          identity.TaskID
	AttemptID           identity.AttemptID
	ReviewerID          identity.SessionID
	ReviewerIncarnation identity.IncarnationID
}

func newReviewFixture(t *testing.T) *reviewFixture {
	t.Helper()
	f := newFeatureFixture(t)
	now := f.clock.Now()
	reviewTask := identity.TaskID(uid(7601))
	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		task := run.NewReviewTask(reviewTask, f.spec.RunID, 2, "commit-head", "tree-head", now)
		task.State = run.TaskActive
		if _, err := wf.TaskIndex().Create(t.Context(), task); err != nil {
			t.Fatalf("create review task: %v", err)
		}
	})
	reviewerID, reviewerIncarnation := f.createWorkerSession(t, reviewTask, run.RoleReviewer, 7602)
	attemptID := identity.AttemptID(uid(7602))
	f.inUOW(t, func(uow app.UnitOfWork) {
		saveAttempt(t, uow, attemptID, func(v run.Attempt) (run.Attempt, error) { return v.Launch(now) })
		saveAttempt(t, uow, attemptID, func(v run.Attempt) (run.Attempt, error) { return v.MarkRunning(now) })
	})
	return &reviewFixture{
		featureFixture: f, ReviewTask: reviewTask, AttemptID: attemptID,
		ReviewerID: reviewerID, ReviewerIncarnation: reviewerIncarnation,
	}
}

// submission builds a matching-subject review submission.
func (f *reviewFixture) submission(n int, verdict run.Verdict) app.ReviewSubmission {
	return app.ReviewSubmission{
		ID: identity.ReviewID(uid(n)), RunID: f.spec.RunID, TaskID: f.ReviewTask, AttemptID: f.AttemptID,
		Session: f.ReviewerID, IncarnationID: f.ReviewerIncarnation,
		SubjectCommitOID: "commit-head", SubjectTreeOID: "tree-head",
		Verdict: verdict, ReasonsPath: "/state/reasons/" + uid(n) + ".md", ReasonsDigest: "reasons-" + uid(n),
	}
}

// TestSubmitReviewAcceptance drives the atomic verdict handoff: the review
// row, the attempt and task completions, the closed mailbox, the
// controller notice to the manager and the acceptance receipt commit
// together; a repeat is duplicate and a different verdict conflicting.
func TestSubmitReviewAcceptance(t *testing.T) {
	f := newReviewFixture(t)

	submission := f.submission(7611, run.VerdictApprove)
	outcome, err := f.store.SubmitReview(t.Context(), submission)
	if err != nil || outcome.Kind != app.ReviewAccepted || outcome.ReviewID != submission.ID {
		t.Fatalf("SubmitReview() = %+v, %v; want accepted", outcome, err)
	}
	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		review, reviewErr := wf.Reviews().ByAttempt(t.Context(), f.AttemptID)
		if reviewErr != nil || review == nil || review.Verdict != run.VerdictApprove || review.SubjectCommitOID != "commit-head" {
			t.Fatalf("accepted review = %+v, %v", review, reviewErr)
		}
		attempt, _, getErr := uow.Attempts().Get(t.Context(), f.AttemptID)
		if getErr != nil || attempt.State != run.AttemptCompleted {
			t.Fatalf("attempt after verdict = %+v, %v; want completed", attempt, getErr)
		}
		task, _, taskErr := uow.Tasks().Get(t.Context(), f.ReviewTask)
		if taskErr != nil || task.State != run.TaskCompleted || !task.MailboxClosed {
			t.Fatalf("task after verdict = %+v, %v; want completed with the mailbox closed", task, taskErr)
		}
	})
	// The controller notice to the manager committed with the acceptance,
	// carrying the reasons artifact as its body.
	var noticeBody string
	if scanErr := writeDBRow(t, f.featureFixture, `SELECT body_path FROM messages WHERE run_id = ? AND sender_kind = 'controller' AND recipient_address = 'manager'`, f.spec.RunID.String()).Scan(&noticeBody); scanErr != nil || noticeBody != submission.ReasonsPath {
		t.Fatalf("controller notice body = %q, %v; want the reasons artifact", noticeBody, scanErr)
	}
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM review_submissions WHERE outcome = 'accepted'`); n != 1 {
		t.Fatalf("accepted review receipts = %d, want 1", n)
	}

	// An identical resubmission is the idempotent duplicate.
	duplicate, err := f.store.SubmitReview(t.Context(), submission)
	if err != nil || duplicate.Kind != app.ReviewDuplicate || duplicate.ReviewID != submission.ID {
		t.Fatalf("duplicate SubmitReview() = %+v, %v", duplicate, err)
	}
	// A different verdict against the accepted one is conflicting, and the
	// accepted review is undisturbed.
	conflicting := f.submission(7612, run.VerdictReject)
	conflictOutcome, err := f.store.SubmitReview(t.Context(), conflicting)
	if err != nil || conflictOutcome.Kind != app.ReviewConflicting || conflictOutcome.ReviewID != submission.ID {
		t.Fatalf("conflicting SubmitReview() = %+v, %v", conflictOutcome, err)
	}
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM reviews`); n != 1 {
		t.Fatalf("review rows = %d, want the one accepted verdict", n)
	}
}

// TestSubmitReviewRefusals drives the refusal matrix, each leaving a
// receipt.
func TestSubmitReviewRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(f *reviewFixture, s *app.ReviewSubmission)
		want   app.ReviewOutcomeKind
		detail string
	}{
		{
			name:   "subject mismatch is stale",
			mutate: func(_ *reviewFixture, s *app.ReviewSubmission) { s.SubjectCommitOID = "another-commit" },
			want:   app.ReviewStale, detail: "verdict subject mismatch",
		},
		{
			name:   "stale incarnation",
			mutate: func(_ *reviewFixture, s *app.ReviewSubmission) { s.IncarnationID = identity.IncarnationID(uid(9996)) },
			want:   app.ReviewStale, detail: "incarnation is not current",
		},
		{
			name: "non-reviewer caller",
			mutate: func(f *reviewFixture, s *app.ReviewSubmission) {
				s.Session, s.IncarnationID = f.ManagerID, f.ManagerIncarnation
			},
			want: app.ReviewStale, detail: "caller is not the review task's reviewer session",
		},
		{
			name:   "wrong attempt is malformed",
			mutate: func(f *reviewFixture, s *app.ReviewSubmission) { s.AttemptID = f.spec.AttemptID },
			want:   app.ReviewMalformed, detail: "attempt/task/run do not agree",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t)
			submission := f.submission(7621, run.VerdictApprove)
			tc.mutate(f, &submission)
			before := countRows(t, f.store, `SELECT COUNT(*) FROM review_submissions`)
			outcome, err := f.store.SubmitReview(t.Context(), submission)
			if err != nil {
				t.Fatalf("SubmitReview() error = %v", err)
			}
			if outcome.Kind != tc.want || !strings.Contains(outcome.Detail, tc.detail) {
				t.Fatalf("SubmitReview() = %+v, want %s with %q", outcome, tc.want, tc.detail)
			}
			if after := countRows(t, f.store, `SELECT COUNT(*) FROM review_submissions`); after != before+1 {
				t.Fatalf("refusal receipts %d -> %d, want one appended", before, after)
			}
			if n := countRows(t, f.store, `SELECT COUNT(*) FROM reviews`); n != 0 {
				t.Fatalf("review rows after refusal = %d, want none", n)
			}
		})
	}

	t.Run("pending mailbox is the retryable transient", func(t *testing.T) {
		f := newReviewFixture(t)
		// The manager sends the reviewer a question; until it is drained
		// the verdict is refused transient.
		send := app.MessageSend{
			ID: identity.MessageID(uid(7631)), RunID: f.spec.RunID,
			Sender: run.SessionPrincipal(f.ManagerID), SenderAddress: run.ManagerAddress(),
			IncarnationID: f.ManagerIncarnation, Recipient: run.TaskAddress(f.ReviewTask), Kind: run.MessageQuestion,
			BodyPath: "/state/bodies/q.md", BodyDigest: "digest-q", BodyBytes: 4,
		}
		if outcome, err := f.store.SendMessage(t.Context(), send); err != nil || outcome.Kind != app.MessageAccepted {
			t.Fatalf("seed pending message: %+v, %v", outcome, err)
		}
		outcome, err := f.store.SubmitReview(t.Context(), f.submission(7632, run.VerdictApprove))
		if err != nil || outcome.Kind != app.ReviewTransient {
			t.Fatalf("SubmitReview(pending mailbox) = %+v, %v; want transient", outcome, err)
		}
		// Drain: fetch and ack at the task address, then resubmit.
		fetch := app.MessageFetch{RunID: f.spec.RunID, SessionID: f.ReviewerID, IncarnationID: f.ReviewerIncarnation, Address: run.TaskAddress(f.ReviewTask)}
		if _, served, fetchErr := f.store.FetchNextMessage(t.Context(), fetch); fetchErr != nil || !served {
			t.Fatalf("drain fetch: %t, %v", served, fetchErr)
		}
		if ackOutcome, ackErr := f.store.AckMessage(t.Context(), app.MessageAck{RunID: f.spec.RunID, MessageID: send.ID, SessionID: f.ReviewerID, IncarnationID: f.ReviewerIncarnation}); ackErr != nil || ackOutcome.Kind != app.AckAccepted {
			t.Fatalf("drain ack: %+v, %v", ackOutcome, ackErr)
		}
		resubmit, err := f.store.SubmitReview(t.Context(), f.submission(7633, run.VerdictApprove))
		if err != nil || resubmit.Kind != app.ReviewAccepted {
			t.Fatalf("resubmit after drain = %+v, %v; want accepted", resubmit, err)
		}
	})
}
