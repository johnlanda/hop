package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/run"
)

// This file is the fake-side half of the typed transient reason contract
// internal/adapters/sqlite's transient_reason_test.go pins against the real
// store: each retryable result or verdict outcome names why, the driving
// use cases carry that reason to the CLI unchanged, and a store outcome
// whose reason does not match its kind is an error rather than a guessed
// retry line.

// TestFakeSubmitResultTransientReasons drives both retryable result
// outcomes through Controller.SubmitResult over the fakes.
func TestFakeSubmitResultTransientReasons(t *testing.T) {
	t.Run("an unsettled launch claim is attempt-not-running", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)

		result, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail))
		if err != nil || result.Kind != string(app.SubmissionTransient) || result.TransientReason != string(app.TransientAttemptNotRunning) {
			t.Fatalf("SubmitResult() = %+v, %v; want transient/%s", result, err, app.TransientAttemptNotRunning)
		}
		last := tc.Store.Submissions[len(tc.Store.Submissions)-1]
		if last.Transient != app.TransientAttemptNotRunning || last.Detail != "run: attempt not yet running: attempt "+detail.AttemptID.String() {
			t.Fatalf("store outcome = %+v, want reason %s with the domain error's own detail, as the real store records", last, app.TransientAttemptNotRunning)
		}
	})

	t.Run("a pending mailbox is undelivered-messages until drained", func(t *testing.T) {
		f := newMailboxFixture(t)
		f.tc.Controller.Submissions = f.store
		if outcome := f.send(t, "pending"); outcome.Kind != app.MessageAccepted {
			t.Fatalf("send = %+v, want accepted", outcome)
		}
		request := app.SubmitResultRequest{
			RunID: f.fr.RunID.String(), TaskID: f.TaskID.String(), AttemptID: f.w.AttemptID.String(),
			IncarnationID: f.w.IncarnationID.String(), CommitOID: "cccccccccccccccccccccccccccccccccccccccc", Summary: "done",
		}

		result, err := f.tc.Controller.SubmitResult(context.Background(), request)
		if err != nil || result.Kind != string(app.SubmissionTransient) || result.TransientReason != string(app.TransientUndeliveredMessages) {
			t.Fatalf("SubmitResult(pending mailbox) = %+v, %v; want transient/%s", result, err, app.TransientUndeliveredMessages)
		}
		if result.Detail != app.GrammarTransientUndeliveredLine {
			t.Fatalf("SubmitResult(pending mailbox) detail = %q, want the drain line, as the real store records", result.Detail)
		}

		f.drain(t)
		result, err = f.tc.Controller.SubmitResult(context.Background(), request)
		if err != nil || result.Kind != string(app.SubmissionAccepted) || result.TransientReason != "" {
			t.Fatalf("SubmitResult(after drain) = %+v, %v; want accepted with no reason", result, err)
		}
	})
}

// TestFakeSubmitReviewTransientReasons drives both retryable verdict
// outcomes through Controller.SubmitReviewVerdict, including AcceptVerdict's
// own order: a launching attempt is attempt-not-running even while its
// mailbox is also pending.
func TestFakeSubmitReviewTransientReasons(t *testing.T) {
	queueMessage := func(t *testing.T, f *reviewFixture) {
		t.Helper()
		msgID := mintMessageID(t, f.tc)
		f.tc.Store.Messages[msgID] = run.NewInfo(msgID, f.fr.RunID, run.ControllerPrincipal(), run.TaskAddress(f.TaskID), "", "/state/m", "d", 1, 1, f.tc.Clock.Now())
	}
	unsettle := func(f *reviewFixture) {
		f.tc.Store.Attempts[f.AttemptID].value.State = run.AttemptLaunching
		claim := f.tc.Store.LaunchClaims[f.IncarnationID]
		claim.State = app.LaunchClaimExecPending
		f.tc.Store.LaunchClaims[f.IncarnationID] = claim
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, f *reviewFixture)
		want  app.TransientReason
	}{
		{"a pending mailbox is undelivered-messages", queueMessage, app.TransientUndeliveredMessages},
		{"an unsettled launch claim is attempt-not-running", func(_ *testing.T, f *reviewFixture) { unsettle(f) }, app.TransientAttemptNotRunning},
		{"a launching attempt with a pending mailbox is attempt-not-running", func(t *testing.T, f *reviewFixture) {
			unsettle(f)
			queueMessage(t, f)
		}, app.TransientAttemptNotRunning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t)
			tc.setup(t, f)
			result := f.submit(t, "approve", f.SubjectCommit, []byte("ok"))
			if result.Outcome != string(app.ReviewTransient) || result.TransientReason != string(tc.want) {
				t.Fatalf("SubmitReviewVerdict() = %+v, want transient/%s", result, tc.want)
			}
			if _, recorded := f.tc.Store.Reviews[f.AttemptID]; recorded {
				t.Fatal("a transient verdict recorded a review row")
			}
		})
	}

	t.Run("an accepted verdict carries no reason", func(t *testing.T) {
		f := newReviewFixture(t)
		if result := f.submit(t, "approve", f.SubjectCommit, []byte("ok")); result.Outcome != string(app.ReviewAccepted) || result.TransientReason != "" {
			t.Fatalf("SubmitReviewVerdict() = %+v, want accepted with no reason", result)
		}
	})
}

// fixedSubmissionStore returns one scripted outcome from SubmitResult.
type fixedSubmissionStore struct {
	app.SubmissionStore
	outcome app.SubmissionOutcome
}

func (s fixedSubmissionStore) SubmitResult(context.Context, app.ResultSubmission) (app.SubmissionOutcome, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	return s.outcome, nil
}

// fixedReviewStore returns one scripted outcome from SubmitReview.
type fixedReviewStore struct{ outcome app.ReviewOutcome }

func (s fixedReviewStore) SubmitReview(context.Context, app.ReviewSubmission) (app.ReviewOutcome, error) { //nolint:gocritic // hugeParam: a scripted port value, assigned by value in each test like fixedSubmissionStore.
	return s.outcome, nil
}

// TestSubmissionTransientReasonContract proves both driving use cases
// refuse a store outcome whose reason does not match its kind — a
// transient outcome with no or an unknown reason, or a reason on a
// non-transient outcome — so no CLI ever has to guess a retry line.
func TestSubmissionTransientReasonContract(t *testing.T) {
	invalid := []struct {
		name   string
		kind   string
		reason app.TransientReason
	}{
		{"a transient outcome with no reason", "transient", ""},
		{"a transient outcome with an unknown reason", "transient", "run-not-running"},
		{"an accepted outcome with a reason", "accepted", app.TransientUndeliveredMessages},
	}
	for _, c := range invalid {
		t.Run("result: "+c.name, func(t *testing.T) {
			tc := newTestController(defaultPolicy())
			_, detail := startedRun(t, tc)
			tc.Controller.Submissions = fixedSubmissionStore{SubmissionStore: tc.Store, outcome: app.SubmissionOutcome{Kind: app.SubmissionOutcomeKind(c.kind), Transient: c.reason}}
			result, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail))
			if !errors.Is(err, app.ErrTransientReasonInvalid) || result != (app.SubmitResultResult{}) {
				t.Fatalf("SubmitResult() = %+v, %v; want ErrTransientReasonInvalid and no result", result, err)
			}
		})
		t.Run("review: "+c.name, func(t *testing.T) {
			f := newReviewFixture(t)
			f.tc.Controller.Reviews = fixedReviewStore{outcome: app.ReviewOutcome{Kind: app.ReviewOutcomeKind(c.kind), Transient: c.reason}}
			result, err := f.tc.Controller.SubmitReviewVerdict(context.Background(), app.SubmitReviewRequest{
				RunID: f.fr.RunID.String(), TaskID: f.TaskID.String(), AttemptID: f.AttemptID.String(),
				SessionID: f.SessionID.String(), IncarnationID: f.IncarnationID.String(),
				Verdict: "approve", SubjectCommitOID: f.SubjectCommit, ReasonsBody: []byte("ok"),
			})
			if !errors.Is(err, app.ErrTransientReasonInvalid) || result != (app.SubmitReviewResult{}) {
				t.Fatalf("SubmitReviewVerdict() = %+v, %v; want ErrTransientReasonInvalid and no result", result, err)
			}
		})
	}
}
