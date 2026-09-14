package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/johnlanda/hop/internal/app"
)

// Observer implements app.Observer over one server socket for a fixed set of
// watched panes. Agent-status subscriptions are pane-scoped in Herdr, so the
// panes to watch are chosen when the observer is built; the snapshot still
// reports every pane.
type Observer struct {
	client *Client
	panes  []string
}

var _ app.Observer = (*Observer)(nil)

// NewObserver builds an observer that watches the given panes for
// agent-status transitions.
func NewObserver(socketPath string, panes ...string) *Observer {
	return &Observer{client: NewClient(socketPath), panes: panes}
}

// Subscribe opens one agent-status subscription per watched pane and returns
// a stream that normalizes the pushed events into app.StatusEvent values.
// It returns only after the server acknowledges the subscription, so
// transitions from that point on are buffered.
func (o *Observer) Subscribe(ctx context.Context) (app.StatusStream, error) {
	subscriptions := make([]EventSubscription, 0, len(o.panes))
	for _, pane := range o.panes {
		subscriptions = append(subscriptions, EventSubscription{Type: "pane.agent_status_changed", PaneID: pane})
	}
	stream, err := o.client.Subscribe(ctx, subscriptions)
	if err != nil {
		return nil, err
	}
	return &statusStream{stream: stream, closed: make(chan struct{})}, nil
}

// statusStream adapts a raw event stream into an app.StatusStream, decoding
// each agent-status event and dropping any other pushed frame.
type statusStream struct {
	stream    *EventStream
	events    chan app.StatusEvent
	start     sync.Once
	closeOnce sync.Once
	closed    chan struct{} // closed by Close to release a pump blocked on a full events channel
}

// Events lazily starts the decode pump and returns the normalized channel.
func (s *statusStream) Events() <-chan app.StatusEvent {
	s.start.Do(func() {
		s.events = make(chan app.StatusEvent)
		go s.pump()
	})
	return s.events
}

// pump decodes raw events into status events until the raw stream ends,
// abandoning a delivery when Close is called so a consumer that stops reading
// cannot leave this goroutine blocked on the channel.
func (s *statusStream) pump() {
	defer close(s.events)
	for raw := range s.stream.Events() {
		if !isAgentStatusEvent(raw.Name) {
			continue
		}
		event, ok := decodeStatusEvent(raw.Data)
		if !ok {
			continue
		}
		select {
		case s.events <- event:
		case <-s.closed:
			return
		}
	}
}

// Err reports why the underlying stream ended.
func (s *statusStream) Err() error {
	return s.stream.Err()
}

// Close ends the underlying stream and releases a blocked pump.
func (s *statusStream) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return s.stream.Close()
}

// isAgentStatusEvent reports whether an event name is an agent-status change,
// accepting both the dot form of scoped subscription events and the
// underscore form of lifecycle events.
func isAgentStatusEvent(name string) bool {
	return name == "pane.agent_status_changed" || name == "pane_agent_status_changed"
}

// paneAgentStatusChanged is the wire shape of an agent-status event.
type paneAgentStatusChanged struct {
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	Agent       string `json:"agent"`
	AgentStatus string `json:"agent_status"`
}

// decodeStatusEvent decodes one agent-status event payload into a status
// event, reporting whether it was well-formed.
func decodeStatusEvent(data json.RawMessage) (app.StatusEvent, bool) {
	var wire paneAgentStatusChanged
	if err := json.Unmarshal(data, &wire); err != nil || wire.PaneID == "" {
		return app.StatusEvent{}, false
	}
	return app.StatusEvent{
		PaneID:      wire.PaneID,
		WorkspaceID: wire.WorkspaceID,
		Agent:       wire.Agent,
		Status:      app.AgentStatus(wire.AgentStatus),
	}, true
}

// snapshotPane is the subset of a session.snapshot pane record the observer
// reduces to an observation.
type snapshotPane struct {
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	Agent       string `json:"agent"`
	AgentStatus string `json:"agent_status"`
}

// sessionSnapshotResult is the session.snapshot result: the snapshot is
// nested under a "snapshot" key, alongside the result "type".
type sessionSnapshotResult struct {
	Snapshot struct {
		Panes []snapshotPane `json:"panes"`
	} `json:"snapshot"`
}

// Snapshot reads the current per-pane observation set from session.snapshot.
func (o *Observer) Snapshot(ctx context.Context) ([]app.PaneObservation, error) {
	var result sessionSnapshotResult
	if err := o.client.Call(ctx, "session.snapshot", nil, &result); err != nil {
		return nil, fmt.Errorf("read session snapshot: %w", err)
	}
	observations := make([]app.PaneObservation, 0, len(result.Snapshot.Panes))
	for _, pane := range result.Snapshot.Panes {
		observations = append(observations, app.PaneObservation{
			PaneID:      pane.PaneID,
			WorkspaceID: pane.WorkspaceID,
			Agent:       pane.Agent,
			Status:      app.AgentStatus(pane.AgentStatus),
		})
	}
	return observations, nil
}
