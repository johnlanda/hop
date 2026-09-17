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
// ReviewStore's outcomes carry the same kind of store-set Reason, which
// SubmitReviewVerdict checks against the outcome kind; reviewRefusalToken
// renders only such a reason and never guesses one —
// TestReviewRefusalTokenIsExhaustive (cmd/hop) pins every admitted pair.

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

// reviewRefusalToken returns a refused SubmitReviewVerdict outcome's
// grammar reason token: the store-set Reason, when the outcome kind admits
// it (app.ReviewReasonAdmitted — stale admits stale, not-reviewer and
// subject-mismatch; malformed and conflicting admit only their own token).
// ok is false for any other pair, and the caller then prints no protocol
// line.
func reviewRefusalToken(outcome, reason string) (token string, ok bool) {
	if reason == "" || !app.ReviewReasonAdmitted(app.ReviewOutcomeKind(outcome), reason) {
		return "", false
	}
	return reason, true
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
