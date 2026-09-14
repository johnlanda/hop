# internal/adapters/herdr

## Purpose

HOP's narrow client of the Herdr boundary. It speaks newline-delimited JSON
over the server's local Unix socket with response-ID correlation and typed
error mapping, probes the installed binary (version, bundled API schema,
harness executables) for the application's doctor use case, and drives
worktree creation, worker-pane lifecycle and occupant inspection for the
worker-launch use case.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [client.go](client.go) | `Client`, `NewClient`, `Call`, `Subscribe`, `EventStream`, `EventSubscription`, `RawEvent` | One connection per call and per subscription, mirroring the herdr CLI; correlates the response ID, decodes results into caller structs, streams pushed events in arrival order after the subscribe acknowledgement, and closes the subscription's connection exactly once through a shared `sync.Once` regardless of whether `Close`, context cancellation or the read pump's own exit triggers it first |
| [errors.go](errors.go) | `APIError`, `ProtocolError` | Server error responses keep their code and message; protocol violations (ID mismatch, non-protocol frames, oversized lines) are distinct from transport errors |
| [probe.go](probe.go) | `InstallationProbe`, `parseSchema` | Implements `app.Probe`: resolves executables, reads `--version` lines, extracts protocol and method constants from `herdr api schema --json`, pings the configured socket |
| [presentation.go](presentation.go) | `Presentation`, `NewPresentation` | Implements `app.AgentPresentation`: `pane.report_metadata` token patches, `agent.view.set` with a manager-first token sort and `agent.view.clear` — all under HOP's fixed source so its view is owned and clearable |
| [observation.go](observation.go) | `Observer`, `NewObserver`, `statusStream`, `DrainRemaining`, `flushBacklog` | Implements `app.Observer`: one `pane.agent_status_changed` subscription per watched pane, normalized into `app.StatusEvent`, and a `session.snapshot` reduced to `app.PaneObservation`; on stop-intake, the decode pump moves its pending event and the rest of the raw backlog into an overflow slice that `DrainRemaining` exposes |
| [runtime.go](runtime.go) | `Runtime`, `NewRuntime`, `ErrPaneNotFound`, `ErrWorkspaceIDRequired` | Implements `app.Runtime`: `worktree.create`, `layout.apply` worker-pane creation, pane recovery by creation label via `session.snapshot`, the `pane.send_text` fallback transport, `pane.read` scrollback capture, `pane.process_info` occupant inspection and `pane.close` |

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
- Stopping intake (`Close` or context cancellation) never discards what was
  already accepted: `statusStream.pump` moves its pending event and the rest
  of the raw subscription's buffered events into an overflow slice, and
  `DrainRemaining` returns it. Every event accepted into the raw queue before
  cancellation is folded, independently of when the consumer resumes
  reading — this holds even with both the raw and normalized buffers full at
  once.
- `EventStream`'s connection is closed exactly once through a shared
  `sync.Once` (`closeConn`), shared by `Close`, the context-cancellation
  watcher and the read pump's own `finish`. `Close` waits for the read pump
  to exit before returning, so it always reports nil for an intentional
  shutdown rather than racing the pump for a `net.ErrClosed` result.
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
- `Runtime.OpenWorkerPane` never sends `layout.apply`'s `tab_id`: naming one
  replaces that tab, where omitting it (with `workspace_id` only) adds
  exactly one tab and leaves every other tab and pane in that workspace
  untouched (Phase 2 capability spike, S6). `focus` is always `false`, so
  opening a worker pane never steals attention. `WorkspaceID` is required —
  `layout.apply` has no way to create a workspace on its own, so an empty
  `WorkspaceID` fails closed with `ErrWorkspaceIDRequired` before any
  request is sent, rather than silently addressing whichever workspace is
  active.
- `Runtime.FindPaneByLabel` reads only `session.snapshot`, never `tab.list`:
  `OpenWorkerPane`'s creation label is a `layout.apply` pane-node label, and
  `session.snapshot` pane records already carry the workspace, tab and pane
  ids alongside it (S7). Zero matches is `(zero value, false, nil)`; two or
  more is an error, since creation labels are assumed unique.
- `Runtime.CreateWorktree` reports exactly what `worktree.create` returns
  (workspace, path, branch); it runs no git itself. Herdr's response has no
  base-commit field, so that provenance is resolved by application code
  through `CommandRunner`, never by this adapter.
- `ErrPaneNotFound` is a typed, `errors.Is`-checkable sentinel every
  pane-addressed `Runtime` method (`SendText`, `ReadPane`, `InspectPane`,
  `ClosePane`) maps Herdr's `pane_not_found` API error onto, through the
  shared `wrapPaneError` helper; every other error keeps its own type under
  the added pane-address context. For `InspectPane` specifically, Herdr
  returns this same code both for no such pane and for a pane that exists
  but has no live runtime yet (the delayed-restore window), so the caller
  reads it as "no runtime," not strictly "no such pane."
- `Runtime.ClosePane` only issues `pane.close`; there is no
  occupant-conditioned or compare-and-swap close upstream (S2), so the close
  rule (re-inspect and match occupant evidence immediately before closing)
  is enforced by application code around this method, never inside it.
- Every result-bearing `Runtime` method validates its response before
  returning: the result's `type` discriminator must match the expected one
  (`worktree_created`, `layout_apply`, `session_snapshot`, `pane_read`,
  `pane_process_info`), and every field the port actually consumes — an id,
  a path, a required process field — must be present, using a nil pointer
  in the wire struct to detect an absent key distinctly from one present
  with Go's zero value. A mismatch or an absent required field is a
  `ProtocolError` naming it; a zero-value id, path or pane handle never
  escapes the adapter as a false success. A JSON field the wire struct does
  not declare — a schema-optional field this port does not consume, or a
  field a newer server added — is tolerated and ignored, matching the
  unknown-fields invariant above; only fields the port actually reads are
  validated. A field the schema itself marks optional (worktree branch, a
  pane's label, shell pid, foreground process group id, argv0/argv/cmdline/
  cwd) keeps Go's zero value when absent — that is its correct, intentional
  representation, not a decode error.

## Dependencies and ports

- Allowed inward imports: [internal/app](../../app/AGENTS.md).
- Implemented ports: `app.Probe` by `InstallationProbe`,
  `app.AgentPresentation` by `Presentation`, `app.Observer` by `Observer`,
  `app.Runtime` by `Runtime`.
- External libraries: none; standard library only.

## Verification

- `go test ./internal/adapters/herdr` — protocol behavior against a scripted
  fake NDJSON endpoint on a real Unix socket (response IDs, error mapping,
  partial frames, disconnects, interleaved events, unknown fields,
  cancellation) and probe behavior against stub shell executables. These
  tests prove HOP behavior, not Herdr compatibility; that is
  the real-process suite's job. `TestReconcileFoldsBothLayerBacklogsOnCancellation`
  saturates the real raw and normalized buffers together, cancels once both
  pumps are parked, and proves the accepted backlog is still folded through
  `DrainRemaining`; `TestReconcileFoldsRawBufferedEventsOnCancellation` covers
  the raw-only case. `TestObserverCloseReleasesBlockedPump` and
  `TestObserverCancellationReleasesBlockedPump` prove a pump blocked on a full
  channel is released, and that `Close`/cancellation report success rather
  than racing the pump's own connection close.
- `TestRuntime*` in [runtime_test.go](runtime_test.go) cover every `Runtime`
  method against a fake NDJSON endpoint. Request shapes are asserted by
  `assertRequestParams`: full structural equality against a literal JSON
  fixture — the exact key set (no extras, none missing), equal values and
  JSON types at every level, and array elements in the given order, so a
  renamed, dropped, added or reordered-in-a-meaningful-way field fails;
  object key order carries no JSON meaning and is not asserted. Response
  coverage includes nominal decoding, a schema-optional field's correct
  absence-as-zero-value, `*MapsPartialResponse` tables per result-bearing
  method (wrong `type` discriminator, a missing required wrapper or leaf,
  an ambiguous match), `ErrPaneNotFound`/`ErrWorkspaceIDRequired` mapping,
  an unrelated API error code passing through unwrapped, a malformed
  response and a non-string response id surfacing as the `Client`'s own
  `ProtocolError`, a wrong-typed field (`argv` as a string) rejected by the
  standard decoder itself before any adapter validation runs, context
  cancellation across every method, and `FindPaneByLabel`'s
  not-found/ambiguous/transport-error cases.
- Test fixtures: none on disk; stubs and wire lines are written by the tests.

## Related guides

- [Parent index](../AGENTS.md)
- [Application layer and ports](../../app/AGENTS.md)
- [Herdr surface audit](../../../docs/architecture/herdr-surface.md)
