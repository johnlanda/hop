# internal/domain

## Purpose

Parent directory for HOP's domain modules: pure business rules and shared
identity values, with no adapters, no application services and no
third-party dependencies. Source imports stay inward per
[architecture.md](../../docs/architecture/architecture.md)'s import-direction
table.

## Child guides

- [internal/domain/identity](identity/AGENTS.md): shared typed IDs, pure
  parsing.
- [internal/domain/run](run/AGENTS.md): `Run`, `Task`, `Attempt`, `Session`,
  `RuntimeBinding`, `Worktree`, `Result` and `Artifact` entities and their
  pure state transitions.

## Rules specific to this subtree

- Domain packages import only other domain categories per the
  architecture's category matrix: `identity` imports nothing first-party;
  other domain packages (`run`, and later `workflow`, `account`) import
  `identity` only, never each other.
- No package here reads a clock, generates an ID, or imports an effectful
  standard package; [internal/arch_test.go](../arch_test.go) enforces both
  the import allowlist and the ambient-clock/randomness ban syntactically.
- No SQL, JSON or TOML struct tags on any domain type.

## Related guides

- [Parent index](../AGENTS.md)
- [Architecture and package catalog](../../docs/architecture/architecture.md)
- [Domain model](../../docs/architecture/domain-model.md)
