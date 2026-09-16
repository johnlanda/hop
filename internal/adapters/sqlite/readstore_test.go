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
	if detail.StateRoot != "/state/root" {
		t.Fatalf("state root = %q, want the frozen /state/root", detail.StateRoot)
	}
	if detail.Binding == nil || detail.Binding.IncarnationID != f.spec.IncarnationID {
		t.Fatalf("binding = %+v, want the current incarnation's binding", detail.Binding)
	}
	if detail.Binding.ServerInstance != "server-instance-1" {
		t.Fatalf("binding server instance = %q, want server-instance-1", detail.Binding.ServerInstance)
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

// TestLoadFrozenRun proves the lease-free frozen-run read returns the
// immutable snapshot, the repository root and the frozen brief.
func TestLoadFrozenRun(t *testing.T) {
	f := newFixture(t)

	frozen, err := f.store.LoadFrozenRun(t.Context(), f.spec.RunID)
	if err != nil {
		t.Fatalf("LoadFrozenRun: %v", err)
	}
	if frozen.RepositoryRoot != "/repos/alpha" {
		t.Fatalf("repository root = %q, want /repos/alpha", frozen.RepositoryRoot)
	}
	if frozen.Brief != f.spec.Brief {
		t.Fatalf("brief = %q, want %q", frozen.Brief, f.spec.Brief)
	}
	if frozen.Snapshot.StateRoot != "/state/root" || frozen.Snapshot.AssignmentDigest != "assignment-digest" {
		t.Fatalf("snapshot = %+v, want the frozen fixture snapshot", frozen.Snapshot)
	}
	if len(frozen.Snapshot.CheckArgv) != 2 || frozen.Snapshot.CheckArgv[1] != "check.sh" {
		t.Fatalf("check argv = %v, want the frozen [sh check.sh]", frozen.Snapshot.CheckArgv)
	}
}

// TestLoadFrozenRunUnknownRun proves an unknown run is ErrNotFound.
func TestLoadFrozenRunUnknownRun(t *testing.T) {
	f := newFixture(t)

	_, err := f.store.LoadFrozenRun(t.Context(), identity.RunID(uid(6601)))

	if !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("LoadFrozenRun of an unknown run = %v, want ErrNotFound", err)
	}
}

// TestLoadRunStatusLastCheck proves LastCheck surfaces both a pending check
// execution and a settled unknown one, with the unknown flag, the detail
// and the retained evidence paths.
func TestLoadRunStatusLastCheck(t *testing.T) {
	f := runningFixture(t)
	accepted, err := f.store.SubmitResult(t.Context(), f.submission(8201, "digest-1"))
	if err != nil || accepted.Kind != app.SubmissionAccepted {
		t.Fatalf("SubmitResult = %+v, %v", accepted, err)
	}
	opID := identity.OperationID(uid(7701))
	f.inUOW(t, func(uow app.UnitOfWork) {
		// A settled check execution whose outcome is unknown, with retained
		// stdout/stderr evidence tied to the accepted result.
		if createErr := uow.Operations().Create(t.Context(), app.Operation{
			ID: opID, RunID: f.spec.RunID, Generation: f.lease.Generation,
			Kind: app.OpCheckRun, State: app.OperationFailed,
			Intent:    map[string]any{"tree_oid": "abc"},
			Outcome:   map[string]any{"unknown": true, "detail": "process group retired after takeover; result unknown"},
			CreatedAt: f.clock.Now(), UpdatedAt: f.clock.Now(),
		}); createErr != nil {
			t.Fatalf("create settled unknown check operation: %v", createErr)
		}
		stdout := run.NewResultArtifact(identity.ArtifactID(uid(7702)), f.spec.RunID, accepted.ResultID, run.ArtifactCheckStdout, "/state/root/runs/x/checks/y/stdout", "d1")
		stderr := run.NewResultArtifact(identity.ArtifactID(uid(7703)), f.spec.RunID, accepted.ResultID, run.ArtifactCheckStderr, "/state/root/runs/x/checks/y/stderr", "d2")
		if saveErr := uow.Artifacts().Save(t.Context(), stdout); saveErr != nil {
			t.Fatalf("save stdout artifact: %v", saveErr)
		}
		if saveErr := uow.Artifacts().Save(t.Context(), stderr); saveErr != nil {
			t.Fatalf("save stderr artifact: %v", saveErr)
		}
	})

	detail, err := f.store.LoadRunStatus(t.Context(), f.spec.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus: %v", err)
	}
	if detail.LastCheck == nil {
		t.Fatal("LastCheck is nil, want the settled unknown check execution")
	}
	if detail.LastCheck.OperationID != opID || detail.LastCheck.State != app.OperationFailed {
		t.Fatalf("LastCheck identity/state = (%s, %s), want (%s, failed)", detail.LastCheck.OperationID, detail.LastCheck.State, opID)
	}
	if !detail.LastCheck.Unknown {
		t.Fatal("LastCheck.Unknown = false, want true for a settled unknown outcome")
	}
	if detail.LastCheck.Detail == "" {
		t.Fatal("LastCheck.Detail is empty, want the recorded unknown-outcome detail")
	}
	if len(detail.LastCheck.EvidencePaths) != 2 {
		t.Fatalf("LastCheck evidence paths = %v, want the retained stdout and stderr", detail.LastCheck.EvidencePaths)
	}
}

// TestLoadRunStatusLastCheckOutcomeVariations proves the outcome payload is
// read as opaque journal JSON: only the "unknown" (bool) and "detail"
// (string) members are surfaced, and an absent, null or differently typed
// member yields its zero value without ever failing the status load.
func TestLoadRunStatusLastCheckOutcomeVariations(t *testing.T) {
	cases := []struct {
		name        string
		outcome     any
		wantUnknown bool
		wantDetail  string
	}{
		{name: "literal unknown true with detail", outcome: map[string]any{"unknown": true, "detail": "retired"}, wantUnknown: true, wantDetail: "retired"},
		{name: "absent members", outcome: map[string]any{"exit_code": 1}, wantUnknown: false, wantDetail: ""},
		{name: "unknown as string", outcome: map[string]any{"unknown": "true", "detail": "x"}, wantUnknown: false, wantDetail: "x"},
		{name: "detail as number", outcome: map[string]any{"unknown": true, "detail": 42}, wantUnknown: true, wantDetail: ""},
		{name: "null members", outcome: map[string]any{"unknown": nil, "detail": nil}, wantUnknown: false, wantDetail: ""},
		{name: "empty outcome object", outcome: map[string]any{}, wantUnknown: false, wantDetail: ""},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			opID := identity.OperationID(uid(7710 + i))
			f.inUOW(t, func(uow app.UnitOfWork) {
				if createErr := uow.Operations().Create(t.Context(), app.Operation{
					ID: opID, RunID: f.spec.RunID, Generation: f.lease.Generation,
					Kind: app.OpCheckRun, State: app.OperationFailed,
					Intent:    map[string]any{"tree_oid": "abc"},
					Outcome:   tc.outcome,
					CreatedAt: f.clock.Now(), UpdatedAt: f.clock.Now(),
				}); createErr != nil {
					t.Fatalf("create check operation: %v", createErr)
				}
			})

			detail, err := f.store.LoadRunStatus(t.Context(), f.spec.RunID)
			if err != nil {
				t.Fatalf("LoadRunStatus must not fail on a type-varied outcome: %v", err)
			}
			if detail.LastCheck == nil {
				t.Fatal("LastCheck is nil, want the check execution")
			}
			if detail.LastCheck.Unknown != tc.wantUnknown {
				t.Fatalf("LastCheck.Unknown = %t, want %t", detail.LastCheck.Unknown, tc.wantUnknown)
			}
			if detail.LastCheck.Detail != tc.wantDetail {
				t.Fatalf("LastCheck.Detail = %q, want %q", detail.LastCheck.Detail, tc.wantDetail)
			}
		})
	}
}

// TestLoadRunStatusStopRequested proves the status read surfaces the run's
// monotonic stop flag, both in the detail and its embedded RunStatus.
func TestLoadRunStatusStopRequested(t *testing.T) {
	f := runningFixture(t)

	before, err := f.store.LoadRunStatus(t.Context(), f.spec.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus: %v", err)
	}
	if before.StopRequested {
		t.Fatal("stop requested = true before any stop request")
	}
	if stopErr := f.store.RequestStop(t.Context(), f.spec.RunID); stopErr != nil {
		t.Fatalf("RequestStop: %v", stopErr)
	}

	after, err := f.store.LoadRunStatus(t.Context(), f.spec.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus after stop: %v", err)
	}
	if !after.StopRequested {
		t.Fatal("stop requested = false after RequestStop")
	}
}

// TestListRunsStopRequested proves the run-list read surfaces the stop flag.
func TestListRunsStopRequested(t *testing.T) {
	f := newFixture(t)
	if err := f.store.RequestStop(t.Context(), f.spec.RunID); err != nil {
		t.Fatalf("RequestStop: %v", err)
	}

	statuses, err := f.store.ListRuns(t.Context(), "/repos/alpha")
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(statuses) != 1 || !statuses[0].StopRequested {
		t.Fatalf("listed statuses = %+v, want one run with StopRequested true", statuses)
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
