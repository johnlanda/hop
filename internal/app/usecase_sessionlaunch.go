package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// SessionLaunchExecRequest is hop launch's Phase 3 input, covering both
// address forms (docs/plan/phase-3-design.md sections 6 and 10): the
// session-addressed form (`--run --session`, every feature-mode pane) and
// the permanent solo shim (`--run --attempt`, the Phase 2 argv solo panes
// keep verbatim), mutually exclusive — exactly one of SessionID and
// AttemptID is set. The shim resolves app-side: the attempt names the
// run's current attempt (ReadStore.LoadRunStatus), that attempt's current
// session is the addressed session, and everything after that is the one
// session-addressed flow — so the flag surface never breaks while the
// Phase 2 LoadLaunchContext port method still retires in slice 6. The
// remaining fields mirror LaunchExecRequest exactly.
type SessionLaunchExecRequest struct {
	RunID     string
	SessionID string
	AttemptID string
	HOPPath   string
	WorkerDir string
	Environ   []string
	PID       int
	// ResolvePath and LookupExecutable are the same composition-supplied
	// seams as LaunchExecRequest's; resolver errors are never echoed.
	ResolvePath      func(path string) (string, error)
	LookupExecutable ExecutableLookup
}

// PrepareSessionLaunchExec performs every hop launch step before the exec
// itself for a session-addressed launch (docs/plan/phase-3-design.md
// section 6; the Phase 2 contract of PrepareLaunchExec, generalized per
// role): it resolves the addressed session (directly, or through the solo
// shim's attempt resolution), loads the session launch context lease-free
// through WorkflowReadStore — failing closed with the typed
// ErrWorkflowReadStoreUnsupported before any side effect when the wired
// store predates Phase 3 — validates the pane-provided HOP_* environment
// against it fail-closed (the session-addressed pane set includes
// HOP_SESSION_ID and HOP_ROLE; the solo shim's Phase 2 pane set does not
// provide them, so on that path they are validated whenever present —
// a present-but-empty variable is a disagreement, never absent; an
// attempt-bearing session requires HOP_TASK_ID and HOP_ATTEMPT_ID, and a
// manager session refuses either entry outright, empty included), refuses on a stop request, a
// session that is not launching, or a claim identity mismatch, sanitizes
// the environment under the frozen policy, composes the role's argv from
// frozen run facts (worker and implementer share the Phase 2 prompt
// shapes byte for byte — the implementer's pointing at its per-attempt
// assignment artifact — while manager and reviewer carry their own
// role-specific shapes; cold relaunch is Claude-only `--resume <ref>
// <continuation prompt>` selected by the context's successor fact, never
// attempt state), cross-checks the launcher's directory against the
// recorded worktree (or, for the manager, the recorded repository root),
// applies the workspace-trust pre-seed, and records the session-keyed
// launch claim. Any failure returns before the claim is written; errors
// name environment variables but never echo their values.
func (c *Controller) PrepareSessionLaunchExec(ctx context.Context, req SessionLaunchExecRequest) (LaunchExecPlan, error) { //nolint:gocritic // hugeParam: SessionLaunchExecRequest is the driving DTO for hop launch, called once per launcher process.
	runID, err := identity.ParseRunID(req.RunID)
	if err != nil {
		return LaunchExecPlan{}, fmt.Errorf("app: parse run id: %w", err)
	}
	if (req.SessionID == "") == (req.AttemptID == "") {
		return LaunchExecPlan{}, fmt.Errorf("app: exactly one of the session and attempt addresses is required; the two launch forms are mutually exclusive")
	}
	if req.PID <= 0 {
		return LaunchExecPlan{}, fmt.Errorf("app: launcher pid %d is not a valid process id", req.PID)
	}
	if !filepath.IsAbs(req.HOPPath) {
		return LaunchExecPlan{}, fmt.Errorf("app: hop executable path is not absolute; the initial prompt carries only absolute paths")
	}
	if !filepath.IsAbs(req.WorkerDir) {
		return LaunchExecPlan{}, fmt.Errorf("app: the launcher working directory is not absolute; the workspace-trust seed key must be the resolved worker directory")
	}
	if req.LookupExecutable == nil {
		return LaunchExecPlan{}, fmt.Errorf("app: no executable lookup supplied")
	}
	if req.ResolvePath == nil {
		return LaunchExecPlan{}, fmt.Errorf("app: no path resolver supplied")
	}

	wf, err := RequireWorkflowReadStore(c.Read, "PrepareSessionLaunchExec")
	if err != nil {
		return LaunchExecPlan{}, err
	}

	shim := req.AttemptID != ""
	var sessionID identity.SessionID
	var shimAttemptID identity.AttemptID
	if shim {
		shimAttemptID, sessionID, err = c.resolveShimSession(ctx, runID, req.AttemptID)
		if err != nil {
			return LaunchExecPlan{}, err
		}
	} else if sessionID, err = identity.ParseSessionID(req.SessionID); err != nil {
		return LaunchExecPlan{}, fmt.Errorf("app: parse session id: %w", err)
	}

	slc, err := wf.LoadSessionLaunchContext(ctx, runID, sessionID)
	if err != nil {
		return LaunchExecPlan{}, fmt.Errorf("app: load session launch context: %w", err)
	}
	if shim && slc.AttemptID != shimAttemptID {
		return LaunchExecPlan{}, fmt.Errorf("app: the resolved session is not bound to the requested attempt; a stale or foreign launcher invocation never execs")
	}
	if envErr := validateSessionLaunchEnvironment(req.Environ, &slc, runID.String(), sessionID.String(), !shim); envErr != nil {
		return LaunchExecPlan{}, envErr
	}
	if slc.StopRequested {
		return LaunchExecPlan{}, fmt.Errorf("app: the run has a stop request; the session is not launched")
	}
	if slc.Session.State != run.SessionLaunching {
		return LaunchExecPlan{}, fmt.Errorf("app: session is %s, not launching; this launcher invocation is not current", slc.Session.State)
	}
	if slc.Claim != nil {
		if slc.Claim.PID != req.PID {
			return LaunchExecPlan{}, fmt.Errorf("app: a launch claim for this incarnation already exists with a different pid; a duplicate launcher never execs")
		}
		if slc.Claim.State != LaunchClaimExecPending {
			return LaunchExecPlan{}, fmt.Errorf("app: the launch claim for this incarnation is already settled %s; this incarnation never execs again", slc.Claim.State)
		}
	}

	// The frozen policy's Harness names the WORKER harness, but the
	// session's own harness is authoritative for what execs here — a
	// [roles.reviewer] harness may differ — and SanitizeEnvironment
	// selects the profile assignment from the policy's harness.
	// Sanitizing under the worker's harness hands a cross-harness
	// reviewer the WRONG profile variable (a Claude worker policy with a
	// profile directory would leave a Codex reviewer with
	// CLAUDE_CONFIG_DIR set and no CODEX_HOME, so the configured profile
	// would not govern the executed binary — it could run under the
	// user's default account instead). The policy value is copied and its
	// harness set to the session's before validation: the frozen snapshot
	// is never mutated, the frozen strip/passthrough/profile-directory
	// values are kept, and the resulting environment feeds the trust
	// seed and the exec alike.
	policy := slc.Snapshot.EnvPolicy
	policy.Harness = string(slc.Harness)
	validated, err := policy.Validate()
	if err != nil {
		return LaunchExecPlan{}, fmt.Errorf("app: frozen environment policy: %w", err)
	}
	env, _ := SanitizeEnvironment(req.Environ, validated)

	tail, err := composeSessionArgvTail(&slc, runID, req.HOPPath)
	if err != nil {
		return LaunchExecPlan{}, err
	}
	executable, err := req.LookupExecutable(string(slc.Harness), environValue(env, "PATH"))
	if err != nil {
		return LaunchExecPlan{}, fmt.Errorf("app: resolve harness executable: %w", err)
	}
	if !filepath.IsAbs(executable) {
		return LaunchExecPlan{}, fmt.Errorf("app: resolved harness executable is not an absolute path")
	}
	argv := append([]string{executable}, tail...)
	digest := launchArgvDigest(argv)
	// The same claim-identity rule as PrepareLaunchExec: an existing
	// same-pid exec_pending claim authorizes only the exact invocation it
	// recorded.
	if slc.Claim != nil && (slc.Claim.Executable != executable || slc.Claim.ArgvDigest != digest) {
		return LaunchExecPlan{}, fmt.Errorf("app: the existing launch claim for this incarnation records a different executable or argv than this invocation composed; a claim is never rewritten and this launcher never execs")
	}

	workerDir, err := c.resolveSessionWorkerDir(ctx, runID, &slc, req.ResolvePath, req.WorkerDir)
	if err != nil {
		return LaunchExecPlan{}, err
	}
	seedEvidence, err := c.seedWorkspaceTrust(ctx, string(slc.Harness), env, workerDir)
	if err != nil {
		return LaunchExecPlan{}, err
	}

	claim := LaunchClaim{
		IncarnationID: slc.IncarnationID,
		RunID:         runID,
		SessionID:     sessionID,
		AttemptID:     slc.AttemptID,
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
	return LaunchExecPlan{Argv: argv, Env: env, IncarnationID: slc.IncarnationID.String(), SeedEvidence: seedEvidence}, nil
}

// resolveShimSession is the permanent solo shim's attempt resolution
// (design sections 10 and 12 row 4): the attempt must be the run's
// current attempt (RunDetail.AttemptID — the Phase 2 single-attempt
// field), and that attempt's current session is the addressed session.
// Fail closed on a mismatch or a missing session; error text never
// carries an identifier value beyond what the caller already supplied.
func (c *Controller) resolveShimSession(ctx context.Context, runID identity.RunID, rawAttemptID string) (identity.AttemptID, identity.SessionID, error) {
	attemptID, err := identity.ParseAttemptID(rawAttemptID)
	if err != nil {
		return "", "", fmt.Errorf("app: parse attempt id: %w", err)
	}
	detail, err := c.Read.LoadRunStatus(ctx, runID)
	if err != nil {
		return "", "", fmt.Errorf("app: load run status: %w", err)
	}
	if detail.AttemptID != attemptID {
		return "", "", fmt.Errorf("app: the attempt is not the run's current attempt; the attempt-addressed launch form resolves only the current attempt")
	}
	if detail.SessionID == "" {
		return "", "", fmt.Errorf("app: the attempt has no current session; the launch cannot be addressed")
	}
	return attemptID, detail.SessionID, nil
}

// validateSessionLaunchEnvironment checks the pane-provided HOP_*
// variables against the loaded session launch context, fail closed
// exactly as Phase 2 specifies: every variable the role's pane
// environment carries must be PRESENT and must agree. The
// session-addressed pane set (feature mode) additionally provides
// HOP_SESSION_ID and HOP_ROLE, so requireSessionVars is true on that
// path; the solo shim's Phase 2 pane set does not provide them, so there
// they are validated whenever PRESENT — and presence is the entry
// existing at all, so a variable that is present but EMPTY is a
// disagreement, never treated as absent. An attempt-bearing session
// requires HOP_TASK_ID and HOP_ATTEMPT_ID; a manager session has neither
// and refuses a pane environment that carries either entry at all, an
// explicitly empty one included — a stale or foreign pane. Errors name
// the variable, never the value.
func validateSessionLaunchEnvironment(environ []string, slc *SessionLaunchContext, runID, sessionID string, requireSessionVars bool) error {
	type expectation struct {
		name     string
		want     string
		required bool
	}
	expected := []expectation{
		{"HOP_STATE_DIR", slc.Snapshot.StateRoot, true},
		{"HOP_RUN_ID", runID, true},
		{"HOP_INCARNATION_ID", slc.IncarnationID.String(), true},
		{"HOP_SESSION_ID", sessionID, requireSessionVars},
		{"HOP_ROLE", string(slc.Session.Role), requireSessionVars},
	}
	if slc.AttemptID != "" {
		expected = append(expected,
			expectation{"HOP_TASK_ID", slc.Attempt.TaskID.String(), true},
			expectation{"HOP_ATTEMPT_ID", slc.AttemptID.String(), true},
		)
	} else {
		for _, name := range []string{"HOP_TASK_ID", "HOP_ATTEMPT_ID"} {
			if _, present := environEntry(environ, name); present {
				return fmt.Errorf("app: %s is set but this session has no attempt; a stale or foreign pane environment never execs", name)
			}
		}
	}
	for _, v := range expected {
		got, present := environEntry(environ, v.name)
		if !present {
			if v.required {
				return fmt.Errorf("app: %s is not set; hop launch runs only in a HOP-created pane, which provides it", v.name)
			}
			continue
		}
		if got != v.want {
			return fmt.Errorf("app: %s does not agree with the session's launch context; a stale or foreign pane environment never execs", v.name)
		}
	}
	return nil
}

// environEntry reports name's value in environ and whether ANY entry for
// name exists, resolving duplicates exactly as environValue does (the
// last entry wins; a name-only entry is present with an empty value).
// validateSessionLaunchEnvironment needs the distinction environValue
// erases: a variable that is present but empty must fail the agreement
// check, never pass as absent.
func environEntry(environ []string, name string) (value string, present bool) {
	for i := len(environ) - 1; i >= 0; i-- {
		entryName, entryValue, _ := strings.Cut(environ[i], "=")
		if entryName == name {
			return entryValue, true
		}
	}
	return "", false
}

// composeSessionArgvTail renders the harness argv after the executable
// for a session-addressed launch, per role and per the context's
// successor fact. Worker and implementer keep the Phase 2 prompt shapes
// byte for byte — the worker pointing at the run's frozen assignment
// artifact, the implementer at its per-attempt assignment artifact —
// while the manager and reviewer carry their role-specific shapes. Cold
// relaunch (Relaunch true) is Claude-only, `--resume` immediately
// adjacent to the durable native reference with the role's continuation
// prompt as the trailing element, exactly as the restored-harness
// predicate requires; Codex and opencode cold resume reports the
// unsupported state unchanged. First launches compose per harness as
// Phase 2 fixes them: Claude `--session-id <ref> <prompt>`, Codex
// `<prompt>`, opencode `--prompt <prompt>`.
func composeSessionArgvTail(slc *SessionLaunchContext, runID identity.RunID, hopPath string) ([]string, error) {
	prompt, err := sessionPrompt(slc, runID, hopPath)
	if err != nil {
		return nil, err
	}
	if slc.Relaunch {
		if slc.Harness != run.HarnessClaude {
			return nil, fmt.Errorf("app: cold resume for harness %q is not supported; its native session capture is unspecified — stop the run or continue it manually", slc.Harness)
		}
		ref := slc.Session.NativeSessionRef
		if ref == "" {
			return nil, fmt.Errorf("app: the session has no pre-assigned native session reference; claude argv cannot be composed")
		}
		return []string{"--resume", ref, prompt.continuation}, nil
	}
	switch slc.Harness {
	case run.HarnessClaude:
		ref := slc.Session.NativeSessionRef
		if ref == "" {
			return nil, fmt.Errorf("app: the session has no pre-assigned native session reference; claude argv cannot be composed")
		}
		return []string{"--session-id", ref, prompt.initial}, nil
	case run.HarnessCodex:
		return []string{prompt.initial}, nil
	case run.HarnessOpenCode:
		return []string{"--prompt", prompt.initial}, nil
	default:
		return nil, fmt.Errorf("app: harness %q is not one of claude, codex, opencode", slc.Harness)
	}
}

// sessionPrompts carries one role's rendered initial and continuation
// prompts; both are composed so the relaunch path never re-derives paths
// under different rules than the first launch did.
type sessionPrompts struct {
	initial      string
	continuation string
}

// sessionPrompt renders the role's prompts from frozen run facts only.
// Every referenced path must be absolute, per the Phase 2 invariant that
// prompts carry only absolute paths.
func sessionPrompt(slc *SessionLaunchContext, runID identity.RunID, hopPath string) (sessionPrompts, error) {
	switch slc.Session.Role {
	case run.RoleWorker:
		if !filepath.IsAbs(slc.Snapshot.AssignmentPath) {
			return sessionPrompts{}, fmt.Errorf("app: the frozen assignment path is not absolute; the prompt carries only absolute paths")
		}
		return sessionPrompts{
			initial:      renderInitialPrompt(slc.Snapshot.AssignmentPath, hopPath),
			continuation: renderContinuationPrompt(slc.Snapshot.AssignmentPath, hopPath),
		}, nil
	case run.RoleImplementer:
		path, err := boundAttemptAssignmentPath(slc, runID)
		if err != nil {
			return sessionPrompts{}, err
		}
		return sessionPrompts{
			initial:      renderInitialPrompt(path, hopPath),
			continuation: renderContinuationPrompt(path, hopPath),
		}, nil
	case run.RoleReviewer:
		path, err := boundAttemptAssignmentPath(slc, runID)
		if err != nil {
			return sessionPrompts{}, err
		}
		return sessionPrompts{
			initial:      renderReviewerInitialPrompt(path, hopPath),
			continuation: renderReviewerContinuationPrompt(path, hopPath),
		}, nil
	case run.RoleManager:
		fields, err := managerPromptFields(slc, runID)
		if err != nil {
			return sessionPrompts{}, err
		}
		return sessionPrompts{
			initial:      renderManagerInitialPrompt(fields.assignment, fields.role, fields.crib, hopPath),
			continuation: renderManagerContinuationPrompt(fields.assignment, fields.role, fields.crib, hopPath),
		}, nil
	default:
		return sessionPrompts{}, fmt.Errorf("app: session role %q has no launch prompt; only worker, manager, implementer and reviewer launch", slc.Session.Role)
	}
}

// boundAttemptAssignmentPath derives an attempt-bearing session's
// assignment path, refusing a context that carries no attempt identity:
// without the guard, a malformed store row would derive a
// plausible-looking path one directory level up instead of failing
// closed, and the launched session would be pointed at an artifact
// nothing ever writes.
func boundAttemptAssignmentPath(slc *SessionLaunchContext, runID identity.RunID) (string, error) {
	if slc.AttemptID == "" {
		return "", fmt.Errorf("app: the session context carries no attempt identity; a %s launch cannot derive its assignment path", slc.Session.Role)
	}
	path := attemptAssignmentPath(slc.Snapshot.StateRoot, runID, slc.AttemptID)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("app: the derived assignment path is not absolute; the prompt carries only absolute paths")
	}
	return path, nil
}

// managerPromptPaths are the three frozen artifacts the manager's prompts
// reference.
type managerPromptPaths struct {
	assignment string
	role       string
	crib       string
}

// managerPromptFields resolves the manager's prompt inputs from the
// frozen snapshot: the brief assignment artifact, the frozen manager role
// copy and the worker-protocol crib, each required absolute.
func managerPromptFields(slc *SessionLaunchContext, runID identity.RunID) (managerPromptPaths, error) {
	if !filepath.IsAbs(slc.Snapshot.AssignmentPath) {
		return managerPromptPaths{}, fmt.Errorf("app: the frozen assignment path is not absolute; the prompt carries only absolute paths")
	}
	rolePath := slc.Snapshot.Workflow.ManagerRolePath
	if !filepath.IsAbs(rolePath) {
		return managerPromptPaths{}, fmt.Errorf("app: the frozen manager role artifact path is missing or not absolute; a feature run freezes it before any launch")
	}
	return managerPromptPaths{
		assignment: slc.Snapshot.AssignmentPath,
		role:       rolePath,
		crib:       workerProtocolCribPath(slc.Snapshot.StateRoot, runID),
	}, nil
}

// resolveSessionWorkerDir cross-checks the launcher's working directory
// per role: an attempt-bearing session must launch in its recorded
// worktree (the Phase 2 rule verbatim), and the manager must launch in
// the run's recorded repository root — both canonically resolved through
// the request's seam, both refusing fail-closed with resolver errors
// never echoed. The resolved directory is the workspace-trust seed key.
func (c *Controller) resolveSessionWorkerDir(ctx context.Context, runID identity.RunID, slc *SessionLaunchContext, resolve func(string) (string, error), workerDir string) (string, error) {
	if slc.Session.Role != run.RoleManager {
		return resolveWorkerDirAgainstWorktree(resolve, workerDir, slc.WorktreePath)
	}
	frozen, err := c.Read.LoadFrozenRun(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("app: load frozen run: %w", err)
	}
	if frozen.RepositoryRoot == "" {
		return "", fmt.Errorf("app: the run has no recorded repository root; the manager launcher directory cannot be validated and is never seeded or execed")
	}
	resolvedWorker, err := resolve(workerDir)
	if err != nil {
		return "", fmt.Errorf("app: the launcher working directory could not be canonically resolved; an unresolvable directory is never seeded or execed")
	}
	resolvedRoot, err := resolve(frozen.RepositoryRoot)
	if err != nil {
		return "", fmt.Errorf("app: the recorded repository root could not be canonically resolved; the launch is refused rather than run against unverified evidence")
	}
	if resolvedWorker != resolvedRoot {
		return "", fmt.Errorf("app: the manager launcher working directory does not resolve to the run's recorded repository root; a foreign directory is never seeded or execed")
	}
	return resolvedWorker, nil
}

// attemptAssignmentPath is the deterministic artifact path for one
// attempt's assignment artifact (the implementer's task assignment, or
// the reviewer's review assignment), under the run's artifact directory —
// the app-side derivation both the launch prompt and the artifact writer
// (usecase_schedule.go's assignment pass) share, so the referenced file
// and the written file can never disagree.
func attemptAssignmentPath(stateRoot string, runID identity.RunID, attemptID identity.AttemptID) string {
	return filepath.Join(stateRoot, "runs", runID.String(), "attempts", attemptID.String(), "assignment.md")
}

// workerProtocolCribPath is the deterministic artifact path of the run's
// worker-protocol crib, rendered from the grammar constants at freeze.
func workerProtocolCribPath(stateRoot string, runID identity.RunID) string {
	return filepath.Join(stateRoot, "runs", runID.String(), "artifacts", "worker-protocol.md")
}

// renderReviewerInitialPrompt renders the reviewer's fixed first-launch
// prompt: absolute paths and the verdict-submission command only — the
// review assignment artifact carries the frozen subject, the diff scope
// and the reviewer role reference, and this prompt only points at it.
// The verb spelling and the transient-retry protocol are quoted from the
// grammar constant set, and test-side reproductions mirror this function
// byte for byte, citing it.
func renderReviewerInitialPrompt(assignmentPath, hopPath string) string {
	return fmt.Sprintf("Read your review assignment at %s and evaluate the frozen subject it names. "+
		"When your review is complete, submit your verdict by running: %s %s --verdict <approve|reject> --subject <commit-oid> --reasons-file <absolute path>. "+
		"If the first output line begins with \"transient\", wait briefly and run the exact same command again.",
		assignmentPath, hopPath, GrammarVerbReviewSubmit)
}

// renderReviewerContinuationPrompt renders the reviewer's fixed
// cold-relaunch continuation prompt, the positional argument after
// `--resume <native-ref>`, mirroring renderContinuationPrompt's shape for
// the same reason it exists: a restored interactive session never re-runs
// a pending turn on --resume.
func renderReviewerContinuationPrompt(assignmentPath, hopPath string) string {
	return fmt.Sprintf("You were relaunched after an interruption; your restored session may show earlier, unfinished work. "+
		"Re-read your review assignment at %s and continue it. "+
		"When your review is complete, submit your verdict by running: %s %s --verdict <approve|reject> --subject <commit-oid> --reasons-file <absolute path>. "+
		"If the first output line begins with \"transient\", wait briefly and run the exact same command again.",
		assignmentPath, hopPath, GrammarVerbReviewSubmit)
}

// renderManagerInitialPrompt renders the manager's fixed first-launch
// prompt: the three frozen artifacts (brief assignment, frozen manager
// role copy, worker-protocol crib) by absolute path, and the message
// polling command quoted from the grammar constant set. Everything else
// the manager needs is in those artifacts, never in the prompt.
func renderManagerInitialPrompt(assignmentPath, rolePath, cribPath, hopPath string) string {
	return fmt.Sprintf("You are this run's manager. Read your assignment at %s, your role instructions at %s and the worker protocol reference at %s before doing anything else; together they are your complete instructions. "+
		"Coordinate the run only through the hop verbs the protocol reference quotes, and poll for messages by running: %s %s.",
		assignmentPath, rolePath, cribPath, hopPath, GrammarVerbMsgWait)
}

// renderManagerContinuationPrompt renders the manager's fixed
// cold-relaunch continuation prompt: the same three artifacts, the same
// polling instruction, prefixed by the relaunch notice every continuation
// prompt carries.
func renderManagerContinuationPrompt(assignmentPath, rolePath, cribPath, hopPath string) string {
	return fmt.Sprintf("You were relaunched after an interruption; your restored session may show earlier, unfinished work. "+
		"Re-read your assignment at %s, your role instructions at %s and the worker protocol reference at %s, then continue coordinating the run. "+
		"Poll for messages by running: %s %s.",
		assignmentPath, rolePath, cribPath, hopPath, GrammarVerbMsgWait)
}
