package app_test

import (
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
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
	}
	for _, tc := range golden {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("rendered %q, golden %q", tc.got, tc.want)
			}
		})
	}
}
