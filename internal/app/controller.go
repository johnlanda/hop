package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// ErrStopRequested reports that an external act was refused at pre-dispatch
// revalidation because the run has a stop request: unstarted work is never
// started after stop (docs/plan/phase-2-design.md sections 4-5).
var ErrStopRequested = errors.New("app: run has a stop request")

// Controller is HOP's run-controller application service: the driving API
// cmd/hop calls to implement hop run, hop status, hop stop, hop resume, hop
// result submit and the controller side of hop check-exec (claiming and
// spawning one check execution). Every exported method takes and returns
// primitives or Controller-defined DTOs; composition never imports domain
// or identity types (docs/plan/phase-2-design.md section 8).
type Controller struct {
	Store       StateStore
	Read        ReadStore
	Submissions SubmissionStore
	Runtime     Runtime
	Artifacts   ArtifactStore
	Clock       Clock
	IDs         IDGenerator
	Commands    CommandRunner
	Groups      ProcessGroupInspector
	Config      ConfigurationSource
}

// RunHandle is an opaque token identifying one run and the lease a
// controller holds on it, returned by StartRun and Resume and consumed by
// every other use-case method that keeps acting on the same run within one
// controller process. Composition holds it opaquely; it never needs to
// import the identity package to do so. Handles returned by StartRun and
// Resume carry a dispatch scope: the cancelable context every external act
// for the run derives from, canceled when a heartbeat fails or the
// controller detaches so in-flight external calls are canceled with it.
type RunHandle struct {
	runID    identity.RunID
	lease    Lease
	dispatch *dispatchState
}

// RunID renders the handle's run identity as a string, for display only.
func (h *RunHandle) RunID() string { return h.runID.String() }

// dispatchState is one controller process's cancelable dispatch scope for
// one run. It is shared by every copy of the RunHandle that created it.
type dispatchState struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// newRunHandle builds a handle with a fresh dispatch scope.
func newRunHandle(runID identity.RunID, lease Lease) RunHandle {
	ctx, cancel := context.WithCancel(context.Background())
	return RunHandle{runID: runID, lease: lease, dispatch: &dispatchState{ctx: ctx, cancel: cancel}}
}

// cancelDispatch cancels the handle's dispatch scope, if it has one.
func (h *RunHandle) cancelDispatch() {
	if h.dispatch != nil {
		h.dispatch.cancel()
	}
}

// actContext derives the context an external act runs under: the caller's
// ctx, additionally canceled when the handle's dispatch scope is canceled
// (a failed heartbeat, or detach). The returned release func must be
// called once the act returns; it detaches the link without canceling the
// caller's own ctx.
func (h *RunHandle) actContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if h.dispatch == nil {
		return ctx, func() {}
	}
	merged, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(h.dispatch.ctx, cancel)
	return merged, func() { stop(); cancel() }
}

// Heartbeat extends the handle's lease TTL through the store's CAS
// contract (run, controller, generation, held). Composition calls it on
// the design's 10s interval so a long check or wait never outlives the 30s
// TTL. A failed heartbeat cancels the handle's dispatch scope — in-flight
// external calls are canceled — and the caller must stop acting
// (docs/plan/phase-2-design.md section 4).
func (c *Controller) Heartbeat(ctx context.Context, handle RunHandle) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; heartbeat runs on a 10s interval, never a hot loop.
	if err := c.Store.Heartbeat(ctx, handle.lease); err != nil {
		handle.cancelDispatch()
		return fmt.Errorf("app: heartbeat: %w", err)
	}
	return nil
}

// Detach releases the run without stopping it (docs/plan/phase-2-design.md
// section 5, controller signals): journal the detach as transition
// evidence, cancel in-flight external calls, and release the lease
// (CAS to released, generation preserved). The worker keeps running and
// the run keeps its state; only `hop stop` ever stops a run.
func (c *Controller) Detach(ctx context.Context, handle RunHandle) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per controller shutdown.
	now := c.Clock.Now()
	journalErr := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		r, _, err := uow.Runs().Get(ctx, handle.runID)
		if err != nil {
			return err
		}
		return recordTransition(ctx, uow, EntityRun, handle.runID.String(), string(r.State), string(r.State), "controller detached; lease released", gen(handle.lease.Generation), now)
	})
	handle.cancelDispatch()
	if err := c.Store.ReleaseLease(ctx, handle.lease); err != nil {
		return fmt.Errorf("app: release lease on detach: %w", err)
	}
	return journalErr
}

// revalidateForDispatch is the section 4 transaction-rule step 2
// revalidation, applied immediately before every external mutation:
// heartbeat fresh (the CAS verifies run, controller, generation and held),
// and — unless the act is itself part of stopping — the stop flag re-read
// under a fenced unit of work. Any failure means the act must not be
// dispatched; an already-committed intent stays pending for recovery per
// the operation decision table.
func (c *Controller) revalidateForDispatch(ctx context.Context, handle RunHandle, actIsStopping bool) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per external act.
	if err := c.Heartbeat(ctx, handle); err != nil {
		return err
	}
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		r, _, err := uow.Runs().Get(ctx, handle.runID)
		if err != nil {
			return err
		}
		if !actIsStopping && r.StopRequested {
			return fmt.Errorf("%w: run %s", ErrStopRequested, handle.runID)
		}
		return nil
	})
}

// observeServerInstance reads the server-process identity behind the
// configured socket, folding an observation failure into "" (unknown):
// unknown continuity is ambiguous evidence, never an error that blocks
// the surrounding flow.
func (c *Controller) observeServerInstance(ctx context.Context) string {
	instance, err := c.Runtime.ServerInstance(ctx)
	if err != nil {
		return ""
	}
	return instance
}

// generatedIdentities are every identity a use case mints through
// IDGenerator before a store call that expects them already parsed.
type generatedIdentities struct {
	Run         identity.RunID
	Task        identity.TaskID
	Attempt     identity.AttemptID
	Session     identity.SessionID
	Worktree    identity.WorktreeID
	Incarnation identity.IncarnationID
}

// generateRunIdentities mints one fresh identity of every kind a new run
// needs, in the order a caller would naturally read them.
func (c *Controller) generateRunIdentities() (generatedIdentities, error) {
	var (
		g   generatedIdentities
		err error
	)
	if g.Run, err = identity.ParseRunID(c.IDs.NewID()); err != nil {
		return generatedIdentities{}, fmt.Errorf("app: generate run id: %w", err)
	}
	if g.Task, err = identity.ParseTaskID(c.IDs.NewID()); err != nil {
		return generatedIdentities{}, fmt.Errorf("app: generate task id: %w", err)
	}
	if g.Attempt, err = identity.ParseAttemptID(c.IDs.NewID()); err != nil {
		return generatedIdentities{}, fmt.Errorf("app: generate attempt id: %w", err)
	}
	if g.Session, err = identity.ParseSessionID(c.IDs.NewID()); err != nil {
		return generatedIdentities{}, fmt.Errorf("app: generate session id: %w", err)
	}
	if g.Worktree, err = identity.ParseWorktreeID(c.IDs.NewID()); err != nil {
		return generatedIdentities{}, fmt.Errorf("app: generate worktree id: %w", err)
	}
	if g.Incarnation, err = identity.ParseIncarnationID(c.IDs.NewID()); err != nil {
		return generatedIdentities{}, fmt.Errorf("app: generate incarnation id: %w", err)
	}
	return g, nil
}

// newOperationID mints a fresh OperationID.
func (c *Controller) newOperationID() (identity.OperationID, error) {
	id, err := identity.ParseOperationID(c.IDs.NewID())
	if err != nil {
		return "", fmt.Errorf("app: generate operation id: %w", err)
	}
	return id, nil
}

// gen returns a pointer to generation, for Transition.Generation.
func gen(generation int64) *int64 { return &generation }

// recordTransition writes one transition-evidence row.
func recordTransition(ctx context.Context, uow UnitOfWork, kind EntityKind, id, from, to, reason string, generation *int64, now time.Time) error {
	return uow.Transitions().Record(ctx, Transition{
		EntityKind: kind,
		EntityID:   id,
		From:       from,
		To:         to,
		Reason:     reason,
		Generation: generation,
		At:         now,
	})
}

// withUnitOfWork opens a unit of work bound to lease, runs fn, and commits
// on success or rolls back on any error fn returns or Commit reports.
func (c *Controller) withUnitOfWork(ctx context.Context, lease Lease, fn func(uow UnitOfWork) error) error {
	uow, err := c.Store.Begin(ctx, lease)
	if err != nil {
		return fmt.Errorf("app: begin unit of work: %w", err)
	}
	if err := fn(uow); err != nil {
		_ = uow.Rollback() //nolint:errcheck // best-effort rollback; fn's error is the one that matters.
		return err
	}
	if err := uow.Commit(); err != nil {
		return fmt.Errorf("app: commit unit of work: %w", err)
	}
	return nil
}
