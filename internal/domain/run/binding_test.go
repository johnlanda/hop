package run_test

import (
	"errors"
	"testing"

	"github.com/johnlanda/hop/internal/domain/run"
)

func newBinding() run.RuntimeBinding {
	return run.NewRuntimeBinding(testSessionID, testIncarnation, "/tmp/herdr.sock", "herdr-server-lifetime/v1 pid=41001 start=1789000000.000001", "workspace-1", "tab-1", "pane-1", "label-1", run.LaunchInitial, epoch())
}

func TestNewRuntimeBinding(t *testing.T) {
	b := newBinding()

	if b.Superseded {
		t.Fatal("NewRuntimeBinding Superseded = true, want false")
	}
	if b.Occupant != nil {
		t.Fatalf("NewRuntimeBinding Occupant = %+v, want nil", b.Occupant)
	}
	if !b.ObservedAt.Equal(epoch()) {
		t.Fatalf("NewRuntimeBinding ObservedAt = %v, want %v", b.ObservedAt, epoch())
	}
	if b.SessionID != testSessionID || b.IncarnationID != testIncarnation || b.LaunchKind != run.LaunchInitial {
		t.Fatalf("NewRuntimeBinding did not preserve its inputs: %+v", b)
	}
}

// TestRuntimeBindingObserve proves that a current binding accepts a
// complete occupant evidence triple, that a superseded binding never
// accepts one ("observations, closes and retirements are valid only
// against a current (non-superseded) binding", section 2), and that
// incomplete evidence is rejected: occupant identity is "label + argv
// marker + pid" together, and "a pid is never evidence alone" (section 2).
func TestRuntimeBindingObserve(t *testing.T) {
	t.Run("current binding accepts a complete evidence triple", func(t *testing.T) {
		b := newBinding()
		evidence := run.OccupantEvidence{Label: "label-1", ArgvMarker: testAttemptID.String(), PID: 4242}

		got, err := b.Observe(evidence, later())
		if err != nil {
			t.Fatalf("Observe: unexpected error: %v", err)
		}
		if got.Occupant == nil || *got.Occupant != evidence {
			t.Fatalf("Observe Occupant = %+v, want %+v", got.Occupant, evidence)
		}
		if !got.ObservedAt.Equal(later()) {
			t.Fatalf("Observe ObservedAt = %v, want %v", got.ObservedAt, later())
		}
	})

	t.Run("superseded binding refuses an observation", func(t *testing.T) {
		b, err := newBinding().Supersede("closed against evidence X", later())
		if err != nil {
			t.Fatalf("Supersede: unexpected error: %v", err)
		}
		complete := run.OccupantEvidence{Label: "label-1", ArgvMarker: testAttemptID.String(), PID: 4242}

		_, err = b.Observe(complete, later())
		if !errors.Is(err, run.ErrInvalidTransition) {
			t.Fatalf("Observe on a superseded binding: error = %v, want ErrInvalidTransition", err)
		}
	})

	t.Run("rejects incomplete evidence", func(t *testing.T) {
		cases := []struct {
			name     string
			evidence run.OccupantEvidence
		}{
			{name: "empty label", evidence: run.OccupantEvidence{Label: "", ArgvMarker: "marker", PID: 1}},
			{name: "empty argv marker", evidence: run.OccupantEvidence{Label: "label-1", ArgvMarker: "", PID: 1}},
			{name: "zero pid", evidence: run.OccupantEvidence{Label: "label-1", ArgvMarker: "marker", PID: 0}},
			{name: "negative pid", evidence: run.OccupantEvidence{Label: "label-1", ArgvMarker: "marker", PID: -1}},
			{name: "empty everything", evidence: run.OccupantEvidence{}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				b := newBinding()

				got, err := b.Observe(tc.evidence, later())

				if !errors.Is(err, run.ErrInvalidTransition) {
					t.Fatalf("Observe(%+v): error = %v, want ErrInvalidTransition", tc.evidence, err)
				}
				if got != b {
					t.Fatalf("Observe(%+v) changed the binding despite the error: %+v", tc.evidence, got)
				}
			})
		}
	})
}

// TestRuntimeBindingSupersede proves supersession's invariants: it succeeds
// exactly once per binding, and it always requires recorded evidence —
// supersession is never assumed.
func TestRuntimeBindingSupersede(t *testing.T) {
	t.Run("first supersession succeeds", func(t *testing.T) {
		b := newBinding()

		got, err := b.Supersede("observed occupant mismatch", later())
		if err != nil {
			t.Fatalf("Supersede: unexpected error: %v", err)
		}
		if !got.Superseded {
			t.Fatal("Supersede did not set Superseded")
		}
		if got.SupersededEvidence != "observed occupant mismatch" {
			t.Fatalf("Supersede SupersededEvidence = %q, want %q", got.SupersededEvidence, "observed occupant mismatch")
		}
		if !got.SupersededAt.Equal(later()) {
			t.Fatalf("Supersede SupersededAt = %v, want %v", got.SupersededAt, later())
		}
	})

	t.Run("second supersession is rejected", func(t *testing.T) {
		b, err := newBinding().Supersede("first evidence", later())
		if err != nil {
			t.Fatalf("first Supersede: unexpected error: %v", err)
		}

		_, err = b.Supersede("second evidence", later())
		if !errors.Is(err, run.ErrInvalidTransition) {
			t.Fatalf("second Supersede: error = %v, want ErrInvalidTransition", err)
		}
	})

	t.Run("empty evidence is rejected", func(t *testing.T) {
		b := newBinding()

		got, err := b.Supersede("", later())

		if !errors.Is(err, run.ErrInvalidTransition) {
			t.Fatalf("Supersede(\"\"): error = %v, want ErrInvalidTransition", err)
		}
		if got.Superseded {
			t.Fatal("Supersede(\"\") set Superseded despite the error")
		}
	})
}
