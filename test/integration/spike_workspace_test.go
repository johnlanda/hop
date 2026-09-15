package integration

import (
	"testing"
)

// TestSpikeWorkspaceCreateResponseShapeAndEnv is spike item S8:
// workspace.create's response carries the same {workspace, tab, root_pane}
// triple as tab.create/worktree.create (repos/herdr/src/api/schema/response.rs
// ResponseResult::WorkspaceCreated), with the root pane's ids consistently
// cross-referencing the workspace and tab ids. Unlike layout.apply (S6) and
// tab.create (S7), workspace.create's own "label" parameter names the
// WORKSPACE, not the pane: it lands on WorkspaceInfo.label
// (Workspace::set_custom_name), and the root pane returned alongside it
// carries no pane-level label of its own. The requested cwd and additive env
// reach the root pane's login shell exactly as tab.create's do (S7): there is
// no "command" field on workspace.create, so the pane runs a shell, not an
// argv process.
func TestSpikeWorkspaceCreateResponseShapeAndEnv(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)

	runID := newSpikeUUID(t)
	marker := "hop-spike-ws-" + runID[:8]
	var created workspaceCreatedResponse
	server.call(t, "workspace.create", map[string]any{
		"cwd":   server.workDir(),
		"focus": true,
		"label": marker,
		"env": map[string]string{
			"HOP_RUN_ID": runID,
		},
	}, &created)

	if created.Type != "workspace_created" {
		t.Errorf("workspace.create result type = %q, want workspace_created", created.Type)
	}
	if created.Workspace.WorkspaceID == "" {
		t.Fatal("workspace.create returned no workspace_id")
	}
	if created.Tab.TabID == "" {
		t.Fatal("workspace.create returned no tab_id")
	}
	if created.RootPane.PaneID == "" {
		t.Fatal("workspace.create returned no root pane id")
	}
	if created.Tab.WorkspaceID != created.Workspace.WorkspaceID {
		t.Errorf("tab.workspace_id = %q, want the created workspace %q", created.Tab.WorkspaceID, created.Workspace.WorkspaceID)
	}
	if created.RootPane.WorkspaceID != created.Workspace.WorkspaceID {
		t.Errorf("root_pane.workspace_id = %q, want %q", created.RootPane.WorkspaceID, created.Workspace.WorkspaceID)
	}
	if created.RootPane.TabID != created.Tab.TabID {
		t.Errorf("root_pane.tab_id = %q, want the created tab %q", created.RootPane.TabID, created.Tab.TabID)
	}
	if created.Workspace.TabCount != 1 || created.Workspace.PaneCount != 1 {
		t.Errorf("fresh workspace tab_count=%d pane_count=%d, want exactly 1 and 1",
			created.Workspace.TabCount, created.Workspace.PaneCount)
	}

	// workspace.create's label names the WORKSPACE (round-trips on
	// WorkspaceInfo.label), not the root pane: the response's own root pane
	// carries no label field, in contrast to a layout.apply pane node (S6).
	if created.Workspace.Label != marker {
		t.Errorf("workspace.label = %q, want the requested creation label %q", created.Workspace.Label, marker)
	}
	if created.RootPane.Label != nil {
		t.Errorf("workspace.create's root pane carries label %q; expected no pane-level label from this request", *created.RootPane.Label)
	}
	if !samePath(t, created.rootPaneCwd(t), server.workDir()) {
		t.Errorf("root_pane.cwd = %q, want the requested cwd %q", created.rootPaneCwd(t), server.workDir())
	}

	// cwd and additive env reach the root pane's login shell: there is no
	// command argv on workspace.create, so this is a shell process. Additive
	// env delivery is proven by printed output (S7's technique, a short
	// marker); the shell's own live cwd is read back through
	// pane.process_info (S6's technique) rather than printed, since this
	// test's absolute temp-root path is long enough to hard-wrap at the
	// pane's 100-column width (testConfig) and split the very substring a
	// printed-text match would look for.
	server.call(t, "pane.send_text", map[string]any{
		"pane_id": created.RootPane.PaneID,
		"text":    "printf 'SPIKE-WS-RUNID=[%s]\\n' \"$HOP_RUN_ID\"\n",
	}, nil)
	server.waitForPaneText(t, created.RootPane.PaneID, "SPIKE-WS-RUNID=["+runID+"]")
	shellInfo := server.processInfo(t, created.RootPane.PaneID)
	if len(shellInfo.ForegroundProcesses) != 1 {
		t.Fatalf("root pane has %d foreground processes, want exactly the login shell", len(shellInfo.ForegroundProcesses))
	}
	if !samePath(t, shellInfo.ForegroundProcesses[0].Cwd, server.workDir()) {
		t.Errorf("root pane shell process cwd = %q, want %q", shellInfo.ForegroundProcesses[0].Cwd, server.workDir())
	}

	// Creation-label round trip through session.snapshot (S7's recovery
	// pattern, one level up: workspace -> its single tab -> that tab's single
	// pane), discarding this test's own create response ids to model a
	// controller that crashed before recording them.
	recoveredWorkspace := server.snapshotWorkspaceByLabel(t, marker)
	if recoveredWorkspace != created.Workspace.WorkspaceID {
		t.Errorf("session.snapshot recovers workspace %q by label %q, want %q", recoveredWorkspace, marker, created.Workspace.WorkspaceID)
	}
	recoveredTab := server.snapshotSoleTabForWorkspace(t, recoveredWorkspace)
	if recoveredTab != created.Tab.TabID {
		t.Errorf("session.snapshot's sole tab for workspace %q is %q, want %q", recoveredWorkspace, recoveredTab, created.Tab.TabID)
	}
	recoveredPane := server.snapshotSolePaneForTab(t, recoveredTab)
	if recoveredPane != created.RootPane.PaneID {
		t.Errorf("session.snapshot's sole pane for tab %q is %q, want the root pane %q", recoveredTab, recoveredPane, created.RootPane.PaneID)
	}
}

// TestSpikeWorkspaceCreateNoFocus is the S8 follow-up: workspace.create's
// "focus" parameter is honored both ways. The very first workspace on a
// session becomes active regardless of the request (Herdr falls back to
// activating it whenever no workspace is active yet -- create_workspace_with_
// launch_env's `focus || self.state.active.is_none()`), so this test
// establishes that baseline explicitly with focus:true and then proves a
// SECOND workspace created with focus:false neither reports itself focused
// nor steals the session's active workspace away from the first.
func TestSpikeWorkspaceCreateNoFocus(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)

	var first workspaceCreatedResponse
	server.call(t, "workspace.create", map[string]any{"cwd": server.workDir(), "focus": true}, &first)
	if !first.Workspace.Focused {
		t.Fatalf("baseline workspace %q was not reported focused", first.Workspace.WorkspaceID)
	}
	before := server.focusedWorkspaceID(t)
	if before != first.Workspace.WorkspaceID {
		t.Fatalf("session.snapshot.focused_workspace_id = %q, want the baseline workspace %q", before, first.Workspace.WorkspaceID)
	}

	var second workspaceCreatedResponse
	server.call(t, "workspace.create", map[string]any{"cwd": server.workDir(), "focus": false}, &second)
	if second.Workspace.Focused {
		t.Errorf("workspace created with focus:false reports focused=true")
	}

	after := server.focusedWorkspaceID(t)
	if after != before {
		t.Errorf("session.snapshot.focused_workspace_id changed from %q to %q after a focus:false workspace.create", before, after)
	}
	if !server.workspaceIsFocused(t, first.Workspace.WorkspaceID) {
		t.Errorf("baseline workspace %q no longer reports focused=true after a focus:false workspace.create", first.Workspace.WorkspaceID)
	}
}

// workspaceCreatedResponse is the workspace.create result reduced to the
// fields this spike asserts on. Label is a plain string (WorkspaceInfo.label
// always has a value -- an automatic one when no custom name was set); the
// root pane's Label is a pointer because PaneInfo.label is schema-optional
// and this probe specifically asserts its ABSENCE for a workspace.create root
// pane.
type workspaceCreatedResponse struct {
	Type      string `json:"type"`
	Workspace struct {
		WorkspaceID string `json:"workspace_id"`
		Label       string `json:"label"`
		Focused     bool   `json:"focused"`
		TabCount    int    `json:"tab_count"`
		PaneCount   int    `json:"pane_count"`
	} `json:"workspace"`
	Tab struct {
		TabID       string `json:"tab_id"`
		WorkspaceID string `json:"workspace_id"`
	} `json:"tab"`
	RootPane struct {
		PaneID      string  `json:"pane_id"`
		WorkspaceID string  `json:"workspace_id"`
		TabID       string  `json:"tab_id"`
		Cwd         *string `json:"cwd"`
		Label       *string `json:"label"`
	} `json:"root_pane"`
}

// Cwd unwraps the root pane's optional cwd field for samePath, failing
// clearly if it was absent rather than comparing against a zero value.
func (r *workspaceCreatedResponse) rootPaneCwd(t *testing.T) string {
	t.Helper()
	if r.RootPane.Cwd == nil {
		t.Fatal("workspace.create root pane carries no cwd")
	}
	return *r.RootPane.Cwd
}

// snapshotWorkspaceByLabel returns the workspace id of the single
// session.snapshot workspace carrying the label, failing if none or several
// match.
func (s *testServer) snapshotWorkspaceByLabel(t *testing.T, label string) string {
	t.Helper()
	var result struct {
		Snapshot struct {
			Workspaces []struct {
				WorkspaceID string `json:"workspace_id"`
				Label       string `json:"label"`
			} `json:"workspaces"`
		} `json:"snapshot"`
	}
	s.call(t, "session.snapshot", nil, &result)
	found := ""
	for _, ws := range result.Snapshot.Workspaces {
		if ws.Label == label {
			if found != "" {
				t.Fatalf("label %q matches workspaces %s and %s; markers must be unique", label, found, ws.WorkspaceID)
			}
			found = ws.WorkspaceID
		}
	}
	if found == "" {
		t.Fatalf("no session.snapshot workspace carries label %q", label)
	}
	return found
}

// snapshotSoleTabForWorkspace returns the single session.snapshot tab id
// belonging to workspaceID, failing if there is not exactly one.
func (s *testServer) snapshotSoleTabForWorkspace(t *testing.T, workspaceID string) string {
	t.Helper()
	var result struct {
		Snapshot struct {
			Tabs []struct {
				TabID       string `json:"tab_id"`
				WorkspaceID string `json:"workspace_id"`
			} `json:"tabs"`
		} `json:"snapshot"`
	}
	s.call(t, "session.snapshot", nil, &result)
	matches := make([]string, 0, 1)
	for _, tab := range result.Snapshot.Tabs {
		if tab.WorkspaceID == workspaceID {
			matches = append(matches, tab.TabID)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("workspace %s has %d tabs %v, want exactly 1", workspaceID, len(matches), matches)
	}
	return matches[0]
}

// snapshotSolePaneForTab returns the single session.snapshot pane id
// belonging to tabID, failing if there is not exactly one.
func (s *testServer) snapshotSolePaneForTab(t *testing.T, tabID string) string {
	t.Helper()
	var result struct {
		Snapshot struct {
			Panes []struct {
				PaneID string `json:"pane_id"`
				TabID  string `json:"tab_id"`
			} `json:"panes"`
		} `json:"snapshot"`
	}
	s.call(t, "session.snapshot", nil, &result)
	matches := make([]string, 0, 1)
	for _, pane := range result.Snapshot.Panes {
		if pane.TabID == tabID {
			matches = append(matches, pane.PaneID)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("tab %s has %d panes %v, want exactly 1", tabID, len(matches), matches)
	}
	return matches[0]
}

// focusedWorkspaceID returns session.snapshot's focused_workspace_id.
func (s *testServer) focusedWorkspaceID(t *testing.T) string {
	t.Helper()
	var result struct {
		Snapshot struct {
			FocusedWorkspaceID string `json:"focused_workspace_id"`
		} `json:"snapshot"`
	}
	s.call(t, "session.snapshot", nil, &result)
	return result.Snapshot.FocusedWorkspaceID
}

// workspaceIsFocused reports whether session.snapshot's own workspace record
// for workspaceID carries focused:true.
func (s *testServer) workspaceIsFocused(t *testing.T, workspaceID string) bool {
	t.Helper()
	var result struct {
		Snapshot struct {
			Workspaces []struct {
				WorkspaceID string `json:"workspace_id"`
				Focused     bool   `json:"focused"`
			} `json:"workspaces"`
		} `json:"snapshot"`
	}
	s.call(t, "session.snapshot", nil, &result)
	for _, ws := range result.Snapshot.Workspaces {
		if ws.WorkspaceID == workspaceID {
			return ws.Focused
		}
	}
	t.Fatalf("session.snapshot no longer lists workspace %s", workspaceID)
	return false
}
