// Package sqlite is the driven adapter behind the application's StateStore,
// UnitOfWork, ReadStore and SubmissionStore ports: one SQLite database at
// <state root>/hop.db, shared by every repository and run, accessed through
// the pure-Go modernc.org/sqlite driver (pinned in go.mod). The caller —
// cmd/hop's single state-root resolver — hands Open an absolute state root;
// this package never resolves environment variables or defaults. Every
// physical connection carries the design's DSN-applied settings (WAL,
// synchronous FULL, foreign keys on, busy_timeout 5000), write transactions
// begin immediate and are retried whole on SQLITE_BUSY, all IDs are
// canonical UUID text, and all times are fixed-width canonical UTC text
// parsed — never string-compared — for lease and expiry decisions.
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	driver "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/johnlanda/hop/internal/app"
)

// timeLayout is the fixed-width canonical UTC timestamp form of every
// persisted time: nine-digit fractional seconds and a literal trailing Z,
// so equal-length lexical comparison agrees with time order. Lease and
// expiry decisions still compare parsed times, never raw strings.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

const (
	// defaultLeaseTTL is the controller heartbeat TTL of the design
	// (section 4, lease rules) applied when Options leaves LeaseTTL zero.
	defaultLeaseTTL = 30 * time.Second
	// txAttempts bounds the whole-transaction retry on a busy database.
	txAttempts = 5
	// txBackoff is the per-attempt backoff unit between busy retries.
	txBackoff = 25 * time.Millisecond
)

// Options configures Open. The zero value is the production configuration:
// the wall clock and the design's 30s lease TTL.
type Options struct {
	// Clock supplies the store's own time readings: lease acquisition and
	// expiry decisions, commit-time fencing, and row timestamps the ports
	// do not carry explicitly. Nil means the UTC wall clock.
	Clock app.Clock
	// LeaseTTL is how far a lease's expires_at is advanced by
	// InitializeRun, AcquireLease and Heartbeat. Zero means 30s.
	LeaseTTL time.Duration
}

// Store is the SQLite persistence adapter. It implements app.StateStore,
// app.ReadStore and app.SubmissionStore over two connection pools onto the
// same database file: a write pool whose transactions begin immediate, and
// a read pool for lease-free reads.
type Store struct {
	writes    *sql.DB
	reads     *sql.DB
	clock     app.Clock
	leaseTTL  time.Duration
	stateRoot string
}

// Interface conformance, checked at compile time.
var (
	_ app.StateStore      = (*Store)(nil)
	_ app.ReadStore       = (*Store)(nil)
	_ app.SubmissionStore = (*Store)(nil)
)

// systemClock is the default clock: the UTC wall clock.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Open opens (creating and migrating as needed) the store rooted at the
// absolute stateRoot: the database lives at <stateRoot>/hop.db. A relative
// root is refused — resolution is the caller's job, and a worker-context
// command must never fall back to an unrelated store.
func Open(ctx context.Context, stateRoot string, opts Options) (*Store, error) {
	if !filepath.IsAbs(stateRoot) {
		return nil, fmt.Errorf("sqlite: state root %q is not an absolute path; the caller resolves the root, this adapter never does", stateRoot)
	}
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		return nil, fmt.Errorf("sqlite: create state root: %w", err)
	}
	path := filepath.Join(stateRoot, "hop.db")
	writes, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open write pool: %w", err)
	}
	reads, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("sqlite: open read pool: %w", err), writes.Close())
	}
	s := &Store{
		writes:    writes,
		reads:     reads,
		clock:     opts.Clock,
		leaseTTL:  opts.LeaseTTL,
		stateRoot: stateRoot,
	}
	if s.clock == nil {
		s.clock = systemClock{}
	}
	if s.leaseTTL <= 0 {
		s.leaseTTL = defaultLeaseTTL
	}
	if err := s.migrate(ctx); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	return s, nil
}

// Close closes both connection pools.
func (s *Store) Close() error {
	return errors.Join(s.writes.Close(), s.reads.Close())
}

// dsn builds the connection string for the database at path. The _pragma
// settings are DSN-applied so every physical connection in the pool gets
// them, not only the first; immediateWrites additionally makes every
// transaction on the pool begin immediate, which is how write transactions
// take their lock up front instead of deadlocking on upgrade. The path is
// percent-encoded through net/url, so filesystem characters ('?', '#',
// '%', spaces) are never read as URI syntax: the database always lands at
// exactly the given path, and no root spelling can smuggle SQLite URI
// options such as mode=memory into the connection.
func dsn(path string, immediateWrites bool) string {
	query := url.Values{}
	if immediateWrites {
		query.Set("_txlock", "immediate")
	}
	query["_pragma"] = []string{
		"busy_timeout(5000)",
		"foreign_keys(1)",
		"journal_mode(WAL)",
		"synchronous(FULL)",
	}
	u := url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: query.Encode()}
	return u.String()
}

// now reads the store's clock in UTC.
func (s *Store) now() time.Time { return s.clock.Now().UTC() }

// formatTime renders t in the store's fixed-width canonical UTC form.
func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// parseTime parses a stored canonical timestamp. Go's layout parsing
// accepts more spellings than the canonical form (a comma fraction, an
// unpadded hour), so the parsed value is re-rendered and must reproduce
// the input exactly: anything else breaks the equal-width lexical-order
// property and is rejected.
func parseTime(value string) (time.Time, error) {
	t, err := time.Parse(timeLayout, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("sqlite: parse stored timestamp %q: %w", value, err)
	}
	if formatTime(t) != value {
		return time.Time{}, fmt.Errorf("sqlite: stored timestamp %q is not in canonical form", value)
	}
	return t, nil
}

// newUUID mints a canonical lowercase UUIDv4 for rows whose identity is
// adapter-internal (repositories, bindings, transitions, receipts); every
// identity a port hands in is used verbatim instead.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("sqlite: mint row id: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// sqliteCode extracts the primary SQLite result code from err, or 0 when
// err carries none.
func sqliteCode(err error) int {
	var se *driver.Error
	if errors.As(err, &se) {
		return se.Code() & 0xff
	}
	return 0
}

// isBusy reports whether err is a busy or locked condition worth retrying
// as a whole transaction.
func isBusy(err error) bool {
	code := sqliteCode(err)
	return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
}

// inWriteTx runs fn inside one immediate write transaction and retries the
// whole transaction, bounded, when SQLite reports the database busy. The
// retry is safe because fn never performs an external act: the design's
// transaction rule keeps every external call outside every transaction, so
// re-running fn re-runs only reads and writes that were rolled back.
func (s *Store) inWriteTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	var lastErr error
	for attempt := range txAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("sqlite: canceled while retrying a busy write transaction: %w", context.Cause(ctx))
			case <-time.After(time.Duration(attempt) * txBackoff):
			}
		}
		lastErr = s.runWriteTx(ctx, fn)
		if lastErr == nil || !isBusy(lastErr) {
			return lastErr
		}
	}
	return fmt.Errorf("sqlite: write transaction still busy after %d attempts: %w", txAttempts, lastErr)
}

// runWriteTx runs fn inside one immediate transaction, committing on nil
// and rolling back on error.
func (s *Store) runWriteTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.writes.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin immediate write transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("sqlite: rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit write transaction: %w", err)
	}
	return nil
}

// nullString renders "" as NULL and any other value as itself.
func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// nullTime renders the zero time as NULL and any other time canonically.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return formatTime(t)
}

// boolToInt renders a bool as the INTEGER 0 or 1 a STRICT table stores.
func boolToInt(v bool) int64 {
	if v {
		return 1
	}
	return 0
}
