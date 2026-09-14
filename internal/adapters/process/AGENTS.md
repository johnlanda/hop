# internal/adapters/process

## Purpose

Local-process driven adapter: the harness exec boundary (`Exec`), the
process-group command runner (`Runner` implementing `app.CommandRunner`,
used for the check's git operations and for spawning `hop check-exec`) and
the process-group inspector (`GroupInspector` implementing
`app.ProcessGroupInspector`, the retirement surface after takeover).
Ownership classification of a listed group stays in `internal/app`; this
package only observes and signals.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [exec.go](exec.go) | `Exec` | syscall.Exec wrapper: rejects an empty, bare or relative argv[0] before any system call, passes the complete env verbatim (nil execs empty), never returns on success |
| [runner.go](runner.go) | `Runner`, `CancellationError` | Runs argv as the leader of a new process group (Setpgid) with the complete env given, captures each stream bounded at 1 MiB, maps completions to exit codes (-1 for a signal), and on ctx cancellation SIGKILLs the whole group, reaps the leader within a bounded wait and returns a typed `*CancellationError` with the output captured so far |
| [inspector.go](inspector.go) | `GroupInspector`, `ArgvUnavailable` | Lists a group's live members (membership from one `ps -A -o pid=,pgid=` parse; a listing failure is an error, never an empty result) and sends the group SIGKILL under the same guard as the runner |
| [argv_darwin.go](argv_darwin.go) | `processArgv` | Exact per-pid argv via the `kern.procargs2` sysctl (raw sysctl(2); the stdlib Sysctl cannot address a pid-parameterized MIB) |
| [argv_linux.go](argv_linux.go) | `processArgv` | Exact per-pid argv via `/proc/<pid>/cmdline` (NUL-separated, byte-for-byte) |

## Invariants

- Absolute executables only: `Exec` and `Runner.Run` reject a bare or
  relative argv[0] — bare names never pin which binary runs (macOS
  path_helper PATH reordering), and the design's exec boundary passes
  absolute paths everywhere.
- One owner of reaping per process: the leader is reaped by exactly one
  Wait (a single goroutine in `Run`), the anchor by `retireAnchor`, and no
  raw Wait4 ever touches a Cmd-managed pid.
- A group is signaled only while an unreaped member of ours pins its id:
  `Run` joins a sleep anchor into the leader's fresh group before any Wait
  exists, so the one cancellation SIGKILL can never reach a recycled group
  id; group absence is then confirmed with signal 0, which only reads
  existence. An anchor orphaned by a crashed controller keeps the group id
  pinned (for at most 100000 s, its sleep argument) so a successor's
  retirement of the recorded group cannot hit a recycled id either; until
  the check group is retired, a listing of it may therefore include the
  anchor (`<resolved sleep path> 100000`) alongside the check processes.
- `SignalGroup` cannot verify membership itself: the application lists and
  argv-matches the group immediately before signaling (the design's
  group-retirement rule), and the window between that listing and the
  signal, in which a fully-exited group's id could be recycled, is a
  documented residual limitation of the cooperative model — no observable
  surface carries a process start time. Both signal paths refuse pgid <= 1
  and the caller's own group, and treat ESRCH as the goal state.
- Member argv is exact, never guessed: it comes from the platform's
  byte-for-byte source (`/proc/<pid>/cmdline`; `kern.procargs2`), so
  arguments containing whitespace survive; ps's whitespace-joined command
  column is never used for argv. A member whose argv cannot be read (raced
  exit, zombie, denied) carries the single entry `ArgvUnavailable`, which
  matches no real argv, so application-side classification fails closed.
- A completed command is a result, not an error: ExitCode carries the exit
  status, -1 for a termination by a signal `Run` did not send; errors are
  reserved for a run that could not be executed or supervised, and
  cancellation returns the typed `*CancellationError` (wrapping the context
  cause) plus the bounded output captured before the kill.
- Nothing is inherited: `Command.Env` and `Exec`'s env are the complete
  environment; nil means empty, never the parent's environment.

## Dependencies and ports

- Allowed inward imports: [internal/app](../../app/AGENTS.md).
- Implemented ports: `app.CommandRunner` by `Runner`,
  `app.ProcessGroupInspector` by `GroupInspector`; `Exec` is consumed
  directly by the exec-boundary commands wired in `cmd/hop` (task 6a).
- External libraries: none; standard library only (`syscall` for process
  groups, signals and the darwin sysctl).

## Verification

- `go test ./internal/adapters/process` — the fixture children are the test
  binary re-run in helper modes (an env-dumping child, a group-spawning
  child that leaves a sleeping descendant with whitespace-bearing argv, a
  never-exiting child), exercised through the real `Runner`: exit-code
  mapping, exact-environment delivery, nil-env emptiness, working
  directory, the 1 MiB capture bound, cancellation killing the whole group
  with the typed result and group-absence proof, and the
  leader-exits-with-children case retired through the real
  `GroupInspector` (exact argv with spaces and tabs matched, empty after
  retirement). `Exec` is proven by a helper subprocess that observes the
  replaced image (same pid, exec-provided env only) plus in-process
  rejection and failure-return cases. Inspection failures are proven
  against injected ps stubs (nonzero exit, missing binary, unparseable
  rows) and an out-of-range pid for the `ArgvUnavailable` marker.
- Test fixtures: none on disk; helper modes and ps stubs are written by the
  tests.

## Related guides

- [Parent index](../AGENTS.md)
- [Application layer and ports](../../app/AGENTS.md)
- [Phase 2 design, sections 3, 6 and 7](../../../docs/plan/phase-2-design.md)
