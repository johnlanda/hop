package app

import (
	"context"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// Message body bounds (docs/plan/phase-3-design.md section 13, open
// question 3): larger content belongs in repository files the message
// references by path, not in the body itself.
const (
	// MessageBodyFileLimit bounds a message or answer body file (`--file`).
	MessageBodyFileLimit = 64 * 1024
	// MessageBodyInlineLimit bounds an inline body (`--body`).
	MessageBodyInlineLimit = 4 * 1024
)

// AddressString renders a's canonical, stable form for the request digest
// and for equality/logging: "manager", "human" or "task:<uuid>". Never
// used for the human-facing status rendering, which additionally carries
// the task's t<seq> label (a status-layer concern, since the digest must
// stay stable across a display convention change).
func AddressString(a run.Address) string {
	switch a.Kind {
	case run.AddressManager:
		return "manager"
	case run.AddressHuman:
		return "human"
	case run.AddressTask:
		return "task:" + a.TaskID.String()
	default:
		return string(a.Kind)
	}
}

// MessageOutcomeKind is the section 7 outcome of one send/answer.
type MessageOutcomeKind string

// Message outcome kinds.
const (
	MessageAccepted     MessageOutcomeKind = "accepted"
	MessageDuplicate    MessageOutcomeKind = "duplicate"
	MessageConflicting  MessageOutcomeKind = "conflicting"
	MessageRefused      MessageOutcomeKind = "refused"
	MessageMalformed    MessageOutcomeKind = "malformed"
	MessageRunNotAccept MessageOutcomeKind = "refused-run-not-accepting"
	MessageMailboxClose MessageOutcomeKind = "refused-mailbox-closed"
)

// MessageOutcome is the recorded result of one send or answer. MessageID is
// set for Accepted and Duplicate.
type MessageOutcome struct {
	Kind      MessageOutcomeKind
	MessageID identity.MessageID
	Detail    string
}

// MessageSend is the application-validated content of one `hop msg send`:
// identities already parsed, the sender's own logical address already
// resolved from its session role/task (for addressing legality and the
// request digest — never the raw session id), and the body already
// written durably (temp-file-then-rename, digest computed) before this
// call, per the file-first protocol. Kind MessageAnswer leaves Recipient
// the zero Address: the destination is derived by the store from the
// referenced question's sender, never caller-chosen, and Recipient is
// never consulted for that kind.
type MessageSend struct {
	ID            identity.MessageID
	RunID         identity.RunID
	Sender        run.Principal
	SenderAddress run.Address
	// IncarnationID is the sending session's current incarnation: section
	// 7 requires eligibility (current incarnation; run running; for a
	// task:<id> destination, an open mailbox) before an ordinary send is
	// accepted.
	IncarnationID identity.IncarnationID
	Recipient     run.Address
	Kind          run.MessageKind
	// ReplyTo is set only for Kind MessageAnswer: the question being
	// answered.
	ReplyTo *identity.MessageID
	// RelayedFrom is set only for a MessageQuestion that relays another
	// question (the manager's human-facing relay of a worker's question).
	RelayedFrom *identity.MessageID
	// RequestID is the caller-stable idempotency key (optional; omitted
	// means server-minted and not retry-idempotent).
	RequestID  string
	BodyPath   string
	BodyDigest string
	BodyBytes  int64
}

// MessageFetch is one `hop msg next`/`hop msg wait` attempt: the caller
// resolved to its address and current incarnation (WorkflowReadStore.
// LoadMessagingContext), never a bare session id — addressing survives
// session churn (lineage), so this is what the store actually looks up
// against.
type MessageFetch struct {
	RunID         identity.RunID
	SessionID     identity.SessionID
	IncarnationID identity.IncarnationID
	Address       run.Address
}

// MessageDelivery is one served message: the envelope and, when it is an
// answer whose reply-to question carries relay provenance, the ORIGINAL
// question's id (the grammar's "origin" field) resolved server-side so a
// restarted manager forwards using only this returned data, never store
// spelunking.
type MessageDelivery struct {
	Message run.Message
	Origin  *identity.MessageID
}

// MessageAck is one `hop msg ack` attempt.
type MessageAck struct {
	RunID         identity.RunID
	MessageID     identity.MessageID
	SessionID     identity.SessionID
	IncarnationID identity.IncarnationID
}

// MessageAckOutcomeKind is the outcome of one ack attempt.
type MessageAckOutcomeKind string

// Message ack outcome kinds.
const (
	AckAccepted  MessageAckOutcomeKind = "accepted"
	AckDuplicate MessageAckOutcomeKind = "duplicate"
	AckRefused   MessageAckOutcomeKind = "refused"
)

// MessageAckOutcome is the recorded result of one ack attempt.
type MessageAckOutcome struct {
	Kind   MessageAckOutcomeKind
	Detail string
}

// HumanAnswer is one `hop answer` attempt: a controller-machine command
// (no HOP_* env, no lease), never issued by a session. The body is
// written durably before this call, exactly as for MessageSend.
type HumanAnswer struct {
	ID         identity.MessageID
	RunID      identity.RunID
	QuestionID identity.MessageID
	RequestID  string
	BodyPath   string
	BodyDigest string
	BodyBytes  int64
}

// MessagingStore holds the section 7 worker-authority messaging writes:
// transactions that never carry a controller generation, validated by the
// caller's session and incarnation currency (AnswerQuestion excepted — a
// human/controller-machine command). Every mutating verb accepts an
// optional caller-stable RequestID inside its request value: an identical
// retry returns the original outcome, a conflicting reuse is refused. An
// empty FetchNextMessage is the one deliberate exception to "every outcome
// leaves evidence": it commits nothing, so a 1s poll loop cannot grow the
// store.
type MessagingStore interface {
	SendMessage(ctx context.Context, send MessageSend) (MessageOutcome, error)
	// FetchNextMessage serves the in-flight delivered-unacknowledged
	// message for fetch.Address if one exists, else the lowest-enqueue-
	// sequence queued message. ok is false for an empty fetch, which
	// commits neither a delivery row nor a receipt.
	FetchNextMessage(ctx context.Context, fetch MessageFetch) (MessageDelivery, bool, error)
	AckMessage(ctx context.Context, ack MessageAck) (MessageAckOutcome, error)
	AnswerQuestion(ctx context.Context, answer HumanAnswer) (MessageOutcome, error)
}
