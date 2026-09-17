# Phase 3 design: manager, workers and communication

Status: design for [phases.md](phases.md) Phase 3. Nothing in this document
is implemented; it fixes the decisions an implementer executes without
re-deriving them, in the same shape as
[phase-2-design.md](phase-2-design.md), whose machinery it extends and never
replaces: the lease/claim model, the sanitizing launcher, the transaction
rule, the corroboration predicate, the close rule and the result protocol
all stand unchanged. Phase 3 adds a manager session, scoped task assignment
with acyclic dependencies, bounded worker concurrency (default two),
one-level-deep delegation, durable pull-only messages, task-attempt
provenance across retries, an independent review, serial integration with
combined-candidate revalidation, and the integration of the Phase 1 native
Agent metadata/view support into actual run state.

Trust model, restated: Phase 3, like Phase 2, enforces cooperative
transitions and accidental-staleness protection, not a security boundary
against a same-UID agent (see [RESEARCH.md](../../RESEARCH.md), "Important
verified limits"). Two guarantees in this design are mechanical and strong
within that model: the manager has no verb that can mark a required guard
passed (section 8), and HOP never types text or keys into a live worker or
manager pane (section 7). What a model does with content it reads is
behavioral, not mechanical, exactly as Phase 2 stated for the assignment
artifact.

A bounded real-process spike (task 0, section 12) precedes port freezing;
its three probes (S8–S10) extend the Phase 2 spike series and are tabulated
in section 13. No probe exists for Herdr's `agent.prompt`/`agent.wait`/
`send-keys` surface because this design deliberately uses none of it
(section 7).

## 1. Scope and exit criteria

Phase 3 delivers the core orchestration alpha: one manager session plans,
up to two concurrent workers implement in separate writer worktrees, an
independent reviewer evaluates the combined candidate, and deterministic
checks — per task and per integration — establish readiness. Everything is
driven by the same foreground controller (`hop run`), the same lease and
the same operation journal as Phase 2.

Flow (the built-in feature workflow, section 8): `hop run "<brief>"` with
`[workflow] mode = "feature"` freezes the run snapshot (Phase 2 fields plus
the workflow policy and the role-instruction artifacts), creates the run
integration branch at the frozen base commit, launches the manager session
through the sanitizing launcher, and enters the controller loop. The
manager plans by creating tasks (`hop task create`, with dependencies) and
closing its plan (`hop plan close`, section 8 — creating a later fix task
reopens it); the controller releases tasks whose prerequisites are
integrated, launches
one worker per released task (bounded by the concurrency slots) in a fresh
worktree branched from the current integration head, and consumes results
exactly as in Phase 2 (result protocol, per-task deterministic check).
A completed task is integrated serially: the controller merges its
candidate into the integration branch and runs the frozen check against
the combined candidate; only a passing combined check makes the task
`integrated` and releases its dependents. When every implement task is
integrated, the controller creates a review task and launches an
independent reviewer against the integration head; the reviewer submits an
explicit verdict (`hop review submit`). Run completion requires, as
application-enforced guards: every implement task integrated, a passing
check receipt for the final integration head, and an approve verdict bound
to exactly that head. Questions flow worker → manager → human and answers
flow back, as durable store rows the recipient pulls; nothing is ever
typed into a running pane.

Exit criteria (restated from [phases.md](phases.md) with how each is
proved):

| Criterion | Evidence |
| --- | --- |
| Manager + two workers exercised | `TestRealProcessFeatureRunEndToEnd` (section 11): fixture manager plans four tasks — three initially eligible (t1, t3, t4) plus t2 depending on t1 — under `MaxWorkers=2`; t1 and t3 launch immediately and are held alive at an observable message barrier, t4 (eligible the whole time) launches only after a slot frees on an observed session termination, all integrate serially, reviewer approves, run completes |
| Dependency release | Same scenario: t2 is not launched until t1 is `integrated` (not merely `completed`) — a distinct assertion from t4's slot wait — proved against the store and the launch journal |
| A relayed question | `TestRealProcessRelayedQuestion`: worker question → manager fetch → manager question to human → `hop answer` → manager answer to the task → worker fetch/ack → work continues; every hop is a journaled row |
| Duplicate / ambiguous message delivery | `TestRealProcessDuplicateAndAmbiguousDelivery`: a worker killed between fetch and ack refetches the same message after relaunch; a duplicate ack is idempotent; a stale-incarnation first ack is refused; the delivery journal shows every attempt |
| Worker interruption | `TestRealProcessWorkerInterruption`: a worker killed mid-attempt is reconciled (Phase 2 machinery), the manager receives the controller's info notice, retries the task, and attempt 2 completes with full provenance |
| Reviewer rejection | `TestRealProcessReviewerRejection`: a reject verdict blocks completion; the manager plans a fix task; the new integration head gets a new review task; approval on the new head completes the run |
| No worker enters a permission dialog through accidental prompt injection | By construction plus evidence (section 7): no port or code path types into a live pane; assignment/message/answer content travels only via argv references, artifact files and store rows; `TestRealProcessInjectionFreeDelivery` seeds hostile content and proves byte-exact file delivery with zero pane input |
| Controller reconnect recovers ownership; no second manager claims the run | `TestRealProcessControllerReconnectNoSecondClaimant`: controller killed; two concurrent `hop resume` invocations race the lease; exactly one wins, reattaches the manager and workers warm; the loser reports the held lease and exits; the store shows one non-terminal manager session throughout |

Out of scope for Phase 3: playbook files and reusable gate definitions
(Phase 4 extracts this workflow into them), harness login-status reporting
(Phase 5), the board (Phase 6), detached controllers, account pools,
worker-to-worker messages, recursive delegation, cross-run messaging, and
any automatic typed "nudge" into an idle harness (section 7 and open
question 3).

## 2. Domain slice

No new domain package. `internal/domain/run` is extended — the
[domain model](../architecture/domain-model.md) already places `Message`
and the task graph in this module — and `internal/domain/identity` gains
`MessageID`, `ReviewID` and `IntegrationID` (same UUID parsing contract).
The Phase 2 rule stands: domain code receives time, identities and digests
as explicit inputs; hashing stays in `internal/app`.

Entity changes and additions (states in section 5):

| Entity | Phase 3 change | Invariants |
| --- | --- | --- |
| `Run` | No new states. Gains the plan flag (feature mode): `PlanClosed`, set by the manager's `hop plan close` and cleared by any accepted `CreateTask` (section 8). Completion context widens: a run completes only through `EvaluateReadiness`, a pure function over the full guard context including the plan flag | Stop precedence and monotonicity unchanged; a feature run's `running` covers planning, execution and integration — rendered conditions, not states; closing an empty plan (zero implement tasks) is refused |
| `Task` | Gains `Kind` (implement, review), `Seq` (dense per run), title, instructions digest, and the extended machine: `pending → ready → active → checking → completed → integrating → integrated`, plus `needs-rework` | Dependencies are edges to same-run tasks; the graph is acyclic (validated at every `hop task create` against the persisted edge set); `ready` requires every prerequisite `integrated`; at most one active attempt (Phase 2 index); a review task never enters `integrating`/`integrated` — its verdict acceptance completes it |
| `TaskDependency` | New value: (task, prerequisite), unique, same run | Immutable once created; edges are created only with their task (a task's dependency set is fixed at creation — simpler than graph edits, and sufficient for this workflow) |
| `Attempt` | Multiple attempts per task: `NewAttempt(number)` with dense numbers from 1; a retry (`hop task retry`) reserves attempt n+1 only when attempt n is terminal | The Phase 2 machine is unchanged except the review-only `CompleteReview` row (section 5); each attempt has its own worktree, session lineage, claims and results; a stale prior-attempt submission is `ErrStaleSubmission` by attempt state (already in the Phase 2 acceptance context) |
| `Session` | `AttemptID` becomes optional (the manager session has none — the revisit recorded in [internal/domain/run/AGENTS.md](../../internal/domain/run/AGENTS.md)); gains `ParentSessionID` (nil for the manager) and the role values `manager`, `implementer`, `reviewer` | Parent and child belong to the same run; a parent must be the run's manager session (one-level delegation — a session with a parent can never itself be a parent); at most one non-terminal manager session per run; the state machine is Phase 2's, identical for all roles |
| `Message` | New: run ID, sender principal (session ID, or the reserved principals `controller` and `human`), recipient address (`manager`, `task:<task-id>`, `human`), kind (`question`, `answer`, `info`), optional `ReplyTo` (answer only), optional `RelayedFrom` (a question relaying another question — the section 7 relay provenance), a durable per-recipient-address enqueue sequence assigned inside the send transaction, an optional caller-stable request ID (section 7 idempotency), body artifact path + digest, created-at | Immutable envelope; an answer's DESTINATION is derived from the original question's sender (never caller-chosen) and references exactly one same-run question addressed to the sender's own address; one accepted answer per question (equal-digest resubmission idempotent, different-digest conflicting and rejected — the Phase 2 receipt rule transplanted); addressing by role/task, never by pane or pid; FIFO is by enqueue sequence, never by caller timestamps |
| `Delivery` / `Ack` | New: append-only delivery observations (message, fetching session, incarnation, at) and a single ack per message (session, incarnation, at) | Delivery is at-least-once: an unacknowledged delivered message is re-served; the FIRST ack requires the recipient's current, non-superseded incarnation (the Phase 2 result eligibility rule); a repeat ack of an acknowledged message is idempotent regardless of incarnation |
| `Review` | New immutable verdict: run, review task, attempt, subject commit + tree object IDs (the tree resolved by the application from the submitted commit against the recorded repository, before the accepting transaction), verdict (`approve`, `reject`), reasons digest, submitted-at | At most one accepted verdict per attempt; acceptance follows the section 8 validation order; a verdict never mutates — a new candidate gets a new review task |
| `Integration` | New: task, source commit (the accepted result), pre-merge integration head, merge commit (once observed), state (`merging`, `checking`, `integrated`, `conflicted`, `check-failed`, `rolled-back`, `interrupted`) | One integration per (task, attempt-that-produced-the-result); serial: at most one non-terminal integration per run (application-scheduled, store-enforced by partial unique index); every state change carries recorded object IDs, never branch names alone; an ancestor/no-op merge is a defined outcome (section 8), not an error or a retry loop |
| `Worktree` | One per attempt (Phase 2 had one per run): branched from the integration head current at attempt creation, base commit recorded | Preserved by stop, failure and retry (a retry's fresh worktree never deletes its predecessor — evidence) |

Pure transition functions keep the Phase 2 shape (explicit `now`, typed
errors). New typed errors: `ErrDependencyCycle`, `ErrDependencyNotIntegrated`,
`ErrDelegationDepth`, `ErrDuplicateAnswer`, `ErrConflictingAnswer`,
`ErrStaleAck`, `ErrNotDelivered` (an ack of a never-delivered message),
`ErrVerdictSubjectMismatch`, `ErrRetryNotTerminal`, `ErrRetryLimit`,
`ErrRunNotAccepting` (a manager verb against a run past `running`),
`ErrEmptyPlan` (closing a plan with no implement task),
`ErrMailboxClosed` (a send to a task whose mailbox acceptance closed) and
`ErrRequestConflict` (a reused request ID with different content).

New pure functions of note, all in the domain or (where they aggregate
cross-entity context assembled by the application) shaped like Phase 2's
`AcceptResult`:

- `ReleaseEligible(task, prerequisites) bool` — every prerequisite
  `integrated`.
- `AcceptVerdict(reviewTask, attempt, prior, submission, ctx, now)` — the
  review acceptance rule (section 8), mirroring `AcceptResult`'s
  receipt-before-eligibility order.
- `AcceptAck(message, deliveries, priorAck, ctx, now)` and
  `NextDeliverable(queue)` — the messaging rules of section 7 (FIFO per
  recipient address; one unacknowledged in-flight message per address).
- `EvaluateReadiness(ctx) (ready bool, missing []GuardShortfall)` — the run
  completion guard (section 8); the ONLY path to `Run.Complete`, and it
  takes only evidence rows (integration states, a check receipt for the
  head, a verdict row bound to the head), never a prose flag.

### Category matrix

One new package and two checker additions, nothing else:
`internal/testsupport/storevectors` (the shared refused-input vectors,
section 11) gains a `test-support` rule row whose production-closure
check proves only test files import it, and the checker itself gains the
type-aware forbidden-call rule of section 7. Otherwise `internal/domain/run`,
`internal/domain/identity`, `internal/app`, the four Phase 2 adapters and
`cmd/hop` all keep their existing rows; the new files land inside them.
The pure-standard allowlists are unchanged (no `crypto/*`/`encoding/*` in
domain). Package guides are updated in the changes that extend each
package.

## 3. Ports (consumer-owned, in internal/app)

Reused unchanged: `StateStore`/`UnitOfWork` lease lifecycle, `Runtime`'s
Phase 2 contracts (close rule, `ServerInstance`, corroboration inputs),
`AgentPresentation`, `Observer`, `Clock`, `IDGenerator`, `CommandRunner`,
`ProcessGroupInspector`, `ArtifactStore`, `ConfigurationSource` (extended
value, same port). One removal: `Runtime.SendText` is DELETED from the
port (section 7) — the send-text launch fallback was never invoked by
`hop run` and an unused typed-input method on a production port is exactly
what the injection invariant must not carry; the S1-verified transport
knowledge stays recorded in the Phase 2 design and the adapter's `Client`
keeps the low-level method for probes only.

Packaging rule for every port change in this phase (the green-boundary
rule of section 12): changes are expressed ADDITIVELY — new standalone
interfaces and new struct fields, never a changed method signature on an
existing interface outside the integrator slice. The blocks below show
the target shapes; where a Phase 2 interface would change (`LaunchClaim`
fields, `LoadLaunchContext` addressing, the `SendText` removal), the
addition lands first and the integrator slice flips consumers and deletes
the deprecated member in one landing.

Extensions:

### UnitOfWork repositories

`UnitOfWork` gains `TaskDependencies()`, `Messages()` (envelopes,
deliveries, acks — controller-side reads and the re-address-free lineage
queries of section 7), `Reviews()` and `Integrations()`. Typed
repositories, domain values, `Save(entity, expectedRevision)` where
mutable; messages, dependencies, deliveries, acks and verdicts are
append-only/immutable and have no revision.

### Worker-authority stores (SubmissionStore and the new sibling interfaces)

The worker authority widens through NEW standalone interfaces implemented
by the same SQLite `Store` (the additive packaging rule above), consumed
via new `Controller` fields. Every method is one internal transaction, no
controller lease, generation-NULL evidence, exactly as Phase 2 defines.

`SubmissionStore` itself changes only additively: `LaunchClaim` gains
`SessionID` (required) and its `AttemptID` becomes optional (B2/section 4 —
NULL only for an attempt-less session); `ClaimCheckExec` generalizes to
claim any pending exec-claimable operation of the current generation —
kinds `check.run` and `integration.merge` (section 8) — keeping its
pid-before-exec contract; `SubmitResult` and `RequestStop` are untouched.
The pending-launch-intent fallback that authorizes a pre-binding claim is
re-keyed from "the run's newest pending launch operation" to "the CLAIMED
SESSION's newest pending launch operation", so two concurrently pending
launches (two workers, or manager plus worker) validate independently and
an earlier launch can never fail currency because a later one's intent is
newer.

```go
type MessagingStore interface {
    // Section 7. All validate the caller's session and incarnation
    // currency; every outcome except an empty fetch commits a receipt.
    // Mutating verbs carry an optional caller-stable RequestID: an
    // identical retry returns the original outcome, a conflicting reuse
    // is refused (ErrRequestConflict).
    SendMessage(ctx, send MessageSend) (MessageOutcome, error)
    FetchNextMessage(ctx, fetch MessageFetch) (MessageDelivery, bool, error)
    AckMessage(ctx, ack MessageAck) (AckOutcome, error)
    // AnswerQuestion records the human's answer to a human-addressed
    // question (controller-machine context, no HOP_* env, no lease).
    AnswerQuestion(ctx, ans HumanAnswer) (MessageOutcome, error)
}

type PlanStore interface {
    // Section 8. Callers must be the run's current manager session
    // (current incarnation), and the run must be `running`
    // (ErrRunNotAccepting otherwise — validated INSIDE the transaction,
    // so a create can never race completion). CreateTask validates
    // title/instructions bounds and the dependency edges against the
    // persisted graph (acyclic, same run), writes the task with its
    // edges and instructions-artifact reference atomically, and clears
    // the run's plan flag. RequestRetry is legal only against a terminal
    // attempt of a needs-rework task below the frozen retry limit.
    // ClosePlan sets the plan flag; closing a plan with zero implement
    // tasks is ErrEmptyPlan. All three take the caller-stable RequestID.
    CreateTask(ctx, req TaskCreate) (TaskCreated, error)
    RequestRetry(ctx, req RetryRequest) (RetryAccepted, error)
    ClosePlan(ctx, req PlanClose) (PlanClosed, error)
}

type ReviewStore interface {
    // Section 8: the reviewer's verdict submission, validated like
    // SubmitResult (receipt before eligibility).
    SubmitReview(ctx, submission ReviewSubmission) (ReviewOutcome, error)
}
```

### ReadStore

A new `LoadSessionLaunchContext(ctx, run RunID, session SessionID)`
addresses launch context by session — covering sessions without attempts
(the manager) — and carries the session's role and, for workers, the task
assignment path; its pending-intent resolution is session-keyed like the
claim fallback above. The Phase 2 `LoadLaunchContext` stays until the
integrator slice removes it with the `hop launch --run --attempt` flip.
`RunDetail`'s launch claim follows the same resolution, for the solo
worker and the feature manager alike: the claim of the incarnation the
session launch context resolves — the current binding's, else, while the
`pane.open` outcome is unrecorded, the session's newest pending launch
intent's; a binding and any pending launch intent of the session that
names another incarnation (not only the newest) surface no claim.
Every reader of the detail therefore decides by claim state whether or
not the binding was recorded (Phase 2 section 4, "claim state decides"):
solo stop retires a live pre-binding launch by its creation label under
the close rule, solo corroboration settles a pre-binding `exec_failed`
claim as the terminal launch failure, the launch-claim deadline applies
only while no claim exists, and `hop status` renders that claim with no
binding.
`RunDetail` gains the task table (states, dependencies, attempt counts),
per-address message queue depths and in-flight message age, pending
human questions, the integration head and guard shortfalls
(`EvaluateReadiness`'s `missing` list rendered verbatim). New:
`LoadMessagingContext(ctx, session SessionID)` for the message CLI verbs
(current incarnation, address resolution) — lease-free like the rest.

### Runtime

Two new methods (and the `SendText` deletion above):

```go
// CreateWorkspace creates a plain workspace (workspace.create) with an
// explicit cwd, additive env and a unique creation label, without focus,
// returning workspace/tab/root-pane IDs. The label names the WORKSPACE
// itself (WorkspaceInfo.label — source-confirmed on 0.9.0), NOT the root
// pane, unlike layout.apply/tab.create labels. Used only for the
// manager's placement; worker and reviewer placement continues to come
// from worktree.create's returned workspace. Shapes S8-confirmed.
CreateWorkspace(ctx context.Context, req WorkspaceRequest) (WorkspaceHandle, error)

// FindWorkspaceByLabel is the workspace.create decision row's recovery
// lookup: resolves a workspace by its unique creation LABEL (a workspace
// attribute), then descends workspace → its sole tab → that tab's sole
// pane to recover the root-pane identity — a fresh recovered workspace
// has exactly one of each, and more than one of either is an error, not
// a guess. Zero matches is (zero, false, nil); two or more workspaces
// with the label is an error. S8-confirmed.
FindWorkspaceByLabel(ctx context.Context, label string) (WorkspaceRef, bool, error)
```

The manager pane itself is then opened with the Phase 2 `OpenWorkerPane`
(a `layout.apply` command pane addressed by that workspace ID) — same
transport, same claim, same corroboration predicate. Recovery of a lost
`workspace.create` response is by creation label through
`session.snapshot`, pinned by S8 before the port freezes.

`ServerInstance` names one server LIFETIME, replacing Phase 2's bare
socket-peer-pid token, whose pid a restarted server could reuse. Herdr
exposes no lifetime value of its own, never re-execs a running server in
place, and starts a new process for every restart and live handoff, so
the herdr adapter renders `herdr-server-lifetime/v1 pid=<peer pid>
start=<sec>.<usec>`: the pid of the process that accepted the connection
(the socket peer) and that process's OS start time. A later process
reusing the pid is created after the earlier holder exited, so its start
time is later, and the token never repeats (only a backwards wall-clock
step landing on the earlier start's exact microsecond could). The token
is implemented on darwin (a raw `kinfo_proc` read, accepted only at the
expected record size carrying the pid) and pinned there by
`TestSpikeLabelSurvivesRestart` under the production transport: it names
the server leader's pid and the start second `ps -o lstart=` reports,
stays identical within a lifetime and changes across a restart. Every
other platform, and any failed lookup, yields "" — unknown — so each
continuity decision there fails closed until a platform identity is added
with its own probe. A token recorded in the Phase 2 `peer-pid:<n>` format
never equals a v1 token, so runs placed by an older HOP compare as
changed: their attested cold relaunch, in-flight hand-off and
launch-ended settlement stay fail-closed. `InspectPane` stamps its result
(`PaneProcess.ServerInstance`) with the lifetime of the server that
answered that very request: Herdr serves one request per API connection,
and the adapter reads the peer pid on that connection and the pid's
lifetime before the request and after the response, stamping only when
both agree.

### ConfigurationSource (extended RunPolicy)

```go
type RunPolicy struct {
    // Phase 2 fields unchanged: CheckArgv, CheckTimeout, CheckRepeatable,
    // EnvStrip, EnvPassthrough, ProfileDir, Harness.

    WorkflowMode    string        // [workflow] mode: "solo" (default) | "feature"
    MaxWorkers      int           // [workers] max; default 2; ≥ 1; counts every non-manager session (open question 2)
    RetryLimit      int           // [retry] max_attempts; default 3; per task
    ManagerRole     string        // [roles.manager] instructions, path relative to .herdr-orchestrator/; required in feature mode
    ImplementerRole string        // [roles.implementer] instructions; required in feature mode
    ReviewerRole    string        // [roles.reviewer] instructions; required in feature mode
    ReviewerHarness string        // [roles.reviewer] harness; default = [worker] harness
    MessageAttention time.Duration // [messages] attention_after; default 120s (section 7's status surface)
    MessageWait      time.Duration // [messages] wait_timeout; default 50s — hop msg wait's default bound
}
```

Complete example of the feature-mode additions to
`.herdr-orchestrator/config.toml`:

```toml
[workflow]
mode = "feature"

[workers]
max = 2

[retry]
max_attempts = 3

[messages]
attention_after = "120s"

[roles.manager]
instructions = "roles/orchestrator.md"

[roles.implementer]
instructions = "roles/implementer.md"

[roles.reviewer]
instructions = "roles/reviewer.md"
harness = "claude"
```

Unknown keys still fail strictly. `mode = "solo"` (or an absent
`[workflow]`) preserves the exact Phase 2 behavior — the Phase 2 suite
keeps passing unmodified. The role instruction files are read at freeze
time, copied into the run's artifact directory with digests, and the
copies are what every launch references; a mid-run edit of the repository
files changes nothing (snapshot immutability, as with the Phase 2 brief).

## 4. Persistence

### Migration 003

(This design's text predates the landed migration
`002_launch_claim_seed_evidence.sql` — the workspace-trust seeding column
— so the version this section originally called "002" lands as 003;
`migrations.go` enforces dense NNN versions and
`internal/adapters/sqlite/AGENTS.md` records the renumbering. Every "002"
in this document's Phase 3 text means this 003; 002 is trust seeding.)

Forward-only, embedded
(`internal/adapters/sqlite/migrations/003_manager_workers_messages.sql`).
Same conventions: UUID TEXT ids, fixed-width canonical UTC times, STRICT
tables, `revision` on mutable rows, foreign keys enforced. "Additive"
means DATA- and BEHAVIOR-preserving for Phase 2 rows, not
`ALTER TABLE ADD COLUMN`-only: relaxing a NOT NULL column in a STRICT
table requires a physical rebuild (create the new table, copy, drop the
old, rename), and the Phase 2 migrator gains explicit rebuild support for
this migration — it runs 003 on a DEDICATED connection with
`foreign_keys` OFF (the pragma is connection-scoped and a no-op inside a
transaction, so the migrator must set it before BEGIN), performs the
rebuilds, runs `PRAGMA foreign_key_check` and fails the migration on any
violation before commit — and that connection is CLOSED, never returned
to any pool, on every path (success, failure, panic-unwind), so a
connection with enforcement off can never serve a later query; the
ordinary DSN-configured pools are untouched throughout. 001 is not
edited.

Changes to landed tables, enumerated against the actual 001 schema
([001_initial_schema.sql](../../internal/adapters/sqlite/migrations/001_initial_schema.sql)):

| Table | 001 → 003 change | Backfill / old-row meaning |
| --- | --- | --- |
| `runs` | add `plan_closed_at` TEXT NULL — the durable plan flag (section 8): set by `ClosePlan`, cleared by an accepted `CreateTask`, read by readiness and review-task creation; it survives reopen and takeover because it is a run-row column under the ordinary revision discipline | NULL (solo runs have no plan) |
| `tasks` | add `kind`, `seq`, `title`, `instructions_path`, `retry_count`, `subject_commit_oid` NULL, `subject_tree_oid` NULL (review tasks only), `mailbox_closed_at` TEXT NULL (section 7's mailbox closure), `created_at`; UNIQUE(run_id, seq). (`instructions_digest` already exists in 001 and is unchanged) | existing solo rows: `kind='implement'`, `seq=1`, `title=''`, `instructions_path=''` (the solo assignment lives in the run snapshot), `retry_count=0`, subjects and mailbox NULL, `created_at` copied from `updated_at` |
| `sessions` | REBUILD: `attempt_id` relaxed to NULL (001 has it NOT NULL), `parent_session_id` FK NULL added (same-run; one-level rule application-validated as below); new partial UNIQUE index one session per run WHERE `role='manager'` AND state NOT IN ('lost','terminated'). The existing `sessions_one_current_per_attempt` index is preserved (SQLite treats NULL attempt IDs as distinct, so attempt-less sessions never collide in it) | rows copied verbatim; `parent_session_id` NULL (solo workers have no manager) |
| `launch_claims` | REBUILD: `attempt_id` relaxed to NULL, `session_id` FK NOT NULL added. NULL `attempt_id` legal only for a claim whose session has no attempt — application-validated inside `ClaimLaunch`'s transaction, since SQLite cannot express the cross-table tie declaratively | `session_id` backfilled from `runtime_bindings` by `incarnation_id`; a claim with NO binding row backfills from the HISTORICAL launch intent — the Phase 2 pane.open/launch.send operation whose intent JSON carries this claim's `incarnation_id`, whose `session_id` field is authoritative (a "current session" substitution is wrong for exactly the interrupted histories a data-preserving migration must keep: after exec_failed or termination there need not be a current session, and a successor session would misattribute the claim); a claim matching no binding AND no intent, or more than one intent, fails the migration with the claim named — real ambiguity is surfaced, never guessed away |
| `check_requests` | REBUILD for the typed check subject (M4/section 8): new shape `id PK, subject_kind ('result','integration'), result_id FK NULL, attempt_id FK NULL, integration_id FK NULL, state, created_at, claimed_generation`, CHECK exactly one subject reference set per kind, partial UNIQUE(result_id) and partial UNIQUE(integration_id) | existing rows copied with `id = result_id` (already a UUID, deterministic), `subject_kind='result'`, `integration_id` NULL |
| `worktrees` | add `attempt_id` FK NULL and `base_commit` NULL (001 has NEITHER column — the Phase 2 base commit lives in the worktree.create operation intent, not the row) | existing rows keep NULL for both; their base remains readable from the operation journal exactly as Phase 2 left it |
| `run_snapshots` | add `workflow` JSON NULL (mode, max workers, retry limit, reviewer harness, message policy, role artifact paths + digests, integration branch name) — immutable like the rest of the snapshot | NULL means solo (Phase 2 rows) |

Every reader that assumed a non-null claim `attempt_id` or a single
pending launch is re-keyed to the session, explicitly: `app.LaunchClaim`
gains `SessionID` and makes `AttemptID` optional; `ClaimLaunch`'s
agreement check validates the claimed SESSION's persisted owning run
(and, when an attempt is claimed, that the attempt belongs to that
session) instead of attempt-owning-run alone; the pre-binding
pending-intent authority (`pendingLaunchIntent`) changes from "the RUN's
newest pending launch operation" to "the claimed SESSION's newest pending
launch operation" — the Phase 2 lookup is a single-worker contract, and
under concurrent launches it would let a later launch's intent invalidate
an earlier claim's currency; `LoadSessionLaunchContext` (section 3) uses
the same session-keyed resolution; the corroboration predicate, result
acceptance, stop retirement and the decision table key claims by
incarnation and are untouched. Solo-mode rows keep a non-null
`attempt_id`, so the Phase 2 suite passes against the rebuilt table; the
different-pid duplicate-launch rejection is preserved verbatim.

The one-level delegation rule is enforced in the application transaction
that creates a child session (parent must be the run's manager session and
must itself have no parent) and proven by tests; SQLite cannot express it
declaratively without recursion, and a trigger would put behavior in the
schema that the domain already owns — the same division Phase 2 used for
the DAG rule ("application/domain validation for DAG and gate rules",
[domain-model.md](../architecture/domain-model.md)).

New tables:

| Table | Columns (abridged) | Constraints |
| --- | --- | --- |
| `task_dependencies` | task_id FK, prerequisite_id FK, created_at | UNIQUE(task_id, prerequisite_id); both same run (application-validated with the acyclicity check inside the creating transaction) |
| `messages` | id PK, run_id FK, sender_kind (session, controller, human), sender_session_id FK NULL, recipient_address (`manager`, `task:<uuid>`, `human`), kind (question, answer, info), reply_to FK NULL, relayed_from FK NULL (question relaying a question — section 7), enqueue_seq INTEGER (per (run, recipient_address), assigned inside the send transaction; FIFO authority), request_id TEXT NULL (informational on the envelope — the request key's authority is the RECEIPT table, so no envelope uniqueness: two runs, or two verbs, may legitimately carry the same ID), body_path, body_digest, body_bytes, created_at | immutable; partial UNIQUE(reply_to) WHERE kind='answer' (one accepted answer per question); UNIQUE(run_id, recipient_address, enqueue_seq); answer destination and same-run agreement validated in the accepting transaction |
| `message_deliveries` | id PK, message_id FK, session_id FK, incarnation_id, delivered_at | append-only; every fetch that served the message, including re-serves |
| `message_acks` | message_id PK FK, session_id FK NULL (NULL for a human ack via `hop answer`), incarnation_id NULL, acked_at | at most one; insertion is the acknowledgement |
| `message_receipts` | id PK, run_id TEXT, op (send, fetch, ack, answer), claimed ids (plain TEXT, no FKs), request_id TEXT NULL, request_digest TEXT NULL (canonical digest of the mutating request's content, for idempotent-retry matching), outcome (accepted, duplicate, conflicting, stale, refused, malformed), created_entity_id TEXT NULL (what an identical retry gets back), detail, at | the messaging analogue of `result_submissions`; partial UNIQUE(run_id, op, request_id) WHERE outcome='accepted' — the SAME (run, verb, request ID) acceptance key as `workflow_receipts`, with refused/malformed observations append-only outside it; every outcome leaves evidence EXCEPT an empty fetch (`none:`), which is deliberately receipt-free — a 1s poll loop must not grow the store (section 13 risks) |
| `reviews` | id PK, run_id FK, task_id FK, attempt_id FK, subject_commit_oid, subject_tree_oid, verdict (approve, reject), reasons_path, reasons_digest, submitted_at | partial UNIQUE(attempt_id) — one accepted verdict per attempt; UNIQUE(attempt_id, reasons_digest) supports idempotent duplicates |
| `review_submissions` | id PK, claimed ids TEXT, outcome, detail, submitted_at | receipts, as for results |
| `integrations` | id PK, run_id FK, task_id FK, result_id FK, source_commit_oid, premerge_head_oid, merge_commit_oid NULL, state, operation_id NULL, revision, created_at, updated_at | UNIQUE(task_id, result_id); partial UNIQUE(run_id) WHERE state in the non-terminal set — serial integration is store-enforced, not just scheduled |
| `retry_requests` | id PK, task_id FK, requested_by_session FK, request_id TEXT NULL, reason, state (pending, consumed, refused), created_at | UNIQUE(task_id) WHERE state='pending'; consumed by the controller's polling loop — a retry retried by request ID after consumption returns the original acceptance (attempt number) from its workflow receipt, never a second pending row |
| `workflow_receipts` | id PK, run_id, op (task-create, retry, plan-close), request_id TEXT NULL, request_digest TEXT NULL, claimed ids TEXT, outcome (accepted, duplicate, conflicting, refused, malformed), created_entity_id TEXT NULL, created_entity_seq INTEGER NULL (the `t<seq>` or attempt number an identical retry gets back), detail, at | append-only; partial UNIQUE(run_id, op, request_id) WHERE outcome='accepted' — ONE authoritative acceptance per request key, with refusals recorded as ordinary additional rows; the duplicate lookup reads the accepted row first (receipt-before-eligibility) |

### Transaction authorities

The three Phase 2 authorities stand; Phase 3 assigns the new writes:

1. Controller transactions (lease-fenced): task release, attempt/session
   creation and every lifecycle transition, integration state, review-task
   creation, run completion, controller info messages to the manager
   (written inside the same transaction as the transition they report —
   one commit, no separate notification step to lose).
2. Worker-authority transactions (`SubmissionStore`, incarnation-validated,
   generation-NULL): result submission (unchanged), message send/fetch/ack,
   review submission, manager task-create and retry requests, launch and
   check-exec claims, stop requests, human answers.
3. Lease operations: unchanged.

Cross-module atomic assignment, as [architecture.md](../architecture/architecture.md)
already licenses: when the controller assigns a released task, the task
claim (`ready → active`), the attempt row, the session row (with parent),
the worktree intent and the launch intent commit together in one
controller transaction.

### The transaction rule and new decision-table rows

External calls never happen inside a transaction; every controller effect
is record-intent → act → record-outcome with pre-dispatch revalidation.
Messaging adds NO rows to the operation decision table: a message send,
fetch, ack, answer, task-create or review submission is a pure store write
with no external act — there is no ambiguous-delivery window inside HOP,
which is precisely why delivery is pull-only (section 7). The ambiguity
that remains (a fetch response lost on its way back into a worker's tool
output) is resolved by the at-least-once re-serve rule, not by a journal
reconciliation.

New operation kinds and their decision-table rows (Phase 2 rows unchanged):

| Operation | Crash between intent and act | Crash between act and outcome | Takeover with the intent unresolved |
| --- | --- | --- | --- |
| `workspace.create` (manager placement) | Adoption by unique creation label — a WORKSPACE attribute on 0.9.0 (S8-confirmed by executed round-trip), so recovery resolves the labeled workspace and then its sole tab and sole root pane (more than one of either fails closed); label absent → bounded wait for the in-flight request, then `reconciling`; never a second create | same | same |
| `integration.init` (the run's integration branch, created at the frozen base commit) | The act is `git update-ref --no-deref refs/heads/hop/r<seq>/integration <base-oid> ""` — the create-only compare-and-swap (G1/G3; `--no-deref` so an existing symbolic ref, dangling or not, refuses instead of having its target created, as the process adapter's real-git probe pins), with no fencing (its expected-old value is the never-again-current empty value); the base commit is frozen in the workflow snapshot inside `InitializeRun`'s transaction. No intent at all (a crash between `InitializeRun` and the intent) means nothing was ever dispatched: a fresh intent is journaled, and any ref its refused CAS finds is foreign, even at the base → failed, the collision named. Every read first asks `git symbolic-ref -q`: a symbolic ref is always a collision — never adopted, never dereferenced, never absence. With the intent unresolved, recovery reads the ref: the intent's base → adopt; another value → failed, the collision named (the human inspects the ref before removing it — it may hold someone's work); not observed present → re-act the SAME operation (a dead controller's surviving dispatch can only write the identical value and create-only lets exactly one land; a refused re-act that then reads the base adopts); a read that cannot be made → `reconciling`, never absence. A failed init leaves the run `created` with the lease released; `hop resume` re-drives a fresh operation only once the prior one settled failed, and `hop stop` takes the run to `stopped`. It is a ref-move intent: stop and the terminal-failure settlement resolve it — completing an absent ref with the same CAS — before any terminal report | same | same |
| `integration.merge` (scratch merge; never touches the ref) | One merge EXECUTION per operation, with the operation ID fixing all three identities immutably: the exec claim (one row, one pid, never re-armed or overwritten), the frozen merge argv, and the private tree path `runs/<run-uuid>/integrations/<operation-uuid>/tree`. The act is two recorded steps: (i) MATERIALIZE — `git worktree add --detach <tree> <premerge-oid>` run to OBSERVED completion under `CommandRunner`, then verify the new tree's HEAD equals the recorded pre-merge head and record the materialization-complete evidence (path + verified HEAD) in act evidence. Adoption of a materialized directory requires that recorded completion evidence, NEVER a HEAD observation alone: `git worktree add` writes HEAD before it finishes populating the checkout and index, and a preparer surviving a dead controller can still be writing a directory whose HEAD already reads correctly — so a recovery that finds the directory without the completion evidence settles the operation failed and allocates a FRESH operation and directory (the ambiguous directory is abandoned, never entered, its possible surviving preparer confined to it, and it is removed only after settlement plus the ordinary retirement checks); (ii) SPAWN the merge through the generalized exec boundary — `hop check-exec --op <operation-uuid> -- git … merge …` (section 8 fixes the argv, including the repo-local hook suppression via `-c core.hooksPath=<empty existing dir under the operation's artifact dir>`) — so a durable pre-exec claim (own pid = group id) exists exactly as for checks, and a Git child surviving a dead controller is retired by the group-retirement rule (argv-matched listing, never a blind signal). A conflict exits non-zero and leaves the tree; no `merge --abort` is ever needed because an operation's tree is never reused. Recovery: no claim after step (ii) dispatched → ambiguous, bounded wait then reconciling (never absence) — implementation note (2b): the merge row's bounded wait is enforced against the intent's durable CreatedAt rather than re-persisted as act evidence, because this operation's act evidence IS the materialization-complete record and the Phase 2 wait-evidence write would clobber it; the window stays durable and enforced across rounds; claim present → retire the group FIRST, settle this operation with the retirement evidence (the old claim row is retained forever as history), THEN read the scratch HEAD — parents exactly {pre-merge head, source} → adopt M; the up-to-date no-op outcome (section 8) → adopt as no-op; anything else → the operation settles failed and any re-act is a NEW `integration.merge` operation under the same integration row, with its own fresh operation ID, claim, argv record, tree path and the takeover's current generation — allocated only AFTER the old operation is settled; the abandoned directory is never adopted and is removed only after settlement | same | same; an unresolved merge blocks further integration (the serial index already prevents a second one) |
| `integration.publish` | The act is `git update-ref refs/heads/hop/r<seq>/integration <M> <expected-head>` — a compare-and-swap on the expected old value, executed directly by the controller. The CAS alone does NOT fence stop or takeover when the head has not moved — a paused controller's publish dispatched after a stop request would still match its expected-old — so the publish carries three explicit layers: (i) PRE-ACT, the Phase 2 revalidation (heartbeat CAS + stop re-read) runs immediately before the `update-ref` dispatch specifically, not merely before the operation; (ii) POST-ACT, the outcome transaction is lease-fenced and re-reads stop — a landed publish can never settle `checking` past either, and a refused outcome routes the published candidate to the stop/rollback path (the reconciliation rule for the pause window); (iii) EXTERNALLY, stop, takeover and terminal-failure settlement retire any unresolved ref-move intent by moving the ref first (the fencing rule below), so a zombie CAS dispatched after that retirement fails at the ref store. Recovery: ref == M → adopt; ref == expected head → re-act (retry-idempotent); any other value → `reconciling` with the observed ref as evidence | same | same, via the fencing rule |
| `integration.reset` (combined-check failure rollback, section 8; also the stop path's retirement of a published-but-unsettled candidate) | The act is two steps with the identity persisted BETWEEN them: (i) create the ROLLBACK COMMIT R (`git commit-tree <premerge>^{tree} -p <M>` — R carries the pre-merge content and keeps the rejected M reachable as its parent) and record R's object ID in `act_evidence` — a plain store write — BEFORE any ref move; (ii) `git update-ref … <R> <M>`. Recovery is decidable in every window: R recorded and ref == R → adopt; R recorded and ref == M → re-act step (ii) only (idempotent CAS); no R recorded and ref == M → re-act from step (i) (a prior orphaned commit-tree object is unreferenced and harmless); anything else → `reconciling` | same | same |
| `worktree.create` (now per attempt) | Phase 2 row verbatim; provenance = repository common-directory equality plus the recorded base commit (now the integration head frozen into the intent). Feature mode resolves an unresolved one on every scheduling pass, on resume, and before stop's and the terminal failure's terminal reports — adopted with its attempt link and the workspace its creation label (the operation ID) names, or settled failed after the bounded wait, the attempt then settling as a terminal launch failure with the budgeted task consequence | same | same |
| `pane.open` (manager, worker, reviewer) | Phase 2 row verbatim — one predicate, one close rule, all roles | same | same |
| `pane.open` whose `exec_pending` claim outlived its placed pane (the launch ended before corroboration) | Phase 2's claim-state logic keeps an `exec_pending` claim ambiguous until it is corroborated or retired; the retirement can also be OBSERVED. The claim settles `exec_failed` — with the fixed reason "launch ended before corroboration: pane absent by id and label; claimed process gone", and the pane id and claimed pid as evidence — only when all of these hold: the claim's own binding is current and not superseded; its pane is positively absent under the one absence rule (`pane_not_found` by id AND a successful creation-label lookup that finds nothing); the claimed process is gone (a successful listing of the claimed pid's own process group shows no member with that pid — the launcher is a session leader, so a living claimed process is always listed there); and server continuity since the placement holds on both sides of those observations (the binding's recorded `ServerInstance` equals a fresh, non-empty read taken immediately before and again after them — server lifetimes are contiguous and a socket path is served by one server at a time, so the observations between were served by that lifetime). The continuity conjunct is what makes the pair conclusive: under one lifetime a pane has a runtime from its creation and keeps its process for its whole life, while after a restart Herdr answers `pane_not_found` for a restored pane whose deferred native restore has not fired, and restores a pane a human renamed under its NEW name, so absence by id and label proves nothing there (`TestSpikeRenamedLabelSurvivesRestart`). From then on the `exec_failed` row decides: a child's attempt fails with the section 5 budgeted task consequence and the manager notice, and the manager lineage's failure cause fails the run. Anything short of the whole set stays ambiguous and settles nothing: any other inspection or lookup error, a pane still answering by label, a failed listing, a pid still listed (a zombie or a recycled pid fails closed), no inspector, an uninspectable pid, and a changed, unknown or older-format server identity — the last reported with the value-free human action: rename a renamed pane back to its launch label, otherwise stop the run once no pane of it remains (stop's own absence observation, which does not read continuity, then finishes it; follow-up STOP-1 records that stop, retirement and feature stop can read a renamed-and-restored pane as absent after a restart). The shapes and the process relation are pinned by `TestSpikeVanishedPaneShapes` (`test/integration/spike_panevanish_test.go`) | same — the scheduling pass's launch corroboration applies it on every round | same — resume's session reconciliation applies it; stop and the terminal-failure shutdown retire an `exec_pending` session only on the same pair, and a vanished pane whose claimed process still runs stays outstanding with the human action named |
| `pane.open` whose `exec_pending` claim outlived a pane whose placement was never recorded (the launch ended before its `pane.open` outcome) — the label-only variant of the row above | With no committed binding there is no pane id to inspect, so absence rests on the creation label: the claim settles `exec_failed` — with the DISTINCT fixed reason "launch ended before its placement was recorded: no pane answers for the creation label; claimed process gone", and the claimed pid as evidence — only when all of these hold: the session has no committed binding; exactly one unresolved, decodable, labeled `pane.open` intent names the session, and no unresolved launch row with unusable identity might; the claim is that intent's own incarnation and `exec_pending`; a successful creation-label lookup finds nothing; the claimed process is gone (the same group listing as above); and server continuity since the launch holds on both sides of those observations (the intent's recorded `ServerInstance`, observed immediately before the pane was created, equals fresh, non-empty reads immediately before and after them — continuity from that point covers the pane's creation and its launcher's claim). The same transaction, which re-reads all of it, resolves that `pane.open` `failed` with a typed outcome saying it was DISPATCHED (its pane's own process wrote the claim) and the pane is gone — never "refused before dispatch" — so nothing stays unresolved; the session's claim keeps resolving through that resolved intent, so the `exec_failed` row decides everywhere afterwards, exactly as above. The label conjunct is sound only within one server lifetime. A claim exists only after the pane's own process ran `hop launch`, so the pane existed at claim time, and Herdr attaches a pane's creation label in the very request that creates it and drops it with the pane (`TestSpikeVanishedPaneShapes`: the first lookup after the create, back-to-back lookups while the pane lives, nothing after exit or close). The one relabelling, a human's `pane.rename`, rewrites that same label: within the lifetime the renamed pane keeps its process, which the process conjunct still observes, but Herdr persists the new name and restores the pane under it after a restart, with a fresh shell or a deferred native restore in place of the claimed process (`TestSpikeRenamedLabelSurvivesRestart`; an unrenamed pane keeps its creation label across a restart, `TestSpikeLabelSurvivesRestart`). So after a restart "no pane answers for the creation label and the claimed process is gone" does not prove the pane gone, and only the continuity conjunct excludes that. Anything short of the whole set stays ambiguous and settles nothing: a failed lookup, a pane answering for the label (it is adopted by label instead), more than one unresolved launch, an unusable row, a live, still-listed or unobservable claimed process, and a changed, unknown or older-format server identity — the last reported with the value-free human action "if a pane of this run was renamed, rename it back to <label>", after which a later round adopts it by its label (every label a detail shows renders through the one escaping boundary, `app.RenderExternal`). RESIDUAL (matches the behavior before this row existed): an unplaced launch whose pane is really gone after a restart has no automatic or attested exit yet — resume stays `resuming` and stop stays `stopping` naming that action; `hop resume --confirm-absent` does not reach an unplaced launch, and extending it would amend Phase 2 section 5 item 5 (follow-up ATTEST-1) | same — the scheduling pass's launch corroboration applies it after its label recovery finds nothing | same — resume adopts an unresolved `pane.open` by its label first and applies it otherwise; stop and every retirement boundary apply it before reporting the launch outstanding. Solo is unchanged: its unbound-launch rule (`retireUnboundLaunch`) keeps failing closed on a claim whose pane answers nothing |
| `session.close` (completion retirement, section 8) | Phase 2 `pane.close` row verbatim | same | same |

There is no standing integration checkout: each integration operation
gets its own scratch detached checkouts under
`runs/<run-uuid>/integrations/<operation-uuid>/` (the check pipeline's
isolation pattern), removed after the operation settles, and the
integration branch is moved ONLY by the `update-ref` compare-and-swap
operations above. Phase 2's honesty about fencing ("fencing the outcome
commit does not fence the act") is answered here in three parts. First,
scratch merges carry a durable exec claim and are retired by the group
rule; a merge re-act is always a new operation with its own claim.
Second, the NEVER-REVISIT rule: every rollback publishes a fresh rollback
commit R rather than returning the ref to a previous value, so the
integration ref never holds the same object ID twice and an expected-old
value is current at most once — a delayed publish or reset dispatched
after the head has moved always fails its CAS. Third, the REF-FENCING
rule closes the residual window the CAS cannot see — a delayed dispatch
whose expected-old is still current because nothing else moved the head
(the paused-publish-after-stop case): before stop, a takeover OR a
terminal-failure settlement reports any terminal state while an
UNRESOLVED ref-move intent exists, it first retires that intent by moving
the ref itself. Retirement DISTINGUISHES the intent kind, because the
invariant is "the ref rests on the last VALIDATED tree", not "the ref
moved":

- Unresolved PUBLISH (the ref still shows the validated pre-candidate
  head): publish a fencing commit F carrying the CURRENT head's tree with
  the current head as parent — content unchanged, expected-old value
  burned.
- Unresolved RESET (the ref shows the rejected candidate M): fencing with
  tree(M) would PRESERVE the rejected content, so retirement instead
  COMPLETES the reset — adopt its persisted R and re-drive the CAS when R
  was recorded, or, when no R was persisted, create the retirement commit
  from the RECORDED VALIDATED PRE-MERGE TREE with the observed current
  head as parent, persist its OID, then CAS; the original reset operation
  settles adopted/completed by this recovery, never fenced-over.
- Unresolved FENCE: a fence is itself a first-class journaled operation,
  `integration.fence` — its intent records the observed head and the
  operation ID of the intent it retires, its commit OID is persisted in
  act evidence before the CAS (never only implied), and its own recovery
  row is the reset row's shape (ref == F → adopt; ref == recorded head →
  re-act; else reconciling). A crash after the fence CAS and before its
  outcome is therefore decidable, not a fresh ambiguity.

Once the retirement commit lands, the stale intent's expected-old is no
longer current and its zombie CAS is dead; if the zombie wins the race
instead (the ref reads as the zombie's target), the standard rollback
path retires the now-published candidate. Either way the retirement is
finite and evidence-recorded, quiescence is REQUIRED before the terminal
report (a run never reports `stopped` or `failed` with a ref-move intent
still unresolved), and a zombie publish that does land can never SETTLE
anything: outcome transactions are lease-fenced and re-read stop. The
initialization CAS needs no fencing (its expected-old is the
never-again-current empty value). Rollback and fencing commits are
byte-deterministic so a persisted object ID is always reconstructable
for diagnosis, though recovery never recomputes one — it compares the
PERSISTED ID only: `git commit-tree` runs with
`GIT_AUTHOR_NAME=hop GIT_AUTHOR_EMAIL=hop@invalid` (committer
identical), both dates fixed to the operation intent's timestamp, and
the message `hop rollback <operation-uuid>` (or `hop fence
<operation-uuid>`). The barrier tests in section 11 pin both windows
(head moved; head unmoved with a stop). R's and F's parents keep every superseded commit reachable —
no gc-pruning caveat. Every git invocation runs the absolute
`Controller.GitExecutable`, per the Phase 2 invariant, and no worktree
ever has the integration branch checked out (scratch trees are detached),
so `update-ref` never fights Git's checked-out-branch protections.

## 5. State machines and decision tables

All transitions remain pure domain functions driven through the journal.
Tables are exhaustive: an unlisted pair is invalid. The Session table is
Phase 2's verbatim (sessions now carry roles and optional parents with no
machine change). The Attempt table is Phase 2's verbatim for implement
tasks (attempts now recur per task) plus exactly ONE review-only row:

| From | To | Cause |
| --- | --- | --- |
| submitted | completed | review attempts only (`Attempt.CompleteReview`): the accepted verdict, applied atomically inside the verdict-acceptance transaction |

`CompleteReview` is a distinct, independently validated transition (the
`Attempt.Reattach` precedent — it does not share the implement table,
which reaches `completed` only from `checking`). It differs deliberately:
a review attempt's settling evidence is the verdict row itself; there is
no check phase, and fabricating a no-op check receipt to reuse the
implement path would make "check receipt" mean something other than a
deterministic execution — corrupting exactly the evidence class the
section 8 guards depend on. The transition takes the task kind as an
explicit input and is invalid for an implement-task attempt.
The Run table keeps Phase 2's exact state set and pairs, with two causes
re-scoped for feature mode (solo mode is untouched): `running →
completing` fires when `EvaluateReadiness` first holds and the completion
transaction begins child-session retirement — never on an individual
task's check being claimed, which in feature mode leaves the run
`running`; and `completing → completed` fires when retirement of every
live session is observed with no stop requested. `launching → running`
is the manager's settled launch claim. Phase 2's `running → failed`
("terminal failure") cause explicitly includes, in feature mode, any task
reaching `failed` (retry exhaustion or an unretryable terminal attempt):
readiness can never hold again, no verb revives a failed task, and the
run fails fast with evidence once its owned work's termination is
observed — restarting is a new run. Only the changed/new tables are
printed below.

One boundary two implementers would otherwise resolve differently — retry
(new attempt) versus same-attempt recovery — is fixed here: same-attempt
cold relaunch remains EXCLUSIVELY a `hop resume` recovery action for
attempts left `reconciling` by controller loss (the Phase 2 machinery,
per session). A LIVE controller that establishes a worker's absence
(Phase 2 evidence rules) with no accepted result marks the attempt
`interrupted` and notifies the manager — a self-exiting worker is a
behavioral failure and retrying it is manager judgment, never an
automatic same-attempt relaunch. The operation journal separates the two
by cause (worker-exit observation vs takeover reconciliation). In feature
mode `hop resume --confirm-absent` takes a required session argument
(`--confirm-absent <session-id>`), and each attestation is journaled per
session with its own continuity evidence — the Phase 2 single-worker flag
has no meaning across several sessions.

Resume resolves pending and ambiguous launches per the decision table
before any new act, and "resolved" includes handing a launch to the
controller loop that corroborates it: the Phase 2 launch row's takeover
column says an unresolved launch or unsettled claim blocks RELAUNCH until
it is retired or settles — it does not block the loop. Concretely, per
session, before its disposition is decided:

- A session with no committed binding is first adopted by its unique
  creation label (the `pane.open` row), so a launch whose outcome the
  lost controller never recorded is placed at resume; an unbound
  `exec_pending` claim whose label answers nothing goes through the
  label-only launch-ended row (section 4).
- A child's placed launch left unsettled is IN FLIGHT, and does not hold
  the run in `reconciling`, when every one of these is a positive
  observation: the session is an implementer or reviewer and is
  `launching` (the loop corroborates nothing else, so a session already
  failed closed to `reconciling` — a forking wrapper — stays that way);
  its binding is committed, current and not superseded; its claim is
  absent or that placement's own `exec_pending` claim; the `pane.open`
  that recorded the placement is not `failed`; the recorded pane answers
  by id, and THAT inspection was answered by the server lifetime the
  placement recorded (the inspection's own `ServerInstance` stamp equals
  the binding's, non-empty — so a restart between an earlier identity
  read and the inspection cannot pass a restored pane off); and the pane
  has a foreground occupant whose own process is the launch's: with a
  claim, the claimed process; with none yet, HOP's own launcher for
  exactly this session — the member at the pane's shell pid reporting
  argv exactly equal to the placement's frozen `hop launch --run <run>
  --session <session>` command, itself a launcher invocation naming this
  run and session. Under one lifetime a pane id names one pane and a
  command pane keeps its process for its whole life, so that process is
  the one the placement spawned with this incarnation's environment
  (`TestSpikeVanishedPaneShapes` pins that the pane's own process reports
  exactly the argv `layout.apply` ran it with). The run then resumes and the loop's corroboration
  finishes the launch — settlement, the forking-wrapper reconcile, the
  launch-ended row, or the pre-claim launch deadline — exactly as a live
  controller would. Server continuity is a conjunct because a graceful
  Herdr restart keeps PUBLIC PANE IDS (pinned by
  `TestSpikeLabelSurvivesRestart`, which also pins the `ServerInstance`
  token changing across the restart): after a restart the recorded id
  answers again, with a fresh shell or a restored harness in it, so "the
  pane still answers" is evidence of the original launch only under an
  unchanged server lifetime, observed on the inspection itself. Unknown
  or changed continuity, a pane gone by id (even with its label
  answering), another process in the pane, any occupant but this
  session's launcher before a claim, an empty foreground or a failed
  placement keep the run `resuming` with the reason named.
- The existing guards stand: the run never goes to `running` while the
  manager's own launch is unsettled (the bootstrap continuation returns it
  to `launching`, and only the manager's settlement moves it on — never a
  child's, and never from `resuming`); nothing in flight is relaunched or
  cold-relaunched, and no second `pane.open` is ever issued for it; a held
  stop routes to stop handling before any of this.

### Task (kind = implement)

| From | To | Cause |
| --- | --- | --- |
| pending | ready | every prerequisite `integrated` (a task with no prerequisites is created `ready`) |
| pending, ready | interrupted | stop before launch |
| ready | active | assignment transaction (attempt + session + worktree intent committed) |
| active | checking | result accepted (Phase 2 atomic handoff) |
| checking | completed | passing per-task check |
| completed | integrating | integration claimed (serial slot free, dependency order) |
| integrating | integrated | merge committed AND combined-candidate check passed |
| integrating | needs-rework | merge conflict, or combined check failed (branch rolled back per the `integration.reset` row), with the retry limit not yet reached |
| active, checking | needs-rework | the attempt reached a terminal non-completed state (failed per-task check, interrupted worker, exec failure) with the retry limit not yet reached; the task then waits for a manager retry request — the retry, once consumed, is what re-arms it |
| needs-rework | ready | retry consumed: attempt n+1 reserved (below the frozen retry limit), fresh worktree from the current integration head |
| active, checking, completed, integrating, needs-rework | interrupted | stop |
| active, checking, integrating, needs-rework | failed | retry limit reached (the settling transaction moves the task directly to `failed` when no retries remain — it never parks in `needs-rework` with no legal exit), or a terminal attempt failure with no retry path; a failed implement task fails the run (intro above) |

### Task (kind = review)

| From | To | Cause |
| --- | --- | --- |
| ready | active | reviewer assignment (created `ready`; review tasks have no dependencies and no integration) |
| active | completed | accepted verdict (approve OR reject — the verdict's content gates the run, not the task state) |
| ready, active, needs-rework | interrupted | stop (a reviewer can be stopped between a failure and its retry consumption) |
| active | needs-rework | reviewer attempt interrupted/failed with retries remaining and a controller-issued retry (review retries are controller policy, not manager judgment: the same candidate must still be reviewed) |
| needs-rework | ready | retry consumed |
| active, needs-rework | failed | retry limit reached |

A review task is created by the controller only (section 8), with a frozen
subject (integration head commit + tree object IDs) in its instructions
artifact and its row; `hop task create` refuses `kind=review`.

### Message delivery

| From | To | Cause |
| --- | --- | --- |
| queued | delivered | first fetch by the recipient address's current session (delivery row written in the fetch transaction) |
| delivered | delivered | re-serve: any later fetch by the address's current session while unacknowledged (a new delivery row each time — at-least-once, journaled) |
| delivered | acknowledged | first valid ack: a delivery row exists for the acking session itself, at its current incarnation, and the session is still its address's current session (section 7 — a predecessor's delivery never authorizes a successor's ack, and a retired session's ack never settles its successor's message) |
| queued, delivered | acknowledged | `human`-addressed questions only: the accepted `hop answer` acknowledges the question in the same transaction — directly from `queued`, since a human question is never fetched (its rendering in `hop status` is not a delivery) |

Per-recipient serialization: for each recipient address (`manager`,
`task:<id>`, `human`) at most one message is `delivered`-unacknowledged at
a time; `FetchNextMessage` serves the in-flight message again if one
exists, else the queued message with the lowest `enqueue_seq` — the
durable per-address sequence assigned inside the send transaction, so
FIFO means commit order, never caller clocks (a delayed writer or a clock
adjustment cannot jump the queue). The table above is exhaustive about
acks too: `queued → acknowledged` exists ONLY for the human-answer row —
a session's ack of a message that was never delivered to it is refused
(`ErrNotDelivered`), so a message can never be silently skipped.
An address's "current session" is one lineage member, never the whole
lineage: for `manager`, the run's manager session that has not ended;
for `task:<id>`, the session of the task's CURRENT attempt — the task's
newest (highest-numbered) attempt, not terminal — that has not ended. A
retired attempt's session (its attempt terminal or succeeded by a retry,
or the session itself lost or terminated, whatever its binding says) is
never served the address and cannot acknowledge a message it was served
while current: that message stays in flight and is re-served to the
current session, which acknowledges it after its own fetch.
There is no message TTL and no expiry in Phase 3; an unfetched queue ages
visibly in `hop status` (section 7's attention condition).

Mailbox closure and admission (the drain-then-submit contract made
transactional — "drain first" alone is not atomic, and a send landing
between the drain and the acceptance would otherwise strand a message
against a retired recipient forever): a task's mailbox has a durable
closed flag (`tasks.mailbox_closed_at`, section 4). The RESULT- and
VERDICT-ACCEPTANCE transactions re-read the mailbox: if any queued or
delivered-unacknowledged message addresses the task, the submission is
refused with the retryable outcome `transient: undelivered messages;
drain with hop msg next, ack, then resubmit` (a receipt, no state
change); if the mailbox is clear, acceptance commits AND closes the
mailbox atomically. Results and verdicts share ONE acceptance order
(`AcceptResult`, `AcceptVerdict`): a prior accepted result or verdict
first (duplicate or conflicting), then the caller's incarnation
currency, the run's stop request, the attempt's state and launch claim,
and only then the mailbox. A caller that can never be accepted — a
superseded or replaced incarnation, a run with a stop request, an attempt
already terminal — is therefore refused `stale` rather than told to
drain a queue its own fetch could never serve, and an attempt still
launching with an unsettled claim is told `transient: attempt not yet
running; retry` whatever its mailbox holds; only an otherwise eligible
caller gets the drain line. Closure is not only acceptance's: EVERY transaction
that makes the recipient permanently non-resumable closes admission in
the same commit — a task settling `failed` (retry exhaustion or an
unretryable terminal attempt, with or without any accepted result)
closes its mailbox there, and the controller's failure notice to the
manager names the orphaned obligations (the queued and
delivered-unacknowledged message IDs at closure, journaled in the
notice's body and retained as rows forever — never acked, never
fabricated, rendered under the failed address in status). The notice
body is a file, and files are written before transactions (the
file-first rule), so the promised at-closure ID set is made true by a
SNAPSHOT-EQUALITY contract: before the failure-settlement transaction,
read the pending-obligation ID set and prepare the immutable notice file
from that snapshot; inside the settlement transaction, re-read the
queued and delivered-unacknowledged message ID set for the address and
require exact equality with the prepared snapshot; on mismatch, roll
back without closing the mailbox or changing lifecycle state, prepare a
new notice outside the transaction, and retry; on equality, atomically
close admission, settle the failure and commit the prepared notice
envelope. No file write occurs inside the transaction, and a send that
lands between preparation and settlement forces the retry rather than
vanishing from the notice. A RETRIABLE
interruption is explicitly different: `needs-rework` leaves the mailbox
OPEN, since the successor attempt's session fetches the same task
address. An ordinary `SendMessage` (question or info) is admitted only
while the run is `running` AND, for a `task:<id>` destination, the
mailbox is open. A run that can never accept again (completed, failed,
stopping or stopped, or any run with a stop request — the run-level
analogue of closure) refuses `refused: run-not-accepting`; a run that
can still reach `running` (created, launching, resuming or completing)
answers the retryable `transient: run not yet running; retry` instead
(section 7, "Run-state acceptance"); a closed mailbox refuses
`refused: mailbox-closed`. A session `answer` whose DERIVED destination
is a task is admitted by the same mailbox rule, with no run-state gate:
once any prior answer to the question is resolved (a replay of an answer
accepted before the closure stays a duplicate), a closed destination
mailbox refuses it `refused: mailbox-closed` and nothing is inserted —
otherwise a task could proceed past acceptance with an unconsumed
answer. Every outcome leaves a receipt — the sender's
CLI failure is the manager's signal, never a stranded row. SQLite serializes every
send/close pair, answers included, so exactly one order exists: either the send lands
first (a pending acceptance is refused `transient` until the worker
drains it; a pending failure settlement records the just-landed message
among the orphaned obligations), or the closure lands first and the send
is refused. A retry (attempt n+1) REOPENS a mailbox closed by
ACCEPTANCE in the same transaction that reserves the attempt; a mailbox
closed by task FAILURE is never reopened (no verb revives a failed
task). Raced from separate store handles in both orders and on both
closure causes in the sqlite suite, and as a real-process scenario
(section 11).

### Integration

| From | To | Cause |
| --- | --- | --- |
| merging | checking | merge commit M produced in the scratch tree AND published to the integration ref by compare-and-swap (the `integration.merge` and `integration.publish` operations of section 4, both outcomes recorded with commit + tree object IDs) |
| merging | checking | the ancestor/no-op outcome (section 8): the source commit is already an ancestor of (or equal to) the pre-merge head — `git merge` reports up-to-date and creates nothing; no publish runs (the ref is already the candidate) and the check subject is the unchanged head, satisfied by an existing settled receipt for exactly that head or by a fresh execution |
| merging | conflicted | merge conflict observed (non-zero exit, conflicted scratch tree retained as evidence until settlement; no abort step exists — the tree is simply never reused) |
| checking | integrated | passing combined-candidate check receipt for the candidate head |
| checking | check-failed | failing combined check; triggers the `integration.reset` operation |
| check-failed | rolled-back | reset outcome observed: the ref moved to the fresh rollback commit R (section 4), which carries the pre-merge content and keeps the rejected M reachable |
| merging | interrupted | stop before a candidate was published: the merge's claimed group is retired under the group rule; any unresolved publish intent is retired by the ref-fencing rule (section 4) before the run reports stopped |
| checking | rolled-back | stop with a published-but-unsettled candidate: `DriveStop` drives the reset (rollback commit, CAS) so the integration ref never rests on an unvalidated candidate in a stopped run; the integration settles `rolled-back` and the task `interrupted` by stop precedence |
| check-failed | rolled-back | stop with a rollback pending: `DriveStop` completes the reset CAS before reporting `stopped` (a single fenced, evidence-preserving act — a half-published rejected candidate is retired, not abandoned); the task then settles `interrupted`, not `needs-rework`, by stop precedence in the same transaction |

`conflicted` and `rolled-back` settle the integration; in the same
outcome transaction the task moves to `needs-rework` ONLY when no stop is
held and the retry budget has room — under a stop it moves to
`interrupted`, and at the exhausted limit directly to `failed` (the
implement table's rows) — and the controller's info message to the
manager (with the conflict or check evidence paths) commits with it.

### Reference traces

The domain and application suites pin these exact event orders (the format
of Phase 2 section 5; each trace is a complete multi-entity scenario):

1. **Feature happy path, dependency release.** Manager launched (Phase 2
   launch trace verbatim for the manager session); manager `CreateTask` A
   (no deps → `ready`) and B (depends on A → `pending`), then `ClosePlan`
   (plan closed); controller assigns A (Task A ready→active, Attempt A1
   reserved→launching, Session reserved→launching, atomically); A
   submits, and the acceptance commits with it the retirement intent for
   A's session (section 6: an accepted result ends the worker's job — the
   close-rule retirement frees the slot on OBSERVED termination, while
   the fixture stays alive at its composer until closed); per-task check
   passes (Phase 2 trace) → A `completed`; integration A claimed → A
   `integrating`, scratch merge produced and published (CAS) →
   integration `checking`, combined check
   passes → A `integrated` AND — same transaction — B `pending→ready` and
   the controller info message to the manager commits; B assigned and
   completes the same way; review task R created `ready` (plan closed,
   all implement tasks integrated) → reviewer assigned → verdict approve
   accepted (R active→completed, reviewer session retired the same way) →
   `EvaluateReadiness` re-validated inside the completion transaction →
   Run completing→completed after manager retirement (section 8).
2. **Relayed question, no lifecycle movement.** Worker (task B) sends
   `question` q1 to `manager` (queued); manager fetch (q1 delivered);
   manager sends `question` q2 to `human` with `--relay-of q1`
   (relayed_from recorded) and acks q1; human `hop answer` on q2 (answer
   a2 accepted, q2 acknowledged, atomically); manager fetch — a2's
   envelope carries `origin=q1` resolved server-side; manager FORWARDS
   first (`answer` a1, reply-to q1, destination DERIVED as `task:B` from
   q1's sender), THEN acks a2 (forward-before-ack, so a manager crash
   between the two re-serves a2 and the forward's request ID makes the
   redo idempotent); worker fetch (a1 delivered), worker acks, continues.
   No Run/Task/Attempt/Session state changes anywhere in the trace —
   messaging is orthogonal to the lifecycle machines.
3. **Duplicate and ambiguous delivery.** Worker fetches m1 (delivered);
   worker killed before ack; cold relaunch (Phase 2 relaunch trace; new
   session, new incarnation, same task address); new worker fetches → the
   SAME m1 re-served (second delivery row); old incarnation's late ack
   refused (`stale`, receipt recorded); new worker acks (acknowledged);
   second ack of m1 from anyone → `duplicate`, idempotent success; next
   fetch serves m2.
4. **Worker interruption and retry provenance.** Attempt B1 running;
   worker killed; reconciliation establishes absence (Phase 2 evidence
   rules) → Attempt B1 interrupted, Session lost/terminated, Task B
   needs-rework, controller info message to manager — one transaction;
   manager `RequestRetry`; controller consumes it: Attempt B2 reserved
   (number 2), fresh worktree from the current integration head, new
   session — Task B ready→active; B2 completes; the store shows both
   attempts, both worktrees, both session lineages. Exhaustion variant,
   pinned in the same suite: the identical interruption with the retry
   limit already consumed moves Task B directly to `failed` in the
   settling transaction (never a parked `needs-rework`), and the run to
   `failed` once owned-work termination is observed.
5. **Reviewer rejection and re-review.** Review R1 verdict reject accepted
   (R1 completed; `EvaluateReadiness` false — shortfall names the reject);
   controller info to manager with the reasons artifact path; manager
   creates fix task F; F completes and integrates → new head H2;
   controller creates review task R2 with subject H2; R1's verdict is now
   bound to a stale subject by construction (subject ≠ head) and can never
   satisfy the guard; R2 approve on H2 → run completes.
6. **Stop precedence across the new machinery.** Stop requested during
   `integrating`: in the merging window the merge command's group is
   cancelled and retired and the integration settles `interrupted`, with
   any unresolved publish intent retired by the ref-fencing rollback; in
   the checking window (candidate already published) `DriveStop` drives
   the reset so the integration settles `rolled-back` with the ref on the
   rollback commit; tasks/attempts interrupt per Phase 2, every child
   session and the manager are closed under the close rule, and the run
   reports `stopping` until every owned process is observed absent — a
   passing combined check that lands after the stop request records
   evidence and yields `interrupted`, never `integrated`.

## 6. Scheduling, delegation and worker launch

### The controller loop, extended

The Phase 2 loop (heartbeats, transition lines, launch corroboration, stop
routing, 2s check polling) gains polled work sources, all durable rows,
with status events only ever wakeups: pending manager requests (new tasks,
retry requests), released-and-assignable tasks, pending integrations,
review-task creation, and the message-age attention condition. One
scheduling pass per poll tick, deterministic order: consume manager
requests → retire sessions whose attempts have settled (below) →
recompute releases → claim at most one integration → create the review
task when due (plan closed, all implement tasks integrated, no review
task for the current head) → assign released tasks into free slots (task
`seq` order) → corroborate launches → drive checks. Every dispatch
revalidates (heartbeat CAS + stop re-read) exactly as Phase 2 requires.

### Per-attempt session retirement

An interactive harness does not exit when its model turn ends: the landed
launcher starts Claude at its composer, and a command pane closes only
when the PROCESS exits. Without retirement, two submitted workers would
hold both slots forever and readiness could never be reached. So a
session is retired by the controller — the Phase 2 close-rule procedure
verbatim (scrollback captured, occupant verified against the recorded
claim evidence, `ClosePane`, absence observed) — at each of these
boundaries, journaled with the boundary as the reason: an implement
attempt's ACCEPTED result (acceptance itself verified the mailbox clear
and closed it — the section 5 mailbox rule — so nothing is, or can
later become, owed to the retired session); a
review attempt's accepted verdict, approve or reject; and any terminal
attempt outcome (failed check settling the attempt, exec failure,
interruption). The slot frees only on the close's observed-absence
outcome, never on dispatch. A manager session's exec failure has no
attempt to settle and fails the run, as a solo exec failure does in
Phase 2: the feature terminal-failure procedure retires every live child
session and group and quiesces ref moves before marking the run failed,
with stop precedence. The failure is read from the manager's launch
claim, resolved as the session launch context resolves it — through the
current binding, else the session's unresolved launch intent, else the
`pane.open` the label-only launch-ended row resolved — so a bootstrap
whose `pane.open` outcome was never recorded, and whose launcher then
failed and closed its pane, still fails the run rather than leaving it
`launching`. A launch that ended before corroboration — its placed pane
and claimed process both observed gone, or, with its placement never
recorded, its creation label answering nothing and its claimed process
gone, each under unbroken server continuity since the launch — is an
exec failure too, for every role: the section 4 launch-ended row or its
label-only variant settles the claim `exec_failed`, and the same
consequences follow. The fixture principals deliberately stay
alive at a composer-like idle loop after submitting, so the suite proves
retirement actually terminates them rather than relying on process exit.

### Bounded concurrency

`MaxWorkers` (default 2) bounds the count of non-terminal, non-manager
sessions (implementers and the reviewer share the bound — open question 2).
The bound is enforced in the assignment transaction by counting
non-terminal child-session rows inside it, so it can never overshoot even
across a takeover (the count and the new session row commit under the
same fenced transaction); a slot frees only when a session's row reaches
a terminal state (`terminated`, `lost`), which Phase 2's evidence rules
grant only on observed absence or attested retirement — never on a pane
event, a lease expiry or a guess. Stated consequence, deliberate: a
crashed-but-unreconciled worker still occupies its slot, and sessions in
`reconciling` or `stopping` count as occupied, so an unlucky run can sit
at zero free slots until reconciliation establishes termination; `hop
status` renders the occupied slots with each session's state so the human
can see exactly which reconciliation is holding the bound. Freeing a slot
on weaker evidence would let the bound overshoot the real process count,
which is the one thing it exists to prevent.

### One-level delegation

Only the controller creates sessions; child sessions carry
`parent_session_id` = the manager session. `CreateTask`/`RequestRetry`
validate the calling session is the run's current manager (role + current
incarnation); worker and reviewer sessions calling them are refused with a
receipt. A session with a parent can never be recorded as a parent
(application validation in the session-creating transaction). Workers get
no verb that creates work — `hop task create` from a worker-context env
fails closed. This is the accidental-misuse enforcement the trust model
promises; a hostile same-UID process is out of scope, as stated.

### Launch, roles and environment

Every session — manager, worker, reviewer — launches through the
unchanged Phase 2 pipeline: pre-assigned native session reference,
`worktree.create`/`workspace.create` + `OpenWorkerPane` (layout.apply
command pane, absolute paths, creation label), `hop launch` claim before
exec, sanitized environment (`SanitizeEnvironment`, strip matrix
unchanged), corroboration predicate, warm/cold recovery. Differences by
role:

| Role | Placement and cwd | Pane env additions | Initial prompt references |
| --- | --- | --- | --- |
| manager | `CreateWorkspace` (label = the operation ID), cwd = repository root | `HOP_STATE_DIR`, `HOP_RUN_ID`, `HOP_SESSION_ID`, `HOP_ROLE=manager`, `HOP_INCARNATION_ID` | the brief assignment artifact, the frozen manager role artifact, and the worker-protocol crib (section 7's grammar, rendered as a fixed artifact at freeze) |
| implementer | `worktree.create` per attempt (branch `hop/r<seq>/t<tseq>a<n>`, base = current integration head, label = the operation ID), cwd = the worktree | Phase 2 set plus `HOP_SESSION_ID`, `HOP_ROLE=implementer` | the task assignment artifact (manager-authored instructions + acceptance criteria + the frozen implementer role artifact reference + prior-attempt feedback paths on retry) |
| reviewer | `worktree.create` (branch `hop/r<seq>/t<tseq>a<n>` — review tasks have a `seq` and attempts like any task, so ONE branch scheme covers every role; base = the subject integration head), cwd = the worktree | Phase 2 set plus `HOP_SESSION_ID`, `HOP_ROLE=reviewer` | the review assignment artifact: the frozen subject (commit + tree oid), the diff scope (base..head), the reviewer role artifact, and the verdict submission instruction |

`hop launch` takes `--run` and `--session` (the attempt, where one exists,
resolves from the session row); the launch context fails closed on any
missing or disagreeing `HOP_*` variable exactly as Phase 2 specifies.
`LaunchClaim` carries the session key of section 3; the corroboration
predicate is untouched — one predicate for all roles.

Cold resume remains Claude-only (`--resume <native-ref>`), per harness
composition unchanged. The Phase 2 restore policy applies to every session
including the manager: a Herdr-restored occupant is never adopted;
positive-evidence retirement or attestation, then cold relaunch.

Manager lineage, defined: the run's manager is a SUCCESSION of manager
sessions — the manager-uniqueness index admits a successor only once its
predecessor is terminal, and a cold relaunch of the manager creates the
successor bound to the same native reference (the manager conversation
continues; there is no manager attempt to rebind). A child session's
`parent_session_id` permanently references the manager session that was
current AT ITS CREATION — historical provenance, never rewritten — while
"the run's current manager" (the session `CreateTask`/`RequestRetry`/
`ClosePlan` validate against, and the recipient of the `manager` address)
is always the run's sole non-terminal manager-role session. A retired
manager incarnation's verbs and acks fail the ordinary
incarnation-currency checks, and a manager session that has ended (lost
or terminated) is never served the `manager` address nor able to ack a
message it was served, even while its binding is still current
(section 7's fetch and ack authority) — so it can never consume its
successor's queue.

### Integration branch and worktree bases

One ref-namespace scheme, used everywhere (a git ref cannot be both a
leaf and a directory, so every HOP ref for the run lives UNDER
`hop/r<seq>/`): the integration branch is `hop/r<seq>/integration`, and
every task-attempt worktree branch — implement and review tasks alike,
since both have a task `seq` and attempt numbers — is
`hop/r<seq>/t<tseq>a<n>`. G1 exercises exactly this family.

At freeze the controller records the base commit (repository HEAD at
freeze, resolved to an object ID) and creates the integration branch at
that commit with a create-only compare-and-swap
(`git update-ref refs/heads/hop/r<seq>/integration <base-oid> ""` — the
empty expected-old value makes a second creation fail instead of moving
an existing ref).

Worktree base honesty (S9-confirmed by executed evidence): `worktree.create`
honors `--base <oid>` ONLY when the branch
is new — an existing branch is checked out at its current tip and the
base is silently ignored. Branch-name uniqueness per attempt is therefore
a hard invariant, held three ways: the `t<tseq>a<n>` scheme is unique by
construction (run seq, task seq and attempt number are each unique in
their scope); the worktree.create intent is preceded by a refuse-if-exists
check of the exact ref (`git rev-parse --verify`, failing the assignment
with the collision named rather than launching on the wrong base); and
the Phase 2 provenance rule is applied immediately after create as well
as at adoption — the new worktree's HEAD must equal the frozen base
object ID, never trusted from the request. Sibling grouping (the
worktree workspaces grouping with the manager's repository workspace)
rides Herdr's shared repo key in the workspace's worktree info; S8/S9
record the observed grouping but nothing in the design depends on it.

Task worktrees branch from the integration head
current at their attempt's creation, so dependent work always builds on
integrated prerequisite work; that is why dependency release requires
`integrated`, not `completed`. Retry worktrees re-base the same way, and
the retry assignment artifact carries the prior attempt's result commit,
check output and (where present) review reasons paths so the worker can
recover useful work deliberately. HOP never pushes, never merges into the
user's own branches, and never deletes `hop/*` branches; publication of
the integration branch is the human's act. Worktrees are retained
evidence for as long as it is undecided, not forever (human decision,
2026-09-15): a run's attempt worktrees stay visible until the run's
integration branch has merged into the target branch, after which HOP
removes the WORKTREES — the branches are kept (`hop clean` for branches
is future work). The mechanism is lazy: on the next controller start or
`hop status` for that repository, HOP detects that the integration head
is an ancestor of the target branch (checked under an exec claim),
removes the run's attempt worktrees under a fresh exec claim, and
persists a "worktrees retired" run fact so the detection never repeats.
The section 12 worktree-retirement slice owns the implementation; no
earlier slice removes anything.

## 7. Durable messages and delivery safety

### The delivery decision: pull-only, nothing typed

The one Herdr surface that could deliver text into a running harness is
typed terminal input (`agent.prompt`, `pane.send_text`, `send-keys`).
Herdr 0.9.0's own documentation rules it out as a safe automatic channel:
blocked detection is deliberately strict, and "unusual new agent prompts
may initially show as `idle` instead of `blocked`"
([agents.mdx](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/agents.mdx),
"Blocked state") — so a status precondition can never prove a pane is not
sitting at an unrecognized permission dialog, and `agent.prompt` submits
text plus Enter, which against a misdetected dialog could accept its
default. `agent.prompt`'s own `agent_blocked` refusal only covers dialogs
Herdr recognizes, and a prompt timeout "does not prove that no input was
sent"
([agent-automation.mdx](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/agent-automation.mdx)).

Phase 3 therefore delivers everything by pull, and HOP sends NO text or
keys to any live pane, ever:

| Content | Channel | Typed into a pane? |
| --- | --- | --- |
| Brief, role instructions, task assignments, retry feedback | Immutable artifact files under the run's artifact directory, referenced by absolute path in the launch argv's fixed template (Phase 2 mechanism, unchanged) | No — argv via execve, no shell, no dialog |
| Messages, answers, controller notices | Store rows + body artifact files; the recipient pulls with `hop msg next` / `hop msg wait`, whose stdout is envelope lines plus the body's absolute path | No — the body reaches the model as file content it chooses to read |
| Review subjects and verdict feedback | The review assignment artifact and the reasons artifact | No |
| Human answers | `hop answer` writes the answer body as an artifact and the message row; the manager pulls it | No |

Consequences, stated as invariants the implementation and tests must hold —
each CHECKED, not merely documented:

- No production HOP code path can deliver terminal input: `SendText` is
  REMOVED from the `app.Runtime` port (section 3), and no port carries
  any typed-input method (`agent.prompt`, `send_keys` shapes included).
  The mechanically enforced scope is precise (section 11): (i) a
  port-surface test asserts the exact method set of EVERY `internal/app`
  port interface against a reviewed allowlist, so a typed-input method
  appearing on any port — not just Runtime — fails tests; (ii) the
  `internal` checker gains a TYPE-AWARE forbidden-call rule (go/types,
  not the package-selector pattern used for `time.Now` — that pattern
  would miss `c.Runtime.SendText`, aliases and method values): production
  code in `internal/app` and `cmd/hop` may not reference a method named
  `SendText`, `SendKeys` or `Prompt` on any type declared in
  `internal/adapters/herdr` or on any `internal/app` port, AND may not
  call `herdr.Client.Call` at all — the raw transport escapes any
  method-name rule (`Client.Call(ctx, "pane.send_input", …)` from
  composition would bypass every port), so raw calls are confined to the
  adapter package; negative fixtures cover chained selectors, interface
  and type aliases, method values, and a raw `Client.Call` from
  composition; (iii) INSIDE `internal/adapters/herdr`, an AST allowlist
  test governs the actual raw request sites: every production `Call`
  invocation's method argument must be a STRING LITERAL on the reviewed
  method allowlist — any dynamic method expression fails the test
  outright — and the terminal-input methods (`pane.send_text`,
  `pane.send_keys`, `pane.send_input`, `agent.prompt`,
  `agent.send_keys`, `agent.start` — S11-confirmed against
  `repos/herdr/src/api/schema.rs` as Herdr 0.9.0's complete real input
  surface; an earlier draft additionally named a `pane.run` method that
  does not exist on this version) are never on it; the adapter's
  own `SendText` method is deleted with the port member in slice 6, and
  probes/tests reach `pane.send_text` through `Client.Call` in test files
  only. The recording fakes still assert zero typed-input calls
  in every feature scenario, and the invariant is recorded in the
  `internal/app` and herdr-adapter guides in the changes that land it.
- Message bodies are never printed raw to a terminal-bound stream: the CLI
  prints the body artifact path, and body files are written
  temp-file-then-rename with digests. A body containing ANSI escapes,
  bracketed-paste sequences or `y\n` therefore cannot reach any terminal
  input or output path through HOP. Checked by the grammar contract tests
  (real binary, hostile bodies) and `TestRealProcessInjectionFreeDelivery`.
- A recipient that never polls is a reported condition, not a nudge
  target: HOP never wakes an idle model by typing (assumed decision 1). The
  condition's exact surface is specified below under "Idle with pending
  deliveries", and a real-process exit scenario asserts it.

### Recipient operating contract

The harness agents' behavior is specified, not assumed: every launch
prompt and role artifact states the polling contract verbatim (quoting the
section 7 grammar constants), and the fixture principals implement exactly
it, so the deterministic suite proves the contract is followable as
written.

- **Manager**: after acting on its plan (creating tasks, sending
  messages), the manager's standing instruction is to run
  `hop msg wait --timeout 50s` in a loop as its ONLY idle activity. On a
  `message` response it must, before the next wait: read the body file;
  then (kind `question`) send the `answer` — or relay to the human as a
  new `question` to `human` with `--relay-of <the question>` — and
  `hop msg ack` the question; (kind `answer` from `human`) locate the
  relayed worker question through the answer's reply-to chain (the
  human's answer replies to the manager's relay question, whose
  `relayed_from` names the original), send the `answer` to that original
  question, and `hop msg ack` the human's answer; (kind
  `info`) act on the notice if it requires planning (a `needs-rework`
  notice, a reject verdict) and `hop msg ack` it. On the
  `none: no message within <timeout>; run hop msg wait again` line it
  runs the wait again. "No pending deliveries" therefore looks like the
  manager sitting in repeated bounded waits — a working harness running a
  cheap tool call, never an idle turn ended without a wait in flight.
- **Worker (implementer)**: works its assignment; when it needs a
  decision it sends one `question` to `manager` and then loops
  `hop msg wait` until the `answer` arrives (acking it), then continues;
  before `hop result submit` it must drain its queue (`hop msg next`
  until `none:`, acking each). After an accepted or duplicate submission
  its instruction is to end its turn — the controller owns everything
  after submission, and the pane closes when the process exits; once its
  attempt is terminal its own `hop msg next`/`wait`/`ack` are refused
  (message verbs below), never answered.
- **Reviewer**: reads the frozen subject, may ask the manager questions
  under the same wait-loop rule, submits exactly one verdict, ends its
  turn; the accepted verdict completes its attempt, after which its own
  message verbs are refused.
- Every ack is issued only after the body file has been read — the ack is
  the recipient's statement of receipt-and-read, which is what makes the
  serialization rule (next message only after ack) a pacing mechanism
  rather than a formality.

### Idle with pending deliveries: the status surface

`hop status -run <id>` renders, for each recipient address with a
non-empty queue or an unacknowledged in-flight message, one condition
line:

```text
attention: messages pending for <address>: in-flight <age> (message <uuid>), queued <n>, oldest <age>
```

with `<address>` the recipient (`manager`, `task:<uuid>` with the task's
`t<seq>` label, or `human`), the in-flight clause present only when a
delivered message awaits its ack, and ages computed from the delivery/
created-at rows. When the address's session is live and the oldest age
exceeds the attention threshold (default 120s, `[messages] attention_after`
in the policy), the run's summary line additionally carries
`blocked, needs attention`, and the detail names the exact human action:
open that session's pane and check that the agent is following its polling
instructions; or answer pending `human` questions with `hop answer`. This
surface is asserted in the `DuplicateAndAmbiguousDelivery` exit scenario
(section 11), which holds a message in-flight past the threshold while the
worker is dead and asserts the condition line, its counts and its age
fields, then asserts the line clears after the relaunch acks.

The manager's verdict channel is exactly this surface: a review verdict's
controller notice (section 8) carries only the reviewer's reasons text as
its body, never the verdict itself, so `hop status -run`'s
`verdict-rejected` guard-shortfall line — naming the review's own id,
subject commit and reasons path, never just "the latest review" — is how
the manager learns a review was rejected, per the manager assignment's
own standing instruction to run it after every controller info notice
and compare the shortfall's reasons path against the fetched notice's
own body path before acting.

Deferred capability — a dialog-safe nudge: a typed wake-up for an idle
recipient stays out of HOP until Herdr offers a surface that removes the
misdetection window, none of which exists on 0.9.0. Research leads a
future phase would have to probe — none is sufficient on its own, and
each has a known gap: (i) delivery conditional on the pane's status
authority being a complete lifecycle integration rather than a screen
manifest (Claude Code and Codex are `session`-only integrations today —
[agents.mdx](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/agents.mdx));
(ii) an Enter-less submission with composer read-back — though a dialog
that accepts single keys is not made safe by omitting Enter; (iii) a
compare-and-swap input guard — though an unrecognized screen that has not
changed remains exactly as unsafe as before. Reintroducing any typed
input would be a new safety decision for the human, not an implementation
option; until then the attention surface above is the whole answer.

Acknowledgment semantics, stated honestly (the Phase 2 formulation): a
delivery row proves the serve COMMITTED — not that the CLI's printed
envelope reached the model, which is exactly why an unacknowledged
message stays re-servable; an ack row proves the recipient ran
`hop msg ack`. Whether the model read the body file and followed it is
behavioral — the fixture worker validates delivered bytes in tests, and
the workflow's result/verdict protocols are the behavioral evidence in
production.

### Message verbs and validation

All verbs resolve the caller from `HOP_SESSION_ID` + `HOP_INCARNATION_ID`
(worker context, absolute `HOP_STATE_DIR` required, no fallback — Phase 2
rule) except `hop answer`, which is a human/controller-machine command.
Validation orders are normative and mirror section 7 of the Phase 2
design (receipt before eligibility):

"Current incarnation", wherever a verb requires it, is ONE rule — the
principal-incarnation rule — applied identically by `hop launch`'s claim,
`task create`, `task retry`, `plan close`, `msg send`, `msg next`/`msg
wait`, `msg ack`, `review submit` and `result submit` (solo and feature
alike; a result submission resolves the attempt's non-terminated session
first). The claimed incarnation is current for the session iff:

- the session's committed, non-superseded binding carries it, and no
  pending launch intent of the session — ANY of them, not only the newest
  — names a different incarnation or none usable (a binding and a pending
  intent that disagree fail closed, exactly as the launch context and the
  status claim do, even when a newer pending intent agrees); or
- the session has no binding row at all (a superseded row without a
  successor retires its incarnation and disables this fallback), and the
  session's newest pending `pane.open`/`launch.send` intent names it
  (this fallback alone selects the newest).

The second branch is the claim's own pre-binding rule: the launcher is
the pane's own command and can claim before the controller records the
`pane.open` outcome, and a controller that dies between the act and that
outcome leaves exactly this state. A live principal in it — manager or
child — is therefore never refused `stale` for the missing binding; its
verbs proceed normally, the run-state acceptance below still applying
(`transient` while the run can still reach `running`). For a child whose
attempt is still launching behind an `exec_pending` claim (LAUNCH-7,
pinned on both stores), that means: `hop result submit` and
`hop review submit` print `transient: attempt not yet running; retry`
whether or not a message is queued to the task (section 5's one
acceptance order), a queued message is served, and an ack is accepted
after the session's own fetch and `not-delivered` before it; with a
pending intent naming another incarnation, or none at all, the same
caller is `stale` for both submissions and its fetch is refused. The change reaches
the Phase 2 solo result submission too: a solo worker submitting before
its binding is recorded is `transient`, the currency its own claim
already had. The intent source is `pending` only, the claim's own: a
`pane.open` recorded `reconciling` because its act returned an error
after the launcher had already claimed is not read, so that principal is
`stale` until label recovery commits the binding (LAUNCH-6, an accepted
residual; widening it widens the claim rule with it).

Every mutating verb here and in section 8 (`msg send`, `answer`,
`task create`, `task retry`, `plan close`; `review submit` already has
per-attempt digest idempotency) takes a caller-stable
`--request-id <uuid>`: the receipt row records it with a canonical digest
of the request's content, an identical retry (same ID, same digest)
returns the ORIGINAL outcome and entity (message ID, task ID and `t<seq>`,
attempt number) from the receipt — surviving consumption, relaunch,
retirement and manager succession, since receipts are permanent — and a
reused ID with different content is refused (`ErrRequestConflict`). The
request KEY is (run, verb, request ID) — messaging receipts and workflow
receipts each hold one authoritative accepted row per key (section 4).
The request DIGEST is canonical and fully specified: version tag
`hop-request-v1`, the Phase 2 length-prefixed field encoding
(`<decimal byte length>:<raw bytes>` per field, SHA-256, lowercase hex)
over, in order: the verb name; the run UUID; the sender's LOGICAL address
(`manager`/`task:<uuid>`/`human` — stable across incarnations and
manager succession, unlike a session ID); then the verb's normalized
payload — for sends: recipient address, kind, reply-to UUID or empty,
relay-of UUID or empty, and the BODY FILE'S BYTES digest (never a path,
so the same content from a different path is the same request); for
task-create: title bytes, the INSTRUCTIONS FILE'S BYTES digest, and the
dependency UUID list sorted; for retry: task UUID and reason bytes; for
plan-close: nothing further; for a human `answer` (`hop answer`, whose
sender's logical address is `human`): the QUESTION UUID then the answer
body's bytes digest — the question UUID is load-bearing, since two
questions can legitimately receive byte-identical answers and must not
collide as one request. At-least-once fetch covers a lost fetch
response; the request ID covers a lost MUTATION response, which
re-running would otherwise duplicate. The flag is optional at the CLI
(omitted → server-minted, retry then not idempotent) but the templates
and fixtures ALWAYS pass it; the duplicate lookup by request key runs
before any eligibility check (receipt-before-eligibility, as everywhere).
A session's `msg send` first establishes only who is asking — the
session's own run and its re-derived logical address, which must equal
the address the request digest covers (Send, below) — so a session that
claims another principal's address never reads a duplicate or
conflicting verdict about that principal's requests.

Run-state acceptance follows the Phase 2 result-submission precedent
([phase-2-design.md](phase-2-design.md) section 7, step 4: a submission
for a launching attempt whose claim has not settled is recorded as
`transient: attempt not yet running; retry`, exit 1, never as a final
refusal). The run-gated verbs are `task create`, `task retry` and
`plan close` (section 8) and an ordinary `msg send` (a question or an
info, relays included). Each checks the run inside its accepting
transaction, AFTER the caller's authority and current incarnation
(`Run.CanAcceptManagerVerb`):

- `running` with no stop request: eligible; the verb's own checks
  follow.
- `created`, `launching`, `resuming` or `completing` with no stop
  request: retryable. The first line is
  `transient: run not yet running; retry`, the exit is 1 and nothing
  changes. The receipt records outcome `transient`, outside the (run,
  verb, request ID) acceptance key, so the SAME request retried later is
  decided afresh and accepted at most once. Each of these states can
  still reach `running`:
  - the controller corroborates the manager's launch claim on its next
    scheduling pass, and a manager that plans immediately issues its
    first `task create` before that;
  - `hop resume` restores a resuming run whose manager kept running;
  - a completion whose final readiness re-validation fails returns the
    run to `running`.
  A retry loop terminates: a run that never gets there reaches a final
  state, and the next retry is refused.
- `completed`, `failed`, `stopping` or `stopped`, or ANY state once a stop
  is requested: `refused: run-not-accepting`, final. A stopping run that
  `hop resume` moves to `resuming` keeps its monotonic stop request.

Fetch (`msg next`, `msg wait`), `msg ack`, a session `answer`
(`msg send --kind answer`) and `hop answer` deliberately carry no
run-state gate in any state. They deliver, acknowledge or answer
messages that already exist rather than creating new work, so neither
the retryable nor the final line applies to them.

- **Send** (`hop msg send --to manager|task:<id> --kind question|info
  [--relay-of <id>] --file <path>`, or `--kind answer --reply-to <id>
  --file <path>` with NO `--to`): parse and bound. The sender's logical
  address is re-derived from its own session row inside the accepting
  transaction for every kind, after the session's own run and BEFORE the
  request-ID receipt lookup and the incarnation check; a sender whose row
  resolves to no address, or to a different one than the request claims
  (the address the request digest covers), is `refused: unauthorized`
  with a receipt whatever request ID and body it carries, and every
  decision below uses only the derived address. A session at its OWN
  address that reuses another principal's request ID is `refused:
  conflicting`: the request key is run-wide and the digest names the
  sender's address, so the digest never matches and the outcome is the
  same whatever the body — the caller learns only that the ID is taken,
  never whether its content matches. Kind/address legality
  by role: workers and reviewers → `question`/`info` to `manager` only;
  the manager → `question` to `human` or `question`/`info` to `task:<id>`;
  an `answer` is legal only from the session answering a question
  addressed to its own address — no `--to` legality applies, since the
  destination is derived. Recipient authority is checked in the accepting
  transaction: the answering session's logical address is re-derived from
  its own session row (never taken from the request) and must equal the
  question's recipient, else `refused: unauthorized` with a receipt. No
  session therefore ever answers a `human`-addressed question — only
  `hop answer` does, and a session's answer can never bundle a human
  question's acknowledgement — and no session answers a question
  addressed to another task or to the manager. The check follows the
  request-ID receipt lookup (a same-request-ID retry still resolves as
  duplicate or conflicting first) and precedes the prior-answer
  comparison below, so a non-recipient never reads a duplicate or
  conflicting verdict about someone else's answer — one claiming the
  recipient's address is refused before the receipt, above; a
  recipient's address is lineage-stable, so its own retries are decided
  exactly as before.
  `info` to `human` is refused at send — humans have no fetch or ack verb,
  so a human-addressed info could never settle (the only human-addressed
  kind is `question`, settled by its answer). An `answer`'s destination is
  never caller-chosen: it is DERIVED from the referenced question's
  sender — the question originator's logical address (`manager` for the
  manager session, `task:<its task>` for a worker/reviewer session) — and
  the accepting transaction validates same-run existence and agreement of
  the reply-to row, the derived recipient and any `--relay-of` reference.
  `--relay-of <q>` on a question records immutable relay provenance
  (`relayed_from`), linking the manager's human-facing question to the
  worker question it relays so the chain is reconstructible after any
  interruption. Duplicate answer (equal body digest for the same
  question) → idempotent success; conflicting answer → refused, receipt,
  the accepted answer undisturbed; eligibility (current incarnation; for
  a question or info, the run-state acceptance above — `transient` while
  the run can still reach `running`, `refused: run-not-accepting` (the
  section 5 run-level closure) once it never will; and for a
  `task:<id>` destination — the requested recipient of a question or
  info, the DERIVED destination of an answer — an OPEN mailbox: a closed
  one is `refused: mailbox-closed` with a receipt, per the section 5
  closure rule. An answer carries no run-state gate, but its admission
  check applies after the duplicate/conflict resolution, so an answer
  accepted before its destination closed still replays as duplicate
  while a first answer to a closed mailbox is refused and inserts
  nothing); accept: body artifact written durably BEFORE the
  row's transaction (temp-file-then-rename, digest recorded), then
  envelope row (enqueue sequence assigned here) + receipt in one
  transaction. Controller info notices follow the same file-first
  protocol: the body file is written before the transition transaction
  that commits their envelope — for the failure notice specifically,
  under the section 5 snapshot-equality contract.
- **Fetch** (`hop msg next`, and `hop msg wait [--timeout <dur>]`, default
  from `[messages] wait_timeout` (50s) — the same fetch in a bounded store
  poll loop, 1s interval, sleeping in the CLI process, not the store):
  resolve the caller's address (`manager` for the manager session;
  `task:<id>` for a worker or reviewer via its attempt's task;
  lineage-based, so a cold-relaunched or retried successor session fetches
  its predecessors' queue without any re-addressing write); after the
  caller's own run, its current incarnation and its address, require it to
  be that address's CURRENT session (section 5): a session that has not
  ended (lost or terminated) and, for a task address, belongs to the
  task's newest attempt, which is not terminal — the attempt numbering
  read inside the fetch transaction. Any other caller is refused
  (`ErrMessagingUnauthorized`: no protocol line, a stderr diagnostic,
  exit 1) with a refusal receipt whose detail names no value ("session is
  not its task's current attempt session", or "session is not the run's
  current manager session") and nothing served, so a retired attempt's
  session can never consume its successor's messages. In particular,
  once a session's attempt is TERMINAL its fetch (and its first ack, see
  Ack) is refused rather than answered `none:` — an accepted verdict
  completes the review attempt at once; an accepted result's attempt
  stays `submitted`/`checking` (fetch still answers, and finds nothing,
  since acceptance required a clear mailbox and closed it) until its
  per-task check settles it `completed` or `failed`; an interrupted or
  failed attempt is terminal immediately. The session has nothing left
  to consume there, and a terminal attempt's session must never take a
  message a retry's successor is meant to read; the recipient operating
  contract already ends the turn after an accepted submission, so a
  well-behaved agent never polls again; serve the
  in-flight delivered-unacknowledged message if one exists, else the
  lowest-enqueue-sequence queued message; write the delivery row and
  receipt in the same transaction that decides; print the envelope and
  body path. An empty fetch prints `none: …` and commits NOTHING — no
  receipt, no row (the poll loop must not grow the store). `wait`'s expiry
  line instructs re-invocation; the bound sits below the harness's own
  tool timeout (L1 pins the live constraint).
- **Ack** (`hop msg ack <message-id>`): already-acknowledged → duplicate,
  idempotent exit 0; a FIRST ack requires a delivery row recorded for the
  ACKING SESSION ITSELF (which also implies it postdates that session's
  start) with its current incarnation — not merely any delivery to the
  lineage, since a predecessor's delivery proves nothing about what the
  successor saw. A successor session therefore always fetches (and is
  re-served) before it can acknowledge; an ack of a message the acking
  session never fetched is refused (`ErrNotDelivered`, receipt recorded).
  A stale incarnation or a foreign session is refused with a receipt; so,
  after those checks, is a session that is no longer its address's
  current session (the Fetch rule; `refused: stale`, `ErrStaleAck`): the
  message it was served stays in flight and is re-served to the current
  session. The order is fixed — an already-acknowledged message is an
  idempotent duplicate first, then the delivery to this session (a
  retired session never served the message is `not-delivered`), then the
  incarnation, then address currency. The ack row commits with its
  receipt.
- **Answer** (`hop answer <question-id> --file <path> | --body "<text>"`):
  validates the question is `human`-addressed and unanswered; the answer
  body artifact is written durably BEFORE the transaction (the same
  file-first protocol as Send), then the answer message row (sender
  `human`, recipient `manager`), the question's acknowledgement and the
  receipt commit in one transaction. Duplicate (equal digest) idempotent;
  conflicting refused. The human is the answering address, and the
  derived destination is always `manager` — only the manager may address
  the human — so a human answer never enters a task mailbox: it is
  accepted even after the relayed worker's task mailbox has closed, and
  it is the manager's forward to that task that the closed mailbox
  refuses. The acceptance still runs the same admission check as a
  session answer, so the rule holds by construction rather than by the
  addressing matrix alone.

### Exactly what is journaled

For every message: the immutable envelope row (sender principal, recipient
address, kind, reply-to, relay provenance, enqueue sequence, request ID,
body path, digest, size, created-at); one delivery row per serve (session,
incarnation, time — re-serves included; a delivery row proves the serve
COMMITTED, not that the CLI's stdout ever reached the model, which is why
delivery is re-servable until acked); at most one ack row; and a receipt
row for every verb outcome including refusals and malformed input
(claimed ids as plain text, no FKs — the Phase 2 `result_submissions`
pattern), with ONE deliberate exception: an empty fetch commits nothing.
Controller info notices are
ordinary message rows committed inside the transitions they report, so the
journal of what the manager was told is exact and totally ordered with the
lifecycle evidence. Nothing about delivery lives only in memory or only in
a pane.

### The worker protocol grammar (one source of truth)

Phase 2's third escaped defect was a worker-facing protocol line the real
CLI never rendered. Phase 3 fixes the class: every worker-facing first
line is specified here as a grammar, implemented once, and pinned by
contract tests that run the REAL `hop` binary and by the fixture worker
parsing those same lines (section 11):

| Verb | First line on success | First line on retryable non-success |
| --- | --- | --- |
| `hop result submit` | `accepted <result-uuid>` / `duplicate <result-uuid>` | `transient: attempt not yet running; retry` (Phase 2, unchanged: the attempt's launch claim has not settled — checked before the mailbox); feature mode adds `transient: undelivered messages; drain with hop msg next, ack, then resubmit` (the section 5 mailbox rule) |
| `hop msg next` | `message <uuid> kind=<kind> from=<principal>[ reply-to=<uuid>][ relay-of=<uuid>][ origin=<uuid>]` then `body: <abs path>` then `ack: hop msg ack <uuid>` — `origin` appears on an `answer` whose reply-to question carries relay provenance: the store resolves reply-to → relayed_from server-side and prints the ORIGINAL question's ID, so a restarted manager forwards a human answer using only the envelope, no store spelunking | `none: no queued message` |
| `hop msg wait` | as `next` | `none: no message within <timeout>; run hop msg wait again` |
| `hop msg show <uuid>` | `message <uuid> kind=<kind> from=<principal> to=<address>[ reply-to=<uuid>][ relay-of=<uuid>] seq=<n>` then `body: <abs path>` then one `delivered: <session> <time>` line per delivery and `acknowledged: <time>` when acked — a READ-ONLY same-run envelope lookup (any of the run's sessions, or the human context), the historical recovery surface for relay chains and audits; it writes nothing, delivers nothing and never substitutes for `next` | `refused: not-found` |
| `hop msg ack` | `acknowledged <uuid>` / `duplicate <uuid>` | — |
| `hop msg send` | `sent <uuid>` / `duplicate <uuid>` | `transient: run not yet running; retry` (a question or info while the run can still reach `running`; run-state acceptance above) |
| `hop task create` | `task <uuid> t<seq> created` / `duplicate <uuid> t<seq>` (request-ID retry) | `transient: run not yet running; retry` (run-state acceptance above) |
| `hop task retry` | `retry accepted t<seq> attempt <n>` / `duplicate t<seq> attempt <n>` | `transient: run not yet running; retry` (run-state acceptance above) |
| `hop plan close` | `plan closed` / `duplicate plan closed` | `transient: run not yet running; retry` (run-state acceptance above) |
| `hop review submit` | `verdict accepted <review-uuid>` / `duplicate <review-uuid>` | the same two lines as `hop result submit`: `transient: attempt not yet running; retry` (the review attempt's launch claim has not settled — checked before the mailbox) or `transient: undelivered messages; drain with hop msg next, ack, then resubmit` (the section 5 mailbox rule) |

Refusals exit 1 with one first line `refused: <reason-token>` and detail
lines after; reason tokens are enumerated in the same grammar constant
set. `hop msg next` and `hop msg wait` have no refusal line: a fetch
refused for authority (another run's session, a stale incarnation, an
address the session does not resolve to, or a session that is not its
address's current session) prints nothing on stdout, one
`hop msg next: …` / `hop msg wait: …` diagnostic on stderr, and exits 1;
`refused: stale` from `hop msg ack` also covers an ack from a session
that is no longer its address's current session. A retryable first line (`transient: …`) also exits 1. The two submit
verbs each have two retryable lines that demand different actions (rerun
after a short delay, or drain first), so their store outcome carries a
TYPED transient reason — attempt-not-running or undelivered-messages, set
where the store decides — and the CLI prints exactly the line that reason
selects, never inferring it from detail text; a transient outcome naming
no known reason is an error and prints no protocol line. A worker that
could not tell the two apart would retry an undrained submission
forever. Both verbs decide in section 5's one acceptance order, so the
drain line reaches only a caller whose incarnation is current, whose run
has no stop request and whose attempt can still accept — exactly a
caller whose own `hop msg next` is served — while a caller that can
never be accepted gets the final `stale` outcome instead. It is the only
stdout line, any detail goes to stderr, nothing changed, and the caller
reruns the same command (same `--request-id`) after a short delay. The assignment and role templates quote these lines verbatim from the
same constants, so template, CLI and fixture can never drift apart
silently.

## 8. The built-in feature workflow: review, guards, serial integration

### Plan closure

Readiness needs a durable statement that the manager finished submitting
its plan — otherwise "every implement task integrated" is vacuously or
transiently true (an empty graph, or the gap between two `task create`
calls). The plan flag provides it: `hop plan close` sets it (refused for
a plan with zero implement tasks — `ErrEmptyPlan`; a zero-work feature
run is a usage error surfaced to the manager); any accepted `CreateTask`
clears it, reopening the plan (the reject-verdict fix-task path closes it
again afterwards). Review-task creation and `EvaluateReadiness` both
require the plan closed. Racing creation against completion is
impossible by construction: `CreateTask`/`RequestRetry`/`ClosePlan`
validate the run INSIDE their store transaction (section 7's run-state
acceptance: `ErrRunNotAccepting` with a receipt once the run can never
accept again — the defined fate of a late request — and a retryable
`transient` receipt while it can still reach `running`, `completing`
included), the run leaves `running` for `completing` only in the
transaction that first establishes readiness, and the final
`completing → completed` transaction RE-VALIDATES `EvaluateReadiness`
against the then-current task set and plan flag, so nothing created in
between can be silently skipped.

### Guards are application behavior, not prose

Run completion is a single controller transaction that calls
`EvaluateReadiness` over evidence rows only:

0. the plan is closed (above);
1. every implement task `integrated`;
2. a passing check receipt (the Phase 2 check pipeline's settled outcome)
   whose candidate is the CURRENT integration head (commit and tree object
   IDs recorded in the integration rows);
3. an `approve` verdict row whose subject commit and tree object IDs equal
   that same head, produced by a review-task attempt whose session role is
   `reviewer` — structurally distinct from every implementer session,
   since a session executes at most one attempt.

There is no verb that writes any of these except their own pipelines:
check receipts come only from `hop check-exec` executions through the
Phase 2 operation journal; verdict rows come only from
`SubmitReview` under the reviewer-session validation below; integration
states come only from the controller's journaled merge/publish/check/reset
operations. `hop task create`, `hop task retry`, `hop plan close`,
`hop msg send` and the read/ack side of messaging
(`hop msg next`/`wait`/`ack`) are the manager's complete verb set. A
manager that writes "all checks passed" in a message has changed nothing.

The combined check runs through a GENERALIZED check pipeline, not the
result-bound Phase 2 one unchanged: the landed `check_requests` table is
one row per accepted result, so a merge commit — which has no accepted
worker result — needs its own typed subject. Migration 003 rebuilds
`check_requests` with `subject_kind` (`result` | `integration`) and
exactly one subject reference (section 4); `ClaimAndRunCheck` resolves
its candidate object ID by subject (a result's commit, or an
integration's merge commit — for the no-op outcome, the unchanged head),
and its outcome transaction dispatches by subject AND workflow mode.
`Run.Complete` is reachable ONLY through `EvaluateReadiness` in feature
mode; the Phase 2 run consequences of a result check are solo-only. The
outcome tables are explicit:

| Subject / mode | Pass | Fail | Unknown (claimed group confirmed absent) | Stop observed in the outcome transaction |
| --- | --- | --- | --- | --- |
| result, solo | Phase 2 verbatim: attempt, task and run complete | Phase 2 verbatim: attempt, task and run fail | Phase 2 unknown-outcome rule verbatim (`check.repeatable` decides) | Phase 2 stop precedence verbatim (`interrupted`) |
| result, feature | attempt completed, task `completed` — the run stays `running`; integration is a separate later claim | attempt failed; task `needs-rework` with retries remaining, else `failed` (and the run fails per section 5) — never `Run.Fail` directly from a check | repeatable → the request requeues for a fresh execution from the same checking attempt (Phase 2 rule, task-scoped); unrepeatable → the attempt's outcome is terminally unknown: task `needs-rework`/`failed` by the retry budget, evidence retained, never completed on unknown | evidence recorded; attempt and task `interrupted`; the run is stop handling's |
| integration, feature | integration `checking → integrated`; dependents release in the same transaction | integration `check-failed` → the reset operation → `rolled-back`, task `needs-rework`/`failed` by budget | repeatable → a fresh execution against the same candidate head; unrepeatable → the integration settles `check-failed` with outcome unknown and the reset runs — an unvalidated candidate never stays published — then `needs-rework`/`failed` by budget with the unknown named in status | integration `rolled-back` via the stop path (section 5), task `interrupted` |

Exec,
capture, evidence retention, the frozen timeout, `check.repeatable` and
the unknown-outcome rule are shared verbatim across both subjects; the
uniqueness contract is one request per result and one per integration
(partial unique indexes). No synthetic result row is ever forged for a
combined candidate.

Per-task guards are Phase 2's, applied per task: acceptance requires the
result protocol; task completion requires the accepted result plus a
passing per-task check; nothing about Herdr idle/Done ever completes
anything.

### Serial integration and combined revalidation

Integrations are claimed one at a time (store-enforced partial unique
index, section 4), in dependency order then task `seq` order, each as the
journaled operation sequence of section 4: the scratch merge, the
compare-and-swap publish of the merge commit to `hop/r<seq>/integration`,
then the frozen check against the candidate through the generalized
pipeline above (its own detached checkout under the check operation's
directory — a scratch merge tree is never the check target). The merge is
spawned through the generalized exec boundary (`hop check-exec --op
<uuid> -- <merge argv>`) so it carries a durable pre-exec group claim,
and its argv is fixed and fully noninteractive:

```text
git -C <scratch tree> -c user.name=hop -c user.email=hop@invalid
    -c core.editor=true -c core.hooksPath=<hooks dir>
    merge --no-ff --no-edit <source-oid>
```

with `GIT_TERMINAL_PROMPT=0` and `GIT_EDITOR=true` in the sanitized spawn
environment, PLUS explicit suppression of inherited configuration —
`GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_SYSTEM=/dev/null`,
`-c commit.gpgsign=false -c merge.verifysignatures=false` — since editor
and prompt variables alone cannot neutralize a user's global hooks,
signing or merge drivers. Repository-LOCAL hooks are suppressed too
(human decision, 2026-09-15): `<hooks dir>` is an empty existing
directory under the operation's own artifact directory, created by the
controller before the spawn, so no repository-local hook runs during the
merge (G2 verified `core.hooksPath` to an empty or nonexistent directory
suppresses BOTH `pre-merge-commit` and `post-merge`, where `--no-verify`
skips only the former) — a failing local hook is therefore no longer a
merge input. Repository-local CONFIGURATION other than hooks remains in
effect, the merge is bounded by the frozen check timeout, and a non-zero
exit that is not a content conflict (a driver error, an unwritable
object database) is still CLASSIFIED with the conflict outcome —
`conflicted`, full output as evidence — because both mean "this
combination did not merge cleanly under repository policy" and both
route to the manager with the evidence. A later switch to HONORING
repository-local hooks is a configuration decision, not a design change
(the human has said "configurable later"). G2 exercises the matrix
including a signing-configured and a hook-configured repository, and G2
also fixes the rollback/fencing `commit-tree` inputs (the deterministic
identity/date/message of section 4). Ordinary Git
outcomes are all defined: a two-parent merge commit (the normal case); a
conflict (non-zero exit, `conflicted`, evidence retained); and the
ancestor/no-op case — a legitimate no-change task whose accepted commit
is already an ancestor of (or equal to) the head succeeds with "already
up to date" and creates nothing, which settles the integration through
the no-op row of section 5 (no publish; the check subject is the
unchanged head, satisfied by an existing settled receipt for exactly that
head or a fresh execution). Recovery never loops on a no-op: the
adoption predicate recognizes it explicitly (section 4). A failing
combined check triggers the journaled compare-and-swap reset (a fresh
rollback commit, section 4) and `needs-rework`; passing per-task checks
on separate branches never substitute — this is the exit criterion's
revalidation, mechanically.

### Review as a task

When every implement task is `integrated` and no review task for the
current head exists, the controller creates one (`kind=review`, subject =
the head's commit + tree object IDs, frozen into the task row and the
review assignment artifact) and assigns it into a concurrency slot like
any task. `SubmitReview`'s validation order: parse and bound (verdict
token, reasons ≤ 64 KiB, subject a full object-ID pair); existence and
agreement (attempt belongs to the review task, task to the run); prior
accepted verdict for the attempt — equal reasons digest and verdict →
duplicate, idempotent; different → conflicting, refused, accepted verdict
undisturbed; eligibility — caller session is the attempt's current session
with a current incarnation, session role `reviewer`, run not
stopping/stopped, attempt `running` (or `launching`/`relaunching` with a
settled claim — the Phase 2 early-submission rule), the review task's
mailbox clear (else the section 5 `transient: undelivered messages`
refusal), AND the submitted
subject equals the review task's frozen subject (`ErrVerdictSubjectMismatch`
→ refused: the reviewer reviewed the wrong candidate); accept atomically —
verdict row, receipt, `Attempt.Submit` then the review-only
`Attempt.CompleteReview` transition (section 5: review attempts never
enter `checking`) and `Task→completed`, with their evidence rows, and the
controller info message to the manager.

A `reject` verdict completes the review task but fails the readiness
guard; the shortfall (with the reasons artifact path) is what the manager
acts on — a fix task, whose integration produces a new head, which gets
its own new review task. An `approve` bound to a superseded head is
harmless by construction: the guard compares object IDs, so staleness is
computed, never stored (the gate-evaluation immutability principle of
[domain-model.md](../architecture/domain-model.md)).

Since the accepting transaction's controller notice names no verdict (its
body is the reasons artifact, read separately through the notice's own
`body:` path), `hop status -run`'s `verdict-rejected` shortfall line —
named in the manager's own standing instruction — is the only channel
that tells the manager a verdict was a reject rather than an approve; it
names the review's own id, subject and reasons path (never just "the
latest review") precisely so the manager can match it against a
specific fetched notice and never plan a second fix for a rejection it
already addressed or a fix has since superseded.

### Reviewer independence

Structural: the reviewer session is a distinct session (its own native
reference, its own worktree at the subject head) that has executed no
implement attempt, launched with the frozen reviewer role artifact. Same
harness by default (`[roles.reviewer] harness`, open question 4);
independence in Phase 3 means a separate session and role, not a separate
provider.

### Completion retirement

Workers and reviewers are already retired at their per-attempt boundaries
(section 6), so completion retirement is normally just the manager: after
`EvaluateReadiness` passes (entering `completing`) the controller retires
the manager and any straggling session under the Phase 2 close rule with
scrollback captured first, re-validates readiness inside the final
transaction (plan closure section above), and only then records
`completed`. Completion preserves the worktrees, the `hop/r<seq>/*`
branches and every artifact; the worktrees are removed only later, by
the lazy post-merge retirement of section 6 (once the run's integration
branch has merged into the target branch), while branches and artifacts
are always kept. Stop precedence still applies: a stop request observed in
any of these transactions yields `interrupted`/`stopping`, never
`completed`. `hop review submit`'s CLI takes the subject COMMIT only; the
application resolves its tree object ID against the recorded repository
before the accepting transaction, so the stored verdict always carries
both IDs the guard compares.

## 9. Controller ownership and the native Agent view

### Ownership: the Phase 2 lease, extended not replaced

One controller per run, the same `run_leases` row, CAS heartbeat/release,
fencing generations, `InitializeRun` creating the first lease. Phase 3
adds nothing to the lease protocol; it adds the manager session UNDER it:
the manager session row is created inside `InitializeRun`'s transaction
(feature mode), and the partial unique index (one non-terminal manager
session per run) makes a second manager unrepresentable regardless of
controller behavior. `hop resume` reconciles the manager exactly as it
reconciles workers (same predicate, same restore policy, the
per-session `--confirm-absent <session-id>` contract of section 5); a
takeover therefore recovers ownership of
the manager conversation (warm reattach) or cold-relaunches it
(`claude --resume <manager-native-ref>`) without ever minting a second
manager lineage. A second concurrent `hop resume` loses the lease CAS and
exits reporting the holder — the exit-criterion scenario.

Detach (SIGINT/SIGTERM) remains distinct from stop and leaves the manager
and workers running; `hop stop` remains the only stop, and now drives
termination of every owned session (manager, workers, reviewer) and every
check/merge group under the unchanged Phase 2 rules.

### Native Agent metadata and view, integrated into run state

The Phase 1/2 presentation machinery becomes live run-state publication
(the [herdr-surface.md](../architecture/herdr-surface.md) token contract):

- Tokens per session, published by the controller on every transition that
  changes them and rehydrated on resume (token metadata is not
  cold-restored): `hop_run` (`r<seq>`), `hop_run_order` (padded),
  `hop_order` (manager `000000`; children `000010 + 10×task seq`; reviewer
  after implementers), `hop_role`, `hop_task` (title, truncated to the
  80-char token limit), `hop_parent` (`manager-r<seq>`, absent on the
  manager), `hop_state` (the HOP condition label: `planning`, `working`,
  `awaiting checks`, `integrating`, `review`, `needs attention`, …).
  Publication uses the Phase 1 `AgentPresentation` port and its
  full-replacement semantics (unset optionals cleared), keyed to the
  session's current binding; a superseded binding's tokens are cleared or
  rebound when the occupant changes, per the surface audit. A pane can
  vanish at any moment (its process exits, or a human closes it), so a
  publication the server answers with `pane_not_found` skips that pane
  for the round and records nothing: the session's fate stays with launch
  corroboration, retirement and resume, under their own evidence rules.
  Every other publication error still ends the pass. The close rule
  treats a `pane.close` answered `pane_not_found` the same way: nothing
  was closed, and the immediate absence re-observation decides.
- View selection is a deliberate user command, never a controller side
  effect: `hop view set --run <id|r<seq>>` installs the run-scoped filter
  and manager-first sort under source `plugin:hop`; `hop view clear`
  clears HOP's own view only. One projection exists server-wide; HOP does
  not stack or restore other owners' views and does not auto-reapply on
  startup in Phase 3 (a startup-hook reapply is deferred until metadata
  rehydration is proven in this phase's suite).

## 10. CLI surface

Existing commands keep their Phase 2 contracts (`run`, `status`, `stop`,
`resume`, `result submit`, `launch`, `check-exec`, `version`, `doctor`,
`plugin-context`); extensions and additions:

| Command | Flags | Behavior and output |
| --- | --- | --- |
| `hop run "<brief>"` | Phase 2 flags plus `--workflow solo\|feature` (overrides `[workflow] mode`; default remains solo — open question 1) | Feature mode: freezes the workflow snapshot and role artifacts, creates the integration branch (create-only CAS; scratch checkouts are per-operation, section 4), launches the manager, runs the extended loop. Solo mode: Phase 2 verbatim |
| `hop status` | unchanged flags | Detail block gains: task table (seq, kind, state, deps, attempt count, worktree), integration head and per-integration states, message queue depth and in-flight age per address, pending human questions (`question <uuid> …` with the body path and the `hop answer` invocation), guard shortfalls verbatim from `EvaluateReadiness`, per-session roles/bindings |
| `hop stop <run-id>` | unchanged | Now retires manager + workers + reviewer + check and merge groups; same observed-termination contract |
| `hop resume <run-id>` | unchanged, except `--confirm-absent <session-id>` takes a required session argument in feature mode | Reconciles every session under the one predicate; each attestation is journaled per session with its own continuity evidence |
| `hop task create` | `--title`, `--file <instructions>`, `--depends-on <task>` (repeatable), `--request-id <uuid>`; identities from the manager's `HOP_*` env | Manager-only (validated); prints `task <uuid> t<seq> created`; refuses cycles, cross-run deps, non-manager callers and `kind=review`. Run-state acceptance (section 7, after the Phase 2 `hop result submit` precedent): while the run can still reach `running` (created, launching, resuming, completing) it prints `transient: run not yet running; retry` and exits 1, and the manager reruns it with the same `--request-id`; once the run never can (completed, failed, stopping, stopped, or a stop requested) it is `refused: run-not-accepting` |
| `hop task retry <task>` | `--reason <text>`, `--request-id <uuid>` | Manager-only; legal only for a terminal attempt of a `needs-rework` task under the retry limit; prints `retry accepted t<seq> attempt <n>`; the same run-state acceptance as `hop task create` |
| `hop plan close` | `--request-id <uuid>` | Manager-only; refuses an empty plan; prints `plan closed` (section 8); the same run-state acceptance as `hop task create` |
| `hop msg send` | `--to`, `--kind`, `--reply-to` (answer only, `--to` forbidden — the destination is derived), `--relay-of` (relayed questions), `--request-id <uuid>`, `--file` (or `--body` ≤ 4 KiB) | Section 7 validation; worker/manager contexts; a question or info has the same run-state acceptance as `hop task create`, an answer none |
| `hop msg next` / `hop msg wait` | `wait`: `--timeout` (default `[messages] wait_timeout`, 50s) | Section 7 grammar; `next` never blocks; an empty fetch writes nothing; no run-state gate |
| `hop msg ack <message-id>` | — | Section 7 ack rules (delivery to the acking session itself required); no run-state gate |
| `hop msg show <message-id>` | — | Read-only same-run envelope lookup: kind, principals, reply-to/relay-of, sequence, body path, delivery and ack history (section 7 grammar); the relay-recovery and audit surface; writes nothing |
| `hop answer <question-id>` | `--file` or `--body`, `--request-id <uuid>` | Human command (controller-machine state root); acks the question atomically with the accepted answer; no run-state gate |
| `hop review submit` | `--verdict approve\|reject`, `--subject <commit-oid>`, `--reasons-file <path>` | Reviewer-only; section 8 validation; `verdict accepted <uuid>` |
| `hop view set` / `hop view clear` | `set`: `--run <id\|r<seq>>` | Owned native-view selection/clear (section 9); exit 0 on server acknowledgement |

Worker plumbing (`launch`, `check-exec`, `result submit`, `msg *`,
`review submit`) stays under its usage heading. `hop launch` gains
`--session` (section 6) while keeping `--run --attempt` exactly as Phase 2
rendered it — solo-mode panes continue to carry the Phase 2 argv verbatim,
and the two forms are mutually exclusive. Exit codes keep the Phase 2
discipline; message and review refusals exit 1 with the `refused:`
grammar, a retryable `transient:` first line also exits 1, and usage
errors exit 2.

Once the worktree-retirement slice (section 12) lands, `hop status` also
runs the lazy post-merge retirement pass (open question 6) before it
renders: the detail block below is a separate, later concern from that
pass, and neither reads the other's output.

The feature-mode detail block's own fixed line text (section 10's `hop
status` row) is specified as its own small grammar, in
`internal/app/grammar.go` alongside the verb table above, since the
fixture manager and the deterministic scenarios (section 11) parse it
exactly as they parse the verb grammar. Every path and Herdr binding
identifier in the table below, and every such value in the detail's
common header — the binding, the solo worktree, the target branch (also
inside the `worktrees:` sentence), artifact and check-evidence paths, the
trust-seed evidence and the last check's detail, both of which embed
them — plus the git object ids (`cmd/hop`'s `safeRenderExternal`, applied
consistently to worktree-retirement's own report and per-row lines too)
renders raw only when it is valid UTF-8 with no C0/DEL/C1 control byte,
no double quote and no backslash; otherwise it renders as Go's
`strconv.Quote` form, which always starts with a double quote a raw
rendering never can — these are operator-, principal- or git-sourced
strings (a checkout location, a workspace/tab/pane id, a ref name, a
process's error output), never HOP-generated, and none of the stores or
resolvers on their path reject control bytes:

| Line | Shape |
| --- | --- |
| Task table row | `task t<seq> <task-uuid>: kind=<kind> state=<state> deps=<t<seq> labels, or (none)> attempts=<n> worktree=<path, or (none)>` |
| Latest integration | `integration <id>: task=t<seq> state=<state> source=<oid, or (none)> premerge=<oid, or (none)> merge=<oid, or (none)>` |
| Guard shortfall | `shortfall: <kind>` (`plan-open`, `check-missing`, `verdict-missing`, `verdict-stale-subject`), `shortfall: <kind> t<seq> <task-uuid>` (`task-not-integrated`), or `shortfall: verdict-rejected review=<review-uuid> subject=<oid> reasons=<path>` — the kind token exactly as `run.ShortfallKind` defines it. `verdict-rejected` names the SPECIFIC review it reports (Astra F3): `EvaluateReadiness` checks subject currency before the verdict value, so a review whose subject no longer equals the head is `verdict-stale-subject` whatever its verdict, and `verdict-rejected` fires only for a reject of the CURRENT head — carrying that review's id, subject commit and reasons path (`reviewReasonsPath`, the exact path its accepting transaction's controller notice used as its own body) so a manager can tell which review is being reported, without guessing from "the latest one". The status read's head is the newest integrated row's merge commit, and its tree is the one HOP recorded for exactly that commit (a review task's frozen subject or an accepted review's subject, each resolved from git), never the commit id; with no such record the tree stays unknown, which can report a review only as stale (its commit then differs from the head) and a check only as missing, never as current. When the recorded trees for the head commit disagree, the check and verdict shortfalls are not reported and one value-free `shortfall: evidence-inconsistent` takes their place — never a rejection, as the manager's standing instruction says — while the plan and task shortfalls and the rest of the block render as usual |
| Attention (section 7, verbatim) | `attention: messages pending for <address>: in-flight <age> (message <uuid>), queued <n>, oldest <age>`, either clause optional but never both absent; `<address>` is `manager`, `human`, or `task:<uuid> (t<seq>)` |
| Attention action (only when the mailbox's Attention condition holds) | `open <workspace>/<tab>/<pane> and check that the agent is following its polling instructions` (a manager or task address, its live session's binding named when known, else "that session's pane"), or `answer pending human questions with hop answer` (the human address) |
| Listing/state-line marker | `blocked, needs attention`, appended alongside `stop requested`/`reconciling` whenever any mailbox is in the Attention condition |
| Pending question | `question <uuid> age=<age> body: <path>` then `hop answer <uuid> --file <path>` (`<path>` a literal placeholder: the answer file does not exist yet) |
| Per-session row | `session <uuid>: role=<role> state=<state> task=<t<seq>, or (none)> attempt=<n> binding=<workspace/tab/pane, or (none)>` |

## 11. Test plan

Integration-first, per the Phase 2 lesson: every new external surface gets
a capability probe pinning the exact observed values under the production
transport BEFORE any decision rule depends on it; app-layer fakes
reproduce the pinned shapes and enforce the real adapters' argument
contracts; the exit scenarios are real-process.

### Spike probes (task 0, `test/integration`, continuing the S-series)

| Probe | Established (executed) | Pins |
| --- | --- | --- |
| S8 `workspace.create` (herdr) | Response shape `{type: "workspace_created", workspace, tab, root_pane}`. The creation `label` is a WORKSPACE attribute (`WorkspaceInfo.label`), never the root pane; the recovery descent (workspace → its sole tab → that tab's sole pane) round-trips through `session.snapshot`, recorded per-run. cwd/env additive delivery to the root pane's login shell confirmed — cwd independently via the LIVE `pane.process_info` (a printed-`$PWD` check line-wrapped at the pane's 100-column width and was abandoned as unreliable). `focus:false` neither reports the workspace focused nor changes the session's active workspace, except the session's very first-ever workspace, which always activates regardless of the request | The `WorkspaceRequest`/`WorkspaceHandle` shapes and the workspace.create decision-table row's recovery rule |
| S9 concurrent worktrees (herdr) | Three CONCURRENT `worktree.create` calls (the full family: t1a1, t2a1 new branches, t3a1 the review task's branch, pre-existing) driven on the RAW request with the `label` field included (the landed Go adapter still sends cwd/branch/base only today — see slice 5). Response shape `{type: "worktree_created", workspace, tab, root_pane, worktree}`. `--base` honored for each new branch (worktree HEAD == the given base) and SILENTLY IGNORED for the pre-existing branch (worktree HEAD == its own prior tip) — confirming refuse-if-exists is load-bearing, not merely defensive. Label round-trip per call. `WorkspaceInfo.worktree.repo_key` matches across every worktree workspace AND the manager's own plain `workspace.create`d parent workspace (Herdr resolves and marks it as the non-linked source on the first `worktree.create` call). Each worktree workspace coexists with an S6 command-pane launch at its own checkout path | Per-attempt worktree creation, the refuse-if-exists + verify-HEAD-after-create rule of section 6, label recovery, and reviewer worktrees |
| S10 multi-pane env isolation (herdr) | Three command panes created CONCURRENTLY (manager-shaped, worker-shaped ×2) each observe exactly their own `HOP_SESSION_ID`/`HOP_ROLE`, checked pairwise against the other two — zero cross-contamination. Creation labels resolve to the correct pane per call. `agent.list` returns exactly three records, each independently keyed to its own pane | That concurrent launches cannot cross-contaminate identity — the multi-session corroboration and close rules rest on it. S10 deliberately does NOT claim to validate the session-keyed claim queries (that is the sqlite raced suite's job) |
| G1 ref namespace and branch family (local git, no herdr) | The complete `hop/r1/integration` + `hop/r1/t1a1`/`t2a1`/`t3a1` family coexists (`git for-each-ref` lists all four). Create-only `update-ref <ref> <new> ""` succeeds only when `<ref>` does not yet exist and is refused (ref unchanged) once it does. NEGATIVE CONTROL, the exact reason the `/integration` suffix is required: a bare `hop/r1` branch collides with any `hop/r1/t<t>a<n>` sibling — `fatal: update_ref failed for ref 'refs/heads/hop/r1/t1a1': cannot lock ref 'refs/heads/hop/r1/t1a1': 'refs/heads/hop/r1' exists; cannot create 'refs/heads/hop/r1/t1a1'` (git's ref storage cannot treat one path as both a file and a directory) | B3's scheme against real Git ref-store behavior |
| G2 merge outcome matrix (local git, no herdr) | Under the frozen argv `git -c user.name=... -c user.email=... -c commit.gpgsign=false -c merge.verifysignatures=false merge --no-ff --no-edit <source>` (env: `GIT_CONFIG_GLOBAL`/`GIT_CONFIG_SYSTEM=/dev/null`, `GIT_TERMINAL_PROMPT=0`), in a DETACHED scratch checkout throughout (the ref stays untouched in every case): two-parent merge (parents == {base, source}); conflict (non-zero exit, `UU` index entries, clean `merge --abort`); already-up-to-date no-op (nothing created, HEAD unchanged); `--no-ff` forces a genuine merge commit on an otherwise fast-forwardable head; `git commit-tree` with pinned tree/parent/identity/dates/message is byte-stable across two invocations (the G3 rollback's precondition). A repository with `commit.gpgsign=true` set LOCALLY does not block the merge — the frozen argv's own `-c commit.gpgsign=false` wins over every config file, local included (testing GLOBAL config is moot: the frozen env already redirects it to `/dev/null`). A non-conflict failure (an unwritable shared object database) is distinguished from a conflict by OUTPUT TEXT and INDEX STATE, never by abortability — `merge --abort` still succeeds after this failure too. HOOKS FIRE under a hooksPath-less argv (no `--no-verify`); suppression options verified: `--no-verify` skips only `pre-merge-commit`; `-c core.hooksPath=<dir>` (an empty existing directory, or one that does not exist at all — both behave identically) suppresses BOTH `pre-merge-commit` and `post-merge`. Adopted (human decision, 2026-09-15): the frozen argv carries `-c core.hooksPath=<empty existing dir under the operation's artifact dir>` (section 8); honoring local hooks again would be a configuration decision, deferred | The `integration.merge` adoption predicate including the no-op row, the noninteractive argv/env with configuration suppression, and the failure classification |
| G3 fenced publish/reset (local git, no herdr) | `update-ref <ref> <new> <old>` succeeds and moves the ref only when `<old>` matches its actual current value; a stale `<old>` is refused, ref left exactly where it was. `""` as `<old>` is create-only (see G1). The rollback construction — `git commit-tree <pre-merge-tree> -p <rejected-merge> -m <message>`, CAS-published in turn — keeps the rejected merge REACHABLE (`git merge-base --is-ancestor <rejected> <ref>` holds; `git log <ref>` lists it) rather than orphaning it via a destructive reset+force | The `integration.publish`/`integration.reset` CAS rows and the never-revisit rule's mechanics |
| S11 request-log observability (herdr) | Location: `<the server's own socket directory>/herdr-server.log`. Format: plain text (`tracing_subscriber`'s default formatter, no JSON/ANSI), one line per event, e.g. `... DEBUG herdr::logging: api request received event="api.request.start" ... request_id="hop-1" method="ping" changes_ui=false`. LEVEL splits exactly by method: `pane.send_text`/`pane.send_keys`/`pane.send_input` log their start/complete pair at DEBUG ONLY when they succeed (invisible at the default `herdr=info` filter) — a FAILED or erroring call among these still completes via the SAME `event="api.request.complete"` record, never a distinct fail event, but at INFO, since the server forces INFO whenever `outcome != "ok"`; `agent.prompt`/`agent.send_keys`/`agent.start` log both their start AND completion records at INFO regardless of outcome — confirmed for all six methods on a SEPARATE client connection, none needing to succeed or need a live/matching agent to reach the log. `event="api.request.fail"` (WARN) is a SEPARATE event reserved for a failure WRITING the response back to the client socket itself (e.g. a disconnect mid-write) — it never fires for an ordinary business-logic refusal, and no Phase 3 scenario relies on it. Every line carries `method` and `request_id` but NEVER the pane/agent target or any request parameter (confirmed: neither a sent marker string nor a target pane id ever appears) — the injection-free claim is therefore honestly method-level, never per-pane. `request_id` is a CLIENT-chosen label the server only echoes, NOT a server-assigned global sequence: two independent client connections to the same server both start counting from `"hop-1"`, and that exact value was observed logged against two unrelated methods in one capture — correlation must locate a call's own line by something unique to it (method name, confirmed) and read the id back from there. A capture window is bracketed by two otherwise-unused methods' own log lines (`pane.list`/`tab.list` in the probe) — never by a chosen label, which the log never carries at all | The `InjectionFreeDelivery` scenario's evidence channel: what a forbidden request WOULD look like in the log, so a zero count over the whole capture is meaningful |

Deliberately not probed for BUSINESS BEHAVIOR, since no Phase 3 decision
rule consumes them (section 7) and probing their actual EFFECT would imply
a channel this design forbids: `agent.prompt`, `agent.wait`,
`agent.send_keys`, `pane.send_keys`, `pane.send_input`, `agent.start`. S11
DOES drive each of these once, cheaply, except `agent.wait` — but only to
establish REQUEST-LOG visibility (level, format, correlation), never any
effect on a pane; several were refused at the business layer (an
unrecognized agent kind, no live/matching agent) before any terminal
interaction could occur, and that refusal still reached the log. L1
(opt-in, live): a real Claude session executing `hop msg wait --timeout
50s` inside its shell tool, pinning that the bounded wait returns within
the harness's tool timeout and the model can loop on the `none:` line;
informs the default only, never CI.

### Countermeasures for the Phase 2 escaped-defect class

Three Phase 2 production defects escaped review because each layer's unit
tests were self-consistent but wrong about a real dependency. Each gets a
structural countermeasure, not just a test:

| Escaped-defect class | Phase 3 countermeasure |
| --- | --- |
| A fake accepted arguments the real adapter refuses (bare `git` argv vs the runner's absolute-path contract) | Every new fake mirrors the real adapter's argument validation, extending `TestFakeStoreContracts`/`TestFakePortsRefuseCallsInsideTransactions`: the fake store enforces the message/review/task contracts (serialization, one answer per question, delivery-before-ack, manager-only verbs, request-ID idempotency, serial-integration index) with a contract test proving fake and real store refuse the SAME inputs. The shared refused-input vectors live in a dedicated test-support package, `internal/testsupport/storevectors` — exported Go declarations importable by BOTH packages' `_test.go` files (cross-package tests cannot import another package's `_test.go` declarations, and shipping vectors as `internal/app` production code would break test-support isolation); the checker gains a `test-support` rule row for it whose production-closure check proves only test files import it |
| A protocol field's real shape differed from the assumed one (argv0 as basename) | No new identity or matching rule is introduced anywhere in Phase 3 — every occupant decision reuses `CorroborateSettlement` verbatim; the only new wire shapes (`workspace.create`, second-worktree responses) are S8/S9-pinned before the port freeze, and the herdr adapter's `assertRequestParams` full-structural fixtures cover the new methods |
| A worker-facing protocol line was never rendered by the real binary | The section 7 grammar is one constant set; `cmd/hop` contract tests execute the REAL built binary for every verb and assert each first line against the constants; the fixture worker/manager/reviewer parse ONLY those constants; a golden-grammar test fails if a constant, a rendered line or a template quotation drifts |

### Layers

| Layer | Tests |
| --- | --- |
| Domain (`internal/domain/run`, `identity`) | Independent transcription tables for the extended Task machines (both kinds), Message delivery, Integration; multi-attempt density and `ErrRetryNotTerminal`/`ErrRetryLimit`; dependency acyclicity and `ReleaseEligible`; delegation-depth and manager-uniqueness rules; `AcceptVerdict`, `AcceptAck`, `NextDeliverable` orders (receipt-before-eligibility, duplicate/conflicting/stale vectors); `EvaluateReadiness` shortfall vectors (missing integration, stale-subject approve, stale-subject reject — subject currency checked before the verdict value, Astra F3 — reject present at the current head carrying its review's id and subject, head moved); the six section 5 reference traces as complete multi-entity traces; new ID parsing with fuzz |
| Application (`internal/app`) | Extended fakes with the shared refused-input vectors; scheduler scenarios: slot bound enforced inside the assignment transaction, release only on `integrated`, deterministic pass order, per-attempt session retirement freeing slots only on observed absence, retry consumption and exhaustion (direct-to-failed), plan closure (empty-plan refusal, reopen-on-create, late create/retry refused by run state, readiness re-validated in the final transaction), review-task creation exactly once per head; integration decision-table rows (all crash columns for merge/publish/reset, the no-op adoption, conflict evidence, stop mid-merge, stop completing a pending rollback) and the two INTEGRATION BARRIERS: (a) controller A resumes after B has taken over, rolled back and advanced the branch — A's zombie publish/reset CAS fails and its scratch group is retired by claim; (b) the unmoved-head window — stop (or takeover) with A's publish intent unresolved and the head unchanged: the ref-fencing rollback lands first and A's late CAS fails, and in the reverse interleaving A's landed publish is rolled back by the stop path and its outcome commit is fenced — the ref never rests on an unvalidated candidate; guard enforcement (no verb reaches the guard rows; a scripted "manager" calling every verb cannot complete a run without real receipts); messaging scenarios (serialization per enqueue sequence, re-serve, ack refused for a queued message AND for one delivered only to a predecessor session, stale/duplicate ack, derived answer destination incl. a wrong-task answer refused, relay-chain recovery driven ONLY through CLI-returned data — the `origin` envelope field and `hop msg show` — across a manager restart, answer-acks-question atomicity, forward-before-ack redo idempotency, lineage fetch after relaunch and retry, request-ID idempotency for every mutating verb incl. an accepted-write/lost-response retry — distinct from fetch-before-ack; mailbox closure: acceptance refused while a message is pending, send refused after closure, reopen on acceptance-closure retry but never after failure-closure, exhaustion-without-result closing the mailbox with the orphaned obligations journaled, raced in both orders and on both closure causes); the D1 retirement-kind barriers (pending publish fenced with the current tree; pending reset COMPLETED — persisted R adopted, or the validated pre-merge tree used with the observed head as parent — never fenced over; a crash after a fence CAS before its outcome recovered through integration.fence's own row) and the D2 materialization barrier (preparer paused after HEAD is visible but before checkout completes: the successor abandons the directory and allocates a fresh operation, never merging in a still-mutable tree); the no-injection invariant checked at its section 7 scope (port-surface allowlist over every port, the type-aware forbidden-call rule's fixtures, zero typed-input calls in every scenario); presentation republication and rehydration; manager lineage (historical parents preserved, successor validation, stale-manager verb refusal); six reference traces at app level |
| SQLite (`internal/adapters/sqlite`) | Migration 003 applied over a POPULATED 001(+002) store — every rebuild preserves rows byte-for-byte where unchanged: bindings and claims intact with backfilled session IDs, solo sessions/roles intact, task seq/kind defaults, check requests re-keyed with `subject_kind='result'`, foreign_key_check clean — and from empty; the migrator's rebuild support (foreign_keys off + check before commit); the shared refused-input vectors against the real store; raced contracts across separate handles: fetch/fetch (exactly ONE serialized in-flight message; a delivery row per successful serve, so two rows when both fetches served it — the at-least-once contract stated unambiguously), ack/ack (one ack), answer/answer (one accepted), request-ID reuse raced (one created entity, the loser reading the winner's receipt), CreateTask cycle check under concurrent edge inserts, serial-integration index raced, manager-uniqueness index raced, retry-request unique-pending raced, two concurrent pending launch intents validating independently under the session-keyed lookup (B2); enqueue-sequence FIFO under interleaved writers (an inverted caller timestamp cannot jump the queue); lineage fetch queries; workflow- and message-receipt key uniqueness on the shared (run, verb, request-ID) acceptance key with duplicate-returns-original vectors, including the SAME request ID accepted independently across two runs and across two verbs, and a reused ID with a different question or body refused; the human-answer digest vectors (question UUID load-bearing: identical bodies to two questions are two requests); the plan flag surviving reopen and takeover; the mailbox send/accept AND send/failure-settlement races from separate handles in both orders, including a send committed AFTER the failure notice's snapshot preparation but BEFORE the first settlement transaction — the first settlement must retry on the snapshot mismatch and the eventual committed notice must include that message ID; the populated-003 fixtures of slice 3 (active, completed, failed and claim-without-binding 001(+002) stores, ambiguous-backfill refusal); receipts for every refusal path and none for empty fetches; `RunDetail` extensions |
| Herdr adapter | `CreateWorkspace` + `FindWorkspaceByLabel` protocol tests (S8 shapes: the label as a workspace attribute, the sole-tab/sole-pane descent, ambiguity errors, full-structural request fixtures, partial-response tables, cancellation); the `SendText` port removal (the adapter type's method set pinned — the transport allowlist of section 7) |
| Process/config/system | TOML tables for the new policy keys (defaults, feature-mode requireds, unknown keys) in `internal/adapters/config` (owned by slice 4); no new process-adapter behavior (merges ride `hop check-exec` + the existing `CommandRunner`) |
| internal (checker) | The type-aware forbidden-call rule with its negative fixtures (chained selector `c.Runtime.SendText`, interface/type aliases, method values, a raw adapter call in composition) and positive fixtures; the `test-support` rule row for `internal/testsupport/storevectors` |
| cmd/hop | New verb tables (dispatch, flags, exit codes, worker/human state-root rules, `--request-id` pass-through, answer's forbidden `--to`, `--confirm-absent <session>`); the real-binary grammar contract tests (above); loop scheduling-pass ordering with the fake clock; `hop view` commands |
| Real process (`test/integration`) | The exit scenarios below, plus: `TestRealProcessIntegrationConflict` (two tasks touching one file; conflict → needs-rework → retry from the new base → completes), `TestRealProcessCombinedCheckFailure` (per-task checks pass, combined check fails, branch reset verified by object ID and by the persisted rollback-commit OID, rework), `TestRealProcessStopDuringFeatureRun` (stop mid-integration: merge group retired, unresolved ref intents fenced, every session terminated, `stopped` only on observed absence), `TestRealProcessManagerColdRelaunch` (manager killed; resume cold-relaunches the manager lineage via `--resume`, task state undisturbed), `TestRealProcessMailboxClosureRace` (a manager send racing a worker's submission: whichever order the store commits, either the submission is refused transient and the worker drains, or the send is refused mailbox-closed — no stranded message, receipts both ways; plus the failure-path variant — a fixture worker exhausts its retries WITHOUT ever submitting an accepted result, the failing settlement closes the mailbox and journals the orphaned message IDs in the manager's notice, and a subsequent send is refused) |
| Live (opt-in) | `TestLiveClaudeFeatureRun`, gated on `HOP_LIVE_HARNESS=1`: real Claude as manager, one worker task and review on a fixture repository — plus L1. Never in CI or `make check`; the deterministic fixture suite is the gate |

### Real-process exit scenarios (fixture harness)

The fixture worker generalizes into a fixture principal: one deterministic
Go program installed as `claude`, dispatching on `FIXTURE-BEHAVIOR:`
directives delivered through its assignment artifact — new behaviors
`manager-feature` (create scripted tasks with a dependency, close the
plan, poll `hop msg wait`, answer questions from a scripted table, handle
a human answer through the relay chain, retry on interruption notices,
relay one question to the human when scripted), `worker-implement`,
`worker-hold` (implements, then blocks at a message barrier — a question
whose answer the test or fixture manager releases deliberately — before
submitting, so slot saturation is observable), `worker-question` (asks,
waits, applies the answer's file content, submits), `worker-fetch-crash`
(fetches then exits before ack), `reviewer-approve`,
`reviewer-reject-once`. Every behavior keeps the Phase 2 contract: dump
observations to files, never judge itself, retry on the `transient` line,
parse only the section 7 grammar — and every behavior IDLES at a
composer-like loop after submitting rather than exiting, so the suite
proves per-attempt retirement (section 6) actually closes interactive
survivors instead of relying on process exit.

Scenarios (all `TestRealProcess*`, disposable server, private roots — the
user's live server at `~/.config/herdr/herdr.sock` is never touched):

1. `FeatureRunEndToEnd` — the section 1 evidence row: four tasks — t1,
   t3, t4 initially eligible, t2 depending on t1 — with `MaxWorkers=2`
   and `worker-hold` barriers keeping t1's and t3's sessions alive at
   will. Asserted separately: t1 and t3 launch immediately (the intended
   two-worker concurrency); t4, eligible throughout, launches only after
   a barrier is released and that session's retirement is OBSERVED (the
   slot assertion, decoupled from process-exit assumptions); t2 launches
   only after t1 is `integrated` (the dependency assertion); serial
   integration order; plan closed before the review task appears; review;
   completion with the manager retired; every guard row present in the
   store; token metadata published manager-first (asserted via
   `agent.list` as in Phase 1).
2. `RelayedQuestion` — trace 2 end to end, with `hop answer` driven as a
   separate one-shot process and a manager KILL between the human's
   answer and the forward: the relaunched fixture manager recovers the
   chain using ONLY CLI-returned data (the re-served answer's `origin`
   field, `hop msg show`), forwards idempotently under its recorded
   request ID, then acks; every envelope/delivery/ack/receipt row
   asserted.
3. `DuplicateAndAmbiguousDelivery` — trace 3 end to end with a real
   worker kill between fetch and ack and a real cold relaunch; while the
   worker is dead and the in-flight age passes the attention threshold,
   `hop status` renders the section 7 attention line with its address,
   in-flight message id and age, and queue count — and the line clears
   once the relaunched worker acks.
4. `WorkerInterruption` — trace 4 end to end (SIGKILL the worker; lease
   and evidence rules do the rest); asserts both attempts' full
   provenance (worktrees, sessions, claims, results).
5. `ReviewerRejection` — trace 5 end to end with `reviewer-reject-once`.
6. `ControllerReconnectNoSecondClaimant` — kill the controller; run two
   `hop resume` processes concurrently; exactly one acquires (lease CAS);
   the winner warm-reattaches manager and workers under the one predicate;
   the store never shows a second non-terminal manager session.
7. `InjectionFreeDelivery` — brief, task instructions and a manager
   answer seeded with hostile bytes (ANSI escapes, bracketed-paste
   open/close, `y` + newline, control characters); fixture principals
   validate byte-exact file delivery. The input-absence evidence is the
   S11 channel, stated honestly on two counts: the test's own client
   does NOT see the production subprocesses' requests (separate
   connections), and the api.request log line carries method and request
   ID, NOT the pane target — so the assertion is method-level, which is
   STRONGER, since HOP legitimately sends no typed-input request to any
   pane at all. The disposable server runs with API request logging
   enabled at the S11-pinned level, with capture-window completeness
   pinned (logging begins before the first HOP process starts and the
   capture is read only after the last one exits, with the S11-pinned
   start/complete record pairing proving no gap); the test performs the
   positive control (its own `pane.send_text` to a test-owned raw pane,
   issued through a SEPARATE connection exactly as production
   subprocesses connect, located in the captured log by its method name —
   S11 established that `request_id` is a per-connection label the server
   only echoes, not a global key, so two independent connections can and
   do log the same id value; a control is identified by method, then its
   own id read back from that line — and asserted PRESENT), then asserts
   the log contains ZERO entries, of EITHER the `api.request.complete`
   (a business-logic refusal) or `api.request.fail` (a response-write
   failure) event kind, for the ENTIRE terminal-input method set of
   section 7 — `pane.send_text`, `pane.send_keys`, `pane.send_input`,
   `agent.prompt`, `agent.send_keys`, `agent.start` (S11-confirmed as
   Herdr 0.9.0's complete real input surface) — across the whole isolated
   server's capture, other than that one identified control, run at the
   S11-pinned elevated level (the default level does not even record a
   successful `pane.send_text`, so the capture server must opt in to it
   for a zero count to be meaningful). The adapter's static allowlist,
   not this grep set, is what covers any future input surface; the
   scenario proves the shipped binary made none of the known ones.

Claim-protocol race cases that Phase 2 recorded as inexpressible without a
production pause/injection hook (launcher killed between claim write and
exec; suppressed `exec_failed` write; paused pre-exec launcher; stop
against a specifically pre-exec claim; check death before the claim write)
remain deferred as real-process scenarios, with the same reasons: Phase
3's exit scenarios do not require those windows, their dispositions stay
covered by the decision-table unit tests, and adding a pause hook to
production code is not justified by this phase either. If Phase 4's gate
work needs one, it must be designed explicitly then. Two of those windows
no longer leave a run waiting forever: a launcher killed between its claim
write and exec, and an exec failure whose `exec_failed` write never landed,
both leave an `exec_pending` claim behind a pane that closes with its
process, and the section 4 launch-ended row settles that claim once the
pane and the claimed process are both observed gone
(`TestLaunchEndedRowResolvesDeferredClaimWindows` drives both observable
sequences). The real-process proof of the row covers a manager whose
harness ends right after exec
(`TestRealProcessManagerLaunchVanishesBeforeCorroboration`); a worker
dying before settlement is proved by the app tables now and joins the
fixture-principal scenarios later.

## 12. Work breakdown

Ordered implementer tasks; each lands with its guides and tests, and each
slice has an explicit GREEN BOUNDARY: the whole-repo `make check` and
`make docs-check` pass at its landing, which the additive packaging rule
of section 3 makes possible — a slice never changes an existing method
signature or removes a member; new capability arrives as new standalone
interfaces (`MessagingStore`, `PlanStore`, `ReviewStore`), new methods on
new types, and new struct fields, so existing implementers keep compiling
untouched. Non-additive flips (retargeting `hop launch`, deleting
`Runtime.SendText` and `LoadLaunchContext`, switching the claim queries)
happen ONLY in the integrator slice 6, which updates every implementer,
fake and command in one landing. Cross-slice ownership is frozen before
parallel work: slice 2a owns every new DTO and port declaration, slice 4
owns the grammar constant set, and no other slice edits those files.
Integrator role as in Phase 2: the slice 6 implementer owns cross-branch
conflict resolution and lands branches one at a time. `go.mod` is
untouched (no new dependency anywhere in this phase).

| # | Task | Packages / files | Green boundary | Depends on | Parallel | Tier |
| --- | --- | --- | --- | --- | --- | --- |
| 0 | Spike: S8–S10 (herdr), G1–G3 (local git), S11 (request-log observability); findings reported before slice 2a freezes shapes | `test/integration` (probe tests + evidence) | probes green on the intended binaries; a probe that only SKIPPED (no herdr/git available) is not promotion evidence — every slice whose shapes a probe pins is BLOCKED from landing until that probe has actually passed, not just this slice | — | with 1 | Sonnet |
| 1 | Domain extension: task kinds/graph/release, multi-attempt, session roles/parent/optional attempt (solo paths proven unchanged by the existing Phase 2 domain suite), Message/Delivery/Ack, Review, Integration, `CompleteReview`, `EvaluateReadiness`, typed errors, the six traces | `internal/domain/run`, `internal/domain/identity` | additive types + new transitions; Phase 2 domain tests untouched and green | — | with 0 | Sonnet |
| 2a | Application ports and coordination: the new store interfaces and every Phase 3 DTO, scheduler (slots, release, assignment transaction), messaging use cases, plan/task/retry consumption, presentation integration; fake extensions; the `internal/testsupport/storevectors` package with its checker rule row | `internal/app`, `internal/testsupport/storevectors`, `internal` (test-support rule) | new files + additive fields only; app suite green against fakes; no existing port member changed | 0, 1 | —; starts after 0 and 1; 2b waits for it | Sonnet |
| 2b | Application enforcement core: guard evaluation wiring, integration operations (merge via fresh per-execution claims, publish/reset CAS with the fencing rule, no-op adoption, both integration barriers), review acceptance, per-attempt retirement with mailbox closure, completion + re-validation, stop/resume extension across roles and per-session attestation; the no-injection checks (the type-aware forbidden-call rule with fixtures incl. raw `Client.Call`, the all-ports surface allowlist test SHIPPING WITH one explicit, comment-dated legacy exception for `Runtime.SendText` that slice 6's removal landing deletes — the test then asserts its absence) | `internal/app`, `internal` (checker rule) | new files + the checker rule with fixtures; compiles against 2a's landed declarations | 0, 1, 2a (it consumes 2a's DTOs and port declarations — NOT parallel with 2a) | with 5 (which needs only 2a); 3 and 4 wait for it | Fable |
| 3 | SQLite: migration 003 (rebuilds, backfills incl. the historical-intent claim rule, migrator rebuild support with the dedicated-connection close), implementations of the new interfaces and the session-keyed queries as NEW methods, PLUS the in-place SQL updates the rebuilt schema forces on existing methods — `ClaimLaunch`'s INSERT gains `session_id` (resolved exactly as its read path already resolves currency: binding, else the session-keyed intent), `SubmitResult`'s check-request INSERT gains `id` and `subject_kind='result'` — behavior-preserving, since a rebuilt NOT NULL column cannot leave old SQL green by definition; raced suites; shared vectors against the real store; populated-migration fixtures covering active, completed, failed and claim-without-binding 001 stores | `internal/adapters/sqlite` | 003 + new methods + the enumerated in-place SQL updates; the Phase 2 sqlite suite green UNCHANGED as the behavior-preservation proof | 2a, 2b | with 4, 5 | Fable |
| 4 | Exec-boundary, config and templates: `PrepareLaunchExec` session addressing as a new use case PLUS the solo shim — `--run --attempt` stays supported permanently, resolved app-side (attempt → its current session → the session context), so the flag surface never breaks while the old port method still retires in 6; generalized check-exec claim kinds; assignment/role/review templates; the grammar constant set; `internal/adapters/config` policy parsing (new keys, feature-mode requireds) | `internal/app` (exec-boundary + grammar files), `internal/adapters/config` | additive use cases + config keys with defaults preserving solo behavior; both suites green | 2a, 2b | with 3 | Fable |
| 5 | Herdr adapter: `CreateWorkspace` and `FindWorkspaceByLabel` per S8 (new methods — additive), and the `CreateWorktree` request extension carrying the operation LABEL (S9; the landed request sends cwd/branch/base only, and the launch decision table requires label recovery) with its full-structural request fixtures | `internal/adapters/herdr` | new methods/fields + protocol tests; nothing existing removed | 2a | with 2b, 3, 4 | Sonnet |
| 6 | Integrator flip: new verbs and flags in `cmd/hop`, loop scheduling pass, status rendering, `hop view`, real-binary grammar contract tests; the non-additive flips — retarget `hop launch` internals onto the session context through slice 4's shim, delete `Runtime.SendText` (port, consumers and the staged allowlist exception), delete `LoadLaunchContext`, switch claim/context consumers to the session-keyed methods — across app, adapters, fakes and commands in one landing. Mechanical by design: every correctness-bearing behavior it wires (session queries, the shim, retirement, guards) was implemented and tested in Fable-owned slices 2b/3/4; this slice moves call sites and deletes deprecated members, and its tests are the dispatch/grammar/exit-code tables | `cmd/hop`, plus the flip's touch-points | whole-repo `make check` after the single flip landing, including the updated allowlist test asserting `SendText` is gone | 3, 4, 5 | — | Sonnet |
| 6b | Feature-run bootstrap: `StartFeatureRun` (the `--workflow feature` policy override, the frozen base commit, the predicted sequence, the role freeze and `InitializeRun`'s feature shape with the manager session, `integration.init`, the manager's `workspace.create` and `pane.open`), the `hop resume` feature startup continuation, stop and retirement of unplaced launches and of an unresolved `integration.init`, and `hop run --workflow` | `internal/app`, `internal/adapters/sqlite`, `internal/testsupport/storevectors`, `cmd/hop` | new use cases, the `InitializeRun` feature branch (the Phase 2 sqlite suite unchanged) and the new flag; app, sqlite and cmd suites green | 6 | with 8 | Fable |
| 7 | Scenario integration: fixture principal behaviors, the seven exit scenarios plus the five additional real-process scenarios, live opt-in test, Makefile target updates if any | `test/integration`, `Makefile` | real-process rows green (or skipped-with-reason where no herdr) | 6 | — | Sonnet |
| 8 | Post-merge worktree retirement (open question 6's resolution): lazy detection on the next controller start or `hop status` for the repository — the run's integration head is an ancestor of the target branch, checked under an exec claim — removal of that run's attempt worktrees under a fresh exec claim, and a persisted "worktrees retired" run fact so detection never repeats; branches are never deleted (`hop clean` is future work) | `internal/app`, `internal/adapters/sqlite` (the run fact), `cmd/hop` | detection and removal green against fixture repositories: a merged integration branch retires the run's worktrees exactly once with the fact persisted, and a run whose integration branch has not merged retires nothing | 5, 6 | with 7 | Sonnet |

Fable owns the three correctness-critical cores, per the tier rule: the
guard/integration/completion enforcement (2b — the phase's genuinely
difficult logic and its central promise), the SQLite atomicity/racing/
migration-rebuild contracts (3), and the exec-boundary/config extension
(4 — it touches the sanitized launch path and policy validation).
Everything else is ordinary generation against this document.

The additive rule governs INTERFACES; slice 3's in-place SQL updates
inside existing method bodies are the one sanctioned non-interface
change, forced by the schema rebuild and proven behavior-preserving by
the unchanged Phase 2 suite.

Landing order: 0 and 1; then 2a; then 2b, 3, 4, 5 (2b before 3 and 4,
which depend on it; 5 needs only 2a); then 6 (the one
interface-non-additive landing); then 7 and 8 (8 with 7, after 5 and 6).
Slice 6b, the feature-run bootstrap, was specified by sections 6, 9 and
10 but owned by no slice of the original breakdown; it was added after
6, on 6's tip.

## 13. Decisions assumed, spike dependencies, open questions, risks

Assumed decisions (directed by this design, pending human override). The
first is a consequence of the selected safety invariant, not a
preference — reversing it would be a new safety decision, never an
implementation option:

1. Pull-only delivery; HOP never types into a live pane, and an idle
   recipient that stops polling is surfaced as needs-attention, never
   nudged (section 7).
2. Dependency release requires `integrated`, and worktrees base on the
   integration head at attempt creation, with branch-uniqueness enforced
   before create and HEAD verified after (section 6).
3. Reviews are tasks; one reviewer evaluates the combined candidate; the
   guard binds verdicts to head object IDs (section 8).
4. Retry creates a new attempt with a fresh worktree; predecessors are
   preserved as evidence; exhaustion fails the task and run directly; a
   live controller never auto-relaunches a self-exited worker's attempt —
   retry is manager judgment, same-attempt recovery is `hop resume`'s
   (sections 2, 5, 6).
5. Messaging is store-only with at-least-once delivery, delivery-before-
   ack, enqueue-sequence FIFO, derived answer destinations, relay
   provenance, request-ID idempotency for every mutating verb, and
   receipt-first validation orders mirroring the Phase 2 result protocol
   (section 7).
6. The manager's plan is explicitly closed and reopened (`hop plan
   close`, reopen-on-create), an empty plan cannot close, and readiness
   is re-validated inside the final completion transaction (section 8).
7. Sessions are retired at per-attempt boundaries under the close rule;
   slots free only on observed termination (section 6).
8. Integration publishes and rolls back through compare-and-swap ref
   moves with the never-revisit rule; merges run under the generalized
   exec claim; the ancestor/no-op merge is a defined integrated outcome
   (sections 4, 8).

Spike dependencies: S8 gates the `CreateWorkspace`/`FindWorkspaceByLabel`
shapes and the manager-placement decision row (the label-names-the-
workspace fact and the sole-tab descent are S8-confirmed by execution);
S9 gates per-attempt worktree provenance — including executing
the existing-branch-ignores-base case that makes refuse-if-exists
mandatory; S10 must pass before multi-session launch lands (a failure
would force serialized pane creation, a loop change, not a model change)
and explicitly does NOT validate the session-keyed claim queries — the
sqlite raced suite does; G1–G3 gate the ref scheme, the merge outcome
matrix and the CAS publish mechanics; S11 gates the injection scenario's
evidence channel (log level, format, positive control). L1 informs the
`msg wait` default only.

Open questions for the human, each with a recommendation:

1. **Default workflow mode.** Should `hop run` default to `feature` once
   Phase 3 lands? Recommendation: no — default stays `solo` (Phase 2
   behavior, suite unchanged); `feature` via config or `--workflow`.
   Phase 4's playbooks are the natural point to revisit the default.
2. **Does the reviewer count against the worker bound?** Recommendation:
   yes — one bound (`[workers] max`, default 2) over all non-manager
   sessions; predictable resource use, no second knob.
3. **Message/answer body bound.** Recommendation: 64 KiB per body file
   (4 KiB for inline `--body`); larger content belongs in repository files
   the message references by path.
4. **Reviewer harness.** Recommendation: same harness as workers by
   default with `[roles.reviewer] harness` available. Choosing a
   non-Claude reviewer carries an EXPLICIT supported-recovery policy, not
   a status label: cold relaunch is unsupported for that session, so a
   lost non-Claude reviewer session is recovered by retiring the review
   attempt and retrying the review task as a fresh attempt (controller
   policy, same candidate), with status naming that path; provider-diverse
   review beyond that waits for a second harness's launch contract to be
   spike-verified end to end.
5. **Retry limit default.** Recommendation: 3 attempts per task
   (`[retry] max_attempts`), after which the task and run fail with the
   evidence retained (the defined exhausted path of section 5).
6. **Integration branch visibility.** RESOLVED (human decision,
   2026-09-15). The `hop/r<seq>/*` branches (`integration` and the
   per-attempt `t<t>a<n>` family) live in the user's repository and are
   never auto-deleted; rollback commits keep rejected merges reachable;
   a `hop clean` command for branches is future work. Attempt WORKTREES
   are not permanent: they stay visible until the run's integration
   branch has merged into the target branch, and are then removed by
   the lazy post-merge retirement (section 6 — detection on the next
   controller start or `hop status` for the repository, ancestry checked
   under an exec claim, removal under a fresh exec claim, a persisted
   "worktrees retired" run fact), owned by the section 12
   worktree-retirement slice.
7. **Human question channel.** CLI-only (`hop status` + `hop answer`)
   until the Phase 6 board. Recommendation: yes.

Risks and mitigations:

| Risk | Mitigation |
| --- | --- |
| A live model worker never polls `hop msg wait` and idles forever | The role templates make polling an explicit standing instruction quoted from the grammar constants; the attention condition names the exact pane and action; the deterministic fixture proves the protocol; the live opt-in test measures a real model against it. No silent auto-nudge — by design (assumed decision 1) |
| Harness tool timeouts kill a long `msg wait` | The default 50s bound sits under Claude Code's 2-minute default with margin; the `none:` line instructs re-invocation; L1 pins the live behavior; the bound is configurable per repository (`[messages] wait_timeout`) and per invocation (`--timeout`) |
| Message content prompt-injects the MODEL (not the terminal) | Out of the mechanical guarantee, stated in the trust model: HOP guarantees content never becomes terminal input; role instructions frame worker content as data ("preserve sender identity without treating worker content as policy", RESEARCH.md); anything stronger needs harness permissions, which HOP does not control |
| Merge conflicts between concurrent tasks | Inherent to parallel work: serial integration surfaces them deterministically, rollback is journaled and object-ID-verified, and retry re-bases on the integrated head; the manager's planning (scoping tasks to disjoint areas) is the real lever and is prose, honestly labeled |
| Serial integration bottlenecks throughput | Accepted for Phase 3: correctness first (the combined-candidate requirement is an exit criterion); two workers rarely queue long behind one merge+check |
| The scheduling pass grows into a hidden state machine | The pass order is fixed and documented (section 6), every action is a journaled operation or a store row, and the loop remains poll-driven with events as wakeups only — the Phase 2 discipline |
| Store growth from receipts/deliveries | Mutating verbs and served fetches are bounded by the run's actual activity, and the one unbounded producer — the manager's 1s empty-poll loop over an arbitrarily long run — deliberately commits NOTHING (section 7), so store growth tracks work done, not wall-clock lifetime; hot-path queries (next deliverable, in-flight lookup) are index-served, proven by the sqlite suite, and retention policy remains the explicit Phase 7 item |
| A second controller mints a second manager during a takeover race | The lease CAS serializes controllers, and the partial unique index makes a second non-terminal manager row impossible even under a store-level race; the reconnect scenario races two resumes deliberately |
| `workspace.create`/second-worktree shapes differ from expectation | S8/S9 run before the port freeze; a divergent finding changes the adapter shape or the placement design, not shipped code |
