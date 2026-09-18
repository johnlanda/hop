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
	worker := buildFixtureWorker(t)
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
	// immediately before acking) m1 specifically.
	observationPath := filepath.Join(scratchDir, "fetch-crash-observed-"+attempt1ID+".txt")
	obs := waitForObservation(t, observationPath)
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

	// Both the worker and the controller must die before any
	// reconciliation, and the CONTROLLER dies FIRST: killControllerLeader
	// signals only fx.controller's own leaderCmd, a process this test
	// itself started and owns (never reaped by anything else), so this
	// is safe regardless of ordering. Killing it before the self-kill
	// below closes the window where a still-live controller's own next
	// scheduling pass could run RetireSettledSessions/observeWorkerExit
	// against the about-to-die worker and reconcile it as an ordinary
	// interruption (a NEW attempt/worktree) instead of the same-attempt
	// cold relaunch this trace needs.
	killControllerLeader(t, fx.controller)
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

	// While the worker is dead: hop status is a one-shot CLI invocation
	// independent of any live controller, so it is checked here, before
	// the lease even expires. Poll until the in-flight age has passed the
	// frozen 2s attention threshold, matching the exact section 7 line --
	// address, in-flight message id and queued count -- with both ages
	// parsed back and checked against the threshold and real elapsed
	// wall-clock time, never a substring/Contains match.
	const attentionThreshold = 2 * time.Second
	wantAddress := grammarTaskAddress(t1, "t1")

	// m1's delivered_at (its ONE delivery row so far, to session1ID) and
	// m2's created_at, read directly from the store rather than
	// approximated from a wall-clock snapshot taken near the fetch:
	// mailboxStatuses (internal/adapters/sqlite/workflow_read.go) computes
	// both ages as exactly `now.Sub(...)` against these same two columns,
	// and GrammarAttentionLine renders them via bare time.Duration.String()
	// with no rounding or truncation anywhere in the read path (confirmed:
	// no Round/Truncate call in internal/app/grammar.go or the sqlite
	// read path) -- so bracketing each hop status call's own start/end
	// against these exact values needs no fudge allowance at all: the
	// CLI's own internal "now" necessarily falls inside [callStart,
	// callEnd] on the same machine clock.
	m1DeliveredAt := fx.messageDeliveredAt(t, m1, session1ID)
	m2CreatedAt := fx.messageCreatedAt(t, m2)
	requireAttentionAgesBracketed := func(inFlightAge, oldestQueuedAge time.Duration, callStart, callEnd time.Time) {
		t.Helper()
		if lower, upper := callStart.Sub(m1DeliveredAt), callEnd.Sub(m1DeliveredAt); inFlightAge < lower || inFlightAge > upper {
			t.Errorf("attention line in-flight age %s outside [%s, %s] bracketed around this hop status call against m1's own delivered_at %s", inFlightAge, lower, upper, m1DeliveredAt)
		}
		if lower, upper := callStart.Sub(m2CreatedAt), callEnd.Sub(m2CreatedAt); oldestQueuedAge < lower || oldestQueuedAge > upper {
			t.Errorf("attention line oldest-queued age %s outside [%s, %s] bracketed around this hop status call against m2's own created_at %s", oldestQueuedAge, lower, upper, m2CreatedAt)
		}
	}

	var inFlightAge, oldestQueuedAge time.Duration
	if !waitUntilDeadlineWithInterval(featureRunTimeout, attentionPollInterval, func() bool {
		callStart := time.Now()
		out := runHop(t, fx.env, repo.Root, "status", "-C", repo.Root, "-run", fx.runID)
		callEnd := time.Now()
		if out.ExitCode != 0 {
			return false
		}
		age1, age2, ok := attentionLineBothClausesExact(firstMatchingLine(out.Stdout, "attention: messages pending for "+wantAddress+":"), wantAddress, m1, 1)
		if !ok {
			return false
		}
		requireAttentionAgesBracketed(age1, age2, callStart, callEnd)
		inFlightAge, oldestQueuedAge = age1, age2
		return inFlightAge > attentionThreshold
	}) {
		t.Fatalf("attention line for %s never reported an in-flight age past the %s threshold within %s", wantAddress, attentionThreshold, featureRunTimeout)
	}
	if inFlightAge <= attentionThreshold {
		t.Errorf("attention line in-flight age %s does not exceed the %s threshold it was polled to satisfy", inFlightAge, attentionThreshold)
	}
	_ = oldestQueuedAge

	callStart := time.Now()
	finalStatus := runHop(t, fx.env, repo.Root, "status", "-C", repo.Root, "-run", fx.runID)
	callEnd := time.Now()
	if finalStatus.ExitCode != 0 {
		t.Fatalf("hop status -run: exit=%d stdout=%q stderr=%q", finalStatus.ExitCode, finalStatus.Stdout, finalStatus.Stderr)
	}
	finalInFlightAge, finalOldestAge := requireAttentionLineBothClausesExact(t, finalStatus.Stdout, wantAddress, m1, 1)
	requireAttentionAgesBracketed(finalInFlightAge, finalOldestAge, callStart, callEnd)
	if finalInFlightAge <= attentionThreshold {
		t.Errorf("final attention line in-flight age %s does not exceed the %s threshold", finalInFlightAge, attentionThreshold)
	}
	if finalOldestAge <= 0 {
		t.Errorf("final attention line oldest-queued age %s is not plausible", finalOldestAge)
	}
	// The run-summary state line's attention marker, matched EXACTLY:
	// cmd/hop/statuscmd.go's renderRunDetail renders it as
	// "  state:         " + state + listingMarkers(...), and
	// listingMarkers appends "stop requested"/"reconciling"/
	// app.GrammarAttentionMarker in that fixed order, joined ", " inside
	// one parenthesized suffix -- neither of the first two markers
	// applies here (no stop requested, and mailboxStatuses/messaging
	// have nothing to do with the unrelated `operations` reconciling
	// state), so the whole line is pinned, not just a Contains on the
	// marker text.
	wantStateLine := "  state:         running (blocked, needs attention)"
	var gotStateLine string
	for _, line := range strings.Split(finalStatus.Stdout, "\n") {
		if strings.HasPrefix(line, "  state:") {
			gotStateLine = line
			break
		}
	}
	if gotStateLine != wantStateLine {
		t.Errorf("hop status -run state line = %q, want %q; full output:\n%s", gotStateLine, wantStateLine, finalStatus.Stdout)
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

	// The re-serve must exist BEFORE the stale ack is ever issued --
	// trace 3's ambiguity is the old incarnation's late ack refused stale
	// WHILE a second delivery row exists, so this waits for the resumed
	// worker's own re-fetch observation and the second delivery row
	// FIRST, and only afterward issues the stale ack.
	resumedObs := waitForObservation(t, observationPath)
	if resumedObs.Fields["id"] != m1 {
		t.Fatalf("resumed worker's fetch-crash observation names id=%s, want the re-served m1=%s", resumedObs.Fields["id"], m1)
	}
	if !messageDeliveredToSession(t, fx, m1, newSession1ID) {
		t.Fatalf("m1 was never delivered to the new session %s (the re-served \"second delivery row\")", newSession1ID)
	}
	if !messageDeliveredToSession(t, fx, m1, session1ID) {
		t.Fatalf("m1's original delivery row to session %s is gone; the re-serve must be a SECOND row, not a replacement", session1ID)
	}
	if n := fx.scalar(t, fmt.Sprintf("SELECT count(*) FROM message_deliveries WHERE message_id = '%s';", m1)); n != "2" {
		t.Errorf("total delivery-row count for m1 = %s, want exactly 2 (the original serve plus the one re-serve)", n)
	}

	// NOW the old (now superseded) incarnation's late ack, issued
	// directly: refused stale, with a receipt recorded, in the state
	// trace 3 actually needs -- the re-serve already exists. This matters
	// because AcceptAck checks "already acknowledged" before incarnation
	// currency (domain/run/message.go), so ordering decides stale vs.
	// duplicate; the release gate below still guarantees this runs before
	// the new incarnation's own ack.
	staleEnv := fx.sessionEnv(session1ID, oldIncarnationID)
	if fx.ackRefusalReceiptRecorded(t, m1, session1ID, oldIncarnationID) {
		t.Fatalf("a stale-ack refusal receipt for m1 claimed by the old incarnation %s already exists before the stale ack was ever issued", oldIncarnationID)
	}
	staleResult := runHop(t, staleEnv, repo.Root, "msg", "ack", m1)
	if staleResult.ExitCode != 1 {
		t.Fatalf("hop msg ack (stale incarnation) exit=%d, want 1; stdout=%q", staleResult.ExitCode, staleResult.Stdout)
	}
	if got := staleResult.FirstStdoutLine(); got != "refused: stale" {
		t.Errorf("hop msg ack (stale incarnation) first line = %q, want %q", got, "refused: stale")
	}
	if !fx.ackRefusalReceiptRecorded(t, m1, session1ID, oldIncarnationID) {
		t.Error("no msg-ack receipt row recorded for the stale-incarnation refusal, claimed by the old incarnation specifically")
	}
	// The stale receipt's own `at` must be LATER than the second
	// delivery's delivered_at -- the store-level fact that actually
	// proves the ordering this scenario claims, never merely that both
	// assertions happened to run in this order in Go.
	secondDeliveredAt := fx.messageDeliveredAt(t, m1, newSession1ID)
	staleReceiptAt := fx.messageReceiptAt(t, "msg-ack", m1, session1ID, oldIncarnationID, "refused")
	if !staleReceiptAt.After(secondDeliveredAt) {
		t.Errorf("stale-ack receipt at %s is not after m1's second delivery (to %s) at %s", staleReceiptAt, newSession1ID, secondDeliveredAt)
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
	newIncarnationID := fx.incarnationForSession(t, newSession1ID)
	if got := fx.scalar(t, fmt.Sprintf("SELECT session_id FROM message_acks WHERE message_id = '%s';", m1)); got != newSession1ID {
		t.Errorf("m1's ack session_id = %q, want the RELAUNCHED worker session %q", got, newSession1ID)
	}
	if got := fx.scalar(t, fmt.Sprintf("SELECT incarnation_id FROM message_acks WHERE message_id = '%s';", m1)); got != newIncarnationID {
		t.Errorf("m1's ack incarnation_id = %q, want the new incarnation %q", got, newIncarnationID)
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
	// acks it too, delivered to the same new session, and only after m1's
	// own ack settles (fetch always re-serves the in-flight message
	// first).
	if !waitUntilDeadline(featureRunTimeout, func() bool { return fx.messageAcked(t, m2) }) {
		t.Fatalf("m2 was never acknowledged within %s", featureRunTimeout)
	}
	if !messageDeliveredToSession(t, fx, m2, newSession1ID) {
		t.Errorf("m2 was never delivered to the new session %s", newSession1ID)
	}
	if n := fx.scalar(t, fmt.Sprintf("SELECT count(*) FROM message_deliveries WHERE message_id = '%s';", m2)); n != "1" {
		t.Errorf("total delivery-row count for m2 = %s, want exactly 1", n)
	}
	if m2DeliveredAt, m1AckedAt := fx.messageDeliveredAt(t, m2, newSession1ID), fx.messageAckedAt(t, m1); m2DeliveredAt.Before(m1AckedAt) {
		t.Errorf("m2's delivered_at %s is before m1's acked_at %s; fetch must re-serve the in-flight message first, so m2 can only be served once m1 is acked", m2DeliveredAt, m1AckedAt)
	}

	// The resumed worker pauses at its own opt-in presubmit gate, after
	// its drain (m1 and m2 both acked) and before its own submit -- so
	// the attention line's absence below is checked while newSession1ID
	// is still ACTIVE, pinning the observation to this run's own
	// mid-flight state. The line's own presence does not depend on
	// address liveness at all (hop status -run renders it for every
	// address with any unacknowledged message, regardless of whether the
	// address is live) -- only the run-summary's separate
	// "(blocked, needs attention)" marker and its "action:" line do --
	// so this is not needed to avoid a vacuous pass, but it is still
	// direct evidence that the drain, not the session's own later
	// retirement, is what cleared the line.
	presubmitObservedPath := filepath.Join(scratchDir, "fetch-crash-presubmit-observed-"+attempt1ID+".txt")
	waitForObservation(t, presubmitObservedPath)
	if state := fx.sessionState(t, newSession1ID); state != "active" {
		t.Fatalf("newSession1ID is %q immediately before the cleared-attention check, want active (the resumed worker's own opt-in presubmit gate should still be holding it)", state)
	}
	preSubmitStatus := runHop(t, fx.env, repo.Root, "status", "-C", repo.Root, "-run", fx.runID)
	if preSubmitStatus.ExitCode != 0 {
		t.Fatalf("hop status -run: exit=%d stdout=%q stderr=%q", preSubmitStatus.ExitCode, preSubmitStatus.Stdout, preSubmitStatus.Stderr)
	}
	if state := fx.sessionState(t, newSession1ID); state != "active" {
		t.Fatalf("newSession1ID is %q immediately after the cleared-attention check, want active (the check must observe the line's absence while this session is still live, not after it retired)", state)
	}
	requireNoAttentionLineForAddress(t, preSubmitStatus.Stdout, wantAddress)

	presubmitReleasePath := filepath.Join(scratchDir, "fetch-crash-presubmit-release-"+attempt1ID)
	presubmitTmp := presubmitReleasePath + ".tmp"
	if err := os.WriteFile(presubmitTmp, []byte("FIXTURE-RELEASE\n"), 0o600); err != nil {
		t.Fatalf("write fetch-crash presubmit release control file: %v", err)
	}
	if err := os.Rename(presubmitTmp, presubmitReleasePath); err != nil {
		t.Fatalf("rename fetch-crash presubmit release control file into place: %v", err)
	}

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
