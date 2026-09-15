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
	if _, err := f.tc.Store.LoadFrozenRun(context.Background(), f.fr.RunID); err == nil {
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
		if _, err := f.tc.Controller.DriveIntegration(context.Background(), f.fr.Handle, integrationHopPath, nil); err == nil {
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
