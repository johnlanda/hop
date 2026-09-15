# internal/domain/identity

## Purpose

Shared strongly typed identity values genuinely crossing HOP's domain-module
boundaries, with pure parsing. This package holds no generic helpers,
business services or persistence; a domain-local ID stays with its owner
until another module needs it.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [id.go](id.go) | `RepositoryID`, `RunID`, `TaskID`, `AttemptID`, `SessionID`, `WorktreeID`, `ResultID`, `ArtifactID`, `OperationID`, `IncarnationID`, `MessageID`, `ReviewID`, `IntegrationID`, `Parse<Kind>ID`, `String`, `ErrInvalidID` | Thirteen distinct string-backed ID types, each with a pure `Parse<Kind>ID(string) (<Kind>ID, error)` and a `String() string`. `MessageID`, `ReviewID` and `IntegrationID` are Phase 3 additions, added the same way as the other ten. |

## Invariants

- Canonical form only: 32 lowercase hexadecimal digits grouped 8-4-4-4-12
  and hyphen-separated. A mixed- or upper-case string, a wrong group length,
  a wrong separator or any non-hex character is `ErrInvalidID`; nothing is
  normalized.
- Every ID type is a distinct Go type; there is no shared underlying "ID"
  type or implicit conversion between them (`RunID` and `TaskID` are never
  interchangeable, even given the same underlying value).
- IDs are generated only through the application's `IDGenerator` port; this
  package only validates and wraps a raw string.
- Parsing never touches a clock or a random source. `strings` and `strconv`
  are the only non-trivial imports; no third-party UUID package.

## Dependencies and ports

- Allowed inward imports: none (`shared-domain` category; standard library
  only: `errors`, `fmt`, `strconv`, `strings`).
- Consumed/implemented ports: none.
- External libraries: none.

## Verification

- `go test ./internal/domain/identity` — table-driven `Parse<Kind>ID` tests
  (valid canonical UUIDs; malformed, mixed-case, wrong-length and
  wrong-separator rejections) and `String()` round-trips, for every one of
  the thirteen ID types.
- `go test -fuzz FuzzParseRunID -fuzztime 10s ./internal/domain/identity`
  (also `FuzzParseSessionID`, `FuzzParseIncarnationID`, `FuzzParseMessageID`,
  `FuzzParseReviewID`, `FuzzParseIntegrationID`) — no input panics parsing,
  and every accepted string is a canonical lowercase UUID that round-trips
  through `String()`.

## Related guides

- [Parent index](../AGENTS.md)
- [internal/domain/run](../run/AGENTS.md): the sole consumer of these IDs.
- [Architecture and package catalog](../../../docs/architecture/architecture.md)
