package sqlite_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// Pending launch intent shapes of the pre-binding window: the session's
// newest pending pane.open names the caller's incarnation (agreeing),
// another incarnation (disagreeing), or no intent exists (absent).
const (
	intentAgreeing    = "agreeing intent"
	intentDisagreeing = "disagreeing intent"
	intentAbsent      = "absent intent"
)

// preBindingWindow is a child session in the launch window before its
// binding row exists: task active, attempt and session launching, no
// binding, and — with an agreeing intent — the caller's own exec_pending
// launch claim.
type preBindingWindow struct {
	*featureFixture
	Task        identity.TaskID
	Attempt     identity.AttemptID
	Session     identity.SessionID
	Incarnation identity.IncarnationID
}

func newPreBindingWindow(t *testing.T, role run.Role, intent string) *preBindingWindow {
	t.Helper()
	f := newFeatureFixture(t)
	now := f.clock.Now()
	w := &preBindingWindow{
		featureFixture: f,
		Task:           identity.TaskID(uid(9601)),
		Attempt:        identity.AttemptID(uid(9602)),
		Session:        identity.SessionID(uid(9603)),
		Incarnation:    identity.IncarnationID(uid(9604)),
	}
	manager := f.managerSessionValue(t)
	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		task := run.NewImplementTask(w.Task, f.spec.RunID, 2, "pre-binding task", "instructions-digest", false, now)
		if role == run.RoleReviewer {
			task = run.NewReviewTask(w.Task, f.spec.RunID, 2, "commit-head", "tree-head", now)
		}
		task.State = run.TaskActive
		if _, err := wf.TaskIndex().Create(t.Context(), task); err != nil {
			t.Fatalf("create task: %v", err)
		}
		attempt, err := run.NewAttempt(w.Attempt, w.Task, 1, now)
		if err != nil {
			t.Fatalf("new attempt: %v", err)
		}
		if attempt, err = attempt.Launch(now); err != nil {
			t.Fatalf("launch attempt: %v", err)
		}
		if _, createErr := wf.AttemptIndex().Create(t.Context(), attempt); createErr != nil {
			t.Fatalf("create attempt: %v", createErr)
		}
		session, err := run.NewChildSession(w.Session, f.spec.RunID, w.Attempt, role, manager, run.HarnessClaude, now)
		if err != nil {
			t.Fatalf("new child session: %v", err)
		}
		if session, err = session.Launch(now); err != nil {
			t.Fatalf("launch child session: %v", err)
		}
		if _, err := uow.Sessions().Create(t.Context(), session); err != nil {
			t.Fatalf("create child session: %v", err)
		}
	})
	switch intent {
	case intentAgreeing:
		createLaunchIntentFor(t, f, 9605, w.Session, w.Incarnation)
		if err := f.store.ClaimLaunch(t.Context(), claimFor(f, w.Incarnation, w.Session, w.Attempt, 4242)); err != nil {
			t.Fatalf("ClaimLaunch() = %v", err)
		}
	case intentDisagreeing:
		other := identity.IncarnationID(uid(9607))
		createLaunchIntentFor(t, f, 9605, w.Session, other)
		if err := f.store.ClaimLaunch(t.Context(), claimFor(f, other, w.Session, w.Attempt, 4343)); err != nil {
			t.Fatalf("ClaimLaunch(other incarnation) = %v", err)
		}
	case intentAbsent:
	default:
		t.Fatalf("unknown intent shape %q", intent)
	}
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM runtime_bindings WHERE session_id = ?`, w.Session.String()); n != 0 {
		t.Fatalf("binding rows = %d, want none in the pre-binding window", n)
	}
	return w
}

func (w *preBindingWindow) result(n int) app.ResultSubmission {
	return app.ResultSubmission{
		ID: identity.ResultID(uid(n)), RunID: w.spec.RunID, TaskID: w.Task, AttemptID: w.Attempt,
		IncarnationID: w.Incarnation, CommitOID: strings.Repeat("d", 40), Summary: "early", Digest: "digest-" + uid(n),
	}
}

func (w *preBindingWindow) review(n int) app.ReviewSubmission {
	return app.ReviewSubmission{
		ID: identity.ReviewID(uid(n)), RunID: w.spec.RunID, TaskID: w.Task, AttemptID: w.Attempt,
		Session: w.Session, IncarnationID: w.Incarnation, SubjectCommitOID: "commit-head", SubjectTreeOID: "tree-head",
		Verdict: run.VerdictApprove, ReasonsPath: "/state/reasons/" + uid(n) + ".md", ReasonsDigest: "reasons-" + uid(n),
	}
}

func (w *preBindingWindow) fetch() app.MessageFetch {
	return app.MessageFetch{RunID: w.spec.RunID, SessionID: w.Session, IncarnationID: w.Incarnation, Address: run.TaskAddress(w.Task)}
}

func (w *preBindingWindow) ack(message identity.MessageID) app.MessageAck {
	return app.MessageAck{RunID: w.spec.RunID, MessageID: message, SessionID: w.Session, IncarnationID: w.Incarnation}
}

// TestPreBindingWindowResultAndMessaging pins the implementer's verbs in
// the launch window before its binding row exists (LAUNCH-7). With an
// agreeing pending intent the caller is current: a result is
// attempt-not-running whether or not a message is queued, the queued
// message is served, an ack before any fetch is not-delivered and one
// after it is accepted. With a disagreeing or absent intent the caller is
// stale: a result is stale whatever the mailbox holds, a fetch is
// unauthorized, and an ack of a message delivered to that incarnation is
// stale.
func TestPreBindingWindowResultAndMessaging(t *testing.T) {
	for _, intent := range []string{intentAgreeing, intentDisagreeing, intentAbsent} {
		for _, queued := range []bool{false, true} {
			name := intent + "/empty mailbox"
			if queued {
				name = intent + "/queued message"
			}
			t.Run(name, func(t *testing.T) {
				w := newPreBindingWindow(t, run.RoleImplementer, intent)
				var message identity.MessageID
				if queued {
					message = queueTaskInfo(t, w.featureFixture, w.Task, 9610)
				}
				current := intent == intentAgreeing
				submit := func(step string, n int) {
					t.Helper()
					outcome, err := w.store.SubmitResult(t.Context(), w.result(n))
					switch {
					case err != nil:
						t.Errorf("%s: SubmitResult() error = %v", step, err)
					case current && (outcome.Kind != app.SubmissionTransient || outcome.Transient != app.TransientAttemptNotRunning):
						t.Errorf("%s: SubmitResult() = %+v, want transient/%s", step, outcome, app.TransientAttemptNotRunning)
					case !current && outcome.Kind != app.SubmissionStale:
						t.Errorf("%s: SubmitResult() = %+v, want stale", step, outcome)
					}
				}
				submit("result submit", 9611)

				if !current {
					if _, served, err := w.store.FetchNextMessage(t.Context(), w.fetch()); served || !errors.Is(err, app.ErrMessagingUnauthorized) || !strings.Contains(err.Error(), "is not current") {
						t.Errorf("fetch: served=%t err=%v, want the not-current refusal", served, err)
					}
					if queued {
						// A delivery row for exactly this incarnation, so the ack
						// reaches the incarnation check.
						rawExec(t, w.store, `INSERT INTO message_deliveries (id, message_id, session_id, incarnation_id, delivered_at) VALUES (?, ?, ?, ?, ?)`,
							uid(9612), message.String(), w.Session.String(), w.Incarnation.String(), "2026-09-14T09:00:00.000000000Z")
						if ack, err := w.store.AckMessage(t.Context(), w.ack(message)); err != nil || ack.Kind != app.AckRefused || ack.Reason != app.GrammarReasonStale {
							t.Errorf("ack: %+v, %v; want refused stale", ack, err)
						}
						submit("result resubmit", 9613)
					}
					if n := countRows(t, w.store, `SELECT COUNT(*) FROM message_acks`); n != 0 {
						t.Errorf("ack rows = %d, want none", n)
					}
					return
				}

				if !queued {
					if delivery, served, err := w.store.FetchNextMessage(t.Context(), w.fetch()); served || err != nil {
						t.Errorf("fetch of an empty mailbox = %+v, %t, %v; want nothing and no error", delivery, served, err)
					}
					return
				}
				if ack, err := w.store.AckMessage(t.Context(), w.ack(message)); err != nil || ack.Kind != app.AckRefused || ack.Reason != app.GrammarReasonNotDelivered {
					t.Errorf("ack before any fetch: %+v, %v; want refused not-delivered", ack, err)
				}
				if delivery, served, err := w.store.FetchNextMessage(t.Context(), w.fetch()); err != nil || !served || delivery.Message.ID != message {
					t.Errorf("fetch: %+v, %t, %v; want the queued message served", delivery, served, err)
				}
				if ack, err := w.store.AckMessage(t.Context(), w.ack(message)); err != nil || ack.Kind != app.AckAccepted {
					t.Errorf("ack after the fetch: %+v, %v; want accepted", ack, err)
				}
				submit("result resubmit after the drain", 9613)
			})
		}
	}
}

// TestPreBindingWindowReviewSubmit pins the reviewer's verdict in the same
// window (LAUNCH-7): attempt-not-running with an agreeing pending intent
// and stale with a disagreeing or absent one, whether or not a message is
// queued to the review task, with no review row either way.
func TestPreBindingWindowReviewSubmit(t *testing.T) {
	for _, intent := range []string{intentAgreeing, intentDisagreeing, intentAbsent} {
		for _, queued := range []bool{false, true} {
			name := intent + "/empty mailbox"
			if queued {
				name = intent + "/queued message"
			}
			t.Run(name, func(t *testing.T) {
				w := newPreBindingWindow(t, run.RoleReviewer, intent)
				if queued {
					queueTaskInfo(t, w.featureFixture, w.Task, 9620)
				}
				outcome, err := w.store.SubmitReview(t.Context(), w.review(9621))
				switch {
				case err != nil:
					t.Fatalf("SubmitReview() error = %v", err)
				case intent == intentAgreeing && (outcome.Kind != app.ReviewTransient || outcome.Transient != app.TransientAttemptNotRunning):
					t.Errorf("SubmitReview() = %+v, want transient/%s", outcome, app.TransientAttemptNotRunning)
				case intent != intentAgreeing && outcome.Kind != app.ReviewStale:
					t.Errorf("SubmitReview() = %+v, want stale", outcome)
				}
				if n := countRows(t, w.store, `SELECT COUNT(*) FROM reviews`); n != 0 {
					t.Errorf("review rows = %d, want none", n)
				}
			})
		}
	}
}
