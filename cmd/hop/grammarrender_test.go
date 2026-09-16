package main

import (
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

// TestReviewRefusalTokenIsExhaustive pins reviewRefusalToken's claim that
// the only ReviewOutcomeKind values it is ever called with are malformed,
// conflicting and stale — accepted, duplicate and transient are rendered
// by writeReviewResultAndExit before reviewRefusalToken is ever reached
// (reviewcmd.go). If a future ReviewOutcomeKind is added without updating
// this table, this test documents exactly which kinds are covered today so
// the gap is visible in a diff rather than silently falling back to
// "unauthorized".
func TestReviewRefusalTokenIsExhaustive(t *testing.T) {
	cases := map[app.ReviewOutcomeKind]string{
		app.ReviewMalformed:   app.GrammarReasonMalformed,
		app.ReviewConflicting: app.GrammarReasonConflicting,
		app.ReviewStale:       app.GrammarReasonStale,
	}
	for kind, want := range cases {
		if got := reviewRefusalToken(string(kind)); got != want {
			t.Errorf("reviewRefusalToken(%q) = %q, want %q", kind, got, want)
		}
	}
	// Every ReviewOutcomeKind this function is ever actually called with
	// (accepted/duplicate/transient are intercepted by the caller first)
	// is covered above; a kind outside that set is a defect elsewhere, not
	// something this function can classify correctly, so the fallback
	// stays the documented generic token.
	if got := reviewRefusalToken("some-future-kind"); got != app.GrammarReasonUnauthorized {
		t.Fatalf("reviewRefusalToken(unrecognized) = %q, want the generic fallback %q", got, app.GrammarReasonUnauthorized)
	}
}
