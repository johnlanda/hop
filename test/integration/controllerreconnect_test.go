package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// resumeRaceGateScript is the shell wrapper every gated `hop resume`
// invocation execs through. It first touches its own ready-marker file
// ($2) — proof, pollable from the test's own goroutine, that this shell
// has reached the barrier statement next — then blocks on a plain
// redirection open of gatePath ($1): `exec 3<path` is a shell BUILTIN
// (no fork), so unlike `cat path`, there is no child process whose own
// fork/exec latency could widen the window between the ready marker and
// the actual blocking open. Once releaseCheckGate's writer opens and
// closes gatePath, every already-blocked reader — both gated invocations
// alike — unblocks in the same instant (a real FIFO rendezvous, verified
// against this exact mechanism: a busy-polling wait for both markers
// reintroduces the fork-latency race by starving the shells of CPU, so
// the marker poll below uses waitUntil's own sleeping loop, never a tight
// spin). "$@" after `shift 2` is the hop binary path plus its own
// arguments, exec'd in place so the wrapper never lingers as an extra
// process the harness would need to reap separately from the real
// controller/loser it fronts.
const resumeRaceGateScript = `: > "$2"; exec 3<"$1"; shift 2; exec "$@"`

// startGatedResume starts `hop <args...>` (a resume invocation) as its own
// anchored process group, blocked on gatePath until releaseCheckGate opens
// it, capturing stdout/stderr to <name>-stdout.log/<name>-stderr.log under
// artifacts exactly as startHopController's do, and returns the ready-marker
// path the caller must observe (via waitUntil, never a fixed sleep) before
// releasing the gate. Cleanup mirrors startHopController's registered
// teardown; safe whether this process is later found to be the race's
// winner (a live controller loop) or its loser (already exited on its own).
func startGatedResume(t *testing.T, s *testServer, artifacts *artifactDir, stateDir, name, gatePath string, args ...string) (sp *serverProcess, readyPath string) {
	t.Helper()
	hopPath := buildHopBinary(t)
	stdout := artifacts.create(t, name+"-stdout.log")
	stderr := artifacts.create(t, name+"-stderr.log")
	readyPath = filepath.Join(artifacts.path, name+"-ready")
	shArgs := append([]string{"-c", resumeRaceGateScript, "sh", gatePath, readyPath, hopPath}, args...)
	cmd := exec.CommandContext(context.Background(), "sh", shArgs...) //nolint:gosec // G204: a fixed shell wrapper gating this suite's own hop binary build; args are chosen by the calling test.
	cmd.Env = s.hopEnviron(stateDir)
	cmd.Dir = stateDir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	sp = startAnchoredLeader(t, cmd, stdout, stderr)
	t.Cleanup(func() {
		if sp.takeTeardown() {
			if err := sp.retireGroup(); err != nil {
				t.Errorf("retire gated resume %q group on cleanup: %v", name, err)
			}
			closeLogs(t, sp.stdout, sp.stderr)
		}
	})
	return sp, readyPath
}

// readNamedLog reads one captured log file by its exact name (unlike
// readControllerLog, which always appends "-stdout.log") — used here for a
// gated resume process's own "<name>-stderr.log".
func readNamedLog(t *testing.T, artifacts *artifactDir, fileName string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(artifacts.path, fileName)) //nolint:gosec // G304: a path this test constructed itself, under its own artifact directory.
	if err != nil {
		t.Fatalf("read %s: %v", fileName, err)
	}
	return string(content)
}

// managerSessionCountSampler polls, in its own goroutine, the count of
// non-terminal manager-role sessions for one run — the exit-criterion
// scenario's own requirement to sample throughout the lease race rather
// than only check once at the end, when a transient double-claim could
// already have settled back to one by the time a single poll looked. It
// never calls testing.T from the sampling goroutine (T.Fatal is only safe
// from the test's own goroutine); Stop joins the goroutine and hands its
// accumulated result back for the caller to assert on directly.
type managerSessionCountSampler struct {
	stop chan struct{}
	done chan struct{}
	mu   sync.Mutex
	max  int
	err  error
}

// startManagerSessionCountSampler begins sampling immediately, at a short,
// fixed interval well under the lease race's own expected duration (a
// local process start plus one sqlite transaction).
func startManagerSessionCountSampler(dbPath, runID string) *managerSessionCountSampler {
	s := &managerSessionCountSampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		query := fmt.Sprintf("SELECT count(*) FROM sessions WHERE run_id = '%s' AND role = 'manager' AND state NOT IN ('lost','terminated');", runID)
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
				out, err := exec.CommandContext(ctx, "sqlite3", dbPath, query).CombinedOutput() //nolint:gosec // G204: fixed sqlite3 invocation against this test's own database path and a query composed from a run id this test minted.
				cancel()
				s.mu.Lock()
				switch {
				case err != nil:
					if s.err == nil {
						s.err = fmt.Errorf("sample manager session count: %w: %s", err, out)
					}
				default:
					if n, perr := strconv.Atoi(strings.TrimSpace(string(out))); perr == nil && n > s.max {
						s.max = n
					}
				}
				s.mu.Unlock()
			}
		}
	}()
	return s
}

// Stop ends sampling and returns the maximum non-terminal manager-session
// count observed across every sample, plus the first sampling error (if
// any query itself failed to run — never a manager-session-count finding).
func (s *managerSessionCountSampler) Stop() (maxCount int, err error) {
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.max, s.err
}

// TestRealProcessControllerReconnectNoSecondClaimant is design section 11
// scenario 6 (the section 1 exit criterion): the controller is killed
// through its own owned process handle (killControllerLeader signals the
// *exec.Cmd this harness itself started and Waits on — never a pid this
// test only observed via pane inspection), the abandoned lease is waited
// out, and two `hop resume` processes are then released together by a
// FIFO rendezvous (never a sleep) to race the lease's compare-and-swap.
// Exactly one must win: the loser reports the held lease with its exact
// production grammar and hop resume's real exit(1), while the winner warm-
// reattaches the still-live manager and worker sessions under the one
// corroboration predicate — no relaunch, no new session rows — and the
// store never shows a second non-terminal manager session at any sampled
// instant across the whole race, not merely at the end.
func TestRealProcessControllerReconnectNoSecondClaimant(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	worker := buildFixtureWorker(t, artifacts)
	installFixtureWorkerAsClaudeStub(t, server, worker)
	server.start(t)

	scratchDir := artifacts.dir(t, "fixture-scratch")
	repo := newFeatureFixtureRepo(t, artifacts, server, "repo", featureFixtureOptions{
		ScratchDir: scratchDir, ReviewerBehavior: "reviewer-approve",
		MaxWorkers: 1, RetryLimit: 3, MessageWaitTimeout: "3s", MessageAttentionAfter: "30s",
	})

	brief := fixtureManagerBrief(scratchDir,
		[]fixtureManagerTask{{Label: "t1", Title: "Implement t1", Behavior: "worker-hold"}},
		[]fixtureManagerAnswer{{Match: fixtureHoldMarker, Action: "relay"}},
		"",
	)
	fx := startFeatureRun(t, artifacts, server, repo, scratchDir, brief)

	// Held preconditions: the manager is up, and t1's worker is launched
	// and blocked at its own message barrier (never released here) so its
	// session, incarnation and claimed pid are all stable across the
	// controller kill and the whole resume race below — nothing to race
	// against but the lease CAS itself.
	managerID := fx.managerSessionID(t)
	t1 := fx.requireTaskBySeq(t, 1)
	fx.requireTaskState(t, t1, "active")
	attemptID, attemptNumber := fx.currentAttempt(t, t1)
	if attemptNumber != 1 {
		t.Fatalf("first attempt number = %d, want 1", attemptNumber)
	}
	workerSessionID := fx.sessionForAttempt(t, attemptID)
	fx.requireSessionState(t, workerSessionID, "active")
	if state := fx.claimState(t, workerSessionID); state != "execed" {
		t.Fatalf("worker's launch claim state = %q before the kill, want execed (settled)", state)
	}
	managerPID, ok := fx.launchClaimPID(t, managerID)
	if !ok || managerPID <= 0 {
		t.Fatalf("no settled launch claim pid recorded for manager session %s before the kill", managerID)
	}
	workerPID, ok := fx.launchClaimPID(t, workerSessionID)
	if !ok || workerPID <= 0 {
		t.Fatalf("no settled launch claim pid recorded for worker session %s before the kill", workerSessionID)
	}

	// Kill the controller through its own owned process handle: never a
	// pid this test only observed via pane inspection (killControllerLeader
	// signals sp.leaderCmd.Process, the *exec.Cmd this harness itself
	// started and Waits on). Wait out the abandoned lease exactly as every
	// other crash-recovery scenario in this package must.
	killControllerLeader(t, fx.controller)
	waitForLeaseExpiry(t, fx.dbPath(), fx.runID)

	// Two `hop resume` invocations, gated on the SAME FIFO, so neither can
	// begin its own process lifetime meaningfully ahead of the other.
	gatePath := filepath.Join(artifacts.path, "resume-race.fifo")
	if err := syscall.Mkfifo(gatePath, 0o600); err != nil {
		t.Fatalf("create resume race fifo %s: %v", gatePath, err)
	}
	raceA, readyA := startGatedResume(t, server, artifacts, fx.stateDir, "resume-a", gatePath, "resume", "-C", fx.repo.Root, fx.runID)
	raceB, readyB := startGatedResume(t, server, artifacts, fx.stateDir, "resume-b", gatePath, "resume", "-C", fx.repo.Root, fx.runID)

	sampler := startManagerSessionCountSampler(fx.dbPath(), fx.runID)

	// Wait, via waitUntil's own bounded sleeping poll (never a busy spin,
	// which would starve the two shells and reopen the very race this
	// barrier exists to close), until BOTH gated invocations have reached
	// their blocking gate open — then releaseCheckGate (checkdeath_test.go)
	// opens gatePath for writing and closes it at once: both readers'
	// blocking opens resolve together on the same underlying FIFO
	// rendezvous, releasing raceA and raceB to actually start within the
	// same instant.
	if !waitUntil(func() bool { return pathExists(t, readyA) && pathExists(t, readyB) }) {
		t.Fatalf("gated resume processes never reached their barrier (ready markers %s, %s)", readyA, readyB)
	}
	releaseCheckGate(t, gatePath)

	// Exactly one of the two exits promptly (the lease CAS loser); the
	// other must still be running when that happens (a live controller
	// loop, never a second exit racing the first).
	var loser, winner *serverProcess
	var loserName, winnerName string
	select {
	case <-raceA.leaderExited:
		loser, loserName = raceA, "resume-a"
		winner, winnerName = raceB, "resume-b"
	case <-raceB.leaderExited:
		loser, loserName = raceB, "resume-b"
		winner, winnerName = raceA, "resume-a"
	case <-time.After(30 * time.Second):
		maxCount, sampleErr := sampler.Stop()
		t.Fatalf("neither gated hop resume process exited within 30s (max sampled manager-session count %d, sample err %v)", maxCount, sampleErr)
	}
	select {
	case <-winner.leaderExited:
		maxCount, sampleErr := sampler.Stop()
		t.Fatalf("both gated hop resume processes exited; want exactly one loser and one still-running winner (max sampled manager-session count %d, sample err %v)", maxCount, sampleErr)
	default:
	}

	// The loser's exact production grammar and exit code (cmd/hop/
	// resumecmd.go's runResumeFeature: AcquireLease's ErrLeaseHeld wrapped
	// and printed verbatim to stderr, exit(1)).
	loserStatus := loser.leaderCmd.ProcessState
	if loserStatus == nil || !loserStatus.Exited() || loserStatus.ExitCode() != 1 {
		t.Errorf("%s exit status = %v, want a clean exit(1)", loserName, loserStatus)
	}
	loserStderr := readNamedLog(t, artifacts, loserName+"-stderr.log")
	const wantLoserLine = "hop resume: app: acquire lease: app: lease is held"
	if !strings.Contains(loserStderr, wantLoserLine) {
		t.Errorf("%s stderr = %q, want it to contain %q", loserName, loserStderr, wantLoserLine)
	}

	// The winner warm-reattaches the manager and the held worker under the
	// one corroboration predicate: "resumed" outcome, both sessions "warm".
	winnerStdout := waitForControllerLog(t, artifacts, winnerName, "resume resumed:")
	if !strings.Contains(winnerStdout, "resume resumed:") {
		t.Fatalf("%s stdout never reported \"resume resumed:\"; got:\n%s", winnerName, winnerStdout)
	}
	for _, want := range []string{
		fmt.Sprintf("session %s (manager): warm", managerID),
		fmt.Sprintf("session %s (implementer): warm", workerSessionID),
	} {
		if !strings.Contains(winnerStdout, want) {
			t.Errorf("%s stdout missing %q; got:\n%s", winnerName, want, winnerStdout)
		}
	}

	// Now that the winner has reported, the race window is over: stop
	// sampling and require that no sample, at any point, ever saw more
	// than one non-terminal manager session for this run.
	maxManagerSessions, sampleErr := sampler.Stop()
	if sampleErr != nil {
		t.Fatalf("manager session sampler: %v", sampleErr)
	}
	if maxManagerSessions > 1 {
		t.Errorf("max sampled non-terminal manager-session count during the resume race = %d, want at most 1 at every sampled instant", maxManagerSessions)
	}
	if maxManagerSessions == 0 {
		t.Error("manager session sampler never observed even one non-terminal manager session; the sampler itself is not exercising anything")
	}

	// No relaunch happened: the same session rows, the same claimed pids —
	// warm reattach rebinds, it never mints a new session or incarnation.
	if got := fx.managerSessionID(t); got != managerID {
		t.Errorf("manager session after the winning resume = %s, want unchanged %s (warm reattach, never a relaunch)", got, managerID)
	}
	if got := fx.sessionForAttempt(t, attemptID); got != workerSessionID {
		t.Errorf("worker session after the winning resume = %s, want unchanged %s (warm reattach, never a relaunch)", got, workerSessionID)
	}
	if got, ok := fx.launchClaimPID(t, managerID); !ok || got != managerPID {
		t.Errorf("manager launch claim pid after the winning resume = %d (ok=%v), want unchanged %d", got, ok, managerPID)
	}
	if got, ok := fx.launchClaimPID(t, workerSessionID); !ok || got != workerPID {
		t.Errorf("worker launch claim pid after the winning resume = %d (ok=%v), want unchanged %d", got, ok, workerPID)
	}

	managerSessionCount := fx.scalar(t, fmt.Sprintf("SELECT count(*) FROM sessions WHERE run_id = '%s' AND role = 'manager';", fx.runID))
	if managerSessionCount != "1" {
		t.Errorf("total manager session row count for run %s = %s, want exactly 1 (the original, warm-reattached, never a second)", fx.runID, managerSessionCount)
	}

	// The winner is now this run's live controller: hand it to fx so the
	// remaining flow (releasing the barrier, completion) drives against
	// the process actually holding the lease.
	fx.controller, fx.controllerName = winner, winnerName

	// Release the held worker's own barrier and let the run complete
	// normally, proving the reattached controller is not merely alive but
	// fully driving the run onward.
	questionID := fx.relayedQuestionFor(t, workerSessionID)
	fx.answerHuman(t, questionID, "release after the reconnect race")
	fx.requireSessionState(t, workerSessionID, "terminated")
	fx.requireTaskState(t, t1, "integrated")
	reviewTaskID := fx.requireReviewTask(t)
	fx.requireTaskState(t, reviewTaskID, "completed")
	fx.requireRunState(t, "completed")
}
