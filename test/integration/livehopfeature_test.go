package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// liveFeatureHarnessTimeout bounds the live feature scenario's run-state
// polls: a manager plus one worker plus one reviewer, each a real Claude
// Code session, is a slower round trip than the solo live test's own
// single launch.
const liveFeatureHarnessTimeout = 20 * time.Minute

// requireLiveFeatureHarness gates TestLiveClaudeFeatureRun exactly like
// livehop_test.go's requireLiveHarness (HOP_LIVE_HARNESS=1 opts in; unset
// skips with a one-line reason; a missing claude binary once opted in is
// a FAILURE, never a skip) — but unlike the solo live test, which
// defaults to the operator's own real home directory (design section 9's
// "user's default profile" call), this scenario launches THREE real
// Claude sessions (manager, implementer, reviewer) and must never read or
// write the operator's own ~/.claude.json: HOP_LIVE_HARNESS_HOME is
// REQUIRED here, never merely an override, and must name an absolute,
// existing, already-authenticated profile directory the operator prepared
// specifically for this test.
func requireLiveFeatureHarness(t *testing.T) (claudePath, home string) {
	t.Helper()
	if os.Getenv("HOP_LIVE_HARNESS") != "1" {
		t.Skip("live-harness test skipped, not run: set HOP_LIVE_HARNESS=1 to opt in")
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		t.Fatalf("HOP_LIVE_HARNESS=1 but no claude binary is on PATH: %v", err)
	}
	override := os.Getenv("HOP_LIVE_HARNESS_HOME")
	if override == "" {
		t.Fatalf("HOP_LIVE_HARNESS_HOME is required for this scenario (never the operator's own real home, unlike TestLiveClaudeDefaultProfileRun): point it at a profile prepared and already authenticated specifically for this test")
	}
	if !filepath.IsAbs(override) {
		t.Fatalf("HOP_LIVE_HARNESS_HOME %q is not an absolute path", override)
	}
	info, statErr := os.Stat(override) //nolint:gosec // G703: the operator chose this path to point every live session's own HOME at a profile they prepared.
	if statErr != nil {
		t.Fatalf("HOP_LIVE_HARNESS_HOME %q: %v", override, statErr)
	}
	if !info.IsDir() {
		t.Fatalf("HOP_LIVE_HARNESS_HOME %q is not a directory", override)
	}
	// HOP_LIVE_HARNESS_HOME=$HOME is a natural carry-over from the solo
	// live test, where the variable is optional and defaults to the
	// operator's real home: it would pass every check above and then let
	// three real Claude sessions read and write the operator's actual
	// ~/.claude.json, exactly what this scenario must never do. Compared
	// after EvalSymlinks so a symlinked override can never disguise
	// itself as distinct; the failure message never echoes either path.
	resolvedOverride, err := filepath.EvalSymlinks(override)
	if err != nil {
		t.Fatalf("HOP_LIVE_HARNESS_HOME could not be resolved (the path is never echoed): %v", err)
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve the operator's own home directory: %v", err)
	}
	resolvedRealHome, err := filepath.EvalSymlinks(realHome)
	if err != nil {
		t.Fatalf("resolve the operator's own home directory (the path is never echoed): %v", err)
	}
	if resolvedOverride == resolvedRealHome {
		t.Fatal("HOP_LIVE_HARNESS_HOME must not resolve to the operator's own home directory (never echoed); point it at a profile prepared specifically for this test")
	}
	return path, override
}

// liveFeatureConfigTOML renders a minimal feature-mode config: the same
// deterministic check.sh/CHECK_RESULT contract fixturerepo_test.go's
// checkScriptSource already provides (an objective pass/fail signal the
// real reviewer's own approved commit must still satisfy), one worker
// slot, a small retry budget, short message timeouts, and the three role
// instruction files — real Claude reads these as genuine guidance, never
// a FIXTURE-BEHAVIOR directive the way the deterministic fixture suite's
// own feature repos (featureharness_test.go's newFeatureFixtureRepo) do.
func liveFeatureConfigTOML() string {
	return "[check]\ncommand = [\"sh\", \"" + checkScriptName + "\"]\ntimeout = \"120s\"\n" +
		"\n[worker]\nharness = \"claude\"\n" +
		"\n[workflow]\nmode = \"feature\"\n" +
		"\n[workers]\nmax = 1\n" +
		"\n[retry]\nmax_attempts = 2\n" +
		"\n[messages]\nwait_timeout = \"50s\"\nattention_after = \"5m\"\n" +
		"\n[roles.manager]\ninstructions = \"roles/manager.md\"\n" +
		"\n[roles.implementer]\ninstructions = \"roles/implementer.md\"\n" +
		"\n[roles.reviewer]\ninstructions = \"roles/reviewer.md\"\nharness = \"claude\"\n"
}

// newLiveFeatureFixtureRepo builds the fixture repository the live
// feature scenario runs against: hello.go plus the deterministic check
// script (so a real reviewer's own judgment is still backstopped by an
// objective pass/fail signal), and genuine, human-readable role
// instructions — plain guidance a real manager/implementer/reviewer
// reads and acts on, never a scripted directive.
func newLiveFeatureFixtureRepo(t *testing.T, artifacts *artifactDir, server *testServer) *fixtureRepo {
	t.Helper()
	repo := initFixtureRepo(t, artifacts, "repo")
	repo.writeFile(t, "hello.go", trivialSourceFile, 0o644)
	repo.writeFile(t, checkScriptName, checkScriptSource, 0o755)
	repo.writeFile(t, checkResultFile, "pass\n", 0o644)
	repo.writeFile(t, ".herdr-orchestrator/roles/manager.md",
		"# Manager role\n\nPlan exactly ONE implementer task: add a short, accurate comment "+
			"directly above the main function in hello.go explaining what it does, with no "+
			"other changes anywhere. Create that one task with hop task create, then close "+
			"the plan with hop plan close. Answer any worker questions plainly and directly. "+
			"If you receive a needs-rework notice for the task, retry it once with hop task "+
			"retry. Otherwise, keep polling for messages until the run finishes.\n", 0o644)
	repo.writeFile(t, ".herdr-orchestrator/roles/implementer.md",
		"# Implementer role\n\nImplement your assigned task exactly as instructed, commit "+
			"your change with git, and submit your result with hop result submit. Make no "+
			"changes beyond what your task instructions ask for.\n", 0o644)
	repo.writeFile(t, ".herdr-orchestrator/roles/reviewer.md",
		"# Reviewer role\n\nReview the diff between the base and head commits you are given. "+
			"Approve it if it does exactly what the task asked and nothing more; otherwise "+
			"reject it with a clear, specific reason. Submit your verdict with hop review "+
			"submit.\n", 0o644)
	repo.writeFile(t, configRelPath, liveFeatureConfigTOML(), 0o644)
	repo.Base = repo.commit(t, "initial commit")
	registerWorktreeCleanup(t, server, repo)
	return repo
}

// TestLiveClaudeFeatureRun is design section 11's "Live (opt-in)" row:
// real Claude Code as the manager, one worker task and a review, driven
// through the real `hop run --workflow feature`/`hop launch` pipeline
// against a fixture repository. Gated on HOP_LIVE_HARNESS=1 exactly like
// livehop_test.go's TestLiveClaudeDefaultProfileRun; unlike that solo
// scenario, every session here runs under an isolated, operator-prepared
// profile (requireLiveFeatureHarness requires HOP_LIVE_HARNESS_HOME) so
// this test never reads or writes the operator's own ~/.claude.json.
//
// WRITTEN AND COMPILE-CHECKED ONLY. Never run by this task — no
// production or test-support code path here executes it; only a human
// runs it deliberately, the same way as the solo live scenario (see the
// guide's "Live scenario" section). It costs real provider tokens across
// three real sessions and leaves session records under
// <home>/.claude/projects for the fixture repository's three worktree
// paths (the manager's own repo-root workspace plus each child's own
// worktree).
//
// Fake, plainly-non-functional credential-shaped values are seeded into
// the server's own environment exactly as TestLiveClaudeDefaultProfileRun
// does, for the identical reason: this pipeline has no fixture-worker-
// style self-report to read back, so the sanitization contract is instead
// evidenced by the run still completing normally through the isolated
// profile's own real, untouched credentials.
func TestLiveClaudeFeatureRun(t *testing.T) {
	claudePath, home := requireLiveFeatureHarness(t)

	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	installRealClaudeStub(t, server, claudePath)
	for _, name := range forbiddenCredentialVars {
		server.extraEnv = append(server.extraEnv, name+"=hop-test-not-a-secret-"+strings.ToLower(name))
	}
	// See TestLiveClaudeDefaultProfileRun's own identical comment: Claude
	// Code's credential storage is keyed by $USER, so a session whose HOME
	// carries a prepared profile but no USER at all cannot find its own
	// login even though that profile is genuinely authenticated. Passed
	// through only here, only from this test process's own environment —
	// the ordinary suite's hermetic environ() is left untouched.
	for _, name := range []string{"USER", "LOGNAME", "LANG", "LC_ALL"} {
		if value := os.Getenv(name); value != "" {
			server.extraEnv = append(server.extraEnv, name+"="+value)
		}
	}
	server.extraEnv = append(server.extraEnv, "HOME="+home)
	server.start(t)

	repo := newLiveFeatureFixtureRepo(t, artifacts, server)
	stateDir := artifacts.dir(t, "state")
	env := server.hopEnviron(stateDir)

	brief := "Coordinate exactly one implementer task, exactly as your role instructions describe, then let the review happen and the run complete. Do not plan more than one task."
	sp := server.startHopController(t, stateDir, "run", "run", "-C", repo.Root, "--workflow", "feature", brief)
	started := waitForControllerLog(t, artifacts, "run", "started")
	label, runID := extractRunID(t, started)

	fields, reached := waitForRunStateWithProcessDiagnostics(t, server, artifacts, "feature-run", env, repo.Root, runID, liveFeatureHarnessTimeout, "completed", "failed", "stopped")
	if !reached || fields["state"] != "completed" {
		if binding := fields["binding"]; binding != "" {
			artifacts.save(t, "worker-pane-scrollback.txt", server.readPane(t, paneIDFromBinding(binding)))
		}
		t.Fatalf("live feature run %s (%s) ended %q, want completed; detail: %+v\ncontroller stdout:\n%s", runID, label, fields["state"], fields, readControllerLog(t, artifacts, "run"))
	}
	waitForControllerExit(t, sp, 30*time.Second)

	dbPath := filepath.Join(stateDir, "hop.db")
	implementTaskState := querySQLite(t, dbPath, fmt.Sprintf("SELECT state FROM tasks WHERE run_id = '%s' AND kind = 'implement';", runID))
	if implementTaskState != "integrated" {
		t.Errorf("implement task state after the live feature run completed = %q, want \"integrated\"", implementTaskState)
	}
	reviewVerdict := querySQLite(t, dbPath, fmt.Sprintf("SELECT verdict FROM reviews WHERE run_id = '%s' ORDER BY submitted_at DESC LIMIT 1;", runID))
	if reviewVerdict != "approve" {
		t.Errorf("final review verdict for run %s = %q, want \"approve\" (the run could not otherwise have completed)", runID, reviewVerdict)
	}

	t.Logf("live feature run %s (%s) completed via real Claude Code as manager, implementer and reviewer, profile %s", runID, label, home)
}
