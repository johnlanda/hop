package main

import (
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

// requireRefusedOnly proves an invocation printed exactly the grammar's
// refusal lines — `refused: <token>` then detail — exited 1 and wrote
// nothing to stderr.
func requireRefusedOnly(t *testing.T, label string, result hopResult, token, detail string) {
	t.Helper()
	want := app.GrammarRefusalLine(token) + "\n" + detail + "\n"
	if result.Stdout != want || result.Stderr != "" || result.ExitCode != exitFailure {
		t.Fatalf("%s: exit=%d stdout=%q stderr=%q, want exit %d, stdout %q and an empty stderr", label, result.ExitCode, result.Stdout, result.Stderr, exitFailure, want)
	}
}

// requireNotAcknowledged proves hop msg show renders messageID with no
// acknowledged line.
func requireNotAcknowledged(t *testing.T, f *featureManager, label, messageID string) {
	t.Helper()
	show := execHop(t, map[string]string{"HOP_STATE_DIR": f.StateRoot}, f.StateRoot, "msg", "show", "--run", f.RunID, messageID)
	if show.ExitCode != exitOK {
		t.Fatalf("%s: msg show exit=%d stdout=%q stderr=%q", label, show.ExitCode, show.Stdout, show.Stderr)
	}
	if strings.Contains(show.Stdout, "acknowledged: ") {
		t.Fatalf("%s: msg show = %q, want the message unacknowledged", label, show.Stdout)
	}
}

// TestGrammarContractAnswerHumanQuestionUnauthorized drives section 7's
// answer authority through the real binary: the worker whose question the
// manager relayed to the human cannot answer that human-addressed relay
// with hop msg send --kind answer, and neither can the manager — each
// prints exactly `refused: unauthorized` and its detail with exit 1, the
// relay stays unacknowledged — and the human's hop answer is then
// accepted.
func TestGrammarContractAnswerHumanQuestionUnauthorized(t *testing.T) {
	f := newFeatureManager(t, 9000, defaultMessageWait)
	worker := f.addImplementTask(t, 9100, "ask the human")

	questionID := requireSentOnly(t, "msg send (worker question)", execHop(t, worker.env(f, nil), f.StateRoot, "msg", "send", "--to", "manager", "--kind", "question", "--body", "which option?"))
	if next := execHop(t, f.env(nil), f.StateRoot, "msg", "next"); next.ExitCode != exitOK {
		t.Fatalf("msg next (manager): exit=%d stdout=%q stderr=%q", next.ExitCode, next.Stdout, next.Stderr)
	}
	if ack := execHop(t, f.env(nil), f.StateRoot, "msg", "ack", questionID); ack.ExitCode != exitOK {
		t.Fatalf("msg ack (manager): exit=%d stdout=%q stderr=%q", ack.ExitCode, ack.Stdout, ack.Stderr)
	}
	relayID := requireSentOnly(t, "msg send (relay to human)", execHop(t, f.env(nil), f.StateRoot, "msg", "send", "--to", "human", "--kind", "question", "--relay-of", questionID, "--body", "the worker asks which option"))

	const detail = "session is not the question's recipient"
	for _, attempt := range []struct {
		label string
		env   map[string]string
	}{
		{"worker answers the human relay", worker.env(f, nil)},
		{"worker retries the same request", worker.env(f, nil)},
		{"manager answers its own relay", f.env(nil)},
	} {
		result := execHop(t, attempt.env, f.StateRoot, "msg", "send", "--kind", "answer", "--reply-to", relayID, "--body", "option A", "--request-id", "stolen-answer")
		requireRefusedOnly(t, attempt.label, result, app.GrammarReasonUnauthorized, detail)
	}
	requireNotAcknowledged(t, f, "the relay after the refused session answers", relayID)

	answer := execHop(t, map[string]string{"HOP_STATE_DIR": f.StateRoot}, f.RepositoryRoot, "answer", "-C", f.RepositoryRoot, "--run", f.RunID, "--body", "option B", "--request-id", "human-answer", relayID)
	answerID := requireSentOnly(t, "answer (human)", answer)
	next := execHop(t, f.env(nil), f.StateRoot, "msg", "next")
	if want := app.GrammarMessageLine(answerID, "answer", "human", relayID, "", questionID); next.FirstStdoutLine() != want {
		t.Fatalf("msg next (manager) first line = %q, want %q; stdout=%q stderr=%q", next.FirstStdoutLine(), want, next.Stdout, next.Stderr)
	}
}

// TestGrammarContractAnswerAnotherTaskUnauthorized proves a worker cannot
// answer a question the manager addressed to a different task: `refused:
// unauthorized` through the real binary, while the addressed worker's
// answer is sent.
func TestGrammarContractAnswerAnotherTaskUnauthorized(t *testing.T) {
	f := newFeatureManager(t, 9200, defaultMessageWait)
	addressed := f.addImplementTask(t, 9300, "the addressed task")
	other := f.addImplementTask(t, 9400, "another task")

	questionID := requireSentOnly(t, "msg send (manager question)", execHop(t, f.env(nil), f.StateRoot, "msg", "send", "--to", "task:"+addressed.TaskID, "--kind", "question", "--body", "status?"))

	stolen := execHop(t, other.env(f, nil), f.StateRoot, "msg", "send", "--kind", "answer", "--reply-to", questionID, "--body", "done")
	requireRefusedOnly(t, "another task's worker answers", stolen, app.GrammarReasonUnauthorized, "session is not the question's recipient")

	requireSentOnly(t, "msg send (addressed worker answers)", execHop(t, addressed.env(f, nil), f.StateRoot, "msg", "send", "--kind", "answer", "--reply-to", questionID, "--body", "done"))
}

// TestGrammarContractAnswerClosedMailbox drives section 5's admission rule
// for answers through the real binary: the reviewer asks the manager a
// question, its verdict is accepted over its empty mailbox (closing it),
// and the manager's later answer prints exactly `refused: mailbox-closed`
// with exit 1 and never reaches the reviewer's queue.
func TestGrammarContractAnswerClosedMailbox(t *testing.T) {
	rf := newReviewFixture(t, 9500)
	reviewerEnv := rf.reviewer.env(rf.featureManager, nil)

	questionID := requireSentOnly(t, "msg send (reviewer question)", execHop(t, reviewerEnv, rf.StateRoot, "msg", "send", "--to", "manager", "--kind", "question", "--body", "is the subject final?"))
	verdict := rf.submit(t, "approve", rf.subjectCommit, writeReasonsFile(t, rf.StateRoot))
	if verdict.ExitCode != exitOK || !strings.HasPrefix(verdict.FirstStdoutLine(), "verdict accepted ") {
		t.Fatalf("review submit: exit=%d stdout=%q stderr=%q, want the verdict accepted", verdict.ExitCode, verdict.Stdout, verdict.Stderr)
	}

	for _, label := range []string{"manager answers after the verdict", "manager retries the same request"} {
		result := execHop(t, rf.env(nil), rf.StateRoot, "msg", "send", "--kind", "answer", "--reply-to", questionID, "--body", "yes", "--request-id", "late-answer")
		requireRefusedOnly(t, label, result, app.GrammarReasonMailboxClosed, "mailbox is closed")
	}
	none := execHop(t, reviewerEnv, rf.StateRoot, "msg", "next")
	if none.Stdout != app.GrammarMsgNoneLine+"\n" || none.ExitCode != exitOK {
		t.Fatalf("msg next (reviewer) after the refused answer: exit=%d stdout=%q stderr=%q, want %q", none.ExitCode, none.Stdout, none.Stderr, app.GrammarMsgNoneLine)
	}
}
