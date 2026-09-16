package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// ReleasedTask is one task RecomputeReleases moved from pending to ready.
type ReleasedTask struct {
	TaskID identity.TaskID
	Seq    int
}

// ReleaseReport is RecomputeReleases's result.
type ReleaseReport struct {
	Released []ReleasedTask
}

// RecomputeReleases is the section 6 scheduling pass's "recompute
// releases" step: every pending, dependency-bearing implement task in the
// run is released (pending -> ready) once every prerequisite it names has
// reached integrated. One controller transaction covers the whole pass —
// releases never conflict with each other or with any other writer, since
// ReleaseEligible is a pure function of already-committed prerequisite
// state and pending is Release's only legal source state.
func (c *Controller) RecomputeReleases(ctx context.Context, handle RunHandle) (ReleaseReport, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; this runs once per scheduling pass, never a hot loop.
	var report ReleaseReport
	now := c.Clock.Now()
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, err := RequireWorkflowRepositories(uow, "RecomputeReleases")
		if err != nil {
			return err
		}
		tasks, err := wf.TaskIndex().ByRun(ctx, handle.runID)
		if err != nil {
			return err
		}
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].Seq < tasks[j].Seq })
		byID := make(map[identity.TaskID]run.Task, len(tasks))
		for i := range tasks {
			byID[tasks[i].ID] = tasks[i]
		}
		edges, err := wf.TaskDependencies().ByRun(ctx, handle.runID)
		if err != nil {
			return err
		}
		edgesByTask := map[identity.TaskID][]run.TaskDependency{}
		for _, e := range edges {
			edgesByTask[e.TaskID] = append(edgesByTask[e.TaskID], e)
		}

		for i := range tasks {
			t := tasks[i]
			if t.State != run.TaskPending || t.Kind == run.TaskKindReview || !t.HasDependencies {
				continue
			}
			taskEdges := edgesByTask[t.ID]
			prereqs := make([]run.Task, 0, len(taskEdges))
			complete := true
			for _, e := range taskEdges {
				p, found := byID[e.PrerequisiteID]
				if !found {
					complete = false
					break
				}
				prereqs = append(prereqs, p)
			}
			if !complete || !run.ReleaseEligible(t, taskEdges, prereqs) {
				continue
			}
			_, taskRev, err := uow.Tasks().Get(ctx, t.ID)
			if err != nil {
				return err
			}
			released, err := t.Release(taskEdges, prereqs, now)
			if err != nil {
				return fmt.Errorf("app: release task %s: %w", t.ID, err)
			}
			if _, err := uow.Tasks().Save(ctx, released, taskRev); err != nil {
				return err
			}
			if err := recordTransition(ctx, uow, EntityTask, t.ID.String(), string(run.TaskPending), string(run.TaskReady), "dependency release", gen(handle.lease.Generation), now); err != nil {
				return err
			}
			report.Released = append(report.Released, ReleasedTask{TaskID: t.ID, Seq: t.Seq})
		}
		return nil
	})
	return report, err
}

// AssignmentOptions configures one AssignReadyTasks call. IntegrationHeadCommitOID
// is the current integration branch head — the base every IMPLEMENT task's
// fresh worktree branches from (section 6); a review task branches from its
// own frozen subject instead, since its base never depends on later
// integration activity. Resolving IntegrationHeadCommitOID is 2b's
// integration-operations concern; AssignReadyTasks only consumes it.
type AssignmentOptions struct {
	MaxWorkers               int
	IntegrationHeadCommitOID string
	Harness                  run.Harness
	ReviewerHarness          run.Harness
	RepositoryRoot           string
	HOPPath                  string
	StateRoot                string
}

// validateAssignmentOptions refuses an AssignmentOptions value the
// scheduling-pass loop has not actually populated: RepositoryRoot,
// HOPPath and StateRoot back every worktree, launch and artifact write
// this step performs, so an empty or relative value here would silently
// misdirect every git and file-write call at the controller process's own
// working directory instead of the run's frozen locations, rather than
// failing loudly the way a caller passing the zero AssignmentOptions{}
// deserves. The refusals name the field, never its value.
func validateAssignmentOptions(opts AssignmentOptions) error { //nolint:gocritic // hugeParam: AssignmentOptions is the per-call DTO already threaded through this file; a pointer would only complicate every call site.
	if !filepath.IsAbs(opts.RepositoryRoot) {
		return errors.New("app: assignment repository root is not absolute")
	}
	if !filepath.IsAbs(opts.HOPPath) {
		return errors.New("app: assignment hop executable path is not absolute")
	}
	if !filepath.IsAbs(opts.StateRoot) {
		return errors.New("app: assignment state root is not absolute")
	}
	return nil
}

// requireFrozenAssignmentRoots refuses an AssignmentOptions whose
// RepositoryRoot or StateRoot is not the run's own frozen value. A known
// run operates on its frozen repository, never on whatever directory the
// caller happens to run in: a worktree created from another repository
// would pass provenance against that same foreign root. The refusals name
// the field, never either value.
func requireFrozenAssignmentRoots(opts *AssignmentOptions, frozen *FrozenRun) error {
	if opts.RepositoryRoot != frozen.RepositoryRoot {
		return errors.New("app: assignment repository root is not the run's frozen repository root")
	}
	if opts.StateRoot != frozen.Snapshot.StateRoot {
		return errors.New("app: assignment state root is not the run's frozen state root")
	}
	return nil
}

// AssignmentDefaults returns every AssignReadyTasks field the run itself
// fixes, read from its frozen record: MaxWorkers, the default Harness and
// ReviewerHarness from the frozen WorkflowSnapshot, and the frozen
// RepositoryRoot and StateRoot — the repository a known run operates on
// is its frozen repository, never the caller's working directory. The
// scheduling-pass loop (cmd/hop) adds HOPPath (the running binary) and the
// per-pass IntegrationHeadCommitOID (ResolveIntegrationHead). Composition
// passes only primitives and app-defined DTOs; the domain Harness type
// this returns is named only here, never in cmd/hop.
func (c *Controller) AssignmentDefaults(ctx context.Context, handle RunHandle) (AssignmentOptions, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per scheduling pass.
	frozen, err := c.Read.LoadFrozenRun(ctx, handle.runID)
	if err != nil {
		return AssignmentOptions{}, fmt.Errorf("app: load frozen run: %w", err)
	}
	return AssignmentOptions{
		MaxWorkers:      frozen.Snapshot.Workflow.MaxWorkers,
		Harness:         run.Harness(frozen.Snapshot.Harness),
		ReviewerHarness: run.Harness(frozen.Snapshot.Workflow.ReviewerHarness),
		RepositoryRoot:  frozen.RepositoryRoot,
		StateRoot:       frozen.Snapshot.StateRoot,
	}, nil
}

// AssignedTask is one task AssignReadyTasks launched.
type AssignedTask struct {
	TaskID        identity.TaskID
	AttemptID     identity.AttemptID
	AttemptNumber int
	SessionID     identity.SessionID
	Role          run.Role
	WorktreeInfo  WorktreeInfo
}

// AssignmentReport is AssignReadyTasks's result.
type AssignmentReport struct {
	Assigned []AssignedTask
	// SlotsFull is true when a ready task remained but every worker slot
	// was occupied at the last attempt.
	SlotsFull bool
}

// AssignReadyTasks is the section 6 scheduling pass's "assign released
// tasks into free slots" step: in task seq order, while a bounded-
// concurrency slot is free, claims the lowest-seq ready task (task
// ready -> active, an attempt reserved or an already-reserved retry
// attempt launched, a fresh implementer or reviewer session delegated
// from the manager, and the worktree/launch intents), one committed
// controller transaction per task exactly as StartRun commits one per
// run. MaxWorkers (default 2, opts.MaxWorkers <= 0) bounds the count of
// non-terminal, non-manager sessions — the reviewer shares the bound —
// counted INSIDE each assignment transaction so it can never overshoot
// even across a takeover.
func (c *Controller) AssignReadyTasks(ctx context.Context, handle RunHandle, opts AssignmentOptions) (AssignmentReport, error) { //nolint:gocritic // hugeParam: RunHandle and AssignmentOptions are per-call DTOs; this runs once per scheduling pass, never a hot loop.
	if err := validateAssignmentOptions(opts); err != nil {
		return AssignmentReport{}, err
	}
	maxWorkers := opts.MaxWorkers
	if maxWorkers <= 0 {
		maxWorkers = 2
	}
	// The frozen run is loaded once, outside every assignment transaction:
	// the per-attempt assignment artifact derives its path from the frozen
	// state root and references the frozen role-artifact copies, exactly
	// the values the launch boundary later re-derives, so writer and
	// prompt can never disagree.
	frozen, err := c.Read.LoadFrozenRun(ctx, handle.runID)
	if err != nil {
		return AssignmentReport{}, fmt.Errorf("app: load frozen run: %w", err)
	}
	// Refused before any task is reserved: every worktree, provenance
	// check and pane below would otherwise follow the supplied roots.
	if err := requireFrozenAssignmentRoots(&opts, &frozen); err != nil {
		return AssignmentReport{}, err
	}
	var report AssignmentReport
	for {
		assigned, full, more, err := c.assignOneReadyTask(ctx, handle, &frozen, opts, maxWorkers)
		if err != nil {
			return report, err
		}
		if full {
			report.SlotsFull = true
			return report, nil
		}
		if !more {
			return report, nil
		}
		report.Assigned = append(report.Assigned, assigned)
	}
}

// assignOneReadyTask claims and launches at most one ready task. ok is
// false when there is nothing left to assign; full is true when a ready
// task remains but every slot is occupied.
func (c *Controller) assignOneReadyTask(ctx context.Context, handle RunHandle, frozen *FrozenRun, opts AssignmentOptions, maxWorkers int) (AssignedTask, bool, bool, error) { //nolint:gocritic // hugeParam: RunHandle and AssignmentOptions are per-call DTOs; called in a bounded loop by AssignReadyTasks.
	now := c.Clock.Now()
	var (
		full          bool
		task          run.Task
		attempt       run.Attempt
		attemptExists bool
		manager       run.Session
		role          run.Role
		baseCommitOID string
		harness       run.Harness
		sessionID     identity.SessionID
		worktreeID    identity.WorktreeID
		incarnationID identity.IncarnationID
		runSeq        int
		prior         *priorAttemptFeedback
		reviewBaseOID string
	)

	txErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, err := RequireWorkflowRepositories(uow, "AssignReadyTasks")
		if err != nil {
			return err
		}

		// Section 4's revalidation rule, applied to this transaction's own
		// writes (not just the later external act): a stop racing between
		// dependency release and assignment must never let this
		// transaction activate a task or launch an attempt/session. Read
		// fresh and checked first, before any other read or write, so a
		// concurrently committed stop is never missed.
		r, _, err := uow.Runs().Get(ctx, handle.runID)
		if err != nil {
			return err
		}
		runSeq = r.Sequence
		if acceptErr := r.CanAcceptManagerVerb(); acceptErr != nil {
			return acceptErr
		}
		if r.StopRequested {
			return fmt.Errorf("%w: run %s", ErrStopRequested, handle.runID)
		}

		occupied, err := countOccupiedChildSessions(ctx, wf, handle.runID)
		if err != nil {
			return err
		}
		if occupied >= maxWorkers {
			full = true
			return nil
		}

		tasks, err := wf.TaskIndex().ByRun(ctx, handle.runID)
		if err != nil {
			return err
		}
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].Seq < tasks[j].Seq })
		found := false
		for i := range tasks {
			if tasks[i].State == run.TaskReady {
				task = tasks[i]
				found = true
				break
			}
		}
		if !found {
			return nil
		}

		mgr, _, err := wf.ManagerSession(ctx, handle.runID)
		if err != nil {
			return err
		}
		manager = mgr

		attempts, err := wf.AttemptIndex().ByTask(ctx, task.ID)
		if err != nil {
			return err
		}
		for _, a := range attempts {
			if a.State == run.AttemptReserved {
				attempt = a
				attemptExists = true
				break
			}
		}
		if !attemptExists {
			var attemptID identity.AttemptID
			attemptID, err = identity.ParseAttemptID(c.IDs.NewID())
			if err != nil {
				return fmt.Errorf("app: generate attempt id: %w", err)
			}
			attempt, err = run.NewAttempt(attemptID, task.ID, 1, now)
			if err != nil {
				return err
			}
		}

		role = run.RoleImplementer
		baseCommitOID = opts.IntegrationHeadCommitOID
		harness = opts.Harness
		if task.Kind == run.TaskKindReview {
			role = run.RoleReviewer
			baseCommitOID = task.SubjectCommitOID
			harness = opts.ReviewerHarness
			if harness == "" {
				harness = opts.Harness
			}
		}

		sid, err := identity.ParseSessionID(c.IDs.NewID())
		if err != nil {
			return fmt.Errorf("app: generate session id: %w", err)
		}
		sessionID = sid
		wid, err := identity.ParseWorktreeID(c.IDs.NewID())
		if err != nil {
			return fmt.Errorf("app: generate worktree id: %w", err)
		}
		worktreeID = wid
		incID, err := identity.ParseIncarnationID(c.IDs.NewID())
		if err != nil {
			return fmt.Errorf("app: generate incarnation id: %w", err)
		}
		incarnationID = incID

		child, err := run.NewChildSession(sessionID, handle.runID, attempt.ID, role, manager, harness, now)
		if err != nil {
			return err
		}
		// A Claude session's first launch is `--session-id <ref>`, so the
		// reference is pre-assigned here, as StartRun assigns the solo
		// worker's; Codex and opencode sessions carry none.
		if harness == run.HarnessClaude {
			if child, err = child.AssignNativeRef(c.IDs.NewID(), run.NativeRefAssigned, now); err != nil {
				return err
			}
		}

		taskFrom := task.State
		activated, err := task.Activate(now)
		if err != nil {
			return err
		}
		_, taskRev, err := uow.Tasks().Get(ctx, task.ID)
		if err != nil {
			return err
		}
		if _, saveErr := uow.Tasks().Save(ctx, activated, taskRev); saveErr != nil {
			return saveErr
		}
		task = activated

		attemptFrom := attempt.State
		launched, err := attempt.Launch(now)
		if err != nil {
			return err
		}
		if attemptExists {
			var attemptRev int64
			_, attemptRev, err = uow.Attempts().Get(ctx, attempt.ID)
			if err != nil {
				return err
			}
			if _, saveErr := uow.Attempts().Save(ctx, launched, attemptRev); saveErr != nil {
				return saveErr
			}
		} else if _, createErr := wf.AttemptIndex().Create(ctx, launched); createErr != nil {
			return createErr
		}
		attempt = launched

		// A retry-reopened task's attempt was already reserved by
		// PlanStore.RequestRetry (worker-authority, immediate — the CLI
		// grammar reports the attempt number in that same call); this
		// assignment is what actually launches it, so the bookkeeping
		// retry_requests row is marked consumed here, in the same
		// transaction, rather than by a separate scheduling step.
		if attemptExists {
			pendingRetries, pendingErr := wf.RetryRequests().Pending(ctx, handle.runID)
			if pendingErr != nil {
				return pendingErr
			}
			for _, req := range pendingRetries {
				if req.TaskID == task.ID {
					if consumeErr := wf.RetryRequests().MarkConsumed(ctx, task.ID, attempt.Number); consumeErr != nil {
						return consumeErr
					}
					break
				}
			}
		}

		// Assignment-artifact inputs, read inside the same transaction that
		// claims the task (the artifact write itself is an external act
		// after commit, like the worktree): a retry attempt's prior-attempt
		// feedback paths, and a review task's diff-scope base — the run's
		// FIRST integration's recorded pre-merge head, which is the
		// integration branch's creation base.
		if task.Kind != run.TaskKindReview && attempt.Number > 1 {
			feedback, fbErr := collectPriorAttemptFeedback(ctx, uow, wf, handle.runID, frozen.Snapshot.StateRoot, attempts, attempt.Number)
			if fbErr != nil {
				return fbErr
			}
			prior = feedback
		}
		if task.Kind == run.TaskKindReview {
			base, baseErr := earliestIntegrationBase(ctx, wf, tasks)
			if baseErr != nil {
				return baseErr
			}
			if base == "" {
				return fmt.Errorf("app: no integration is recorded for the run; a review task's diff scope cannot be rendered")
			}
			reviewBaseOID = base
		}

		if _, createErr := uow.Sessions().Create(ctx, child); createErr != nil {
			return createErr
		}
		launchedSession, err := child.Launch(now)
		if err != nil {
			return err
		}
		if _, err := uow.Sessions().Save(ctx, launchedSession, 1); err != nil {
			return err
		}

		generation := gen(handle.lease.Generation)
		if err := recordTransition(ctx, uow, EntityTask, task.ID.String(), string(taskFrom), string(task.State), "assignment", generation, now); err != nil {
			return err
		}
		if err := recordTransition(ctx, uow, EntityAttempt, attempt.ID.String(), string(attemptFrom), string(attempt.State), "assignment", generation, now); err != nil {
			return err
		}
		return recordTransition(ctx, uow, EntitySession, sessionID.String(), string(run.SessionReserved), string(launchedSession.State), "delegated by manager", generation, now)
	})
	if txErr != nil {
		return AssignedTask{}, false, false, txErr
	}
	if full || task.ID == "" {
		return AssignedTask{}, full, false, nil
	}

	branch := fmt.Sprintf("hop/r%d/t%da%d", runSeq, task.Seq, attempt.Number)
	worktreeInfo, err := c.createAttemptWorktree(ctx, handle, worktreeID, attempt.ID, opts.RepositoryRoot, branch, baseCommitOID, now)
	if err != nil {
		return AssignedTask{}, false, false, err
	}
	// The per-attempt assignment artifact is written after the worktree
	// and before the pane opens: the launch prompt references it by the
	// same derived path, and the worker reads it the moment it starts.
	if err := c.writeAttemptAssignment(ctx, frozen, &task, &attempt, role, opts.HOPPath, prior, reviewBaseOID); err != nil {
		return AssignedTask{}, false, false, err
	}
	if err := c.openChildPane(ctx, handle, task.ID, attempt.ID, sessionID, incarnationID, role, worktreeInfo, opts.HOPPath, opts.StateRoot, now); err != nil {
		return AssignedTask{}, false, false, err
	}

	return AssignedTask{
		TaskID: task.ID, AttemptID: attempt.ID, AttemptNumber: attempt.Number,
		SessionID: sessionID, Role: role, WorktreeInfo: worktreeInfo,
	}, false, true, nil
}

// collectPriorAttemptFeedback assembles a retry attempt's prior-attempt
// feedback (design section 6): the newest earlier attempt's accepted
// result commit, its check execution's retained stdout/stderr paths
// (located by the newest check.run operation whose intent names that
// result), and — when the run's latest accepted review is a reject — the
// review reasons path. Every element is optional evidence; absence
// renders as absence, never as an error.
func collectPriorAttemptFeedback(ctx context.Context, uow UnitOfWork, wf WorkflowRepositories, runID identity.RunID, stateRoot string, attempts []run.Attempt, attemptNumber int) (*priorAttemptFeedback, error) {
	var priorAttempt *run.Attempt
	for i := range attempts {
		if attempts[i].Number >= attemptNumber {
			continue
		}
		if priorAttempt == nil || attempts[i].Number > priorAttempt.Number {
			priorAttempt = &attempts[i]
		}
	}
	if priorAttempt == nil {
		return nil, nil //nolint:nilnil // no earlier attempt means no feedback section, not an error.
	}
	feedback := &priorAttemptFeedback{Number: priorAttempt.Number}
	result, err := uow.Results().Accepted(ctx, priorAttempt.ID)
	if err != nil {
		return nil, err
	}
	if result != nil {
		feedback.ResultCommitOID = result.CommitOID
		ops, opErr := uow.Operations().ByKind(ctx, runID, OpCheckRun)
		if opErr != nil {
			return nil, opErr
		}
		for i := range ops { // newest first: the first match is the newest execution.
			intent, ok := decodeOperationPayload[CheckRunIntent](ops[i].Intent)
			if !ok || intent.ResultID != result.ID.String() {
				continue
			}
			feedback.CheckStdoutPath, feedback.CheckStderrPath = checkEvidencePaths(stateRoot, runID, ops[i].ID)
			break
		}
	}
	latest, err := wf.Reviews().Latest(ctx, runID)
	if err != nil {
		return nil, err
	}
	if latest != nil && latest.Verdict == run.VerdictReject {
		feedback.ReviewReasonsPath = reviewReasonsPath(stateRoot, runID, latest.ID)
	}
	return feedback, nil
}

// earliestIntegrationBase finds the run's FIRST integration's recorded
// pre-merge head across every task — the integration branch's creation
// base, which is the review assignment's diff-scope base. "" when the
// run has no integration row at all.
func earliestIntegrationBase(ctx context.Context, wf WorkflowRepositories, tasks []run.Task) (string, error) {
	var (
		base     string
		earliest time.Time
	)
	for i := range tasks {
		integrations, err := wf.Integrations().ByTask(ctx, tasks[i].ID)
		if err != nil {
			return "", err
		}
		for j := range integrations {
			if base == "" || integrations[j].CreatedAt.Before(earliest) {
				earliest = integrations[j].CreatedAt
				base = integrations[j].PremergeHeadOID
			}
		}
	}
	return base, nil
}

// writeAttemptAssignment renders and durably writes one assigned
// attempt's assignment artifact — the implementer's task assignment or
// the reviewer's review assignment — at the app-derived per-attempt path
// the launch prompt references (attemptAssignmentPath), referencing the
// frozen role-artifact copies. Called between the worktree act and the
// pane.open intent; a failed write fails the assignment before any pane
// exists.
func (c *Controller) writeAttemptAssignment(ctx context.Context, frozen *FrozenRun, task *run.Task, attempt *run.Attempt, role run.Role, hopPath string, prior *priorAttemptFeedback, reviewBaseOID string) error {
	stateRoot := frozen.Snapshot.StateRoot
	path := attemptAssignmentPath(stateRoot, task.RunID, attempt.ID)
	var content []byte
	if role == run.RoleReviewer {
		rolePath := frozen.Snapshot.Workflow.ReviewerRolePath
		if !filepath.IsAbs(rolePath) {
			return fmt.Errorf("app: the frozen reviewer role artifact path is missing or not absolute; a feature run freezes it before any assignment")
		}
		content = renderReviewAssignment(&reviewAssignmentFields{
			RunID: task.RunID.String(), TaskID: task.ID.String(), AttemptID: attempt.ID.String(),
			TaskSeq: task.Seq, AttemptNumber: attempt.Number,
			SubjectCommitOID: task.SubjectCommitOID, SubjectTreeOID: task.SubjectTreeOID,
			DiffBaseOID: reviewBaseOID, RolePath: rolePath, AssignmentPath: path, HOPPath: hopPath,
		})
	} else {
		rolePath := frozen.Snapshot.Workflow.ImplementerRolePath
		if !filepath.IsAbs(rolePath) {
			return fmt.Errorf("app: the frozen implementer role artifact path is missing or not absolute; a feature run freezes it before any assignment")
		}
		content = renderTaskAssignment(&taskAssignmentFields{
			RunID: task.RunID.String(), TaskID: task.ID.String(), AttemptID: attempt.ID.String(),
			TaskSeq: task.Seq, AttemptNumber: attempt.Number, Title: task.Title,
			InstructionsPath: taskInstructionsPath(stateRoot, task.RunID, task.ID),
			RolePath:         rolePath, AssignmentPath: path, HOPPath: hopPath, Prior: prior,
		})
	}
	if err := c.Artifacts.WriteArtifact(ctx, path, content); err != nil {
		return fmt.Errorf("app: write attempt assignment artifact: %w", err)
	}
	return nil
}

// countOccupiedChildSessions counts runID's non-terminal, non-manager
// sessions: the section 6 bounded-concurrency contract. A session in
// reconciling or stopping still occupies its slot; only terminated and
// lost do not.
func countOccupiedChildSessions(ctx context.Context, wf WorkflowRepositories, runID identity.RunID) (int, error) {
	sessions, err := wf.SessionIndex().ByRun(ctx, runID)
	if err != nil {
		return 0, err
	}
	count := 0
	for i := range sessions {
		if sessions[i].Role == run.RoleManager {
			continue
		}
		if sessions[i].State == run.SessionTerminated || sessions[i].State == run.SessionLost {
			continue
		}
		count++
	}
	return count, nil
}

// attemptWorktreeCreateIntent is the OpWorktreeCreate operation's intent
// payload for a per-attempt worktree: the Phase 2 shape
// (worktreeCreateIntent) plus the attempt it belongs to, since Phase 3
// creates one worktree per attempt rather than one per run. Declared
// separately from worktreeCreateIntent (usecase_run.go, the Phase 2 solo
// flow) so that flow's recovery payload shape is untouched.
type attemptWorktreeCreateIntent struct {
	RepositoryRoot string             `json:"repository_root"`
	Branch         string             `json:"branch"`
	BaseRef        string             `json:"base_ref"`
	AttemptID      identity.AttemptID `json:"attempt_id"`
}

// createAttemptWorktree drives the OpWorktreeCreate operation for one
// attempt's fresh worktree: record-intent, act (Runtime.CreateWorktree),
// record-outcome, mirroring StartRun's createWorktree but carrying the
// owning attempt in its own intent shape (attemptWorktreeCreateIntent)
// rather than reusing the Phase 2 solo-flow's worktreeCreateIntent.
func (c *Controller) createAttemptWorktree(ctx context.Context, handle RunHandle, worktreeID identity.WorktreeID, attemptID identity.AttemptID, repositoryRoot, branch, baseOID string, now time.Time) (WorktreeInfo, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per task assignment.
	opID, err := c.newOperationID()
	if err != nil {
		return WorktreeInfo{}, err
	}
	intent := attemptWorktreeCreateIntent{RepositoryRoot: repositoryRoot, Branch: branch, BaseRef: baseOID, AttemptID: attemptID}
	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpWorktreeCreate, State: OperationPending, Intent: intent,
			CreatedAt: now, UpdatedAt: now,
		})
	}); err != nil {
		return WorktreeInfo{}, fmt.Errorf("app: record worktree.create intent: %w", err)
	}

	if err := c.revalidateForDispatch(ctx, handle, false); err != nil {
		return WorktreeInfo{}, fmt.Errorf("app: revalidate before worktree.create: %w", err)
	}
	actCtx, release := handle.actContext(ctx)
	info, actErr := c.Runtime.CreateWorktree(actCtx, WorktreeRequest{
		RepositoryRoot: repositoryRoot, Branch: branch, BaseRef: baseOID,
	})
	provenance := worktreeAmbiguous
	provenanceDetail := ""
	if actErr == nil {
		provenance, provenanceDetail = c.classifyWorktreeProvenance(actCtx, info.Path, repositoryRoot, baseOID)
	}
	release()

	outcomeErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		op, getErr := uow.Operations().Get(ctx, opID)
		if getErr != nil {
			return getErr
		}
		op.UpdatedAt = c.Clock.Now()
		switch {
		case actErr != nil:
			op.State = OperationReconciling
			op.Outcome = actErr.Error()
			return uow.Operations().Save(ctx, op)
		case provenance == worktreeValid:
			r, _, runErr := uow.Runs().Get(ctx, handle.runID)
			if runErr != nil {
				return runErr
			}
			// The row names its attempt and verified base: the launch
			// boundary finds a feature session's worktree by attempt, never
			// by run, once the run holds more than one worktree.
			worktree, wtErr := run.NewAttemptWorktree(worktreeID, r.RepositoryID, handle.runID, attemptID, baseOID, info.Path, info.Branch)
			if wtErr != nil {
				return wtErr
			}
			if _, createErr := uow.Worktrees().Create(ctx, worktree); createErr != nil {
				return createErr
			}
			op.State = OperationSucceeded
			op.ActEvidence = worktreeCreateOutcome{Info: info, BaseCommit: baseOID}
			return uow.Operations().Save(ctx, op)
		case provenance == worktreeUnrelated:
			op.State = OperationFailed
			op.Outcome = provenanceDetail
			return uow.Operations().Save(ctx, op)
		default:
			op.State = OperationReconciling
			op.Outcome = provenanceDetail
			return uow.Operations().Save(ctx, op)
		}
	})
	var resultErr error
	switch {
	case actErr != nil:
		resultErr = fmt.Errorf("app: worktree.create: %w (operation %s is reconciling)", actErr, opID)
	case provenance == worktreeUnrelated:
		resultErr = fmt.Errorf("app: worktree.create provenance: %s", provenanceDetail)
	case provenance != worktreeValid:
		resultErr = fmt.Errorf("app: worktree.create provenance ambiguous: %s (operation %s is reconciling)", provenanceDetail, opID)
	}
	switch {
	case resultErr != nil && outcomeErr != nil:
		return WorktreeInfo{}, fmt.Errorf("%w; recording the outcome also failed: %w", resultErr, outcomeErr)
	case outcomeErr != nil:
		return WorktreeInfo{}, fmt.Errorf("app: record worktree.create outcome: %w", outcomeErr)
	case resultErr != nil:
		return WorktreeInfo{}, resultErr
	}
	return info, nil
}

// childPaneOpenIntent is the OpPaneOpen operation's intent payload for a
// delegated (implementer/reviewer) session's pane: the Phase 2 shape
// (paneOpenIntent) already carries IncarnationID/SessionID, which is all
// SubmissionStore.ClaimLaunch's pre-binding fallback needs; declared here
// only to keep this file self-contained from usecase_run.go's Phase 2
// solo-flow intent construction.
func (c *Controller) openChildPane(ctx context.Context, handle RunHandle, taskID identity.TaskID, attemptID identity.AttemptID, sessionID identity.SessionID, incarnationID identity.IncarnationID, role run.Role, worktree WorktreeInfo, hopPath, stateRoot string, now time.Time) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per task assignment.
	opID, err := c.newOperationID()
	if err != nil {
		return err
	}
	argv := []string{hopPath, "launch", "--run", handle.runID.String(), "--session", sessionID.String()}
	env := map[string]string{
		"HOP_STATE_DIR":      stateRoot,
		"HOP_RUN_ID":         handle.runID.String(),
		"HOP_TASK_ID":        taskID.String(),
		"HOP_ATTEMPT_ID":     attemptID.String(),
		"HOP_INCARNATION_ID": incarnationID.String(),
		"HOP_SESSION_ID":     sessionID.String(),
		"HOP_ROLE":           string(role),
	}
	serverInstance := c.observeServerInstance(ctx)
	intent := paneOpenIntent{Command: argv, Cwd: worktree.Path, WorkspaceID: worktree.WorkspaceID, Label: opID.String(), IncarnationID: incarnationID, SessionID: sessionID, ServerInstance: serverInstance}

	if err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		return uow.Operations().Create(ctx, Operation{
			ID: opID, RunID: handle.runID, Generation: handle.lease.Generation,
			Kind: OpPaneOpen, State: OperationPending, Intent: intent,
			CreatedAt: now, UpdatedAt: now,
		})
	}); err != nil {
		return fmt.Errorf("app: record pane.open intent: %w", err)
	}

	if err := c.revalidateForDispatch(ctx, handle, false); err != nil {
		return fmt.Errorf("app: revalidate before pane.open: %w", err)
	}
	actCtx, release := handle.actContext(ctx)
	paneHandle, actErr := c.Runtime.OpenWorkerPane(actCtx, WorkerPaneRequest{
		WorkspaceID: worktree.WorkspaceID, Cwd: worktree.Path, Command: argv, Env: env, Label: opID.String(),
	})
	if actErr != nil {
		if ref, found, findErr := c.Runtime.FindPaneByLabel(actCtx, opID.String()); findErr == nil && found {
			paneHandle = PaneHandle(ref)
			actErr = nil
		}
	}
	release()

	outcomeErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		op, getErr := uow.Operations().Get(ctx, opID)
		if getErr != nil {
			return getErr
		}
		op.UpdatedAt = c.Clock.Now()
		if actErr != nil {
			op.State = OperationReconciling
			op.Outcome = actErr.Error()
			return uow.Operations().Save(ctx, op)
		}
		binding := run.NewRuntimeBinding(sessionID, incarnationID, "", serverInstance, paneHandle.WorkspaceID, paneHandle.TabID, paneHandle.PaneID, opID.String(), run.LaunchInitial, op.UpdatedAt)
		if bindErr := uow.Bindings().Create(ctx, binding); bindErr != nil {
			return bindErr
		}
		op.State = OperationSucceeded
		op.ActEvidence = paneHandle
		return uow.Operations().Save(ctx, op)
	})
	if outcomeErr != nil {
		return fmt.Errorf("app: record pane.open outcome: %w", outcomeErr)
	}
	if actErr != nil {
		return fmt.Errorf("app: pane.open: %w (operation %s is reconciling)", actErr, opID)
	}
	return nil
}
