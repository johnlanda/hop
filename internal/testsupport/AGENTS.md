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
  (`MessagingStore`, `PlanStore`), consumed by `internal/app`'s tests today
  and by `internal/adapters/sqlite`'s once that adapter lands.

## Rules specific to this subtree

- A package here may import `internal/app` and the domain packages
  (whatever its own vectors need to construct), never `cmd/hop` or another
  adapter package — the same inward-import direction ordinary `test-helper`
  code follows (`internal/arch_test.go`'s category matrix).
- A package here never bootstraps a run, session or store handle itself:
  that setup is inherently per-backing-store (a fake's direct field
  seeding versus a real store's SQL inserts). It supplies request VALUES
  and documents the refusal a correct store must produce; the consuming
  test drives the actual port call and setup.
- Every package here needs its own `internal/arch_test.go` rule row
  (`category: categoryTestHelper`) and an entry in `internal/app`'s (or
  whichever consumer's) `testFirstParty` list before that consumer's test
  files may import it.

## Related guides

- [Parent index](../AGENTS.md)
- [Architecture and package catalog](../../docs/architecture/architecture.md)
