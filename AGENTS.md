# HOP — Herdr Orchestrator Plugin

## Purpose

HOP is a Go, terminal-native orchestration plugin for single-developer workflows
on Herdr. It manages runs, tasks, agent sessions, playbooks, gates, messages and
account pools. CLIProxyAPI is reference material for account management, not a
model-routing dependency.

## Project map

- [Research](RESEARCH.md): workflow scope and source observations.
- [Documentation guide](docs/AGENTS.md): planning documents and navigation.
- [Architecture guide](docs/architecture/AGENTS.md): package boundaries, engineering
  standards and conceptual entity relationships.
- `repos/`: reference checkouts; HOP implementation belongs outside these trees.

The repository currently contains research and architecture plans. Go packages,
build targets, lint configuration and architecture tests are not implemented yet.
Package paths and symbols in planning documents are proposed, not existing code.

## Engineering rules

- Use Go with hexagonal architecture and domain-driven design. Source dependencies
  point inward. Only the composition root wires concrete adapters together.
- Domain packages contain business invariants; application services coordinate
  use cases through consumer-owned ports. Adapters translate external systems.
- Enforce package import boundaries with automated tests. Every Go package needs
  an explicit rule; unknown packages fail validation.
- Prefer table-driven behavior tests and deterministic clocks/IDs. Exercise
  persistence and concurrency guarantees with real adapter integration tests.
- Comments describe current behavior, contracts and invariants. Keep code history,
  dates, attribution to reviews and decision narratives out of code comments.
- Use strict formatting and linting. The engineering plan defines the proposed
  checks; report checks as passing only after their implementation and execution.
- Every Go package must have a local AGENTS.md with purpose, entities, actual
  symbols/files, dependency constraints and focused verification instructions.
  Ancestor AGENTS.md files link to their immediate children. Update the affected
  package guide with changes to its responsibility or public surface.
- Do not copy unrelated conventions or product-specific lint exemptions from the
  reference repositories. Their instructions apply within their own directories.

## Working context

Read this file, the ancestor guides for the target path and that package's guide.
Follow specific port/entity links as needed; do not load every sibling guide.
Create package guides with packages, not speculative empty package directories.
