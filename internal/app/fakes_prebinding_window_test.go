package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// Pending launch intent shapes of the pre-binding window, as the sqlite
// adapter's prebinding_window_test.go names them.
const (
	fakeIntentAgreeing    = "agreeing intent"
	fakeIntentDisagreeing = "disagreeing intent"
	fakeIntentAbsent      = "absent intent"
)

// fakePreBindingWindow is the fake half of the sqlite adapter's
// preBindingWindow: a child session of role launching on a launching
// attempt with no binding, and the pending intent shape the test names.
type fakePreBindingWindow struct {
	tc          *testController
	fr          featureRun
	store       *featureStore
	Task        identity.TaskID
	Attempt     identity.AttemptID
	Session     identity.SessionID
	Incarnation identity.IncarnationID
}

func newFakePreBindingWindow(t *testing.T, role run.Role, intent string) *fakePreBindingWindow {
	t.Helper()
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	now := tc.Clock.Now()
	mint := func() string { return tc.IDs.NewID() }
	w := &fakePreBindingWindow{
		tc: tc, fr: fr, store: &featureStore{fakeStore: tc.Store},
		Task: identity.TaskID(mint()), Attempt: identity.AttemptID(mint()),
		Session: identity.SessionID(mint()), Incarnation: identity.IncarnationID(mint()),
	}
	task := run.NewImplementTask(w.Task, fr.RunID, 1, "pre-binding task", "instructions-digest", false, now)
	if role == run.RoleReviewer {
		task = run.NewReviewTask(w.Task, fr.RunID, 1, "commit-head", fakeSubjectTree, now)
	}
	task.State = run.TaskActive
	tc.Store.Tasks[w.Task] = &entityRow[run.Task]{value: task, revision: 1}
	attempt, err := run.NewAttempt(w.Attempt, w.Task, 1, now)
	if err != nil {
		t.Fatalf("NewAttempt() error = %v", err)
	}
	if attempt, err = attempt.Launch(now); err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	tc.Store.Attempts[w.Attempt] = &entityRow[run.Attempt]{value: attempt, revision: 1}
	session, err := run.NewChildSession(w.Session, fr.RunID, w.Attempt, role, tc.Store.Sessions[fr.ManagerID].value, run.HarnessClaude, now)
	if err != nil {
		t.Fatalf("NewChildSession() error = %v", err)
	}
	if session, err = session.Launch(now); err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	tc.Store.Sessions[w.Session] = &entityRow[run.Session]{value: session, revision: 1}

	intend := func(incarnation identity.IncarnationID, pid int) {
		opID := identity.OperationID(mint())
		tc.Store.Operations[opID] = app.Operation{
			ID: opID, RunID: fr.RunID, Kind: app.OpPaneOpen, State: app.OperationPending,
			Intent:    map[string]any{"session_id": w.Session.String(), "incarnation_id": incarnation.String(), "creation_label": "label-" + opID.String()},
			CreatedAt: now, UpdatedAt: now,
		}
		if err := tc.Store.ClaimLaunch(context.Background(), app.LaunchClaim{
			IncarnationID: incarnation, RunID: fr.RunID, SessionID: w.Session, AttemptID: w.Attempt,
			Executable: "/opt/harness/claude", ArgvDigest: "argv-digest", PID: pid, ClaimedAt: now,
		}); err != nil {
			t.Fatalf("ClaimLaunch() error = %v", err)
		}
	}
	switch intent {
	case fakeIntentAgreeing:
		intend(w.Incarnation, 4242)
	case fakeIntentDisagreeing:
		intend(identity.IncarnationID(mint()), 4343)
	case fakeIntentAbsent:
	default:
		t.Fatalf("unknown intent shape %q", intent)
	}
	if n := len(tc.Store.Bindings[w.Session]); n != 0 {
		t.Fatalf("binding rows = %d, want none in the pre-binding window", n)
	}
	return w
}

func (w *fakePreBindingWindow) queue(t *testing.T) identity.MessageID {
	t.Helper()
	id := mintMessageID(t, w.tc)
	outcome, err := w.tc.Store.SendMessage(context.Background(), app.MessageSend{
		ID: id, RunID: w.fr.RunID, Sender: run.SessionPrincipal(w.fr.ManagerID), SenderAddress: run.ManagerAddress(),
		IncarnationID: w.fr.ManagerIncarnation, Recipient: run.TaskAddress(w.Task), Kind: run.MessageInfo,
		BodyPath: "/state/b", BodyDigest: "d-" + id.String(), BodyBytes: 1,
	})
	if err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("queue info = %+v, %v", outcome, err)
	}
	return id
}

func (w *fakePreBindingWindow) fetch() (app.MessageDelivery, bool, error) {
	return w.tc.Store.FetchNextMessage(context.Background(), app.MessageFetch{RunID: w.fr.RunID, SessionID: w.Session, IncarnationID: w.Incarnation, Address: run.TaskAddress(w.Task)})
}

func (w *fakePreBindingWindow) ack(message identity.MessageID) (app.MessageAckOutcome, error) {
	return w.tc.Store.AckMessage(context.Background(), app.MessageAck{RunID: w.fr.RunID, MessageID: message, SessionID: w.Session, IncarnationID: w.Incarnation})
}

// TestFakePreBindingWindowResultAndMessaging is the fake half of the sqlite
// adapter's TestPreBindingWindowResultAndMessaging, shape for shape.
func TestFakePreBindingWindowResultAndMessaging(t *testing.T) {
	for _, intent := range []string{fakeIntentAgreeing, fakeIntentDisagreeing, fakeIntentAbsent} {
		for _, queued := range []bool{false, true} {
			name := intent + "/empty mailbox"
			if queued {
				name = intent + "/queued message"
			}
			t.Run(name, func(t *testing.T) {
				w := newFakePreBindingWindow(t, run.RoleImplementer, intent)
				var message identity.MessageID
				if queued {
					message = w.queue(t)
				}
				current := intent == fakeIntentAgreeing
				submit := func(step string) {
					t.Helper()
					resultID, err := identity.ParseResultID(w.tc.IDs.NewID())
					if err != nil {
						t.Fatalf("parse result id: %v", err)
					}
					outcome, err := w.store.SubmitResult(context.Background(), app.ResultSubmission{
						ID: resultID, RunID: w.fr.RunID, TaskID: w.Task, AttemptID: w.Attempt,
						IncarnationID: w.Incarnation, CommitOID: strings.Repeat("d", 40), Summary: "early", Digest: "d-" + resultID.String(),
					})
					switch {
					case err != nil:
						t.Errorf("%s: SubmitResult() error = %v", step, err)
					case current && (outcome.Kind != app.SubmissionTransient || outcome.Transient != app.TransientAttemptNotRunning):
						t.Errorf("%s: SubmitResult() = %+v, want transient/%s", step, outcome, app.TransientAttemptNotRunning)
					case !current && outcome.Kind != app.SubmissionStale:
						t.Errorf("%s: SubmitResult() = %+v, want stale", step, outcome)
					}
				}
				submit("result submit")

				if !current {
					if _, served, err := w.fetch(); served || !errors.Is(err, app.ErrMessagingUnauthorized) || !strings.Contains(err.Error(), "is not current") {
						t.Errorf("fetch: served=%t err=%v, want the not-current refusal", served, err)
					}
					if queued {
						w.tc.Store.MessageDeliveries[message] = append(w.tc.Store.MessageDeliveries[message], run.Delivery{
							MessageID: message, SessionID: w.Session, IncarnationID: w.Incarnation, At: w.tc.Clock.Now(),
						})
						if ack, err := w.ack(message); err != nil || ack.Kind != app.AckRefused || ack.Reason != app.GrammarReasonStale {
							t.Errorf("ack: %+v, %v; want refused stale", ack, err)
						}
						submit("result resubmit")
					}
					if n := len(w.tc.Store.MessageAcks); n != 0 {
						t.Errorf("acks = %d, want none", n)
					}
					return
				}

				if !queued {
					if delivery, served, err := w.fetch(); served || err != nil {
						t.Errorf("fetch of an empty mailbox = %+v, %t, %v; want nothing and no error", delivery, served, err)
					}
					return
				}
				if ack, err := w.ack(message); err != nil || ack.Kind != app.AckRefused || ack.Reason != app.GrammarReasonNotDelivered {
					t.Errorf("ack before any fetch: %+v, %v; want refused not-delivered", ack, err)
				}
				if delivery, served, err := w.fetch(); err != nil || !served || delivery.Message.ID != message {
					t.Errorf("fetch: %+v, %t, %v; want the queued message served", delivery, served, err)
				}
				if ack, err := w.ack(message); err != nil || ack.Kind != app.AckAccepted {
					t.Errorf("ack after the fetch: %+v, %v; want accepted", ack, err)
				}
				submit("result resubmit after the drain")
			})
		}
	}
}

// TestFakePreBindingWindowReviewSubmit is the fake half of the sqlite
// adapter's TestPreBindingWindowReviewSubmit.
func TestFakePreBindingWindowReviewSubmit(t *testing.T) {
	for _, intent := range []string{fakeIntentAgreeing, fakeIntentDisagreeing, fakeIntentAbsent} {
		for _, queued := range []bool{false, true} {
			name := intent + "/empty mailbox"
			if queued {
				name = intent + "/queued message"
			}
			t.Run(name, func(t *testing.T) {
				w := newFakePreBindingWindow(t, run.RoleReviewer, intent)
				if queued {
					w.queue(t)
				}
				reviewID, err := identity.ParseReviewID(w.tc.IDs.NewID())
				if err != nil {
					t.Fatalf("parse review id: %v", err)
				}
				outcome, err := w.store.SubmitReview(context.Background(), app.ReviewSubmission{
					ID: reviewID, RunID: w.fr.RunID, TaskID: w.Task, AttemptID: w.Attempt,
					Session: w.Session, IncarnationID: w.Incarnation, SubjectCommitOID: "commit-head", SubjectTreeOID: fakeSubjectTree,
					Verdict: run.VerdictApprove, ReasonsPath: "/state/reasons", ReasonsDigest: "reasons",
				})
				switch {
				case err != nil:
					t.Fatalf("SubmitReview() error = %v", err)
				case intent == fakeIntentAgreeing && (outcome.Kind != app.ReviewTransient || outcome.Transient != app.TransientAttemptNotRunning):
					t.Errorf("SubmitReview() = %+v, want transient/%s", outcome, app.TransientAttemptNotRunning)
				case intent != fakeIntentAgreeing && outcome.Kind != app.ReviewStale:
					t.Errorf("SubmitReview() = %+v, want stale", outcome)
				}
				if _, recorded := w.tc.Store.Reviews[w.Attempt]; recorded {
					t.Error("a refused verdict recorded a review")
				}
			})
		}
	}
}
