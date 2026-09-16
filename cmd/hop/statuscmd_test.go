package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

func statusEnv() map[string]string { return map[string]string{"HOME": "/home/u"} }

func TestRunStatusListing(t *testing.T) {
	runs := []app.RunSummaryView{
		{RunID: testRunID, Sequence: 1, State: "running"},
		{RunID: "22222222-2222-4222-8222-222222222222", Sequence: 2, State: "completed"},
		{RunID: "33333333-3333-4333-8333-333333333333", Sequence: 3, State: "stopping", StopRequested: true, Reconciling: true},
	}
	ctrl := &fakeController{}
	ctrl.status = func(req app.StatusRequest) (app.StatusResult, error) {
		if req.RunID != "" {
			t.Errorf("listing must not ask for one run; got %q", req.RunID)
		}
		return app.StatusResult{Runs: runs}, nil
	}

	t.Run("default lists non-terminal runs only", func(t *testing.T) {
		td := newTestDeps(ctrl, statusEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runStatus(nil, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitOK {
			t.Errorf("exit code = %d, want %d (state is data, not an exit code)", code, exitOK)
		}
		out := stdout.String()
		if !strings.Contains(out, "r1 "+testRunID+" running\n") {
			t.Errorf("output lacks the running run:\n%s", out)
		}
		if strings.Contains(out, "completed") {
			t.Errorf("a terminal run leaked into the default listing:\n%s", out)
		}
		if !strings.Contains(out, "r3 33333333-3333-4333-8333-333333333333 stopping (stop requested, reconciling)\n") {
			t.Errorf("output lacks the condition markers:\n%s", out)
		}
	})

	t.Run("-all includes terminal runs", func(t *testing.T) {
		td := newTestDeps(ctrl, statusEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runStatus([]string{"-all"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitOK {
			t.Errorf("exit code = %d", code)
		}
		if !strings.Contains(stdout.String(), "r2 22222222-2222-4222-8222-222222222222 completed\n") {
			t.Errorf("-all output lacks the completed run:\n%s", stdout.String())
		}
	})

	t.Run("an empty listing says so", func(t *testing.T) {
		empty := &fakeController{status: func(app.StatusRequest) (app.StatusResult, error) {
			return app.StatusResult{}, nil
		}}
		td := newTestDeps(empty, statusEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		if _, err := runStatus(nil, &stdout, &stderr, td.deps); err != nil {
			t.Fatalf("write error: %v", err)
		}

		if !strings.Contains(stdout.String(), "no runs (use -all to include finished runs)") {
			t.Errorf("output = %q", stdout.String())
		}
	})
}

func TestRunStatusDetail(t *testing.T) {
	detail := &app.RunDetailView{
		RunSummaryView:     app.RunSummaryView{RunID: testRunID, Sequence: 1, State: "running"},
		TaskState:          "active",
		AttemptState:       "running",
		WorktreePath:       "/worktrees/run-1",
		BindingSummary:     "ws/tab/pane",
		ClaimState:         "execed",
		SeedEvidence:       "workspace trust seeded for /worktrees/run-1",
		PendingOps:         1,
		LastSubmission:     "transient",
		Artifacts:          []string{"/state/runs/x/artifacts/assignment.md"},
		LastCheckOperation: testOperationID,
		LastCheckState:     "failed",
		LastCheckUnknown:   true,
		LastCheckDetail:    "outcome unknown after takeover",
		LastCheckEvidence:  []string{"/state/runs/x/checks/op/stdout"},
		LastCheckOptions:   "inspect the retained evidence at the listed paths",
	}
	ctrl := &fakeController{}
	ctrl.status = func(req app.StatusRequest) (app.StatusResult, error) {
		if req.RunID == "" {
			return app.StatusResult{Runs: []app.RunSummaryView{{RunID: testRunID, Sequence: 1, State: "running"}}}, nil
		}
		if req.RunID != testRunID {
			t.Errorf("detail requested for %q", req.RunID)
		}
		return app.StatusResult{Detail: detail}, nil
	}

	t.Run("full detail block renders every populated field", func(t *testing.T) {
		td := newTestDeps(ctrl, statusEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runStatus([]string{"-run", testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitOK {
			t.Errorf("exit code = %d (state is data, not an exit code)", code)
		}
		out := stdout.String()
		for _, want := range []string{
			"run r1 " + testRunID,
			"workflow:      solo",
			"task:          active",
			"attempt:       running",
			"worktree:      /worktrees/run-1",
			"binding:       ws/tab/pane",
			"launch claim:  execed",
			"trust seed:    workspace trust seeded for /worktrees/run-1",
			"pending ops:   1",
			"last submit:   transient",
			"artifact:      /state/runs/x/artifacts/assignment.md",
			"last check:    " + testOperationID + " (failed)",
			"evidence:    /state/runs/x/checks/op/stdout",
			"unknown outcome — options: inspect the retained evidence",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("detail lacks %q; got:\n%s", want, out)
			}
		}
	})

	t.Run("a feature-mode run renders workflow: feature", func(t *testing.T) {
		featureCtrl := &fakeController{}
		featureCtrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			featureDetail := *detail
			featureDetail.Mode = "feature"
			return app.StatusResult{Detail: &featureDetail}, nil
		}
		td := newTestDeps(featureCtrl, statusEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runStatus([]string{"-run", testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK || !strings.Contains(stdout.String(), "workflow:      feature") {
			t.Errorf("code = %d, output:\n%s", code, stdout.String())
		}
	})

	retiredAt := time.Date(2026, 9, 16, 10, 30, 0, 0, time.UTC)
	targetCases := []struct {
		name      string
		mode      string
		target    string
		retiredAt *time.Time
		want      []string
		absent    []string
	}{
		{
			name: "a feature run with a target, not yet retired, states when and warns about ignored files",
			mode: "feature", target: "refs/heads/main",
			want: []string{
				"  target:        refs/heads/main\n",
				"  worktrees:     not retired (removed once the integration branch is merged into refs/heads/main; removal deletes ignored files such as build output; commit anything you want to keep)\n",
			},
		},
		{
			name: "a feature run frozen on a detached HEAD never retires",
			mode: "feature",
			want: []string{
				"  target:        none (detached HEAD at freeze; worktrees are never retired automatically)\n",
				"  worktrees:     kept (no target branch)\n",
			},
		},
		{
			name: "a retired feature run renders the fact",
			mode: "feature", target: "refs/heads/main", retiredAt: &retiredAt,
			want: []string{"  target:        refs/heads/main\n", "  worktrees:     retired 2026-09-16T10:30:00Z\n"},
		},
		{
			name:   "a solo run renders neither line",
			absent: []string{"target:", "worktrees:"},
		},
	}
	for _, tc := range targetCases {
		t.Run(tc.name, func(t *testing.T) {
			targetCtrl := &fakeController{}
			targetCtrl.status = func(app.StatusRequest) (app.StatusResult, error) {
				targetDetail := *detail
				targetDetail.Mode = tc.mode
				targetDetail.TargetBranch = tc.target
				targetDetail.WorktreesRetiredAt = tc.retiredAt
				return app.StatusResult{Detail: &targetDetail}, nil
			}
			td := newTestDeps(targetCtrl, statusEnv(), t.TempDir())
			var stdout, stderr bytes.Buffer

			code, err := runStatus([]string{"-run", testRunID}, &stdout, &stderr, td.deps)
			if err != nil {
				t.Fatalf("write error: %v", err)
			}
			if code != exitOK {
				t.Fatalf("exit code = %d", code)
			}
			out := stdout.String()
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("detail lacks %q; got:\n%s", want, out)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(out, absent) {
					t.Errorf("detail carries %q; got:\n%s", absent, out)
				}
			}
			if tc.mode == "feature" && !strings.Contains(out, "  workflow:      feature\n  target:") {
				t.Errorf("the target line does not follow the workflow line:\n%s", out)
			}
		})
	}

	t.Run("-run accepts the r<seq> label", func(t *testing.T) {
		td := newTestDeps(ctrl, statusEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runStatus([]string{"-run", "r1"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitOK || !strings.Contains(stdout.String(), "run r1 "+testRunID) {
			t.Errorf("code = %d, output:\n%s", code, stdout.String())
		}
	})

	t.Run("an unknown label is a usage error", func(t *testing.T) {
		td := newTestDeps(ctrl, statusEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runStatus([]string{"-run", "r9"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
		if !strings.Contains(stderr.String(), "no run r9") {
			t.Errorf("stderr = %q", stderr.String())
		}
	})

	t.Run("positional arguments are refused", func(t *testing.T) {
		td := newTestDeps(ctrl, statusEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runStatus([]string{"extra"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitUsage || !strings.Contains(stderr.String(), "unexpected argument") {
			t.Errorf("code = %d, stderr = %q", code, stderr.String())
		}
	})
}
