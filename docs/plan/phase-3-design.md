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
manager plans by creating tasks (`hop task create`, with dependencies);
the controller releases tasks whose prerequisites are integrated, launches
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
| Manager + two workers exercised | `TestRealProcessFeatureRunEndToEnd` (section 11): fixture manager plans three tasks (one dependent), two fixture workers run concurrently, the third task waits for a slot, all integrate serially, reviewer approves, run completes |
| Dependency release | Same scenario: the dependent task is not launched until its prerequisite is `integrated` (not merely `completed`), asserted against the store and the launch journal |
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
| `Run` | No new states. Completion context widens (section 8): a run completes only through `EvaluateReadiness`, a pure function over the full guard context | Stop precedence and monotonicity unchanged; a feature run's `running` covers planning, execution and integration — rendered conditions, not states |
| `Task` | Gains `Kind` (implement, review), `Seq` (dense per run), title, instructions digest, and the extended machine: `pending → ready → active → checking → completed → integrating → integrated`, plus `needs-rework` | Dependencies are edges to same-run tasks; the graph is acyclic (validated at every `hop task create` against the persisted edge set); `ready` requires every prerequisite `integrated`; at most one active attempt (Phase 2 index); a review task never enters `integrating`/`integrated` — its verdict acceptance completes it |
| `TaskDependency` | New value: (task, prerequisite), unique, same run | Immutable once created; edges are created only with their task (a task's dependency set is fixed at creation — simpler than graph edits, and sufficient for this workflow) |
| `Attempt` | Multiple attempts per task: `NewAttempt(number)` with dense numbers from 1; a retry (`hop task retry`) reserves attempt n+1 only when attempt n is terminal | The Phase 2 machine is unchanged; each attempt has its own worktree, session lineage, claims and results; a stale prior-attempt submission is `ErrStaleSubmission` by attempt state (already in the Phase 2 acceptance context) |
| `Session` | `AttemptID` becomes optional (the manager session has none — the revisit recorded in [internal/domain/run/AGENTS.md](../../internal/domain/run/AGENTS.md)); gains `ParentSessionID` (nil for the manager) and the role values `manager`, `implementer`, `reviewer` | Parent and child belong to the same run; a parent must be the run's manager session (one-level delegation — a session with a parent can never itself be a parent); at most one non-terminal manager session per run; the state machine is Phase 2's, identical for all roles |
| `Message` | New: run ID, sender principal (session ID, or the reserved principals `controller` and `human`), recipient address (`manager`, `task:<task-id>`, `human`), kind (`question`, `answer`, `info`), optional `ReplyTo` (answer only), body artifact path + digest, created-at | Immutable envelope; an answer references exactly one question addressed to the sender's own address; one accepted answer per question (equal-digest resubmission idempotent, different-digest conflicting and rejected — the Phase 2 receipt rule transplanted); addressing by role/task, never by pane or pid |
| `Delivery` / `Ack` | New: append-only delivery observations (message, fetching session, incarnation, at) and a single ack per message (session, incarnation, at) | Delivery is at-least-once: an unacknowledged delivered message is re-served; the FIRST ack requires the recipient's current, non-superseded incarnation (the Phase 2 result eligibility rule); a repeat ack of an acknowledged message is idempotent regardless of incarnation |
| `Review` | New immutable verdict: run, review task, attempt, subject commit + tree object IDs, verdict (`approve`, `reject`), reasons digest, submitted-at | At most one accepted verdict per attempt; acceptance follows the section 8 validation order; a verdict never mutates — a new candidate gets a new review task |
| `Integration` | New: task, source commit (the accepted result), pre-merge integration head, merge commit (once observed), state (`merging`, `checking`, `integrated`, `conflicted`, `check-failed`, `rolled-back`) | One integration per (task, attempt-that-produced-the-result); serial: at most one non-terminal integration per run (application-scheduled, store-enforced by partial unique index); every state change carries recorded object IDs, never branch names alone |
| `Worktree` | One per attempt (Phase 2 had one per run): branched from the integration head current at attempt creation, base commit recorded | Preserved by stop, failure and retry (a retry's fresh worktree never deletes its predecessor — evidence) |

Pure transition functions keep the Phase 2 shape (explicit `now`, typed
errors). New typed errors: `ErrDependencyCycle`, `ErrDependencyNotIntegrated`,
`ErrDelegationDepth`, `ErrDuplicateAnswer`, `ErrConflictingAnswer`,
`ErrStaleAck`, `ErrVerdictSubjectMismatch`, `ErrRetryNotTerminal`,
`ErrRetryLimit`.

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

No new packages and no new checker rule rows: `internal/domain/run`,
`internal/domain/identity`, `internal/app`, the four Phase 2 adapters and
`cmd/hop` all keep their existing rows; the new files land inside them.
The pure-standard allowlists are unchanged (no `crypto/*`/`encoding/*` in
domain). Package guides are updated in the changes that extend each
package.

## 3. Ports (consumer-owned, in internal/app)

Reused unchanged: `StateStore`/`UnitOfWork` lease lifecycle, `Runtime`
(all Phase 2 methods and their contracts, including the close rule and
`ServerInstance`), `AgentPresentation`, `Observer`, `Clock`, `IDGenerator`,
`CommandRunner`, `ProcessGroupInspector`, `ArtifactStore`,
`ConfigurationSource` (extended value, same port). `SendText` remains the
documented launch fallback only; no new caller.

Extensions:

### UnitOfWork repositories

`UnitOfWork` gains `TaskDependencies()`, `Messages()` (envelopes,
deliveries, acks — controller-side reads and the re-address-free lineage
queries of section 7), `Reviews()` and `Integrations()`. Typed
repositories, domain values, `Save(entity, expectedRevision)` where
mutable; messages, dependencies, deliveries, acks and verdicts are
append-only/immutable and have no revision.

### SubmissionStore (worker and non-controller writes)

The worker authority widens. Every method is one internal transaction, no
controller lease, generation-NULL evidence, exactly as Phase 2 defines:

```go
type SubmissionStore interface {
    // Phase 2, unchanged:
    ClaimLaunch(ctx, claim LaunchClaim) error
    SettleLaunchFailure(ctx, incarnation IncarnationID, reason string) error
    ClaimCheckExec(ctx, op OperationID, pid int) error
    SubmitResult(ctx, submission ResultSubmission) (SubmissionOutcome, error)
    RequestStop(ctx, run RunID) error

    // Phase 3 messaging (section 7). All validate the caller's session and
    // incarnation currency; every outcome, refusals included, commits a
    // receipt.
    SendMessage(ctx, send MessageSend) (MessageOutcome, error)
    FetchNextMessage(ctx, fetch MessageFetch) (MessageDelivery, bool, error)
    AckMessage(ctx, ack MessageAck) (AckOutcome, error)

    // Phase 3 manager verbs (section 8): durable requests the controller
    // consumes by polling, exactly as check requests are consumed. The
    // caller must be the run's current manager session (current
    // incarnation); CreateTask validates titles/instructions bounds, the
    // dependency edges against the persisted graph (acyclic, same run) and
    // writes the task with its edges and instructions-artifact reference
    // atomically. RequestRetry is legal only against a terminal attempt of
    // a non-terminal implement task and below the frozen retry limit.
    CreateTask(ctx, req TaskCreate) (TaskCreated, error)
    RequestRetry(ctx, req RetryRequest) (RetryAccepted, error)

    // Phase 3 review (section 8): the reviewer's verdict submission,
    // validated like SubmitResult (receipt before eligibility).
    SubmitReview(ctx, submission ReviewSubmission) (ReviewOutcome, error)

    // Human answer (section 7): records the answer to a human-addressed
    // question; runs in a controller-machine context (no HOP_* env) but
    // still without the run lease.
    AnswerQuestion(ctx, ans HumanAnswer) (MessageOutcome, error)
}
```

### ReadStore

`LoadLaunchContext` widens to sessions without attempts (the manager) and
carries the session's role and — for workers — the task assignment path.
`RunDetail` gains the task table (states, dependencies, attempt counts),
per-address message queue depths and in-flight message age, pending
human questions, the integration head and guard shortfalls
(`EvaluateReadiness`'s `missing` list rendered verbatim). New:
`LoadMessagingContext(ctx, session SessionID)` for the message CLI verbs
(current incarnation, address resolution) — lease-free like the rest.

### Runtime

One new method:

```go
// CreateWorkspace creates a plain workspace (workspace.create) with an
// explicit cwd, additive env and a unique creation label, without focus,
// returning workspace/tab/root-pane IDs. Used only for the manager's
// placement; worker and reviewer placement continues to come from
// worktree.create's returned workspace. Shapes frozen against S8.
CreateWorkspace(ctx context.Context, req WorkspaceRequest) (WorkspaceHandle, error)
```

The manager pane itself is then opened with the Phase 2 `OpenWorkerPane`
(a `layout.apply` command pane addressed by that workspace ID) — same
transport, same claim, same corroboration predicate. Recovery of a lost
`workspace.create` response is by creation label through
`session.snapshot`, pinned by S8 before the port freezes.

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

### Migration 002

Forward-only, embedded, applied by the Phase 2 migrator
(`internal/adapters/sqlite/migrations/002_manager_workers_messages.sql`).
Same conventions: UUID TEXT ids, fixed-width canonical UTC times, STRICT
tables, `revision` on mutable rows, foreign keys enforced.

Altered tables:

| Table | Change |
| --- | --- |
| `tasks` | add `kind` (implement, review), `seq` (dense per run, UNIQUE(run_id, seq)), `title`, `instructions_path`, `instructions_digest`, `retry_count` |
| `sessions` | `attempt_id` becomes NULLable; add `parent_session_id` FK NULL (must reference a same-run session; the one-level rule is application-validated and belt-and-braces enforced by a trigger-free partial-index design: see `session_roles` note below); add partial UNIQUE index: one session per run WHERE `role='manager'` AND state not in the terminal set |
| `run_snapshots` | add `workflow JSON` (mode, max workers, retry limit, reviewer harness, role artifact paths + digests, integration branch name) — immutable like the rest of the snapshot |
| `worktrees` | add `attempt_id` FK NULL (Phase 2 rows keep NULL; new rows are per attempt), `base_commit` already exists |

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
| `messages` | id PK, run_id FK, sender_kind (session, controller, human), sender_session_id FK NULL, recipient_address (`manager`, `task:<uuid>`, `human`), kind (question, answer, info), reply_to FK NULL, body_path, body_digest, body_bytes, created_at | immutable; partial UNIQUE(reply_to) WHERE kind='answer' (one accepted answer per question); answers additionally validated against the question's address in the accepting transaction |
| `message_deliveries` | id PK, message_id FK, session_id FK, incarnation_id, delivered_at | append-only; every fetch that served the message, including re-serves |
| `message_acks` | message_id PK FK, session_id FK NULL (NULL for a human ack via `hop answer`), incarnation_id NULL, acked_at | at most one; insertion is the acknowledgement |
| `message_receipts` | id PK, op (send, fetch, ack, answer), claimed ids (plain TEXT, no FKs), outcome (accepted, duplicate, conflicting, stale, refused, malformed), detail, at | the messaging analogue of `result_submissions`: every outcome leaves evidence, malformed included |
| `reviews` | id PK, run_id FK, task_id FK, attempt_id FK, subject_commit_oid, subject_tree_oid, verdict (approve, reject), reasons_path, reasons_digest, submitted_at | partial UNIQUE(attempt_id) — one accepted verdict per attempt; UNIQUE(attempt_id, reasons_digest) supports idempotent duplicates |
| `review_submissions` | id PK, claimed ids TEXT, outcome, detail, submitted_at | receipts, as for results |
| `integrations` | id PK, run_id FK, task_id FK, result_id FK, source_commit_oid, premerge_head_oid, merge_commit_oid NULL, state, operation_id NULL, revision, created_at, updated_at | UNIQUE(task_id, result_id); partial UNIQUE(run_id) WHERE state in the non-terminal set — serial integration is store-enforced, not just scheduled |
| `retry_requests` | id PK, task_id FK, requested_by_session FK, reason, state (pending, consumed, refused), created_at | UNIQUE(task_id) WHERE state='pending'; consumed by the controller's polling loop |

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
| `workspace.create` (manager placement) | Adoption by unique creation label via `session.snapshot` (S8 pins the round-trip); label absent → bounded wait for the in-flight request, then `reconciling`; never a second create | same | same |
| `integration.merge` | Recovery by recorded object IDs, never branch names: integration branch head == recorded pre-merge head AND the integration checkout is clean → re-act; head == a commit whose parents are exactly {pre-merge head, source commit} → adopt as the merge commit; a dirty integration checkout whose path and branch match the intent → `git merge --abort` (the checkout is controller-owned plumbing), verify head unchanged, then re-act; any other head or a provenance mismatch → `reconciling` with the observed head as evidence | same | same; an unresolved merge blocks any further integration (the serial index already prevents a second one) |
| `integration.reset` (combined-check failure rollback, section 8) | Head == recorded merge commit → re-act (`git reset --hard <premerge>` in the integration checkout, plus `git branch -f` provenance-checked); head == pre-merge head → adopt; anything else → `reconciling` | same | same |
| `worktree.create` (now per attempt) | Phase 2 row verbatim; provenance = repository common-directory equality plus the recorded base commit (now the integration head frozen into the intent) | same | same |
| `pane.open` (manager, worker, reviewer) | Phase 2 row verbatim — one predicate, one close rule, all roles | same | same |
| `session.close` (completion retirement, section 8) | Phase 2 `pane.close` row verbatim | same | same |

The integration checkout is `runs/<run-uuid>/integration/tree`, a
`git worktree add` of the integration branch under the state root, created
once per run at freeze (journaled), used by every merge and reset, and
never a Herdr workspace — it is controller plumbing, inspectable by path
from `hop status`, not a pane. Merges and resets run `git -C` with the
absolute `Controller.GitExecutable`, per the Phase 2 invariant.

## 5. State machines and decision tables

All transitions remain pure domain functions driven through the journal.
Tables are exhaustive: an unlisted pair is invalid. The Attempt and
Session tables are Phase 2's verbatim (attempts now recur per task;
sessions now carry roles and optional parents with no machine change).
The Run table keeps Phase 2's exact state set and pairs, with two causes
re-scoped for feature mode (solo mode is untouched): `running →
completing` fires when `EvaluateReadiness` first holds and the completion
transaction begins child-session retirement — never on an individual
task's check being claimed, which in feature mode leaves the run
`running`; and `completing → completed` fires when retirement of every
live session is observed with no stop requested. `launching → running`
is the manager's settled launch claim. Only the changed/new tables are
printed below.

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
| integrating | needs-rework | merge conflict, or combined check failed (branch rolled back per the `integration.reset` row) |
| active, checking | needs-rework | the attempt reached a terminal non-completed state (failed per-task check, interrupted worker, exec failure) with the retry limit not yet reached; the task then waits for a manager retry request — the retry, once consumed, is what re-arms it |
| needs-rework | ready | retry consumed: attempt n+1 reserved (below the frozen retry limit), fresh worktree from the current integration head |
| active, checking, completed, integrating, needs-rework | interrupted | stop |
| active, checking, needs-rework | failed | retry limit reached, or terminal attempt failure with no retry path |

### Task (kind = review)

| From | To | Cause |
| --- | --- | --- |
| ready | active | reviewer assignment (created `ready`; review tasks have no dependencies and no integration) |
| active | completed | accepted verdict (approve OR reject — the verdict's content gates the run, not the task state) |
| ready, active | interrupted | stop |
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
| delivered | delivered | re-serve: any later fetch while unacknowledged (a new delivery row each time — at-least-once, journaled) |
| delivered | acknowledged | first valid ack (current incarnation of the fetching lineage) |
| queued, delivered | acknowledged | `human`-addressed questions only: the accepted `hop answer` acknowledges the question in the same transaction — directly from `queued`, since a human question is never fetched (its rendering in `hop status` is not a delivery) |

Per-recipient serialization: for each recipient address (`manager`,
`task:<id>`, `human`) at most one message is `delivered`-unacknowledged at
a time; `FetchNextMessage` serves the in-flight message again if one
exists, else the oldest `queued` message, FIFO by created-at with the
message ID as the deterministic tie-break. There is no message TTL and no
expiry in Phase 3; an unfetched queue ages visibly in `hop status`
(section 9's attention condition).

### Integration

| From | To | Cause |
| --- | --- | --- |
| merging | checking | merge commit observed (outcome recorded with the commit + tree object IDs) |
| merging | conflicted | merge conflict observed; abort evidence recorded |
| checking | integrated | passing combined-candidate check receipt for the merge commit |
| checking | check-failed | failing combined check; triggers the `integration.reset` operation |
| check-failed | rolled-back | reset outcome observed (branch back at the pre-merge head) |
| merging, checking | interrupted | stop (an in-flight merge is retired like a check group: the merge command runs under `CommandRunner` cancellation; recovery uses the `integration.merge` row) |

`conflicted` and `rolled-back` settle the integration; the task moves to
`needs-rework` in the same outcome transaction, and the controller's info
message to the manager (with the conflict or check evidence paths)
commits with it.

### Reference traces

The domain and application suites pin these exact event orders (the format
of Phase 2 section 5; each trace is a complete multi-entity scenario):

1. **Feature happy path, dependency release.** Manager launched (Phase 2
   launch trace verbatim for the manager session); manager `CreateTask` A
   (no deps → `ready`) and B (depends on A → `pending`); controller
   assigns A (Task A ready→active, Attempt A1 reserved→launching, Session
   reserved→launching, atomically); A submits, per-task check passes
   (Phase 2 trace) → A `completed`; integration A claimed → A
   `integrating`, merge observed → integration `checking`, combined check
   passes → A `integrated` AND — same transaction — B `pending→ready` and
   the controller info message to the manager commits; B assigned and
   completes the same way; review task R created `ready` → reviewer
   assigned → verdict approve accepted (R active→completed) →
   `EvaluateReadiness` true → Run completing→completed after child-session
   retirement (section 8).
2. **Relayed question, no lifecycle movement.** Worker (task B) sends
   `question` q1 to `manager` (queued); manager fetch (q1 delivered);
   manager sends `question` q2 to `human` (queued) and acks q1; human
   `hop answer` on q2 (answer a2 accepted, q2 acknowledged, atomically);
   manager fetch (a2 delivered), manager acks a2, manager sends `answer`
   a1 reply-to q1 to `task:B`; worker fetch (a1 delivered), worker acks,
   continues. No Run/Task/Attempt/Session state changes anywhere in the
   trace — messaging is orthogonal to the lifecycle machines.
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
   attempts, both worktrees, both session lineages.
5. **Reviewer rejection and re-review.** Review R1 verdict reject accepted
   (R1 completed; `EvaluateReadiness` false — shortfall names the reject);
   controller info to manager with the reasons artifact path; manager
   creates fix task F; F completes and integrates → new head H2;
   controller creates review task R2 with subject H2; R1's verdict is now
   bound to a stale subject by construction (subject ≠ head) and can never
   satisfy the guard; R2 approve on H2 → run completes.
6. **Stop precedence across the new machinery.** Stop requested during
   `integrating`: the merge command's group is cancelled and retired, the
   integration settles `interrupted`, tasks/attempts interrupt per Phase 2,
   every child session and the manager are closed under the close rule,
   and the run reports `stopping` until every owned process is observed
   absent — a passing combined check that lands after the stop request
   records evidence and yields `interrupted`, never `integrated`.

## 6. Scheduling, delegation and worker launch

### The controller loop, extended

The Phase 2 loop (heartbeats, transition lines, launch corroboration, stop
routing, 2s check polling) gains polled work sources, all durable rows,
with status events only ever wakeups: pending manager requests (new tasks,
retry requests), released-and-assignable tasks, pending integrations,
review-task creation, and the message-age attention condition. One
scheduling pass per poll tick, deterministic order: consume manager
requests → recompute releases → claim at most one integration → create the
review task when due → assign released tasks into free slots (task `seq`
order) → corroborate launches → drive checks. Every dispatch revalidates
(heartbeat CAS + stop re-read) exactly as Phase 2 requires.

### Bounded concurrency

`MaxWorkers` (default 2) bounds the count of non-terminal, non-manager
sessions (implementers and the reviewer share the bound — open question 2).
The bound is enforced in the assignment transaction by counting live
child-session rows inside it, so two controllers can never overshoot (and
there is only one controller per run anyway — the lease); a slot frees
when a session reaches a terminal state, never merely when a pane event
arrives (Phase 2's evidence rules decide termination).

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
| reviewer | `worktree.create` (branch `hop/r<seq>/review<n>`, base = the subject integration head), cwd = the worktree | Phase 2 set plus `HOP_SESSION_ID`, `HOP_ROLE=reviewer` | the review assignment artifact: the frozen subject (commit + tree oid), the diff scope (base..head), the reviewer role artifact, and the verdict submission instruction |

`hop launch` takes `--run` and `--session` (the attempt, where one exists,
resolves from the session row); the launch context fails closed on any
missing or disagreeing `HOP_*` variable exactly as Phase 2 specifies.
`LaunchClaim` and its corroboration are untouched — one predicate for all
roles.

Cold resume remains Claude-only (`--resume <native-ref>`), per harness
composition unchanged. The Phase 2 restore policy applies to every session
including the manager: a Herdr-restored occupant is never adopted;
positive-evidence retirement or attestation, then cold relaunch.

### Integration branch and worktree bases

At freeze the controller records the base commit (repository HEAD at
freeze, resolved to an object ID) and creates branch `hop/r<seq>` — the
integration branch — at that commit, plus the state-root integration
checkout (section 4). Task worktrees branch from the integration head
current at their attempt's creation, so dependent work always builds on
integrated prerequisite work; that is why dependency release requires
`integrated`, not `completed`. Retry worktrees re-base the same way, and
the retry assignment artifact carries the prior attempt's result commit,
check output and (where present) review reasons paths so the worker can
recover useful work deliberately. HOP never pushes, never merges into the
user's own branches, and never deletes `hop/*` branches or superseded
worktrees; publication of the integration branch is the human's act.

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

- No `app` use case calls `Runtime.SendText`; no `agent.prompt`,
  `agent.send_keys` or `pane.send_keys` method exists on any port. The
  send-text launch fallback remains declared and remains unused by
  `hop run` (already the Phase 2 status quo, recorded in
  [test/integration/AGENTS.md](../../test/integration/AGENTS.md)).
  Checked three ways (section 11): the existing narrow AST forbidden-call
  checker in `internal` (the mechanism
  [engineering.md](../architecture/engineering.md) already uses for
  ambient clock/ID calls) gains a rule that `internal/app` production code
  never references `Runtime.SendText`; a port-surface test asserts
  `app.Runtime`'s exact method set (a typed-pane-input method appearing on
  any port fails the build's tests, not just review); and the fake Runtime
  records every call so the feature scenarios assert zero `SendText`
  invocations. The invariant is also recorded in the `internal/app` and
  herdr-adapter guides in the changes that land it.
- Message bodies are never printed raw to a terminal-bound stream: the CLI
  prints the body artifact path, and body files are written
  temp-file-then-rename with digests. A body containing ANSI escapes,
  bracketed-paste sequences or `y\n` therefore cannot reach any terminal
  input or output path through HOP. Checked by the grammar contract tests
  (real binary, hostile bodies) and `TestRealProcessInjectionFreeDelivery`.
- A recipient that never polls is a reported condition, not a nudge
  target: HOP never wakes an idle model by typing (open question 3). The
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
  new `question` to `human` — and `hop msg ack` the question; (kind
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
  after submission, and the pane closes when the process exits.
- **Reviewer**: reads the frozen subject, may ask the manager questions
  under the same wait-loop rule, submits exactly one verdict, ends its
  turn.
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

Deferred capability — a dialog-safe nudge: a typed wake-up for an idle
recipient stays out of HOP until Herdr offers a surface that removes the
misdetection window, none of which exists on 0.9.0. Facts that would
unblock it, each needing its own capability probe first: (i) a prompt API
whose delivery is server-side conditional on the pane's status authority
being a complete lifecycle integration rather than a screen manifest
(Claude Code and Codex are `session`-only integrations today, so their
`blocked` remains screen-detected and strict — [agents.mdx](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/agents.mdx));
(ii) a submission mode that delivers text without Enter and reports what
the composer contained, so a misdirected delivery is inert and detectable;
or (iii) a compare-and-swap input guard (refuse unless the screen state
the caller inspected is still current). Until one is verified, the
attention surface above is the whole answer.

Acknowledgment semantics, stated honestly (the Phase 2 formulation): a
delivery row proves the recipient's harness ran `hop msg next` and
received the envelope and path; an ack row proves it ran `hop msg ack`.
Whether the model read the body file and followed it is behavioral — the
fixture worker validates delivered bytes in tests, and the workflow's
result/verdict protocols are the behavioral evidence in production.

### Message verbs and validation

All verbs resolve the caller from `HOP_SESSION_ID` + `HOP_INCARNATION_ID`
(worker context, absolute `HOP_STATE_DIR` required, no fallback — Phase 2
rule) except `hop answer`, which is a human/controller-machine command.
Validation orders are normative and mirror section 7 of the Phase 2
design (receipt before eligibility):

- **Send** (`hop msg send --to manager|task:<id>|human --kind
  question|info|answer [--reply-to <id>] --file <path>`): parse and bound
  (kind/address legality by role: workers → `manager` only; manager →
  `task:<id>`, `human`; `answer` requires `reply-to` naming an
  unanswered question addressed to the sender's own address); duplicate
  answer (equal body digest for the same question) → idempotent success;
  conflicting answer → refused, receipt, the accepted answer undisturbed;
  eligibility (current incarnation, run not stopping/stopped); accept:
  body artifact written BEFORE the row's transaction (a file write is not
  an external call in the journal sense, but the row commits only after
  the bytes are durable — temp-file-then-rename, digest recorded), then
  envelope row + receipt in one transaction.
- **Fetch** (`hop msg next`, and `hop msg wait [--timeout 50s]` — the same
  fetch in a bounded store poll loop, 1s interval, sleeping in the CLI
  process, not the store): resolve the caller's address (`manager` for the
  manager session; `task:<id>` for a worker or reviewer via its attempt's
  task; lineage-based, so a cold-relaunched or retried successor session
  fetches its predecessors' queue without any re-addressing write);
  serve the in-flight delivered-unacknowledged message if one exists, else
  the oldest queued; write the delivery row and receipt in the same
  transaction that decides; print the envelope and body path. `wait`
  prints the protocol line `none: no message within <timeout>; run hop msg
  wait again` on expiry — bounded below the harness's own tool timeout
  (default 50s; L1 in section 11 pins the live constraint).
- **Ack** (`hop msg ack <message-id>`): already-acknowledged → duplicate,
  idempotent exit 0; first ack requires the caller to resolve to the
  message's recipient address with a current incarnation — a stale
  incarnation or a foreign session is refused with a receipt; the ack row
  commits with its receipt.
- **Answer** (`hop answer <question-id> --file <path> | --body "<text>"`):
  validates the question is `human`-addressed and unanswered; writes the
  answer body artifact, the answer message row (sender `human`, recipient
  `manager`), the question's ack and the receipt in one transaction.
  Duplicate (equal digest) idempotent; conflicting refused.

### Exactly what is journaled

For every message: the immutable envelope row (sender principal, recipient
address, kind, reply-to, body path, digest, size, created-at); one
delivery row per serve (session, incarnation, time — re-serves included);
at most one ack row; and a receipt row for EVERY verb outcome including
refusals and malformed input (claimed ids as plain text, no FKs — the
Phase 2 `result_submissions` pattern). Controller info notices are
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
| `hop result submit` | `accepted <result-uuid>` / `duplicate <result-uuid>` | `transient: attempt not yet running; retry` (Phase 2, unchanged) |
| `hop msg next` | `message <uuid> kind=<kind> from=<principal>[ reply-to=<uuid>]` then `body: <abs path>` then `ack: hop msg ack <uuid>` | `none: no queued message` |
| `hop msg wait` | as `next` | `none: no message within <timeout>; run hop msg wait again` |
| `hop msg ack` | `acknowledged <uuid>` / `duplicate <uuid>` | — |
| `hop msg send` | `sent <uuid>` / `duplicate <uuid>` | — |
| `hop task create` | `task <uuid> t<seq> created` | — |
| `hop task retry` | `retry accepted t<seq> attempt <n>` | — |
| `hop review submit` | `verdict accepted <review-uuid>` / `duplicate <review-uuid>` | — |

Refusals exit 1 with one first line `refused: <reason-token>` and detail
lines after; reason tokens are enumerated in the same grammar constant
set. The assignment and role templates quote these lines verbatim from the
same constants, so template, CLI and fixture can never drift apart
silently.

## 8. The built-in feature workflow: review, guards, serial integration

### Guards are application behavior, not prose

Run completion is a single controller transaction that calls
`EvaluateReadiness` over evidence rows only:

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
states come only from the controller's journaled merge/check/reset
operations. `hop task create`, `hop task retry`, `hop msg send` and the
read/ack side of messaging (`hop msg next`/`wait`/`ack`) are the manager's
complete verb set. A manager that writes "all checks passed" in a message
has changed nothing.

Per-task guards are Phase 2's, applied per task: acceptance requires the
result protocol; task completion requires the accepted result plus a
passing per-task check; nothing about Herdr idle/Done ever completes
anything.

### Serial integration and combined revalidation

Integrations are claimed one at a time (store-enforced partial unique
index, section 4), in dependency order then task `seq` order, each as the
journaled operation pair of section 4: merge (`git merge --no-ff
<source-commit>` in the integration checkout; conflict → abort, evidence,
`conflicted`) then the frozen check against the merge commit through the
UNCHANGED Phase 2 check pipeline (detached checkout of the merge commit
under the check operation's directory — the integration checkout itself is
never the check target). A failing combined check triggers the journaled
reset (branch back to the recorded pre-merge head) and `needs-rework`;
passing per-task checks on separate branches never substitute — this is
the exit criterion's revalidation, mechanically.

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
settled claim — the Phase 2 early-submission rule), AND the submitted
subject equals the review task's frozen subject (`ErrVerdictSubjectMismatch`
→ refused: the reviewer reviewed the wrong candidate); accept atomically —
verdict row, receipt, attempt `submitted`→`completed` collapse (review
attempts skip `checking`; the acceptance transaction applies
Attempt→submitted→completed and Task→completed with their evidence rows),
and the controller info message to the manager.

A `reject` verdict completes the review task but fails the readiness
guard; the shortfall (with the reasons artifact path) is what the manager
acts on — a fix task, whose integration produces a new head, which gets
its own new review task. An `approve` bound to a superseded head is
harmless by construction: the guard compares object IDs, so staleness is
computed, never stored (the gate-evaluation immutability principle of
[domain-model.md](../architecture/domain-model.md)).

### Reviewer independence

Structural: the reviewer session is a distinct session (its own native
reference, its own worktree at the subject head) that has executed no
implement attempt, launched with the frozen reviewer role artifact. Same
harness by default (`[roles.reviewer] harness`, open question 5);
independence in Phase 3 means a separate session and role, not a separate
provider.

### Completion retirement

After `EvaluateReadiness` passes and before the run records `completed`,
the controller retires the run's live sessions (workers are normally
already gone — their panes close on exit; the manager and any idle
survivor are closed under the Phase 2 close rule with scrollback captured
first). Worktrees, the integration branch and every artifact are
preserved. Stop precedence still applies: a stop request observed in the
completion transaction yields `interrupted`/`stopping`, never `completed`.

## 9. Controller ownership and the native Agent view

### Ownership: the Phase 2 lease, extended not replaced

One controller per run, the same `run_leases` row, CAS heartbeat/release,
fencing generations, `InitializeRun` creating the first lease. Phase 3
adds nothing to the lease protocol; it adds the manager session UNDER it:
the manager session row is created inside `InitializeRun`'s transaction
(feature mode), and the partial unique index (one non-terminal manager
session per run) makes a second manager unrepresentable regardless of
controller behavior. `hop resume` reconciles the manager exactly as it
reconciles workers (same predicate, same restore policy, same
`--confirm-absent` contract); a takeover therefore recovers ownership of
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
  rebound when the occupant changes, per the surface audit.
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
| `hop run "<brief>"` | Phase 2 flags plus `--workflow solo\|feature` (overrides `[workflow] mode`; default remains solo — open question 1) | Feature mode: freezes the workflow snapshot and role artifacts, creates the integration branch and checkout, launches the manager, runs the extended loop. Solo mode: Phase 2 verbatim |
| `hop status` | unchanged flags | Detail block gains: task table (seq, kind, state, deps, attempt count, worktree), integration head and per-integration states, message queue depth and in-flight age per address, pending human questions (`question <uuid> …` with the body path and the `hop answer` invocation), guard shortfalls verbatim from `EvaluateReadiness`, per-session roles/bindings |
| `hop stop <run-id>` | unchanged | Now retires manager + workers + reviewer + check and merge groups; same observed-termination contract |
| `hop resume <run-id>` | unchanged, incl. `--confirm-absent` | Reconciles every session under the one predicate; the attestation contract applies per session |
| `hop task create` | `--title`, `--file <instructions>`, `--depends-on <task>` (repeatable); identities from the manager's `HOP_*` env | Manager-only (validated); prints `task <uuid> t<seq> created`; refuses cycles, cross-run deps, non-manager callers, `kind=review` |
| `hop task retry <task>` | `--reason <text>` | Manager-only; legal only for a terminal attempt of a `needs-rework` task under the retry limit; prints `retry accepted t<seq> attempt <n>` |
| `hop msg send` | `--to`, `--kind`, `--reply-to`, `--file` (or `--body` ≤ 4 KiB) | Section 7 validation; worker/manager contexts |
| `hop msg next` / `hop msg wait` | `wait`: `--timeout` (default 50s) | Section 7 grammar; `next` never blocks |
| `hop msg ack <message-id>` | — | Section 7 ack rules |
| `hop answer <question-id>` | `--file` or `--body` | Human command (controller-machine state root); acks the question atomically with the accepted answer |
| `hop review submit` | `--verdict approve\|reject`, `--subject <commit-oid>`, `--reasons-file <path>` | Reviewer-only; section 8 validation; `verdict accepted <uuid>` |
| `hop view set` / `hop view clear` | `set`: `--run <id\|r<seq>>` | Owned native-view selection/clear (section 9); exit 0 on server acknowledgement |

Worker plumbing (`launch`, `check-exec`, `result submit`, `msg *`,
`review submit`) stays under its usage heading. `hop launch` gains
`--session` (section 6) while keeping `--run --attempt` exactly as Phase 2
rendered it — solo-mode panes continue to carry the Phase 2 argv verbatim,
and the two forms are mutually exclusive. Exit codes keep the Phase 2
discipline; message and review refusals exit 1 with the `refused:`
grammar; usage errors 2.

## 11. Test plan

Integration-first, per the Phase 2 lesson: every new external surface gets
a capability probe pinning the exact observed values under the production
transport BEFORE any decision rule depends on it; app-layer fakes
reproduce the pinned shapes and enforce the real adapters' argument
contracts; the exit scenarios are real-process.

### Spike probes (task 0, `test/integration`, continuing the S-series)

| Probe | Question | Pins |
| --- | --- | --- |
| S8 `workspace.create` | Response shape (workspace/tab/root-pane IDs), creation label round-trip through `session.snapshot`, cwd/env delivery to the root pane, no-focus behavior | The `WorkspaceRequest`/`WorkspaceHandle` shapes and the workspace.create decision-table row's label recovery |
| S9 concurrent worktrees | Two+ `worktree.create` calls against one repository: `--base <oid>` honored (HEAD of the new branch equals the base), branch-exists behavior, returned workspace/path/branch per call, grouping with the parent workspace, label round-trip, coexistence with the S6 command-pane launch in each | Per-attempt worktree creation, the base-commit provenance rule against integration heads, and reviewer worktrees |
| S10 multi-pane env isolation | Three command panes created back-to-back with distinct env maps and labels (manager-shaped, worker-shaped ×2): each process observes exactly its own additive env (`HOP_SESSION_ID`/`HOP_ROLE` differ), labels resolve to the right panes, agent detection per pane independent | That concurrent launches cannot cross-contaminate identity — the multi-session corroboration and close rules rest on it |

Deliberately not probed, with the reason recorded: `agent.prompt`,
`agent.wait`, `agent.send_keys`, `pane.send_keys` — no Phase 3 decision
rule consumes them (section 7); probing them would imply a channel this
design forbids. L1 (opt-in, live): a real Claude session executing
`hop msg wait --timeout 50s` inside its shell tool, pinning that the
bounded wait returns within the harness's tool timeout and the model can
loop on the `none:` line; informs the default only, never CI.

### Countermeasures for the Phase 2 escaped-defect class

Three Phase 2 production defects escaped review because each layer's unit
tests were self-consistent but wrong about a real dependency. Each gets a
structural countermeasure, not just a test:

| Escaped-defect class | Phase 3 countermeasure |
| --- | --- |
| A fake accepted arguments the real adapter refuses (bare `git` argv vs the runner's absolute-path contract) | Every new fake mirrors the real adapter's argument validation, extending `TestFakeStoreContracts`/`TestFakePortsRefuseCallsInsideTransactions`: the fake store enforces the message/review/task contracts (serialization, one answer per question, ack incarnation rule, manager-only verbs, serial-integration index) with a contract test proving fake and real store refuse the SAME inputs — the sqlite suite and the app fakes share one table of refused-input vectors (a Go slice both packages' tests import from a tiny internal test-support constant set in `internal/app`, as the existing contract tests do) |
| A protocol field's real shape differed from the assumed one (argv0 as basename) | No new identity or matching rule is introduced anywhere in Phase 3 — every occupant decision reuses `CorroborateSettlement` verbatim; the only new wire shapes (`workspace.create`, second-worktree responses) are S8/S9-pinned before the port freeze, and the herdr adapter's `assertRequestParams` full-structural fixtures cover the new methods |
| A worker-facing protocol line was never rendered by the real binary | The section 7 grammar is one constant set; `cmd/hop` contract tests execute the REAL built binary for every verb and assert each first line against the constants; the fixture worker/manager/reviewer parse ONLY those constants; a golden-grammar test fails if a constant, a rendered line or a template quotation drifts |

### Layers

| Layer | Tests |
| --- | --- |
| Domain (`internal/domain/run`, `identity`) | Independent transcription tables for the extended Task machines (both kinds), Message delivery, Integration; multi-attempt density and `ErrRetryNotTerminal`/`ErrRetryLimit`; dependency acyclicity and `ReleaseEligible`; delegation-depth and manager-uniqueness rules; `AcceptVerdict`, `AcceptAck`, `NextDeliverable` orders (receipt-before-eligibility, duplicate/conflicting/stale vectors); `EvaluateReadiness` shortfall vectors (missing integration, stale-subject approve, reject present, head moved); the six section 5 reference traces as complete multi-entity traces; new ID parsing with fuzz |
| Application (`internal/app`) | Extended fakes with the shared refused-input vectors; scheduler scenarios: slot bound enforced inside the assignment transaction, release only on `integrated`, deterministic pass order, retry consumption, review-task creation exactly once per head; integration decision-table rows (all three crash columns for merge and reset, conflict evidence, stop mid-merge); guard enforcement (no verb reaches the guard rows; a scripted "manager" calling every verb cannot complete a run without real receipts); messaging scenarios (serialization per address, re-serve, stale ack, duplicate ack, answer-acks-question atomicity, lineage fetch after relaunch and retry); the no-injection invariant checked (section 7): the `internal` AST forbidden-call rule (`internal/app` production code never references `Runtime.SendText`), the `app.Runtime` port-surface method-set test, and every feature scenario asserting the recording fake saw zero SendText calls; presentation republication on transitions and rehydration on resume; completion retirement under the close rule; six reference traces at app level |
| SQLite (`internal/adapters/sqlite`) | Migration 002 applied over a populated 001 store (Phase 2 rows intact, new columns defaulted) and from empty; the shared refused-input vectors against the real store; raced contracts across separate handles: fetch/fetch (one delivery row winner per serve), ack/ack (one ack), answer/answer (one accepted), CreateTask cycle check under concurrent edge inserts, serial-integration index raced, manager-uniqueness index raced, retry-request unique-pending raced; lineage fetch queries; receipts for every refusal path; `RunDetail` extensions |
| Herdr adapter | `CreateWorkspace` protocol tests (S8 shapes, full-structural request fixture, partial-response tables, label recovery, cancellation); no other adapter change expected |
| Process/config/system | TOML tables for the new policy keys (defaults, feature-mode requireds, unknown keys); no new process-adapter behavior (merge/reset ride the existing `CommandRunner`) |
| cmd/hop | New verb tables (dispatch, flags, exit codes, worker/human state-root rules); the real-binary grammar contract tests (above); loop scheduling-pass ordering with the fake clock; `hop view` commands |
| Real process (`test/integration`) | The exit scenarios below, plus: `TestRealProcessIntegrationConflict` (two tasks touching one file; conflict → needs-rework → retry from the new base → completes), `TestRealProcessCombinedCheckFailure` (per-task checks pass, combined check fails, branch reset verified by object ID, rework), `TestRealProcessStopDuringFeatureRun` (stop mid-integration: merge group retired, every session terminated, `stopped` only on observed absence), `TestRealProcessManagerColdRelaunch` (manager killed; resume cold-relaunches the manager lineage via `--resume`, task state undisturbed) |
| Live (opt-in) | `TestLiveClaudeFeatureRun`, gated on `HOP_LIVE_HARNESS=1`: real Claude as manager, one worker task and review on a fixture repository — plus L1. Never in CI or `make check`; the deterministic fixture suite is the gate |

### Real-process exit scenarios (fixture harness)

The fixture worker generalizes into a fixture principal: one deterministic
Go program installed as `claude`, dispatching on `FIXTURE-BEHAVIOR:`
directives delivered through its assignment artifact — new behaviors
`manager-feature` (create scripted tasks with a dependency, poll
`hop msg wait`, answer questions from a scripted table, retry on
interruption notices, relay one question to the human when scripted),
`worker-implement`, `worker-question` (asks, waits, applies the answer's
file content, submits), `worker-fetch-crash` (fetches then exits before
ack), `reviewer-approve`, `reviewer-reject-once`. Every behavior keeps the
Phase 2 contract: dump observations to files, never judge itself, retry on
the `transient` line, parse only the section 7 grammar.

Scenarios (all `TestRealProcess*`, disposable server, private roots — the
user's live server at `~/.config/herdr/herdr.sock` is never touched):

1. `FeatureRunEndToEnd` — the section 1 evidence row: three tasks
   (t2 depends on t1; t3 independent), `MaxWorkers=2`, assert t3 launches
   only after a slot frees and t2 only after t1 is `integrated`; serial
   integration order; review; completion; every guard row present in the
   store; token metadata published manager-first (asserted via
   `agent.list` as in Phase 1).
2. `RelayedQuestion` — trace 2 end to end, with `hop answer` driven as a
   separate one-shot process; every envelope/delivery/ack/receipt row
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
   validate byte-exact file delivery; the worker pane's scrollback and the
   server's request log (the suite drives its own client, so every request
   HOP made is observable) contain no `pane.send_text`/`agent.prompt` to
   any live session pane; the only send_text anywhere is none.

Claim-protocol race cases that Phase 2 recorded as inexpressible without a
production pause/injection hook (launcher killed between claim write and
exec; suppressed `exec_failed` write; paused pre-exec launcher; stop
against a specifically pre-exec claim; check death before the claim write)
remain deferred with the same reasons: Phase 3's exit scenarios do not
require those windows, their dispositions stay covered by the
decision-table unit tests, and adding a pause hook to production code is
not justified by this phase either. If Phase 4's gate work needs one, it
must be designed explicitly then.

## 12. Work breakdown

Ordered implementer tasks; each lands with its guides and tests, each
branch self-contained (own checker rows where any, `make check` and
`make docs-check` green at every boundary). Integrator role as in Phase 2:
the task 6 implementer owns cross-branch conflict resolution (notably
`internal/app/AGENTS.md` and the sqlite guide) and lands branches one at a
time. `go.mod` is untouched (no new dependency anywhere in this phase).

| # | Task | Packages / files | Tests | Depends on | Parallel | Tier |
| --- | --- | --- | --- | --- | --- | --- |
| 0 | Spike S8–S10; findings reported before task 2 freezes shapes | `test/integration` (probe tests + evidence) | the probes | — | with 1 | Sonnet |
| 1 | Domain extension: task kinds/graph/release, multi-attempt, session roles/parent/optional attempt, Message/Delivery/Ack, Review, Integration, `EvaluateReadiness`, typed errors, the six traces | `internal/domain/run`, `internal/domain/identity` | Domain rows of section 11 | — | with 0 | Sonnet |
| 2a | Application: scheduler (slots, release, assignment transaction), messaging use cases, task-create/retry consumption, presentation integration, DTOs; fake extensions with the shared refused-input vectors | `internal/app` | App rows except guards/integration | 0, 1 | with 2b | Sonnet |
| 2b | Application enforcement core: guard evaluation wiring, serial integration operations (merge/check/reset decision rows), review acceptance, completion + retirement, stop/resume extension across roles; the no-injection checks (the `internal` forbidden-call rule for `Runtime.SendText` in app production code, and the port-surface method-set test) | `internal/app`, `internal` (checker rule) | Guard, integration, stop/resume and trace rows; the forbidden-call rule's positive/negative fixtures | 0, 1 | with 2a (disjoint files; 2b owns `usecase_integrate.go`, `usecase_review.go`, completion) | Fable |
| 3 | SQLite: migration 002, new/extended repositories, messaging/review/task/retry submission contracts, raced suites, shared vectors against the real store | `internal/adapters/sqlite` | SQLite rows of section 11 | 2a, 2b | with 4, 5 | Fable |
| 4 | Exec-boundary and template extension: `hop launch --session`, role-aware launch contexts, assignment/role/review templates quoting the grammar constants, the grammar constant set | `internal/app` (exec-boundary + grammar files), templates | Exec-boundary tables; grammar golden tests | 2a, 2b | with 3, 5 | Fable |
| 5 | Herdr adapter: `CreateWorkspace` per S8 | `internal/adapters/herdr` | Adapter row | 2a | with 3, 4 | Sonnet |
| 6 | Command wiring (integrator): new verbs, loop scheduling pass, status rendering, `hop view`, real-binary grammar contract tests | `cmd/hop` | cmd rows | 3, 4, 5 | — | Sonnet |
| 7 | Scenario integration: fixture principal behaviors, the seven exit scenarios plus the four additional real-process scenarios, live opt-in test, Makefile target updates if any | `test/integration`, `Makefile` | Real-process rows | 6 | — | Sonnet |

Fable owns the three correctness-critical cores, per the tier rule: the
guard/integration/completion enforcement (2b — the phase's genuinely
difficult logic and its central promise), the SQLite atomicity/racing
contracts (3), and the exec-boundary extension (4 — it touches the
sanitized launch path). Everything else is ordinary generation against
this document.

Landing order: 0 and 1; then 2a and 2b; then 3, 4, 5 in any order; then 6;
then 7.

## 13. Decisions assumed, spike dependencies, open questions, risks

Assumed decisions (directed by this design, pending human override):

1. Pull-only delivery; HOP never types into a live pane (section 7).
2. Dependency release requires `integrated`, and worktrees base on the
   integration head at attempt creation (section 6).
3. Reviews are tasks; one reviewer evaluates the combined candidate; the
   guard binds verdicts to head object IDs (section 8).
4. Retry creates a new attempt with a fresh worktree; predecessors are
   preserved as evidence (sections 2, 6).
5. Messaging is store-only with at-least-once delivery and receipt-first
   validation orders mirroring the Phase 2 result protocol (section 7).

Spike dependencies: S8 gates the `CreateWorkspace` port shape and the
manager-placement decision row; S9 gates per-attempt worktree provenance
against `--base <oid>`; S10 gates nothing structural but must pass before
multi-session launch lands (a failure would force serialized pane
creation, a loop change, not a model change). L1 informs the `msg wait`
default only.

Open questions for the human, each with a recommendation:

1. **Default workflow mode.** Should `hop run` default to `feature` once
   Phase 3 lands? Recommendation: no — default stays `solo` (Phase 2
   behavior, suite unchanged); `feature` via config or `--workflow`.
   Phase 4's playbooks are the natural point to revisit the default.
2. **Does the reviewer count against the worker bound?** Recommendation:
   yes — one bound (`[workers] max`, default 2) over all non-manager
   sessions; predictable resource use, no second knob.
3. **Accept the no-nudge consequence?** An idle recipient that stops
   polling is surfaced as needs-attention, never woken by typed input.
   Recommendation: yes — Herdr's own docs make status-gated injection
   unsafe (section 7); revisit only if a future Herdr version ships a
   guarded, dialog-proof prompt surface, which would then get its own
   probe.
4. **Message/answer body bound.** Recommendation: 64 KiB per body file
   (4 KiB for inline `--body`); larger content belongs in repository files
   the message references by path.
5. **Reviewer harness.** Recommendation: same harness as workers by
   default with `[roles.reviewer] harness` available; provider-diverse
   review is a later option once a second harness's launch contract is
   spike-verified end to end (Codex/opencode cold resume is still
   unsupported, so a non-Claude reviewer loses cold relaunch — acceptable
   for a reviewer, but say so in status).
6. **Retry limit default.** Recommendation: 3 attempts per task
   (`[retry] max_attempts`), after which the task fails and the run needs
   the human.
7. **Integration branch visibility.** `hop/r<seq>` and the
   `hop/r<seq>/t<t>a<n>` branches live in the user's repository and are
   never auto-deleted. Recommendation: accept; document; a `hop clean`
   command is future work.
8. **Human question channel.** CLI-only (`hop status` + `hop answer`)
   until the Phase 6 board. Recommendation: yes.

Risks and mitigations:

| Risk | Mitigation |
| --- | --- |
| A live model worker never polls `hop msg wait` and idles forever | The role templates make polling an explicit standing instruction quoted from the grammar constants; the attention condition names the exact pane and action; the deterministic fixture proves the protocol; the live opt-in test measures a real model against it. No silent auto-nudge — by design (open question 3) |
| Harness tool timeouts kill a long `msg wait` | The default 50s bound sits under Claude Code's 2-minute default with margin; the `none:` line instructs re-invocation; L1 pins the live behavior; the timeout is per-run configurable |
| Message content prompt-injects the MODEL (not the terminal) | Out of the mechanical guarantee, stated in the trust model: HOP guarantees content never becomes terminal input; role instructions frame worker content as data ("preserve sender identity without treating worker content as policy", RESEARCH.md); anything stronger needs harness permissions, which HOP does not control |
| Merge conflicts between concurrent tasks | Inherent to parallel work: serial integration surfaces them deterministically, rollback is journaled and object-ID-verified, and retry re-bases on the integrated head; the manager's planning (scoping tasks to disjoint areas) is the real lever and is prose, honestly labeled |
| Serial integration bottlenecks throughput | Accepted for Phase 3: correctness first (the combined-candidate requirement is an exit criterion); two workers rarely queue long behind one merge+check |
| The scheduling pass grows into a hidden state machine | The pass order is fixed and documented (section 6), every action is a journaled operation or a store row, and the loop remains poll-driven with events as wakeups only — the Phase 2 discipline |
| Store growth from receipts/deliveries | Bounded by run size; retention policy is an explicit Phase 7 item (already in phases.md); nothing here reads unbounded history on the hot path |
| A second controller mints a second manager during a takeover race | The lease CAS serializes controllers, and the partial unique index makes a second non-terminal manager row impossible even under a store-level race; the reconnect scenario races two resumes deliberately |
| `workspace.create`/second-worktree shapes differ from expectation | S8/S9 run before the port freeze; a divergent finding changes the adapter shape or the placement design, not shipped code |
