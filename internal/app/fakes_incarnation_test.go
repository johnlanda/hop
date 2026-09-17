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

// incarnationPrincipal is one session whose incarnation currency a verb
// decides, with the incarnation its seeded binding carries.
type incarnationPrincipal struct {
	fr      featureRun
	session identity.SessionID
	bound   identity.IncarnationID
	task    identity.TaskID
	attempt identity.AttemptID
}

// seedPendingLaunchIntent journals a pending pane.open naming session and
// incarnation: the controller's pre-dispatch write whose outcome a lost
// controller never recorded.
func seedPendingLaunchIntent(t *testing.T, tc *testController, runID identity.RunID, session identity.SessionID, incarnation identity.IncarnationID) {
	t.Helper()
	opID, err := identity.ParseOperationID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse operation id: %v", err)
	}
	now := tc.Clock.Now()
	tc.Store.Operations[opID] = app.Operation{
		ID: opID, RunID: runID, Generation: 1, Kind: app.OpPaneOpen, State: app.OperationPending,
		Intent:    map[string]any{"session_id": session.String(), "incarnation_id": incarnation.String(), "label": opID.String()},
		CreatedAt: now, UpdatedAt: now,
	}
}

// incarnationShape puts one principal into a state of the one
// principal-incarnation rule and returns the caller incarnations it must
// accept and refuse.
type incarnationShape struct {
	name    string
	arrange func(t *testing.T, tc *testController, p incarnationPrincipal, unbound identity.IncarnationID) (current, stale []identity.IncarnationID)
}

// incarnationShapes mirror internal/adapters/sqlite's principalShapes
// against the fake: the four states the rule distinguishes.
func incarnationShapes() []incarnationShape {
	return []incarnationShape{
		{
			name: "committed binding",
			arrange: func(_ *testing.T, _ *testController, p incarnationPrincipal, unbound identity.IncarnationID) ([]identity.IncarnationID, []identity.IncarnationID) {
				return []identity.IncarnationID{p.bound}, []identity.IncarnationID{unbound}
			},
		},
		{
			name: "pending intent only",
			arrange: func(t *testing.T, tc *testController, p incarnationPrincipal, unbound identity.IncarnationID) ([]identity.IncarnationID, []identity.IncarnationID) {
				delete(tc.Store.Bindings, p.session)
				seedPendingLaunchIntent(t, tc, p.fr.RunID, p.session, unbound)
				return []identity.IncarnationID{unbound}, []identity.IncarnationID{p.bound}
			},
		},
		{
			name: "binding and pending intent disagree",
			arrange: func(t *testing.T, tc *testController, p incarnationPrincipal, unbound identity.IncarnationID) ([]identity.IncarnationID, []identity.IncarnationID) {
				seedPendingLaunchIntent(t, tc, p.fr.RunID, p.session, unbound)
				return nil, []identity.IncarnationID{p.bound, unbound}
			},
		},
		{
			name: "superseded binding",
			arrange: func(t *testing.T, tc *testController, p incarnationPrincipal, unbound identity.IncarnationID) ([]identity.IncarnationID, []identity.IncarnationID) {
				history := tc.Store.Bindings[p.session]
				superseded, err := history[len(history)-1].Supersede("observed replacement occupant", tc.Clock.Now())
				if err != nil {
					t.Fatalf("Supersede() error = %v", err)
				}
				history[len(history)-1] = superseded
				seedPendingLaunchIntent(t, tc, p.fr.RunID, p.session, unbound)
				return nil, []identity.IncarnationID{p.bound, unbound}
			},
		},
	}
}

// managerIncarnationPrincipal seeds a running feature run with its bound
// manager.
func managerIncarnationPrincipal(t *testing.T, tc *testController) incarnationPrincipal {
	t.Helper()
	fr := seedFeatureRun(t, tc, 2)
	return incarnationPrincipal{fr: fr, session: fr.ManagerID, bound: fr.ManagerIncarnation}
}

// launchingChildIncarnationPrincipal assigns one implement task: a bound
// child session whose attempt is launching with no claim, so a current
// caller's submission is transient.
func launchingChildIncarnationPrincipal(t *testing.T, tc *testController) incarnationPrincipal {
	t.Helper()
	fr := seedFeatureRun(t, tc, 2)
	task := seedImplementTask(t, tc, fr.RunID, 1, "child", false, run.TaskReady)
	session, bound := seedWorkerSession(t, tc, fr, task)
	return incarnationPrincipal{fr: fr, session: session, bound: bound, task: task, attempt: tc.Store.Sessions[session].value.AttemptID}
}

// launchingReviewerIncarnationPrincipal seeds a review task whose attempt
// is launching with no claim and its bound reviewer session.
func launchingReviewerIncarnationPrincipal(t *testing.T, tc *testController) incarnationPrincipal {
	t.Helper()
	fr := seedFeatureRun(t, tc, 2)
	session, bound := seedForeignReviewer(t, tc, fr, 1)
	attemptID := tc.Store.Sessions[session].value.AttemptID
	attempt := tc.Store.Attempts[attemptID]
	attempt.value.State = run.AttemptLaunching
	delete(tc.Store.LaunchClaims, bound)
	return incarnationPrincipal{fr: fr, session: session, bound: bound, task: attempt.value.TaskID, attempt: attemptID}
}

// judgeIncarnationOutcome maps an outcome to currency: currentOK when the
// store judged the caller current, staleOK when it refused it stale, and a
// test failure for anything else.
func judgeIncarnationOutcome(t *testing.T, verb string, err error, currentOK, staleOK bool, outcome any) bool {
	t.Helper()
	switch {
	case err != nil:
		t.Fatalf("%s error = %v", verb, err)
	case currentOK:
		return true
	case staleOK:
		return false
	}
	t.Fatalf("%s outcome = %+v; want the current-caller outcome or a stale refusal", verb, outcome)
	return false
}

// incarnationVerb is one worker-authority write that validates its
// caller's incarnation: judge submits one request at caller and reports
// whether the fake judged the incarnation current.
type incarnationVerb struct {
	name  string
	setup func(t *testing.T, tc *testController) incarnationPrincipal
	judge func(t *testing.T, tc *testController, p incarnationPrincipal, caller identity.IncarnationID) bool
}

// incarnationVerbs are the real store's TestPrincipalIncarnationRule verbs.
func incarnationVerbs() []incarnationVerb {
	return []incarnationVerb{
		{
			name: "task create", setup: managerIncarnationPrincipal,
			judge: func(t *testing.T, tc *testController, p incarnationPrincipal, caller identity.IncarnationID) bool {
				taskID, err := identity.ParseTaskID(tc.IDs.NewID())
				if err != nil {
					t.Fatalf("parse task id: %v", err)
				}
				got, err := tc.Store.CreateTask(context.Background(), app.TaskCreate{
					ID: taskID, RunID: p.fr.RunID, Session: p.session, IncarnationID: caller,
					Title: "task", InstructionsPath: "/state/instructions.md", InstructionsDigest: "instructions-digest",
				})
				return judgeIncarnationOutcome(t, "CreateTask", err, got.Outcome == app.WorkflowAccepted,
					got.Outcome == app.WorkflowRefused && got.Reason == app.GrammarReasonStale, got)
			},
		},
		{
			name: "task retry", setup: managerIncarnationPrincipal,
			judge: func(t *testing.T, tc *testController, p incarnationPrincipal, caller identity.IncarnationID) bool {
				task := seedImplementTask(t, tc, p.fr.RunID, 50+len(tc.Store.Tasks), "rework", false, run.TaskNeedsRework)
				attemptID, err := identity.ParseAttemptID(tc.IDs.NewID())
				if err != nil {
					t.Fatalf("parse attempt id: %v", err)
				}
				attempt, err := run.NewAttempt(attemptID, task, 1, tc.Clock.Now())
				if err != nil {
					t.Fatalf("NewAttempt() error = %v", err)
				}
				attempt.State = run.AttemptFailed
				tc.Store.Attempts[attemptID] = &entityRow[run.Attempt]{value: attempt, revision: 1}
				got, err := tc.Store.RequestRetry(context.Background(), app.RetryRequest{TaskID: task, RunID: p.fr.RunID, Session: p.session, IncarnationID: caller, Reason: "retry"})
				return judgeIncarnationOutcome(t, "RequestRetry", err, got.Outcome == app.WorkflowAccepted,
					got.Outcome == app.WorkflowRefused && got.Reason == app.GrammarReasonStale, got)
			},
		},
		{
			name: "plan close", setup: managerIncarnationPrincipal,
			judge: func(t *testing.T, tc *testController, p incarnationPrincipal, caller identity.IncarnationID) bool {
				seedImplementTask(t, tc, p.fr.RunID, 90+len(tc.Store.Tasks), "planned", false, run.TaskReady)
				got, err := tc.Store.ClosePlan(context.Background(), app.PlanClose{RunID: p.fr.RunID, Session: p.session, IncarnationID: caller})
				return judgeIncarnationOutcome(t, "ClosePlan", err, got.Outcome == app.WorkflowAccepted,
					got.Outcome == app.WorkflowRefused && got.Reason == app.GrammarReasonStale, got)
			},
		},
		{
			name: "message send", setup: managerIncarnationPrincipal,
			judge: func(t *testing.T, tc *testController, p incarnationPrincipal, caller identity.IncarnationID) bool {
				messageID, err := identity.ParseMessageID(tc.IDs.NewID())
				if err != nil {
					t.Fatalf("parse message id: %v", err)
				}
				got, err := tc.Store.SendMessage(context.Background(), app.MessageSend{
					ID: messageID, RunID: p.fr.RunID, Sender: run.SessionPrincipal(p.session), SenderAddress: run.ManagerAddress(),
					IncarnationID: caller, Recipient: run.HumanAddress(), Kind: run.MessageQuestion,
					BodyPath: "/state/bodies/q.md", BodyDigest: "digest-" + messageID.String(), BodyBytes: 2,
				})
				return judgeIncarnationOutcome(t, "SendMessage", err, got.Kind == app.MessageAccepted,
					got.Kind == app.MessageRefused && got.Reason == app.GrammarReasonStale, got)
			},
		},
		{
			name: "message fetch", setup: managerIncarnationPrincipal,
			judge: func(t *testing.T, tc *testController, p incarnationPrincipal, caller identity.IncarnationID) bool {
				_, _, err := tc.Store.FetchNextMessage(context.Background(), app.MessageFetch{RunID: p.fr.RunID, SessionID: p.session, IncarnationID: caller, Address: run.ManagerAddress()})
				if err == nil {
					return true
				}
				if errors.Is(err, app.ErrMessagingUnauthorized) && strings.Contains(err.Error(), "is not current") {
					return false
				}
				t.Fatalf("FetchNextMessage() error = %v; want served or the not-current refusal", err)
				return false
			},
		},
		{
			name: "message ack", setup: managerIncarnationPrincipal,
			judge: func(t *testing.T, tc *testController, p incarnationPrincipal, caller identity.IncarnationID) bool {
				// A worker's info to the manager with a delivery recorded for
				// exactly (manager, caller): the ack reaches the currency check.
				task := seedImplementTask(t, tc, p.fr.RunID, 70+len(tc.Store.Tasks), "sender", false, run.TaskReady)
				worker, workerInc := seedWorkerSession(t, tc, p.fr, task)
				messageID, err := identity.ParseMessageID(tc.IDs.NewID())
				if err != nil {
					t.Fatalf("parse message id: %v", err)
				}
				send, err := tc.Store.SendMessage(context.Background(), app.MessageSend{
					ID: messageID, RunID: p.fr.RunID, Sender: run.SessionPrincipal(worker), SenderAddress: run.TaskAddress(task),
					IncarnationID: workerInc, Recipient: run.ManagerAddress(), Kind: run.MessageInfo,
					BodyPath: "/state/bodies/i.md", BodyDigest: "digest-" + messageID.String(), BodyBytes: 2,
				})
				if err != nil || send.Kind != app.MessageAccepted {
					t.Fatalf("seed info = %+v, %v", send, err)
				}
				tc.Store.MessageDeliveries[messageID] = append(tc.Store.MessageDeliveries[messageID], run.Delivery{
					MessageID: messageID, SessionID: p.session, IncarnationID: caller, At: tc.Clock.Now(),
				})
				got, err := tc.Store.AckMessage(context.Background(), app.MessageAck{RunID: p.fr.RunID, MessageID: messageID, SessionID: p.session, IncarnationID: caller})
				return judgeIncarnationOutcome(t, "AckMessage", err, got.Kind == app.AckAccepted,
					got.Kind == app.AckRefused && got.Reason == app.GrammarReasonStale, got)
			},
		},
		{
			name: "launch claim", setup: managerIncarnationPrincipal,
			judge: func(t *testing.T, tc *testController, p incarnationPrincipal, caller identity.IncarnationID) bool {
				err := tc.Store.ClaimLaunch(context.Background(), app.LaunchClaim{
					IncarnationID: caller, RunID: p.fr.RunID, SessionID: p.session,
					Executable: "/usr/local/bin/claude", ArgvDigest: "argv-digest", PID: 4242, State: app.LaunchClaimExecPending,
				})
				if err == nil {
					return true
				}
				if strings.Contains(err.Error(), "current identity") {
					return false
				}
				t.Fatalf("ClaimLaunch() error = %v; want nil or the currency refusal", err)
				return false
			},
		},
		{
			name: "review submit", setup: launchingReviewerIncarnationPrincipal,
			judge: func(t *testing.T, tc *testController, p incarnationPrincipal, caller identity.IncarnationID) bool {
				reviewID, err := identity.ParseReviewID(tc.IDs.NewID())
				if err != nil {
					t.Fatalf("parse review id: %v", err)
				}
				got, err := tc.Store.SubmitReview(context.Background(), app.ReviewSubmission{
					ID: reviewID, RunID: p.fr.RunID, TaskID: p.task, AttemptID: p.attempt,
					Session: p.session, IncarnationID: caller,
					SubjectCommitOID: "other-subject-commit", SubjectTreeOID: fakeSubjectTree,
					Verdict: run.VerdictApprove, ReasonsPath: "/state/reasons.md", ReasonsDigest: "reasons-" + reviewID.String(),
				})
				return judgeIncarnationOutcome(t, "SubmitReview", err, got.Kind == app.ReviewTransient, got.Kind == app.ReviewStale, got)
			},
		},
		{
			name: "result submit", setup: launchingChildIncarnationPrincipal,
			judge: func(t *testing.T, tc *testController, p incarnationPrincipal, caller identity.IncarnationID) bool {
				resultID, err := identity.ParseResultID(tc.IDs.NewID())
				if err != nil {
					t.Fatalf("parse result id: %v", err)
				}
				got, err := tc.Store.SubmitResult(context.Background(), app.ResultSubmission{
					ID: resultID, RunID: p.fr.RunID, TaskID: p.task, AttemptID: p.attempt, IncarnationID: caller,
					CommitOID: strings.Repeat("a", 40), Summary: "summary", Digest: "digest-" + resultID.String(),
				})
				return judgeIncarnationOutcome(t, "SubmitResult", err, got.Kind == app.SubmissionTransient, got.Kind == app.SubmissionStale, got)
			},
		},
	}
}

// TestFakeStorePrincipalIncarnationRule holds the fake to the real
// store's one principal-incarnation rule (internal/adapters/sqlite
// TestPrincipalIncarnationRule, the same verbs and shapes): a committed
// binding's incarnation is current; with no binding row, the session's
// pending launch intent's incarnation is current; a binding and a pending
// intent that disagree fail closed; a superseded binding stays stale.
func TestFakeStorePrincipalIncarnationRule(t *testing.T) {
	for _, verb := range incarnationVerbs() {
		for _, shape := range incarnationShapes() {
			t.Run(verb.name+"/"+shape.name, func(t *testing.T) {
				tc := newTestController(defaultPolicy())
				p := verb.setup(t, tc)
				unbound, err := identity.ParseIncarnationID(tc.IDs.NewID())
				if err != nil {
					t.Fatalf("parse incarnation id: %v", err)
				}
				current, stale := shape.arrange(t, tc, p, unbound)
				for _, caller := range current {
					if !verb.judge(t, tc, p, caller) {
						t.Errorf("caller incarnation %s judged stale; want current", caller)
					}
				}
				for _, caller := range stale {
					if verb.judge(t, tc, p, caller) {
						t.Errorf("caller incarnation %s judged current; want stale", caller)
					}
				}
			})
		}
	}
}
