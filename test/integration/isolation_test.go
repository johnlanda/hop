package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRealProcessHomeIsIsolated proves a pane shell runs under the test-owned
// home, not the developer's: its $HOME is the temp home, and the startup file
// it sources is the temp one. If a pane used the developer's real home, this
// sentinel — under the temp home — would never appear.
func TestRealProcessHomeIsIsolated(t *testing.T) {
	server := prepareServer(t, newArtifactDir(t))

	// A startup file in the test-owned home writes a sentinel when the login
	// shell of a pane sources it. It must be in place before the server (and
	// its root pane) starts.
	sentinel := filepath.Join(server.homeDir(), "profile-sourced")
	profile := "printf '' > " + sentinel + "\n"
	if err := os.WriteFile(filepath.Join(server.homeDir(), ".profile"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}

	server.start(t)

	// Creating a workspace starts a root pane whose login shell sources the
	// temp .profile, writing the sentinel inside the test roots.
	var workspace struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	server.call(t, "workspace.create", map[string]any{"cwd": server.workDir(), "focus": true}, &workspace)

	if !waitUntil(func() bool { _, err := os.Stat(sentinel); return err == nil }) {
		t.Fatalf("the temp-home .profile sentinel %s was never written; a pane did not source the test-owned home", sentinel)
	}
	if !strings.HasPrefix(sentinel, server.base+string(filepath.Separator)) {
		t.Errorf("sentinel %s is not confined to the test roots %s", sentinel, server.base)
	}

	// A pane shell's $HOME is the test-owned home, not the developer's.
	server.call(t, "pane.send_text", map[string]any{
		"pane_id": workspace.RootPane.PaneID,
		"text":    "printf 'HOME=[%s]\\n' \"$HOME\"\n",
	}, nil)
	server.waitForPaneText(t, workspace.RootPane.PaneID, "HOME=["+server.homeDir()+"]")
	if realHome := os.Getenv("HOME"); realHome != "" && strings.Contains(server.readPane(t, workspace.RootPane.PaneID), "HOME=["+realHome+"]") {
		t.Errorf("pane HOME is the developer's real home %q; it must be the temp home", realHome)
	}
}

// TestRealProcessDoctorUsesControlledHarnessPath proves the plugin doctor
// action resolves the native-harness names to the fixture's stub executables,
// never the developer's real harness binaries: the reported harness version is
// the stub's. The plugin command is a process launched with the fixture PATH
// (stub directory first), so it does not depend on a login shell's PATH order.
func TestRealProcessDoctorUsesControlledHarnessPath(t *testing.T) {
	server := prepareServer(t, newArtifactDir(t))
	stage := stagePlugin(t)
	server.start(t)
	server.call(t, "plugin.link", map[string]any{"path": stage, "enabled": true}, &struct{}{})

	record := server.invokeAndAwait(t, "doctor")
	if !strings.Contains(record.Stdout, "0.0.0-stub") {
		t.Errorf("doctor did not resolve the stub harnesses; a real harness may have run:\n%s", record.Stdout)
	}
	for _, real := range realHarnessMarkers() {
		if strings.Contains(record.Stdout, real) {
			t.Errorf("doctor output names a real harness version %q; the stub PATH did not shadow it:\n%s", real, record.Stdout)
		}
	}
}

// realHarnessMarkers are substrings a real harness --version would print but a
// stub would not, used to confirm no real harness was executed.
func realHarnessMarkers() []string {
	return []string{"(Claude Code)", "codex-cli"}
}
