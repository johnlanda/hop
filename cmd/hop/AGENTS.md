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
| [main.go](main.go) | `main`, `run`, `dispatch`, `printUsage`, `exitOK`, `exitFailure`, `exitUsage` | Entry point; maps a command name to its handler (worker plumbing listed under its own usage heading) and a failed write to `exitFailure` |
| [compose.go](compose.go) | `controllerAPI`, `controllerConfig`, `deps`, `defaultDeps`, `openController`, `describeStoreOpenFailure`, `waitInterval`, `lookupExecutable` | The composition root proper: opens the SQLite store under the resolved state root, wires the system/process/config adapters and — for pane-acting commands — the Herdr runtime (compile-time `app.Runtime` assertion) into one `app.Controller`; `deps` carries every effectful seam (env, clock, wait, signals, exec, store opening) so command tests substitute fakes; `lookupExecutable` implements `app.ExecutableLookup` over the sanitized PATH |
| [stateroot.go](stateroot.go) | `resolveStateRoot`, `requireWorkerStateRoot` | The design's single state-root rule: `${XDG_STATE_HOME:-$HOME/.local/state}/hop` with `HOP_STATE_DIR` as the only override (relative refused); worker contexts REQUIRE the launch-provided absolute `HOP_STATE_DIR` and never fall back |
| [loop.go](loop.go) | `runControllerLoop`, `runHeartbeats`, `loopResult`, `detachAndReport`, `releaseQuietly`, `resolveRunArg`, `seqLabel`, `exitForRunState` | The foreground controller loop: concurrent 10s heartbeats under the 30s TTL, one line per run transition, launch corroboration, stop routing, 2s check polling; run arguments resolve as UUIDs or `r<seq>` labels |
| [runcmd.go](runcmd.go) | `runRun`, `finishControllerLoop`, `watchDetachSignals`, `resolveRepositoryRoot`, `hopExecutablePath`, `stringList` | `hop run "<brief>"`: StartRun, the start line, then the loop; SIGINT/SIGTERM detach (second signal force-exits); `ErrStartRefused` maps to exit 2 |
| [statuscmd.go](statuscmd.go) | `runStatus`, `renderRunListing`, `renderRunDetail` | `hop status`: non-terminal runs by default, `-all` includes completed/failed/stopped, `-run` renders the full detail block including the unknown-check options; state is data, not an exit code |
| [stopcmd.go](stopcmd.go) | `runStop`, `driveStopRounds`, `observeStop` | `hop stop <run-id>`: monotonic stop request, then DriveStop rounds with the acquired lease or observation of a live controller's stop; exit 0 only on observed `stopped`, rerunnable otherwise |
| [resumecmd.go](resumecmd.go) | `runResume`, `resumeDetailLine`, `runLabel` | `hop resume <run-id>` with `--confirm-absent`: prints what reconciliation established; continuable outcomes stay in the loop, fail-closed/reconciling/unsupported reports release the lease and exit 1 for the human to act and rerun |
| [resultcmd.go](resultcmd.go) | `runResult`, `runResultSubmit`, `submissionLine` | `hop result submit` (section 7): `--summary`/`--commit`, IDs defaulting from `HOP_*`, incarnation from `HOP_INCARNATION_ID`; accepted/duplicate exit 0, everything else 1 with the transient first line verbatim |
| [launchcmd.go](launchcmd.go) | `runLaunch` | `hop launch --run --attempt`, the worker exec boundary: worker state root, `PrepareLaunchExec` (validate → sanitize → per-harness compose — all three harnesses launch, cold resume is Claude-only — resolve → claim), `process.Exec`; a post-claim failure settles exec_failed via `FailLaunchExec`; one stderr line, never an environment value |
| [checkexeccmd.go](checkexeccmd.go) | `runCheckExec` | `hop check-exec --op -- <argv>`, the check exec boundary: group-leadership fact, `PrepareCheckExec` (claim before exec), `process.ExecResolved` so the frozen argv runs verbatim and the check's exit status propagates through the exec |
| [doctor.go](doctor.go) | `runDoctor`, `renderReport`, `renderStateRoot`, `defaultDoctorTimeout` | `hop doctor`: the Phase 1 report plus the store-path line (resolved state root, source label, store presence) computed without opening the database |
| [version.go](version.go), [plugincontext.go](plugincontext.go) | `runVersion`, `resolveVersion`, `recordingWriter`, `runPluginContext` | Unchanged Phase 0/1 commands |
| [fakes_test.go](fakes_test.go) | `fakeController`, `testDeps`, `newTestDeps` | Scripted `controllerAPI` fake and the fully-faked `deps` (fixed clock, sleepless waits, recording exec seams) |

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
- Exit codes: 0 success, 1 failure, 2 usage. `hop run`/`hop resume` exit 0
  only on a `completed` run; failed, stopped, detach and errors exit 1; a
  StartRun refusal before any side effect (`app.ErrStartRefused`) is usage.
  `hop stop` exits 0 only when `stopped` was observed. `hop status` exits 0
  on rendering success — state is data. `hop result submit` exits 0 for
  accepted and duplicate only. The exec boundaries exit 1 on every failure
  with one stderr line that never echoes an environment value; on success
  they never return. A write failure takes precedence, including flag
  diagnostics through `recordingWriter`.
- One state-root rule (section 4): controller commands resolve
  XDG-with-`HOP_STATE_DIR`-override through `resolveStateRoot`; worker
  commands (`launch`, `check-exec`, `result submit`) require the provided
  absolute `HOP_STATE_DIR` and never fall back. The SQLite adapter receives
  the absolute root and creates it 0700.
- Adapters are constructed only here, once per command; the Herdr runtime
  is wired only for commands that act on panes/worktrees (`run`, `stop`,
  `resume`). `hop status`, the exec boundaries and `result submit` run
  store-only with a nil Runtime.
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
- Consumed/implemented ports: wires the adapters into every `app.Controller`
  port; implements `app.ExecutableLookup` (`lookupExecutable`); constructs
  `herdr.InstallationProbe` for `app.Doctor`.
- External libraries: none; standard library only.

## Verification

- `go test ./cmd/hop` — table-driven command behavior against the scripted
  `controllerAPI` fake and fully-faked `deps` (no real adapter is opened):
  dispatch and exit codes, state-root resolution (default, override,
  relative rejection) and the doctor line, the controller loop with a fake
  clock (transitions, stop routing, detach on signal, heartbeat-failure
  exit), hop run's refusal/failure/detach paths and flag surface, resume
  outcome routing, stop's drive/observe/deadline paths, status filtering
  and detail rendering, result submit's outcome and exit-code contract,
  and both exec boundaries (state-root requirement, prepared request
  shape, recorded exec argv/env, exec_failed settlement after a failed
  exec, no exec on refusal).
- `go test -race -shuffle=on ./cmd/hop` — the loop's concurrent heartbeat
  goroutine under the race detector.
- `make build && .bin/hop version` — builds the binary and prints the version
  the Go toolchain derived from version control.
- `.bin/hop doctor` — probes the real installation on this machine.
- Real-adapter, real-Herdr behavior is exercised only by the
  `test/integration` suite (task 6b), never by these unit tests.
- Test fixtures: stub shell scripts written to temporary directories; no
  live Herdr session is contacted.

## Related guides

- [Parent index](../AGENTS.md)
- [Root guide](../../AGENTS.md)
- [Application layer](../../internal/app/AGENTS.md)
- [Herdr adapter](../../internal/adapters/herdr/AGENTS.md)
- [SQLite store](../../internal/adapters/sqlite/AGENTS.md)
- [Process adapter](../../internal/adapters/process/AGENTS.md)
- [System adapter](../../internal/adapters/system/AGENTS.md)
- [Config adapter](../../internal/adapters/config/AGENTS.md)
