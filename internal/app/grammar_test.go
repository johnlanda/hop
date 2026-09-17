package app_test

import (
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/run"
)

// TestGoldenGrammar pins the section 7 worker-protocol grammar byte for
// byte: every constant and every rendered line is compared against a
// golden literal retyped here, never derived from the constants under
// test, so a drift in grammar.go fails this table before it can reach a
// template, cmd/hop or a fixture. The template-quotation half of the
// countermeasure lives in TestTemplatesQuoteGrammar; the real-binary
// per-verb contract tests are slice 6's (design section 11, L1569) and
// assert the same constants against the built hop binary.
func TestGoldenGrammar(t *testing.T) {
	const (
		msgID    = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		otherID  = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
		thirdID  = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
		senderID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	)
	at := time.Date(2026, 9, 15, 14, 30, 5, 0, time.FixedZone("PDT", -7*3600))

	golden := []struct {
		name string
		got  string
		want string
	}{
		{"verb result submit", app.GrammarVerbResultSubmit, "result submit"},
		{"verb msg next", app.GrammarVerbMsgNext, "msg next"},
		{"verb msg wait", app.GrammarVerbMsgWait, "msg wait"},
		{"verb msg show", app.GrammarVerbMsgShow, "msg show"},
		{"verb msg ack", app.GrammarVerbMsgAck, "msg ack"},
		{"verb msg send", app.GrammarVerbMsgSend, "msg send"},
		{"verb task create", app.GrammarVerbTaskCreate, "task create"},
		{"verb task retry", app.GrammarVerbTaskRetry, "task retry"},
		{"verb plan close", app.GrammarVerbPlanClose, "plan close"},
		{"verb review submit", app.GrammarVerbReviewSubmit, "review submit"},
		{"verb answer", app.GrammarVerbAnswer, "answer"},

		{"transient not running", app.GrammarTransientNotRunningLine, "transient: attempt not yet running; retry"},
		{"transient undelivered", app.GrammarTransientUndeliveredLine, "transient: undelivered messages; drain with hop msg next, ack, then resubmit"},
		{"transient run not running", app.GrammarTransientRunNotRunningLine, "transient: run not yet running; retry"},
		{"transient prefix", app.GrammarTransientPrefix, "transient: "},
		{"msg none", app.GrammarMsgNoneLine, "none: no queued message"},
		{"plan closed", app.GrammarPlanClosedLine, "plan closed"},
		{"plan close duplicate", app.GrammarPlanCloseDuplicateLine, "duplicate plan closed"},
		{"refusal prefix", app.GrammarRefusalPrefix, "refused: "},

		{"reason not-found", app.GrammarReasonNotFound, "not-found"},
		{"reason unauthorized", app.GrammarReasonUnauthorized, "unauthorized"},
		{"reason malformed", app.GrammarReasonMalformed, "malformed"},
		{"reason conflicting", app.GrammarReasonConflicting, "conflicting"},
		{"reason stale", app.GrammarReasonStale, "stale"},
		{"reason not-delivered", app.GrammarReasonNotDelivered, "not-delivered"},
		{"reason run-not-accepting", app.GrammarReasonRunNotAccepting, "run-not-accepting"},
		{"reason mailbox-closed", app.GrammarReasonMailboxClosed, "mailbox-closed"},
		{"reason not-manager", app.GrammarReasonNotManager, "not-manager"},
		{"reason not-reviewer", app.GrammarReasonNotReviewer, "not-reviewer"},
		{"reason dependency-cycle", app.GrammarReasonDependencyCycle, "dependency-cycle"},
		{"reason empty-plan", app.GrammarReasonEmptyPlan, "empty-plan"},
		{"reason retry-not-terminal", app.GrammarReasonRetryNotTerminal, "retry-not-terminal"},
		{"reason retry-limit", app.GrammarReasonRetryLimit, "retry-limit"},
		{"reason subject-mismatch", app.GrammarReasonSubjectMismatch, "subject-mismatch"},

		{"refusal line", app.GrammarRefusalLine(app.GrammarReasonNotFound), "refused: not-found"},

		{"principal session", app.GrammarPrincipal("session", senderID), senderID},
		{"principal human", app.GrammarPrincipal("human", ""), "human"},
		{"principal controller", app.GrammarPrincipal("controller", ""), "controller"},
		{"time renders UTC RFC 3339", app.GrammarTime(at), "2026-09-15T21:30:05Z"},

		{"result accepted", app.GrammarResultAcceptedLine(msgID), "accepted " + msgID},
		{"result duplicate", app.GrammarResultDuplicateLine(msgID), "duplicate " + msgID},
		{"result stale", app.GrammarResultRefusalLine(app.GrammarReasonStale, "incarnation is not current"), "stale: incarnation is not current"},
		{"result refusal without detail", app.GrammarResultRefusalLine(app.GrammarReasonMalformed, ""), "malformed"},

		{
			"message line minimal",
			app.GrammarMessageLine(msgID, "info", senderID, "", "", ""),
			"message " + msgID + " kind=info from=" + senderID,
		},
		{
			"message line every optional field",
			app.GrammarMessageLine(msgID, "answer", "human", otherID, thirdID, senderID),
			"message " + msgID + " kind=answer from=human reply-to=" + otherID + " relay-of=" + thirdID + " origin=" + senderID,
		},
		{"body line", app.GrammarBodyLine("/state/runs/r/messages/m.md"), "body: /state/runs/r/messages/m.md"},
		{"ack hint", app.GrammarAckHintLine(msgID), "ack: hop msg ack " + msgID},
		{"wait none", app.GrammarMsgWaitNoneLine(50 * time.Second), "none: no message within 50s; run hop msg wait again"},

		{
			"show line minimal",
			app.GrammarMessageShowLine(msgID, "question", senderID, "manager", "", "", 3),
			"message " + msgID + " kind=question from=" + senderID + " to=manager seq=3",
		},
		{
			"show line with provenance",
			app.GrammarMessageShowLine(msgID, "question", senderID, "human", "", otherID, 7),
			"message " + msgID + " kind=question from=" + senderID + " to=human relay-of=" + otherID + " seq=7",
		},
		{"delivered line", app.GrammarDeliveredLine(senderID, at), "delivered: " + senderID + " 2026-09-15T21:30:05Z"},
		{"acknowledged line", app.GrammarAcknowledgedLine(at), "acknowledged: 2026-09-15T21:30:05Z"},

		{"ack accepted", app.GrammarAckAcceptedLine(msgID), "acknowledged " + msgID},
		{"ack duplicate", app.GrammarAckDuplicateLine(msgID), "duplicate " + msgID},
		{"sent", app.GrammarSentLine(msgID), "sent " + msgID},
		{"send duplicate", app.GrammarSendDuplicateLine(msgID), "duplicate " + msgID},

		{"task created", app.GrammarTaskCreatedLine(msgID, 4), "task " + msgID + " t4 created"},
		{"task create duplicate", app.GrammarTaskCreateDuplicateLine(msgID, 4), "duplicate " + msgID + " t4"},
		{"retry accepted", app.GrammarRetryAcceptedLine(2, 3), "retry accepted t2 attempt 3"},
		{"retry duplicate", app.GrammarRetryDuplicateLine(2, 3), "duplicate t2 attempt 3"},
		{"verdict accepted", app.GrammarVerdictAcceptedLine(msgID), "verdict accepted " + msgID},
		{"verdict duplicate", app.GrammarVerdictDuplicateLine(msgID), "duplicate " + msgID},

		{
			"attention line, in-flight and queued",
			app.GrammarAttentionLine("manager", msgID, 5*time.Minute, 2, time.Hour),
			"attention: messages pending for manager: in-flight 5m0s (message " + msgID + "), queued 2, oldest 1h0m0s",
		},
		{
			"attention line, queued only",
			app.GrammarAttentionLine("human", "", 0, 1, 90*time.Second),
			"attention: messages pending for human: queued 1, oldest 1m30s",
		},
		{
			"attention line, in-flight only",
			app.GrammarAttentionLine("task:"+msgID+" (t3)", msgID, 30*time.Second, 0, 0),
			"attention: messages pending for task:" + msgID + " (t3): in-flight 30s (message " + msgID + ")",
		},
		{"task address", app.GrammarTaskAddress(msgID, "t3"), "task:" + msgID + " (t3)"},
		{
			"attention action session, no binding",
			app.GrammarAttentionActionSession(""),
			"open that session's pane and check that the agent is following its polling instructions",
		},
		{
			"attention action session, with binding",
			app.GrammarAttentionActionSession("ws/tab/pane"),
			"open ws/tab/pane and check that the agent is following its polling instructions",
		},
		{"attention action human", app.GrammarAttentionActionHuman, "answer pending human questions with hop answer"},
		{"attention marker", app.GrammarAttentionMarker, "blocked, needs attention"},

		{"task label", app.GrammarTaskLabel(4), "t4"},
		{
			"task line",
			app.GrammarTaskLine("t1", msgID, "implement", "active", "t2,t3", 2, "/worktrees/t1a2"),
			"task t1 " + msgID + ": kind=implement state=active deps=t2,t3 attempts=2 worktree=/worktrees/t1a2",
		},
		{
			"task line, no deps or worktree",
			app.GrammarTaskLine("t1", msgID, "implement", "pending", "(none)", 0, "(none)"),
			"task t1 " + msgID + ": kind=implement state=pending deps=(none) attempts=0 worktree=(none)",
		},

		{
			"integration line",
			app.GrammarIntegrationLine(msgID, "t1", "integrated", "srcoid", "premoid", "mergeoid"),
			"integration " + msgID + ": task=t1 state=integrated source=srcoid premerge=premoid merge=mergeoid",
		},

		{"shortfall verdict-rejected token", app.GrammarShortfallVerdictRejected, "verdict-rejected"},
		{"shortfall token matches the domain", app.GrammarShortfallVerdictRejected, string(run.ShortfallVerdictRejected)},
		{"shortfall evidence-inconsistent token", app.GrammarShortfallEvidenceInconsistent, "evidence-inconsistent"},
		{"evidence-inconsistent token matches the read model", app.GrammarShortfallEvidenceInconsistent, string(app.ShortfallEvidenceInconsistent)},
		{
			"shortfall line, evidence-inconsistent",
			app.GrammarShortfallLine(app.GrammarShortfallEvidenceInconsistent, "", ""),
			"shortfall: evidence-inconsistent",
		},
		{
			"shortfall line, no task",
			app.GrammarShortfallLine("plan-open", "", ""),
			"shortfall: plan-open",
		},
		{
			"shortfall line, with task",
			app.GrammarShortfallLine("task-not-integrated", "t2", msgID),
			"shortfall: task-not-integrated t2 " + msgID,
		},

		{
			"question line",
			app.GrammarQuestionLine(msgID, 90*time.Second, "/state/runs/r/messages/m.md"),
			"question " + msgID + " age=1m30s body: /state/runs/r/messages/m.md",
		},
		{"answer invocation", app.GrammarAnswerInvocationLine(msgID), "hop answer " + msgID + " --file <path>"},

		{
			"session line",
			app.GrammarSessionLine(msgID, "implementer", "active", "t2", 1, "ws/tab/pane"),
			"session " + msgID + ": role=implementer state=active task=t2 attempt=1 binding=ws/tab/pane",
		},
		{
			"session line, manager",
			app.GrammarSessionLine(msgID, "manager", "active", "(none)", 0, "ws/tab/pane"),
			"session " + msgID + ": role=manager state=active task=(none) attempt=0 binding=ws/tab/pane",
		},
		{
			"session launch-corroboration action",
			app.GrammarSessionLaunchCorroborationAction,
			"another process on this session's pane carries the launch identity, so the launch is not corroborated yet; " +
				"the controller re-inspects it every pass and needs nothing, and if this state persists, open that pane and check whether the harness was started through a process that forks it",
		},
	}
	for _, tc := range golden {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("rendered %q, golden %q", tc.got, tc.want)
			}
		})
	}

	transientLines := []struct {
		reason app.TransientReason
		want   string
		known  bool
	}{
		{"attempt-not-running", "transient: attempt not yet running; retry", true},
		{"undelivered-messages", "transient: undelivered messages; drain with hop msg next, ack, then resubmit", true},
		{"", "", false},
		{"run-not-running", "", false},
	}
	for _, tc := range transientLines {
		t.Run("submission transient line for "+string(tc.reason), func(t *testing.T) {
			got, known := app.GrammarSubmissionTransientLine(tc.reason)
			if got != tc.want || known != tc.known {
				t.Errorf("GrammarSubmissionTransientLine(%q) = %q, %t; golden %q, %t", tc.reason, got, known, tc.want, tc.known)
			}
		})
	}
	if app.TransientAttemptNotRunning != "attempt-not-running" || app.TransientUndeliveredMessages != "undelivered-messages" {
		t.Errorf("transient reasons = %q, %q; golden %q, %q", app.TransientAttemptNotRunning, app.TransientUndeliveredMessages, "attempt-not-running", "undelivered-messages")
	}
}
