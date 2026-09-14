package sqlite_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// TestSaveRevisionConflict proves an update against a moved revision fails
// with ErrRevisionConflict and writes nothing, for every mutable entity.
func TestSaveRevisionConflict(t *testing.T) {
	cases := []struct {
		name string
		save func(t *testing.T, f *fixture, uow app.UnitOfWork, staleRevision int64) error
	}{
		{
			name: "run",
			save: func(t *testing.T, f *fixture, uow app.UnitOfWork, staleRevision int64) error {
				t.Helper()
				v, _, err := uow.Runs().Get(t.Context(), f.spec.RunID)
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				next, err := v.Launch(f.clock.Now())
				if err != nil {
					t.Fatalf("transition: %v", err)
				}
				_, err = uow.Runs().Save(t.Context(), next, staleRevision)
				return err
			},
		},
		{
			name: "task",
			save: func(t *testing.T, f *fixture, uow app.UnitOfWork, staleRevision int64) error {
				t.Helper()
				v, _, err := uow.Tasks().Get(t.Context(), f.spec.TaskID)
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				next, err := v.Activate(f.clock.Now())
				if err != nil {
					t.Fatalf("transition: %v", err)
				}
				_, err = uow.Tasks().Save(t.Context(), next, staleRevision)
				return err
			},
		},
		{
			name: "attempt",
			save: func(t *testing.T, f *fixture, uow app.UnitOfWork, staleRevision int64) error {
				t.Helper()
				v, _, err := uow.Attempts().Get(t.Context(), f.spec.AttemptID)
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				next, err := v.Launch(f.clock.Now())
				if err != nil {
					t.Fatalf("transition: %v", err)
				}
				_, err = uow.Attempts().Save(t.Context(), next, staleRevision)
				return err
			},
		},
		{
			name: "session",
			save: func(t *testing.T, f *fixture, uow app.UnitOfWork, staleRevision int64) error {
				t.Helper()
				v, _, err := uow.Sessions().Get(t.Context(), f.spec.SessionID)
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				next, err := v.Launch(f.clock.Now())
				if err != nil {
					t.Fatalf("transition: %v", err)
				}
				_, err = uow.Sessions().Save(t.Context(), next, staleRevision)
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			uow, err := f.store.Begin(t.Context(), f.lease)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() {
				if rbErr := uow.Rollback(); rbErr != nil {
					t.Errorf("rollback: %v", rbErr)
				}
			}()

			err = tc.save(t, f, uow, 41)

			if !errors.Is(err, app.ErrRevisionConflict) {
				t.Fatalf("save at a stale revision = %v, want ErrRevisionConflict", err)
			}
		})
	}
}

// TestSaveAdvancesRevision proves a correct save advances the revision by
// one and a repeat of the original expectedRevision then conflicts.
func TestSaveAdvancesRevision(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Now()
	f.inUOW(t, func(uow app.UnitOfWork) {
		v, revision, err := uow.Runs().Get(t.Context(), f.spec.RunID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		next, err := v.Launch(now)
		if err != nil {
			t.Fatalf("transition: %v", err)
		}
		saved, err := uow.Runs().Save(t.Context(), next, revision)
		if err != nil {
			t.Fatalf("first save: %v", err)
		}
		if saved != revision+1 {
			t.Fatalf("revision after save = %d, want %d", saved, revision+1)
		}
		if _, err := uow.Runs().Save(t.Context(), next, revision); !errors.Is(err, app.ErrRevisionConflict) {
			t.Fatalf("repeat save at the consumed revision = %v, want ErrRevisionConflict", err)
		}
	})
}

// TestAttemptReservationRacedAcrossConnections proves the partial unique
// index admits exactly one active attempt per task when two handles race
// the reservation insert.
func TestAttemptReservationRacedAcrossConnections(t *testing.T) {
	clock := newFakeClock()
	root := t.TempDir()
	storeA := openStoreAt(t, root, clock)
	storeB := openStoreAt(t, root, clock)
	spec := newSpec("/repos/alpha", specStride, clock.Now())
	if _, _, err := storeA.InitializeRun(t.Context(), spec); err != nil {
		t.Fatalf("InitializeRun: %v", err)
	}
	// InitializeRun already reserved attempt 1 (active). Race two more
	// reservations, numbers 2 and 3, from separate handles: the partial
	// unique index on the active set must admit neither while attempt 1 is
	// active, or — after attempt 1 is terminal — exactly one.
	if _, err := sqlite.WriteDB(storeA).ExecContext(t.Context(),
		`UPDATE attempts SET state = 'interrupted' WHERE id = ?`, spec.AttemptID.String(),
	); err != nil {
		t.Fatalf("retire attempt 1: %v", err)
	}
	insert := func(store *sqlite.Store, id string, number int) error {
		_, err := sqlite.WriteDB(store).ExecContext(t.Context(),
			`INSERT INTO attempts (id, task_id, number, state, revision, updated_at)
			 VALUES (?, ?, ?, 'reserved', 1, '2026-09-14T10:00:00.000000000Z')`,
			id, spec.TaskID.String(), number,
		)
		return err
	}
	var (
		start sync.WaitGroup
		done  sync.WaitGroup
	)
	start.Add(1)
	errs := make([]error, 2)
	done.Add(2)
	go func() {
		defer done.Done()
		start.Wait()
		errs[0] = insert(storeA, uid(9101), 2)
	}()
	go func() {
		defer done.Done()
		start.Wait()
		errs[1] = insert(storeB, uid(9102), 3)
	}()
	start.Done()
	done.Wait()

	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("raced reservations succeeded %d times, want exactly 1 (errors: %v)", succeeded, errs)
	}
	active := countRows(t, storeA,
		`SELECT COUNT(*) FROM attempts WHERE task_id = ? AND state NOT IN ('completed', 'failed', 'interrupted')`,
		spec.TaskID.String(),
	)
	if active != 1 {
		t.Fatalf("active attempts after the race = %d, want 1", active)
	}
}

// TestInitializeRunRacedOnSameRootPath proves repository get-or-create
// resolves through the UNIQUE(root_path) constraint: two handles racing
// InitializeRun for the same repository both succeed against one
// repository row with distinct sequence numbers.
func TestInitializeRunRacedOnSameRootPath(t *testing.T) {
	clock := newFakeClock()
	root := t.TempDir()
	storeA := openStoreAt(t, root, clock)
	storeB := openStoreAt(t, root, clock)
	specA := newSpec("/repos/alpha", 1*specStride, clock.Now())
	specB := newSpec("/repos/alpha", 2*specStride, clock.Now())
	var (
		start sync.WaitGroup
		done  sync.WaitGroup
	)
	start.Add(1)
	errs := make([]error, 2)
	done.Add(2)
	go func() {
		defer done.Done()
		start.Wait()
		_, _, errs[0] = storeA.InitializeRun(t.Context(), specA)
	}()
	go func() {
		defer done.Done()
		start.Wait()
		_, _, errs[1] = storeB.InitializeRun(t.Context(), specB)
	}()
	start.Done()
	done.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("raced InitializeRun %d: %v", i, err)
		}
	}
	repositories := countRows(t, storeA, `SELECT COUNT(*) FROM repositories`)
	if repositories != 1 {
		t.Fatalf("repository rows after the race = %d, want 1", repositories)
	}
	sequences := countRows(t, storeA, `SELECT COUNT(DISTINCT seq) FROM runs`)
	if sequences != 2 {
		t.Fatalf("distinct run sequences after the race = %d, want 2", sequences)
	}
}

// TestConstraintCoverage proves the design's unique constraints reject the
// second conflicting row, each through raw SQL so the constraint itself is
// what rejects.
func TestConstraintCoverage(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *fixture)
		query string
		args  func(f *fixture) []any
	}{
		{
			name:  "one accepted result per attempt",
			setup: acceptOneResult,
			query: `INSERT INTO results (id, attempt_id, commit_oid, summary, content_digest, accepted, submitted_at) VALUES (?, ?, 'b', 's', 'other-digest', 1, '2026-09-14T10:00:00.000000000Z')`,
			args:  func(f *fixture) []any { return []any{uid(9301), f.spec.AttemptID.String()} },
		},
		{
			name:  "unique attempt and content digest",
			setup: acceptOneResult,
			query: `INSERT INTO results (id, attempt_id, commit_oid, summary, content_digest, accepted, submitted_at) VALUES (?, ?, 'b', 's', 'digest-1', 0, '2026-09-14T10:00:00.000000000Z')`,
			args:  func(f *fixture) []any { return []any{uid(9302), f.spec.AttemptID.String()} },
		},
		{
			name:  "unique check request per result",
			setup: acceptOneResult,
			query: `INSERT INTO check_requests (result_id, attempt_id, state, created_at, claimed_generation) VALUES (?, ?, 'requested', '2026-09-14T10:00:00.000000000Z', NULL)`,
			args:  func(f *fixture) []any { return []any{uid(8001), f.spec.AttemptID.String()} },
		},
		{
			name:  "unique worktree path",
			setup: createWorktree,
			query: `INSERT INTO worktrees (id, repository_id, run_id, path, branch, state, revision, created_at) SELECT ?, repository_id, id, '/worktrees/alpha-r1', 'other-branch', 'active', 1, '2026-09-14T10:00:00.000000000Z' FROM runs WHERE id = ?`,
			args:  func(f *fixture) []any { return []any{uid(9303), f.spec.RunID.String()} },
		},
		{
			name:  "unique run sequence per repository",
			setup: func(*testing.T, *fixture) {},
			query: `INSERT INTO runs (id, repository_id, seq, brief, brief_digest, state, stop_requested_at, revision, created_at, updated_at) SELECT ?, repository_id, seq, 'b', 'd', 'created', NULL, 1, created_at, updated_at FROM runs WHERE id = ?`,
			args:  func(f *fixture) []any { return []any{uid(9304), f.spec.RunID.String()} },
		},
		{
			name:  "unique attempt number per task",
			setup: func(*testing.T, *fixture) {},
			query: `INSERT INTO attempts (id, task_id, number, state, revision, updated_at) VALUES (?, ?, 1, 'interrupted', 1, '2026-09-14T10:00:00.000000000Z')`,
			args:  func(f *fixture) []any { return []any{uid(9305), f.spec.TaskID.String()} },
		},
		{
			name:  "one current session per attempt",
			setup: func(*testing.T, *fixture) {},
			query: `INSERT INTO sessions (id, run_id, attempt_id, role, harness, native_session_ref, native_ref_source, state, revision, updated_at) VALUES (?, ?, ?, 'worker', 'claude', NULL, NULL, 'reserved', 1, '2026-09-14T10:00:00.000000000Z')`,
			args: func(f *fixture) []any {
				return []any{uid(9306), f.spec.RunID.String(), f.spec.AttemptID.String()}
			},
		},
		{
			name: "unique binding per session and incarnation",
			setup: func(t *testing.T, f *fixture) {
				t.Helper()
				f.createBinding(t)
			},
			query: `INSERT INTO runtime_bindings (id, session_id, incarnation_id, server_socket_path, server_instance, workspace_id, tab_id, pane_id, creation_label, launch_kind, occupant_evidence, observed_at, superseded, superseded_at, superseded_evidence) VALUES (?, ?, ?, '/s', NULL, 'w', 't', 'p', 'l', 'initial', NULL, '2026-09-14T10:00:00.000000000Z', 0, NULL, NULL)`,
			args: func(f *fixture) []any {
				return []any{uid(9307), f.spec.SessionID.String(), f.spec.IncarnationID.String()}
			},
		},
		{
			name:  "unique repository root path",
			setup: func(*testing.T, *fixture) {},
			query: `INSERT INTO repositories (id, root_path, created_at) VALUES (?, '/repos/alpha', '2026-09-14T10:00:00.000000000Z')`,
			args:  func(*fixture) []any { return []any{uid(9308)} },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.setup(t, f)

			_, err := sqlite.WriteDB(f.store).ExecContext(t.Context(), tc.query, tc.args(f)...)

			if err == nil {
				t.Fatal("the conflicting insert was accepted; the constraint is missing")
			}
		})
	}
}

// acceptOneResult drives the fixture to running and accepts one result
// with digest-1, so results and check_requests rows exist.
func acceptOneResult(t *testing.T, f *fixture) {
	t.Helper()
	f.launchAttempt(t)
	f.createBinding(t)
	f.claimLaunch(t)
	f.settleClaimExeced(t)
	f.markRunning(t)
	outcome, err := f.store.SubmitResult(t.Context(), f.submission(8001, "digest-1"))
	if err != nil {
		t.Fatalf("SubmitResult: %v", err)
	}
	if outcome.Kind != app.SubmissionAccepted {
		t.Fatalf("submission outcome = %s, want accepted", outcome.Kind)
	}
}

// createWorktree records the fixture worktree through the port.
func createWorktree(t *testing.T, f *fixture) {
	t.Helper()
	f.inUOW(t, func(uow app.UnitOfWork) {
		runV, _, err := uow.Runs().Get(t.Context(), f.spec.RunID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		worktree := run.NewWorktree(f.spec.WorktreeID, runV.RepositoryID, f.spec.RunID, "/worktrees/alpha-r1", "hop/r1")
		if _, err := uow.Worktrees().Create(t.Context(), worktree); err != nil {
			t.Fatalf("create worktree: %v", err)
		}
	})
}

// TestReopenMidOperationReadsBackPendingWork proves a store reopened after
// a crash mid-operation still serves the pending journal rows and the
// unsettled launch claim, through the ports.
func TestReopenMidOperationReadsBackPendingWork(t *testing.T) {
	clock := newFakeClock()
	root := t.TempDir()
	store := openStoreAt(t, root, clock)
	spec := newSpec("/repos/alpha", specStride, clock.Now())
	_, lease, err := store.InitializeRun(t.Context(), spec)
	if err != nil {
		t.Fatalf("InitializeRun: %v", err)
	}
	f := &fixture{store: store, clock: clock, spec: spec, lease: lease}
	f.launchAttempt(t)
	f.createBinding(t)
	opID := identity.OperationID(uid(7001))
	f.inUOW(t, func(uow app.UnitOfWork) {
		createErr := uow.Operations().Create(t.Context(), app.Operation{
			ID:         opID,
			RunID:      spec.RunID,
			Generation: lease.Generation,
			Kind:       app.OpPaneOpen,
			State:      app.OperationPending,
			Intent:     map[string]any{"creation_label": uid(9001)},
			CreatedAt:  clock.Now(),
			UpdatedAt:  clock.Now(),
		})
		if createErr != nil {
			t.Fatalf("create operation: %v", createErr)
		}
	})
	f.claimLaunch(t)

	// The controller "crashes": its handle closes with the operation
	// pending and the claim unsettled. A new handle reopens the store.
	reopened := openStoreAt(t, root, clock)
	takeover, err := reopened.AcquireLease(t.Context(), spec.RunID, "controller-b")
	if errors.Is(err, app.ErrLeaseHeld) {
		clock.Advance(leaseTTL + time.Second)
		takeover, err = reopened.AcquireLease(t.Context(), spec.RunID, "controller-b")
	}
	if err != nil {
		t.Fatalf("takeover on the reopened store: %v", err)
	}
	uow, err := reopened.Begin(t.Context(), takeover)
	if err != nil {
		t.Fatalf("begin on the reopened store: %v", err)
	}
	defer func() {
		if rbErr := uow.Rollback(); rbErr != nil {
			t.Errorf("rollback: %v", rbErr)
		}
	}()

	pending, err := uow.Operations().Pending(t.Context(), spec.RunID)
	if err != nil {
		t.Fatalf("pending operations: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != opID || pending[0].State != app.OperationPending {
		t.Fatalf("pending operations = %+v, want the one pending pane.open %s", pending, opID)
	}
	intent, ok := pending[0].Intent.(map[string]any)
	if !ok || intent["creation_label"] != uid(9001) {
		t.Fatalf("pending intent = %#v, want the recorded creation label", pending[0].Intent)
	}
	claim, ok, err := uow.LaunchClaims().Get(t.Context(), spec.IncarnationID)
	if err != nil {
		t.Fatalf("get launch claim: %v", err)
	}
	if !ok || claim.State != app.LaunchClaimExecPending || claim.PID != 111 {
		t.Fatalf("reopened claim = %+v (found %t), want exec_pending pid 111", claim, ok)
	}
	pendingClaims, err := uow.LaunchClaims().Pending(t.Context(), spec.RunID)
	if err != nil {
		t.Fatalf("pending launch claims: %v", err)
	}
	if len(pendingClaims) != 1 || pendingClaims[0].IncarnationID != spec.IncarnationID {
		t.Fatalf("pending claims = %+v, want the one exec_pending claim", pendingClaims)
	}
}

// TestOperationSaveAndCheckExecClaimReadback proves the journal update path
// and the read-only check-exec claim repository.
func TestOperationSaveAndCheckExecClaimReadback(t *testing.T) {
	f := newFixture(t)
	opID := identity.OperationID(uid(7002))
	f.inUOW(t, func(uow app.UnitOfWork) {
		err := uow.Operations().Create(t.Context(), app.Operation{
			ID:         opID,
			RunID:      f.spec.RunID,
			Generation: f.lease.Generation,
			Kind:       app.OpCheckRun,
			State:      app.OperationPending,
			Intent:     map[string]any{"tree_oid": "abc"},
			CreatedAt:  f.clock.Now(),
			UpdatedAt:  f.clock.Now(),
		})
		if err != nil {
			t.Fatalf("create operation: %v", err)
		}
	})
	if err := f.store.ClaimCheckExec(t.Context(), opID, 4242); err != nil {
		t.Fatalf("ClaimCheckExec: %v", err)
	}

	f.inUOW(t, func(uow app.UnitOfWork) {
		claim, ok, err := uow.CheckExecClaims().Get(t.Context(), opID)
		if err != nil {
			t.Fatalf("get check-exec claim: %v", err)
		}
		if !ok || claim.PID != 4242 || claim.OperationID != opID {
			t.Fatalf("check-exec claim = %+v (found %t), want pid 4242", claim, ok)
		}
		op, err := uow.Operations().Get(t.Context(), opID)
		if err != nil {
			t.Fatalf("get operation: %v", err)
		}
		op.State = app.OperationFailed
		op.Outcome = map[string]any{"outcome": "unknown"}
		op.UpdatedAt = f.clock.Now()
		if err := uow.Operations().Save(t.Context(), op); err != nil {
			t.Fatalf("save operation: %v", err)
		}
	})
	f.inUOW(t, func(uow app.UnitOfWork) {
		op, err := uow.Operations().Get(t.Context(), opID)
		if err != nil {
			t.Fatalf("reload operation: %v", err)
		}
		if op.State != app.OperationFailed {
			t.Fatalf("operation state after save = %s, want failed", op.State)
		}
		outcome, ok := op.Outcome.(map[string]any)
		if !ok || outcome["outcome"] != "unknown" {
			t.Fatalf("operation outcome after save = %#v, want the recorded unknown outcome", op.Outcome)
		}
	})
}
