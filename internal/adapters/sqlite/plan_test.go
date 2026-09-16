package sqlite_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// taskCreate builds a manager task-create request against the fixture.
func taskCreate(f *featureFixture, n int, title, requestID string, deps ...identity.TaskID) app.TaskCreate {
	return app.TaskCreate{
		ID: identity.TaskID(uid(n)), RunID: f.spec.RunID, Session: f.ManagerID, IncarnationID: f.ManagerIncarnation,
		Title: title, InstructionsPath: "/state/instructions/" + uid(n) + ".md", InstructionsDigest: "instructions-" + uid(n),
		DependsOn: deps, RequestID: requestID,
	}
}

// TestCreateTaskLifecycle drives acceptance, dependency persistence, the
// request-ID idempotency pair and plan reopen-on-create.
func TestCreateTaskLifecycle(t *testing.T) {
	f := newFeatureFixture(t)

	// Close the plan first: an accepted create must reopen it.
	if outcome, err := f.store.ClosePlan(t.Context(), app.PlanClose{RunID: f.spec.RunID, Session: f.ManagerID, IncarnationID: f.ManagerIncarnation}); err != nil || outcome.Outcome != app.WorkflowAccepted {
		t.Fatalf("ClosePlan() = %+v, %v; want accepted", outcome, err)
	}
	f.inUOW(t, func(uow app.UnitOfWork) {
		if r, _, err := uow.Runs().Get(t.Context(), f.spec.RunID); err != nil || !r.PlanClosed {
			t.Fatalf("run plan flag after close = %t, %v; want closed", r.PlanClosed, err)
		}
	})

	created, err := f.store.CreateTask(t.Context(), taskCreate(f, 7501, "task one", "create-1"))
	if err != nil || created.Outcome != app.WorkflowAccepted {
		t.Fatalf("CreateTask() = %+v, %v; want accepted", created, err)
	}
	if created.Seq != 2 {
		t.Fatalf("created task seq = %d, want 2 (the bootstrap task holds 1)", created.Seq)
	}
	f.inUOW(t, func(uow app.UnitOfWork) {
		if r, _, getErr := uow.Runs().Get(t.Context(), f.spec.RunID); getErr != nil || r.PlanClosed {
			t.Fatalf("run plan flag after create = %t, %v; an accepted create reopens the plan", r.PlanClosed, getErr)
		}
		task, _, getErr := uow.Tasks().Get(t.Context(), created.TaskID)
		if getErr != nil || task.Title != "task one" || task.Kind != run.TaskKindImplement || task.State != run.TaskReady || task.HasDependencies {
			t.Fatalf("created task = %+v, %v; want a ready zero-dependency implement task", task, getErr)
		}
	})
	var instructionsPath string
	if scanErr := sqlite.WriteDB(f.store).QueryRowContext(t.Context(), `SELECT instructions_path FROM tasks WHERE id = ?`, created.TaskID.String()).Scan(&instructionsPath); scanErr != nil || instructionsPath == "" {
		t.Fatalf("created task instructions_path = %q, %v; want the artifact reference persisted", instructionsPath, scanErr)
	}

	// A dependent task lands pending with its edge persisted.
	dependent, err := f.store.CreateTask(t.Context(), taskCreate(f, 7502, "task two", "", created.TaskID))
	if err != nil || dependent.Outcome != app.WorkflowAccepted || dependent.Seq != 3 {
		t.Fatalf("CreateTask(dependent) = %+v, %v; want accepted at seq 3", dependent, err)
	}
	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		edges, edgesErr := wf.TaskDependencies().ByRun(t.Context(), f.spec.RunID)
		if edgesErr != nil || len(edges) != 1 || edges[0].TaskID != dependent.TaskID || edges[0].PrerequisiteID != created.TaskID {
			t.Fatalf("edges = %+v, %v; want the one persisted dependency", edges, edgesErr)
		}
		task, _, getErr := uow.Tasks().Get(t.Context(), dependent.TaskID)
		if getErr != nil || task.State != run.TaskPending || !task.HasDependencies {
			t.Fatalf("dependent task = %+v, %v; want pending with dependencies", task, getErr)
		}
	})

	// Identical retry (a fresh task id, the same content): the original
	// acceptance, reported duplicate.
	retryReq := taskCreate(f, 7501, "task one", "create-1")
	retryReq.ID = identity.TaskID(uid(7503))
	retry, err := f.store.CreateTask(t.Context(), retryReq)
	if err != nil || retry.Outcome != app.WorkflowDuplicate || retry.TaskID != created.TaskID || retry.Seq != created.Seq {
		t.Fatalf("identical retry = %+v, %v; want duplicate of t%d", retry, err, created.Seq)
	}
	// Conflicting reuse: same ID, different content.
	conflicting, err := f.store.CreateTask(t.Context(), taskCreate(f, 7504, "different content", "create-1"))
	if err != nil || conflicting.Outcome != app.WorkflowRefused || !strings.Contains(conflicting.Detail, "request id reused") {
		t.Fatalf("conflicting reuse = %+v, %v; want refused", conflicting, err)
	}
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM tasks WHERE run_id = ?`, f.spec.RunID.String()); n != 3 {
		t.Fatalf("tasks after retries = %d, want bootstrap + the two accepted creates", n)
	}
}

// TestCreateTaskRefusals drives the refusal matrix.
func TestCreateTaskRefusals(t *testing.T) {
	f := newFeatureFixture(t)

	t.Run("non-manager caller", func(t *testing.T) {
		taskB := f.createFeatureTask(t, 7511, 2, run.TaskActive)
		workerID, workerIncarnation := f.createWorkerSession(t, taskB, run.RoleImplementer, 7512)
		req := taskCreate(f, 7515, "worker-created", "")
		req.Session, req.IncarnationID = workerID, workerIncarnation
		outcome, err := f.store.CreateTask(t.Context(), req)
		if err != nil || outcome.Outcome != app.WorkflowRefused {
			t.Fatalf("CreateTask(non-manager) = %+v, %v; want refused", outcome, err)
		}
	})
	t.Run("stale incarnation", func(t *testing.T) {
		req := taskCreate(f, 7516, "stale", "")
		req.IncarnationID = identity.IncarnationID(uid(9994))
		outcome, err := f.store.CreateTask(t.Context(), req)
		if err != nil || outcome.Outcome != app.WorkflowRefused {
			t.Fatalf("CreateTask(stale incarnation) = %+v, %v; want refused", outcome, err)
		}
	})
	t.Run("oversized title is malformed", func(t *testing.T) {
		req := taskCreate(f, 7517, strings.Repeat("x", app.TaskTitleLimit+1), "")
		outcome, err := f.store.CreateTask(t.Context(), req)
		if err != nil || outcome.Outcome != app.WorkflowMalformed {
			t.Fatalf("CreateTask(oversized title) = %+v, %v; want malformed", outcome, err)
		}
	})
	t.Run("cross-run dependency", func(t *testing.T) {
		other := newSpec("/repos/other", 2*specStride, f.clock.Now())
		if _, _, err := f.store.InitializeRun(t.Context(), other); err != nil {
			t.Fatalf("InitializeRun other: %v", err)
		}
		outcome, err := f.store.CreateTask(t.Context(), taskCreate(f, 7518, "cross-run dep", "", other.TaskID))
		if err != nil || outcome.Outcome != app.WorkflowRefused || !strings.Contains(outcome.Detail, "dependency is not in this run") {
			t.Fatalf("CreateTask(cross-run dep) = %+v, %v; want refused", outcome, err)
		}
	})
	t.Run("run not accepting after stop", func(t *testing.T) {
		if err := f.store.RequestStop(t.Context(), f.spec.RunID); err != nil {
			t.Fatalf("RequestStop: %v", err)
		}
		outcome, err := f.store.CreateTask(t.Context(), taskCreate(f, 7519, "late create", ""))
		if err != nil || outcome.Outcome != app.WorkflowRefused {
			t.Fatalf("CreateTask(stopping run) = %+v, %v; want refused", outcome, err)
		}
	})
}

// TestCreateTaskRaced races two creates from separate handles: both
// commit with distinct sequence numbers under UNIQUE(run_id, seq), and the
// acyclicity check reads the persisted graph inside each transaction.
func TestCreateTaskRaced(t *testing.T) {
	clock := newFakeClock()
	root := t.TempDir()
	storeA := openStoreAt(t, root, clock)
	storeB := openStoreAt(t, root, clock)
	f := buildMessagingFixtureAt(t, storeA, clock)

	var (
		start    sync.WaitGroup
		done     sync.WaitGroup
		outcomes [2]app.TaskCreated
		errs     [2]error
	)
	start.Add(1)
	done.Add(2)
	go func() {
		defer done.Done()
		start.Wait()
		outcomes[0], errs[0] = storeA.CreateTask(t.Context(), taskCreate(f.featureFixture, 7521, "raced A", "raced-a"))
	}()
	go func() {
		defer done.Done()
		start.Wait()
		outcomes[1], errs[1] = storeB.CreateTask(t.Context(), taskCreate(f.featureFixture, 7522, "raced B", "raced-b", f.TaskB))
	}()
	start.Done()
	done.Wait()

	if errs[0] != nil || errs[1] != nil || outcomes[0].Outcome != app.WorkflowAccepted || outcomes[1].Outcome != app.WorkflowAccepted {
		t.Fatalf("raced creates = %+v %v / %+v %v; want both accepted", outcomes[0], errs[0], outcomes[1], errs[1])
	}
	if outcomes[0].Seq == outcomes[1].Seq {
		t.Fatalf("raced creates share seq %d; UNIQUE(run_id, seq) must have serialized them apart", outcomes[0].Seq)
	}
	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		edges, err := wf.TaskDependencies().ByRun(t.Context(), f.spec.RunID)
		if err != nil || len(edges) != 1 {
			t.Fatalf("edges after the race = %+v, %v; want B's one edge", edges, err)
		}
	})
}

// TestClosePlan drives acceptance, the duplicate retry, the empty-plan
// refusal and the flag surviving reopen and takeover.
func TestClosePlan(t *testing.T) {
	t.Run("empty plan is refused", func(t *testing.T) {
		f := newFeatureFixture(t)
		// The bootstrap solo task backfills as an implement task; recast it
		// so the run genuinely has no implement task.
		rawExec(t, f.store, `UPDATE tasks SET kind = 'review' WHERE id = ?`, f.spec.TaskID.String())
		outcome, err := f.store.ClosePlan(t.Context(), app.PlanClose{RunID: f.spec.RunID, Session: f.ManagerID, IncarnationID: f.ManagerIncarnation})
		if err != nil || outcome.Outcome != app.WorkflowRefused || !strings.Contains(outcome.Detail, "no implement task") {
			t.Fatalf("ClosePlan(empty plan) = %+v, %v; want refused", outcome, err)
		}
	})

	t.Run("flag survives reopen and takeover", func(t *testing.T) {
		f := newFeatureFixture(t)
		closeReq := app.PlanClose{RunID: f.spec.RunID, Session: f.ManagerID, IncarnationID: f.ManagerIncarnation, RequestID: "close-1"}
		if outcome, err := f.store.ClosePlan(t.Context(), closeReq); err != nil || outcome.Outcome != app.WorkflowAccepted {
			t.Fatalf("ClosePlan() = %+v, %v; want accepted", outcome, err)
		}
		if outcome, err := f.store.ClosePlan(t.Context(), closeReq); err != nil || outcome.Outcome != app.WorkflowDuplicate {
			t.Fatalf("repeat ClosePlan() = %+v, %v; want duplicate", outcome, err)
		}

		// An accepted create REOPENS the plan...
		if outcome, err := f.store.CreateTask(t.Context(), taskCreate(f, 7531, "reopener", "")); err != nil || outcome.Outcome != app.WorkflowAccepted {
			t.Fatalf("CreateTask(reopener) = %+v, %v", outcome, err)
		}
		f.inUOW(t, func(uow app.UnitOfWork) {
			if r, _, err := uow.Runs().Get(t.Context(), f.spec.RunID); err != nil || r.PlanClosed {
				t.Fatalf("plan flag after reopener = %t, %v; want open", r.PlanClosed, err)
			}
		})
		// ...a fresh close sets it again...
		reclose := app.PlanClose{RunID: f.spec.RunID, Session: f.ManagerID, IncarnationID: f.ManagerIncarnation}
		if outcome, err := f.store.ClosePlan(t.Context(), reclose); err != nil || outcome.Outcome != app.WorkflowAccepted {
			t.Fatalf("re-close = %+v, %v; want accepted", outcome, err)
		}
		// ...and a takeover (a NEW lease generation) reads the same durable
		// flag: it is a run-row column under the ordinary revision
		// discipline, not lease state.
		if err := f.store.ReleaseLease(t.Context(), f.lease); err != nil {
			t.Fatalf("ReleaseLease: %v", err)
		}
		takeover, err := f.store.AcquireLease(t.Context(), f.spec.RunID, "controller-b")
		if err != nil {
			t.Fatalf("AcquireLease: %v", err)
		}
		uow, err := f.store.Begin(t.Context(), takeover)
		if err != nil {
			t.Fatalf("Begin under takeover: %v", err)
		}
		defer uow.Rollback() //nolint:errcheck // read-only unit of work.
		if r, _, err := uow.Runs().Get(t.Context(), f.spec.RunID); err != nil || !r.PlanClosed {
			t.Fatalf("plan flag after takeover = %t, %v; want still closed", r.PlanClosed, err)
		}
	})
}

// TestRequestRetry drives the full retry cycle: reservation inside the
// accepting transaction, the bookkeeping row, mailbox reopen, the
// duplicate surviving consumption, and the refusal matrix.
func TestRequestRetry(t *testing.T) {
	f := newFeatureFixture(t)
	taskB := f.createFeatureTask(t, 7541, 2, run.TaskNeedsRework)
	// A terminal prior attempt, mailbox closed by the prior settlement.
	seedTerminalAttempt(t, f, taskB, 7542, 1)
	rawExec(t, f.store, `UPDATE tasks SET mailbox_closed_at = '2026-09-14T09:00:00.000000000Z' WHERE id = ?`, taskB.String())

	request := app.RetryRequest{
		TaskID: taskB, RunID: f.spec.RunID, Session: f.ManagerID, IncarnationID: f.ManagerIncarnation,
		Reason: "flaky check", RequestID: "retry-1",
	}
	accepted, err := f.store.RequestRetry(t.Context(), request)
	if err != nil || accepted.Outcome != app.WorkflowAccepted || accepted.AttemptNumber != 2 {
		t.Fatalf("RequestRetry() = %+v, %v; want attempt 2 reserved", accepted, err)
	}
	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		attempts, listErr := wf.AttemptIndex().ByTask(t.Context(), taskB)
		if listErr != nil || len(attempts) != 2 || attempts[1].Number != 2 || attempts[1].State != run.AttemptReserved {
			t.Fatalf("attempts after retry = %+v, %v; want the reserved successor", attempts, listErr)
		}
		task, _, getErr := uow.Tasks().Get(t.Context(), taskB)
		if getErr != nil || task.State != run.TaskReady || task.MailboxClosed {
			t.Fatalf("task after retry = %+v, %v; want ready with the mailbox reopened", task, getErr)
		}
		pending, pendingErr := wf.RetryRequests().Pending(t.Context(), f.spec.RunID)
		if pendingErr != nil || len(pending) != 1 || pending[0].TaskID != taskB {
			t.Fatalf("pending retry rows = %+v, %v; want the one bookkeeping row", pending, pendingErr)
		}
	})
	var retryCount int
	if scanErr := sqlite.WriteDB(f.store).QueryRowContext(t.Context(), `SELECT retry_count FROM tasks WHERE id = ?`, taskB.String()).Scan(&retryCount); scanErr != nil || retryCount != 1 {
		t.Fatalf("retry_count = %d, %v; want 1 accepted retry recorded", retryCount, scanErr)
	}

	// A second request while one is pending is refused. The acceptance
	// already moved the task to ready, so the state check would fire
	// first; put the task back to needs-rework to expose the unique-
	// pending refusal itself (defense in depth against a state row that
	// regressed while the bookkeeping row stands).
	rawExec(t, f.store, `UPDATE tasks SET state = 'needs-rework' WHERE id = ?`, taskB.String())
	second := request
	second.RequestID = "retry-2"
	if outcome, retryErr := f.store.RequestRetry(t.Context(), second); retryErr != nil || outcome.Outcome != app.WorkflowRefused || !strings.Contains(outcome.Detail, "already pending") {
		t.Fatalf("RequestRetry(pending exists) = %+v, %v; want refused", outcome, retryErr)
	}
	rawExec(t, f.store, `UPDATE tasks SET state = 'ready' WHERE id = ?`, taskB.String())

	// The identical retry survives CONSUMPTION: it reads the original
	// acceptance back from the receipt, never a second pending row.
	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		if consumeErr := wf.RetryRequests().MarkConsumed(t.Context(), taskB, 2); consumeErr != nil {
			t.Fatalf("MarkConsumed: %v", consumeErr)
		}
	})
	duplicate, err := f.store.RequestRetry(t.Context(), request)
	if err != nil || duplicate.Outcome != app.WorkflowDuplicate || duplicate.AttemptNumber != 2 {
		t.Fatalf("identical retry after consumption = %+v, %v; want duplicate naming attempt 2", duplicate, err)
	}
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM retry_requests WHERE task_id = ? AND state = 'pending'`, taskB.String()); n != 0 {
		t.Fatalf("pending rows after duplicate = %d, want none", n)
	}
}

// TestRequestRetryRefusals drives the retry refusal matrix.
func TestRequestRetryRefusals(t *testing.T) {
	f := newFeatureFixture(t)

	t.Run("task not needs-rework", func(t *testing.T) {
		taskB := f.createFeatureTask(t, 7551, 2, run.TaskActive)
		outcome, err := f.store.RequestRetry(t.Context(), app.RetryRequest{TaskID: taskB, RunID: f.spec.RunID, Session: f.ManagerID, IncarnationID: f.ManagerIncarnation})
		if err != nil || outcome.Outcome != app.WorkflowRefused || !strings.Contains(outcome.Detail, "not needs-rework") {
			t.Fatalf("RequestRetry(active task) = %+v, %v", outcome, err)
		}
	})
	t.Run("prior attempt not terminal", func(t *testing.T) {
		taskB := f.createFeatureTask(t, 7552, 3, run.TaskNeedsRework)
		f.inUOW(t, func(uow app.UnitOfWork) {
			wf := workflowRepos(t, uow)
			attempt, attemptErr := run.NewAttempt(identity.AttemptID(uid(7553)), taskB, 1, f.clock.Now())
			if attemptErr != nil {
				t.Fatal(attemptErr)
			}
			if _, err := wf.AttemptIndex().Create(t.Context(), attempt); err != nil {
				t.Fatal(err)
			}
		})
		outcome, err := f.store.RequestRetry(t.Context(), app.RetryRequest{TaskID: taskB, RunID: f.spec.RunID, Session: f.ManagerID, IncarnationID: f.ManagerIncarnation})
		if err != nil || outcome.Outcome != app.WorkflowRefused || !strings.Contains(outcome.Detail, "not terminal") {
			t.Fatalf("RequestRetry(live attempt) = %+v, %v", outcome, err)
		}
	})
	t.Run("retry limit reached", func(t *testing.T) {
		taskB := f.createFeatureTask(t, 7554, 4, run.TaskNeedsRework)
		seedTerminalAttempt(t, f, taskB, 7555, 3) // the frozen limit
		outcome, err := f.store.RequestRetry(t.Context(), app.RetryRequest{TaskID: taskB, RunID: f.spec.RunID, Session: f.ManagerID, IncarnationID: f.ManagerIncarnation})
		if err != nil || outcome.Outcome != app.WorkflowRefused || !strings.Contains(outcome.Detail, "retry limit") {
			t.Fatalf("RequestRetry(at limit) = %+v, %v", outcome, err)
		}
	})
	t.Run("unknown task is malformed", func(t *testing.T) {
		outcome, err := f.store.RequestRetry(t.Context(), app.RetryRequest{TaskID: identity.TaskID(uid(9995)), RunID: f.spec.RunID, Session: f.ManagerID, IncarnationID: f.ManagerIncarnation})
		if err != nil || outcome.Outcome != app.WorkflowMalformed {
			t.Fatalf("RequestRetry(unknown task) = %+v, %v", outcome, err)
		}
	})
}

// TestRetryRequestUniquePendingRaced races two handles requesting a retry
// for the same task with distinct request IDs: exactly one acceptance, one
// pending row, one reserved successor attempt.
func TestRetryRequestUniquePendingRaced(t *testing.T) {
	clock := newFakeClock()
	root := t.TempDir()
	storeA := openStoreAt(t, root, clock)
	storeB := openStoreAt(t, root, clock)
	f := buildMessagingFixtureAt(t, storeA, clock)
	taskC := f.createFeatureTask(t, 7561, 3, run.TaskNeedsRework)
	seedTerminalAttempt(t, f.featureFixture, taskC, 7562, 1)

	request := func(id string) app.RetryRequest {
		return app.RetryRequest{TaskID: taskC, RunID: f.spec.RunID, Session: f.ManagerID, IncarnationID: f.ManagerIncarnation, Reason: "raced", RequestID: id}
	}
	var (
		start    sync.WaitGroup
		done     sync.WaitGroup
		outcomes [2]app.RetryAccepted
		errs     [2]error
	)
	start.Add(1)
	done.Add(2)
	go func() {
		defer done.Done()
		start.Wait()
		outcomes[0], errs[0] = storeA.RequestRetry(t.Context(), request("raced-a"))
	}()
	go func() {
		defer done.Done()
		start.Wait()
		outcomes[1], errs[1] = storeB.RequestRetry(t.Context(), request("raced-b"))
	}()
	start.Done()
	done.Wait()

	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("raced retries errored: %v %v", errs[0], errs[1])
	}
	accepted, refused := 0, 0
	for _, o := range outcomes {
		switch o.Outcome {
		case app.WorkflowAccepted:
			accepted++
		case app.WorkflowRefused:
			refused++
		default:
			t.Fatalf("raced retry outcome = %+v", o)
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatalf("raced retries = %d accepted, %d refused; want exactly one acceptance", accepted, refused)
	}
	if n := countRows(t, storeA, `SELECT COUNT(*) FROM retry_requests WHERE task_id = ? AND state = 'pending'`, taskC.String()); n != 1 {
		t.Fatalf("pending rows after the race = %d, want 1", n)
	}
	if n := countRows(t, storeA, `SELECT COUNT(*) FROM attempts WHERE task_id = ?`, taskC.String()); n != 2 {
		t.Fatalf("attempts after the race = %d, want the prior and the one reserved successor", n)
	}
}

// seedTerminalAttempt inserts a FAILED attempt with the given number for
// task through the controller index.
func seedTerminalAttempt(t *testing.T, f *featureFixture, task identity.TaskID, n, number int) {
	t.Helper()
	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		attempt, err := run.NewAttempt(identity.AttemptID(uid(n)), task, number, f.clock.Now())
		if err != nil {
			t.Fatalf("new attempt: %v", err)
		}
		attempt.State = run.AttemptFailed
		if _, err := wf.AttemptIndex().Create(t.Context(), attempt); err != nil {
			t.Fatalf("create terminal attempt: %v", err)
		}
	})
}

// TestWorkflowReceiptAcceptanceKey proves the (run, verb, request ID)
// key's scope for the plan verbs: the same request ID is accepted
// independently across two VERBS and across two RUNS, while a reuse
// within one (run, verb) with different content stays refused.
func TestWorkflowReceiptAcceptanceKey(t *testing.T) {
	clock := newFakeClock()
	store := openStoreAt(t, t.TempDir(), clock)
	f := buildMessagingFixtureAt(t, store, clock)
	const requestID = "shared-plan-id"

	// Accepted under task-create.
	created, err := store.CreateTask(t.Context(), taskCreate(f.featureFixture, 8101, "keyed task", requestID))
	if err != nil || created.Outcome != app.WorkflowAccepted {
		t.Fatalf("CreateTask() = %+v, %v", created, err)
	}
	// The SAME ID under task-retry is an independent acceptance.
	taskC := f.createFeatureTask(t, 8102, 4, run.TaskNeedsRework)
	seedTerminalAttempt(t, f.featureFixture, taskC, 8103, 1)
	retry, err := store.RequestRetry(t.Context(), app.RetryRequest{
		TaskID: taskC, RunID: f.spec.RunID, Session: f.ManagerID, IncarnationID: f.ManagerIncarnation,
		Reason: "keyed retry", RequestID: requestID,
	})
	if err != nil || retry.Outcome != app.WorkflowAccepted || retry.AttemptNumber != 2 {
		t.Fatalf("RequestRetry(same id, other verb) = %+v, %v; want an independent acceptance", retry, err)
	}
	// The SAME ID under plan-close is a third independent acceptance.
	planClose, err := store.ClosePlan(t.Context(), app.PlanClose{RunID: f.spec.RunID, Session: f.ManagerID, IncarnationID: f.ManagerIncarnation, RequestID: requestID})
	if err != nil || planClose.Outcome != app.WorkflowAccepted {
		t.Fatalf("ClosePlan(same id, third verb) = %+v, %v; want an independent acceptance", planClose, err)
	}
	// The SAME ID in another RUN accepts independently under task-create.
	other := buildSecondMessagingFixtureAt(t, store, clock)
	otherCreate, err := store.CreateTask(t.Context(), taskCreate(other.featureFixture, 8104, "keyed task", requestID))
	if err != nil || otherCreate.Outcome != app.WorkflowAccepted {
		t.Fatalf("CreateTask(same id, other run) = %+v, %v; want an independent acceptance", otherCreate, err)
	}
	// Within one (run, verb), a different-content reuse stays refused.
	conflicting, err := store.CreateTask(t.Context(), taskCreate(f.featureFixture, 8105, "different content", requestID))
	if err != nil || conflicting.Outcome != app.WorkflowRefused {
		t.Fatalf("CreateTask(conflicting reuse) = %+v, %v; want refused", conflicting, err)
	}
}
