# test/integration

## Purpose

The real-process Herdr suite: proves HOP's plugin and protocol behavior
against a real, disposable `herdr server` rather than a fake. Each test
spawns its own named-session server on temporary config, state and runtime
roots with a test-only config file, links the staged HOP plugin built from
this tree, and drives everything through HOP's own protocol client.

## Quick reference

| File | Entities / functions | Responsibility |
| --- | --- | --- |
| [harness_test.go](harness_test.go) | `requireHerdr`, `prepareServer`, `testServer`, `serverProcess`, `start`, `startAnchoredLeader`, `restart`, `reapServer`, `stagePlugin`, `newArtifactDir`, `artifactDir.dir`, `registrySnapshot`, `assertNoRegistryLeak`, `waitForLog`, `waitUntil`, `killProcessGroupThenReap`, `safeToSignalGroup`, `call` | Disposable server lifecycle (spawn, bounded socket-readiness wait, API `server.stop` then a process-group `SIGKILL` and reap of the leader), a `restart` that gracefully stops and reaps the current server before relaunching on the same roots so persisted session state survives (S3), per-`serverProcess` at-most-once reaping, `startAnchoredLeader` (the shared start-as-group-leader-and-anchor step every owned leader in this suite uses — the server, spike fixture leaders and task 6b's hop subcommands), hermetic subprocess environment built from scratch (`shell` selects the pane login shell), plugin staging (`go build` + manifest copy), `artifactDir.dir` for evidence that is a directory tree rather than one file (a fixture repository, a built binary), failure-retained evidence, user-global registry leak checks and bounded polling |
| [reap_test.go](reap_test.go) | `TestSafeToSignalGroup`, `TestKillProcessGroupThenReapKillsDescendants`, `processExists` | Table-driven guard against signaling an unsafe process group (non-positive, init, or the caller's own group), and a fixture leader + backgrounded grandchild proving the whole group is torn down and the leader reaped |
| [plugin_test.go](plugin_test.go) | `TestRealProcessPluginLifecycle`, `TestRealProcessStartupHookRunsOnServerStart` | Link → action list → invoke → completed log records with real output → workspace/pane open and rendered evidence → unlink; and the startup hook proven to run on server start, not on link |
| [presentation_test.go](presentation_test.go) | `TestRealProcessAgentPresentationAndView`, `createRunFixture` | A deterministic manager+workers fixture (custom agent identity/state via `pane.report_agent`), HOP metadata published through the presentation adapter, tokens round-tripped through `agent.list`, manager-first ordering, HOP's owned view select/clear, and another owner's view surviving HOP's owned clear |
| [observation_test.go](observation_test.go) | `TestRealProcessEventObservationReconcile`, `TestRealProcessContextInjection` | Subscribe-before-snapshot reconciliation over a real status transition, and context injected into a live pane through `pane.send_text` |
| [launchenv_test.go](launchenv_test.go) | `TestRealProcessLaunchEnvironment`, `TestRealProcessLaunchEnvironmentIsAdditive` | The launch-environment compatibility results: workspace/tab/pane creation carry an explicit env to the launched shell, and the env map is additive — a Herdr-managed variable is authoritative and cannot be overridden. See [launch-environment.md](../../docs/architecture/launch-environment.md) |
| [pty_test.go](pty_test.go) | `TestRealProcessPTYRendering`, `ptyClient` | A PTY-attached client at fixed 100x30 dimensions capturing how the native Agents sidebar renders HOP's projection: the fixture agents drawn, manager-first order on screen, and a HOP metadata token in the rows |
| [artifacts_test.go](artifacts_test.go) | `TestRealProcessArtifactDirRemovedOnPassingRun`, `TestArtifactDirRemovedOnPassingRun`, `TestArtifactRetentionDecision` | A real server run with an open subscription proving the process-group teardown leaves the artifact directory empty, plus harness self-tests for the evidence-retention contract that need no herdr binary |
| [spike_fixture_test.go](spike_fixture_test.go) | `buildSpikeFixtures`, `spikeFixtures`, `spikeFixtureSource`, `newSpikeUUID`, `processInfo`, `spikeProcessInfo`, `agentRecords`, `waitForAgent`, `assertNeverListedAsAgent`, `waitForShellReady`, `waitForAgentStart` | Shared Phase 2 capability-spike fixtures: one Go program built and installed under a launcher-stand-in name (execve to a harness) and under a detection-recognized `claude` name and an unrecognized name; `pane.process_info`/`agent.list` readers and bounded agent-detection polling; `waitForShellReady` polls for the pane shell being the sole idle foreground process (Herdr's `agent.start` precondition), and `waitForAgentStart` retries `agent.start` itself through `waitUntil` on `agent_pane_busy` since that inspection is not atomic with the call — the busy rejection runs before any state mutation, so retrying is always safe |
| [spike_launch_test.go](spike_launch_test.go) | `TestSpikeLaunchLineDetection`, `TestSpikeUnrecognizedProcessNameIsNotDetected`, `createSpikeWorkerPane` | S1: a `send_text` `exec <launcher> … -- <harness>` line launches despite noisy `/bin/sh` and zsh login-shell startup (zsh skipped with a reason when absent), the execve'd recognized process is detected as an agent (an unrecognized name is not), the additive env survives both execs, and the exec'd process's pid is asserted equal to `shell_pid` and the foreground process group |
| [spike_harness_test.go](spike_harness_test.go) | `TestSpikeRestartBarrier`, `TestSpikeGroupAnchor`, `TestSpikeOwnedGroupTeardown`, `TestSpikeOwnedGroupDrainDeadline`, `startFixtureLeader` | Harness self-tests, no herdr binary: the S3 restart barrier waits for the leader's actual exit (release-gated save survives) and force-kills when it never exits; the group anchor pins the pgid after the leader is reaped and a leader that already exited cannot be anchored (inconclusive); the live-probe owned-group lifecycle retires surviving children on both the deadline and normal-exit paths; and the bounded output drain fires its deadline and closes the reader when a write descriptor is held open outside the group |
| [spike_identity_test.go](spike_identity_test.go) | `TestSpikePaneProcessIdentity`, `waitForForegroundProcess` | S2: the exact `pane.process_info` identity fields (pid, name, argv0, full argv, cmdline, cwd; empty `tty` on macOS; no start time), and a same-kind occupant replacement distinguished by pid + argv with a stable `shell_pid` |
| [spike_restore_test.go](spike_restore_test.go) | `TestSpikeRestorePlainPaneLosesAdditiveEnv`, `TestSpikeRestoreAutoRelaunchBypassesLauncher`, `snapshotDump`, `snapshotPaneIDForLabel` | S3: a graceful restart restores a pane's label but not its creation-time additive env; a recorded native session makes Herdr auto-relaunch via `claude --resume` (deferred until client geometry), bypassing the launcher and dropping `HOP_*`; the delayed-restore window shows a phantom idle agent in `session.snapshot` |
| [spike_claude_session_test.go](spike_claude_session_test.go) | `TestSpikeClaudePreassignedSessionID`, `requireClaude`, `runClaude`, `findTranscript`, `forbiddenClaudeEnv` | S4: against the installed claude in print mode, in a scratch `CLAUDE_CONFIG_DIR` with a credential-free environment, `--session-id <uuid>` is accepted and creates the transcript under that UUID, `--resume <uuid>` finds it (unknown → not found); the only test that runs a real harness, so it is opt-in (`HOP_LIVE_HARNESS=1`, else skipped with a reason) and also skips when no claude binary is installed |
| [spike_agentstart_test.go](spike_agentstart_test.go) | `TestSpikeAgentStartArgvCapability` | S5: `agent.start` rejects an unrecognized kind and control-char args and composes argv as `[executable(kind)] + args`, so no wrapper can be interposed; and it types a bare name whose resolution the pane shell's PATH (`path_helper`) decides |
| [spike_layout_test.go](spike_layout_test.go) | `TestSpikeLayoutApplyCommandPane`, `TestSpikeLayoutApplyAddsTabToExistingWorkspace`, `snapshotPaneByLabel`, `snapshotIDs`, `paneExists`, `drainEventNames` | S6: `layout.apply` pane nodes carry `command`+`env`+`label` and run the argv as the pane process (asserted, at the requested cwd) with no shell; an additive apply (`workspace_id` only) adds one tab and every pre-existing tab/pane id survives (+1 tab, +1 pane); a non-zero command exit closes the pane and carries no exit status in `pane.exited` |
| [spike_markers_test.go](spike_markers_test.go) | `TestSpikeCreationMarkerTabLabel` | S7: a `tab.create` creation-time `label` round-trips through `tab.list` and `session.snapshot`, letting a crashed controller recover its own tab and root pane by a unique marker without the create response; the additive env is never a snapshot field |
| [spike_workspace_test.go](spike_workspace_test.go) | `TestSpikeWorkspaceCreateResponseShapeAndEnv`, `TestSpikeWorkspaceCreateNoFocus`, `workspaceCreatedResponse`, `snapshotWorkspaceByLabel`, `snapshotSoleTabForWorkspace`, `snapshotSolePaneForTab`, `focusedWorkspaceID`, `workspaceIsFocused` | Phase 3 spike S8: `workspace.create`'s `{workspace, tab, root_pane}` response shape, cross-referenced ids, and cwd/env delivery to the root pane's login shell (no `command` field, unlike layout.apply); its `label` names the WORKSPACE (WorkspaceInfo.label), never the root pane, and round-trips through `session.snapshot` one level up from S7's tab recovery (workspace → its sole tab → that tab's sole pane); `focus:false` neither reports the new workspace focused nor steals the session's active workspace, honored against the one exception that the session's very first workspace always activates regardless of the request |
| [spike_worktree_test.go](spike_worktree_test.go) | `TestSpikeConcurrentWorktreeCreate`, `TestSpikeWorktreeCreateExistingBranchIgnoresBase`, `worktreeAttempt`, `worktreeCreatedResponse`, `workspaceGet`, `callWorktreeCreateAsync`, `worktreeHead` | Phase 3 spike S9: THREE concurrent `worktree.create` calls against one repository -- implement task 1, implement task 2 and the REVIEW task, all sharing the ordinary `hop/r1/t<t>a1` naming with no separate review-branch scheme -- covering BOTH `base` cases side by side (t1a1/t2a1 new branches honor `--base`; t3a1 reuses a branch created ahead of time, silently ignoring it), the `{workspace, tab, root_pane, worktree}` response shape, a distinct workspace/path/branch per call, the `label` field driven on the raw request (the Phase 2 adapter's `CreateWorktree` does not send one yet -- a gap for whichever task wires this up), and repo_key-based grouping with the parent workspace (the manager's own `workspace.create`d workspace, which worktree.create resolves and marks as the non-linked source); every worktree workspace coexists with an S6 command-pane launch at its own checkout path. A second, isolated test repeats the existing-branch-ignores-base FINDING on its own |
| [spike_paneenv_test.go](spike_paneenv_test.go) | `TestSpikeMultiPaneEnvIsolation`, `applyCommandPaneAsync`, `waitForEnvDumpAtPath`, `parseEnvDump` | Phase 3 spike S10: three S6 command-pane launches issued CONCURRENTLY into one workspace with distinct `HOP_SESSION_ID`/`HOP_ROLE` (manager-shaped, worker-shaped ×2) each observe exactly their own additive env with no cross-contamination, their creation labels resolve to the right panes, and `agent.list` detects each independently (three records, no merging) |
| [spike_apilog_test.go](spike_apilog_test.go) | `TestSpikeAPIRequestLogObservability`, `lineContaining`, `firstLineIndex`, `extractRequestID` | Phase 3 pre-freeze probe (provisional numbering): Herdr's own request log (`herdr-server.log`, next to the server's socket) records `pane.send_text`/`pane.send_keys`/`pane.send_input` at DEBUG only (invisible at the default `herdr=info` filter) while `agent.prompt`/`agent.send_keys`/`agent.start` are always visible at INFO — a positive control on a SEPARATE client connection proves both an elevated-level capture and correlates each call's logged `request_id` back by method name, since `request_id` is a CLIENT-chosen label the server only echoes (confirmed colliding across two independent connections, never a global key); every line carries method + request_id but never the pane/agent target, so an injection-free claim can only be "no such request was logged during the run," never per-pane; a capture window is bracketed by two otherwise-unused methods' own log lines (`pane.list`/`tab.list`), not by searching for a label the log never carries at all |
| [spike_gitrefs_test.go](spike_gitrefs_test.go) | `TestSpikeIntegrationRefCompareAndSwap`, `TestSpikeDetachedMergeLeavesRefUntouched`, `TestSpikeRunScopedRefFamilyCoexists`, `TestSpikeBareRunBranchCollidesWithAttemptBranches`, `TestSpikeUpdateRefCreateOnlySemantics`, `TestSpikeIntegrationRollbackPreservesRejectedMergeReachable`, `runGit`, `runGitWithEnv`, `mustRunGit`, `readRef` | Phase 3 pre-freeze git-level probes G1/G3 (provisional numbering, requested by the design reviewer for the fenced-publish assumptions a revised integration design rests on). `refs/heads/hop/r<seq>/integration` is the CONFIRMED real run-scoped ref scheme; `TestSpikeBareRunBranchCollidesWithAttemptBranches` is the executed negative control proving why (the ORIGINAL bare `hop/r<seq>` branch cannot coexist with any `hop/r<seq>/t<t>a<n>` sibling: git's ref storage cannot treat one path as both a file and a directory). `git update-ref <ref> <new> <old>` against a `worktree.create`'d checkout is a true compare-and-swap (matches → moves; stale → refused, ref untouched); `""` as `<old>` is create-only (succeeds only when the ref does not yet exist). A `git merge --no-ff` in a plain DETACHED scratch checkout (never the integration branch's own checkout) computes a candidate merge commit without moving the ref at all — publishing (and rolling back a rejected merge via `git commit-tree` with the rejected merge as parent, keeping it reachable) is a separate, explicit CAS step |
| [spike_gitmerge_test.go](spike_gitmerge_test.go) | `TestSpikeMergeTwoParentCommit`, `TestSpikeMergeConflictExitCodeAndIndexState`, `TestSpikeMergeAlreadyUpToDateNoOp`, `TestSpikeMergeNoFFForcesCommitOnFastForwardableHead`, `TestSpikeMergeRepoLocalHooksFire`, `TestSpikeMergeHooksPathSuppressesRepoLocalHooks`, `TestSpikeMergeRepoLocalSigningConfigDoesNotBlock`, `TestSpikeMergeNonConflictFailureIsDistinctFromConflict`, `TestSpikeCommitTreeIsDeterministic`, `mergeIdentityArgs`, `mergeEnviron`, `runMerge`, `detachedScratch` | Phase 3 pre-freeze git-level probe G2 (provisional numbering): the merge outcome matrix under the frozen non-interactive argv (`git -c user.name=... -c user.email=... -c commit.gpgsign=false -c merge.verifysignatures=false merge --no-ff --no-edit`) and env (`GIT_TERMINAL_PROMPT=0`, plus fixtureGitEnviron's `GIT_CONFIG_GLOBAL`/`GIT_CONFIG_SYSTEM=/dev/null`) — two-parent merge, conflict (exit code, `UU` index state, clean abort), already-up-to-date no-op, and `--no-ff` forcing a merge commit on an otherwise fast-forwardable head, all leaving the run-scoped ref untouched. FINDINGS: repo-LOCAL `.git/hooks` (`pre-merge-commit`, `post-merge`) fire under this argv (no `--no-verify`), but `-c core.hooksPath=<dir>` (an empty existing directory OR one that does not exist at all) suppresses BOTH -- a verified suppression option, not a design choice made here; repo-local `commit.gpgsign=true` does NOT block the merge (command-line `-c` wins). A non-conflict failure (an unwritable shared object database) is distinct from a conflict (no `CONFLICT` text, no `UU` entries) but -- unlike that difference -- still cleanly `merge --abort`s. `git commit-tree` with pinned tree/parent/identity/dates/message is deterministic (identical oid twice), the property the rollback construction depends on |
| [spike_worktreeremove_test.go](spike_worktreeremove_test.go) | `TestSpikeWorktreeRemoveHerdrShapes`, `TestSpikeWorktreeRemoveNeedsOpenWorkspace`, `TestSpikeWorkspaceIDReissuedAfterRestart`, `TestSpikeGitWorktreeRemoveLeavesHerdrWorkspace`, `callRaw`, `requireAPIError`, `createAttemptWorktree`, `worktreeList`, `listedByHerdr`, `listedByGit`, `canonicalMaybeMissing`, `sameMaybeMissingPath`, `pidAlive`, `pathExists`, `workspaceExists` | Slice 8's removal-transport probe (provisional numbering R1; [phase-3-worktree-retirement.md](../../docs/plan/phase-3-worktree-retirement.md)): Herdr's `worktree.remove` result shape, its `dirty_worktree_requires_force` refusal without force, and its effect on the linked workspace (closed, root shell terminated, parent kept, branch kept); its dependence on an OPEN workspace (`workspace_not_found` once the workspace is closed, checkout untouched); a closed workspace's id reissued after a server restart (ids are not durable addresses); and what a git-side `git worktree remove` leaves in Herdr (the linked workspace open with stale membership and a live shell, `worktree.list` no longer naming the checkout, `worktree.remove` on it failing, `workspace.close` tidying it) |
| [hopcmd_test.go](hopcmd_test.go) | `TestMain`, `buildHopBinary`, `hopResult`, `runHop`, `requireHopCommand`, `hopEnviron`, `startHopController`, `groupMember`, `listGroupMembers`, `parseGroupMember`, `TestBuildAndRunHopBinary`, `TestHopEnviron`, `TestStartHopController`, `TestListGroupMembers` | Task 6b harness extensions (phase A, item 4): `TestMain` + `buildHopBinary` build `./cmd/hop` once per test binary run into a shared temp dir instead of per test; `runHop` runs a bounded one-shot hop subcommand capturing stdout/stderr/exit code, and `requireHopCommand` skips a scenario with a clear reason when its command is cmd/hop's "unknown command" response (task 6a not yet landed/merged); `hopEnviron` builds the isolated `HOP_STATE_DIR` + this disposable server's own socket/binary path on top of the suite's hermetic base; `startHopController` starts a long-running hop subcommand (`hop run`/`hop resume`) as its own anchored, logged process group via `startAnchoredLeader`, returned as a `serverProcess` so existing group helpers apply unchanged; `listGroupMembers`/`parseGroupMember` independently list an arbitrary process group's members via `ps` — deliberately NOT importing `internal/adapters/process` (disallowed by the architecture checker for this package), so a scenario proving group retirement is evidence against the OS process table, not an echo of the port under test |
| [fixturerepo_test.go](fixturerepo_test.go) | `fixtureRepo`, `newFixtureRepo`, `newFixtureRepoGitDependentCheck`, `newFixtureRepoWithSubmodule`, `initFixtureRepo`, `CommitCheckResult`, `fixtureConfigTOML`, `fixtureGitEnviron`, `runShellScript`, `TestFixtureRepoDeterministicCheck`, `TestFixtureRepoGitDependentCheck`, `TestFixtureRepoSubmoduleFailsClearly` | Task 6b fixture repository builder (phase A, item 2): a temporary SHA-1 git repository (`git init --object-format=sha1`, isolated from any developer git config, its own local commit identity) with a trivial source file, the deterministic `check.sh` (`CommitCheckResult` toggles CHECK_RESULT and commits, so the caller picks pass/fail "on demand" by choosing which commit it submits), the git-dependent `check-git.sh` variant (`newFixtureRepoGitDependentCheck`, succeeds only inside a real git checkout), and the submodule variant (`newFixtureRepoWithSubmodule`) whose `check-submodule.sh` fails clearly against a detached `git worktree add` checkout that never initializes submodules. Each constructor takes the scenario's `*testServer` (or `nil` for a pure git-level probe that never touches one) and registers that repository's worktree cleanup (worktreecleanup_test.go's `registerWorktreeCleanup`) |
| [worktreecleanup_test.go](worktreecleanup_test.go) | `registerWorktreeCleanup`, `cleanupServerWorktrees`, `serverScratchRoots`, `removeGitWorktree`, `worktreeBelongsToRepo`, `parseWorktreeListPaths`, `TestRealProcessWorktreeCleanedUpAfterRun` | Real git-worktree teardown, registered as a `t.Cleanup` at every fixture-repo construction that has a server: resolves every worktree to remove ONLY from `git -C <repo> worktree list --porcelain` (repo's own administrative metadata), never by globbing `~/.herdr` or any path outside the test's own roots; a listed worktree outside the test's known scratch roots (the pre-fix leak case, evidence (b)) is removed only after independently confirming its `.git` file points back at this fixture repo, otherwise the test fails naming the exact path rather than removing it blind. A second pass sweeps any directory left under the configured `[worktrees]` directory that never finished registering with git. `TestRealProcessWorktreeCleanedUpAfterRun` drives the cleanup directly (not only via the automatic `t.Cleanup`) against a worktree a real `hop run` created, so it can assert the postcondition — directory gone, `git worktree list` down to the main checkout — inside its own body |
| [fixtureworker_test.go](fixtureworker_test.go) | `fixtureWorkerSource`, `fixtureWorkerBrief`, `fixtureManagerTask`, `fixtureManagerAnswer`, `fixtureManagerBrief`, `buildFixtureWorker`, `testAssignmentPrompt`, `testContinuationPrompt`, `writeTransientOnceHopStub`, `isResumeInvocation`, `requireResumeShape`, `parsedStandInPID`, `newRequestID`, `runHopCLIRetryable`, `verdictRejectedShortfallToken`, `evidenceInconsistentShortfallToken`, `evidenceInconsistentShortfallLine`, `statusHasEvidenceInconsistentShortfall`, `fixtureEvidenceInconsistentQuestionBody`, `verdictRejectedLinePrefix`, `verdictRejection`, `parseVerdictRejectedLines`, `statusOutcomeForNotice`, `matchTaskNeedsReworkLine`, `matchIntegrationLine`, `integrationNoticeStates`, `commitConflictingChange`, `syncOutput`, `TestFixtureWorkerSelfKillOnControlFile`, `TestFixtureWorkerIdleSelfKillOnControlFile`, `TestFixtureWorkerSubmitValid`, `TestFixtureWorkerExitWithoutSubmitting`, `TestFixtureWorkerResumeContinuation`, `TestFixtureWorkerResumeShapeRejections` | Task 6b fixture worker (phase A, item 3), generalized in Phase 3 slice 7a into the fixture PRINCIPAL (design section 11): one Go program (`fixtureWorkerSource`, still the same identifier) built (never installed) and installed under the recognized name `claude`, dispatching on `HOP_ROLE` (absent for solo) into `runWorker` (solo and feature-mode implementer — they share the Phase 2 prompt/result-submission shapes byte for byte), `runManager` (`manager-feature`) and `runReviewer` (`reviewer-approve`/`reviewer-reject-once`), documented in full at the const's own doc comment. Before anything about ANY role is observable (the re-exec'd incarnation included) it spawns ONE long-lived MCP stand-in child into its own process group — this same binary in stand-in mode (`fixture-mcp-stand-in` argv[1]), marker-free argv, detached stdio except a stdin pipe held open forever so the child exits on EOF exactly when the parent exits or execs — reproducing the pinned Claude Code 2.1.270 shape so EVERY scenario's settlement, stop, reattach and retirement decisions run against a multi-member foreground group regardless of role; the child's pid is recorded in every role's observation dump (`mcp_stand_in_pid`) and the self-test asserts it dies with its parent. Every role reads its required `HOP_*` env, computes its own assignment/instructions/role-artifact path independently from it (never parsed out of a prompt — solo/implementer: `attemptAssignmentPath`'s formula per role; manager: the run-level `artifacts/assignment.md`; reviewer: `roleArtifactPath(stateRoot, runID, "reviewer")`), and cross-validates that computed path against the one extracted from its pinned initial/continuation prompt (`promptMarkers`/`workerPromptMarkers`/`reviewerPromptMarkers`/`managerPromptMarkers`, retyped from the same golden literals `roleprompts_test.go` pins — a mismatch fails the run loudly). Every Phase 3 behavior's `FIXTURE-BEHAVIOR: <name> <scratch-dir>` directive carries a TEST-OWNED scratch directory as its first argument (`requireScratchDir`, `scratchDirRequiringBehaviors`): `HOP_STATE_DIR`'s tree is HOP's own, so observation dumps, message bodies a principal authors (the worker-hold barrier question, `fixtureHoldMarker`) and the reviewer's one-shot marker file all live there instead — Phase 2 solo behaviors (`submit-valid`, `submit-stale`, `submit-twice`, `exit-without-submitting`, `exec-keep-pid`) are UNCHANGED and carry no such argument. New worker behaviors: `worker-implement` (drains its mailbox — `drainMailbox` — then submits, the section 5 mailbox rule applied uniformly), `worker-hold` (implements, sends one `question` to `manager` carrying `fixtureHoldMarker`, loops `hop msg wait` for the matching `reply-to` answer — never accepting an unrelated delivery — acks it, drains, then submits: the messaging-based barrier a real pane scenario releases with one test-owned `hop answer`, since typed pane input is forbidden by design), and `worker-conflict` (`commitConflictingChange`, `TestRealProcessIntegrationConflict`: writes `env["HOP_TASK_ID"]` — not a caller-supplied argument, since the manager's own directive rendering always appends the scratch directory as this behavior's one argument, leaving no room for a second — to one FIXED, shared filename, so two independent tasks using it, each from its own worktree branched off the same integration head, produce a genuine git merge conflict once both reach integration). The `idle-self-kill` behavior is SOLO-only (`scratchDirRequiringBehaviors`), exactly the unnamed default's own behavior (submit nothing, stay alive) but scratch-dir-registered so the self-kill watcher gets wired for a solo crash-recovery scenario (`resume_test.go`'s `startSelfKillableFixtureRun`/`endWithSelfKill`): `fixtureWorkerBrief`'s solo directive line carries no separate scratch-dir argument of its own, so the scratch directory is folded directly into the behavior string it renders verbatim (`"idle-self-kill <scratchDir>"`) rather than needing new plumbing. `runManager` parses its scripted plan from the brief (`parseManagerScript`/`managerScript`/`managerTask`/`managerAnswerRule`, the `TASK`/`ANSWER`/`FIX` line grammar documented on `managerScript`'s own doc comment; `fixtureManagerBrief`/`fixtureManagerTask`/`fixtureManagerAnswer` are this file's own builders for it), creates tasks and closes the plan through the real `hop task create`/`hop plan close`, then spends its entire remaining lifetime in the section 7 idle-poll loop (`hop msg wait`) via `handleManagerMessage`: a `question` is answered from the scripted table or relayed to `human` (`--relay-of`); a human `answer` is forwarded using only the envelope's own `origin` field (`ErrRequestConflict`-safe re-derivation, never a guess); an `info` notice naming `needs-rework` (`parseNeedsReworkLabel`) triggers `hop task retry`: an ANCHORED, exact-line match against BOTH production renderers' shapes, since neither pins a line position (design section 7 promises only "a needs-rework notice") and a match restricted to line 1 alone silently stops retrying `renderIntegrationNotice`-shaped needs-rework, hanging `TestRealProcessIntegrationConflict`/`TestRealProcessCombinedCheckFailure` for their full `featureRunTimeout` against a real herdr server — `matchTaskNeedsReworkLine` recognizes `task t<seq> needs-rework` at line 1 (`usecase_featurecheck.go`'s `renderTaskNotice`: worker interruption, per-task check failure) OR, via `matchIntegrationLine`, at line 2 when line 1 is exactly `integration <id> <state>` with state one of `integrationNoticeStates`'s own retyped tokens, `conflicted`/`rolled-back` (`usecase_featuresettle.go`'s `renderIntegrationNotice`: merge conflict, combined-check failure/rollback) — never a substring search, so a reason or evidence line merely naming "needs-rework" cannot match; any OTHER info notice (the review-verdict acceptance notice included — its body is only the reviewer's own raw reasons text, per `sqlite/review.go`'s `persistVerdictAcceptance`, carrying no marker at all) triggers `statusOutcomeForNotice`, the standing hop-status-polling instruction of design section 7/8's manager verdict channel (STATUS-1, landed): it runs `hop status -C <repo> -run <run>` ONCE, parses every `shortfall: verdict-rejected review=<id> subject=<oid> reasons=<path>` line with `parseVerdictRejectedLines` (STATUS-1's own grammar, `internal/app/grammar.go`'s `GrammarVerdictRejectedLine` mirrored — retyped, never derived — including `strconv.Unquote` for a `safeRenderExternal`-quoted path), and plans exactly one fix task only when a shortfall's reasons path equals THIS notice's own body path (`msg.BodyPath`) — the correlation rule that identifies which review a marker-free acceptance notice reports. `plannedFixReviews` (keyed by review id) ensures a re-served or redelivered notice for a review already acted on never plans a second fix. An `evidence-inconsistent` shortfall (`statusHasEvidenceInconsistentShortfall`) is never a rejection either, but per the verdict-channel paragraph's own remaining rule it is NOT simply ignored: the manager sends one `question` to `human` (`fixtureEvidenceInconsistentQuestionBody`) asking them to inspect the run, and plans no fix. `runReviewer` resolves its behavior from the run's FROZEN reviewer role artifact (a review task's own assignment carries no manager-authored free text at all); `reviewer-reject-once` tracks its one-shot state as a marker file under its own scratch directory (never `HOP_STATE_DIR`), shared correctly across every review task's separate reviewer process since the frozen role artifact — and the scratch directory it names — is identical for all of them; every behavior retries `hop result submit`/`hop review submit` on a `transient` first line, pausing `fixtureRetryInterval` (200ms) between attempts, draining before each verdict retry; every OTHER mutating manager/worker verb (`task create`/`task retry`/`plan close`/`msg send`, wherever this file calls them) goes through `runHopCLIRetryable`, the same `transient:`-prefix retry generalized past `hop result submit`/`hop review submit` (a manager/message verb legitimately gets a retryable `transient:` line, never `refused:`, while the run is still launching or resuming) — it reuses the SAME `--request-id` (`newRequestID`, a fresh UUIDv4-shaped id minted once per logical call) on every retry of that call, and never retries a `refused:` line. A cold-relaunch invocation is detected by `isResumeInvocation` (any exact `--resume` element — detection only, shared by every role since `composeSessionArgvTail` composes the identical tail regardless of role) and validated by `requireResumeShape` against HOP's one supported shape, exactly `<exe> --resume <native-ref> <continuation prompt>` with a UUID-shaped reference; every other resume-shaped argv fails loudly (`TestFixtureWorkerResumeShapeRejections`). `testContinuationPrompt` mirrors internal/app's `renderContinuationPrompt` byte for byte for the solo/implementer shape (`TestFixtureWorkerResumeContinuation` drives it directly); `roleprompts_test.go` mirrors the reviewer/manager shapes the same way. `TestFixtureWorkerSelfKillOnControlFile` drives a `worker-hold` implementer directly until it sends its own barrier question, then writes the self-kill control file `featureharness_test.go`'s `killSession` also writes, asserting SIGKILL of the fixture's own doing; its captured stdout/stderr use `syncOutput` (a mutex-protected buffer with a `snapshot` method), never a bare `strings.Builder`, since a live poll reads it concurrently with os/exec's own output-copy goroutine — a real data race under `go test -race` otherwise — and a `sync.Once`-guarded `wait()` closure is the sole `cmd.Wait` owner on every path (the happy path and an early-failure `t.Cleanup`), so a readiness-poll timeout still reaps the directly-started child rather than leaving it and its output-copy goroutine dangling. |
| [fixtureprincipal_test.go](fixtureprincipal_test.go) | `fakeHopSource`, `buildFakeHopStub`, `isSupportedMessageKind`, `isSupportedAddress`, `fakeMessageBlock`, `writeFakeHopMsgScript`, `newFakeHopTaskCounter`, `readFakeHopLog`, `logHasLine`, `countLogLinesWithPrefix`, `requestIDFromLogLine`, `managerScriptDispatchFixture`, `buildManagerScriptDispatchFixture`, `runVerdictCorrelationCase`, `runNeedsReworkCase`, `fakeHopValidEnv`, `TestFixtureManagerScriptDispatch`, `TestFixtureManagerRetriesTransientTaskCreate`, `TestFixtureManagerNeverRetriesRefused`, `TestFixtureManagerVerdictRejectedCorrelation`, `TestFixtureManagerNeedsReworkNoticeShapes`, `TestFixtureReviewerRejectOnce`, `TestFixtureWorkerHoldBarrier`, `TestFakeHopRejectsInvalidInvocations` | Phase 3 slice 7a's isolated (no herdr) unit coverage of the fixture principal's new manager/reviewer/worker-hold parsing and dispatch, driving the compiled principal (`buildFixtureWorker`) directly against `fakeHopSource` — a minimal scriptable `hop` stand-in returning fixed success lines for every worker-plumbing verb (`task create`'s own line increments a caller-provided counter file so dependency ids are distinguishable; `status`'s own guard-shortfall line reproduces STATUS-1's real rendering verbatim, scripted via `FAKE_HOP_STATUS_SHORTFALL` — `verdict-rejected` rendered from `FAKE_HOP_STATUS_REVIEW`/`FAKE_HOP_STATUS_SUBJECT`/`FAKE_HOP_STATUS_REASONS`, or value-free `evidence-inconsistent`; a `task create` call also honors `FAKE_HOP_TASK_CREATE_REFUSED`/`FAKE_HOP_TASK_CREATE_TRANSIENT_COUNT` for the transient-retry tests below), serving a test-authored ordered script for `msg wait`/`msg next` (`fakeMessageBlock`/`writeFakeHopMsgScript`, blocks delimited by a line reading exactly `---`), and logging every invocation's full argv (`readFakeHopLog`) for assertion — `logHasLine`/`countLogLinesWithPrefix` match that log by prefix/suffix rather than an exact line once an invocation carries a fresh per-call `--request-id` (`requestIDFromLogLine` extracts it for cross-call comparison). `TestFixtureManagerScriptDispatch` proves the complete manager-feature dispatch in one run: dependency-correct task creation, plan close, a scripted barrier question relayed to the human, a human answer forwarded through the relay chain by `origin` alone, a needs-rework notice retried against the right task id, and an unrecognized info notice (deliberately unmarked reviewer prose) driving a scripted `hop status` check whose verdict-rejected shortfall's reasons path equals that exact notice's own body path into a fix task plus a second plan close. `buildManagerScriptDispatchFixture`'s one-task manager fixture backs `TestFixtureManagerRetriesTransientTaskCreate`/`TestFixtureManagerNeverRetriesRefused` and `TestFixtureManagerVerdictRejectedCorrelation` (`runVerdictCorrelationCase`, its own shared driver): `TestFixtureManagerRetriesTransientTaskCreate` proves `runHopCLIRetryable` retries a scripted `transient:` `hop task create` response until it succeeds, reusing the IDENTICAL `--request-id` (`newRequestID`, fixtureworker_test.go) on every attempt of the same logical call; `TestFixtureManagerNeverRetriesRefused` proves a hard `refused:` line is never retried — exactly one `hop task create` call, then the manager fatals; `TestFixtureManagerVerdictRejectedCorrelation` proves the manager's STATUS-1 correlation rule in four subtests — a matching reasons path plans one fix, a different path plans none, the SAME review's notice delivered twice (identical body path) plans only one fix total (never a second), and an `evidence-inconsistent` shortfall plans none. `TestFixtureManagerNeedsReworkNoticeShapes` (`runNeedsReworkCase`, its own shared driver) proves `parseNeedsReworkLabel` recognizes BOTH production notice renderers' shapes with an anchored, exact-line match in five subtests — `renderTaskNotice`'s task-line-first shape, `renderIntegrationNotice`'s task-line-second shape for both `conflicted` and `rolled-back`, a task line at an unrecognized position, and a reason line merely mentioning "needs-rework" — the last two proving a substring search would wrongly match where the anchored one does not; restricting the anchored match to line 1 only fails exactly the two `renderIntegrationNotice`-shaped subtests, confirmed directly. `TestFixtureReviewerRejectOnce` drives the compiled reviewer TWICE with the same `HOP_STATE_DIR`/`HOP_RUN_ID` (as two separate review tasks' reviewer sessions always are), proving the first invocation rejects and durably marks its scratch directory while the second — a fresh process — finds the marker and approves. `TestFixtureWorkerHoldBarrier` drives a feature-mode `worker-hold` implementer directly, proving it sends exactly one barrier question, accepts only a reply-to-matching answer, and drains-then-submits after release. Every scripted `fakeMessageBlock` id (and every reply-to/relay-of value referencing one) across this file is a canonical lowercase UUID: `msg ack`'s positional argument and `msg send`'s `--reply-to`/`--relay-of` are UUID-validated (`identity.ParseMessageID`'s real contract), `msg send`'s `--kind`/`--to` are checked against the real supported sets (`isSupportedMessageKind`/`isSupportedAddress`, mirroring `internal/app/usecase_message.go`'s `parseMessageKind`/`parseAddress`) with a non-empty resolved body required, `msg wait`'s `--timeout` is a real `time.Duration` flag (a nonsense value fails to parse, exit 2), and `requireCallerContext` requires an absolute `HOP_STATE_DIR` (mirroring `cmd/hop/stateroot.go`'s `requireWorkerStateRoot`) — request ids stay opaque, as in production. `TestFakeHopRejectsInvalidInvocations` (`fakeHopValidEnv`, 10 subtests) drives the compiled fake directly, proving each invalid shape above exits 2. |
| [roleprompts_test.go](roleprompts_test.go) | `testReviewerInitialPrompt`, `testReviewerContinuationPrompt`, `testManagerInitialPrompt`, `testManagerContinuationPrompt`, `TestRolePromptMirrors` | Phase 3 slice 4's role-prompt mirrors: byte-for-byte reproductions of internal/app/usecase_sessionlaunch.go's reviewer and manager prompt renderers (unexported there; the fixture principals slice 7 builds are standalone programs that cannot import internal/app), exactly as `testAssignmentPrompt`/`testContinuationPrompt` mirror the two solo shapes. `TestRolePromptMirrors` (no herdr needed) pins the mirrors against the same retyped golden literals internal/app's `TestGoldenRolePrompts` pins the renderers against, so either side drifting fails a test before the two desynchronize |
| [resultsubmit_test.go](resultsubmit_test.go) | `fixtureRun`, `newFixtureRunEnv`, `startRun`, `startFixtureRun`, `dbPath`, `taskAndAttemptIDs`, `currentIncarnationID`, `requireRunState`, `containsState`, `TestRealProcessDuplicateSubmissionAfterCompletion` | Phase B's shared setup: `newFixtureRunEnv` builds and starts a disposable server plus the fixture worker installed as `claude`; `startRun` runs `hop run` against a caller-built (and possibly pre-mutated) fixture repository through the `started` line; `startFixtureRun` composes both for the common case. `fixtureRun` tracks the live controller (`controller`/`controllerName`, reassigned together by any scenario that kills and replaces it) plus `dbPath`/`taskAndAttemptIDs`/`currentIncarnationID` helpers for direct `hop result submit`/sqlite assertions, and `requireRunState` (bounded to `runEndToEndTimeout`) fails with the worker pane's scrollback captured first. `TestRealProcessDuplicateSubmissionAfterCompletion` proves section 7 step 3: a fresh submission carrying the already-accepted commit and summary is accepted as `duplicate`, disturbing nothing |
| [featureharness_test.go](featureharness_test.go) | `featureRunTimeout`, `featureConfigTOML`, `featureFixtureOptions`, `newFeatureFixtureRepo`, `featureRun`, `startFeatureRun`, `requireRunState`, `taskState`/`requireTaskState`, `currentAttempt`, `attemptCount`, `attemptState`/`requireAttemptState`, `sessionForAttempt`, `managerSessionID`, `sessionState`/`requireSessionState`, `paneForSession`/`requirePane`, `foregroundPID`, `killSession`, `relayedQuestionFor`, `answerHuman`, `messageBodyContent`, `requireManagerNoticeFirstLine`, `worktreeForAttempt`, `launchClaimPID`, `resultCount`, `integrationForTask`/`requireIntegrationState`, `integrationHead`, `reviewVerdict`, `planClosed`, `reviewTaskID`/`requireReviewTask`/`requireReviewTaskOtherThan`, `transitionCount`, `transitionAt`/`requireTransitionAt`, `waitForTaskBySeq`/`requireTaskBySeq`, `integrationWindow`/`allIntegrationWindows`/`requireSerialIntegrationOrder` | Phase 3 slice 7a's reusable base for every feature-mode real-process scenario (7b/7c extend it on this tip without editing 7a's own scenarios): `newFeatureFixtureRepo` builds a feature-mode fixture repository — the Phase 2 check/CHECK_RESULT contract plus `.herdr-orchestrator/roles/{manager,implementer,reviewer}.md` (the reviewer's alone carries a `FIXTURE-BEHAVIOR:` directive with its own `ScratchDir`, per the manager/implementer's behavior traveling through the brief and task instructions instead) and a `[workflow] mode=feature` config (`featureConfigTOML`) with short `[messages]` timeouts so a scenario's idle-poll loops advance quickly. `startFeatureRun` runs `hop run --workflow feature` through the `started` line, mirroring `resultsubmit_test.go`'s solo `startRun`. Every other function is a read-only poll against the run's own sqlite store (`querySQLite`, never `hop status`, except where a scenario deliberately drives it) or a real one-shot CLI invocation (`answerHuman` drives `hop answer`, the only way a barrier is ever released — never typed pane input); every `require*` bounds its poll by `featureRunTimeout` and fails naming what it was still waiting for. `relayedQuestionFor` matches a relayed human question by its RELAY CHAIN (`messages.relayed_from`) traced back to the barrier's own originating session — never by delivery/creation order — since more than one task's barrier can be held (and relayed) concurrently. `transitionAt`/`requireTransitionAt` and `allIntegrationWindows`/`requireSerialIntegrationOrder` prove causal ordering directly from the `transitions`/`integrations` journal (fixed-width canonical UTC timestamps, lexically comparable) instead of a racy snapshot poll — the dependency-release and slot-wait assertions in particular need this, since both are otherwise indistinguishable from a passing snapshot taken at the wrong moment. `foregroundPID` mirrors `pgroup_test.go`'s `fixtureRun.workerForegroundPID`, generalized to any session/role; `killSession` SIGKILLs a session's own claimed pid and waits for its pane to close itself (S6) |
| [featurerunendtoend_test.go](featurerunendtoend_test.go) | `TestRealProcessFeatureRunEndToEnd` | Design section 11 scenario 1 (the section 1 evidence row): four tasks (t1, t3, t4 initially eligible, t2 depending on t1) under `MaxWorkers=2`, `worker-hold` barriers on t1 and t3. Asserts, each independently: t1 and t3 reach exactly `active` immediately (a held worker cannot progress further, so this is exact, not a forward-looking superset check); t4 and t2 stay `ready`/`pending` while both slots are held (the store's own bounded-concurrency transaction, not a timing race); token metadata manager-first (`agentTokens`/`assertManagerFirst`, reused from `presentation_test.go`) while manager+2 workers are live; releasing t1's barrier BY ITS OWN RELAY CHAIN (never queue order, since t3's is held concurrently) and asserting the relayed body carries the original marker unchanged; t4's session beginning to launch no earlier than t1's session was OBSERVED terminated (`transitionAt`, the slot-freed-by-retirement assertion, decoupled from process-exit assumptions); t2 becoming ready no earlier than t1's own `integrated` transition (the dependency assertion, distinct from mere completion); serial integration order (`requireSerialIntegrationOrder`); the plan closed before the review task appears; review approval bound to the exact final integration head; completion with the manager session observed `terminated`; and every guard row (plan flag, every task AND its integration row `integrated`) re-checked at the run's own terminal state |
| [workerinterruption_test.go](workerinterruption_test.go) | `workerInterruptionNoticeFirstLine`, `TestRealProcessWorkerInterruption` | Design section 11 scenario 4 (reference trace 4): both attempts of a single task use `worker-hold` so the kill point is fully deterministic (blocked indefinitely on its own barrier, killable at will with no race against its own submission) and so attempt 2 — reusing the SAME task instructions and therefore the SAME scripted behavior — is releasable and completes the identical message-barrier way `TestRealProcessFeatureRunEndToEnd` already proves. A LIVE controller (no crash, no resume) reconciles the kill on its own next scheduling pass: attempt 1 interrupts, the task moves to `needs-rework`, the session terminates — asserted with an exact `transitionCount` of 1, ruling out a double-fired settlement — and the manager's notice is asserted against `workerInterruptionNoticeFirstLine`, EXACTLY `renderTaskNotice`'s own rendered text (`requireManagerNoticeFirstLine`, retyped never derived, per the manager directive that a format drift must fail here). The scripted manager retries automatically on the notice; attempt 2 gets a fresh worktree from the current integration head (`repo.Base`, since nothing has integrated yet) distinct from attempt 1's, is released and completes; asserts full provenance for BOTH attempts — worktrees, sessions, launch claims (`launchClaimPID`, needing no live pane, unlike `foregroundPID`) and results (`resultCount`: 0 for the interrupted attempt, 1 for the completed one) |
| [reviewerrejection_test.go](reviewerrejection_test.go) | `verdictRejectedShortfallToken`, `verdictStaleSubjectShortfallToken`, `verdictRejectedLinePrefix`, `verdictRejectedLine`, `parseVerdictRejectedLine`, `TestRealProcessReviewerRejection` | Design section 11 scenario 5 (reference trace 5), online now that defect STATUS-1 has landed on main: `reviewer-reject-once` rejects R1 without completing the run; `parseVerdictRejectedLine` parses the SOLE `shortfall: verdict-rejected review=<id> subject=<oid> reasons=<path>` line from a real `hop status -run` rendering (retyped from `internal/app/grammar.go`'s `GrammarVerdictRejectedLine`, unquoting a `strconv.Quote`d reasons path exactly as the fixture manager's own parser does), and its three fields are asserted against the STORE directly — R1's own accepted review row id (`reviews.id`, distinct from R1's task id), R1's subject commit (`headAfterT1`) and its `reasons_path` column — not just the bare kind token. The manager plans a fix task, whose integration moves the head; immediately after, `hop status` is asserted to never again name `verdict-rejected` for R1 (it reports `verdictStaleSubjectShortfallToken` once the head has moved and no new review is current, or no verdict shortfall at all if a fast new review already superseded it — logged, not asserted, since either is correct per design). A new review task (`requireReviewTaskOtherThan`, distinct from R1) approves against the NEW head; the run completes; and R1's stale verdict (bound to the superseded head) is confirmed never able to satisfy the guard |
| [lifecycle_test.go](lifecycle_test.go) | `leaseTimeLayout`, `killControllerLeader`, `waitForLeaseExpiry`, `TestRealProcessDetachDistinctFromStop` | `killControllerLeader` SIGKILLs a controller's leader without touching its anchor; `waitForLeaseExpiry` polls a run's own `run_leases` row (mirroring `internal/adapters/sqlite`'s unexported time layout, since this package may not import that adapter) until it is no longer held past its recorded expiry — the real, unavoidable cost every crash-recovery scenario in this package pays. `TestRealProcessDetachDistinctFromStop` proves a SIGINT to a live controller releases the lease at once (never waiting out the TTL) and disturbs neither run state nor the worker |
| [check_test.go](check_test.go) | `statusDetailValues`, `TestRealProcessFailedCheckRetainsArtifacts`, `TestRealProcessGitDependentCheckSucceedsInDetachedCheckout`, `TestRealProcessSubmoduleRepositoryFailsClearly` | `statusDetailValues` scrapes every occurrence of a repeated `hop status -run` line (`artifact:`/`evidence:`), since `parseStatusDetail`'s flat map keeps only the last. The three scenarios: a deterministic failing check ends the run `failed` with both stdout/stderr retained at their rendered paths; `newFixtureRepoGitDependentCheck`'s check-git.sh passes inside the real detached-checkout candidate isolation; `newFixtureRepoWithSubmodule`'s submodule reference is rejected by `internal/app`'s own gitlink scan BEFORE any checkout or check argv ever runs (no evidence retained — check-submodule.sh never executes) |
| [stop_test.go](stop_test.go) | `slowCheckScriptName`/`slowCheckScriptSource`, `withSlowCheck`, `TestRealProcessStopWithTerminationObserved`, `TestRealProcessStopInterruptsRunningCheckGroup` | `withSlowCheck` rewires a fixture repository's `[check]` command to a `sleep 20` script and commits it, giving a scenario a check execution long-lived enough to interrupt mid-flight (confirmed via a durable `check_exec_claims` row, no race). The two scenarios drive `hop stop` from a separate one-shot process (observing, since the live controller holds the lease and drives the stop itself): a plain running worker terminates cleanly to `stopped` with the worktree preserved; a running check group is interrupted by the loop's own context cancellation (CommandRunner kills the group), which DriveStop's retry then settles as an *unknown* outcome (not a distinct "interrupted" one — an empty group alone cannot distinguish "killed a moment ago" from any other cause), while task/attempt still move to `interrupted` and the run reaches `stopped` |
| [launch_test.go](launch_test.go) | `TestRealProcessDuplicateLaunchInvocation` | Design section 6's duplicate-launcher rule: caught while the attempt is still `launching`/`relaunching` (`PrepareLaunchExec` refuses on attempt state before it ever reaches the claim's pid check, so this is the only window the different-pid refusal is observable in), a second `hop launch` invocation for the same incarnation — a different pid by construction — is rejected, the claim is unchanged, and the original launch settles normally afterward |
| [execfailure_test.go](execfailure_test.go) | `installBrokenClaudeStub`, `TestRealProcessExecFailureSettlesExecFailed` | A deterministic (non-racy) exec failure: the resolved `claude` stub is a plain executable-bit-set text file with no shebang, which still passes `PrepareLaunchExec`'s regular-file check (so the claim is written) but fails the kernel's `execve` (ENOEXEC). The claim settles `exec_failed`, attempt/task/run all fail immediately (unrecoverable on a first launch, never a detour through `running`), and a `hop stop` against the already-failed run is a clean no-op reporting the run's real state |
| [forkingwrapper_test.go](forkingwrapper_test.go) | `forkingWrapperSource`, `buildForkingWrapper`, `TestRealProcessForkingWrapperAfterExec` | The "wrapper forks the real harness after exec" fail-closed topology (design section 6): a wrapper execs preserving the launch claim's pid, forks the real fixture worker as a child sharing its own argv with `argv[0]` forced to the wrapper's own invocation name (a bare `select {}` here trips Go's own deadlock detector — the wrapper waits on the child instead). The pane's foreground group then holds the wrapper (the claim's pid, itself matching executable identity and marker — its exec'd argv carries them), the forked child (matching identity and marker under a DIFFERENT pid) and the fixture's own MCP stand-in grandchild, so this is the live wrapper-precedence proof: `CorroborateSettlement` classifies the matching different-pid member `SettlementForkingWrapper` even though the claim-pid member also matches, the claim stays `exec_pending`, and the controller reports "needs interaction" |
| [pgroup_test.go](pgroup_test.go) | `fixtureRun.workerForegroundPID`, `TestRealProcessSettlementWithMCPGroupMembers` | The executed multi-member-settlement probe under the production transport, reproducing the live defect shape (`TestLiveClaudeDefaultProfileRun` on main 3be1748: Claude Code 2.1.270 spawns MCP servers — `npm exec ...`, `node ...`, app-installed MCP server binaries — into its own pgrp, and settlement reading only `foreground[0]` left the claim `exec_pending` for the whole bound): an idle-behavior worker (no submission, so the early-acceptance path can never mask a stuck settlement) settles purely through corroboration against a foreground group of worker + MCP stand-in, and the scenario asserts only position-free facts — the settled claim pid, the group id equal to the worker's pid, a multi-member group with the claimed pid among the members, every non-claim member the marker-free MCP stand-in, the worker member's verbatim stub argv[0] — while RECORDING (t.Logf, never asserting) the observed member order and the claimed pid's index, since position carries no semantics and an observed ordering is not a guarantee; the deterministic position coverage lives in internal/app's decision-table vectors. `workerForegroundPID` is the shared helper every scenario uses to locate the worker by CLAIM pid, never by listing index |
| [checkdeath_test.go](checkdeath_test.go) | `leaderExitCheckScriptName`/`Source`, `configWithTimeout`, `checkGateScriptName`/`Source`, `withGatedCheck`, `releaseCheckGate`, `TestRealProcessCheckLeaderExitWithLiveChildren`, `TestRealProcessCheckDeathUnknownOutcomeRepeatable` | `configWithTimeout` renders a `[check]` config with an explicit timeout/`repeatable` (unlike `fixtureConfigTOML`'s fixed 30s/false). "Leader exit with live children": a check backgrounds a child and exits, so its stdout/stderr pipes never see EOF on their own — `CommandRunner`'s capture hangs until the (short, here) timeout force-kills the whole group, landing the operation `reconciling` with the timeout-driven cancellation named as evidence (the exact further auto-resolution latency to a settled outcome is not pinned here — a live controller's own recovery cadence for an already-reconciling operation was not established as fast/bounded within a reasonable test iteration). "Unknown outcome, repeatable": getting a genuinely unknown (not a definite failure) outcome needs the check's process group gone with **no live watcher** having captured what happened — directly `SIGKILL`ing a check under a live controller instead produces a definite `exit(-1)` failure — so the whole controller is crash-killed instead; `withGatedCheck` wires a check that blocks reading a FIFO (`checkGateScriptName`, `exec sh check.sh` once released, mirroring `gitDependentCheckScriptSource`'s own hand-off) so the kill is provably concurrent with a still-running check rather than racing a fast, undelayed check that can finish and be observed first under load — `releaseCheckGate` lets it proceed only after the kill is confirmed, then immediately swaps the FIFO path for an empty regular file (safe once the write-open call returns, since a FIFO open resolves its path before it blocks, so an already-rendezvoused reader is unaffected by a later rename) so `check.repeatable=true`'s automatic retry — which reruns this same committed script — finds an ordinary already-at-EOF file on its own `cat` and never blocks a second time; `hop resume` then finds the first execution's group naturally empty with nothing recorded, settles it unknown, and requeues it automatically to a normal completion |
| [resume_test.go](resume_test.go) | `testLaunchArgvDigest`, `TestRealProcessControllerKillResumeWarmReattach`, `coldRelaunchAfterCrash`, `TestRealProcessConfirmAbsentColdRelaunchNonRestart`, `TestRealProcessStaleSubmissionFromRetiredIncarnation`, `TestRealProcessConfirmAbsentRefusedAfterServerRestart` | Design section 5's resume cases against real hard kills. Warm reattach: only the controller dies (worker/pane untouched); once the lease expires, `hop resume` reattaches to the same live occupant (same pid, same incarnation, no new session). `coldRelaunchAfterCrash` additionally SIGKILLs the worker itself (its no-shell command pane closes itself, S6, giving both required absence conjuncts) before killing the controller, then drives `hop resume --confirm-absent` to a cold relaunch — shared by the non-restart-litmus-test and stale-submission scenarios. The non-restart scenario also pins the relaunched claim's exact argv: it recomputes the canonical `hop-argv-v1` digest (`testLaunchArgvDigest`) over `<claim executable> --resume <native-ref> <continuation prompt>` — the prompt rebuilt byte for byte from the run's assignment path and the hop path the relaunched worker's own observation dump records — and requires it to equal `launch_claims.argv_digest`, so a prompt-less relaunch argv fails. `TestRealProcessConfirmAbsentRefusedAfterServerRestart` repeats the identical crash but restarts the herdr server first: continuity breaks, the attestation is still journaled, and the relaunch is refused |
| [trustseed_test.go](trustseed_test.go) | `TestRealProcessLaunchSeedsWorkspaceTrust`, `TestRealProcessLaunchWithoutProfileConfigNotSeeded`, `trustFixtureConfig`, `seededTrustConfig`, `fixtureRun.resolvedWorktreePath`, `fixtureRun.seedEvidence` | The launcher-boundary workspace-trust pre-seed against a real run: a fixture `.claude.json` in the worker pane's isolated HOME ends up with exactly `projects[<symlink-resolved worktree path>].hasTrustDialogAccepted = true` and every other byte preserved (asserted byte-exact), the launch claim's `seed_evidence` and the `hop status -run` trust-seed line both carry the seeded evidence; and with no `.claude.json` at all, the claim records "not seeded: profile config absent", the run still completes, and no config file is created |
| [herdradapter_test.go](herdradapter_test.go) | `TestRealProcessHerdrAdapterWorkspaceAndWorktreeLabels` | Phase 3 slice 5: drives the herdr adapter's `CreateWorkspace`, `FindWorkspaceByLabel` and the labeled `CreateWorktree` end to end through `herdr.Runtime` itself (not raw wire calls, unlike the S8/S9 spikes) against a disposable test-owned server — a labeled workspace created and recovered by label, a not-found label proven non-erroring, and a labeled worktree checked out, HEAD-verified against its base and recovered through the same label descent |
| [spike_panevanish_test.go](spike_panevanish_test.go) | `TestSpikeVanishedPaneShapes`, `TestSpikeLabelSurvivesRestart`, `TestSpikeRenamedLabelSurvivesRestart`, `openGatedPane`, `requireLabelResolves`, `requireLabelAnswersNothing`, `requireServerLifetime`, `psStartSecond`, `renamePane`, `labelContinuitySamples`, `lifetimeStabilitySamples`, `serverLifetimePattern`, `requireVanishedShapes`, `waitPaneGone`, `vanishingPane`, `gatedPaneScript` | The executed probe behind the launch-ended decision row, its label-only variant, their server-continuity conjunct, resume's in-flight rule and the vanished-pane tolerance (docs/plan/phase-3-design.md sections 3, 4, 5 and 9), through the production `herdr.Runtime` and `herdr.Presentation` against a disposable server: a `layout.apply` `/bin/sh` command pane whose process exits, and one the test closes. The creation label answers for exactly the created pane on the first lookup after `OpenWorkerPane` returns and on every one of `labelContinuitySamples` back-to-back lookups while it lives. While the pane lives, its command pid is the pane's shell pid and foreground group id, the member at that pid reports exactly the argv `layout.apply` ran it with, the inspection is stamped with the answering server lifetime, `getpgid` and (on darwin, through `processSessionID`) `getsid` both return it (a session leader leading its own group), and the suite's own `listGroupMembers` lists it in that group; once the pane is gone, `pane.process_info` answers `pane_not_found` "pane not found", `pane.report_metadata` and `pane.close` answer `pane_not_found` "pane <id> not found", each adapter method reports `app.ErrPaneNotFound`, the creation label resolves nothing, and the group no longer lists the pid. `requireServerLifetime` pins `Runtime.ServerInstance`: on darwin the `herdr-server-lifetime/v1` token names the running server leader's pid and the start second `ps -o lstart=` reports for it (`psStartSecond`, independent of the adapter's kinfo_proc read) and stays identical across `lifetimeStabilitySamples` back-to-back reads; elsewhere it is empty. `TestSpikeLabelSurvivesRestart` restarts the server gracefully on the same roots: the first label lookup the new server serves finds the restored pane, which keeps the created public pane id and answers every later sample, runs a fresh process (never the original pid, whose group state is logged) and is stamped with the new lifetime, which differs from the old. `TestSpikeRenamedLabelSurvivesRestart` renames a live pane through Herdr's own `pane.rename` (`renamePane`): the creation label then answers nothing while the new name answers the pane, whose original process still runs; after a graceful restart the new name answers the restored pane under its created id with a fresh process, the creation label still answers nothing (`requireLabelAnswersNothing`, sampled), the lifetime has changed and the original pid is gone. Process observation goes through `listGroupMembers`, never the process adapter under test |
| [sessionid_darwin_test.go](sessionid_darwin_test.go), [sessionid_other_test.go](sessionid_other_test.go) | `processSessionID`, `sessionIDUnavailable` | GOOS-selected: `getsid(2)` through the syscall package on darwin; on every other platform no session id is read (the syscall package offers no `Getsid` there), and the probe skips its session-leader check with `sessionIDUnavailable` as the logged reason |
| [launchvanish_test.go](launchvanish_test.go) | `TestRealProcessManagerLaunchVanishesBeforeCorroboration`, `vanishingClaudeStub`, `featureConfigTOML`, `newFeatureFixtureRepo` | The real-process proof of the launch-ended row for the manager: `hop run --workflow feature` against a feature-configured fixture repository (feature mode plus the three role instruction files) whose `claude` stub is a shebang script that exits at once — executed as `/bin/sh`, so corroboration can never settle it whatever the timing. The live controller keeps running until it observes the pane and the claimed process gone, settles the manager's claim `exec_failed` with the fixed reason (the bound pane and claimed pid as evidence), fails the run through the terminal-failure path (the manager session terminated, the manager-launch cause recorded, never a transition to running), prints `launch failed` and exits 1 with no controller error. A worker dying before settlement is covered by `internal/app`'s tables and is left to the fixture-principal feature scenarios |
| [livehop_test.go](livehop_test.go) | `requireLiveHarness`, `installRealClaudeStub`, `liveTranscriptExists`, `TestLiveClaudeDefaultProfileRun` | Design section 9's one opt-in live scenario: real Claude Code (symlinked, not copied, as the `claude` stub) driven through the real `hop run`/`hop launch` pipeline in the operator's own default profile, with one small brief to a passing check, then a forced cold relaunch (`claude --resume <preassigned-uuid> "<continuation prompt>"`) continuation — interactive Claude Code never re-runs a pending turn on `--resume` (observed live), so this run passing is the verification that a restored session acts on the positional continuation prompt (native-harness-compat.md's Claude Code verified list, 2026-09-15 against 2.1.270). See "Live scenario" below |

## Invariants

- The developer's live Herdr session is never addressed: subprocess
  environments are allowlists built from scratch (no inherited `HERDR_*`),
  every server is a named session under a temporary `XDG_CONFIG_HOME`, and
  the suite only ever reads the real user-global registry files to prove
  they did not change.
- Every test server's harness config pins `[worktrees] directory` under
  that server's own scratch root (`harness_test.go`'s `testConfig`) — never
  the herdr default (`~/.herdr/worktrees`, which expands against whatever
  `HOME` the server is given). This closes a real leak (evidence (b)): a
  live run's `HOME` is deliberately the operator's own real home directory
  (see "Live scenario" below), and without this pin Herdr's own default
  would create worktrees inside it. Every fixture-repo constructor also
  registers a `t.Cleanup` (`worktreecleanup_test.go`'s
  `registerWorktreeCleanup`/`cleanupServerWorktrees`) that removes every
  worktree `git -C <repo> worktree list` reports for that repository through
  real `git worktree remove --force` + `git worktree prune` calls, as a
  second, independent layer on top of the directory pin itself.
- No fixed sleeps: readiness and completion are bounded polls against the
  socket, plugin log records or pane text.
- An invocation response is not success. Completion is the log record the
  returned log ID resolves to, asserted on status, exit code and output.
- Cleanup asks for a graceful `server.stop` through the test server's own
  socket, then `killProcessGroupThenReap` `SIGKILL`s the server's whole
  process group and reaps the leader with `Wait`, signaling the group while
  the leader is still unreaped so its pgid cannot have been recycled onto an
  unrelated process. `safeToSignalGroup` rejects any non-positive pgid, init
  (`1`) and the caller's own group before any signal is sent. After the
  leader is reaped, it polls signal-0-to-the-group for `ESRCH` (bounded, and
  a timeout fails the test) so a descendant reparented to init and still
  holding an artifact log file open cannot race artifact removal. Panes are
  closed by the IDs earlier responses returned. No process matching.
- Evidence (server output, action stdout, pane snapshots, plugin logs) is
  written under `$TMPDIR/hop-integration/<test>-*` and retained only when
  the test fails; the path is logged.
- When no herdr binary is installed the suite skips with the explicit
  reason `real-process suite skipped, not run: ...`. GitHub-hosted CI
  runners have no herdr binary, so the CI gate does not run this suite;
  the workflow prints the skip lines rather than implying coverage.
  `HOP_TEST_HERDR_BIN` pins a specific binary.
- These tests prove behavior against herdr 0.9.0 on this machine; they are
  compatibility evidence for exactly the binary they ran against.
- `pane.process_info` foreground members carry NO positional semantics,
  and no test may read `ForegroundProcesses[0]` as "the worker". Herdr
  reports every member of the foreground process group in raw platform
  listing order — macOS: unsorted `proc_listpids`
  (repos/herdr/src/platform/macos.rs, `foreground_job`); Linux: ascending
  pid (linux.rs, `foreground_process_group_members_with`); mapped
  verbatim by the API (repos/herdr/src/app/api/panes.rs,
  `handle_pane_process_info`) — and the fixture worker (like real Claude
  Code 2.1.270 with its MCP servers) spawns a same-pgid child before it
  is ready. The observed macOS order has listed children first (the live
  probe saw an MCP server at index 0) — an observation, never a
  guarantee, so no test asserts any position either way. Scenarios
  locate the worker by the launch claim's recorded pid via
  `fixtureRun.workerForegroundPID` (the live scenario, which runs outside
  `fixtureRun`, reads `launch_claims.pid` directly), and
  `TestRealProcessSettlementWithMCPGroupMembers` records the observed
  shape and order.

## Live scenario

`TestLiveClaudeDefaultProfileRun` (livehop_test.go) is the one test in this
suite that runs a real harness against a real provider and is never run by
this repository's own automated verification — a human runs it
deliberately via `make test-live`.

- **Gate**: `HOP_LIVE_HARNESS=1` opts in (unset skips with a one-line
  reason, matching S4's existing gate); once opted in, a missing `claude`
  binary is a hard failure, not a skip.
- **Profile**: the worker pane's `HOME` is the operator's own real home
  directory by default (`os.UserHomeDir()`), so the launched harness uses
  its already-authenticated default profile at `<home>/.claude` — the
  sanitizing launcher still strips `CLAUDE_CONFIG_DIR` unconditionally, so
  nothing here can redirect it. Everything else (the herdr server, the HOP
  state root, the fixture repository) stays isolated in temporary roots
  exactly as in every other test. `HOP_LIVE_HARNESS_HOME` overrides the
  profile directory (must be an absolute, existing directory, or the test
  fails clearly) — point it at a profile prepared specifically for this
  test if you would rather not use your daily one.
- **Cost and side effects**: this run makes real requests to the
  operator's configured provider and therefore costs real tokens. It
  leaves a real Claude Code session record under
  `<home>/.claude/projects/<encoded-fixture-worktree-path>/<uuid>.jsonl`
  for the temporary fixture repository's worktree — harmless (the fixture
  itself is removed on success like every other artifact) but real, and
  not something `make test-live` cleans up on the profile side.
- **Run it**: `make test-live`, or directly:
  `HOP_LIVE_HARNESS=1 go test -count=1 -v -run '^TestLiveClaudeDefaultProfileRun$' -timeout 20m ./test/integration`.
  `HOP_LIVE_HARNESS_HOME=/path/to/profile` prepends to either form to use a
  prepared profile instead of the real one.
- **Account identity env**: this test alone also passes `USER`/`LOGNAME`
  (and `LANG`/`LC_ALL` when set) from the test process's own environment
  into the server's `extraEnv`, on top of the `HOME` override above.
  `testServer.environ()` otherwise builds every subprocess's environment
  from scratch with no login shell, so a worker pane spawned this way
  inherits no `USER`/`LOGNAME` at all — confirmed live (`ps -E` on the
  worker showed `HOME`, `SHELL`, `TERM`, `XDG_*`, but neither). Claude
  Code's keychain credential item is keyed by the account name (`$USER`;
  see native-harness-compat.md's Claude Code "Credential storage" note), so
  a worker with the operator's `HOME` but no `USER` cannot find its own
  login and reports "Not logged in" even though the operator is
  authenticated. This is a harness/test-environment gap, not a HOP
  behavior — `internal/app/launchenv.go`'s strip matrix never names these
  variables, so HOP's own sanitizing launcher already passes them through.
  The ordinary suite's constructed environment (`testServer.environ`) is
  left untouched; only this live test sets them, and only with values read
  from the test process's own environment, never hardcoded.
- **Worktree isolation**: every test server's config pins
  `[worktrees] directory` under that server's own scratch root (see
  Invariants below) specifically because this test sets `HOME` to the
  operator's real home directory — without the pin, Herdr's own
  `~/.herdr/worktrees` default would expand against that real `HOME` and
  create worktrees in the operator's actual home. The test asserts the
  created worktree's path resolves under the scratch root as a direct
  check that the pin held. This test's fixture repository also gets the
  same worktree cleanup as every other (`worktreecleanup_test.go`'s
  `registerWorktreeCleanup`), which resolves every worktree to remove ONLY
  from `git worktree list` against the fixture repo itself — never by
  globbing `~/.herdr` — so even if the directory pin above were ever
  defeated, a worktree this run created outside the scratch root would
  still be found and removed (after independently confirming its `.git`
  points back at this fixture repository) rather than left in the
  operator's real home.
- **Diagnostics**: on both the first (real Claude) launch and the
  post-cold-relaunch continuation, every `hop status` poll that already
  knows the worker pane's binding also captures that pane's full foreground
  process list (pid, name, argv0, argv, cmdline, in Herdr's own order) via
  the same `pane.process_info` adapter call and rendering every other
  real-process scenario in this suite uses (`testServer.processInfo`,
  `renderProcessInfo`). The accumulated per-poll trail is saved as
  `first-launch-process-info.txt` / `second-launch-process-info.txt`
  regardless of outcome, and a snapshot immediately before the forced kill
  is saved as `worker-pane-process-info-before-kill.txt`; the pane
  scrollback is still saved on the two run-state failure paths as before.
  This closes a real gap: a prior live run that never settled left only
  the scrollback, with no record of which foreground process Herdr had
  actually reported at any point.

## Section 9 real-process scenario coverage

The transient early-submission path (design section 7 step 4) is exercised
opportunistically by `TestRealProcessRunEndToEnd` — the design's own
reference trace 1 makes either submission ordering legitimate, so that
scenario asserts the receipt-history invariant both orderings satisfy
rather than forcing one — and deterministically by `cmd/hop`'s own
`result submit` tests and the sqlite store's submission tests.

Design section 9's real-process row is organized here into three groups:
scenarios this task expresses and lands, scenarios judged expressible but
not attempted in this task's timeframe, and scenarios judged not
expressible against the unmodified `hop` binary without a pause/injection
hook added to production code (out of scope for this task; never added).
The not-expressible group's dispositions are already covered by
`internal/app`'s own decision-table unit tests (`decision_test.go`,
`usecase_launch_test.go`, `usecase_execboundary_test.go`).

### Covered

Every real-process scenario in the quick-reference table above: the
CI-safe end-to-end run and sanitized-exec assertions; duplicate submission
after completion; the transient early-submission path (both legitimate
orderings, see above); stale submission from a retired incarnation;
duplicate `hop launch` invocation; the deterministic (unsuppressed)
exec-failure case; the wrapper-forks-the-real-harness-after-exec
fail-closed topology (`TestRealProcessForkingWrapperAfterExec` — a
matching member under a different pid classifies forking-wrapper under
wrapper precedence, even though the wrapper member itself matches under
the claim's pid, and the claim fails closed exactly as designed);
multi-member settlement against the harness's own MCP-shaped process
group, with the observed member order recorded rather than asserted
(`TestRealProcessSettlementWithMCPGroupMembers`); stop with termination
observed (a plain worker, and
a running check group); controller kill + resume warm reattach;
confirm-absent cold relaunch (non-restart case) and its refusal after a
real server restart; detach distinct from stop; the
failed-check/git-dependent-check/submodule-repository check-pipeline
scenarios; check-death's leader-exit-with-live-children and
`check.repeatable`'s two-outcome contract; and the opt-in live-claude
scenario (compiles, skips cleanly by default, never run by this task).

### Expressible, but deferred (no production hook or race needed)

- **Server-restart positive-evidence retirement of an auto-relaunched
  occupant, the phantom-snapshot case, and the fail-closed
  same-kind-replacement case**: `spike_restore_test.go`'s S3 probe
  (`TestSpikeRestoreAutoRelaunchBypassesLauncher`) already proves Herdr's
  own auto-relaunch mechanism fires against a generically-named `claude`
  process, and `resume_test.go`'s `coldRelaunchAfterCrash` /
  `TestRealProcessConfirmAbsentRefusedAfterServerRestart` already compose
  `server.restart` with a crash-killed worker and controller cleanly. The
  missing piece is driving a real run through the auto-relaunch path
  itself (kill only the worker's OS process while leaving its recorded
  native session ref in place, so Herdr's own restore fires on server
  restart) rather than constructing the restored occupant by hand as the
  spike does — genuine additional engineering, not attempted in this
  task's timeframe.
- **A check-exec claim-write failure refusing to run**: expressible by
  holding an exclusive transaction against the run's own sqlite database
  from a second connection (`BEGIN EXCLUSIVE` via the `sqlite3` CLI, the
  same tool `querySQLite` already shells out to) for the whole window
  `hop check-exec` attempts `ClaimCheckExec` — no process-timing race
  involved, since the lock can be held for the entire invocation rather
  than landing inside a sub-millisecond window. Not attempted in this
  task's timeframe.

### Not expressible without a production pause/injection hook

- **Launcher killed after the claim write, before exec** (design section
  6): the window between `PrepareLaunchExec`'s claim write and the
  caller's `syscall.Exec` is a handful of Go statements in a single OS
  process — no external observer, even a tight in-process
  `pane.process_info` poll, can reliably land a signal inside it. Its
  disposition is no longer an indefinite wait: the claim stays
  `exec_pending` behind a pane that closes with the launcher, and the
  Phase 3 launch-ended row settles it once pane and process are both
  observed gone (`internal/app`'s
  `TestLaunchEndedRowResolvesDeferredClaimWindows`; the same row's
  real-process proof is `TestRealProcessManagerLaunchVanishesBeforeCorroboration`).
- **Exec failure with the `exec_failed` write itself suppressed**: the
  same class of problem one step later, between the exec failing and
  `FailLaunchExec`'s write landing. (The *unsuppressed* exec-failure case
  — the write lands normally — is expressible and covered by
  `TestRealProcessExecFailureSettlesExecFailed`.) The suppressed case now
  settles through the same launch-ended row as the case above.
- **Paused pre-exec launcher argv exclusion**: requires catching the
  launcher's own argv as the pane's foreground process before it execs —
  the identical race as the first case above.
- **Stop against a pre-exec launch claim specifically**: same root cause
  as the first case — reliably catching (and holding) the attempt in
  `launching` with an *unsettled* claim before issuing `hop stop` needs
  the same pause hook.
- **Check death after spawn, before the claim write**: the same class of
  sub-millisecond internal race as the first case, one boundary over (the
  window between `hop check-exec` starting and `ClaimCheckExec`'s write
  landing) — unlike the claim-write-*failure* case above (an externally
  held lock, no race), this needs the actual write to never happen at
  all, which only a timing race against the process itself could produce.
- **The fallback-transport shell-fork pid case**: `internal/app` never had
  a decision-table branch, config flag or fallback path that selected the
  send-text transport — `hop run`'s launch flow only ever used the primary
  `layout.apply` transport — and slice 6 deleted `Runtime.SendText` and
  its herdr adapter method entirely as dead, uncalled surface (the S1
  probe, `spike_launch_test.go`, still exercises the raw
  `pane.send_text` wire method directly through `Client.Call`, unrelated
  to the deleted port member).

## Phase 3 slice 7a real-process scenario coverage

Design section 11's exit-scenario row, this task's slice: `TestRealProcessFeatureRunEndToEnd` (scenario 1), `TestRealProcessWorkerInterruption` (scenario 4) and `TestRealProcessReviewerRejection` (scenario 5 — see its own quick-reference row and `reviewerrejection_test.go`'s doc comment; defect STATUS-1 has landed on main and the manager's own verdict-channel correlation logic is in place). Scenarios 2 (`RelayedQuestion`), 3 (`DuplicateAndAmbiguousDelivery`), 6 (`ControllerReconnectNoSecondClaimant`) and 7 (`InjectionFreeDelivery`), plus the additional messaging/controller/integration scenarios design section 11's "Layers" table names, are slices 7b/7c's on this tip — this task builds `featureharness_test.go` and the generalized fixture principal as their reusable base and does not implement them.

Defects found while implementing this slice, reported to and confirmed by the manager, tracked on separate branches:

- **STATUS-1 (FIXED, landed on main as `0e82d8c`, merged into this branch's `7e42e09`)**: `cmd/hop/statuscmd.go`'s `renderRunDetail` did not render `RunDetailView.GuardShortfalls`, `.Mailboxes`/`NeedsAttention` (the section 7 attention line), `.Tasks` or `.LatestIntegration`, despite `internal/app/usecase_status.go`'s `runDetailView` fully populating all four from the store — `hop status -run` could not show a reject-verdict guard shortfall, or anything else needed to detect one. Fixed on main: `hop status -run` now renders `featureDetailLines`'s six-section feature-mode block, including `EvaluateReadiness`'s guard shortfalls verbatim — `verdict-rejected` dispatches to `GrammarVerdictRejectedLine` (review id, subject commit, `safeRenderExternal`d reasons path), the same deterministic path (`reviewReasonsPath`) the accepting transaction wrote as the controller notice's own body. The fixture manager's own detection (`verdictRejectionMatchingNotice` in `fixtureWorkerSource`, this task's follow-up per the manager verdict-channel paragraph, `internal/app/templates.go`'s `renderManagerAssignment`) parses that exact line and plans a fix only when its reasons path equals the fetched notice's own body path, never planning a second fix from the same review — `TestRealProcessReviewerRejection` is unskipped and `TestFixtureManagerVerdictRejectedCorrelation` covers the correlation rule in isolation.

## Dependencies and ports

- Allowed inward imports: [internal/app](../../internal/app/AGENTS.md) and
  [internal/adapters/herdr](../../internal/adapters/herdr/AGENTS.md)
  (rule `test/integration`, category `integration-test`); the presentation
  and observation tests drive the `app` use cases through the real adapters,
  and wire-shape structs the assertions need are declared locally in the
  tests.
- Consumed/implemented ports: none; the suite drives `herdr.Client` directly.
- External libraries: `github.com/creack/pty` (test-only), used by the PTY
  smoke to attach a client on a pseudo-terminal — Go's standard library has
  no PTY. It is allowlisted only for this integration-test package
  (`testThirdParty` in the `test/integration` rule); the architecture
  checker's production-closure check proves no production package reaches it.
- External binaries: `herdr` (skipped when absent), the `go` tool to build the
  staged plugin, the compiled `TestSpike*` fixture, the cached `./cmd/hop`
  build (`buildHopBinary`) and the compiled fixture worker, `git` (fixture
  repository construction and the fixture worker's own commits — isolated
  per repository from any developer git configuration, never the invariant
  the suite is proving), `ps` (`listGroupMembers`'s independent process-group
  listing), `/bin/sh` (the vanished-pane probe's gated pane command and the
  vanishing `claude` stub's interpreter), `sqlite3` (`querySQLite`'s read-only inspection of a test's own
  throwaway `hop.db`, shelled out to the same way this suite already uses
  `git`/`ps`), and — for S4 and the live scenario only — `claude` (both
  opt-in behind `HOP_LIVE_HARNESS=1` and skipped, not failed, when that is
  unset; S4 also skips, with its own reason, if `claude` is missing even
  when opted in, while the live scenario treats a missing `claude` once
  opted in as a hard failure, never a skip — see "Live scenario" above).
  The `TestSpike*` cases
  also skip a shell capability case (zsh) with a reason when that shell is
  not installed.

## Verification

- `go test -count=1 -run TestRealProcess -v ./test/integration` — the Phase 1
  suite, verbosely, showing pass or the explicit skip.
- `go test -count=1 -run TestSpike ./test/integration` — the Phase 2 capability
  spike (S1, S2, S3, S5, S6, S7 and the harness self-tests) plus the Phase 3
  pre-freeze spike (S8, S9, S10 — `TestSpikeWorkspaceCreateResponseShapeAndEnv`,
  `TestSpikeWorkspaceCreateNoFocus`, `TestSpikeConcurrentWorktreeCreate`,
  `TestSpikeWorktreeCreateExistingBranchIgnoresBase`,
  `TestSpikeMultiPaneEnvIsolation`; plus the provisional G1/G2/G3 git-level
  and log-observability probes: `TestSpikeIntegrationRefCompareAndSwap`,
  `TestSpikeDetachedMergeLeavesRefUntouched`,
  `TestSpikeRunScopedRefFamilyCoexists`,
  `TestSpikeBareRunBranchCollidesWithAttemptBranches`,
  `TestSpikeUpdateRefCreateOnlySemantics`,
  `TestSpikeIntegrationRollbackPreservesRejectedMergeReachable`,
  `TestSpikeMergeTwoParentCommit`, `TestSpikeMergeConflictExitCodeAndIndexState`,
  `TestSpikeMergeAlreadyUpToDateNoOp`,
  `TestSpikeMergeNoFFForcesCommitOnFastForwardableHead`,
  `TestSpikeMergeRepoLocalHooksFire`,
  `TestSpikeMergeHooksPathSuppressesRepoLocalHooks`,
  `TestSpikeMergeRepoLocalSigningConfigDoesNotBlock`,
  `TestSpikeMergeNonConflictFailureIsDistinctFromConflict`,
  `TestSpikeCommitTreeIsDeterministic`,
  `TestSpikeAPIRequestLogObservability`; plus slice 8's removal-transport
  probe: `TestSpikeWorktreeRemoveHerdrShapes`,
  `TestSpikeWorktreeRemoveNeedsOpenWorkspace`,
  `TestSpikeWorkspaceIDReissuedAfterRestart`,
  `TestSpikeGitWorktreeRemoveLeavesHerdrWorkspace`; plus the vanished-pane
  probes behind the launch-ended rows and their server-continuity
  conjunct: `TestSpikeVanishedPaneShapes`, `TestSpikeLabelSurvivesRestart`,
  `TestSpikeRenamedLabelSurvivesRestart`). S4 is skipped
  here; run it opt-in with `HOP_LIVE_HARNESS=1 go test -count=1 -run
  TestSpikeClaudePreassignedSessionID ./test/integration`, which invokes the
  real `claude` in print mode against a scratch profile.
- `go test ./test/integration` — also runs the harness self-tests that need
  no herdr binary: `TestSafeToSignalGroup`,
  `TestKillProcessGroupThenReapKillsDescendants`,
  `TestArtifactDirRemovedOnPassingRun`, `TestArtifactRetentionDecision`,
  `TestSpikeRestartWaitsForGracefulExit`,
  `TestSpikeOwnedGroupTeardownKillsPipeHoldingChild`, `TestListGroupMembers`,
  and task 6b's harness/fixture self-tests below.
- `go test -count=1 -run 'TestBuildAndRunHopBinary|TestHopEnviron|TestStartHopController|TestFixtureRepo|TestFixtureWorker' -v ./test/integration` —
  task 6b's Phase A self-tests: the cached `cmd/hop` build and one-shot
  runner against the Phase 1 commands (`version`), `requireHopCommand`'s skip
  detection against a command name that will never exist, `hopEnviron`'s
  isolation contract asserted directly with no process execution, the
  anchored-leader mechanics via a short-lived command, the fixture
  repository's deterministic/git-dependent/submodule checks proven at the
  git level with no herdr or hop binary, and the fixture worker driven
  directly (no herdr) against a fake `hop` stub proving argv/env parsing,
  transient-retry and the FIXTURE-BEHAVIOR dispatch.
- `go test -count=1 -run TestRealProcess -v ./test/integration` also covers
  every task 6b Phase B scenario (their names all share the
  `TestRealProcess` prefix): `TestRealProcessRunEndToEnd`,
  `TestRealProcessSanitizedLaunchExec`,
  `TestRealProcessDuplicateSubmissionAfterCompletion`,
  `TestRealProcessDetachDistinctFromStop`,
  `TestRealProcessFailedCheckRetainsArtifacts`,
  `TestRealProcessGitDependentCheckSucceedsInDetachedCheckout`,
  `TestRealProcessSubmoduleRepositoryFailsClearly`,
  `TestRealProcessControllerKillResumeWarmReattach`,
  `TestRealProcessConfirmAbsentColdRelaunchNonRestart`,
  `TestRealProcessStaleSubmissionFromRetiredIncarnation`,
  `TestRealProcessConfirmAbsentRefusedAfterServerRestart`,
  `TestRealProcessStopWithTerminationObserved`,
  `TestRealProcessStopInterruptsRunningCheckGroup`,
  `TestRealProcessDuplicateLaunchInvocation`,
  `TestRealProcessExecFailureSettlesExecFailed`,
  `TestRealProcessForkingWrapperAfterExec`,
  `TestRealProcessSettlementWithMCPGroupMembers`,
  `TestRealProcessCheckLeaderExitWithLiveChildren`,
  `TestRealProcessCheckDeathUnknownOutcomeRepeatable`,
  `TestRealProcessLaunchSeedsWorkspaceTrust`,
  `TestRealProcessLaunchWithoutProfileConfigNotSeeded`,
  `TestRealProcessWorktreeCleanedUpAfterRun`. Several pay a real,
  unavoidable wall-clock cost (the lease TTL a crash-recovery scenario
  waits out, a herdr server restart, a real check's sleep) — this sweep
  takes several minutes, not seconds; run a narrower `-run` pattern while
  iterating on one scenario.
- `go test -count=1 -run 'TestFixtureManagerScriptDispatch|TestFixtureManagerVerdictRejectedCorrelation|TestFixtureReviewerRejectOnce|TestFixtureWorkerHoldBarrier' -v ./test/integration` —
  Phase 3 slice 7a's isolated (no herdr) fixture principal unit coverage:
  the manager-feature scripted dispatch, the STATUS-1 verdict-rejected
  correlation rule in isolation, reviewer-reject-once's one-shot marker
  across two invocations, and the worker-hold barrier, all driven directly
  against the fake hop stub (`fixtureprincipal_test.go`).
- `go test -count=1 -run 'TestRealProcessFeatureRunEndToEnd|TestRealProcessWorkerInterruption|TestRealProcessReviewerRejection' -v ./test/integration` —
  Phase 3 slice 7a's own real-process scenarios, all three landed (defect
  STATUS-1 is fixed on main); each pays the same class of real wall-clock
  cost as the task 6b sweep below (concurrent worker scheduling, serial
  integration, a real review round trip).
- `TestLiveClaudeDefaultProfileRun` is excluded from every command above by
  its own gate (`HOP_LIVE_HARNESS` unset): confirm it compiles and skips
  cleanly with `go test -count=1 -v -run '^TestLiveClaudeDefaultProfileRun$'
  ./test/integration`; run it for real only via `make test-live` or the
  command under "Live scenario" above.
- Test fixtures: none on disk as static files; every fixture (servers, roots,
  the staged plugin, the spike fixture binaries, the cached `cmd/hop` build,
  fixture repositories and the fixture worker) is generated or compiled per
  test run and removed by cleanup — retained only on failure, under the same
  artifact directory as every other evidence this suite produces. The suite
  never removes the shared `$TMPDIR/hop-integration` root by hand — only its
  own per-test directories.

## Related guides

- [Parent index](../AGENTS.md)
- [Herdr adapter](../../internal/adapters/herdr/AGENTS.md)
- [Plugin testing plan](../../docs/architecture/plugin-testing.md)
