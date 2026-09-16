package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// SendMessageRequest is `hop msg send`'s CLI-facing input, exactly as
// parsed from flags and the caller's HOP_* environment: identities as
// plain strings (composition never imports domain or identity types,
// docs/plan/phase-2-design.md section 8), and Body already read from
// --file or --body by the caller, with Inline recording which flag was
// used (the two carry different size bounds). To and ReplyTo/RelayOf are
// "" when not applicable to Kind.
type SendMessageRequest struct {
	RunID         string
	SessionID     string
	IncarnationID string
	StateRoot     string
	To            string // "manager" | "human" | "task:<uuid>"; ignored for Kind "answer"
	Kind          string // "question" | "info" | "answer"
	ReplyTo       string // required for Kind "answer", forbidden otherwise
	RelayOf       string // optional, Kind "question" only
	Body          []byte
	Inline        bool // true when Body came from --body, false from --file
	RequestID     string
}

// SendMessageResult is SendMessage's outcome, string-only per the driving
// API convention. Reason is one of grammar.go's GrammarReason* tokens on
// every non-accepted, non-duplicate outcome; empty otherwise.
type SendMessageResult struct {
	Outcome   string
	MessageID string
	Reason    string
	Detail    string
}

// SendMessage is `hop msg send`'s driving use case: resolves the caller's
// logical address, bounds and writes the body durably (temp-file-then-
// rename via ArtifactStore, digest computed here — the file-first
// protocol, before any store call), then delegates validation and
// acceptance to MessagingStore.SendMessage, which owns the section 7
// addressing/eligibility rules and the request-ID receipt.
func (c *Controller) SendMessage(ctx context.Context, req SendMessageRequest) (SendMessageResult, error) { //nolint:gocritic // hugeParam: SendMessageRequest is the driving DTO for hop msg send, carrying a message body; a pointer would only complicate composition's call site.
	if c.Messages == nil {
		return SendMessageResult{}, fmt.Errorf("%w: SendMessage", ErrFeatureModeUnsupported)
	}
	wf, err := RequireWorkflowReadStore(c.Read, "SendMessage")
	if err != nil {
		return SendMessageResult{}, err
	}

	runID, err := identity.ParseRunID(req.RunID)
	if err != nil {
		return SendMessageResult{}, fmt.Errorf("app: parse run id: %w", err)
	}
	sessionID, err := identity.ParseSessionID(req.SessionID)
	if err != nil {
		return SendMessageResult{}, fmt.Errorf("app: parse session id: %w", err)
	}
	incarnationID, err := identity.ParseIncarnationID(req.IncarnationID)
	if err != nil {
		return SendMessageResult{}, fmt.Errorf("app: parse incarnation id: %w", err)
	}

	kind, err := parseMessageKind(req.Kind)
	if err != nil {
		return SendMessageResult{Outcome: string(MessageMalformed), Reason: GrammarReasonMalformed, Detail: err.Error()}, nil
	}
	limit := MessageBodyFileLimit
	if req.Inline {
		limit = MessageBodyInlineLimit
	}
	if len(req.Body) == 0 || len(req.Body) > limit {
		return SendMessageResult{Outcome: string(MessageMalformed), Reason: GrammarReasonMalformed, Detail: "body is empty or exceeds the size bound"}, nil
	}

	var recipient run.Address
	if kind != run.MessageAnswer {
		if recipient, err = parseAddress(req.To); err != nil {
			return SendMessageResult{Outcome: string(MessageMalformed), Reason: GrammarReasonMalformed, Detail: err.Error()}, nil
		}
	}
	var replyTo, relayOf *identity.MessageID
	if req.ReplyTo != "" {
		id, parseErr := identity.ParseMessageID(req.ReplyTo)
		if parseErr != nil {
			return SendMessageResult{Outcome: string(MessageMalformed), Reason: GrammarReasonMalformed, Detail: "invalid reply-to"}, nil
		}
		replyTo = &id
	}
	if req.RelayOf != "" {
		id, parseErr := identity.ParseMessageID(req.RelayOf)
		if parseErr != nil {
			return SendMessageResult{Outcome: string(MessageMalformed), Reason: GrammarReasonMalformed, Detail: "invalid relay-of"}, nil
		}
		relayOf = &id
	}

	msgCtx, err := wf.LoadMessagingContext(ctx, sessionID)
	if err != nil {
		return SendMessageResult{}, fmt.Errorf("app: load messaging context: %w", err)
	}
	if msgCtx.RunID != runID {
		return SendMessageResult{Outcome: string(MessageRefused), Reason: GrammarReasonUnauthorized, Detail: "session does not belong to this run"}, nil
	}

	msgID, err := identity.ParseMessageID(c.IDs.NewID())
	if err != nil {
		return SendMessageResult{}, fmt.Errorf("app: generate message id: %w", err)
	}
	bodyPath := messageBodyPath(req.StateRoot, runID, msgID)
	bodyDigest := sha256Hex(req.Body)
	if err = c.Artifacts.WriteArtifact(ctx, bodyPath, req.Body); err != nil {
		return SendMessageResult{}, fmt.Errorf("app: write message body: %w", err)
	}

	outcome, err := c.Messages.SendMessage(ctx, MessageSend{
		ID: msgID, RunID: runID, Sender: run.SessionPrincipal(sessionID), SenderAddress: msgCtx.Address,
		IncarnationID: incarnationID, Recipient: recipient, Kind: kind, ReplyTo: replyTo, RelayedFrom: relayOf,
		RequestID: req.RequestID, BodyPath: bodyPath, BodyDigest: bodyDigest, BodyBytes: int64(len(req.Body)),
	})
	if err != nil {
		return SendMessageResult{}, fmt.Errorf("app: send message: %w", err)
	}
	return SendMessageResult{Outcome: string(outcome.Kind), MessageID: outcome.MessageID.String(), Reason: outcome.Reason, Detail: outcome.Detail}, nil
}

// FetchMessageRequest is `hop msg next`'s driving input (also the single
// fetch `hop msg wait`'s CLI-side 1s poll loop repeats until its own
// timeout — the loop and the timeout both live in cmd/hop, never here).
type FetchMessageRequest struct {
	RunID         string
	SessionID     string
	IncarnationID string
}

// FetchMessageResult is FetchMessage's outcome. Delivered is false for an
// empty queue (the section 7 "commits nothing" case); every other field
// is meaningful only when Delivered is true. SenderSession is set only
// when SenderKind is "session". Origin is set only for an answer whose
// reply-to question carries relay provenance (the grammar's "origin"
// field, section 7).
type FetchMessageResult struct {
	Delivered     bool
	MessageID     string
	Kind          string
	SenderKind    string
	SenderSession string
	ReplyTo       string
	RelayOf       string
	Origin        string
	BodyPath      string
}

// FetchMessage is `hop msg next`'s driving use case: resolves the
// caller's logical address (lineage-based — a cold-relaunched or retried
// successor session fetches its predecessor's queue with no re-addressing
// write) and delegates to MessagingStore.FetchNextMessage.
func (c *Controller) FetchMessage(ctx context.Context, req FetchMessageRequest) (FetchMessageResult, error) {
	if c.Messages == nil {
		return FetchMessageResult{}, fmt.Errorf("%w: FetchMessage", ErrFeatureModeUnsupported)
	}
	wf, err := RequireWorkflowReadStore(c.Read, "FetchMessage")
	if err != nil {
		return FetchMessageResult{}, err
	}
	runID, err := identity.ParseRunID(req.RunID)
	if err != nil {
		return FetchMessageResult{}, fmt.Errorf("app: parse run id: %w", err)
	}
	sessionID, err := identity.ParseSessionID(req.SessionID)
	if err != nil {
		return FetchMessageResult{}, fmt.Errorf("app: parse session id: %w", err)
	}
	incarnationID, err := identity.ParseIncarnationID(req.IncarnationID)
	if err != nil {
		return FetchMessageResult{}, fmt.Errorf("app: parse incarnation id: %w", err)
	}
	msgCtx, err := wf.LoadMessagingContext(ctx, sessionID)
	if err != nil {
		return FetchMessageResult{}, fmt.Errorf("app: load messaging context: %w", err)
	}
	if msgCtx.RunID != runID {
		return FetchMessageResult{}, fmt.Errorf("%w: session does not belong to run %s", ErrMessagingUnauthorized, runID)
	}

	delivery, ok, err := c.Messages.FetchNextMessage(ctx, MessageFetch{
		RunID: runID, SessionID: sessionID, IncarnationID: incarnationID, Address: msgCtx.Address,
	})
	if err != nil {
		return FetchMessageResult{}, fmt.Errorf("app: fetch message: %w", err)
	}
	if !ok {
		return FetchMessageResult{}, nil
	}
	result := FetchMessageResult{
		Delivered: true, MessageID: delivery.Message.ID.String(), Kind: string(delivery.Message.Kind),
		SenderKind: string(delivery.Message.Sender.Kind), BodyPath: delivery.Message.BodyPath,
	}
	if delivery.Message.Sender.Kind == run.PrincipalSession {
		result.SenderSession = delivery.Message.Sender.SessionID.String()
	}
	if delivery.Message.ReplyTo != nil {
		result.ReplyTo = delivery.Message.ReplyTo.String()
	}
	if delivery.Message.RelayedFrom != nil {
		result.RelayOf = delivery.Message.RelayedFrom.String()
	}
	if delivery.Origin != nil {
		result.Origin = delivery.Origin.String()
	}
	return result, nil
}

// AckMessageRequest is `hop msg ack`'s driving input.
type AckMessageRequest struct {
	RunID         string
	MessageID     string
	SessionID     string
	IncarnationID string
}

// AckMessageResult is AckMessage's outcome. Reason is one of grammar.go's
// GrammarReason* tokens on every refused outcome; empty on accepted/
// duplicate.
type AckMessageResult struct {
	Outcome string
	Reason  string
	Detail  string
}

// AckMessage is `hop msg ack`'s driving use case.
func (c *Controller) AckMessage(ctx context.Context, req AckMessageRequest) (AckMessageResult, error) {
	if c.Messages == nil {
		return AckMessageResult{}, fmt.Errorf("%w: AckMessage", ErrFeatureModeUnsupported)
	}
	wf, err := RequireWorkflowReadStore(c.Read, "AckMessage")
	if err != nil {
		return AckMessageResult{}, err
	}
	runID, err := identity.ParseRunID(req.RunID)
	if err != nil {
		return AckMessageResult{}, fmt.Errorf("app: parse run id: %w", err)
	}
	messageID, err := identity.ParseMessageID(req.MessageID)
	if err != nil {
		return AckMessageResult{}, fmt.Errorf("app: parse message id: %w", err)
	}
	sessionID, err := identity.ParseSessionID(req.SessionID)
	if err != nil {
		return AckMessageResult{}, fmt.Errorf("app: parse session id: %w", err)
	}
	incarnationID, err := identity.ParseIncarnationID(req.IncarnationID)
	if err != nil {
		return AckMessageResult{}, fmt.Errorf("app: parse incarnation id: %w", err)
	}
	msgCtx, err := wf.LoadMessagingContext(ctx, sessionID)
	if err != nil {
		return AckMessageResult{}, fmt.Errorf("app: load messaging context: %w", err)
	}
	if msgCtx.RunID != runID {
		return AckMessageResult{Outcome: string(AckRefused), Reason: GrammarReasonUnauthorized, Detail: "session does not belong to this run"}, nil
	}
	outcome, err := c.Messages.AckMessage(ctx, MessageAck{
		RunID: runID, MessageID: messageID, SessionID: sessionID, IncarnationID: incarnationID,
	})
	if err != nil {
		return AckMessageResult{}, fmt.Errorf("app: ack message: %w", err)
	}
	return AckMessageResult{Outcome: string(outcome.Kind), Reason: outcome.Reason, Detail: outcome.Detail}, nil
}

// MessageWaitDefault resolves `hop msg wait`'s default --timeout: the
// run's own frozen [messages] wait_timeout (WorkflowSnapshot.MessageWait)
// when set, else DefaultMessageWait — a solo run's zero WorkflowSnapshot
// always resolves to the default, matching ApplyWorkflowDefaults' own
// fallback. Lease-free (ReadStore.LoadFrozenRun): hop msg wait runs with
// no controller lease, like every other worker-plumbing verb, and the
// caller (cmd/hop) uses this only when --timeout was not explicitly given
// (an explicit flag always wins).
func (c *Controller) MessageWaitDefault(ctx context.Context, runID string) (time.Duration, error) {
	rid, err := identity.ParseRunID(runID)
	if err != nil {
		return 0, fmt.Errorf("app: parse run id: %w", err)
	}
	frozen, err := c.Read.LoadFrozenRun(ctx, rid)
	if err != nil {
		return 0, fmt.Errorf("app: load frozen run: %w", err)
	}
	if frozen.Snapshot.Workflow.MessageWait > 0 {
		return frozen.Snapshot.Workflow.MessageWait, nil
	}
	return DefaultMessageWait, nil
}

// AnswerRequest is `hop answer`'s driving input: a controller-machine
// command, never a session (no HOP_* env, no lease, no incarnation).
type AnswerRequest struct {
	RunID      string
	QuestionID string
	StateRoot  string
	Body       []byte
	Inline     bool
	RequestID  string
}

// AnswerResult is Answer's outcome. Reason is one of grammar.go's
// GrammarReason* tokens on every non-accepted, non-duplicate outcome;
// empty otherwise.
type AnswerResult struct {
	Outcome   string
	MessageID string
	Reason    string
	Detail    string
}

// Answer is `hop answer`'s driving use case: writes the answer body
// durably before the call (file-first), then delegates to
// MessagingStore.AnswerQuestion.
func (c *Controller) Answer(ctx context.Context, req AnswerRequest) (AnswerResult, error) { //nolint:gocritic // hugeParam: AnswerRequest is the driving DTO for hop answer, carrying an answer body; a pointer would only complicate composition's call site.
	if c.Messages == nil {
		return AnswerResult{}, fmt.Errorf("%w: Answer", ErrFeatureModeUnsupported)
	}
	runID, err := identity.ParseRunID(req.RunID)
	if err != nil {
		return AnswerResult{}, fmt.Errorf("app: parse run id: %w", err)
	}
	questionID, err := identity.ParseMessageID(req.QuestionID)
	if err != nil {
		return AnswerResult{}, fmt.Errorf("app: parse question id: %w", err)
	}
	limit := MessageBodyFileLimit
	if req.Inline {
		limit = MessageBodyInlineLimit
	}
	if len(req.Body) == 0 || len(req.Body) > limit {
		return AnswerResult{Outcome: string(MessageMalformed), Reason: GrammarReasonMalformed, Detail: "body is empty or exceeds the size bound"}, nil
	}

	answerID, err := identity.ParseMessageID(c.IDs.NewID())
	if err != nil {
		return AnswerResult{}, fmt.Errorf("app: generate message id: %w", err)
	}
	bodyPath := messageBodyPath(req.StateRoot, runID, answerID)
	bodyDigest := sha256Hex(req.Body)
	if err = c.Artifacts.WriteArtifact(ctx, bodyPath, req.Body); err != nil {
		return AnswerResult{}, fmt.Errorf("app: write answer body: %w", err)
	}

	outcome, err := c.Messages.AnswerQuestion(ctx, HumanAnswer{
		ID: answerID, RunID: runID, QuestionID: questionID, RequestID: req.RequestID,
		BodyPath: bodyPath, BodyDigest: bodyDigest, BodyBytes: int64(len(req.Body)),
	})
	if err != nil {
		return AnswerResult{}, fmt.Errorf("app: answer question: %w", err)
	}
	return AnswerResult{Outcome: string(outcome.Kind), MessageID: outcome.MessageID.String(), Reason: outcome.Reason, Detail: outcome.Detail}, nil
}

// ShowMessageRequest is `hop msg show`'s driving input: a read-only,
// same-run envelope lookup callable by any of the run's sessions, or the
// human controller-machine context — section 7's grammar table validates
// no caller identity for this verb, so unlike every other message verb no
// HOP_* identity is parsed or checked here.
type ShowMessageRequest struct {
	RunID     string
	MessageID string
}

// ShowMessageDelivery is one delivery line ShowMessage renders: the
// grammar's `delivered: <session> <time>`.
type ShowMessageDelivery struct {
	SessionID string
	At        time.Time
}

// ShowMessageResult is ShowMessage's outcome. Found is false when no such
// message exists in the run (the grammar's `refused: not-found`); every
// other field is meaningful only when Found is true. SenderSession is set
// only when SenderKind is "session". Acknowledged names the single ack,
// when one exists.
type ShowMessageResult struct {
	Found          bool
	MessageID      string
	Kind           string
	SenderKind     string
	SenderSession  string
	Recipient      string
	ReplyTo        string
	RelayOf        string
	Seq            int
	BodyPath       string
	Deliveries     []ShowMessageDelivery
	Acknowledged   bool
	AcknowledgedAt time.Time
}

// ShowMessage is `hop msg show`'s driving use case:
// WorkflowReadStore.LoadMessageDetail is the entire lookup — read-only, no
// lease, no caller-identity validation, and it never delivers, acks or
// writes anything (section 7).
func (c *Controller) ShowMessage(ctx context.Context, req ShowMessageRequest) (ShowMessageResult, error) {
	wf, err := RequireWorkflowReadStore(c.Read, "ShowMessage")
	if err != nil {
		return ShowMessageResult{}, err
	}
	runID, err := identity.ParseRunID(req.RunID)
	if err != nil {
		return ShowMessageResult{}, fmt.Errorf("app: parse run id: %w", err)
	}
	messageID, err := identity.ParseMessageID(req.MessageID)
	if err != nil {
		return ShowMessageResult{}, fmt.Errorf("app: parse message id: %w", err)
	}

	detail, err := wf.LoadMessageDetail(ctx, runID, messageID)
	if errors.Is(err, ErrNotFound) {
		return ShowMessageResult{}, nil
	}
	if err != nil {
		return ShowMessageResult{}, fmt.Errorf("app: load message detail: %w", err)
	}

	result := ShowMessageResult{
		Found: true, MessageID: detail.Message.ID.String(), Kind: string(detail.Message.Kind),
		SenderKind: string(detail.Message.Sender.Kind), Recipient: AddressString(detail.Message.Recipient),
		Seq: detail.Message.EnqueueSeq, BodyPath: detail.Message.BodyPath,
	}
	if detail.Message.Sender.Kind == run.PrincipalSession {
		result.SenderSession = detail.Message.Sender.SessionID.String()
	}
	if detail.Message.ReplyTo != nil {
		result.ReplyTo = detail.Message.ReplyTo.String()
	}
	if detail.Message.RelayedFrom != nil {
		result.RelayOf = detail.Message.RelayedFrom.String()
	}
	for _, d := range detail.Deliveries {
		result.Deliveries = append(result.Deliveries, ShowMessageDelivery{SessionID: d.SessionID.String(), At: d.At})
	}
	if detail.Ack != nil {
		result.Acknowledged = true
		result.AcknowledgedAt = detail.Ack.At
	}
	return result, nil
}

// messageBodyPath is the deterministic artifact path for one message or
// answer body, under the run's artifact directory.
func messageBodyPath(stateRoot string, runID identity.RunID, messageID identity.MessageID) string {
	return filepath.Join(stateRoot, "runs", runID.String(), "messages", messageID.String()+".md")
}

// parseMessageKind validates s as one of the section 7 message kinds.
func parseMessageKind(s string) (run.MessageKind, error) {
	switch run.MessageKind(s) {
	case run.MessageQuestion, run.MessageInfo, run.MessageAnswer:
		return run.MessageKind(s), nil
	default:
		return "", fmt.Errorf("app: %q is not one of question, info, answer", s)
	}
}

// parseAddress parses s as one of the section 7 logical addresses:
// "manager", "human" or "task:<uuid>".
func parseAddress(s string) (run.Address, error) {
	switch {
	case s == "manager":
		return run.ManagerAddress(), nil
	case s == "human":
		return run.HumanAddress(), nil
	case len(s) > len("task:") && s[:len("task:")] == "task:":
		taskID, err := identity.ParseTaskID(s[len("task:"):])
		if err != nil {
			return run.Address{}, fmt.Errorf("app: invalid task address %q: %w", s, err)
		}
		return run.TaskAddress(taskID), nil
	default:
		return run.Address{}, fmt.Errorf("app: %q is not a recognized address (manager, human, task:<uuid>)", s)
	}
}
