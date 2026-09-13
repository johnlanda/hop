# cmd

## Purpose

Executable entrypoints. Each subdirectory is one binary's composition root:
the only place that constructs concrete adapters and injects them into
application services. Nothing imports a `cmd` package.

## Guides

| Directory | Guide | Binary |
| --- | --- | --- |
| `hop/` | [cmd/hop](hop/AGENTS.md) | The `hop` command: `version`, `doctor` and `plugin-context` |

## Rules for this subtree

- A composition root may import the application package and concrete adapter
  packages; it never holds business rules.
- Every new binary directory gets a `composition` rule in
  [internal/arch_test.go](../internal/arch_test.go) and its own AGENTS.md in
  the change that creates it.

## Related guides

- [Root guide](../AGENTS.md)
- [Internal packages](../internal/AGENTS.md)
