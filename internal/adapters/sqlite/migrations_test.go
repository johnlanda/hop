package sqlite_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
)

// expectedTables is the complete table set of the migration chain (001
// creates every table; 002 only adds a column) plus the migrator's own
// version table.
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
	if version != 2 {
		t.Fatalf("schema version = %d, want 2", version)
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
	if before != 2 || after != 2 {
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
	if n != 2 {
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

	if want := expectedTables(); n != len(want) {
		t.Fatalf("store holds %d tables, the design's migration defines %d: %s", n, len(want), fmt.Sprint(want))
	}
}
