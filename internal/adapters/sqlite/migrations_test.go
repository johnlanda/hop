package sqlite_test

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
)

// expectedTables is the Phase 2 table set (001 creates every table; 002
// only adds a column) plus the migrator's own version table; the Phase 3
// additions live in expectedPhase3Tables (migration_phase3_test.go), and
// TestMigrationTableListMatchesDesign pins the two lists' union.
func expectedTables() []string {
	return []string{
		"schema_migrations",
		"repositories",
		"runs",
		"run_snapshots",
		"tasks",
		"attempts",
		"sessions",
		"runtime_bindings",
		"launch_claims",
		"worktrees",
		"results",
		"result_submissions",
		"check_requests",
		"check_exec_claims",
		"artifacts",
		"transitions",
		"operations",
		"run_leases",
	}
}

// TestMigrateFromEmpty proves opening an empty state root creates every
// table, applies the full migration chain and records its latest version.
func TestMigrateFromEmpty(t *testing.T) {
	store := openStoreAt(t, t.TempDir(), newFakeClock())

	for _, table := range expectedTables() {
		n := countRows(t, store, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, table)

		if n != 1 {
			t.Errorf("table %q does not exist after migration", table)
		}
	}
	var version int
	if err := sqlite.WriteDB(store).QueryRowContext(t.Context(), `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != sqlite.LatestMigrationVersion() {
		t.Fatalf("schema version = %d, want the chain's latest %d", version, sqlite.LatestMigrationVersion())
	}
}

// TestReopenAtSameVersion proves a second open of a migrated store applies
// nothing and keeps the recorded version rows unchanged.
func TestReopenAtSameVersion(t *testing.T) {
	root := t.TempDir()
	clock := newFakeClock()
	first := openStoreAt(t, root, clock)
	before := countRows(t, first, `SELECT COUNT(*) FROM schema_migrations`)

	second := openStoreAt(t, root, clock)

	after := countRows(t, second, `SELECT COUNT(*) FROM schema_migrations`)
	if latest := sqlite.LatestMigrationVersion(); before != latest || after != latest {
		t.Fatalf("schema_migrations rows: first open %d, second open %d; want one row per migration, unchanged by the reopen", before, after)
	}
}

// TestRefuseFutureSchema proves a store recorded at a version past this
// binary's latest known migration refuses to open with ErrFutureSchema.
func TestRefuseFutureSchema(t *testing.T) {
	root := t.TempDir()
	clock := newFakeClock()
	store := openStoreAt(t, root, clock)
	if _, err := sqlite.WriteDB(store).ExecContext(t.Context(),
		`INSERT INTO schema_migrations (version, applied_at) VALUES (99, '2026-09-14T00:00:00.000000000Z')`,
	); err != nil {
		t.Fatalf("seed future version: %v", err)
	}

	_, err := sqlite.Open(t.Context(), root, sqlite.Options{Clock: clock})

	if !errors.Is(err, sqlite.ErrFutureSchema) {
		t.Fatalf("Open on a future-version store = %v, want ErrFutureSchema", err)
	}
}

// TestConcurrentOpenAndMigrate proves two handles opening the same empty
// root concurrently both succeed: each migration applies in an immediate
// transaction that re-reads the version inside it, so the loser skips.
func TestConcurrentOpenAndMigrate(t *testing.T) {
	root := t.TempDir()
	clock := newFakeClock()
	const handles = 2
	var (
		start sync.WaitGroup
		done  sync.WaitGroup
	)
	start.Add(1)
	errs := make([]error, handles)
	stores := make([]*sqlite.Store, handles)
	for i := range handles {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			stores[i], errs[i] = sqlite.Open(t.Context(), root, sqlite.Options{Clock: clock})
		}()
	}
	start.Done()
	done.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent open %d: %v", i, err)
		}
		defer stores[i].Close() //nolint:errcheck,gocritic // test cleanup of handles opened in this scope; close failures would already surface as errors above.
	}
	n := countRows(t, stores[0], `SELECT COUNT(*) FROM schema_migrations`)
	if n != sqlite.LatestMigrationVersion() {
		t.Fatalf("schema_migrations rows after concurrent migrate = %d, want one row per migration", n)
	}
}

// TestOpenRejectsRelativeRoot proves resolution is the caller's job: a
// relative state root is refused outright.
func TestOpenRejectsRelativeRoot(t *testing.T) {
	if _, err := sqlite.Open(t.Context(), "relative/root", sqlite.Options{}); err == nil {
		t.Fatal("Open accepted a relative state root")
	}
}

// TestConnectionChurnKeepsPragmas proves the DSN-applied per-connection
// settings hold on freshly created physical connections, not only the
// first: the idle pool is emptied so every loop iteration dials anew.
func TestConnectionChurnKeepsPragmas(t *testing.T) {
	store := openStoreAt(t, t.TempDir(), newFakeClock())
	sqlite.WriteDB(store).SetMaxIdleConns(0)
	sqlite.ReadDB(store).SetMaxIdleConns(0)
	check := func(t *testing.T, name, query, want string) {
		t.Helper()
		db := sqlite.WriteDB(store)
		if name == "reads" {
			db = sqlite.ReadDB(store)
		}
		for churn := range 4 {
			conn, err := db.Conn(t.Context())
			if err != nil {
				t.Fatalf("fresh connection %d: %v", churn, err)
			}
			var got string
			scanErr := conn.QueryRowContext(t.Context(), query).Scan(&got)
			if closeErr := conn.Close(); scanErr == nil && closeErr != nil {
				t.Fatalf("close connection %d: %v", churn, closeErr)
			}
			if scanErr != nil {
				t.Fatalf("connection %d %s: %v", churn, query, scanErr)
			}
			if got != want {
				t.Fatalf("%s pool connection %d: %s = %q, want %q", name, churn, query, got, want)
			}
		}
	}
	for _, pool := range []string{"writes", "reads"} {
		t.Run(pool, func(t *testing.T) {
			cases := []struct {
				name  string
				query string
				want  string
			}{
				{name: "foreign keys enforced", query: `PRAGMA foreign_keys`, want: "1"},
				{name: "journal mode WAL", query: `PRAGMA journal_mode`, want: "wal"},
				{name: "synchronous FULL", query: `PRAGMA synchronous`, want: "2"},
				{name: "busy timeout 5000", query: `PRAGMA busy_timeout`, want: "5000"},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					check(t, pool, tc.query, tc.want)
				})
			}
		})
	}
}

// TestMigrationTableListMatchesDesign pins the schema surface: an
// unexpected extra table in the migration is surfaced here, so schema
// growth is a reviewed decision.
func TestMigrationTableListMatchesDesign(t *testing.T) {
	store := openStoreAt(t, t.TempDir(), newFakeClock())

	n := countRows(t, store, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)

	want := append(expectedTables(), expectedPhase3Tables()...)
	if n != len(want) {
		t.Fatalf("store holds %d tables, the design's migrations define %d: %s", n, len(want), fmt.Sprint(want))
	}
}

// TestUpgradePopulatedV1StoreToV2 upgrades a genuine populated version-1
// store: the 001 schema file executed verbatim on a raw connection with
// schema_migrations recording version 1 and a full claim chain (repository
// → run → task → attempt → a settled launch claim, written before the
// seed_evidence column existed). The fixture and every assertion are the
// Phase 2 test's verbatim, held at its ORIGINAL 001→002 migration
// boundary through the real migrator paths (sqlite.MigrateUpTo): a plain
// Open now continues to 003, whose launch-claim session backfill rightly
// refuses this deliberately minimal fixture (its claim has no session
// source at all) — the populated 001(+002)→003 upgrades have their own
// fixtures in migration_phase3_test.go. The old rows, values and
// relationships survive with NULL seed evidence, a new evidence-bearing
// claim row round-trips, and a re-migration at the boundary applies
// nothing further.
func TestUpgradePopulatedV1StoreToV2(t *testing.T) {
	root := t.TempDir()
	schema, err := os.ReadFile(filepath.Join("migrations", "001_initial_schema.sql"))
	if err != nil {
		t.Fatalf("read 001 schema: %v", err)
	}
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(root, "hop.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const ts = "2026-09-01T00:00:00.000000000Z"
	for _, stmt := range []string{
		string(schema),
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL) STRICT`,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (1, '` + ts + `')`,
		`INSERT INTO repositories (id, root_path, created_at) VALUES ('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', '/repos/v1', '` + ts + `')`,
		`INSERT INTO runs (id, repository_id, seq, brief, brief_digest, state, stop_requested_at, revision, created_at, updated_at)
		 VALUES ('bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', 1, 'v1 brief', 'v1-brief-digest', 'running', NULL, 1, '` + ts + `', '` + ts + `')`,
		`INSERT INTO tasks (id, run_id, instructions_digest, state, revision, updated_at)
		 VALUES ('cccccccc-cccc-4ccc-8ccc-cccccccccccc', 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', 'v1-instructions', 'active', 1, '` + ts + `')`,
		`INSERT INTO attempts (id, task_id, number, state, revision, updated_at)
		 VALUES ('dddddddd-dddd-4ddd-8ddd-dddddddddddd', 'cccccccc-cccc-4ccc-8ccc-cccccccccccc', 1, 'running', 1, '` + ts + `')`,
		`INSERT INTO launch_claims (incarnation_id, run_id, attempt_id, executable, argv_digest, pid, state, error, claimed_at, settled_at, settlement_evidence)
		 VALUES ('eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee', 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', 'dddddddd-dddd-4ddd-8ddd-dddddddddddd', '/opt/harness/claude', 'v1-argv-digest', 4242, 'execed', NULL, '` + ts + `', '` + ts + `', 'settled by the v1 controller')`,
	} {
		if _, execErr := raw.ExecContext(t.Context(), stmt); execErr != nil {
			t.Fatalf("build v1 store: %v\nstatement: %.80s", execErr, stmt)
		}
	}
	if closeErr := raw.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	if err := sqlite.MigrateUpTo(t.Context(), root, 2, newFakeClock()); err != nil {
		t.Fatalf("migrate to the 002 boundary: %v", err)
	}
	db := openRaw(t, root)

	var version int
	if err := db.QueryRowContext(t.Context(), `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != 2 {
		t.Fatalf("schema version after the upgrade = %d, want 2", version)
	}
	var (
		executable, argvDigest, state, settlementEvidence string
		pid                                               int
		seedEvidence                                      sql.NullString
	)
	if err := db.QueryRowContext(t.Context(),
		`SELECT lc.executable, lc.argv_digest, lc.pid, lc.state, lc.settlement_evidence, lc.seed_evidence
		 FROM launch_claims lc
		 JOIN attempts a ON a.id = lc.attempt_id
		 JOIN tasks tk ON tk.id = a.task_id AND tk.run_id = lc.run_id
		 JOIN runs r ON r.id = lc.run_id
		 JOIN repositories rp ON rp.id = r.repository_id AND rp.root_path = '/repos/v1'
		 WHERE lc.incarnation_id = 'eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee'`,
	).Scan(&executable, &argvDigest, &pid, &state, &settlementEvidence, &seedEvidence); err != nil {
		t.Fatalf("read the upgraded v1 claim through its full relationship chain: %v", err)
	}
	if executable != "/opt/harness/claude" || argvDigest != "v1-argv-digest" || pid != 4242 || state != "execed" || settlementEvidence != "settled by the v1 controller" {
		t.Fatalf("v1 claim values changed by the upgrade: %s %s %d %s %q", executable, argvDigest, pid, state, settlementEvidence)
	}
	if seedEvidence.Valid {
		t.Fatalf("v1 claim seed evidence = %q, want NULL", seedEvidence.String)
	}

	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO launch_claims (incarnation_id, run_id, attempt_id, executable, argv_digest, pid, state, error, claimed_at, settled_at, settlement_evidence, seed_evidence)
		 VALUES ('ffffffff-ffff-4fff-8fff-ffffffffffff', 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', 'dddddddd-dddd-4ddd-8ddd-dddddddddddd', '/opt/harness/claude', 'v2-argv-digest', 4243, 'exec_pending', NULL, '`+ts+`', NULL, NULL, 'workspace trust seeded for /worktrees/v2')`,
	); err != nil {
		t.Fatalf("insert an evidence-bearing claim after the upgrade: %v", err)
	}
	var newEvidence string
	if err := db.QueryRowContext(t.Context(),
		`SELECT seed_evidence FROM launch_claims WHERE incarnation_id = 'ffffffff-ffff-4fff-8fff-ffffffffffff'`,
	).Scan(&newEvidence); err != nil || newEvidence != "workspace trust seeded for /worktrees/v2" {
		t.Fatalf("new claim evidence = %q, %v", newEvidence, err)
	}
	if err := sqlite.MigrateUpTo(t.Context(), root, 2, newFakeClock()); err != nil {
		t.Fatalf("re-migrate at the boundary: %v", err)
	}
	if n := rawCount(t, db, `SELECT COUNT(*) FROM schema_migrations`); n != 2 {
		t.Fatalf("schema_migrations rows after reopen = %d, want one per migration, unchanged", n)
	}
	if n := rawCount(t, db, `SELECT COUNT(*) FROM launch_claims`); n != 2 {
		t.Fatalf("launch claims after reopen = %d, want both rows intact", n)
	}
}

// rawCount counts rows through a raw handle.
func rawCount(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), query).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	return n
}
