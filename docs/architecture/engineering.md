# HOP engineering plan

These are required implementation standards and proposed validation mechanisms.
No build targets or tests described here exist yet.

## Reference assessment

Reference files inspected:

- [Reference architecture test](/Users/jonathanlanda/dev/agent-router/agent-router-edge/internal/arch_test.go)
- [Reference lint configuration](/Users/jonathanlanda/dev/agent-router/agent-router-edge/.golangci.yml)
- [Reference Makefile](/Users/jonathanlanda/dev/agent-router/agent-router-edge/Makefile)

Retain explicit per-package allowlists, rejection of unknown nested packages,
negative tests of the checker itself, strict formatting, race testing and specific
lint suppression reasons. Do not inherit its UI/build tags, historical comments,
large exception lists, product conventions or dependencies.

## Executable dependency rules

Implement `internal/arch_test.go` with a table of exact package paths and allowed
first-party, standard-library and third-party imports. Read the module path from
go.mod rather than coupling the test to a placeholder repository URL.

Use two complementary inventories:

1. Parse imports in every HOP `.go` source file with `go/parser`, including tests,
   generated files and OS/build-tagged files. Do not depend on the current host's
   active file set. Skip reference checkouts, vendor and fixture data as explicit
   non-production trees. Reject unexpected nested modules that could hide HOP code.
2. Use `go list -deps -json` and its test/build metadata for supported build
   configurations to validate real package resolution and production dependency
   closure. Classify standard packages using Go's `Standard` metadata, not the
   presence or absence of a dot in a path. Fail on parse, listing or load errors.

Go documents package and test metadata in [go list](https://pkg.go.dev/cmd/go#hdr-List_packages_or_modules).
The source walk sees imports in inactive files; build-matrix compilation verifies
that supported variants actually type-check. Unsupported combinations should not
be presented as tested. Pin an initial OS/architecture matrix when adapter support
is selected; do not inherit the reference project's desktop/e2e tags.

Requirements for the checker:

- Unknown packages and nested packages fail; a parent rule grants no permission.
- First-party paths are exact. Third-party package-family matching is slash-aware:
  `example.org/lib` may allow itself and `/sub`, never `example.org/library`.
- Production imports may not reach `cmd`, tests, fixtures or reference repos.
- Domain siblings, outward domain/app dependencies and sibling adapter imports fail.
- Pure-domain rules reject effectful standard packages, not only third-party SDKs.
- Apply equivalent architectural boundaries to same-package and external-package
  unit tests, adding only explicit test helpers/target-package imports. A domain
  test requiring SQLite belongs in `test/integration`, not an exception in domain.
- Integration tests have their own enumerated permissions. Production closure
  must exclude integration packages and test helpers.
- Check generated production imports; generation is not a boundary exception.
- Report importer, file, imported path and violated rule; sort diagnostics.

Table-driven fixtures must demonstrate rejection of: unknown top-level/nested
package; domain→app/adapter; app→adapter; adapter→adapter; effectful stdlib in domain;
hidden build-tag import; test-only violation; generated import; misleading import
prefix; unexpected module; malformed source and failed listing. Include positive
cases for inward imports, domain-local helpers, composition and approved adapter
libraries. Pair synthetic fixtures with a nonempty real-tree check so a broken
enumeration cannot pass by checking zero packages.

Imports alone cannot prove purity: `time.Now` is available through an otherwise
useful time import. Add narrow AST/type-aware forbidden-call checks for ambient
clock/ID creation in domain code, plus review and behavior tests. Do not claim
that the import checker proves every runtime architectural property.

## Testing

See [plugin testing](plugin-testing.md) for the real Herdr process harness,
plugin-registry isolation and the distinction between protocol and UI evidence.

Prefer named table-driven subtests for validation, transitions, eligibility,
round-robin rotation, gate validity and error mapping. Use explicit setup, action
and expected outcomes; avoid generic test DSLs. Use standalone scenario tests
when a recovery sequence is clearer than a table. Prefer external `package x_test`
for public behavior; same-package tests are appropriate for internal algorithms.

| Layer | Required evidence |
| --- | --- |
| Domain | Pure transitions, invalid values, invariant preservation, stale/duplicate results, deterministic selection |
| Application | Handwritten port fakes; effects and ordering, cancellation, persistence failures and recovery decisions |
| SQLite | Real temporary DB; atomic reservations across separate connections, revisions, constraints, reopen/recovery and migrations |
| Herdr | Protocol fixtures, occupant replacement, missing events, ambiguous prompt/launch outcomes; opt-in live smoke |
| Native harness | Profile isolation, login failures and refresh ownership; opt-in authenticated smoke |
| Workflow runner | Exit/error mapping, cancellation, retries, check-input hashes and blocking behavior |
| End-to-end | One implementation/review/check workflow, stop/resume and crash recovery |

Use fake clocks rather than sleeps, channels/barriers for concurrency assertions,
and temporary directories for fixtures. Parallelize independent cases only. Do not
use `t.Parallel` with process-global env/cwd changes. No live credentials or network
are required for the normal test suite. Run `go test -race -shuffle=on ./...` on
supported native environments. Test build-tag variants separately; do not force
mutually exclusive tags on together. Fuzz parsers/IDs where malformed input matters.
Benchmark scheduling and large task graphs when scale becomes material; no blanket
benchmark requirement for every package or documentation change.

## Formatting, linting and comments

Pin the Go toolchain, golangci-lint and formatter versions during the tooling
bootstrap. Keep developer and CI invocations identical. Use gofumpt strict extra
formatting plus goimports with HOP's final module path as the local prefix.
Validate the configuration against the pinned linter schema. Current formatter
docs mark `extra-rules` deprecated in favor of named `extra` settings; do not copy
that key blindly from the reference. [Formatter configuration](https://golangci-lint.run/docs/formatters/configuration/)

Proposed explicit linter set (`default: none`): `govet` with shadow/nilness,
`staticcheck`, `errcheck`, `errorlint`, `revive`, `gocritic`, `gosec`, `misspell`,
`unused`, `ineffassign`, `bodyclose`, `noctx`, `contextcheck`, `unparam`,
`gochecknoglobals`, `gochecknoinits`, and `nolintlint`. Confirm names/settings against
the pinned release. Rules and settings are documented in the official
[linter configuration](https://golangci-lint.run/docs/linters/configuration/).

No package-wide security exclusions by default. Process execution is expected in
adapters but still deserves local, specific suppressions when a finding is false.
Each suppression identifies the linter and the current invariant that makes the
site acceptable. Immutable sentinel errors may have narrow global-variable
exceptions; prefer local tables in tests. Avoid arbitrary function-length rules
that reward fragmentation rather than clear responsibility.

Comments use Go doc-comment shape and explain behavior or a non-obvious invariant:

```go
// ReserveAccount selects an eligible account and records its session lease.
// The pool cursor and lease are committed in the same transaction.
```

Do not narrate prior behavior, fixes, review conversations or decision dates.
Architecture rationale belongs in docs. Linters check spelling and comment shape;
semantic declarative style still needs review. A future comment check should parse
comments, not scan string literals for dates or URLs.

Proposed build targets:

| Target | Contract |
| --- | --- |
| `make fmt` | Intentionally rewrite HOP Go formatting |
| `make fmt-check` | Report formatting differences without rewriting |
| `make arch` | Import/purity rules and checker fixture tests |
| `make docs-check` | Guide coverage, immediate-child navigation, broken local links |
| `make lint` | Pinned linter, vet, module tidiness check |
| `make test` | Race/shuffle tests including architecture checks |
| `make check` | Non-mutating fmt-check, docs-check, lint and tests |

Scan only HOP source, excluding `repos/`. `make check` must fail on unformatted
code rather than silently format it. Authenticated/live tests are separately named
and opt-in. Once bootstrap exists, run focused checks during changes and the full
gate before delivering implementation work. Do not report plan-only checks as run.

## Progressive AGENTS.md contract

Each Go package, including commands and integration-test packages, gets a local
guide in the same change that creates the package. Root and grouping directories
have short maps to immediate child guides. A leaf links only to its direct ports,
domain contracts and relevant tests. Expected reading path:

```text
AGENTS.md
  → internal/AGENTS.md
    → internal/adapters/AGENTS.md
      → internal/adapters/sqlite/AGENTS.md
```

Leaf guides describe purpose, key entities, actual exported entrypoints, important
files, invariants, dependencies and focused tests. Use the
[package guide template](package-guide-template.md). Target roughly 60–120 lines
per leaf, fewer for a small package; avoid repeating ancestor standards. `doc.go`
provides Go package documentation; AGENTS.md provides an agent's working map.

The planned guide checker inventories HOP Go packages, requires guide coverage and
ancestor links, and checks local links. It excludes `repos/`, vendor and fixture
directories without excluding actual HOP test packages. Symbol references should
be checked with Go AST where expressed in a machine-checkable form. Tests cannot
prove prose completeness; reviewers verify that the quick-reference map is useful.
