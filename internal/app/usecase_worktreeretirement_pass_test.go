package app_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/run"
)

// pass runs one RetireWorktrees pass under the fixture's current lease,
// recording how many spawns had happened when the first removal was
// announced (-1 when it never was).
func (f *retireFixture) pass() (report app.WorktreeRetirementReport, announcedAt int) {
	f.t.Helper()
	announcedAt, calls := -1, 0
	report, err := f.tc.Controller.RetireWorktrees(context.Background(), f.handle, app.RetireWorktreesOptions{
		HOPPath:     detectHOP,
		Environ:     []string{"PATH=/usr/bin:/bin", "ANTHROPIC_API_KEY=secret-value"},
		InspectPath: f.inspector(),
		BeforeFirstRemoval: func() {
			calls++
			if announcedAt < 0 {
				announcedAt = len(f.spawns)
			}
		},
	})
	if err != nil {
		f.t.Fatalf("RetireWorktrees() error = %v", err)
	}
	if calls > 1 {
		f.t.Fatalf("BeforeFirstRemoval called %d times, want at most once", calls)
	}
	if report.RunID != f.fr.RunID.String() || report.Sequence != f.tc.Store.Runs[f.fr.RunID].value.Sequence {
		f.t.Fatalf("report identity = %s r%d, want the fixture run", report.RunID, report.Sequence)
	}
	return report, announcedAt
}

func (f *retireFixture) retiredAt() bool {
	_, set := f.tc.Store.WorktreesRetiredAt[f.fr.RunID]
	return set
}

func outcomes(lines []app.WorktreeRetirementLine) []app.WorktreeRetirementOutcome {
	out := make([]app.WorktreeRetirementOutcome, 0, len(lines))
	for i := range lines {
		out = append(out, lines[i].Outcome)
	}
	return out
}

// TestRetireWorktrees covers the pass driver: detection gates removal,
// every final row counts, the fact is set exactly when every row is final
// and never twice, a moved or replaced repository is never touched, and a
// pass that cannot finish leaves the fact unset without an error.
func TestRetireWorktrees(t *testing.T) {
	t.Run("a merged run retires every worktree once and records the fact", func(t *testing.T) {
		f := newRetireFixture(t)
		a := f.addAttempt(1, fakeAttemptWorktree{}, nil)
		gone := f.addAttempt(2, fakeAttemptWorktree{}, func(w *fakeAttemptWorktree) { w.Present, w.Unlisted = false, true })
		other := f.addAttempt(3, fakeAttemptWorktree{}, func(w *fakeAttemptWorktree) { w.Branch = "refs/heads/elsewhere" })
		b := f.addAttempt(4, fakeAttemptWorktree{Ignored: true}, nil)
		report, announcedAt := f.pass()
		if report.Disposition != app.RetirementRetired || report.Removed != 2 || report.Absent != 1 || report.Released != 1 {
			t.Fatalf("report = %+v, want retired with 2 removed, 1 absent, 1 released", report)
		}
		want := []app.WorktreeRetirementOutcome{app.WorktreeOutcomeRemoved, app.WorktreeOutcomeAbsent, app.WorktreeOutcomeReleased, app.WorktreeOutcomeRemoved}
		if got := outcomes(report.Worktrees); !slices.Equal(got, want) {
			t.Fatalf("lines = %v, want %v", got, want)
		}
		if line := report.Worktrees[2]; line.Released != app.ReleasedOtherBranch || line.LeftOnDisk || line.Path != other.Recorded || line.Branch != other.Branch {
			t.Fatalf("released line = %+v", line)
		}
		if announcedAt != 1 {
			t.Fatalf("the first removal was announced after %d spawns, want after the detection check alone", announcedAt)
		}
		at, set := f.tc.Store.WorktreesRetiredAt[f.fr.RunID]
		if !set || !at.Equal(f.tc.Clock.Now()) {
			t.Fatalf("fact = %v (set %v), want the clock's time", at, set)
		}
		for _, w := range []retireAttempt{a, b} {
			if f.rowState(w.WorktreeID) != run.WorktreeRemoved {
				t.Fatalf("row %s = %s, want removed", w.Branch, f.rowState(w.WorktreeID))
			}
		}
		if f.rowState(gone.WorktreeID) != run.WorktreeAbsent || f.rowState(other.WorktreeID) != run.WorktreeReleased {
			t.Fatalf("rows = %s, %s; want absent and released", f.rowState(gone.WorktreeID), f.rowState(other.WorktreeID))
		}
		if len(f.retires()) != 4 || len(f.checks()) != 1 {
			t.Fatalf("journal = %d retire and %d check operations, want 4 and 1", len(f.retires()), len(f.checks()))
		}
		if err := f.tc.Controller.ReleaseRetirement(context.Background(), f.handle); err != nil {
			t.Fatal(err)
		}
		operations, transitions := len(f.tc.Store.Operations), len(f.tc.Store.Transitions)
		for range 2 {
			if _, err := f.tc.Controller.AcquireForRetirement(context.Background(), f.fr.RunID.String(), "status-8"); !errors.Is(err, app.ErrRetirementNotEligible) {
				t.Fatalf("a retired run was acquired again: %v", err)
			}
		}
		if len(f.tc.Store.Operations) != operations || len(f.tc.Store.Transitions) != transitions {
			t.Fatalf("a retired run's later passes journaled something")
		}
		if removes, _, _ := f.git.attemptLog(); len(removes) != 2 {
			t.Fatalf("removals = %v, want exactly two, never repeated", removes)
		}
	})

	t.Run("a retained worktree keeps the fact unset until a later pass removes it", func(t *testing.T) {
		f := newRetireFixture(t)
		f.addAttempt(1, fakeAttemptWorktree{}, nil)
		dirty := f.addAttempt(2, fakeAttemptWorktree{Untracked: true}, nil)
		report, _ := f.pass()
		if report.Disposition != app.RetirementInProgress || report.Removed != 1 || f.retiredAt() {
			t.Fatalf("report = %+v (fact %v), want in progress with one removed", report, f.retiredAt())
		}
		if got := outcomes(report.Worktrees); !slices.Equal(got, []app.WorktreeRetirementOutcome{app.WorktreeOutcomeRemoved, app.WorktreeOutcomeRetained}) || report.Worktrees[1].Retained != app.RetainedUncommittedChanges {
			t.Fatalf("lines = %+v", report.Worktrees)
		}
		f.acquire()
		if again, _ := f.pass(); again.Disposition != app.RetirementInProgress || len(again.Worktrees) != 1 || again.Worktrees[0].Branch != dirty.Branch {
			t.Fatalf("while still dirty: %+v, want only the retained worktree reported", again)
		}
		f.git.addAttemptWorktree(dirty.Listed, fakeAttemptWorktree{Branch: "refs/heads/" + dirty.Branch, Head: dirty.Head, Present: true})
		f.acquire()
		final, _ := f.pass()
		if final.Disposition != app.RetirementRetired || final.Removed != 2 || len(final.Worktrees) != 1 || !f.retiredAt() {
			t.Fatalf("after cleaning: %+v, want retired with both removed", final)
		}
		if len(f.checks()) != 1 {
			t.Fatalf("detection repeated after the settled merge: %d checks", len(f.checks()))
		}
	})

	t.Run("a worktree listing cut by the capture bound defers the run: every row retained, nothing removed or settled", func(t *testing.T) {
		f := newRetireFixture(t)
		a := f.addAttempt(1, fakeAttemptWorktree{}, nil)
		gone := f.addAttempt(2, fakeAttemptWorktree{}, func(w *fakeAttemptWorktree) { w.Present = false })
		// The first kept prefix ends on a record boundary just before the
		// first candidate: without the truncation report it reads as a
		// complete listing that names neither checkout.
		f.git.fillListingBefore(t, a.Listed, app.RetirementListingOutputBytesForTest)
		for range 2 {
			report, announcedAt := f.pass()
			if report.Disposition != app.RetirementInProgress || report.Removed+report.Absent+report.Released != 0 || announcedAt != -1 || f.retiredAt() {
				t.Fatalf("report = %+v (fact %v), want in progress with nothing final", report, f.retiredAt())
			}
			want := []app.WorktreeRetirementLine{
				{Branch: a.Branch, Path: a.Recorded, Outcome: app.WorktreeOutcomeRetained, Retained: app.RetainedInspectionFailed},
				{Branch: gone.Branch, Path: gone.Recorded, Outcome: app.WorktreeOutcomeRetained, Retained: app.RetainedInspectionFailed},
			}
			if !slices.Equal(report.Worktrees, want) {
				t.Fatalf("lines = %+v, want both retained as uninspectable", report.Worktrees)
			}
			if len(f.retires()) != 0 || f.rowState(a.WorktreeID) != run.WorktreeActive || f.rowState(gone.WorktreeID) != run.WorktreeActive {
				t.Fatalf("a cut listing settled something: %d retire operations, rows %s, %s", len(f.retires()), f.rowState(a.WorktreeID), f.rowState(gone.WorktreeID))
			}
			if removes, _, _ := f.git.attemptLog(); len(removes) != 0 {
				t.Fatalf("removals = %v, want none", removes)
			}
			f.acquire()
		}

		// A complete listing lets the next pass finish.
		f.git.addAttemptWorktree(fakeListingSibling, fakeAttemptWorktree{Head: fakeGitMainHead, Present: true, Locked: true, LockReason: "x"})
		final, _ := f.pass()
		if final.Disposition != app.RetirementRetired || final.Removed != 1 || final.Absent != 1 || !f.retiredAt() {
			t.Fatalf("after the listing fits: %+v, want retired with one removed and the gone one pruned as absent", final)
		}
		if _, still := f.git.attemptWorktree(fakeListingSibling); !still {
			t.Fatal("the unrelated sibling checkout was removed")
		}
	})

	t.Run("a merged run without worktree rows is retired at once", func(t *testing.T) {
		f := newRetireFixture(t)
		report, announcedAt := f.pass()
		if report.Disposition != app.RetirementRetired || len(report.Worktrees) != 0 || announcedAt != -1 || !f.retiredAt() {
			t.Fatalf("report = %+v, announced %d, fact %v", report, announcedAt, f.retiredAt())
		}
	})

	t.Run("a stopped run carrying its stop request retires", func(t *testing.T) {
		f := newRetireFixture(t)
		row := f.tc.Store.Runs[f.fr.RunID]
		row.value.State, row.value.StopRequested = run.RunStopped, true
		f.addAttempt(1, fakeAttemptWorktree{}, nil)
		f.acquire()
		if report, _ := f.pass(); report.Disposition != app.RetirementRetired || report.Removed != 1 {
			t.Fatalf("report = %+v, want retired", report)
		}
	})

	gated := []struct {
		name  string
		setup func(f *retireFixture)
		want  app.WorktreeRetirementDisposition
		// checks is the number of retirement.check operations the pass
		// journals.
		checks int
	}{
		{"not merged", func(f *retireFixture) {
			f.git.setRef(detectMainRef, f.git.newCommit("tree-main-behind", f.base))
		}, app.RetirementNotMerged, 1},
		{"the target branch is gone", func(f *retireFixture) { f.git.deleteRef(detectMainRef) }, app.RetirementNotMerged, 0},
		{"nothing integrated", func(f *retireFixture) { clear(f.tc.Store.Integrations) }, app.RetirementNothingIntegrated, 0},
		{"a failing check", func(f *retireFixture) {
			f.tc.Commands.RunHook = func(ctx context.Context, cmd app.Command) (app.CommandResult, bool, error) {
				if len(cmd.Argv) > 1 && cmd.Argv[1] == "check-exec" {
					return app.CommandResult{ExitCode: 128}, true, nil
				}
				return f.baseHook(ctx, cmd)
			}
		}, app.RetirementCheckFailed, 1},
	}
	for _, tc := range gated {
		t.Run("no removal: "+tc.name, func(t *testing.T) {
			f := newRetireFixture(t)
			a := f.addAttempt(1, fakeAttemptWorktree{}, nil)
			tc.setup(f)
			report, announcedAt := f.pass()
			if report.Disposition != tc.want || len(report.Worktrees) != 0 || announcedAt != -1 {
				t.Fatalf("report = %+v, announced %d; want %s with nothing removed", report, announcedAt, tc.want)
			}
			if len(f.retires()) != 0 || len(f.checks()) != tc.checks || f.rowState(a.WorktreeID) != run.WorktreeActive || f.retiredAt() {
				t.Fatalf("journal: %d retire, %d check operations; row %s; fact %v", len(f.retires()), len(f.checks()), f.rowState(a.WorktreeID), f.retiredAt())
			}
			if removes, _, _ := f.git.attemptLog(); len(removes) != 0 {
				t.Fatalf("removals = %v, want none", removes)
			}
		})
	}

	repositories := []struct {
		name string
		move func(f *retireFixture)
	}{
		{"the repository was moved away", func(f *retireFixture) {
			f.git.setAttemptRepository("/moved/repo", "/moved/repo/.git")
			delete(f.present, detectRoot)
		}},
		{"another repository without the run's history now lives at the root", func(f *retireFixture) {
			f.git.mu.Lock()
			delete(f.git.commits, f.merge)
			f.git.mu.Unlock()
		}},
	}
	for _, tc := range repositories {
		t.Run("never acted on: "+tc.name, func(t *testing.T) {
			f := newRetireFixture(t)
			f.addAttempt(1, fakeAttemptWorktree{}, nil)
			stranger := f.addAttempt(2, fakeAttemptWorktree{Modified: true}, nil)
			first, _ := f.pass()
			if first.Disposition != app.RetirementInProgress {
				t.Fatalf("first pass = %+v, want in progress", first)
			}
			// An unresolved removal whose group is gone — recovery would
			// settle it — and a checkout git would no longer list — removal
			// would release it — are both waiting when the repository
			// changes underneath.
			pending := f.addAttempt(3, fakeAttemptWorktree{}, nil)
			f.interruptRemoval(true, nil)
			f.acquire()
			if interrupted, _ := f.pass(); interrupted.Disposition != app.RetirementInProgress {
				t.Fatalf("second pass = %+v", interrupted)
			}
			unresolved := f.retires()[len(f.retires())-1]
			if unresolved.State != app.OperationReconciling {
				t.Fatalf("the interrupted removal = %+v, want reconciling", unresolved)
			}
			f.git.addAttemptWorktree(stranger.Listed, fakeAttemptWorktree{Branch: "refs/heads/" + stranger.Branch, Head: stranger.Head, Present: true, Unlisted: true})
			f.tc.Commands.RunHook = f.baseHook
			tc.move(f)
			operations := len(f.tc.Store.Operations)
			f.tc.Clock.Advance(time.Minute)
			f.acquire()
			report, announcedAt := f.pass()
			if report.Disposition != app.RetirementHistoryMissing || len(report.Worktrees) != 0 || announcedAt != -1 {
				t.Fatalf("report = %+v, want history-missing with nothing done", report)
			}
			if after := f.tc.Store.Operations[unresolved.ID]; len(f.tc.Store.Operations) != operations || after.State != app.OperationReconciling || !after.UpdatedAt.Equal(unresolved.UpdatedAt) {
				t.Fatalf("the pass journaled or recovered something over a changed repository: %+v", after)
			}
			if f.rowState(stranger.WorktreeID) != run.WorktreeActive || f.rowState(pending.WorktreeID) != run.WorktreeActive || f.retiredAt() {
				t.Fatalf("rows moved over a changed repository: %s, %s", f.rowState(stranger.WorktreeID), f.rowState(pending.WorktreeID))
			}
		})
	}

	t.Run("an unresolved earlier removal blocks the pass", func(t *testing.T) {
		f := newRetireFixture(t)
		f.addAttempt(1, fakeAttemptWorktree{}, nil)
		b := f.addAttempt(2, fakeAttemptWorktree{}, nil)
		var inner []string
		f.onRemoval(func(cmd app.Command) (app.CommandResult, bool, error) {
			if !slices.Contains(cmd.Argv, b.Listed) {
				return app.CommandResult{}, false, nil
			}
			for i, arg := range cmd.Argv {
				if arg == "--op" {
					f.git.OnCheckExec(cmd.Argv[i+1], nil)
				}
				if arg == "--" {
					inner = cmd.Argv[i+1:]
					break
				}
			}
			return app.CommandResult{}, true, errors.New("controller died")
		})
		if report, _ := f.pass(); report.Disposition != app.RetirementInProgress {
			t.Fatalf("first pass = %+v", report)
		}
		f.tc.Commands.RunHook = f.baseHook
		f.tc.Groups.Processes[detectPID] = []app.GroupProcess{{PID: detectPID, Argv: inner}}
		f.acquire()
		report, _ := f.pass()
		if report.Disposition != app.RetirementBlocked || len(report.Worktrees) != 0 || f.retiredAt() {
			t.Fatalf("report = %+v, want blocked", report)
		}
		if f.rowState(b.WorktreeID) != run.WorktreeActive {
			t.Fatalf("a blocked row moved")
		}
	})

	t.Run("a pass that loses its lease before an act leaves the fact unset", func(t *testing.T) {
		f := newRetireFixture(t)
		f.addAttempt(1, fakeAttemptWorktree{}, nil)
		heartbeats := 0
		f.tc.Store.HeartbeatHook = func() {
			heartbeats++
			if heartbeats < 2 {
				return
			}
			f.tc.Store.mu.Lock()
			defer f.tc.Store.mu.Unlock()
			f.tc.Store.Leases[f.fr.RunID].held = false
		}
		report, announcedAt := f.pass()
		if report.Disposition != app.RetirementInterrupted || announcedAt != -1 || f.retiredAt() {
			t.Fatalf("report = %+v, announced %d; want interrupted before any removal", report, announcedAt)
		}
		if got := outcomes(report.Worktrees); !slices.Equal(got, []app.WorktreeRetirementOutcome{app.WorktreeOutcomeNotDispatched}) {
			t.Fatalf("lines = %v", got)
		}
	})

	t.Run("a solo run is refused", func(t *testing.T) {
		f := newRetireFixture(t)
		f.tc.Store.Snapshots[f.fr.RunID] = app.RunSnapshot{StateRoot: "/state"}
		if _, err := f.tc.Controller.RetireWorktrees(context.Background(), f.handle, app.RetireWorktreesOptions{HOPPath: detectHOP}); !errors.Is(err, app.ErrRetirementNotEligible) {
			t.Fatalf("RetireWorktrees() error = %v, want ErrRetirementNotEligible", err)
		}
	})
}

// TestStatusWorktreeLines covers the per-row status input: after a pass,
// hop status -run's detail names every feature worktree row with its
// state, a released row's reason, and an active row's refused category or
// unfinished removal from its journal; a solo run carries none.
func TestStatusWorktreeLines(t *testing.T) {
	f := newRetireFixture(t)
	removed := f.addAttempt(1, fakeAttemptWorktree{}, nil)
	absent := f.addAttempt(2, fakeAttemptWorktree{}, func(w *fakeAttemptWorktree) { w.Present, w.Unlisted = false, true })
	released := f.addAttempt(3, fakeAttemptWorktree{}, func(w *fakeAttemptWorktree) { w.Branch = "" })
	refused := f.addAttempt(4, fakeAttemptWorktree{}, nil)
	incomplete := f.addAttempt(5, fakeAttemptWorktree{}, nil)
	unresolved := f.addAttempt(6, fakeAttemptWorktree{}, nil)
	dirty := f.addAttempt(7, fakeAttemptWorktree{Modified: true}, nil)
	f.onRemoval(func(cmd app.Command) (app.CommandResult, bool, error) {
		target := cmd.Argv[len(cmd.Argv)-1]
		switch target {
		case refused.Listed:
			w, _ := f.git.attemptWorktree(target)
			w.Locked = true
			f.git.addAttemptWorktree(target, w)
		case incomplete.Listed:
			w, _ := f.git.attemptWorktree(target)
			w.Present = false
			f.git.addAttemptWorktree(target, w)
			return app.CommandResult{ExitCode: 128}, true, nil
		case unresolved.Listed:
			return app.CommandResult{}, true, errors.New("fork/exec: resource temporarily unavailable")
		}
		return app.CommandResult{}, false, nil
	})
	if report, _ := f.pass(); report.Disposition != app.RetirementInProgress {
		t.Fatalf("pass = %+v, want in progress", report)
	}
	status, err := f.tc.Controller.Status(context.Background(), app.StatusRequest{RunID: f.fr.RunID.String()})
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	want := []app.WorktreeView{
		{Branch: removed.Branch, Path: removed.Recorded, State: "removed"},
		{Branch: absent.Branch, Path: absent.Recorded, State: "absent"},
		{Branch: released.Branch, Path: released.Recorded, State: "released", Released: app.ReleasedDetached},
		{Branch: refused.Branch, Path: refused.Recorded, State: "active", Retained: app.RetainedLocked},
		{Branch: incomplete.Branch, Path: incomplete.Recorded, State: "active", Removal: "incomplete"},
		{Branch: unresolved.Branch, Path: unresolved.Recorded, State: "active", Removal: "unresolved"},
		{Branch: dirty.Branch, Path: dirty.Recorded, State: "active"},
	}
	got := status.Detail.Worktrees
	if len(got) == len(want) && got[3].EvidencePath != "" {
		want[3].EvidencePath = got[3].EvidencePath
	}
	if !slices.Equal(got, want) {
		t.Fatalf("worktree lines =\n%+v\nwant\n%+v", got, want)
	}
	if !strings.HasSuffix(got[3].EvidencePath, "/stderr") || !strings.HasPrefix(got[3].EvidencePath, "/state/runs/"+f.fr.RunID.String()+"/retirement/") {
		t.Fatalf("the refused row's evidence = %q, want its retained stderr", got[3].EvidencePath)
	}

	// The next pass settles the unresolved removal (no claim: never
	// executed) and removes it afresh; the interrupted shape comes from a
	// claimed act that died.
	f.tc.Commands.RunHook = f.baseHook
	interrupted := f.addAttempt(8, fakeAttemptWorktree{}, nil)
	f.onRemoval(func(cmd app.Command) (app.CommandResult, bool, error) {
		if cmd.Argv[len(cmd.Argv)-1] != interrupted.Listed {
			return app.CommandResult{}, false, nil
		}
		for i, arg := range cmd.Argv {
			if arg == "--op" {
				f.git.OnCheckExec(cmd.Argv[i+1], nil)
			}
		}
		return app.CommandResult{}, true, errors.New("controller died")
	})
	f.acquire()
	f.pass()
	f.tc.Commands.RunHook = f.baseHook
	f.acquire()
	if blocking := f.recover(); blocking != "" {
		t.Fatalf("recovery blocked: %s", blocking)
	}
	status, err = f.tc.Controller.Status(context.Background(), app.StatusRequest{RunID: f.fr.RunID.String()})
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if got := status.Detail.Worktrees; got[7].Removal != "interrupted" || got[7].State != "active" || got[5].Removal != "" {
		t.Fatalf("after recovery alone: %+v, want the dead act interrupted and the never-executed one plain", got)
	}
	f.acquire()
	f.pass()
	status, err = f.tc.Controller.Status(context.Background(), app.StatusRequest{RunID: f.fr.RunID.String()})
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	lines := status.Detail.Worktrees
	if len(lines) != 8 || lines[5].State != "removed" || lines[5].Removal != "" || lines[6].Retained != "" {
		t.Fatalf("after recovery: %+v", lines)
	}
	if last := lines[7]; last.State != "removed" {
		t.Fatalf("the interrupted row = %+v, want removed once its checkout was clean", last)
	}

	solo := newTestController(defaultPolicy())
	_, detail := startedRun(t, solo)
	soloStatus, err := solo.Controller.Status(context.Background(), app.StatusRequest{RunID: detail.RunID.String()})
	if err != nil || soloStatus.Detail.Worktrees != nil || soloStatus.Detail.WorktreePath == "" {
		t.Fatalf("a solo run's detail = %+v, %v; want its single worktree path and no per-row lines", soloStatus.Detail, err)
	}
}
