# internal/adapters/sqlite

## Purpose

The persistence adapter behind the application's `StateStore`, `UnitOfWork`
(with `WorkflowRepositories`), `ReadStore` (with `WorkflowReadStore`),
`SubmissionStore`, `MessagingStore`, `PlanStore` and `ReviewStore` ports
(plus `WorktreeRetirementRepositories` on the same unit of work):
one SQLite database at
`<state root>/hop.db` shared by every repository and run, holding runs,
their frozen snapshots (incl. the feature-mode workflow policy), tasks
with their dependency edges, attempts, sessions (roles, optional attempts,
one-level parents), runtime bindings, launch and check-exec claims,
worktrees, results, submission receipts, check requests (typed subjects),
messages with their deliveries, acks and receipts, reviews and their
receipts, integrations, retry requests, workflow receipts, artifacts,
transition evidence, the operation journal and the controller leases. The driver is the pure-Go `modernc.org/sqlite`, pinned
at v1.58.0 in `go.mod` by this package as its first importer. The caller
(cmd/hop's single state-root resolver) hands `Open` an absolute state root;
this package never resolves environment variables or defaults.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [sqlite.go](sqlite.go) | `Store`, `Options`, `Open`, `Close`, `dsn`, `inWriteTx`, `formatTime`, `parseTime`, `newUUID` | Two pools onto one file (immediate-write and read), DSN-applied per-connection settings, bounded whole-transaction busy retry, canonical fixed-width UTC timestamps, adapter-internal UUID minting |
| [migrations.go](migrations.go) | `ErrFutureSchema`, `migrate`, `applyMigration`, `applyRebuildMigration`, `validateLaunchClaimBackfill`, `requireCleanForeignKeys`, `loadMigrations`, `schemaVersion` | Ordered embedded migrations, each applied in its own immediate transaction with the version re-read inside it; refuses a store newer than the binary. A REBUILD migration (003) runs on a DEDICATED connection whose DSN sets foreign_keys(0) before BEGIN, validates the launch-claim session backfill in Go (real ambiguity refused NAMING the claim), requires a clean `PRAGMA foreign_key_check` before commit, and closes that connection on every path — it is never any pool |
| [migrations/001_initial_schema.sql](migrations/001_initial_schema.sql) | — | The complete Phase 2 schema: 17 STRICT tables and the partial unique indexes (one active attempt per task, one accepted result per attempt, one current session per attempt) |
| [migrations/002_launch_claim_seed_evidence.sql](migrations/002_launch_claim_seed_evidence.sql) | — | Adds `launch_claims.seed_evidence` (nullable TEXT): the workspace-trust pre-seeding outcome hop launch records with the claim; evidence only, NULL on pre-migration rows |
| [migrations/003_manager_workers_messages.sql](migrations/003_manager_workers_messages.sql) | — | The Phase 3 schema (design section 4; its text's "002"): ten new tables (task_dependencies, messages, message_deliveries, message_acks, message_receipts, reviews, review_submissions, integrations, retry_requests, workflow_receipts) with the partial unique acceptance/serialization indexes; additive columns on runs (plan_closed_at), run_snapshots (workflow), worktrees (attempt_id, base_commit) and tasks (kind/seq/title/instructions_path/retry_count/subjects/mailbox_closed_at/created_at, defaults = the solo backfill); STRICT rebuilds of sessions (attempt_id relaxed, parent_session_id, the one-manager partial index), launch_claims (session_id NOT NULL, backfilled from the binding else the historical launch intent) and check_requests (typed subject, old rows re-keyed id=result_id, subject_kind='result') |
| [migrations/004_run_worktrees_retired.sql](migrations/004_run_worktrees_retired.sql) | — | The post-merge worktree retirement run fact ([phase-3-worktree-retirement.md](../../../docs/plan/phase-3-worktree-retirement.md)): `runs.worktrees_retired_at` (nullable TEXT, no default), NULL on every pre-migration row; an additive ALTER on the ordinary migration path, no rebuild. Nothing reads or writes it yet |
| [workflow_uow.go](workflow_uow.go) | `unitOfWork` as `app.WorkflowRepositories`: `TaskDependencies`, `TaskIndex`, `AttemptIndex`, `SessionIndex`, `WorktreeIndex`, `Messages`, `Reviews`, `Integrations`, `RetryRequests`, `ManagerSession` | The Phase 3 controller-transaction repositories on the same fenced unit of work (the additive packaging rule's slice-3 half); controller review-task creation, the store-assigned enqueue sequence, the sorted PendingByAddress mailbox-closure snapshot, the serial integration slot, and `WorktreeIndex().ByAttempt` (the newest row linked to an attempt, in attemptWorktreePath's order; `app.ErrNotFound` otherwise) |
| [messaging.go](messaging.go) | `SendMessage`, `FetchNextMessage`, `AckMessage`, `AnswerQuestion`, `insertMessageReceipt`, `acceptedMessageReceipt`, `sendRequestDigest` | The section 7 worker-authority messaging port: request-ID receipts first, the caller session's OWN run and current incarnation re-derived per verb, derived answer destinations, the bundled human-question ack, receipts for every outcome except the deliberately receipt-free empty fetch. Only an ordinary send checks the run state (`Run.CanAcceptManagerVerb`: a `transient` receipt while the run can still reach running, `refused-run-not-accepting` once it never will); fetch, ack and both answer paths have no run-state gate |
| [plan.go](plan.go) | `CreateTask`, `RequestRetry`, `ClosePlan`, `insertWorkflowReceipt`, `acceptedWorkflowReceipt`, `requireManagerCaller`, `runAcceptanceReason` | The section 8 worker-authority plan port: manager-only verbs, run-state-gated after the caller checks (`Run.CanAcceptManagerVerb`: a `transient` receipt and nothing else while the run can still reach running, `refused` with `run-not-accepting` once it never will), the retry's successor attempt reserved in the accepting transaction (its outcome carries the task's seq and the attempt number, re-read on a receipt replay), the plan flag set/cleared on runs.plan_closed_at, one authoritative acceptance per (run, verb, request ID) |
| [review.go](review.go) | `SubmitReview`, `persistVerdictAcceptance`, `reviewerSessionEligible` | The section 8 worker-authority verdict write: SubmitResult's order mirrored, acceptance persisting the review, completing attempt and task, closing the mailbox and committing the controller's reasons-bearing manager notice atomically |
| [workflow_read.go](workflow_read.go) | `LoadSessionLaunchContext`, `LoadMessagingContext`, `LoadMessageDetail`, `worktreePathForAttempt`, `attemptWorktreePath`, `featureRunDetail`, `mailboxStatuses`, `guardShortfalls`, `pendingQuestions` | The Phase 3 lease-free reads (`app.WorkflowReadStore`): session-addressed launch context (binding else the SESSION-keyed pending intent, fail closed; the attempt row, worktree path and Relaunch successor fact), the messaging context, hop msg show's detail, and RunDetail's feature extensions. The launch context's worktree comes from the newest row linked to the attempt, else the run's only row while that row is unlinked, else ""; a row linked to another attempt is never served, and several rows are never guessed among. The status task table uses the linked row alone |
| [messages.go](messages.go) | `parseAddress`, `scanMessage`, `getMessage`, `messagesByAddress`, `nextEnqueueSeq`, `insertMessage`, `messageDeliveries`, `messageAck`, `resolveSessionAddress` | Shared message row mapping: Message.State reconstructed from the delivery/ack rows in the same snapshot (never a persisted column), the per-(run, recipient) FIFO sequence, lineage-based address resolution |
| [statestore.go](statestore.go) | `InitializeRun`, `AcquireLease`, `Heartbeat`, `ReleaseLease`, `Begin`, `validateLease` | The controller authority: run bootstrap in one transaction (the snapshot's workflow JSON round-tripped, NULL for solo; a feature spec inserts the run, snapshot, manager session and lease only, refusing `app.ErrFeatureRunSpecInvalid` before the transaction and `app.ErrRunSequenceMismatch` inside it), lease CAS with monotonic generations, fenced unit-of-work begin |
| [uow.go](uow.go) | `unitOfWork` and the typed repositories (`Runs`…`CheckExecClaims`), `OperationRepository.Pending`/`ByKind`, `Commit`, `Rollback` | One immediate transaction per unit of work; optimistic-concurrency saves; append-only bindings and transitions; journal payloads persisted as uninterpreted JSON; controller-side launch-claim settlement |
| [entities.go](entities.go) | `getRun`, `getTask`, `getAttempt`, `getSession`, `currentSession`, `currentBinding`, `getWorktree`, `scanWorktree`, `selectWorktreeColumns`, `getLaunchClaim`, `acceptedResult`, `incarnationCurrent`, `launchIncarnationCurrent`, `pendingLaunchIntent` | Row ↔ domain-value mapping shared by all three authorities through the `querier` interface (`getWorktree` and the listing's `scanWorktree` map NULL `attempt_id`/`base_commit` to the solo row's empty links; a worktree's `state` column is read verbatim, including the retirement states `removed`/`absent`/`released`) |
| [submission.go](submission.go) | `SubmitResult`, `RecordMalformed`, `ClaimLaunch`, `SettleLaunchFailure`, `ClaimCheckExec`, `RequestStop`, `insertReceipt` | The worker authority: the section 7 validation order with the domain's `AcceptResult` inside one transaction, receipts for every outcome, the pre-exec claim contracts, the monotonic stop request |
| [readstore.go](readstore.go) | `ListRuns`, `LoadRunStatus`, `LoadFrozenRun`, `LoadCheckExecutionContext`, `lastCheckSummary`, `retirementIntentExecution` | Lease-free reads, each inside one deferred read transaction for a consistent WAL snapshot; `LoadRunStatus` carries the snapshot's frozen `TargetBranch` and the run's `worktrees_retired_at` fact, and for a feature run `featureWorktreeDetail` adds every worktree row (oldest first, non-nil) and the run's `worktree.retire` operations (newest first, through the shared `operationsByKind`) |
| [worktreeretirement_read.go](worktreeretirement_read.go) | `Store` as `app.RetirementReadStore`: `ListRetirementCandidates`, `terminalUnretiredRuns`, `retirementCandidateRecord`, `collectIntegrations` | The worktree-retirement triage read, in one read transaction. It returns the repository's completed, failed and stopped runs whose fact is unset, whose frozen workflow is feature mode with a target, and that integrated at least one row adding content, in sequence order. Each record carries its integrated rows (oldest first), its `retirement.check` operations (newest first) and whether any `retirement.check` or `worktree.retire` is pending or reconciling. An unknown root has no candidates |
| [worktreeretirement.go](worktreeretirement.go) | `unitOfWork` as `app.WorktreeRetirementRepositories`: `WorktreesForRetirement`, `WorktreesRetiredAt`, `MarkWorktreesRetired`; `runWorktrees`, `runWorktreesRetiredAt` | `WorktreesForRetirement` lists every worktree row of the leased run (another run is `ErrFenced`) in insertion order (`created_at, rowid`), any state, with its revision; row state saves go through `Worktrees().Save`. `WorktreesRetiredAt` reads the leased run's fact inside the transaction (another run is `ErrFenced`). Migration 004's worktrees-retired run fact: written once inside the fenced unit of work (the leased run only; the UPDATE applies only while NULL, so a repeat keeps the first value; a set-once housekeeping column that does not move `runs.revision`), read back as nil for NULL or the canonical time (anything else fails closed) |

## Invariants

- Every physical connection carries the DSN-applied settings
  `journal_mode(WAL)`, `synchronous(FULL)`, `foreign_keys(1)` and
  `busy_timeout(5000)`; the write pool additionally begins every
  transaction immediate. A connection-churn test proves the settings on
  freshly created connections, not just the first.
- The DSN percent-encodes the database path through net/url, so filesystem
  characters (`?`, `#`, `%`, spaces) in a state root are never read as URI
  syntax: the database always lands at exactly `<root>/hop.db`, and no
  root spelling can smuggle SQLite URI options such as `mode=memory` into
  the connection.
- A unit of work is one run's exclusive write authority: every repository
  mutation resolves its target's persisted owning run (directly, or
  through its task/attempt/session/result ancestry) and refuses a target
  of any other run with `app.ErrFenced` before any SQL executes. Only a
  lease for that run — including one taken over after expiry — can mutate
  it.
- Write transactions are retried whole (bounded, with backoff) on
  SQLITE_BUSY/SQLITE_LOCKED. This is safe because no external act ever
  happens inside a transaction — the design's transaction rule, which this
  package must never weaken.
- All IDs are canonical lowercase UUID TEXT. All times are the fixed-width
  canonical UTC form `2006-01-02T15:04:05.000000000Z`; lexical order agrees
  with time order, but every lease and expiry decision compares parsed
  times, never strings. parseTime re-renders what it parsed and rejects
  anything that does not reproduce the stored text exactly, so a
  noncanonical spelling Go's layout parsing would tolerate (a comma
  fraction, an unpadded hour) never enters a comparison.
- Mutable entity tables (runs, tasks, attempts, sessions, worktrees) carry
  `revision`; every save runs `WHERE id = ? AND revision = ?` and zero
  affected rows is `app.ErrRevisionConflict`.
- The worktree insert writes `attempt_id` and `base_commit` from the
  domain value (NULL for a solo row). A linked attempt must exist
  (`app.ErrNotFound`) and belong to the leased run (`app.ErrFenced`), the
  same ownership check the session insert applies. Every feature attempt's
  row is linked (`AssignReadyTasks`), and the launch context finds a
  session's worktree by that link.
- The lease row is created only by `InitializeRun` and never deleted or
  reinserted; its generation is monotonic for the life of the row and
  preserved across release. Heartbeat and release are CAS on (run,
  controller, generation, held); acquisition succeeds only on a released or
  expired row; `Commit` re-reads the lease inside the transaction and fails
  with `app.ErrFenced` unless it matches and is unexpired at the
  commit-time clock reading.
- `SubmitResult` runs the domain's `run.AcceptResult` inside one write
  transaction (load run/task/attempt/prior accepted result and the
  incarnation/claim context → decide → persist), and every outcome —
  accepted, duplicate, stale, conflicting, transient, malformed — commits a
  `result_submissions` receipt whose claimed identities are plain text with
  no foreign keys. Recorded outcomes return a nil error; a non-nil error is
  an infrastructure failure with nothing recorded. Acceptance persists the
  result, the accepted receipt, the unique check request and the
  attempt/task (and, for an early submission, run) transitions with their
  generation-NULL evidence rows atomically.
- `ClaimLaunch` validates agreement first — the claimed attempt's
  persisted owning run must be the claimed run, so a mixed tuple never has
  its stop or currency questions answered against the wrong run — then
  refuses when the run is stopping or stopped, the incarnation is not
  current, or a claim exists with a different run, attempt or pid. A
  same-pid retry on the same tuple is accepted only while the claim is
  exec_pending and its executable and argv digest match. It refreshes
  only seed_evidence to the retry's outcome; all invocation identity
  fields remain unchanged, and settled claims refuse retries. Currency:
  the
  attempt's current session's current binding decides when one exists;
  before ANY binding row exists for that session (the launcher is the
  pane's own command and can claim before the controller records the
  pane.open outcome), the authority is the run's newest pending launch
  operation (kind pane.open or launch.send), whose intent JSON must carry
  BOTH the claim's incarnation and the current session's id under the keys
  `incarnation_id` and `session_id` — a documented contract between the
  application (which commits the intent before dispatching the pane
  request) and this store (which reads them with `json_extract`). The
  session conjunct keeps a retired incarnation's still-pending old intent
  from authorizing a claim after a cold relaunch replaces the session, and
  a superseded binding without a successor retires the incarnation: any
  existing binding row disables the intent fallback.
  `LaunchClaims().Settle` moves exec_pending to execed or exec_failed only,
  idempotent per target state. `ClaimCheckExec` requires a pending
  operation whose kind `app.OperationKind.ExecClaimable` accepts
  (`check.run`, `integration.merge`, `retirement.check`,
  `worktree.retire` — every kind the generalized exec boundary spawns) of
  the run's current lease generation. `LoadCheckExecutionContext`
  resolves each kind's frozen argv: the snapshot's check argv, the merge
  intent's `merge_argv`/`tree_path`, or a worktree-retirement intent's
  `argv`/`cwd`; a malformed intent fails closed.
- Stop requests are monotonic: `stop_requested_at` is set once and never
  cleared or moved; `RunStatus.StopRequested` and `RunDetail.StopRequested`
  mirror it for the read model.
- `LoadFrozenRun` serves the frozen snapshot, repository root and brief
  lease-free; `RunDetail.StateRoot` carries the frozen state root and
  `RunDetail.LastCheck` summarizes the newest `check.run` operation
  whatever its state — a settled unknown outcome stays visible with its
  `Unknown` flag, detail and retained evidence paths, so an unrepeatable
  unknown result stays actionable through status.
- Phase 3 re-keying: every launch claim is keyed to the SESSION it
  launches. `ClaimLaunch` validates the claimed session's owning run
  (an attempt-only Phase 2 caller resolves the attempt's current session
  exactly as the currency read path does), currency is the session's
  current binding else the SESSION's newest pending launch intent (B2:
  two concurrently pending launches validate independently), and the
  INSERT records `session_id` (NOT NULL) with `attempt_id` NULL for an
  attempt-less (manager) claim. `LoadSessionLaunchContext` resolves the
  incarnation HOP_INCARNATION_ID must match the same session-keyed way and
  fails closed with `app.ErrNotFound`; slice 6 deleted the Phase 2
  run-keyed `LoadLaunchContext`, so this is the only launch-context read
  now — every role, the permanent solo shim included.
- Worker-authority request idempotency is receipt-first: every mutating
  messaging/plan verb resolves the (run, verb, request ID) acceptance key
  before anything else — an identical retry returns the original outcome
  as duplicate, a conflicting reuse is refused — and every outcome leaves
  a receipt row EXCEPT an empty fetch, which commits nothing. A
  `transient` receipt (a run-gated verb while the run can still reach
  running) sits outside the accepted-only partial unique index, so the
  same request retried later is decided afresh and accepted at most once.
  Message
  envelopes are immutable; `Message.State` is reconstructed from the
  delivery/ack rows, the enqueue sequence is assigned in commit order
  inside the send transaction (FIFO authority, never caller clocks), and
  a successful serve's evidence is its append-only delivery row.
- Acceptance closes the mailbox in the same commit: `SubmitResult`'s
  first acceptance re-reads the task mailbox (queued or
  delivered-unacknowledged → the retryable transient outcome with the
  drain grammar) and closes it atomically, as does `SubmitReview`'s,
  which also commits the controller's reasons-bearing info notice to the
  manager; `PlanStore.RequestRetry` is the only reopen path, and
  `taskRepository.Save` persists the flag under the revision discipline
  for the controller's failure-closure settlement.
- `runtime_bindings.server_instance` maps to
  `run.RuntimeBinding.ServerInstance` on Create and every read, empty
  string ↔ NULL. `NewRuntimeBinding` takes `serverInstance` right after
  `serverSocketPath`.
- The store receives the state root as an absolute path and refuses a
  relative one; worker-context resolution rules live in cmd/hop, not here.

## Dependencies and ports

- Allowed inward imports: [internal/app](../../app/AGENTS.md),
  [internal/domain/run](../../domain/run/AGENTS.md),
  [internal/domain/identity](../../domain/identity/AGENTS.md).
- Implemented ports: `app.StateStore`, `app.UnitOfWork` (with every typed
  repository including `LaunchClaimRepository` and
  `CheckExecClaimRepository`) plus `app.WorkflowRepositories` and
  `app.WorktreeRetirementRepositories`,
  `app.ReadStore` plus `app.WorkflowReadStore`, `app.SubmissionStore`,
  `app.MessagingStore`, `app.PlanStore` and `app.ReviewStore`, all by
  `*Store` and its unit of work. Test files additionally import
  [internal/testsupport/storevectors](../../testsupport/storevectors/AGENTS.md)
  and
  [internal/testsupport/runnervectors](../../testsupport/runnervectors/AGENTS.md)
  (production code never does; the checker's test-support rule proves it).
- External libraries: `modernc.org/sqlite` v1.58.0 (pure-Go SQLite driver;
  no cgo, exact version pinned) and its `lib` subpackage for result-code
  constants.

## Verification

- `go test ./internal/adapters/sqlite` — the real-temporary-database suite:
  migrations from empty (the Phase 3 index/column surface asserted), reopen
  at the same version, refusal of a future
  version (`ErrFutureSchema`), concurrent open/migrate from two handles, and
  a populated version-1→latest upgrade (`TestUpgradePopulatedV1StoreToV2`: the
  001 schema built raw with a full claim chain, upgraded through
  `sqlite.Open`, old values and relationships intact with NULL seed
  evidence, a new evidence-bearing claim, idempotent reopen);
  DSN escaping (roots containing `#`, `?`, `%`, spaces, Unicode and a
  mode=memory lookalike land at `<root>/hop.db` per `pragma_database_list`
  and survive reopen); connection-churn PRAGMA checks on both pools; the
  lease CAS matrix (initial lease, held refusal, takeover after expiry,
  stale heartbeat and release across handles, raced acquisition across
  handles with one winner, release preserving the generation and the row,
  post-release and post-expiry commit rejection); revision conflicts for
  every mutable entity including worktrees; unit-of-work run scoping
  (every cross-run mutation fenced; takeover of the other run grants it);
  the bounded whole-transaction busy retry against a captured real
  SQLITE_BUSY plus two-writer contention with a held immediate
  transaction; the attempt-reservation and repository get-or-create races
  across separate `*sql.DB` handles standing in for separate processes;
  constraint coverage for every unique rule; reopen mid-operation reading
  back pending operations and unsettled claims; `SubmitResult` outcomes
  (accepted, early acceptance, transient, duplicate including after the
  terminal state, conflicting, stale by incarnation, superseded binding
  and stop precedence, malformed by existence/agreement) with receipts
  asserted, plus the same-content two-handle race committing one result,
  two receipts and one check request; `ClaimLaunch` idempotence,
  different-pid rejection (raced), run/attempt agreement (mixed tuples
  refused before and after the owner stops), the pre-binding intent
  fallback (matching intent accepted, stale incarnation refused, binding
  precedence, supersession retirement, replacement-session refusal);
  `SettleLaunchFailure` and controller settlement transitions;
  `ClaimCheckExec` generation/kind/state matrix; monotonic `RequestStop`;
  the read-store loads (`LoadFrozenRun`; the session-keyed launch-context
  suite below is `LoadLaunchContext`'s successor and only remaining
  reader); `RunDetail.LastCheck` for a
  settled unknown execution with evidence paths; `StopRequested` in both
  the detail and the run list; `OperationRepository.ByKind` ordering and
  all-state coverage; and the `server_instance` NULL/non-NULL binding
  round-trip. No sleeps: a shared hand-advanced fake clock decides every
  expiry.
- The Phase 3 suites (same command): migration 003 over POPULATED
  pre-003 stores (`TestMigration003*`: active, completed, failed and
  claim-without-binding fixtures — rebuilt rows byte-for-byte where
  unchanged, session backfills from binding and historical intent, task
  defaults, re-keyed check requests, clean foreign keys — plus the three
  ambiguity refusals naming the claim with the store left unmigrated);
  the WorkflowRepositories suite (`TestTaskIndex`,
  `TestTaskDependenciesByRun`, `TestAttemptIndex`, `TestSessionIndexByRun`,
  `TestManagerSession`, `TestMessageRepository`, `TestReviewRepository`,
  `TestIntegrationRepository`, `TestRetryRequestRepository`); the
  worker-authority suites (`TestMessagingLifecycle`,
  `TestSendRefusalMatrix`, `TestUnauthorizedFetchLeavesReceipt`,
  `TestRelayedAnswerLineage`, `TestReceiptAcceptanceKey`,
  `TestHumanAnswerDigestVectors`, `TestCreateTask*`, `TestClosePlan`,
  `TestRequestRetry*`, `TestSubmitReview*`,
  `TestRunGatedVerbsAcrossRunStates` — task create, task retry, plan
  close, a relayed question and a task notice against every run state
  with and without a stop request: accepted while running, transient
  with only a `transient` receipt while created, launching, resuming or
  completing, then accepted exactly once and duplicate under the same
  request IDs once running, refused `run-not-accepting` once completed,
  failed, stopping or stopped or with a stop request; fetch, ack and an
  answer proceeding in every state); the raced contracts across
  separate handles (`TestFetchFetchRaced` — one serialized in-flight
  message, a delivery row per serve — `TestAckAckRaced`,
  `TestAnswerAnswerRaced`, `TestRequestIDReuseRaced`,
  `TestCreateTaskRaced`, `TestRetryRequestUniquePendingRaced`,
  `TestManagerUniquenessRaced`, `TestSerialIntegrationIndexRaced`,
  `TestEnqueueSequenceFIFO` with the inverted clock, the mailbox
  send/accept and send/failure-settlement races in both orders incl. the
  snapshot-mismatch retry); the session-keyed claim vectors
  (`TestClaimLaunchSessionKeyedIntents` — B2 —
  `TestClaimLaunchByAttemptResolvesSession`,
  `TestClaimLaunchManagerSession`,
  `TestClaimCheckExecAcceptsMergeOperations`); the Phase 3 reads
  (`TestLoadSessionLaunchContext` — worktree rows written through the
  production repository, including the one-unlinked-row fallback and the
  several-unlinked-rows refusal — `TestLoadMessagingContext`,
  `TestLoadMessageDetail`, `TestRunDetailFeatureExtensions`,
  `TestRunDetailSoloZeroValues`, `TestLoadCheckExecutionContextByKind`);
  the feature worktree linkage on the real store
  (`TestAssignedWorktreesLinkTheirAttempts`: `ResumeFeature` takes over a
  seeded run and `AssignReadyTasks` assigns two implementers and a
  reviewer, each row linked to its attempt and base, each Claude child
  with its own native reference, and `PrepareSessionLaunchExec` accepting
  each session from its own worktree and refusing it from a sibling's;
  `TestUnlinkedWorktreeRowsRefuseTheLaunch`, the pre-fix unlinked rows
  refusing every launch with no claim;
  `TestWorktreeFallbackServesOnlyALoneUnlinkedRow`, the run-wide fallback
  serving only the run's lone unlinked row: a lone sibling-linked row, or
  one beside an unlinked row, resolves nothing and the launch is refused;
  `TestWorktreeRepositoryAttemptLink`, the linked and solo round trips;
  `TestWorktreeIndexByAttempt`, the by-attempt lookup reading back a row
  created in the same unit of work and nothing after a rollback, beside
  the shared `WorktreeLookupByAttempt` vector).
  The harness's git fake `linkGit` follows the runner's capture contract
  (`runnervectors.ValidateBound` before answering, `BoundCapture` on
  every answer, the refuse-if-exists answers included);
  `TestLinkGitCaptureContract` runs every shared `CaptureVectors` case
  through it and pins its own common-directory answer under a negative,
  a one-byte and an exact bound;
  the feature bootstrap (`bootstrap_test.go`:
  `TestInitializeRunFeatureShape` — run, snapshot workflow JSON, a
  reserved attempt-less parentless manager with its assigned native
  reference, the lease, and no task/attempt/worktree row —
  `TestInitializeRunFeatureSecondManagerUnrepresentable` raced from two
  handles, `TestStoreVectorsFeatureBootstrap` and
  `TestInitializeRunFeatureSequenceRace`); the Phase 3 repository suites
  seed their feature runs through `initLegacyFeatureRun` (solo bootstrap
  rows under a feature workflow column), since they address the solo
  task/attempt/session as the run's first task;
  and `TestStoreVectors`, the real-store half of the shared
  refused-input contract, driving every
  [internal/testsupport/storevectors](../../testsupport/storevectors/AGENTS.md)
  vector to the identical refusal internal/app observes against its
  fakes.
- Worktree-retirement exec kinds (same command):
  `TestClaimCheckExecRetirementKinds` (both kinds claimed at the current
  generation, same-pid retry idempotent, another pid, a prior generation
  and a settled operation refused) and
  `TestLoadCheckExecutionContextRetirementKinds` (the intent's argv and
  spawn directory verbatim; missing argv, a non-string element, an empty
  argv, a missing directory and a non-object intent each fail closed).
- `TestAcquireLeaseOnTerminalRun` (same command): a failed run's released
  lease is taken under the next generation, and a unit of work under it
  records the retired fact — the store behavior the retirement pass
  relies on.
- `TestListRetirementCandidates` (same command): of eleven seeded runs,
  only the completed, failed and stopped feature runs with a target, an
  unset fact and content are listed, in sequence order. Excluded are a
  solo run, running and stopping runs, a run with no target, a retired
  run, a merging-only run, a no-op-only run, and another repository's
  run. Integrated rows come oldest first without the merging row, and
  retirement checks newest first. The unresolved flag counts only open
  retirement operations. An unknown root lists none, and the other
  repository lists its own run.
- `TestLoadRunStatusWorktreeRows` (same command): a feature run without
  rows reports an empty list; two rows come back in insertion order with
  their current states, and only the worktree.retire operations come
  back, newest first; a solo run carries neither.
- `TestWorktreesForRetirement` (same command): a run without rows lists
  none; four linked rows inserted under descending ids list in insertion
  order with attempt, base, state and revision; each retirement state
  round-trips through `Worktrees().Save`; another run is fenced.
- `TestMarkWorktreesRetired` (same command): the fact read inside a unit
  of work (unset, then the transaction's own uncommitted mark, another
  run fenced), set once through
  a committed unit of work, a repeat keeping the first value with the run
  revision unchanged, another run fenced before any write, a rollback
  leaving nothing, and a commit after lease expiry fenced with nothing
  recorded.
- Worktree-retirement reads (same command):
  `TestFrozenWorkflowWithoutTargetBranch` (a snapshot frozen before the
  field existed loads with no target; the key round-trips, and
  `TestInitializeRunFeatureShape` freezes one through InitializeRun) and
  `TestLoadRunStatusRetirementFields` (the detail's target, NULL fact as
  nil, a canonical time read back exactly, a noncanonical one refused).
- Migration 004 (same command): `TestMigration004Surface` (the chain's
  latest version, one `schema_migrations` row per migration, the column's
  nullable default-free TEXT shape, NULL for a freshly initialized run) and
  `TestMigration004UpgradesPopulated003Store` (a populated store stopped at
  the 003 boundary through `MigrateUpTo`, upgraded by `Open`: existing runs
  NULL, runs/sessions/leases byte-for-byte intact, clean foreign keys, a
  reopen applying nothing). `TestMigration003Surface` pins that version 3
  is applied; the chain's latest version is pinned only by the newest
  migration's surface test.
- `go test -count=3 ./internal/adapters/sqlite` — flake resistance for the
  raced scenarios.
- Test fixtures: none on disk; every database is created in a `t.TempDir`
  root and every identity is minted by the tests.

## Related guides

- [Parent index](../AGENTS.md)
- [Application layer and ports](../../app/AGENTS.md)
- [Run domain module](../../domain/run/AGENTS.md)
- [Phase 2 design, sections 4, 7 and 9](../../../docs/plan/phase-2-design.md)
- [Phase 3 design, sections 4, 7, 8 and 11](../../../docs/plan/phase-3-design.md)
