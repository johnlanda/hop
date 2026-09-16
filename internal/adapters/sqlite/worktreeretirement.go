package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
)

var _ app.WorktreeRetirementRepositories = (*unitOfWork)(nil)

// MarkWorktreesRetired sets the leased run's worktrees-retired fact once:
// the UPDATE applies only while the column is NULL, so a repeated call
// keeps the first value. The fact is a set-once housekeeping column
// outside the run's lifecycle, so it does not move runs.revision — no
// revision-checked save decides anything from it.
func (u *unitOfWork) MarkWorktreesRetired(ctx context.Context, runID identity.RunID, at time.Time) error {
	if err := u.requireLeasedRun(runID, "run", runID.String()); err != nil {
		return err
	}
	var exists int
	err := u.tx.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE id = ?`, runID.String()).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("sqlite: run %s: %w", runID, app.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("sqlite: read run %s: %w", runID, err)
	}
	if _, err := u.tx.ExecContext(ctx,
		`UPDATE runs SET worktrees_retired_at = ? WHERE id = ? AND worktrees_retired_at IS NULL`,
		formatTime(at), runID.String(),
	); err != nil {
		return fmt.Errorf("sqlite: record worktrees-retired fact of run %s: %w", runID, err)
	}
	return nil
}

// runWorktreesRetiredAt reads the run's worktrees-retired fact
// (migration 004's runs.worktrees_retired_at): nil when the column is
// NULL, the parsed canonical time otherwise.
func runWorktreesRetiredAt(ctx context.Context, q querier, runID identity.RunID) (*time.Time, error) {
	var at sql.NullString
	err := q.QueryRowContext(ctx, `SELECT worktrees_retired_at FROM runs WHERE id = ?`, runID.String()).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("sqlite: run %s: %w", runID, app.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: read worktrees-retired fact of run %s: %w", runID, err)
	}
	if !at.Valid {
		return nil, nil //nolint:nilnil // a nil time with a nil error is the documented "not retired" value.
	}
	parsed, err := parseTime(at.String)
	if err != nil {
		return nil, fmt.Errorf("sqlite: worktrees-retired fact of run %s: %w", runID, err)
	}
	return &parsed, nil
}
