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
```

## Invariants

- `[check] command` is required: a policy without a check cannot gate
  completion, and Load fails naming the file.
- Defaults are exactly the design's: timeout 10m, repeatable false,
  harness claude; only `claude`, `codex` and `opencode` are accepted as
  harnesses (the `app.Harness*` constants).
- The profile directory is resolved to an absolute, cleaned path against
  the repository root at load — never later, so the frozen policy cannot
  carry a working-directory-dependent path — and a value containing a
  control byte (any byte below 0x20) is rejected, matching what
  `app.EnvPolicy.Validate` enforces when the policy is frozen. An absent
  or empty value stays `""` and selects the harness's default profile.
- Wire shapes stay here: callers receive `app.RunPolicy` values only, and
  returned slices are clones of the decoded document.
- Errors are actionable: strict-decoding failures name the offending key
  (`check.repetable`, `check.timeout`, …) and validation failures name the
  file and the rejected value.

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
  (named), duplicate key and duplicate table, unparsable/zero/negative/
  wrongly-typed timeout, unsupported harness and control bytes in the
  profile dir (escaped-sequence and literal); a missing file wrapping
  fs.ErrNotExist; context cancellation.
- Test fixtures: none on disk; policy files are written under t.TempDir by
  the tests.

## Related guides

- [Parent index](../AGENTS.md)
- [Application layer and ports](../../app/AGENTS.md)
- [Native harness compatibility](../../../docs/architecture/native-harness-compat.md)
