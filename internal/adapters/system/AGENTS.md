# internal/adapters/system

## Purpose

Local-machine driven adapter for the application's smallest effect ports:
the real UTC clock, crypto-random identity minting, durable artifact
file writes and the workspace-trust profile-config seed. Application code
never reads a clock, randomness or the
filesystem ambiently; it consumes these ports.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [clock.go](clock.go) | `Clock` | `app.Clock` over the wall clock, normalized to UTC |
| [idgenerator.go](idgenerator.go) | `IDGenerator` | `app.IDGenerator`: crypto/rand UUIDv4 in canonical lowercase form |
| [artifactstore.go](artifactstore.go) | `ArtifactStore` | `app.ArtifactStore`: temp-file-then-rename writes with fsync, parent-directory creation and 0600 files; whole-file reads |
| [trustseed.go](trustseed.go) | `TrustSeeder` | `app.TrustSeeder`: a serialized, atomic read-modify-write of a Claude profile's `.claude.json` through `app.SeedTrustEdit` — exclusive flock of the advisory lock file beside the config, temp+rename at 0600 with file and directory fsync |

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
- Every write establishes the whole parent chain's durability, not only
  what it created: each directory on the destination's parent chain is
  created if missing (0700) and fsynced — pre-existing directories
  included, because a directory's existence proves nothing about the
  durability of an entry an earlier, failed attempt created — so a retry
  after a failed parent fsync re-syncs exactly the directory whose sync
  failed. Paths are chosen by the application from the run's frozen
  artifact directories, never by this adapter.
- `WriteArtifact` is never called from inside a store transaction (the
  port's contract): the use case commits intent first, writes as the
  external act, then records the outcome.
- `SeedWorkspaceTrust` serializes concurrent hop launch processes on an
  exclusive flock of `<config>.hop-trust-seed.lock` (created 0600, never
  removed — unlinking a lock file another process may have opened would
  let two later lockers hold different inodes), polls under the caller's
  context while another holder exists, and publishes with the same
  temp+rename+fsync discipline as artifacts. An absent or unparsable
  config is a not-seeded outcome — the file is never created or
  rewritten, so the seed cannot bootstrap a profile — and a document
  already carrying true writes nothing at all. Every hard error carries a
  fixed category established through errors.Is only; the profile path
  derives from environment values and is never echoed.

## Dependencies and ports

- Allowed inward imports: [internal/app](../../app/AGENTS.md),
  [internal/domain/identity](../../domain/identity/AGENTS.md) (granted for
  the canonical-form contract; the tests parse minted IDs through it).
- Implemented ports: `app.Clock` by `Clock`, `app.IDGenerator` by
  `IDGenerator`, `app.ArtifactStore` by `ArtifactStore`, `app.TrustSeeder`
  by `TrustSeeder`.
- External libraries: none; standard library only.

## Verification

- `go test ./internal/adapters/system` — clock UTC-ness and monotonic
  bracketing; 1000 minted IDs each canonical (regexp), unique and accepted
  by `identity.ParseRunID`; artifact round trips (text, empty, binary)
  with directory creation, mode 0600 and no temp residue; overwrite
  replacing content completely; a failed publish (rename onto a directory)
  leaving no temp file; deep-hierarchy creation with every new directory
  0700; a file occupying the directory chain rejected by name; a
  missing-artifact read wrapping fs.ErrNotExist; and, through the
  directory-sync recording seam, a retry after an injected parent-sync
  failure re-fsyncing that same parent and every chain directory even
  though they already exist. Crash-ordering itself (what survives power
  loss mid-sequence) is not provable by these tests; the sequence above is
  the documented mechanism.
- The same command's `TestTrustSeeder*` cases cover the seed adapter:
  byte-exact seeding with unknown fields and other projects preserved,
  false→true overwrite, an already-true document left untouched (bytes
  and mtime), absent config/profile-directory and malformed-config
  not-seeded outcomes with nothing created or rewritten, the exact key
  landing through a symlinked profile path, eight concurrent seeds of
  distinct keys all surviving (run under -race), a held lock blocking
  until the context ends and proceeding after release (flock scopes to
  the open file description, so a second descriptor contends exactly as a
  second process would), value-free permission-failure errors on both
  sides of the lock, no temp residue, mode 0600, and a canceled context
  writing nothing.
- Test fixtures: none on disk; paths live under t.TempDir.

## Related guides

- [Parent index](../AGENTS.md)
- [Application layer and ports](../../app/AGENTS.md)
- [Identity values](../../domain/identity/AGENTS.md)
