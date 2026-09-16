package app_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// storeState is everything a lease-free triage must leave untouched.
type storeState struct {
	operations, transitions, retired int
	lease                            leaseRow
	worktrees                        map[identity.WorktreeID]int64
	checkExecs                       int
	removals                         int
}

func (f *retireFixture) state() storeState {
	f.t.Helper()
	f.tc.Store.mu.Lock()
	defer f.tc.Store.mu.Unlock()
	s := storeState{
		operations: len(f.tc.Store.Operations), transitions: len(f.tc.Store.Transitions),
		retired: len(f.tc.Store.WorktreesRetiredAt), lease: *f.tc.Store.Leases[f.fr.RunID],
		worktrees: map[identity.WorktreeID]int64{},
	}
	for id, row := range f.tc.Store.Worktrees {
		s.worktrees[id] = row.revision
	}
	f.git.mu.Lock()
	s.checkExecs = f.git.CheckExecCalls
	f.git.mu.Unlock()
	removes, _, _ := f.git.attemptLog()
	s.removals = len(removes)
	return s
}

func (s *storeState) equal(other *storeState) bool {
	return s.operations == other.operations && s.transitions == other.transitions && s.retired == other.retired &&
		s.lease == other.lease && maps.Equal(s.worktrees, other.worktrees) &&
		s.checkExecs == other.checkExecs && s.removals == other.removals
}

// release ends the fixture's current pass lease, as a finished pass does.
func (f *retireFixture) release() {
	f.t.Helper()
	if err := f.tc.Controller.ReleaseRetirement(context.Background(), f.handle); err != nil {
		f.t.Fatalf("ReleaseRetirement() error = %v", err)
	}
	f.handle = app.RunHandle{}
}

// triage runs lease-free triage for the fixture repository and requires
// that it took no lease and wrote nothing.
func (f *retireFixture) triage(exclude string) []app.RetirementCandidate {
	f.t.Helper()
	before := f.state()
	candidates, err := f.tc.Controller.RetirementCandidates(context.Background(), detectRoot, exclude)
	if err != nil {
		f.t.Fatalf("RetirementCandidates() error = %v", err)
	}
	if after := f.state(); !before.equal(&after) {
		f.t.Fatalf("triage took a lease or wrote something:\nbefore %+v\nafter  %+v", before, after)
	}
	return candidates
}

// only returns the fixture run's verdict, failing when triage returned
// anything else.
func (f *retireFixture) only(candidates []app.RetirementCandidate) app.RetirementCandidate {
	f.t.Helper()
	if len(candidates) != 1 || candidates[0].RunID != f.fr.RunID.String() || candidates[0].Sequence != f.tc.Store.Runs[f.fr.RunID].value.Sequence {
		f.t.Fatalf("candidates = %+v, want only the fixture run", candidates)
	}
	return candidates[0]
}

// runPass acquires a lease, runs one pass and releases it, as hop status
// does for a candidate that needs one.
func (f *retireFixture) runPass() app.WorktreeRetirementReport {
	f.t.Helper()
	f.acquire()
	report, _ := f.pass()
	f.release()
	return report
}

// TestRetirementCandidates covers lease-free triage: its verdicts follow
// detection's own rules, and every triage — over nothing eligible, over an
// unchanged unmerged run, over a moved repository — takes no lease, writes
// nothing and spawns nothing claimed.
func TestRetirementCandidates(t *testing.T) {
	ineligible := []struct {
		name  string
		setup func(f *retireFixture)
	}{
		{"a running run", func(f *retireFixture) { f.tc.Store.Runs[f.fr.RunID].value.State = run.RunRunning }},
		{"a stopping run", func(f *retireFixture) { f.tc.Store.Runs[f.fr.RunID].value.State = run.RunStopping }},
		{"a solo run", func(f *retireFixture) { f.tc.Store.Snapshots[f.fr.RunID] = app.RunSnapshot{StateRoot: "/state"} }},
		{"a run frozen on a detached HEAD", func(f *retireFixture) {
			snapshot := f.tc.Store.Snapshots[f.fr.RunID]
			snapshot.Workflow.TargetBranch = ""
			f.tc.Store.Snapshots[f.fr.RunID] = snapshot
		}},
		{"a retired run", func(f *retireFixture) {
			f.tc.Store.WorktreesRetiredAt[f.fr.RunID] = time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
		}},
		{"a run that integrated nothing", func(f *retireFixture) { clear(f.tc.Store.Integrations) }},
		{"a run whose integrations were all no-ops", func(f *retireFixture) {
			clear(f.tc.Store.Integrations)
			f.seedIntegration(f.base, f.base, f.tc.Clock.Now())
		}},
	}
	for _, tc := range ineligible {
		t.Run("nothing eligible, no lease, no write: "+tc.name, func(t *testing.T) {
			f := newRetireFixture(t)
			f.addAttempt(1, fakeAttemptWorktree{}, nil)
			f.release()
			tc.setup(f)
			for range 3 {
				if candidates := f.triage(""); len(candidates) != 0 {
					t.Fatalf("candidates = %+v, want none", candidates)
				}
			}
		})
	}

	t.Run("an unchanged unmerged run is triaged without a lease or a write, again and again", func(t *testing.T) {
		f := newRetireFixture(t)
		a := f.addAttempt(1, fakeAttemptWorktree{}, nil)
		f.git.setRef(detectMainRef, f.git.newCommit("tree-main-behind", f.base))
		f.release()
		if verdict := f.only(f.triage("")); !verdict.NeedsPass {
			t.Fatalf("a head and tip never checked = %+v, want a pass", verdict)
		}
		if report := f.runPass(); report.Disposition != app.RetirementNotMerged {
			t.Fatalf("pass = %+v, want not merged", report)
		}
		generation := f.tc.Store.Leases[f.fr.RunID].lease.Generation
		for range 3 {
			verdict := f.only(f.triage(""))
			if verdict.NeedsPass || verdict.Disposition != app.RetirementNotMerged {
				t.Fatalf("unchanged run = %+v, want not merged with no pass", verdict)
			}
		}
		if got := f.tc.Store.Leases[f.fr.RunID].lease.Generation; got != generation {
			t.Fatalf("lease generation moved from %d to %d", generation, got)
		}
		if len(f.checks()) != 1 || f.rowState(a.WorktreeID) != run.WorktreeActive {
			t.Fatalf("the journal grew or the row moved: %d checks", len(f.checks()))
		}

		f.git.setRef(detectMainRef, f.git.newCommit("tree-main-moved", f.git.ref(detectMainRef)))
		if verdict := f.only(f.triage("")); !verdict.NeedsPass {
			t.Fatalf("a moved target = %+v, want a pass", verdict)
		}
		if report := f.runPass(); report.Disposition != app.RetirementNotMerged || len(f.checks()) != 2 {
			t.Fatalf("pass = %+v with %d checks, want a second not-merged check", report, len(f.checks()))
		}
		f.git.setRef(detectMainRef, f.git.newCommit("tree-main-merged", f.git.ref(detectMainRef), f.merge))
		if verdict := f.only(f.triage("")); !verdict.NeedsPass {
			t.Fatalf("a merged target = %+v, want a pass", verdict)
		}
		if report := f.runPass(); report.Disposition != app.RetirementRetired {
			t.Fatalf("pass = %+v, want retired", report)
		}
		if candidates := f.triage(""); len(candidates) != 0 {
			t.Fatalf("a retired run is still a candidate: %+v", candidates)
		}
	})

	t.Run("a merged run needs passes until its fact is set", func(t *testing.T) {
		f := newRetireFixture(t)
		dirty := f.addAttempt(1, fakeAttemptWorktree{Modified: true}, nil)
		f.release()
		if report := f.runPass(); report.Disposition != app.RetirementInProgress {
			t.Fatalf("pass = %+v, want in progress", report)
		}
		for range 2 {
			if verdict := f.only(f.triage("")); !verdict.NeedsPass {
				t.Fatalf("a merged run with a retained worktree = %+v, want a pass", verdict)
			}
		}
		f.git.addAttemptWorktree(dirty.Listed, fakeAttemptWorktree{Branch: "refs/heads/" + dirty.Branch, Head: dirty.Head, Present: true})
		if report := f.runPass(); report.Disposition != app.RetirementRetired {
			t.Fatalf("pass = %+v, want retired", report)
		}
		if candidates := f.triage(""); len(candidates) != 0 {
			t.Fatalf("candidates = %+v, want none once retired", candidates)
		}
	})

	verdicts := []struct {
		name      string
		setup     func(f *retireFixture)
		wantPass  bool
		wantWhy   app.WorktreeRetirementDisposition
		wantEmpty bool
	}{
		{name: "a head and tip never checked", wantPass: true},
		{name: "the target branch is gone", setup: func(f *retireFixture) { f.git.deleteRef(detectMainRef) }, wantWhy: app.RetirementNotMerged},
		{name: "the repository was moved away", setup: func(f *retireFixture) {
			f.git.setAttemptRepository("/moved/repo", "/moved/repo/.git")
		}, wantWhy: app.RetirementHistoryMissing},
		{name: "a moved repository with an unresolved operation", setup: func(f *retireFixture) {
			f.seedUnresolvedRemoval()
			f.git.setAttemptRepository("/moved/repo", "/moved/repo/.git")
		}, wantWhy: app.RetirementHistoryMissing},
		{name: "an unresolved operation", setup: func(f *retireFixture) { f.seedUnresolvedRemoval() }, wantPass: true},
		{name: "an unresolved operation on an unmerged run", setup: func(f *retireFixture) {
			f.git.setRef(detectMainRef, f.git.newCommit("tree-main-behind", f.base))
			f.seedUnresolvedRemoval()
		}, wantPass: true},
		{name: "two latest integrated heads disagree", setup: func(f *retireFixture) {
			f.seedIntegration(f.base, f.git.newCommit("tree-other", f.base), f.tc.Clock.Now())
		}, wantWhy: app.RetirementCheckFailed},
		{name: "the target branch cannot be read", setup: func(f *retireFixture) {
			f.tc.Commands.RunHook = func(ctx context.Context, cmd app.Command) (app.CommandResult, bool, error) {
				if slices.Contains(cmd.Argv, "rev-parse") {
					return app.CommandResult{}, true, errors.New("fork/exec: resource temporarily unavailable")
				}
				return f.baseHook(ctx, cmd)
			}
		}, wantWhy: app.RetirementCheckFailed},
		{name: "a failed check of this head and tip", setup: func(f *retireFixture) {
			f.tc.Commands.RunHook = func(ctx context.Context, cmd app.Command) (app.CommandResult, bool, error) {
				if len(cmd.Argv) > 1 && cmd.Argv[1] == "check-exec" {
					return app.CommandResult{ExitCode: 128}, true, nil
				}
				return f.baseHook(ctx, cmd)
			}
			if report := f.runPass(); report.Disposition != app.RetirementCheckFailed {
				f.t.Fatalf("pass = %+v, want a failed check", report)
			}
			f.tc.Commands.RunHook = f.baseHook
		}, wantWhy: app.RetirementCheckFailed},
		{name: "the invoking run is excluded", wantEmpty: true},
	}
	for _, tc := range verdicts {
		t.Run("verdict: "+tc.name, func(t *testing.T) {
			f := newRetireFixture(t)
			f.addAttempt(1, fakeAttemptWorktree{}, nil)
			f.release()
			if tc.setup != nil {
				tc.setup(f)
			}
			exclude := ""
			if tc.wantEmpty {
				exclude = f.fr.RunID.String()
			}
			candidates := f.triage(exclude)
			if tc.wantEmpty {
				if len(candidates) != 0 {
					t.Fatalf("candidates = %+v, want the invoking run excluded", candidates)
				}
				return
			}
			verdict := f.only(candidates)
			if verdict.NeedsPass != tc.wantPass || verdict.Disposition != tc.wantWhy {
				t.Fatalf("verdict = %+v, want pass=%v why=%q", verdict, tc.wantPass, tc.wantWhy)
			}
		})
	}

	t.Run("a read store without the triage read fails closed", func(t *testing.T) {
		f := newRetireFixture(t)
		plain := *f.tc.Controller
		plain.Read = plainReadStore{inner: f.tc.Store}
		if _, err := plain.RetirementCandidates(context.Background(), detectRoot, ""); !errors.Is(err, app.ErrRetirementReadStoreUnsupported) {
			t.Fatalf("RetirementCandidates() error = %v, want ErrRetirementReadStoreUnsupported", err)
		}
	})
}

// seedUnresolvedRemoval records a reconciling worktree.retire operation
// left by an earlier generation.
func (f *retireFixture) seedUnresolvedRemoval() {
	opID := identity.OperationID(f.tc.IDs.NewID())
	f.tc.Store.Operations[opID] = app.Operation{
		ID: opID, RunID: f.fr.RunID, Generation: 1, Kind: app.OpWorktreeRetire, State: app.OperationReconciling,
		Intent: map[string]any{"decision": "remove"}, CreatedAt: f.tc.Clock.Now(), UpdatedAt: f.tc.Clock.Now(),
	}
}

// TestFakeRetirementCandidatesContract proves the fake's triage read
// honors the SQLite store's contract (TestListRetirementCandidates):
// terminal feature runs with a target, the fact unset and content
// integrated, in sequence order, with integrated rows oldest first,
// retirement checks newest first and the unresolved flag over both
// retirement kinds only.
func TestFakeRetirementCandidatesContract(t *testing.T) {
	tc := newTestController(defaultPolicy())
	seed := func(seq int, state run.RunState, target string, integrations ...[2]string) identity.RunID {
		fr := seedRetirableRun(t, tc, state, target)
		tc.Store.Runs[fr.RunID].value.Sequence = seq
		task := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskIntegrated)
		for i, pair := range integrations {
			id := identity.IntegrationID(tc.IDs.NewID())
			created := time.Date(2026, 9, 16, 12, 0, 10-i, 0, time.UTC)
			tc.Store.Integrations[id] = &entityRow[run.Integration]{value: run.Integration{
				ID: id, RunID: fr.RunID, TaskID: task, ResultID: identity.ResultID(tc.IDs.NewID()),
				PremergeHeadOID: pair[0], MergeCommitOID: pair[1], State: run.IntegrationIntegrated,
				CreatedAt: created, UpdatedAt: created,
			}, revision: 1}
		}
		return fr.RunID
	}
	op := func(runID identity.RunID, kind app.OperationKind, state app.OperationState, second int) identity.OperationID {
		id := identity.OperationID(tc.IDs.NewID())
		at := time.Date(2026, 9, 16, 14, 0, second, 0, time.UTC)
		tc.Store.Operations[id] = app.Operation{ID: id, RunID: runID, Generation: 1, Kind: kind, State: state, CreatedAt: at, UpdatedAt: at}
		return id
	}

	stopped := seed(3, run.RunStopped, "refs/heads/trunk", [2]string{"base", "merge-3"})
	first := seed(1, run.RunCompleted, "refs/heads/main", [2]string{"merge-1", "merge-1"}, [2]string{"base", "merge-1"})
	failed := seed(2, run.RunFailed, "refs/heads/main", [2]string{"base", "merge-2"})
	seed(4, run.RunRunning, "refs/heads/main", [2]string{"base", "merge-4"})
	seed(5, run.RunCompleted, "", [2]string{"base", "merge-5"})
	retired := seed(6, run.RunCompleted, "refs/heads/main", [2]string{"base", "merge-6"})
	tc.Store.WorktreesRetiredAt[retired] = time.Date(2026, 9, 16, 13, 0, 0, 0, time.UTC)
	seed(7, run.RunCompleted, "refs/heads/main")
	seed(8, run.RunCompleted, "refs/heads/main", [2]string{"base", "base"})
	solo := seed(9, run.RunCompleted, "refs/heads/main", [2]string{"base", "merge-9"})
	tc.Store.Snapshots[solo] = app.RunSnapshot{StateRoot: "/state"}

	older := op(first, app.OpRetirementCheck, app.OperationFailed, 1)
	newer := op(first, app.OpRetirementCheck, app.OperationSucceeded, 2)
	op(first, app.OpWorktreeRetire, app.OperationReconciling, 3)
	op(first, app.OpPaneOpen, app.OperationPending, 4)
	op(failed, app.OpWorktreeRetire, app.OperationSucceeded, 5)
	op(failed, app.OpPaneOpen, app.OperationReconciling, 6)

	records, err := tc.Store.ListRetirementCandidates(context.Background(), "/repo")
	if err != nil {
		t.Fatalf("ListRetirementCandidates() error = %v", err)
	}
	var got []identity.RunID
	for i := range records {
		got = append(got, records[i].RunID)
	}
	if want := []identity.RunID{first, failed, stopped}; !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	one := records[0]
	if one.Sequence != 1 || one.RepositoryRoot != "/repo" || one.TargetBranch != "refs/heads/main" || !one.Unresolved {
		t.Fatalf("first record = %+v", one)
	}
	if len(one.Integrated) != 2 || one.Integrated[0].PremergeHeadOID != "base" || !one.Integrated[0].CreatedAt.Before(one.Integrated[1].CreatedAt) {
		t.Fatalf("integrated rows = %+v, want oldest first", one.Integrated)
	}
	if len(one.Checks) != 2 || one.Checks[0].ID != newer || one.Checks[1].ID != older {
		t.Fatalf("checks = %+v, want newest first", one.Checks)
	}
	if records[1].Unresolved || len(records[1].Checks) != 0 || records[2].TargetBranch != "refs/heads/trunk" {
		t.Fatalf("records = %+v", records[1:])
	}
	if none, err := tc.Store.ListRetirementCandidates(context.Background(), "/unknown"); err != nil || len(none) != 0 {
		t.Fatalf("an unknown root = %+v, %v", none, err)
	}
}
