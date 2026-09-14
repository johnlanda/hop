package app_test

import (
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// forceAttemptReserved directly sets runID's task and attempt back to
// pending/reserved, bypassing the domain's own transitions. It exists only
// to reach an otherwise-unreachable-through-the-use-cases fixture: a stop
// requested before StartRun's launch intent ever committed (the narrow
// crash window between InitializeRun and the pane.open intent).
func forceAttemptReserved(t *testing.T, tc *testController, runID identity.RunID) {
	t.Helper()
	taskID := tc.Store.TaskByRun[runID]
	attemptID := tc.Store.AttemptByRun[runID]
	tc.Store.Tasks[taskID].value.State = run.TaskPending
	tc.Store.Attempts[attemptID].value.State = run.AttemptReserved
}

// forceAttemptChecking directly sets detail's run/task/attempt to the
// completing/checking states a real check claim would leave them in,
// without driving ClaimAndRunCheck (which is synchronous end-to-end and so
// cannot itself produce an in-progress "checking" fixture for a stop test).
func forceAttemptChecking(t *testing.T, tc *testController, detail app.RunDetail) { //nolint:gocritic // hugeParam: RunDetail is a per-call test fixture value.
	t.Helper()
	tc.Store.Runs[detail.RunID].value.State = run.RunCompleting
	tc.Store.Tasks[detail.TaskID].value.State = run.TaskChecking
	tc.Store.Attempts[detail.AttemptID].value.State = run.AttemptChecking
}

// seedPendingCheckOperation inserts a pending OpCheckRun operation whose
// intent carries checkArgv, as ClaimAndRunCheck would have recorded before
// spawning hop check-exec, and returns its id.
func seedPendingCheckOperation(t *testing.T, tc *testController, runID identity.RunID, checkArgv []string) identity.OperationID {
	t.Helper()
	opID, err := identity.ParseOperationID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse operation id: %v", err)
	}
	tc.Store.Operations[opID] = app.Operation{
		ID: opID, RunID: runID, Generation: tc.Store.Leases[runID].lease.Generation,
		Kind: app.OpCheckRun, State: app.OperationPending,
		Intent: app.CheckRunIntent{CheckArgv: checkArgv},
	}
	return opID
}
