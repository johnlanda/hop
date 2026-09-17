package integration

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This file adds the one capability slice 7a's harness does not itself
// provide: driving a manager verb (hop task create/plan close/msg send)
// as a direct, test-issued one-shot process authenticated with the run's
// OWN live manager session identity -- exactly the technique
// featureharness_test.go's answerHuman already uses for "human" (a
// separate real hop process, never typed pane input), generalized to the
// manager's own HOP_SESSION_ID/HOP_INCARNATION_ID so a scenario can
// construct an exact send/submission ordering deterministically (never
// with a sleep), or seed an artifact-delivery channel with content the
// scripted manager-feature grammar cannot carry losslessly (its
// whitespace-delimited, underscore-substituted TASK/ANSWER fields), in
// place of depending on the SCRIPTED fixture manager's own timing or
// text encoding. A call issued this way is exactly as legitimate as one
// the live scripted manager process would make itself: the store
// validates only the session id, the run it belongs to, its current
// incarnation and its resolved address's currency (design section 7's
// run.CurrentAddressSession -- internal/app/usecase_message.go's
// SendMessage, internal/adapters/sqlite/messaging.go's SendMessage) --
// never which OS process happened to invoke the CLI. Shared by
// injectionfreedelivery_test.go, mailboxclosurerace_test.go,
// relayedquestion_test.go and duplicateandambiguousdelivery_test.go.

// managerVerbRetryInterval paces runManagerVerb's retry-on-transient
// loop, mirroring the embedded fixture's own fixtureRetryInterval.
const managerVerbRetryInterval = 200 * time.Millisecond

// managerVerbRetryDeadline bounds runManagerVerb, mirroring the embedded
// fixture's own submitOnce/runHopCLIRetryable bound.
const managerVerbRetryDeadline = 2 * time.Minute

// runManagerVerb runs one manager-identity verb (task create, plan
// close, msg send/answer) as a direct one-shot process, retrying while
// the first stdout line begins with "transient:" -- design section 7's
// run-state acceptance rule (a manager verb legitimately gets a
// retryable line while the run is still launching or resuming) applies
// to a directly-issued call exactly as it does to the scripted fixture
// manager's own runHopCLIRetryable, which this mirrors. It never retries
// a "refused:" line.
func runManagerVerb(t *testing.T, env []string, dir string, args ...string) hopResult {
	t.Helper()
	deadline := time.Now().Add(managerVerbRetryDeadline)
	for {
		result := runHop(t, env, dir, args...)
		if !strings.HasPrefix(result.FirstStdoutLine(), "transient:") || !time.Now().Before(deadline) {
			return result
		}
		time.Sleep(managerVerbRetryInterval)
	}
}

// managerEnv returns the environment for a one-shot hop CLI invocation
// authenticated as this run's own manager session.
//
// This never collides with the live manager process's OWN concurrent
// hop msg wait loop (scripted or unscripted, whichever a scenario uses):
// fetch is scoped strictly to the caller's own RESOLVED LOGICAL ADDRESS
// (LoadMessagingContext -- a manager session always resolves to
// "manager"; a worker/reviewer session resolves to "task:<its task>").
// The manager process has no code path that ever fetches from a
// "task:<id>" mailbox at all, so a message this helper addresses to a
// task can never be seen, consumed or acted on by the concurrently
// running manager, regardless of timing -- the address space itself
// rules it out, not any ordering this test happens to get right.
func (f *featureRun) managerEnv(t *testing.T) []string {
	t.Helper()
	sessionID := f.managerSessionID(t)
	return f.sessionEnv(sessionID, f.incarnationForSession(t, sessionID))
}

// sessionEnv returns the environment for a one-shot hop CLI invocation
// authenticated as an arbitrary session/incarnation pair -- the same
// technique managerEnv uses for the manager's own current identity,
// generalized so a scenario can issue a call under a RETIRED (superseded)
// session's identity too, e.g. RelayedQuestion/DuplicateAndAmbiguousDelivery
// issuing a stale incarnation's late ack directly.
func (f *featureRun) sessionEnv(sessionID, incarnationID string) []string {
	return append(append([]string{}, f.env...),
		"HOP_RUN_ID="+f.runID,
		"HOP_SESSION_ID="+sessionID,
		"HOP_INCARNATION_ID="+incarnationID,
	)
}

// incarnationForSession reads sessionID's current runtime binding's
// incarnation id -- the same value a real launched process would see in
// its own HOP_INCARNATION_ID env, valid once a launch claim has settled
// for it. Generalizes resultsubmit_test.go's attempt-keyed
// currentIncarnationID to any session (the manager's own, here), since a
// direct manager-identity invocation needs the MANAGER session's current
// incarnation, not a worker attempt's.
func (f *featureRun) incarnationForSession(t *testing.T, sessionID string) string {
	t.Helper()
	id := f.scalar(t, fmt.Sprintf("SELECT incarnation_id FROM runtime_bindings WHERE session_id = '%s' ORDER BY observed_at DESC, rowid DESC LIMIT 1;", sessionID))
	if id == "" {
		t.Fatalf("no runtime binding (and therefore no incarnation id) recorded yet for session %s", sessionID)
	}
	return id
}

// taskAddress renders a task's canonical recipient-address form,
// "task:<uuid>" (design section 7; internal/app/messaging.go's
// AddressString for run.AddressTask) -- retyped here, never imported,
// since this suite is not importing internal/app or internal/domain/run.
func taskAddress(taskID string) string { return "task:" + taskID }

// sessionQuestionTo polls for the oldest question sessionID itself sent
// to recipientAddress, bounded by featureRunTimeout -- locates a
// worker-hold barrier's own question directly, for a scenario that
// answers it itself rather than following featureharness_test.go's
// relayedQuestionFor human-relay chain.
func (f *featureRun) sessionQuestionTo(t *testing.T, sessionID, recipientAddress string) (questionID string) {
	t.Helper()
	ok := waitUntilDeadline(featureRunTimeout, func() bool {
		id := f.scalar(t, fmt.Sprintf(
			"SELECT id FROM messages WHERE run_id = '%s' AND sender_session_id = '%s' AND recipient_address = '%s' AND kind = 'question' ORDER BY enqueue_seq LIMIT 1;",
			f.runID, sessionID, recipientAddress))
		if id == "" {
			return false
		}
		questionID = id
		return true
	})
	if !ok {
		t.Fatalf("no question observed from session %s to %s within %s", sessionID, recipientAddress, featureRunTimeout)
	}
	return questionID
}

// taskMailboxClosed reports whether taskID's mailbox is currently closed
// (tasks.mailbox_closed_at IS NOT NULL).
func (f *featureRun) taskMailboxClosed(t *testing.T, taskID string) bool {
	t.Helper()
	return f.scalar(t, fmt.Sprintf("SELECT CASE WHEN mailbox_closed_at IS NULL THEN '' ELSE '1' END FROM tasks WHERE id = '%s';", taskID)) == "1"
}

// messageAcked reports whether messageID has an acknowledgement row.
func (f *featureRun) messageAcked(t *testing.T, messageID string) bool {
	t.Helper()
	return f.scalar(t, fmt.Sprintf("SELECT count(*) FROM message_acks WHERE message_id = '%s';", messageID)) == "1"
}

// parseSentID parses hop msg send/answer's accepted/duplicate first line
// ("sent <uuid>" / "duplicate <uuid>") and returns the message id,
// failing the test loudly on any other shape. Test-side retyping of the
// embedded fixture's own parseSentMessageID (fixtureworker_test.go,
// compiled only into the separate fixture "claude" binary and therefore
// not callable from this package's own test code).
func parseSentID(t *testing.T, stdout string) string {
	t.Helper()
	first, _, _ := strings.Cut(stdout, "\n")
	fields := strings.Fields(first)
	if len(fields) >= 2 && (fields[0] == "sent" || fields[0] == "duplicate") {
		return fields[1]
	}
	t.Fatalf("unexpected hop msg send/answer first line %q, want \"sent <uuid>\" or \"duplicate <uuid>\"", first)
	return ""
}

// parseTaskID parses hop task create's accepted/duplicate first line
// ("task <uuid> t<seq> created" / "duplicate <uuid> t<seq>") and returns
// the task id, failing the test loudly on any other shape. Test-side
// retyping of the embedded fixture's own parseCreatedTaskID, for the same
// reason as parseSentID above.
func parseTaskID(t *testing.T, stdout string) string {
	t.Helper()
	first, _, _ := strings.Cut(stdout, "\n")
	fields := strings.Fields(first)
	if len(fields) >= 2 && (fields[0] == "task" || fields[0] == "duplicate") {
		return fields[1]
	}
	t.Fatalf("unexpected hop task create first line %q, want \"task <uuid> t<seq> created\" or \"duplicate <uuid> t<seq>\"", first)
	return ""
}

// waitForObservation polls for a fixture principal's observation dump to
// appear at path, bounded by featureRunTimeout, and parses it --
// writeObservation's write is atomic (temp-file-then-rename), so a
// caller never observes a partial file, only "not yet present".
func waitForObservation(t *testing.T, path string) workerObservation {
	t.Helper()
	ok := waitUntilDeadline(featureRunTimeout, func() bool {
		_, err := os.Stat(path)
		return err == nil
	})
	if !ok {
		t.Fatalf("fixture principal observation dump %s was never observed within %s", path, featureRunTimeout)
	}
	return readWorkerObservation(t, path)
}

// attentionPollInterval paces a poll loop that spawns a full `hop status`
// CLI subprocess (opening the sqlite store) on every iteration, rather
// than the suite's default 25ms pollInterval: hammering the store that
// frequently, right after a SIGKILL of its own writer (killControllerLeader),
// risks transient SQLite WAL lock contention. querySQLite tolerates this via
// sqlite3's own busy-timeout (`-cmd ".timeout 5000"`, matching production's
// busy_timeout(5000)), not the string-matching Go-level retry loop it
// replaced (17533e0) -- but that is a per-call busy-WAIT inside one sqlite3
// invocation, not a retry of the whole command: a lock still held after 5s
// still fails the test immediately. A condition that only needs to observe
// crossing a multi-second attention threshold does not need millisecond
// polling granularity regardless.
const attentionPollInterval = 500 * time.Millisecond

// waitUntilDeadlineWithInterval polls condition like waitUntilDeadline, but
// at a caller-chosen interval instead of the suite's default pollInterval.
func waitUntilDeadlineWithInterval(deadline, interval time.Duration, condition func() bool) bool {
	end := time.Now().Add(deadline)
	for {
		if condition() {
			return true
		}
		if time.Now().After(end) {
			return false
		}
		time.Sleep(interval)
	}
}

// requireByteExactFile fails the test unless the file at path exists and
// its content equals want byte for byte -- design section 11's
// injection-free claim rests on exact bytes, never a substring or
// digest-only check.
func requireByteExactFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path) //nolint:gosec // G304: a path this test computed itself from its own run state.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("content of %s is not byte-exact: got %q (%d bytes), want %q (%d bytes)", path, got, len(got), want, len(want))
	}
}

// nativeSessionRef reads one session's recorded native_session_ref --
// shared by RelayedQuestion and DuplicateAndAmbiguousDelivery to prove a
// cold relaunch's successor session binds to the SAME native reference as
// its prior session (usecase_featureresume.go's coldRelaunchFeatureSession:
// "successor.AssignNativeRef(prior.NativeSessionRef, ...)").
func (f *featureRun) nativeSessionRef(t *testing.T, sessionID string) string {
	t.Helper()
	ref := f.scalar(t, fmt.Sprintf("SELECT native_session_ref FROM sessions WHERE id = '%s';", sessionID))
	if ref == "" {
		t.Fatalf("no native_session_ref recorded for session %s", sessionID)
	}
	return ref
}

// ackRefusalReceiptRecorded reports whether a msg-ack receipt row exists
// for messageID claimed by sessionID/incarnationID with outcome "refused"
// -- design section 7's "a receipt row for every verb outcome including
// refusals" (internal/adapters/sqlite/messaging.go's msgAckVerb =
// "msg-ack"; AckMessage's own record closure always writes
// outcome=string(kind), and app.AckRefused's string value is "refused").
// Binding the check to the CLAIMED incarnation too (not just the session)
// lets a caller assert both "no such row before the call" (using the
// specific old incarnation that is about to be refused) and, after the
// call, that the recorded row is claimed by that exact incarnation --
// never merely inferred from timing.
func (f *featureRun) ackRefusalReceiptRecorded(t *testing.T, messageID, sessionID, incarnationID string) bool {
	t.Helper()
	return f.scalar(t, fmt.Sprintf(
		"SELECT count(*) FROM message_receipts WHERE run_id = '%s' AND op = 'msg-ack' AND claimed_message_id = '%s' AND claimed_session_id = '%s' AND claimed_incarnation_id = '%s' AND outcome = 'refused';",
		f.runID, messageID, sessionID, incarnationID)) != "0"
}

// messageDeliveredAt reads the delivered_at timestamp of messageID's
// delivery row to sessionID specifically, parsed with the store's own
// fixed-width UTC timestamp layout (leaseTimeLayout, lifecycle_test.go,
// mirrors internal/adapters/sqlite's unexported timeLayout) -- every
// table's timestamp column is written through the SAME formatTime, so
// this parse applies uniformly across messages/message_deliveries/
// message_acks/message_receipts.
func (f *featureRun) messageDeliveredAt(t *testing.T, messageID, sessionID string) time.Time {
	t.Helper()
	raw := f.scalar(t, fmt.Sprintf("SELECT delivered_at FROM message_deliveries WHERE message_id = '%s' AND session_id = '%s';", messageID, sessionID))
	if raw == "" {
		t.Fatalf("no delivery row for message %s to session %s", messageID, sessionID)
	}
	at, err := time.Parse(leaseTimeLayout, raw)
	if err != nil {
		t.Fatalf("parse delivered_at %q for message %s (session %s): %v", raw, messageID, sessionID, err)
	}
	return at
}

// messageCreatedAt reads messageID's created_at timestamp, parsed the same
// way as messageDeliveredAt.
func (f *featureRun) messageCreatedAt(t *testing.T, messageID string) time.Time {
	t.Helper()
	raw := f.scalar(t, fmt.Sprintf("SELECT created_at FROM messages WHERE id = '%s';", messageID))
	if raw == "" {
		t.Fatalf("no messages row for %s", messageID)
	}
	at, err := time.Parse(leaseTimeLayout, raw)
	if err != nil {
		t.Fatalf("parse created_at %q for message %s: %v", raw, messageID, err)
	}
	return at
}

// messageAckedAt reads messageID's acked_at timestamp, parsed the same way
// as messageDeliveredAt.
func (f *featureRun) messageAckedAt(t *testing.T, messageID string) time.Time {
	t.Helper()
	raw := f.scalar(t, fmt.Sprintf("SELECT acked_at FROM message_acks WHERE message_id = '%s';", messageID))
	if raw == "" {
		t.Fatalf("no message_acks row for %s", messageID)
	}
	at, err := time.Parse(leaseTimeLayout, raw)
	if err != nil {
		t.Fatalf("parse acked_at %q for message %s: %v", raw, messageID, err)
	}
	return at
}

// messageReceiptAt reads the `at` timestamp of the single message_receipts
// row matching op/messageID/sessionID/incarnationID/outcome, parsed the
// same way as messageDeliveredAt.
func (f *featureRun) messageReceiptAt(t *testing.T, op, messageID, sessionID, incarnationID, outcome string) time.Time {
	t.Helper()
	raw := f.scalar(t, fmt.Sprintf(
		"SELECT at FROM message_receipts WHERE run_id = '%s' AND op = '%s' AND claimed_message_id = '%s' AND claimed_session_id = '%s' AND claimed_incarnation_id = '%s' AND outcome = '%s';",
		f.runID, op, messageID, sessionID, incarnationID, outcome))
	if raw == "" {
		t.Fatalf("no message_receipts row for op=%s message=%s session=%s incarnation=%s outcome=%s", op, messageID, sessionID, incarnationID, outcome)
	}
	at, err := time.Parse(leaseTimeLayout, raw)
	if err != nil {
		t.Fatalf("parse message_receipts.at %q for op=%s message=%s: %v", raw, op, messageID, err)
	}
	return at
}

// grammarTaskAddress mirrors internal/app/grammar.go's GrammarTaskAddress
// (retyped, never imported): a task mailbox's display address exactly as
// hop status -run's feature-mode detail block renders it -- the uuid form
// section 7's message verbs themselves take, alongside its t<seq> label
// for readability.
func grammarTaskAddress(taskID, label string) string {
	return "task:" + taskID + " (" + label + ")"
}

// attentionLineBothClausesExact retypes internal/app/grammar.go's
// GrammarAttentionLine rendering for the shape this suite's scenarios
// need -- both the in-flight and queued clauses present, joined with
// ", " -- never imported, and requires an EXACT, anchored match against
// wantAddress/wantInFlightID/wantQueued (never a substring/Contains
// check): a task address itself carries a colon ("task:<uuid> (t<seq>)"),
// so this cuts on the address's own known value rather than guessing
// where it ends. The two ages are the line's only free values, parsed
// back with time.ParseDuration (GrammarAttentionLine renders them via
// time.Duration.String(), never a fixed-width or custom format) rather
// than compared as text. ok is false for any line that is not this exact
// shape for this exact address/id/count, including a line for a
// different address or a queued count that does not match.
func attentionLineBothClausesExact(line, wantAddress, wantInFlightID string, wantQueued int) (inFlightAge, oldestAge time.Duration, ok bool) {
	prefix := "attention: messages pending for " + wantAddress + ": in-flight "
	rest, ok := strings.CutPrefix(line, prefix)
	if !ok {
		return 0, 0, false
	}
	mid := " (message " + wantInFlightID + "), queued " + strconv.Itoa(wantQueued) + ", oldest "
	inFlightStr, oldestStr, ok := strings.Cut(rest, mid)
	if !ok {
		return 0, 0, false
	}
	inFlightAge, err := time.ParseDuration(inFlightStr)
	if err != nil {
		return 0, 0, false
	}
	oldestAge, err = time.ParseDuration(oldestStr)
	if err != nil {
		return 0, 0, false
	}
	return inFlightAge, oldestAge, true
}

// requireAttentionLineBothClausesExact scans one full "hop status -run"
// rendering for the ONE line matching attentionLineBothClausesExact,
// failing the test loudly (naming the full output) if no line matches --
// never a loose Contains check on the rendering as a whole.
func requireAttentionLineBothClausesExact(t *testing.T, statusOutput, wantAddress, wantInFlightID string, wantQueued int) (inFlightAge, oldestAge time.Duration) {
	t.Helper()
	for _, line := range strings.Split(statusOutput, "\n") {
		if age1, age2, ok := attentionLineBothClausesExact(strings.TrimSpace(line), wantAddress, wantInFlightID, wantQueued); ok {
			return age1, age2
		}
	}
	t.Fatalf("hop status output carries no attention line matching address=%q in-flight=%q queued=%d; full output:\n%s",
		wantAddress, wantInFlightID, wantQueued, statusOutput)
	return 0, 0
}

// requireNoAttentionLineForAddress asserts one full "hop status -run"
// rendering carries no attention line for wantAddress at all -- the
// mailbox condition (design section 7) has fully cleared. Checked against
// the full output, never just a first line or a single field.
func requireNoAttentionLineForAddress(t *testing.T, statusOutput, wantAddress string) {
	t.Helper()
	prefix := "attention: messages pending for " + wantAddress + ":"
	for _, line := range strings.Split(statusOutput, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			t.Fatalf("hop status output still carries an attention line for %s after it should have cleared: %q; full output:\n%s", wantAddress, line, statusOutput)
		}
	}
}
