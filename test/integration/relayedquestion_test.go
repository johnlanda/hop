package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRealProcessRelayedQuestion is design section 11 scenario 2 (reference
// trace 2, extended with a manager cold relaunch mid-relay so the recovery
// path itself is exercised): a worker (task t1, behavior worker-hold) sends
// question q1 to the manager; the manager relays it to the human as q2
// (--relay-of q1); the human answers via a direct hop answer (a2, accepted
// atomically with q2's own acknowledgement); the manager fetches a2 --
// deterministically, the fixture manager's opt-in pre-forward barrier
// (fixtureworker_test.go's handleManagerMessage "answer" case) blocks it
// there, dumping an observation naming a2's id -- and this test kills that
// ORIGINAL manager session at exactly that point (never a race against the
// manager's own next poll tick, unlike a structurally-favored ordering).
// The controller is then killed and hop resume --confirm-absent=<manager
// session> cold-relaunches the manager: usecase_featureresume.go's
// coldRelaunchFeatureSession mints a brand-new SUCCESSOR session id bound
// to the SAME native_session_ref (confirmed from source, never assumed --
// a manager relaunch never reuses the prior session id with a new
// incarnation), marking the prior session "lost" in the same transaction.
// The relaunched (resumed) manager's own barrier is a no-op (one-shot,
// keyed to runManager's resumed flag): it re-fetches the re-served a2 using
// ONLY CLI-returned data (the envelope's own origin field), forwards it as
// a1 (reply-to q1, destination derived as task:t1) under its own recorded
// --request-id, THEN acks a2 (forward-before-ack, so a manager crash
// between the two would re-serve a2 and the forward's request id makes any
// redo idempotent) -- and the de-risking assertion that actually PROVES
// recovery, not mere idempotent luck: a1's sender_session_id is the
// RELAUNCHED (successor) manager session, never the original. The worker
// (never killed, still polling its own barrier) fetches a1, acks it,
// drains and submits; the task integrates and the run completes. Every
// envelope/delivery/ack row is audited both directly against the store and
// independently through the read-only hop msg show surface. Never restarts
// the disposable server: relaunch requires server continuity.
func TestRealProcessRelayedQuestion(t *testing.T) {
	start := time.Now()
	defer func() { t.Logf("TestRealProcessRelayedQuestion wall time: %s", time.Since(start)) }()

	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	worker := buildFixtureWorker(t, artifacts)
	installFixtureWorkerAsClaudeStub(t, server, worker)
	server.start(t)

	scratchDir := artifacts.dir(t, "fixture-scratch")
	// Enable the manager's opt-in pre-forward barrier BEFORE the run ever
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

	fx.killSession(t, managerSessionID, managerSessionID)

	killControllerLeader(t, fx.controller)
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

	// Forward-before-ack, proved directly from the journal: messages.created_at
	// and message_acks.acked_at are both written through sqlite.go's own
	// single formatTime/timeLayout pair over the SAME s.now() clock
	// (internal/adapters/sqlite/messages.go's insertMessage: formatTime(m.CreatedAt);
	// internal/adapters/sqlite/messaging.go's AckMessage: formatTime(outcomeVal.Ack.At)) --
	// one shared format and clock source across every table in this
	// store, so a direct lexical comparison across these two columns is
	// valid, not merely within one table.
	a1CreatedAt := fx.scalar(t, fmt.Sprintf("SELECT created_at FROM messages WHERE id = '%s';", a1))
	a2AckedAt := fx.scalar(t, fmt.Sprintf("SELECT acked_at FROM message_acks WHERE message_id = '%s';", a2))
	if a1CreatedAt == "" || a2AckedAt == "" {
		t.Fatalf("missing timestamps for the forward-before-ack check: a1.created_at=%q a2.acked_at=%q", a1CreatedAt, a2AckedAt)
	}
	if a1CreatedAt > a2AckedAt {
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

	// Every hop is a journaled row: audit q1, q2, a1, a2 independently
	// through hop msg show (the read-only same-run envelope lookup, needs
	// no caller identity) as well as the direct store reads above.
	requireMsgShowLine(t, fx, q1, "kind="+"question"+" from="+session1ID+" to=manager")
	requireMsgShowLine(t, fx, q2, "kind="+"question"+" from="+managerSessionID+" to=human"+" relay-of="+q1)
	requireMsgShowLine(t, fx, a2, "kind="+"answer"+" from=human to=manager"+" reply-to="+q2)
	requireMsgShowLine(t, fx, a1, "kind="+"answer"+" from="+newManagerSessionID+" to="+taskAddress(t1)+" reply-to="+q1)
	for _, id := range []string{q1, q2, a1, a2} {
		out := runHop(t, fx.env, fx.repo.Root, "msg", "show", "-run", fx.runID, id)
		if out.ExitCode != 0 {
			t.Fatalf("hop msg show %s: exit=%d stdout=%q", id, out.ExitCode, out.Stdout)
		}
		if !strings.Contains(out.Stdout, "acknowledged: ") {
			t.Errorf("hop msg show %s does not report an acknowledged: line; got:\n%s", id, out.Stdout)
		}
	}
}

// requireMsgShowLine runs "hop msg show <id> -run <runID>" and asserts its
// first line CONTAINS every one of wantFields' space-separated
// "key=value" tokens (each checked independently, order-independent,
// since GrammarMessageShowLine's optional reply-to/relay-of fields only
// ever appear in a fixed relative order this scenario already knows) --
// design section 7's grammar, retyped never imported.
func requireMsgShowLine(t *testing.T, fx *featureRun, messageID, wantFields string) {
	t.Helper()
	out := runHop(t, fx.env, fx.repo.Root, "msg", "show", "-run", fx.runID, messageID)
	if out.ExitCode != 0 {
		t.Fatalf("hop msg show %s: exit=%d stdout=%q", messageID, out.ExitCode, out.Stdout)
	}
	first := out.FirstStdoutLine()
	if !strings.HasPrefix(first, "message "+messageID+" ") {
		t.Fatalf("hop msg show %s first line = %q, want it to start with \"message %s \"", messageID, first, messageID)
	}
	for _, field := range strings.Fields(wantFields) {
		if !strings.Contains(first, field) {
			t.Errorf("hop msg show %s first line missing %q; got: %q", messageID, field, first)
		}
	}
}
