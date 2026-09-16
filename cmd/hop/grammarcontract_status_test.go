package main

import (
	"strings"
	"testing"
)

// TestGrammarContractStatusFeatureDetailSections drives hop status -run's
// section 10/section 7 feature-mode detail block through the built binary
// end to end: a shortfall naming a task (task-not-integrated, an implement
// task seeded ready) alongside one that never names a task
// (verdict-rejected, produced by a REAL hop review submit --verdict
// reject), a mailbox attention line (a REAL hop msg send left queued and
// undelivered), and a pending human question with its exact hop answer
// invocation. Crossing the run's frozen [messages] attention_after
// threshold to also prove the nested action line and the listing's
// "blocked, needs attention" marker is left to the in-process unit tests
// (cmd/hop's own TestRunStatusFeatureDetailBlock and internal/app's
// fake-store TestStatusMailboxesAndAttention): this suite has no seam to
// fast-forward the exec'd binary's own wall clock, and the attention
// LINE itself (this test's concern) renders unconditionally on a
// non-empty queue regardless of age.
func TestGrammarContractStatusFeatureDetailSections(t *testing.T) {
	rf := newReviewFixture(t, 7700)
	reasons := writeReasonsFile(t, rf.StateRoot)

	impl := rf.addImplementTask(t, 7710, "implement this")

	rejected := rf.submit(t, "reject", rf.subjectCommit, reasons)
	if rejected.ExitCode != exitOK || !strings.HasPrefix(rejected.FirstStdoutLine(), "verdict accepted ") {
		t.Fatalf("review submit reject: exit=%d stdout=%q stderr=%q", rejected.ExitCode, rejected.Stdout, rejected.Stderr)
	}

	queued := execHop(t, rf.env(nil), rf.StateRoot, "msg", "send", "--to", "task:"+impl.TaskID, "--kind", "info", "--body", "fyi")
	if queued.ExitCode != exitOK {
		t.Fatalf("msg send (task mailbox): exit=%d stdout=%q stderr=%q", queued.ExitCode, queued.Stdout, queued.Stderr)
	}

	question := execHop(t, rf.env(nil), rf.StateRoot, "msg", "send", "--to", "human", "--kind", "question", "--body", "proceed?")
	if question.ExitCode != exitOK {
		t.Fatalf("msg send (human question): exit=%d stdout=%q stderr=%q", question.ExitCode, question.Stdout, question.Stderr)
	}
	questionID := strings.TrimPrefix(question.FirstStdoutLine(), "sent ")

	detail := execHop(t, map[string]string{"HOP_STATE_DIR": rf.StateRoot}, rf.RepositoryRoot, "status", "-C", rf.RepositoryRoot, "-run", rf.RunID)
	if detail.ExitCode != exitOK {
		t.Fatalf("status -run: exit=%d stdout=%q stderr=%q", detail.ExitCode, detail.Stdout, detail.Stderr)
	}
	out := detail.Stdout

	for _, want := range []string{
		"task t7710 " + impl.TaskID + ": kind=implement state=ready",
		"shortfall: task-not-integrated t7710 " + impl.TaskID,
		"shortfall: verdict-rejected",
		"attention: messages pending for task:" + impl.TaskID + " (t7710): queued 1, oldest ",
		"question " + questionID,
		"hop answer " + questionID + " --file <path>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status -run output missing %q; got:\n%s", want, out)
		}
	}
}
