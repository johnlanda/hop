package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// mustStatusDetail renders hop status -run for runID and fails the test on
// any error or a missing detail block.
func mustStatusDetail(t *testing.T, tc *testController, runID identity.RunID) app.RunDetailView {
	t.Helper()
	result, err := tc.Controller.Status(context.Background(), app.StatusRequest{RunID: runID.String()})
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if result.Detail == nil {
		t.Fatalf("Status() Detail is nil")
	}
	return *result.Detail
}

func TestStatusFeatureModeTaskTable(t *testing.T) {
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	taskA := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
	taskB := seedImplementTask(t, tc, fr.RunID, 2, "B", true, run.TaskPending)
	edge, err := run.NewTaskDependency(taskB, taskA, tc.Clock.Now())
	if err != nil {
		t.Fatalf("NewTaskDependency() error = %v", err)
	}
	tc.Store.TaskDependencies = append(tc.Store.TaskDependencies, edge)
	seedWorkerSession(t, tc, fr, taskA) // gives task A one reserved attempt

	view := mustStatusDetail(t, tc, fr.RunID)
	if len(view.Tasks) != 2 {
		t.Fatalf("Tasks = %+v, want exactly 2 rows", view.Tasks)
	}
	byID := map[string]app.TaskSummaryView{}
	for _, row := range view.Tasks {
		byID[row.TaskID] = row
	}
	a, ok := byID[taskA.String()]
	if !ok {
		t.Fatalf("task A missing from Tasks: %+v", view.Tasks)
	}
	if a.Seq != 1 || a.Kind != "implement" || a.State != string(run.TaskActive) || a.AttemptCount != 1 {
		t.Fatalf("task A row = %+v, want seq=1 kind=implement state=active attempts=1", a)
	}
	if len(a.DependsOn) != 0 {
		t.Fatalf("task A DependsOn = %v, want none", a.DependsOn)
	}
	b, ok := byID[taskB.String()]
	if !ok {
		t.Fatalf("task B missing from Tasks: %+v", view.Tasks)
	}
	if b.Seq != 2 || b.State != string(run.TaskPending) || b.AttemptCount != 0 {
		t.Fatalf("task B row = %+v, want seq=2 state=pending attempts=0", b)
	}
	if len(b.DependsOn) != 1 || b.DependsOn[0] != taskA.String() {
		t.Fatalf("task B DependsOn = %v, want [%s]", b.DependsOn, taskA)
	}
}

func TestStatusLatestIntegration(t *testing.T) {
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	taskA := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskIntegrating)
	now := tc.Clock.Now()

	integrationID, err := identity.ParseIntegrationID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse integration id: %v", err)
	}
	resultID, err := identity.ParseResultID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse result id: %v", err)
	}
	integ := run.NewIntegration(integrationID, fr.RunID, taskA, resultID, "source-oid", "premerge-oid", now)
	integ, err = integ.EnterChecking("merge-oid", now)
	if err != nil {
		t.Fatalf("EnterChecking() error = %v", err)
	}
	integ, err = integ.Integrate(now)
	if err != nil {
		t.Fatalf("Integrate() error = %v", err)
	}
	tc.Store.Integrations[integrationID] = &entityRow[run.Integration]{value: integ, revision: 1}

	view := mustStatusDetail(t, tc, fr.RunID)
	if view.LatestIntegration == nil {
		t.Fatalf("LatestIntegration is nil, want the seeded row")
	}
	if view.LatestIntegration.ID != integrationID.String() || view.LatestIntegration.TaskID != taskA.String() ||
		view.LatestIntegration.MergeCommitOID != "merge-oid" || view.LatestIntegration.State != string(run.IntegrationIntegrated) {
		t.Fatalf("LatestIntegration = %+v, want the seeded integrated row", view.LatestIntegration)
	}
}

func TestStatusGuardShortfalls(t *testing.T) {
	t.Run("an open plan reports plan-open alongside the always-missing check", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)

		view := mustStatusDetail(t, tc, fr.RunID)
		want := map[string]bool{"plan-open": true, "check-missing": true, "verdict-missing": true}
		if len(view.GuardShortfalls) != len(want) {
			t.Fatalf("GuardShortfalls = %+v, want exactly %v", view.GuardShortfalls, want)
		}
		for _, s := range view.GuardShortfalls {
			if !want[s.Kind] {
				t.Fatalf("unexpected shortfall %+v", s)
			}
		}
	})

	t.Run("a closed plan with an unintegrated task reports task-not-integrated", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskA := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskActive)
		rRow := tc.Store.Runs[fr.RunID]
		closed, err := rRow.value.ClosePlan(true, tc.Clock.Now())
		if err != nil {
			t.Fatalf("ClosePlan() error = %v", err)
		}
		rRow.value = closed

		view := mustStatusDetail(t, tc, fr.RunID)
		var sawTaskNotIntegrated, sawPlanOpen bool
		for _, s := range view.GuardShortfalls {
			switch s.Kind {
			case "task-not-integrated":
				sawTaskNotIntegrated = true
				if s.TaskID != taskA.String() {
					t.Fatalf("task-not-integrated TaskID = %s, want %s", s.TaskID, taskA)
				}
			case "plan-open":
				sawPlanOpen = true
			}
		}
		if !sawTaskNotIntegrated {
			t.Fatalf("GuardShortfalls = %+v, want task-not-integrated", view.GuardShortfalls)
		}
		if sawPlanOpen {
			t.Fatalf("GuardShortfalls = %+v, want plan-open cleared once the plan is closed", view.GuardShortfalls)
		}
	})

	t.Run("verdict shortfalls track the latest accepted review against the integrated head", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskA := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskIntegrated)
		rRow := tc.Store.Runs[fr.RunID]
		closed, err := rRow.value.ClosePlan(true, tc.Clock.Now())
		if err != nil {
			t.Fatalf("ClosePlan() error = %v", err)
		}
		rRow.value = closed

		integrationID, err := identity.ParseIntegrationID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse integration id: %v", err)
		}
		resultID, err := identity.ParseResultID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse result id: %v", err)
		}
		now := tc.Clock.Now()
		integ := run.NewIntegration(integrationID, fr.RunID, taskA, resultID, "source-oid", "premerge-oid", now)
		integ, err = integ.EnterChecking("head-oid", now)
		if err != nil {
			t.Fatalf("EnterChecking() error = %v", err)
		}
		integ, err = integ.Integrate(now)
		if err != nil {
			t.Fatalf("Integrate() error = %v", err)
		}
		tc.Store.Integrations[integrationID] = &entityRow[run.Integration]{value: integ, revision: 1}

		attemptID, err := identity.ParseAttemptID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse attempt id: %v", err)
		}
		reviewID, err := identity.ParseReviewID(tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse review id: %v", err)
		}

		// An approved verdict against the exact head: task-not-integrated,
		// plan-open and every verdict shortfall clear. check-missing
		// remains — this fake tracks no combined-candidate check receipt
		// (see guardShortfallsLocked's doc comment), so it never clears.
		tc.Store.Reviews[attemptID] = run.Review{
			ID: reviewID, RunID: fr.RunID, TaskID: taskA, AttemptID: attemptID,
			SubjectCommitOID: "head-oid", SubjectTreeOID: "head-oid", Verdict: run.VerdictApprove, SubmittedAt: now,
		}
		view := mustStatusDetail(t, tc, fr.RunID)
		if len(view.GuardShortfalls) != 1 || view.GuardShortfalls[0].Kind != "check-missing" {
			t.Fatalf("GuardShortfalls = %+v, want exactly check-missing", view.GuardShortfalls)
		}

		// A reject verdict against the same head.
		tc.Store.Reviews[attemptID] = run.Review{
			ID: reviewID, RunID: fr.RunID, TaskID: taskA, AttemptID: attemptID,
			SubjectCommitOID: "head-oid", SubjectTreeOID: "head-oid", Verdict: run.VerdictReject, SubmittedAt: now,
		}
		view = mustStatusDetail(t, tc, fr.RunID)
		if !containsShortfall(view.GuardShortfalls, "verdict-rejected") {
			t.Fatalf("GuardShortfalls = %+v, want verdict-rejected", view.GuardShortfalls)
		}

		// An approval bound to a superseded subject.
		tc.Store.Reviews[attemptID] = run.Review{
			ID: reviewID, RunID: fr.RunID, TaskID: taskA, AttemptID: attemptID,
			SubjectCommitOID: "stale-oid", SubjectTreeOID: "stale-oid", Verdict: run.VerdictApprove, SubmittedAt: now,
		}
		view = mustStatusDetail(t, tc, fr.RunID)
		if !containsShortfall(view.GuardShortfalls, "verdict-stale-subject") {
			t.Fatalf("GuardShortfalls = %+v, want verdict-stale-subject", view.GuardShortfalls)
		}
	})
}

func containsShortfall(shortfalls []app.GuardShortfallView, kind string) bool {
	for _, s := range shortfalls {
		if s.Kind == kind {
			return true
		}
	}
	return false
}

// shortfallOf finds shortfalls' entry of kind, failing the test if absent.
func shortfallOf(t *testing.T, shortfalls []app.GuardShortfallView, kind string) app.GuardShortfallView {
	t.Helper()
	for _, s := range shortfalls {
		if s.Kind == kind {
			return s
		}
	}
	t.Fatalf("shortfalls = %+v, want %s among them", shortfalls, kind)
	return app.GuardShortfallView{}
}

// TestStatusVerdictRejectedShortfallCorrelation exercises STATUS-1's
// manager verdict channel end to end at the read-model layer (Astra F3):
// a real hop review submit --verdict reject through SubmitReviewVerdict
// writes the controller notice (body = the reasons artifact path) in the
// same transaction as the review row, and hop status's verdict-rejected
// shortfall must name EXACTLY that review — its id, subject commit and
// reasons path — so a manager comparing a fetched notice's body path
// against the shortfall's reasons path can tell whether that notice IS
// this rejection. The reasons path is scoped by review id
// (".../reviews/<review-id>"), so a different review — hence any
// unrelated notice, which is never shaped like this review's own
// reasons path — can never collide with it (the "no second fix"
// guarantee). Once a fix integrates a new head, the same review becomes
// stale-subject, not rejected, clearing the correlation fields (F3(a)'s
// reordering, proven end to end through the read model this time, not
// just the domain function in isolation).
func TestStatusVerdictRejectedShortfallCorrelation(t *testing.T) {
	rf := newReviewFixture(t)

	// The guard compares commit AND tree object IDs together, and the
	// completion guard's head collapses both to one Integration.MergeCommitOID
	// (no separate tree field, by design) — so a review can only ever
	// match "the current head" when ITS OWN subject commit and tree are
	// equal. newReviewFixture's review task is frozen with DIFFERENT
	// commit and tree (rf.SubjectCommit and the fake git's constant
	// fakeSubjectTree) precisely so other tests never accidentally
	// depend on a match; widen just this task's frozen subject tree to
	// equal its commit, and script this one subject's git tree
	// resolution to match, so a real hop review submit (through
	// SubmitReviewVerdict, exactly as production runs it) accepts a
	// review whose commit and tree are both rf.SubjectCommit.
	task := rf.tc.Store.Tasks[rf.TaskID]
	task.value.SubjectTreeOID = rf.SubjectCommit
	rf.tc.Commands.Results["/usr/bin/git -C /repo rev-parse "+rf.SubjectCommit+"^{tree}"] = app.CommandResult{
		ExitCode: 0, Stdout: []byte(rf.SubjectCommit + "\n"),
	}

	integrationID, err := identity.ParseIntegrationID(rf.tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse integration id: %v", err)
	}
	resultID, err := identity.ParseResultID(rf.tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse result id: %v", err)
	}
	now := rf.tc.Clock.Now()
	integ := run.NewIntegration(integrationID, rf.fr.RunID, rf.TaskID, resultID, rf.SubjectCommit, "premerge-oid", now)
	if integ, err = integ.EnterChecking(rf.SubjectCommit, now); err != nil {
		t.Fatalf("EnterChecking() error = %v", err)
	}
	if integ, err = integ.Integrate(now); err != nil {
		t.Fatalf("Integrate() error = %v", err)
	}
	rf.tc.Store.Integrations[integrationID] = &entityRow[run.Integration]{value: integ, revision: 1}

	result := rf.submit(t, "reject", rf.SubjectCommit, []byte("looks wrong"))
	if result.Outcome != string(app.ReviewAccepted) {
		t.Fatalf("submit reject: result = %+v, want accepted", result)
	}

	view := mustStatusDetail(t, rf.tc, rf.fr.RunID)
	shortfall := shortfallOf(t, view.GuardShortfalls, "verdict-rejected")
	if shortfall.ReviewID != result.ReviewID || shortfall.SubjectCommitOID != rf.SubjectCommit || shortfall.ReasonsPath == "" {
		t.Fatalf("verdict-rejected shortfall = %+v, want ReviewID %s SubjectCommitOID %s", shortfall, result.ReviewID, rf.SubjectCommit)
	}
	if !strings.HasSuffix(shortfall.ReasonsPath, "/reviews/"+shortfall.ReviewID) {
		t.Fatalf("reasons path = %q, want it scoped by this review's own id (no other review's notice can ever share it)", shortfall.ReasonsPath)
	}

	// The manager fetches the controller notice: its body path must equal
	// the shortfall's reasons path exactly — the correlating case, where
	// planning exactly one fix task is correct.
	fetch, err := rf.tc.Controller.FetchMessage(context.Background(), app.FetchMessageRequest{
		RunID: rf.fr.RunID.String(), SessionID: rf.fr.ManagerID.String(), IncarnationID: rf.fr.ManagerIncarnation.String(),
	})
	if err != nil {
		t.Fatalf("FetchMessage() error = %v", err)
	}
	if !fetch.Delivered || fetch.BodyPath != shortfall.ReasonsPath {
		t.Fatalf("controller notice body = %q delivered=%v, want it to equal the shortfall's reasons path %q", fetch.BodyPath, fetch.Delivered, shortfall.ReasonsPath)
	}

	// A fix integrates a new head (a second integration row, settled
	// later — guardShortfallsLocked picks the newest INTEGRATED row by
	// UpdatedAt): the
	// same review's subject is now stale, so the shortfall flips to
	// stale-subject and carries neither a review id nor a reasons path —
	// there is nothing to act on for it any more, in either direction.
	fixIntegrationID, err := identity.ParseIntegrationID(rf.tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse fix integration id: %v", err)
	}
	fixResultID, err := identity.ParseResultID(rf.tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse fix result id: %v", err)
	}
	later := now.Add(time.Minute)
	fixIntegration := mustReintegrateAt(t, fixIntegrationID, rf.fr.RunID, rf.TaskID, fixResultID, "fixed-head-oid", later)
	rf.tc.Store.Integrations[fixIntegrationID] = &entityRow[run.Integration]{value: fixIntegration, revision: 1}

	view = mustStatusDetail(t, rf.tc, rf.fr.RunID)
	if containsShortfall(view.GuardShortfalls, "verdict-rejected") {
		t.Fatalf("GuardShortfalls = %+v, want no verdict-rejected once the head has moved", view.GuardShortfalls)
	}
	stale := shortfallOf(t, view.GuardShortfalls, "verdict-stale-subject")
	if stale.ReviewID != "" || stale.SubjectCommitOID != "" || stale.ReasonsPath != "" {
		t.Fatalf("verdict-stale-subject shortfall = %+v, want no review-correlation fields", stale)
	}
}

// mustReintegrateAt builds a fresh, already-integrated Integration row at
// a new head object id, standing in for "a fix landed a new candidate" —
// a second integration row a real run would create through DriveIntegration,
// simplified here to the read model's own construction helper.
func mustReintegrateAt(t *testing.T, id identity.IntegrationID, runID identity.RunID, taskID identity.TaskID, resultID identity.ResultID, headOID string, now time.Time) run.Integration {
	t.Helper()
	integ := run.NewIntegration(id, runID, taskID, resultID, headOID, "premerge-2", now)
	integ, err := integ.EnterChecking(headOID, now)
	if err != nil {
		t.Fatalf("EnterChecking() error = %v", err)
	}
	if integ, err = integ.Integrate(now); err != nil {
		t.Fatalf("Integrate() error = %v", err)
	}
	return integ
}

func TestStatusMailboxesAndAttention(t *testing.T) {
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	snapshot := tc.Store.Snapshots[fr.RunID]
	snapshot.Workflow.MessageAttention = 60 * time.Second
	tc.Store.Snapshots[fr.RunID] = snapshot
	taskB := seedImplementTask(t, tc, fr.RunID, 1, "B", false, run.TaskReady)
	workerID, workerIncarnation := seedWorkerSession(t, tc, fr, taskB)
	ctx := context.Background()

	send, err := tc.Controller.SendMessage(ctx, app.SendMessageRequest{
		RunID: fr.RunID.String(), SessionID: workerID.String(), IncarnationID: workerIncarnation.String(),
		StateRoot: "/state", To: "manager", Kind: "question", Body: []byte("q?"),
	})
	if err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}

	// Still queued, well under the threshold: no attention yet, but the
	// mailbox already appears with its queued count.
	tc.Clock.Advance(10 * time.Second)
	view := mustStatusDetail(t, tc, fr.RunID)
	mgr := mailboxManager(t, view)
	if mgr.QueuedCount != 1 || mgr.InFlightMessageID != "" {
		t.Fatalf("manager mailbox = %+v, want 1 queued, none in flight", mgr)
	}
	if mgr.Attention || view.NeedsAttention {
		t.Fatalf("attention fired before the threshold: mailbox=%+v view.NeedsAttention=%v", mgr, view.NeedsAttention)
	}

	// Past the threshold, address still live (the manager session is
	// active): attention fires, rolled up onto the view.
	tc.Clock.Advance(55 * time.Second)
	view = mustStatusDetail(t, tc, fr.RunID)
	mgr = mailboxManager(t, view)
	if !mgr.Attention || !view.NeedsAttention {
		t.Fatalf("attention did not fire past the threshold: mailbox=%+v view.NeedsAttention=%v", mgr, view.NeedsAttention)
	}
	if mgr.OldestQueuedAge != 65*time.Second {
		t.Fatalf("OldestQueuedAge = %s, want 65s", mgr.OldestQueuedAge)
	}

	// A dead manager session never triggers attention, however stale.
	mgrRow := tc.Store.Sessions[fr.ManagerID]
	mgrRow.value.State = run.SessionTerminated
	view = mustStatusDetail(t, tc, fr.RunID)
	mgr = mailboxManager(t, view)
	if mgr.AddressLive || mgr.Attention || view.NeedsAttention {
		t.Fatalf("attention fired for a dead session: mailbox=%+v view.NeedsAttention=%v", mgr, view.NeedsAttention)
	}
	mgrRow.value.State = run.SessionActive

	// Delivered but unacknowledged: an in-flight entry with its own age,
	// queued count back to zero.
	if _, err := tc.Controller.FetchMessage(ctx, app.FetchMessageRequest{
		RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
	}); err != nil {
		t.Fatalf("FetchMessage() error = %v", err)
	}
	tc.Clock.Advance(3 * time.Second)
	view = mustStatusDetail(t, tc, fr.RunID)
	mgr = mailboxManager(t, view)
	if mgr.QueuedCount != 0 || mgr.InFlightMessageID != send.MessageID || mgr.InFlightAge != 3*time.Second {
		t.Fatalf("manager mailbox after delivery = %+v, want in-flight %s at 3s", mgr, send.MessageID)
	}

	// Acknowledged: the mailbox drops out of the status surface entirely.
	if _, err := tc.Controller.AckMessage(ctx, app.AckMessageRequest{
		RunID: fr.RunID.String(), MessageID: send.MessageID, SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
	}); err != nil {
		t.Fatalf("AckMessage() error = %v", err)
	}
	view = mustStatusDetail(t, tc, fr.RunID)
	for _, m := range view.Mailboxes {
		if m.Address == "manager" {
			t.Fatalf("manager mailbox still present after ack: %+v", m)
		}
	}
}

// mailboxManager returns view's "manager" mailbox entry — the only address
// these attention-surface tests exercise.
func mailboxManager(t *testing.T, view app.RunDetailView) app.MailboxView { //nolint:gocritic // hugeParam: RunDetailView is a small view value rendered once per assertion in these tests.
	t.Helper()
	for _, m := range view.Mailboxes {
		if m.Address == "manager" {
			return m
		}
	}
	t.Fatalf("no manager mailbox in %+v", view.Mailboxes)
	return app.MailboxView{}
}

func TestStatusPendingQuestions(t *testing.T) {
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	ctx := context.Background()

	send, err := tc.Controller.SendMessage(ctx, app.SendMessageRequest{
		RunID: fr.RunID.String(), SessionID: fr.ManagerID.String(), IncarnationID: fr.ManagerIncarnation.String(),
		StateRoot: "/state", To: "human", Kind: "question", Body: []byte("proceed?"),
	})
	if err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}

	view := mustStatusDetail(t, tc, fr.RunID)
	if len(view.PendingQuestions) != 1 || view.PendingQuestions[0].MessageID != send.MessageID {
		t.Fatalf("PendingQuestions = %+v, want exactly the pending question", view.PendingQuestions)
	}

	if _, err := tc.Controller.Answer(ctx, app.AnswerRequest{
		RunID: fr.RunID.String(), QuestionID: send.MessageID, StateRoot: "/state", Body: []byte("yes"),
	}); err != nil {
		t.Fatalf("Answer() error = %v", err)
	}
	view = mustStatusDetail(t, tc, fr.RunID)
	if len(view.PendingQuestions) != 0 {
		t.Fatalf("PendingQuestions after answer = %+v, want none", view.PendingQuestions)
	}
}

func TestStatusSoloRunLeavesFeatureFieldsEmpty(t *testing.T) {
	tc := newTestController(defaultPolicy())
	now := tc.Clock.Now()

	runID, err := identity.ParseRunID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse run id: %v", err)
	}
	repoID := identity.RepositoryID(tc.IDs.NewID())
	r := run.NewRun(runID, repoID, 1, "briefdigest", now)
	if r, err = r.Launch(now); err != nil {
		t.Fatalf("launch run: %v", err)
	}
	if r, err = r.MarkRunning(now); err != nil {
		t.Fatalf("mark run running: %v", err)
	}
	tc.Store.Runs[runID] = &entityRow[run.Run]{value: r, revision: 1}
	tc.Store.Snapshots[runID] = app.RunSnapshot{StateRoot: "/state"} // zero Workflow: solo

	view := mustStatusDetail(t, tc, runID)
	if len(view.Tasks) != 0 || view.LatestIntegration != nil || len(view.GuardShortfalls) != 0 ||
		len(view.Mailboxes) != 0 || len(view.PendingQuestions) != 0 || view.NeedsAttention {
		t.Fatalf("solo run rendered feature-mode fields: %+v", view)
	}
}
