package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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

// waitForRunStateWithProcessDiagnostics polls exactly like waitForRunState
// (hopcmd_test.go), but on every poll where a worker pane binding is already
// known it also appends a foreground-process-list snapshot, taken through
// the same pane.process_info adapter call and rendering every other
// real-process scenario in this suite uses (testServer.processInfo,
// renderProcessInfo — see e.g. spike_identity_test.go,
// spike_launch_test.go). Evidence (a) that motivated this diagnostic — a
// prior live run that never settled left only the pane scrollback, with no
// record of which foreground process Herdr had actually reported at any
// point along the way — is exactly what a poll-by-poll trail closes: it
// survives even when the run never reaches a terminal state and the final
// failure path has nothing but scrollback left to read. tryProcessInfo is
// used instead of processInfo so a poll landing before the pane has a live
// runtime yet does not fail the test; the snapshot for that poll is simply
// omitted. The accumulated log is saved once, under name+"-process-info.txt",
// regardless of outcome (a passing run's artifact directory is removed on
// cleanup like every other evidence file).
func waitForRunStateWithProcessDiagnostics(t *testing.T, server *testServer, artifacts *artifactDir, name string, env []string, repoRoot, runID string, deadline time.Duration, want ...string) (fields map[string]string, reached bool) {
	t.Helper()
	var log strings.Builder
	reached = waitUntilDeadline(deadline, func() bool {
		result := runHop(t, env, repoRoot, "status", "-C", repoRoot, "-run", runID)
		fields = parseStatusDetail(result.Stdout)
		if binding := fields["binding"]; binding != "" {
			paneID := paneIDFromBinding(binding)
			if info, err := server.tryProcessInfo(t, paneID); err == nil {
				fmt.Fprintf(&log, "state=%s %s", fields["state"], renderProcessInfo(info))
			}
		}
		return slices.Contains(want, fields["state"])
	})
	if log.Len() > 0 {
		artifacts.save(t, name+"-process-info.txt", log.String())
	}
	return fields, reached
}

// TestLiveClaudeDefaultProfileRun is design section 9's opt-in live test:
// real Claude Code, in the operator's own default profile, driven through
// the real hop run/hop launch pipeline against the fixture repository with
// one small brief to a passing check, then a cold-relaunch
// ("claude --resume <preassigned-uuid> <continuation prompt>")
// continuation proving the native-session mechanism end to end against a
// real harness rather than the fixture worker's own --resume support
// (fixtureworker_test.go's isResumeInvocation). This run is also the
// VERIFICATION of the continuation prompt: interactive Claude Code
// restores the resumed transcript but does not re-run a pending user
// turn (observed live 2026-09-15 on claude 2.1.270 — a prompt-less
// `--resume` left the restored session idle at its input box), and
// `claude --help` documents the `[prompt]` positional; this test
// passing (2026-09-15, main 133ed5f, claude 2.1.270) is the executed
// evidence that a restored session acts on it — see
// docs/architecture/native-harness-compat.md's Claude Code verified
// list. Re-running this test is how a version drift gets re-verified.
// It never runs in the normal suite or make check —
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
	// environ() builds every subprocess's environment from scratch with no
	// login shell (harness_test.go's testServer.environ), so nothing here
	// inherits USER/LOGNAME by default — confirmed live: a `ps -E` on a real
	// worker pane spawned this way showed HOME, SHELL, TERM and XDG_* but no
	// USER, no LOGNAME. Claude Code's keychain credential item is keyed by
	// the account name ($USER; see "Credential storage" under Claude Code in
	// docs/architecture/native-harness-compat.md), so a worker whose HOME is
	// the operator's real home but whose USER is unset cannot find its own
	// login and reports "Not logged in" even though the operator is
	// authenticated. HOP's own sanitizing launcher (internal/app/launchenv.go's
	// StripMatrixV1) is a strip list that never names USER/LOGNAME/LANG/
	// LC_ALL, so passing them through is a harness/test-environment fix, not
	// a HOP behavior change — the ordinary suite's constructed environment
	// (testServer.environ) is left untouched; only this live test, and only
	// with values read from this test process's own environment, ever sets
	// them.
	for _, name := range []string{"USER", "LOGNAME", "LANG", "LC_ALL"} {
		if value := os.Getenv(name); value != "" {
			server.extraEnv = append(server.extraEnv, name+"="+value)
		}
	}
	server.extraEnv = append(server.extraEnv, "HOME="+home)
	server.start(t)

	repo := newFixtureRepo(t, artifacts, server, "repo")
	stateDir := artifacts.dir(t, "state")
	env := server.hopEnviron(stateDir)

	brief := "Add a short, accurate comment above the main function in hello.go explaining what it does, then commit the change. Make no other changes."
	sp := server.startHopController(t, stateDir, "run", "run", "-C", repo.Root, brief)
	started := waitForControllerLog(t, artifacts, "run", "started")
	label, runID := extractRunID(t, started)

	fields, reached := waitForRunStateWithProcessDiagnostics(t, server, artifacts, "first-launch", env, repo.Root, runID, liveHarnessTimeout, "running", "failed", "stopped")
	if !reached || fields["state"] != "running" {
		if binding := fields["binding"]; binding != "" {
			artifacts.save(t, "worker-pane-scrollback-first-launch.txt", server.readPane(t, paneIDFromBinding(binding)))
		}
		t.Fatalf("run %s never reached running for its first (real Claude) launch; got %q; detail: %+v (trust seed: %q — if the saved scrollback shows the \"Quick safety check\" workspace-trust dialog, this claude version no longer honors the pre-seeded key; see docs/architecture/native-harness-compat.md)\ncontroller stdout:\n%s", runID, fields["state"], fields, fields["trust seed"], readControllerLog(t, artifacts, "run"))
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

	// The harness config pins [worktrees] directory under the test's own
	// scratch root (harness_test.go's testConfig) for every server, not only
	// this one, precisely so a run like this one — whose HOME is the
	// operator's real home directory — can never create a worktree under
	// the operator's real ~/.herdr/worktrees default. Assert it held.
	worktreePath := querySQLite(t, dbPath, fmt.Sprintf("SELECT path FROM worktrees WHERE run_id = '%s';", runID))
	if worktreePath == "" {
		t.Fatalf("run %s has no recorded worktree path", runID)
	}
	resolvedWorktree, err := filepath.EvalSymlinks(worktreePath)
	if err != nil {
		t.Fatalf("resolve recorded worktree path %q: %v", worktreePath, err)
	}
	resolvedScratchRoot, err := filepath.EvalSymlinks(server.base)
	if err != nil {
		t.Fatalf("resolve test scratch root %q: %v", server.base, err)
	}
	if !strings.HasPrefix(resolvedWorktree, resolvedScratchRoot+string(os.PathSeparator)) {
		t.Fatalf("worktree path %q is not under the test's scratch root %q; the harness's [worktrees] directory pin may not be effective", resolvedWorktree, resolvedScratchRoot)
	}

	// Force a cold relaunch: kill the real claude worker and the
	// controller, wait out the abandoned lease, then confirm absence — the
	// same mechanism resume_test.go's coldRelaunchAfterCrash uses against
	// the fixture worker, driven here by hand since a live run's own
	// scratch environment differs (real HOME, real claude symlink).
	//
	// The worker pid is identified by the launch claim's own recorded pid
	// (launch_claims.pid for this attempt), never by ForegroundProcesses[0]:
	// against real Claude Code, the foreground process group also holds the
	// operator's own MCP servers, and Herdr lists foreground members
	// unsorted, so index 0 is not reliably the worker. At this point in the
	// test exactly one launch_claims row exists for this attempt (the
	// cold-relaunch's own row is written later), so this lookup is
	// unambiguous.
	claimPID := querySQLite(t, dbPath, fmt.Sprintf("SELECT pid FROM launch_claims WHERE attempt_id = '%s';", attemptID))
	if claimPID == "" {
		t.Fatalf("no launch_claims pid recorded for attempt %s", attemptID)
	}
	workerPID, err := strconv.Atoi(claimPID)
	if err != nil {
		t.Fatalf("launch_claims pid %q for attempt %s is not an integer: %v", claimPID, attemptID, err)
	}
	info := server.processInfo(t, firstPaneID)
	artifacts.save(t, "worker-pane-process-info-before-kill.txt", renderProcessInfo(info))
	found := false
	for _, p := range info.ForegroundProcesses {
		if int(p.PID) == workerPID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("launch claim pid %d not found among pane %s's foreground processes:\n%s", workerPID, firstPaneID, renderProcessInfo(info))
	}
	// End the pane through Herdr's own pane.close (this test's own server
	// client, an owned action against the server it is already connected
	// to) rather than a raw OS signal to workerPID, since that pid was
	// only OBSERVED via pane.process_info above, never one this harness
	// owns the Wait/reap lifecycle of — the OS could recycle it between
	// observation and signal (see fixtureworker_test.go's watchForSelfKill
	// doc comment for the same reasoning). A real Claude process cannot be
	// asked to self-kill via a control file the way the fixture can, so
	// pane.close is the safe substitute here: a layout.apply command pane
	// has no shell, its process IS the launched command directly (S6),
	// and production's own per-attempt retirement and stop paths already
	// rely on this same call. The call itself is not the proof, though:
	// design section 9's close rule makes the IMMEDIATE ABSENCE RE-
	// OBSERVATION decide (a pane.close answered pane_not_found closed
	// nothing), which is exactly the poll right below — never inferred
	// from the close call succeeding. This changes nothing about the
	// resume path under test: the same postcondition (pane gone by id,
	// confirmed below, and therefore gone by label and process too)
	// still drives hop resume --confirm-absent through the identical
	// corroboration/absence logic and the same "cold-relaunched"
	// disposition asserted afterward — only the mechanism producing that
	// postcondition changed, not what is proved.
	server.call(t, "pane.close", map[string]any{"pane_id": firstPaneID}, nil)
	if !waitUntil(func() bool { return !server.paneExists(t, firstPaneID) }) {
		t.Fatalf("pane %s still exists after pane.close", firstPaneID)
	}
	killControllerLeader(t, sp)
	waitForLeaseExpiry(t, dbPath, runID)

	resumed := server.startHopController(t, stateDir, "resume", "resume", "-C", repo.Root, "--confirm-absent", runID)
	resumeStdout := waitForControllerLog(t, artifacts, "resume", "resume cold-relaunched")
	if !strings.Contains(resumeStdout, "resume cold-relaunched") {
		t.Fatalf("hop resume --confirm-absent did not report cold-relaunched (native ref %s); stdout:\n%s", nativeRef, resumeStdout)
	}

	fields, reached = waitForRunStateWithProcessDiagnostics(t, server, artifacts, "second-launch", env, repo.Root, runID, liveHarnessTimeout, "completed", "failed", "stopped")
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
