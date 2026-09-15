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

// Fixed identities of the exec-boundary tests.
const (
	ebRunID         = "11111111-1111-4111-8111-111111111111"
	ebTaskID        = "22222222-2222-4222-8222-222222222222"
	ebAttemptID     = "33333333-3333-4333-8333-333333333333"
	ebIncarnationID = "44444444-4444-4444-8444-444444444444"
	ebNativeRef     = "55555555-5555-4555-8555-555555555555"
	ebOperationID   = "66666666-6666-4666-8666-666666666666"
)

// ebReadStub is a minimal ReadStore serving one launch context and one
// check execution context; every call is appended to calls.
type ebReadStub struct {
	launch   LaunchContext
	launchE  error
	checkCtx CheckExecutionContext
	checkE   error
	frozen   FrozenRun
	frozenE  error
	calls    *[]string
}

func (s *ebReadStub) record(name string) {
	if s.calls != nil {
		*s.calls = append(*s.calls, name)
	}
}

func (*ebReadStub) ListRuns(context.Context, string) ([]RunStatus, error) {
	return nil, errors.New("unexpected ListRuns")
}

func (*ebReadStub) LoadRunStatus(context.Context, identity.RunID) (RunDetail, error) {
	return RunDetail{}, errors.New("unexpected LoadRunStatus")
}

func (s *ebReadStub) LoadFrozenRun(context.Context, identity.RunID) (FrozenRun, error) {
	s.record("LoadFrozenRun")
	return s.frozen, s.frozenE
}

func (s *ebReadStub) LoadLaunchContext(context.Context, identity.RunID, identity.AttemptID) (LaunchContext, error) {
	s.record("LoadLaunchContext")
	return s.launch, s.launchE
}

func (s *ebReadStub) LoadCheckExecutionContext(context.Context, identity.OperationID) (CheckExecutionContext, error) {
	s.record("LoadCheckExecutionContext")
	return s.checkCtx, s.checkE
}

// ebSubmissionStub is a minimal SubmissionStore recording claims and
// settlements; every call is appended to calls.
type ebSubmissionStub struct {
	claimErr      error
	checkClaimErr error
	settleErr     error

	claims       []LaunchClaim
	checkClaims  []int
	checkOps     []identity.OperationID
	settled      []identity.IncarnationID
	settleReason []string
	calls        *[]string
}

func (s *ebSubmissionStub) record(name string) {
	if s.calls != nil {
		*s.calls = append(*s.calls, name)
	}
}

func (s *ebSubmissionStub) ClaimLaunch(_ context.Context, claim LaunchClaim) error { //nolint:gocritic // hugeParam: the fake mirrors the SubmissionStore port signature, which passes the claim by value.
	s.record("ClaimLaunch")
	if s.claimErr != nil {
		return s.claimErr
	}
	s.claims = append(s.claims, claim)
	return nil
}

func (s *ebSubmissionStub) SettleLaunchFailure(_ context.Context, incarnation identity.IncarnationID, reason string) error {
	s.record("SettleLaunchFailure")
	if s.settleErr != nil {
		return s.settleErr
	}
	s.settled = append(s.settled, incarnation)
	s.settleReason = append(s.settleReason, reason)
	return nil
}

func (s *ebSubmissionStub) ClaimCheckExec(_ context.Context, op identity.OperationID, pid int) error {
	s.record("ClaimCheckExec")
	if s.checkClaimErr != nil {
		return s.checkClaimErr
	}
	s.checkOps = append(s.checkOps, op)
	s.checkClaims = append(s.checkClaims, pid)
	return nil
}

func (*ebSubmissionStub) SubmitResult(context.Context, ResultSubmission) (SubmissionOutcome, error) {
	return SubmissionOutcome{}, errors.New("unexpected SubmitResult")
}

func (*ebSubmissionStub) RecordMalformed(context.Context, ClaimedSubmission) (SubmissionOutcome, error) {
	return SubmissionOutcome{}, errors.New("unexpected RecordMalformed")
}

func (*ebSubmissionStub) RequestStop(context.Context, identity.RunID) error {
	return errors.New("unexpected RequestStop")
}

// ebClock is a fixed clock.
type ebClock struct{ now time.Time }

func (c ebClock) Now() time.Time { return c.now }

// ebLaunchContext builds a valid first-launch context.
func ebLaunchContext(t *testing.T) LaunchContext {
	t.Helper()
	attemptID, err := identity.ParseAttemptID(ebAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	incarnationID, err := identity.ParseIncarnationID(ebIncarnationID)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := identity.ParseTaskID(ebTaskID)
	if err != nil {
		t.Fatal(err)
	}
	return LaunchContext{
		Snapshot: RunSnapshot{
			EnvPolicy: EnvPolicy{Version: EnvPolicyVersion1, Harness: HarnessClaude},
			Harness:   HarnessClaude,
			StateRoot: "/state",
			AssignmentPath: "/state/runs/" + ebRunID +
				"/artifacts/assignment.md",
		},
		Harness:       run.HarnessClaude,
		Attempt:       run.Attempt{ID: attemptID, TaskID: taskID, State: run.AttemptLaunching},
		Session:       run.Session{NativeSessionRef: ebNativeRef},
		IncarnationID: incarnationID,
	}
}

// ebEnviron is a launch environment carrying the pane-provided HOP_*
// variables, a strip-matrix credential and a PATH.
func ebEnviron() []string {
	return []string{
		"PATH=/usr/bin:/bin",
		"ANTHROPIC_API_KEY=secret-value",
		"HOP_STATE_DIR=/state",
		"HOP_RUN_ID=" + ebRunID,
		"HOP_TASK_ID=" + ebTaskID,
		"HOP_ATTEMPT_ID=" + ebAttemptID,
		"HOP_INCARNATION_ID=" + ebIncarnationID,
	}
}

// environWithout returns environ minus every entry of name.
func environWithout(environ []string, name string) []string {
	var kept []string
	for _, entry := range environ {
		entryName, _, _ := strings.Cut(entry, "=")
		if entryName != name {
			kept = append(kept, entry)
		}
	}
	return kept
}

// ebLookup returns an ExecutableLookup resolving every name under /resolved.
func ebLookup(recorded *[]string) ExecutableLookup {
	return func(name, pathValue string) (string, error) {
		if recorded != nil {
			*recorded = append(*recorded, name+" in "+pathValue)
		}
		return "/resolved/" + name, nil
	}
}

func TestPrepareLaunchExec(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	newController := func(read *ebReadStub, subs *ebSubmissionStub) *Controller {
		return &Controller{Read: read, Submissions: subs, Clock: ebClock{now: now}}
	}
	baseRequest := func() LaunchExecRequest {
		return LaunchExecRequest{
			RunID:            ebRunID,
			AttemptID:        ebAttemptID,
			HOPPath:          "/opt/hop/bin/hop",
			Environ:          ebEnviron(),
			PID:              4242,
			LookupExecutable: ebLookup(nil),
		}
	}

	t.Run("first launch composes session-id argv, sanitizes and claims", func(t *testing.T) {
		read := &ebReadStub{launch: ebLaunchContext(t)}
		subs := &ebSubmissionStub{}
		var lookups []string
		req := baseRequest()
		req.LookupExecutable = ebLookup(&lookups)

		plan, err := newController(read, subs).PrepareLaunchExec(context.Background(), req)
		if err != nil {
			t.Fatalf("PrepareLaunchExec: %v", err)
		}

		if len(plan.Argv) != 4 || plan.Argv[0] != "/resolved/claude" || plan.Argv[1] != "--session-id" || plan.Argv[2] != ebNativeRef {
			t.Fatalf("argv = %q", plan.Argv)
		}
		prompt := plan.Argv[3]
		for _, want := range []string{"/state/runs/" + ebRunID + "/artifacts/assignment.md", "/opt/hop/bin/hop result submit", "transient"} {
			if !strings.Contains(prompt, want) {
				t.Errorf("prompt lacks %q; got %q", want, prompt)
			}
		}
		if len(lookups) != 1 || lookups[0] != "claude in /usr/bin:/bin" {
			t.Errorf("lookups = %q, want the harness resolved in the sanitized PATH", lookups)
		}
		joined := strings.Join(plan.Env, "\n")
		if strings.Contains(joined, "ANTHROPIC_API_KEY") {
			t.Errorf("sanitized env still carries a strip-matrix variable:\n%s", joined)
		}
		if !strings.Contains(joined, "HOP_INCARNATION_ID="+ebIncarnationID) {
			t.Errorf("sanitized env lost a HOP_* variable:\n%s", joined)
		}
		if len(subs.claims) != 1 {
			t.Fatalf("claims = %d, want 1", len(subs.claims))
		}
		claim := subs.claims[0]
		if claim.PID != 4242 || claim.Executable != "/resolved/claude" || claim.State != LaunchClaimExecPending {
			t.Errorf("claim = %+v", claim)
		}
		if claim.IncarnationID.String() != ebIncarnationID || claim.RunID.String() != ebRunID || claim.AttemptID.String() != ebAttemptID {
			t.Errorf("claim identities = %+v", claim)
		}
		if claim.ArgvDigest != launchArgvDigest(plan.Argv) {
			t.Errorf("claim digest %q does not match the composed argv", claim.ArgvDigest)
		}
		if !claim.ClaimedAt.Equal(now) {
			t.Errorf("claimed at %v, want %v", claim.ClaimedAt, now)
		}
		if plan.IncarnationID != ebIncarnationID {
			t.Errorf("plan incarnation = %q", plan.IncarnationID)
		}
	})

	t.Run("an identical same-pid retry is idempotent and execs", func(t *testing.T) {
		lc := ebLaunchContext(t)
		expectedArgv := []string{
			"/resolved/claude", "--session-id", ebNativeRef,
			renderInitialPrompt(lc.Snapshot.AssignmentPath, "/opt/hop/bin/hop"),
		}
		lc.Claim = &LaunchClaim{
			PID: 4242, State: LaunchClaimExecPending,
			Executable: "/resolved/claude",
			ArgvDigest: launchArgvDigest(expectedArgv),
		}
		read := &ebReadStub{launch: lc}
		subs := &ebSubmissionStub{}

		plan, err := newController(read, subs).PrepareLaunchExec(context.Background(), baseRequest())
		if err != nil {
			t.Fatalf("PrepareLaunchExec: %v", err)
		}

		if len(subs.claims) != 1 {
			t.Fatalf("claims = %d; the exact-identity retry re-writes idempotently", len(subs.claims))
		}
		if plan.Argv[0] != "/resolved/claude" {
			t.Errorf("argv = %q", plan.Argv)
		}
	})

	t.Run("codex first launch composes the prompt argv with its profile shape", func(t *testing.T) {
		lc := ebLaunchContext(t)
		lc.Snapshot.Harness = HarnessCodex
		lc.Snapshot.EnvPolicy.Harness = HarnessCodex
		lc.Snapshot.EnvPolicy.ProfileDir = "/profiles/codex"
		lc.Session.NativeSessionRef = "" // only Claude needs the pre-assigned reference
		read := &ebReadStub{launch: lc}
		subs := &ebSubmissionStub{}

		plan, err := newController(read, subs).PrepareLaunchExec(context.Background(), baseRequest())
		if err != nil {
			t.Fatalf("PrepareLaunchExec: %v", err)
		}

		if len(plan.Argv) != 2 || plan.Argv[0] != "/resolved/codex" {
			t.Fatalf("argv = %q, want the codex executable and the prompt only", plan.Argv)
		}
		prompt := plan.Argv[1]
		for _, want := range []string{"/state/runs/" + ebRunID + "/artifacts/assignment.md", "/opt/hop/bin/hop result submit", "transient"} {
			if !strings.Contains(prompt, want) {
				t.Errorf("prompt lacks %q; got %q", want, prompt)
			}
		}
		joined := strings.Join(plan.Env, "\n")
		if !strings.Contains(joined, "CODEX_HOME=/profiles/codex") {
			t.Errorf("codex profile shape not assigned:\n%s", joined)
		}
		if len(subs.claims) != 1 || subs.claims[0].Executable != "/resolved/codex" || subs.claims[0].ArgvDigest != launchArgvDigest(plan.Argv) {
			t.Errorf("claim = %+v", subs.claims)
		}
	})

	t.Run("opencode first launch composes the --prompt argv", func(t *testing.T) {
		lc := ebLaunchContext(t)
		lc.Snapshot.Harness = HarnessOpencode
		lc.Snapshot.EnvPolicy.Harness = HarnessOpencode
		lc.Session.NativeSessionRef = ""
		read := &ebReadStub{launch: lc}
		subs := &ebSubmissionStub{}

		plan, err := newController(read, subs).PrepareLaunchExec(context.Background(), baseRequest())
		if err != nil {
			t.Fatalf("PrepareLaunchExec: %v", err)
		}

		if len(plan.Argv) != 3 || plan.Argv[0] != "/resolved/opencode" || plan.Argv[1] != "--prompt" {
			t.Fatalf("argv = %q, want opencode --prompt <prompt>", plan.Argv)
		}
		if !strings.Contains(plan.Argv[2], "/state/runs/"+ebRunID+"/artifacts/assignment.md") {
			t.Errorf("prompt lacks the assignment path (the argv marker); got %q", plan.Argv[2])
		}
		if len(subs.claims) != 1 || subs.claims[0].Executable != "/resolved/opencode" {
			t.Errorf("claim = %+v", subs.claims)
		}
	})

	t.Run("cold relaunch composes resume argv", func(t *testing.T) {
		lc := ebLaunchContext(t)
		lc.Attempt.State = run.AttemptRelaunching
		read := &ebReadStub{launch: lc}
		subs := &ebSubmissionStub{}

		plan, err := newController(read, subs).PrepareLaunchExec(context.Background(), baseRequest())
		if err != nil {
			t.Fatalf("PrepareLaunchExec: %v", err)
		}

		want := []string{"/resolved/claude", "--resume", ebNativeRef}
		if len(plan.Argv) != len(want) {
			t.Fatalf("argv = %q, want %q", plan.Argv, want)
		}
		for i := range want {
			if plan.Argv[i] != want[i] {
				t.Fatalf("argv = %q, want %q", plan.Argv, want)
			}
		}
	})

	refusals := []struct {
		name    string
		mutate  func(lc *LaunchContext, req *LaunchExecRequest, subs *ebSubmissionStub)
		wantErr string
	}{
		{
			name:    "malformed run id",
			mutate:  func(_ *LaunchContext, req *LaunchExecRequest, _ *ebSubmissionStub) { req.RunID = "not-a-uuid" },
			wantErr: "parse run id",
		},
		{
			name:    "malformed attempt id",
			mutate:  func(_ *LaunchContext, req *LaunchExecRequest, _ *ebSubmissionStub) { req.AttemptID = "nope" },
			wantErr: "parse attempt id",
		},
		{
			name:    "relative hop path",
			mutate:  func(_ *LaunchContext, req *LaunchExecRequest, _ *ebSubmissionStub) { req.HOPPath = "bin/hop" },
			wantErr: "not absolute",
		},
		{
			name: "missing HOP_INCARNATION_ID",
			mutate: func(_ *LaunchContext, req *LaunchExecRequest, _ *ebSubmissionStub) {
				req.Environ = environWithout(req.Environ, "HOP_INCARNATION_ID")
			},
			wantErr: "HOP_INCARNATION_ID is not set",
		},
		{
			name: "missing HOP_STATE_DIR",
			mutate: func(_ *LaunchContext, req *LaunchExecRequest, _ *ebSubmissionStub) {
				req.Environ = environWithout(req.Environ, "HOP_STATE_DIR")
			},
			wantErr: "HOP_STATE_DIR is not set",
		},
		{
			name: "missing HOP_RUN_ID",
			mutate: func(_ *LaunchContext, req *LaunchExecRequest, _ *ebSubmissionStub) {
				req.Environ = environWithout(req.Environ, "HOP_RUN_ID")
			},
			wantErr: "HOP_RUN_ID is not set",
		},
		{
			name: "missing HOP_TASK_ID",
			mutate: func(_ *LaunchContext, req *LaunchExecRequest, _ *ebSubmissionStub) {
				req.Environ = environWithout(req.Environ, "HOP_TASK_ID")
			},
			wantErr: "HOP_TASK_ID is not set",
		},
		{
			name: "missing HOP_ATTEMPT_ID",
			mutate: func(_ *LaunchContext, req *LaunchExecRequest, _ *ebSubmissionStub) {
				req.Environ = environWithout(req.Environ, "HOP_ATTEMPT_ID")
			},
			wantErr: "HOP_ATTEMPT_ID is not set",
		},
		{
			name: "mismatched incarnation",
			mutate: func(_ *LaunchContext, req *LaunchExecRequest, _ *ebSubmissionStub) {
				req.Environ = append(req.Environ, "HOP_INCARNATION_ID="+ebNativeRef)
			},
			wantErr: "HOP_INCARNATION_ID does not agree",
		},
		{
			name: "mismatched HOP_RUN_ID",
			mutate: func(_ *LaunchContext, req *LaunchExecRequest, _ *ebSubmissionStub) {
				req.Environ = append(req.Environ, "HOP_RUN_ID="+ebTaskID)
			},
			wantErr: "HOP_RUN_ID does not agree",
		},
		{
			name: "mismatched HOP_TASK_ID",
			mutate: func(_ *LaunchContext, req *LaunchExecRequest, _ *ebSubmissionStub) {
				req.Environ = append(req.Environ, "HOP_TASK_ID="+ebRunID)
			},
			wantErr: "HOP_TASK_ID does not agree",
		},
		{
			name: "mismatched HOP_STATE_DIR never opens a foreign run's exec",
			mutate: func(_ *LaunchContext, req *LaunchExecRequest, _ *ebSubmissionStub) {
				req.Environ = append(req.Environ, "HOP_STATE_DIR=/some/other/root")
			},
			wantErr: "HOP_STATE_DIR does not agree",
		},
		{
			name:    "stop requested",
			mutate:  func(lc *LaunchContext, _ *LaunchExecRequest, _ *ebSubmissionStub) { lc.StopRequested = true },
			wantErr: "stop request",
		},
		{
			name: "attempt no longer launching",
			mutate: func(lc *LaunchContext, _ *LaunchExecRequest, _ *ebSubmissionStub) {
				lc.Attempt.State = run.AttemptRunning
			},
			wantErr: "not launching or relaunching",
		},
		{
			name: "existing claim with a different pid",
			mutate: func(lc *LaunchContext, _ *LaunchExecRequest, _ *ebSubmissionStub) {
				lc.Claim = &LaunchClaim{PID: 999, State: LaunchClaimExecPending}
			},
			wantErr: "different pid",
		},
		{
			name: "same-pid claim recording a different executable never execs",
			mutate: func(lc *LaunchContext, _ *LaunchExecRequest, _ *ebSubmissionStub) {
				lc.Claim = &LaunchClaim{PID: 4242, State: LaunchClaimExecPending, Executable: "/old/claude", ArgvDigest: "differs"}
			},
			wantErr: "records a different executable or argv",
		},
		{
			name: "same-pid claim recording a different argv digest never execs",
			mutate: func(lc *LaunchContext, req *LaunchExecRequest, _ *ebSubmissionStub) {
				lc.Claim = &LaunchClaim{
					PID: 4242, State: LaunchClaimExecPending,
					Executable: "/resolved/claude",
					ArgvDigest: launchArgvDigest([]string{"/resolved/claude", "--session-id", ebNativeRef, "another prompt"}),
				}
				_ = req
			},
			wantErr: "records a different executable or argv",
		},
		{
			name: "settled claim never execs again",
			mutate: func(lc *LaunchContext, _ *LaunchExecRequest, _ *ebSubmissionStub) {
				lc.Claim = &LaunchClaim{PID: 4242, State: LaunchClaimExecFailed}
			},
			wantErr: "already settled",
		},
		{
			name: "codex cold resume names the unsupported state",
			mutate: func(lc *LaunchContext, _ *LaunchExecRequest, _ *ebSubmissionStub) {
				lc.Snapshot.Harness = HarnessCodex
				lc.Snapshot.EnvPolicy.Harness = HarnessCodex
				lc.Attempt.State = run.AttemptRelaunching
			},
			wantErr: "cold resume for harness",
		},
		{
			name: "opencode cold resume names the unsupported state",
			mutate: func(lc *LaunchContext, _ *LaunchExecRequest, _ *ebSubmissionStub) {
				lc.Snapshot.Harness = HarnessOpencode
				lc.Snapshot.EnvPolicy.Harness = HarnessOpencode
				lc.Attempt.State = run.AttemptRelaunching
			},
			wantErr: "cold resume for harness",
		},
		{
			name: "missing native reference refuses a claude launch",
			mutate: func(lc *LaunchContext, _ *LaunchExecRequest, _ *ebSubmissionStub) {
				lc.Session.NativeSessionRef = ""
			},
			wantErr: "no pre-assigned native session reference",
		},
		{
			name: "invalid frozen policy",
			mutate: func(lc *LaunchContext, _ *LaunchExecRequest, _ *ebSubmissionStub) {
				lc.Snapshot.EnvPolicy.Version = "hop-env-v0"
			},
			wantErr: "environment policy",
		},
		{
			name: "executable lookup failure",
			mutate: func(_ *LaunchContext, req *LaunchExecRequest, _ *ebSubmissionStub) {
				req.LookupExecutable = func(string, string) (string, error) { return "", errors.New("claude not found in PATH") }
			},
			wantErr: "resolve harness executable",
		},
		{
			name: "refused claim write",
			mutate: func(_ *LaunchContext, _ *LaunchExecRequest, subs *ebSubmissionStub) {
				subs.claimErr = errors.New("incarnation is not current")
			},
			wantErr: "claim launch",
		},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			lc := ebLaunchContext(t)
			subs := &ebSubmissionStub{}
			req := baseRequest()
			tc.mutate(&lc, &req, subs)
			read := &ebReadStub{launch: lc}

			_, err := newController(read, subs).PrepareLaunchExec(context.Background(), req)

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Errorf("error echoes an environment value: %v", err)
			}
			if len(subs.claims) != 0 {
				t.Errorf("a refused launch wrote a claim: %+v", subs.claims)
			}
		})
	}
}

func TestFailLaunchExec(t *testing.T) {
	subs := &ebSubmissionStub{}
	c := &Controller{Submissions: subs}

	if err := c.FailLaunchExec(context.Background(), ebIncarnationID, "exec /resolved/claude: permission denied"); err != nil {
		t.Fatalf("FailLaunchExec: %v", err)
	}

	if len(subs.settled) != 1 || subs.settled[0].String() != ebIncarnationID {
		t.Fatalf("settled = %v", subs.settled)
	}
	if subs.settleReason[0] != "exec /resolved/claude: permission denied" {
		t.Errorf("reason = %q", subs.settleReason[0])
	}

	if err := c.FailLaunchExec(context.Background(), "junk", "r"); err == nil {
		t.Error("a malformed incarnation id was accepted")
	}
}

func TestPrepareCheckExec(t *testing.T) {
	frozenArgv := []string{"sh", "check.sh"}
	newContext := func() CheckExecutionContext {
		return CheckExecutionContext{
			EnvPolicy:    EnvPolicy{Version: EnvPolicyVersion1, Harness: HarnessClaude},
			StateRoot:    "/state",
			CheckoutPath: "/state/runs/r/checks/op/tree",
			CheckArgv:    frozenArgv,
		}
	}
	baseRequest := func() CheckExecRequest {
		return CheckExecRequest{
			OperationID:       ebOperationID,
			CheckArgv:         []string{"sh", "check.sh"},
			Environ:           []string{"PATH=/usr/bin:/bin", "ANTHROPIC_API_KEY=secret-value", "HOP_STATE_DIR=/state"},
			PID:               777,
			LeadsProcessGroup: true,
			LookupExecutable:  ebLookup(nil),
		}
	}

	t.Run("claims before loading, keeps the frozen argv verbatim", func(t *testing.T) {
		var calls []string
		read := &ebReadStub{checkCtx: newContext(), calls: &calls}
		subs := &ebSubmissionStub{calls: &calls}
		c := &Controller{Read: read, Submissions: subs}

		plan, err := c.PrepareCheckExec(context.Background(), baseRequest())
		if err != nil {
			t.Fatalf("PrepareCheckExec: %v", err)
		}

		if len(calls) < 2 || calls[0] != "ClaimCheckExec" || calls[1] != "LoadCheckExecutionContext" {
			t.Fatalf("call order = %v, want the claim before the context load", calls)
		}
		if len(subs.checkClaims) != 1 || subs.checkClaims[0] != 777 || subs.checkOps[0].String() != ebOperationID {
			t.Fatalf("check claim = ops %v pids %v", subs.checkOps, subs.checkClaims)
		}
		if plan.ExecPath != "/resolved/sh" {
			t.Errorf("exec path = %q", plan.ExecPath)
		}
		if len(plan.Argv) != 2 || plan.Argv[0] != "sh" || plan.Argv[1] != "check.sh" {
			t.Errorf("argv = %q, want the frozen argv unrewritten", plan.Argv)
		}
		joined := strings.Join(plan.Env, "\n")
		if strings.Contains(joined, "ANTHROPIC_API_KEY") {
			t.Errorf("sanitized env still carries a strip-matrix variable:\n%s", joined)
		}
		if !strings.Contains(joined, "HOP_STATE_DIR=/state") {
			t.Errorf("sanitized env lost HOP_STATE_DIR:\n%s", joined)
		}
	})

	refusals := []struct {
		name      string
		mutate    func(req *CheckExecRequest, read *ebReadStub, subs *ebSubmissionStub)
		wantErr   string
		wantClaim bool
	}{
		{
			name:    "malformed operation id",
			mutate:  func(req *CheckExecRequest, _ *ebReadStub, _ *ebSubmissionStub) { req.OperationID = "junk" },
			wantErr: "parse operation id",
		},
		{
			name:    "not a process-group leader",
			mutate:  func(req *CheckExecRequest, _ *ebReadStub, _ *ebSubmissionStub) { req.LeadsProcessGroup = false },
			wantErr: "does not lead its own process group",
		},
		{
			name: "refused claim runs nothing",
			mutate: func(_ *CheckExecRequest, _ *ebReadStub, subs *ebSubmissionStub) {
				subs.checkClaimErr = errors.New("operation is not a pending check execution")
			},
			wantErr: "claim check execution",
		},
		{
			name: "command-line argv must equal the frozen argv",
			mutate: func(req *CheckExecRequest, _ *ebReadStub, _ *ebSubmissionStub) {
				req.CheckArgv = []string{"sh", "other.sh"}
			},
			wantErr:   "does not equal the frozen check argv",
			wantClaim: true,
		},
		{
			name: "lookup failure",
			mutate: func(req *CheckExecRequest, _ *ebReadStub, _ *ebSubmissionStub) {
				req.LookupExecutable = func(string, string) (string, error) { return "", errors.New("sh not found") }
			},
			wantErr:   "resolve check executable",
			wantClaim: true,
		},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			read := &ebReadStub{checkCtx: newContext()}
			subs := &ebSubmissionStub{}
			req := baseRequest()
			tc.mutate(&req, read, subs)

			_, err := (&Controller{Read: read, Submissions: subs}).PrepareCheckExec(context.Background(), req)

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if !tc.wantClaim && len(subs.checkClaims) != 0 {
				t.Errorf("a refused pre-claim step wrote a claim: %v", subs.checkClaims)
			}
		})
	}
}

func TestCheckSpawnEnvironment(t *testing.T) {
	runID, err := identity.ParseRunID(ebRunID)
	if err != nil {
		t.Fatal(err)
	}
	read := &ebReadStub{frozen: FrozenRun{Snapshot: RunSnapshot{
		EnvPolicy: EnvPolicy{Version: EnvPolicyVersion1, Harness: HarnessClaude},
	}}}
	c := &Controller{Read: read}
	handle := RunHandle{runID: runID}

	env, err := c.CheckSpawnEnvironment(context.Background(), handle, []string{"PATH=/bin", "OPENAI_API_KEY=secret-value", "HOP_STATE_DIR=/state"})
	if err != nil {
		t.Fatalf("CheckSpawnEnvironment: %v", err)
	}

	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "OPENAI_API_KEY") {
		t.Errorf("spawn env still carries a strip-matrix variable:\n%s", joined)
	}
	if !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "HOP_STATE_DIR=/state") {
		t.Errorf("spawn env lost an expected entry:\n%s", joined)
	}
}

func TestEnvironValueLastEntryWins(t *testing.T) {
	environ := []string{"A=first", "B=only", "A=last", "C"}
	if got := environValue(environ, "A"); got != "last" {
		t.Errorf(`environValue(A) = %q, want "last"`, got)
	}
	if got := environValue(environ, "C"); got != "" {
		t.Errorf(`environValue(C) = %q, want ""`, got)
	}
	if got := environValue(environ, "missing"); got != "" {
		t.Errorf(`environValue(missing) = %q, want ""`, got)
	}
}
