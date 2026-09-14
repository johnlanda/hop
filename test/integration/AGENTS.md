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
| [harness_test.go](harness_test.go) | `requireHerdr`, `prepareServer`, `testServer`, `serverProcess`, `start`, `restart`, `reapServer`, `stagePlugin`, `newArtifactDir`, `registrySnapshot`, `assertNoRegistryLeak`, `waitForLog`, `waitUntil`, `killProcessGroupThenReap`, `safeToSignalGroup`, `call` | Disposable server lifecycle (spawn, bounded socket-readiness wait, API `server.stop` then a process-group `SIGKILL` and reap of the leader), a `restart` that gracefully stops and reaps the current server before relaunching on the same roots so persisted session state survives (S3), per-`serverProcess` at-most-once reaping, hermetic subprocess environment built from scratch (`shell` selects the pane login shell), plugin staging (`go build` + manifest copy), failure-retained evidence, user-global registry leak checks and bounded polling |
| [reap_test.go](reap_test.go) | `TestSafeToSignalGroup`, `TestKillProcessGroupThenReapKillsDescendants`, `processExists` | Table-driven guard against signaling an unsafe process group (non-positive, init, or the caller's own group), and a fixture leader + backgrounded grandchild proving the whole group is torn down and the leader reaped |
| [plugin_test.go](plugin_test.go) | `TestRealProcessPluginLifecycle`, `TestRealProcessStartupHookRunsOnServerStart` | Link → action list → invoke → completed log records with real output → workspace/pane open and rendered evidence → unlink; and the startup hook proven to run on server start, not on link |
| [presentation_test.go](presentation_test.go) | `TestRealProcessAgentPresentationAndView`, `createRunFixture` | A deterministic manager+workers fixture (custom agent identity/state via `pane.report_agent`), HOP metadata published through the presentation adapter, tokens round-tripped through `agent.list`, manager-first ordering, HOP's owned view select/clear, and another owner's view surviving HOP's owned clear |
| [observation_test.go](observation_test.go) | `TestRealProcessEventObservationReconcile`, `TestRealProcessContextInjection` | Subscribe-before-snapshot reconciliation over a real status transition, and context injected into a live pane through `pane.send_text` |
| [launchenv_test.go](launchenv_test.go) | `TestRealProcessLaunchEnvironment`, `TestRealProcessLaunchEnvironmentIsAdditive` | The launch-environment compatibility results: workspace/tab/pane creation carry an explicit env to the launched shell, and the env map is additive — a Herdr-managed variable is authoritative and cannot be overridden. See [launch-environment.md](../../docs/architecture/launch-environment.md) |
| [pty_test.go](pty_test.go) | `TestRealProcessPTYRendering`, `ptyClient` | A PTY-attached client at fixed 100x30 dimensions capturing how the native Agents sidebar renders HOP's projection: the fixture agents drawn, manager-first order on screen, and a HOP metadata token in the rows |
| [artifacts_test.go](artifacts_test.go) | `TestRealProcessArtifactDirRemovedOnPassingRun`, `TestArtifactDirRemovedOnPassingRun`, `TestArtifactRetentionDecision` | A real server run with an open subscription proving the process-group teardown leaves the artifact directory empty, plus harness self-tests for the evidence-retention contract that need no herdr binary |
| [spike_fixture_test.go](spike_fixture_test.go) | `buildSpikeFixtures`, `spikeFixtures`, `spikeFixtureSource`, `newSpikeUUID`, `processInfo`, `spikeProcessInfo`, `agentRecords`, `waitForAgent`, `assertNeverListedAsAgent` | Shared Phase 2 capability-spike fixtures: one Go program built and installed under a launcher-stand-in name (execve to a harness) and under a detection-recognized `claude` name and an unrecognized name; `pane.process_info`/`agent.list` readers and bounded agent-detection polling |
| [spike_launch_test.go](spike_launch_test.go) | `TestSpikeLaunchLineDetection`, `TestSpikeUnrecognizedProcessNameIsNotDetected`, `createSpikeWorkerPane` | S1: a `send_text` `exec <launcher> … -- <harness>` line launches despite noisy `/bin/sh` and zsh login-shell startup (zsh skipped with a reason when absent), the execve'd recognized process is detected as an agent (an unrecognized name is not), the additive env survives both execs, and the exec'd process's pid is asserted equal to `shell_pid` and the foreground process group |
| [spike_harness_test.go](spike_harness_test.go) | `TestSpikeRestartBarrier`, `TestSpikeGroupAnchor`, `TestSpikeOwnedGroupTeardown`, `startFixtureLeader` | Harness self-tests, no herdr binary: the S3 restart barrier waits for the leader's actual exit (release-gated save survives) and force-kills when it never exits; the group anchor pins the pgid after the leader is reaped and a leader that already exited cannot be anchored (inconclusive); and the live-probe owned-group lifecycle retires surviving children on both the deadline and normal-exit paths |
| [spike_identity_test.go](spike_identity_test.go) | `TestSpikePaneProcessIdentity`, `waitForForegroundProcess` | S2: the exact `pane.process_info` identity fields (pid, name, argv0, full argv, cmdline, cwd; empty `tty` on macOS; no start time), and a same-kind occupant replacement distinguished by pid + argv with a stable `shell_pid` |
| [spike_restore_test.go](spike_restore_test.go) | `TestSpikeRestorePlainPaneLosesAdditiveEnv`, `TestSpikeRestoreAutoRelaunchBypassesLauncher`, `snapshotDump`, `snapshotPaneIDForLabel` | S3: a graceful restart restores a pane's label but not its creation-time additive env; a recorded native session makes Herdr auto-relaunch via `claude --resume` (deferred until client geometry), bypassing the launcher and dropping `HOP_*`; the delayed-restore window shows a phantom idle agent in `session.snapshot` |
| [spike_claude_session_test.go](spike_claude_session_test.go) | `TestSpikeClaudePreassignedSessionID`, `requireClaude`, `runClaude`, `findTranscript`, `forbiddenClaudeEnv` | S4: against the installed claude in print mode, in a scratch `CLAUDE_CONFIG_DIR` with a credential-free environment, `--session-id <uuid>` is accepted and creates the transcript under that UUID, `--resume <uuid>` finds it (unknown → not found); the only test that runs a real harness, so it is opt-in (`HOP_LIVE_HARNESS=1`, else skipped with a reason) and also skips when no claude binary is installed |
| [spike_agentstart_test.go](spike_agentstart_test.go) | `TestSpikeAgentStartArgvCapability` | S5: `agent.start` rejects an unrecognized kind and control-char args and composes argv as `[executable(kind)] + args`, so no wrapper can be interposed; and it types a bare name whose resolution the pane shell's PATH (`path_helper`) decides |
| [spike_layout_test.go](spike_layout_test.go) | `TestSpikeLayoutApplyCommandPane`, `TestSpikeLayoutApplyAddsTabToExistingWorkspace`, `snapshotPaneByLabel`, `snapshotIDs`, `paneExists`, `drainEventNames` | S6: `layout.apply` pane nodes carry `command`+`env`+`label` and run the argv as the pane process (asserted, at the requested cwd) with no shell; an additive apply (`workspace_id` only) adds one tab and every pre-existing tab/pane id survives (+1 tab, +1 pane); a non-zero command exit closes the pane and carries no exit status in `pane.exited` |
| [spike_markers_test.go](spike_markers_test.go) | `TestSpikeCreationMarkerTabLabel` | S7: a `tab.create` creation-time `label` round-trips through `tab.list` and `session.snapshot`, letting a crashed controller recover its own tab and root pane by a unique marker without the create response; the additive env is never a snapshot field |

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
  staged plugin and the compiled `TestSpike*` fixture, and — for S4 only —
  `claude` (opt-in and skipped otherwise). The `TestSpike*` cases also skip a
  shell capability case (zsh) with a reason when that shell is not installed.

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
  `TestSpikeRestartWaitsForGracefulExit` and
  `TestSpikeOwnedGroupTeardownKillsPipeHoldingChild`.
- Test fixtures: none on disk; servers, roots, the staged plugin and the spike
  fixture binaries are created per test and removed by cleanup. The suite never
  removes the shared `$TMPDIR/hop-integration` root by hand — only its own
  per-test directories.

## Related guides

- [Parent index](../AGENTS.md)
- [Herdr adapter](../../internal/adapters/herdr/AGENTS.md)
- [Plugin testing plan](../../docs/architecture/plugin-testing.md)
