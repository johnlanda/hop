package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

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
	var planClosedAt sql.NullString
	err := q.QueryRowContext(ctx,
		`SELECT repository_id, seq, brief_digest, state, stop_requested_at, plan_closed_at, revision, updated_at FROM runs WHERE id = ?`,
		id.String(),
	).Scan(&repositoryID, &seq, &briefDigest, &state, &stopRequestedAt, &planClosedAt, &revision, &updatedAt)
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
		PlanClosed:    planClosedAt.Valid,
		UpdatedAt:     updated,
	}, revision, nil
}

// selectTaskColumns selects everything scanTask maps, including the
// has-dependencies fact derived from the task's persisted edge set (the
// domain field is fixed at creation; the edges are its ground truth) and
// the mailbox flag derived from mailbox_closed_at.
const selectTaskColumns = `SELECT t.id, t.run_id, t.kind, t.seq, t.title, t.instructions_digest,
	EXISTS (SELECT 1 FROM task_dependencies td WHERE td.task_id = t.id),
	t.subject_commit_oid, t.subject_tree_oid, t.mailbox_closed_at, t.state, t.revision, t.updated_at
	FROM tasks t`

// scanTask maps one tasks row from a row scanner.
func scanTask(scan func(dest ...any) error) (run.Task, int64, error) {
	var (
		id, runID, kind, title, instructionsDigest, state, updatedAt string
		subjectCommit, subjectTree, mailboxClosedAt                  sql.NullString
		seq, hasDependencies, revision                               int64
	)
	if err := scan(&id, &runID, &kind, &seq, &title, &instructionsDigest, &hasDependencies, &subjectCommit, &subjectTree, &mailboxClosedAt, &state, &revision, &updatedAt); err != nil {
		return run.Task{}, 0, err
	}
	taskID, err := identity.ParseTaskID(id)
	if err != nil {
		return run.Task{}, 0, fmt.Errorf("sqlite: task id: %w", err)
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
		ID:                 taskID,
		RunID:              parsedRunID,
		Kind:               run.TaskKind(kind),
		Seq:                int(seq),
		Title:              title,
		InstructionsDigest: instructionsDigest,
		HasDependencies:    hasDependencies != 0,
		SubjectCommitOID:   subjectCommit.String,
		SubjectTreeOID:     subjectTree.String,
		MailboxClosed:      mailboxClosedAt.Valid,
		State:              run.TaskState(state),
		UpdatedAt:          updated,
	}, revision, nil
}

// getTask loads one task and its revision.
func getTask(ctx context.Context, q querier, id identity.TaskID) (run.Task, int64, error) {
	task, revision, err := scanTask(q.QueryRowContext(ctx, selectTaskColumns+` WHERE t.id = ?`, id.String()).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return run.Task{}, 0, fmt.Errorf("sqlite: task %s: %w", id, app.ErrNotFound)
	}
	if err != nil {
		return run.Task{}, 0, fmt.Errorf("sqlite: load task %s: %w", id, err)
	}
	return task, revision, nil
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

// scanSession maps one sessions row. attempt_id and parent_session_id are
// NULL for the roles that carry none (a manager binds neither) and map to
// the domain's empty AttemptID and nil ParentSessionID.
func scanSession(row *sql.Row, id identity.SessionID) (run.Session, int64, error) {
	var (
		runID, role, harness, state, updatedAt          string
		attemptID, parentID, nativeRef, nativeRefSource sql.NullString
		revision                                        int64
	)
	err := row.Scan(&runID, &attemptID, &parentID, &role, &harness, &nativeRef, &nativeRefSource, &state, &revision, &updatedAt)
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
	session := run.Session{
		ID:               id,
		RunID:            parsedRunID,
		Role:             run.Role(role),
		Harness:          run.Harness(harness),
		NativeSessionRef: nativeRef.String,
		NativeRefSource:  run.NativeRefSource(nativeRefSource.String),
		State:            run.SessionState(state),
	}
	if attemptID.Valid {
		parsedAttemptID, attemptErr := identity.ParseAttemptID(attemptID.String)
		if attemptErr != nil {
			return run.Session{}, 0, fmt.Errorf("sqlite: session %s attempt id: %w", id, attemptErr)
		}
		session.AttemptID = parsedAttemptID
	}
	if parentID.Valid {
		parsedParentID, parentErr := identity.ParseSessionID(parentID.String)
		if parentErr != nil {
			return run.Session{}, 0, fmt.Errorf("sqlite: session %s parent session id: %w", id, parentErr)
		}
		session.ParentSessionID = &parsedParentID
	}
	if session.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return run.Session{}, 0, err
	}
	return session, revision, nil
}

const selectSessionColumns = `SELECT run_id, attempt_id, parent_session_id, role, harness, native_session_ref, native_ref_source, state, revision, updated_at FROM sessions`

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

// selectWorktreeColumns is the worktree row every loader scans with
// scanWorktree.
const selectWorktreeColumns = `SELECT id, repository_id, run_id, attempt_id, base_commit, path, branch, state, revision FROM worktrees`

// getWorktree loads one worktree and its revision. attempt_id and
// base_commit are NULL for a solo row and map to the domain's empty
// values.
func getWorktree(ctx context.Context, q querier, where, arg string) (run.Worktree, int64, error) {
	worktree, revision, err := scanWorktree(q.QueryRowContext(ctx, selectWorktreeColumns+` WHERE `+where+` = ?`, arg).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return run.Worktree{}, 0, fmt.Errorf("sqlite: worktree with %s %s: %w", where, arg, app.ErrNotFound)
	}
	if err != nil {
		return run.Worktree{}, 0, fmt.Errorf("sqlite: load worktree with %s %s: %w", where, arg, err)
	}
	return worktree, revision, nil
}

// scanWorktree maps one selectWorktreeColumns row through scan, passing a
// scan error (sql.ErrNoRows included) through unwrapped.
func scanWorktree(scan func(dest ...any) error) (run.Worktree, int64, error) {
	var (
		id, repositoryID, runID, path, branch, state string
		attemptID, baseCommit                        sql.NullString
		revision                                     int64
	)
	if err := scan(&id, &repositoryID, &runID, &attemptID, &baseCommit, &path, &branch, &state, &revision); err != nil {
		return run.Worktree{}, 0, err
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
	worktree := run.Worktree{
		ID:           worktreeID,
		RepositoryID: repoID,
		RunID:        parsedRunID,
		BaseCommit:   baseCommit.String,
		Path:         path,
		Branch:       branch,
		State:        run.WorktreeState(state),
	}
	if attemptID.Valid {
		parsedAttemptID, attemptErr := identity.ParseAttemptID(attemptID.String)
		if attemptErr != nil {
			return run.Worktree{}, 0, fmt.Errorf("sqlite: worktree %s attempt id: %w", id, attemptErr)
		}
		worktree.AttemptID = parsedAttemptID
	}
	return worktree, revision, nil
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

const selectBindingColumns = `SELECT session_id, incarnation_id, server_socket_path, server_instance, workspace_id, tab_id, pane_id, creation_label, launch_kind, occupant_evidence, observed_at, superseded, superseded_at, superseded_evidence FROM runtime_bindings`

// scanBinding maps one runtime_bindings row. server_instance stores NULL
// for an empty ServerInstance and round-trips it back as "".
func scanBinding(row *sql.Row) (run.RuntimeBinding, error) {
	var (
		sessionID, incarnationID, socketPath, workspaceID, tabID, paneID, label, kind, observedAt string
		serverInstance, occupant, supersededAt, supersededEvidence                                sql.NullString
		superseded                                                                                int64
	)
	err := row.Scan(&sessionID, &incarnationID, &socketPath, &serverInstance, &workspaceID, &tabID, &paneID, &label, &kind, &occupant, &observedAt, &superseded, &supersededAt, &supersededEvidence)
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
		ServerInstance:     serverInstance.String,
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
		runID, sessionID, executable, argvDigest, state, claimedAt         string
		attemptID, claimError, settledAt, settlementEvidence, seedEvidence sql.NullString
		pid                                                                int64
	)
	err := q.QueryRowContext(ctx,
		`SELECT run_id, session_id, attempt_id, executable, argv_digest, pid, state, error, claimed_at, settled_at, settlement_evidence, seed_evidence FROM launch_claims WHERE incarnation_id = ?`,
		incarnationID.String(),
	).Scan(&runID, &sessionID, &attemptID, &executable, &argvDigest, &pid, &state, &claimError, &claimedAt, &settledAt, &settlementEvidence, &seedEvidence)
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
	parsedSessionID, err := identity.ParseSessionID(sessionID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: launch claim %s session id: %w", incarnationID, err)
	}
	claim := &app.LaunchClaim{
		IncarnationID:      incarnationID,
		RunID:              parsedRunID,
		SessionID:          parsedSessionID,
		Executable:         executable,
		ArgvDigest:         argvDigest,
		PID:                int(pid),
		State:              app.LaunchClaimState(state),
		Error:              claimError.String,
		SettlementEvidence: settlementEvidence.String,
		SeedEvidence:       seedEvidence.String,
	}
	if attemptID.Valid {
		parsedAttemptID, attemptErr := identity.ParseAttemptID(attemptID.String)
		if attemptErr != nil {
			return nil, fmt.Errorf("sqlite: launch claim %s attempt id: %w", incarnationID, attemptErr)
		}
		claim.AttemptID = parsedAttemptID
	}
	if claim.ClaimedAt, err = parseTime(claimedAt); err != nil {
		return nil, err
	}
	if settledAt.Valid {
		if claim.SettledAt, err = parseTime(settledAt.String); err != nil {
			return nil, err
		}
	}
	return claim, nil
}

// incarnationCurrent reports whether incarnationID is the attempt's current
// identity: the attempt's non-terminated session holds it under the one
// principal-incarnation rule (sessionIncarnationCurrent).
func incarnationCurrent(ctx context.Context, q querier, attemptID identity.AttemptID, incarnationID identity.IncarnationID) (bool, error) {
	session, _, ok, err := currentSession(ctx, q, attemptID)
	if err != nil || !ok {
		return false, err
	}
	return sessionIncarnationCurrent(ctx, q, session.ID, incarnationID)
}

// sessionIncarnationCurrent is the one principal-incarnation currency rule
// (docs/plan/phase-3-design.md section 7). Every worker-authority write
// that validates its caller's incarnation decides through it: ClaimLaunch,
// the plan verbs, message send, fetch and ack, review submission and
// result submission, solo and feature alike. It must also hold in the
// window before the controller records the binding row: the launcher is
// the pane's own command and can claim first, and a principal whose
// pane.open outcome the controller never recorded still runs. The
// incarnation is current for sessionID iff
//
//   - the session's current (non-superseded) binding carries exactly this
//     incarnation, and no pending launch intent of the session names a
//     different one — a binding and a pending intent that disagree fail
//     closed, as the launch context does; or
//   - no binding row exists for the session at all — a superseded row
//     without a successor retires the incarnation, so any row disables the
//     fallback — and the SESSION's newest pending launch operation (kind
//     pane.open or launch.send) carries this incarnation in its intent
//     JSON under the documented "incarnation_id" key, matched to the
//     session by the "session_id" key the controller commits before
//     dispatching the pane request.
//
// Keying the intent lookup to the session (never the run's newest pending
// launch) lets two concurrently pending launches validate independently:
// another session's later intent can no longer invalidate this session's
// own (B2).
//
// The intent source is state pending only (pendingLaunchIntentOfSession).
// A pane.open the controller recorded reconciling — its act returned an
// error after the launcher had already claimed — is not read, so its
// principal is stale until label recovery commits the binding (LAUNCH-6,
// an accepted residual; widening it would widen ClaimLaunch too).
func sessionIncarnationCurrent(ctx context.Context, q querier, sessionID identity.SessionID, incarnationID identity.IncarnationID) (bool, error) {
	binding, hasBinding, err := currentBinding(ctx, q, sessionID)
	if err != nil {
		return false, err
	}
	intentIncarnation, hasIntent, err := pendingLaunchIntentOfSession(ctx, q, sessionID)
	if err != nil {
		return false, err
	}
	if hasBinding {
		if hasIntent && intentIncarnation != binding.IncarnationID.String() {
			return false, nil
		}
		return binding.IncarnationID == incarnationID, nil
	}
	var bindingRows int64
	if countErr := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runtime_bindings WHERE session_id = ?`, sessionID.String(),
	).Scan(&bindingRows); countErr != nil {
		return false, fmt.Errorf("sqlite: count bindings of session %s: %w", sessionID, countErr)
	}
	if bindingRows > 0 {
		return false, nil
	}
	return hasIntent && intentIncarnation == incarnationID.String(), nil
}

// pendingLaunchIntentOfSession reads the SESSION's newest pending launch
// operation (kind pane.open or launch.send, matched on the intent JSON's
// "session_id" key) and returns its intent's "incarnation_id"; ok is false
// when the session has no pending launch operation.
func pendingLaunchIntentOfSession(ctx context.Context, q querier, sessionID identity.SessionID) (incarnationID string, ok bool, err error) {
	var intentIncarnation sql.NullString
	err = q.QueryRowContext(ctx,
		`SELECT json_extract(intent, '$.incarnation_id') FROM operations
		 WHERE state = ? AND kind IN (?, ?) AND json_extract(intent, '$.session_id') = ?
		 ORDER BY created_at DESC, rowid DESC LIMIT 1`,
		string(app.OperationPending), string(app.OpPaneOpen), string(app.OpLaunchSend), sessionID.String(),
	).Scan(&intentIncarnation)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("sqlite: read pending launch intent of session %s: %w", sessionID, err)
	}
	return intentIncarnation.String, true, nil
}

// mailboxClear reports whether task's mailbox has no queued or
// delivered-unacknowledged message: the section 5 drain-then-submit
// contract. Vacuously true for a solo task, which no message ever
// addresses.
func mailboxClear(ctx context.Context, q querier, taskID identity.TaskID) (bool, error) {
	var pending int64
	err := q.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM messages m
		  WHERE m.recipient_address = ?
		    AND NOT EXISTS (SELECT 1 FROM message_acks a WHERE a.message_id = m.id))`,
		app.AddressString(run.TaskAddress(taskID)),
	).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("sqlite: read mailbox of task %s: %w", taskID, err)
	}
	return pending == 0, nil
}

// runOfTask returns the persisted owning run of a task row.
func runOfTask(ctx context.Context, q querier, id identity.TaskID) (identity.RunID, error) {
	return ownerRun(ctx, q, `SELECT run_id FROM tasks WHERE id = ?`, "task", id.String())
}

// runOfAttempt returns the persisted owning run of an attempt row, through
// its task.
func runOfAttempt(ctx context.Context, q querier, id identity.AttemptID) (identity.RunID, error) {
	return ownerRun(ctx, q, `SELECT t.run_id FROM attempts a JOIN tasks t ON a.task_id = t.id WHERE a.id = ?`, "attempt", id.String())
}

// runOfSession returns the persisted owning run of a session row.
func runOfSession(ctx context.Context, q querier, id identity.SessionID) (identity.RunID, error) {
	return ownerRun(ctx, q, `SELECT run_id FROM sessions WHERE id = ?`, "session", id.String())
}

// runOfWorktree returns the persisted owning run of a worktree row.
func runOfWorktree(ctx context.Context, q querier, id identity.WorktreeID) (identity.RunID, error) {
	return ownerRun(ctx, q, `SELECT run_id FROM worktrees WHERE id = ?`, "worktree", id.String())
}

// runOfResult returns the persisted owning run of a result row, through
// its attempt and task.
func runOfResult(ctx context.Context, q querier, id identity.ResultID) (identity.RunID, error) {
	return ownerRun(ctx, q, `SELECT t.run_id FROM results r JOIN attempts a ON r.attempt_id = a.id JOIN tasks t ON a.task_id = t.id WHERE r.id = ?`, "result", id.String())
}

// runOfOperationRow returns the persisted owning run of an operation row.
func runOfOperationRow(ctx context.Context, q querier, id identity.OperationID) (identity.RunID, error) {
	return ownerRun(ctx, q, `SELECT run_id FROM operations WHERE id = ?`, "operation", id.String())
}

// ownerRun resolves one entity's persisted owning run through query, which
// must select exactly the run id column for the given entity id.
func ownerRun(ctx context.Context, q querier, query, kind, id string) (identity.RunID, error) {
	var raw string
	err := q.QueryRowContext(ctx, query, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("sqlite: %s %s: %w", kind, id, app.ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("sqlite: resolve owning run of %s %s: %w", kind, id, err)
	}
	runID, err := identity.ParseRunID(raw)
	if err != nil {
		return "", fmt.Errorf("sqlite: owning run of %s %s: %w", kind, id, err)
	}
	return runID, nil
}
