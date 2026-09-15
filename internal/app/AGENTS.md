# internal/app

## Purpose

Application layer: HOP's use cases and the ports they consume. This slice
carries the Phase 1 doctor, plugin-invocation, presentation and observation
use cases, plus the Phase 2 durable single-worker run controller: run,
status, stop, resume, result submission and the deterministic check, driven
through consumer-owned ports against a lease-fenced store, Herdr's runtime
surface, local processes and repository configuration. Ports are declared
here and implemented by adapters; only `cmd/hop` and tests wire the two
sides together. `cmd/hop` never imports domain or identity types: every
`Controller` method takes and returns primitives or `app`-defined DTOs
(`docs/plan/phase-2-design.md` section 8).

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [ports.go](ports.go) | `Probe`, `BinaryInfo`, `SchemaInfo`, `ServerInfo` | Consumer-owned port for inspecting the Herdr installation, plus the observation values it returns |
| [doctor.go](doctor.go) | `Doctor`, `Report`, `Check`, `CheckStatus`, `requiredFeatures`, `supportedHarnesses` | Builds a report of checks: herdr binary, bundled API schema, per-feature method support, configured server socket, and the fixed harness set (Claude Code, Codex, opencode) |
| [invocation.go](invocation.go) | `Invocation`, `InvocationFromEnviron`, `Trigger`, `Validate`, `Describe`, `ErrNotPluginInvocation` | Reads the `HERDR_*` plugin environment into a value, names the trigger, validates it and renders deterministic report lines |
| [presentation.go](presentation.go) | `AgentPresentation`, `Presenter`, `AgentDisplay`, `PaneMetadata`, `ViewSelection`, `Role`, `SortDisplays`, token constants | Consumer-owned presentation port and its ordering: a display's manager-first `hop_order` key and padded token map, and the view selection HOP installs |
| [observation.go](observation.go) | `Observer`, `StatusStream`, `PaneObservation`, `StatusEvent`, `Reconcile`, `ReconcileState`, `AgentStatus` | Consumer-owned observation port and the no-replay reconciliation: subscribe first, snapshot second, fold buffered events plus `StatusStream.DrainRemaining()`'s accepted backlog onto the snapshot |
| [store.go](store.go) | `StateStore`, `UnitOfWork`, `Lease`, `NewRunSpec`, `RunSnapshot`, `<Entity>Repository` (Runs, Tasks, Attempts, Sessions, Worktrees, Results, Artifacts, Bindings, LaunchClaims, CheckExecClaims, Operations, Transitions, CheckRequests), `Operation`, `OperationKind`/`OperationState`, `Transition`, `CheckRequest`, `CheckExecClaim`, `ErrRevisionConflict`, `ErrFenced`, `ErrNotFound`, `ErrLeaseHeld` | The controller's authority: lease lifecycle (`InitializeRun`, `AcquireLease`, `Heartbeat`, `ReleaseLease`, CAS-fenced) and fenced units of work over typed repositories; the operation journal's shapes. `OperationRepository.ByKind` lists a run's operations of one kind, newest first, for recovery evidence |
| [readstore.go](readstore.go) | `ReadStore`, `RunStatus`, `RunDetail`, `CheckExecutionSummary`, `FrozenRun`, `LaunchContext`, `CheckExecutionContext` | Lease-free reads for `hop status` and both exec boundaries. `RunDetail` carries the identities stop and resume need, the frozen `StateRoot` for evidence capture, and `LastCheck` (the newest check execution's identity, state and retained evidence); `FrozenRun` is the check use case's frozen-input source; `LaunchContext` is resolvable from the run snapshot and the recorded launch intent alone, never from a binding that may not exist yet |
| [submission.go](submission.go) | `SubmissionStore`, `LaunchClaim`, `LaunchClaimState`, `LaunchClaimSettlement`, `LaunchClaimRepository`, `ResultSubmission`, `SubmissionOutcome`, `SubmissionOutcomeKind`, `ClaimedSubmission`, `ClaimedSubmissionFieldLimit` | Worker-authority writes with no controller lease: launch/check-exec claims, `SubmitResult`'s atomic section 7 handoff, `RecordMalformed` for a submission that failed parsing before any typed identity existed, monotonic stop requests |
| [runtime.go](runtime.go) | `Runtime`, `ErrPaneNotFound`, `WorktreeRequest`/`WorktreeInfo`, `WorkerPaneRequest`, `PaneHandle`/`PaneRef`, `ProcessInfo`, `PaneProcess` | Herdr's pane and worktree surface: `CreateWorktree`, `OpenWorkerPane` (a `layout.apply` command pane), `FindPaneByLabel` recovery, `SendText` fallback, `ReadPane` evidence, `InspectPane` occupant identity (reporting the typed `ErrPaneNotFound` for a pane that positively does not exist — distinct from any inspection failure), `ClosePane`, and `ServerInstance` — the opaque identity scoped to both the configured socket and the server process behind it, compared by equality only |
| [artifactstore.go](artifactstore.go) | `ArtifactStore` | Durable local file writes/reads under the run's artifact directories (assignment, pane scrollback, check stdout/stderr), temp-file-then-rename, never called from inside a `StateStore` transaction |
| [system.go](system.go) | `Clock`, `IDGenerator` | Explicit time and identity generation |
| [process.go](process.go) | `CommandRunner`, `Command`, `CommandResult`, `ProcessGroupInspector`, `GroupProcess` | Process-group-leader execution with cancellation (git operations, spawning `hop check-exec`) and local process-table listing/signaling for group retirement |
| [configuration.go](configuration.go) | `ConfigurationSource`, `RunPolicy` | Loads and validates one repository's `.herdr-orchestrator/config.toml`-decoded policy: check contract, env strip/passthrough, profile dir, harness |
| [digest.go](digest.go) | `ResultDigestTag`, `ComputeResultDigest` | The canonical `"hop-result-v1"` result digest: length-prefixed fields, SHA-256 hex, computed only here — the domain receives it as an opaque validated string |
| [decision.go](decision.go) | `LaunchClaimDeadline`, `LaunchDeadlineExpired`, `LaunchSettlement`, `CorroborateSettlement`, `FirstMarkerMatch`, `OccupantMatches`, `ServerContinuityEstablished`, `GroupRetirementOutcome`, `ClassifyGroupRetirement`, `ArgvUnavailable` | The section 6 claim-corroboration predicate (claim-derived executable identity, durable marker set, explicit `hop launch` exclusion, fail-closed on missing identity), close-rule occupant matching, the server-continuity predicate, and the four-outcome process-group-retirement classifier matching both the frozen check argv and its check-exec invocation |
| [operation_payload.go](operation_payload.go) | `decodeOperationPayload` | Reads a persisted operation intent/evidence/outcome payload without relying on Go type identity: a value of the target type passes through, anything else round-trips through JSON. Callers still validate required fields and fail closed on a failed decode |
| [controller.go](controller.go) | `Controller`, `RunHandle`, `Heartbeat`, `Detach`, `ErrStopRequested` | The driving service composition calls; ports as fields. `RunHandle` is an opaque per-run token (run identity, the held lease and a dispatch scope) so composition never touches identity types. `Heartbeat` extends the lease on the design's interval and cancels in-flight external calls on failure; `Detach` journals, cancels and releases without stopping; `revalidateForDispatch` is the section 4 step 2 revalidation every external mutation runs first |
| [assignment.go](assignment.go) | `renderAssignment` | Deterministic assignment-artifact content: brief, identities and absolute paths only, referenced by the launch argv, never typed into a dialog |
| [usecase_run.go](usecase_run.go) | `StartRun`, `StartRunRequest`, `StartRunResult`, `ErrStartRefused`, `classifyWorktreeProvenance` | Refuses unsupported repositories (SHA-256 object format, launch-line-hostile HOP paths) and unusable policies before any side effect — every such refusal wraps `ErrStartRefused`, hop run's usage exit — freezes the run snapshot, calls `InitializeRun`, writes/records the assignment artifact, then drives `worktree.create` (base commit resolved to an object id before the intent commits; canonical common-directory EQUALITY plus frozen-base HEAD comparison validate provenance) and `pane.open` as record-intent/act/record-outcome units |
| [usecase_launch.go](usecase_launch.go) | `CorroborateLaunch`, `LaunchProgress` | One inspection round toward settling a launch claim under the section 6 predicate: settled, needs-interaction (forking wrapper), failed (`exec_failed`, terminating the session), already-settled (early acceptance; activates a still-launching session) or still pending — recovers a lost binding by creation label, never sleeps or resends |
| [usecase_stop.go](usecase_stop.go) | `RequestStop`, `DriveStop`, `StopReport`, `CheckRunIntent`, `closePaneOperation` | One stop round: retire every check execution's group under the group-retirement rule and the recorded worker under the shared pane.close operation procedure, and mark stopped ONLY once all owned work is observed absent; mismatches and failed inspections stay outstanding, never absent |
| [usecase_resume.go](usecase_resume.go) | `Resume`, `ResumeRequest`, `ResumeResult`, `ResumeOutcome` | Acquires a new fencing generation (idempotently for a resuming run; NothingToDo for a terminal one), recovers pending operations per the decision table (worktree adoption by provenance, pane adoption by label, check retirement by claim/group, persisted bounded waits), then reconciles per section 5: corroboration for launching attempts, warm adoption of settled claims under the one predicate with the run's state restored atomically, positive-evidence retirement through the pane.close procedure under a distinct observation incarnation, continuity-gated `--confirm-absent` attestation journaled as `absence.attested`, and lineage/workspace-validated cold relaunch |
| [usecase_submit.go](usecase_submit.go) | `SubmitResult`, `SubmitResultRequest`, `SubmitResultResult` | Section 7 step 1 (parse and bound the inputs) and the canonical digest, computed here; steps 2-5 are `SubmissionStore.SubmitResult`'s contract |
| [usecase_check.go](usecase_check.go) | `ClaimAndRunCheck`, `CheckReport`, `CheckClaimDeadline` | Recovers unresolved executions first (claim/group retirement, confirmed absence, then the unknown-outcome rule; bounded no-claim ambiguity), reopens orphaned claimed requests, then claims the oldest pending request atomically with its execution intent and lifecycle transitions, validates and materializes the detached checkout (submodule candidates fail clearly), spawns `hop check-exec` bounded by the frozen timeout, retains stdout/stderr evidence, and applies the section 7 outcome transaction including stop precedence and request settlement |
| [usecase_execboundary.go](usecase_execboundary.go) | `PrepareLaunchExec`, `LaunchExecRequest`/`LaunchExecPlan`, `FailLaunchExec`, `PrepareCheckExec`, `CheckExecRequest`/`CheckExecPlan`, `CheckSpawnEnvironment`, `ExecutableLookup` | The string-facing exec-boundary use cases behind `hop launch` and `hop check-exec` (composition passes raw strings; typed IDs are parsed here): section 6 launch preparation — HOP_* environment validation against the launch context, sanitization under the frozen policy, per-harness argv composition (Claude only; the pre-assigned native reference via `--session-id` plus the fixed initial prompt, `--resume` on cold relaunch), executable resolution through the composition-supplied `ExecutableLookup`, then `ClaimLaunch` — and section 7 check-exec preparation (group-leadership check, `ClaimCheckExec` BEFORE anything else, frozen-argv verification, sanitized env). The exec itself stays in `cmd/hop` through the process adapter; `FailLaunchExec` is the post-claim failure path and `CheckSpawnEnvironment` composes ClaimAndRunCheck's sanitized spawn env |
| [usecase_status.go](usecase_status.go) | `Status`, `StatusRequest`, `StatusResult`, `RunSummaryView`, `RunDetailView` | Renders `ReadStore` into string-only view DTOs for `hop status`, including the last check execution's identity, evidence paths and the human's options for an unknown outcome |

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
- The run controller follows the section 4 transaction rule throughout:
  every controller-side effect is record-intent (one `UnitOfWork`,
  committed), then act (the external call, never inside a transaction), then
  record-outcome (a second `UnitOfWork`). Immediately before every external
  mutation, `revalidateForDispatch` runs a fresh heartbeat CAS and re-reads
  the stop flag under a fenced unit of work; a refused revalidation leaves
  the committed intent pending for recovery, and unstarted work is never
  started after a stop request (`ErrStopRequested`). A failed heartbeat
  cancels the handle's dispatch scope, so in-flight external calls (a long
  check command included) are canceled with the lost lease.
- A unit of work's closure always returns nil on a successful write even
  when the write records a failure or ambiguous outcome
  (`OperationFailed`, `OperationReconciling`); returning a non-nil error
  from the closure rolls back everything the closure staged, including that
  outcome, so the causal error is surfaced to the caller only after
  `Commit` succeeds.
- `pane.open`'s intent transaction carries `IncarnationID` and `SessionID` in
  its JSON payload under the stable keys `incarnation_id` and `session_id`:
  `SubmissionStore.ClaimLaunch`'s pre-binding fallback matches against these
  fields on the newest pending `pane.open` operation for the attempt.
  Persisted intents and outcomes are decoded only through
  `decodeOperationPayload`, never Go type identity, and callers validate
  required fields and fail closed on a failed decode.
- The section 6 corroboration predicate (`CorroborateSettlement`) is the
  ONLY adoption rule, used identically by settlement, warm adoption and
  reattach: the expected executable comes from the claim itself, the
  accepted argv markers come from durable launch/binding context (run,
  attempt and incarnation identifiers plus the session's native reference),
  a `hop launch` invocation is explicitly excluded, and missing identity or
  an empty marker set is unresolved — never settled. Adoption requires a
  settled claim; an exec_pending claim settles through the ordinary fenced
  corroboration path first.
- Every close runs the shared pane.close operation procedure
  (`closePaneOperation`): the intent freezes the exact target evidence
  (pane, label, session/incarnation, pid, durable argv markers, reason)
  before any act; pane scrollback is captured through the `ArtifactStore`
  before the close (a refused evidence commit aborts the close, and a
  fresh revalidation runs immediately before the mutation, which
  dispatches under the handle's cancelable act context); the outcome
  commits only on OBSERVED absence and supersedes the target's binding.
  Reuse of an unresolved close decodes and uses the PERSISTED target in
  full — never a value derived from the current occupant — so a changed
  pid under the same marker is never closed under an old row; recovery
  adopts an already-gone target, re-acts exactly once against the same
  positively matched target, and stays reconciling on mismatch. A
  dispatched close or group signal is never itself termination:
  `DriveStop` reports stopped only once the recorded worker (including a
  worker recovered by label when no binding was ever committed; an
  unobserved pre-claim launch window stays outstanding) and every check
  group are observed absent.
- Pane absence is one strict rule (`observePaneAbsence`): the pane must be
  POSITIVELY absent by id (`ErrPaneNotFound`) AND by creation label, each
  a successful observation. Any inspection or lookup error is ambiguous
  with the error named, and an empty-foreground pane that still answers by
  id or by label is never absence.
- The group-retirement rule signals only a `GroupMatched` listing, matching
  both the frozen check argv and the `hop check-exec` invocation, and a
  signal failure is returned as reconciliation evidence, never discarded.
- Resume entry is idempotent (a resuming run is not re-transitioned; a
  terminal run is NothingToDo), unknown run or attempt states fail closed,
  and a held stop request always routes back to stop handling
  (`ResumeStopPending`) before any adoption or new dispatch — a stopping
  run is never turned back toward running. Pending operations from prior
  generations are recovered per the decision table before any
  reconciliation decision, with bounded waits persisted as act evidence
  and enforced across rounds; nothing is ever re-sent while an intent is
  unresolved, and recovery returns a BLOCKING disposition — unknown
  operation kinds, undecodable payloads, still-unresolved check
  executions — that refuses startup continuation, retirement and every
  cold-relaunch path while it stands. A positive-evidence retirement whose
  close outcome committed durably finishes the cold recovery on a later
  round even after its binding was superseded. Applying a settled claim's
  lifecycle consequences is historical catch-up, never live adoption: the
  occupant is verified under the predicate before any warm report, and an
  exec_pending claim on a reconciling attempt settles under the same
  inspected predicate. Startup continuation takes its repository root and
  brief from `FrozenRun` and verifies (or recreates byte-identically,
  failing closed on digest mismatch) the assignment artifact before any
  worker is launched.
- A worktree is adopted by provenance, never path existence: canonical
  absolute git common directories of the intended repository and the
  candidate must be EQUAL, and the candidate's HEAD must equal the base
  commit object id frozen into the intent. An unrelated checkout is a
  failure; absence, ambiguity and transport errors stay reconciling.
- A worktree adopted after its create response was lost has no recorded
  workspace placement; the worker pane is then NOT opened (its workspace
  would be a guess), and resume reports the run reconciling with that exact
  reason. Continuing startup and cold relaunch require a recorded
  placement: the current binding's workspace, or the worktree.create
  outcome's.
- A cold relaunch validates its inputs before any transition commits: the
  replacement session is created with the prior session's immutable native
  reference (Claude's `--resume` cannot be rendered without it) and a
  recorded workspace placement; the restored-observed binding row is
  minted under a DISTINCT observation incarnation — the store's
  UNIQUE(session_id, incarnation_id) key is never reused — and superseded
  only after the retirement is observed.
- `hop resume --confirm-absent` journals an `absence.attested` operation
  (both required assertions plus the recorded/observed server-continuity
  evidence) and authorizes cold relaunch only when
  `ServerContinuityEstablished` holds — both instance tokens non-empty and
  equal; under the socket-scoped `ServerInstance` contract token equality
  is the whole predicate — with the pane already positively absent by id
  and by label; unknown continuity, inspection errors and a changed
  instance keep the run resuming with the item-2 report. The server
  instance is observed through `Runtime.ServerInstance` immediately before
  pane creation, frozen into the pane.open intent (`server_instance`) and
  recorded in the creation binding; recovery by label copies the
  CREATION-time value from the intent, never a fresh observation, so
  continuity across a restart cannot be fabricated. An undecodable pending
  launch payload blocks retirement.
- The check request is claimed atomically with its execution intent
  (carrying the result id and the pre-resolved candidate tree object id)
  and the lifecycle transitions; execution inputs come from `FrozenRun`,
  never the caller; the frozen timeout bounds the spawned command; a spawn
  error is ambiguous (reconciling), never an immediate unknown; and the
  unknown-outcome rule runs only after a claimed group's confirmed
  absence, with stop precedence applied INSIDE its transaction (a held
  stop settles the execution and request and interrupts task and attempt,
  leaving the run to stop handling). Repeatable requeues the request for a
  fresh execution from the same checking attempt; unrepeatable fails the
  task and attempt and settles the request, but the run fails only after
  its worker's termination has been observed. A stop-retired check settles
  its request too. Check stdout/stderr are retained as result-linked
  artifact rows on EVERY command result — an ambiguous spawn's partial
  output included — and retention is mandatory: a retention failure fails
  the execution rather than letting completion claim lost evidence, and
  the checkout is cleaned up (with revalidation, under the act context)
  only after retention and a recorded outcome. Status options for an
  unknown outcome name only implemented actions.
- `Session.Terminate` is invalid directly from `active` (a stop against a
  corroborated live process goes through `stopping` first); the shared
  `terminateSession` helper transitions through `Stop` first when needed
  and is idempotent once a session is already `Terminated`. The session
  moves to `stopping` only for a stop-reason close of its own worker; a
  positive-evidence retirement targets a foreign restored occupant and
  leaves the session for the relaunch to mark lost.

## Dependencies and ports

- Allowed inward imports: [internal/domain/identity](../domain/identity/AGENTS.md),
  [internal/domain/run](../domain/run/AGENTS.md) (application code;
  standard library only beyond these, no third-party dependencies).
- Consumed ports and their adapters: `Probe`, `AgentPresentation`,
  `Observer` and `Runtime` (including `ServerInstance`) are implemented by
  [internal/adapters/herdr](../adapters/herdr/AGENTS.md); `StateStore`,
  `ReadStore` and `SubmissionStore` by
  [internal/adapters/sqlite](../adapters/sqlite/AGENTS.md); `CommandRunner`
  and `ProcessGroupInspector` by
  [internal/adapters/process](../adapters/process/AGENTS.md); `Clock`,
  `IDGenerator` and `ArtifactStore` by
  [internal/adapters/system](../adapters/system/AGENTS.md);
  `ConfigurationSource` by
  [internal/adapters/config](../adapters/config/AGENTS.md). Only `cmd/hop`
  wires the two sides together.
- External libraries: none.

## Verification

- `go test ./internal/app` — table-driven doctor report behavior against a
  handwritten `Probe` fake, invocation parsing/trigger/validation/output,
  the presentation token and manager-first ordering logic, and reconciliation
  against handwritten `AgentPresentation` and `Observer` fakes.
  `TestReconcileDrainsBufferedEventsBeforeCancellation` and
  `TestReconcileFoldsDrainRemainingAfterChannelEvents` prove the Phase 1
  drain behavior end to end.
- The Phase 2 run-controller scenario suite runs against `fakeStore` (one
  handwritten `StateStore`/`ReadStore`/`SubmissionStore` over shared
  in-memory state), `fakeRuntime`, `fakeArtifacts`, `fakeCommands`,
  `fakeGroups` and `fakeConfig` (`fakes_test.go`, `fakes_uow_test.go`,
  `fakes_ports_test.go`). The fakes enforce the store contracts the
  scenarios rely on, proven by `TestFakeStoreContracts`: commits validate
  everything before applying anything (revision recheck against the rows
  the transaction first read, strict lease expiry, binding-key uniqueness,
  claim-settlement legality, operation-payload serializability failing the
  commit atomically), `SubmitResult` applies existence/agreement before
  receipts and moves the same row revisions the real acceptance and stop
  transactions do (`TestWorkerWritesMoveRevisions`), `ClaimLaunch`
  enforces the stop/incarnation-currency/different-pid rules with the
  pre-binding intent fallback, `ClaimCheckExec` requires a pending check
  operation of the current generation, and every Runtime, CommandRunner,
  ProcessGroupInspector and ArtifactStore fake refuses any call made while
  a unit of work is open (`TestFakePortsRefuseCallsInsideTransactions`).
- Named scenario coverage, by test:
  `TestLeaseFencing` (takeover barrier with staged writes discarded,
  heartbeat/release CAS refusals, monotonic generations) and
  `TestDispatchRevalidation` (the controller-level A/B dispatch barrier —
  an act whose intent A committed is never dispatched after B takes over —
  heartbeat failure canceling an in-flight check command, and stop refusing
  non-stopping dispatch); `TestDetach` (release without stop);
  `TestStartRun` (effect ordering, worktree provenance incl. the
  wrong-repository-prefix and wrong-base rejections, frozen base object
  id, transport errors left reconciling, label recovery, refused object
  format and HOP path); `TestCorroborateLaunch` (the four settlement
  outcomes, native-reference markers, pre-binding label recovery,
  exec-failure session termination, early-acceptance session activation);
  `TestCorroborateSettlement`/`TestOccupantMatches`/
  `TestClassifyGroupRetirement` (the pure decision tables incl. launcher
  exclusion, fail-closed empty identity, dual check/check-exec argv);
  `TestDriveStop` and `TestPaneCloseInterruption` (observed termination,
  mismatch-never-absence, the pane.close operation's interruption windows
  on both sides of the act); `TestSubmitResult` (submission order incl.
  the early-acceptance handoff); `TestClaimAndRunCheck` (atomic claiming,
  ambiguous spawn errors, unknown-outcome recovery under both repeatable
  settings, frozen argv/timeout, submodule rejection, evidence retention
  with result-linked rows and the status summary);
  `TestResumeDuringCheck` and `TestOrphanedClaimedCheckRequest` (takeover
  reads the check-exec claim; requeued second execution; reopened orphaned
  claims); `TestResume` (warm adoption via the one predicate only,
  executable replacement and missing claims failing closed, cold relaunch
  superseding stale intents); `TestResumeOperationRecovery` (all four
  worktree/pane crash windows with persisted bounded waits and
  never-resend); `TestResumeRounds` (fail-closed→warm restoring all four
  entities; warm→submit→passing check to completed);
  `TestResumePositiveEvidenceRetirement` (the complete section 5 case 2);
  `TestResumeAttestation` (continuity-gated `--confirm-absent`, refusal on
  changed/unknown instance, delayed restore retired by positive evidence);
  and the four section 5 reference traces as complete four-entity app
  scenarios (`TestReferenceTraceSubmitBeforeRunning` with its
  acceptance-wins-first alternative,
  `TestReferenceTraceColdRelaunchSubmitBeforeRunning`,
  `TestReferenceTraceExecFailureAfterRelaunch`,
  `TestReferenceTraceStopDuringLaunching`).
- `go test ./internal/app -run 'TestPrepareLaunchExec|TestFailLaunchExec|TestPrepareCheckExec|TestCheckSpawnEnvironment'` —
  the exec-boundary tables against minimal handwritten ReadStore and
  SubmissionStore stubs: launch environment validation (variable named,
  value never echoed), the no-claim guarantee on every pre-claim refusal,
  per-harness argv composition and the unsupported non-Claude states, the
  claim's recorded executable/digest/pid, check-exec's claim-before-load
  ordering, group-leadership refusal and the frozen argv kept verbatim.
- `go test ./internal/app -run 'TestComputeResultDigest|TestDecodeOperationPayload'` —
  the canonical digest vectors (`digest_test.go`) and the persisted-payload
  decode contract (`operation_payload_internal_test.go`, a same-package
  test of `decodeOperationPayload`'s typed and JSON-generic paths).
- Test fixtures: none on disk; the fakes and environ slices live in the
  test files. No real process, file, socket or SQLite access anywhere in
  this package's tests.

## Launch-environment sanitization ([launchenv.go](launchenv.go))

Credential-handling core of the sanitizing launcher (Phase 2 design,
section 6). Symbols: `EnvPolicy` (Version, Harness, Strip, Passthrough,
ProfileDir), `EnvPolicy.Validate() (ValidatedEnvPolicy, error)`,
`ValidatedEnvPolicy`, `SanitizeEnvironment(environ, policy) (env, removed)`,
the version constant `EnvPolicyVersion1`, the harness constants
`HarnessClaude`/`HarnessCodex`/`HarnessOpencode`, and `StripMatrixV1()`.
Its only consumers are the exec-boundary use cases in
[usecase_execboundary.go](usecase_execboundary.go), behind `hop launch`
and `hop check-exec`; controller use-case code never calls it.

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
- [internal/domain/identity](../domain/identity/AGENTS.md)
- [internal/domain/run](../domain/run/AGENTS.md)
- [Herdr adapter](../adapters/herdr/AGENTS.md)
- [Composition root](../../cmd/hop/AGENTS.md)
- [Architecture and package catalog](../../docs/architecture/architecture.md)
- [Phase 2 design](../../docs/plan/phase-2-design.md)
