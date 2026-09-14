package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSpikeRestorePlainPaneLosesAdditiveEnv is part of spike item S3: on a
// normal-configuration server (session.resume_agents_on_restore defaults
// true; the test config does not disable it), a graceful restart on the same
// roots restores a pane's identity — its creation-time label round-trips —
// but the restored pane's shell does NOT carry the creation-time additive
// environment. The pane was created with an additive HOP_RUN_ID; after the
// restart the same variable reads empty in the restored shell. This is the
// executed counterpart to the audited cold-restore path building
// PaneLaunchEnv::from_extra(Vec::new()) (repos/herdr/src/persist/restore.rs).
func TestSpikeRestorePlainPaneLosesAdditiveEnv(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)

	runID := newSpikeUUID(t)
	marker := "hop-spike-restore-" + runID[:8]

	var workspace struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
	}
	server.call(t, "workspace.create", map[string]any{"cwd": server.workDir(), "focus": true}, &workspace)
	var tab struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	server.call(t, "tab.create", map[string]any{
		"workspace_id": workspace.Workspace.WorkspaceID,
		"focus":        true,
		"label":        marker,
		"env":          map[string]string{"HOP_RUN_ID": runID},
	}, &tab)
	pane := tab.RootPane.PaneID

	// Before the restart the additive variable is present.
	server.call(t, "pane.send_text", map[string]any{
		"pane_id": pane,
		"text":    "printf 'BEFORE=[%s]\\n' \"$HOP_RUN_ID\"\n",
	}, nil)
	server.waitForPaneText(t, pane, "BEFORE=["+runID+"]")
	artifacts.save(t, "snapshot-before-restart.txt", server.snapshotDump(t))

	// Graceful restart on the same roots, then a client to drive geometry.
	server.restart(t)
	client := server.attachPTYClient(t)
	defer client.close(t)

	// The pane is restored: its creation-time label is found in the fresh
	// server's snapshot, mapped to a (new) pane id.
	var restoredPane string
	if !waitUntil(func() bool {
		restoredPane = server.snapshotPaneIDForLabel(t, marker)
		return restoredPane != ""
	}) {
		t.Fatalf("no restored pane carried the creation label %q; snapshot:\n%s", marker, server.snapshotDump(t))
	}
	artifacts.save(t, "snapshot-after-restart.txt", server.snapshotDump(t))

	// The restored shell has lost the additive variable.
	server.call(t, "pane.send_text", map[string]any{
		"pane_id": restoredPane,
		"text":    "printf 'AFTER=[%s]DONE\\n' \"$HOP_RUN_ID\"\n",
	}, nil)
	snapshot := server.waitForPaneText(t, restoredPane, "AFTER=[")
	artifacts.save(t, "restored-pane.txt", snapshot)
	if strings.Contains(snapshot, "AFTER=["+runID+"]") {
		t.Errorf("restored pane still carries the additive HOP_RUN_ID; want it absent\n%s", snapshot)
	}
	if !strings.Contains(snapshot, "AFTER=[]DONE") {
		t.Errorf("restored shell did not report an empty HOP_RUN_ID as expected\n%s", snapshot)
	}
}

// TestSpikeRestoreAutoRelaunchBypassesLauncher is the design-critical half of
// S3: a pane with a recorded native agent session ("herdr:claude" + a session
// id) is relaunched on restart by Herdr's own auto-restore, which runs
// `claude --resume <id>` in a fresh login shell — bypassing any HOP launcher
// and carrying none of the creation-time additive HOP_* environment. It
// establishes (a) that and when the relaunch happens (deferred until a client
// supplies geometry), (b) what the delayed-restore window looks like in
// session.snapshot, (c) that the only observable "restoration progressed"
// signals are ordinary agent detection/status events, and (d) that the
// relaunched process's environment lacks the additive vars.
//
// The fixture directory is first on the pane shell's PATH, so the bare
// `claude` the resume command runs resolves to the fixture harness, which
// dumps its environment to a pid-named file in the worktree.
func TestSpikeRestoreAutoRelaunchBypassesLauncher(t *testing.T) {
	fixtures := buildSpikeFixtures(t)
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	// Resolve a bare `claude` to the fixture in every login shell, including
	// the one the deferred resume spawns. Both sh and zsh login shells read
	// these files from the temp HOME.
	fixtureDir := filepath.Dir(fixtures.claude)
	prepend := "PATH=\"" + fixtureDir + ":$PATH\"\nexport PATH\n"
	for _, name := range []string{".profile", ".zprofile", ".zshrc"} {
		if err := os.WriteFile(filepath.Join(server.homeDir(), name), []byte(prepend), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	server.start(t)

	runID, sessionID := newSpikeUUID(t), newSpikeUUID(t)
	var workspace struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
	}
	server.call(t, "workspace.create", map[string]any{"cwd": server.workDir(), "focus": true}, &workspace)
	var tab struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	server.call(t, "tab.create", map[string]any{
		"workspace_id": workspace.Workspace.WorkspaceID,
		"focus":        true,
		"env":          map[string]string{"HOP_RUN_ID": runID},
	}, &tab)
	pane := tab.RootPane.PaneID

	// Launch the fixture harness so the pane is a live, detected claude agent,
	// carrying the additive HOP_RUN_ID.
	server.call(t, "pane.send_text", map[string]any{
		"pane_id": pane,
		"text":    "exec claude --run " + runID + "\n",
	}, nil)
	server.waitForPaneText(t, pane, "SPIKE-HARNESS-STARTED name=[claude]")
	server.waitForPaneText(t, pane, "SPIKE-HOP_RUN_ID=["+runID+"]")
	server.waitForAgent(t, pane, "claude")
	initialEnvFiles := spikeEnvFiles(t, server.workDir())

	// Record the native conversation session so Herdr's auto-restore will
	// build a `claude --resume <sessionID>` plan for this pane.
	server.call(t, "pane.report_agent_session", map[string]any{
		"pane_id":          pane,
		"source":           "herdr:claude",
		"agent":            "claude",
		"agent_session_id": sessionID,
	}, &struct{}{})

	artifacts.save(t, "snapshot-before-restart.txt", server.snapshotDump(t))

	// Graceful restart on the same roots. The old server is fully reaped
	// before the new one starts.
	server.restart(t)

	// Delayed-restore window: right after restart, before any client supplies
	// geometry, capture what session.snapshot shows. Recorded as evidence;
	// the exact content of this window is the point of the observation.
	artifacts.save(t, "snapshot-delayed-restore-window.txt", server.snapshotDump(t))

	// A client supplies geometry, which is what lets the deferred resume fire.
	client := server.attachPTYClient(t)
	defer client.close(t)

	// The relaunch happened: a NEW fixture-harness env dump appears in the
	// worktree (a different pid from the pre-restart launch).
	var resumedEnvFile string
	if !waitUntil(func() bool {
		for f := range spikeEnvFiles(t, server.workDir()) {
			if !initialEnvFiles[f] {
				resumedEnvFile = f
				return true
			}
		}
		return false
	}) {
		t.Fatalf("auto-restore never relaunched the harness; snapshot:\n%s", server.snapshotDump(t))
	}
	artifacts.save(t, "snapshot-after-relaunch.txt", server.snapshotDump(t))

	content, err := os.ReadFile(resumedEnvFile) //nolint:gosec // G304: a path under this test's own worktree.
	if err != nil {
		t.Fatalf("read resumed env dump: %v", err)
	}
	environ := string(content)
	artifacts.save(t, "resumed-environ.txt", environ)

	// The resumed process was started via `claude --resume <sessionID>` — its
	// argv carries the resume flag, and its environment carries HERDR_* but
	// NOT the creation-time additive HOP_RUN_ID.
	if !strings.Contains(environ, "HERDR_PANE_ID=") {
		t.Errorf("resumed process environment lacks HERDR_ identity vars:\n%s", environ)
	}
	if strings.Contains(environ, "HOP_RUN_ID="+runID) {
		t.Errorf("resumed process unexpectedly kept the additive HOP_RUN_ID=%s; auto-restore was expected to drop it\n%s", runID, environ)
	}
}

// spikeEnvFiles returns the set of fixture env-dump files currently in dir.
func spikeEnvFiles(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read worktree dir: %v", err)
	}
	files := map[string]bool{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "spike-env-") {
			files[filepath.Join(dir, entry.Name())] = true
		}
	}
	return files
}

// snapshotDump renders session.snapshot's tabs and panes for evidence files.
func (s *testServer) snapshotDump(t *testing.T) string {
	t.Helper()
	var result struct {
		Snapshot struct {
			Tabs []struct {
				TabID string `json:"tab_id"`
				Label string `json:"label"`
			} `json:"tabs"`
			Panes []struct {
				PaneID      string `json:"pane_id"`
				Label       string `json:"label"`
				Agent       string `json:"agent"`
				AgentStatus string `json:"agent_status"`
			} `json:"panes"`
			Agents []struct {
				PaneID        string `json:"pane_id"`
				Agent         string `json:"agent"`
				LaunchPending bool   `json:"launch_pending"`
			} `json:"agents"`
		} `json:"snapshot"`
	}
	s.call(t, "session.snapshot", nil, &result)
	var out strings.Builder
	out.WriteString("tabs:\n")
	for _, tab := range result.Snapshot.Tabs {
		out.WriteString("  " + tab.TabID + " label=" + tab.Label + "\n")
	}
	out.WriteString("panes:\n")
	for _, pane := range result.Snapshot.Panes {
		out.WriteString("  " + pane.PaneID + " label=" + pane.Label + " agent=" + pane.Agent + " status=" + pane.AgentStatus + "\n")
	}
	out.WriteString("agents:\n")
	for _, agent := range result.Snapshot.Agents {
		out.WriteString("  " + agent.PaneID + " agent=" + agent.Agent + "\n")
	}
	return out.String()
}

// snapshotPaneIDForLabel returns the pane id whose tab carries the label, or
// empty if none. It maps a restored tab (found by its stable creation label)
// to its single pane.
func (s *testServer) snapshotPaneIDForLabel(t *testing.T, label string) string {
	t.Helper()
	var result struct {
		Snapshot struct {
			Tabs []struct {
				TabID string `json:"tab_id"`
				Label string `json:"label"`
			} `json:"tabs"`
			Panes []struct {
				PaneID string `json:"pane_id"`
				TabID  string `json:"tab_id"`
				Label  string `json:"label"`
			} `json:"panes"`
		} `json:"snapshot"`
	}
	s.call(t, "session.snapshot", nil, &result)
	// A tab label or a pane label may carry the marker; check both.
	for _, pane := range result.Snapshot.Panes {
		if pane.Label == label {
			return pane.PaneID
		}
	}
	tabID := ""
	for _, tab := range result.Snapshot.Tabs {
		if tab.Label == label {
			tabID = tab.TabID
		}
	}
	if tabID == "" {
		return ""
	}
	for _, pane := range result.Snapshot.Panes {
		if pane.TabID == tabID {
			return pane.PaneID
		}
	}
	return ""
}
