# internal/adapters/herdr

## Purpose

HOP's narrow client of the Herdr boundary. It speaks newline-delimited JSON
over the server's local Unix socket with response-ID correlation and typed
error mapping, probes the installed binary (version, bundled API schema,
harness executables) for the application's doctor use case, and drives
worktree creation, plain-workspace creation, worker-pane lifecycle and
occupant inspection for the worker-launch use case.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [client.go](client.go) | `Client`, `NewClient`, `Call`, `lifetimeObserver`, `Subscribe`, `EventStream`, `EventSubscription`, `RawEvent` | One connection per call and per subscription, mirroring the herdr CLI; correlates the response ID, decodes results into caller structs, tells a result implementing `lifetimeObserver` which server lifetime answered it (read on the call's own connection), streams pushed events in arrival order after the subscribe acknowledgement, and closes the subscription's connection exactly once through a shared `sync.Once` regardless of whether `Close`, context cancellation or the read pump's own exit triggers it first |
| [errors.go](errors.go) | `APIError`, `ProtocolError` | Server error responses keep their code and message; protocol violations (ID mismatch, non-protocol frames, oversized lines) are distinct from transport errors |
| [probe.go](probe.go) | `InstallationProbe`, `parseSchema` | Implements `app.Probe`: resolves executables, reads `--version` lines, extracts protocol and method constants from `herdr api schema --json`, pings the configured socket |
| [presentation.go](presentation.go) | `Presentation`, `NewPresentation` | Implements `app.AgentPresentation`: `pane.report_metadata` token patches, `agent.view.set` with a manager-first token sort and `agent.view.clear` — all under HOP's fixed source so its view is owned and clearable. A `pane_not_found` answer to `pane.report_metadata` ("pane <id> not found": an id the server no longer resolves, or a pane with no terminal) wraps both `app.ErrPaneNotFound` and `ErrPaneNotFound`; every other error keeps its own type |
| [observation.go](observation.go) | `Observer`, `NewObserver`, `statusStream`, `DrainRemaining`, `flushBacklog` | Implements `app.Observer`: one `pane.agent_status_changed` subscription per watched pane, normalized into `app.StatusEvent`, and a `session.snapshot` reduced to `app.PaneObservation`; on stop-intake, the decode pump moves its pending event and the rest of the raw backlog into an overflow slice that `DrainRemaining` exposes |
| [runtime.go](runtime.go) | `Runtime`, `NewRuntime`, `ErrPaneNotFound`, `ErrWorkspaceIDRequired` | Implements `app.Runtime`: `worktree.create` (cwd/branch/base, plus an S9 creation label sent only when the caller supplies one), `layout.apply` worker-pane creation, pane recovery by creation label via `session.snapshot`, `pane.read` scrollback capture, `pane.process_info` occupant inspection stamped with the answering server lifetime, `pane.close`, and `ServerInstance`'s dial-inspect-close server-lifetime lookup |
| [workspace.go](workspace.go) | `Runtime.CreateWorkspace`, `Runtime.FindWorkspaceByLabel`, `ErrWorkspaceCwdNotAbsolute`, `ErrWorkspaceLabelRequired` | Implements `app.WorkspaceRuntime` on the same `Runtime` type: `workspace.create` (explicit absolute cwd, additive env, a required unique creation label — an empty or relative cwd and an empty label are refused with the typed errors before any request is sent — focus always false) and its S8 recovery lookup — resolve the labeled workspace via `session.snapshot`, then descend to its sole tab and that tab's sole pane |
| [lifetime.go](lifetime.go) | `serverLifetime`, `stableLifetime` | The server-lifetime token (`herdr-server-lifetime/v1 pid=<pid> start=<sec>.<usec>`) of the process that accepted a connection, and the before/after agreement rule an observation is stamped under |
| [lifetime_darwin.go](lifetime_darwin.go), [lifetime_other.go](lifetime_other.go) | `serverLifetimeTag`, `peerPID`, `processLifetime`, `processStartTime`, `parseKinfoProcStart`, `kinfoProc` | GOOS-selected: on darwin `peerPID` reads a dialed connection's socket peer pid (`getsockopt(SOL_LOCAL, LOCAL_PEERPID)`) and `processLifetime` renders that pid with its start time from a raw `sysctl(CTL_KERN, KERN_PROC, KERN_PROC_PID, pid)` kinfo_proc record, read only when the record is exactly 648 bytes and carries the pid at offset 40; on every other platform both are unconditionally unknown (no executed probe pins a lifetime identity there) |

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
  more is an error, since creation labels are assumed unique. `panes` is
  schema-required on the snapshot, so an absent `panes` key is a
  `ProtocolError`, distinct from an explicit empty array — the legitimate
  "no panes yet" state — which is a genuine not-found.
- `Runtime.CreateWorktree` reports exactly what `worktree.create` returns
  (workspace, path, branch); it runs no git itself. Herdr's response has no
  base-commit field, so that provenance is resolved by application code
  through `CommandRunner`, never by this adapter.
- `worktree.create`'s wire request also carries a creation `label` (S9:
  Herdr accepts the same kind of creation label on `worktree.create` as on
  `workspace.create`, naming the new workspace), sent only when
  `app.WorktreeRequest.Label` is non-empty — every existing caller leaves it
  empty, so the wire shape is byte-identical to before this field existed.
  No `internal/app` call site sets it yet: per-attempt labeling (the
  operation UUID) and the worktree.create label-recovery decision row are
  application-layer work landing in slices 2b/6, not this adapter.
- `Runtime.CreateWorkspace` and `Runtime.FindWorkspaceByLabel` implement
  `app.WorkspaceRuntime` on the same `Runtime` type (its argument refusals —
  a non-absolute cwd, an empty label — are checked before any request and
  are driven by the shared `storevectors.WorkspaceRequestsRefused` vectors
  that internal/app's fake runtime refuses identically; a separate interface
  from `app.Runtime`, per that port's own doc comment) — no new adapter
  type, no change to `NewRuntime`. `CreateWorkspace` calls `workspace.create`
  with an explicit cwd, additive env and a unique creation label, `focus`
  always `false` regardless of anything the caller supplies (the port
  carries no focus field at all): S8-confirmed, this never steals the
  session's active workspace except that session's very first-ever one,
  which Herdr always activates regardless of the request. Label and env are
  wire-optional and omitted when the request carries none.
  `FindWorkspaceByLabel` reads a single `session.snapshot` and resolves the
  label against `snapshot.workspaces` — the label names the WORKSPACE
  (`WorkspaceInfo.label`), never a pane, unlike `FindPaneByLabel`'s
  pane-level labels — then descends to the resolved workspace's tabs
  (matched by `workspace_id`) and that tab's panes (matched by `tab_id`).
  An empty lookup label is refused before any request is sent, since Herdr
  never assigns one. Zero matching workspaces is `(zero value, false, nil)`;
  more than one workspace carrying the label, or anything other than
  exactly one tab or exactly one pane at either descent step (including
  zero — a workspace Herdr created always has both), is an error, never a
  guess. Every schema-required field the descent filters on — a workspace's
  label, a tab's `workspace_id`, a pane's `tab_id` — is decoded as a
  pointer and validated on EVERY record scanned, not only the eventual
  match: these fields are read to decide what matches, so an absent or
  empty one anywhere is a `ProtocolError`, never a silently-skipped
  non-match (an absent label decoding as `""` could otherwise let an
  empty lookup label match a malformed workspace).
- `ErrPaneNotFound` is a typed, `errors.Is`-checkable sentinel every
  pane-addressed `Runtime` method (`ReadPane`, `InspectPane`, `ClosePane`)
  maps Herdr's `pane_not_found` API error onto, through the
  shared `wrapPaneError` helper; every other error keeps its own type under
  the added pane-address context. `InspectPane` and `ClosePane`, whose port
  contracts report absence, map it through `wrapAbsentPaneError` instead,
  which wraps both `ErrPaneNotFound` and `app.ErrPaneNotFound`; `pane.close`
  resolves the id alone, so its `pane_not_found` means no pane has the id. For `InspectPane` specifically, Herdr
  returns this same code both for no such pane and for a pane that exists
  but has no live runtime yet (the delayed-restore window), so the caller
  reads it as "no runtime," not strictly "no such pane." `InspectPane`
  alone uses a second, dedicated helper, `wrapInspectPaneError`: the same
  `pane_not_found` mapping, but wrapping both `ErrPaneNotFound` and
  `app.ErrPaneNotFound` — the `app.Runtime` port's own "positively does not
  exist" absence contract — so `errors.Is(err, app.ErrPaneNotFound)` holds
  for that case and only that case; every other `InspectPane` failure
  (transport, protocol, a wrong-typed response) satisfies neither sentinel.
  `FindPaneByLabel`'s own not-found reporting is unrelated and unchanged by
  this: it is `(zero value, false, nil)`, never an error.
- `ServerInstance` dials the configured socket (no NDJSON frame is sent —
  a bare connect, `serverLifetime`, close, reusing `Client.dial`'s existing
  context handling so a wedged server cannot hang the call) and renders
  the lifetime of the process that accepted the connection:
  `"herdr-server-lifetime/v1 pid=<peer pid> start=<sec>.<usec>"`, the pid
  from the socket and the start time from that pid's kinfo_proc record.
  Herdr has no server-provided lifetime value, never re-execs a running
  server in place, and starts a new process for every restart and live
  handoff, so a token names exactly one server lifetime; a later process
  reusing the pid starts after the earlier holder exited and so carries a
  later start time (only a backwards wall-clock step landing on the exact
  microsecond could repeat a token). Any lookup failure, a peer that has
  exited, a kinfo_proc record of an unexpected size or carrying another
  pid, and every non-darwin platform yield `("", nil)` — unknown, never a
  fabricated value, and never equal to a recorded token. Only a dial
  failure is an error. A token of the earlier `peer-pid:<n>` format never
  equals a v1 token, so identities recorded by older runs compare as
  changed.
- `InspectPane` stamps `app.PaneProcess.ServerInstance` with the lifetime
  of the server that answered that very request: Herdr serves exactly one
  request per API connection, so `Client.Call` reads the peer pid once
  after connecting (the peer is fixed at connect, and Herdr closes the
  connection after answering) and the pid's lifetime before the request and
  after the response; the stamp is that lifetime when both reads agree and
  `""` otherwise (`stableLifetime`). A pid cannot be reused while its
  process lives, so agreeing reads mean one process served the whole
  exchange.
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

- Test files additionally import
  [internal/testsupport/storevectors](../../testsupport/storevectors/AGENTS.md)
  (the shared `WorkspaceRequestsRefused` vectors); production code never
  does.

- Allowed inward imports: [internal/app](../../app/AGENTS.md).
- Implemented ports: `app.Probe` by `InstallationProbe`,
  `app.AgentPresentation` by `Presentation`, `app.Observer` by `Observer`,
  `app.Runtime` by `Runtime`, `app.WorkspaceRuntime` by the same `Runtime`
  (a deliberately separate port interface — see `workspace.go`).
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
- `TestClientCallHonorsCancellation` and `TestRuntimeHonorsCancellation`'s
  fake endpoint holds every connection open, unread past the first line and
  unanswered, until the test itself ends — it must never close or respond
  first, since the real Herdr server never closes an idle request early.
  Reading that first line tolerates a clean, zero-byte EOF without failing
  the test: net's own Unix-socket dial code can accept-then-abandon a
  connection it already completed at the kernel level if the caller's
  context is canceled in the narrow window right after connect(2) succeeds,
  closing it before writing anything, which this fake's accept loop still
  receives. Any other read error (a non-empty partial line, for instance)
  still fails the test.
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
  cancellation across every method, `FindPaneByLabel`'s
  not-found/ambiguous/transport-error cases, `ServerInstance` and
  `InspectPane`'s stamp (on darwin the v1 token naming this process's
  pid, since the in-process temp Unix listener and the dialer are the same
  process, stable across calls; elsewhere `""`; a dead socket errors with
  an empty token) and
  `TestRuntimeInspectPaneAppErrPaneNotFoundClassification` (a table proving
  `errors.Is(err, app.ErrPaneNotFound)` holds only for the `pane_not_found`
  case, never for a transport, unrelated-API-code or protocol failure),
  with its siblings `TestRuntimeClosePaneAppErrPaneNotFoundClassification`
  and `TestPresentationReportMetadataNotFoundClassification`. The wire
  shapes these tables use for a vanished pane — `pane.process_info`
  answering `pane_not_found` "pane not found", `pane.report_metadata` and
  `pane.close` answering `pane_not_found` "pane <id> not found" — are
  pinned against a real server by test/integration's
  `TestSpikeVanishedPaneShapes`.
  `TestRuntimeCreateWorktreeSendsLabelWhenSet` proves the S9 label is sent
  only when the request carries one, alongside the pre-existing unlabeled
  fixture asserted byte-identical.
- `TestRuntimeCreateWorkspace*` and `TestRuntimeFindWorkspaceByLabel*` in
  [workspace_test.go](workspace_test.go) cover `CreateWorkspace` and
  `FindWorkspaceByLabel` the same way: full-structural request fixtures
  (including that `focus` is always sent as `false`, and that `label`/`env`
  are omitted when unset), `*MapsPartialResponse` tables, an unrelated API
  error passing through, and `FindWorkspaceByLabel`'s empty-input-label
  refusal (asserted to send no request at all), not-found, ambiguous label,
  ambiguous OR ZERO tab/pane at either descent step, transport-error,
  matched-record missing-required-id, and — separately — a missing or
  explicitly empty label/`workspace_id`/`tab_id` on a record the descent
  merely SCANS (not necessarily the eventual match), proving those
  schema-required filter fields fail closed rather than silently acting as
  a non-match; both methods are included in `TestRuntimeHonorsCancellation`.
- [lifetime_internal_test.go](lifetime_internal_test.go) (`package herdr`,
  same-package per the internal-algorithm testing guidance) —
  `TestServerLifetimeUnknownForNonSocketConn` proves `serverLifetime`
  reports `""` for a `net.Pipe` connection, which implements no
  `syscall.Conn` on any platform, and `TestStableLifetime` pins the
  before/after agreement rule.
  [lifetime_darwin_test.go](lifetime_darwin_test.go) pins the kinfo_proc
  layout: `TestProcessStartTimeMatchesTheProcessTable` compares the start
  second of this process and of a live child with `ps -o lstart=` (an
  independent source) and proves a reaped child's pid yields no start
  time; `TestParseKinfoProcStartRefusesUnknownLayouts` refuses records of
  another size or pid and out-of-range times. The production transport
  pins the token itself (test/integration `TestSpikeLabelSurvivesRestart`:
  the server leader's pid, `ps`'s start time, stable within a lifetime,
  different across a restart).
- [callsites_test.go](callsites_test.go) is the herdr-adapter half of the
  no-injection mechanism (docs/plan/phase-3-design.md section 11, part iii).
  `TestProductionCallSitesUseAllowlistedStringLiterals` type-checks every
  non-test `.go` file in this package with `go/types` (a stub importer
  resolves this package's own sibling import, `internal/app`, to an empty
  stand-in package, since only `Client`/`Call` — which depend on nothing
  from it — need to resolve correctly) and finds every reference this
  package makes to `(*Client).Call` BY TYPE — not by name or shape — so a
  method value assigned to a variable, a method expression, or any other
  indirection cannot hide from it the way a purely syntactic "is this a
  4-argument `Call(...)`" walk could.
  A method-expression reference is always rejected; a method value is
  accepted only when it is the immediate callee of a direct, 4-argument
  call whose `method` argument is a string literal on the reviewed
  allowlist. The six terminal-input methods (`pane.send_text`,
  `pane.send_keys`, `pane.send_input`, `agent.prompt`, `agent.send_keys`,
  `agent.start`, S11-confirmed as Herdr 0.9.0's complete real input
  surface) are never on that allowlist: slice 6 deleted the one staged
  exception this adapter ever carried, `pane.send_text` for the `SendText`
  port member itself, together with the member. The test cross-checks its
  own reference count
  against a plain syntactic count of `.Call`-named selectors and fails if
  they disagree, so a type-checking gap cannot silently under-cover.
  `TestCallSiteAllowlistRuleCatchesViolations` proves the rule actually
  catches what it claims to — including the method-value escape itself,
  both with a dynamic method and with an otherwise-allowed literal, a bare
  method-expression reference, a forbidden terminal-input method, an
  unlisted literal and a direct call with the wrong arity — against a
  synthetic self-contained package (its own `Client` type and `Call`
  method), rather than only passing vacuously against today's clean tree.
- Test fixtures: none on disk; stubs and wire lines are written by the tests.

## Related guides

- [Parent index](../AGENTS.md)
- [Application layer and ports](../../app/AGENTS.md)
- [Herdr surface audit](../../../docs/architecture/herdr-surface.md)
