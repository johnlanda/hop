package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// forbiddenClaudeEnv is the credential and provider-endpoint set that must
// never reach the probed claude: a probe that carried any of these could
// authenticate or contact a provider. The environment is built from scratch
// (an allowlist), so these are absent by construction; the set is asserted
// absent below. CLAUDE_CONFIG_DIR is deliberately NOT here: S4 sets it to a
// scratch profile on purpose (the launcher's default-profile stripping of an
// inherited override is a separate concern, covered by the design section 6
// contract and the launcher tests, not by this profile-isolation probe).
func forbiddenClaudeEnv() []string {
	return []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_HOME",
	}
}

// requireClaude returns the installed claude binary or skips. Only S4 runs the
// real harness, and only in print mode against a scratch profile.
func requireClaude(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("S4 skipped, not run: no claude binary on PATH (%v)", err)
	}
	return path
}

// TestSpikeClaudePreassignedSessionID is spike item S4: against the installed
// Claude Code, in print mode, in a scratch CLAUDE_CONFIG_DIR profile with a
// sanitized environment (no credentials, no provider base URLs), it
// establishes that
//
//   - `claude --session-id <uuid> -p …` is accepted and creates the
//     transcript under the pre-assigned UUID (not a claude-chosen one);
//   - `claude -p --resume <uuid>` finds that session from a different working
//     directory, progressing to the not-logged-in error (success: lookup
//     succeeded);
//   - an unknown UUID fails with "No conversation found", proving the lookup
//     is real and scoped to the profile.
//
// Progressing to the not-logged-in error is the success signal: the profile
// is empty of credentials and no provider is ever contacted. The exact claude
// version is recorded; this result is evidence for that version only and
// carries version-drift risk.
func TestSpikeClaudePreassignedSessionID(t *testing.T) {
	claude := requireClaude(t)

	scratch := t.TempDir()
	configDir := filepath.Join(scratch, "cfg")
	home := filepath.Join(scratch, "home")
	work := filepath.Join(scratch, "work")
	other := filepath.Join(scratch, "other")
	for _, dir := range []string{configDir, home, work, other} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A sanitized environment built from scratch: a minimal system PATH plus
	// the scratch profile and home. None of the forbidden variables are
	// present, so no credential or provider endpoint can leak in.
	env := []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin:" + filepath.Dir(claude),
		"HOME=" + home,
		"CLAUDE_CONFIG_DIR=" + configDir,
		"TERM=dumb",
	}
	assertNoForbiddenClaudeEnv(t, env)

	version := runClaude(t, claude, env, work, "--version")
	t.Logf("claude --version: %s", strings.TrimSpace(version))

	sessionID := newSpikeUUID(t)

	// 1. Pre-assigned session id accepted; not-logged-in is the success
	// signal that it ran without contacting a provider.
	out := runClaude(t, claude, env, work, "--session-id", sessionID, "-p", "Reply with the single word ok", "--model", "haiku")
	t.Logf("session-id launch output: %s", strings.TrimSpace(out))
	if !strings.Contains(out, "Not logged in") {
		t.Errorf("session-id launch did not reach the not-logged-in state; output:\n%s", out)
	}

	// The transcript exists under the pre-assigned UUID inside the profile.
	transcript := findTranscript(t, configDir, sessionID)
	if transcript == "" {
		t.Fatalf("no transcript named %s.jsonl created under the profile %s", sessionID, configDir)
	}
	t.Logf("transcript created at %s", transcript)

	// 2. Resume finds the pre-assigned session from a DIFFERENT cwd.
	resume := runClaude(t, claude, env, other, "-p", "--resume", sessionID, "hi", "--model", "haiku")
	t.Logf("resume output: %s", strings.TrimSpace(resume))
	if strings.Contains(resume, "No conversation found") {
		t.Errorf("resume of the pre-assigned session failed to find it:\n%s", resume)
	}
	if !strings.Contains(resume, "Not logged in") {
		t.Errorf("resume did not progress to the not-logged-in state (lookup success signal):\n%s", resume)
	}

	// 3. An unknown UUID is not found — the lookup is real.
	unknown := newSpikeUUID(t)
	missing := runClaude(t, claude, env, other, "-p", "--resume", unknown, "hi", "--model", "haiku")
	t.Logf("unknown-resume output: %s", strings.TrimSpace(missing))
	if !strings.Contains(missing, "No conversation found") {
		t.Errorf("resume of an unknown UUID did not report a missing conversation:\n%s", missing)
	}
}

// runClaude runs the installed claude with the sanitized environment and cwd,
// returning combined output. A non-zero exit (print-mode not-logged-in exits
// 0 on the probed version, but this stays tolerant) is not itself a failure;
// the assertions read the output.
func runClaude(t *testing.T, claude string, env []string, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, claude, args...) //nolint:gosec // G204: the installed claude under test, with arguments chosen by this suite.
	cmd.Env = env
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("claude %v timed out: %v\n%s", args, ctx.Err(), out)
	}
	if err != nil {
		// Record but do not fail: the not-logged-in path is the expected
		// outcome and its exit code is not the contract under test.
		t.Logf("claude %v exited: %v", args, err)
	}
	return string(out)
}

// findTranscript returns the path of the <sessionID>.jsonl transcript under
// the profile's projects tree, or empty if none exists.
func findTranscript(t *testing.T, configDir, sessionID string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(filepath.Join(configDir, "projects"), func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if filepath.Base(path) == sessionID+".jsonl" {
			found = path
		}
		return nil
	})
	if err != nil {
		t.Logf("walk profile projects: %v", err)
	}
	return found
}

// assertNoForbiddenClaudeEnv fails if any forbidden variable is present in the
// environment the probe will hand to claude.
func assertNoForbiddenClaudeEnv(t *testing.T, env []string) {
	t.Helper()
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		for _, forbidden := range forbiddenClaudeEnv() {
			if name == forbidden {
				t.Fatalf("the sanitized claude environment still carries forbidden variable %s", forbidden)
			}
		}
	}
}
