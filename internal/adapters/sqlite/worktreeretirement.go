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
