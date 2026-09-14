package sqlite_test

import (
	"errors"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// TestListRuns proves listing by repository root: an unknown root has no
// runs and no error, and a known root lists its runs in sequence order
// with the reconciling condition surfaced.
func TestListRuns(t *testing.T) {
	f := newFixture(t)
	second := newSpec("/repos/alpha", 2*specStride, f.clock.Now())
	if _, _, err := f.store.InitializeRun(t.Context(), second); err != nil {
		t.Fatalf("second InitializeRun: %v", err)
	}

	t.Run("unknown root returns no runs", func(t *testing.T) {
		statuses, err := f.store.ListRuns(t.Context(), "/repos/unknown")
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(statuses) != 0 {
			t.Fatalf("runs of an unknown root = %+v, want none", statuses)
		}
	})

	t.Run("runs listed in sequence order", func(t *testing.T) {
		statuses, err := f.store.ListRuns(t.Context(), "/repos/alpha")
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(statuses) != 2 {
			t.Fatalf("listed %d runs, want 2", len(statuses))
		}
		if statuses[0].Sequence != 1 || statuses[1].Sequence != 2 {
			t.Fatalf("sequences = %d, %d; want 1, 2", statuses[0].Sequence, statuses[1].Sequence)
		}
		if statuses[0].RunID != f.spec.RunID || statuses[1].RunID != second.RunID {
			t.Fatalf("listed ids = %s, %s; want %s, %s", statuses[0].RunID, statuses[1].RunID, f.spec.RunID, second.RunID)
		}
		if statuses[0].State != run.RunCreated || statuses[0].Reconciling {
			t.Fatalf("first status = %+v, want created and not reconciling", statuses[0])
		}
	})

	t.Run("reconciling condition surfaces", func(t *testing.T) {
		f.inUOW(t, func(uow app.UnitOfWork) {
			err := uow.Operations().Create(t.Context(), app.Operation{
				ID: identity.OperationID(uid(7201)), RunID: f.spec.RunID, Generation: f.lease.Generation,
				Kind: app.OpPaneOpen, State: app.OperationReconciling,
				Intent:    map[string]any{"creation_label": "l"},
				CreatedAt: f.clock.Now(), UpdatedAt: f.clock.Now(),
			})
			if err != nil {
				t.Fatalf("create reconciling operation: %v", err)
			}
		})

		statuses, err := f.store.ListRuns(t.Context(), "/repos/alpha")
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if !statuses[0].Reconciling || statuses[1].Reconciling {
			t.Fatalf("reconciling flags = %t, %t; want true, false", statuses[0].Reconciling, statuses[1].Reconciling)
		}
	})
}

// TestLoadRunStatus proves the detail block assembles the identities,
// states, binding, claim, pending operations, last submission and
// artifacts of one run.
func TestLoadRunStatus(t *testing.T) {
	f := runningFixture(t)
	createWorktree(t, f)
	outcome, err := f.store.SubmitResult(t.Context(), f.submission(8101, "digest-1"))
	if err != nil || outcome.Kind != app.SubmissionAccepted {
		t.Fatalf("SubmitResult = %+v, %v", outcome, err)
	}
	artifactID := identity.ArtifactID(uid(7301))
	f.inUOW(t, func(uow app.UnitOfWork) {
		artifact := run.NewArtifact(artifactID, f.spec.RunID, run.ArtifactAssignment, "/state/root/runs/x/artifacts/assignment.md", "digest")
		if saveErr := uow.Artifacts().Save(t.Context(), artifact); saveErr != nil {
			t.Fatalf("save artifact: %v", saveErr)
		}
		createErr := uow.Operations().Create(t.Context(), app.Operation{
			ID: identity.OperationID(uid(7302)), RunID: f.spec.RunID, Generation: f.lease.Generation,
			Kind: app.OpCheckRun, State: app.OperationPending,
			Intent:    map[string]any{"tree_oid": "abc"},
			CreatedAt: f.clock.Now(), UpdatedAt: f.clock.Now(),
		})
		if createErr != nil {
			t.Fatalf("create pending operation: %v", createErr)
		}
	})

	detail, err := f.store.LoadRunStatus(t.Context(), f.spec.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus: %v", err)
	}
	if detail.RunID != f.spec.RunID || detail.Sequence != 1 {
		t.Fatalf("detail identity = %s seq %d, want %s seq 1", detail.RunID, detail.Sequence, f.spec.RunID)
	}
	if detail.TaskID != f.spec.TaskID || detail.AttemptID != f.spec.AttemptID || detail.SessionID != f.spec.SessionID {
		t.Fatalf("detail ids = (%s, %s, %s), want the fixture's task, attempt and session", detail.TaskID, detail.AttemptID, detail.SessionID)
	}
	if detail.State != run.RunRunning || detail.TaskState != run.TaskChecking || detail.AttemptState != run.AttemptSubmitted {
		t.Fatalf("states = (%s, %s, %s), want (running, checking, submitted)", detail.State, detail.TaskState, detail.AttemptState)
	}
	if detail.WorktreePath != "/worktrees/alpha-r1" {
		t.Fatalf("worktree path = %q, want /worktrees/alpha-r1", detail.WorktreePath)
	}
	if detail.Binding == nil || detail.Binding.IncarnationID != f.spec.IncarnationID {
		t.Fatalf("binding = %+v, want the current incarnation's binding", detail.Binding)
	}
	if detail.Claim == nil || detail.Claim.State != app.LaunchClaimExeced {
		t.Fatalf("claim = %+v, want the settled execed claim", detail.Claim)
	}
	if len(detail.PendingOperations) != 1 || detail.PendingOperations[0].ID != identity.OperationID(uid(7302)) {
		t.Fatalf("pending operations = %+v, want the one pending check.run", detail.PendingOperations)
	}
	if detail.LastSubmission == nil || detail.LastSubmission.Kind != app.SubmissionAccepted || detail.LastSubmission.ResultID != identity.ResultID(uid(8101)) {
		t.Fatalf("last submission = %+v, want the accepted receipt", detail.LastSubmission)
	}
	if len(detail.Artifacts) != 1 || detail.Artifacts[0].ID != artifactID {
		t.Fatalf("artifacts = %+v, want the recorded assignment artifact", detail.Artifacts)
	}
}

// TestLoadRunStatusBeforeLaunch proves the detail block is well-defined for
// a freshly initialized run: no binding, no claim, no submission.
func TestLoadRunStatusBeforeLaunch(t *testing.T) {
	f := newFixture(t)

	detail, err := f.store.LoadRunStatus(t.Context(), f.spec.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus: %v", err)
	}
	if detail.State != run.RunCreated || detail.TaskState != run.TaskPending || detail.AttemptState != run.AttemptReserved {
		t.Fatalf("states = (%s, %s, %s), want (created, pending, reserved)", detail.State, detail.TaskState, detail.AttemptState)
	}
	if detail.SessionID != f.spec.SessionID {
		t.Fatalf("session id = %s, want the reserved session %s", detail.SessionID, f.spec.SessionID)
	}
	if detail.WorktreePath != "" || detail.Binding != nil || detail.Claim != nil || detail.LastSubmission != nil {
		t.Fatalf("pre-launch detail carries phantom state: %+v", detail)
	}
}

// TestLoadRunStatusUnknownRun proves an unknown run is ErrNotFound.
func TestLoadRunStatusUnknownRun(t *testing.T) {
	f := newFixture(t)

	_, err := f.store.LoadRunStatus(t.Context(), identity.RunID(uid(6301)))

	if !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("LoadRunStatus of an unknown run = %v, want ErrNotFound", err)
	}
}

// TestLoadLaunchContext proves the launch exec boundary's lease-free load:
// the frozen snapshot, session, binding, claim state and stop state.
func TestLoadLaunchContext(t *testing.T) {
	f := newFixture(t)
	f.launchAttempt(t)
	f.createBinding(t)
	f.claimLaunch(t)

	launchContext, err := f.store.LoadLaunchContext(t.Context(), f.spec.RunID, f.spec.AttemptID)
	if err != nil {
		t.Fatalf("LoadLaunchContext: %v", err)
	}
	snapshot := launchContext.Snapshot
	if snapshot.Harness != "claude" || snapshot.StateRoot != "/state/root" || snapshot.AssignmentDigest != "assignment-digest" {
		t.Fatalf("snapshot = %+v, want the frozen fixture snapshot", snapshot)
	}
	if len(snapshot.CheckArgv) != 2 || snapshot.CheckArgv[0] != "sh" {
		t.Fatalf("check argv = %v, want the frozen [sh check.sh]", snapshot.CheckArgv)
	}
	if snapshot.EnvPolicy.Version != app.EnvPolicyVersion1 || snapshot.EnvPolicy.Harness != app.HarnessClaude {
		t.Fatalf("env policy = %+v, want the frozen versioned policy", snapshot.EnvPolicy)
	}
	if launchContext.Harness != run.HarnessClaude {
		t.Fatalf("harness = %s, want claude", launchContext.Harness)
	}
	if launchContext.Session.ID != f.spec.SessionID || launchContext.Session.NativeSessionRef != f.spec.NativeSessionRef {
		t.Fatalf("session = %+v, want the fixture session with its native reference", launchContext.Session)
	}
	if launchContext.Binding.IncarnationID != f.spec.IncarnationID {
		t.Fatalf("binding incarnation = %s, want %s", launchContext.Binding.IncarnationID, f.spec.IncarnationID)
	}
	if launchContext.Claim == nil || launchContext.Claim.State != app.LaunchClaimExecPending {
		t.Fatalf("claim = %+v, want the exec_pending claim", launchContext.Claim)
	}
	if launchContext.StopRequested {
		t.Fatal("stop requested = true on a run with no stop request")
	}

	if stopErr := f.store.RequestStop(t.Context(), f.spec.RunID); stopErr != nil {
		t.Fatalf("RequestStop: %v", stopErr)
	}
	stopped, err := f.store.LoadLaunchContext(t.Context(), f.spec.RunID, f.spec.AttemptID)
	if err != nil {
		t.Fatalf("LoadLaunchContext after stop: %v", err)
	}
	if !stopped.StopRequested {
		t.Fatal("stop requested = false after RequestStop")
	}
}

// TestLoadLaunchContextBeforeBinding proves the launcher's load succeeds
// as soon as InitializeRun's rows exist: hop launch is the pane's own
// command and can run before the controller records the binding row, so a
// missing binding yields the zero Binding and a nil Claim, never an error.
func TestLoadLaunchContextBeforeBinding(t *testing.T) {
	f := newFixture(t)

	launchContext, err := f.store.LoadLaunchContext(t.Context(), f.spec.RunID, f.spec.AttemptID)
	if err != nil {
		t.Fatalf("LoadLaunchContext before the binding row: %v", err)
	}
	if launchContext.Snapshot.StateRoot != "/state/root" || launchContext.Snapshot.AssignmentDigest != "assignment-digest" {
		t.Fatalf("snapshot = %+v, want the frozen fixture snapshot", launchContext.Snapshot)
	}
	if launchContext.Session.ID != f.spec.SessionID || launchContext.Session.NativeSessionRef != f.spec.NativeSessionRef {
		t.Fatalf("session = %+v, want the fixture session with its native reference", launchContext.Session)
	}
	if launchContext.Binding != (run.RuntimeBinding{}) {
		t.Fatalf("binding before the row is committed = %+v, want the zero value", launchContext.Binding)
	}
	if launchContext.Claim != nil {
		t.Fatalf("claim before any binding = %+v, want nil", launchContext.Claim)
	}
	if launchContext.StopRequested {
		t.Fatal("stop requested = true on a run with no stop request")
	}
}

// TestLoadLaunchContextAttemptOfOtherRun proves the agreement check: an
// attempt that does not belong to the run is ErrNotFound.
func TestLoadLaunchContextAttemptOfOtherRun(t *testing.T) {
	f := newFixture(t)
	other := newSpec("/repos/beta", 2*specStride, f.clock.Now())
	if _, _, err := f.store.InitializeRun(t.Context(), other); err != nil {
		t.Fatalf("second InitializeRun: %v", err)
	}

	_, err := f.store.LoadLaunchContext(t.Context(), f.spec.RunID, other.AttemptID)

	if !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("LoadLaunchContext across runs = %v, want ErrNotFound", err)
	}
}

// TestLoadCheckExecutionContext proves the check exec boundary's load: the
// frozen policy and argv, and the layout-derived checkout path.
func TestLoadCheckExecutionContext(t *testing.T) {
	f := newFixture(t)
	opID := identity.OperationID(uid(7401))
	f.inUOW(t, func(uow app.UnitOfWork) {
		err := uow.Operations().Create(t.Context(), app.Operation{
			ID: opID, RunID: f.spec.RunID, Generation: f.lease.Generation,
			Kind: app.OpCheckRun, State: app.OperationPending,
			Intent:    map[string]any{"tree_oid": "abc"},
			CreatedAt: f.clock.Now(), UpdatedAt: f.clock.Now(),
		})
		if err != nil {
			t.Fatalf("create operation: %v", err)
		}
	})

	executionContext, err := f.store.LoadCheckExecutionContext(t.Context(), opID)
	if err != nil {
		t.Fatalf("LoadCheckExecutionContext: %v", err)
	}
	if executionContext.StateRoot != "/state/root" {
		t.Fatalf("state root = %q, want the frozen /state/root", executionContext.StateRoot)
	}
	wantCheckout := "/state/root/runs/" + f.spec.RunID.String() + "/checks/" + opID.String() + "/tree"
	if executionContext.CheckoutPath != wantCheckout {
		t.Fatalf("checkout path = %q, want %q", executionContext.CheckoutPath, wantCheckout)
	}
	if len(executionContext.CheckArgv) != 2 || executionContext.CheckArgv[1] != "check.sh" {
		t.Fatalf("check argv = %v, want the frozen [sh check.sh]", executionContext.CheckArgv)
	}
	if executionContext.EnvPolicy.Version != app.EnvPolicyVersion1 {
		t.Fatalf("env policy = %+v, want the frozen versioned policy", executionContext.EnvPolicy)
	}
}

// TestLoadCheckExecutionContextWrongKind proves a non-check operation is
// refused.
func TestLoadCheckExecutionContextWrongKind(t *testing.T) {
	f := newFixture(t)
	opID := identity.OperationID(uid(7402))
	f.inUOW(t, func(uow app.UnitOfWork) {
		err := uow.Operations().Create(t.Context(), app.Operation{
			ID: opID, RunID: f.spec.RunID, Generation: f.lease.Generation,
			Kind: app.OpPaneOpen, State: app.OperationPending,
			Intent:    map[string]any{"creation_label": "l"},
			CreatedAt: f.clock.Now(), UpdatedAt: f.clock.Now(),
		})
		if err != nil {
			t.Fatalf("create operation: %v", err)
		}
	})

	if _, err := f.store.LoadCheckExecutionContext(t.Context(), opID); err == nil {
		t.Fatal("LoadCheckExecutionContext accepted a pane.open operation")
	}
}
