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

// featureCheckFixture is a feature run whose implement task has an
// accepted result awaiting its per-task check, behind a fake git
// repository whose simulated check-exec children write their claims.
type featureCheckFixture struct {
	*mailboxFixture
	git *fakeGitRepo
}

func newFeatureCheckFixture(t *testing.T) *featureCheckFixture {
	t.Helper()
	f := newMailboxFixture(t)
	if outcome := f.submit(t); outcome.Kind != app.SubmissionAccepted {
		t.Fatalf("submission = %+v, want accepted", outcome)
	}
	f.tc.Store.repoByRoot["/repo"] = f.tc.Store.Runs[f.fr.RunID].value.RepositoryID
	snap := f.tc.Store.Snapshots[f.fr.RunID]
	snap.CheckArgv = []string{"/bin/hopcheck"}
	f.tc.Store.Snapshots[f.fr.RunID] = snap
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
		f.tc.Store.CheckExecClaims[id] = app.CheckExecClaim{OperationID: id, PID: 9912, ClaimedAt: f.tc.Clock.Now()}
	}
	return &featureCheckFixture{mailboxFixture: f, git: git}
}

// checkRuns lists the run's check executions.
func (f *featureCheckFixture) checkRuns() []app.Operation {
	var out []app.Operation
	for _, op := range f.tc.Store.Operations { //nolint:gocritic // rangeValCopy: test helper over a small map.
		if op.RunID == f.fr.RunID && op.Kind == app.OpCheckRun {
			out = append(out, op)
		}
	}
	return out
}

// requireRequeuedUnspawned requires the one execution settled failed as
// never spawned, unclaimed, with its request back to requested and the
// attempt and task still checking.
func (f *featureCheckFixture) requireRequeuedUnspawned(t *testing.T) {
	t.Helper()
	runs := f.checkRuns()
	if len(runs) != 1 {
		t.Fatalf("check executions = %d, want one", len(runs))
	}
	if runs[0].State != app.OperationFailed {
		t.Fatalf("execution state = %s, want failed: an unspawned execution is never left pending", runs[0].State)
	}
	if outcome, ok := runs[0].Outcome.(string); !ok || !strings.HasPrefix(outcome, "never spawned: ") {
		t.Fatalf("execution outcome = %#v, want the never-spawned record", runs[0].Outcome)
	}
	if _, claimed := f.tc.Store.CheckExecClaims[runs[0].ID]; claimed {
		t.Fatalf("an unspawned execution carries a claim")
	}
	if f.git.CheckExecCalls != 0 {
		t.Fatalf("check spawns = %d, want none", f.git.CheckExecCalls)
	}
	resultID := f.tc.Store.Results[f.w.AttemptID].ID
	if got := f.tc.Store.CheckRequests[resultID].State; got != app.CheckRequestRequested {
		t.Fatalf("check request state = %s, want requested for a fresh execution", got)
	}
	if got := f.tc.Store.Attempts[f.w.AttemptID].value.State; got != run.AttemptChecking {
		t.Fatalf("attempt state = %s, want checking", got)
	}
	if got := f.tc.Store.Tasks[f.TaskID].value.State; got != run.TaskChecking {
		t.Fatalf("task state = %s, want checking", got)
	}
}

// requireFreshExecutionCompletes requires a later round to run a fresh
// execution — nothing of the unspawned one blocks it — and complete the
// task.
func (f *featureCheckFixture) requireFreshExecutionCompletes(t *testing.T) {
	t.Helper()
	report, err := f.tc.Controller.DriveFeatureChecks(context.Background(), f.fr.Handle, integrationHopPath, nil)
	if err != nil {
		t.Fatalf("DriveFeatureChecks() error = %v", err)
	}
	if !report.Ran || !report.Passed || report.Blocked != "" {
		t.Fatalf("DriveFeatureChecks() = %+v, want a fresh passing execution", report)
	}
	if f.git.CheckExecCalls != 1 {
		t.Fatalf("check spawns = %d, want exactly the fresh one", f.git.CheckExecCalls)
	}
	if got := f.tc.Store.Tasks[f.TaskID].value.State; got != run.TaskCompleted {
		t.Fatalf("task state = %s, want completed", got)
	}
}

// TestFeatureCheckPreSpawnInterruption pins a per-task check round that
// ends before its spawn: nothing can have claimed the execution, so it
// settles failed as never spawned and its request is requeued, instead of
// staying a claimless pending intent that blocks every later round.
func TestFeatureCheckPreSpawnInterruption(t *testing.T) {
	t.Run("canceled during the checkout materialization", func(t *testing.T) {
		f := newFeatureCheckFixture(t)
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
		done := make(chan error, 1)
		go func() {
			_, err := f.tc.Controller.DriveFeatureChecks(ctx, f.fr.Handle, integrationHopPath, nil)
			done <- err
		}()
		<-entered
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("round error = %v, want the cancellation", err)
		}
		f.git.GitGate = nil

		f.requireRequeuedUnspawned(t)
		f.requireFreshExecutionCompletes(t)
	})

	t.Run("canceled at the dispatch revalidation leaves the handle usable", func(t *testing.T) {
		f := newFeatureCheckFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// The cancel lands once the checkout exists: the round's next step
		// is the revalidation before the spawn, under a canceled context.
		f.git.GitGate = func(_ context.Context, args []string) (app.CommandResult, bool, error) {
			if isCheckCheckoutAdd(args) {
				cancel()
			}
			return app.CommandResult{}, false, nil
		}
		_, err := f.tc.Controller.DriveFeatureChecks(ctx, f.fr.Handle, integrationHopPath, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("round error = %v, want the cancellation", err)
		}
		f.git.GitGate = nil

		f.requireRequeuedUnspawned(t)
		// The caller's own cancellation says nothing about the lease: the
		// handle's dispatch scope stays live for every later act.
		if !app.DispatchLiveForTest(f.fr.Handle) {
			t.Fatalf("the interrupted round's canceled heartbeat ended the handle's dispatch scope")
		}
		f.requireFreshExecutionCompletes(t)
	})

	t.Run("a stop refuses the spawn after the checkout", func(t *testing.T) {
		f := newFeatureCheckFixture(t)
		f.git.GitGate = func(_ context.Context, args []string) (app.CommandResult, bool, error) {
			if isCheckCheckoutAdd(args) {
				requestRunStop(f.tc, f.fr.RunID)
			}
			return app.CommandResult{}, false, nil
		}
		_, err := f.tc.Controller.DriveFeatureChecks(context.Background(), f.fr.Handle, integrationHopPath, nil)
		if !errors.Is(err, app.ErrStopRequested) {
			t.Fatalf("round error = %v, want the stop refusal", err)
		}
		f.git.GitGate = nil

		f.requireRequeuedUnspawned(t)
		// Nothing of the round is left for the stop to wait on.
		for _, op := range f.checkRuns() {
			if op.State == app.OperationPending || op.State == app.OperationReconciling {
				t.Fatalf("execution %s left %s under a stop", op.ID, op.State)
			}
		}
	})
}
