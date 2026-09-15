# test/integration

## Purpose

The real-process Herdr suite: proves HOP's plugin and protocol behavior
against a real, disposable `herdr server` rather than a fake. Each test
spawns its own named-session server on temporary config, state and runtime
roots with a test-only config file, links the staged HOP plugin built from
this tree, and drives everything through HOP's own protocol client.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [harness_test.go](harness_test.go) | `requireHerdr`, `prepareServer`, `testServer`, `serverProcess`, `start`, `startAnchoredLeader`, `restart`, `reapServer`, `stagePlugin`, `newArtifactDir`, `artifactDir.dir`, `registrySnapshot`, `assertNoRegistryLeak`, `waitForLog`, `waitUntil`, `killProcessGroupThenReap`, `safeToSignalGroup`, `call` | Disposable server lifecycle (spawn, bounded socket-readiness wait, API `server.stop` then a process-group `SIGKILL` and reap of the leader), a `restart` that gracefully stops and reaps the current server before relaunching on the same roots so persisted session state survives (S3), per-`serverProcess` at-most-once reaping, `startAnchoredLeader` (the shared start-as-group-leader-and-anchor step every owned leader in this suite uses — the server, spike fixture leaders and task 6b's hop subcommands), hermetic subprocess environment built from scratch (`shell` selects the pane login shell), plugin staging (`go build` + manifest copy), `artifactDir.dir` for evidence that is a directory tree rather than one file (a fixture repository, a built binary), failure-retained evidence, user-global registry leak checks and bounded polling |
| [reap_test.go](reap_test.go) | `TestSafeToSignalGroup`, `TestKillProcessGroupThenReapKillsDescendants`, `processExists` | Table-driven guard against signaling an unsafe process group (non-positive, init, or the caller's own group), and a fixture leader + backgrounded grandchild proving the whole group is torn down and the leader reaped |
| [plugin_test.go](plugin_test.go) | `TestRealProcessPluginLifecycle`, `TestRealProcessStartupHookRunsOnServerStart` | Link → action list → invoke → completed log records with real output → workspace/pane open and rendered evidence → unlink; and the startup hook proven to run on server start, not on link |
| [presentation_test.go](presentation_test.go) | `TestRealProcessAgentPresentationAndView`, `createRunFixture` | A deterministic manager+workers fixture (custom agent identity/state via `pane.report_agent`), HOP metadata published through the presentation adapter, tokens round-tripped through `agent.list`, manager-first ordering, HOP's owned view select/clear, and another owner's view surviving HOP's owned clear |
| [observation_test.go](observation_test.go) | `TestRealProcessEventObservationReconcile`, `TestRealProcessContextInjection` | Subscribe-before-snapshot reconciliation over a real status transition, and context injected into a live pane through `pane.send_text` |
| [launchenv_test.go](launchenv_test.go) | `TestRealProcessLaunchEnvironment`, `TestRealProcessLaunchEnvironmentIsAdditive` | The launch-environment compatibility results: workspace/tab/pane creation carry an explicit env to the launched shell, and the env map is additive — a Herdr-managed variable is authoritative and cannot be overridden. See [launch-environment.md](../../docs/architecture/launch-environment.md) |
| [pty_test.go](pty_test.go) | `TestRealProcessPTYRendering`, `ptyClient` | A PTY-attached client at fixed 100x30 dimensions capturing how the native Agents sidebar renders HOP's projection: the fixture agents drawn, manager-first order on screen, and a HOP metadata token in the rows |
| [artifacts_test.go](artifacts_test.go) | `TestRealProcessArtifactDirRemovedOnPassingRun`, `TestArtifactDirRemovedOnPassingRun`, `TestArtifactRetentionDecision` | A real server run with an open subscription proving the process-group teardown leaves the artifact directory empty, plus harness self-tests for the evidence-retention contract that need no herdr binary |
| [spike_fixture_test.go](spike_fixture_test.go) | `buildSpikeFixtures`, `spikeFixtures`, `spikeFixtureSource`, `newSpikeUUID`, `processInfo`, `spikeProcessInfo`, `agentRecords`, `waitForAgent`, `assertNeverListedAsAgent`, `waitForShellReady`, `waitForAgentStart` | Shared Phase 2 capability-spike fixtures: one Go program built and installed under a launcher-stand-in name (execve to a harness) and under a detection-recognized `claude` name and an unrecognized name; `pane.process_info`/`agent.list` readers and bounded agent-detection polling; `waitForShellReady` polls for the pane shell being the sole idle foreground process (Herdr's `agent.start` precondition), and `waitForAgentStart` retries `agent.start` itself through `waitUntil` on `agent_pane_busy` since that inspection is not atomic with the call — the busy rejection runs before any state mutation, so retrying is always safe |
| [spike_launch_test.go](spike_launch_test.go) | `TestSpikeLaunchLineDetection`, `TestSpikeUnrecognizedProcessNameIsNotDetected`, `createSpikeWorkerPane` | S1: a `send_text` `exec <launcher> … -- <harness>` line launches despite noisy `/bin/sh` and zsh login-shell startup (zsh skipped with a reason when absent), the execve'd recognized process is detected as an agent (an unrecognized name is not), the additive env survives both execs, and the exec'd process's pid is asserted equal to `shell_pid` and the foreground process group |
| [spike_harness_test.go](spike_harness_test.go) | `TestSpikeRestartBarrier`, `TestSpikeGroupAnchor`, `TestSpikeOwnedGroupTeardown`, `TestSpikeOwnedGroupDrainDeadline`, `startFixtureLeader` | Harness self-tests, no herdr binary: the S3 restart barrier waits for the leader's actual exit (release-gated save survives) and force-kills when it never exits; the group anchor pins the pgid after the leader is reaped and a leader that already exited cannot be anchored (inconclusive); the live-probe owned-group lifecycle retires surviving children on both the deadline and normal-exit paths; and the bounded output drain fires its deadline and closes the reader when a write descriptor is held open outside the group |
| [spike_identity_test.go](spike_identity_test.go) | `TestSpikePaneProcessIdentity`, `waitForForegroundProcess` | S2: the exact `pane.process_info` identity fields (pid, name, argv0, full argv, cmdline, cwd; empty `tty` on macOS; no start time), and a same-kind occupant replacement distinguished by pid + argv with a stable `shell_pid` |
| [spike_restore_test.go](spike_restore_test.go) | `TestSpikeRestorePlainPaneLosesAdditiveEnv`, `TestSpikeRestoreAutoRelaunchBypassesLauncher`, `snapshotDump`, `snapshotPaneIDForLabel` | S3: a graceful restart restores a pane's label but not its creation-time additive env; a recorded native session makes Herdr auto-relaunch via `claude --resume` (deferred until client geometry), bypassing the launcher and dropping `HOP_*`; the delayed-restore window shows a phantom idle agent in `session.snapshot` |
| [spike_claude_session_test.go](spike_claude_session_test.go) | `TestSpikeClaudePreassignedSessionID`, `requireClaude`, `runClaude`, `findTranscript`, `forbiddenClaudeEnv` | S4: against the installed claude in print mode, in a scratch `CLAUDE_CONFIG_DIR` with a credential-free environment, `--session-id <uuid>` is accepted and creates the transcript under that UUID, `--resume <uuid>` finds it (unknown → not found); the only test that runs a real harness, so it is opt-in (`HOP_LIVE_HARNESS=1`, else skipped with a reason) and also skips when no claude binary is installed |
| [spike_agentstart_test.go](spike_agentstart_test.go) | `TestSpikeAgentStartArgvCapability` | S5: `agent.start` rejects an unrecognized kind and control-char args and composes argv as `[executable(kind)] + args`, so no wrapper can be interposed; and it types a bare name whose resolution the pane shell's PATH (`path_helper`) decides |
| [spike_layout_test.go](spike_layout_test.go) | `TestSpikeLayoutApplyCommandPane`, `TestSpikeLayoutApplyAddsTabToExistingWorkspace`, `snapshotPaneByLabel`, `snapshotIDs`, `paneExists`, `drainEventNames` | S6: `layout.apply` pane nodes carry `command`+`env`+`label` and run the argv as the pane process (asserted, at the requested cwd) with no shell; an additive apply (`workspace_id` only) adds one tab and every pre-existing tab/pane id survives (+1 tab, +1 pane); a non-zero command exit closes the pane and carries no exit status in `pane.exited` |
| [spike_markers_test.go](spike_markers_test.go) | `TestSpikeCreationMarkerTabLabel` | S7: a `tab.create` creation-time `label` round-trips through `tab.list` and `session.snapshot`, letting a crashed controller recover its own tab and root pane by a unique marker without the create response; the additive env is never a snapshot field |
| [hopcmd_test.go](hopcmd_test.go) | `TestMain`, `buildHopBinary`, `hopResult`, `runHop`, `requireHopCommand`, `hopEnviron`, `startHopController`, `groupMember`, `listGroupMembers`, `parseGroupMember`, `TestBuildAndRunHopBinary`, `TestHopEnviron`, `TestStartHopController`, `TestListGroupMembers` | Task 6b harness extensions (phase A, item 4): `TestMain` + `buildHopBinary` build `./cmd/hop` once per test binary run into a shared temp dir instead of per test; `runHop` runs a bounded one-shot hop subcommand capturing stdout/stderr/exit code, and `requireHopCommand` skips a scenario with a clear reason when its command is cmd/hop's "unknown command" response (task 6a not yet landed/merged); `hopEnviron` builds the isolated `HOP_STATE_DIR` + this disposable server's own socket/binary path on top of the suite's hermetic base; `startHopController` starts a long-running hop subcommand (`hop run`/`hop resume`) as its own anchored, logged process group via `startAnchoredLeader`, returned as a `serverProcess` so existing group helpers apply unchanged; `listGroupMembers`/`parseGroupMember` independently list an arbitrary process group's members via `ps` — deliberately NOT importing `internal/adapters/process` (disallowed by the architecture checker for this package), so a scenario proving group retirement is evidence against the OS process table, not an echo of the port under test |
| [fixturerepo_test.go](fixturerepo_test.go) | `fixtureRepo`, `newFixtureRepo`, `newFixtureRepoGitDependentCheck`, `newFixtureRepoWithSubmodule`, `initFixtureRepo`, `CommitCheckResult`, `fixtureConfigTOML`, `fixtureGitEnviron`, `runShellScript`, `TestFixtureRepoDeterministicCheck`, `TestFixtureRepoGitDependentCheck`, `TestFixtureRepoSubmoduleFailsClearly` | Task 6b fixture repository builder (phase A, item 2): a temporary SHA-1 git repository (`git init --object-format=sha1`, isolated from any developer git config, its own local commit identity) with a trivial source file, the deterministic `check.sh` (`CommitCheckResult` toggles CHECK_RESULT and commits, so the caller picks pass/fail "on demand" by choosing which commit it submits), the git-dependent `check-git.sh` variant (`newFixtureRepoGitDependentCheck`, succeeds only inside a real git checkout), and the submodule variant (`newFixtureRepoWithSubmodule`) whose `check-submodule.sh` fails clearly against a detached `git worktree add` checkout that never initializes submodules |
| [fixtureworker_test.go](fixtureworker_test.go) | `fixtureWorkerSource`, `fixtureWorkerBrief`, `buildFixtureWorker`, `testAssignmentPrompt`, `writeTransientOnceHopStub`, `isResumeInvocation`, `TestFixtureWorkerSubmitValid`, `TestFixtureWorkerExitWithoutSubmitting` | Task 6b fixture worker (phase A, item 3): a Go program built (never installed) and installed under the recognized name `claude` so `hop launch`'s PATH resolution finds it; reads required `HOP_*` env, locates and reads the assignment artifact (computed from `HOP_STATE_DIR`/`HOP_RUN_ID`, cross-validated against a best-effort marker parsed from its own prompt argv), and dispatches on a `FIXTURE-BEHAVIOR: <name>` directive delivered through the brief -> assignment.md channel (`fixtureWorkerBrief` renders it): `submit-valid`, `submit-stale` (gated on a `FIXTURE-GO` line on stdin, for a test to retire the incarnation first), `submit-twice`, `exit-without-submitting`, `exec-keep-pid` (`syscall.Exec` of itself, proving pid survives exec); every behavior writes an atomic `worker-observed.txt` dump beside the assignment file rather than judging its own correctness, and retries `hop result submit` on a `transient` first line. A cold-relaunch invocation (`--resume <native-ref>`, no prompt argv at all) is detected by `isResumeInvocation` and recovers the hop path from a `.hop-path` sibling file the first launch persisted, rather than parsing a prompt that does not exist (phase B: TestRealProcessConfirmAbsentColdRelaunchNonRestart's discovery) |
| [resultsubmit_test.go](resultsubmit_test.go) | `fixtureRun`, `newFixtureRunEnv`, `startRun`, `startFixtureRun`, `dbPath`, `taskAndAttemptIDs`, `currentIncarnationID`, `requireRunState`, `containsState`, `TestRealProcessDuplicateSubmissionAfterCompletion` | Phase B's shared setup: `newFixtureRunEnv` builds and starts a disposable server plus the fixture worker installed as `claude`; `startRun` runs `hop run` against a caller-built (and possibly pre-mutated) fixture repository through the `started` line; `startFixtureRun` composes both for the common case. `fixtureRun` tracks the live controller (`controller`/`controllerName`, reassigned together by any scenario that kills and replaces it) plus `dbPath`/`taskAndAttemptIDs`/`currentIncarnationID` helpers for direct `hop result submit`/sqlite assertions, and `requireRunState` (bounded to `runEndToEndTimeout`) fails with the worker pane's scrollback captured first. `TestRealProcessDuplicateSubmissionAfterCompletion` proves section 7 step 3: a fresh submission carrying the already-accepted commit and summary is accepted as `duplicate`, disturbing nothing |
| [lifecycle_test.go](lifecycle_test.go) | `leaseTimeLayout`, `killControllerLeader`, `waitForLeaseExpiry`, `TestRealProcessDetachDistinctFromStop` | `killControllerLeader` SIGKILLs a controller's leader without touching its anchor; `waitForLeaseExpiry` polls a run's own `run_leases` row (mirroring `internal/adapters/sqlite`'s unexported time layout, since this package may not import that adapter) until it is no longer held past its recorded expiry — the real, unavoidable cost every crash-recovery scenario in this package pays. `TestRealProcessDetachDistinctFromStop` proves a SIGINT to a live controller releases the lease at once (never waiting out the TTL) and disturbs neither run state nor the worker |
| [check_test.go](check_test.go) | `statusDetailValues`, `TestRealProcessFailedCheckRetainsArtifacts`, `TestRealProcessGitDependentCheckSucceedsInDetachedCheckout`, `TestRealProcessSubmoduleRepositoryFailsClearly` | `statusDetailValues` scrapes every occurrence of a repeated `hop status -run` line (`artifact:`/`evidence:`), since `parseStatusDetail`'s flat map keeps only the last. The three scenarios: a deterministic failing check ends the run `failed` with both stdout/stderr retained at their rendered paths; `newFixtureRepoGitDependentCheck`'s check-git.sh passes inside the real detached-checkout candidate isolation; `newFixtureRepoWithSubmodule`'s submodule reference is rejected by `internal/app`'s own gitlink scan BEFORE any checkout or check argv ever runs (no evidence retained — check-submodule.sh never executes) |
| [stop_test.go](stop_test.go) | `slowCheckScriptName`/`slowCheckScriptSource`, `withSlowCheck`, `TestRealProcessStopWithTerminationObserved`, `TestRealProcessStopInterruptsRunningCheckGroup` | `withSlowCheck` rewires a fixture repository's `[check]` command to a `sleep 20` script and commits it, giving a scenario a check execution long-lived enough to interrupt mid-flight (confirmed via a durable `check_exec_claims` row, no race). The two scenarios drive `hop stop` from a separate one-shot process (observing, since the live controller holds the lease and drives the stop itself): a plain running worker terminates cleanly to `stopped` with the worktree preserved; a running check group is interrupted by the loop's own context cancellation (CommandRunner kills the group), which DriveStop's retry then settles as an *unknown* outcome (not a distinct "interrupted" one — an empty group alone cannot distinguish "killed a moment ago" from any other cause), while task/attempt still move to `interrupted` and the run reaches `stopped` |
| [launch_test.go](launch_test.go) | `TestRealProcessDuplicateLaunchInvocation` | Design section 6's duplicate-launcher rule: caught while the attempt is still `launching`/`relaunching` (`PrepareLaunchExec` refuses on attempt state before it ever reaches the claim's pid check, so this is the only window the different-pid refusal is observable in), a second `hop launch` invocation for the same incarnation — a different pid by construction — is rejected, the claim is unchanged, and the original launch settles normally afterward |
| [execfailure_test.go](execfailure_test.go) | `installBrokenClaudeStub`, `TestRealProcessExecFailureSettlesExecFailed` | A deterministic (non-racy) exec failure: the resolved `claude` stub is a plain executable-bit-set text file with no shebang, which still passes `PrepareLaunchExec`'s regular-file check (so the claim is written) but fails the kernel's `execve` (ENOEXEC). The claim settles `exec_failed`, attempt/task/run all fail immediately (unrecoverable on a first launch, never a detour through `running`), and a `hop stop` against the already-failed run is a clean no-op reporting the run's real state |
| [forkingwrapper_test.go](forkingwrapper_test.go) | `forkingWrapperSource`, `buildForkingWrapper`, `TestRealProcessForkingWrapperAfterExec` | The "wrapper forks the real harness after exec" fail-closed topology (design section 6), confirmed empirically before being written as assertions: a wrapper execs preserving the launch claim's pid, forks the real fixture worker as a child sharing its own argv with `argv[0]` forced to the wrapper's own invocation name (a bare `select {}` here trips Go's own deadlock detector — the wrapper waits on the child instead), then Herdr's `pane.process_info` reports that CHILD as `foreground[0]` — a different pid than the claim despite matching executable identity and marker. `CorroborateSettlement` classifies this `SettlementForkingWrapper`: the claim stays `exec_pending`, and the controller reports "needs interaction" |
| [checkdeath_test.go](checkdeath_test.go) | `leaderExitCheckScriptName`/`Source`, `configWithTimeout`, `TestRealProcessCheckLeaderExitWithLiveChildren`, `TestRealProcessCheckDeathUnknownOutcomeRepeatable` | `configWithTimeout` renders a `[check]` config with an explicit timeout/`repeatable` (unlike `fixtureConfigTOML`'s fixed 30s/false). "Leader exit with live children": a check backgrounds a child and exits, so its stdout/stderr pipes never see EOF on their own — `CommandRunner`'s capture hangs until the (short, here) timeout force-kills the whole group, landing the operation `reconciling` with the timeout-driven cancellation named as evidence (the exact further auto-resolution latency to a settled outcome is not pinned here — a live controller's own recovery cadence for an already-reconciling operation was not established as fast/bounded within a reasonable test iteration). "Unknown outcome, repeatable": getting a genuinely unknown (not a definite failure) outcome needs the check's process group gone with **no live watcher** having captured what happened — directly `SIGKILL`ing a check under a live controller instead produces a definite `exit(-1)` failure — so the whole controller is crash-killed leaving an ordinary fast-passing check running unwatched; `hop resume` finds the group naturally empty with nothing recorded, settles it unknown, and (`check.repeatable=true`) requeues it automatically to a normal completion |
| [resume_test.go](resume_test.go) | `TestRealProcessControllerKillResumeWarmReattach`, `coldRelaunchAfterCrash`, `TestRealProcessConfirmAbsentColdRelaunchNonRestart`, `TestRealProcessStaleSubmissionFromRetiredIncarnation`, `TestRealProcessConfirmAbsentRefusedAfterServerRestart` | Design section 5's resume cases against real hard kills. Warm reattach: only the controller dies (worker/pane untouched); once the lease expires, `hop resume` reattaches to the same live occupant (same pid, same incarnation, no new session). `coldRelaunchAfterCrash` additionally SIGKILLs the worker itself (its no-shell command pane closes itself, S6, giving both required absence conjuncts) before killing the controller, then drives `hop resume --confirm-absent` to a cold relaunch — shared by the non-restart-litmus-test and stale-submission scenarios. `TestRealProcessConfirmAbsentRefusedAfterServerRestart` repeats the identical crash but restarts the herdr server first: continuity breaks, the attestation is still journaled, and the relaunch is refused |
| [livehop_test.go](livehop_test.go) | `requireLiveHarness`, `installRealClaudeStub`, `liveTranscriptExists`, `TestLiveClaudeDefaultProfileRun` | Design section 9's one opt-in live scenario: real Claude Code (symlinked, not copied, as the `claude` stub) driven through the real `hop run`/`hop launch` pipeline in the operator's own default profile, with one small brief to a passing check, then a forced cold relaunch (`claude --resume <preassigned-uuid>`) continuation. See "Live scenario" below |

## Invariants

- The developer's live Herdr session is never addressed: subprocess
  environments are allowlists built from scratch (no inherited `HERDR_*`),
  every server is a named session under a temporary `XDG_CONFIG_HOME`, and
  the suite only ever reads the real user-global registry files to prove
  they did not change.
- No fixed sleeps: readiness and completion are bounded polls against the
  socket, plugin log records or pane text.
- An invocation response is not success. Completion is the log record the
  returned log ID resolves to, asserted on status, exit code and output.
- Cleanup asks for a graceful `server.stop` through the test server's own
  socket, then `killProcessGroupThenReap` `SIGKILL`s the server's whole
  process group and reaps the leader with `Wait`, signaling the group while
  the leader is still unreaped so its pgid cannot have been recycled onto an
  unrelated process. `safeToSignalGroup` rejects any non-positive pgid, init
  (`1`) and the caller's own group before any signal is sent. After the
  leader is reaped, it polls signal-0-to-the-group for `ESRCH` (bounded, and
  a timeout fails the test) so a descendant reparented to init and still
  holding an artifact log file open cannot race artifact removal. Panes are
  closed by the IDs earlier responses returned. No process matching.
- Evidence (server output, action stdout, pane snapshots, plugin logs) is
  written under `$TMPDIR/hop-integration/<test>-*` and retained only when
  the test fails; the path is logged.
- When no herdr binary is installed the suite skips with the explicit
  reason `real-process suite skipped, not run: ...`. GitHub-hosted CI
  runners have no herdr binary, so the CI gate does not run this suite;
  the workflow prints the skip lines rather than implying coverage.
  `HOP_TEST_HERDR_BIN` pins a specific binary.
- These tests prove behavior against herdr 0.9.0 on this machine; they are
  compatibility evidence for exactly the binary they ran against.

## Live scenario

`TestLiveClaudeDefaultProfileRun` (livehop_test.go) is the one test in this
suite that runs a real harness against a real provider and is never run by
this repository's own automated verification — a human runs it
deliberately via `make test-live`.

- **Gate**: `HOP_LIVE_HARNESS=1` opts in (unset skips with a one-line
  reason, matching S4's existing gate); once opted in, a missing `claude`
  binary is a hard failure, not a skip.
- **Profile**: the worker pane's `HOME` is the operator's own real home
  directory by default (`os.UserHomeDir()`), so the launched harness uses
  its already-authenticated default profile at `<home>/.claude` — the
  sanitizing launcher still strips `CLAUDE_CONFIG_DIR` unconditionally, so
  nothing here can redirect it. Everything else (the herdr server, the HOP
  state root, the fixture repository) stays isolated in temporary roots
  exactly as in every other test. `HOP_LIVE_HARNESS_HOME` overrides the
  profile directory (must be an absolute, existing directory, or the test
  fails clearly) — point it at a profile prepared specifically for this
  test if you would rather not use your daily one.
- **Cost and side effects**: this run makes real requests to the
  operator's configured provider and therefore costs real tokens. It
  leaves a real Claude Code session record under
  `<home>/.claude/projects/<encoded-fixture-worktree-path>/<uuid>.jsonl`
  for the temporary fixture repository's worktree — harmless (the fixture
  itself is removed on success like every other artifact) but real, and
  not something `make test-live` cleans up on the profile side.
- **Run it**: `make test-live`, or directly:
  `HOP_LIVE_HARNESS=1 go test -count=1 -v -run '^TestLiveClaudeDefaultProfileRun$' -timeout 20m ./test/integration`.
  `HOP_LIVE_HARNESS_HOME=/path/to/profile` prepends to either form to use a
  prepared profile instead of the real one.

## Section 9 real-process scenario coverage

Every design section 9 real-process row this task could express against
the unmodified `hop` binary and current landed code is a scenario listed
above. The following were evaluated and are **not expressible** without
either a pause/injection hook added to production code (out of scope for
this task; never added) or hand-seeded store state bypassing the real
protocol (which would stop proving anything about the real binary) —
their dispositions are already covered by `internal/app`'s own
decision-table unit tests (`decision_test.go`, `usecase_launch_test.go`,
`usecase_execboundary_test.go`):

- **Launcher killed after the claim write, before exec** (design section
  6): the window between `PrepareLaunchExec`'s claim write and the
  caller's `syscall.Exec` is a handful of Go statements in a single OS
  process — no external observer, even a tight in-process
  `pane.process_info` poll, can reliably land a signal inside it.
- **Exec failure with the `exec_failed` write itself suppressed**: the
  same class of problem one step later, between the exec failing and
  `FailLaunchExec`'s write landing. (The *unsuppressed* exec-failure case
  — the write lands normally — is expressible and covered by
  `TestRealProcessExecFailureSettlesExecFailed`.)
- **Paused pre-exec launcher argv exclusion**: requires catching the
  launcher's own argv as the pane's foreground process before it execs —
  the identical race as the first case above.
- **The fallback-transport shell-fork pid case, and the send-text
  fallback as its own scenario**: `internal/app` never calls
  `Runtime.SendText` anywhere — `hop run`'s launch flow only ever uses the
  primary `layout.apply` transport, with no decision-table branch,
  config flag or fallback path that would select send-text. The
  send-text mechanism itself is a working, independently-tested port and
  adapter method (`internal/adapters/herdr`'s own tests, and the S1 probe,
  `spike_launch_test.go`) — there is simply nothing in `hop run`'s own
  behavior today to drive it through.
- **Stop against a pre-exec launch claim specifically**: same root cause
  as the first case — reliably catching (and holding) the attempt in
  `launching` with an *unsettled* claim before issuing `hop stop` needs
  the same pause hook.
- **Server-restart positive-evidence retirement of an auto-relaunched
  occupant, the phantom-snapshot case, and the fail-closed
  same-kind-replacement case**: not fundamentally blocked (S3's spike,
  `spike_restore_test.go`, already proves Herdr's own auto-relaunch
  mechanism against a generically-named `claude` process) but not
  attempted in this task's timeframe — a genuine scope/time tradeoff, not
  an infeasibility finding, flagged in the handoff for whoever picks up
  task 6b next if it matters.
- **Check death before the claim write, and a claim-write failure
  refusing to run**: both need either the same class of pause hook (to
  land a kill in the process-spawn-to-claim-write window) or externally
  holding an exclusive sqlite lock against the run's own database from a
  second connection — feasible in principle but not attempted within the
  one-iteration time-box this family of scenarios was given; the
  reliable, fast parts of the check-death family that ARE expressible
  (leader exit with live children, and check.repeatable's two-outcome
  contract) are covered by `checkdeath_test.go`.
- **Wrapper forks the real harness after exec**: expressible, and
  covered — `TestRealProcessForkingWrapperAfterExec` confirms Herdr
  reports the forked child as `foreground[0]` and the claim fails closed
  exactly as designed.

## Dependencies and ports

- Allowed inward imports: [internal/app](../../internal/app/AGENTS.md) and
  [internal/adapters/herdr](../../internal/adapters/herdr/AGENTS.md)
  (rule `test/integration`, category `integration-test`); the presentation
  and observation tests drive the `app` use cases through the real adapters,
  and wire-shape structs the assertions need are declared locally in the
  tests.
- Consumed/implemented ports: none; the suite drives `herdr.Client` directly.
- External libraries: `github.com/creack/pty` (test-only), used by the PTY
  smoke to attach a client on a pseudo-terminal — Go's standard library has
  no PTY. It is allowlisted only for this integration-test package
  (`testThirdParty` in the `test/integration` rule); the architecture
  checker's production-closure check proves no production package reaches it.
- External binaries: `herdr` (skipped when absent), the `go` tool to build the
  staged plugin, the compiled `TestSpike*` fixture, the cached `./cmd/hop`
  build (`buildHopBinary`) and the compiled fixture worker, `git` (fixture
  repository construction and the fixture worker's own commits — isolated
  per repository from any developer git configuration, never the invariant
  the suite is proving), `ps` (`listGroupMembers`'s independent process-group
  listing), `sqlite3` (`querySQLite`'s read-only inspection of a test's own
  throwaway `hop.db`, shelled out to the same way this suite already uses
  `git`/`ps`), and — for S4 and the live scenario only — `claude` (both
  opt-in behind `HOP_LIVE_HARNESS=1` and skipped, not failed, when that is
  unset; S4 also skips, with its own reason, if `claude` is missing even
  when opted in, while the live scenario treats a missing `claude` once
  opted in as a hard failure, never a skip — see "Live scenario" above).
  The `TestSpike*` cases
  also skip a shell capability case (zsh) with a reason when that shell is
  not installed.

## Verification

- `go test -count=1 -run TestRealProcess -v ./test/integration` — the Phase 1
  suite, verbosely, showing pass or the explicit skip.
- `go test -count=1 -run TestSpike ./test/integration` — the Phase 2 capability
  spike (S1, S2, S3, S5, S6, S7 and the harness self-tests). S4 is skipped
  here; run it opt-in with `HOP_LIVE_HARNESS=1 go test -count=1 -run
  TestSpikeClaudePreassignedSessionID ./test/integration`, which invokes the
  real `claude` in print mode against a scratch profile.
- `go test ./test/integration` — also runs the harness self-tests that need
  no herdr binary: `TestSafeToSignalGroup`,
  `TestKillProcessGroupThenReapKillsDescendants`,
  `TestArtifactDirRemovedOnPassingRun`, `TestArtifactRetentionDecision`,
  `TestSpikeRestartWaitsForGracefulExit`,
  `TestSpikeOwnedGroupTeardownKillsPipeHoldingChild`, `TestListGroupMembers`,
  and task 6b's harness/fixture self-tests below.
- `go test -count=1 -run 'TestBuildAndRunHopBinary|TestHopEnviron|TestStartHopController|TestFixtureRepo|TestFixtureWorker' -v ./test/integration` —
  task 6b's Phase A self-tests: the cached `cmd/hop` build and one-shot
  runner against the Phase 1 commands (`version`), `requireHopCommand`'s skip
  detection against a command name that will never exist, `hopEnviron`'s
  isolation contract asserted directly with no process execution, the
  anchored-leader mechanics via a short-lived command, the fixture
  repository's deterministic/git-dependent/submodule checks proven at the
  git level with no herdr or hop binary, and the fixture worker driven
  directly (no herdr) against a fake `hop` stub proving argv/env parsing,
  transient-retry and the FIXTURE-BEHAVIOR dispatch.
- `go test -count=1 -run TestRealProcess -v ./test/integration` also covers
  every task 6b Phase B scenario (their names all share the
  `TestRealProcess` prefix): `TestRealProcessRunEndToEnd`,
  `TestRealProcessSanitizedLaunchExec`,
  `TestRealProcessDuplicateSubmissionAfterCompletion`,
  `TestRealProcessDetachDistinctFromStop`,
  `TestRealProcessFailedCheckRetainsArtifacts`,
  `TestRealProcessGitDependentCheckSucceedsInDetachedCheckout`,
  `TestRealProcessSubmoduleRepositoryFailsClearly`,
  `TestRealProcessControllerKillResumeWarmReattach`,
  `TestRealProcessConfirmAbsentColdRelaunchNonRestart`,
  `TestRealProcessStaleSubmissionFromRetiredIncarnation`,
  `TestRealProcessConfirmAbsentRefusedAfterServerRestart`,
  `TestRealProcessStopWithTerminationObserved`,
  `TestRealProcessStopInterruptsRunningCheckGroup`,
  `TestRealProcessDuplicateLaunchInvocation`,
  `TestRealProcessExecFailureSettlesExecFailed`,
  `TestRealProcessForkingWrapperAfterExec`,
  `TestRealProcessCheckLeaderExitWithLiveChildren`,
  `TestRealProcessCheckDeathUnknownOutcomeRepeatable`. Several pay a real,
  unavoidable wall-clock cost (the lease TTL a crash-recovery scenario
  waits out, a herdr server restart, a real check's sleep) — this sweep
  takes several minutes, not seconds; run a narrower `-run` pattern while
  iterating on one scenario.
- `TestLiveClaudeDefaultProfileRun` is excluded from every command above by
  its own gate (`HOP_LIVE_HARNESS` unset): confirm it compiles and skips
  cleanly with `go test -count=1 -v -run '^TestLiveClaudeDefaultProfileRun$'
  ./test/integration`; run it for real only via `make test-live` or the
  command under "Live scenario" above.
- Test fixtures: none on disk as static files; every fixture (servers, roots,
  the staged plugin, the spike fixture binaries, the cached `cmd/hop` build,
  fixture repositories and the fixture worker) is generated or compiled per
  test run and removed by cleanup — retained only on failure, under the same
  artifact directory as every other evidence this suite produces. The suite
  never removes the shared `$TMPDIR/hop-integration` root by hand — only its
  own per-test directories.

## Related guides

- [Parent index](../AGENTS.md)
- [Herdr adapter](../../internal/adapters/herdr/AGENTS.md)
- [Plugin testing plan](../../docs/architecture/plugin-testing.md)
