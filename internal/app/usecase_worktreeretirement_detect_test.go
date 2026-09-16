package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

const (
	detectHOP     = "/usr/local/bin/hop"
	detectRoot    = "/repo"
	detectMainRef = "refs/heads/main"
	detectPID     = 4242
)

// detectFixture is a completed feature run with one integrated task whose
// merge commit is (or is not) contained in main, and a retirement pass's
// lease over it.
type detectFixture struct {
	t      *testing.T
	tc     *testController
	git    *fakeGitRepo
	fr     featureRun
	handle app.RunHandle
	base   string
	merge  string
	task   identity.TaskID
	// claims counts simulated hop check-exec claims.
	claims int
	// spawns records every hop check-exec spawn.
	spawns []app.Command
}

func newDetectFixture(t *testing.T, merged bool) *detectFixture {
	t.Helper()
	tc := newTestController(defaultPolicy())
	fr := seedRetirableRun(t, tc, run.RunCompleted, detectMainRef)
	snapshot := tc.Store.Snapshots[fr.RunID]
	snapshot.EnvPolicy = app.EnvPolicy{Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude}
	tc.Store.Snapshots[fr.RunID] = snapshot

	git := newFakeGitRepo(tc.Controller.GitExecutable, detectHOP)
	git.setAttemptRepository(detectRoot, "/repo/.git")
	f := &detectFixture{t: t, tc: tc, git: git, fr: fr}
	f.base = git.newCommit("tree-base")
	src := git.newCommit("tree-src", f.base)
	f.merge = git.newCommit("tree-merge", f.base, src)
	if merged {
		git.setRef(detectMainRef, git.newCommit("tree-main", f.merge))
	} else {
		git.setRef(detectMainRef, git.newCommit("tree-main", f.base))
	}
	tc.Commands.RunHook = func(ctx context.Context, cmd app.Command) (app.CommandResult, bool, error) {
		if len(cmd.Argv) > 1 && cmd.Argv[1] == "check-exec" {
			f.spawns = append(f.spawns, cmd)
		}
		return git.Hook(ctx, cmd)
	}
	git.OnCheckExec = func(opID string, _ []string) {
		if err := tc.Store.ClaimCheckExec(context.Background(), identity.OperationID(opID), detectPID); err != nil {
			t.Errorf("simulated check-exec claim refused: %v", err)
			return
		}
		f.claims++
	}
	f.task = seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskIntegrated)
	f.seedIntegration(f.base, f.merge, tc.Clock.Now())
	f.acquire()
	return f
}

// seedIntegration records an integrated row for the fixture's task.
func (f *detectFixture) seedIntegration(premerge, merge string, created time.Time) {
	f.t.Helper()
	id, err := identity.ParseIntegrationID(f.tc.IDs.NewID())
	if err != nil {
		f.t.Fatal(err)
	}
	f.tc.Store.Integrations[id] = &entityRow[run.Integration]{value: run.Integration{
		ID: id, RunID: f.fr.RunID, TaskID: f.task, ResultID: identity.ResultID(f.tc.IDs.NewID()),
		SourceCommitOID: merge, PremergeHeadOID: premerge, MergeCommitOID: merge,
		State: run.IntegrationIntegrated, CreatedAt: created, UpdatedAt: created,
	}, revision: 1}
}

// acquire starts a new pass: the previous pass's lease (if any) is released
// and a fresh generation acquired.
func (f *detectFixture) acquire() {
	f.t.Helper()
	if f.handle.RunID() != "" {
		if err := f.tc.Controller.ReleaseRetirement(context.Background(), f.handle); err != nil {
			f.t.Fatalf("ReleaseRetirement() error = %v", err)
		}
	}
	handle, err := f.tc.Controller.AcquireForRetirement(context.Background(), f.fr.RunID.String(), "status-7")
	if err != nil {
		f.t.Fatalf("AcquireForRetirement() error = %v", err)
	}
	f.handle = handle
}

func (f *detectFixture) detect() app.DetectionForTest {
	f.t.Helper()
	d, err := app.DetectForRetirementForTest(context.Background(), f.tc.Controller, f.handle, detectHOP,
		[]string{"PATH=/usr/bin:/bin", "ANTHROPIC_API_KEY=secret-value"})
	if err != nil {
		f.t.Fatalf("detection error = %v", err)
	}
	return d
}

// checks returns the run's retirement.check operations, oldest first: by
// generation (each pass takes a new one), then creation time.
func (f *detectFixture) checks() []app.Operation {
	f.t.Helper()
	var out []app.Operation
	for _, op := range f.tc.Store.Operations { //nolint:gocritic // rangeValCopy: test reads of small operation values.
		if op.RunID == f.fr.RunID && op.Kind == app.OpRetirementCheck {
			out = append(out, op)
		}
	}
	slices.SortFunc(out, func(a, b app.Operation) int {
		if a.Generation != b.Generation {
			return int(a.Generation - b.Generation)
		}
		return a.CreatedAt.Compare(b.CreatedAt)
	})
	return out
}

func outcomeResult(t *testing.T, op *app.Operation) string {
	t.Helper()
	fields, ok := jsonFields(op.Outcome)
	if !ok {
		t.Fatalf("outcome %v is not an object", op.Outcome)
	}
	result, ok := fields["result"].(string)
	if !ok {
		return ""
	}
	return result
}

// TestDetectRetirementMerge covers the detection step: one claimed ancestry
// check over two frozen object ids with its exact intent, the settled
// answers that stop repeated passes from journaling anything, every
// pre-check that ends detection without a check, and each recovery row of
// the retirement.check decision table.
func TestDetectRetirementMerge(t *testing.T) {
	t.Run("merged: one claimed check with the frozen intent, never repeated", func(t *testing.T) {
		f := newDetectFixture(t, true)
		d := f.detect()
		if d.State != "merged" || d.Head != f.merge {
			t.Fatalf("detection = %+v, want merged at %s", d, f.merge)
		}
		ops := f.checks()
		if len(ops) != 1 || ops[0].State != app.OperationSucceeded || outcomeResult(t, &ops[0]) != "merged" || f.claims != 1 {
			t.Fatalf("checks = %+v (claims %d), want one succeeded merged check behind one claim", ops, f.claims)
		}
		intent, _ := jsonFields(ops[0].Intent)
		wantArgv := []any{f.tc.Controller.GitExecutable, "-C", detectRoot, "merge-base", "--is-ancestor", f.merge, d.Target}
		if !equalJSONList(intent["argv"], wantArgv) || intent["cwd"] != detectRoot || intent["head_oid"] != f.merge || intent["target_ref"] != detectMainRef || intent["target_oid"] != d.Target {
			t.Fatalf("intent = %v, want the frozen argv %v over the head and the target tip", intent, wantArgv)
		}
		spawnArgv := append([]any{detectHOP, "check-exec", "--op", ops[0].ID.String(), "--"}, wantArgv...)
		if !equalJSONList(intent["spawn_argv"], spawnArgv) {
			t.Fatalf("spawn argv = %v, want %v", intent["spawn_argv"], spawnArgv)
		}
		if len(f.spawns) != 1 {
			t.Fatalf("spawns = %d, want 1", len(f.spawns))
		}
		spawn := f.spawns[0]
		env := strings.Join(spawn.Env, "\n")
		for _, want := range []string{"HOP_STATE_DIR=/state", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0", "PATH=/usr/bin:/bin"} {
			if !slices.Contains(spawn.Env, want) {
				t.Errorf("spawn env lacks %q:\n%s", want, env)
			}
		}
		if strings.Contains(env, "ANTHROPIC_API_KEY") || spawn.Dir != detectRoot {
			t.Errorf("spawn dir %q env:\n%s\nwant the root and a sanitized environment", spawn.Dir, env)
		}

		f.acquire()
		again := f.detect()
		if again.State != "merged" || len(f.checks()) != 1 || len(f.spawns) != 1 {
			t.Fatalf("second pass = %+v with %d checks and %d spawns, want merged from the settled check and nothing new", again, len(f.checks()), len(f.spawns))
		}
	})

	t.Run("not merged: settled once, silent while the tip is unchanged, re-checked when it moves", func(t *testing.T) {
		f := newDetectFixture(t, false)
		if d := f.detect(); d.State != "not-merged" {
			t.Fatalf("detection = %+v, want not-merged", d)
		}
		ops := f.checks()
		if len(ops) != 1 || ops[0].State != app.OperationSucceeded || outcomeResult(t, &ops[0]) != "not-merged" {
			t.Fatalf("checks = %+v, want one settled not-merged check", ops)
		}
		f.acquire()
		if d := f.detect(); d.State != "not-merged" || len(f.checks()) != 1 || len(f.spawns) != 1 {
			t.Fatalf("unchanged tip: %+v with %d checks, want not-merged and nothing journaled", d, len(f.checks()))
		}
		f.git.setRef(detectMainRef, f.git.newCommit("tree-main-2", f.git.ref(detectMainRef)))
		f.acquire()
		if d := f.detect(); d.State != "not-merged" || len(f.checks()) != 2 {
			t.Fatalf("moved tip: %+v with %d checks, want a second not-merged check", d, len(f.checks()))
		}
		f.git.setRef(detectMainRef, f.git.newCommit("tree-main-merged", f.git.ref(detectMainRef), f.merge))
		f.acquire()
		if d := f.detect(); d.State != "merged" || len(f.checks()) != 3 {
			t.Fatalf("merged tip: %+v with %d checks, want merged on a third check", d, len(f.checks()))
		}
	})

	preChecks := []struct {
		name  string
		setup func(f *detectFixture)
		want  string
	}{
		{"nothing integrated", func(f *detectFixture) { clear(f.tc.Store.Integrations) }, "nothing-integrated"},
		{"only no-op integrations", func(f *detectFixture) {
			clear(f.tc.Store.Integrations)
			f.seedIntegration(f.base, f.base, f.tc.Clock.Now())
		}, "nothing-integrated"},
		{"two latest integrated heads disagree", func(f *detectFixture) {
			f.seedIntegration(f.base, f.git.newCommit("tree-other", f.base), f.tc.Clock.Now())
		}, "failed"},
		{"the root no longer holds the head (moved or replaced repository)", func(f *detectFixture) {
			clear(f.tc.Store.Integrations)
			f.seedIntegration(f.base, "commit-9999", f.tc.Clock.Now())
		}, "history-missing"},
		{"the target branch no longer exists", func(f *detectFixture) { f.git.deleteRef(detectMainRef) }, "not-merged"},
	}
	for _, tc := range preChecks {
		t.Run("no check journaled: "+tc.name, func(t *testing.T) {
			f := newDetectFixture(t, true)
			tc.setup(f)
			if d := f.detect(); d.State != tc.want {
				t.Fatalf("detection = %+v, want %s", d, tc.want)
			}
			if len(f.checks()) != 0 || len(f.spawns) != 0 {
				t.Fatalf("%d checks journaled and %d spawned, want none", len(f.checks()), len(f.spawns))
			}
		})
	}

	t.Run("a failed check settles failed and is not repeated for the same head and tip", func(t *testing.T) {
		f := newDetectFixture(t, true)
		hook := f.tc.Commands.RunHook
		f.tc.Commands.RunHook = func(ctx context.Context, cmd app.Command) (app.CommandResult, bool, error) {
			if len(cmd.Argv) > 1 && cmd.Argv[1] == "check-exec" {
				f.spawns = append(f.spawns, cmd)
				return app.CommandResult{ExitCode: 128, Stderr: []byte("fatal: Not a valid commit name\n")}, true, nil
			}
			return hook(ctx, cmd)
		}
		if d := f.detect(); d.State != "failed" {
			t.Fatalf("detection = %+v, want failed", d)
		}
		ops := f.checks()
		if len(ops) != 1 || ops[0].State != app.OperationFailed || outcomeResult(t, &ops[0]) != "failed" {
			t.Fatalf("checks = %+v, want one failed check", ops)
		}
		f.acquire()
		if d := f.detect(); d.State != "failed" || len(f.checks()) != 1 || len(f.spawns) != 1 {
			t.Fatalf("second pass = %+v, %d checks, %d spawns; want the settled failure, nothing new", d, len(f.checks()), len(f.spawns))
		}
	})

	t.Run("an unobserved spawn is recovered by the next pass as never executed, then re-checked", func(t *testing.T) {
		f := newDetectFixture(t, true)
		hook := f.tc.Commands.RunHook
		f.tc.Commands.RunHook = func(ctx context.Context, cmd app.Command) (app.CommandResult, bool, error) {
			if len(cmd.Argv) > 1 && cmd.Argv[1] == "check-exec" {
				return app.CommandResult{}, true, errors.New("fork/exec: resource temporarily unavailable")
			}
			return hook(ctx, cmd)
		}
		if d := f.detect(); d.State != "interrupted" {
			t.Fatalf("detection = %+v, want interrupted", d)
		}
		if ops := f.checks(); len(ops) != 1 || ops[0].State != app.OperationReconciling {
			t.Fatalf("checks = %+v, want one reconciling check", ops)
		}
		f.tc.Commands.RunHook = hook
		f.acquire()
		if d := f.detect(); d.State != "merged" {
			t.Fatalf("next pass = %+v, want merged on a fresh check", d)
		}
		ops := f.checks()
		if len(ops) != 2 || ops[0].State != app.OperationFailed || outcomeResult(t, &ops[0]) != "never-executed" || outcomeResult(t, &ops[1]) != "merged" {
			t.Fatalf("checks = %+v, want the unobserved one settled never-executed and a fresh merged one", ops)
		}
	})

	recovery := []struct {
		name        string
		group       []app.GroupProcess
		listErr     error
		wantState   string
		wantOpState app.OperationState
		wantResult  string
		wantSignal  bool
	}{
		{name: "claimed group already gone: settled unknown, re-checked", wantState: "merged", wantOpState: app.OperationFailed, wantResult: "unknown"},
		{name: "claimed group still running its argv: signaled, pass blocked", group: []app.GroupProcess{{PID: detectPID}}, wantState: "blocked", wantOpState: app.OperationReconciling, wantSignal: true},
		{name: "claimed group running something else: never signaled, blocked", group: []app.GroupProcess{{PID: detectPID, Argv: []string{"/usr/bin/vim"}}}, wantState: "blocked", wantOpState: app.OperationReconciling},
		{name: "claimed group cannot be listed: blocked", listErr: errors.New("ps failed"), wantState: "blocked", wantOpState: app.OperationReconciling},
	}
	for _, tc := range recovery {
		t.Run("recovery with a claim: "+tc.name, func(t *testing.T) {
			f := newDetectFixture(t, true)
			hook := f.tc.Commands.RunHook
			var inner []string
			f.tc.Commands.RunHook = func(ctx context.Context, cmd app.Command) (app.CommandResult, bool, error) {
				if len(cmd.Argv) > 1 && cmd.Argv[1] == "check-exec" {
					for i, arg := range cmd.Argv {
						if arg == "--op" {
							f.git.OnCheckExec(cmd.Argv[i+1], nil)
						}
						if arg == "--" {
							inner = cmd.Argv[i+1:]
							break
						}
					}
					return app.CommandResult{}, true, errors.New("controller died while the check ran")
				}
				return hook(ctx, cmd)
			}
			f.detect()
			f.tc.Commands.RunHook = hook
			group := tc.group
			if len(group) == 1 && group[0].Argv == nil {
				group = []app.GroupProcess{{PID: detectPID, Argv: inner}}
			}
			if group != nil {
				f.tc.Groups.Processes[detectPID] = group
			}
			if tc.listErr != nil {
				f.tc.Groups.ListErr[detectPID] = tc.listErr
			}
			f.acquire()
			d := f.detect()
			if d.State != tc.wantState {
				t.Fatalf("next pass = %+v, want %s", d, tc.wantState)
			}
			first := f.checks()[0]
			if first.State != tc.wantOpState || (tc.wantResult != "" && outcomeResult(t, &first) != tc.wantResult) {
				t.Fatalf("recovered check = %+v, want %s %s", first, tc.wantOpState, tc.wantResult)
			}
			if signaled := slices.Contains(f.tc.Groups.Signaled, detectPID); signaled != tc.wantSignal {
				t.Fatalf("group signaled = %v, want %v", signaled, tc.wantSignal)
			}
			if tc.wantState == "blocked" && len(f.checks()) != 1 {
				t.Fatalf("a blocked pass journaled a new check")
			}
		})
	}

	for _, state := range []run.RunState{run.RunStopped, run.RunFailed} {
		t.Run("a "+string(state)+" run carrying its stop request is checked once, never re-journaled", func(t *testing.T) {
			f := newDetectFixture(t, true)
			row := f.tc.Store.Runs[f.fr.RunID]
			row.value.State, row.value.StopRequested = state, true
			f.acquire()
			if d := f.detect(); d.State != "merged" {
				t.Fatalf("detection = %+v, want merged: a terminal run's stop request never blocks retirement", d)
			}
			f.acquire()
			if d := f.detect(); d.State != "merged" || len(f.checks()) != 1 || len(f.spawns) != 1 {
				t.Fatalf("second pass = %+v with %d checks and %d spawns, want the settled merged check and nothing new", d, len(f.checks()), len(f.spawns))
			}
		})
	}

	dispatchBarriers := []struct {
		name  string
		apply func(f *detectFixture)
	}{
		{"running", func(f *detectFixture) { f.tc.Store.Runs[f.fr.RunID].value.State = run.RunRunning }},
		{"stopping", func(f *detectFixture) { f.tc.Store.Runs[f.fr.RunID].value.State = run.RunStopping }},
		{"resuming", func(f *detectFixture) { f.tc.Store.Runs[f.fr.RunID].value.State = run.RunResuming }},
		{"completing", func(f *detectFixture) { f.tc.Store.Runs[f.fr.RunID].value.State = run.RunCompleting }},
		{"already retired", func(f *detectFixture) {
			f.tc.Store.WorktreesRetiredAt[f.fr.RunID] = time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
		}},
	}
	for _, tc := range dispatchBarriers {
		t.Run("a stale eligibility read is refused at dispatch: "+tc.name, func(t *testing.T) {
			f := newDetectFixture(t, true)
			f.tc.Store.HeartbeatHook = func() {
				f.tc.Store.mu.Lock()
				defer f.tc.Store.mu.Unlock()
				tc.apply(f)
			}
			if d := f.detect(); d.State != "interrupted" {
				t.Fatalf("detection = %+v, want interrupted", d)
			}
			if ops := f.checks(); len(ops) != 1 || ops[0].State != app.OperationPending || len(f.spawns) != 0 || f.claims != 0 {
				t.Fatalf("checks = %+v, spawns %d, claims %d; want the intent pending and nothing spawned", ops, len(f.spawns), f.claims)
			}
		})
	}

	t.Run("repeated passes over settled runs append no journal rows", func(t *testing.T) {
		for _, setup := range []struct {
			name   string
			merged bool
			prep   func(f *detectFixture)
			want   string
		}{
			{name: "merged", merged: true, want: "merged"},
			{name: "not merged", want: "not-merged"},
			{name: "nothing integrated", merged: true, prep: func(f *detectFixture) { clear(f.tc.Store.Integrations) }, want: "nothing-integrated"},
			{name: "stopped with its stop request", merged: true, prep: func(f *detectFixture) {
				row := f.tc.Store.Runs[f.fr.RunID]
				row.value.State, row.value.StopRequested = run.RunStopped, true
			}, want: "merged"},
		} {
			f := newDetectFixture(t, setup.merged)
			if setup.prep != nil {
				setup.prep(f)
			}
			f.detect()
			ops, transitions, spawns := len(f.tc.Store.Operations), len(f.tc.Store.Transitions), len(f.spawns)
			for range 3 {
				f.acquire()
				if d := f.detect(); d.State != setup.want {
					t.Fatalf("%s: repeated pass = %+v, want %s", setup.name, d, setup.want)
				}
			}
			if len(f.tc.Store.Operations) != ops || len(f.tc.Store.Transitions) != transitions || len(f.spawns) != spawns {
				t.Fatalf("%s: repeated passes grew the journal: operations %d -> %d, transitions %d -> %d, spawns %d -> %d",
					setup.name, ops, len(f.tc.Store.Operations), transitions, len(f.tc.Store.Transitions), spawns, len(f.spawns))
			}
		}
	})

	t.Run("a pass that loses its lease before dispatch spawns nothing", func(t *testing.T) {
		f := newDetectFixture(t, true)
		f.tc.Store.HeartbeatHook = func() {
			f.tc.Store.mu.Lock()
			defer f.tc.Store.mu.Unlock()
			f.tc.Store.Leases[f.fr.RunID].held = false
		}
		if d := f.detect(); d.State != "interrupted" {
			t.Fatalf("detection = %+v, want interrupted", d)
		}
		if ops := f.checks(); len(ops) != 1 || ops[0].State != app.OperationPending || len(f.spawns) != 0 {
			t.Fatalf("checks = %+v, spawns %d; want the intent pending and nothing spawned", ops, len(f.spawns))
		}
	})
}

// jsonFields renders a persisted payload as a generic JSON object.
func jsonFields(v any) (map[string]any, bool) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, false
	}
	return fields, true
}

// equalJSONList reports whether a generic JSON array equals want.
func equalJSONList(v any, want []any) bool {
	list, ok := v.([]any)
	if !ok || len(list) != len(want) {
		return false
	}
	for i := range list {
		if fmt.Sprint(list[i]) != fmt.Sprint(want[i]) {
			return false
		}
	}
	return true
}
