# internal/testsupport

## Purpose

Parent directory for HOP's shared, cross-package test-support code:
declarations importable by more than one package's own `_test.go` files,
so a fact worth testing twice (a fake and a real adapter refusing the
same malformed input, for instance) is expressed once. Nothing here is
production code — every package in this subtree carries the
`test-helper` category and stays out of every production import closure
(`internal/arch_test.go`'s production-closure check enforces this
mechanically, not just by convention).

## Child guides

- [internal/testsupport/storevectors](storevectors/AGENTS.md): shared
  refused-input vectors for the Phase 3 worker-authority store ports
  (`MessagingStore`, `PlanStore`, `ReviewStore`) and the controller's
  worktree repository, consumed by both `internal/app`'s and
  `internal/adapters/sqlite`'s tests.
- [internal/testsupport/runnervectors](runnervectors/AGENTS.md): the
  `app.CommandRunner` capture contract (output bounds, truncation flags,
  a negative bound refused) as shared vectors plus `BoundCapture`, the
  reference bound every handwritten runner fake applies; consumed by
  `internal/adapters/process`'s, `internal/app`'s and
  `internal/adapters/sqlite`'s tests.
- [internal/testsupport/hopfixtures](hopfixtures/AGENTS.md): string-typed
  run/task/session/binding seeding through the application's store port
  interfaces, consumed solely by `cmd/hop`'s real-binary grammar contract
  tests — the one package in this subtree that DOES bootstrap store state
  itself (see below).

## Rules specific to this subtree

- A package here may import `internal/app` and the domain packages
  (whatever its own vectors or fixtures need to construct), never
  `cmd/hop` or another adapter package — the same inward-import direction
  ordinary `test-helper` code follows (`internal/arch_test.go`'s category
  matrix). This holds even though `hopfixtures` drives a real store
  through `app.StateStore`/`app.SubmissionStore`: it consumes those PORT
  INTERFACES (declared in `internal/app`), never the concrete
  `internal/adapters/sqlite` package — the caller opens the real adapter
  and passes it in structurally.
- Two seeding shapes coexist here, by design, not by drift: a VECTOR
  package (`storevectors`) never bootstraps a run, session or store handle
  itself — that setup is inherently per-backing-store (a fake's direct
  field seeding versus a real store's SQL inserts), so it supplies request
  VALUES only and the consuming test drives the actual port call and
  setup against fixture state IT already built. A FIXTURE package
  (`hopfixtures`) exists for the opposite reason: its one consumer
  (`cmd/hop`) has no CLI path to build feature-mode state at all (that is
  slice 6b's `hop run --workflow feature`, still landing), so the package
  itself drives `InitializeRun`/`UnitOfWork`/`SubmissionStore` calls to
  reach that state directly. `runnervectors` is a vector package too; it
  also carries the pure reference bound its fakes apply, and it never
  runs a command. Before adding another package here, decide which shape
  it actually is; do not blend request-vector and state-bootstrap
  responsibilities in one package.
- Every package here needs its own `internal/arch_test.go` rule row
  (`category: categoryTestHelper`) and an entry in its consumer's (or
  consumers') `testFirstParty` list before that consumer's test files may
  import it.

## Related guides

- [Parent index](../AGENTS.md)
- [Architecture and package catalog](../../docs/architecture/architecture.md)
