package main

import (
	"bytes"
	"context"
	"errors"
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
}
