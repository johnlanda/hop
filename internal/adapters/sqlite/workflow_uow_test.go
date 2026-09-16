package sqlite_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// TestWorkflowRepositoriesCapability proves the additive packaging rule's
// slice-3 half: the same fenced unit of work satisfies
// app.RequireWorkflowRepositories, so feature-mode use cases reach the
// Phase 3 repositories against the real store exactly as they do against
// the fakes.
func TestWorkflowRepositoriesCapability(t *testing.T) {
	f := newFeatureFixture(t)
	f.inUOW(t, func(uow app.UnitOfWork) {
		if _, err := app.RequireWorkflowRepositories(uow, "capability probe"); err != nil {
			t.Fatalf("RequireWorkflowRepositories() = %v, want the sqlite unit of work to implement it", err)
		}
	})
}

// TestTaskIndex proves controller-transaction task creation and the bulk
// listing: a review task's row carries kind, seq, frozen subject and an
// empty instructions path; ByRun returns every task in seq order with
// dependency-shape and mailbox facts mapped.
func TestTaskIndex(t *testing.T) {
	f := newFeatureFixture(t)
	now := f.clock.Now()
	reviewID := identity.TaskID(uid(7101))

	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		review := run.NewReviewTask(reviewID, f.spec.RunID, 2, "commit-head", "tree-head", now)
		revision, err := wf.TaskIndex().Create(t.Context(), review)
		if err != nil {
			t.Fatalf("TaskIndex().Create() = %v", err)
		}
		if revision != 1 {
			t.Fatalf("created task revision = %d, want 1", revision)
		}
	})

	var instructionsPath string
	if err := sqlite.WriteDB(f.store).QueryRowContext(t.Context(), `SELECT instructions_path FROM tasks WHERE id = ?`, reviewID.String()).Scan(&instructionsPath); err != nil {
		t.Fatalf("read review task row: %v", err)
	}
	if instructionsPath != "" {
		t.Fatalf("review task instructions_path = %q, want empty", instructionsPath)
	}

	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		tasks, err := wf.TaskIndex().ByRun(t.Context(), f.spec.RunID)
		if err != nil {
			t.Fatalf("TaskIndex().ByRun() = %v", err)
		}
		if len(tasks) != 2 {
			t.Fatalf("ByRun returned %d tasks, want the bootstrap task and the review task", len(tasks))
		}
		if tasks[0].ID != f.spec.TaskID || tasks[0].Seq != 1 || tasks[0].Kind != run.TaskKindImplement {
			t.Fatalf("bootstrap task row = %+v, want seq 1 implement", tasks[0])
		}
		review := tasks[1]
		if review.ID != reviewID || review.Kind != run.TaskKindReview || review.Seq != 2 ||
			review.SubjectCommitOID != "commit-head" || review.SubjectTreeOID != "tree-head" ||
			review.State != run.TaskReady || review.HasDependencies || review.MailboxClosed {
			t.Fatalf("review task row = %+v, want the frozen subject with seq 2 ready", review)
		}
		task, _, err := uow.Tasks().Get(t.Context(), reviewID)
		if err != nil {
			t.Fatalf("Tasks().Get(review) = %v", err)
		}
		if task.Kind != run.TaskKindReview || task.SubjectCommitOID != "commit-head" {
			t.Fatalf("Tasks().Get(review) = %+v, want the same typed row", task)
		}
	})
}

// TestTaskDependenciesByRun reads persisted dependency edges back in
// creation order, scoped to the run.
func TestTaskDependenciesByRun(t *testing.T) {
	f := newFeatureFixture(t)
	taskB := f.createFeatureTask(t, 7111, 2, run.TaskPending)
	at := "2026-09-14T10:00:00.000000000Z"
	rawExec(t, f.store, `INSERT INTO task_dependencies (task_id, prerequisite_id, created_at) VALUES (?, ?, ?)`,
		taskB.String(), f.spec.TaskID.String(), at)

	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		edges, err := wf.TaskDependencies().ByRun(t.Context(), f.spec.RunID)
		if err != nil {
			t.Fatalf("TaskDependencies().ByRun() = %v", err)
		}
		if len(edges) != 1 || edges[0].TaskID != taskB || edges[0].PrerequisiteID != f.spec.TaskID {
			t.Fatalf("edges = %+v, want the one persisted edge", edges)
		}
		task, _, err := uow.Tasks().Get(t.Context(), taskB)
		if err != nil {
			t.Fatalf("get dependent task: %v", err)
		}
		if !task.HasDependencies {
			t.Fatal("dependent task's HasDependencies = false, want the edge set as ground truth")
		}
	})
}

// TestAttemptIndex proves reservation-order listing, creation under the
// fence, and the cross-run refusal.
func TestAttemptIndex(t *testing.T) {
	f := newFeatureFixture(t)
	taskB := f.createFeatureTask(t, 7121, 3, run.TaskReady)
	now := f.clock.Now()

	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		attempt, attemptErr := run.NewAttempt(identity.AttemptID(uid(7122)), taskB, 1, now)
		if attemptErr != nil {
			t.Fatalf("new attempt: %v", attemptErr)
		}
		if _, err := wf.AttemptIndex().Create(t.Context(), attempt); err != nil {
			t.Fatalf("AttemptIndex().Create() = %v", err)
		}
		attempts, err := wf.AttemptIndex().ByTask(t.Context(), taskB)
		if err != nil {
			t.Fatalf("AttemptIndex().ByTask() = %v", err)
		}
		if len(attempts) != 1 || attempts[0].Number != 1 || attempts[0].State != run.AttemptReserved {
			t.Fatalf("attempts = %+v, want the one reserved attempt", attempts)
		}
	})

	// A second store's run cannot have attempts created through this run's
	// lease: the fence resolves the task's persisted owning run.
	other := newSpec("/repos/other", 2*specStride, f.clock.Now())
	if _, _, err := f.store.InitializeRun(t.Context(), other); err != nil {
		t.Fatalf("InitializeRun other: %v", err)
	}
	uow, err := f.store.Begin(t.Context(), f.lease)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer uow.Rollback() //nolint:errcheck // rollback of a deliberately-fenced unit of work.
	wf := workflowRepos(t, uow)
	attempt, err := run.NewAttempt(identity.AttemptID(uid(7123)), other.TaskID, 2, now)
	if err != nil {
		t.Fatalf("new attempt: %v", err)
	}
	if _, err := wf.AttemptIndex().Create(t.Context(), attempt); !errors.Is(err, app.ErrFenced) {
		t.Fatalf("cross-run AttemptIndex().Create() = %v, want ErrFenced", err)
	}
}

// TestSessionIndexByRun lists every session of the run — the bootstrap
// worker, the manager and a delegated child — with roles, parents and
// NULL-attempt shapes intact.
func TestSessionIndexByRun(t *testing.T) {
	f := newFeatureFixture(t)
	taskB := f.createFeatureTask(t, 7131, 2, run.TaskActive)
	childID, childIncarnation := f.createWorkerSession(t, taskB, run.RoleImplementer, 7132)
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM runtime_bindings WHERE session_id = ? AND incarnation_id = ?`, childID.String(), childIncarnation.String()); n != 1 {
		t.Fatalf("child binding rows = %d, want the fixture's bound incarnation", n)
	}

	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		sessions, err := wf.SessionIndex().ByRun(t.Context(), f.spec.RunID)
		if err != nil {
			t.Fatalf("SessionIndex().ByRun() = %v", err)
		}
		byID := map[identity.SessionID]run.Session{}
		for _, s := range sessions {
			byID[s.ID] = s
		}
		if len(sessions) != 3 {
			t.Fatalf("sessions = %d rows, want bootstrap worker + manager + child", len(sessions))
		}
		manager := byID[f.ManagerID]
		if manager.Role != run.RoleManager || manager.AttemptID != "" || manager.ParentSessionID != nil {
			t.Fatalf("manager row = %+v, want attempt-less parent-less manager", manager)
		}
		child := byID[childID]
		if child.Role != run.RoleImplementer || child.ParentSessionID == nil || *child.ParentSessionID != f.ManagerID {
			t.Fatalf("child row = %+v, want the manager as parent", child)
		}
	})
}

// TestManagerSession proves the current-manager resolution: found while
// non-terminal, ErrNotFound before creation and again once terminated (a
// historical relaunched-away row must not shadow a live one — resolution
// is by the partial-unique-index predicate, never role scan order).
func TestManagerSession(t *testing.T) {
	f := newFeatureFixture(t)
	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		manager, _, err := wf.ManagerSession(t.Context(), f.spec.RunID)
		if err != nil {
			t.Fatalf("ManagerSession() = %v", err)
		}
		if manager.ID != f.ManagerID {
			t.Fatalf("ManagerSession() = %s, want %s", manager.ID, f.ManagerID)
		}
	})

	// Terminate the manager: the run has no current manager again.
	now := f.clock.Now()
	f.inUOW(t, func(uow app.UnitOfWork) {
		saveSession(t, uow, f.ManagerID, func(v run.Session) (run.Session, error) { return v.Reconcile(now) })
		saveSession(t, uow, f.ManagerID, func(v run.Session) (run.Session, error) { return v.Terminate(now) })
	})
	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		if _, _, err := wf.ManagerSession(t.Context(), f.spec.RunID); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("ManagerSession() after termination = %v, want ErrNotFound", err)
		}
	})

	// A successor manager resolves; the historical row never shadows it.
	successorID := identity.SessionID(uid(7141))
	f.inUOW(t, func(uow app.UnitOfWork) {
		successor := run.NewManagerSession(successorID, f.spec.RunID, run.HarnessClaude, now)
		if _, err := uow.Sessions().Create(t.Context(), successor); err != nil {
			t.Fatalf("create successor manager: %v", err)
		}
	})
	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		manager, _, err := wf.ManagerSession(t.Context(), f.spec.RunID)
		if err != nil || manager.ID != successorID {
			t.Fatalf("ManagerSession() = %s, %v; want the successor", manager.ID, err)
		}
	})
}

// TestManagerUniquenessRaced races two handles creating a manager session
// for the same run: the sessions_one_manager_per_run partial unique index
// admits exactly one.
func TestManagerUniquenessRaced(t *testing.T) {
	clock := newFakeClock()
	root := t.TempDir()
	storeA := openStoreAt(t, root, clock)
	storeB := openStoreAt(t, root, clock)
	spec := newSpec("/repos/raced", specStride, clock.Now())
	spec.Snapshot.Workflow = featureWorkflow()
	_, lease, err := storeA.InitializeRun(t.Context(), spec)
	if err != nil {
		t.Fatalf("InitializeRun: %v", err)
	}
	now := clock.Now()
	create := func(store *sqlite.Store, n int) error {
		uow, err := store.Begin(t.Context(), lease)
		if err != nil {
			return err
		}
		defer uow.Rollback() //nolint:errcheck // rollback after commit is a documented no-op.
		manager := run.NewManagerSession(identity.SessionID(uid(n)), spec.RunID, run.HarnessClaude, now)
		if _, err := uow.Sessions().Create(t.Context(), manager); err != nil {
			return err
		}
		return uow.Commit()
	}
	var (
		start sync.WaitGroup
		done  sync.WaitGroup
	)
	start.Add(1)
	errs := make([]error, 2)
	done.Add(2)
	go func() { defer done.Done(); start.Wait(); errs[0] = create(storeA, 7151) }()
	go func() { defer done.Done(); start.Wait(); errs[1] = create(storeB, 7152) }()
	start.Done()
	done.Wait()

	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("raced manager creations succeeded %d times, want exactly 1 (errors: %v)", succeeded, errs)
	}
	if n := countRows(t, storeA, `SELECT COUNT(*) FROM sessions WHERE run_id = ? AND role = 'manager'`, spec.RunID.String()); n != 1 {
		t.Fatalf("manager sessions after the race = %d, want 1", n)
	}
}

// TestMessageRepository proves the controller-side envelope journal: the
// store assigns the per-(run, recipient) enqueue sequence in commit order
// — an inverted caller timestamp cannot jump the queue — Get and
// ByAddress reconstruct state, and PendingByAddress reads the sorted
// unacknowledged set inside the caller's transaction.
func TestMessageRepository(t *testing.T) {
	f := newFeatureFixture(t)
	now := f.clock.Now()
	first := identity.MessageID(uid(7161))
	second := identity.MessageID(uid(7162))

	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		// The second-committed message carries an EARLIER caller timestamp;
		// its enqueue sequence is still later — commit order, never clocks.
		m1 := run.NewInfo(first, f.spec.RunID, run.ControllerPrincipal(), run.ManagerAddress(), "", "/state/m1.md", "digest-1", 2, 0, now)
		created1, err := wf.Messages().Create(t.Context(), m1)
		if err != nil {
			t.Fatalf("Messages().Create(first) = %v", err)
		}
		m2 := run.NewInfo(second, f.spec.RunID, run.ControllerPrincipal(), run.ManagerAddress(), "", "/state/m2.md", "digest-2", 2, 0, now.Add(-time.Hour))
		created2, err := wf.Messages().Create(t.Context(), m2)
		if err != nil {
			t.Fatalf("Messages().Create(second) = %v", err)
		}
		if created1.EnqueueSeq != 1 || created2.EnqueueSeq != 2 {
			t.Fatalf("enqueue sequences = %d, %d; want 1, 2 in commit order", created1.EnqueueSeq, created2.EnqueueSeq)
		}
	})

	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		got, err := wf.Messages().Get(t.Context(), first)
		if err != nil {
			t.Fatalf("Messages().Get() = %v", err)
		}
		if got.State != run.MessageQueued || got.Sender.Kind != run.PrincipalController || !got.Recipient.Equal(run.ManagerAddress()) {
			t.Fatalf("Get() = %+v, want a queued controller info to the manager", got)
		}
		byAddress, err := wf.Messages().ByAddress(t.Context(), f.spec.RunID, run.ManagerAddress())
		if err != nil {
			t.Fatalf("Messages().ByAddress() = %v", err)
		}
		if len(byAddress) != 2 || byAddress[0].ID != first || byAddress[1].ID != second {
			t.Fatalf("ByAddress order = %+v, want enqueue order", byAddress)
		}
		pending, err := wf.Messages().PendingByAddress(t.Context(), f.spec.RunID, run.ManagerAddress())
		if err != nil {
			t.Fatalf("Messages().PendingByAddress() = %v", err)
		}
		if len(pending) != 2 {
			t.Fatalf("pending = %v, want both unacknowledged ids", pending)
		}
		if !sortedMessageIDs(pending) {
			t.Fatalf("pending ids are not sorted: %v", pending)
		}
	})

	// An acknowledged message leaves the pending set.
	rawExec(t, f.store, `INSERT INTO message_acks (message_id, session_id, incarnation_id, acked_at) VALUES (?, ?, ?, ?)`,
		first.String(), f.ManagerID.String(), f.ManagerIncarnation.String(), "2026-09-14T11:00:00.000000000Z")
	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		pending, err := wf.Messages().PendingByAddress(t.Context(), f.spec.RunID, run.ManagerAddress())
		if err != nil {
			t.Fatalf("PendingByAddress() = %v", err)
		}
		if len(pending) != 1 || pending[0] != second {
			t.Fatalf("pending after ack = %v, want only the second message", pending)
		}
		got, err := wf.Messages().Get(t.Context(), first)
		if err != nil || got.State != run.MessageAcknowledged {
			t.Fatalf("Get(acked) state = %s, %v; want acknowledged", got.State, err)
		}
	})
}

// sortedMessageIDs reports whether ids ascend lexically.
func sortedMessageIDs(ids []identity.MessageID) bool {
	for i := 1; i < len(ids); i++ {
		if string(ids[i-1]) > string(ids[i]) {
			return false
		}
	}
	return true
}

// TestReviewRepository reads accepted verdicts: ByAttempt resolves the one
// row, Latest picks the newest by submission time, and both report the
// documented nil for none.
func TestReviewRepository(t *testing.T) {
	f := newFeatureFixture(t)
	taskB := f.createFeatureTask(t, 7171, 2, run.TaskActive)
	_, _ = f.createWorkerSession(t, taskB, run.RoleReviewer, 7172)
	attemptID := identity.AttemptID(uid(7172))

	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		review, err := wf.Reviews().ByAttempt(t.Context(), attemptID)
		if err != nil || review != nil {
			t.Fatalf("ByAttempt(no review) = %v, %v; want nil, nil", review, err)
		}
		latest, err := wf.Reviews().Latest(t.Context(), f.spec.RunID)
		if err != nil || latest != nil {
			t.Fatalf("Latest(no review) = %v, %v; want nil, nil", latest, err)
		}
	})

	rawExec(t, f.store, `INSERT INTO reviews (id, run_id, task_id, attempt_id, subject_commit_oid, subject_tree_oid, verdict, reasons_path, reasons_digest, submitted_at)
		VALUES (?, ?, ?, ?, 'commit-head', 'tree-head', 'approve', '/state/reasons.md', 'reasons-digest', '2026-09-14T12:00:00.000000000Z')`,
		uid(7173), f.spec.RunID.String(), taskB.String(), attemptID.String())

	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		review, err := wf.Reviews().ByAttempt(t.Context(), attemptID)
		if err != nil || review == nil {
			t.Fatalf("ByAttempt() = %v, %v", review, err)
		}
		if review.Verdict != run.VerdictApprove || review.SubjectCommitOID != "commit-head" || review.TaskID != taskB {
			t.Fatalf("ByAttempt() = %+v, want the accepted approve", review)
		}
		latest, err := wf.Reviews().Latest(t.Context(), f.spec.RunID)
		if err != nil || latest == nil || latest.ID != review.ID {
			t.Fatalf("Latest() = %+v, %v; want the accepted review", latest, err)
		}
	})
}

// TestIntegrationRepository proves the serial merge journal: create, get,
// revision-checked save, the Current slot, and ByTask newest-first.
func TestIntegrationRepository(t *testing.T) {
	f := newFeatureFixture(t)
	resultID := seedAcceptedResult(t, f, 7181)
	integrationID := identity.IntegrationID(uid(7182))
	now := f.clock.Now()

	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		integration := run.NewIntegration(integrationID, f.spec.RunID, f.spec.TaskID, resultID, "source-oid", "premerge-oid", now)
		if _, err := wf.Integrations().Create(t.Context(), integration); err != nil {
			t.Fatalf("Integrations().Create() = %v", err)
		}
		current, revision, ok, err := wf.Integrations().Current(t.Context(), f.spec.RunID)
		if err != nil || !ok {
			t.Fatalf("Current() = %v (found %t)", err, ok)
		}
		if current.ID != integrationID || revision != 1 || current.State != run.IntegrationMerging {
			t.Fatalf("Current() = %+v rev %d, want the merging integration at revision 1", current, revision)
		}
	})

	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		integration, revision, err := wf.Integrations().Get(t.Context(), integrationID)
		if err != nil {
			t.Fatalf("Get() = %v", err)
		}
		checking, err := integration.EnterChecking("merge-oid", f.clock.Now())
		if err != nil {
			t.Fatalf("EnterChecking: %v", err)
		}
		next, err := wf.Integrations().Save(t.Context(), checking, revision)
		if err != nil || next != revision+1 {
			t.Fatalf("Save() = %d, %v; want revision advanced", next, err)
		}
		// A stale save is a revision conflict.
		if _, err := wf.Integrations().Save(t.Context(), checking, revision); !errors.Is(err, app.ErrRevisionConflict) {
			t.Fatalf("stale Save() = %v, want ErrRevisionConflict", err)
		}
	})

	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		integration, revision, err := wf.Integrations().Get(t.Context(), integrationID)
		if err != nil {
			t.Fatalf("Get() = %v", err)
		}
		if integration.MergeCommitOID != "merge-oid" || integration.State != run.IntegrationChecking {
			t.Fatalf("Get() after save = %+v, want checking with the merge oid", integration)
		}
		integrated, err := integration.Integrate(f.clock.Now())
		if err != nil {
			t.Fatalf("Integrate: %v", err)
		}
		if _, err := wf.Integrations().Save(t.Context(), integrated, revision); err != nil {
			t.Fatalf("Save(integrated) = %v", err)
		}
	})

	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		if _, _, ok, err := wf.Integrations().Current(t.Context(), f.spec.RunID); err != nil || ok {
			t.Fatalf("Current() after integration = found %t, %v; want the slot free", ok, err)
		}
		byTask, err := wf.Integrations().ByTask(t.Context(), f.spec.TaskID)
		if err != nil || len(byTask) != 1 || byTask[0].ID != integrationID {
			t.Fatalf("ByTask() = %+v, %v; want the one integration", byTask, err)
		}
	})
}

// TestSerialIntegrationIndexRaced races two handles claiming the run's
// serial integration slot: the partial unique index admits exactly one
// non-terminal integration per run.
func TestSerialIntegrationIndexRaced(t *testing.T) {
	clock := newFakeClock()
	root := t.TempDir()
	storeA := openStoreAt(t, root, clock)
	storeB := openStoreAt(t, root, clock)
	spec := newSpec("/repos/serial", specStride, clock.Now())
	spec.Snapshot.Workflow = featureWorkflow()
	_, lease, err := storeA.InitializeRun(t.Context(), spec)
	if err != nil {
		t.Fatalf("InitializeRun: %v", err)
	}
	f := &fixture{store: storeA, clock: clock, spec: spec, lease: lease}
	ff := &featureFixture{fixture: f}
	resultA := seedAcceptedResult(t, ff, 7191)
	// A second (unaccepted) result row gives the loser a distinct
	// (task, result) pair, so only the serial-slot index can decide.
	resultB := identity.ResultID(uid(7193))
	rawExec(t, storeA, `INSERT INTO results (id, attempt_id, commit_oid, summary, content_digest, accepted, submitted_at)
		VALUES (?, ?, 'commit-b', 'second candidate', 'digest-b', 0, '2026-09-14T12:00:00.000000000Z')`,
		resultB.String(), spec.AttemptID.String())

	now := clock.Now()
	create := func(store *sqlite.Store, integrationN int, result identity.ResultID) error {
		uow, err := store.Begin(t.Context(), lease)
		if err != nil {
			return err
		}
		defer uow.Rollback() //nolint:errcheck // rollback after commit is a documented no-op.
		wf, err := app.RequireWorkflowRepositories(uow, "raced create")
		if err != nil {
			return err
		}
		integration := run.NewIntegration(identity.IntegrationID(uid(integrationN)), spec.RunID, spec.TaskID, result, "source-oid", "premerge-oid", now)
		if _, err := wf.Integrations().Create(t.Context(), integration); err != nil {
			return err
		}
		return uow.Commit()
	}
	var (
		start sync.WaitGroup
		done  sync.WaitGroup
	)
	start.Add(1)
	errs := make([]error, 2)
	done.Add(2)
	go func() { defer done.Done(); start.Wait(); errs[0] = create(storeA, 7194, resultA) }()
	go func() { defer done.Done(); start.Wait(); errs[1] = create(storeB, 7195, resultB) }()
	start.Done()
	done.Wait()

	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("raced serial-slot creations succeeded %d times, want exactly 1 (errors: %v)", succeeded, errs)
	}
	if n := countRows(t, storeA, `SELECT COUNT(*) FROM integrations WHERE run_id = ?`, spec.RunID.String()); n != 1 {
		t.Fatalf("integrations after the race = %d, want 1", n)
	}
}

// TestRetryRequestRepository reads pending bookkeeping rows oldest first
// and consumes exactly the pending one.
func TestRetryRequestRepository(t *testing.T) {
	f := newFeatureFixture(t)
	taskB := f.createFeatureTask(t, 7201, 2, run.TaskNeedsRework)
	rawExec(t, f.store, `INSERT INTO retry_requests (id, task_id, requested_by_session, request_id, reason, state, created_at)
		VALUES (?, ?, ?, 'req-1', 'flaky check', 'pending', '2026-09-14T12:00:00.000000000Z')`,
		uid(7202), taskB.String(), f.ManagerID.String())

	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		pending, err := wf.RetryRequests().Pending(t.Context(), f.spec.RunID)
		if err != nil {
			t.Fatalf("RetryRequests().Pending() = %v", err)
		}
		if len(pending) != 1 || pending[0].TaskID != taskB || pending[0].RequestedBy != f.ManagerID ||
			pending[0].Reason != "flaky check" || pending[0].RequestID != "req-1" {
			t.Fatalf("pending = %+v, want the one recorded request", pending)
		}
		if err := wf.RetryRequests().MarkConsumed(t.Context(), taskB, 2); err != nil {
			t.Fatalf("MarkConsumed() = %v", err)
		}
	})
	f.inUOW(t, func(uow app.UnitOfWork) {
		wf := workflowRepos(t, uow)
		pending, err := wf.RetryRequests().Pending(t.Context(), f.spec.RunID)
		if err != nil || len(pending) != 0 {
			t.Fatalf("Pending() after consumption = %+v, %v; want none", pending, err)
		}
		// A second consumption has nothing pending to consume.
		if err := wf.RetryRequests().MarkConsumed(t.Context(), taskB, 2); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("MarkConsumed() again = %v, want ErrNotFound", err)
		}
	})
}

// seedAcceptedResult drives the fixture's bootstrap attempt to an accepted
// result through the ordinary worker-authority path and returns its id.
// Unlike the Phase 2 runningFixture it never transitions the RUN — a
// feature fixture's run is already running (the manager's settled launch),
// only the bootstrap task/attempt/session move.
func seedAcceptedResult(t *testing.T, f *featureFixture, resultN int) identity.ResultID {
	t.Helper()
	now := f.clock.Now()
	f.inUOW(t, func(uow app.UnitOfWork) {
		saveTask(t, uow, f.spec.TaskID, func(v run.Task) (run.Task, error) { return v.Activate(now) })
		saveAttempt(t, uow, f.spec.AttemptID, func(v run.Attempt) (run.Attempt, error) { return v.Launch(now) })
		saveSession(t, uow, f.spec.SessionID, func(v run.Session) (run.Session, error) { return v.Launch(now) })
	})
	f.createBinding(t)
	f.claimLaunch(t)
	f.settleClaimExeced(t)
	f.inUOW(t, func(uow app.UnitOfWork) {
		saveAttempt(t, uow, f.spec.AttemptID, func(v run.Attempt) (run.Attempt, error) { return v.MarkRunning(now) })
		saveSession(t, uow, f.spec.SessionID, func(v run.Session) (run.Session, error) { return v.ConfirmActive(now) })
	})
	submission := f.submission(resultN, "digest-"+uid(resultN))
	outcome, err := f.store.SubmitResult(t.Context(), submission)
	if err != nil || outcome.Kind != app.SubmissionAccepted {
		t.Fatalf("SubmitResult() = %+v, %v; want accepted", outcome, err)
	}
	return submission.ID
}
