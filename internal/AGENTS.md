# internal

## Purpose

Two roles share this directory. As a Go package, `internal` holds HOP's
repository-wide checks: the architecture checker over import boundaries and
the guide checker over AGENTS.md coverage. It contains test files only and is
never imported. As a grouping directory, it is the parent of every future
domain, application and adapter package; none exists yet.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [tree_test.go](tree_test.go) | `moduleRoot`, `readTreeFile`, `goModDirective`, `isExcludedDirectory`, `excludedComponent`, `sourceFile`, `sourceTree`, `walkSources`, `parseSourceFile`, `hasBuildConstraint`, `writeFiles` | Source inventory shared by both checkers: parses every Go file outside `repos/`, `vendor`, `testdata` and hidden or underscore-prefixed directories (so `.worktrees/` too), including tests, generated and build-constrained files; fails on a nested module, a parse error or an empty tree |
| [arch_test.go](arch_test.go) | `category`, `rule`, `ruleTable`, `productionRules`, `validateRules`, `inFamily`, `diagnostic`, `goList`, `checker`, `checkTree`, `forbiddenSelectors`, `fixtureRules`, `writeFixtureModule`, `TestArchitectureRuleTableIsValid`, `TestArchitectureRealTree`, `TestArchitectureThirdPartyFamilyMatching`, `TestArchitectureDefaultPackageName`, `TestArchitectureFixtures` | Import and purity rules: exact per-package allowlists, category matrix, `go list -deps -test` resolution and production closure, ambient clock and randomness check, synthetic positive and negative fixtures |
| [guides_test.go](guides_test.go) | `guideIssue`, `guideDirectories`, `markdownLinks`, `resolveLink`, `findGuides`, `checkGuides`, `TestGuidesRealTree`, `TestGuidesDirectoryMap`, `TestGuidesLinkResolution`, `TestGuidesFixtures` | Guide coverage: an AGENTS.md in the root, every package directory and every grouping directory; immediate-child links; resolvable local links in every AGENTS.md |

## Child guides

None yet. The grouping directories the architecture proposes (`internal/domain`,
`internal/adapters`) receive index guides, and packages receive leaf guides,
in the change that creates them.

## Invariants

- `productionRules` is exactly the set of HOP packages. A package without a
  rule and a rule without a package both fail `TestArchitectureRealTree`; a
  nested package needs its own entry because a parent entry grants nothing.
- Categories fix the import direction: `composition` sees `application` and
  adapters; `domain` sees `shared-domain` only; `application` sees domain
  categories; adapters see `application` and domain categories, never each
  other. `validateRules` rejects a table that grants anything else, a
  third-party family for domain or application code, or an effectful standard
  package for a domain category, so an allowlist edit cannot widen the
  architecture.
- Two inventories must agree: the source walk (every `.go` file, whatever its
  build constraints) decides allowlists; `go list -deps -test ./...` proves
  resolution, classifies standard packages by the go tool's `Standard`
  metadata and supplies the production closure, which may not reach a
  test-only package, the composition root, an unruled package or an excluded
  tree. Third-party family matching is slash-aware.
- Domain categories may import only allowlisted pure standard packages and
  may not reference `time.Now`, `time.Since`, `time.Until`, `time.Sleep`,
  `time.Tick`, `time.After`, `time.AfterFunc`, `time.NewTimer`,
  `time.NewTicker` or any identifier of `math/rand`, `math/rand/v2` and
  `crypto/rand`. The check is syntactic, keyed on the import's local name.
- Test files inherit their package's production allowlist plus the package
  under test and explicitly listed test helpers; test-only categories never
  enter a production closure.
- Diagnostics are sorted and name importer, file, import and rule. A tree that
  cannot be inventoried (nested module, parse error, failed listing, no
  packages, invalid table) is an error, never a pass.
- Guide rule: the root, every package directory and every grouping directory
  carry an AGENTS.md that links each immediate child guide; every local link
  in every AGENTS.md outside the excluded trees resolves; links into `repos/`
  are skipped because reference checkouts may be absent.

## Dependencies and ports

- Allowed inward imports: none. Rule `internal` has category
  `architecture-test`; the tests use the standard library and the `go` tool.
- Consumed/implemented ports: none.
- External libraries: none.

## Verification

- `make arch` — `go test -count=1 -run '^TestArchitecture' ./internal`:
  real-tree check with a nonempty inventory, rule-table validity, family
  matching and the fixture table.
- `make docs-check` — `go test -count=1 -run '^TestGuides' ./internal`:
  real-tree guides with a nonempty inventory and the fixture table.
- Test fixtures: synthetic modules are written to temporary directories by
  `writeFixtureModule` (architecture) and `writeFiles` (guides). Third-party
  imports resolve through local `replace` directives, so no network is used.
  Every rejection listed in the engineering standards has a named case in
  `TestArchitectureFixtures`, including the hidden build-constrained import,
  the generated import, the misleading prefixes, the nested module, the
  malformed source and the failed listing.

## Related guides

- [Root guide](../AGENTS.md)
- [Engineering standards](../docs/architecture/engineering.md)
- [Architecture and package catalog](../docs/architecture/architecture.md)
