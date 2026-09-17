package app_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// closePlanDirectly closes the plan through the domain rule, as the
// manager's accepted `hop plan close` would.
func closePlanDirectly(t *testing.T, tc *testController, runID identity.RunID) {
	t.Helper()
	row := tc.Store.Runs[runID]
	closed, err := row.value.ClosePlan(true, tc.Clock.Now())
	if err != nil {
		t.Fatalf("ClosePlan() error = %v", err)
	}
	row.value = closed
	row.revision++
}

// seedReviewerFor binds a running reviewer attempt and session to the
// controller-created review task.
func seedReviewerFor(t *testing.T, f *integrationFixture, reviewTaskID identity.TaskID) *reviewFixture {
	t.Helper()
	tc := f.tc
	now := tc.Clock.Now()
	tc.Store.Tasks[reviewTaskID].value.State = run.TaskActive

	attemptID, err := identity.ParseAttemptID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse attempt id: %v", err)
	}
	attempt, err := run.NewAttempt(attemptID, reviewTaskID, 1, now)
	if err != nil {
		t.Fatalf("NewAttempt() error = %v", err)
	}
	attempt.State = run.AttemptRunning
	tc.Store.Attempts[attemptID] = &entityRow[run.Attempt]{value: attempt, revision: 1}

	sessionID, err := identity.ParseSessionID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse session id: %v", err)
	}
	reviewer, err := run.NewChildSession(sessionID, f.fr.RunID, attemptID, run.RoleReviewer, tc.Store.Sessions[f.fr.ManagerID].value, run.HarnessClaude, now)
	if err != nil {
		t.Fatalf("NewChildSession() error = %v", err)
	}
	if reviewer, err = reviewer.Launch(now); err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	if reviewer, err = reviewer.ConfirmActive(now); err != nil {
		t.Fatalf("ConfirmActive() error = %v", err)
	}
	tc.Store.Sessions[sessionID] = &entityRow[run.Session]{value: reviewer, revision: 1}

	incarnationID, err := identity.ParseIncarnationID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse incarnation id: %v", err)
	}
	binding := run.NewRuntimeBinding(sessionID, incarnationID, "", fakeServerToken(1), "ws-r", "tab-r", "pane-r", "label-r", run.LaunchInitial, now)
	tc.Store.Bindings[sessionID] = append(tc.Store.Bindings[sessionID], binding)
	tc.Store.LaunchClaims[incarnationID] = app.LaunchClaim{
		IncarnationID: incarnationID, RunID: f.fr.RunID, SessionID: sessionID, AttemptID: attemptID,
		Executable: "/usr/local/bin/claude", PID: 556, State: app.LaunchClaimExeced, ClaimedAt: now,
	}
	subject := tc.Store.Tasks[reviewTaskID].value.SubjectCommitOID
	return &reviewFixture{tc: tc, fr: f.fr, TaskID: reviewTaskID, AttemptID: attemptID, SessionID: sessionID, IncarnationID: incarnationID, SubjectCommit: subject}
}

// findReviewTask returns the run's review task, if any.
func findReviewTask(tc *testController, runID identity.RunID) (identity.TaskID, bool) {
	for id, row := range tc.Store.Tasks {
		if row.value.RunID == runID && row.value.Kind == run.TaskKindReview {
			return id, true
		}
	}
	return "", false
}

func TestGuardEnforcement(t *testing.T) {
	// A scripted "manager" calling every verb it owns cannot complete a
	// run without real receipts: no verb writes check receipts, verdicts
	// or integration states except their own pipelines, and prose changes
	// nothing.
	f := newIntegrationFixture(t, false)
	ctx := context.Background()

	// The scripted manager's complete verb set: task create, retry, plan
	// close, msg send/next/ack. It claims everything passed.
	created, err := f.tc.Store.CreateTask(ctx, app.TaskCreate{
		ID: mintTaskID(t, f.tc), RunID: f.fr.RunID, Session: f.fr.ManagerID, IncarnationID: f.fr.ManagerIncarnation,
		Title: "declare victory", InstructionsDigest: "d", RequestID: "gt-1",
	})
	if err != nil || created.Outcome != app.WorkflowAccepted {
		t.Fatalf("CreateTask() = %+v err=%v", created, err)
	}
	closePlanDirectly(t, f.tc, f.fr.RunID)
	send, err := f.tc.Store.SendMessage(ctx, app.MessageSend{
		ID: mintMessageID(t, f.tc), RunID: f.fr.RunID,
		Sender: run.SessionPrincipal(f.fr.ManagerID), SenderAddress: run.ManagerAddress(),
		IncarnationID: f.fr.ManagerIncarnation, Recipient: run.TaskAddress(created.TaskID),
		Kind: run.MessageInfo, BodyPath: "/state/b", BodyDigest: "d", BodyBytes: 20, RequestID: "gt-2",
	})
	if err != nil || send.Kind != app.MessageAccepted {
		t.Fatalf("SendMessage() = %+v err=%v", send, err)
	}

	// The guard sees only evidence rows: nothing above satisfied any of
	// them.
	ready, missing, err := f.tc.Controller.EvaluateRunReadiness(ctx, f.fr.Handle)
	if err != nil {
		t.Fatalf("EvaluateRunReadiness() error = %v", err)
	}
	if ready {
		t.Fatalf("readiness held with no integration, no check receipt and no verdict")
	}
	joined := strings.Join(missing, " ")
	for _, want := range []string{string(run.ShortfallTaskNotIntegrated), string(run.ShortfallCheckMissing), string(run.ShortfallVerdictMissing)} {
		if !strings.Contains(joined, want) {
			t.Errorf("shortfalls %v lack %s", missing, want)
		}
	}
	report, err := f.tc.Controller.DriveCompletion(ctx, f.fr.Handle)
	if err != nil {
		t.Fatalf("DriveCompletion() error = %v", err)
	}
	if report.Completed || report.RunState != string(run.RunRunning) {
		t.Fatalf("DriveCompletion() = %+v; the run must stay running without real receipts", report)
	}
	// Run.Complete is unreachable except through the guard: the run never
	// left running, so the state machine cannot even be asked.
	if got := f.tc.Store.Runs[f.fr.RunID].value.State; got != run.RunRunning {
		t.Fatalf("run state = %s, want running", got)
	}
}

func mintTaskID(t *testing.T, tc *testController) identity.TaskID {
	t.Helper()
	id, err := identity.ParseTaskID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse task id: %v", err)
	}
	return id
}

func mintMessageID(t *testing.T, tc *testController) identity.MessageID {
	t.Helper()
	id, err := identity.ParseMessageID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse message id: %v", err)
	}
	return id
}

func TestEnsureReviewTask(t *testing.T) {
	t.Run("created exactly once per head, only when due", func(t *testing.T) {
		f := newIntegrationFixture(t, false)
		ctx := context.Background()

		// Not due: plan open.
		created, err := f.tc.Controller.EnsureReviewTask(ctx, f.fr.Handle)
		if err != nil {
			t.Fatalf("EnsureReviewTask() error = %v", err)
		}
		if created {
			t.Fatalf("a review task was created with the plan open and nothing integrated")
		}

		closePlanDirectly(t, f.tc, f.fr.RunID)
		// Not due: the implement task is not integrated yet.
		if created, err = f.tc.Controller.EnsureReviewTask(ctx, f.fr.Handle); err != nil || created {
			t.Fatalf("EnsureReviewTask() = %v err=%v with an unintegrated task", created, err)
		}

		f.driveUntil(t, string(run.IntegrationIntegrated), 6)
		created, err = f.tc.Controller.EnsureReviewTask(ctx, f.fr.Handle)
		if err != nil {
			t.Fatalf("EnsureReviewTask() error = %v", err)
		}
		if !created {
			t.Fatalf("no review task created once the plan closed and every implement task integrated")
		}
		reviewTaskID, ok := findReviewTask(f.tc, f.fr.RunID)
		if !ok {
			t.Fatalf("review task not found")
		}
		review := f.tc.Store.Tasks[reviewTaskID].value
		head := f.git.ref(integrationRefName)
		if review.SubjectCommitOID != head || review.SubjectTreeOID != f.git.treeOf(head) {
			t.Fatalf("review subject = (%s, %s), want the current head's object IDs", review.SubjectCommitOID, review.SubjectTreeOID)
		}
		if review.State != run.TaskReady {
			t.Fatalf("review task state = %s, want created ready", review.State)
		}

		// Exactly once per head.
		if created, err = f.tc.Controller.EnsureReviewTask(ctx, f.fr.Handle); err != nil || created {
			t.Fatalf("EnsureReviewTask() = %v err=%v, want no second task for the same head", created, err)
		}
	})
}

func TestDriveCompletion(t *testing.T) {
	// integratedFixture drives one task through the full integration flow
	// and accepts an approving verdict, leaving the run ready.
	integratedFixture := func(t *testing.T) (*integrationFixture, *reviewFixture) {
		t.Helper()
		f := newIntegrationFixture(t, false)
		f.tc.Controller.Reviews = &featureStore{fakeStore: f.tc.Store}
		closePlanDirectly(t, f.tc, f.fr.RunID)
		f.driveUntil(t, string(run.IntegrationIntegrated), 6)
		if created, err := f.tc.Controller.EnsureReviewTask(context.Background(), f.fr.Handle); err != nil || !created {
			t.Fatalf("EnsureReviewTask() = %v err=%v", created, err)
		}
		reviewTaskID, _ := findReviewTask(f.tc, f.fr.RunID)
		rf := seedReviewerFor(t, f, reviewTaskID)
		result, err := f.tc.Controller.SubmitReviewVerdict(context.Background(), app.SubmitReviewRequest{
			RunID: f.fr.RunID.String(), TaskID: rf.TaskID.String(), AttemptID: rf.AttemptID.String(),
			SessionID: rf.SessionID.String(), IncarnationID: rf.IncarnationID.String(),
			Verdict: "approve", SubjectCommitOID: rf.SubjectCommit, ReasonsBody: []byte("approved"),
		})
		if err != nil || result.Outcome != string(app.ReviewAccepted) {
			t.Fatalf("SubmitReviewVerdict() = %+v err=%v", result, err)
		}
		return f, rf
	}

	t.Run("readiness completes the run after manager retirement and re-validation", func(t *testing.T) {
		f, rf := integratedFixture(t)
		f.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound }

		report, err := f.tc.Controller.DriveCompletion(context.Background(), f.fr.Handle)
		if err != nil {
			t.Fatalf("DriveCompletion() error = %v", err)
		}
		if !report.Ready || !report.Completed || report.RunState != string(run.RunCompleted) {
			t.Fatalf("DriveCompletion() = %+v, want ready, completed", report)
		}
		if got := f.tc.Store.Sessions[f.fr.ManagerID].value.State; got != run.SessionTerminated {
			t.Fatalf("manager state = %s, want retired before completed", got)
		}
		if got := f.tc.Store.Sessions[rf.SessionID].value.State; got != run.SessionTerminated {
			t.Fatalf("reviewer state = %s, want retired (a straggler closes under the same rule)", got)
		}
		// Worktrees and branches are preserved at completion.
		if head := f.git.ref(integrationRefName); head == "" {
			t.Fatalf("integration branch missing after completion")
		}
	})

	t.Run("an approved head reads ready to the guard and verdict-clear to status", func(t *testing.T) {
		f, rf := integratedFixture(t)
		head := f.git.ref(integrationRefName)
		if tree := f.git.treeOf(head); tree == "" || tree == head || rf.SubjectCommit != head {
			t.Fatalf("head %s tree %q reviewed subject %s; want an approve of the head, its tree distinct from its commit", head, tree, rf.SubjectCommit)
		}
		ready, missing, err := f.tc.Controller.EvaluateRunReadiness(context.Background(), f.fr.Handle)
		if err != nil || !ready || len(missing) != 0 {
			t.Fatalf("EvaluateRunReadiness() = %v %v err=%v, want ready with nothing missing", ready, missing, err)
		}
		// The status read holds no check receipt, so check-missing is its
		// one shortfall; the approve of the head clears every verdict one.
		view := mustStatusDetail(t, f.tc, f.fr.RunID)
		if len(view.GuardShortfalls) != 1 || !containsShortfall(view.GuardShortfalls, string(run.ShortfallCheckMissing)) {
			t.Fatalf("status GuardShortfalls = %+v, want exactly check-missing", view.GuardShortfalls)
		}
	})

	t.Run("completion retirement is observed, never declared", func(t *testing.T) {
		f, _ := integratedFixture(t)
		// The manager still idles at its composer: retirement dispatches
		// but the run must not complete.
		f.tc.Runtime.InspectPaneFn = func(paneID string) (app.PaneProcess, error) {
			if paneID == "pane-mgr" {
				return app.PaneProcess{ShellPID: 1, Foreground: []app.ProcessInfo{{PID: 42, Argv: []string{"/usr/local/bin/claude"}, Cmdline: "claude"}}}, nil
			}
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		report, err := f.tc.Controller.DriveCompletion(context.Background(), f.fr.Handle)
		if err != nil {
			t.Fatalf("DriveCompletion() error = %v", err)
		}
		if report.Completed {
			t.Fatalf("completed with the manager still observed alive")
		}
		if report.RunState != string(run.RunCompleting) || len(report.Outstanding) == 0 {
			t.Fatalf("DriveCompletion() = %+v, want completing with retirement outstanding", report)
		}
	})

	t.Run("the final transaction re-validates readiness against then-current evidence", func(t *testing.T) {
		f, rf := integratedFixture(t)
		f.tc.Runtime.InspectPaneFn = func(paneID string) (app.PaneProcess, error) {
			if paneID == "pane-mgr" {
				return app.PaneProcess{ShellPID: 1, Foreground: []app.ProcessInfo{{PID: 42, Argv: []string{"/usr/local/bin/claude"}, Cmdline: "claude"}}}, nil
			}
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		// Round 1 enters completing; the manager still lives.
		report, err := f.tc.Controller.DriveCompletion(context.Background(), f.fr.Handle)
		if err != nil || report.RunState != string(run.RunCompleting) {
			t.Fatalf("round 1 = %+v err=%v, want completing", report, err)
		}
		// The verdict evidence vanishes before the final transaction (the
		// re-validation window made concrete).
		delete(f.tc.Store.Reviews, rf.AttemptID)
		f.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound }

		report, err = f.tc.Controller.DriveCompletion(context.Background(), f.fr.Handle)
		if err != nil {
			t.Fatalf("round 2 error = %v", err)
		}
		if report.Completed {
			t.Fatalf("completed over re-validation failure")
		}
		if report.RunState != string(run.RunRunning) {
			t.Fatalf("run state = %s, want returned to running", report.RunState)
		}
		if !slices.Contains(report.Missing, string(run.ShortfallVerdictMissing)) {
			t.Fatalf("missing = %v, want the verdict shortfall named", report.Missing)
		}
	})

	t.Run("stop precedence: a stop observed in the completion window never yields completed", func(t *testing.T) {
		f, _ := integratedFixture(t)
		f.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{}, app.ErrPaneNotFound }
		row := f.tc.Store.Runs[f.fr.RunID]
		row.value = row.value.RequestStop(f.tc.Clock.Now())
		row.revision++

		report, err := f.tc.Controller.DriveCompletion(context.Background(), f.fr.Handle)
		if err != nil {
			t.Fatalf("DriveCompletion() error = %v", err)
		}
		if report.Completed {
			t.Fatalf("completed under a held stop request")
		}
		if got := f.tc.Store.Runs[f.fr.RunID].value.State; got == run.RunCompleted {
			t.Fatalf("run state = %s; stop precedence forbids completed", got)
		}
	})
}
