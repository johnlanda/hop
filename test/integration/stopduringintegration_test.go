package integration

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stopMidIntegrationCheckScriptName names the check command
// TestRealProcessStopDuringFeatureRun wires: unlike stop_test.go's
// slowCheckScriptSource (a fixed sleep shared by every execution),
// section 8 runs the per-task and combined checks through the identical
// [check] command and committed script, so distinguishing "the first,
// per-task execution" from "the second, combined execution" needs a
// stateful marker outside git rather than a sleep.
const stopMidIntegrationCheckScriptName = "check-hold-integration.sh"

// stopMidIntegrationCheckScriptSource: the FIRST execution finds no
// markerPath, creates it and exits 0 at once (the per-task check passes
// immediately — nothing in this scenario needs it slow). Every later
// execution (integration's own combined check, the only other check this
// scenario's single task ever reaches) finds the marker and blocks
// reading gatePath — a real file barrier the test controls the release
// of, never a sleep.
func stopMidIntegrationCheckScriptSource(markerPath, gatePath string) string {
	return "#!/bin/sh\nset -eu\n" +
		"if [ -f \"" + markerPath + "\" ]; then\n" +
		"  cat \"" + gatePath + "\" >/dev/null\n" +
		"else\n" +
		"  : > \"" + markerPath + "\"\n" +
		"fi\n" +
		"exit 0\n"
}

// withStopMidIntegrationCheck rewires repo's [check] command to
// stopMidIntegrationCheckScriptSource and commits the change. markerPath
// and gatePath live under the test's own artifact directory, never inside
// the repository or HOP_STATE_DIR, exactly as checkdeath_test.go's
// checkGateScriptSource/withGatedCheck keep their own gate paths external.
func withStopMidIntegrationCheck(t *testing.T, repo *fixtureRepo, opts featureFixtureOptions, markerPath, gatePath string) { //nolint:gocritic // hugeParam: featureFixtureOptions is a one-shot scenario-build options struct, mirroring newFeatureFixtureRepo's own signature.
	t.Helper()
	if err := syscall.Mkfifo(gatePath, 0o600); err != nil {
		t.Fatalf("create stop-mid-integration check gate fifo %s: %v", gatePath, err)
	}
	repo.writeFile(t, stopMidIntegrationCheckScriptName, stopMidIntegrationCheckScriptSource(markerPath, gatePath), 0o755)
	repo.writeFile(t, configRelPath, featureConfigTOML([]string{"sh", stopMidIntegrationCheckScriptName}, opts.MaxWorkers, opts.RetryLimit, opts.MessageWaitTimeout, opts.MessageAttentionAfter), 0o644)
	repo.commit(t, "wire a check that holds only on its second (combined) execution")
}

// jsonStringField decodes one top-level string field from a JSON object
// text — used here to read integration.reset's own act_evidence
// (rollback_oid) out of the operations table without a dedicated column.
func jsonStringField(t *testing.T, jsonText, field string) string {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(jsonText), &decoded); err != nil {
		t.Fatalf("parse JSON %q: %v", jsonText, err)
	}
	v, _ := decoded[field].(string)
	return v
}

// TestRealProcessStopDuringFeatureRun is the Layers row's own stop-mid-
// integration scenario: `hop stop` arrives while a single task's
// candidate has already been merged and published to the integration
// branch and its combined check is genuinely in flight (a real
// check_exec_claims row, not merely a requested one). Asserts: the task
// (in "integrating") moves to "interrupted"; the integration itself moves
// to "rolled-back" through the SAME reset construction a failing combined
// check uses (design section 4/8) — a fresh rollback commit whose OID is
// persisted as the operation's own act evidence before the ref CAS, the
// integration branch reset to exactly that commit, and the rejected
// (unvalidated) merge candidate kept reachable as the rollback's own
// parent, never orphaned; every session (manager and the already-retired
// implementer alike) ends terminated; and the run reaches "stopped" only
// once termination is observed, never merely requested.
func TestRealProcessStopDuringFeatureRun(t *testing.T) {
	t.Skip("STOP-2: the combined integration check runs synchronously in the feature loop, so stop cannot interrupt it; unskip when STOP-2 lands")
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
	markerPath := filepath.Join(artifacts.path, "check-hold-marker")
	gatePath := filepath.Join(artifacts.path, "check-hold.fifo")
	withStopMidIntegrationCheck(t, repo, opts, markerPath, gatePath)

	brief := fixtureManagerBrief(scratchDir,
		[]fixtureManagerTask{{Label: "t1", Title: "Implement t1", Behavior: "worker-implement"}},
		nil, "")
	fx := startFeatureRun(t, artifacts, server, repo, scratchDir, brief)

	t1 := fx.requireTaskBySeq(t, 1)
	fx.requireTaskState(t, t1, "integrating")

	// The merge already succeeded and its candidate is already published
	// (EnterChecking commits in the same transaction as the publish, per
	// usecase_integration.go): capture the rejected candidate's own OID
	// now, before the reset below replaces it on the ref.
	if !waitUntil(func() bool {
		return fx.scalar(t, fmt.Sprintf("SELECT state FROM integrations WHERE run_id = '%s' AND task_id = '%s';", fx.runID, t1)) == "checking"
	}) {
		t.Fatalf("integration for task %s never reached \"checking\"", t1)
	}
	rejectedMergeOID := fx.scalar(t, fmt.Sprintf("SELECT merge_commit_oid FROM integrations WHERE run_id = '%s' AND task_id = '%s';", fx.runID, t1))
	if rejectedMergeOID == "" {
		t.Fatalf("integration for task %s has no recorded merge_commit_oid while checking", t1)
	}

	// The combined check's own execution is genuinely in flight (not
	// merely requested): its check_exec_claims row exists, the second for
	// this run (the first being the per-task check, already settled and
	// passed) — mirroring stop_test.go's TestRealProcessStopInterrupts
	// RunningCheckGroup's own "wait for the claim before stopping"
	// technique, generalized to the combined subject.
	if !waitUntil(func() bool {
		return fx.scalar(t, fmt.Sprintf(
			"SELECT count(*) FROM check_exec_claims c JOIN operations o ON o.id = c.operation_id WHERE o.run_id = '%s' AND o.kind = 'check.run';",
			fx.runID)) == "2"
	}) {
		t.Fatalf("the combined check's own check_exec_claims row never appeared for run %s", fx.runID)
	}

	result := runHop(t, fx.env, fx.repo.Root, "stop", "-C", fx.repo.Root, fx.runID)
	if result.ExitCode != 0 {
		t.Fatalf("hop stop exit=%d, want 0; stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "stopped") {
		t.Errorf("hop stop stdout = %q, want it to report stopped", result.Stdout)
	}

	status := waitForControllerExit(t, fx.controller, 30*time.Second)
	if !status.Exited() || status.ExitCode() != 1 {
		t.Errorf("hop run exit status after being stopped = %v, want exit(1)", status)
	}

	fx.requireRunState(t, "stopped")
	if got := fx.taskState(t, t1); got != "interrupted" {
		t.Errorf("task %s state after stop mid-integration = %q, want \"interrupted\"", t1, got)
	}
	integrationState := fx.scalar(t, fmt.Sprintf("SELECT state FROM integrations WHERE run_id = '%s' AND task_id = '%s';", fx.runID, t1))
	if integrationState != "rolled-back" {
		t.Errorf("integration state after stop mid-integration = %q, want \"rolled-back\"", integrationState)
	}

	// The reset construction, verified both by object id (the ref itself)
	// and by the persisted rollback-commit OID (act evidence written
	// before the CAS, per design section 4): the ref now points AT that
	// exact commit, whose own parent is the rejected merge candidate,
	// which stays reachable rather than being orphaned by a destructive
	// reset.
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
	// fixtureRepo.git fails the test itself on a non-zero exit, so a
	// successful return here IS the ancestry assertion.
	fx.repo.git(t, "merge-base", "--is-ancestor", rejectedMergeOID, integrationRef)

	// Every session terminated: the manager, and the implementer (already
	// retired at its own per-attempt acceptance boundary, well before
	// integration began — restated here as the scenario's own "every
	// session" requirement, not merely inferred from an earlier scenario).
	managerSessionID := fx.scalar(t, fmt.Sprintf("SELECT id FROM sessions WHERE run_id = '%s' AND role = 'manager' ORDER BY rowid DESC LIMIT 1;", fx.runID))
	if managerSessionID == "" {
		t.Fatalf("no manager session found for run %s", fx.runID)
	}
	fx.requireSessionState(t, managerSessionID, "terminated")
	attemptID, _ := fx.currentAttempt(t, t1)
	implementerSessionID := fx.sessionForAttempt(t, attemptID)
	fx.requireSessionState(t, implementerSessionID, "terminated")
}
