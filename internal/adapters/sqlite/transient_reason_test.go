package sqlite_test

import (
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// This file pins the real store's half of the typed transient reason
// contract (internal/app's fakes_transient_test.go is the fake's): every
// retryable result or verdict outcome names why it is retryable, so
// hop result submit and hop review submit print the one matching grammar
// line without reading Detail.

// TestSubmitResultTransientReasons proves both retryable result outcomes
// carry their reason, with Detail and the transient receipt unchanged, and
// that an accepted outcome carries none.
func TestSubmitResultTransientReasons(t *testing.T) {
	t.Run("an unsettled launch claim is attempt-not-running", func(t *testing.T) {
		f := newFixture(t)
		f.launchAttempt(t)
		f.createBinding(t)
		f.claimLaunch(t)

		outcome, err := f.store.SubmitResult(t.Context(), f.submission(8101, "digest-1"))
		if err != nil || outcome.Kind != app.SubmissionTransient || outcome.Transient != app.TransientAttemptNotRunning {
			t.Fatalf("SubmitResult() = %+v, %v; want transient/%s", outcome, err, app.TransientAttemptNotRunning)
		}
		if !strings.Contains(outcome.Detail, "attempt not yet running") {
			t.Fatalf("Detail = %q, want the domain error's own text", outcome.Detail)
		}
		if n := receiptCount(t, f.store, f.spec.RunID, app.SubmissionTransient); n != 1 {
			t.Fatalf("transient receipts = %d, want 1", n)
		}
	})

	t.Run("a pending mailbox is undelivered-messages until drained", func(t *testing.T) {
		clock := newFakeClock()
		store := openStoreAt(t, t.TempDir(), clock)
		f := newMailboxFixtureAt(t, store, clock)
		send := f.managerTaskSend(8102)
		if outcome, err := store.SendMessage(t.Context(), send); err != nil || outcome.Kind != app.MessageAccepted {
			t.Fatalf("seed pending message: %+v, %v", outcome, err)
		}

		outcome, err := store.SubmitResult(t.Context(), f.resultFor(8103))
		if err != nil || outcome.Kind != app.SubmissionTransient || outcome.Transient != app.TransientUndeliveredMessages {
			t.Fatalf("SubmitResult(pending mailbox) = %+v, %v; want transient/%s", outcome, err, app.TransientUndeliveredMessages)
		}

		fetch := app.MessageFetch{RunID: f.spec.RunID, SessionID: f.WorkerID, IncarnationID: f.WorkerIncarnation, Address: run.TaskAddress(f.TaskB)}
		if _, served, fetchErr := store.FetchNextMessage(t.Context(), fetch); fetchErr != nil || !served {
			t.Fatalf("drain fetch = %t, %v", served, fetchErr)
		}
		if ack, ackErr := store.AckMessage(t.Context(), app.MessageAck{RunID: f.spec.RunID, MessageID: send.ID, SessionID: f.WorkerID, IncarnationID: f.WorkerIncarnation}); ackErr != nil || ack.Kind != app.AckAccepted {
			t.Fatalf("drain ack = %+v, %v", ack, ackErr)
		}
		accepted, err := store.SubmitResult(t.Context(), f.resultFor(8103))
		if err != nil || accepted.Kind != app.SubmissionAccepted || accepted.Transient != "" {
			t.Fatalf("SubmitResult(after drain) = %+v, %v; want accepted with no reason", accepted, err)
		}
	})
}

// newLaunchingReviewFixture is newReviewFixture with the reviewer's attempt
// left launching and no launch claim at all: the early-submission window.
func newLaunchingReviewFixture(t *testing.T) *reviewFixture {
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
	})
	return &reviewFixture{
		featureFixture: f, ReviewTask: reviewTask, AttemptID: attemptID,
		ReviewerID: reviewerID, ReviewerIncarnation: reviewerIncarnation,
	}
}

// queueReviewMessage sends the review task one manager question.
func queueReviewMessage(t *testing.T, f *reviewFixture, n int) {
	t.Helper()
	send := app.MessageSend{
		ID: identity.MessageID(uid(n)), RunID: f.spec.RunID,
		Sender: run.SessionPrincipal(f.ManagerID), SenderAddress: run.ManagerAddress(),
		IncarnationID: f.ManagerIncarnation, Recipient: run.TaskAddress(f.ReviewTask), Kind: run.MessageQuestion,
		BodyPath: "/state/bodies/q.md", BodyDigest: "digest-q", BodyBytes: 4,
	}
	if outcome, err := f.store.SendMessage(t.Context(), send); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("seed pending message: %+v, %v", outcome, err)
	}
}

// TestSubmitReviewTransientReasons proves both retryable verdict outcomes
// carry their reason in AcceptVerdict's own order — a launching attempt is
// attempt-not-running even while its mailbox is also pending — with a
// transient receipt and no review row, and that an accepted verdict
// carries none.
func TestSubmitReviewTransientReasons(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) *reviewFixture
		want  app.TransientReason
	}{
		{"a pending mailbox is undelivered-messages", func(t *testing.T) *reviewFixture {
			f := newReviewFixture(t)
			queueReviewMessage(t, f, 8111)
			return f
		}, app.TransientUndeliveredMessages},
		{"an unsettled launch claim is attempt-not-running", newLaunchingReviewFixture, app.TransientAttemptNotRunning},
		{"a launching attempt with a pending mailbox is attempt-not-running", func(t *testing.T) *reviewFixture {
			f := newLaunchingReviewFixture(t)
			queueReviewMessage(t, f, 8112)
			return f
		}, app.TransientAttemptNotRunning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.setup(t)
			outcome, err := f.store.SubmitReview(t.Context(), f.submission(8113, run.VerdictApprove))
			if err != nil || outcome.Kind != app.ReviewTransient || outcome.Transient != tc.want {
				t.Fatalf("SubmitReview() = %+v, %v; want transient/%s", outcome, err, tc.want)
			}
			if n := countRows(t, f.store, `SELECT COUNT(*) FROM review_submissions WHERE outcome = 'transient'`); n != 1 {
				t.Fatalf("transient review receipts = %d, want 1", n)
			}
			if n := countRows(t, f.store, `SELECT COUNT(*) FROM reviews`); n != 0 {
				t.Fatalf("review rows after a transient verdict = %d, want 0", n)
			}
		})
	}

	t.Run("an accepted verdict carries no reason", func(t *testing.T) {
		f := newReviewFixture(t)
		outcome, err := f.store.SubmitReview(t.Context(), f.submission(8114, run.VerdictApprove))
		if err != nil || outcome.Kind != app.ReviewAccepted || outcome.Transient != "" {
			t.Fatalf("SubmitReview() = %+v, %v; want accepted with no reason", outcome, err)
		}
	})
}
