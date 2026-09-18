package app_test

import (
	"context"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// launchClaimPID is the fixture manager launch claim's pid, matched by
// every InspectPaneFn stub the tests in this file script.
const launchClaimPID = 4242

// launchingFeatureRun seeds a feature-mode run directly into tc.Store,
// stopped one step short of seedFeatureRun's own bootstrap: the run stays
// RunLaunching and its manager session stays SessionLaunching, with an
// exec_pending launch claim and a runtime binding carrying a pane — the
// shape CorroborateSessionLaunches inspects on its very first round,
// before any settlement has ever committed.
func launchingFeatureRun(t *testing.T, tc *testController) (runID identity.RunID, managerID identity.SessionID, managerIncarnation identity.IncarnationID, handle app.RunHandle) {
	t.Helper()
	now := tc.Clock.Now()

	runID, err := identity.ParseRunID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse run id: %v", err)
	}
	repoID := identity.RepositoryID(tc.IDs.NewID())
	r := run.NewRun(runID, repoID, 1, "briefdigest", now)
	if r, err = r.Launch(now); err != nil {
		t.Fatalf("launch run: %v", err)
	}
	tc.Store.Runs[runID] = &entityRow[run.Run]{value: r, revision: 1}
	tc.Store.Snapshots[runID] = app.RunSnapshot{
		StateRoot: "/state",
		Workflow: app.WorkflowSnapshot{
			Mode: "feature", MaxWorkers: 2, RetryLimit: 3,
			ManagerRolePath:     "/state/runs/" + runID.String() + "/artifacts/roles/manager.md",
			ImplementerRolePath: "/state/runs/" + runID.String() + "/artifacts/roles/implementer.md",
			ReviewerRolePath:    "/state/runs/" + runID.String() + "/artifacts/roles/reviewer.md",
			IntegrationBranch:   "hop/r1/integration",
		},
	}
	tc.Store.Briefs[runID] = "feature brief"

	managerID, err = identity.ParseSessionID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse manager session id: %v", err)
	}
	manager := run.NewManagerSession(managerID, runID, run.HarnessClaude, now)
	if manager, err = manager.Launch(now); err != nil {
		t.Fatalf("launch manager session: %v", err)
	}
	tc.Store.Sessions[managerID] = &entityRow[run.Session]{value: manager, revision: 1}

	managerIncarnation, err = identity.ParseIncarnationID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse manager incarnation id: %v", err)
	}
	binding := run.NewRuntimeBinding(managerID, managerIncarnation, "", fakeServerToken(1), "workspace-mgr", "tab-mgr", "pane-mgr", "label-mgr", run.LaunchInitial, now)
	tc.Store.Bindings[managerID] = append(tc.Store.Bindings[managerID], binding)

	tc.Store.LaunchClaims[managerIncarnation] = app.LaunchClaim{
		IncarnationID: managerIncarnation, RunID: runID, SessionID: managerID,
		Executable: "/usr/bin/claude", PID: launchClaimPID, State: app.LaunchClaimExecPending,
	}

	lease := app.Lease{Run: runID, ControllerID: "controller-1", Generation: 1, ExpiresAt: now.Add(leaseTTL)}
	tc.Store.Leases[runID] = &leaseRow{lease: lease, held: true, repoID: repoID, created: true}
	handle = app.NewRunHandleForTest(runID, lease)
	return runID, managerID, managerIncarnation, handle
}

// TestCorroborateSessionLaunchesSettlesManagerRunToRunning proves
// settleSessionExeced applies the Run-level launching->running transition
// when the manager's own launch claim settles (design L560): without it,
// a launching feature run's manager settling its own launch claim would
// never move the run to running, and every manager verb (hop task
// create/plan close) would stay refused run-not-accepting forever. This
// drives the real Controller with the port fakes through exactly that
// settlement and proves both the transition and that a manager verb the
// settlement unblocks actually proceeds afterward.
func TestCorroborateSessionLaunchesSettlesManagerRunToRunning(t *testing.T) {
	tc := newTestController(defaultPolicy())
	runID, managerID, managerIncarnation, handle := launchingFeatureRun(t, tc)

	tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
		return app.PaneProcess{Foreground: []app.ProcessInfo{
			{PID: launchClaimPID, Argv0: "claude", Name: "claude", Argv: []string{"/usr/bin/claude", runID.String()}},
		}}, nil
	}

	reports, err := tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
	if err != nil {
		t.Fatalf("CorroborateSessionLaunches() error = %v", err)
	}
	if len(reports) != 1 || reports[0].SessionID != managerID.String() || reports[0].Progress != app.LaunchSettled {
		t.Fatalf("reports = %+v, want exactly one settled report for the manager session", reports)
	}

	updated, err := tc.Store.LoadRunStatus(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRunStatus() error = %v", err)
	}
	if updated.State != run.RunRunning {
		t.Fatalf("Run.State = %s, want %s", updated.State, run.RunRunning)
	}
	if tc.Store.Sessions[managerID].value.State != run.SessionActive {
		t.Fatalf("manager session state = %s, want active", tc.Store.Sessions[managerID].value.State)
	}
	if tc.Store.LaunchClaims[managerIncarnation].State != app.LaunchClaimExeced {
		t.Fatalf("launch claim was not settled to execed")
	}

	// The pass proceeds: a manager verb CanAcceptManagerVerb refused
	// run-not-accepting before this fix now succeeds.
	created, err := tc.Controller.CreateTask(context.Background(), app.CreateTaskRequest{
		RunID: runID.String(), SessionID: managerID.String(), IncarnationID: managerIncarnation.String(),
		StateRoot: "/state", Title: "A", InstructionsBody: []byte("do the thing"),
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if created.Outcome != string(app.WorkflowAccepted) {
		t.Fatalf("CreateTask() after settlement = %+v, want accepted (the run must be running)", created)
	}
}

// TestCorroborateSessionLaunchesAlreadyExecedAppliesRunRunning covers the
// "claim already execed" branch (a prior round settled the claim through
// some other route without the run-running consequence — the same defect
// shape, reached via the idempotent catch-up path instead of a fresh
// inspection).
func TestCorroborateSessionLaunchesAlreadyExecedAppliesRunRunning(t *testing.T) {
	tc := newTestController(defaultPolicy())
	runID, _, managerIncarnation, handle := launchingFeatureRun(t, tc)

	claim := tc.Store.LaunchClaims[managerIncarnation]
	claim.State = app.LaunchClaimExeced
	tc.Store.LaunchClaims[managerIncarnation] = claim

	reports, err := tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
	if err != nil {
		t.Fatalf("CorroborateSessionLaunches() error = %v", err)
	}
	if len(reports) != 1 || reports[0].Progress != app.LaunchSettled {
		t.Fatalf("reports = %+v, want exactly one settled report", reports)
	}

	updated, err := tc.Store.LoadRunStatus(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRunStatus() error = %v", err)
	}
	if updated.State != run.RunRunning {
		t.Fatalf("Run.State = %s, want %s", updated.State, run.RunRunning)
	}
}

// TestApplyManagerRunRunningNeverAdvancesAResumingRun is the manager's
// correction: applyManagerRunRunning must move the run only from
// launching, never from resuming — resuming->running is ResumeFeature's
// own gate (every session warm, relaunched or retired, and no blocked
// integration operation), and a manager settlement alone bypassing it
// would be wrong the moment a feature run has more than the one session a
// solo run always has. Both the fresh-settlement path and the
// already-execed catch-up path must leave a resuming run exactly as they
// found it.
func TestApplyManagerRunRunningNeverAdvancesAResumingRun(t *testing.T) {
	t.Run("the settle path", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		runID, managerID, _, handle := launchingFeatureRun(t, tc)
		rRow := tc.Store.Runs[runID]
		resuming, err := rRow.value.EnterResuming(tc.Clock.Now())
		if err != nil {
			t.Fatalf("EnterResuming() error = %v", err)
		}
		rRow.value = resuming

		tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) {
			return app.PaneProcess{Foreground: []app.ProcessInfo{
				{PID: launchClaimPID, Argv0: "claude", Name: "claude", Argv: []string{"/usr/bin/claude", runID.String()}},
			}}, nil
		}

		reports, err := tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		if len(reports) != 1 || reports[0].SessionID != managerID.String() || reports[0].Progress != app.LaunchSettled {
			t.Fatalf("reports = %+v, want exactly one settled report for the manager session", reports)
		}

		updated, err := tc.Store.LoadRunStatus(context.Background(), runID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.State != run.RunResuming {
			t.Fatalf("Run.State = %s, want unchanged %s (only ResumeFeature may advance a resuming run)", updated.State, run.RunResuming)
		}
	})

	t.Run("the already-execed catch-up path", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		runID, _, managerIncarnation, handle := launchingFeatureRun(t, tc)
		rRow := tc.Store.Runs[runID]
		resuming, err := rRow.value.EnterResuming(tc.Clock.Now())
		if err != nil {
			t.Fatalf("EnterResuming() error = %v", err)
		}
		rRow.value = resuming

		claim := tc.Store.LaunchClaims[managerIncarnation]
		claim.State = app.LaunchClaimExeced
		tc.Store.LaunchClaims[managerIncarnation] = claim

		reports, err := tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		if len(reports) != 1 || reports[0].Progress != app.LaunchSettled {
			t.Fatalf("reports = %+v, want exactly one settled report", reports)
		}

		updated, err := tc.Store.LoadRunStatus(context.Background(), runID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if updated.State != run.RunResuming {
			t.Fatalf("Run.State = %s, want unchanged %s (only ResumeFeature may advance a resuming run)", updated.State, run.RunResuming)
		}
	})
}
