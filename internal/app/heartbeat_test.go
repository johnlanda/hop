package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// TestHeartbeatUnderCallerCancellation pins the line between lease loss
// and the caller's own cancellation: a heartbeat refused only because the
// caller's context ended leaves the handle's dispatch scope live, so the
// acts that follow still dispatch, while a heartbeat the store refused for
// the lease ends it.
func TestHeartbeatUnderCallerCancellation(t *testing.T) {
	t.Run("a canceled caller context leaves the dispatch scope live", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, _ := runningRun(t, tc)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := tc.Controller.Heartbeat(ctx, handle); !errors.Is(err, context.Canceled) {
			t.Fatalf("Heartbeat() under a canceled context error = %v, want the cancellation", err)
		}
		if !app.DispatchLiveForTest(handle) {
			t.Fatalf("the caller's own cancellation ended the handle's dispatch scope")
		}
		if err := tc.Controller.Heartbeat(context.Background(), handle); err != nil {
			t.Fatalf("the next Heartbeat() error = %v, want the lease still held", err)
		}
	})

	t.Run("a lost lease ends the dispatch scope", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		tc.Clock.Advance(leaseTTL + time.Second)
		if _, err := tc.Store.AcquireLease(context.Background(), detail.RunID, "controller-B"); err != nil {
			t.Fatalf("AcquireLease() (B) error = %v", err)
		}

		if err := tc.Controller.Heartbeat(context.Background(), handle); !errors.Is(err, app.ErrFenced) {
			t.Fatalf("Heartbeat() error = %v, want ErrFenced", err)
		}
		if app.DispatchLiveForTest(handle) {
			t.Fatalf("a fenced heartbeat left the handle's dispatch scope live")
		}
	})
}
