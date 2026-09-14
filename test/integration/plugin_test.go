package integration

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRealProcessPluginLifecycle links the staged HOP plugin into a
// disposable Herdr server, lists and invokes its actions, waits for the
// commands to complete and asserts their real output, opens the plugin pane,
// reads the rendered evidence and unlinks — with every follow-up operation
// driven by IDs returned from earlier responses, and the user-global plugin
// registry proven untouched.
func TestRealProcessPluginLifecycle(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	registryBefore := registrySnapshot(t)
	stage := stagePlugin(t)
	server.start(t)

	var linked struct {
		Plugin struct {
			PluginID   string `json:"plugin_id"`
			Enabled    bool   `json:"enabled"`
			PluginRoot string `json:"plugin_root"`
		} `json:"plugin"`
	}
	server.call(t, "plugin.link", map[string]any{"path": stage, "enabled": true}, &linked)
	if linked.Plugin.PluginID != "hop" || !linked.Plugin.Enabled {
		t.Fatalf("plugin.link = %+v, want plugin hop enabled", linked.Plugin)
	}

	assertRegistrationConfined(t, server)

	var actions struct {
		Actions []struct {
			PluginID string `json:"plugin_id"`
			ActionID string `json:"action_id"`
		} `json:"actions"`
	}
	server.call(t, "plugin.action.list", map[string]any{"plugin_id": linked.Plugin.PluginID}, &actions)
	got := map[string]bool{}
	for _, action := range actions.Actions {
		got[action.ActionID] = true
	}
	if !got["doctor"] || !got["show-context"] {
		t.Fatalf("plugin.action.list = %+v, want the doctor and show-context actions", actions.Actions)
	}

	// The invocation response is not success: completion is the log record
	// the returned log ID resolves to, with its exit code and output.
	contextRecord := server.invokeAndAwait(t, "show-context")
	assertRecordSucceeded(t, &contextRecord, "show-context")
	for _, want := range []string{
		"trigger: action show-context\n",
		"plugin: hop\n",
		"socket: " + server.socketPath + "\n",
	} {
		if !strings.Contains(contextRecord.Stdout, want) {
			t.Errorf("show-context stdout lacks %q; got:\n%s", want, contextRecord.Stdout)
		}
	}
	// Herdr reports some injected paths with symbolic links resolved (on
	// macOS /var is a link into /private/var), so paths are compared
	// canonically rather than textually.
	if root := describedValue(contextRecord.Stdout, "root"); !samePath(t, root, stage) {
		t.Errorf("reported plugin root %q is not the staged directory %q", root, stage)
	}
	stateDir := describedValue(contextRecord.Stdout, "state dir")
	if !strings.HasPrefix(canonicalPath(t, stateDir), canonicalPath(t, server.base)+string(filepath.Separator)) {
		t.Errorf("state dir %q is not confined to the test roots %s", stateDir, server.base)
	}

	doctorRecord := server.invokeAndAwait(t, "doctor")
	artifacts.save(t, "action-doctor-stdout.txt", doctorRecord.Stdout)
	assertRecordSucceeded(t, &doctorRecord, "doctor")
	for _, want := range []string{
		"server socket: herdr ", // the action reached this test server's socket from inside the plugin environment
		"hop doctor: no problems found",
	} {
		if !strings.Contains(doctorRecord.Stdout, want) {
			t.Errorf("doctor stdout lacks %q; got:\n%s", want, doctorRecord.Stdout)
		}
	}

	var workspace struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	server.call(t, "workspace.create", map[string]any{"cwd": filepath.Join(server.base, "work"), "focus": true}, &workspace)
	if workspace.RootPane.PaneID == "" {
		t.Fatal("workspace.create returned no root pane id")
	}

	var opened struct {
		PluginPane struct {
			Entrypoint string `json:"entrypoint"`
			Pane       struct {
				PaneID string `json:"pane_id"`
			} `json:"pane"`
		} `json:"plugin_pane"`
	}
	server.call(t, "plugin.pane.open", map[string]any{
		"plugin_id":      "hop",
		"entrypoint":     "doctor",
		"placement":      "split",
		"target_pane_id": workspace.RootPane.PaneID,
		"focus":          false,
	}, &opened)
	paneID := opened.PluginPane.Pane.PaneID
	if opened.PluginPane.Entrypoint != "doctor" || paneID == "" {
		t.Fatalf("plugin.pane.open = %+v, want the doctor entrypoint with a pane id", opened.PluginPane)
	}

	var snapshot string
	rendered := waitUntil(func() bool {
		snapshot = server.readPane(t, paneID)
		return strings.Contains(snapshot, "herdr binary")
	})
	artifacts.save(t, "plugin-pane.txt", snapshot)
	if !rendered {
		t.Fatalf("the plugin pane never rendered doctor output; last snapshot:\n%s", snapshot)
	}

	server.call(t, "pane.close", map[string]any{"pane_id": paneID}, nil)

	var unlinked struct {
		Removed bool `json:"removed"`
	}
	server.call(t, "plugin.unlink", map[string]any{"plugin_id": "hop"}, &unlinked)
	if !unlinked.Removed {
		t.Fatalf("plugin.unlink = %+v, want removed", unlinked)
	}
	var plugins struct {
		Plugins []struct {
			PluginID string `json:"plugin_id"`
		} `json:"plugins"`
	}
	server.call(t, "plugin.list", nil, &plugins)
	if len(plugins.Plugins) != 0 {
		t.Fatalf("plugin.list after unlink = %+v, want none", plugins.Plugins)
	}

	assertNoRegistryLeak(t, registryBefore)
}

// TestRealProcessStartupHookRunsOnServerStart registers the plugin while no
// server is running, then starts one and proves the startup hook ran on
// startup rather than on link: before the start there is no server to run
// anything, and afterwards the startup log record exists with the hook's
// output.
func TestRealProcessStartupHookRunsOnServerStart(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	registryBefore := registrySnapshot(t)
	stage := stagePlugin(t)

	out, err := server.runCLI(t, "plugin", "link", stage)
	if err != nil {
		t.Fatalf("offline plugin link: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(server.socketPath); !os.IsNotExist(statErr) {
		t.Fatalf("socket %s exists before the server started (stat: %v); the link was not offline", server.socketPath, statErr)
	}
	listed, err := server.runCLI(t, "plugin", "list", "--json")
	if err != nil {
		t.Fatalf("offline plugin list: %v\n%s", err, listed)
	}
	if !strings.Contains(listed, "hop") {
		t.Fatalf("offline plugin list does not show the linked plugin:\n%s", listed)
	}

	server.start(t)
	record := server.waitForLog(t, func(r *pluginLogRecord) bool { return r.Event == "startup" })
	artifacts.save(t, "startup-stdout.txt", record.Stdout)
	assertRecordSucceeded(t, &record, "startup hook")
	for _, want := range []string{"trigger: startup\n", "plugin: hop\n"} {
		if !strings.Contains(record.Stdout, want) {
			t.Errorf("startup stdout lacks %q; got:\n%s", want, record.Stdout)
		}
	}

	assertNoRegistryLeak(t, registryBefore)
}

// invokeAndAwait invokes one manifest action and waits for the log record
// its response identifies to complete.
func (s *testServer) invokeAndAwait(t *testing.T, actionID string) pluginLogRecord {
	t.Helper()
	var invoked struct {
		Action struct {
			ActionID string `json:"action_id"`
		} `json:"action"`
		Log struct {
			LogID string `json:"log_id"`
		} `json:"log"`
	}
	s.call(t, "plugin.action.invoke", map[string]any{"action_id": actionID, "plugin_id": "hop"}, &invoked)
	if invoked.Action.ActionID != actionID || invoked.Log.LogID == "" {
		t.Fatalf("plugin.action.invoke(%s) = %+v, want the action with a log id", actionID, invoked)
	}
	return s.waitForLog(t, func(r *pluginLogRecord) bool { return r.LogID == invoked.Log.LogID })
}

// assertRecordSucceeded fails unless a completed log record reports success.
func assertRecordSucceeded(t *testing.T, record *pluginLogRecord, label string) {
	t.Helper()
	if record.Status != "succeeded" {
		t.Fatalf("%s status = %s (stderr: %s)", label, record.Status, record.Stderr)
	}
	if record.ExitCode == nil || *record.ExitCode != 0 {
		t.Fatalf("%s exit code = %v, want 0 (stderr: %s)", label, record.ExitCode, record.Stderr)
	}
}

// describedValue extracts one "label: value" line from plugin-context output.
func describedValue(output, label string) string {
	for line := range strings.SplitSeq(output, "\n") {
		if value, ok := strings.CutPrefix(line, label+": "); ok {
			return value
		}
	}
	return ""
}

// canonicalPath resolves symbolic links so paths from different reporters
// compare equal.
func canonicalPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Errorf("resolve %s: %v", path, err)
		return path
	}
	return resolved
}

// samePath reports whether two paths name the same file after resolving
// symbolic links.
func samePath(t *testing.T, a, b string) bool {
	t.Helper()
	return canonicalPath(t, a) == canonicalPath(t, b)
}

// readPane reads the pane's recent text through the socket API.
func (s *testServer) readPane(t *testing.T, paneID string) string {
	t.Helper()
	var result struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	s.call(t, "pane.read", map[string]any{"pane_id": paneID, "source": "recent", "lines": 50}, &result)
	return result.Read.Text
}

// assertRegistrationConfined proves the linked plugin is recorded inside the
// test roots: some registry file under the temporary base names it.
func assertRegistrationConfined(t *testing.T, server *testServer) {
	t.Helper()
	found := false
	err := filepath.WalkDir(server.base, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || entry.Name() != "plugins.json" {
			return nil //nolint:nilerr // transient files may vanish mid-walk; the assertion is on what is found.
		}
		content, readErr := os.ReadFile(path) //nolint:gosec // G304: the path is inside this test's own temporary base.
		if readErr == nil && strings.Contains(string(content), "\"hop\"") {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk test roots: %v", err)
	}
	if !found {
		t.Error("no registry file under the test roots names the linked plugin; registration landed elsewhere")
	}
}
