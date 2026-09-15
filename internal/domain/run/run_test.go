package run_test

import (
	"errors"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/domain/run"
)

// runStates enumerates every Run state so the exhaustiveness tests below can
// walk every (from, to) pair. It is a function, not a package-level slice,
// only to keep this shared fixture out of the global-variable set the
// project's lint configuration restricts.
func runStates() []run.RunState {
	return []run.RunState{
		run.RunCreated, run.RunLaunching, run.RunRunning, run.RunCompleting,
		run.RunCompleted, run.RunStopping, run.RunStopped, run.RunResuming, run.RunFailed,
	}
}

// runValidTransitions is section 5's Run table ("### Run" in
// docs/plan/phase-2-design.md), transcribed independently of the production
// transitionTable so this test proves the implementation against the design
// document rather than against itself.
func runValidTransitions() map[[2]run.RunState]bool {
	return map[[2]run.RunState]bool{
		{run.RunCreated, run.RunLaunching}:    true,
		{run.RunResuming, run.RunLaunching}:   true,
		{run.RunLaunching, run.RunRunning}:    true,
		{run.RunResuming, run.RunRunning}:     true,
		{run.RunCompleting, run.RunRunning}:   true,
		{run.RunResuming, run.RunCompleting}:  true,
		{run.RunRunning, run.RunCompleting}:   true,
		{run.RunCompleting, run.RunCompleted}: true,
		{run.RunCreated, run.RunStopping}:     true,
		{run.RunLaunching, run.RunStopping}:   true,
		{run.RunRunning, run.RunStopping}:     true,
		{run.RunCompleting, run.RunStopping}:  true,
		{run.RunResuming, run.RunStopping}:    true,
		{run.RunStopping, run.RunStopped}:     true,
		{run.RunLaunching, run.RunFailed}:     true,
		{run.RunRunning, run.RunFailed}:       true,
		{run.RunCompleting, run.RunFailed}:    true,
		{run.RunResuming, run.RunFailed}:      true,
		{run.RunCreated, run.RunResuming}:     true,
		{run.RunLaunching, run.RunResuming}:   true,
		{run.RunRunning, run.RunResuming}:     true,
		{run.RunCompleting, run.RunResuming}:  true,
		{run.RunStopping, run.RunResuming}:    true,
	}
}

// TestRunTransitions exhaustively checks every (from, to) pair for every
// named Run transition function against runValidTransitions: a listed pair
// succeeds and lands on to with UpdatedAt set to now; an unlisted pair
// returns ErrInvalidTransition and leaves the run unchanged.
func TestRunTransitions(t *testing.T) {
	steps := []struct {
		name string
		to   run.RunState
		fn   func(run.Run, time.Time) (run.Run, error)
	}{
		{"Launch", run.RunLaunching, run.Run.Launch},
		{"MarkRunning", run.RunRunning, run.Run.MarkRunning},
		{"EnterCompleting", run.RunCompleting, run.Run.EnterCompleting},
		{"Complete", run.RunCompleted, run.Run.Complete},
		{"Fail", run.RunFailed, run.Run.Fail},
		{"MarkStopped", run.RunStopped, run.Run.MarkStopped},
		{"EnterResuming", run.RunResuming, run.Run.EnterResuming},
	}

	valid := runValidTransitions()
	for _, step := range steps {
		for _, from := range runStates() {
			t.Run(step.name+"/"+string(from)+"_to_"+string(step.to), func(t *testing.T) {
				r := run.Run{ID: testRunID, State: from}

				got, err := step.fn(r, epoch())

				if valid[[2]run.RunState{from, step.to}] {
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

// TestRunCompleteRefusesWhenStopRequested proves the stop-precedence
// invariant: a run with a stop request never completes, even from a state
// the bare table lists as valid, because completion is a settling
// transition and stop takes precedence over every one of those.
func TestRunCompleteRefusesWhenStopRequested(t *testing.T) {
	r := run.Run{ID: testRunID, State: run.RunCompleting, StopRequested: true}

	got, err := r.Complete(epoch())

	if !errors.Is(err, run.ErrInvalidTransition) {
		t.Fatalf("Complete with a stop request: error = %v, want ErrInvalidTransition", err)
	}
	if got.State != run.RunCompleting {
		t.Fatalf("Complete with a stop request: State = %s, want unchanged", got.State)
	}
}

// TestRunRequestStop proves the effect of a stop request on Run.State from
// every state: it moves the run to stopping from every state section 5
// lists as a valid stopping source, and otherwise leaves the state
// unchanged.
func TestRunRequestStop(t *testing.T) {
	cases := []struct {
		from      run.RunState
		wantState run.RunState
	}{
		{run.RunCreated, run.RunStopping},
		{run.RunLaunching, run.RunStopping},
		{run.RunRunning, run.RunStopping},
		{run.RunCompleting, run.RunStopping},
		{run.RunResuming, run.RunStopping},
		{run.RunStopping, run.RunStopping},
		{run.RunStopped, run.RunStopped},
		{run.RunCompleted, run.RunCompleted},
		{run.RunFailed, run.RunFailed},
	}
	for _, tc := range cases {
		t.Run(string(tc.from), func(t *testing.T) {
			r := run.Run{ID: testRunID, State: tc.from}

			got := r.RequestStop(epoch())

			if !got.StopRequested {
				t.Fatalf("RequestStop from %s: StopRequested = false, want true", tc.from)
			}
			if got.State != tc.wantState {
				t.Fatalf("RequestStop from %s: State = %s, want %s", tc.from, got.State, tc.wantState)
			}
		})
	}
}

// TestRunRequestStopIsMonotonic proves the request cannot be withdrawn: a
// second, later request against an already-stopping run stays stopping and
// stays requested, and the flag never reverts to false.
func TestRunRequestStopIsMonotonic(t *testing.T) {
	r := run.Run{ID: testRunID, State: run.RunRunning}

	first := r.RequestStop(epoch())
	second := first.RequestStop(later())

	if !first.StopRequested || !second.StopRequested {
		t.Fatal("StopRequested reverted to false across repeated requests")
	}
	if second.State != run.RunStopping {
		t.Fatalf("second RequestStop: State = %s, want stopping", second.State)
	}
}

// TestRunClosePlan proves ClosePlan's two guards: the run must be running
// (ErrRunNotAccepting), and the plan must have at least one implement task
// (ErrEmptyPlan).
func TestRunClosePlan(t *testing.T) {
	t.Run("running with an implement task", func(t *testing.T) {
		r := run.Run{ID: testRunID, State: run.RunRunning}

		got, err := r.ClosePlan(true, epoch())
		if err != nil {
			t.Fatalf("ClosePlan: unexpected error: %v", err)
		}
		if !got.PlanClosed {
			t.Fatal("ClosePlan: PlanClosed = false, want true")
		}
	})

	t.Run("empty plan refused", func(t *testing.T) {
		r := run.Run{ID: testRunID, State: run.RunRunning}

		got, err := r.ClosePlan(false, epoch())
		if !errors.Is(err, run.ErrEmptyPlan) {
			t.Fatalf("ClosePlan(no implement task): error = %v, want ErrEmptyPlan", err)
		}
		if got.PlanClosed {
			t.Fatal("ClosePlan(no implement task): PlanClosed = true, want unchanged")
		}
	})

	t.Run("not running refused", func(t *testing.T) {
		for _, from := range runStates() {
			if from == run.RunRunning {
				continue
			}
			r := run.Run{ID: testRunID, State: from}

			_, err := r.ClosePlan(true, epoch())
			if !errors.Is(err, run.ErrRunNotAccepting) {
				t.Fatalf("ClosePlan from %s: error = %v, want ErrRunNotAccepting", from, err)
			}
		}
	})
}

// TestRunReopenPlan proves ReopenPlan clears the flag unconditionally.
func TestRunReopenPlan(t *testing.T) {
	r := run.Run{ID: testRunID, State: run.RunRunning, PlanClosed: true}

	got := r.ReopenPlan(later())

	if got.PlanClosed {
		t.Fatal("ReopenPlan: PlanClosed = true, want false")
	}
	if !got.UpdatedAt.Equal(later()) {
		t.Fatalf("ReopenPlan: UpdatedAt = %v, want %v", got.UpdatedAt, later())
	}
}

// TestRunCanAcceptManagerVerb proves the shared eligibility check every
// manager verb's accepting transaction re-validates: only running.
func TestRunCanAcceptManagerVerb(t *testing.T) {
	for _, from := range runStates() {
		r := run.Run{ID: testRunID, State: from}
		err := r.CanAcceptManagerVerb()
		if from == run.RunRunning {
			if err != nil {
				t.Fatalf("CanAcceptManagerVerb from %s: unexpected error: %v", from, err)
			}
			continue
		}
		if !errors.Is(err, run.ErrRunNotAccepting) {
			t.Fatalf("CanAcceptManagerVerb from %s: error = %v, want ErrRunNotAccepting", from, err)
		}
	}
}

func TestNewRun(t *testing.T) {
	r := run.NewRun(testRunID, testRepositoryID, 1, "brief-digest", epoch())

	if r.State != run.RunCreated {
		t.Fatalf("NewRun State = %s, want created", r.State)
	}
	if r.StopRequested {
		t.Fatal("NewRun StopRequested = true, want false")
	}
	if r.ID != testRunID || r.RepositoryID != testRepositoryID || r.Sequence != 1 || r.BriefDigest != "brief-digest" {
		t.Fatalf("NewRun did not preserve its inputs: %+v", r)
	}
	if !r.UpdatedAt.Equal(epoch()) {
		t.Fatalf("NewRun UpdatedAt = %v, want %v", r.UpdatedAt, epoch())
	}
}
