package run

import (
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// MessageKind distinguishes a message's purpose.
type MessageKind string

// Message kinds.
const (
	MessageQuestion MessageKind = "question"
	MessageAnswer   MessageKind = "answer"
	MessageInfo     MessageKind = "info"
)

// MessageState is one state of the Message delivery state machine (section
// 5, "Message delivery"). Unlike Run/Task/Attempt/Session, this state is
// not itself a persisted column (the messages row is an immutable
// envelope); it is the application's computed view over the message's
// delivery/ack rows, reconstructed before calling into this package.
type MessageState string

// Message delivery states.
const (
	MessageQueued       MessageState = "queued"
	MessageDelivered    MessageState = "delivered"
	MessageAcknowledged MessageState = "acknowledged"
)

// PrincipalKind distinguishes a message sender: an ordinary session, or one
// of the two reserved principals that are never sessions.
type PrincipalKind string

// Principal kinds.
const (
	PrincipalSession    PrincipalKind = "session"
	PrincipalController PrincipalKind = "controller"
	PrincipalHuman      PrincipalKind = "human"
)

// Principal identifies a message's sender. SessionID is set only when Kind
// is PrincipalSession.
type Principal struct {
	Kind      PrincipalKind
	SessionID identity.SessionID
}

// SessionPrincipal identifies a message sent by session.
func SessionPrincipal(session identity.SessionID) Principal {
	return Principal{Kind: PrincipalSession, SessionID: session}
}

// ControllerPrincipal identifies a controller-authored info notice.
func ControllerPrincipal() Principal { return Principal{Kind: PrincipalController} }

// HumanPrincipal identifies a human-authored answer (hop answer).
func HumanPrincipal() Principal { return Principal{Kind: PrincipalHuman} }

// AddressKind distinguishes a message's logical destination shape.
type AddressKind string

// Address kinds.
const (
	AddressManager AddressKind = "manager"
	AddressTask    AddressKind = "task"
	AddressHuman   AddressKind = "human"
)

// Address is a message's logical destination: the manager, one task's
// mailbox, or the human. Addressing survives session churn — a cold-
// relaunched or retried successor session fetches at the same address as
// its predecessor, with no re-addressing write. TaskID is set only when
// Kind is AddressTask.
type Address struct {
	Kind   AddressKind
	TaskID identity.TaskID
}

// ManagerAddress is the manager's logical address.
func ManagerAddress() Address { return Address{Kind: AddressManager} }

// HumanAddress is the human's logical address.
func HumanAddress() Address { return Address{Kind: AddressHuman} }

// TaskAddress is task's mailbox address.
func TaskAddress(task identity.TaskID) Address { return Address{Kind: AddressTask, TaskID: task} }

// Equal reports whether a and b name the same address.
func (a Address) Equal(b Address) bool { return a.Kind == b.Kind && a.TaskID == b.TaskID }

// Message is a durable, attributed communication between sessions (or the
// controller or the human). The envelope is immutable once created; State
// is the application's reconstructed view of the message's current
// delivery status, not a persisted envelope field.
type Message struct {
	ID          identity.MessageID
	RunID       identity.RunID
	Sender      Principal
	Recipient   Address
	Kind        MessageKind
	ReplyTo     *identity.MessageID
	RelayedFrom *identity.MessageID
	EnqueueSeq  int
	RequestID   string
	BodyPath    string
	BodyDigest  string
	BodyBytes   int64
	State       MessageState
	CreatedAt   time.Time
}

// ValidateSendAddressing enforces the kind/address legality matrix of
// section 7 for an ordinary send (question or info; an answer's
// destination is never caller-chosen — AcceptAnswer validates it
// instead). senderAddress is the sending session's own logical address,
// resolved by the application from its role (and, for an implementer or
// reviewer, its task). Workers and reviewers (AddressTask) may only send
// to the manager; the manager (AddressManager) may send a question to the
// human or a question/info to a task; info to the human is always
// refused, since a human has no fetch or ack verb and could never settle
// it.
func ValidateSendAddressing(senderAddress Address, kind MessageKind, recipient Address) error {
	if kind == MessageAnswer {
		return fmt.Errorf("%w: an answer's destination is derived, not validated by ValidateSendAddressing", ErrInvalidTransition)
	}
	switch senderAddress.Kind {
	case AddressManager:
		if recipient.Kind == AddressHuman && kind == MessageQuestion {
			return nil
		}
		if recipient.Kind == AddressTask && (kind == MessageQuestion || kind == MessageInfo) {
			return nil
		}
	case AddressTask:
		if recipient.Kind == AddressManager && (kind == MessageQuestion || kind == MessageInfo) {
			return nil
		}
	case AddressHuman:
		// The human sends only through AcceptAnswer, never through this
		// ordinary send path.
	}
	return fmt.Errorf("%w: %s cannot send %s to %s", ErrInvalidTransition, senderAddress.Kind, kind, recipient.Kind)
}

// NewQuestion constructs a queued question message. relayedFrom is set
// only when this question relays another question (the manager's
// human-facing relay of a worker's question); nil otherwise.
func NewQuestion(id identity.MessageID, runID identity.RunID, sender Principal, recipient Address, relayedFrom *identity.MessageID, requestID, bodyPath, bodyDigest string, bodyBytes int64, enqueueSeq int, now time.Time) Message {
	return Message{
		ID: id, RunID: runID, Sender: sender, Recipient: recipient, Kind: MessageQuestion,
		RelayedFrom: relayedFrom, RequestID: requestID, EnqueueSeq: enqueueSeq,
		BodyPath: bodyPath, BodyDigest: bodyDigest, BodyBytes: bodyBytes,
		State: MessageQueued, CreatedAt: now,
	}
}

// NewInfo constructs a queued info message: manager to task, or the
// controller's own notices to the manager.
func NewInfo(id identity.MessageID, runID identity.RunID, sender Principal, recipient Address, requestID, bodyPath, bodyDigest string, bodyBytes int64, enqueueSeq int, now time.Time) Message {
	return Message{
		ID: id, RunID: runID, Sender: sender, Recipient: recipient, Kind: MessageInfo,
		RequestID: requestID, EnqueueSeq: enqueueSeq,
		BodyPath: bodyPath, BodyDigest: bodyDigest, BodyBytes: bodyBytes,
		State: MessageQueued, CreatedAt: now,
	}
}

// Deliver moves message into delivered: the first fetch (from queued) or
// a re-serve (from delivered — an at-least-once re-serve while
// unacknowledged; the application still records a fresh Delivery row for
// a re-serve even though State itself does not change). Message carries no
// UpdatedAt (its envelope is immutable; only State is a reconstructed
// view), so this transition takes no now.
func (m Message) Deliver() (Message, error) { //nolint:gocritic // hugeParam: Message is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	if m.State != MessageQueued && m.State != MessageDelivered {
		return m, fmt.Errorf("%w: message %s: %s to %s", ErrInvalidTransition, m.ID, m.State, MessageDelivered)
	}
	m.State = MessageDelivered
	return m, nil
}

// Delivery is one append-only record of a serve: session fetched message
// at incarnation, whether the first serve or a re-serve.
type Delivery struct {
	MessageID     identity.MessageID
	SessionID     identity.SessionID
	IncarnationID identity.IncarnationID
	At            time.Time
}

// Ack is a message's single acknowledgement. SessionID and IncarnationID
// are empty for a human ack via hop answer — a human question is never
// fetched, so its acceptance transaction acknowledges it directly.
type Ack struct {
	MessageID     identity.MessageID
	SessionID     identity.SessionID
	IncarnationID identity.IncarnationID
	At            time.Time
}

// AckContext is the application-assembled context AcceptAck needs beyond
// message and its rows: whether a delivery row exists for the ACKING
// SESSION ITSELF (never merely its lineage — a predecessor's delivery
// never authorizes a successor's ack), and whether that session's
// incarnation is its current, non-superseded one.
type AckContext struct {
	DeliveredToSession bool
	IncarnationCurrent bool
}

// AckOutcome is the state AcceptAck decided: the message after acceptance
// (Acknowledged on a fresh accept; unchanged otherwise) and the ack row
// (the prior one, on a duplicate).
type AckOutcome struct {
	Message Message
	Ack     Ack
}

// AcceptAck decides the outcome of one ack. A repeat ack of an already-
// acknowledged message is idempotent success, checked first and
// regardless of incarnation (receipt-before-eligibility) — the caller
// distinguishes "duplicate" from "accepted" by whether it passed a
// non-nil priorAck, exactly as it assembled that fact. Otherwise: message
// must be delivered (ErrNotDelivered otherwise), a delivery row must
// exist for the acking session itself (ErrNotDelivered otherwise), and
// that incarnation must be current (ErrStaleAck otherwise).
func AcceptAck(message Message, priorAck *Ack, ctx AckContext, ack Ack, now time.Time) (AckOutcome, error) { //nolint:gocritic // hugeParam: Message is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	if priorAck != nil {
		return AckOutcome{Message: message, Ack: *priorAck}, nil
	}
	if message.State != MessageDelivered {
		return AckOutcome{Message: message}, fmt.Errorf("%w: message %s: not delivered", ErrNotDelivered, message.ID)
	}
	if !ctx.DeliveredToSession {
		return AckOutcome{Message: message}, fmt.Errorf("%w: message %s: session %s", ErrNotDelivered, message.ID, ack.SessionID)
	}
	if !ctx.IncarnationCurrent {
		return AckOutcome{Message: message}, fmt.Errorf("%w: message %s: session %s incarnation %s", ErrStaleAck, message.ID, ack.SessionID, ack.IncarnationID)
	}
	ack.MessageID = message.ID
	ack.At = now
	message.State = MessageAcknowledged
	return AckOutcome{Message: message, Ack: ack}, nil
}

// NextDeliverable selects the next message FetchNextMessage should serve
// for one recipient address's queue: the in-flight delivered-
// unacknowledged message if one exists (a re-serve — per-recipient
// serialization keeps at most one), else the queued message with the
// lowest EnqueueSeq, the durable per-address sequence assigned inside the
// send transaction, so FIFO means commit order, never caller clocks. ok is
// false when nothing remains to serve.
func NextDeliverable(queue []Message) (Message, bool) {
	var (
		inFlight    Message
		hasInFlight bool
		lowest      Message
		hasQueued   bool
	)
	for i := range queue {
		switch queue[i].State {
		case MessageDelivered:
			inFlight, hasInFlight = queue[i], true
		case MessageQueued:
			if !hasQueued || queue[i].EnqueueSeq < lowest.EnqueueSeq {
				lowest, hasQueued = queue[i], true
			}
		case MessageAcknowledged:
			// Settled; never a delivery candidate.
		}
	}
	if hasInFlight {
		return inFlight, true
	}
	if hasQueued {
		return lowest, true
	}
	return Message{}, false
}

// AnswerSubmission is the application-validated content of one answer: a
// pre-generated message identity and the body's canonicalized digest,
// path and size. The domain treats BodyDigest as opaque and already
// computed (hashing stays in internal/app).
type AnswerSubmission struct {
	ID         identity.MessageID
	BodyPath   string
	BodyDigest string
	BodyBytes  int64
}

// AnswerOutcome is the state AcceptAnswer decided: the answer (existing,
// for a duplicate/conflict; newly accepted, otherwise) and question after
// any transition acceptance implies. question is only ever mutated
// (queued/delivered -> acknowledged) when it was human-addressed: a
// human question is never fetched, so its own ack is bundled into its
// answer's acceptance; every other question is acknowledged separately,
// by its recipient's own explicit AcceptAck.
type AnswerOutcome struct {
	Question Message
	Answer   Message
}

// AcceptAnswer decides the outcome of one answer to question, in the
// validation order of section 7 (receipt before eligibility): question
// must be a MessageQuestion (ErrInvalidTransition otherwise); any prior
// accepted answer is resolved first — an equal body digest is
// ErrDuplicateAnswer (idempotent, the accepted answer returned unchanged),
// an unequal one is ErrConflictingAnswer (the accepted answer undisturbed).
// "Unanswered" is entirely governed by prior: an ordinary (non-human)
// question's own delivery/ack status is orthogonal to answering it — the
// manager typically acks q1 upon reading it, well before composing and
// forwarding its answer, so this function imposes no delivery-state
// precondition of its own. destination is the question's ORIGINATOR's
// logical address, resolved by the application from the original sender's
// role/task at question time — the new answer's derived recipient, never
// caller-chosen. sender is the answering principal. enqueueSeq is the new
// answer's durable per-recipient-address position, assigned by the
// application inside the accepting transaction.
func AcceptAnswer(question Message, prior *Message, destination Address, sender Principal, submission AnswerSubmission, enqueueSeq int, now time.Time) (AnswerOutcome, error) { //nolint:gocritic // hugeParam: Message is an immutable domain value returned by every transition; a pointer receiver would let a caller's original be mutated through it, breaking the pure-transition contract.
	unchanged := AnswerOutcome{Question: question}
	if question.Kind != MessageQuestion {
		return unchanged, fmt.Errorf("%w: message %s: not a question", ErrInvalidTransition, question.ID)
	}
	if prior != nil {
		unchanged.Answer = *prior
		if prior.BodyDigest == submission.BodyDigest {
			return unchanged, fmt.Errorf("%w: question %s: answer %s", ErrDuplicateAnswer, question.ID, prior.ID)
		}
		return unchanged, fmt.Errorf("%w: question %s: answer %s", ErrConflictingAnswer, question.ID, prior.ID)
	}

	ackedQuestion := question
	if question.Recipient.Kind == AddressHuman {
		ackedQuestion.State = MessageAcknowledged
	}

	replyTo := question.ID
	answer := Message{
		ID: submission.ID, RunID: question.RunID, Sender: sender, Recipient: destination,
		Kind: MessageAnswer, ReplyTo: &replyTo, EnqueueSeq: enqueueSeq,
		BodyPath: submission.BodyPath, BodyDigest: submission.BodyDigest, BodyBytes: submission.BodyBytes,
		State: MessageQueued, CreatedAt: now,
	}
	return AnswerOutcome{Question: ackedQuestion, Answer: answer}, nil
}

// ResolveOrigin returns the original question ID a fetched answer's
// reply-to chain ultimately traces to: replyToQuestion.RelayedFrom when
// set (the manager's human-facing relay of a worker's question), else
// replyToQuestion.ID itself. This is the grammar's "origin" field (section
// 7): a restarted manager forwards a human answer using only this
// CLI-returned data, never store spelunking.
func ResolveOrigin(replyToQuestion Message) identity.MessageID { //nolint:gocritic // hugeParam: Message is passed by value everywhere in this package; this read-only helper mirrors that convention.
	if replyToQuestion.RelayedFrom != nil {
		return *replyToQuestion.RelayedFrom
	}
	return replyToQuestion.ID
}
