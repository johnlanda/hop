package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
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
