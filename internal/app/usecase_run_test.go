package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// testController assembles a Controller and its fakes for one test, with a
// shared fake clock so the store and the controller see the same time.
type testController struct {
	Controller *app.Controller
	Store      *fakeStore
	Runtime    *fakeRuntime
	Artifacts  *fakeArtifacts
	Clock      *fakeClock
	IDs        *fakeIDs
	Commands   *fakeCommands
	Groups     *fakeGroups
	Config     *fakeConfig
}

func newTestController(policy app.RunPolicy) *testController { //nolint:gocritic // hugeParam: RunPolicy is a small test fixture value passed once per test setup.
	clock := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	store := newFakeStore(clock)
	runtime := newFakeRuntime()
	artifacts := newFakeArtifacts()
	ids := newFakeIDs()
	commands := newFakeCommands()
	groups := newFakeGroups()
	config := &fakeConfig{Policy: policy}

	return &testController{
		Controller: &app.Controller{
			Store: store, Read: store, Submissions: store,
			Runtime: runtime, Artifacts: artifacts, Clock: clock, IDs: ids,
			Commands: commands, Groups: groups, Config: config,
		},
		Store: store, Runtime: runtime, Artifacts: artifacts, Clock: clock,
		IDs: ids, Commands: commands, Groups: groups, Config: config,
	}
}

func defaultPolicy() app.RunPolicy {
	return app.RunPolicy{CheckArgv: []string{"sh", "check.sh"}, Harness: "claude"}
}

func defaultStartRunRequest() app.StartRunRequest {
	return app.StartRunRequest{
		RepositoryRoot: "/repo", Brief: "do the thing", ControllerID: "controller-1",
		StateRoot: "/state", HOPPath: "/usr/local/bin/hop",
	}
}

func TestStartRun(t *testing.T) {
	t.Run("happy path freezes the run and opens the worker pane", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		result, handle, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if err != nil {
			t.Fatalf("StartRun() error = %v", err)
		}
		if result.RunID == "" {
			t.Fatalf("StartRun() returned an empty run id")
		}
		if result.Sequence != 1 {
			t.Fatalf("Sequence = %d, want 1", result.Sequence)
		}
		if handle.RunID() != result.RunID {
			t.Fatalf("handle run id %s != result run id %s", handle.RunID(), result.RunID)
		}

		detail, err := tc.Store.LoadRunStatus(context.Background(), mustRunID(t, result.RunID))
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if detail.State != run.RunLaunching {
			t.Fatalf("Run.State = %s, want %s", detail.State, run.RunLaunching)
		}
		if detail.TaskState != run.TaskActive {
			t.Fatalf("Task.State = %s, want %s", detail.TaskState, run.TaskActive)
		}
		if detail.AttemptState != run.AttemptLaunching {
			t.Fatalf("Attempt.State = %s, want %s", detail.AttemptState, run.AttemptLaunching)
		}
		if detail.Binding == nil || detail.Binding.PaneID == "" {
			t.Fatalf("no runtime binding recorded after pane open")
		}
		if len(tc.Artifacts.files) != 1 {
			t.Fatalf("assignment artifact was not written: %d files", len(tc.Artifacts.files))
		}
		if len(detail.Artifacts) != 1 || detail.Artifacts[0].Kind != run.ArtifactAssignment {
			t.Fatalf("assignment artifact row not recorded: %+v", detail.Artifacts)
		}
	})

	t.Run("effect ordering: intent is journaled before the external act", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		var sawIntentBeforeAct bool
		tc.Runtime.OpenWorkerPaneFn = func(req app.WorkerPaneRequest) (app.PaneHandle, error) {
			// By the time OpenWorkerPane (the act) runs, the intent
			// transaction must already have committed: Run/Attempt should
			// already show launching.
			runID := tc.onlyRunID(t)
			detail, err := tc.Store.LoadRunStatus(context.Background(), runID)
			if err != nil {
				t.Fatalf("LoadRunStatus during act: %v", err)
			}
			sawIntentBeforeAct = detail.AttemptState == run.AttemptLaunching
			return app.PaneHandle{WorkspaceID: req.WorkspaceID, TabID: "tab-1", PaneID: "pane-1"}, nil
		}
		if _, _, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest()); err != nil {
			t.Fatalf("StartRun() error = %v", err)
		}
		if !sawIntentBeforeAct {
			t.Fatalf("pane.open's act ran before its intent was committed")
		}
	})

	t.Run("rejects a repository policy with no check command", func(t *testing.T) {
		tc := newTestController(app.RunPolicy{Harness: "claude"})
		_, _, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if err == nil {
			t.Fatalf("StartRun() succeeded with no [check] command")
		}
	})

	t.Run("refuses a SHA-256-object-format repository before any side effect", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		tc.Commands.Results["git -C /repo rev-parse --show-object-format"] = app.CommandResult{ExitCode: 0, Stdout: []byte("sha256\n")}
		_, _, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if err == nil {
			t.Fatalf("StartRun() accepted an unsupported object format")
		}
		if len(tc.Store.Runs) != 0 {
			t.Fatalf("StartRun() created a run despite the unsupported object format")
		}
	})

	t.Run("refuses a HOP path containing a single quote before any side effect", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		req := defaultStartRunRequest()
		req.HOPPath = "/usr/local/bin/it's-hop"
		_, _, err := tc.Controller.StartRun(context.Background(), req)
		if err == nil {
			t.Fatalf("StartRun() accepted a HOP path with a single quote")
		}
		if len(tc.Store.Runs) != 0 {
			t.Fatalf("StartRun() created a run despite the refused path")
		}
	})

	t.Run("worktree.create transport error is ambiguous: the operation stays reconciling, never failed", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		tc.Runtime.CreateWorktreeErr = context.DeadlineExceeded
		_, _, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if err == nil {
			t.Fatalf("StartRun() succeeded despite worktree.create failing")
		}
		if len(tc.Store.Worktrees) != 0 {
			t.Fatalf("a worktree row was recorded despite the act failing")
		}
		runID := tc.onlyRunID(t)
		lease, ok := tc.Store.Leases[runID]
		if !ok || lease.held {
			t.Fatalf("lease was not released after a fatal startup failure")
		}
		var op app.Operation
		for _, o := range tc.Store.Operations {
			if o.Kind == app.OpWorktreeCreate {
				op = o
			}
		}
		if op.State != app.OperationReconciling {
			t.Fatalf("worktree.create operation state = %s, want %s (creation could have happened)", op.State, app.OperationReconciling)
		}
	})

	t.Run("worktree provenance: a checkout of a different repository is rejected, path prefixes never adopt", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		// The candidate's common directory is a lexical PREFIX-extension of
		// the intended repository ("/repo-other" vs "/repo"): the old
		// prefix comparison would adopt it; equality must reject it.
		tc.Commands.Results["git -C /worktrees/w rev-parse --path-format=absolute --git-common-dir"] = app.CommandResult{ExitCode: 0, Stdout: []byte("/repo-other/.git\n")}
		_, _, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if err == nil {
			t.Fatalf("StartRun() adopted a checkout of a different repository")
		}
		if len(tc.Store.Worktrees) != 0 {
			t.Fatalf("a worktree row was recorded for an unrelated checkout")
		}
		var op app.Operation
		for _, o := range tc.Store.Operations {
			if o.Kind == app.OpWorktreeCreate {
				op = o
			}
		}
		if op.State != app.OperationFailed {
			t.Fatalf("worktree.create operation state = %s, want %s (unrelated checkout is a failure)", op.State, app.OperationFailed)
		}
	})

	t.Run("worktree provenance: a candidate at the wrong base commit is rejected", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		tc.Commands.Results["git -C /worktrees/w rev-parse HEAD^{commit}"] = app.CommandResult{ExitCode: 0, Stdout: []byte("dddddddddddddddddddddddddddddddddddddddd\n")}
		_, _, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if err == nil {
			t.Fatalf("StartRun() adopted a checkout at the wrong base commit")
		}
		if len(tc.Store.Worktrees) != 0 {
			t.Fatalf("a worktree row was recorded for a wrong-base checkout")
		}
	})

	t.Run("worktree intent freezes the resolved base object id, never a mutable ref", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		if _, _, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest()); err != nil {
			t.Fatalf("StartRun() error = %v", err)
		}
		for _, o := range tc.Store.Operations {
			if o.Kind != app.OpWorktreeCreate {
				continue
			}
			intent, ok := o.Intent.(map[string]any)
			if !ok {
				t.Fatalf("worktree.create intent shape = %T, want a JSON object", o.Intent)
			}
			base, isString := intent["base_ref"].(string)
			if !isString || base != "cccccccccccccccccccccccccccccccccccccccc" {
				t.Fatalf("intent base_ref = %q, want the resolved base commit object id", base)
			}
		}
	})

	t.Run("crash between pane.open's act and outcome leaves the operation reconciling", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		tc.Runtime.OpenWorkerPaneErr = context.DeadlineExceeded
		// No pane is found by label either: a genuinely lost create.
		_, _, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if err == nil {
			t.Fatalf("StartRun() succeeded despite pane.open failing")
		}
		var op app.Operation
		found := false
		for _, o := range tc.Store.Operations {
			if o.Kind == app.OpPaneOpen {
				op, found = o, true
			}
		}
		if !found {
			t.Fatalf("pane.open operation was not journaled")
		}
		if op.State != app.OperationReconciling {
			t.Fatalf("pane.open operation state = %s, want %s (never re-created)", op.State, app.OperationReconciling)
		}
	})

	t.Run("recovers a lost create response by creation label", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		tc.Runtime.OpenWorkerPaneErr = context.DeadlineExceeded
		tc.Runtime.FindPaneByLabelFn = func(string) (app.PaneRef, bool, error) {
			return app.PaneRef{WorkspaceID: "workspace-1", TabID: "tab-recovered", PaneID: "pane-recovered"}, true, nil
		}
		_, handle, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if err != nil {
			t.Fatalf("StartRun() error = %v", err)
		}
		detail, err := tc.Store.LoadRunStatus(context.Background(), tc.onlyRunID(t))
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		if detail.Binding == nil || detail.Binding.PaneID != "pane-recovered" {
			t.Fatalf("recovery by label did not adopt the found pane: %+v", detail.Binding)
		}
		_ = handle
	})
}

// mustRunID parses s as a RunID or fails the test.
func mustRunID(t *testing.T, s string) identity.RunID {
	t.Helper()
	id, err := identity.ParseRunID(s)
	if err != nil {
		t.Fatalf("parse run id %q: %v", s, err)
	}
	return id
}

// onlyRunID returns the single run id currently in the store, failing the
// test if there is not exactly one.
func (tc *testController) onlyRunID(t *testing.T) identity.RunID {
	t.Helper()
	tc.Store.mu.Lock()
	defer tc.Store.mu.Unlock()
	if len(tc.Store.Runs) != 1 {
		t.Fatalf("expected exactly one run, found %d", len(tc.Store.Runs))
	}
	for id := range tc.Store.Runs {
		return id
	}
	return ""
}
