# HOP phased implementation

Status: proposed sequence. Phase 0 is implemented on the engineering-foundation
branch (module, `hop version`, `make check`, checkers, guides, CI); later phases
are not started. Commands below describe the intended interface.

## Delivery strategy

Build small end-to-end workflows against a real Herdr process. Establish inward
dependency tests first, validate risky integration assumptions next, and add domain
complexity only when a working use case requires it. The full ERD is a direction,
not a requirement to create every table before launching the first worker.

The first useful release is a durable single-worker CLI. The central orchestration
milestone is manager → implementation → independent review → checks. Accounts and
the optional board extend that workflow after its execution semantics work.

| Phase | Usable outcome | Dependency |
| --- | --- | --- |
| 0. Engineering foundation | Buildable `hop` with enforceable conventions | None |
| 1. Herdr integration proof | Plugin action and native Agent view against isolated Herdr | 0 |
| 2. Durable single-worker run | Run/status/stop/resume for one scoped task | 1 |
| 3. Manager and workers | Bounded delegation, messages and independent review | 2 |
| 4. Repeatable playbooks and gates | Versioned workflows with enforced transitions | 3 |
| 5. Account profiles and pools | Native logins and session-level balancing | 4; compatibility explored in 1 |
| 6. Optional terminal board | Progressive task/decision/check inspection | 4–5 |
| 7. Release hardening | Installable, documented and recovery-tested release | 0–6; board may be omitted from a CLI release |

Do not postpone recovery tests to phase 7. Every side effect gains its interruption
and retry tests when introduced. Every package gains an AGENTS.md and exact import
classification when created. Follow [engineering standards](../architecture/engineering.md)
and the [real Herdr testing plan](../architecture/plugin-testing.md).

## Phase 0 — Engineering foundation

Deliver a minimal Go module and `cmd/hop` composition root with a version command.
Choose the module path, supported initial platform and pinned Go/tool versions.
Add strict formatting/lint configuration, non-mutating `make check`, CI, and
progressive root/group/package guides. Add only packages needed for this slice.

Implement import classification and its table-driven positive/negative fixtures.
Include inactive build-tagged sources, tests, effectful domain imports and unknown
packages. Implement guide coverage and local-link checks. Establish fake clock/ID
patterns when the first consumers appear; avoid a speculative utility library.

Exit: a clean build and full gate pass; injecting an outward or unclassified import
makes architecture tests fail; a missing package guide fails documentation checks.
The checker is not allowed to pass solely because it enumerated no packages.

## Phase 1 — Prove the Herdr boundary

Deliver a linkable manifest, a small Go action and a narrow Herdr adapter. Add
`hop doctor` for binary/version/schema availability and selected feature support.
Build an isolated real-process test fixture with temporary registry/config/state,
socket readiness, owned-process cleanup and failure artifacts.

Prove worktree/pane creation, explicit launch environment, metadata publication,
Agent view selection/owned clearing, context injection and event observation.
Use a deterministic reporting fixture process for ordinary CI. Attach a PTY client
for evidence of row rendering, manager-first navigation and view behavior.

Investigate the two account questions early: native profile isolation during launch,
and account-correct cold resume. Use an opt-in native-harness smoke where needed.
Record an explicit supported restore policy before phase 2 enables native cold
resume. If account-aware restore is not safe, return an actionable unsupported
state rather than silently using default credentials. This does not require a
complete account pool implementation yet.

Exit: actual plugin actions complete successfully in isolated Herdr, returned IDs
drive follow-up operations, view ownership is respected, and test registration
does not leak into user-global state. Have a documented compatibility result for
each claimed launch/restore mode; unsupported modes remain unavailable.

## Phase 2 — Durable single-worker run

Deliver `hop run`, `hop status`, `hop stop` and a scoped `hop resume`. Accept one
brief, freeze its effective instructions, create one task/attempt, and launch one
native worker in a worktree. Initially use an explicitly selected existing native
profile. Add the minimum SQLite schema and application-owned storage/runtime ports.

Record launch intent before calling Herdr, then reconcile the result. Workers
submit explicit attempt-tagged results; Herdr idle/Done never completes the task.
Require a configured deterministic check for the initial coding workflow. Persist
result artifacts, state transitions and interrupted-operation evidence.

Stopping halts new work and deliberately interrupts owned active work, reporting
stopping until termination is observed. It preserves worktrees and artifacts.
Resume reattaches/reconciles surviving processes and only relaunches when absence
and supported resume semantics are established. It does not promise every native
conversation can survive a cold server restart.

Exit: repeatable brief → worker → result → check → completion. Duplicate result
submission is idempotent; stale results fail; crashes before/after launch do not
silently duplicate workers; a failed check blocks completion. Board closure/client
detach is distinct from stopping the run. This is the first useful CLI milestone.

## Phase 3 — Manager, workers and communication

Deliver a manager session, scoped task assignment, acyclic dependencies and bounded
worker concurrency (default two). Add durable messages, per-recipient serialization,
acknowledgements, questions and task-attempt provenance. Keep delegation one level
deep and writer worktrees separate. Integrate the native Agent metadata/view support
from phase 1 into actual run state.

Use one built-in feature workflow: manager plans, workers implement, an independent
reviewer evaluates the candidate, deterministic checks establish readiness. Put
minimum review/check guards in application behavior now; phase 4 makes them reusable
definitions. The manager handles judgment but cannot declare required guards passed
through prose alone. Integrate worker changes serially and revalidate the combined
candidate; passing checks on separate branches does not validate their combination.

Exit: exercise manager + two workers, dependency release, a relayed question,
duplicate/ambiguous message delivery, worker interruption and reviewer rejection.
No worker enters a permission dialog through accidental prompt injection. Controller
reconnect recovers ownership without another manager claiming the same run. This
is the core orchestration alpha.

## Phase 4 — Versioned playbooks and blocking gates

Extract the proven feature workflow into a strict, versioned file format. Support
command, agent and explicit manual-decision steps; sequential flow and bounded
fan-out; conditions, timeouts and explicit safe retry policy. Add lifecycle bindings
at HOP-controlled transitions rather than treating Herdr events as veto hooks.

Freeze each run's playbook revision. Tie gate evidence to the candidate fingerprint
and relevant policy/check inputs. Persist step attempts and decisions. Unknown keys,
invalid references and dependency cycles fail validation before work starts.
Do not add nested playbooks, a canvas editor or arbitrary expression scripting yet.

Exit: the same workflow runs repeatedly with explicit inputs, setup failure prevents
worker launch, failed/stale gates prevent progress, retry does not repeat unsafe
effects, and stop/resume preserves step outcomes. Include inspection/validation
commands so users can understand execution without opening a board.

## Phase 5 — Account profiles and pools

Deliver common account login/list/status and pool management, starting with Claude
Code, Codex and opencode. Prefer native login/refresh and isolate harness profiles. Store
non-secret account metadata and credential references; preserve user settings.
Install Herdr integrations in the correct profile where supported.

Implement provider-specific membership, stable account identity, eligibility,
round-robin selection and account/session leases. Cursor movement, task claim,
account capacity and launch reservation commit atomically across concurrent runs.
Explicit account selection remains available. Live sessions keep their assigned
account; this is not request-level routing or automatic in-session account swapping.

Exit: simultaneous launches cannot overbook accounts; pool exhaustion queues work
with a reason; disabled/cooling-down accounts stop receiving new assignments;
lease release waits for runtime reconciliation; credentials/history do not bleed
between profiles. Account-correct restore follows the policy verified earlier.
Gemini/Grok adapters follow when their native compatibility evidence is sufficient.

## Phase 6 — Optional terminal board

Build a full-pane task list and open-agent action first, then details, questions,
gate evidence and account inspection. Reuse application read models and commands;
the board never imports sibling adapters. Native Agents view remains the lightweight
default for day-to-day navigation. Add overlays/splits only after the basic view works.

Exit: fixed-geometry PTY/UI checks cover narrow terminals, focus restoration,
keyboard behavior and stale/disconnected state. Closing the board does not stop
the controller or workers. All essential operations remain possible through CLI.
Do not delay a usable CLI release solely for optional board polish.

## Phase 7 — Release hardening and compatibility

Package reproducible binaries and a manifest with an evidence-based minimum Herdr
version. Validate installation/build/link behavior, schema migrations, retention,
version reporting and clean uninstall semantics. Test against the minimum supported
and current stable Herdr versions on each claimed OS/architecture.

Run recovery scenarios across controller kill, Herdr restart, worker exit, lost
subscriptions, ambiguous launch, stale results and concurrent runs. Record actionable
diagnostics without secrets. Verify native account integration only on explicitly
supported versions. Document known limitations and troubleshooting commands.

Exit: a clean installation completes the feature workflow, failed transitions retain
useful evidence, restarting does not silently duplicate work or switch accounts, and
package guides/install docs match the shipped binary. Add detached service management
only if foreground controller operation is inadequate; it is a separate lifecycle
feature, not a prerequisite for basic orchestration.

## First implementation backlog

The initial work should cover phases 0 and 1 in this order:

1. Resolve module/toolchain/platform pins; bootstrap `hop version` and build tooling.
2. Add architecture-check fixtures and the real-tree checker.
3. Add guide coverage, strict formatter/linter configuration and CI.
4. Introduce the minimal application ports and linkable plugin action.
5. Build the isolated Herdr process fixture and complete link/invoke/log cleanup.
6. Prove native metadata/view behavior and collect PTY navigation evidence.
7. Resolve launch environment and native profile/restore compatibility.

Avoid creating all ERD entities or the full CLI command tree up front. Each step
must name the behavior proved and any unsupported capability it exposes. Engineering
exit criteria are executable checks, not extra approval ceremonies.

## Scope held outside the initial release

Cross-client/team collaboration, model routing, request-level account balancing,
browser/editor embedding, native collapsible Herdr agent trees, recursive delegation,
nested playbooks, remote orchestration and a general integration marketplace remain
outside the initial release. Existing CLI/MCP tools can supply workflow-specific
capabilities without adding them to HOP's core.
