# Phase 2 design: durable single-worker run

Status: design for [phases.md](phases.md) Phase 2, revised after cross-provider
review. Nothing in this document is implemented; it fixes the decisions an
implementer executes without re-deriving them. It applies the settled account
decisions: accounts are delegated to the harnesses (workers run in each
harness's default profile), HOP performs no logins and stores no credentials,
and a sanitizing launcher at the harness exec boundary strips provider
credential variables by default. HOP runs on the user's normal Herdr server
with no configuration change. Persistence is SQLite via `modernc.org/sqlite`
(pure Go); the CLI stays on the standard `flag` package. The first worker is
Claude Code in the default profile against a fixture repository created by
the tests.

Trust model, stated once: Phase 2 enforces cooperative controller
transitions and freezes policy selection. It is not a security boundary
against the worker, which runs as the same user with a full shell and can in
principle write the store or artifacts directly (see
[RESEARCH.md](../../RESEARCH.md), "Important verified limits"). The controls
in this design protect against accidental races, stale processes and crashed
controllers, not against a malicious same-UID agent.

A bounded real-process spike (task 0, section 10) precedes port freezing.
Items below marked "spike-dependent" name the spike result that settles them;
section 11 lists them together.

## 1. Scope and exit criteria

Phase 2 delivers four commands — `hop run "<brief>"`, `hop status`, `hop stop`
and a scoped `hop resume` — plus two supporting commands the workflow needs
(`hop result submit`, invoked by the worker, and `hop launch`, the sanitizing
exec-boundary launcher). One brief becomes one run with frozen effective
instructions, one task, one attempt and one native worker launched in a
dedicated worktree. The controller is the foreground `hop run` process.

Flow: `hop run` loads repository policy from `.herdr-orchestrator/`, freezes
the run snapshot (check argv and timeout, environment policy, harness, the
assignment artifact and its digest, the absolute state root, generated
identities including a pre-assigned native session reference), persists the
run/task/attempt/session rows and a launch intent, creates a worktree through
Herdr, opens a worker pane with an explicit additive environment, sends one
fixed-grammar launch line, and waits. `hop launch` — the process the line
starts — resolves everything else from the store, records a durable launch
receipt, and execve's the harness with the sanitized environment and an
initial-prompt argv referencing the assignment artifact (section 6). The
worker submits an explicit result tagged with run/task/attempt identities;
Herdr idle/done never completes a task. The controller validates the result,
runs the frozen deterministic check against a fresh export of the submitted
commit, and only a passing check completes the task and run. Artifacts, state
transitions and interrupted-operation evidence are persisted throughout.

Applied delegation decisions:

- Workers run in the harness's default profile. HOP never logs in, never
  reads or writes `~/.claude`, `~/.codex` or opencode state, and stores no
  credential or account entity.
- An optional configuration value may name a user-prepared alternate profile
  directory; HOP resolves it to an absolute path at freeze time and only
  passes it through (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, or opencode's
  documented `HOME`/XDG layout, section 6). No pools, account leases, or
  cross-account resume; those stay in Phase 5.
- The launcher strips provider credential, provider-routing and
  profile-override variables by default, with explicit opt-in passthrough
  per repository or per run (section 6).

Exit criteria (restated from [phases.md](phases.md) with how each is proved):

| Criterion | Evidence |
| --- | --- |
| Repeatable brief → worker → result → check → completion | Real-process test with the deterministic fixture worker, which must read and validate the delivered assignment (section 9); opt-in live test with Claude Code |
| Duplicate result submission is idempotent | Receipt lookup precedes state preconditions (section 7); tested for immediate retries, retries after every terminal state, and retries after controller takeover |
| Stale results fail | Submission with a rotated launch incarnation, a superseded attempt, or a stopped run is rejected and recorded |
| Crash before/after launch does not silently duplicate workers | The operation decision table (section 4) makes an unresolved prior-generation launch intent block relaunch; interruption tests kill the controller between intent and act and between act and outcome; a barrier test resumes controller A after controller B has taken over |
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
| `Attempt` | Task ID, number, session ID, state | Numbers are dense from 1; retry is a new attempt (not exercised in Phase 2 beyond the invariant); result acceptance for new content requires the attempt active and its current incarnation |
| `Session` | Run ID, role, harness, native session reference (assigned or captured, with its source), state | One session executes at most one attempt; Phase 2 has exactly one worker session and no manager; the native reference is immutable once assigned |
| `RuntimeBinding` | Session ID, incarnation ID, server identity, workspace/tab/pane IDs, occupant identity (process identity fields, spike-dependent S2), observed-at, superseded flag | Append-only history, one row per launch incarnation; observations and closes are valid only against the current (non-superseded) binding; supersession requires recorded evidence, never assumption |
| `Worktree` | Repository ID, run ID, path, branch, state | Created before launch; preserved by stop and by failure |
| `Result` | Attempt ID, commit object ID, summary, content digest (opaque validated string, computed by the application), accepted flag | At most one accepted result per attempt; an accepted receipt is immutable; equal digest resubmission is idempotent in every state including terminal ones; a different digest for the same attempt is a conflict |
| `Artifact` | Run ID, optional result ID, kind, path, digest | References files under the run's artifact directory, never blobs in domain state |

Pure transition functions take the current value, the inputs that justify the
transition and an explicit `now time.Time`, and return the next value or a
typed error (`ErrInvalidTransition`, `ErrStaleSubmission`,
`ErrConflictingResult`, `ErrDuplicateResult` distinguishing the idempotent
case). Result acceptance is a pure function of a complete
application-assembled context: attempt state, current incarnation, run stop
state, prior accepted result, and the submitted digest. The digest is an
opaque string the application has already validated and canonicalized
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
creates it. One integrator owns all `internal/arch_test.go` and
[internal/adapters/AGENTS.md](../../internal/adapters/AGENTS.md) edits
(section 10).

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
    // AcquireLease claims or takes over the run's controller lease and
    // returns the new fencing generation. Generations are monotonic for
    // the life of the run row, preserved across release and reacquisition.
    AcquireLease(ctx context.Context, run RunID, controllerID string) (Lease, error)
    Heartbeat(ctx context.Context, lease Lease) error
    ReleaseLease(ctx context.Context, lease Lease) error // marks released; never deletes the row

    // Begin opens a controller unit of work bound to the lease. Commit
    // re-reads the lease inside the transaction and fails with ErrFenced
    // unless the generation still matches.
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

### SubmissionStore (worker and non-controller authority)

Worker-side and third-party writes do not hold the controller lease and are
never stamped with a controller generation. Each method is one internal
transaction with its own contract (section 4, "Transaction authorities"):

```go
type SubmissionStore interface {
    // RecordLaunchReceipt is written by hop launch immediately before exec:
    // run, attempt, incarnation, argv digest, own pid and process start
    // time. Duplicate receipts for the same incarnation are idempotent.
    RecordLaunchReceipt(ctx context.Context, receipt LaunchReceipt) error
    // SubmitResult applies the validation order of section 7 atomically:
    // receipt lookup first, then incarnation and state preconditions, and
    // on acceptance records result + evidence + one durable check request.
    SubmitResult(ctx context.Context, submission ResultSubmission) (SubmissionOutcome, error)
    // RequestStop sets the run's monotonic stop request without a lease.
    RequestStop(ctx context.Context, run RunID) error
}
```

### Runtime (extends the Phase 1 Herdr adapter)

```go
type Runtime interface {
    CreateWorktree(ctx context.Context, req WorktreeRequest) (WorktreeInfo, error) // worktree.create; no env — see launch-environment.md
    OpenWorkerPane(ctx context.Context, req WorkerPaneRequest) (PaneHandle, error) // tab.create inside the worktree with an explicit additive env map
    SendText(ctx context.Context, paneID, text string) error                        // used ONLY for the fixed-grammar launch line (section 6)
    ReadPane(ctx context.Context, paneID string, lines int) (string, error)         // evidence snapshots
    InspectPane(ctx context.Context, paneID string) (PaneProcess, error)            // occupant identity for launch/stop/adoption decisions
    ClosePane(ctx context.Context, paneID string) error                             // stop interruption; callers apply the guarded-close rule below
}
```

Implemented by a new `runtime.go` in `internal/adapters/herdr` over the
existing `Client`. `WorkerPaneRequest.Env` is the additive map from
[launch-environment.md](../architecture/launch-environment.md): it carries
only additions — `HOP_STATE_DIR` (the frozen absolute state root),
`HOP_RUN_ID`, `HOP_TASK_ID`, `HOP_ATTEMPT_ID`, `HOP_INCARNATION_ID` (unique
per launch) — all removal happens in the launcher. `PaneHandle` returns the
workspace, tab and pane IDs for the runtime binding. `PaneProcess` fields
(PID, process start time, command, occupant identity) are frozen only after
spike item S2 establishes what Herdr actually reports; the port lands with
exactly the verified fields. Guarded-close rule: before `ClosePane`, the
caller re-inspects and matches the occupant against the current binding's
recorded identity; on mismatch, missing identity, or inspection failure it
fails closed (no close, operation `reconciling`). If S2 finds no
identity-conditioned close upstream, the residual inspect-then-close race is
documented as a limitation, not papered over.

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
    // Run starts the argv in its own process group, reports the spawned
    // pid and start time through the Started callback before waiting, and
    // kills the whole group on ctx cancellation.
    Run(ctx context.Context, cmd Command, started func(pid int, startedAt time.Time)) (CommandResult, error)
}
```

Implemented in `internal/adapters/process`; used for the deterministic check
and the git verifications. The same package provides
`Exec(argv []string, env []string) error` (a `syscall.Exec` wrapper that
only returns on failure), used by the `hop launch` command.

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
    CheckArgv      []string      // [check] command; required, Load fails without it
    CheckTimeout   time.Duration // [check] timeout; default 10m
    EnvStrip       []string      // [env] strip additions to the harness matrix
    EnvPassthrough []string      // [env] passthrough opt-ins
    ProfileDir     string        // [profile] dir; resolved to an absolute path against the repository root at load
    Harness        string        // [worker] harness; default "claude"; Phase 2 launches only claude
}
```

Complete example of the file:

```toml
[check]
command = ["sh", "check.sh"]
timeout = "10m"

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
in `internal/app` that the command, the controller (check environments) and
the tests share. `EnvPolicy` is a versioned value frozen into the run
snapshot; precedence within it is defined in section 6.

## 4. Persistence

### Store location and layout

One user-scoped SQLite store shared by all repositories and runs, per
[architecture.md](../architecture/architecture.md). The state root is
canonical and identical for every entrypoint:
`${XDG_STATE_HOME:-$HOME/.local/state}/hop`, with `HOP_STATE_DIR` as the
only override. `HERDR_PLUGIN_STATE_DIR` is deliberately not consulted, so a
shell-invoked and a plugin-invoked command can never resolve different
stores; the doctor's new store-path line (computed by a shared resolver in
`cmd/hop` without opening or migrating the database) reports the same path
from both entrypoints and labels the source of resolution (`override` or
`default`). At `hop run` the resolved root is made absolute, frozen into the
run snapshot, and handed to the worker as `HOP_STATE_DIR` in the pane's
additive env, so the launcher and every worker subshell resolve the same
store deterministically. Layout:

```text
<state root>/
  hop.db                                one database for all repositories and runs
  runs/<run-uuid>/artifacts/            assignment artifact, pane snapshots, stop evidence
  runs/<run-uuid>/checks/<operation-uuid>/   per-execution export tree and outputs (section 7)
```

Runs partition by `repository_id` and `run_id` columns, not by database
file. Repository identity is the symlink-resolved absolute path of the
repository root, held in `repositories` with a generated `RepositoryID`.

### Schema (initial migration)

All IDs are UUID TEXT. All times are fixed-width canonical UTC TEXT,
`2006-01-02T15:04:05.000000000Z` (nine-digit fractional seconds, always
`Z`), so equal-length lexical comparison agrees with time order; code still
compares parsed times, never raw strings, for lease decisions. Every mutable
table has `revision INTEGER NOT NULL` (optimistic concurrency: updates run
`... WHERE id = ? AND revision = ?` and zero affected rows is
`ErrRevisionConflict`). Foreign keys are enforced.

| Table | Columns (abridged) | Constraints |
| --- | --- | --- |
| `schema_migrations` | version PK, applied_at | forward-only |
| `repositories` | id PK, root_path, created_at | UNIQUE(root_path) |
| `runs` | id PK, repository_id FK, seq, brief, state, stop_requested_at NULL, revision, created_at, updated_at | UNIQUE(repository_id, seq); seq assigned inside the creating transaction |
| `run_snapshots` | run_id PK FK, check_argv JSON, check_timeout_ms, env_policy JSON (versioned), harness, profile_dir NULL (absolute), state_root (absolute), assignment_path, assignment_digest, created_at | immutable after insert |
| `tasks` | id PK, run_id FK, state, revision, … | one row per run in this phase |
| `attempts` | id PK, task_id FK, number, session_id FK NULL, state, revision, … | UNIQUE(task_id, number); partial UNIQUE(task_id) WHERE state in active set |
| `sessions` | id PK, run_id FK, role, harness, native_session_ref NULL, native_ref_source NULL (assigned, captured), state, revision, … | reference immutable once set |
| `runtime_bindings` | id PK, session_id FK, incarnation_id, server_socket_path, server_instance NULL (spike S2/S3), workspace_id, tab_id, pane_id, occupant_pid NULL, occupant_started_at NULL, launch_kind (initial, resume), observed_at, superseded, superseded_evidence NULL | append-only; UNIQUE(session_id, incarnation_id) |
| `launch_receipts` | incarnation_id PK, run_id, attempt_id, argv_digest, pid, pid_started_at, state (execed, exec_failed), error NULL, recorded_at | written by hop launch; duplicate insert for the same incarnation is idempotent |
| `worktrees` | id PK, repository_id FK, run_id FK, path, branch, state, created_at | UNIQUE(path) |
| `results` | id PK, attempt_id FK, commit_oid, summary, content_digest, accepted, submitted_at | partial UNIQUE(attempt_id) WHERE accepted = 1; UNIQUE(attempt_id, content_digest) |
| `result_submissions` | id PK, claimed_run_id, claimed_task_id, claimed_attempt_id, claimed_incarnation_id, content_digest NULL, outcome (accepted, duplicate, stale, conflicting, malformed), detail, submitted_at | claimed ids are plain TEXT, no FKs, so malformed submissions still leave evidence |
| `check_requests` | id PK, result_id FK, state (requested, claimed, settled), created_at, claimed_generation NULL | UNIQUE(result_id) |
| `artifacts` | id PK, run_id FK, result_id FK NULL, kind, path, digest, created_at | |
| `transitions` | id PK, entity_kind, entity_id, from_state, to_state, reason, generation NULL, at | append-only evidence; generation NULL for non-controller writes |
| `operations` | id PK, run_id FK, generation, kind, state (pending, succeeded, failed, reconciling), intent JSON, act_evidence JSON NULL, outcome JSON NULL, created_at, updated_at | the operation journal; act_evidence records observations made between act and outcome (for example the spawned check pid) |
| `run_leases` | run_id PK FK, controller_id, generation, state (held, released), acquired_at, heartbeat_at, expires_at | generation monotonic for the life of the row; the row is never deleted or reinserted |

### Connections, transactions and driver

- Driver pinned to an exact `modernc.org/sqlite` version in `go.mod`
  (chosen and recorded by the integrator's dependency-pin commit,
  section 10).
- Per-connection settings are applied by the DSN so every physical
  connection in the `database/sql` pool gets them:
  `?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)`.
  A connection-churn test verifies the PRAGMAs on freshly created
  connections, not just the first.
- Write transactions begin `immediate`, so a read-then-write upgrade cannot
  deadlock-then-fail mid-transaction; a busy begin is retried whole with
  bounded backoff (the entire transaction body re-runs; external acts are
  never inside it, so retry is safe by construction). External acts are
  never blindly retried.
- Migrations: ordered, embedded SQL files
  (`internal/adapters/sqlite/migrations/NNN_*.sql` via `embed`). The
  migrator opens an immediate transaction, re-reads the schema version
  inside it, applies only still-pending migrations, and records them; two
  processes opening concurrently therefore serialize correctly. A store
  newer than the binary's latest known migration is an error. Forward-only.

### Transaction authorities

Three write authorities exist, with different fencing:

1. Controller transactions (`StateStore.Begin`): stamped with and fenced by
   the run lease generation; `Commit` re-reads the lease inside the
   transaction and fails with `ErrFenced` on any mismatch. A stale
   controller can never commit new state.
2. Worker transactions (`SubmissionStore.RecordLaunchReceipt`,
   `SubmitResult`): never stamped with a controller generation — a worker
   is not a controller and must survive controller takeover. They are
   validated instead by the launch incarnation: the pane env carries
   `HOP_INCARNATION_ID`, minted at launch-intent time; a new-content
   submission is accepted only when its incarnation is the session's
   current, non-superseded binding. Warm takeover preserves the incarnation
   (the surviving worker stays valid); a genuine relaunch mints a new one
   (the old worker's new submissions become stale). Duplicate receipts are
   exempt (section 7).
3. Stop requests (`SubmissionStore.RequestStop`) and lease operations:
   single-row writes with their own contracts; no generation. Stop is
   monotonic.

### The transaction rule and the operation decision table

External calls never happen inside a database transaction. Every
controller-side effect follows record-intent, act, record-outcome:

1. In one transaction: apply the domain transition, write the `operations`
   row as `pending` with the full intent payload, commit.
2. Immediately before dispatch, revalidate: heartbeat fresh, generation
   unchanged, stop not requested (unless the act is itself part of
   stopping). Then perform the external call. Observations made during the
   act (a spawned pid, a returned pane ID) are recorded promptly in
   `act_evidence` — a plain write, not an external call.
3. In a second transaction: write the observed outcome, mark the operation
   `succeeded` or `failed`, apply the consequent transition, commit.

Fencing the outcome commit does not fence the act itself: a descheduled
controller can perform an already-committed intent after takeover. The
decision table below is therefore normative; the barrier test (controller A
resumes after controller B takes over) exercises it. Unresolved
prior-generation intents block replacement even when a snapshot currently
shows absence.

| Operation | Crash between intent and act | Crash between act and outcome | Takeover with the intent unresolved |
| --- | --- | --- | --- |
| `worktree.create` | Path from intent inspected; exists → adopt as outcome; absent → safe to re-act (idempotent by unique path) | same inspection | same; never create a second worktree path for the run |
| `pane.open` | Intent names workspace/worktree; a pane matching the intent in a snapshot → adopt; none → re-act | same | same; adopted pane is re-inspected before use |
| `launch.send` (launch line) | No launch receipt and no harness occupant → the line may still be buffered; NEVER re-send automatically; mark `reconciling`, capture a pane snapshot, require occupant retirement (guarded close of the pane) before a new incarnation is launched | receipt present → adopt (the receipt is the acknowledgment); no receipt → as previous column | an unresolved `launch.send` from a prior generation blocks relaunch until the pane's occupant is conclusively retired or the receipt appears |
| `pane.close` (stop, retirement) | Re-inspect; occupant already gone → adopt; still present and identity matches → re-act | same | same |
| `check.run` | No pid in `act_evidence` → no process was observed started; safe to start a fresh execution (new operation) | pid recorded → takeover kills that pid guarded by its recorded start time, waits for absence, then applies the unknown-outcome rule (section 7) | same as previous column |

Lease rules: `AcquireLease` succeeds only when the row is absent-for-run
(first acquisition creates it), `released`, or expired; it increments the
generation in place. `expires_at` decisions use parsed times. Heartbeat TTL
30s, interval 10s; a controller whose heartbeat write fails or is fenced
cancels its in-flight external calls and exits its act loop.

## 5. Run state machine

All transitions are pure domain functions; the controller drives them
through the operation journal. The tables are exhaustive: a pair not listed
is invalid.

### Run

| From | To | Cause |
| --- | --- | --- |
| created | launching | launch intent journaled |
| launching | running | launch receipt + agent detection |
| running | completing | accepted result's check claimed |
| completing | completed | passing check receipt, no stop requested |
| completing | running | check failed-unknown, new execution pending (section 7) |
| created, launching, running, completing | stopping | stop requested |
| stopping | stopped | termination of owned work observed (worker and check) |
| launching, running, completing | failed | terminal failure (rejected candidate, failed check, unresumable loss) |
| any persisted non-terminal | resuming | `hop resume` acquired the lease |
| resuming | launching, running, completing, stopping, failed | reconciliation outcome |

### Task

| From | To | Cause |
| --- | --- | --- |
| pending | active | attempt launched |
| active | checking | result accepted |
| checking | completed | passing check |
| active, checking | failed | rejected candidate or failed check |
| active, checking | interrupted | stop |

### Attempt

| From | To | Cause |
| --- | --- | --- |
| reserved | launching | launch intent |
| reserved | interrupted | stop before launch (no act performed) |
| launching | running | receipt + detection |
| launching | reconciling | ambiguous launch (no receipt, occupant unknown) |
| running | submitted | result accepted |
| submitted | checking | check execution started |
| checking | completed | passing check |
| submitted, checking | failed | conflicting/invalid candidate or failed check |
| running, submitted, checking | interrupted | stop, or established worker loss without resume support |
| running, submitted, checking | reconciling | takeover or observation loss; identity not yet established |
| reconciling | running, submitted, checking | warm reattach verified (returns to its prior state) |
| reconciling | interrupted | retirement established |

### Session

| From | To | Cause |
| --- | --- | --- |
| reserved | launching | launch intent |
| launching | active | receipt + detection |
| launching, active | reconciling | ambiguity or takeover |
| reconciling | active | occupant verified against current binding |
| reconciling | lost | absence conclusively established |
| active | stopping | interrupt dispatched |
| stopping | terminated | termination observed |
| reserved, launching | terminated | stopped before any process existed |

A session's settling evidence must come from its current runtime binding;
an observation from a superseded binding is ignored.

### Stop

`hop stop <run-id>` calls `SubmissionStore.RequestStop`. The live controller
(or `hop stop` itself when the lease is free, after acquiring it with a new
generation) then: journals the interrupt intent; marks not-yet-acted work
`interrupted` (a `reserved` attempt is never launched); cancels and reaps
its own running check via `CommandRunner` cancellation (process-group kill)
and, on takeover, the recorded check pid guarded by start time; interrupts
the worker by guarded `ClosePane` against the current binding's occupant
identity; reports the run `stopping` until termination of both the worker
and any check execution is observed (bounded `InspectPane`/snapshot polling,
status events as wakeups); then records `stopped`. Check-outcome
transactions re-read the stop request and record evidence without
completing: stop precedence means a passing check observed after stop was
requested yields `interrupted`, not `completed`. Worktrees and artifacts are
preserved. A stop that cannot observe termination within its deadline leaves
the run `stopping` with a `reconciling` operation and says so.

### Controller signals (detach, distinct from stop)

`hop run`/`hop resume` handle SIGINT/SIGTERM as detach, not stop: journal a
controller-detach event, cancel in-flight external calls, release the lease
(state `released`, generation preserved), print the resume instruction and
exit 1. The worker keeps running; the run keeps its state. A second signal
during shutdown exits immediately. Stopping the run is only ever the
explicit `hop stop`.

### Resume

`hop resume <run-id>` acquires the lease (new generation) and reconciles.
The bootstrap is a bounded `app.Reconcile` (subscribe before snapshot,
Phase 1 semantics, short context) followed by continuous watching; decisions
come from `InspectPane` and the store, not from status events alone.

1. Surviving worker (warm reattach — supported): the current binding's pane
   exists and its occupant identity matches the recorded launch receipt
   (pid + start time, plus S2 fields). Rebind observations, keep the
   incarnation (the worker's submissions stay valid), republish
   presentation tokens (token metadata is not cold-restored, see
   [herdr-surface.md](../architecture/herdr-surface.md)), continue.
2. Herdr auto-restored occupant: never adopted as a conforming worker. A
   restored process was not started through the sanitizing launcher — the
   audited restore path replays no pane env and inherits the server
   environment, so stripped credentials or profile overrides can be back
   (see [native-harness-compat.md](../architecture/native-harness-compat.md),
   Herdr cold-restore interaction) — and it lacks the `HOP_*` submission
   context. When the restored occupant is provably the run's pane occupant
   (pane ID matches the binding; occupant fails receipt-identity match),
   HOP retires it: guarded `ClosePane`, binding superseded with evidence,
   then cold relaunch as in item 3.
3. Absent worker, cold relaunch (supported for Claude): absence
   conclusively established (guarded inspection, and the restore-settled
   condition below) → new incarnation, fresh pane in the same worktree,
   launch line as in section 6 with the resume argv
   (`claude --resume <native-ref>`); the native reference is the session's
   durably assigned or verified captured reference — for Claude it is
   pre-assigned by HOP before first launch (section 6), so no capture is
   needed. Cold relaunch argv is composed per harness from the snapshot's
   harness field; Claude argv is never rendered for another harness, and
   Codex/opencode cold resume is out of Phase 2 scope (their capture is
   unspecified; resume reports an actionable unsupported state).
4. Restore-settled condition: snapshot absence does not exclude a pending
   Herdr restore — agents may appear after client attachment. No verified
   "restoration finished" signal is currently known; spike item S3 probes
   for one. Until one is verified, after a server restart the run stays
   `reconciling` with an actionable report (naming the pane to check) and
   relaunches only once the restored occupant has appeared and been retired
   (item 2) or S3's verified signal establishes settlement. This is an
   honest capability limit, not a silent fallback.
5. Pending or ambiguous operations from the previous generation are
   resolved per the decision table (section 4) before any new act.

Resume tests cover: delayed restore (restore fires after resume's first
snapshot), an inherited key or profile override returning on a restored
occupant, missing `HOP_*` env on the restored occupant, the alternate-profile
configuration, and the takeover barrier.

## 6. Sanitizing launcher and assignment delivery

### Placement

The launcher must be the argv that finally execs the harness:
[launch-environment.md](../architecture/launch-environment.md) verified that
Herdr's env maps are additive and cannot unset an inherited variable, that
`worktree.create` and `agent.start` carry no env, and that panes default to
login shells whose startup files can re-export provider keys after upstream
sanitation ([native-harness-compat.md](../architecture/native-harness-compat.md)).
Whether `agent.start` could instead carry a wrapper argv is unverified
either way; spike item S5 settles it, and S6 probes pane-creation-with-
command. The primary transport below stands unless the spike shows a
strictly simpler supported one.

### Launch line: fixed grammar, no user text

The controller sends exactly one line to the worker pane's shell:

```text
exec '<abs-hop-path>' launch --run <run-uuid> --attempt <attempt-uuid>
```

- The only shell-quoted token is the HOP executable path, single-quoted.
  `hop run` refuses at start (usage error, before any side effect) if that
  path contains a single quote or any control character; UUIDs are
  lowercase hex and hyphens. The line is therefore identical and valid in
  POSIX sh, bash and zsh (the supported shells; others are a documented
  risk). No brief text, profile path or policy ever appears on the line.
- Precondition: `InspectPane` shows the pane's shell as the foreground
  process (bounded retry). This is a precondition, not the authority.
- The authority is the launch receipt: `hop launch` records it durably
  before exec (below). If no receipt appears within the launch deadline
  (default 120s), the line is never re-sent automatically; the operation
  goes `reconciling` with a pane snapshot as evidence, per the decision
  table.

### hop launch behavior

`hop launch --run <uuid> --attempt <uuid>`:

1. Resolves the store from `HOP_STATE_DIR` (present in the pane env; the
   canonical resolution is the fallback), loads the run snapshot, validates
   the attempt is current and `HOP_INCARNATION_ID` matches the session's
   current binding.
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
   before the first launch intent, which removes any dependence on Herdr
   exposing Claude's conversation ID (spike S4 verifies `--session-id`
   end to end against the installed version). The initial prompt is a fixed
   template containing only absolute paths and identities, no brief text:
   it instructs the worker to read the assignment artifact at its absolute
   path and to submit with the absolute HOP path (`<hop> result submit`).
   Argv is passed via execve — no shell parses it, so multiline or quoted
   brief content never needs escaping.
4. Resolves the harness executable to an absolute path using the sanitized
   environment's PATH before exec.
5. Records the launch receipt — run, attempt, incarnation, argv digest, its
   own pid and process start time — then `Exec`s. execve preserves the pid,
   so the receipt's pid is the harness's pid: the occupant identity the
   binding and warm reattach verify against. A failed exec updates the
   receipt to `exec_failed` with the error and exits 1; flag or resolution
   errors before the receipt exit 2/1 with one stderr line, consistent with
   `cmd/hop` exit codes.

### Assignment artifact

The brief and the full worker instructions are one immutable artifact,
`runs/<run-uuid>/artifacts/assignment.md`, written temp-file-then-rename at
freeze time; its digest and path are recorded in the run snapshot, and the
artifact row references it. Delivery is by reference in the initial-prompt
argv — never typed into a running dialog, so there is no post-launch
prompt-injection step to journal or retype. Acknowledgment semantics, stated
honestly: the launch receipt proves the sanitized launcher delivered the
frozen argv (and thus the assignment reference) to the exec boundary;
whether the model read and followed the file is not mechanically provable —
the fixture worker validates delivered content (including multiline and
quoted text) in tests, and the result protocol is the behavioral evidence in
production. Readiness and blocking: after the receipt, the controller waits
for Herdr agent detection within the launch deadline; a detected agent that
reports `blocked` (trust or permission prompt) keeps the attempt in its
state with a `blocked, needs interaction` condition surfaced by
`hop status` — HOP never answers dialogs.

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
real path (`OpenWorkerPane` → launch line → `hop launch`, with the
snapshot's harness argv pointing at the fixture). Asserted: every matrix
variable is absent even when deliberately seeded into the Herdr server's
environment; a login-shell rc file that re-exports a stripped variable
(planted in the test's scratch HOME) does not survive; policy `strip`
additions are removed and `passthrough` entries survive with original
values; `profile.dir` sets exactly the documented shape; `HERDR_*` and
`HOP_*` are present; the fixture validates the delivered assignment content
(multiline, quotes); a HOP path containing a space works and one containing
a single quote is refused at `hop run`; exec failure produces an
`exec_failed` receipt. Process-only tests (no Herdr) cover
`app.SanitizeEnvironment` tables and `adapters/process.Exec` directly.

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
(idempotent receipts, below) and controller recovery (sections 4–5) are
required Phase 2 behavior; creating a second attempt after a failed check is
Phase 3 and is not smuggled in through either.

### Submission

```text
hop result submit --summary "<text>" --commit <oid> [--run ID --task ID --attempt ID]
```

ID flags default from `HOP_RUN_ID`, `HOP_TASK_ID` and `HOP_ATTEMPT_ID`; the
incarnation is read from `HOP_INCARNATION_ID` (no flag — a worker never
chooses it). Validation order inside one `SubmitResult` transaction is
normative:

1. Parse and bound the inputs: IDs canonical UUIDs; commit a full 40-hex
   lowercase object ID; summary at most 4 KiB of valid UTF-8. Failure:
   recorded in `result_submissions` as `malformed` with the claimed values
   (no foreign keys required), exit 1.
2. Existence and agreement: attempt belongs to task, task to run. Failure:
   `malformed`, exit 1.
3. Receipt lookup before any state precondition: compute the canonical
   digest (below) and look up an accepted result for (attempt, digest). Hit
   → `duplicate`: idempotent success in every state, including `completed`,
   `failed`, `stopped` and after takeover — exit 0, print the existing
   result ID, enqueue nothing.
4. New content only: the submission's incarnation must be the session's
   current, non-superseded binding (else `stale`); the attempt must be the
   task's active attempt in `running` (else `stale`); the run must not be
   stopping/stopped (stop precedence, else `stale`). Exit 1 with the reason;
   evidence recorded.
5. A different digest where an accepted result exists for the attempt →
   `conflicting`, recorded, exit 1.
6. Accept: insert the result (partial unique index enforces one accepted
   result per attempt), record the `accepted` submission, insert the unique
   `check_requests` row for the result. Exit 0.

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
complete acceptance context (attempt state, incarnation currency, run stop
state, prior result).

### Deterministic check

On claiming a `check_requests` row, the controller runs one check execution
per the intent/act/outcome rule; the operation ID is the check execution ID
and namespaces all its files.

1. Candidate isolation: the check never runs in the live worktree, which
   the worker can keep changing. The controller materializes a fresh export
   of the submitted commit — `git archive <oid>` extracted to
   `runs/<run-uuid>/checks/<operation-uuid>/tree/` — after verifying the
   commit exists in the worktree's repository. (HEAD/clean-tree equality is
   no longer the gate; the export is.) A worker that submits a commit whose
   own content weakens a committed check script is inside the cooperative
   trust model, stated above; the frozen snapshot only guarantees the
   *selection* of the check command cannot change mid-run.
2. Execution: the frozen `check_argv` with `Dir` = the export tree, a
   sanitized environment, the frozen timeout, in its own process group. The
   spawned pid and start time are recorded in the operation's
   `act_evidence` immediately.
3. Outputs are written to `checks/<operation-uuid>/stdout`, `stderr` via
   temp-file-then-rename, with digests; `artifacts` rows tie them to the
   result. Executions never share paths, so an orphaned writer cannot
   corrupt a later execution's evidence.
4. Outcome transaction re-reads the stop request: exit 0 and no stop →
   attempt/task/run complete; exit non-zero → attempt/task/run `failed`
   with the failing command named; stop requested → evidence recorded,
   `interrupted` (stop precedence).
5. Unknown-outcome rule: if the controller dies during the check, takeover
   finds the pending operation with a recorded pid, kills it guarded by
   start time, waits for absence, marks the operation `failed` with outcome
   `unknown`, and starts a new check execution (new operation ID, fresh
   export) against the same accepted result. Each execution is isolated by
   construction, so re-running is safe; partial artifacts of the dead
   execution are retained under its ID. Completion always requires a settled
   receipt for the current result and execution.

## 8. CLI surface

Consistent with the existing `cmd/hop` contract
([cmd/hop/AGENTS.md](../../cmd/hop/AGENTS.md)): standard `flag`, thin
commands, exit 0 success / 1 failure / 2 usage, deterministic output lines,
one context deadline per command where bounded. Composition does not import
domain packages: commands pass raw strings; `internal/app` parses them into
typed IDs and returns DTO report values for rendering.

| Command | Flags | Behavior and output |
| --- | --- | --- |
| `hop run "<brief>"` | `-C dir` (repository, default cwd), `-env-passthrough VAR` (repeatable, merged into the frozen policy), `-socket` / `-herdr` (as doctor) | Foreground controller. Prints `run <seq-label> <uuid> started`, then one line per transition. Exit 0 only on `completed`; 1 on `failed`/`stopped`/detach/error; 2 on usage — including a missing or check-less `.herdr-orchestrator/config.toml` and a HOP executable path failing the launch-line character rules, both refused before any side effect |
| `hop status` | `-C dir`, `-run ID`, `-all` | Without `-run`: one line per run of the repository. With `-run`: the full detail block — states, worktree path, current binding and incarnation, pending/`reconciling` operations, `blocked, needs interaction` condition, last submission outcome, artifact paths. Exit 0 when rendering succeeds; state is data, not an exit code |
| `hop stop <run-id>` | `-C dir`, `-timeout` (default 30s) | Requests stop; drives it directly when the lease is free (section 5). Reports `stopping` until termination observed, then `stopped`. Exit 0 when `stopped` was reached, 1 when the deadline left it `stopping` |
| `hop resume <run-id>` | `-C dir` | Acquires the lease, reconciles per section 5, continues as the foreground controller. Prints what it established (warm reattach, retirement + cold relaunch, or the `reconciling` report). Exit codes as `hop run` |
| `hop result submit` | section 7 | Worker-facing |
| `hop launch` | `--run`, `--attempt` only (section 6) | Worker exec boundary; listed in usage under a "worker plumbing" heading |

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
| Spike (`test/integration`, task 0) | S1 launch-line detection and shell readiness (sh and zsh login shells, rc noise); S2 pane process identity fields and same-kind replacement distinguishability; S3 restore behavior/timing after server restart, delayed-restore window, settled-signal search; S4 `claude --session-id`/`--resume` with a preassigned UUID in a scratch profile, unauthenticated; S5 `agent.start` argv capability; S6 pane/tab creation with a command argv |
| Domain (`internal/domain/...`) | Table-driven transitions exactly matching the section 5 tables (including the listed pairs' invalidity), stop monotonicity and precedence, result acceptance context (duplicate in every state incl. terminal, conflicting, stale by incarnation, stale by state), binding supersession evidence, ID parsing (with fuzz) |
| Application (`internal/app`) | Handwritten fakes for every port. Scenarios: intent/act/outcome ordering with revalidation before dispatch; every row and column of the operation decision table, including the takeover barrier (A resumes after B took over) and never-resend of the launch line; stop while launching/running/checking, stop cancels the check, stop precedence in the outcome transaction; detach vs stop; resume cases 1–5 of section 5 including delayed restore and restored-occupant retirement; fencing and generation preservation across release/reacquisition; `SanitizeEnvironment` precedence tables; canonical digest vectors; submission validation order (receipt-before-state proven by a duplicate against a `completed` run) |
| SQLite (`internal/adapters/sqlite`) | Real temporary database, separate `*sql.DB` handles for cross-process claims: atomic attempt reservation raced from two connections; revision conflicts; lease acquisition/takeover/heartbeat-expiry/fencing raced from two connections, generation monotonic across release; concurrent open+migrate from two processes; connection-churn PRAGMA verification on fresh pool connections; immediate-transaction busy retry; reopen mid-operation and journal recovery; fixed-width timestamp round-trips; constraint coverage (one accepted result, unique (attempt, digest), unique check request, unique worktree path) |
| Herdr adapter | Fake NDJSON endpoint tests for the new `Runtime` methods (request shapes, error mapping, cancellation), same style as the existing client tests; `PaneProcess` decoding matches the S2-verified fields |
| Process/config/system adapters | `Exec` and process-group `CommandRunner` behavior (cancellation reaps the group, `started` callback ordering); TOML decoding tables: comments, escaped strings, multiline arrays, duplicate keys, unknown keys, invalid timeout/harness, relative profile dir resolved absolute |
| Real process (`test/integration`) | Extends the Phase 1 harness (`prepareServer`, `stagePlugin`, `waitUntil`, artifact retention). CI-safe deterministic run: fixture repository + fixture worker driven end to end — run → worktree → pane env → launch line → receipt → sanitized exec (section 6 assertions) → assignment content validated by the fixture → result submit → export-tree check → completed; duplicate submission after completion; stale submission from a retired incarnation; stop with termination observed including a running check; controller kill + resume warm reattach; server restart + restored-occupant retirement path (as far as S3 findings allow); detach distinct from stop; failed-check run ends `failed` with retained artifacts; death during check, after output creation, and before outcome commit |
| Live (opt-in) | One named test, `TestLiveClaudeDefaultProfileRun`, gated on `HOP_LIVE_HARNESS=1` and skipped otherwise with an explicit reason: real Claude Code in the user's default profile against the fixture repository, one small brief to a passing check, plus one `--resume <preassigned-uuid>` continuation. Never runs in CI or `make check` |

Fixture repository generator (test helper in `test/integration`): a
temporary git repository with an initial commit, a trivial source file,
committed `check.sh` pass/fail variants and a
`.herdr-orchestrator/config.toml` per the section 3 example. Fixture worker:
a test-built executable substituted as the snapshot's harness; it dumps its
environment, validates the assignment artifact against expected content
(multiline, quotes), optionally reports a custom agent identity as the
Phase 1 presentation fixture does, waits for a scripted trigger, commits a
change and invokes the real `hop result submit`, then idles until closed.
Everything is generated per test; no fixture data on disk.

## 10. Work breakdown

Ordered implementer tasks. Each lands with its package AGENTS.md guides and
tests; `make check` and `make docs-check` pass at every task boundary.
Integrator role: the task 6a implementer also serves as integrator from the
fan-out point — sole owner of every `internal/arch_test.go` and
[internal/adapters/AGENTS.md](../../internal/adapters/AGENTS.md) edit and of
`go.mod`: after task 2, the integrator lands one dependency-pin commit
(exact `modernc.org/sqlite` and `github.com/pelletier/go-toml/v2` versions)
before tasks 3–5 fan out, and tasks 3–5 merge through the integrator. The
Makefile is edited only in task 6b; `cmd/hop` only in task 6a.

| # | Task | Packages / files | Tests | Depends on | Parallel | Tier |
| --- | --- | --- | --- | --- | --- | --- |
| 0 | Spike: the six bounded real-process probes (S1–S6); findings reported before task 2 freezes ports | `test/integration` (probe tests + retained evidence) | the probes themselves | — | with 1 | Sonnet |
| 1 | Domain slice: identity types, run-module entities, pure transitions per the section 5 tables, typed errors; `internal/domain` index guide; domain rules in the checker handed to the integrator | `internal/domain/identity`, `internal/domain/run` | Domain tables, parser fuzz | — | with 0 | Sonnet |
| 2 | Application ports and controller logic: all section 3 ports (shapes finalized against S1/S2 findings), `SanitizeEnvironment`, canonical digest, run/status/stop/resume/submit/check use cases against handwritten fakes, the operation decision table as behavior, DTOs for composition | `internal/app` | App scenario + table tests incl. the takeover barrier | 0, 1 | — | Sonnet |
| — | Integrator dependency pin: one commit adding both third-party pins to `go.mod` | `go.mod` | build | 2 | — | integrator |
| 3 | SQLite store: schema migration 001, typed repositories, unit of work, revisions, journal, lease/fencing, submission-store contracts, store-path resolver | `internal/adapters/sqlite` | Real temp-DB suite of section 9 | 2, pin | with 4, 5 | Fable |
| 4 | Launcher core and local adapters: `Exec` + process-group `CommandRunner` (`internal/adapters/process`), `Clock`/`IDGenerator` (`internal/adapters/system`), strict TOML policy loading (`internal/adapters/config`). Scope call: task 4 tests only the adapters and `app.SanitizeEnvironment` directly (fixture binaries through `Exec`); the `hop launch` command surface itself is built and tested in 6a/6b, keeping all `cmd/hop` edits in one task | `internal/adapters/process`, `internal/adapters/system`, `internal/adapters/config` | Section 9 adapter rows | 2, pin | with 3, 5 | Fable |
| 5 | Herdr runtime extension: `Runtime` over `Client` with the S2-verified `PaneProcess` fields and guarded-close support | `internal/adapters/herdr` (`runtime.go`) | Fake-endpoint protocol tests | 2, pin | with 3, 4 | Sonnet |
| 6a | Command wiring (integrator): `hop run`/`status`/`stop`/`resume`/`result submit`/`launch` in `cmd/hop`, launch-line rendering and path rules, doctor store-path line via the shared resolver, merges of 3–5 | `cmd/hop` | Command tables: dispatch, exit codes, flag surfaces, signal handling | 3, 4, 5 | — | Sonnet |
| 6b | Scenario integration: fixture repository generator, fixture worker, the full real-process suite of section 9, the opt-in live test, Makefile target for it | `test/integration`, `Makefile` | Section 9 real-process rows | 6a | — | Sonnet |

Fable implements the two correctness-critical cores: the SQLite
atomicity/recovery/fencing store (task 3) and the launcher/process boundary
(task 4).

## 11. Decisions assumed, spike dependencies, open questions, risks

Assumed decisions (directed by the orchestrator, pending human override):

1. Store root is always `${XDG_STATE_HOME:-$HOME/.local/state}/hop` with
   `HOP_STATE_DIR` as the only override; `HERDR_PLUGIN_STATE_DIR` is not
   consulted (section 4).
2. No second attempt in Phase 2. Retrying an acknowledged submission and
   controller recovery are in scope now; a `--retry` command is not
   (section 7).
3. Cold resume uses a HOP-preassigned Claude session UUID
   (`--session-id` at launch, `--resume` on cold relaunch); Codex/opencode
   capture is unspecified in Phase 2 and Claude argv is never rendered for
   another harness (sections 5–6).

Spike-dependent items (task 0 settles these before task 2 freezes ports):

| Item | Dependent design element |
| --- | --- |
| S1 launch-line detection and shell readiness | Launch transport confirmation; readiness precondition details (section 6) |
| S2 pane process identity fields | `PaneProcess` port struct, `runtime_bindings` occupant columns, guarded-close feasibility, whether the inspect-then-close race is closable (sections 3–5) |
| S3 restore behavior and settled signal | Whether resume after server restart can ever relaunch without first observing and retiring the restored occupant (section 5, item 4) |
| S4 `claude --session-id`/`--resume` with preassigned UUID | Cold-resume argv claims and the live test's resume leg (sections 5–6) |
| S5 `agent.start` argv capability | Fallback transport ranking only; the primary design does not depend on it |
| S6 pane/tab creation with command argv | Possible replacement of the send-text transport, removing the quoting/readiness surface entirely |

Open questions for the human (beyond confirming the assumed decisions):
none new; everything else is spike-resolvable or decided above.

Risks and mitigations:

| Risk | Mitigation |
| --- | --- |
| The launch line is not detected as an agent, or shell startup consumes it | S1 probes before ports freeze; the receipt-or-reconciling rule means a lost line never causes an automatic resend or a duplicate worker; fallbacks ranked by S5/S6 findings keep the launcher contract unchanged |
| Herdr auto-restore races resume | Restored occupants are never adopted; relaunch requires observed retirement or an S3-verified settled signal, else `reconciling` with an actionable report (section 5) |
| A stale controller acts on an already-committed intent after takeover | The operation decision table plus pre-dispatch revalidation and the takeover barrier test (section 4); fencing alone is explicitly not treated as external fencing |
| `modernc.org/sqlite` behavior differences (locking, WAL, pool PRAGMAs) | Exact version pin; DSN-applied per-connection settings with churn tests; immediate write transactions with bounded whole-transaction retry; concurrent open/migrate tests |
| Same-UID worker interference with store, artifacts or gate content | Out of scope by the stated trust model; the design freezes policy selection and isolates check execution per export/execution ID, and claims nothing stronger |
| Claude version drift invalidates `--session-id`/strip-matrix assumptions | S4 records the probed version; the strip matrix is version-scoped and named; the live test exercises the real binary |
| Unsupported login shells mangle the launch line | Grammar restricted to a single-quoted path plus UUID flags, valid in sh/bash/zsh; other shells documented as unsupported for worker panes in Phase 2 |
