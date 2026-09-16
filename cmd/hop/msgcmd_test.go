package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// TestRunMsgWaitTimeoutDefault covers ruling C: hop msg wait's default
// --timeout resolves from the run's frozen [messages] wait_timeout
// (Controller.MessageWaitDefault) when --timeout is absent, an explicit
// --timeout always wins over it (and MessageWaitDefault is never called),
// and the "none" line renders whichever timeout was actually used.
func TestRunMsgWaitTimeoutDefault(t *testing.T) {
	t.Run("an explicit --timeout wins and skips the frozen-default read", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.fetchMessage = func(app.FetchMessageRequest) (app.FetchMessageResult, error) {
			return app.FetchMessageResult{}, nil
		}
		// messageWaitDefault is left nil: any call fails the fake, proving
		// an explicit --timeout never triggers the frozen-default read.
		td := newTestDeps(ctrl, managerEnv(), t.TempDir())
		td.deps.wait = func(ctx context.Context, _ time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return context.DeadlineExceeded
		}
		var stdout, stderr bytes.Buffer

		code, err := runMsgWait([]string{"--timeout", "5s"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK {
			t.Errorf("exit code = %d, want %d (a timeout is a retryable state, not a failure)", code, exitOK)
		}
		want := app.GrammarMsgWaitNoneLine(5 * time.Second)
		if stdout.String() != want+"\n" {
			t.Errorf("stdout = %q, want %q", stdout.String(), want+"\n")
		}
		for _, call := range ctrl.recorded() {
			if call == "MessageWaitDefault" {
				t.Fatalf("MessageWaitDefault was called despite an explicit --timeout")
			}
		}
	})

	t.Run("the default resolves from the run's frozen wait_timeout", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.fetchMessage = func(app.FetchMessageRequest) (app.FetchMessageResult, error) {
			return app.FetchMessageResult{}, nil
		}
		var gotRunID string
		ctrl.messageWaitDefault = func(runID string) (time.Duration, error) {
			gotRunID = runID
			return 17 * time.Second, nil
		}
		td := newTestDeps(ctrl, managerEnv(), t.TempDir())
		td.deps.wait = func(ctx context.Context, _ time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return context.DeadlineExceeded
		}
		var stdout, stderr bytes.Buffer

		code, err := runMsgWait(nil, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK {
			t.Errorf("exit code = %d, want %d (a timeout is a retryable state, not a failure)", code, exitOK)
		}
		if gotRunID != testRunID {
			t.Errorf("MessageWaitDefault run id = %q, want %q", gotRunID, testRunID)
		}
		want := app.GrammarMsgWaitNoneLine(17 * time.Second)
		if stdout.String() != want+"\n" {
			t.Errorf("stdout = %q, want %q", stdout.String(), want+"\n")
		}
	})

	t.Run("the poll loop retries until a message is delivered", func(t *testing.T) {
		ctrl := &fakeController{}
		attempts := 0
		ctrl.fetchMessage = func(app.FetchMessageRequest) (app.FetchMessageResult, error) {
			attempts++
			if attempts < 3 {
				return app.FetchMessageResult{}, nil
			}
			return app.FetchMessageResult{
				Delivered: true, MessageID: "44444444-4444-4444-8444-444444444444",
				Kind: "question", SenderKind: "session", SenderSession: "55555555-5555-4555-8555-555555555555",
				BodyPath: "/state/body.md",
			}, nil
		}
		// The default testDeps wait advances a fake clock and returns nil
		// for msgPollInterval (never blocking on real time), so the loop
		// free-runs through both not-delivered attempts deterministically.
		td := newTestDeps(ctrl, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgWait([]string{"--timeout", "5s"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK {
			t.Errorf("exit code = %d, want %d, stderr = %q", code, exitOK, stderr.String())
		}
		if attempts != 3 {
			t.Errorf("fetchMessage attempts = %d, want 3 (retried until delivered)", attempts)
		}
		if !strings.HasPrefix(stdout.String(), "message 44444444-4444-4444-8444-444444444444 ") {
			t.Errorf("stdout = %q", stdout.String())
		}
	})

	t.Run("a frozen-default lookup failure is a command failure", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.fetchMessage = func(app.FetchMessageRequest) (app.FetchMessageResult, error) {
			return app.FetchMessageResult{}, nil
		}
		ctrl.messageWaitDefault = func(string) (time.Duration, error) {
			return 0, errors.New("boom")
		}
		td := newTestDeps(ctrl, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgWait(nil, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout = %q, want empty on a setup failure", stdout.String())
		}
	})

	t.Run("the environment is validated through the first fetch before the default is ever resolved", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.fetchMessage = func(app.FetchMessageRequest) (app.FetchMessageResult, error) {
			return app.FetchMessageResult{}, errors.New("app: parse run id: malformed")
		}
		// messageWaitDefault is left nil: any call fails the fake, proving
		// a fetch error (an invalid HOP_* identity, in production) is
		// never followed by a MessageWaitDefault read.
		td := newTestDeps(ctrl, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgWait(nil, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		if !strings.Contains(stderr.String(), "parse run id") {
			t.Errorf("stderr = %q, want the fetch error surfaced directly", stderr.String())
		}
		for _, call := range ctrl.recorded() {
			if call == "MessageWaitDefault" {
				t.Fatalf("MessageWaitDefault was called despite a failed environment-validating fetch")
			}
		}
	})
}

// TestRunMsgSend covers hop msg send's dispatch, happy path, refusal and
// usage-error surface (design section 7).
func TestRunMsgSend(t *testing.T) {
	t.Run("accepted prints the sent line and reads identities from env", func(t *testing.T) {
		ctrl := &fakeController{}
		var req app.SendMessageRequest
		ctrl.sendMessage = func(r app.SendMessageRequest) (app.SendMessageResult, error) {
			req = r
			return app.SendMessageResult{Outcome: "accepted", MessageID: "msg-1"}, nil
		}
		td := newTestDeps(ctrl, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgSend([]string{"--to", "manager", "--kind", "question", "--body", "q?"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK || stdout.String() != "sent msg-1\n" {
			t.Errorf("code = %d, stdout = %q (stderr: %s)", code, stdout.String(), stderr.String())
		}
		if req.RunID != testRunID || req.SessionID == "" || req.IncarnationID == "" {
			t.Errorf("identities not read from HOP_* env: %+v", req)
		}
		if req.To != "manager" || req.Kind != "question" || string(req.Body) != "q?" || !req.Inline {
			t.Errorf("request = %+v", req)
		}
	})

	t.Run("duplicate prints the duplicate line", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.sendMessage = func(app.SendMessageRequest) (app.SendMessageResult, error) {
			return app.SendMessageResult{Outcome: "duplicate", MessageID: "msg-1"}, nil
		}
		td := newTestDeps(ctrl, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgSend([]string{"--to", "manager", "--kind", "question", "--body", "q?"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK || stdout.String() != "duplicate msg-1\n" {
			t.Errorf("code = %d, stdout = %q", code, stdout.String())
		}
	})

	t.Run("a refusal renders refused: <token>", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.sendMessage = func(app.SendMessageRequest) (app.SendMessageResult, error) {
			return app.SendMessageResult{Outcome: "refused", Reason: app.GrammarReasonStale, Detail: "incarnation is not current"}, nil
		}
		td := newTestDeps(ctrl, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgSend([]string{"--to", "manager", "--kind", "question", "--body", "q?"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure || stdout.String() != "refused: stale\nincarnation is not current\n" {
			t.Errorf("code = %d, stdout = %q", code, stdout.String())
		}
	})

	t.Run("missing --kind is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgSend([]string{"--to", "manager", "--body", "q?"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})

	t.Run("--to is forbidden for --kind answer", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgSend([]string{"--to", "manager", "--kind", "answer", "--reply-to", "q-1", "--body", "a"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})

	t.Run("--reply-to is required for --kind answer", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgSend([]string{"--kind", "answer", "--body", "a"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})

	t.Run("--to is required for a non-answer kind", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgSend([]string{"--kind", "question", "--body", "q?"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})

	t.Run("--reply-to is forbidden for a non-answer kind", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgSend([]string{"--to", "manager", "--kind", "question", "--reply-to", "q-1", "--body", "q?"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})

	t.Run("neither --file nor --body is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgSend([]string{"--to", "manager", "--kind", "question"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})

	t.Run("both --file and --body is a usage error", func(t *testing.T) {
		bodyFile := filepath.Join(t.TempDir(), "body.md")
		if err := os.WriteFile(bodyFile, []byte("q?"), 0o600); err != nil {
			t.Fatal(err)
		}
		td := newTestDeps(&fakeController{}, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgSend([]string{"--to", "manager", "--kind", "question", "--file", bodyFile, "--body", "q?"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})

	t.Run("unexpected argument is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgSend([]string{"--to", "manager", "--kind", "question", "--body", "q?", "extra"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})
}

// TestRunMsgNext covers hop msg next's dispatch, happy path and empty
// queue.
func TestRunMsgNext(t *testing.T) {
	t.Run("a delivered message renders the three-line envelope", func(t *testing.T) {
		ctrl := &fakeController{}
		var req app.FetchMessageRequest
		ctrl.fetchMessage = func(r app.FetchMessageRequest) (app.FetchMessageResult, error) {
			req = r
			return app.FetchMessageResult{
				Delivered: true, MessageID: "msg-1", Kind: "question",
				SenderKind: "session", SenderSession: "worker-1", BodyPath: "/state/body.md",
			}, nil
		}
		td := newTestDeps(ctrl, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgNext(nil, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		want := "message msg-1 kind=question from=worker-1\nbody: /state/body.md\nack: hop msg ack msg-1\n"
		if code != exitOK || stdout.String() != want {
			t.Errorf("code = %d, stdout = %q, want %q (stderr: %s)", code, stdout.String(), want, stderr.String())
		}
		if req.RunID != testRunID || req.SessionID == "" || req.IncarnationID == "" {
			t.Errorf("identities not read from HOP_* env: %+v", req)
		}
	})

	t.Run("an empty queue prints none", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.fetchMessage = func(app.FetchMessageRequest) (app.FetchMessageResult, error) {
			return app.FetchMessageResult{}, nil
		}
		td := newTestDeps(ctrl, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgNext(nil, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK || stdout.String() != "none: no queued message\n" {
			t.Errorf("code = %d, stdout = %q", code, stdout.String())
		}
	})

	t.Run("unexpected argument is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgNext([]string{"extra"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})
}

// TestRunMsgAck covers hop msg ack's dispatch, happy path, refusal and
// usage-error surface.
func TestRunMsgAck(t *testing.T) {
	t.Run("accepted prints the acknowledged line", func(t *testing.T) {
		ctrl := &fakeController{}
		var req app.AckMessageRequest
		ctrl.ackMessage = func(r app.AckMessageRequest) (app.AckMessageResult, error) {
			req = r
			return app.AckMessageResult{Outcome: "accepted"}, nil
		}
		td := newTestDeps(ctrl, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgAck([]string{"msg-1"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK || stdout.String() != "acknowledged msg-1\n" {
			t.Errorf("code = %d, stdout = %q", code, stdout.String())
		}
		if req.MessageID != "msg-1" || req.RunID != testRunID {
			t.Errorf("request = %+v", req)
		}
	})

	t.Run("duplicate prints the duplicate line", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.ackMessage = func(app.AckMessageRequest) (app.AckMessageResult, error) {
			return app.AckMessageResult{Outcome: "duplicate"}, nil
		}
		td := newTestDeps(ctrl, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgAck([]string{"msg-1"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK || stdout.String() != "duplicate msg-1\n" {
			t.Errorf("code = %d, stdout = %q", code, stdout.String())
		}
	})

	t.Run("a refusal renders refused: <token>", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.ackMessage = func(app.AckMessageRequest) (app.AckMessageResult, error) {
			return app.AckMessageResult{Outcome: "refused", Reason: app.GrammarReasonNotDelivered, Detail: "message was not delivered"}, nil
		}
		td := newTestDeps(ctrl, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgAck([]string{"msg-1"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure || !strings.HasPrefix(stdout.String(), "refused: not-delivered\n") {
			t.Errorf("code = %d, stdout = %q", code, stdout.String())
		}
	})

	t.Run("missing message-id argument is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgAck(nil, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})
}

// TestRunMsgShow covers hop msg show's dispatch, happy path, not-found
// and usage-error surface. It is the one message verb with no caller
// identity validation (design section 7).
func TestRunMsgShow(t *testing.T) {
	t.Run("found renders the envelope, body and delivery/ack lines", func(t *testing.T) {
		ctrl := &fakeController{}
		var req app.ShowMessageRequest
		at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
		ctrl.showMessage = func(r app.ShowMessageRequest) (app.ShowMessageResult, error) {
			req = r
			return app.ShowMessageResult{
				Found: true, MessageID: "msg-1", Kind: "question", SenderKind: "session", SenderSession: "worker-1",
				Recipient: "manager", Seq: 2, BodyPath: "/state/body.md",
				Deliveries:   []app.ShowMessageDelivery{{SessionID: "manager-1", At: at}},
				Acknowledged: true, AcknowledgedAt: at,
			}, nil
		}
		td := newTestDeps(ctrl, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgShow([]string{"msg-1"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		want := "message msg-1 kind=question from=worker-1 to=manager seq=2\n" +
			"body: /state/body.md\n" +
			"delivered: manager-1 2026-09-14T12:00:00Z\n" +
			"acknowledged: 2026-09-14T12:00:00Z\n"
		if code != exitOK || stdout.String() != want {
			t.Errorf("code = %d, stdout = %q, want %q", code, stdout.String(), want)
		}
		if req.MessageID != "msg-1" || req.RunID != testRunID {
			t.Errorf("request = %+v", req)
		}
	})

	t.Run("an unknown message id refuses not-found", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.showMessage = func(app.ShowMessageRequest) (app.ShowMessageResult, error) {
			return app.ShowMessageResult{Found: false}, nil
		}
		td := newTestDeps(ctrl, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgShow([]string{"unknown-id"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure || stdout.String() != "refused: not-found\n" {
			t.Errorf("code = %d, stdout = %q", code, stdout.String())
		}
	})

	t.Run("--run overrides HOP_RUN_ID", func(t *testing.T) {
		ctrl := &fakeController{}
		var req app.ShowMessageRequest
		ctrl.showMessage = func(r app.ShowMessageRequest) (app.ShowMessageResult, error) {
			req = r
			return app.ShowMessageResult{Found: true, MessageID: "msg-1", SenderKind: "human"}, nil
		}
		td := newTestDeps(ctrl, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgShow([]string{"--run", "other-run", "msg-1"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK {
			t.Errorf("code = %d, stderr = %s", code, stderr.String())
		}
		if req.RunID != "other-run" {
			t.Errorf("RunID = %q, want the explicit --run value", req.RunID)
		}
	})

	t.Run("missing message-id argument is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runMsgShow(nil, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})
}

// answerEnv is hop answer's own environment: a human/controller-machine
// command with no HOP_* worker identity at all — it resolves -C/--run
// instead, so HOME (resolveStateRoot's own fallback) is the only
// variable it needs.
func answerEnv() map[string]string {
	return map[string]string{"HOME": "/home/controller"}
}

// TestRunAnswer covers hop answer's dispatch, happy path, refusal and
// usage-error surface, and proves it never reads worker HOP_* env.
func TestRunAnswer(t *testing.T) {
	t.Run("accepted prints the sent line and never reads worker env", func(t *testing.T) {
		ctrl := &fakeController{}
		var req app.AnswerRequest
		ctrl.answer = func(r app.AnswerRequest) (app.AnswerResult, error) {
			req = r
			return app.AnswerResult{Outcome: "accepted", MessageID: "answer-1"}, nil
		}
		td := newTestDeps(ctrl, answerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runAnswer([]string{"--run", testRunID, "--body", "the answer", "question-1"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK || stdout.String() != "sent answer-1\n" {
			t.Errorf("code = %d, stdout = %q (stderr: %s)", code, stdout.String(), stderr.String())
		}
		if req.RunID != testRunID || req.QuestionID != "question-1" || string(req.Body) != "the answer" {
			t.Errorf("request = %+v", req)
		}
	})

	t.Run("a refusal renders refused: <token>", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.answer = func(app.AnswerRequest) (app.AnswerResult, error) {
			return app.AnswerResult{Outcome: "refused", Reason: app.GrammarReasonMalformed, Detail: "unknown question"}, nil
		}
		td := newTestDeps(ctrl, answerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runAnswer([]string{"--run", testRunID, "--body", "a", "question-1"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure || !strings.HasPrefix(stdout.String(), "refused: malformed\n") {
			t.Errorf("code = %d, stdout = %q", code, stdout.String())
		}
	})

	t.Run("missing --run is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, answerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runAnswer([]string{"--body", "a", "question-1"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})

	t.Run("missing question-id argument is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, answerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runAnswer([]string{"--run", testRunID, "--body", "a"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})

	t.Run("neither --file nor --body is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, answerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runAnswer([]string{"--run", testRunID, "question-1"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})
}
