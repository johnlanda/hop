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
package storevectors

import (
	"strings"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// TaskCreateSelfDependency returns a PlanStore.CreateTask request whose
// sole dependency is its own not-yet-created task id — the cheapest
// possible malformed dependency graph, needing no pre-existing graph
// state. Refused app.WorkflowRefused, detail "dependency is not in this
// run": CreateTask resolves DependsOn against already-persisted tasks
// before it ever reaches acyclicity, and the new task's own id can never
// be found among them (it does not exist until this call succeeds) — so
// no caller can construct a genuine multi-node cycle through this port at
// all: every dependency edge is created pointing only at an
// already-existing task, so the persisted graph is a DAG by construction.
// internal/domain/run's own ValidateAcyclic check exists as defense in
// depth for exactly that reason, not because this port can reach it.
func TaskCreateSelfDependency(runID identity.RunID, session identity.SessionID, incarnation identity.IncarnationID, taskID identity.TaskID) app.TaskCreate {
	return app.TaskCreate{
		ID: taskID, RunID: runID, Session: session, IncarnationID: incarnation,
		Title: "self-dependent task", InstructionsPath: "/state/instructions.md", InstructionsDigest: "instructions-digest",
		DependsOn: []identity.TaskID{taskID},
	}
}

// TaskCreateRequestIDConflictFirst is the SETUP half of the reused-
// request-ID vector: submitted first against a store, it is accepted.
// TaskCreateRequestIDConflictSecond shares its requestID but a different
// Title; submitted second against the SAME store, it is refused
// app.WorkflowRefused, detail "request id reused with different content"
// — section 7's canonical request-digest rule (an identical retry is
// idempotent; a conflicting reuse is refused). The request key is (run,
// verb, request id) alone, so the two calls may share or differ in every
// other field including their task id.
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

// TaskCreateNonManagerCaller returns a CreateTask request from a session
// that is not the run's manager (any implementer/reviewer session, from
// the caller's own fixture) — refused app.WorkflowRefused, detail "caller
// is not the run's manager": PlanStore.CreateTask is a manager-only verb
// (section 6's one-level delegation — "workers get no verb that creates
// work").
func TaskCreateNonManagerCaller(runID identity.RunID, nonManagerSession identity.SessionID, nonManagerIncarnation identity.IncarnationID, taskID identity.TaskID) app.TaskCreate {
	return app.TaskCreate{
		ID: taskID, RunID: runID, Session: nonManagerSession, IncarnationID: nonManagerIncarnation,
		Title: "task created by a non-manager", InstructionsPath: "/state/instructions.md", InstructionsDigest: "instructions-digest",
	}
}

// TaskCreateOversizedTitle returns a CreateTask request whose Title
// exceeds app.TaskTitleLimit by one byte — refused app.WorkflowMalformed,
// detail "title is empty or exceeds the size bound": CreateTask validates
// title/instructions bounds INSIDE its own transaction (section 3),
// independent of usecase_plan.go's own pre-check, so a caller reaching the
// port directly (bypassing the CreateTask driving use case) is still
// refused.
func TaskCreateOversizedTitle(runID identity.RunID, session identity.SessionID, incarnation identity.IncarnationID, taskID identity.TaskID) app.TaskCreate {
	return app.TaskCreate{
		ID: taskID, RunID: runID, Session: session, IncarnationID: incarnation,
		Title:              strings.Repeat("x", app.TaskTitleLimit+1),
		InstructionsPath:   "/state/instructions.md",
		InstructionsDigest: "instructions-digest",
	}
}

// AckMessageStaleIncarnation returns a MessagingStore.AckMessage request
// carrying an incarnation id that is NOT session's current, non-superseded
// one — refused app.AckRefused: section 7's ack rule requires the ACKING
// SESSION's current incarnation, since a predecessor's delivery proves
// nothing about what a successor session actually saw.
func AckMessageStaleIncarnation(runID identity.RunID, messageID identity.MessageID, session identity.SessionID, staleIncarnation identity.IncarnationID) app.MessageAck {
	return app.MessageAck{RunID: runID, MessageID: messageID, SessionID: session, IncarnationID: staleIncarnation}
}

// MessageSendAnswerUnknownQuestion returns a MessagingStore.SendMessage
// request (Kind answer) whose ReplyTo id does not exist in the run —
// refused app.MessageMalformed, detail "unknown question": an answer's
// eligibility starts with resolving the referenced question before
// anything else (receipt-before-eligibility, section 7).
func MessageSendAnswerUnknownQuestion(runID identity.RunID, session identity.SessionID, senderAddress run.Address, incarnation identity.IncarnationID, answerID, unknownQuestionID identity.MessageID, bodyPath, bodyDigest string, bodyBytes int64) app.MessageSend {
	return app.MessageSend{
		ID: answerID, RunID: runID, Sender: run.SessionPrincipal(session), SenderAddress: senderAddress,
		IncarnationID: incarnation, Kind: run.MessageAnswer, ReplyTo: &unknownQuestionID,
		BodyPath: bodyPath, BodyDigest: bodyDigest, BodyBytes: bodyBytes,
	}
}

// MessageSendCrossRun returns a MessagingStore.SendMessage request whose
// sender session belongs to a DIFFERENT run than claimedRunID names —
// refused app.MessageRefused, detail "session does not belong to this
// run": every messaging verb resolves the caller's session and requires
// session.RunID == the request's stated run; the stated RunID is a
// caller-supplied field, never authoritative on its own. Kind is fixed to
// MessageQuestion: the cross-run check runs before addressing legality,
// but callers must still pass a (senderAddress, recipient) pair
// ValidateSendAddressing would otherwise accept (e.g. ManagerAddress to
// HumanAddress), or a passing test would prove nothing — an addressing
// refusal and a cross-run refusal share the same outcome kind.
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

// AckMessageCrossRun returns a MessagingStore.AckMessage request whose
// session belongs to a DIFFERENT run than claimedRunID names — refused
// app.AckRefused, detail "session does not belong to this run".
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
