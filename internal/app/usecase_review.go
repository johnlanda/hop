package app

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// ReviewReasonsLimit bounds a verdict's reasons body (section 8:
// "reasons ≤ 64 KiB").
const ReviewReasonsLimit = 64 * 1024

// SubmitReviewRequest is the string-facing input behind `hop review
// submit`: composition passes raw strings (the reviewer pane's HOP_* env
// and flags); typed identities are parsed here.
type SubmitReviewRequest struct {
	RunID         string
	TaskID        string
	AttemptID     string
	SessionID     string
	IncarnationID string
	// Verdict is the CLI token: "approve" or "reject".
	Verdict string
	// SubjectCommitOID is the reviewed candidate's commit — the CLI takes
	// the commit only; the tree object ID is resolved here, against the
	// recorded repository, before the accepting transaction, so the
	// stored verdict always carries both IDs the guard compares.
	SubjectCommitOID string
	// ReasonsBody is the verdict's reasons content, written durably
	// (file-first) before the accepting transaction.
	ReasonsBody []byte
}

// SubmitReviewResult is SubmitReviewVerdict's outcome. TransientReason is
// one of the TransientReason values as a string, set exactly when Outcome
// is "transient": it alone selects the reviewer's retry line
// (GrammarSubmissionTransientLine); Detail is diagnostic evidence.
type SubmitReviewResult struct {
	Outcome         string
	ReviewID        string
	Detail          string
	TransientReason string
}

// SubmitReviewVerdict is the driving use case behind `hop review submit`
// (section 8): parse and bound the inputs, resolve the subject tree
// object ID against the recorded repository, write the reasons artifact
// durably (file-first), then delegate validation and acceptance to
// ReviewStore.SubmitReview — one worker-authority transaction that
// validates in the AcceptResult order (receipt before eligibility) and,
// on acceptance, completes the review attempt and task atomically.
func (c *Controller) SubmitReviewVerdict(ctx context.Context, req SubmitReviewRequest) (SubmitReviewResult, error) { //nolint:gocritic // hugeParam: SubmitReviewRequest is the driving DTO for hop review submit, called once per submission.
	if c.Reviews == nil {
		return SubmitReviewResult{}, fmt.Errorf("%w: review submission", ErrFeatureModeUnsupported)
	}
	runID, err := identity.ParseRunID(req.RunID)
	if err != nil {
		return SubmitReviewResult{Outcome: string(ReviewMalformed), Detail: "run id does not parse"}, nil
	}
	taskID, err := identity.ParseTaskID(req.TaskID)
	if err != nil {
		return SubmitReviewResult{Outcome: string(ReviewMalformed), Detail: "task id does not parse"}, nil
	}
	attemptID, err := identity.ParseAttemptID(req.AttemptID)
	if err != nil {
		return SubmitReviewResult{Outcome: string(ReviewMalformed), Detail: "attempt id does not parse"}, nil
	}
	sessionID, err := identity.ParseSessionID(req.SessionID)
	if err != nil {
		return SubmitReviewResult{Outcome: string(ReviewMalformed), Detail: "session id does not parse"}, nil
	}
	incarnationID, err := identity.ParseIncarnationID(req.IncarnationID)
	if err != nil {
		return SubmitReviewResult{Outcome: string(ReviewMalformed), Detail: "incarnation id does not parse"}, nil
	}
	var verdict run.Verdict
	switch req.Verdict {
	case string(run.VerdictApprove):
		verdict = run.VerdictApprove
	case string(run.VerdictReject):
		verdict = run.VerdictReject
	default:
		return SubmitReviewResult{Outcome: string(ReviewMalformed), Detail: "verdict must be approve or reject"}, nil
	}
	if req.SubjectCommitOID == "" {
		return SubmitReviewResult{Outcome: string(ReviewMalformed), Detail: "subject commit is required"}, nil
	}
	if len(req.ReasonsBody) > ReviewReasonsLimit {
		return SubmitReviewResult{Outcome: string(ReviewMalformed), Detail: "reasons exceed the 64 KiB bound"}, nil
	}

	frozen, err := c.Read.LoadFrozenRun(ctx, runID)
	if err != nil {
		return SubmitReviewResult{}, fmt.Errorf("app: load frozen run: %w", err)
	}
	subjectTree, err := c.runGit(ctx, frozen.RepositoryRoot, "rev-parse", req.SubjectCommitOID+"^{tree}")
	if err != nil {
		return SubmitReviewResult{Outcome: string(ReviewMalformed), Detail: "subject commit does not resolve in the recorded repository"}, nil
	}

	reviewID, err := identity.ParseReviewID(c.IDs.NewID())
	if err != nil {
		return SubmitReviewResult{}, fmt.Errorf("app: generate review id: %w", err)
	}
	reasonsPath := filepath.Join(frozen.Snapshot.StateRoot, "runs", runID.String(), "reviews", reviewID.String())
	if writeErr := c.Artifacts.WriteArtifact(ctx, reasonsPath, req.ReasonsBody); writeErr != nil {
		return SubmitReviewResult{}, fmt.Errorf("app: write reasons artifact: %w", writeErr)
	}

	outcome, err := c.Reviews.SubmitReview(ctx, ReviewSubmission{
		ID: reviewID, RunID: runID, TaskID: taskID, AttemptID: attemptID,
		Session: sessionID, IncarnationID: incarnationID,
		SubjectCommitOID: req.SubjectCommitOID, SubjectTreeOID: subjectTree,
		Verdict: verdict, ReasonsPath: reasonsPath, ReasonsDigest: sha256Hex(req.ReasonsBody),
	})
	if err != nil {
		return SubmitReviewResult{}, fmt.Errorf("app: submit review: %w", err)
	}
	if err := checkTransientReason(outcome.Kind == ReviewTransient, outcome.Transient); err != nil {
		return SubmitReviewResult{}, fmt.Errorf("app: submit review: %w", err)
	}
	return SubmitReviewResult{
		Outcome: string(outcome.Kind), ReviewID: outcome.ReviewID.String(), Detail: outcome.Detail,
		TransientReason: string(outcome.Transient),
	}, nil
}
