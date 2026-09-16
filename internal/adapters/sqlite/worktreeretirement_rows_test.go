package sqlite_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// TestWorktreesForRetirement proves the leased worktree listing's store
// contract, which the app fake mirrors
// (TestFakeWorktreesForRetirementContract): every row of the run in
// insertion order with its attempt link, base commit, state and revision;
// each retirement state round-trips through Worktrees().Save; a run with
// no rows lists none; and another run is refused as fenced.
func TestWorktreesForRetirement(t *testing.T) {
	f := newFeatureFixture(t)
	list := func(t *testing.T, runID identity.RunID) ([]app.RetirementWorktree, error) {
		t.Helper()
		var (
			rows []app.RetirementWorktree
			err  error
		)
		f.inUOW(t, func(uow app.UnitOfWork) {
			repos, reqErr := app.RequireWorktreeRetirementRepositories(uow, "store test")
			if reqErr != nil {
				t.Fatalf("RequireWorktreeRetirementRepositories: %v", reqErr)
			}
			rows, err = repos.WorktreesForRetirement(t.Context(), runID)
		})
		return rows, err
	}

	if rows, err := list(t, f.spec.RunID); err != nil || len(rows) != 0 {
		t.Fatalf("a run without worktrees: %+v, %v; want none", rows, err)
	}

	var want []app.RetirementWorktree
	// Descending identities: the listing orders by insertion, never by id.
	for i, n := range []int{7940, 7930, 7920, 7910} {
		task := f.createFeatureTask(t, n+6, i+2, run.TaskActive)
		f.createWorkerSession(t, task, run.RoleImplementer, n)
		f.clock.Advance(time.Second)
		path, branch := fmt.Sprintf("/worktrees/feature/t%da1", i+2), fmt.Sprintf("refs/heads/hop/r1/t%da1", i+2)
		f.createAttemptWorktree(t, n+5, identity.AttemptID(uid(n)), path, branch)
		w, err := run.NewAttemptWorktree(identity.WorktreeID(uid(n+5)), f.repositoryID(t), f.spec.RunID, identity.AttemptID(uid(n)), "base-oid", path, branch)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, app.RetirementWorktree{Worktree: w, Revision: 1})
	}
	rows, err := list(t, f.spec.RunID)
	if err != nil || !slices.Equal(rows, want) {
		t.Fatalf("WorktreesForRetirement = %+v, %v; want %+v", rows, err, want)
	}

	for i, state := range []run.WorktreeState{run.WorktreeRemoved, run.WorktreeAbsent, run.WorktreeReleased} {
		retired, retireErr := want[i].Worktree.Retire(state)
		if retireErr != nil {
			t.Fatal(retireErr)
		}
		f.inUOW(t, func(uow app.UnitOfWork) {
			if _, saveErr := uow.Worktrees().Save(t.Context(), retired, want[i].Revision); saveErr != nil {
				t.Fatalf("save %s: %v", state, saveErr)
			}
		})
		want[i] = app.RetirementWorktree{Worktree: retired, Revision: 2}
	}
	rows, err = list(t, f.spec.RunID)
	if err != nil || !slices.Equal(rows, want) {
		t.Fatalf("after the retirement saves: %+v, %v; want %+v", rows, err, want)
	}

	other := newFeatureSpec("/repos/other-rows", specStride*2, f.clock)
	if _, _, initErr := f.store.InitializeRun(t.Context(), other); initErr != nil {
		t.Fatalf("InitializeRun(other): %v", initErr)
	}
	uow, err := f.store.Begin(t.Context(), f.lease)
	if err != nil {
		t.Fatal(err)
	}
	repos, err := app.RequireWorktreeRetirementRepositories(uow, "store test")
	if err != nil {
		t.Fatal(err)
	}
	if _, listErr := repos.WorktreesForRetirement(t.Context(), other.RunID); !errors.Is(listErr, app.ErrFenced) {
		t.Fatalf("listing another run: %v, want ErrFenced", listErr)
	}
	if rollbackErr := uow.Rollback(); rollbackErr != nil {
		t.Fatal(rollbackErr)
	}
}

// TestLoadRunStatusWorktreeRows proves the status detail's per-row input:
// a feature run lists every worktree row oldest first in its current state
// and its worktree.retire operations newest first (a run without rows an
// empty list), while a solo run keeps its single worktree path and carries
// neither.
func TestLoadRunStatusWorktreeRows(t *testing.T) {
	f := newFeatureFixture(t)
	detail, err := f.store.LoadRunStatus(t.Context(), f.spec.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus: %v", err)
	}
	if detail.Worktrees == nil || len(detail.Worktrees) != 0 || len(detail.WorktreeRetirements) != 0 {
		t.Fatalf("a feature run without rows = %+v / %+v, want an empty list", detail.Worktrees, detail.WorktreeRetirements)
	}

	for i, n := range []int{7840, 7830} {
		task := f.createFeatureTask(t, n+6, i+2, run.TaskActive)
		f.createWorkerSession(t, task, run.RoleImplementer, n)
		f.clock.Advance(time.Second)
		f.createAttemptWorktree(t, n+5, identity.AttemptID(uid(n)), fmt.Sprintf("/wt/t%da1", i+2), fmt.Sprintf("hop/r1/t%da1", i+2))
	}
	f.inUOW(t, func(uow app.UnitOfWork) {
		row, revision, getErr := uow.Worktrees().Get(t.Context(), identity.WorktreeID(uid(7835)))
		if getErr != nil {
			t.Fatal(getErr)
		}
		released, retireErr := row.Retire(run.WorktreeReleased)
		if retireErr != nil {
			t.Fatal(retireErr)
		}
		if _, saveErr := uow.Worktrees().Save(t.Context(), released, revision); saveErr != nil {
			t.Fatal(saveErr)
		}
	})
	seedRetirementOperation(t, f.store, f.spec.RunID, 9101, app.OpWorktreeRetire, app.OperationSucceeded,
		`{"decision":"released","worktree_id":"`+uid(7835)+`"}`, `{"result":"released","released_reason":"detached"}`, 1)
	seedRetirementOperation(t, f.store, f.spec.RunID, 9102, app.OpWorktreeRetire, app.OperationReconciling,
		`{"decision":"remove","worktree_id":"`+uid(7845)+`"}`, `"not observed"`, 2)
	seedRetirementOperation(t, f.store, f.spec.RunID, 9103, app.OpRetirementCheck, app.OperationSucceeded, `{}`, `{"result":"merged"}`, 3)

	detail, err = f.store.LoadRunStatus(t.Context(), f.spec.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus: %v", err)
	}
	if len(detail.Worktrees) != 2 || detail.Worktrees[0].ID.String() != uid(7845) || detail.Worktrees[1].ID.String() != uid(7835) {
		t.Fatalf("rows = %+v, want both rows in insertion order", detail.Worktrees)
	}
	if detail.Worktrees[0].State != run.WorktreeActive || detail.Worktrees[1].State != run.WorktreeReleased || detail.Worktrees[1].Branch != "hop/r1/t3a1" {
		t.Fatalf("row states = %+v", detail.Worktrees)
	}
	if len(detail.WorktreeRetirements) != 2 || detail.WorktreeRetirements[0].ID.String() != uid(9102) || detail.WorktreeRetirements[1].ID.String() != uid(9101) {
		t.Fatalf("retirements = %+v, want the two worktree.retire operations newest first", detail.WorktreeRetirements)
	}

	solo := newFixture(t)
	soloDetail, err := solo.store.LoadRunStatus(t.Context(), solo.spec.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus(solo): %v", err)
	}
	if soloDetail.Worktrees != nil || soloDetail.WorktreeRetirements != nil {
		t.Fatalf("a solo run carries per-row input: %+v", soloDetail)
	}
}
