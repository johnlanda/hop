# test

## Purpose

Test-only packages that sit outside the production tree: cross-boundary
suites with their own enumerated import rules. Production closures never
reach anything under this directory.

## Guides

| Directory | Guide | Suite |
| --- | --- | --- |
| `integration/` | [test/integration](integration/AGENTS.md) | Real-process Herdr suite against a disposable server |

## Rules for this subtree

- Every package here has category `integration-test` (or another test-only
  category) with an exact rule in
  [internal/arch_test.go](../internal/arch_test.go).
- Suites that need external binaries skip with an explicit reason when the
  binary is absent; a skip is never reported as a pass.

## Related guides

- [Root guide](../AGENTS.md)
- [Plugin testing plan](../docs/architecture/plugin-testing.md)
