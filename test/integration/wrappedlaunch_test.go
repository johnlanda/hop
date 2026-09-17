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

// wrappedLaunchStubSource is the implementer's resolved "claude" for this
// scenario: a stub that puts one OTHER process carrying the launch
// identity into the pane's foreground group, deterministically, before
// the claimed process itself can satisfy the corroboration predicate.
//
// Determinism is the whole design. `hop launch` writes its claim and then
// execs this stub in the same pid, so the instant the stub carries the
// launched argv the predicate could settle. The stub therefore re-execs
// ITSELF first, in place, with its args stripped — keeping the claim's
// executable identity in argv[0] but carrying no launch marker, which the
// predicate reads as unresolved, never settled. Only then does it put the
// other process up, wait for it to be observable, and finally exec the
// real fixture worker with the original argv restored. From that exec
// onwards the foreground group holds the claimed process AND a matching
// different-pid process at the same time — with no window in which it
// held only the claimed one.
//
// The other process is started through an intermediate that exits as soon
// as it is up, so it is reparented to init rather than left a zombie
// child of a process that has exec'd away and will never reap it; a
// zombie's listing is exactly the kind of platform detail no decision here
// should depend on. It exits by itself the moment the test's gate file
// appears, so nothing is ever signaled by an observed pid.
//
// Roles other than the implementer exec the real worker immediately: the
// manager and reviewer launches are ordinary in this scenario.
const wrappedLaunchStubSource = `package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "wrapped-launch stub: "+format+"\n", args...)
	os.Exit(1)
}

// execWorker becomes the real fixture worker in this pid, under argv.
func execWorker(argv []string, env []string) {
	worker := os.Getenv("HOP_WEDGE_WORKER")
	if worker == "" {
		fail("HOP_WEDGE_WORKER is not set")
	}
	if err := syscall.Exec(worker, argv, env); err != nil {
		fail("exec the real worker: %v", err)
	}
}

// withPhase returns the environment with the stub's phase set to phase and
// the original argv carried across the exec that strips it.
func withPhase(phase string, argv []string) []string {
	out := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HOP_WEDGE_PHASE=") || strings.HasPrefix(kv, "HOP_WEDGE_ARGV=") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "HOP_WEDGE_PHASE="+phase)
	if argv != nil {
		out = append(out, "HOP_WEDGE_ARGV="+strings.Join(argv, "\n"))
	}
	return out
}

// launchedArgv is the argv HOP launched, carried across the stripped exec.
func launchedArgv() []string {
	raw := os.Getenv("HOP_WEDGE_ARGV")
	if raw == "" {
		fail("HOP_WEDGE_ARGV is not set")
	}
	return strings.Split(raw, "\n")
}

// selfPath is this stub's own image: the resolved harness path HOP's claim
// recorded, which argv[0] also names.
func selfPath() string {
	exe, err := os.Executable()
	if err != nil {
		fail("resolve my own path: %v", err)
	}
	return exe
}

// start runs this stub again in phase under argv, without waiting.
func start(phase string, argv []string) *exec.Cmd {
	cmd := exec.Command(selfPath())
	cmd.Args = argv
	cmd.Env = withPhase(phase, argv)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fail("start phase %s: %v", phase, err)
	}
	return cmd
}

// waitForFile blocks until path exists, bounded.
func waitForFile(path string) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			fail("%s never appeared", path)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func main() {
	ready, gate := os.Getenv("HOP_WEDGE_READY"), os.Getenv("HOP_WEDGE_GATE")
	if ready == "" || gate == "" {
		fail("the ready and gate paths are required")
	}

	switch os.Getenv("HOP_WEDGE_PHASE") {
	case "holder":
		// The other process carrying the launch identity: the launched argv
		// under a pid of its own, in the pane's own foreground group. It
		// announces itself and then leaves on the test's gate alone.
		if err := os.WriteFile(ready, nil, 0o600); err != nil {
			fail("announce the holder: %v", err)
		}
		waitForFile(gate)
		return
	case "spawner":
		// Started and reaped by the phase below, so the holder it leaves
		// behind is reparented to init.
		start("holder", os.Args)
		waitForFile(ready)
		return
	case "stripped":
		// argv[0] alone: the claim's executable identity with no launch
		// marker, so the predicate cannot settle this pid yet.
		argv := launchedArgv()
		spawner := start("spawner", argv)
		if err := spawner.Wait(); err != nil {
			fail("the spawner failed: %v", err)
		}
		waitForFile(ready)
		execWorker(argv, withPhase("", nil))
		return
	}

	if os.Getenv("HOP_ROLE") != "implementer" {
		execWorker(os.Args, os.Environ())
		return
	}
	if err := syscall.Exec(selfPath(), os.Args[:1], withPhase("stripped", os.Args)); err != nil {
		fail("strip my own argv: %v", err)
	}
}
`

// buildWrappedLaunchStub compiles wrappedLaunchStubSource for the calling
// test.
func buildWrappedLaunchStub(t *testing.T, artifacts *artifactDir) string {
	t.Helper()
	src := artifacts.dir(t, "wrapped-launch-src")
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module wrappedlaunch\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(wrappedLaunchStubSource), 0o600); err != nil {
		t.Fatal(err)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go tool is required to build the wrapped-launch stub: %v", err)
	}
	out := filepath.Join(artifacts.dir(t, "wrapped-launch-bin"), "stub")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, goBin, "build", "-o", out, ".") //nolint:gosec // G204: the go tool builds this test's own generated fixture module.
	build.Dir = src
	if combined, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("go build wrapped-launch stub: %v\n%s", buildErr, combined)
	}
	return out
}

// wrappedLaunchPersistence is how long the scenario watches a wrapped
// launch stay unsettled before releasing it: several controller
// corroboration passes (loopPollInterval is 2s in production), so the
// fail-closed outcome is proven to hold across passes rather than to have
// been one pass's blip.
const wrappedLaunchPersistence = 8 * time.Second

// TestRealProcessLaunchCorroborationRevisitsAWrappedLaunch is the
// real-process regression for the live launch corroboration's revisit rule
// (docs/plan/phase-3-design.md section 6). An implementer launch whose
// pane's foreground group holds another process carrying the launch
// identity fails closed to `reconciling` — and STAYS there, unsettled,
// across passes, with the live reason recorded and the human action
// rendered. Once that other process leaves, the very next pass re-inspects
// the same session, settles its claim, activates it, and the attempt
// completes the run.
//
// Before the fix the live corroboration inspected only `launching`
// sessions, so the session it had just moved to `reconciling` was never
// looked at again: the claim stayed `exec_pending`, the worker retried
// `transient: attempt not yet running` forever, and the run never
// completed. This scenario fails there at the post-release waits.
func TestRealProcessLaunchCorroborationRevisitsAWrappedLaunch(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)

	worker := buildFixtureWorker(t, artifacts)
	realWorkerPath := filepath.Join(artifacts.dir(t, "real-worker-bin"), "claude-real")
	copyExecutable(t, worker, realWorkerPath)
	installFixtureWorkerAsClaudeStub(t, server, buildWrappedLaunchStub(t, artifacts))

	gates := artifacts.dir(t, "wrapped-launch-gates")
	ready := filepath.Join(gates, "holder-ready")
	gate := filepath.Join(gates, "release")
	server.extraEnv = append(server.extraEnv,
		"HOP_WEDGE_WORKER="+realWorkerPath,
		"HOP_WEDGE_READY="+ready,
		"HOP_WEDGE_GATE="+gate)
	release := releaseWrappedLaunch(t, gate)
	// Registered before the server starts: every failure path lets the
	// other process leave on its own, and no pid is ever signaled.
	t.Cleanup(release)
	server.start(t)

	scratchDir := artifacts.dir(t, "fixture-scratch")
	repo := newFeatureFixtureRepo(t, artifacts, server, "wrapped-launch-repo", featureFixtureOptions{
		ScratchDir: scratchDir, ReviewerBehavior: "reviewer-approve",
		MaxWorkers: 1, RetryLimit: 3, MessageWaitTimeout: "3s", MessageAttentionAfter: "30s",
	})
	brief := fixtureManagerBrief(scratchDir,
		[]fixtureManagerTask{{Label: "t1", Title: "Implement t1", Behavior: "worker-implement"}}, nil, "")
	fx := startFeatureRun(t, artifacts, server, repo, scratchDir, brief)

	// Every read below the decisive assertion is a PRECONDITION with its
	// own bounded, failing wait: nothing reads across a controller
	// scheduling pass, and a precondition that times out says so, so it can
	// never be mistaken for the defect.
	t1 := requireWrappedTask(t, fx)
	attemptID, sessionID := requireWrappedAttempt(t, fx, t1)
	requireWrappedReconciling(t, fx, sessionID)
	requireWrappedGroup(t, fx, sessionID)

	if reason := reconcilingReason(t, fx, sessionID); !strings.HasPrefix(reason, "launch corroboration: ") {
		t.Errorf("reconciling transition reason = %q, want the live corroboration's own value-free reason, not resume's", reason)
	}

	// It holds across passes, and settles nothing. waitUntilDeadline
	// returning false here is the asserted outcome, not a timeout failure.
	waitUntilDeadline(wrappedLaunchPersistence, func() bool {
		return fx.claimState(t, sessionID) != "exec_pending"
	})
	if state := fx.claimState(t, sessionID); state != "exec_pending" {
		t.Fatalf("launch claim state = %q after %s of a live wrapped launch, want it to stay exec_pending", state, wrappedLaunchPersistence)
	}
	if state := fx.sessionState(t, sessionID); state != "reconciling" {
		t.Fatalf("session state = %q after %s, want it to stay reconciling", state, wrappedLaunchPersistence)
	}
	// Exactly one entry into reconciling: the fail-closed branch writes
	// nothing on the passes that follow.
	if n := fx.transitionCount(t, "session", sessionID, "launching", "reconciling"); n != 1 {
		t.Errorf("session launching->reconciling transition count = %d, want 1 (a persistent wrapper writes nothing further)", n)
	}
	// And the human sees the action, value-free, under that session's line.
	requireStatusAction(t, fx, sessionID)

	// THE DECISIVE ASSERTION. The other process leaves; the controller must
	// re-inspect the very session it had already moved to reconciling.
	release()
	requireRevisitedAndSettled(t, fx, sessionID)
	fx.requireAttemptState(t, attemptID, "running", "checking", "integrating", "completed")

	// The run finishes: the worker's own retried submission is accepted
	// once its attempt is running, and the ordinary feature flow follows.
	fx.requireTaskState(t, t1, "integrated")
	reviewTaskID := fx.requireReviewTask(t)
	fx.requireTaskState(t, reviewTaskID, "completed")
	if verdict, verdictOK := fx.reviewVerdict(t, reviewTaskID); !verdictOK || verdict != "approve" {
		t.Errorf("review verdict = %q (found=%v), want approve", verdict, verdictOK)
	}
	fx.requireRunState(t, "completed")
	if n := fx.resultCount(t, attemptID); n != 1 {
		t.Errorf("attempt %s has %d result rows, want exactly 1", attemptID, n)
	}
}

// The two kinds of failure this scenario can produce, prefixed so neither
// can be mistaken for the other. A precondition failure means the scenario
// never reached the point where it says anything about the revisit rule —
// the wrapper window was not held, or the controller had not acted yet —
// and proves nothing either way. The decisive failure is the defect.
const (
	wrappedLaunchPrecondition = "PRECONDITION NOT REACHED — this scenario proved nothing about the revisit rule:"
	wrappedLaunchDecisive     = "DECISIVE ASSERTION FAILED — the reconciling session was never re-inspected:"
)

// requireWrappedTask waits for the scripted manager's own task to exist
// and to be assigned (ready -> active), which is the transaction that also
// reserves its attempt.
func requireWrappedTask(t *testing.T, fx *featureRun) string {
	t.Helper()
	taskID, found := fx.waitForTaskBySeq(t, 1, featureRunTimeout)
	if !found {
		t.Fatalf("%s the scripted manager never created its task within %s", wrappedLaunchPrecondition, featureRunTimeout)
	}
	state := ""
	if !waitUntilDeadline(featureRunTimeout, func() bool {
		state = fx.taskState(t, taskID)
		return state == "active"
	}) {
		t.Fatalf("%s task %s is %q after %s, want active (the assignment pass never ran)", wrappedLaunchPrecondition, taskID, state, featureRunTimeout)
	}
	return taskID
}

// requireWrappedAttempt waits for the assignment transaction's own rows —
// the reserved attempt and the session bound to it — instead of reading
// them the instant the task exists.
func requireWrappedAttempt(t *testing.T, fx *featureRun, taskID string) (attemptID, sessionID string) {
	t.Helper()
	if !waitUntilDeadline(featureRunTimeout, func() bool {
		attemptID = fx.scalar(t, fmt.Sprintf("SELECT id FROM attempts WHERE task_id = '%s' ORDER BY number DESC LIMIT 1;", taskID))
		if attemptID == "" {
			return false
		}
		sessionID = fx.scalar(t, fmt.Sprintf("SELECT id FROM sessions WHERE attempt_id = '%s' ORDER BY rowid DESC LIMIT 1;", attemptID))
		return sessionID != ""
	}) {
		t.Fatalf("%s no attempt (%q) and session (%q) for task %s after %s", wrappedLaunchPrecondition, attemptID, sessionID, taskID, featureRunTimeout)
	}
	if number := fx.scalar(t, fmt.Sprintf("SELECT number FROM attempts WHERE id = '%s';", attemptID)); number != "1" {
		t.Fatalf("%s attempt %s is number %q, want the first attempt", wrappedLaunchPrecondition, attemptID, number)
	}
	return attemptID, sessionID
}

// requireWrappedReconciling waits for the state the whole scenario rests
// on: the launch failed closed while the other process is there, with its
// claim still unsettled. Its timeout means the stub did not hold the
// window, not that the revisit rule is missing.
func requireWrappedReconciling(t *testing.T, fx *featureRun, sessionID string) {
	t.Helper()
	var sessionState, claim string
	if !waitUntilDeadline(featureRunTimeout, func() bool {
		sessionState, claim = fx.sessionState(t, sessionID), fx.claimState(t, sessionID)
		return sessionState == "reconciling" && claim == "exec_pending"
	}) {
		t.Fatalf("%s session %s is %q with its launch claim %q after %s; want reconciling with an exec_pending claim.\nsession transitions recorded:\n%s",
			wrappedLaunchPrecondition, sessionID, sessionState, claim, featureRunTimeout, sessionTransitionJournal(t, fx, sessionID))
	}
}

// requireWrappedGroup is the independent confirmation of WHY, from Herdr's
// own process_info: the claimed process matches the claim's executable
// identity and carries the run marker, and so does another member under a
// different pid. Members are matched by pid and marker, never by listing
// position.
func requireWrappedGroup(t *testing.T, fx *featureRun, sessionID string) {
	t.Helper()
	paneID := fx.requirePane(t, sessionID)
	claimPID, ok := fx.launchClaimPID(t, sessionID)
	if !ok || claimPID <= 0 {
		t.Fatalf("%s no launch claim pid recorded for session %s (ok=%v pid=%d)", wrappedLaunchPrecondition, sessionID, ok, claimPID)
	}
	var info spikeProcessInfo
	claimed, other := 0, 0
	if !waitUntilDeadline(featureRunTimeout, func() bool {
		info = fx.server.processInfo(t, paneID)
		claimed, other = 0, 0
		for _, fg := range info.ForegroundProcesses {
			if fg.Argv0 != "claude" || !strings.Contains(fg.Cmdline, fx.runID) {
				continue
			}
			if int(fg.PID) == claimPID {
				claimed++
				continue
			}
			other++
		}
		return claimed == 1 && other > 0
	}) {
		t.Fatalf("%s the foreground group holds %d matching members at the claim pid %d and %d under other pids after %s, want the claimed one plus at least one other; group: %+v",
			wrappedLaunchPrecondition, claimed, claimPID, other, featureRunTimeout, info.ForegroundProcesses)
	}
}

// requireRevisitedAndSettled is the decisive assertion. On timeout it
// prints what distinguishes "the session was never re-inspected" from "the
// run merely ran out of time": the claim state, the session state and
// every transition the session recorded.
func requireRevisitedAndSettled(t *testing.T, fx *featureRun, sessionID string) {
	t.Helper()
	var sessionState, claim string
	if !waitUntilDeadline(featureRunTimeout, func() bool {
		sessionState, claim = fx.sessionState(t, sessionID), fx.claimState(t, sessionID)
		return sessionState == "active" && claim == "execed"
	}) {
		t.Fatalf("%s after the other process left, session %s is %q with its launch claim %q after %s; want active with the claim execed.\nsession transitions recorded:\n%s",
			wrappedLaunchDecisive, sessionID, sessionState, claim, featureRunTimeout, sessionTransitionJournal(t, fx, sessionID))
	}
}

// sessionTransitionJournal renders every transition one session recorded,
// oldest first, for a failure message.
func sessionTransitionJournal(t *testing.T, fx *featureRun, sessionID string) string {
	t.Helper()
	return fx.scalar(t, fmt.Sprintf(
		"SELECT COALESCE(group_concat(line, char(10)), '(none recorded)') FROM (SELECT at || '  ' || from_state || ' -> ' || to_state || '  : ' || reason AS line FROM transitions WHERE entity_kind = 'session' AND entity_id = '%s' ORDER BY at, rowid);",
		sessionID))
}

// releaseWrappedLaunch returns the idempotent release of the other
// process: creating the gate file, which it polls for and exits on. Safe
// to call twice, since both the scenario and its cleanup call it.
func releaseWrappedLaunch(t *testing.T, gate string) func() {
	t.Helper()
	released := false
	return func() {
		if released {
			return
		}
		released = true
		if err := os.WriteFile(gate, nil, 0o600); err != nil {
			t.Errorf("release the wrapped launch: %v", err)
		}
	}
}

// reconcilingReason reads the reason of the newest recorded transition of
// one session into reconciling.
func reconcilingReason(t *testing.T, fx *featureRun, sessionID string) string {
	t.Helper()
	return fx.scalar(t, fmt.Sprintf(
		"SELECT reason FROM transitions WHERE entity_kind = 'session' AND entity_id = '%s' AND to_state = 'reconciling' ORDER BY at DESC, rowid DESC LIMIT 1;",
		sessionID))
}

// requireStatusAction asserts a real `hop status -run` renders the nested
// action line under sessionID's own session line.
func requireStatusAction(t *testing.T, fx *featureRun, sessionID string) {
	t.Helper()
	result := runHop(t, fx.env, fx.repo.Root, "status", "-C", fx.repo.Root, "-run", fx.runID)
	if result.ExitCode != 0 {
		t.Fatalf("hop status -run: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
	lines := strings.Split(result.Stdout, "\n")
	idx := slices.IndexFunc(lines, func(line string) bool {
		return strings.HasPrefix(strings.TrimSpace(line), "session "+sessionID+":")
	})
	if idx < 0 || idx+1 >= len(lines) {
		t.Errorf("hop status -run has no session line for %s with a line under it; got:\n%s", sessionID, result.Stdout)
		return
	}
	// Reported, never fatal: the rendering is one fact this scenario
	// pins, and a run that never settles afterwards is the other. A base
	// run must reach both.
	action := strings.TrimSpace(lines[idx+1])
	if !strings.HasPrefix(action, "action:") || !strings.Contains(action, "carries the launch identity") {
		t.Errorf("the line under session %s is %q, want its launch-corroboration action; got:\n%s", sessionID, action, result.Stdout)
	}
}
