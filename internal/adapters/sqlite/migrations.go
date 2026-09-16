package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrFutureSchema reports that the store's schema version is newer than the
// latest migration this binary knows: a newer HOP wrote it, and applying
// this binary against it could silently misread rows. Forward-only, never
// downgraded.
var ErrFutureSchema = errors.New("sqlite: store schema is newer than this binary")

// migrationFiles embeds the ordered migration SQL files. Each file is named
// NNN_<name>.sql; NNN is its 1-based version and versions are dense.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// createSchemaMigrations bootstraps the version table itself; it is not a
// migration, so an empty database and a current one converge on the same
// statement.
const createSchemaMigrations = `CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
) STRICT`

// migration is one embedded migration file.
type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads and orders the embedded migrations, verifying the
// versions are dense from 1.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("sqlite: read embedded migrations: %w", err)
	}
	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			return nil, fmt.Errorf("sqlite: migration file %q is not named NNN_<name>.sql", name)
		}
		version, err := strconv.Atoi(prefix)
		if err != nil || version < 1 {
			return nil, fmt.Errorf("sqlite: migration file %q has no positive numeric version prefix", name)
		}
		content, err := fs.ReadFile(migrationFiles, "migrations/"+name)
		if err != nil {
			return nil, fmt.Errorf("sqlite: read migration %q: %w", name, err)
		}
		migrations = append(migrations, migration{version: version, name: name, sql: string(content)})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	for i, m := range migrations {
		if m.version != i+1 {
			return nil, fmt.Errorf("sqlite: migration versions are not dense from 1: %q is version %d at position %d", m.name, m.version, i+1)
		}
	}
	if len(migrations) == 0 {
		return nil, errors.New("sqlite: no embedded migrations")
	}
	return migrations, nil
}

// migrate brings the database to this binary's latest schema version. Each
// migration applies in its own immediate transaction that re-reads the
// version inside it, so two processes opening concurrently serialize
// correctly and the loser of the race skips the already-applied step. A
// store past the latest known version is refused with ErrFutureSchema even
// when nothing is pending.
func (s *Store) migrate(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	latest := migrations[len(migrations)-1].version
	if err := s.inWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, createSchemaMigrations)
		return err
	}); err != nil {
		return fmt.Errorf("sqlite: bootstrap schema_migrations: %w", err)
	}
	for _, m := range migrations {
		if rebuildMigrations[m.version] {
			if err := s.applyRebuildMigration(ctx, m, latest); err != nil {
				return err
			}
			continue
		}
		if err := s.applyMigration(ctx, m, latest); err != nil {
			return err
		}
	}
	return s.inWriteTx(ctx, func(tx *sql.Tx) error {
		current, err := schemaVersion(ctx, tx)
		if err != nil {
			return err
		}
		if current > latest {
			return fmt.Errorf("%w: store version %d, latest known migration %d", ErrFutureSchema, current, latest)
		}
		return nil
	})
}

// applyMigration applies one still-pending migration in its own immediate
// transaction, re-reading the schema version inside it first.
func (s *Store) applyMigration(ctx context.Context, m migration, latest int) error {
	return s.inWriteTx(ctx, func(tx *sql.Tx) error {
		current, err := schemaVersion(ctx, tx)
		if err != nil {
			return err
		}
		if current > latest {
			return fmt.Errorf("%w: store version %d, latest known migration %d", ErrFutureSchema, current, latest)
		}
		if current >= m.version {
			return nil
		}
		if m.version != current+1 {
			return fmt.Errorf("sqlite: migration %q is version %d but the store is at %d; migrations apply densely in order", m.name, m.version, current)
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("sqlite: apply migration %q: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			m.version, formatTime(s.now()),
		); err != nil {
			return fmt.Errorf("sqlite: record migration %q: %w", m.name, err)
		}
		return nil
	})
}

// schemaVersion reads the highest applied migration version inside tx; an
// empty table is version 0.
func schemaVersion(ctx context.Context, tx *sql.Tx) (int, error) {
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, fmt.Errorf("sqlite: read schema version: %w", err)
	}
	return version, nil
}

// rebuildMigrations names the migrations that physically rebuild STRICT
// tables (create new, copy, drop, rename) and therefore run on a dedicated
// foreign-keys-off connection with an explicit foreign_key_check before
// commit: 003 relaxes NOT NULL columns on sessions, launch_claims and
// check_requests (docs/plan/phase-3-design.md section 4).
var rebuildMigrations = map[int]bool{3: true} //nolint:gochecknoglobals // fixed, immutable classification of the embedded migration chain; never mutates after init.

// applyRebuildMigration applies one still-pending rebuild migration on a
// DEDICATED connection whose DSN sets foreign_keys(0) before any BEGIN (the
// pragma is connection-scoped and a no-op inside a transaction). The
// dedicated handle is closed on every path — success, failure and
// panic-unwind alike, via the deferred Close — and is never any pool the
// store serves queries from, so a connection with enforcement off cannot
// outlive the migration. Inside one immediate transaction it re-reads the
// version (two racing processes serialize; the loser skips), validates the
// launch-claim session backfill in Go so ambiguity fails NAMING the claim
// rather than as a bare NOT NULL violation, executes the migration SQL,
// requires a clean PRAGMA foreign_key_check, and records the version.
func (s *Store) applyRebuildMigration(ctx context.Context, m migration, latest int) error {
	// Cheap pre-check on the ordinary pool: a store already at or past this
	// version never opens the dedicated connection at all. The authoritative
	// re-check still happens inside the rebuild transaction.
	applied := false
	if err := s.inWriteTx(ctx, func(tx *sql.Tx) error {
		current, err := schemaVersion(ctx, tx)
		if err != nil {
			return err
		}
		applied = current >= m.version
		return nil
	}); err != nil {
		return err
	}
	if applied {
		return nil
	}

	db, err := sql.Open("sqlite", rebuildDSN(filepath.Join(s.stateRoot, "hop.db")))
	if err != nil {
		return fmt.Errorf("sqlite: open dedicated rebuild connection: %w", err)
	}
	defer db.Close() //nolint:errcheck // the dedicated rebuild handle is closed on every path; a close failure after a committed (or failed) migration changes nothing the error return has not already said.
	db.SetMaxOpenConns(1)

	var lastErr error
	for attempt := range txAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("sqlite: canceled while retrying a busy rebuild migration: %w", context.Cause(ctx))
			case <-time.After(time.Duration(attempt) * txBackoff):
			}
		}
		lastErr = s.runRebuildTx(ctx, db, m, latest)
		if lastErr == nil || !isBusy(lastErr) {
			return lastErr
		}
	}
	return fmt.Errorf("sqlite: rebuild migration still busy after %d attempts: %w", txAttempts, lastErr)
}

// runRebuildTx runs one attempt of the rebuild migration transaction on the
// dedicated foreign-keys-off handle.
func (s *Store) runRebuildTx(ctx context.Context, db *sql.DB, m migration, latest int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin rebuild migration transaction: %w", err)
	}
	if err := s.rebuildInTx(ctx, tx, m, latest); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("sqlite: rollback rebuild migration: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit rebuild migration: %w", err)
	}
	return nil
}

// rebuildInTx is the body of the rebuild migration transaction.
func (s *Store) rebuildInTx(ctx context.Context, tx *sql.Tx, m migration, latest int) error {
	current, err := schemaVersion(ctx, tx)
	if err != nil {
		return err
	}
	if current > latest {
		return fmt.Errorf("%w: store version %d, latest known migration %d", ErrFutureSchema, current, latest)
	}
	if current >= m.version {
		return nil
	}
	if m.version != current+1 {
		return fmt.Errorf("sqlite: migration %q is version %d but the store is at %d; migrations apply densely in order", m.name, m.version, current)
	}
	if err := validateLaunchClaimBackfill(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("sqlite: apply rebuild migration %q: %w", m.name, err)
	}
	if err := requireCleanForeignKeys(ctx, tx, m.name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		m.version, formatTime(s.now()),
	); err != nil {
		return fmt.Errorf("sqlite: record rebuild migration %q: %w", m.name, err)
	}
	return nil
}

// validateLaunchClaimBackfill proves, BEFORE the launch_claims rebuild SQL
// runs, that every claim's session_id backfill is unambiguous: exactly one
// distinct binding session for the claim's incarnation, or — for a claim
// with NO binding row — exactly one historical pane.open/launch.send
// operation whose intent JSON carries the claim's incarnation_id, with a
// usable session_id. A claim matching no binding and no intent, more than
// one intent, more than one binding session, or an intent without a session
// identity fails the migration NAMING the claim — real ambiguity is
// surfaced, never guessed away (docs/plan/phase-3-design.md section 4).
func validateLaunchClaimBackfill(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT lc.incarnation_id,
		       (SELECT COUNT(DISTINCT rb.session_id) FROM runtime_bindings rb
		         WHERE rb.incarnation_id = lc.incarnation_id),
		       (SELECT COUNT(*) FROM operations o
		         WHERE o.kind IN ('pane.open', 'launch.send')
		           AND json_extract(o.intent, '$.incarnation_id') = lc.incarnation_id),
		       (SELECT COUNT(*) FROM operations o
		         WHERE o.kind IN ('pane.open', 'launch.send')
		           AND json_extract(o.intent, '$.incarnation_id') = lc.incarnation_id
		           AND COALESCE(json_extract(o.intent, '$.session_id'), '') != '')
		FROM launch_claims lc`)
	if err != nil {
		return fmt.Errorf("sqlite: validate launch-claim session backfill: %w", err)
	}
	defer rows.Close() //nolint:errcheck // the deferred close of a fully-iterated read cursor has no failure the rows.Err check below misses.
	for rows.Next() {
		var (
			incarnation                                    string
			bindingSessions, intents, sessionCarryingCount int64
		)
		if err := rows.Scan(&incarnation, &bindingSessions, &intents, &sessionCarryingCount); err != nil {
			return fmt.Errorf("sqlite: scan launch-claim backfill row: %w", err)
		}
		switch {
		case bindingSessions == 1:
			// The binding is authoritative; intents are irrelevant.
		case bindingSessions > 1:
			return fmt.Errorf("sqlite: migration 003 refused: launch claim %s matches %d distinct binding sessions; the session backfill is ambiguous", incarnation, bindingSessions)
		case intents == 0:
			return fmt.Errorf("sqlite: migration 003 refused: launch claim %s matches no binding row and no historical launch intent; its session cannot be backfilled", incarnation)
		case intents > 1:
			return fmt.Errorf("sqlite: migration 003 refused: launch claim %s matches no binding row and %d historical launch intents; the session backfill is ambiguous", incarnation, intents)
		case sessionCarryingCount != 1:
			return fmt.Errorf("sqlite: migration 003 refused: launch claim %s's one historical launch intent carries no session identity", incarnation)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite: iterate launch-claim backfill rows: %w", err)
	}
	return nil
}

// requireCleanForeignKeys fails the migration on any foreign_key_check
// violation, naming each violating table and parent, before anything
// commits.
func requireCleanForeignKeys(ctx context.Context, tx *sql.Tx, name string) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("sqlite: foreign_key_check after %q: %w", name, err)
	}
	defer rows.Close() //nolint:errcheck // the deferred close of a fully-iterated read cursor has no failure the rows.Err check below misses.
	var violations []string
	for rows.Next() {
		var (
			table, parent string
			rowid, fkid   sql.NullInt64
		)
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return fmt.Errorf("sqlite: scan foreign_key_check row: %w", err)
		}
		violations = append(violations, fmt.Sprintf("%s rowid %d references missing %s", table, rowid.Int64, parent))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite: iterate foreign_key_check rows: %w", err)
	}
	if len(violations) > 0 {
		return fmt.Errorf("sqlite: migration %q leaves foreign key violations, refused before commit: %s", name, strings.Join(violations, "; "))
	}
	return nil
}
