package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

const ebSessionID = "77777777-7777-4777-8777-777777777777"

// mustSessionID / mustAttemptID parse fixed test UUIDs, failing the test
// on the impossible parse error instead of blanking it.
func mustSessionID(t *testing.T, raw string) identity.SessionID {
	t.Helper()
	id, err := identity.ParseSessionID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustAttemptID(t *testing.T, raw string) identity.AttemptID {
	t.Helper()
	id, err := identity.ParseAttemptID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// ebSessionReadStub extends the plain exec-boundary ReadStore stub with
// the WorkflowReadStore surface PrepareSessionLaunchExec asserts to; the
// two messaging lookups stay unexercised.
type ebSessionReadStub struct {
	ebReadStub
	session  SessionLaunchContext
	sessionE error
	detail   RunDetail
	detailE  error
}

func (s *ebSessionReadStub) LoadRunStatus(context.Context, identity.RunID) (RunDetail, error) {
	s.record("LoadRunStatus")
	return s.detail, s.detailE
}

func (s *ebSessionReadStub) LoadSessionLaunchContext(context.Context, identity.RunID, identity.SessionID) (SessionLaunchContext, error) {
	s.record("LoadSessionLaunchContext")
	return s.session, s.sessionE
}

func (*ebSessionReadStub) LoadMessagingContext(context.Context, identity.SessionID) (MessagingContext, error) {
	return MessagingContext{}, errors.New("unexpected LoadMessagingContext")
}

func (*ebSessionReadStub) LoadMessageDetail(context.Context, identity.RunID, identity.MessageID) (MessageDetail, error) {
	return MessageDetail{}, errors.New("unexpected LoadMessageDetail")
}

// ebSessionContext builds a valid first-launch session context for role.
func ebSessionContext(t *testing.T, role run.Role) SessionLaunchContext {
	t.Helper()
	sessionID, err := identity.ParseSessionID(ebSessionID)
	if err != nil {
		t.Fatal(err)
	}
	runID, err := identity.ParseRunID(ebRunID)
	if err != nil {
		t.Fatal(err)
	}
	incarnationID, err := identity.ParseIncarnationID(ebIncarnationID)
	if err != nil {
		t.Fatal(err)
	}
	slc := SessionLaunchContext{
		Snapshot: RunSnapshot{
			EnvPolicy:      EnvPolicy{Version: EnvPolicyVersion1, Harness: HarnessClaude},
			Harness:        HarnessClaude,
			StateRoot:      "/state",
			AssignmentPath: "/state/runs/" + ebRunID + "/artifacts/assignment.md",
		},
		Harness: run.HarnessClaude,
		Session: run.Session{
			ID: sessionID, RunID: runID, Role: role, Harness: run.HarnessClaude,
			NativeSessionRef: ebNativeRef, State: run.SessionLaunching,
		},
		IncarnationID: incarnationID,
	}
	if role == run.RoleManager {
		slc.Snapshot.Workflow = WorkflowSnapshot{
			Mode:            "feature",
			ManagerRolePath: "/state/runs/" + ebRunID + "/artifacts/roles/manager.md",
		}
		return slc
	}
	attemptID, err := identity.ParseAttemptID(ebAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := identity.ParseTaskID(ebTaskID)
	if err != nil {
		t.Fatal(err)
	}
	slc.AttemptID = attemptID
	slc.Attempt = run.Attempt{ID: attemptID, TaskID: taskID, State: run.AttemptLaunching}
	slc.Session.AttemptID = attemptID
	slc.WorktreePath = "/private/var/worktrees/hop-run-1"
	if role != run.RoleWorker {
		slc.Snapshot.Workflow = WorkflowSnapshot{Mode: "feature"}
	}
	return slc
}

// ebSessionEnviron is a session-addressed pane environment for role:
// the Phase 2 five plus HOP_SESSION_ID and HOP_ROLE, minus the attempt
// pair for the manager.
func ebSessionEnviron(role run.Role) []string {
	environ := []string{
		"PATH=/usr/bin:/bin",
		"ANTHROPIC_API_KEY=secret-value",
		"HOP_STATE_DIR=/state",
		"HOP_RUN_ID=" + ebRunID,
		"HOP_INCARNATION_ID=" + ebIncarnationID,
		"HOP_SESSION_ID=" + ebSessionID,
		"HOP_ROLE=" + string(role),
	}
	if role != run.RoleManager {
		environ = append(environ, "HOP_TASK_ID="+ebTaskID, "HOP_ATTEMPT_ID="+ebAttemptID)
	}
	return environ
}

func ebSessionRequest() SessionLaunchExecRequest {
	return SessionLaunchExecRequest{
		RunID:            ebRunID,
		SessionID:        ebSessionID,
		HOPPath:          "/usr/local/bin/hop",
		WorkerDir:        "/var/worktrees/hop-run-1",
		PID:              4242,
		ResolvePath:      ebResolve,
		LookupExecutable: ebLookup(nil),
	}
}

func TestPrepareSessionLaunchExec(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	newController := func(read ReadStore, subs *ebSubmissionStub, trust *ebTrustStub) *Controller {
		c := &Controller{Read: read, Submissions: subs, Clock: ebClock{now: now}}
		if trust != nil {
			c.Trust = trust
		}
		return c
	}

	attemptPath := "/state/runs/" + ebRunID + "/attempts/" + ebAttemptID + "/assignment.md"
	cribPath := "/state/runs/" + ebRunID + "/artifacts/worker-protocol.md"

	t.Run("implementer first launch: Phase 2 prompt shape at the per-attempt assignment", func(t *testing.T) {
		read := &ebSessionReadStub{session: ebSessionContext(t, run.RoleImplementer)}
		subs := &ebSubmissionStub{}
		req := ebSessionRequest()
		req.Environ = ebSessionEnviron(run.RoleImplementer)

		plan, err := newController(read, subs, nil).PrepareSessionLaunchExec(context.Background(), req)
		if err != nil {
			t.Fatalf("PrepareSessionLaunchExec: %v", err)
		}

		wantPrompt := renderInitialPrompt(attemptPath, "/usr/local/bin/hop")
		wantArgv := []string{"/resolved/claude", "--session-id", ebNativeRef, wantPrompt}
		if len(plan.Argv) != len(wantArgv) {
			t.Fatalf("argv = %q", plan.Argv)
		}
		for i := range wantArgv {
			if plan.Argv[i] != wantArgv[i] {
				t.Fatalf("argv[%d] = %q, want %q", i, plan.Argv[i], wantArgv[i])
			}
		}
		if len(subs.claims) != 1 {
			t.Fatalf("claims = %d", len(subs.claims))
		}
		claim := subs.claims[0]
		if claim.SessionID.String() != ebSessionID || claim.AttemptID.String() != ebAttemptID {
			t.Errorf("claim keys = session %s attempt %s", claim.SessionID, claim.AttemptID)
		}
		if claim.Executable != "/resolved/claude" || claim.ArgvDigest != launchArgvDigest(plan.Argv) || claim.PID != 4242 {
			t.Errorf("claim identity = %+v", claim)
		}
		if !strings.Contains(claim.SeedEvidence, "not seeded") {
			t.Errorf("seed evidence = %q, want the no-seeder-plan not-seeded outcome", claim.SeedEvidence)
		}
		if strings.Contains(strings.Join(plan.Env, "\n"), "ANTHROPIC_API_KEY") {
			t.Errorf("sanitized env still carries a strip-matrix variable")
		}
	})

	t.Run("reviewer first launch: reviewer prompt shape at the same per-attempt path", func(t *testing.T) {
		read := &ebSessionReadStub{session: ebSessionContext(t, run.RoleReviewer)}
		subs := &ebSubmissionStub{}
		req := ebSessionRequest()
		req.Environ = ebSessionEnviron(run.RoleReviewer)

		plan, err := newController(read, subs, nil).PrepareSessionLaunchExec(context.Background(), req)
		if err != nil {
			t.Fatalf("PrepareSessionLaunchExec: %v", err)
		}
		if want := renderReviewerInitialPrompt(attemptPath, "/usr/local/bin/hop"); plan.Argv[3] != want {
			t.Errorf("prompt = %q, want the reviewer shape", plan.Argv[3])
		}
	})

	t.Run("manager first launch: three frozen artifacts, repository-root cross-check, attempt-less claim", func(t *testing.T) {
		read := &ebSessionReadStub{session: ebSessionContext(t, run.RoleManager)}
		read.frozen = FrozenRun{RepositoryRoot: "/var/repos/project", Snapshot: read.session.Snapshot}
		subs := &ebSubmissionStub{}
		req := ebSessionRequest()
		req.WorkerDir = "/var/repos/project"
		req.Environ = ebSessionEnviron(run.RoleManager)

		plan, err := newController(read, subs, nil).PrepareSessionLaunchExec(context.Background(), req)
		if err != nil {
			t.Fatalf("PrepareSessionLaunchExec: %v", err)
		}
		want := renderManagerInitialPrompt(read.session.Snapshot.AssignmentPath, read.session.Snapshot.Workflow.ManagerRolePath, cribPath, "/usr/local/bin/hop")
		if plan.Argv[3] != want {
			t.Errorf("prompt = %q, want the manager shape", plan.Argv[3])
		}
		if subs.claims[0].AttemptID != "" {
			t.Errorf("manager claim carries an attempt: %q", subs.claims[0].AttemptID)
		}
		if subs.claims[0].SessionID.String() != ebSessionID {
			t.Errorf("manager claim session = %q", subs.claims[0].SessionID)
		}
	})

	t.Run("worker relaunch: the Phase 2 cold-resume argv byte for byte", func(t *testing.T) {
		slc := ebSessionContext(t, run.RoleWorker)
		slc.Relaunch = true
		read := &ebSessionReadStub{session: slc}
		subs := &ebSubmissionStub{}
		req := ebSessionRequest()
		req.Environ = ebSessionEnviron(run.RoleWorker)

		plan, err := newController(read, subs, nil).PrepareSessionLaunchExec(context.Background(), req)
		if err != nil {
			t.Fatalf("PrepareSessionLaunchExec: %v", err)
		}
		wantPrompt := renderContinuationPrompt(slc.Snapshot.AssignmentPath, "/usr/local/bin/hop")
		wantArgv := []string{"/resolved/claude", "--resume", ebNativeRef, wantPrompt}
		for i := range wantArgv {
			if plan.Argv[i] != wantArgv[i] {
				t.Fatalf("argv[%d] = %q, want %q", i, plan.Argv[i], wantArgv[i])
			}
		}
	})

	t.Run("manager relaunch: --resume adjacent to the reference, manager continuation trailing", func(t *testing.T) {
		slc := ebSessionContext(t, run.RoleManager)
		slc.Relaunch = true
		read := &ebSessionReadStub{session: slc}
		read.frozen = FrozenRun{RepositoryRoot: "/var/repos/project", Snapshot: slc.Snapshot}
		subs := &ebSubmissionStub{}
		req := ebSessionRequest()
		req.WorkerDir = "/var/repos/project"
		req.Environ = ebSessionEnviron(run.RoleManager)

		plan, err := newController(read, subs, nil).PrepareSessionLaunchExec(context.Background(), req)
		if err != nil {
			t.Fatalf("PrepareSessionLaunchExec: %v", err)
		}
		if plan.Argv[1] != "--resume" || plan.Argv[2] != ebNativeRef {
			t.Fatalf("argv = %q, want --resume immediately followed by the reference", plan.Argv)
		}
		want := renderManagerContinuationPrompt(slc.Snapshot.AssignmentPath, slc.Snapshot.Workflow.ManagerRolePath, cribPath, "/usr/local/bin/hop")
		if plan.Argv[3] != want {
			t.Errorf("continuation = %q", plan.Argv[3])
		}
	})

	t.Run("solo shim: attempt resolves to the current session; Phase 2 env set suffices", func(t *testing.T) {
		var calls []string
		slc := ebSessionContext(t, run.RoleWorker)
		read := &ebSessionReadStub{session: slc}
		read.calls = &calls
		sessionID, parseErr := identity.ParseSessionID(ebSessionID)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		attemptID, parseErr := identity.ParseAttemptID(ebAttemptID)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		read.detail = RunDetail{AttemptID: attemptID, SessionID: sessionID}
		subs := &ebSubmissionStub{}
		req := ebSessionRequest()
		req.SessionID = ""
		req.AttemptID = ebAttemptID
		req.Environ = ebEnviron() // the Phase 2 pane set: no HOP_SESSION_ID, no HOP_ROLE

		plan, err := newController(read, subs, nil).PrepareSessionLaunchExec(context.Background(), req)
		if err != nil {
			t.Fatalf("PrepareSessionLaunchExec: %v", err)
		}
		if len(calls) < 2 || calls[0] != "LoadRunStatus" || calls[1] != "LoadSessionLaunchContext" {
			t.Fatalf("call order = %v", calls)
		}
		wantPrompt := renderInitialPrompt(slc.Snapshot.AssignmentPath, "/usr/local/bin/hop")
		if plan.Argv[3] != wantPrompt {
			t.Errorf("shim prompt = %q, want the Phase 2 solo shape at the frozen assignment path", plan.Argv[3])
		}
		if subs.claims[0].SessionID != sessionID {
			t.Errorf("shim claim session = %q", subs.claims[0].SessionID)
		}
	})

	t.Run("solo shim: a present but disagreeing HOP_SESSION_ID refuses", func(t *testing.T) {
		slc := ebSessionContext(t, run.RoleWorker)
		read := &ebSessionReadStub{session: slc}
		sessionID := mustSessionID(t, ebSessionID)
		attemptID := mustAttemptID(t, ebAttemptID)
		read.detail = RunDetail{AttemptID: attemptID, SessionID: sessionID}
		subs := &ebSubmissionStub{}
		req := ebSessionRequest()
		req.SessionID = ""
		req.AttemptID = ebAttemptID
		req.Environ = append(ebEnviron(), "HOP_SESSION_ID="+ebNativeRef)

		_, err := newController(read, subs, nil).PrepareSessionLaunchExec(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), "HOP_SESSION_ID does not agree") {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), ebNativeRef) {
			t.Errorf("error echoes an environment value: %v", err)
		}
		if len(subs.claims) != 0 {
			t.Errorf("a refused launch wrote a claim")
		}
	})

	shimRefusals := []struct {
		name    string
		detail  RunDetail
		wantErr string
	}{
		{
			name:    "shim refuses a non-current attempt",
			detail:  RunDetail{},
			wantErr: "not the run's current attempt",
		},
		{
			name: "shim refuses an attempt with no current session",
			detail: func() RunDetail {
				attemptID := mustAttemptID(t, ebAttemptID)
				return RunDetail{AttemptID: attemptID}
			}(),
			wantErr: "no current session",
		},
	}
	for _, tc := range shimRefusals {
		t.Run(tc.name, func(t *testing.T) {
			read := &ebSessionReadStub{session: ebSessionContext(t, run.RoleWorker)}
			read.detail = tc.detail
			subs := &ebSubmissionStub{}
			req := ebSessionRequest()
			req.SessionID = ""
			req.AttemptID = ebAttemptID
			req.Environ = ebEnviron()

			_, err := newController(read, subs, nil).PrepareSessionLaunchExec(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if len(subs.claims) != 0 {
				t.Errorf("a refused shim launch wrote a claim")
			}
		})
	}

	t.Run("shim refuses a session bound to a different attempt", func(t *testing.T) {
		slc := ebSessionContext(t, run.RoleWorker)
		other := mustAttemptID(t, ebOperationID) // any distinct UUID
		slc.AttemptID = other
		slc.Attempt.ID = other
		read := &ebSessionReadStub{session: slc}
		sessionID := mustSessionID(t, ebSessionID)
		attemptID := mustAttemptID(t, ebAttemptID)
		read.detail = RunDetail{AttemptID: attemptID, SessionID: sessionID}
		subs := &ebSubmissionStub{}
		req := ebSessionRequest()
		req.SessionID = ""
		req.AttemptID = ebAttemptID
		req.Environ = ebEnviron()

		_, err := newController(read, subs, nil).PrepareSessionLaunchExec(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), "not bound to the requested attempt") {
			t.Fatalf("err = %v", err)
		}
		if len(subs.claims) != 0 {
			t.Errorf("a refused shim launch wrote a claim")
		}
	})

	t.Run("plain ReadStore fails closed with the typed sentinel", func(t *testing.T) {
		read := &ebReadStub{} // deliberately not a WorkflowReadStore
		subs := &ebSubmissionStub{}
		req := ebSessionRequest()
		req.Environ = ebSessionEnviron(run.RoleWorker)

		_, err := newController(read, subs, nil).PrepareSessionLaunchExec(context.Background(), req)
		if !errors.Is(err, ErrWorkflowReadStoreUnsupported) {
			t.Fatalf("err = %v, want ErrWorkflowReadStoreUnsupported", err)
		}
		if len(subs.claims) != 0 {
			t.Errorf("a refused launch wrote a claim")
		}
	})

	t.Run("trust seed runs before the claim and a write failure refuses claimless", func(t *testing.T) {
		var order []string
		slc := ebSessionContext(t, run.RoleImplementer)
		slc.Snapshot.EnvPolicy.ProfileDir = "/profiles/claude-alt"
		read := &ebSessionReadStub{session: slc}
		subs := &ebSubmissionStub{calls: &order}
		trust := &ebTrustStub{outcome: TrustSeedOutcome{Seeded: true}, names: &order}
		req := ebSessionRequest()
		req.Environ = ebSessionEnviron(run.RoleImplementer)

		if _, err := newController(read, subs, trust).PrepareSessionLaunchExec(context.Background(), req); err != nil {
			t.Fatalf("PrepareSessionLaunchExec: %v", err)
		}
		if len(order) != 2 || order[0] != "SeedWorkspaceTrust" || order[1] != "ClaimLaunch" {
			t.Fatalf("order = %v, want the seed immediately before the claim", order)
		}

		failing := &ebTrustStub{err: errors.New("locked write failed")}
		subs2 := &ebSubmissionStub{}
		if _, err := newController(&ebSessionReadStub{session: slc}, subs2, failing).PrepareSessionLaunchExec(context.Background(), req); err == nil {
			t.Fatal("a failed seeding write did not refuse the launch")
		}
		if len(subs2.claims) != 0 {
			t.Errorf("a refused launch wrote a claim")
		}
	})

	refusals := []struct {
		name    string
		role    run.Role
		mutate  func(req *SessionLaunchExecRequest, read *ebSessionReadStub)
		wantErr string
	}{
		{
			name: "both address forms supplied",
			role: run.RoleWorker,
			mutate: func(req *SessionLaunchExecRequest, _ *ebSessionReadStub) {
				req.AttemptID = ebAttemptID
			},
			wantErr: "mutually exclusive",
		},
		{
			name: "neither address form supplied",
			role: run.RoleWorker,
			mutate: func(req *SessionLaunchExecRequest, _ *ebSessionReadStub) {
				req.SessionID = ""
			},
			wantErr: "exactly one of the session and attempt addresses",
		},
		{
			name: "missing HOP_SESSION_ID on the session-addressed path",
			role: run.RoleImplementer,
			mutate: func(req *SessionLaunchExecRequest, _ *ebSessionReadStub) {
				req.Environ = environWithout(req.Environ, "HOP_SESSION_ID")
			},
			wantErr: "HOP_SESSION_ID is not set",
		},
		{
			name: "disagreeing HOP_ROLE",
			role: run.RoleImplementer,
			mutate: func(req *SessionLaunchExecRequest, _ *ebSessionReadStub) {
				req.Environ = append(environWithout(req.Environ, "HOP_ROLE"), "HOP_ROLE=reviewer")
			},
			wantErr: "HOP_ROLE does not agree",
		},
		{
			name: "manager pane carrying HOP_TASK_ID",
			role: run.RoleManager,
			mutate: func(req *SessionLaunchExecRequest, _ *ebSessionReadStub) {
				req.Environ = append(req.Environ, "HOP_TASK_ID="+ebTaskID)
			},
			wantErr: "HOP_TASK_ID is set but this session has no attempt",
		},
		{
			name: "missing HOP_ATTEMPT_ID for an attempt-bearing session",
			role: run.RoleImplementer,
			mutate: func(req *SessionLaunchExecRequest, _ *ebSessionReadStub) {
				req.Environ = environWithout(req.Environ, "HOP_ATTEMPT_ID")
			},
			wantErr: "HOP_ATTEMPT_ID is not set",
		},
		{
			name: "stop requested",
			role: run.RoleImplementer,
			mutate: func(_ *SessionLaunchExecRequest, read *ebSessionReadStub) {
				read.session.StopRequested = true
			},
			wantErr: "stop request",
		},
		{
			name: "session not launching",
			role: run.RoleImplementer,
			mutate: func(_ *SessionLaunchExecRequest, read *ebSessionReadStub) {
				read.session.Session.State = run.SessionActive
			},
			wantErr: "not launching; this launcher invocation is not current",
		},
		{
			name: "existing claim under a different pid",
			role: run.RoleImplementer,
			mutate: func(_ *SessionLaunchExecRequest, read *ebSessionReadStub) {
				read.session.Claim = &LaunchClaim{PID: 9999, State: LaunchClaimExecPending}
			},
			wantErr: "different pid",
		},
		{
			name: "existing settled claim",
			role: run.RoleImplementer,
			mutate: func(_ *SessionLaunchExecRequest, read *ebSessionReadStub) {
				read.session.Claim = &LaunchClaim{PID: 4242, State: LaunchClaimExeced}
			},
			wantErr: "already settled",
		},
		{
			name: "existing claim recording a different argv",
			role: run.RoleImplementer,
			mutate: func(_ *SessionLaunchExecRequest, read *ebSessionReadStub) {
				read.session.Claim = &LaunchClaim{PID: 4242, State: LaunchClaimExecPending, Executable: "/resolved/claude", ArgvDigest: "different"}
			},
			wantErr: "never rewritten",
		},
		{
			name: "relaunch for a non-claude harness",
			role: run.RoleImplementer,
			mutate: func(_ *SessionLaunchExecRequest, read *ebSessionReadStub) {
				read.session.Relaunch = true
				read.session.Harness = run.HarnessCodex
				read.session.Session.Harness = run.HarnessCodex
			},
			wantErr: "cold resume for harness",
		},
		{
			name: "missing native reference",
			role: run.RoleImplementer,
			mutate: func(_ *SessionLaunchExecRequest, read *ebSessionReadStub) {
				read.session.Session.NativeSessionRef = ""
			},
			wantErr: "no pre-assigned native session reference",
		},
		{
			name: "worker directory does not resolve to the recorded worktree",
			role: run.RoleImplementer,
			mutate: func(req *SessionLaunchExecRequest, _ *ebSessionReadStub) {
				req.WorkerDir = "/var/somewhere-else"
			},
			wantErr: "does not resolve to the attempt's recorded worktree",
		},
		{
			name: "attempt session with no recorded worktree path",
			role: run.RoleImplementer,
			mutate: func(_ *SessionLaunchExecRequest, read *ebSessionReadStub) {
				read.session.WorktreePath = ""
			},
			wantErr: "no recorded worktree path",
		},
		{
			name: "manager directory does not resolve to the repository root",
			role: run.RoleManager,
			mutate: func(req *SessionLaunchExecRequest, read *ebSessionReadStub) {
				read.frozen = FrozenRun{RepositoryRoot: "/var/repos/project"}
				req.WorkerDir = "/var/repos/other"
			},
			wantErr: "does not resolve to the run's recorded repository root",
		},
		{
			name: "manager role artifact path missing",
			role: run.RoleManager,
			mutate: func(req *SessionLaunchExecRequest, read *ebSessionReadStub) {
				read.session.Snapshot.Workflow.ManagerRolePath = ""
				read.frozen = FrozenRun{RepositoryRoot: "/var/repos/project"}
				req.WorkerDir = "/var/repos/project"
			},
			wantErr: "manager role artifact path is missing",
		},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			read := &ebSessionReadStub{session: ebSessionContext(t, tc.role)}
			if tc.role == run.RoleManager {
				read.frozen = FrozenRun{RepositoryRoot: "/var/repos/project"}
			}
			subs := &ebSubmissionStub{}
			req := ebSessionRequest()
			if tc.role == run.RoleManager {
				req.WorkerDir = "/var/repos/project"
			}
			req.Environ = ebSessionEnviron(tc.role)
			tc.mutate(&req, read)

			_, err := newController(read, subs, nil).PrepareSessionLaunchExec(context.Background(), req)

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if len(subs.claims) != 0 {
				t.Errorf("a refused launch wrote a claim: %+v", subs.claims)
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Errorf("error echoes an environment value: %v", err)
			}
		})
	}
}

// TestGoldenRolePrompts pins every launch prompt's exact bytes against
// retyped golden literals — the solo worker shapes the test/integration
// fixture mirrors reproduce byte for byte, and the Phase 3 role shapes —
// so a template drift fails here before it can desynchronize a mirror.
func TestGoldenRolePrompts(t *testing.T) {
	const (
		a   = "/state/runs/r/artifacts/assignment.md"
		ro  = "/state/runs/r/artifacts/roles/manager.md"
		cr  = "/state/runs/r/artifacts/worker-protocol.md"
		hop = "/usr/local/bin/hop"
	)
	golden := []struct {
		name string
		got  string
		want string
	}{
		{
			"worker initial (Phase 2, frozen for the fixture mirrors)",
			renderInitialPrompt(a, hop),
			"Read your assignment at " + a + " and complete it. " +
				"When your work is committed, submit it by running: " + hop + " result submit --summary \"<one-line summary>\" --commit <commit-oid>. " +
				"If the first output line begins with \"transient\", wait briefly and run the exact same command again.",
		},
		{
			"worker continuation (Phase 2, frozen for the fixture mirrors)",
			renderContinuationPrompt(a, hop),
			"You were relaunched after an interruption; your restored session may show earlier, unfinished work. " +
				"Re-read your assignment at " + a + " and continue it. " +
				"When your work is committed, submit it by running: " + hop + " result submit --summary \"<one-line summary>\" --commit <commit-oid>. " +
				"If the first output line begins with \"transient\", wait briefly and run the exact same command again.",
		},
		{
			"reviewer initial",
			renderReviewerInitialPrompt(a, hop),
			"Read your review assignment at " + a + " and evaluate the frozen subject it names. " +
				"When your review is complete, submit your verdict by running: " + hop + " review submit --verdict <approve|reject> --subject <commit-oid> --reasons-file <absolute path>. " +
				"If the first output line begins with \"transient\", wait briefly and run the exact same command again.",
		},
		{
			"reviewer continuation",
			renderReviewerContinuationPrompt(a, hop),
			"You were relaunched after an interruption; your restored session may show earlier, unfinished work. " +
				"Re-read your review assignment at " + a + " and continue it. " +
				"When your review is complete, submit your verdict by running: " + hop + " review submit --verdict <approve|reject> --subject <commit-oid> --reasons-file <absolute path>. " +
				"If the first output line begins with \"transient\", wait briefly and run the exact same command again.",
		},
		{
			"manager initial",
			renderManagerInitialPrompt(a, ro, cr, hop),
			"You are this run's manager. Read your assignment at " + a + ", your role instructions at " + ro + " and the worker protocol reference at " + cr + " before doing anything else; together they are your complete instructions. " +
				"Coordinate the run only through the hop verbs the protocol reference quotes, and poll for messages by running: " + hop + " msg wait.",
		},
		{
			"manager continuation",
			renderManagerContinuationPrompt(a, ro, cr, hop),
			"You were relaunched after an interruption; your restored session may show earlier, unfinished work. " +
				"Re-read your assignment at " + a + ", your role instructions at " + ro + " and the worker protocol reference at " + cr + ", then continue coordinating the run. " +
				"Poll for messages by running: " + hop + " msg wait.",
		},
	}
	for _, tc := range golden {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("rendered:\n%s\ngolden:\n%s", tc.got, tc.want)
			}
		})
	}
}
