package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"testing"
)

// captureBusyError provokes a genuine SQLITE_BUSY from the pinned driver:
// one raw connection with no busy timeout holds an immediate transaction
// while a second raw connection with no busy timeout tries to begin its
// own. The captured error drives the retry-loop tests without sleeps or
// fabricated error values.
func captureBusyError(t *testing.T, dbPath string) error {
	t.Helper()
	rawDSN := (&url.URL{Scheme: "file", OmitHost: true, Path: dbPath, RawQuery: "_txlock=immediate&_pragma=busy_timeout(0)"}).String()
	holder, err := sql.Open("sqlite", rawDSN)
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := holder.Close(); closeErr != nil {
			t.Errorf("close holder: %v", closeErr)
		}
	})
	contender, err := sql.Open("sqlite", rawDSN)
	if err != nil {
		t.Fatalf("open contender: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := contender.Close(); closeErr != nil {
			t.Errorf("close contender: %v", closeErr)
		}
	})
	heldTx, err := holder.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin held immediate transaction: %v", err)
	}
	_, busyErr := contender.BeginTx(t.Context(), nil)
	if rbErr := heldTx.Rollback(); rbErr != nil {
		t.Fatalf("release held transaction: %v", rbErr)
	}
	if busyErr == nil {
		t.Fatal("contending immediate begin with busy_timeout 0 did not return busy")
	}
	if !isBusy(busyErr) {
		t.Fatalf("contending begin returned %v, which isBusy does not classify", busyErr)
	}
	return busyErr
}

// TestInWriteTxRetriesBusyWholeTransaction proves the bounded
// whole-transaction retry against a real driver busy error: a transaction
// body that reports busy twice is re-run whole and its third attempt's
// write commits exactly once, and a body that stays busy fails after
// exactly txAttempts attempts.
func TestInWriteTxRetriesBusyWholeTransaction(t *testing.T) {
	root := t.TempDir()
	store, err := Open(t.Context(), root, Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	busyErr := captureBusyError(t, filepath.Join(root, "hop.db"))

	t.Run("retries whole and commits once", func(t *testing.T) {
		attempts := 0
		err := store.inWriteTx(t.Context(), func(tx *sql.Tx) error {
			attempts++
			if _, err := tx.ExecContext(t.Context(),
				`INSERT INTO repositories (id, root_path, created_at) VALUES (?, ?, ?)`,
				fmt.Sprintf("00000000-0000-4000-8000-%012d", attempts), "/repos/busy", "2026-09-14T10:00:00.000000000Z",
			); err != nil {
				return err
			}
			if attempts < 3 {
				return busyErr
			}
			return nil
		})
		if err != nil {
			t.Fatalf("inWriteTx after transient busy: %v", err)
		}
		if attempts != 3 {
			t.Fatalf("transaction body ran %d times, want 3 (two busy retries)", attempts)
		}
		var rows int
		if err := store.writes.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM repositories WHERE root_path = '/repos/busy'`,
		).Scan(&rows); err != nil {
			t.Fatalf("count committed rows: %v", err)
		}
		if rows != 1 {
			t.Fatalf("committed rows = %d, want exactly 1 — busy attempts must roll back whole", rows)
		}
	})

	t.Run("bounded when busy persists", func(t *testing.T) {
		attempts := 0
		err := store.inWriteTx(t.Context(), func(*sql.Tx) error {
			attempts++
			return busyErr
		})
		if !errors.Is(err, busyErr) {
			t.Fatalf("persistent busy = %v, want the busy error wrapped", err)
		}
		if attempts != txAttempts {
			t.Fatalf("transaction body ran %d times, want the bounded %d", attempts, txAttempts)
		}
	})
}
