package run_test

import (
	"errors"
	"testing"

	"github.com/johnlanda/hop/internal/domain/run"
)

func baseRun(state run.RunState, stopRequested bool) run.Run {
	return run.Run{ID: testRunID, RepositoryID: testRepositoryID, Sequence: 1, State: state, StopRequested: stopRequested}
}

func baseTask(state run.TaskState) run.Task {
	return run.Task{ID: testTaskID, RunID: testRunID, State: state}
}

func baseAttempt(state run.AttemptState) run.Attempt {
	return run.Attempt{ID: testAttemptID, TaskID: testTaskID, Number: 1, State: state}
}

// acceptedSubmission is a function, not a package-level value, only to keep
// this shared fixture out of the global-variable set the project's lint
// configuration restricts.
func acceptedSubmission() run.ResultSubmission {
	return run.ResultSubmission{ID: testResultID, CommitOID: "deadbeef", Summary: "did the thing", Digest: "digest-v1"}
}

// TestAcceptResultOrdinaryAcceptance proves the atomic handoff for a normal
// submission (attempt already running): the attempt moves to submitted, the
// task to checking, and the run — already running — is left as is.
func TestAcceptResultOrdinaryAcceptance(t *testing.T) {
	r := baseRun(run.RunRunning, false)
	task := baseTask(run.TaskActive)
	attempt := baseAttempt(run.AttemptRunning)

	outcome, err := run.AcceptResult(r, task, attempt, nil, run.AcceptanceContext{IncarnationCurrent: true}, acceptedSubmission(), epoch())
	if err != nil {
		t.Fatalf("AcceptResult: unexpected error: %v", err)
	}
	if outcome.Attempt.State != run.AttemptSubmitted {
		t.Fatalf("Attempt.State = %s, want submitted", outcome.Attempt.State)
	}
	if outcome.Task.State != run.TaskChecking {
		t.Fatalf("Task.State = %s, want checking", outcome.Task.State)
	}
	if outcome.Run.State != run.RunRunning {
		t.Fatalf("Run.State = %s, want unchanged running", outcome.Run.State)
	}
	if !outcome.Result.Accepted || outcome.Result.ID != testResultID || outcome.Result.ContentDigest != acceptedSubmission().Digest {
		t.Fatalf("Result = %+v, want an accepted result carrying the submission", outcome.Result)
	}
	if !outcome.Result.SubmittedAt.Equal(epoch()) {
		t.Fatalf("Result.SubmittedAt = %v, want %v", outcome.Result.SubmittedAt, epoch())
	}
}

// TestAcceptResultEarlySubmission proves the early-submission atomic
// handoff (trace 1's alternative and trace 2): a settled claim lets a
// launching or relaunching attempt accept a result before the controller's
// own lifecycle observation, moving Run launching to running in the same
// transaction.
func TestAcceptResultEarlySubmission(t *testing.T) {
	for _, attemptState := range []run.AttemptState{run.AttemptLaunching, run.AttemptRelaunching} {
		t.Run(string(attemptState), func(t *testing.T) {
			r := baseRun(run.RunLaunching, false)
			task := baseTask(run.TaskActive)
			attempt := baseAttempt(attemptState)
			ctx := run.AcceptanceContext{IncarnationCurrent: true, LaunchClaimSettled: true}

			outcome, err := run.AcceptResult(r, task, attempt, nil, ctx, acceptedSubmission(), epoch())
			if err != nil {
				t.Fatalf("AcceptResult: unexpected error: %v", err)
			}
			if outcome.Attempt.State != run.AttemptSubmitted {
				t.Fatalf("Attempt.State = %s, want submitted", outcome.Attempt.State)
			}
			if outcome.Task.State != run.TaskChecking {
				t.Fatalf("Task.State = %s, want checking", outcome.Task.State)
			}
			if outcome.Run.State != run.RunRunning {
				t.Fatalf("Run.State = %s, want running (launching->running applied)", outcome.Run.State)
			}
		})
	}
}

// TestAcceptResultTransient proves the transient case: a launching or
// relaunching attempt whose claim has not settled is not yet eligible, and
// the caller is expected to retry.
func TestAcceptResultTransient(t *testing.T) {
	for _, attemptState := range []run.AttemptState{run.AttemptLaunching, run.AttemptRelaunching} {
		t.Run(string(attemptState), func(t *testing.T) {
			r := baseRun(run.RunLaunching, false)
			task := baseTask(run.TaskActive)
			attempt := baseAttempt(attemptState)
			ctx := run.AcceptanceContext{IncarnationCurrent: true, LaunchClaimSettled: false}

			outcome, err := run.AcceptResult(r, task, attempt, nil, ctx, acceptedSubmission(), epoch())

			if !errors.Is(err, run.ErrTransientNotRunning) {
				t.Fatalf("AcceptResult: error = %v, want ErrTransientNotRunning", err)
			}
			if outcome.Attempt != attempt || outcome.Task != task || outcome.Run != r {
				t.Fatalf("AcceptResult mutated its inputs on a transient outcome: %+v", outcome)
			}
		})
	}
}

// TestAcceptResultStaleByIncarnation proves that a submission for a
// superseded incarnation is stale even though the attempt's own state would
// otherwise be eligible.
func TestAcceptResultStaleByIncarnation(t *testing.T) {
	r := baseRun(run.RunRunning, false)
	task := baseTask(run.TaskActive)
	attempt := baseAttempt(run.AttemptRunning)
	ctx := run.AcceptanceContext{IncarnationCurrent: false}

	_, err := run.AcceptResult(r, task, attempt, nil, ctx, acceptedSubmission(), epoch())

	if !errors.Is(err, run.ErrStaleSubmission) {
		t.Fatalf("AcceptResult: error = %v, want ErrStaleSubmission", err)
	}
}

// TestAcceptResultStaleByStopRequest proves stop precedence: a run with a
// stop request rejects a first acceptance as stale, regardless of the
// attempt's own state.
func TestAcceptResultStaleByStopRequest(t *testing.T) {
	r := baseRun(run.RunStopping, true)
	task := baseTask(run.TaskActive)
	attempt := baseAttempt(run.AttemptRunning)
	ctx := run.AcceptanceContext{IncarnationCurrent: true}

	_, err := run.AcceptResult(r, task, attempt, nil, ctx, acceptedSubmission(), epoch())

	if !errors.Is(err, run.ErrStaleSubmission) {
		t.Fatalf("AcceptResult: error = %v, want ErrStaleSubmission", err)
	}
}

// TestAcceptResultStaleByAttemptState proves that every attempt state other
// than running, or launching/relaunching with a settled claim, is stale.
func TestAcceptResultStaleByAttemptState(t *testing.T) {
	ineligible := []run.AttemptState{
		run.AttemptReserved, run.AttemptSubmitted, run.AttemptChecking,
		run.AttemptCompleted, run.AttemptFailed, run.AttemptInterrupted, run.AttemptReconciling,
	}
	for _, state := range ineligible {
		t.Run(string(state), func(t *testing.T) {
			r := baseRun(run.RunRunning, false)
			task := baseTask(run.TaskActive)
			attempt := baseAttempt(state)
			ctx := run.AcceptanceContext{IncarnationCurrent: true, LaunchClaimSettled: true}

			_, err := run.AcceptResult(r, task, attempt, nil, ctx, acceptedSubmission(), epoch())

			if !errors.Is(err, run.ErrStaleSubmission) {
				t.Fatalf("AcceptResult from %s: error = %v, want ErrStaleSubmission", state, err)
			}
		})
	}
}

// TestAcceptResultPropagatesInconsistentTaskState proves that AcceptResult
// does not silently accept an application-supplied Task inconsistent with
// an eligible Attempt: if Task is not active, its own EnterChecking
// transition fails and AcceptResult surfaces that error rather than
// completing a partial handoff.
func TestAcceptResultPropagatesInconsistentTaskState(t *testing.T) {
	r := baseRun(run.RunRunning, false)
	task := baseTask(run.TaskInterrupted)
	attempt := baseAttempt(run.AttemptRunning)

	outcome, err := run.AcceptResult(r, task, attempt, nil, run.AcceptanceContext{IncarnationCurrent: true}, acceptedSubmission(), epoch())

	if !errors.Is(err, run.ErrInvalidTransition) {
		t.Fatalf("AcceptResult with an inconsistent task: error = %v, want ErrInvalidTransition", err)
	}
	if outcome.Run != r || outcome.Task != task || outcome.Attempt != attempt {
		t.Fatalf("AcceptResult mutated its inputs despite failing: %+v", outcome)
	}
}

// TestAcceptResultDuplicateIsIdempotentInEveryState proves that a
// resubmission with the same digest as the already-accepted result is an
// idempotent ErrDuplicateResult in every attempt state, including every
// terminal one ("an accepted receipt is immutable ... equal digest
// resubmission is idempotent in every state including terminal ones",
// section 2), and leaves Run, Task and Attempt unchanged.
func TestAcceptResultDuplicateIsIdempotentInEveryState(t *testing.T) {
	prior := run.Result{ID: testResultID, AttemptID: testAttemptID, ContentDigest: "digest-v1", Accepted: true, CommitOID: "deadbeef"}

	for _, state := range attemptStates() {
		for _, runState := range []run.RunState{run.RunRunning, run.RunCompleted, run.RunFailed, run.RunStopped} {
			t.Run(string(state)+"/run_"+string(runState), func(t *testing.T) {
				r := baseRun(runState, false)
				task := baseTask(run.TaskChecking)
				attempt := baseAttempt(state)
				resubmission := run.ResultSubmission{ID: identityOtherResultID, CommitOID: "deadbeef", Summary: "resubmit", Digest: "digest-v1"}

				outcome, err := run.AcceptResult(r, task, attempt, &prior, run.AcceptanceContext{IncarnationCurrent: true}, resubmission, later())

				if !errors.Is(err, run.ErrDuplicateResult) {
					t.Fatalf("AcceptResult: error = %v, want ErrDuplicateResult", err)
				}
				if outcome.Result != prior {
					t.Fatalf("Result = %+v, want the unchanged prior %+v", outcome.Result, prior)
				}
				if outcome.Run != r || outcome.Task != task || outcome.Attempt != attempt {
					t.Fatalf("AcceptResult mutated its inputs on a duplicate outcome: %+v", outcome)
				}
			})
		}
	}
}

// TestAcceptResultDuplicateOrderingIsAdversarial proves that duplicate
// resolution genuinely happens before any eligibility precondition, not
// merely alongside states where eligibility would also have passed: an
// identical digest is still ErrDuplicateResult when the incarnation is not
// current (including on a terminal attempt, where eligibility would in any
// case be denied by state) and when the run has an active stop request.
func TestAcceptResultDuplicateOrderingIsAdversarial(t *testing.T) {
	prior := run.Result{ID: testResultID, AttemptID: testAttemptID, ContentDigest: "digest-v1", Accepted: true, CommitOID: "deadbeef"}
	resubmission := run.ResultSubmission{ID: identityOtherResultID, CommitOID: "deadbeef", Summary: "resubmit", Digest: "digest-v1"}

	cases := []struct {
		name    string
		run     run.Run
		task    run.Task
		attempt run.Attempt
		ctx     run.AcceptanceContext
	}{
		{
			name:    "non-current incarnation, running attempt",
			run:     baseRun(run.RunRunning, false),
			task:    baseTask(run.TaskChecking),
			attempt: baseAttempt(run.AttemptRunning),
			ctx:     run.AcceptanceContext{IncarnationCurrent: false},
		},
		{
			name:    "non-current incarnation, terminal completed attempt",
			run:     baseRun(run.RunCompleted, false),
			task:    baseTask(run.TaskCompleted),
			attempt: baseAttempt(run.AttemptCompleted),
			ctx:     run.AcceptanceContext{IncarnationCurrent: false},
		},
		{
			name:    "non-current incarnation, terminal failed attempt",
			run:     baseRun(run.RunFailed, false),
			task:    baseTask(run.TaskFailed),
			attempt: baseAttempt(run.AttemptFailed),
			ctx:     run.AcceptanceContext{IncarnationCurrent: false},
		},
		{
			name:    "run stopping with an active stop request",
			run:     baseRun(run.RunStopping, true),
			task:    baseTask(run.TaskChecking),
			attempt: baseAttempt(run.AttemptChecking),
			ctx:     run.AcceptanceContext{IncarnationCurrent: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome, err := run.AcceptResult(tc.run, tc.task, tc.attempt, &prior, tc.ctx, resubmission, later())

			if !errors.Is(err, run.ErrDuplicateResult) {
				t.Fatalf("AcceptResult: error = %v, want ErrDuplicateResult", err)
			}
			if outcome.Result != prior {
				t.Fatalf("Result = %+v, want the unchanged prior %+v", outcome.Result, prior)
			}
			if outcome.Run != tc.run || outcome.Task != tc.task || outcome.Attempt != tc.attempt {
				t.Fatalf("AcceptResult mutated its inputs on a duplicate outcome: %+v", outcome)
			}
		})
	}
}

// TestAcceptResultConflictingNeverDisturbsAcceptedResult proves that a
// resubmission with a different digest is rejected as conflicting in every
// attempt state and never replaces or alters the accepted result.
func TestAcceptResultConflictingNeverDisturbsAcceptedResult(t *testing.T) {
	prior := run.Result{ID: testResultID, AttemptID: testAttemptID, ContentDigest: "digest-v1", Accepted: true, CommitOID: "deadbeef"}

	for _, state := range attemptStates() {
		t.Run(string(state), func(t *testing.T) {
			r := baseRun(run.RunRunning, false)
			task := baseTask(run.TaskChecking)
			attempt := baseAttempt(state)
			conflicting := run.ResultSubmission{ID: identityOtherResultID, CommitOID: "c0ffee", Summary: "different work", Digest: "digest-v2"}

			outcome, err := run.AcceptResult(r, task, attempt, &prior, run.AcceptanceContext{IncarnationCurrent: true}, conflicting, later())

			if !errors.Is(err, run.ErrConflictingResult) {
				t.Fatalf("AcceptResult: error = %v, want ErrConflictingResult", err)
			}
			if outcome.Result != prior {
				t.Fatalf("Result = %+v, want the unchanged, still-accepted prior %+v", outcome.Result, prior)
			}
			if outcome.Run != r || outcome.Task != task || outcome.Attempt != attempt {
				t.Fatalf("AcceptResult mutated its inputs on a conflicting outcome: %+v", outcome)
			}
		})
	}
}
