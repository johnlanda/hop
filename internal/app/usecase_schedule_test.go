package app_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// featureRun bundles a seeded feature-mode run's identities for scheduler
// and messaging scenarios.
type featureRun struct {
	RunID              identity.RunID
	ManagerID          identity.SessionID
	ManagerIncarnation identity.IncarnationID
	Handle             app.RunHandle
}

// fakeHeadCommitOID is the base commit object id classifyWorktreeProvenance
// accepts as valid against fakeCommands' default git stubs (fakes_ports_test.go):
// its "rev-parse HEAD" stub returns exactly this value regardless of cmd.Dir.
const fakeHeadCommitOID = "cccccccccccccccccccccccccccccccccccccccc"

// seedFeatureRun directly seeds a feature-mode run into tc.Store,
// bypassing StartRun's Phase 2 one-task/one-attempt bootstrap: a running
// Run with a WorkflowSnapshot, an active manager session and a controller
// lease, returning a legitimate RunHandle over that lease.
func seedFeatureRun(t *testing.T, tc *testController, maxWorkers int) featureRun {
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
	if r, err = r.MarkRunning(now); err != nil {
		t.Fatalf("mark run running: %v", err)
	}
	tc.Store.Runs[runID] = &entityRow[run.Run]{value: r, revision: 1}
	tc.Store.Snapshots[runID] = app.RunSnapshot{
		StateRoot: "/state",
		Workflow: app.WorkflowSnapshot{
			Mode: "feature", MaxWorkers: maxWorkers, RetryLimit: 3,
			IntegrationBranch: "hop/r1/integration",
		},
	}
	tc.Store.Briefs[runID] = "feature brief"

	managerID, err := identity.ParseSessionID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse manager session id: %v", err)
	}
	manager := run.NewManagerSession(managerID, runID, run.HarnessClaude, now)
	if manager, err = manager.Launch(now); err != nil {
		t.Fatalf("launch manager: %v", err)
	}
	if manager, err = manager.ConfirmActive(now); err != nil {
		t.Fatalf("activate manager: %v", err)
	}
	tc.Store.Sessions[managerID] = &entityRow[run.Session]{value: manager, revision: 1}

	managerIncarnation, err := identity.ParseIncarnationID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse manager incarnation id: %v", err)
	}
	binding := run.NewRuntimeBinding(managerID, managerIncarnation, "", "peer-pid:1", "workspace-mgr", "tab-mgr", "pane-mgr", "label-mgr", run.LaunchInitial, now)
	tc.Store.Bindings[managerID] = append(tc.Store.Bindings[managerID], binding)

	lease := app.Lease{Run: runID, ControllerID: "controller-1", Generation: 1, ExpiresAt: now.Add(leaseTTL)}
	tc.Store.Leases[runID] = &leaseRow{lease: lease, held: true, repoID: repoID, created: true}

	return featureRun{RunID: runID, ManagerID: managerID, ManagerIncarnation: managerIncarnation, Handle: app.NewRunHandleForTest(runID, lease)}
}

// seedImplementTask inserts an implement task directly, in the given
// state, with hasDependencies as given.
func seedImplementTask(t *testing.T, tc *testController, runID identity.RunID, seq int, title string, hasDependencies bool, state run.TaskState) identity.TaskID {
	t.Helper()
	id, err := identity.ParseTaskID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse task id: %v", err)
	}
	now := tc.Clock.Now()
	task := run.NewImplementTask(id, runID, seq, title, "instructions-digest", hasDependencies, now)
	task.State = state
	tc.Store.Tasks[id] = &entityRow[run.Task]{value: task, revision: 1}
	return id
}

func mustAssignedAttempt(t *testing.T, tc *testController, id identity.AttemptID) run.Attempt {
	t.Helper()
	row, ok := tc.Store.Attempts[id]
	if !ok {
		t.Fatalf("attempt %s not found", id)
	}
	return row.value
}

func TestRecomputeReleases(t *testing.T) {
	t.Run("a dependent task releases once its prerequisite integrates", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)

		taskA := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskIntegrated)
		taskB := seedImplementTask(t, tc, fr.RunID, 2, "B", true, run.TaskPending)
		edge, err := run.NewTaskDependency(taskB, taskA, tc.Clock.Now())
		if err != nil {
			t.Fatalf("NewTaskDependency() error = %v", err)
		}
		tc.Store.TaskDependencies = append(tc.Store.TaskDependencies, edge)

		report, err := tc.Controller.RecomputeReleases(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("RecomputeReleases() error = %v", err)
		}
		if len(report.Released) != 1 || report.Released[0].TaskID != taskB {
			t.Fatalf("Released = %+v, want exactly task B", report.Released)
		}
		if got := tc.Store.Tasks[taskB].value.State; got != run.TaskReady {
			t.Fatalf("task B state = %s, want ready", got)
		}
		if got := tc.Store.Tasks[taskA].value.State; got != run.TaskIntegrated {
			t.Fatalf("task A state = %s, want unchanged integrated", got)
		}
	})

	t.Run("a dependent task stays pending while its prerequisite is not integrated", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)

		taskA := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskActive)
		taskB := seedImplementTask(t, tc, fr.RunID, 2, "B", true, run.TaskPending)
		edge, err := run.NewTaskDependency(taskB, taskA, tc.Clock.Now())
		if err != nil {
			t.Fatalf("NewTaskDependency() error = %v", err)
		}
		tc.Store.TaskDependencies = append(tc.Store.TaskDependencies, edge)

		report, err := tc.Controller.RecomputeReleases(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("RecomputeReleases() error = %v", err)
		}
		if len(report.Released) != 0 {
			t.Fatalf("Released = %+v, want none", report.Released)
		}
		if got := tc.Store.Tasks[taskB].value.State; got != run.TaskPending {
			t.Fatalf("task B state = %s, want unchanged pending", got)
		}
	})

	t.Run("a zero-dependency task is untouched", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskA := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)

		report, err := tc.Controller.RecomputeReleases(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("RecomputeReleases() error = %v", err)
		}
		if len(report.Released) != 0 {
			t.Fatalf("Released = %+v, want none", report.Released)
		}
		if got := tc.Store.Tasks[taskA].value.State; got != run.TaskReady {
			t.Fatalf("task A state = %s, want unchanged ready", got)
		}
	})
}

func defaultAssignmentOptions() app.AssignmentOptions {
	return app.AssignmentOptions{
		MaxWorkers:               2,
		IntegrationHeadCommitOID: fakeHeadCommitOID,
		Harness:                  run.HarnessClaude,
		RepositoryRoot:           "/repo",
		HOPPath:                  "/usr/local/bin/hop",
		StateRoot:                "/state",
	}
}

func TestAssignReadyTasks(t *testing.T) {
	t.Run("assigns a ready implement task: task, attempt, session, worktree, pane", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskA := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)

		report, err := tc.Controller.AssignReadyTasks(context.Background(), fr.Handle, defaultAssignmentOptions())
		if err != nil {
			t.Fatalf("AssignReadyTasks() error = %v", err)
		}
		if len(report.Assigned) != 1 {
			t.Fatalf("Assigned = %+v, want exactly one", report.Assigned)
		}
		assigned := report.Assigned[0]
		if assigned.TaskID != taskA {
			t.Fatalf("assigned task = %s, want %s", assigned.TaskID, taskA)
		}
		if assigned.Role != run.RoleImplementer {
			t.Fatalf("assigned role = %s, want implementer", assigned.Role)
		}
		if assigned.AttemptNumber != 1 {
			t.Fatalf("assigned attempt number = %d, want 1", assigned.AttemptNumber)
		}

		if got := tc.Store.Tasks[taskA].value.State; got != run.TaskActive {
			t.Fatalf("task state = %s, want active", got)
		}
		attempt := mustAssignedAttempt(t, tc, assigned.AttemptID)
		if attempt.State != run.AttemptLaunching {
			t.Fatalf("attempt state = %s, want launching", attempt.State)
		}
		sessionRow, ok := tc.Store.Sessions[assigned.SessionID]
		if !ok {
			t.Fatalf("session %s not recorded", assigned.SessionID)
		}
		if sessionRow.value.State != run.SessionLaunching {
			t.Fatalf("session state = %s, want launching", sessionRow.value.State)
		}
		if sessionRow.value.Role != run.RoleImplementer {
			t.Fatalf("session role = %s, want implementer", sessionRow.value.Role)
		}
		if sessionRow.value.ParentSessionID == nil || *sessionRow.value.ParentSessionID != fr.ManagerID {
			t.Fatalf("session parent = %v, want manager %s", sessionRow.value.ParentSessionID, fr.ManagerID)
		}

		wt, ok := tc.Store.worktreeByRunLocked(fr.RunID)
		if !ok {
			t.Fatalf("no worktree recorded for run")
		}
		wantBranch := "hop/r1/t1a1"
		if wt.Branch != wantBranch {
			t.Fatalf("worktree branch = %s, want %s", wt.Branch, wantBranch)
		}

		if len(tc.Runtime.ClosedPanes) != 0 {
			t.Fatalf("no pane should have been closed during assignment")
		}
		binding, ok := tc.Store.currentBindingLocked(assigned.SessionID)
		if !ok {
			t.Fatalf("no runtime binding recorded for assigned session")
		}
		if binding.PaneID == "" {
			t.Fatalf("binding has no pane id")
		}
	})

	t.Run("a review task branches from its own frozen subject, not the integration head", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		now := tc.Clock.Now()
		reviewID, err := identity.ParseTaskID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse task id: %v", err)
		}
		subjectOID := fakeHeadCommitOID
		review := run.NewReviewTask(reviewID, fr.RunID, 1, subjectOID, "tttttttttttttttttttttttttttttttttttttttt", now)
		tc.Store.Tasks[reviewID] = &entityRow[run.Task]{value: review, revision: 1}

		opts := defaultAssignmentOptions()
		opts.IntegrationHeadCommitOID = "0000000000000000000000000000000000000000" // deliberately wrong for a review task
		opts.ReviewerHarness = run.HarnessClaude

		report, err := tc.Controller.AssignReadyTasks(context.Background(), fr.Handle, opts)
		if err != nil {
			t.Fatalf("AssignReadyTasks() error = %v", err)
		}
		if len(report.Assigned) != 1 {
			t.Fatalf("Assigned = %+v, want exactly one", report.Assigned)
		}
		if report.Assigned[0].Role != run.RoleReviewer {
			t.Fatalf("assigned role = %s, want reviewer", report.Assigned[0].Role)
		}
	})

	t.Run("MaxWorkers bounds concurrent assignment; a later seq waits for a free slot", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 1)
		taskA := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
		taskB := seedImplementTask(t, tc, fr.RunID, 2, "B", false, run.TaskReady)

		opts := defaultAssignmentOptions()
		opts.MaxWorkers = 1
		report, err := tc.Controller.AssignReadyTasks(context.Background(), fr.Handle, opts)
		if err != nil {
			t.Fatalf("AssignReadyTasks() error = %v", err)
		}
		if len(report.Assigned) != 1 || report.Assigned[0].TaskID != taskA {
			t.Fatalf("Assigned = %+v, want exactly task A", report.Assigned)
		}
		if !report.SlotsFull {
			t.Fatalf("SlotsFull = false, want true with a ready task still waiting")
		}
		if got := tc.Store.Tasks[taskB].value.State; got != run.TaskReady {
			t.Fatalf("task B state = %s, want unchanged ready (slot occupied)", got)
		}
	})

	t.Run("nothing to assign reports no error and no assignment", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		report, err := tc.Controller.AssignReadyTasks(context.Background(), fr.Handle, defaultAssignmentOptions())
		if err != nil {
			t.Fatalf("AssignReadyTasks() error = %v", err)
		}
		if len(report.Assigned) != 0 || report.SlotsFull {
			t.Fatalf("report = %+v, want empty", report)
		}
	})

	t.Run("a consumed retry's reserved attempt is launched, not recreated, and the pending request clears", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		now := tc.Clock.Now()

		taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskNeedsRework)
		firstAttemptID, err := identity.ParseAttemptID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse attempt id: %v", err)
		}
		firstAttempt, err := run.NewAttempt(firstAttemptID, taskID, 1, now)
		if err != nil {
			t.Fatalf("NewAttempt() error = %v", err)
		}
		if firstAttempt, err = firstAttempt.Launch(now); err != nil {
			t.Fatalf("Launch() error = %v", err)
		}
		if firstAttempt, err = firstAttempt.Interrupt(now); err != nil {
			t.Fatalf("Interrupt() error = %v", err)
		}
		tc.Store.Attempts[firstAttemptID] = &entityRow[run.Attempt]{value: firstAttempt, revision: 1}

		retryReq := app.RetryRequest{
			TaskID: taskID, RunID: fr.RunID, Session: fr.ManagerID, RequestID: "retry-1", Reason: "flaky",
			IncarnationID: fr.ManagerIncarnation,
		}

		accepted, err := tc.Store.RequestRetry(context.Background(), retryReq)
		if err != nil {
			t.Fatalf("RequestRetry() error = %v", err)
		}
		if accepted.Outcome != app.WorkflowAccepted || accepted.AttemptNumber != 2 {
			t.Fatalf("RequestRetry() = %+v, want accepted attempt 2", accepted)
		}
		if got := tc.Store.Tasks[taskID].value.State; got != run.TaskReady {
			t.Fatalf("task state after retry request = %s, want ready", got)
		}

		report, err := tc.Controller.AssignReadyTasks(context.Background(), fr.Handle, defaultAssignmentOptions())
		if err != nil {
			t.Fatalf("AssignReadyTasks() error = %v", err)
		}
		if len(report.Assigned) != 1 {
			t.Fatalf("Assigned = %+v, want exactly one", report.Assigned)
		}
		if report.Assigned[0].AttemptNumber != 2 {
			t.Fatalf("assigned attempt number = %d, want the reserved retry attempt 2", report.Assigned[0].AttemptNumber)
		}
		if tc.Store.RetryRequestStates[taskID] != app.RetryRequestConsumed {
			t.Fatalf("retry request state = %s, want consumed", tc.Store.RetryRequestStates[taskID])
		}
		if _, stillPending := tc.Store.RetryRequests[taskID]; stillPending {
			t.Fatalf("retry request row still present after consumption")
		}
	})
}

// plainUnitOfWork implements exactly app.UnitOfWork over an inner
// *fakeUnitOfWork, deliberately NOT also implementing
// app.WorkflowRepositories — a store that predates Phase 3 feature-mode
// support, for proving the fail-closed contract.
type plainUnitOfWork struct{ inner *fakeUnitOfWork }

func (p *plainUnitOfWork) Runs() app.RunRepository         { return p.inner.Runs() }
func (p *plainUnitOfWork) Tasks() app.TaskRepository       { return p.inner.Tasks() }
func (p *plainUnitOfWork) Attempts() app.AttemptRepository { return p.inner.Attempts() }

func (p *plainUnitOfWork) Sessions() app.SessionRepository { return p.inner.Sessions() }

func (p *plainUnitOfWork) Worktrees() app.WorktreeRepository { return p.inner.Worktrees() }

func (p *plainUnitOfWork) Results() app.ResultRepository { return p.inner.Results() }

func (p *plainUnitOfWork) Artifacts() app.ArtifactRepository { return p.inner.Artifacts() }

func (p *plainUnitOfWork) Bindings() app.BindingRepository { return p.inner.Bindings() }

func (p *plainUnitOfWork) LaunchClaims() app.LaunchClaimRepository { return p.inner.LaunchClaims() }

func (p *plainUnitOfWork) CheckExecClaims() app.CheckExecClaimRepository {
	return p.inner.CheckExecClaims()
}

func (p *plainUnitOfWork) Operations() app.OperationRepository { return p.inner.Operations() }

func (p *plainUnitOfWork) Transitions() app.TransitionRepository { return p.inner.Transitions() }

func (p *plainUnitOfWork) CheckRequests() app.CheckRequestRepository { return p.inner.CheckRequests() }
func (p *plainUnitOfWork) Commit() error                             { return p.inner.Commit() }
func (p *plainUnitOfWork) Rollback() error                           { return p.inner.Rollback() }

// plainStore wraps *fakeStore, overriding Begin to hand out a
// plainUnitOfWork instead of the fake's own *fakeUnitOfWork (which
// implements app.WorkflowRepositories).
type plainStore struct{ *fakeStore }

func (p *plainStore) Begin(ctx context.Context, lease app.Lease) (app.UnitOfWork, error) {
	uow, err := p.fakeStore.Begin(ctx, lease)
	if err != nil {
		return nil, err
	}
	inner, ok := uow.(*fakeUnitOfWork)
	if !ok {
		return nil, fmt.Errorf("app_test: unexpected UnitOfWork type %T", uow)
	}
	return &plainUnitOfWork{inner: inner}, nil
}

func TestRequireWorkflowRepositoriesFailsClosed(t *testing.T) {
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskPending)

	legacy := *tc.Controller
	legacy.Store = &plainStore{fakeStore: tc.Store}

	_, err := legacy.RecomputeReleases(context.Background(), fr.Handle)
	if err == nil {
		t.Fatalf("RecomputeReleases() over a store lacking WorkflowRepositories succeeded; want ErrWorkflowRepositoriesUnsupported")
	}
	if !errors.Is(err, app.ErrWorkflowRepositoriesUnsupported) {
		t.Fatalf("RecomputeReleases() error = %v, want ErrWorkflowRepositoriesUnsupported", err)
	}

	_, err = legacy.AssignReadyTasks(context.Background(), fr.Handle, defaultAssignmentOptions())
	if !errors.Is(err, app.ErrWorkflowRepositoriesUnsupported) {
		t.Fatalf("AssignReadyTasks() error = %v, want ErrWorkflowRepositoriesUnsupported", err)
	}

	// The refusal happens before any side effect, inside the same
	// transaction that would have read/written it: the task is untouched.
	if got := tc.Store.Tasks[taskID].value.State; got != run.TaskPending {
		t.Fatalf("task state = %s, want unchanged pending", got)
	}
}
