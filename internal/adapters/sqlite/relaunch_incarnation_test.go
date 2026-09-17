package sqlite_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// Every relaunch path mints a NEW session, so the principal-incarnation
// rule's binding/intent disagreement conjunct — which compares one
// session's own binding with that same session's pending intent — never
// sees the prior session's binding next to the successor's intent. The
// tests below replay each path's transactions on the real store in the
// order the application commits them (internal/app
// coldRelaunchFeatureSession then openRelaunchFeaturePane; coldRelaunch,
// after retireAndRelaunch for a restored occupant, then openPane) and
// prove the successor's claim and first verb are current both before and
// after the pane.open outcome commits, while the prior incarnation stays
// stale.

// relaunchIntent journals the successor's pending pane.open with the
// application's intent keys.
func relaunchIntent(t *testing.T, fx *fixture, opN int, session identity.SessionID, incarnation identity.IncarnationID) identity.OperationID {
	t.Helper()
	opID := identity.OperationID(uid(opN))
	fx.inUOW(t, func(uow app.UnitOfWork) {
		err := uow.Operations().Create(t.Context(), app.Operation{
			ID: opID, RunID: fx.spec.RunID, Generation: fx.lease.Generation,
			Kind: app.OpPaneOpen, State: app.OperationPending,
			Intent: map[string]any{
				"command": []string{"/usr/local/bin/hop", "launch"}, "cwd": "/worktrees/relaunch",
				"workspace_id": "workspace-r", "label": opID.String(),
				"incarnation_id": incarnation.String(), "session_id": session.String(),
				"server_instance": "peer-pid:7",
			},
			CreatedAt: fx.clock.Now(), UpdatedAt: fx.clock.Now(),
		})
		if err != nil {
			t.Fatalf("journal relaunch pane.open intent: %v", err)
		}
	})
	return opID
}

// relaunchOutcome commits the successor's pane.open outcome: its binding
// and the operation's success, in one transaction.
func relaunchOutcome(t *testing.T, fx *fixture, opID identity.OperationID, session identity.SessionID, incarnation identity.IncarnationID) {
	t.Helper()
	fx.inUOW(t, func(uow app.UnitOfWork) {
		op, err := uow.Operations().Get(t.Context(), opID)
		if err != nil {
			t.Fatalf("get relaunch operation: %v", err)
		}
		binding := run.NewRuntimeBinding(session, incarnation, "", "peer-pid:7", "workspace-r", "tab-r", "pane-"+opID.String(), opID.String(), run.LaunchResume, fx.clock.Now())
		if err := uow.Bindings().Create(t.Context(), binding); err != nil {
			t.Fatalf("create relaunch binding: %v", err)
		}
		op.State = app.OperationSucceeded
		op.ActEvidence = map[string]string{"pane_id": "pane-" + opID.String()}
		op.UpdatedAt = fx.clock.Now()
		if err := uow.Operations().Save(t.Context(), op); err != nil {
			t.Fatalf("record relaunch outcome: %v", err)
		}
	})
}

// supersedeCurrentBinding supersedes session's current binding.
func supersedeCurrentBinding(t *testing.T, uow app.UnitOfWork, session identity.SessionID, evidence string, fx *fixture) run.RuntimeBinding {
	t.Helper()
	binding, ok, err := uow.Bindings().Current(t.Context(), session)
	if err != nil || !ok {
		t.Fatalf("current binding of %s: %v (found %t)", session, err, ok)
	}
	superseded, err := binding.Supersede(evidence, fx.clock.Now())
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if err := uow.Bindings().Save(t.Context(), superseded); err != nil {
		t.Fatalf("save superseded binding: %v", err)
	}
	return binding
}

// featureColdRelaunch replays coldRelaunchFeatureSession's transaction:
// the prior session reconciling then lost, its current binding superseded,
// and the successor — a manager, or a child on the prior's attempt —
// created launching with the lineage's native reference.
func featureColdRelaunch(t *testing.T, f *featureFixture, prior, successor identity.SessionID) {
	t.Helper()
	now := f.clock.Now()
	manager := f.managerSessionValue(t)
	f.inUOW(t, func(uow app.UnitOfWork) {
		priorSession, rev, err := uow.Sessions().Get(t.Context(), prior)
		if err != nil {
			t.Fatalf("get prior session: %v", err)
		}
		if priorSession.State == run.SessionActive {
			reconciling, trErr := priorSession.Reconcile(now)
			if trErr != nil {
				t.Fatalf("reconcile prior: %v", trErr)
			}
			if rev, err = uow.Sessions().Save(t.Context(), reconciling, rev); err != nil {
				t.Fatalf("save reconciling prior: %v", err)
			}
			priorSession = reconciling
		}
		lost, err := priorSession.MarkLost(now)
		if err != nil {
			t.Fatalf("mark prior lost: %v", err)
		}
		if _, saveErr := uow.Sessions().Save(t.Context(), lost, rev); saveErr != nil {
			t.Fatalf("save lost prior: %v", saveErr)
		}
		supersedeCurrentBinding(t, uow, prior, "attested absent; cold relaunch", f.fixture)

		var next run.Session
		if priorSession.Role == run.RoleManager {
			next = run.NewManagerSession(successor, f.spec.RunID, priorSession.Harness, now)
		} else {
			child, childErr := run.NewChildSession(successor, f.spec.RunID, priorSession.AttemptID, priorSession.Role, manager, priorSession.Harness, now)
			if childErr != nil {
				t.Fatalf("new successor child: %v", childErr)
			}
			next = child
		}
		next, err = next.AssignNativeRef("native-ref-"+successor.String(), run.NativeRefAssigned, now)
		if err != nil {
			t.Fatalf("assign native ref: %v", err)
		}
		if _, createErr := uow.Sessions().Create(t.Context(), next); createErr != nil {
			t.Fatalf("create successor: %v", createErr)
		}
		launching, err := next.Launch(now)
		if err != nil {
			t.Fatalf("launch successor: %v", err)
		}
		if _, err := uow.Sessions().Save(t.Context(), launching, 1); err != nil {
			t.Fatalf("save launching successor: %v", err)
		}
	})
}

// requireFetchCurrent asserts a fetch by (session, incarnation) at
// address is served or empty, never refused.
func requireFetchCurrent(t *testing.T, f *featureFixture, session identity.SessionID, incarnation identity.IncarnationID, address run.Address, when string) {
	t.Helper()
	if _, _, err := f.store.FetchNextMessage(t.Context(), app.MessageFetch{RunID: f.spec.RunID, SessionID: session, IncarnationID: incarnation, Address: address}); err != nil {
		t.Fatalf("FetchNextMessage(successor %s) %s = %v; want current", incarnation, when, err)
	}
}

// requireFetchStale asserts a fetch by (session, incarnation) is refused
// as not current.
func requireFetchStale(t *testing.T, f *featureFixture, session identity.SessionID, incarnation identity.IncarnationID, address run.Address) {
	t.Helper()
	_, _, err := f.store.FetchNextMessage(t.Context(), app.MessageFetch{RunID: f.spec.RunID, SessionID: session, IncarnationID: incarnation, Address: address})
	if !errors.Is(err, app.ErrMessagingUnauthorized) || !strings.Contains(err.Error(), "is not current") {
		t.Fatalf("FetchNextMessage(prior %s) = %v; want the not-current refusal", incarnation, err)
	}
}

// TestFeatureColdRelaunchSuccessorIsCurrent replays the feature cold
// relaunch for an implementer, a reviewer and the manager.
func TestFeatureColdRelaunchSuccessorIsCurrent(t *testing.T) {
	for _, tc := range []struct {
		name string
		role run.Role
	}{
		{name: "implementer", role: run.RoleImplementer},
		{name: "reviewer", role: run.RoleReviewer},
		{name: "manager", role: run.RoleManager},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFeatureFixture(t)
			now := f.clock.Now()
			f.inUOW(t, func(uow app.UnitOfWork) {
				saveRun(t, uow, f.spec.RunID, func(v run.Run) (run.Run, error) { return v.EnterResuming(now) })
			})

			prior, priorInc, address := f.ManagerID, f.ManagerIncarnation, run.ManagerAddress()
			var task identity.TaskID
			if tc.role != run.RoleManager {
				task = identity.TaskID(uid(8201))
				f.inUOW(t, func(uow app.UnitOfWork) {
					v := run.NewImplementTask(task, f.spec.RunID, 2, "relaunched task", "instructions-digest", false, now)
					if tc.role == run.RoleReviewer {
						v = run.NewReviewTask(task, f.spec.RunID, 2, "commit-head", "tree-head", now)
					}
					v.State = run.TaskActive
					if _, err := workflowRepos(t, uow).TaskIndex().Create(t.Context(), v); err != nil {
						t.Fatalf("create task: %v", err)
					}
				})
				prior, priorInc = f.createWorkerSession(t, task, tc.role, 8202)
				address = run.TaskAddress(task)
			}
			successor := identity.SessionID(uid(8211))
			successorInc := identity.IncarnationID(uid(8212))

			featureColdRelaunch(t, f, prior, successor)
			opID := relaunchIntent(t, f.fixture, 8213, successor, successorInc)

			// The launcher claims before the outcome commits.
			if err := f.store.ClaimLaunch(t.Context(), claimFor(f, successorInc, successor, "", 8214)); err != nil {
				t.Fatalf("ClaimLaunch(successor) before the outcome = %v", err)
			}
			requireFetchCurrent(t, f, successor, successorInc, address, "before the outcome")
			if tc.role == run.RoleManager {
				req := taskCreate(f, 8215, "planned after relaunch", "relaunch-1")
				req.Session, req.IncarnationID = successor, successorInc
				if outcome, err := f.store.CreateTask(t.Context(), req); err != nil || outcome.Outcome != app.WorkflowTransient {
					t.Fatalf("CreateTask(successor manager, resuming run) = %+v, %v; want transient", outcome, err)
				}
			}
			requireFetchStale(t, f, prior, priorInc, address)

			relaunchOutcome(t, f.fixture, opID, successor, successorInc)
			if err := f.store.ClaimLaunch(t.Context(), claimFor(f, successorInc, successor, "", 8214)); err != nil {
				t.Fatalf("ClaimLaunch(successor) retry after the outcome = %v", err)
			}
			requireFetchCurrent(t, f, successor, successorInc, address, "after the outcome")
			requireFetchStale(t, f, prior, priorInc, address)
		})
	}
}

// TestSoloColdRelaunchSuccessorIsCurrent replays the solo cold relaunch —
// after an attestation, and after retireAndRelaunch's restored-observed
// binding — and proves the successor's attempt-keyed claim and first
// result submission are current.
func TestSoloColdRelaunchSuccessorIsCurrent(t *testing.T) {
	for _, restored := range []bool{false, true} {
		name := "attested absence"
		if restored {
			name = "restored occupant retired"
		}
		t.Run(name, func(t *testing.T) {
			f := runningFixture(t)
			now := f.clock.Now()
			// Resume's entry: run resuming, attempt and session reconciling.
			f.inUOW(t, func(uow app.UnitOfWork) {
				saveRun(t, uow, f.spec.RunID, func(v run.Run) (run.Run, error) { return v.EnterResuming(now) })
				saveAttempt(t, uow, f.spec.AttemptID, func(v run.Attempt) (run.Attempt, error) { return v.Reconcile(now) })
				saveSession(t, uow, f.spec.SessionID, func(v run.Session) (run.Session, error) { return v.Reconcile(now) })
			})
			if restored {
				// retireAndRelaunch: the launch binding superseded and the
				// restored occupant recorded on the OLD session.
				f.inUOW(t, func(uow app.UnitOfWork) {
					launch := supersedeCurrentBinding(t, uow, f.spec.SessionID, "observed process argv carries native session reference", f)
					observed := run.NewRuntimeBinding(f.spec.SessionID, identity.IncarnationID(uid(8301)), launch.ServerSocketPath, "peer-pid:8", launch.WorkspaceID, launch.TabID, launch.PaneID, launch.CreationLabel, run.LaunchRestoredObserved, now)
					if err := uow.Bindings().Create(t.Context(), observed); err != nil {
						t.Fatalf("create restored-observed binding: %v", err)
					}
				})
			}
			successor := identity.SessionID(uid(8311))
			successorInc := identity.IncarnationID(uid(8312))
			// coldRelaunch's transaction.
			f.inUOW(t, func(uow app.UnitOfWork) {
				saveAttempt(t, uow, f.spec.AttemptID, func(v run.Attempt) (run.Attempt, error) { return v.Relaunch(now) })
				saveRun(t, uow, f.spec.RunID, func(v run.Run) (run.Run, error) { return v.Launch(now) })
				saveSession(t, uow, f.spec.SessionID, func(v run.Session) (run.Session, error) { return v.MarkLost(now) })
				next := run.NewSession(successor, f.spec.RunID, f.spec.AttemptID, run.HarnessClaude, now)
				next, err := next.AssignNativeRef("native-ref-solo", run.NativeRefAssigned, now)
				if err != nil {
					t.Fatalf("assign native ref: %v", err)
				}
				if next, err = next.Launch(now); err != nil {
					t.Fatalf("launch successor: %v", err)
				}
				if _, err := uow.Sessions().Create(t.Context(), next); err != nil {
					t.Fatalf("create successor: %v", err)
				}
			})
			opID := relaunchIntent(t, f, 8313, successor, successorInc)

			claim := app.LaunchClaim{
				IncarnationID: successorInc, RunID: f.spec.RunID, AttemptID: f.spec.AttemptID,
				Executable: "/opt/harness/claude", ArgvDigest: "argv-digest", PID: 8314,
			}
			if err := f.store.ClaimLaunch(t.Context(), claim); err != nil {
				t.Fatalf("ClaimLaunch(successor, by attempt) before the outcome = %v", err)
			}
			submit := func(n int, incarnation identity.IncarnationID) app.SubmissionOutcome {
				t.Helper()
				sub := f.submission(n, "digest-"+uid(n))
				sub.IncarnationID = incarnation
				outcome, err := f.store.SubmitResult(t.Context(), sub)
				if err != nil {
					t.Fatalf("SubmitResult: %v", err)
				}
				return outcome
			}
			if outcome := submit(8321, successorInc); outcome.Kind != app.SubmissionTransient {
				t.Fatalf("SubmitResult(successor) before the outcome = %+v; want transient (relaunching, unsettled)", outcome)
			}
			if outcome := submit(8322, f.spec.IncarnationID); outcome.Kind != app.SubmissionStale {
				t.Fatalf("SubmitResult(prior incarnation) = %+v; want stale", outcome)
			}

			relaunchOutcome(t, f, opID, successor, successorInc)
			if err := f.store.ClaimLaunch(t.Context(), claim); err != nil {
				t.Fatalf("ClaimLaunch(successor) retry after the outcome = %v", err)
			}
			if outcome := submit(8323, successorInc); outcome.Kind != app.SubmissionTransient {
				t.Fatalf("SubmitResult(successor) after the outcome = %+v; want transient", outcome)
			}
			if outcome := submit(8324, f.spec.IncarnationID); outcome.Kind != app.SubmissionStale || !strings.Contains(outcome.Detail, "not current") {
				t.Fatalf("SubmitResult(prior incarnation) after the outcome = %+v; want stale", outcome)
			}
		})
	}
}
