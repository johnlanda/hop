# Package AGENTS.md template

Copy the following structure when creating a real package. Replace placeholders
with existing symbols/files and executable checks. Do not leave speculative
entities in a guide for implemented code.

```markdown
# <import path>

## Purpose

<Responsibility, owning domain/use case, and boundary.>

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [actual.go](actual.go) | `ActualType`, `ActualFunction` | <Current behavior> |

## Invariants

- <Business or adapter contract that changes must preserve.>
- <Identity, ordering, lifecycle or transaction constraint.>

## Dependencies and ports

- Allowed inward imports: <exact packages; link to their guides>.
- Consumed/implemented ports: <symbols and links to definitions>.
- External libraries: <current dependencies and why this adapter needs them>.

## Verification

- `<focused executable command>` — <behavior verified>.
- Test fixtures: <paths, setup and important edge cases>.

## Related guides

- [Parent index](../AGENTS.md)
- <Only directly relevant consumer/provider guides.>
```

For a grouping directory, use purpose, an immediate-child guide table and any
rules specific to that subtree. Do not duplicate every leaf's symbol index.
