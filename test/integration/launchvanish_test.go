package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// vanishingClaudeStub is a "claude" PATH stub that ends the moment it is
// exec'd: hop launch writes its exec_pending claim and execs it, and the
// pane closes with the process before any corroboration can settle the
// claim. Executed through its shebang, the process is /bin/sh reading this
// file, so its argv never carries the claim's executable as argv[0] and
// corroboration could not settle it even if an inspection landed in its
// lifetime: the scenario is deterministic, never a timing race.
const vanishingClaudeStub = "#!/bin/sh\nexit 0\n"

// launchEndedClaimReason is the controller's fixed exec_failed settlement
// reason for a launch whose pane and claimed process were observed gone.
const launchEndedClaimReason = "launch ended before corroboration: pane absent by id and label; claimed process gone"

// vanishFeatureConfigTOML extends fixtureConfigTOML with feature mode and
// the three role instruction files the feature policy requires, resolved
// against .herdr-orchestrator. Named distinctly from featureharness_test.
// go's own richer featureConfigTOML (MaxWorkers/RetryLimit/message
// timeouts, built independently on a different branch for the fuller
// scenario suite): this scenario needs none of those knobs, only the
// minimal feature-mode config a vanished manager launch can fail against.
func vanishFeatureConfigTOML(checkCommand []string) string {
	return fixtureConfigTOML(checkCommand) +
		"\n[workflow]\nmode = \"feature\"\n" +
		"\n[roles.manager]\ninstructions = \"roles/manager.md\"\n" +
		"\n[roles.implementer]\ninstructions = \"roles/implementer.md\"\n" +
		"\n[roles.reviewer]\ninstructions = \"roles/reviewer.md\"\n"
}

// newVanishFeatureFixtureRepo is newFixtureRepo configured for feature
// mode, named distinctly from featureharness_test.go's own richer
// newFeatureFixtureRepo (see vanishFeatureConfigTOML).
func newVanishFeatureFixtureRepo(t *testing.T, artifacts *artifactDir, server *testServer) *fixtureRepo {
	t.Helper()
	repo := initFixtureRepo(t, artifacts, "repo")
	repo.writeFile(t, "hello.go", trivialSourceFile, 0o644)
	repo.writeFile(t, checkScriptName, checkScriptSource, 0o755)
	repo.writeFile(t, checkResultFile, "pass\n", 0o644)
	repo.writeFile(t, configRelPath, vanishFeatureConfigTOML([]string{"sh", checkScriptName}), 0o644)
	for _, role := range []string{"manager", "implementer", "reviewer"} {
		repo.writeFile(t, filepath.Join(".herdr-orchestrator", "roles", role+".md"), "# "+role+" role\n\nFixture role instructions.\n", 0o644)
	}
	repo.Base = repo.commit(t, "initial commit")
	registerWorktreeCleanup(t, server, repo)
	return repo
}

// TestRealProcessManagerLaunchVanishesBeforeCorroboration is the real-
// process proof of the launch-ended decision row for the manager
// (docs/plan/phase-3-design.md section 4): a feature run whose manager
// harness ends right after exec — its pane closing with it before any
// corroboration — is never left launching. The live controller keeps
// running until it observes the pane absent by id and label and the
// claimed process gone, settles the manager's claim exec_failed with the
// fixed reason, fails the run through the terminal-failure path, prints
// the launch-failed line and then exits with the failed run's exit code,
// never with a controller error.
func TestRealProcessManagerLaunchVanishesBeforeCorroboration(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	writeHarnessStub(t, filepath.Join(server.base, "bin", "claude"), []byte(vanishingClaudeStub))
	server.start(t)
	repo := newVanishFeatureFixtureRepo(t, artifacts, server)

	stateDir := artifacts.dir(t, "state")
	fx := &fixtureRun{artifacts: artifacts, server: server, repo: repo, stateDir: stateDir, env: server.hopEnviron(stateDir), controllerName: "run"}
	fx.controller = server.startHopController(t, stateDir, "run", "run", "-C", repo.Root, "--workflow", "feature", "vanishing manager brief")
	started := waitForControllerLog(t, artifacts, "run", "started")
	fx.label, fx.runID = extractRunID(t, started)

	fx.requireRunState(t, "failed")
	state := waitForControllerExit(t, fx.controller, 60*time.Second)
	if state == nil || state.ExitCode() != 1 {
		t.Errorf("hop run exit = %v, want 1 for a failed run", state)
	}

	stdout := readControllerLog(t, artifacts, "run")
	for _, want := range []string{"run " + fx.label + " launching", "launch failed", "run " + fx.label + " failed"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("controller stdout lacks %q:\n%s", want, stdout)
		}
	}
	stderr, err := os.ReadFile(filepath.Join(artifacts.path, "run-stderr.log"))
	if err != nil {
		t.Fatalf("read controller stderr: %v", err)
	}
	if strings.Contains(string(stderr), "hop run:") {
		t.Errorf("controller stderr reports a controller error; the loop must end on the failed run, not on an error:\n%s", stderr)
	}

	db := fx.dbPath()
	manager := querySQLite(t, db, fmt.Sprintf("SELECT id FROM sessions WHERE run_id = '%s' AND role = 'manager';", fx.runID))
	if manager == "" || strings.Contains(manager, "\n") {
		t.Fatalf("manager sessions = %q, want exactly one", manager)
	}
	claim := querySQLite(t, db, fmt.Sprintf("SELECT state || '|' || error || '|' || pid FROM launch_claims WHERE session_id = '%s';", manager))
	parts := strings.SplitN(claim, "|", 3)
	if len(parts) != 3 || parts[0] != "exec_failed" || parts[1] != launchEndedClaimReason {
		t.Fatalf("manager claim = %q, want exec_failed with the launch-ended reason", claim)
	}
	evidence := querySQLite(t, db, fmt.Sprintf("SELECT settlement_evidence FROM launch_claims WHERE session_id = '%s';", manager))
	pane := querySQLite(t, db, fmt.Sprintf("SELECT pane_id FROM runtime_bindings WHERE session_id = '%s';", manager))
	if !strings.Contains(evidence, `"pane_id":"`+pane+`"`) || !strings.Contains(evidence, `"pid":`+parts[2]) {
		t.Errorf("settlement evidence = %s, want the bound pane %s and the claimed pid %s", evidence, pane, parts[2])
	}
	if got := querySQLite(t, db, fmt.Sprintf("SELECT state FROM sessions WHERE id = '%s';", manager)); got != "terminated" {
		t.Errorf("manager session state = %q, want terminated", got)
	}
	reason := querySQLite(t, db, fmt.Sprintf("SELECT reason FROM transitions WHERE entity_kind = 'run' AND entity_id = '%s' AND to_state = 'failed';", fx.runID))
	if !strings.HasPrefix(reason, "manager launch exec failed") {
		t.Errorf("run failure reason = %q, want the manager launch failure cause", reason)
	}
	if n := querySQLite(t, db, fmt.Sprintf("SELECT count(*) FROM transitions WHERE entity_kind = 'run' AND entity_id = '%s' AND to_state = 'running';", fx.runID)); n != "0" {
		t.Errorf("run passed through running %s time(s), want never", n)
	}
}
