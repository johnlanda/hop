package integration

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// runEndToEndTimeout bounds a full run -> worktree -> launch pane ->
// corroborated claim settlement -> assignment delivery -> result submit ->
// detached-checkout check -> completed round trip against a real,
// disposable herdr server. It is longer than the suite's standard
// conditionTimeout: this is several real subsystems in sequence (pane
// creation, exec, a real git commit, a spawned check process), not one
// bounded condition.
const runEndToEndTimeout = 150 * time.Second

// forbiddenCredentialVars is the union of every provider-credential
// variable the sanitizing launcher's strip matrix removes
// (docs/plan/phase-2-design.md section 6); this suite asserts every one of
// them is absent from every spawned test process's observed environment,
// matching the hard constraint that no test ever sets or exposes a real
// credential.
var forbiddenCredentialVars = []string{ //nolint:gochecknoglobals // an immutable fixed table shared by every exec-boundary assertion in this file; not configuration, not mutated after init.
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
	"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR",
	"OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_HOME",
}

// requiredWorkerEnvVars are the pane-provided HOP_* variables
// validateLaunchEnvironment requires present and agreeing
// (internal/app/usecase_execboundary.go).
var requiredWorkerEnvVars = []string{ //nolint:gochecknoglobals // an immutable fixed table shared by every exec-boundary assertion in this file.
	"HOP_STATE_DIR", "HOP_RUN_ID", "HOP_TASK_ID", "HOP_ATTEMPT_ID", "HOP_INCARNATION_ID",
}

// assertExecutableIsFixtureStub asserts that the worker observed its own
// resolved executable image as exactly the fixture-installed claude stub
// under this test's own server root — proving hop launch's PATH-based
// harness resolution (section 6) never reaches a real claude binary
// elsewhere on the machine, regardless of what else might be on PATH.
func assertExecutableIsFixtureStub(t *testing.T, server *testServer, observed *workerObservation) {
	t.Helper()
	want := filepath.Join(server.base, "bin", "claude")
	got := observed.Fields["executable"]
	if got != want {
		t.Errorf("worker's own resolved executable = %q, want the installed fixture stub %q; a real claude on this machine must never be exec'd", got, want)
	}
}

// TestRealProcessRunEndToEnd is the CI-safe deterministic run of section 9's
// test plan: a fixture repository and fixture worker driven through the
// real hop binary and a real, disposable herdr server, end to end — run,
// worktree creation, the launch pane (env, label, argv implied by
// corroborated settlement), the claim settling, the sanitized exec, the
// fixture validating its delivered assignment content, a real git commit
// and hop result submit, the detached-checkout check running check.sh, and
// the run reaching completed with hop run itself exiting 0.
func TestRealProcessRunEndToEnd(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	worker := buildFixtureWorker(t, artifacts)
	installFixtureWorkerAsClaudeStub(t, server, worker)
	server.start(t)

	repo := newFixtureRepo(t, artifacts, "repo")
	stateDir := artifacts.dir(t, "state")
	env := server.hopEnviron(stateDir)

	brief := fixtureWorkerBrief("submit-valid")
	sp := server.startHopController(t, stateDir, "run", "run", "-C", repo.Root, brief)

	started := waitForControllerLog(t, artifacts, "run", "stdout", "started", 30*time.Second)
	label, runID := extractRunID(t, started)

	fields, reached := waitForRunState(t, env, repo.Root, runID, runEndToEndTimeout, "completed", "failed", "stopped")
	stdout := readControllerLog(t, artifacts, "run", "stdout")
	if !reached || fields["state"] != "completed" {
		// The worker pane's own scrollback is never otherwise retained — it
		// is ephemeral, gone with the pane — so capture it before failing.
		if binding := fields["binding"]; binding != "" {
			artifacts.save(t, "worker-pane-scrollback.txt", server.readPane(t, paneIDFromBinding(binding)))
		}
		t.Fatalf("run %s ended %q, want completed; hop status detail: %+v\ncontroller stdout:\n%s", runID, fields["state"], fields, stdout)
	}

	status := waitForControllerExit(t, sp, 30*time.Second)
	if !status.Exited() || status.ExitCode() != 0 {
		t.Errorf("hop run exit status = %v, want a clean exit(0)", status)
	}

	for _, want := range []string{
		"run " + label + " " + runID + " started",
		"run " + label + " running",
		"run " + label + " completing",
		"run " + label + " completed",
		"launch settled",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("controller stdout missing %q; got:\n%s", want, stdout)
		}
	}

	// Pin what the S2 spike never did: the launched pane's foreground
	// process, observed independently through pane.process_info (not
	// through the worker's own self-report), carries the resolved fixture
	// stub path verbatim in argv[0] and — on macOS — the basename in argv0
	// (Herdr's process_argv0_name; Linux never reports argv0 at all).
	wantExecutable := filepath.Join(server.base, "bin", "claude")
	info := server.processInfo(t, paneIDFromBinding(fields["binding"]))
	if len(info.ForegroundProcesses) == 0 {
		t.Fatalf("pane %s reports no foreground process after the run completed", fields["binding"])
	}
	fg := info.ForegroundProcesses[0]
	if len(fg.Argv) == 0 || fg.Argv[0] != wantExecutable {
		t.Errorf("launched pane foreground argv[0] = %q, want %q verbatim", fg.Argv, wantExecutable)
	}
	if runtime.GOOS == "darwin" && fg.Argv0 != "claude" {
		t.Errorf("launched pane foreground argv0 = %q, want the basename %q", fg.Argv0, "claude")
	}

	// The fixture worker's own observation dump is the section 6 delivery
	// and content-validation evidence: it read the assignment by the exact
	// mechanism the design specifies (argv reference, cross-checked against
	// the independently computed path) and saw the delivered brief.
	observedPath := filepath.Join(stateDir, "runs", runID, "artifacts", "worker-observed.txt")
	observed := readWorkerObservation(t, observedPath)
	assertExecutableIsFixtureStub(t, server, &observed)
	for _, name := range requiredWorkerEnvVars {
		if !observed.HasEnvName(name) {
			t.Errorf("worker environment missing %s", name)
		}
	}
	for _, credVar := range forbiddenCredentialVars {
		if observed.HasEnvName(credVar) {
			t.Errorf("worker environment carries the provider-credential variable %s", credVar)
		}
	}
	if observed.Fields["hop_path"] == "" {
		t.Error("worker did not observe a hop path")
	}
	if !strings.Contains(observed.AssignmentContent, brief) {
		t.Errorf("delivered assignment content does not carry the brief %q; content:\n%s", brief, observed.AssignmentContent)
	}

	if fields["last submit"] == "" || fields["last submit"] == "(none)" {
		t.Error("hop status does not report a last submission")
	}
	if fields["last check"] == "" || fields["last check"] == "(none)" {
		t.Error("hop status does not report a last check execution")
	}
}

// TestRealProcessSanitizedLaunchExec proves the section 6 sanitized-exec
// contract directly: every strip-matrix variable is absent from the
// worker's observed environment even when deliberately seeded (a known,
// fake, non-functional value — never a real credential) into the herdr
// server's own environment, which the pane inherits additively.
func TestRealProcessSanitizedLaunchExec(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	worker := buildFixtureWorker(t, artifacts)
	installFixtureWorkerAsClaudeStub(t, server, worker)
	for _, name := range forbiddenCredentialVars {
		// A plainly fake value, never shaped like a real provider token (no
		// sk-ant-/sk-/ghp_ style prefix): main is pushed to a public
		// repository, and a realistic-looking shape would trip secret
		// scanners even though it is not a real credential.
		server.extraEnv = append(server.extraEnv, name+"=hop-test-not-a-secret-"+strings.ToLower(name))
	}
	server.start(t)

	repo := newFixtureRepo(t, artifacts, "repo")
	stateDir := artifacts.dir(t, "state")

	brief := fixtureWorkerBrief("exit-without-submitting")
	server.startHopController(t, stateDir, "run", "run", "-C", repo.Root, brief)

	started := waitForControllerLog(t, artifacts, "run", "stdout", "started", 30*time.Second)
	_, runID := extractRunID(t, started)

	observedPath := filepath.Join(stateDir, "runs", runID, "artifacts", "worker-observed.txt")
	if !waitUntilDeadline(runEndToEndTimeout, func() bool {
		_, statErr := os.Stat(observedPath)
		return statErr == nil
	}) {
		t.Fatalf("worker observation dump %s never appeared; controller stdout:\n%s", observedPath, readControllerLog(t, artifacts, "run", "stdout"))
	}
	observed := readWorkerObservation(t, observedPath)
	assertExecutableIsFixtureStub(t, server, &observed)

	for _, credVar := range forbiddenCredentialVars {
		if observed.HasEnvName(credVar) {
			t.Errorf("worker environment carries the seeded provider-credential variable %s; the strip matrix did not remove it", credVar)
		}
	}
	for _, name := range requiredWorkerEnvVars {
		if !observed.HasEnvName(name) {
			t.Errorf("worker environment missing %s", name)
		}
	}
}
