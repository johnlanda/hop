package sqlite_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
)

// TestFrozenWorkflowWithoutTargetBranch proves a feature snapshot frozen
// before the TargetBranch field existed — workflow JSON without the key —
// loads with no retirement target, and that the key round-trips when
// present (TestInitializeRunFeatureShape freezes one through
// InitializeRun).
func TestFrozenWorkflowWithoutTargetBranch(t *testing.T) {
	clock := newFakeClock()
	store := openStoreAt(t, t.TempDir(), clock)
	spec := newFeatureSpec("/repos/legacy-target", specStride, clock)
	if _, _, err := store.InitializeRun(t.Context(), spec); err != nil {
		t.Fatalf("InitializeRun: %v", err)
	}

	legacy := map[string]any{}
	encoded, err := json.Marshal(spec.Snapshot.Workflow)
	if err != nil {
		t.Fatal(err)
	}
	if decodeErr := json.Unmarshal(encoded, &legacy); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if legacy["TargetBranch"] != "refs/heads/main" {
		t.Fatalf("the workflow JSON key is %v, want TargetBranch carrying the frozen ref", legacy)
	}
	delete(legacy, "TargetBranch")
	withoutKey, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, execErr := sqlite.WriteDB(store).ExecContext(t.Context(),
		`UPDATE run_snapshots SET workflow = ? WHERE run_id = ?`, string(withoutKey), spec.RunID.String(),
	); execErr != nil {
		t.Fatalf("rewrite the snapshot without the key: %v", execErr)
	}

	frozen, err := store.LoadFrozenRun(t.Context(), spec.RunID)
	if err != nil {
		t.Fatalf("LoadFrozenRun: %v", err)
	}
	if frozen.Snapshot.Workflow.TargetBranch != "" {
		t.Errorf("a snapshot without the key loads TargetBranch %q, want empty (never retired)", frozen.Snapshot.Workflow.TargetBranch)
	}
	if !frozen.Snapshot.Workflow.Feature() || frozen.Snapshot.Workflow.BaseCommitOID != spec.Snapshot.Workflow.BaseCommitOID {
		t.Errorf("the rest of the legacy snapshot did not load: %+v", frozen.Snapshot.Workflow)
	}
}

// TestLoadRunStatusRetirementFields proves the detail read carries the
// frozen target and the worktrees-retired fact: NULL reads as nil, a
// canonical time reads back exactly, and a noncanonical stored value fails
// closed rather than being guessed.
func TestLoadRunStatusRetirementFields(t *testing.T) {
	clock := newFakeClock()
	store := openStoreAt(t, t.TempDir(), clock)
	spec := newFeatureSpec("/repos/retirement-detail", specStride, clock)
	if _, _, err := store.InitializeRun(t.Context(), spec); err != nil {
		t.Fatalf("InitializeRun: %v", err)
	}

	detail, err := store.LoadRunStatus(t.Context(), spec.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus: %v", err)
	}
	if detail.TargetBranch != "refs/heads/main" || detail.WorktreesRetiredAt != nil {
		t.Fatalf("detail target %q retired %v, want refs/heads/main and nil", detail.TargetBranch, detail.WorktreesRetiredAt)
	}

	setRetired := func(value string) {
		t.Helper()
		if _, execErr := sqlite.WriteDB(store).ExecContext(t.Context(),
			`UPDATE runs SET worktrees_retired_at = ? WHERE id = ?`, value, spec.RunID.String(),
		); execErr != nil {
			t.Fatal(execErr)
		}
	}
	setRetired("2026-09-16T10:30:00.000000000Z")
	detail, err = store.LoadRunStatus(t.Context(), spec.RunID)
	if err != nil {
		t.Fatalf("LoadRunStatus: %v", err)
	}
	want := time.Date(2026, 9, 16, 10, 30, 0, 0, time.UTC)
	if detail.WorktreesRetiredAt == nil || !detail.WorktreesRetiredAt.Equal(want) {
		t.Fatalf("retired at = %v, want %v", detail.WorktreesRetiredAt, want)
	}

	setRetired("2026-09-16 10:30:00")
	if _, err := store.LoadRunStatus(t.Context(), spec.RunID); err == nil {
		t.Fatal("a noncanonical retired-at value loaded; want fail closed")
	}
}
