package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/testsupport/hopfixtures"
)

// This file seeds fixture state for the tests in
// grammarcontract_*_test.go through internal/testsupport/hopfixtures —
// never through `hop run`, which would place a worker through Herdr (see
// hopfixtures' own package doc comment). Every seed opens the real
// internal/adapters/sqlite Store, the SAME production package
// cmd/hop/compose.go wires, so the fixture and the command under test
// share an identical persistence contract.

// openFixtureStore opens the real adapter at stateRoot for fixture
// seeding, closed automatically at test cleanup. The exec'd hop binary
// under test opens its OWN separate connection to the same file — WAL
// mode plus the adapter's busy_timeout make that safe, and no test here
// holds a lease the exec'd binary would ever need to acquire (every
// worker-plumbing verb this slice tests is lease-free; see
// cmd/hop/AGENTS.md's invariants).
func openFixtureStore(t *testing.T, stateRoot string) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(context.Background(), stateRoot, sqlite.Options{})
	if err != nil {
		t.Fatalf("open fixture store at %s: %v", stateRoot, err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("close fixture store: %v", closeErr)
		}
	})
	return store
}

// freezeWorkflowSnapshot promotes runID's frozen snapshot from solo (the
// NULL InitializeRun always writes) to feature mode by writing wf's JSON
// directly into run_snapshots.workflow. This is the ONE raw SQL write
// this suite's fixtures need that hopfixtures cannot perform itself (a
// test-helper package's architecture-checker category may depend on
// application and domain code only, never a driven adapter, so it never
// imports internal/adapters/sqlite or triggers database/sql driver
// registration): no port method sets this column after InitializeRun,
// since production code only ever reaches feature mode through slice
// 6b's still-landing `hop run --workflow feature` bootstrap, which
// writes it inside InitializeRun's own transaction. This function is the
// pre-6b legacy substitute; 6b's merge can replace every caller of it
// with the real feature InitializeRun once that bootstrap lands.
//
// Composition code (cmd/hop, including its test files) may import any
// standard package; database/sql's "sqlite" driver is registered
// transitively by this package's own internal/adapters/sqlite import
// (modernc.org/sqlite registers itself under the name "sqlite" from its
// own init()), so no additional import is needed to open a second,
// independent connection to the same database file.
func freezeWorkflowSnapshot(t *testing.T, stateRoot, runID string, wf app.WorkflowSnapshot) { //nolint:gocritic // hugeParam: WorkflowSnapshot is a small fixture value built and consumed once per call, never a hot loop; mirrors this codebase's own convention for domain-shaped DTOs passed by value.
	t.Helper()
	encoded, err := json.Marshal(wf)
	if err != nil {
		t.Fatalf("encode workflow snapshot: %v", err)
	}
	dsn := "file:" + filepath.Join(stateRoot, "hop.db") + "?_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open raw db for workflow freeze: %v", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("close raw db after workflow freeze: %v", closeErr)
		}
	}()
	result, err := db.ExecContext(context.Background(), `UPDATE run_snapshots SET workflow = ? WHERE run_id = ?`, string(encoded), runID)
	if err != nil {
		t.Fatalf("freeze workflow snapshot: %v", err)
	}
	if n, rowsErr := result.RowsAffected(); rowsErr != nil || n != 1 {
		t.Fatalf("freeze workflow snapshot: rows affected = %d (err %v); want exactly 1 row for run %s", n, rowsErr, runID)
	}
}

// featureWorkflowSnapshot builds the frozen feature-mode policy a
// feature fixture freezes onto its run, mirroring
// internal/adapters/sqlite's own test fixtures' featureWorkflow(). Every
// field is a fixture-fixed, plausible value except messageWait, which
// callers set short (a few seconds) when they need hop msg wait's
// rendered default timeout to actually elapse inside the test's own
// bound.
func featureWorkflowSnapshot(runID string, messageWait time.Duration) app.WorkflowSnapshot {
	return app.WorkflowSnapshot{
		Mode: "feature", MaxWorkers: 2, RetryLimit: 3,
		ManagerRolePath:       "/state/roles/manager.md",
		ManagerRoleDigest:     "manager-digest",
		ImplementerRolePath:   "/state/roles/implementer.md",
		ImplementerRoleDigest: "implementer-digest",
		ReviewerRolePath:      "/state/roles/reviewer.md",
		ReviewerRoleDigest:    "reviewer-digest",
		ReviewerHarness:       app.HarnessClaude,
		MessageAttention:      2 * time.Minute,
		MessageWait:           messageWait,
		IntegrationBranch:     "hop/fixture-" + runID + "/integration",
	}
}

// defaultMessageWait is the frozen [messages] wait_timeout every feature
// fixture uses unless a test needs a different (typically shorter) one.
const defaultMessageWait = 50 * time.Second

// featureManager is a running feature-mode run with its manager
// session — the fixture every hop task/plan/msg/answer/review test in
// this package starts from. Built entirely through hopfixtures plus
// this file's own workflow freeze, never through `hop run`. store and
// lease are kept so addImplementTask/addReviewTask can seed further
// state without reopening the store or reacquiring the lease — every
// fixture call in one test finishes in milliseconds, comfortably inside
// the lease's default TTL.
type featureManager struct {
	StateRoot          string
	RepositoryRoot     string
	RunID              string
	ManagerID          string
	ManagerIncarnation string

	store *sqlite.Store
	lease app.Lease
}

// newFeatureManager seeds a fresh feature-mode run driven running with an
// active, bound manager session, under a plain (non-git) repository
// root. seed selects this fixture's identity block; give two fixtures in
// the same test seeds at least 1000 apart.
func newFeatureManager(t *testing.T, seed int, messageWait time.Duration) *featureManager {
	t.Helper()
	return newFeatureManagerAt(t, seed, messageWait, realDir(t))
}

// newFeatureManagerAt is newFeatureManager with an explicit repository
// root — the seam hop review submit's fixtures need, since
// SubmitReviewVerdict resolves --subject's tree object id by running
// `git rev-parse <subject>^{tree}` against the run's recorded repository
// root (internal/app/usecase_review.go), so a review fixture's
// repository root must be a real, on-disk git repository
// (initFixtureGitRepo builds one).
func newFeatureManagerAt(t *testing.T, seed int, messageWait time.Duration, repoRoot string) *featureManager {
	t.Helper()
	stateRoot := freshStateDir(t)
	store := openFixtureStore(t, stateRoot)
	ctx := context.Background()
	now := time.Now().UTC()

	base, lease, err := hopfixtures.Initialize(ctx, store, stateRoot, repoRoot, seed, now)
	if err != nil {
		t.Fatalf("initialize feature run base: %v", err)
	}
	freezeWorkflowSnapshot(t, stateRoot, base.RunID, featureWorkflowSnapshot(base.RunID, messageWait))
	managerID, managerIncarnation, err := hopfixtures.SeedManager(ctx, store, lease, base.RunID, seed, now)
	if err != nil {
		t.Fatalf("seed feature manager: %v", err)
	}
	return &featureManager{
		StateRoot: stateRoot, RepositoryRoot: repoRoot, RunID: base.RunID,
		ManagerID: managerID, ManagerIncarnation: managerIncarnation,
		store: store, lease: lease,
	}
}

// newFeatureManagerIn seeds a second, independent feature-mode run and
// manager inside an ALREADY-open fixture's own store and state root
// (same database, different run) — used only to construct a genuine
// cross-run scenario: a session that exists but belongs to a different
// run than the one under test, which two entirely separate state roots
// cannot exercise (a session id from a different database is simply
// unknown there, not merely foreign).
func (f *featureManager) newFeatureManagerIn(t *testing.T, seed int, messageWait time.Duration) *featureManager {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	base, lease, err := hopfixtures.Initialize(ctx, f.store, f.StateRoot, f.RepositoryRoot, seed, now)
	if err != nil {
		t.Fatalf("initialize second feature run base: %v", err)
	}
	freezeWorkflowSnapshot(t, f.StateRoot, base.RunID, featureWorkflowSnapshot(base.RunID, messageWait))
	managerID, managerIncarnation, err := hopfixtures.SeedManager(ctx, f.store, lease, base.RunID, seed, now)
	if err != nil {
		t.Fatalf("seed second feature manager: %v", err)
	}
	return &featureManager{
		StateRoot: f.StateRoot, RepositoryRoot: f.RepositoryRoot, RunID: base.RunID,
		ManagerID: managerID, ManagerIncarnation: managerIncarnation,
		store: f.store, lease: lease,
	}
}

// env builds this fixture's manager-identity HOP_* variables, merged with
// extra (extra wins on key collision — a test overriding one identity to
// prove a refusal shape).
func (f *featureManager) env(extra map[string]string) map[string]string {
	vars := map[string]string{
		"HOP_STATE_DIR":      f.StateRoot,
		"HOP_RUN_ID":         f.RunID,
		"HOP_SESSION_ID":     f.ManagerID,
		"HOP_INCARNATION_ID": f.ManagerIncarnation,
	}
	for k, v := range extra {
		vars[k] = v
	}
	return vars
}

// childSession is one implementer or reviewer session this fixture's
// manager delegated, bound to a running attempt on TaskID.
type childSession struct {
	TaskID        string
	AttemptID     string
	SessionID     string
	IncarnationID string
}

// env builds this session's HOP_* variables against the owning fixture's
// run and state root, merged with extra. HOP_TASK_ID/HOP_ATTEMPT_ID are
// always included (hop review submit reads them; every other
// worker-plumbing verb this session might drive simply ignores them).
func (c childSession) env(f *featureManager, extra map[string]string) map[string]string {
	vars := map[string]string{
		"HOP_STATE_DIR":      f.StateRoot,
		"HOP_RUN_ID":         f.RunID,
		"HOP_TASK_ID":        c.TaskID,
		"HOP_ATTEMPT_ID":     c.AttemptID,
		"HOP_SESSION_ID":     c.SessionID,
		"HOP_INCARNATION_ID": c.IncarnationID,
	}
	for k, v := range extra {
		vars[k] = v
	}
	return vars
}

// addImplementTask seeds one running implement task with a running child
// implementer session, delegated by f's manager.
func (f *featureManager) addImplementTask(t *testing.T, seed int, title string) childSession {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	taskID, err := hopfixtures.SeedImplementTask(ctx, f.store, f.lease, f.RunID, seed, seed, title, "ready", now)
	if err != nil {
		t.Fatalf("seed implement task: %v", err)
	}
	return f.bindChild(t, seed, "implementer", taskID)
}

// addReworkTask seeds one needs-rework implement task (task seq = seed)
// behind an interrupted first attempt: the shape hop task retry accepts,
// reserving attempt 2.
func (f *featureManager) addReworkTask(t *testing.T, seed int, title string) string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	taskID, err := hopfixtures.SeedImplementTask(ctx, f.store, f.lease, f.RunID, seed, seed, title, "needs-rework", now)
	if err != nil {
		t.Fatalf("seed needs-rework task: %v", err)
	}
	if _, err := hopfixtures.SeedInterruptedAttempt(ctx, f.store, f.lease, taskID, seed+10, now); err != nil {
		t.Fatalf("seed interrupted attempt: %v", err)
	}
	return taskID
}

// addReviewTask seeds one running review task, its subject frozen at
// subjectCommitOID/subjectTreeOID, with a running child reviewer session
// delegated by f's manager.
func (f *featureManager) addReviewTask(t *testing.T, seed int, subjectCommitOID, subjectTreeOID string) childSession {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	taskID, err := hopfixtures.SeedReviewTask(ctx, f.store, f.lease, f.RunID, seed, seed, subjectCommitOID, subjectTreeOID, "active", now)
	if err != nil {
		t.Fatalf("seed review task: %v", err)
	}
	return f.bindChild(t, seed, "reviewer", taskID)
}

// bindChild is addImplementTask/addReviewTask's shared tail: bind a
// child session of role to taskID and drive its attempt running.
func (f *featureManager) bindChild(t *testing.T, seed int, role, taskID string) childSession {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	sessionID, incarnationID, attemptID, err := hopfixtures.SeedChildSession(ctx, f.store, f.lease, f.RunID, taskID, f.ManagerID, role, seed+10, now)
	if err != nil {
		t.Fatalf("seed child session: %v", err)
	}
	if err := hopfixtures.RunAttempt(ctx, f.store, f.lease, attemptID, now); err != nil {
		t.Fatalf("run child attempt: %v", err)
	}
	return childSession{TaskID: taskID, AttemptID: attemptID, SessionID: sessionID, IncarnationID: incarnationID}
}

// soloFixture is a solo-mode run seeded through hopfixtures, never
// through `hop run`.
type soloFixture struct {
	hopfixtures.Base
	StateRoot      string
	RepositoryRoot string

	store *sqlite.Store
	lease app.Lease
}

// newSoloReserved seeds a fresh solo run at InitializeRun's own initial
// reserved states — the fixture hop launch's refusal-before-exec shapes
// drive further themselves (LaunchBase, then a deliberately conflicting
// SeedLaunchClaim).
func newSoloReserved(t *testing.T, seed int) *soloFixture {
	t.Helper()
	stateRoot := freshStateDir(t)
	repoRoot := realDir(t)
	store := openFixtureStore(t, stateRoot)
	base, lease, err := hopfixtures.Initialize(context.Background(), store, stateRoot, repoRoot, seed, time.Now().UTC())
	if err != nil {
		t.Fatalf("initialize solo fixture: %v", err)
	}
	return &soloFixture{Base: base, StateRoot: stateRoot, RepositoryRoot: repoRoot, store: store, lease: lease}
}

// newSoloRunning seeds a fresh solo run driven all the way to running
// (binding, settled launch claim, running lifecycle transitions) — the
// state hop result submit's accepted/duplicate shapes and a solo
// worker's messaging verbs need.
func newSoloRunning(t *testing.T, seed int) *soloFixture {
	t.Helper()
	f := newSoloReserved(t, seed)
	if err := hopfixtures.RunBase(context.Background(), f.store, f.lease, f.Base, time.Now().UTC()); err != nil {
		t.Fatalf("run solo fixture: %v", err)
	}
	return f
}

// launch drives this fixture from InitializeRun's initial reserved
// states through to launching (hopfixtures.LaunchBase) — the state hop
// launch's PrepareSessionLaunchExec requires before it will even
// validate the pane environment.
func (f *soloFixture) launch(t *testing.T) {
	t.Helper()
	if err := hopfixtures.LaunchBase(context.Background(), f.store, f.lease, f.Base, time.Now().UTC()); err != nil {
		t.Fatalf("launch solo fixture: %v", err)
	}
}

// conflictingClaimPID is a pid this test process's own exec'd hop launch
// child can never itself be (pid 1 is the kernel/init process on every
// platform this suite runs on).
const conflictingClaimPID = 1

// seedConflictingClaim writes a launch claim for this fixture's
// incarnation under conflictingClaimPID, without any exec — so a
// subsequent real `hop launch` exec (a different, real process, whose
// pid is never 1) observes a claim under a DIFFERENT pid than its own
// and refuses. Call this AFTER launch.
func (f *soloFixture) seedConflictingClaim(t *testing.T) {
	t.Helper()
	err := hopfixtures.SeedLaunchClaim(context.Background(), f.store, f.RunID, f.SessionID, f.AttemptID, f.IncarnationID, conflictingClaimPID, time.Now().UTC())
	if err != nil {
		t.Fatalf("seed conflicting launch claim: %v", err)
	}
}

// env builds this fixture's worker-identity HOP_* variables, merged with
// extra.
func (f *soloFixture) env(extra map[string]string) map[string]string {
	vars := map[string]string{
		"HOP_STATE_DIR":      f.StateRoot,
		"HOP_RUN_ID":         f.RunID,
		"HOP_TASK_ID":        f.TaskID,
		"HOP_ATTEMPT_ID":     f.AttemptID,
		"HOP_SESSION_ID":     f.SessionID,
		"HOP_INCARNATION_ID": f.IncarnationID,
	}
	for k, v := range extra {
		vars[k] = v
	}
	return vars
}
