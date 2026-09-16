package sqlite_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
	"github.com/johnlanda/hop/internal/testsupport/storevectors"
)

// newFeatureSpec builds a VALID feature-mode NewRunSpec from newSpec's
// identity block: the block's session is the manager, the solo bootstrap
// identities are cleared, and the frozen integration branch names
// sequence 1 — what the store assigns the first run of a repository.
func newFeatureSpec(repoRoot string, base int, f *fakeClock) app.NewRunSpec {
	spec := newSpec(repoRoot, base, f.Now())
	spec.TaskID = ""
	spec.AttemptID = ""
	spec.WorktreeID = ""
	spec.InstructionsDigest = ""
	spec.Snapshot.Workflow = featureWorkflow()
	spec.Snapshot.Workflow.IntegrationBranch = app.IntegrationBranchName(1)
	spec.Snapshot.Workflow.BaseCommitOID = "cccccccccccccccccccccccccccccccccccccccc"
	spec.Snapshot.Workflow.TargetBranch = "refs/heads/main"
	return spec
}

// TestInitializeRunFeatureShape proves the feature branch of InitializeRun
// (docs/plan/phase-3-design.md section 9): one immediate transaction
// creates the run, the snapshot with its workflow JSON, the manager
// session — role manager, no attempt, no parent, the pre-assigned native
// reference, reserved — and the first lease, and NO task, attempt or
// worktree row. The app fake's TestFakeFeatureBootstrapContracts asserts
// the same shape.
func TestInitializeRunFeatureShape(t *testing.T) {
	clock := newFakeClock()
	store := openStoreAt(t, t.TempDir(), clock)
	spec := newFeatureSpec("/repos/feature-shape", specStride, clock)

	runID, lease, err := store.InitializeRun(t.Context(), spec)
	if err != nil {
		t.Fatalf("InitializeRun: %v", err)
	}
	if runID != spec.RunID || lease.Generation != 1 || lease.ControllerID != spec.ControllerID {
		t.Fatalf("InitializeRun = %s, %+v; want the spec's run at generation 1", runID, lease)
	}

	var (
		state   string
		seq     int
		encoded sql.NullString
	)
	if scanErr := writeDBRowOn(t, store, `SELECT r.state, r.seq, s.workflow FROM runs r JOIN run_snapshots s ON s.run_id = r.id WHERE r.id = ?`, spec.RunID.String()).Scan(&state, &seq, &encoded); scanErr != nil {
		t.Fatalf("read run and snapshot: %v", scanErr)
	}
	if state != string(run.RunCreated) || seq != 1 || !encoded.Valid {
		t.Fatalf("run row = (%s, %d, workflow valid %v), want (created, 1, true)", state, seq, encoded.Valid)
	}
	var stored app.WorkflowSnapshot
	if decodeErr := json.Unmarshal([]byte(encoded.String), &stored); decodeErr != nil {
		t.Fatalf("decode stored workflow: %v", decodeErr)
	}
	if stored != spec.Snapshot.Workflow {
		t.Fatalf("stored workflow = %+v, want %+v", stored, spec.Snapshot.Workflow)
	}
	frozen, err := store.LoadFrozenRun(t.Context(), spec.RunID)
	if err != nil {
		t.Fatalf("LoadFrozenRun: %v", err)
	}
	if frozen.Snapshot.Workflow != spec.Snapshot.Workflow || frozen.RepositoryRoot != spec.RepositoryRoot {
		t.Fatalf("frozen run = %+v, want the spec's workflow and root", frozen)
	}

	var (
		role, harness, sessionState string
		attemptID, parentID         sql.NullString
		nativeRef, nativeRefSource  sql.NullString
		sessionRunID                string
	)
	if scanErr := writeDBRowOn(t, store, `SELECT run_id, role, harness, state, attempt_id, parent_session_id, native_session_ref, native_ref_source FROM sessions WHERE id = ?`, spec.SessionID.String()).
		Scan(&sessionRunID, &role, &harness, &sessionState, &attemptID, &parentID, &nativeRef, &nativeRefSource); scanErr != nil {
		t.Fatalf("read manager session: %v", scanErr)
	}
	if sessionRunID != spec.RunID.String() || role != string(run.RoleManager) || harness != string(spec.Harness) || sessionState != string(run.SessionReserved) {
		t.Fatalf("manager session = (%s, %s, %s, %s), want (%s, manager, %s, reserved)", sessionRunID, role, harness, sessionState, spec.RunID, spec.Harness)
	}
	if attemptID.Valid || parentID.Valid {
		t.Fatalf("manager session attempt/parent = %v/%v, want NULL/NULL", attemptID, parentID)
	}
	if nativeRef.String != spec.NativeSessionRef || nativeRefSource.String != string(run.NativeRefAssigned) {
		t.Fatalf("manager native reference = (%v, %v), want (%s, assigned)", nativeRef, nativeRefSource, spec.NativeSessionRef)
	}

	for _, table := range []struct{ name, query string }{
		{"tasks", `SELECT COUNT(*) FROM tasks WHERE run_id = ?`},
		{"attempts", `SELECT COUNT(*) FROM attempts a JOIN tasks t ON t.id = a.task_id WHERE t.run_id = ?`},
		{"worktrees", `SELECT COUNT(*) FROM worktrees WHERE run_id = ?`},
		{"non-manager sessions", `SELECT COUNT(*) FROM sessions WHERE run_id = ? AND role <> 'manager'`},
	} {
		if n := countRows(t, store, table.query, spec.RunID.String()); n != 0 {
			t.Fatalf("feature InitializeRun created %d %s, want 0", n, table.name)
		}
	}
	if n := countRows(t, store, `SELECT COUNT(*) FROM run_leases WHERE run_id = ? AND generation = 1 AND state = 'held'`, spec.RunID.String()); n != 1 {
		t.Fatalf("held generation-1 lease rows = %d, want 1", n)
	}

	f := &fixture{store: store, clock: clock, spec: spec, lease: lease}
	f.inUOW(t, func(uow app.UnitOfWork) {
		manager, _, managerErr := workflowRepos(t, uow).ManagerSession(t.Context(), spec.RunID)
		if managerErr != nil {
			t.Fatalf("ManagerSession: %v", managerErr)
		}
		if manager.ID != spec.SessionID {
			t.Fatalf("ManagerSession = %s, want the bootstrap manager %s", manager.ID, spec.SessionID)
		}
	})
	detail, err := store.LoadRunStatus(t.Context(), spec.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus: %v", err)
	}
	if detail.Mode != app.WorkflowModeFeature || detail.Sequence != 1 {
		t.Fatalf("run detail mode/sequence = %q/%d, want feature/1", detail.Mode, detail.Sequence)
	}
}

// TestInitializeRunFeatureSecondManagerUnrepresentable proves the
// one-manager partial unique index backs the bootstrap manager: a second
// non-terminal manager for the run is refused, even raced from two store
// handles, and a successor becomes representable only once the first is
// terminal.
func TestInitializeRunFeatureSecondManagerUnrepresentable(t *testing.T) {
	clock := newFakeClock()
	root := t.TempDir()
	storeA := openStoreAt(t, root, clock)
	storeB := openStoreAt(t, root, clock)
	spec := newFeatureSpec("/repos/one-manager", specStride, clock)
	_, lease, err := storeA.InitializeRun(t.Context(), spec)
	if err != nil {
		t.Fatalf("InitializeRun: %v", err)
	}
	now := clock.Now()
	createManager := func(begin func() (app.UnitOfWork, error), n int) error {
		uow, err := begin()
		if err != nil {
			return err
		}
		defer uow.Rollback() //nolint:errcheck // rollback after commit is a documented no-op.
		manager := run.NewManagerSession(identity.SessionID(uid(n)), spec.RunID, run.HarnessClaude, now)
		if _, err := uow.Sessions().Create(t.Context(), manager); err != nil {
			return err
		}
		return uow.Commit()
	}

	var (
		start sync.WaitGroup
		done  sync.WaitGroup
	)
	errs := make([]error, 2)
	start.Add(1)
	done.Add(2)
	go func() {
		defer done.Done()
		start.Wait()
		errs[0] = createManager(func() (app.UnitOfWork, error) { return storeA.Begin(t.Context(), lease) }, 7851)
	}()
	go func() {
		defer done.Done()
		start.Wait()
		errs[1] = createManager(func() (app.UnitOfWork, error) { return storeB.Begin(t.Context(), lease) }, 7852)
	}()
	start.Done()
	done.Wait()
	for i, err := range errs {
		if err == nil {
			t.Fatalf("raced second-manager creation %d succeeded beside the bootstrap manager", i)
		}
	}
	if n := countRows(t, storeA, `SELECT COUNT(*) FROM sessions WHERE run_id = ? AND role = 'manager'`, spec.RunID.String()); n != 1 {
		t.Fatalf("manager sessions after the race = %d, want the bootstrap manager alone", n)
	}

	f := &fixture{store: storeA, clock: clock, spec: spec, lease: lease}
	f.inUOW(t, func(uow app.UnitOfWork) {
		saveSession(t, uow, spec.SessionID, func(v run.Session) (run.Session, error) { return v.Terminate(now) })
	})
	if err := createManager(func() (app.UnitOfWork, error) { return storeA.Begin(t.Context(), lease) }, 7853); err != nil {
		t.Fatalf("successor manager after the bootstrap manager terminated: %v", err)
	}
}

// TestStoreVectorsFeatureBootstrap is the real-store half of the
// feature-bootstrap vectors internal/app's TestStoreVectorsFeatureBootstrap
// drives against its fake: each refusal carries its typed sentinel and
// commits nothing — no repository, run, snapshot, session or lease row —
// and the valid spec resubmitted afterward initializes at sequence 1.
func TestStoreVectorsFeatureBootstrap(t *testing.T) {
	cases := []struct {
		name   string
		bend   func(valid app.NewRunSpec) app.NewRunSpec
		target error
	}{
		{"FeatureRunSpecWithTask", func(v app.NewRunSpec) app.NewRunSpec {
			return storevectors.FeatureRunSpecWithTask(v, identity.TaskID(uid(8901)))
		}, app.ErrFeatureRunSpecInvalid},
		{"FeatureRunSpecWithAttempt", func(v app.NewRunSpec) app.NewRunSpec {
			return storevectors.FeatureRunSpecWithAttempt(v, identity.AttemptID(uid(8902)))
		}, app.ErrFeatureRunSpecInvalid},
		{"FeatureRunSpecWithWorktree", func(v app.NewRunSpec) app.NewRunSpec {
			return storevectors.FeatureRunSpecWithWorktree(v, identity.WorktreeID(uid(8903)))
		}, app.ErrFeatureRunSpecInvalid},
		{"FeatureRunSpecWithoutManager", storevectors.FeatureRunSpecWithoutManager, app.ErrFeatureRunSpecInvalid},
		{"FeatureRunSpecSequenceMismatch", func(v app.NewRunSpec) app.NewRunSpec {
			return storevectors.FeatureRunSpecSequenceMismatch(v, 1)
		}, app.ErrRunSequenceMismatch},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			clock := newFakeClock()
			store := openStoreAt(t, t.TempDir(), clock)
			valid := newFeatureSpec("/repos/vectors", specStride, clock)

			_, _, err := store.InitializeRun(t.Context(), tt.bend(valid))
			if !errors.Is(err, tt.target) {
				t.Fatalf("InitializeRun(%s) error = %v, want %v", tt.name, err, tt.target)
			}
			for _, table := range []string{"repositories", "runs", "run_snapshots", "sessions", "run_leases", "tasks", "attempts"} {
				if n := countRows(t, store, `SELECT COUNT(*) FROM `+table); n != 0 {
					t.Fatalf("a refused InitializeRun left %d %s rows", n, table)
				}
			}

			if _, _, validErr := store.InitializeRun(t.Context(), valid); validErr != nil {
				t.Fatalf("InitializeRun(valid) after the refusal: %v", validErr)
			}
			detail, err := store.LoadRunStatus(t.Context(), valid.RunID)
			if err != nil {
				t.Fatalf("LoadRunStatus: %v", err)
			}
			if detail.Sequence != 1 {
				t.Fatalf("valid run sequence = %d, want 1: the refused attempt must consume nothing", detail.Sequence)
			}
		})
	}
}

// TestInitializeRunFeatureSequenceRace is the refusal's real-world cause:
// a feature freeze predicted sequence 1, another run of the same
// repository committed first, and the predicted spec is refused with
// nothing written; re-frozen for sequence 2, it initializes.
func TestInitializeRunFeatureSequenceRace(t *testing.T) {
	clock := newFakeClock()
	store := openStoreAt(t, t.TempDir(), clock)
	const root = "/repos/sequence-race"

	winner := newSpec(root, specStride, clock.Now())
	if _, _, err := store.InitializeRun(t.Context(), winner); err != nil {
		t.Fatalf("InitializeRun(winner): %v", err)
	}
	predicted := newFeatureSpec(root, 3*specStride, clock)
	if _, _, err := store.InitializeRun(t.Context(), predicted); !errors.Is(err, app.ErrRunSequenceMismatch) {
		t.Fatalf("InitializeRun(predicted sequence 1) error = %v, want ErrRunSequenceMismatch", err)
	}
	if n := countRows(t, store, `SELECT COUNT(*) FROM runs WHERE id = ?`, predicted.RunID.String()); n != 0 {
		t.Fatalf("the refused feature run left %d run rows", n)
	}
	if n := countRows(t, store, `SELECT COUNT(*) FROM sessions WHERE id = ?`, predicted.SessionID.String()); n != 0 {
		t.Fatalf("the refused feature run left %d manager rows", n)
	}

	refrozen := predicted
	refrozen.Snapshot.Workflow.IntegrationBranch = app.IntegrationBranchName(2)
	if _, _, err := store.InitializeRun(t.Context(), refrozen); err != nil {
		t.Fatalf("InitializeRun(re-frozen for sequence 2): %v", err)
	}
	detail, err := store.LoadRunStatus(t.Context(), refrozen.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus: %v", err)
	}
	if detail.Sequence != 2 {
		t.Fatalf("re-frozen run sequence = %d, want 2", detail.Sequence)
	}
}

// writeDBRowOn queries one row through store's write pool.
func writeDBRowOn(t *testing.T, store *sqlite.Store, query string, args ...any) *sql.Row {
	t.Helper()
	return sqlite.WriteDB(store).QueryRowContext(t.Context(), query, args...)
}
