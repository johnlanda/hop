package app

import "context"

// WorktreeRequest is CreateWorktree's input: the repository to check out
// from, the branch to create and the base ref to create it at.
type WorktreeRequest struct {
	RepositoryRoot string
	Branch         string
	BaseRef        string
}

// WorktreeInfo is what Herdr created: the workspace it lives in, the
// absolute checkout path and the branch. Herdr's worktree.create response
// carries no commit — the Herdr adapter runs no git — so the base commit an
// adoption decision validates against (docs/plan/phase-2-design.md
// section 4) is resolved separately, through CommandRunner. The worker
// pane is opened inside WorkspaceID so it shares the worktree's workspace
// rather than creating a second one.
type WorktreeInfo struct {
	WorkspaceID string
	Path        string
	Branch      string
}

// WorkerPaneRequest is OpenWorkerPane's input: a layout.apply pane node
// whose command IS the launch argv (absolute executable path first), the
// worktree cwd, the additive env map and a unique creation label (the
// launch operation ID). WorkspaceID is required: it is the workspace
// Runtime.CreateWorktree already created, so the pane joins it as one
// added tab, leaving every other tab and pane in that workspace untouched.
type WorkerPaneRequest struct {
	WorkspaceID string
	Cwd         string
	Command     []string
	Env         map[string]string
	Label       string
}

// PaneHandle identifies a pane Herdr created: the workspace, tab and pane
// IDs a runtime binding records.
type PaneHandle struct {
	WorkspaceID string
	TabID       string
	PaneID      string
}

// PaneRef identifies a pane FindPaneByLabel recovered.
type PaneRef struct {
	WorkspaceID string
	TabID       string
	PaneID      string
}

// ProcessInfo is one process observed in a pane's foreground group: the
// S2-verified per-process fields. There is no process start time anywhere
// in this surface.
type ProcessInfo struct {
	PID     int
	Name    string
	Argv0   string
	Argv    []string
	Cmdline string
	Cwd     string
}

// PaneProcess is one InspectPane observation: the pane's shell pid, its
// foreground process group id, and the foreground processes themselves
// (ordinarily one; a forking-wrapper topology reports more than the
// launched harness alone, which the section 6 corroboration predicate
// treats as unsupported). An empty Foreground means no foreground process
// was observed.
type PaneProcess struct {
	ShellPID          int
	ForegroundGroupID int
	Foreground        []ProcessInfo
}

// Runtime is the consumer-owned port over Herdr's pane and worktree
// surface. The Herdr adapter (internal/adapters/herdr) implements it.
//
// Close rule, used identically by stop, retirement and recovery:
// immediately before ClosePane, the caller re-inspects and matches the
// occupant's argv and label against the recorded evidence for that specific
// target, and on mismatch, missing identity or inspection failure fails
// closed (no close, operation reconciling). This rule is enforced by
// application code around ClosePane, not by the port itself.
type Runtime interface {
	// CreateWorktree calls worktree.create; it carries no env (see
	// docs/architecture/launch-environment.md).
	CreateWorktree(ctx context.Context, req WorktreeRequest) (WorktreeInfo, error)
	// OpenWorkerPane calls layout.apply to add one pane whose command is
	// the launch argv, with env, cwd and a unique creation label.
	OpenWorkerPane(ctx context.Context, req WorkerPaneRequest) (PaneHandle, error)
	// FindPaneByLabel is the recovery lookup used when a create response
	// was lost: creation labels round-trip through tab.list and
	// session.snapshot.
	FindPaneByLabel(ctx context.Context, label string) (PaneRef, bool, error)
	// SendText is the fallback transport only: the fixed-grammar launch
	// line (docs/plan/phase-2-design.md section 6). It is never used to
	// send anything else.
	SendText(ctx context.Context, paneID, text string) error
	// ReadPane captures scrollback evidence; it vanishes with the pane, so
	// callers capture it periodically and before every stop or retirement
	// action.
	ReadPane(ctx context.Context, paneID string, lines int) (string, error)
	// InspectPane returns the occupant identity used by every launch,
	// stop and adoption decision.
	InspectPane(ctx context.Context, paneID string) (PaneProcess, error)
	// ClosePane requests pane closure; callers apply the close rule above.
	ClosePane(ctx context.Context, paneID string) error
}
