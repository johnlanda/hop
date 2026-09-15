package integration

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureRun is the shared setup for scenarios that drive one real hop run
// controller against a fixture repository and fixture worker: build, wire
// and start everything through the "started" line, leaving every later
// action (wait for a state, stop, kill, resume, a direct hop result submit)
// to the caller, since scenarios sharing this setup diverge immediately
// afterward.
type fixtureRun struct {
	artifacts  *artifactDir
	server     *testServer
	repo       *fixtureRepo
	stateDir   string
	env        []string
	controller *serverProcess
	label      string
	runID      string
}

// startFixtureRun builds a fixture repository and fixture worker, installs
// the worker as the claude PATH stub, starts a disposable herdr server and
// runs `hop run` against the repository with brief embedding the given
// FIXTURE-BEHAVIOR directive (fixtureWorkerBrief), returning once the
// controller has printed its "started" line.
func startFixtureRun(t *testing.T, behavior string) *fixtureRun {
	t.Helper()
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	worker := buildFixtureWorker(t, artifacts)
	installFixtureWorkerAsClaudeStub(t, server, worker)
	server.start(t)

	repo := newFixtureRepo(t, artifacts, "repo")
	stateDir := artifacts.dir(t, "state")
	env := server.hopEnviron(stateDir)

	brief := fixtureWorkerBrief(behavior)
	sp := server.startHopController(t, stateDir, "run", "run", "-C", repo.Root, brief)
	started := waitForControllerLog(t, artifacts, "run", "stdout", "started", 30*time.Second)
	label, runID := extractRunID(t, started)

	return &fixtureRun{
		artifacts:  artifacts,
		server:     server,
		repo:       repo,
		stateDir:   stateDir,
		env:        env,
		controller: sp,
		label:      label,
		runID:      runID,
	}
}

// dbPath is this run's own throwaway hop.db, valid once the state root has
// been created (at the latest, once the controller's "started" line has
// printed).
func (f *fixtureRun) dbPath() string {
	return filepath.Join(f.stateDir, "hop.db")
}

// taskAndAttemptIDs queries this run's single task and attempt id, valid
// once StartRun has journaled the initial launch intent (by the time the
// controller's "started" line has printed).
func (f *fixtureRun) taskAndAttemptIDs(t *testing.T) (taskID, attemptID string) {
	t.Helper()
	taskID = querySQLite(t, f.dbPath(), fmt.Sprintf("SELECT id FROM tasks WHERE run_id = '%s';", f.runID))
	attemptID = querySQLite(t, f.dbPath(), fmt.Sprintf("SELECT id FROM attempts WHERE task_id = '%s';", taskID))
	return taskID, attemptID
}

// currentIncarnationID queries the current (non-terminated) session's own
// incarnation id for attemptID, valid once a launch claim has been recorded
// for it — the same value a real worker would see in its own
// HOP_INCARNATION_ID. hop result submit requires a syntactically valid
// incarnation id even when the outcome under test (duplicate) does not
// depend on its value, since parsing is validated before any state or
// incarnation precondition (design section 7 step 1).
func (f *fixtureRun) currentIncarnationID(t *testing.T, attemptID string) string {
	t.Helper()
	return querySQLite(t, f.dbPath(), fmt.Sprintf(
		"SELECT incarnation_id FROM runtime_bindings WHERE session_id = (SELECT id FROM sessions WHERE attempt_id = '%s' AND state NOT IN ('lost', 'terminated')) ORDER BY observed_at DESC LIMIT 1;",
		attemptID))
}

// requireRunState drives waitForRunState to deadline and fails the test on
// anything but one of want, capturing the worker pane's scrollback first —
// it is ephemeral, gone with the pane — exactly as TestRealProcessRunEndToEnd
// does.
func (f *fixtureRun) requireRunState(t *testing.T, deadline time.Duration, want ...string) map[string]string {
	t.Helper()
	fields, reached := waitForRunState(t, f.env, f.repo.Root, f.runID, deadline, want...)
	if !reached || !containsState(want, fields["state"]) {
		if binding := fields["binding"]; binding != "" {
			f.artifacts.save(t, "worker-pane-scrollback.txt", f.server.readPane(t, paneIDFromBinding(binding)))
		}
		t.Fatalf("run %s ended %q, want one of %v; hop status detail: %+v\ncontroller stdout:\n%s", f.runID, fields["state"], want, fields, readControllerLog(t, f.artifacts, "run", "stdout"))
	}
	return fields
}

func containsState(want []string, state string) bool {
	for _, w := range want {
		if w == state {
			return true
		}
	}
	return false
}

// TestRealProcessDuplicateSubmissionAfterCompletion proves section 7 step 3's
// idempotent-duplicate contract directly: once a run has completed, a fresh
// `hop result submit` invocation carrying the same commit and summary as the
// already-accepted result — the digest the application computes is
// therefore identical — is accepted as a `duplicate` receipt naming the
// existing result id, exits 0, and disturbs neither the run's terminal state
// nor the accepted result.
func TestRealProcessDuplicateSubmissionAfterCompletion(t *testing.T) {
	fx := startFixtureRun(t, "submit-valid")
	fx.requireRunState(t, runEndToEndTimeout, "completed")
	waitForControllerExit(t, fx.controller, 30*time.Second)

	taskID, attemptID := fx.taskAndAttemptIDs(t)
	row := querySQLite(t, fx.dbPath(), "SELECT id, commit_oid, summary FROM results WHERE accepted = 1;")
	parts := strings.SplitN(row, "|", 3)
	if len(parts) != 3 {
		t.Fatalf("accepted result row = %q, want \"id|commit_oid|summary\"", row)
	}
	resultID, commitOID, summary := parts[0], parts[1], parts[2]
	incarnationID := fx.currentIncarnationID(t, attemptID)

	env := append(append([]string{}, fx.env...), "HOP_INCARNATION_ID="+incarnationID)
	result := runHop(t, env, fx.stateDir, "result", "submit",
		"--summary", summary, "--commit", commitOID,
		"--run", fx.runID, "--task", taskID, "--attempt", attemptID)
	if result.ExitCode != 0 {
		t.Fatalf("hop result submit (duplicate) exit=%d, want 0; stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
	if want := "duplicate " + resultID; result.FirstStdoutLine() != want {
		t.Errorf("hop result submit (duplicate) first line = %q, want %q", result.FirstStdoutLine(), want)
	}

	receipts := querySQLite(t, fx.dbPath(), fmt.Sprintf(
		"SELECT count(*) FROM result_submissions WHERE claimed_attempt_id = '%s' AND outcome = 'duplicate' AND result_id = '%s';",
		attemptID, resultID))
	if receipts != "1" {
		t.Errorf("result_submissions duplicate-receipt count for attempt %s = %s, want 1", attemptID, receipts)
	}

	acceptedCount := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT count(*) FROM results WHERE attempt_id = '%s' AND accepted = 1;", attemptID))
	if acceptedCount != "1" {
		t.Errorf("accepted result count for attempt %s after the duplicate submission = %s, want 1 (unchanged)", attemptID, acceptedCount)
	}

	state := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM runs WHERE id = '%s';", fx.runID))
	if state != "completed" {
		t.Errorf("run state after the duplicate submission = %q, want still completed", state)
	}
}
