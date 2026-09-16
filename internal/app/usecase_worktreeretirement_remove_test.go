package app_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// retireCommon is the fixture repository's git common directory.
const retireCommon = "/repo/.git"

// retireFixture is a merged, completed feature run (detectFixture) whose
// attempt worktree rows point at checkouts of the fake git's attempt model,
// each vouched for by its attempt's succeeded worktree.create operation in
// the shape the real store returns (a JSON round trip).
type retireFixture struct {
	*detectFixture
	// present are extra paths the path inspector reports as existing.
	present map[string]bool
	// failPath makes the path inspector fail for that path.
	failPath string
	// ignoredOnly are checkouts whose only unseen data is ignored files:
	// the one approved deletion git's own check does not see.
	ignoredOnly map[string]bool
	// baseHook is the detection fixture's command hook, which every
	// onRemoval hook wraps.
	baseHook func(ctx context.Context, cmd app.Command) (app.CommandResult, bool, error)
}

// retireAttempt is one seeded attempt worktree.
type retireAttempt struct {
	WorktreeID identity.WorktreeID
	AttemptID  identity.AttemptID
	Branch     string
	// Recorded is Herdr's non-canonical spelling; Listed is git's.
	Recorded string
	Listed   string
	Head     string
}

func newRetireFixture(t *testing.T) *retireFixture {
	t.Helper()
	f := &retireFixture{
		detectFixture: newDetectFixture(t, true),
		present:       map[string]bool{detectRoot: true, retireCommon: true},
		ignoredOnly:   map[string]bool{},
	}
	f.baseHook = f.tc.Commands.RunHook
	t.Cleanup(func() {
		f.git.requireNoForcedAttemptRemovals(t)
		_, _, hidden := f.git.attemptLog()
		for _, path := range hidden {
			if !f.ignoredOnly[path] {
				t.Errorf("a removal deleted data git's own check did not see at %s", path)
			}
		}
	})
	return f
}

// addAttempt seeds attempt n's worktree row, its succeeded worktree.create
// operation and its checkout. state's branch defaults to the attempt
// branch and its HEAD to a commit descending from the row's base; edit, if
// set, adjusts the model state last.
func (f *retireFixture) addAttempt(n int, state fakeAttemptWorktree, edit func(w *fakeAttemptWorktree)) retireAttempt { //nolint:gocritic // hugeParam: test seeding takes the state by value so a caller's literal is copied, never aliased.
	f.t.Helper()
	return f.addAttemptFor(f.fr.RunID, n, state, edit)
}

// addAttemptFor is addAttempt for any run of the fixture repository.
func (f *retireFixture) addAttemptFor(runID identity.RunID, n int, state fakeAttemptWorktree, edit func(w *fakeAttemptWorktree)) retireAttempt { //nolint:gocritic // hugeParam: test seeding takes the state by value so a caller's literal is copied, never aliased.
	f.t.Helper()
	a := retireAttempt{
		WorktreeID: identity.WorktreeID(f.tc.IDs.NewID()),
		AttemptID:  identity.AttemptID(f.tc.IDs.NewID()),
		Branch:     fmt.Sprintf("hop/r2/t%da1", n),
		Recorded:   fmt.Sprintf("/var/wt/hop-r2-t%da1", n),
		Listed:     fmt.Sprintf("/private/var/wt/hop-r2-t%da1", n),
		Head:       f.git.newCommit(fmt.Sprintf("tree-t%d", n), f.base),
	}
	repositoryID := f.tc.Store.Runs[runID].value.RepositoryID
	row, err := run.NewAttemptWorktree(a.WorktreeID, repositoryID, runID, a.AttemptID, f.base, a.Recorded, a.Branch)
	if err != nil {
		f.t.Fatal(err)
	}
	f.tc.Store.Worktrees[a.WorktreeID] = &entityRow[run.Worktree]{value: row, revision: 1}
	f.tc.Store.worktreeInsertSeq++
	f.tc.Store.worktreeInsertOrder[a.WorktreeID] = f.tc.Store.worktreeInsertSeq
	opID := identity.OperationID(f.tc.IDs.NewID())
	now := f.tc.Clock.Now()
	f.tc.Store.Operations[opID] = app.Operation{
		ID: opID, RunID: runID, Generation: 1, Kind: app.OpWorktreeCreate, State: app.OperationSucceeded,
		Intent: map[string]any{"repository_root": detectRoot, "branch": a.Branch, "base_ref": f.base, "attempt_id": a.AttemptID.String()},
		ActEvidence: map[string]any{
			"info":        map[string]any{"WorkspaceID": fmt.Sprintf("w%d", n), "Path": a.Recorded, "Branch": a.Branch},
			"base_commit": f.base,
		},
		CreatedAt: now, UpdatedAt: now,
	}
	state.Branch, state.Head, state.Present = "refs/heads/"+a.Branch, a.Head, true
	if edit != nil {
		edit(&state)
	}
	if state.Ignored && !state.AssumeUnchanged && !state.SkipWorktree && !state.Untracked && !state.NestedRepo {
		f.ignoredOnly[a.Listed] = true
	}
	f.git.addAttemptWorktree(a.Listed, state)
	return a
}

// inspector canonicalizes /var/ to /private/var/ and reports existence
// from the model and the fixture's extra paths.
func (f *retireFixture) inspector() app.PathInspector {
	return func(path string) (string, bool, error) {
		if path == f.failPath {
			return "", false, errors.New("lstat " + path + ": permission denied")
		}
		canonical := path
		if rest, ok := strings.CutPrefix(path, "/var/"); ok {
			canonical = "/private/var/" + rest
		}
		if w, modeled := f.git.attemptWorktree(canonical); modeled {
			return canonical, w.Present, nil
		}
		return canonical, f.present[canonical], nil
	}
}

// remove runs the removal step under the fixture's current lease.
func (f *retireFixture) remove() []app.RetirementRowForTest {
	f.t.Helper()
	rows, err := app.RemoveRunWorktreesForTest(context.Background(), f.tc.Controller, f.handle, detectHOP,
		[]string{"PATH=/usr/bin:/bin", "ANTHROPIC_API_KEY=secret-value"}, f.inspector())
	if err != nil {
		f.t.Fatalf("removal step error = %v", err)
	}
	return rows
}

// retires returns the run's worktree.retire operations, oldest first.
func (f *retireFixture) retires() []app.Operation {
	f.t.Helper()
	var out []app.Operation
	for _, op := range f.tc.Store.Operations { //nolint:gocritic // rangeValCopy: test reads of small operation values.
		if op.RunID == f.fr.RunID && op.Kind == app.OpWorktreeRetire {
			out = append(out, op)
		}
	}
	slices.SortFunc(out, func(a, b app.Operation) int {
		if a.Generation != b.Generation {
			return int(a.Generation - b.Generation)
		}
		return strings.Compare(a.ID.String(), b.ID.String())
	})
	return out
}

func (f *retireFixture) rowState(id identity.WorktreeID) run.WorktreeState {
	return f.tc.Store.Worktrees[id].value.State
}

// onRemoval installs a hook that runs when the removal's hop check-exec is
// spawned, before the inner git runs: it may change the model, and when it
// returns handled the inner git never runs. It replaces any earlier
// onRemoval hook.
func (f *retireFixture) onRemoval(hook func(cmd app.Command) (app.CommandResult, bool, error)) {
	previous := f.baseHook
	f.tc.Commands.RunHook = func(ctx context.Context, cmd app.Command) (app.CommandResult, bool, error) {
		if len(cmd.Argv) > 1 && cmd.Argv[1] == "check-exec" && slices.Contains(cmd.Argv, "remove") {
			if result, handled, err := hook(cmd); handled {
				f.spawns = append(f.spawns, cmd)
				return result, true, err
			}
		}
		return previous(ctx, cmd)
	}
}

func outcomeField(t *testing.T, op *app.Operation, key string) any {
	t.Helper()
	fields, ok := jsonFields(op.Outcome)
	if !ok {
		t.Fatalf("outcome %v is not an object", op.Outcome)
	}
	return fields[key]
}

// TestRemoveRunWorktrees covers the removal step: the claimed no-force
// removal with its exact frozen intent, every settled final decision with
// no act, every retained category with nothing journaled, each post-act
// observation, and a pass halted before dispatch.
func TestRemoveRunWorktrees(t *testing.T) {
	t.Run("clean checkouts are removed through one claimed no-force removal each", func(t *testing.T) {
		f := newRetireFixture(t)
		a := f.addAttempt(1, fakeAttemptWorktree{}, nil)
		b := f.addAttempt(2, fakeAttemptWorktree{}, nil)
		rows := f.remove()
		if len(rows) != 2 || rows[0].Outcome != "removed" || rows[1].Outcome != "removed" || rows[0].WorktreeID != a.WorktreeID.String() {
			t.Fatalf("rows = %+v, want both removed in insertion order", rows)
		}
		if rows[0].Path != a.Recorded || rows[0].Branch != a.Branch {
			t.Fatalf("row report = %+v, want the recorded path and branch", rows[0])
		}
		for _, w := range []retireAttempt{a, b} {
			if got := f.rowState(w.WorktreeID); got != run.WorktreeRemoved {
				t.Fatalf("row %s state = %s, want removed", w.Branch, got)
			}
			if _, modeled := f.git.attemptWorktree(w.Listed); modeled {
				t.Fatalf("checkout %s still exists", w.Listed)
			}
		}
		ops := f.retires()
		if len(ops) != 2 || f.claims != 2 || len(f.spawns) != 2 {
			t.Fatalf("worktree.retire ops %d, claims %d, spawns %d; want one claimed spawn per removal", len(ops), f.claims, len(f.spawns))
		}
		removes, _, _ := f.git.attemptLog()
		wantRemoves := []string{"status.showUntrackedFiles=all " + a.Listed, "status.showUntrackedFiles=all " + b.Listed}
		if !slices.Equal(removes, wantRemoves) {
			t.Fatalf("removal calls = %q, want %q", removes, wantRemoves)
		}

		op := ops[0]
		if op.State != app.OperationSucceeded || outcomeResult(t, &op) != "removed" {
			t.Fatalf("operation = %+v, want succeeded removed", op)
		}
		intent, _ := jsonFields(op.Intent)
		wantArgv := []any{f.tc.Controller.GitExecutable, "-C", detectRoot, "-c", "status.showUntrackedFiles=all", "worktree", "remove", a.Listed}
		if !equalJSONList(intent["argv"], wantArgv) || intent["cwd"] != detectRoot {
			t.Fatalf("intent argv/cwd = %v %v, want %v in the root", intent["argv"], intent["cwd"], wantArgv)
		}
		spawnArgv := append([]any{detectHOP, "check-exec", "--op", op.ID.String(), "--"}, wantArgv...)
		if !equalJSONList(intent["spawn_argv"], spawnArgv) {
			t.Fatalf("spawn argv = %v, want %v", intent["spawn_argv"], spawnArgv)
		}
		for key, want := range map[string]any{
			"decision": "remove", "worktree_id": a.WorktreeID.String(), "attempt_id": a.AttemptID.String(),
			"repository_root": detectRoot, "recorded_path": a.Recorded, "listed_path": a.Listed,
			"branch": a.Branch, "base_oid": f.base,
		} {
			if intent[key] != want {
				t.Errorf("intent[%s] = %v, want %v", key, intent[key], want)
			}
		}
		argvList, isList := intent["argv"].([]any)
		if !isList {
			t.Fatalf("intent argv = %v, want a list", intent["argv"])
		}
		for _, arg := range argvList {
			if arg == "-f" || arg == "--force" {
				t.Fatalf("removal argv carries a force option: %v", intent["argv"])
			}
		}
		spawn := f.spawns[len(f.spawns)-1]
		env := strings.Join(spawn.Env, "\n")
		for _, want := range []string{"HOP_STATE_DIR=/state", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0", "PATH=/usr/bin:/bin"} {
			if !slices.Contains(spawn.Env, want) {
				t.Errorf("removal spawn env lacks %q:\n%s", want, env)
			}
		}
		if strings.Contains(env, "ANTHROPIC_API_KEY") || spawn.Dir != detectRoot {
			t.Errorf("removal spawn dir %q env:\n%s\nwant the root and a sanitized environment", spawn.Dir, env)
		}

		f.acquire()
		if again := f.remove(); len(again) != 0 || len(f.retires()) != 2 {
			t.Fatalf("a second pass reported %+v and journaled %d operations, want nothing new", again, len(f.retires())-2)
		}
	})

	t.Run("ignored files are deleted with the checkout (approved: follow git)", func(t *testing.T) {
		f := newRetireFixture(t)
		a := f.addAttempt(1, fakeAttemptWorktree{Ignored: true}, nil)
		if rows := f.remove(); len(rows) != 1 || rows[0].Outcome != "removed" {
			t.Fatalf("rows = %+v, want removed", rows)
		}
		if _, _, hidden := f.git.attemptLog(); !slices.Equal(hidden, []string{a.Listed}) {
			t.Fatalf("ignored-file deletions = %v, want exactly the checkout", hidden)
		}
	})

	t.Run("a listed checkout whose directory is gone is removed by the same act and settles absent", func(t *testing.T) {
		f := newRetireFixture(t)
		a := f.addAttempt(1, fakeAttemptWorktree{}, func(w *fakeAttemptWorktree) { w.Present = false })
		rows := f.remove()
		if len(rows) != 1 || rows[0].Outcome != "absent" || f.rowState(a.WorktreeID) != run.WorktreeAbsent {
			t.Fatalf("rows = %+v, state %s; want absent", rows, f.rowState(a.WorktreeID))
		}
		ops := f.retires()
		intent, _ := jsonFields(ops[0].Intent)
		if len(ops) != 1 || ops[0].State != app.OperationSucceeded || outcomeResult(t, &ops[0]) != "absent" || intent["was_absent"] != true || intent["decision"] != "remove" {
			t.Fatalf("operation = %+v, want a succeeded removal act settled absent", ops)
		}
	})

	decisions := []struct {
		name       string
		state      fakeAttemptWorktree
		edit       func(f *retireFixture, a *retireAttempt, w *fakeAttemptWorktree)
		after      func(f *retireFixture, a *retireAttempt)
		wantRow    run.WorktreeState
		wantResult string
		wantReason app.WorktreeReleaseReason
	}{
		{
			name: "already gone and unlisted: absent",
			edit: func(_ *retireFixture, _ *retireAttempt, w *fakeAttemptWorktree) { w.Present, w.Unlisted = false, true },
			// The model keeps an unlisted, missing entry; git no longer knows it.
			wantRow: run.WorktreeAbsent, wantResult: "absent",
		},
		{
			name:    "a directory git does not list",
			edit:    func(_ *retireFixture, _ *retireAttempt, w *fakeAttemptWorktree) { w.Unlisted = true },
			wantRow: run.WorktreeReleased, wantResult: "released", wantReason: app.ReleasedNotRegistered,
		},
		{
			name: "checked out on another branch",
			edit: func(_ *retireFixture, _ *retireAttempt, w *fakeAttemptWorktree) {
				w.Branch = "refs/heads/feature/other"
			},
			wantRow: run.WorktreeReleased, wantResult: "released", wantReason: app.ReleasedOtherBranch,
		},
		{
			name:    "a detached HEAD",
			edit:    func(_ *retireFixture, _ *retireAttempt, w *fakeAttemptWorktree) { w.Branch = "" },
			wantRow: run.WorktreeReleased, wantResult: "released", wantReason: app.ReleasedDetached,
		},
		{
			name:    "another repository's checkout",
			edit:    func(_ *retireFixture, _ *retireAttempt, w *fakeAttemptWorktree) { w.CommonDir = "/elsewhere/.git" },
			after:   func(f *retireFixture, _ *retireAttempt) { f.present["/elsewhere/.git"] = true },
			wantRow: run.WorktreeReleased, wantResult: "released", wantReason: app.ReleasedOtherRepository,
		},
		{
			name: "HEAD no longer descends from the recorded base",
			edit: func(f *retireFixture, _ *retireAttempt, w *fakeAttemptWorktree) {
				w.Head = f.git.newCommit("tree-unrelated")
			},
			wantRow: run.WorktreeReleased, wantResult: "released", wantReason: app.ReleasedBaseNotAncestor,
		},
		{
			name: "the recorded path is the repository root",
			after: func(f *retireFixture, a *retireAttempt) {
				row := f.tc.Store.Worktrees[a.WorktreeID]
				row.value.Path = detectRoot
				for id, op := range f.tc.Store.Operations {
					if op.Kind == app.OpWorktreeCreate {
						op.ActEvidence = map[string]any{"info": map[string]any{"Path": detectRoot, "Branch": a.Branch}, "base_commit": f.base}
						f.tc.Store.Operations[id] = op
					}
				}
			},
			wantRow: run.WorktreeReleased, wantResult: "released", wantReason: app.ReleasedRepositoryRoot,
		},
		{
			name: "no succeeded worktree.create vouches for the row",
			after: func(f *retireFixture, _ *retireAttempt) {
				for id, op := range f.tc.Store.Operations {
					if op.Kind == app.OpWorktreeCreate {
						op.State = app.OperationReconciling
						f.tc.Store.Operations[id] = op
					}
				}
			},
			wantRow: run.WorktreeReleased, wantResult: "released", wantReason: app.ReleasedUnverified,
		},
		{
			name: "the worktree.create records another path",
			after: func(f *retireFixture, a *retireAttempt) {
				for id, op := range f.tc.Store.Operations {
					if op.Kind == app.OpWorktreeCreate {
						op.ActEvidence = map[string]any{"info": map[string]any{"Path": a.Recorded + "-other", "Branch": a.Branch}, "base_commit": f.base}
						f.tc.Store.Operations[id] = op
					}
				}
			},
			wantRow: run.WorktreeReleased, wantResult: "released", wantReason: app.ReleasedUnverified,
		},
		{
			name: "an unlinked row",
			after: func(f *retireFixture, a *retireAttempt) {
				f.tc.Store.Worktrees[a.WorktreeID].value.AttemptID = ""
			},
			wantRow: run.WorktreeReleased, wantResult: "released", wantReason: app.ReleasedUnverified,
		},
	}
	for _, tc := range decisions {
		t.Run("settled with no act: "+tc.name, func(t *testing.T) {
			f := newRetireFixture(t)
			var a retireAttempt
			a = f.addAttempt(1, tc.state, func(w *fakeAttemptWorktree) {
				if tc.edit != nil {
					tc.edit(f, &a, w)
				}
			})
			if tc.after != nil {
				tc.after(f, &a)
			}
			spawnsBefore := len(f.spawns)
			rows := f.remove()
			if len(rows) != 1 || rows[0].Outcome != string(tc.wantRow) || rows[0].Released != tc.wantReason || rows[0].LeftOnDisk {
				t.Fatalf("rows = %+v, want %s (%s)", rows, tc.wantRow, tc.wantReason)
			}
			if got := f.rowState(a.WorktreeID); got != tc.wantRow {
				t.Fatalf("row state = %s, want %s", got, tc.wantRow)
			}
			ops := f.retires()
			if len(ops) != 1 || ops[0].State != app.OperationSucceeded || outcomeResult(t, &ops[0]) != tc.wantResult {
				t.Fatalf("operations = %+v, want one succeeded %s decision", ops, tc.wantResult)
			}
			if reason := outcomeField(t, &ops[0], "released_reason"); tc.wantReason != "" && reason != string(tc.wantReason) {
				t.Fatalf("released_reason = %v, want %s", reason, tc.wantReason)
			}
			intent, _ := jsonFields(ops[0].Intent)
			if intent["decision"] != tc.wantResult || intent["argv"] != nil || intent["spawn_argv"] != nil {
				t.Fatalf("decision intent = %v, want no exec shape", intent)
			}
			if detail, isString := outcomeField(t, &ops[0], "detail").(string); !isString || strings.Contains(detail, "/var/") {
				t.Fatalf("decision detail %q echoes a path", detail)
			}
			if removes, _, _ := f.git.attemptLog(); len(removes) != 0 || len(f.spawns) != spawnsBefore {
				t.Fatalf("a settled decision acted: removals %v, spawns %d", removes, len(f.spawns)-spawnsBefore)
			}
			f.acquire()
			if again := f.remove(); len(again) != 0 || len(f.retires()) != 1 {
				t.Fatalf("the final row was examined again: %+v", again)
			}
		})
	}

	retained := []struct {
		name  string
		state fakeAttemptWorktree
		hide  bool
		fail  bool
		want  app.WorktreeRetainedCategory
	}{
		{name: "untracked file", state: fakeAttemptWorktree{Untracked: true}, want: app.RetainedUncommittedChanges},
		{name: "modified file", state: fakeAttemptWorktree{Modified: true}, want: app.RetainedUncommittedChanges},
		{name: "staged file", state: fakeAttemptWorktree{Staged: true}, want: app.RetainedUncommittedChanges},
		{name: "nested repository", state: fakeAttemptWorktree{NestedRepo: true}, want: app.RetainedUncommittedChanges},
		{name: "HAZARD untracked file hidden by status.showUntrackedFiles=no", state: fakeAttemptWorktree{Untracked: true}, hide: true, want: app.RetainedUncommittedChanges},
		{name: "HAZARD assume-unchanged", state: fakeAttemptWorktree{AssumeUnchanged: true}, want: app.RetainedHiddenChanges},
		{name: "HAZARD skip-worktree", state: fakeAttemptWorktree{SkipWorktree: true}, want: app.RetainedHiddenChanges},
		{name: "HAZARD ignored files beside an assume-unchanged change", state: fakeAttemptWorktree{Ignored: true, AssumeUnchanged: true}, want: app.RetainedHiddenChanges},
		{name: "locked", state: fakeAttemptWorktree{Locked: true, LockReason: "in use"}, want: app.RetainedLocked},
		{name: "unreadable path", fail: true, want: app.RetainedInspectionFailed},
	}
	for _, tc := range retained {
		t.Run("retained with nothing journaled: "+tc.name, func(t *testing.T) {
			f := newRetireFixture(t)
			f.git.setHideUntracked(tc.hide)
			a := f.addAttempt(1, tc.state, nil)
			if tc.fail {
				f.failPath = a.Recorded
			}
			spawnsBefore := len(f.spawns)
			for range 2 {
				rows := f.remove()
				if len(rows) != 1 || rows[0].Outcome != "retained" || rows[0].Retained != tc.want {
					t.Fatalf("rows = %+v, want retained %s", rows, tc.want)
				}
				f.acquire()
			}
			if len(f.retires()) != 0 || len(f.spawns) != spawnsBefore || f.rowState(a.WorktreeID) != run.WorktreeActive {
				t.Fatalf("a retained checkout journaled %d operations, spawned %d, state %s", len(f.retires()), len(f.spawns)-spawnsBefore, f.rowState(a.WorktreeID))
			}
			if removes, _, _ := f.git.attemptLog(); len(removes) != 0 {
				t.Fatalf("a retained checkout was dispatched for removal: %v", removes)
			}
			if _, modeled := f.git.attemptWorktree(a.Listed); !modeled {
				t.Fatalf("a retained checkout is gone")
			}
		})
	}

	t.Run("a dirty checkout is retained, then removed once cleaned", func(t *testing.T) {
		f := newRetireFixture(t)
		a := f.addAttempt(1, fakeAttemptWorktree{Modified: true}, nil)
		if rows := f.remove(); rows[0].Outcome != "retained" {
			t.Fatalf("rows = %+v, want retained", rows)
		}
		f.git.addAttemptWorktree(a.Listed, fakeAttemptWorktree{Branch: "refs/heads/" + a.Branch, Head: a.Head, Present: true})
		f.acquire()
		if rows := f.remove(); rows[0].Outcome != "removed" || f.rowState(a.WorktreeID) != run.WorktreeRemoved {
			t.Fatalf("rows = %+v, want removed once clean", rows)
		}
	})

	t.Run("interrupted-removal replaces uncommitted-changes only after an interrupted act", func(t *testing.T) {
		for result, want := range map[string]app.WorktreeRetainedCategory{
			"interrupted": app.RetainedInterruptedRemoval,
			"refused":     app.RetainedUncommittedChanges,
		} {
			f := newRetireFixture(t)
			a := f.addAttempt(1, fakeAttemptWorktree{Modified: true}, nil)
			for _, older := range []struct {
				result     string
				generation int64
			}{{"refused", 1}, {result, 2}} {
				opID := identity.OperationID(f.tc.IDs.NewID())
				f.tc.Store.Operations[opID] = app.Operation{
					ID: opID, RunID: f.fr.RunID, Generation: older.generation, Kind: app.OpWorktreeRetire, State: app.OperationFailed,
					Intent:    map[string]any{"decision": "remove", "worktree_id": a.WorktreeID.String()},
					Outcome:   map[string]any{"result": older.result, "exit_code": -1},
					CreatedAt: f.tc.Clock.Now(), UpdatedAt: f.tc.Clock.Now(),
				}
			}
			if rows := f.remove(); len(rows) != 1 || rows[0].Retained != want {
				t.Fatalf("latest settled result %s: rows = %+v, want %s", result, rows, want)
			}
		}
	})

	postAct := []struct {
		name         string
		hook         func(f *retireFixture, a *retireAttempt) func(app.Command) (app.CommandResult, bool, error)
		wantRow      string
		wantState    run.WorktreeState
		wantOpState  app.OperationState
		wantResult   string
		wantRetained app.WorktreeRetainedCategory
		wantReleased app.WorktreeReleaseReason
		wantLeft     bool
		wantExit     int
		wantEvidence bool
	}{
		{
			name: "the checkout became dirty after the pre-check: git refuses, retained with evidence",
			hook: func(f *retireFixture, a *retireAttempt) func(app.Command) (app.CommandResult, bool, error) {
				return func(app.Command) (app.CommandResult, bool, error) {
					w, _ := f.git.attemptWorktree(a.Listed)
					w.Untracked = true
					f.git.addAttemptWorktree(a.Listed, w)
					return app.CommandResult{}, false, nil
				}
			},
			wantRow: "retained", wantState: run.WorktreeActive, wantOpState: app.OperationFailed, wantResult: "refused",
			wantRetained: app.RetainedUncommittedChanges, wantExit: 128, wantEvidence: true,
		},
		{
			name: "HAZARD an untracked file appears after the pre-check where status.showUntrackedFiles=no: the override keeps git's refusal",
			hook: func(f *retireFixture, a *retireAttempt) func(app.Command) (app.CommandResult, bool, error) {
				return func(app.Command) (app.CommandResult, bool, error) {
					f.git.setHideUntracked(true)
					w, _ := f.git.attemptWorktree(a.Listed)
					w.Untracked = true
					f.git.addAttemptWorktree(a.Listed, w)
					return app.CommandResult{}, false, nil
				}
			},
			wantRow: "retained", wantState: run.WorktreeActive, wantOpState: app.OperationFailed, wantResult: "refused",
			wantRetained: app.RetainedUncommittedChanges, wantExit: 128, wantEvidence: true,
		},
		{
			name: "the checkout was locked after the pre-check: git refuses, retained locked",
			hook: func(f *retireFixture, a *retireAttempt) func(app.Command) (app.CommandResult, bool, error) {
				return func(app.Command) (app.CommandResult, bool, error) {
					w, _ := f.git.attemptWorktree(a.Listed)
					w.Locked = true
					f.git.addAttemptWorktree(a.Listed, w)
					return app.CommandResult{}, false, nil
				}
			},
			wantRow: "retained", wantState: run.WorktreeActive, wantOpState: app.OperationFailed, wantResult: "refused",
			wantRetained: app.RetainedLocked, wantExit: 128, wantEvidence: true,
		},
		{
			name: "a non-zero exit with nothing to show for it: remove-refused with the exit code",
			hook: func(*retireFixture, *retireAttempt) func(app.Command) (app.CommandResult, bool, error) {
				return func(app.Command) (app.CommandResult, bool, error) {
					return app.CommandResult{ExitCode: 1, Stderr: []byte("check-exec: refused\n")}, true, nil
				}
			},
			wantRow: "retained", wantState: run.WorktreeActive, wantOpState: app.OperationFailed, wantResult: "refused",
			wantRetained: app.RetainedRemoveRefused, wantExit: 1, wantEvidence: true,
		},
		{
			name: "exit 0 with the checkout still listed and present: handled like a refusal",
			hook: func(*retireFixture, *retireAttempt) func(app.Command) (app.CommandResult, bool, error) {
				return func(app.Command) (app.CommandResult, bool, error) { return app.CommandResult{}, true, nil }
			},
			wantRow: "retained", wantState: run.WorktreeActive, wantOpState: app.OperationFailed, wantResult: "refused",
			wantRetained: app.RetainedRemoveRefused, wantExit: 0, wantEvidence: true,
		},
		{
			name: "the directory went but git still lists it: incomplete, row active",
			hook: func(f *retireFixture, a *retireAttempt) func(app.Command) (app.CommandResult, bool, error) {
				return func(app.Command) (app.CommandResult, bool, error) {
					w, _ := f.git.attemptWorktree(a.Listed)
					w.Present = false
					f.git.addAttemptWorktree(a.Listed, w)
					return app.CommandResult{ExitCode: 128, Stderr: []byte("fatal: failed to delete\n")}, true, nil
				}
			},
			wantRow: "incomplete", wantState: run.WorktreeActive, wantOpState: app.OperationFailed, wantResult: "incomplete",
			wantExit: 128, wantEvidence: true,
		},
		{
			name: "git dropped its entry but the directory stays: released, left on disk",
			hook: func(f *retireFixture, a *retireAttempt) func(app.Command) (app.CommandResult, bool, error) {
				return func(app.Command) (app.CommandResult, bool, error) {
					w, _ := f.git.attemptWorktree(a.Listed)
					w.Unlisted = true
					f.git.addAttemptWorktree(a.Listed, w)
					return app.CommandResult{ExitCode: 128, Stderr: []byte("fatal: failed to delete\n")}, true, nil
				}
			},
			wantRow: "released", wantState: run.WorktreeReleased, wantOpState: app.OperationFailed, wantResult: "released",
			wantReleased: app.ReleasedNotRegistered, wantLeft: true, wantExit: 128, wantEvidence: true,
		},
		{
			name: "the spawn's exit status is lost: reconciling for the next pass",
			hook: func(*retireFixture, *retireAttempt) func(app.Command) (app.CommandResult, bool, error) {
				return func(app.Command) (app.CommandResult, bool, error) {
					return app.CommandResult{}, true, errors.New("fork/exec: resource temporarily unavailable")
				}
			},
			wantRow: "unresolved", wantState: run.WorktreeActive, wantOpState: app.OperationReconciling,
		},
		{
			name: "the checkout cannot be observed after the act: reconciling for the next pass",
			hook: func(f *retireFixture, a *retireAttempt) func(app.Command) (app.CommandResult, bool, error) {
				return func(app.Command) (app.CommandResult, bool, error) {
					f.failPath = a.Recorded
					return app.CommandResult{}, false, nil
				}
			},
			wantRow: "unresolved", wantState: run.WorktreeActive, wantOpState: app.OperationReconciling,
		},
	}
	for _, tc := range postAct {
		t.Run("after the act: "+tc.name, func(t *testing.T) {
			f := newRetireFixture(t)
			a := f.addAttempt(1, fakeAttemptWorktree{}, nil)
			f.onRemoval(tc.hook(f, &a))
			rows := f.remove()
			if len(rows) != 1 {
				t.Fatalf("rows = %+v, want one", rows)
			}
			row := rows[0]
			if row.Outcome != tc.wantRow || row.Retained != tc.wantRetained || row.Released != tc.wantReleased || row.LeftOnDisk != tc.wantLeft {
				t.Fatalf("row = %+v, want %s retained=%q released=%q left=%v", row, tc.wantRow, tc.wantRetained, tc.wantReleased, tc.wantLeft)
			}
			if got := f.rowState(a.WorktreeID); got != tc.wantState {
				t.Fatalf("row state = %s, want %s", got, tc.wantState)
			}
			ops := f.retires()
			if len(ops) != 1 || ops[0].State != tc.wantOpState {
				t.Fatalf("operations = %+v, want one %s", ops, tc.wantOpState)
			}
			if tc.wantResult != "" {
				if got := outcomeResult(t, &ops[0]); got != tc.wantResult {
					t.Fatalf("result = %s, want %s", got, tc.wantResult)
				}
				if got := outcomeField(t, &ops[0], "exit_code"); fmt.Sprint(got) != fmt.Sprint(tc.wantExit) || row.ExitCode != tc.wantExit {
					t.Fatalf("exit code = %v (row %d), want %d", got, row.ExitCode, tc.wantExit)
				}
			}
			if tc.wantEvidence {
				stderrPath := fmt.Sprintf("/state/runs/%s/retirement/%s/stderr", f.fr.RunID, ops[0].ID)
				if row.EvidencePath != stderrPath || outcomeField(t, &ops[0], "stderr_path") != stderrPath {
					t.Fatalf("evidence = %q / %v, want %s", row.EvidencePath, outcomeField(t, &ops[0], "stderr_path"), stderrPath)
				}
				if _, err := f.tc.Artifacts.ReadArtifact(context.Background(), stderrPath); err != nil {
					t.Fatalf("stderr was not retained: %v", err)
				}
				if _, err := f.tc.Artifacts.ReadArtifact(context.Background(), strings.TrimSuffix(stderrPath, "stderr")+"stdout"); err != nil {
					t.Fatalf("stdout was not retained: %v", err)
				}
			} else if row.EvidencePath != "" {
				t.Fatalf("evidence %q retained for %s", row.EvidencePath, tc.wantRow)
			}
		})
	}

	t.Run("an incomplete removal is completed by the next pass with the same no-force removal", func(t *testing.T) {
		f := newRetireFixture(t)
		a := f.addAttempt(1, fakeAttemptWorktree{}, nil)
		once := true
		f.onRemoval(func(app.Command) (app.CommandResult, bool, error) {
			if !once {
				return app.CommandResult{}, false, nil
			}
			once = false
			w, _ := f.git.attemptWorktree(a.Listed)
			w.Present = false
			f.git.addAttemptWorktree(a.Listed, w)
			return app.CommandResult{ExitCode: 128}, true, nil
		})
		if rows := f.remove(); rows[0].Outcome != "incomplete" {
			t.Fatalf("first pass = %+v, want incomplete", rows)
		}
		f.acquire()
		if rows := f.remove(); rows[0].Outcome != "absent" || f.rowState(a.WorktreeID) != run.WorktreeAbsent {
			t.Fatalf("second pass = %+v, want absent", rows)
		}
		removes, _, _ := f.git.attemptLog()
		if want := []string{"status.showUntrackedFiles=all " + a.Listed}; !slices.Equal(removes, want) {
			t.Fatalf("removal calls = %q, want the one completing no-force removal %q", removes, want)
		}
		ops := f.retires()
		if len(ops) != 2 || outcomeResult(t, &ops[0]) != "incomplete" || outcomeResult(t, &ops[1]) != "absent" {
			t.Fatalf("operations = %+v, want incomplete then absent", ops)
		}
	})

	t.Run("a pass that loses its lease before an act spawns nothing and stops", func(t *testing.T) {
		f := newRetireFixture(t)
		a := f.addAttempt(1, fakeAttemptWorktree{}, nil)
		b := f.addAttempt(2, fakeAttemptWorktree{}, nil)
		f.tc.Store.HeartbeatHook = func() {
			f.tc.Store.mu.Lock()
			defer f.tc.Store.mu.Unlock()
			f.tc.Store.Leases[f.fr.RunID].held = false
		}
		spawnsBefore := len(f.spawns)
		rows := f.remove()
		if len(rows) != 1 || rows[0].Outcome != "not-dispatched" || rows[0].WorktreeID != a.WorktreeID.String() {
			t.Fatalf("rows = %+v, want the first row not dispatched and the pass stopped", rows)
		}
		ops := f.retires()
		if len(ops) != 1 || ops[0].State != app.OperationPending || len(f.spawns) != spawnsBefore {
			t.Fatalf("operations = %+v, spawns %d; want one pending intent and nothing spawned", ops, len(f.spawns)-spawnsBefore)
		}
		if f.rowState(a.WorktreeID) != run.WorktreeActive || f.rowState(b.WorktreeID) != run.WorktreeActive {
			t.Fatalf("rows moved without an act")
		}
	})
}
