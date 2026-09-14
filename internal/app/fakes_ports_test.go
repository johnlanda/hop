package app_test

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/johnlanda/hop/internal/app"
)

// fakeRuntime is a handwritten Runtime: it records every call and returns
// scripted responses/errors, letting a test drive exactly the sequence of
// pane states a scenario needs.
type fakeRuntime struct {
	mu sync.Mutex

	nextPaneN int

	CreateWorktreeErr error
	CreateWorktreeFn  func(app.WorktreeRequest) (app.WorktreeInfo, error)

	OpenWorkerPaneErr error
	OpenWorkerPaneFn  func(app.WorkerPaneRequest) (app.PaneHandle, error)

	FindPaneByLabelFn func(label string) (app.PaneRef, bool, error)

	InspectPaneFn func(paneID string) (app.PaneProcess, error)

	ClosePaneErr error
	ClosedPanes  []string

	SentText     []string
	PaneContents map[string]string
}

func newFakeRuntime() *fakeRuntime { return &fakeRuntime{PaneContents: map[string]string{}} }

func (r *fakeRuntime) CreateWorktree(_ context.Context, req app.WorktreeRequest) (app.WorktreeInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.CreateWorktreeFn != nil {
		return r.CreateWorktreeFn(req)
	}
	if r.CreateWorktreeErr != nil {
		return app.WorktreeInfo{}, r.CreateWorktreeErr
	}
	r.nextPaneN++
	return app.WorktreeInfo{WorkspaceID: fmt.Sprintf("workspace-%d", r.nextPaneN), Path: "/worktrees/w", Branch: req.Branch}, nil
}

func (r *fakeRuntime) OpenWorkerPane(_ context.Context, req app.WorkerPaneRequest) (app.PaneHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.OpenWorkerPaneFn != nil {
		return r.OpenWorkerPaneFn(req)
	}
	if r.OpenWorkerPaneErr != nil {
		return app.PaneHandle{}, r.OpenWorkerPaneErr
	}
	r.nextPaneN++
	return app.PaneHandle{WorkspaceID: req.WorkspaceID, TabID: fmt.Sprintf("tab-%d", r.nextPaneN), PaneID: fmt.Sprintf("pane-%d", r.nextPaneN)}, nil
}

func (r *fakeRuntime) FindPaneByLabel(_ context.Context, label string) (app.PaneRef, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.FindPaneByLabelFn != nil {
		return r.FindPaneByLabelFn(label)
	}
	return app.PaneRef{}, false, nil
}

func (r *fakeRuntime) SendText(_ context.Context, paneID, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.SentText = append(r.SentText, paneID+": "+text)
	return nil
}

func (r *fakeRuntime) ReadPane(_ context.Context, paneID string, _ int) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.PaneContents[paneID], nil
}

func (r *fakeRuntime) InspectPane(_ context.Context, paneID string) (app.PaneProcess, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.InspectPaneFn != nil {
		return r.InspectPaneFn(paneID)
	}
	return app.PaneProcess{}, fmt.Errorf("app_test: no InspectPaneFn configured")
}

func (r *fakeRuntime) ClosePane(_ context.Context, paneID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ClosePaneErr != nil {
		return r.ClosePaneErr
	}
	r.ClosedPanes = append(r.ClosedPanes, paneID)
	return nil
}

// fakeArtifacts is a handwritten ArtifactStore: an in-memory file map.
type fakeArtifacts struct {
	mu       sync.Mutex
	files    map[string][]byte
	WriteErr error
}

func newFakeArtifacts() *fakeArtifacts { return &fakeArtifacts{files: map[string][]byte{}} }

func (a *fakeArtifacts) WriteArtifact(_ context.Context, path string, content []byte) error {
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
	Results map[string]app.CommandResult
	Errs    map[string]error
	Calls   []app.Command
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{Results: map[string]app.CommandResult{}, Errs: map[string]error{}}
}

func (c *fakeCommands) key(cmd app.Command) string { return strings.Join(cmd.Argv, " ") }

func (c *fakeCommands) Run(_ context.Context, cmd app.Command) (app.CommandResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Calls = append(c.Calls, cmd)
	k := c.key(cmd)
	if err, ok := c.Errs[k]; ok {
		return app.CommandResult{}, err
	}
	if result, ok := c.Results[k]; ok {
		return result, nil
	}
	switch {
	case strings.Contains(k, "rev-parse --git-common-dir"):
		// Real git resolves a worktree's common dir to the *main*
		// repository's .git, not the worktree's own directory; tests use
		// "/repo" as the default repository root (defaultStartRunRequest),
		// so the default stub matches that regardless of cmd.Dir.
		return app.CommandResult{ExitCode: 0, Stdout: []byte("/repo/.git\n")}, nil
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

// fakeGroups is a handwritten ProcessGroupInspector.
type fakeGroups struct {
	mu        sync.Mutex
	Processes map[int][]app.GroupProcess
	ListErr   map[int]error
	Signaled  []int
}

func newFakeGroups() *fakeGroups {
	return &fakeGroups{Processes: map[int][]app.GroupProcess{}, ListErr: map[int]error{}}
}

func (g *fakeGroups) GroupProcesses(_ context.Context, pgid int) ([]app.GroupProcess, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err, ok := g.ListErr[pgid]; ok {
		return nil, err
	}
	return g.Processes[pgid], nil
}

func (g *fakeGroups) SignalGroup(_ context.Context, pgid int) error {
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
