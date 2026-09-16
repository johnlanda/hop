# internal/adapters/process

## Purpose

Local-process driven adapter: the harness exec boundary (`Exec`,
`ExecResolved`), the
process-group command runner (`Runner` implementing `app.CommandRunner`,
used for the check's git operations and for spawning `hop check-exec`) and
the process-group inspector (`GroupInspector` implementing
`app.ProcessGroupInspector`, the retirement surface after takeover).
Ownership classification of a listed group stays in `internal/app`; this
package only observes and signals.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [exec.go](exec.go) | `Exec`, `ExecResolved` | syscall.Exec wrappers, never returning on success, passing the complete env verbatim (nil execs empty). `Exec` rejects an empty, bare or relative argv[0] before any system call; `ExecResolved` executes an absolute path while preserving the caller's argv byte-for-byte (argv[0] may stay the bare name the caller resolved) — hop check-exec's boundary, whose running argv must equal the frozen check argv exactly for group retirement to match |
| [runner.go](runner.go) | `Runner`, `CancellationError` | Runs argv as the leader of a new process group (Setpgid) with the complete env given, captures each stream bounded at 1 MiB, maps completions to exit codes (-1 for a signal), and on ctx cancellation SIGKILLs the whole group, reaps the leader within a bounded wait and returns a typed `*CancellationError` with the output captured so far; an unstartable anchor retires the group while the leader is provably unreaped instead of inferring anything from the failure's errno |
| [inspector.go](inspector.go) | `GroupInspector`, `ArgvUnavailable` | Lists a group's live members (membership from one `ps -A -o pid=,pgid=` parse; a listing failure is an error, never an empty result) and sends the group SIGKILL under the same guard as the runner |
| [argv_darwin.go](argv_darwin.go) | `processArgv`, `parseProcargs2` | Exact per-pid argv via the `kern.procargs2` sysctl (raw sysctl(2); the stdlib Sysctl cannot address a pid-parameterized MIB), parsed by the exact layout: argc, the executable path in a NUL-padded region of len(path)+1 rounded to 8, then exactly argc entries with empties preserved — never reading into the environment region and never guessing a boundary |
| [argv_linux.go](argv_linux.go) | `processArgv`, `parseCmdline` | Exact per-pid argv via `/proc/<pid>/cmdline` (NUL-separated, byte-for-byte, empty entries preserved) |

## Invariants

- Absolute executables only: `Exec` and `Runner.Run` reject a bare or
  relative argv[0], and `ExecResolved` rejects a bare or relative executed
  path — bare names never pin which binary runs (macOS path_helper PATH
  reordering). `ExecResolved` separates the executed path from the reported
  argv so the frozen check argv is never rewritten; which binary runs is
  still pinned by the absolute path alone.
- One owner of reaping per process and every teardown wait bounded: the
  leader is reaped by exactly one Wait goroutine, the anchor by the single
  owner `retireAnchor` spawns, no raw Wait4 ever touches a Cmd-managed
  pid, and a wait that exceeds its bound returns an error while its owner
  goroutine keeps the eventual reap — no path leaves a child without one.
- A group is signaled only while an unreaped member of ours pins its id:
  `Run` joins a sleep anchor into the leader's fresh group before any Wait
  exists, so the one cancellation SIGKILL can never reach a recycled group
  id; group absence is then confirmed with signal 0, which only reads
  existence. An anchor start failure proves nothing about the leader (the
  errno cannot separate an empty group from the anchor's own exec
  failure), so that path never infers completion: it retires the group
  while the leader is provably unreaped, still returns a completed
  command's own recorded result, and reports a supervision error for a
  leader that was alive. An anchor orphaned by a crashed controller keeps
  the group id pinned (for at most 100000 s, its sleep argument) so a
  successor's retirement of the recorded group cannot hit a recycled id
  either; until the check group is retired, a listing of it may therefore
  include the anchor (`<resolved sleep path> 100000`) alongside the check
  processes.
- `SignalGroup` cannot verify membership itself: the application lists and
  argv-matches the group immediately before signaling (the design's
  group-retirement rule), and the window between that listing and the
  signal, in which a fully-exited group's id could be recycled, is a
  documented residual limitation of the cooperative model — no observable
  surface carries a process start time. Both signal paths refuse pgid <= 1
  and the caller's own group, and treat ESRCH as the goal state.
- Member argv is exact, never guessed: it comes from the platform's
  byte-for-byte source (`/proc/<pid>/cmdline`; `kern.procargs2`), so
  arguments containing whitespace and empty arguments — leading ones
  included — survive exactly, and an environment string can never be
  returned as an argument (the darwin parser stops at the argc-th
  terminator and rejects a layout it does not understand rather than
  guessing a boundary; the 8-byte path-region rule is pinned by real-child
  regression tests). ps's whitespace-joined command column is never used
  for argv. A member whose argv cannot be read (raced exit, zombie,
  denied, unrecognized layout) carries the single entry `ArgvUnavailable`,
  which matches no real argv, so application-side classification fails
  closed.
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
  `app.ProcessGroupInspector` by `GroupInspector`; `Exec` (hop launch) and
  `ExecResolved` (hop check-exec) are consumed directly by the
  exec-boundary commands wired in `cmd/hop`.
- External libraries: none; standard library only (`syscall` for process
  groups, signals and the darwin sysctl).

## Verification

- `go test ./internal/adapters/process` — the fixture children are the test
  binary re-run in helper modes (an env-dumping child, a group-spawning
  child that leaves a sleeping descendant with whitespace-bearing argv, a
  never-exiting child), exercised through the real `Runner`: exit-code
  mapping, exact-environment delivery, nil-env emptiness, working
  directory, the 1 MiB capture bound, cancellation killing the whole group
  with the typed result and group-absence proof, an injected unstartable
  anchor retiring a live leader with an error, and the
  leader-exits-with-children case retired through the real
  `GroupInspector` (exact argv with spaces and tabs matched, the listing
  including the test-owned anchor, empty after retirement). Test cleanup
  never signals a group it cannot prove it owns: the cancellation test's
  cleanup cancels and waits for `Run`'s own retirement, and the inspector
  test's cleanup holds its own unreaped anchor as the identity pin and is
  disarmed before that pin is released. `Exec` is proven by a helper
  subprocess that observes the replaced image (same pid, exec-provided env
  only) plus in-process rejection and failure-return cases; `ExecResolved` by a
  helper whose replaced image reports the renamed argv[0] and tail verbatim
  with the pid preserved, plus rejection cases. Inspection
  failures are proven against injected ps stubs (nonzero exit, missing
  binary, unparseable rows) and an out-of-range pid for the
  `ArgvUnavailable` marker. The exact-argv parsers have pure tables
  (`parseProcargs2`: empty leading/middle/trailing arguments, truncation
  inside the path region or an argument, non-NUL padding; `parseCmdline`:
  empties preserved, empty buffer unavailable) and darwin real-child
  regressions proving empty-argument vectors byte-for-byte with no
  environment string ever appearing as an argument.
- `go test -run TestGitProbe ./internal/adapters/process` —
  [gitworktree_probe_test.go](gitworktree_probe_test.go), the worktree
  retirement git probe
  ([phase-3-worktree-retirement.md](../../../docs/plan/phase-3-worktree-retirement.md)):
  every git value the retirement rules consume, executed through the real
  `Runner` with an absolute git and the retirement environment against
  throwaway repositories — `git worktree remove` without force per checkout
  condition (exit status, exact refusal text, checkout and listing
  afterwards, branch kept, the `status --porcelain=v1
  --untracked-files=all` and `ls-files -v` pre-check observations), the
  three data-deleting hazards (ignored files; untracked files hidden by
  `status.showUntrackedFiles=no`, refused again under `-c
  status.showUntrackedFiles=all`; modifications hidden by
  assume-unchanged/skip-worktree), missing and symlinked checkout paths,
  the `worktree list --porcelain -z` record shapes, and the target/ancestry
  exit statuses. Skips with a reason when no git is on PATH.
- Test fixtures: none on disk; helper modes and ps stubs are written by the
  tests.

## Related guides

- [Parent index](../AGENTS.md)
- [Application layer and ports](../../app/AGENTS.md)
- [Phase 2 design, sections 3, 6 and 7](../../../docs/plan/phase-2-design.md)
