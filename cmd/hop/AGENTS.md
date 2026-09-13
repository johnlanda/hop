# cmd/hop

## Purpose

Composition root of the `hop` binary. It parses the command line with the
standard `flag` package, constructs the concrete Herdr adapter where a
command needs one, dispatches to the selected command and owns the process
exit codes. It holds no business rule: `version` reports the build, `doctor`
wires the installation probe into the application's doctor use case, and
`plugin-context` reports the plugin invocation environment.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [main.go](main.go) | `main`, `run`, `dispatch`, `printUsage`, `exitOK`, `exitFailure`, `exitUsage` | Entry point; maps a command name to its handler and a failed write to `exitFailure` |
| [version.go](version.go) | `version`, `devVersion`, `recordingWriter`, `runVersion`, `resolveVersion`, `shortRevision` | `hop version`: prints the resolved version with the Go version and platform of the build; flag diagnostics pass through `recordingWriter` so their write failures are reported too |
| [doctor.go](doctor.go) | `runDoctor`, `renderReport`, `defaultDoctorTimeout` | `hop doctor`: wires `herdr.InstallationProbe` into `app.Doctor`, prints one line per check plus advice, exits 1 on an unhealthy report; `-herdr` and `-socket` default from `HERDR_BIN_PATH` and `HERDR_SOCKET_PATH` |
| [plugincontext.go](plugincontext.go) | `runPluginContext` | `hop plugin-context`: validates and prints the injected plugin invocation; the manifest's startup hook and context action |
| [main_test.go](main_test.go) | `TestRun`, `TestRunReportsWriteFailuresThroughTheExitCode`, `TestResolveVersion` | Table-driven behavior of dispatch, exit codes and version precedence |
| [doctor_test.go](doctor_test.go), [plugincontext_test.go](plugincontext_test.go) | `TestRunDoctor*`, `TestRunPluginContext*` | Command behavior with injected getenv/environ and stub executables; nothing reaches a live Herdr session |

The repository root's [herdr-plugin.toml](../../herdr-plugin.toml) declares
how Herdr invokes this binary: a startup hook and a `show-context` action run
`plugin-context`, a `doctor` action and a pane entrypoint run `doctor`. Its
commands expect the built binary at `.bin/hop` (see `make build`).

## Invariants

- Exit codes: 0 success, 1 failure (a write to stdout or stderr failed, an
  unhealthy doctor report, or an invocation outside a plugin command), 2
  usage error. Usage text goes to stderr on an error and to stdout for
  `help`. A write failure takes precedence over a usage error, including
  when the failed write is a flag diagnostic produced while parsing.
- Version precedence: a value stamped with `-ldflags "-X main.version=..."`,
  then the main module version recorded in build information, then
  `devel+<revision>` (suffixed `.modified` for a dirty tree), then `devel`.
- `version` is the only package-level variable; it exists for `-X` and is
  never written at run time.
- Commands stay thin: environment reading, flag parsing and rendering live
  here; decisions live in [internal/app](../../internal/app/AGENTS.md).
- Every doctor run is bounded by one context deadline (default 10s).
- No CLI framework and no third-party dependency.

## Dependencies and ports

- Allowed inward imports: [internal/app](../../internal/app/AGENTS.md) and
  [internal/adapters/herdr](../../internal/adapters/herdr/AGENTS.md), per
  rule `cmd/hop` in [internal/arch_test.go](../../internal/arch_test.go).
- Consumed/implemented ports: constructs `herdr.InstallationProbe` and
  injects it as `app.Doctor`'s `Probe`.
- External libraries: none; standard library only.

## Verification

- `go test ./cmd/hop` — dispatch, exit codes, write-failure handling,
  version precedence, doctor rendering against stub binaries and
  plugin-context behavior against fixed environ slices.
- `make build && .bin/hop version` — builds the binary and prints the version
  the Go toolchain derived from version control.
- `.bin/hop doctor` — probes the real installation on this machine.
- Test fixtures: stub shell scripts written to temporary directories; no
  live Herdr session is contacted.

## Related guides

- [Parent index](../AGENTS.md)
- [Root guide](../../AGENTS.md)
- [Application layer](../../internal/app/AGENTS.md)
- [Herdr adapter](../../internal/adapters/herdr/AGENTS.md)
