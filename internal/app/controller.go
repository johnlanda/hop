package app

import (
	"context"
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
)

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
// import the identity package to do so.
type RunHandle struct {
	runID identity.RunID
	lease Lease
}

// RunID renders the handle's run identity as a string, for display only.
func (h *RunHandle) RunID() string { return h.runID.String() }

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
