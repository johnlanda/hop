# Implementation planning

## Purpose

Map HOP's architecture into increments with usable outcomes, dependencies and
testable exit criteria. These are implementation proposals, not completed work.

## Map

- [Phased implementation](phases.md): build sequence, milestones and first backlog.
- [Phase 2 design](phase-2-design.md): the durable single-worker run — domain
  slice, ports, SQLite persistence, state machines, sanitizing launcher,
  result protocol, CLI surface, test plan and work breakdown.
- [Phase 3 design](phase-3-design.md): manager, workers and communication —
  task graph and bounded concurrency, pull-only durable messages, the
  built-in feature workflow with review and serial integration, migration
  002, test plan and work breakdown.
- [Phase 3 worktree retirement](phase-3-worktree-retirement.md): slice 8's
  design note — post-merge removal of a run's attempt worktrees (target
  branch, eligibility, provenance and cleanliness rules, the git transport,
  the two exec-claimable operations and their decision-table rows,
  migration 004, status rendering, test plan).
- [Architecture](../architecture/AGENTS.md): boundaries, entities and engineering contracts.
- [Terminal UX](../ux/AGENTS.md): native Herdr and optional board interactions.

Each phase must leave the application usable at its stated scope. Introduce domain
types, migrations and package guides with behavior that needs them. Exit criteria
are engineering checks, not additional user-approval requirements.
