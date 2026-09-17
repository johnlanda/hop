package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRealProcessRelayedQuestion is design section 11 scenario 2 (reference
// trace 2), in two subtests sharing the same setup shape: a worker (task
// t1, behavior worker-hold) sends question q1 to the manager; the manager
// relays it to the human as q2 (--relay-of q1); the human answers via a
// direct hop answer (a2, accepted atomically with q2's own acknowledgement);
// the manager fetches a2 and forwards it as a1 (reply-to q1, destination
// derived as task:t1) under a request id it derives DETERMINISTICALLY from
// a2's own id (CLI-returned data, never persisted), then acks a2
// (forward-before-ack). Both subtests kill the ORIGINAL manager session at a
// deterministic point (an opt-in fixture barrier, never a race against the
// manager's own next poll tick) and cold-relaunch it via
// hop resume --confirm-absent=<manager session>: usecase_featureresume.go's
// coldRelaunchFeatureSession mints a brand-new SUCCESSOR session id bound to
// the SAME native_session_ref (confirmed from source, never assumed -- a
// manager relaunch never reuses the prior session id with a new
// incarnation), marking the prior session "lost" in the same transaction.
//
//   - PreForwardKill (design section 11's own wording): the barrier fires
//     BEFORE the forward, so the relaunched manager's incarnation performs
//     the ONLY forward this run ever makes. Asserts every envelope,
//     delivery, ack and receipt row the design's messaging layer promises.
//   - PostForwardPreAckKill (reference trace 2's idempotency case): the
//     barrier fires AFTER the forward is accepted and BEFORE the ack, so
//     the relaunched manager's own incarnation re-fetches the re-served a2
//     and redoes the forward call under the SAME recorded request id --
//     proving the redo is idempotent (a duplicate outcome, not a second
//     answer), never merely that the barrier never fires. Kept lean: it
//     asserts the idempotency facts specifically, not the full row set
//     PreForwardKill already covers.
//
// Never restarts the disposable server in either subtest: relaunch requires
// server continuity.
func TestRealProcessRelayedQuestion(t *testing.T) {
	t.Run("PreForwardKill", testRelayedQuestionPreForwardKill)
	t.Run("PostForwardPreAckKill", testRelayedQuestionPostForwardPreAckKill)
}

// testRelayedQuestionPreForwardKill is documented on TestRealProcessRelayedQuestion.
func testRelayedQuestionPreForwardKill(t *testing.T) {
	start := time.Now()
	defer func() {
		t.Logf("TestRealProcessRelayedQuestion/PreForwardKill wall time: %s", time.Since(start))
	}()

	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	worker := buildFixtureWorker(t, artifacts)
	installFixtureWorkerAsClaudeStub(t, server, worker)
	server.start(t)

	scratchDir := artifacts.dir(t, "fixture-scratch")
	// Enable the manager's opt-in PRE-forward barrier BEFORE the run ever
	// starts: created here, in the test's own scratch directory, so its
	// presence is unambiguous by the time the manager's first incarnation
	// ever reaches the "answer" case. No existing scenario creates this
	// file, so their own behavior is unaffected (TestFixtureManagerPreForwardBarrier
	// proves both paths in isolation).
	if err := os.WriteFile(filepath.Join(scratchDir, preForwardBarrierControlFile), []byte("enable\n"), 0o600); err != nil {
		t.Fatalf("write pre-forward barrier control file: %v", err)
	}

	repo := newFeatureFixtureRepo(t, artifacts, server, "relayed-question-repo", featureFixtureOptions{
		ScratchDir: scratchDir, ReviewerBehavior: "reviewer-approve",
		MaxWorkers: 1, RetryLimit: 3, MessageWaitTimeout: "3s", MessageAttentionAfter: "30s",
	})

	brief := fixtureManagerBrief(scratchDir,
		[]fixtureManagerTask{{Label: "t1", Title: "Implement t1", Behavior: "worker-hold"}},
		[]fixtureManagerAnswer{{Match: fixtureHoldMarker, Action: "relay"}},
		"",
	)
	fx := startFeatureRun(t, artifacts, server, repo, scratchDir, brief)

	t1 := fx.requireTaskBySeq(t, 1)
	fx.requireTaskState(t, t1, "active")
	attempt1ID, _ := fx.currentAttempt(t, t1)
	session1ID := fx.sessionForAttempt(t, attempt1ID)

	// Captured before any kill, while definitely still the live manager.
	managerSessionID := fx.managerSessionID(t)

	q1 := fx.sessionQuestionTo(t, session1ID, "manager")
	q2 := fx.relayedQuestionFor(t, session1ID)
	if got := fx.messageBodyContent(t, q2); got != fixtureHoldMarker+"\n" {
		t.Errorf("relayed question q2's body = %q, want the original marker unchanged: %q", got, fixtureHoldMarker+"\n")
	}

	const releaseText = "release the barrier: proceed with the fixture change\n"
	fx.answerHuman(t, q2, releaseText)

	a2 := ""
	if !waitUntilDeadline(featureRunTimeout, func() bool {
		a2 = fx.scalar(t, fmt.Sprintf("SELECT id FROM messages WHERE run_id = '%s' AND reply_to = '%s';", fx.runID, q2))
		return a2 != ""
	}) {
		t.Fatalf("human answer a2 (reply-to %s) was never accepted within %s", q2, featureRunTimeout)
	}

	// The deterministic kill point: wait for the ORIGINAL manager's own
	// pre-forward barrier observation naming a2, proving it has fetched
	// a2 and is now blocked immediately before forwarding it -- never a
	// race against the manager's own next hop msg wait poll tick.
	barrierObs := waitForObservation(t, filepath.Join(scratchDir, preForwardBarrierObservedFile))
	if barrierObs.Fields["id"] != a2 {
		t.Fatalf("pre-forward barrier observation names id=%s, want the accepted answer %s", barrierObs.Fields["id"], a2)
	}
	if want := sha256HexOf(releaseText); barrierObs.Fields["sha256"] != want {
		t.Errorf("pre-forward barrier observation sha256 = %q, want %q (sha256 of the exact human answer bytes)", barrierObs.Fields["sha256"], want)
	}

	// Kill the controller FIRST -- killControllerLeader signals only
	// fx.controller's own leaderCmd, a process this test started and owns
	// (never reaped by anything else), so this is safe regardless of
	// ordering -- THEN self-kill the manager. Closes the window where a
	// still-live controller's own next scheduling pass could reconcile
	// the about-to-die manager session before the deliberate cold
	// relaunch below ever runs.
	killControllerLeader(t, fx.controller)
	fx.killSession(t, managerSessionID, managerSessionID)

	waitForLeaseExpiry(t, fx.dbPath(), fx.runID)

	resumed := fx.server.startHopController(t, fx.stateDir, "resume", "resume", "-C", fx.repo.Root, "--confirm-absent="+managerSessionID, fx.runID)
	waitForControllerLog(t, fx.artifacts, "resume", "session "+managerSessionID+" (manager): relaunched")
	fx.controller, fx.controllerName = resumed, "resume"
	fx.requireRunState(t, "running")

	// The prior manager session is retired (usecase_featureresume.go's
	// coldRelaunchFeatureSession marks it lost in the SAME transaction
	// that creates its successor); the successor is a NEW session id
	// bound to the SAME native reference -- confirmed directly from
	// source, never assumed either way.
	fx.requireSessionState(t, managerSessionID, "lost")
	newManagerSessionID := fx.managerSessionID(t)
	if newManagerSessionID == managerSessionID {
		t.Fatalf("manager session after cold relaunch = %s, want a NEW successor session distinct from the original %s (a manager relaunch never reuses the prior session id)", newManagerSessionID, managerSessionID)
	}
	if got, want := fx.nativeSessionRef(t, newManagerSessionID), fx.nativeSessionRef(t, managerSessionID); got != want {
		t.Errorf("relaunched manager's native_session_ref = %q, want the SAME reference the prior session carried: %q (the conversation continues, it is never a fresh one)", got, want)
	}

	// The relaunched manager recovers using ONLY CLI-returned data (the
	// re-served a2's own origin field) and forwards idempotently under
	// its own recorded request id, then acks a2 -- forward-before-ack.
	a1 := ""
	if !waitUntilDeadline(featureRunTimeout, func() bool {
		a1 = fx.scalar(t, fmt.Sprintf("SELECT id FROM messages WHERE run_id = '%s' AND reply_to = '%s' AND kind = 'answer';", fx.runID, q1))
		return a1 != ""
	}) {
		t.Fatalf("forwarded answer a1 (reply-to %s) was never observed within %s", q1, featureRunTimeout)
	}
	// Exactly one answer replies to q1 -- an explicit count
	// query, never inferred from a scalar SELECT that would silently
	// return a newline-joined value if a second a1 ever existed.
	if n := fx.scalar(t, fmt.Sprintf("SELECT count(*) FROM messages WHERE run_id = '%s' AND kind = 'answer' AND reply_to = '%s';", fx.runID, q1)); n != "1" {
		t.Errorf("count(answers with reply_to=%s) = %s, want exactly 1", q1, n)
	}

	// The de-risking assertion: the accepted forward's sender is the
	// RELAUNCHED manager's own session, proving genuine recovery through
	// the resume path rather than the original manager (which is dead)
	// having somehow won a race.
	if got := fx.scalar(t, fmt.Sprintf("SELECT sender_session_id FROM messages WHERE id = '%s';", a1)); got != newManagerSessionID {
		t.Errorf("forwarded answer a1's sender_session_id = %q, want the RELAUNCHED manager session %q", got, newManagerSessionID)
	}
	if got, want := fx.scalar(t, fmt.Sprintf("SELECT recipient_address FROM messages WHERE id = '%s';", a1)), taskAddress(t1); got != want {
		t.Errorf("forwarded answer a1's recipient_address = %q, want %q (derived from q1's own sender)", got, want)
	}
	if got := fx.messageBodyContent(t, a1); got != releaseText {
		t.Errorf("forwarded answer a1's body = %q, want the human's own answer text byte-exact: %q", got, releaseText)
	}

	// a2 is now acked by the RELAUNCHED manager, never the original.
	if !waitUntilDeadline(featureRunTimeout, func() bool { return fx.messageAcked(t, a2) }) {
		t.Fatalf("human answer a2 was never acknowledged within %s", featureRunTimeout)
	}
	a2AckSession := fx.scalar(t, fmt.Sprintf("SELECT session_id FROM message_acks WHERE message_id = '%s';", a2))
	if a2AckSession != newManagerSessionID {
		t.Errorf("a2's ack session_id = %q, want the RELAUNCHED manager session %q", a2AckSession, newManagerSessionID)
	}

	// Forward-before-ack, proved directly from the journal:
	// messages.created_at and message_acks.acked_at are both written
	// through sqlite.go's own single formatTime/timeLayout pair over the
	// SAME s.now() clock, so a direct comparison across these two columns
	// is valid, not merely within one table.
	a1CreatedAt := fx.messageCreatedAt(t, a1)
	a2AckedAt := fx.messageAckedAt(t, a2)
	if a1CreatedAt.After(a2AckedAt) {
		t.Errorf("forward-before-ack violated: a1 created_at=%s is AFTER a2 acked_at=%s", a1CreatedAt, a2AckedAt)
	}

	// The worker (never killed, still polling its own barrier) fetches
	// a1, acks it and proceeds normally.
	fx.requireTaskState(t, t1, "integrated")
	fx.requireRunState(t, "completed")
	if !waitUntilDeadline(featureRunTimeout, func() bool { return fx.messageAcked(t, a1) }) {
		t.Fatalf("forwarded answer a1 was never acknowledged by the worker within %s", featureRunTimeout)
	}
	if got := fx.scalar(t, fmt.Sprintf("SELECT session_id FROM message_acks WHERE message_id = '%s';", a1)); got != session1ID {
		t.Errorf("a1's ack session_id = %q, want the worker's own session %q", got, session1ID)
	}

	// Every delivery this trace promises, asserted directly.
	// a2's TWO delivery rows, original manager then relaunched manager.
	if !messageDeliveredToSession(t, fx, a2, managerSessionID) {
		t.Errorf("a2 was never delivered to the original manager session %s", managerSessionID)
	}
	if !messageDeliveredToSession(t, fx, a2, newManagerSessionID) {
		t.Errorf("a2 was never delivered to the relaunched manager session %s", newManagerSessionID)
	}
	if n := fx.scalar(t, fmt.Sprintf("SELECT count(*) FROM message_deliveries WHERE message_id = '%s';", a2)); n != "2" {
		t.Errorf("total delivery-row count for a2 = %s, want exactly 2 (the original serve plus the one re-serve)", n)
	}
	if original, resumedAt := fx.messageDeliveredAt(t, a2, managerSessionID), fx.messageDeliveredAt(t, a2, newManagerSessionID); !resumedAt.After(original) {
		t.Errorf("a2's re-serve (delivered_at=%s) is not after its original delivery (delivered_at=%s)", resumedAt, original)
	}
	// q1's delivery to the original manager (the only manager session
	// that was ever live when the worker sent it) and its ack session.
	if !messageDeliveredToSession(t, fx, q1, managerSessionID) {
		t.Errorf("q1 was never delivered to the original manager session %s", managerSessionID)
	}
	if got := fx.scalar(t, fmt.Sprintf("SELECT session_id FROM message_acks WHERE message_id = '%s';", q1)); got != managerSessionID {
		t.Errorf("q1's ack session_id = %q, want the original manager session %q", got, managerSessionID)
	}
	// a1's delivery to the worker (already asserted acked above).
	if !messageDeliveredToSession(t, fx, a1, session1ID) {
		t.Errorf("a1 was never delivered to the worker's session %s", session1ID)
	}

	// A message_receipts row for every send and every ack this
	// trace promises.
	requireMessageSendReceiptAccepted(t, fx, q1)
	requireMessageSendReceiptAccepted(t, fx, q2)
	requireMessageSendReceiptAccepted(t, fx, a2)
	requireMessageSendReceiptAccepted(t, fx, a1)
	requireMessageAckReceiptAccepted(t, fx, q1)
	requireMessageAckReceiptAccepted(t, fx, q2)
	requireMessageAckReceiptAccepted(t, fx, a2)
	requireMessageAckReceiptAccepted(t, fx, a1)

	// Every hop is a journaled row: audit q1, q2, a1, a2 independently
	// through hop msg show (the read-only same-run envelope lookup, needs
	// no caller identity) as well as the direct store reads above --
	// GrammarMessageShowLine is fully determined by kind/from/to/reply-to/
	// relay-of/seq, so the WHOLE first line is compared, never a per-token
	// Contains.
	requireMsgShowLine(t, fx, q1, "question", session1ID, "manager", "", "")
	requireMsgShowLine(t, fx, q2, "question", managerSessionID, "human", "", q1)
	requireMsgShowLine(t, fx, a2, "answer", "human", "manager", q2, "")
	requireMsgShowLine(t, fx, a1, "answer", newManagerSessionID, taskAddress(t1), q1, "")
	for _, id := range []string{q1, q2, a1, a2} {
		requireMsgShowAcknowledgedLine(t, fx, id)
	}
}

// testRelayedQuestionPostForwardPreAckKill is documented on
// TestRealProcessRelayedQuestion: reference trace 2's idempotency case.
// Kept lean -- it proves the redo is idempotent specifically, relying on
// its PreForwardKill sibling for the full envelope/delivery/receipt row
// set, since both subtests share the identical setup and messaging shape
// up to the barrier.
func testRelayedQuestionPostForwardPreAckKill(t *testing.T) {
	start := time.Now()
	defer func() {
		t.Logf("TestRealProcessRelayedQuestion/PostForwardPreAckKill wall time: %s", time.Since(start))
	}()

	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	worker := buildFixtureWorker(t, artifacts)
	installFixtureWorkerAsClaudeStub(t, server, worker)
	server.start(t)

	scratchDir := artifacts.dir(t, "fixture-scratch")
	// Enable the manager's opt-in POST-forward barrier instead of the
	// pre-forward one: the manager forwards a2 as a1 FIRST, then
	// self-kills before its own ack.
	if err := os.WriteFile(filepath.Join(scratchDir, postForwardBarrierControlFile), []byte("enable\n"), 0o600); err != nil {
		t.Fatalf("write post-forward barrier control file: %v", err)
	}

	repo := newFeatureFixtureRepo(t, artifacts, server, "relayed-question-post-forward-repo", featureFixtureOptions{
		ScratchDir: scratchDir, ReviewerBehavior: "reviewer-approve",
		MaxWorkers: 1, RetryLimit: 3, MessageWaitTimeout: "3s", MessageAttentionAfter: "30s",
	})

	brief := fixtureManagerBrief(scratchDir,
		[]fixtureManagerTask{{Label: "t1", Title: "Implement t1", Behavior: "worker-hold"}},
		[]fixtureManagerAnswer{{Match: fixtureHoldMarker, Action: "relay"}},
		"",
	)
	fx := startFeatureRun(t, artifacts, server, repo, scratchDir, brief)

	t1 := fx.requireTaskBySeq(t, 1)
	fx.requireTaskState(t, t1, "active")
	attempt1ID, _ := fx.currentAttempt(t, t1)
	session1ID := fx.sessionForAttempt(t, attempt1ID)

	managerSessionID := fx.managerSessionID(t)

	q1 := fx.sessionQuestionTo(t, session1ID, "manager")
	q2 := fx.relayedQuestionFor(t, session1ID)

	const releaseText = "release the barrier: proceed with the fixture change (post-forward variant)\n"
	fx.answerHuman(t, q2, releaseText)

	a2 := ""
	if !waitUntilDeadline(featureRunTimeout, func() bool {
		a2 = fx.scalar(t, fmt.Sprintf("SELECT id FROM messages WHERE run_id = '%s' AND reply_to = '%s';", fx.runID, q2))
		return a2 != ""
	}) {
		t.Fatalf("human answer a2 (reply-to %s) was never accepted within %s", q2, featureRunTimeout)
	}

	// The deterministic kill point: wait for the ORIGINAL manager's own
	// post-forward barrier observation naming a2, proving it has ALREADY
	// forwarded a2 (as a1) and is now blocked immediately before its own
	// ack of a2.
	barrierObs := waitForObservation(t, filepath.Join(scratchDir, postForwardBarrierObservedFile))
	if barrierObs.Fields["id"] != a2 {
		t.Fatalf("post-forward barrier observation names id=%s, want the accepted answer %s", barrierObs.Fields["id"], a2)
	}

	a1 := fx.scalar(t, fmt.Sprintf("SELECT id FROM messages WHERE run_id = '%s' AND reply_to = '%s' AND kind = 'answer';", fx.runID, q1))
	if a1 == "" {
		t.Fatalf("forwarded answer a1 (reply-to %s) does not exist yet at the post-forward barrier; the deterministic kill point did not hold", q1)
	}
	if fx.messageAcked(t, a2) {
		t.Fatalf("a2 was already acked before the self-kill; the deterministic kill point did not hold")
	}
	originalRequestID := fx.scalar(t, fmt.Sprintf(
		"SELECT request_id FROM message_receipts WHERE run_id = '%s' AND op = 'msg-send' AND created_entity_id = '%s' AND outcome = 'accepted';",
		fx.runID, a1))
	if originalRequestID == "" {
		t.Fatalf("no accepted msg-send receipt found for a1=%s", a1)
	}

	killControllerLeader(t, fx.controller)
	fx.killSession(t, managerSessionID, managerSessionID)

	waitForLeaseExpiry(t, fx.dbPath(), fx.runID)

	resumed := fx.server.startHopController(t, fx.stateDir, "resume", "resume", "-C", fx.repo.Root, "--confirm-absent="+managerSessionID, fx.runID)
	waitForControllerLog(t, fx.artifacts, "resume", "session "+managerSessionID+" (manager): relaunched")
	fx.controller, fx.controllerName = resumed, "resume"
	fx.requireRunState(t, "running")

	fx.requireSessionState(t, managerSessionID, "lost")
	newManagerSessionID := fx.managerSessionID(t)
	if newManagerSessionID == managerSessionID {
		t.Fatalf("manager session after cold relaunch = %s, want a NEW successor session distinct from the original %s", newManagerSessionID, managerSessionID)
	}

	// The idempotency proof: the relaunched manager re-fetches the
	// re-served a2 (never persisted anywhere by this fixture -- purely
	// CLI-returned data) and recomputes the SAME deterministic request id
	// from a2's own id, so its own redo of the forward call is refused as
	// a DUPLICATE of the already-accepted a1, never a second answer, and
	// the duplicate receipt carries that SAME request id.
	if !waitUntilDeadline(featureRunTimeout, func() bool { return fx.messageAcked(t, a2) }) {
		t.Fatalf("human answer a2 was never acknowledged by the relaunched manager within %s", featureRunTimeout)
	}
	if got := fx.scalar(t, fmt.Sprintf("SELECT session_id FROM message_acks WHERE message_id = '%s';", a2)); got != newManagerSessionID {
		t.Errorf("a2's ack session_id = %q, want the RELAUNCHED manager session %q", got, newManagerSessionID)
	}

	duplicateRequestID := ""
	if !waitUntilDeadline(featureRunTimeout, func() bool {
		duplicateRequestID = fx.scalar(t, fmt.Sprintf(
			"SELECT request_id FROM message_receipts WHERE run_id = '%s' AND op = 'msg-send' AND created_entity_id = '%s' AND outcome = 'duplicate';",
			fx.runID, a1))
		return duplicateRequestID != ""
	}) {
		t.Fatalf("no duplicate msg-send receipt for a1=%s was ever recorded by the relaunched manager's redo", a1)
	}
	if duplicateRequestID != originalRequestID {
		t.Errorf("relaunched manager's redo used request_id=%q, want the SAME deterministic id the original accepted forward used: %q", duplicateRequestID, originalRequestID)
	}

	if n := fx.scalar(t, fmt.Sprintf("SELECT count(*) FROM messages WHERE run_id = '%s' AND kind = 'answer' AND reply_to = '%s';", fx.runID, q1)); n != "1" {
		t.Errorf("count(answers with reply_to=%s) = %s, want exactly 1 (an idempotent redo must never create a second answer)", q1, n)
	}

	// Forward-before-ack still holds: a1's own created_at strictly
	// precedes a2's acked_at, even though the ack landed only after the
	// relaunch.
	a1CreatedAt := fx.messageCreatedAt(t, a1)
	a2AckedAt := fx.messageAckedAt(t, a2)
	if a1CreatedAt.After(a2AckedAt) {
		t.Errorf("forward-before-ack violated: a1 created_at=%s is AFTER a2 acked_at=%s", a1CreatedAt, a2AckedAt)
	}

	fx.requireTaskState(t, t1, "integrated")
	fx.requireRunState(t, "completed")
	if !waitUntilDeadline(featureRunTimeout, func() bool { return fx.messageAcked(t, a1) }) {
		t.Fatalf("forwarded answer a1 was never acknowledged by the worker within %s", featureRunTimeout)
	}
}

// requireMessageSendReceiptAccepted asserts an accepted msg-send receipt
// row exists naming messageID as its created_entity_id -- design section
// 7's "a receipt row for every verb outcome," checked for the acceptance
// case specifically.
func requireMessageSendReceiptAccepted(t *testing.T, fx *featureRun, messageID string) {
	t.Helper()
	if n := fx.scalar(t, fmt.Sprintf(
		"SELECT count(*) FROM message_receipts WHERE run_id = '%s' AND op = 'msg-send' AND created_entity_id = '%s' AND outcome = 'accepted';",
		fx.runID, messageID)); n == "0" {
		t.Errorf("no accepted msg-send receipt found for message %s", messageID)
	}
}

// requireMessageAckReceiptAccepted asserts an accepted msg-ack receipt row
// exists claiming messageID.
func requireMessageAckReceiptAccepted(t *testing.T, fx *featureRun, messageID string) {
	t.Helper()
	if n := fx.scalar(t, fmt.Sprintf(
		"SELECT count(*) FROM message_receipts WHERE run_id = '%s' AND op = 'msg-ack' AND claimed_message_id = '%s' AND outcome = 'accepted';",
		fx.runID, messageID)); n == "0" {
		t.Errorf("no accepted msg-ack receipt found for message %s", messageID)
	}
}

// grammarMessageShowLine mirrors internal/app/grammar.go's
// GrammarMessageShowLine exactly (retyped, never imported): hop msg show's
// first line is fully determined by messageID/kind/from/to/replyTo/relayOf
// and the message's own enqueue_seq, so a scenario that knows all of these
// can compare the WHOLE line, never a per-token Contains. replyTo and
// relayOf are optional ("" omits the field, exactly as the renderer does).
func grammarMessageShowLine(messageID, kind, from, to, replyTo, relayOf string, seq int) string {
	line := "message " + messageID + " kind=" + kind + " from=" + from + " to=" + to
	if replyTo != "" {
		line += " reply-to=" + replyTo
	}
	if relayOf != "" {
		line += " relay-of=" + relayOf
	}
	return line + " seq=" + strconv.Itoa(seq)
}

// requireMsgShowLine runs "hop msg show <id> -run <runID>" and asserts its
// first line equals EXACTLY grammarMessageShowLine's rendering for
// kind/from/to/replyTo/relayOf, with seq read from the store's own
// enqueue_seq column -- GrammarMessageShowLine is fully determined, so a
// per-token Contains would miss a stray, reordered or malformed field.
func requireMsgShowLine(t *testing.T, fx *featureRun, messageID, kind, from, to, replyTo, relayOf string) {
	t.Helper()
	seqRaw := fx.scalar(t, fmt.Sprintf("SELECT enqueue_seq FROM messages WHERE id = '%s';", messageID))
	seq, err := strconv.Atoi(seqRaw)
	if err != nil {
		t.Fatalf("message %s has no parsable enqueue_seq (%q): %v", messageID, seqRaw, err)
	}
	want := grammarMessageShowLine(messageID, kind, from, to, replyTo, relayOf, seq)
	out := runHop(t, fx.env, fx.repo.Root, "msg", "show", "-run", fx.runID, messageID)
	if out.ExitCode != 0 {
		t.Fatalf("hop msg show %s: exit=%d stdout=%q", messageID, out.ExitCode, out.Stdout)
	}
	if got := out.FirstStdoutLine(); got != want {
		t.Errorf("hop msg show %s first line = %q, want %q", messageID, got, want)
	}
}

// requireMsgShowAcknowledgedLine asserts hop msg show's output for
// messageID carries its acknowledged line rendered EXACTLY as
// GrammarAcknowledgedLine does (retyped, never imported): "acknowledged: "
// plus GrammarTime's own at.UTC().Format(time.RFC3339) -- never a bare
// Contains("acknowledged: ").
func requireMsgShowAcknowledgedLine(t *testing.T, fx *featureRun, messageID string) {
	t.Helper()
	ackedAt := fx.messageAckedAt(t, messageID)
	want := "acknowledged: " + ackedAt.UTC().Format(time.RFC3339)
	out := runHop(t, fx.env, fx.repo.Root, "msg", "show", "-run", fx.runID, messageID)
	if out.ExitCode != 0 {
		t.Fatalf("hop msg show %s: exit=%d stdout=%q", messageID, out.ExitCode, out.Stdout)
	}
	var got string
	for _, line := range strings.Split(out.Stdout, "\n") {
		if strings.HasPrefix(line, "acknowledged: ") {
			got = line
			break
		}
	}
	if got != want {
		t.Errorf("hop msg show %s acknowledged line = %q, want %q; full output:\n%s", messageID, got, want, out.Stdout)
	}
}
