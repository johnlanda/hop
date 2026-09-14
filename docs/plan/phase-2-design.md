# Phase 2 design: durable single-worker run

Status: design for [phases.md](phases.md) Phase 2. Nothing in this document is
implemented; it fixes the decisions an implementer executes without re-deriving
them. It applies the settled account decisions: accounts are delegated to the
harnesses (workers run in each harness's default profile), HOP performs no
logins and stores no credentials, and a sanitizing launcher at the harness exec
boundary strips provider credential variables by default. HOP runs on the
user's normal Herdr server with no configuration change. Persistence is SQLite
via `modernc.org/sqlite` (pure Go); the CLI stays on the standard `flag`
package. The first worker is Claude Code in the default profile against a
fixture repository created by the tests.

## 1. Scope and exit criteria

Phase 2 delivers four commands — `hop run "<brief>"`, `hop status`, `hop stop`
and a scoped `hop resume` — plus two supporting commands the workflow needs
(`hop result submit`, invoked by the worker, and `hop launch`, the sanitizing
exec-boundary launcher). One brief becomes one run with frozen effective
instructions, one task, one attempt and one native worker launched in a
dedicated worktree. The controller is the foreground `hop run` process.

Flow: `hop run` loads repository policy from `.herdr-orchestrator/`, freezes
the effective instructions (brief, check command, environment policy, result
protocol text, generated identities), persists the run/task/attempt/session
rows and a launch intent, creates a worktree through Herdr, opens a worker
pane with an explicit environment, execs the harness through the sanitizing
launcher, and then waits. The worker submits an explicit result tagged with
run/task/attempt IDs; Herdr idle/done never completes a task (see
[RESEARCH.md](../../RESEARCH.md), "Important verified limits"). The controller
validates the result, runs the configured deterministic check against the
identified commit, and only a passing check completes the task and run.
Artifacts, state transitions and interrupted-operation evidence are persisted
throughout.

Applied delegation decisions:

- Workers run in the harness's default profile. HOP never logs in, never
  reads or writes `~/.claude`, `~/.codex` or opencode state, and stores no
  credential or account entity.
- An optional configuration value may name a user-prepared alternate profile
  directory; HOP only passes it through (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`,
  or opencode's `HOME` plus all four `XDG_*_HOME` variables). No pools,
  leases on accounts, or cross-account resume; those stay in Phase 5.
- The launcher strips provider credential variables by default with explicit
  opt-in passthrough per run or repository (section 6).

Exit criteria (restated from [phases.md](phases.md) with how each is proved):

| Criterion | Evidence |
| --- | --- |
| Repeatable brief → worker → result → check → completion | Real-process test with the deterministic fixture worker (section 9); opt-in live test with Claude Code |
| Duplicate result submission is idempotent | Domain table test plus a real submission repeated against the same store |
| Stale results fail | Submission against a non-active attempt or stopped run is rejected and recorded |
| Crash before/after launch does not silently duplicate workers | Interruption tests kill the controller between intent and act, and between act and outcome; resume reconciles instead of relaunching blindly |
| A failed check blocks completion | Fixture repository configured with a failing check; run ends `failed` with retained evidence, never `completed` |
| Board closure / client detach is distinct from stop | No board exists yet; the integration test detaches the observing client and closes HOP-opened non-worker panes, then proves the controller and worker keep running |

Out of scope for Phase 2: manager sessions, messages, multiple tasks or
attempts-after-failure, playbooks, gates beyond the single required check,
account pools, the board, and detached (non-foreground) controllers.

## 2. Domain slice

New packages: `internal/domain/identity` and `internal/domain/run`. No
Message, Playbook or Account entity is created. Domain code receives time and
identities as explicit inputs; it never reads a clock or generates an ID
(enforced by the checker's forbidden-selector rules).

### internal/domain/identity

Strongly typed IDs shared across boundaries, with pure parsing:

- `RepositoryID`, `RunID`, `TaskID`, `AttemptID`, `SessionID`, `WorktreeID`,
  `ResultID`, `ArtifactID`, `OperationID` — each a distinct string type
  wrapping a UUID, with `Parse<Kind>ID(string) (<Kind>ID, error)` validating
  canonical UUID form and a `String()` method. IDs are generated only through
  the application's `IDGenerator` port.

### internal/domain/run

Entities, their invariants and pure transitions (states in section 5):

| Entity | Owned state | Invariants |
| --- | --- | --- |
| `Run` | Repository ID, sequence number, brief digest, state, stop request | One task in this phase; completion requires the task completed; a stop request is monotonic (cannot be withdrawn) |
| `Task` | Run ID, instructions digest, state | At most one active attempt; a task completes only through an accepted result plus a passing check |
| `Attempt` | Task ID, number, session ID, state | Numbers are dense from 1; retry is a new attempt (not exercised in Phase 2 beyond the invariant); a non-active attempt accepts no result |
| `Session` | Run ID, role, harness, state | One session executes at most one attempt; Phase 2 has exactly one worker session and no manager |
| `RuntimeBinding` | Session ID, workspace/tab/pane IDs, agent name, optional native session reference, observed-at, superseded flag | Observations from a superseded binding cannot settle the current session; bindings are append-only history |
| `Worktree` | Repository ID, run ID, path, branch, state | Created before launch; preserved by stop and by failure |
| `Result` | Attempt ID, commit SHA, summary, content digest, accepted flag | At most one accepted result per attempt; equal digest resubmission is idempotent; a different digest for the same attempt is a conflict |
| `Artifact` | Run ID, optional result ID, kind, path, digest | References files under the run's artifact directory, never blobs in domain state |

Pure transition functions take the current value, the inputs that justify the
transition and an explicit `now time.Time`, and return the next value or a
typed error (`ErrInvalidTransition`, `ErrStaleResult`, `ErrConflictingResult`,
`ErrDuplicateResult` distinguishing the idempotent case). Result acceptance is
a pure function of (attempt state, existing result, submitted digest).

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
| `internal/adapters/config` | `driven-adapter` | `internal/app` | none |
| `internal/adapters/herdr` (existing) | `driven-adapter` | unchanged (`internal/app`) | none |
| `cmd/hop` (existing) | `composition` | gains the four new adapter packages | none |
| `test/integration` (existing) | `integration-test` | gains the packages its scenarios drive | unchanged |

The pure-standard allowlists for the two domain packages name only pure
packages (`errors`, `fmt`, `strings`, `time`, `sort`, `crypto/sha256`,
`encoding/hex` as needed); the checker already rejects effectful standard
packages and ambient clock/randomness there. The grouping directory
`internal/domain` gains an index AGENTS.md, and each new package gains a leaf
guide, in the change that creates it.

## 3. Ports (consumer-owned, in internal/app)

Reused Phase 1 symbols, unchanged: `app.Probe` (doctor), `app.Invocation`,
`app.AgentPresentation` / `app.Presenter` / `app.AgentDisplay` /
`app.SortDisplays` (the run controller publishes the worker pane's
`hop_run`/`hop_role`/`hop_task`/`hop_state` tokens), `app.Observer` /
`app.StatusStream` / `app.Reconcile` / `app.ReconcileState` (stop and resume
reconciliation), and in the adapter `herdr.Client`, `herdr.InstallationProbe`,
`herdr.Presentation`, `herdr.Observer`. The intentional-deferral note in
[internal/app/AGENTS.md](../../internal/app/AGENTS.md) is discharged: the
worker-launch use case is the first consumer of `Runtime`, `Clock` and
`IDGenerator`.

New ports declared in `internal/app`:

### StateStore / UnitOfWork

```go
type StateStore interface {
    // Begin opens a unit of work. A controller-scoped store binds every unit
    // to its run lease generation; Commit fails with ErrFenced when the
    // lease has moved on.
    Begin(ctx context.Context) (UnitOfWork, error)
}

type UnitOfWork interface {
    Runs() RunRepository            // and Tasks(), Attempts(), Sessions(),
                                    // Worktrees(), Results(), Artifacts(),
                                    // Bindings(), Operations(), Transitions()
    Commit() error
    Rollback() error
}
```

Typed repositories return domain values, never rows. Every mutable entity
carries a revision; `Save(entity, expectedRevision)` returns
`ErrRevisionConflict` when the row moved. `OperationRepository` records
intent/outcome journal entries (section 4). The store implements atomic
multi-entity writes as one transaction per unit of work.

### Runtime (extends the Phase 1 Herdr adapter)

```go
type Runtime interface {
    CreateWorktree(ctx context.Context, req WorktreeRequest) (WorktreeInfo, error) // worktree.create; no env — see launch-environment.md
    OpenWorkerPane(ctx context.Context, req WorkerPaneRequest) (PaneHandle, error) // tab.create inside the worktree with an explicit env map
    SendText(ctx context.Context, paneID, text string) error                        // pane.send_text: launcher command line, prompts
    ReadPane(ctx context.Context, paneID string, lines int) (string, error)         // evidence snapshots
    InspectPane(ctx context.Context, paneID string) (PaneProcess, error)            // foreground process identity for launch/stop reconciliation
    ClosePane(ctx context.Context, paneID string) error                             // stop interruption
}
```

Implemented by a new `runtime.go` in `internal/adapters/herdr` over the
existing `Client`. `WorkerPaneRequest.Env` is the additive map from
[launch-environment.md](../architecture/launch-environment.md): it carries
only additions (`HOP_RUN_ID`, `HOP_TASK_ID`, `HOP_ATTEMPT_ID`,
`HOP_STATE_DIR`); all removal happens in the launcher argv. `PaneHandle`
returns the workspace, tab and pane IDs for the runtime binding.

### Clock and IDGenerator

```go
type Clock interface{ Now() time.Time }
type IDGenerator interface{ NewID() string } // canonical UUID; parsed into typed IDs by the caller
```

Implemented in `internal/adapters/system` (`crypto/rand` UUIDv4,
`time.Now().UTC()`). Tests use handwritten fakes with fixed sequences.

### CommandRunner

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
    Run(ctx context.Context, cmd Command) (CommandResult, error) // ctx cancellation kills the process group
}
```

Implemented in `internal/adapters/process`; used for the deterministic check
and the git verifications (`git rev-parse HEAD`, `git status --porcelain`).
The same package provides `Exec(argv, env)` (a `syscall.Exec` wrapper) used
only by the `hop launch` command.

### ConfigurationSource

```go
type ConfigurationSource interface {
    Load(ctx context.Context, repositoryRoot string) (RunPolicy, error)
}
```

Implemented in `internal/adapters/config`: reads
`.herdr-orchestrator/config.toml` at the repository root and returns an
application value:

```go
type RunPolicy struct {
    CheckArgv       []string          // required; Load fails without it
    CheckTimeout    time.Duration     // default 10m
    EnvPassthrough  []string          // opt-in variables the launcher keeps
    ProfileDir      string            // optional alternate profile directory, passed through only
    Harness         string            // default "claude"
}
```

Decoding uses the standard library (a minimal TOML subset is not acceptable;
use deliberate line-based parsing of the small fixed schema, documented in
the adapter guide, rather than adding a TOML dependency — the file has only
string, string-array and duration keys). Unknown keys fail validation.

### Launcher contract

The launcher is not a port: it is the `hop launch` command (section 6) plus a
pure application function `SanitizeEnvironment(environ []string, policy
EnvPolicy) (env []string, removed []string)` in `internal/app` that both the
command and its tests share. `EnvPolicy` is frozen into the run snapshot.

## 4. Persistence

### Store location and layout

One user-scoped SQLite store shared by all repositories and runs, per
[architecture.md](../architecture/architecture.md). Resolution of the state
root, identical in every command: `HOP_STATE_DIR` if set (tests and explicit
override), else `HERDR_PLUGIN_STATE_DIR` if set (plugin-command invocations,
see [internal/app/AGENTS.md](../../internal/app/AGENTS.md) invocation fields),
else `${XDG_STATE_HOME:-$HOME/.local/state}/hop`. Phase 2 commands are typed
in ordinary shells, so the XDG default is the operative path; the doctor
gains a line reporting the resolved store path, and section 11 carries the
plugin/CLI divergence question. Layout:

```text
<state root>/
  hop.db                      one database for all repositories and runs
  runs/<run-uuid>/artifacts/  check output, pane snapshots, stop evidence
```

Runs partition by `repository_id` and `run_id` columns, not by database file,
so later phases can share cross-run state (account pools) in one store.
Repository identity is the symlink-resolved absolute path of the repository
root, held in `repositories` with a generated `RepositoryID`.

### Schema (initial migration)

All IDs are UUID TEXT; times are RFC 3339 UTC TEXT; every mutable table has
`revision INTEGER NOT NULL` (optimistic concurrency: updates run
`... WHERE id = ? AND revision = ?` and zero affected rows is
`ErrRevisionConflict`). Foreign keys are enforced.

| Table | Columns (abridged) | Constraints |
| --- | --- | --- |
| `schema_migrations` | version PK, applied_at | forward-only |
| `repositories` | id PK, root_path, created_at | UNIQUE(root_path) |
| `runs` | id PK, repository_id FK, seq, brief, state, stop_requested_at NULL, revision, created_at, updated_at | UNIQUE(repository_id, seq) |
| `run_snapshots` | run_id PK FK, instructions, check_argv JSON, check_timeout_ms, env_policy JSON, harness, profile_dir, created_at | immutable after insert |
| `tasks` | id PK, run_id FK, state, revision, … | one row per run in this phase |
| `attempts` | id PK, task_id FK, number, session_id FK NULL, state, revision, … | UNIQUE(task_id, number); partial UNIQUE(task_id) WHERE state in active set |
| `sessions` | id PK, run_id FK, role, harness, state, revision, … | |
| `runtime_bindings` | id PK, session_id FK, workspace_id, tab_id, pane_id, agent_name, native_session_ref NULL, observed_at, superseded | append-only |
| `worktrees` | id PK, repository_id FK, run_id FK, path, branch, state, created_at | UNIQUE(path) |
| `results` | id PK, attempt_id FK, commit_sha, summary, content_digest, accepted, submitted_at | partial UNIQUE(attempt_id) WHERE accepted = 1 |
| `result_submissions` | id PK, run_id, task_id, attempt_id, content_digest, outcome (accepted, duplicate, stale, conflicting), submitted_at | evidence of every submission, including rejected ones |
| `artifacts` | id PK, run_id FK, result_id FK NULL, kind, path, digest, created_at | |
| `transitions` | id PK, entity_kind, entity_id, from_state, to_state, reason, generation, at | append-only evidence |
| `operations` | id PK, run_id FK, generation, kind, state (pending, succeeded, failed, reconciling), intent JSON, outcome JSON NULL, created_at, updated_at | the operation journal |
| `run_leases` | run_id PK FK, controller_id, generation, acquired_at, heartbeat_at, expires_at | fencing |

Connection settings: `journal_mode=WAL`, `synchronous=FULL`,
`foreign_keys=ON`, `busy_timeout=5000`. One `*sql.DB` per process; SQLite's
own locking plus the lease serialize cross-process writers.

### Migrations

Ordered, embedded SQL files (`internal/adapters/sqlite/migrations/NNN_*.sql`
via `embed`), each applied in its own transaction and recorded in
`schema_migrations`. Opening the store applies pending migrations; a store
newer than the binary's latest known migration is an error, not a guess.
Forward-only; no down migrations.

### Single controller per run: lease and fencing

`hop run` and `hop resume` acquire the run's lease: insert or take over the
`run_leases` row, incrementing `generation`, only when the row is absent,
expired, or explicitly released. The controller heartbeats `heartbeat_at`
and `expires_at` (lease TTL 30s, heartbeat every 10s). Every controller unit
of work re-reads the lease row inside its transaction and aborts with
`ErrFenced` unless the generation still matches the one the controller
acquired — a stale controller can therefore never commit new state. Lease
expiry alone is not proof the previous controller's worker exited; takeover
reconciles observed runtime identity before acting (section 5).
Operation-journal rows record the writing generation.

### Transaction rule

External calls never happen inside a database transaction. Every side effect
follows record-intent, act, record-outcome:

1. In one transaction: apply the domain transition, write the `operations`
   row as `pending` with the full intent payload, commit.
2. Perform the external call (Herdr request, process exec, check command).
3. In a second transaction: write the observed outcome, mark the operation
   `succeeded` or `failed`, apply the consequent transition, commit.

A crash between 1 and 3 leaves a `pending` operation; recovery inspects
runtime state (pane process info, snapshots, worktree existence) and either
completes the record or marks the operation `reconciling`. An ambiguous
launch stays `reconciling` and is surfaced by `hop status`; it is never
blindly repeated — Herdr does not promise idempotent launch by operation ID
([architecture.md](../architecture/architecture.md)).

## 5. Run state machine

States and transitions (all transitions are pure domain functions; the
controller drives them through the operation journal):

### Run

`created → launching → running → completing → completed`, with
`running|launching → stopping → stopped`, `any-active → failed`, and
`resuming` entered by `hop resume` before it settles into `running`,
`stopping` or `failed`. `stop_requested_at` may be set in any active state;
the controller halts new work immediately and moves to `stopping`.

### Task

`pending → active → checking → completed`, plus `active|checking → failed`
and `active → interrupted` (stop or worker loss). Phase 2 has no retry: a
failed check leaves the task `failed` with evidence and the worktree
preserved.

### Attempt

`reserved → launching → running → submitted → checking → completed`, plus
`launching → reconciling` (ambiguous launch), `running → interrupted` (stop,
pane exit, controller crash takeover), `checking|submitted → failed`
(rejected result or failed check), and `reconciling → running|interrupted`
once runtime identity is established.

### Session

`reserved → launching → active → terminated`, with `active → stopping`
(interrupt sent, termination not yet observed), `launching|active →
reconciling`, and `reconciling → active|lost`. A session's settling evidence
must come from its current runtime binding; an observation from a superseded
pane occupant is ignored (binding invariant, section 2).

### Stop

`hop stop <run-id>` sets `stop_requested_at` in the store. The live
controller (or `hop stop` itself when the lease is expired, after taking the
lease with a new generation) then: records the interrupt intent; halts any
not-yet-performed work (a `reserved` attempt is marked `interrupted` without
launching); interrupts owned active work by closing the worker pane through
`Runtime.ClosePane`; reports the run `stopping` until termination is
observed (the pane's exit event via the Phase 1 `Observer`, or its absence
from a snapshot, or `InspectPane` reporting no process); then records
`stopped`. Worktrees and artifacts are preserved. A stop that cannot observe
termination within its deadline leaves the run `stopping` with a
`reconciling` operation and says so; it does not claim `stopped`.

### Resume

`hop resume <run-id>` acquires the lease (new generation), replays recovery:

1. Reconcile observations first: construct the `Observer` for the recorded
   pane set, `app.Reconcile` (subscribe before snapshot, Phase 1 semantics),
   and match live panes against the session's current runtime binding by
   pane ID and occupant.
2. Surviving worker (warm reattach — supported): rebind, republish
   presentation tokens (metadata is not cold-restored, see
   [herdr-surface.md](../architecture/herdr-surface.md)), continue waiting.
3. Herdr auto-restore already relaunched the worker: acceptable with default
   profiles (no per-pane profile overrides are lost, per
   [native-harness-compat.md](../architecture/native-harness-compat.md) —
   the restored pane only misses HOP's additive `HOP_*` variables, which
   resume re-establishes for result submission by re-sending the launcher
   line only if the harness is absent; an auto-restored live harness is
   treated as the warm case with a new binding).
4. Absent worker: relaunch only when absence is established (no pane, or
   pane process info shows no harness) and supported resume semantics exist
   — HOP-driven cold resume in the default profile requires a recorded
   `native_session_ref` for the session; the relaunch execs
   `hop launch … -- claude --resume <ref>` in a fresh pane in the same
   worktree. Without a recorded reference the attempt is `interrupted` and
   `hop resume` reports an actionable unsupported state rather than starting
   a fresh conversation silently.
5. Pending/ambiguous operations from the previous generation are completed
   or marked `reconciling` per the transaction rule.

## 6. Sanitizing launcher

### Placement and mechanism

The launcher must be the argv that finally execs the harness:
[launch-environment.md](../architecture/launch-environment.md) verified that
Herdr's env maps are additive and cannot unset an inherited variable, that
`worktree.create` and `agent.start` carry no env at all, and that panes
default to login shells whose startup files can re-export provider keys after
any upstream sanitation
([native-harness-compat.md](../architecture/native-harness-compat.md)).
Therefore:

- `hop launch` is a worker-facing `hop` subcommand. It computes the sanitized
  environment from its own inherited environment and the policy flags, then
  replaces itself with the harness via `Exec` (execve). No shell remains
  between the sanitized environment and the harness.
- The worker pane is created with `OpenWorkerPane` (additive env: the
  `HOP_*` identifiers), and the controller starts the harness by sending one
  line to the pane shell: `exec <abs-hop-path> launch <flags> -- <harness
  argv>` via `SendText`, after `InspectPane` confirms the shell is the
  foreground process. Herdr's own agent detection recognizes the harness
  process exactly as it does when a user types `claude` in a shell;
  `agent.start` is not used for launch because it composes its own harness
  argv and cannot interpose the sanitizer. The Phase 1 real-process suite
  already proves text injection (`TestRealProcessContextInjection`).

### Contract

```text
hop launch [--passthrough VAR]... [--profile-dir DIR] [--harness NAME] -- <argv...>
```

- Strips, by default, the provider credential variables:
  `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`,
  `CLAUDE_SECURESTORAGE_CONFIG_DIR`, `OPENAI_API_KEY`, `OPENAI_BASE_URL`,
  plus the profile-selection variables `CLAUDE_CONFIG_DIR`, `CODEX_HOME`
  (default-profile decision: a stray inherited override must not silently
  select a non-default profile). The list is a named constant per harness
  family with one shared base; repository policy may extend it
  (`env.strip`) and opt variables back in (`env.passthrough`, or the
  per-run `--env-passthrough` flag, which the controller renders into
  `--passthrough` flags on the launch line).
- `--profile-dir DIR` (only when the repository policy names one) sets the
  harness's profile variable(s) — `CLAUDE_CONFIG_DIR` for Claude,
  `CODEX_HOME` for Codex, `HOME` plus all four `XDG_*_HOME` for opencode —
  to the user-prepared directory. HOP passes the value through unmodified
  and never prepares, inspects or writes the directory.
- Keeps everything else, including Herdr's authoritative `HERDR_*` variables
  and the additive `HOP_*` identifiers from pane creation.
- Execs `<argv...>` with the resulting environment; a failed exec prints one
  diagnostic line to stderr and exits 2 (usage) or 1 (exec failure),
  consistent with `cmd/hop` exit codes.

The env computation is the pure `app.SanitizeEnvironment` (section 3), so
the same logic is table-tested without processes.

### Exec-boundary tests

The integration suite builds a fixture executable that writes its complete
sorted environment to a file named by an argument, then execs it through the
real pane path (`OpenWorkerPane` → `SendText` of the launch line). Asserted:
the stripped variables are absent even when deliberately injected into the
Herdr server's environment (the harness's own environment seeds them, as the
Phase 1 harness builds server environments from scratch); a `--passthrough`
variable survives; `--profile-dir` sets exactly the harness's profile
variables; `HERDR_*` and `HOP_*` are present; a login-shell re-export of a
stripped variable (planted via the pane shell's rc file in the test's
scratch HOME) is still removed. A process-only variant (no Herdr) runs
`hop launch` directly for fast coverage of the flag surface.

## 7. Result protocol and deterministic check

### Protocol choice: CLI subcommand

Workers submit results by running `hop result submit`. A file-based protocol
was rejected: it would need a watcher plus atomic-rename conventions inside
the worktree, validation would happen after the fact, and idempotency and
stale rejection would be split between writer and reader. The subcommand
validates against the live store at submission time, gives the worker an
immediate, parseable verdict, and reuses the store's transactional
guarantees. The worker terminal is a full shell, so invoking the absolute
`hop` path is no extra capability.

### Submission

```text
hop result submit --summary "<text>" --commit <sha> [--run ID --task ID --attempt ID]
```

The ID flags default from `HOP_RUN_ID`, `HOP_TASK_ID` and `HOP_ATTEMPT_ID`,
which the launcher's environment carries into the harness and every subshell
it spawns; the frozen instructions tell the worker to commit its work in the
worktree and run exactly this command. Validation, in one transaction:

- IDs parse, exist and agree (attempt belongs to task, task to run).
- The attempt is the task's active attempt and in state `running` or
  `submitted`; the run is not stopped/stopping. Otherwise: recorded in
  `result_submissions` as `stale`, exit 1 with the reason.
- Content digest = SHA-256 over (run, task, attempt, commit, summary).
  Same attempt + same digest → `duplicate`: idempotent success (exit 0,
  prints the existing result ID). Same attempt + different digest →
  `conflicting`: recorded, exit 1.
- Accepted: result row inserted (accepted, partial unique index enforces at
  most one), attempt → `submitted`, a `check.run` pending operation is
  journaled, exit 0.

The controller consumes durable pending operations by polling the store
(2s interval) while also watching pane events; a notification is never the
only trigger ([architecture.md](../architecture/architecture.md)).

### Deterministic check

On `check.run`, the controller (record intent → act → record outcome):

1. Verifies the worktree at the submitted commit: `git rev-parse HEAD`
   equals the result's SHA and `git status --porcelain` is empty, via
   `CommandRunner`. Mismatch → result rejected, attempt `failed`, evidence
   recorded.
2. Runs the frozen `check_argv` from the run snapshot (never re-read from
   the working tree, so a worker cannot weaken the gate mid-run) with
   `Dir` = worktree, a sanitized environment, and the frozen timeout.
3. Persists exit code, duration, and stdout/stderr as artifacts under
   `runs/<run-uuid>/artifacts/`, plus `artifacts` rows tied to the result.
4. Exit 0 → attempt/task/run complete. Non-zero → attempt `failed`, task
   `failed`, run `failed`; completion is blocked and the evidence names the
   failing command.

## 8. CLI surface

Consistent with the existing `cmd/hop` contract
([cmd/hop/AGENTS.md](../../cmd/hop/AGENTS.md)): standard `flag`, thin
commands, exit 0 success / 1 failure / 2 usage, deterministic output lines,
one context deadline per command where bounded.

| Command | Flags | Behavior and output |
| --- | --- | --- |
| `hop run "<brief>"` | `-C dir` (repository, default cwd), `-env-passthrough VAR` (repeatable), `-socket` / `-herdr` (as doctor) | Foreground controller. Prints `run <seq-label> <uuid> started`, then one line per state transition (`run r3 launching`, `worker pane w2:p4`, `result accepted <id>`, `check passed`, `run r3 completed`). Exit 0 only on `completed`; 1 on `failed`/`stopped`/error; 2 on usage, including a missing or check-less `.herdr-orchestrator/config.toml` (refused before any side effect) |
| `hop status` | `-C dir`, `-run ID`, `-all` | Without `-run`: one line per run of the repository (`r3  running  task=active attempt=1 worker=w2:p4  updated=<ts>`). With `-run`: the full detail block — states, worktree path, binding, pending/reconciling operations, last submission outcome, artifact paths. Exit 0 when rendering succeeds; state is data, not an exit code |
| `hop stop <run-id>` | `-C dir`, `-timeout` (default 30s) | Requests stop; drives it directly when the lease is free (section 5). Reports `stopping` lines until termination is observed, then `stopped`. Exit 0 when `stopped` was reached, 1 when the deadline left it `stopping` |
| `hop resume <run-id>` | `-C dir` | Acquires the lease, reconciles, continues as the foreground controller. Prints what it established (`reattached worker pane w2:p4` / `worker absent, cold resume …` / the actionable unsupported message). Exit codes as `hop run` |
| `hop result submit` | section 7 | Worker-facing |
| `hop launch` | section 6 | Worker exec boundary; listed in usage under a "worker plumbing" heading |

Run IDs are accepted as full UUIDs or the repository-scoped `r<seq>` label
shown by `status`. `version`, `doctor` and `plugin-context` are unchanged;
`doctor` gains the resolved-store-path line (section 4).

## 9. Test plan

Mapped to the layers in [engineering.md](../architecture/engineering.md) and
[plugin-testing.md](../architecture/plugin-testing.md). Every side effect
ships with its interruption/retry tests in the same change, per
[phases.md](phases.md).

| Layer | Tests |
| --- | --- |
| Domain (`internal/domain/...`) | Table-driven transitions for Run/Task/Attempt/Session, invalid transitions, stop monotonicity, result acceptance (accepted / duplicate-idempotent / conflicting / stale), binding supersession, ID parsing (including fuzz on parsers) |
| Application (`internal/app`) | Handwritten fakes for every new port. Scenarios: happy path effect ordering (intent before act before outcome); crash injection between intent/act and act/outcome via a failing fake, then recovery decisions; stop while launching, while running, while checking; resume against each reconciliation case in section 5; fencing (a unit of work under a stale generation cannot commit); `SanitizeEnvironment` tables; check gating (mismatched commit, dirty tree, failing check) |
| SQLite (`internal/adapters/sqlite`) | Real temporary database, separate `*sql.DB` connections for cross-process claims: atomic attempt reservation (partial unique index) raced from two connections; revision conflicts; lease acquisition, takeover, heartbeat expiry and fencing raced from two connections; reopen mid-operation and journal recovery; migrations from empty, reopen at same version, refusal of a future version; constraint coverage (unique accepted result, unique worktree path) |
| Herdr adapter | Fake NDJSON endpoint tests for the new `Runtime` methods (request shapes, error mapping, cancellation), same style as the existing client tests |
| Real process (`test/integration`) | Extends the Phase 1 harness (`prepareServer`, `stagePlugin`, `waitUntil`, artifact retention). CI-safe deterministic run: fixture repository + fixture worker (below) driven end to end — run → worktree → pane env → sanitized exec → result submit → check → completed; duplicate and stale submission against the live store; stop with termination observed; controller kill + resume with the surviving fixture worker (warm reattach); client detach distinct from stop; failed-check run ends `failed` with retained artifacts |
| Live (opt-in) | One named test, `TestLiveClaudeDefaultProfileRun`, gated on `HOP_LIVE_HARNESS=1` and skipped otherwise with an explicit reason: launches real Claude Code in the user's default profile against the fixture repository, drives one small brief to a passing check. Never runs in CI or `make check` |

Fixture repository generator (test helper in `test/integration`): creates a
temporary git repository with an initial commit, a trivial source file, a
committed `check.sh` (pass and fail variants) and a
`.herdr-orchestrator/config.toml` whose `check.command` invokes it. No fixture
data on disk; everything is generated per test, matching the harness's
existing conventions.

Fixture worker: a test-built executable exec'd through the real launcher in
place of the harness argv. It dumps its environment (section 6 assertions),
optionally reports a custom agent identity the way the Phase 1 presentation
fixture does, waits for a scripted trigger, commits a change in the worktree
and invokes the real `hop result submit`, then idles until closed. It makes
the whole slice CI-safe: no model calls, no credentials, deterministic
output.

## 10. Work breakdown

Ordered implementer tasks. Each lands with its package AGENTS.md guides, its
exact rules in `internal/arch_test.go`, and its tests; `make check` and
`make docs-check` pass at every task boundary. Only task 3 edits `go.mod`;
only task 6 edits the Makefile/manifest; `cmd/hop` is edited only in task 6.

| # | Task | Packages / files | Tests | Depends on | Parallel | Tier |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | Domain slice: identity types, run-module entities, pure transitions, typed errors; `internal/domain` index guide; domain rules in the checker | `internal/domain/identity`, `internal/domain/run` | Domain tables, parser fuzz | — | — | Sonnet |
| 2 | Application ports and run controller logic: `StateStore`/`UnitOfWork`, `Runtime`, `Clock`, `IDGenerator`, `CommandRunner`, `ConfigurationSource` declarations; `SanitizeEnvironment`; run/status/stop/resume/submit/check use cases against handwritten fakes; operation-journal and fencing semantics as application behavior | `internal/app` (new files: `ports_run.go`, `runservice.go`, `launchenv.go`, …) | App scenario + table tests with fakes, crash-injection orderings | 1 | — | Sonnet |
| 3 | SQLite store: schema migration 001, typed repositories, unit of work, revisions, operation journal, lease/fencing, store-path resolution | `internal/adapters/sqlite` (+ `go.mod`: `modernc.org/sqlite`) | Real temp-DB suite across separate connections (section 9) | 1, 2 (port shapes) | with 4, 5 | Fable |
| 4 | Sanitizing launcher and local adapters: `Exec` + `CommandRunner` in `internal/adapters/process`, `Clock`/`IDGenerator` in `internal/adapters/system`, policy loading in `internal/adapters/config` | `internal/adapters/process`, `internal/adapters/system`, `internal/adapters/config` | Process-level launcher tests with an env-dumping fixture binary; runner cancellation/exit mapping; config validation tables | 2 | with 3, 5 | Fable |
| 5 | Herdr runtime extension: `Runtime` implementation over `Client` (worktree.create, tab.create with env, send_text, pane read/inspect/close), plus an early real-process probe that the send-text launch line is detected as an agent | `internal/adapters/herdr` (`runtime.go`) | Fake-endpoint protocol tests; the probe test in `test/integration` | 2 | with 3, 4 | Sonnet |
| 6 | Composition and end-to-end: `hop run`/`status`/`stop`/`resume`/`result submit`/`launch` commands wired in `cmd/hop`; fixture repository generator and fixture worker; the full real-process scenarios and the opt-in live test; Makefile target for the live test; doctor store-path line | `cmd/hop`, `test/integration` | Command tables (dispatch, exit codes, flag surfaces); the section 9 real-process suite | 3, 4, 5 | — | Sonnet |

Tasks 3, 4 and 5 run in parallel on separate worktrees once task 2 has fixed
the port signatures. Fable implements the two correctness-critical cores:
the SQLite atomicity/recovery/fencing store (task 3) and the sanitizing
launcher (task 4).

## 11. Open questions and risks

Open questions for the human (settled decisions are not re-asked):

1. Store-path divergence: a future plugin-invoked command would resolve
   `HERDR_PLUGIN_STATE_DIR` while shell-invoked commands resolve the XDG
   default, splitting the store. Preferred fix when it first matters
   (Phase 3): symlink/redirect one root at the other, or drop the
   plugin-dir branch and always use the XDG path?
2. Phase 2 has no retry: a failed check leaves the run `failed` with the
   worktree preserved, and a new brief starts a new run. Acceptable until
   Phase 3 attempts/retries, or should `hop run` grow a `--retry <run-id>`
   that opens a second attempt within the same run in this phase?
3. HOP-driven cold resume is scoped to sessions with an observed
   `native_session_ref`. Is that scoping acceptable for Phase 2, with the
   capture mechanism (whether Herdr exposes the native conversation ID
   through its agent/integration surface) resolved by the task 5 probe —
   and if Herdr does not expose it, cold resume in Phase 2 degrades to
   "Herdr auto-restore plus warm reconciliation" only?

Risks and mitigations:

| Risk | Mitigation |
| --- | --- |
| The send-text launch line is not detected as an agent, or the shell race corrupts the injected line | Task 5 ships an early real-process probe before task 6 builds on it; fallback is `agent.start` after verifying whether its argv can carry the launcher, or a pane whose command is the launcher argv itself; the launcher contract is unchanged in every variant |
| Herdr auto-restore races `hop resume` and duplicates a worker | Resume reconciles before any relaunch decision (absence must be established), bindings are matched by pane and occupant, and the reconciling state absorbs ambiguity instead of launching |
| A stale controller writes after takeover | Fencing generation checked inside every transaction; raced two-connection tests in task 3 |
| `modernc.org/sqlite` behavior differences (locking, WAL) from C SQLite | The store test suite runs against the real driver with separate connections and reopen/recovery cases; no in-memory-only shortcuts |
| Worker (an unsandboxed agent) edits `.herdr-orchestrator/` to weaken the gate | The check argv and env policy are frozen into the run snapshot at `hop run` and never re-read from the tree |
| Login-shell startup files re-export stripped variables | Sanitation happens at exec, after the shell; the integration test plants a re-exporting rc file and asserts removal |
