package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	// A failure path that ends the test before hop stop ever runs (or
	// before stop's own retirement kills the check's process group) would
	// otherwise leave "sh -c 'cat gatePath'" blocked on this FIFO forever,
	// in its own process group outside the controller's — nothing this
	// harness's cleanup would ever retire. Opening it O_RDWR never blocks
	// (unlike the O_WRONLY open a normal release needs a live reader
	// for), so this is always safe, whether or not the check ever reached
	// its blocking read: closing the fd delivers EOF to any blocked
	// reader, ending it.
	t.Cleanup(func() {
		gate, err := os.OpenFile(gatePath, os.O_RDWR, 0) //nolint:gosec // G304: gatePath is the FIFO this test created under its own artifact directory.
		if err != nil {
			return
		}
		if err := gate.Close(); err != nil {
			t.Logf("cleanup: close stop-mid-integration check gate: %v", err)
		}
	})
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
	v, ok := decoded[field].(string)
	if !ok {
		return ""
	}
	return v
}

// stopInterruptBound is the ceiling this scenario holds the interval
// between issuing `hop stop` and observing it complete to: well under the
// fixture's own configured `[check]` timeout (30s, featureConfigTOML), so
// a pass here is evidence the combined check's own process group was
// killed on stop's own cadence rather than merely outlived by its
// configured timeout — the two are otherwise indistinguishable by exit
// code alone, since either path eventually reports "stopped".
const stopInterruptBound = 15 * time.Second

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
// implementer alike) ends terminated; the run reaches "stopped" only once
// termination is observed, never merely requested; and stop's own
// interval — request to observed completion — stays under
// stopInterruptBound, proving the combined check was interrupted rather
// than outlived.
func TestRealProcessStopDuringFeatureRun(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	worker := buildFixtureWorker(t)
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

	// Captured before stop: the manager's pane binding (a durable
	// runtime_bindings row, safe to resolve while the session is still
	// live) and the combined check's own process-group pid (its own pid
	// IS its pgid — process/runner.go sets Setpgid on every claimed
	// group) — the "merge group retired" and "manager pane gone"
	// assertions below need evidence from while the run was still
	// healthy, not a name resolved after the fact.
	managerSessionID := fx.managerSessionID(t)
	managerPaneID := fx.requirePane(t, managerSessionID)
	checkPIDStr := fx.scalar(t, fmt.Sprintf(
		"SELECT c.pid FROM check_exec_claims c JOIN operations o ON o.id = c.operation_id WHERE o.run_id = '%s' AND o.kind = 'check.run' ORDER BY o.created_at DESC LIMIT 1;",
		fx.runID))
	checkPID, err := strconv.Atoi(checkPIDStr)
	if err != nil || checkPID <= 0 {
		t.Fatalf("combined check's own check_exec_claims pid = %q, want a positive integer", checkPIDStr)
	}

	// stopRequestedAt anchors the interval this scenario's whole point
	// rests on: `hop stop` itself polls until it observes "stopped" (or
	// its own deadline), so the CLI's own return is the observed
	// consequence, and the gap between the two wall-clock reads is the
	// stop-request-to-consequence interval this scenario bounds — never
	// the test's own total wall time, most of which is the run reaching
	// "checking" in the first place.
	stopRequestedAt := time.Now()
	result := runHop(t, fx.env, fx.repo.Root, "stop", "-C", fx.repo.Root, fx.runID)
	stopInterval := time.Since(stopRequestedAt)
	if result.ExitCode != 0 {
		t.Fatalf("hop stop exit=%d, want 0; stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "stopped") {
		t.Errorf("hop stop stdout = %q, want it to report stopped", result.Stdout)
	}
	t.Logf("hop stop request to observed completion: %s (bound %s)", stopInterval, stopInterruptBound)
	if stopInterval > stopInterruptBound {
		t.Errorf("hop stop took %s to observe \"stopped\", want under %s: the combined check's own configured 30s timeout, not stop's own interruption, is the more likely explanation", stopInterval, stopInterruptBound)
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
	// Positive control: the merge candidate's own tree must actually
	// differ from its pre-merge (first-parent) tree, or the rollback-tree
	// comparison below would hold vacuously even for a rollback built from
	// the WRONG (post-merge) tree.
	if mergeTree, premergeTree := fx.repo.git(t, "rev-parse", rejectedMergeOID+"^{tree}"), fx.repo.git(t, "rev-parse", rejectedMergeOID+"^1^{tree}"); mergeTree == premergeTree {
		t.Fatalf("rejected merge %s's tree equals its own pre-merge tree %s; the rollback-tree check below cannot distinguish rollback source", rejectedMergeOID, premergeTree)
	}
	// The rollback commit's own TREE, not merely its parent linkage, must
	// be the pre-merge content (design section 4): the rejected merge's
	// own tree still carries whatever stop caught mid-check, so a
	// rollback built from THAT tree instead of the merge's first
	// parent's would restore nothing.
	if got, want := fx.repo.git(t, "rev-parse", rollbackOID+"^{tree}"), fx.repo.git(t, "rev-parse", rejectedMergeOID+"^1^{tree}"); got != want {
		t.Errorf("rollback commit %s's tree = %s, want the rejected merge's own pre-merge (first-parent) tree %s", rollbackOID, got, want)
	}
	// fixtureRepo.git fails the test itself on a non-zero exit, so a
	// successful return here IS the ancestry assertion.
	fx.repo.git(t, "merge-base", "--is-ancestor", rejectedMergeOID, integrationRef)

	// Merge/check group retired: no member of the combined check's own
	// process group remains — evidence against the OS process table
	// directly (listGroupMembers is ps-based and sends no signal), since
	// process/runner.go's Setpgid makes the claimed pid double as the
	// group's pgid.
	if members := listGroupMembers(t, checkPID); len(members) != 0 {
		t.Errorf("combined check process group %d still has %d member(s) after stop, want 0: %+v", checkPID, len(members), members)
	}

	// Unresolved ref intents fenced: no integration ref-move operation
	// (init/merge/publish/reset) is left pending or reconciling once stop
	// reports (design section 4's fencing rule, section 6's quiescence
	// requirement before the terminal report).
	if n := fx.scalar(t, fmt.Sprintf(
		"SELECT count(*) FROM operations WHERE run_id = '%s' AND kind LIKE 'integration.%%' AND state IN ('pending', 'reconciling');",
		fx.runID)); n != "0" {
		t.Errorf("run %s has %s unresolved integration operation(s) after stop, want 0 (unresolved ref intents fenced)", fx.runID, n)
	}

	// The manager's own pane is gone: the session ended by the close
	// rule's own observed-absence outcome, never merely marked terminated
	// with the pane left dangling.
	if fx.server.paneExists(t, managerPaneID) {
		t.Errorf("manager pane %s still exists after stop", managerPaneID)
	}

	// Every session in the run terminated — not merely the two hand-
	// picked ones the scenario's own narrative names below.
	if n := fx.scalar(t, fmt.Sprintf("SELECT count(*) FROM sessions WHERE run_id = '%s' AND state != 'terminated';", fx.runID)); n != "0" {
		t.Errorf("run %s has %s non-terminated session(s) after stop, want 0", fx.runID, n)
	}

	// The manager, and the implementer (already retired at its own
	// per-attempt acceptance boundary, well before integration began).
	fx.requireSessionState(t, managerSessionID, "terminated")
	attemptID, _ := fx.currentAttempt(t, t1)
	implementerSessionID := fx.sessionForAttempt(t, attemptID)
	fx.requireSessionState(t, implementerSessionID, "terminated")

	// "stopped" only on observed absence: the run's own stopped
	// transition must never precede the evidence that termination
	// actually happened — every session's own terminated transition, and
	// the combined check operation's last update — read directly from the
	// journal's canonical, lexically comparable timestamps, never
	// inferred from reading end states only after the controller has
	// already exited.
	stoppedAt := fx.requireTransitionAt(t, "run", fx.runID, "stopped")
	managerTerminatedAt := fx.requireTransitionAt(t, "session", managerSessionID, "terminated")
	implementerTerminatedAt := fx.requireTransitionAt(t, "session", implementerSessionID, "terminated")
	checkUpdatedAt, checkState := fx.scalar(t, fmt.Sprintf(
		"SELECT updated_at FROM operations WHERE run_id = '%s' AND kind = 'check.run' ORDER BY created_at DESC LIMIT 1;", fx.runID)),
		fx.scalar(t, fmt.Sprintf(
			"SELECT state FROM operations WHERE run_id = '%s' AND kind = 'check.run' ORDER BY created_at DESC LIMIT 1;", fx.runID))
	if checkUpdatedAt == "" {
		t.Fatalf("no check.run operation found for run %s", fx.runID)
	}
	// The check operation must have actually SETTLED, or the "stopped
	// after the check's last update" comparison below would hold
	// vacuously against updated_at still at its creation time (an
	// operation that was created but never touched again).
	if checkState == "pending" || checkState == "reconciling" {
		t.Errorf("combined check operation state after stop = %q, want settled (not pending/reconciling), or the ordering check below proves nothing", checkState)
	}
	if stoppedAt < managerTerminatedAt {
		t.Errorf("run reported stopped at %s before the manager session terminated at %s", stoppedAt, managerTerminatedAt)
	}
	if stoppedAt < implementerTerminatedAt {
		t.Errorf("run reported stopped at %s before the implementer session terminated at %s", stoppedAt, implementerTerminatedAt)
	}
	if stoppedAt < checkUpdatedAt {
		t.Errorf("run reported stopped at %s before the combined check operation's last update at %s", stoppedAt, checkUpdatedAt)
	}
}
