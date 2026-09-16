package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/johnlanda/hop/internal/app"
)

// WriteDB exposes the write pool to adapter tests that need raw SQL:
// seeding schema versions, racing constraint inserts from separate handles,
// and inspecting rows no port reads back.
func WriteDB(s *Store) *sql.DB { return s.writes }

// ReadDB exposes the read pool to the connection-churn PRAGMA test.
func ReadDB(s *Store) *sql.DB { return s.reads }

// TxOf exposes a unit of work's transaction, so a test can mutate the lease
// row inside the transaction and prove Commit's re-read rejects a lease
// that is no longer held — a window no port write can open, because every
// other writer serializes behind the open immediate transaction.
func TxOf(u app.UnitOfWork) *sql.Tx {
	uow, ok := u.(*unitOfWork)
	if !ok {
		return nil
	}
	return uow.tx
}

// LatestMigrationVersion exposes the embedded chain's newest version, so
// chain-length expectations in tests derive from the chain itself instead
// of literals that drift every time a migration lands.
func LatestMigrationVersion() int {
	migrations, err := loadMigrations()
	if err != nil {
		panic(err)
	}
	return migrations[len(migrations)-1].version
}

// MigrateUpTo opens the store's pools at stateRoot and applies the
// embedded chain THROUGH THE REAL migrator paths up to and including
// version, then closes: the boundary hook that lets a Phase 2 migration
// test keep proving its original 001→002 upgrade verbatim after later
// migrations extend the chain (a plain Open always migrates to latest).
func MigrateUpTo(ctx context.Context, stateRoot string, version int, clock app.Clock) error {
	if !filepath.IsAbs(stateRoot) {
		return fmt.Errorf("sqlite: state root %q is not an absolute path", stateRoot)
	}
	path := filepath.Join(stateRoot, "hop.db")
	writes, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return fmt.Errorf("sqlite: open boundary write pool: %w", err)
	}
	reads, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return errors.Join(fmt.Errorf("sqlite: open boundary read pool: %w", err), writes.Close())
	}
	s := &Store{writes: writes, reads: reads, clock: clock, leaseTTL: defaultLeaseTTL, stateRoot: stateRoot}
	migrations, err := loadMigrations()
	if err != nil {
		return errors.Join(err, s.Close())
	}
	if err := s.inWriteTx(ctx, func(tx *sql.Tx) error {
		_, execErr := tx.ExecContext(ctx, createSchemaMigrations)
		return execErr
	}); err != nil {
		return errors.Join(err, s.Close())
	}
	for _, m := range migrations {
		if m.version > version {
			break
		}
		if rebuildMigrations[m.version] {
			if err := s.applyRebuildMigration(ctx, m, version); err != nil {
				return errors.Join(err, s.Close())
			}
			continue
		}
		if err := s.applyMigration(ctx, m, version); err != nil {
			return errors.Join(err, s.Close())
		}
	}
	return s.Close()
}
