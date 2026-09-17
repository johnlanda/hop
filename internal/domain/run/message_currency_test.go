package run_test

import (
	"errors"
	"testing"

	"github.com/johnlanda/hop/internal/domain/run"
)

// containsText reports whether sub occurs in s (this package's tests may
// import only errors, fmt, time and testing).
func containsText(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestAcceptAckAttemptCurrency pins where the address-currency fact sits in
// AcceptAck's order: a prior ack is an idempotent duplicate whatever the
// caller's currency; a session never served the message is not-delivered
// before its currency is read; the incarnation is checked before the
// attempt; and a delivered, current-incarnation ack from a session that is
// no longer its address's current session is stale, leaving the message
// delivered (in flight, re-servable). AttemptCurrent's zero value refuses.
func TestAcceptAckAttemptCurrency(t *testing.T) {
	delivered := run.Message{ID: testMessageID, RunID: testRunID, Recipient: run.TaskAddress(testTaskID), State: run.MessageDelivered}
	ack := run.Ack{SessionID: testSessionID, IncarnationID: testIncarnation}

	t.Run("a prior ack is a duplicate for a retired session", func(t *testing.T) {
		prior := run.Ack{MessageID: testMessageID, SessionID: testSecondSessionID, IncarnationID: testSecondIncarnation, At: epoch()}
		acked := delivered
		acked.State = run.MessageAcknowledged
		outcome, err := run.AcceptAck(acked, &prior, run.AckContext{DeliveredToSession: true, IncarnationCurrent: true, AttemptCurrent: false}, ack, later())
		if err != nil || outcome.Ack != prior {
			t.Fatalf("AcceptAck(duplicate, retired session) = %+v, %v; want the prior ack and no error", outcome, err)
		}
	})

	t.Run("a retired session never served the message is not-delivered", func(t *testing.T) {
		_, err := run.AcceptAck(delivered, nil, run.AckContext{DeliveredToSession: false, IncarnationCurrent: true, AttemptCurrent: false}, ack, later())
		if !errors.Is(err, run.ErrNotDelivered) {
			t.Fatalf("AcceptAck = %v, want ErrNotDelivered", err)
		}
	})

	t.Run("the incarnation is checked before the attempt", func(t *testing.T) {
		_, err := run.AcceptAck(delivered, nil, run.AckContext{DeliveredToSession: true, IncarnationCurrent: false, AttemptCurrent: false}, ack, later())
		if !errors.Is(err, run.ErrStaleAck) || !containsText(err.Error(), "incarnation") {
			t.Fatalf("AcceptAck = %v, want the incarnation's ErrStaleAck", err)
		}
	})

	for name, ctx := range map[string]run.AckContext{
		"a session that is no longer its address's current session": {DeliveredToSession: true, IncarnationCurrent: true, AttemptCurrent: false},
		"AttemptCurrent left at its zero value":                     {DeliveredToSession: true, IncarnationCurrent: true},
	} {
		t.Run(name, func(t *testing.T) {
			outcome, err := run.AcceptAck(delivered, nil, ctx, ack, later())
			if !errors.Is(err, run.ErrStaleAck) || !containsText(err.Error(), "current session") {
				t.Fatalf("AcceptAck = %v, want the address-currency ErrStaleAck", err)
			}
			if containsText(err.Error(), testSessionID.String()) || containsText(err.Error(), testIncarnation.String()) {
				t.Fatalf("AcceptAck error = %q echoes the caller's session or incarnation", err)
			}
			if outcome.Message != delivered || outcome.Ack != (run.Ack{}) {
				t.Fatalf("AcceptAck changed the message on a refusal: %+v", outcome)
			}
		})
	}
}

// TestCurrentAddressSession pins section 7's consumer rule, against an
// independent transcription of the terminal session and attempt states: a
// manager session that has not ended is current with no attempt at all —
// its attempt arguments are never consulted — and an implementer or
// reviewer session is current only while it has not ended and its own
// attempt is its task's newest, non-terminal attempt.
func TestCurrentAddressSession(t *testing.T) {
	endedSession := map[run.SessionState]bool{run.SessionLost: true, run.SessionTerminated: true}
	terminalAttempt := map[run.AttemptState]bool{run.AttemptCompleted: true, run.AttemptFailed: true, run.AttemptInterrupted: true}

	manager := func(state run.SessionState) run.Session {
		return run.Session{ID: testManagerSessionID, RunID: testRunID, Role: run.RoleManager, State: state}
	}
	child := func(role run.Role, state run.SessionState) run.Session {
		return run.Session{ID: testSessionID, RunID: testRunID, AttemptID: testAttemptID, Role: role, State: state}
	}
	attempt := func(id string, number int, state run.AttemptState) run.Attempt {
		a := baseAttempt(state)
		if id == "second" {
			a.ID = testSecondAttemptID
		}
		a.Number = number
		return a
	}

	t.Run("the manager needs no attempt", func(t *testing.T) {
		for _, state := range sessionStates() {
			want := !endedSession[state]
			if got := run.CurrentAddressSession(manager(state), run.Attempt{}, run.Attempt{}); got != want {
				t.Errorf("manager %s with no attempt = %t, want %t", state, got, want)
			}
			// Whatever a caller passes, a manager's currency never depends on
			// an attempt: the rule reads none.
			terminal := attempt("first", 1, run.AttemptInterrupted)
			newer := attempt("second", 2, run.AttemptRunning)
			if got := run.CurrentAddressSession(manager(state), terminal, newer); got != want {
				t.Errorf("manager %s with unrelated attempts = %t, want %t", state, got, want)
			}
		}
	})

	t.Run("a child on its task's newest attempt", func(t *testing.T) {
		for _, role := range []run.Role{run.RoleImplementer, run.RoleReviewer} {
			for _, sessionState := range sessionStates() {
				for _, attemptState := range attemptStates() {
					own := attempt("first", 1, attemptState)
					want := !endedSession[sessionState] && !terminalAttempt[attemptState]
					if got := run.CurrentAddressSession(child(role, sessionState), own, own); got != want {
						t.Errorf("%s %s on its newest %s attempt = %t, want %t", role, sessionState, attemptState, got, want)
					}
				}
			}
		}
	})

	t.Run("never current", func(t *testing.T) {
		own := attempt("first", 1, run.AttemptRunning)
		retired := attempt("first", 1, run.AttemptInterrupted)
		successor := attempt("second", 2, run.AttemptRunning)
		otherTask := own
		otherTask.TaskID = testSecondTaskID
		foreign := attempt("second", 1, run.AttemptRunning)
		cases := []struct {
			name            string
			session         run.Session
			attempt, newest run.Attempt
		}{
			{"a child whose attempt a retry succeeded", child(run.RoleImplementer, run.SessionActive), retired, successor},
			{"a child on a live attempt that is not the newest", child(run.RoleImplementer, run.SessionActive), own, successor},
			{"a reviewer whose attempt a retry succeeded", child(run.RoleReviewer, run.SessionActive), retired, successor},
			{"a child whose attempt rows were not read", child(run.RoleImplementer, run.SessionActive), run.Attempt{}, run.Attempt{}},
			{"a child given another session's attempt", child(run.RoleImplementer, run.SessionActive), foreign, foreign},
			{"a child whose newest attempt belongs to another task", child(run.RoleImplementer, run.SessionActive), own, otherTask},
			{"a child with no attempt", run.Session{ID: testSessionID, RunID: testRunID, Role: run.RoleImplementer, State: run.SessionActive}, run.Attempt{}, run.Attempt{}},
			{"a Phase 2 solo worker", child(run.RoleWorker, run.SessionActive), own, own},
			{"a session with no role", child("", run.SessionActive), own, own},
		}
		for _, tc := range cases {
			if run.CurrentAddressSession(tc.session, tc.attempt, tc.newest) {
				t.Errorf("%s: CurrentAddressSession = true, want false", tc.name)
			}
		}
	})
}
