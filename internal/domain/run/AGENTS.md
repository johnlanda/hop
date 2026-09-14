# internal/domain/run

## Purpose

The run domain module: `Run`, `Task`, `Attempt`, `Session`,
`RuntimeBinding`, `Worktree`, `Result` and `Artifact` entities plus their
pure state transitions, implementing the exhaustive state tables and
result-acceptance rule of
[phase-2-design.md](../../../docs/plan/phase-2-design.md) sections 2 and 5.
Phase 2 gives every run exactly one task; `Message`, `Playbook` and
`Account` entities are out of scope here.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [run.go](run.go) | `Run`, `RunState`, `NewRun`, `Launch`, `MarkRunning`, `EnterCompleting`, `Complete`, `Fail`, `RequestStop`, `MarkStopped`, `EnterResuming` | Run's state machine (section 5, "Run"); stop-precedence-guarded completion; monotonic stop request |
| [task.go](task.go) | `Task`, `TaskState`, `NewTask`, `Activate`, `EnterChecking`, `Complete`, `Fail`, `Interrupt` | Task's state machine (section 5, "Task") |
| [attempt.go](attempt.go) | `Attempt`, `AttemptState`, `NewAttempt`, `Launch`, `MarkRunning`, `Submit`, `EnterChecking`, `Complete`, `Fail`, `Interrupt`, `Reconcile`, `Relaunch`, `Reattach` | Attempt's state machine (section 5, "Attempt"), including cold-relaunch and warm-reattach transitions |
| [session.go](session.go) | `Session`, `SessionState`, `Role`, `Harness`, `NativeRefSource`, `NewSession`, `AssignNativeRef`, `Launch`, `ConfirmActive`, `Reconcile`, `MarkLost`, `Stop`, `Terminate` | Session's state machine (section 5, "Session") and the immutable-once-assigned native reference |
| [binding.go](binding.go) | `RuntimeBinding`, `LaunchKind`, `OccupantEvidence`, `NewRuntimeBinding`, `Observe`, `Supersede` | Append-only runtime placement history and evidence-gated supersession |
| [worktree.go](worktree.go) | `Worktree`, `WorktreeState`, `NewWorktree` | Checkout provenance; Phase 2 reaches only the active state |
| [result.go](result.go) | `Result`, `ResultSubmission`, `AcceptanceContext`, `AcceptanceOutcome`, `AcceptResult` | The section 7 result-acceptance rule as one pure, cross-entity function |
| [artifact.go](artifact.go) | `Artifact`, `ArtifactKind`, `NewArtifact`, `NewResultArtifact` | File references owned by a run or a result |
| [errors.go](errors.go) | `ErrInvalidTransition`, `ErrStaleSubmission`, `ErrConflictingResult`, `ErrDuplicateResult`, `ErrTransientNotRunning` | Typed errors every transition and `AcceptResult` return |
| [transition.go](transition.go) | `transitionTable`, `fromAny`, `concatPairs` | The generic, table-driven legality check shared by every entity's state machine |

## Invariants

- The section 5 state tables are exhaustive: a (from, to) pair a table does
  not list is `ErrInvalidTransition`, proven by an independent
  transcription of every table in the test suite rather than by checking
  the production table against itself.
- Multiple causes reaching the same target state share one function (for
  example `Run.MarkRunning` covers both "claim settled" and a repeatable
  check's requeue); the causal detail belongs to the application's
  operation/transitions journal, not to the domain — EXCEPT
  `Attempt.Reattach`, which section 5 restricts to `running`, `submitted`
  or `checking` from `reconciling` only, and which therefore validates
  independently of `MarkRunning`/`Submit`/`EnterChecking` rather than
  sharing their transition table (sharing it would let those functions also
  succeed from `reconciling`, conflating "claim settled" with "warm
  reattach verified").
- `Run.StopRequested` is monotonic: `RequestStop` never withdraws it, and
  moves `Run.State` to `stopping` only from a state section 5 lists as a
  valid source; from any other state, including a terminal one, the request
  is recorded without changing state.
- Stop takes precedence over completion in every settling transition:
  `Run.Complete` refuses (`ErrInvalidTransition`) once `StopRequested` is
  set, even from `completing`, the table's otherwise-valid source.
- `AcceptResult` is a pure function of the current `Run`, `Task`, `Attempt`,
  any prior accepted `Result`, an application-assembled `AcceptanceContext`
  and an opaque, already-validated/canonicalized digest string
  (`ResultSubmission.Digest`); it never hashes and reads no clock beyond the
  explicit `now`. Validation order is normative: resolve a prior accepted
  result before any state or incarnation precondition (an equal digest is
  `ErrDuplicateResult`, idempotent in every attempt state including
  terminal ones; an unequal digest is `ErrConflictingResult` and never
  disturbs the accepted result); only then check eligibility (current
  incarnation, no run stop request, attempt `running` or
  `launching`/`relaunching` with a settled launch claim —
  `ErrTransientNotRunning` for an unsettled claim there,
  `ErrStaleSubmission` for every other case). On acceptance, `Attempt` moves
  to `submitted`, `Task` to `checking`, and — when the run has not yet
  reached it — `Run` from `launching` to `running`, atomically.
- A session's native reference is immutable once assigned: `AssignNativeRef`
  refuses a second call, even with an identical value.
- A runtime binding's observations and supersession are valid only against
  a current (non-superseded) binding; supersession requires non-empty
  recorded evidence and succeeds at most once.
- No entity carries a persistence revision/version field: optimistic-
  concurrency bookkeeping is the SQLite adapter's job (section 4's
  `revision INTEGER NOT NULL` schema column). Repositories are expected to
  return a domain value alongside its current revision and accept
  `Save(entity, expectedRevision)`; tasks 2 and 3 share this shape without
  the domain carrying it.
- `UpdatedAt` on `Run`/`Task`/`Attempt`/`Session` is set from each
  transition's explicit `now` and is descriptive only; it never
  participates in a legality decision.
- `Session.AttemptID` is not optional: Phase 2 has no manager session. A
  manager session (Phase 3) will revisit this field's optionality.
- Domain code never reads a clock, generates an identity or computes a
  digest; every transition takes its `now` and every identity/digest as an
  explicit input.

## Dependencies and ports

- Allowed inward imports: [internal/domain/identity](../identity/AGENTS.md)
  only.
- Consumed/implemented ports: none; this package has no ports of its own.
  `internal/app` (task 2) consumes these types directly as its domain
  layer.
- External libraries: none.

## Verification

- `go test ./internal/domain/run` — table-driven transition tests for every
  entity, each checked against every state pair from an independently
  transcribed copy of its section 5 table (not the production table under
  test); stop monotonicity and precedence; `Attempt.Reattach`'s restricted
  target set; runtime binding observation/supersession; `AcceptResult`'s
  outcomes (ordinary and early-submission acceptance in both race orders,
  transient, stale by incarnation, stale by stop request, stale by attempt
  state, duplicate in every attempt state including terminal, conflicting
  never disturbing the accepted result).
- `go test -run TestReferenceTrace ./internal/domain/run` — the section 5
  "Reference traces": the four exact four-entity event orders (plus trace
  1's controller-observes-first/acceptance-wins-first race) exercised end
  to end through these functions.

## Related guides

- [Parent index](../AGENTS.md)
- [internal/domain/identity](../identity/AGENTS.md)
- [Phase 2 design, sections 2, 5, 7](../../../docs/plan/phase-2-design.md)
- [Domain model](../../../docs/architecture/domain-model.md)
