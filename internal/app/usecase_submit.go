package app

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// maxSubmissionSummaryBytes is section 7 step 1's summary bound.
const maxSubmissionSummaryBytes = 4096

// SubmitResultRequest is hop result submit's input, exactly as parsed from
// CLI flags and the worker's launch-provided environment (HOP_RUN_ID,
// HOP_TASK_ID, HOP_ATTEMPT_ID, HOP_INCARNATION_ID).
type SubmitResultRequest struct {
	RunID         string
	TaskID        string
	AttemptID     string
	IncarnationID string
	CommitOID     string
	Summary       string
}

// SubmitResultResult is what hop result submit reports: Kind is one of the
// SubmissionOutcomeKind values as a string, for the caller's exit-code and
// message decision. Detail carries the required first-line transient
// message verbatim when Kind is "transient".
type SubmitResultResult struct {
	Kind     string
	ResultID string
	Detail   string
}

// SubmitResult performs section 7 step 1 (parse and bound the inputs) here,
// computes the canonical digest, and delegates steps 2-5 (existence,
// duplicate/conflict resolution, eligibility, atomic acceptance) to
// SubmissionStore.SubmitResult.
func (c *Controller) SubmitResult(ctx context.Context, req SubmitResultRequest) (SubmitResultResult, error) { //nolint:gocritic // hugeParam: SubmitResultRequest is the driving DTO for hop result submit, called once per worker submission.
	claimed := ClaimedSubmission{
		RunID: req.RunID, TaskID: req.TaskID, AttemptID: req.AttemptID,
		IncarnationID: req.IncarnationID, CommitOID: req.CommitOID, Summary: req.Summary,
	}

	runID, err := identity.ParseRunID(req.RunID)
	if err != nil {
		return c.recordMalformed(ctx, claimed, "run id: "+err.Error())
	}
	taskID, err := identity.ParseTaskID(req.TaskID)
	if err != nil {
		return c.recordMalformed(ctx, claimed, "task id: "+err.Error())
	}
	attemptID, err := identity.ParseAttemptID(req.AttemptID)
	if err != nil {
		return c.recordMalformed(ctx, claimed, "attempt id: "+err.Error())
	}
	incarnationID, err := identity.ParseIncarnationID(req.IncarnationID)
	if err != nil {
		return c.recordMalformed(ctx, claimed, "incarnation id: "+err.Error())
	}
	if !isCommitObjectID(req.CommitOID) {
		return c.recordMalformed(ctx, claimed, "commit must be a full 40-hex lowercase object id")
	}
	if len(req.Summary) > maxSubmissionSummaryBytes {
		return c.recordMalformed(ctx, claimed, "summary exceeds 4 KiB")
	}
	if !utf8.ValidString(req.Summary) {
		return c.recordMalformed(ctx, claimed, "summary is not valid UTF-8")
	}

	resultID, err := identity.ParseResultID(c.IDs.NewID())
	if err != nil {
		return SubmitResultResult{}, fmt.Errorf("app: generate result id: %w", err)
	}
	digest := ComputeResultDigest(runID, taskID, attemptID, req.CommitOID, req.Summary)

	outcome, err := c.Submissions.SubmitResult(ctx, ResultSubmission{
		ID: resultID, RunID: runID, TaskID: taskID, AttemptID: attemptID,
		IncarnationID: incarnationID, CommitOID: req.CommitOID, Summary: req.Summary, Digest: digest,
	})
	if err != nil {
		return SubmitResultResult{}, fmt.Errorf("app: submit result: %w", err)
	}
	return toSubmitResultResult(outcome), nil
}

// recordMalformed reports one section 7 step 1 failure through
// SubmissionStore.RecordMalformed, so a malformed submission still leaves
// evidence even though its claimed values never became typed identities.
func (c *Controller) recordMalformed(ctx context.Context, claimed ClaimedSubmission, detail string) (SubmitResultResult, error) { //nolint:gocritic // hugeParam: ClaimedSubmission is a per-call DTO, called at most once per malformed submission.
	claimed.Detail = detail
	outcome, err := c.Submissions.RecordMalformed(ctx, claimed)
	if err != nil {
		return SubmitResultResult{}, fmt.Errorf("app: record malformed submission: %w", err)
	}
	return toSubmitResultResult(outcome), nil
}

func toSubmitResultResult(outcome SubmissionOutcome) SubmitResultResult {
	return SubmitResultResult{Kind: string(outcome.Kind), ResultID: outcome.ResultID.String(), Detail: outcome.Detail}
}

// isCommitObjectID reports whether s is a full 40-hex lowercase commit
// object id (SHA-256-object-format repositories are rejected earlier, at
// repository validation in hop run, not here).
func isCommitObjectID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
