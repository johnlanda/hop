# internal/adapters/sqlite

## Purpose

The persistence adapter behind the application's `StateStore`, `UnitOfWork`,
`ReadStore` and `SubmissionStore` ports: one SQLite database at
`<state root>/hop.db` shared by every repository and run, holding runs,
their frozen snapshots, tasks, attempts, sessions, runtime bindings, launch
and check-exec claims, worktrees, results, submission receipts, check
requests, artifacts, transition evidence, the operation journal and the
controller leases. The driver is the pure-Go `modernc.org/sqlite`, pinned
at v1.58.0 in `go.mod` by this package as its first importer. The caller
(cmd/hop's single state-root resolver) hands `Open` an absolute state root;
this package never resolves environment variables or defaults.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [sqlite.go](sqlite.go) | `Store`, `Options`, `Open`, `Close`, `dsn`, `inWriteTx`, `formatTime`, `parseTime`, `newUUID` | Two pools onto one file (immediate-write and read), DSN-applied per-connection settings, bounded whole-transaction busy retry, canonical fixed-width UTC timestamps, adapter-internal UUID minting |
| [migrations.go](migrations.go) | `ErrFutureSchema`, `migrate`, `applyMigration`, `loadMigrations`, `schemaVersion` | Ordered embedded migrations, each applied in its own immediate transaction with the version re-read inside it; refuses a store newer than the binary |
| [migrations/001_initial_schema.sql](migrations/001_initial_schema.sql) | — | The complete Phase 2 schema: 17 STRICT tables and the partial unique indexes (one active attempt per task, one accepted result per attempt, one current session per attempt) |
| [statestore.go](statestore.go) | `InitializeRun`, `AcquireLease`, `Heartbeat`, `ReleaseLease`, `Begin`, `validateLease` | The controller authority: run bootstrap in one transaction, lease CAS with monotonic generations, fenced unit-of-work begin |
| [uow.go](uow.go) | `unitOfWork` and the typed repositories (`Runs`…`CheckExecClaims`), `OperationRepository.Pending`/`ByKind`, `Commit`, `Rollback` | One immediate transaction per unit of work; optimistic-concurrency saves; append-only bindings and transitions; journal payloads persisted as uninterpreted JSON; controller-side launch-claim settlement |
| [entities.go](entities.go) | `getRun`, `getTask`, `getAttempt`, `getSession`, `currentSession`, `currentBinding`, `getLaunchClaim`, `acceptedResult`, `incarnationCurrent`, `launchIncarnationCurrent`, `pendingLaunchIntent` | Row ↔ domain-value mapping shared by all three authorities through the `querier` interface |
| [submission.go](submission.go) | `SubmitResult`, `RecordMalformed`, `ClaimLaunch`, `SettleLaunchFailure`, `ClaimCheckExec`, `RequestStop`, `insertReceipt` | The worker authority: the section 7 validation order with the domain's `AcceptResult` inside one transaction, receipts for every outcome, the pre-exec claim contracts, the monotonic stop request |
| [readstore.go](readstore.go) | `ListRuns`, `LoadRunStatus`, `LoadFrozenRun`, `LoadLaunchContext`, `LoadCheckExecutionContext`, `launchIdentity`, `lastCheckSummary` | Lease-free reads, each inside one deferred read transaction for a consistent WAL snapshot |

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
  current, or a claim exists with a different run, attempt or pid; a
  same-pid rewrite on the same tuple is idempotent. Currency: the
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
  `check.run` operation of the run's current lease generation.
- Stop requests are monotonic: `stop_requested_at` is set once and never
  cleared or moved; `RunStatus.StopRequested` and `RunDetail.StopRequested`
  mirror it for the read model.
- `LoadLaunchContext` returns the incarnation HOP_INCARNATION_ID must match
  (`LaunchContext.IncarnationID`), not a binding: the current binding
  decides it when one exists, the session's pending launch intent
  (`incarnation_id`, gated on a matching `session_id`) otherwise, and when
  both exist they must agree — a disagreement, a malformed intent identity,
  or neither source fails closed with `app.ErrNotFound` rather than handing
  the launcher an identity nothing recorded.
- `LoadFrozenRun` serves the frozen snapshot, repository root and brief
  lease-free; `RunDetail.StateRoot` carries the frozen state root and
  `RunDetail.LastCheck` summarizes the newest `check.run` operation
  whatever its state — a settled unknown outcome stays visible with its
  `Unknown` flag, detail and retained evidence paths, so an unrepeatable
  unknown result stays actionable through status.
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
  `CheckExecClaimRepository`), `app.ReadStore` and `app.SubmissionStore`,
  all by `*Store` and its unit of work.
- External libraries: `modernc.org/sqlite` v1.58.0 (pure-Go SQLite driver;
  no cgo, exact version pinned) and its `lib` subpackage for result-code
  constants.

## Verification

- `go test ./internal/adapters/sqlite` — the real-temporary-database suite:
  migrations from empty, reopen at the same version, refusal of a future
  version (`ErrFutureSchema`), concurrent open/migrate from two handles;
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
  the read-store loads, including `LoadLaunchContext` resolving the
  incarnation from a binding, from the pending intent pre-binding, and
  failing closed on no-source, binding/intent disagreement and a
  wrong-session intent; `LoadFrozenRun`; `RunDetail.LastCheck` for a
  settled unknown execution with evidence paths; `StopRequested` in both
  the detail and the run list; `OperationRepository.ByKind` ordering and
  all-state coverage; and the `server_instance` NULL/non-NULL binding
  round-trip. No sleeps: a shared hand-advanced fake clock decides every
  expiry.
- `go test -count=3 ./internal/adapters/sqlite` — flake resistance for the
  raced scenarios.
- Test fixtures: none on disk; every database is created in a `t.TempDir`
  root and every identity is minted by the tests.

## Related guides

- [Parent index](../AGENTS.md)
- [Application layer and ports](../../app/AGENTS.md)
- [Run domain module](../../domain/run/AGENTS.md)
- [Phase 2 design, sections 4, 7 and 9](../../../docs/plan/phase-2-design.md)
