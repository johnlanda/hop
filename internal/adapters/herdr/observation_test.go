package herdr_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"testing"
	"time"

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

// normalizedPumpSymbol is the goroutine the barrier tests watch for the
// normalized (statusStream) pump.
const normalizedPumpSymbol = "(*statusStream).pump"

// subscribeAndStartPump subscribes and starts the decode pump without reading
// it, so the pump parks blocked on its first delivery.
func subscribeAndStartPump(ctx context.Context, t *testing.T) app.StatusStream {
	t.Helper()
	//nolint:contextcheck // the fake endpoint's lifetime is bound to t.Cleanup, not to the subscription context passed to Subscribe below.
	endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
		request := readRequestLine(t, bufio.NewReader(conn))
		if request == nil {
			return
		}
		writeLine(t, conn, fmt.Sprintf(`{"id":%q,"result":{"type":"subscription_started"}}`, requestID(t, request)))
		// Flood past both the raw and the normalized buffer so, with no
		// consumer, the normalized pump parks blocked in its delivery select.
		floodLines(conn, `{"event":"pane.agent_status_changed","data":{"pane_id":"w1:p1","workspace_id":"w1","agent_status":"working"}}`, 600)
		holdUntilPeerCloses(conn)
	})
	stream, err := herdr.NewObserver(endpoint.socketPath, "w1:p1").Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	stream.Events() // start the pump; deliberately never read it
	return stream
}

// pumpDone type-asserts a stream to its pump-completion signal.
func pumpDone(t *testing.T, stream app.StatusStream) <-chan struct{} {
	t.Helper()
	done, ok := stream.(interface{ Done() <-chan struct{} })
	if !ok {
		t.Fatalf("stream %T has no Done() signal", stream)
	}
	return done.Done()
}

func TestObserverCloseReleasesBlockedPump(t *testing.T) {
	// With no consumer the normalized pump fills its buffer and parks blocked
	// in its delivery select. Close must release it. The test never reads the
	// events channel, so it cannot itself release the leak.
	stream := subscribeAndStartPump(testContext(t), t)
	waitParkedInSelect(t, normalizedPumpSymbol)

	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	assertGoroutineGone(t, normalizedPumpSymbol)
	select {
	case <-pumpDone(t, stream):
	case <-time.After(barrierTimeout):
		t.Fatal("the normalized pump did not finish after Close")
	}
}

func TestObserverCancellationReleasesBlockedPump(t *testing.T) {
	// Cancellation WITHOUT Close must also release the blocked normalized
	// pump, so its send select honors the subscription context, not only the
	// Close signal.
	ctx, cancel := context.WithCancel(testContext(t))
	stream := subscribeAndStartPump(ctx, t)
	defer closeQuietly(stream)
	waitParkedInSelect(t, normalizedPumpSymbol)

	cancel()

	assertGoroutineGone(t, normalizedPumpSymbol)
	select {
	case <-pumpDone(t, stream):
	case <-time.After(barrierTimeout):
		t.Fatal("the normalized pump did not finish after cancellation")
	}
}

// barrierObserver wraps a real Observer and cancels the context the moment
// the raw read pump is parked in its send select — i.e. the subscription
// buffer is full and every buffered event predates cancellation. It cancels
// from Snapshot, before Reconcile begins draining, so the cutoff is exact.
type barrierObserver struct {
	inner  *herdr.Observer
	t      *testing.T
	cancel context.CancelFunc
}

func (o barrierObserver) Subscribe(ctx context.Context) (app.StatusStream, error) {
	return o.inner.Subscribe(ctx)
}

func (o barrierObserver) Snapshot(ctx context.Context) ([]app.PaneObservation, error) {
	observations, err := o.inner.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	waitParkedInSelect(o.t, "(*EventStream).send")
	o.cancel()
	return observations, nil
}

// TestReconcileFoldsRawBufferedEventsOnCancellation proves R4 against the real
// Observer, Client and Reconcile: with more events accepted into the raw
// subscription buffer than the normalized channel can hold ready, cancellation
// must still fold every accepted event before returning. A drain that only
// takes what is ready on the normalized channel returns zero.
func TestReconcileFoldsRawBufferedEventsOnCancellation(t *testing.T) {
	endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
		reader := bufio.NewReader(conn)
		for {
			request := readRequestLoop(t, reader)
			if request == nil {
				return
			}
			id := requestID(t, request)
			if request["method"] == "session.snapshot" {
				writeLine(t, conn, fmt.Sprintf(`{"id":%q,"result":{"type":"session_snapshot","snapshot":{"panes":[]}}}`, id))
				continue
			}
			writeLine(t, conn, fmt.Sprintf(`{"id":%q,"result":{"type":"subscription_started"}}`, id))
			// Flood well past the raw buffer capacity; the read pump accepts
			// eventBufferSize (256) and parks on the next send.
			floodLines(conn, `{"event":"pane.agent_status_changed","data":{"pane_id":"w1:p1","workspace_id":"w1","agent_status":"done"}}`, 400)
			holdUntilPeerCloses(conn)
		}
	})
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	observer := barrierObserver{inner: herdr.NewObserver(endpoint.socketPath, "w1:p1"), t: t, cancel: cancel}

	result, err := app.Reconcile(ctx, observer)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(result.Events) < 256 {
		t.Errorf("Reconcile folded %d events, want the >=256 accepted into the raw buffer before cancellation", len(result.Events))
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
