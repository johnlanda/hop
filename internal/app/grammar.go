package app

import (
	"strconv"
	"strings"
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
	// GrammarTransientNotRunningLine is the retryable line of hop result
	// submit (Phase 2, unchanged) AND hop review submit: the attempt is
	// launching or relaunching with an unsettled claim
	// (TransientAttemptNotRunning).
	GrammarTransientNotRunningLine = "transient: attempt not yet running; retry"
	// GrammarTransientUndeliveredLine is the feature-mode retryable line
	// of hop result submit AND hop review submit: the section 5 mailbox
	// rule — drain before submitting (TransientUndeliveredMessages).
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
	// stated run, or its resolved address disagrees with the request or
	// with the addressing rules — including an answer from a session whose
	// address is not the question's recipient (no session answers a human
	// question). An incarnation that is not current is stale instead.
	GrammarReasonUnauthorized = "unauthorized"
	// GrammarReasonMalformed: parse or bounds failure (section 7 step 1).
	GrammarReasonMalformed = "malformed"
	// GrammarReasonConflicting: a request ID reused with different
	// content, or a resubmission conflicting with accepted content; the
	// accepted entity is never disturbed.
	GrammarReasonConflicting = "conflicting"
	// GrammarReasonStale: a stale incarnation; a send or ack from a session
	// that is no longer its address's current session; or a submission
	// against an entity no longer eligible to accept it.
	GrammarReasonStale = "stale"
	// GrammarReasonNotDelivered: an ack of a message never delivered to
	// the acking session itself.
	GrammarReasonNotDelivered = "not-delivered"
	// GrammarReasonRunNotAccepting: a manager verb or message send against
	// a run that can never accept one again (completed, failed, stopping or
	// stopped, or a stop request; run.ErrRunNotAccepting).
	GrammarReasonRunNotAccepting = "run-not-accepting"
	// GrammarReasonMailboxClosed: a send, or an answer whose derived
	// destination is a task, addressed to a task whose mailbox admission
	// has closed.
	GrammarReasonMailboxClosed = "mailbox-closed"
	// GrammarReasonNotManager: a plan verb (task create/retry, plan close)
	// from a session that is not the run's current manager.
	GrammarReasonNotManager = "not-manager"
	// GrammarReasonNotReviewer: a first review submission from a session
	// that is not the review attempt's own reviewer session (another role,
	// another run, or another attempt).
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

// GrammarSubmissionTransientLine renders hop result submit's and hop
// review submit's retryable first line for a typed transient reason: the
// one line that tells the worker what to do before rerunning. ok is false
// for any other reason; a caller then prints no protocol line at all rather
// than guess one.
func GrammarSubmissionTransientLine(reason TransientReason) (line string, ok bool) {
	switch reason {
	case TransientAttemptNotRunning:
		return GrammarTransientNotRunningLine, true
	case TransientUndeliveredMessages:
		return GrammarTransientUndeliveredLine, true
	default:
		return "", false
	}
}

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

// GrammarResultRefusalLine renders hop result submit's final non-success
// first line: the outcome kind ("stale", "conflicting" or "malformed", each
// spelled as its GrammarReason token), then ": " and the detail when there
// is one. Unlike the other verbs' `refused: <token>` lines, the detail is
// part of the first line.
func GrammarResultRefusalLine(kind, detail string) string {
	if detail == "" {
		return kind
	}
	return kind + ": " + detail
}

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

// `hop status -run`'s feature-mode detail lines (docs/plan/phase-3-design.md
// section 7's "Idle with pending deliveries" and section 10's `hop status`
// row): fixed line text the fixture manager and the deterministic
// scenarios parse, exactly like the worker-protocol lines above. Task and
// review labels ("t<seq>") are resolved by the caller (cmd/hop, against
// the task table it is also rendering) before reaching these renderers —
// this file only assembles already-resolved strings into fixed shapes.

// GrammarAttentionLine renders section 7's per-mailbox attention condition
// line VERBATIM: the in-flight clause appears only when a message is
// delivered and unacknowledged (inFlightMessageID != ""), the queued
// clause only when the queue is non-empty (queuedCount > 0) — at least one
// is always present, since a mailbox with neither never reaches this
// renderer. address is already resolved to its display form ("manager",
// "human", or "task:<uuid> (t<seq>)" — the uuid form section 7's message
// verbs themselves take, alongside the t<seq> label for readability).
func GrammarAttentionLine(address, inFlightMessageID string, inFlightAge time.Duration, queuedCount int, oldestQueuedAge time.Duration) string {
	var clauses []string
	if inFlightMessageID != "" {
		clauses = append(clauses, "in-flight "+inFlightAge.String()+" (message "+inFlightMessageID+")")
	}
	if queuedCount > 0 {
		clauses = append(clauses, "queued "+strconv.Itoa(queuedCount)+", oldest "+oldestQueuedAge.String())
	}
	return "attention: messages pending for " + address + ": " + strings.Join(clauses, ", ")
}

// GrammarTaskAddress renders a task mailbox's display address: the uuid
// form section 7's message verbs take, alongside its t<seq> label.
func GrammarTaskAddress(taskID, taskLabel string) string {
	return "task:" + taskID + " (" + taskLabel + ")"
}

// GrammarAttentionActionSession renders section 7's named human action for
// a manager or task mailbox in the Attention condition: when the session
// currently has a binding, name it (workspace/tab/pane) so the human knows
// exactly which pane to open; otherwise the generic instruction, since no
// pane can be named.
func GrammarAttentionActionSession(binding string) string {
	if binding == "" {
		return "open that session's pane and check that the agent is following its polling instructions"
	}
	return "open " + binding + " and check that the agent is following its polling instructions"
}

// GrammarAttentionActionHuman is section 7's named human action for the
// human mailbox in the Attention condition: humans have no pane to open.
const GrammarAttentionActionHuman = "answer pending human questions with hop answer"

// GrammarAttentionMarker is section 7's run-summary condition — appended
// to hop status's listing markers (cmd/hop's listingMarkers) whenever at
// least one mailbox is in the Attention condition, on both the bare
// listing and the detail block's state line.
const GrammarAttentionMarker = "blocked, needs attention"

// GrammarActionPrefix is the literal "action:" token beginning every
// nested human-action line hop status renders — under an attention
// condition, a worktree-create operation, or a retirement worktree row.
// Callers own their own surrounding indentation, which differs by
// context (the aligned detail block pads it; the flat retirement report
// list does not); this constant is only the shared word itself.
const GrammarActionPrefix = "action:"

// GrammarTaskLabel renders a task's stable display label: "t<seq>".
func GrammarTaskLabel(seq int) string { return "t" + strconv.Itoa(seq) }

// GrammarTaskLine renders one row of the feature-mode task table (section
// 10): label AND uuid (hop task retry takes the uuid), kind, state,
// dependencies (already joined into "(none)" or comma-separated t<seq>
// labels by the caller), attempt count, and — last, since a path may
// contain spaces — the worktree path ("(none)" when the task has none
// yet).
func GrammarTaskLine(label, taskID, kind, state, deps string, attempts int, worktree string) string {
	return "task " + label + " " + taskID + ": kind=" + kind + " state=" + state + " deps=" + deps +
		" attempts=" + strconv.Itoa(attempts) + " worktree=" + worktree
}

// GrammarIntegrationLine renders the run's most recently created
// integration row (section 10): identity, the task it merges (by label),
// state, and its object IDs when set ("(none)" otherwise, resolved by the
// caller).
func GrammarIntegrationLine(id, taskLabel, state, source, premerge, merge string) string {
	return "integration " + id + ": task=" + taskLabel + " state=" + state +
		" source=" + source + " premerge=" + premerge + " merge=" + merge
}

// GrammarShortfallVerdictRejected mirrors run.ShortfallVerdictRejected:
// the one EvaluateReadiness shortfall kind token the manager's standing
// instruction (renderManagerAssignment) names as its fix-task trigger.
// Kept as its own grammar constant, rather than an internal/domain/run
// import here, so this file's constants stay self-contained; parity with
// the domain constant is pinned by TestGoldenGrammar.
const GrammarShortfallVerdictRejected = "verdict-rejected"

// GrammarShortfallEvidenceInconsistent is the status read model's own
// shortfall kind token (ShortfallEvidenceInconsistent): rendered through
// GrammarShortfallLine with no task, value-free, in place of the check
// and verdict shortfalls when the recorded evidence about the head
// contradicts itself. The manager's standing instruction names it as
// never a rejection.
const GrammarShortfallEvidenceInconsistent = "evidence-inconsistent"

// GrammarShortfallLine renders one EvaluateReadiness shortfall verbatim
// (section 10): the kind token exactly as the domain defines it, plus the
// task's label AND uuid when the shortfall names a task
// (task-not-integrated) — taskLabel is "" for every other shortfall kind.
// verdict-rejected carries its own review identity instead: see
// GrammarVerdictRejectedLine.
func GrammarShortfallLine(kind, taskLabel, taskID string) string {
	line := "shortfall: " + kind
	if taskLabel != "" {
		line += " " + taskLabel + " " + taskID
	}
	return line
}

// GrammarVerdictRejectedLine renders the one shortfall kind that names a
// specific review rather than a task: the review's id and subject commit,
// then — last, since a path may contain spaces — its reasons artifact
// path, already resolved by the caller to the exact path the accepting
// transaction used as the controller notice's own body (templates.go's
// reviewReasonsPath) and already rendered safe by the caller's F2
// rendering boundary. A caller correlates this shortfall to a fetched
// controller notice by comparing reasonsPath against the notice's own
// body path (STATUS-1's manager verdict channel): the notice names no
// verdict, so this line is the only place a review's rejection and its
// identity are stated together.
func GrammarVerdictRejectedLine(reviewID, subjectCommitOID, safeReasonsPath string) string {
	return "shortfall: " + GrammarShortfallVerdictRejected + " review=" + reviewID + " subject=" + subjectCommitOID + " reasons=" + safeReasonsPath
}

// GrammarQuestionLine renders one pending human-addressed question
// (section 13 item 7's CLI-only human question channel): its id, age and
// body path.
func GrammarQuestionLine(messageID string, age time.Duration, bodyPath string) string {
	return "question " + messageID + " age=" + age.String() + " body: " + bodyPath
}

// GrammarAnswerInvocationLine renders the exact hop answer invocation for
// one pending question. The file path is deliberately the literal
// placeholder "<path>": no answer file exists yet — the human names one
// when they write their answer, exactly as the review assignment's
// "<approve|reject>" and "<absolute path>" placeholders name choices the
// artifact cannot make for its reader.
func GrammarAnswerInvocationLine(messageID string) string {
	return "hop " + GrammarVerbAnswer + " " + messageID + " --file <path>"
}

// GrammarSessionLine renders one row of the feature-mode per-session
// roles/bindings listing (section 10): identity, role, state, the task it
// is bound to (already resolved to a "t<seq>" label, "(none)" for the
// manager) and attempt number (0 for the manager), and its current
// binding summary ("(none)" when it has none).
func GrammarSessionLine(sessionID, role, state, taskLabel string, attemptNumber int, binding string) string {
	return "session " + sessionID + ": role=" + role + " state=" + state +
		" task=" + taskLabel + " attempt=" + strconv.Itoa(attemptNumber) + " binding=" + binding
}

// GrammarSessionRestartNote renders what the restart-lifecycle step did to
// one session, under that session's own line. disposition is one of the
// fixed RestartDisposition* values and nothing else, so no journal reason,
// lifetime token, pid, label or path can reach this surface. A manager's
// note renders on the manager's own session line, which is what makes a
// manager relaunch — the one that moves the run's lineage — separately
// visible from a worker's.
func GrammarSessionRestartNote(disposition string) string {
	return "restart: " + disposition
}

// Controller info notices to the manager (design section 7, "the
// controller notice grammar"): a task settlement's own consequence, or an
// integration settlement's. ONE line order applies to both notice shapes
// (renderTaskNotice in usecase_featurecheck.go, renderIntegrationNotice in
// usecase_featuresettle.go) so a reader — human or fixture — never tries
// two positions for the fact it acts on: the task consequence line is
// always FIRST, since it is the one fact every notice carries and the one
// the manager decides from. Line position is meaningful only because
// every line is exactly one line BY CONSTRUCTION: taskSeq is an int,
// integrationID a uuid, consequence and state typed values and obligation
// ids HOP uuids, none of which can carry a newline, while a reason or an
// evidence path is externally sourced (a check or git command's own
// detail text, a filesystem path) and renders through RenderExternal
// here — the one escaping boundary — so it can never inject a newline
// that forges a second line (a fake task-consequence line, say) or
// otherwise break the one-line-per-fact grammar a line-oriented reader
// depends on.

// GrammarNoticeTaskLine renders a controller notice's task-consequence
// line: what happened to the task (one of needs-rework, failed,
// interrupted). Always the notice body's first line, in both the
// per-task and the integration-settlement notice shapes.
func GrammarNoticeTaskLine(taskSeq int, consequence string) string {
	return "task " + GrammarTaskLabel(taskSeq) + " " + consequence
}

// GrammarNoticeIntegrationLine renders an integration-settlement notice's
// own line: which integration record settled, and how (one of conflicted,
// rolled-back). Second in that notice's body, after the task line —
// supporting evidence for why the task consequence follows, never the
// fact itself.
func GrammarNoticeIntegrationLine(integrationID, state string) string {
	return "integration " + integrationID + " " + state
}

// GrammarNoticeReasonLine renders a controller notice's reason line.
// reason is externally sourced (a check or git command's own detail
// text) and renders through RenderExternal, so this is always exactly
// one line.
func GrammarNoticeReasonLine(reason string) string {
	return "reason: " + RenderExternal(reason)
}

// GrammarNoticeEvidenceLine renders one evidence-path line of an
// integration-settlement notice (zero or more, one per path). path
// renders through RenderExternal, so this is always exactly one line.
func GrammarNoticeEvidenceLine(path string) string {
	return "evidence: " + RenderExternal(path)
}

// GrammarNoticeObligationsNoneLine is a failing task consequence's
// trailing orphaned-obligations line when there are none.
const GrammarNoticeObligationsNoneLine = "orphaned obligations: none"

// GrammarNoticeObligationsLine renders a failing task consequence's
// trailing orphaned-obligations line: the pending message ids left
// addressed to a task whose mailbox the failure closes, so nothing owed
// to it goes unnoticed.
func GrammarNoticeObligationsLine(obligationIDs []string) string {
	return "orphaned obligations: " + strings.Join(obligationIDs, " ")
}

// GrammarSessionLaunchCorroborationAction is the named human action for a
// session reconciling because another process on its pane carries the
// launch identity (SessionView.LaunchCorroborationPending). It states
// what the controller is doing about it — re-inspecting every pass, so
// the state clears itself once one observation is clean — and what the
// human does when it does not clear. Value-free: the session line above
// it already names the session and its pane.
const GrammarSessionLaunchCorroborationAction = "another process on this session's pane carries the launch identity, so the launch is not corroborated yet; " +
	"the controller re-inspects it every pass and needs nothing, and if this state persists, open that pane and check whether the harness was started through a process that forks it"
