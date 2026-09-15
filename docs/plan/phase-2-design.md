# Phase 2 design: durable single-worker run

Status: design for [phases.md](phases.md) Phase 2, revised through two
cross-provider review rounds. Nothing in this document is implemented; it
fixes the decisions an implementer executes without re-deriving them. It
applies the settled account decisions: accounts are delegated to the
harnesses (workers run in each harness's default profile), HOP performs no
logins and stores no credentials, and a sanitizing launcher at the harness
exec boundary strips provider credential variables by default. HOP runs on
the user's normal Herdr server with no configuration change. Persistence is
SQLite via `modernc.org/sqlite` (pure Go); the CLI stays on the standard
`flag` package. The first worker is Claude Code in the default profile
against a fixture repository created by the tests.

Trust model, stated once: Phase 2 enforces cooperative controller
transitions and freezes policy selection. It is not a security boundary
against the worker, which runs as the same user with a full shell and can in
principle write the store or artifacts directly (see
[RESEARCH.md](../../RESEARCH.md), "Important verified limits"). The controls
in this design protect against accidental races, stale processes and crashed
controllers, not against a malicious same-UID agent.

A bounded real-process spike (task 0, section 10) preceded port freezing;
all seven items (S1–S7, plus the S3/S4 and layout.apply follow-ups) are
established on herdr 0.9.0 with executed evidence and folded into this
revision; section 11 tabulates them.

## 1. Scope and exit criteria

Phase 2 delivers four commands — `hop run "<brief>"`, `hop status`, `hop stop`
and a scoped `hop resume` — plus three plumbing commands the workflow needs
(`hop result submit`, invoked by the worker; `hop launch`, the sanitizing
exec-boundary launcher; and `hop check-exec`, the check exec boundary the
controller spawns). One brief becomes one run with frozen effective
instructions, one task, one attempt and one native worker launched in a
dedicated worktree. The controller is the foreground `hop run` process.

Flow: `hop run` loads repository policy from `.herdr-orchestrator/`, freezes
the run snapshot (check argv, timeout and repeatability, environment policy,
harness, the assignment artifact and its digest, the absolute state root,
generated identities including a pre-assigned native session reference),
initializes the run rows and the controller lease in one transaction,
persists a launch intent, creates a worktree through Herdr, and creates the
worker pane with the launch command argv, the additive environment and a
unique creation label in one request (the layout.apply transport of
section 6; a send-text line into a shell pane is the documented fallback),
and waits. `hop launch` — the process the line starts — resolves
everything else from the store, records a durable launch claim
(`exec_pending`), and execve's the harness with the sanitized environment
and an initial-prompt argv referencing the assignment artifact (section 6);
the claim settles to `execed` only on corroborating observation. The worker
submits an explicit result tagged with run/task/attempt identities; Herdr
idle/done never completes a task. The controller validates the result, runs
the frozen deterministic check against an isolated detached checkout of the
submitted commit, and only a passing check completes the task and run.
Artifacts, state transitions and interrupted-operation evidence are
persisted throughout.

Applied delegation decisions:

- Workers run in the harness's default profile. HOP never logs in, never
  reads or writes `~/.claude`, `~/.codex` or opencode state, and stores no
  credential or account entity.
- An optional configuration value may name a user-prepared alternate profile
  directory; HOP resolves it to an absolute path at freeze time and only
  passes it through (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, or opencode's
  documented `HOME`/XDG layout, section 6). No pools, account leases, or
  cross-account resume; those stay in Phase 8 (account phasing per the
  delegated-accounts planning revision).
- The launcher strips provider credential, provider-routing and
  profile-override variables by default, with explicit opt-in passthrough
  per repository or per run (section 6).

Exit criteria (restated from [phases.md](phases.md) with how each is proved):

| Criterion | Evidence |
| --- | --- |
| Repeatable brief → worker → result → check → completion | Real-process test with the deterministic fixture worker, which must read and validate the delivered assignment (section 9); opt-in live test with Claude Code |
| Duplicate result submission is idempotent | Accepted-receipt lookup precedes state preconditions (section 7); tested for immediate retries, retries after every terminal state, and retries after controller takeover |
| Stale results fail | New-content submission with a rotated launch incarnation, a superseded attempt, or a stopped run is rejected and recorded |
| Crash before/after launch does not silently duplicate workers | The operation decision table (section 4) makes an unresolved prior-generation intent, an unsettled launch claim, or an in-flight pane creation block replacement; interruption tests kill the controller and the launcher at every window; a barrier test resumes controller A after controller B has taken over |
| A failed check blocks completion | Fixture repository configured with a failing check; run ends `failed` with retained evidence, never `completed` |
| Board closure / client detach is distinct from stop | No board exists yet; the integration test detaches the observing client, closes HOP-opened non-worker panes and kills the controller (detach path, section 5), then proves the worker keeps running and `hop resume` reattaches |

Out of scope for Phase 2: manager sessions, messages, multiple tasks, second
attempts after a failed check (Phase 3 — see the retry distinction in
section 7), playbooks, gates beyond the single required check, account
pools, the board, and detached (non-foreground) controllers.

## 2. Domain slice

New packages: `internal/domain/identity` and `internal/domain/run`. No
Message, Playbook or Account entity is created. Domain code receives time,
identities and digests as explicit inputs; it never reads a clock, generates
an ID or computes a hash (hashing stays in the application layer, section 7,
so the checker's pure-package classifier is not expanded).

### internal/domain/identity

Strongly typed IDs shared across boundaries, with pure parsing:

- `RepositoryID`, `RunID`, `TaskID`, `AttemptID`, `SessionID`, `WorktreeID`,
  `ResultID`, `ArtifactID`, `OperationID`, `IncarnationID` — each a distinct
  string type wrapping a UUID, with `Parse<Kind>ID(string) (<Kind>ID, error)`
  validating canonical lowercase UUID form (implemented with `strings` and
  `strconv`; no third-party UUID package) and a `String()` method. IDs are
  generated only through the application's `IDGenerator` port.

### internal/domain/run

Entities, their invariants and pure transitions (states in section 5):

| Entity | Owned state | Invariants |
| --- | --- | --- |
| `Run` | Repository ID, sequence number, brief digest, state, stop request | One task in this phase; completion requires the task completed; a stop request is monotonic (cannot be withdrawn) and takes precedence over completion in any settling transition |
| `Task` | Run ID, instructions digest, state | At most one active attempt; a task completes only through an accepted result plus a passing check |
| `Attempt` | Task ID, number, state | Numbers are dense from 1; a new attempt is Phase 3 — within Phase 2 one attempt may be executed by a sequence of sessions over time (initial launch, then cold relaunches), with at most one non-terminated session and one current incarnation at any instant; result acceptance for new content requires the current incarnation |
| `Session` | Run ID, attempt ID, role, harness, native session reference (assigned or captured, with its source), state | One session executes at most one attempt; a cold relaunch is a new session bound to the same attempt, never a revived old one; Phase 2 has exactly one worker session per incarnation and no manager; the native reference lives on the run's worker lineage and is immutable once assigned |
| `RuntimeBinding` | Session ID, incarnation ID, server identity, workspace/tab/pane IDs, creation label, launch kind (initial, resume, restored-observed), occupant identity (label + argv marker + pid; the S2-verified surface has no process start time, so a pid is never evidence alone), observed-at, superseded flag | Append-only history, one row per launch incarnation plus observed-restoration rows; observations, closes and retirements are valid only against a current (non-superseded) binding; supersession requires recorded evidence, never assumption |
| `Worktree` | Repository ID, run ID, path, branch, state | Created before launch; preserved by stop and by failure |
| `Result` | Attempt ID, commit object ID, summary, content digest (opaque validated string, computed by the application), accepted flag | At most one accepted result per attempt; an accepted receipt is immutable and never replaced; equal digest resubmission is idempotent in every state including terminal ones; a different digest for the same attempt is a conflict and does not disturb the accepted result |
| `Artifact` | Run ID, optional result ID, kind, path, digest | References files under the run's artifact directory, never blobs in domain state |

Pure transition functions take the current value, the inputs that justify the
transition and an explicit `now time.Time`, and return the next value or a
typed error (`ErrInvalidTransition`, `ErrStaleSubmission`,
`ErrConflictingResult`, `ErrDuplicateResult` distinguishing the idempotent
case, `ErrTransientNotRunning` for the early-submission case in section 7).
Result acceptance is a pure function of a complete application-assembled
context: attempt state, launch-claim settlement for the current incarnation,
run stop state, prior accepted result, and the submitted digest. The digest
is an opaque string the application has already validated and canonicalized
(section 7); the domain never hashes.

### Category matrix additions

Per [architecture.md](../architecture/architecture.md) and the rule table in
[internal/arch_test.go](../../internal/arch_test.go):

| Package | Category | firstParty | thirdParty |
| --- | --- | --- | --- |
| `internal/domain/identity` | `shared-domain` | none | none |
| `internal/domain/run` | `domain` | `internal/domain/identity` | none |
| `internal/app` (existing) | `application` | gains `internal/domain/identity`, `internal/domain/run` | none |
| `internal/adapters/sqlite` | `driven-adapter` | `internal/app`, `internal/domain/run`, `internal/domain/identity` | `modernc.org/sqlite` |
| `internal/adapters/system` | `driven-adapter` | `internal/app`, `internal/domain/identity` | none |
| `internal/adapters/process` | `driven-adapter` | `internal/app` | none |
| `internal/adapters/config` | `driven-adapter` | `internal/app` | `github.com/pelletier/go-toml/v2` |
| `internal/adapters/herdr` (existing) | `driven-adapter` | unchanged (`internal/app`) | none |
| `cmd/hop` (existing) | `composition` | gains the four new adapter packages | none |
| `test/integration` (existing) | `integration-test` | gains the packages its scenarios drive | unchanged |

The pure-standard allowlists for the two domain packages stay inside the
existing classifier (`errors`, `fmt`, `strings`, `strconv`, `sort`, `time`
as needed) — no `crypto/*` or `encoding/*` entries, and no expansion of
`isPureStandardPackage`. The ambient time/randomness bans are untouched.
Composition (`cmd/hop`) may not import domain packages, so commands pass raw
strings to application-facing parsers and DTOs; `internal/app` converts them
to typed IDs. The grouping directory `internal/domain` gains an index
AGENTS.md, and each new package gains a leaf guide, in the change that
creates it. Each implementer task adds its own packages' rule rows and guide
links on its branch (so every branch passes the checker standalone); one
integrator, assigned before task 1, resolves all merge conflicts in
`internal/arch_test.go` and
[internal/adapters/AGENTS.md](../../internal/adapters/AGENTS.md) and lands
branches one at a time (section 10).

## 3. Ports (consumer-owned, in internal/app)

Reused Phase 1 symbols: `app.Probe` (doctor), `app.Invocation`,
`app.AgentPresentation` / `app.Presenter` / `app.AgentDisplay` /
`app.SortDisplays` (the run controller publishes the worker pane's
`hop_run`/`hop_role`/`hop_task`/`hop_state` tokens), and in the adapter
`herdr.Client`, `herdr.InstallationProbe`, `herdr.Presentation`,
`herdr.Observer`. The Phase 1 `app.Observer`/`app.Reconcile` machinery is
reused with its real contract, not beyond it: the Herdr observer subscribes
only to `pane.agent_status_changed` and its snapshots carry no process
identity, and `app.Reconcile` returns only on context cancellation or stream
end — so status events serve as wakeups and bootstrap only, every
termination or occupant decision is established by bounded `InspectPane` and
snapshot polling, and resume bounds its bootstrap `Reconcile` call with a
short context (default 5s) before handing off to continuous watching. If the
spike (S2) verifies a richer lifecycle/process event source, a follow-up may
tighten this; the design does not assume one. The intentional-deferral note
in [internal/app/AGENTS.md](../../internal/app/AGENTS.md) is discharged: the
worker-launch use case is the first consumer of `Runtime`, `Clock` and
`IDGenerator`.

New ports declared in `internal/app`:

### StateStore / UnitOfWork (controller authority)

```go
type StateStore interface {
    // InitializeRun atomically creates the repository row (get-or-create by
    // resolved root path), the run with its sequence number, the frozen
    // snapshot, the task, attempt and session rows, and the initial
    // controller lease, in one transaction. This is the only way a run and
    // its first lease come into being, so there is no bootstrap cycle.
    InitializeRun(ctx context.Context, spec NewRunSpec) (RunID, Lease, error)

    // AcquireLease claims or takes over an existing run's controller lease
    // and returns the new fencing generation. Generations are monotonic for
    // the life of the run row, preserved across release and reacquisition.
    // Acquisition succeeds only when the row is released or expired.
    AcquireLease(ctx context.Context, run RunID, controllerID string) (Lease, error)
    // Heartbeat and ReleaseLease are compare-and-swap mutations: they
    // succeed only when the row still matches (run, controller_id,
    // generation, state = held). A stale controller can neither extend nor
    // release a successor's lease. Release marks the row released; it never
    // deletes it and never touches the generation.
    Heartbeat(ctx context.Context, lease Lease) error
    ReleaseLease(ctx context.Context, lease Lease) error

    // Begin opens a controller unit of work bound to the lease. Commit
    // re-reads the lease inside the transaction and fails with ErrFenced
    // unless it still matches (run, controller_id, generation, held) AND is
    // unexpired at commit time (expiry rule: parsed expires_at earlier than
    // the transaction's clock reading).
    Begin(ctx context.Context, lease Lease) (UnitOfWork, error)
}

type UnitOfWork interface {
    Runs() RunRepository            // and Tasks(), Attempts(), Sessions(),
                                    // Worktrees(), Results(), Artifacts(),
                                    // Bindings(), Operations(), Transitions(),
                                    // CheckRequests()
    Commit() error
    Rollback() error
}
```

Typed repositories return domain values, never rows. Every mutable entity
carries a revision; `Save(entity, expectedRevision)` returns
`ErrRevisionConflict` when the row moved. `OperationRepository` records
intent/outcome journal entries (section 4).

### ReadStore (lease-free reads)

Status rendering, `hop launch` and `hop check-exec` read without any lease:

```go
type ReadStore interface {
    ListRuns(ctx context.Context, repository RepositoryID) ([]RunStatus, error)
    LoadRunStatus(ctx context.Context, run RunID) (RunDetail, error)
    // LoadLaunchContext returns what the launch exec boundary needs: the
    // frozen snapshot (policy, harness, assignment path, native reference),
    // the session's current binding and incarnation, the launch-claim state
    // and the stop state.
    LoadLaunchContext(ctx context.Context, run RunID, attempt AttemptID) (LaunchContext, error)
    // LoadCheckExecutionContext returns what the check exec boundary needs,
    // addressed by the check execution's operation ID: the frozen env
    // policy, the absolute state root, the candidate checkout path and the
    // frozen check argv. hop check-exec additionally receives its absolute
    // HOP_STATE_DIR from the controller in the spawned environment, so it
    // resolves the same store without any fallback.
    LoadCheckExecutionContext(ctx context.Context, op OperationID) (CheckExecutionContext, error)
}
```

### SubmissionStore (worker and non-controller writes)

Worker-side and third-party writes do not hold the controller lease and are
never stamped with a controller generation. Each method is one internal
transaction with its own contract (section 4, "Transaction authorities"):

```go
type SubmissionStore interface {
    // ClaimLaunch is written by hop launch BEFORE exec: run, attempt,
    // incarnation, the expected executable's resolved absolute path, argv
    // digest, own pid, state exec_pending. It fails —
    // and the caller must not exec — when the run
    // is stopping or stopped, the incarnation is not current, or a claim
    // for this incarnation already exists with a different pid. A rewrite
    // by the same pid is idempotent. SettleLaunchFailure records
    // exec_failed on the launcher's error path.
    ClaimLaunch(ctx context.Context, claim LaunchClaim) error
    SettleLaunchFailure(ctx context.Context, incarnation IncarnationID, reason string) error
    // ClaimCheckExec is written by hop check-exec BEFORE exec: operation
    // id and own pid (its process-group id). It fails when the operation is
    // not a pending check execution of the current generation.
    ClaimCheckExec(ctx context.Context, op OperationID, pid int) error
    // SubmitResult applies the validation order of section 7 atomically,
    // including the attempt and task transitions on acceptance.
    SubmitResult(ctx context.Context, submission ResultSubmission) (SubmissionOutcome, error)
    // RequestStop sets the run's monotonic stop request without a lease.
    RequestStop(ctx context.Context, run RunID) error
}
```

Settlement of a launch claim to `execed` is a controller write (it requires
corroborating observation, section 6), not a launcher write.

### Runtime (extends the Phase 1 Herdr adapter)

```go
type Runtime interface {
    CreateWorktree(ctx context.Context, req WorktreeRequest) (WorktreeInfo, error) // worktree.create; no env — see launch-environment.md
    OpenWorkerPane(ctx context.Context, req WorkerPaneRequest) (PaneHandle, error) // layout.apply: one pane whose command IS the launch argv, with env, cwd and creation label
    FindPaneByLabel(ctx context.Context, label string) (PaneRef, bool, error)       // recovery lookup when the create response was lost (S7: labels round-trip through tab.list/session.snapshot); returns the pane/tab/workspace IDs the ownership policy needs
    SendText(ctx context.Context, paneID, text string) error                        // fallback transport ONLY: the fixed-grammar launch line (section 6)
    ReadPane(ctx context.Context, paneID string, lines int) (string, error)         // evidence snapshots; captured periodically — scrollback vanishes with the pane
    InspectPane(ctx context.Context, paneID string) (PaneProcess, error)            // occupant identity for launch/stop/adoption decisions
    ClosePane(ctx context.Context, paneID string) error                             // stop interruption; callers apply the close rule below
}
```

Implemented by a new `runtime.go` in `internal/adapters/herdr` over the
existing `Client`. `WorkerPaneRequest` carries the launch command argv
(absolute executable path — S5 showed bare names are unreliable under macOS
`path_helper` PATH reordering), the worktree cwd, a unique creation label
(the launch operation ID; S7 verified creation labels round-trip through
`layout.apply` responses and `session.snapshot`), and the additive env map
from [launch-environment.md](../architecture/launch-environment.md):
`HOP_STATE_DIR` (the frozen absolute state root), `HOP_RUN_ID`,
`HOP_TASK_ID`, `HOP_ATTEMPT_ID`, `HOP_INCARNATION_ID` (unique per launch) —
all removal happens in the launcher, and S6 verified the env map plus the
`HERDR_*` identity are delivered to the command process. `PaneHandle`
returns the workspace, tab and pane IDs for the runtime binding.
`PaneProcess` fields are the S2-verified surface: shell pid, foreground
process group id, and per foreground process pid, name, argv0, full argv,
cmdline and cwd. There is NO process start time anywhere in that surface,
so a pid is never reuse-proof on its own: occupant identity is always the
creation label plus the argv marker (the run/attempt/incarnation or native
session UUIDs visible in the full argv) plus the pid — never the pid alone.
Close rule, used identically by stop, retirement and recovery: immediately
before `ClosePane`, the caller re-inspects and matches the occupant's argv
and label against the recorded evidence for that specific target — a
current binding's claim identity, or an observed-restoration binding's
positive evidence (section 5) — and on mismatch, missing identity, or
inspection failure it fails closed (no close, operation `reconciling`).
S2 established that no identity-conditioned close exists upstream
(`pane.close` takes only a pane id), so the residual inspect-then-close
race is a documented limitation, minimized by the immediate re-inspection,
not papered over.

### Clock and IDGenerator

```go
type Clock interface{ Now() time.Time }
type IDGenerator interface{ NewID() string } // canonical lowercase UUID; parsed into typed IDs by the caller
```

Implemented in `internal/adapters/system` (`crypto/rand` UUIDv4,
`time.Now().UTC()`). Tests use handwritten fakes with fixed sequences.

### CommandRunner and Exec

```go
type Command struct {
    Argv []string
    Dir  string
    Env  []string // complete environment, not additions
}
type CommandResult struct {
    ExitCode int
    Stdout   []byte
    Stderr   []byte
    Duration time.Duration
}
type CommandRunner interface {
    // Run starts the argv as the leader of a new process group and kills
    // the whole group on ctx cancellation. Durable execution identity is
    // not this port's job: the exec-boundary commands record their own pid
    // before exec (sections 6-7).
    Run(ctx context.Context, cmd Command) (CommandResult, error)
}
```

Implemented in `internal/adapters/process`; used for the git operations and
for spawning `hop check-exec`. The same package provides
`Exec(argv []string, env []string) error` (a `syscall.Exec` wrapper that
only returns on failure), used by the `hop launch` and `hop check-exec`
commands.

### ProcessGroupInspector

Group retirement after takeover has no surviving `CommandRunner` context to
cancel, so it is a consumer-owned port injected into the stop and resume
use cases (testable against fakes in task 2), implemented in
`internal/adapters/process`:

```go
type ProcessGroupInspector interface {
    // GroupProcesses lists the group's members (pid and argv each) via the
    // platform process table; a listing that cannot be made is an error,
    // never an empty result.
    GroupProcesses(ctx context.Context, pgid int) ([]GroupProcess, error)
    // SignalGroup kills the whole group.
    SignalGroup(ctx context.Context, pgid int) error
}
```

Ownership decisions stay in `internal/app`: the retirement function
classifies the listing against the expected argv and returns a typed
outcome — `empty` (already gone, no signal), `matched` (signal, then await
absence), `mismatched` (never signaled; `reconciling` with the listing as
evidence) or `inspection failed` (fail closed) — per the group-retirement
rule of section 7. Herdr's pane surface has no process start time, so group
identity is always corroborated by argv, never by pid or pgid alone.

### ConfigurationSource

```go
type ConfigurationSource interface {
    Load(ctx context.Context, repositoryRoot string) (RunPolicy, error)
}
```

Implemented in `internal/adapters/config`: reads
`.herdr-orchestrator/config.toml` at the repository root with
`github.com/pelletier/go-toml/v2` — a maintained TOML 1.0 decoder with
strict unknown-field rejection and no transitive dependencies; a hand-rolled
line parser would be an undocumented TOML subset. Unknown keys, duplicate
keys and type mismatches fail validation with the offending key named.

```go
type RunPolicy struct {
    CheckArgv       []string      // [check] command; required, Load fails without it
    CheckTimeout    time.Duration // [check] timeout; default 10m
    CheckRepeatable bool          // [check] repeatable; default false — section 7 unknown-outcome rule
    EnvStrip        []string      // [env] strip additions to the harness matrix
    EnvPassthrough  []string      // [env] passthrough opt-ins
    ProfileDir      string        // [profile] dir; resolved to an absolute path against the repository root at load
    Harness         string        // [worker] harness; default "claude"; Phase 2 launches only claude
}
```

Complete example of the file:

```toml
[check]
command = ["sh", "check.sh"]
timeout = "10m"
repeatable = true # this check is safe to re-run after an unknown outcome

[env]
strip = ["MY_ORG_PROXY_TOKEN"]
passthrough = ["OPENAI_API_KEY"]

[worker]
harness = "claude"

[profile]
dir = ".profiles/claude-alt" # optional; resolved absolute at load
```

### Launcher and environment policy

The launcher is not a port: it is the `hop launch` command (section 6) plus
a pure application function
`SanitizeEnvironment(environ []string, policy EnvPolicy) (env []string, removed []string)`
in `internal/app`. `EnvPolicy` is a versioned value frozen into the run
snapshot; precedence within it is defined in section 6. The policy value,
the version-scoped strip matrix and `SanitizeEnvironment` are
credential-handling code: they live in their own file in `internal/app`
(`launchenv.go`) authored and owned by task 4 (section 10), with no
dependency on task 2 — its only consumers are the two exec-boundary
commands, `hop launch` and `hop check-exec`, wired in task 6a; controller
use-case code never calls it.

## 4. Persistence

### Store location and layout

One user-scoped SQLite store shared by all repositories and runs, per
[architecture.md](../architecture/architecture.md). The state root
resolution rule is one shared resolver owned by `cmd/hop` (task 6a), which
resolves once per command and passes the absolute path to the SQLite
adapter: `${XDG_STATE_HOME:-$HOME/.local/state}/hop`, with `HOP_STATE_DIR`
as the only override. `HERDR_PLUGIN_STATE_DIR` is deliberately not
consulted. Every entrypoint applies the identical rule, so under the same
effective environment every command resolves the same store; the doctor's
new store-path line (computed by the shared resolver without opening or
migrating the database) reports the resolved path and labels the source
(`override` or `default`). At `hop run` the resolved root is made absolute,
frozen into the run snapshot, and handed to the worker as `HOP_STATE_DIR` in
the pane's additive env. A worker-context command (`hop launch`,
`hop result submit` relying on `HOP_*` ids) REQUIRES that provided absolute
`HOP_STATE_DIR`: if it is missing or relative, the command fails with a
diagnostic naming the lost variable — it never falls back to the default
resolution, which in a worker context could silently open an unrelated
store. Layout:

```text
<state root>/
  hop.db                                one database for all repositories and runs
  runs/<run-uuid>/artifacts/            assignment artifact, pane snapshots, stop evidence
  runs/<run-uuid>/checks/<operation-uuid>/   per-execution outputs; tree/ is a detached
                                             git worktree of the candidate, removed after
                                             evidence capture (section 7)
```

Runs partition by `repository_id` and `run_id` columns, not by database
file. Repository identity is the symlink-resolved absolute path of the
repository root, held in `repositories` with a generated `RepositoryID`.

### Schema (initial migration)

All IDs are UUID TEXT. All times are fixed-width canonical UTC TEXT,
`2006-01-02T15:04:05.000000000Z` (nine-digit fractional seconds, always
`Z`), so equal-length lexical comparison agrees with time order; code still
compares parsed times, never raw strings, for lease and expiry decisions.
Every mutable table has `revision INTEGER NOT NULL` (optimistic concurrency:
updates run `... WHERE id = ? AND revision = ?` and zero affected rows is
`ErrRevisionConflict`). Foreign keys are enforced.

| Table | Columns (abridged) | Constraints |
| --- | --- | --- |
| `schema_migrations` | version PK, applied_at | forward-only |
| `repositories` | id PK, root_path, created_at | UNIQUE(root_path); get-or-create raced across processes resolves through this constraint |
| `runs` | id PK, repository_id FK, seq, brief, state, stop_requested_at NULL, revision, created_at, updated_at | UNIQUE(repository_id, seq); seq assigned inside InitializeRun's transaction |
| `run_snapshots` | run_id PK FK, check_argv JSON, check_timeout_ms, check_repeatable, env_policy JSON (versioned), harness, profile_dir NULL (absolute), state_root (absolute), assignment_path, assignment_digest, created_at | immutable after insert |
| `tasks` | id PK, run_id FK, state, revision, … | one row per run in this phase |
| `attempts` | id PK, task_id FK, number, state, revision, … | UNIQUE(task_id, number); partial UNIQUE(task_id) WHERE state in active set |
| `sessions` | id PK, run_id FK, attempt_id FK, role, harness, native_session_ref NULL, native_ref_source NULL (assigned, captured), state, revision, … | reference immutable once set; a cold relaunch inserts a new session for the same attempt |
| `runtime_bindings` | id PK, session_id FK, incarnation_id, server_socket_path, server_instance NULL (the opaque, adapter-formatted identity of the server process behind the configured socket at binding creation — the herdr adapter formats it from the socket peer pid, e.g. `peer-pid:42111`; NULL/empty means unknown; compared by equality only for the resume continuity check; peer-pid recycling is a documented residual limitation, start-time hardening is a Phase 7 recovery item), workspace_id, tab_id, pane_id, creation_label, occupant_evidence JSON NULL (label + argv marker + pid; never pid alone), launch_kind (initial, resume, restored-observed), observed_at, superseded, superseded_evidence NULL | append-only; UNIQUE(session_id, incarnation_id) |
| `launch_claims` | incarnation_id PK, run_id, attempt_id, executable (expected absolute harness/fixture path, for the section 6 predicate), argv_digest, pid, state (exec_pending, execed, exec_failed), error NULL, claimed_at, settled_at NULL, settlement_evidence NULL | claimed by hop launch before exec; settled to execed only by the controller on corroboration; a claim with a different pid for an existing incarnation is rejected |
| `worktrees` | id PK, repository_id FK, run_id FK, path, branch, base_commit, state, created_at | UNIQUE(path); adoption validates repository and base, not path existence (decision table) |
| `results` | id PK, attempt_id FK, commit_oid, summary, content_digest, accepted, submitted_at | partial UNIQUE(attempt_id) WHERE accepted = 1; UNIQUE(attempt_id, content_digest) |
| `result_submissions` | id PK, claimed_run_id, claimed_task_id, claimed_attempt_id, claimed_incarnation_id, content_digest NULL, outcome (accepted, duplicate, stale, conflicting, transient, malformed), detail, submitted_at | claimed ids are plain TEXT, no FKs, so malformed submissions still leave evidence |
| `check_requests` | id PK, result_id FK, state (requested, claimed, settled), created_at, claimed_generation NULL | UNIQUE(result_id) |
| `artifacts` | id PK, run_id FK, result_id FK NULL, kind, path, digest, created_at | |
| `transitions` | id PK, entity_kind, entity_id, from_state, to_state, reason, generation NULL, at | append-only evidence; generation NULL for non-controller writes |
| `operations` | id PK, run_id FK, generation, kind, state (pending, succeeded, failed, reconciling), intent JSON, act_evidence JSON NULL, outcome JSON NULL, created_at, updated_at | the operation journal; act_evidence records observations made between act and outcome; human attestations (`hop resume --confirm-absent`) are recorded as journal entries of kind `absence.attested` |
| `run_leases` | run_id PK FK, controller_id, generation, state (held, released), acquired_at, heartbeat_at, expires_at | generation monotonic for the life of the row; the row is created only by InitializeRun and never deleted or reinserted |

### Connections, transactions and driver

- Driver pinned to an exact `modernc.org/sqlite` version in `go.mod`, added
  by its first importer (task 3; section 10).
- Per-connection settings are applied by the DSN so every physical
  connection in the `database/sql` pool gets them:
  `?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)`.
  A connection-churn test verifies the PRAGMAs on freshly created
  connections, not just the first.
- Write transactions begin `immediate`; a busy begin, or a transaction the
  database reports rolled back, is retried whole with bounded backoff.
  External acts are never inside a transaction and are dispatched only
  after a confirmed commit, never from inside a retry body; an ambiguous
  commit is reconciled by re-reading the operation row by its ID, never by
  repeating the act.
- Migrations: ordered, embedded SQL files
  (`internal/adapters/sqlite/migrations/NNN_*.sql` via `embed`). The
  migrator opens an immediate transaction, re-reads the schema version
  inside it, applies only still-pending migrations, and records them; two
  processes opening concurrently therefore serialize correctly. A store
  newer than the binary's latest known migration is an error. Forward-only.

### Transaction authorities

Three write authorities exist, with different fencing:

1. Controller transactions (`StateStore.Begin`): stamped with and fenced by
   the run lease; `Commit` re-reads the lease inside the transaction and
   fails with `ErrFenced` unless it matches (run, controller, generation,
   held) and is unexpired. A stale or released controller can never commit
   new state, extend a successor's lease (heartbeat CAS), or release it
   (release CAS).
2. Worker transactions (`SubmissionStore`): never stamped with a controller
   generation — a worker is not a controller and must survive controller
   takeover. They are validated by the launch incarnation: the pane env
   carries `HOP_INCARNATION_ID`, minted at launch-intent time; a
   new-content submission is accepted only for the session's current,
   non-superseded incarnation. Warm takeover preserves the incarnation (the
   surviving worker stays valid); a cold relaunch mints a new one (the old
   worker's new submissions become stale). This is accidental-staleness
   protection, not authentication: a cooperative subprocess inherits the
   variable; a worker is never told to fetch a newer incarnation. Duplicate
   result receipts are exempt (section 7).
3. Stop requests (`RequestStop`) and lease operations: single-row writes
   with their own CAS contracts as above. Stop is monotonic.

### The transaction rule and the operation decision table

External calls never happen inside a database transaction. Every
controller-side effect follows record-intent, act, record-outcome:

1. In one transaction: apply the domain transition, write the `operations`
   row as `pending` with the full intent payload, commit.
2. Immediately before dispatch, revalidate: heartbeat fresh, generation
   unchanged, stop not requested (unless the act is itself part of
   stopping). Then perform the external call. Observations made during the
   act (a returned pane ID) are recorded promptly in `act_evidence` — a
   plain write, not an external call. For process spawns, durable identity
   is written by the spawned exec-boundary command itself before exec
   (launch claims, check-exec claims), which closes the
   spawned-but-unrecorded window to the interval between fork and the
   child's own write — and that window is treated as ambiguous, never as
   absence.
3. In a second transaction: write the observed outcome, mark the operation
   `succeeded` or `failed`, apply the consequent transition, commit.

Fencing the outcome commit does not fence the act itself: a descheduled
controller can perform an already-committed intent after takeover. The
decision table below is therefore normative; the barrier test (controller A
resumes after controller B takes over) exercises it. Unresolved
prior-generation intents block replacement even when a snapshot currently
shows absence — and snapshots are weak evidence in both directions: S3
established that a post-restart `session.snapshot` can report an agent
before any process is live (a phantom), so presence or absence of a
snapshot row never substitutes for live process inspection, and
`pane.exited` carries no exit status, so no pane event ever proves worker
success.

| Operation | Crash between intent and act | Crash between act and outcome | Takeover with the intent unresolved |
| --- | --- | --- | --- |
| `worktree.create` | Adoption requires provenance, not path existence: the path exists AND `git -C <path>` reports the intended repository and base commit; a path occupied by an unrelated checkout is a failure, not adoption; a Herdr-reported in-progress worktree operation is ambiguous, wait bounded | same provenance validation | same; never create a second worktree path for the run |
| `pane.open` | Pane creation is NOT treated as repeatable and cwd/workspace match is NOT adoption evidence (many legitimate panes can share both). Adoption requires the unique creation label (S7-verified: labels round-trip through the layout.apply/tab.create response, tab.list and session.snapshot), recovered by label lookup even when the create response was lost; no label found → bounded wait for the in-flight request to surface, then reconciling, never a second create | same | same |
| `launch.send` (fallback transport only) and launch-pane creation | Claim state decides. No claim row → the launch may still be in flight (a buffered line, or a pane not yet spawned); NEVER re-send or re-create; reconciling with a pane snapshot as evidence. Claim `exec_pending` → a launcher process exists or existed; ambiguous until corroborated (settle to execed) or retired (close under the close rule, against the claim's argv/label evidence). Claim `execed` → adopt. Claim `exec_failed` → the incarnation is dead; relaunch requires a new incarnation through the normal path | same claim-state logic | an unresolved launch operation or unsettled claim from a prior generation blocks relaunch until the claimed process is conclusively retired or settles |
| `pane.close` (stop, retirement) | Re-inspect; occupant already gone → adopt; still present and argv/label evidence matches the recorded target → re-act; mismatch → fail closed | same | same |
| `check.run` | No check-exec claim → either nothing spawned or the child died pre-write; AMBIGUOUS, never absence: bounded wait for a claim, then retire any recorded claim's group under the group-retirement rule of section 7, else escalate to reconciling | claim present → takeover retires that process group under the section 7 rule (local argv verification before signaling; an empty or unmatched group is never signaled), waits for group absence, then applies the unknown-outcome rule | same as previous column; a resumed prior controller's dispatch is fenced at its next store write and its spawned group is retired by the claim |

Lease rules: `AcquireLease` succeeds only when the row is `released` or
expired (parsed times); it increments the generation in place. Heartbeat TTL
30s, interval 10s; a controller whose heartbeat write fails or is fenced
cancels its in-flight external calls and exits its act loop.

## 5. Run state machine

All transitions are pure domain functions; the controller drives them
through the operation journal. The tables are exhaustive: a pair not listed
is invalid, and the domain test suite proves complete traces — initial
launch to completion, stop injected at every step, exec failure, warm
takeover, and cold replacement — through all four tables together, not just
pairwise validity.

Reconciling as a run condition, stated explicitly: `reconciling` is NOT a
`Run` state. The persisted run state during recovery is `resuming`; "the
run is reconciling" means the run is `resuming` (or `stopping`) with at
least one operation in state `reconciling`. `hop status` renders the
condition alongside the state.

### Run

| From | To | Cause |
| --- | --- | --- |
| created | launching | launch intent journaled |
| launching | running | launch claim settled `execed` by the section 6 corroboration predicate (agent detection only triggers the inspection, it never settles), or atomically within an early-acceptance transaction (section 7) |
| launching | failed | exec_failed claim, or launch-window expiry with a conclusively retired incarnation and no resume path |
| running | completing | accepted result's check claimed |
| completing | completed | passing check receipt, no stop requested |
| completing | running | check settled failed-unknown with `check.repeatable = true`, new execution pending (section 7) |
| created, launching, running, completing, resuming | stopping | stop requested |
| stopping | stopped | termination of owned work observed (worker and check) |
| launching, running, completing, resuming | failed | terminal failure (rejected candidate, failed check, unresumable loss, unrepeatable unknown check outcome after stop of its worker) |
| created, launching, running, completing, stopping | resuming | `hop resume` acquired the lease |
| resuming | launching, running, completing, stopping, failed | reconciliation outcome (launching on an authorized cold relaunch) |

### Task

| From | To | Cause |
| --- | --- | --- |
| pending | active | attempt launched |
| pending | interrupted | stop before launch |
| active | checking | result accepted |
| checking | completed | passing check |
| active, checking | failed | rejected candidate or failed check |
| active, checking | interrupted | stop |

### Attempt

| From | To | Cause |
| --- | --- | --- |
| reserved | launching | launch intent |
| reserved | interrupted | stop before launch (no act performed) |
| launching | running | claim settled `execed` (section 6 predicate) |
| launching | submitted | early submission accepted: settled claim for the current incarnation (section 7 atomic handoff) |
| launching | failed | exec_failed claim, unrecoverable |
| launching | interrupted | stop during launch; pre-exec claimed process retired |
| launching | reconciling | ambiguous launch (no claim, or unsettled claim) |
| running | submitted | result accepted |
| submitted | checking | check execution started |
| checking | completed | passing check |
| submitted, checking | failed | conflicting/invalid candidate, failed check, or unrepeatable unknown outcome left unresolved |
| running, submitted, checking | interrupted | stop, or established worker loss without a resume path |
| running, submitted, checking | reconciling | takeover or observation loss; identity not yet established |
| reconciling | running, submitted, checking | warm reattach verified (returns to its prior state) |
| reconciling | relaunching | cold relaunch authorized: absence established (positive retirement evidence or recorded human attestation) and supported resume semantics exist |
| reconciling | interrupted | retirement established with no resume path, or stop |
| relaunching | running | new incarnation's claim settled `execed` (section 6 predicate) |
| relaunching | submitted | early submission accepted: settled claim for the new incarnation (section 7 atomic handoff) |
| relaunching | failed | settled exec_failed claim on the new incarnation, unrecoverable |
| relaunching | reconciling | new launch ambiguous |
| relaunching | interrupted | stop during relaunch |

The same attempt is executed by a sequence of sessions: cold relaunch
(`relaunching`) binds a NEW session and incarnation to the attempt; the old
session ends `lost` or `terminated` and is never revived.

### Session

| From | To | Cause |
| --- | --- | --- |
| reserved | launching | launch intent |
| launching | active | claim settled `execed` (section 6 predicate) |
| launching | stopping | stop with a corroborated live process before the active transition; termination is then observed, never declared |
| launching, active | reconciling | ambiguity or takeover |
| reconciling | active | occupant verified against the current binding's claim evidence |
| reconciling | lost | absence conclusively established (positive evidence or attestation) |
| reconciling | stopping | stop requested with a verified current occupant to interrupt |
| reconciling | terminated | stop requested with no live process established, or retirement completed |
| active | stopping | interrupt dispatched |
| stopping | terminated | termination observed |
| reserved, launching | terminated | stopped, or exec failure, with no corroborated live process (a stop against a corroborated live process goes through launching → stopping above) |

A session's settling evidence must come from a current runtime binding; an
observation from a superseded binding is ignored.

Worker-exit observation: under the primary transport the pane's process IS
the worker, and the pane closes when it exits — identically for success and
failure, and `pane.exited` carries NO exit status (its payload is pane id,
type and workspace id; S6 follow-up, established). Worker success therefore
comes only from the result protocol and the check, never from a pane event.
Exit is observed as pane absence together with the launch claim's process
being gone. The controller
captures ReadPane evidence periodically during the run and before
every stop or retirement action — scrollback vanishes with the pane. A
worker exit and a human closing the pane produce the same observable
evidence; HOP distinguishes them only as far as the evidence allows and
reports the ambiguity honestly rather than guessing.

### Reference traces

The domain test suite proves these four exact event orders as complete
four-entity traces (each line: event → Run / Task / Attempt / Session);
they are the orders two implementers could otherwise resolve differently:

1. Fixture worker (name never detected) submits before running: intent →
   launching/active/launching/launching; claim exec_pending → unchanged;
   submission with a still-unsettled claim → `transient` (no transitions);
   claim settled `execed` via the inspection predicate → Run running,
   Attempt running, Session active; resubmission accepted → Run running
   (idempotent — already there), Task checking, Attempt submitted; check
   claimed → Run completing, Attempt checking. Alternative inside the same
   trace: if the claim settles and acceptance wins before the controller
   applies the lifecycle transitions, the acceptance transaction itself
   moves Run launching→running, Task active→checking, Attempt
   launching→submitted atomically, and the controller's later observation
   of the settled claim is a no-op.
2. Cold relaunch, submit before running: relaunch authorized → Run
   resuming→launching, Attempt reconciling→relaunching, new Session'
   reserved→launching (new incarnation); claim settles; submission
   accepted → the same atomic handoff with Attempt relaunching→submitted,
   Run launching→running, Task active→checking; Session'
   launching→active on the settled claim.
3. Exec failure after relaunch: as trace 2 up to the claim, which settles
   `exec_failed` → Attempt relaunching→failed, Session'
   launching→terminated, Task active→failed, Run launching→failed.
4. Stop during launching with a corroborated live process: stop requested
   → Run launching→stopping; Session launching→stopping (interrupt under
   the close rule); termination observed → Session terminated, Attempt
   launching→interrupted, Task active→interrupted, Run stopping→stopped.

### Stop

`hop stop <run-id>` calls `SubmissionStore.RequestStop`. The live controller
(or `hop stop` itself when the lease is free, after acquiring it with a new
generation) then: journals the interrupt intent; marks not-yet-acted work
`interrupted` (a `reserved` attempt is never launched); captures ReadPane
evidence of the worker pane before acting (scrollback vanishes with the
pane); cancels and reaps its own running check via `CommandRunner`
cancellation (process-group kill) and, on takeover, the check-exec claim's
process group under the section 7 group-retirement rule; retires a pre-exec
launch claim (`exec_pending`) by closing under the close rule against the
claim's argv/label evidence; interrupts a running worker by `ClosePane`
under the same rule against the current binding's claim evidence; reports
the run `stopping` until termination of the worker and every check group
is observed (pane absence plus the claim's process gone, by bounded
`InspectPane`/snapshot polling with status events as wakeups — under the
primary transport the pane closes itself when the command exits); then
records `stopped`. Check-outcome transactions re-read the
stop request and record evidence without completing: stop precedence means
a passing check observed after stop was requested yields `interrupted`,
never `completed`. Worktrees and artifacts are preserved. A stop that
cannot observe termination within its deadline leaves the run `stopping`
with a `reconciling` operation and says so; `hop stop` remains available
and re-drives it.

### Controller signals (detach, distinct from stop)

`hop run`/`hop resume` handle SIGINT/SIGTERM as detach, not stop: journal a
controller-detach event, cancel in-flight external calls, release the lease
(CAS to `released`, generation preserved), print the resume instruction and
exit 1. The worker keeps running; the run keeps its state. A second signal
during shutdown exits immediately. Stopping the run is only ever the
explicit `hop stop`.

### Resume

`hop resume <run-id>` acquires the lease (new generation) and reconciles.
The bootstrap is a bounded `app.Reconcile` (subscribe before snapshot,
Phase 1 semantics, short context) followed by continuous watching; decisions
come from `InspectPane` and the store, not from status events alone — and
not from snapshots either: S3 established that after a server restart
`session.snapshot` can report a pane's agent (kind and status) BEFORE any
process is live, because the restore plan is deferred until a client
supplies geometry. A snapshot row is therefore never evidence of a live
process, and absence is established only by live process inspection.
Pending or ambiguous operations from the previous generation are resolved
per the decision table (section 4) before any new act.

1. Surviving worker (warm reattach — supported): the current binding's pane
   exists (found by pane ID or `FindPaneByLabel`) and its occupant
   observation satisfies the section 6 corroboration predicate against the
   settled launch claim — the same predicate as settlement and adoption,
   with no separate warm-reattach rule. S1 established the supported
   process relations: under the fallback transport an `exec` in the
   top-level pane shell leaves harness pid = shell pid = foreground process
   group; under the primary transport the command process is the pane's
   process directly. An occupant matching on executable identity, marker
   and binding but not on pid is the unsupported forking-wrapper topology
   and fails closed exactly as in section 6. Rebind observations, keep the
   incarnation (the worker's submissions stay valid), republish
   presentation tokens (token metadata is not cold-restored, see
   [herdr-surface.md](../architecture/herdr-surface.md)), continue.
2. Restored or replaced occupant — no negative-evidence ownership: a
   restored process was not started through the sanitizing launcher (the
   audited restore path replays no pane env and inherits the server
   environment, see
   [native-harness-compat.md](../architecture/native-harness-compat.md))
   and is never adopted as a conforming worker. But "the pane matches the
   binding and the occupant fails the claim-identity match" proves only
   that a different process occupies the old pane — it equally matches a
   human's unrelated replacement shell — and NEVER authorizes retirement.
   Automatic retirement requires positive evidence tying the occupant to
   the run's native session and server instance: the concrete candidate is
   an observed process command line carrying the run's pre-assigned native
   session reference (`--resume <uuid>` / `--session-id <uuid>`) — S2
   established that full process argv IS observable through pane
   inspection, and S3 established that a recorded native session makes
   Herdr auto-relaunch exactly as `claude --resume <id>` (bypassing
   `hop launch` and dropping the `HOP_*` env — the restored process's
   dump showed `HERDR_*` present and `HOP_RUN_ID` absent), so this
   positive-evidence mechanism is verified end to end.
   Such an occupant is recorded as an
   observed-restoration binding (`launch_kind = restored-observed`, with
   the observation as its evidence) and becomes a guarded retirement
   target: guarded close against exactly that evidence, supersession
   recorded, then cold relaunch as in item 3. Without positive evidence,
   fail closed: `hop resume` reports the pane, the occupant identity it
   observed, and the exact human action (inspect that pane; close it or
   `hop stop` the run; then rerun `hop resume`, or attest absence as
   below). Stop, recovery and the S2/S3 tests use this one rule, including
   a same-kind unrelated replacement case.
3. Absent worker, cold relaunch (supported for Claude): absence
   conclusively established — positive retirement evidence (item 2), a
   settled `exec_failed` claim, or a recorded human attestation
   (`hop resume --confirm-absent`, below) — and supported resume semantics
   exist. The attempt moves to `relaunching`: a NEW session and incarnation
   bound to the same attempt, a fresh pane in the same worktree, the launch
   line as in section 6 with the resume argv
   (`claude --resume <native-ref>`); the native reference is durably
   assigned or verified captured — for Claude it is pre-assigned by HOP
   before first launch (section 6), so no capture is needed. Cold relaunch
   argv is composed per harness from the snapshot's harness field; Claude
   argv is never rendered for another harness, and Codex/opencode cold
   resume is out of Phase 2 scope (their capture is unspecified; resume
   reports an actionable unsupported state).
4. Restore-settled condition (S3, established): there is NO "restoration
   finished" signal — only ordinary detection/status events exist — and a
   deferred restore fires only when a client supplies geometry (plus a
   short theme wait), so snapshot absence never excludes a pending
   restore; worse, a post-restart snapshot can show the agent as a phantom
   before any process runs (the resume-intro note above). After a server
   restart the run therefore stays `resuming` with `reconciling`
   operations and the item-2 report, and relaunches only through item 2
   (positive-evidence retirement of the restored occupant once it actually
   runs). This is the established behavior, not a pending question.
5. Finite exit from a stuck reconciliation: `hop resume --confirm-absent`
   records a human attestation as an `absence.attested` journal entry with
   the reported evidence. The attestation must cover BOTH facts: no worker
   for this run is currently running anywhere, AND every outstanding
   mechanism that could still start one has been retired — for HOP's own
   side that is the launch claims and intents the claim table retires;
   for Herdr it would require canceling or settling a deferred native
   restore, and S3 established that no route for that exists on herdr
   0.9.0. A
   truthful present-tense "no worker is running" cannot retire a restore
   Herdr may still fire on a later client attachment, so:
   `--confirm-absent` does not authorize relaunch while a prior Herdr
   restore or launch request may still execute; those pending mechanisms
   must first be retired independently. Concretely, the flag enables the
   item-3 cold relaunch only when the non-restart case is POSITIVELY
   established by the server-continuity check: the creation binding
   records the server-process identity observed through the configured
   socket immediately before pane creation (`Runtime.ServerInstance`, an
   opaque adapter-formatted token scoped to both the configured socket
   and the server process behind it — equality of non-empty tokens
   implies the same socket and server; the token is frozen into the
   pane.open intent so recovery restores creation evidence, never a
   recovery-time observation), resume observes it again, and continuity
   is established iff both tokens are non-empty and equal
   (`app.ServerContinuityEstablished`). A deferred native restore fires
   only after a server restart, so an unchanged server process with the
   pane gone (positively absent by pane id AND by creation label, each a
   successful observation — an inspection error or an empty-foreground
   pane that still answers is never absence) cannot have a restore
   pending; a restart or live handoff changes the identity and fails
   closed. With continuity
   established, HOP's own pending launch claims/intents are retired by
   the relaunch itself. Unknown continuity, inspection errors and any
   post-restart indication keep the run `resuming` with the item-2
   report naming the observed mismatch or unknown; the case stays there
   until the restored occupant appears and is retired by positive
   evidence — S3 established there is no cancellation or settlement
   route for a deferred restore. The attestation is journaled as an
   `absence.attested` entry with both assertions and the observed
   continuity evidence either way. It is retirement evidence, not a
   bypass: there is no force-relaunch that skips prior-intent
   retirement. `hop stop` is always available as the other finite exit.

Resume tests cover: delayed restore (restore fires after resume's first
snapshot), an inherited key or profile override present on a restored
occupant, a same-kind unrelated replacement occupant (must fail closed),
missing `HOP_*` env on the restored occupant, the alternate-profile
configuration, the attestation path, and the takeover barrier.

## 6. Sanitizing launcher and assignment delivery

### Placement

The launcher must be the argv that finally execs the harness:
[launch-environment.md](../architecture/launch-environment.md) verified that
Herdr's env maps are additive and cannot unset an inherited variable, that
`worktree.create` and `agent.start` carry no env, and that panes default to
login shells whose startup files can re-export provider keys after upstream
sanitation ([native-harness-compat.md](../architecture/native-harness-compat.md)).
The spike settled the transport surfaces: S5 established that `agent.start`
CANNOT interpose a wrapper (its kind must be a known agent label, args only
append after the fixed harness executable, and it types a bare name into
the shell, which macOS `path_helper` PATH reordering can resolve to the
wrong binary — bare names never pin which binary runs). S6 established that
a `layout.apply` pane node carries `command` (argv), `env` and `label`, and
that the argv IS the pane's process — no shell, no rc files, no PATH
ambiguity — with the additive env and `HERDR_*` identity delivered, agent
detection working on a recognized process name, and the pane closing when
the command exits. Detection everywhere is foreground process-name
identification of known harness names; an unrecognized binary name is never
detected as an agent (S1).

### Launch pane (primary transport)

The controller creates the worker pane and its command in one request: a
`layout.apply` adding one pane whose command is the argv

```text
[<abs-hop-path>, "launch", "--run", <run-uuid>, "--attempt", <attempt-uuid>]
```

with cwd = the worktree, the additive env map, and the launch operation ID
as the pane's creation label. All executable paths are absolute — the HOP
path here and the harness path the launcher resolves — never bare names
(S5). The S6 follow-up established that a `layout.apply` addressed by
`workspace_id` only ADDS one tab and leaves every pre-existing tab and
pane untouched (`tab_id` would name a tab to replace, and is not used);
the transport is confirmed as primary, with the fallback below retained as
the documented alternative.

The launch-claim deadline (default 120s from pane creation) bounds
mechanical launcher start: if no claim row appears, the operation is never
re-created or re-sent; it goes `reconciling` with a pane snapshot as
evidence, per the decision table. Human-interaction time after the claim is
unbounded: a claimed-and-corroborated harness waiting at a trust or
permission dialog is reported as `blocked, needs interaction` for as long
as it takes, with no resend and no timeout failure. Detection that arrives
after any internal report is folded in when observed (late evidence is
evidence); absence of detection is never treated as failure while the claim
is unsettled — it stays ambiguous.

### Send-text fallback (documented fallback only)

Verified working by S1 (the line is consumed even when injected immediately
after tab.create, under /bin/sh and a zsh login shell with noisy rc files,
and after `exec` the harness pid equals the shell pid and foreground
group). The controller opens a shell pane with the additive env and a
creation label, waits for `InspectPane` to show the pane's shell as the
foreground process (a precondition, not the authority), and sends exactly
one line:

```text
exec '<abs-hop-path>' launch --run <run-uuid> --attempt <attempt-uuid>
```

The only shell-quoted token is the HOP executable path, single-quoted.
`hop run` refuses at start (usage error, before any side effect) if that
path contains a single quote or any control character; UUIDs are lowercase
hex and hyphens. The line is identical and valid in POSIX sh, bash and zsh
(the supported shells; others are a documented risk). No brief text,
profile path or policy ever appears on the line. The same claim deadline
and never-resend rule apply.

### hop launch behavior

`hop launch --run <uuid> --attempt <uuid>`:

1. Requires the absolute `HOP_STATE_DIR` provided at pane creation (a
   missing or relative value is a diagnostic failure — no fallback
   resolution in a worker context), opens the store through `ReadStore`,
   loads the launch context, and validates: the attempt is current,
   `HOP_INCARNATION_ID` matches the session's current binding, the run is
   not stopping or stopped, and no launch claim exists for this incarnation
   with a different pid. Any failure: no claim, no exec, exit 1 with one
   stderr line.
2. Computes the sanitized environment: `app.SanitizeEnvironment` over its
   own inherited environment with the snapshot's frozen, versioned
   `EnvPolicy`. Precedence, applied in order: start from the inherited
   environment; remove the harness strip matrix plus policy `strip`
   additions; restore `passthrough` entries from their original values
   (passthrough beats strip — it is the opt-in); apply profile assignments
   (profile beats passthrough for the same variable); the Herdr-managed
   `HERDR_*` and the pane's `HOP_*` additions pass through untouched.
3. Composes the harness argv from the snapshot's harness field — for
   Claude, first launch:
   `claude --session-id <native-ref> "<fixed initial prompt>"`; cold
   resume: `claude --resume <native-ref>`. The native reference is a
   crypto-random UUID minted by the controller and persisted on the session
   lineage before the first launch intent. S4 established the contract
   against claude 2.1.270 (scratch profile, credential-free): a
   pre-assigned `--session-id <uuid>` creates the transcript under that
   UUID and `--resume <uuid>` finds it from a different cwd; the contract
   is version-scoped and re-verified on version drift. The initial prompt
   is a fixed
   template containing only absolute paths and identities, no brief text:
   it instructs the worker to read the assignment artifact at its absolute
   path and to submit with the absolute HOP path (`<hop> result submit`),
   including the retry instruction for a `transient` response (section 7).
   Argv is passed via execve — no shell parses it, so multiline or quoted
   brief content never needs escaping.
4. Resolves the harness executable to an absolute path using the sanitized
   environment's PATH.
5. Records the launch claim (`exec_pending`): run, attempt, incarnation,
   the resolved expected executable path, the argv digest, and its own
   pid. execve preserves the pid, so the claim's pid is the harness's pid
   on success. A second `hop launch` for the same incarnation is rejected
   by the claim's different-pid rule and never execs — a duplicate
   launcher invocation cannot mint a second worker.
6. `Exec`s. A failed exec settles the claim to `exec_failed` with the
   error and exits 1; if that settlement write is itself lost, the claim
   stays `exec_pending` and recovery treats it as ambiguous (decision
   table) — a pre-exec write is never read as proof of exec.

Claim settlement — one corroboration predicate, used identically by
settlement, adoption and warm reattach: the claim is an intent
acknowledgment, not proof the harness ran. The controller settles it to
`execed` only when an `InspectPane` observation satisfies ALL of:

- the pane matches the claim's current creation binding (pane ID, or
  recovery by creation label);
- the foreground process's executable identity (name/argv0 resolved path,
  from the S2-verified surface) equals the expected harness or fixture
  executable recorded in the claim — which explicitly excludes a
  still-running `hop launch` (argv0 = the HOP binary, subcommand
  `launch`), so a paused pre-exec launcher can never satisfy the
  predicate;
- the process argv carries the claim's marker;
- the foreground pid equals the claim pid (execve preserves it).

Herdr agent detection (recognized harness names only, per S1) and a valid
`transient`-rejected early submission are wakeups that TRIGGER this
inspection; neither ever settles a claim by itself. An observation that
matches on executable identity, marker and binding but differs on pid is
the forking-wrapper topology, which Phase 2 does not support: it fails
closed — the claim stays `exec_pending` and the run surfaces `needs
interaction` with the observed identity, so the human decides. This is the
settlement path for the test fixture worker too, whose binary name is
never agent-detected. The decision table and warm reattach adopt only
settled claims; an `exec_pending` claim is always ambiguous.

### Assignment artifact

The brief and the full worker instructions are one immutable artifact,
`runs/<run-uuid>/artifacts/assignment.md`, written temp-file-then-rename at
freeze time; its digest and path are recorded in the run snapshot, and the
artifact row references it. Delivery is by reference in the initial-prompt
argv — never typed into a running dialog, so there is no post-launch
prompt-injection step to journal or retype. Acknowledgment semantics, stated
honestly: a settled (`execed`) claim proves the sanitized launcher reached
the exec boundary with the frozen argv (and thus the assignment reference);
whether the model read and followed the file is not mechanically provable —
the fixture worker validates delivered content (including multiline and
quoted text) in tests, and the result protocol is the behavioral evidence in
production.

### Strip matrix (version-scoped)

The default strip set is the union of the credential, provider-routing and
profile-override variables below. It was assembled against Claude Code
2.1.x, Codex 0.154.x and opencode 1.18.x (the versions probed in
[native-harness-compat.md](../architecture/native-harness-compat.md)) plus
current official documentation, and is a named, versioned constant — future
harness versions require re-verification, and the policy `strip` list
extends it per repository. This is sanitation of the exec environment, not
prevention of configuration or credentials a harness loads later by its own
rules.

| Harness family | Stripped by default |
| --- | --- |
| Claude | `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`, `CLAUDE_CODE_OAUTH_TOKEN`, `CLAUDE_SECURESTORAGE_CONFIG_DIR`, `CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX`, `AWS_BEARER_TOKEN_BEDROCK`, `CLAUDE_CONFIG_DIR` |
| Codex / OpenAI | `OPENAI_API_KEY`, `OPENAI_BASE_URL`, `CODEX_HOME` |
| opencode | `OPENCODE_CONFIG`, `OPENCODE_CONFIG_CONTENT`, `OPENCODE_CONFIG_DIR` |

The full union applies regardless of the launched harness (a stray
cross-provider key must not leak into any worker). Profile-selection
variables are stripped so an inherited override cannot silently select a
non-default profile; when the policy names a `profile.dir` (absolute after
load), the launcher sets exactly the harness's documented profile shape:
`CLAUDE_CONFIG_DIR=DIR` (Claude), `CODEX_HOME=DIR` (Codex), and for
opencode the documented layout `HOME=DIR`,
`XDG_DATA_HOME=DIR/.local/share`, `XDG_CONFIG_HOME=DIR/.config`,
`XDG_STATE_HOME=DIR/.local/state`, `XDG_CACHE_HOME=DIR/.cache`. HOP passes
the directory through unmodified and never prepares, inspects or writes it.

### Exec-boundary tests

The integration suite builds a fixture worker executable exec'd through the
real path (`OpenWorkerPane` launch pane → `hop launch`, with the snapshot's
harness argv pointing at the fixture; the send-text fallback exercised
separately). Asserted: every matrix
variable is absent even when deliberately seeded into the Herdr server's
environment; a login-shell rc file that re-exports a stripped variable
(planted in the test's scratch HOME) does not survive; policy `strip`
additions are removed and `passthrough` entries survive with original
values; `profile.dir` sets exactly the documented shape; `HERDR_*` and
`HOP_*` are present; the fixture validates the delivered assignment content
(multiline, quotes); a HOP path containing a space works and one containing
a single quote is refused at `hop run`; a missing `HOP_STATE_DIR` fails
with the diagnostic. Claim-protocol tests: launcher killed after the claim
write and before exec (claim stays `exec_pending`, recovery treats it as
ambiguous, stop retires it); exec failure with the `exec_failed` write
suppressed (still ambiguous, never `execed`); a duplicate `hop launch`
invocation for the same incarnation (rejected, no second exec); on the
fallback transport, a shell that forks before starting hop (the claim's
self-recorded pid stays valid even when it differs from Herdr's shell pid —
relation per S1); a paused pre-exec launcher whose own argv carries the
run/attempt UUIDs (must NOT settle the claim — the executable-identity
conjunct excludes it); and a wrapper that forks the real harness after
exec (fail-closed: the claim stays `exec_pending`, the run surfaces `needs
interaction` with the observed identity, and no lifecycle transition
fires). Process-only tests (no Herdr) cover `app.SanitizeEnvironment`
tables and `adapters/process.Exec` directly.

## 7. Result protocol and deterministic check

### Protocol choice: CLI subcommand

Workers submit results by running `hop result submit`. A file-based protocol
was rejected: it would need a watcher plus atomic-rename conventions inside
the worktree, validation would happen after the fact, and idempotency and
stale rejection would be split between writer and reader. The subcommand
validates against the live store at submission time inside one transaction
and gives the worker an immediate, parseable verdict. This supplies
validation, not authentication of the agent (see the trust model above).

Retry vocabulary, fixed here: retrying an acknowledged submission
(idempotent receipts, below), retrying a `transient` response, and
controller recovery (sections 4–5) are required Phase 2 behavior; creating
a second attempt after a failed check is Phase 3 and is not smuggled in
through any of them.

### Submission

```text
hop result submit --summary "<text>" --commit <oid> [--run ID --task ID --attempt ID]
```

ID flags default from `HOP_RUN_ID`, `HOP_TASK_ID` and `HOP_ATTEMPT_ID`; the
incarnation is read from `HOP_INCARNATION_ID` (no flag — the value is
launch-provided context, though as inherited environment a cooperative
process could alter it; see the trust model). The absolute `HOP_STATE_DIR`
is required as in section 4. Validation order inside one `SubmitResult`
transaction is normative:

1. Parse and bound the inputs: IDs canonical UUIDs; commit a full 40-hex
   lowercase object ID (SHA-256-object-format repositories are out of
   Phase 2 scope and are rejected at repository validation in `hop run`,
   not here); summary at most 4 KiB of valid UTF-8. Failure: recorded in
   `result_submissions` as `malformed` with the claimed values (no foreign
   keys required), exit 1.
2. Existence and agreement: attempt belongs to task, task to run. Failure:
   `malformed`, exit 1.
3. Resolve the existing accepted result for the attempt, before any state
   or incarnation precondition. Equal digest → `duplicate`: idempotent
   success in every state, including `completed`, `failed`, `stopped` and
   after takeover — exit 0, print the existing result ID, enqueue nothing.
   Unequal digest → `conflicting`: recorded, exit 1; the accepted result is
   never replaced and the already-accepted work is not failed by the
   conflicting retry.
4. First acceptance only — eligibility: the submission's incarnation must
   be the session's current, non-superseded binding (else `stale`); the run
   must not be stopping/stopped (stop precedence, else `stale`); the
   attempt must be `running`, or `launching` or `relaunching` with a
   settled (`execed`) launch claim for that incarnation — the
   early-submission case, where a fast initial or cold-relaunched worker
   submits before the controller's lifecycle transition. `launching` or
   `relaunching` with an unsettled claim returns `transient`: recorded,
   exit 1 with first output line `transient: attempt not yet running;
   retry` — the assignment template instructs the worker to retry after a
   short delay, and the controller treats the observed transient
   submission as a wakeup to attempt claim corroboration under the
   section 6 predicate. Any other state is `stale`.
5. Accept, atomically with the receipt — the atomic handoff: insert the
   result (partial unique index enforces one accepted result per attempt),
   record the `accepted` submission, insert the unique `check_requests`
   row, and apply Attempt→`submitted` (from `running`, `launching` or
   `relaunching`), Task→`checking`, and — when the run has not yet reached
   it — Run `launching`→`running`, with their transition-evidence rows,
   all in this same transaction. Exit 0. Subsequent lifecycle observation
   is idempotent: the controller's later corroboration of a claim whose
   attempt is already `submitted` (and run already `running`) is a no-op,
   and check claiming then proceeds through the ordinary
   `running`→`completing` row.

The controller consumes durable pending check requests by polling the store
(2s interval) while using status events only as wakeups; a notification is
never the only trigger.

### Canonical digest (application-owned)

Version tag `hop-result-v1`. Encoding: the ASCII tag, then each field as
`<decimal byte length>:<raw bytes>` in fixed order — run UUID, task UUID,
attempt UUID, commit object ID, summary bytes. SHA-256 over that byte
string, lowercase hex. Computed only in `internal/app`
(`crypto/sha256`/`encoding/hex` live there, not in domain packages); the
domain receives the digest as an opaque validated string together with the
complete acceptance context (attempt state, claim settlement, incarnation
currency, run stop state, prior result).

### Deterministic check

On claiming a `check_requests` row, the controller runs one check execution
per the intent/act/outcome rule; the operation ID is the check execution ID
and namespaces all its files.

1. Candidate isolation — detached checkout, not archive: the check never
   runs in the live worktree, which the worker can keep changing. The
   controller validates the submitted oid is a commit object
   (`git -C <recorded repository root> cat-file -t <oid>` = `commit`; the
   `-C` pins the recorded root so cwd can never select a subtree), records
   the materialized tree oid (`git rev-parse <oid>^{tree}`) in the
   operation intent, then materializes
   `git -C <recorded repository root> worktree add --detach
   runs/<run-uuid>/checks/<operation-uuid>/tree <oid>`. A detached checkout
   keeps Git metadata available (git-dependent checks work) and does not
   pass through archive attribute transforms. It is removed with
   `git worktree remove --force` after evidence capture; outputs stay.
   Export contract, explicit: the Phase 2 check must be self-contained in
   that checkout — submodules are unsupported and fail clearly, and
   network or shared-path side effects are the check author's
   responsibility under the cooperative trust model.
2. Execution through the check exec boundary: the controller spawns
   `hop check-exec --op <operation-uuid> -- <frozen check argv...>` via
   `CommandRunner` (which makes it a process-group leader) with `Dir` = the
   checkout. `hop check-exec` verifies it leads its own process group,
   durably records its pid (= the group id) as the check-exec claim
   (`ClaimCheckExec`) — refusing to run if that write fails or the
   operation is not a current pending check — applies
   `app.SanitizeEnvironment` with the frozen policy, and execve's the check
   argv. The pid-before-exec trick from the launcher closes the
   spawn-without-identity window to the child's pre-write interval, which
   the decision table treats as ambiguous, never as absence. Descendant
   termination is by process group; a leader that exits with children
   alive still leaves the recorded group id as the retirement handle.
   Group-retirement rule (no process start time exists anywhere in the
   observable surface, so a pgid is never trusted alone): before signaling
   a recorded group, the retirer lists it through the
   `ProcessGroupInspector` port (section 3, typed outcomes) and requires at
   least one member whose
   argv matches the frozen check argv or the check-exec invocation; an
   empty group is already gone (no signal), and a non-empty group with no
   matching member is never signaled — the operation goes `reconciling`
   with the listing as evidence. The window in which a fully-exited group's
   pgid could be recycled is thereby never signaled blindly; this is a
   documented residual limitation of the cooperative model, not a solved
   race.
3. Outputs are written to `checks/<operation-uuid>/stdout`, `stderr` via
   temp-file-then-rename, with digests; `artifacts` rows tie them to the
   result. Executions never share paths, so an orphaned writer cannot
   corrupt a later execution's evidence.
4. Outcome transaction re-reads the stop request: exit 0 and no stop →
   attempt/task/run complete; exit non-zero → attempt/task/run `failed`
   with the failing command named; stop requested → evidence recorded,
   `interrupted` (stop precedence).
5. Unknown-outcome rule: if the controller dies during the check, takeover
   finds the pending operation; with a check-exec claim it retires that
   process group under the group-retirement rule above, waits for group
   absence, and marks the operation `failed` with outcome `unknown`;
   without a claim it holds the ambiguous window per the decision table
   first. Whether a new execution may run is an explicit contract:
   `check.repeatable` in the frozen snapshot (default `false`). Repeatable
   → a new execution (new operation ID, fresh checkout) runs against the
   same accepted result. Not repeatable → the unknown outcome is terminal
   for automation: the run stays actionable (status names the execution
   and its evidence and the human's options — rerun after inspection is a
   human decision, not an automatic one) and can never complete on an
   unknown outcome. Directory isolation makes reruns evidence-safe; only
   the declared contract makes them effect-safe. Completion always
   requires a settled receipt for the current result and execution.

## 8. CLI surface

Consistent with the existing `cmd/hop` contract
([cmd/hop/AGENTS.md](../../cmd/hop/AGENTS.md)): standard `flag`, thin
commands, exit 0 success / 1 failure / 2 usage, deterministic output lines,
one context deadline per command where bounded. Composition does not import
domain packages: commands pass raw strings; `internal/app` parses them into
typed IDs and returns DTO report values for rendering. `cmd/hop` owns the
single state-root resolver (section 4).

| Command | Flags | Behavior and output |
| --- | --- | --- |
| `hop run "<brief>"` | `-C dir` (repository, default cwd), `-env-passthrough VAR` (repeatable, merged into the frozen policy), `-socket` / `-herdr` (as doctor) | Foreground controller. Prints `run <seq-label> <uuid> started`, then one line per transition. Exit 0 only on `completed`; 1 on `failed`/`stopped`/detach/error; 2 on usage — including a missing or check-less `.herdr-orchestrator/config.toml`, an unsupported repository object format, and a HOP executable path failing the launch-line character rules, all refused before any side effect |
| `hop status` | `-C dir`, `-run ID`, `-all` | Without `-run`: one line per run of the repository. With `-run`: the full detail block — states, worktree path, current binding and incarnation, launch-claim state, pending/`reconciling` operations, `blocked, needs interaction` condition, last submission outcome, artifact paths, and for an unrepeatable unknown check outcome the human's options. Exit 0 when rendering succeeds; state is data, not an exit code |
| `hop stop <run-id>` | `-C dir`, `-timeout` (default 30s) | Requests stop; drives it directly when the lease is free (section 5). Reports `stopping` until termination observed, then `stopped`. Exit 0 when `stopped` was reached, 1 when the deadline left it `stopping` (rerunnable) |
| `hop resume <run-id>` | `-C dir`, `--confirm-absent` | Acquires the lease, reconciles per section 5, continues as the foreground controller. Prints what it established (warm reattach, positive-evidence retirement + cold relaunch, or the fail-closed report naming the pane, the observed occupant and the exact human action). `--confirm-absent` records the human's absence attestation; it enables cold relaunch only in the non-restart cases — after a server restart the run stays reconciling until pending restore is retired (section 5, item 5). Exit codes as `hop run` |
| `hop result submit` | section 7 | Worker-facing; `transient` first line signals retry |
| `hop launch` | `--run`, `--attempt` only (section 6) | Worker exec boundary; listed in usage under a "worker plumbing" heading |
| `hop check-exec` | `--op`, then `--` and the check argv (section 7) | Check exec boundary, spawned only by the controller; same usage heading |

SIGINT/SIGTERM on a foreground controller detach (section 5); they never
stop the run. Run IDs are accepted as full UUIDs or the repository-scoped
`r<seq>` label shown by `status`. `version`, `doctor` and `plugin-context`
are unchanged except the doctor's store-path line, computed by the shared
resolver in `cmd/hop` without opening or migrating the database
(`app.Doctor`'s report contract is untouched).

## 9. Test plan

Mapped to the layers in [engineering.md](../architecture/engineering.md) and
[plugin-testing.md](../architecture/plugin-testing.md). Every side effect
ships with its interruption/retry tests in the same change, per
[phases.md](phases.md).

| Layer | Tests |
| --- | --- |
| Spike (`test/integration`, task 0) | Complete, with executed evidence on herdr 0.9.0: S1 launch-line detection and shell readiness (`TestSpikeLaunchLineDetection`, `TestSpikeUnrecognizedProcessNameIsNotDetected`); S2 pane process identity surface (`TestSpikePaneProcessIdentity`); S3 restore behavior (`TestSpikeRestorePlainPaneLosesAdditiveEnv`, `TestSpikeRestoreAutoRelaunchBypassesLauncher`); S4 preassigned Claude session id on claude 2.1.270 (`TestSpikeClaudePreassignedSessionID`); S5 `agent.start` cannot wrap (NO); S6 layout.apply command panes (`TestSpikeLayoutApplyCommandPane`) and the workspace-preservation follow-up (`TestSpikeLayoutApplyAddsTabToExistingWorkspace`); S7 creation labels round-trip (`TestSpikeCreationMarkerTabLabel`) |
| Domain (`internal/domain/...`) | Table-driven transitions matching the section 5 tables plus complete traces (initial launch to completion; stop injected at every step; exec failure; warm takeover; cold replacement through relaunching), stop monotonicity and precedence, result acceptance context (duplicate in every state incl. terminal, conflicting never disturbing the accepted result, transient for unsettled-claim launching, stale by incarnation and by state), binding supersession evidence, ID parsing (with fuzz) |
| Application (`internal/app`) | Handwritten fakes for every port. Scenarios: intent/act/outcome ordering with revalidation before dispatch; every row and column of the operation decision table, including the takeover barrier (A resumes after B took over), never-resend, claim-state adoption (exec_pending ambiguous, execed adopt, exec_failed dead), non-adoption of cwd-matching panes, worktree provenance validation; stop while launching/running/checking, stop retiring a pre-exec claim, stop cancels the check group, stop precedence in the outcome transaction; detach vs stop; resume cases 1–5 of section 5 including delayed restore, positive-evidence retirement, same-kind replacement failing closed, and the attestation path; lease CAS (stale release, stale heartbeat, post-release commit rejection, expiry at commit); raced InitializeRun; group-retirement classification against a fake `ProcessGroupInspector` (all four typed outcomes); the section 5 reference traces as app-level scenarios; `SanitizeEnvironment` precedence tables (task 4); canonical digest vectors; submission validation order (receipt-before-state proven by a duplicate against a `completed` run; conflicting-before-eligibility proven by a changed-content retry against a `checking` attempt; early submission in all three orders relative to detection and check claiming) |
| SQLite (`internal/adapters/sqlite`) | Real temporary database, separate `*sql.DB` handles for cross-process claims: atomic attempt reservation raced from two connections; revision conflicts; lease CAS matrix raced from two connections (acquire on held/released/expired, stale heartbeat, stale release, generation monotonic across release); InitializeRun raced (repository get-or-create through the unique constraint); launch-claim different-pid rejection raced; concurrent open+migrate from two processes; connection-churn PRAGMA verification; immediate-transaction busy retry with act-after-commit ordering; reopen mid-operation and journal recovery; fixed-width timestamp round-trips; constraint coverage (one accepted result, unique (attempt, digest), unique check request, unique worktree path) |
| Herdr adapter | Fake NDJSON endpoint tests for the new `Runtime` methods (request shapes, error mapping, cancellation), same style as the existing client tests; `PaneProcess` decoding matches the S2-verified fields |
| Process/config/system adapters | `Exec` and process-group `CommandRunner` behavior (cancellation reaps the group; leader-exit-with-children retirement by group); TOML decoding tables: comments, escaped strings, multiline arrays, duplicate keys, unknown keys, invalid timeout/harness, `check.repeatable`, relative profile dir resolved absolute |
| Real process (`test/integration`) | Extends the Phase 1 harness (`prepareServer`, `stagePlugin`, `waitUntil`, artifact retention). CI-safe deterministic run: fixture repository + fixture worker driven end to end — run → worktree → launch pane (env, label, argv) → claim → corroborated settlement → sanitized exec (section 6 assertions) → assignment content validated by the fixture → result submit → detached-checkout check → completed; the send-text fallback exercised as its own scenario; duplicate submission after completion; transient early submission; stale submission from a retired incarnation; the launcher claim-protocol tests of section 6; stop with termination observed including a running check group and a pre-exec claim; controller kill + resume warm reattach; server restart + positive-evidence retirement of the auto-relaunched `--resume` occupant, the phantom-snapshot case (agent reported before any live process), and the fail-closed same-kind replacement; `--confirm-absent` cold relaunch in the non-restart case and its refusal after a server restart; detach distinct from stop; failed-check run ends `failed` with retained artifacts; check death after spawn before the claim write, leader exit with live children, claim-write failure refusing to run, and unknown outcome under both `check.repeatable` settings; git-dependent check succeeding in the detached checkout; submodule repository failing clearly |
| Live (opt-in) | One named test, `TestLiveClaudeDefaultProfileRun`, gated on `HOP_LIVE_HARNESS=1` and skipped otherwise with an explicit reason: real Claude Code in the user's default profile against the fixture repository, one small brief to a passing check, plus one `--resume <preassigned-uuid>` continuation. Never runs in CI or `make check` |

Fixture repository generator (test helper in `test/integration`): a
temporary git repository with an initial commit, a trivial source file,
committed `check.sh` pass/fail variants and a
`.herdr-orchestrator/config.toml` per the section 3 example. Fixture worker:
a test-built executable substituted as the snapshot's harness; it dumps its
environment, validates the assignment artifact against expected content
(multiline, quotes), optionally reports a custom agent identity as the
Phase 1 presentation fixture does, waits for a scripted trigger, commits a
change and invokes the real `hop result submit` (retrying on `transient`),
then idles until closed. Everything is generated per test; no fixture data
on disk.

## 10. Work breakdown

Ordered implementer tasks. Each lands with its package AGENTS.md guides and
tests, and each branch is self-contained: it adds its own packages'
`internal/arch_test.go` rows and guide links, and any `go.mod` dependency
lands with its first importer (never a bare pin — `make check` runs
`go mod tidy -diff`, so an importless requirement cannot be a green
boundary). `make check` and `make docs-check` therefore pass at every task
boundary as landed. Integrator role, assigned before task 1: the task 6a
implementer owns conflict resolution in `internal/arch_test.go` and
[internal/adapters/AGENTS.md](../../internal/adapters/AGENTS.md) when
branches combine, and lands branches one at a time. The Makefile is edited
only in task 6b; `cmd/hop` only in task 6a; the state-root resolver is
owned by `cmd/hop` (6a) alone and the SQLite adapter receives an absolute
path.

| # | Task | Packages / files | Tests | Depends on | Parallel | Tier |
| --- | --- | --- | --- | --- | --- | --- |
| 0 | Spike: the seven bounded real-process probes (S1–S7); findings reported before task 2 freezes ports | `test/integration` (probe tests + retained evidence) | the probes themselves | — | with 1, 4a | Sonnet |
| 1 | Domain slice: identity types, run-module entities, pure transitions per the section 5 tables with complete-trace tests, typed errors; `internal/domain` index guide and its own checker rows | `internal/domain/identity`, `internal/domain/run` | Domain tables, trace tests, parser fuzz | — | with 0, 4a | Sonnet |
| 4a | Credential-handling core: `internal/app/launchenv.go` — the versioned `EnvPolicy` value, the version-scoped strip matrix, `SanitizeEnvironment` and its precedence tables. Self-contained file with its own tests; no dependency on task 2 (its consumers are the exec-boundary commands in 6a) | `internal/app/launchenv.go` | Precedence and matrix tables | — | with 0, 1 | Fable |
| 2 | Application ports and controller logic: all section 3 ports (shapes finalized against S1/S2/S7 findings), canonical digest, run/status/stop/resume/submit/check use cases against handwritten fakes, the operation decision table as behavior, DTOs for composition. Does not consume `SanitizeEnvironment` | `internal/app` | App scenario + table tests incl. the takeover barrier | 0, 1 | — | Sonnet |
| 3 | SQLite store: schema migration 001, typed repositories, unit of work, revisions, journal, lease CAS + InitializeRun, claim contracts, ReadStore; adds the pinned `modernc.org/sqlite` requirement as its first importer; receives the state root as an absolute path (no resolver here) | `internal/adapters/sqlite`, `go.mod` | Real temp-DB suite of section 9 | 2 | with 4b, 5 | Fable |
| 4b | Local adapters: `Exec`, process-group `CommandRunner` and the `ProcessGroupInspector` implementation (`internal/adapters/process`), `Clock`/`IDGenerator` (`internal/adapters/system`), strict TOML policy loading (`internal/adapters/config`, adding the pinned `github.com/pelletier/go-toml/v2` requirement as its first importer) | `internal/adapters/process`, `internal/adapters/system`, `internal/adapters/config`, `go.mod` | Section 9 adapter rows | 2 | with 3, 5 | Fable |
| 5 | Herdr runtime extension: `Runtime` over `Client` with the S2-verified `PaneProcess` fields, `FindPaneByLabel`, close-rule support and the S7 creation-marker mechanics | `internal/adapters/herdr` (`runtime.go`) | Fake-endpoint protocol tests | 2 | with 3, 4b | Sonnet |
| 6a | Command wiring (integrator): `hop run`/`status`/`stop`/`resume`/`result submit`/`launch`/`check-exec` in `cmd/hop`, launch-line rendering and path rules, the state-root resolver, doctor store-path line, merges of 3/4b/5 | `cmd/hop` | Command tables: dispatch, exit codes, flag surfaces, signal handling | 3, 4a, 4b, 5 | — | Sonnet |
| 6b | Scenario integration: fixture repository generator, fixture worker, the full real-process suite of section 9, the opt-in live test, Makefile target for it | `test/integration`, `Makefile` | Section 9 real-process rows | 6a | — | Sonnet |

`go.mod` is touched by tasks 3 and 4b, each adding only its own requirement
with its importer; the integrator serializes their landing. Fable implements
the three correctness-critical cores: the credential-handling environment
policy (4a), the SQLite atomicity/recovery/fencing store (3) and the
process/exec boundary (4b).

## 11. Decisions assumed, spike dependencies, open questions, risks

Assumed decisions (directed by the orchestrator, pending human override):

1. Store root is always `${XDG_STATE_HOME:-$HOME/.local/state}/hop` with
   `HOP_STATE_DIR` as the only override; worker-context commands require
   the provided absolute root and never fall back (section 4).
2. No second attempt in Phase 2. Retrying an acknowledged submission, the
   `transient` retry, and controller recovery are in scope now; a
   `--retry` command is not (section 7).
3. Cold resume uses a HOP-preassigned Claude session UUID
   (`--session-id` at launch, `--resume` on cold relaunch); Codex/opencode
   capture is unspecified in Phase 2 and Claude argv is never rendered for
   another harness (sections 5–6).
4. Occupant retirement requires positive evidence or human attestation;
   `check.repeatable` defaults to false (sections 5, 7).

Spike results (task 0; complete, folded into this revision, on herdr 0.9.0
with executed evidence):

| Item | Status | Design consequence |
| --- | --- | --- |
| S1 launch-line detection, shell readiness, fork topology | Established (YES) | Send-text fallback verified under sh and zsh login shells; detection is process-name identification of known harness names; after `exec` the harness pid = shell pid = foreground group (section 6) |
| S2 pane process identity fields, command-line observability | Established (YES, with caveats) | `PaneProcess` fields frozen (shell pid, foreground group, per-process pid/name/argv0/argv/cmdline/cwd); NO process start time exists, so occupant identity is label + argv marker + pid, never pid alone; no guarded close upstream — inspect-then-close is a documented race (sections 3–5) |
| S3 restore behavior and settled signal, restored argv shape | Established (YES) | Restore restores the pane label but not the additive env; a recorded native session auto-relaunches exactly as `claude --resume <id>`, deferred until a client supplies geometry, bypassing `hop launch` and dropping `HOP_*` (env dump verified) — so positive-evidence retirement is verified end to end; a post-restart snapshot can show a phantom agent before any process runs, so absence is established only by live process inspection; no "restore finished" signal and no cancellation route exist — the post-restart `--confirm-absent` limitation and reconciling-plus-report stance stand as established behavior (section 5, items 2–5) |
| S4 `claude --session-id`/`--resume` with preassigned UUID | Established (YES, claude 2.1.270) | Print mode in a scratch profile with a credential-free env: `--session-id <uuid>` accepted, transcript created under the pre-assigned UUID, `--resume <uuid>` finds it from a different cwd, unknown UUID rejected; version-scoped, re-verify on drift (sections 5–6) |
| S5 `agent.start` argv capability | Established (NO) | agent.start cannot interpose the sanitizer and types bare names (PATH-unsafe under macOS path_helper); absolute executable paths everywhere (section 6) |
| S6 pane creation with command argv, plus the workspace-preservation follow-up | Established (YES) | layout.apply command panes are the primary transport — no shell, no rc, no PATH ambiguity; addressed by `workspace_id` it only adds one tab, leaving every pre-existing tab and pane untouched; the pane closes identically on success and failure and `pane.exited` carries no exit status, so worker success comes only from the result protocol and check (sections 5–6) |
| S7 creation-time markers | Established (YES) | Creation labels round-trip through responses, tab.list and session.snapshot; the pane-open decision-table row recovers by unique label (section 4) |

Open questions for the human (beyond confirming the assumed decisions):
none new; everything else is spike-resolvable or decided above.

Risks and mitigations:

| Risk | Mitigation |
| --- | --- |
| A future herdr version changes layout.apply's add-a-tab semantics | The workspace-preservation behavior is spike-established against herdr 0.9.0 only; the S1-verified send-text fallback carries the identical launcher contract, and the claim-or-reconciling rule means a lost launch never causes an automatic resend or a duplicate worker on either transport |
| The fixture worker's unrecognized process name is never agent-detected | Established behavior (S1), designed for: claim settlement has the process-observation path (argv marker), and detection is only ever corroboration, never the authority |
| Herdr auto-restore races resume | Restored occupants are never adopted; retirement needs positive evidence or attestation; without either, resume fails closed with the exact human action (section 5) |
| A stale controller acts on an already-committed intent after takeover | The operation decision table plus pre-dispatch revalidation, claim-based process identity and the takeover barrier test (section 4); fencing alone is explicitly not treated as external fencing |
| `modernc.org/sqlite` behavior differences (locking, WAL, pool PRAGMAs) | Exact version pin; DSN-applied per-connection settings with churn tests; immediate write transactions with bounded whole-transaction retry and act-after-confirmed-commit ordering; concurrent open/migrate tests |
| Same-UID worker interference with store, artifacts, incarnation env or gate content | Out of scope by the stated trust model; the design freezes policy selection, isolates check execution per checkout/execution ID, and claims nothing stronger |
| Claude version drift invalidates `--session-id`/strip-matrix assumptions | The `--session-id` contract is established against claude 2.1.270 and re-verified on drift; the strip matrix is version-scoped and named; the live test exercises the real binary |
| Unsupported login shells mangle the fallback launch line | Primary transport involves no shell at all; the fallback grammar is restricted to a single-quoted path plus UUID flags, valid in sh/bash/zsh (S1-verified for sh and zsh); other shells documented as unsupported for worker panes in Phase 2 |
| pid or pgid reuse defeats process identity (no start time observable) | Occupant identity always pairs the pid with the creation label and argv marker; group retirement lists and argv-matches members before signaling and never signals blindly; documented residual limitation |
| A run with no restore occupant stays in recovery (no settled signal or cancellation route exists — S3-established) | Finite exits exist by design: `hop stop` always, and `hop resume --confirm-absent` in the non-restart cases; the post-server-restart case deliberately stays reconciling until pending restore is retired (a deferred Herdr restore firing after a truthful absence attestation would otherwise duplicate the native lineage) — documented as a support limitation, not silent relaunch |
