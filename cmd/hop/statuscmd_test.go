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

	t.Run("a run needing attention carries the blocked marker", func(t *testing.T) {
		attentionCtrl := &fakeController{status: func(app.StatusRequest) (app.StatusResult, error) {
			return app.StatusResult{Runs: []app.RunSummaryView{
				{RunID: testRunID, Sequence: 1, State: "running", NeedsAttention: true},
			}}, nil
		}}
		td := newTestDeps(attentionCtrl, statusEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		if _, err := runStatus(nil, &stdout, &stderr, td.deps); err != nil {
			t.Fatalf("write error: %v", err)
		}

		if !strings.Contains(stdout.String(), "r1 "+testRunID+" running (blocked, needs attention)\n") {
			t.Errorf("output lacks the attention marker:\n%s", stdout.String())
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

	t.Run("an unresolved feature worktree operation renders with its action", func(t *testing.T) {
		featureCtrl := &fakeController{}
		featureCtrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			featureDetail := *detail
			featureDetail.Mode = "feature"
			featureDetail.WorktreeOperations = []app.WorktreeOperationView{{
				OperationID: testOperationID, Branch: "hop/r1/t1a1", State: "reconciling",
				Action: "the checkout of this branch could not be verified; inspect it",
			}, {
				OperationID: testOperationID, State: "reconciling", Action: "the operation's intent names no attempt",
			}}
			return app.StatusResult{Detail: &featureDetail}, nil
		}
		td := newTestDeps(featureCtrl, statusEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runStatus([]string{"-run", testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		want := "  pending ops:   1\n" +
			"  last submit:   transient\n" +
			"  worktree op:   " + testOperationID + " hop/r1/t1a1 (reconciling)\n" +
			"    action:      the checkout of this branch could not be verified; inspect it\n" +
			"  worktree op:   " + testOperationID + " (none) (reconciling)\n" +
			"    action:      the operation's intent names no attempt\n" +
			"  artifact:      "
		if code != exitOK || !strings.Contains(stdout.String(), want) {
			t.Errorf("code = %d, output:\n%s\nwant the block:\n%s", code, stdout.String(), want)
		}
	})

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

// TestRunStatusFeatureDetailBlock pins every section 10 feature-mode
// detail line renderRunDetail renders: the task table (with dependencies,
// attempt count and worktree), the latest integration, guard shortfalls
// (one naming a task, one — verdict-rejected — that never does), the
// section 7 per-mailbox attention lines (in-flight-only, queued-only, a
// task and a human address, attention with a known live-session binding,
// attention with a live address but no known binding, and one mailbox
// with Attention false — no action line for it), a pending human
// question with its exact hop answer invocation, and the per-session
// roles/bindings listing (manager, implementer, terminated reviewer).
func TestRunStatusFeatureDetailBlock(t *testing.T) {
	const (
		t1ID          = "10000000-0000-4000-8000-000000000001"
		t2ID          = "20000000-0000-4000-8000-000000000002"
		t3ID          = "30000000-0000-4000-8000-000000000003"
		integID       = "40000000-0000-4000-8000-000000000004"
		inFlightID    = "50000000-0000-4000-8000-000000000005"
		questionID    = "60000000-0000-4000-8000-000000000006"
		mgrSessionID  = "70000000-0000-4000-8000-000000000007"
		implSessionID = "80000000-0000-4000-8000-000000000008"
		revSessionID  = "90000000-0000-4000-8000-000000000009"
	)
	detail := &app.RunDetailView{
		RunSummaryView: app.RunSummaryView{RunID: testRunID, Sequence: 1, State: "running"},
		Mode:           "feature",
		Tasks: []app.TaskSummaryView{
			{TaskID: t1ID, Seq: 1, Kind: "implement", State: "integrated", AttemptCount: 1},
			{TaskID: t2ID, Seq: 2, Kind: "implement", State: "active", DependsOn: []string{t1ID}, AttemptCount: 1, WorktreePath: "/worktrees/r1/t2a1"},
			{TaskID: t3ID, Seq: 3, Kind: "review", State: "completed", AttemptCount: 1, WorktreePath: "/worktrees/r1/t3a1"},
		},
		LatestIntegration: &app.IntegrationView{
			ID: integID, TaskID: t1ID, SourceCommitOID: "src1", PremergeHeadOID: "pre1", MergeCommitOID: "merge1", State: "integrated",
		},
		GuardShortfalls: []app.GuardShortfallView{
			{Kind: "task-not-integrated", TaskID: t2ID},
			{Kind: "verdict-rejected"},
		},
		Mailboxes: []app.MailboxView{
			{Address: "task:" + t2ID, InFlightMessageID: inFlightID, InFlightAge: 5 * time.Minute, AddressLive: true, Attention: true},
			{Address: "manager", QueuedCount: 1, OldestQueuedAge: 10 * time.Minute, AddressLive: true, Attention: true},
			{Address: "human", QueuedCount: 2, OldestQueuedAge: 3 * time.Hour, AddressLive: true, Attention: true},
			{Address: "task:" + t1ID, QueuedCount: 1, OldestQueuedAge: 20 * time.Minute, AddressLive: false, Attention: false},
		},
		PendingQuestions: []app.PendingQuestionView{
			{MessageID: questionID, Age: 45 * time.Second, BodyPath: "/state/runs/r1/messages/q.md"},
		},
		Sessions: []app.SessionView{
			{SessionID: mgrSessionID, Role: "manager", State: "active"},
			{SessionID: implSessionID, Role: "implementer", State: "active", TaskID: t2ID, AttemptNumber: 1, BindingSummary: "ws2/tab2/pane2"},
			{SessionID: revSessionID, Role: "reviewer", State: "terminated", TaskID: t3ID, AttemptNumber: 1},
		},
	}

	var stdout bytes.Buffer
	code, err := renderRunDetail(&stdout, detail)
	if err != nil {
		t.Fatalf("renderRunDetail() error = %v", err)
	}
	if code != exitOK {
		t.Fatalf("exit code = %d", code)
	}
	out := stdout.String()

	want := []string{
		"  task t1 " + t1ID + ": kind=implement state=integrated deps=(none) attempts=1 worktree=(none)",
		"  task t2 " + t2ID + ": kind=implement state=active deps=t1 attempts=1 worktree=/worktrees/r1/t2a1",
		"  task t3 " + t3ID + ": kind=review state=completed deps=(none) attempts=1 worktree=/worktrees/r1/t3a1",
		"  integration " + integID + ": task=t1 state=integrated source=src1 premerge=pre1 merge=merge1",
		"  shortfall: task-not-integrated t2 " + t2ID,
		"  shortfall: verdict-rejected",
		"  attention: messages pending for task:" + t2ID + " (t2): in-flight 5m0s (message " + inFlightID + ")",
		"    action:      open ws2/tab2/pane2 and check that the agent is following its polling instructions",
		"  attention: messages pending for manager: queued 1, oldest 10m0s",
		"    action:      open that session's pane and check that the agent is following its polling instructions",
		"  attention: messages pending for human: queued 2, oldest 3h0m0s",
		"    action:      answer pending human questions with hop answer",
		"  attention: messages pending for task:" + t1ID + " (t1): queued 1, oldest 20m0s",
		"  question " + questionID + " age=45s body: /state/runs/r1/messages/q.md",
		"    hop answer " + questionID + " --file <path>",
		"  session " + mgrSessionID + ": role=manager state=active task=(none) attempt=0 binding=(none)",
		"  session " + implSessionID + ": role=implementer state=active task=t2 attempt=1 binding=ws2/tab2/pane2",
		"  session " + revSessionID + ": role=reviewer state=terminated task=t3 attempt=1 binding=(none)",
	}
	for _, line := range want {
		if !strings.Contains(out, line) {
			t.Errorf("output missing line %q; got:\n%s", line, out)
		}
	}
	// The Attention=false mailbox (task:t1) gets no action line: exactly
	// one "action:" line per Attention=true mailbox above (three).
	if n := strings.Count(out, "action:"); n != 3 {
		t.Errorf(`"action:" count = %d, want 3; got:\n%s`, n, out)
	}
}

// TestRunStatusFeatureDetailEmptyMailboxes proves an idle feature run (no
// mailbox holds a queued or in-flight message) renders no attention line
// at all — Mailboxes is empty, never a list of zero-obligation entries.
func TestRunStatusFeatureDetailEmptyMailboxes(t *testing.T) {
	detail := &app.RunDetailView{
		RunSummaryView: app.RunSummaryView{RunID: testRunID, Sequence: 1, State: "running"},
		Mode:           "feature",
	}
	var stdout bytes.Buffer
	if _, err := renderRunDetail(&stdout, detail); err != nil {
		t.Fatalf("renderRunDetail() error = %v", err)
	}
	if strings.Contains(stdout.String(), "attention:") {
		t.Errorf("empty-mailbox feature run rendered an attention line:\n%s", stdout.String())
	}
}
