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
- [Command guide](cmd/AGENTS.md): executable entrypoints; `cmd/hop` is the
  composition root.
- [Internal guide](internal/AGENTS.md): HOP packages and the architecture and
  guide checkers.
- `repos/`: reference checkouts; HOP implementation belongs outside these trees.

The repository contains the Phase 0 engineering foundation: the Go module
`github.com/johnlanda/hop`, the `cmd/hop` composition root with a `version`
command, the `internal` checker package, the Makefile targets, pinned lint and
format configuration and a CI workflow. Domain, application and adapter
packages named in the planning documents are proposed, not existing code.

## Toolchain and gate

- Go 1.27.1 is pinned by the `go` directive in `go.mod`; `make` exports
  `GOTOOLCHAIN` from it, so `make` is the reproducible entrypoint and a bare
  `go test` may run on a newer local toolchain.
- golangci-lint v2.13.2 is pinned in the Makefile alone and installed into
  `.bin/` on first use. It bundles the formatters `make fmt` applies: gofumpt
  v0.11.0 with the extra rules and goimports from golang.org/x/tools v0.49.0
  with local prefix `github.com/johnlanda/hop`. `.golangci.yml` is validated
  against that release's schema by `make lint`.
- `make check` is the non-mutating gate (fmt-check, docs-check, lint, race and
  shuffle tests) and fails on unformatted code; `make fmt` is the only target
  that rewrites files. CI runs `make check` natively on linux/amd64,
  darwin/arm64 and darwin/amd64.

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
- Use strict formatting and linting. Run focused checks during changes and
  `make check` before delivering; report a check as passing only after running it.
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
