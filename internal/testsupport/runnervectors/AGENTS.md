# internal/testsupport/runnervectors

## Purpose

The `app.CommandRunner` capture contract, expressed once. Each captured
stream is bounded (1 MiB by default, or the command's own
`MaxOutputBytes`). A stream is flagged truncated exactly when bytes were
discarded, and a negative bound is refused before anything runs.

The real process runner (`internal/adapters/process`) and every
handwritten `CommandRunner` fake (`internal/app`'s `fakeCommands`,
`internal/adapters/sqlite`'s `linkGit`) run the IDENTICAL vectors against
their own implementation. The fakes also apply `BoundCapture`, the
reference implementation, to their answers, so a fake cannot silently
accept a bound the runner refuses or return output the runner would
have cut. This is the countermeasure for the escaped-defect class "a fake
accepted arguments the real adapter refuses".

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [runnervectors.go](runnervectors.go) | `DefaultCaptureBytes`, `Fill`, `CaptureVector`, `CaptureVectors`, `CaptureVector.Answer`, `CaptureVector.Check` | The vectors. Each one is a command writing an exact number of `Fill` bytes to each stream, with an exit code and a bound, and states the kept lengths, the flags, or the refusal. The cases: small and empty output; exactly the default bound (complete) and one byte past it (truncated); truncation on a successful and on a failing exit; each stream flagged alone and both together; a per-call bound above the default (output past 1 MiB kept), exactly at it and one byte past it; a per-call bound applied to each stream separately; bounds of 100 bytes and 1 byte; and a negative bound refused. `Answer` is the complete answer a fake returns before bounding. `Check` returns what a runner's result got wrong, or nil: the exit code, exactly the kept `Fill` prefix of each stream, and the flags, or an error for a refused vector |
| [runnervectors.go](runnervectors.go) | `ValidateBound`, `ErrNegativeBound`, `BoundCapture` | The reference contract a fake applies. `ValidateBound` refuses a negative bound, for use before answering. `BoundCapture` keeps each stream's first bound bytes and sets the flags; an answer claiming a truncation with anything but exactly the bound is refused as a fake misuse |

## Invariants

- This is a vector package in this subtree's sense: it never runs a
  command, opens a store or asserts through `testing`. `Check` returns an
  error for the consuming test to report.
- `BoundCapture` is pure, with no state; it is the one place a fake's
  bound lives.
- A new handwritten `CommandRunner` applies `ValidateBound` before
  answering and `BoundCapture` to its answer, and runs `CaptureVectors`
  in its own tests.
- The real runner is never implemented through this package: its bound
  is its own `boundedBuffer`, and the vectors are what prove the two
  agree.

## Dependencies and ports

- Allowed inward imports: [internal/app](../../app/AGENTS.md).
- Consumed/implemented ports: none. It builds `app.CommandResult` values
  and checks results of `app.CommandRunner.Run`; it implements no port.
- External libraries: none.

## Verification

- `go test ./internal/adapters/process -run TestRunnerReportsTruncation`:
  every vector through the real `Runner`, via the `flood` helper child.
  A refused vector's child never starts; this is checked with a marker
  file.
- `go test ./internal/app -run TestFakeCommandsBoundCapturedOutput`:
  every vector through `fakeCommands`, both scripted and hooked. A
  refused vector is never answered.
- `go test ./internal/adapters/sqlite -run TestLinkGitCaptureContract`:
  every vector through `linkGit`, plus its own common-directory answer
  under a negative bound, a one-byte bound and its exact length.
- This package itself has no `_test.go` file. The vectors mean nothing
  apart from a runner, and the three consuming tests above are their
  assertions.

## Related guides

- [Parent index](../AGENTS.md)
- [internal/adapters/process](../../adapters/process/AGENTS.md),
  [internal/app](../../app/AGENTS.md) and
  [internal/adapters/sqlite](../../adapters/sqlite/AGENTS.md): the
  consumers.
- [Architecture and package catalog](../../../docs/architecture/architecture.md)
