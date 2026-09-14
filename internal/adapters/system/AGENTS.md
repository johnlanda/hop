# internal/adapters/system

## Purpose

Local-machine driven adapter for the application's smallest effect ports:
the real UTC clock, crypto-random identity minting and durable artifact
file writes. Application code never reads a clock, randomness or the
filesystem ambiently; it consumes these ports.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [clock.go](clock.go) | `Clock` | `app.Clock` over the wall clock, normalized to UTC |
| [idgenerator.go](idgenerator.go) | `IDGenerator` | `app.IDGenerator`: crypto/rand UUIDv4 in canonical lowercase form |
| [artifactstore.go](artifactstore.go) | `ArtifactStore` | `app.ArtifactStore`: temp-file-then-rename writes with fsync, parent-directory creation and 0600 files; whole-file reads |

## Invariants

- Every `Now` is UTC, so persisted timestamps and domain inputs carry one
  location.
- Every `NewID` is a version-4, RFC 4122-variant UUID in canonical
  lowercase form — exactly the form the `internal/domain/identity` parsers
  accept — from crypto/rand; the generator panics rather than minting an
  identity if the platform's randomness source fails (crypto/rand
  documents that it never does on supported platforms).
- A reader never observes a partial artifact: content lands in a temp file
  inside the destination directory, is pinned to mode 0600 and then
  fsynced (so the file sync covers the final mode as well as the data),
  and one atomic rename publishes it; the destination directory entry is
  fsynced after the rename. Every failure path removes the temp file, so
  no `.hop-artifact-*` file is left behind.
- The failure guarantee is exactly what rename atomicity gives: a failure
  before the rename leaves the destination in its previous state; a
  failure after it (the directory fsync) may leave the complete new
  content whose durability is not yet established, and the caller must
  treat such an outcome as uncertain durability, never as a rollback.
  The destination is never anything partial.
- Missing parent directories are created (0700) with each new entry
  fsynced into the directory that holds it, walking from the deepest
  already-existing ancestor, so a first write into a fresh hierarchy is
  durably reachable when WriteArtifact returns. Paths are chosen by the
  application from the run's frozen artifact directories, never by this
  adapter.
- `WriteArtifact` is never called from inside a store transaction (the
  port's contract): the use case commits intent first, writes as the
  external act, then records the outcome.

## Dependencies and ports

- Allowed inward imports: [internal/app](../../app/AGENTS.md),
  [internal/domain/identity](../../domain/identity/AGENTS.md) (granted for
  the canonical-form contract; the tests parse minted IDs through it).
- Implemented ports: `app.Clock` by `Clock`, `app.IDGenerator` by
  `IDGenerator`, `app.ArtifactStore` by `ArtifactStore`.
- External libraries: none; standard library only.

## Verification

- `go test ./internal/adapters/system` — clock UTC-ness and monotonic
  bracketing; 1000 minted IDs each canonical (regexp), unique and accepted
  by `identity.ParseRunID`; artifact round trips (text, empty, binary)
  with directory creation, mode 0600 and no temp residue; overwrite
  replacing content completely; a failed publish (rename onto a directory)
  leaving no temp file; deep-hierarchy creation with every new directory
  0700; a file occupying the directory chain rejected by name; a
  missing-artifact read wrapping fs.ErrNotExist. Crash-ordering itself
  (what survives power loss mid-sequence) is not provable by these tests;
  the sequence above is the documented mechanism.
- Test fixtures: none on disk; paths live under t.TempDir.

## Related guides

- [Parent index](../AGENTS.md)
- [Application layer and ports](../../app/AGENTS.md)
- [Identity values](../../domain/identity/AGENTS.md)
