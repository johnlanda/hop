package integration

import (
	"fmt"
	"path/filepath"
	"testing"
)

// combinedCheckFailureCheckScriptName names the check command
// TestRealProcessCombinedCheckFailure wires: like
// stopduringintegration_test.go's stopMidIntegrationCheckScriptName, the
// per-task and combined checks share one committed script (section 8), so
// singling out exactly one execution — here, the FIRST combined-candidate
// check specifically, never a later retry's own per-task or combined
// check — needs a counter rather than a boolean marker.
const combinedCheckFailureCheckScriptName = "check-fail-second.sh"

// combinedCheckFailureCheckScriptSource fails on exactly its SECOND
// execution (counterPath holds a plain decimal count across executions,
// outside git, under the test's own artifact directory) and passes on
// every other one: the first execution is the sole task's per-task check
// (must pass so integration is ever claimed at all); the second is the
// first combined-candidate check (must fail, to exercise the reset); the
// third and fourth — the retry's own per-task and combined checks — must
// both pass so the reworked task, and the run, complete.
func combinedCheckFailureCheckScriptSource(counterPath string) string {
	return "#!/bin/sh\nset -eu\n" +
		"n=0\n" +
		"if [ -f \"" + counterPath + "\" ]; then n=$(cat \"" + counterPath + "\"); fi\n" +
		"n=$((n + 1))\n" +
		"echo \"$n\" > \"" + counterPath + "\"\n" +
		"if [ \"$n\" = \"2\" ]; then exit 1; fi\n" +
		"exit 0\n"
}

// withCombinedCheckFailureCheck rewires repo's [check] command to
// combinedCheckFailureCheckScriptSource and commits the change.
func withCombinedCheckFailureCheck(t *testing.T, repo *fixtureRepo, opts featureFixtureOptions, counterPath string) { //nolint:gocritic // hugeParam: featureFixtureOptions mirrors newFeatureFixtureRepo's own signature.
	t.Helper()
	repo.writeFile(t, combinedCheckFailureCheckScriptName, combinedCheckFailureCheckScriptSource(counterPath), 0o755)
	repo.writeFile(t, configRelPath, featureConfigTOML([]string{"sh", combinedCheckFailureCheckScriptName}, opts.MaxWorkers, opts.RetryLimit, opts.MessageWaitTimeout, opts.MessageAttentionAfter), 0o644)
	repo.commit(t, "wire a check that fails only on its second execution")
}

// TestRealProcessCombinedCheckFailure is the Layers row's own combined-
// check-failure scenario: a single task's per-task check passes, but the
// combined check against the published merge candidate fails outright (a
// definite non-zero exit, never an ambiguous/unknown outcome). Asserts
// the branch reset — the SAME construction StopDuringFeatureRun's stop
// path uses, here driven by a failing check instead — verified both by
// the integration ref's object id and by the integration.reset
// operation's own persisted rollback-commit OID (the rejected merge kept
// reachable as that commit's parent); the task reworked (a fresh, freshly
// based attempt); and the run completing once the reworked attempt's own
// checks both pass.
func TestRealProcessCombinedCheckFailure(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	worker := buildFixtureWorker(t, artifacts)
	installFixtureWorkerAsClaudeStub(t, server, worker)
	server.start(t)

	scratchDir := artifacts.dir(t, "fixture-scratch")
	opts := featureFixtureOptions{
		ScratchDir: scratchDir, ReviewerBehavior: "reviewer-approve",
		MaxWorkers: 1, RetryLimit: 3, MessageWaitTimeout: "3s", MessageAttentionAfter: "30s",
	}
	repo := newFeatureFixtureRepo(t, artifacts, server, "repo", opts)
	counterPath := filepath.Join(artifacts.path, "check-fail-second-counter")
	withCombinedCheckFailureCheck(t, repo, opts, counterPath)

	brief := fixtureManagerBrief(scratchDir,
		[]fixtureManagerTask{{Label: "t1", Title: "Implement t1", Behavior: "worker-implement"}},
		nil, "")
	fx := startFeatureRun(t, artifacts, server, repo, scratchDir, brief)

	t1 := fx.requireTaskBySeq(t, 1)
	// worker-implement submits at once (no barrier), so t1 can already be
	// well past "active" by the first poll; wait for the attempt ROW
	// itself rather than racing a specific task state.
	if !waitUntil(func() bool { return fx.attemptCount(t, t1) > 0 }) {
		t.Fatalf("no attempt ever reserved for task %s", t1)
	}
	attempt1ID, attempt1Number := fx.currentAttempt(t, t1)
	if attempt1Number != 1 {
		t.Fatalf("first attempt number = %d, want 1", attempt1Number)
	}

	// The first (per-task) execution must pass, or nothing here reaches
	// integration at all — a precondition, not the behavior under test.
	fx.requireTaskState(t, t1, "integrating")
	rejectedMergeOID := ""
	if !waitUntil(func() bool {
		state, mergeOID, ok := fx.integrationForTask(t, t1)
		if !ok || mergeOID == "" || state != "checking" {
			return false
		}
		rejectedMergeOID = mergeOID
		return true
	}) {
		t.Fatalf("integration for task %s never reached \"checking\" with a recorded merge commit", t1)
	}

	// The durable transition into needs-rework, never a live poll: the
	// scripted manager retries automatically on the notice
	// (workerinterruption_test.go's own reasoning applies here too).
	fx.requireTransitionAt(t, "task", t1, "needs-rework")
	integrationState, _, ok := fx.integrationForTask(t, t1)
	if !ok || integrationState != "rolled-back" {
		t.Errorf("integration state after the combined check failed = %q (found=%v), want \"rolled-back\"", integrationState, ok)
	}

	// The reset construction, verified both by object id and by the
	// persisted rollback-commit OID.
	resetEvidence := fx.scalar(t, fmt.Sprintf(
		"SELECT act_evidence FROM operations WHERE run_id = '%s' AND kind = 'integration.reset' ORDER BY updated_at DESC LIMIT 1;", fx.runID))
	rollbackOID := jsonStringField(t, resetEvidence, "rollback_oid")
	if rollbackOID == "" {
		t.Fatalf("no rollback_oid recorded in integration.reset act evidence %q", resetEvidence)
	}
	integrationRef := "refs/heads/hop/" + fx.label + "/integration"
	if got := fx.repo.git(t, "rev-parse", "--verify", integrationRef); got != rollbackOID {
		t.Errorf("integration ref %s = %s, want it reset to the persisted rollback commit %s", integrationRef, got, rollbackOID)
	}
	if got := fx.repo.git(t, "rev-parse", "--verify", rollbackOID+"^"); got != rejectedMergeOID {
		t.Errorf("rollback commit %s's parent = %s, want the rejected merge commit %s (kept reachable, never orphaned)", rollbackOID, got, rejectedMergeOID)
	}
	// The rollback commit's own TREE, not merely its parent linkage, must
	// be the pre-merge content (design section 4): the rejected merge's
	// own tree still carries whatever the failed combined check rejected,
	// so a rollback built from THAT tree instead of the merge's first
	// parent's would restore nothing.
	if got, want := fx.repo.git(t, "rev-parse", rollbackOID+"^{tree}"), fx.repo.git(t, "rev-parse", rejectedMergeOID+"^1^{tree}"); got != want {
		t.Errorf("rollback commit %s's tree = %s, want the rejected merge's own pre-merge (first-parent) tree %s", rollbackOID, got, want)
	}
	// fixtureRepo.git fails the test itself on a non-zero exit, so a
	// successful return here IS the ancestry assertion.
	fx.repo.git(t, "merge-base", "--is-ancestor", rejectedMergeOID, integrationRef)

	// The task is reworked: a fresh attempt, freshly based off the current
	// (rolled-back) head — never the rejected candidate's own tree.
	fx.requireTaskState(t, t1, "active")
	attempt2ID, attempt2Number := fx.currentAttempt(t, t1)
	if attempt2Number != 2 {
		t.Fatalf("retried attempt number = %d, want 2", attempt2Number)
	}
	if attempt2ID == attempt1ID {
		t.Fatal("the retried attempt has the same id as the failed one")
	}
	_, _, worktree2Base := fx.requireWorktreeForAttempt(t, attempt2ID)
	if worktree2Base != rollbackOID {
		t.Errorf("retried attempt's worktree base = %s, want the current (rolled-back) integration head %s", worktree2Base, rollbackOID)
	}

	// The reworked attempt's own per-task and combined checks both pass
	// (executions 3 and 4 — the script's sole failure was execution 2 —
	// so the run completes.
	fx.requireTaskState(t, t1, "integrated")
	reviewTaskID := fx.requireReviewTask(t)
	fx.requireTaskState(t, reviewTaskID, "completed")
	if verdict, ok := fx.reviewVerdict(t, reviewTaskID); !ok || verdict != "approve" {
		t.Errorf("review verdict = %q (found=%v), want approve", verdict, ok)
	}
	fx.requireRunState(t, "completed")

	if fx.attemptCount(t, t1) != 2 {
		t.Errorf("task t1 has %d recorded attempts, want exactly 2", fx.attemptCount(t, t1))
	}
}
