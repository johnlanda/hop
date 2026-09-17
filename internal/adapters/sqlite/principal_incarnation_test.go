package sqlite_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// principal is one session whose incarnation currency a verb decides,
// with the incarnation its committed binding carries.
type principal struct {
	session identity.SessionID
	bound   identity.IncarnationID
	task    identity.TaskID
	attempt identity.AttemptID
}

// principalShape puts one principal into an incarnation state of the one
// principal-incarnation rule and returns the caller incarnations the rule
// must accept and refuse. unbound is a fresh incarnation no binding
// carries.
type principalShape struct {
	name    string
	arrange func(t *testing.T, f *featureFixture, p principal, unbound identity.IncarnationID, opN int) (current, stale []identity.IncarnationID)
}

// principalShapes are the four states the rule distinguishes.
func principalShapes() []principalShape {
	return []principalShape{
		{
			name: "committed binding",
			arrange: func(_ *testing.T, _ *featureFixture, p principal, unbound identity.IncarnationID, _ int) ([]identity.IncarnationID, []identity.IncarnationID) {
				return []identity.IncarnationID{p.bound}, []identity.IncarnationID{unbound}
			},
		},
		{
			// The controller died between the pane.open act and its recorded
			// outcome: no binding row, the session's pending intent names the
			// live principal's incarnation.
			name: "pending intent only",
			arrange: func(t *testing.T, f *featureFixture, p principal, unbound identity.IncarnationID, opN int) ([]identity.IncarnationID, []identity.IncarnationID) {
				rawExec(t, f.store, `DELETE FROM runtime_bindings WHERE session_id = ?`, p.session.String())
				createLaunchIntentFor(t, f, opN, p.session, unbound)
				return []identity.IncarnationID{unbound}, []identity.IncarnationID{p.bound}
			},
		},
		{
			name: "binding and pending intent disagree",
			arrange: func(t *testing.T, f *featureFixture, p principal, unbound identity.IncarnationID, opN int) ([]identity.IncarnationID, []identity.IncarnationID) {
				createLaunchIntentFor(t, f, opN, p.session, unbound)
				return nil, []identity.IncarnationID{p.bound, unbound}
			},
		},
		{
			// A superseded row with no successor retires the incarnation, and
			// any binding row disables the pending-intent fallback.
			name: "superseded binding",
			arrange: func(t *testing.T, f *featureFixture, p principal, unbound identity.IncarnationID, opN int) ([]identity.IncarnationID, []identity.IncarnationID) {
				f.inUOW(t, func(uow app.UnitOfWork) {
					binding, ok, err := uow.Bindings().Current(t.Context(), p.session)
					if err != nil || !ok {
						t.Fatalf("current binding: %v (found %t)", err, ok)
					}
					superseded, err := binding.Supersede("observed replacement occupant", f.clock.Now())
					if err != nil {
						t.Fatalf("supersede: %v", err)
					}
					if err := uow.Bindings().Save(t.Context(), superseded); err != nil {
						t.Fatalf("save superseded binding: %v", err)
					}
				})
				createLaunchIntentFor(t, f, opN, p.session, unbound)
				return nil, []identity.IncarnationID{p.bound, unbound}
			},
		},
		{
			// Every pending intent of the session decides once a binding
			// exists, not only the newest: an older one naming another
			// incarnation fails closed although the newest agrees.
			name: "an older pending intent disagrees while the newest agrees",
			arrange: func(t *testing.T, f *featureFixture, p principal, unbound identity.IncarnationID, opN int) ([]identity.IncarnationID, []identity.IncarnationID) {
				createLaunchIntentFor(t, f, opN, p.session, unbound)
				createLaunchIntentFor(t, f, opN+2, p.session, p.bound)
				return nil, []identity.IncarnationID{p.bound, unbound}
			},
		},
		{
			name: "a pending intent of the session names no usable incarnation",
			arrange: func(t *testing.T, f *featureFixture, p principal, unbound identity.IncarnationID, opN int) ([]identity.IncarnationID, []identity.IncarnationID) {
				createLaunchIntentPayload(t, f, opN, map[string]any{"session_id": p.session.String(), "incarnation_id": 42})
				return nil, []identity.IncarnationID{p.bound, unbound}
			},
		},
	}
}

// createLaunchIntentPayload journals, through the fenced repository, a
// pending pane.open whose intent is exactly payload — a shape no
// well-formed controller intent has.
func createLaunchIntentPayload(t *testing.T, f *featureFixture, opN int, payload map[string]any) {
	t.Helper()
	f.inUOW(t, func(uow app.UnitOfWork) {
		err := uow.Operations().Create(t.Context(), app.Operation{
			ID: identity.OperationID(uid(opN)), RunID: f.spec.RunID, Generation: f.lease.Generation,
			Kind: app.OpPaneOpen, State: app.OperationPending, Intent: payload,
			CreatedAt: f.clock.Now(), UpdatedAt: f.clock.Now(),
		})
		if err != nil {
			t.Fatalf("create launch intent: %v", err)
		}
	})
}

// TestPrincipalIncarnationOlderConflictingIntent is the real-store
// reproduction of a bound manager whose newest pending launch intent names
// its own incarnation while an older pending one names another: the
// principal is stale, never accepted.
func TestPrincipalIncarnationOlderConflictingIntent(t *testing.T) {
	f := newFeatureFixture(t)
	createLaunchIntentFor(t, f, 99801, f.ManagerID, identity.IncarnationID(uid(99802)))
	createLaunchIntentFor(t, f, 99803, f.ManagerID, f.ManagerIncarnation)
	outcome, err := f.store.ClosePlan(t.Context(), app.PlanClose{RunID: f.spec.RunID, Session: f.ManagerID, IncarnationID: f.ManagerIncarnation})
	if err != nil {
		t.Fatalf("ClosePlan() error = %v", err)
	}
	if outcome.Outcome != app.WorkflowRefused || outcome.Reason != app.GrammarReasonStale {
		t.Fatalf("ClosePlan() = %+v, want a stale refusal: an older pending intent names another incarnation", outcome)
	}
}

// principalVerb is one worker-authority write that validates its caller's
// incarnation. judge submits one request at caller and reports whether
// the store judged the incarnation current, failing on any outcome that
// is neither the verb's current-caller outcome nor its stale refusal.
type principalVerb struct {
	name string
	// setup builds the fixture and the principal the verb's caller is.
	setup func(t *testing.T) (*featureFixture, principal)
	judge func(t *testing.T, f *featureFixture, p principal, caller identity.IncarnationID, n int) bool
}

// managerPrincipal is the feature fixture's bound, active manager.
func managerPrincipal(t *testing.T) (*featureFixture, principal) {
	t.Helper()
	f := newFeatureFixture(t)
	return f, principal{session: f.ManagerID, bound: f.ManagerIncarnation}
}

// launchingChildPrincipal is a bound child session of role on a fresh
// task whose attempt is launching with no claim: a current caller's
// submission is transient, never accepted and never stale.
func launchingChildPrincipal(t *testing.T, role run.Role) (*featureFixture, principal) {
	t.Helper()
	f := newFeatureFixture(t)
	now := f.clock.Now()
	task := identity.TaskID(uid(7871))
	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		v := run.NewImplementTask(task, f.spec.RunID, 2, "child task", "instructions-digest", false, now)
		if role == run.RoleReviewer {
			v = run.NewReviewTask(task, f.spec.RunID, 2, "commit-head", "tree-head", now)
		}
		v.State = run.TaskActive
		if _, err := wf.TaskIndex().Create(t.Context(), v); err != nil {
			t.Fatalf("create child task: %v", err)
		}
	})
	session, bound := f.createWorkerSession(t, task, role, 7872)
	attempt := identity.AttemptID(uid(7872))
	f.inUOW(t, func(uow app.UnitOfWork) {
		saveAttempt(t, uow, attempt, func(v run.Attempt) (run.Attempt, error) { return v.Launch(now) })
	})
	return f, principal{session: session, bound: bound, task: task, attempt: attempt}
}

// judgeWorkflow maps a plan verb's outcome to currency.
func judgeWorkflow(t *testing.T, verb string, outcome app.WorkflowOutcomeKind, reason string, err error, current app.WorkflowOutcomeKind) bool {
	t.Helper()
	switch {
	case err != nil:
		t.Fatalf("%s: %v", verb, err)
	case outcome == current:
		return true
	case outcome == app.WorkflowRefused && reason == app.GrammarReasonStale:
		return false
	}
	t.Fatalf("%s outcome = %s (reason %q); want %s or refused stale", verb, outcome, reason, current)
	return false
}

func principalVerbs() []principalVerb {
	manager := managerPrincipal
	return []principalVerb{
		{
			name: "task create", setup: manager,
			judge: func(t *testing.T, f *featureFixture, p principal, caller identity.IncarnationID, n int) bool {
				req := taskCreate(f, n, "task "+uid(n), "")
				req.Session, req.IncarnationID = p.session, caller
				outcome, err := f.store.CreateTask(t.Context(), req)
				return judgeWorkflow(t, "CreateTask", outcome.Outcome, outcome.Reason, err, app.WorkflowAccepted)
			},
		},
		{
			name: "task retry", setup: manager,
			judge: func(t *testing.T, f *featureFixture, p principal, caller identity.IncarnationID, n int) bool {
				task := f.createFeatureTask(t, n, 10+n%1000, run.TaskNeedsRework)
				seedTerminalAttempt(t, f, task, n+1, 1)
				outcome, err := f.store.RequestRetry(t.Context(), app.RetryRequest{TaskID: task, RunID: f.spec.RunID, Session: p.session, IncarnationID: caller, Reason: "retry"})
				return judgeWorkflow(t, "RequestRetry", outcome.Outcome, outcome.Reason, err, app.WorkflowAccepted)
			},
		},
		{
			name: "plan close", setup: manager,
			judge: func(t *testing.T, f *featureFixture, p principal, caller identity.IncarnationID, _ int) bool {
				outcome, err := f.store.ClosePlan(t.Context(), app.PlanClose{RunID: f.spec.RunID, Session: p.session, IncarnationID: caller})
				return judgeWorkflow(t, "ClosePlan", outcome.Outcome, outcome.Reason, err, app.WorkflowAccepted)
			},
		},
		{
			name: "message send", setup: manager,
			judge: func(t *testing.T, f *featureFixture, p principal, caller identity.IncarnationID, n int) bool {
				outcome, err := f.store.SendMessage(t.Context(), app.MessageSend{
					ID: identity.MessageID(uid(n)), RunID: f.spec.RunID,
					Sender: run.SessionPrincipal(p.session), SenderAddress: run.ManagerAddress(),
					IncarnationID: caller, Recipient: run.HumanAddress(), Kind: run.MessageQuestion,
					BodyPath: "/state/bodies/" + uid(n) + ".md", BodyDigest: "digest-" + uid(n), BodyBytes: 8,
				})
				switch {
				case err != nil:
					t.Fatalf("SendMessage: %v", err)
				case outcome.Kind == app.MessageAccepted:
					return true
				case outcome.Kind == app.MessageRefused && outcome.Reason == app.GrammarReasonStale:
					return false
				}
				t.Fatalf("SendMessage outcome = %+v; want accepted or refused stale", outcome)
				return false
			},
		},
		{
			name: "message fetch", setup: manager,
			judge: func(t *testing.T, f *featureFixture, p principal, caller identity.IncarnationID, _ int) bool {
				_, _, err := f.store.FetchNextMessage(t.Context(), app.MessageFetch{RunID: f.spec.RunID, SessionID: p.session, IncarnationID: caller, Address: run.ManagerAddress()})
				switch {
				case err == nil:
					return true
				case errors.Is(err, app.ErrMessagingUnauthorized) && strings.Contains(err.Error(), "is not current"):
					return false
				}
				t.Fatalf("FetchNextMessage = %v; want served or the not-current refusal", err)
				return false
			},
		},
		{
			name: "message ack", setup: manager,
			judge: func(t *testing.T, f *featureFixture, p principal, caller identity.IncarnationID, n int) bool {
				// A worker's info to the manager, with a delivery row recorded
				// for exactly (manager, caller): the ack reaches the currency
				// check whatever the caller's state.
				task := f.createFeatureTask(t, n, 10+n%1000, run.TaskActive)
				worker, workerInc := f.createWorkerSession(t, task, run.RoleImplementer, n+1)
				send := app.MessageSend{
					ID: identity.MessageID(uid(n + 5)), RunID: f.spec.RunID,
					Sender: run.SessionPrincipal(worker), SenderAddress: run.TaskAddress(task),
					IncarnationID: workerInc, Recipient: run.ManagerAddress(), Kind: run.MessageInfo,
					BodyPath: "/state/bodies/" + uid(n+5) + ".md", BodyDigest: "digest-" + uid(n+5), BodyBytes: 8,
				}
				if outcome, err := f.store.SendMessage(t.Context(), send); err != nil || outcome.Kind != app.MessageAccepted {
					t.Fatalf("seed info: %+v, %v", outcome, err)
				}
				rawExec(t, f.store, `INSERT INTO message_deliveries (id, message_id, session_id, incarnation_id, delivered_at) VALUES (?, ?, ?, ?, ?)`,
					uid(n+6), send.ID.String(), p.session.String(), caller.String(), "2026-09-14T09:00:00.000000000Z")
				outcome, err := f.store.AckMessage(t.Context(), app.MessageAck{RunID: f.spec.RunID, MessageID: send.ID, SessionID: p.session, IncarnationID: caller})
				switch {
				case err != nil:
					t.Fatalf("AckMessage: %v", err)
				case outcome.Kind == app.AckAccepted:
					return true
				case outcome.Kind == app.AckRefused && outcome.Reason == app.GrammarReasonStale:
					return false
				}
				t.Fatalf("AckMessage outcome = %+v; want accepted or refused stale", outcome)
				return false
			},
		},
		{
			name: "launch claim", setup: manager,
			judge: func(t *testing.T, f *featureFixture, p principal, caller identity.IncarnationID, _ int) bool {
				err := f.store.ClaimLaunch(t.Context(), claimFor(f, caller, p.session, "", 4242))
				switch {
				case err == nil:
					return true
				case strings.Contains(err.Error(), "current identity"):
					return false
				}
				t.Fatalf("ClaimLaunch = %v; want nil or the currency refusal", err)
				return false
			},
		},
		{
			name:  "review submit",
			setup: func(t *testing.T) (*featureFixture, principal) { return launchingChildPrincipal(t, run.RoleReviewer) },
			judge: func(t *testing.T, f *featureFixture, p principal, caller identity.IncarnationID, n int) bool {
				outcome, err := f.store.SubmitReview(t.Context(), app.ReviewSubmission{
					ID: identity.ReviewID(uid(n)), RunID: f.spec.RunID, TaskID: p.task, AttemptID: p.attempt,
					Session: p.session, IncarnationID: caller,
					SubjectCommitOID: "commit-head", SubjectTreeOID: "tree-head",
					Verdict: run.VerdictApprove, ReasonsPath: "/state/reasons/" + uid(n) + ".md", ReasonsDigest: "reasons-" + uid(n),
				})
				switch {
				case err != nil:
					t.Fatalf("SubmitReview: %v", err)
				case outcome.Kind == app.ReviewTransient:
					return true
				case outcome.Kind == app.ReviewStale:
					return false
				}
				t.Fatalf("SubmitReview outcome = %+v; want transient (launching, unsettled) or stale", outcome)
				return false
			},
		},
		{
			name: "result submit",
			setup: func(t *testing.T) (*featureFixture, principal) {
				return launchingChildPrincipal(t, run.RoleImplementer)
			},
			judge: func(t *testing.T, f *featureFixture, p principal, caller identity.IncarnationID, n int) bool {
				outcome, err := f.store.SubmitResult(t.Context(), app.ResultSubmission{
					ID: identity.ResultID(uid(n)), RunID: f.spec.RunID, TaskID: p.task, AttemptID: p.attempt,
					IncarnationID: caller, CommitOID: strings.Repeat("a", 40), Summary: "summary", Digest: "digest-" + uid(n),
				})
				return judgeSubmission(t, outcome, err)
			},
		},
	}
}

// judgeSubmission maps a result submission for a launching attempt with
// no settled claim to currency: transient when current, stale otherwise.
func judgeSubmission(t *testing.T, outcome app.SubmissionOutcome, err error) bool {
	t.Helper()
	switch {
	case err != nil:
		t.Fatalf("SubmitResult: %v", err)
	case outcome.Kind == app.SubmissionTransient:
		return true
	case outcome.Kind == app.SubmissionStale:
		return false
	}
	t.Fatalf("SubmitResult outcome = %+v; want transient (launching, unsettled) or stale", outcome)
	return false
}

// TestPrincipalIncarnationRule pins the one principal-incarnation rule
// (docs/plan/phase-3-design.md section 7) against the real store for every
// verb that validates its caller's incarnation: a committed binding's
// incarnation is current; with no binding row, the session's pending
// launch intent's incarnation is current, so a live principal whose
// pane.open outcome was never recorded is never refused stale; a binding
// and a pending intent that disagree fail closed; and a superseded binding
// stays stale.
func TestPrincipalIncarnationRule(t *testing.T) {
	for _, verb := range principalVerbs() {
		for _, shape := range principalShapes() {
			t.Run(verb.name+"/"+shape.name, func(t *testing.T) {
				f, p := verb.setup(t)
				unbound := identity.IncarnationID(uid(7899))
				current, stale := shape.arrange(t, f, p, unbound, 7890)
				n := 7900
				for _, caller := range current {
					n += 20
					if !verb.judge(t, f, p, caller, n) {
						t.Errorf("caller incarnation %s judged stale; want current", caller)
					}
				}
				for _, caller := range stale {
					n += 20
					if verb.judge(t, f, p, caller, n) {
						t.Errorf("caller incarnation %s judged current; want stale", caller)
					}
				}
			})
		}
	}
}

// TestPrincipalIncarnationPendingIntentKeepsRunStateRule proves the rule
// changes currency only: a manager current through its pending intent
// still meets the run-state acceptance rule, so a launching run records
// the retryable transient outcome, never a final refusal.
func TestPrincipalIncarnationPendingIntentKeepsRunStateRule(t *testing.T) {
	clock := newFakeClock()
	store := openStoreAt(t, t.TempDir(), clock)
	spec := newSpec("/repos/launching-manager", specStride, clock.Now())
	lease := initLegacyFeatureRun(t, store, &spec)
	f := &featureFixture{fixture: &fixture{store: store, clock: clock, spec: spec, lease: lease}, ManagerID: identity.SessionID(uid(7951))}
	now := clock.Now()
	f.inUOW(t, func(uow app.UnitOfWork) {
		saveRun(t, uow, spec.RunID, func(v run.Run) (run.Run, error) { return v.Launch(now) })
		manager := run.NewManagerSession(f.ManagerID, spec.RunID, run.HarnessClaude, now)
		manager, err := manager.Launch(now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := uow.Sessions().Create(t.Context(), manager); err != nil {
			t.Fatalf("create manager session: %v", err)
		}
	})
	incarnation := identity.IncarnationID(uid(7952))
	createLaunchIntentFor(t, f, 7953, f.ManagerID, incarnation)

	req := taskCreate(f, 7955, "early plan", "early-1")
	req.IncarnationID = incarnation
	outcome, err := store.CreateTask(t.Context(), req)
	if err != nil || outcome.Outcome != app.WorkflowTransient {
		t.Fatalf("CreateTask(pending-intent manager, launching run) = %+v, %v; want transient", outcome, err)
	}
	stale := req
	stale.IncarnationID = identity.IncarnationID(uid(7956))
	stale.RequestID = "early-2"
	if outcome, err := store.CreateTask(t.Context(), stale); err != nil || outcome.Outcome != app.WorkflowRefused || outcome.Reason != app.GrammarReasonStale {
		t.Fatalf("CreateTask(unrelated incarnation, launching run) = %+v, %v; want refused stale", outcome, err)
	}
}

// TestPrincipalIncarnationSoloResult pins the same rule on the Phase 2
// solo result submission: a solo worker submitting before its pane.open
// outcome is recorded is transient, the currency its own claim already
// had, and every other shape stays stale.
func TestPrincipalIncarnationSoloResult(t *testing.T) {
	unbound := identity.IncarnationID(uid(6071))
	supersede := func(t *testing.T, f *fixture) {
		t.Helper()
		f.inUOW(t, func(uow app.UnitOfWork) {
			binding, ok, err := uow.Bindings().Current(t.Context(), f.spec.SessionID)
			if err != nil || !ok {
				t.Fatalf("current binding: %v (found %t)", err, ok)
			}
			superseded, err := binding.Supersede("observed replacement occupant", f.clock.Now())
			if err != nil {
				t.Fatalf("supersede: %v", err)
			}
			if err := uow.Bindings().Save(t.Context(), superseded); err != nil {
				t.Fatalf("save superseded binding: %v", err)
			}
		})
	}
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, f *fixture)
		current []identity.IncarnationID
		stale   []identity.IncarnationID
	}{
		{
			name:    "committed binding",
			arrange: func(t *testing.T, f *fixture) { f.createBinding(t) },
			current: []identity.IncarnationID{"fixture"},
			stale:   []identity.IncarnationID{unbound},
		},
		{
			name:    "pending intent only",
			arrange: func(t *testing.T, f *fixture) { f.createLaunchIntent(t, f.spec.SessionID, f.spec.IncarnationID) },
			current: []identity.IncarnationID{"fixture"},
			stale:   []identity.IncarnationID{unbound},
		},
		{
			name: "binding and pending intent disagree",
			arrange: func(t *testing.T, f *fixture) {
				f.createBinding(t)
				f.createLaunchIntent(t, f.spec.SessionID, unbound)
			},
			stale: []identity.IncarnationID{"fixture", unbound},
		},
		{
			name: "superseded binding",
			arrange: func(t *testing.T, f *fixture) {
				f.createBinding(t)
				supersede(t, f)
				f.createLaunchIntent(t, f.spec.SessionID, f.spec.IncarnationID)
			},
			stale: []identity.IncarnationID{"fixture"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.launchAttempt(t)
			tc.arrange(t, f)
			resolve := func(inc identity.IncarnationID) identity.IncarnationID {
				if inc == "fixture" {
					return f.spec.IncarnationID
				}
				return inc
			}
			n := 8100
			for _, caller := range tc.current {
				n++
				sub := f.submission(n, "digest-"+uid(n))
				sub.IncarnationID = resolve(caller)
				if outcome, err := f.store.SubmitResult(t.Context(), sub); !judgeSubmission(t, outcome, err) {
					t.Errorf("solo result from %s judged stale; want transient", sub.IncarnationID)
				}
			}
			for _, caller := range tc.stale {
				n++
				sub := f.submission(n, "digest-"+uid(n))
				sub.IncarnationID = resolve(caller)
				if outcome, err := f.store.SubmitResult(t.Context(), sub); judgeSubmission(t, outcome, err) {
					t.Errorf("solo result from %s judged current; want stale", sub.IncarnationID)
				}
			}
		})
	}
}
