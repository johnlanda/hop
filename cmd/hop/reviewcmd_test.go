package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

// reviewerEnv is a reviewer session's worker environment: the same
// HOP_STATE_DIR/HOP_RUN_ID/HOP_SESSION_ID/HOP_INCARNATION_ID every worker
// verb reads, plus the task/attempt identities hop review submit also
// forwards.
func reviewerEnv() map[string]string {
	env := managerEnv()
	env["HOP_TASK_ID"] = "44444444-4444-4444-8444-444444444444"
	env["HOP_ATTEMPT_ID"] = "55555555-5555-4555-8555-555555555555"
	return env
}

func TestRunReviewSubmit(t *testing.T) {
	reasonsFile := filepath.Join(t.TempDir(), "reasons.md")
	if err := os.WriteFile(reasonsFile, []byte("looks good"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("accepted prints the verdict accepted line", func(t *testing.T) {
		ctrl := &fakeController{}
		var req app.SubmitReviewRequest
		ctrl.submitReview = func(r app.SubmitReviewRequest) (app.SubmitReviewResult, error) {
			req = r
			return app.SubmitReviewResult{Outcome: "accepted", ReviewID: "review-1"}, nil
		}
		td := newTestDeps(ctrl, reviewerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runReview([]string{"submit", "--verdict", "approve", "--subject", "cccccccccccccccccccccccccccccccccccccccc", "--reasons-file", reasonsFile}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK || stdout.String() != "verdict accepted review-1\n" {
			t.Errorf("code = %d, stdout = %q (stderr: %s)", code, stdout.String(), stderr.String())
		}
		if req.RunID != testRunID || req.TaskID == "" || req.AttemptID == "" || req.SessionID == "" || req.IncarnationID == "" {
			t.Errorf("identities not read from HOP_* env: %+v", req)
		}
		if req.Verdict != "approve" || req.SubjectCommitOID != "cccccccccccccccccccccccccccccccccccccccc" || string(req.ReasonsBody) != "looks good" {
			t.Errorf("request = %+v", req)
		}
	})

	t.Run("duplicate prints the duplicate line", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.submitReview = func(app.SubmitReviewRequest) (app.SubmitReviewResult, error) {
			return app.SubmitReviewResult{Outcome: "duplicate", ReviewID: "review-1"}, nil
		}
		td := newTestDeps(ctrl, reviewerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runReview([]string{"submit", "--verdict", "approve", "--subject", "cccccccccccccccccccccccccccccccccccccccc", "--reasons-file", reasonsFile}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK || stdout.String() != "duplicate review-1\n" {
			t.Errorf("code = %d, stdout = %q", code, stdout.String())
		}
	})

	transients := []struct {
		name       string
		result     app.SubmitReviewResult
		wantStdout string
		wantStderr string
	}{
		{
			name:       "an undelivered-messages transient prints the drain line and the detail goes to stderr",
			result:     app.SubmitReviewResult{Outcome: "transient", Detail: "run: mailbox has undelivered messages: task t", TransientReason: string(app.TransientUndeliveredMessages)},
			wantStdout: "transient: undelivered messages; drain with hop msg next, ack, then resubmit\n",
			wantStderr: "hop review submit: run: mailbox has undelivered messages: task t\n",
		},
		{
			name:       "an attempt-not-running transient prints the not-running line and the detail goes to stderr",
			result:     app.SubmitReviewResult{Outcome: "transient", Detail: "run: attempt not yet running: attempt a", TransientReason: string(app.TransientAttemptNotRunning)},
			wantStdout: "transient: attempt not yet running; retry\n",
			wantStderr: "hop review submit: run: attempt not yet running: attempt a\n",
		},
		{
			name:       "the reason decides, never the detail text",
			result:     app.SubmitReviewResult{Outcome: "transient", Detail: "2 undelivered messages", TransientReason: string(app.TransientAttemptNotRunning)},
			wantStdout: app.GrammarTransientNotRunningLine + "\n",
			wantStderr: "hop review submit: 2 undelivered messages\n",
		},
		{
			name:       "a transient with no reason prints no protocol line",
			result:     app.SubmitReviewResult{Outcome: "transient", Detail: "2 undelivered messages"},
			wantStdout: "",
			wantStderr: "hop review submit: transient outcome names no known retry reason\n",
		},
		{
			name:       "a transient with an unknown reason prints no protocol line",
			result:     app.SubmitReviewResult{Outcome: "transient", TransientReason: "mailbox-closed"},
			wantStdout: "",
			wantStderr: "hop review submit: transient outcome names no known retry reason\n",
		},
	}
	for _, tc := range transients {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &fakeController{}
			ctrl.submitReview = func(app.SubmitReviewRequest) (app.SubmitReviewResult, error) {
				return tc.result, nil
			}
			td := newTestDeps(ctrl, reviewerEnv(), t.TempDir())
			var stdout, stderr bytes.Buffer

			code, err := runReview([]string{"submit", "--verdict", "approve", "--subject", "cccccccccccccccccccccccccccccccccccccccc", "--reasons-file", reasonsFile}, &stdout, &stderr, td.deps)
			if err != nil {
				t.Fatalf("write error: %v", err)
			}
			if code != exitFailure || stdout.String() != tc.wantStdout || stderr.String() != tc.wantStderr {
				t.Errorf("code = %d, stdout = %q, stderr = %q; want %d, %q, %q", code, stdout.String(), stderr.String(), exitFailure, tc.wantStdout, tc.wantStderr)
			}
		})
	}

	t.Run("a refusal renders refused: <token>", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.submitReview = func(app.SubmitReviewRequest) (app.SubmitReviewResult, error) {
			return app.SubmitReviewResult{Outcome: "stale", Detail: "verdict subject mismatch"}, nil
		}
		td := newTestDeps(ctrl, reviewerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runReview([]string{"submit", "--verdict", "approve", "--subject", "cccccccccccccccccccccccccccccccccccccccc", "--reasons-file", reasonsFile}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure || !strings.HasPrefix(stdout.String(), "refused: stale\n") {
			t.Errorf("code = %d, stdout = %q", code, stdout.String())
		}
	})

	t.Run("missing required flags is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, reviewerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runReview([]string{"submit", "--verdict", "approve"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})

	t.Run("an unreadable reasons file is a command failure, not a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, reviewerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runReview([]string{"submit", "--verdict", "approve", "--subject", "cccccccccccccccccccccccccccccccccccccccc", "--reasons-file", filepath.Join(t.TempDir(), "missing.md")}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
	})

	t.Run("unexpected argument is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, reviewerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runReview([]string{"submit", "--verdict", "approve", "--subject", "cccccccccccccccccccccccccccccccccccccccc", "--reasons-file", reasonsFile, "extra"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})

	t.Run("an unknown hop review subcommand is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, reviewerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runReview([]string{"bogus"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})

	t.Run("no subcommand is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, reviewerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runReview(nil, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})
}
