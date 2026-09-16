package main

import (
	"github.com/johnlanda/hop/internal/app"
)

// This file renders a worker-plumbing use case's refusal as the section 7
// grammar's enumerated reason token — grammar.go's own doc comment on
// GrammarRefusalPrefix assigns exactly this job to cmd/hop: "cmd/hop maps
// a verb's outcome kind and detail to exactly one token; the detail
// itself goes on the following lines, never into the token." Every
// worker-authority store (PlanStore, MessagingStore) and its fake set a
// typed Reason field directly at each decision point (errors.Is against a
// domain sentinel, or the adapter's own fixed decision) — never a literal
// Detail string or substring cmd/hop would have to re-parse; refusalToken
// only applies the shared fallback for the empty-reason defect case.
// ReviewStore's outcome kind alone already names a distinct reason per
// refusal shape (malformed/conflicting/stale; accepted, duplicate and
// transient are rendered before ever reaching a refusal line), so
// reviewRefusalToken maps by kind directly — TestReviewRefusalTokenIsExhaustive
// (cmd/hop) pins that no other kind reaches its default branch.

// refusalToken renders a store-set Reason directly. An empty reason on a
// refused/malformed outcome is a defect the store side must never produce
// (TestPlanRefusalReasonsAlwaysSet and TestMessageRefusalReasonsAlwaysSet,
// internal/app); this renders the generic fallback token at runtime
// rather than guessing which refusal occurred.
func refusalToken(reason string) string {
	if reason == "" {
		return app.GrammarReasonUnauthorized
	}
	return reason
}

// reviewRefusalToken classifies a SubmitReviewVerdict ReviewOutcomeKind:
// the only kinds SubmitReviewResult can carry here are malformed,
// conflicting and stale (accepted/duplicate/transient are rendered by the
// caller before this is ever called).
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
