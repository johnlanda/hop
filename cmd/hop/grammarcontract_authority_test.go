package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/testsupport/hopfixtures"
)

// TestGrammarContractResultSubmitSupersededIncarnationIsStale drives, through
// the real binary, a worker whose binding was superseded with no successor
// while a message is queued to its task. `hop result submit` prints the
// final `stale: …` line and exits 1, on the first submission and on a
// retry — never the drain line, which would loop a caller whose own
// `hop msg next` is refused.
func TestGrammarContractResultSubmitSupersededIncarnationIsStale(t *testing.T) {
	f := newFeatureManager(t, 10200, defaultMessageWait)
	worker := f.addActiveChild(t, 10300, "implement", "", "", true)
	requireSentOnly(t, "msg send (manager info to the task)", execHop(t, f.env(nil), f.StateRoot, "msg", "send", "--to", "task:"+worker.TaskID, "--kind", "info", "--body", "read this first"))
	if err := hopfixtures.SupersedeBinding(context.Background(), f.store, f.lease, worker.SessionID, time.Now().UTC()); err != nil {
		t.Fatalf("supersede worker binding: %v", err)
	}

	for _, attempt := range []string{"first", "retried"} {
		result := execHop(t, worker.env(f, nil), f.StateRoot, "result", "submit", "--summary", "implemented the fixture change", "--commit", strings.Repeat("c", 40))
		if result.ExitCode != exitFailure || !strings.HasPrefix(result.Stdout, "stale: ") || strings.Count(result.Stdout, "\n") != 1 || result.Stderr != "" {
			t.Fatalf("result submit (%s): exit=%d stdout=%q stderr=%q, want exit %d and exactly one `stale: ` line", attempt, result.ExitCode, result.Stdout, result.Stderr, exitFailure)
		}
		if strings.HasPrefix(result.Stdout, app.GrammarTransientPrefix) {
			t.Fatalf("result submit (%s): stdout = %q, a stale caller must never get a retry line", attempt, result.Stdout)
		}
	}

	next := execHop(t, worker.env(f, nil), f.StateRoot, "msg", "next")
	if next.ExitCode != exitFailure || next.Stdout != "" || !strings.HasPrefix(next.Stderr, "hop msg next: ") {
		t.Fatalf("msg next (superseded worker): exit=%d stdout=%q stderr=%q, want a refusal on stderr only", next.ExitCode, next.Stdout, next.Stderr)
	}
}

// messageDeliveryCount reads, through a read-only raw query, how many
// delivery rows messageID has.
func messageDeliveryCount(t *testing.T, stateRoot, messageID string) int {
	t.Helper()
	return readOnlyCount(t, stateRoot, `SELECT COUNT(*) FROM message_deliveries WHERE message_id = ?`, messageID)
}

// taskMessageCount reads, through a read-only raw query, how many messages
// address taskID.
func taskMessageCount(t *testing.T, stateRoot, taskID string) int {
	t.Helper()
	return readOnlyCount(t, stateRoot, `SELECT COUNT(*) FROM messages WHERE recipient_address = ?`, "task:"+taskID)
}

// readOnlyCount runs one COUNT query against the fixture store through a
// read-only raw connection.
func readOnlyCount(t *testing.T, stateRoot, query string, args ...any) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(stateRoot, "hop.db")+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw db for a read: %v", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("close raw db after a read: %v", closeErr)
		}
	}()
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("read-only count: %v", err)
	}
	return n
}

// TestGrammarContractMsgNextRetiredAttemptSessionIsRefused drives the task
// address's fetch authority through the real binary. Attempt 1's worker was
// settled by the worker-termination shape (attempt interrupted, session
// terminated, binding left current) and the retry's attempt 2 runs behind
// its own session when the manager queues an info to the task. The old
// worker's `hop msg next` and `hop msg wait` print nothing on stdout, one
// value-free diagnostic on stderr, and exit 1, with nothing served; its
// `hop msg ack` is refused not-delivered; the successor is then served the
// info as its first delivery and acks it.
func TestGrammarContractMsgNextRetiredAttemptSessionIsRefused(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	f := newFeatureManager(t, 10400, defaultMessageWait)
	old := f.addActiveChild(t, 10500, "implement", "", "", true)
	if err := hopfixtures.SettleWorkerTermination(ctx, f.store, f.lease, old.TaskID, old.AttemptID, old.SessionID, now); err != nil {
		t.Fatalf("settle worker termination: %v", err)
	}
	if err := hopfixtures.ConsumeRetry(ctx, f.store, f.lease, old.TaskID, now); err != nil {
		t.Fatalf("consume retry: %v", err)
	}
	sessionID, incarnationID, attemptID, err := hopfixtures.SeedChildSession(ctx, f.store, f.lease, f.RunID, old.TaskID, f.ManagerID, "implementer", 10600, now)
	if err != nil {
		t.Fatalf("seed successor session: %v", err)
	}
	if err := hopfixtures.RunAttempt(ctx, f.store, f.lease, attemptID, now); err != nil {
		t.Fatalf("run successor attempt: %v", err)
	}
	successor := childSession{TaskID: old.TaskID, AttemptID: attemptID, SessionID: sessionID, IncarnationID: incarnationID}
	infoID := requireSentOnly(t, "msg send (manager info to the task)", execHop(t, f.env(nil), f.StateRoot, "msg", "send", "--to", "task:"+old.TaskID, "--kind", "info", "--body", "for the retry"))

	for _, verb := range [][]string{{"msg", "next"}, {"msg", "wait", "--timeout", "2s"}} {
		label := "hop " + strings.Join(verb[:2], " ")
		result := execHop(t, old.env(f, nil), f.StateRoot, verb...)
		if result.ExitCode != exitFailure || result.Stdout != "" || strings.Count(result.Stderr, "\n") != 1 || !strings.HasPrefix(result.Stderr, label+": ") {
			t.Fatalf("%s (retired worker): exit=%d stdout=%q stderr=%q, want exit %d, no stdout and one %q line", label, result.ExitCode, result.Stdout, result.Stderr, exitFailure, label+": ")
		}
		if !strings.Contains(result.Stderr, "session is not its task's current attempt session") ||
			strings.Contains(result.Stderr, old.SessionID) || strings.Contains(result.Stderr, old.IncarnationID) {
			t.Fatalf("%s (retired worker): stderr = %q, want the value-free refusal", label, result.Stderr)
		}
	}
	if n := messageDeliveryCount(t, f.StateRoot, infoID); n != 0 {
		t.Fatalf("delivery rows after the refused fetches = %d, want 0", n)
	}
	if ack := execHop(t, old.env(f, nil), f.StateRoot, "msg", "ack", infoID); ack.ExitCode != exitFailure || ack.FirstStdoutLine() != app.GrammarRefusalLine(app.GrammarReasonNotDelivered) {
		t.Fatalf("msg ack (retired worker): exit=%d stdout=%q stderr=%q, want %q", ack.ExitCode, ack.Stdout, ack.Stderr, app.GrammarRefusalLine(app.GrammarReasonNotDelivered))
	}

	next := execHop(t, successor.env(f, nil), f.StateRoot, "msg", "next")
	if want := app.GrammarMessageLine(infoID, "info", f.ManagerID, "", "", ""); next.FirstStdoutLine() != want {
		t.Fatalf("msg next (successor) first line = %q, want %q; stderr=%q", next.FirstStdoutLine(), want, next.Stderr)
	}
	if n := messageDeliveryCount(t, f.StateRoot, infoID); n != 1 {
		t.Fatalf("delivery rows after the successor's fetch = %d, want 1", n)
	}
	if ack := execHop(t, successor.env(f, nil), f.StateRoot, "msg", "ack", infoID); ack.Stdout != app.GrammarAckAcceptedLine(infoID)+"\n" {
		t.Fatalf("msg ack (successor): exit=%d stdout=%q stderr=%q", ack.ExitCode, ack.Stdout, ack.Stderr)
	}
}

// TestGrammarContractMsgSendRetiredAttemptSessionIsStale drives the one
// currency rule for sends through the real binary. The manager asks the
// task a question and attempt 1's worker is served it; the attempt is then
// settled by the worker-termination shape (binding left current) and the
// retry's attempt 2 runs behind its own session. The old worker's answer
// to that question, and its question to the manager, each print exactly
// `refused: stale` and the value-free detail, exit 1 and create no
// envelope; the successor is re-served the question and its answer is
// sent.
func TestGrammarContractMsgSendRetiredAttemptSessionIsStale(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	f := newFeatureManager(t, 10700, defaultMessageWait)
	old := f.addActiveChild(t, 10800, "implement", "", "", true)
	questionID := requireSentOnly(t, "msg send (manager question to the task)", execHop(t, f.env(nil), f.StateRoot, "msg", "send", "--to", "task:"+old.TaskID, "--kind", "question", "--body", "which approach?"))
	if next := execHop(t, old.env(f, nil), f.StateRoot, "msg", "next"); next.FirstStdoutLine() != app.GrammarMessageLine(questionID, "question", f.ManagerID, "", "", "") {
		t.Fatalf("msg next (worker while current): stdout=%q stderr=%q", next.Stdout, next.Stderr)
	}
	if err := hopfixtures.SettleWorkerTermination(ctx, f.store, f.lease, old.TaskID, old.AttemptID, old.SessionID, now); err != nil {
		t.Fatalf("settle worker termination: %v", err)
	}
	if err := hopfixtures.ConsumeRetry(ctx, f.store, f.lease, old.TaskID, now); err != nil {
		t.Fatalf("consume retry: %v", err)
	}
	sessionID, incarnationID, attemptID, err := hopfixtures.SeedChildSession(ctx, f.store, f.lease, f.RunID, old.TaskID, f.ManagerID, "implementer", 10900, now)
	if err != nil {
		t.Fatalf("seed successor session: %v", err)
	}
	if err := hopfixtures.RunAttempt(ctx, f.store, f.lease, attemptID, now); err != nil {
		t.Fatalf("run successor attempt: %v", err)
	}
	successor := childSession{TaskID: old.TaskID, AttemptID: attemptID, SessionID: sessionID, IncarnationID: incarnationID}

	const detail = "session is not its task's current attempt session"
	requireRefusedOnly(t, "msg send --kind answer (retired worker)",
		execHop(t, old.env(f, nil), f.StateRoot, "msg", "send", "--kind", "answer", "--reply-to", questionID, "--body", "a stale answer"),
		app.GrammarReasonStale, detail)
	requireRefusedOnly(t, "msg send --kind question (retired worker)",
		execHop(t, old.env(f, nil), f.StateRoot, "msg", "send", "--to", "manager", "--kind", "question", "--body", "a stale question"),
		app.GrammarReasonStale, detail)
	if n := readOnlyCount(t, f.StateRoot, `SELECT COUNT(*) FROM messages`); n != 1 {
		t.Fatalf("envelopes after the refused sends = %d, want only the manager's question", n)
	}

	if next := execHop(t, successor.env(f, nil), f.StateRoot, "msg", "next"); next.FirstStdoutLine() != app.GrammarMessageLine(questionID, "question", f.ManagerID, "", "", "") {
		t.Fatalf("msg next (successor): stdout=%q stderr=%q, want the question re-served", next.Stdout, next.Stderr)
	}
	requireSentOnly(t, "msg send --kind answer (successor)",
		execHop(t, successor.env(f, nil), f.StateRoot, "msg", "send", "--kind", "answer", "--reply-to", questionID, "--body", "the current answer"))
}
