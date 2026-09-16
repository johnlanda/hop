package app_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// interruptRemoval makes the next removal act die with the controller:
// when claim holds, hop check-exec claims the operation first; effect, if
// set, then runs against the model (the part of git's removal that
// happened); the controller never observes an exit status.
func (f *retireFixture) interruptRemoval(claim bool, effect func(inner []string)) {
	f.onRemoval(func(cmd app.Command) (app.CommandResult, bool, error) {
		opID, inner := "", []string(nil)
		for i, arg := range cmd.Argv {
			if arg == "--op" {
				opID = cmd.Argv[i+1]
			}
			if arg == "--" {
				inner = cmd.Argv[i+1:]
				break
			}
		}
		if claim {
			f.git.OnCheckExec(opID, inner)
		}
		if effect != nil {
			effect(inner)
		}
		return app.CommandResult{}, true, errors.New("controller died while the removal ran")
	})
}

// recover runs the recovery step under the fixture's current lease.
func (f *retireFixture) recover() string {
	f.t.Helper()
	blocking, err := app.RecoverRetirementForTest(context.Background(), f.tc.Controller, f.handle, detectHOP, f.inspector())
	if err != nil {
		f.t.Fatalf("recovery error = %v", err)
	}
	return blocking
}

// TestRecoverWorktreeRetire covers the worktree.retire decision-table row
// for an unresolved removal an earlier pass left behind: no claim settles
// never-executed; a claimed group is retired first and the checkout
// observed again; every group or observation that cannot be resolved
// blocks the pass and the removal is never re-dispatched.
func TestRecoverWorktreeRetire(t *testing.T) {
	t.Run("no claim: never executed, the row re-observed and removed afresh", func(t *testing.T) {
		f := newRetireFixture(t)
		a := f.addAttempt(1, fakeAttemptWorktree{}, nil)
		f.interruptRemoval(false, nil)
		if rows := f.remove(); rows[0].Outcome != "unresolved" {
			t.Fatalf("first pass = %+v, want unresolved", rows)
		}
		f.tc.Commands.RunHook = f.baseHook
		f.acquire()
		if blocking := f.recover(); blocking != "" {
			t.Fatalf("recovery blocked: %s", blocking)
		}
		ops := f.retires()
		if len(ops) != 1 || ops[0].State != app.OperationFailed || outcomeResult(t, &ops[0]) != "never-executed" || f.rowState(a.WorktreeID) != run.WorktreeActive {
			t.Fatalf("operations = %+v, row %s; want never-executed with the row unchanged", ops, f.rowState(a.WorktreeID))
		}
		if rows := f.remove(); rows[0].Outcome != "removed" || len(f.retires()) != 2 {
			t.Fatalf("next removal = %+v with %d operations, want a fresh removal", rows, len(f.retires()))
		}
	})

	observed := []struct {
		name       string
		start      func(w *fakeAttemptWorktree)
		effect     func(f *retireFixture, a *retireAttempt) func(inner []string)
		wantState  app.OperationState
		wantResult string
		wantRow    run.WorktreeState
	}{
		{
			name: "the removal finished before the controller died: adopted as removed",
			effect: func(f *retireFixture, _ *retireAttempt) func([]string) {
				return func(inner []string) { f.git.runGitArgv(inner[1:]) }
			},
			wantState: app.OperationSucceeded, wantResult: "removed", wantRow: run.WorktreeRemoved,
		},
		{
			name:  "a prunable checkout's removal finished: adopted as absent",
			start: func(w *fakeAttemptWorktree) { w.Present = false },
			effect: func(f *retireFixture, _ *retireAttempt) func([]string) {
				return func(inner []string) { f.git.runGitArgv(inner[1:]) }
			},
			wantState: app.OperationSucceeded, wantResult: "absent", wantRow: run.WorktreeAbsent,
		},
		{
			name:      "nothing was removed: interrupted, the row active",
			wantState: app.OperationFailed, wantResult: "interrupted", wantRow: run.WorktreeActive,
		},
		{
			name: "the directory went but git still lists it: interrupted, the row active",
			effect: func(f *retireFixture, a *retireAttempt) func([]string) {
				return func([]string) {
					w, _ := f.git.attemptWorktree(a.Listed)
					w.Present = false
					f.git.addAttemptWorktree(a.Listed, w)
				}
			},
			wantState: app.OperationFailed, wantResult: "interrupted", wantRow: run.WorktreeActive,
		},
		{
			name: "git dropped its entry but the directory stays: released",
			effect: func(f *retireFixture, a *retireAttempt) func([]string) {
				return func([]string) {
					w, _ := f.git.attemptWorktree(a.Listed)
					w.Unlisted = true
					f.git.addAttemptWorktree(a.Listed, w)
				}
			},
			wantState: app.OperationFailed, wantResult: "released", wantRow: run.WorktreeReleased,
		},
	}
	for _, tc := range observed {
		t.Run("claim, group gone: "+tc.name, func(t *testing.T) {
			f := newRetireFixture(t)
			a := f.addAttempt(1, fakeAttemptWorktree{}, tc.start)
			var effect func([]string)
			if tc.effect != nil {
				effect = tc.effect(f, &a)
			}
			f.interruptRemoval(true, effect)
			f.remove()
			if ops := f.retires(); len(ops) != 1 || ops[0].State != app.OperationReconciling {
				t.Fatalf("after the lost act: %+v, want one reconciling operation", ops)
			}
			f.tc.Commands.RunHook = f.baseHook
			f.acquire()
			if blocking := f.recover(); blocking != "" {
				t.Fatalf("recovery blocked: %s", blocking)
			}
			ops := f.retires()
			if len(ops) != 1 || ops[0].State != tc.wantState || outcomeResult(t, &ops[0]) != tc.wantResult {
				t.Fatalf("recovered operation = %+v, want %s %s", ops, tc.wantState, tc.wantResult)
			}
			if got := f.rowState(a.WorktreeID); got != tc.wantRow {
				t.Fatalf("row state = %s, want %s", got, tc.wantRow)
			}
			if len(f.tc.Groups.Signaled) != 0 {
				t.Fatalf("an absent group was signaled: %v", f.tc.Groups.Signaled)
			}
		})
	}

	t.Run("an interrupted removal renders interrupted-removal while dirty, then is removed once clean", func(t *testing.T) {
		f := newRetireFixture(t)
		a := f.addAttempt(1, fakeAttemptWorktree{}, nil)
		f.interruptRemoval(true, func([]string) {
			w, _ := f.git.attemptWorktree(a.Listed)
			w.Modified = true
			f.git.addAttemptWorktree(a.Listed, w)
		})
		f.remove()
		f.tc.Commands.RunHook = f.baseHook
		f.acquire()
		if blocking := f.recover(); blocking != "" {
			t.Fatalf("recovery blocked: %s", blocking)
		}
		if rows := f.remove(); len(rows) != 1 || rows[0].Retained != app.RetainedInterruptedRemoval {
			t.Fatalf("rows = %+v, want interrupted-removal", rows)
		}
		f.git.addAttemptWorktree(a.Listed, fakeAttemptWorktree{Branch: "refs/heads/" + a.Branch, Head: a.Head, Present: true})
		f.acquire()
		if blocking := f.recover(); blocking != "" {
			t.Fatalf("recovery blocked: %s", blocking)
		}
		if rows := f.remove(); rows[0].Outcome != "removed" {
			t.Fatalf("rows = %+v, want removed once clean", rows)
		}
	})

	blocked := []struct {
		name       string
		group      func(inner []string) []app.GroupProcess
		listErr    error
		unobserved bool
		// cutListing makes the root's worktree listing end, at the capture
		// bound, on the record boundary just before the checkout.
		cutListing bool
		wantSignal bool
	}{
		{name: "the claimed group still runs the removal: signaled", group: func(inner []string) []app.GroupProcess {
			return []app.GroupProcess{{PID: detectPID, Argv: inner}}
		}, wantSignal: true},
		{name: "the claimed group runs something else: never signaled", group: func([]string) []app.GroupProcess {
			return []app.GroupProcess{{PID: detectPID, Argv: []string{"/usr/bin/vim"}}}
		}},
		{name: "the claimed group cannot be listed", listErr: errors.New("ps failed")},
		{name: "the group is gone but the checkout cannot be observed", unobserved: true},
		{name: "the group is gone but the worktree listing is cut by the capture bound", cutListing: true},
	}
	for _, tc := range blocked {
		t.Run("blocked, never re-dispatched: "+tc.name, func(t *testing.T) {
			f := newRetireFixture(t)
			a := f.addAttempt(1, fakeAttemptWorktree{}, nil)
			var inner []string
			f.interruptRemoval(true, func(argv []string) { inner = argv })
			f.remove()
			f.tc.Commands.RunHook = f.baseHook
			if tc.group != nil {
				f.tc.Groups.Processes[detectPID] = tc.group(inner)
			}
			if tc.listErr != nil {
				f.tc.Groups.ListErr[detectPID] = tc.listErr
			}
			if tc.unobserved {
				f.failPath = a.Recorded
			}
			if tc.cutListing {
				f.git.fillListingBefore(t, a.Listed, app.RetirementListingOutputBytesForTest)
			}
			spawns := len(f.spawns)
			for range 2 {
				f.acquire()
				if blocking := f.recover(); blocking == "" || strings.Contains(blocking, "/var/") {
					t.Fatalf("recovery blocking = %q, want a path-free blocking detail", blocking)
				}
			}
			ops := f.retires()
			if len(ops) != 1 || ops[0].State != app.OperationReconciling || f.rowState(a.WorktreeID) != run.WorktreeActive {
				t.Fatalf("operations = %+v, row %s; want the removal still reconciling", ops, f.rowState(a.WorktreeID))
			}
			if len(f.spawns) != spawns {
				t.Fatalf("a blocked removal was dispatched again")
			}
			if signaled := slices.Contains(f.tc.Groups.Signaled, detectPID); signaled != tc.wantSignal {
				t.Fatalf("group signaled = %v, want %v", signaled, tc.wantSignal)
			}
			if _, modeled := f.git.attemptWorktree(a.Listed); !modeled {
				t.Fatalf("a blocked checkout is gone")
			}
		})
	}

	t.Run("an unobservable checkout recovers once it can be observed", func(t *testing.T) {
		f := newRetireFixture(t)
		a := f.addAttempt(1, fakeAttemptWorktree{}, nil)
		f.interruptRemoval(true, nil)
		f.remove()
		f.tc.Commands.RunHook = f.baseHook
		f.failPath = a.Recorded
		f.acquire()
		if blocking := f.recover(); blocking == "" {
			t.Fatalf("an unobservable checkout did not block")
		}
		f.failPath = ""
		f.acquire()
		if blocking := f.recover(); blocking != "" {
			t.Fatalf("recovery blocked: %s", blocking)
		}
		if ops := f.retires(); ops[0].State != app.OperationFailed || outcomeResult(t, &ops[0]) != "interrupted" {
			t.Fatalf("operations = %+v, want interrupted", ops)
		}
	})

	t.Run("an operation of the pass's own generation is left alone", func(t *testing.T) {
		f := newRetireFixture(t)
		f.addAttempt(1, fakeAttemptWorktree{}, nil)
		f.interruptRemoval(true, nil)
		f.remove()
		if blocking := f.recover(); !strings.Contains(blocking, "this pass's generation") {
			t.Fatalf("blocking = %q, want the same-generation refusal", blocking)
		}
		if ops := f.retires(); ops[0].State != app.OperationReconciling {
			t.Fatalf("operations = %+v, want untouched", ops)
		}
	})

	malformed := []struct {
		name   string
		intent any
	}{
		{"an undecodable intent", "not an intent"},
		{"a settled decision's shape left pending", map[string]any{"decision": "released", "worktree_id": "x", "recorded_path": "/var/wt/x", "repository_root": detectRoot}},
		{"an intent naming another repository root", map[string]any{
			"decision": "remove", "worktree_id": "x", "recorded_path": "/var/wt/x", "repository_root": "/elsewhere",
			"argv": []any{"/usr/bin/git"}, "spawn_argv": []any{detectHOP},
		}},
	}
	for _, tc := range malformed {
		t.Run("fails closed: "+tc.name, func(t *testing.T) {
			f := newRetireFixture(t)
			opID := identity.OperationID(f.tc.IDs.NewID())
			f.tc.Store.Operations[opID] = app.Operation{
				ID: opID, RunID: f.fr.RunID, Generation: 1, Kind: app.OpWorktreeRetire, State: app.OperationPending,
				Intent: tc.intent, CreatedAt: f.tc.Clock.Now(), UpdatedAt: f.tc.Clock.Now(),
			}
			if blocking := f.recover(); !strings.Contains(blocking, "failing closed") {
				t.Fatalf("blocking = %q, want a fail-closed detail", blocking)
			}
			if op := f.tc.Store.Operations[opID]; op.State != app.OperationReconciling {
				t.Fatalf("operation = %+v, want reconciling", op)
			}
		})
	}
}
