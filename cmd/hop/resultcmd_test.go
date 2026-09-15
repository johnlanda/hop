package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

const (
	testTaskID   = "22222222-2222-4222-8222-222222222222"
	testResultID = "77777777-7777-4777-8777-777777777777"
	testCommit   = "0123456789abcdef0123456789abcdef01234567"
)

func TestRunResultSubmit(t *testing.T) {
	workerEnv := map[string]string{
		"HOP_STATE_DIR":      "/state/root",
		"HOP_RUN_ID":         testRunID,
		"HOP_TASK_ID":        testTaskID,
		"HOP_ATTEMPT_ID":     testAttemptID,
		"HOP_INCARNATION_ID": testIncarnationID,
	}

	outcomes := []struct {
		name      string
		result    app.SubmitResultResult
		wantCode  int
		firstLine string
	}{
		{
			name:      "accepted exits 0 and prints the result id",
			result:    app.SubmitResultResult{Kind: "accepted", ResultID: testResultID},
			wantCode:  exitOK,
			firstLine: "accepted " + testResultID,
		},
		{
			name:      "duplicate is idempotent success printing the existing result id",
			result:    app.SubmitResultResult{Kind: "duplicate", ResultID: testResultID},
			wantCode:  exitOK,
			firstLine: "duplicate " + testResultID,
		},
		{
			name:      "transient exits 1 with the retry line verbatim first",
			result:    app.SubmitResultResult{Kind: "transient", Detail: "transient: attempt not yet running; retry"},
			wantCode:  exitFailure,
			firstLine: "transient: attempt not yet running; retry",
		},
		{
			name:      "stale exits 1",
			result:    app.SubmitResultResult{Kind: "stale", Detail: "incarnation is superseded"},
			wantCode:  exitFailure,
			firstLine: "stale: incarnation is superseded",
		},
		{
			name:      "conflicting exits 1",
			result:    app.SubmitResultResult{Kind: "conflicting", Detail: "an accepted result exists with different content"},
			wantCode:  exitFailure,
			firstLine: "conflicting: an accepted result exists with different content",
		},
		{
			name:      "malformed is recorded and exits 1",
			result:    app.SubmitResultResult{Kind: "malformed", Detail: "commit must be a full 40-hex lowercase object id"},
			wantCode:  exitFailure,
			firstLine: "malformed: commit must be a full 40-hex lowercase object id",
		},
	}
	for _, tc := range outcomes {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &fakeController{}
			var submitted app.SubmitResultRequest
			ctrl.submitResult = func(req app.SubmitResultRequest) (app.SubmitResultResult, error) {
				submitted = req
				return tc.result, nil
			}
			td := newTestDeps(ctrl, workerEnv, t.TempDir())
			var stdout, stderr bytes.Buffer

			code, err := runResult([]string{"submit", "--summary", "did the thing", "--commit", testCommit}, &stdout, &stderr, td.deps)
			if err != nil {
				t.Fatalf("write error: %v", err)
			}

			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d", code, tc.wantCode)
			}
			lines := strings.SplitN(stdout.String(), "\n", 2)
			if lines[0] != tc.firstLine {
				t.Errorf("first line = %q, want %q", lines[0], tc.firstLine)
			}
			if submitted.RunID != testRunID || submitted.TaskID != testTaskID || submitted.AttemptID != testAttemptID {
				t.Errorf("ids defaulted from HOP_* wrong: %+v", submitted)
			}
			if submitted.IncarnationID != testIncarnationID {
				t.Errorf("incarnation = %q, want the HOP_INCARNATION_ID value (no flag exists for it)", submitted.IncarnationID)
			}
			if submitted.CommitOID != testCommit || submitted.Summary != "did the thing" {
				t.Errorf("payload = %+v", submitted)
			}
		})
	}

	t.Run("explicit id flags beat the environment defaults", func(t *testing.T) {
		other := "99999999-9999-4999-8999-999999999999"
		ctrl := &fakeController{}
		var submitted app.SubmitResultRequest
		ctrl.submitResult = func(req app.SubmitResultRequest) (app.SubmitResultResult, error) {
			submitted = req
			return app.SubmitResultResult{Kind: "accepted", ResultID: testResultID}, nil
		}
		td := newTestDeps(ctrl, workerEnv, t.TempDir())
		var stdout, stderr bytes.Buffer

		if _, err := runResult([]string{"submit", "--summary", "s", "--commit", testCommit, "--run", other, "--task", other, "--attempt", other}, &stdout, &stderr, td.deps); err != nil {
			t.Fatalf("write error: %v", err)
		}

		if submitted.RunID != other || submitted.TaskID != other || submitted.AttemptID != other {
			t.Errorf("flags did not override the environment: %+v", submitted)
		}
	})

	t.Run("missing HOP_STATE_DIR is refused before any store access", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, map[string]string{"HOME": "/home/u"}, t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runResult([]string{"submit", "--summary", "s", "--commit", testCommit}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		if !strings.Contains(stderr.String(), "HOP_STATE_DIR is not set") {
			t.Errorf("stderr = %q", stderr.String())
		}
		if len(td.openCalls) != 0 {
			t.Error("the store was opened without a worker state root")
		}
	})

	usage := []struct {
		name string
		args []string
		want string
	}{
		{name: "no subcommand", args: nil, want: "usage: hop result submit"},
		{name: "unknown subcommand", args: []string{"list"}, want: "usage: hop result submit"},
		{name: "missing summary and commit", args: []string{"submit"}, want: "--summary and --commit are required"},
		{name: "unexpected positional", args: []string{"submit", "--summary", "s", "--commit", testCommit, "x"}, want: "unexpected argument"},
	}
	for _, tc := range usage {
		t.Run(tc.name, func(t *testing.T) {
			td := newTestDeps(&fakeController{}, workerEnv, t.TempDir())
			var stdout, stderr bytes.Buffer

			code, err := runResult(tc.args, &stdout, &stderr, td.deps)
			if err != nil {
				t.Fatalf("write error: %v", err)
			}

			if code != exitUsage {
				t.Errorf("exit code = %d, want %d", code, exitUsage)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.want)
			}
		})
	}
}
