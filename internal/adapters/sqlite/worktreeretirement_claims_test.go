package sqlite_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
)

// retirementIntent is a well-formed worktree-retirement intent's exec
// shape, as the application persists it.
func retirementIntent(argv []string, cwd string) map[string]any {
	return map[string]any{"argv": argv, "cwd": cwd}
}

var (
	retirementCheckTestArgv = []string{"/usr/bin/git", "-C", "/repos/feature", "merge-base", "--is-ancestor", "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"}       //nolint:gochecknoglobals // immutable test vector shared by the claim and context tests.
	worktreeRetireTestArgv  = []string{"/usr/bin/git", "-C", "/repos/feature", "-c", "status.showUntrackedFiles=all", "-c", "core.fsmonitor=false", "worktree", "remove", "/worktrees/feature/hop-r1-t1a1"} //nolint:gochecknoglobals // immutable test vector shared by the claim and context tests.
)

// TestClaimCheckExecRetirementKinds proves both worktree-retirement kinds
// are exec-claimable exactly like check.run: a pending operation of the
// current generation is claimed once (same-pid retry idempotent, another
// pid refused); a prior generation or a settled operation is refused.
func TestClaimCheckExecRetirementKinds(t *testing.T) {
	for _, kind := range []app.OperationKind{app.OpRetirementCheck, app.OpWorktreeRetire} {
		newOp := func(t *testing.T, f *featureFixture, opN int, state app.OperationState) identity.OperationID {
			t.Helper()
			opID := identity.OperationID(uid(opN))
			f.inUOW(t, func(uow app.UnitOfWork) {
				if err := uow.Operations().Create(t.Context(), app.Operation{
					ID: opID, RunID: f.spec.RunID, Generation: f.lease.Generation, Kind: kind, State: state,
					Intent:    retirementIntent(worktreeRetireTestArgv, "/repos/feature"),
					CreatedAt: f.clock.Now(), UpdatedAt: f.clock.Now(),
				}); err != nil {
					t.Fatalf("create operation: %v", err)
				}
			})
			return opID
		}
		t.Run(string(kind)+" pending of the current generation", func(t *testing.T) {
			f := newFeatureFixture(t)
			opID := newOp(t, f, 7801, app.OperationPending)
			if err := f.store.ClaimCheckExec(t.Context(), opID, 4242); err != nil {
				t.Fatalf("ClaimCheckExec: %v", err)
			}
			if err := f.store.ClaimCheckExec(t.Context(), opID, 4242); err != nil {
				t.Fatalf("same-pid retry: %v", err)
			}
			if err := f.store.ClaimCheckExec(t.Context(), opID, 9999); err == nil {
				t.Fatal("a claim by a different pid was accepted")
			}
			if n := countRows(t, f.store, `SELECT COUNT(*) FROM check_exec_claims WHERE operation_id = ? AND pid = 4242`, opID.String()); n != 1 {
				t.Fatalf("claim rows = %d, want the one original row", n)
			}
		})
		t.Run(string(kind)+" of a prior generation", func(t *testing.T) {
			f := newFeatureFixture(t)
			opID := newOp(t, f, 7802, app.OperationPending)
			if err := f.store.ReleaseLease(t.Context(), f.lease); err != nil {
				t.Fatalf("release: %v", err)
			}
			if _, err := f.store.AcquireLease(t.Context(), f.spec.RunID, "controller-b"); err != nil {
				t.Fatalf("reacquire: %v", err)
			}
			if err := f.store.ClaimCheckExec(t.Context(), opID, 4242); err == nil {
				t.Fatal("a claim against a prior-generation retirement operation was accepted")
			}
			if n := countRows(t, f.store, `SELECT COUNT(*) FROM check_exec_claims WHERE operation_id = ?`, opID.String()); n != 0 {
				t.Fatalf("claim rows = %d, want none", n)
			}
		})
		t.Run(string(kind)+" already settled", func(t *testing.T) {
			f := newFeatureFixture(t)
			opID := newOp(t, f, 7803, app.OperationFailed)
			if err := f.store.ClaimCheckExec(t.Context(), opID, 4242); err == nil || !strings.Contains(err.Error(), "not a pending exec-claimable execution") {
				t.Fatalf("ClaimCheckExec(settled) = %v, want the pending-execution refusal", err)
			}
		})
	}
}

// TestLoadCheckExecutionContextRetirementKinds proves the store resolves
// each retirement kind's frozen argv and spawn directory from the intent
// verbatim, and fails closed on a malformed intent.
func TestLoadCheckExecutionContextRetirementKinds(t *testing.T) {
	f := newFeatureFixture(t)
	type seeded struct {
		id     identity.OperationID
		kind   app.OperationKind
		intent any
	}
	ops := []seeded{
		{identity.OperationID(uid(7811)), app.OpRetirementCheck, retirementIntent(retirementCheckTestArgv, "/repos/feature")},
		{identity.OperationID(uid(7812)), app.OpWorktreeRetire, retirementIntent(worktreeRetireTestArgv, "/repos/feature")},
		{identity.OperationID(uid(7813)), app.OpWorktreeRetire, map[string]any{"cwd": "/repos/feature"}},
		{identity.OperationID(uid(7814)), app.OpWorktreeRetire, map[string]any{"argv": []any{"/usr/bin/git", 7}, "cwd": "/repos/feature"}},
		{identity.OperationID(uid(7815)), app.OpRetirementCheck, map[string]any{"argv": retirementCheckTestArgv}},
		{identity.OperationID(uid(7816)), app.OpRetirementCheck, map[string]any{"argv": []string{}, "cwd": "/repos/feature"}},
		{identity.OperationID(uid(7817)), app.OpWorktreeRetire, "not an object"},
	}
	f.inUOW(t, func(uow app.UnitOfWork) {
		for _, op := range ops {
			if err := uow.Operations().Create(t.Context(), app.Operation{
				ID: op.id, RunID: f.spec.RunID, Generation: f.lease.Generation, Kind: op.kind, State: app.OperationPending,
				Intent: op.intent, CreatedAt: f.clock.Now(), UpdatedAt: f.clock.Now(),
			}); err != nil {
				t.Fatalf("create operation: %v", err)
			}
		}
	})

	for i, want := range [][]string{retirementCheckTestArgv, worktreeRetireTestArgv} {
		got, err := f.store.LoadCheckExecutionContext(t.Context(), ops[i].id)
		if err != nil {
			t.Fatalf("LoadCheckExecutionContext(%s) = %v", ops[i].kind, err)
		}
		if !slices.Equal(got.CheckArgv, want) {
			t.Errorf("%s argv = %q, want the intent's frozen argv %q", ops[i].kind, got.CheckArgv, want)
		}
		if got.CheckoutPath != "/repos/feature" || got.StateRoot != f.spec.Snapshot.StateRoot {
			t.Errorf("%s context = %+v, want the intent's spawn directory under the frozen state root", ops[i].kind, got)
		}
	}
	for _, broken := range ops[2:] {
		if _, err := f.store.LoadCheckExecutionContext(t.Context(), broken.id); err == nil {
			t.Errorf("malformed %s intent %v loaded; want fail closed", broken.kind, broken.intent)
		}
	}
}
