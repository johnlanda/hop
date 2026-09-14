# Native harness compatibility: profile isolation and cold resume

## Purpose and scope

Phase 1 requires an evidence-based answer to two account questions before
phase 2 enables native cold resume: whether harness profiles can be isolated
during launch, and whether cold resume can be made account-correct (see
[phases](../plan/phases.md)). Initial harness coverage is Claude Code, Codex
and opencode; other harnesses (Gemini, Grok and the rest of Herdr's supported
agents) are deferred until their own evidence exists. This document records
the verified facts, the launch recipes they support, a proposed supported
restore policy and the open questions that need the developer's accounts.

Method: experiments in throwaway profile directories under a session scratch
directory, help/`--version` output, inspection of the installed CLI binaries'
own code with `strings`, and the local Herdr checkout. No real credential
value was read, no login was performed, and no live Herdr server was used.
The Claude Code and Codex probes were fully unauthenticated; opencode probes
ran on its built-in free model, except one probe that silently adopted the
`OPENAI_API_KEY` exported by the developer's shell — itself recorded below as
an isolation finding. Facts marked code inspection describe what the shipped
binary does by construction and still deserve one authenticated confirmation
run.

Environment for every observation below, recorded 2026-09-13 on macOS
(Darwin 25.6.0, arm64):

| Command | Observed |
| --- | --- |
| `claude --version` | `2.1.259 (Claude Code)` at observation time; `2.1.270` after a same-day auto-update |
| `codex --version` | `codex-cli 0.154.0` |
| `opencode --version` | `1.18.30` |
| `herdr --version` | `herdr 0.9.0` |

Claude Code version drift: the Claude observations and binary extracts below
were made on 2.1.259; that Caskroom directory has since been replaced by
2.1.270, so the quoted binary path no longer exists. The 2.1.259 code already
consulted `CLAUDE_SECURESTORAGE_CONFIG_DIR`, an override that selects the
credential-storage root independently of `CLAUDE_CONFIG_DIR`; any launch
environment policy must control it too. 2.1.270 behavior is not re-verified
here and needs its own pass before HOP supports it.

"Isolated profile" below means a fresh empty directory passed as
`CLAUDE_CONFIG_DIR` (Claude Code), `CODEX_HOME` (Codex) or, for opencode, a
fresh `HOME` with all four `XDG_*_HOME` directory variables either cleared or
pointed inside it (see the opencode section for why `HOME` alone is not
enough).

One cross-harness fact shapes every recipe: provider API keys in the launch
environment bypass profile isolation differently per harness. With
`OPENAI_API_KEY` and `OPENAI_BASE_URL` exported (they are exported by the
developer's shell profile), an isolated opencode profile silently used that
key (`llm.provider=openai` in its own log), while `codex exec` in an isolated
home still failed with 401, not adopting the same variable for its own auth.
The Claude Code probes below were run with `ANTHROPIC_API_KEY` explicitly
unset; its binary also adopts `CLAUDE_CODE_OAUTH_TOKEN` and
`CLAUDE_SECURESTORAGE_CONFIG_DIR` from the environment (code inspection).

Sanitizing that environment is only possible at the harness exec boundary,
not through Herdr's launch surfaces: pane/workspace creation `env` is an
additive `HashMap<String, String>`
([panes.rs](../../repos/herdr/src/api/schema/panes.rs) line 42), and
`apply_pane_launch_env` ([pane.rs](../../repos/herdr/src/pane.rs)) removes
only `CODEX_THREAD_ID` and `OMPCODE` before adding the supplied entries — it
cannot unset an inherited server variable. Panes also default to login shells
on macOS (`shell_mode_uses_login_shell`, same file), whose startup files can
re-export provider keys after any upstream sanitation. HOP therefore needs a
controlled launcher that unsets/sets the environment immediately before
exec'ing the harness (for example an `env -u OPENAI_API_KEY … <harness>`
argv, or a HOP wrapper executable); recipes that rely only on the pane env
map are conditional until such a launcher arrangement is verified.

## Claude Code

### Verified

- Profile isolation. With `CLAUDE_CONFIG_DIR` set to an empty directory,
  `claude -p 'Reply with the single word ok' --model haiku` (no
  `ANTHROPIC_API_KEY` in the environment) printed `Not logged in · Please run
  /login` even though the developer's global login exists: `security
  find-generic-password -s "Claude Code-credentials"` reports a matching
  generic-password item in the login keychain, and `~/.claude/.credentials.json`
  does not exist. An isolated profile therefore has no usable inherited
  authentication; this is a behavioral result, not a filesystem or keychain
  access trace (v2.1.259).
- First-launch writes. That failed print-mode run created, inside the profile
  directory only: `.claude.json` (mode 0600; identity and onboarding keys such
  as `firstStartTime`, `machineID`, `userID`), `projects/<munged-cwd>/`
  containing a `<session-uuid>.jsonl` transcript, `backups/` and `sessions/`
  (mode 0700). A transcript is written even when authentication fails. The
  munged directory name is the absolute working directory with separators and
  dots replaced by `-`.
- Session identity and resume lookup. Transcripts are JSONL records carrying a
  `sessionId` UUID. `claude -p --resume <uuid>` found the session from a
  different working directory in the same profile (it progressed to the
  `Not logged in` error), and an unknown UUID failed earlier with
  `No conversation found with session ID: <uuid>`. Resume lookup is scoped to
  `CLAUDE_CONFIG_DIR`, not to the working directory.
- Transcript portability. Copying the transcript file into a second fresh
  profile under the same `projects/<munged-cwd>/` path made
  `claude -p --resume <uuid>` in that profile find the session (it progressed
  to the login error). Before the copy the same command in that profile failed
  with `No conversation found`. Portability of the file format across profiles
  is real at the lookup level; an authenticated continuation is untested.
- Credential storage (code inspection via `strings` of the then-installed
  binary `/opt/homebrew/Caskroom/claude-code@latest/2.1.259/claude`; that
  path has since been replaced by 2.1.270 — see the version drift note).
  Credentials are
  a macOS keychain generic password whose account is `$USER` and whose service
  name is `Claude Code-credentials` for the default configuration directory,
  with `-<first 8 hex of sha256(config dir)>` appended when `CLAUDE_CONFIG_DIR`
  is set. Distinct profile directories therefore map to distinct keychain items
  by construction, so two profiles can hold different accounts concurrently.
  The plaintext fallback is `.credentials.json` (mode 0600) inside the profile
  directory. Token refresh is performed by the CLI itself, guarded by a
  `.oauth_refresh.lock` file inside the profile directory, so refresh is
  serialized per profile and one profile's refresh does not touch another's
  item. `CLAUDE_CODE_OAUTH_TOKEN` is accepted from the environment as an
  access token without a refresh token.
- Workspace trust. `claude --help` states the workspace trust dialog is
  skipped when Claude runs non-interactively (`-p`, or stdout not a TTY); the
  print-mode probes above ran without any prompt. Interactive trust acceptance
  is recorded per project as the `hasTrustDialogAccepted` key of the
  `projects` entry in the profile's `.claude.json` (key observed in a real
  configuration; values not read).
- Onboarding. A fresh profile's first interactive launch renders a theme
  selection screen (`Welcome to Claude Code v2.1.259` … `Choose the text style
  that looks best with your terminal`) before any prompt can be delivered.
  Capture procedure: `script -q <file> claude` under a fresh
  `CLAUDE_CONFIG_DIR` with `ANTHROPIC_API_KEY` unset, no input sent, killed
  after 12 seconds; the capture file was then stripped of ANSI sequences.
  `.claude.json` carries `hasCompletedOnboarding` and `lastOnboardingVersion`
  keys.
- Limit signals (code inspection). The binary contains the user-facing
  messages `you have reached your weekly usage limit`, `usage limit reached`
  and `new messages wait for your usage limit to reset`, and a `rate_limits`
  result block described as "Claude.ai subscription usage limits" in
  print-mode JSON output. `.claude.json` has a `cachedUsageUtilization` key.

### Unverified or unsupported

- No login was performed inside an isolated profile, so the derived keychain
  service name, the non-inheritance of the global item after login and
  concurrent refresh of two accounts are code-inspection facts awaiting one
  authenticated confirmation.
- Authenticated `--resume`, resuming under a different account than the one
  that created the transcript, and provider-side acceptance of a copied
  transcript are untested.
- Live limit and quota behavior (what a running session shows when an account
  hits its limit) was not reproduced.
- The exact post-theme onboarding sequence (login screen, trust dialog
  ordering) was not captured.

### Launch recipe

For a worker in a Herdr pane using account profile `<dir>`:

1. Create `<dir>` (once per account) and bootstrap it interactively: a human
   runs the first launch in the workspace, answering the onboarding, login
   and trust prompts for exactly that workspace root — no ancestor-path
   trust. Automated pre-seeding of `.claude.json` (`hasCompletedOnboarding`,
   theme, per-project `hasTrustDialogAccepted`) is a pending design option:
   the key names exist, but no verified seed contents suppress every
   first-run prompt, so it must not be relied on yet. Print-mode launches
   skip the trust dialog and need no bootstrap.
2. Perform the account login once, interactively, in that profile
   (`CLAUDE_CONFIG_DIR=<dir> claude` then `/login`); this is a human step and
   can be part of the same bootstrap session.
3. Launch through the sanitizing launcher described in the scope section:
   pane/tab creation `env` can add `CLAUDE_CONFIG_DIR=<dir>` (`agent.start`
   itself has no env parameter, see [Herdr surface](herdr-surface.md)), but
   it cannot unset inherited variables (`ANTHROPIC_API_KEY`,
   `CLAUDE_CODE_OAUTH_TOKEN`, `CLAUDE_SECURESTORAGE_CONFIG_DIR`), and a
   macOS login shell can re-export them; removal must happen in the argv
   that finally execs `claude`. This recipe is conditional until that
   launcher arrangement is verified end to end.
4. Record the session UUID, profile directory, working directory and account
   label as the session binding. Cold resume needs all four:
   `CLAUDE_CONFIG_DIR=<dir> claude --resume <uuid>` from any directory, though
   restoring the original cwd preserves project settings and trust.

## Codex

### Verified

- Profile isolation. With `CODEX_HOME` set to an empty directory,
  `codex login status` printed `Not logged in` (exit 1) even though
  `~/.codex/auth.json` exists (mode 0600, top-level keys `OPENAI_API_KEY`,
  `auth_mode`, `last_refresh`, `tokens`; `tokens` keys `access_token`,
  `account_id`, `id_token`, `refresh_token`; key names only, values not read).
  `codex exec --skip-git-repo-check 'say ok'` in that profile reached the API
  and failed with `401 Unauthorized: Missing bearer or basic authentication`:
  the request carried no usable inherited authentication. A 401 is not an
  access trace, so "the global file was never opened" is not claimed
  (v0.154.0).
- First-launch writes. Those runs created, inside `CODEX_HOME` only:
  `sessions/YYYY/MM/DD/rollout-<timestamp>-<uuid>.jsonl`, SQLite stores
  (`state_5.sqlite`, `thread_history_1.sqlite`, `logs_2.sqlite`,
  `memories_1.sqlite`, `queue_1.sqlite`, `goals_1.sqlite`), `installation_id`,
  `skills/`, `tmp/` and lock files. A rollout is written even when
  authentication fails.
- Session identity and resume lookup. `codex exec resume <uuid>` in the owning
  `CODEX_HOME` found the rollout and progressed to the 401 error; the same
  command in a different fresh `CODEX_HOME` failed with `thread/resume failed:
  no rollout found for thread id <uuid> (code -32600)`. Resume lookup is
  scoped to `CODEX_HOME`. Interactive `codex resume` filters its picker by
  working directory unless `--all` is passed (help text), but an explicit id
  resolves regardless of cwd.
- Transcript portability. Copying the rollout file into a second fresh
  `CODEX_HOME` under the same `sessions/YYYY/MM/DD/` path made
  `codex exec resume <uuid>` find it (progressed to 401), with no SQLite
  migration step required.
- Credential storage. Codex selects a credential backend via
  `cli_auth_credentials_store`: an invalid value fails with "unknown variant
  …, expected one of `file`, `keyring`, `auto`, `ephemeral`" (observed
  against an isolated `CODEX_HOME`), and the binary's strings carry the
  default
  `cli_auth_credentials_store = "file"`. Under that default file backend,
  login state is a plain `auth.json` inside `CODEX_HOME` (structure above),
  which is a structural basis for holding different accounts in different
  homes; the `keyring` and `auto` backends store outside the home and are
  not covered by any claim here. Refresh independence between two logged-in
  homes is unverified — `last_refresh` is a key name, not observed refresh
  behavior. `codex login` supports a browser flow, `--device-auth`, and
  non-interactive `--with-api-key` / `--with-access-token` reading from
  stdin (help text).
- Config and trust. `-c key=value` overrides any `config.toml` value;
  `--profile <name>` layers `$CODEX_HOME/<name>.config.toml` over the base
  config — a config layer, not credential isolation, since `auth.json` is per
  home. Per-project trust is recorded as `[projects."<path>"]` sections with a
  `trust_level` key, and hook trust as `[hooks.state."<path>"]` with
  `trusted_hash` (section/key names observed in a real config; values not
  read). `codex exec` ran headless with no trust or approval prompt; sandbox
  and approvals are launch flags (`--sandbox`, `--ask-for-approval`).
- Limit signals (code inspection, `/opt/homebrew/Caskroom/codex/0.154.0/bin/codex`).
  The binary contains `You've hit your usage limit for `, rate-limit fetch and
  reset handling, and 429 detection strings.

### Unverified or unsupported

- No login was performed in an isolated home, so account-holding across two
  homes is a structural fact of the default file backend (separate files)
  awaiting an authenticated confirmation with a second account, and refresh
  independence is entirely unverified. The `keyring`/`auto`/`ephemeral`
  backends were not exercised at all.
- Authenticated resume, resume under a different account, and provider-side
  acceptance of a copied rollout are untested.
- The interactive first-run screens (sign-in, trust prompt wording, MCP
  approval prompts) were not captured; a PTY capture attempt rendered nothing
  usable. Their exact sequence needs the opt-in interactive run.
- Live limit and quota behavior was not reproduced.

### Launch recipe

For a worker in a Herdr pane using account home `<dir>`:

1. Create `<dir>` (once per account) and bootstrap it interactively: a human
   runs the first launch in the workspace and answers the sign-in, trust and
   any hook/MCP approval prompts for exactly that workspace root. Automated
   pre-seeding of `config.toml` (`[projects."<workspace-root>"]`
   `trust_level = "trusted"`, `model`, `approval_policy`, `sandbox_mode`,
   MCP servers) is a pending design option: the interactive first-run
   sequence was not captured, so no seed is verified to suppress every
   prompt. Per-launch `-c` overrides remain available for settings that are
   not trust decisions.
2. Perform the account login once in that home (`CODEX_HOME=<dir> codex
   login`, or `codex login --with-api-key` fed from stdin for API-key
   accounts); the browser flow is a human step.
3. Launch through the sanitizing launcher described in the scope section:
   pane/tab creation `env` can add `CODEX_HOME=<dir>` but cannot unset
   inherited variables; removal must happen in the argv that finally execs
   `codex`. Conditional until that launcher arrangement is verified.
4. Record the thread UUID (rollout filename suffix), home directory, working
   directory and account label. Cold resume is `CODEX_HOME=<dir> codex resume
   <uuid>` (interactive) or `codex exec resume <uuid>` (non-interactive).

## opencode

### Verified

- Profile isolation. With `HOME` set to an empty directory, every file
  opencode wrote landed under that directory: `.config/opencode/`
  (`opencode.jsonc`, a `.gitignore`, and a `node_modules` tree it installs on
  first run — first launch performs a network dependency install),
  `.local/share/opencode/` (`opencode.db` SQLite database, `log/`, `repos/`,
  `snapshot/`), `.local/state/opencode/locks/` and `.cache/opencode/`. With
  `XDG_DATA_HOME` and `XDG_CONFIG_HOME` set, the data (including the
  database) and config moved to `<XDG_DATA_HOME>/opencode` and
  `<XDG_CONFIG_HOME>/opencode` respectively, while state and cache stayed
  under `HOME` (`XDG_STATE_HOME`/`XDG_CACHE_HOME` were not set) (v1.18.30).
- `HOME` alone does not isolate. With a fresh `HOME` and an inherited
  `XDG_DATA_HOME`, `opencode auth list` resolved `auth.json` (and created
  `opencode.db`) under `<XDG_DATA_HOME>/opencode`; changing `HOME` to a
  second fresh directory left that credential path identical, while config,
  state and cache followed `HOME`. An inherited XDG variable therefore
  splits the profile and can point credentials at a shared directory. Every
  opencode profile must set or clear all four `XDG_*_HOME` variables
  explicitly.
- No login wall. `opencode run 'say ok'` in an isolated profile with all
  provider env vars stripped succeeded on the built-in free model
  (`llm.provider=opencode`, `llm.model=big-pickle` in its log). The
  interactive first run (PTY capture) opens directly at a usable prompt
  showing `Build · Big Pickle OpenCode Zen` and the tip `Run /connect to add
  an AI provider` — no onboarding, login or trust dialog blocks launch.
  Because unauthenticated runs succeed, "does an isolated profile see the
  global account" must be answered with `opencode auth list`, not with run
  success.
- Credential storage. `opencode auth list` prints the credential file path
  `~/.local/share/opencode/auth.json` (resolved against the active profile)
  and reported `0 credentials` in an isolated profile; with `OPENAI_API_KEY`
  exported it additionally lists an `Environment` section (`OpenAI
  OPENAI_API_KEY`). The developer's machine currently has no global
  `auth.json` (never logged in), so global-account inheritance cannot be
  probed until a login exists. Per the official
  [providers documentation](https://opencode.ai/docs/providers/):
  `auth.json` holds "API keys, OAuth tokens"; opencode refreshes OAuth
  tokens itself for the providers that document it; Anthropic Claude
  Pro/Max subscription OAuth is prohibited and its plugins were removed as
  of 1.3.0, so Anthropic access is API-key based.
- Session identity and resume. Sessions are rows in the profile's
  `opencode.db` with IDs like `ses_f631426d5ffeu7nZRPtfl3fpL7`;
  `opencode session list` shows them. `opencode run -s <id> '<msg>'`
  continued the session from the original and from a different working
  directory (exit 0); the same command in a different isolated profile
  printed `Error: Session not found` — and still exited 0, so HOP must parse
  the error, not the exit code. TUI resume flags are `--continue|-c`,
  `--session|-s`, `--fork`.
- Transcript portability. `opencode export <id>` (JSON to stdout) in one
  profile followed by `opencode import <file>` in another reported `Imported
  session: <same id>` and the session then appeared in the second profile's
  `session list`. Portability is a first-class export/import feature; no
  database file copying is needed.
- Herdr support. Herdr detects agent kind `opencode`, treats the OpenCode
  integration as a lifecycle authority, and cold-resumes it with
  `opencode --session <id>` (integration ≥ 5)
  ([session state](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/session-state.mdx),
  [integrations](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/integrations.mdx)).
  Herdr's integration installer writes
  `~/.config/opencode/plugins/herdr-agent-state.js` with the opencode config
  directory hardcoded to `home_dir()/.config/opencode`
  ([env.rs](../../repos/herdr/src/integration/env.rs)); unlike Claude Code
  (`CLAUDE_CONFIG_DIR`) and Codex (`CODEX_HOME`) there is no env override, so
  per-profile integration install requires the `HOME`-override profile shape
  (an `XDG_CONFIG_HOME`-only profile is invisible to the installer).

### Unverified or unsupported

- No login was performed anywhere (no global `auth.json` exists), so account
  isolation between two logged-in profiles, refresh independence and
  account-mismatch resume behavior are all untested; they need the opt-in
  experiments below.
- Whether `XDG_STATE_HOME`/`XDG_CACHE_HOME` are honored was not exercised.
- Limit and quota signals: unknown; nothing was observed and the free-tier
  throttling behavior was not probed.
- Authenticated resume and resume under a different provider account are
  untested.

### Launch recipe

For a worker in a Herdr pane using account profile `<dir>`:

1. Set the full profile environment: `HOME=<dir>` plus all four XDG
   directory variables pinned inside it (`XDG_DATA_HOME=<dir>/.local/share`,
   `XDG_CONFIG_HOME=<dir>/.config`, `XDG_STATE_HOME=<dir>/.local/state`,
   `XDG_CACHE_HOME=<dir>/.cache`) so an inherited XDG value cannot redirect
   credentials and opencode's config path agrees with Herdr's HOME-based
   integration installer. Strip provider API-key variables
   (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `OPENAI_BASE_URL`, …).
2. Run the account login once in that profile (`opencode auth login` /
   `/connect`); browser OAuth flows are a human step. Expect the first launch
   to install plugin dependencies over the network.
3. Launch through the sanitizing launcher described in the scope section —
   the pane env map can add these variables but cannot unset inherited ones,
   so the `HOME`/XDG/unset set must be applied in the argv that finally
   execs `opencode`. Conditional until that launcher arrangement is
   verified.
4. Record the `ses_…` id, the profile env, working directory and account
   label. Cold resume is `opencode run -s <id>` (non-interactive) or
   `opencode --session <id>` (TUI) with the same profile env; check stderr
   for `Session not found` rather than the exit code.

## Herdr cold-restore interaction

From the local checkout (`3be35231497f33f38aeffd84df02642479995bc6`, matching
installed `herdr 0.9.0` documentation):

- Native agent session restore is enabled by default
  (`resume_agents_on_restore` disables it) and relaunches supported agents
  after a server restart with `claude --resume <id>` (integration ≥ 6),
  `codex resume <id>` (integration ≥ 5) and `opencode --session <id>`
  (integration ≥ 5); stale or invalid references restore
  as normal shells
  ([session state](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/session-state.mdx)).
- The cold-restore path constructs its launch environment as
  `PaneLaunchEnv::from_extra(Vec::new())`
  ([restore implementation](../../repos/herdr/src/persist/restore.rs)), as
  already noted by the [Herdr surface audit](herdr-surface.md). Precisely:
  the per-pane env overrides HOP supplied at creation are not replayed. The
  restored pane still inherits the Herdr server's own environment, so this
  is not a claim that all variables are absent — only that the
  profile-selecting overrides are.

Combined with the profile-scoped resume lookups verified above for all three
harnesses, Herdr auto-resume of a session that was launched in an isolated
profile runs without the overrides that selected that profile: the resume
command resolves against whatever profile the inherited server/shell
environment implies. If that inherited profile differs from the recorded
one, the transcript may be missing or a session may run with unintended
credentials (for opencode, with whatever provider key that environment
carries). The binding is not restored reliably; auto-resume of
isolated-profile sessions is unsupported.

## Proposed supported restore policy

| Mode | Status | Evidence |
| --- | --- | --- |
| Warm reattach: Herdr server and agent process survived; HOP reconciles bindings | Supported | No relaunch occurs; Herdr keeps live processes ([session state](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/session-state.mdx)) |
| Cold resume by HOP: relaunch with the recorded profile env, same account, recorded cwd | Pending, per harness, on the account-identity smoke below | Resume lookup verified unauthenticated for Claude Code and Codex; full unauthenticated resume round-trip verified for opencode on its free model — which proves nothing about accounts; authenticated, account-verified continuation untested everywhere |
| Cold resume via Herdr auto-restore of an isolated-profile session | Unsupported | Original per-pane profile overrides are not replayed; resume uses the inherited server/shell environment, so the recorded profile/account binding is not guaranteed (restore path above; resume lookup profile-scoped, verified) |
| Cold resume where the resuming profile's account differs from the original | Needs opt-in evidence | Transcript portability verified (file copy for Claude Code/Codex, export/import for opencode); provider-side acceptance and account semantics unknown; requires a second account |
| Automatic account failover during restore | Unsupported | No evidence; [architecture](architecture.md) requires confirmed credential isolation before claiming it |

Consequences HOP must implement: run HOP-managed sessions on a dedicated Herdr
server/session with `resume_agents_on_restore = false`, or otherwise ensure
Herdr does not race HOP's own account-correct relaunch; on resume, verify the
recorded profile still holds the expected account before relaunching, and
return an actionable unsupported state instead of silently launching on
default credentials ([phases](../plan/phases.md), phase 1).

## Opt-in live smoke test (described, not implemented)

Per [plugin testing](plugin-testing.md) layer 5, one Go test package
(proposed `test/live`, build tag `live_harness`, skipped unless
`HOP_LIVE_HARNESS=1`). Its filesystem contract is a test-owned scratch-root
lifecycle: the suite first creates a scratch root and empty profile
directories inside it, prints the paths, and stops with instructions; the
human authenticates those specific profiles; on the next run the suite
validates that every `HOP_LIVE_CLAUDE_PROFILE` / `HOP_LIVE_CODEX_HOME` /
`HOP_LIVE_OPENCODE_PROFILE` path is inside its scratch root before executing
anything. Harness writes (transcripts, databases, token refresh into the
profile) are then by construction inside the scratch root. The one permitted
side effect outside it is Claude Code's macOS keychain item for the scratch
config dir (service `Claude Code-credentials-<hash>`), which the human's
login creates; the suite may check that item's existence by service name but
never reads or writes it, and cleanup instructions tell the human to delete
it. Codex must run with the default `file` backend (`keyring`/`auto` are out
of scope). Metadata-only existence/mtime checks of the global
`~/.claude.json`, `~/.codex/auth.json` and
`~/.local/share/opencode/auth.json` (unchanged, or still absent) are test
assertions only — the production port keeps its stricter no-access rule.

Per harness, the sequence and promotion condition are:

1. Unauthenticated baseline in a fresh profile with provider env vars
   stripped: Claude Code and Codex fail with their recorded not-logged-in
   errors; opencode answers on its free model and `opencode auth list`
   reports zero credentials.
2. In the authenticated profile, verify the intended account identity
   before launching (non-secret identity surfaces only, e.g. account
   label/id fields, never token values), and select the intended provider
   and model explicitly in the launch arguments.
3. Send one fixed prompt, record the session/thread id and the responding
   provider/model from the harness's own structured output or log, then
   kill the process.
4. Resume by id with the same profile env, send a fixed continuation prompt,
   and assert: a new turn is appended to the same session id, the responding
   provider/model equal the intended ones on BOTH turns, and the account
   identity observed after resume equals the one verified before launch.
   For opencode, a turn answered by the free fallback (provider `opencode`)
   is an explicit failure, since the doc shows free-model success proves
   nothing about the account.

A policy-table row is promoted from "pending" only when this passes for that
harness; a transcript continuation without the account/provider/model
evidence is not sufficient. The suite never stores or logs token values, and
the ordinary test suite must not depend on it.

## Evidence ledger

Sanitized reproduction detail for the observations above. Every probe ran
from a scratch working directory (a fresh `git init` directory under the
session scratch root, called "scratch cwd" below) with fresh profile
directories, on the versions in the table. "clean env" means
`ANTHROPIC_API_KEY`, `OPENAI_API_KEY` and `OPENAI_BASE_URL` were unset via
`env -u`. Paths are abbreviated; no output shown here contains identifiers.

| # | Command (env) | Result |
| --- | --- | --- |
| L1 | `claude -p 'Reply with the single word ok' --model haiku` (clean env, `CLAUDE_CONFIG_DIR=<fresh>`), scratch cwd | `Not logged in · Please run /login`; profile files as listed (modes from `ls -la`: `.claude.json` 0600, `sessions/` 0700) |
| L2 | `claude -p --resume <uuid> 'hi'` (same env), other scratch cwd | Same login error → lookup succeeded; unknown uuid → `No conversation found with session ID: …` |
| L3 | `cp` of the session JSONL into a second fresh profile's matching `projects/<munged-cwd>/`, then L2 there | Login error → lookup succeeded; before the copy: `No conversation found` |
| L4 | `script -q <file> claude` (clean env, fresh `CLAUDE_CONFIG_DIR`), scratch cwd, no input, killed after 12 s | Theme-selection onboarding screen (v2.1.259) |
| L5 | `codex login status` / `codex exec --skip-git-repo-check 'say ok' </dev/null` (`CODEX_HOME=<fresh>`; `OPENAI_API_KEY` still exported in this shell) | `Not logged in` exit 1; 401 `Missing bearer or basic authentication`; profile files as listed via `find` |
| L6 | `codex exec resume <uuid>` in owning home, in a second fresh home, and in a second home after `cp` of the rollout file | 401 after lookup; `no rollout found for thread id … (code -32600)`; 401 after lookup |
| L7 | `codex -c 'cli_auth_credentials_store="invalid-review-mode"' login status` (clean env, fresh `CODEX_HOME`) | `unknown variant …, expected one of file, keyring, auto, ephemeral`; `strings` shows default `cli_auth_credentials_store = "file"` |
| L8 | `opencode run 'say ok' </dev/null` (clean env, `HOME=<fresh>`), scratch cwd | Answered on `llm.provider=opencode`, `llm.model=big-pickle` (own log); writes enumerated with `find <home> -maxdepth 4` |
| L9 | Same command with `OPENAI_API_KEY`/`OPENAI_BASE_URL` exported | Answered via `llm.provider=openai` — env credential adopted inside the isolated profile |
| L10 | `opencode run -s <ses id> …` same home/same cwd, same home/other cwd, different fresh home (clean env) | Continued; continued; `Error: Session not found`, exit 0 |
| L11 | `opencode export <ses id>` in home A; `opencode import <file>` in home B; `opencode session list` in B (clean env) | `Imported session: <same id>`; session listed in B |
| L12 | `opencode auth list` (clean env) with `HOME=<fresh1>` + `XDG_DATA_HOME=<shared>`, then `HOME=<fresh2>` + same `XDG_DATA_HOME` | Both print the identical `<shared>/opencode/auth.json`, `0 credentials`; config/state/cache followed `HOME` |
| L13 | `script -q <file> opencode` (clean env, `HOME=<fresh>`), scratch cwd, no input, killed after 15 s | Prompt screen `Build · Big Pickle OpenCode Zen`, tip `Run /connect to add an AI provider`; no blocking prompt |
| L14 | `security find-generic-password -s "Claude Code-credentials"` / `ls -la` on `~/.claude`, `~/.codex`, `~/.local/share/opencode` / `jq 'keys'` on the two global JSON files | Names, modes and key names only, as quoted in the harness sections; no values read |

Code-inspection facts cite `strings` output of the named installed binaries
(Claude 2.1.259, Codex 0.154.0) or the Herdr checkout files linked inline.

## Open questions for the human

1. Can you provide a second Claude account and a second OpenAI account (or
   approve using existing spares) for the cross-account and concurrent-refresh
   experiments? Without them those rows stay "needs opt-in evidence".
2. Approve one interactive login per harness into a scratch profile directory
   (steps: `CLAUDE_CONFIG_DIR=<tmp> claude` → `/login`;
   `CODEX_HOME=<tmp> codex login`; `HOME=<tmp> opencode auth login`) so the
   keychain-item naming, `auth.json` creation and global-profile
   non-interference become observations instead of code inspection. You run
   the logins; automation only inspects file names and keychain item names
   afterward. For opencode, which provider should the account use (ChatGPT
   OAuth or an API key), given Anthropic subscription OAuth is prohibited
   there?
3. Which onboarding values should HOP pre-seed for Claude profiles (theme
   choice, `hasCompletedOnboarding`), and is pre-seeding trust
   (`hasTrustDialogAccepted`, Codex `trust_level = "trusted"`) for the
   workspace root acceptable, or should first launches stay interactive?
4. Should HOP-managed runs live on a dedicated Herdr server with
   `resume_agents_on_restore = false`, per the policy above?
5. Your shell profile exports `OPENAI_API_KEY`/`OPENAI_BASE_URL`, which
   opencode adopts silently inside an otherwise isolated profile. Should HOP
   always strip provider key variables from worker environments, or is there
   a workflow that intentionally relies on them?

## Implications for HarnessProfiles

The `HarnessProfiles` port ([architecture](architecture.md)) must:

- own one directory per account profile and produce launch specifications
  that set `CLAUDE_CONFIG_DIR`, `CODEX_HOME` or the opencode full
  `HOME`+XDG set AND name the variables to unset (provider API keys,
  `CLAUDE_CODE_OAUTH_TOKEN`, `CLAUDE_SECURESTORAGE_CONFIG_DIR`), applied by
  a launcher at the harness exec boundary — Herdr's pane env map is additive
  and `agent.start` accepts no env, so neither can perform the removal;
- establish and verify launch readiness per profile: a human-approved
  interactive bootstrap for the specific workspace until verified pre-seed
  contents exist, an explicit trust decision per workspace root (never
  ancestor-path trust), and a not-launch-ready state for a profile that
  would block on login, onboarding or trust;
- record session/thread id, profile directory, working directory and account
  label at launch, and refuse a resume specification when any of them is
  missing or the profile's account no longer matches the binding;
- surface the unsupported restore modes above as explicit errors.

It must never: read or write the developer's default `~/.claude`,
`~/.claude.json`, `~/.codex` or `~/.local/share/opencode` state; copy
credentials or keychain items between profiles; rewrite a live profile's
credentials to switch accounts; or rely on Herdr auto-restore for
isolated-profile sessions.
