# Phase 3 slice 8: post-merge worktree retirement

Design note for [phase-3-design.md](phase-3-design.md) section 12, row 8,
implementing the human decision of 2026-09-15 (section 6, "Integration
branch and worktree bases"; section 8, "Completion retirement"; open
question 6). Approved on 2026-09-16 with the decisions and conditions in
section 12. Section 13 records the decisions Phase B made where the note
is silent.

The design fixes the mechanism: lazy detection on the next controller
start or `hop status` for the repository, an ancestry check under an exec
claim, removal of the run's attempt worktrees under a fresh exec claim, a
persisted "worktrees retired" run fact so detection never repeats, and
branches never deleted. This note settles what the design leaves open.

## 1. Recommendations at a glance

| # | Question | Recommendation |
| --- | --- | --- |
| 1 | Target branch | Recorded at freeze by `StartFeatureRun`: the full ref `git symbolic-ref -q HEAD` names at the repository root (`refs/heads/<b>`), in a new frozen `WorkflowSnapshot.TargetBranch`. Exit 1 (detached HEAD) records `""`, and that run never retires automatically. No config key. |
| 2 | Uncommitted work | HOP's own pre-check decides, never git's clean check alone. Three executed hazards make git's check unsafe on its own (section 4). A worktree with changes is retained with a fixed category and re-examined on every pass. Removal never uses a force option. |
| 3 | Eligible runs | Terminal (`completed`, `failed`, `stopped`) feature runs with a target, the fact unset, and at least one integrated row that added content. The pass takes the run's lease through `AcquireLease` and skips the run when another controller holds it. |
| 4 | Candidates | Worktree rows of the run with `attempt_id` set, each verified against its attempt's succeeded `worktree.create` operation. The checkout must also appear in the repository's own `git worktree list`, share its common directory, sit on the recorded branch, and descend from the recorded base. The repository root is never a candidate. |
| 5 | Transport | `git worktree remove` without force, spawned through `hop check-exec` via `CommandRunner` with the absolute git. **Not** Herdr's `worktree.remove` (section 5). |
| 6 | Journal | Two exec-claimable operation kinds, `retirement.check` and `worktree.retire`, with the decision-table rows in section 7. |
| 7 | Run fact | Migration 004: `runs.worktrees_retired_at TEXT NULL`. Per-worktree final states reuse `worktrees.state` (`removed`, `absent`, `released`), which needs no DDL. |

## 2. Target branch and the detection subject

**Target.** `StartFeatureRun` (slice 6b) runs one read-only
`git -C <root> symbolic-ref -q HEAD` before `InitializeRun`, alongside its
HEAD resolution:

- Exit 0 with a `refs/heads/` name records that full ref.
- Exit 1 (detached; pinned: no output) records `""`.
- Anything else refuses the start before any side effect
  (`ErrStartRefused`), like the other freeze-time git refusals.

The value is frozen in `WorkflowSnapshot.TargetBranch`, a JSON key in
`run_snapshots.workflow`, so no DDL is needed. Runs frozen before this
slice decode `""` and never retire. Solo runs have no integration branch
and are never candidates.

**Integration head H.** H is the merge commit of the run's most recently
created `integrated` integration row. Serial integration makes that row
the last validated head. H comes from HOP's own validated record, never
from the live `hop/r<seq>/integration` ref: after a merge the human may
move or delete that branch, but H stays reachable from the target.

**Content rule.** A no-op row records `MergeCommitOID == PremergeHeadOID`.
A run is eligible only when some integrated row differs, meaning it
integrated new content.

- Without that rule, H equals the base, which is the target's tip at
  freeze.
- `merge-base --is-ancestor X X` exits 0 (pinned).
- So such a run would read "merged" on its first pass and lose its
  failed attempts' checkouts before any human decision.

**Repository identity.** Before anything else, the pass confirms that the
frozen repository root still holds this run's history:
`git -C <root> cat-file -e H^{commit}` must exit 0. Pinned: a missing
object, a non-commit object, and a root that no longer exists all exit
128.

If the root no longer resolves (the repository was moved), or another
repository now lives there without H, the run is skipped. The pass
renders "the repository root no longer holds this run's history", takes
no lease, and writes nothing.

A clone that does contain H is still caught per worktree: it does not
list HOP's checkouts, and its common directory differs.

**Detection.** Before any journal row, the target's tip T is resolved
read-only (`rev-parse --verify -q <target>^{commit}`). Exit 1 means the
branch is gone (pinned: exit 1, no output), which reports not merged and
journals nothing.

The claimed act then compares two immutable object ids,
`git -C <root> merge-base --is-ancestor H T` (pinned exit statuses:

- 0: H is contained.
- 1: H is not contained.
- 128: unknown object.

Freezing both ids into the intent makes the check deterministic and
repeatable, and its evidence self-describing.

## 3. The pass

**Where it runs.**

- `hop status`, both forms, runs the pass for the resolved repository
  before rendering.
- `hop run` and `hop resume` run it after flag parsing and repository
  resolution, before `StartRun`/`Resume`, excluding their own run. Running
  it first means the new controller's own lease is never waiting on a slow
  removal.
- The pass never changes an exit code. `hop status` stays Herdr-free:
  git only, no Runtime wired.

**Lease-free triage first.** A new read,
`RetirementReadStore.ListRetirementCandidates(root)`, returns each
eligible run's:

- target
- H and the content flag
- last settled `retirement.check` (H, T, result)
- whether any retirement operation is unresolved
- active worktree count

The pass takes a run's lease only when there is work:

- an unresolved retirement operation to recover;
- a new (H, T) pair not yet settled as not-merged or failed; or
- a merged run with active worktrees.

A repeated `hop status` on an unmerged run therefore takes no lease and
writes nothing. Journal growth tracks target movement, not polling, the
same principle as section 7's receipt-free empty fetch.

**Per run, under the lease.**

- `AcquireLease` → `ErrLeaseHeld` renders "deferred" and skips the run.
- `cmd/hop` runs the existing `runHeartbeats` for the handle.
- The run is re-read inside a fenced unit of work: terminal, feature, fact
  unset.
- Recovery runs first (section 7). An unresolved operation that cannot be
  settled blocks the run's pass.
- Detection, unless a settled `retirement.check` already says merged.
  Detection never repeats after that.
- Removal, per active candidate (section 4).
- Set the fact when every worktree row is final.
- `ReleaseLease`: a plain release. `Detach` is not used, because it would
  journal a transition on every pass.

**Budget.** The pass has a fixed 5-minute act bound, and status prints
`retiring worktrees of r<seq>…` before the first removal act. A canceled
act kills the claimed group (`CommandRunner`) and leaves the operation for
the next pass's recovery.

## 4. Candidates, provenance and cleanliness

### Candidate set

The candidates are worktree rows of the run in state `active` with
`attempt_id` set. The linkage and `base_commit` come from the
phase-3/worktree-attempt-link fix.

Each row is verified against the attempt's succeeded `worktree.create`
operation: `attemptWorktreeCreateIntent` plus `worktreeCreateOutcome`.
The attempt, branch, base, outcome path, and the intent's repository root
(which must equal the frozen root) must all agree with the row.

A row that fails verification, or has no attempt, is `released` with a
reason. HOP never acts on a checkout it cannot tie to its own record.

### Pre-act observation

These are read-only git invocations through `CommandRunner`, with the
retirement environment `GIT_CONFIG_GLOBAL=/dev/null`,
`GIT_CONFIG_SYSTEM=/dev/null` and `GIT_TERMINAL_PROMPT=0`, and nothing
inherited. They are not journaled and follow the precedent of
`classifyWorktreeProvenance`.

A composition seam, `InspectPath`, returns the recorded path's canonical
form (the deepest existing ancestor resolved, then the remainder
appended) and whether the path exists. Herdr records the path in its
requested, possibly non-canonical spelling, while git lists the canonical
form (both pinned), so every comparison uses this canonical form. It follows the precedent of the
launch boundary's `ResolvePath`. `internal/app` itself stays free of
filesystem access.

| Observation | Outcome |
| --- | --- |
| canonical path == canonical repository root | `released`: repository root (never) |
| not in `git -C <root> worktree list --porcelain -z`, path absent | `absent` (final; nothing to do) |
| not listed, path present | `released`: not a registered worktree (HOP never deletes a directory git does not vouch for) |
| listed, `branch` ≠ recorded, or `detached` | `released`: checked out elsewhere (a detached HEAD's commits would become unreachable) |
| listed with `locked[ <reason>]` | retained: `locked` |
| listed `prunable` (directory gone) on the recorded branch | removable (the act prunes the entry; outcome `absent`) |
| `git -C <path> rev-parse --path-format=absolute --git-common-dir` ≠ the root's | `released`: another repository |
| `merge-base --is-ancestor <recorded base> <listed HEAD>` exit 1 | `released`: HEAD no longer descends from the recorded base |
| `git -C <path> -c core.fsmonitor=false status --porcelain=v1 --untracked-files=all --ignore-submodules=none` non-empty | retained: `uncommitted-changes` |
| `git -C <path> ls-files -v -z` has a lowercase tag or `S` | retained: `hidden-changes` |
| any of the above exits unexpectedly | retained: `inspection-failed` |
| otherwise | removable |

**Final outcomes.** `absent` and `released` are final. Each is journaled
once as a `worktree.retire` operation that is created settled, is never
dispatched, and has no act. The row moves to that state in the same
transaction.

**Retained outcomes** are not persisted and are re-observed on every
pass. The only exception is a retained outcome produced by a refused act
(below), which that act's operation journals.

### Why HOP's own check, not git's

These values were executed and are pinned in `TestGitProbeWorktreeRemoveOutcomes`
(git 2.54.0).

**git refuses** (exit 128) with:

```
fatal: '<path>' contains modified or untracked files, use --force to delete it
```

when the checkout has untracked, modified or staged files, or a nested
untracked repository.

Locked checkouts are refused with different text, with or without a
reason:

- without a reason:
  `fatal: cannot remove a locked working tree;\nuse 'remove -f -f' to override or unlock first`
- with a reason:
  `…locked working tree, lock reason: <r>\n…`

**Git removes with exit 0 and deletes the data** in three cases:

- **Ignored files** (`.gitignore`, `info/exclude`). They are not
  uncommitted work to git.
- **Untracked files hidden by a repository-local
  `status.showUntrackedFiles=no`.** A command-line
  `-c status.showUntrackedFiles=all` restores git's refusal (pinned), and
  HOP's pre-check passes `--untracked-files=all` explicitly.
- **Modifications hidden by `assume-unchanged` or `skip-worktree`.**
  `status` shows nothing. Only `ls-files -v` reveals them (tags `h` and
  `S`, pinned).

So the act's argv carries the override. HOP's pre-check covers the flag
case, and git's own check stays a second line of defense for the
remaining window.

**Ignored files (approved: follow git).** Ignored files do not block
removal. Retaining on them would retain nearly every real checkout,
because workers produce build output (`node_modules/`, `target/`), and
the feature would then do nothing.

The user-facing documentation and `hop status` say it plainly: *removal
deletes ignored files such as build output; commit anything you want to
keep* (section 6).

Hidden changes (assume-unchanged, skip-worktree) and untracked files
still retain the worktree, under any `status.showUntrackedFiles` setting.

### Act and outcome

1. Commit the intent.
2. `revalidateForDispatch`.
3. Spawn
   `hop check-exec --op <op> -- <git> -C <root> -c status.showUntrackedFiles=all -c core.fsmonitor=false worktree remove <listed path>`,
   with `Dir` set to the root and the environment set to the run's
   sanitized spawn environment plus `HOP_STATE_DIR` and the retirement git
   variables.
4. After the act, observe again (listing plus `InspectPath`):
   - **unlisted and absent:** succeeded; `removed`, or `absent` when the
     directory was already gone before the act. The row moves to that
     state.
   - **listed and present after a non-zero exit:** failed. The category
     comes from the pre-check run again (else `remove-refused` with the
     exit code). stdout and stderr are retained as artifacts under
     `runs/<run>/retirement/<op>/`.
   - **listed but absent:** failed. The row stays active and the next pass
     prunes the entry.
   - **unlisted but present:** `released`.
   - **spawn error:** `reconciling`.

No argv anywhere contains `--force` or `-f`. The argv is pinned
byte-for-byte by a unit test.

## 5. Removal transport: git, not Herdr

Evidence read from the 0.9.0 source and the installed binary's schema.
Section 11 pins the executed values.

- **Shapes.**
  - Request: `worktree.remove {workspace_id (required), force (default false), trust_repository}`.
  - Result: `{type: "worktree_removed", workspace_id, path, forced}`.
  - Dirty refusal: `dirty_worktree_requires_force` carrying git's
    message; its detection is exactly git's check, with the same hazards.
- **Addressed by workspace id, and only while that workspace is open**
  (`workspace_not_found` otherwise). A run's attempt workspaces are
  routinely closed by the human after completion. Closing is Herdr state
  only; the checkout stays.
- **Workspace ids are not durable.** Ids are `w<n>` from a per-process
  counter re-seeded after a restart from the highest *restored* id
  (`reserve_workspace_ids`). A workspace closed before a restart can have
  its id reissued to a new linked worktree, possibly one of a later live
  run of the same repository. A recorded id is an unsafe removal address.
- **No exec claim is possible.** Herdr runs `git` (resolved on its own
  PATH) in its own background thread. HOP spawns nothing: there is no
  pre-exec claim, no lease-generation fence at the act, and no group
  retirement. A lost response leaves a Herdr-side removal HOP can only
  wait on. The design requires removal "under a fresh exec claim".
- **Closing the workspace.** On success Herdr closes the linked workspace,
  terminating its shells, including one the human may be working in.
- **`hop status` stays Herdr-free with git.** The Herdr transport would
  make status dial the server.

The git transport addresses the checkout by path (the durable identity,
which git canonicalizes; pinned), fences at the claim, is retired by the
group rule, and runs HOP's own absolute git.

**What git leaves in Herdr.** Probe
`TestSpikeGitWorktreeRemoveLeavesHerdrWorkspace`, values in section 11.
An open linked workspace stays open. Its membership still names the
removed path, its root shell keeps running in a deleted directory, and
`worktree.list` no longer names the checkout.

The human closes that workspace (`workspace.close`), and the removal line
tells them to. A Herdr-side tidy by the controller (close a workspace
whose recorded worktree is gone) could be a later slice. It is not
needed for correctness.

## 6. Status rendering

`hop status` prints the pass's lines after the listing (or after the
detail block), each prefixed with the run label. It still exits 0: state
is data. A pass error prints one classified, value-free line to stderr,
and rendering continues. The fixed shapes, pinned by golden tests in
`cmd/hop`:

```
r3 worktrees retired: 2 removed, 1 already absent, 0 released (removal deletes ignored files such as build output)
r3 worktree hop/r3/t2a1 removed: /abs/path (close its Herdr workspace if one is still open)
r3 worktree hop/r3/t4a1 retained (uncommitted changes): /abs/path
  action: commit or discard the changes, then run hop status again
r3 worktree hop/r3/t5a1 released (checked out on another branch): /abs/path; HOP will not remove it
r3 worktree retirement deferred: the run is held by another controller
r3 worktree retirement blocked: an earlier retirement process could not be verified gone
```

Retained categories and their actions:

| Category | Action |
| --- | --- |
| `uncommitted-changes` | commit or discard the changes |
| `hidden-changes` | clear `git update-index --no-assume-unchanged/--no-skip-worktree`, then commit or discard |
| `locked` | `git worktree unlock` |
| `interrupted-removal` | inspect; restore (`git checkout -- .`) or remove it yourself |
| `remove-refused` | inspect the retained evidence path |
| `inspection-failed` | check the checkout |

Each action ends with ", then run hop status again".
`interrupted-removal` replaces `uncommitted-changes` when the latest
settled `worktree.retire` for the row recorded an interrupted act.

`hop status -run` adds:

- `target: refs/heads/main`, or
  `target: none (detached HEAD at freeze; worktrees are never retired automatically)`;
- `worktrees: retired <time>`, or, before retirement,
  `worktrees: not retired (removed once the integration branch is merged into <target>; removal deletes ignored files such as build output; commit anything you want to keep)`;
- one `worktree: <branch> <state> <path>` line per row, with the released
  reason.

## 7. Operations, exec claims and the decision table

Both kinds join `check.run` and `integration.merge` as exec-claimable:

- `ClaimCheckExec`'s accepted kind set widens, an in-place change to an
  existing method body.
- `LoadCheckExecutionContext` reads a generic `{argv, cwd}` from the
  intent for the two new kinds and fails closed when that shape is
  malformed.
- `PrepareCheckExec` is unchanged.

**Why the no-claim case is decisive.** Every pass acquires a fresh
generation before recovery. `ClaimCheckExec` refuses a claim for an
operation that is not pending in the current generation, and that
refusal happens inside its write transaction, serialized with
`AcquireLease`. So once the successor holds the lease, a claim-free older
operation can never act.

| Operation | Crash between intent and act | Crash between act and outcome | Takeover with the intent unresolved |
| --- | --- | --- | --- |
| `retirement.check` (intent: H, target ref, T, argv `git -C <root> merge-base --is-ancestor H T`) | No claim: the operation can never run under a superseded generation → settle `failed` ("never executed"); the pass may journal a fresh check | Claim present: retire the claimed group first (argv-matched listing: the frozen argv and its `hop check-exec` invocation, `ClassifyGroupRetirement`), confirm absence, then settle `failed` with result `unknown` (the exit status was never observed). Ancestry over two immutable ids is repeatable, so a fresh check is always safe. Mismatch or inspection failure → stays `reconciling`, and the run's pass is blocked | Same as the two columns, decided by claim presence |
| `worktree.retire` (intent: worktree, attempt, root, listed path, branch, base, pre-act evidence, argv `git -C <root> -c status.showUntrackedFiles=all worktree remove <path>`) | No claim → settle `failed` ("never executed"). The row is unchanged and re-observed from scratch | Claim present: retire the group first (a surviving `git worktree remove` is killed mid-delete), confirm absence, then observe: unlisted & absent → adopt `removed` (row final); listed & present → `failed`, result `interrupted` (row active; later passes render `interrupted-removal` while the checkout is dirty); listed & absent → `failed`, `interrupted` (the next pass prunes the entry); unlisted & present → `released`. Group mismatch → `reconciling`, pass blocked | Same, by claim presence |

Settled-at-creation `worktree.retire` rows (the `absent`/`released`
decisions with no act) are never pending and never recovered. Neither
kind blocks anything outside its own run's retirement pass. Stop, resume
and the scheduling pass never read them, and runs can never leave a
terminal state.

## 8. Schema and ports

**Migration 004** (`004_run_worktrees_retired.sql`) is the next dense
version after 003. If another landing claims 004 first, this one is
renumbered at merge; `loadMigrations` enforces density. It is additive
and forward-only, with no rebuild. It runs on the ordinary path: its own
immediate transaction, with the version re-read inside.

```sql
ALTER TABLE runs ADD COLUMN worktrees_retired_at TEXT;
```

- NULL means not retired. Every existing row stays NULL.
- The value is canonical fixed-width UTC.
- It is written only through the fenced unit of work (`requireLeasedRun`),
  set once, never cleared or moved (the `stop_requested_at` discipline).
- Future-schema refusal is unchanged.

**`worktrees.state`** already has no CHECK constraint, so the new values
need no DDL. The domain gains `WorktreeRemoved`, `WorktreeAbsent` and
`WorktreeReleased`, plus `Worktree.Retire(state)`, which is valid only
from `active`.

**Ports** follow the additive packaging rule: separate interfaces,
type-asserted, failing closed with typed sentinels.

- `RetirementRepositories`, reached from `UnitOfWork`:
  - `WorktreesForRetirement(run)`: rows with attempt, base, state and
    revision.
  - `MarkWorktreesRetired(run, at)`: idempotent, set only when NULL.

  Row state saves reuse `Worktrees().Save`.
- `RetirementReadStore`, reached from `ReadStore`:
  - `ListRetirementCandidates(root)`.
- `RunDetail` gains additive fields: `TargetBranch`, `WorktreesRetiredAt`
  and `Worktrees` (branch, path, state, reason).
- Controller methods for `cmd/hop`:
  - `RetirementCandidates`
  - `AcquireForRetirement`
  - `RetireWorktrees`
  - `ReleaseRetirement`

  `controllerAPI` and its fake gain them with the real contracts.

## 9. Test plan

**Process probe (landed, executed): `gitworktree_probe_test.go`.** Every
git value above, under `process.Runner` with the retirement environment.

**Real-Herdr probe (landed, executed, passing):
`spike_worktreeremove_test.go`.** Section 11.

**Domain.** The `Retire` transition table (only from `active`; each final
state is terminal).

**SQLite.**

- Migration 004:
  - from empty, and over a populated 003 store (existing runs NULL, every
    value intact);
  - reopen is idempotent.
- New states round-trip.
- `MarkWorktreesRetired`: set-once, and fenced across runs.
- `ListRetirementCandidates` filters: solo, non-terminal, no target,
  retired, nothing integrated, no-op-only.
- `ClaimCheckExec` generation, state and kind matrix for both new kinds.
- `LoadCheckExecutionContext` for both kinds, including a malformed
  intent.
- Shared storevectors for the refused inputs.

**Fakes.** A stateful fake git in `fakeCommands` reproduces the pinned
table, hazards included: a hidden-flag or config-hidden checkout IS
deleted by its fake `worktree remove`. So a test proves HOP never
dispatches the removal, rather than trusting git.

The fake also:

- refuses a non-absolute argv[0];
- fails any `worktree remove` argv containing `--force` or `-f`;
- applies the config-hidden hazard when the `-c` override is missing;
- serves the pinned `-z` records, porcelain lines, ls-files tags and
  exit statuses.

The fake store implements both new interfaces and the widened claim
kinds.

**App scenarios.**

- **Green boundary:** a merged run retires every worktree exactly once
  with the fact set. An unmerged run retires nothing and journals only
  one check. A repeat pass journals nothing and invokes no removal.
- **Eligibility:** detached-HEAD target, nothing-integrated,
  no-op-only, and solo runs are never touched. A non-terminal run is
  never touched. A lease held by another controller defers the run. The
  invoking run is excluded.
- **Triage is free:** a pass with nothing eligible, and a repeated pass
  over an unchanged unmerged run, take no lease (the lease generation is
  unchanged) and write nothing (no operation, transition or row revision).
- **Moved repository:** the frozen root no longer exists, or a different
  repository without H lives there. No lease, no operation, nothing
  released, and a skip line.
- **Cleanliness:** every retained category, including the three hazards
  (dispatch never happens). A dirty checkout later cleaned is removed on
  the next pass. `locked`.
- **Released reasons:** not-registered, other branch, detached, other
  repository, base not an ancestor, repository root, unverifiable
  provenance.
- **Absent:** before the act, and prunable.
- **Crash points:** every cell of section 7, each at both kinds. Also a
  takeover barrier: a zombie pass's spawn after the successor's
  acquisition is refused at the claim.
- **Argv pins:** exact argv for both acts.

**cmd/hop.**

- Status runs the pass before rendering and exits 0 with retained,
  deferred and blocked outcomes.
- Golden lines for section 6.
- `run` and `resume` run the pass first and exclude their own run.
- `TestStoreOpenDiagnostics`-style value-free error lines.
- **Real-binary tier:** hopfixtures seeds a terminal feature run whose
  worktree rows point at real `git worktree add` checkouts of a fixture
  repository (`hop status` runs with the Herdr canary armed).
  - The integration branch merged into `main` → checkouts removed,
    branches kept, fact set, lines printed.
  - A second `hop status` → nothing.
  - Unmerged → nothing removed.
  - Dirty → retained, then removed after cleaning.

**Checks.** `make check`, then a suite request for the probe.

## 10. Limits, deliberately out of scope

- **Squash and rebase merges** never contain H, so their worktrees stay.
  `hop clean` is future work.
- **Branches, artifacts, and scratch integration trees** are untouched.
  The scratch trees keep their own settlement cleanup.
- **The residual window inside git** between its own clean check and its
  deletion is unavoidable (Herdr has the same).
- **A repository-local `core.fsmonitor` hook** could misreport status and
  also executes repository-local code. Phase B passes
  `-c core.fsmonitor=false` on the pre-check's index reads and on the act,
  pinned by probe rows (section 13).
- **Global ignore rules.** Retirement git runs with global and system
  configuration suppressed, so a file ignored only by the operator's
  global `core.excludesFile` counts as untracked and retains the
  worktree. This errs toward keeping data; committing, deleting or
  repo-ignoring such files lets the next pass remove the worktree.
- **Slice 7** (real-process feature scenarios) is not part of this slice.

## 11. Probe observations (Herdr 0.9.0, git 2.54.0)

Git values are in section 4 and pinned by the process probe (executed,
passing).

Herdr values come from `spike_worktreeremove_test.go`. They were executed
against a disposable test server, and all four tests passed (exit 0,
2.7s).

1. **Paths are not canonicalized.** `worktree.create` reports the
   checkout path in the requested spelling
   (`/var/folders/…/attempt-t1a1`, not `/private/var/…`). HOP's recorded
   path can therefore be non-canonical, while git lists the canonical
   form. This makes the `InspectPath` canonicalization in section 4
   load-bearing.
2. **Dirty refusal.** `worktree.remove {workspace_id, force: false}` with
   an untracked file returns API error `dirty_worktree_requires_force`
   with message exactly
   `fatal: '<recorded path>' contains modified or untracked files, use --force to delete it`.
   The checkout, workspace, root pane and root shell are untouched.
3. **Clean removal.**
   - The result's key set is exactly `{forced, path, type, workspace_id}`,
     with values `{false, <recorded path>, "worktree_removed", <same id>}`.
   - The directory and git's listing entry are gone, `worktree.list` no
     longer names the checkout, and the branch is kept.
   - The linked workspace is closed (`workspace.get` →
     `workspace_not_found`), its root pane is gone, and its root shell
     process exits.
   - The parent workspace stays.
4. **Closed workspace.** `workspace.close` on the linked workspace returns
   `{type: "ok"}`, and the checkout stays on disk and in git's list. A
   later `worktree.remove` returns `workspace_not_found` with message
   `workspace <id> not found`. `worktree.list` still names the checkout,
   with `open_workspace_id` null and `is_prunable` false.
5. **Id reissue.** Before the restart the ids were a=`w1` and b=`w2`.
   After closing b and restarting, a keeps `w1` and the next new
   workspace receives `w2`, the closed workspace's id.
6. **After a git-side `git worktree remove`** (no force):
   - The linked workspace stays open. Its membership still records
     `checkout_path` = the removed path (non-canonical) and
     `is_linked_worktree: true`; `repo_key` is the canonical common
     directory and `repo_root` the non-canonical root.
   - Its root pane and shell stay alive.
   - `worktree.list` no longer names the checkout.
   - `worktree.remove` on that stale workspace fails with
     `worktree_remove_failed`, message
     `fatal: '<path>' is not a working tree`, and the workspace stays
     open.
   - `workspace.close` tidies it: the shell exits and the parent stays.

## 12. Approval (2026-09-16) and Phase B conditions

Approved: recommendations 1, 3, 4, 6 and 7 as written. That includes
never retiring a run that integrated nothing or only no-ops, and
migration 004 as an additive nullable column. Decisions:

- **(a) Ignored files.** Follow git, with the plain-language warning in
  the documentation and in `hop status` (sections 4 and 6).
- **(b) Transport.** git `worktree remove` without force, through
  `hop check-exec` and `CommandRunner`, with
  `-c status.showUntrackedFiles=all`. `hop status` never dials Herdr for
  this. The removed-worktree line tells the human to close a
  still-open Herdr workspace.
- **(c) Placement.** `hop status` for the repository, and `hop run` /
  `hop resume` before `StartRun`/`Resume`, excluding their own run.
  Triage with nothing eligible takes no lease and writes nothing, and a
  test proves it.
- **(d) Released.** `released` is a final outcome, journaled once.

Conditions:

- **Moved repository.** Never act on a run whose frozen root no longer
  resolves to the same repository. Section 2's identity check, and the
  common-directory check, cover this, and the test plan includes a moved
  repository.
- **Live working directories.** A worktree that is some live process's
  working directory is not detected; the design relies on the run being
  terminal and on the status hint.
- **Start gate.** Phase B starts only after slice 6b (`StartFeatureRun`,
  which freezes `TargetBranch`) and the phase-3/worktree-attempt-link fix
  (`attempt_id`, `base_commit`) have both landed on main.
- **Claim-kind parity.** `retirement.check` and `worktree.retire` extend
  the check-exec argv-by-kind boundary. Each kind's argv stays frozen per
  operation and passes byte-identically through the claim, as the
  existing kinds do, and the fakes match.

## 13. Phase B implementation decisions

Decisions the implementation made where the note above is silent. Each
one was proposed to the manager before or while it was built, and each is
consistent with sections 1 to 12.

**Store and journal.**

- **One claimable-kind predicate.** `OperationKind.ExecClaimable` is the
  single predicate the store and the fakes share. The store's refusal text
  is "not a pending exec-claimable execution".
- **The fact does not move the run's revision.** `MarkWorktreesRetired`
  sets a set-once housekeeping column, and no revision-checked save
  decides anything from it.
- **Leased reads.** `WorktreesForRetirement` lists every row of the leased
  run in insertion order, in any state. `WorktreesRetiredAt` reads the fact
  inside the unit of work. Both refuse another run with `ErrFenced`.

**Freeze.**

- `StartFeatureRun` refuses the start (`ErrStartRefused`, "the
  repository's checked-out branch could not be read") when `symbolic-ref`
  fails or names a non-`refs/heads/` ref.

**Detection.**

- **Failed checks.** A check that exits with anything other than 0 or 1
  settles `failed` and is not repeated for the same (H, T). An unobserved
  outcome settles `unknown` or `never-executed`, which allows a re-check.
- **Ambiguous heads.** Two integrated rows sharing the latest creation
  time with different merge commits fail detection closed.
- **Missing claims.** Recovery treats a missing claim as decisive only
  for an operation of an older generation. Every pass acquires a fresh
  generation, so same-generation recovery never happens in a pass.
- **Environment.** Retirement git reads run with exactly the three
  retirement variables (section 4). The claimed acts get the sanitized
  spawn environment, plus `HOP_STATE_DIR`, plus those three.

- **Identity first.** A pass reads the validated head and runs the
  repository-identity check before recovery and before any settled
  answer. A run whose root no longer holds the head is left entirely
  untouched: an earlier pass's unresolved removal is neither recovered nor
  observed there, and no checkout is released against another repository.

**The pass result.**

- `RetireWorktrees` reports one disposition per run: `retired`,
  `in-progress`, `not-merged`, `nothing-integrated`, `history-missing`,
  `blocked`, `check-failed` or `interrupted`. `hop status` reports
  `deferred` itself.
- The fact is set in the same unit of work that finds no active row. A
  merged run with no rows is retired at once.
- A pass that could not dispatch an act reports `interrupted` and leaves
  the fact unset.
- The first removal is announced through a callback just before its
  spawn, so status can print its "retiring" line before a slow act.

**Triage.**

- The read returns each candidate's integrated rows and all of its
  `retirement.check` operations, not just the last settled one. App code
  applies detection's own rules to them (the head rule, the settled merge,
  the settled answer for the exact (H, T) pair), so triage and detection
  cannot disagree.
- The read does not return an active-worktree count. A merged run needs a
  pass until its fact is set, whether worktrees remain or only the fact
  does.
- Triage follows detection's order. A moved or replaced repository is
  `history-missing` and never leased, even with an unresolved operation.
  An ambiguous head, an unreadable target and a failed check of the same
  pair are reported as `check-failed`, with no lease.

**Commands.**

- **Bound and signals.** `hop status` gives the whole pass a 15-minute
  bound, separate from its 10-second rendering bound. A first
  SIGINT/SIGTERM cancels the pass (a running removal is killed and
  recovered by a later pass), and a second one exits.
- **An unlocatable hop binary.** If the hop binary cannot be located, the
  pass is skipped with one line, and `hop status` still renders and
  exits 0.
- **Placement.** `hop run` passes after `ResolveRunWorkflow` accepts the
  workflow, so a usage-refused run removes nothing. `hop resume` passes
  once its run argument resolves, excluding that run.
- **Output.** Pass lines print before a controller start's own lines, and
  after `hop status`'s listing or detail block. The `retiring worktrees
  of r<seq>…` line prints just before the first removal spawn.
- **Failures.** A triage, acquisition, pass, heartbeat or release failure
  is one value-free stderr line naming only the run label, and the
  remaining runs are still attempted. A run found ineligible at
  acquisition (a raced read) is silent.
- **Lines beyond section 6's examples** (same shapes):
  - `already absent`;
  - `removal incomplete` and `removal unresolved`, each ending "hop status
    will finish the removal";
  - `not removed yet … hop status will retry`;
  - a post-act release ending "left on disk; no longer managed by HOP";
  - a refused removal's category carrying its exit code, and an action
    naming the evidence path;
  - run-level `skipped` (history missing), `check failed` and
    `interrupted`.

  Not-merged and nothing-integrated runs print nothing.
- **`hop status -run` for a feature run.**
  - The single `worktree:` line is replaced by one line per row: removed,
    absent, released with its reason ("left on disk; no longer managed by
    HOP"), retained with the category of its last refused removal and an
    `action:` line, `removal incomplete|interrupted|unresolved` ("hop
    status will finish the removal"), or active.
  - A solo run keeps its single line. `RunDetail` carries the rows and the
    run's `worktree.retire` operations, and app code derives each row's
    view.

**Repository code in the checkout.**

- **The fsmonitor hook.** A repository-local `core.fsmonitor` hook runs
  on every index read git makes, including the pre-check's `status` and
  `ls-files` and the removal's own clean check. The pre-check's index
  reads and the removal argv therefore carry `-c core.fsmonitor=false`,
  so the removal argv is
  `git -C <root> -c status.showUntrackedFiles=all -c core.fsmonitor=false worktree remove <path>`.
  Child git processes inherit command-line configuration through
  `GIT_CONFIG_PARAMETERS`, so the override also reaches a submodule
  `status` recurses into.
  - The probe rows prove the hazard first: each plain invocation runs the
    hook, including a submodule's own. Then they prove the protection:
    with the override, the hook never runs
    (`TestGitProbeFsmonitorOverride`, `TestGitProbeSubmoduleCheckouts`).
  - The real-binary merged test repeats this with a hook that
    `hop status` must never run.
- **Submodules.**
  - The pre-check passes `--ignore-submodules=none` explicitly. A
    repository-local `diff.ignoreSubmodules=all` otherwise hides a dirty
    submodule from `status` (probe-pinned), so such a checkout is retained
    as `uncommitted-changes`.
  - `git worktree remove` refuses any checkout that holds an initialized
    submodule, clean or not: exit 128, "fatal: working trees containing
    submodules cannot be moved or removed". A clean one therefore reaches
    the act, is refused, and is retained as `remove-refused` with the
    evidence path. HOP never removes it.
- **Residual: clean and smudge filters.** `git status` can run
  repository-configured clean filters (`filter.<name>.clean`, selected by
  `.gitattributes`) on racily-clean files, and no single command-line
  switch neutralizes that. This is a documented residual, not a defect.
  - The design's trust model treats workers as cooperative participants
    in the run's transitions, not as a boundary against an agent running
    under the operator's own UID, which can already run code as the
    operator.
  - The integration check already runs repository code by design (the
    check command).
  - Retirement's own git reads suppress global and system configuration
    and fsmonitor, and never force a removal.

**Output completeness.**

- **The runner reports truncation.** The process runner keeps at most
  1 MiB of each stream, or the command's own `Command.MaxOutputBytes`
  (0 is the default, and a negative bound is refused before anything
  starts). The bound applies to each stream separately, and the buffer
  grows only as the child writes, never beyond the bound.
  `CommandResult.StdoutTruncated` and `StderrTruncated` report a stream
  whose later bytes it discarded, whatever the exit status. The fake
  runner applies the same bounds and computes the same flags. A kept prefix can end exactly on a record
  boundary (an `ls-files -v -z` entry, a `worktree list -z` record) and
  then parses as complete output, so no rule decides on the prefix's
  shape.
- **Larger bounds for the two growing reads.** The index scan
  (`ls-files -v -z`) and the worktree listing are read with a 64 MiB
  bound (`retirementListingOutputBytes`), so a repository with a large
  index or many worktrees is still inspected in full. Every other
  retirement read keeps the 1 MiB default; `status` among them, whose
  output past 1 MiB already means changes. The bound only moves the cut:
  the truncation rule below still applies to every read.
- **Retirement never decides on a truncated read.** `retirementGit`
  refuses every retirement read with either stream truncated, and each
  caller already treats a failed read as unobservable:
  - the pre-check retains the row as `inspection-failed`, removes
    nothing and journals nothing, and the next pass looks again;
  - the post-act observation leaves the operation `reconciling`;
  - recovery blocks;
  - the target read fails the check, and the identity check reports
    `history-missing`, which touches nothing.

  A hidden-flag entry past the bound therefore retains the checkout
  instead of letting a removal delete the hidden change. A worktree
  listing cut before a candidate's record retains that row (never
  `released` as unregistered, never `absent`); since the listing is the
  same for every row, the whole run defers with the existing dispositions
  and its fact stays unset. A truncated `status` read is retained as
  `inspection-failed` too, so every truncated read has one outcome.
- **The `ls-files` terminator.** Every `ls-files -v -z` entry is
  NUL-terminated (probe-pinned), so output ending without one is
  malformed. This catches a cut inside an entry, but proves nothing
  about completeness; the truncation report does.
- **The claimed acts.** The check decides on its exit status alone, and
  a removal on the post-act observation, so a truncated act output
  changes no decision. A failed removal's evidence files hold at most the
  captured prefix of each stream.
- **The human action.** `inspection-failed` renders "the checkout could
  not be fully inspected; check it and its repository, then run hop
  status again", replacing section 6's "check the checkout".

**Path resolution (`InspectPath`).**

- **The filesystem resolves the spelling.** The composition's inspector
  never cleans a recorded path before resolving it. Collapsing
  `<dir>/link/../wt` to `<dir>/wt` is wrong whenever `link` is a
  symbolic link: the filesystem's answer is the parent of the link's
  target. Such a collapse can report an existing, registered checkout as
  a missing unlisted one, and so as `absent` for good.
  - Existence is `Lstat` of the recorded spelling.
  - An existing path is `EvalSymlinks` of it. A dangling link leaf is
    its resolved parent plus its own name.
  - A missing path is walked component by component. Each symbolic link
    is resolved before the next component, and `..` steps up from the
    directory resolved so far. The walk stops at the first missing
    component, and the remaining plain names are appended (section 4's
    "deepest existing ancestor, then the remainder").
- **An unresolvable spelling is never absent.** The inspector returns an
  error, and the row is retained as `inspection-failed`, when it meets:
  - a `..` after a missing component;
  - a dangling or looping link on the way (an unmounted volume, for
    example);
  - a non-directory used as a directory;
  - an unreadable component;
  - a path that changed while it was being resolved.

  Its errors carry at most the system cause, never the path.

**Dispatch revalidation (`revalidateRetirementDispatch`).** Both claimed
acts run it immediately before the spawn:

1. A fresh heartbeat CAS.
2. A fenced re-read. The run's state must be exactly `completed`, `failed`
   or `stopped`, and the worktrees-retired fact must be unset. Any other
   state (`stopping`, `resuming` and `completing` included) refuses the
   act, and so does a set fact.

It does not consult the run's stop request. The only reason is that a
terminal run can never start work again: every stopped run carries a stop
request, and on a terminal run that request is history. The ordinary
`revalidateForDispatch` refuses under a held stop request, so a stopped
run would never have dispatched its check. Each pass would then have left
one more never-executed check behind.

**Candidates.**

- A row is a candidate only when exactly one succeeded `worktree.create`
  operation of the run carries its attempt, and that operation's intent
  and recorded outcome agree with the row. The outcome is the act
  evidence, or a recovery's outcome.
- The candidate's branch is `refs/heads/` plus the recorded short name.
  Herdr reports the requested name (spike S9), and git lists the full ref.
- A row whose recorded branch already starts with `refs/` is unverified.

**Removal operations and outcomes.**

- **(a) Operation states.**

  | Case | Operation state | Result | Row |
  | --- | --- | --- | --- |
  | Created settled, checkout absent | succeeded | `absent` | `absent` |
  | Created settled, released (including unverified provenance) | succeeded | `released`, with the reason | `released` |
  | After the act: unlisted and present | failed | `released` | `released` |
  | After the act: listed and absent | failed | `incomplete` | stays active |

  The recovery rows keep section 7's `interrupted`.
  - A post-act release tells the human that the directory was left on
    disk and HOP no longer manages it.
  - After an `incomplete` act, the next pass completes the removal with
    the same no-force `worktree remove`, which the probe pinned for a
    missing directory. It does not use a separate prune command.
- **(b) An exit of 0 with the checkout still listed and present.** This
  is handled like a non-zero exit: the operation fails, and the category
  comes from re-inspection (else `remove-refused`), with the evidence
  retained.
- **(c) An unobservable outcome.** A post-act observation that cannot be
  made (the listing, or the path inspection, fails after an observed
  exit) leaves the operation `reconciling`. The next pass's recovery then
  retires the claimed group and observes again.
- **(d) `interrupted-removal`.** It is rendered when the row's latest
  settled `worktree.retire` has result `interrupted` and re-inspection
  reports `uncommitted-changes`. "Latest" means the newest generation,
  then the newest creation.
- **Evidence.** A failed act's stdout and stderr are written under
  `runs/<run>/retirement/<op>/` through `ArtifactStore`. Their paths are
  recorded in the operation's outcome, and no artifact rows are written,
  so the domain needs no new artifact kinds.
- **A halted pass.** When an act's dispatch revalidation refuses, the
  pass stops: the intent stays pending (the next pass settles it
  `never-executed`), and later rows wait for the next pass.
