package app_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

func TestReconcileState(t *testing.T) {
	cases := []struct {
		name     string
		snapshot []app.PaneObservation
		events   []app.StatusEvent
		want     map[string]app.PaneObservation
	}{
		{
			name: "snapshot with no events is the base state",
			snapshot: []app.PaneObservation{
				{PaneID: "w1:p1", WorkspaceID: "w1", Agent: "claude", Status: app.StatusWorking},
			},
			want: map[string]app.PaneObservation{
				"w1:p1": {PaneID: "w1:p1", WorkspaceID: "w1", Agent: "claude", Status: app.StatusWorking},
			},
		},
		{
			name: "buffered event overrides the snapshot for its pane",
			snapshot: []app.PaneObservation{
				{PaneID: "w1:p1", WorkspaceID: "w1", Agent: "claude", Status: app.StatusWorking},
			},
			events: []app.StatusEvent{
				{PaneID: "w1:p1", WorkspaceID: "w1", Status: app.StatusDone},
			},
			want: map[string]app.PaneObservation{
				"w1:p1": {PaneID: "w1:p1", WorkspaceID: "w1", Agent: "claude", Status: app.StatusDone},
			},
		},
		{
			name: "last event wins per pane",
			events: []app.StatusEvent{
				{PaneID: "w1:p1", WorkspaceID: "w1", Agent: "claude", Status: app.StatusWorking},
				{PaneID: "w1:p1", WorkspaceID: "w1", Status: app.StatusBlocked},
				{PaneID: "w1:p1", WorkspaceID: "w1", Status: app.StatusIdle},
			},
			want: map[string]app.PaneObservation{
				"w1:p1": {PaneID: "w1:p1", WorkspaceID: "w1", Agent: "claude", Status: app.StatusIdle},
			},
		},
		{
			name: "an event for an unsnapshotted pane is added",
			snapshot: []app.PaneObservation{
				{PaneID: "w1:p1", WorkspaceID: "w1", Status: app.StatusIdle},
			},
			events: []app.StatusEvent{
				{PaneID: "w1:p2", WorkspaceID: "w1", Agent: "codex", Status: app.StatusWorking},
			},
			want: map[string]app.PaneObservation{
				"w1:p1": {PaneID: "w1:p1", WorkspaceID: "w1", Status: app.StatusIdle},
				"w1:p2": {PaneID: "w1:p2", WorkspaceID: "w1", Agent: "codex", Status: app.StatusWorking},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := app.ReconcileState(tc.snapshot, tc.events)

			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ReconcileState() = %v, want %v", got, tc.want)
			}
		})
	}
}

// fakeStream is a handwritten StatusStream delivering a fixed event slice.
type fakeStream struct {
	events chan app.StatusEvent
	err    error
	closed bool
}

func newFakeStream(events []app.StatusEvent) *fakeStream {
	channel := make(chan app.StatusEvent, len(events))
	for _, event := range events {
		channel <- event
	}
	close(channel)
	return &fakeStream{events: channel}
}

func (s *fakeStream) Events() <-chan app.StatusEvent { return s.events }
func (s *fakeStream) Err() error                     { return s.err }
func (s *fakeStream) Close() error {
	s.closed = true
	return nil
}

// fakeObserver is a handwritten Observer that opens one prepared stream and
// returns a fixed snapshot. subscribeErr and snapshotErr inject failures.
type fakeObserver struct {
	stream       *fakeStream
	snapshot     []app.PaneObservation
	subscribeErr error
	snapshotErr  error
	subscribed   bool
	// order records whether Subscribe ran before Snapshot.
	order *[]string
}

func (o *fakeObserver) Subscribe(context.Context) (app.StatusStream, error) {
	if o.order != nil {
		*o.order = append(*o.order, "subscribe")
	}
	if o.subscribeErr != nil {
		return nil, o.subscribeErr
	}
	o.subscribed = true
	return o.stream, nil
}

func (o *fakeObserver) Snapshot(context.Context) ([]app.PaneObservation, error) {
	if o.order != nil {
		*o.order = append(*o.order, "snapshot")
	}
	if o.snapshotErr != nil {
		return nil, o.snapshotErr
	}
	return o.snapshot, nil
}

func TestReconcileSubscribesBeforeSnapshot(t *testing.T) {
	var order []string
	observer := &fakeObserver{
		stream:   newFakeStream(nil),
		snapshot: []app.PaneObservation{{PaneID: "w1:p1", Status: app.StatusIdle}},
		order:    &order,
	}

	if _, err := app.Reconcile(t.Context(), observer); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	want := []string{"subscribe", "snapshot"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("call order = %v, want %v; a subscription must open before the snapshot", order, want)
	}
}

func TestReconcileAppliesBufferedEventsOverSnapshot(t *testing.T) {
	observer := &fakeObserver{
		stream: newFakeStream([]app.StatusEvent{
			{PaneID: "w1:p1", WorkspaceID: "w1", Status: app.StatusDone},
			{PaneID: "w1:p2", WorkspaceID: "w1", Agent: "codex", Status: app.StatusWorking},
		}),
		snapshot: []app.PaneObservation{
			{PaneID: "w1:p1", WorkspaceID: "w1", Agent: "claude", Status: app.StatusWorking},
		},
	}

	result, err := app.Reconcile(t.Context(), observer)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if got := result.State["w1:p1"].Status; got != app.StatusDone {
		t.Errorf("w1:p1 status = %s, want the buffered done transition to win over the working snapshot", got)
	}
	if got := result.State["w1:p2"].Agent; got != "codex" {
		t.Errorf("w1:p2 agent = %q, want the buffered event to add the pane", got)
	}
	if len(result.Events) != 2 {
		t.Errorf("observed %d events, want 2", len(result.Events))
	}
	if !observer.stream.closed {
		t.Error("Reconcile did not close the stream")
	}
}

func TestReconcileStopsAtContextCancellation(t *testing.T) {
	// A live stream that never closes: the drain must end when the caller
	// cancels, returning the state reconciled so far.
	stream := &fakeStream{events: make(chan app.StatusEvent)}
	observer := &fakeObserver{stream: stream, snapshot: []app.PaneObservation{{PaneID: "w1:p1", Status: app.StatusIdle}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	result, err := app.Reconcile(ctx, observer)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if got := result.State["w1:p1"].Status; got != app.StatusIdle {
		t.Errorf("w1:p1 status = %s, want the snapshot state", got)
	}
}

func TestReconcileErrors(t *testing.T) {
	t.Run("subscribe failure", func(t *testing.T) {
		sentinel := errors.New("no socket")
		observer := &fakeObserver{subscribeErr: sentinel}

		_, err := app.Reconcile(t.Context(), observer)

		if !errors.Is(err, sentinel) {
			t.Errorf("error = %v, want it to wrap %v", err, sentinel)
		}
	})
	t.Run("snapshot failure", func(t *testing.T) {
		sentinel := errors.New("snapshot refused")
		observer := &fakeObserver{stream: newFakeStream(nil), snapshotErr: sentinel}

		_, err := app.Reconcile(t.Context(), observer)

		if !errors.Is(err, sentinel) {
			t.Errorf("error = %v, want it to wrap %v", err, sentinel)
		}
	})
	t.Run("stream failure surfaces", func(t *testing.T) {
		stream := newFakeStream(nil)
		stream.err = errors.New("connection reset")
		observer := &fakeObserver{stream: stream, snapshot: nil}

		_, err := app.Reconcile(t.Context(), observer)

		if err == nil || !errors.Is(err, stream.err) {
			t.Errorf("error = %v, want it to wrap the stream error", err)
		}
	})
}
