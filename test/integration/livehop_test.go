package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// liveHarnessTimeout bounds one real Claude Code invocation within the live
// scenario: model latency is unpredictable and far slower than any fixture
// worker, so this is generous compared to every other timeout in this
// suite.
const liveHarnessTimeout = 10 * time.Minute

// requireLiveHarness gates the one real-harness scenario in this package.
// HOP_LIVE_HARNESS=1 opts in explicitly; unset or any other value skips
// with a one-line reason, exactly like every other opt-in gate in this
// suite. Once opted in, a missing claude binary is a FAILURE, never a skip
// — opting in means the operator expects this to actually run, so a setup
// problem must not be silently swallowed as "not applicable".
//
// It also resolves the HOME the worker pane will use. HOP_LIVE_HARNESS_HOME
// overrides it — it must be an absolute, existing directory, or the test
// fails clearly rather than silently falling back. Otherwise it is the
// operator's own real home directory: design section 9 calls for "real
// Claude Code in the user's default profile", which is the product's own
// delegated-account model (a worker runs in the harness's default profile,
// exactly as the operator's own manual QA does). CLAUDE_CONFIG_DIR is never
// set by this test — the sanitizing launcher strips it (it is in the
// credential/profile strip matrix, docs/plan/phase-2-design.md section 6)
// — so the default profile this resolves to is exactly <home>/.claude.
func requireLiveHarness(t *testing.T) (claudePath, home string) {
	t.Helper()
	if os.Getenv("HOP_LIVE_HARNESS") != "1" {
		t.Skip("live-harness test skipped, not run: set HOP_LIVE_HARNESS=1 to opt in")
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		t.Fatalf("HOP_LIVE_HARNESS=1 but no claude binary is on PATH: %v", err)
	}
	if override := os.Getenv("HOP_LIVE_HARNESS_HOME"); override != "" {
		if !filepath.IsAbs(override) {
			t.Fatalf("HOP_LIVE_HARNESS_HOME %q is not an absolute path", override)
		}
		info, statErr := os.Stat(override) //nolint:gosec // G703: the operator chose this path to point the live worker pane at a profile they prepared.
		if statErr != nil {
			t.Fatalf("HOP_LIVE_HARNESS_HOME %q: %v", override, statErr)
		}
		if !info.IsDir() {
			t.Fatalf("HOP_LIVE_HARNESS_HOME %q is not a directory", override)
		}
		return path, override
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve the operator's real home directory: %v", err)
	}
	return path, realHome
}

// installRealClaudeStub symlinks the real, installed claude binary as the
// server's "claude" PATH stub — a symlink rather than installFixtureWorkerAsClaudeStub's
// plain file copy, so any lookup claude itself does relative to its own
// resolved path (shared resources, a wrapper script's own directory) still
// resolves correctly.
func installRealClaudeStub(t *testing.T, server *testServer, claudePath string) {
	t.Helper()
	target := filepath.Join(server.base, "bin", "claude")
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove existing claude stub: %v", err)
	}
	if err := os.Symlink(claudePath, target); err != nil {
		t.Fatalf("symlink real claude as the claude stub: %v", err)
	}
}

// liveTranscriptExists reports whether a Claude Code transcript for
// nativeRef exists anywhere under home's profile, the same evidence
// spike_claude_session_test.go's findTranscript looks for, confirming the
// launched harness actually used the default (unmodified) profile location
// rather than one CLAUDE_CONFIG_DIR quietly redirected it to.
func liveTranscriptExists(t *testing.T, home, nativeRef string) bool {
	t.Helper()
	found := false
	root := filepath.Join(home, ".claude", "projects")
	err := filepath.WalkDir(root, func(path string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil //nolint:nilerr // a missing/unreadable subtree is absence, not a walk failure worth aborting on.
		}
		if filepath.Base(path) == nativeRef+".jsonl" {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Logf("walk %s: %v", root, err)
	}
	return found
}

// TestLiveClaudeDefaultProfileRun is design section 9's opt-in live test:
// real Claude Code, in the operator's own default profile, driven through
// the real hop run/hop launch pipeline against the fixture repository with
// one small brief to a passing check, then a cold-relaunch
// ("claude --resume <preassigned-uuid>") continuation proving the
// native-session mechanism end to end against a real harness rather than
// the fixture worker's own --resume support (fixtureworker_test.go's
// isResumeInvocation). It never runs in the normal suite or make check —
// only the Makefile's test-live target ever sets HOP_LIVE_HARNESS=1 — and
// it is never run by this task's own automated verification; a human runs
// it deliberately.
//
// This costs real provider tokens and leaves a session record under
// <home>/.claude/projects for the fixture repository's worktree path (see
// the guide's "Live scenario" section).
//
// Fake, plainly-non-functional credential-shaped values are seeded into
// the server's own environment exactly as TestRealProcessSanitizedLaunchExec
// does: since this pipeline has no fixture-worker-style self-report to read
// back, the sanitization contract is instead evidenced by the run still
// completing normally through the operator's own real, untouched
// credentials — a leaked fake value would plausibly break real
// authentication rather than silently succeed.
func TestLiveClaudeDefaultProfileRun(t *testing.T) {
	claudePath, home := requireLiveHarness(t)

	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	installRealClaudeStub(t, server, claudePath)
	for _, name := range forbiddenCredentialVars {
		server.extraEnv = append(server.extraEnv, name+"=hop-test-not-a-secret-"+strings.ToLower(name))
	}
	server.extraEnv = append(server.extraEnv, "HOME="+home)
	server.start(t)

	repo := newFixtureRepo(t, artifacts, "repo")
	stateDir := artifacts.dir(t, "state")
	env := server.hopEnviron(stateDir)

	brief := "Add a short, accurate comment above the main function in hello.go explaining what it does, then commit the change. Make no other changes."
	sp := server.startHopController(t, stateDir, "run", "run", "-C", repo.Root, brief)
	started := waitForControllerLog(t, artifacts, "run", "started")
	label, runID := extractRunID(t, started)

	fields, reached := waitForRunState(t, env, repo.Root, runID, liveHarnessTimeout, "running", "failed", "stopped")
	if !reached || fields["state"] != "running" {
		if binding := fields["binding"]; binding != "" {
			artifacts.save(t, "worker-pane-scrollback-first-launch.txt", server.readPane(t, paneIDFromBinding(binding)))
		}
		t.Fatalf("run %s never reached running for its first (real Claude) launch; got %q; detail: %+v\ncontroller stdout:\n%s", runID, fields["state"], fields, readControllerLog(t, artifacts, "run"))
	}
	firstPaneID := paneIDFromBinding(fields["binding"])

	dbPath := filepath.Join(stateDir, "hop.db")
	taskID := querySQLite(t, dbPath, fmt.Sprintf("SELECT id FROM tasks WHERE run_id = '%s';", runID))
	attemptID := querySQLite(t, dbPath, fmt.Sprintf("SELECT id FROM attempts WHERE task_id = '%s';", taskID))
	nativeRef := querySQLite(t, dbPath, fmt.Sprintf(
		"SELECT native_session_ref FROM sessions WHERE attempt_id = '%s';", attemptID))
	if nativeRef == "" {
		t.Fatalf("no native_session_ref recorded for attempt %s after the first launch settled", attemptID)
	}

	// Force a cold relaunch: kill the real claude worker and the
	// controller, wait out the abandoned lease, then confirm absence — the
	// same mechanism resume_test.go's coldRelaunchAfterCrash uses against
	// the fixture worker, driven here by hand since a live run's own
	// scratch environment differs (real HOME, real claude symlink).
	info := server.processInfo(t, firstPaneID)
	if len(info.ForegroundProcesses) == 0 {
		t.Fatalf("pane %s reports no foreground worker process before the forced cold relaunch", firstPaneID)
	}
	workerPID := int(info.ForegroundProcesses[0].PID)
	if err := syscall.Kill(workerPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill live claude worker pid %d: %v", workerPID, err)
	}
	if !waitUntil(func() bool { return !server.paneExists(t, firstPaneID) }) {
		t.Fatalf("pane %s still exists after its foreground worker was killed", firstPaneID)
	}
	killControllerLeader(t, sp)
	waitForLeaseExpiry(t, dbPath, runID)

	resumed := server.startHopController(t, stateDir, "resume", "resume", "-C", repo.Root, "--confirm-absent", runID)
	resumeStdout := waitForControllerLog(t, artifacts, "resume", "resume cold-relaunched")
	if !strings.Contains(resumeStdout, "resume cold-relaunched") {
		t.Fatalf("hop resume --confirm-absent did not report cold-relaunched (native ref %s); stdout:\n%s", nativeRef, resumeStdout)
	}

	fields, reached = waitForRunState(t, env, repo.Root, runID, liveHarnessTimeout, "completed", "failed", "stopped")
	if !reached || fields["state"] != "completed" {
		if binding := fields["binding"]; binding != "" {
			artifacts.save(t, "worker-pane-scrollback-second-launch.txt", server.readPane(t, paneIDFromBinding(binding)))
		}
		t.Fatalf("run %s ended %q after the cold-relaunched (--resume %s) continuation, want completed; detail: %+v\nresume controller stdout:\n%s", runID, fields["state"], nativeRef, fields, readControllerLog(t, artifacts, "resume"))
	}
	waitForControllerExit(t, resumed, 30*time.Second)

	if fields["last submit"] == "" || fields["last submit"] == "(none)" {
		t.Error("hop status reports no last submission after the live run completed")
	}
	if fields["last check"] == "" || fields["last check"] == "(none)" {
		t.Error("hop status reports no last check execution after the live run completed")
	}

	if !liveTranscriptExists(t, home, nativeRef) {
		t.Errorf("no Claude Code transcript %s.jsonl found under %s/.claude/projects; the default profile may not have been used", nativeRef, home)
	}

	sessionCount := querySQLite(t, dbPath, fmt.Sprintf("SELECT count(*) FROM sessions WHERE attempt_id = '%s';", attemptID))
	if sessionCount != "2" {
		t.Errorf("session count for attempt %s = %s, want 2 (the first launch, now lost, plus the --resume cold relaunch)", attemptID, sessionCount)
	}

	t.Logf("run %s (%s) completed via real Claude Code, native session %s, profile %s", runID, label, nativeRef, home)
}
