package integration

import (
	"fmt"
	"os"
	"strings"
	"testing"
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
// validates only the session id, its current incarnation and the run it
// belongs to (internal/app/usecase_message.go's SendMessage,
// internal/adapters/sqlite/messaging.go's SendMessage) -- never which OS
// process happened to invoke the CLI. Shared by
// injectionfreedelivery_test.go and mailboxclosurerace_test.go.

// managerEnv returns the environment for a one-shot hop CLI invocation
// authenticated as this run's own manager session.
func (f *featureRun) managerEnv(t *testing.T) []string {
	t.Helper()
	sessionID := f.managerSessionID(t)
	return append(append([]string{}, f.env...),
		"HOP_RUN_ID="+f.runID,
		"HOP_SESSION_ID="+sessionID,
		"HOP_INCARNATION_ID="+f.incarnationForSession(t, sessionID),
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
