package sqlite_test

import (
	"database/sql"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
)

// TestMigration004Surface pins migration 004's chain facts: the chain's
// latest version is 4, a fresh store records one row per migration, and
// runs carries the nullable, default-free worktrees_retired_at column,
// NULL for a freshly initialized run.
func TestMigration004Surface(t *testing.T) {
	if got := sqlite.LatestMigrationVersion(); got != 4 {
		t.Fatalf("latest migration version = %d, want 4", got)
	}
	f := newFixture(t)
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM schema_migrations`); n != 4 {
		t.Fatalf("schema_migrations rows = %d, want one per migration", n)
	}
	var (
		notNull int
		dflt    sql.NullString
		typ     string
	)
	if err := sqlite.WriteDB(f.store).QueryRowContext(t.Context(),
		`SELECT "notnull", dflt_value, type FROM pragma_table_info('runs') WHERE name = 'worktrees_retired_at'`,
	).Scan(&notNull, &dflt, &typ); err != nil {
		t.Fatalf("runs.worktrees_retired_at is missing after migration: %v", err)
	}
	if notNull != 0 || dflt.Valid || typ != "TEXT" {
		t.Errorf("runs.worktrees_retired_at = %s notnull=%d default=%v, want a nullable TEXT column with no default", typ, notNull, dflt)
	}
	var retired sql.NullString
	if err := sqlite.WriteDB(f.store).QueryRowContext(t.Context(),
		`SELECT worktrees_retired_at FROM runs WHERE id = ?`, f.spec.RunID.String(),
	).Scan(&retired); err != nil {
		t.Fatal(err)
	}
	if retired.Valid {
		t.Errorf("a freshly initialized run has worktrees_retired_at = %q, want NULL", retired.String)
	}
}

// TestMigration004UpgradesPopulated003Store applies 004 over a populated
// store at the 003 boundary through the real migrator: every existing run
// reads NULL (not retired), every pre-existing value and relationship is
// byte-for-byte intact, foreign keys stay clean, and a reopen applies
// nothing further.
func TestMigration004UpgradesPopulated003Store(t *testing.T) {
	root := t.TempDir()
	buildV2Store(t, root, coreV2Fixture("completed", "completed", "completed", "terminated"))
	if err := sqlite.MigrateUpTo(t.Context(), root, 3, newFakeClock()); err != nil {
		t.Fatalf("migrate to the 003 boundary: %v", err)
	}
	raw := openRaw(t, root)
	runColumns := []string{"id", "repository_id", "seq", "brief", "brief_digest", "state", "stop_requested_at", "revision", "created_at", "updated_at", "plan_closed_at"}
	runsBefore := tableSnapshot(t, raw, "runs", runColumns...)
	sessionsBefore := tableSnapshot(t, raw, "sessions", "id", "run_id", "attempt_id", "role", "state", "revision", "parent_session_id")
	leasesBefore := tableSnapshot(t, raw, "run_leases", "run_id", "controller_id", "generation", "state")
	if len(runsBefore) != 1 {
		t.Fatalf("fixture runs = %d, want 1", len(runsBefore))
	}
	var version int
	if err := raw.QueryRowContext(t.Context(), `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 3 {
		t.Fatalf("boundary version = %d (%v), want 3", version, err)
	}

	store := openStoreAt(t, root, newFakeClock())

	if err := raw.QueryRowContext(t.Context(), `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 4 {
		t.Fatalf("version after the upgrade = %d (%v), want 4", version, err)
	}
	requireEqualRows(t, "runs", runsBefore, tableSnapshot(t, raw, "runs", runColumns...))
	requireEqualRows(t, "sessions", sessionsBefore, tableSnapshot(t, raw, "sessions", "id", "run_id", "attempt_id", "role", "state", "revision", "parent_session_id"))
	requireEqualRows(t, "run_leases", leasesBefore, tableSnapshot(t, raw, "run_leases", "run_id", "controller_id", "generation", "state"))
	if got := tableSnapshot(t, raw, "runs", "worktrees_retired_at"); len(got) != 1 || got[0] != "<NULL>" {
		t.Fatalf("pre-existing run worktrees_retired_at = %v, want NULL", got)
	}
	requireCleanFKCheck(t, raw)

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	openStoreAt(t, root, newFakeClock())
	if n := rawCount(t, raw, `SELECT COUNT(*) FROM schema_migrations`); n != 4 {
		t.Fatalf("schema_migrations rows after reopen = %d, want 4, unchanged", n)
	}
	requireEqualRows(t, "runs", runsBefore, tableSnapshot(t, raw, "runs", runColumns...))
}
