package herdr

import (
	"context"
	"fmt"

	"github.com/johnlanda/hop/internal/app"
)

var _ app.WorkspaceRuntime = (*Runtime)(nil)

// workspaceCreateParams is the wire shape of workspace.create: an explicit
// cwd, additive env and a unique creation label, with focus always false so
// creating the manager's workspace never steals the session's active
// workspace -- except the session's very first-ever workspace, which Herdr
// always activates regardless of the request (S8-confirmed). label and env
// are schema-optional on the request (an absent label gets an
// automatically-assigned one); this adapter always supplies a label, but
// omits the field rather than sending an empty string when the caller
// didn't set one.
type workspaceCreateParams struct {
	Cwd   string            `json:"cwd"`
	Focus bool              `json:"focus"`
	Label string            `json:"label,omitempty"`
	Env   map[string]string `json:"env,omitempty"`
}

// workspaceCreatedResult is the workspace.create result: the created
// workspace, its sole tab and that tab's sole root pane (S8-confirmed shape
// {type: "workspace_created", workspace, tab, root_pane}). WorkspaceID,
// TabID and the root pane's PaneID are all required: a successful
// workspace.create always names every one of them.
type workspaceCreatedResult struct {
	Type      string `json:"type"`
	Workspace struct {
		WorkspaceID *string `json:"workspace_id"`
	} `json:"workspace"`
	Tab struct {
		TabID *string `json:"tab_id"`
	} `json:"tab"`
	RootPane struct {
		PaneID *string `json:"pane_id"`
	} `json:"root_pane"`
}

// CreateWorkspace calls workspace.create with an explicit cwd, additive env
// and a unique creation label, never requesting focus, and reports the
// created workspace, tab and root pane ids.
func (r *Runtime) CreateWorkspace(ctx context.Context, req app.WorkspaceRequest) (app.WorkspaceHandle, error) {
	params := workspaceCreateParams{Cwd: req.Cwd, Focus: false, Label: req.Label, Env: req.Env}
	var result workspaceCreatedResult
	if err := r.client.Call(ctx, "workspace.create", params, &result); err != nil {
		return app.WorkspaceHandle{}, fmt.Errorf("create workspace labeled %q: %w", req.Label, err)
	}
	if result.Type != "workspace_created" {
		return app.WorkspaceHandle{}, protocolErrorf("workspace.create result type %q, want workspace_created", result.Type)
	}
	workspaceID, err := requireString("workspace.create", result.Workspace.WorkspaceID, "workspace.workspace_id")
	if err != nil {
		return app.WorkspaceHandle{}, err
	}
	tabID, err := requireString("workspace.create", result.Tab.TabID, "tab.tab_id")
	if err != nil {
		return app.WorkspaceHandle{}, err
	}
	paneID, err := requireString("workspace.create", result.RootPane.PaneID, "root_pane.pane_id")
	if err != nil {
		return app.WorkspaceHandle{}, err
	}
	return app.WorkspaceHandle{WorkspaceID: workspaceID, TabID: tabID, PaneID: paneID}, nil
}

// snapshotWorkspaceByLabel is the subset of a session.snapshot workspace
// record FindWorkspaceByLabel filters on. Label is a pointer even though
// WorkspaceInfo.label always has a value (Herdr assigns every workspace
// one, custom or automatic): it is the field EVERY workspace record is
// filtered on, so its absence on any record -- not only a matched one -- is
// a protocol violation, never a legitimate empty string. Decoding it as a
// plain string would let an absent label decode as "" and an empty lookup
// label match a malformed workspace by accident.
type snapshotWorkspaceByLabel struct {
	WorkspaceID *string `json:"workspace_id"`
	Label       *string `json:"label"`
}

// snapshotTabByWorkspace is the subset of a session.snapshot tab record used
// to descend from a resolved workspace to its sole tab. WorkspaceID is the
// field every tab record is filtered on, so its absence on any record is a
// protocol violation for the same reason snapshotWorkspaceByLabel.Label's
// is.
type snapshotTabByWorkspace struct {
	TabID       *string `json:"tab_id"`
	WorkspaceID *string `json:"workspace_id"`
}

// snapshotPaneByTab is the subset of a session.snapshot pane record used to
// descend from a resolved tab to its sole pane. TabID is the field every
// pane record is filtered on, so its absence on any record is a protocol
// violation for the same reason.
type snapshotPaneByTab struct {
	PaneID *string `json:"pane_id"`
	TabID  *string `json:"tab_id"`
}

// findWorkspaceByLabelResult is the session.snapshot result reduced to its
// workspace, tab and pane records. Each list is a pointer for the same
// reason findPaneByLabelResult's Panes field is: workspaces, tabs and panes
// are all schema-required on the snapshot, so an absent key is a protocol
// violation distinct from the legitimate "none yet" empty array.
type findWorkspaceByLabelResult struct {
	Type     string `json:"type"`
	Snapshot *struct {
		Workspaces *[]snapshotWorkspaceByLabel `json:"workspaces"`
		Tabs       *[]snapshotTabByWorkspace   `json:"tabs"`
		Panes      *[]snapshotPaneByTab        `json:"panes"`
	} `json:"snapshot"`
}

// FindWorkspaceByLabel resolves a workspace by its unique creation LABEL --
// a WORKSPACE attribute (WorkspaceInfo.label), never a pane's -- then
// descends workspace -> its sole tab -> that tab's sole pane, all from one
// session.snapshot call. label must be non-empty: Herdr never assigns an
// empty label, so an empty search label could never legitimately match and
// is rejected before any request is sent, rather than silently returning
// not-found or risking a match against a malformed record. Zero matching
// workspaces is (zero, false, nil); more than one workspace carrying the
// label, or more than one tab or pane at any descent step (including
// zero -- a workspace Herdr created always has both), is an error, never a
// guess. Every schema-required field this method filters on -- a
// workspace's label, a tab's workspace_id, a pane's tab_id -- is validated
// on EVERY record it scans, not only the eventual match: these fields are
// read to decide what matches, so an absent one anywhere is a protocol
// violation, never a silently-skipped non-match.
func (r *Runtime) FindWorkspaceByLabel(ctx context.Context, label string) (app.WorkspaceRef, bool, error) {
	if label == "" {
		return app.WorkspaceRef{}, false, fmt.Errorf("find workspace by label: label must not be empty")
	}
	var result findWorkspaceByLabelResult
	if err := r.client.Call(ctx, "session.snapshot", nil, &result); err != nil {
		return app.WorkspaceRef{}, false, fmt.Errorf("find workspace by label %q: %w", label, err)
	}
	if result.Type != "session_snapshot" {
		return app.WorkspaceRef{}, false, protocolErrorf("session.snapshot result type %q, want session_snapshot", result.Type)
	}
	if result.Snapshot == nil {
		return app.WorkspaceRef{}, false, protocolErrorf("session.snapshot result missing required field %q", "snapshot")
	}
	if result.Snapshot.Workspaces == nil {
		return app.WorkspaceRef{}, false, protocolErrorf("session.snapshot result missing required field %q", "snapshot.workspaces")
	}
	if result.Snapshot.Tabs == nil {
		return app.WorkspaceRef{}, false, protocolErrorf("session.snapshot result missing required field %q", "snapshot.tabs")
	}
	if result.Snapshot.Panes == nil {
		return app.WorkspaceRef{}, false, protocolErrorf("session.snapshot result missing required field %q", "snapshot.panes")
	}

	var foundWorkspace snapshotWorkspaceByLabel
	workspaceMatches := 0
	for i, ws := range *result.Snapshot.Workspaces {
		wsLabel, err := requireString("session.snapshot", ws.Label, fmt.Sprintf("workspaces[%d].label", i))
		if err != nil {
			return app.WorkspaceRef{}, false, err
		}
		if wsLabel == label {
			workspaceMatches++
			foundWorkspace = ws
		}
	}
	switch workspaceMatches {
	case 0:
		return app.WorkspaceRef{}, false, nil
	case 1:
		// Exactly one match; descend below.
	default:
		return app.WorkspaceRef{}, false, fmt.Errorf("find workspace by label %q: %d workspaces carry this label, want at most one", label, workspaceMatches)
	}
	workspaceID, err := requireString("session.snapshot", foundWorkspace.WorkspaceID, "workspaces[].workspace_id")
	if err != nil {
		return app.WorkspaceRef{}, false, err
	}

	var foundTab snapshotTabByWorkspace
	tabMatches := 0
	for i, tab := range *result.Snapshot.Tabs {
		tabWorkspaceID, scanErr := requireString("session.snapshot", tab.WorkspaceID, fmt.Sprintf("tabs[%d].workspace_id", i))
		if scanErr != nil {
			return app.WorkspaceRef{}, false, scanErr
		}
		if tabWorkspaceID == workspaceID {
			tabMatches++
			foundTab = tab
		}
	}
	if tabMatches != 1 {
		return app.WorkspaceRef{}, false, fmt.Errorf("find workspace by label %q: workspace %s has %d tabs, want exactly 1", label, workspaceID, tabMatches)
	}
	tabID, err := requireString("session.snapshot", foundTab.TabID, "tabs[].tab_id")
	if err != nil {
		return app.WorkspaceRef{}, false, err
	}

	var foundPane snapshotPaneByTab
	paneMatches := 0
	for i, pane := range *result.Snapshot.Panes {
		paneTabID, scanErr := requireString("session.snapshot", pane.TabID, fmt.Sprintf("panes[%d].tab_id", i))
		if scanErr != nil {
			return app.WorkspaceRef{}, false, scanErr
		}
		if paneTabID == tabID {
			paneMatches++
			foundPane = pane
		}
	}
	if paneMatches != 1 {
		return app.WorkspaceRef{}, false, fmt.Errorf("find workspace by label %q: tab %s has %d panes, want exactly 1", label, tabID, paneMatches)
	}
	paneID, err := requireString("session.snapshot", foundPane.PaneID, "panes[].pane_id")
	if err != nil {
		return app.WorkspaceRef{}, false, err
	}

	return app.WorkspaceRef{WorkspaceID: workspaceID, TabID: tabID, PaneID: paneID}, true, nil
}
