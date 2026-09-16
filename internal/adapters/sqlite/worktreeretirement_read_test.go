package sqlite_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/sqlite"
	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// retirementRunSeed is one run of the triage read's fixture repository.
type retirementRunSeed struct {
	root string
	base int
	// feature freezes the feature workflow with target; otherwise the run
	// is solo.
	feature bool
	target  string
	state   run.RunState
	// integrated are (pre-merge, merge) pairs recorded as integrated rows,
	// one second apart; merging adds one row that never integrated.
	integrated [][2]string
	merging    bool
	retired    bool
}

// seedRetirementRun initializes seed's run and writes its frozen target,
// integration rows, state and fact directly.
func seedRetirementRun(t *testing.T, store *sqlite.Store, clock *fakeClock, seed *retirementRunSeed) app.NewRunSpec {
	t.Helper()
	spec := newSpec(seed.root, seed.base, clock.Now())
	if seed.feature {
		initLegacyFeatureRun(t, store, &spec)
		spec.Snapshot.Workflow.TargetBranch = seed.target
		encoded, err := json.Marshal(spec.Snapshot.Workflow)
		if err != nil {
			t.Fatal(err)
		}
		rawExec(t, store, `UPDATE run_snapshots SET workflow = ? WHERE run_id = ?`, string(encoded), spec.RunID.String())
	} else if _, _, err := store.InitializeRun(t.Context(), spec); err != nil {
		t.Fatalf("InitializeRun: %v", err)
	}
	rows := slices.Clone(seed.integrated)
	if seed.merging {
		rows = append(rows, [2]string{"premerge-m", ""})
	}
	for i, pair := range rows {
		resultID, integrationID := uid(seed.base+50+i), uid(seed.base+70+i)
		at := time.Date(2026, 9, 16, 12, 0, i, 0, time.UTC).Format("2006-01-02T15:04:05.000000000Z")
		rawExec(t, store, `INSERT INTO results (id, attempt_id, commit_oid, summary, content_digest, accepted, submitted_at) VALUES (?, ?, ?, 'summary', ?, 0, ?)`,
			resultID, spec.AttemptID.String(), fmt.Sprintf("commit-%d", i), fmt.Sprintf("digest-%d", i), at)
		state, merge := string(run.IntegrationIntegrated), any(pair[1])
		if pair[1] == "" {
			state, merge = string(run.IntegrationMerging), nil
		}
		rawExec(t, store, `INSERT INTO integrations (id, run_id, task_id, result_id, source_commit_oid, premerge_head_oid, merge_commit_oid, state, revision, created_at, updated_at) VALUES (?, ?, ?, ?, 'source', ?, ?, ?, 1, ?, ?)`,
			integrationID, spec.RunID.String(), spec.TaskID.String(), resultID, pair[0], merge, state, at, at)
	}
	rawExec(t, store, `UPDATE runs SET state = ? WHERE id = ?`, string(seed.state), spec.RunID.String())
	if seed.retired {
		rawExec(t, store, `UPDATE runs SET worktrees_retired_at = '2026-09-16T13:00:00.000000000Z' WHERE id = ?`, spec.RunID.String())
	}
	return spec
}

// seedRetirementOperation records one retirement operation directly.
func seedRetirementOperation(t *testing.T, store *sqlite.Store, runID identity.RunID, n int, kind app.OperationKind, state app.OperationState, intent, outcome string, at int) {
	t.Helper()
	created := time.Date(2026, 9, 16, 14, 0, at, 0, time.UTC).Format("2006-01-02T15:04:05.000000000Z")
	rawExec(t, store, `INSERT INTO operations (id, run_id, generation, kind, state, intent, act_evidence, outcome, created_at, updated_at) VALUES (?, ?, 1, ?, ?, ?, NULL, ?, ?, ?)`,
		uid(n), runID.String(), string(kind), string(state), intent, outcome, created, created)
}

// TestListRetirementCandidates proves the triage read's store contract,
// which the app fake mirrors (TestFakeRetirementCandidatesContract): only
// the repository's terminal feature runs with a target, the fact unset
// and content integrated, in sequence order, each with its integrated rows
// oldest first, its retirement checks newest first and whether any
// retirement operation is unresolved.
func TestListRetirementCandidates(t *testing.T) {
	clock := newFakeClock()
	store := openStoreAt(t, t.TempDir(), clock)
	const root = "/repos/retirement-triage"
	seeds := []*retirementRunSeed{
		{root: root, base: 1 * specStride, feature: true, target: "refs/heads/main", state: run.RunCompleted, integrated: [][2]string{{"base", "base"}, {"base", "merge-1"}}, merging: true},
		{root: root, base: 2 * specStride, feature: true, target: "refs/heads/main", state: run.RunFailed, integrated: [][2]string{{"base", "merge-2"}}},
		{root: root, base: 3 * specStride, feature: true, target: "refs/heads/trunk", state: run.RunStopped, integrated: [][2]string{{"base", "merge-3"}}},
		{root: root, base: 4 * specStride, state: run.RunCompleted, integrated: [][2]string{{"base", "merge-4"}}},
		{root: root, base: 5 * specStride, feature: true, target: "refs/heads/main", state: run.RunRunning, integrated: [][2]string{{"base", "merge-5"}}},
		{root: root, base: 6 * specStride, feature: true, target: "refs/heads/main", state: run.RunStopping, integrated: [][2]string{{"base", "merge-6"}}},
		{root: root, base: 7 * specStride, feature: true, state: run.RunCompleted, integrated: [][2]string{{"base", "merge-7"}}},
		{root: root, base: 8 * specStride, feature: true, target: "refs/heads/main", state: run.RunCompleted, integrated: [][2]string{{"base", "merge-8"}}, retired: true},
		{root: root, base: 9 * specStride, feature: true, target: "refs/heads/main", state: run.RunCompleted, merging: true},
		{root: root, base: 10 * specStride, feature: true, target: "refs/heads/main", state: run.RunCompleted, integrated: [][2]string{{"base", "base"}, {"merge-x", "merge-x"}}},
		{root: "/repos/elsewhere", base: 11 * specStride, feature: true, target: "refs/heads/main", state: run.RunCompleted, integrated: [][2]string{{"base", "merge-11"}}},
	}
	specs := make([]app.NewRunSpec, len(seeds))
	for i, seed := range seeds {
		specs[i] = seedRetirementRun(t, store, clock, seed)
	}
	first, second := specs[0].RunID, specs[1].RunID
	seedRetirementOperation(t, store, first, 9001, app.OpRetirementCheck, app.OperationFailed, `{"head_oid":"merge-1","target_oid":"t1"}`, `{"result":"unknown"}`, 1)
	seedRetirementOperation(t, store, first, 9002, app.OpRetirementCheck, app.OperationSucceeded, `{"head_oid":"merge-1","target_oid":"t2"}`, `{"result":"not-merged"}`, 2)
	seedRetirementOperation(t, store, first, 9003, app.OpWorktreeRetire, app.OperationReconciling, `{"decision":"remove"}`, `"not observed"`, 3)
	seedRetirementOperation(t, store, first, 9004, app.OpPaneOpen, app.OperationPending, `{}`, `null`, 4)
	seedRetirementOperation(t, store, second, 9005, app.OpWorktreeRetire, app.OperationSucceeded, `{"decision":"absent"}`, `{"result":"absent"}`, 5)
	seedRetirementOperation(t, store, second, 9006, app.OpPaneOpen, app.OperationReconciling, `{}`, `null`, 6)

	records, err := store.ListRetirementCandidates(t.Context(), root)
	if err != nil {
		t.Fatalf("ListRetirementCandidates: %v", err)
	}
	var got []identity.RunID
	for i := range records {
		got = append(got, records[i].RunID)
	}
	if want := []identity.RunID{first, second, specs[2].RunID}; !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, want %v (completed, failed, stopped)", got, want)
	}
	one := records[0]
	if one.Sequence != 1 || one.RepositoryRoot != root || one.TargetBranch != "refs/heads/main" || !one.Unresolved {
		t.Fatalf("first record = %+v", one)
	}
	if records[2].Sequence != 3 || records[2].TargetBranch != "refs/heads/trunk" {
		t.Fatalf("third record = %+v", records[2])
	}
	if len(one.Integrated) != 2 || one.Integrated[0].MergeCommitOID != "base" || one.Integrated[1].MergeCommitOID != "merge-1" || one.Integrated[1].State != run.IntegrationIntegrated {
		t.Fatalf("integrated rows = %+v, want both integrated rows oldest first and no merging row", one.Integrated)
	}
	if len(one.Checks) != 2 || one.Checks[0].ID.String() != uid(9002) || one.Checks[1].ID.String() != uid(9001) || one.Checks[0].Kind != app.OpRetirementCheck {
		t.Fatalf("checks = %+v, want the two retirement checks newest first", one.Checks)
	}
	if records[1].Unresolved || len(records[1].Checks) != 0 {
		t.Fatalf("second record = %+v, want nothing unresolved: a settled removal and another kind's open operation do not count", records[1])
	}
	if none, err := store.ListRetirementCandidates(t.Context(), "/repos/unknown"); err != nil || len(none) != 0 {
		t.Fatalf("an unknown root = %+v, %v; want none", none, err)
	}
	if other, err := store.ListRetirementCandidates(t.Context(), "/repos/elsewhere"); err != nil || len(other) != 1 || other[0].RunID != specs[10].RunID {
		t.Fatalf("the other repository = %+v, %v; want its own run only", other, err)
	}
}
