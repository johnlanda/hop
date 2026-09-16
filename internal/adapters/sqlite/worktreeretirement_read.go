package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

var _ app.RetirementReadStore = (*Store)(nil)

// ListRetirementCandidates is the worktree-retirement triage read, inside
// one read transaction: the repository's terminal runs with the
// worktrees-retired fact unset whose frozen workflow is feature mode with
// a target branch and that integrated at least one row adding content, in
// sequence order, each with its integrated rows (oldest first), its
// retirement.check operations (newest first) and whether any retirement
// operation is pending or reconciling. An unknown root has no candidates.
func (s *Store) ListRetirementCandidates(ctx context.Context, repositoryRoot string) ([]app.RetirementCandidateRecord, error) {
	var records []app.RetirementCandidateRecord
	err := s.inReadTx(ctx, func(tx *sql.Tx) error {
		var repositoryID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM repositories WHERE root_path = ?`, repositoryRoot).Scan(&repositoryID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("sqlite: resolve repository root: %w", err)
		}
		candidates, err := terminalUnretiredRuns(ctx, tx, repositoryID)
		if err != nil {
			return err
		}
		for i := range candidates {
			record, eligible, recordErr := retirementCandidateRecord(ctx, tx, candidates[i], repositoryRoot)
			if recordErr != nil {
				return recordErr
			}
			if eligible {
				records = append(records, record)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// terminalUnretiredRuns lists the repository's completed, failed and
// stopped runs whose worktrees-retired fact is unset, in sequence order.
func terminalUnretiredRuns(ctx context.Context, q querier, repositoryID string) ([]app.RetirementCandidateRecord, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, seq FROM runs WHERE repository_id = ? AND state IN (?, ?, ?) AND worktrees_retired_at IS NULL ORDER BY seq`,
		repositoryID, string(run.RunCompleted), string(run.RunFailed), string(run.RunStopped),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list terminal runs: %w", err)
	}
	defer rows.Close() //nolint:errcheck // the deferred close of a fully-iterated read cursor has no failure the rows.Err check below misses.
	var runs []app.RetirementCandidateRecord
	for rows.Next() {
		var (
			id  string
			seq int64
		)
		if scanErr := rows.Scan(&id, &seq); scanErr != nil {
			return nil, fmt.Errorf("sqlite: scan terminal run row: %w", scanErr)
		}
		runID, parseErr := identity.ParseRunID(id)
		if parseErr != nil {
			return nil, fmt.Errorf("sqlite: terminal run id: %w", parseErr)
		}
		runs = append(runs, app.RetirementCandidateRecord{RunID: runID, Sequence: int(seq)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate terminal runs: %w", err)
	}
	return runs, nil
}

// retirementCandidateRecord completes one terminal run's record; eligible
// is false for a solo run, a run frozen without a target, and a run that
// integrated no new content.
func retirementCandidateRecord(ctx context.Context, q querier, record app.RetirementCandidateRecord, repositoryRoot string) (app.RetirementCandidateRecord, bool, error) { //nolint:gocritic // hugeParam: the record is built up and returned by value once per candidate.
	snapshot, err := loadSnapshot(ctx, q, record.RunID)
	if err != nil {
		return record, false, err
	}
	if !snapshot.Workflow.Feature() || snapshot.Workflow.TargetBranch == "" {
		return record, false, nil
	}
	record.RepositoryRoot, record.TargetBranch = repositoryRoot, snapshot.Workflow.TargetBranch

	integrated, err := q.QueryContext(ctx,
		selectIntegrationColumns+` WHERE run_id = ? AND state = ? ORDER BY created_at, rowid`,
		record.RunID.String(), string(run.IntegrationIntegrated),
	)
	if err != nil {
		return record, false, fmt.Errorf("sqlite: list integrated rows of run %s: %w", record.RunID, err)
	}
	if record.Integrated, err = collectIntegrations(integrated); err != nil {
		return record, false, err
	}
	content := false
	for i := range record.Integrated {
		content = content || record.Integrated[i].MergeCommitOID != record.Integrated[i].PremergeHeadOID
	}
	if !content {
		return record, false, nil
	}

	if record.Checks, err = operationsByKind(ctx, q, record.RunID, app.OpRetirementCheck); err != nil {
		return record, false, err
	}
	var unresolved int64
	if err := q.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM operations WHERE run_id = ? AND kind IN (?, ?) AND state IN (?, ?))`,
		record.RunID.String(), string(app.OpRetirementCheck), string(app.OpWorktreeRetire),
		string(app.OperationPending), string(app.OperationReconciling),
	).Scan(&unresolved); err != nil {
		return record, false, fmt.Errorf("sqlite: unresolved retirement operations of run %s: %w", record.RunID, err)
	}
	record.Unresolved = unresolved != 0
	return record, true, nil
}

// collectIntegrations drains one integrations query into decoded rows.
func collectIntegrations(rows *sql.Rows) ([]run.Integration, error) {
	defer rows.Close() //nolint:errcheck // the deferred close of a fully-iterated read cursor has no failure the rows.Err check below misses.
	var integrations []run.Integration
	for rows.Next() {
		integration, _, err := scanIntegration(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan integration row: %w", err)
		}
		integrations = append(integrations, integration)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate integration rows: %w", err)
	}
	return integrations, nil
}
