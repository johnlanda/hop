package main

import (
	"slices"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

// TestRefusalTokenFallsBackOnEmptyReason pins refusalToken's runtime
// fallback for the defect case a store must never produce (an empty
// Reason on a refused/malformed outcome): grammarrender.go's own
// TestPlanRefusalReasonsAlwaysSet and TestMessageRefusalReasonsAlwaysSet
// (internal/app) assert the store side never actually does this: this
// pins only cmd/hop's own defensive rendering.
func TestRefusalTokenFallsBackOnEmptyReason(t *testing.T) {
	if got := refusalToken(""); got != app.GrammarReasonUnauthorized {
		t.Fatalf("refusalToken(\"\") = %q, want %q", got, app.GrammarReasonUnauthorized)
	}
	if got := refusalToken(app.GrammarReasonStale); got != app.GrammarReasonStale {
		t.Fatalf("refusalToken(%q) = %q, want it returned verbatim", app.GrammarReasonStale, got)
	}
}

// TestReviewRefusalTokenIsExhaustive pins reviewRefusalToken against every
// refused review outcome kind and every reason token: a pair renders its
// reason exactly when the kind admits it — stale admits stale, not-reviewer
// and subject-mismatch; malformed and conflicting only their own token —
// and any other pair, an empty reason or an unknown kind renders nothing,
// never a guessed token.
func TestReviewRefusalTokenIsExhaustive(t *testing.T) {
	admitted := map[app.ReviewOutcomeKind][]string{
		app.ReviewMalformed:   {app.GrammarReasonMalformed},
		app.ReviewConflicting: {app.GrammarReasonConflicting},
		app.ReviewStale:       {app.GrammarReasonStale, app.GrammarReasonNotReviewer, app.GrammarReasonSubjectMismatch},
	}
	reasons := []string{
		"", app.GrammarReasonNotFound, app.GrammarReasonUnauthorized, app.GrammarReasonMalformed,
		app.GrammarReasonConflicting, app.GrammarReasonStale, app.GrammarReasonNotDelivered,
		app.GrammarReasonRunNotAccepting, app.GrammarReasonMailboxClosed, app.GrammarReasonNotManager,
		app.GrammarReasonNotReviewer, app.GrammarReasonDependencyCycle, app.GrammarReasonEmptyPlan,
		app.GrammarReasonRetryNotTerminal, app.GrammarReasonRetryLimit, app.GrammarReasonSubjectMismatch,
	}
	kinds := []app.ReviewOutcomeKind{
		app.ReviewMalformed, app.ReviewConflicting, app.ReviewStale,
		app.ReviewAccepted, app.ReviewDuplicate, app.ReviewTransient, "some-future-kind",
	}
	for _, kind := range kinds {
		for _, reason := range reasons {
			want := slices.Contains(admitted[kind], reason)
			token, ok := reviewRefusalToken(string(kind), reason)
			if ok != want || (ok && token != reason) || (!ok && token != "") {
				t.Errorf("reviewRefusalToken(%q, %q) = %q, %t; want the reason rendered: %t", kind, reason, token, ok, want)
			}
		}
	}
}
