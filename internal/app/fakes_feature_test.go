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
// commits the controller's notice to the manager. Every decision — the
// order, the reviewer-session eligibility and the mailbox rule, which
// refuses a submission with the retryable transient outcome after every
// other eligibility check — is the base fakeStore's, exactly as the real
// store decides it; the wrapper only adds the side effects, inside the
// same critical section as the acceptance.
type featureStore struct{ *fakeStore }

func (s *featureStore) SubmitResult(_ context.Context, submission app.ResultSubmission) (app.SubmissionOutcome, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.submitResultLocked(&submission, func() {
		s.closeTaskMailboxLocked(submission.TaskID)
	}), nil
}

func (s *featureStore) SubmitReview(_ context.Context, submission app.ReviewSubmission) (app.ReviewOutcome, error) { //nolint:gocritic // hugeParam: implements the port's interface signature exactly.
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.submitReviewLocked(&submission, func() {
		s.closeTaskMailboxLocked(submission.TaskID)
		// The controller info message to the manager commits with the
		// acceptance (section 8); its body is the reasons artifact the
		// manager acts on.
		id, idErr := identity.ParseMessageID(fmt.Sprintf("f0000000-0000-4000-8000-%012x", len(s.Messages)+1))
		if idErr == nil {
			seq := nextEnqueueSeq(s.fakeStore, submission.RunID, run.ManagerAddress())
			notice := run.NewInfo(id, submission.RunID, run.ControllerPrincipal(), run.ManagerAddress(), "", submission.ReasonsPath, submission.ReasonsDigest, 0, seq, s.clock.Now())
			s.Messages[notice.ID] = notice
		}
	}), nil
}

// closeTaskMailboxLocked closes the task's mailbox in the same critical
// section as the acceptance that authorized it. Callers hold s.mu.
func (s *featureStore) closeTaskMailboxLocked(taskID identity.TaskID) {
	if row, ok := s.Tasks[taskID]; ok {
		row.value = row.value.CloseMailbox(s.clock.Now())
		row.revision++
	}
}
