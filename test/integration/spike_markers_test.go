package integration

import (
	"testing"
)

// TestSpikeCreationMarkerTabLabel is spike item S7: tab.create accepts a
// label chosen BEFORE creation, and both tab.list and session.snapshot
// report it back, so a controller that crashed between tab.create and
// recording the response can recognize its own tab — and from it the root
// pane — by a unique marker rather than by workspace/cwd matching. The
// recovery below deliberately never uses the create response's ids.
//
// The rest of S7 is settled by schema and by the other spike tests: pane
// creation via tab.create/pane.split/workspace.create takes no pane-level
// label or metadata (only layout.apply pane nodes do — see
// TestSpikeLayoutApplyCommandPane); session.snapshot pane records expose
// label and metadata tokens but never the creation-time additive env; and
// pane.process_info has no process start time field, so a recorded pid is
// only distinguishable from a reused pid through the process's argv identity
// (see TestSpikePaneProcessIdentity).
func TestSpikeCreationMarkerTabLabel(t *testing.T) {
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
	marker := "hop-spike-tab-" + runID[:8]
	// The marker is chosen before creation; the response is deliberately
	// discarded to model a controller that crashed before recording it. The
	// run id is retained so the rebind below can wait for output that the
	// injected command line does not itself contain.
	server.call(t, "tab.create", map[string]any{
		"workspace_id": workspace.Workspace.WorkspaceID,
		"label":        marker,
		"focus":        true,
		"env":          map[string]string{"HOP_RUN_ID": runID},
	}, &struct{}{})

	// Recovery path 1: tab.list finds exactly one tab by the marker.
	var tabs struct {
		Tabs []struct {
			TabID       string `json:"tab_id"`
			WorkspaceID string `json:"workspace_id"`
			Label       string `json:"label"`
		} `json:"tabs"`
	}
	server.call(t, "tab.list", nil, &tabs)
	recoveredTab := ""
	for _, tab := range tabs.Tabs {
		if tab.Label == marker {
			if recoveredTab != "" {
				t.Fatalf("label %q matches tabs %s and %s; markers must be unique", marker, recoveredTab, tab.TabID)
			}
			recoveredTab = tab.TabID
		}
	}
	if recoveredTab == "" {
		t.Fatalf("no tab carries the creation-time label %q; tabs: %+v", marker, tabs.Tabs)
	}

	// Recovery path 2: session.snapshot maps the recovered tab to its panes.
	var snapshot struct {
		Snapshot struct {
			Tabs []struct {
				TabID string `json:"tab_id"`
				Label string `json:"label"`
			} `json:"tabs"`
			Panes []struct {
				PaneID string `json:"pane_id"`
				TabID  string `json:"tab_id"`
			} `json:"panes"`
		} `json:"snapshot"`
	}
	server.call(t, "session.snapshot", nil, &snapshot)
	labeled := false
	for _, tab := range snapshot.Snapshot.Tabs {
		if tab.TabID == recoveredTab && tab.Label == marker {
			labeled = true
		}
	}
	if !labeled {
		t.Errorf("session.snapshot does not report label %q on tab %s", marker, recoveredTab)
	}
	panes := make([]string, 0, 1)
	for _, pane := range snapshot.Snapshot.Panes {
		if pane.TabID == recoveredTab {
			panes = append(panes, pane.PaneID)
		}
	}
	if len(panes) != 1 {
		t.Fatalf("recovered tab %s has %d panes %v, want exactly the root pane", recoveredTab, len(panes), panes)
	}

	// The recovered pane is live and addressable: the controller can now
	// rebind to it. Wait for RECOVERED=[<runID>], the expanded output; the
	// echoed command line contains RECOVERED=[%s] and $HOP_RUN_ID, never the
	// run id itself, so terminal echo cannot satisfy this wait — only the
	// shell actually running the printf can.
	server.call(t, "pane.send_text", map[string]any{
		"pane_id": panes[0],
		"text":    "printf 'RECOVERED=[%s]\\n' \"$HOP_RUN_ID\"\n",
	}, nil)
	server.waitForPaneText(t, panes[0], "RECOVERED=["+runID+"]")
	artifacts.save(t, "recovered-pane.txt", server.readPane(t, panes[0]))
}
