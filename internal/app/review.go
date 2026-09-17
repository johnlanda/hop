package app

import (
	"context"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// ReviewOutcomeKind is the section 8 outcome of one review submission,
// mirroring SubmissionOutcomeKind: AcceptVerdict validates in the exact
// same order as AcceptResult (receipt before eligibility) and returns the
// same error family, plus its own mailbox and subject-match refusals.
type ReviewOutcomeKind string

// Review outcome kinds.
const (
	ReviewAccepted    ReviewOutcomeKind = "accepted"
	ReviewDuplicate   ReviewOutcomeKind = "duplicate"
	ReviewConflicting ReviewOutcomeKind = "conflicting"
	ReviewStale       ReviewOutcomeKind = "stale"
	ReviewTransient   ReviewOutcomeKind = "transient"
	ReviewMalformed   ReviewOutcomeKind = "malformed"
)

// ReviewSubmission is the application-validated content of one `hop
// review submit`: identities already parsed and confirmed to agree
// (attempt belongs to the review task, task to the run), the reasons
// artifact already written durably before this call, and SubjectTreeOID
// already resolved by the application against the recorded repository
// from the caller-supplied commit — the domain never touches a
// repository, so this resolution happens here, exactly like
// ComputeResultDigest for results.
type ReviewSubmission struct {
	ID               identity.ReviewID
	RunID            identity.RunID
	TaskID           identity.TaskID
	AttemptID        identity.AttemptID
	Session          identity.SessionID
	IncarnationID    identity.IncarnationID
	SubjectCommitOID string
	SubjectTreeOID   string
	Verdict          run.Verdict
	ReasonsPath      string
	ReasonsDigest    string
}

// ReviewOutcome is the recorded result of one review submission. ReviewID
// is set for Accepted and Duplicate. Transient is set exactly when Kind is
// ReviewTransient (TransientReasonOf the AcceptVerdict error).
type ReviewOutcome struct {
	Kind      ReviewOutcomeKind
	ReviewID  identity.ReviewID
	Detail    string
	Transient TransientReason
}

// ReviewStore holds the section 8 worker-authority review write: one
// internal transaction, no controller lease, generation-NULL evidence,
// exactly as SubmissionStore.SubmitResult. Its acceptance transaction
// (AcceptVerdict, the retirement of the reviewer's session and the guard
// re-evaluation that may follow) is driving-controller behavior owned by
// a later slice; this port only declares the worker-authority write's
// shape.
type ReviewStore interface {
	SubmitReview(ctx context.Context, submission ReviewSubmission) (ReviewOutcome, error)
}
