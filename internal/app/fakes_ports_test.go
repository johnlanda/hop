package app_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/johnlanda/hop/internal/app"
)

// fakeRuntime is a handwritten Runtime: it records every call and returns
// scripted responses/errors, letting a test drive exactly the sequence of
// pane states a scenario needs. Every call refuses to run while a store
// unit of work is open (the section 4 transaction rule).
type fakeRuntime struct {
	mu sync.Mutex

	// store, when set, rejects any call made inside an open transaction.
	store *fakeStore

	nextPaneN int
	// worktreeN counts successful default CreateWorktree responses; each
	// gets its own path, as Herdr's worktree.create gives every created
	// checkout (spike S9, TestSpikeConcurrentWorktreeCreate), and as the
	// store's UNIQUE(path) requires.
	worktreeN int

	CreateWorktreeErr error
	CreateWorktreeFn  func(app.WorktreeRequest) (app.WorktreeInfo, error)

	// CreateWorktreeRequests records every request as received, so a test
	// can assert on fields (like Label) that no scripted response echoes
	// back.
	CreateWorktreeRequests []app.WorktreeRequest

	OpenWorkerPaneErr error
	OpenWorkerPaneFn  func(app.WorkerPaneRequest) (app.PaneHandle, error)

	FindPaneByLabelFn func(label string) (app.PaneRef, bool, error)

	// InspectPaneFn scripts InspectPane. Scripted PaneProcess values must
	// use the real pane.process_info surface's shapes: Foreground lists
	// EVERY member of the foreground process group in raw platform order
	// (macOS unsorted proc_listpids, Linux ascending pid), so a launched
	// harness with children — Claude Code spawns its MCP servers into its
	// own group — is a multi-member listing with no index semantics;
	// mcpGroupPane (helpers_test.go) reproduces the pinned live shape.
	InspectPaneFn func(paneID string) (app.PaneProcess, error)

	// ReadPaneFn, when set, handles ReadPane; a test can take the lease
	// over mid-capture through it.
	ReadPaneFn func(paneID string, lines int) (string, error)

	ClosePaneErr error
	ClosedPanes  []string

	PaneContents map[string]string

	// ServerInstanceValue is what ServerInstance reports; "" means the
	// server process identity could not be established (unknown).
	ServerInstanceValue string
	ServerInstanceErr   error
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{PaneContents: map[string]string{}, ServerInstanceValue: "peer-pid:1"}
}

func (r *fakeRuntime) ServerInstance(context.Context) (string, error) {
	if err := r.store.refuseInsideTransaction("Runtime.ServerInstance"); err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ServerInstanceErr != nil {
		return "", r.ServerInstanceErr
	}
	return r.ServerInstanceValue, nil
}

func (r *fakeRuntime) CreateWorktree(_ context.Context, req app.WorktreeRequest) (app.WorktreeInfo, error) {
	if err := r.store.refuseInsideTransaction("Runtime.CreateWorktree"); err != nil {
		return app.WorktreeInfo{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.CreateWorktreeRequests = append(r.CreateWorktreeRequests, req)
	if r.CreateWorktreeFn != nil {
		return r.CreateWorktreeFn(req)
	}
	if r.CreateWorktreeErr != nil {
		return app.WorktreeInfo{}, r.CreateWorktreeErr
	}
	r.nextPaneN++
	r.worktreeN++
	path := "/worktrees/w"
	if r.worktreeN > 1 {
		path = fmt.Sprintf("/worktrees/w-%d", r.worktreeN)
	}
	return app.WorktreeInfo{WorkspaceID: fmt.Sprintf("workspace-%d", r.nextPaneN), Path: path, Branch: req.Branch}, nil
}

func (r *fakeRuntime) OpenWorkerPane(_ context.Context, req app.WorkerPaneRequest) (app.PaneHandle, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	if err := r.store.refuseInsideTransaction("Runtime.OpenWorkerPane"); err != nil {
		return app.PaneHandle{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.OpenWorkerPaneFn != nil {
		return r.OpenWorkerPaneFn(req)
	}
	// The port's documented contract: the pane joins an existing workspace
	// with a command argv, a cwd and a unique creation label; the fake
	// rejects omissions rather than silently accepting them.
	if req.WorkspaceID == "" || req.Cwd == "" || len(req.Command) == 0 || req.Label == "" {
		return app.PaneHandle{}, fmt.Errorf("app_test: WorkerPaneRequest is missing required fields: %+v", req)
	}
	if r.OpenWorkerPaneErr != nil {
		return app.PaneHandle{}, r.OpenWorkerPaneErr
	}
	r.nextPaneN++
	return app.PaneHandle{WorkspaceID: req.WorkspaceID, TabID: fmt.Sprintf("tab-%d", r.nextPaneN), PaneID: fmt.Sprintf("pane-%d", r.nextPaneN)}, nil
}

func (r *fakeRuntime) FindPaneByLabel(_ context.Context, label string) (app.PaneRef, bool, error) {
	if err := r.store.refuseInsideTransaction("Runtime.FindPaneByLabel"); err != nil {
		return app.PaneRef{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.FindPaneByLabelFn != nil {
		return r.FindPaneByLabelFn(label)
	}
	return app.PaneRef{}, false, nil
}

func (r *fakeRuntime) ReadPane(_ context.Context, paneID string, lines int) (string, error) {
	if err := r.store.refuseInsideTransaction("Runtime.ReadPane"); err != nil {
		return "", err
	}
	r.mu.Lock()
	fn := r.ReadPaneFn
	r.mu.Unlock()
	if fn != nil {
		return fn(paneID, lines)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.PaneContents[paneID], nil
}

func (r *fakeRuntime) InspectPane(_ context.Context, paneID string) (app.PaneProcess, error) {
	if err := r.store.refuseInsideTransaction("Runtime.InspectPane"); err != nil {
		return app.PaneProcess{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.InspectPaneFn != nil {
		return r.InspectPaneFn(paneID)
	}
	return app.PaneProcess{}, fmt.Errorf("app_test: no InspectPaneFn configured")
}

func (r *fakeRuntime) ClosePane(_ context.Context, paneID string) error {
	if err := r.store.refuseInsideTransaction("Runtime.ClosePane"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ClosePaneErr != nil {
		return r.ClosePaneErr
	}
	r.ClosedPanes = append(r.ClosedPanes, paneID)
	return nil
}

// fakeArtifacts is a handwritten ArtifactStore: an in-memory file map.
// Writes and reads refuse to run while a store unit of work is open.
type fakeArtifacts struct {
	mu       sync.Mutex
	store    *fakeStore
	files    map[string][]byte
	WriteErr error
}

func newFakeArtifacts() *fakeArtifacts { return &fakeArtifacts{files: map[string][]byte{}} }

func (a *fakeArtifacts) WriteArtifact(_ context.Context, path string, content []byte) error {
	if err := a.store.refuseInsideTransaction("ArtifactStore.WriteArtifact"); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.WriteErr != nil {
		return a.WriteErr
	}
	stored := make([]byte, len(content))
	copy(stored, content)
	a.files[path] = stored
	return nil
}

func (a *fakeArtifacts) ReadArtifact(_ context.Context, path string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	content, ok := a.files[path]
	if !ok {
		return nil, fmt.Errorf("app_test: no artifact at %s", path)
	}
	return content, nil
}

// fakeCommands is a handwritten CommandRunner: scripted results keyed by
// the joined argv, falling back to a generic git success for
// "git worktree"/"git rev-parse"/"git cat-file" prefixes so tests only
// script the commands they care about.
type fakeCommands struct {
	mu      sync.Mutex
	store   *fakeStore
	Results map[string]app.CommandResult
	Errs    map[string]error
	Calls   []app.Command

	// CheckExecFn, when set, handles the hop check-exec spawn (matched by
	// argv[1] == "check-exec"), receiving the act context so a test can
	// block until dispatch cancellation or inject state mid-execution; git
	// commands keep their scripted defaults.
	CheckExecFn func(ctx context.Context, cmd app.Command) (app.CommandResult, error)

	// RunHook, when set, is consulted first for every command with the act
	// context; handled=false falls through to the scripted defaults.
	RunHook func(ctx context.Context, cmd app.Command) (result app.CommandResult, handled bool, err error)

	// CheckExecExitCode and CheckExecErr script the hop check-exec spawn
	// specifically, matched by argv[1] == "check-exec" rather than by exact
	// argv text, since its argv always includes a freshly generated
	// operation id no test can predict.
	CheckExecExitCode int
	CheckExecErr      error
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{Results: map[string]app.CommandResult{}, Errs: map[string]error{}}
}

func (*fakeCommands) key(cmd app.Command) string { return strings.Join(cmd.Argv, " ") }

func (c *fakeCommands) Run(ctx context.Context, cmd app.Command) (app.CommandResult, error) {
	// Mirror the real Runner's own contract (internal/adapters/process.
	// Runner.Run): argv must be non-empty and argv[0] must be an absolute
	// path. Enforcing it here, not just in the real adapter, is what makes
	// a caller passing a bare or relative executable name fail the app
	// suite instead of only surfacing against a real process at runtime.
	if len(cmd.Argv) == 0 {
		return app.CommandResult{}, fmt.Errorf("app_test: CommandRunner.Run called with an empty argv")
	}
	if !filepath.IsAbs(cmd.Argv[0]) {
		return app.CommandResult{}, fmt.Errorf("app_test: CommandRunner.Run called with executable %q, which is not an absolute path; the real Runner refuses this", cmd.Argv[0])
	}
	if err := c.store.refuseInsideTransaction("CommandRunner.Run"); err != nil {
		return app.CommandResult{}, err
	}
	c.mu.Lock()
	hook := c.RunHook
	c.mu.Unlock()
	if hook != nil {
		if result, handled, err := hook(ctx, cmd); handled {
			c.mu.Lock()
			c.Calls = append(c.Calls, cmd)
			c.mu.Unlock()
			return result, err
		}
	}
	c.mu.Lock()
	c.Calls = append(c.Calls, cmd)
	if len(cmd.Argv) >= 2 && cmd.Argv[1] == "check-exec" {
		if fn := c.CheckExecFn; fn != nil {
			c.mu.Unlock()
			return fn(ctx, cmd)
		}
		defer c.mu.Unlock()
		if c.CheckExecErr != nil {
			return app.CommandResult{}, c.CheckExecErr
		}
		return app.CommandResult{ExitCode: c.CheckExecExitCode}, nil
	}
	defer c.mu.Unlock()
	k := c.key(cmd)
	if err, ok := c.Errs[k]; ok {
		return app.CommandResult{}, err
	}
	if result, ok := c.Results[k]; ok {
		return result, nil
	}
	switch {
	case strings.Contains(k, "--git-common-dir"):
		// Real git resolves a worktree's common dir to the *main*
		// repository's .git, not the worktree's own directory; tests use
		// "/repo" as the default repository root (defaultStartRunRequest),
		// so the default stub matches that regardless of cmd.Dir.
		return app.CommandResult{ExitCode: 0, Stdout: []byte("/repo/.git\n")}, nil
	case strings.Contains(k, "^{tree}"):
		return app.CommandResult{ExitCode: 0, Stdout: []byte("tttttttttttttttttttttttttttttttttttttttt\n")}, nil
	case strings.Contains(k, "--show-object-format"):
		return app.CommandResult{ExitCode: 0, Stdout: []byte("sha1\n")}, nil
	case strings.Contains(k, "rev-parse HEAD"):
		return app.CommandResult{ExitCode: 0, Stdout: []byte("cccccccccccccccccccccccccccccccccccccccc\n")}, nil
	case strings.Contains(k, "cat-file -t"):
		return app.CommandResult{ExitCode: 0, Stdout: []byte("commit\n")}, nil
	case strings.Contains(k, "worktree add"), strings.Contains(k, "worktree remove"):
		return app.CommandResult{ExitCode: 0}, nil
	case strings.Contains(k, "check-exec"):
		return app.CommandResult{ExitCode: 0}, nil
	}
	return app.CommandResult{ExitCode: 0}, nil
}

// fakeGroups is a handwritten ProcessGroupInspector. Calls refuse to run
// while a store unit of work is open.
type fakeGroups struct {
	mu        sync.Mutex
	store     *fakeStore
	Processes map[int][]app.GroupProcess
	ListErr   map[int]error
	Signaled  []int
}

func newFakeGroups() *fakeGroups {
	return &fakeGroups{Processes: map[int][]app.GroupProcess{}, ListErr: map[int]error{}}
}

func (g *fakeGroups) GroupProcesses(_ context.Context, pgid int) ([]app.GroupProcess, error) {
	if err := g.store.refuseInsideTransaction("ProcessGroupInspector.GroupProcesses"); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err, ok := g.ListErr[pgid]; ok {
		return nil, err
	}
	return g.Processes[pgid], nil
}

func (g *fakeGroups) SignalGroup(_ context.Context, pgid int) error {
	if err := g.store.refuseInsideTransaction("ProcessGroupInspector.SignalGroup"); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Signaled = append(g.Signaled, pgid)
	return nil
}

// fakeConfig is a handwritten ConfigurationSource.
type fakeConfig struct {
	Policy app.RunPolicy
	Err    error
}

func (c *fakeConfig) Load(context.Context, string) (app.RunPolicy, error) {
	if c.Err != nil {
		return app.RunPolicy{}, c.Err
	}
	return c.Policy, nil
}
