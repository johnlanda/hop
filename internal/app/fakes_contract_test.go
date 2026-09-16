package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
	"github.com/johnlanda/hop/internal/testsupport/runnervectors"
	"github.com/johnlanda/hop/internal/testsupport/storevectors"
)

// TestFakeStoreContracts proves the handwritten fakes enforce the store
// contracts the scenarios rely on (docs/plan/phase-2-design.md sections 4
// and 7): validation happens before any write is applied, so a failed
// commit or refused claim leaves base state unchanged.
func TestFakeStoreContracts(t *testing.T) {
	t.Run("commit rejects a duplicate runtime binding key and applies nothing", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		lease := tc.Store.Leases[detail.RunID].lease
		before := len(tc.Store.Bindings[detail.SessionID])
		transitionsBefore := len(tc.Store.Transitions)

		uow, err := tc.Store.Begin(context.Background(), lease)
		if err != nil {
			t.Fatalf("Begin() error = %v", err)
		}
		// The same (session, incarnation) key the pane.open outcome already
		// recorded, staged alongside an unrelated write.
		if err := uow.Bindings().Create(context.Background(), *detail.Binding); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		if err := uow.Transitions().Record(context.Background(), app.Transition{EntityKind: app.EntityRun, EntityID: detail.RunID.String()}); err != nil {
			t.Fatalf("Record() error = %v", err)
		}
		if err := uow.Commit(); err == nil {
			t.Fatalf("Commit() accepted a duplicate UNIQUE(session_id, incarnation_id) binding key")
		}
		if got := len(tc.Store.Bindings[detail.SessionID]); got != before {
			t.Fatalf("binding history length = %d after failed commit, want %d (nothing applied)", got, before)
		}
		if got := len(tc.Store.Transitions); got != transitionsBefore {
			t.Fatalf("transitions length = %d after failed commit, want %d (nothing applied)", got, transitionsBefore)
		}
	})

	t.Run("commit rechecks revisions read at begin against the store's current rows", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		lease := tc.Store.Leases[detail.RunID].lease

		uowA, err := tc.Store.Begin(context.Background(), lease)
		if err != nil {
			t.Fatalf("Begin() (A) error = %v", err)
		}
		rA, revA, err := uowA.Runs().Get(context.Background(), detail.RunID)
		if err != nil {
			t.Fatalf("Get() (A) error = %v", err)
		}
		if _, saveErr := uowA.Runs().Save(context.Background(), rA, revA); saveErr != nil {
			t.Fatalf("Save() (A) error = %v", saveErr)
		}

		// B reads and commits the same row first.
		uowB, err := tc.Store.Begin(context.Background(), lease)
		if err != nil {
			t.Fatalf("Begin() (B) error = %v", err)
		}
		rB, revB, err := uowB.Runs().Get(context.Background(), detail.RunID)
		if err != nil {
			t.Fatalf("Get() (B) error = %v", err)
		}
		if _, err := uowB.Runs().Save(context.Background(), rB, revB); err != nil {
			t.Fatalf("Save() (B) error = %v", err)
		}
		if err := uowB.Commit(); err != nil {
			t.Fatalf("Commit() (B) error = %v", err)
		}
		movedRevision := tc.Store.Runs[detail.RunID].revision

		if err := uowA.Commit(); !errors.Is(err, app.ErrRevisionConflict) {
			t.Fatalf("Commit() (A) error = %v, want ErrRevisionConflict (row moved since A's read)", err)
		}
		if got := tc.Store.Runs[detail.RunID].revision; got != movedRevision {
			t.Fatalf("run revision = %d after A's failed commit, want %d (B's write intact)", got, movedRevision)
		}
	})

	t.Run("commit at the lease expiry instant is fenced (strict expiry)", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		lease := tc.Store.Leases[detail.RunID].lease

		uow, err := tc.Store.Begin(context.Background(), lease)
		if err != nil {
			t.Fatalf("Begin() error = %v", err)
		}
		tc.Clock.Advance(lease.ExpiresAt.Sub(tc.Clock.Now())) // exactly the expiry instant
		if err := uow.Commit(); !errors.Is(err, app.ErrFenced) {
			t.Fatalf("Commit() at the expiry instant error = %v, want ErrFenced", err)
		}
	})

	t.Run("ClaimLaunch refuses a run with a stop request", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
		err := tc.Store.ClaimLaunch(context.Background(), app.LaunchClaim{
			IncarnationID: detail.Binding.IncarnationID, RunID: detail.RunID, AttemptID: detail.AttemptID,
			Executable: "/usr/bin/claude", PID: 4242, State: app.LaunchClaimExecPending,
		})
		if err == nil {
			t.Fatalf("ClaimLaunch() accepted a claim for a stopping run")
		}
	})

	t.Run("ClaimLaunch refuses a non-current incarnation", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		stale, err := identity.ParseIncarnationID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse incarnation id: %v", err)
		}
		claimErr := tc.Store.ClaimLaunch(context.Background(), app.LaunchClaim{
			IncarnationID: stale, RunID: detail.RunID, AttemptID: detail.AttemptID,
			Executable: "/usr/bin/claude", PID: 4242, State: app.LaunchClaimExecPending,
		})
		if claimErr == nil {
			t.Fatalf("ClaimLaunch() accepted a claim for a non-current incarnation")
		}
	})

	t.Run("ClaimLaunch same-pid rewrite is idempotent; different pid is refused", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		claimLaunch(t, tc, detail, 4242)
		claimLaunch(t, tc, detail, 4242) // idempotent rewrite
		err := tc.Store.ClaimLaunch(context.Background(), app.LaunchClaim{
			IncarnationID: detail.Binding.IncarnationID, RunID: detail.RunID, AttemptID: detail.AttemptID,
			Executable: "/usr/bin/claude", PID: 9999, State: app.LaunchClaimExecPending,
		})
		if err == nil {
			t.Fatalf("ClaimLaunch() accepted a second claim with a different pid")
		}
	})

	t.Run("ClaimCheckExec refuses a non-pending operation and a prior generation", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)

		settled := seedPendingCheckOperation(t, tc, detail.RunID, []string{"sh", "check.sh"})
		op := tc.Store.Operations[settled]
		op.State = app.OperationSucceeded
		tc.Store.Operations[settled] = op
		if err := tc.Store.ClaimCheckExec(context.Background(), settled, 5150); err == nil {
			t.Fatalf("ClaimCheckExec() accepted a settled operation")
		}

		priorGen := seedPendingCheckOperation(t, tc, detail.RunID, []string{"sh", "check.sh"})
		op = tc.Store.Operations[priorGen]
		op.Generation--
		tc.Store.Operations[priorGen] = op
		if err := tc.Store.ClaimCheckExec(context.Background(), priorGen, 5150); err == nil {
			t.Fatalf("ClaimCheckExec() accepted an operation of a prior generation")
		}
	})

	t.Run("ClaimCheckExec claims every exec-claimable kind and refuses every other", func(t *testing.T) {
		// The Phase 3 generalization (design section 3): check.run,
		// integration.merge and the two worktree-retirement executions are
		// the exec-claimable kinds; a ref-move operation or a Herdr act the
		// controller executes directly never accepts a claim.
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)

		seedKind := func(kind app.OperationKind) identity.OperationID {
			opID, err := identity.ParseOperationID(tc.IDs.NewID())
			if err != nil {
				t.Fatalf("parse operation id: %v", err)
			}
			tc.Store.Operations[opID] = app.Operation{
				ID: opID, RunID: detail.RunID, Generation: tc.Store.Leases[detail.RunID].lease.Generation,
				Kind: kind, State: app.OperationPending,
			}
			return opID
		}

		for _, kind := range []app.OperationKind{app.OpIntegrationMerge, app.OpRetirementCheck, app.OpWorktreeRetire} {
			if err := tc.Store.ClaimCheckExec(context.Background(), seedKind(kind), 5151); err != nil {
				t.Fatalf("ClaimCheckExec() refused a pending %s of the current generation: %v", kind, err)
			}
		}
		for _, kind := range []app.OperationKind{app.OpIntegrationPublish, app.OpIntegrationReset, app.OpIntegrationFence, app.OpPaneOpen} {
			if err := tc.Store.ClaimCheckExec(context.Background(), seedKind(kind), 5152); err == nil {
				t.Fatalf("ClaimCheckExec() accepted a pending %s operation, which is never exec-claimable", kind)
			}
		}
	})

	t.Run("ClaimCheckExec never overwrites: same pid idempotent, different pid refused", func(t *testing.T) {
		// The real store's pid-before-exec contract (sqlite submission.go's
		// ClaimCheckExec) for BOTH exec-claimable kinds: the claim row is
		// the group-retirement handle, so a claim is never re-armed or
		// overwritten — a same-pid retry returns success leaving the
		// original row untouched, and any other pid is refused.
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)

		for kind, seed := range map[app.OperationKind]func() identity.OperationID{
			app.OpCheckRun: func() identity.OperationID {
				return seedPendingCheckOperation(t, tc, detail.RunID, []string{"sh", "check.sh"})
			},
			app.OpIntegrationMerge: func() identity.OperationID {
				opID, err := identity.ParseOperationID(tc.IDs.NewID())
				if err != nil {
					t.Fatalf("parse operation id: %v", err)
				}
				tc.Store.Operations[opID] = app.Operation{
					ID: opID, RunID: detail.RunID, Generation: tc.Store.Leases[detail.RunID].lease.Generation,
					Kind: app.OpIntegrationMerge, State: app.OperationPending,
				}
				return opID
			},
		} {
			opID := seed()
			if err := tc.Store.ClaimCheckExec(context.Background(), opID, 5150); err != nil {
				t.Fatalf("%s: first claim refused: %v", kind, err)
			}
			original := tc.Store.CheckExecClaims[opID]
			if err := tc.Store.ClaimCheckExec(context.Background(), opID, 5150); err != nil {
				t.Fatalf("%s: same-pid retry refused: %v", kind, err)
			}
			if tc.Store.CheckExecClaims[opID] != original {
				t.Fatalf("%s: a same-pid retry rewrote the claim row", kind)
			}
			if err := tc.Store.ClaimCheckExec(context.Background(), opID, 6160); err == nil {
				t.Fatalf("%s: ClaimCheckExec() accepted a different pid over an existing claim", kind)
			}
			if tc.Store.CheckExecClaims[opID] != original {
				t.Fatalf("%s: a refused different-pid claim disturbed the row", kind)
			}
		}
	})

	t.Run("SubmitResult checks existence and agreement before duplicate receipts", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		req := defaultSubmitRequest(detail)
		if _, err := tc.Controller.SubmitResult(context.Background(), req); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}

		// The identical content, resubmitted under a task id the attempt
		// does not belong to: agreement fails before the receipt lookup, so
		// the outcome is malformed, never an idempotent duplicate.
		wrongTask, err := identity.ParseTaskID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse task id: %v", err)
		}
		disagreeing := req
		disagreeing.TaskID = wrongTask.String()
		result, err := tc.Controller.SubmitResult(context.Background(), disagreeing)
		if err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}
		if result.Kind != string(app.SubmissionMalformed) {
			t.Fatalf("Kind = %s, want %s (agreement precedes receipts)", result.Kind, app.SubmissionMalformed)
		}
	})

	t.Run("expired lease still refuses acquisition before its expiry instant", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		tc.Clock.Advance(leaseTTL - time.Second)
		if _, err := tc.Store.AcquireLease(context.Background(), detail.RunID, "controller-B"); !errors.Is(err, app.ErrLeaseHeld) {
			t.Fatalf("AcquireLease() error = %v, want ErrLeaseHeld while the lease is unexpired", err)
		}
	})
}

// TestFakePortsRefuseCallsInsideTransactions proves the fakes enforce the
// section 4 transaction rule mechanically: any Runtime, CommandRunner,
// ProcessGroupInspector or ArtifactStore call made while a unit of work is
// open fails, so a use case that leaks an external call into a store
// transaction cannot stay green.
func TestFakePortsRefuseCallsInsideTransactions(t *testing.T) {
	tc := newTestController(defaultPolicy())
	_, detail := startedRun(t, tc)
	lease := tc.Store.Leases[detail.RunID].lease

	uow, err := tc.Store.Begin(context.Background(), lease)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}

	if _, err := tc.Runtime.ServerInstance(context.Background()); err == nil {
		t.Fatalf("Runtime.ServerInstance succeeded inside an open transaction")
	}
	if _, err := tc.Runtime.InspectPane(context.Background(), detail.Binding.PaneID); err == nil {
		t.Fatalf("Runtime.InspectPane succeeded inside an open transaction")
	}
	if _, _, err := tc.Runtime.FindPaneByLabel(context.Background(), "label"); err == nil {
		t.Fatalf("Runtime.FindPaneByLabel succeeded inside an open transaction")
	}
	if err := tc.Runtime.ClosePane(context.Background(), detail.Binding.PaneID); err == nil {
		t.Fatalf("Runtime.ClosePane succeeded inside an open transaction")
	}
	if _, err := tc.Runtime.CreateWorkspace(context.Background(), app.WorkspaceRequest{Cwd: "/repo", Label: "label"}); err == nil {
		t.Fatalf("WorkspaceRuntime.CreateWorkspace succeeded inside an open transaction")
	}
	if _, _, err := tc.Runtime.FindWorkspaceByLabel(context.Background(), "label"); err == nil {
		t.Fatalf("WorkspaceRuntime.FindWorkspaceByLabel succeeded inside an open transaction")
	}
	if _, err := tc.Commands.Run(context.Background(), app.Command{Argv: []string{"/usr/bin/git", "-C", "/repo", "status"}}); err == nil {
		t.Fatalf("CommandRunner.Run succeeded inside an open transaction")
	}
	if _, err := tc.Groups.GroupProcesses(context.Background(), 1); err == nil {
		t.Fatalf("ProcessGroupInspector.GroupProcesses succeeded inside an open transaction")
	}
	if err := tc.Groups.SignalGroup(context.Background(), 1); err == nil {
		t.Fatalf("ProcessGroupInspector.SignalGroup succeeded inside an open transaction")
	}
	if err := tc.Artifacts.WriteArtifact(context.Background(), "/state/x", []byte("y")); err == nil {
		t.Fatalf("ArtifactStore.WriteArtifact succeeded inside an open transaction")
	}

	if err := uow.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	// With the transaction closed, the same calls run again.
	if _, err := tc.Runtime.ServerInstance(context.Background()); err != nil {
		t.Fatalf("Runtime.ServerInstance after commit error = %v", err)
	}
	if _, err := tc.Commands.Run(context.Background(), app.Command{Argv: []string{"/usr/bin/git", "-C", "/repo", "status"}}); err != nil {
		t.Fatalf("CommandRunner.Run after commit error = %v", err)
	}
}

// TestFakeCommandsBoundCapturedOutput runs the shared capture contract
// (internal/testsupport/runnervectors, which the real Runner's
// TestRunnerReportsTruncation also runs) through the fake CommandRunner,
// for scripted and hooked answers alike: each stream keeps its bound — 1
// MiB, or the command's own MaxOutputBytes — and is flagged truncated
// exactly when bytes were discarded, and a negative bound is refused
// before anything answers. Answers claiming a truncation the real Runner
// could never report are refused too.
func TestFakeCommandsBoundCapturedOutput(t *testing.T) {
	argv := []string{"/usr/bin/git", "-C", "/repo", "ls-files", "-v", "-z"}
	for _, v := range runnervectors.CaptureVectors() {
		for _, via := range []string{"scripted", "hooked"} {
			t.Run(v.Name+", "+via, func(t *testing.T) {
				commands := newTestController(defaultPolicy()).Commands
				answer := v.Answer()
				if via == "scripted" {
					commands.Results[strings.Join(argv, " ")] = answer
				} else {
					commands.RunHook = func(context.Context, app.Command) (app.CommandResult, bool, error) { return answer, true, nil }
				}
				result, err := commands.Run(context.Background(), app.Command{Argv: argv, MaxOutputBytes: v.MaxOutputBytes})
				if checkErr := v.Check(result, err); checkErr != nil {
					t.Error(checkErr)
				}
				if answered := len(commands.Calls) != 0; answered == v.Refused {
					t.Errorf("the command was answered = %v, want %v", answered, !v.Refused)
				}
			})
		}
	}

	const bound = fakeCaptureBytes
	t.Run("a hooked answer is bounded alongside its error, as the real Runner returns output with a cancellation", func(t *testing.T) {
		commands := newTestController(defaultPolicy()).Commands
		canceled := errors.New("canceled")
		commands.RunHook = func(context.Context, app.Command) (app.CommandResult, bool, error) {
			return app.CommandResult{ExitCode: -1, Stdout: make([]byte, bound+1)}, true, canceled
		}
		result, err := commands.Run(context.Background(), app.Command{Argv: argv})
		if !errors.Is(err, canceled) || len(result.Stdout) != bound || !result.StdoutTruncated {
			t.Fatalf("Run = %d bytes, truncated %v, %v; want the bounded output with the hook's error", len(result.Stdout), result.StdoutTruncated, err)
		}
	})

	for name, tc := range map[string]struct {
		answer app.CommandResult
		limit  int
	}{
		"a claimed stdout truncation short of the bound":                    {answer: app.CommandResult{Stdout: make([]byte, 10), StdoutTruncated: true}},
		"a claimed stderr truncation with no output":                        {answer: app.CommandResult{StderrTruncated: true}},
		"a claimed truncation holding the default bound under a larger one": {answer: app.CommandResult{Stdout: make([]byte, bound), StdoutTruncated: true}, limit: 2 * bound},
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			commands := newTestController(defaultPolicy()).Commands
			commands.RunHook = func(context.Context, app.Command) (app.CommandResult, bool, error) { return tc.answer, true, nil }
			if result, err := commands.Run(context.Background(), app.Command{Argv: argv, MaxOutputBytes: tc.limit}); err == nil {
				t.Fatalf("Run = %+v; want the impossible truncation refused", result)
			}
		})
	}
	for name, tc := range map[string]struct {
		answer app.CommandResult
		limit  int
	}{
		"a claimed truncation holding exactly the default bound": {answer: app.CommandResult{Stdout: make([]byte, bound), StdoutTruncated: true}},
		"a claimed truncation holding exactly a per-call bound":  {answer: app.CommandResult{Stderr: make([]byte, 100), StderrTruncated: true}, limit: 100},
	} {
		t.Run(name+" is kept", func(t *testing.T) {
			commands := newTestController(defaultPolicy()).Commands
			commands.RunHook = func(context.Context, app.Command) (app.CommandResult, bool, error) { return tc.answer, true, nil }
			result, err := commands.Run(context.Background(), app.Command{Argv: argv, MaxOutputBytes: tc.limit})
			if err != nil || result.StdoutTruncated != tc.answer.StdoutTruncated || result.StderrTruncated != tc.answer.StderrTruncated {
				t.Fatalf("Run = %+v, %v; want the claim kept", result, err)
			}
		})
	}
}

// TestWorkerWritesMoveRevisions proves the fake mirrors the real store's
// row movement for worker-authority writes: a stop request or an accepted
// submission bumps the same entity revisions the real transactions do, so
// a stale controller unit of work conflicts at commit instead of silently
// overwriting them.
func TestWorkerWritesMoveRevisions(t *testing.T) {
	t.Run("a staged unit of work cannot overwrite a stop request", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		lease := tc.Store.Leases[detail.RunID].lease

		uow, err := tc.Store.Begin(context.Background(), lease)
		if err != nil {
			t.Fatalf("Begin() error = %v", err)
		}
		r, rev, err := uow.Runs().Get(context.Background(), detail.RunID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if _, err := uow.Runs().Save(context.Background(), r, rev); err != nil {
			t.Fatalf("Save() error = %v", err)
		}

		// The worker-authority stop request lands while the unit of work is
		// still open.
		if err := tc.Controller.RequestStop(context.Background(), detail.RunID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}

		if err := uow.Commit(); !errors.Is(err, app.ErrRevisionConflict) {
			t.Fatalf("Commit() error = %v, want ErrRevisionConflict", err)
		}
		if !tc.Store.Runs[detail.RunID].value.StopRequested {
			t.Fatalf("the stop request was overwritten by the stale unit of work")
		}
	})

	t.Run("a staged unit of work cannot overwrite an accepted submission", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := runningRun(t, tc)
		lease := tc.Store.Leases[detail.RunID].lease

		uow, err := tc.Store.Begin(context.Background(), lease)
		if err != nil {
			t.Fatalf("Begin() error = %v", err)
		}
		a, rev, err := uow.Attempts().Get(context.Background(), detail.AttemptID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if _, err := uow.Attempts().Save(context.Background(), a, rev); err != nil {
			t.Fatalf("Save() error = %v", err)
		}

		if result, submitErr := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); submitErr != nil || result.Kind != string(app.SubmissionAccepted) {
			t.Fatalf("SubmitResult() = %+v, err %v; want accepted", result, submitErr)
		}

		if err := uow.Commit(); !errors.Is(err, app.ErrRevisionConflict) {
			t.Fatalf("Commit() error = %v, want ErrRevisionConflict", err)
		}
		if got := tc.Store.Attempts[detail.AttemptID].value.State; got != run.AttemptSubmitted {
			t.Fatalf("Attempt.State = %s; the atomic handoff was overwritten by the stale unit of work", got)
		}
	})

	t.Run("an unserializable operation payload fails the whole commit", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, detail := startedRun(t, tc)
		lease := tc.Store.Leases[detail.RunID].lease
		transitionsBefore := len(tc.Store.Transitions)

		uow, err := tc.Store.Begin(context.Background(), lease)
		if err != nil {
			t.Fatalf("Begin() error = %v", err)
		}
		opID, err := identity.ParseOperationID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse operation id: %v", err)
		}
		if err := uow.Operations().Create(context.Background(), app.Operation{
			ID: opID, RunID: detail.RunID, Kind: app.OpCheckRun, State: app.OperationPending,
			Intent: make(chan int), // not serializable
		}); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		if err := uow.Transitions().Record(context.Background(), app.Transition{EntityKind: app.EntityRun, EntityID: detail.RunID.String()}); err != nil {
			t.Fatalf("Record() error = %v", err)
		}

		if err := uow.Commit(); err == nil {
			t.Fatalf("Commit() accepted an unserializable operation payload")
		}
		if _, exists := tc.Store.Operations[opID]; exists {
			t.Fatalf("the unserializable operation reached the store")
		}
		if got := len(tc.Store.Transitions); got != transitionsBefore {
			t.Fatalf("transitions length = %d after failed commit, want %d (atomic rejection)", got, transitionsBefore)
		}
	})
}

// TestFakeFeatureBootstrapContracts proves the fakes reproduce the real
// adapters' feature-bootstrap contracts: fakeStore.InitializeRun's
// feature shape (the SQLite adapter's TestInitializeRunFeatureShape
// asserts the same rows), the one-manager partial unique index at commit,
// and the herdr adapter's WorkspaceRuntime argument refusals.
func TestFakeFeatureBootstrapContracts(t *testing.T) {
	t.Run("feature InitializeRun creates the run, snapshot, manager and lease only", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		spec := validFeatureRunSpec(t, tc, "/repo")
		runID, lease, err := tc.Store.InitializeRun(context.Background(), spec)
		if err != nil {
			t.Fatalf("InitializeRun() error = %v", err)
		}
		if runID != spec.RunID || lease.Generation != 1 || lease.ControllerID != spec.ControllerID {
			t.Fatalf("InitializeRun() = %s, %+v; want the spec's run at generation 1", runID, lease)
		}
		r := tc.Store.Runs[spec.RunID].value
		if r.State != run.RunCreated || r.Sequence != 1 {
			t.Fatalf("run = %+v, want created at sequence 1", r)
		}
		if got := tc.Store.Snapshots[spec.RunID].Workflow; got != spec.Snapshot.Workflow {
			t.Fatalf("frozen workflow = %+v, want %+v", got, spec.Snapshot.Workflow)
		}
		manager := tc.Store.Sessions[spec.SessionID].value
		if manager.Role != run.RoleManager || manager.AttemptID != "" || manager.ParentSessionID != nil ||
			manager.State != run.SessionReserved || manager.NativeSessionRef != spec.NativeSessionRef ||
			manager.NativeRefSource != run.NativeRefAssigned || manager.Harness != spec.Harness {
			t.Fatalf("manager session = %+v, want a reserved attempt-less parentless manager with the assigned native reference", manager)
		}
		if len(tc.Store.Tasks) != 0 || len(tc.Store.Attempts) != 0 || len(tc.Store.Worktrees) != 0 || len(tc.Store.Sessions) != 1 {
			t.Fatalf("feature InitializeRun created tasks=%d attempts=%d worktrees=%d sessions=%d, want 0/0/0/1",
				len(tc.Store.Tasks), len(tc.Store.Attempts), len(tc.Store.Worktrees), len(tc.Store.Sessions))
		}
	})

	t.Run("commit refuses a second non-terminal manager and admits a successor of a lost one", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		spec := validFeatureRunSpec(t, tc, "/repo")
		_, lease, err := tc.Store.InitializeRun(context.Background(), spec)
		if err != nil {
			t.Fatalf("InitializeRun() error = %v", err)
		}
		now := tc.Clock.Now()
		second := run.NewManagerSession(identity.SessionID(tc.IDs.NewID()), spec.RunID, run.HarnessClaude, now)

		uow, err := tc.Store.Begin(context.Background(), lease)
		if err != nil {
			t.Fatalf("Begin() error = %v", err)
		}
		if _, createErr := uow.Sessions().Create(context.Background(), second); createErr != nil {
			t.Fatalf("Create() error = %v", createErr)
		}
		if commitErr := uow.Commit(); commitErr == nil {
			t.Fatalf("Commit() accepted a second non-terminal manager session")
		}
		if _, ok := tc.Store.Sessions[second.ID]; ok {
			t.Fatalf("the refused commit applied the second manager")
		}

		uow, err = tc.Store.Begin(context.Background(), lease)
		if err != nil {
			t.Fatalf("Begin() error = %v", err)
		}
		first, rev, err := uow.Sessions().Get(context.Background(), spec.SessionID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		terminated, err := first.Terminate(now)
		if err != nil {
			t.Fatalf("Terminate() error = %v", err)
		}
		if _, err := uow.Sessions().Save(context.Background(), terminated, rev); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		if _, err := uow.Sessions().Create(context.Background(), second); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		if err := uow.Commit(); err != nil {
			t.Fatalf("Commit() of a successor to a terminated manager error = %v", err)
		}
	})

	t.Run("workspace runtime refuses the adapter's refused arguments", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		ctx := context.Background()
		// The shared vectors the herdr adapter's
		// TestRuntimeCreateWorkspaceRefusesInvalidRequests drives too.
		for _, vector := range storevectors.WorkspaceRequestsRefused() {
			if _, err := tc.Runtime.CreateWorkspace(ctx, vector.Request); err == nil {
				t.Fatalf("CreateWorkspace accepted the %s vector", vector.Name)
			}
		}
		if len(tc.Runtime.Workspaces) != 0 {
			t.Fatalf("a refused CreateWorkspace created a workspace")
		}
		if _, _, err := tc.Runtime.FindWorkspaceByLabel(ctx, ""); err == nil {
			t.Fatalf("FindWorkspaceByLabel accepted an empty label")
		}
		handle, err := tc.Runtime.CreateWorkspace(ctx, app.WorkspaceRequest{Cwd: "/repo", Label: "op-1"})
		if err != nil {
			t.Fatalf("CreateWorkspace() error = %v", err)
		}
		ref, found, err := tc.Runtime.FindWorkspaceByLabel(ctx, "op-1")
		if err != nil || !found || app.WorkspaceHandle(ref) != handle {
			t.Fatalf("FindWorkspaceByLabel(op-1) = %+v, %v, %v; want the created workspace's sole tab and root pane %+v", ref, found, err, handle)
		}
	})
}
