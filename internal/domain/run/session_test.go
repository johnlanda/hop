package run_test

import (
	"errors"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/domain/run"
)

// sessionStates enumerates every Session state. It is a function, not a
// package-level slice, only to keep this shared fixture out of the
// global-variable set the project's lint configuration restricts.
func sessionStates() []run.SessionState {
	return []run.SessionState{
		run.SessionReserved, run.SessionLaunching, run.SessionActive,
		run.SessionReconciling, run.SessionLost, run.SessionStopping, run.SessionTerminated,
	}
}

// sessionValidTransitions is section 5's Session table, transcribed
// independently of the production transitionTable.
func sessionValidTransitions() map[[2]run.SessionState]bool {
	return map[[2]run.SessionState]bool{
		{run.SessionReserved, run.SessionLaunching}:     true,
		{run.SessionLaunching, run.SessionActive}:       true,
		{run.SessionReconciling, run.SessionActive}:     true,
		{run.SessionLaunching, run.SessionReconciling}:  true,
		{run.SessionActive, run.SessionReconciling}:     true,
		{run.SessionReconciling, run.SessionLost}:       true,
		{run.SessionLaunching, run.SessionStopping}:     true,
		{run.SessionActive, run.SessionStopping}:        true,
		{run.SessionReconciling, run.SessionStopping}:   true,
		{run.SessionReserved, run.SessionTerminated}:    true,
		{run.SessionLaunching, run.SessionTerminated}:   true,
		{run.SessionStopping, run.SessionTerminated}:    true,
		{run.SessionReconciling, run.SessionTerminated}: true,
	}
}

func TestSessionTransitions(t *testing.T) {
	steps := []struct {
		name string
		to   run.SessionState
		fn   func(run.Session, time.Time) (run.Session, error)
	}{
		{"Launch", run.SessionLaunching, run.Session.Launch},
		{"ConfirmActive", run.SessionActive, run.Session.ConfirmActive},
		{"Reconcile", run.SessionReconciling, run.Session.Reconcile},
		{"MarkLost", run.SessionLost, run.Session.MarkLost},
		{"Stop", run.SessionStopping, run.Session.Stop},
		{"Terminate", run.SessionTerminated, run.Session.Terminate},
	}

	valid := sessionValidTransitions()
	for _, step := range steps {
		for _, from := range sessionStates() {
			t.Run(step.name+"/"+string(from)+"_to_"+string(step.to), func(t *testing.T) {
				session := run.Session{ID: testSessionID, RunID: testRunID, AttemptID: testAttemptID, State: from}

				got, err := step.fn(session, epoch())

				if valid[[2]run.SessionState{from, step.to}] {
					if err != nil {
						t.Fatalf("%s from %s: unexpected error: %v", step.name, from, err)
					}
					if got.State != step.to {
						t.Fatalf("%s from %s: State = %s, want %s", step.name, from, got.State, step.to)
					}
					if !got.UpdatedAt.Equal(epoch()) {
						t.Fatalf("%s from %s: UpdatedAt = %v, want %v", step.name, from, got.UpdatedAt, epoch())
					}
					return
				}
				if err == nil {
					t.Fatalf("%s from %s: got %+v, want ErrInvalidTransition", step.name, from, got)
				}
				if !errors.Is(err, run.ErrInvalidTransition) {
					t.Fatalf("%s from %s: error = %v, want wrapping ErrInvalidTransition", step.name, from, err)
				}
				if got.State != from {
					t.Fatalf("%s from %s: State = %s, want unchanged", step.name, from, got.State)
				}
			})
		}
	}
}

func TestNewSession(t *testing.T) {
	session := run.NewSession(testSessionID, testRunID, testAttemptID, run.HarnessClaude, epoch())

	if session.State != run.SessionReserved {
		t.Fatalf("NewSession State = %s, want reserved", session.State)
	}
	if session.Role != run.RoleWorker {
		t.Fatalf("NewSession Role = %s, want worker", session.Role)
	}
	if session.ID != testSessionID || session.RunID != testRunID || session.AttemptID != testAttemptID || session.Harness != run.HarnessClaude {
		t.Fatalf("NewSession did not preserve its inputs: %+v", session)
	}
	if !session.UpdatedAt.Equal(epoch()) {
		t.Fatalf("NewSession UpdatedAt = %v, want %v", session.UpdatedAt, epoch())
	}
}

// TestSessionAssignNativeRefIsImmutable proves that a session's native
// reference, once assigned, refuses a second assignment — even an
// identical one.
func TestSessionAssignNativeRefIsImmutable(t *testing.T) {
	session := run.NewSession(testSessionID, testRunID, testAttemptID, run.HarnessClaude, epoch())

	assigned, err := session.AssignNativeRef("native-ref-1", run.NativeRefAssigned, epoch())
	if err != nil {
		t.Fatalf("first AssignNativeRef: unexpected error: %v", err)
	}
	if assigned.NativeSessionRef != "native-ref-1" || assigned.NativeRefSource != run.NativeRefAssigned {
		t.Fatalf("first AssignNativeRef did not record the reference: %+v", assigned)
	}

	t.Run("same reference", func(t *testing.T) {
		_, err := assigned.AssignNativeRef("native-ref-1", run.NativeRefAssigned, later())
		if !errors.Is(err, run.ErrInvalidTransition) {
			t.Fatalf("second AssignNativeRef (same value): error = %v, want ErrInvalidTransition", err)
		}
	})

	t.Run("different reference", func(t *testing.T) {
		_, err := assigned.AssignNativeRef("native-ref-2", run.NativeRefCaptured, later())
		if !errors.Is(err, run.ErrInvalidTransition) {
			t.Fatalf("second AssignNativeRef (different value): error = %v, want ErrInvalidTransition", err)
		}
	})

	if assigned.NativeSessionRef != "native-ref-1" {
		t.Fatalf("AssignNativeRef mutated the reference: %+v", assigned)
	}
}

// TestSessionAssignNativeRefRejectsInvalidInput proves that an empty
// reference and a source other than NativeRefAssigned/NativeRefCaptured are
// both rejected, without recording anything and without opening a route to
// a later valid-looking assignment on top of an invalid one.
func TestSessionAssignNativeRefRejectsInvalidInput(t *testing.T) {
	session := run.NewSession(testSessionID, testRunID, testAttemptID, run.HarnessClaude, epoch())

	cases := []struct {
		name   string
		ref    string
		source run.NativeRefSource
	}{
		{name: "empty reference, valid source", ref: "", source: run.NativeRefAssigned},
		{name: "empty reference, unknown source", ref: "", source: run.NativeRefSource("")},
		{name: "non-empty reference, unknown source", ref: "native-ref-1", source: run.NativeRefSource("bogus")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := session.AssignNativeRef(tc.ref, tc.source, later())

			if !errors.Is(err, run.ErrInvalidTransition) {
				t.Fatalf("AssignNativeRef(%q, %q): error = %v, want ErrInvalidTransition", tc.ref, tc.source, err)
			}
			if got != session {
				t.Fatalf("AssignNativeRef(%q, %q) changed the session despite the error: %+v", tc.ref, tc.source, got)
			}
		})
	}

	t.Run("rejected empty reference never opens the door to a later assignment", func(t *testing.T) {
		afterRejection, err := session.AssignNativeRef("", run.NativeRefAssigned, later())
		if !errors.Is(err, run.ErrInvalidTransition) {
			t.Fatalf("AssignNativeRef(\"\"): error = %v, want ErrInvalidTransition", err)
		}

		got, err := afterRejection.AssignNativeRef("native-ref-1", run.NativeRefAssigned, later())
		if err != nil {
			t.Fatalf("AssignNativeRef after a rejected empty reference: unexpected error: %v", err)
		}
		if got.NativeSessionRef != "native-ref-1" {
			t.Fatalf("AssignNativeRef after a rejected empty reference = %+v, want native-ref-1 recorded", got)
		}
	})
}
