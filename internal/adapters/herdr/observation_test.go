package herdr_test

import (
	"bufio"
	"fmt"
	"net"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/herdr"
	"github.com/johnlanda/hop/internal/app"
)

func TestObserverSnapshot(t *testing.T) {
	endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
		request := readRequestLine(t, bufio.NewReader(conn))
		if request == nil {
			return
		}
		if request["method"] != "session.snapshot" {
			t.Errorf("method = %v, want session.snapshot", request["method"])
		}
		writeLine(t, conn, fmt.Sprintf(`{"id":%q,"result":{"type":"session_snapshot","snapshot":{"version":"0.9.0","protocol":22,"workspaces":[],"tabs":[],"layouts":[],"agents":[],"panes":[`+
			`{"pane_id":"w1:p1","terminal_id":"t1","workspace_id":"w1","tab_id":"w1:t1","focused":true,"revision":1,"agent":"claude","agent_status":"working"},`+
			`{"pane_id":"w1:p2","terminal_id":"t2","workspace_id":"w1","tab_id":"w1:t1","focused":false,"revision":1,"agent_status":"idle"}`+
			`]}}}`, requestID(t, request)))
	})
	observer := herdr.NewObserver(endpoint.socketPath, "w1:p1")

	observations, err := observer.Snapshot(testContext(t))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if len(observations) != 2 {
		t.Fatalf("observed %d panes, want 2", len(observations))
	}
	if observations[0].PaneID != "w1:p1" || observations[0].Status != app.StatusWorking || observations[0].Agent != "claude" {
		t.Errorf("first observation = %+v, want w1:p1 claude working", observations[0])
	}
	if observations[1].Status != app.StatusIdle {
		t.Errorf("second observation status = %s, want idle", observations[1].Status)
	}
}

func TestObserverSubscribeNormalizesStatusEvents(t *testing.T) {
	endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
		reader := bufio.NewReader(conn)
		request := readRequestLine(t, reader)
		if request == nil {
			return
		}
		id := requestID(t, request)
		writeLine(t, conn, fmt.Sprintf(`{"id":%q,"result":{"type":"subscription_started"}}`, id))
		// A scoped status event (dot form), an unrelated scroll event that
		// must be dropped, and a lifecycle status event (underscore form).
		writeLine(t, conn, `{"event":"pane.agent_status_changed","data":{"pane_id":"w1:p1","workspace_id":"w1","agent":"claude","agent_status":"working","future":1}}`)
		writeLine(t, conn, `{"event":"pane.scroll_changed","data":{"pane_id":"w1:p1"}}`)
		writeLine(t, conn, `{"event":"pane_agent_status_changed","data":{"pane_id":"w1:p1","workspace_id":"w1","agent_status":"done"}}`)
	})
	observer := herdr.NewObserver(endpoint.socketPath, "w1:p1")

	stream, err := observer.Subscribe(testContext(t))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer closeQuietly(stream)

	first := <-stream.Events()
	if first.PaneID != "w1:p1" || first.Status != app.StatusWorking || first.Agent != "claude" {
		t.Errorf("first event = %+v, want w1:p1 claude working", first)
	}
	second := <-stream.Events()
	if second.Status != app.StatusDone {
		t.Errorf("second event status = %s, want done (the scroll event must be dropped)", second.Status)
	}
}

func TestObserverSubscribeRequestsEachPane(t *testing.T) {
	got := make(chan map[string]any, 1)
	endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
		request := readRequestLine(t, bufio.NewReader(conn))
		if request == nil {
			return
		}
		got <- request
		writeLine(t, conn, fmt.Sprintf(`{"id":%q,"result":{"type":"subscription_started"}}`, requestID(t, request)))
	})
	observer := herdr.NewObserver(endpoint.socketPath, "w1:p1", "w1:p2")

	stream, err := observer.Subscribe(testContext(t))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer closeQuietly(stream)

	request := <-got
	if request["method"] != "events.subscribe" {
		t.Fatalf("method = %v, want events.subscribe", request["method"])
	}
	params, ok := request["params"].(map[string]any)
	if !ok {
		t.Fatalf("no params object: %v", request)
	}
	subscriptions, ok := params["subscriptions"].([]any)
	if !ok || len(subscriptions) != 2 {
		t.Fatalf("subscriptions = %v, want one per watched pane", params["subscriptions"])
	}
}

// TestObserverFeedsReconcileState proves the adapter's Subscribe and Snapshot
// satisfy the reconciliation contract: subscribing first, then snapshotting,
// then folding the buffered event onto the snapshot yields the transition
// overriding the stale snapshot state. app.Reconcile's own drain loop is
// covered in the app package against fakes; the live end-to-end reconcile
// runs in the real-process suite.
func TestObserverFeedsReconcileState(t *testing.T) {
	endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
		reader := bufio.NewReader(conn)
		for {
			request := readRequestLoop(t, reader)
			if request == nil {
				return
			}
			id := requestID(t, request)
			switch request["method"] {
			case "events.subscribe":
				writeLine(t, conn, fmt.Sprintf(`{"id":%q,"result":{"type":"subscription_started"}}`, id))
				writeLine(t, conn, `{"event":"pane.agent_status_changed","data":{"pane_id":"w1:p1","workspace_id":"w1","agent_status":"done"}}`)
			case "session.snapshot":
				writeLine(t, conn, fmt.Sprintf(`{"id":%q,"result":{"type":"session_snapshot","snapshot":{"version":"0.9.0","protocol":22,"workspaces":[],"tabs":[],"layouts":[],"agents":[],"panes":[{"pane_id":"w1:p1","terminal_id":"t1","workspace_id":"w1","tab_id":"w1:t1","focused":true,"revision":1,"agent":"claude","agent_status":"working"}]}}}`, id))
			default:
				writeLine(t, conn, fmt.Sprintf(`{"id":%q,"result":{"type":"ok"}}`, id))
			}
		}
	})
	observer := herdr.NewObserver(endpoint.socketPath, "w1:p1")
	ctx := testContext(t)

	stream, err := observer.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer closeQuietly(stream)
	snapshot, err := observer.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	event := <-stream.Events()

	state := app.ReconcileState(snapshot, []app.StatusEvent{event})

	if state["w1:p1"].Status != app.StatusDone {
		t.Errorf("reconciled status = %s, want the buffered done transition over the working snapshot", state["w1:p1"].Status)
	}
	if state["w1:p1"].Agent != "claude" {
		t.Errorf("reconciled agent = %q, want claude carried from the snapshot", state["w1:p1"].Agent)
	}
}
