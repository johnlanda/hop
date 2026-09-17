package app_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// requestRunStop records a stop request on the run's row, as the
// worker-authority RequestStop does.
func requestRunStop(tc *testController, runID identity.RunID) {
	row := tc.Store.Runs[runID]
	row.value = row.value.RequestStop(tc.Clock.Now())
	row.revision++
}

// scriptAbsentPanes makes every pane inspection answer not found, so a
// stop round observes every session gone.
func scriptAbsentPanes(tc *testController) {
	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{}, app.ErrPaneNotFound
	}
}

// claimOnCheckExec makes every simulated check-exec child write its
// pre-exec claim under pid, whose group is then observed empty.
func claimOnCheckExec(f *integrationFixture, pid int) {
	f.git.OnCheckExec = func(opID string, _ []string) {
		id, err := identity.ParseOperationID(opID)
		if err != nil {
			return
		}
		f.tc.Store.CheckExecClaims[id] = app.CheckExecClaim{OperationID: id, PID: pid, ClaimedAt: f.tc.Clock.Now()}
	}
}

// gitSubcommand returns a git invocation's arguments from its subcommand
// on, past the leading -C <dir> and -c <key=value> options.
func gitSubcommand(args []string) []string {
	for len(args) >= 2 && (args[0] == "-C" || args[0] == "-c") {
		args = args[2:]
	}
	return args
}

// isCheckCheckoutAdd reports whether args materialize a check execution's
// detached checkout (checkExecutionCheckoutPath's .../checks/<op>/tree).
func isCheckCheckoutAdd(args []string) bool {
	sub := gitSubcommand(args)
	return len(sub) >= 3 && sub[0] == "worktree" && sub[1] == "add" && sub[2] == "--detach" &&
		slices.ContainsFunc(sub, func(arg string) bool { return strings.Contains(arg, "/checks/") })
}

// seedSettledResultCheck records the settled per-task check request an
// integrated task's accepted result always carries by the time its
// integration runs.
func seedSettledResultCheck(f *integrationFixture) {
	f.tc.Store.CheckRequests[f.resultID] = app.CheckRequest{
		ResultID: f.resultID, AttemptID: f.attemptID, State: app.CheckRequestSettled, CreatedAt: f.tc.Clock.Now(),
	}
}

// newCheckingFixture is an integration fixture driven to a published
// candidate in checking, returning the candidate and the check spawns
// the merge already made.
func newCheckingFixture(t *testing.T) (f *integrationFixture, merged string, spawns int) {
	t.Helper()
	f = newIntegrationFixture(t, false)
	seedSettledResultCheck(f)
	f.driveUntil(t, string(run.IntegrationChecking), 5)
	return f, f.git.ref(integrationRefName), f.git.CheckExecCalls
}

// requireStopTerminates drives one stop round with every pane absent and
// requires the run stopped with the candidate rolled back.
func requireStopTerminates(t *testing.T, f *integrationFixture, merged string) {
	t.Helper()
	scriptAbsentPanes(f.tc)
	report, err := f.tc.Controller.DriveFeatureStop(context.Background(), f.fr.Handle)
	if err != nil {
		t.Fatalf("DriveFeatureStop() error = %v", err)
	}
	if !report.Terminated {
		t.Fatalf("DriveFeatureStop() = %+v, want terminated: nothing of the interrupted round may stay ambiguous", report)
	}
	if got := f.currentIntegrationRow(t).State; got != run.IntegrationRolledBack {
		t.Fatalf("integration state = %s, want rolled-back by the stop path", got)
	}
	head := f.git.ref(integrationRefName)
	if parents := f.git.parentsOf(head); len(parents) != 1 || parents[0] != merged {
		t.Fatalf("ref %s has parents %v, want the rollback of the published candidate %s", head, parents, merged)
	}
	if got := f.tc.Store.Tasks[f.taskID].value.State; got != run.TaskInterrupted {
		t.Fatalf("task state = %s, want interrupted", got)
	}
	if got := f.tc.Store.Runs[f.fr.RunID].value.State; got != run.RunStopped {
		t.Fatalf("run state = %s, want stopped", got)
	}
}

// TestCombinedCheckInterruptedMidSpawn pins what a stop's interruption of
// an in-flight combined check leaves: the canceled round returns at once
// with the execution journaled reconciling under its claim, and the stop
// path retires it by that claim and rolls the candidate back.
func TestCombinedCheckInterruptedMidSpawn(t *testing.T) {
	f, merged, _ := newCheckingFixture(t)
	claimOnCheckExec(f, 7272)
	entered := make(chan struct{})
	f.git.CheckExecGate = func(ctx context.Context, _ string) (app.CommandResult, bool, error) {
		close(entered)
		return canceledCommand(ctx)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan roundResult, 1)
	go func() {
		report, err := driveIntegrationStep(ctx, f.tc.Controller, f.fr.Handle)
		done <- roundResult{report, err}
	}()
	<-entered
	requestRunStop(f.tc, f.fr.RunID)
	cancel()

	result := awaitRound(t, done, "the interrupted combined-check round")
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("round error = %v, want the cancellation", result.err)
	}
	checks := f.opsOfKind(app.OpCheckRun)
	if len(checks) != 1 || checks[0].State != app.OperationReconciling {
		t.Fatalf("check executions = %+v, want one, reconciling under its claim", checks)
	}
	if got := f.currentIntegrationRow(t).State; got != run.IntegrationChecking {
		t.Fatalf("integration state = %s, want left checking for the stop path", got)
	}

	requireStopTerminates(t, f, merged)
	if got := f.tc.Store.Operations[checks[0].ID].State; got != app.OperationFailed {
		t.Fatalf("interrupted execution state = %s, want settled failed by the stop's group retirement", got)
	}
}

// TestCombinedCheckIntentRefusedUnderFailureCause pins that no combined
// check starts in a run carrying a terminal-failure cause: the candidate
// belongs to the terminal-failure shutdown.
func TestCombinedCheckIntentRefusedUnderFailureCause(t *testing.T) {
	f, _, spawns := newCheckingFixture(t)
	seedImplementTask(t, f.tc, f.fr.RunID, 2, "B", false, run.TaskFailed)

	report, err := driveIntegrationStep(context.Background(), f.tc.Controller, f.fr.Handle)
	if err != nil {
		t.Fatalf("integration step error = %v", err)
	}
	if !report.Interrupted {
		t.Fatalf("report = %+v, want the round refused as interrupted", report)
	}
	if f.git.CheckExecCalls != spawns || len(f.opsOfKind(app.OpCheckRun)) != 0 {
		t.Fatalf("a combined check started in a failing run: %d spawn(s), %d execution(s)", f.git.CheckExecCalls-spawns, len(f.opsOfKind(app.OpCheckRun)))
	}
	if got := f.currentIntegrationRow(t).State; got != run.IntegrationChecking {
		t.Fatalf("integration state = %s, want left checking for the shutdown", got)
	}
}

// TestCombinedCheckShutdownRace pins the combined-check outcome against a
// concurrent terminal-failure shutdown: the shutdown settles the running
// execution by its claim and resets the candidate while the round's own
// passing outcome is still to be recorded. The round must back off — the
// integration rolled back, never integrated with the ref reset under it.
func TestCombinedCheckShutdownRace(t *testing.T) {
	f, merged, _ := newCheckingFixture(t)
	claimOnCheckExec(f, 7373)

	entered, proceed, roundDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var letRoundProceed sync.Once
	f.git.CheckExecGate = func(context.Context, string) (app.CommandResult, bool, error) {
		close(entered)
		<-proceed
		return app.CommandResult{ExitCode: 0}, true, nil
	}
	resetCASes := 0
	f.git.GitGate = func(_ context.Context, args []string) (app.CommandResult, bool, error) {
		// The shutdown's rollback CAS (expected-old = the published
		// candidate) waits until the round has recorded whatever it will.
		if sub := gitSubcommand(args); len(sub) >= 1 && sub[0] == "update-ref" && sub[len(sub)-1] == merged {
			resetCASes++
			letRoundProceed.Do(func() { close(proceed) })
			<-roundDone
		}
		return app.CommandResult{}, false, nil
	}

	done := make(chan roundResult, 1)
	go func() {
		report, err := driveIntegrationStep(context.Background(), f.tc.Controller, f.fr.Handle)
		close(roundDone)
		done <- roundResult{report, err}
	}()
	<-entered

	// The run's durable failure cause arises while the check runs, and the
	// scheduling pass's retirement step drives the shutdown.
	seedImplementTask(t, f.tc, f.fr.RunID, 2, "B", false, run.TaskFailed)
	scriptAbsentPanes(f.tc)
	_, retireErr := f.tc.Controller.RetireSettledSessions(context.Background(), f.fr.Handle)
	letRoundProceed.Do(func() { close(proceed) })
	result := awaitRound(t, done, "the combined-check round")

	if resetCASes == 0 {
		t.Fatalf("the shutdown never reached the candidate's rollback CAS (retirement error %v)", retireErr)
	}
	if retireErr != nil {
		t.Fatalf("RetireSettledSessions() error = %v", retireErr)
	}
	if result.err != nil {
		t.Fatalf("round error = %v", result.err)
	}
	integ := f.currentIntegrationRow(t)
	if integ.State != run.IntegrationRolledBack {
		t.Fatalf("integration state = %s with the ref at %s, want rolled-back: the shutdown settled the execution first", integ.State, f.git.ref(integrationRefName))
	}
	if head := f.git.ref(integrationRefName); head == merged {
		t.Fatalf("the ref rests on the candidate the shutdown rolled back")
	}
	if got := f.tc.Store.Tasks[f.taskID].value.State; got == run.TaskIntegrated {
		t.Fatalf("task integrated past the shutdown's rollback")
	}
	checks := f.opsOfKind(app.OpCheckRun)
	if len(checks) != 1 || checks[0].State != app.OperationFailed {
		t.Fatalf("check executions = %+v, want the shutdown's failed settlement kept", checks)
	}
	if !result.report.Interrupted {
		t.Fatalf("round report = %+v, want interrupted: the shutdown owned the outcome", result.report)
	}
}

// TestCombinedCheckPreSpawnInterruption pins a combined-check round that
// ends before its spawn: nothing can have claimed the execution, so it
// settles failed as never spawned instead of staying a claimless pending
// intent that no stop round could ever resolve.
func TestCombinedCheckPreSpawnInterruption(t *testing.T) {
	requireNeverSpawned := func(t *testing.T, f *integrationFixture, spawns int) {
		t.Helper()
		checks := f.opsOfKind(app.OpCheckRun)
		if len(checks) != 1 {
			t.Fatalf("check executions = %d, want one", len(checks))
		}
		if checks[0].State != app.OperationFailed {
			t.Fatalf("execution state = %s, want failed", checks[0].State)
		}
		outcome, ok := checks[0].Outcome.(string)
		if !ok || !strings.HasPrefix(outcome, "never spawned: ") {
			t.Fatalf("execution outcome = %#v, want the never-spawned record", checks[0].Outcome)
		}
		if _, claimed := f.tc.Store.CheckExecClaims[checks[0].ID]; claimed {
			t.Fatalf("an unspawned execution carries a claim")
		}
		if f.git.CheckExecCalls != spawns {
			t.Fatalf("check spawns = %d, want none", f.git.CheckExecCalls-spawns)
		}
		if got := f.currentIntegrationRow(t).State; got != run.IntegrationChecking {
			t.Fatalf("integration state = %s, want checking", got)
		}
	}

	t.Run("canceled during the checkout materialization", func(t *testing.T) {
		f, merged, spawns := newCheckingFixture(t)
		entered := make(chan struct{})
		f.git.GitGate = func(ctx context.Context, args []string) (app.CommandResult, bool, error) {
			if !isCheckCheckoutAdd(args) {
				return app.CommandResult{}, false, nil
			}
			close(entered)
			return canceledCommand(ctx)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan roundResult, 1)
		go func() {
			report, err := driveIntegrationStep(ctx, f.tc.Controller, f.fr.Handle)
			done <- roundResult{report, err}
		}()
		<-entered
		requestRunStop(f.tc, f.fr.RunID)
		cancel()
		if result := awaitRound(t, done, "the interrupted round"); !errors.Is(result.err, context.Canceled) {
			t.Fatalf("round error = %v, want the cancellation", result.err)
		}
		f.git.GitGate = nil

		requireNeverSpawned(t, f, spawns)
		requireStopTerminates(t, f, merged)
	})

	t.Run("canceled at the dispatch revalidation, the stop still acts", func(t *testing.T) {
		f, merged, spawns := newCheckingFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// The stop and its interruption land once the checkout exists: the
		// round's next step is the revalidation before the spawn, under a
		// canceled context.
		f.git.GitGate = func(_ context.Context, args []string) (app.CommandResult, bool, error) {
			if isCheckCheckoutAdd(args) {
				requestRunStop(f.tc, f.fr.RunID)
				cancel()
			}
			return app.CommandResult{}, false, nil
		}
		if _, err := driveIntegrationStep(ctx, f.tc.Controller, f.fr.Handle); !errors.Is(err, context.Canceled) {
			t.Fatalf("round error = %v, want the cancellation", err)
		}
		f.git.GitGate = nil

		requireNeverSpawned(t, f, spawns)
		// The stop's own acts (the rollback commit and its CAS) run under
		// the handle's dispatch scope, which the interrupted round's
		// cancellation must not have ended.
		if !app.DispatchLiveForTest(f.fr.Handle) {
			t.Fatalf("the interrupted round's canceled heartbeat ended the handle's dispatch scope")
		}
		requireStopTerminates(t, f, merged)
	})

	t.Run("a stop refuses the spawn after the checkout", func(t *testing.T) {
		f, merged, spawns := newCheckingFixture(t)
		f.git.GitGate = func(_ context.Context, args []string) (app.CommandResult, bool, error) {
			if isCheckCheckoutAdd(args) {
				requestRunStop(f.tc, f.fr.RunID)
			}
			return app.CommandResult{}, false, nil
		}
		_, err := driveIntegrationStep(context.Background(), f.tc.Controller, f.fr.Handle)
		if !errors.Is(err, app.ErrStopRequested) {
			t.Fatalf("round error = %v, want the stop refusal", err)
		}
		f.git.GitGate = nil

		requireNeverSpawned(t, f, spawns)
		requireStopTerminates(t, f, merged)
	})
}
