# internal/adapters

## Purpose

Driven and driving adapters: each subdirectory translates between one
external system and the application's ports. No Go package exists at this
directory; it is a grouping directory.

## Guides

| Directory | Guide | External system |
| --- | --- | --- |
| `herdr/` | [internal/adapters/herdr](herdr/AGENTS.md) | Herdr CLI and socket API |
| `sqlite/` | [internal/adapters/sqlite](sqlite/AGENTS.md) | SQLite state store (modernc.org/sqlite) |

## Rules for this subtree

- Adapters implement ports declared in [internal/app](../app/AGENTS.md) and
  never import sibling adapters.
- Wire shapes (request/response structs, JSON tags, protocol enums) stay in
  the adapter; callers receive application values or narrow result structs.
- Every new adapter package gets its own AGENTS.md, an exact rule in
  [internal/arch_test.go](../arch_test.go) and a row in the table above, in
  the change that creates it.

## Related guides

- [Parent index](../AGENTS.md)
- [Application layer](../app/AGENTS.md)
