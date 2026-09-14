package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/herdr"
	"github.com/johnlanda/hop/internal/app"
)

// TestRealProcessEventObservationReconcile proves the no-replay
// reconciliation against a real server: it subscribes to a fixture pane's
// agent-status transitions before it snapshots, then drives a working→idle
// transition through the public API and shows the buffered transition
// reconciled over the snapshot. A snapshot alone would still read "working".
func TestRealProcessEventObservationReconcile(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)

	fixture := server.createRunFixture(t)
	observer := herdr.NewObserver(server.socketPath, fixture.implementer)

	// Subscribe first so the transition is buffered; snapshot second.
	stream, err := observer.Subscribe(testContext(t))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		if closeErr := stream.Close(); closeErr != nil {
			t.Logf("close status stream: %v", closeErr)
		}
	}()
	snapshot, err := observer.Snapshot(testContext(t))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if statusOfPane(snapshot, fixture.implementer) != app.StatusWorking {
		t.Fatalf("snapshot implementer status = %s, want working", statusOfPane(snapshot, fixture.implementer))
	}

	// Drive the transition the reconciliation must capture.
	server.reportAgent(t, fixture.implementer, "implementer", "idle")

	event := awaitStatusEvent(t, stream, fixture.implementer, app.StatusIdle)
	state := app.ReconcileState(snapshot, []app.StatusEvent{event})
	if got := state[fixture.implementer].Status; got != app.StatusIdle {
		t.Errorf("reconciled implementer status = %s, want the buffered idle transition over the working snapshot", got)
	}
	if got := state[fixture.manager].Status; got != app.StatusIdle {
		t.Errorf("manager status = %s, want its snapshot idle preserved", got)
	}
}

// TestRealProcessReconcileLoopFoldsLiveTransitions exercises app.Reconcile
// end to end against the real normalized adapter: it subscribes first, then
// snapshots, then folds real agent-status transitions delivered while it runs,
// and stops on cancellation with those transitions folded. The other
// observation test folds a snapshot and one event directly; this one drives
// the Reconcile loop itself.
func TestRealProcessReconcileLoopFoldsLiveTransitions(t *testing.T) {
	server := prepareServer(t, newArtifactDir(t))
	server.start(t)
	fixture := server.createRunFixture(t)
	observer := herdr.NewObserver(server.socketPath, fixture.implementer)

	ctx, cancel := context.WithCancel(testContext(t))
	type outcome struct {
		result app.Reconciliation
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := app.Reconcile(ctx, observer)
		done <- outcome{result: result, err: err}
	}()

	// Drive real transitions after Reconcile has started, confirming each one
	// landed on the server before driving the next, so the subscription
	// certainly observes them.
	states := []app.AgentStatus{app.StatusIdle, app.StatusWorking, app.StatusIdle}
	for _, want := range states {
		server.reportAgent(t, fixture.implementer, "implementer", string(want))
		if !waitUntil(func() bool { return server.agentStatus(t, fixture.implementer) == want }) {
			t.Fatalf("server never reported the implementer as %s", want)
		}
	}
	cancel()

	got := <-done
	if got.err != nil {
		t.Fatalf("Reconcile: %v", got.err)
	}
	if len(got.result.Events) == 0 {
		t.Error("Reconcile folded no live transitions; the loop did not observe the subscription")
	}
	if status := got.result.State[fixture.implementer].Status; status != app.StatusIdle && status != app.StatusWorking {
		t.Errorf("reconciled implementer status = %q, want a real observed status", status)
	}
}

// TestRealProcessOptionalMetadataClears proves that publishing a display with
// an optional field now unset removes that token in Herdr, rather than
// leaving the stale value from an earlier publish.
func TestRealProcessOptionalMetadataClears(t *testing.T) {
	server := prepareServer(t, newArtifactDir(t))
	stage := stagePlugin(t)
	server.start(t)
	server.call(t, "plugin.link", map[string]any{"path": stage, "enabled": true}, &struct{}{})

	fixture := server.createRunFixture(t)
	presenter := &app.Presenter{Presentation: herdr.NewPresentation(server.socketPath)}

	// Publish with a task and account set.
	set := app.AgentDisplay{
		PaneID: fixture.implementer, Run: "r1", RunSequence: 1,
		Role: app.RoleImplementer, WorkerSequence: 1,
		Task: "old task", Account: "old account", ParentLabel: "manager-r1",
	}
	if err := presenter.Publish(testContext(t), &set); err != nil {
		t.Fatalf("publish set: %v", err)
	}
	tokens := server.agentTokens(t)[fixture.implementer]
	if tokens["hop_task"] != "old task" || tokens["hop_account"] != "old account" {
		t.Fatalf("after set, tokens = %v, want the task and account present", tokens)
	}

	// Publish the same pane with those fields now unset; they must be removed.
	unset := app.AgentDisplay{
		PaneID: fixture.implementer, Run: "r1", RunSequence: 1,
		Role: app.RoleImplementer, WorkerSequence: 1,
	}
	if err := presenter.Publish(testContext(t), &unset); err != nil {
		t.Fatalf("publish unset: %v", err)
	}
	cleared := server.agentTokens(t)[fixture.implementer]
	if _, present := cleared["hop_task"]; present {
		t.Errorf("hop_task still present after unset: %v", cleared)
	}
	if _, present := cleared["hop_account"]; present {
		t.Errorf("hop_account still present after unset: %v", cleared)
	}
	if _, present := cleared["hop_parent"]; present {
		t.Errorf("hop_parent still present after unset: %v", cleared)
	}
	if cleared["hop_role"] != "implementer" {
		t.Errorf("hop_role = %q, want it still set", cleared["hop_role"])
	}
}

// TestRealProcessContextInjection proves HOP can inject context into a live
// process: it sends text to a running shell pane and reads the same text
// back from that pane. Injection is HOP's message-delivery transport; this
// shows the injected text reaches the process, not that it was acted on.
func TestRealProcessContextInjection(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)

	var workspace struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	server.call(t, "workspace.create", map[string]any{"cwd": server.workDir(), "focus": true}, &workspace)

	marker := "HOP-CONTEXT-9f3a"
	server.call(t, "pane.send_text", map[string]any{
		"pane_id": workspace.RootPane.PaneID,
		"text":    "printf 'INJECTED:%s\\n' " + marker + "\n",
	}, nil)

	snapshot := server.waitForPaneText(t, workspace.RootPane.PaneID, "INJECTED:"+marker)
	artifacts.save(t, "context-injection-pane.txt", snapshot)
}

// statusOfPane returns the observed status of one pane in a snapshot.
func statusOfPane(snapshot []app.PaneObservation, paneID string) app.AgentStatus {
	for _, observation := range snapshot {
		if observation.PaneID == paneID {
			return observation.Status
		}
	}
	return ""
}

// awaitStatusEvent reads status events until one matches the wanted pane and
// status, with a bounded deadline.
func awaitStatusEvent(t *testing.T, stream app.StatusStream, paneID string, want app.AgentStatus) app.StatusEvent {
	t.Helper()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case event, ok := <-stream.Events():
			if !ok {
				t.Fatalf("status stream ended before %s reached %s: %v", paneID, want, stream.Err())
			}
			if event.PaneID == paneID && event.Status == want {
				return event
			}
		case <-deadline:
			t.Fatalf("no %s transition for %s before the deadline", want, paneID)
		}
	}
}

// waitForPaneText polls a pane's recent output until it contains want, then
// returns the snapshot; it fails on timeout with the last snapshot.
func (s *testServer) waitForPaneText(t *testing.T, paneID, want string) string {
	t.Helper()
	var snapshot string
	found := waitUntil(func() bool {
		snapshot = s.readPane(t, paneID)
		return strings.Contains(snapshot, want)
	})
	if !found {
		t.Fatalf("pane %s never showed %q; last snapshot:\n%s", paneID, want, snapshot)
	}
	return snapshot
}
