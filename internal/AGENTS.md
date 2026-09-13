# internal

## Purpose

Two roles share this directory. As a Go package, `internal` holds HOP's
repository-wide checks: the architecture checker over import boundaries and
the guide checker over AGENTS.md coverage. It contains test files only and is
never imported. As a grouping directory, it is the parent of the application
and adapter packages, and of every future domain package.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [tree_test.go](tree_test.go) | `moduleRoot`, `readTreeFile`, `goModDirective`, `isExcludedDirectory`, `excludedComponent`, `sourceFile`, `sourceTree`, `walkSources`, `parseSourceFile`, `hasBuildConstraint`, `writeFiles` | Source inventory shared by both checkers: parses every Go file outside `repos/`, `vendor`, `testdata` and hidden or underscore-prefixed directories (so `.worktrees/` too), including tests, generated and build-constrained files; fails on a nested module, a parse error or an empty tree |
| [arch_test.go](arch_test.go) | `category`, `rule`, `ruleTable`, `productionRules`, `validateRules`, `inFamily`, `diagnostic`, `listedPackage`, `listedError`, `isBuildConstraintExclusion`, `goList`, `checker`, `excludedRoot`, `loadDiagnostics`, `checkTree`, `forbiddenSelectors`, `fixtureRules`, `writeFixtureModule`, `TestArchitectureRuleTableIsValid`, `TestArchitectureRealTree`, `TestArchitectureThirdPartyFamilyMatching`, `TestArchitectureDefaultPackageName`, `TestArchitectureFixtures`, `TestArchitectureCleanFixtureCompiles` | Import and purity rules: exact per-package allowlists, category matrix, `go list -deps -test` resolution and production closure, physical excluded-tree check on listed packages, load-error diagnostics, ambient clock and randomness check, synthetic positive and negative fixtures |
| [guides_test.go](guides_test.go) | `guideIssue`, `guideDirectories`, `markdownDocument`, `parseMarkdown`, `bracketSpan`, `inlineDestination`, `stripMarkdownCode`, `listContentIndent`, `blankInlineCode`, `resolveLink`, `findGuides`, `checkGuides`, `TestGuidesRealTree`, `TestGuidesDirectoryMap`, `TestGuidesMarkdownLinks`, `TestGuidesLinkResolution`, `TestGuidesFixtures` | Guide coverage: an AGENTS.md in the root, every package directory and every grouping directory; immediate-child links among the rendered links; resolvable inline, image, reference-use and definition targets in every AGENTS.md, with code blocks, code spans and escapes read literally |

## Child guides

- [internal/app](app/AGENTS.md): use cases and consumer-owned ports.
- [internal/adapters](adapters/AGENTS.md): adapter index; currently the
  Herdr adapter.

The remaining grouping directory the architecture proposes
(`internal/domain`) receives an index guide, and its packages receive leaf
guides, in the change that creates them.

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
- Excluded trees are enforced physically as well as by import path: a listed
  package whose directory lies inside `repos/`, `.worktrees/`, `vendor`,
  `testdata` or another excluded directory of the repository, such as a
  third-party module pulled in through a `replace` directive, is rejected at
  every file that imports it and wherever it enters a production closure.
- Imports the main listing does not reach (those in build-constrained files
  the host never compiles) are listed with `-e`. An import the go tool cannot
  resolve, or whose dependency it cannot load, is a diagnostic at each
  importing file, quoting the go tool's message. A package whose files are
  all excluded by build constraints on this host is not an error: it exists,
  and the build matrix compiles it where its constraints select it.
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
  carry an AGENTS.md whose rendered links reach each immediate child guide.
  Rendered links are inline links, images, and full `[text][label]`,
  collapsed `[label][]` and shortcut `[label]` reference uses resolved through
  the guide's definitions; a definition alone renders nothing and never
  counts as navigation, though its target must still resolve. Every such
  local target in every AGENTS.md outside the excluded trees must exist, and
  a full or collapsed reference use without a definition is an issue. Links
  into `repos/` are skipped because reference checkouts may be absent.
- Markdown handling is a purpose-built scanner, not a CommonMark renderer.
  Handled: fenced code blocks (three or more backticks or tildes, closed by a
  run of the same character at least as long); indented code blocks (a line
  after a blank line indented four columns beyond the content column of the
  most recent list item, or four columns outside a list; a tab counts to the
  next multiple of four); inline code spans (matching backtick-run length,
  never across a blank line, an unmatched run is literal); backslash escapes
  of ASCII punctuation; balanced brackets inside link and reference text,
  with a bracketed span that is not itself a link scanned inside so an
  enclosed link is still found; destinations in angle brackets or bare up to
  whitespace or an unbalanced `)`, with an optional quoted or parenthesized
  title; list items marked by `-`, `+`, `*` or a number with `.` or `)`,
  whose continuation paragraphs after a blank line are prose; lines holding
  only spaces or tabs as blank lines; CR LF line endings. Tab handling is
  indentation measurement only (a tab advances to the next multiple of
  four): a list marker is recognized with space indentation and a space
  after the marker, so a tab-indented or tab-separated marker is not a list
  item. Limits: a reference label may not contain brackets; only one line of
  list nesting is tracked (a less-indented non-list line leaves the list);
  blockquotes, HTML blocks, setext headings, autolinks and multi-line
  definitions are not modeled, so links inside those constructs are read as
  ordinary text; an angle-bracket destination must stay on one line.

## Dependencies and ports

- Allowed inward imports: none. Rule `internal` has category
  `architecture-test`; the tests use the standard library and the `go` tool.
- Consumed/implemented ports: none.
- External libraries: none.

## Verification

- `make arch` — `go test -count=1 -run '^TestArchitecture' ./internal`:
  real-tree check with a nonempty inventory, rule-table validity, family
  matching, the fixture table, and `go vet` of the clean fixture so its
  passing is evidence about a tree that type-checks.
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
