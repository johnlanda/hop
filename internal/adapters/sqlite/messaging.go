package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// Interface conformance for the section 7 worker-authority messaging port.
var _ app.MessagingStore = (*Store)(nil)

// Message-receipt verbs: the op column of message_receipts and the verb
// half of the (run, verb, request ID) acceptance key. The digest verbs for
// send and answer match internal/app's fakeStore recipes exactly, so both
// halves of the fakes law compute identical request digests.
const (
	msgSendVerb   = "msg-send"
	msgFetchVerb  = "msg-fetch"
	msgAckVerb    = "msg-ack"
	msgAnswerVerb = "answer"
)

// messageReceipt is one message_receipts row: claimed identities as plain
// text with no foreign keys, so a refused or malformed request still
// leaves evidence.
type messageReceipt struct {
	runID, op                                      string
	sessionID, incarnationID, messageID, recipient string
	requestID, requestDigest                       string
	outcome, createdEntityID, detail               string
}

// insertMessageReceipt records one messaging receipt at time at.
func insertMessageReceipt(ctx context.Context, q querier, r *messageReceipt, at time.Time) error {
	id, err := newUUID()
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx,
		`INSERT INTO message_receipts (id, run_id, op, claimed_session_id, claimed_incarnation_id, claimed_message_id, claimed_recipient, request_id, request_digest, outcome, created_entity_id, detail, at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, truncateClaim(r.runID), r.op, nullString(truncateClaim(r.sessionID)), nullString(truncateClaim(r.incarnationID)),
		nullString(truncateClaim(r.messageID)), nullString(truncateClaim(r.recipient)),
		nullString(r.requestID), nullString(r.requestDigest), r.outcome,
		nullString(r.createdEntityID), truncateClaim(r.detail), formatTime(at),
	); err != nil {
		return fmt.Errorf("sqlite: record message receipt: %w", err)
	}
	return nil
}

// acceptedMessageReceipt reads the ONE authoritative acceptance for the
// (run, verb, request ID) key, if any: the request digest it accepted and
// the entity it created.
func acceptedMessageReceipt(ctx context.Context, q querier, runID, op, requestID string) (digest, createdEntityID string, ok bool, err error) {
	var storedDigest, createdEntity sql.NullString
	err = q.QueryRowContext(ctx,
		`SELECT request_digest, created_entity_id FROM message_receipts WHERE run_id = ? AND op = ? AND request_id = ? AND outcome = 'accepted'`,
		runID, op, requestID,
	).Scan(&storedDigest, &createdEntity)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("sqlite: read accepted %s receipt: %w", op, err)
	}
	return storedDigest.String, createdEntity.String, true, nil
}

// sendRequestDigest computes the section 7 request digest for one hop msg
// send, normalizing an answer's payload (question uuid + body digest)
// separately from an ordinary question/info send — internal/app's
// fakeStore recipe verbatim.
func sendRequestDigest(send *app.MessageSend) string {
	senderAddr := app.AddressString(send.SenderAddress)
	if send.Kind == run.MessageAnswer {
		replyTo := ""
		if send.ReplyTo != nil {
			replyTo = send.ReplyTo.String()
		}
		return app.ComputeRequestDigest(msgSendVerb, send.RunID.String(), senderAddr, replyTo, send.BodyDigest)
	}
	replyTo, relayOf := "", ""
	if send.ReplyTo != nil {
		replyTo = send.ReplyTo.String()
	}
	if send.RelayedFrom != nil {
		relayOf = send.RelayedFrom.String()
	}
	return app.ComputeRequestDigest(msgSendVerb, send.RunID.String(), senderAddr, app.AddressString(send.Recipient), string(send.Kind), replyTo, relayOf, send.BodyDigest)
}

// SendMessage applies the section 7 send/answer validation order inside
// one worker-authority transaction: the request-ID receipt first (an
// identical retry returns the original acceptance as duplicate; a reused
// ID with different content is refused), then the caller session's OWN
// run (send.RunID is caller-supplied and never trusted alone), the
// current incarnation, and — for an ordinary send — addressing legality,
// run state and the recipient mailbox; an answer's destination is derived
// from the referenced question's sender, never caller-chosen. Every
// outcome leaves a receipt; only an acceptance occupies the (run, verb,
// request ID) key.
func (s *Store) SendMessage(ctx context.Context, send app.MessageSend) (app.MessageOutcome, error) { //nolint:gocritic // hugeParam: the port passes the send value; the adapter mirrors its signature.
	var outcome app.MessageOutcome
	err := s.inWriteTx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		digest := sendRequestDigest(&send)
		record := func(kind app.MessageOutcomeKind, messageID identity.MessageID, detail string) error {
			outcome = app.MessageOutcome{Kind: kind, MessageID: messageID, Detail: detail}
			receipt := &messageReceipt{
				runID: send.RunID.String(), op: msgSendVerb,
				sessionID: send.Sender.SessionID.String(), incarnationID: send.IncarnationID.String(),
				recipient: app.AddressString(send.Recipient),
				requestID: send.RequestID, requestDigest: digest,
				outcome: string(kind), createdEntityID: messageID.String(), detail: detail,
			}
			if send.ReplyTo != nil {
				receipt.messageID = send.ReplyTo.String()
			}
			return insertMessageReceipt(ctx, tx, receipt, now)
		}

		if send.RequestID != "" {
			priorDigest, createdEntity, ok, err := acceptedMessageReceipt(ctx, tx, send.RunID.String(), msgSendVerb, send.RequestID)
			if err != nil {
				return err
			}
			if ok {
				if priorDigest == digest {
					// The grammar reports an identical request-ID retry as
					// duplicate, distinct from the original accepted line,
					// even though the underlying entity is unchanged.
					created, parseErr := identity.ParseMessageID(createdEntity)
					if parseErr != nil {
						return fmt.Errorf("sqlite: accepted send receipt entity id: %w", parseErr)
					}
					return record(app.MessageDuplicate, created, "")
				}
				return record(app.MessageRefused, "", "request id reused with different content")
			}
		}

		runV, _, err := getRun(ctx, tx, send.RunID)
		if errors.Is(err, app.ErrNotFound) {
			return record(app.MessageMalformed, "", "unknown run")
		}
		if err != nil {
			return err
		}
		// The sender session's OWN run is the only authoritative source for
		// which run it may act in.
		sender, _, err := getSession(ctx, tx, send.Sender.SessionID)
		if errors.Is(err, app.ErrNotFound) || (err == nil && sender.RunID != send.RunID) {
			return record(app.MessageRefused, "", "session does not belong to this run")
		}
		if err != nil {
			return err
		}
		binding, hasBinding, err := currentBinding(ctx, tx, send.Sender.SessionID)
		if err != nil {
			return err
		}
		if !hasBinding || binding.IncarnationID != send.IncarnationID || binding.Superseded {
			return record(app.MessageRefused, "", "incarnation is not current")
		}

		if send.Kind == run.MessageAnswer {
			return acceptSessionAnswer(ctx, tx, &send, record, now)
		}
		if addrErr := run.ValidateSendAddressing(send.SenderAddress, send.Kind, send.Recipient); addrErr != nil {
			return record(app.MessageRefused, "", addrErr.Error())
		}
		if runV.State != run.RunRunning {
			return record(app.MessageRunNotAccept, "", "run is not accepting messages")
		}
		if send.Recipient.Kind == run.AddressTask {
			task, _, taskErr := getTask(ctx, tx, send.Recipient.TaskID)
			if errors.Is(taskErr, app.ErrNotFound) || (taskErr == nil && task.RunID != send.RunID) {
				return record(app.MessageMalformed, "", "unknown task")
			}
			if taskErr != nil {
				return taskErr
			}
			if task.MailboxClosed {
				return record(app.MessageMailboxClose, "", "mailbox is closed")
			}
		}
		seq, err := nextEnqueueSeq(ctx, tx, send.RunID, send.Recipient)
		if err != nil {
			return err
		}
		var message run.Message
		if send.Kind == run.MessageQuestion {
			message = run.NewQuestion(send.ID, send.RunID, send.Sender, send.Recipient, send.RelayedFrom, send.RequestID, send.BodyPath, send.BodyDigest, send.BodyBytes, seq, now)
		} else {
			message = run.NewInfo(send.ID, send.RunID, send.Sender, send.Recipient, send.RequestID, send.BodyPath, send.BodyDigest, send.BodyBytes, seq, now)
		}
		if err := insertMessage(ctx, tx, &message); err != nil {
			return err
		}
		return record(app.MessageAccepted, message.ID, "")
	})
	if err != nil {
		return app.MessageOutcome{}, err
	}
	return outcome, nil
}

// acceptSessionAnswer handles a session's answer to a question addressed
// to its own logical address, via run.AcceptAnswer: the destination is
// derived from the question's own sender, never from send.Recipient.
func acceptSessionAnswer(ctx context.Context, tx *sql.Tx, send *app.MessageSend, record func(app.MessageOutcomeKind, identity.MessageID, string) error, now time.Time) error {
	if send.ReplyTo == nil {
		return record(app.MessageMalformed, "", "answer requires reply-to")
	}
	question, questionErr := getMessage(ctx, tx, *send.ReplyTo)
	if errors.Is(questionErr, app.ErrNotFound) || (questionErr == nil && question.RunID != send.RunID) {
		return record(app.MessageMalformed, "", "unknown question")
	}
	if questionErr != nil {
		return questionErr
	}
	destination, resolvable, destErr := answerDestination(ctx, tx, &question)
	if destErr != nil {
		return destErr
	}
	if !resolvable {
		return record(app.MessageMalformed, "", "question originator is not resolvable")
	}
	prior, priorErr := acceptedAnswer(ctx, tx, question.ID)
	if priorErr != nil {
		return priorErr
	}
	seq, seqErr := nextEnqueueSeq(ctx, tx, send.RunID, destination)
	if seqErr != nil {
		return seqErr
	}
	outcomeVal, err := run.AcceptAnswer(question, prior, destination, send.Sender, run.AnswerSubmission{
		ID: send.ID, BodyPath: send.BodyPath, BodyDigest: send.BodyDigest, BodyBytes: send.BodyBytes,
	}, seq, now)
	switch {
	case err == nil:
		if insertErr := insertMessage(ctx, tx, &outcomeVal.Answer); insertErr != nil {
			return insertErr
		}
		if ackErr := persistBundledQuestionAck(ctx, tx, &outcomeVal.Question, now); ackErr != nil {
			return ackErr
		}
		return record(app.MessageAccepted, outcomeVal.Answer.ID, "")
	case errors.Is(err, run.ErrDuplicateAnswer):
		return record(app.MessageDuplicate, outcomeVal.Answer.ID, "")
	case errors.Is(err, run.ErrConflictingAnswer):
		return record(app.MessageConflicting, "", err.Error())
	default:
		return record(app.MessageRefused, "", err.Error())
	}
}

// answerDestination resolves an answer's destination from the referenced
// question's own sender: the sender's logical address for a session
// sender; a controller- or human-authored question has no fetchable
// originator address to answer back to through this path.
func answerDestination(ctx context.Context, q querier, question *run.Message) (run.Address, bool, error) {
	if question.Sender.Kind != run.PrincipalSession {
		return run.Address{}, false, nil
	}
	sender, _, err := getSession(ctx, q, question.Sender.SessionID)
	if errors.Is(err, app.ErrNotFound) {
		return run.Address{}, false, nil
	}
	if err != nil {
		return run.Address{}, false, err
	}
	return resolveSessionAddress(ctx, q, &sender)
}

// acceptedAnswer loads the question's already-accepted answer, or nil.
func acceptedAnswer(ctx context.Context, q querier, questionID identity.MessageID) (*run.Message, error) {
	answer, err := scanMessage(q.QueryRowContext(ctx,
		selectMessageColumns+` WHERE m.kind = 'answer' AND m.reply_to = ?`, questionID.String(),
	).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // a nil answer with a nil error is the documented "not answered yet" value.
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: load accepted answer of question %s: %w", questionID, err)
	}
	return &answer, nil
}

// persistBundledQuestionAck records the acknowledgement AcceptAnswer
// bundles into an acceptance when the question was human-addressed (a
// human question is never fetched, so its answer's acceptance is its
// ack). The ack row carries no session or incarnation — the human is not
// a session — and an already-acknowledged question keeps its original row.
func persistBundledQuestionAck(ctx context.Context, q querier, question *run.Message, now time.Time) error {
	if question.State != run.MessageAcknowledged {
		return nil
	}
	prior, err := messageAck(ctx, q, question.ID)
	if err != nil {
		return err
	}
	if prior != nil {
		return nil
	}
	if _, err := q.ExecContext(ctx,
		`INSERT INTO message_acks (message_id, session_id, incarnation_id, acked_at) VALUES (?, NULL, NULL, ?)`,
		question.ID.String(), formatTime(now),
	); err != nil {
		return fmt.Errorf("sqlite: record bundled question ack: %w", err)
	}
	return nil
}

// FetchNextMessage serves the in-flight delivered-unacknowledged message
// for the caller's address if one exists, else the lowest-enqueue-sequence
// queued message. Before touching the queue it independently re-derives
// every caller-supplied identity from the session row itself — the
// session's own run, its current non-superseded binding's incarnation and
// its resolved logical address — and any disagreement is
// app.ErrMessagingUnauthorized with a refusal receipt committed (the one
// evidence a refused fetch leaves). An EMPTY fetch commits neither a
// delivery row nor a receipt, so a 1s poll loop cannot grow the store; a
// successful serve's evidence is its append-only delivery row.
func (s *Store) FetchNextMessage(ctx context.Context, fetch app.MessageFetch) (app.MessageDelivery, bool, error) { //nolint:gocritic // hugeParam: the port passes the fetch value; the adapter mirrors its signature.
	var (
		delivery app.MessageDelivery
		served   bool
		refusal  error
	)
	err := s.inWriteTx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		refuse := func(detail string, cause error) error {
			refusal = cause
			return insertMessageReceipt(ctx, tx, &messageReceipt{
				runID: fetch.RunID.String(), op: msgFetchVerb,
				sessionID: fetch.SessionID.String(), incarnationID: fetch.IncarnationID.String(),
				recipient: app.AddressString(fetch.Address),
				outcome:   string(app.MessageRefused), detail: detail,
			}, now)
		}

		session, _, err := getSession(ctx, tx, fetch.SessionID)
		if errors.Is(err, app.ErrNotFound) || (err == nil && session.RunID != fetch.RunID) {
			return refuse("session does not belong to this run",
				fmt.Errorf("%w: session %s does not belong to run %s", app.ErrMessagingUnauthorized, fetch.SessionID, fetch.RunID))
		}
		if err != nil {
			return err
		}
		binding, hasBinding, err := currentBinding(ctx, tx, fetch.SessionID)
		if err != nil {
			return err
		}
		if !hasBinding || binding.IncarnationID != fetch.IncarnationID || binding.Superseded {
			return refuse("incarnation is not current",
				fmt.Errorf("%w: session %s incarnation %s is not current", app.ErrMessagingUnauthorized, fetch.SessionID, fetch.IncarnationID))
		}
		address, resolvable, err := resolveSessionAddress(ctx, tx, &session)
		if err != nil {
			return err
		}
		if !resolvable || !address.Equal(fetch.Address) {
			return refuse("session does not resolve to the claimed address",
				fmt.Errorf("%w: session %s does not resolve to address %s", app.ErrMessagingUnauthorized, fetch.SessionID, app.AddressString(fetch.Address)))
		}

		queue, err := messagesByAddress(ctx, tx, fetch.RunID, fetch.Address)
		if err != nil {
			return err
		}
		next, ok := run.NextDeliverable(queue)
		if !ok {
			// A poll loop must not grow the store: no delivery row, no
			// receipt.
			return nil
		}
		deliveryID, err := newUUID()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO message_deliveries (id, message_id, session_id, incarnation_id, delivered_at) VALUES (?, ?, ?, ?, ?)`,
			deliveryID, next.ID.String(), fetch.SessionID.String(), fetch.IncarnationID.String(), formatTime(now),
		); err != nil {
			return fmt.Errorf("sqlite: record delivery of message %s: %w", next.ID, err)
		}
		served = true
		delivery = app.MessageDelivery{Message: next}
		if next.Kind == run.MessageAnswer && next.ReplyTo != nil {
			question, questionErr := getMessage(ctx, tx, *next.ReplyTo)
			if questionErr == nil {
				origin := run.ResolveOrigin(question)
				delivery.Origin = &origin
			} else if !errors.Is(questionErr, app.ErrNotFound) {
				return questionErr
			}
		}
		return nil
	})
	if err != nil {
		return app.MessageDelivery{}, false, err
	}
	if refusal != nil {
		return app.MessageDelivery{}, false, refusal
	}
	return delivery, served, nil
}

// AckMessage applies the section 7 ack order: receipt before eligibility
// (a repeat ack of an acknowledged message is idempotent duplicate,
// whatever the caller's current incarnation), then the acking session's
// OWN run, a delivery row for the ACKING SESSION ITSELF at the CLAIMED
// incarnation (a predecessor session's — or a superseded incarnation's —
// delivery proves nothing), and that incarnation's currency. Insertion of
// the ack row is the acknowledgement.
func (s *Store) AckMessage(ctx context.Context, ack app.MessageAck) (app.MessageAckOutcome, error) {
	var outcome app.MessageAckOutcome
	err := s.inWriteTx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		record := func(kind app.MessageAckOutcomeKind, detail string) error {
			outcome = app.MessageAckOutcome{Kind: kind, Detail: detail}
			return insertMessageReceipt(ctx, tx, &messageReceipt{
				runID: ack.RunID.String(), op: msgAckVerb,
				sessionID: ack.SessionID.String(), incarnationID: ack.IncarnationID.String(),
				messageID: ack.MessageID.String(),
				outcome:   string(kind), detail: detail,
			}, now)
		}

		message, err := getMessage(ctx, tx, ack.MessageID)
		if errors.Is(err, app.ErrNotFound) || (err == nil && message.RunID != ack.RunID) {
			return record(app.AckRefused, "unknown message")
		}
		if err != nil {
			return err
		}
		acker, _, err := getSession(ctx, tx, ack.SessionID)
		if errors.Is(err, app.ErrNotFound) || (err == nil && acker.RunID != ack.RunID) {
			return record(app.AckRefused, "session does not belong to this run")
		}
		if err != nil {
			return err
		}

		priorAck, err := messageAck(ctx, tx, ack.MessageID)
		if err != nil {
			return err
		}
		var deliveredToSession bool
		if scanErr := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM message_deliveries WHERE message_id = ? AND session_id = ? AND incarnation_id = ?)`,
			ack.MessageID.String(), ack.SessionID.String(), ack.IncarnationID.String(),
		).Scan(&deliveredToSession); scanErr != nil {
			return fmt.Errorf("sqlite: read deliveries of message %s: %w", ack.MessageID, scanErr)
		}
		binding, hasBinding, err := currentBinding(ctx, tx, ack.SessionID)
		if err != nil {
			return err
		}
		incarnationIsCurrent := hasBinding && binding.IncarnationID == ack.IncarnationID && !binding.Superseded

		outcomeVal, err := run.AcceptAck(message, priorAck,
			run.AckContext{DeliveredToSession: deliveredToSession, IncarnationCurrent: incarnationIsCurrent},
			run.Ack{SessionID: ack.SessionID, IncarnationID: ack.IncarnationID}, now)
		switch {
		case err != nil:
			return record(app.AckRefused, err.Error())
		case priorAck != nil:
			return record(app.AckDuplicate, "")
		default:
			if _, insertErr := tx.ExecContext(ctx,
				`INSERT INTO message_acks (message_id, session_id, incarnation_id, acked_at) VALUES (?, ?, ?, ?)`,
				outcomeVal.Ack.MessageID.String(), ack.SessionID.String(), ack.IncarnationID.String(), formatTime(outcomeVal.Ack.At),
			); insertErr != nil {
				return fmt.Errorf("sqlite: record ack of message %s: %w", ack.MessageID, insertErr)
			}
			return record(app.AckAccepted, "")
		}
	})
	if err != nil {
		return app.MessageAckOutcome{}, err
	}
	return outcome, nil
}

// AnswerQuestion applies one hop answer — a controller-machine command
// with no session — in the section 7 order: the request-ID receipt first
// (the digest binds the QUESTION UUID with the body digest, so identical
// bodies to two questions are two requests), then the question's
// existence, its human address, its derived destination and any prior
// accepted answer through run.AcceptAnswer. Acceptance commits the answer
// envelope AND the question's bundled acknowledgement atomically — a
// human question is never fetched, so its answer's acceptance is its ack.
func (s *Store) AnswerQuestion(ctx context.Context, answer app.HumanAnswer) (app.MessageOutcome, error) { //nolint:gocritic // hugeParam: the port passes the answer value; the adapter mirrors its signature.
	var outcome app.MessageOutcome
	err := s.inWriteTx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		digest := app.ComputeRequestDigest(msgAnswerVerb, answer.RunID.String(), app.AddressString(run.HumanAddress()), answer.QuestionID.String(), answer.BodyDigest)
		record := func(kind app.MessageOutcomeKind, messageID identity.MessageID, detail string) error {
			outcome = app.MessageOutcome{Kind: kind, MessageID: messageID, Detail: detail}
			return insertMessageReceipt(ctx, tx, &messageReceipt{
				runID: answer.RunID.String(), op: msgAnswerVerb,
				messageID: answer.QuestionID.String(),
				requestID: answer.RequestID, requestDigest: digest,
				outcome: string(kind), createdEntityID: messageID.String(), detail: detail,
			}, now)
		}

		if answer.RequestID != "" {
			priorDigest, createdEntity, ok, err := acceptedMessageReceipt(ctx, tx, answer.RunID.String(), msgAnswerVerb, answer.RequestID)
			if err != nil {
				return err
			}
			if ok {
				if priorDigest == digest {
					created, parseErr := identity.ParseMessageID(createdEntity)
					if parseErr != nil {
						return fmt.Errorf("sqlite: accepted answer receipt entity id: %w", parseErr)
					}
					return record(app.MessageDuplicate, created, "")
				}
				return record(app.MessageRefused, "", "request id reused with different content")
			}
		}

		question, err := getMessage(ctx, tx, answer.QuestionID)
		if errors.Is(err, app.ErrNotFound) || (err == nil && question.RunID != answer.RunID) {
			return record(app.MessageMalformed, "", "unknown question")
		}
		if err != nil {
			return err
		}
		if question.Recipient.Kind != run.AddressHuman {
			return record(app.MessageRefused, "", "question is not human-addressed")
		}
		destination, resolvable, err := answerDestination(ctx, tx, &question)
		if err != nil {
			return err
		}
		if !resolvable {
			return record(app.MessageMalformed, "", "question originator is not resolvable")
		}
		prior, err := acceptedAnswer(ctx, tx, question.ID)
		if err != nil {
			return err
		}
		seq, err := nextEnqueueSeq(ctx, tx, answer.RunID, destination)
		if err != nil {
			return err
		}
		outcomeVal, err := run.AcceptAnswer(question, prior, destination, run.HumanPrincipal(), run.AnswerSubmission{
			ID: answer.ID, BodyPath: answer.BodyPath, BodyDigest: answer.BodyDigest, BodyBytes: answer.BodyBytes,
		}, seq, now)
		switch {
		case err == nil:
			if insertErr := insertMessage(ctx, tx, &outcomeVal.Answer); insertErr != nil {
				return insertErr
			}
			if ackErr := persistBundledQuestionAck(ctx, tx, &outcomeVal.Question, now); ackErr != nil {
				return ackErr
			}
			return record(app.MessageAccepted, outcomeVal.Answer.ID, "")
		case errors.Is(err, run.ErrDuplicateAnswer):
			return record(app.MessageDuplicate, outcomeVal.Answer.ID, "")
		case errors.Is(err, run.ErrConflictingAnswer):
			return record(app.MessageConflicting, "", err.Error())
		default:
			return record(app.MessageRefused, "", err.Error())
		}
	})
	if err != nil {
		return app.MessageOutcome{}, err
	}
	return outcome, nil
}
