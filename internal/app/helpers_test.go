package app_test

import (
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// mcpGroupPane reproduces the pinned real InspectPane shape for a worker
// whose harness spawned MCP servers into its own process group (Claude
// Code 2.1.270, observed live through pane.process_info against herdr
// 0.9.0: `npm exec <server>`, `node .../<server>` and app-installed MCP
// server processes as same-pgid children of the harness). Herdr reports
// the members in raw platform listing order — macOS unsorted
// proc_listpids, Linux ascending pid — so the worker holds no particular
// index; in the executed live probe an MCP server was index 0, which this
// fixture mirrors by listing the foreign members FIRST. The foreign
// members carry foreign executables and no HOP marker anywhere.
func mcpGroupPane(workerPID int, workerArgv ...string) app.PaneProcess { //nolint:unparam // every current scenario scripts the suite's conventional worker pid 4242, but the parameter ties the fixture to the claim pid the calling test asserts against.
	return app.PaneProcess{
		ShellPID:          workerPID,
		ForegroundGroupID: workerPID,
		Foreground: []app.ProcessInfo{
			{PID: workerPID + 63, Argv0: "npm", Name: "npm", Argv: []string{"npm", "exec", "@executeautomation/playwright-mcp-server"}, Cmdline: "npm exec @executeautomation/playwright-mcp-server"},
			{PID: workerPID + 230, Argv0: "node", Name: "node", Argv: []string{"/opt/node/bin/node", "/tmp/npx/playwright-mcp-server"}, Cmdline: "/opt/node/bin/node /tmp/npx/playwright-mcp-server"},
			{PID: workerPID, Argv0: "claude", Name: "claude", Argv: workerArgv, Cmdline: ""},
		},
	}
}

// forceAttemptReserved directly sets runID's task, attempt and session
// back to pending/reserved, bypassing the domain's own transitions. It
// exists only to reach an otherwise-unreachable-through-the-use-cases
// fixture: the narrow crash window between InitializeRun and the pane.open
// launch intent.
func forceAttemptReserved(t *testing.T, tc *testController, runID identity.RunID) {
	t.Helper()
	taskID := tc.Store.TaskByRun[runID]
	attemptID := tc.Store.AttemptByRun[runID]
	tc.Store.Tasks[taskID].value.State = run.TaskPending
	tc.Store.Attempts[attemptID].value.State = run.AttemptReserved
	for id, sess := range tc.Store.Sessions {
		if sess.value.AttemptID == attemptID {
			tc.Store.Sessions[id].value.State = run.SessionReserved
		}
	}
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
