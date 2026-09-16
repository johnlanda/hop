package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
	"github.com/johnlanda/hop/internal/testsupport/runnervectors"
)

// Fixed values of the assignment harness below.
const (
	linkHOPPath        = "/usr/local/bin/hop"
	linkGitPath        = "/usr/bin/git"
	linkHarnessPath    = "/opt/harness/claude"
	linkHeadOID        = "1111111111111111111111111111111111111111"
	linkSubjectOID     = "2222222222222222222222222222222222222222"
	linkSubjectTreeOID = "3333333333333333333333333333333333333333"
	linkManagerPane    = "pane-m"
	linkManagerPID     = 4242
	linkServerInstance = "server-instance-1"
)

// linkRuntime is the Herdr side of the assignment harness: every created
// worktree gets its own path (spike S9's shape), remembered with the base
// it was requested at so linkGit can answer provenance for it; the manager
// pane reports its corroborating harness process.
type linkRuntime struct {
	mu      sync.Mutex
	n       int
	bases   map[string]string
	manager app.PaneProcess
}

func (r *linkRuntime) CreateWorktree(_ context.Context, req app.WorktreeRequest) (app.WorktreeInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	path := "/worktrees/feature/" + strings.ReplaceAll(req.Branch, "/", "-")
	r.bases[path] = req.BaseRef
	return app.WorktreeInfo{WorkspaceID: fmt.Sprintf("ws-%d", r.n), Path: path, Branch: req.Branch}, nil
}

func (r *linkRuntime) OpenWorkerPane(_ context.Context, req app.WorkerPaneRequest) (app.PaneHandle, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	return app.PaneHandle{WorkspaceID: req.WorkspaceID, TabID: fmt.Sprintf("tab-%d", r.n), PaneID: fmt.Sprintf("pane-%d", r.n)}, nil
}

func (*linkRuntime) FindPaneByLabel(context.Context, string) (app.PaneRef, bool, error) {
	return app.PaneRef{}, false, nil
}

func (*linkRuntime) ReadPane(context.Context, string, int) (string, error) { return "", nil }

func (r *linkRuntime) InspectPane(_ context.Context, paneID string) (app.PaneProcess, error) {
	if paneID != linkManagerPane {
		return app.PaneProcess{}, fmt.Errorf("unexpected inspection of pane %s", paneID)
	}
	return r.manager, nil
}

func (*linkRuntime) ClosePane(context.Context, string) error {
	return errors.New("the assignment harness never closes a pane")
}

func (*linkRuntime) ServerInstance(context.Context) (string, error) { return linkServerInstance, nil }

// linkGit answers exactly the git invocations worktree provenance runs:
// every known checkout shares the repository's common directory, and each
// created worktree's HEAD is the base it was requested at. It follows the
// real runner's capture contract (runnervectors): a negative output bound
// is refused before anything answers, and every answer is bounded with its
// truncation flags.
type linkGit struct {
	repositoryRoot string
	runtime        *linkRuntime
	// answer, when set, replaces the answer to every accepted invocation,
	// so the shared capture vectors run through this fake's own Run.
	answer *app.CommandResult
}

func (g linkGit) Run(_ context.Context, cmd app.Command) (app.CommandResult, error) {
	if err := runnervectors.ValidateBound(cmd.MaxOutputBytes); err != nil {
		return app.CommandResult{}, err
	}
	result, err := g.respond(cmd)
	if err != nil {
		return app.CommandResult{}, err
	}
	return runnervectors.BoundCapture(result, cmd.MaxOutputBytes)
}

// respond is linkGit's complete answer to cmd, before the capture bound.
func (g linkGit) respond(cmd app.Command) (app.CommandResult, error) {
	if len(cmd.Argv) < 4 || cmd.Argv[0] != linkGitPath || cmd.Argv[1] != "-C" {
		return app.CommandResult{}, fmt.Errorf("unexpected command %q", cmd.Argv)
	}
	dir, args := cmd.Argv[2], strings.Join(cmd.Argv[3:], " ")
	g.runtime.mu.Lock()
	base, created := g.runtime.bases[dir]
	g.runtime.mu.Unlock()
	var result app.CommandResult
	switch {
	case args == "rev-parse --path-format=absolute --git-common-dir" && (created || dir == g.repositoryRoot):
		result = app.CommandResult{Stdout: []byte(g.repositoryRoot + "/.git\n")}
	case args == "rev-parse HEAD^{commit}" && created:
		result = app.CommandResult{Stdout: []byte(base + "\n")}
	default:
		return app.CommandResult{}, fmt.Errorf("unexpected git invocation %q in %s", args, dir)
	}
	if g.answer != nil {
		result = *g.answer
	}
	return result, nil
}

// linkArtifacts keeps written artifacts in memory.
type linkArtifacts struct {
	mu    sync.Mutex
	files map[string][]byte
}

func (a *linkArtifacts) WriteArtifact(_ context.Context, path string, content []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.files[path] = append([]byte(nil), content...)
	return nil
}

func (a *linkArtifacts) ReadArtifact(_ context.Context, path string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	content, ok := a.files[path]
	if !ok {
		return nil, fmt.Errorf("artifact %s: %w", path, app.ErrNotFound)
	}
	return content, nil
}

// linkIDs mints sequential identities from a block no fixture uses.
type linkIDs struct {
	mu sync.Mutex
	n  int
}

func (g *linkIDs) NewID() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return uid(8500 + g.n)
}

// linkTrust never seeds: the harness environment carries no profile, so
// no seed is ever planned.
type linkTrust struct{}

func (linkTrust) SeedWorkspaceTrust(context.Context, string, string) (app.TrustSeedOutcome, error) {
	return app.TrustSeedOutcome{}, errors.New("the assignment harness plans no trust seed")
}

// assignmentHarness is a feature run on the REAL store, taken over by a
// Controller whose external ports are the in-memory fakes above: the
// store-facing path — assignment, launch context, launch claim — is
// production code end to end.
type assignmentHarness struct {
	*featureFixture
	ctrl    *app.Controller
	runtime *linkRuntime
	handle  app.RunHandle
	// implementA and implementB are ready implement tasks; review is a
	// ready review task over linkSubjectOID.
	implementA, implementB, review identity.TaskID
}

// newAssignmentHarness seeds a running feature run (a bound, corroborable
// manager; the InitializeRun bootstrap session retired so it neither
// reconciles nor holds a slot), two ready implement tasks, an integration
// row and a ready review task, then hands the run to a fresh controller
// through ResumeFeature's lease takeover — the only way a RunHandle is
// minted outside the app package.
func newAssignmentHarness(t *testing.T) *assignmentHarness {
	t.Helper()
	f := newFeatureFixture(t)
	ctx := t.Context()
	now := f.clock.Now()

	f.inUOW(t, func(uow app.UnitOfWork) {
		saveSession(t, uow, f.spec.SessionID, func(v run.Session) (run.Session, error) { return v.Terminate(now) })
	})
	if err := f.store.ClaimLaunch(ctx, app.LaunchClaim{
		IncarnationID: f.ManagerIncarnation, RunID: f.spec.RunID, SessionID: f.ManagerID,
		Executable: linkHarnessPath, ArgvDigest: "manager-argv-digest", PID: linkManagerPID,
	}); err != nil {
		t.Fatalf("ClaimLaunch(manager): %v", err)
	}
	f.inUOW(t, func(uow app.UnitOfWork) {
		if err := uow.LaunchClaims().Settle(ctx, f.ManagerIncarnation, app.LaunchClaimSettlement{
			State: app.LaunchClaimExeced, PaneID: linkManagerPane, PID: linkManagerPID,
			Executable: linkHarnessPath, ArgvMarker: f.spec.RunID.String(), At: now,
		}); err != nil {
			t.Fatalf("settle manager claim: %v", err)
		}
	})

	h := &assignmentHarness{featureFixture: f}
	h.implementA = f.createFeatureTask(t, 8401, 2, run.TaskReady)
	h.implementB = f.createFeatureTask(t, 8402, 3, run.TaskReady)
	// The review assignment's diff scope needs a recorded integration; any
	// of the run's tasks carries it.
	rawExec(t, f.store, `INSERT INTO results (id, attempt_id, commit_oid, summary, content_digest, accepted, submitted_at)
		VALUES (?, ?, 'source-oid', 'done', 'digest-link', 1, '2026-09-14T10:00:00.000000000Z')`, uid(8403), f.spec.AttemptID.String())
	rawExec(t, f.store, `INSERT INTO integrations (id, run_id, task_id, result_id, source_commit_oid, premerge_head_oid, merge_commit_oid, state, operation_id, revision, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'source-oid', 'premerge-oid', ?, 'integrated', NULL, 2, '2026-09-14T10:00:00.000000000Z', '2026-09-14T10:05:00.000000000Z')`,
		uid(8404), f.spec.RunID.String(), f.spec.TaskID.String(), uid(8403), linkSubjectOID)
	h.review = identity.TaskID(uid(8405))
	f.inUOW(t, func(uow app.UnitOfWork) {
		review := run.NewReviewTask(h.review, f.spec.RunID, 4, linkSubjectOID, linkSubjectTreeOID, now)
		if _, err := workflowRepos(t, uow).TaskIndex().Create(ctx, review); err != nil {
			t.Fatalf("create review task: %v", err)
		}
	})
	if err := f.store.ReleaseLease(ctx, f.lease); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}

	managerArgv := []string{linkHarnessPath, "manager prompt for run " + f.spec.RunID.String()}
	h.runtime = &linkRuntime{
		bases: map[string]string{},
		manager: app.PaneProcess{ShellPID: 1, ForegroundGroupID: linkManagerPID, Foreground: []app.ProcessInfo{{
			PID: linkManagerPID, Name: "claude", Argv0: "claude",
			Argv: managerArgv, Cmdline: strings.Join(managerArgv, " "),
		}}},
	}
	h.ctrl = &app.Controller{
		Store: f.store, Read: f.store, Submissions: f.store,
		Messages: f.store, Plan: f.store, Reviews: f.store,
		Runtime:       h.runtime,
		Artifacts:     &linkArtifacts{files: map[string][]byte{}},
		Clock:         f.clock,
		IDs:           &linkIDs{},
		Commands:      linkGit{repositoryRoot: f.spec.RepositoryRoot, runtime: h.runtime},
		Trust:         linkTrust{},
		GitExecutable: linkGitPath,
	}
	resumed, handle, err := h.ctrl.ResumeFeature(ctx, app.ResumeFeatureRequest{
		RunID: f.spec.RunID.String(), ControllerID: "controller-b", HOPPath: linkHOPPath,
	})
	if err != nil {
		t.Fatalf("ResumeFeature: %v", err)
	}
	if resumed.Outcome != "resumed" {
		t.Fatalf("ResumeFeature = %+v, want resumed (the harness needs a running run)", resumed)
	}
	h.handle = handle
	return h
}

// assign runs the production scheduling step with the run's frozen
// defaults and room for all three ready tasks.
func (h *assignmentHarness) assign(t *testing.T) map[identity.TaskID]app.AssignedTask {
	t.Helper()
	opts, err := h.ctrl.AssignmentDefaults(t.Context(), h.handle)
	if err != nil {
		t.Fatalf("AssignmentDefaults: %v", err)
	}
	opts.MaxWorkers = 3
	opts.HOPPath = linkHOPPath
	opts.IntegrationHeadCommitOID = linkHeadOID
	report, err := h.ctrl.AssignReadyTasks(t.Context(), h.handle, opts)
	if err != nil {
		t.Fatalf("AssignReadyTasks: %v", err)
	}
	byTask := map[identity.TaskID]app.AssignedTask{}
	for _, assigned := range report.Assigned {
		byTask[assigned.TaskID] = assigned
	}
	if len(byTask) != 3 || byTask[h.implementA].Role != run.RoleImplementer ||
		byTask[h.implementB].Role != run.RoleImplementer || byTask[h.review].Role != run.RoleReviewer {
		t.Fatalf("AssignReadyTasks assigned %+v, want both implement tasks and the review task", report.Assigned)
	}
	return byTask
}

// launchRequest is the hop launch request a session's own pane would make
// from dir, with the pane environment openChildPane recorded.
func (h *assignmentHarness) launchRequest(slc *app.SessionLaunchContext, dir string, pid int) app.SessionLaunchExecRequest {
	return app.SessionLaunchExecRequest{
		RunID: h.spec.RunID.String(), SessionID: slc.Session.ID.String(),
		HOPPath: linkHOPPath, WorkerDir: dir, PID: pid,
		Environ: []string{
			"PATH=/opt/harness",
			"HOP_STATE_DIR=" + h.spec.Snapshot.StateRoot,
			"HOP_RUN_ID=" + h.spec.RunID.String(),
			"HOP_TASK_ID=" + slc.Attempt.TaskID.String(),
			"HOP_ATTEMPT_ID=" + slc.AttemptID.String(),
			"HOP_INCARNATION_ID=" + slc.IncarnationID.String(),
			"HOP_SESSION_ID=" + slc.Session.ID.String(),
			"HOP_ROLE=" + string(slc.Session.Role),
		},
		ResolvePath: func(path string) (string, error) { return path, nil },
		LookupExecutable: func(name, _ string) (string, error) {
			if name != "claude" {
				return "", fmt.Errorf("unexpected executable %q", name)
			}
			return linkHarnessPath, nil
		},
	}
}

// TestAssignedWorktreesLinkTheirAttempts is the feature-mode worktree
// linkage end to end on the real store: AssignReadyTasks records every
// attempt's worktree row with its attempt and verified base (the
// integration head for an implementer, the frozen subject for the
// reviewer), every Claude child carries its own pre-assigned native
// reference, the session launch context resolves each session's OWN path
// out of a run holding three rows, and hop launch's exec boundary accepts
// each session from its own worktree — composing `--session-id <ref>` —
// and refuses it from a sibling's.
func TestAssignedWorktreesLinkTheirAttempts(t *testing.T) {
	h := newAssignmentHarness(t)
	assigned := h.assign(t)
	ctx := t.Context()

	contexts := map[identity.TaskID]app.SessionLaunchContext{}
	paths := map[string]identity.TaskID{}
	refs := map[string]identity.TaskID{}
	for taskID, a := range assigned {
		slc, err := h.store.LoadSessionLaunchContext(ctx, h.spec.RunID, a.SessionID)
		if err != nil {
			t.Fatalf("LoadSessionLaunchContext(%s): %v", a.Role, err)
		}
		// Every Claude child is created with its own pre-assigned native
		// reference, the `--session-id` its first launch needs.
		ref := slc.Session.NativeSessionRef
		if _, parseErr := identity.ParseSessionID(ref); parseErr != nil || slc.Session.NativeRefSource != run.NativeRefAssigned {
			t.Fatalf("%s native reference = %q (%s), want a pre-assigned canonical UUID", a.Role, ref, slc.Session.NativeRefSource)
		}
		if other, dup := refs[ref]; dup {
			t.Fatalf("tasks %s and %s share native reference %q", other, taskID, ref)
		}
		refs[ref] = taskID
		if slc.WorktreePath == "" || slc.WorktreePath != a.WorktreeInfo.Path {
			t.Fatalf("%s launch context worktree = %q, want its own created worktree %q", a.Role, slc.WorktreePath, a.WorktreeInfo.Path)
		}
		if other, dup := paths[slc.WorktreePath]; dup {
			t.Fatalf("tasks %s and %s resolved the same worktree %q", other, taskID, slc.WorktreePath)
		}
		paths[slc.WorktreePath] = taskID
		contexts[taskID] = slc

		wantBase := linkHeadOID
		if a.Role == run.RoleReviewer {
			wantBase = linkSubjectOID
		}
		var attemptID, baseCommit string
		if err := writeDBRow(t, h.featureFixture, `SELECT attempt_id, base_commit FROM worktrees WHERE path = ?`, a.WorktreeInfo.Path).Scan(&attemptID, &baseCommit); err != nil {
			t.Fatalf("read %s worktree row: %v", a.Role, err)
		}
		if attemptID != a.AttemptID.String() || baseCommit != wantBase {
			t.Fatalf("%s worktree row = attempt %s base %s, want attempt %s base %s", a.Role, attemptID, baseCommit, a.AttemptID, wantBase)
		}
	}

	detail, err := h.store.LoadRunStatus(ctx, h.spec.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus: %v", err)
	}
	for _, row := range detail.Tasks {
		if a, ok := assigned[row.TaskID]; ok && row.WorktreePath != a.WorktreeInfo.Path {
			t.Fatalf("status row for task %s shows worktree %q, want %q", row.TaskID, row.WorktreePath, a.WorktreeInfo.Path)
		}
	}

	t.Run("each session's launch is refused from a sibling's worktree", func(t *testing.T) {
		for taskID, slc := range contexts {
			sibling := contexts[h.implementA].WorktreePath
			if taskID == h.implementA {
				sibling = contexts[h.implementB].WorktreePath
			}
			_, err := h.ctrl.PrepareSessionLaunchExec(ctx, h.launchRequest(&slc, sibling, 7000))
			if err == nil || !strings.Contains(err.Error(), "does not resolve to the attempt's recorded worktree") {
				t.Fatalf("%s launch from a sibling worktree = %v, want the worktree disagreement refusal", slc.Session.Role, err)
			}
			if claim := h.claimOf(ctx, t, slc.IncarnationID); claim {
				t.Fatalf("%s refused launch left a claim", slc.Session.Role)
			}
		}
	})

	t.Run("each session launches from its own worktree", func(t *testing.T) {
		pid := 7100
		for _, slc := range contexts {
			pid++
			plan, err := h.ctrl.PrepareSessionLaunchExec(ctx, h.launchRequest(&slc, slc.WorktreePath, pid))
			if err != nil {
				t.Fatalf("%s launch from its own worktree: %v", slc.Session.Role, err)
			}
			if plan.IncarnationID != slc.IncarnationID.String() || len(plan.Argv) < 3 || plan.Argv[0] != linkHarnessPath ||
				plan.Argv[1] != "--session-id" || plan.Argv[2] != slc.Session.NativeSessionRef {
				t.Fatalf("%s launch plan = %+v, want the Claude first launch with its own native reference", slc.Session.Role, plan)
			}
			if !h.claimOf(ctx, t, slc.IncarnationID) {
				t.Fatalf("%s launch recorded no claim", slc.Session.Role)
			}
		}
	})
}

// TestUnlinkedWorktreeRowsRefuseTheLaunch is the refusal regression: the
// row shape every feature attempt had before the link was recorded — three
// UNLINKED rows in one run — resolves no session's worktree, and hop
// launch refuses every session fail-closed with no claim written.
func TestUnlinkedWorktreeRowsRefuseTheLaunch(t *testing.T) {
	h := newAssignmentHarness(t)
	assigned := h.assign(t)
	ctx := t.Context()
	rawExec(t, h.store, `UPDATE worktrees SET attempt_id = NULL, base_commit = NULL WHERE run_id = ?`, h.spec.RunID.String())

	for _, a := range assigned {
		slc, err := h.store.LoadSessionLaunchContext(ctx, h.spec.RunID, a.SessionID)
		if err != nil {
			t.Fatalf("LoadSessionLaunchContext(%s): %v", a.Role, err)
		}
		if slc.WorktreePath != "" {
			t.Fatalf("%s resolved %q out of several unlinked rows, want no guess", a.Role, slc.WorktreePath)
		}
		_, err = h.ctrl.PrepareSessionLaunchExec(ctx, h.launchRequest(&slc, a.WorktreeInfo.Path, 7200))
		if err == nil || !strings.Contains(err.Error(), "no recorded worktree path") {
			t.Fatalf("%s launch over unlinked rows = %v, want the missing-worktree refusal", a.Role, err)
		}
		if h.claimOf(ctx, t, slc.IncarnationID) {
			t.Fatalf("%s refused launch left a claim", a.Role)
		}
	}
}

// claimOf reports whether a launch claim row exists for incarnation.
func (h *assignmentHarness) claimOf(ctx context.Context, t *testing.T, incarnation identity.IncarnationID) bool {
	t.Helper()
	var count int
	if err := sqlite.WriteDB(h.store).QueryRowContext(ctx, `SELECT COUNT(*) FROM launch_claims WHERE incarnation_id = ?`, incarnation.String()).Scan(&count); err != nil {
		t.Fatalf("count launch claims: %v", err)
	}
	return count != 0
}

// TestWorktreeRepositoryAttemptLink pins the repository round trip under
// the assignment path: an attempt-bound row reads back its attempt and
// base commit, and a solo row keeps both NULL. The refused links (an
// unknown attempt, another run's attempt) are TestStoreVectors' shared
// worktree vectors.
func TestWorktreeRepositoryAttemptLink(t *testing.T) {
	f := newFeatureFixture(t)
	ctx := t.Context()
	task := f.createFeatureTask(t, 8601, 2, run.TaskActive)
	f.createWorkerSession(t, task, run.RoleImplementer, 8602)
	attemptID := identity.AttemptID(uid(8602))

	repositoryID := f.repositoryID(t)
	linked, err := run.NewAttemptWorktree(identity.WorktreeID(uid(8610)), repositoryID, f.spec.RunID, attemptID, linkHeadOID, "/wt/linked", "hop/r1/t2a1")
	if err != nil {
		t.Fatalf("NewAttemptWorktree: %v", err)
	}
	f.inUOW(t, func(uow app.UnitOfWork) {
		if _, createErr := uow.Worktrees().Create(ctx, linked); createErr != nil {
			t.Fatalf("create linked worktree: %v", createErr)
		}
		got, revision, getErr := uow.Worktrees().Get(ctx, linked.ID)
		if getErr != nil || revision != 1 || got != linked {
			t.Fatalf("Get(linked) = %+v rev %d, %v; want %+v rev 1", got, revision, getErr, linked)
		}
	})

	var solo run.Worktree
	f.inUOW(t, func(uow app.UnitOfWork) {
		solo = run.NewWorktree(identity.WorktreeID(uid(8611)), repositoryID, f.spec.RunID, "/wt/solo", "hop/run-1")
		if _, createErr := uow.Worktrees().Create(ctx, solo); createErr != nil {
			t.Fatalf("create solo worktree: %v", createErr)
		}
		got, _, getErr := uow.Worktrees().Get(ctx, solo.ID)
		if getErr != nil || got != solo {
			t.Fatalf("Get(solo) = %+v, %v; want %+v", got, getErr, solo)
		}
	})
	var attemptNull, baseNull bool
	if err := writeDBRow(t, f, `SELECT attempt_id IS NULL, base_commit IS NULL FROM worktrees WHERE id = ?`, solo.ID.String()).Scan(&attemptNull, &baseNull); err != nil || !attemptNull || !baseNull {
		t.Fatalf("solo row attempt/base NULL = %v/%v, %v; want both NULL", attemptNull, baseNull, err)
	}
}

// launchingClaudeChild reserves an attempt on task and creates a LAUNCHING
// Claude implementer with a pre-assigned native reference and a pending
// launch intent naming its incarnation: the state a delegated session is
// in when its pane runs hop launch.
func launchingClaudeChild(t *testing.T, f *featureFixture, task identity.TaskID, n int) (identity.SessionID, identity.IncarnationID) {
	t.Helper()
	attemptID := identity.AttemptID(uid(n))
	sessionID := identity.SessionID(uid(n + 1))
	incarnation := identity.IncarnationID(uid(n + 2))
	now := f.clock.Now()
	manager := f.managerSessionValue(t)
	f.inUOW(t, func(uow app.UnitOfWork) {
		attempt, err := run.NewAttempt(attemptID, task, 1, now)
		if err != nil {
			t.Fatalf("new attempt: %v", err)
		}
		if _, createErr := workflowRepos(t, uow).AttemptIndex().Create(t.Context(), attempt); createErr != nil {
			t.Fatalf("create attempt: %v", createErr)
		}
		session, err := run.NewChildSession(sessionID, f.spec.RunID, attemptID, run.RoleImplementer, manager, run.HarnessClaude, now)
		if err != nil {
			t.Fatalf("new child session: %v", err)
		}
		if session, err = session.AssignNativeRef(uid(n+3), run.NativeRefAssigned, now); err != nil {
			t.Fatalf("assign native reference: %v", err)
		}
		if session, err = session.Launch(now); err != nil {
			t.Fatalf("launch child session: %v", err)
		}
		if _, createErr := uow.Sessions().Create(t.Context(), session); createErr != nil {
			t.Fatalf("create child session: %v", createErr)
		}
	})
	createLaunchIntentFor(t, f, n+4, sessionID, incarnation)
	return sessionID, incarnation
}

// TestWorktreeFallbackServesOnlyALoneUnlinkedRow pins the launch
// boundary's run-wide fallback on the real store: an attempt with no row
// of its own falls back to the run's only row ONLY while that row is
// unlinked (the solo shape). A lone row linked to a sibling attempt, or a
// sibling-linked row beside an unlinked one, resolves nothing, and hop
// launch refuses the session there with no claim written.
func TestWorktreeFallbackServesOnlyALoneUnlinkedRow(t *testing.T) {
	const (
		siblingPath  = "/wt/sibling-a"
		unlinkedPath = "/wt/unlinked"
	)
	for _, tc := range []struct {
		name     string
		sibling  bool // a row linked to the sibling attempt exists
		unlinked bool // an unlinked row exists
		want     string
	}{
		{name: "lone row linked to a sibling attempt", sibling: true, want: ""},
		{name: "sibling-linked row beside an unlinked row", sibling: true, unlinked: true, want: ""},
		{name: "the run's only row, unlinked", unlinked: true, want: unlinkedPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFeatureFixture(t)
			ctx := t.Context()
			siblingTask := f.createFeatureTask(t, 9101, 2, run.TaskActive)
			f.createWorkerSession(t, siblingTask, run.RoleImplementer, 9102)
			task := f.createFeatureTask(t, 9111, 3, run.TaskActive)
			session, incarnation := launchingClaudeChild(t, f, task, 9112)
			if tc.sibling {
				f.createAttemptWorktree(t, 9121, identity.AttemptID(uid(9102)), siblingPath, "hop/r1/t2a1")
			}
			if tc.unlinked {
				f.createWorktree(t, run.NewWorktree(identity.WorktreeID(uid(9122)), f.repositoryID(t), f.spec.RunID, unlinkedPath, "hop/run-1"))
			}

			slc, err := f.store.LoadSessionLaunchContext(ctx, f.spec.RunID, session)
			if err != nil {
				t.Fatalf("LoadSessionLaunchContext: %v", err)
			}
			if slc.WorktreePath != tc.want {
				t.Fatalf("worktree path = %q, want %q", slc.WorktreePath, tc.want)
			}

			// Launch preparation needs only the store and the trust port.
			h := &assignmentHarness{featureFixture: f, ctrl: &app.Controller{
				Read: f.store, Submissions: f.store, Clock: f.clock, Trust: linkTrust{},
			}}
			dir := siblingPath
			if tc.want != "" {
				dir = tc.want
			}
			plan, err := h.ctrl.PrepareSessionLaunchExec(ctx, h.launchRequest(&slc, dir, 7400))
			if tc.want == "" {
				if err == nil || !strings.Contains(err.Error(), "no recorded worktree path") {
					t.Fatalf("launch from %s = %+v, %v; want the missing-worktree refusal", dir, plan, err)
				}
				if h.claimOf(ctx, t, incarnation) {
					t.Fatal("a refused launch left a claim")
				}
				return
			}
			if err != nil || plan.IncarnationID != incarnation.String() || !h.claimOf(ctx, t, incarnation) {
				t.Fatalf("solo-fallback launch = %+v, %v; want accepted with a claim", plan, err)
			}
		})
	}
}

// TestLinkGitCaptureContract runs the shared capture contract
// (internal/testsupport/runnervectors, which the real process runner also
// runs) through linkGit, and pins its own common-directory answer under a
// refused negative bound, a one-byte bound and its exact length.
func TestLinkGitCaptureContract(t *testing.T) {
	argv := []string{linkGitPath, "-C", "/fixture/repo", "rev-parse", "--path-format=absolute", "--git-common-dir"}
	for _, v := range runnervectors.CaptureVectors() {
		t.Run(v.Name, func(t *testing.T) {
			answer := v.Answer()
			g := linkGit{repositoryRoot: "/fixture/repo", runtime: &linkRuntime{}, answer: &answer}
			result, err := g.Run(context.Background(), app.Command{Argv: argv, MaxOutputBytes: v.MaxOutputBytes})
			if checkErr := v.Check(result, err); checkErr != nil {
				t.Error(checkErr)
			}
		})
	}

	g := linkGit{repositoryRoot: "/fixture/repo", runtime: &linkRuntime{}}
	const common = "/fixture/repo/.git\n"
	if _, err := g.Run(context.Background(), app.Command{Argv: argv, MaxOutputBytes: -1}); err == nil {
		t.Error("a negative bound was accepted; the real runner refuses it")
	}
	for bound, want := range map[int]struct {
		stdout    string
		truncated bool
	}{
		0:               {common, false},
		1:               {"/", true},
		len(common) - 1: {common[:len(common)-1], true},
		len(common):     {common, false},
	} {
		result, err := g.Run(context.Background(), app.Command{Argv: argv, MaxOutputBytes: bound})
		if err != nil || string(result.Stdout) != want.stdout || result.StdoutTruncated != want.truncated {
			t.Errorf("bound %d: stdout %q, truncated %v, %v; want %q, %v", bound, result.Stdout, result.StdoutTruncated, err, want.stdout, want.truncated)
		}
	}
}
