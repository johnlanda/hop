package app_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// fakeSubjectTree is fakeCommands' default answer for any
// `git rev-parse <X>^{tree}` without a scripted git repo.
const fakeSubjectTree = "tttttttttttttttttttttttttttttttttttttttt"

// reviewFixture is a review task with a running reviewer attempt and a
// bound reviewer session, submitted against through the featureStore
// wrapper (the acceptance-side mailbox/eligibility contract).
type reviewFixture struct {
	tc            *testController
	fr            featureRun
	TaskID        identity.TaskID
	AttemptID     identity.AttemptID
	SessionID     identity.SessionID
	IncarnationID identity.IncarnationID
	SubjectCommit string
}

func newReviewFixture(t *testing.T) *reviewFixture {
	t.Helper()
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	tc.Store.repoByRoot["/repo"] = tc.Store.Runs[fr.RunID].value.RepositoryID
	tc.Controller.Reviews = &featureStore{fakeStore: tc.Store}
	now := tc.Clock.Now()

	taskID, err := identity.ParseTaskID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse task id: %v", err)
	}
	subject := "5ub7ec7c0mm17"
	review := run.NewReviewTask(taskID, fr.RunID, 2, subject, fakeSubjectTree, now)
	review.State = run.TaskActive
	tc.Store.Tasks[taskID] = &entityRow[run.Task]{value: review, revision: 1}

	attemptID, err := identity.ParseAttemptID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse attempt id: %v", err)
	}
	attempt, err := run.NewAttempt(attemptID, taskID, 1, now)
	if err != nil {
		t.Fatalf("NewAttempt() error = %v", err)
	}
	attempt.State = run.AttemptRunning
	tc.Store.Attempts[attemptID] = &entityRow[run.Attempt]{value: attempt, revision: 1}

	sessionID, err := identity.ParseSessionID(tc.IDs.NewID())
	if err != nil {
		t.Fatalf("parse session id: %v", err)
	}
	reviewer, err := run.NewChildSession(sessionID, fr.RunID, attemptID, run.RoleReviewer, tc.Store.Sessions[fr.ManagerID].value, run.HarnessClaude, now)
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
		IncarnationID: incarnationID, RunID: fr.RunID, SessionID: sessionID, AttemptID: attemptID,
		Executable: "/usr/local/bin/claude", PID: 555, State: app.LaunchClaimExeced, ClaimedAt: now,
	}
	return &reviewFixture{tc: tc, fr: fr, TaskID: taskID, AttemptID: attemptID, SessionID: sessionID, IncarnationID: incarnationID, SubjectCommit: subject}
}

func (f *reviewFixture) submit(t *testing.T, verdict, subject string, reasons []byte) app.SubmitReviewResult {
	t.Helper()
	result, err := f.tc.Controller.SubmitReviewVerdict(context.Background(), app.SubmitReviewRequest{
		RunID: f.fr.RunID.String(), TaskID: f.TaskID.String(), AttemptID: f.AttemptID.String(),
		SessionID: f.SessionID.String(), IncarnationID: f.IncarnationID.String(),
		Verdict: verdict, SubjectCommitOID: subject, ReasonsBody: reasons,
	})
	if err != nil {
		t.Fatalf("SubmitReviewVerdict() error = %v", err)
	}
	return result
}

func TestSubmitReviewVerdict(t *testing.T) {
	t.Run("an accepted approve completes the review attempt and task atomically", func(t *testing.T) {
		f := newReviewFixture(t)
		result := f.submit(t, "approve", f.SubjectCommit, []byte("looks correct"))
		if result.Outcome != string(app.ReviewAccepted) {
			t.Fatalf("outcome = %+v, want accepted", result)
		}
		review, ok := f.tc.Store.Reviews[f.AttemptID]
		if !ok {
			t.Fatalf("no review row recorded")
		}
		if review.SubjectCommitOID != f.SubjectCommit || review.SubjectTreeOID != fakeSubjectTree {
			t.Fatalf("review subject = (%s, %s), want both object IDs the guard compares", review.SubjectCommitOID, review.SubjectTreeOID)
		}
		if got := f.tc.Store.Attempts[f.AttemptID].value.State; got != run.AttemptCompleted {
			t.Fatalf("attempt state = %s, want completed (CompleteReview, never checking)", got)
		}
		if got := f.tc.Store.Tasks[f.TaskID].value.State; got != run.TaskCompleted {
			t.Fatalf("task state = %s, want completed", got)
		}
		if !f.tc.Store.Tasks[f.TaskID].value.MailboxClosed {
			t.Fatalf("review task mailbox open after the accepted verdict; acceptance closes it atomically")
		}
		// The reasons artifact was written file-first, and the acceptance
		// committed the controller notice to the manager referencing it.
		reasonsPath := "/state/runs/" + f.fr.RunID.String() + "/reviews/" + result.ReviewID
		if _, ok := f.tc.Artifacts.files[reasonsPath]; !ok {
			t.Fatalf("reasons artifact %s was never written", reasonsPath)
		}
		noticeFound := false
		for _, m := range f.tc.Store.Messages {
			if m.Sender.Kind == run.PrincipalController && m.Recipient.Kind == run.AddressManager && m.BodyPath == reasonsPath {
				noticeFound = true
			}
		}
		if !noticeFound {
			t.Fatalf("no controller notice referencing the reasons artifact")
		}
	})

	t.Run("a reject verdict also completes the review task; the verdict's content gates the run", func(t *testing.T) {
		f := newReviewFixture(t)
		result := f.submit(t, "reject", f.SubjectCommit, []byte("broken"))
		if result.Outcome != string(app.ReviewAccepted) {
			t.Fatalf("outcome = %+v, want accepted", result)
		}
		if got := f.tc.Store.Tasks[f.TaskID].value.State; got != run.TaskCompleted {
			t.Fatalf("task state = %s, want completed — approve and reject both complete the task", got)
		}
		if got := f.tc.Store.Reviews[f.AttemptID].Verdict; got != run.VerdictReject {
			t.Fatalf("verdict = %s, want reject", got)
		}
	})

	t.Run("an identical retry is duplicate; a different verdict is conflicting and never disturbs the accepted row", func(t *testing.T) {
		f := newReviewFixture(t)
		first := f.submit(t, "approve", f.SubjectCommit, []byte("ok"))
		if first.Outcome != string(app.ReviewAccepted) {
			t.Fatalf("first = %+v, want accepted", first)
		}
		dup := f.submit(t, "approve", f.SubjectCommit, []byte("ok"))
		if dup.Outcome != string(app.ReviewDuplicate) {
			t.Fatalf("identical retry = %+v, want duplicate", dup)
		}
		conflict := f.submit(t, "reject", f.SubjectCommit, []byte("changed my mind"))
		if conflict.Outcome != string(app.ReviewConflicting) {
			t.Fatalf("conflicting resubmission = %+v, want conflicting", conflict)
		}
		if got := f.tc.Store.Reviews[f.AttemptID].Verdict; got != run.VerdictApprove {
			t.Fatalf("accepted verdict = %s, want the original approve undisturbed", got)
		}
	})

	t.Run("a wrong subject is refused: the reviewer reviewed the wrong candidate", func(t *testing.T) {
		f := newReviewFixture(t)
		result := f.submit(t, "approve", "wr0ng5ubjec7", []byte("ok"))
		if result.Outcome == string(app.ReviewAccepted) || result.Outcome == string(app.ReviewDuplicate) {
			t.Fatalf("outcome = %+v, want a refusal on the frozen-subject mismatch", result)
		}
		if _, ok := f.tc.Store.Reviews[f.AttemptID]; ok {
			t.Fatalf("a verdict row was recorded despite the subject mismatch")
		}
	})

	t.Run("a non-reviewer session is refused", func(t *testing.T) {
		f := newReviewFixture(t)
		// Rebind the submission to the manager session (a non-reviewer).
		result, err := f.tc.Controller.SubmitReviewVerdict(context.Background(), app.SubmitReviewRequest{
			RunID: f.fr.RunID.String(), TaskID: f.TaskID.String(), AttemptID: f.AttemptID.String(),
			SessionID: f.fr.ManagerID.String(), IncarnationID: f.fr.ManagerIncarnation.String(),
			Verdict: "approve", SubjectCommitOID: f.SubjectCommit, ReasonsBody: []byte("ok"),
		})
		if err != nil {
			t.Fatalf("SubmitReviewVerdict() error = %v", err)
		}
		if result.Outcome == string(app.ReviewAccepted) {
			t.Fatalf("a non-reviewer session's verdict was accepted")
		}
		if _, ok := f.tc.Store.Reviews[f.AttemptID]; ok {
			t.Fatalf("a verdict row was recorded for a non-reviewer caller")
		}
	})

	t.Run("an unclear mailbox refuses the verdict transient until drained", func(t *testing.T) {
		f := newReviewFixture(t)
		msgID, err := identity.ParseMessageID(f.tc.IDs.NewID())
		if err != nil {
			t.Fatalf("parse message id: %v", err)
		}
		pending := run.NewInfo(msgID, f.fr.RunID, run.ControllerPrincipal(), run.TaskAddress(f.TaskID), "", "/state/m", "d", 1, 1, f.tc.Clock.Now())
		f.tc.Store.Messages[msgID] = pending

		result := f.submit(t, "approve", f.SubjectCommit, []byte("ok"))
		if result.Outcome != string(app.ReviewTransient) {
			t.Fatalf("outcome = %+v, want the transient undelivered-messages refusal", result)
		}
		// Drain: the reviewer fetches and acks, then resubmits.
		delivery, ok, err := f.tc.Store.FetchNextMessage(context.Background(), app.MessageFetch{
			RunID: f.fr.RunID, SessionID: f.SessionID, IncarnationID: f.IncarnationID, Address: run.TaskAddress(f.TaskID),
		})
		if err != nil || !ok {
			t.Fatalf("FetchNextMessage() = %v ok=%v", err, ok)
		}
		if ackOutcome, err := f.tc.Store.AckMessage(context.Background(), app.MessageAck{
			RunID: f.fr.RunID, MessageID: delivery.Message.ID, SessionID: f.SessionID, IncarnationID: f.IncarnationID,
		}); err != nil || ackOutcome.Kind != app.AckAccepted {
			t.Fatalf("AckMessage() = %+v err=%v", ackOutcome, err)
		}
		result = f.submit(t, "approve", f.SubjectCommit, []byte("ok"))
		if result.Outcome != string(app.ReviewAccepted) {
			t.Fatalf("resubmission after drain = %+v, want accepted", result)
		}
	})

	t.Run("oversized reasons are malformed before any side effect", func(t *testing.T) {
		f := newReviewFixture(t)
		artifactsBefore := len(f.tc.Artifacts.files)
		result := f.submit(t, "approve", f.SubjectCommit, bytes.Repeat([]byte("x"), app.ReviewReasonsLimit+1))
		if result.Outcome != string(app.ReviewMalformed) {
			t.Fatalf("outcome = %+v, want malformed", result)
		}
		if len(f.tc.Artifacts.files) != artifactsBefore {
			t.Fatalf("a reasons artifact was written for a malformed submission")
		}
	})
}
