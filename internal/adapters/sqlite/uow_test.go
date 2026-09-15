package sqlite_test

import (
	"database/sql"
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
		name  string
		setup func(t *testing.T, f *fixture)
		save  func(t *testing.T, f *fixture, uow app.UnitOfWork, staleRevision int64) error
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
		{
			name:  "worktree",
			setup: createWorktree,
			save: func(t *testing.T, f *fixture, uow app.UnitOfWork, staleRevision int64) error {
				t.Helper()
				v, _, err := uow.Worktrees().ByRun(t.Context(), f.spec.RunID)
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				_, err = uow.Worktrees().Save(t.Context(), v, staleRevision)
				return err
			},
		},
		{
			name: "missing task id",
			save: func(t *testing.T, f *fixture, uow app.UnitOfWork, staleRevision int64) error {
				t.Helper()
				missing := run.Task{ID: identity.TaskID(uid(6401)), RunID: f.spec.RunID, State: run.TaskPending, UpdatedAt: f.clock.Now()}
				_, err := uow.Tasks().Save(t.Context(), missing, staleRevision)
				return err
			},
		},
		{
			name: "missing attempt id",
			save: func(t *testing.T, f *fixture, uow app.UnitOfWork, staleRevision int64) error {
				t.Helper()
				missing := run.Attempt{ID: identity.AttemptID(uid(6402)), TaskID: f.spec.TaskID, Number: 1, State: run.AttemptReserved, UpdatedAt: f.clock.Now()}
				_, err := uow.Attempts().Save(t.Context(), missing, staleRevision)
				return err
			},
		},
		{
			name: "missing session id",
			save: func(t *testing.T, f *fixture, uow app.UnitOfWork, staleRevision int64) error {
				t.Helper()
				missing := run.NewSession(identity.SessionID(uid(6403)), f.spec.RunID, f.spec.AttemptID, run.HarnessClaude, f.clock.Now())
				_, err := uow.Sessions().Save(t.Context(), missing, staleRevision)
				return err
			},
		},
		{
			name: "missing worktree id",
			save: func(t *testing.T, f *fixture, uow app.UnitOfWork, staleRevision int64) error {
				t.Helper()
				runV, _, err := uow.Runs().Get(t.Context(), f.spec.RunID)
				if err != nil {
					t.Fatalf("get run: %v", err)
				}
				missing := run.NewWorktree(identity.WorktreeID(uid(6404)), runV.RepositoryID, f.spec.RunID, "/worktrees/missing", "hop/missing")
				_, err = uow.Worktrees().Save(t.Context(), missing, staleRevision)
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.setup != nil {
				tc.setup(t, f)
			}
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

// TestUnitOfWorkIsScopedToTheLeasedRun proves a unit of work is one run's
// exclusive write authority: under run A's valid lease, every repository
// mutation against run B or B's descendants is refused with ErrFenced
// while B has its own live holder, and only a lease FOR B (here taken over
// after expiry) can mutate B.
func TestUnitOfWorkIsScopedToTheLeasedRun(t *testing.T) {
	clock := newFakeClock()
	store := openStoreAt(t, t.TempDir(), clock)
	specA := newSpec("/repos/alpha", 1*specStride, clock.Now())
	specB := newSpec("/repos/beta", 2*specStride, clock.Now())
	_, leaseA, err := store.InitializeRun(t.Context(), specA)
	if err != nil {
		t.Fatalf("InitializeRun A: %v", err)
	}
	_, leaseB, err := store.InitializeRun(t.Context(), specB)
	if err != nil {
		t.Fatalf("InitializeRun B: %v", err)
	}
	// Drive B to an accepted result under its own lease, so a binding, a
	// claim, a result and a check request all exist to aim A's lease at.
	fB := &fixture{store: store, clock: clock, spec: specB, lease: leaseB}
	fB.launchAttempt(t)
	fB.createBinding(t)
	fB.claimLaunch(t)
	fB.settleClaimExeced(t)
	fB.markRunning(t)
	accepted, err := store.SubmitResult(t.Context(), fB.submission(8801, "digest-b"))
	if err != nil || accepted.Kind != app.SubmissionAccepted {
		t.Fatalf("SubmitResult on B = %+v, %v", accepted, err)
	}
	if err := store.Heartbeat(t.Context(), leaseB); err != nil {
		t.Fatalf("heartbeat B's live holder: %v", err)
	}

	now := clock.Now()
	cases := []struct {
		name   string
		mutate func(t *testing.T, uow app.UnitOfWork) error
	}{
		{
			name: "run save",
			mutate: func(t *testing.T, uow app.UnitOfWork) error {
				t.Helper()
				v, revision, err := uow.Runs().Get(t.Context(), specB.RunID)
				if err != nil {
					t.Fatalf("get B's run: %v", err)
				}
				next, err := v.EnterCompleting(now)
				if err != nil {
					t.Fatalf("transition: %v", err)
				}
				_, err = uow.Runs().Save(t.Context(), next, revision)
				return err
			},
		},
		{
			name: "task save",
			mutate: func(t *testing.T, uow app.UnitOfWork) error {
				t.Helper()
				v, revision, err := uow.Tasks().Get(t.Context(), specB.TaskID)
				if err != nil {
					t.Fatalf("get B's task: %v", err)
				}
				_, err = uow.Tasks().Save(t.Context(), v, revision)
				return err
			},
		},
		{
			name: "attempt save",
			mutate: func(t *testing.T, uow app.UnitOfWork) error {
				t.Helper()
				v, revision, err := uow.Attempts().Get(t.Context(), specB.AttemptID)
				if err != nil {
					t.Fatalf("get B's attempt: %v", err)
				}
				_, err = uow.Attempts().Save(t.Context(), v, revision)
				return err
			},
		},
		{
			name: "session save",
			mutate: func(t *testing.T, uow app.UnitOfWork) error {
				t.Helper()
				v, revision, err := uow.Sessions().Get(t.Context(), specB.SessionID)
				if err != nil {
					t.Fatalf("get B's session: %v", err)
				}
				_, err = uow.Sessions().Save(t.Context(), v, revision)
				return err
			},
		},
		{
			name: "session create",
			mutate: func(t *testing.T, uow app.UnitOfWork) error {
				t.Helper()
				_, err := uow.Sessions().Create(t.Context(), run.NewSession(identity.SessionID(uid(8850)), specB.RunID, specB.AttemptID, run.HarnessClaude, now))
				return err
			},
		},
		{
			name: "worktree create",
			mutate: func(t *testing.T, uow app.UnitOfWork) error {
				t.Helper()
				runB, _, err := uow.Runs().Get(t.Context(), specB.RunID)
				if err != nil {
					t.Fatalf("get B's run: %v", err)
				}
				_, err = uow.Worktrees().Create(t.Context(), run.NewWorktree(specB.WorktreeID, runB.RepositoryID, specB.RunID, "/worktrees/beta-r1", "hop/beta"))
				return err
			},
		},
		{
			name: "artifact save",
			mutate: func(t *testing.T, uow app.UnitOfWork) error {
				t.Helper()
				return uow.Artifacts().Save(t.Context(), run.NewArtifact(identity.ArtifactID(uid(8851)), specB.RunID, run.ArtifactPaneSnapshot, "/p", "d"))
			},
		},
		{
			name: "binding create",
			mutate: func(t *testing.T, uow app.UnitOfWork) error {
				t.Helper()
				return uow.Bindings().Create(t.Context(), run.NewRuntimeBinding(specB.SessionID, identity.IncarnationID(uid(8852)), "/s", "si", "w", "t", "p", "l", run.LaunchResume, now))
			},
		},
		{
			name: "binding save",
			mutate: func(t *testing.T, uow app.UnitOfWork) error {
				t.Helper()
				binding, ok, err := uow.Bindings().Current(t.Context(), specB.SessionID)
				if err != nil || !ok {
					t.Fatalf("current B binding: %v (found %t)", err, ok)
				}
				superseded, err := binding.Supersede("cross-run attempt", now)
				if err != nil {
					t.Fatalf("supersede: %v", err)
				}
				return uow.Bindings().Save(t.Context(), superseded)
			},
		},
		{
			name: "launch claim settle",
			mutate: func(t *testing.T, uow app.UnitOfWork) error {
				t.Helper()
				return uow.LaunchClaims().Settle(t.Context(), specB.IncarnationID, app.LaunchClaimSettlement{State: app.LaunchClaimExecFailed, Reason: "cross-run attempt", At: now})
			},
		},
		{
			name: "operation create",
			mutate: func(t *testing.T, uow app.UnitOfWork) error {
				t.Helper()
				return uow.Operations().Create(t.Context(), app.Operation{
					ID: identity.OperationID(uid(8853)), RunID: specB.RunID, Generation: leaseA.Generation,
					Kind: app.OpCheckRun, State: app.OperationPending,
					Intent: map[string]any{}, CreatedAt: now, UpdatedAt: now,
				})
			},
		},
		{
			name: "transition record",
			mutate: func(t *testing.T, uow app.UnitOfWork) error {
				t.Helper()
				return uow.Transitions().Record(t.Context(), app.Transition{
					EntityKind: app.EntityRun, EntityID: specB.RunID.String(),
					From: "running", To: "failed", Reason: "cross-run attempt", At: now,
				})
			},
		},
		{
			name: "check request save",
			mutate: func(t *testing.T, uow app.UnitOfWork) error {
				t.Helper()
				request, err := uow.CheckRequests().Get(t.Context(), accepted.ResultID)
				if err != nil {
					t.Fatalf("get B's check request: %v", err)
				}
				request.State = app.CheckRequestClaimed
				return uow.CheckRequests().Save(t.Context(), request)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uow, err := store.Begin(t.Context(), leaseA)
			if err != nil {
				t.Fatalf("begin under A's lease: %v", err)
			}
			defer func() {
				if rbErr := uow.Rollback(); rbErr != nil {
					t.Errorf("rollback: %v", rbErr)
				}
			}()

			err = tc.mutate(t, uow)

			if !errors.Is(err, app.ErrFenced) {
				t.Fatalf("cross-run %s under A's lease = %v, want ErrFenced", tc.name, err)
			}
		})
	}

	t.Run("takeover of B grants B", func(t *testing.T) {
		clock.Advance(leaseTTL + time.Second)
		takeover, err := store.AcquireLease(t.Context(), specB.RunID, "controller-a")
		if err != nil {
			t.Fatalf("take over B's lease: %v", err)
		}
		uow, err := store.Begin(t.Context(), takeover)
		if err != nil {
			t.Fatalf("begin under the takeover lease: %v", err)
		}
		v, revision, err := uow.Runs().Get(t.Context(), specB.RunID)
		if err != nil {
			t.Fatalf("get B's run: %v", err)
		}
		next, err := v.EnterCompleting(clock.Now())
		if err != nil {
			t.Fatalf("transition: %v", err)
		}
		if _, err := uow.Runs().Save(t.Context(), next, revision); err != nil {
			t.Fatalf("save B under B's own takeover lease: %v", err)
		}
		if err := uow.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	})
}

// TestBindingServerInstanceRoundTrip proves the reserved server_instance
// column maps to run.RuntimeBinding.ServerInstance both ways: a non-empty
// value stores non-NULL and reads back equal, and an empty value stores
// NULL and reads back "".
func TestBindingServerInstanceRoundTrip(t *testing.T) {
	cases := []struct {
		name           string
		serverInstance string
		wantNull       bool
	}{
		{name: "present", serverInstance: "srv-7f3a", wantNull: false},
		{name: "absent", serverInstance: "", wantNull: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.launchAttempt(t)
			f.inUOW(t, func(uow app.UnitOfWork) {
				binding := run.NewRuntimeBinding(
					f.spec.SessionID, f.spec.IncarnationID,
					"/tmp/herdr.sock", tc.serverInstance, "workspace-1", "tab-1", "pane-1",
					uid(9001), run.LaunchInitial, f.clock.Now(),
				)
				if err := uow.Bindings().Create(t.Context(), binding); err != nil {
					t.Fatalf("create binding: %v", err)
				}
			})

			f.inUOW(t, func(uow app.UnitOfWork) {
				binding, ok, err := uow.Bindings().Current(t.Context(), f.spec.SessionID)
				if err != nil || !ok {
					t.Fatalf("current binding: %v (found %t)", err, ok)
				}
				if binding.ServerInstance != tc.serverInstance {
					t.Fatalf("server instance = %q, want %q", binding.ServerInstance, tc.serverInstance)
				}
			})

			var stored sql.NullString
			if err := sqlite.WriteDB(f.store).QueryRowContext(t.Context(),
				`SELECT server_instance FROM runtime_bindings WHERE session_id = ?`, f.spec.SessionID.String(),
			).Scan(&stored); err != nil {
				t.Fatalf("read server_instance column: %v", err)
			}
			if stored.Valid == tc.wantNull {
				t.Fatalf("server_instance column valid=%t, want NULL=%t", stored.Valid, tc.wantNull)
			}
		})
	}
}

// TestOperationsByKind proves ByKind returns every operation of one kind
// for a run whatever its state, newest first, and excludes other kinds.
func TestOperationsByKind(t *testing.T) {
	f := newFixture(t)
	// Three check.run operations in distinct states, plus a pane.open that
	// ByKind(check.run) must exclude.
	specs := []struct {
		n     int
		kind  app.OperationKind
		state app.OperationState
	}{
		{n: 7601, kind: app.OpCheckRun, state: app.OperationSucceeded},
		{n: 7602, kind: app.OpCheckRun, state: app.OperationFailed},
		{n: 7603, kind: app.OpCheckRun, state: app.OperationPending},
		{n: 7604, kind: app.OpPaneOpen, state: app.OperationPending},
	}
	f.inUOW(t, func(uow app.UnitOfWork) {
		for i, s := range specs {
			// Distinct created_at so newest-first ordering is unambiguous.
			at := f.clock.Now().Add(time.Duration(i) * time.Second)
			err := uow.Operations().Create(t.Context(), app.Operation{
				ID: identity.OperationID(uid(s.n)), RunID: f.spec.RunID, Generation: f.lease.Generation,
				Kind: s.kind, State: s.state,
				Intent: map[string]any{}, CreatedAt: at, UpdatedAt: at,
			})
			if err != nil {
				t.Fatalf("create operation %d: %v", s.n, err)
			}
		}
	})

	f.inUOW(t, func(uow app.UnitOfWork) {
		checks, err := uow.Operations().ByKind(t.Context(), f.spec.RunID, app.OpCheckRun)
		if err != nil {
			t.Fatalf("ByKind: %v", err)
		}
		if len(checks) != 3 {
			t.Fatalf("ByKind(check.run) returned %d operations, want 3 across all states", len(checks))
		}
		wantOrder := []identity.OperationID{
			identity.OperationID(uid(7603)), identity.OperationID(uid(7602)), identity.OperationID(uid(7601)),
		}
		for i, want := range wantOrder {
			if checks[i].ID != want {
				t.Fatalf("ByKind order[%d] = %s, want %s (newest first)", i, checks[i].ID, want)
			}
		}
		states := map[app.OperationState]bool{}
		for _, op := range checks {
			states[op.State] = true
			if op.Kind != app.OpCheckRun {
				t.Fatalf("ByKind(check.run) returned a %s operation", op.Kind)
			}
		}
		for _, want := range []app.OperationState{app.OperationSucceeded, app.OperationFailed, app.OperationPending} {
			if !states[want] {
				t.Fatalf("ByKind coverage missing state %s: %v", want, states)
			}
		}
	})
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
