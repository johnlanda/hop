package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/run"
)

// roundResult is one integration-step call's return, carried across a
// goroutine boundary.
type roundResult struct {
	report app.IntegrationReport
	err    error
}

// awaitRound receives one round's result, failing the test when the round
// does not return within the bound.
func awaitRound(t *testing.T, done <-chan roundResult, what string) roundResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return within the bound", what)
		return roundResult{}
	}
}

// TestDriveIntegrationReportsTheCombinedCheckDue pins the split of the
// integration step: DriveIntegration never runs the combined check — a
// checking integration with no settled receipt is reported CheckDue,
// repeatably, with nothing journaled or spawned — and
// DriveIntegrationCheck runs exactly that execution.
func TestDriveIntegrationReportsTheCombinedCheckDue(t *testing.T) {
	f := newIntegrationFixture(t, false)
	ctx := context.Background()
	env := []string{"PATH=/usr/bin"}
	for range 3 { // claim, merge, publish
		if _, err := f.tc.Controller.DriveIntegration(ctx, f.fr.Handle, integrationHopPath, env); err != nil {
			t.Fatalf("DriveIntegration() error = %v", err)
		}
	}
	if got := f.currentIntegrationRow(t).State; got != run.IntegrationChecking {
		t.Fatalf("integration state = %s, want checking after the publish", got)
	}
	spawns := f.git.CheckExecCalls // the merge's own

	for round := range 2 {
		report, err := f.tc.Controller.DriveIntegration(ctx, f.fr.Handle, integrationHopPath, env)
		if err != nil {
			t.Fatalf("DriveIntegration() round %d error = %v", round, err)
		}
		if !report.CheckDue || report.State != string(run.IntegrationChecking) || report.InFlight {
			t.Fatalf("DriveIntegration() round %d = %+v, want CheckDue on the checking integration", round, report)
		}
		if f.git.CheckExecCalls != spawns {
			t.Fatalf("DriveIntegration() spawned %d combined check(s); it must leave the execution to DriveIntegrationCheck", f.git.CheckExecCalls-spawns)
		}
		if ops := f.opsOfKind(app.OpCheckRun); len(ops) != 0 {
			t.Fatalf("DriveIntegration() journaled %d check execution(s), want none", len(ops))
		}
	}

	report, err := f.tc.Controller.DriveIntegrationCheck(ctx, f.fr.Handle, integrationHopPath, env)
	if err != nil {
		t.Fatalf("DriveIntegrationCheck() error = %v", err)
	}
	if report.State != string(run.IntegrationIntegrated) || report.CheckDue {
		t.Fatalf("DriveIntegrationCheck() = %+v, want the passing candidate integrated", report)
	}
	if f.git.CheckExecCalls != spawns+1 || len(f.opsOfKind(app.OpCheckRun)) != 1 {
		t.Fatalf("check spawns = %d, executions = %d, want exactly one of each", f.git.CheckExecCalls-spawns, len(f.opsOfKind(app.OpCheckRun)))
	}
	if got := f.tc.Store.Tasks[f.taskID].value.State; got != run.TaskIntegrated {
		t.Fatalf("task state = %s, want integrated", got)
	}
}

// TestIntegrationStepExclusion pins the per-handle exclusion of the two
// integration-step calls: a call made while the other runs does nothing
// and reports InFlight, and the combined-check round starts nothing while
// the step has an unresolved operation or no checking integration.
func TestIntegrationStepExclusion(t *testing.T) {
	env := []string{"PATH=/usr/bin"}

	t.Run("a combined-check round in flight leaves DriveIntegration inert", func(t *testing.T) {
		f, _, _ := newCheckingFixture(t)
		entered, release := make(chan struct{}), make(chan struct{})
		f.git.CheckExecGate = func(context.Context, string) (app.CommandResult, bool, error) {
			close(entered)
			<-release
			return app.CommandResult{ExitCode: 0}, true, nil
		}
		done := make(chan roundResult, 1)
		go func() {
			report, err := f.tc.Controller.DriveIntegrationCheck(context.Background(), f.fr.Handle, integrationHopPath, env)
			done <- roundResult{report, err}
		}()
		<-entered

		callsBefore := len(f.tc.Commands.Calls)
		report, err := f.tc.Controller.DriveIntegration(context.Background(), f.fr.Handle, integrationHopPath, env)
		callsAfter := len(f.tc.Commands.Calls)
		close(release)
		if err != nil || !report.InFlight || report.CheckDue || report.State != "" {
			t.Fatalf("DriveIntegration() during the round = %+v err=%v, want InFlight and nothing else", report, err)
		}
		if callsAfter != callsBefore {
			t.Fatalf("DriveIntegration() ran %d command(s) while the round held the step", callsAfter-callsBefore)
		}

		result := awaitRound(t, done, "DriveIntegrationCheck()")
		if result.err != nil || result.report.State != string(run.IntegrationIntegrated) {
			t.Fatalf("DriveIntegrationCheck() = %+v err=%v, want integrated", result.report, result.err)
		}
		after, err := f.tc.Controller.DriveIntegration(context.Background(), f.fr.Handle, integrationHopPath, env)
		if err != nil || after.InFlight {
			t.Fatalf("DriveIntegration() after the round = %+v err=%v, want the step free again", after, err)
		}
	})

	t.Run("DriveIntegration in flight leaves the combined-check round inert", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		f.drive(t) // claim
		entered, release := make(chan struct{}), make(chan struct{})
		f.git.GitGate = func(_ context.Context, args []string) (app.CommandResult, bool, error) {
			if sub := gitSubcommand(args); len(sub) >= 2 && sub[0] == "worktree" && sub[1] == "add" {
				close(entered)
				<-release
			}
			return app.CommandResult{}, false, nil
		}
		done := make(chan roundResult, 1)
		go func() {
			report, err := f.tc.Controller.DriveIntegration(context.Background(), f.fr.Handle, integrationHopPath, env)
			done <- roundResult{report, err}
		}()
		<-entered

		report, err := f.tc.Controller.DriveIntegrationCheck(context.Background(), f.fr.Handle, integrationHopPath, env)
		close(release)
		if err != nil || !report.InFlight {
			t.Fatalf("DriveIntegrationCheck() during the merge = %+v err=%v, want InFlight", report, err)
		}
		if result := awaitRound(t, done, "DriveIntegration()"); result.err != nil {
			t.Fatalf("DriveIntegration() error = %v", result.err)
		}
		if f.git.CheckExecCalls != 1 {
			t.Fatalf("spawns = %d, want the merge's alone", f.git.CheckExecCalls)
		}
	})

	t.Run("an unresolved integration-step operation blocks the round", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		spawns := f.git.CheckExecCalls
		merged := f.git.newCommit("tree-merged", f.base, f.src)
		f.git.setRef(integrationRefName, merged)
		integrationID := f.seedIntegrationRow(t, run.IntegrationChecking, merged)
		f.seedOperation(t, app.OpCheckRun, app.OperationReconciling, map[string]any{
			"integration_id":     integrationID.String(),
			"subject_commit_oid": merged,
			"subject_tree_oid":   "tree-merged",
			"check_argv":         []any{"/bin/hopcheck"},
		}, nil)
		opsBefore := len(f.tc.Store.Operations)

		report, err := f.tc.Controller.DriveIntegrationCheck(context.Background(), f.fr.Handle, integrationHopPath, env)
		if err != nil || report.Blocked == "" {
			t.Fatalf("DriveIntegrationCheck() = %+v err=%v, want blocked on the unresolved execution", report, err)
		}
		if f.git.CheckExecCalls != spawns || len(f.tc.Store.Operations) != opsBefore {
			t.Fatalf("the round spawned %d check(s) and journaled %d operation(s) past an unresolved one", f.git.CheckExecCalls-spawns, len(f.tc.Store.Operations)-opsBefore)
		}
	})

	t.Run("an integration not in checking is left alone", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		f.drive(t) // claim: merging
		opsBefore := len(f.tc.Store.Operations)
		report, err := f.tc.Controller.DriveIntegrationCheck(context.Background(), f.fr.Handle, integrationHopPath, env)
		if err != nil || report.State != string(run.IntegrationMerging) || report.CheckDue {
			t.Fatalf("DriveIntegrationCheck() = %+v err=%v, want the merging integration reported untouched", report, err)
		}
		if f.git.CheckExecCalls != 0 || len(f.tc.Store.Operations) != opsBefore {
			t.Fatalf("the round acted on a merging integration")
		}
	})
}
