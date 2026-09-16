# internal/adapters/config

## Purpose

Repository-policy driven adapter: loads `.herdr-orchestrator/config.toml`
at a repository root into the `app.RunPolicy` that `hop run` freezes into
the run snapshot. Decoding is strict TOML 1.0 via
`github.com/pelletier/go-toml/v2` — unknown keys, duplicate keys and type
mismatches fail with the offending key named — because a hand-rolled
parser would be an undocumented TOML subset.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [config.go](config.go) | `Source` | `app.ConfigurationSource`: reads the fixed path under the given root, decodes strictly into adapter-local wire structs, validates, applies the design's defaults and resolves the profile directory absolute |

## Policy file

The complete shape, at `<repository root>/.herdr-orchestrator/config.toml`:

```toml
[check]
command = ["sh", "check.sh"]  # required; Load fails without it
timeout = "10m"               # Go duration string; default "10m"; must be positive
repeatable = true             # default false — the section 7 unknown-outcome rule

[env]
strip = ["MY_ORG_PROXY_TOKEN"]     # additions to the harness strip matrix
passthrough = ["OPENAI_API_KEY"]   # opt-in passthrough

[worker]
harness = "claude"            # default "claude"; only claude, codex, opencode

[profile]
dir = ".profiles/claude-alt"  # optional; resolved absolute against the repository root at load

# Phase 3 feature-mode additions (docs/plan/phase-3-design.md section 3).
# Every section below is optional; a file without them loads to the exact
# Phase 2 policy with every Phase 3 field zero.

[workflow]
mode = "feature"              # "solo" (also the absent default) | "feature"

[workers]
max = 2                       # default 2 (feature mode); >= 1; bounds every non-manager session

[retry]
max_attempts = 3              # default 3 (feature mode); >= 1; per task

[messages]
attention_after = "120s"      # default 120s (feature mode); positive Go duration
wait_timeout = "50s"          # default 50s (feature mode); hop msg wait's default bound

[roles.manager]
instructions = "roles/orchestrator.md"   # required in feature mode

[roles.implementer]
instructions = "roles/implementer.md"    # required in feature mode

[roles.reviewer]
instructions = "roles/reviewer.md"       # required in feature mode
harness = "claude"            # default: the [worker] harness (feature mode)
```

## Invariants

- `[check] command` is required: a policy without a check cannot gate
  completion, and Load fails naming the file.
- Defaults are exactly the design's: timeout 10m, repeatable false,
  harness claude — and a default applies only when the key is absent. An
  explicitly supplied value is always validated: `timeout = ""` and
  `harness = ""` are rejections, never silent defaults. Only `claude`,
  `codex` and `opencode` are accepted as harnesses (the `app.Harness*`
  constants).
- The profile directory is resolved to an absolute, cleaned path against
  the repository root at load — never later, so the frozen policy cannot
  carry a working-directory-dependent path — and a value containing a
  control byte (any byte below 0x20) is rejected, matching what
  `app.EnvPolicy.Validate` enforces when the policy is frozen. An absent
  or empty value stays `""` and selects the harness's default profile.
- The Phase 3 sections are shape-validated whenever PRESENT regardless of
  mode — `hop run --workflow feature` can override a solo-mode file
  (design section 10), so a file may carry them without setting
  `mode = "feature"`, and a bad value fails at load either way — while
  their defaults and the feature-mode requireds (the three role paths;
  the bounds and durations) are applied and enforced only under
  `mode = "feature"`, through the shared app helpers the override path
  calls too (`app.ApplyWorkflowDefaults`, `app.ValidateFeaturePolicy`),
  so the two paths can never disagree. An absent `[workflow]` loads the
  zero mode (solo semantics); an explicit `"solo"` is kept; any other
  mode, an explicitly empty one included, is rejected. Role instruction
  paths resolve relative to `.herdr-orchestrator/` at load (absolute
  values pass through cleaned), exactly like `profile.dir`, and reject
  control bytes. `IntegrationBranch` is NOT a policy key: it derives from
  the run's sequence at freeze (`app.WorkflowSnapshot`).
- Wire shapes stay here: callers receive `app.RunPolicy` values only, and
  returned slices are clones of the decoded document.
- Errors are actionable and never disclose: every diagnostic names the
  file, the position or key (`check.repetable`, `check.timeout`, …) and
  the expected shape or supported set — and never echoes the supplied
  value or a decoder message that could quote document content, so a
  secret mistakenly pasted into the policy file cannot leak through a
  rendered error.

## Dependencies and ports

- Allowed inward imports: [internal/app](../../app/AGENTS.md).
- Implemented ports: `app.ConfigurationSource` by `Source`.
- External libraries: `github.com/pelletier/go-toml/v2` (pinned in go.mod
  by this package as its first importer) — a maintained TOML 1.0 decoder
  with strict unknown-field rejection and no transitive dependencies.

## Verification

- `go test ./internal/adapters/config` — table-driven parsing: the minimal
  policy with every default, the complete policy with comments, escaped
  strings and multiline arrays, absolute profile dir cleaning, every
  harness; rejections for a missing/empty check command, unknown key
  (named), duplicate key and duplicate table, unparsable/explicitly
  empty/zero/negative/wrongly-typed timeout, unsupported and explicitly
  empty harness, a positioned syntax error, and control bytes in the
  profile dir (escaped-sequence and literal); every rejection asserted
  never to echo an embedded dummy-secret marker; a missing file wrapping
  fs.ErrNotExist; context cancellation. The Phase 3 tables
  (config_phase3_test.go, design section 11 L1579): a Phase 2 file
  leaving every workflow field zero, explicit solo, the minimal feature
  policy applying every default (reviewer harness inherited), the
  complete feature policy, absolute role paths cleaned, feature sections
  carried-not-defaulted under solo mode; rejections for unsupported and
  empty mode, zero/negative bounds, unparsable/empty/non-positive
  durations, instruction-less and empty-instruction role tables, control
  bytes in a role path, unsupported/empty reviewer harness, each missing
  feature-required role, unknown keys inside the new tables and a
  wrongly-typed bound — none echoing the dummy-secret marker; and the
  shared-helper pin (`TestWorkflowDefaultsAndRequiredsAreSharedHelpers`)
  driving `app.ApplyWorkflowDefaults`/`app.ValidateFeaturePolicy` over
  the override-path shape directly.
- Test fixtures: none on disk; policy files are written under t.TempDir by
  the tests.

## Related guides

- [Parent index](../AGENTS.md)
- [Application layer and ports](../../app/AGENTS.md)
- [Native harness compatibility](../../../docs/architecture/native-harness-compat.md)
