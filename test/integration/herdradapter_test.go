package integration

import (
	"context"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/herdr"
	"github.com/johnlanda/hop/internal/app"
)

// TestRealProcessHerdrAdapterWorkspaceAndWorktreeLabels drives Phase 3 slice
// 5's herdr adapter additions end to end against a real, disposable herdr
// server (never the operator's own live server): CreateWorkspace and
// FindWorkspaceByLabel (S8), plus CreateWorktree's new creation-label field
// (S9). Unlike the spike probes, which drive the raw wire protocol directly,
// this test goes through the Go adapter (herdr.Runtime) itself, since that
// is the boundary this slice actually changed.
func TestRealProcessHerdrAdapterWorkspaceAndWorktreeLabels(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)
	runtime := herdr.NewRuntime(server.socketPath)

	runID := newSpikeUUID(t)
	workspaceLabel := "hop-adapter-ws-" + runID

	callCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), callTimeout)
	}

	// CreateWorkspace: an explicit cwd, additive env and a unique creation
	// label, through the adapter method rather than a raw workspace.create
	// call.
	ctx, cancel := callCtx()
	handle, err := runtime.CreateWorkspace(ctx, app.WorkspaceRequest{
		Cwd:   server.workDir(),
		Label: workspaceLabel,
		Env:   map[string]string{"HOP_RUN_ID": runID},
	})
	cancel()
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if handle.WorkspaceID == "" || handle.TabID == "" || handle.PaneID == "" {
		t.Fatalf("CreateWorkspace returned an incomplete handle: %+v", handle)
	}

	// FindWorkspaceByLabel recovers exactly the same workspace/tab/pane,
	// discarding CreateWorkspace's own response ids to model a controller
	// that crashed before recording them (S8's recovery pattern).
	ctx, cancel = callCtx()
	ref, found, err := runtime.FindWorkspaceByLabel(ctx, workspaceLabel)
	cancel()
	if err != nil {
		t.Fatalf("FindWorkspaceByLabel: %v", err)
	}
	if !found {
		t.Fatal("FindWorkspaceByLabel did not find the just-created workspace")
	}
	want := app.WorkspaceRef(handle)
	if ref != want {
		t.Errorf("FindWorkspaceByLabel recovered %+v, want %+v matching CreateWorkspace's own response", ref, want)
	}

	// A label with no matching workspace is a legitimate not-found, never
	// an error.
	missingLabel := workspaceLabel + "-missing"
	ctx, cancel = callCtx()
	_, found, err = runtime.FindWorkspaceByLabel(ctx, missingLabel)
	cancel()
	if err != nil || found {
		t.Errorf("FindWorkspaceByLabel(%q) = found=%v err=%v, want not found, nil error", missingLabel, found, err)
	}

	// CreateWorktree's S9 creation label: sent on the real wire request even
	// though no internal/app call site sets it yet (that lands with the
	// application-layer work in slices 2b/6), and recovered through the
	// exact same FindWorkspaceByLabel descent, since worktree.create's label
	// names the new WORKSPACE precisely like workspace.create's does.
	repo := newFixtureRepo(t, artifacts, server, "repo")
	const branch = "hop/r1/t1a1"
	worktreeLabel := "hop-adapter-wt-" + runID

	ctx, cancel = callCtx()
	info, err := runtime.CreateWorktree(ctx, app.WorktreeRequest{
		RepositoryRoot: repo.Root, Branch: branch, BaseRef: repo.Base, Label: worktreeLabel,
	})
	cancel()
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if info.WorkspaceID == "" || info.Path == "" {
		t.Fatalf("CreateWorktree returned an incomplete result: %+v", info)
	}
	if info.Branch != branch {
		t.Errorf("CreateWorktree branch = %q, want %q", info.Branch, branch)
	}
	if info.WorkspaceID == handle.WorkspaceID {
		t.Fatalf("CreateWorktree reused the manager-shaped workspace %q instead of creating its own", info.WorkspaceID)
	}

	// The adapter surfaces exactly what the app's own verify-HEAD-after-create
	// rule needs (the checkout path); the HEAD check itself runs the same way
	// application code runs it, through a plain git invocation.
	if got := worktreeHead(t, repo, info.Path); got != repo.Base {
		t.Errorf("worktree HEAD = %s, want the requested base %s", got, repo.Base)
	}

	ctx, cancel = callCtx()
	worktreeRef, found, err := runtime.FindWorkspaceByLabel(ctx, worktreeLabel)
	cancel()
	if err != nil {
		t.Fatalf("FindWorkspaceByLabel(worktree label): %v", err)
	}
	if !found {
		t.Fatal("FindWorkspaceByLabel did not find the worktree's labeled workspace")
	}
	if worktreeRef.WorkspaceID != info.WorkspaceID {
		t.Errorf("FindWorkspaceByLabel resolved workspace %q, want the worktree's own workspace %q", worktreeRef.WorkspaceID, info.WorkspaceID)
	}

	// Read the recovered worktree workspace's root pane back through
	// InspectPane, proving the descent's pane id is real and addressable,
	// not merely well-formed.
	ctx, cancel = callCtx()
	_, err = runtime.InspectPane(ctx, worktreeRef.PaneID)
	cancel()
	if err != nil {
		t.Errorf("InspectPane(%s) on the recovered worktree root pane: %v", worktreeRef.PaneID, err)
	}
}
