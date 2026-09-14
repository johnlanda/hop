package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/run"
)

// TestDispatchRevalidation proves the section 4 transaction-rule step 2:
// every external mutation is preceded by a fresh heartbeat and a stop-flag
// re-read, so a controller whose lease has moved — or whose run has a stop
// request — never dispatches an act its committed intent would otherwise
// authorize.
func TestDispatchRevalidation(t *testing.T) {
	t.Run("A/B dispatch barrier: an act intended by A is not dispatched after B takes over", func(t *testing.T) {
		tc := newTestController(defaultPolicy())

		// B takes the lease over at exactly A's pre-dispatch revalidation
		// point: after A committed the worktree.create intent, before the
		// CreateWorktree act. A's heartbeat then fails its CAS and the act
		// must never reach the Runtime.
		tookOver := false
		tc.Store.HeartbeatHook = func() {
			if tookOver {
				return
			}
			tookOver = true
			tc.Clock.Advance(leaseTTL + time.Second)
			if _, err := tc.Store.AcquireLease(context.Background(), tc.onlyRunID(t), "controller-B"); err != nil {
				t.Errorf("AcquireLease() (B) error = %v", err)
			}
		}
		created := false
		tc.Runtime.CreateWorktreeFn = func(app.WorktreeRequest) (app.WorktreeInfo, error) {
			created = true
			return app.WorktreeInfo{WorkspaceID: "workspace-1", Path: "/worktrees/w", Branch: "b"}, nil
		}

		_, _, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if !errors.Is(err, app.ErrFenced) {
			t.Fatalf("StartRun() error = %v, want ErrFenced from the pre-dispatch heartbeat", err)
		}
		if created {
			t.Fatalf("CreateWorktree was dispatched despite the superseded lease")
		}

		// The committed intent stays pending for the successor's recovery,
		// never marked failed by the fenced predecessor.
		for _, op := range tc.Store.Operations {
			if op.Kind == app.OpWorktreeCreate && op.State != app.OperationPending {
				t.Fatalf("worktree.create operation state = %s, want pending", op.State)
			}
		}
	})

	t.Run("heartbeat failure cancels an in-flight check command", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := runningRun(t, tc)
		if _, err := tc.Controller.SubmitResult(context.Background(), defaultSubmitRequest(detail)); err != nil {
			t.Fatalf("SubmitResult() error = %v", err)
		}

		started := make(chan struct{})
		canceled := make(chan struct{})
		tc.Commands.CheckExecFn = func(ctx context.Context, _ app.Command) (app.CommandResult, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			return app.CommandResult{}, ctx.Err()
		}

		done := make(chan error, 1)
		go func() {
			_, err := tc.Controller.ClaimAndRunCheck(context.Background(), handle, "/usr/local/bin/hop", "/repo", "/state", []string{"sh", "check.sh"}, false, nil)
			done <- err
		}()
		<-started

		// The lease moves to B while the check runs; A's next heartbeat
		// fails and must cancel the in-flight command's context.
		tc.Clock.Advance(leaseTTL + time.Second)
		if _, err := tc.Store.AcquireLease(context.Background(), detail.RunID, "controller-B"); err != nil {
			t.Fatalf("AcquireLease() (B) error = %v", err)
		}
		if err := tc.Controller.Heartbeat(context.Background(), handle); !errors.Is(err, app.ErrFenced) {
			t.Fatalf("Heartbeat() error = %v, want ErrFenced", err)
		}

		select {
		case <-canceled:
		case <-time.After(5 * time.Second):
			t.Fatalf("in-flight check command was not canceled by the failed heartbeat")
		}
		if err := <-done; err == nil {
			t.Fatalf("ClaimAndRunCheck() succeeded despite the canceled dispatch and fenced lease")
		}
	})

	t.Run("a stop request refuses non-stopping dispatch and preserves the pending intent", func(t *testing.T) {
		tc := newTestController(defaultPolicy())

		opened := false
		tc.Runtime.OpenWorkerPaneFn = func(req app.WorkerPaneRequest) (app.PaneHandle, error) {
			opened = true
			return app.PaneHandle{WorkspaceID: req.WorkspaceID, TabID: "tab-1", PaneID: "pane-1"}, nil
		}
		// The stop request lands between the pane.open intent commit and its
		// dispatch: the first heartbeat revalidation happens before
		// worktree.create, the second before pane.open, so the hook injects
		// the stop at the second one.
		heartbeats := 0
		tc.Store.HeartbeatHook = func() {
			heartbeats++
			if heartbeats != 2 {
				return
			}
			if err := tc.Controller.RequestStop(context.Background(), tc.onlyRunID(t).String()); err != nil {
				t.Errorf("RequestStop() error = %v", err)
			}
		}

		_, _, err := tc.Controller.StartRun(context.Background(), defaultStartRunRequest())
		if !errors.Is(err, app.ErrStopRequested) {
			t.Fatalf("StartRun() error = %v, want ErrStopRequested", err)
		}
		if opened {
			t.Fatalf("OpenWorkerPane was dispatched despite the stop request")
		}
	})
}

// TestDetach proves detach is distinct from stop (docs/plan/phase-2-design.md
// section 5, controller signals): the lease is released with the generation
// preserved, detach evidence is journaled, and the run's state is untouched.
func TestDetach(t *testing.T) {
	tc := newTestController(defaultPolicy())
	handle, detail := runningRun(t, tc)

	if err := tc.Controller.Detach(context.Background(), handle); err != nil {
		t.Fatalf("Detach() error = %v", err)
	}

	leaseRow := tc.Store.Leases[detail.RunID]
	if leaseRow.held {
		t.Fatalf("lease is still held after detach")
	}
	releasedGeneration := leaseRow.lease.Generation
	updated, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus() error = %v", err)
	}
	if updated.State != run.RunRunning {
		t.Fatalf("Run.State = %s after detach, want %s (detach never stops the run)", updated.State, run.RunRunning)
	}

	found := false
	for _, tr := range tc.Store.Transitions {
		if tr.EntityKind == app.EntityRun && tr.Reason == "controller detached; lease released" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no controller-detach transition evidence was journaled")
	}

	// The released lease resumes with the generation preserved and advanced.
	resumed, err := tc.Store.AcquireLease(context.Background(), detail.RunID, "controller-2")
	if err != nil {
		t.Fatalf("AcquireLease() after detach error = %v", err)
	}
	if resumed.Generation != releasedGeneration+1 {
		t.Fatalf("generation after reacquire = %d, want %d", resumed.Generation, releasedGeneration+1)
	}
}
