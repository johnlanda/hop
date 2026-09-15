package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// launchArgvDigestTag versions the canonical launch-argv digest encoding.
const launchArgvDigestTag = "hop-argv-v1"

// ExecutableLookup resolves a command name to the absolute path of the
// binary that would run, searching pathValue (the sanitized environment's
// PATH) for a bare name. It is the one effectful step of the exec-boundary
// preparation, supplied by composition — cmd/hop implements it over the
// filesystem, tests substitute a fake — because this package performs no
// filesystem access itself.
type ExecutableLookup func(name, pathValue string) (string, error)

// LaunchExecRequest is hop launch's input: the --run/--attempt flag values
// as raw strings, the absolute path of the running hop executable (rendered
// into the initial prompt's submit instruction), the launcher's
// symlink-resolved absolute working directory (the attempt worktree the
// pane was created at — execve preserves it, so it is exactly the path the
// harness resolves as its own cwd), the launcher's complete inherited
// environment and its own pid, and the composition-supplied executable
// lookup.
type LaunchExecRequest struct {
	RunID     string
	AttemptID string
	HOPPath   string
	WorkerDir string
	Environ   []string
	PID       int
	// ResolvePath canonically resolves an absolute path (symlinks
	// followed), supplied by composition like LookupExecutable because
	// this package performs no filesystem access itself. PrepareLaunchExec
	// resolves BOTH the launcher's working directory and the recorded
	// worktree path through it and refuses on disagreement; its errors are
	// never echoed (they may carry the path).
	ResolvePath      func(path string) (string, error)
	LookupExecutable ExecutableLookup
}

// LaunchExecPlan is what hop launch execs once PrepareLaunchExec has
// validated, seeded, claimed and composed: the harness argv (argv[0] the
// resolved absolute executable recorded in the claim) and the sanitized
// complete environment. IncarnationID is the claimed incarnation as a
// string, so a failed exec can settle the claim through FailLaunchExec
// without the caller holding a typed identity. SeedEvidence is the
// workspace-trust pre-seeding outcome exactly as the claim recorded it.
type LaunchExecPlan struct {
	Argv          []string
	Env           []string
	IncarnationID string
	SeedEvidence  string
}

// PrepareLaunchExec performs every hop launch step before the exec itself
// (docs/plan/phase-2-design.md section 6): it loads the launch context
// lease-free, validates the HOP_* environment against it fail-closed —
// every pane-provided variable must be present and agree with the context
// (validateLaunchEnvironment) — together with attempt currency, the stop
// state and the different-pid claim rule; sanitizes the launcher's inherited environment under the
// snapshot's frozen, versioned policy; composes the harness argv from the
// frozen snapshot per harness (first launches for Claude, Codex and
// opencode share the fixed initial prompt; a cold relaunch is Claude's
// --resume with the pre-assigned native reference, and Codex/opencode
// cold resume reports the Phase 2 unsupported state); resolves the
// harness executable to
// an absolute path using the sanitized environment's PATH; applies the
// workspace-trust pre-seed (PlanTrustSeed over the sanitized environment
// and the launcher's resolved working directory, written through the
// TrustSeeder port — an absent or unparsable profile config is a
// not-seeded outcome and the launch proceeds, while a seeding failure
// refuses the launch fail-closed); and records the
// launch claim (exec_pending) with that exact executable, the argv digest,
// the launcher's own pid and the seed outcome as evidence — evidence only,
// never a decision input. Any failure returns before the claim is
// written — no claim, no exec — except a failed claim write itself, which
// is the store's refusal. Error messages name environment variables but
// never echo their values.
func (c *Controller) PrepareLaunchExec(ctx context.Context, req LaunchExecRequest) (LaunchExecPlan, error) { //nolint:gocritic // hugeParam: LaunchExecRequest is the driving DTO for hop launch, called once per launcher process.
	runID, err := identity.ParseRunID(req.RunID)
	if err != nil {
		return LaunchExecPlan{}, fmt.Errorf("app: parse run id: %w", err)
	}
	attemptID, err := identity.ParseAttemptID(req.AttemptID)
	if err != nil {
		return LaunchExecPlan{}, fmt.Errorf("app: parse attempt id: %w", err)
	}
	if req.PID <= 0 {
		return LaunchExecPlan{}, fmt.Errorf("app: launcher pid %d is not a valid process id", req.PID)
	}
	if !filepath.IsAbs(req.HOPPath) {
		return LaunchExecPlan{}, fmt.Errorf("app: hop executable path is not absolute; the initial prompt carries only absolute paths")
	}
	if !filepath.IsAbs(req.WorkerDir) {
		return LaunchExecPlan{}, fmt.Errorf("app: the launcher working directory is not absolute; the workspace-trust seed key must be the resolved worktree path")
	}
	if req.LookupExecutable == nil {
		return LaunchExecPlan{}, fmt.Errorf("app: no executable lookup supplied")
	}
	if req.ResolvePath == nil {
		return LaunchExecPlan{}, fmt.Errorf("app: no path resolver supplied")
	}

	lc, err := c.Read.LoadLaunchContext(ctx, runID, attemptID)
	if err != nil {
		return LaunchExecPlan{}, fmt.Errorf("app: load launch context: %w", err)
	}
	if envErr := validateLaunchEnvironment(req.Environ, &lc, runID.String(), attemptID.String()); envErr != nil {
		return LaunchExecPlan{}, envErr
	}
	if lc.StopRequested {
		return LaunchExecPlan{}, fmt.Errorf("app: the run has a stop request; the worker is not launched")
	}
	if lc.Attempt.State != run.AttemptLaunching && lc.Attempt.State != run.AttemptRelaunching {
		return LaunchExecPlan{}, fmt.Errorf("app: attempt is %s, not launching or relaunching; this launcher invocation is not current", lc.Attempt.State)
	}
	if lc.Claim != nil {
		if lc.Claim.PID != req.PID {
			return LaunchExecPlan{}, fmt.Errorf("app: a launch claim for this incarnation already exists with a different pid; a duplicate launcher never execs")
		}
		if lc.Claim.State != LaunchClaimExecPending {
			return LaunchExecPlan{}, fmt.Errorf("app: the launch claim for this incarnation is already settled %s; this incarnation never execs again", lc.Claim.State)
		}
	}

	validated, err := lc.Snapshot.EnvPolicy.Validate()
	if err != nil {
		return LaunchExecPlan{}, fmt.Errorf("app: frozen environment policy: %w", err)
	}
	env, _ := SanitizeEnvironment(req.Environ, validated)

	tail, err := composeHarnessArgvTail(&lc, req.HOPPath)
	if err != nil {
		return LaunchExecPlan{}, err
	}
	executable, err := req.LookupExecutable(lc.Snapshot.Harness, environValue(env, "PATH"))
	if err != nil {
		return LaunchExecPlan{}, fmt.Errorf("app: resolve harness executable: %w", err)
	}
	if !filepath.IsAbs(executable) {
		return LaunchExecPlan{}, fmt.Errorf("app: resolved harness executable is not an absolute path")
	}
	argv := append([]string{executable}, tail...)
	digest := launchArgvDigest(argv)
	// An existing same-pid exec_pending claim authorizes only the exact
	// invocation it recorded: the corroboration predicate settles a claim
	// by its recorded executable and argv identity, and the store keeps
	// the existing row on an idempotent same-pid rewrite, so executing a
	// differently composed plan under it would run an invocation the claim
	// does not describe. Identical retries stay idempotent; anything else
	// fails closed before exec.
	if lc.Claim != nil && (lc.Claim.Executable != executable || lc.Claim.ArgvDigest != digest) {
		return LaunchExecPlan{}, fmt.Errorf("app: the existing launch claim for this incarnation records a different executable or argv than this invocation composed; a claim is never rewritten and this launcher never execs")
	}

	workerDir, err := resolveWorkerDirAgainstWorktree(req.ResolvePath, req.WorkerDir, lc.WorktreePath)
	if err != nil {
		return LaunchExecPlan{}, err
	}
	seedEvidence, err := c.seedWorkspaceTrust(ctx, lc.Snapshot.Harness, env, workerDir)
	if err != nil {
		return LaunchExecPlan{}, err
	}

	claim := LaunchClaim{
		IncarnationID: lc.IncarnationID,
		RunID:         runID,
		AttemptID:     attemptID,
		Executable:    executable,
		ArgvDigest:    digest,
		PID:           req.PID,
		State:         LaunchClaimExecPending,
		ClaimedAt:     c.Clock.Now(),
		SeedEvidence:  seedEvidence,
	}
	if err := c.Submissions.ClaimLaunch(ctx, claim); err != nil {
		return LaunchExecPlan{}, fmt.Errorf("app: claim launch: %w", err)
	}
	return LaunchExecPlan{Argv: argv, Env: env, IncarnationID: lc.IncarnationID.String(), SeedEvidence: seedEvidence}, nil
}

// resolveWorkerDirAgainstWorktree canonically resolves the launcher's
// working directory and the run's recorded worktree path through the
// composition-supplied resolver and requires them to be one directory: the
// workspace-trust seed grants trust and the exec runs work, so both must
// target the attempt's own recorded worktree, never whatever directory a
// launcher happens to run in. It returns the resolved directory — the
// exact seed key. A missing recorded path, a resolution failure on either
// side, and a disagreement all refuse the launch fail-closed; resolver
// errors may carry a path and are never echoed.
func resolveWorkerDirAgainstWorktree(resolve func(string) (string, error), workerDir, recordedWorktree string) (string, error) {
	if recordedWorktree == "" {
		return "", fmt.Errorf("app: the run has no recorded worktree path; the launcher directory cannot be validated and is never seeded or execed")
	}
	resolvedWorker, err := resolve(workerDir)
	if err != nil {
		return "", fmt.Errorf("app: the launcher working directory could not be canonically resolved; an unresolvable directory is never seeded or execed")
	}
	resolvedRecorded, err := resolve(recordedWorktree)
	if err != nil {
		return "", fmt.Errorf("app: the recorded worktree path could not be canonically resolved; the launch is refused rather than run against unverified evidence")
	}
	if resolvedWorker != resolvedRecorded {
		return "", fmt.Errorf("app: the launcher working directory does not resolve to the attempt's recorded worktree; a foreign directory is never seeded or execed")
	}
	return resolvedWorker, nil
}

// seedWorkspaceTrust applies the launch's workspace-trust pre-seed and
// renders its claim evidence. The step is planned by PlanTrustSeed over
// the launched harness, the sanitized environment and the launcher's
// resolved working directory; a planned seed is written through the Trust
// port immediately before the claim, while the profile's harness is
// guaranteed not to be running for this incarnation yet. A not-seeded
// outcome (no seed applies, or the profile config is absent or
// unparsable) is evidence and the launch proceeds — the interactive trust
// dialog stays the surfaced fallback — but a seeding write failure is an
// error and the caller refuses the launch before any claim exists.
func (c *Controller) seedWorkspaceTrust(ctx context.Context, harness string, sanitizedEnv []string, workerDir string) (string, error) {
	step := PlanTrustSeed(harness, sanitizedEnv, workerDir)
	if !step.Seeds() {
		return "workspace trust not seeded: " + step.Reason, nil
	}
	if c.Trust == nil {
		return "", fmt.Errorf("app: no workspace-trust seeder supplied")
	}
	outcome, err := c.Trust.SeedWorkspaceTrust(ctx, step.ConfigPath, step.ProjectKey)
	if err != nil {
		return "", fmt.Errorf("app: seed workspace trust: %w", err)
	}
	if !outcome.Seeded {
		return "workspace trust not seeded: " + outcome.Reason, nil
	}
	return "workspace trust seeded for " + step.ProjectKey + " (verified; best-effort against external profile writers)", nil
}

// FailLaunchExec settles the launch claim of incarnationID to exec_failed
// with reason: the launcher's own error path once its claim is written and
// the exec — or any step after the claim — fails
// (docs/plan/phase-2-design.md section 6, step 7).
func (c *Controller) FailLaunchExec(ctx context.Context, incarnationID, reason string) error {
	id, err := identity.ParseIncarnationID(incarnationID)
	if err != nil {
		return fmt.Errorf("app: parse incarnation id: %w", err)
	}
	if err := c.Submissions.SettleLaunchFailure(ctx, id, reason); err != nil {
		return fmt.Errorf("app: settle launch failure: %w", err)
	}
	return nil
}

// validateLaunchEnvironment checks the pane-provided HOP_* variables
// against the loaded launch context, fail closed: every variable the
// design's pane environment carries — HOP_STATE_DIR, HOP_RUN_ID,
// HOP_TASK_ID, HOP_ATTEMPT_ID, HOP_INCARNATION_ID — must be PRESENT and
// must agree with the frozen state root, the command's own parsed flags,
// the attempt's task and the incarnation the launch intent recorded. A
// missing or disagreeing variable refuses the launch before any claim.
// Errors name the variable and what disagreed, never the value.
func validateLaunchEnvironment(environ []string, lc *LaunchContext, runID, attemptID string) error {
	expected := []struct{ name, want string }{
		{"HOP_STATE_DIR", lc.Snapshot.StateRoot},
		{"HOP_RUN_ID", runID},
		{"HOP_TASK_ID", lc.Attempt.TaskID.String()},
		{"HOP_ATTEMPT_ID", attemptID},
		{"HOP_INCARNATION_ID", lc.IncarnationID.String()},
	}
	for _, v := range expected {
		got := environValue(environ, v.name)
		if got == "" {
			return fmt.Errorf("app: %s is not set; hop launch runs only in a HOP-created worker pane, which provides it", v.name)
		}
		if got != v.want {
			return fmt.Errorf("app: %s does not agree with the run's launch context; a stale or foreign pane environment never execs", v.name)
		}
	}
	return nil
}

// composeHarnessArgvTail renders the harness argv after the executable
// from the frozen snapshot, per harness. A cold relaunch (attempt
// relaunching) is supported for Claude only — `--resume <native-ref>`,
// where the reference was pre-assigned by HOP before first launch —
// because Codex and opencode native-session capture is unspecified in
// Phase 2. First launches are supported for all three harnesses, sharing
// the one fixed initial prompt: Claude `--session-id <ref> <prompt>`
// (Claude alone needs the native reference), Codex `<prompt>`, opencode
// `--prompt <prompt>`. Claude argv is never rendered for another harness.
func composeHarnessArgvTail(lc *LaunchContext, hopPath string) ([]string, error) {
	if lc.Attempt.State == run.AttemptRelaunching {
		if lc.Snapshot.Harness != HarnessClaude {
			return nil, fmt.Errorf("app: cold resume for harness %q is not supported in Phase 2; its native session capture is unspecified — stop the run or continue it manually", lc.Snapshot.Harness)
		}
		ref := lc.Session.NativeSessionRef
		if ref == "" {
			return nil, fmt.Errorf("app: the session has no pre-assigned native session reference; claude argv cannot be composed")
		}
		return []string{"--resume", ref}, nil
	}
	if !filepath.IsAbs(lc.Snapshot.AssignmentPath) {
		return nil, fmt.Errorf("app: the frozen assignment path is not absolute; the initial prompt carries only absolute paths")
	}
	prompt := renderInitialPrompt(lc.Snapshot.AssignmentPath, hopPath)
	switch lc.Snapshot.Harness {
	case HarnessClaude:
		ref := lc.Session.NativeSessionRef
		if ref == "" {
			return nil, fmt.Errorf("app: the session has no pre-assigned native session reference; claude argv cannot be composed")
		}
		return []string{"--session-id", ref, prompt}, nil
	case HarnessCodex:
		return []string{prompt}, nil
	case HarnessOpencode:
		return []string{"--prompt", prompt}, nil
	default:
		return nil, fmt.Errorf("app: harness %q is not one of claude, codex, opencode", lc.Snapshot.Harness)
	}
}

// renderInitialPrompt renders the fixed first-launch prompt: absolute paths
// and identities only, no brief text — the assignment artifact carries the
// brief, and this prompt only points the worker at it and at the submit
// command, including the transient retry instruction
// (docs/plan/phase-2-design.md sections 6-7).
func renderInitialPrompt(assignmentPath, hopPath string) string {
	return fmt.Sprintf("Read your assignment at %s and complete it. "+
		"When your work is committed, submit it by running: %s result submit --summary \"<one-line summary>\" --commit <commit-oid>. "+
		"If the first output line begins with \"transient\", wait briefly and run the exact same command again.",
		assignmentPath, hopPath)
}

// launchArgvDigest computes the canonical digest recorded in a launch
// claim: the version tag, then each argv element as
// `<decimal byte length>:<raw bytes>`, SHA-256 over the whole encoding,
// lowercase hex — unambiguous under any element content.
func launchArgvDigest(argv []string) string {
	var b strings.Builder
	b.WriteString(launchArgvDigestTag)
	for _, arg := range argv {
		fmt.Fprintf(&b, "%d:%s", len(arg), arg)
	}
	return sha256Hex(b.String())
}

// environValue returns the value of name in environ, resolving duplicates
// as SanitizeEnvironment does: the last entry of a name wins. A name-only
// entry and an absent name both return "".
func environValue(environ []string, name string) string {
	for i := len(environ) - 1; i >= 0; i-- {
		entryName, value, _ := strings.Cut(environ[i], "=")
		if entryName == name {
			return value
		}
	}
	return ""
}

// CheckExecRequest is hop check-exec's input: the --op flag value, the
// check argv the controller passed after `--` (validated against the
// frozen argv, which is the one executed), the supervisor's complete
// spawned environment, its own pid, whether it leads its own process
// group, and the composition-supplied executable lookup.
type CheckExecRequest struct {
	OperationID       string
	CheckArgv         []string
	Environ           []string
	PID               int
	LeadsProcessGroup bool
	LookupExecutable  ExecutableLookup
}

// CheckExecPlan is what hop check-exec execs: the frozen check argv exactly
// as recorded — group retirement matches a running member's argv against
// that exact value, so argv is never rewritten — the separately resolved
// absolute path of the binary argv[0] names, and the sanitized complete
// environment.
type CheckExecPlan struct {
	ExecPath string
	Argv     []string
	Env      []string
}

// PrepareCheckExec performs every hop check-exec step before the exec
// itself (docs/plan/phase-2-design.md section 7): it requires the
// supervisor to lead its own process group, durably records its pid as the
// check-exec claim BEFORE anything else can spawn — refusing to run when
// the write fails or the operation is not a current pending check
// execution — then loads the frozen execution context, verifies the argv
// the controller passed equals the frozen check argv, sanitizes the
// spawned environment under the frozen policy and resolves the check
// executable through the sanitized PATH. A failure after the claim exits
// without running the check; the recorded group id remains the retirement
// handle.
func (c *Controller) PrepareCheckExec(ctx context.Context, req CheckExecRequest) (CheckExecPlan, error) { //nolint:gocritic // hugeParam: CheckExecRequest is the driving DTO for hop check-exec, called once per supervisor process.
	opID, err := identity.ParseOperationID(req.OperationID)
	if err != nil {
		return CheckExecPlan{}, fmt.Errorf("app: parse operation id: %w", err)
	}
	if req.PID <= 0 {
		return CheckExecPlan{}, fmt.Errorf("app: supervisor pid %d is not a valid process id", req.PID)
	}
	if !req.LeadsProcessGroup {
		return CheckExecPlan{}, fmt.Errorf("app: hop check-exec does not lead its own process group; the recorded pid must be the group id that retires every descendant")
	}
	if req.LookupExecutable == nil {
		return CheckExecPlan{}, fmt.Errorf("app: no executable lookup supplied")
	}
	if claimErr := c.Submissions.ClaimCheckExec(ctx, opID, req.PID); claimErr != nil {
		return CheckExecPlan{}, fmt.Errorf("app: claim check execution: %w", claimErr)
	}

	cec, err := c.Read.LoadCheckExecutionContext(ctx, opID)
	if err != nil {
		return CheckExecPlan{}, fmt.Errorf("app: load check execution context: %w", err)
	}
	if len(cec.CheckArgv) == 0 {
		return CheckExecPlan{}, fmt.Errorf("app: the frozen check argv is empty")
	}
	if !equalArgv(req.CheckArgv, cec.CheckArgv) {
		return CheckExecPlan{}, fmt.Errorf("app: the check argv passed on the command line does not equal the frozen check argv; only the frozen command runs")
	}
	validated, err := cec.EnvPolicy.Validate()
	if err != nil {
		return CheckExecPlan{}, fmt.Errorf("app: frozen environment policy: %w", err)
	}
	env, _ := SanitizeEnvironment(req.Environ, validated)
	execPath, err := req.LookupExecutable(cec.CheckArgv[0], environValue(env, "PATH"))
	if err != nil {
		return CheckExecPlan{}, fmt.Errorf("app: resolve check executable: %w", err)
	}
	if !filepath.IsAbs(execPath) {
		return CheckExecPlan{}, fmt.Errorf("app: resolved check executable is not an absolute path")
	}
	return CheckExecPlan{ExecPath: execPath, Argv: cec.CheckArgv, Env: env}, nil
}

// equalArgv reports whether two argvs are element-wise equal.
func equalArgv(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CheckSpawnEnvironment composes the complete environment ClaimAndRunCheck
// spawns hop check-exec with: the caller's inherited environment sanitized
// under the run's frozen policy, per the design's rule that the spawn env
// is a composition input built from the frozen run
// (docs/plan/phase-2-design.md section 7). ClaimAndRunCheck itself overlays
// HOP_STATE_DIR afterward.
func (c *Controller) CheckSpawnEnvironment(ctx context.Context, handle RunHandle, environ []string) ([]string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per controller process.
	frozen, err := c.Read.LoadFrozenRun(ctx, handle.runID)
	if err != nil {
		return nil, fmt.Errorf("app: load frozen run: %w", err)
	}
	validated, err := frozen.Snapshot.EnvPolicy.Validate()
	if err != nil {
		return nil, fmt.Errorf("app: frozen environment policy: %w", err)
	}
	env, _ := SanitizeEnvironment(environ, validated)
	return env, nil
}
