package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// Lease row states. The row is created only by InitializeRun and never
// deleted or reinserted; released is the only other state.
const (
	leaseHeld     = "held"
	leaseReleased = "released"
)

// selectLease reads the lease row's CAS-relevant columns.
const selectLease = `SELECT controller_id, generation, state, expires_at FROM run_leases WHERE run_id = ?`

// InitializeRun atomically creates the repository row (get-or-create by
// resolved root path), the run with its repository-scoped sequence number,
// the frozen snapshot, the task, attempt and session rows, and the initial
// controller lease at generation 1, in one immediate transaction.
func (s *Store) InitializeRun(ctx context.Context, spec app.NewRunSpec) (identity.RunID, app.Lease, error) { //nolint:gocritic // hugeParam: the port passes the spec by value; the adapter mirrors its signature.
	now := spec.Now
	if now.IsZero() {
		now = s.now()
	}
	at := formatTime(now)
	var lease app.Lease
	err := s.inWriteTx(ctx, func(tx *sql.Tx) error {
		repositoryID, err := repositoryIDForRoot(ctx, tx, spec.RepositoryRoot, at)
		if err != nil {
			return err
		}
		var seq int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(seq), 0) + 1 FROM runs WHERE repository_id = ?`, repositoryID,
		).Scan(&seq); err != nil {
			return fmt.Errorf("sqlite: next run sequence: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO runs (id, repository_id, seq, brief, brief_digest, state, stop_requested_at, revision, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, NULL, 1, ?, ?)`,
			spec.RunID.String(), repositoryID, seq, spec.Brief, spec.BriefDigest, string(run.RunCreated), at, at,
		); err != nil {
			return fmt.Errorf("sqlite: insert run: %w", err)
		}
		if err := insertSnapshot(ctx, tx, spec.RunID, &spec.Snapshot, at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tasks (id, run_id, instructions_digest, state, revision, updated_at) VALUES (?, ?, ?, ?, 1, ?)`,
			spec.TaskID.String(), spec.RunID.String(), spec.InstructionsDigest, string(run.TaskPending), at,
		); err != nil {
			return fmt.Errorf("sqlite: insert task: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO attempts (id, task_id, number, state, revision, updated_at) VALUES (?, ?, 1, ?, 1, ?)`,
			spec.AttemptID.String(), spec.TaskID.String(), string(run.AttemptReserved), at,
		); err != nil {
			return fmt.Errorf("sqlite: insert attempt: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (id, run_id, attempt_id, role, harness, native_session_ref, native_ref_source, state, revision, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
			spec.SessionID.String(), spec.RunID.String(), spec.AttemptID.String(), string(run.RoleWorker), string(spec.Harness),
			nullString(spec.NativeSessionRef), nativeRefSource(spec.NativeSessionRef), string(run.SessionReserved), at,
		); err != nil {
			return fmt.Errorf("sqlite: insert session: %w", err)
		}
		expires := now.Add(s.leaseTTL)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO run_leases (run_id, controller_id, generation, state, acquired_at, heartbeat_at, expires_at)
			 VALUES (?, ?, 1, ?, ?, ?, ?)`,
			spec.RunID.String(), spec.ControllerID, leaseHeld, at, at, formatTime(expires),
		); err != nil {
			return fmt.Errorf("sqlite: insert initial lease: %w", err)
		}
		lease = app.Lease{Run: spec.RunID, ControllerID: spec.ControllerID, Generation: 1, ExpiresAt: expires}
		return nil
	})
	if err != nil {
		return "", app.Lease{}, err
	}
	return spec.RunID, lease, nil
}

// nativeRefSource returns the stored native_ref_source for a pre-assigned
// reference: assigned when one exists, NULL otherwise. InitializeRun only
// ever records references HOP itself pre-assigned; captured sources arrive
// through SessionRepository.Save.
func nativeRefSource(ref string) any {
	if ref == "" {
		return nil
	}
	return string(run.NativeRefAssigned)
}

// repositoryIDForRoot gets or creates the repository row for a resolved
// root path. A race between two processes resolves through the UNIQUE
// (root_path) constraint: the loser's insert is a no-op and both read the
// same surviving row.
func repositoryIDForRoot(ctx context.Context, q querier, root, at string) (string, error) {
	id, err := newUUID()
	if err != nil {
		return "", err
	}
	if _, err := q.ExecContext(ctx,
		`INSERT INTO repositories (id, root_path, created_at) VALUES (?, ?, ?) ON CONFLICT (root_path) DO NOTHING`,
		id, root, at,
	); err != nil {
		return "", fmt.Errorf("sqlite: get-or-create repository: %w", err)
	}
	var existing string
	if err := q.QueryRowContext(ctx, `SELECT id FROM repositories WHERE root_path = ?`, root).Scan(&existing); err != nil {
		return "", fmt.Errorf("sqlite: read repository row: %w", err)
	}
	return existing, nil
}

// insertSnapshot persists the run's immutable frozen configuration.
func insertSnapshot(ctx context.Context, tx *sql.Tx, runID identity.RunID, snapshot *app.RunSnapshot, at string) error {
	checkArgv, err := json.Marshal(snapshot.CheckArgv)
	if err != nil {
		return fmt.Errorf("sqlite: encode check argv: %w", err)
	}
	envPolicy, err := json.Marshal(snapshot.EnvPolicy)
	if err != nil {
		return fmt.Errorf("sqlite: encode env policy: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO run_snapshots (run_id, check_argv, check_timeout_ms, check_repeatable, env_policy, harness, profile_dir, state_root, assignment_path, assignment_digest, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID.String(), string(checkArgv), snapshot.CheckTimeout.Milliseconds(), boolToInt(snapshot.CheckRepeatable),
		string(envPolicy), snapshot.Harness, nullString(snapshot.ProfileDir), snapshot.StateRoot,
		snapshot.AssignmentPath, snapshot.AssignmentDigest, at,
	); err != nil {
		return fmt.Errorf("sqlite: insert run snapshot: %w", err)
	}
	return nil
}

// AcquireLease claims or takes over an existing run's controller lease.
// Acquisition succeeds only when the row is released or expired at the
// store clock's reading (parsed times, never string comparison), and it
// increments the generation in place — monotonic for the life of the row.
func (s *Store) AcquireLease(ctx context.Context, runID identity.RunID, controllerID string) (app.Lease, error) {
	var lease app.Lease
	err := s.inWriteTx(ctx, func(tx *sql.Tx) error {
		var (
			holder, state, expiresAt string
			generation               int64
		)
		err := tx.QueryRowContext(ctx, selectLease, runID.String()).Scan(&holder, &generation, &state, &expiresAt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: lease of run %s: %w", runID, app.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("sqlite: read lease of run %s: %w", runID, err)
		}
		now := s.now()
		expires, err := parseTime(expiresAt)
		if err != nil {
			return err
		}
		if state == leaseHeld && !expires.Before(now) {
			return fmt.Errorf("sqlite: run %s lease is held by %q until %s: %w", runID, holder, expiresAt, app.ErrLeaseHeld)
		}
		nextGeneration := generation + 1
		nextExpires := now.Add(s.leaseTTL)
		at := formatTime(now)
		if _, err := tx.ExecContext(ctx,
			`UPDATE run_leases SET controller_id = ?, generation = ?, state = ?, acquired_at = ?, heartbeat_at = ?, expires_at = ? WHERE run_id = ?`,
			controllerID, nextGeneration, leaseHeld, at, at, formatTime(nextExpires), runID.String(),
		); err != nil {
			return fmt.Errorf("sqlite: acquire lease of run %s: %w", runID, err)
		}
		lease = app.Lease{Run: runID, ControllerID: controllerID, Generation: nextGeneration, ExpiresAt: nextExpires}
		return nil
	})
	if err != nil {
		return app.Lease{}, err
	}
	return lease, nil
}

// Heartbeat extends the lease by compare-and-swap: the update applies only
// while the row still matches (run, controller, generation, held), so a
// stale controller can never extend a successor's lease.
func (s *Store) Heartbeat(ctx context.Context, lease app.Lease) error {
	return s.inWriteTx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		result, err := tx.ExecContext(ctx,
			`UPDATE run_leases SET heartbeat_at = ?, expires_at = ? WHERE run_id = ? AND controller_id = ? AND generation = ? AND state = ?`,
			formatTime(now), formatTime(now.Add(s.leaseTTL)), lease.Run.String(), lease.ControllerID, lease.Generation, leaseHeld,
		)
		if err != nil {
			return fmt.Errorf("sqlite: heartbeat lease of run %s: %w", lease.Run, err)
		}
		return requireCAS(result, fmt.Errorf("sqlite: heartbeat of run %s generation %d: %w", lease.Run, lease.Generation, app.ErrFenced))
	})
}

// ReleaseLease releases the lease by the same compare-and-swap. The row is
// marked released, never deleted, and the generation is untouched.
func (s *Store) ReleaseLease(ctx context.Context, lease app.Lease) error {
	return s.inWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx,
			`UPDATE run_leases SET state = ? WHERE run_id = ? AND controller_id = ? AND generation = ? AND state = ?`,
			leaseReleased, lease.Run.String(), lease.ControllerID, lease.Generation, leaseHeld,
		)
		if err != nil {
			return fmt.Errorf("sqlite: release lease of run %s: %w", lease.Run, err)
		}
		return requireCAS(result, fmt.Errorf("sqlite: release of run %s generation %d: %w", lease.Run, lease.Generation, app.ErrFenced))
	})
}

// requireCAS turns a zero-row compare-and-swap update into fenced.
func requireCAS(result sql.Result, fenced error) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: rows affected: %w", err)
	}
	if affected == 0 {
		return fenced
	}
	return nil
}

// Begin opens a controller unit of work bound to lease: an immediate write
// transaction that is refused up front when the lease row no longer matches
// (run, controller, generation, held). Commit re-validates, adding the
// expiry check against the commit-time clock reading.
func (s *Store) Begin(ctx context.Context, lease app.Lease) (app.UnitOfWork, error) {
	var (
		tx  *sql.Tx
		err error
	)
	for attempt := range txAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("sqlite: canceled while retrying a busy begin: %w", context.Cause(ctx))
			case <-time.After(time.Duration(attempt) * txBackoff):
			}
		}
		tx, err = s.writes.BeginTx(ctx, nil)
		if err == nil || !isBusy(err) {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin unit of work: %w", err)
	}
	if err := validateLease(ctx, tx, lease, time.Time{}); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return nil, errors.Join(err, rbErr)
		}
		return nil, err
	}
	return &unitOfWork{store: s, tx: tx, ctx: ctx, lease: lease}, nil
}

// validateLease re-reads the lease row inside tx and fails with ErrFenced
// unless it still matches (run, controller, generation, held). A non-zero
// now additionally requires the lease unexpired: expires_at earlier than
// now, on parsed times, is fenced.
func validateLease(ctx context.Context, tx *sql.Tx, lease app.Lease, now time.Time) error {
	var (
		holder, state, expiresAt string
		generation               int64
	)
	err := tx.QueryRowContext(ctx, selectLease, lease.Run.String()).Scan(&holder, &generation, &state, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("sqlite: lease of run %s: %w", lease.Run, app.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("sqlite: re-read lease of run %s: %w", lease.Run, err)
	}
	if holder != lease.ControllerID || generation != lease.Generation || state != leaseHeld {
		return fmt.Errorf("sqlite: run %s lease is %s by %q at generation %d: %w", lease.Run, state, holder, generation, app.ErrFenced)
	}
	if now.IsZero() {
		return nil
	}
	expires, err := parseTime(expiresAt)
	if err != nil {
		return err
	}
	if expires.Before(now) {
		return fmt.Errorf("sqlite: run %s lease generation %d expired at %s: %w", lease.Run, lease.Generation, expiresAt, app.ErrFenced)
	}
	return nil
}
