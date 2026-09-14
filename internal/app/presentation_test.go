package app_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

func TestAgentDisplayTokens(t *testing.T) {
	cases := []struct {
		name        string
		display     app.AgentDisplay
		wantTokens  map[string]string
		wantCleared []string
	}{
		{
			name: "manager clears task, parent and account",
			display: app.AgentDisplay{
				PaneID: "w1:p1", Run: "r18", RunSequence: 18,
				Role: app.RoleManager, WorkerSequence: 0, State: "planning",
			},
			wantTokens: map[string]string{
				"hop_run": "r18", "hop_run_order": "000018",
				"hop_order": "0000000", "hop_role": "manager", "hop_state": "planning",
			},
			wantCleared: []string{"hop_account", "hop_parent", "hop_task"},
		},
		{
			name: "worker carries every populated token and clears nothing",
			display: app.AgentDisplay{
				PaneID: "w1:p2", Run: "r18", RunSequence: 18,
				Role: app.RoleImplementer, WorkerSequence: 10,
				Task: "retry policy", ParentLabel: "manager-r18",
				Account: "claude-a", State: "awaiting checks",
			},
			wantTokens: map[string]string{
				"hop_run": "r18", "hop_run_order": "000018",
				"hop_order": "1000010", "hop_role": "implementer",
				"hop_task": "retry policy", "hop_parent": "manager-r18",
				"hop_account": "claude-a", "hop_state": "awaiting checks",
			},
			wantCleared: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.display.Tokens(); !reflect.DeepEqual(got, tc.wantTokens) {
				t.Errorf("Tokens() = %v, want %v", got, tc.wantTokens)
			}
			if got := tc.display.Cleared(); !reflect.DeepEqual(got, tc.wantCleared) {
				t.Errorf("Cleared() = %v, want %v", got, tc.wantCleared)
			}
		})
	}
}

func TestPresenterPublishClearsUnsetOptionalTokens(t *testing.T) {
	fake := &recordingPresentation{}
	presenter := &app.Presenter{Presentation: fake}
	// A worker that has lost its task and account: publishing it must delete
	// those tokens, not leave them as a stale sparse patch.
	display := app.AgentDisplay{
		PaneID: "w1:p2", Run: "r1", RunSequence: 1,
		Role: app.RoleImplementer, WorkerSequence: 1, State: "idle",
	}

	if err := presenter.Publish(t.Context(), &display); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if len(fake.metadata) != 1 {
		t.Fatalf("recorded %d reports, want 1", len(fake.metadata))
	}
	got := fake.metadata[0]
	if _, present := got.Tokens["hop_task"]; present {
		t.Errorf("Tokens still writes hop_task for an unset task: %v", got.Tokens)
	}
	if !reflect.DeepEqual(got.Clear, []string{"hop_account", "hop_parent", "hop_task"}) {
		t.Errorf("Clear = %v, want the unset optional tokens deleted", got.Clear)
	}
}

func TestSortDisplaysIsManagerFirstAcrossRuns(t *testing.T) {
	displays := []app.AgentDisplay{
		{PaneID: "w2:p1", Run: "r19", RunSequence: 19, Role: app.RoleImplementer, WorkerSequence: 1},
		{PaneID: "w1:p3", Run: "r18", RunSequence: 18, Role: app.RoleReviewer, WorkerSequence: 2},
		{PaneID: "w1:p1", Run: "r18", RunSequence: 18, Role: app.RoleManager},
		{PaneID: "w2:p0", Run: "r19", RunSequence: 19, Role: app.RoleManager},
		{PaneID: "w1:p2", Run: "r18", RunSequence: 18, Role: app.RoleImplementer, WorkerSequence: 1},
	}

	got := app.SortDisplays(displays)

	var order []string
	for _, d := range got {
		order = append(order, d.PaneID)
	}
	want := []string{"w1:p1", "w1:p2", "w1:p3", "w2:p0", "w2:p1"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want run-grouped and manager-first %v", order, want)
	}
}

func TestSortDisplaysPadsOrderingKeysNumerically(t *testing.T) {
	// A two-digit worker sequence must sort after single-digit ones, which
	// string comparison only gets right when the keys are zero-padded.
	displays := []app.AgentDisplay{
		{PaneID: "p10", Run: "r1", Role: app.RoleImplementer, WorkerSequence: 10},
		{PaneID: "p2", Run: "r1", Role: app.RoleImplementer, WorkerSequence: 2},
		{PaneID: "p0", Run: "r1", Role: app.RoleManager},
	}

	got := app.SortDisplays(displays)

	order := []string{got[0].PaneID, got[1].PaneID, got[2].PaneID}
	want := []string{"p0", "p2", "p10"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v (padded numeric order)", order, want)
	}
}

// recordingPresentation is a handwritten AgentPresentation fake that records
// every port call in order.
type recordingPresentation struct {
	metadata []app.PaneMetadata
	views    []app.ViewSelection
	clears   int
	failNext error
}

func (r *recordingPresentation) ReportMetadata(_ context.Context, metadata app.PaneMetadata) error {
	if r.failNext != nil {
		return r.failNext
	}
	r.metadata = append(r.metadata, metadata)
	return nil
}

func (r *recordingPresentation) SelectView(_ context.Context, selection app.ViewSelection) error {
	if r.failNext != nil {
		return r.failNext
	}
	r.views = append(r.views, selection)
	return nil
}

func (r *recordingPresentation) ClearView(context.Context) error {
	if r.failNext != nil {
		return r.failNext
	}
	r.clears++
	return nil
}

func TestPresenterPublishReportsTokens(t *testing.T) {
	fake := &recordingPresentation{}
	presenter := &app.Presenter{Presentation: fake}
	display := app.AgentDisplay{PaneID: "w1:p1", Run: "r1", RunSequence: 1, Role: app.RoleManager}

	if err := presenter.Publish(t.Context(), &display); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if len(fake.metadata) != 1 {
		t.Fatalf("recorded %d metadata reports, want 1", len(fake.metadata))
	}
	got := fake.metadata[0]
	if got.PaneID != "w1:p1" || got.Tokens["hop_role"] != "manager" {
		t.Errorf("reported metadata = %+v, want pane w1:p1 with the manager role token", got)
	}
}

func TestPresenterFocusAndClear(t *testing.T) {
	fake := &recordingPresentation{}
	presenter := &app.Presenter{Presentation: fake}

	if err := presenter.Focus(t.Context(), "r18", "HOP r18"); err != nil {
		t.Fatalf("Focus: %v", err)
	}
	if err := presenter.Clear(t.Context()); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	if len(fake.views) != 1 || fake.views[0].Run != "r18" || fake.views[0].Label != "HOP r18" {
		t.Errorf("views = %+v, want one selection for run r18", fake.views)
	}
	if fake.clears != 1 {
		t.Errorf("clears = %d, want 1", fake.clears)
	}
}

func TestPresenterPublishPropagatesPortErrors(t *testing.T) {
	sentinel := errors.New("socket closed")
	presenter := &app.Presenter{Presentation: &recordingPresentation{failNext: sentinel}}

	err := presenter.Publish(t.Context(), &app.AgentDisplay{PaneID: "w1:p1"})

	if !errors.Is(err, sentinel) {
		t.Errorf("Publish error = %v, want it to wrap %v", err, sentinel)
	}
}
