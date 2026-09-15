package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/herdr"
)

// TestSpikeLayoutApplyCommandPane is spike item S6: of every pane-creating
// surface, only layout.apply pane nodes carry a command argv (tab.create,
// workspace.create and pane.split carry env but no command — see their
// request schemas). A layout.apply pane node with command + env + label runs
// that argv directly as the pane's process, with no shell underneath: the
// pane's command IS the launch line, so no text injection and no login-shell
// startup files are involved. The label is a creation-time marker (S7) that
// session.snapshot reports back.
func TestSpikeLayoutApplyCommandPane(t *testing.T) {
	fixtures := buildSpikeFixtures(t)
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)

	var workspace struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
	}
	server.call(t, "workspace.create", map[string]any{"cwd": server.workDir(), "focus": true}, &workspace)

	runID := newSpikeUUID(t)
	marker := "hop-spike-" + runID[:8]
	envFile := filepath.Join(server.workDir(), "layout-pane-env.txt")
	var applied struct {
		Layout struct {
			TabID string `json:"tab_id"`
			Root  struct {
				PaneID  string   `json:"pane_id"`
				Label   string   `json:"label"`
				Command []string `json:"command"`
			} `json:"root"`
		} `json:"layout"`
	}
	server.call(t, "layout.apply", map[string]any{
		"workspace_id": workspace.Workspace.WorkspaceID,
		"focus":        true,
		"root": map[string]any{
			"type":    "pane",
			"label":   marker,
			"cwd":     server.workDir(),
			"command": []string{fixtures.claude, "--run", runID},
			"env": map[string]string{
				"HOP_RUN_ID":         runID,
				"HOP_SPIKE_ENV_FILE": envFile,
			},
		},
	}, &applied)
	pane := applied.Layout.Root.PaneID
	if pane == "" {
		t.Fatal("layout.apply returned no root pane id")
	}

	// The pane process is the argv itself, carrying the additive env.
	server.waitForPaneText(t, pane, "SPIKE-HARNESS-STARTED name=[claude]")
	server.waitForPaneText(t, pane, "SPIKE-HOP_RUN_ID=["+runID+"]")
	info := server.waitForForegroundProcess(t, pane)
	artifacts.save(t, "layout-pane-process-info.txt", renderProcessInfo(info))
	process := foregroundClaudeProcess(info)
	if got := strings.Join(process.Argv, " "); got != fixtures.claude+" --run "+runID {
		t.Errorf("pane process argv = %q, want exactly the layout command", got)
	}
	// No shell underneath: the pane's child process is the command process.
	if process.PID != info.ShellPID {
		t.Errorf("command pane has shell_pid=%d distinct from command pid=%d; expected the command to be the pane child",
			info.ShellPID, process.PID)
	}

	// The recognized process name is detected as an agent exactly as in S1.
	record := server.waitForAgent(t, pane, "claude")
	t.Logf("layout command pane agent record: %+v", record)

	// The full environment dump proves Herdr's identity variables and the
	// additive env reached the process, with no login-shell rc in between.
	var environ string
	if !waitUntil(func() bool {
		content, err := os.ReadFile(envFile) //nolint:gosec // G304: the path is inside this test's own roots.
		environ = string(content)
		return err == nil && strings.Contains(environ, "HERDR_PANE_ID=")
	}) {
		t.Fatal("the layout command pane never wrote its environment dump")
	}
	artifacts.save(t, "layout-pane-environ.txt", environ)
	for _, want := range []string{"HOP_RUN_ID=" + runID, "HERDR_PANE_ID=" + pane, "HERDR_SOCKET_PATH="} {
		if !strings.Contains(environ, want) {
			t.Errorf("layout command pane environment lacks %q", want)
		}
	}

	// The creation-time label round-trips through the apply response and
	// session.snapshot, so a crashed controller can recognize its own pane
	// by a unique marker chosen before creation (S7).
	if applied.Layout.Root.Label != marker {
		t.Errorf("layout.apply reported label %q, want %q", applied.Layout.Root.Label, marker)
	}
	snapshotPane := server.snapshotPaneByLabel(t, marker)
	if snapshotPane != pane {
		t.Errorf("session.snapshot finds pane %q by label %q, want %q", snapshotPane, marker, pane)
	}

	// When the command process exits the pane closes: an argv pane has no
	// surviving shell. This is recorded behavior the stop/reconcile design
	// depends on.
	server.call(t, "pane.send_text", map[string]any{"pane_id": pane, "text": "SPIKE-QUIT\n"}, nil)
	if !waitUntil(func() bool { return !server.paneExists(t, pane) }) {
		t.Errorf("command pane %s still exists after its process exited", pane)
	}
}

// TestSpikeLayoutApplyAddsTabToExistingWorkspace is the S6 follow-up: on a
// workspace that already holds tabs and panes, layout.apply with only a
// workspace_id ADDS one new tab containing the command pane: after it, every
// pre-existing tab and pane id still exists and exactly one tab and one pane
// were added. This asserts id survival and the +1 counts, not that no field of
// any pre-existing pane changed (a weaker, honest claim than "leaves every
// pre-existing pane untouched"). Replacement happens only when the request
// names a tab_id (the handler closes exactly that tab —
// repos/herdr/src/app/api/layouts.rs). It also asserts the new pane runs the
// exact command at the requested cwd, and records what a non-zero command exit
// looks like: the pane closes exactly as on a clean exit and the pane.exited
// payload carries no exit status (it is only {pane_id} —
// repos/herdr/src/api/schema/events.rs:178-180).
func TestSpikeLayoutApplyAddsTabToExistingWorkspace(t *testing.T) {
	fixtures := buildSpikeFixtures(t)
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)

	// A populated workspace: the root pane, a split beside it, and a second
	// tab — the shape of a user's normal server.
	var workspace struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	server.call(t, "workspace.create", map[string]any{"cwd": server.workDir(), "focus": true}, &workspace)
	server.splitPane(t, workspace.RootPane.PaneID)
	var extraTab struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	server.call(t, "tab.create", map[string]any{
		"workspace_id": workspace.Workspace.WorkspaceID, "focus": false, "label": "pre-existing",
	}, &extraTab)

	beforeTabs, beforePanes := server.snapshotIDs(t)
	artifacts.save(t, "snapshot-before-apply.txt", renderIDs(beforeTabs, beforePanes))

	// The exact request shape that adds one tab: workspace_id only (no
	// tab_id), a single pane node with cwd, command argv, env and label.
	runID := newSpikeUUID(t)
	marker := "hop-spike-add-" + runID[:8]
	var applied struct {
		Layout struct {
			TabID string `json:"tab_id"`
			Root  struct {
				PaneID string `json:"pane_id"`
			} `json:"root"`
		} `json:"layout"`
	}
	server.call(t, "layout.apply", map[string]any{
		"workspace_id": workspace.Workspace.WorkspaceID,
		"tab_label":    marker,
		"focus":        false,
		"root": map[string]any{
			"type":    "pane",
			"label":   marker,
			"cwd":     server.workDir(),
			"command": []string{fixtures.claude, "--run", runID},
			"env":     map[string]string{"HOP_RUN_ID": runID},
		},
	}, &applied)
	pane := applied.Layout.Root.PaneID
	server.waitForPaneText(t, pane, "SPIKE-HARNESS-STARTED name=[claude]")

	// The added pane runs exactly the requested command at the requested cwd.
	added := server.waitForForegroundProcess(t, pane)
	addedProcess := foregroundClaudeProcess(added)
	if got := strings.Join(addedProcess.Argv, " "); got != fixtures.claude+" --run "+runID {
		t.Errorf("added pane argv = %q, want exactly the layout command", got)
	}
	if !samePath(t, addedProcess.Cwd, server.workDir()) {
		t.Errorf("added pane cwd = %q, want the requested %q", addedProcess.Cwd, server.workDir())
	}

	afterTabs, afterPanes := server.snapshotIDs(t)
	artifacts.save(t, "snapshot-after-apply.txt", renderIDs(afterTabs, afterPanes))
	for _, tab := range beforeTabs {
		if !containsString(afterTabs, tab) {
			t.Errorf("pre-existing tab %s disappeared after layout.apply", tab)
		}
	}
	for _, existing := range beforePanes {
		if !containsString(afterPanes, existing) {
			t.Errorf("pre-existing pane %s disappeared after layout.apply", existing)
		}
	}
	if got, want := len(afterTabs), len(beforeTabs)+1; got != want {
		t.Errorf("tabs after apply = %d (%v), want %d: exactly one added", got, afterTabs, want)
	}
	if got, want := len(afterPanes), len(beforePanes)+1; got != want {
		t.Errorf("panes after apply = %d (%v), want %d: exactly one added", got, afterPanes, want)
	}

	// Non-zero exit: subscribe to the pane's exit and close events first,
	// then make the command exit 3. The pane closes exactly as on a clean
	// exit, and the recorded raw payloads show no exit status field.
	stream, err := server.client.Subscribe(testContext(t), []herdr.EventSubscription{
		{Type: "pane.exited", PaneID: pane},
		{Type: "pane.closed", PaneID: pane},
	})
	if err != nil {
		t.Fatalf("subscribe to exit events: %v", err)
	}
	defer func() {
		if closeErr := stream.Close(); closeErr != nil {
			t.Logf("close event stream: %v", closeErr)
		}
	}()
	server.call(t, "pane.send_text", map[string]any{"pane_id": pane, "text": "SPIKE-QUIT-FAIL\n"}, nil)
	if !waitUntil(func() bool { return !server.paneExists(t, pane) }) {
		t.Errorf("command pane %s still exists after its process exited non-zero", pane)
	}
	events := drainEventNames(t, stream, "pane.exited")
	artifacts.save(t, "exit-events.txt", events)
	if !strings.Contains(events, "pane_exited") {
		t.Errorf("no pane exited event observed for the non-zero exit; events:\n%s", events)
	}
	if strings.Contains(events, "exit_code") || strings.Contains(events, "status") {
		t.Errorf("exit events unexpectedly carry a status field; the spike recorded none in the schema:\n%s", events)
	}
}

// snapshotPaneByLabel returns the pane id of the single session.snapshot
// pane carrying the label, failing if none or several match.
func (s *testServer) snapshotPaneByLabel(t *testing.T, label string) string {
	t.Helper()
	var result struct {
		Snapshot struct {
			Panes []struct {
				PaneID string `json:"pane_id"`
				Label  string `json:"label"`
			} `json:"panes"`
		} `json:"snapshot"`
	}
	s.call(t, "session.snapshot", nil, &result)
	found := ""
	for _, pane := range result.Snapshot.Panes {
		if pane.Label == label {
			if found != "" {
				t.Fatalf("label %q matches panes %s and %s; markers must be unique", label, found, pane.PaneID)
			}
			found = pane.PaneID
		}
	}
	if found == "" {
		t.Fatalf("no session.snapshot pane carries label %q", label)
	}
	return found
}

// snapshotIDs returns the sorted tab and pane ids session.snapshot reports.
func (s *testServer) snapshotIDs(t *testing.T) (tabs, panes []string) {
	t.Helper()
	var result struct {
		Snapshot struct {
			Tabs []struct {
				TabID string `json:"tab_id"`
			} `json:"tabs"`
			Panes []struct {
				PaneID string `json:"pane_id"`
			} `json:"panes"`
		} `json:"snapshot"`
	}
	s.call(t, "session.snapshot", nil, &result)
	for _, tab := range result.Snapshot.Tabs {
		tabs = append(tabs, tab.TabID)
	}
	for _, pane := range result.Snapshot.Panes {
		panes = append(panes, pane.PaneID)
	}
	sort.Strings(tabs)
	sort.Strings(panes)
	return tabs, panes
}

// renderIDs formats snapshot ids for evidence files.
func renderIDs(tabs, panes []string) string {
	return fmt.Sprintf("tabs: %s\npanes: %s\n", strings.Join(tabs, " "), strings.Join(panes, " "))
}

// containsString reports whether the sorted slice contains the value.
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// drainEventNames reads raw events until the named event arrives (bounded),
// then keeps draining briefly and returns every observed event as
// "name: payload" lines for evidence. Pushed event names use underscores
// (pane_exited) while subscriptions use dots (pane.exited); the stream also
// ends once the watched pane is gone, which after the wanted event is a
// normal outcome, not a failure.
func drainEventNames(t *testing.T, stream *herdr.EventStream, until string) string {
	t.Helper()
	normalized := strings.ReplaceAll(until, ".", "_")
	var out strings.Builder
	deadline := timeAfterConditionTimeout()
	seen := false
	for !seen {
		select {
		case event, ok := <-stream.Events():
			if !ok {
				t.Fatalf("event stream ended before %s: %v\n%s", until, stream.Err(), out.String())
			}
			fmt.Fprintf(&out, "%s: %s\n", event.Name, string(event.Data))
			if strings.ReplaceAll(event.Name, ".", "_") == normalized {
				seen = true
			}
		case <-deadline:
			t.Fatalf("no %s event before the deadline; observed:\n%s", until, out.String())
		}
	}
	// Drain whatever arrived with the exit without waiting for more.
	for {
		select {
		case event, ok := <-stream.Events():
			if !ok {
				return out.String()
			}
			fmt.Fprintf(&out, "%s: %s\n", event.Name, string(event.Data))
		case <-time.After(100 * time.Millisecond):
			return out.String()
		}
	}
}

// paneExists reports whether pane.list still names the pane.
func (s *testServer) paneExists(t *testing.T, paneID string) bool {
	t.Helper()
	var result struct {
		Panes []struct {
			PaneID string `json:"pane_id"`
		} `json:"panes"`
	}
	s.call(t, "pane.list", nil, &result)
	for _, pane := range result.Panes {
		if pane.PaneID == paneID {
			return true
		}
	}
	return false
}
