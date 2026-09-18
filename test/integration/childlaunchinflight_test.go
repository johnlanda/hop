package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// childLaunchGateStubSource stands in for the implementer's resolved
// "claude" long enough to hold its launch claim exec_pending under a live,
// pid-matching pane: exec'd by hop launch in the claim's own pid, it
// immediately re-execs itself, in place, with its argv stripped to just its
// own path — carrying the claim's executable identity but no launch marker,
// so the corroboration predicate reads every inspection of it as unresolved
// rather than settled — then blocks reading a FIFO gate. Only once released
// does it exec the real fixture worker under the original, marker-bearing
// argv, in the same pid throughout. Every role other than the implementer
// execs the real worker immediately, so the manager's own launch is never
// held.
const childLaunchGateStubSource = `package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "child-launch-gate stub: "+format+"\n", args...)
	os.Exit(1)
}

// withPhase returns the environment with the stub's own phase set to phase
// (dropped entirely when phase is "") and, when argv is non-nil, the
// original launched argv carried across the exec that strips it — joined by
// newlines, since none of its elements can contain one.
func withPhase(phase string, argv []string) []string {
	out := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HOP_WEDGE_PHASE=") || strings.HasPrefix(kv, "HOP_WEDGE_ARGV=") {
			continue
		}
		out = append(out, kv)
	}
	if phase != "" {
		out = append(out, "HOP_WEDGE_PHASE="+phase)
	}
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

func main() {
	if os.Getenv("HOP_ROLE") != "implementer" {
		execWorker(os.Args, os.Environ())
		return
	}
	ready, gate := os.Getenv("HOP_WEDGE_READY"), os.Getenv("HOP_WEDGE_GATE")
	if ready == "" || gate == "" {
		fail("the ready and gate paths are required")
	}
	if os.Getenv("HOP_WEDGE_PHASE") != "gated" {
		// Strip this pid's own argv down to its executable identity alone,
		// before anything else can observe the launched argv it started
		// with: the fastest step this stub can take, minimizing the window
		// in which a live inspection could see the marker-bearing argv.
		if err := syscall.Exec(selfPath(), os.Args[:1], withPhase("gated", os.Args)); err != nil {
			fail("strip my own argv: %v", err)
		}
		return
	}
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		fail("announce readiness: %v", err)
	}
	f, err := os.Open(gate) // a FIFO: this blocks until a writer opens it.
	if err != nil {
		fail("open the gate: %v", err)
	}
	f.Close()
	execWorker(launchedArgv(), withPhase("", nil))
}
`

// buildChildLaunchGateStub compiles childLaunchGateStubSource for the
// calling test.
func buildChildLaunchGateStub(t *testing.T, artifacts *artifactDir) string {
	t.Helper()
	src := artifacts.dir(t, "child-launch-gate-src")
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module childlaunchgate\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(childLaunchGateStubSource), 0o600); err != nil {
		t.Fatal(err)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go tool is required to build the child-launch-gate stub: %v", err)
	}
	out := filepath.Join(artifacts.dir(t, "child-launch-gate-bin"), "stub")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, goBin, "build", "-o", out, ".") //nolint:gosec // G204: the go tool builds this test's own generated fixture module.
	build.Dir = src
	if combined, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("go build child-launch-gate stub: %v\n%s", buildErr, combined)
	}
	return out
}

// childLaunchInFlightDetail mirrors internal/app/usecase_featureresume.go's
// own literal: the disposition detail resume renders for a placed child
// launch its own controller loop still corroborates.
const childLaunchInFlightDetail = "launch claim not settled; the placed launch is in flight, and the controller loop corroborates it"

// requireChildLaunchTarget waits for taskID's reserved attempt and the
// session bound to it.
func requireChildLaunchTarget(t *testing.T, fx *featureRun, taskID string) (attemptID, sessionID string) {
	t.Helper()
	if !waitUntilDeadline(featureRunTimeout, func() bool {
		attemptID = fx.scalar(t, fmt.Sprintf("SELECT id FROM attempts WHERE task_id = '%s' ORDER BY number DESC LIMIT 1;", taskID))
		if attemptID == "" {
			return false
		}
		sessionID = fx.scalar(t, fmt.Sprintf("SELECT id FROM sessions WHERE attempt_id = '%s' ORDER BY rowid DESC LIMIT 1;", attemptID))
		return sessionID != ""
	}) {
		t.Fatalf("no attempt (%q) and session (%q) observed for task %s after %s", attemptID, sessionID, taskID, featureRunTimeout)
	}
	return attemptID, sessionID
}

// TestRealProcessChildLaunchInFlightAcrossResume proves resume's in-flight
// rule for a child session still `launching` (docs/plan/phase-3-design.md
// section 5, placedLaunchInFlight's `session.State == run.SessionLaunching`
// admission) against a real Herdr server and a real pane process: a launch
// claim written exec_pending, its controller then hard-killed while every
// inspection of its pane still reads unresolved — the stub's own argv
// carries no launch marker, so no pass can settle or reclassify it — with
// the pane still holding exactly the claimed process throughout. Without
// resume corroborating a placed-but-unsettled launch through its own pane
// inspection, nothing ever revisits this session again — its claim cannot
// advance on its own, and only a fresh hop resume round re-inspects the
// pane. hop resume must admit the run as resumed and running, reporting
// the child pending under its own in-flight detail, rather than leaving
// the round `reconciling` — the run left in `resuming`, the lease
// released, and hop resume exiting 1 without ever becoming a running
// controller. The pane, the claimed pid and the controller loop that
// finishes the launch afterward are all real throughout; nothing here is
// a fake or a table.
func TestRealProcessChildLaunchInFlightAcrossResume(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)

	worker := buildFixtureWorker(t, artifacts)
	realWorkerPath := filepath.Join(artifacts.dir(t, "real-worker-bin"), "claude-real")
	copyExecutable(t, worker, realWorkerPath)
	installFixtureWorkerAsClaudeStub(t, server, buildChildLaunchGateStub(t, artifacts))

	gates := artifacts.dir(t, "child-launch-gates")
	ready := filepath.Join(gates, "ready")
	gate := filepath.Join(gates, "release")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatalf("create child launch gate fifo %s: %v", gate, err)
	}
	server.extraEnv = append(server.extraEnv,
		"HOP_WEDGE_WORKER="+realWorkerPath,
		"HOP_WEDGE_READY="+ready,
		"HOP_WEDGE_GATE="+gate)
	gateReleased := false
	release := func() {
		if gateReleased {
			return
		}
		gateReleased = true
		releaseCheckGate(t, gate)
	}
	server.start(t)
	// Registered after the server starts: LIFO teardown then retires the
	// controller before it stops the server, so the pane — and with it the
	// FIFO's only reader — is still alive when this cleanup runs on every
	// failure path after the stub is held (has written ready and is
	// blocked on its own gate open) that never reaches the test's own
	// release() call. Gated on that same ready marker so a failure BEFORE
	// the stub ever reaches its gate open — nothing yet to rendezvous
	// with — never blocks this cleanup on an unreachable FIFO open. No pid
	// is ever signaled.
	t.Cleanup(func() {
		if pathExists(t, ready) {
			release()
		}
	})

	scratchDir := artifacts.dir(t, "fixture-scratch")
	repo := newFeatureFixtureRepo(t, artifacts, server, "child-launch-inflight-repo", featureFixtureOptions{
		ScratchDir: scratchDir, ReviewerBehavior: "reviewer-approve",
		MaxWorkers: 1, RetryLimit: 3, MessageWaitTimeout: "3s", MessageAttentionAfter: "30s",
	})
	brief := fixtureManagerBrief(scratchDir,
		[]fixtureManagerTask{{Label: "t1", Title: "Implement t1", Behavior: "worker-implement"}}, nil, "")
	fx := startFeatureRun(t, artifacts, server, repo, scratchDir, brief)

	t1 := fx.requireTaskBySeq(t, 1)
	attemptID, sessionID := requireChildLaunchTarget(t, fx, t1)

	if !waitUntilDeadline(featureRunTimeout, func() bool { return pathExists(t, ready) }) {
		t.Fatalf("the gated stub never reached its own held phase (ready marker %s)", ready)
	}
	// The precondition this whole scenario rests on: the claim is written,
	// the session is still launching, and the pane's own process is the
	// claimed pid — never settled, since the held stub's own argv carries no
	// launch marker.
	if state := fx.claimState(t, sessionID); state != "exec_pending" {
		t.Fatalf("launch claim state = %q once the stub announced readiness, want exec_pending", state)
	}
	if state := fx.sessionState(t, sessionID); state != "launching" {
		t.Fatalf("session state = %q once the stub announced readiness, want launching", state)
	}
	claimPID, ok := fx.launchClaimPID(t, sessionID)
	if !ok || claimPID <= 0 {
		t.Fatalf("no settled launch claim pid recorded for session %s", sessionID)
	}
	paneID := fx.requirePane(t, sessionID)
	info := fx.server.processInfo(t, paneID)
	if int(info.ShellPID) != claimPID {
		t.Fatalf("pane %s shell pid = %d, want the claimed pid %d", paneID, info.ShellPID, claimPID)
	}

	// The crash: the controller dies with the claim still exec_pending and
	// the pane still holding that very process — every inspection of it
	// since it stripped its own argv, live or resumed, reads unresolved (no
	// launch marker in its argv or cmdline), so no pass has settled or
	// reclassified it.
	killControllerLeader(t, fx.controller)
	waitForLeaseExpiry(t, fx.dbPath(), fx.runID)
	if state := fx.claimState(t, sessionID); state != "exec_pending" {
		t.Fatalf("launch claim state after the kill = %q, want it unchanged at exec_pending", state)
	}
	info = fx.server.processInfo(t, paneID)
	if int(info.ShellPID) != claimPID {
		t.Fatalf("pane %s shell pid after the kill = %d, want the still-claimed pid %d unchanged", paneID, info.ShellPID, claimPID)
	}
	if state := fx.sessionState(t, sessionID); state != "launching" {
		t.Fatalf("session state after the kill = %q, want it unchanged at launching: this scenario's whole point is the launching disjunct", state)
	}

	resumed := fx.server.startHopController(t, fx.stateDir, "resume", "resume", "-C", fx.repo.Root, fx.runID)
	fx.controller, fx.controllerName = resumed, "resume"

	// THE DECISIVE ASSERTION: hop resume admits the placed-but-unsettled
	// launch as in flight and hands the run back to a running controller
	// loop, rather than reporting the same session pending under a
	// different, uncorroborated detail while the round itself ends
	// reconciling. Without that admission this exact wait times out: resume
	// instead leaves the run in resuming, releases the lease and exits 1
	// immediately, never becoming a running controller at all.
	stdout := waitForControllerLog(t, artifacts, "resume", childLaunchInFlightDetail)
	if !strings.Contains(stdout, "resume resumed:") {
		t.Errorf("resume stdout never reports \"resumed\"; got:\n%s", stdout)
	}
	select {
	case <-resumed.leaderExited:
		t.Fatalf("hop resume exited instead of continuing as a running controller; stdout:\n%s", readControllerLog(t, artifacts, "resume"))
	default:
	}
	fx.requireRunState(t, "running")
	if state := fx.claimState(t, sessionID); state != "exec_pending" {
		t.Errorf("launch claim state right after resume = %q, want unchanged exec_pending: resume itself never settles it", state)
	}
	if state := fx.sessionState(t, sessionID); state != "launching" {
		t.Errorf("session state right after resume = %q, want unchanged launching: this scenario's whole point is the launching disjunct", state)
	}

	// Release the held process into the real fixture worker; the resumed
	// controller's own corroboration loop settles the claim it just
	// admitted, and the run completes exactly as an uninterrupted controller
	// would have finished it.
	release()
	if !waitUntilDeadline(featureRunTimeout, func() bool { return fx.claimState(t, sessionID) == "execed" }) {
		t.Fatalf("launch claim never settled after release; state = %q", fx.claimState(t, sessionID))
	}
	fx.requireSessionState(t, sessionID, "active")
	fx.requireAttemptState(t, attemptID, "running", "checking", "integrating", "completed")
	fx.requireTaskState(t, t1, "integrated")
	reviewTaskID := fx.requireReviewTask(t)
	fx.requireTaskState(t, reviewTaskID, "completed")
	fx.requireRunState(t, "completed")
}
