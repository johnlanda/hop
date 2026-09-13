# cmd/hop

## Purpose

Composition root of the `hop` binary. It parses the command line with the
standard `flag` package, dispatches to the selected command and owns the
process exit codes. It holds no business rule and wires no adapter yet: the
only command is `hop version`.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [main.go](main.go) | `main`, `run`, `dispatch`, `printUsage`, `exitOK`, `exitFailure`, `exitUsage` | Entry point; maps a command name to its handler and a failed write to `exitFailure` |
| [version.go](version.go) | `version`, `devVersion`, `recordingWriter`, `runVersion`, `resolveVersion`, `shortRevision` | `hop version`: prints the resolved version with the Go version and platform of the build; flag diagnostics pass through `recordingWriter` so their write failures are reported too |
| [main_test.go](main_test.go) | `TestRun`, `TestRunReportsWriteFailuresThroughTheExitCode`, `TestResolveVersion` | Table-driven behavior of dispatch, exit codes and version precedence |

## Invariants

- Exit codes: 0 success, 1 a write to stdout or stderr failed, 2 usage error.
  Usage text goes to stderr on an error and to stdout for `help`. A write
  failure takes precedence over a usage error, including when the failed
  write is a flag diagnostic produced while parsing.
- Version precedence: a value stamped with `-ldflags "-X main.version=..."`,
  then the main module version recorded in build information, then
  `devel+<revision>` (suffixed `.modified` for a dirty tree), then `devel`.
- `version` is the only package-level variable; it exists for `-X` and is
  never written at run time.
- No CLI framework and no third-party dependency. Commands stay thin and will
  call application services once those exist.

## Dependencies and ports

- Allowed inward imports: none yet. Rule `cmd/hop` in
  [internal/arch_test.go](../../internal/arch_test.go) has category
  `composition` with empty first-party and third-party allowlists; extend it
  in the change that introduces the application package.
- Consumed/implemented ports: none.
- External libraries: none; standard library only.

## Verification

- `go test ./cmd/hop` — dispatch, exit codes, write-failure handling and
  version precedence.
- `make build && .bin/hop version` — builds the binary and prints the version
  the Go toolchain derived from version control.
- `make build VERSION=v1.2.3 && .bin/hop version` — a stamped version wins.
- Test fixtures: none on disk; tests use in-memory writers and constructed
  `debug.BuildInfo` values.

## Related guides

- [Parent index](../AGENTS.md)
- [Root guide](../../AGENTS.md)
- [Architecture and guide checkers](../../internal/AGENTS.md)
