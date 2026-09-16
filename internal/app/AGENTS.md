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
| [store.go](store.go) | `StateStore`, `UnitOfWork`, `Lease`, `NewRunSpec`, `RunSnapshot`, `WorkflowSnapshot`, `<Entity>Repository` (Runs, Tasks, Attempts, Sessions, Worktrees, Results, Artifacts, Bindings, LaunchClaims, CheckExecClaims, Operations, Transitions, CheckRequests), `Operation`, `OperationKind`/`OperationState`, `Transition`, `CheckRequest`, `CheckExecClaim`, `ErrRevisionConflict`, `ErrFenced`, `ErrNotFound`, `ErrLeaseHeld` | The controller's authority: lease lifecycle (`InitializeRun`, `AcquireLease`, `Heartbeat`, `ReleaseLease`, CAS-fenced) and fenced units of work over typed repositories; the operation journal's shapes. `OperationRepository.ByKind` lists a run's operations of one kind, newest first, for recovery evidence. `WorktreeRepository` rows are unlinked for a solo run (`ByRun` serves that shape only) and linked to their attempt and verified base commit in feature mode; `Create` refuses a link naming an unknown attempt (`ErrNotFound`) or another run's attempt (`ErrFenced`). `RunSnapshot.Workflow` (`WorkflowSnapshot`) is a feature-mode run's frozen `[workflow]`/`[workers]`/`[retry]`/`[roles]`/`[messages]` policy plus the computed integration branch name and the frozen base commit (`BaseCommitOID`); the zero value means solo. A feature-mode `NewRunSpec` describes the MANAGER session and carries no task/attempt/worktree identity: `ValidateFeatureRunSpec` (`ErrFeatureRunSpecInvalid`) and `RequireIntegrationBranchForSequence` (`ErrRunSequenceMismatch`) are the shared refusals every `StateStore` applies |
| [workflow.go](workflow.go) | `WorkflowRepositories`, `WorkflowReadStore`, `RequireWorkflowRepositories`, `RequireWorkflowReadStore`, `ErrWorkflowRepositoriesUnsupported`, `ErrWorkflowReadStoreUnsupported`, `ErrFeatureModeUnsupported`, `TaskDependencyRepository`, `TaskIndexRepository`, `AttemptIndexRepository`, `SessionIndexRepository`, `WorktreeIndexRepository`, `MessageRepository`, `ReviewRepository`, `IntegrationRepository`, `RetryRequestRepository`, `RetryRequestRecord`, `SessionLaunchContext`, `MessagingContext`, `MessageDetail` | The Phase 3 controller-side capability extensions: `WorkflowRepositories` and `WorkflowReadStore` are declared as SEPARATE interfaces from `UnitOfWork`/`ReadStore` (never new methods on them) so the existing SQLite adapter keeps compiling until it also implements them; every feature-mode use case type-asserts to reach them and fails closed with the typed sentinel errors on a mismatch, never a nil-interface panic. Solo-mode code never performs the assertion. `WorkflowReadStore.LoadMessageDetail` is `hop msg show`'s entire lookup (envelope plus full delivery/ack history, `MessageDetail`): read-only, no lease, callable by any of the run's sessions or the human context — deliberately NOT on `MessageRepository`/`WorkflowRepositories`, which is the worker-authority, controller-transaction-only path `ShowMessage` (usecase_message.go) never uses. `MessagingContext.RunID` is the session's OWN run, never the caller-supplied one: every driving messaging use case (`SendMessage`/`FetchMessage`/`AckMessage`) compares it against its own parsed RunID and refuses a mismatch before any side effect — the use-case half of the section 7 cross-run authorization check `MessagingStore`'s own methods independently re-enforce (messaging.go). `TaskIndexRepository.Create` is controller-transaction task creation under the lease fence, used for review tasks (section 8 — only the controller creates them); `PlanStore.CreateTask` remains the worker-authority path and still refuses `kind=review`. `WorktreeIndexRepository.ByAttempt` is the feature-mode worktree lookup (the newest row linked to an attempt, `ErrNotFound` otherwise; an unlinked solo row or a sibling attempt's row never answers), which the per-attempt worktree.create recovery and launch continuation use because `WorktreeRepository.ByRun` serves only the solo one-row shape. `MessageRepository.PendingByAddress` is the mailbox-closure snapshot set (queued + delivered-unacknowledged IDs) read INSIDE the caller's transaction, so a settlement's obligation snapshot and its final equality check never escape the unit of work through a lease-free read port |
| [messaging.go](messaging.go) | `MessagingStore`, `MessageSend`, `MessageFetch`, `MessageDelivery`, `MessageAck`, `MessageOutcome`/`MessageOutcomeKind`, `MessageAckOutcome`/`MessageAckOutcomeKind`, `HumanAnswer`, `AddressString`, `MessageBodyFileLimit`, `MessageBodyInlineLimit`, `ErrMessagingUnauthorized` | Section 7's worker-authority messaging port and its DTOs: send/fetch/ack/answer, request-ID idempotency, relay provenance, the `origin` resolution for a forwarded human answer. Every method independently re-derives the caller session's own run (and, for fetch, its current incarnation and resolved address) rather than trusting the request's caller-supplied fields — `ErrMessagingUnauthorized` is `FetchNextMessage`'s refusal signal (it has no outcome-kind field); `SendMessage`/`AckMessage` express the identical check through their own outcome kind (`MessageRefused`/`AckRefused`) |
| [plan.go](plan.go) | `PlanStore`, `TaskCreate`/`TaskCreated`, `RetryRequest`/`RetryAccepted`, `PlanClose`/`PlanCloseResult`, `WorkflowOutcomeKind`, `TaskTitleLimit`, `TaskInstructionsLimit` | Section 8's worker-authority manager-plan port and its DTOs: `hop task create`/`hop task retry`/`hop plan close`, each manager-only and run-state-gated inside its own transaction |
| [review.go](review.go) | `ReviewStore`, `ReviewSubmission`, `ReviewOutcome`/`ReviewOutcomeKind` | Section 8's worker-authority review-verdict port and its DTOs; the accepting transaction's guard/retirement wiring is driving-controller behavior owned by a later slice |
| [readstore.go](readstore.go) | `ReadStore`, `RunStatus`, `RunDetail`, `CheckExecutionSummary`, `FrozenRun`, `CheckExecutionContext`, `TaskSummary`, `IntegrationSummary`, `MailboxStatus`, `InFlightMessage`, `PendingQuestion`, `SessionSummary` | Lease-free reads for `hop status` and both exec boundaries. `RunDetail` carries the identities stop and resume need, the frozen `StateRoot` for evidence capture, and `LastCheck` (the newest check execution's identity, state and retained evidence); `FrozenRun` is the check use case's frozen-input source. Phase 3 additions to `RunDetail`, all additive fields, empty for a solo run: `Tasks` (the feature-mode task table), `LatestIntegration` (the run's most recently CREATED integration row — reading exactly what is recorded, never resolving the live git ref, which is 2b's integration-operations concern), `GuardShortfalls` (`EvaluateReadiness`'s missing list, rendered verbatim), `Mailboxes` (section 7's per-address queue-depth/in-flight-age status surface, `Attention` computed once here against the frozen `[messages] attention_after` threshold), `PendingQuestions` and (STATUS-1) `Sessions` — every session the run has ever created, its role, state, current binding and (for a delegated child) the task and attempt number it is bound to; a plain additive struct field (unlike the packaging rule below, which is about widening a PORT), so `LoadRunStatus`'s existing signature and every other `ReadStore` implementer are untouched. `RunDetail.TargetBranch` and `RunDetail.WorktreesRetiredAt` are the frozen worktree-retirement target and the run's worktrees-retired fact (nil until set); a feature run's `RunDetail.Worktrees` (every row, oldest first) and `RunDetail.WorktreeRetirements` (its `worktree.retire` operations, newest first) are the per-row status lines' input, both nil for a solo run. Slice 6 deleted the Phase 2 run-keyed `LoadLaunchContext`/`LaunchContext` in favor of `WorkflowReadStore.LoadSessionLaunchContext` — `hop launch` is session-addressed for every role now, the permanent solo shim included |
| [submission.go](submission.go) | `SubmissionStore`, `LaunchClaim`, `LaunchClaimState`, `LaunchClaimSettlement`, `LaunchClaimRepository`, `ResultSubmission`, `SubmissionOutcome`, `SubmissionOutcomeKind`, `ClaimedSubmission`, `ClaimedSubmissionFieldLimit` | Worker-authority writes with no controller lease: launch/check-exec claims, `SubmitResult`'s atomic section 7 handoff, `RecordMalformed` for a submission that failed parsing before any typed identity existed, monotonic stop requests. `LaunchClaim.SessionID` (required; `AttemptID` now optional, empty for an attempt-less session) is the Phase 3 session-keyed claim addition; `LaunchClaim.SeedEvidence` records the workspace-trust pre-seeding outcome (evidence only — no decision ever reads it) |
| [runtime.go](runtime.go) | `Runtime`, `ErrPaneNotFound`, `WorktreeRequest`/`WorktreeInfo`, `WorkerPaneRequest`, `PaneHandle`/`PaneRef`, `ProcessInfo`, `PaneProcess`, `WorkspaceRuntime`, `WorkspaceRequest`/`WorkspaceHandle`/`WorkspaceRef` | Herdr's pane and worktree surface: `CreateWorktree`, `OpenWorkerPane` (a `layout.apply` command pane), `FindPaneByLabel` recovery, `ReadPane` evidence, `InspectPane` occupant identity (reporting the typed `ErrPaneNotFound` for a pane that positively does not exist — distinct from any inspection failure), `ClosePane`, and `ServerInstance` — the opaque identity scoped to both the configured socket and the server process behind it, compared by equality only. `SendText` (the fixed-grammar launch-line fallback transport) was deleted in slice 6: HOP delivers everything by pull and never had a call site for it. `WorkspaceRuntime` (manager placement, S8-pinned shapes) is a SEPARATE interface from `Runtime`, consumed through the optional `Controller.Workspaces` field, for the same reason `WorkflowRepositories` is separate from `UnitOfWork` |
| [artifactstore.go](artifactstore.go) | `ArtifactStore` | Durable local file writes/reads under the run's artifact directories (assignment, pane scrollback, check stdout/stderr), temp-file-then-rename, never called from inside a `StateStore` transaction |
| [system.go](system.go) | `Clock`, `IDGenerator` | Explicit time and identity generation |
| [process.go](process.go) | `CommandRunner`, `Command`, `CommandResult`, `ProcessGroupInspector`, `GroupProcess` | Process-group-leader execution with cancellation (git operations, spawning `hop check-exec`) and local process-table listing/signaling for group retirement. `Command.MaxOutputBytes` sets a per-stream capture bound for one invocation (0 keeps the runner's 1 MiB default; negative is refused). `CommandResult` carries each stream's bounded capture and whether the runner discarded bytes of it (`StdoutTruncated`, `StderrTruncated`); a caller deciding on a whole stream treats a truncated one as unobserved |
| [configuration.go](configuration.go) | `ConfigurationSource`, `RunPolicy` | Loads and validates one repository's `.herdr-orchestrator/config.toml`-decoded policy: check contract, env strip/passthrough, profile dir, harness, and (Phase 3, zero value for solo) the `[workflow]`/`[workers]`/`[retry]`/`[roles]`/`[messages]` keys; the config adapter owns defaults and feature-mode-required validation through workflowpolicy.go's shared helpers |
| [workflowpolicy.go](workflowpolicy.go) | `WorkflowModeSolo`/`WorkflowModeFeature`, `DefaultMaxWorkers`/`DefaultRetryLimit`/`DefaultMessageAttention`/`DefaultMessageWait`, `ApplyWorkflowDefaults`, `ValidateFeaturePolicy`, `ErrFeaturePolicyInvalid` | The ONE implementation of the feature-mode policy defaults and requireds, called by the config adapter for a `mode = "feature"` file AND by hop run's `--workflow feature` override path (slice 6), so the two can never disagree; errors name keys, never values |
| [grammar.go](grammar.go) | `GrammarVerb*`, `Grammar*Line` constants and renderers, `GrammarReason*`, `GrammarRefusalLine`, `GrammarPrincipal`, `GrammarTime` | The section 7 worker-protocol grammar's one source of truth: verb spellings, every worker-facing first line, the fixed follow-on lines, the rendering conventions and the enumerated refusal reason tokens behind `refused: <token>`. Templates, cmd/hop and the fixtures QUOTE these; `TestGoldenGrammar` pins every constant and rendered line against retyped golden literals, and slice 6's real-binary contract tests assert the same constants per verb. STATUS-1 extends the same file with `hop status -run`'s own small grammar (section 10): `GrammarAttentionLine`/`GrammarAttentionActionSession`/`GrammarAttentionActionHuman`/`GrammarAttentionMarker` (section 7's mailbox condition, verbatim), `GrammarTaskLabel`/`GrammarTaskLine`/`GrammarTaskAddress`, `GrammarIntegrationLine`, `GrammarShortfallLine`/`GrammarShortfallVerdictRejected` (kept as its own constant rather than an `internal/domain/run` import, parity pinned by `TestGoldenGrammar` against `run.ShortfallVerdictRejected`), `GrammarQuestionLine`/`GrammarAnswerInvocationLine` and `GrammarSessionLine` — cmd/hop resolves every task/session uuid to its `t<seq>` label before calling these, so the constants themselves take already-resolved strings, exactly like `GrammarMessageLine` above takes an already-resolved `from` |
| [templates.go](templates.go) | `renderManagerAssignment`, `renderWorkerProtocolCrib`, `renderTaskAssignment`, `renderReviewAssignment`, `priorAttemptFeedback`, `FreezeWorkflowArtifacts`, `WorkflowFreezeRequest`, `IntegrationBranchName`, path derivations (`roleArtifactPath`, `reviewReasonsPath`, `checkEvidencePaths`) | The section 6 assignment/role/review artifacts: pure, deterministic, frozen-run-facts-only templates quoting every protocol line from grammar.go (`TestTemplatesQuoteGrammar`); `FreezeWorkflowArtifacts` is slice 6's feature-freeze hook — role files read through `ArtifactStore`, copied into the run's artifact directory with digests, the crib written beside them, the integration branch name derived from the run sequence (never a config key). STATUS-1 adds `managerAssignmentFields.RepositoryRoot` (frozen at both call sites — `StartFeatureRun`'s `req.RepositoryRoot` and `ensureManagerArtifacts`'s `frozen.RepositoryRoot`, so a recreated assignment still reproduces its frozen digest) and `renderManagerAssignment`'s verdict-channel paragraph: run `<hop> status -C <repository root> -run <run uuid>` after any controller info notice and plan a fix task on a `verdict-rejected` shortfall, since the review-verdict notice's own body is only the reasons artifact (section 7's "the manager's verdict channel") |
| [usecase_sessionlaunch.go](usecase_sessionlaunch.go) | `PrepareSessionLaunchExec`, `SessionLaunchExecRequest`, `validateSessionLaunchEnvironment`, `composeSessionArgvTail`, role prompt renderers (`renderManagerInitialPrompt`/`renderManagerContinuationPrompt`/`renderReviewerInitialPrompt`/`renderReviewerContinuationPrompt`), `attemptAssignmentPath`, `workerProtocolCribPath` | hop launch's Phase 3 exec boundary: `--run --session` for every feature pane plus the permanent solo shim (`--run --attempt` resolved app-side — the run's current attempt per `LoadRunStatus`, its current session, then the one session-addressed flow), context through `WorkflowReadStore` failing closed with the typed sentinel, per-role fail-closed env validation (the shim's Phase 2 pane set never provides `HOP_SESSION_ID`/`HOP_ROLE`, so there they are validated whenever PRESENT — presence is the entry existing at all, and a present-but-empty variable is a disagreement, fail closed; a manager pane refuses the task/attempt pair outright, explicitly empty entries included), currency by SESSION state (launching) and first-vs-resume by the context's successor fact — never attempt state — the Phase 2 prompt shapes kept byte-identical for worker and implementer, role-specific shapes for manager and reviewer (`TestGoldenRolePrompts` pins all six), the manager's directory cross-check against the recorded repository root, and the session-keyed claim |
| [digest.go](digest.go) | `ResultDigestTag`, `ComputeResultDigest`, `RequestDigestTag`, `ComputeRequestDigest` | The canonical `"hop-result-v1"` result digest and the canonical `"hop-request-v1"` messaging/plan request digest: length-prefixed fields, SHA-256 hex, computed only here — the domain receives each as an opaque validated string |
| [decision.go](decision.go) | `LaunchClaimDeadline`, `LaunchDeadlineExpired`, `LaunchSettlement`, `SettlementEvidence`, `CorroborateSettlement`, `ClaimProcessMatches`, `RestoredHarnessOutcome`, `MatchRestoredHarness`, `restoreInvocationFor`, `MatchRetirementTarget`, `processMarkerMatch`, `OccupantMatches`, `ServerContinuityEstablished`, `GroupRetirementOutcome`, `ClassifyGroupRetirement`, `ArgvUnavailable` | The section 6 claim-corroboration predicate over EVERY foreground-group member (claim-derived executable identity, durable marker set, explicit per-member `hop launch` exclusion, fail-closed on missing identity, wrapper precedence; returns the corroborated member and marker as `SettlementEvidence`), the claimed-process-present check resume uses to keep the wrapper classification, the restored-harness predicate (Herdr's native restore invocation per harness, exact argv elements on one member, exactly one match) that alone authorizes positive-evidence retirement and its close-time recheck, the per-member close-rule occupant match, the server-continuity predicate, and the four-outcome process-group-retirement classifier matching both the frozen check argv and its check-exec invocation |
| [operation_payload.go](operation_payload.go) | `decodeOperationPayload` | Reads a persisted operation intent/evidence/outcome payload without relying on Go type identity: a value of the target type passes through, anything else round-trips through JSON. Callers still validate required fields and fail closed on a failed decode |
| [controller.go](controller.go) | `Controller`, `RunHandle`, `Heartbeat`, `Detach`, `ErrStopRequested` | The driving service composition calls; ports as fields, plus `GitExecutable` (the absolute path of the git binary every repository/worktree command runs, resolved once by composition). `RunHandle` is an opaque per-run token (run identity, the held lease and a dispatch scope) so composition never touches identity types. `Heartbeat` extends the lease on the design's interval and cancels in-flight external calls on failure; `Detach` journals, cancels and releases without stopping; `revalidateForDispatch` is the section 4 step 2 revalidation every external mutation runs first. `Messages`/`Plan`/`Reviews`/`Workspaces`/`Presentation` are optional Phase 3 fields, nil for a solo-only Controller (`Presentation` is the Phase 1 `AgentPresentation` port, consumed by section 9's run-state token publication) |
| [assignment.go](assignment.go) | `renderAssignment` | Deterministic assignment-artifact content: brief, identities and absolute paths only, referenced by the launch argv, never typed into a dialog |
| [usecase_run.go](usecase_run.go) | `StartRun`, `StartRunRequest`, `StartRunResult`, `ErrStartRefused`, `classifyWorktreeProvenance` | Refuses unsupported repositories (SHA-256 object format, launch-line-hostile HOP paths) and unusable policies before any side effect — every such refusal wraps `ErrStartRefused`, hop run's usage exit — freezes the run snapshot, calls `InitializeRun`, writes/records the assignment artifact, then drives `worktree.create` (base commit resolved to an object id before the intent commits; canonical common-directory EQUALITY plus frozen-base HEAD comparison validate provenance) and `pane.open` as record-intent/act/record-outcome units |
| [usecase_featurestart.go](usecase_featurestart.go) | `StartFeatureRun`, `ResolveRunWorkflow`, `OpIntegrationInit`, `OpWorkspaceCreate`, `ErrIntegrationBranchExists`, `continueFeatureBootstrap`, `recoverIntegrationInit`, `recoverManagerWorkspace`, `settleIntegrationInitForShutdown` | The feature-run bootstrap (slice 6b; design sections 1, 6, 9, 10): pre-side-effect refusals wrapping `ErrStartRefused` (policy with the `--workflow feature` override applied through the shared helpers, role files pre-read, sha1, HEAD → base oid, predicted sequence, advisory `rev-parse --verify` of the predicted integration ref); the freeze (`FreezeWorkflowArtifacts`) and `InitializeRun`'s feature shape, re-frozen under the same run id on `ErrRunSequenceMismatch` (at most three attempts); then the manager assignment and three record-intent/revalidate/act/record-outcome acts — `integration.init` (`git update-ref --no-deref <ref> <base> ""`, G1/G3; a symbolic integration ref, detected by `git symbolic-ref -q`, is a collision on every path), `workspace.create` (label = operation id, recovered by label, never created twice) and the manager's `pane.open` (`<hop> launch --run --session`, cwd = repository root, the five-variable manager env; the intent moves the run to launching). `ResolveRunWorkflow` chooses hop run's start use case. The resume continuation finishes a bootstrap a lost controller left undone, and `settleIntegrationInitForShutdown` resolves an unresolved init before a stopped or failed report. Errors never echo paths or environment values; transport causes stay in the journal; before the freeze it also reads the branch checked out at the repository root (`resolveTargetBranch`: `symbolic-ref -q HEAD` through `classifyTargetBranch`) and freezes it as `WorkflowSnapshot.TargetBranch`, the worktree-retirement target — a detached HEAD freezes none, any other answer refuses the start value-free |
| [usecase_unplacedlaunch.go](usecase_unplacedlaunch.go) | `resolveUnplacedLaunch`, `unplacedLaunch` | Phase 2's unbound-launch rule keyed to one session, used by feature stop and per-attempt/completion/terminal-failure retirement for a session with no committed binding |
| [usecase_launch.go](usecase_launch.go) | `CorroborateLaunch`, `LaunchProgress` | One inspection round toward settling a launch claim under the section 6 predicate: settled, needs-interaction (forking wrapper), failed (`exec_failed`, terminating the session), already-settled (early acceptance; activates a still-launching session) or still pending — recovers a lost binding by creation label, never sleeps or resends |
| [usecase_stop.go](usecase_stop.go) | `RequestStop`, `DriveStop`, `StopReport`, `CheckRunIntent`, `closePaneOperation` | One stop round: retire every check execution's group under the group-retirement rule and the recorded worker under the shared pane.close operation procedure, and mark stopped ONLY once all owned work is observed absent; mismatches and failed inspections stay outstanding, never absent |
| [usecase_sessioncorroborate.go](usecase_sessioncorroborate.go) | `CorroborateSessionLaunches`, `SessionLaunchProgress`, `settleSessionExeced`, `applyManagerRunRunning`, `driveLaunchingRunFailure`, `recoverSessionBindingByLabel`, `driveSessionLaunchDeadline` | The feature-mode, per-session sibling of `CorroborateLaunch`: one inspection round for every launching session under the one corroboration predicate; the manager's settlement moves a launching run to running (never a resuming one — `ResumeFeature`'s gate). An exec_failed claim is terminal, placed or not: a child's settles through `settleChildExecFailure`; a manager's session is terminated and, while the run is launching (the pass runs nothing else then), `driveLaunchingRunFailure` fails the run through `RetireSettledSessions` on this and every later round until it settles |
| [usecase_resume.go](usecase_resume.go) | `Resume`, `ResumeRequest`, `ResumeResult`, `ResumeOutcome` | Acquires a new fencing generation (idempotently for a resuming run; NothingToDo for a terminal one), recovers pending operations per the decision table (worktree adoption by provenance, pane adoption by label, check retirement by claim/group, persisted bounded waits), then reconciles per section 5: corroboration for launching attempts, warm adoption of settled claims under the one predicate with the run's state restored atomically, positive-evidence retirement through the pane.close procedure under a distinct observation incarnation, continuity-gated `--confirm-absent` attestation journaled as `absence.attested`, and lineage/workspace-validated cold relaunch |
| [usecase_submit.go](usecase_submit.go) | `SubmitResult`, `SubmitResultRequest`, `SubmitResultResult` | Section 7 step 1 (parse and bound the inputs) and the canonical digest, computed here; steps 2-5 are `SubmissionStore.SubmitResult`'s contract |
| [usecase_check.go](usecase_check.go) | `ClaimAndRunCheck`, `CheckReport`, `CheckClaimDeadline` | Recovers unresolved executions first (claim/group retirement, confirmed absence, then the unknown-outcome rule; bounded no-claim ambiguity), reopens orphaned claimed requests, then claims the oldest pending request atomically with its execution intent and lifecycle transitions, validates and materializes the detached checkout (submodule candidates fail clearly), spawns `hop check-exec` bounded by the frozen timeout, retains stdout/stderr evidence, and applies the section 7 outcome transaction including stop precedence and request settlement |
| [usecase_execboundary.go](usecase_execboundary.go) | `PrepareLaunchExec`, `LaunchExecRequest`/`LaunchExecPlan`, `FailLaunchExec`, `PrepareCheckExec`, `CheckExecRequest`/`CheckExecPlan`, `CheckSpawnEnvironment`, `ExecutableLookup` | The string-facing exec-boundary use cases behind `hop launch` and `hop check-exec` (composition passes raw strings; typed IDs are parsed here): section 6 launch preparation — fail-closed HOP_* environment validation against the launch context (every pane-provided variable present and agreeing: state root, run, task, attempt, incarnation), sanitization under the frozen policy, per-harness argv composition (first launches for Claude `--session-id <ref>` + fixed prompt, Codex `<prompt>` and opencode `--prompt <prompt>`; cold relaunch is Claude-only `--resume <ref> <continuation prompt>` — the adjacent pair the restore predicate requires, then `renderContinuationPrompt`'s fixed template from the frozen assignment path and hop path, because interactive Claude Code never re-runs a pending turn on `--resume` (native-harness-compat.md, 2.1.270) — and Codex/opencode cold resume reports the Phase 2 unsupported state), executable resolution through the composition-supplied `ExecutableLookup`, the worktree cross-check (`resolveWorkerDirAgainstWorktree`: the launcher's cwd and the recorded worktree path both canonically resolved through the request's `ResolvePath` seam and required equal — a missing recorded path, an unresolvable side or a disagreement refuses fail-closed with resolver errors never echoed, and the resolved directory is the seed key), the workspace-trust pre-seed (`seedWorkspaceTrust` over the sanitized environment and the request's resolved `WorkerDir`, written through the `Trust` port immediately before the claim; not-seeded outcomes are evidence and the launch proceeds, a seeding write failure refuses before any claim), then `ClaimLaunch` recording the seed evidence — and section 7 check-exec preparation (group-leadership check, `ClaimCheckExec` BEFORE anything else, frozen-argv verification, sanitized env), generalized in Phase 3 to BOTH exec-claimable kinds — `check.run` and `integration.merge`, the frozen argv resolved by operation kind through `LoadCheckExecutionContext` and passed through byte-identically either way, since group retirement matches the running argv. The exec itself stays in `cmd/hop` through the process adapter; `FailLaunchExec` is the post-claim failure path and `CheckSpawnEnvironment` composes ClaimAndRunCheck's sanitized spawn env |
| [trustseed.go](trustseed.go) | `TrustSeeder`, `TrustSeedOutcome`, `TrustSeedStep`, `PlanTrustSeed`, `SeedTrustEdit` | Workspace-trust pre-seeding (verified against Claude Code 2.1.270): `PlanTrustSeed` purely resolves the profile trust file from the launched harness, the SANITIZED environment (`CLAUDE_CONFIG_DIR`, else `HOME`) and the resolved worktree path — codex/opencode and every unresolvable case are not-seeded reasons, never errors — and `SeedTrustEdit` computes the byte-surgical `.claude.json` edit setting exactly `projects[<worktree>].hasTrustDialogAccepted = true` (overwrite false→true, compact insertions only, every other byte preserved, last duplicate key wins, unparsable documents are errors the caller maps to not-seeded). `TrustSeeder` is the consumer-owned port for the locked atomic file write |
| [usecase_status.go](usecase_status.go) | `Status`, `StatusRequest`, `StatusResult`, `RunSummaryView`, `RunDetailView`, `TaskSummaryView`, `IntegrationView`, `GuardShortfallView`, `MailboxView`, `PendingQuestionView`, `WorktreeOperationView`, `SessionView` | Renders `ReadStore` into string-only (plus `time.Time`/`time.Duration`, matching `RunSummaryView.UpdatedAt`'s existing precedent) view DTOs for `hop status`, including the last check execution's identity, evidence paths and the human's options for an unknown outcome (`SeedEvidence` among them, from the launch claim). `RunDetailView`'s Phase 3 additions mirror `RunDetail`'s one-for-one (`runDetailView` converts identity/domain values to their string forms); `RunSummaryView.NeedsAttention` is derived by OR-reducing `Mailboxes[].Attention` — populated only by the `-run` detail render, never the bare listing, which has no per-address message data to compute it from. The exact `attention: …` line text (section 7) is deliberately NOT composed here: it stays structured data for `cmd/hop` (slice 6) to render, since slice 4 owns the grammar constant set. `RunDetailView.TargetBranch`/`WorktreesRetiredAt` mirror `RunDetail`'s worktree-retirement fields, and `RunDetailView.Worktrees` (`WorktreeView`: branch, path, state, a released row's reason, an active row's last refused category with its evidence path, or its unfinished removal — incomplete, interrupted or unresolved) is derived by `worktreeViews`. `RunDetailView.WorktreeOperations` lists a feature run's unresolved per-attempt `worktree.create` operations with their fixed-text human action (`worktreeOperationAction`); `RunDetailView.Sessions` (STATUS-1, one-for-one with `RunDetail.Sessions`) carries each `SessionView`'s task as a bare uuid (`TaskID`, `""` for the manager) — the `t<seq>` label lookup, like every other uuid-to-label resolution this view leaves undone, stays cmd/hop's job against the `Tasks` table in the same view |
| [usecase_schedule.go](usecase_schedule.go) | `RecomputeReleases`, `AssignReadyTasks`, `AssignmentDefaults`, `AssignmentOptions`, `AssignedTask`, `AssignmentReport` (`Launches`, `Blocked`), `ReleasedTask`, `ReleaseReport`, `requireFrozenAssignmentRoots`, `createAttemptWorktree`, `openChildPane`, `attemptWorktreeCreateIntent` | The section 6 scheduling pass's dependency-release and assignment steps: `RecomputeReleases` moves a dependent task pending→ready once every prerequisite is integrated; `AssignReadyTasks` claims the lowest-seq ready task into a free worker slot (task ready→active, a fresh or already-reserved-by-retry attempt launched, a delegated implementer/reviewer session, the per-attempt worktree and launch intents), one committed transaction per task, the slot bound counted inside it. The attempt's worktree row is recorded linked to its attempt and verified base commit (`run.NewAttemptWorktree`, the integration head for an implementer, the frozen subject for a reviewer), because hop launch's worktree cross-check finds a feature session's worktree by attempt once the run holds several rows. A Claude child is created with a pre-assigned native reference (`c.IDs.NewID()`, `NativeRefAssigned`), which its `--session-id` first launch needs, the same way StartRun pre-assigns the solo worker's; Codex and opencode children carry none. Retried and review attempts get their sessions here too; this is the only production path that creates a fresh child session (a cold-relaunch successor copies its predecessor's reference). A pending `hop task retry`'s bookkeeping row (reserved immediately by `PlanStore.RequestRetry`, worker-authority) is marked consumed in the SAME transaction that launches its already-reserved attempt. Resolving "the current integration head" for an implement task's worktree base is 2b's integration-operations concern; this file only consumes `AssignmentOptions.IntegrationHeadCommitOID` as given. `AssignmentDefaults` serves every run-fixed field from the frozen run — `MaxWorkers`, the harnesses and the frozen `RepositoryRoot`/`StateRoot` — and `AssignReadyTasks` refuses, before reserving any task, a root other than the frozen one (a known run operates on its frozen repository, never the caller's directory); every option refusal names the field, never the path. Each assignment also writes the attempt's assignment artifact (templates.go's task/review shapes, feedback and diff-scope facts gathered inside the claiming transaction) between the worktree act and the pane.open intent, at the app-derived per-attempt path the launch prompt references. The assignment transaction assigns nothing into a run that carries a terminal-failure cause (`featureFailureCauseLocked`), including one a launch settlement earlier in the same pass created. Every pass first recovers assigned attempts whose launch stopped short of a pane (`recoverAttemptLaunches`, usecase_attemptlaunch.go), so a settled launch failure frees its slot for the same pass; the launch after each assignment commit runs through the shared `launchAttempt` (the section 6 refuse-if-exists check of `refs/heads/hop/r<seq>/t<tseq>a<n>` — symbolic or present is a collision that settles the attempt, naming the ref only; unobservable defers the create — then `createAttemptWorktree`, labeled with its operation ID and recording the act's response as evidence when provenance cannot be established, then the assignment artifact and `openChildPane`). A launch that does not reach an opened pane is an `AssignmentReport.Launches` entry, never a pass failure; the error return is reserved for store and lease failures and the assignment transaction's own refusals, and a held stop refusing a dispatch ends the pass |
| [usecase_attemptlaunch.go](usecase_attemptlaunch.go) | `AttemptLaunchCondition`, `AttemptLaunchOpened`/`Settled`/`Waiting`/`Reconciling`/`StopRequested`, `recoverAttemptLaunches`, `resolveAttemptWorktreeCreate`, `adoptAttemptWorktree`, `unresolvedAttemptLaunches`, `advanceAttemptLaunch`, `launchAttempt`, `continueAttemptLaunch`, `settleAttemptLaunchFailure`/`workerLaunchFailure`, `featureFailureCauseLocked`, `launchSuppression`/`launchSuppressionLocked`, `revalidateChildDispatch`, `dispatchRefusal`, `paneOpenRefusalOutcome`, `refusedBeforeDispatch`, `readAttemptLaunchState`, `resolveAttemptWorktreesForShutdown`, `worktreeOperationAction`, `attemptWorktreeCondition` | Feature-mode recovery of an assigned attempt whose launch stopped between the assignment commit and its pane.open intent (design section 4's per-attempt `worktree.create` row), shared by the scheduling pass, `ResumeFeature`, `DriveFeatureStop` and the terminal-failure settlement. Every unresolved per-attempt `worktree.create` is resolved first: a row linked to the attempt (`WorktreeIndex().ByAttempt`) settles it; else the repository's `git worktree list --porcelain` is searched for the intent's branch — a checkout is adopted only by provenance against the intent's base (`classifyWorktreeProvenance`), the row created with `run.NewAttemptWorktree` and the placement taken from the act's recorded response or recovered by the operation's creation label (`FindWorkspaceByLabel`; S9 and `TestRealProcessHerdrAdapterWorkspaceAndWorktreeLabels` pin the round trip), an unrelated checkout settles it failed, an unverifiable one or an unreadable listing stays reconciling (no bounded wait, the Phase 2 row), and no checkout stays in flight until the bounded wait anchored at the operation's creation passes, then settles failed. An intent naming no attempt of the run is marked reconciling and blocks automatic settlement and any re-drive. In continue mode each launching child attempt with no binding and no launch intent (its attempt's only session) then advances: a row continues to the assignment artifact and a pane with a fresh incarnation, only in a recorded workspace; a failed operation settles the attempt as a terminal launch failure through `settleWorkerTermination` (attempt failed, budgeted task consequence, mailbox closure at the limit, manager notice, session terminated); no operation at all re-drives the create (`launchAttempt`, base = the review task's subject or the current integration head, refusing an existing branch). Resolve-only mode (stop, terminal failure, or a resume while a failure cause stands) never creates, writes or opens anything. Within a continue-mode round, settlements run before any launch step, and a held stop or a durable terminal-failure cause (`featureFailureCauseLocked`: a failed task, or the manager lineage's exec failure read in launching, running AND resuming) is re-read before each target, before each worktree intent, before the assignment artifact and before each pane intent (`launchSuppression`), so a cause the round itself created — or a manager exec failure recorded meanwhile — leaves every later launch, and every later settlement, undone while operations keep resolving. The feature child's final dispatch revalidation (`revalidateChildDispatch`, used by `createAttemptWorktree` and `openChildPane`) reads the lease, the stop flag and that cause together; a refusal records the operation failed as refused before dispatch in the same transaction, so stop and failure cleanup resolve it as never dispatched (`refusedBeforeDispatch`: unplaced-launch resolution reads only unresolved intents, the launch-deadline driver never reopens one, and such an intent does not own its session for recovery). Report details and `worktreeOperationAction`'s status text are fixed, path-free text; raw causes stay in the journal as `attemptWorktreeCondition` |
| [usecase_message.go](usecase_message.go) | `SendMessage`, `FetchMessage`, `AckMessage`, `Answer`, `ShowMessage`, `SendMessageRequest`/`Result`, `FetchMessageRequest`/`Result`, `AckMessageRequest`/`Result`, `AnswerRequest`/`Result`, `ShowMessageRequest`/`Result`, `ShowMessageDelivery` | Section 7's driving messaging use cases behind `hop msg send`/`next`/`ack`/`show` and `hop answer`: resolve the caller's logical address via `WorkflowReadStore.LoadMessagingContext`, bound and write the body durably (file-first, digest computed here) before delegating validation/acceptance to `MessagingStore`, which owns addressing legality, eligibility and the request-ID receipt. `hop msg wait`'s 1s poll loop and timeout live in `cmd/hop`, never here — `FetchMessage` is one non-blocking attempt. `ShowMessage` is the one message verb with no caller identity at all (any session, or the human context, per the grammar table): `WorkflowReadStore.LoadMessageDetail` is its entire lookup — read-only, no lease, never delivers or acks, `ErrNotFound` (translated to `Found: false`) for an unknown id or one from a different run. `SendMessage`/`FetchMessage`/`AckMessage` each compare `LoadMessagingContext`'s returned `RunID` against their own parsed `RunID` and refuse a cross-run mismatch before any side effect (before the body artifact write, for `SendMessage`) — `AckMessage` calls `LoadMessagingContext` for exactly this check even though it needs no address from it |
| [usecase_plan.go](usecase_plan.go) | `CreateTask`, `RequestRetry`, `ClosePlan`, `CreateTaskRequest`/`Result`, `RequestRetryRequest`/`Result`, `ClosePlanRequest`/`Result` | Section 8's driving manager-plan use cases behind `hop task create`/`hop task retry`/`hop plan close`: `CreateTask` bounds and writes the instructions body durably (file-first, digest computed here) before delegating to `PlanStore.CreateTask`, which owns manager/run-state validation, acyclicity, the plan-flag reopen and the request-ID receipt; `RequestRetry` and `ClosePlan` delegate directly, since neither writes a file. `RequestRetry`'s acceptance reserves the new attempt IMMEDIATELY inside `PlanStore` (see the invariant above) — this use case is a thin wrapper, not where that reservation happens |

| [usecase_integration.go](usecase_integration.go) | `DriveIntegration`, `IntegrationReport`, `ResolveIntegrationHead`, `OpIntegrationMerge`/`Publish`/`Reset`/`Fence`, the `integration*Intent`/evidence payloads, `integrationMergeArgv`, `releaseEligibleTasks` | The section 4/8 serial-integration driver (slice 2b): claim the lowest-seq completed implement task into the store-enforced serial slot (pre-merge head from the live ref, never rows); `integration.merge` as one execution per operation — materialize to OBSERVED completion with the HEAD verified and recorded as act evidence before the spawn, the frozen noninteractive argv (Q5(a): repo-local hooks suppressed via `-c core.hooksPath=<empty dir under the operation's artifact dir>`, created before the spawn), classification merged/no-op/conflict/failed; `integration.publish` update-ref CAS with the three fencing layers; a passing combined candidate integrates the task, releases dependents and commits the manager notice in ONE transaction |
| [usecase_integration_check.go](usecase_integration_check.go) | `runIntegrationCheck`, `settleCombinedCheck*`, `failIntegrationCheck` | The combined-candidate check execution: the generalized pipeline's integration subject (`integrationCheckIntent` on an `OpCheckRun` row until migration 002's typed request table), its own detached checkout under the check operation's directory, mandatory evidence retention, and the integration/feature outcome dispatch — a passing check landing after a stop records evidence and never integrates |
| [usecase_integration_recovery.go](usecase_integration_recovery.go) | `recoverIntegrationOperations`, `retireUnresolvedRefIntents`, per-kind `recoverIntegration*`, `actAndSettleFence`, `retireResetIntent` | Every integration decision-table row's recovery, and the ref-fencing rule: retirement DISTINGUISHES intent kinds — a pending publish is fenced with the CURRENT head's tree (expected-old burned) through `integration.fence`'s own journaled operation (F persisted before the CAS, its own recovery row), a pending reset is COMPLETED (persisted R adopted, re-driven, or rebuilt from the validated pre-merge tree against the observed head), never fenced over; a fence whose CAS loses to the zombie it meant to retire resolves through its own recovery row (`adoptZombiePublishAfterFence`: observed head == the retired publish's candidate → fence settles failed, publish adopted, the standard rollback retires the candidate); fence identity is IDEMPOTENT per retired operation — retirement recovers dependencies first (fences before publishes), re-reads each operation at its turn, and resumes an unresolved fence's own journal row and persisted OID before ever allocating a new one; quiescence is required before any terminal report. Deviation, accepted 2026-09-15: the merge row's bounded no-claim wait is enforced against the intent's durable CreatedAt rather than re-persisted as act evidence, which would clobber the materialization-complete record — the window stays durable and enforced across rounds |
| [usecase_featurecheck.go](usecase_featurecheck.go) | `DriveFeatureChecks`, `FeatureCheckReport`, `settleFeatureCheckOutcome`, `recoverFeatureCheckExecutions` | The per-task (result-subject) check pipeline at feature scope: the `result, feature` outcome row — pass completes attempt and task with the RUN staying running; fail applies the budgeted task consequence; the unknown-outcome rule requeues (repeatable) or settles by budget; stop precedence interrupts inside the outcome transaction |
| [usecase_featuresettle.go](usecase_featuresettle.go) | `settleIntegrationTerminal`, `decideTaskConsequence`, `prepareControllerNotice`/`commitControllerNotice`, `pendingTaskObligations` | Terminal settlements: the section 5 task consequence recomputed INSIDE the transaction (stop → interrupted; budget room → needs-rework; exhausted → failed), failure closing the mailbox under the orphaned-obligations SNAPSHOT-EQUALITY contract (prepare the notice file outside, re-read the pending ID set inside, retry on mismatch), and the controller notice committed with the transition it reports |
| [usecase_retirement.go](usecase_retirement.go) | `RetireSettledSessions`, `RetirementReport` (`RunFailed`, and `RunFailing` while a durable failure cause waits on owned work — the caller schedules nothing more), `observeWorkerExit`, `settleChildExecFailure`, `settleWorkerTermination`/`workerTermination`, `managerLaunchFailedLocked`/`managerLineageFailedLocked`, `sessionCloseEvidence`/`sessionLaunchClaimLocked`, `driveFeatureTerminalFailure` | Section 6 per-attempt retirement: an accepted result, an accepted verdict or a terminal attempt outcome retires the session under the shared close-rule procedure (journaled kind `pane.close` with the boundary as the frozen reason — the decision table's `session.close` row is that procedure verbatim), the slot freeing ONLY on observed absence; a LIVE controller's self-exit observation (positive absence under a settled claim) interrupts the attempt and notifies the manager — retry is manager judgment; a child's exec_failed claim is the same kind of settlement with the attempt failed instead (`settleChildExecFailure`: budgeted task consequence, mailbox closure at the limit, manager notice, session terminated, one transaction; interrupted task under a held stop) — a Controller method with no loop assumptions, so resume's reconciliation can settle through it too; the durable terminal-failure cause is a failed task or, failing that, a manager lineage whose most recent session (the live manager, else the one member never marked lost) has an exec_failed claim while the run is launching or running (`managerLaunchFailedLocked`, the retirement pass's reading of `managerLineageFailedLocked`, whose admissible run states the caller names), and either one is recorded as the failing transition's reason; a session's claim resolves through its current binding, else its unresolved launch intent (`sessionLaunchClaimLocked`, the session launch context's own resolution), so an exec failure whose pane.open outcome was never recorded is still seen; a failed task fails the run only after the stop-equivalent shutdown — executions actively retired, ref intents fenced, an unresolved `integration.init` resolved (`settleIntegrationInitForShutdown`), every unresolved per-attempt `worktree.create` adopted or settled failed (`resolveAttemptWorktreesForShutdown`), the current integration settled through the shared shutdown procedure (a published candidate rolled back) — with quiescence re-verified inside the failing transaction; the current manager resolves through the `ManagerSession` port, never by role scan (a historical relaunched-away row must not shadow the live one) |
| [usecase_review.go](usecase_review.go) | `SubmitReviewVerdict`, `SubmitReviewRequest`/`Result`, `ReviewReasonsLimit` | The driving use case behind `hop review submit`: parse and bound (verdict token, reasons ≤ 64 KiB), resolve the subject TREE object ID against the recorded repository before the accepting transaction, write the reasons artifact file-first, delegate to `ReviewStore.SubmitReview` |
| [usecase_completion.go](usecase_completion.go) | `EnsureReviewTask`, `EvaluateRunReadiness`, `DriveCompletion`, `CompletionReport`, `resolveGuardHead` | Section 8's guard wiring: the head OBSERVED from the live integration ref (outside any transaction) and accepted only when an integrated row vouches for it as its merge candidate (repeated no-op rows each vouch for the unchanged head; an unvouched head — a rolled-back or unsettled candidate — is no head, failing closed), re-verified inside each guard transaction; the run leaves running ONLY in the transaction that first establishes readiness; completion retirement closes the manager and stragglers with scrollback first; the final transaction RE-VALIDATES readiness (failure returns the run to running) — `Run.Complete` is reachable only through this guard in feature mode |
| [usecase_featurestop.go](usecase_featurestop.go) | `DriveFeatureStop`, `settleIntegrationForStop`, `finishFeatureStop` | Stop across roles: check and merge groups retired by claim, ref intents retired under the fencing rule BEFORE the integration settles (a landed zombie publish is adopted then rolled back — the ref never rests on an unvalidated candidate in a stopped run), a merging integration interrupts, a published-but-unsettled or check-failed one completes its reset, every owned session closes under the close rule (a session with no committed binding through `resolveUnplacedLaunch`), an unresolved `integration.init` is resolved first (`settleIntegrationInitForShutdown`) and blocks the terminal transaction, so does an unresolved per-attempt `worktree.create` (`resolveAttemptWorktreesForShutdown`: adopted and recorded, or settled failed after its bounded wait, never launched), and stopped is reported only on observed absence of everything owned; a fence or reset that journals reconciling without settling is OUTSTANDING work, and the stopped/failed transaction re-derives quiescence from its own repositories (`assertRunQuiescedLocked`, which counts an unresolved `worktree.create` too) before committing |
| [usecase_featureresume.go](usecase_featureresume.go) | `ResumeFeature`, `ResumeFeatureRequest`/`Result`, `FeatureSessionReport`, session dispositions, `coldRelaunchFeatureSession` | Feature-mode resume: takeover by lease CAS, a held stop routed to stop handling before any adoption, integration operations recovered first, then the feature startup continuation (`continueFeatureBootstrap`: while the manager is reserved, `integration.init` re-driven or recovered and the manager placed and launched; a launching manager recovered by label with the run returned to launching, its pending report not holding the run reconciling), then the per-attempt launch recovery (`resumeAttemptLaunches`: continue mode, resolve-only while a terminal-failure cause stands; a child whose pane it opened reports pending without holding the run reconciling, and an unresolved one's report names its condition), EVERY session reconciled (while a terminal-failure cause stands, an unplaced launch whose worktree work is resolved reports `SessionLaunchSuppressed` and does not hold the run reconciling: the run returns to running and the controller loop's retirement pass — which reads the cause there — retires it and fails the run) under the one corroboration predicate; cold relaunch is authorized per session only through `--confirm-absent <session-id>` — the attestation journaled (`absence.attested`) with both assertions and the recorded/observed server-instance evidence, relaunch requiring positive absence AND `ServerContinuityEstablished`, Claude-only; a manager relaunch creates the SUCCESSOR manager session bound to the same native reference with the predecessor marked terminal in the same transaction, children keeping their historical `parent_session_id`. An exec_failed claim is retired with no process: a child's settles through `settleChildExecFailure` exactly as launch corroboration settles it (a controller that died before corroborating never strands the attempt); the manager's session is only terminated, and the manager-lineage failure cause fails the run on the next retirement pass |
| [worktreeretirement.go](worktreeretirement.go) | `OpRetirementCheck`, `OpWorktreeRetire`, `OperationKind.ExecClaimable`, `worktreeRetirementExecIntent`, `worktreeRetirementCheckArgv`, `worktreeRetireArgv`, `WorktreeRetirementRepositories`, `RetirementWorktree`, `RequireWorktreeRetirementRepositories`, `ErrWorktreeRetirementUnsupported` | The post-merge worktree retirement operation kinds ([phase-3-worktree-retirement.md](../../docs/plan/phase-3-worktree-retirement.md) section 7) and their frozen exec shapes: `ExecClaimable` is the ONE exec-claimable kind set (`check.run`, `integration.merge`, `retirement.check`, `worktree.retire`) the SQLite store and the fakes both decide by; the intent's `argv`/`cwd` members are the store's documented contract; the detection argv compares two object ids (`merge-base --is-ancestor`) and the removal argv is `git -C <root> -c status.showUntrackedFiles=all -c core.fsmonitor=false worktree remove <path>`, never a force option. No use case drives them yet; "retirement" here is worktree retirement, distinct from usecase_retirement.go's per-attempt session retirement `WorktreeRetirementRepositories` is the retirement pass's controller-transaction surface, a separate interface under the additive packaging rule reached by `RequireWorktreeRetirementRepositories` (failing closed with `ErrWorktreeRetirementUnsupported`); `WorktreesForRetirement` lists every worktree row of the leased run, oldest first and in any state, as `RetirementWorktree` (the domain value plus the revision `Worktrees().Save` expects); `WorktreesRetiredAt` reads the fact inside the unit of work (this transaction's own mark included); `MarkWorktreesRetired` sets the leased run's set-once fact (another run is `ErrFenced` for all three) |
| [worktreeretirement_git.go](worktreeretirement_git.go) | `listedWorktree`, `parseWorktreeListZ`, `hasHiddenIndexFlags`, `ancestryResult`, `classifyAncestry`, `classifyTargetBranch`, `errTargetUnreadable` | Pure readers of the git observations worktree retirement decides on, each shape pinned by the process-adapter probe: `worktree list --porcelain -z` records (unknown attributes skipped, a malformed record an error), `ls-files -v -z` tags (lowercase = assume-unchanged, `S` = skip-worktree, both hide changes from status; every entry NUL-terminated, so a cut final entry is an error — a necessary check, never a completeness proof), the `merge-base --is-ancestor` exit statuses (0 contained, 1 not, anything else failed) and the freeze-time `symbolic-ref -q HEAD` target (a `refs/heads/` ref, or no target for a detached HEAD) |
| [worktreeretirement_inspect.go](worktreeretirement_inspect.go) | `WorktreeRetainedCategory` and its `Retained*` constants, `WorktreeReleaseReason` and its `Released*` constants, `PathInspector`, `retirementGitEnv`, `retirementGit`, `attemptCheckout`, `checkoutVerdict`, `inspectAttemptCheckout`, `listedCheckout`, `observeCheckout`, `retirementCommonDir` | The pre-act inspection of one candidate attempt worktree (design note section 4), read-only and never journaled, stopping at the first observation that decides. The recorded root and path are resolved through the composition-supplied `PathInspector` (Herdr records a non-canonical spelling, git lists the canonical one; the inspector resolves a spelling as the filesystem does and errs, never answers absent, on one it cannot resolve), and the repository root itself is released. The checkout must be listed by the root's own `worktree list -z`: a missing unlisted checkout is absent, and a present unlisted one is released. A detached checkout or one on another branch is released; a locked one is retained. A listed-but-gone checkout is removable (outcome absent). The checkout's common directory must equal the root's, and its HEAD must descend from the recorded base; otherwise it is released. `status --porcelain=v1 --untracked-files=all --ignore-submodules=none` output retains it as uncommitted changes (the explicit submodule flag defeats a repository-local `diff.ignoreSubmodules`), `ls-files -v -z` flags retain it as hidden changes; both index reads carry `-c core.fsmonitor=false` (`retirementIndexRead`) so a repository-local fsmonitor hook never runs, and any unreadable observation retains it as inspection-failed. Every read is the absolute git under `retirementGitEnv`, exactly the probe's environment. The index scan and the worktree listing run with the 64 MiB bound `retirementListingOutputBytes` (`retirementGitBounded`); every other read keeps the default. `retirementGit` refuses any read whose stdout or stderr the runner truncated (`errRetirementOutputTruncated`), whatever its bound: a cut index scan or worktree listing can end on a record boundary and look complete, so every caller treats the read as unobservable — pre-check inspection-failed, post-act reconciling, recovery blocked, detection failed or history-missing — and nothing is removed or settled on it. `listedCheckout` is the shared listing lookup: the record whose path resolves to the candidate's canonical path wins. Git's registered spelling may differ from the current canonical path after an ancestor moved, so when no record matches and any record failed to resolve, the listing is unobservable and neither absent nor released can be concluded. `observeCheckout` is the removal act's post-act observation (listed, exists) |
| [worktreeretirement_verify.go](worktreeretirement_verify.go) | `verifyRetirementCandidate`, `recordedWorktreeCreateOutcome`, `attemptBranchRefPrefix` | Candidate provenance (design note section 4, "Candidate set"): a worktree row becomes a candidate only when exactly one succeeded `worktree.create` operation of its run carries its attempt's `attemptWorktreeCreateIntent`, and that intent plus the recorded `worktreeCreateOutcome` (the act evidence, else a recovery's outcome) agree with the row's attempt, branch, base commit and path, with the intent's repository root equal to the frozen root. The candidate's branch is the full ref (`refs/heads/` + the recorded short name, which Herdr reports as requested). Anything else is unverified — the caller releases the row — with a detail that never echoes a path |
| [usecase_worktreeretirement.go](usecase_worktreeretirement.go) | `AcquireForRetirement`, `ReleaseRetirement`, `ErrRetirementNotEligible`, `retirementEligible`, `isTerminalRunState`, `revalidateRetirementDispatch` | The worktree-retirement pass's lease discipline (design note section 3). `AcquireForRetirement` refuses an ineligible run before touching the lease: a solo run, a run frozen without a target branch, a non-terminal run, or an already-retired one. It passes `ErrLeaseHeld` through (the pass defers that run) and re-checks terminality under the acquired lease, releasing again on refusal. `ReleaseRetirement` cancels the handle and releases the lease without journaling anything, so repeated passes do not grow the store. `revalidateRetirementDispatch` is the dispatch revalidation of both claimed retirement acts: a fresh heartbeat CAS, then a fenced re-read requiring the run's state to be exactly completed, failed or stopped and its fact unset (stopping, resuming and completing refuse). It deliberately ignores the run's stop request, since a terminal run can never start work again and every stopped run carries one |
| [usecase_worktreeretirement_detect.go](usecase_worktreeretirement_detect.go) | `detectForRetirement`, `retirementHead`, `detectRetirementMerge`, `latestIntegratedHead`, `settledMerged`, `settledAnswer`, `targetTip`, `repositoryHoldsHead`, `runRetirementCheck`, `recoverRetirementCheck`, `integratedHead`, `retirementCheckIntent`/`retirementCheckOutcome`, `retirementDetection` and its states, `retirementPassOptions`, `retirementSpawnEnv`, `settleRetirementOperation`, `retirementActTimeout` | The pass's detection step (design note sections 2 and 7), run under a pass's lease. Before anything else, `retirementHead` reads the validated head and runs the repository-identity check; a run with nothing new integrated, ambiguous heads, or a root without the head ends the pass untouched: no recovery and nothing journaled.<br>**Recovery.** Unresolved retirement operations of both kinds from earlier passes are recovered first (`recoverRetirementOperations`); for `retirement.check`:<ul><li>no claim is decisive, because the pass holds a newer generation that `ClaimCheckExec` refuses, so the check settles never-executed;</li><li>a claimed group is retired by the group rule and then settles unknown;</li><li>a group still running or mismatched blocks the pass.</li></ul>**Head.** The validated head `integratedHead` is the latest-created integrated row's merge commit. A run whose integrated rows are all no-ops never retires, and two latest rows that disagree fail closed.<br>**Checks, in order.**<ul><li>A settled merged check ends detection for good.</li><li>`cat-file -e <head>^{commit}` must succeed; otherwise the history is missing and the pass skips.</li><li>The target tip is read with `rev-parse --verify -q`; a missing branch means not merged.</li><li>A settled not-merged or failed check for the same head and tip is not repeated.</li><li>Otherwise one claimed check runs: `hop check-exec` over `worktreeRetirementCheckArgv`, spawned in the root with the sanitized environment plus `HOP_STATE_DIR` and the retirement git variables, bounded by `retirementActTimeout`.</li></ul>**Outcomes.** An unobserved exit is reconciling, and a lease lost before dispatch leaves the intent pending. Removal lands with the attempt-link fix |
| [usecase_worktreeretirement_remove.go](usecase_worktreeretirement_remove.go) | `removeRunWorktrees`, `retireWorktreeRow`, `removeAttemptCheckout`, `settleRetireDecision`, `settleRetireAct`, `retireRowLocked`, `retirementRows`, `latestRetireInterrupted`, `latestRetireOutcome`, `retireNamesWorktree`, `worktreeViews`, `retainRetireOutput`, `retirementOpDir`, `worktreeRetireIntent`/`worktreeRetireOutcome` and their decision/result constants, `retirementRowReport`/`retirementRowOutcome`, `errRetirementPassHalted` | The removal step (design note sections 4 and 7; decisions in section 13), over every active row of a merged run, oldest first.<br>**Per row.** Provenance comes first (`verifyRetirementCandidate`), then the pre-act inspection:<ul><li>**Unverified, released or absent:** journaled once as a `worktree.retire` operation created settled (succeeded, result `released` with its reason or `absent`, no exec shape, never claimed), with the row moved to that final state in the same unit of work.</li><li>**Retained:** journals nothing and is reported. `uncommitted-changes` becomes `interrupted-removal` when the row's latest settled operation (newest generation, then newest creation) recorded `interrupted`.</li><li>**Removable:** one claimed act, described next.</li></ul>**The act.** The intent carries the frozen no-force argv over git's listed path plus its `hop check-exec` invocation. `revalidateRetirementDispatch` runs next, then the spawn in the root under `retirementSpawnEnv`, bounded by `retirementActTimeout`, then the observation (`observeCheckout`):<ul><li>unlisted and absent: succeeded, `removed` (or `absent` for a checkout already gone), row final;</li><li>listed and present, whatever the exit: failed `refused`, with the category re-inspected (uncommitted, hidden or locked, else `remove-refused`);</li><li>listed and absent: failed `incomplete`, row active, and the next pass repeats the same removal;</li><li>unlisted and present: failed `released`, row released, left on disk;</li><li>a lost exit status or an unobservable checkout: reconciling.</li></ul>A failed act retains its stdout and stderr under `runs/<run>/retirement/<op>/` through `ArtifactStore`, with the paths recorded in the outcome (no artifact rows). A dispatch refusal leaves the intent pending and halts the pass (`errRetirementPassHalted`) |
| [usecase_worktreeretirement_recover.go](usecase_worktreeretirement_recover.go) | `recoverRetirementOperations`, `unresolvedOperations`, `recoverWorktreeRetire` | The pass's first step (design note section 7): every unresolved `retirement.check` (`recoverRetirementCheck`) and `worktree.retire` of the run is recovered, oldest first within each kind, and the first blocking detail is returned after all were attempted. A pass always holds a newer generation than an earlier pass's operation, so a missing claim is decisive. For `worktree.retire`:<ul><li>**No claim:** settles failed `never-executed` and leaves the row unchanged.</li><li>**A claim:** the claimed group is retired first (the frozen argv and its `hop check-exec` invocation). Once the group is gone, the checkout is observed again: unlisted and absent adopts `removed` (or `absent`), a listed checkout settles failed `interrupted` with the row active, and unlisted but present releases the row, left on disk.</li><li>**Blocking:** a running group (signaled), a mismatched or uninspectable group, an unobservable checkout, an operation of the pass's own generation, and an undecodable intent or one naming another root each block, and the removal is never re-dispatched.</li></ul> |
| [usecase_worktreeretirement_pass.go](usecase_worktreeretirement_pass.go) | `RetireWorktrees`, `RetireWorktreesOptions`, `WorktreeRetirementReport`, `WorktreeRetirementLine`, `WorktreeRetirementDisposition` (`Retirement*`), `WorktreeRetirementOutcome` (`WorktreeOutcome*`), `markWorktreesRetiredWhenFinal` | The pass driver cmd/hop calls between `AcquireForRetirement` and `ReleaseRetirement` while it heartbeats the handle (design note section 3). It loads the frozen run (a feature run with a target, else `ErrRetirementNotEligible`) and sanitizes the caller's environment (`CheckSpawnEnvironment`), then runs detection. Only a merged run proceeds to the removal step. Every other detection answer maps to a disposition: `not-merged`, `nothing-integrated`, `history-missing`, `blocked`, `check-failed` or `interrupted`. After removal, one unit of work counts the run's removed, absent and released rows and, when no row is still active, sets the worktrees-retired fact at the clock's time (`retired`, else `in-progress`); a row that was never dispatched makes the pass `interrupted` with the fact untouched. The report carries the run's id and sequence, the counts, and one line per row this pass examined (branch, path, outcome, retained category or released reason, left-on-disk, evidence path, exit code). `BeforeFirstRemoval` is called once, just before the first removal spawn, so status can announce a slow act |
| [usecase_worktreeretirement_triage.go](usecase_worktreeretirement_triage.go) | `RetirementReadStore`, `RequireRetirementReadStore`, `ErrRetirementReadStoreUnsupported`, `RetirementCandidateRecord`, `RetirementCandidates`, `RetirementCandidate`, `triageRetirement` | Lease-free triage (design note section 3). `RetirementReadStore.ListRetirementCandidates` is a separate interface under the additive packaging rule. It returns the repository's terminal feature runs with a target, the fact unset and content integrated, each with its integrated rows, its `retirement.check` operations and whether any retirement operation is unresolved. `RetirementCandidates(root, excludeRunID)` decides each run with detection's own shared rules (`latestIntegratedHead`, `repositoryHoldsHead`, `settledMerged`, `targetTip`, `settledAnswer`), in detection's order:<ul><li>an ambiguous head is `check-failed`;</li><li>no content is `nothing-integrated`;</li><li>a root without the head is `history-missing`, never leased;</li><li>an unresolved operation or a settled merge needs a pass;</li><li>a gone target is `not-merged`;</li><li>an unreadable target is `check-failed`;</li><li>a settled answer for this head and tip is `not-merged` or `check-failed`;</li><li>otherwise the pair is new and needs a pass.</li></ul>Triage never takes a lease, writes nothing and spawns only unclaimed, read-only git |
| [usecase_presentation.go](usecase_presentation.go) | `PublishRunPresentation`, `PresentationReport` | Section 9 run-state token publication through `Controller.Presentation`: manager-first ordering, the task/parent/state tokens per role, full-replacement semantics (unset optionals cleared), superseded bindings never published to; rehydration on resume is a fresh full publication |

## Invariants

- Phase 3's additive packaging rule for an EXISTING port: new capability
  never arrives as a new method on `UnitOfWork`, `ReadStore` or `Runtime`
  directly, since each already has a concrete out-of-package implementer
  (the SQLite adapter, the Herdr adapter) that a widened interface would
  stop compiling before that adapter is updated. Instead, the new surface
  is a SEPARATE interface (`WorkflowRepositories`, `WorkflowReadStore`,
  `WorkspaceRuntime`) a caller reaches either by type-asserting the value
  an existing method already returned (`RequireWorkflowRepositories`,
  `RequireWorkflowReadStore` against the `UnitOfWork`/`ReadStore` a
  `Begin`/method call returned) or, where the port is a plain `Controller`
  struct field rather than a per-call value (`Runtime`), through a new
  optional field (`Workspaces`) instead. Every such assertion or nil
  field is checked and failed closed (the typed `ErrWorkflow*Unsupported`
  / `ErrFeatureModeUnsupported` errors) before any side effect; solo-mode
  code never performs the check. The SQLite adapter (slice 3) adds
  `var _ app.WorkflowRepositories = (*unitOfWork)(nil)` (and the
  `WorkflowReadStore` equivalent on its `ReadStore` implementer) once it
  implements the new getters, at which point the assertions also succeed
  in production; a later slice (6) may fold the getters into
  `UnitOfWork`/`ReadStore` proper as one of its non-additive flips, once
  every implementer is updated together.
- A `hop task retry` reserves its new attempt IMMEDIATELY inside
  `PlanStore.RequestRetry`'s own worker-authority transaction (terminal-
  prior-attempt and retry-limit validation, `Task.Reopen` +
  `ReopenMailbox`), matching the CLI grammar's `retry accepted t<seq>
  attempt <n>` naming the attempt number in that same response
  (`RetryAccepted.TaskSeq`/`AttemptNumber`, also on a receipt replay's
  duplicate) — never
  deferred to a later controller step. The `retry_requests` bookkeeping
  row it also writes (state pending) is consumed by
  `AssignReadyTasks` in the SAME transaction that launches the
  already-reserved attempt; nothing else ever creates a retry's attempt.
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
  the committed intent pending for recovery — except a feature child's
  worktree.create or pane.open, whose `revalidateChildDispatch` also reads
  the terminal-failure cause and records a refusal as failed, never
  dispatched, and unstarted work is never
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
- Foreground-group decisions never depend on member POSITION. Herdr
  reports `pane.process_info` foreground members in raw platform listing
  order — macOS: unsorted `proc_listpids` (repos/herdr/src/platform/
  macos.rs, `foreground_job`); Linux: ascending pid (linux.rs,
  `foreground_process_group_members_with`) — and a real harness (Claude
  Code 2.1.270) spawns its configured MCP servers into its own process
  group right after the trust check, so the launched process holds no
  particular index (the live probe observed an MCP server at index 0; see
  docs/architecture/native-harness-compat.md). Per consumer, the predicate
  is one of exactly three shapes, each applied to ONE member at a time —
  conjuncts are never combined across members:
  - claimed-process-among-the-members (identity conjuncts on the SAME
    member): `CorroborateSettlement` scans every member, skipping
    `hop launch` invocations per member; ANY member matching executable +
    marker under a pid different from the claim's classifies
    forking-wrapper (wrapper precedence — a live wrapper that already
    exec'd matches all three conjuncts under the claim's pid, so a
    settled-first reading would adopt the refused topology), else the
    claim-pid member matching executable + marker settles, else
    unresolved; the corroborated member's own pid and marker are recorded
    (`SettlementEvidence`) as claim settlement and binding occupant
    evidence (`settleExeced`, the resume reconciliation settle and warm
    adoption). `OccupantMatches` and a stop target's `closeTargetMismatch`
    (usecase_stop.go) require the recorded pid and a marker on that same
    member. Launch markers are matched as an exact argv element or a
    cmdline substring (`processMarkerMatch`), because the run, attempt and
    incarnation markers sit inside the larger prompt argument. Resume keeps
    the wrapper classification on both reconciliation paths and never
    falls through to retirement: an exec_pending claim on a reconciling
    attempt with any forking-wrapper classification, and a settled claim
    whose group holds the matching claimed process (`ClaimProcessMatches`)
    alongside a matching different-pid process, each report
    `ResumeFailedClosed` with nothing retired. A settled claim's
    different-pid match with NO member under the claim's pid still matching
    (an observation about the listed members, not proof that pid is absent)
    is the restored occupant case, decided only by the next shape.
  - restored harness (positive evidence for retiring an occupant that is
    not the claim's corroborated process): `MatchRestoredHarness` requires,
    on the SAME member, the session harness's native restore executable
    and the exact restore argument shape for the durable native reference
    — for Claude Code (the only harness `restoreInvocationFor` knows, matching
    Herdr's `agent_resume::plan` and the executed S3 probe
    `TestSpikeRestoreAutoRelaunchBypassesLauncher`) the basename of argv[0]
    `claude` and an argv element `--resume` immediately followed by an argv
    element EXACTLY equal to the reference. Whenever Herdr reports argv,
    elements are matched exactly and cmdline is never consulted, so a
    child whose argument merely embeds the reference is not evidence; only
    when argv is absent does the documented fallback apply (identity from
    argv0/name, `--resume <ref>` space-delimited in cmdline). Exactly one
    matching member authorizes `retireAndRelaunch`, whose close target
    records that member's pid; two or more are ambiguous and fail closed
    with the candidate pids in the report; none, or a harness with no
    restore invocation, fails closed. The close procedure rechecks a
    persisted positive-evidence retirement target — in the authorizing
    round and whenever a later round replays the pending close — by
    classifying the WHOLE current group again (`MatchRetirementTarget`):
    it closes only when `MatchRestoredHarness` finds exactly one candidate
    for the recorded native reference AND that candidate has the recorded
    pid, never pid plus a cmdline substring. A second candidate beside the
    recorded member is ambiguous and a unique candidate under another pid
    is never adopted as a new target; either way no close is dispatched,
    the operation stays reconciling with its recorded intent and act
    evidence, and the report names the pane and, for ambiguity, every
    candidate pid.
    `observePaneAbsence` treats any non-empty foreground as an occupant.
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
- Every git invocation (`gitOutput`, `runGit` in usecase_run.go, backing
  StartRun's object-format check and base-commit resolution, worktree
  provenance classification and check materialization) runs argv[0] as
  `Controller.GitExecutable`, never a bare `"git"`: `CommandRunner.Run`
  requires an absolute executable path, and both helpers refuse before any
  side effect when it is empty or not absolute — `runGit`'s refusal reaches
  StartRun's callers as an ordinary error, wrapped in `ErrStartRefused` when
  it happens before any side effect, exactly like every other pre-side-effect
  refusal.
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
  outcome's. A feature attempt's create carries its operation ID as the
  creation label, so its adoption records the workspace that label still
  names, or the act's own recorded response; only an unlabeled intent or a
  label that resolves nothing leaves the placement unrecorded.
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
  standard library only beyond these, no third-party dependencies). Test
  files additionally import
  [internal/testsupport/storevectors](../testsupport/storevectors/AGENTS.md)
  (the shared refused-input vectors) and
  [internal/testsupport/runnervectors](../testsupport/runnervectors/AGENTS.md)
  (the shared runner capture contract) — production code never does.
- Consumed ports and their adapters: `Probe`, `AgentPresentation`,
  `Observer` and `Runtime` (including `ServerInstance`) are implemented by
  [internal/adapters/herdr](../adapters/herdr/AGENTS.md); `StateStore`,
  `ReadStore` and `SubmissionStore` by
  [internal/adapters/sqlite](../adapters/sqlite/AGENTS.md); `CommandRunner`
  and `ProcessGroupInspector` by
  [internal/adapters/process](../adapters/process/AGENTS.md); `Clock`,
  `IDGenerator`, `ArtifactStore` and `TrustSeeder` by
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
  pre-binding intent fallback, `ClaimCheckExec` requires a pending
  exec-claimable operation (`OperationKind.ExecClaimable`) of the current
  generation, and every Runtime, CommandRunner,
  ProcessGroupInspector and ArtifactStore fake refuses any call made while
  a unit of work is open (`TestFakePortsRefuseCallsInsideTransactions`).
  `fakeCommands.Run` also mirrors the real Runner's own argv contract,
  refusing an empty argv or a non-absolute argv[0], so calling it with a
  bare executable name fails the app suite directly rather than only
  surfacing against a real process at runtime. It follows the Runner's
  capture contract through `runnervectors`: a negative bound is refused
  before anything answers (`ValidateBound`), and every answer — scripted,
  hooked or modeled — is bounded to the command's `MaxOutputBytes` (else
  1 MiB) with the truncation flags computed by `BoundCapture`, which also
  refuses an answer claiming a truncation the Runner could never report.
  `TestFakeCommandsBoundCapturedOutput` runs every shared
  `CaptureVectors` case, scripted and hooked — the same cases the process
  adapter's `TestRunnerReportsTruncation` runs against the real Runner. Scripted `InspectPaneFn`
  panes use the real pane.process_info shapes: `mcpGroupPane`
  (helpers_test.go) reproduces the pinned multi-member foreground group —
  MCP-server children listed before the worker in raw platform order —
  that the executed real-process probe
  (`TestRealProcessSettlementWithMCPGroupMembers`,
  test/integration/pgroup_test.go) observed under the production
  transport.
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
  format and HOP path, and an unconfigured `GitExecutable` refused before
  any side effect); `TestCorroborateLaunch` (the four settlement
  outcomes, native-reference markers, pre-binding label recovery,
  exec-failure session termination, early-acceptance session activation,
  settlement from behind MCP-server members with the corroborated
  member's evidence recorded, and wrapper precedence failing closed);
  `TestCorroborateSettlement`/`TestClaimProcessMatches`/
  `TestMatchRestoredHarness`/`TestMatchRetirementTarget`/
  `TestOccupantMatches`/
  `TestClassifyGroupRetirement` (the pure decision tables incl. launcher
  exclusion, fail-closed empty identity, dual check/check-exec argv, and
  the multi-member foreground vectors — the claimed member at index 1+
  behind MCP-shaped foreign members, foreign-members-only,
  cross-member-conjunct refusal, per-member launcher skip, and wrapper
  precedence over a simultaneously matching claim-pid member — with the
  returned `SettlementEvidence` pinned per row; the restored-harness
  table covers the legitimate restore argv in both member orders, the
  trailing-positional tolerance (HOP's own relaunch argv with the
  continuation prompt after the adjacent pair), an
  embedded-reference path, a foreign executable with the exact pair,
  `--session-id`/joined/suffixed/trailing-flag shapes, argv-present
  cmdline substrings ignored, cross-member refusal, two candidates
  ambiguous, the argv-absent fallback with its token boundaries, and
  unsupported harness/empty reference);
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
  `TestResumeRestoredHarnessPredicate` (behind a marker-free foreign first
  member: an embedded-reference child, a foreign executable with the exact
  `--resume <ref>` pair, an argv-present cmdline substring and two
  candidates each fail closed with no close, supersession or relaunch; the
  restored harness ahead of its MCP child and the argv-absent fallback are
  retired; a retirement target whose pid changed occupant fails the
  close-time recheck); `TestRetirementRecheckRequiresUniqueCandidate` (A
  authorizes the retirement, then A plus a second restored-harness
  candidate at the close-time recheck, in both orders, and again when a
  later resume round replays the pending close: no close dispatched, the
  operation reconciling with its recorded pid and act evidence kept, the
  pane and both pids reported, no relaunch; stop facing the same group
  never re-dispatches the persisted close);
  `TestResumeForkingWrapperNeverRetires` (a settled
  claim with the claimed process plus a matching different-pid process,
  and an exec_pending claim on a reconciling attempt with that pair or the
  different-pid process alone, in every member order, fail closed with the
  wrapper report and nothing retired);
  `TestResumeAttestation` (continuity-gated `--confirm-absent`, refusal on
  changed/unknown instance, delayed restore retired by positive evidence);
  and the four section 5 reference traces as complete four-entity app
  scenarios (`TestReferenceTraceSubmitBeforeRunning` with its
  acceptance-wins-first alternative,
  `TestReferenceTraceColdRelaunchSubmitBeforeRunning`,
  `TestReferenceTraceExecFailureAfterRelaunch`,
  `TestReferenceTraceStopDuringLaunching`).
- `go test ./internal/app -run 'TestPrepareLaunchExec|TestFailLaunchExec|TestPrepareCheckExec|TestCheckSpawnEnvironment|TestPrepareSessionLaunchExec'` —
  the exec-boundary tables against minimal handwritten ReadStore,
  SubmissionStore and TrustSeeder stubs: launch environment validation
  (variable named,
  value never echoed), the no-claim guarantee on every pre-claim refusal,
  per-harness argv composition (incl. the exact cold-relaunch argv —
  `--resume` adjacent to the reference, the continuation prompt as the
  trailing element covered by the claim digest — and the fail-closed
  relative-assignment-path refusal on relaunch) and the unsupported
  non-Claude states, the
  claim's recorded executable/digest/pid, the trust-seed step (config-dir
  and HOME resolution, seed-before-claim ordering, not-seeded evidence for
  codex/opencode/absent configs, the fail-closed no-claim refusal on a
  seeding write failure or a missing seeder), check-exec's claim-before-load
  ordering, group-leadership refusal and the frozen argv kept verbatim —
  for BOTH exec-claimable kinds: the integration.merge vectors pin the
  frozen merge argv passing through byte-identically and a rearranged
  merge argv refused after the claim. `TestPrepareSessionLaunchExec`
  covers the Phase 3 session boundary per role: implementer/reviewer/
  manager first launches and relaunches (prompt shape, claim keys, the
  manager's repository-root cross-check and attempt-less claim), the
  solo shim's resolution order and Phase 2 env set, present-but-
  disagreeing optional variables, the plain-ReadStore typed fail-closed
  sentinel, seed-before-claim ordering and the no-claim guarantee on
  every refusal.
- `go test ./internal/app -run 'TestPlanTrustSeed|TestSeedTrustEdit|TestSanitizedEnvironmentToTrustSeed'` —
  the pure trust-seed tables in [trustseed_test.go](trustseed_test.go):
  profile-file resolution per harness and environment shape, the composed
  sanitizer-to-seeder boundary (a hostile inherited CLAUDE_CONFIG_DIR
  stripped, passthrough honored, a configured ProfileDir winning, a
  policy-stripped HOME never falling back ambiently, codex/opencode
  planning no seed), and the
  byte-surgical edit (create-if-absent map/entry/key, false→true
  overwrite with formatting preserved, unknown fields, other projects,
  number spellings, exponents and escapes byte-identical,
  duplicate-key last-wins, escaped keys, and the
  unparsable-document error table).
- `go test ./internal/app -run 'TestGoldenGrammar|TestGoldenRolePrompts|TestTemplatesQuoteGrammar|TestFreezeWorkflowArtifacts'` —
  the golden-grammar countermeasure (design section 11, L1569): every
  grammar constant and rendered line pinned against retyped golden
  literals (grammar_test.go), all six launch prompt shapes pinned
  byte-exactly — the two Phase 2 solo shapes included, since the
  test/integration fixture mirrors reproduce them, and
  test/integration's `TestRolePromptMirrors` pins the mirror side
  against the same goldens — every template quotation of a grammar line
  or verb asserted verbatim, and the role-artifact freeze (copies,
  digests, the crib's exact bytes, the derived integration branch name,
  and its refusal table). cmd/hop's real-binary per-verb contract tests
  are slice 6's and assert the same constants.
- `go test ./internal/app -run 'TestWorktreeRetirement|TestOperationKindExecClaimable'` —
  the worktree-retirement exec shapes: both frozen argvs byte for byte (no
  force option anywhere, an option-shaped path kept positional), the
  intent's exact JSON members through `decodeOperationPayload`, and the
  exact exec-claimable kind set over every operation kind;
  `TestPrepareCheckExec`'s retirement vectors pass each argv through the
  exec boundary byte-identically and refuse a caller argv with a force
  option inserted, after the claim.
- `go test ./internal/app -run TestAcquireForRetirement` — each terminal
  state is acquired under the next generation and released with no
  transition or operation journaled. A running run, a detached-HEAD run,
  a retired run and a solo run are refused before the lease moves. A
  live controller's lease defers the run, and a stale lease-free read is
  caught under the lease, which is then released again.
- `go test ./internal/app -run TestDetectRetirementMerge` — the detection
  step (through `DetectForRetirementForTest`) against the fake git and a
  simulated `hop check-exec` claim:
  - **Merged:** one claimed check with its exact frozen intent, spawn argv,
    root directory and sanitized environment, never repeated.
  - **Not merged:** silent while the tip is unchanged, re-checked when it
    moves, merged once it contains the head.
  - **No check journaled:** nothing integrated, only no-op integrations,
    ambiguous latest heads, a root that no longer holds the head, a
    deleted target branch.
  - **Failed check:** not repeated for the same head and tip.
  - **Unobserved spawn:** recovered next pass as never executed, then
    re-checked.
  - **Recovery with a claim:** each row (group gone → unknown then
    re-check; running → signaled and blocked; mismatched or unlistable →
    blocked, never signaled).
  - **Lease lost before dispatch:** nothing spawned.
  - **Dispatch barrier:** a stale eligibility read (the run running,
    stopping, resuming or completing, or its fact set, by dispatch time)
    leaves the intent pending with nothing spawned or claimed.
  - **Terminal stop requests:** a stopped or failed run carrying its stop
    request is checked once and never re-journaled.
  - **No growth:** repeated passes over merged, not-merged,
    nothing-integrated and stopped runs append no operation, transition or
    spawn; repeated refusals of an ineligible run
    (`TestAcquireForRetirement`) journal nothing and leave the lease.
- `go test ./internal/app -run TestRemoveRunWorktrees` — the removal
  step (through `RemoveRunWorktreesForTest`, export_test.go) against the
  fake git's attempt model, with rows and succeeded worktree.create
  operations seeded in the real store's JSON shape:
  - **A cut or unresolvable post-act listing:** after a refused act, a
    worktree listing cut by the capture bound before the checkout, or one
    naming the checkout only under a registered spelling that no longer
    resolves, leaves the operation reconciling and the row active, never
    released.
  - **Removal:** clean checkouts are removed by one claimed spawn each,
    with the exact frozen argv (no force option), spawn argv, root
    directory and sanitized environment, and a second pass does nothing.
    Ignored files are deleted with the checkout (approved). A listed
    checkout with no directory is removed by the same act and settles
    absent.
  - **Settled with no act:** absent, not registered, other branch,
    detached, other repository, base not an ancestor, repository root,
    and unverified provenance (no succeeded create, another path, an
    unlinked row) are each journaled once with no exec shape and a
    path-free detail, and are never examined again.
  - **Retained, journaling nothing across passes:** each uncommitted
    shape, the three hazards, a hazard beside ignored files, locked, and
    an unreadable path. A dirty checkout is removed once cleaned, and
    `interrupted-removal` is shown only after an interrupted act.
  - **After the act:** a checkout turned dirty (including under a
    repository-local `status.showUntrackedFiles=no`, where only the argv's
    override keeps git's refusal), or locked, or a bare non-zero or zero
    exit, is refused with evidence retained. The other outcomes are
    incomplete, released left on disk, and reconciling (a lost exit or an
    unobservable checkout). An incomplete removal is completed by the
    next pass's single no-force removal.
  - **Halted:** a lease lost before an act leaves one pending intent and
    touches no later row.
  - **Invariants:** no forced removal in any scenario, and no data unseen
    by git is deleted except the approved ignored files.
- `go test ./internal/app -run 'TestWorktreeRetireDecisionTable|TestWorktreeRetirementZombiePass|TestWorktreeRetirementSiblingRows'` —
  whole-pass scenarios.
  - **Decision table:** every `worktree.retire` cell of design note
    section 7, each through two `RetireWorktrees` passes (the first dies
    at the cell's crash point, the successor is a newer generation):
    - a lease lost before dispatch, and a death before any claim, each
      settle never-executed and are then removed;
    - a claimed removal that finished is adopted;
    - nothing removed is interrupted and then removed, or retained as
      `interrupted-removal` when the kill left changes;
    - a directory gone but still listed is interrupted, then pruned as
      absent;
    - an entry dropped with the directory left is released;
    - a running group is signaled and a foreign one is not, and both
      block the pass.

    Removal counts and signals are pinned per cell.
  - **Takeover barrier:** a successor acquires between a pass's
    revalidation and its spawn's claim. The zombie's claim is refused, so
    git never runs; its outcome is fenced and its intent stays pending.
    The successor settles it never-executed and removes the checkout
    exactly once.
  - **Sibling rows:** another run's rows in the same repository wait for
    that run's own pass (its checkout and fact untouched), then retire
    there. A row whose record names a sibling's checkout is released as
    another branch's, and only the sibling's own row removes that
    checkout, once. A retired run's fact never moves.
  - **Every scenario:** `retireFixture` asserts no forced removal and no
    data unseen by git deleted, the approved ignored-only checkouts
    excepted.
- `go test ./internal/app -run TestStatusWorktreeLines` — the per-row
  status input after real passes. Each row is covered in turn: removed,
  absent, released with its reason, active with a refused category and
  its evidence path, incomplete, unresolved, and plain active. A
  recovered dead act reads interrupted until the next pass removes it,
  and a solo run carries no rows.
- `go test ./internal/app -run 'TestRetirementCandidates|TestFakeRetirementCandidatesContract'` —
  lease-free triage. Every triage call is checked to take no lease and
  write nothing: the lease row, the operation and transition counts, the
  fact, every worktree revision, check-exec spawns and removals are all
  unchanged.
  - **Nothing eligible:** a running, stopping, solo, detached-HEAD,
    retired, nothing-integrated or no-op-only run is no candidate, however
    often it is triaged.
  - **Unchanged unmerged run:** it needs one pass for a new head and tip,
    then triages as `not-merged` again and again with the lease
    generation unchanged. A moved tip and then a merged tip each need a
    pass, and a retired run drops out.
  - **Merged run:** it needs passes while a worktree is retained, and not
    once retired.
  - **Verdicts:** a gone target, a moved repository (even with an
    unresolved operation), an unresolved operation (even when unmerged),
    ambiguous heads, an unreadable target, a failed check of the same pair
    and the excluded invoking run.
  - **Fail closed:** a read store without the triage read.
  - **Fake contract:** the fake's read mirrors the SQLite filters and
    ordering (`TestListRetirementCandidates`).
- `go test ./internal/app -run TestRetireWorktrees` — the pass driver:
  - **A merged run:** removed, absent, released and ignored-file rows are
    retired in one pass, with the counts, the lines, the fact at the
    clock's time and the first removal announced after the detection
    spawn. The retired run is never acquired again, and nothing is
    journaled or removed twice.
  - **Partial retirement:** a retained row keeps the pass in progress,
    only it is reported again, and it is retired once clean, without
    re-detection.
  - **Retired at once:** a merged run with no rows, and a stopped run
    carrying its stop request.
  - **No removal:** not merged, a deleted target, nothing integrated, and
    a failing check.
  - **Moved or replaced repository:** a merged, partly retired run whose
    root moved away, or now holds another repository without the head,
    is left untouched. An unresolved removal is not recovered, and a
    checkout git no longer lists is not released.
  - **Blocked and interrupted:** an earlier removal's running group
    blocks the pass. A lease lost before the first act makes it
    interrupted, with the fact unset.
  - **A cut worktree listing defers the run:** with the listing reaching
    its 64 MiB capture bound exactly at the record boundary before the first
    candidate, every row (present or gone) is retained as
    inspection-failed on every pass, nothing is removed or journaled and
    the fact stays unset; once the listing fits, the run retires.
  - **Refused:** a solo run.
- `go test ./internal/app -run TestRecoverWorktreeRetire` — the
  `worktree.retire` recovery row (through `RecoverRetirementForTest`)
  over removals whose controller died mid-act:
  - **No claim:** never-executed, then a fresh removal.
  - **Claimed group gone:** a finished removal is adopted as removed (or
    absent for a prunable checkout); a listed checkout, present or not, is
    interrupted, and while dirty it renders `interrupted-removal` until it
    is removed once clean; git's entry dropped with the directory left
    releases the row.
  - **Blocked across passes, never re-dispatched and never removed:** a
    running group (signaled), a foreign group (never signaled), an
    unlistable group, an unobservable checkout, which settles once it
    can be observed, a worktree listing cut by the capture bound before
    the checkout's record, and a listing naming the checkout only under a
    registered spelling that no longer resolves (neither is ever released
    as unregistered).
  - **Left alone or failing closed:** the pass's own generation is left
    alone; an undecodable intent, a decision shape left pending, and an
    intent naming another root fail closed.
- `go test ./internal/app -run TestInspectAttemptCheckout` — the pre-act
  inspection (through `InspectAttemptCheckoutForTest`, export_test.go)
  against the fake git's attempt-worktree model: every removable, absent,
  released and retained verdict, including the three hazards and a
  non-canonical recorded path. An index or listing past the default
  1 MiB bound but within the reads' own 64 MiB bound is decided in full
  (hidden flags found, a clean checkout removable), and one filling that
  bound exactly still is. Output past a read's bound is never decided
  on: an assume-unchanged or skip-worktree flag beyond the kept prefix
  (cut exactly on an entry boundary, or inside an entry), a clean index
  past the bound, a truncated stderr, a status read past its 1 MiB
  default, a checkout common-directory read whose prefix names another
  existing repository, and a worktree listing cut on the record boundary
  before the candidate (present or gone) are all retained as
  inspection-failed. So is a candidate git lists only under a spelling
  that no longer resolves — its own canonical path, or a registered
  spelling that differs from it (present or gone). An unresolvable
  unrelated record does not block a verified match. Only the index scan
  and the listing may carry the raised bound. Every git read must be the absolute git under the
  retirement environment, and nothing is ever removed.
- `go test ./internal/app -run TestVerifyRetirementCandidate` — candidate
  provenance: a row agreeing with its attempt's one succeeded
  worktree.create (typed or JSON round-tripped, outcome in act evidence or
  in the outcome, beside other attempts' and unsettled records) becomes a
  candidate on the full branch ref; an unlinked row, a missing base,
  branch or path, a full-ref branch, no/another attempt's/unsettled/
  another run's/another kind's operation, two succeeded operations, an
  undecodable outcome, another root, and each disagreeing branch, base or
  path are unverified with a path-free detail.
- `go test ./internal/app -run TestFakeAttemptWorktreesReproduceTheProbe` —
  the fake git's linked attempt-worktree model
  (`fakes_gitworktree_test.go`, installed on `fakeGitRepo`) against the
  process probe's executed table: status and `ls-files -v -z` pre-check
  output, every refusal text and exit status, the three data-deleting
  hazards (recorded in `DeletedHiddenData`), the untracked-files override,
  the `worktree list --porcelain -z` records (locked with and without a
  reason, detached, prunable), a prunable removal, an unlisted directory
  refused, `cat-file -e` identity answers (a moved root cannot be
  entered), and a force option recorded as a violation
  (`requireNoForcedAttemptRemovals`). The model also carries a
  repository-local fsmonitor hook: every index read or removal made
  without `core.fsmonitor=false` is recorded (`requireNoFsmonitorRuns`,
  asserted by every retirement fixture, which enables the hook). A
  submodule checkout refuses removal with the probe's text, and a dirty
  one shows ` M sub`. The pre-check is answered only in its exact
  pinned shape. The existing detached scratch-tree removals keep their
  own behavior. `IndexPadding` adds clean `d/…` entries of an exact byte
  count between `.gitignore` and `f.txt` (`indexPadding`: ordered,
  well-formed, and — filling the bound — captured by `fakeCommands` as a
  truncated prefix ending on an entry boundary with the flag cut off);
  `fillListingBefore` models a detached sibling locked with a reason sized
  so the listing holds exactly a chosen byte count before a checkout's
  record.
- `go test ./internal/app -run TestFakeWorktreesForRetirementContract` —
  the fake's leased worktree listing against the SQLite store's contract
  (`TestWorktreesForRetirement`): a seeded row with no insertion sequence
  first, then committed rows in insertion order and this transaction's
  own creates, staged saves overlaid with their revisions, and another
  run fenced.
- `go test ./internal/app -run TestFakeWorktreeRetirementContract` —
  the fake's worktrees-retired fact against the SQLite store's contract
  (`TestMarkWorktreesRetired`): set once and read back through
  `LoadRunStatus` and, identically, `WorktreesRetiredAt` inside a unit of
  work (a staged mark visible there, another run fenced), a repeat keeping the first value, another run fenced
  with nothing staged, a rollback leaving nothing, and a unit of work
  without the capability failing closed (`plainUnitOfWork`).
- `go test ./internal/app -run TestStartFeatureRunFreezesTargetBranch` —
  the freeze-time worktree-retirement target: the root's branch frozen as
  its full ref (nested names whole) and carried to the status view, a
  detached HEAD freezing none while the run still starts, and a git
  failure, a runner failure, a non-branch ref or a detached answer with
  output each refusing before InitializeRun with a value-free reason.
- `go test ./internal/app -run 'TestParseWorktreeListZ|TestHasHiddenIndexFlags|TestClassifyAncestry|TestClassifyTargetBranch'` —
  the worktree-retirement git readers against the probe-pinned bytes,
  including their malformed-input refusals (an unterminated final
  `ls-files` entry among them).
- `go test ./internal/app -run 'TestComputeResultDigest|TestDecodeOperationPayload'` —
  the canonical digest vectors (`digest_test.go`) and the persisted-payload
  decode contract (`operation_payload_internal_test.go`, a same-package
  test of `decodeOperationPayload`'s typed and JSON-generic paths).
- Phase 3 scenario suite, against the same `fakeStore` extended with
  `fakeUnitOfWork`'s `WorkflowRepositories` implementation and
  `fakeStore`'s own `MessagingStore`/`PlanStore`/`ReviewStore`/
  `WorkflowReadStore` implementations (`fakes_workflow_test.go`), wired
  into `newTestController` alongside the Phase 2 fakes
  (`usecase_run_test.go`); `seedFeatureRun`/`seedImplementTask`/
  `seedWorkerSession` (`usecase_schedule_test.go`,
  `usecase_message_test.go`) build feature-mode fixtures directly (a
  running `Run` with a `WorkflowSnapshot`, an active manager session and a
  legitimate `RunHandle`), a shortcut past `StartFeatureRun`'s bootstrap,
  which has its own suites (below):
  - `go test ./internal/app -run 'TestRecomputeReleases|TestAssignReadyTasks'` —
    the section 6 scheduling pass: dependency release only once every
    prerequisite is integrated; assignment claims the lowest-seq ready
    task into a free slot (task/attempt/session/worktree/pane, a review
    task's own frozen subject rather than the integration head,
    `MaxWorkers` bounded inside the assignment transaction, a consumed
    retry's already-reserved attempt launched rather than recreated); a
    stop racing between release and assignment creates nothing — the
    assignment transaction reads the run fresh and checks
    `CanAcceptManagerVerb()`/`StopRequested` before any task/attempt/
    session write, not merely before the later external worktree/pane
    act. The first case also pins the worktree row's attempt/base link and
    the Claude session's pre-assigned native reference.
  - `go test ./internal/app -run 'TestAssignedSessionsLaunchFromTheirOwnWorktrees|TestAssignedNonClaudeSessionsCarryNoNativeReference|TestUnlinkedWorktreeRowsResolveNoLaunchDirectory|TestWorktreeFallbackServesOnlyALoneUnlinkedRow'` —
    feature worktree linkage through hop launch on the fake store (the
    real-store half is `internal/adapters/sqlite`'s
    `TestAssignedWorktreesLinkTheirAttempts`). Two implementers and a
    reviewer each get a linked row, their own native reference and a
    launch context naming their own worktree; `PrepareSessionLaunchExec`
    accepts each one from that worktree (`--session-id <ref>`) and refuses
    it from a sibling's with no claim. Codex/opencode children carry no
    reference. Several unlinked rows resolve no worktree and the launch
    refuses fail-closed. The run-wide fallback serves only the run's lone
    unlinked row (the solo shape): a lone row linked to a sibling attempt,
    or one beside an unlinked row, resolves nothing and the launch is
    refused.
    The fake store enforces the real contracts these paths meet: the
    worktree insert's run/attempt/UNIQUE(path) rules, and
    `LoadSessionLaunchContext`'s attempt row, by-attempt worktree rule,
    relaunch fact and fail-closed incarnation. The fake runtime gives
    every created worktree its own path (spike S9).
  - `go test ./internal/app -run TestRequireWorkflowRepositoriesFailsClosed` —
    the `WorkflowRepositories` fail-closed table test via
    `plainStore`/`plainUnitOfWork` (a `StateStore` implementing exactly
    `UnitOfWork`, deliberately not also `WorkflowRepositories`): every
    feature-mode use case refuses with `ErrWorkflowRepositoriesUnsupported`
    before any side effect, never a nil-interface panic.
  - `go test ./internal/app -run 'TestCreateTask|TestClosePlanReopenOnCreate|TestClosePlanRefusesEmptyPlan|TestRequestRetry'` —
    section 8's manager-plan verbs: zero- and chained-dependency creation,
    self-dependency and cross-run dependency refusal, non-manager caller
    refusal, request-ID idempotency (identical retry returns the original
    acceptance), an oversized title malformed before any side effect
    (`TaskCreate`'s own file-first write included), plan reopen-on-create,
    empty-plan refusal, and a full retry cycle (terminal needs-rework
    attempt, below the retry limit, the new attempt reserved immediately
    inside `RequestRetry` itself).
  - `go test ./internal/app -run 'TestMessagingReferenceTraceRelayedQuestion|TestSendMessageValidation|TestAckMessageRequiresOwnDelivery|TestAckMessageRequiresCurrentIncarnation|TestShowMessage|TestFetchMessageEmptyQueueCommitsNothing|TestFetchMessageCrossRunRefused|TestAckMessageCrossRunRefused'` —
    section 7's messaging verbs: the complete relayed-question reference
    trace end to end (worker question → manager relay to human with
    `--relay-of` → human answer → manager forwards using only the
    `origin` envelope field → worker acks, incl. forward-before-ack redo
    idempotency); send addressing/kind legality, mailbox-closed refusal,
    an oversized inline body malformed before any store call; ack refused
    for a message never delivered to the ACKING session itself (a
    predecessor's delivery proves nothing), then accepted after fetch,
    then idempotently duplicate on repeat; ack refused for a delivery
    served to an earlier, now-superseded incarnation of the SAME session
    (a warm reattach), then accepted once the current incarnation is
    itself served; `ShowMessage` (the envelope plus full delivery/ack
    history incl. a re-serve, writing/delivering/acking nothing itself,
    not-found for an unknown id or one from a different run, and its own
    `WorkflowReadStore` fail-closed case via `plainReadStore`, a
    `ReadStore` deliberately not also `WorkflowReadStore`); an empty
    fetch committing neither a delivery row nor a receipt; a session
    claiming a run other than its own is refused by send (before the
    body artifact is ever written), fetch and ack alike.
  - `go test ./internal/app -run TestStatus` — the presentation/status
    integration: the feature-mode task table (seq, kind, state,
    dependencies, attempt count); the run's most recently created
    integration row; guard shortfalls (`EvaluateReadiness`'s missing list)
    across an open plan, an unintegrated task once the plan closes, and
    every verdict shortfall (reject, stale-subject) against an integrated
    head — `check-missing` never clears in this suite, since no fake
    tracks a combined-candidate check receipt yet (documented on
    `guardShortfallsLocked`, not a bug); the section 7 attention surface
    (queued vs. in-flight ages, the `[messages] attention_after`
    threshold, `AddressLive` gating on the manager session's own state,
    `NeedsAttention`'s OR-reduction over `Mailboxes`); pending human
    questions clearing once answered; a solo run rendering every Phase 3
    field at its zero value.
  - `go test ./internal/app -run TestStoreVectors` — every
    `internal/testsupport/storevectors` vector
    ([its own guide](../testsupport/storevectors/AGENTS.md)) driven
    against `fakeStore` through the ordinary `PlanStore`/`MessagingStore`
    ports and the unit of work's worktree repository, confirming each
    documented refusal and the by-attempt lookup vector
    (`WorktreeLookupByAttempt`); `TestFakeWorktreeIndexContract` adds the
    same-transaction read-back and rollback cases the sqlite adapter's
    `TestWorktreeIndexByAttempt` pins.
- Slice 6b bootstrap suites (`go test ./internal/app -run '<name>'`):
  `TestStartFeatureRun` (the journal order freeze → initialize →
  integration.init → workspace.create → pane.open with each act's intent
  pending at dispatch and one revalidation heartbeat before it; the happy
  shape; the pre-check refusal writing nothing; a ref appearing after the
  pre-check failing as a collision even at the base; git's refusal and a
  runner failure; workspace.create and pane.open failures with label
  recovery found/absent/ambiguous; a stop request or a lost lease before
  each act dispatching nothing; eleven pre-side-effect refusals;
  StartRun's exact unloadable-policy text; the sequence mismatch
  re-frozen with final digests, and exhaustion); `TestResolveRunWorkflow`;
  `TestResumeFeatureBootstrapContinuation` (every crash point of
  integration.init, workspace.create and the manager's pane.open,
  idempotent re-rounds, stop over a failed init, stop-pending before any
  continuation); `TestFeatureStopUnplacedLaunch` and
  `TestRetirementUnplacedLaunch` (found/absent/ambiguous/undecodable/
  unclaimed, manager and worker); `TestFeatureStopResolvesIntegrationInit`
  (adopt, collision, completing CAS, zombie adoption, read error and a
  refused completion outstanding); `TestStoreVectorsFeatureBootstrap` and
  `TestFeatureBootstrapSymbolicIntegrationRef` (dangling and
  existing-target symrefs failing closed at start, before the CAS, on
  resume and on stop, the foreign target never written);
  `TestFakeFeatureBootstrapContracts` (the fake's feature shape,
  one-manager index and workspace argument refusals). The bootstrap's
  composition with slice 6 (`usecase_featurehandoff_test.go`):
  `TestStartFeatureRunHandsOffToLaunchCorroboration` (StartFeatureRun then
  the loop's launching-only pass: a settled manager claim moves the run
  to running, an unsettled one leaves it launching, an exec failure fails
  it with the manager-launch cause, and the pass's assignment defaults are
  the roots the bootstrap froze); `TestResumeFeatureBootstrapManagerExecFailure`
  (an interrupted bootstrap whose manager exec failed — bound, or with the
  pane.open outcome lost and the pane gone, before or after the resume
  round — fails through the manager-lineage disposition, never left
  launching; an unbound exec-pending claim stays in flight past the
  launch-claim deadline); `TestFeatureTerminalFailureResolvesIntegrationInit`
  (the failure path completes, adopts or fails an unresolved init before
  the failed report, and stays failing while the ref is unobservable);
  `TestFeatureTerminalFailureWaitsOnUnplacedLaunch`. The stateful fake
  git mints 40-hex object IDs, reproduces G1/G3's create-only CAS and the
  probe-pinned symbolic-ref/`--no-deref` behavior, and every integration
  and bootstrap scenario requires zero dereferencing update-refs; the
  fake runtime's workspaces carry the S8 shape.
- Slice 2b scenario suites (`go test ./internal/app -run '<name>'`):
  `TestDriveIntegration*`/`TestIntegration*` — the integration
  decision-table rows against a stateful fake git whose `update-ref`
  reproduces the G1/G3-pinned CAS semantics (all crash columns for
  merge/publish/reset, D1 retirement-kind barriers, the D2
  materialization barrier, no-op adoption incl. settled-receipt reuse,
  conflict evidence, the unknown-outcome rule at integration scope, stop
  mid-merge, stop completing a pending rollback, and both INTEGRATION
  BARRIERS — the zombie CAS dead at the ref store after a takeover
  advanced the branch, and the unmoved-head window in both
  interleavings); `TestRetireSettledSessions` (dispatch is never
  termination, mismatch never absence, exec-failed retirement, self-exit
  interruption, terminal failure only after observed termination) and
  `TestRetireSettledSessionsReportsRunFailing`;
  `TestCorroborateSessionLaunchesSettlesChildExecFailure` (from a
  launching attempt: needs-rework with budget left, failed with the
  mailbox closed at the limit, interrupted under a held stop — attempt,
  task, session, mailbox, notice and transition reasons each asserted —
  and the pending/already-execed branches unchanged);
  `TestResumeFeatureSettlesChildExecFailure` (the same settlement on
  resume after a controller death: budget left, exhausted budget with
  the run failing on retirement, a held stop routed to stop handling,
  a stop landing mid-resume) and
  `TestResumeFeatureManagerExecFailureFailsTheRunOnRetirement`;
  `TestManagerExecFailureFailsTheRun` (launching run failed from
  corroboration, the durable retry, a running run's exec-failed
  successor) and `TestManagerExecFailureDispositionFalsePositives`
  (an earlier exec-failed member before a working successor; every
  manager legitimately terminal at completion, stop, or after
  re-validation);
  `TestSubmitReviewVerdict` (acceptance atomicity, duplicate/conflicting,
  frozen-subject mismatch, non-reviewer refusal, mailbox transient,
  oversized reasons); `TestMailboxClosure` (both orders, both closure
  causes, the snapshot-equality forced retry); `TestGuardEnforcement`
  (a scripted manager calling every verb cannot complete a run without
  real receipts), `TestEnsureReviewTask`, `TestDriveCompletion`
  (retirement observed, re-validation fallback, stop precedence);
  `TestResumeFeature` (warm reattach, per-session attestation with
  continuity evidence, manager lineage/succession with historical
  parents preserved and stale-manager verb refusal, changed/unknown
  instance refusal, Claude-only relaunch);
  `TestPublishRunPresentation` (manager-first tokens, republication,
  rehydration); `TestPortSurfaceAllowlist` (the exact method set of
  every port interface, asserting no typed-input method exists anywhere
  now that slice 6 deleted `Runtime.SendText`, the one legacy exception
  it ever admitted); and the feature reference traces
  `TestFeatureTrace*` (traces 1, 3, 4 with the exhaustion variant, 5
  and 6 at app level; trace 2 is `TestMessagingReferenceTraceRelayedQuestion`).
- Per-attempt launch recovery (`go test ./internal/app -run 'TestAttemptLaunch|TestStatusWorktreeOperations|TestFakeWorktreeIndexContract'`;
  the pass and assignment-path suites in usecase_attemptlaunch_test.go,
  the resume, stop and failure suites in
  usecase_attemptrecovery_test.go): `TestAttemptLaunchLoopActError` (an act
  error reported, never a pass failure; a later pass adopting the lost
  response's checkout by provenance with the workspace its creation label
  names and launching it; nothing created twice within the bounded wait,
  the attempt settled past it and the freed slot filled in the same pass;
  unrelated checkouts at act time and at recovery, the retry limit
  included; an unverifiable created checkout keeping the act's workspace;
  an unrecorded placement opening nothing; an unreadable listing);
  `TestAttemptLaunchAssignmentPath` (the refuse-if-exists check —
  present and symbolic branches settle naming the ref only, an
  unobservable check defers the create — an unwritable assignment
  artifact continued on the next pass, a pane act error recovered by
  label through launch corroboration); `TestAttemptLaunchStopRefusal` (a
  stop refusing the worktree or the pane dispatch ends the pass cleanly,
  the intent is recorded refused before dispatch, and stop reaches stopped
  in its first round);
  `TestAttemptLaunchCrashRecovery` (every crash point on resume: after the
  intent — bounded wait anchored at the operation's creation, then the
  budgeted settlement at both budgets; after the act — adoption, the
  linked row and a fresh-incarnation launch accepted by hop launch from
  the recorded worktree, resume ending resumed; after the worktree —
  artifact and pane continued; before the intent — re-drive at the
  integration head, or the collision settled; a sibling attempt's row
  never adopted); `TestAttemptLaunchShutdownResolvesWorktree` (stop
  adopting without launching, stop outstanding over an unverifiable
  checkout, the terminal failure waiting on the operation, a failing
  run's resume launching nothing); `TestAttemptLaunchUnattributableIntent`;
  `TestAttemptLaunchFailurePrecedence`,
  `TestAttemptLaunchManagerFailurePrecedence`,
  `TestAttemptLaunchFailureHandoff` (repeated resumes hand a
  launch-suppressed child to the loop's failure cleanup, which fails the
  run with no stop; unresolved worktree work keeps the run reconciling)
  and `TestAttemptLaunchFailureAtDispatch` (a successor manager's
  exec_pending → exec_failed during the child's dispatch heartbeat refuses
  the worktree.create and the pane.open, each recorded failed before
  dispatch and resolved by the failure cleanup without waiting)
  (usecase_attemptprecedence_test.go: an exhausted recovery settlement or
  fresh collision assigns nothing more in the same pass; a manager exec
  failure recorded before the create suppresses the intent; on resume a
  settlement that fails a task stops a sibling's continuation in either
  target order and leaves a sibling's failed launch resolved but
  unsettled; a failed successor manager suppresses a recorded child's
  continuation and a no-intent child's re-drive while the run is
  resuming, and a historical failed member does not);
  `TestStatusWorktreeOperations`. The fakes reproduce the pinned shapes
  these paths read: `fakeCommands` answers `symbolic-ref -q` with exit 1
  and `rev-parse --verify` of an attempt branch as absent (the process
  adapter's `TestGitRefSemanticsThroughRunner`), and `fakeRuntime`'s
  labeled `CreateWorktree` names its workspace for `FindWorkspaceByLabel`.
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
