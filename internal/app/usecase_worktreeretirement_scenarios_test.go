package app_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// retireResults returns the outcome results of the row's worktree.retire
// operations, oldest first; an unsettled operation reads as its state.
func (f *retireFixture) retireResults(worktreeID identity.WorktreeID) []string {
	f.t.Helper()
	var results []string
	for _, op := range f.retires() { //nolint:gocritic // rangeValCopy: test reads of small operation values.
		intent, _ := jsonFields(op.Intent)
		if intent["worktree_id"] != worktreeID.String() {
			continue
		}
		if op.State == app.OperationPending || op.State == app.OperationReconciling {
			results = append(results, string(op.State))
			continue
		}
		results = append(results, outcomeResult(f.t, &op))
	}
	return results
}

// TestWorktreeRetireDecisionTable drives every worktree.retire cell of
// the design note's section 7 decision table through two whole passes: the
// first dies at the cell's crash point, and the successor pass — a newer
// generation, as every pass is — settles the operation by claim presence,
// then finishes the row the way the table says. No cell ever removes a
// checkout twice or with force.
func TestWorktreeRetireDecisionTable(t *testing.T) {
	cells := []struct {
		name string
		// start edits the checkout before the first pass.
		start func(w *fakeAttemptWorktree)
		// crash sets up the first pass's crash point.
		crash func(f *retireFixture, a *retireAttempt)
		// group is the claimed group the successor observes: nil when it is
		// gone.
		group func(inner []string) []app.GroupProcess
		// wantResults are the row's operation results after the successor.
		wantResults []string
		wantRow     run.WorktreeState
		wantPass    app.WorktreeRetirementDisposition
		wantLine    app.WorktreeRetirementOutcome
		wantKind    app.WorktreeRetainedCategory
		wantRemoves int
		wantSignal  bool
	}{
		{
			name: "intent committed, lease lost before dispatch: never executed, then removed",
			crash: func(f *retireFixture, _ *retireAttempt) {
				heartbeats := 0
				f.tc.Store.HeartbeatHook = func() {
					heartbeats++
					if heartbeats < 2 {
						return
					}
					f.tc.Store.mu.Lock()
					defer f.tc.Store.mu.Unlock()
					f.tc.Store.Leases[f.fr.RunID].held = false
					f.tc.Store.HeartbeatHook = nil
				}
			},
			wantResults: []string{"never-executed", "removed"}, wantRow: run.WorktreeRemoved,
			wantPass: app.RetirementRetired, wantLine: app.WorktreeOutcomeRemoved, wantRemoves: 1,
		},
		{
			name:        "dispatched, the controller died before any claim: never executed, then removed",
			crash:       func(f *retireFixture, _ *retireAttempt) { f.interruptRemoval(false, nil) },
			wantResults: []string{"never-executed", "removed"}, wantRow: run.WorktreeRemoved,
			wantPass: app.RetirementRetired, wantLine: app.WorktreeOutcomeRemoved, wantRemoves: 1,
		},
		{
			name: "claimed, the removal finished: adopted as removed",
			crash: func(f *retireFixture, _ *retireAttempt) {
				f.interruptRemoval(true, func(inner []string) { f.git.runGitArgv(inner[1:]) })
			},
			wantResults: []string{"removed"}, wantRow: run.WorktreeRemoved,
			wantPass: app.RetirementRetired, wantRemoves: 1,
		},
		{
			name:        "claimed, nothing removed, still clean: interrupted, then removed",
			crash:       func(f *retireFixture, _ *retireAttempt) { f.interruptRemoval(true, nil) },
			wantResults: []string{"interrupted", "removed"}, wantRow: run.WorktreeRemoved,
			wantPass: app.RetirementRetired, wantLine: app.WorktreeOutcomeRemoved, wantRemoves: 1,
		},
		{
			name: "claimed, killed mid-delete leaving changes: interrupted, retained as an interrupted removal",
			crash: func(f *retireFixture, a *retireAttempt) {
				f.interruptRemoval(true, func([]string) {
					w, _ := f.git.attemptWorktree(a.Listed)
					w.Modified = true
					f.git.addAttemptWorktree(a.Listed, w)
				})
			},
			wantResults: []string{"interrupted"}, wantRow: run.WorktreeActive,
			wantPass: app.RetirementInProgress, wantLine: app.WorktreeOutcomeRetained, wantKind: app.RetainedInterruptedRemoval,
		},
		{
			name: "claimed, the directory went but git still lists it: interrupted, then pruned as absent",
			crash: func(f *retireFixture, a *retireAttempt) {
				f.interruptRemoval(true, func([]string) {
					w, _ := f.git.attemptWorktree(a.Listed)
					w.Present = false
					f.git.addAttemptWorktree(a.Listed, w)
				})
			},
			wantResults: []string{"interrupted", "absent"}, wantRow: run.WorktreeAbsent,
			wantPass: app.RetirementRetired, wantLine: app.WorktreeOutcomeAbsent, wantRemoves: 1,
		},
		{
			name: "claimed, git dropped its entry but the directory stays: released",
			crash: func(f *retireFixture, a *retireAttempt) {
				f.interruptRemoval(true, func([]string) {
					w, _ := f.git.attemptWorktree(a.Listed)
					w.Unlisted = true
					f.git.addAttemptWorktree(a.Listed, w)
				})
			},
			wantResults: []string{"released"}, wantRow: run.WorktreeReleased,
			wantPass: app.RetirementRetired,
		},
		{
			name:  "claimed, the removal still runs: signaled, the pass blocked",
			crash: func(f *retireFixture, _ *retireAttempt) { f.interruptRemoval(true, nil) },
			group: func(inner []string) []app.GroupProcess {
				return []app.GroupProcess{{PID: detectPID, Argv: inner}}
			},
			wantResults: []string{"reconciling"}, wantRow: run.WorktreeActive,
			wantPass: app.RetirementBlocked, wantSignal: true,
		},
		{
			name:  "claimed, the group runs something else: never signaled, the pass blocked",
			crash: func(f *retireFixture, _ *retireAttempt) { f.interruptRemoval(true, nil) },
			group: func([]string) []app.GroupProcess {
				return []app.GroupProcess{{PID: detectPID, Argv: []string{"/usr/bin/vim"}}}
			},
			wantResults: []string{"reconciling"}, wantRow: run.WorktreeActive,
			wantPass: app.RetirementBlocked,
		},
	}
	for _, tc := range cells {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetireFixture(t)
			a := f.addAttempt(1, fakeAttemptWorktree{}, tc.start)
			tc.crash(f, &a)
			if first, _ := f.pass(); first.Disposition == app.RetirementRetired {
				t.Fatalf("the first pass finished despite its crash: %+v", first)
			}
			var inner []string
			for _, op := range f.retires() {
				if fields, ok := jsonFields(op.Intent); ok {
					if argv, isList := fields["argv"].([]any); isList {
						for _, arg := range argv {
							if s, isString := arg.(string); isString {
								inner = append(inner, s)
							}
						}
					}
				}
			}
			f.tc.Commands.RunHook = f.baseHook
			if tc.group != nil {
				f.tc.Groups.Processes[detectPID] = tc.group(inner)
			}
			if !f.tc.Store.Leases[f.fr.RunID].held {
				f.handle = app.RunHandle{} // the crashed pass's lease is already gone
			}
			f.acquire()
			report, _ := f.pass()
			if report.Disposition != tc.wantPass {
				t.Fatalf("successor pass = %+v, want %s", report, tc.wantPass)
			}
			if got := f.retireResults(a.WorktreeID); !slices.Equal(got, tc.wantResults) {
				t.Fatalf("operation results = %v, want %v", got, tc.wantResults)
			}
			if got := f.rowState(a.WorktreeID); got != tc.wantRow {
				t.Fatalf("row = %s, want %s", got, tc.wantRow)
			}
			if tc.wantLine != "" {
				if len(report.Worktrees) != 1 || report.Worktrees[0].Outcome != tc.wantLine || report.Worktrees[0].Retained != tc.wantKind {
					t.Fatalf("successor lines = %+v, want %s %s", report.Worktrees, tc.wantLine, tc.wantKind)
				}
			}
			if removes, _, _ := f.git.attemptLog(); len(removes) != tc.wantRemoves {
				t.Fatalf("git removals = %v, want %d", removes, tc.wantRemoves)
			}
			if signaled := slices.Contains(f.tc.Groups.Signaled, detectPID); signaled != tc.wantSignal {
				t.Fatalf("group signaled = %v, want %v", signaled, tc.wantSignal)
			}
		})
	}
}

// TestWorktreeRetirementZombiePass is the takeover barrier: a pass whose
// lease a successor takes after the pass revalidated, but before its
// removal spawn claims, is refused at the claim — git never runs for it —
// and cannot record anything; the successor settles the orphaned intent
// as never executed and removes the checkout exactly once.
func TestWorktreeRetirementZombiePass(t *testing.T) {
	f := newRetireFixture(t)
	a := f.addAttempt(1, fakeAttemptWorktree{}, nil)
	var (
		successor app.RunHandle
		refused   error
	)
	f.onRemoval(func(cmd app.Command) (app.CommandResult, bool, error) {
		// The zombie's lease lapses and a successor acquires the run
		// between the zombie's revalidation and its spawn's claim.
		f.tc.Store.mu.Lock()
		f.tc.Store.Leases[f.fr.RunID].held = false
		f.tc.Store.mu.Unlock()
		handle, err := f.tc.Controller.AcquireForRetirement(context.Background(), f.fr.RunID.String(), "status-9")
		if err != nil {
			t.Fatalf("successor acquisition: %v", err)
		}
		successor = handle
		for i, arg := range cmd.Argv {
			if arg == "--op" {
				refused = f.tc.Store.ClaimCheckExec(context.Background(), identity.OperationID(cmd.Argv[i+1]), detectPID)
			}
		}
		if refused == nil {
			t.Fatalf("the zombie's claim was accepted under a superseded generation")
		}
		// hop check-exec refuses before exec: git never runs.
		return app.CommandResult{ExitCode: 1, Stderr: []byte("hop check-exec: the operation is not a pending exec-claimable execution\n")}, true, nil
	})
	_, err := f.tc.Controller.RetireWorktrees(context.Background(), f.handle, app.RetireWorktreesOptions{
		HOPPath: detectHOP, Environ: []string{"PATH=/usr/bin:/bin"}, InspectPath: f.inspector(),
	})
	if !errors.Is(err, app.ErrFenced) {
		t.Fatalf("the zombie pass = %v, want its outcome fenced", err)
	}
	if removes, _, _ := f.git.attemptLog(); len(removes) != 0 || f.rowState(a.WorktreeID) != run.WorktreeActive {
		t.Fatalf("the zombie removed %v or moved the row to %s", removes, f.rowState(a.WorktreeID))
	}
	if got := f.retireResults(a.WorktreeID); !slices.Equal(got, []string{"pending"}) {
		t.Fatalf("the zombie's operation = %v, want its intent still pending", got)
	}

	f.tc.Commands.RunHook = f.baseHook
	f.handle = successor
	report, _ := f.pass()
	if report.Disposition != app.RetirementRetired || f.rowState(a.WorktreeID) != run.WorktreeRemoved {
		t.Fatalf("successor = %+v, row %s; want the checkout removed", report, f.rowState(a.WorktreeID))
	}
	if got := f.retireResults(a.WorktreeID); !slices.Equal(got, []string{"never-executed", "removed"}) {
		t.Fatalf("operations = %v, want the zombie's never executed and one removal", got)
	}
	if removes, _, _ := f.git.attemptLog(); len(removes) != 1 {
		t.Fatalf("removals = %v, want exactly one", removes)
	}
}

// TestWorktreeRetirementSiblingRows covers rows that are not the pass's
// own to remove: another run's rows in the same repository stay untouched
// until that run's own pass, and a row whose record names a sibling
// attempt's checkout never removes it — the sibling's own row does,
// exactly once.
func TestWorktreeRetirementSiblingRows(t *testing.T) {
	t.Run("another run's rows wait for that run's own pass", func(t *testing.T) {
		f := newRetireFixture(t)
		own := f.addAttempt(1, fakeAttemptWorktree{}, nil)
		sibling := seedRetirableRun(t, f.tc, run.RunCompleted, detectMainRef)
		snapshot := f.tc.Store.Snapshots[sibling.RunID]
		snapshot.EnvPolicy = app.EnvPolicy{Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude}
		f.tc.Store.Snapshots[sibling.RunID] = snapshot
		siblingTask := seedImplementTask(t, f.tc, sibling.RunID, 1, "S", false, run.TaskIntegrated)
		id := identity.IntegrationID(f.tc.IDs.NewID())
		f.tc.Store.Integrations[id] = &entityRow[run.Integration]{value: run.Integration{
			ID: id, RunID: sibling.RunID, TaskID: siblingTask, ResultID: identity.ResultID(f.tc.IDs.NewID()),
			SourceCommitOID: f.merge, PremergeHeadOID: f.base, MergeCommitOID: f.merge,
			State: run.IntegrationIntegrated, CreatedAt: f.tc.Clock.Now(), UpdatedAt: f.tc.Clock.Now(),
		}, revision: 1}
		theirs := f.addAttemptFor(sibling.RunID, 11, fakeAttemptWorktree{}, nil)

		if report, _ := f.pass(); report.Disposition != app.RetirementRetired || len(report.Worktrees) != 1 {
			t.Fatalf("own pass = %+v, want only its own row retired", report)
		}
		if f.rowState(own.WorktreeID) != run.WorktreeRemoved || f.rowState(theirs.WorktreeID) != run.WorktreeActive {
			t.Fatalf("rows = own %s, sibling %s", f.rowState(own.WorktreeID), f.rowState(theirs.WorktreeID))
		}
		if _, modeled := f.git.attemptWorktree(theirs.Listed); !modeled {
			t.Fatalf("the sibling run's checkout was removed by another run's pass")
		}
		if _, retired := f.tc.Store.WorktreesRetiredAt[sibling.RunID]; retired {
			t.Fatalf("the sibling run's fact was set by another run's pass")
		}

		f.release()
		handle, err := f.tc.Controller.AcquireForRetirement(context.Background(), sibling.RunID.String(), "status-8")
		if err != nil {
			t.Fatalf("acquire the sibling: %v", err)
		}
		report, err := f.tc.Controller.RetireWorktrees(context.Background(), handle, app.RetireWorktreesOptions{
			HOPPath: detectHOP, Environ: []string{"PATH=/usr/bin:/bin"}, InspectPath: f.inspector(),
		})
		if err != nil || report.Disposition != app.RetirementRetired || f.rowState(theirs.WorktreeID) != run.WorktreeRemoved {
			t.Fatalf("sibling pass = %+v, %v; row %s", report, err, f.rowState(theirs.WorktreeID))
		}
		if removes, _, _ := f.git.attemptLog(); len(removes) != 2 {
			t.Fatalf("removals = %v, want one per checkout", removes)
		}
	})

	t.Run("a row naming a sibling's checkout never removes it", func(t *testing.T) {
		f := newRetireFixture(t)
		stray := f.addAttempt(1, fakeAttemptWorktree{}, nil)
		sibling := f.addAttempt(2, fakeAttemptWorktree{}, nil)
		// The stray row and its own worktree.create both name the
		// sibling's checkout: its provenance agrees, but git lists that
		// checkout on the sibling's branch.
		f.tc.Store.Worktrees[stray.WorktreeID].value.Path = sibling.Recorded
		for id, op := range f.tc.Store.Operations {
			intent, _ := jsonFields(op.Intent)
			if op.Kind == app.OpWorktreeCreate && intent["attempt_id"] == stray.AttemptID.String() {
				op.ActEvidence = map[string]any{"info": map[string]any{"Path": sibling.Recorded, "Branch": stray.Branch}, "base_commit": f.base}
				f.tc.Store.Operations[id] = op
			}
		}
		report, _ := f.pass()
		if report.Disposition != app.RetirementRetired || len(report.Worktrees) != 2 {
			t.Fatalf("pass = %+v", report)
		}
		if line := report.Worktrees[0]; line.Outcome != app.WorktreeOutcomeReleased || line.Released != app.ReleasedOtherBranch {
			t.Fatalf("stray row = %+v, want released as checked out on another branch", line)
		}
		if f.rowState(sibling.WorktreeID) != run.WorktreeRemoved {
			t.Fatalf("the sibling row = %s, want removed", f.rowState(sibling.WorktreeID))
		}
		removes, _, _ := f.git.attemptLog()
		if want := []string{"status.showUntrackedFiles=all " + sibling.Listed}; !slices.Equal(removes, want) {
			t.Fatalf("removals = %v, want only the sibling's own, once", removes)
		}
		if _, modeled := f.git.attemptWorktree(stray.Listed); !modeled {
			t.Fatalf("the stray row's own checkout was touched")
		}
	})

	t.Run("the fact is never set twice", func(t *testing.T) {
		f := newRetireFixture(t)
		f.addAttempt(1, fakeAttemptWorktree{}, nil)
		f.pass()
		first := f.tc.Store.WorktreesRetiredAt[f.fr.RunID]
		f.tc.Clock.Advance(time.Hour)
		f.release()
		if _, err := f.tc.Controller.AcquireForRetirement(context.Background(), f.fr.RunID.String(), "status-8"); !errors.Is(err, app.ErrRetirementNotEligible) {
			t.Fatalf("a retired run was acquired: %v", err)
		}
		if got := f.tc.Store.WorktreesRetiredAt[f.fr.RunID]; !got.Equal(first) {
			t.Fatalf("fact moved from %v to %v", first, got)
		}
	})
}
