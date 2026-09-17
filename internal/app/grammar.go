package app

import (
	"strconv"
	"time"
)

// This file is the section 7 worker-protocol grammar's ONE source of truth
// (docs/plan/phase-3-design.md, "The worker protocol grammar"): the verb
// spellings, every worker-facing first line, the fixed follow-on lines and
// the enumerated refusal reason tokens. Phase 2's third escaped defect was
// a worker-facing protocol line the real CLI never rendered; the fix is
// the class, not the instance — templates, cmd/hop and the test fixtures
// all QUOTE these constants (the fixture worker's standalone source cannot
// import this package and mirrors them byte for byte, citing this file),
// and TestGoldenGrammar pins every constant and rendered line byte-exactly
// so any drift fails there first. cmd/hop's real-binary grammar contract
// tests (slice 6, design section 11 L1569) execute each verb against the
// built binary and assert its first line against these same constants.
//
// Rendering conventions, fixed here so every consumer agrees byte for
// byte: a principal renders as the sender session's UUID, or the literal
// "human" or "controller" (GrammarPrincipal); times render as UTC RFC 3339
// (GrammarTime); durations render as Go duration strings
// (time.Duration.String, e.g. "50s"); task labels render as "t<seq>" and
// attempt numbers as plain decimals.

// Verb spellings: the words after the hop executable, exactly as the
// templates and the crib quote them.
const (
	GrammarVerbResultSubmit = "result submit"
	GrammarVerbMsgNext      = "msg next"
	GrammarVerbMsgWait      = "msg wait"
	GrammarVerbMsgShow      = "msg show"
	GrammarVerbMsgAck       = "msg ack"
	GrammarVerbMsgSend      = "msg send"
	GrammarVerbTaskCreate   = "task create"
	GrammarVerbTaskRetry    = "task retry"
	GrammarVerbPlanClose    = "plan close"
	GrammarVerbReviewSubmit = "review submit"
	GrammarVerbAnswer       = "answer"
)

// Fixed whole-line constants.
const (
	// GrammarTransientNotRunningLine is hop result submit's Phase 2
	// retryable line, unchanged: the attempt is launching or relaunching
	// with an unsettled claim.
	GrammarTransientNotRunningLine = "transient: attempt not yet running; retry"
	// GrammarTransientUndeliveredLine is the feature-mode retryable line
	// of hop result submit AND hop review submit: the section 5 mailbox
	// rule — drain before submitting.
	GrammarTransientUndeliveredLine = "transient: undelivered messages; drain with hop msg next, ack, then resubmit"
	// GrammarTransientRunNotRunningLine is the retryable line of hop task
	// create, hop task retry, hop plan close and hop msg send: the run is
	// created, launching, resuming or completing and can still reach
	// running (run.ErrRunNotYetRunning). The request changed nothing; the
	// caller reruns the same command after a short delay.
	GrammarTransientRunNotRunningLine = "transient: run not yet running; retry"
	// GrammarTransientPrefix begins every retryable first line (exit 1).
	GrammarTransientPrefix = "transient: "
	// GrammarMsgNoneLine is hop msg next's empty-queue line; the fetch
	// commits nothing.
	GrammarMsgNoneLine = "none: no queued message"
	// GrammarPlanClosedLine / GrammarPlanCloseDuplicateLine are hop plan
	// close's success and request-ID-retry lines.
	GrammarPlanClosedLine         = "plan closed"
	GrammarPlanCloseDuplicateLine = "duplicate plan closed"
	// GrammarRefusalPrefix begins every refusal's first line; the reason
	// token follows, detail lines after (exit 1).
	GrammarRefusalPrefix = "refused: "
)

// Refusal reason tokens: the enumerated vocabulary after
// GrammarRefusalPrefix. cmd/hop maps a verb's outcome kind and detail to
// exactly one token; the detail itself goes on the following lines, never
// into the token.
const (
	// GrammarReasonNotFound: the id names nothing in this run — an unknown
	// message (hop msg show) or one belonging to a different run, reported
	// identically so envelope content never leaks across runs.
	GrammarReasonNotFound = "not-found"
	// GrammarReasonUnauthorized: the caller session does not belong to the
	// stated run, or its resolved address or incarnation disagrees with
	// the request.
	GrammarReasonUnauthorized = "unauthorized"
	// GrammarReasonMalformed: parse or bounds failure (section 7 step 1).
	GrammarReasonMalformed = "malformed"
	// GrammarReasonConflicting: a request ID reused with different
	// content, or a resubmission conflicting with accepted content; the
	// accepted entity is never disturbed.
	GrammarReasonConflicting = "conflicting"
	// GrammarReasonStale: a stale incarnation, or a submission against an
	// entity no longer eligible to accept it.
	GrammarReasonStale = "stale"
	// GrammarReasonNotDelivered: an ack of a message never delivered to
	// the acking session itself.
	GrammarReasonNotDelivered = "not-delivered"
	// GrammarReasonRunNotAccepting: a manager verb or message send against
	// a run that can never accept one again (completed, failed, stopping or
	// stopped, or a stop request; run.ErrRunNotAccepting).
	GrammarReasonRunNotAccepting = "run-not-accepting"
	// GrammarReasonMailboxClosed: a send addressed to a task whose mailbox
	// admission has closed.
	GrammarReasonMailboxClosed = "mailbox-closed"
	// GrammarReasonNotManager: a plan verb (task create/retry, plan close)
	// from a session that is not the run's current manager.
	GrammarReasonNotManager = "not-manager"
	// GrammarReasonNotReviewer: a review submission from a session whose
	// role is not reviewer, or that is not the review attempt's current
	// session.
	GrammarReasonNotReviewer = "not-reviewer"
	// GrammarReasonDependencyCycle: a task create whose dependency edges
	// would close a cycle.
	GrammarReasonDependencyCycle = "dependency-cycle"
	// GrammarReasonEmptyPlan: a plan close with zero implement tasks.
	GrammarReasonEmptyPlan = "empty-plan"
	// GrammarReasonRetryNotTerminal: a retry whose prior attempt is not
	// terminal.
	GrammarReasonRetryNotTerminal = "retry-not-terminal"
	// GrammarReasonRetryLimit: a retry against a task at its frozen retry
	// limit.
	GrammarReasonRetryLimit = "retry-limit"
	// GrammarReasonSubjectMismatch: a verdict whose subject does not equal
	// the review task's frozen subject.
	GrammarReasonSubjectMismatch = "subject-mismatch"
)

// GrammarRefusalLine renders a refusal's first line from one enumerated
// reason token.
func GrammarRefusalLine(reasonToken string) string {
	return GrammarRefusalPrefix + reasonToken
}

// GrammarPrincipal renders a message sender for the from= field: the
// sender session's UUID, or the literal principal kind ("human",
// "controller") for the two session-less principals.
func GrammarPrincipal(kind, sessionID string) string {
	if kind == "session" {
		return sessionID
	}
	return kind
}

// GrammarTime renders a grammar timestamp: UTC RFC 3339.
func GrammarTime(at time.Time) string {
	return at.UTC().Format(time.RFC3339)
}

// GrammarResultAcceptedLine / GrammarResultDuplicateLine are hop result
// submit's success lines.
func GrammarResultAcceptedLine(resultID string) string { return "accepted " + resultID }

// GrammarResultDuplicateLine is the idempotent-resubmission line.
func GrammarResultDuplicateLine(resultID string) string { return "duplicate " + resultID }

// GrammarMessageLine renders hop msg next/wait's first line. replyTo,
// relayOf and origin are optional ("" omits the field); origin appears on
// an answer whose reply-to question carries relay provenance — the
// ORIGINAL question's id, resolved server-side.
func GrammarMessageLine(messageID, kind, from, replyTo, relayOf, origin string) string {
	line := "message " + messageID + " kind=" + kind + " from=" + from
	if replyTo != "" {
		line += " reply-to=" + replyTo
	}
	if relayOf != "" {
		line += " relay-of=" + relayOf
	}
	if origin != "" {
		line += " origin=" + origin
	}
	return line
}

// GrammarBodyLine names the durable body artifact, absolute path only.
func GrammarBodyLine(absPath string) string { return "body: " + absPath }

// GrammarAckHintLine is hop msg next/wait's third line: the exact ack
// invocation for the served message.
func GrammarAckHintLine(messageID string) string { return "ack: hop msg ack " + messageID }

// GrammarMsgWaitNoneLine is hop msg wait's timeout line.
func GrammarMsgWaitNoneLine(timeout time.Duration) string {
	return "none: no message within " + timeout.String() + "; run hop msg wait again"
}

// GrammarMessageShowLine renders hop msg show's first line: the envelope
// with its recipient address and enqueue sequence. replyTo and relayOf are
// optional ("" omits the field).
func GrammarMessageShowLine(messageID, kind, from, to, replyTo, relayOf string, seq int) string {
	line := "message " + messageID + " kind=" + kind + " from=" + from + " to=" + to
	if replyTo != "" {
		line += " reply-to=" + replyTo
	}
	if relayOf != "" {
		line += " relay-of=" + relayOf
	}
	return line + " seq=" + strconv.Itoa(seq)
}

// GrammarDeliveredLine is one hop msg show delivery-history line,
// re-serves included.
func GrammarDeliveredLine(sessionID string, at time.Time) string {
	return "delivered: " + sessionID + " " + GrammarTime(at)
}

// GrammarAcknowledgedLine is hop msg show's ack line, rendered when the
// message is acknowledged.
func GrammarAcknowledgedLine(at time.Time) string {
	return "acknowledged: " + GrammarTime(at)
}

// GrammarAckAcceptedLine / GrammarAckDuplicateLine are hop msg ack's
// success lines.
func GrammarAckAcceptedLine(messageID string) string { return "acknowledged " + messageID }

// GrammarAckDuplicateLine is the idempotent re-ack line.
func GrammarAckDuplicateLine(messageID string) string { return "duplicate " + messageID }

// GrammarSentLine / GrammarSendDuplicateLine are hop msg send's success
// lines.
func GrammarSentLine(messageID string) string { return "sent " + messageID }

// GrammarSendDuplicateLine is the request-ID-retry line.
func GrammarSendDuplicateLine(messageID string) string { return "duplicate " + messageID }

// GrammarTaskCreatedLine / GrammarTaskCreateDuplicateLine are hop task
// create's success lines.
func GrammarTaskCreatedLine(taskID string, seq int) string {
	return "task " + taskID + " t" + strconv.Itoa(seq) + " created"
}

// GrammarTaskCreateDuplicateLine is the request-ID-retry line.
func GrammarTaskCreateDuplicateLine(taskID string, seq int) string {
	return "duplicate " + taskID + " t" + strconv.Itoa(seq)
}

// GrammarRetryAcceptedLine / GrammarRetryDuplicateLine are hop task
// retry's success lines; the attempt number is named in the accepting
// response because RequestRetry reserves it immediately.
func GrammarRetryAcceptedLine(taskSeq, attemptNumber int) string {
	return "retry accepted t" + strconv.Itoa(taskSeq) + " attempt " + strconv.Itoa(attemptNumber)
}

// GrammarRetryDuplicateLine is the request-ID-retry line.
func GrammarRetryDuplicateLine(taskSeq, attemptNumber int) string {
	return "duplicate t" + strconv.Itoa(taskSeq) + " attempt " + strconv.Itoa(attemptNumber)
}

// GrammarVerdictAcceptedLine / GrammarVerdictDuplicateLine are hop review
// submit's success lines.
func GrammarVerdictAcceptedLine(reviewID string) string { return "verdict accepted " + reviewID }

// GrammarVerdictDuplicateLine is the idempotent-resubmission line.
func GrammarVerdictDuplicateLine(reviewID string) string { return "duplicate " + reviewID }
