package herdr

import (
	"context"
	"errors"
	"fmt"

	"github.com/johnlanda/hop/internal/app"
)

// ErrWorkspaceIDRequired reports that OpenWorkerPane was called with no
// WorkspaceID. layout.apply has no "create a new workspace" mode of its
// own — omitting workspace_id addresses whichever workspace happens to be
// active, never a fresh one — so a caller must supply the workspace
// CreateWorktree already established.
var ErrWorkspaceIDRequired = errors.New("herdr: workspace id is required")

// ErrPaneNotFound reports Herdr's pane_not_found API error for a pane
// address. For InspectPane specifically, Herdr returns this same code when
// the pane exists but currently has no live runtime attached (for example
// during the delayed-restore window before Herdr respawns its shell), so
// this means "no such pane, or no runtime yet" rather than strictly "no
// such pane." errors.Is distinguishes it from every other adapter failure.
var ErrPaneNotFound = errors.New("herdr: pane not found")

// Runtime implements app.Runtime over one server socket: worktree
// creation, worker-pane creation, pane recovery by creation label, the
// send-text fallback transport, scrollback reads, occupant inspection and
// pane closure.
type Runtime struct {
	client *Client
}

var _ app.Runtime = (*Runtime)(nil)

// NewRuntime builds the runtime adapter for a server socket.
func NewRuntime(socketPath string) *Runtime {
	return &Runtime{client: NewClient(socketPath)}
}

// worktreeCreateParams is the wire shape of worktree.create. It carries no
// env: the launch environment is set by opening a pane inside the created
// worktree, not by this call (docs/architecture/launch-environment.md).
type worktreeCreateParams struct {
	Cwd    string `json:"cwd"`
	Branch string `json:"branch"`
	Base   string `json:"base"`
}

// worktreeCreatedResult is the worktree.create result: the workspace Herdr
// opened the checkout into, and the checkout's own path and branch.
type worktreeCreatedResult struct {
	Workspace struct {
		WorkspaceID string `json:"workspace_id"`
	} `json:"workspace"`
	Worktree struct {
		Path   string `json:"path"`
		Branch string `json:"branch"`
	} `json:"worktree"`
}

// CreateWorktree calls worktree.create and reports the workspace, checkout
// path and branch it created. Herdr's response carries no base commit;
// that provenance is resolved by application code through CommandRunner,
// not by this adapter.
func (r *Runtime) CreateWorktree(ctx context.Context, req app.WorktreeRequest) (app.WorktreeInfo, error) {
	params := worktreeCreateParams{Cwd: req.RepositoryRoot, Branch: req.Branch, Base: req.BaseRef}
	var result worktreeCreatedResult
	if err := r.client.Call(ctx, "worktree.create", params, &result); err != nil {
		return app.WorktreeInfo{}, fmt.Errorf("create worktree for %s branch %s: %w", req.RepositoryRoot, req.Branch, err)
	}
	return app.WorktreeInfo{
		WorkspaceID: result.Workspace.WorkspaceID,
		Path:        result.Worktree.Path,
		Branch:      result.Worktree.Branch,
	}, nil
}

// workerPaneParams is the wire shape of layout.apply for OpenWorkerPane: a
// single pane node added to an existing workspace. tab_id is never sent —
// naming one would replace that tab instead of adding a new one (verified
// by the Phase 2 capability spike) — and focus is always false so opening a
// worker pane never steals the user's attention.
type workerPaneParams struct {
	WorkspaceID string         `json:"workspace_id"`
	Focus       bool           `json:"focus"`
	Root        workerPaneNode `json:"root"`
}

// workerPaneNode is the layout.apply pane node: the command argv IS the
// pane's process (no shell), with its additive env, cwd and creation label.
type workerPaneNode struct {
	Type    string            `json:"type"`
	Label   string            `json:"label,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Command []string          `json:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// workerPaneResult is the layout.apply result: the workspace and tab it
// applied to, and the root pane node it created.
type workerPaneResult struct {
	Layout struct {
		WorkspaceID string `json:"workspace_id"`
		TabID       string `json:"tab_id"`
		Root        struct {
			PaneID string `json:"pane_id"`
		} `json:"root"`
	} `json:"layout"`
}

// OpenWorkerPane calls layout.apply to add exactly one tab, containing one
// pane node whose command is the launch argv, to req.WorkspaceID — leaving
// every pre-existing tab and pane in that workspace untouched. WorkspaceID
// is required: layout.apply has no way to create a new workspace on its
// own, so an empty WorkspaceID fails with ErrWorkspaceIDRequired before any
// request is sent.
func (r *Runtime) OpenWorkerPane(ctx context.Context, req app.WorkerPaneRequest) (app.PaneHandle, error) { //nolint:gocritic // hugeParam: req's shape is fixed by the app.Runtime port signature this method implements; OpenWorkerPane runs once per launch, never a hot loop.
	if req.WorkspaceID == "" {
		return app.PaneHandle{}, ErrWorkspaceIDRequired
	}
	params := workerPaneParams{
		WorkspaceID: req.WorkspaceID,
		Focus:       false,
		Root: workerPaneNode{
			Type:    "pane",
			Label:   req.Label,
			Cwd:     req.Cwd,
			Command: req.Command,
			Env:     req.Env,
		},
	}
	var result workerPaneResult
	if err := r.client.Call(ctx, "layout.apply", params, &result); err != nil {
		return app.PaneHandle{}, fmt.Errorf("open worker pane in workspace %s: %w", req.WorkspaceID, err)
	}
	return app.PaneHandle{
		WorkspaceID: result.Layout.WorkspaceID,
		TabID:       result.Layout.TabID,
		PaneID:      result.Layout.Root.PaneID,
	}, nil
}

// snapshotPaneByLabel is the subset of a session.snapshot pane record
// FindPaneByLabel matches on.
type snapshotPaneByLabel struct {
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	Label       string `json:"label"`
}

// findPaneByLabelResult is the session.snapshot result reduced to its pane
// records.
type findPaneByLabelResult struct {
	Snapshot struct {
		Panes []snapshotPaneByLabel `json:"panes"`
	} `json:"snapshot"`
}

// FindPaneByLabel recovers a pane OpenWorkerPane created when its create
// response was lost. It reads session.snapshot only: OpenWorkerPane's label
// is a layout.apply pane-node label, not a tab.create tab label, and
// session.snapshot's pane records already carry the workspace, tab and pane
// ids alongside it, so tab.list (tab-level labels only) adds nothing here.
// Not found is the zero PaneRef, false, nil. Two or more panes carrying the
// label is an error: creation labels must be unique.
func (r *Runtime) FindPaneByLabel(ctx context.Context, label string) (app.PaneRef, bool, error) {
	var result findPaneByLabelResult
	if err := r.client.Call(ctx, "session.snapshot", nil, &result); err != nil {
		return app.PaneRef{}, false, fmt.Errorf("find pane by label %q: %w", label, err)
	}
	var found snapshotPaneByLabel
	matches := 0
	for _, pane := range result.Snapshot.Panes {
		if pane.Label == label {
			matches++
			found = pane
		}
	}
	switch matches {
	case 0:
		return app.PaneRef{}, false, nil
	case 1:
		return app.PaneRef{WorkspaceID: found.WorkspaceID, TabID: found.TabID, PaneID: found.PaneID}, true, nil
	default:
		return app.PaneRef{}, false, fmt.Errorf("find pane by label %q: %d panes carry this label, want at most one", label, matches)
	}
}

// sendTextParams is the wire shape of pane.send_text.
type sendTextParams struct {
	PaneID string `json:"pane_id"`
	Text   string `json:"text"`
}

// SendText sends one line of text into a pane's foreground process. It is
// the fallback transport only: the fixed-grammar launch line
// (docs/plan/phase-2-design.md section 6). It is never used for anything
// else.
func (r *Runtime) SendText(ctx context.Context, paneID, text string) error {
	err := r.client.Call(ctx, "pane.send_text", sendTextParams{PaneID: paneID, Text: text}, nil)
	return wrapPaneError("send text to", paneID, err)
}

// readPaneParams is the wire shape of pane.read for scrollback capture:
// the "recent" source (scrollback, not just the visible viewport) as plain
// text with ANSI stripped (strip_ansi's server default), bounded to lines
// rows when positive. A non-positive lines omits the field, so the server
// applies its own default window.
type readPaneParams struct {
	PaneID string `json:"pane_id"`
	Source string `json:"source"`
	Lines  int    `json:"lines,omitempty"`
}

// readPaneResult is the pane.read result, reduced to its text.
type readPaneResult struct {
	Read struct {
		Text string `json:"text"`
	} `json:"read"`
}

// ReadPane captures scrollback evidence. It vanishes with the pane, so
// callers capture it periodically and before every stop or retirement
// action.
func (r *Runtime) ReadPane(ctx context.Context, paneID string, lines int) (string, error) {
	params := readPaneParams{PaneID: paneID, Source: "recent", Lines: lines}
	var result readPaneResult
	if err := r.client.Call(ctx, "pane.read", params, &result); err != nil {
		return "", wrapPaneError("read", paneID, err)
	}
	return result.Read.Text, nil
}

// processInfoParams is the wire shape of pane.process_info.
type processInfoParams struct {
	PaneID string `json:"pane_id"`
}

// processInfoResult is the pane.process_info result: the pane's shell pid,
// foreground process group id and the foreground processes themselves —
// the complete S2-verified field set. There is no process start time
// anywhere in this surface.
type processInfoResult struct {
	ProcessInfo struct {
		ShellPID                 int                  `json:"shell_pid"`
		ForegroundProcessGroupID int                  `json:"foreground_process_group_id"`
		ForegroundProcesses      []processInfoProcess `json:"foreground_processes"`
	} `json:"process_info"`
}

// processInfoProcess is one foreground process record.
type processInfoProcess struct {
	PID     int      `json:"pid"`
	Name    string   `json:"name"`
	Argv0   string   `json:"argv0"`
	Argv    []string `json:"argv"`
	Cmdline string   `json:"cmdline"`
	Cwd     string   `json:"cwd"`
}

// InspectPane returns the occupant identity used by every launch, stop and
// adoption decision. Herdr's pane_not_found error from this method means
// either no such pane, or a pane that exists but has no live runtime yet;
// ErrPaneNotFound covers both so the caller can classify "no runtime"
// apart from every other failure.
func (r *Runtime) InspectPane(ctx context.Context, paneID string) (app.PaneProcess, error) {
	var result processInfoResult
	if err := r.client.Call(ctx, "pane.process_info", processInfoParams{PaneID: paneID}, &result); err != nil {
		return app.PaneProcess{}, wrapPaneError("inspect", paneID, err)
	}
	foreground := make([]app.ProcessInfo, 0, len(result.ProcessInfo.ForegroundProcesses))
	for _, p := range result.ProcessInfo.ForegroundProcesses {
		foreground = append(foreground, app.ProcessInfo{
			PID:     p.PID,
			Name:    p.Name,
			Argv0:   p.Argv0,
			Argv:    p.Argv,
			Cmdline: p.Cmdline,
			Cwd:     p.Cwd,
		})
	}
	return app.PaneProcess{
		ShellPID:          result.ProcessInfo.ShellPID,
		ForegroundGroupID: result.ProcessInfo.ForegroundProcessGroupID,
		Foreground:        foreground,
	}, nil
}

// closePaneParams is the wire shape of pane.close: pane_id only. There is
// no occupant-conditioned or compare-and-swap close upstream (S2); the
// close rule that guards against closing the wrong occupant is enforced by
// application code immediately before calling this method, not by the
// port or this adapter.
type closePaneParams struct {
	PaneID string `json:"pane_id"`
}

// ClosePane requests pane closure. Callers apply the close rule (re-inspect
// and match occupant evidence immediately before calling this) themselves;
// this method only issues the request.
func (r *Runtime) ClosePane(ctx context.Context, paneID string) error {
	err := r.client.Call(ctx, "pane.close", closePaneParams{PaneID: paneID}, nil)
	return wrapPaneError("close", paneID, err)
}

// wrapPaneError adds pane-address context to a pane call's error, mapping
// Herdr's pane_not_found code to the typed, errors.Is-checkable
// ErrPaneNotFound; every other error keeps its own type (an APIError stays
// an APIError, a ProtocolError stays a ProtocolError) under the added
// context. A nil err returns nil.
func wrapPaneError(action, paneID string, err error) error {
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == "pane_not_found" {
		return fmt.Errorf("%s pane %s: %w", action, paneID, ErrPaneNotFound)
	}
	return fmt.Errorf("%s pane %s: %w", action, paneID, err)
}
