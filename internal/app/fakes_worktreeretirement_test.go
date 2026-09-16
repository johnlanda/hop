package app_test

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
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

// WorktreesRetiredAt mirrors the SQLite store's contract: the leased run
// only (ErrFenced otherwise), an existing run only (ErrNotFound), and this
// transaction's own staged mark visible before the committed fact.
func (u *fakeUnitOfWork) WorktreesRetiredAt(_ context.Context, runID identity.RunID) (*time.Time, error) {
	u.ensureOpen()
	if runID != u.lease.Run {
		return nil, fmt.Errorf("app_test: run %s is not the leased run %s: %w", runID, u.lease.Run, app.ErrFenced)
	}
	if _, staged := u.runs[runID]; !staged {
		if _, exists := u.store.Runs[runID]; !exists {
			return nil, fmt.Errorf("%w: run %s", app.ErrNotFound, runID)
		}
	}
	if at, ok := u.store.WorktreesRetiredAt[runID]; ok {
		return &at, nil
	}
	if at, ok := u.worktreesRetired[runID]; ok {
		return &at, nil
	}
	return nil, nil //nolint:nilnil // a nil time with a nil error is the documented "not retired" value.
}

// WorktreesForRetirement mirrors the SQLite store's contract: the leased
// run only (ErrFenced otherwise), every row of the run in any state with
// its revision, committed rows in insertion order (a row seeded straight
// into the store, which has no insertion sequence, first, by id) followed
// by this transaction's own creates, and this transaction's staged saves
// overlaid.
func (u *fakeUnitOfWork) WorktreesForRetirement(_ context.Context, runID identity.RunID) ([]app.RetirementWorktree, error) {
	u.ensureOpen()
	if runID != u.lease.Run {
		return nil, fmt.Errorf("app_test: run %s is not the leased run %s: %w", runID, u.lease.Run, app.ErrFenced)
	}
	var committed []app.RetirementWorktree
	for id, row := range u.store.Worktrees {
		if row.value.RunID != runID {
			continue
		}
		committed = append(committed, app.RetirementWorktree{Worktree: row.value, Revision: row.revision})
		if staged, ok := u.worktrees[id]; ok {
			committed[len(committed)-1] = app.RetirementWorktree{Worktree: staged.value, Revision: staged.revision}
		}
	}
	slices.SortFunc(committed, func(a, b app.RetirementWorktree) int {
		return cmp.Or(
			cmp.Compare(u.store.worktreeInsertOrder[a.Worktree.ID], u.store.worktreeInsertOrder[b.Worktree.ID]),
			cmp.Compare(a.Worktree.ID, b.Worktree.ID),
		)
	})
	for i := range u.worktreeCreated {
		created := u.worktreeCreated[i]
		if created.RunID != runID {
			continue
		}
		row := app.RetirementWorktree{Worktree: created, Revision: 1}
		if staged, ok := u.worktrees[created.ID]; ok {
			row = app.RetirementWorktree{Worktree: staged.value, Revision: staged.revision}
		}
		committed = append(committed, row)
	}
	return committed, nil
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
		if markErr == nil {
			staged, readErr := repos.WorktreesRetiredAt(context.Background(), runID)
			if readErr != nil || staged == nil {
				t.Fatalf("the staged mark read back as %v, %v; want it visible inside the unit of work", staged, readErr)
			}
		}
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
		uow, err := tc.Store.Begin(context.Background(), lease)
		if err != nil {
			t.Fatalf("Begin() error = %v", err)
		}
		defer func() {
			if rollbackErr := uow.Rollback(); rollbackErr != nil {
				t.Errorf("Rollback() error = %v", rollbackErr)
			}
		}()
		repos, err := app.RequireWorktreeRetirementRepositories(uow, "contract test")
		if err != nil {
			t.Fatalf("RequireWorktreeRetirementRepositories() error = %v", err)
		}
		inside, err := repos.WorktreesRetiredAt(context.Background(), detail.RunID)
		if err != nil {
			t.Fatalf("WorktreesRetiredAt() error = %v", err)
		}
		if (inside == nil) != (got.WorktreesRetiredAt == nil) || (inside != nil && !inside.Equal(*got.WorktreesRetiredAt)) {
			t.Fatalf("the fact inside a unit of work %v disagrees with LoadRunStatus %v", inside, got.WorktreesRetiredAt)
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
	fencedRead, err := tc.Store.Begin(context.Background(), lease)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	fencedRepos, err := app.RequireWorktreeRetirementRepositories(fencedRead, "contract test")
	if err != nil {
		t.Fatal(err)
	}
	if _, readErr := fencedRepos.WorktreesRetiredAt(context.Background(), other); !errors.Is(readErr, app.ErrFenced) {
		t.Fatalf("reading another run's fact: err %v, want ErrFenced", readErr)
	}
	if rollbackErr := fencedRead.Rollback(); rollbackErr != nil {
		t.Fatal(rollbackErr)
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

// TestFakeWorktreesForRetirementContract proves the fake's leased worktree
// listing honors the SQLite store's contract (TestWorktreesForRetirement):
// every row of the run in any state with its revision, oldest first, this
// transaction's creates and saves included, and another run fenced.
func TestFakeWorktreesForRetirementContract(t *testing.T) {
	tc := newTestController(defaultPolicy())
	_, detail := startedRun(t, tc)
	lease := tc.Store.Leases[detail.RunID].lease
	started, ok := tc.Store.worktreeByRunLocked(detail.RunID)
	if !ok {
		t.Fatalf("the started run has no worktree")
	}
	seeded := run.NewWorktree("00000000-0000-4000-8000-0000000000a1", started.RepositoryID, detail.RunID, "/worktrees/seeded", "hop/seeded")
	seeded.State = run.WorktreeReleased
	tc.Store.Worktrees[seeded.ID] = &entityRow[run.Worktree]{value: seeded, revision: 3}
	foreign := run.NewWorktree("00000000-0000-4000-8000-0000000000a2", started.RepositoryID, "99999999-9999-4999-8999-999999999999", "/worktrees/foreign", "hop/foreign")
	tc.Store.Worktrees[foreign.ID] = &entityRow[run.Worktree]{value: foreign, revision: 1}

	uow, err := tc.Store.Begin(context.Background(), lease)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	defer func() {
		if rollbackErr := uow.Rollback(); rollbackErr != nil {
			t.Errorf("Rollback() error = %v", rollbackErr)
		}
	}()
	repos, err := app.RequireWorktreeRetirementRepositories(uow, "contract test")
	if err != nil {
		t.Fatalf("RequireWorktreeRetirementRepositories() error = %v", err)
	}
	created := run.NewWorktree("00000000-0000-4000-8000-0000000000a3", started.RepositoryID, detail.RunID, "/worktrees/created", "hop/created")
	if _, createErr := uow.Worktrees().Create(context.Background(), created); createErr != nil {
		t.Fatalf("Create() error = %v", createErr)
	}
	removed, err := started.Retire(run.WorktreeRemoved)
	if err != nil {
		t.Fatal(err)
	}
	if _, saveErr := uow.Worktrees().Save(context.Background(), removed, 1); saveErr != nil {
		t.Fatalf("Save() error = %v", saveErr)
	}

	got, err := repos.WorktreesForRetirement(context.Background(), detail.RunID)
	if err != nil {
		t.Fatalf("WorktreesForRetirement() error = %v", err)
	}
	want := []app.RetirementWorktree{{Worktree: seeded, Revision: 3}, {Worktree: removed, Revision: 2}, {Worktree: created, Revision: 1}}
	if !slices.Equal(got, want) {
		t.Fatalf("WorktreesForRetirement() = %+v, want %+v", got, want)
	}
	if _, err := repos.WorktreesForRetirement(context.Background(), foreign.RunID); !errors.Is(err, app.ErrFenced) {
		t.Fatalf("another run's rows: err %v, want ErrFenced", err)
	}
}
