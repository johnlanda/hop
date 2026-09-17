package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// grammarTransientUndeliveredLine mirrors
// internal/app/grammar.go's GrammarTransientUndeliveredLine (retyped,
// never imported, per this suite's own convention for cross-boundary
// grammar constants): the feature-mode retryable first line hop result
// submit renders when the task's mailbox is not yet clear (design
// section 5's drain-then-submit contract).
const grammarTransientUndeliveredLine = "transient: undelivered messages; drain with hop msg next, ack, then resubmit"

// grammarRefusalMailboxClosed mirrors GrammarRefusalLine(GrammarReasonMailboxClosed):
// a send addressed to a task whose mailbox is closed.
const grammarRefusalMailboxClosed = "refused: mailbox-closed"

// TestRealProcessMailboxClosureRace is the design section 11 "Layers"
// row's real-process proof: a manager send racing a worker's submission
// -- whichever order the store commits, either the submission is
// refused transient and the worker drains, or the send is refused
// mailbox-closed; no message is stranded, and receipts exist both ways
// -- plus the failure-path variant, where a worker exhausts its retries
// without an accepted result, the failing settlement closes the mailbox
// and journals the orphaned message ids in the manager's notice, and a
// later send is refused.
//
// The ordering in each case is made deterministic with fixture barriers
// and structural sequencing, never with sleeps: the sqlite adapter suite
// already proves the true concurrent race from separate store handles
// (design section 11's Layers row); this scenario's job is to prove the
// SAME two outcomes hold when driven through the real, shipped binary,
// which it does by constructing each order explicitly.
func TestRealProcessMailboxClosureRace(t *testing.T) {
	t.Run("Race", testRealProcessMailboxClosureRaceOrdering)
	t.Run("FailureClosesMailboxWithOrphanedObligations", testRealProcessMailboxClosureRaceFailure)
}

// testRealProcessMailboxClosureRaceOrdering covers both commit orders in
// one run: task t1 (behavior submit-valid, a legacy Phase-2 behavior
// with no messaging awareness at all -- it never pre-drains) proves
// "the send lands first" (queued from the moment the task exists, long
// before any worker process even launches, so the worker's own FIRST
// hop result submit call is refused transient by construction, and its
// own built-in retry-on-transient logic, submitOnce, drains and
// resubmits); task t2 (behavior worker-implement, which drains before
// ever submitting and so integrates normally) proves "the closure lands
// first" (a send issued only once its mailbox is already durably closed
// is refused).
func testRealProcessMailboxClosureRaceOrdering(t *testing.T) {
	start := time.Now()
	defer func() { t.Logf("MailboxClosureRace/Race wall time: %s", time.Since(start)) }()

	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	worker := buildFixtureWorker(t, artifacts)
	installFixtureWorkerAsClaudeStub(t, server, worker)
	server.start(t)

	scratchDir := artifacts.dir(t, "fixture-scratch")
	repo := newFeatureFixtureRepo(t, artifacts, server, "mailbox-race-repo", featureFixtureOptions{
		ScratchDir: scratchDir, ReviewerBehavior: "reviewer-approve",
		MaxWorkers: 2, RetryLimit: 3, MessageWaitTimeout: "3s", MessageAttentionAfter: "30s",
	})

	brief := fixtureManagerBrief(scratchDir,
		[]fixtureManagerTask{
			{Label: "t1", Title: "Implement t1 (send lands first)", Behavior: "submit-valid"},
			{Label: "t2", Title: "Implement t2 (closure lands first)", Behavior: "worker-implement"},
		},
		nil, "",
	)
	fx := startFeatureRun(t, artifacts, server, repo, scratchDir, brief)

	t1 := fx.requireTaskBySeq(t, 1)
	t2 := fx.requireTaskBySeq(t, 2)
	managerEnv := fx.managerEnv(t)

	// --- Order A: the send lands first. ---
	// Addressed to t1's mailbox the INSTANT the task row exists (before
	// the controller has even run one scheduling pass, let alone before
	// the real worker process has launched, committed a change and
	// called hop result submit itself) -- a structural guarantee, not a
	// timed race: the message is unconditionally queued long before
	// submit-valid's worker can possibly reach its own submission.
	sendFirstPath := filepath.Join(scratchDir, "race-send-first-t1.md")
	if err := os.WriteFile(sendFirstPath, []byte("manager info racing t1's own first submission attempt\n"), 0o600); err != nil {
		t.Fatalf("write race message body: %v", err)
	}
	sendFirstResult := runManagerVerb(t, managerEnv, repo.Root, "msg", "send", "--to", taskAddress(t1), "--kind", "info", "--file", sendFirstPath, "--request-id", newSpikeUUID(t))
	if sendFirstResult.ExitCode != 0 {
		t.Fatalf("hop msg send (send-lands-first race message): exit=%d stdout=%q stderr=%q", sendFirstResult.ExitCode, sendFirstResult.Stdout, sendFirstResult.Stderr)
	}
	sendFirstMessageID := parseSentID(t, sendFirstResult.Stdout)

	fx.requireTaskState(t, t1, "active", "checking", "completed", "integrating", "integrated")
	attempt1ID, _ := fx.currentAttempt(t, t1)
	session1ID := fx.sessionForAttempt(t, attempt1ID)

	// The submission is refused transient at least once: submit-valid's
	// FIRST hop result submit call runs against a mailbox that is not
	// yet clear, by construction.
	if !waitUntilDeadline(featureRunTimeout, func() bool {
		return fx.scalar(t, fmt.Sprintf(
			"SELECT count(*) FROM result_submissions WHERE claimed_attempt_id = '%s' AND outcome = 'transient' AND detail = '%s';",
			attempt1ID, grammarTransientUndeliveredLine)) != "0"
	}) {
		t.Fatalf("no transient (mailbox-not-clear) result_submissions receipt observed for attempt %s within %s", attempt1ID, featureRunTimeout)
	}

	// The worker's OWN built-in retry-on-transient logic (submitOnce)
	// drains the race message and resubmits successfully -- no test-side
	// retry, no message stranded, and the task completes normally.
	fx.requireTaskState(t, t1, "integrated")
	if !fx.messageAcked(t, sendFirstMessageID) {
		t.Errorf("race message %s was never acknowledged after t1 integrated; a message must never be stranded", sendFirstMessageID)
	}
	if !messageDeliveredToSession(t, fx, sendFirstMessageID, session1ID) {
		t.Errorf("race message %s was never delivered to t1's own session %s", sendFirstMessageID, session1ID)
	}
	if n := fx.scalar(t, fmt.Sprintf("SELECT count(*) FROM result_submissions WHERE claimed_attempt_id = '%s' AND outcome = 'accepted';", attempt1ID)); n != "1" {
		t.Errorf("accepted result_submissions receipt count for attempt %s = %s, want exactly 1", attempt1ID, n)
	}

	// --- Order B: the closure lands first. ---
	// t2 (worker-implement: drains -- vacuously, nothing pending -- then
	// submits) integrates normally; its acceptance closes its own
	// mailbox in the SAME transaction (design section 5). Only once that
	// is durably true does this send land, so the refusal it proves is
	// "closure landed first", not a race whose outcome happens to be
	// this one.
	fx.requireTaskState(t, t2, "integrated")
	if !fx.taskMailboxClosed(t, t2) {
		t.Fatalf("task %s's mailbox is not closed after it integrated", t2)
	}
	closureFirstPath := filepath.Join(scratchDir, "race-closure-first-t2.md")
	closureFirstBody := "manager info addressed after t2's own mailbox already closed\n"
	if err := os.WriteFile(closureFirstPath, []byte(closureFirstBody), 0o600); err != nil {
		t.Fatalf("write race message body: %v", err)
	}
	closureFirstResult := runManagerVerb(t, managerEnv, repo.Root, "msg", "send", "--to", taskAddress(t2), "--kind", "info", "--file", closureFirstPath, "--request-id", newSpikeUUID(t))
	if closureFirstResult.ExitCode == 0 {
		t.Fatalf("hop msg send to a closed mailbox unexpectedly succeeded: stdout=%q", closureFirstResult.Stdout)
	}
	if got := closureFirstResult.FirstStdoutLine(); got != grammarRefusalMailboxClosed {
		t.Errorf("hop msg send to a closed mailbox first line = %q, want %q", got, grammarRefusalMailboxClosed)
	}
	closureFirstDigest := sha256HexOf(closureFirstBody)
	if n := fx.scalar(t, fmt.Sprintf(
		"SELECT count(*) FROM messages WHERE run_id = '%s' AND recipient_address = '%s' AND body_digest = '%s';",
		fx.runID, taskAddress(t2), closureFirstDigest)); n != "0" {
		t.Errorf("a message row was created despite the mailbox-closed refusal (count=%s)", n)
	}
	if n := fx.scalar(t, fmt.Sprintf(
		"SELECT count(*) FROM message_receipts WHERE run_id = '%s' AND outcome = 'refused-mailbox-closed' AND claimed_recipient = '%s';",
		fx.runID, taskAddress(t2))); n == "0" {
		t.Errorf("no message_receipts row recorded the mailbox-closed refusal for %s", taskAddress(t2))
	}

	fx.requireRunState(t, "completed")
}

// testRealProcessMailboxClosureRaceFailure proves the failure-path
// variant: a worker-hold task killed mid-attempt with its retry budget
// already exhausted (RetryLimit=1: one attempt, no room) fails directly
// -- never a needs-rework detour (design section 5 reference trace 4's
// exhaustion variant) -- and the failing settlement's snapshot-equality
// contract journals an orphaned message, addressed to the still-open
// mailbox the instant before settlement, in the manager's own notice.
// The mailbox never reopens after a failure closure: a later send is
// refused.
func testRealProcessMailboxClosureRaceFailure(t *testing.T) {
	start := time.Now()
	defer func() {
		t.Logf("MailboxClosureRace/FailureClosesMailboxWithOrphanedObligations wall time: %s", time.Since(start))
	}()

	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	worker := buildFixtureWorker(t, artifacts)
	installFixtureWorkerAsClaudeStub(t, server, worker)
	server.start(t)

	scratchDir := artifacts.dir(t, "fixture-scratch")
	repo := newFeatureFixtureRepo(t, artifacts, server, "mailbox-race-failure-repo", featureFixtureOptions{
		ScratchDir: scratchDir, ReviewerBehavior: "reviewer-approve",
		MaxWorkers: 1, RetryLimit: 1, MessageWaitTimeout: "3s", MessageAttentionAfter: "30s",
	})

	brief := fixtureManagerBrief(scratchDir,
		[]fixtureManagerTask{{Label: "t1", Title: "Implement t1", Behavior: "worker-hold"}},
		[]fixtureManagerAnswer{{Match: fixtureHoldMarker, Action: "relay"}},
		"",
	)
	fx := startFeatureRun(t, artifacts, server, repo, scratchDir, brief)

	t1 := fx.requireTaskBySeq(t, 1)
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
	// Captured while the manager is definitely still live, before this
	// scenario kills the WORKER (a different session): the manager's own
	// identity does not depend on anything this test does to the worker.
	managerEnv := fx.managerEnv(t)

	// Kill the worker before its barrier is ever answered: mailbox still
	// open, no accepted result, its own launch already settled (execed,
	// asserted above) -- design section 11 scenario 4's own precondition,
	// generalized here to the exhaustion variant by RetryLimit=1.
	fx.killSession(t, session1ID, attempt1ID)

	// Immediately -- a single near-instant hop CLI call issued the
	// moment killSession's own bounded wait confirms the pane is gone,
	// comfortably ahead of the live controller's own multi-second
	// scheduling-pass cadence -- address an info message to the
	// STILL-OPEN mailbox: this is the orphaned obligation the failing
	// settlement's snapshot-equality contract (design section 5) must
	// capture in its notice.
	orphanPath := filepath.Join(scratchDir, "orphan-info-t1.md")
	if err := os.WriteFile(orphanPath, []byte("manager info addressed to t1 while its worker is dead and its mailbox still open\n"), 0o600); err != nil {
		t.Fatalf("write orphan message body: %v", err)
	}
	orphanResult := runManagerVerb(t, managerEnv, repo.Root, "msg", "send", "--to", taskAddress(t1), "--kind", "info", "--file", orphanPath, "--request-id", newSpikeUUID(t))
	if orphanResult.ExitCode != 0 {
		t.Fatalf("hop msg send (orphan info) exit=%d stdout=%q stderr=%q", orphanResult.ExitCode, orphanResult.Stdout, orphanResult.Stderr)
	}
	orphanMessageID := parseSentID(t, orphanResult.Stdout)

	// The live controller reconciles on its own next scheduling pass:
	// the retry budget is already exhausted (one attempt, RetryLimit=1),
	// so the task fails directly, never parking at needs-rework.
	fx.requireTaskState(t, t1, "failed")
	fx.requireSessionState(t, session1ID, "terminated")

	noticeID := fx.requireManagerNoticeFirstLine(t, featureRunTimeout, "task t1 failed")
	noticeBody := fx.messageBodyContent(t, noticeID)
	// Exact line match, not a loose substring: this is the ONLY message
	// ever addressed to task:t1's own mailbox before the kill, so the
	// snapshot-equality contract's obligation set must name it and
	// nothing else.
	wantObligationsLine := "orphaned obligations: " + orphanMessageID
	if !slices.Contains(strings.Split(noticeBody, "\n"), wantObligationsLine) {
		t.Errorf("failure notice does not carry the exact line %q; body:\n%s", wantObligationsLine, noticeBody)
	}

	if !fx.taskMailboxClosed(t, t1) {
		t.Error("task t1's mailbox is not closed after its failing settlement")
	}
	if fx.messageAcked(t, orphanMessageID) {
		t.Errorf("orphaned message %s was acknowledged; it must never be -- the recipient is gone and a failure closure never reopens the mailbox", orphanMessageID)
	}

	// A failure closure never reopens: a later send is refused.
	laterPath := filepath.Join(scratchDir, "later-info-t1.md")
	if err := os.WriteFile(laterPath, []byte("a later manager send against the same, now failure-closed, mailbox\n"), 0o600); err != nil {
		t.Fatalf("write later message body: %v", err)
	}
	laterResult := runManagerVerb(t, managerEnv, repo.Root, "msg", "send", "--to", taskAddress(t1), "--kind", "info", "--file", laterPath, "--request-id", newSpikeUUID(t))
	if laterResult.ExitCode == 0 {
		t.Fatalf("hop msg send to a failure-closed mailbox unexpectedly succeeded: stdout=%q", laterResult.Stdout)
	}
	if got := laterResult.FirstStdoutLine(); got != grammarRefusalMailboxClosed {
		t.Errorf("hop msg send to a failure-closed mailbox first line = %q, want %q", got, grammarRefusalMailboxClosed)
	}

	fx.requireRunState(t, "failed")
}
