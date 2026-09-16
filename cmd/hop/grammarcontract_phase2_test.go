package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file covers the five Phase 2 verbs that never had real-binary
// grammar contract tests before this slice (hop status, stop, resume,
// launch, result submit) — in scope per the manager's original directive
// alongside this slice's own six verb families. Each verb's own SKIPPED
// shapes (whatever needs a live Herdr connection or an actual harness
// exec) are documented at each test's own doc comment, not re-derived
// here.

// TestGrammarContractStatusNoRuns proves hop status's "no runs" listing
// line against a repository with zero runs; no fixture needed.
func TestGrammarContractStatusNoRuns(t *testing.T) {
	dir := realDir(t)
	result := execHop(t, map[string]string{}, dir, "status", "-C", dir)
	if result.ExitCode != exitOK {
		t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", result.ExitCode, exitOK, result.Stdout, result.Stderr)
	}
	if want := "no runs (use -all to include finished runs)"; result.FirstStdoutLine() != want {
		t.Errorf("first line = %q, want %q", result.FirstStdoutLine(), want)
	}
}

// TestGrammarContractStatusUnknownRun proves hop status -run against a
// well-formed but unknown run id is a command failure with empty stdout;
// no fixture needed (an empty freshly-migrated store is enough).
func TestGrammarContractStatusUnknownRun(t *testing.T) {
	dir := freshStateDir(t)
	result := execHop(t, map[string]string{"HOP_STATE_DIR": dir}, dir, "status", "-run", testUUID(1))
	if result.ExitCode != exitFailure {
		t.Fatalf("exit = %d, want %d (failure); stdout=%q stderr=%q", result.ExitCode, exitFailure, result.Stdout, result.Stderr)
	}
	if result.Stdout != "" {
		t.Errorf("unknown run wrote to stdout: %q", result.Stdout)
	}
}

// TestGrammarContractStatusUnknownLabel proves hop status -run against
// an unknown r<seq> label is a usage error (resolveRunArg's own listing
// lookup fails); no fixture needed.
func TestGrammarContractStatusUnknownLabel(t *testing.T) {
	dir := realDir(t)
	result := execHop(t, map[string]string{}, dir, "status", "-C", dir, "-run", "r1")
	if result.ExitCode != exitUsage {
		t.Fatalf("exit = %d, want %d (usage); stdout=%q stderr=%q", result.ExitCode, exitUsage, result.Stdout, result.Stderr)
	}
}

// TestGrammarContractStatusFeatureRunDetail proves hop status -run
// renders "workflow: feature" for a feature-mode run, and a bare
// listing includes it (the workflow: cross-check the HANDOFF flagged as
// cheap once a featureManager fixture exists).
func TestGrammarContractStatusFeatureRunDetail(t *testing.T) {
	f := newFeatureManager(t, 8000, defaultMessageWait)

	stateDir := map[string]string{"HOP_STATE_DIR": f.StateRoot}
	detail := execHop(t, stateDir, f.RepositoryRoot, "status", "-C", f.RepositoryRoot, "-run", f.RunID)
	if detail.ExitCode != exitOK {
		t.Fatalf("status -run: exit=%d stdout=%q stderr=%q", detail.ExitCode, detail.Stdout, detail.Stderr)
	}
	if !strings.Contains(detail.Stdout, "workflow:      feature") {
		t.Errorf("status -run stdout does not contain the feature workflow line: %q", detail.Stdout)
	}

	listing := execHop(t, stateDir, f.RepositoryRoot, "status", "-C", f.RepositoryRoot)
	if listing.ExitCode != exitOK {
		t.Fatalf("status listing: exit=%d stdout=%q stderr=%q", listing.ExitCode, listing.Stdout, listing.Stderr)
	}
	if !strings.Contains(listing.Stdout, f.RunID) {
		t.Errorf("status listing does not mention the seeded run: %q", listing.Stdout)
	}
}

// TestGrammarContractStopUsage covers hop stop's argument-count usage
// error; no fixture needed.
func TestGrammarContractStopUsage(t *testing.T) {
	dir := realDir(t)
	cases := [][]string{
		{"stop"},
		{"stop", testUUID(1), testUUID(2)},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			result := execHop(t, map[string]string{}, dir, args...)
			if result.ExitCode != exitUsage {
				t.Fatalf("exit = %d, want %d (usage); stdout=%q stderr=%q", result.ExitCode, exitUsage, result.Stdout, result.Stderr)
			}
		})
	}
}

// TestGrammarContractStopUnknownRun proves hop stop against a
// well-formed but unknown run id fails as a command error, reached
// entirely before RequestStop or Resume/ResumeFeature (so never touches
// Herdr even though hop stop wires the Runtime port) — the canary here
// proves that boundary. No fixture needed.
//
// Every OTHER hop stop outcome (RequestStop succeeding, then driving or
// observing a stop through Resume/ResumeFeature and DriveStop/
// DriveFeatureStop) is SKIPPED in this suite: those calls always touch
// Herdr (confirmed by reading stopcmd.go — Resume/ResumeFeature run
// unconditionally once the run is confirmed to exist), so they need a
// live pane this suite must never provide.
func TestGrammarContractStopUnknownRun(t *testing.T) {
	dir := freshStateDir(t)
	result := execHop(t, map[string]string{}, dir, "stop", "-C", dir, testUUID(1))
	if result.ExitCode != exitFailure {
		t.Fatalf("exit = %d, want %d (failure); stdout=%q stderr=%q", result.ExitCode, exitFailure, result.Stdout, result.Stderr)
	}
}

// TestGrammarContractStopUnknownLabel proves hop stop against an unknown
// r<seq> label is a usage error; no fixture needed.
func TestGrammarContractStopUnknownLabel(t *testing.T) {
	dir := realDir(t)
	result := execHop(t, map[string]string{}, dir, "stop", "-C", dir, "r1")
	if result.ExitCode != exitUsage {
		t.Fatalf("exit = %d, want %d (usage); stdout=%q stderr=%q", result.ExitCode, exitUsage, result.Stdout, result.Stderr)
	}
}

// TestGrammarContractResumeUsage covers hop resume's argument-count
// usage error; no fixture needed.
func TestGrammarContractResumeUsage(t *testing.T) {
	dir := realDir(t)
	cases := [][]string{
		{"resume"},
		{"resume", testUUID(1), testUUID(2)},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			result := execHop(t, map[string]string{}, dir, args...)
			if result.ExitCode != exitUsage {
				t.Fatalf("exit = %d, want %d (usage); stdout=%q stderr=%q", result.ExitCode, exitUsage, result.Stdout, result.Stderr)
			}
		})
	}
}

// TestGrammarContractResumeUnknownRun proves hop resume against a
// well-formed but unknown run id fails as a command error, reached
// before Resume/ResumeFeature is ever called; no fixture needed.
//
// Every Resume/ResumeFeature outcome (warm reattach, cold relaunch,
// nothing-to-do, reconciling, ...) is SKIPPED in this suite: reaching
// them always touches Herdr. Only the two --confirm-absent usage
// refusals below are reachable without it.
func TestGrammarContractResumeUnknownRun(t *testing.T) {
	dir := freshStateDir(t)
	result := execHop(t, map[string]string{}, dir, "resume", "-C", dir, testUUID(1))
	if result.ExitCode != exitFailure {
		t.Fatalf("exit = %d, want %d (failure); stdout=%q stderr=%q", result.ExitCode, exitFailure, result.Stdout, result.Stderr)
	}
}

// TestGrammarContractResumeConfirmAbsentUsage proves hop resume's two
// --confirm-absent usage refusals, each reached AFTER the run's mode is
// loaded via Status but BEFORE Resume/ResumeFeature is ever called (so
// neither touches Herdr): a feature-mode run's bare/empty
// --confirm-absent form, and a solo run's non-boolean --confirm-absent
// value.
func TestGrammarContractResumeConfirmAbsentUsage(t *testing.T) {
	t.Run("feature mode requires a session id", func(t *testing.T) {
		f := newFeatureManager(t, 8200, defaultMessageWait)
		env := map[string]string{"HOP_STATE_DIR": f.StateRoot}
		result := execHop(t, env, f.RepositoryRoot, "resume", "-C", f.RepositoryRoot, "--confirm-absent", f.RunID)
		if result.ExitCode != exitUsage {
			t.Fatalf("exit = %d, want %d (usage); stdout=%q stderr=%q", result.ExitCode, exitUsage, result.Stdout, result.Stderr)
		}
	})
	t.Run("solo takes no confirm-absent value", func(t *testing.T) {
		f := newSoloReserved(t, 8300)
		env := map[string]string{"HOP_STATE_DIR": f.StateRoot}
		result := execHop(t, env, f.RepositoryRoot, "resume", "-C", f.RepositoryRoot, "--confirm-absent=not-a-bool", f.RunID)
		if result.ExitCode != exitUsage {
			t.Fatalf("exit = %d, want %d (usage); stdout=%q stderr=%q", result.ExitCode, exitUsage, result.Stdout, result.Stderr)
		}
	})
}

// canaryHarnessName is the executable name hop launch's lookup searches
// for every launch fixture in this file: hopfixtures freezes the claude
// harness, and no fixture policy configures a harness executable path
// beyond that name.
const canaryHarnessName = "claude"

// launchExecCanary is the direct "exec never reached" barrier for hop
// launch's contract tests: a directory placed FIRST on the launch's PATH
// holding one executable under the configured harness name — the name the
// launch's executable lookup resolves — whose only behavior is creating a
// marker file and exiting non-zero. It imitates no harness: no prompt, no
// output, no arguments read. A launch that ever reached its exec would
// run this script (never a real harness installed later on PATH) and
// leave the marker behind; every test here asserts the marker never
// exists.
type launchExecCanary struct {
	dir    string
	marker string
}

// newLaunchExecCanary writes one canary for t and registers the cleanup
// that fails t if the marker exists.
func newLaunchExecCanary(t *testing.T) *launchExecCanary {
	t.Helper()
	c := &launchExecCanary{dir: t.TempDir(), marker: filepath.Join(t.TempDir(), "launch-exec-reached")}
	if strings.ContainsAny(c.marker, "'\n") {
		t.Fatalf("the canary marker path cannot be quoted for the script")
	}
	script := "#!/bin/sh\n: > '" + c.marker + "'\nexit 97\n"
	if err := os.WriteFile(filepath.Join(c.dir, canaryHarnessName), []byte(script), 0o700); err != nil { //nolint:gosec // G306: the canary must be executable.
		t.Fatalf("write the launch exec canary: %v", err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(c.marker); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("hop launch reached its exec: the harness canary ran (stat: %v)", err)
		}
	})
	return c
}

// execLaunch runs one hop launch invocation under the isolated
// environment with a fresh launchExecCanary first on PATH.
func execLaunch(t *testing.T, hopVars map[string]string, dir string, args ...string) hopResult {
	t.Helper()
	canary := newLaunchExecCanary(t)
	return runHop(t, isolatedTestEnv(t, canary.dir, hopVars), dir, args...)
}

// TestGrammarContractLaunchExecCanaryIsLive proves the barrier would fire:
// under a launch invocation's own environment, lookupExecutable — the
// resolver composition hands PrepareSessionLaunchExec, applied to the
// same PATH value (sanitization never strips PATH) — resolves the
// configured harness name to the canary, and running what it resolves
// creates the marker.
func TestGrammarContractLaunchExecCanaryIsLive(t *testing.T) {
	c := newLaunchExecCanary(t)
	env := isolatedTestEnv(t, c.dir, nil)
	resolved, err := lookupExecutable(canaryHarnessName, envValue(env, envPath))
	if want := filepath.Join(c.dir, canaryHarnessName); err != nil || resolved != want {
		t.Fatalf("lookupExecutable(%s) = %q, %v; want the canary %q", canaryHarnessName, resolved, err, want)
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	run := exec.CommandContext(ctx, resolved) //nolint:gosec // G204: this test's own canary script.
	run.Env = env
	var exitErr *exec.ExitError
	if err := run.Run(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 97 {
		t.Fatalf("running the canary = %v, want exit 97", err)
	}
	if _, err := os.Stat(c.marker); err != nil {
		t.Fatalf("the canary ran but left no marker: %v", err)
	}
	if err := os.Remove(c.marker); err != nil {
		t.Fatalf("remove the control marker: %v", err)
	}
}

// TestGrammarContractLaunchUsage covers hop launch's usage-error branch
// (--run required, exactly one of --attempt/--session, unexpected
// argument); no fixture needed. Every hop launch invocation in this file
// runs through execLaunch, so each also proves its exec was never reached.
func TestGrammarContractLaunchUsage(t *testing.T) {
	dir := freshStateDir(t)
	cases := []struct {
		name string
		args []string
	}{
		{"missing run", []string{"launch", "--attempt", testUUID(1)}},
		{"neither attempt nor session", []string{"launch", "--run", testUUID(1)}},
		{"both attempt and session", []string{"launch", "--run", testUUID(1), "--attempt", testUUID(2), "--session", testUUID(3)}},
		{"unexpected argument", []string{"launch", "--run", testUUID(1), "--attempt", testUUID(2), "extra"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := execLaunch(t, map[string]string{"HOP_STATE_DIR": dir}, dir, tc.args...)
			if result.ExitCode != exitUsage {
				t.Fatalf("exit = %d, want %d (usage); stdout=%q stderr=%q", result.ExitCode, exitUsage, result.Stdout, result.Stderr)
			}
		})
	}
}

// TestGrammarContractLaunchStateDirRequired proves hop launch fails
// (not usage) when HOP_STATE_DIR is missing or relative, without ever
// opening a store.
func TestGrammarContractLaunchStateDirRequired(t *testing.T) {
	dir := realDir(t)
	t.Run("missing", func(t *testing.T) {
		result := execLaunch(t, map[string]string{}, dir, "launch", "--run", testUUID(1), "--attempt", testUUID(2))
		if result.ExitCode != exitFailure {
			t.Fatalf("exit = %d, want %d (failure); stdout=%q stderr=%q", result.ExitCode, exitFailure, result.Stdout, result.Stderr)
		}
	})
	t.Run("relative", func(t *testing.T) {
		result := execLaunch(t, map[string]string{"HOP_STATE_DIR": "relative/path"}, dir, "launch", "--run", testUUID(1), "--attempt", testUUID(2))
		if result.ExitCode != exitFailure {
			t.Fatalf("exit = %d, want %d (failure); stdout=%q stderr=%q", result.ExitCode, exitFailure, result.Stdout, result.Stderr)
		}
	})
}

// TestGrammarContractLaunchUnknownRun proves hop launch against a
// well-formed but unknown run/session and run/attempt pair fails as a
// command error, reached before any claim is written and long before any
// exec is attempted; no fixture needed.
func TestGrammarContractLaunchUnknownRun(t *testing.T) {
	dir := freshStateDir(t)
	cases := []struct {
		name string
		args []string
	}{
		{"unknown run, session form", []string{"launch", "--run", testUUID(1), "--session", testUUID(2)}},
		{"unknown run, attempt shim form", []string{"launch", "--run", testUUID(1), "--attempt", testUUID(2)}},
		{"malformed run id", []string{"launch", "--run", malformedUUID, "--attempt", testUUID(2)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := execLaunch(t, map[string]string{"HOP_STATE_DIR": dir}, dir, tc.args...)
			if result.ExitCode != exitFailure {
				t.Fatalf("exit = %d, want %d (failure); stdout=%q stderr=%q", result.ExitCode, exitFailure, result.Stdout, result.Stderr)
			}
			if result.Stdout != "" {
				t.Errorf("wrote to stdout: %q", result.Stdout)
			}
		})
	}
}

// launchEnv builds a hop launch invocation's process environment for a
// solo attempt-shim fixture: the pane-provided HOP_* set
// validateSessionLaunchEnvironment checks for agreement, overridable per
// case to construct a specific refusal.
func launchEnv(f *soloFixture, overrides map[string]string) map[string]string {
	env := map[string]string{
		"HOP_STATE_DIR":      f.StateRoot,
		"HOP_RUN_ID":         f.RunID,
		"HOP_INCARNATION_ID": f.IncarnationID,
		"HOP_TASK_ID":        f.TaskID,
		"HOP_ATTEMPT_ID":     f.AttemptID,
	}
	for k, v := range overrides {
		env[k] = v
	}
	return env
}

// TestGrammarContractLaunchEnvironmentDisagreement proves hop launch's
// refusal when the pane-provided HOP_RUN_ID does not agree with the
// session launch context loaded for --run/--attempt — reached only
// after LaunchBase has put the fixture's session in the launching state
// a claim requires, and entirely before any claim is written.
func TestGrammarContractLaunchEnvironmentDisagreement(t *testing.T) {
	f := newSoloReserved(t, 8500)
	f.launch(t)
	env := launchEnv(f, map[string]string{"HOP_RUN_ID": testUUID(9999)})
	result := execLaunch(t, env, f.StateRoot, "launch", "--run", f.RunID, "--attempt", f.AttemptID)
	if result.ExitCode != exitFailure {
		t.Fatalf("exit = %d, want %d (failure); stdout=%q stderr=%q", result.ExitCode, exitFailure, result.Stdout, result.Stderr)
	}
	if result.Stdout != "" {
		t.Errorf("wrote to stdout: %q", result.Stdout)
	}
}

// TestGrammarContractLaunchClaimConflict proves hop launch's refusal
// when a launch claim for the target incarnation already exists under a
// DIFFERENT pid than the real exec'd process's own — constructed without
// ever letting a launcher actually exec: SeedLaunchClaim writes the
// conflicting claim directly, under a fixed pid the real test process
// can never itself be.
func TestGrammarContractLaunchClaimConflict(t *testing.T) {
	f := newSoloReserved(t, 8600)
	f.launch(t)
	f.seedConflictingClaim(t)
	env := launchEnv(f, nil)
	result := execLaunch(t, env, f.StateRoot, "launch", "--run", f.RunID, "--attempt", f.AttemptID)
	if result.ExitCode != exitFailure {
		t.Fatalf("exit = %d, want %d (failure); stdout=%q stderr=%q", result.ExitCode, exitFailure, result.Stdout, result.Stderr)
	}
	if result.Stdout != "" {
		t.Errorf("wrote to stdout: %q", result.Stdout)
	}
}

// TestGrammarContractResultSubmitUsage covers hop result submit's
// usage-error branch (--summary/--commit required); no fixture needed.
func TestGrammarContractResultSubmitUsage(t *testing.T) {
	dir := freshStateDir(t)
	cases := []struct {
		name string
		args []string
	}{
		{"missing summary", []string{"result", "submit", "--commit", strings.Repeat("a", 40)}},
		{"missing commit", []string{"result", "submit", "--summary", "done"}},
		{"unexpected argument", []string{"result", "submit", "--summary", "done", "--commit", strings.Repeat("a", 40), "extra"}},
		{"dispatch: no subcommand", []string{"result"}},
		{"dispatch: unknown subcommand", []string{"result", "bogus"}},
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

// TestGrammarContractResultSubmitMalformed proves hop result submit's
// "malformed: <detail>" first line for a bad run/task/attempt/
// incarnation id or a badly formed commit object id — RecordMalformed
// writes a foreign-key-free receipt regardless of whether the claimed
// entities exist, so none of these cases needs a fixture beyond a freshly
// created (empty) store.
func TestGrammarContractResultSubmitMalformed(t *testing.T) {
	dir := freshStateDir(t)
	validEnv := map[string]string{
		"HOP_STATE_DIR":      dir,
		"HOP_RUN_ID":         testUUID(1),
		"HOP_TASK_ID":        testUUID(2),
		"HOP_ATTEMPT_ID":     testUUID(3),
		"HOP_INCARNATION_ID": testUUID(4),
	}
	commit := strings.Repeat("a", 40)

	for _, key := range []string{"HOP_RUN_ID", "HOP_TASK_ID", "HOP_ATTEMPT_ID", "HOP_INCARNATION_ID"} {
		t.Run(key+"_malformed", func(t *testing.T) {
			env := map[string]string{}
			for k, v := range validEnv {
				env[k] = v
			}
			env[key] = malformedUUID
			result := execHop(t, env, dir, "result", "submit", "--summary", "done", "--commit", commit)
			if !strings.HasPrefix(result.FirstStdoutLine(), "malformed: ") {
				t.Errorf("first line = %q, want a \"malformed: \" prefix; stdout=%q stderr=%q", result.FirstStdoutLine(), result.Stdout, result.Stderr)
			}
			if result.ExitCode != exitFailure {
				t.Errorf("exit = %d, want %d", result.ExitCode, exitFailure)
			}
		})
	}

	t.Run("commit not 40 hex", func(t *testing.T) {
		result := execHop(t, validEnv, dir, "result", "submit", "--summary", "done", "--commit", "not-a-commit")
		if !strings.HasPrefix(result.FirstStdoutLine(), "malformed: ") {
			t.Errorf("first line = %q, want a \"malformed: \" prefix; stdout=%q stderr=%q", result.FirstStdoutLine(), result.Stdout, result.Stderr)
		}
	})
}

// TestGrammarContractResultSubmitAccepted drives hop result submit's
// accepted and duplicate outcomes against a solo run driven fully
// running.
func TestGrammarContractResultSubmitAccepted(t *testing.T) {
	f := newSoloRunning(t, 8700)
	commit := strings.Repeat("b", 40)

	accepted := execHop(t, f.env(nil), f.StateRoot, "result", "submit", "--summary", "implemented the fixture change", "--commit", commit)
	if accepted.ExitCode != exitOK {
		t.Fatalf("accepted: exit=%d stdout=%q stderr=%q", accepted.ExitCode, accepted.Stdout, accepted.Stderr)
	}
	if !strings.HasPrefix(accepted.FirstStdoutLine(), "accepted ") {
		t.Fatalf("accepted first line = %q, want an \"accepted \" prefix", accepted.FirstStdoutLine())
	}

	duplicate := execHop(t, f.env(nil), f.StateRoot, "result", "submit", "--summary", "implemented the fixture change", "--commit", commit)
	if duplicate.ExitCode != exitOK {
		t.Fatalf("duplicate: exit=%d stdout=%q stderr=%q", duplicate.ExitCode, duplicate.Stdout, duplicate.Stderr)
	}
	if !strings.HasPrefix(duplicate.FirstStdoutLine(), "duplicate ") {
		t.Errorf("duplicate first line = %q, want a \"duplicate \" prefix", duplicate.FirstStdoutLine())
	}
}
