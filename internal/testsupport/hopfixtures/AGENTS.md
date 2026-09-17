# internal/testsupport/hopfixtures

## Purpose

Seeds runs, tasks, attempts, sessions and bindings directly through the
application's store port interfaces (`app.StateStore`, `app.SubmissionStore`,
`app.UnitOfWork`, `app.WorkflowRepositories`) for exactly one consumer:
`cmd/hop`'s real-binary grammar contract tests
(`docs/plan/phase-3-design.md` section 11, L1569;
`cmd/hop/grammarcontract_*_test.go`). Those tests exec the built `hop`
binary against a fixture state root and must never start a Herdr server
or call `hop run` (which places a worker, or with `--workflow feature`
the manager, through Herdr) — the only CLI bootstrap of a feature-mode
run — so the fixture state has to be built directly against the store,
through the SAME
`internal/adapters/sqlite` production package `cmd/hop/compose.go`
wires — this package makes that possible without `cmd/hop`'s test files
ever importing a domain type, which `internal/arch_test.go`'s
composition-category rule forbids even in `_test.go` files.

No other package should import this one: it is shaped entirely around
`cmd/hop`'s grammar contract fixtures (fixed cosmetic binding/claim
values, a deterministic `uid(seed)` identity scheme, a `Store` interface
that happens to match exactly what those fixtures need), not a general
seeding library.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [hopfixtures.go](hopfixtures.go) | `Store`, `Base`, `Initialize`, `LaunchBase`, `ErrBaseAlreadyLaunched`, `RunBase`, `SeedLaunchClaim`, `SeedLaunchClaimWithSeedEvidence`, `SeedManager`, `SeedImplementTask`, `SeedReviewTask`, `SeedChildSession`, `SeedInterruptedAttempt`, `RunAttempt`, `SeedIntegratedTask`, `SeedAttemptWorktree`, `SeedCheckEvidence`, `FinishRun`, `FeatureBase`, `InitializeFeature`, `LaunchManager`, `SettleManagerLaunch` | String-typed seeding API: `Initialize` mirrors `InitializeRun`'s own initial reserved state; `LaunchBase`/`RunBase` drive a run's bootstrap task/attempt/session to launching, then fully running (binding, settled launch claim, running lifecycle transitions); `SeedManager` drives a run running with an active, bound manager session (call after `cmd/hop`'s own raw-SQL workflow freeze — see below); `SeedImplementTask`/`SeedReviewTask`/`SeedChildSession`/`RunAttempt` add worker/reviewer sessions bound to tasks and attempts; `SeedInterruptedAttempt` records a terminal prior attempt (the shape `hop task retry` accepts behind a needs-rework task); `SeedLaunchClaim` writes a launch claim under a caller-chosen pid without any lifecycle transition, so a real `hop launch` exec against the same identity observes a claim under a DIFFERENT pid than its own process — the real-binary proof of hop launch's claim-conflict refusal, reached without ever letting a launcher exec; `SeedLaunchClaimWithSeedEvidence` also records caller-given trust-seed evidence (a pre-binding claim `hop status` renders). Worktree retirement ([phase-3-worktree-retirement.md](../../../docs/plan/phase-3-worktree-retirement.md)): `SeedIntegratedTask` seeds an implement task whose result was accepted through `SubmitResult` and whose integration row was driven merging → checking → integrated over caller-given pre-merge and merge commits (the task left integrated); `SeedAttemptWorktree` records an attempt's worktree the way the assignment act does, as a succeeded `worktree.create` operation in the store's persisted JSON shape plus the linked row, in one unit of work; `FinishRun` drives a running run to completed, failed or stopped and releases its lease. `SeedCheckEvidence` records a failed `check.run` operation (exit 1, a caller-given detail) and its retained stdout evidence artifact at a caller-given path in one unit of work — the retained check a status test renders under a hostile state root. A freshly started feature run (LAUNCH-1's window): `InitializeFeature` goes through the store's feature `InitializeRun` with the caller's frozen workflow (a reserved manager, no task, attempt or worktree); `LaunchManager` commits the bootstrap's manager launch intent (run and manager launching, a pending `pane.open` naming the manager's incarnation) and then its recorded outcome (the binding, the operation succeeded); `SeedLaunchClaim` with no attempt then leaves the manager's claim `exec_pending`; `SettleManagerLaunch` applies the corroboration's commit (run running, claim execed with the occupant pid and marker, the binding observed, the manager active) |

## Invariants

- `LaunchBase` is single-use per `Base`, not idempotent: once the run has
  left `created`, a second call — or `RunBase` after `LaunchBase`, since
  `RunBase` launches the Base itself — is refused with
  `ErrBaseAlreadyLaunched` before anything is written (pinned against the
  real store by `cmd/hop`'s `TestHopfixturesLaunchBaseIsSingleUse`).

- Every exported parameter and return value is a plain string or a type
  declared in `internal/app` (`app.Lease`, the `Store` interface). No
  function here accepts or returns `internal/domain/identity` or
  `internal/domain/run` types — the entire reason this package exists is
  to keep those types out of `cmd/hop`'s test files, which the
  architecture checker forbids from importing them (composition category,
  enforced on `_test.go` files too).
- This package does NOT write `run_snapshots.workflow` (the frozen
  feature-mode policy the solo `InitializeRun` this package drives leaves
  NULL; the feature `InitializeRun` creates its own manager session and
  refuses the solo base `Initialize` seeds). That one raw
  SQL write against the store file stays in `cmd/hop`'s own test file
  (`grammarcontract_seed_test.go`'s `freezeWorkflowSnapshot`), by the
  manager's own ruling: composition code may import any standard package
  (`database/sql`), and the sqlite driver is registered transitively by
  `cmd/hop`'s own `internal/adapters/sqlite` import — this package has no
  reason to know the driver exists at all, since its own `Store`
  parameter is satisfied structurally by whatever the caller opened.
  `SeedManager` must be called AFTER that freeze; it does not itself
  touch the snapshot row. `InitializeFeature` is the other way to a
  feature run: the store's own feature `InitializeRun` writes the
  workflow it is given, and the caller's `IntegrationBranch` must name
  the sequence the store assigns (`app.IntegrationBranchName(1)` in a
  fresh state root).
- `Store` composes exactly `app.StateStore` and `app.SubmissionStore` —
  the two port interfaces every fixture function needs (unit-of-work
  access, and the launcher's pre-exec claim write). The real
  `internal/adapters/sqlite.Store` implements both, among the other Phase
  3 ports `cmd/hop` wires; this package never imports that adapter
  itself — a `test-helper` package's architecture-checker category may
  depend on application and domain code only, never a driven adapter
  (`internal/arch_test.go`'s category matrix) — so `cmd/hop`'s test files
  open the real store and pass it in.
- `uid(seed)` is a pure function of one integer to one canonical-shaped
  UUID string; callers space their own seeds a safe distance apart
  (multiples of 1000 clear this package's own internal per-call offsets)
  so two fixtures built in the same test never collide. This mirrors
  `internal/adapters/sqlite`'s own test fixtures' `uid`/`specStride`
  convention, independently, since that convention lives in `_test.go`
  files this package cannot import.
- Every seeding function opens and commits (or rolls back) its own unit
  of work; none batches multiple fixture calls into one transaction. This
  keeps the exported surface simple at the cost of a few extra round
  trips — fine for fixture setup, never a pattern production code should
  copy.

## Dependencies and ports

- Allowed inward imports: [internal/app](../../app/AGENTS.md),
  [internal/domain/identity](../../domain/identity/AGENTS.md),
  [internal/domain/run](../../domain/run/AGENTS.md).
- Consumed/implemented ports: consumes `app.StateStore` and
  `app.SubmissionStore` (via the `Store` interface) and, through a unit of
  work, `app.WorkflowRepositories`; implements none.
- External libraries: none.

## Verification

- This package has no `_test.go` file of its own: every function is
  exercised only as `cmd/hop`'s grammar contract tests' own fixture setup
  (`go test ./cmd/hop -run TestGrammarContract` and the package's full
  suite), which is where a seeding defect would surface — there is
  nothing meaningful to assert about a fixture builder in isolation from
  the real commands it sets up state for. The feature launch helpers are
  pinned by `TestGrammarContractManagerVerbsWhileLaunching`, whose raw
  read asserts the run, manager and claim states each helper leaves.

## Related guides

- [Parent index](../AGENTS.md)
- [cmd/hop](../../../cmd/hop/AGENTS.md): the sole consumer.
- [Application layer and ports](../../app/AGENTS.md)
- [SQLite store](../../adapters/sqlite/AGENTS.md): the production
  adapter `cmd/hop`'s tests open and pass in as this package's `Store`.
