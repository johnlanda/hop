# internal/app

## Purpose

Application layer: HOP's use cases and the ports they consume. This slice
carries the doctor use case (is the local Herdr installation usable?) and the
plugin-invocation use case (what did Herdr inject into a plugin command?).
Ports are declared here and implemented by adapters; only `cmd/hop` and tests
wire the two sides together.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [ports.go](ports.go) | `Probe`, `BinaryInfo`, `SchemaInfo`, `ServerInfo` | Consumer-owned port for inspecting the Herdr installation, plus the observation values it returns |
| [doctor.go](doctor.go) | `Doctor`, `Report`, `Check`, `CheckStatus`, `requiredFeatures`, `supportedHarnesses` | Builds a report of checks: herdr binary, bundled API schema, per-feature method support, configured server socket, and the fixed harness set (Claude Code, Codex, opencode) |
| [invocation.go](invocation.go) | `Invocation`, `InvocationFromEnviron`, `Trigger`, `Validate`, `Describe`, `ErrNotPluginInvocation` | Reads the `HERDR_*` plugin environment into a value, names the trigger, validates it and renders deterministic report lines |
| [presentation.go](presentation.go) | `AgentPresentation`, `Presenter`, `AgentDisplay`, `PaneMetadata`, `ViewSelection`, `Role`, `SortDisplays`, token constants | Consumer-owned presentation port and its ordering: a display's manager-first `hop_order` key and padded token map, and the view selection HOP installs |
| [observation.go](observation.go) | `Observer`, `StatusStream`, `PaneObservation`, `StatusEvent`, `Reconcile`, `ReconcileState`, `AgentStatus` | Consumer-owned observation port and the no-replay reconciliation: subscribe first, snapshot second, fold buffered events plus `StatusStream.DrainRemaining()`'s accepted backlog onto the snapshot |

## Invariants

- The doctor reports observations, never guesses: a capability that could not
  be checked is `skipped` or `unavailable` with the reason, and a feature is
  `ok` only when the schema advertises every required method. `unsupported`
  and `unavailable` are the only statuses that make `Report.Healthy` false.
- An absent harness is `not found` and never unhealthy; HOP orchestrates with
  the harnesses that exist. Harness coverage is fixed at Claude Code, Codex
  and opencode for now.
- A missing herdr binary skips the schema and feature checks instead of
  repeating the same root cause per check.
- `Describe` output is deterministic: a fixed line order and an explicit
  `(unset)` marker, so plugin logs are comparable across runs.
- This package is pure coordination: no filesystem, network, process or
  environment access; observations arrive through ports and `Invocation`
  values are parsed from an environ slice the caller supplies.
- Presentation is manager-first and deterministic: a manager's ordering key
  ranks before every other role, numeric keys are zero-padded so Herdr's
  string sort orders them numerically, and an unset optional field is cleared
  (published as a delete), so publishing a full display is a complete
  replacement of HOP's optional tokens rather than a sparse patch that leaves
  stale values. View selection is one server-wide action under HOP's source;
  clearing is owned. `SortDisplays` mirrors the token sort and adds a pane-ID
  tie-break for HOP-side determinism; Herdr's own view keeps incoming order
  for equal keys, so the two agree only up to that tie-break. Ordering keys
  assume run and worker sequences in `[0, 999999]`; larger or negative values
  are out of the validated range.
- Reconciliation assumes no event replay: a caller subscribes before it
  snapshots, and `ReconcileState` folds the buffered events onto the snapshot
  with the last writer winning per pane. `Reconcile` prefers a buffered event
  over cancellation, drains `Events()` until it closes, and then appends
  `StatusStream.DrainRemaining()` on both the cancellation and the normal
  channel-close exit paths, so a transition observed before the cutoff is
  never dropped — including one still held behind a full channel when the
  consumer was descheduled.
- `StatusStream.DrainRemaining` returns events a subscription accepted before
  it ended but could not deliver through `Events`; it is meaningful only
  after `Events` has closed, and implementations return nothing when every
  accepted event was already delivered.

## Intentionally deferred

The `Runtime`, `Clock` and `IDGenerator` ports the architecture proposes are
not declared here, and the `internal/adapters/system` and `internal/adapters/cli`
packages are not created, because this slice has no consumer for them: there
is no worker-launch use case or domain yet, commands stay thin in `cmd/hop`,
and request IDs are an adapter-internal counter. They arrive with their first
consumer, per the engineering standard of not adding speculative ports or
packages.

## Dependencies and ports

- Allowed inward imports: none (application code; standard library only, no
  third-party dependencies).
- Consumed ports: `Probe`, `AgentPresentation` and `Observer`, declared here
  and implemented by [internal/adapters/herdr](../adapters/herdr/AGENTS.md).
- External libraries: none.

## Verification

- `go test ./internal/app` — table-driven doctor report behavior against a
  handwritten `Probe` fake, invocation parsing/trigger/validation/output,
  the presentation token and manager-first ordering logic, and reconciliation
  against handwritten `AgentPresentation` and `Observer` fakes.
  `TestReconcileDrainsBufferedEventsBeforeCancellation` proves `Reconcile`
  drains `Events()` fully until it closes rather than stopping at the first
  cancellation it observes, against a staged fake stream that only ever hands
  over one event at a time.
  `TestReconcileFoldsDrainRemainingAfterChannelEvents` proves the
  `DrainRemaining` fold itself against a fake stream with both channel events
  and a non-empty overflow, on both exit paths (channel closed normally,
  context canceled): the overflow is appended after the channel events, so
  its transition for a pane the channel already touched wins and its
  transition for a pane the channel never mentioned still appears. It is also
  proven end-to-end against the real adapter; see
  [internal/adapters/herdr/AGENTS.md](../adapters/herdr/AGENTS.md).
- Test fixtures: none on disk; the fakes and environ slices live in the
  test files.

## Launch-environment sanitization ([launchenv.go](launchenv.go))

Credential-handling core of the sanitizing launcher (Phase 2 design,
section 6). Symbols: `EnvPolicy` (Version, Harness, Strip, Passthrough,
ProfileDir), `EnvPolicy.Validate() (ValidatedEnvPolicy, error)`,
`ValidatedEnvPolicy`, `SanitizeEnvironment(environ, policy) (env, removed)`,
the version constant `EnvPolicyVersion1`, the harness constants
`HarnessClaude`/`HarnessCodex`/`HarnessOpencode`, and `StripMatrixV1()`.
Nothing imports it yet: its intended consumers are the exec-boundary
commands (`hop launch`, `hop check-exec`), to be wired in `cmd/hop` by
task 6a; controller use-case code never calls it.

- `StripMatrixV1` is the design's version-scoped default strip matrix,
  per harness family; `SanitizeEnvironment` removes the union of every
  family regardless of the launched harness. A harness version drift needs
  a re-verified new revision, never an edit to revision 1.
- Precedence is strip < passthrough < profile: custom strip entries extend
  the matrix, passthrough entries keep their inherited values, and a
  configured profile directory assigns the launched harness's documented
  profile shape last (`CLAUDE_CONFIG_DIR`; `CODEX_HOME`; for opencode
  `HOME` plus all four `XDG_*_HOME` variables — never a flat directory).
  `HERDR_*` and `HOP_*` variables are never stripped.
- Parse-don't-validate: `SanitizeEnvironment` accepts only
  `ValidatedEnvPolicy`, constructible (non-zero) solely through
  `Validate`, which rejects an unknown matrix revision, an unsupported
  harness, entries HOP does not accept as variable names (empty,
  containing `=`, or containing a control byte — NUL cannot appear in a
  native environment string; the other bytes below 0x20 are disallowed by
  HOP policy), and a profile directory that carries a control byte or is
  not absolute (the config adapter resolves profile paths before
  freezing). Validation errors identify a
  rejected entry by list, position and name portion only — never anything
  after an entry's first `=`. The zero `ValidatedEnvPolicy` sanitizes as
  the strictest policy: full union stripped, no passthrough, no profile.
- The function is pure and deterministic: entries resolve by name (split
  at the first `=`; a name-only entry is legal and rendered verbatim),
  matching is exact and case-sensitive with no trimming, the last
  duplicate wins at its inherited position by HOP's documented policy
  (POSIX leaves duplicate resolution undefined), input is never mutated,
  and `removed` carries variable names only, never values.
- Verification: `go test ./internal/app -run 'TestEnvPolicy|TestStripMatrix|TestSanitizeEnvironment'` —
  matrix-equality, validation, precedence, per-harness profile-mapping,
  environ-shape and strictest-zero-policy tables in
  [launchenv_test.go](launchenv_test.go).

## Related guides

- [Parent index](../AGENTS.md)
- [Herdr adapter](../adapters/herdr/AGENTS.md)
- [Composition root](../../cmd/hop/AGENTS.md)
- [Architecture and package catalog](../../docs/architecture/architecture.md)
