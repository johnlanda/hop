package sqlite_test

import (
	"errors"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// TestMarkWorktreesRetired proves the worktrees-retired fact's store
// contract, which the app fake mirrors (TestFakeWorktreeRetirementContract):
// set once through a committed unit of work, a later value never
// replacing the first, the run's revision untouched, another run refused
// as fenced before any write, a rolled-back write leaving nothing, and a
// commit after the lease was lost fenced with nothing recorded.
func TestMarkWorktreesRetired(t *testing.T) {
	f := newFeatureFixture(t)
	first := time.Date(2026, 9, 16, 10, 30, 0, 0, time.UTC)
	revision := func() int {
		return countRows(t, f.store, `SELECT revision FROM runs WHERE id = ?`, f.spec.RunID.String())
	}
	retiredAt := func() *time.Time {
		t.Helper()
		detail, err := f.store.LoadRunStatus(t.Context(), f.spec.RunID)
		if err != nil {
			t.Fatalf("LoadRunStatus: %v", err)
		}
		return detail.WorktreesRetiredAt
	}
	repos := func(t *testing.T, uow app.UnitOfWork) app.WorktreeRetirementRepositories {
		t.Helper()
		r, err := app.RequireWorktreeRetirementRepositories(uow, "store test")
		if err != nil {
			t.Fatalf("RequireWorktreeRetirementRepositories: %v", err)
		}
		return r
	}
	before := revision()

	uow, err := f.store.Begin(t.Context(), f.lease)
	if err != nil {
		t.Fatal(err)
	}
	if markErr := repos(t, uow).MarkWorktreesRetired(t.Context(), f.spec.RunID, first); markErr != nil {
		t.Fatalf("MarkWorktreesRetired: %v", markErr)
	}
	if rollbackErr := uow.Rollback(); rollbackErr != nil {
		t.Fatal(rollbackErr)
	}
	if got := retiredAt(); got != nil {
		t.Fatalf("a rolled-back mark recorded %v", got)
	}

	f.inUOW(t, func(uow app.UnitOfWork) {
		if markErr := repos(t, uow).MarkWorktreesRetired(t.Context(), f.spec.RunID, first); markErr != nil {
			t.Fatalf("MarkWorktreesRetired: %v", markErr)
		}
	})
	if got := retiredAt(); got == nil || !got.Equal(first) {
		t.Fatalf("fact = %v, want %v", got, first)
	}
	f.inUOW(t, func(uow app.UnitOfWork) {
		if markErr := repos(t, uow).MarkWorktreesRetired(t.Context(), f.spec.RunID, first.Add(time.Hour)); markErr != nil {
			t.Fatalf("a repeated MarkWorktreesRetired: %v", markErr)
		}
	})
	if got := retiredAt(); got == nil || !got.Equal(first) {
		t.Fatalf("fact after a repeat = %v, want the first value %v", got, first)
	}
	if got := revision(); got != before {
		t.Fatalf("run revision moved from %d to %d; the fact is outside the lifecycle revision", before, got)
	}

	other := newFeatureSpec("/repos/other-retirement", specStride*2, f.clock)
	if _, _, initErr := f.store.InitializeRun(t.Context(), other); initErr != nil {
		t.Fatalf("InitializeRun(other): %v", initErr)
	}
	uow, err = f.store.Begin(t.Context(), f.lease)
	if err != nil {
		t.Fatal(err)
	}
	if markErr := repos(t, uow).MarkWorktreesRetired(t.Context(), other.RunID, first); !errors.Is(markErr, app.ErrFenced) {
		t.Fatalf("marking another run: %v, want ErrFenced", markErr)
	}
	if rollbackErr := uow.Rollback(); rollbackErr != nil {
		t.Fatal(rollbackErr)
	}
	if n := countRows(t, f.store, `SELECT COUNT(*) FROM runs WHERE id = ? AND worktrees_retired_at IS NOT NULL`, other.RunID.String()); n != 0 {
		t.Fatalf("a fenced mark wrote the other run's fact")
	}

	// A commit after the lease was lost is fenced and records nothing.
	lost := newFeatureFixture(t)
	uow, err = lost.store.Begin(t.Context(), lost.lease)
	if err != nil {
		t.Fatal(err)
	}
	if markErr := repos(t, uow).MarkWorktreesRetired(t.Context(), lost.spec.RunID, first); markErr != nil {
		t.Fatalf("MarkWorktreesRetired: %v", markErr)
	}
	lost.clock.Advance(time.Hour)
	if commitErr := uow.Commit(); !errors.Is(commitErr, app.ErrFenced) {
		t.Fatalf("commit after lease expiry: %v, want ErrFenced", commitErr)
	}
	if n := countRows(t, lost.store, `SELECT COUNT(*) FROM runs WHERE worktrees_retired_at IS NOT NULL`); n != 0 {
		t.Fatalf("a fenced commit recorded the fact")
	}
}
