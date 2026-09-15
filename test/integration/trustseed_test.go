package integration

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// trustFixtureConfig is the profile .claude.json these scenarios seed: an
// unrelated project entry and an unknown key, so the assertions prove the
// seed touches exactly one key and preserves everything else byte for
// byte.
const trustFixtureConfig = `{"numStartups":3,"projects":{"/other/project":{"hasTrustDialogAccepted":false}},"customUnknownKey":{"nested":[1,2,3]}}`

// seededTrustConfig renders the exact document the seed must produce from
// trustFixtureConfig for worktreePath: the new entry inserted compactly at
// the head of the projects map, every other byte unchanged.
func seededTrustConfig(worktreePath string) string {
	return `{"numStartups":3,"projects":{"` + worktreePath + `":{"hasTrustDialogAccepted":true},"/other/project":{"hasTrustDialogAccepted":false}},"customUnknownKey":{"nested":[1,2,3]}}`
}

// resolvedWorktreePath reads the run's recorded worktree path and resolves
// its symlinks, which is exactly the launcher's own cwd resolution (macOS
// resolves the temp roots' /var prefix to /private/var).
func (f *fixtureRun) resolvedWorktreePath(t *testing.T) string {
	t.Helper()
	recorded := querySQLite(t, f.dbPath(), fmt.Sprintf("SELECT path FROM worktrees WHERE run_id = '%s';", f.runID))
	if recorded == "" {
		t.Fatalf("run %s has no recorded worktree path", f.runID)
	}
	resolved, err := filepath.EvalSymlinks(recorded)
	if err != nil {
		t.Fatalf("resolve recorded worktree path %q: %v", recorded, err)
	}
	return resolved
}

// seedEvidence reads the launch claim's recorded workspace-trust seed
// evidence.
func (f *fixtureRun) seedEvidence(t *testing.T) string {
	t.Helper()
	return querySQLite(t, f.dbPath(), fmt.Sprintf("SELECT seed_evidence FROM launch_claims WHERE run_id = '%s';", f.runID))
}

// TestRealProcessLaunchSeedsWorkspaceTrust proves the launcher-boundary
// workspace-trust pre-seed end to end against a real run: the worker
// pane's HOME carries a fixture .claude.json, and after the launch the
// file holds exactly projects[<resolved worktree path>]
// .hasTrustDialogAccepted = true with every other byte preserved, the
// launch claim records the seeded evidence, and hop status renders it.
func TestRealProcessLaunchSeedsWorkspaceTrust(t *testing.T) {
	artifacts, server := newFixtureRunEnv(t)
	configPath := filepath.Join(server.homeDir(), ".claude.json")
	if err := os.WriteFile(configPath, []byte(trustFixtureConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := newFixtureRepo(t, artifacts, server, "repo")
	fx := startRun(t, artifacts, server, repo, "submit-valid")

	fields := fx.requireRunState(t, "completed")

	worktreePath := fx.resolvedWorktreePath(t)
	content, err := os.ReadFile(configPath) //nolint:gosec // G304: a path this test constructed itself under the disposable server's temp home.
	if err != nil {
		t.Fatalf("read seeded profile config: %v", err)
	}
	if want := seededTrustConfig(worktreePath); string(content) != want {
		t.Errorf("seeded profile config:\n%s\nwant:\n%s", content, want)
	}

	wantEvidence := "workspace trust seeded for " + worktreePath + " (verified; best-effort against external profile writers)"
	if evidence := fx.seedEvidence(t); evidence != wantEvidence {
		t.Errorf("launch claim seed evidence = %q, want %q", evidence, wantEvidence)
	}
	if got := fields["trust seed"]; got != wantEvidence {
		t.Errorf("hop status trust seed line = %q, want %q", got, wantEvidence)
	}
}

// TestRealProcessLaunchWithoutProfileConfigNotSeeded proves the absent
// profile-config fallback: with no .claude.json in the worker pane's HOME
// the launch proceeds unchanged to completion, the claim records the
// not-seeded evidence, and no profile config is created.
func TestRealProcessLaunchWithoutProfileConfigNotSeeded(t *testing.T) {
	fx := startFixtureRun(t, "submit-valid")

	fields := fx.requireRunState(t, "completed")

	wantEvidence := "workspace trust not seeded: profile config absent"
	if evidence := fx.seedEvidence(t); evidence != wantEvidence {
		t.Errorf("launch claim seed evidence = %q, want %q", evidence, wantEvidence)
	}
	if got := fields["trust seed"]; got != wantEvidence {
		t.Errorf("hop status trust seed line = %q, want %q", got, wantEvidence)
	}
	configPath := filepath.Join(fx.server.homeDir(), ".claude.json")
	if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an absent profile config was created at the worker HOME (stat err = %v)", err)
	}
}
