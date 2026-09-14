# Herdr integration surface audit

Scope: local checkout `3be35231497f33f38aeffd84df02642479995bc6`, published 0.9.0
documentation, selected draft comparisons and implementation inspection. This is
a static audit, not a live compatibility test. No user configuration was changed.

## Main finding: native Agents view integration

HOP can customize the built-in Agents view without keeping its board open:

- `pane.report_metadata` publishes run, role, task, parent and account tokens.
- `ui.sidebar.agents.rows` renders those tokens in native agent entries.
- `agent.view.set` filters and sorts entries across workspace boundaries.
- Native mouse selection, indexed focus and next/previous-agent navigation follow
  that projection.

This provides grouping by adjacency and labels, not a collapsible tree.
`AgentViewSetParams` has source, label, filter and sort fields, but no parent,
children, group-by, group-header, indentation or collapse fields. The renderer
consumes a flat list. The default `spaces` sort is labeled `grouped` in the UI;
that means workspace ordering, not manager/worker lineage.

The earlier pane mockups understated this capability. Plugins cannot insert
arbitrary native sidebar widgets, but they can configure the existing Agent view.

Evidence: [Agent view schema](../../repos/herdr/src/api/schema/agents.rs),
[projection implementation](../../repos/herdr/src/app/agent_view.rs),
[native renderer](../../repos/herdr/src/client/shell/agent_sidebar.rs),
[Agent view documentation](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/socket-api.mdx).

## Interaction surface

| Area | Existing capabilities | HOP mapping / constraints |
| --- | --- | --- |
| Discovery | `herdr api schema --json`, `session.snapshot`, `ping` | Inspect installed contracts and bootstrap; checkout version is not proof of installed-server support |
| Workspace/worktree | Create/list/get/focus/rename/reorder/close; Git worktree create/open/remove | Isolate workers and preserve repository grouping; closing UI state is different from deleting checkouts |
| Tabs, panes and layout | Split/move/swap/resize/zoom/focus; export/apply layout; supported creation requests accept cwd/env | Arrange real processes; use returned IDs after moves |
| Agent lifecycle | Start/get/list/rename/focus/read/prompt/wait/send-keys/explain | Manage recognized agents; readiness and Done are not task acceptance |
| Native Agent view | `agent.view.set`, `agent.view.clear` | Run filtering and manager-first order; one transient override per server |
| Display metadata | Pane/workspace tokens, titles, display agent and state labels | Show HOP facts without taking over runtime state; layout/styles remain client-local |
| Terminal I/O | Text/keys, process info, output waits, text/ANSI reads | Inspect native output; reconcile ambiguous submissions before retrying |
| Direct terminal streams | CLI attach/control/observe | Jump to real terminals; no need to build another terminal emulator |
| Events | Subscriptions, one-shot waits, snapshots | Controller wakeups; no historical lifecycle replay |
| Plugin packaging | Manifest metadata/platforms/minimum version, build commands, install/link/enable/disable/uninstall | Ship a Go executable; linked plugins build separately; registry is user-global |
| Actions/bindings | Manifest actions, invocation contexts and custom keybindings | Board, run picker, accounts, native-view selection; no runtime action registration |
| Plugin panes | Overlay, popup, split, tab and zoomed placements | Rich details where native rows are insufficient; popup is modal, singleton and not a normal pane |
| Startup/event hooks | One-shot startup and selected emitted lifecycle events | Rehydrate state; not daemon supervision or synchronous veto gates |
| Plugin context/storage | State/config/root paths, socket/binary paths, invocation JSON and IDs | HOP owns files/schema; plugin cwd is not the target repository |
| Link handlers | URL regex to manifest action with clicked URL context | Optional issue/run link actions |
| Notifications/titles | `notification.show`, client title set/clear | Optional attention/title display; notifications can be disabled |
| Agent integrations | Install/uninstall/status, semantic reports, native conversation references | Profile-aware setup; Claude/Codex hooks report identity while screen manifests determine lifecycle |
| Persistence | Detach, snapshot restore, native resume, experimental handoff | Reconcile bindings; cold restart is not process continuity |
| Graphics | Conditional pane image layers and streaming | Available but unnecessary for HOP's initial terminal UI |
| Configuration | Defaults output, reload, keybindings, sidebar rows/styles, headless size | Supply minimal config patches, preserve user settings |
| Remote/named sessions | Separate server namespaces and remote targeting | Keep server identity in runtime bindings; remote orchestration remains deferred |

## Native manager/worker presentation

Proposed metadata contract:

| Token | Example | Meaning |
| --- | --- | --- |
| `hop_run` | `r18` | Run filter value |
| `hop_run_order` | `000018` | Stable ordering between runs |
| `hop_order` | `000000` / `000010` | Manager first, deterministic worker sequence |
| `hop_role` | `manager` / `implementer` | Role label |
| `hop_task` | `retry policy` | Task summary |
| `hop_parent` | `manager-r18` | Readable parent label, absent for manager |
| `hop_account` | `claude-a` | Non-secret account label |
| `hop_state` | `awaiting checks` | HOP workflow state, independent of native status |

Actual parent relationships remain HOP session IDs in durable state. A display
token is neither an authorization signal nor a native Herdr parent link. Token
sorts are string comparisons, so numeric ordering keys must be padded. Leading
whitespace is trimmed; do not depend on it for indentation. Tree glyphs in labels
would be decorative only.

Illustrative metadata request:

```json
{
  "id": "hop-metadata-1",
  "method": "pane.report_metadata",
  "params": {
    "pane_id": "w2:p1",
    "source": "plugin:hop",
    "tokens": {
      "hop_run": "r18",
      "hop_run_order": "000018",
      "hop_order": "000010",
      "hop_role": "implementer",
      "hop_task": "retry policy",
      "hop_parent": "manager-r18",
      "hop_account": "claude-a"
    }
  }
}
```

Optional client configuration, not applied automatically:

```toml
[ui.sidebar.agents]
rows = [
  ["state_icon", "agent", "$hop_role"],
  ["$hop_run", "$hop_task"],
  ["$hop_parent", "$hop_account"],
  ["workspace", "tab"],
]
```

Missing values disappear, so non-HOP agents remain readable. Offer a shorter
layout for many agents. Existing `rows_by_agent` settings replace the entire row
layout and must be considered when generating a config patch. Overrides are keyed
by canonical harness kind, not by role/run. Custom multiline rows apply to the
expanded sidebar; do not promise identical layout in compact/mobile modes.

Illustrative focused-run view:

```json
{
  "id": "hop-view-1",
  "method": "agent.view.set",
  "params": {
    "source": "plugin:hop",
    "label": "HOP r18",
    "filter": {
      "op": "eq",
      "field": {"token": "hop_run"},
      "value": "r18"
    },
    "sort": [
      {"field": {"token": "hop_order"}, "order": "asc"}
    ]
  }
}
```

The `plugin:hop` source assumes an enabled installed plugin with manifest ID `hop`.
The inspected CLI has no dedicated `herdr agent view` wrapper; use a narrow raw
socket path in the Herdr adapter. Unix sockets and Windows named pipes differ.

Conceptual rendering, with every entry representing a live detected agent:

```text
+---------------------------------+--------------------------------------+
| agents                  HOP r18  | retry-policy / agent                 |
|                                 |                                      |
| * Claude Code . manager         | Native implementer terminal          |
|   r18 . Add retry support       |                                      |
|   payments . manager            | Updating retry.go ...                |
|                                 |                                      |
| * Claude Code . implementer     |                                      |
|   r18 . retry policy            |                                      |
|   manager-r18 . claude-a        |                                      |
|   retry-policy . agent          |                                      |
|                                 |                                      |
| - Codex . reviewer              |                                      |
|   r18 . review                  |                                      |
|   manager-r18 . codex-b         |                                      |
|   retry-policy . review         |                                      |
+---------------------------------+--------------------------------------+
```

No synthetic group header or collapse triangle is implied. Queued tasks and exited
agents cannot be inserted as native agent rows. HOP's board supplies the full tree,
history, pending work, messages and gates independently.

## View ownership and metadata lifecycle

- One server-wide projection exists. A set replaces the previous view atomically;
  it is not scoped independently to each plugin/run. Make selection a deliberate
  user action, not a side effect of every worker update.
- Offer all agents ordered by HOP run; selected run; selected run plus blocked/done
  elsewhere; and normal Herdr view. Filtering does not change `agent.list`, global
  attention counts, detection or notifications.
- Structural ordering and attention-first ordering are different modes: sorting
  attention first can move workers away from their managers.
- Serialize HOP view selection per server and persist the user's choice. Run
  controllers publish metadata; they must not compete to replace the view.
- Clear with `source: plugin:hop` so another owner's view stays intact. There is
  no documented compare-and-swap set or view-get returning a previous owner's full
  query; do not promise to stack/restore arbitrary other plugins' projections.
- The view disappears on clear/replacement, owner disable/unlink/uninstall or
  server exit. Startup hooks may reapply it, but only after metadata/bindings are
  reconciled; restored native agents may appear later, after client attachment.

Token limits: 16 keys per report, 32 retained keys per resource, 32-character token
names and 80-character values. Use `hop_` names and a stable source identity.
Metadata patches leave omitted keys untouched. Token patches are not protected by
the optional agent/lifecycle-source presentation guards; clear or rebind tokens
when the occupant changes. Use TTL for transient progress and rehydrate durable
labels after cold restart. Token metadata is not cold-restored. If sequencing is
used, keep monotonically increasing source-scoped values across controller restarts;
source slots are bounded, so do not allocate a new reporter source per event.

## Other findings affecting implementation

### Account-aware launch and restore

`agent.start` takes a pane, kind, args and timeout, but no env map. Supported
workspace/tab/pane creation and layout leaf requests accept env overrides. Create
the shell with the selected account's profile context before starting its agent;
HOP's process environment does not reconfigure an already-running shell. For a
new worktree, opening a tab/pane there with explicit env is a viable path.

Claude integration installation honors `CLAUDE_CONFIG_DIR`; Codex honors
`CODEX_HOME`. Per-profile hooks can therefore be installed while preserving native
settings. They report native conversation identity, not task completion.

The inspected cold-restore path builds `PaneLaunchEnv::from_extra(Vec::new())`.
Native resume is therefore not evidence that the original account/profile env is
preserved. Account-bound cold restore needs explicit verification and coordination
with Herdr's auto-resume to avoid duplicate launches or the wrong default account.
Investigate a dedicated HOP Herdr session with native auto-resume disabled, or
verified profile-aware resume support. Do not change unrelated sessions' restore
settings as a shortcut. This is a compatibility finding, not a live reproduction.

Current policy (2026-09-14, see
[native harness compatibility](native-harness-compat.md#adopted-policy)): HOP
runs on the user's normal Herdr server with no configuration change; a
dedicated session with `resume_agents_on_restore` disabled is a later,
optional opt-in for the alternate-profile pointer case, not the default. This
does not resolve the restore-coordination question above, which remains open
regardless of server choice — see
[native harness compatibility](native-harness-compat.md#herdr-cold-restore-interaction).

Sources: [launch schema](../../repos/herdr/src/api/schema/agents.rs),
[workspace env](../../repos/herdr/src/api/schema/workspaces.rs),
[pane env](../../repos/herdr/src/api/schema/panes.rs),
[cold restore](../../repos/herdr/src/persist/restore.rs).

### Events versus hooks

Manifest hooks accept only `PLUGIN_HOOK_EVENT_KINDS`: workspace/worktree/tab
lifecycle plus pane create/close/focus/move/exit, agent detection and agent status
changes. Pane updates/output/scroll, workspace metadata and layout updates are not
in that hook set. API subscribers have a broader surface. Use subscriptions for
controller observation; do not assume every event can invoke a manifest hook.
Neither mechanism is a transactional workflow veto. Required HOP gates still
execute inside HOP's application layer. See [event definitions](../../repos/herdr/src/api/schema/events.rs).

### Configuration and version differences

The config-reference MDX file is a wrapper. Its canonical inventory comes from
[config-reference.json](../../repos/herdr/docs/versions/0.9.0/website/src/data/config-reference.json).
The inventory includes presentation, keybindings, terminal defaults, headless
geometry, notifications/sounds, restore, worktrees, updates, logs and remote settings.
Presentation is client-local; runtime settings belong to the selected server.
Reload applies most settings without restarting panes; some require restart.

Notification delivery defaults to off in that inventory. Persistent attention
items must remain visible without toasts. Startup hooks run after restore/socket
readiness, including handoff, but not on plugin link/enable or client attach.
They are initialization commands, not supervised daemons. Manifest argv and custom
command strings have different shell semantics; use executable argv for HOP.

Stable and draft Agent view sections match. Draft configuration adds conditional
`hide = true` token styling absent from stable 0.9.0 docs; the HOP examples do not
depend on it. Some restore integration-version tables disagree, notably Hermes;
verify installed integration status rather than copying every version claim.

## Architecture implications and verification

Add an application-owned `AgentPresentation` port for metadata and native-view
selection/clearing, implemented by the existing Herdr adapter. Keep view selection
out of business aggregates. Parent relationships belong to HOP sessions, not pane
IDs. A native flat view and a full HOP tree are complementary read models.

Before shipping, verify manager-first sorting across workspaces; lexical token
order; missing/expired metadata; occupant replacement; row overrides; narrow and
compact displays; focus navigation; another plugin replacing the view; owned clear;
cold restart rehydration; sequencing across controllers; actual hook coverage;
per-profile integration setup; and account-correct resume. No live guarantees are
claimed here.

## Source sweep index

- [Agents](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/agents.mdx): detection, lifecycle, labels and authority.
- [Plugins](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/plugins.mdx): manifest, hooks, context, panes, actions and storage.
- [Configuration](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/configuration.mdx): rows, styles, bindings, reload and local/server ownership.
- [Config reference](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/config-reference.mdx): generated key reference wrapper.
- [Agent automation](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/agent-automation.mdx): launch, prompts, waits, readiness and reads.
- [CLI reference](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/cli-reference.mdx): command availability, metadata, targeting and terminal streams.
- [Socket API](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/socket-api.mdx): protocol, projection, subscriptions and snapshots.
- [Session state](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/session-state.mdx): live persistence, cold restore and handoff.
- [Integrations](../../repos/herdr/docs/versions/0.9.0/website/src/content/docs/integrations.mdx): native identity and profile-aware installation.
