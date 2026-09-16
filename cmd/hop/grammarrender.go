package main

import (
	"strings"

	"github.com/johnlanda/hop/internal/app"
)

// This file maps a worker-plumbing use case's coarse outcome kind (and,
// where that alone is not enough, its free-text Detail) to the section 7
// grammar's enumerated reason token — grammar.go's own doc comment on
// GrammarRefusalPrefix assigns exactly this job to cmd/hop: "cmd/hop maps
// a verb's outcome kind and detail to exactly one token; the detail
// itself goes on the following lines, never into the token." The
// messaging and review ports already return a distinctly-named outcome
// kind per refusal shape (e.g. "refused-mailbox-closed",
// "refused-run-not-accepting"), so those map by kind alone; the plan
// port collapses every refusal into one generic "refused" kind, so its
// mapping classifies by Detail text instead, matching the exact,
// deliberately-chosen literal strings internal/adapters/sqlite/plan.go
// records today. A detail this table does not recognize falls back to
// "unauthorized" rather than inventing a new token; the real text is
// still printed as the follow-on line, so nothing is ever silently lost.

// messageRefusalToken classifies a SendMessage/Answer MessageOutcomeKind.
func messageRefusalToken(outcome string) string {
	switch outcome {
	case "malformed":
		return app.GrammarReasonMalformed
	case "conflicting":
		return app.GrammarReasonConflicting
	case "refused-run-not-accepting":
		return app.GrammarReasonRunNotAccepting
	case "refused-mailbox-closed":
		return app.GrammarReasonMailboxClosed
	default: // "refused": every current call site is a cross-run mismatch.
		return app.GrammarReasonUnauthorized
	}
}

// ackRefusalToken classifies an AckMessage MessageAckOutcomeKind's Detail:
// AckRefused alone covers an unknown message, a cross-run session and an
// undelivered message (ErrNotDelivered), which the outcome kind cannot
// distinguish.
func ackRefusalToken(detail string) string {
	switch detail {
	case "unknown message":
		return app.GrammarReasonNotFound
	case "session does not belong to this run":
		return app.GrammarReasonUnauthorized
	default:
		// Every other AckMessage refusal observed today is
		// ErrNotDelivered's own message text ("message ... was not
		// delivered to session ...").
		if strings.Contains(detail, "not delivered") {
			return app.GrammarReasonNotDelivered
		}
		return app.GrammarReasonUnauthorized
	}
}

// reviewRefusalToken classifies a SubmitReviewVerdict ReviewOutcomeKind.
func reviewRefusalToken(outcome string) string {
	switch outcome {
	case "malformed":
		return app.GrammarReasonMalformed
	case "conflicting":
		return app.GrammarReasonConflicting
	case "stale":
		return app.GrammarReasonStale
	default:
		return app.GrammarReasonUnauthorized
	}
}

// planRefusalToken classifies a CreateTask/RequestRetry/ClosePlan
// WorkflowOutcomeKind: "malformed" outcomes map directly, but every
// "refused" outcome shares one generic kind across every plan verb, so
// classification falls to Detail — the literal strings
// internal/adapters/sqlite/plan.go records for each refusal today.
func planRefusalToken(outcome, detail string) string {
	if outcome == "malformed" {
		return app.GrammarReasonMalformed
	}
	switch detail {
	case "request id reused with different content", "a retry request is already pending for this task":
		return app.GrammarReasonConflicting
	case "caller is not the run's manager":
		return app.GrammarReasonNotManager
	case "incarnation is not current":
		return app.GrammarReasonStale
	case "dependency is not in this run":
		return app.GrammarReasonDependencyCycle
	case "task is not needs-rework", "prior attempt is not terminal":
		return app.GrammarReasonRetryNotTerminal
	case "retry limit reached":
		return app.GrammarReasonRetryLimit
	case "plan has no implement task":
		return app.GrammarReasonEmptyPlan
	}
	switch {
	case strings.HasPrefix(detail, "run: dependency cycle"):
		return app.GrammarReasonDependencyCycle
	case strings.HasPrefix(detail, "run: run is not accepting this request"):
		return app.GrammarReasonRunNotAccepting
	default:
		return app.GrammarReasonUnauthorized
	}
}

// renderRefusal renders a refused/malformed outcome as the grammar's
// two-line shape: the fixed "refused: <token>" first line, then detail as
// a plain follow-on line (never folded into the token itself) whenever
// non-empty.
func renderRefusal(token, detail string) []string {
	lines := []string{app.GrammarRefusalLine(token)}
	if detail != "" {
		lines = append(lines, detail)
	}
	return lines
}
