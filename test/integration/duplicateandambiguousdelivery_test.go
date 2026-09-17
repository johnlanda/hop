package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRealProcessDuplicateAndAmbiguousDelivery is design section 11
// scenario 3 (reference trace 3): a worker (task t1, behavior
// worker-fetch-crash) fetches message m1 (delivered) then is killed before
// it can ack -- the fixture's own deterministic kill point, mirroring
// RelayedQuestion's manager barrier: the worker dumps an observation
// naming m1 before blocking, this test waits for it, then self-kills.
// Both the worker and the controller are killed (the SAME topology
// resume_test.go's coldRelaunchAfterCrash uses for solo, generalized to
// feature mode's per-session --confirm-absent contract), so a live
// controller never gets the chance to reconcile the dead worker as an
// ordinary interruption (which would mint a NEW attempt/worktree, not the
// same-attempt cold relaunch this trace needs) -- and while the worker
// stays dead, hop status -run's section 7 attention line is asserted
// EXACTLY (address, in-flight message id and queued count, never a
// substring match) once the in-flight age has passed the frozen policy's
// low attention threshold. hop resume --confirm-absent=<worker session>
// cold-relaunches the worker: usecase_featureresume.go's
// coldRelaunchFeatureSession mints a brand-new successor session (design
// trace 3's own "new session, new incarnation, same task address",
// confirmed identical to the manager's own relaunch mechanics from
// source), re-serving the SAME m1 as a second delivery row. The prior
// (now superseded) incarnation's late ack is issued directly and refused
// stale, with a receipt recorded; the new incarnation is then released
// (a second, separate control-file gate distinct from the self-kill one,
// giving this stale-ack assertion a guaranteed happens-before relationship
// rather than racing the relaunched process's own speed) and acks
// successfully; a further ack of the same message from the old identity
// is now idempotently duplicate (AcceptAck checks "already acknowledged"
// before incarnation currency); the next fetch serves m2, which the
// resumed worker's own pre-submit drain acks too, clearing the attention
// line entirely. The task then integrates and the run completes normally.
func TestRealProcessDuplicateAndAmbiguousDelivery(t *testing.T) {
	start := time.Now()
	defer func() { t.Logf("TestRealProcessDuplicateAndAmbiguousDelivery wall time: %s", time.Since(start)) }()

	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	worker := buildFixtureWorker(t, artifacts)
	installFixtureWorkerAsClaudeStub(t, server, worker)
	server.start(t)

	scratchDir := artifacts.dir(t, "fixture-scratch")
	repo := newFeatureFixtureRepo(t, artifacts, server, "duplicate-ambiguous-delivery-repo", featureFixtureOptions{
		ScratchDir: scratchDir, ReviewerBehavior: "reviewer-approve",
		// The frozen policy's own low attention threshold (design section
		// 11 scenario 3's own instruction), so this scenario proves the
		// escalation without paying a real multi-minute wall-clock cost.
		MaxWorkers: 1, RetryLimit: 3, MessageWaitTimeout: "3s", MessageAttentionAfter: "2s",
	})

	brief := fixtureManagerBrief(scratchDir,
		[]fixtureManagerTask{{Label: "t1", Title: "Implement t1", Behavior: "worker-fetch-crash"}},
		nil, "",
	)
	fx := startFeatureRun(t, artifacts, server, repo, scratchDir, brief)

	t1 := fx.requireTaskBySeq(t, 1)
	managerEnv := fx.managerEnv(t)

	// Both messages queued up front: m1 is fetched almost immediately by
	// worker-fetch-crash's own first hop msg wait call; m2 stays queued
	// (fetch always re-serves the in-flight m1 first, design section 7)
	// for the entire "worker dead" window, so its own age is what proves
	// the queued clause and the eventual attention escalation together
	// with m1's in-flight age.
	m1Path := filepath.Join(scratchDir, "m1-body.md")
	if err := os.WriteFile(m1Path, []byte("manager info m1 for t1\n"), 0o600); err != nil {
		t.Fatalf("write m1 body: %v", err)
	}
	m1Result := runManagerVerb(t, managerEnv, repo.Root, "msg", "send", "--to", taskAddress(t1), "--kind", "info", "--file", m1Path, "--request-id", newSpikeUUID(t))
	if m1Result.ExitCode != 0 {
		t.Fatalf("hop msg send (m1): exit=%d stdout=%q stderr=%q", m1Result.ExitCode, m1Result.Stdout, m1Result.Stderr)
	}
	m1 := parseSentID(t, m1Result.Stdout)

	m2Path := filepath.Join(scratchDir, "m2-body.md")
	if err := os.WriteFile(m2Path, []byte("manager info m2 for t1\n"), 0o600); err != nil {
		t.Fatalf("write m2 body: %v", err)
	}
	m2Result := runManagerVerb(t, managerEnv, repo.Root, "msg", "send", "--to", taskAddress(t1), "--kind", "info", "--file", m2Path, "--request-id", newSpikeUUID(t))
	if m2Result.ExitCode != 0 {
		t.Fatalf("hop msg send (m2): exit=%d stdout=%q stderr=%q", m2Result.ExitCode, m2Result.Stdout, m2Result.Stderr)
	}
	m2 := parseSentID(t, m2Result.Stdout)

	fx.requireTaskState(t, t1, "active")
	attempt1ID, attempt1Number := fx.currentAttempt(t, t1)
	if attempt1Number != 1 {
		t.Fatalf("first attempt number = %d, want 1", attempt1Number)
	}
	session1ID := fx.sessionForAttempt(t, attempt1ID)
	fx.requireSessionState(t, session1ID, "active")
	if state := fx.claimState(t, session1ID); state != "execed" {
		t.Fatalf("attempt 1's launch claim state = %q before the kill, want execed (settled)", state)
	}
	oldIncarnationID := fx.incarnationForSession(t, session1ID)

	// The deterministic kill point: wait for the worker's own barrier
	// observation naming m1, proving it has fetched (and is now blocked
	// immediately before acking) m1 specifically. deliveredApprox is a
	// real wall-clock reference point at or after m1's own delivery
	// (design section 7's in-flight age is computed from the delivery
	// row's own timestamp), used below as an upper bound on the parsed
	// in-flight age.
	observationPath := filepath.Join(scratchDir, "fetch-crash-observed-"+attempt1ID+".txt")
	obs := waitForObservation(t, observationPath)
	deliveredApprox := time.Now()
	if obs.Fields["id"] != m1 {
		t.Fatalf("worker's fetch-crash observation names id=%s, want m1=%s", obs.Fields["id"], m1)
	}
	if fx.messageAcked(t, m1) {
		t.Fatalf("m1 was acked before the self-kill; the deterministic kill point did not hold")
	}
	// The resumed incarnation writes to this SAME fixed path (keyed by
	// attempt id, shared across incarnations of one attempt, like the
	// self-kill control file). Removed here so the later wait for the
	// resumed incarnation's own observation cannot spuriously succeed
	// against this already-consumed file.
	if err := os.Remove(observationPath); err != nil {
		t.Fatalf("remove fetch-crash observation file before relaunch: %v", err)
	}

	fx.killSession(t, session1ID, attempt1ID)

	// killSession's own self-kill control file is keyed by ATTEMPT id
	// (fixtureworker_test.go's watchForSelfKill call in runWorker), never
	// removed by killSession itself -- harmless for every EXISTING
	// scenario, whose next attempt (a RETRY) always gets a fresh attempt
	// id of its own, but this scenario's cold relaunch deliberately
	// reuses the SAME attempt id. Left in place, the resumed incarnation's
	// own background watchForSelfKill goroutine would find this file
	// already present the instant it starts and self-kill itself again
	// before ever reaching its own fetch -- removed here, exactly the
	// "remove its own control file before hop resume" one-shot discipline
	// RelayedQuestion's manager barrier documents (there, a brand-new
	// session id makes this unnecessary; here, the session changes but
	// the attempt id, which this file is keyed by, does not).
	if err := os.Remove(filepath.Join(scratchDir, "self-kill-"+attempt1ID)); err != nil {
		t.Fatalf("remove self-kill control file before relaunch: %v", err)
	}

	// Both the worker and the controller are killed before any
	// reconciliation: a live controller would otherwise reconcile the
	// dead worker as an ordinary interruption (a NEW attempt/worktree),
	// never the same-attempt cold relaunch this trace needs.
	killControllerLeader(t, fx.controller)

	// While the worker is dead: hop status is a one-shot CLI invocation
	// independent of any live controller, so it is checked here, before
	// the lease even expires. Poll until the in-flight age has passed the
	// frozen 2s attention threshold, matching the exact section 7 line --
	// address, in-flight message id and queued count -- with both ages
	// parsed back and checked against the threshold and real elapsed
	// wall-clock time, never a substring/Contains match.
	const attentionThreshold = 2 * time.Second
	wantAddress := grammarTaskAddress(t1, "t1")
	var inFlightAge, oldestQueuedAge time.Duration
	if !waitUntilDeadlineWithInterval(featureRunTimeout, attentionPollInterval, func() bool {
		out := runHop(t, fx.env, repo.Root, "status", "-C", repo.Root, "-run", fx.runID)
		if out.ExitCode != 0 {
			return false
		}
		age1, age2, ok := attentionLineBothClausesExact(firstMatchingLine(out.Stdout, "attention: messages pending for "+wantAddress+":"), wantAddress, m1, 1)
		if !ok {
			return false
		}
		inFlightAge, oldestQueuedAge = age1, age2
		return inFlightAge > attentionThreshold
	}) {
		t.Fatalf("attention line for %s never reported an in-flight age past the %s threshold within %s", wantAddress, attentionThreshold, featureRunTimeout)
	}
	// inFlightAge must exceed the threshold (the wait condition above)
	// and can never exceed the real wall-clock time elapsed since m1 was
	// observed delivered, plus a small allowance for this poll's own CLI
	// round-trip latency.
	if inFlightAge <= attentionThreshold {
		t.Errorf("attention line in-flight age %s does not exceed the %s threshold it was polled to satisfy", inFlightAge, attentionThreshold)
	}
	if upperBound := time.Since(deliveredApprox) + 5*time.Second; inFlightAge > upperBound {
		t.Errorf("attention line in-flight age %s exceeds the real elapsed time since m1 was observed delivered (%s, +5s CLI-latency allowance)", inFlightAge, upperBound)
	}
	if oldestQueuedAge <= 0 || oldestQueuedAge > time.Since(deliveredApprox)+5*time.Second {
		t.Errorf("attention line oldest-queued age %s is not plausible relative to real elapsed wall-clock time", oldestQueuedAge)
	}
	finalStatus := runHop(t, fx.env, repo.Root, "status", "-C", repo.Root, "-run", fx.runID)
	if finalStatus.ExitCode != 0 {
		t.Fatalf("hop status -run: exit=%d stdout=%q stderr=%q", finalStatus.ExitCode, finalStatus.Stdout, finalStatus.Stderr)
	}
	finalInFlightAge, finalOldestAge := requireAttentionLineBothClausesExact(t, finalStatus.Stdout, wantAddress, m1, 1)
	if finalInFlightAge <= attentionThreshold {
		t.Errorf("final attention line in-flight age %s does not exceed the %s threshold", finalInFlightAge, attentionThreshold)
	}
	if finalOldestAge <= 0 {
		t.Errorf("final attention line oldest-queued age %s is not plausible", finalOldestAge)
	}
	if !strings.Contains(finalStatus.Stdout, "(blocked, needs attention)") {
		t.Errorf("hop status -run does not carry the run-summary attention marker once past the threshold; full output:\n%s", finalStatus.Stdout)
	}

	waitForLeaseExpiry(t, fx.dbPath(), fx.runID)
	resumed := fx.server.startHopController(t, fx.stateDir, "resume", "resume", "-C", fx.repo.Root, "--confirm-absent="+session1ID, fx.runID)
	waitForControllerLog(t, fx.artifacts, "resume", "session "+session1ID+" (implementer): relaunched")
	fx.controller, fx.controllerName = resumed, "resume"
	fx.requireRunState(t, "running")

	fx.requireSessionState(t, session1ID, "lost")
	newSession1ID := fx.sessionForAttempt(t, attempt1ID)
	if newSession1ID == session1ID {
		t.Fatalf("worker session after cold relaunch = %s, want a NEW successor session distinct from the original %s", newSession1ID, session1ID)
	}
	if got, want := fx.nativeSessionRef(t, newSession1ID), fx.nativeSessionRef(t, session1ID); got != want {
		t.Errorf("relaunched worker's native_session_ref = %q, want the SAME reference the prior session carried: %q", got, want)
	}

	// The old (now superseded) incarnation's late ack, issued directly:
	// refused stale, with a receipt recorded -- issued BEFORE the new
	// incarnation's own ack (below), which matters because AcceptAck
	// checks "already acknowledged" before incarnation currency (domain/
	// run/message.go), so ordering decides stale vs. duplicate.
	staleEnv := fx.sessionEnv(session1ID, oldIncarnationID)
	staleResult := runHop(t, staleEnv, repo.Root, "msg", "ack", m1)
	if staleResult.ExitCode != 1 {
		t.Fatalf("hop msg ack (stale incarnation) exit=%d, want 1; stdout=%q", staleResult.ExitCode, staleResult.Stdout)
	}
	if got := staleResult.FirstStdoutLine(); got != "refused: stale" {
		t.Errorf("hop msg ack (stale incarnation) first line = %q, want %q", got, "refused: stale")
	}
	if !fx.ackRefusalReceiptRecorded(t, m1, session1ID) {
		t.Error("no msg-ack receipt row recorded for the stale-incarnation refusal")
	}

	// The re-served delivery: the SAME m1 delivered a second time, to the
	// NEW session -- proven directly from the journal, and via the
	// resumed worker's own observation of the exact fetched id, before
	// releasing it to ack.
	resumedObs := waitForObservation(t, observationPath)
	if resumedObs.Fields["id"] != m1 {
		t.Fatalf("resumed worker's fetch-crash observation names id=%s, want the re-served m1=%s", resumedObs.Fields["id"], m1)
	}
	if !messageDeliveredToSession(t, fx, m1, newSession1ID) {
		t.Fatalf("m1 was never delivered to the new session %s (the re-served \"second delivery row\")", newSession1ID)
	}
	if n := fx.scalar(t, fmt.Sprintf("SELECT count(*) FROM message_deliveries WHERE message_id = '%s';", m1)); n != "2" {
		t.Errorf("total delivery-row count for m1 = %s, want exactly 2 (the original serve plus the one re-serve)", n)
	}

	// Release the resumed incarnation's own ack, deterministically after
	// the stale-ack assertion above.
	releasePath := filepath.Join(scratchDir, "fetch-crash-release-"+attempt1ID)
	tmp := releasePath + ".tmp"
	if err := os.WriteFile(tmp, []byte("FIXTURE-RELEASE\n"), 0o600); err != nil {
		t.Fatalf("write fetch-crash release control file: %v", err)
	}
	if err := os.Rename(tmp, releasePath); err != nil {
		t.Fatalf("rename fetch-crash release control file into place: %v", err)
	}

	if !waitUntilDeadline(featureRunTimeout, func() bool { return fx.messageAcked(t, m1) }) {
		t.Fatalf("m1 was never acknowledged by the relaunched worker within %s", featureRunTimeout)
	}
	if got := fx.scalar(t, fmt.Sprintf("SELECT session_id FROM message_acks WHERE message_id = '%s';", m1)); got != newSession1ID {
		t.Errorf("m1's ack session_id = %q, want the RELAUNCHED worker session %q", got, newSession1ID)
	}

	// A further ack of m1, from anyone, is now idempotently duplicate:
	// AcceptAck's "already acknowledged" check runs first and regardless
	// of incarnation, so even the old (still-stale) identity now observes
	// duplicate rather than stale.
	dupResult := runHop(t, staleEnv, repo.Root, "msg", "ack", m1)
	if dupResult.ExitCode != 0 {
		t.Fatalf("hop msg ack (duplicate) exit=%d, want 0; stdout=%q", dupResult.ExitCode, dupResult.Stdout)
	}
	if got := dupResult.FirstStdoutLine(); got != "duplicate "+m1 {
		t.Errorf("hop msg ack (duplicate) first line = %q, want %q", got, "duplicate "+m1)
	}

	// The next fetch serves m2: the resumed worker's own pre-submit drain
	// acks it too, delivered to the same new session.
	if !waitUntilDeadline(featureRunTimeout, func() bool { return fx.messageAcked(t, m2) }) {
		t.Fatalf("m2 was never acknowledged within %s", featureRunTimeout)
	}
	if !messageDeliveredToSession(t, fx, m2, newSession1ID) {
		t.Errorf("m2 was never delivered to the new session %s", newSession1ID)
	}

	// The attention condition has fully cleared: no line for this address
	// at all, checked against the FULL status output.
	if !waitUntilDeadlineWithInterval(featureRunTimeout, attentionPollInterval, func() bool {
		out := runHop(t, fx.env, repo.Root, "status", "-C", repo.Root, "-run", fx.runID)
		if out.ExitCode != 0 {
			return false
		}
		for _, line := range strings.Split(out.Stdout, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "attention: messages pending for "+wantAddress+":") {
				return false
			}
		}
		return true
	}) {
		t.Fatalf("attention line for %s never cleared within %s", wantAddress, featureRunTimeout)
	}
	clearedStatus := runHop(t, fx.env, repo.Root, "status", "-C", repo.Root, "-run", fx.runID)
	if clearedStatus.ExitCode != 0 {
		t.Fatalf("hop status -run: exit=%d stdout=%q stderr=%q", clearedStatus.ExitCode, clearedStatus.Stdout, clearedStatus.Stderr)
	}
	requireNoAttentionLineForAddress(t, clearedStatus.Stdout, wantAddress)

	fx.requireTaskState(t, t1, "integrated")
	fx.requireRunState(t, "completed")
}

// firstMatchingLine returns the first full line in output whose trimmed
// form has prefix, "" if none matches.
func firstMatchingLine(output, prefix string) string {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) {
			return trimmed
		}
	}
	return ""
}
