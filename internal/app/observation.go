package app

import (
	"context"
	"fmt"
)

// AgentStatus is the effective lifecycle status Herdr reports for an agent
// pane. It is display/observation input, not a HOP workflow state.
type AgentStatus string

// The effective agent lifecycle statuses Herdr reports.
const (
	StatusIdle    AgentStatus = "idle"
	StatusWorking AgentStatus = "working"
	StatusBlocked AgentStatus = "blocked"
	StatusDone    AgentStatus = "done"
	StatusUnknown AgentStatus = "unknown"
)

// PaneObservation is the observed lifecycle state of one pane. It is what a
// snapshot row and a status event both reduce to.
type PaneObservation struct {
	PaneID      string
	WorkspaceID string
	Agent       string
	Status      AgentStatus
}

// StatusEvent is one observed agent-status transition. Agent is empty when
// the event does not name the occupant.
type StatusEvent struct {
	PaneID      string
	WorkspaceID string
	Agent       string
	Status      AgentStatus
}

// StatusStream is an open subscription to agent-status transitions. Events
// are delivered in arrival order until the channel closes; Err then reports
// why, with nil meaning a deliberate close.
type StatusStream interface {
	Events() <-chan StatusEvent
	Close() error
	Err() error
}

// Observer opens agent-status subscriptions and reads bootstrap snapshots.
// The Herdr adapter implements it. Subscriptions do not replay events retained
// before acknowledgement, so a caller must subscribe before it snapshots.
type Observer interface {
	// Subscribe opens a status stream and returns only after the server has
	// acknowledged it, so events from that point on are buffered.
	Subscribe(ctx context.Context) (StatusStream, error)
	// Snapshot returns the current per-pane observation set.
	Snapshot(ctx context.Context) ([]PaneObservation, error)
}

// closeStream closes a status stream, discarding the close result: the
// reconciliation is already done, and the stream's own Err reports a
// premature end.
func closeStream(stream StatusStream) {
	_ = stream.Close() //nolint:errcheck // best-effort close; the stream's Err already reports a premature end.
}

// ReconcileState folds an ordered event sequence onto a snapshot base and
// returns the current per-pane observation. The snapshot seeds the state;
// each event then overrides its pane, last writer winning. This is the pure
// core of reconciliation: no subscription replay is available, so a caller
// subscribes first, snapshots second, and applies the buffered events here.
func ReconcileState(snapshot []PaneObservation, events []StatusEvent) map[string]PaneObservation {
	state := make(map[string]PaneObservation, len(snapshot))
	for _, observation := range snapshot {
		state[observation.PaneID] = observation
	}
	for _, event := range events {
		current := state[event.PaneID]
		current.PaneID = event.PaneID
		if event.WorkspaceID != "" {
			current.WorkspaceID = event.WorkspaceID
		}
		if event.Agent != "" {
			current.Agent = event.Agent
		}
		current.Status = event.Status
		state[event.PaneID] = current
	}
	return state
}

// Reconciliation is the settled result of a reconcile: the per-pane state and
// the events that were observed while producing it, in arrival order.
type Reconciliation struct {
	State  map[string]PaneObservation
	Events []StatusEvent
}

// Reconcile performs the no-replay reconciliation against a live Observer: it
// subscribes first so transitions are buffered, takes the snapshot base,
// then drains every event observed up to the caller's context deadline or an
// explicit stream end. The buffered events override the snapshot, so a
// transition between subscribe and snapshot is not lost. The caller bounds
// the drain by canceling ctx; that is the intended way to end a live watch.
func Reconcile(ctx context.Context, observer Observer) (Reconciliation, error) {
	stream, err := observer.Subscribe(ctx)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("subscribe before snapshot: %w", err)
	}
	defer closeStream(stream)

	snapshot, err := observer.Snapshot(ctx)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("snapshot after subscribe: %w", err)
	}

	var events []StatusEvent
	for {
		// Prefer an already-buffered event over cancellation, so a
		// transition that arrived before the context was canceled is folded
		// in rather than dropped by select's random choice among ready cases.
		select {
		case event, ok := <-stream.Events():
			if !ok {
				if streamErr := stream.Err(); streamErr != nil {
					return Reconciliation{}, fmt.Errorf("status stream ended: %w", streamErr)
				}
				return Reconciliation{State: ReconcileState(snapshot, events), Events: events}, nil
			}
			events = append(events, event)
			continue
		default:
		}

		select {
		case <-ctx.Done():
			// Drain the events already buffered at cancellation before
			// returning, so none observed before cancellation is lost.
			events = drainBuffered(stream, events)
			return Reconciliation{State: ReconcileState(snapshot, events), Events: events}, nil
		case event, ok := <-stream.Events():
			if !ok {
				if streamErr := stream.Err(); streamErr != nil {
					return Reconciliation{}, fmt.Errorf("status stream ended: %w", streamErr)
				}
				return Reconciliation{State: ReconcileState(snapshot, events), Events: events}, nil
			}
			events = append(events, event)
		}
	}
}

// drainBuffered appends every event already sitting in the stream's channel,
// without blocking, so cancellation does not drop events that had already
// been delivered.
func drainBuffered(stream StatusStream, events []StatusEvent) []StatusEvent {
	for {
		select {
		case event, ok := <-stream.Events():
			if !ok {
				return events
			}
			events = append(events, event)
		default:
			return events
		}
	}
}
