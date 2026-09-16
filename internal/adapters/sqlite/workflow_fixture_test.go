package sqlite_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// featureWorkflow is the frozen feature-mode policy every feature fixture
// carries.
func featureWorkflow() app.WorkflowSnapshot {
	return app.WorkflowSnapshot{
		Mode: "feature", MaxWorkers: 2, RetryLimit: 3,
		ManagerRolePath: "/state/roles/manager.md", ManagerRoleDigest: "manager-digest",
		ImplementerRolePath: "/state/roles/implementer.md", ImplementerRoleDigest: "implementer-digest",
		ReviewerRolePath: "/state/roles/reviewer.md", ReviewerRoleDigest: "reviewer-digest",
		ReviewerHarness: "claude", MessageAttention: 2 * time.Minute, MessageWait: 50 * time.Second,
		IntegrationBranch: "hop/r1/integration",
	}
}

// featureFixture is one feature-mode run: the fixture's bootstrap task,
// attempt and worker session (re-labeled the run's t1 implementer by the
// helpers below) plus a current, bound manager session.
type featureFixture struct {
	*fixture
	ManagerID          identity.SessionID
	ManagerIncarnation identity.IncarnationID
}

// Identity offsets for feature-fixture entities, distinct from every
// newSpec block.
const (
	fxBase        = 7000
	offManager    = fxBase + 1
	offManagerInc = fxBase + 2
)

// newFeatureFixture initializes a feature-mode run at a fresh store: the
// frozen workflow snapshot, the run driven to running, and a current
// manager session with its binding — the caller identity every manager
// verb validates against.
func newFeatureFixture(t *testing.T) *featureFixture {
	t.Helper()
	clock := newFakeClock()
	store := openStoreAt(t, t.TempDir(), clock)
	spec := newSpec("/repos/feature", specStride, clock.Now())
	spec.Snapshot.Workflow = featureWorkflow()
	_, lease, err := store.InitializeRun(t.Context(), spec)
	if err != nil {
		t.Fatalf("InitializeRun: %v", err)
	}
	f := &featureFixture{
		fixture:            &fixture{store: store, clock: clock, spec: spec, lease: lease},
		ManagerID:          identity.SessionID(uid(offManager)),
		ManagerIncarnation: identity.IncarnationID(uid(offManagerInc)),
	}
	seedFeatureRunState(t, f)
	return f
}

// managerSessionValue loads the fixture's manager session row.
func (f *featureFixture) managerSessionValue(t *testing.T) run.Session {
	t.Helper()
	var manager run.Session
	f.inUOW(t, func(uow app.UnitOfWork) {
		v, _, err := uow.Sessions().Get(t.Context(), f.ManagerID)
		if err != nil {
			t.Fatalf("get manager session: %v", err)
		}
		manager = v
	})
	return manager
}

// createFeatureTask creates one implement task through the controller
// index (bypassing PlanStore, whose own path is tested separately), ready
// to bind workers to.
func (f *featureFixture) createFeatureTask(t *testing.T, n, seq int, state run.TaskState) identity.TaskID {
	t.Helper()
	taskID := identity.TaskID(uid(n))
	now := f.clock.Now()
	f.inUOW(t, func(uow app.UnitOfWork) {
		wf, err := app.RequireWorkflowRepositories(uow, "test fixture")
		if err != nil {
			t.Fatalf("RequireWorkflowRepositories: %v", err)
		}
		task := run.NewImplementTask(taskID, f.spec.RunID, seq, "task "+uid(n), "instructions-digest", false, now)
		task.State = state
		if _, err := wf.TaskIndex().Create(t.Context(), task); err != nil {
			t.Fatalf("create feature task: %v", err)
		}
	})
	return taskID
}

// createWorkerSession reserves an attempt on task and creates an active,
// bound child session of the fixture manager with the given role,
// returning its identities.
func (f *featureFixture) createWorkerSession(t *testing.T, task identity.TaskID, role run.Role, n int) (identity.SessionID, identity.IncarnationID) {
	t.Helper()
	attemptID := identity.AttemptID(uid(n))
	sessionID := identity.SessionID(uid(n + 1))
	incarnationID := identity.IncarnationID(uid(n + 2))
	now := f.clock.Now()
	manager := f.managerSessionValue(t)
	f.inUOW(t, func(uow app.UnitOfWork) {
		wf, err := app.RequireWorkflowRepositories(uow, "test fixture")
		if err != nil {
			t.Fatalf("RequireWorkflowRepositories: %v", err)
		}
		attempts, err := wf.AttemptIndex().ByTask(t.Context(), task)
		if err != nil {
			t.Fatalf("list attempts: %v", err)
		}
		attempt, attemptErr := run.NewAttempt(attemptID, task, len(attempts)+1, now)
		if attemptErr != nil {
			t.Fatalf("new attempt: %v", attemptErr)
		}
		if _, err := wf.AttemptIndex().Create(t.Context(), attempt); err != nil {
			t.Fatalf("create attempt: %v", err)
		}
		session, sessionErr := run.NewChildSession(sessionID, f.spec.RunID, attemptID, role, manager, run.HarnessClaude, now)
		if sessionErr != nil {
			t.Fatalf("new child session: %v", sessionErr)
		}
		if session, sessionErr = session.Launch(now); sessionErr != nil {
			t.Fatalf("launch child session: %v", sessionErr)
		}
		if session, sessionErr = session.ConfirmActive(now); sessionErr != nil {
			t.Fatalf("activate child session: %v", sessionErr)
		}
		if _, err := uow.Sessions().Create(t.Context(), session); err != nil {
			t.Fatalf("create child session: %v", err)
		}
		binding := run.NewRuntimeBinding(
			sessionID, incarnationID,
			"/tmp/herdr.sock", "server-instance-1", "workspace-w", "tab-w", "pane-"+uid(n),
			uid(n+3), run.LaunchInitial, now,
		)
		if err := uow.Bindings().Create(t.Context(), binding); err != nil {
			t.Fatalf("create child binding: %v", err)
		}
	})
	return sessionID, incarnationID
}

// repositoryID reads the fixture run's repository identity.
func (f *featureFixture) repositoryID(t *testing.T) identity.RepositoryID {
	t.Helper()
	var repositoryID identity.RepositoryID
	f.inUOW(t, func(uow app.UnitOfWork) {
		runV, _, err := uow.Runs().Get(t.Context(), f.spec.RunID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		repositoryID = runV.RepositoryID
	})
	return repositoryID
}

// createWorktree records w through the controller's worktree repository —
// the production writer.
func (f *featureFixture) createWorktree(t *testing.T, w run.Worktree) { //nolint:gocritic // hugeParam: the port passes domain values by value; the helper mirrors it.
	t.Helper()
	f.inUOW(t, func(uow app.UnitOfWork) {
		if _, err := uow.Worktrees().Create(t.Context(), w); err != nil {
			t.Fatalf("create worktree %s: %v", w.Path, err)
		}
	})
}

// createAttemptWorktree records attemptID's worktree in the shape
// AssignReadyTasks writes: linked to the attempt and its base commit.
func (f *featureFixture) createAttemptWorktree(t *testing.T, n int, attemptID identity.AttemptID, path, branch string) {
	t.Helper()
	w, err := run.NewAttemptWorktree(identity.WorktreeID(uid(n)), f.repositoryID(t), f.spec.RunID, attemptID, "base-oid", path, branch)
	if err != nil {
		t.Fatalf("NewAttemptWorktree: %v", err)
	}
	f.createWorktree(t, w)
}

// workflowRepos asserts the unit of work's WorkflowRepositories capability.
func workflowRepos(t *testing.T, uow app.UnitOfWork) app.WorkflowRepositories {
	t.Helper()
	wf, err := app.RequireWorkflowRepositories(uow, "test")
	if err != nil {
		t.Fatalf("RequireWorkflowRepositories: %v", err)
	}
	return wf
}

// rawExec runs one raw SQL statement against the store's write pool.
func rawExec(t *testing.T, store *sqlite.Store, query string, args ...any) {
	t.Helper()
	if _, err := sqlite.WriteDB(store).ExecContext(t.Context(), query, args...); err != nil {
		t.Fatalf("raw exec: %v\nquery: %.120s", err, query)
	}
}

// writeDBRow queries one row through the store's write pool.
func writeDBRow(t *testing.T, f *featureFixture, query string, args ...any) *sql.Row {
	t.Helper()
	return sqlite.WriteDB(f.store).QueryRowContext(t.Context(), query, args...)
}
