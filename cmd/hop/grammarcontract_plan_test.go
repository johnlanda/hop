package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

// TestGrammarContractTaskCreateUsage covers every hop task create
// usage-error branch: --title and --file required, before any store is
// opened, so no fixture is needed.
func TestGrammarContractTaskCreateUsage(t *testing.T) {
	dir := freshStateDir(t)
	instructions := filepath.Join(dir, "instructions.md")
	if err := os.WriteFile(instructions, []byte("do the thing"), 0o600); err != nil {
		t.Fatalf("write instructions fixture: %v", err)
	}
	cases := []struct {
		name string
		args []string
	}{
		{"missing title", []string{"task", "create", "--file", instructions}},
		{"missing file", []string{"task", "create", "--title", "t"}},
		{"unexpected argument", []string{"task", "create", "--title", "t", "--file", instructions, "extra"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := execHop(t, map[string]string{"HOP_STATE_DIR": dir}, dir, tc.args...)
			if result.ExitCode != exitUsage {
				t.Fatalf("exit = %d, want %d (usage); stdout=%q stderr=%q", result.ExitCode, exitUsage, result.Stdout, result.Stderr)
			}
		})
	}
}

// TestGrammarContractTaskRetryUsage covers hop task retry's usage-error
// branch (--reason required, argument count); no fixture needed.
func TestGrammarContractTaskRetryUsage(t *testing.T) {
	dir := freshStateDir(t)
	cases := []struct {
		name string
		args []string
	}{
		{"missing task id", []string{"task", "retry", "--reason", "flaky"}},
		{"too many args", []string{"task", "retry", "--reason", "flaky", testUUID(1), testUUID(2)}},
		{"missing reason", []string{"task", "retry", testUUID(1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := execHop(t, map[string]string{"HOP_STATE_DIR": dir}, dir, tc.args...)
			if result.ExitCode != exitUsage {
				t.Fatalf("exit = %d, want %d (usage); stdout=%q stderr=%q", result.ExitCode, exitUsage, result.Stdout, result.Stderr)
			}
		})
	}
}

// TestGrammarContractTaskDispatchUsage covers hop task/hop plan's own
// dispatch usage (no or unknown subcommand); no fixture needed.
func TestGrammarContractTaskDispatchUsage(t *testing.T) {
	dir := freshStateDir(t)
	cases := [][]string{
		{"task"},
		{"task", "bogus"},
		{"plan"},
		{"plan", "bogus"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			result := execHop(t, map[string]string{"HOP_STATE_DIR": dir}, dir, args...)
			if result.ExitCode != exitUsage {
				t.Fatalf("exit = %d, want %d (usage); stdout=%q stderr=%q", result.ExitCode, exitUsage, result.Stdout, result.Stderr)
			}
		})
	}
}

// TestGrammarContractPlanCloseUsage covers hop plan close's usage-error
// branch (an unexpected argument); no fixture needed.
func TestGrammarContractPlanCloseUsage(t *testing.T) {
	dir := freshStateDir(t)
	result := execHop(t, map[string]string{"HOP_STATE_DIR": dir}, dir, "plan", "close", "extra")
	if result.ExitCode != exitUsage {
		t.Fatalf("exit = %d, want %d (usage); stdout=%q stderr=%q", result.ExitCode, exitUsage, result.Stdout, result.Stderr)
	}
}

// TestGrammarContractPlanStateDirRequired proves hop task create/retry
// and hop plan close fail (not usage) when HOP_STATE_DIR is missing or
// relative, without ever opening a store.
func TestGrammarContractPlanStateDirRequired(t *testing.T) {
	dir := freshStateDir(t)
	instructions := filepath.Join(dir, "instructions.md")
	if err := os.WriteFile(instructions, []byte("do the thing"), 0o600); err != nil {
		t.Fatalf("write instructions fixture: %v", err)
	}
	verbs := [][]string{
		{"task", "create", "--title", "t", "--file", instructions},
		{"task", "retry", "--reason", "flaky", testUUID(1)},
		{"plan", "close"},
	}
	for _, args := range verbs {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			result := execHop(t, map[string]string{}, dir, args...)
			if result.ExitCode != exitFailure {
				t.Fatalf("exit = %d, want %d (failure); stdout=%q stderr=%q", result.ExitCode, exitFailure, result.Stdout, result.Stderr)
			}
		})
	}
}

// TestGrammarContractPlanIdentityParseFailures proves hop task
// create/retry and hop plan close fail as a plain command error (not a
// grammar refusal line — the app layer's identity.ParseXxxID calls
// return a bare error before any store call) when HOP_RUN_ID,
// HOP_SESSION_ID or HOP_INCARNATION_ID is missing or malformed. No
// fixture needed.
func TestGrammarContractPlanIdentityParseFailures(t *testing.T) {
	dir := freshStateDir(t)
	instructions := filepath.Join(dir, "instructions.md")
	if err := os.WriteFile(instructions, []byte("do the thing"), 0o600); err != nil {
		t.Fatalf("write instructions fixture: %v", err)
	}
	base := map[string]string{
		"HOP_STATE_DIR":      dir,
		"HOP_RUN_ID":         testUUID(1),
		"HOP_SESSION_ID":     testUUID(2),
		"HOP_INCARNATION_ID": testUUID(3),
	}
	for _, key := range []string{"HOP_RUN_ID", "HOP_SESSION_ID", "HOP_INCARNATION_ID"} {
		for _, mutate := range []string{"missing", "malformed"} {
			t.Run(key+"_"+mutate, func(t *testing.T) {
				env := map[string]string{}
				for k, v := range base {
					env[k] = v
				}
				if mutate == "missing" {
					delete(env, key)
				} else {
					env[key] = malformedUUID
				}
				result := execHop(t, env, dir, "task", "create", "--title", "t", "--file", instructions)
				if result.ExitCode != exitFailure {
					t.Errorf("exit = %d, want %d (failure); stdout=%q stderr=%q", result.ExitCode, exitFailure, result.Stdout, result.Stderr)
				}
				if result.Stdout != "" {
					t.Errorf("wrote to stdout: %q", result.Stdout)
				}
			})
		}
	}
}

// TestGrammarContractTaskCreateAccepted drives hop task create's
// accepted and duplicate (request-id retry) outcomes from the manager,
// and the not-manager refusal from a delegated child session.
func TestGrammarContractTaskCreateAccepted(t *testing.T) {
	f := newFeatureManager(t, 6000, defaultMessageWait)
	dir := f.StateRoot
	instructions := filepath.Join(dir, "instructions.md")
	if err := os.WriteFile(instructions, []byte("implement the fixture change"), 0o600); err != nil {
		t.Fatalf("write instructions fixture: %v", err)
	}

	create := execHop(t, f.env(nil), dir, "task", "create", "--title", "fixture task", "--file", instructions, "--request-id", "req-task-1")
	if create.ExitCode != exitOK {
		t.Fatalf("task create: exit=%d stdout=%q stderr=%q", create.ExitCode, create.Stdout, create.Stderr)
	}
	first := create.FirstStdoutLine()
	if !strings.HasSuffix(first, " created") || !strings.HasPrefix(first, "task ") {
		t.Fatalf("task create first line = %q, want \"task <id> t<seq> created\"", first)
	}

	dup := execHop(t, f.env(nil), dir, "task", "create", "--title", "fixture task", "--file", instructions, "--request-id", "req-task-1")
	if got := dup.FirstStdoutLine(); !strings.HasPrefix(got, "duplicate ") {
		t.Errorf("duplicate task create first line = %q, want a \"duplicate \" prefix", got)
	}

	worker := f.addImplementTask(t, 6100, "a second, already-bound task")
	notManager := execHop(t, worker.env(f, nil), dir, "task", "create", "--title", "should refuse", "--file", instructions)
	want := app.GrammarRefusalLine(app.GrammarReasonNotManager)
	if got := notManager.FirstStdoutLine(); got != want {
		t.Errorf("non-manager task create first line = %q, want %q; stdout=%q stderr=%q", got, want, notManager.Stdout, notManager.Stderr)
	}
	if notManager.ExitCode != exitFailure {
		t.Errorf("exit = %d, want %d", notManager.ExitCode, exitFailure)
	}
}

// TestGrammarContractTaskRetryNotTerminal proves hop task retry's
// `refused: retry-not-terminal` first line against a task whose only
// attempt is still running (not yet in a terminal state).
func TestGrammarContractTaskRetryNotTerminal(t *testing.T) {
	f := newFeatureManager(t, 6200, defaultMessageWait)
	worker := f.addImplementTask(t, 6300, "a running implement task")

	result := execHop(t, f.env(nil), f.StateRoot, "task", "retry", "--reason", "flaky", worker.TaskID)
	want := app.GrammarRefusalLine(app.GrammarReasonRetryNotTerminal)
	if got := result.FirstStdoutLine(); got != want {
		t.Errorf("first line = %q, want %q; stdout=%q stderr=%q", got, want, result.Stdout, result.Stderr)
	}
	if result.ExitCode != exitFailure {
		t.Errorf("exit = %d, want %d", result.ExitCode, exitFailure)
	}
}

// TestGrammarContractTaskRetryAccepted drives hop task retry's accepted
// and duplicate (request-id retry) first lines against a needs-rework
// task behind a terminal first attempt: both name the task by its t<seq>
// label and the reserved attempt 2 through the shared grammar renderers,
// never by the raw task id argument.
func TestGrammarContractTaskRetryAccepted(t *testing.T) {
	const taskSeq = 6700
	f := newFeatureManager(t, 6650, defaultMessageWait)
	taskID := f.addReworkTask(t, taskSeq, "a task to retry")

	for _, tc := range []struct {
		name string
		want string
	}{
		{name: "accepted", want: app.GrammarRetryAcceptedLine(taskSeq, 2)},
		{name: "duplicate", want: app.GrammarRetryDuplicateLine(taskSeq, 2)},
	} {
		result := execHop(t, f.env(nil), f.StateRoot, "task", "retry", "--reason", "flaky check", "--request-id", "req-retry-1", taskID)
		if got := result.FirstStdoutLine(); got != tc.want {
			t.Errorf("%s first line = %q, want %q; stdout=%q stderr=%q", tc.name, got, tc.want, result.Stdout, result.Stderr)
		}
		if strings.Contains(result.Stdout, taskID) {
			t.Errorf("%s stdout names the raw task id: %q", tc.name, result.Stdout)
		}
		if result.ExitCode != exitOK || result.Stderr != "" {
			t.Errorf("%s exit = %d stderr = %q, want %d and nothing", tc.name, result.ExitCode, result.Stderr, exitOK)
		}
	}
}

// empty-plan (GrammarReasonEmptyPlan) is NOT covered here: every
// hopfixtures.Initialize fixture's InitializeRun bootstrap creates one
// task row, and migration 003 defaults an untyped kind to 'implement'
// (internal/adapters/sqlite/migrations/003_manager_workers_messages.sql),
// so a genuinely empty plan is not reachable through this seeding
// technique without an extra raw SQL deletion this suite has no other
// reason to need. internal/app's own TestPlanRefusalReasonsAlwaysSet
// covers it exhaustively against the real decision logic; this suite's
// job is narrower (confirming cmd/hop renders a token correctly), so the
// gap is accepted rather than worked around.

// TestGrammarContractPlanCloseAccepted drives hop plan close's accepted
// and duplicate outcomes against a run with one implement task.
func TestGrammarContractPlanCloseAccepted(t *testing.T) {
	f := newFeatureManager(t, 6500, defaultMessageWait)
	f.addImplementTask(t, 6600, "the plan's only task")

	closeResult := execHop(t, f.env(map[string]string{}), f.StateRoot, "plan", "close", "--request-id", "req-close-1")
	if want := app.GrammarPlanClosedLine; closeResult.FirstStdoutLine() != want {
		t.Errorf("first line = %q, want %q; stdout=%q stderr=%q", closeResult.FirstStdoutLine(), want, closeResult.Stdout, closeResult.Stderr)
	}
	if closeResult.ExitCode != exitOK {
		t.Errorf("exit = %d, want %d", closeResult.ExitCode, exitOK)
	}

	dup := execHop(t, f.env(nil), f.StateRoot, "plan", "close", "--request-id", "req-close-1")
	if want := app.GrammarPlanCloseDuplicateLine; dup.FirstStdoutLine() != want {
		t.Errorf("duplicate first line = %q, want %q; stdout=%q stderr=%q", dup.FirstStdoutLine(), want, dup.Stdout, dup.Stderr)
	}
}
