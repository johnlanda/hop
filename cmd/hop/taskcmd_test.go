package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

func managerEnv() map[string]string {
	return map[string]string{
		"HOP_STATE_DIR":      "/state/root",
		"HOP_RUN_ID":         testRunID,
		"HOP_SESSION_ID":     "22222222-2222-4222-8222-222222222222",
		"HOP_INCARNATION_ID": "33333333-3333-4333-8333-333333333333",
	}
}

func TestRunTaskCreate(t *testing.T) {
	instructions := filepath.Join(t.TempDir(), "instructions.md")
	if err := os.WriteFile(instructions, []byte("do the thing"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("accepted prints the created line", func(t *testing.T) {
		ctrl := &fakeController{}
		var req app.CreateTaskRequest
		ctrl.createTask = func(r app.CreateTaskRequest) (app.CreateTaskResult, error) {
			req = r
			return app.CreateTaskResult{Outcome: "accepted", TaskID: "task-1", Seq: 3}, nil
		}
		td := newTestDeps(ctrl, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runTask([]string{"create", "--title", "do X", "--file", instructions, "--depends-on", "dep-1"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK {
			t.Errorf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
		}
		if stdout.String() != "task task-1 t3 created\n" {
			t.Errorf("stdout = %q", stdout.String())
		}
		if req.Title != "do X" || string(req.InstructionsBody) != "do the thing" || len(req.DependsOn) != 1 {
			t.Errorf("request = %+v", req)
		}
		if req.RunID != testRunID || req.SessionID == "" || req.IncarnationID == "" {
			t.Errorf("identities not read from HOP_* env: %+v", req)
		}
	})

	t.Run("a refusal renders refused: <token>", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.createTask = func(app.CreateTaskRequest) (app.CreateTaskResult, error) {
			return app.CreateTaskResult{Outcome: "refused", Reason: app.GrammarReasonNotManager, Detail: "caller is not the run's manager"}, nil
		}
		td := newTestDeps(ctrl, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runTask([]string{"create", "--title", "x", "--file", instructions}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		if !strings.HasPrefix(stdout.String(), "refused: not-manager\n") {
			t.Errorf("stdout = %q", stdout.String())
		}
	})

	t.Run("missing --title or --file is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runTask([]string{"create", "--title", "x"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})
}

func TestRunTaskRetry(t *testing.T) {
	for _, tt := range []struct {
		outcome string
		want    string
	}{
		{outcome: "accepted", want: app.GrammarRetryAcceptedLine(7, 2)},
		{outcome: "duplicate", want: app.GrammarRetryDuplicateLine(7, 2)},
	} {
		t.Run(tt.outcome+" names the task by t<seq>, never the raw id argument", func(t *testing.T) {
			ctrl := &fakeController{}
			var req app.RequestRetryRequest
			ctrl.requestRetry = func(r app.RequestRetryRequest) (app.RequestRetryResult, error) {
				req = r
				return app.RequestRetryResult{Outcome: tt.outcome, TaskSeq: 7, AttemptNumber: 2}, nil
			}
			td := newTestDeps(ctrl, managerEnv(), t.TempDir())
			var stdout, stderr bytes.Buffer

			code, err := runTask([]string{"retry", "--reason", "flaky", "task-9"}, &stdout, &stderr, td.deps)
			if err != nil {
				t.Fatalf("write error: %v", err)
			}
			if code != exitOK || stdout.String() != tt.want+"\n" || stderr.Len() != 0 {
				t.Errorf("code = %d, stdout = %q, stderr = %q; want %q", code, stdout.String(), stderr.String(), tt.want)
			}
			if req.TaskID != "task-9" || req.Reason != "flaky" {
				t.Errorf("request = %+v", req)
			}
		})
	}

	t.Run("missing --reason is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runTask([]string{"retry", "task-9"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})
}

func TestRunPlanClose(t *testing.T) {
	t.Run("accepted prints plan closed", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.closePlan = func(app.ClosePlanRequest) (app.ClosePlanResult, error) {
			return app.ClosePlanResult{Outcome: "accepted"}, nil
		}
		td := newTestDeps(ctrl, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runPlan([]string{"close"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK || stdout.String() != "plan closed\n" {
			t.Errorf("code = %d, stdout = %q", code, stdout.String())
		}
	})

	t.Run("an empty plan refuses", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.closePlan = func(app.ClosePlanRequest) (app.ClosePlanResult, error) {
			return app.ClosePlanResult{Outcome: "refused", Reason: app.GrammarReasonEmptyPlan, Detail: "plan has no implement task"}, nil
		}
		td := newTestDeps(ctrl, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runPlan([]string{"close"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure || !strings.HasPrefix(stdout.String(), "refused: empty-plan\n") {
			t.Errorf("code = %d, stdout = %q", code, stdout.String())
		}
	})

	t.Run("unknown hop task subcommand is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, managerEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runTask([]string{"bogus"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})
}

// TestVerbsRenderTransientRunNotRunning pins the retryable shape of every
// verb whose accepting transaction checks the run state: the transient
// line alone on stdout, the store's value-free detail as one
// "<verb>: <detail>" stderr line, exit 1, and no refusal token anywhere.
func TestVerbsRenderTransientRunNotRunning(t *testing.T) {
	instructions := filepath.Join(t.TempDir(), "instructions.md")
	if err := os.WriteFile(instructions, []byte("do the thing"), 0o600); err != nil {
		t.Fatal(err)
	}
	detail := app.RunNotRunningDetail("launching")
	cases := []struct {
		verb   string
		script func(*fakeController)
		run    func(stdout, stderr *bytes.Buffer, d *deps) (int, error)
	}{
		{
			verb: "hop task create",
			script: func(ctrl *fakeController) {
				ctrl.createTask = func(app.CreateTaskRequest) (app.CreateTaskResult, error) {
					return app.CreateTaskResult{Outcome: string(app.WorkflowTransient), Detail: detail}, nil
				}
			},
			run: func(stdout, stderr *bytes.Buffer, d *deps) (int, error) {
				return runTask([]string{"create", "--title", "x", "--file", instructions, "--request-id", "r1"}, stdout, stderr, d)
			},
		},
		{
			verb: "hop task retry",
			script: func(ctrl *fakeController) {
				ctrl.requestRetry = func(app.RequestRetryRequest) (app.RequestRetryResult, error) {
					return app.RequestRetryResult{Outcome: string(app.WorkflowTransient), Detail: detail}, nil
				}
			},
			run: func(stdout, stderr *bytes.Buffer, d *deps) (int, error) {
				return runTask([]string{"retry", "--reason", "flaky", "task-9"}, stdout, stderr, d)
			},
		},
		{
			verb: "hop plan close",
			script: func(ctrl *fakeController) {
				ctrl.closePlan = func(app.ClosePlanRequest) (app.ClosePlanResult, error) {
					return app.ClosePlanResult{Outcome: string(app.WorkflowTransient), Detail: detail}, nil
				}
			},
			run: func(stdout, stderr *bytes.Buffer, d *deps) (int, error) {
				return runPlan([]string{"close"}, stdout, stderr, d)
			},
		},
		{
			verb: "hop msg send",
			script: func(ctrl *fakeController) {
				ctrl.sendMessage = func(app.SendMessageRequest) (app.SendMessageResult, error) {
					return app.SendMessageResult{Outcome: string(app.MessageTransient), Detail: detail}, nil
				}
			},
			run: func(stdout, stderr *bytes.Buffer, d *deps) (int, error) {
				return runMsg([]string{"send", "--to", "human", "--kind", "question", "--body", "q?"}, stdout, stderr, d)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			ctrl := &fakeController{}
			tc.script(ctrl)
			td := newTestDeps(ctrl, managerEnv(), t.TempDir())
			var stdout, stderr bytes.Buffer

			code, err := tc.run(&stdout, &stderr, td.deps)
			if err != nil {
				t.Fatalf("write error: %v", err)
			}
			if code != exitFailure {
				t.Errorf("exit code = %d, want %d", code, exitFailure)
			}
			if got, want := stdout.String(), "transient: run not yet running; retry\n"; got != want {
				t.Errorf("stdout = %q, want %q", got, want)
			}
			if got, want := stderr.String(), tc.verb+": run is launching, not yet running\n"; got != want {
				t.Errorf("stderr = %q, want %q", got, want)
			}
		})
	}
}

// TestWriteLinesAndExitCodes pins the worker-protocol exit mapping by
// first line: success exits 0, a refusal or a transient line exits 1.
func TestWriteLinesAndExitCodes(t *testing.T) {
	for _, tt := range []struct {
		first string
		want  int
	}{
		{app.GrammarPlanClosedLine, exitOK},
		{app.GrammarRefusalLine(app.GrammarReasonRunNotAccepting), exitFailure},
		{app.GrammarTransientRunNotRunningLine, exitFailure},
		{app.GrammarTransientUndeliveredLine, exitFailure},
	} {
		var stdout bytes.Buffer
		code, err := writeLinesAndExit(&stdout, []string{tt.first, "detail"})
		if err != nil || code != tt.want || stdout.String() != tt.first+"\ndetail\n" {
			t.Errorf("writeLinesAndExit(%q) = %d, %v, stdout %q; want %d", tt.first, code, err, stdout.String(), tt.want)
		}
	}
}
