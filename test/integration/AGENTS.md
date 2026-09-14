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
| [harness_test.go](harness_test.go) | `requireHerdr`, `prepareServer`, `testServer`, `stagePlugin`, `newArtifactDir`, `registrySnapshot`, `assertNoRegistryLeak`, `waitForLog`, `waitUntil`, `killProcessGroupThenReap`, `safeToSignalGroup`, `call` | Disposable server lifecycle (spawn, bounded socket-readiness wait, API `server.stop` then a process-group `SIGKILL` and reap of the leader), hermetic subprocess environment built from scratch, plugin staging (`go build` + manifest copy), failure-retained evidence, user-global registry leak checks and bounded polling |
| [reap_test.go](reap_test.go) | `TestSafeToSignalGroup`, `TestKillProcessGroupThenReapKillsDescendants`, `processExists` | Table-driven guard against signaling an unsafe process group (non-positive, init, or the caller's own group), and a fixture leader + backgrounded grandchild proving the whole group is torn down and the leader reaped |
| [plugin_test.go](plugin_test.go) | `TestRealProcessPluginLifecycle`, `TestRealProcessStartupHookRunsOnServerStart` | Link → action list → invoke → completed log records with real output → workspace/pane open and rendered evidence → unlink; and the startup hook proven to run on server start, not on link |
| [presentation_test.go](presentation_test.go) | `TestRealProcessAgentPresentationAndView`, `createRunFixture` | A deterministic manager+workers fixture (custom agent identity/state via `pane.report_agent`), HOP metadata published through the presentation adapter, tokens round-tripped through `agent.list`, manager-first ordering, HOP's owned view select/clear, and another owner's view surviving HOP's owned clear |
| [observation_test.go](observation_test.go) | `TestRealProcessEventObservationReconcile`, `TestRealProcessContextInjection` | Subscribe-before-snapshot reconciliation over a real status transition, and context injected into a live pane through `pane.send_text` |
| [launchenv_test.go](launchenv_test.go) | `TestRealProcessLaunchEnvironment`, `TestRealProcessLaunchEnvironmentIsAdditive` | The launch-environment compatibility results: workspace/tab/pane creation carry an explicit env to the launched shell, and the env map is additive — a Herdr-managed variable is authoritative and cannot be overridden. See [launch-environment.md](../../docs/architecture/launch-environment.md) |
| [pty_test.go](pty_test.go) | `TestRealProcessPTYRendering`, `ptyClient` | A PTY-attached client at fixed 100x30 dimensions capturing how the native Agents sidebar renders HOP's projection: the fixture agents drawn, manager-first order on screen, and a HOP metadata token in the rows |
| [artifacts_test.go](artifacts_test.go) | `TestRealProcessArtifactDirRemovedOnPassingRun`, `TestArtifactDirRemovedOnPassingRun`, `TestArtifactRetentionDecision` | A real server run with an open subscription proving the process-group teardown leaves the artifact directory empty, plus harness self-tests for the evidence-retention contract that need no herdr binary |

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
- External binaries: `herdr` (skipped when absent) and the `go` tool to
  build the staged plugin.

## Verification

- `go test -count=1 -run TestRealProcess -v ./test/integration` — the suite
  itself, verbosely, showing pass or the explicit skip.
- `go test ./test/integration` — also runs the harness self-tests that need
  no herdr binary: `TestSafeToSignalGroup`,
  `TestKillProcessGroupThenReapKillsDescendants`,
  `TestArtifactDirRemovedOnPassingRun` and `TestArtifactRetentionDecision`.
- Test fixtures: none on disk; servers, roots and the staged plugin are
  created per test and removed by cleanup.

## Related guides

- [Parent index](../AGENTS.md)
- [Herdr adapter](../../internal/adapters/herdr/AGENTS.md)
- [Plugin testing plan](../../docs/architecture/plugin-testing.md)
