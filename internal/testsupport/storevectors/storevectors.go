// Package storevectors holds Phase 3's shared refused-input vectors:
// request shapes internal/app's worker-authority store ports
// (MessagingStore, PlanStore, ReviewStore) must refuse, expressed once so both
// internal/app's fakeStore tests and internal/adapters/sqlite's future
// real-store tests exercise the IDENTICAL input against their own backing
// implementation — the section 11 countermeasure for the Phase 2
// escaped-defect class "a fake accepted arguments the real adapter
// refuses" (docs/plan/phase-3-design.md section 11).
//
// Each vector is a plain Go function building the exact request value to
// pass to the port method its doc comment names, given caller-supplied
// identities from the consuming test's own fixture (a run, a session, an
// existing task or message). This package never bootstraps a run or
// session itself — that setup is inherently per-backing-store (a fake's
// direct field seeding versus a real store's SQL inserts) — it only
// supplies the malformed or conflicting REQUEST VALUE and documents the
// exact refusal a correct store must produce. Every vector's doc comment
// names the outcome and detail observed against the current fakeStore
// (internal/app); a future sqlite vector-contract test asserts the same
// outcome from the real store.
//
// One vector, TaskRetry, pins an ACCEPTED outcome's shape rather than a
// refusal: the values hop task retry renders into the shared grammar
// lines, which the fake and the real store must return identically.
package storevectors

import (
	"strings"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// TaskCreateSelfDependencyReason is TaskCreateSelfDependency's expected
// refusal reason token.
const TaskCreateSelfDependencyReason = app.GrammarReasonDependencyCycle

// TaskCreateSelfDependency returns a PlanStore.CreateTask request whose
// sole dependency is its own not-yet-created task id — the cheapest
// possible malformed dependency graph, needing no pre-existing graph
// state. Refused app.WorkflowRefused (reason TaskCreateSelfDependencyReason),
// detail "dependency is not in this run": CreateTask resolves DependsOn
// against already-persisted tasks before it ever reaches acyclicity, and
// the new task's own id can never be found among them (it does not exist
// until this call succeeds) — so no caller can construct a genuine
// multi-node cycle through this port at all: every dependency edge is
// created pointing only at an already-existing task, so the persisted
// graph is a DAG by construction. internal/domain/run's own
// ValidateAcyclic check exists as defense in depth for exactly that
// reason, not because this port can reach it.
func TaskCreateSelfDependency(runID identity.RunID, session identity.SessionID, incarnation identity.IncarnationID, taskID identity.TaskID) app.TaskCreate {
	return app.TaskCreate{
		ID: taskID, RunID: runID, Session: session, IncarnationID: incarnation,
		Title: "self-dependent task", InstructionsPath: "/state/instructions.md", InstructionsDigest: "instructions-digest",
		DependsOn: []identity.TaskID{taskID},
	}
}

// TaskCreateRequestIDConflictReason is TaskCreateRequestIDConflictSecond's
// expected refusal reason token.
const TaskCreateRequestIDConflictReason = app.GrammarReasonConflicting

// TaskCreateRequestIDConflictFirst is the SETUP half of the reused-
// request-ID vector: submitted first against a store, it is accepted.
// TaskCreateRequestIDConflictSecond shares its requestID but a different
// Title; submitted second against the SAME store, it is refused
// app.WorkflowRefused (reason TaskCreateRequestIDConflictReason), detail
// "request id reused with different content" — section 7's canonical
// request-digest rule (an identical retry is idempotent; a conflicting
// reuse is refused). The request key is (run, verb, request id) alone, so
// the two calls may share or differ in every other field including their
// task id.
func TaskCreateRequestIDConflictFirst(runID identity.RunID, session identity.SessionID, incarnation identity.IncarnationID, taskID identity.TaskID, requestID string) app.TaskCreate {
	return app.TaskCreate{
		ID: taskID, RunID: runID, Session: session, IncarnationID: incarnation,
		Title: "first content", InstructionsPath: "/state/instructions.md", InstructionsDigest: "instructions-digest",
		RequestID: requestID,
	}
}

// TaskCreateRequestIDConflictSecond is the CONFLICTING half of the
// reused-request-ID vector; see TaskCreateRequestIDConflictFirst.
func TaskCreateRequestIDConflictSecond(runID identity.RunID, session identity.SessionID, incarnation identity.IncarnationID, taskID identity.TaskID, requestID string) app.TaskCreate {
	return app.TaskCreate{
		ID: taskID, RunID: runID, Session: session, IncarnationID: incarnation,
		Title: "conflicting different content", InstructionsPath: "/state/instructions.md", InstructionsDigest: "instructions-digest",
		RequestID: requestID,
	}
}

// TaskCreateNonManagerCallerReason is TaskCreateNonManagerCaller's expected
// refusal reason token.
const TaskCreateNonManagerCallerReason = app.GrammarReasonNotManager

// TaskCreateNonManagerCaller returns a CreateTask request from a session
// that is not the run's manager (any implementer/reviewer session, from
// the caller's own fixture) — refused app.WorkflowRefused (reason
// TaskCreateNonManagerCallerReason), detail "caller is not the run's
// manager": PlanStore.CreateTask is a manager-only verb (section 6's
// one-level delegation — "workers get no verb that creates work").
func TaskCreateNonManagerCaller(runID identity.RunID, nonManagerSession identity.SessionID, nonManagerIncarnation identity.IncarnationID, taskID identity.TaskID) app.TaskCreate {
	return app.TaskCreate{
		ID: taskID, RunID: runID, Session: nonManagerSession, IncarnationID: nonManagerIncarnation,
		Title: "task created by a non-manager", InstructionsPath: "/state/instructions.md", InstructionsDigest: "instructions-digest",
	}
}

// TaskCreateOversizedTitleReason is TaskCreateOversizedTitle's expected
// refusal reason token.
const TaskCreateOversizedTitleReason = app.GrammarReasonMalformed

// TaskCreateOversizedTitle returns a CreateTask request whose Title
// exceeds app.TaskTitleLimit by one byte — refused app.WorkflowMalformed
// (reason TaskCreateOversizedTitleReason), detail "title is empty or
// exceeds the size bound": CreateTask validates title/instructions bounds
// INSIDE its own transaction (section 3), independent of usecase_plan.go's
// own pre-check, so a caller reaching the port directly (bypassing the
// CreateTask driving use case) is still refused.
func TaskCreateOversizedTitle(runID identity.RunID, session identity.SessionID, incarnation identity.IncarnationID, taskID identity.TaskID) app.TaskCreate {
	return app.TaskCreate{
		ID: taskID, RunID: runID, Session: session, IncarnationID: incarnation,
		Title:              strings.Repeat("x", app.TaskTitleLimit+1),
		InstructionsPath:   "/state/instructions.md",
		InstructionsDigest: "instructions-digest",
	}
}

// AckMessageStaleIncarnationReason is AckMessageStaleIncarnation's expected
// refusal reason token.
const AckMessageStaleIncarnationReason = app.GrammarReasonStale

// AckMessageStaleIncarnation returns a MessagingStore.AckMessage request
// carrying staleIncarnation, which must be the EXACT incarnation the
// message was delivered to, now superseded by a fresh one — refused
// app.AckRefused (reason AckMessageStaleIncarnationReason): the delivery
// row satisfies the (session, incarnation) delivery check (AcceptAck's
// first gate), so the refusal reaches the currency check and observes the
// binding is no longer current, ErrStaleAck. An incarnation that was
// never delivered to at all is a DIFFERENT refusal shape (ErrNotDelivered,
// checked first — receipt-before-eligibility): the caller's own fixture
// must supersede the SAME incarnation it delivered through, never swap in
// an unrelated one, or this vector proves the wrong thing.
func AckMessageStaleIncarnation(runID identity.RunID, messageID identity.MessageID, session identity.SessionID, staleIncarnation identity.IncarnationID) app.MessageAck {
	return app.MessageAck{RunID: runID, MessageID: messageID, SessionID: session, IncarnationID: staleIncarnation}
}

// AckMessageUnknownMessageReason is AckMessageUnknownMessage's expected
// refusal reason token.
const AckMessageUnknownMessageReason = app.GrammarReasonNotFound

// AckMessageUnknownMessage returns a MessagingStore.AckMessage request
// naming a message id that does not exist in the run at all — refused
// app.AckRefused (reason AckMessageUnknownMessageReason), detail "unknown
// message": receipt-before-eligibility resolves the message before ever
// checking the acking session or any delivery.
func AckMessageUnknownMessage(runID identity.RunID, unknownMessageID identity.MessageID, session identity.SessionID, incarnation identity.IncarnationID) app.MessageAck {
	return app.MessageAck{RunID: runID, MessageID: unknownMessageID, SessionID: session, IncarnationID: incarnation}
}

// AckMessageNotDeliveredReason is AckMessageNotDelivered's expected
// refusal reason token.
const AckMessageNotDeliveredReason = app.GrammarReasonNotDelivered

// AckMessageNotDelivered returns a MessagingStore.AckMessage request for
// an existing, queued message that has never been delivered to session at
// all (the caller's own fixture creates the message through an ordinary
// send and must not fetch it first) — refused app.AckRefused (reason
// AckMessageNotDeliveredReason): a first ack requires a delivery row for
// the acking session itself, which proves the recipient actually pulled
// the message before claiming to have read it.
func AckMessageNotDelivered(runID identity.RunID, messageID identity.MessageID, session identity.SessionID, incarnation identity.IncarnationID) app.MessageAck {
	return app.MessageAck{RunID: runID, MessageID: messageID, SessionID: session, IncarnationID: incarnation}
}

// MessageSendAnswerUnknownQuestionReason is MessageSendAnswerUnknownQuestion's
// expected refusal reason token.
const MessageSendAnswerUnknownQuestionReason = app.GrammarReasonMalformed

// MessageSendAnswerUnknownQuestion returns a MessagingStore.SendMessage
// request (Kind answer) whose ReplyTo id does not exist in the run —
// refused app.MessageMalformed (reason MessageSendAnswerUnknownQuestionReason),
// detail "unknown question": an answer's eligibility starts with resolving
// the referenced question before anything else (receipt-before-
// eligibility, section 7).
func MessageSendAnswerUnknownQuestion(runID identity.RunID, session identity.SessionID, senderAddress run.Address, incarnation identity.IncarnationID, answerID, unknownQuestionID identity.MessageID, bodyPath, bodyDigest string, bodyBytes int64) app.MessageSend {
	return app.MessageSend{
		ID: answerID, RunID: runID, Sender: run.SessionPrincipal(session), SenderAddress: senderAddress,
		IncarnationID: incarnation, Kind: run.MessageAnswer, ReplyTo: &unknownQuestionID,
		BodyPath: bodyPath, BodyDigest: bodyDigest, BodyBytes: bodyBytes,
	}
}

// MessageSendCrossRunReason is MessageSendCrossRun's expected refusal
// reason token.
const MessageSendCrossRunReason = app.GrammarReasonUnauthorized

// MessageSendCrossRun returns a MessagingStore.SendMessage request whose
// sender session belongs to a DIFFERENT run than claimedRunID names —
// refused app.MessageRefused (reason MessageSendCrossRunReason), detail
// "session does not belong to this run": every messaging verb resolves
// the caller's session and requires session.RunID == the request's stated
// run; the stated RunID is a caller-supplied field, never authoritative
// on its own. Kind is fixed to MessageQuestion: the cross-run check runs
// before addressing legality, but callers must still pass a
// (senderAddress, recipient) pair ValidateSendAddressing would otherwise
// accept (e.g. ManagerAddress to HumanAddress), or a passing test would
// prove nothing — an addressing refusal and a cross-run refusal share the
// same outcome kind AND the same reason token (both Unauthorized), so
// only the Detail text (never asserted by token-checking tests) actually
// distinguishes them.
func MessageSendCrossRun(claimedRunID identity.RunID, session identity.SessionID, senderAddress run.Address, incarnation identity.IncarnationID, messageID identity.MessageID, recipient run.Address, bodyPath, bodyDigest string, bodyBytes int64) app.MessageSend {
	return app.MessageSend{
		ID: messageID, RunID: claimedRunID, Sender: run.SessionPrincipal(session), SenderAddress: senderAddress,
		IncarnationID: incarnation, Recipient: recipient, Kind: run.MessageQuestion,
		BodyPath: bodyPath, BodyDigest: bodyDigest, BodyBytes: bodyBytes,
	}
}

// MessageFetchCrossRun returns a MessagingStore.FetchNextMessage request
// whose session belongs to a DIFFERENT run than claimedRunID names —
// refused app.ErrMessagingUnauthorized: FetchNextMessage independently
// re-derives the session's own run, current incarnation and resolved
// address rather than trusting any of MessageFetch's caller-supplied
// fields.
func MessageFetchCrossRun(claimedRunID identity.RunID, session identity.SessionID, incarnation identity.IncarnationID, address run.Address) app.MessageFetch {
	return app.MessageFetch{RunID: claimedRunID, SessionID: session, IncarnationID: incarnation, Address: address}
}

// AckMessageCrossRunReason is AckMessageCrossRun's expected refusal reason
// token.
const AckMessageCrossRunReason = app.GrammarReasonUnauthorized

// AckMessageCrossRun returns a MessagingStore.AckMessage request whose
// session belongs to a DIFFERENT run than claimedRunID names — refused
// app.AckRefused (reason AckMessageCrossRunReason), detail "session does
// not belong to this run".
func AckMessageCrossRun(claimedRunID identity.RunID, messageID identity.MessageID, session identity.SessionID, incarnation identity.IncarnationID) app.MessageAck {
	return app.MessageAck{RunID: claimedRunID, MessageID: messageID, SessionID: session, IncarnationID: incarnation}
}

// ReviewSubmitForeignReviewer returns a ReviewStore.SubmitReview request
// whose submitting session is a live, currently-bound reviewer that is
// NOT the claimed attempt's own reviewer session: a reviewer of another
// run, or one assigned to a different review attempt of the same run (the
// consuming test's fixture decides which shape it builds — both must be
// refused identically). foreignIncarnation is that session's own CURRENT
// incarnation, so nothing but the session-to-attempt binding can be the
// refusal's cause. Refused app.ReviewStale, detail "caller is not the
// review task's reviewer session", with NO review row and NO state
// transition committed: the acceptance context is never assembled from a
// foreign session's binding or launch claim, which would otherwise let a
// live reviewer elsewhere complete this attempt.
func ReviewSubmitForeignReviewer(runID identity.RunID, taskID identity.TaskID, attemptID identity.AttemptID, foreignSession identity.SessionID, foreignIncarnation identity.IncarnationID, reviewID identity.ReviewID, subjectCommitOID, subjectTreeOID, reasonsPath, reasonsDigest string) app.ReviewSubmission {
	return app.ReviewSubmission{
		ID: reviewID, RunID: runID, TaskID: taskID, AttemptID: attemptID,
		Session: foreignSession, IncarnationID: foreignIncarnation,
		SubjectCommitOID: subjectCommitOID, SubjectTreeOID: subjectTreeOID,
		Verdict: run.VerdictApprove, ReasonsPath: reasonsPath, ReasonsDigest: reasonsDigest,
	}
}

// TaskRetryReason is TaskRetry's expected reason token: empty, since both
// outcomes it pins are successes.
const TaskRetryReason = ""

// TaskRetry returns a PlanStore.RequestRetry request from the run's
// current manager for taskID, which the consuming test's own fixture has
// put in needs-rework behind a terminal prior attempt below the retry
// limit. Not a refusal: the first submission is app.WorkflowAccepted with
// TaskSeq equal to the task's own seq and AttemptNumber equal to the
// prior attempt's number plus one; the identical request submitted again
// (same requestID, same content) is app.WorkflowDuplicate carrying the
// SAME TaskSeq and AttemptNumber, read back from the original acceptance.
// Both carry TaskRetryReason. These are exactly the values hop task retry
// renders as `retry accepted t<seq> attempt <n>` / `duplicate t<seq>
// attempt <n>` (app.GrammarRetryAcceptedLine / GrammarRetryDuplicateLine).
func TaskRetry(runID identity.RunID, manager identity.SessionID, incarnation identity.IncarnationID, taskID identity.TaskID, requestID string) app.RetryRequest {
	return app.RetryRequest{
		TaskID: taskID, RunID: runID, Session: manager, IncarnationID: incarnation,
		Reason: "retry the terminal attempt", RequestID: requestID,
	}
}
