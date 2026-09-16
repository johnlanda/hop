# internal/testsupport/storevectors

## Purpose

Shared refused-input vectors for Phase 3's worker-authority store ports
(`app.MessagingStore`, `app.PlanStore`, `app.ReviewStore`) and the
controller unit of work's `app.WorktreeRepository`: request
shapes a correct store must refuse, expressed once so both
`internal/app`'s `fakeStore` tests and `internal/adapters/sqlite`'s
real-store tests exercise the
IDENTICAL input against their own backing implementation. This is the
section 11 countermeasure for the Phase 2 escaped-defect class "a fake
accepted arguments the real adapter refuses" — a shape one implementation
happens to accept and the other genuinely refuses is exactly the drift a
shared vector catches, since both sides run the same input.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [storevectors.go](storevectors.go) | `TaskCreateSelfDependency`, `TaskCreateRequestIDConflictFirst`/`Second`, `TaskCreateNonManagerCaller`, `TaskCreateOversizedTitle`, `AckMessageStaleIncarnation`, `AckMessageUnknownMessage`, `AckMessageNotDelivered`, `MessageSendAnswerUnknownQuestion`, `MessageSendCrossRun`, `MessageFetchCrossRun`, `AckMessageCrossRun`, `ReviewSubmitForeignReviewer`, `TaskRetry`, `WorktreeCreateUnknownAttempt`, `WorktreeCreateForeignAttempt`, `WorktreeVectorBaseCommit` | Refused-input vectors plus one accepted-shape vector (`TaskRetry`: accepted and its identical replay as duplicate, both carrying the retried task's `TaskSeq` and the reserved `AttemptNumber` that `hop task retry` renders into the shared grammar lines). The refusal vectors: a malformed dependency graph, a reused request-ID with conflicting content, a non-manager caller, an oversized title, a stale acking incarnation, an answer replying to an unknown question, a send/fetch/ack from a session belonging to a DIFFERENT run than the request claims, and a review submission from a live reviewer that is not the claimed attempt's own (another run's reviewer, or another review attempt's — both fixture shapes share the one vector) — one function per vector, each returning the exact `app.TaskCreate`/`app.MessageAck`/`app.MessageSend`/`app.MessageFetch`/`app.ReviewSubmission` value to pass to the port method its doc comment names. The two worktree vectors are `run.Worktree` values for a unit of work's `Worktrees().Create` under the named run's own lease, well-formed except for the attempt link: an unknown attempt (refused `app.ErrNotFound`) or another run's attempt (refused `app.ErrFenced`), with no row written either way |

## Invariants

- `TaskRetry` is the one vector that is not a refusal: it pins the
  retry outcome's rendered values so a fake that drops `TaskSeq` (or a
  store that forgets it on receipt replay) fails both suites alike.
- A vector is a pure function of its caller-supplied identities (run,
  session, incarnation, task or message ids) to a request value — it never
  calls a port itself, seeds any store, or asserts anything. The consuming
  test supplies already-established fixture state (an existing run and its
  current manager/non-manager sessions) and asserts the refusal.
- Every vector's doc comment names the EXACT outcome kind and detail
  observed against `internal/app`'s `fakeStore` — this is empirical
  documentation of current behavior, not a guessed contract. A vector whose
  observed refusal reason changes needs its comment updated in the same
  change.
- `TaskCreateSelfDependency` does NOT exercise `internal/domain/run`'s
  `ValidateAcyclic` cycle detection — no multi-node cycle is reachable
  through `PlanStore.CreateTask` at all, since every dependency edge must
  reference an ALREADY-persisted task (the persisted graph is a DAG by
  construction). It is refused earlier, as an unknown dependency; recorded
  here so a future reader does not re-derive this by surprise.
- This package takes no dependency on `internal/adapters/sqlite`: the
  sqlite vector-contract test imports storevectors, never the
  reverse (storevectors has no reason to know sqlite exists).

## Dependencies and ports

- Allowed inward imports: [internal/app](../../app/AGENTS.md),
  [internal/domain/identity](../../domain/identity/AGENTS.md),
  [internal/domain/run](../../domain/run/AGENTS.md).
- Consumed/implemented ports: none — this package builds `app.TaskCreate`/
  `app.MessageAck`/`app.MessageSend`/`app.ReviewSubmission` request VALUES
  for `app.PlanStore`/`app.MessagingStore`/`app.ReviewStore`, and
  `run.Worktree` values for `app.WorktreeRepository`; it implements none
  of them.
- External libraries: none.

## Verification

- `go test ./internal/app -run TestStoreVectors` — the consuming contract
  test (`internal/app/storevectors_test.go`) drives every vector above
  against the fakes (`ReviewSubmitForeignReviewer` against BOTH fake
  layers, the featureStore wrapper and the base fakeStore) through the
  ordinary `PlanStore`/`MessagingStore`/`ReviewStore` ports and the unit
  of work's worktree repository, and asserts the documented refusal.
- `go test ./internal/adapters/sqlite -run 'TestStoreVectors|TestSubmitReviewForeignReviewerRefused'` —
  the real-store half drives the identical vectors against the SQLite
  adapter.
- This package itself has no `_test.go` file: each vector is a pure
  constructor with nothing meaningful to assert in isolation from an
  actual store — the assertion worth making only exists once a vector runs
  against `fakeStore` or the real store, which is exactly what the
  consuming contract tests above do.

## Related guides

- [Parent index](../AGENTS.md)
- [internal/app](../../app/AGENTS.md) and
  [internal/adapters/sqlite](../../adapters/sqlite/AGENTS.md): the two
  consumers, each driving the identical vectors against its own store.
- [Architecture and package catalog](../../../docs/architecture/architecture.md)
