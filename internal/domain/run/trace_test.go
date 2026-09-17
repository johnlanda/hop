package run_test

import (
	"errors"
	"testing"

	"github.com/johnlanda/hop/internal/domain/run"
)

// TestReferenceTraceFixtureWorkerSubmitsBeforeRunning is reference trace 1
// (docs/plan/phase-2-design.md section 5, "Reference traces"): a fixture
// worker submits before the controller observes its claim settle. Both
// orderings of that race are exercised: the controller applying the
// lifecycle transition first (subtest "controller-observes-first"), and the
// early-submission atomic handoff winning first (subtest
// "acceptance-wins-first").
func TestReferenceTraceFixtureWorkerSubmitsBeforeRunning(t *testing.T) {
	t.Run("controller-observes-first", func(t *testing.T) {
		r := run.NewRun(testRunID, testRepositoryID, 1, "brief-digest", epoch())
		task := run.NewTask(testTaskID, testRunID, "instructions-digest", epoch())
		attempt, err := run.NewAttempt(testAttemptID, testTaskID, 1, epoch())
		mustNoError(t, err)
		session := run.NewSession(testSessionID, testRunID, testAttemptID, run.HarnessClaude, epoch())

		// Intent journaled: Run created->launching, Task pending->active,
		// Attempt reserved->launching, Session reserved->launching.
		r, err = r.Launch(epoch())
		mustNoError(t, err)
		task, err = task.Activate(epoch())
		mustNoError(t, err)
		attempt, err = attempt.Launch(epoch())
		mustNoError(t, err)
		session, err = session.Launch(epoch())
		mustNoError(t, err)
		mustState(t, "run", string(r.State), string(run.RunLaunching))
		mustState(t, "task", string(task.State), string(run.TaskActive))
		mustState(t, "attempt", string(attempt.State), string(run.AttemptLaunching))
		mustState(t, "session", string(session.State), string(run.SessionLaunching))

		// A launch claim (exec_pending) is a store record the domain does
		// not model directly; it changes nothing here.

		// Submission arrives with the claim still unsettled: transient, no
		// transitions.
		submission := run.ResultSubmission{ID: testResultID, CommitOID: "deadbeef", Summary: "done", Digest: "digest-v1"}
		outcome, err := run.AcceptResult(r, task, attempt, nil, run.AcceptanceContext{IncarnationCurrent: true, LaunchClaimSettled: false, MailboxClear: true}, submission, later())
		if !errors.Is(err, run.ErrTransientNotRunning) {
			t.Fatalf("early submission before settlement: error = %v, want ErrTransientNotRunning", err)
		}
		if outcome.Run != r || outcome.Task != task || outcome.Attempt != attempt {
			t.Fatalf("transient submission mutated entities: %+v", outcome)
		}

		// Claim settles execed via the inspection predicate: Run running,
		// Attempt running, Session active.
		r, err = r.MarkRunning(later())
		mustNoError(t, err)
		attempt, err = attempt.MarkRunning(later())
		mustNoError(t, err)
		session, err = session.ConfirmActive(later())
		mustNoError(t, err)
		mustState(t, "run", string(r.State), string(run.RunRunning))
		mustState(t, "attempt", string(attempt.State), string(run.AttemptRunning))
		mustState(t, "session", string(session.State), string(run.SessionActive))

		// Resubmission accepted: Run running (idempotent, already there),
		// Task checking, Attempt submitted.
		outcome, err = run.AcceptResult(r, task, attempt, nil, run.AcceptanceContext{IncarnationCurrent: true, MailboxClear: true}, submission, later())
		mustNoError(t, err)
		r, task, attempt = outcome.Run, outcome.Task, outcome.Attempt
		mustState(t, "run", string(r.State), string(run.RunRunning))
		mustState(t, "task", string(task.State), string(run.TaskChecking))
		mustState(t, "attempt", string(attempt.State), string(run.AttemptSubmitted))

		// Check claimed: Run completing, Attempt checking.
		r, err = r.EnterCompleting(later())
		mustNoError(t, err)
		attempt, err = attempt.EnterChecking(later())
		mustNoError(t, err)
		mustState(t, "run", string(r.State), string(run.RunCompleting))
		mustState(t, "attempt", string(attempt.State), string(run.AttemptChecking))
	})

	t.Run("acceptance-wins-first", func(t *testing.T) {
		r := run.NewRun(testRunID, testRepositoryID, 1, "brief-digest", epoch())
		task := run.NewTask(testTaskID, testRunID, "instructions-digest", epoch())
		attempt, err := run.NewAttempt(testAttemptID, testTaskID, 1, epoch())
		mustNoError(t, err)
		session := run.NewSession(testSessionID, testRunID, testAttemptID, run.HarnessClaude, epoch())

		r, err = r.Launch(epoch())
		mustNoError(t, err)
		task, err = task.Activate(epoch())
		mustNoError(t, err)
		attempt, err = attempt.Launch(epoch())
		mustNoError(t, err)
		session, err = session.Launch(epoch())
		mustNoError(t, err)

		// The claim settles and the early-submission atomic handoff wins
		// before the controller applies its own lifecycle observation:
		// Run launching->running, Task active->checking, Attempt
		// launching->submitted, all in this one call.
		submission := run.ResultSubmission{ID: testResultID, CommitOID: "deadbeef", Summary: "done", Digest: "digest-v1"}
		outcome, err := run.AcceptResult(r, task, attempt, nil, run.AcceptanceContext{IncarnationCurrent: true, LaunchClaimSettled: true, MailboxClear: true}, submission, later())
		mustNoError(t, err)
		r, task, attempt = outcome.Run, outcome.Task, outcome.Attempt
		mustState(t, "run", string(r.State), string(run.RunRunning))
		mustState(t, "task", string(task.State), string(run.TaskChecking))
		mustState(t, "attempt", string(attempt.State), string(run.AttemptSubmitted))

		// The controller's later observation of the settled claim only
		// still has work to do for Session (Run and Attempt already moved
		// past the states that observation would have driven).
		session, err = session.ConfirmActive(later())
		mustNoError(t, err)
		mustState(t, "session", string(session.State), string(run.SessionActive))
	})
}

// TestReferenceTraceColdRelaunchSubmitBeforeRunning is reference trace 2: a
// cold relaunch whose new incarnation submits before the controller
// observes it running.
func TestReferenceTraceColdRelaunchSubmitBeforeRunning(t *testing.T) {
	r := run.Run{ID: testRunID, RepositoryID: testRepositoryID, Sequence: 1, State: run.RunResuming}
	task := run.Task{ID: testTaskID, RunID: testRunID, State: run.TaskActive}
	attempt := run.Attempt{ID: testAttemptID, TaskID: testTaskID, Number: 1, State: run.AttemptReconciling}

	// Relaunch authorized: Run resuming->launching, Attempt
	// reconciling->relaunching, new Session' reserved->launching (a fresh
	// session and incarnation for the same attempt).
	r, err := r.Launch(epoch())
	mustNoError(t, err)
	attempt, err = attempt.Relaunch(epoch())
	mustNoError(t, err)
	newSession := run.NewSession(testSecondSessionID, testRunID, testAttemptID, run.HarnessClaude, epoch())
	newSession, err = newSession.Launch(epoch())
	mustNoError(t, err)
	mustState(t, "run", string(r.State), string(run.RunLaunching))
	mustState(t, "attempt", string(attempt.State), string(run.AttemptRelaunching))
	mustState(t, "session'", string(newSession.State), string(run.SessionLaunching))

	// Claim settles; submission accepted: the same atomic handoff with
	// Attempt relaunching->submitted, Run launching->running, Task
	// active->checking.
	submission := run.ResultSubmission{ID: testResultID, CommitOID: "deadbeef", Summary: "done", Digest: "digest-v1"}
	outcome, err := run.AcceptResult(r, task, attempt, nil, run.AcceptanceContext{IncarnationCurrent: true, LaunchClaimSettled: true, MailboxClear: true}, submission, later())
	mustNoError(t, err)
	r, task, attempt = outcome.Run, outcome.Task, outcome.Attempt
	mustState(t, "run", string(r.State), string(run.RunRunning))
	mustState(t, "task", string(task.State), string(run.TaskChecking))
	mustState(t, "attempt", string(attempt.State), string(run.AttemptSubmitted))

	// Session' launching->active on the settled claim.
	newSession, err = newSession.ConfirmActive(later())
	mustNoError(t, err)
	mustState(t, "session'", string(newSession.State), string(run.SessionActive))
}

// TestReferenceTraceExecFailureAfterRelaunch is reference trace 3: as trace
// 2 up to the claim, which instead settles exec_failed.
func TestReferenceTraceExecFailureAfterRelaunch(t *testing.T) {
	r := run.Run{ID: testRunID, RepositoryID: testRepositoryID, Sequence: 1, State: run.RunResuming}
	task := run.Task{ID: testTaskID, RunID: testRunID, State: run.TaskActive}
	attempt := run.Attempt{ID: testAttemptID, TaskID: testTaskID, Number: 1, State: run.AttemptReconciling}

	r, err := r.Launch(epoch())
	mustNoError(t, err)
	attempt, err = attempt.Relaunch(epoch())
	mustNoError(t, err)
	newSession := run.NewSession(testSecondSessionID, testRunID, testAttemptID, run.HarnessClaude, epoch())
	newSession, err = newSession.Launch(epoch())
	mustNoError(t, err)

	// The claim settles exec_failed: Attempt relaunching->failed, Session'
	// launching->terminated, Task active->failed, Run launching->failed.
	attempt, err = attempt.Fail(later())
	mustNoError(t, err)
	newSession, err = newSession.Terminate(later())
	mustNoError(t, err)
	task, err = task.Fail(later())
	mustNoError(t, err)
	r, err = r.Fail(later())
	mustNoError(t, err)

	mustState(t, "attempt", string(attempt.State), string(run.AttemptFailed))
	mustState(t, "session'", string(newSession.State), string(run.SessionTerminated))
	mustState(t, "task", string(task.State), string(run.TaskFailed))
	mustState(t, "run", string(r.State), string(run.RunFailed))
}

// TestReferenceTraceStopDuringLaunchingWithLiveProcess is reference trace 4:
// stop requested while launching, with a corroborated live process to
// interrupt.
func TestReferenceTraceStopDuringLaunchingWithLiveProcess(t *testing.T) {
	r := run.NewRun(testRunID, testRepositoryID, 1, "brief-digest", epoch())
	task := run.NewTask(testTaskID, testRunID, "instructions-digest", epoch())
	attempt, err := run.NewAttempt(testAttemptID, testTaskID, 1, epoch())
	mustNoError(t, err)
	session := run.NewSession(testSessionID, testRunID, testAttemptID, run.HarnessClaude, epoch())

	r, err = r.Launch(epoch())
	mustNoError(t, err)
	task, err = task.Activate(epoch())
	mustNoError(t, err)
	attempt, err = attempt.Launch(epoch())
	mustNoError(t, err)
	session, err = session.Launch(epoch())
	mustNoError(t, err)

	// Stop requested: Run launching->stopping; Session
	// launching->stopping (interrupt under the close rule, a corroborated
	// live process).
	r = r.RequestStop(later())
	session, err = session.Stop(later())
	mustNoError(t, err)
	mustState(t, "run", string(r.State), string(run.RunStopping))
	if !r.StopRequested {
		t.Fatal("run.StopRequested = false, want true")
	}
	mustState(t, "session", string(session.State), string(run.SessionStopping))

	// Termination observed: Session terminated, Attempt
	// launching->interrupted, Task active->interrupted, Run
	// stopping->stopped.
	session, err = session.Terminate(later())
	mustNoError(t, err)
	attempt, err = attempt.Interrupt(later())
	mustNoError(t, err)
	task, err = task.Interrupt(later())
	mustNoError(t, err)
	r, err = r.MarkStopped(later())
	mustNoError(t, err)

	mustState(t, "session", string(session.State), string(run.SessionTerminated))
	mustState(t, "attempt", string(attempt.State), string(run.AttemptInterrupted))
	mustState(t, "task", string(task.State), string(run.TaskInterrupted))
	mustState(t, "run", string(r.State), string(run.RunStopped))
}

func mustNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func mustState(t *testing.T, entity, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s state = %s, want %s", entity, got, want)
	}
}
