package run_test

import (
	"errors"
	"testing"

	"github.com/johnlanda/hop/internal/domain/run"
)

func TestValidateSendAddressing(t *testing.T) {
	cases := []struct {
		name      string
		sender    run.Address
		kind      run.MessageKind
		recipient run.Address
		wantOK    bool
	}{
		{"manager question to human", run.ManagerAddress(), run.MessageQuestion, run.HumanAddress(), true},
		{"manager info to human refused", run.ManagerAddress(), run.MessageInfo, run.HumanAddress(), false},
		{"manager question to task", run.ManagerAddress(), run.MessageQuestion, run.TaskAddress(testTaskID), true},
		{"manager info to task", run.ManagerAddress(), run.MessageInfo, run.TaskAddress(testTaskID), true},
		{"manager to manager refused", run.ManagerAddress(), run.MessageQuestion, run.ManagerAddress(), false},
		{"task question to manager", run.TaskAddress(testTaskID), run.MessageQuestion, run.ManagerAddress(), true},
		{"task info to manager", run.TaskAddress(testTaskID), run.MessageInfo, run.ManagerAddress(), true},
		{"task question to human refused", run.TaskAddress(testTaskID), run.MessageQuestion, run.HumanAddress(), false},
		{"task question to task refused", run.TaskAddress(testTaskID), run.MessageQuestion, run.TaskAddress(testSecondTaskID), false},
		{"human sender never validated here", run.HumanAddress(), run.MessageQuestion, run.ManagerAddress(), false},
		{"answer never validated here", run.ManagerAddress(), run.MessageAnswer, run.TaskAddress(testTaskID), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := run.ValidateSendAddressing(tc.sender, tc.kind, tc.recipient)
			if tc.wantOK && err != nil {
				t.Fatalf("ValidateSendAddressing: unexpected error: %v", err)
			}
			if !tc.wantOK && !errors.Is(err, run.ErrInvalidTransition) {
				t.Fatalf("ValidateSendAddressing: error = %v, want ErrInvalidTransition", err)
			}
		})
	}
}

func TestAddressEqual(t *testing.T) {
	if !run.ManagerAddress().Equal(run.ManagerAddress()) {
		t.Fatal("ManagerAddress().Equal(ManagerAddress()) = false, want true")
	}
	if !run.TaskAddress(testTaskID).Equal(run.TaskAddress(testTaskID)) {
		t.Fatal("TaskAddress(x).Equal(TaskAddress(x)) = false, want true")
	}
	if run.TaskAddress(testTaskID).Equal(run.TaskAddress(testSecondTaskID)) {
		t.Fatal("TaskAddress(x).Equal(TaskAddress(y)) = true, want false")
	}
	if run.ManagerAddress().Equal(run.HumanAddress()) {
		t.Fatal("ManagerAddress().Equal(HumanAddress()) = true, want false")
	}
}

func TestMessageDeliver(t *testing.T) {
	cases := []struct {
		from    run.MessageState
		wantErr bool
	}{
		{run.MessageQueued, false},
		{run.MessageDelivered, false},
		{run.MessageAcknowledged, true},
	}
	for _, tc := range cases {
		t.Run(string(tc.from), func(t *testing.T) {
			m := run.Message{ID: testMessageID, RunID: testRunID, State: tc.from}

			got, err := m.Deliver()

			if tc.wantErr {
				if !errors.Is(err, run.ErrInvalidTransition) {
					t.Fatalf("Deliver from %s: error = %v, want ErrInvalidTransition", tc.from, err)
				}
				if got.State != tc.from {
					t.Fatalf("Deliver from %s: State = %s, want unchanged", tc.from, got.State)
				}
				return
			}
			if err != nil {
				t.Fatalf("Deliver from %s: unexpected error: %v", tc.from, err)
			}
			if got.State != run.MessageDelivered {
				t.Fatalf("Deliver from %s: State = %s, want delivered", tc.from, got.State)
			}
		})
	}
}

func TestAcceptAck(t *testing.T) {
	baseMessage := func(state run.MessageState) run.Message {
		return run.Message{ID: testMessageID, RunID: testRunID, Recipient: run.ManagerAddress(), State: state}
	}
	ack := run.Ack{SessionID: testSessionID, IncarnationID: testIncarnation}

	t.Run("duplicate is idempotent regardless of incarnation", func(t *testing.T) {
		prior := run.Ack{MessageID: testMessageID, SessionID: testSessionID, IncarnationID: testIncarnation, At: epoch()}
		outcome, err := run.AcceptAck(baseMessage(run.MessageAcknowledged), &prior, run.AckContext{}, ack, later())
		if err != nil {
			t.Fatalf("AcceptAck(duplicate): unexpected error: %v", err)
		}
		if outcome.Ack != prior {
			t.Fatalf("AcceptAck(duplicate) Ack = %+v, want the prior ack unchanged: %+v", outcome.Ack, prior)
		}
	})

	t.Run("not delivered", func(t *testing.T) {
		_, err := run.AcceptAck(baseMessage(run.MessageQueued), nil, run.AckContext{DeliveredToSession: true, IncarnationCurrent: true, AttemptCurrent: true}, ack, later())
		if !errors.Is(err, run.ErrNotDelivered) {
			t.Fatalf("AcceptAck(queued): error = %v, want ErrNotDelivered", err)
		}
	})

	t.Run("no delivery to this session", func(t *testing.T) {
		_, err := run.AcceptAck(baseMessage(run.MessageDelivered), nil, run.AckContext{DeliveredToSession: false, IncarnationCurrent: true, AttemptCurrent: true}, ack, later())
		if !errors.Is(err, run.ErrNotDelivered) {
			t.Fatalf("AcceptAck(no delivery to session): error = %v, want ErrNotDelivered", err)
		}
	})

	t.Run("stale incarnation", func(t *testing.T) {
		_, err := run.AcceptAck(baseMessage(run.MessageDelivered), nil, run.AckContext{DeliveredToSession: true, IncarnationCurrent: false, AttemptCurrent: true}, ack, later())
		if !errors.Is(err, run.ErrStaleAck) {
			t.Fatalf("AcceptAck(stale incarnation): error = %v, want ErrStaleAck", err)
		}
	})

	t.Run("accepted", func(t *testing.T) {
		outcome, err := run.AcceptAck(baseMessage(run.MessageDelivered), nil, run.AckContext{DeliveredToSession: true, IncarnationCurrent: true, AttemptCurrent: true}, ack, later())
		if err != nil {
			t.Fatalf("AcceptAck: unexpected error: %v", err)
		}
		if outcome.Message.State != run.MessageAcknowledged {
			t.Fatalf("AcceptAck: Message.State = %s, want acknowledged", outcome.Message.State)
		}
		if !outcome.Ack.At.Equal(later()) || outcome.Ack.MessageID != testMessageID {
			t.Fatalf("AcceptAck: Ack = %+v, want At=%v MessageID=%s", outcome.Ack, later(), testMessageID)
		}
	})
}

func TestNextDeliverable(t *testing.T) {
	t.Run("empty queue", func(t *testing.T) {
		_, ok := run.NextDeliverable(nil)
		if ok {
			t.Fatal("NextDeliverable(empty) ok = true, want false")
		}
	})

	t.Run("in-flight message wins over any queued message", func(t *testing.T) {
		inFlight := run.Message{ID: testMessageID, EnqueueSeq: 5, State: run.MessageDelivered}
		queued := run.Message{ID: testSecondMessageID, EnqueueSeq: 1, State: run.MessageQueued}

		got, ok := run.NextDeliverable([]run.Message{queued, inFlight})
		if !ok || got.ID != testMessageID {
			t.Fatalf("NextDeliverable = (%+v, %v), want the in-flight message", got, ok)
		}
	})

	t.Run("lowest enqueue sequence wins among queued messages", func(t *testing.T) {
		a := run.Message{ID: testMessageID, EnqueueSeq: 3, State: run.MessageQueued}
		b := run.Message{ID: testSecondMessageID, EnqueueSeq: 1, State: run.MessageQueued}
		c := run.Message{ID: testThirdMessageID, EnqueueSeq: 2, State: run.MessageQueued}

		got, ok := run.NextDeliverable([]run.Message{a, b, c})
		if !ok || got.ID != testSecondMessageID {
			t.Fatalf("NextDeliverable = (%+v, %v), want the lowest-sequence message %s", got, ok, testSecondMessageID)
		}
	})

	t.Run("acknowledged messages are never candidates", func(t *testing.T) {
		acked := run.Message{ID: testMessageID, EnqueueSeq: 1, State: run.MessageAcknowledged}

		_, ok := run.NextDeliverable([]run.Message{acked})
		if ok {
			t.Fatal("NextDeliverable(only acknowledged) ok = true, want false")
		}
	})
}

func TestAcceptAnswer(t *testing.T) {
	question := func(state run.MessageState, recipient run.Address) run.Message {
		return run.Message{ID: testMessageID, RunID: testRunID, Kind: run.MessageQuestion, Recipient: recipient, State: state}
	}
	submission := run.AnswerSubmission{ID: testSecondMessageID, BodyPath: "/body", BodyDigest: "digest-v1", BodyBytes: 4}
	byManager := run.AnswerContext{AnswererAddress: run.ManagerAddress()}
	byHuman := run.AnswerContext{AnswererAddress: run.HumanAddress()}

	t.Run("accepted, ordinary session-addressed question is not auto-acked", func(t *testing.T) {
		q := question(run.MessageDelivered, run.ManagerAddress())

		outcome, err := run.AcceptAnswer(q, nil, byManager, run.TaskAddress(testTaskID), run.SessionPrincipal(testManagerSessionID), submission, 1, epoch())
		if err != nil {
			t.Fatalf("AcceptAnswer: unexpected error: %v", err)
		}
		if outcome.Question.State != run.MessageDelivered {
			t.Fatalf("AcceptAnswer: Question.State = %s, want unchanged (delivered)", outcome.Question.State)
		}
		if outcome.Answer.Kind != run.MessageAnswer || outcome.Answer.Recipient != run.TaskAddress(testTaskID) {
			t.Fatalf("AcceptAnswer: Answer = %+v, want kind=answer recipient=task:%s", outcome.Answer, testTaskID)
		}
		if outcome.Answer.ReplyTo == nil || *outcome.Answer.ReplyTo != q.ID {
			t.Fatalf("AcceptAnswer: Answer.ReplyTo = %v, want %s", outcome.Answer.ReplyTo, q.ID)
		}
	})

	t.Run("accepted, a task-addressed question answered by that task", func(t *testing.T) {
		q := question(run.MessageDelivered, run.TaskAddress(testTaskID))
		byTask := run.AnswerContext{AnswererAddress: run.TaskAddress(testTaskID)}

		outcome, err := run.AcceptAnswer(q, nil, byTask, run.ManagerAddress(), run.SessionPrincipal(testSessionID), submission, 1, epoch())
		if err != nil {
			t.Fatalf("AcceptAnswer: unexpected error: %v", err)
		}
		if outcome.Answer.Recipient != run.ManagerAddress() || outcome.Question.State != run.MessageDelivered {
			t.Fatalf("AcceptAnswer: outcome = %+v, want an answer to the manager and the question unchanged", outcome)
		}
	})

	t.Run("accepted, human-addressed question is acked atomically", func(t *testing.T) {
		q := question(run.MessageQueued, run.HumanAddress())

		outcome, err := run.AcceptAnswer(q, nil, byHuman, run.ManagerAddress(), run.HumanPrincipal(), submission, 1, epoch())
		if err != nil {
			t.Fatalf("AcceptAnswer: unexpected error: %v", err)
		}
		if outcome.Question.State != run.MessageAcknowledged {
			t.Fatalf("AcceptAnswer(human question): Question.State = %s, want acknowledged", outcome.Question.State)
		}
	})

	t.Run("not a question", func(t *testing.T) {
		notQuestion := run.Message{ID: testMessageID, Kind: run.MessageInfo, Recipient: run.ManagerAddress()}
		// The shape check precedes authority: a non-question is refused
		// as not a question whoever answers it.
		for _, answerer := range []run.AnswerContext{byHuman, byManager} {
			_, err := run.AcceptAnswer(notQuestion, nil, answerer, run.ManagerAddress(), run.HumanPrincipal(), submission, 1, epoch())
			if !errors.Is(err, run.ErrInvalidTransition) {
				t.Fatalf("AcceptAnswer(not a question, answerer %s): error = %v, want ErrInvalidTransition", answerer.AnswererAddress.Kind, err)
			}
		}
	})

	t.Run("an ordinary question's own ack state is orthogonal to answering it", func(t *testing.T) {
		// The manager typically acks q1 upon reading it, well before
		// composing and forwarding its answer (trace 2): answering an
		// already-acknowledged, non-human question is not itself an
		// error — "unanswered" is governed entirely by prior, checked
		// above.
		q := question(run.MessageAcknowledged, run.ManagerAddress())

		outcome, err := run.AcceptAnswer(q, nil, byManager, run.TaskAddress(testTaskID), run.SessionPrincipal(testManagerSessionID), submission, 1, epoch())
		if err != nil {
			t.Fatalf("AcceptAnswer(already acknowledged, ordinary question): unexpected error: %v", err)
		}
		if outcome.Question.State != run.MessageAcknowledged {
			t.Fatalf("AcceptAnswer(already acknowledged, ordinary question): Question.State = %s, want unchanged", outcome.Question.State)
		}
	})

	t.Run("duplicate answer is idempotent", func(t *testing.T) {
		q := question(run.MessageDelivered, run.ManagerAddress())
		prior := run.Message{ID: testThirdMessageID, BodyDigest: "digest-v1"}

		outcome, err := run.AcceptAnswer(q, &prior, byManager, run.TaskAddress(testTaskID), run.SessionPrincipal(testManagerSessionID), submission, 1, epoch())
		if !errors.Is(err, run.ErrDuplicateAnswer) {
			t.Fatalf("AcceptAnswer(duplicate): error = %v, want ErrDuplicateAnswer", err)
		}
		if outcome.Answer != prior {
			t.Fatalf("AcceptAnswer(duplicate): Answer = %+v, want the prior answer unchanged: %+v", outcome.Answer, prior)
		}
	})

	t.Run("conflicting answer refused, prior undisturbed", func(t *testing.T) {
		q := question(run.MessageDelivered, run.ManagerAddress())
		prior := run.Message{ID: testThirdMessageID, BodyDigest: "different-digest"}

		outcome, err := run.AcceptAnswer(q, &prior, byManager, run.TaskAddress(testTaskID), run.SessionPrincipal(testManagerSessionID), submission, 1, epoch())
		if !errors.Is(err, run.ErrConflictingAnswer) {
			t.Fatalf("AcceptAnswer(conflicting): error = %v, want ErrConflictingAnswer", err)
		}
		if outcome.Answer != prior {
			t.Fatalf("AcceptAnswer(conflicting): Answer = %+v, want the prior answer unchanged: %+v", outcome.Answer, prior)
		}
	})
}

// TestAcceptAnswerRecipientAuthority pins section 7's answer authority: only
// the principal whose logical address IS the question's recipient may
// answer it, checked before any prior answer so a refused answerer never
// receives a duplicate or conflicting verdict about someone else's answer,
// and a refusal leaves the question exactly as it was (a human question is
// never acknowledged by a refused answer).
func TestAcceptAnswerRecipientAuthority(t *testing.T) {
	submission := run.AnswerSubmission{ID: testSecondMessageID, BodyPath: "/body", BodyDigest: "digest-v1", BodyBytes: 4}
	sameBodyPrior := run.Message{ID: testThirdMessageID, BodyDigest: "digest-v1"}
	otherBodyPrior := run.Message{ID: testThirdMessageID, BodyDigest: "digest-other"}
	cases := []struct {
		name      string
		recipient run.Address
		answerer  run.Address
		prior     *run.Message
	}{
		{"a task session answering a human question", run.HumanAddress(), run.TaskAddress(testTaskID), nil},
		{"the manager answering a human question", run.HumanAddress(), run.ManagerAddress(), nil},
		{"a task answering another task's question", run.TaskAddress(testTaskID), run.TaskAddress(testSecondTaskID), nil},
		{"the manager answering a task's question", run.TaskAddress(testTaskID), run.ManagerAddress(), nil},
		{"a task answering the manager's own inbound question", run.ManagerAddress(), run.TaskAddress(testTaskID), nil},
		{"the human answering a manager-addressed question", run.ManagerAddress(), run.HumanAddress(), nil},
		{"a non-recipient repeating the accepted body", run.HumanAddress(), run.TaskAddress(testTaskID), &sameBodyPrior},
		{"a non-recipient sending a different body", run.HumanAddress(), run.TaskAddress(testTaskID), &otherBodyPrior},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := run.Message{ID: testMessageID, RunID: testRunID, Kind: run.MessageQuestion, Recipient: tc.recipient, State: run.MessageQueued}
			outcome, err := run.AcceptAnswer(q, tc.prior, run.AnswerContext{AnswererAddress: tc.answerer}, run.ManagerAddress(), run.SessionPrincipal(testSessionID), submission, 1, epoch())
			if !errors.Is(err, run.ErrAnswerNotRecipient) {
				t.Fatalf("AcceptAnswer: error = %v, want ErrAnswerNotRecipient", err)
			}
			if errors.Is(err, run.ErrDuplicateAnswer) || errors.Is(err, run.ErrConflictingAnswer) {
				t.Fatalf("AcceptAnswer: error = %v also reports the prior answer's verdict", err)
			}
			if outcome.Question != q || outcome.Answer != (run.Message{}) {
				t.Fatalf("AcceptAnswer: outcome = %+v, want the question unchanged and no answer", outcome)
			}
		})
	}
}

// TestAcceptAnswerClosedDestination pins section 5's admission rule for
// answers: a closed destination mailbox refuses a FIRST acceptance, after
// the prior answer is resolved, so an answer accepted before the closure
// still replays as duplicate (or conflicting) rather than as a closure
// refusal.
func TestAcceptAnswerClosedDestination(t *testing.T) {
	submission := run.AnswerSubmission{ID: testSecondMessageID, BodyPath: "/body", BodyDigest: "digest-v1", BodyBytes: 4}
	closed := run.AnswerContext{AnswererAddress: run.ManagerAddress(), DestinationMailboxClosed: true}
	q := run.Message{ID: testMessageID, RunID: testRunID, Kind: run.MessageQuestion, Recipient: run.ManagerAddress(), State: run.MessageAcknowledged}

	t.Run("a first answer is refused", func(t *testing.T) {
		outcome, err := run.AcceptAnswer(q, nil, closed, run.TaskAddress(testTaskID), run.SessionPrincipal(testManagerSessionID), submission, 1, epoch())
		if !errors.Is(err, run.ErrMailboxClosed) {
			t.Fatalf("AcceptAnswer: error = %v, want ErrMailboxClosed", err)
		}
		if outcome.Question != q || outcome.Answer != (run.Message{}) {
			t.Fatalf("AcceptAnswer: outcome = %+v, want the question unchanged and no answer", outcome)
		}
	})

	t.Run("an identical answer accepted before the closure is a duplicate", func(t *testing.T) {
		prior := run.Message{ID: testThirdMessageID, BodyDigest: "digest-v1"}
		outcome, err := run.AcceptAnswer(q, &prior, closed, run.TaskAddress(testTaskID), run.SessionPrincipal(testManagerSessionID), submission, 1, epoch())
		if !errors.Is(err, run.ErrDuplicateAnswer) || errors.Is(err, run.ErrMailboxClosed) {
			t.Fatalf("AcceptAnswer: error = %v, want ErrDuplicateAnswer alone", err)
		}
		if outcome.Answer != prior {
			t.Fatalf("AcceptAnswer: Answer = %+v, want the prior answer %+v", outcome.Answer, prior)
		}
	})

	t.Run("a different answer after the closure is conflicting", func(t *testing.T) {
		prior := run.Message{ID: testThirdMessageID, BodyDigest: "digest-other"}
		_, err := run.AcceptAnswer(q, &prior, closed, run.TaskAddress(testTaskID), run.SessionPrincipal(testManagerSessionID), submission, 1, epoch())
		if !errors.Is(err, run.ErrConflictingAnswer) || errors.Is(err, run.ErrMailboxClosed) {
			t.Fatalf("AcceptAnswer: error = %v, want ErrConflictingAnswer alone", err)
		}
	})

	t.Run("a human answer is refused too, and the question stays unacknowledged", func(t *testing.T) {
		humanQ := run.Message{ID: testMessageID, RunID: testRunID, Kind: run.MessageQuestion, Recipient: run.HumanAddress(), State: run.MessageQueued}
		humanClosed := run.AnswerContext{AnswererAddress: run.HumanAddress(), DestinationMailboxClosed: true}
		outcome, err := run.AcceptAnswer(humanQ, nil, humanClosed, run.TaskAddress(testTaskID), run.HumanPrincipal(), submission, 1, epoch())
		if !errors.Is(err, run.ErrMailboxClosed) {
			t.Fatalf("AcceptAnswer: error = %v, want ErrMailboxClosed", err)
		}
		if outcome.Question.State != run.MessageQueued {
			t.Fatalf("AcceptAnswer: Question.State = %s, want queued (no bundled ack on a refusal)", outcome.Question.State)
		}
	})

	t.Run("authority precedes the closure", func(t *testing.T) {
		stranger := run.AnswerContext{AnswererAddress: run.TaskAddress(testSecondTaskID), DestinationMailboxClosed: true}
		_, err := run.AcceptAnswer(q, nil, stranger, run.TaskAddress(testTaskID), run.SessionPrincipal(testSessionID), submission, 1, epoch())
		if !errors.Is(err, run.ErrAnswerNotRecipient) {
			t.Fatalf("AcceptAnswer: error = %v, want ErrAnswerNotRecipient", err)
		}
	})
}

func TestResolveOrigin(t *testing.T) {
	t.Run("no relay", func(t *testing.T) {
		q := run.Message{ID: testMessageID}
		if got := run.ResolveOrigin(q); got != testMessageID {
			t.Fatalf("ResolveOrigin(no relay) = %s, want %s", got, testMessageID)
		}
	})

	t.Run("relayed", func(t *testing.T) {
		original := testSecondMessageID
		q := run.Message{ID: testMessageID, RelayedFrom: &original}
		if got := run.ResolveOrigin(q); got != original {
			t.Fatalf("ResolveOrigin(relayed) = %s, want %s", got, original)
		}
	})
}
