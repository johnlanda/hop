package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
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

// ebReadStub is a minimal ReadStore serving one check execution context;
// every call is appended to calls.
type ebReadStub struct {
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

// ebTrustStub is a scripted TrustSeeder recording every call.
type ebTrustStub struct {
	outcome TrustSeedOutcome
	err     error
	calls   []string
	names   *[]string
}

func (s *ebTrustStub) SeedWorkspaceTrust(_ context.Context, configPath, projectKey string) (TrustSeedOutcome, error) {
	s.calls = append(s.calls, configPath+" <- "+projectKey)
	if s.names != nil {
		*s.names = append(*s.names, "SeedWorkspaceTrust")
	}
	if s.err != nil {
		return TrustSeedOutcome{}, s.err
	}
	return s.outcome, nil
}

// ebResolve canonicalizes paths the way the composition resolver does on
// macOS: a /var/ prefix resolves to /private/var/, anything else is
// already canonical, and a path naming "cannot-resolve" fails with a
// path-bearing error (which preparation must never echo).
func ebResolve(path string) (string, error) {
	if strings.Contains(path, "cannot-resolve") {
		return "", errors.New("lstat " + path + ": no such file or directory")
	}
	if strings.HasPrefix(path, "/var/") {
		return "/private" + path, nil
	}
	return path, nil
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

	t.Run("integration.merge: the frozen merge argv passes through byte-identically", func(t *testing.T) {
		// The generalized boundary (design sections 3 and 8): the context
		// resolved for an integration.merge operation carries the intent's
		// frozen noninteractive merge argv, and the boundary treats it
		// exactly like a check.run's — claim first, argv equality, verbatim
		// pass-through (group retirement matches the running argv), env
		// sanitized, only the executed path resolved.
		mergeArgv := []string{
			"/usr/bin/git", "-C", "/state/runs/r/integrations/op/tree",
			"-c", "user.name=hop", "-c", "user.email=hop@invalid",
			"-c", "core.editor=true", "-c", "core.hooksPath=/state/runs/r/integrations/op/hooks",
			"-c", "commit.gpgsign=false", "-c", "merge.verifysignatures=false",
			"merge", "--no-ff", "--no-edit", "0123456789abcdef0123456789abcdef01234567",
		}
		var calls []string
		mergeCtx := newContext()
		mergeCtx.CheckArgv = append([]string(nil), mergeArgv...)
		mergeCtx.CheckoutPath = "/state/runs/r/integrations/op/tree"
		read := &ebReadStub{checkCtx: mergeCtx, calls: &calls}
		subs := &ebSubmissionStub{calls: &calls}
		c := &Controller{Read: read, Submissions: subs}

		req := baseRequest()
		req.CheckArgv = append([]string(nil), mergeArgv...)
		plan, err := c.PrepareCheckExec(context.Background(), req)
		if err != nil {
			t.Fatalf("PrepareCheckExec: %v", err)
		}

		if len(calls) < 2 || calls[0] != "ClaimCheckExec" || calls[1] != "LoadCheckExecutionContext" {
			t.Fatalf("call order = %v, want the claim before the context load", calls)
		}
		if len(plan.Argv) != len(mergeArgv) {
			t.Fatalf("argv length = %d, want %d", len(plan.Argv), len(mergeArgv))
		}
		for i := range mergeArgv {
			if plan.Argv[i] != mergeArgv[i] {
				t.Fatalf("argv[%d] = %q, want %q verbatim", i, plan.Argv[i], mergeArgv[i])
			}
		}
		if plan.ExecPath != "/resolved//usr/bin/git" {
			t.Errorf("exec path = %q, want the lookup's resolution of the frozen argv[0]", plan.ExecPath)
		}
		if strings.Contains(strings.Join(plan.Env, "\n"), "ANTHROPIC_API_KEY") {
			t.Errorf("sanitized env still carries a strip-matrix variable")
		}
	})

	t.Run("integration.merge: a rearranged merge argv is refused after the claim", func(t *testing.T) {
		mergeArgv := []string{"/usr/bin/git", "merge", "--no-ff", "--no-edit", "abc"}
		mergeCtx := newContext()
		mergeCtx.CheckArgv = mergeArgv
		read := &ebReadStub{checkCtx: mergeCtx}
		subs := &ebSubmissionStub{}

		req := baseRequest()
		req.CheckArgv = []string{"/usr/bin/git", "merge", "--no-edit", "--no-ff", "abc"}
		_, err := (&Controller{Read: read, Submissions: subs}).PrepareCheckExec(context.Background(), req)

		if err == nil || !strings.Contains(err.Error(), "does not equal the frozen check argv") {
			t.Fatalf("err = %v, want the frozen-argv refusal", err)
		}
		if len(subs.checkClaims) != 1 {
			t.Errorf("claim count = %d, want the pre-verification claim recorded", len(subs.checkClaims))
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
