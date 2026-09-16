# internal/domain/run

## Purpose

The run domain module: `Run`, `Task`, `Attempt`, `Session`,
`RuntimeBinding`, `Worktree`, `Result`, `Artifact`, `Message`/`Delivery`/
`Ack`, `Review` and `Integration` entities plus their pure state
transitions, implementing the exhaustive state tables, the task
dependency graph, the messaging and review acceptance rules and the run
completion guard of [phase-2-design.md](../../../docs/plan/phase-2-design.md)
(sections 2, 5, 7) and [phase-3-design.md](../../../docs/plan/phase-3-design.md)
(sections 2, 5, 7, 8). Phase 2 gives every run exactly one task, no
dependencies and no manager; Phase 3 extends the same entities additively
— task kinds, a dependency graph, multiple attempts, session roles and
one-level delegation, durable messages, an independent review and serial
integration — without changing any Phase 2 transition's legality.
`Playbook` and `Account` entities are out of scope here.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [run.go](run.go) | `Run`, `RunState`, `NewRun`, `Launch`, `MarkRunning`, `EnterCompleting`, `Complete`, `Fail`, `RequestStop`, `MarkStopped`, `EnterResuming`, `CanAcceptManagerVerb`, `ClosePlan`, `ReopenPlan` | Run's state machine (section 5, "Run"); stop-precedence-guarded completion; monotonic stop request; the feature-mode plan-closure flag (`PlanClosed`) manager verbs gate on |
| [task.go](task.go) | `Task`, `TaskKind`, `TaskState`, `NewTask`, `NewImplementTask`, `NewReviewTask`, `Activate`, `Release`, `Reopen`, `EnterChecking`, `Complete`, `EnterIntegrating`, `Integrate`, `NeedsRework`, `Fail`, `Interrupt`, `CloseMailbox`, `ReopenMailbox` | Task's two kind-dispatched state machines (`TaskKindImplement`, `TaskKindReview`; section 5, "Task (kind = implement/review)") |
| [dependency.go](dependency.go) | `TaskDependency`, `NewTaskDependency`, `ValidateAcyclic`, `ReleaseEligible` | The task dependency graph: acyclicity validation and the release rule (every prerequisite `integrated`) |
| [attempt.go](attempt.go) | `Attempt`, `AttemptState`, `NewAttempt`, `NewRetryAttempt`, `Launch`, `MarkRunning`, `Submit`, `EnterChecking`, `Complete`, `Fail`, `Interrupt`, `Reconcile`, `Relaunch`, `Reattach`, `CompleteReview` | Attempt's state machine (section 5, "Attempt"), including cold-relaunch/warm-reattach transitions, multi-attempt retry reservation and the review-only `CompleteReview` row |
| [session.go](session.go) | `Session`, `SessionState`, `Role`, `Harness`, `NativeRefSource`, `NewSession`, `NewManagerSession`, `NewChildSession`, `AssignNativeRef`, `Launch`, `ConfirmActive`, `Reconcile`, `MarkLost`, `Stop`, `Terminate` | Session's state machine (section 5, "Session") and its roles: `RoleWorker` (Phase 2/solo), `RoleManager`, `RoleImplementer`, `RoleReviewer` (Phase 3), with one-level delegation |
| [binding.go](binding.go) | `RuntimeBinding`, `LaunchKind`, `OccupantEvidence`, `NewRuntimeBinding`, `Observe`, `Supersede` | Append-only runtime placement history and evidence-gated supersession |
| [worktree.go](worktree.go) | `Worktree`, `WorktreeState` (`WorktreeActive`, `WorktreeRemoved`, `WorktreeAbsent`, `WorktreeReleased`), `NewWorktree`, `NewAttemptWorktree`, `Retire` | Checkout provenance: one unlinked row per solo run (`NewWorktree`), one per attempt in feature mode (`NewAttemptWorktree`, carrying `AttemptID` and the verified `BaseCommit`); post-merge worktree retirement's final states ([phase-3-worktree-retirement.md](../../../docs/plan/phase-3-worktree-retirement.md) section 8) |
| [result.go](result.go) | `Result`, `ResultSubmission`, `AcceptanceContext`, `AcceptanceOutcome`, `AcceptResult` | The section 7 result-acceptance rule as one pure, cross-entity function |
| [message.go](message.go) | `Message`, `MessageKind`, `MessageState`, `Principal`, `Address`, `Delivery`, `Ack`, `AckContext`, `AckOutcome`, `AnswerSubmission`, `AnswerOutcome`, `NewQuestion`, `NewInfo`, `Deliver`, `AcceptAck`, `NextDeliverable`, `AcceptAnswer`, `ResolveOrigin`, `ValidateSendAddressing` | The durable message/delivery/ack model (section 7): the `queued`→`delivered`→`acknowledged` machine, FIFO selection, ack eligibility and the derived-destination answer rule |
| [review.go](review.go) | `Review`, `Verdict`, `ReviewSubmission`, `ReviewAcceptanceContext`, `VerdictOutcome`, `AcceptVerdict` | The section 8 review-verdict acceptance rule, mirroring `AcceptResult`'s receipt-before-eligibility order |
| [integration.go](integration.go) | `Integration`, `IntegrationState`, `NewIntegration`, `EnterChecking`, `Conflict`, `Integrate`, `FailCheck`, `RollBack`, `Interrupt` | Serial per-task integration's state machine (section 5, "Integration") |
| [readiness.go](readiness.go) | `GuardContext`, `CheckReceipt`, `GuardShortfall`, `ShortfallKind`, `EvaluateReadiness` | The run-completion guard (section 8): a pure function meant to gate `Run.Complete` in feature mode (the actual wiring is a later application slice — see Invariants) |
| [artifact.go](artifact.go) | `Artifact`, `ArtifactKind`, `NewArtifact`, `NewResultArtifact` | File references owned by a run or a result |
| [errors.go](errors.go) | `ErrInvalidTransition`, `ErrStaleSubmission`, `ErrConflictingResult`, `ErrDuplicateResult`, `ErrTransientNotRunning`, `ErrDependencyCycle`, `ErrDependencyNotIntegrated`, `ErrDependencyEvidenceMissing`, `ErrDelegationDepth`, `ErrDuplicateAnswer`, `ErrConflictingAnswer`, `ErrStaleAck`, `ErrNotDelivered`, `ErrVerdictSubjectMismatch`, `ErrRetryNotTerminal`, `ErrRetryLimit`, `ErrRunNotAccepting`, `ErrEmptyPlan`, `ErrMailboxClosed`, `ErrMailboxNotClear`, `ErrRequestConflict`, `ErrTaskNotReleased` | Typed errors every transition and cross-entity acceptance function returns |
| [transition.go](transition.go) | `transitionTable`, `fromAny`, `concatPairs` | The generic, table-driven legality check shared by every entity's state machine |

## Invariants

- The section 5 state tables are exhaustive: a (from, to) pair a table does
  not list is `ErrInvalidTransition`, proven by an independent
  transcription of every table in the test suite rather than by checking
  the production table against itself. Phase 3 only ever ADDS rows to the
  Phase 2 tables it extends (Task, and Attempt's `CompleteReview` row); no
  previously valid pair became invalid, and the Phase 2 test suite's own
  transcriptions still pass unmodified.
- Multiple causes reaching the same target state share one function (for
  example `Run.MarkRunning` covers both "claim settled" and a repeatable
  check's requeue); the causal detail belongs to the application's
  operation/transitions journal, not to the domain — EXCEPT
  `Attempt.Reattach` and `Attempt.CompleteReview`, both independently
  validated rather than sharing their target state's other transition
  functions' tables: `Reattach` restricts `reconciling`'s three reattach
  targets so `MarkRunning`/`Submit`/`EnterChecking` cannot also succeed
  from `reconciling`; `CompleteReview` requires `TaskKindReview` and source
  state `submitted`, since a review attempt's settling evidence is the
  verdict row itself — there is no check phase to share with `Complete`'s
  `checking`→`completed` row.
- `Run.StopRequested` is monotonic: `RequestStop` never withdraws it, and
  moves `Run.State` to `stopping` only from a state section 5 lists as a
  valid source; from any other state, including a terminal one, the request
  is recorded without changing state.
- Stop takes precedence over completion in every settling transition:
  `Run.Complete` refuses (`ErrInvalidTransition`) once `StopRequested` is
  set, even from `completing`, the table's otherwise-valid source.
- `Run.PlanClosed` is feature mode's durable plan-closure flag: `ClosePlan`
  sets it (refusing `ErrEmptyPlan` for zero implement tasks) and
  `ReopenPlan` clears it; both are additive to Phase 2 — solo runs never
  call either and never consult the flag. Every manager verb (`ClosePlan`
  included) first calls the shared `CanAcceptManagerVerb`
  (`ErrRunNotAccepting` outside `running`).
- `Task.Kind` dispatches which of two exhaustive tables `Task.transition`
  validates against: `TaskKindReview` uses the shorter review table
  (`ready`→`active`→`completed`, no `checking`/`integrating`/`integrated`);
  every other `Kind`, INCLUDING THE PHASE 2 ZERO VALUE, uses the implement
  table — this is why Phase 2's `NewTask`-built tasks keep working
  unmodified. `Task.Activate` special-cases `pending`→`active` outside
  either table: legal directly, unconditionally, for a task with no
  dependencies (`HasDependencies == false`, the Phase 2/solo shape, since
  Phase 2 never populates `HasDependencies` at all); for a DEPENDENT task,
  refused (`ErrTaskNotReleased`) until `Task.Release` has moved it to
  `ready` first. `HasDependencies` is a plain, comparable flag — `Task`
  stores no `[]identity.TaskID` field (that would make `Task`
  incomparable, breaking every Phase 2 test's `==`/`!=` use of it) — but
  it is NOT merely Activate's own concern: `Release` and `ReleaseEligible`
  both check it FIRST, requiring it to agree with whether the supplied
  edges (the task's own persisted `TaskDependency` rows — one naming a
  different `TaskID` is rejected outright) is empty or not
  (`ErrDependencyEvidenceMissing` from `Release` on a mismatch — a
  dependent task given zero edges, or a zero-dependency task given any).
  Without this check, an empty edge set would be indistinguishable from
  "genuinely zero dependencies" and satisfy a dependent task vacuously.
  Only once shapes agree does `ReleaseEligible` check that prerequisites
  accounts for
  EXACTLY the IDs edges name — no missing, no foreign (an ID edges never
  named), no duplicate — with every one `integrated`
  (`ErrDependencyNotIntegrated` otherwise). `Release` never accepts a bare
  caller-asserted eligibility boolean; it computes eligibility itself.
  A zero-dependency task is created `ready` directly (`NewImplementTask`)
  and never calls `Release` in practice.
- `TaskDependency` edges are immutable once created (a task's dependency
  set is fixed at creation); `ValidateAcyclic` checks one candidate edge
  against the run's persisted edge set by graph reachability, not by
  reproving the whole graph from scratch.
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
  `AcceptVerdict` mirrors this exact order for a review submission
  (reusing `ErrDuplicateResult`/`ErrConflictingResult`, since section 8
  frames verdict duplication/conflict as the same transplanted rule), adds
  the mailbox-clear (`ErrMailboxNotClear`) and frozen-subject
  (`ErrVerdictSubjectMismatch`) checks, and drives `Attempt.Submit` then
  `Attempt.CompleteReview` (never `EnterChecking`/`Complete`) plus
  `Task.Complete` — approve and reject both complete the review task; the
  verdict's content gates run readiness, not the task's own state.
- A session's native reference is immutable once assigned: `AssignNativeRef`
  refuses a second call, even with an identical value.
- A feature-mode worktree names the attempt it was created for and the
  base commit it was verified against: `NewAttemptWorktree` refuses an
  empty value for either (`ErrInvalidTransition`), since an unlinked
  feature row looks exactly like a solo row and the launch boundary could
  not find it by attempt. `NewWorktree` builds the solo shape, with both
  links empty.
- A worktree leaves `active` only through `Worktree.Retire`, to exactly one
  of the final states `removed`, `absent` or `released`; a final state is
  never left, and any other target is `ErrInvalidTransition`. Stop and
  failure never move a worktree.
- `Session.AttemptID` is optional in the sense that its zero value (the
  empty string) means "no attempt" — the manager session's own shape,
  since `identity.AttemptID` is a plain string type and Phase 2 code
  outside this package already constructs and reads the field directly by
  value, never through a pointer. `Session.ParentSessionID` (`*identity.
  SessionID`) is genuinely nilable: nil for the manager and for every
  Phase 2 `RoleWorker` session, set for a Phase 3 child. `NewChildSession`
  enforces every fact about a candidate parent that a single `Session`
  value can attest to on its own: no parent of its own
  (`ErrDelegationDepth` otherwise — a session with a parent can never
  itself be a parent), `RoleManager`, the SAME `RunID` as the child, a
  well-formed manager shape (empty `AttemptID`, nil `ParentSessionID`) and
  a non-terminal state — any of the latter four is `ErrInvalidTransition`.
  The new child's own role must be `RoleImplementer` or `RoleReviewer`
  (also `ErrInvalidTransition`). That the supplied parent is actually the
  run's SOLE current (non-terminal) manager among every session — as
  opposed to merely A well-formed, same-run, non-terminal manager, which
  is everything one `Session` value can ever prove — is a cross-session
  uniqueness fact only the application, querying every session, can
  validate before calling this constructor.
- A runtime binding's observations and supersession are valid only against
  a current (non-superseded) binding; supersession requires non-empty
  recorded evidence and succeeds at most once.
- `Message.State` is NOT a persisted envelope field (the `messages` row is
  immutable: sender, recipient, kind, reply-to, relay provenance, enqueue
  sequence, request ID, body reference, created-at only) — it is the
  application's reconstructed view over the message's delivery/ack rows,
  threaded through this package's functions like any other entity's state.
  `NextDeliverable` selects, from one recipient address's queue, the
  in-flight delivered-unacknowledged message if one exists, else the
  queued message with the lowest `EnqueueSeq` (commit order, never a
  caller clock). `AcceptAck`'s first check is a repeat ack
  (`priorAck != nil`): idempotent success regardless of incarnation,
  checked before delivery/incarnation eligibility (receipt-before-
  eligibility); a first ack requires a delivery row for the ACKING SESSION
  ITSELF (`ErrNotDelivered` otherwise — a predecessor's delivery never
  authorizes a successor's ack) at its current incarnation
  (`ErrStaleAck` otherwise). `AcceptAnswer` resolves any prior accepted
  answer first (`ErrDuplicateAnswer`/`ErrConflictingAnswer`, the accepted
  answer never disturbed) and otherwise imposes NO delivery-state
  precondition on the question being answered — an ordinary (non-human)
  question's own ack is independent of when it is eventually answered
  (the manager typically acks on read, before composing its reply). The
  new answer's own acceptance bundles the QUESTION's ack
  (`queued`/`delivered`→`acknowledged` directly) ONLY when the question
  was human-addressed — the one row section 5's delivery table allows
  outside the ordinary fetch-then-ack path, since a human question is
  never fetched. `ValidateSendAddressing` covers only `question`/`info`
  sends (an answer's destination is derived, validated by `AcceptAnswer`
  instead) and enforces the section 7 legality matrix by the SENDER'S OWN
  logical `Address` (resolved by the application from its role/task), not
  its raw `Principal`.
- `EvaluateReadiness` is a pure function over an application-assembled
  `GuardContext` (the plan flag, every implement task, the current
  integration head's object IDs, the most recent `CheckReceipt`, if any,
  and the most recently accepted review verdict, whatever either one's
  subject or value). It computes BOTH the check's and the verdict's
  subject match itself by comparing object IDs against the head — never
  trusting a bare `Passed`/pre-filtered boolean, and never trusting a
  passing receipt or an approve bound to a superseded head — and reports
  EVERY unsatisfied guard, not just the first. `Task.transition`'s tables
  and `AcceptResult`/`AcceptVerdict` are themselves domain-enforced;
  `EvaluateReadiness` being THE gate `Run.Complete` actually goes through
  is application wiring this slice does not build — `Run.Complete`
  (run.go) has no feature-mode or readiness argument of its own and
  remains callable directly from `completing` exactly as Phase 2 left it.
  A later slice wires the controller's completion transaction to call
  `EvaluateReadiness` before ever calling `Run.Complete`; this package
  only guarantees that the FUNCTION ITSELF is a correct, evidence-based
  guard, not that every caller of `Run.Complete` uses it.
- No entity carries a persistence revision/version field: optimistic-
  concurrency bookkeeping is the SQLite adapter's job (section 4's
  `revision INTEGER NOT NULL` schema column). Repositories are expected to
  return a domain value alongside its current revision and accept
  `Save(entity, expectedRevision)`; tasks 2 and 3 share this shape without
  the domain carrying it. `Message`, `Delivery`, `Ack`, `Review` rows are
  append-only/immutable and carry no revision at all.
- `UpdatedAt` on `Run`/`Task`/`Attempt`/`Session`/`Integration` is set from
  each transition's explicit `now` and is descriptive only; it never
  participates in a legality decision. `Message`, `Delivery`, `Ack`,
  `Review` and `TaskDependency` carry no `UpdatedAt` (their rows are
  immutable once written; only `Message.State` is a reconstructed view,
  covered above).
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
  test), for BOTH `Task` kinds; stop monotonicity and precedence;
  `Attempt.Reattach`'s restricted target set and `Attempt.CompleteReview`'s
  kind/state restriction; `NewRetryAttempt`'s terminal/limit rules; the
  dependency graph's acyclicity and `ReleaseEligible`'s bypass vectors
  (missing, foreign and duplicate prerequisites, a foreign edge naming a
  different task, an empty prerequisite set against a real dependency,
  and edges disagreeing with `HasDependencies` in either direction);
  `NewAttemptWorktree`'s required attempt and base-commit links;
  `Worktree.Retire`'s table (`TestWorktreeRetireTransitions`: every state
  pair plus unknown targets, each final state only from active);
  `NewChildSession`'s delegation-depth, role and full parent-shape checks
  (worker parent, cross-run manager, malformed attempt-bound manager,
  terminated manager); runtime binding observation/supersession;
  `AcceptResult`'s and `AcceptVerdict`'s outcomes (ordinary and
  early-submission acceptance in both race orders, transient, stale by
  incarnation/stop/state, duplicate in every state including terminal,
  conflicting never disturbing the accepted row, plus `AcceptVerdict`'s own
  mailbox-clear and subject-match guards); `AcceptAck`/`NextDeliverable`/
  `AcceptAnswer` order and vectors (duplicate/conflicting, stale/not-
  delivered, the human-question ack bundling); `EvaluateReadiness`'s
  shortfall vectors (plan open, missing integration, check missing, a
  failing check, a stale/head-moved passing check, no verdict, reject
  present, stale-subject approve, every shortfall reported together).
- `go test -run TestReferenceTrace ./internal/domain/run` — the Phase 2
  section 5 "Reference traces" (four exact event orders, plus trace 1's
  controller-observes-first/acceptance-wins-first race) AND the Phase 3
  section 5 traces (six more: feature happy path with dependency release,
  a relayed question with no lifecycle movement, duplicate/ambiguous
  delivery across a cold relaunch, worker interruption with two-attempt
  provenance plus the exhaustion-to-failed variant, reviewer rejection and
  re-review, and stop precedence in both integration windows), all
  exercised end to end through these functions.

## Related guides

- [Parent index](../AGENTS.md)
- [internal/domain/identity](../identity/AGENTS.md)
- [Phase 2 design, sections 2, 5, 7](../../../docs/plan/phase-2-design.md)
- [Phase 3 design, sections 2, 5, 7, 8](../../../docs/plan/phase-3-design.md)
- [Domain model](../../../docs/architecture/domain-model.md)
