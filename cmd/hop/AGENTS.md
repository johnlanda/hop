# cmd/hop

## Purpose

Composition root of the `hop` binary. It parses the command line with the
standard `flag` package, constructs the concrete adapters and the
application `Controller` exactly once per command, dispatches to the
selected command and owns the process exit codes, the single state-root
resolver and the process-signal handling. It holds no business rule:
decisions live in [internal/app](../../internal/app/AGENTS.md), and the
two exec boundaries (`hop launch`, `hop check-exec`) drive the app's
prepare use cases and then exec through the process adapter.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [main.go](main.go) | `main`, `run`, `dispatch`, `printUsage`, `exitOK`, `exitFailure`, `exitUsage` | Entry point; maps a command name to its handler — Phase 2's `run`/`status`/`stop`/`resume`/`result`/`version`/`doctor`/`plugin-context`/`help`, worker plumbing `launch`/`check-exec` (listed under its own usage heading) and Phase 3's `task`/`plan`/`msg`/`answer`/`review`/`view` — and a failed write to `exitFailure` |
| [compose.go](compose.go) | `controllerAPI`, `controllerConfig`, `deps`, `defaultDeps`, `openController`, `describeStoreOpenFailure`, `waitInterval`, `lookupExecutable` | The composition root proper: resolves the controller's own `git` executable once against its own PATH (`os.Getenv("PATH")`, never the sanitized worker environment) and sets `Controller.GitExecutable`, opens the SQLite store under the resolved state root, wires the system/process/config adapters (including `system.TrustSeeder` into the `Trust` port), the same SQLite store into `Messages`/`Plan`/`Reviews` unconditionally, and — for pane-acting commands — the Herdr runtime (compile-time `app.Runtime` assertion) plus `Workspaces` (the same `herdr.Runtime` value, a separate port) and `Presentation` into one `app.Controller`; `deps` carries every effectful seam (env, clock, wait, signals, exec, store opening, and `inspectPath`, the retirement path seam) so command tests substitute fakes; `lookupExecutable` implements `app.ExecutableLookup` over the sanitized PATH. `controllerAPI` is slice 6's complete driving surface: every Phase 2 method, hop run's feature bootstrap (`StartFeatureRun`, `ResolveRunWorkflow`), the post-merge worktree-retirement pass (`RetirementCandidates`/`AcquireForRetirement`/`RetireWorktrees`/`ReleaseRetirement`), plus the Phase 3 worker-plumbing verbs (`SendMessage`/`FetchMessage`/`AckMessage`/`ShowMessage`/`Answer`/`CreateTask`/`RequestRetry`/`ClosePlan`/`SubmitReviewVerdict`), the feature-mode scheduling pass (`RetireSettledSessions`/`RecomputeReleases`/`DriveIntegration`/`EnsureReviewTask`/`AssignReadyTasks`/`DriveCompletion`/`CorroborateSessionLaunches`/`DriveFeatureChecks`), feature-mode resume/stop (`ResumeFeature`/`DriveFeatureStop`), presentation (`PublishRunPresentation`), `hop view` (`SelectRunView`/`ClearRunView`) and three scheduling-pass support methods `featureloop.go` calls each tick: `MessageWaitDefault` (hop msg wait's frozen-default resolver), `AssignmentDefaults` (every run-fixed `AssignReadyTasks` field: frozen `MaxWorkers`/`Harness`/`ReviewerHarness` and the frozen `RepositoryRoot`/`StateRoot`) and `ResolveIntegrationHead` (the run's current integration branch head) |
| [pathinspect.go](pathinspect.go) | `inspectCanonicalPath`, `resolveExistingPath`, `resolveMissingPath`, `resolveExistingPrefix`, `splitLeaf`, `plainName`, `unresolvable`, `errPathNotAbsolute`, `errPathUnresolvable` | Implements `app.PathInspector` for worktree retirement ([phase-3-worktree-retirement.md](../../docs/plan/phase-3-worktree-retirement.md) sections 4 and 13). The recorded spelling is resolved as the filesystem resolves it, never collapsed first, because `link/..` is the parent of the link's target. Existence comes from `Lstat` on the raw spelling (a dangling link exists and is not followed). An existing path is canonicalized by `EvalSymlinks`, except a dangling link leaf, which is its resolved parent (split without `filepath.Dir`, which cleans) plus its name. A missing path is canonicalized by `resolveExistingPrefix`, which walks the components in order: each symbolic link is resolved before the next component, and `..` steps up from the directory resolved so far. The walk stops at the first missing component, and the missing names are appended, so a checkout that is already gone still matches git's canonical listing. The following are `errPathUnresolvable` (the checkout is retained, never absent): a `..` among the missing names, a dangling or looping link on the way, a non-directory used as a directory, an unreadable component, and a path that changed mid-inspection. Every error is value-free (`unresolvable` keeps only the system cause), and a relative path is refused. `defaultDeps` wires it as `deps.inspectPath` |
| [retirement.go](retirement.go) | `runWorktreeRetirement`, `retireOneRun`, `reportLines`, `worktreeOutcomeLines`, `dispositionLines`, `worktreeDetailLines`, `retainedLabel`, `retainedAction`, `releasedLabel`, `printLines`, `retirementPassTimeout`, `retirementReleaseTimeout` | The worktree-retirement pass as the commands run it ([phase-3-worktree-retirement.md](../../docs/plan/phase-3-worktree-retirement.md) sections 3 and 6). It triages the repository lease-free (`RetirementCandidates`, excluding the invoking run). Each run with work gets `AcquireForRetirement`; `ErrLeaseHeld` renders the "deferred" line and a raced ineligibility is silent. Then `runHeartbeats` for the handle, `RetireWorktrees` (the hop binary, the process environment, `deps.inspectPath`, and a callback printing `retiring worktrees of r<seq>…` just before the first removal spawn), the heartbeats stopped, and `ReleaseRetirement` under its own bound. It returns section 6's fixed lines: the retired summary; removed (with the Herdr-workspace hint), already absent, released (`HOP will not remove it`, or `left on disk; no longer managed by HOP` after an act), retained with its category and `  action:` line, incomplete, unresolved and not-dispatched lines; and the deferred, skipped (history missing), blocked, check-failed and interrupted run lines. Not-merged and nothing-integrated runs print nothing. Every failure (triage, acquisition, pass, lost heartbeat, release) is one value-free stderr line naming only the run label, and the remaining runs are still attempted. `worktreeDetailLines` renders `hop status -run`'s per-row lines for a feature run, each non-final row with its human action. Every branch and path (`worktreeOutcomeLines`, `worktreeDetailLines`, `retainedAction`'s evidence path) renders through statuscmd.go's `safeRenderExternal` (Astra F2) |
| [stateroot.go](stateroot.go) | `resolveStateRoot`, `requireWorkerStateRoot` | The design's single state-root rule: `${XDG_STATE_HOME:-$HOME/.local/state}/hop` with `HOP_STATE_DIR` as the only override (relative refused); worker contexts REQUIRE the launch-provided absolute `HOP_STATE_DIR` and never fall back |
| [loop.go](loop.go) | `runControllerLoop`, `runHeartbeats`, `loopResult`, `detachAndReport`, `releaseQuietly`, `resolveRunArg`, `seqLabel`, `exitForRunState` | The foreground controller loop: concurrent 10s heartbeats under the 30s TTL, one line per run transition, launch corroboration, stop routing, 2s check polling; run arguments resolve as UUIDs or `r<seq>` labels. When `CorroborateLaunch` reports `LaunchSettled` or `LaunchAlreadySettled`, the loop re-reads run status once and prints the transition (`printRunState`, the shared gate the ordinary per-tick poll also uses) before driving checks that same tick — otherwise a trivial check claimed and finished later in the same tick could advance the run straight to completing/completed before the next poll ever observed it running |
| [runcmd.go](runcmd.go) | `runRun`, `finishControllerLoop`, `watchDetachSignals`, `resolveRepositoryRoot`, `hopExecutablePath`, `stringList` | `hop run "<brief>" [--workflow solo\|feature]`: an unknown `--workflow` is usage before any store access; `ResolveRunWorkflow` picks the workflow (a refusal there runs no retirement pass), then the repository's worktree-retirement pass runs and prints its lines (a new run has nothing to exclude), then StartRun + the Phase 2 loop or StartFeatureRun + the feature loop (`finishFeatureControllerLoop`); the start line, then the loop; SIGINT/SIGTERM detach (second signal force-exits); `ErrStartRefused` maps to exit 2 for both workflows |
| [statuscmd.go](statuscmd.go) | `runStatus`, `statusRetirementPass`, `renderStatus`, `renderRunListing`, `renderRunDetail`, `featureDetailLines`, `isFeatureMode`, `workflowLabel`, `targetBranchLabel`, `worktreeRetirementLabel`, `taskLabelsByID`, `taskLabelFor`, `depLabels`, `sessionBindingFor`, `safeRenderExternal` | `safeRenderExternal` (Astra F2), delegating byte for byte to `app.RenderExternal`, is the one rendering boundary for every externally sourced string `hop status` prints, each as one whole field: paths (the solo worktree, feature worktree rows, the task table's worktree, artifacts, check evidence, question bodies, reasons), Herdr binding identifiers (the detail's, each session's, an attention action's), branch names (the target, on its own line and inside the `worktrees:` sentence, plus worktree rows' and worktree operations' branches — retirement.go's report lines included), text embedding such values (the trust-seed evidence, which names the seeded worktree path, and the last check's detail, which is recorded error text) and git object ids (the integration's source, pre-merge and merge commits and a rejected review's subject); every other field is a HOP-generated token or fixed text: raw when valid UTF-8 with no C0/DEL/C1 control byte, no double quote and no backslash, else `strconv.Quote`'s escaped form, which always starts with a double quote a raw rendering never can — these are operator- or principal-selected strings no store or resolver on their path rejects for control bytes. `hop status`: non-terminal runs by default, `-all` includes completed/failed/stopped, `-run` renders the full detail block including the claim's trust-seed evidence line, the unknown-check options, (Phase 3) a `workflow: solo|feature` line sourced only from `WorkflowSnapshot.Mode` and one `worktree op:` line plus its `action:` line per unresolved feature `worktree.create`; a feature run's detail adds, right after it, the frozen worktree-retirement `target:` (or `none (detached HEAD at freeze; …)`) and the `worktrees:` fact — `retired <RFC 3339 UTC>`, `kept (no target branch)`, or `not retired (…)` carrying the plain warning that removal deletes ignored files ([phase-3-worktree-retirement.md](../../docs/plan/phase-3-worktree-retirement.md) section 6); a feature run's detail replaces the single `worktree:` line with one line per worktree row (`worktreeDetailLines`), while a solo run keeps its single line; then (STATUS-1) `featureDetailLines`' six-section feature-mode block — task table, latest integration, `EvaluateReadiness`'s guard shortfalls verbatim (`verdict-rejected` alone dispatches to `GrammarVerdictRejectedLine`, naming its review's id, subject and safe-rendered reasons path — Astra F3), one section 7 attention line per mailbox (with its named human action when Attention holds), pending human questions with the exact `hop answer` invocation, and per-session roles/bindings — every uuid reference resolved to its `t<seq>` label here, never in `internal/app`. Both forms first run the repository's worktree-retirement pass (`statusRetirementPass`: its own 15-minute bound, where a first SIGINT/SIGTERM cancels the pass and a second exits, and a hop binary that cannot be located skips it with one line). The `retiring…` line comes before the rendering and the pass lines after it. `listingMarkers` adds `blocked, needs attention` (section 7) on both the listing and the detail state line. The command never wires the Herdr runtime, and the pass never changes the exit code. State is data, not an exit code |
| [stopcmd.go](stopcmd.go) | `runStop`, `dispatchStopResume`, `driveStopRounds`, `observeStop`, `outstandingSummary` | `hop stop <run-id>`: monotonic stop request (lease-free, always reachable once the run is confirmed to exist), then dispatches by the run's frozen mode — `Resume`/`DriveStop` for solo, `ResumeFeature`/`DriveFeatureStop` for feature, never a fallback between them — with the acquired lease or observation of a live controller's stop; exit 0 only on observed `stopped`, rerunnable otherwise. Every step past `RequestStop` touches the Herdr runtime |
| [resumecmd.go](resumecmd.go) | `confirmAbsentValue`, `runResume`, `runResumeFeature`, `resumeFeatureSessionLines`, `resumeDetailLine`, `runLabel` | `hop resume <run-id>`: once the run argument resolves, the repository's worktree-retirement pass runs excluding that run and prints its lines; then it loads the run's frozen mode via `Status` before any lease acquisition, then dispatches by mode — `Resume` for solo (`--confirm-absent` the bare Phase 2 boolean) or `ResumeFeature` for feature (`--confirm-absent=<session-id>`, one `confirmAbsentValue` flag.Value serving both forms); continuable outcomes stay in the matching loop (`finishControllerLoop`/`finishFeatureControllerLoop`; the caller's directory only scopes an `r<seq>` lookup — a feature run is scheduled in its frozen repository, whichever directory `hop resume` ran in), fail-closed/reconciling/unsupported reports release the lease and exit 1 for the human to act and rerun. The mode-load and both `--confirm-absent` usage refusals run before `Resume`/`ResumeFeature`, so they never touch Herdr; every outcome past that point does |
| [resultcmd.go](resultcmd.go) | `runResult`, `runResultSubmit`, `submissionLine` | `hop result submit` (section 7): `--summary`/`--commit`, IDs defaulting from `HOP_*`, incarnation from `HOP_INCARNATION_ID`; accepted/duplicate exit 0, everything else 1; a transient outcome's first line is exactly the retry line its typed reason selects (`app.GrammarSubmissionTransientLine`: not-running or drain), never inferred from the store's `Detail` (which goes to stderr), and a transient outcome naming no known reason prints no protocol line and exits 1 |
| [launchcmd.go](launchcmd.go) | `runLaunch`, `launcherWorkerDir`, `resolveCanonicalPath` | `hop launch --run --attempt` (the permanent solo shim) and `hop launch --run --session` (every feature-mode pane), mutually exclusive, the worker exec boundary: worker state root, the launcher's own symlink-resolved working directory (refused with a diagnostic before any store access when unresolvable), `resolveCanonicalPath` as the request's `ResolvePath` seam for the recorded-worktree (or, for the manager, repository-root) cross-check whose resolved directory is the workspace-trust seed key, `PrepareSessionLaunchExec` (session context load → validate → sanitize → per-role/per-harness compose — all three harnesses launch, cold resume is Claude-only — resolve → worktree cross-check → trust-seed → session-keyed claim), `process.Exec`; a post-claim failure settles exec_failed via `FailLaunchExec`; one stderr line, never an environment value. Never wires a Runtime, so every refusal before the claim is store-only |
| [checkexeccmd.go](checkexeccmd.go) | `runCheckExec` | `hop check-exec --op -- <argv>`, the check exec boundary: group-leadership fact, `PrepareCheckExec` (claim before exec), `process.ExecResolved` so the frozen argv runs verbatim and the check's exit status propagates through the exec |
| [taskcmd.go](taskcmd.go) | `runTask`, `runTaskCreate`, `taskCreateLines`, `runTaskRetry`, `taskRetryLines`, `runPlan`, `runPlanClose`, `planCloseLines`, `managerEnvIdentities`, `readManagerEnvIdentities`, `writeLinesAndExit` | `hop task create/retry` and `hop plan close` (design section 8): manager-only, identities from `HOP_*` env (no flags name them — these verbs run only inside a HOP-launched manager pane), the instructions/file-first protocol before any store call; every success line renders through `internal/app/grammar.go` (`hop task retry`'s `retry accepted t<seq> attempt <n>` / `duplicate t<seq> attempt <n>` from the store-reported `TaskSeq`); `writeLinesAndExit` is every worker-plumbing verb's shared exit-code mapping (a `refused:` or `transient:` first line always exits 1); `writeVerbOutcomeAndExit` adds a transient outcome's value-free store detail as one `<verb>: <detail>` stderr line. A transient plan verb prints `app.GrammarTransientRunNotRunningLine` alone on stdout |
| [msgcmd.go](msgcmd.go) | `runMsg`, `runMsgSend`, `sendMessageLines`, `runMsgNext`, `fetchOneMessage`, `deliveredMessageLines`, `runMsgWait`, `runMsgAck`, `ackMessageLines`, `runMsgShow`, `showMessageLines`, `runAnswer`, `answerLines`, `readBodyFlag` | `hop msg send/next/wait/ack/show` (design section 7) and `hop answer`: a transient send renders like a transient plan verb (`writeVerbOutcomeAndExit`); `readBodyFlag` resolves a body from `--file` xor `--body`; `fetchOneMessage` is the one non-blocking `FetchMessage` call both `hop msg next` and every round of `hop msg wait`'s CLI-side 1s poll loop share — the FIRST attempt validates the caller's `HOP_*` identity before `hop msg wait` ever reaches `MessageWaitDefault`, so a missing/malformed identity fails identically for both verbs; `hop msg wait`'s default `--timeout` resolves from the run's frozen `[messages] wait_timeout` only when `flag.Visit` finds no explicit flag. `hop answer` is a human/controller-machine command — no `HOP_*` env, no lease — resolving the repository and state root exactly like `hop stop`/`hop resume` |
| [reviewcmd.go](reviewcmd.go) | `runReview`, `runReviewSubmit`, `writeReviewResultAndExit` | `hop review submit` (design section 8): reviewer-only, identities from `HOP_*` env including `HOP_TASK_ID`/`HOP_ATTEMPT_ID`, the reasons body read from `--reasons-file` before any store call; `writeReviewResultAndExit` renders exactly the retry line the verdict's typed transient reason selects — not-running or drain, the same two lines and the same unknown-reason refusal as `hop result submit` — (store `Detail` to stderr, never the stdout protocol line) separately from `writeLinesAndExit`'s ordinary accepted/duplicate/refused handling |
| [viewcmd.go](viewcmd.go) | `runView`, `runViewSet`, `runViewClear` | `hop view set --run <id\|r<seq>>` / `hop view clear` (design section 9): always wires the Herdr runtime (`withRuntime: true`) since `SelectRunView`/`ClearRunView` unconditionally dial the `Presentation` port — `resolveRunArg`/`runLabel` resolve and validate the run BEFORE that call, so only usage errors and an unknown run/label are store-only; every success line requires a live Herdr connection |
| [grammarrender.go](grammarrender.go) | `refusalToken`, `reviewRefusalToken`, `renderRefusal` | Renders a worker-plumbing use case's refusal as the section 7 grammar's enumerated reason token: `refusalToken` renders a store-set `Reason` field directly (every `PlanStore`/`MessagingStore` outcome sets one at its own decision point — never Detail-text matched), falling back to `unauthorized` only on the empty-reason defect case; `reviewRefusalToken` classifies `SubmitReviewVerdict`'s outcome kind directly (`malformed`/`conflicting`/`stale` — the only three kinds that reach it; `not-reviewer` and `subject-mismatch` are real refusal SCENARIOS `internal/adapters/sqlite/review.go` distinguishes internally, but neither is a distinct `ReviewOutcomeKind` this function ever sees, so both render `refused: stale` — pinned by `TestReviewRefusalTokenIsExhaustive`) |
| [featureloop.go](featureloop.go) | `featureCheckDriver`, `runFeatureControllerLoop`, `featurePassResult`, `managerLaunchLines`, `attemptLaunchLines`, `runFeatureSchedulingPass`, `describeFeatureCheckReport`, `finishFeatureControllerLoop` | The feature-mode foreground loop: one scheduling pass per tick (`RetireSettledSessions` → `RecomputeReleases` → `DriveIntegration` → `EnsureReviewTask` → `DriveCompletion` → `AssignReadyTasks` → `CorroborateSessionLaunches`), gated to `CorroborateSessionLaunches`-only while the run is launching (no settled manager session yet to schedule against) and to `DriveCompletion`-only while it is completing (completion retirement continues across ticks), plus async `DriveFeatureChecks` via `featureCheckDriver` (mirrors `checkDriver`'s solo shape). The pass honors its reports: retirement's `RunFailed`/`RunFailing` or a completion `RunState` other than running halts it — nothing further runs, no new check round is dispatched (`AssignReadyTasks` accepts only a running run) — and the loop observes the terminal state on a later tick and exits with its normal code. A manager whose launch exec failed fails the run in the app; the pass prints the solo loop's own `launch failed` line for it (`managerLaunchLines`, reusing `describeLaunchProgress`), and the loop then exits 1; `runFeatureSchedulingPass` populates every `AssignReadyTasks` field each pass: `AssignmentDefaults` for every run-fixed one (the frozen repository and state roots included — never the invoking directory), the running binary for `HOPPath`, `ResolveIntegrationHead` for `IntegrationHeadCommitOID`. An attempt launch the pass could not complete never ends the loop: `AssignReadyTasks` reports it (`AssignmentReport.Launches`/`Blocked`) and `attemptLaunchLines` prints one fixed-text `attempt t<seq>a<n> <disposition>: <detail>` (or `worktree blocked: …`) line each, while an error from `AssignReadyTasks` — store or lease — still ends the loop after those lines |
| [doctor.go](doctor.go) | `runDoctor`, `renderReport`, `renderStateRoot`, `defaultDoctorTimeout` | `hop doctor`: the Phase 1 report plus the store-path line (resolved state root, source label, store presence) computed without opening the database |
| [version.go](version.go), [plugincontext.go](plugincontext.go) | `runVersion`, `resolveVersion`, `recordingWriter`, `runPluginContext` | Unchanged Phase 0/1 commands |
| [fakes_test.go](fakes_test.go) | `fakeController`, `testDeps`, `newTestDeps` | Scripted `controllerAPI` fake (every Phase 2 and Phase 3 method, including `messageWaitDefault`/`assignmentDefaults`/`resolveIntegrationHead` script fields) enforcing the real use cases' contracts it scripts around. Worktree retirement: unscripted triage reports no candidates. The fake tracks the held retirement lease (`retirementHeld`) and refuses a second acquisition while one is held, a pass or release without one, an acquisition without a run or controller id, and a pass without an absolute hop path, the path inspector and the caller's environment. Scheduling: `AssignReadyTasks` refuses non-absolute roots, roots other than the scripted run's frozen ones (`frozenRoots`) and every run state but running (`runState`/`setRunState`, which the unscripted `DriveCompletion` also reports), and the fully-faked `deps` (fixed clock, sleepless waits, recording exec seams) |
| [grammarcontract_fixture_test.go](grammarcontract_fixture_test.go) | `TestMain`, `buildHopBinary`, `buildInto`, `goBuildVars`, `runHop`, `hopResult`, `herdrCanary`, `canaryBase`, `isolatedEnvironment`, `isolatedTestEnv`, `isolatedHopEnv`, `execHop`, `runFixtureGit`, `freshStateDir`, `realDir`, `testUUID`, `TestIsolatedEnvironment`, `TestCanaryBaseFitsTheSocketLimit` | The real-binary grammar contract suite's build/exec/isolation harness (design section 11, L1569): builds this package once (`go test`'s cwd IS cmd/hop's source dir, so `go build .` needs no module-root discovery), runs every subprocess — the hop binary, the one `go build`, the fixture git commands (`runFixtureGit`, bounded by `callTimeout`) — under `isolatedEnvironment`: an explicit allowlist (`PATH`, optionally behind a prefix, and `TMPDIR` from the host, plus the caller's named variables), a fresh `HOME`, and a `herdrCanary` unix-socket listener at `HERDR_SOCKET_PATH` that fails the suite if anything ever connects (bound in its own `os.MkdirTemp` directory under `canaryBase` — the process temporary directory when even the longest generated socket path fits the platform's sockaddr_un limit (103 bytes on darwin, 107 on linux), else the short fixed base `/tmp` — never `t.TempDir()`, so a long `TMPDIR` never breaks the bind); a caller can override none of those fixed keys, and nothing else is inherited. The build carries only `GOCACHE`/`GOMODCACHE`/`GOPATH` plus `GOTOOLCHAIN`/`GOFLAGS` when set, resolved in-process by `goBuildVars` (the test process's environment, else the go command's documented defaults) — nothing is executed to find them, so every subprocess, the build included, runs under a temporary `HOME` — and its cache stays warm. cmd/hop's only `TestMain` (Go allows one per package) |
| [grammarcontract_seed_test.go](grammarcontract_seed_test.go) | `openFixtureStore`, `freezeWorkflowSnapshot`, `featureWorkflowSnapshot`, `featureManager`, `newFeatureManager`, `newFeatureManagerAt`, `newFeatureManagerIn`, `addReworkTask`, `childSession`, `soloFixture`, `newSoloReserved`, `newSoloRunning`, `fixtureRowRevisions`, `TestHopfixturesLaunchBaseIsSingleUse` | Seeds fixture state through [internal/testsupport/hopfixtures](../../internal/testsupport/hopfixtures/AGENTS.md) — never through `hop run`, which would place a worker through Herdr — opening the real `internal/adapters/sqlite` Store directly (the same production call `compose.go` makes) and passing it to hopfixtures' string-typed API. `freezeWorkflowSnapshot` is the one raw `database/sql` write hopfixtures cannot perform itself (promotes a seeded run from solo to feature mode by writing JSON into `run_snapshots.workflow`; kept after 6b because the feature `InitializeRun` creates its own manager session and refuses the solo base hopfixtures seeds). `newFeatureManagerIn` seeds a SECOND run into an already-open fixture's own store — the only way to build a genuine cross-run scenario, since two separate state roots make a foreign session merely unknown there, not foreign |
| [grammarcontract_retirement_test.go](grammarcontract_retirement_test.go) | `retirementFixture`, `newRetirementFixture`, `retirementSetup`, `retirementCheckout`, `requireLines`, `TestGrammarContractWorktreeRetirement*` | The real-binary green boundary of worktree retirement. A real fixture repository (`main`, a `build/` ignore rule) gets real `git worktree add` checkouts, created and recorded through a symbolic link so the recorded spelling is never git's canonical one. An integration branch merges them one by one, and main is optionally fast-forwarded onto it. hopfixtures seeds a completed feature run, frozen with the target (or none), whose integrated rows carry the real pre-merge and merge commits and whose linked worktree rows and succeeded `worktree.create` operations name the checkouts; its lease is released. The built `hop status` runs under the Herdr canary. `journal` reads the run's operation count and lease generation through a raw read-only connection |
| [grammarcontract_retirement_paths_test.go](grammarcontract_retirement_paths_test.go) | `respellRecordedPath`, `symlinkedParentSpelling`, `TestGrammarContractWorktreeRetirementSymlinkParentSpelling`, `TestGrammarContractWorktreeRetirementDanglingLinkSpelling`, `TestGrammarContractWorktreeRetirementUnreadableRegisteredSpelling` | Retirement of checkouts whose spellings only the filesystem resolves, through the built binary. `respellRecordedPath` rewrites the first checkout's row path and its `worktree.create` outcome path together through a raw connection, so provenance still agrees; `symlinkedParentSpelling` builds `<dir>/link/../<checkout>`, which resolves to the checkout while its collapse names nothing |
| [grammarcontract_retirement_capture_test.go](grammarcontract_retirement_capture_test.go) | `retirementCaptureBytes`, `retirementListingBytes`, `fixtureGitBytes`, `writeIndexFiller`, `lockSiblingToFill`, `TestGrammarContractWorktreeRetirementLargeIndex`, `TestGrammarContractWorktreeRetirementLargeListing` | Retirement against git output larger than the process runner's default per-stream capture bound, through the built binary. `fixtureGitBytes` returns a fixture git command's exact stdout; `writeIndexFiller` writes clean files whose `ls-files -v -z` entries total an exact byte count, so a test places a hidden flag's entry at a chosen offset. `lockSiblingToFill` writes a sibling checkout's lock reason to its administrative `locked` file, placing the candidate's listing record at a chosen offset |
| [grammarcontract_launching_test.go](grammarcontract_launching_test.go) | `launchingFeature`, `newLaunchingFeature`, `TestGrammarContractManagerVerbsWhileLaunching` | LAUNCH-1's real-binary proof: a feature run started through hopfixtures' `InitializeFeature`/`LaunchManager` with the manager's claim still `exec_pending` (the run launching). `hop task create`, `hop plan close` and `hop msg send` print `app.GrammarTransientRunNotRunningLine` alone, the value-free detail on stderr, exit 1, and change nothing but a transient receipt; after `SettleManagerLaunch` the same invocations (same `--request-id`) print their success lines, and a retry prints the duplicate line. A read-only raw query pins the run, manager and claim states and the receipt outcomes |
| [grammarcontract_msg_test.go](grammarcontract_msg_test.go), [grammarcontract_answer_test.go](grammarcontract_answer_test.go), [grammarcontract_transient_test.go](grammarcontract_transient_test.go), [grammarcontract_plan_test.go](grammarcontract_plan_test.go), [grammarcontract_review_test.go](grammarcontract_review_test.go), [grammarcontract_view_test.go](grammarcontract_view_test.go), [grammarcontract_phase2_test.go](grammarcontract_phase2_test.go) | `TestGrammarContract*` | The real-binary grammar contract tests themselves, one file per verb-family group: msg send/next/wait/ack/show + answer (the hostile-body proof checks both streams of every producing and consuming invocation for exact permitted output, an empty stderr and independent canaries — ESC, both bracketed-paste markers, the SGR sequence, a body fragment); the answer refusals (`requireRefusedOnly`: exactly `refused: unauthorized` for a worker or the manager answering the human relay — the relay left unacknowledged and the human's `hop answer` then sent — and for another task's worker, and exactly `refused: mailbox-closed` for the manager's answer after the reviewer's accepted verdict closed its mailbox); the submit verbs' typed retry lines (`addActiveChild` seeds an active implement or review task whose attempt is running or, through `hopfixtures.LaunchAttempt`, still launching: a message queued to the task makes `hop result submit` print exactly the drain line and the resubmission after `msg next`/`msg ack` is accepted, a launching attempt prints exactly the not-running line for `hop result submit` and `hop review submit` alike, the latter over an empty mailbox); task create/retry + plan close; review submit (`initFixtureGitRepo`/`commitFixtureChange`/`gitRevParseTree` build an isolated real git repository, since `SubmitReviewVerdict` resolves `--subject`'s tree object id via a real `git rev-parse` against the run's recorded repository root); view set/clear; the five Phase 2 verbs never covered before (`status`/`stop`/`resume`/`launch`/`result submit`) — `launchEnv` builds a solo launch fixture's pane environment, `hopfixtures.LaunchBase` plus a deliberately mismatched or conflicting value constructs each refusal-before-exec shape without ever letting `hop launch` reach `d.exec`, and every `hop launch` invocation runs through `execLaunch`: a `launchExecCanary` executable under the configured harness name (`claude`) sits first on the launch's PATH and only creates a marker, which each test asserts never exists (`TestGrammarContractLaunchExecCanaryIsLive` proves the lookup resolves to it and that running it leaves the marker). Every test asserts the observed first line against an `internal/app/grammar.go` constant (or documents why a shape is unreachable without a live Herdr connection) — `TestGoldenGrammar` (`internal/app`) already pins that a constant's literal text is right; this suite answers only "does cmd/hop actually render it" |
| [grammarcontract_authority_test.go](grammarcontract_authority_test.go) | `TestGrammarContractResultSubmitSupersededIncarnationIsStale` | Submission and messaging authority through the built binary. A worker whose binding was superseded with no successor (`hopfixtures.SupersedeBinding`), with a manager info queued to its task, prints exactly one `stale: …` line from `hop result submit` and exits 1, on the first submission and on a retry — never a retry line — and its own `hop msg next` is refused on stderr with exit 1 |
| [grammarcontract_status_test.go](grammarcontract_status_test.go) | `TestGrammarContractStatusFeatureDetailSections`, `TestGrammarContractStatusVerdictFollowsTheIntegratedHead`, `TestGrammarContractStatusHostileStateRootNeverForgesALine`, `TestGrammarContractStatusPreBindingClaim`, `integrateHead`, `statusDetail`, `reviewReasonsPath` | Drives `hop status -run`'s STATUS-1 feature-mode detail block through the built binary against a real git repository, whose tree ids differ from its commit ids. `integrateHead` seeds an integrated task whose merge is a real commit (`hopfixtures.SeedIntegratedTask`). The sections test: a real `hop review submit --verdict reject` of the integrated head (the full `verdict-rejected` line with its review id, subject and reasons path), a seeded implement task (task-not-integrated), a real `hop msg send` left queued (the attention line), and a pending human question with its exact `hop answer` invocation. The verdict test: the reject's reasons path equals the body path `hop msg next` serves the manager for the controller notice; after a fix integrates a new real commit the same review renders `verdict-stale-subject` and no rejection; a real approve of the integrated head renders no verdict shortfall. The hostile-state-root test renders a pending question's body path, a retained check's artifact and evidence paths (`hopfixtures.SeedCheckEvidence`) and its detail naming the root, under a root containing ESC and newline bytes, each quoted, with no raw control byte and no forged line. The pre-binding-claim test renders a solo worker's claim written before its binding (`hopfixtures.SeedLaunchClaimWithSeedEvidence` after `LaunchBase`): `binding: (none)`, `launch claim: exec_pending`, and hostile trust-seed evidence quoted, never raw. Crossing the attention threshold to reach the nested action line and the `blocked, needs attention` marker is left to the in-process unit tests (`cmd/hop`'s `TestRunStatusFeatureDetailBlock`, `internal/app`'s `TestStatusMailboxesAndAttention`) since this suite has no seam to fast-forward the exec'd binary's own clock |

The repository root's [herdr-plugin.toml](../../herdr-plugin.toml) declares
how Herdr invokes this binary: a startup hook and a `show-context` action run
`plugin-context`, a `doctor` action and a pane entrypoint run `doctor`. Its
commands expect the built binary at `.bin/hop` (see `make build`).

## Invariants

- A store-open failure is rendered only through
  `describeStoreOpenFailure`: a classified, value-free line naming where
  the root came from (`HOP_STATE_DIR` for worker commands, the resolved
  state root for controller commands) — the raw sqlite/filesystem error
  chain carries the complete path and is never printed.
- An operator-supplied file a worker-plumbing verb reads (`--file`,
  `--reasons-file`) that cannot be read renders `pathErrorCategory`'s fixed
  category, never the path; artifact-write failures reach stderr already
  path-free from the artifact store (`internal/adapters/system`).
- Exit codes: 0 success, 1 failure, 2 usage. `hop run`/`hop resume` exit 0
  only on a `completed` run; failed, stopped, detach and errors exit 1; a
  StartRun or StartFeatureRun refusal before any side effect
  (`app.ErrStartRefused`), and an unknown `--workflow`, are usage.
  `hop stop` exits 0 only when `stopped` was observed. `hop status` exits 0
  on rendering success — state is data. `hop result submit` exits 0 for
  accepted and duplicate only. The exec boundaries exit 1 on every failure
  with one stderr line that never echoes an environment value; on success
  they never return. A write failure takes precedence, including flag
  diagnostics through `recordingWriter`. Every worker-plumbing verb
  (`hop task/plan/msg/answer/review`, design section 7) shares one
  convention through `writeLinesAndExit`: exit 0 unless the rendered first
  line begins `refused: ` (`app.GrammarRefusalPrefix`) or `transient: `
  (`app.GrammarTransientPrefix`), which always exit 1 — neither is a usage
  error, since the CLI syntax was fine and the request itself was refused
  or must be rerun. `hop task create/retry`, `hop plan close` and
  `hop msg send` print the retryable run-not-running line while the run
  is not yet running, with the store's value-free detail on stderr. `hop review submit`'s transient outcome
  is the one exception, rendered by its own `writeReviewResultAndExit`
  (also exit 1, the fixed retry line only, Detail to stderr).
- One state-root rule (section 4): controller commands resolve
  XDG-with-`HOP_STATE_DIR`-override through `resolveStateRoot`; worker
  commands (`launch`, `check-exec`, `result submit`, and every
  `hop task/plan/msg/review` worker-plumbing verb) require the provided
  absolute `HOP_STATE_DIR` and never fall back. `hop answer` is the one
  worker-plumbing-shaped verb that is NOT a worker context (no `HOP_*` env
  at all) and resolves the CONTROLLER state-root rule instead, exactly
  like `hop status`/`hop stop`/`hop resume`. The SQLite adapter receives
  the absolute root and creates it 0700.
- Adapters are constructed only here, once per command; the Herdr runtime,
  `Workspaces` and `Presentation` are wired only for commands that act on
  panes/worktrees or the native Agents view (`run`, `stop`, `resume`,
  `view set`, `view clear`). `hop status`, the exec boundaries,
  `result submit` and every `hop task/plan/msg/answer/review`
  worker-plumbing verb run store-only with a nil `Runtime`. `Messages`,
  `Plan` and `Reviews` (the same SQLite store) are wired for every command
  unconditionally: the worker-plumbing verbs they back are store-only,
  like `result submit`, and never need a Runtime. `hop view set/clear`
  wiring `withRuntime: true` does NOT mean its store-only refusal shapes
  touch Herdr — `SelectRunView`/`ClearRunView` are the only calls that
  dial the Presentation port, and `resolveRunArg`/`runLabel` run and can
  fail before either is ever called.
- `openController` resolves `Controller.GitExecutable` before opening the
  store, for every command: `lookupExecutable("git", os.Getenv("PATH"))`
  against the controller process's own environment. A bare `"git"` is
  never passed to `app`'s git invocations — `CommandRunner.Run` requires an
  absolute argv[0]. Composition fails with a fixed, value-free diagnostic
  ("git executable not found on PATH") when it cannot be resolved.
- SIGINT/SIGTERM on a foreground controller detach — journal, cancel,
  release, print the resume instruction — and never stop the run; a second
  signal exits immediately. Only `hop stop` stops a run.
- The loop never sleeps outside the injected `wait`, heartbeats run
  concurrently so a long check cannot outlive the lease TTL, and every
  exit path releases the lease (best-effort on error paths).
- `hop check-exec` execs the frozen check argv byte-for-byte through
  `ExecResolved` (the running argv is what group retirement matches); only
  the executed path is resolved. `hop launch` execs an absolute argv[0]
  that equals the claim's recorded executable.
- Commands stay thin: environment reading, flag parsing and rendering live
  here; decisions live in [internal/app](../../internal/app/AGENTS.md).
- No CLI framework and no third-party dependency.

## Dependencies and ports

- Allowed inward imports: [internal/app](../../internal/app/AGENTS.md),
  [internal/adapters/config](../../internal/adapters/config/AGENTS.md),
  [internal/adapters/herdr](../../internal/adapters/herdr/AGENTS.md),
  [internal/adapters/process](../../internal/adapters/process/AGENTS.md),
  [internal/adapters/sqlite](../../internal/adapters/sqlite/AGENTS.md),
  [internal/adapters/system](../../internal/adapters/system/AGENTS.md), per
  rule `cmd/hop` in [internal/arch_test.go](../../internal/arch_test.go).
  `_test.go` files additionally import
  [internal/testsupport/hopfixtures](../../internal/testsupport/hopfixtures/AGENTS.md)
  (the checker's `testFirstParty` list, not the production import list —
  production cmd/hop code never imports it, and never imports
  `internal/domain/identity` or `internal/domain/run` even from a test
  file; hopfixtures exists precisely so the grammar contract tests never
  need to).
- Consumed/implemented ports: wires the adapters into every `app.Controller`
  port; implements `app.ExecutableLookup` (`lookupExecutable`); constructs
  `herdr.InstallationProbe` for `app.Doctor`.
- External libraries: none; standard library only.

## Verification

- `go test ./cmd/hop` — table-driven command behavior against the scripted
  `controllerAPI` fake and fully-faked `deps` (no real adapter is opened):
  dispatch and exit codes, the feature loop printing unresolved attempt
  launches and continuing (`TestFeatureLoopKeepsRunningOverUnresolvedLaunches`),
  the feature loop running on past a vanished pane the presentation round
  skipped (`TestFeatureLoopContinuesPastAVanishedPane`),
  `hop run --workflow` dispatch and exit codes
  for both workflows (`TestRunRunWorkflowDispatch`), a feature run
  started from a repository subdirectory scheduled in the root it froze
  (`TestRunFeatureFromRepositorySubdirectory`), state-root
  resolution (default, override,
  relative rejection) and the doctor line, the controller loop with a fake
  clock (transitions, stop routing, detach on signal, heartbeat-failure
  exit), hop run's refusal/failure/detach paths and flag surface, resume
  outcome routing (a resumed feature run whose child is
  launch-suppressed enters the loop and reaches failed through the
  retirement pass, never a stop; one whose child launch is in flight
  renders its pending line and enters the loop, whose corroboration
  settles it), stop's drive/observe/deadline paths, status filtering
  and detail rendering — including STATUS-1's feature-mode block: the task
  table, latest integration, guard shortfalls, per-mailbox attention lines
  (with and without a known live-session binding, with and without
  Attention itself), a pending question's `hop answer` invocation, the
  per-session listing and the `blocked, needs attention` marker
  (`TestRunStatusFeatureDetailBlock`, `TestRunStatusFeatureDetailEmptyMailboxes`),
  every externally sourced detail field rendered quoted one hostile value
  at a time, with no raw control byte, no forged line and the benign line
  count (`TestRunStatusDetailEscapesEveryExternalField`), and the whole solo
  and feature headers byte for byte for ordinary values
  (`TestRunStatusDetailOrdinaryValuesRenderRaw`) —
  result submit's outcome and exit-code contract, and both exec boundaries
  (state-root requirement, prepared request shape, recorded exec argv/env,
  exec_failed settlement after a failed exec, no exec on refusal).
- `go test ./cmd/hop -run 'TestVerbsRenderTransientRunNotRunning|TestWriteLinesAndExitCodes'` —
  the retryable run-not-running shape of `hop task create`,
  `hop task retry`, `hop plan close` and `hop msg send` (the line alone on
  stdout, `<verb>: <detail>` on stderr, exit 1) and the exit mapping by
  first line (success 0; a refusal or any transient line 1).
- `go test -race -shuffle=on ./cmd/hop` — the loop's concurrent heartbeat
  goroutine under the race detector.
- `go test ./cmd/hop -run 'TestStatusRunsWorktreeRetirement|TestRunAndResumeRunWorktreeRetirementFirst|TestWorktreeDetailLines'` —
  the retirement wiring against the scripted fake:
  - **hop status, both forms:** triage runs for the resolved repository
    before rendering. Each run with work is acquired, passed and
    released, in order. A held lease is deferred; history-missing,
    blocked and not-merged runs follow their rules; a pass error prints
    one value-free line, with the lease still released and a path canary
    on neither stream. The announcement comes first, and section 6's
    golden lines follow the rendering. Exit is 0, and the runtime is
    never wired.
  - **Status failure shapes:** a triage failure is one line and the
    listing still renders. A first signal cancels a running pass, which
    still releases and prints its unresolved line. An unlocatable hop
    binary skips the pass.
  - **Controller starts:** hop run passes between `ResolveRunWorkflow`
    and `StartRun`, printing its lines first, and a usage-refused run
    passes over nothing. hop resume passes excluding its own resolved run
    before `Resume`.
  - **Golden wording:** the per-row detail lines, every category, reason
    and disposition, and a feature run without rows rendering no
    worktree line. The `inspection failed` action says the checkout could
    not be fully inspected.
- `storeopen_test.go` drives the real `openController` (not the scripted
  fake) directly: `TestStoreOpenDiagnosticsNeverEchoTheRoot` proves every
  worker and controller command classifies a store-open failure without
  ever echoing the state-root value, and `TestOpenControllerGitExecutableNotFound`
  proves composition refuses with a fixed, value-free diagnostic when git
  cannot be resolved on its own PATH.
- `pathinspect_test.go` (`TestInspectCanonicalPath`,
  `TestInspectCanonicalPathRefusesUnresolvablePaths`) drives the
  retirement path seam against the real filesystem under `t.TempDir`
  (`pathLayout`):
  - existing paths, followed through a symlink, resolve fully, and every
    existing answer equals `EvalSymlinks` of the spelling;
  - a missing checkout, one level or several deep, resolves through its
    deepest existing prefix;
  - an uncleaned spelling resolves component by component;
  - `..` after a symbolic link (absolute or relative) leaves the link's
    target: an existing checkout, a missing leaf, several missing levels
    and a dangling leaf behind `<dir>/link/..` resolve under the target's
    parent, never under `<dir>`;
  - repeated separators, a trailing `.` and `..` above `/` resolve;
  - a dangling link leaf exists and is not followed;
  - a relative path is refused;
  - refused with the value-free `errPathUnresolvable`, never answered
    absent: a `..` after a missing component (whose collapse names an
    existing directory), a dangling link passed through or followed by
    `..`, a link loop, a regular file used as a directory, and a trailing
    separator on a dangling leaf.
- `fileerrors_test.go` proves the same value-free rule for file reads
  and artifact writes: `TestFileReadFailuresNeverEchoThePath` (every
  `--file`/`--reasons-file` verb, missing and unreadable, a path canary on
  neither stream, no store opened) and
  `TestArtifactWriteFailureNeverEchoesThePath` (a real artifact-store
  failure rendered by `hop msg send`); the grammar contract suite's
  `TestGrammarContractMsgSendArtifactWriteFailureEchoesNoPath` repeats it
  against the real binary.
- `make build && .bin/hop version` — builds the binary and prints the version
  the Go toolchain derived from version control.
- `.bin/hop doctor` — probes the real installation on this machine.
- `go test ./cmd/hop -run TestGrammarContract` — the real-binary grammar
  contract suite (design section 11, L1569;
  `grammarcontract_*_test.go`): builds the actual `hop` binary once and
  execs every verb this slice owns, plus the five Phase 2 verbs that
  predate it, against a fixture state root seeded through
  [internal/testsupport/hopfixtures](../../internal/testsupport/hopfixtures/AGENTS.md)
  over the REAL `internal/adapters/sqlite` Store — real-adapter, but
  never real-Herdr: every exec runs under `isolatedHopEnv`'s
  `herdrCanary`, which fails the test if anything ever connects to
  `HERDR_SOCKET_PATH`. This is a third tier distinct from both the
  scripted-fake unit tests above (no real adapter) and
  `test/integration` below (real adapter AND a real Herdr server); it
  lives entirely under `cmd/hop`, never under `test/integration`, and
  must never start a Herdr server itself.
- `go test ./cmd/hop -run TestGrammarContractWorktreeRetirement` — the
  worktree-retirement green boundary through the real binary and real
  git:
  - **Merged:** both checkouts are removed, ignored build output
    included, with a repository-local fsmonitor hook that never runs and
    the branches kept, git's list empty, section 6's lines
    printed and the fact rendered by `-run` with per-row lines. A second
    `hop status` prints only the listing, journals nothing and takes no
    lease.
  - **Unmerged:** after its one check, repeated calls print nothing,
    journal nothing and take no lease, and the checkouts stay.
  - **Retained:** a dirty checkout and an assume-unchanged hidden change
    are retained with their actions while a clean sibling is removed.
    Once both are cleaned, the next call removes them and sets the fact.
  - **Detached checkout:** it is released and left on disk, and its
    sibling is removed.
  - **Detached-HEAD target:** the run is never acted on.
  - **Moved repository:** with an empty directory or a different git
    repository now at the root, the run is skipped with no lease, no
    operation and nothing removed.
  - **Large index** (`grammarcontract_retirement_capture_test.go`): a
    checkout whose `ls-files -v -z` output outgrows the default capture
    bound, with an assume-unchanged or skip-worktree change in the entry
    right where a default-bounded read would end (or past it), is read in
    full and retained as `hidden changes` with its action. The hidden
    change survives, git still lists the checkout, and the run is not
    retired.
  - **Large listing:** a sibling checkout locked with a reason sized so
    the worktree listing reaches a bound exactly at the record boundary
    before the candidate. At the default bound the listing is read in
    full and the candidate is removed. At the listing's own 64 MiB bound
    the candidate is retained as `inspection failed`, never released or
    recorded absent. The sibling and its lock are untouched.
  - **Symlinked-parent spelling**
    (`grammarcontract_retirement_paths_test.go`): a checkout recorded as
    `<dir>/link/../<checkout>` is removed when it exists. When it was
    deleted while git still lists it, git's entry is pruned and the row
    settles absent.
  - **Dangling-link spelling:** a checkout recorded through a link that
    no longer resolves is retained as `inspection failed`, never
    recorded absent, and stays on disk.
  - **Unreadable registered spelling:** the checkout's parent is
    relocated behind a symbolic link through a gate directory, so git's
    registered spelling differs from the current canonical path. With the
    gate unreadable, the checkout is retained as `inspection failed`,
    never released as unregistered. With the gate readable again, the
    same spelling resolves to the checkout, which is removed. The test
    skips, with a reason, where permissions do not deny access.

  Recording the checkouts through a symbolic link makes the path
  canonicalization load-bearing: an inspector that skips it would
  release the checkouts instead of removing them.

  Each case fails against a weaker implementation:
  - Against the code before the truncation report, the large-index cases
    remove the checkout with its hidden change, and a listing cut on a
    record boundary releases the checkout as unregistered.
  - With the reads' raised bound dropped, the large-index cases and the
    default-bound listing are retained as uninspectable instead of being
    decided in full.
  - Against the inspector that collapsed spellings first:
    - the symlinked-parent checkout is recorded absent while it still
      exists;
    - the deleted one stays in git's list;
    - the dangling-link checkout is recorded absent.
  - Against the listing lookup that skipped an unresolvable record whose
    spelling differed from the canonical path, the unreadable registered
    spelling releases the checkout as unregistered.
- Real-Herdr behavior (a live server, real panes, real worktrees) is
  exercised only by the `test/integration` suite (task 6b), never by
  anything in this package.
- Test fixtures: stub shell scripts written to temporary directories for
  the scripted-fake unit tests above; the grammar contract suite's own
  fixtures are real SQLite state roots built through hopfixtures. No
  live Herdr session is ever contacted by anything in this package.

## Related guides

- [Parent index](../AGENTS.md)
- [Root guide](../../AGENTS.md)
- [Application layer](../../internal/app/AGENTS.md)
- [Herdr adapter](../../internal/adapters/herdr/AGENTS.md)
- [SQLite store](../../internal/adapters/sqlite/AGENTS.md)
- [hopfixtures test-support package](../../internal/testsupport/hopfixtures/AGENTS.md):
  the grammar contract suite's fixture seeder — `_test.go` files only.
- [Process adapter](../../internal/adapters/process/AGENTS.md)
- [System adapter](../../internal/adapters/system/AGENTS.md)
- [Config adapter](../../internal/adapters/config/AGENTS.md)
