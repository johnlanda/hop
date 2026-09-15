package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The fixture worker stands in for the launched harness end to end: it is
// installed under the recognized name "claude" so hop launch's PATH
// resolution (section 6) finds it, reads the HOP_* environment and locates
// the assignment artifact exactly the way section 6 delivers it, validates
// its delivered content, and submits through the real `hop result submit`
// at the hop path its own prompt argv names. Its behavior is scriptable
// through a "FIXTURE-BEHAVIOR: <name> [args...]" directive line the test
// embeds in hop run's brief — delivered to the worker through the very same
// brief -> assignment.md channel a real brief travels through, so no
// separate plumbing is needed through the controller's fixed HOP_* env
// keys. fixtureWorkerBrief renders that line.
//
// Locating the assignment file: usecase_run.go computes its path as
// filepath.Join(StateRoot, "runs", <run-uuid>, "artifacts", "assignment.md")
// — a stable convention independent of the prompt's exact wording — so the
// worker computes it the same way from HOP_STATE_DIR and HOP_RUN_ID rather
// than parsing it out of the prompt.
//
// The hop path has no such independent source: it reaches the worker only
// through the prompt argv, by design. internal/app/usecase_execboundary.go's
// renderInitialPrompt (the ACTUAL landed prompt renderer hop launch uses,
// distinct from internal/app/assignment.go's renderAssignment, which renders
// the separate assignment.md FILE content) renders exactly:
//
//	Read your assignment at %s and complete it. When your work is
//	committed, submit it by running: %s result submit --summary
//	"<one-line summary>" --commit <commit-oid>. If the first output line
//	begins with "transient", wait briefly and run the exact same command
//	again.
//
// — a single-line string with no separate "submit" line to isolate, so the
// worker extracts both paths from this exact wording: the assignment path
// between "Read your assignment at " and " and complete it." — cross-
// validated against the independently computed path above, and a mismatch
// or an absent marker fails the run loudly rather than silently preferring
// one source — and the hop path between "submit it by running: " and
// " result submit --summary" (its only source: this worker never falls back
// to a PATH lookup or a guessed location for the hop binary).
const fixtureWorkerSource = `package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// reexecMarkerEnv, once present in the environment, means this process is
// the re-exec target of the exec-keep-pid behavior: skip straight to idling
// so the second incarnation cannot re-exec forever.
const reexecMarkerEnv = "HOP_FIXTURE_REEXEC_DONE"

func main() {
	if os.Getenv(reexecMarkerEnv) != "" {
		fmt.Printf("FIXTURE-REEXECED pid=[%d]\n", os.Getpid())
		idle()
		return
	}

	env := requireEnv("HOP_STATE_DIR", "HOP_RUN_ID", "HOP_TASK_ID", "HOP_ATTEMPT_ID", "HOP_INCARNATION_ID")
	assignmentPath := filepath.Join(env["HOP_STATE_DIR"], "runs", env["HOP_RUN_ID"], "artifacts", "assignment.md")

	prompt := ""
	if len(os.Args) > 0 {
		prompt = os.Args[len(os.Args)-1]
	}
	promptAssignmentPath := extractMarked(prompt, "Read your assignment at ", " and complete it.")
	hopPath := extractMarked(prompt, "submit it by running: ", " result submit --summary")
	if hopPath == "" {
		fatalf("prompt does not carry the %q marker naming the hop path", "submit it by running: ")
	}
	if promptAssignmentPath == "" {
		fatalf("prompt does not carry the %q marker naming the assignment path", "Read your assignment at ")
	}
	// The StateRoot-derived path is what this worker actually reads, but the
	// prompt's own path is cross-validated against it rather than merely
	// logged: a mismatch means the launch delivered a different assignment
	// than the one this run's own state root computes, which must fail the
	// scenario loudly, never silently prefer one source over the other.
	if promptAssignmentPath != assignmentPath {
		fatalf("prompt's assignment path (%s) does not match the path computed from HOP_STATE_DIR/HOP_RUN_ID (%s)", promptAssignmentPath, assignmentPath)
	}

	assignmentContent, err := os.ReadFile(assignmentPath)
	if err != nil {
		fatalf("read assignment %s: %v", assignmentPath, err)
	}
	behavior, behaviorArgs := parseBehavior(string(assignmentContent))

	writeObservation(filepath.Join(filepath.Dir(assignmentPath), "worker-observed.txt"), assignmentPath, promptAssignmentPath, hopPath, string(assignmentContent), behavior, behaviorArgs)
	fmt.Println("FIXTURE-WORKER-READY")

	switch behavior {
	case "submit-valid":
		oid := commitChange("fixture worker change")
		submitOnce(hopPath, oid, "fixture worker result")
	case "submit-stale":
		waitForGo()
		oid := commitChange("fixture worker stale change")
		submitOnce(hopPath, oid, "fixture worker stale result")
	case "submit-twice":
		oid := commitChange("fixture worker change")
		submitOnce(hopPath, oid, "fixture worker result")
		submitOnce(hopPath, oid, "fixture worker result")
	case "exit-without-submitting":
		waitForGo()
		return
	case "exec-keep-pid":
		waitForGo()
		reexecSelf()
	default:
		// Unknown or empty directive: submit nothing, just stay alive, so a
		// scenario that only needs a settled, idle worker still gets one.
	}
	idle()
}

func requireEnv(keys ...string) map[string]string {
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		value := os.Getenv(key)
		if value == "" {
			fatalf("required %s is not set", key)
		}
		values[key] = value
	}
	return values
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fixture worker: "+format+"\n", args...)
	os.Exit(1)
}

// extractMarked returns the text between the first occurrence of start and
// the following occurrence of end, or "" if either is absent.
func extractMarked(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	i += len(start)
	j := strings.Index(s[i:], end)
	if j < 0 {
		return ""
	}
	return s[i : i+j]
}

// parseBehavior finds the "FIXTURE-BEHAVIOR: <name> [args...]" directive
// line the test embedded in hop run's brief text and returns the behavior
// name and its extra arguments. An assignment carrying no directive returns
// an empty behavior, which idles without submitting anything.
func parseBehavior(assignment string) (behavior string, args []string) {
	const prefix = "FIXTURE-BEHAVIOR: "
	for _, line := range strings.Split(assignment, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(trimmed, prefix))
		if len(fields) == 0 {
			return "", nil
		}
		return fields[0], fields[1:]
	}
	return "", nil
}

// writeObservation dumps everything this worker observed to path,
// atomically (write-temp-then-rename), so the test can read it back after
// the fact rather than trusting this process's own judgment of correctness
// — the same philosophy the Phase 1 spike fixture uses for its environment
// dump. promptAssignmentPath is the prompt's own marker extraction, already
// verified equal to assignmentPath (the path actually read) by the time this
// is called — main fails closed before reaching here on any mismatch. The
// dump carries the worker's COMPLETE inherited environment (os.Environ()),
// not just the required HOP_* subset, so a scenario can assert the full
// sanitized-exec contract directly: every strip-matrix and policy-strip
// variable absent even when seeded into the server's own environment,
// HERDR_*/HOP_* present, and passthrough/profile entries exactly as
// configured (docs/plan/phase-2-design.md section 6).
func writeObservation(path string, assignmentPath, promptAssignmentPath, hopPath, assignmentContent, behavior string, behaviorArgs []string) {
	var b strings.Builder
	fmt.Fprintf(&b, "pid=%d\n", os.Getpid())
	// os.Executable() is this process's own resolved image path — the
	// absolute path hop launch's PATH-based harness resolution actually
	// exec'd. A scenario asserts this equals the fixture-installed stub
	// path exactly, never a real claude binary elsewhere on the machine.
	if executable, execErr := os.Executable(); execErr == nil {
		fmt.Fprintf(&b, "executable=%s\n", executable)
	} else {
		fmt.Fprintf(&b, "executable_error=%v\n", execErr)
	}
	fmt.Fprintf(&b, "assignment_path=%s\n", assignmentPath)
	fmt.Fprintf(&b, "prompt_assignment_path=%s\n", promptAssignmentPath)
	fmt.Fprintf(&b, "hop_path=%s\n", hopPath)
	fmt.Fprintf(&b, "behavior=%s\n", behavior)
	fmt.Fprintf(&b, "behavior_args=%s\n", strings.Join(behaviorArgs, " "))
	environ := os.Environ()
	sort.Strings(environ)
	for _, entry := range environ {
		fmt.Fprintf(&b, "env:%s\n", entry)
	}
	fmt.Fprintf(&b, "assignment_content_begin\n%s\nassignment_content_end\n", assignmentContent)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		fatalf("write observation: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		fatalf("rename observation into place: %v", err)
	}
}

// commitChange writes a small, unique change to the working tree (its own
// cwd, which hop launch's exec leaves at the launched worktree, per
// WorkerPaneRequest's Cwd) and commits it, using the fixture repository's
// own local git identity (initFixtureRepo, in the Go test process) — nothing
// here depends on HOME or any environment variable surviving the sanitized
// exec boundary. It returns the new commit's object id.
func commitChange(message string) string {
	path := fmt.Sprintf("fixture-worker-change-%d.txt", time.Now().UnixNano())
	if err := os.WriteFile(path, []byte(message+"\n"), 0o644); err != nil {
		fatalf("write change: %v", err)
	}
	runGit("add", "-A")
	runGit("commit", "-m", message)
	return strings.TrimSpace(runGit("rev-parse", "HEAD^{commit}"))
}

func runGit(args ...string) string {
	cmd := exec.Command("git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// submitOnce runs "<hopPath> result submit --summary <summary> --commit
// <oid>", retrying while the first stdout line begins with "transient" (the
// assignment template's own retry instruction), bounded so a persistent
// rejection does not hang the worker forever. It prints the final outcome
// so the test can read it from the pane/log evidence in addition to the
// observation dump.
func submitOnce(hopPath, oid, summary string) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		cmd := exec.Command(hopPath, "result", "submit", "--summary", summary, "--commit", oid)
		var out strings.Builder
		cmd.Stdout = &out
		cmd.Stderr = &out
		runErr := cmd.Run()
		first, _, _ := strings.Cut(out.String(), "\n")
		fmt.Printf("FIXTURE-SUBMIT-RESULT err=[%v] first-line=[%s]\n", runErr, first)
		if strings.HasPrefix(first, "transient") && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		return
	}
}

// waitForGo blocks on stdin for a line reading exactly "FIXTURE-GO",
// letting the test control this worker's timing precisely (e.g. drive a
// relaunch that retires this incarnation before releasing a submit-stale
// worker). Stdin reaching EOF before that line is treated as an immediate
// go-ahead, so a test that does not care about precise timing can just
// close stdin.
func waitForGo() {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "FIXTURE-GO" {
			return
		}
	}
}

// reexecSelf replaces this process image via execve while keeping its pid,
// proving the exec-keep-pid capability the design's claim-settlement
// predicate depends on (section 6: "execve preserves it"). The child
// carries reexecMarkerEnv so it idles immediately instead of looping.
func reexecSelf() {
	self, err := os.Executable()
	if err != nil {
		fatalf("resolve own executable for re-exec: %v", err)
	}
	env := append(os.Environ(), reexecMarkerEnv+"=1")
	if err := syscall.Exec(self, []string{self}, env); err != nil {
		fatalf("exec self: %v", err)
	}
}

// idle keeps this process alive as the pane's foreground process (worker
// success and failure are both observed only through the result protocol
// and check, never through pane exit — section 5) until told to stop:
// FIXTURE-QUIT exits 0, FIXTURE-QUIT-FAIL exits 3, and stdin EOF exits 0
// like a worker that was simply closed.
func idle() {
	fmt.Println("FIXTURE-WORKER-IDLE")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		switch strings.TrimSpace(scanner.Text()) {
		case "FIXTURE-QUIT":
			return
		case "FIXTURE-QUIT-FAIL":
			os.Exit(3)
		}
	}
}
`

// fixtureWorkerBrief renders a hop run brief whose text embeds the fixture
// worker's scripted behavior directive, so the brief -> assignment.md
// delivery path (section 6) is what carries the script to the worker — no
// separate plumbing through the controller's fixed HOP_* env keys. Known
// behavior names: "submit-valid", "submit-stale", "submit-twice",
// "exit-without-submitting", "exec-keep-pid"; an empty or unrecognized
// behavior makes the worker idle without ever submitting.
func fixtureWorkerBrief(behavior string) string {
	return "FIXTURE-BEHAVIOR: " + behavior + "\n"
}

// buildFixtureWorker compiles fixtureWorkerSource once for the calling test
// into a temporary module under the test's artifact directory and installs
// it under the name "claude" — never anywhere on the real system — so hop
// launch's PATH-based harness resolution (section 6) finds it exactly as it
// would the real Claude Code binary.
func buildFixtureWorker(t *testing.T, artifacts *artifactDir) string {
	t.Helper()
	src := artifacts.dir(t, "fixture-worker-src")
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module fixtureworker\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(fixtureWorkerSource), 0o600); err != nil {
		t.Fatal(err)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go tool is required to build the fixture worker: %v", err)
	}
	binDir := artifacts.dir(t, "fixture-worker-bin")
	out := filepath.Join(binDir, "claude")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, goBin, "build", "-o", out, ".") //nolint:gosec // G204: the go tool builds this test's own generated fixture module.
	build.Dir = src
	if combined, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("go build fixture worker: %v\n%s", buildErr, combined)
	}
	return out
}

// testAssignmentPrompt reproduces internal/app/usecase_execboundary.go's
// renderInitialPrompt byte for byte (that function is unexported, so this
// package cannot call it directly) — the actual landed prompt hop launch
// composes into the harness argv. Used only to exercise the fixture worker's
// parsing directly, in isolation from cmd/hop and herdr; the full
// run/launch/corroborate scenarios exercise the real rendering end to end.
func testAssignmentPrompt(assignmentPath, hopPath string) string {
	return fmt.Sprintf("Read your assignment at %s and complete it. "+
		"When your work is committed, submit it by running: %s result submit --summary \"<one-line summary>\" --commit <commit-oid>. "+
		"If the first output line begins with \"transient\", wait briefly and run the exact same command again.",
		assignmentPath, hopPath)
}

// TestFixtureWorkerSubmitValid drives the compiled fixture worker directly
// (no herdr, no cmd/hop) against a fixture repository and a fake hop stub
// that answers "transient" once before succeeding, proving: HOP_* env
// validation, prompt parsing for the hop path, assignment-path computation
// from HOP_STATE_DIR/HOP_RUN_ID, FIXTURE-BEHAVIOR directive extraction from
// the brief, a real git commit against the fixture repository's own
// identity, transient-retry, and graceful idle-then-quit.
func TestFixtureWorkerSubmitValid(t *testing.T) {
	artifacts := newArtifactDir(t)
	worker := buildFixtureWorker(t, artifacts)
	repo := newFixtureRepo(t, artifacts, "repo")

	stateDir := artifacts.dir(t, "state")
	runID := "11111111-1111-1111-1111-111111111111"
	runDir := filepath.Join(stateDir, "runs", runID, "artifacts")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	assignmentPath := filepath.Join(runDir, "assignment.md")
	brief := fixtureWorkerBrief("submit-valid")
	assignmentContent := "# HOP Assignment\n\n## Brief\n\n" + brief + "\n## Instructions\n"
	if err := os.WriteFile(assignmentPath, []byte(assignmentContent), 0o600); err != nil {
		t.Fatal(err)
	}

	hopStub, transientCountFile := writeTransientOnceHopStub(t, artifacts)
	prompt := testAssignmentPrompt(assignmentPath, hopStub)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, worker, "--session-id", "22222222-2222-2222-2222-222222222222", prompt) //nolint:gosec // G204: fixed test-owned binary and arguments.
	cmd.Dir = repo.Root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOP_STATE_DIR=" + stateDir,
		"HOP_RUN_ID=" + runID,
		"HOP_TASK_ID=33333333-3333-3333-3333-333333333333",
		"HOP_ATTEMPT_ID=44444444-4444-4444-4444-444444444444",
		"HOP_INCARNATION_ID=55555555-5555-5555-5555-555555555555",
	}
	var stdout strings.Builder
	cmd.Stdout = &stdout
	cmd.Stdin = strings.NewReader("FIXTURE-QUIT\n")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run fixture worker: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"FIXTURE-WORKER-READY", "FIXTURE-SUBMIT-RESULT", "FIXTURE-WORKER-IDLE"} {
		if !strings.Contains(out, want) {
			t.Errorf("worker stdout missing %q; got:\n%s", want, out)
		}
	}

	calls, err := os.ReadFile(transientCountFile) //nolint:gosec // G304: a path this test constructed itself, under its own artifact directory.
	if err != nil {
		t.Fatalf("read hop stub call count: %v", err)
	}
	if strings.TrimSpace(string(calls)) != "2" {
		t.Errorf("hop stub called %s times, want 2 (one transient, one accepted)", strings.TrimSpace(string(calls)))
	}

	observed, err := os.ReadFile(filepath.Join(runDir, "worker-observed.txt")) //nolint:gosec // G304: a path this test constructed itself, under its own artifact directory.
	if err != nil {
		t.Fatalf("read worker observation dump: %v", err)
	}
	dump := string(observed)
	for _, want := range []string{
		"hop_path=" + hopStub,
		"behavior=submit-valid",
		"env:HOP_RUN_ID=" + runID,
		"assignment_content_begin",
		brief,
	} {
		if !strings.Contains(dump, want) {
			t.Errorf("worker observation dump missing %q; got:\n%s", want, dump)
		}
	}

	head := repo.git(t, "rev-parse", "HEAD^{commit}")
	if head == repo.Base {
		t.Error("fixture worker did not commit a change before submitting")
	}
}

// writeTransientOnceHopStub writes a fake `hop` at a fresh path that prints
// "transient: retry" and exits 1 on its first invocation, then prints a
// normal acceptance line and exits 0 on every later call, counting
// invocations in a sibling file — a minimal stand-in for the real `hop
// result submit`, used only to prove the fixture worker's own retry
// behavior in isolation from cmd/hop.
func writeTransientOnceHopStub(t *testing.T, artifacts *artifactDir) (hopPath, countFile string) {
	t.Helper()
	dir := artifacts.dir(t, "hop-stub")
	countFile = filepath.Join(dir, "calls")
	if err := os.WriteFile(countFile, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"n=$(cat '" + countFile + "')\n" +
		"n=$((n + 1))\n" +
		"echo \"$n\" > '" + countFile + "'\n" +
		"if [ \"$n\" -eq 1 ]; then\n" +
		"  echo 'transient: attempt not yet running; retry'\n" +
		"  exit 1\n" +
		"fi\n" +
		"echo 'accepted result deadbeef'\n" +
		"exit 0\n"
	hopPath = filepath.Join(dir, "hop")
	if err := os.WriteFile(hopPath, []byte(script), 0o755); err != nil { //nolint:gosec // G306: this stub must be executable; it lives in this test's own artifact directory.
		t.Fatal(err)
	}
	return hopPath, countFile
}

// TestFixtureWorkerExitWithoutSubmitting proves the exit-without-submitting
// behavior never invokes hop at all, and that closing stdin (no FIXTURE-GO)
// is treated as an immediate go-ahead.
func TestFixtureWorkerExitWithoutSubmitting(t *testing.T) {
	artifacts := newArtifactDir(t)
	worker := buildFixtureWorker(t, artifacts)
	repo := newFixtureRepo(t, artifacts, "repo")

	stateDir := artifacts.dir(t, "state")
	runID := "66666666-6666-6666-6666-666666666666"
	runDir := filepath.Join(stateDir, "runs", runID, "artifacts")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	assignmentPath := filepath.Join(runDir, "assignment.md")
	brief := fixtureWorkerBrief("exit-without-submitting")
	if err := os.WriteFile(assignmentPath, []byte("# HOP Assignment\n\n## Brief\n\n"+brief+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	hopStub, countFile := writeTransientOnceHopStub(t, artifacts)
	prompt := testAssignmentPrompt(assignmentPath, hopStub)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, worker, "--session-id", "77777777-7777-7777-7777-777777777777", prompt) //nolint:gosec // G204: fixed test-owned binary and arguments.
	cmd.Dir = repo.Root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOP_STATE_DIR=" + stateDir,
		"HOP_RUN_ID=" + runID,
		"HOP_TASK_ID=88888888-8888-8888-8888-888888888888",
		"HOP_ATTEMPT_ID=99999999-9999-9999-9999-999999999999",
		"HOP_INCARNATION_ID=00000000-0000-0000-0000-000000000000",
	}
	// No FIXTURE-GO is sent; stdin closes at once, which waitForGo treats as
	// an immediate go-ahead, and exit-without-submitting returns without
	// ever idling.
	if err := cmd.Run(); err != nil {
		t.Fatalf("run fixture worker: %v", err)
	}

	calls, err := os.ReadFile(countFile) //nolint:gosec // G304: a path this test constructed itself, under its own artifact directory.
	if err != nil {
		t.Fatalf("read hop stub call count: %v", err)
	}
	if strings.TrimSpace(string(calls)) != "0" {
		t.Errorf("hop stub called %s times, want 0 (exit-without-submitting must never invoke hop)", strings.TrimSpace(string(calls)))
	}
	if head := repo.git(t, "rev-parse", "HEAD^{commit}"); head != repo.Base {
		t.Error("exit-without-submitting committed a change; it must leave the repository untouched")
	}
}

// workerObservation is the parsed contents of one worker-observed.txt dump
// (writeObservation, inside fixtureWorkerSource).
type workerObservation struct {
	Fields            map[string]string // pid, assignment_path, prompt_assignment_path, hop_path, behavior, behavior_args
	Environ           []string          // every "NAME=VALUE" entry from the worker's own os.Environ()
	AssignmentContent string
}

// readWorkerObservation reads and parses one worker-observed.txt dump.
func readWorkerObservation(t *testing.T, path string) workerObservation {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a path this test constructed itself, under its own artifact/state directory.
	if err != nil {
		t.Fatalf("read worker observation %s: %v", path, err)
	}
	obs := workerObservation{Fields: map[string]string{}}
	lines := strings.Split(string(raw), "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		switch {
		case line == "assignment_content_begin":
			var content []string
			for i++; i < len(lines) && lines[i] != "assignment_content_end"; i++ {
				content = append(content, lines[i])
			}
			obs.AssignmentContent = strings.Join(content, "\n")
		case strings.HasPrefix(line, "env:"):
			obs.Environ = append(obs.Environ, strings.TrimPrefix(line, "env:"))
		default:
			if key, value, ok := strings.Cut(line, "="); ok {
				obs.Fields[key] = value
			}
		}
	}
	return obs
}

// HasEnv reports whether the observed environment carries name=value
// exactly.
func (o workerObservation) HasEnv(name, value string) bool {
	return slices.Contains(o.Environ, name+"="+value)
}

// HasEnvName reports whether the observed environment carries any entry for
// name, regardless of value.
func (o workerObservation) HasEnvName(name string) bool {
	for _, entry := range o.Environ {
		if n, _, ok := strings.Cut(entry, "="); ok && n == name {
			return true
		}
	}
	return false
}
