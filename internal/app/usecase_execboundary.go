package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/johnlanda/hop/internal/domain/identity"
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

// LaunchExecPlan is what hop launch execs once PrepareSessionLaunchExec
// has validated, seeded, claimed and composed: the harness argv (argv[0]
// the resolved absolute executable recorded in the claim) and the
// sanitized complete environment. IncarnationID is the claimed incarnation
// as a string, so a failed exec can settle the claim through
// FailLaunchExec without the caller holding a typed identity. SeedEvidence
// is the workspace-trust pre-seeding outcome exactly as the claim
// recorded it.
type LaunchExecPlan struct {
	Argv          []string
	Env           []string
	IncarnationID string
	SeedEvidence  string
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

// renderInitialPrompt renders the fixed first-launch prompt: absolute paths
// and identities only, no brief text — the assignment artifact carries the
// brief, and this prompt only points the worker at it and at the submit
// command, including the transient retry instruction
// (docs/plan/phase-2-design.md sections 6-7).
func renderInitialPrompt(assignmentPath, hopPath string) string {
	return fmt.Sprintf("Read your assignment at %s and complete it. "+
		"When your work is committed, submit it by running: %s result submit --summary \"<one-line summary>\" --commit <commit-oid>. "+
		"If the first output line begins with \"transient\", follow its instruction, then wait briefly and run the exact same command again.",
		assignmentPath, hopPath)
}

// renderContinuationPrompt renders the fixed cold-relaunch continuation
// prompt, the positional argument after `--resume <native-ref>`. It exists
// because interactive Claude Code restores a resumed session's transcript
// but does not re-run a pending user turn (observed live against claude
// 2.1.270; the positional prompt is documented by `claude --help`, its
// continuation verified by the human-run live test —
// docs/architecture/native-harness-compat.md), so a restored
// session with no new prompt sits idle at its input box forever. Like the
// initial prompt it is built from durable run facts only — the frozen
// absolute assignment path and the same `<hop> result submit` command line
// as the initial brief — never environment values, and it is passed as an
// argv element via execve, never typed into the pane. This function is the
// one source of truth for the template; test-side reproductions
// (test/integration's fixture worker and scenario assertions) mirror it
// byte for byte and cite it.
func renderContinuationPrompt(assignmentPath, hopPath string) string {
	return fmt.Sprintf("You were relaunched after an interruption; your restored session may show earlier, unfinished work. "+
		"Re-read your assignment at %s and continue it. "+
		"When your work is committed, submit it by running: %s result submit --summary \"<one-line summary>\" --commit <commit-oid>. "+
		"If the first output line begins with \"transient\", follow its instruction, then wait briefly and run the exact same command again.",
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
// argv the controller passed after `--` (validated against the frozen
// argv, which is the one executed), the supervisor's complete spawned
// environment, its own pid, whether it leads its own process group, and
// the composition-supplied executable lookup. The operation may be either
// exec-claimable kind (docs/plan/phase-3-design.md sections 3 and 8): a
// check.run execution, whose frozen argv is the run's check command, or
// an integration.merge execution, whose frozen argv is the intent's
// noninteractive merge command — LoadCheckExecutionContext resolves the
// frozen argv by operation kind, and this boundary treats both
// identically.
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
// itself (docs/plan/phase-2-design.md section 7; generalized to both
// exec-claimable kinds by docs/plan/phase-3-design.md sections 3 and 8):
// it requires the supervisor to lead its own process group, durably
// records its pid as the check-exec claim BEFORE anything else can
// spawn — refusing to run when the write fails or the operation is not a
// pending check.run or integration.merge execution of the current
// generation — then loads the frozen execution context (argv resolved by
// operation kind: check.run's frozen check command, or the merge intent's
// frozen noninteractive merge argv), verifies the argv the controller
// passed equals that frozen argv byte for byte (group retirement matches
// the running argv, so it is never rewritten), sanitizes the spawned
// environment under the frozen policy and resolves the executable through
// the sanitized PATH. A failure after the claim exits without running
// anything; the recorded group id remains the retirement handle.
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
