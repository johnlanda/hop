package app_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
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

// fakeStream is a handwritten StatusStream. A closed channel of buffered
// events models a stream that has already ended; a staged stream (see
// newStagedStream) models the adapter's two-stage delivery under cancellation.
type fakeStream struct {
	events    chan app.StatusEvent
	err       error
	closeOnce sync.Once
	stop      chan struct{}
	closed    bool
	overflow  []app.StatusEvent // returned by DrainRemaining
}

func newFakeStream(events []app.StatusEvent) *fakeStream {
	channel := make(chan app.StatusEvent, len(events))
	for _, event := range events {
		channel <- event
	}
	close(channel)
	return &fakeStream{events: channel}
}

// newStagedStream models the real adapter's delivery: events are held in a
// source ("the subscription buffer") and handed one at a time to a consumer
// over an UNBUFFERED channel, then the channel is closed. At any instant no
// event is sitting ready on the channel, so a reader that only takes what is
// immediately ready folds almost nothing; only a reader that drains until the
// channel closes folds them all. This is what distinguishes a correct
// cancellation drain from a non-blocking one.
func newStagedStream(events []app.StatusEvent) *fakeStream {
	s := &fakeStream{events: make(chan app.StatusEvent), stop: make(chan struct{})}
	go func() {
		defer close(s.events)
		for _, event := range events {
			select {
			case s.events <- event:
			case <-s.stop:
				return
			}
		}
	}()
	return s
}

func (s *fakeStream) Events() <-chan app.StatusEvent    { return s.events }
func (s *fakeStream) DrainRemaining() []app.StatusEvent { return s.overflow }
func (s *fakeStream) Err() error                        { return s.err }
func (s *fakeStream) Close() error {
	s.closed = true
	if s.stop != nil {
		s.closeOnce.Do(func() { close(s.stop) })
	}
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

func TestReconcileDrainsBufferedEventsBeforeCancellation(t *testing.T) {
	// A large batch of events is already buffered when the context is
	// canceled. Reconcile must fold every one of them, not drop them because
	// select happened to pick cancellation.
	const count = 1000
	events := make([]app.StatusEvent, count)
	for i := range events {
		status := app.StatusWorking
		if i == count-1 {
			status = app.StatusDone // the final state to observe
		}
		events[i] = app.StatusEvent{PaneID: "w1:p1", WorkspaceID: "w1", Status: status}
	}
	// The events are staged behind an unbuffered channel, as the adapter
	// delivers its subscription buffer; only draining until the stream closes
	// folds them all.
	observer := &fakeObserver{stream: newStagedStream(events), snapshot: []app.PaneObservation{{PaneID: "w1:p1", Status: app.StatusIdle}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel up front; every event was accepted before cancellation

	result, err := app.Reconcile(ctx, observer)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(result.Events) != count {
		t.Errorf("folded %d events, want all %d accepted before cancellation", len(result.Events), count)
	}
	if got := result.State["w1:p1"].Status; got != app.StatusDone {
		t.Errorf("final status = %s, want the last buffered transition done", got)
	}
}

// TestReconcileFoldsDrainRemainingAfterChannelEvents proves the DrainRemaining
// fold itself: with a stream whose channel delivers some events and whose
// DrainRemaining also returns some (a backlog held behind a full channel by
// the real adapter), Reconcile must append the channel events first, then the
// DrainRemaining overflow, on both exit paths. The overflow's transition for
// a pane the channel already touched must win (last writer), and its
// transition for a pane the channel never mentioned must still appear —
// neither is true if the overflow is silently dropped.
func TestReconcileFoldsDrainRemainingAfterChannelEvents(t *testing.T) {
	channelEvents := []app.StatusEvent{
		{PaneID: "w1:p1", WorkspaceID: "w1", Status: app.StatusWorking},
		{PaneID: "w1:p1", WorkspaceID: "w1", Status: app.StatusBlocked},
	}
	overflow := []app.StatusEvent{
		{PaneID: "w1:p1", WorkspaceID: "w1", Status: app.StatusDone},
		{PaneID: "w2:p1", WorkspaceID: "w2", Status: app.StatusWorking},
	}
	snapshot := []app.PaneObservation{{PaneID: "w1:p1", Status: app.StatusIdle}}

	cases := []struct {
		name      string
		newStream func() *fakeStream
		cancel    bool
	}{
		{
			name:      "channel closes without cancellation",
			newStream: func() *fakeStream { return newFakeStream(channelEvents) },
		},
		{
			// The staged stream only hands over one event at a time, as the
			// real adapter does, so this exercises the ctx.Done() branch's
			// own drain loop rather than the plain channel-close branch.
			name:      "context canceled before the channel drains",
			newStream: func() *fakeStream { return newStagedStream(channelEvents) },
			cancel:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := tc.newStream()
			stream.overflow = overflow
			observer := &fakeObserver{stream: stream, snapshot: snapshot}
			ctx := t.Context()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel() // canceled up front; every event was already accepted
			}

			result, err := app.Reconcile(ctx, observer)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			want := append(append([]app.StatusEvent{}, channelEvents...), overflow...)
			if !reflect.DeepEqual(result.Events, want) {
				t.Errorf("Events = %v, want the channel events followed by the DrainRemaining overflow %v", result.Events, want)
			}
			if got := result.State["w1:p1"].Status; got != app.StatusDone {
				t.Errorf("w1:p1 status = %s, want the overflow's done transition to win over the channel's blocked transition", got)
			}
			if got := result.State["w2:p1"].Status; got != app.StatusWorking {
				t.Errorf("w2:p1 status = %s, want the overflow event to add a pane the channel never mentioned", got)
			}
		})
	}
}

func TestReconcileStopsAtContextCancellation(t *testing.T) {
	// A stream with nothing buffered that closes on cancellation: the drain
	// ends immediately, returning the snapshot state.
	observer := &fakeObserver{stream: newStagedStream(nil), snapshot: []app.PaneObservation{{PaneID: "w1:p1", Status: app.StatusIdle}}}
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
