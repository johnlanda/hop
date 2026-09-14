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
| [store.go](store.go) | `StateStore`, `UnitOfWork`, `Lease`, `NewRunSpec`, `RunSnapshot`, `<Entity>Repository` (Runs, Tasks, Attempts, Sessions, Worktrees, Results, Artifacts, Bindings, LaunchClaims, CheckExecClaims, Operations, Transitions, CheckRequests), `Operation`, `OperationKind`/`OperationState`, `Transition`, `CheckRequest`, `CheckExecClaim`, `ErrRevisionConflict`, `ErrFenced`, `ErrNotFound`, `ErrLeaseHeld` | The controller's authority: lease lifecycle (`InitializeRun`, `AcquireLease`, `Heartbeat`, `ReleaseLease`, CAS-fenced) and fenced units of work over typed repositories; the operation journal's shapes |
| [readstore.go](readstore.go) | `ReadStore`, `RunStatus`, `RunDetail`, `LaunchContext`, `CheckExecutionContext` | Lease-free reads for `hop status` and both exec boundaries; `RunDetail` carries the identities (`TaskID`, `AttemptID`, `SessionID`) stop and resume need, and `LaunchContext` is resolvable from the run snapshot and the recorded launch intent alone, never from a binding that may not exist yet |
| [submission.go](submission.go) | `SubmissionStore`, `LaunchClaim`, `LaunchClaimState`, `LaunchClaimSettlement`, `LaunchClaimRepository`, `ResultSubmission`, `SubmissionOutcome`, `SubmissionOutcomeKind`, `ClaimedSubmission`, `ClaimedSubmissionFieldLimit` | Worker-authority writes with no controller lease: launch/check-exec claims, `SubmitResult`'s atomic section 7 handoff, `RecordMalformed` for a submission that failed parsing before any typed identity existed, monotonic stop requests |
| [runtime.go](runtime.go) | `Runtime`, `WorktreeRequest`/`WorktreeInfo`, `WorkerPaneRequest`, `PaneHandle`/`PaneRef`, `ProcessInfo`, `PaneProcess` | Herdr's pane and worktree surface: `CreateWorktree`, `OpenWorkerPane` (a `layout.apply` command pane), `FindPaneByLabel` recovery, `SendText` fallback, `ReadPane` evidence, `InspectPane` occupant identity, `ClosePane` |
| [artifactstore.go](artifactstore.go) | `ArtifactStore` | Durable local file writes/reads under the run's artifact directories (assignment, pane snapshots, check stdout/stderr), temp-file-then-rename, never called from inside a `StateStore` transaction |
| [system.go](system.go) | `Clock`, `IDGenerator` | Explicit time and identity generation; the worker-launch use case is their first consumer |
| [process.go](process.go) | `CommandRunner`, `Command`, `CommandResult`, `ProcessGroupInspector`, `GroupProcess` | Process-group-leader execution with cancellation (git operations, spawning `hop check-exec`) and local process-table listing/signaling for group retirement |
| [configuration.go](configuration.go) | `ConfigurationSource`, `RunPolicy` | Loads and validates one repository's `.herdr-orchestrator/config.toml`-decoded policy: check contract, env strip/passthrough, profile dir, harness |
| [digest.go](digest.go) | `ResultDigestTag`, `ComputeResultDigest` | The canonical `"hop-result-v1"` result digest: length-prefixed fields, SHA-256 hex, computed only here — the domain receives it as an opaque validated string |
| [decision.go](decision.go) | `LaunchClaimDeadline`, `LaunchDeadlineExpired`, `LaunchSettlement`, `CorroborateSettlement`, `OccupantMatches`, `GroupRetirementOutcome`, `ClassifyGroupRetirement`, `ArgvUnavailable` | The section 6 claim-corroboration predicate, close-rule occupant matching, and the four-outcome process-group-retirement classifier (including the process adapter's sleep-anchor and unreadable-argv cases) |
| [controller.go](controller.go) | `Controller`, `RunHandle` | The driving service composition calls; ports as fields. `RunHandle` is an opaque per-run token (run identity plus the held lease) so composition never touches identity types |
| [assignment.go](assignment.go) | `renderAssignment` | Deterministic assignment-artifact content: brief, identities and absolute paths only, referenced by the launch argv, never typed into a dialog |
| [usecase_run.go](usecase_run.go) | `StartRun`, `StartRunRequest`, `StartRunResult` | Freezes the run snapshot, calls `InitializeRun`, writes/records the assignment artifact, then drives `worktree.create` and `pane.open` as separate record-intent/act/record-outcome units; `pane.open`'s intent is also the run's single "launch intent" moment |
| [usecase_launch.go](usecase_launch.go) | `CorroborateLaunch`, `LaunchProgress` | One inspection round toward settling a launch claim under the section 6 predicate: settled, needs-interaction (forking wrapper), failed (`exec_failed`), or still pending — never sleeps or resends |
| [usecase_stop.go](usecase_stop.go) | `RequestStop`, `DriveStop`, `StopReport`, `CheckRunIntent` | One round of stop interruption per attempt state (reserved, launching, running/submitted, checking), each idempotent and fail-closed on any identity mismatch or inspection failure |
| [usecase_resume.go](usecase_resume.go) | `Resume`, `ResumeRequest`, `ResumeResult`, `ResumeOutcome` | Acquires a new fencing generation and performs one section 5 reconciliation round: warm reattach, positive-evidence retirement into cold relaunch, fail-closed, or `--confirm-absent`-gated cold relaunch; retires the prior session's pending launch intents and marks it lost before the replacement session's own intent commits |
| [usecase_submit.go](usecase_submit.go) | `SubmitResult`, `SubmitResultRequest`, `SubmitResultResult` | Section 7 step 1 (parse and bound the inputs) and the canonical digest, computed here; steps 2-5 are `SubmissionStore.SubmitResult`'s contract |
| [usecase_check.go](usecase_check.go) | `ClaimAndRunCheck`, `CheckReport` | Claims the oldest pending check request, materializes and validates a detached checkout via `CommandRunner`, spawns `hop check-exec`, and applies the section 7 outcome transaction including stop precedence and the unknown-outcome rule |
| [usecase_status.go](usecase_status.go) | `Status`, `StatusRequest`, `StatusResult`, `RunSummaryView`, `RunDetailView` | Renders `ReadStore` into string-only view DTOs for `hop status` |

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
  record-outcome (a second `UnitOfWork`). A unit of work's closure always
  returns nil on a successful write even when the write records a failure
  or ambiguous outcome (`OperationFailed`, `OperationReconciling`); returning
  a non-nil error from the closure rolls back everything the closure staged,
  including that outcome, so the causal error is surfaced to the caller only
  after `Commit` succeeds.
- `pane.open`'s intent transaction carries `IncarnationID` and `SessionID` in
  its JSON payload under the stable keys `incarnation_id` and `session_id`:
  `SubmissionStore.ClaimLaunch`'s pre-binding fallback (a launcher claiming
  before the pane.open outcome — and therefore the runtime binding — has
  committed) matches against these fields on the newest pending `pane.open`
  operation for the attempt, since the binding a launcher would otherwise be
  validated against need not exist yet.
- `openWorkerPane` (initial launch) and `openRelaunchPane` (cold relaunch)
  share one core parameterized by `run.LaunchKind`: the initial launch's
  intent transaction performs the Run/Task/Attempt/Session transitions
  (`applyLaunchIntent`), while a relaunch's equivalent transitions already
  happened in `coldRelaunch`'s own transaction, so its intent step only
  journals the operation — applying them twice is an invalid second
  transition on values already at their target state.
- A cold relaunch never revives the prior session: in the same transaction
  that creates the replacement session, it marks the prior session `Lost`
  and retires (`OperationReconciling`) every still-pending `pane.open`
  intent that named it, so a retired incarnation's launcher can never claim
  against a stale intent.
- The close rule (immediately before `ClosePane`, re-inspect and match
  occupant argv/pid against recorded evidence) and the group-retirement rule
  (list, classify, signal only on `GroupMatched`, never blindly) are applied
  identically by stop and resume; both fail closed on any mismatch, missing
  identity or inspection failure rather than acting on ambiguity.
- `Session.Terminate` is invalid directly from `active` (a stop against a
  corroborated live process goes through `stopping` first); the shared
  `terminateSession` helper transitions through `Stop` first when needed and
  is idempotent once a session is already `Terminated`.

## Not yet implemented

Every port here is a consumer-owned interface with no adapter behind it yet:
`internal/adapters/sqlite` (`StateStore`/`ReadStore`/`SubmissionStore`),
`internal/adapters/system` (`Clock`/`IDGenerator`/`ArtifactStore`),
`internal/adapters/process` (`CommandRunner`/`ProcessGroupInspector`),
`internal/adapters/config` (`ConfigurationSource`) and the `Runtime`
extension to `internal/adapters/herdr` are separate tasks. `cmd/hop`'s
command wiring, the state-root resolver and the launch-line/HOP-path
validation the CLI surface needs are also separate (composition, task 6a).

## Dependencies and ports

- Allowed inward imports: [internal/domain/identity](../domain/identity/AGENTS.md),
  [internal/domain/run](../domain/run/AGENTS.md) (application code;
  standard library only beyond these, no third-party dependencies).
- Consumed ports: `Probe`, `AgentPresentation` and `Observer` (Phase 1),
  implemented by [internal/adapters/herdr](../adapters/herdr/AGENTS.md); the
  Phase 2 ports above (`StateStore`, `ReadStore`, `SubmissionStore`,
  `Runtime`, `ArtifactStore`, `Clock`, `IDGenerator`, `CommandRunner`,
  `ProcessGroupInspector`, `ConfigurationSource`) have no adapter yet.
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
- `go test ./internal/app -run 'TestStartRun|TestCorroborateLaunch|TestDriveStop|TestResume|TestSubmitResult|TestClaimAndRunCheck|TestLeaseFencing'` —
  the Phase 2 run-controller scenario suite, against `fakeStore` (one
  handwritten `StateStore`/`ReadStore`/`SubmissionStore` over shared
  in-memory state with a real optimistic-concurrency, lease-fenced unit of
  work), `fakeRuntime`, `fakeArtifacts`, `fakeCommands`, `fakeGroups` and
  `fakeConfig` (`fakes_test.go`, `fakes_uow_test.go`, `fakes_ports_test.go`).
  Covers: effect ordering (intent committed before the external act) and
  the crash windows on both sides of an act (intent-committed-act-unknown;
  act-done-outcome-unrecorded, left `reconciling`, never resent); worktree
  and pane-open recovery by creation label; the four launch-settlement
  outcomes; stop for every attempt state (reserved, launching, running,
  checking), each idempotent and fail-closed on mismatch; submission order
  (accepted, duplicate, conflicting, transient, the early-acceptance
  handoff, malformed via `RecordMalformed`); check gating (passing, failing,
  the repeatable and unrepeatable unknown-outcome rules, stop precedence);
  resume's warm-reattach, fail-closed, cold-relaunch and unsupported-harness
  cases; and `TestLeaseFencing`'s takeover barrier (a unit of work opened
  under a lease a later `AcquireLease` has superseded cannot commit even
  though its writes already ran) plus heartbeat/release CAS refusals.
- `go test ./internal/app -run 'TestComputeResultDigest|TestCorroborateSettlement|TestOccupantMatches|TestClassifyGroupRetirement'` —
  the pure decision-function tables in `digest_test.go` and
  `decision_test.go`.
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
- [internal/domain/identity](../domain/identity/AGENTS.md)
- [internal/domain/run](../domain/run/AGENTS.md)
- [Herdr adapter](../adapters/herdr/AGENTS.md)
- [Composition root](../../cmd/hop/AGENTS.md)
- [Architecture and package catalog](../../docs/architecture/architecture.md)
- [Phase 2 design](../../docs/plan/phase-2-design.md)
