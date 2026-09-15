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
		_, err := run.AcceptAck(baseMessage(run.MessageQueued), nil, run.AckContext{DeliveredToSession: true, IncarnationCurrent: true}, ack, later())
		if !errors.Is(err, run.ErrNotDelivered) {
			t.Fatalf("AcceptAck(queued): error = %v, want ErrNotDelivered", err)
		}
	})

	t.Run("no delivery to this session", func(t *testing.T) {
		_, err := run.AcceptAck(baseMessage(run.MessageDelivered), nil, run.AckContext{DeliveredToSession: false, IncarnationCurrent: true}, ack, later())
		if !errors.Is(err, run.ErrNotDelivered) {
			t.Fatalf("AcceptAck(no delivery to session): error = %v, want ErrNotDelivered", err)
		}
	})

	t.Run("stale incarnation", func(t *testing.T) {
		_, err := run.AcceptAck(baseMessage(run.MessageDelivered), nil, run.AckContext{DeliveredToSession: true, IncarnationCurrent: false}, ack, later())
		if !errors.Is(err, run.ErrStaleAck) {
			t.Fatalf("AcceptAck(stale incarnation): error = %v, want ErrStaleAck", err)
		}
	})

	t.Run("accepted", func(t *testing.T) {
		outcome, err := run.AcceptAck(baseMessage(run.MessageDelivered), nil, run.AckContext{DeliveredToSession: true, IncarnationCurrent: true}, ack, later())
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

	t.Run("accepted, ordinary session-addressed question is not auto-acked", func(t *testing.T) {
		q := question(run.MessageDelivered, run.ManagerAddress())

		outcome, err := run.AcceptAnswer(q, nil, run.TaskAddress(testTaskID), run.SessionPrincipal(testManagerSessionID), submission, 1, epoch())
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

	t.Run("accepted, human-addressed question is acked atomically", func(t *testing.T) {
		q := question(run.MessageQueued, run.HumanAddress())

		outcome, err := run.AcceptAnswer(q, nil, run.ManagerAddress(), run.HumanPrincipal(), submission, 1, epoch())
		if err != nil {
			t.Fatalf("AcceptAnswer: unexpected error: %v", err)
		}
		if outcome.Question.State != run.MessageAcknowledged {
			t.Fatalf("AcceptAnswer(human question): Question.State = %s, want acknowledged", outcome.Question.State)
		}
	})

	t.Run("not a question", func(t *testing.T) {
		notQuestion := run.Message{ID: testMessageID, Kind: run.MessageInfo}
		_, err := run.AcceptAnswer(notQuestion, nil, run.ManagerAddress(), run.HumanPrincipal(), submission, 1, epoch())
		if !errors.Is(err, run.ErrInvalidTransition) {
			t.Fatalf("AcceptAnswer(not a question): error = %v, want ErrInvalidTransition", err)
		}
	})

	t.Run("already answered and unanswerable state", func(t *testing.T) {
		q := question(run.MessageAcknowledged, run.HumanAddress())
		_, err := run.AcceptAnswer(q, nil, run.ManagerAddress(), run.HumanPrincipal(), submission, 1, epoch())
		if !errors.Is(err, run.ErrInvalidTransition) {
			t.Fatalf("AcceptAnswer(already acknowledged, no prior recorded): error = %v, want ErrInvalidTransition", err)
		}
	})

	t.Run("duplicate answer is idempotent", func(t *testing.T) {
		q := question(run.MessageDelivered, run.ManagerAddress())
		prior := run.Message{ID: testThirdMessageID, BodyDigest: "digest-v1"}

		outcome, err := run.AcceptAnswer(q, &prior, run.TaskAddress(testTaskID), run.SessionPrincipal(testManagerSessionID), submission, 1, epoch())
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

		outcome, err := run.AcceptAnswer(q, &prior, run.TaskAddress(testTaskID), run.SessionPrincipal(testManagerSessionID), submission, 1, epoch())
		if !errors.Is(err, run.ErrConflictingAnswer) {
			t.Fatalf("AcceptAnswer(conflicting): error = %v, want ErrConflictingAnswer", err)
		}
		if outcome.Answer != prior {
			t.Fatalf("AcceptAnswer(conflicting): Answer = %+v, want the prior answer unchanged: %+v", outcome.Answer, prior)
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
