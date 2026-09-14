# Testing HOP against Herdr

Static investigation of the local Herdr checkout and its plugin documentation.
No tests were executed for this investigation. Distinguish public plugin-author
guidance, Herdr's own internal tests, and proposed HOP test infrastructure.

## Available surfaces

Herdr plugins are ordinary executable packages, not an embedded-language SDK.
The inspected documentation does not provide a dedicated plugin test runner or
public mock server. Plugins can test their own code normally, then exercise the
real Herdr binary through CLI/socket APIs.

Public authoring guidance is to build locally, `plugin link` the working directory,
list/invoke actions, open plugin panes and inspect plugin command logs. Linking
does not execute build commands. See [plugin documentation](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/plugins.mdx).

Herdr's own test surfaces provide concrete examples:

| Surface | Existing evidence | What it demonstrates |
| --- | --- | --- |
| In-process runtime tests | [Plugin API tests](../../repos/herdr/src/app/api/plugins/mod.rs) | Manifest validation, context/env injection, startup/event commands, disabled plugins, command limits, link handlers and pane ownership |
| Real process / CLI integration | [Plugin CLI tests](../../repos/herdr/tests/cli/plugins.rs) | Link/list/invoke/log/pane/unlink flows, installation failures and global registry behavior |
| Headless and PTY process fixtures | [CLI harness](../../repos/herdr/tests/cli/harness.rs) | Spawn an isolated real server, wait for socket readiness, issue CLI calls and clean up owned processes |
| Fake IPC receiver | [Hook tests](../../repos/herdr/tests/cli/hooks.rs) | Execute hook scripts and assert emitted request payloads without a full server |
| Portable example manifest | [Smoke fixture](../../repos/herdr/tests/fixtures/plugin-smoke/herdr-plugin.toml) | Small action, event and pane entrypoints |
| Live reproduction guidance | [Throwaway reproduction guide](../../repos/herdr/.agents/skills/herdr-throwaway-repro/SKILL.md) | Disposable named session, real panes/PTYs, API-driven transitions and captured evidence |

The in-process tests are Rust tests internal to Herdr, not an API HOP can import
from Go. Reuse the external contracts and fixture ideas rather than embedding
Herdr's AppState or requiring its entire source build for normal HOP development.

## Recommended integration workflow from Herdr's guidance

The local reproduction guide recommends a unique disposable named session when
testing real panes, PTYs or agents. Inspect the installed binary's help/version
first, drive the session using CLI/API operations, obtain IDs from responses, use
bounded waits instead of arbitrary sleeps, record before/after evidence and clean
up only resources created by the test. The default/main session is not a test target.

For interactive reproduction from inside Herdr, it uses an outer disposable pane
with a temporary attached TUI to supply geometry. It explicitly enables nesting
in test-only configuration and clears inherited socket/session/caller IDs. This
is separate from upstream automated headless process spawning; do not copy its
headless environment clearing as a bypass for interactive nesting checks.

Important isolation scopes:

- Named sessions isolate runtime state, panes, sockets and session persistence.
- Plugin registration is user-global. Named sessions still share it by default.
- A config-file override alone does not isolate all user-global storage.
- HOP tests should use temporary config, state and runtime roots, plus a test-only
  config path, on every subprocess. Unix upstream fixtures use XDG overrides;
  verify corresponding Windows paths before claiming portable test isolation.
- Clear inherited Herdr socket/session/caller overrides before explicitly targeting
  the test server. Do not overwrite the developer's native harness profiles.

The real CLI smoke test creates a temporary plugin and workspace, invokes an
action, reads plugin logs, opens a pane and unlinks the plugin. HOP should also
wait for command completion and assert the actual output/artifacts: a successful
action invocation response is not evidence the action finished successfully.

## Proposed HOP test layers

1. Fast Go domain/application tests with table-driven cases and handwritten port
   fakes. No Herdr binary or credentials required.
2. Herdr adapter protocol tests against a small fake NDJSON endpoint. Cover response
   IDs, errors, partial frames, disconnects, interleaved events and unknown optional
   fields. Such a fake checks HOP behavior, not Herdr compatibility.
3. Real headless Herdr contract tests using a pinned binary, temporary roots and
   temporary Git repositories. Link the built HOP plugin, run actual entrypoints,
   inspect state/events/logs and exercise failure/recovery paths.
4. PTY-attached UI tests or repeatable visual smoke tests at fixed dimensions.
   Verify native Agent order, labels, keyboard/mouse focus, popup restoration,
   compact layouts and board closure separately from controller termination.
5. Small opt-in live harness tests for Claude/Codex auth, detection, native resume
   and account-profile isolation. Ordinary plugin tests must not require model calls.

For deterministic headless scenarios, use a fixture process that reports custom
agent identity/state through public APIs. It can validate metadata and lifecycle
integration but cannot prove native Claude/Codex screen detection or OAuth behavior.
Similarly, successfully setting an Agent projection does not prove its rendering;
that belongs in PTY/UI evidence.

## First real-process suite

| Scenario | Required assertions |
| --- | --- |
| Link and invoke | Manifest accepted; action receives correct plugin/state/context paths; command completes |
| Startup | Link before server launch; hook runs on startup, not merely on link; restore remains idempotent |
| Worktree event | Hook identifies event target even when another workspace is focused |
| Native Agent view | Metadata accepted; manager/worker ordering and owned clear; another view owner remains intact |
| Board | Pane opens; board closes without stopping controller/workers |
| Registry | Test plugin remains confined to test roots despite multiple named sessions |
| Restart | Reconnect and rehydrate metadata; no duplicate work; account binding verified separately |
| Failure cleanup | Deadlines cancel clients; stop/reap only tracked child processes; retain useful logs on failure |

Use process handles and returned IDs for cleanup, never broad process matching.
Track subprocess ownership with Go test cleanup handlers and report cleanup failures.
Record binary/version, test config, request/response traces, plugin logs and pane
snapshots. Pin both server/client versions for the UI suite; later run compatibility
checks against HOP's minimum supported and current stable Herdr versions.

Herdr's contributor commands are `just test`, `just check` and focused
`just test-one <filter>`; those validate Herdr itself. HOP's fake-endpoint
protocol tests live in `internal/adapters/herdr` and the first real-process
suite lives in `test/integration` (see its guide), both running under the
ordinary `make check`; the real-process suite skips with an explicit reason
where no herdr binary is installed, and CI runners have none. Of the table
above, the link-and-invoke and startup rows are implemented; the remaining
rows and the live-harness tests are planned, not implemented.
