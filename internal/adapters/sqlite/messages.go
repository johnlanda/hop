package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// parseAddress parses the stored canonical recipient form back into a
// run.Address: "manager", "human" or "task:<uuid>" (app.AddressString's
// exact rendering, the only writer of the column).
func parseAddress(text string) (run.Address, error) {
	switch {
	case text == "manager":
		return run.ManagerAddress(), nil
	case text == "human":
		return run.HumanAddress(), nil
	case strings.HasPrefix(text, "task:"):
		taskID, err := identity.ParseTaskID(strings.TrimPrefix(text, "task:"))
		if err != nil {
			return run.Address{}, fmt.Errorf("sqlite: recipient address %q task id: %w", text, err)
		}
		return run.TaskAddress(taskID), nil
	default:
		return run.Address{}, fmt.Errorf("sqlite: recipient address %q is not a canonical address", text)
	}
}

// selectMessageColumns selects everything scanMessage maps. The two
// trailing EXISTS columns reconstruct Message.State — not a persisted
// envelope field (the envelope is immutable) — from the delivery and ack
// rows in the same snapshot.
const selectMessageColumns = `SELECT m.id, m.run_id, m.sender_kind, m.sender_session_id, m.recipient_address,
	m.kind, m.reply_to, m.relayed_from, m.enqueue_seq, m.request_id,
	m.body_path, m.body_digest, m.body_bytes, m.created_at,
	EXISTS (SELECT 1 FROM message_deliveries d WHERE d.message_id = m.id),
	EXISTS (SELECT 1 FROM message_acks a WHERE a.message_id = m.id)
	FROM messages m`

// scanMessage maps one messages row (with the two reconstruction columns)
// from a row scanner.
func scanMessage(scan func(dest ...any) error) (run.Message, error) {
	var (
		id, runID, senderKind, recipientAddress, kind, bodyPath, bodyDigest, createdAt string
		senderSession, replyTo, relayedFrom, requestID                                 sql.NullString
		enqueueSeq, bodyBytes, delivered, acked                                        int64
	)
	if err := scan(&id, &runID, &senderKind, &senderSession, &recipientAddress, &kind, &replyTo, &relayedFrom, &enqueueSeq, &requestID, &bodyPath, &bodyDigest, &bodyBytes, &createdAt, &delivered, &acked); err != nil {
		return run.Message{}, err
	}
	messageID, err := identity.ParseMessageID(id)
	if err != nil {
		return run.Message{}, fmt.Errorf("sqlite: message id: %w", err)
	}
	parsedRunID, err := identity.ParseRunID(runID)
	if err != nil {
		return run.Message{}, fmt.Errorf("sqlite: message %s run id: %w", id, err)
	}
	recipient, err := parseAddress(recipientAddress)
	if err != nil {
		return run.Message{}, fmt.Errorf("sqlite: message %s: %w", id, err)
	}
	message := run.Message{
		ID:         messageID,
		RunID:      parsedRunID,
		Sender:     run.Principal{Kind: run.PrincipalKind(senderKind)},
		Recipient:  recipient,
		Kind:       run.MessageKind(kind),
		EnqueueSeq: int(enqueueSeq),
		RequestID:  requestID.String,
		BodyPath:   bodyPath,
		BodyDigest: bodyDigest,
		BodyBytes:  bodyBytes,
	}
	if senderSession.Valid {
		sessionID, senderErr := identity.ParseSessionID(senderSession.String)
		if senderErr != nil {
			return run.Message{}, fmt.Errorf("sqlite: message %s sender session id: %w", id, senderErr)
		}
		message.Sender.SessionID = sessionID
	}
	if replyTo.Valid {
		parsed, replyErr := identity.ParseMessageID(replyTo.String)
		if replyErr != nil {
			return run.Message{}, fmt.Errorf("sqlite: message %s reply-to id: %w", id, replyErr)
		}
		message.ReplyTo = &parsed
	}
	if relayedFrom.Valid {
		parsed, relayErr := identity.ParseMessageID(relayedFrom.String)
		if relayErr != nil {
			return run.Message{}, fmt.Errorf("sqlite: message %s relayed-from id: %w", id, relayErr)
		}
		message.RelayedFrom = &parsed
	}
	if message.CreatedAt, err = parseTime(createdAt); err != nil {
		return run.Message{}, err
	}
	switch {
	case acked != 0:
		message.State = run.MessageAcknowledged
	case delivered != 0:
		message.State = run.MessageDelivered
	default:
		message.State = run.MessageQueued
	}
	return message, nil
}

// getMessage loads one message with its reconstructed state.
func getMessage(ctx context.Context, q querier, id identity.MessageID) (run.Message, error) {
	message, err := scanMessage(q.QueryRowContext(ctx, selectMessageColumns+` WHERE m.id = ?`, id.String()).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return run.Message{}, fmt.Errorf("sqlite: message %s: %w", id, app.ErrNotFound)
	}
	if err != nil {
		return run.Message{}, fmt.Errorf("sqlite: load message %s: %w", id, err)
	}
	return message, nil
}

// collectMessages drains one messages query into decoded rows.
func collectMessages(rows *sql.Rows) ([]run.Message, error) {
	defer rows.Close() //nolint:errcheck // the deferred close of a fully-iterated read cursor has no failure the rows.Err check below misses.
	var messages []run.Message
	for rows.Next() {
		message, err := scanMessage(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan message row: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate message rows: %w", err)
	}
	return messages, nil
}

// messagesByAddress lists one recipient address's envelopes, lowest
// enqueue sequence first, states reconstructed.
func messagesByAddress(ctx context.Context, q querier, runID identity.RunID, address run.Address) ([]run.Message, error) {
	rows, err := q.QueryContext(ctx,
		selectMessageColumns+` WHERE m.run_id = ? AND m.recipient_address = ? ORDER BY m.enqueue_seq`,
		runID.String(), app.AddressString(address),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list messages of %s at %s: %w", runID, app.AddressString(address), err)
	}
	return collectMessages(rows)
}

// nextEnqueueSeq assigns the next durable per-(run, recipient address)
// message sequence inside the caller's transaction — the FIFO authority
// (section 7): commit order, never caller clocks. Raced writers serialize
// behind SQLite's single-writer lock, and UNIQUE(run_id,
// recipient_address, enqueue_seq) backstops the invariant.
func nextEnqueueSeq(ctx context.Context, q querier, runID identity.RunID, address run.Address) (int, error) {
	var next int64
	if err := q.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(enqueue_seq), 0) + 1 FROM messages WHERE run_id = ? AND recipient_address = ?`,
		runID.String(), app.AddressString(address),
	).Scan(&next); err != nil {
		return 0, fmt.Errorf("sqlite: next enqueue sequence for %s at %s: %w", runID, app.AddressString(address), err)
	}
	return int(next), nil
}

// insertMessage persists one immutable envelope row.
func insertMessage(ctx context.Context, q querier, m *run.Message) error {
	var senderSession any
	if m.Sender.Kind == run.PrincipalSession {
		senderSession = m.Sender.SessionID.String()
	}
	var replyTo, relayedFrom any
	if m.ReplyTo != nil {
		replyTo = m.ReplyTo.String()
	}
	if m.RelayedFrom != nil {
		relayedFrom = m.RelayedFrom.String()
	}
	if _, err := q.ExecContext(ctx,
		`INSERT INTO messages (id, run_id, sender_kind, sender_session_id, recipient_address, kind, reply_to, relayed_from, enqueue_seq, request_id, body_path, body_digest, body_bytes, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID.String(), m.RunID.String(), string(m.Sender.Kind), senderSession, app.AddressString(m.Recipient),
		string(m.Kind), replyTo, relayedFrom, m.EnqueueSeq, nullString(m.RequestID),
		m.BodyPath, m.BodyDigest, m.BodyBytes, formatTime(m.CreatedAt),
	); err != nil {
		return fmt.Errorf("sqlite: insert message %s: %w", m.ID, err)
	}
	return nil
}

// messageDeliveries lists one message's delivery history, oldest first,
// re-serves included.
func messageDeliveries(ctx context.Context, q querier, id identity.MessageID) ([]run.Delivery, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT session_id, incarnation_id, delivered_at FROM message_deliveries WHERE message_id = ? ORDER BY delivered_at, rowid`,
		id.String(),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list deliveries of message %s: %w", id, err)
	}
	defer rows.Close() //nolint:errcheck // the deferred close of a fully-iterated read cursor has no failure the rows.Err check below misses.
	var deliveries []run.Delivery
	for rows.Next() {
		var sessionID, incarnationID, deliveredAt string
		if err := rows.Scan(&sessionID, &incarnationID, &deliveredAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan delivery row: %w", err)
		}
		parsedSessionID, parseErr := identity.ParseSessionID(sessionID)
		if parseErr != nil {
			return nil, fmt.Errorf("sqlite: delivery session id: %w", parseErr)
		}
		parsedIncarnationID, parseErr2 := identity.ParseIncarnationID(incarnationID)
		if parseErr2 != nil {
			return nil, fmt.Errorf("sqlite: delivery incarnation id: %w", parseErr2)
		}
		at, timeErr := parseTime(deliveredAt)
		if timeErr != nil {
			return nil, timeErr
		}
		deliveries = append(deliveries, run.Delivery{
			MessageID: id, SessionID: parsedSessionID, IncarnationID: parsedIncarnationID, At: at,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate delivery rows: %w", err)
	}
	return deliveries, nil
}

// messageAck loads one message's acknowledgement, or nil when it has none.
// SessionID and IncarnationID are empty for a human ack.
func messageAck(ctx context.Context, q querier, id identity.MessageID) (*run.Ack, error) {
	var (
		sessionID, incarnationID sql.NullString
		ackedAt                  string
	)
	err := q.QueryRowContext(ctx,
		`SELECT session_id, incarnation_id, acked_at FROM message_acks WHERE message_id = ?`, id.String(),
	).Scan(&sessionID, &incarnationID, &ackedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // a nil ack with a nil error is the documented "not acknowledged yet" value.
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: load ack of message %s: %w", id, err)
	}
	ack := run.Ack{MessageID: id}
	if sessionID.Valid {
		parsed, parseErr := identity.ParseSessionID(sessionID.String)
		if parseErr != nil {
			return nil, fmt.Errorf("sqlite: ack session id: %w", parseErr)
		}
		ack.SessionID = parsed
	}
	if incarnationID.Valid {
		parsed, parseErr := identity.ParseIncarnationID(incarnationID.String)
		if parseErr != nil {
			return nil, fmt.Errorf("sqlite: ack incarnation id: %w", parseErr)
		}
		ack.IncarnationID = parsed
	}
	if ack.At, err = parseTime(ackedAt); err != nil {
		return nil, err
	}
	return &ack, nil
}

// addressSessionCurrent reports run.CurrentAddressSession for session: for
// a session with an attempt, its own attempt row and its task's newest
// attempt by the store's attempt numbering (latestAttempt), both read
// inside the caller's transaction; a session with no attempt — the
// manager — reads no attempt row.
func addressSessionCurrent(ctx context.Context, q querier, session *run.Session) (bool, error) {
	var attempt, newest run.Attempt
	if session.AttemptID != "" {
		var err error
		if attempt, _, err = getAttempt(ctx, q, session.AttemptID); err != nil {
			return false, err
		}
		if newest, err = latestAttempt(ctx, q, attempt.TaskID); err != nil {
			return false, err
		}
	}
	return run.CurrentAddressSession(*session, attempt, newest), nil
}

// addressSessionRefusal is the value-free detail a fetch or send refused by
// addressSessionCurrent records and returns.
func addressSessionRefusal(address run.Address) string {
	if address.Kind == run.AddressManager {
		return "session is not the run's current manager session"
	}
	return "session is not its task's current attempt session"
}

// resolveSessionAddress resolves a session's logical address: manager for
// a manager session, task:<id> for an implementer or reviewer via its
// attempt's task — lineage-based, so every session ever bound to that
// task resolves the same address, current or historical. A Phase 2 solo
// worker has no logical address (solo runs have no messaging); ok is
// false for it and any unknown role.
func resolveSessionAddress(ctx context.Context, q querier, session *run.Session) (run.Address, bool, error) {
	switch session.Role {
	case run.RoleManager:
		return run.ManagerAddress(), true, nil
	case run.RoleImplementer, run.RoleReviewer:
		attempt, _, err := getAttempt(ctx, q, session.AttemptID)
		if err != nil {
			return run.Address{}, false, err
		}
		return run.TaskAddress(attempt.TaskID), true, nil
	default:
		return run.Address{}, false, nil
	}
}
