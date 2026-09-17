package sqlite_test

import (
	"errors"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// TestWorkflowReadStoreCapability proves RequireWorkflowReadStore succeeds
// against the real store.
func TestWorkflowReadStoreCapability(t *testing.T) {
	f := newFeatureFixture(t)
	if _, err := app.RequireWorkflowReadStore(f.store, "capability probe"); err != nil {
		t.Fatalf("RequireWorkflowReadStore() = %v, want the sqlite store to implement it", err)
	}
}

// TestLoadSessionLaunchContext drives the session-addressed launch
// context: a bound worker resolves its binding's incarnation with the
// attempt row and worktree path attached; a pre-binding manager resolves
// its own pending intent with a zero attempt; a successor session carrying
// a predecessor's native reference reports Relaunch; and every ambiguous
// identity fails closed.
func TestLoadSessionLaunchContext(t *testing.T) {
	f := newFeatureFixture(t)
	taskB := f.createFeatureTask(t, 7901, 2, run.TaskActive)
	workerID, workerIncarnation := f.createWorkerSession(t, taskB, run.RoleImplementer, 7902)
	workerAttempt := identity.AttemptID(uid(7902))
	f.createAttemptWorktree(t, 7905, workerAttempt, "/wt/attempt-b", "hop/r1/t2")

	t.Run("bound worker resolves binding, attempt and worktree", func(t *testing.T) {
		out, err := f.store.LoadSessionLaunchContext(t.Context(), f.spec.RunID, workerID)
		if err != nil {
			t.Fatalf("LoadSessionLaunchContext() = %v", err)
		}
		if out.IncarnationID != workerIncarnation || out.Session.ID != workerID || out.AttemptID != workerAttempt {
			t.Fatalf("context = %+v, want the binding's incarnation and the session's attempt", out)
		}
		if out.Attempt.ID != workerAttempt || out.Attempt.TaskID != taskB {
			t.Fatalf("attempt row = %+v, want the bound attempt", out.Attempt)
		}
		if out.WorktreePath != "/wt/attempt-b" {
			t.Fatalf("worktree path = %q, want the attempt-linked row", out.WorktreePath)
		}
		if out.Relaunch {
			t.Fatal("a first launch reported Relaunch")
		}
		if !out.Snapshot.Workflow.Feature() {
			t.Fatal("snapshot lost its workflow policy on the session-addressed load")
		}
	})

	t.Run("manager resolves the session-keyed pending intent", func(t *testing.T) {
		// The fixture manager has a binding; a second run's fresh manager
		// exercises the pre-binding window through claims_phase3_test's
		// fixture instead. Here: the manager's own binding decides.
		out, err := f.store.LoadSessionLaunchContext(t.Context(), f.spec.RunID, f.ManagerID)
		if err != nil {
			t.Fatalf("LoadSessionLaunchContext(manager) = %v", err)
		}
		if out.IncarnationID != f.ManagerIncarnation || out.AttemptID != "" || out.Attempt.ID != "" || out.WorktreePath != "" {
			t.Fatalf("manager context = %+v, want the attempt-less shape", out)
		}
	})

	t.Run("successor session reports Relaunch", func(t *testing.T) {
		now := f.clock.Now()
		successorID := identity.SessionID(uid(7906))
		successorIncarnation := identity.IncarnationID(uid(7907))
		manager := f.managerSessionValue(t)
		const nativeRef = "native-ref-lineage"
		f.inUOW(t, func(uow app.UnitOfWork) {
			// Give the predecessor its durable native reference, retire it,
			// and create the successor bound to the SAME reference — the
			// cold-relaunch shape.
			saveSession(t, uow, workerID, func(v run.Session) (run.Session, error) {
				return v.AssignNativeRef(nativeRef, run.NativeRefAssigned, now)
			})
			saveSession(t, uow, workerID, func(v run.Session) (run.Session, error) { return v.Reconcile(now) })
			saveSession(t, uow, workerID, func(v run.Session) (run.Session, error) { return v.Terminate(now) })
			successor, err := run.NewChildSession(successorID, f.spec.RunID, workerAttempt, run.RoleImplementer, manager, run.HarnessClaude, now)
			if err != nil {
				t.Fatalf("new successor: %v", err)
			}
			if successor, err = successor.AssignNativeRef(nativeRef, run.NativeRefAssigned, now); err != nil {
				t.Fatalf("assign native ref: %v", err)
			}
			if _, err := uow.Sessions().Create(t.Context(), successor); err != nil {
				t.Fatalf("create successor: %v", err)
			}
			binding := run.NewRuntimeBinding(successorID, successorIncarnation,
				"/tmp/herdr.sock", "server-instance-1", "workspace-w", "tab-w", "pane-s", uid(7908), run.LaunchResume, now)
			if err := uow.Bindings().Create(t.Context(), binding); err != nil {
				t.Fatalf("create successor binding: %v", err)
			}
		})
		out, err := f.store.LoadSessionLaunchContext(t.Context(), f.spec.RunID, successorID)
		if err != nil {
			t.Fatalf("LoadSessionLaunchContext(successor) = %v", err)
		}
		if !out.Relaunch {
			t.Fatal("a successor carrying the predecessor's native reference did not report Relaunch")
		}
		if out.IncarnationID != successorIncarnation {
			t.Fatalf("successor incarnation = %s, want its own binding's", out.IncarnationID)
		}
	})

	t.Run("unlinked rows: one is the solo fallback, several resolve nothing", func(t *testing.T) {
		g := newFeatureFixture(t)
		task := g.createFeatureTask(t, 7951, 2, run.TaskActive)
		session, _ := g.createWorkerSession(t, task, run.RoleImplementer, 7952)
		load := func() string {
			t.Helper()
			out, err := g.store.LoadSessionLaunchContext(t.Context(), g.spec.RunID, session)
			if err != nil {
				t.Fatalf("LoadSessionLaunchContext() = %v", err)
			}
			return out.WorktreePath
		}
		if got := load(); got != "" {
			t.Fatalf("worktree path before any row = %q, want empty", got)
		}
		repositoryID := g.repositoryID(t)
		g.createWorktree(t, run.NewWorktree(identity.WorktreeID(uid(7956)), repositoryID, g.spec.RunID, "/wt/solo-one", "hop/run-1"))
		if got := load(); got != "/wt/solo-one" {
			t.Fatalf("worktree path with the run's single unlinked row = %q, want that row", got)
		}
		g.createWorktree(t, run.NewWorktree(identity.WorktreeID(uid(7957)), repositoryID, g.spec.RunID, "/wt/solo-two", "hop/run-2"))
		if got := load(); got != "" {
			t.Fatalf("worktree path among several unlinked rows = %q, want no guess", got)
		}
		g.createAttemptWorktree(t, 7958, identity.AttemptID(uid(7952)), "/wt/linked", "hop/r1/t2a1")
		if got := load(); got != "/wt/linked" {
			t.Fatalf("worktree path with a linked row among unlinked ones = %q, want the linked row", got)
		}
	})

	t.Run("fail-closed identities", func(t *testing.T) {
		// A session of another run is not found.
		other := newSpec("/repos/other", 2*specStride, f.clock.Now())
		if _, _, err := f.store.InitializeRun(t.Context(), other); err != nil {
			t.Fatalf("InitializeRun other: %v", err)
		}
		if _, err := f.store.LoadSessionLaunchContext(t.Context(), f.spec.RunID, other.SessionID); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("cross-run session load = %v, want ErrNotFound", err)
		}
		// A session with neither a binding nor a pending intent has no
		// launch identity.
		taskD := f.createFeatureTask(t, 7911, 4, run.TaskActive)
		bare, _ := pendingChildSession(t, f, taskD, 7912)
		if _, err := f.store.LoadSessionLaunchContext(t.Context(), f.spec.RunID, bare); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("no-source load = %v, want ErrNotFound", err)
		}
		// Its own pending intent resolves it...
		incarnation := identity.IncarnationID(uid(7915))
		createLaunchIntentFor(t, f, 7916, bare, incarnation)
		out, err := f.store.LoadSessionLaunchContext(t.Context(), f.spec.RunID, bare)
		if err != nil || out.IncarnationID != incarnation {
			t.Fatalf("intent-resolved load = %+v, %v; want the intent's incarnation", out, err)
		}
		// ...and a binding disagreeing with the still-pending intent fails
		// closed.
		f.inUOW(t, func(uow app.UnitOfWork) {
			binding := run.NewRuntimeBinding(bare, identity.IncarnationID(uid(7917)),
				"/tmp/herdr.sock", "server-instance-1", "workspace-w", "tab-w", "pane-d", uid(7918), run.LaunchInitial, f.clock.Now())
			if err := uow.Bindings().Create(t.Context(), binding); err != nil {
				t.Fatalf("create disagreeing binding: %v", err)
			}
		})
		if _, err := f.store.LoadSessionLaunchContext(t.Context(), f.spec.RunID, bare); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("binding/intent disagreement = %v, want ErrNotFound", err)
		}
	})
}

// TestLoadMessagingContext resolves the caller's OWN run, lineage address
// and current incarnation.
func TestLoadMessagingContext(t *testing.T) {
	f := newFeatureFixture(t)
	taskB := f.createFeatureTask(t, 7921, 2, run.TaskActive)
	workerID, workerIncarnation := f.createWorkerSession(t, taskB, run.RoleImplementer, 7922)

	manager, err := f.store.LoadMessagingContext(t.Context(), f.ManagerID)
	if err != nil || manager.RunID != f.spec.RunID || !manager.Address.Equal(run.ManagerAddress()) || manager.IncarnationID != f.ManagerIncarnation {
		t.Fatalf("LoadMessagingContext(manager) = %+v, %v", manager, err)
	}
	worker, err := f.store.LoadMessagingContext(t.Context(), workerID)
	if err != nil || !worker.Address.Equal(run.TaskAddress(taskB)) || worker.IncarnationID != workerIncarnation {
		t.Fatalf("LoadMessagingContext(worker) = %+v, %v; want the task address", worker, err)
	}
	// A Phase 2 solo worker has no logical messaging address.
	if _, err := f.store.LoadMessagingContext(t.Context(), f.spec.SessionID); err == nil {
		t.Fatal("LoadMessagingContext(solo worker) succeeded; want the no-address refusal")
	}
	if _, err := f.store.LoadMessagingContext(t.Context(), identity.SessionID(uid(9997))); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("LoadMessagingContext(unknown session) = %v, want ErrNotFound", err)
	}
}

// TestLoadMessageDetail reads the envelope with its full delivery/ack
// history — the re-serve included — and reports a cross-run id exactly
// like an unknown one.
func TestLoadMessageDetail(t *testing.T) {
	f := newMessagingFixture(t)
	send := f.workerSend(7931, run.MessageQuestion, "")
	if outcome, err := f.store.SendMessage(t.Context(), send); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("send: %+v, %v", outcome, err)
	}
	// Two serves (the second the at-least-once re-serve), then the ack.
	for i := range 2 {
		if _, served, err := f.store.FetchNextMessage(t.Context(), f.managerFetch()); err != nil || !served {
			t.Fatalf("fetch %d: %t, %v", i, served, err)
		}
		f.clock.Advance(time.Second)
	}
	if outcome, err := f.store.AckMessage(t.Context(), app.MessageAck{RunID: f.spec.RunID, MessageID: send.ID, SessionID: f.ManagerID, IncarnationID: f.ManagerIncarnation}); err != nil || outcome.Kind != app.AckAccepted {
		t.Fatalf("ack: %+v, %v", outcome, err)
	}

	detail, err := f.store.LoadMessageDetail(t.Context(), f.spec.RunID, send.ID)
	if err != nil {
		t.Fatalf("LoadMessageDetail() = %v", err)
	}
	if detail.Message.ID != send.ID || detail.Message.State != run.MessageAcknowledged {
		t.Fatalf("detail envelope = %+v, want the acknowledged question", detail.Message)
	}
	if len(detail.Deliveries) != 2 || detail.Deliveries[0].SessionID != f.ManagerID || detail.Deliveries[1].IncarnationID != f.ManagerIncarnation {
		t.Fatalf("deliveries = %+v, want both serves oldest first", detail.Deliveries)
	}
	if !detail.Deliveries[1].At.After(detail.Deliveries[0].At) {
		t.Fatalf("deliveries out of order: %v then %v", detail.Deliveries[0].At, detail.Deliveries[1].At)
	}
	if detail.Ack == nil || detail.Ack.SessionID != f.ManagerID {
		t.Fatalf("ack = %+v, want the manager's acknowledgement", detail.Ack)
	}

	if _, err := f.store.LoadMessageDetail(t.Context(), f.spec.RunID, identity.MessageID(uid(9998))); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("unknown message = %v, want ErrNotFound", err)
	}
	// A message of ANOTHER run is reported exactly like an unknown one.
	other := newSpec("/repos/other", 2*specStride, f.clock.Now())
	if _, _, err := f.store.InitializeRun(t.Context(), other); err != nil {
		t.Fatalf("InitializeRun other: %v", err)
	}
	if _, err := f.store.LoadMessageDetail(t.Context(), other.RunID, send.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("cross-run message = %v, want ErrNotFound", err)
	}
}

// TestRunDetailFeatureExtensions drives the Phase 3 status surface: the
// task table with dependencies, attempt counts and per-attempt worktrees,
// the latest integration, guard shortfalls, the mailbox surface with the
// attention threshold, and pending human questions — plus the solo run
// rendering every extension at its zero value.
func TestRunDetailFeatureExtensions(t *testing.T) {
	f := newMessagingFixture(t)
	// A second, dependent task.
	taskC := f.createFeatureTask(t, 7941, 3, run.TaskPending)
	rawExec(t, f.store, `INSERT INTO task_dependencies (task_id, prerequisite_id, created_at) VALUES (?, ?, '2026-09-14T10:00:00.000000000Z')`,
		taskC.String(), f.TaskB.String())
	// A worktree row linked to the worker's attempt.
	f.createAttemptWorktree(t, 7942, identity.AttemptID(uid(7302)), "/wt/t2-a1", "hop/r1/t2")
	// An integration row for the worker task.
	resultID := uid(7943)
	rawExec(t, f.store, `INSERT INTO results (id, attempt_id, commit_oid, summary, content_digest, accepted, submitted_at)
		VALUES (?, ?, 'source-oid', 'done', 'digest-r', 1, '2026-09-14T10:00:00.000000000Z')`, resultID, uid(7302))
	rawExec(t, f.store, `INSERT INTO integrations (id, run_id, task_id, result_id, source_commit_oid, premerge_head_oid, merge_commit_oid, state, operation_id, revision, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'source-oid', 'premerge-oid', 'merge-oid', 'integrated', NULL, 2, '2026-09-14T10:00:00.000000000Z', '2026-09-14T10:05:00.000000000Z')`,
		uid(7944), f.spec.RunID.String(), f.TaskB.String(), resultID)

	// A worker question sits QUEUED at the manager, and a manager question
	// to the human stays unanswered.
	workerQuestion := f.workerSend(7945, run.MessageQuestion, "")
	if outcome, err := f.store.SendMessage(t.Context(), workerQuestion); err != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("worker question: %+v, %v", outcome, err)
	}
	humanQuestion := seedHumanQuestion(t, f.store, f, 7946)

	// Age everything past the frozen 2m attention threshold.
	f.clock.Advance(3 * time.Minute)

	detail, err := f.store.LoadRunStatus(t.Context(), f.spec.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus() = %v", err)
	}
	if detail.TaskID != "" || detail.AttemptID != "" {
		t.Fatalf("feature detail carries solo task identities: %+v", detail.RunStatus)
	}
	if detail.Mode != "feature" {
		t.Fatalf("Mode = %q, want %q", detail.Mode, "feature")
	}
	if detail.SessionID != f.ManagerID || detail.Binding == nil || detail.Binding.IncarnationID != f.ManagerIncarnation {
		t.Fatalf("feature detail session = %s binding %+v, want the current manager", detail.SessionID, detail.Binding)
	}

	// Sessions lists every session the run has ever created: the legacy
	// solo-shaped bootstrap session initLegacyFeatureRun's promoted run
	// still carries (reserved, never bound — the same "bootstrap" row the
	// task-table assertion below counts), the manager (no task or
	// attempt), and the implementer bound to TaskB's first attempt, each
	// of the latter two with its current binding.
	if len(detail.Sessions) != 3 {
		t.Fatalf("Sessions = %+v, want the bootstrap, manager and implementer sessions", detail.Sessions)
	}
	byRole := map[run.Role]app.SessionSummary{}
	for _, s := range detail.Sessions {
		byRole[s.Role] = s
	}
	bootstrap := byRole[run.RoleWorker]
	if bootstrap.State != run.SessionReserved || bootstrap.TaskID == "" || bootstrap.AttemptNumber != 1 || bootstrap.Binding != nil {
		t.Fatalf("bootstrap session summary = %+v", bootstrap)
	}
	manager := byRole[run.RoleManager]
	if manager.SessionID != f.ManagerID || manager.TaskID != "" || manager.AttemptNumber != 0 ||
		manager.Binding == nil || manager.Binding.IncarnationID != f.ManagerIncarnation {
		t.Fatalf("manager session summary = %+v", manager)
	}
	worker := byRole[run.RoleImplementer]
	if worker.SessionID != f.WorkerID || worker.TaskID != f.TaskB || worker.AttemptNumber != 1 ||
		worker.Binding == nil || worker.Binding.IncarnationID != f.WorkerIncarnation {
		t.Fatalf("implementer session summary = %+v", worker)
	}

	if len(detail.Tasks) != 3 {
		t.Fatalf("task table = %+v, want bootstrap + worker task + dependent", detail.Tasks)
	}
	byID := map[identity.TaskID]app.TaskSummary{}
	for _, row := range detail.Tasks {
		byID[row.TaskID] = row
	}
	workerRow := byID[f.TaskB]
	if workerRow.Seq != 2 || workerRow.AttemptCount != 1 || workerRow.WorktreePath != "/wt/t2-a1" {
		t.Fatalf("worker task row = %+v, want seq 2 with its attempt worktree", workerRow)
	}
	dependentRow := byID[taskC]
	if len(dependentRow.DependsOn) != 1 || dependentRow.DependsOn[0] != f.TaskB || dependentRow.AttemptCount != 0 || dependentRow.WorktreePath != "" {
		t.Fatalf("dependent task row = %+v, want the persisted edge and no attempts", dependentRow)
	}

	if detail.LatestIntegration == nil || detail.LatestIntegration.TaskID != f.TaskB ||
		detail.LatestIntegration.MergeCommitOID != "merge-oid" || detail.LatestIntegration.State != run.IntegrationIntegrated {
		t.Fatalf("latest integration = %+v", detail.LatestIntegration)
	}

	// Guard shortfalls: the plan is open, the dependent task and bootstrap
	// are not integrated, no check receipt is tracked, no verdict exists.
	kinds := map[run.ShortfallKind]int{}
	for _, shortfall := range detail.GuardShortfalls {
		kinds[shortfall.Kind]++
	}
	if kinds[run.ShortfallPlanOpen] != 1 || kinds[run.ShortfallTaskNotIntegrated] != 3 ||
		kinds[run.ShortfallCheckMissing] != 1 || kinds[run.ShortfallVerdictMissing] != 1 {
		t.Fatalf("guard shortfalls = %+v; want plan-open, all three implement tasks unintegrated, check-missing, verdict-missing", detail.GuardShortfalls)
	}

	// Mailboxes: the manager address holds the queued worker question (its
	// session live, over threshold → attention); the human address holds
	// the unanswered relay (no session to go stale, attention).
	if len(detail.Mailboxes) != 2 {
		t.Fatalf("mailboxes = %+v, want manager and human entries", detail.Mailboxes)
	}
	byAddress := map[string]app.MailboxStatus{}
	for _, mailbox := range detail.Mailboxes {
		byAddress[app.AddressString(mailbox.Address)] = mailbox
	}
	managerBox := byAddress["manager"]
	// Two queued worker questions: the direct one and the one the human
	// relay was built from.
	if managerBox.QueuedCount != 2 || managerBox.InFlight != nil || !managerBox.AddressLive || !managerBox.Attention {
		t.Fatalf("manager mailbox = %+v, want the queued questions needing attention", managerBox)
	}
	if managerBox.OldestQueuedAge < 3*time.Minute {
		t.Fatalf("manager queue age = %v, want the aged queue", managerBox.OldestQueuedAge)
	}
	humanBox := byAddress["human"]
	if humanBox.QueuedCount != 1 || !humanBox.AddressLive || !humanBox.Attention {
		t.Fatalf("human mailbox = %+v", humanBox)
	}

	// The bare listing carries the SAME attention condition ListRuns just
	// computed for this run (Astra F4): pinned from the identical aged
	// mailbox fixture the detail assertions above already used.
	statuses, err := f.store.ListRuns(t.Context(), "/repos/feature")
	if err != nil {
		t.Fatalf("ListRuns() = %v", err)
	}
	listingFound := false
	for _, s := range statuses {
		if s.RunID != f.spec.RunID {
			continue
		}
		listingFound = true
		if !s.NeedsAttention {
			t.Fatalf("listing NeedsAttention = false, want true (both mailboxes are in the Attention condition)")
		}
	}
	if !listingFound {
		t.Fatalf("ListRuns() = %+v, want the fixture run among them", statuses)
	}

	if len(detail.PendingQuestions) != 1 || detail.PendingQuestions[0].MessageID != humanQuestion {
		t.Fatalf("pending questions = %+v, want the unanswered relay", detail.PendingQuestions)
	}

	// Once answered, the pending question clears and the manager gains the
	// queued answer.
	if outcome, answerErr := f.store.AnswerQuestion(t.Context(), app.HumanAnswer{
		ID: identity.MessageID(uid(7948)), RunID: f.spec.RunID, QuestionID: humanQuestion,
		BodyPath: "/state/bodies/answer.md", BodyDigest: "digest-a", BodyBytes: 4,
	}); answerErr != nil || outcome.Kind != app.MessageAccepted {
		t.Fatalf("answer: %+v, %v", outcome, answerErr)
	}
	detail, err = f.store.LoadRunStatus(t.Context(), f.spec.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus(after answer) = %v", err)
	}
	if len(detail.PendingQuestions) != 0 {
		t.Fatalf("pending questions after answer = %+v, want none", detail.PendingQuestions)
	}
}

// TestRunDetailSoloZeroValues proves a solo run renders every Phase 3
// extension at its zero value while keeping the exact Phase 2 shape.
func TestRunDetailSoloZeroValues(t *testing.T) {
	f := runningFixture(t)
	detail, err := f.store.LoadRunStatus(t.Context(), f.spec.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus() = %v", err)
	}
	if detail.TaskID != f.spec.TaskID || detail.AttemptID != f.spec.AttemptID || detail.SessionID != f.spec.SessionID {
		t.Fatalf("solo identities = %+v, want the Phase 2 single-task shape", detail)
	}
	if detail.Mode != "" {
		t.Fatalf("Mode = %q, want \"\" for a solo run", detail.Mode)
	}
	if detail.Tasks != nil || detail.LatestIntegration != nil || detail.GuardShortfalls != nil ||
		detail.Mailboxes != nil || detail.PendingQuestions != nil || detail.Sessions != nil {
		t.Fatalf("solo run rendered feature-mode fields: %+v", detail)
	}
}

// TestRunDetailVerdictAgainstRecordedHeadTree drives the status guard's
// verdict shortfalls over real review rows whose tree id differs from
// their commit id, as git's objects do: the head's tree is the one HOP
// recorded for exactly the head commit (the review task's frozen subject,
// the accepted review's subject), never the merge commit id itself.
func TestRunDetailVerdictAgainstRecordedHeadTree(t *testing.T) {
	t.Run("a reject of the integrated head is verdict-rejected until a fix moves the head", func(t *testing.T) {
		f := newReviewFixture(t)
		seedIntegratedHeadRow(t, f.featureFixture, 7620, f.spec.TaskID, f.spec.AttemptID, "commit-head", "2026-09-14T10:05:00.000000000Z")
		reject := f.submission(7611, run.VerdictReject)
		if outcome, err := f.store.SubmitReview(t.Context(), reject); err != nil || outcome.Kind != app.ReviewAccepted {
			t.Fatalf("SubmitReview(reject) = %+v, %v", outcome, err)
		}

		shortfalls := loadShortfalls(t, f.featureFixture)
		if countKind(shortfalls, run.ShortfallVerdictStaleSubject) != 0 || countKind(shortfalls, run.ShortfallVerdictRejected) != 1 {
			t.Fatalf("guard shortfalls = %+v, want verdict-rejected and no stale subject for a reject of the head", shortfalls)
		}
		for _, s := range shortfalls {
			if s.Kind == run.ShortfallVerdictRejected && (s.ReviewID != reject.ID || s.SubjectCommitOID != "commit-head") {
				t.Fatalf("verdict-rejected shortfall = %+v, want review %s at commit-head", s, reject.ID)
			}
		}

		fixTask := f.createFeatureTask(t, 7630, 3, run.TaskIntegrated)
		f.createWorkerSession(t, fixTask, run.RoleImplementer, 7631)
		seedIntegratedHeadRow(t, f.featureFixture, 7635, fixTask, identity.AttemptID(uid(7631)), "commit-fix", "2026-09-14T10:10:00.000000000Z")
		shortfalls = loadShortfalls(t, f.featureFixture)
		if countKind(shortfalls, run.ShortfallVerdictRejected) != 0 || countKind(shortfalls, run.ShortfallVerdictStaleSubject) != 1 {
			t.Fatalf("guard shortfalls = %+v, want only verdict-stale-subject once a fix moved the head", shortfalls)
		}
	})

	t.Run("an approve of the integrated head leaves no verdict shortfall", func(t *testing.T) {
		f := newReviewFixture(t)
		seedIntegratedHeadRow(t, f.featureFixture, 7620, f.spec.TaskID, f.spec.AttemptID, "commit-head", "2026-09-14T10:05:00.000000000Z")
		if outcome, err := f.store.SubmitReview(t.Context(), f.submission(7611, run.VerdictApprove)); err != nil || outcome.Kind != app.ReviewAccepted {
			t.Fatalf("SubmitReview(approve) = %+v, %v", outcome, err)
		}
		shortfalls := loadShortfalls(t, f.featureFixture)
		for _, kind := range []run.ShortfallKind{run.ShortfallVerdictMissing, run.ShortfallVerdictRejected, run.ShortfallVerdictStaleSubject} {
			if countKind(shortfalls, kind) != 0 {
				t.Fatalf("guard shortfalls = %+v, want no %s for an approve of the head", shortfalls, kind)
			}
		}
		if countKind(shortfalls, run.ShortfallCheckMissing) != 1 || countKind(shortfalls, run.ShortfallPlanOpen) != 1 {
			t.Fatalf("guard shortfalls = %+v, want the plan and check guards still reported", shortfalls)
		}
	})

	t.Run("a head no row records a tree for reads the latest review as stale", func(t *testing.T) {
		f := newReviewFixture(t)
		seedIntegratedHeadRow(t, f.featureFixture, 7620, f.spec.TaskID, f.spec.AttemptID, "commit-other", "2026-09-14T10:05:00.000000000Z")
		if outcome, err := f.store.SubmitReview(t.Context(), f.submission(7611, run.VerdictReject)); err != nil || outcome.Kind != app.ReviewAccepted {
			t.Fatalf("SubmitReview(reject) = %+v, %v", outcome, err)
		}
		shortfalls := loadShortfalls(t, f.featureFixture)
		if countKind(shortfalls, run.ShortfallVerdictRejected) != 0 || countKind(shortfalls, run.ShortfallVerdictStaleSubject) != 1 ||
			countKind(shortfalls, run.ShortfallCheckMissing) != 1 {
			t.Fatalf("guard shortfalls = %+v, want check-missing and verdict-stale-subject", shortfalls)
		}
	})

	t.Run("recorded trees that disagree about the head report evidence-inconsistent", func(t *testing.T) {
		f := newReviewFixture(t)
		seedIntegratedHeadRow(t, f.featureFixture, 7620, f.spec.TaskID, f.spec.AttemptID, "commit-head", "2026-09-14T10:05:00.000000000Z")
		f.inUOW(t, func(uow app.UnitOfWork) {
			second := run.NewReviewTask(identity.TaskID(uid(7640)), f.spec.RunID, 4, "commit-head", "tree-other", f.clock.Now())
			if _, err := workflowRepos(t, uow).TaskIndex().Create(t.Context(), second); err != nil {
				t.Fatalf("create second review task: %v", err)
			}
		})
		if outcome, err := f.store.SubmitReview(t.Context(), f.submission(7611, run.VerdictReject)); err != nil || outcome.Kind != app.ReviewAccepted {
			t.Fatalf("SubmitReview(reject) = %+v, %v", outcome, err)
		}

		detail, err := f.store.LoadRunStatus(t.Context(), f.spec.RunID)
		if err != nil {
			t.Fatalf("LoadRunStatus() = %v, want the detail read to succeed", err)
		}
		kinds := map[string]int{}
		for _, s := range detail.GuardShortfalls {
			kinds[string(s.Kind)]++
			if s.ReviewID != "" || s.SubjectCommitOID != "" {
				t.Fatalf("shortfall %+v names a review; an inconsistent head names none", s)
			}
		}
		want := map[string]int{"plan-open": 1, "task-not-integrated": 1, "evidence-inconsistent": 1}
		if len(kinds) != len(want) {
			t.Fatalf("guard shortfalls = %+v, want exactly %v", detail.GuardShortfalls, want)
		}
		for kind, n := range want {
			if kinds[kind] != n {
				t.Fatalf("guard shortfalls = %+v, want exactly %v", detail.GuardShortfalls, want)
			}
		}
		// The rest of the detail is untouched: the tasks, the integration
		// and the reject notice queued for the manager.
		if len(detail.Tasks) != 3 || detail.LatestIntegration == nil || len(detail.Mailboxes) != 1 {
			t.Fatalf("detail = tasks %d, integration %+v, mailboxes %+v; want the ordinary block", len(detail.Tasks), detail.LatestIntegration, detail.Mailboxes)
		}
	})
}

// seedIntegratedHeadRow records an integrated integration row for task
// whose merge commit is mergeOID, settled at updatedAt, over a result
// accepted for attemptID.
func seedIntegratedHeadRow(t *testing.T, f *featureFixture, n int, task identity.TaskID, attemptID identity.AttemptID, mergeOID, updatedAt string) {
	t.Helper()
	resultID := uid(n)
	rawExec(t, f.store, `INSERT INTO results (id, attempt_id, commit_oid, summary, content_digest, accepted, submitted_at)
		VALUES (?, ?, 'source-oid', 'done', ?, 1, '2026-09-14T10:00:00.000000000Z')`, resultID, attemptID.String(), "digest-"+resultID)
	rawExec(t, f.store, `INSERT INTO integrations (id, run_id, task_id, result_id, source_commit_oid, premerge_head_oid, merge_commit_oid, state, operation_id, revision, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'source-oid', 'premerge-oid', ?, 'integrated', NULL, 2, '2026-09-14T10:00:00.000000000Z', ?)`,
		uid(n+1), f.spec.RunID.String(), task.String(), resultID, mergeOID, updatedAt)
}

// loadShortfalls reads the fixture run's guard shortfalls.
func loadShortfalls(t *testing.T, f *featureFixture) []run.GuardShortfall {
	t.Helper()
	detail, err := f.store.LoadRunStatus(t.Context(), f.spec.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus() = %v", err)
	}
	return detail.GuardShortfalls
}

// countKind counts shortfalls of kind.
func countKind(shortfalls []run.GuardShortfall, kind run.ShortfallKind) int {
	n := 0
	for _, s := range shortfalls {
		if s.Kind == kind {
			n++
		}
	}
	return n
}

// TestLoadCheckExecutionContextByKind proves the argv dispatch: a
// check.run returns the frozen snapshot argv, an integration.merge the
// intent's own merge argv and scratch tree, a malformed merge intent and
// a foreign kind fail closed.
func TestLoadCheckExecutionContextByKind(t *testing.T) {
	f := newFeatureFixture(t)
	mergeOp := identity.OperationID(uid(7951))
	brokenOp := identity.OperationID(uid(7952))
	paneOp := identity.OperationID(uid(7953))
	f.inUOW(t, func(uow app.UnitOfWork) {
		for _, op := range []app.Operation{
			{
				ID: mergeOp, RunID: f.spec.RunID, Generation: f.lease.Generation, Kind: app.OpIntegrationMerge, State: app.OperationPending,
				Intent: map[string]any{
					"integration_id": uid(7954), "premerge_oid": "premerge", "source_oid": "source",
					"tree_path":  "/state/root/runs/" + f.spec.RunID.String() + "/integrations/" + mergeOp.String() + "/tree",
					"hooks_path": "/state/root/hooks-empty",
					"merge_argv": []string{"/usr/bin/git", "-C", "/tree", "merge", "--no-ff", "source"},
					"spawn_argv": []string{"/usr/local/bin/hop", "check-exec", "--op", mergeOp.String()},
				},
				CreatedAt: f.clock.Now(), UpdatedAt: f.clock.Now(),
			},
			{
				ID: brokenOp, RunID: f.spec.RunID, Generation: f.lease.Generation, Kind: app.OpIntegrationMerge, State: app.OperationPending,
				Intent: map[string]any{"tree_path": "/tree"}, CreatedAt: f.clock.Now(), UpdatedAt: f.clock.Now(),
			},
			{
				ID: paneOp, RunID: f.spec.RunID, Generation: f.lease.Generation, Kind: app.OpPaneOpen, State: app.OperationPending,
				Intent: map[string]any{"session_id": uid(1)}, CreatedAt: f.clock.Now(), UpdatedAt: f.clock.Now(),
			},
		} {
			if err := uow.Operations().Create(t.Context(), op); err != nil {
				t.Fatalf("create operation: %v", err)
			}
		}
	})

	merge, err := f.store.LoadCheckExecutionContext(t.Context(), mergeOp)
	if err != nil {
		t.Fatalf("LoadCheckExecutionContext(merge) = %v", err)
	}
	wantArgv := []string{"/usr/bin/git", "-C", "/tree", "merge", "--no-ff", "source"}
	if len(merge.CheckArgv) != len(wantArgv) {
		t.Fatalf("merge argv = %v, want the intent's frozen argv", merge.CheckArgv)
	}
	for i := range wantArgv {
		if merge.CheckArgv[i] != wantArgv[i] {
			t.Fatalf("merge argv[%d] = %q, want %q", i, merge.CheckArgv[i], wantArgv[i])
		}
	}
	if merge.CheckoutPath == "" || merge.StateRoot != f.spec.Snapshot.StateRoot {
		t.Fatalf("merge context = %+v, want the intent tree path under the frozen root", merge)
	}

	if _, err := f.store.LoadCheckExecutionContext(t.Context(), brokenOp); err == nil {
		t.Fatal("a merge intent without an argv loaded; want fail closed")
	}
	if _, err := f.store.LoadCheckExecutionContext(t.Context(), paneOp); err == nil {
		t.Fatal("a pane.open operation loaded as an execution context; want refused")
	}
}
