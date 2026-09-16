package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// The Phase 3 lease-free read capability: the same Store also implements
// app.WorkflowReadStore, so app.RequireWorkflowReadStore succeeds against
// this adapter (the additive packaging rule's read-side half).
var _ app.WorkflowReadStore = (*Store)(nil)

// LoadSessionLaunchContext addresses launch context by session rather than
// by attempt, covering sessions without one (the manager). The incarnation
// resolves exactly like LoadLaunchContext's — the session's current
// binding when one exists, the SESSION-keyed pending launch intent
// otherwise, agreement required when both exist — and fails closed with
// ErrNotFound rather than hand the launcher an identity nothing recorded.
func (s *Store) LoadSessionLaunchContext(ctx context.Context, runID identity.RunID, sessionID identity.SessionID) (app.SessionLaunchContext, error) {
	var out app.SessionLaunchContext
	err := s.inReadTx(ctx, func(tx *sql.Tx) error {
		session, _, err := getSession(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if session.RunID != runID {
			return fmt.Errorf("sqlite: session %s does not belong to run %s: %w", sessionID, runID, app.ErrNotFound)
		}
		runV, _, err := getRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		snapshot, err := loadSnapshot(ctx, tx, runID)
		if err != nil {
			return err
		}
		incarnation, err := sessionLaunchIdentity(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		claim, err := getLaunchClaim(ctx, tx, incarnation)
		if err != nil {
			return err
		}
		out = app.SessionLaunchContext{
			Snapshot:      snapshot,
			Harness:       session.Harness,
			Session:       session,
			AttemptID:     session.AttemptID,
			IncarnationID: incarnation,
			Claim:         claim,
			StopRequested: runV.StopRequested || runV.State == run.RunStopping || runV.State == run.RunStopped,
		}
		if session.AttemptID != "" {
			attempt, _, attemptErr := getAttempt(ctx, tx, session.AttemptID)
			if attemptErr != nil {
				return attemptErr
			}
			out.Attempt = attempt
			if out.WorktreePath, attemptErr = worktreePathForAttempt(ctx, tx, runID, session.AttemptID); attemptErr != nil {
				return attemptErr
			}
		}
		if out.Relaunch, err = sessionIsSuccessor(ctx, tx, &session); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return app.SessionLaunchContext{}, err
	}
	return out, nil
}

// sessionLaunchIdentity resolves the incarnation HOP_INCARNATION_ID must
// match for one session-addressed launch: the session's current binding
// when one exists, the session's newest pending launch intent otherwise,
// and when both exist they must agree. A disagreement, a malformed intent
// identity or neither source fails closed with ErrNotFound.
func sessionLaunchIdentity(ctx context.Context, q querier, sessionID identity.SessionID) (identity.IncarnationID, error) {
	binding, hasBinding, err := currentBinding(ctx, q, sessionID)
	if err != nil {
		return "", err
	}
	rawIntent, hasIntent, err := pendingLaunchIntentOfSession(ctx, q, sessionID)
	if err != nil {
		return "", err
	}
	var intentIncarnation identity.IncarnationID
	if hasIntent {
		parsed, parseErr := identity.ParseIncarnationID(rawIntent)
		if parseErr != nil {
			return "", fmt.Errorf("sqlite: pending launch intent of session %s carries a malformed incarnation id (%s): %w", sessionID, parseErr.Error(), app.ErrNotFound)
		}
		intentIncarnation = parsed
	}
	switch {
	case hasBinding && hasIntent:
		if binding.IncarnationID != intentIncarnation {
			return "", fmt.Errorf("sqlite: binding incarnation %s and pending intent incarnation %s disagree for session %s: %w", binding.IncarnationID, intentIncarnation, sessionID, app.ErrNotFound)
		}
		return binding.IncarnationID, nil
	case hasBinding:
		return binding.IncarnationID, nil
	case hasIntent:
		return intentIncarnation, nil
	default:
		return "", fmt.Errorf("sqlite: no binding and no pending launch intent for session %s: %w", sessionID, app.ErrNotFound)
	}
}

// worktreePathForAttempt resolves the recorded worktree path the launch
// boundary cross-checks: the attempt's own row when one is linked
// (worktrees.attempt_id, every feature-mode row), else — the solo shape —
// the run's only worktree row, and only while that row is unlinked; ""
// otherwise. A row linked to another attempt is never served, and a run
// holding several rows, linked or not, is never guessed among.
func worktreePathForAttempt(ctx context.Context, q querier, runID identity.RunID, attemptID identity.AttemptID) (string, error) {
	path, linked, err := attemptWorktreePath(ctx, q, attemptID)
	if err != nil || linked {
		return path, err
	}
	rows, err := q.QueryContext(ctx, `SELECT path, attempt_id IS NULL FROM worktrees WHERE run_id = ?`, runID.String())
	if err != nil {
		return "", fmt.Errorf("sqlite: load worktrees of run %s: %w", runID, err)
	}
	defer rows.Close() //nolint:errcheck // the deferred close of a fully-iterated read cursor has no failure the rows.Err check below misses.
	var (
		count        int
		onlyPath     string
		onlyUnlinked bool
	)
	for rows.Next() {
		if err := rows.Scan(&onlyPath, &onlyUnlinked); err != nil {
			return "", fmt.Errorf("sqlite: scan worktree path: %w", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("sqlite: iterate worktree paths: %w", err)
	}
	if count == 1 && onlyUnlinked {
		return onlyPath, nil
	}
	return "", nil
}

// attemptWorktreePath loads the path of the newest worktree row linked to
// attemptID; linked is false when no row names the attempt.
func attemptWorktreePath(ctx context.Context, q querier, attemptID identity.AttemptID) (path string, linked bool, err error) {
	err = q.QueryRowContext(ctx,
		`SELECT path FROM worktrees WHERE attempt_id = ? ORDER BY rowid DESC LIMIT 1`, attemptID.String(),
	).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("sqlite: load worktree of attempt %s: %w", attemptID, err)
	}
	return path, true, nil
}

// sessionIsSuccessor reports the cold-relaunch successor fact: another
// session of the same run carries the same (non-empty) native reference —
// the shape every relaunch path creates and a first launch's pre-assigned
// reference never has.
func sessionIsSuccessor(ctx context.Context, q querier, session *run.Session) (bool, error) {
	if session.NativeSessionRef == "" {
		return false, nil
	}
	var exists int64
	err := q.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM sessions WHERE run_id = ? AND id != ? AND native_session_ref = ?)`,
		session.RunID.String(), session.ID.String(), session.NativeSessionRef,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("sqlite: read native-reference lineage of session %s: %w", session.ID, err)
	}
	return exists != 0, nil
}

// LoadMessagingContext resolves the caller session's OWN run, logical
// address and current incarnation for the message CLI verbs, lease-free.
// The returned RunID is the session row's, never a caller-supplied one.
func (s *Store) LoadMessagingContext(ctx context.Context, sessionID identity.SessionID) (app.MessagingContext, error) {
	var out app.MessagingContext
	err := s.inReadTx(ctx, func(tx *sql.Tx) error {
		session, _, err := getSession(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		address, resolvable, err := resolveSessionAddress(ctx, tx, &session)
		if err != nil {
			return err
		}
		if !resolvable {
			return fmt.Errorf("sqlite: session %s (role %s) has no logical messaging address", sessionID, session.Role)
		}
		runV, _, err := getRun(ctx, tx, session.RunID)
		if err != nil {
			return err
		}
		out = app.MessagingContext{RunID: session.RunID, Address: address, StopRequested: runV.StopRequested}
		if binding, ok, bindingErr := currentBinding(ctx, tx, sessionID); bindingErr != nil {
			return bindingErr
		} else if ok {
			out.IncarnationID = binding.IncarnationID
		}
		return nil
	})
	if err != nil {
		return app.MessagingContext{}, err
	}
	return out, nil
}

// LoadMessageDetail is hop msg show's entire lookup: the immutable
// envelope plus the full delivery/ack history, addressed by run and
// message id alone. A message belonging to a different run is reported
// exactly like one that does not exist, so the lookup can never leak
// envelope content across runs.
func (s *Store) LoadMessageDetail(ctx context.Context, runID identity.RunID, messageID identity.MessageID) (app.MessageDetail, error) {
	var out app.MessageDetail
	err := s.inReadTx(ctx, func(tx *sql.Tx) error {
		message, err := getMessage(ctx, tx, messageID)
		if err != nil {
			return err
		}
		if message.RunID != runID {
			return fmt.Errorf("sqlite: message %s: %w", messageID, app.ErrNotFound)
		}
		out = app.MessageDetail{Message: message}
		if out.Deliveries, err = messageDeliveries(ctx, tx, messageID); err != nil {
			return err
		}
		if out.Ack, err = messageAck(ctx, tx, messageID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return app.MessageDetail{}, err
	}
	return out, nil
}

// featureRunDetail assembles RunDetail's Phase 3 feature-mode extensions
// (section 3): the task table, the most recently created integration, the
// guard shortfalls, the per-address mailbox surface and the pending human
// questions.
func featureRunDetail(ctx context.Context, q querier, detail *app.RunDetail, snapshot *app.RunSnapshot, now time.Time) error {
	runID := detail.RunID
	tasks, err := tasksByRun(ctx, q, runID)
	if err != nil {
		return err
	}
	edges, err := taskDependenciesByRun(ctx, q, runID)
	if err != nil {
		return err
	}
	for i := range tasks {
		summary := app.TaskSummary{
			TaskID: tasks[i].ID, Seq: tasks[i].Seq, Kind: tasks[i].Kind, State: tasks[i].State,
		}
		for _, edge := range edges {
			if edge.TaskID == tasks[i].ID {
				summary.DependsOn = append(summary.DependsOn, edge.PrerequisiteID)
			}
		}
		attempts, attemptsErr := attemptsByTask(ctx, q, tasks[i].ID)
		if attemptsErr != nil {
			return attemptsErr
		}
		summary.AttemptCount = len(attempts)
		if len(attempts) > 0 {
			path, _, pathErr := attemptWorktreePath(ctx, q, attempts[len(attempts)-1].ID)
			if pathErr != nil {
				return pathErr
			}
			summary.WorktreePath = path
		}
		detail.Tasks = append(detail.Tasks, summary)
	}

	if detail.LatestIntegration, err = latestIntegrationSummary(ctx, q, runID); err != nil {
		return err
	}
	if detail.GuardShortfalls, err = guardShortfalls(ctx, q, runID, tasks); err != nil {
		return err
	}
	if detail.Mailboxes, err = mailboxStatuses(ctx, q, runID, snapshot.Workflow.MessageAttention, now); err != nil {
		return err
	}
	if detail.PendingQuestions, err = pendingQuestions(ctx, q, runID, now); err != nil {
		return err
	}
	return nil
}

// latestIntegrationSummary loads the run's most recently CREATED
// integration row, if any — reading exactly what is recorded, never
// resolving the live git ref.
func latestIntegrationSummary(ctx context.Context, q querier, runID identity.RunID) (*app.IntegrationSummary, error) {
	integration, _, err := scanIntegration(q.QueryRowContext(ctx,
		selectIntegrationColumns+` WHERE run_id = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`, runID.String(),
	).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // a nil summary with a nil error is the documented "no integration attempted yet" value.
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: load latest integration of run %s: %w", runID, err)
	}
	return &app.IntegrationSummary{
		ID: integration.ID, TaskID: integration.TaskID,
		SourceCommitOID: integration.SourceCommitOID, PremergeHeadOID: integration.PremergeHeadOID,
		MergeCommitOID: integration.MergeCommitOID, State: integration.State,
	}, nil
}

// guardShortfalls assembles a GuardContext from exactly the evidence this
// read model tracks and evaluates it: the plan flag and implement tasks
// are real; the head object IDs come from the most recently INTEGRATED
// integration row, both collapsing to its merge commit (Integration
// carries no separate tree id); LatestCheck is nil — no combined-candidate
// check receipt is recorded by this slice's tables, so check-missing is
// reported whenever a head exists, honestly reflecting the absent evidence
// (the completion guard itself, usecase_completion.go, assembles its own
// context from the live ref, never from this status surface).
func guardShortfalls(ctx context.Context, q querier, runID identity.RunID, tasks []run.Task) ([]run.GuardShortfall, error) {
	runV, _, err := getRun(ctx, q, runID)
	if err != nil {
		return nil, err
	}
	guardCtx := run.GuardContext{PlanClosed: runV.PlanClosed}
	for i := range tasks {
		if tasks[i].Kind == run.TaskKindImplement {
			guardCtx.ImplementTasks = append(guardCtx.ImplementTasks, tasks[i])
		}
	}
	integrated, _, err := scanIntegration(q.QueryRowContext(ctx,
		selectIntegrationColumns+` WHERE run_id = ? AND state = 'integrated' ORDER BY updated_at DESC, rowid DESC LIMIT 1`,
		runID.String(),
	).Scan)
	switch {
	case err == nil:
		guardCtx.HeadCommitOID = integrated.MergeCommitOID
		guardCtx.HeadTreeOID = integrated.MergeCommitOID
	case errors.Is(err, sql.ErrNoRows):
	default:
		return nil, fmt.Errorf("sqlite: load integrated head of run %s: %w", runID, err)
	}
	if guardCtx.LatestReview, err = latestReview(ctx, q, runID); err != nil {
		return nil, err
	}
	_, missing := run.EvaluateReadiness(guardCtx)
	return missing, nil
}

// mailboxStatuses assembles section 7's per-address status surface: one
// entry per address holding a non-empty queue or an unacknowledged
// in-flight message, addresses in their canonical string order, Attention
// computed once here against the frozen [messages] attention_after
// threshold and the address's live session.
func mailboxStatuses(ctx context.Context, q querier, runID identity.RunID, threshold time.Duration, now time.Time) ([]app.MailboxStatus, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT DISTINCT recipient_address FROM messages WHERE run_id = ? ORDER BY recipient_address`, runID.String(),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list message addresses of run %s: %w", runID, err)
	}
	defer rows.Close() //nolint:errcheck // the deferred close of a fully-iterated read cursor has no failure the rows.Err check below misses.
	var addressTexts []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, fmt.Errorf("sqlite: scan message address: %w", err)
		}
		addressTexts = append(addressTexts, text)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate message addresses: %w", err)
	}

	var out []app.MailboxStatus
	for _, text := range addressTexts {
		address, err := parseAddress(text)
		if err != nil {
			return nil, err
		}
		queue, err := messagesByAddress(ctx, q, runID, address)
		if err != nil {
			return nil, err
		}
		status := app.MailboxStatus{Address: address}
		var oldestQueued time.Time
		for i := range queue {
			switch queue[i].State {
			case run.MessageDelivered:
				deliveries, deliveriesErr := messageDeliveries(ctx, q, queue[i].ID)
				if deliveriesErr != nil {
					return nil, deliveriesErr
				}
				if len(deliveries) == 0 {
					continue
				}
				latest := deliveries[0].At
				for _, d := range deliveries[1:] {
					if d.At.After(latest) {
						latest = d.At
					}
				}
				status.InFlight = &app.InFlightMessage{MessageID: queue[i].ID, Age: now.Sub(latest)}
			case run.MessageQueued:
				status.QueuedCount++
				if oldestQueued.IsZero() || queue[i].CreatedAt.Before(oldestQueued) {
					oldestQueued = queue[i].CreatedAt
				}
			case run.MessageAcknowledged:
				// Settled; never a pending obligation.
			}
		}
		if status.InFlight == nil && status.QueuedCount == 0 {
			continue
		}
		if status.QueuedCount > 0 {
			status.OldestQueuedAge = now.Sub(oldestQueued)
		}
		if status.AddressLive, err = addressLive(ctx, q, runID, address); err != nil {
			return nil, err
		}
		pendingAge := status.OldestQueuedAge
		if status.InFlight != nil && status.InFlight.Age > pendingAge {
			pendingAge = status.InFlight.Age
		}
		status.Attention = status.AddressLive && threshold > 0 && pendingAge > threshold
		out = append(out, status)
	}
	return out, nil
}

// addressLive reports whether address's own session is currently
// non-terminal: the run's current manager for AddressManager, the task's
// latest attempt's current session for AddressTask, always true for
// AddressHuman.
func addressLive(ctx context.Context, q querier, runID identity.RunID, address run.Address) (bool, error) {
	switch address.Kind {
	case run.AddressHuman:
		return true, nil
	case run.AddressManager:
		_, _, ok, err := managerSession(ctx, q, runID)
		return ok, err
	case run.AddressTask:
		attempts, err := attemptsByTask(ctx, q, address.TaskID)
		if err != nil {
			return false, err
		}
		if len(attempts) == 0 {
			return false, nil
		}
		_, _, ok, err := currentSession(ctx, q, attempts[len(attempts)-1].ID)
		return ok, err
	default:
		return false, nil
	}
}

// pendingQuestions lists every unanswered human-addressed question,
// oldest first. A human question's ack is bundled into its answer's
// acceptance, so the ack rows alone decide "answered".
func pendingQuestions(ctx context.Context, q querier, runID identity.RunID, now time.Time) ([]app.PendingQuestion, error) {
	questions, err := messagesByAddress(ctx, q, runID, run.HumanAddress())
	if err != nil {
		return nil, err
	}
	sort.SliceStable(questions, func(i, j int) bool { return questions[i].EnqueueSeq < questions[j].EnqueueSeq })
	var out []app.PendingQuestion
	for i := range questions {
		if questions[i].Kind != run.MessageQuestion || questions[i].State == run.MessageAcknowledged {
			continue
		}
		out = append(out, app.PendingQuestion{
			MessageID: questions[i].ID, BodyPath: questions[i].BodyPath, Age: now.Sub(questions[i].CreatedAt),
		})
	}
	return out, nil
}
