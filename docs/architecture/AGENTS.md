# Architecture planning

## Purpose

Define HOP's dependency boundaries, engineering checks and domain model before
implementation. The Go/hexagonal/DDD direction and documentation standards are
user requirements; the concrete package and entity designs remain proposals.

## Map

- [Architecture and package catalog](architecture.md): dependency matrix, ports,
  package ownership, execution and transaction boundaries.
- [Engineering standards](engineering.md): import tests, test strategy, formatting,
  linting and progressive AGENTS.md requirements.
- [Domain model](domain-model.md): conceptual ERDs, aggregates and invariants.
- [Herdr surface audit](herdr-surface.md): API capabilities, native Agents view,
  metadata/configuration and integration limitations.
- [Native harness compatibility](native-harness-compat.md): verified Claude
  Code, Codex and opencode profile isolation and cold-resume evidence, launch
  recipes and the proposed supported restore policy.
- [Plugin testing](plugin-testing.md): upstream testing patterns and proposed
  HOP real-process, protocol and UI test layers.
- [Package guide template](package-guide-template.md): format for package guides.

Maintain consistency across these documents. Do not describe planned tests or
symbols as implemented. Diagram arrows must identify whether they mean imports
or data relationships.
