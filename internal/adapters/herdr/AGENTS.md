# internal/adapters/herdr

## Purpose

HOP's narrow client of the Herdr boundary. It speaks newline-delimited JSON
over the server's local Unix socket with response-ID correlation and typed
error mapping, and probes the installed binary (version, bundled API schema,
harness executables) for the application's doctor use case.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [client.go](client.go) | `Client`, `NewClient`, `Call`, `Subscribe`, `EventStream`, `EventSubscription`, `RawEvent` | One connection per call and per subscription, mirroring the herdr CLI; correlates the response ID, decodes results into caller structs, and streams pushed events in arrival order after the subscribe acknowledgement |
| [errors.go](errors.go) | `APIError`, `ProtocolError` | Server error responses keep their code and message; protocol violations (ID mismatch, non-protocol frames, oversized lines) are distinct from transport errors |
| [probe.go](probe.go) | `InstallationProbe`, `parseSchema` | Implements `app.Probe`: resolves executables, reads `--version` lines, extracts protocol and method constants from `herdr api schema --json`, pings the configured socket |
| [presentation.go](presentation.go) | `Presentation`, `NewPresentation` | Implements `app.AgentPresentation`: `pane.report_metadata` token patches, `agent.view.set` with a manager-first token sort and `agent.view.clear` — all under HOP's fixed source so its view is owned and clearable |
| [observation.go](observation.go) | `Observer`, `NewObserver` | Implements `app.Observer`: one `pane.agent_status_changed` subscription per watched pane, normalized into `app.StatusEvent`, and a `session.snapshot` reduced to `app.PaneObservation` |

## Invariants

- Response correlation is enforced before anything else: a response whose ID
  does not match the request is a `ProtocolError`, even when it carries an
  error body.
- Unknown JSON fields are ignored everywhere, per Herdr's protocol stability
  contract; unknown pushed event names are delivered as `RawEvent` values for
  the caller to interpret, never dropped by the transport.
- `Subscribe` buffers events from the acknowledgement on (channel capacity
  256, then backpressure), so a caller can subscribe first and snapshot
  second without losing a transition. After `Events()` closes, `Err()` is
  nil only for a deliberate `Close` or context cancellation.
- A line is capped at 64 MiB; a longer one is a protocol violation rather
  than unbounded memory growth.
- Probe subprocesses run with every `HERDR_*` variable scrubbed from the
  environment, so a session this process runs inside cannot be addressed by
  accident. A binary that exists but fails `--version` is reported found
  with an empty version, never invented.
- Schema method extraction walks the request schema structurally for
  `properties.method.const`, so layout changes inside that section do not
  hide methods; zero methods is an error, not an empty success.
- Presentation always reports under `app.PresentationSource` (`plugin:hop`),
  so metadata patches are attributable and `agent.view.clear` is scoped —
  another owner's projection is never disturbed. The view sort is by the
  padded `hop_run_order` then `hop_order` tokens, which is what makes the
  native, string-comparing sort render manager-first.
- Agent-status subscriptions are pane-scoped in Herdr, so an `Observer`
  watches a fixed pane set chosen at construction; the snapshot still covers
  every pane. Non-status pushed frames are dropped, and both the dot and
  underscore spellings of the status event are accepted.

## Dependencies and ports

- Allowed inward imports: [internal/app](../../app/AGENTS.md).
- Implemented ports: `app.Probe` by `InstallationProbe`,
  `app.AgentPresentation` by `Presentation`, `app.Observer` by `Observer`.
- External libraries: none; standard library only.

## Verification

- `go test ./internal/adapters/herdr` — protocol behavior against a scripted
  fake NDJSON endpoint on a real Unix socket (response IDs, error mapping,
  partial frames, disconnects, interleaved events, unknown fields,
  cancellation) and probe behavior against stub shell executables. These
  tests prove HOP behavior, not Herdr compatibility; that is
  the real-process suite's job.
- Test fixtures: none on disk; stubs and wire lines are written by the tests.

## Related guides

- [Parent index](../AGENTS.md)
- [Application layer and ports](../../app/AGENTS.md)
- [Herdr surface audit](../../../docs/architecture/herdr-surface.md)
