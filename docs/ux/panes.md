# HOP in Herdr: proposed pane workflows

These ASCII mockups are conceptual. Herdr's sidebar, borders and tabs are simplified;
the HOP board and its shortcuts are proposed. Native agent output is illustrative.
This document describes the optional board experience alongside the CLI; the
controller operates independently of the board.

The [Herdr surface audit](../architecture/herdr-surface.md) adds a native-first
option: publish HOP metadata into built-in Agent rows and filter/sort that panel
by run, manager and worker. This is a flat native projection, not a collapsible
agent tree. It can keep relationships visible while the HOP board is closed.

## Surfaces and ownership

| Surface | Owner | Purpose |
| --- | --- | --- |
| Workspace/sidebar, tabs and terminal splits | Herdr | Navigate repositories, worktrees and processes |
| Native Agents view with HOP metadata and sort/filter | Herdr, configured by HOP | Run-scoped manager/worker navigation across workspaces |
| HOP board terminal | HOP | Task progress, dependencies, checks, decisions and account bindings |
| Manager/worker terminal | Native harness inside Herdr | Direct conversation and real tool output |
| Check output terminal | Command runner inside Herdr where configured | Inspect deterministic validation output |
| Temporary board/details view | HOP in a Herdr overlay or popup | Inspect run state without keeping a permanent split |

Herdr plugin v1 offers terminal placements `split`, `tab`, `zoomed`, `overlay` and
`popup`, but no native non-terminal plugin UI. The examples require no custom
Herdr sidebar widgets. See [plugin panes](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/plugins.mdx)
and [workspace concepts](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/concepts.mdx).

## 1. Start a run and watch the plan

From an existing repository shell:

```text
$ hop run "Add retry support" --playbook feature
Run r17 started. Open the HOP board or continue in this terminal.
```

Proposed default when opening the board on a wide terminal: a control tab in the
repository workspace, with the board and manager side by side. Preserve existing
user tabs and layout; creating workers should not steal focus.

```text
+---------------+----------------------------------------------------------+
| HERDR         | repository: payments       [HOP r17] [shell] [runtime]    |
|               +-----------------------------+----------------------------+
| > payments    | HOP / r17                   | manager / Claude Code      |
|               | Add retry support           |                            |
|               |                             | I am checking the retry    |
|               | Planning                    | behavior and test setup.   |
|               | Workers: 0 / 2              |                            |
|               |                             | Proposed tasks:            |
|               | Task             State      | 1. Implement retry policy  |
|               | retry policy     planned    | 2. Review behavior         |
|               | review           waiting    | 3. Run required checks     |
|               | checks           waiting    |                            |
|               |                             | >                          |
|               | [Enter] details [m] manager | Native agent input         |
+---------------+-----------------------------+----------------------------+
```

The manager is an ordinary agent terminal. The board renders application state.
Typing while focused in the manager sends input to that harness; HOP shortcuts
apply only while the board has focus. Planning does not imply a mandatory human
approval step; the playbook/policy determines whether one exists.

The `runtime` tab contains the foreground controller process in the initial
architecture. It is not a second manager agent. Closing the board keeps that
process running. Closing its runtime pane interrupts coordination; HOP must show
that interruption on reconnect. Worker processes may survive it. Detached service
management can replace this arrangement later without changing board semantics.

## 2. Parallel work without squeezing every worker into the board

Writing workers get their own worktree workspaces. HOP assigns readable workspace
and pane labels using available Herdr APIs; the sidebar grouping below represents
Herdr's existing repository/worktree grouping, not a custom HOP task tree.

```text
+--------------------+-----------------------------------------------------+
| HERDR              | payments                 [HOP r18] [shell] [runtime]|
|                    +-----------------------------------------------------+
| > payments         | HOP / r18  Add retry support                        |
|   retry-policy     | Workers: 2 / 2                     No input needed  |
|   retry-tests      |                                                     |
|                    | TASK             STATE       ACCOUNT      CHECKOUT  |
|                    | > retry policy   working     claude-a     policy    |
|                    |   retry tests    working     codex-b      tests     |
|                    |   review         waiting     --           --        |
|                    |                                                     |
|                    | Latest: tests worker requested retry error shape.   |
|                    | Manager replied with the agreed interface.          |
|                    |                                                     |
|                    | [Enter] task  [o] open agent  [m] manager  [e] events|
+--------------------+-----------------------------------------------------+
```

Opening the selected worker changes the focused Herdr workspace/pane. It does not
mirror an interactive worker terminal inside the board or move it out of its
worktree workspace. From a worker, a configurable Herdr plugin-action binding can
open the board as an overlay; do not prescribe a global key until conflict checks.

```text
+--------------------+-----------------------------------------------------+
| HERDR              | retry-policy                       [agent] [checks] |
|                    +-----------------------------------------------------+
|   payments         | implementer / Claude Code                           |
| > retry-policy     |                                                     |
|   retry-tests      | Updating retry.go and retry_test.go ...              |
|                    |                                                     |
|                    | Tool output and conversation appear here exactly    |
|                    | as they do in a normal native agent terminal.       |
|                    |                                                     |
|                    | >                                                   |
+--------------------+-----------------------------------------------------+
```

Reviewers can inspect a completed writer's worktree; concurrent independent
writers retain separate checkouts. A checks pane is opened when useful, not
preallocated for every worker. HOP never equates a Herdr Done badge with task
acceptance; explicit results and workflow checks drive the board's task state.

## 3. A question needs the developer

The manager handles questions within the standing policy. When a decision cannot
be resolved, the board presents an attention item with the affected scope. No
automatic modal should interrupt typing in another terminal.

```text
+-------------------------------------------------------------------------+
| HOP / r18                         Needs input: 1       Workers: 1 active |
+-------------------------------------------------------------------------+
| retry policy    needs decision     retry tests    working                |
|                                                                         |
| Decision: retry HTTP 429 responses?                                      |
| From: implementer -> manager -> you                                     |
|                                                                         |
| The task specifies transient failures but does not define throttling.    |
|                                                                         |
| > Retry with Retry-After, within the existing attempt limit               |
|   Treat 429 as a terminal error                                          |
|   Write another answer                                                  |
|                                                                         |
| [Enter] answer  [o] inspect agent  [Esc] back                             |
+-------------------------------------------------------------------------+
```

Record the answer against a decision ID and resume only affected work. Other
workers continue unless dependencies or policy require a pause. Repeated answers
must be idempotent. A native permission dialog is a different attention type:
identify it and open the actual agent terminal for the deliberate response unless
a supported, authorized adapter can handle it. Do not inject a text answer into
an unknown permission UI. If information is stale, refresh before acting.

## 4. Review and deterministic gates

The board explains what prevents progress and lets the developer inspect evidence.
This illustration stops at ready-for-integration; it does not add a mandatory
publish or merge approval to every workflow.

```text
+-------------------------------------------------------------------------+
| HOP / r18                                     Checking candidate a84d9e2 |
+------------------------------------------+------------------------------+
| Task                   State             | CHECK OUTPUT                 |
| retry policy           result submitted  |                              |
| retry tests            result submitted  | $ go test ./...              |
| review                 passed            | ok   payments/retry          |
|                                          | FAIL payments/api            |
| Gate                   Outcome           |                              |
| reviewer acceptance    passed            | TestRetryAfter:              |
| unit tests             failed            | expected delay >= 1s         |
| lint                   passed            |                              |
| ready for integration  blocked           |                              |
|                                          |                              |
| Next: manager assigns a fix for the      |                              |
| failing test, then reruns stale checks.  |                              |
|                                          |                              |
| [Enter] evidence  [o] open checks         |                              |
+------------------------------------------+------------------------------+
```

This view is HOP-rendered details plus an output excerpt, not two cross-workspace
Herdr panes. `open checks` jumps to the real output pane when one exists; persisted
logs remain readable after that pane closes. Revision changes mark dependent
evaluations stale. Passed checks on an older revision cannot appear as approval of
the new candidate. When all required conditions pass, show `Ready for integration`
or `Completed` according to the playbook's terminal state.

## 5. Login status inspection

HOP delegates authentication to each harness; it performs no logins and stores
no credentials. Login status details can be a board screen or a user-opened
transient popup. `[l]` opens that harness's own dedicated login flow (for
example `claude` then `/login`, or `codex login`) in a normal terminal; a
session-modal popup should not own a long login.

```text
+-----------------------------------------------------------------------+
| HOP / login status                                                    |
+-----------------------------------------------------------------------+
| HARNESS       PROFILE              LOGIN STATE                        |
| claude        default              logged in                          |
| codex         default              logged in                          |
| opencode      alt: ~/.hop/op-b     login required                     |
|                                                                       |
| HOP performs no logins and stores no credentials; it only reports     |
| each harness's own status. Login opens that harness's normal flow.   |
|                                                                       |
| [Enter] details  [l] login  [Esc] back                                |
+-----------------------------------------------------------------------+
```

Report only what the harness itself reports (logged in / login required /
unknown); HOP does not track quota, cooldowns or organization/project
membership. The PROFILE column shows `default` or the configured
alternate-profile directory; HOP only passes that profile's variables through
and never bootstraps or verifies its contents beyond this status report.
Multi-account pools and round-robin assignment are a later, optional phase —
see [RESEARCH.md](../../RESEARCH.md).

## Layout and interaction rules

- Wide terminal: optional board/manager split. Small terminal: full-pane board;
  opening an agent switches focus, and details replace the list until Back.
- Keep visible task columns to task, state and account; reveal attempts, message
  history, checkout provenance and gate fingerprints in details.
- Display conceptual states distinctly: task waiting on a dependency, agent
  working, native permission blocked, gate failed, controller disconnected.
- `q` closes a HOP view where offered; it never means stop the run. Provide a
  separate explicit Stop action. Report requested/stopping/stopped truthfully.
- Detaching a Herdr client leaves its server processes running. Stopping the
  Herdr server is different and can terminate panes, including the controller.
- Live board updates never take focus. On disconnect show stale/disconnected
  state; disable commands requiring current state until reattachment/reconciliation.
- Treat the board as optional. `hop status`, `hop events`, `hop resume` and native
  terminal access remain useful without it. Mouse selection should complement
  keyboard navigation, with no conflict between board input and Herdr bindings.

## Implementation mapping

`adapters/board` renders application read models and issues use-case commands.
It does not import `adapters/herdr` or `adapters/sqlite`. Application navigation
use cases request focus through the runtime port; details/history use store ports.
The composition root wires these implementations. Use Herdr manifest actions to
open a run board, run picker or accounts screen. Resolve run context from HOP's
stored runtime bindings, including current server identity, and offer a picker
when invocation context does not identify exactly one run.

The board need not ship before the first CLI workflow. Implement the full-pane
task list and open-agent action first, then details/attention, then optional split
and popup layouts. Validate focus restoration, narrow terminals and board closure
independently from run termination before presenting this as a finished UI.
