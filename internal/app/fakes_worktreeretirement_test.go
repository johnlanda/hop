package app_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
)

var _ app.WorktreeRetirementRepositories = (*fakeUnitOfWork)(nil)

// MarkWorktreesRetired mirrors the SQLite store's contract: the leased run
// only (ErrFenced otherwise, nothing staged), an existing run only
// (ErrNotFound), and a set-once fact — a repeated call, staged or already
// committed, keeps the first value.
func (u *fakeUnitOfWork) MarkWorktreesRetired(_ context.Context, runID identity.RunID, at time.Time) error {
	u.ensureOpen()
	if runID != u.lease.Run {
		return fmt.Errorf("app_test: run %s is not the leased run %s: %w", runID, u.lease.Run, app.ErrFenced)
	}
	if _, staged := u.runs[runID]; !staged {
		if _, exists := u.store.Runs[runID]; !exists {
			return fmt.Errorf("%w: run %s", app.ErrNotFound, runID)
		}
	}
	if u.worktreesRetired == nil {
		u.worktreesRetired = map[identity.RunID]time.Time{}
	}
	if _, staged := u.worktreesRetired[runID]; !staged {
		u.worktreesRetired[runID] = at.UTC()
	}
	return nil
}

// TestFakeWorktreeRetirementContract proves the fake's worktrees-retired
// fact honors the SQLite store's contract (TestMarkWorktreesRetired): set
// once through a committed unit of work and read back by LoadRunStatus, a
// later value never replacing the first, another run refused as fenced
// with nothing staged, a rolled-back write leaving nothing, and a unit of
// work lacking the capability failing closed with the typed sentinel.
func TestFakeWorktreeRetirementContract(t *testing.T) {
	tc := newTestController(defaultPolicy())
	_, detail := startedRun(t, tc)
	lease := tc.Store.Leases[detail.RunID].lease
	first := time.Date(2026, 9, 16, 10, 30, 0, 0, time.UTC)

	mark := func(runID identity.RunID, at time.Time, commit bool) error {
		t.Helper()
		uow, err := tc.Store.Begin(context.Background(), lease)
		if err != nil {
			t.Fatalf("Begin() error = %v", err)
		}
		repos, err := app.RequireWorktreeRetirementRepositories(uow, "contract test")
		if err != nil {
			t.Fatalf("RequireWorktreeRetirementRepositories() error = %v", err)
		}
		markErr := repos.MarkWorktreesRetired(context.Background(), runID, at)
		if !commit || markErr != nil {
			if rollbackErr := uow.Rollback(); rollbackErr != nil {
				t.Fatalf("Rollback() error = %v", rollbackErr)
			}
			return markErr
		}
		return uow.Commit()
	}
	retiredAt := func() *time.Time {
		t.Helper()
		got, err := tc.Store.LoadRunStatus(context.Background(), detail.RunID)
		if err != nil {
			t.Fatalf("LoadRunStatus() error = %v", err)
		}
		return got.WorktreesRetiredAt
	}

	if err := mark(detail.RunID, first, false); err != nil || retiredAt() != nil {
		t.Fatalf("a rolled-back mark: err %v, fact %v; want nothing recorded", err, retiredAt())
	}
	if err := mark(detail.RunID, first, true); err != nil {
		t.Fatalf("MarkWorktreesRetired() error = %v", err)
	}
	if got := retiredAt(); got == nil || !got.Equal(first) {
		t.Fatalf("fact = %v, want %v", got, first)
	}
	if err := mark(detail.RunID, first.Add(time.Hour), true); err != nil {
		t.Fatalf("a repeated MarkWorktreesRetired() error = %v, want success", err)
	}
	if got := retiredAt(); got == nil || !got.Equal(first) {
		t.Fatalf("fact after a repeat = %v, want the first value %v", got, first)
	}

	other := identity.RunID("99999999-9999-4999-8999-999999999999")
	if err := mark(other, first, true); !errors.Is(err, app.ErrFenced) {
		t.Fatalf("marking another run: err %v, want ErrFenced", err)
	}
	if _, staged := tc.Store.WorktreesRetiredAt[other]; staged {
		t.Fatalf("a fenced mark recorded a fact for the other run")
	}

	uow, err := tc.Store.Begin(context.Background(), lease)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	defer func() {
		if rollbackErr := uow.Rollback(); rollbackErr != nil {
			t.Errorf("Rollback() error = %v", rollbackErr)
		}
	}()
	inner, ok := uow.(*fakeUnitOfWork)
	if !ok {
		t.Fatalf("Begin() returned %T", uow)
	}
	if _, err := app.RequireWorktreeRetirementRepositories(&plainUnitOfWork{inner: inner}, "plain unit of work"); !errors.Is(err, app.ErrWorktreeRetirementUnsupported) {
		t.Fatalf("a plain unit of work: err %v, want ErrWorktreeRetirementUnsupported", err)
	}
}
