package app_test

import (
	"context"
	"fmt"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// featureStore wraps fakeStore with the acceptance-side effects the real
// store's accepting transactions commit (docs/plan/phase-3-design.md
// section 5, "Mailbox closure and admission"): an accepted result or
// verdict closes the task's mailbox atomically, and an accepted verdict
// commits the controller's notice to the manager. The mailbox rule itself
// — a queued or delivered-unacknowledged message refuses a submission with
// the retryable transient outcome — is decided by the base fakeStore
// through AcceptResult and AcceptVerdict, after every other eligibility
// check, exactly as the real store decides it. Verdict acceptance
// additionally enforces the section 8 reviewer-session eligibility.
// Receipt order is preserved: a duplicate or conflicting submission
// resolves BEFORE any eligibility check.
type featureStore struct{ *fakeStore }

func (s *featureStore) SubmitResult(ctx context.Context, submission app.ResultSubmission) (app.SubmissionOutcome, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	outcome, err := s.fakeStore.SubmitResult(ctx, submission)
	if err == nil && outcome.Kind == app.SubmissionAccepted {
		s.closeTaskMailbox(submission.TaskID)
	}
	return outcome, err
}

func (s *featureStore) SubmitReview(ctx context.Context, submission app.ReviewSubmission) (app.ReviewOutcome, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	s.mu.Lock()
	_, hasPrior := s.Reviews[submission.AttemptID]
	sessRow, sessOK := s.Sessions[submission.Session]
	taskRow, taskOK := s.Tasks[submission.TaskID]
	s.mu.Unlock()

	// Receipt before eligibility: a prior accepted verdict resolves as
	// duplicate or conflicting whatever the caller's current eligibility.
	// A first acceptance requires THE review attempt's own reviewer — a
	// reviewer-role session of the submission's run bound to exactly the
	// attempt being settled — so a live reviewer of another run, or of a
	// different review attempt in this run, is refused before its binding
	// or launch claim ever reaches the acceptance context (the shared
	// storevectors.ReviewSubmitForeignReviewer contract, matching the real
	// store's reviewerSessionEligible).
	if !hasPrior {
		if !sessOK || sessRow.value.Role != run.RoleReviewer || !taskOK || taskRow.value.Kind != run.TaskKindReview ||
			sessRow.value.RunID != submission.RunID || sessRow.value.AttemptID != submission.AttemptID {
			return app.ReviewOutcome{Kind: app.ReviewStale, Detail: "caller is not the review task's reviewer session"}, nil
		}
	}
	outcome, err := s.fakeStore.SubmitReview(ctx, submission)
	if err == nil && outcome.Kind == app.ReviewAccepted {
		s.closeTaskMailbox(submission.TaskID)
		// The controller info message to the manager commits with the
		// acceptance (section 8); its body is the reasons artifact the
		// manager acts on.
		s.mu.Lock()
		id, idErr := identity.ParseMessageID(fmt.Sprintf("f0000000-0000-4000-8000-%012x", len(s.Messages)+1))
		if idErr == nil {
			seq := nextEnqueueSeq(s.fakeStore, submission.RunID, run.ManagerAddress())
			notice := run.NewInfo(id, submission.RunID, run.ControllerPrincipal(), run.ManagerAddress(), "", submission.ReasonsPath, submission.ReasonsDigest, 0, seq, s.clock.Now())
			s.Messages[notice.ID] = notice
		}
		s.mu.Unlock()
	}
	return outcome, err
}

// closeTaskMailbox closes the task's mailbox in the same logical step as
// the acceptance that authorized it.
func (s *featureStore) closeTaskMailbox(taskID identity.TaskID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.Tasks[taskID]; ok {
		row.value = row.value.CloseMailbox(s.clock.Now())
		row.revision++
	}
}
