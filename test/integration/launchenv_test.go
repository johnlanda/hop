package integration

import (
	"fmt"
	"strings"
	"testing"
)

// TestRealProcessLaunchEnvironment establishes the compatibility result for
// each launch mode HOP can use to start a worker shell with an explicit
// environment: the workspace, tab and pane creation requests all carry an
// env map to the launched shell, so a HOP worker can be launched in a
// selected account/profile context. Each mode echoes an injected variable
// and reads it back, proving the environment reached the real process. The
// unsupported modes and the additive-only limitation are documented in
// docs/architecture/launch-environment.md and asserted below.
func TestRealProcessLaunchEnvironment(t *testing.T) {
	server := prepareServer(t, newArtifactDir(t))
	server.start(t)

	cases := []struct {
		name  string
		value string
		// launch creates a shell pane carrying env {HOP_ROLE: value} and
		// returns its pane id.
		launch func(t *testing.T, s *testServer, value string) string
	}{
		{
			name:   "workspace.create carries env to its root pane",
			value:  "manager-1",
			launch: launchWorkspaceWithEnv,
		},
		{
			name:   "tab.create carries env to its root pane",
			value:  "reviewer-2",
			launch: launchTabWithEnv,
		},
		{
			name:   "pane.split carries env to the new pane",
			value:  "implementer-3",
			launch: launchSplitWithEnv,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pane := tc.launch(t, server, tc.value)
			server.echoEnv(t, pane, "HOP_ROLE")
			server.waitForPaneText(t, pane, "ROLE=["+tc.value+"]")
		})
	}
}

// TestRealProcessLaunchEnvironmentIsAdditive establishes the boundary of the
// launch environment: the env map adds variables the shell sees, but a
// Herdr-managed variable is authoritative and cannot be overridden through
// it. A clean-slate launch that must unset an inherited variable therefore
// needs a sanitizing launcher argv, not the env map alone (see
// docs/architecture/launch-environment.md and native-harness-compat.md).
func TestRealProcessLaunchEnvironmentIsAdditive(t *testing.T) {
	server := prepareServer(t, newArtifactDir(t))
	server.start(t)

	var workspace struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	server.call(t, "workspace.create", map[string]any{"cwd": server.workDir(), "focus": true}, &workspace)

	var split struct {
		Pane struct {
			PaneID string `json:"pane_id"`
		} `json:"pane"`
	}
	// The env map both sets a normal variable and attempts to override a
	// Herdr-managed one; only the normal variable takes effect.
	server.call(t, "pane.split", map[string]any{
		"pane_id":   workspace.RootPane.PaneID,
		"direction": "right",
		"focus":     false,
		"env":       map[string]string{"HOP_ROLE": "worker", "HERDR_PANE_ID": "spoofed"},
	}, &split)
	pane := split.Pane.PaneID

	server.call(t, "pane.send_text", map[string]any{
		"pane_id": pane,
		"text":    "printf 'ROLE=[%s]PANE=[%s]\\n' \"$HOP_ROLE\" \"$HERDR_PANE_ID\"\n",
	}, nil)

	// The normal variable is delivered.
	server.waitForPaneText(t, pane, "ROLE=[worker]")
	// The Herdr-managed variable keeps its real value; the override is ignored.
	server.waitForPaneText(t, pane, "PANE=["+pane+"]")
	snapshot := server.readPane(t, pane)
	if strings.Contains(snapshot, "PANE=[spoofed]") {
		t.Errorf("the launch env overrode the Herdr-managed HERDR_PANE_ID; want Herdr's value to win\n%s", snapshot)
	}
}

// launchWorkspaceWithEnv creates a workspace whose root pane carries the env.
func launchWorkspaceWithEnv(t *testing.T, s *testServer, value string) string {
	t.Helper()
	var workspace struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	s.call(t, "workspace.create", map[string]any{
		"cwd":   s.workDir(),
		"focus": true,
		"env":   map[string]string{"HOP_ROLE": value},
	}, &workspace)
	return workspace.RootPane.PaneID
}

// launchTabWithEnv creates a tab whose root pane carries the env.
func launchTabWithEnv(t *testing.T, s *testServer, value string) string {
	t.Helper()
	var base struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
	}
	s.call(t, "workspace.create", map[string]any{"cwd": s.workDir(), "focus": true}, &base)
	var tab struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	s.call(t, "tab.create", map[string]any{
		"workspace_id": base.Workspace.WorkspaceID,
		"focus":        true,
		"env":          map[string]string{"HOP_ROLE": value},
	}, &tab)
	return tab.RootPane.PaneID
}

// launchSplitWithEnv splits a fresh workspace's root pane, carrying the env
// on the new pane.
func launchSplitWithEnv(t *testing.T, s *testServer, value string) string {
	t.Helper()
	var workspace struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	s.call(t, "workspace.create", map[string]any{"cwd": s.workDir(), "focus": true}, &workspace)
	var split struct {
		Pane struct {
			PaneID string `json:"pane_id"`
		} `json:"pane"`
	}
	s.call(t, "pane.split", map[string]any{
		"pane_id":   workspace.RootPane.PaneID,
		"direction": "down",
		"focus":     false,
		"env":       map[string]string{"HOP_ROLE": value},
	}, &split)
	return split.Pane.PaneID
}

// echoEnv writes a command into a pane that prints a named variable in a
// stable ROLE=[value] form the pane read can match.
func (s *testServer) echoEnv(t *testing.T, paneID, name string) {
	t.Helper()
	s.call(t, "pane.send_text", map[string]any{
		"pane_id": paneID,
		"text":    fmt.Sprintf("printf 'ROLE=[%%s]\\n' \"$%s\"\n", name),
	}, nil)
}
