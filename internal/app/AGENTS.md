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
| [observation.go](observation.go) | `Observer`, `StatusStream`, `PaneObservation`, `StatusEvent`, `Reconcile`, `ReconcileState`, `AgentStatus` | Consumer-owned observation port and the no-replay reconciliation: subscribe first, snapshot second, fold buffered events onto the snapshot |

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
  string sort orders them numerically, and empty optional tokens are omitted
  so non-HOP agents stay readable. View selection is one server-wide action
  under HOP's source; clearing is owned.
- Reconciliation assumes no event replay: a caller subscribes before it
  snapshots, and `ReconcileState` folds the buffered events onto the snapshot
  with the last writer winning per pane.

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
- Test fixtures: none on disk; the fakes and environ slices live in the
  test files.

## Related guides

- [Parent index](../AGENTS.md)
- [Herdr adapter](../adapters/herdr/AGENTS.md)
- [Composition root](../../cmd/hop/AGENTS.md)
- [Architecture and package catalog](../../docs/architecture/architecture.md)
