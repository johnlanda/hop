package app

import (
	"context"
	"errors"
)

// WorktreeRequest is CreateWorktree's input: the repository to check out
// from, the branch to create, the base ref to create it at, and a unique
// creation label (S9-confirmed: worktree.create accepts the same kind of
// creation label as workspace.create, naming the new workspace, and it
// round-trips through session.snapshot exactly like a CreateWorkspace
// label). Label is optional here — the adapter sends it only when
// non-empty — because no call site sets it yet; per-attempt labeling (the
// operation UUID) and the label-recovery decision row for worktree.create
// land with the application-layer work in slices 2b/6, not this one.
type WorktreeRequest struct {
	RepositoryRoot string
	Branch         string
	BaseRef        string
	Label          string
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

// ErrPaneNotFound is the typed result Runtime.InspectPane reports (wrapped
// or directly) when the pane POSITIVELY does not exist on the server — a
// successful observation of absence, as opposed to a transport, timeout or
// permission failure, which is an ordinary error and never absence
// evidence. Absence decisions require this exact distinction.
var ErrPaneNotFound = errors.New("app: pane not found")

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
	// ReadPane captures scrollback evidence; it vanishes with the pane, so
	// callers capture it periodically and before every stop or retirement
	// action.
	ReadPane(ctx context.Context, paneID string, lines int) (string, error)
	// InspectPane returns the occupant identity used by every launch,
	// stop and adoption decision. A pane that positively does not exist is
	// reported as an error wrapping ErrPaneNotFound; any other error is an
	// inspection failure and never counts as absence.
	InspectPane(ctx context.Context, paneID string) (PaneProcess, error)
	// ClosePane requests pane closure; callers apply the close rule above.
	ClosePane(ctx context.Context, paneID string) error
	// ServerInstance returns an opaque, adapter-formatted identity scoped
	// to BOTH the configured socket path and the server process behind it:
	// the adapter derives the token from its own connection to that
	// socket (for example the socket peer pid), so equality of two
	// non-empty tokens implies the same socket and the same server
	// process. An empty string means the identity could not be
	// established — unknown, never fabricated — and is not an error; the
	// application compares tokens by equality only and treats unknown as
	// ambiguous. Peer-pid recycling is not detected (a documented residual
	// limitation; start-time hardening is a Phase 7 recovery item). The
	// token is captured immediately before pane creation, frozen into the
	// pane.open intent and recorded in the creation binding; resume
	// observes it again to establish server continuity: a deferred native
	// restore fires only after a server restart, so an unchanged server
	// process with the pane gone cannot have a restore pending.
	ServerInstance(ctx context.Context) (string, error)
}

// WorkspaceRequest is CreateWorkspace's input: an explicit cwd, additive
// env and a unique creation label. Unlike WorktreeRequest, a plain
// workspace carries no repository/branch/base — it is the manager's own
// placement, at the repository root, never a worktree.
type WorkspaceRequest struct {
	Cwd   string
	Env   map[string]string
	Label string
}

// WorkspaceHandle is what Herdr created: the workspace, its sole tab and
// that tab's sole root pane (S8-confirmed shape:
// {type: "workspace_created", workspace, tab, root_pane}).
type WorkspaceHandle struct {
	WorkspaceID string
	TabID       string
	PaneID      string
}

// WorkspaceRef identifies a workspace FindWorkspaceByLabel recovered,
// descended to its sole tab's sole root pane.
type WorkspaceRef struct {
	WorkspaceID string
	TabID       string
	PaneID      string
}

// WorkspaceRuntime is the consumer-owned port over Herdr's plain-workspace
// surface: the manager's placement only (worker and reviewer placement
// keeps coming from Runtime.CreateWorktree's returned workspace). Declared
// as a SEPARATE interface from Runtime, never as new methods added to it,
// for the identical reason WorkflowRepositories is separate from
// UnitOfWork: Runtime is a Controller struct field the existing Herdr
// adapter already satisfies, and adding methods to it would break that
// satisfaction — and cmd/hop's composition wiring — before the adapter
// (slice 5) implements them. Composition wires a Controller.Workspaces
// field of this type once the adapter exists; it is nil for a Controller
// that never runs feature mode, and no solo-mode code path reads it.
type WorkspaceRuntime interface {
	// CreateWorkspace calls workspace.create: no focus, so it never steals
	// the session's active workspace except that session's very
	// first-ever one, which Herdr always activates regardless of the
	// request (S8-confirmed).
	CreateWorkspace(ctx context.Context, req WorkspaceRequest) (WorkspaceHandle, error)
	// FindWorkspaceByLabel resolves a workspace by its unique creation
	// LABEL — a WORKSPACE attribute (WorkspaceInfo.label), never the root
	// pane's — then descends workspace -> its sole tab -> that tab's sole
	// pane. Zero matches is (zero, false, nil); more than one workspace
	// with the label, or more than one tab or pane on the resolved
	// workspace, is an error, never a guess (S8-confirmed).
	FindWorkspaceByLabel(ctx context.Context, label string) (WorkspaceRef, bool, error)
}
