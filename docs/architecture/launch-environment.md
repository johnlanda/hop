# Launch environment compatibility

## Purpose and scope

To launch a worker in a selected account or profile context, HOP must set an
explicit environment on the shell that will host the harness. This document
records which Herdr launch surfaces carry that environment, which do not, and
the one limitation that shapes every recipe, each with a verified
compatibility result. Per-harness profile directories, credential handling
and account-correct cold resume are the subject of
[native harness compatibility](native-harness-compat.md); this document covers
only the Herdr launch surface underneath them.

Verified against `herdr 0.9.0` (socket protocol 22) on macOS. The verifying
tests live in [test/integration/launchenv_test.go](../../test/integration/launchenv_test.go);
they run under `make check` on a machine with the herdr binary and skip with
an explicit reason where it is absent.

## The launch sequence

Herdr's `agent.start` runs a harness in an existing shell pane; it takes a
pane, kind, args and timeout but no environment map, and it never creates
layout. So HOP sets the launch environment when it creates the shell, then
starts the harness in that shell. The environment is applied only to the newly
launched process.

## Compatibility results

| Launch surface | Carries an env map | Compatibility result |
| --- | --- | --- |
| `workspace.create` | Yes | Supported: the workspace's root pane shell sees the env. Verified. |
| `tab.create` | Yes | Supported: the new tab's root pane shell sees the env. Verified. |
| `pane.split` | Yes | Supported: the new pane's shell sees the env. Verified. |
| `layout.apply` pane nodes | Yes (per pane node) | Supported by schema; a declarative tab can set per-pane env. Not exercised in the current suite. |
| `worktree.create` | No | Unsupported at the request: it has no env field. Open a tab or pane **inside** the worktree with env, then `agent.start` there. |
| `agent.start` | No | Unsupported: it has no env field. The env must already be on the shell before the harness starts. |

For a worker in a new worktree, the env-carrying part of the path is
therefore: `worktree.create` (no env) → open a tab or split a pane in that
worktree with the launch env. Each of those steps is one of the supported
rows above. What runs inside that pane is not `agent.start` — see
[Current policy](#current-policy) below, which the additive-only limitation
in the next section motivates.

## The additive-only limitation

The env map is additive. It adds variables to the launched shell, but:

- **Herdr-managed variables are authoritative.** Herdr injects
  `HERDR_SOCKET_PATH`, `HERDR_ENV`, `HERDR_WORKSPACE_ID`, `HERDR_TAB_ID` and
  `HERDR_PANE_ID` into managed pane processes and overrides any conflicting
  caller value. An attempt to set `HERDR_PANE_ID` through the launch env is
  silently ignored; Herdr's own value wins. Verified.
- **The map cannot unset an inherited variable.** It expresses key→value
  additions only, with no removal. A variable already present in the server's
  environment (for example a stray `OPENAI_API_KEY` in the developer's shell)
  is inherited by the launched harness and cannot be removed through the map.

A clean-slate launch that must remove an inherited variable therefore requires
a sanitizing launcher argv rather than the env map alone — for example an
`env -u OPENAI_API_KEY … <harness>` command, or a small HOP wrapper executable
that fixes the environment immediately before exec'ing the harness. The
[native harness compatibility](native-harness-compat.md) document treats this
as a precondition for account-correct launches and marks recipes that rely
only on the additive env map as conditional until such a launcher is verified
end to end.

## Current policy

HOP's actual worker-launch path does not call `agent.start`. It opens a tab
or pane with the launch env (one of the supported rows above), then runs the
sanitizing launcher there as a plain command — not through `agent.start`'s
harness-kind launch — which strips credential variables, applies the
resolved profile env, and execs the harness. `agent.start` is not part of
this path; see [architecture](architecture.md) and
[native harness compatibility](native-harness-compat.md) for the launcher
and the profile decision it applies.

## What this does not cover

Account and profile isolation (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, opencode's
profile root), credential storage, onboarding and login flows, and
account-correct cold resume are all in
[native harness compatibility](native-harness-compat.md). HOP does not yet
build the worker-launch use case or the harness-profile adapters; this phase
establishes only the launch-surface compatibility the later use case will
rely on.
