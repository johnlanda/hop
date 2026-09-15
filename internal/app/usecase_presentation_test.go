package app_test

import (
	"context"
	"sync"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/run"
)

// runStatePresentation is a handwritten AgentPresentation that records
// every metadata patch, refusing calls inside an open transaction.
type runStatePresentation struct {
	mu      sync.Mutex
	store   *fakeStore
	Patches []app.PaneMetadata
}

func (p *runStatePresentation) ReportMetadata(_ context.Context, metadata app.PaneMetadata) error {
	if err := p.store.refuseInsideTransaction("AgentPresentation.ReportMetadata"); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Patches = append(p.Patches, metadata)
	return nil
}

func (*runStatePresentation) SelectView(context.Context, app.ViewSelection) error { return nil }
func (*runStatePresentation) ClearView(context.Context) error                     { return nil }

// lastPatchFor returns the newest patch published for one pane.
func (p *runStatePresentation) lastPatchFor(paneID string) (app.PaneMetadata, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.Patches) - 1; i >= 0; i-- {
		if p.Patches[i].PaneID == paneID {
			return p.Patches[i], true
		}
	}
	return app.PaneMetadata{}, false
}

func TestPublishRunPresentation(t *testing.T) {
	t.Run("publishes manager-first tokens and republication reflects transitions", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		presenter := &runStatePresentation{store: tc.Store}
		tc.Controller.Presentation = presenter
		taskID := seedImplementTask(t, tc, fr.RunID, 1, "Fix the flaky check", false, run.TaskReady)
		w := seedSettledWorker(t, tc, fr, taskID, 4711)

		report, err := tc.Controller.PublishRunPresentation(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("PublishRunPresentation() error = %v", err)
		}
		if len(report.Published) != 2 {
			t.Fatalf("published = %v, want the manager and the worker", report.Published)
		}
		if report.Published[0] != "pane-mgr" {
			t.Fatalf("publish order = %v, want the manager first", report.Published)
		}

		mgrPatch, ok := presenter.lastPatchFor("pane-mgr")
		if !ok {
			t.Fatalf("no patch for the manager pane")
		}
		if mgrPatch.Tokens[app.FieldRole] != string(app.RoleManager) || mgrPatch.Tokens[app.FieldOrder] != "0000000" {
			t.Fatalf("manager tokens = %v, want role manager with the manager-first order key", mgrPatch.Tokens)
		}
		if mgrPatch.Tokens[app.FieldState] != "planning" {
			t.Fatalf("manager state token = %q, want planning with the plan open", mgrPatch.Tokens[app.FieldState])
		}
		// Full replacement: the manager has no task, so hop_task is
		// CLEARED, never left stale.
		clearedTask := false
		for _, cleared := range mgrPatch.Clear {
			if cleared == app.FieldTask {
				clearedTask = true
			}
		}
		if !clearedTask {
			t.Fatalf("manager patch Clear = %v, want hop_task deleted", mgrPatch.Clear)
		}

		workerPatch, ok := presenter.lastPatchFor(w.PaneID)
		if !ok {
			t.Fatalf("no patch for the worker pane")
		}
		if workerPatch.Tokens[app.FieldRole] != string(app.RoleImplementer) {
			t.Fatalf("worker role token = %q", workerPatch.Tokens[app.FieldRole])
		}
		if workerPatch.Tokens[app.FieldTask] != "Fix the flaky check" {
			t.Fatalf("worker task token = %q", workerPatch.Tokens[app.FieldTask])
		}
		if workerPatch.Tokens[app.FieldParent] != "manager-r1" {
			t.Fatalf("worker parent token = %q, want manager-r1", workerPatch.Tokens[app.FieldParent])
		}
		if workerPatch.Tokens[app.FieldState] != "working" {
			t.Fatalf("worker state token = %q, want working", workerPatch.Tokens[app.FieldState])
		}
		// String comparison is Herdr's sort: the manager key must rank first.
		if mgrPatch.Tokens[app.FieldOrder] >= workerPatch.Tokens[app.FieldOrder] {
			t.Fatalf("ordering keys manager=%q worker=%q, want manager first", mgrPatch.Tokens[app.FieldOrder], workerPatch.Tokens[app.FieldOrder])
		}

		// Republication after transitions: the plan closes and the task
		// enters checking — both state tokens change on the next publish.
		closePlanDirectly(t, tc, fr.RunID)
		tc.Store.Tasks[taskID].value.State = run.TaskChecking
		if _, err := tc.Controller.PublishRunPresentation(context.Background(), fr.Handle); err != nil {
			t.Fatalf("republication error = %v", err)
		}
		mgrPatch, _ = presenter.lastPatchFor("pane-mgr")
		if mgrPatch.Tokens[app.FieldState] != "working" {
			t.Fatalf("manager state after plan close = %q, want working", mgrPatch.Tokens[app.FieldState])
		}
		workerPatch, _ = presenter.lastPatchFor(w.PaneID)
		if workerPatch.Tokens[app.FieldState] != "awaiting checks" {
			t.Fatalf("worker state after check claim = %q, want awaiting checks", workerPatch.Tokens[app.FieldState])
		}
	})

	t.Run("rehydration republishes current bindings only", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		presenter := &runStatePresentation{store: tc.Store}
		tc.Controller.Presentation = presenter
		taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
		w := seedSettledWorker(t, tc, fr, taskID, 4711)

		// The worker's binding is superseded (its occupant was retired):
		// rehydration must not publish to the dead pane.
		history := tc.Store.Bindings[w.SessionID]
		superseded, err := history[len(history)-1].Supersede("retired", tc.Clock.Now())
		if err != nil {
			t.Fatalf("Supersede() error = %v", err)
		}
		history[len(history)-1] = superseded
		tc.Store.Bindings[w.SessionID] = history

		report, err := tc.Controller.PublishRunPresentation(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("PublishRunPresentation() error = %v", err)
		}
		if len(report.Published) != 1 || report.Published[0] != "pane-mgr" {
			t.Fatalf("published = %v, want the manager only (a superseded binding is never published to)", report.Published)
		}
	})

	t.Run("a solo-only controller fails closed", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		_, err := tc.Controller.PublishRunPresentation(context.Background(), fr.Handle)
		if err == nil {
			t.Fatalf("publication with a nil Presentation port succeeded; want the fail-closed refusal")
		}
	})
}
