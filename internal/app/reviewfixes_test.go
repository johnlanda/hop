package app_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// Tracked ports of the pass-1 security review's regression cases
// (.bin/review/REPORT.md). Each subtest pins the unsafe outcome the
// review reproduced, against the fixed behavior.

// TestReviewFixReadPortsStayOutsideTransactions pins finding 6: the
// settlement path assembles its obligation snapshot and equality check
// through the transaction's own repositories, and the fakes enforce the
// no-port-calls-in-transaction law on the lease-free reads — so an
// ordinary conflict settlement completes under that enforcement.
func TestReviewFixReadPortsStayOutsideTransactions(t *testing.T) {
	f := newIntegrationFixture(t, false)
	f.git.MergeOutcome = "conflict"
	f.drive(t) // claim
	f.drive(t) // conflict settlement: snapshot, equality check, notice — all in-transaction reads
	if got := f.currentIntegrationRow(t).State; got != run.IntegrationConflicted {
		t.Fatalf("integration state = %s, want conflicted", got)
	}

	// The enforcement itself: a lease-free read inside an open unit of
	// work is refused by the fake, exactly like an external call.
	uow, err := f.tc.Store.Begin(context.Background(), app.Lease{Run: f.fr.RunID, ControllerID: "controller-1", Generation: 1})
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	defer func() { _ = uow.Rollback() }() //nolint:errcheck // cleanup of a deliberately unused transaction.
	if _, loadErr := f.tc.Store.LoadFrozenRun(context.Background(), f.fr.RunID); loadErr == nil {
		t.Fatalf("LoadFrozenRun inside an open unit of work was not refused")
	}
	read, err := app.RequireWorkflowReadStore(f.tc.Store, "test")
	if err != nil {
		t.Fatalf("RequireWorkflowReadStore() error = %v", err)
	}
	if _, err := read.LoadMessageDetail(context.Background(), f.fr.RunID, identity.MessageID("00000000-0000-4000-8000-000000000001")); err == nil || !strings.Contains(err.Error(), "unit of work is open") {
		t.Fatalf("LoadMessageDetail inside an open unit of work was not refused: %v", err)
	}
}

// TestReviewFixFenceRaceLosingCAS pins finding 1: a fence whose CAS
// loses to a zombie publish landing on the integration ref journals
// reconciling and reports OUTSTANDING work — the stop round never
// commits stopped over the moved ref. The follow-through rounds resolve
// the race through the fence's own recovery row: the observed head IS
// the retired publish's candidate, so the fence settles failed, the
// publish outcome is adopted (the integration enters checking on its
// published candidate), and the stop reset rolls the candidate back
// before the run stops.
func TestReviewFixFenceRaceLosingCAS(t *testing.T) {
	f := newIntegrationFixture(t, false)
	id := f.seedIntegrationRow(t, run.IntegrationMerging, "")
	merged := f.git.newCommit("tree-merged", f.base, f.src)
	f.seedOperation(t, app.OpIntegrationPublish, app.OperationPending, map[string]any{
		"integration_id":   id.String(),
		"ref":              integrationRefName,
		"new_oid":          merged,
		"expected_old_oid": f.base,
	}, nil)
	row := f.tc.Store.Runs[f.fr.RunID]
	row.value = row.value.RequestStop(f.tc.Clock.Now())
	row.revision++
	f.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound }

	// The zombie publisher lands its ref move exactly while the fence's
	// own update-ref runs: the fence CAS loses against a head it did not
	// expect.
	raced := false
	f.tc.Commands.RunHook = func(ctx context.Context, cmd app.Command) (app.CommandResult, bool, error) {
		if !raced && slices.Contains(cmd.Argv, "update-ref") {
			raced = true
			f.git.setRef(integrationRefName, merged)
		}
		return f.git.Hook(ctx, cmd)
	}

	report, err := f.tc.Controller.DriveFeatureStop(context.Background(), f.fr.Handle)
	if err != nil {
		t.Fatalf("DriveFeatureStop() round 1 error = %v", err)
	}
	if report.Terminated {
		t.Fatalf("unsafe: stopped with an unresolved fence and a published unvalidated candidate; report=%+v", report)
	}
	if len(report.Outstanding) == 0 {
		t.Fatalf("the losing fence CAS reported no outstanding work; report=%+v", report)
	}

	final := report
	for i := 0; i < 6 && !final.Terminated; i++ {
		final, err = f.tc.Controller.DriveFeatureStop(context.Background(), f.fr.Handle)
		if err != nil {
			t.Fatalf("DriveFeatureStop() round %d error = %v", i+2, err)
		}
	}
	if !final.Terminated {
		t.Fatalf("stop never terminated after the fence race resolved; report=%+v", final)
	}
	fences := f.opsOfKind(app.OpIntegrationFence)
	if len(fences) != 1 || fences[0].State != app.OperationFailed {
		t.Fatalf("fence operations = %+v, want exactly one, settled failed by its recovery row", fences)
	}
	if got := f.currentIntegrationRow(t).State; got != run.IntegrationRolledBack {
		t.Fatalf("integration state = %s, want the adopted candidate rolled back under stop", got)
	}
	if head := f.git.ref(integrationRefName); head == merged {
		t.Fatalf("the ref rests on the unvalidated candidate in a stopped run")
	}
	if got := f.tc.Store.Runs[f.fr.RunID].value.State; got != run.RunStopped {
		t.Fatalf("run state = %s, want stopped only after quiescence", got)
	}
}

// TestReviewFixTerminalFailureRetiresCandidate pins finding 3: a
// feature run's terminal failure mirrors stop — the current
// integration's published-but-unsettled candidate is rolled back
// through the shared shutdown procedure BEFORE the run marks failed,
// and the terminal transaction re-validates quiescence, so the
// integration ref never rests on an unvalidated candidate in a failed
// run.
func TestReviewFixTerminalFailureRetiresCandidate(t *testing.T) {
	f := newIntegrationFixture(t, false)
	f.driveUntil(t, string(run.IntegrationChecking), 5)
	merged := f.git.ref(integrationRefName)
	seedImplementTask(t, f.tc, f.fr.RunID, 2, "exhausted independent task", false, run.TaskFailed)
	f.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound }

	var report app.RetirementReport
	var err error
	for i := 0; i < 6 && !report.RunFailed; i++ {
		report, err = f.tc.Controller.RetireSettledSessions(context.Background(), f.fr.Handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() round %d error = %v", i+1, err)
		}
	}
	if !report.RunFailed {
		t.Fatalf("terminal failure never completed; report=%+v", report)
	}
	if got := f.currentIntegrationRow(t).State; got != run.IntegrationRolledBack {
		t.Fatalf("integration state = %s, want the published candidate rolled back before the failed commit", got)
	}
	if head := f.git.ref(integrationRefName); head == merged {
		t.Fatalf("unsafe: terminal failure left the unvalidated candidate published")
	}
	if got := f.tc.Store.Runs[f.fr.RunID].value.State; got != run.RunFailed {
		t.Fatalf("run state = %s, want failed", got)
	}
}

// TestReviewFixHistoricalManagerNeverShadows pins finding 4: the
// current manager resolves through the ManagerSession port, so a
// historical lost manager row (a cold relaunch's retired predecessor)
// can never shadow the live manager in map order and let the run fail
// while the real manager is still active. 64 iterations force the
// fake's map iteration through both orders.
func TestReviewFixHistoricalManagerNeverShadows(t *testing.T) {
	for i := 0; i < 64; i++ {
		f := newIntegrationFixture(t, false)
		seedImplementTask(t, f.tc, f.fr.RunID, 2, "exhausted", false, run.TaskFailed)
		old := f.tc.Store.Sessions[f.fr.ManagerID].value
		old.ID = identity.SessionID(f.tc.IDs.NewID())
		old.State = run.SessionLost
		f.tc.Store.Sessions[old.ID] = &entityRow[run.Session]{value: old, revision: 1}

		// The live manager's pane observation is AMBIGUOUS: it must not
		// be retired, and the run must not fail past it.
		f.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{}, errors.New("live manager is not absent")
		}
		report, err := f.tc.Controller.RetireSettledSessions(context.Background(), f.fr.Handle)
		if err != nil {
			t.Fatalf("iteration %d: RetireSettledSessions() error = %v", i, err)
		}
		if report.RunFailed {
			t.Fatalf("iteration %d: run failed while the live manager was unretired; report=%+v", i, report)
		}
		if got := f.tc.Store.Sessions[f.fr.ManagerID].value.State; got != run.SessionActive {
			t.Fatalf("iteration %d: live manager state = %s, want left active under ambiguity", i, got)
		}

		// Absence observed: the live manager retires and the run fails —
		// the historical lost row never blocks the terminal state either.
		f.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound }
		for j := 0; j < 6 && !report.RunFailed; j++ {
			report, err = f.tc.Controller.RetireSettledSessions(context.Background(), f.fr.Handle)
			if err != nil {
				t.Fatalf("iteration %d: RetireSettledSessions() round %d error = %v", i, j+2, err)
			}
		}
		if !report.RunFailed {
			t.Fatalf("iteration %d: terminal failure never completed after absence; report=%+v", i, report)
		}
	}
}

// TestReviewFixGuardHeadIsObservedNotInferred pins finding 5: the guard
// head is OBSERVED from the live integration ref and validated against
// integrated rows — never inferred from the recorded chain, which
// repeated no-op merges at one head break (each no-op row records the
// unchanged head, so chain consumption sees the rows consume each
// other and resolves no head at all).
func TestReviewFixGuardHeadIsObservedNotInferred(t *testing.T) {
	t.Run("consecutive no-ops at one head still resolve it; the review task is created", func(t *testing.T) {
		f := newIntegrationFixture(t, true)
		f.git.MergeOutcome = "no-op"
		f.driveUntil(t, string(run.IntegrationIntegrated), 5)

		// A second completed task whose result is ALSO already contained:
		// the second integration is another no-op at the same head.
		priorAttempt := f.tc.Store.Attempts[f.attemptID].value
		priorResult := f.tc.Store.Results[f.attemptID]
		f.taskID = seedImplementTask(t, f.tc, f.fr.RunID, 2, "second no-op", false, run.TaskCompleted)
		f.attemptID = identity.AttemptID(f.tc.IDs.NewID())
		f.resultID = identity.ResultID(f.tc.IDs.NewID())
		priorAttempt.ID = f.attemptID
		priorAttempt.TaskID = f.taskID
		priorResult.ID = f.resultID
		priorResult.AttemptID = f.attemptID
		f.tc.Store.Attempts[f.attemptID] = &entityRow[run.Attempt]{value: priorAttempt, revision: 1}
		f.tc.Store.Results[f.attemptID] = priorResult
		f.driveUntil(t, string(run.IntegrationIntegrated), 5)

		closePlanDirectly(t, f.tc, f.fr.RunID)
		created, err := f.tc.Controller.EnsureReviewTask(context.Background(), f.fr.Handle)
		if err != nil {
			t.Fatalf("EnsureReviewTask() error = %v", err)
		}
		if !created {
			t.Fatalf("review task missing after two integrated no-op rows at the same head")
		}
	})

	t.Run("a rolled-back head is vouched for by no row: readiness fails closed", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		f.driveUntil(t, string(run.IntegrationChecking), 5)
		merged := f.git.ref(integrationRefName)
		f.git.CheckExitCode = 1
		f.driveUntil(t, string(run.IntegrationRolledBack), 5)

		head := f.git.ref(integrationRefName)
		if head == merged {
			t.Fatalf("the rejected candidate stayed published; the rollback never landed")
		}
		ready, missing, err := f.tc.Controller.EvaluateRunReadiness(context.Background(), f.fr.Handle)
		if err != nil {
			t.Fatalf("EvaluateRunReadiness() error = %v", err)
		}
		if ready {
			t.Fatalf("unsafe: readiness on a fresh rollback commit no integrated row vouches for")
		}
		if len(missing) == 0 {
			t.Fatalf("readiness reported no shortfalls on an unvouched head")
		}
	})
}

// TestReviewFixFenceIdentityIsStable pins the round-2 residual (P2): a
// transiently failed fence must be recovered through its OWN journal
// row — never duplicated by the publish it retires. Before the fix,
// the next round visited the pending publish first and unconditionally
// allocated a second fence; the second fence moved the head, and the
// first stayed reconciling forever at a ref equal to neither its
// recorded head, its persisted OID, nor the publish's candidate — so
// the quiescence guard refused a terminal commit indefinitely. The fix
// is idempotence per retired operation ID, dependency-first recovery
// order, and per-turn re-reads; the quiescence guard itself is kept.
// Three faults — a one-time commit-tree failure, a one-time CAS
// failure with the head unchanged, and a crash right after the fence
// intent was journaled — each under both stop and terminal failure:
// exactly one fence identity ever exists, it settles succeeded, and
// the run terminates once the fault clears.
func TestReviewFixFenceIdentityIsStable(t *testing.T) {
	type fault struct {
		name string
		arm  func(t *testing.T, f *integrationFixture, publishID identity.OperationID)
	}
	faults := []fault{
		{name: "one-time commit-tree failure", arm: func(_ *testing.T, f *integrationFixture, _ identity.OperationID) {
			failed := false
			f.tc.Commands.RunHook = func(ctx context.Context, cmd app.Command) (app.CommandResult, bool, error) {
				if !failed && slices.Contains(cmd.Argv, "commit-tree") {
					failed = true
					return app.CommandResult{ExitCode: 128, Stderr: []byte("transient object write failure")}, true, nil
				}
				return f.git.Hook(ctx, cmd)
			}
		}},
		{name: "one-time CAS failure, head unchanged", arm: func(_ *testing.T, f *integrationFixture, _ identity.OperationID) {
			failed := false
			f.tc.Commands.RunHook = func(ctx context.Context, cmd app.Command) (app.CommandResult, bool, error) {
				if !failed && slices.Contains(cmd.Argv, "update-ref") {
					failed = true // the ref is NOT moved: the store refused transiently.
					return app.CommandResult{ExitCode: 128, Stderr: []byte("transient ref lock failure")}, true, nil
				}
				return f.git.Hook(ctx, cmd)
			}
		}},
		{name: "crash after fence intent creation", arm: func(t *testing.T, f *integrationFixture, publishID identity.OperationID) {
			// The fence row exists journaled pending with NO act evidence
			// — the crash column between record-intent and act.
			f.seedOperation(t, app.OpIntegrationFence, app.OperationPending, map[string]any{
				"ref":                    integrationRefName,
				"observed_head_oid":      f.base,
				"retired_operation_id":   publishID.String(),
				"retired_operation_kind": string(app.OpIntegrationPublish),
			}, nil)
		}},
	}

	seed := func(t *testing.T) (*integrationFixture, identity.OperationID) {
		f := newIntegrationFixture(t, false)
		id := f.seedIntegrationRow(t, run.IntegrationMerging, "")
		merged := f.git.newCommit("tree-merged", f.base, f.src)
		publishID := f.seedOperation(t, app.OpIntegrationPublish, app.OperationPending, map[string]any{
			"integration_id":   id.String(),
			"ref":              integrationRefName,
			"new_oid":          merged,
			"expected_old_oid": f.base,
		}, nil)
		f.tc.Clock.Advance(time.Millisecond)
		f.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound }
		return f, publishID
	}

	assertOneSettledFence := func(t *testing.T, f *integrationFixture) {
		t.Helper()
		fences := f.opsOfKind(app.OpIntegrationFence)
		if len(fences) != 1 {
			t.Fatalf("fence operations = %d, want exactly one journal identity per retired intent", len(fences))
		}
		if fences[0].State != app.OperationSucceeded {
			t.Fatalf("fence state = %s, want succeeded through its own row", fences[0].State)
		}
	}

	for _, fc := range faults {
		t.Run("stop: "+fc.name, func(t *testing.T) {
			f, publishID := seed(t)
			row := f.tc.Store.Runs[f.fr.RunID]
			row.value = row.value.RequestStop(f.tc.Clock.Now())
			row.revision++
			fc.arm(t, f, publishID)

			var report app.StopReport
			for i := 0; i < 6 && !report.Terminated; i++ {
				var err error
				report, err = f.tc.Controller.DriveFeatureStop(context.Background(), f.fr.Handle)
				if err != nil {
					t.Fatalf("DriveFeatureStop() round %d error = %v", i+1, err)
				}
				if fences := f.opsOfKind(app.OpIntegrationFence); len(fences) > 1 {
					t.Fatalf("round %d allocated a duplicate fence: %d rows", i+1, len(fences))
				}
				f.tc.Clock.Advance(time.Millisecond)
			}
			if !report.Terminated {
				t.Fatalf("stop never recovered after the fault cleared; report=%+v", report)
			}
			assertOneSettledFence(t, f)
		})
		t.Run("terminal failure: "+fc.name, func(t *testing.T) {
			f, publishID := seed(t)
			seedImplementTask(t, f.tc, f.fr.RunID, 2, "exhausted independent task", false, run.TaskFailed)
			fc.arm(t, f, publishID)

			var report app.RetirementReport
			for i := 0; i < 6 && !report.RunFailed; i++ {
				var err error
				report, err = f.tc.Controller.RetireSettledSessions(context.Background(), f.fr.Handle)
				if err != nil {
					t.Fatalf("RetireSettledSessions() round %d error = %v", i+1, err)
				}
				if fences := f.opsOfKind(app.OpIntegrationFence); len(fences) > 1 {
					t.Fatalf("round %d allocated a duplicate fence: %d rows", i+1, len(fences))
				}
				f.tc.Clock.Advance(time.Millisecond)
			}
			if !report.RunFailed {
				t.Fatalf("terminal failure never recovered after the fault cleared; report=%+v", report)
			}
			assertOneSettledFence(t, f)
			if got := f.tc.Store.Runs[f.fr.RunID].value.State; got != run.RunFailed {
				t.Fatalf("run state = %s, want failed", got)
			}
		})
	}
}

// TestReviewFixRetentionFailure pins finding 2: a zero-exit check whose
// output retention failed is never adopted as passing evidence — the
// combined check settles the integration check-failed (the reset
// retires the candidate), and a feature result check settles the
// budgeted failure path, never completion.
func TestReviewFixRetentionFailure(t *testing.T) {
	t.Run("combined check: retention failure fails the check, the reset retires the candidate", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		f.driveUntil(t, string(run.IntegrationChecking), 5)
		merged := f.git.ref(integrationRefName)

		f.tc.Artifacts.WriteErr = errors.New("injected output retention failure")
		if _, err := driveIntegrationStep(context.Background(), f.tc.Controller, f.fr.Handle); err == nil {
			t.Fatalf("retention failure did not surface as an error")
		}
		f.tc.Artifacts.WriteErr = nil
		if got := f.currentIntegrationRow(t).State; got != run.IntegrationCheckFailed {
			t.Fatalf("integration state = %s, want check-failed on lost evidence", got)
		}

		report := f.drive(t)
		if report.State == string(run.IntegrationIntegrated) {
			t.Fatalf("unsafe: retention failure adopted as passing receipt")
		}
		if got := f.currentIntegrationRow(t).State; got != run.IntegrationRolledBack {
			t.Fatalf("integration state = %s, want rolled-back", got)
		}
		if head := f.git.ref(integrationRefName); head == merged {
			t.Fatalf("the unvalidated candidate stayed published after lost evidence")
		}
	})

	t.Run("a failed-state zero-exit operation is never a usable receipt", func(t *testing.T) {
		f := newIntegrationFixture(t, true)
		f.git.MergeOutcome = "no-op"
		// A retention-anomaly row for exactly the candidate head: failed
		// journal state, zero exit.
		opID := f.seedOperation(t, app.OpCheckRun, app.OperationFailed, map[string]any{
			"integration_id":     "prior-integration",
			"subject_commit_oid": f.base,
			"subject_tree_oid":   "tree-base",
			"check_argv":         []any{"/bin/hopcheck"},
		}, nil)
		op := f.tc.Store.Operations[opID]
		op.Outcome = map[string]any{"exit_code": float64(0), "detail": "evidence retention failed: injected"}
		f.tc.Store.Operations[opID] = op

		f.drive(t) // claim
		f.drive(t) // merge -> no-op -> checking
		spawnsBefore := f.git.CheckExecCalls
		f.driveUntil(t, string(run.IntegrationIntegrated), 3)
		if f.git.CheckExecCalls != spawnsBefore+1 {
			t.Fatalf("check spawns = %d, want a FRESH execution — the anomaly row must not be adopted", f.git.CheckExecCalls-spawnsBefore)
		}
	})

	t.Run("feature result check: a zero exit with lost evidence never completes; recovery settles the failure", func(t *testing.T) {
		f := newMailboxFixture(t)
		if outcome := f.submit(t); outcome.Kind != app.SubmissionAccepted {
			t.Fatalf("submission = %+v, want accepted", outcome)
		}
		f.tc.Store.repoByRoot["/repo"] = f.tc.Store.Runs[f.fr.RunID].value.RepositoryID
		// The simulated check-exec child writes its pre-exec claim, so a
		// later recovery round can resolve the execution through the claim
		// table.
		git := newFakeGitRepo("/usr/bin/git", integrationHopPath)
		git.mu.Lock()
		git.commits["c0mm17"] = fakeGitCommit{tree: "tree-c0mm17"} // the fixture's submitted result commit
		git.mu.Unlock()
		f.tc.Commands.RunHook = git.Hook
		git.OnCheckExec = func(opID string, _ []string) {
			id, err := identity.ParseOperationID(opID)
			if err != nil {
				return
			}
			f.tc.Store.CheckExecClaims[id] = app.CheckExecClaim{OperationID: id, PID: 9911, ClaimedAt: f.tc.Clock.Now()}
		}

		// With artifact storage down, the zero-exit execution cannot
		// retain evidence OR prepare its settlement notice: the round
		// errors, nothing completes, the operation stays unresolved.
		f.tc.Artifacts.WriteErr = errors.New("injected output retention failure")
		if _, err := f.tc.Controller.DriveFeatureChecks(context.Background(), f.fr.Handle, integrationHopPath, nil); err == nil {
			t.Fatalf("retention failure did not surface as an error")
		}
		if got := f.tc.Store.Tasks[f.TaskID].value.State; got == run.TaskCompleted {
			t.Fatalf("task completed on lost evidence")
		}

		// Storage restored: recovery resolves the execution through its
		// claim and the unknown-outcome rule — the defined failure path,
		// never a completion claimed from the exit code alone.
		f.tc.Artifacts.WriteErr = nil
		if _, err := f.tc.Controller.DriveFeatureChecks(context.Background(), f.fr.Handle, integrationHopPath, nil); err != nil {
			t.Fatalf("recovery round error = %v", err)
		}
		if got := f.tc.Store.Tasks[f.TaskID].value.State; got != run.TaskNeedsRework {
			t.Fatalf("task state = %s, want needs-rework via the unknown rule", got)
		}
		if got := f.tc.Store.Attempts[f.w.AttemptID].value.State; got != run.AttemptFailed {
			t.Fatalf("attempt state = %s, want failed", got)
		}
	})
}
