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

// querier is the shared query surface of *sql.DB and *sql.Tx, so entity
// scanners serve controller transactions, worker transactions and
// lease-free reads alike.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// terminalSessionStates are the session states that end a session; a
// session in any other state is the attempt's current session.
const terminalSessionStates = `('lost', 'terminated')`

// getRun loads one run and its revision.
func getRun(ctx context.Context, q querier, id identity.RunID) (run.Run, int64, error) {
	var (
		repositoryID, briefDigest, state, updatedAt string
		stopRequestedAt                             sql.NullString
		seq, revision                               int64
	)
	err := q.QueryRowContext(ctx,
		`SELECT repository_id, seq, brief_digest, state, stop_requested_at, revision, updated_at FROM runs WHERE id = ?`,
		id.String(),
	).Scan(&repositoryID, &seq, &briefDigest, &state, &stopRequestedAt, &revision, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return run.Run{}, 0, fmt.Errorf("sqlite: run %s: %w", id, app.ErrNotFound)
	}
	if err != nil {
		return run.Run{}, 0, fmt.Errorf("sqlite: load run %s: %w", id, err)
	}
	repoID, err := identity.ParseRepositoryID(repositoryID)
	if err != nil {
		return run.Run{}, 0, fmt.Errorf("sqlite: run %s repository id: %w", id, err)
	}
	updated, err := parseTime(updatedAt)
	if err != nil {
		return run.Run{}, 0, err
	}
	return run.Run{
		ID:            id,
		RepositoryID:  repoID,
		Sequence:      int(seq),
		BriefDigest:   briefDigest,
		State:         run.RunState(state),
		StopRequested: stopRequestedAt.Valid,
		UpdatedAt:     updated,
	}, revision, nil
}

// getTask loads one task and its revision.
func getTask(ctx context.Context, q querier, id identity.TaskID) (run.Task, int64, error) {
	var (
		runID, instructionsDigest, state, updatedAt string
		revision                                    int64
	)
	err := q.QueryRowContext(ctx,
		`SELECT run_id, instructions_digest, state, revision, updated_at FROM tasks WHERE id = ?`,
		id.String(),
	).Scan(&runID, &instructionsDigest, &state, &revision, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return run.Task{}, 0, fmt.Errorf("sqlite: task %s: %w", id, app.ErrNotFound)
	}
	if err != nil {
		return run.Task{}, 0, fmt.Errorf("sqlite: load task %s: %w", id, err)
	}
	parsedRunID, err := identity.ParseRunID(runID)
	if err != nil {
		return run.Task{}, 0, fmt.Errorf("sqlite: task %s run id: %w", id, err)
	}
	updated, err := parseTime(updatedAt)
	if err != nil {
		return run.Task{}, 0, err
	}
	return run.Task{
		ID:                 id,
		RunID:              parsedRunID,
		InstructionsDigest: instructionsDigest,
		State:              run.TaskState(state),
		UpdatedAt:          updated,
	}, revision, nil
}

// taskByRun loads the run's single Phase 2 task and its revision.
func taskByRun(ctx context.Context, q querier, runID identity.RunID) (run.Task, int64, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM tasks WHERE run_id = ?`, runID.String()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return run.Task{}, 0, fmt.Errorf("sqlite: task of run %s: %w", runID, app.ErrNotFound)
	}
	if err != nil {
		return run.Task{}, 0, fmt.Errorf("sqlite: load task of run %s: %w", runID, err)
	}
	taskID, err := identity.ParseTaskID(id)
	if err != nil {
		return run.Task{}, 0, fmt.Errorf("sqlite: task id of run %s: %w", runID, err)
	}
	return getTask(ctx, q, taskID)
}

// getAttempt loads one attempt and its revision.
func getAttempt(ctx context.Context, q querier, id identity.AttemptID) (run.Attempt, int64, error) {
	var (
		taskID, state, updatedAt string
		number, revision         int64
	)
	err := q.QueryRowContext(ctx,
		`SELECT task_id, number, state, revision, updated_at FROM attempts WHERE id = ?`,
		id.String(),
	).Scan(&taskID, &number, &state, &revision, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return run.Attempt{}, 0, fmt.Errorf("sqlite: attempt %s: %w", id, app.ErrNotFound)
	}
	if err != nil {
		return run.Attempt{}, 0, fmt.Errorf("sqlite: load attempt %s: %w", id, err)
	}
	parsedTaskID, err := identity.ParseTaskID(taskID)
	if err != nil {
		return run.Attempt{}, 0, fmt.Errorf("sqlite: attempt %s task id: %w", id, err)
	}
	updated, err := parseTime(updatedAt)
	if err != nil {
		return run.Attempt{}, 0, err
	}
	return run.Attempt{
		ID:        id,
		TaskID:    parsedTaskID,
		Number:    int(number),
		State:     run.AttemptState(state),
		UpdatedAt: updated,
	}, revision, nil
}

// latestAttempt loads the task's highest-numbered attempt and its revision.
func latestAttempt(ctx context.Context, q querier, taskID identity.TaskID) (run.Attempt, int64, error) {
	var id string
	err := q.QueryRowContext(ctx,
		`SELECT id FROM attempts WHERE task_id = ? ORDER BY number DESC LIMIT 1`,
		taskID.String(),
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return run.Attempt{}, 0, fmt.Errorf("sqlite: attempt of task %s: %w", taskID, app.ErrNotFound)
	}
	if err != nil {
		return run.Attempt{}, 0, fmt.Errorf("sqlite: load attempt of task %s: %w", taskID, err)
	}
	attemptID, err := identity.ParseAttemptID(id)
	if err != nil {
		return run.Attempt{}, 0, fmt.Errorf("sqlite: attempt id of task %s: %w", taskID, err)
	}
	return getAttempt(ctx, q, attemptID)
}

// scanSession maps one sessions row.
func scanSession(row *sql.Row, id identity.SessionID) (run.Session, int64, error) {
	var (
		runID, attemptID, role, harness, state, updatedAt string
		nativeRef, nativeRefSource                        sql.NullString
		revision                                          int64
	)
	err := row.Scan(&runID, &attemptID, &role, &harness, &nativeRef, &nativeRefSource, &state, &revision, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return run.Session{}, 0, fmt.Errorf("sqlite: session %s: %w", id, app.ErrNotFound)
	}
	if err != nil {
		return run.Session{}, 0, fmt.Errorf("sqlite: load session %s: %w", id, err)
	}
	parsedRunID, err := identity.ParseRunID(runID)
	if err != nil {
		return run.Session{}, 0, fmt.Errorf("sqlite: session %s run id: %w", id, err)
	}
	parsedAttemptID, err := identity.ParseAttemptID(attemptID)
	if err != nil {
		return run.Session{}, 0, fmt.Errorf("sqlite: session %s attempt id: %w", id, err)
	}
	updated, err := parseTime(updatedAt)
	if err != nil {
		return run.Session{}, 0, err
	}
	return run.Session{
		ID:               id,
		RunID:            parsedRunID,
		AttemptID:        parsedAttemptID,
		Role:             run.Role(role),
		Harness:          run.Harness(harness),
		NativeSessionRef: nativeRef.String,
		NativeRefSource:  run.NativeRefSource(nativeRefSource.String),
		State:            run.SessionState(state),
		UpdatedAt:        updated,
	}, revision, nil
}

const selectSessionColumns = `SELECT run_id, attempt_id, role, harness, native_session_ref, native_ref_source, state, revision, updated_at FROM sessions`

// getSession loads one session and its revision.
func getSession(ctx context.Context, q querier, id identity.SessionID) (run.Session, int64, error) {
	return scanSession(q.QueryRowContext(ctx, selectSessionColumns+` WHERE id = ?`, id.String()), id)
}

// currentSession loads the attempt's non-terminated session, when one
// exists; ok is false when every session of the attempt has ended.
func currentSession(ctx context.Context, q querier, attemptID identity.AttemptID) (session run.Session, revision int64, ok bool, err error) {
	var id string
	err = q.QueryRowContext(ctx,
		`SELECT id FROM sessions WHERE attempt_id = ? AND state NOT IN `+terminalSessionStates,
		attemptID.String(),
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return run.Session{}, 0, false, nil
	}
	if err != nil {
		return run.Session{}, 0, false, fmt.Errorf("sqlite: load current session of attempt %s: %w", attemptID, err)
	}
	sessionID, err := identity.ParseSessionID(id)
	if err != nil {
		return run.Session{}, 0, false, fmt.Errorf("sqlite: current session id of attempt %s: %w", attemptID, err)
	}
	session, revision, err = getSession(ctx, q, sessionID)
	if err != nil {
		return run.Session{}, 0, false, err
	}
	return session, revision, true, nil
}

// getWorktree loads one worktree and its revision.
func getWorktree(ctx context.Context, q querier, where, arg string) (run.Worktree, int64, error) {
	var (
		id, repositoryID, runID, path, branch, state string
		revision                                     int64
	)
	err := q.QueryRowContext(ctx,
		`SELECT id, repository_id, run_id, path, branch, state, revision FROM worktrees WHERE `+where+` = ?`,
		arg,
	).Scan(&id, &repositoryID, &runID, &path, &branch, &state, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return run.Worktree{}, 0, fmt.Errorf("sqlite: worktree with %s %s: %w", where, arg, app.ErrNotFound)
	}
	if err != nil {
		return run.Worktree{}, 0, fmt.Errorf("sqlite: load worktree with %s %s: %w", where, arg, err)
	}
	worktreeID, err := identity.ParseWorktreeID(id)
	if err != nil {
		return run.Worktree{}, 0, fmt.Errorf("sqlite: worktree id: %w", err)
	}
	repoID, err := identity.ParseRepositoryID(repositoryID)
	if err != nil {
		return run.Worktree{}, 0, fmt.Errorf("sqlite: worktree %s repository id: %w", id, err)
	}
	parsedRunID, err := identity.ParseRunID(runID)
	if err != nil {
		return run.Worktree{}, 0, fmt.Errorf("sqlite: worktree %s run id: %w", id, err)
	}
	return run.Worktree{
		ID:           worktreeID,
		RepositoryID: repoID,
		RunID:        parsedRunID,
		Path:         path,
		Branch:       branch,
		State:        run.WorktreeState(state),
	}, revision, nil
}

// acceptedResult loads the attempt's accepted result, or nil when none has
// been accepted yet.
func acceptedResult(ctx context.Context, q querier, attemptID identity.AttemptID) (*run.Result, error) {
	var id, commitOID, summary, contentDigest, submittedAt string
	err := q.QueryRowContext(ctx,
		`SELECT id, commit_oid, summary, content_digest, submitted_at FROM results WHERE attempt_id = ? AND accepted = 1`,
		attemptID.String(),
	).Scan(&id, &commitOID, &summary, &contentDigest, &submittedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // a nil result with a nil error is the port's documented "no accepted result yet" value.
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: load accepted result of attempt %s: %w", attemptID, err)
	}
	resultID, err := identity.ParseResultID(id)
	if err != nil {
		return nil, fmt.Errorf("sqlite: accepted result id of attempt %s: %w", attemptID, err)
	}
	submitted, err := parseTime(submittedAt)
	if err != nil {
		return nil, err
	}
	return &run.Result{
		ID:            resultID,
		AttemptID:     attemptID,
		CommitOID:     commitOID,
		Summary:       summary,
		ContentDigest: contentDigest,
		Accepted:      true,
		SubmittedAt:   submitted,
	}, nil
}

// occupantJSON is the stored form of run.OccupantEvidence.
type occupantJSON struct {
	Label      string `json:"label"`
	ArgvMarker string `json:"argv_marker"`
	PID        int    `json:"pid"`
}

const selectBindingColumns = `SELECT session_id, incarnation_id, server_socket_path, workspace_id, tab_id, pane_id, creation_label, launch_kind, occupant_evidence, observed_at, superseded, superseded_at, superseded_evidence FROM runtime_bindings`

// scanBinding maps one runtime_bindings row.
func scanBinding(row *sql.Row) (run.RuntimeBinding, error) {
	var (
		sessionID, incarnationID, socketPath, workspaceID, tabID, paneID, label, kind, observedAt string
		occupant, supersededAt, supersededEvidence                                                sql.NullString
		superseded                                                                                int64
	)
	err := row.Scan(&sessionID, &incarnationID, &socketPath, &workspaceID, &tabID, &paneID, &label, &kind, &occupant, &observedAt, &superseded, &supersededAt, &supersededEvidence)
	if err != nil {
		return run.RuntimeBinding{}, err
	}
	parsedSessionID, err := identity.ParseSessionID(sessionID)
	if err != nil {
		return run.RuntimeBinding{}, fmt.Errorf("sqlite: binding session id: %w", err)
	}
	parsedIncarnationID, err := identity.ParseIncarnationID(incarnationID)
	if err != nil {
		return run.RuntimeBinding{}, fmt.Errorf("sqlite: binding incarnation id: %w", err)
	}
	observed, err := parseTime(observedAt)
	if err != nil {
		return run.RuntimeBinding{}, err
	}
	binding := run.RuntimeBinding{
		SessionID:          parsedSessionID,
		IncarnationID:      parsedIncarnationID,
		ServerSocketPath:   socketPath,
		WorkspaceID:        workspaceID,
		TabID:              tabID,
		PaneID:             paneID,
		CreationLabel:      label,
		LaunchKind:         run.LaunchKind(kind),
		ObservedAt:         observed,
		Superseded:         superseded != 0,
		SupersededEvidence: supersededEvidence.String,
	}
	if supersededAt.Valid {
		if binding.SupersededAt, err = parseTime(supersededAt.String); err != nil {
			return run.RuntimeBinding{}, err
		}
	}
	if occupant.Valid {
		var stored occupantJSON
		if err := json.Unmarshal([]byte(occupant.String), &stored); err != nil {
			return run.RuntimeBinding{}, fmt.Errorf("sqlite: decode occupant evidence of binding %s/%s: %w", sessionID, incarnationID, err)
		}
		binding.Occupant = &run.OccupantEvidence{Label: stored.Label, ArgvMarker: stored.ArgvMarker, PID: stored.PID}
	}
	return binding, nil
}

// currentBinding loads the session's current (non-superseded) binding, when
// one exists. Among multiple non-superseded rows the newest insertion wins,
// though supersession evidence normally keeps at most one current.
func currentBinding(ctx context.Context, q querier, sessionID identity.SessionID) (run.RuntimeBinding, bool, error) {
	row := q.QueryRowContext(ctx,
		selectBindingColumns+` WHERE session_id = ? AND superseded = 0 ORDER BY rowid DESC LIMIT 1`,
		sessionID.String(),
	)
	binding, err := scanBinding(row)
	if errors.Is(err, sql.ErrNoRows) {
		return run.RuntimeBinding{}, false, nil
	}
	if err != nil {
		return run.RuntimeBinding{}, false, fmt.Errorf("sqlite: load current binding of session %s: %w", sessionID, err)
	}
	return binding, true, nil
}

// getLaunchClaim loads one launch claim, or nil when the incarnation has
// none.
func getLaunchClaim(ctx context.Context, q querier, incarnationID identity.IncarnationID) (*app.LaunchClaim, error) {
	var (
		runID, attemptID, executable, argvDigest, state, claimedAt string
		claimError, settledAt, settlementEvidence                  sql.NullString
		pid                                                        int64
	)
	err := q.QueryRowContext(ctx,
		`SELECT run_id, attempt_id, executable, argv_digest, pid, state, error, claimed_at, settled_at, settlement_evidence FROM launch_claims WHERE incarnation_id = ?`,
		incarnationID.String(),
	).Scan(&runID, &attemptID, &executable, &argvDigest, &pid, &state, &claimError, &claimedAt, &settledAt, &settlementEvidence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // a nil claim with a nil error is the documented "no claim recorded" value.
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: load launch claim of incarnation %s: %w", incarnationID, err)
	}
	parsedRunID, err := identity.ParseRunID(runID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: launch claim %s run id: %w", incarnationID, err)
	}
	parsedAttemptID, err := identity.ParseAttemptID(attemptID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: launch claim %s attempt id: %w", incarnationID, err)
	}
	claimed, err := parseTime(claimedAt)
	if err != nil {
		return nil, err
	}
	var settled time.Time
	if settledAt.Valid {
		if settled, err = parseTime(settledAt.String); err != nil {
			return nil, err
		}
	}
	return &app.LaunchClaim{
		IncarnationID:      incarnationID,
		RunID:              parsedRunID,
		AttemptID:          parsedAttemptID,
		Executable:         executable,
		ArgvDigest:         argvDigest,
		PID:                int(pid),
		State:              app.LaunchClaimState(state),
		Error:              claimError.String,
		ClaimedAt:          claimed,
		SettledAt:          settled,
		SettlementEvidence: settlementEvidence.String,
	}, nil
}

// incarnationCurrent reports whether incarnationID is the attempt's current
// identity: the attempt has a non-terminated session whose current
// (non-superseded) binding carries exactly this incarnation.
func incarnationCurrent(ctx context.Context, q querier, attemptID identity.AttemptID, incarnationID identity.IncarnationID) (bool, error) {
	session, _, ok, err := currentSession(ctx, q, attemptID)
	if err != nil || !ok {
		return false, err
	}
	binding, ok, err := currentBinding(ctx, q, session.ID)
	if err != nil || !ok {
		return false, err
	}
	return binding.IncarnationID == incarnationID, nil
}

// launchIncarnationCurrent decides ClaimLaunch's currency rule, which must
// also work in the window before the controller records the binding row:
// the launcher is the pane's own command and can claim first. The
// incarnation is current iff (a) the attempt's current session has a
// current binding carrying exactly this incarnation, or (b) no binding row
// exists yet for that session — a superseded row without a successor
// retires the incarnation, so any row at all disables the fallback — and
// the run's newest pending launch operation (kind pane.open or
// launch.send) carries the claim's incarnation in its intent JSON under
// the documented "incarnation_id" key, which the controller commits before
// dispatching the pane request. Any other case is not current.
func launchIncarnationCurrent(ctx context.Context, q querier, runID identity.RunID, attemptID identity.AttemptID, incarnationID identity.IncarnationID) (bool, error) {
	session, _, ok, err := currentSession(ctx, q, attemptID)
	if err != nil || !ok {
		return false, err
	}
	binding, ok, err := currentBinding(ctx, q, session.ID)
	if err != nil {
		return false, err
	}
	if ok {
		return binding.IncarnationID == incarnationID, nil
	}
	var bindingRows int64
	if countErr := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runtime_bindings WHERE session_id = ?`, session.ID.String(),
	).Scan(&bindingRows); countErr != nil {
		return false, fmt.Errorf("sqlite: count bindings of session %s: %w", session.ID, countErr)
	}
	if bindingRows > 0 {
		return false, nil
	}
	var intentIncarnation sql.NullString
	err = q.QueryRowContext(ctx,
		`SELECT json_extract(intent, '$.incarnation_id') FROM operations
		 WHERE run_id = ? AND state = ? AND kind IN (?, ?)
		 ORDER BY created_at DESC, rowid DESC LIMIT 1`,
		runID.String(), string(app.OperationPending), string(app.OpPaneOpen), string(app.OpLaunchSend),
	).Scan(&intentIncarnation)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sqlite: read pending launch intent of run %s: %w", runID, err)
	}
	return intentIncarnation.Valid && intentIncarnation.String == incarnationID.String(), nil
}
