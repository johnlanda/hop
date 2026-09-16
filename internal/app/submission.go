package app

import (
	"context"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// LaunchClaimState is a launch claim's own small lifecycle.
type LaunchClaimState string

// Launch claim states.
const (
	LaunchClaimExecPending LaunchClaimState = "exec_pending"
	LaunchClaimExeced      LaunchClaimState = "execed"
	LaunchClaimExecFailed  LaunchClaimState = "exec_failed"
)

// LaunchClaim is the durable record `hop launch` writes before it execs:
// the launcher's own pid and the harness executable it is about to become,
// so a later inspection can corroborate — or fail to corroborate — that the
// exec happened as claimed.
type LaunchClaim struct {
	IncarnationID identity.IncarnationID
	RunID         identity.RunID
	// SessionID is required (docs/plan/phase-3-design.md section 3/4,
	// B2): every claim is keyed to the session it launches, not only to
	// an attempt. AttemptID is optional — empty for an attempt-less
	// session (the manager); the pre-binding pending-launch-intent
	// fallback that authorizes a claim before any binding exists is
	// keyed to the CLAIMED SESSION's newest pending launch operation,
	// never the run's, so two concurrently pending launches validate
	// independently.
	SessionID          identity.SessionID
	AttemptID          identity.AttemptID
	Executable         string
	ArgvDigest         string
	PID                int
	State              LaunchClaimState
	Error              string
	ClaimedAt          time.Time
	SettledAt          time.Time
	SettlementEvidence string
	// SeedEvidence records the launch's workspace-trust pre-seeding outcome
	// ("workspace trust seeded for <worktree path> (verified; best-effort
	// against external profile writers)", or "workspace trust not seeded:
	// <reason>" — the seed write is verified by a post-publish re-read but
	// remains best-effort against a running harness rewriting the same
	// profile). It is evidence only — no decision ever reads
	// it — and never carries an environment value or profile path.
	SeedEvidence string
}

// LaunchClaimSettlement is the controller's corroborated settlement of one
// launch claim (docs/plan/phase-2-design.md section 6): the target state
// the corroboration predicate decided and the evidence that established it.
type LaunchClaimSettlement struct {
	State      LaunchClaimState // LaunchClaimExeced or LaunchClaimExecFailed
	PaneID     string
	PID        int
	Executable string
	ArgvMarker string
	Reason     string // populated for LaunchClaimExecFailed
	At         time.Time
}

// LaunchClaimRepository reads launch claims and records the controller's
// settlement of one to execed or exec_failed. ClaimLaunch and
// SettleLaunchFailure on SubmissionStore stay the launcher's own pre-exec
// write and error path; this repository is the controller's counterpart,
// used only under the lease once corroboration (or a settled exec_failed
// claim's consequences) is being applied.
type LaunchClaimRepository interface {
	Get(ctx context.Context, incarnation identity.IncarnationID) (LaunchClaim, bool, error)
	// Pending lists the run's still-exec_pending claims.
	Pending(ctx context.Context, run identity.RunID) ([]LaunchClaim, error)
	// Settle moves a claim to settlement.State. execed is legal only from
	// exec_pending; exec_failed is legal only from exec_pending.
	// Resettling to the same state is idempotent; any other transition is
	// an error.
	Settle(ctx context.Context, incarnation identity.IncarnationID, settlement LaunchClaimSettlement) error
}

// SubmissionOutcomeKind is the section 7 outcome of one result submission.
type SubmissionOutcomeKind string

// Submission outcome kinds.
const (
	SubmissionAccepted    SubmissionOutcomeKind = "accepted"
	SubmissionDuplicate   SubmissionOutcomeKind = "duplicate"
	SubmissionStale       SubmissionOutcomeKind = "stale"
	SubmissionConflicting SubmissionOutcomeKind = "conflicting"
	SubmissionTransient   SubmissionOutcomeKind = "transient"
	SubmissionMalformed   SubmissionOutcomeKind = "malformed"
)

// ResultSubmission is the application-validated content of one result
// submission handed to SubmissionStore.SubmitResult: identities already
// parsed and confirmed to agree (attempt belongs to task, task to run), and
// Digest already computed by ComputeResultDigest. SubmissionStore never
// hashes.
type ResultSubmission struct {
	ID            identity.ResultID
	RunID         identity.RunID
	TaskID        identity.TaskID
	AttemptID     identity.AttemptID
	IncarnationID identity.IncarnationID
	CommitOID     string
	Summary       string
	Digest        string
}

// SubmissionOutcome is the recorded result of one submission attempt.
// ResultID is set for Accepted and Duplicate.
type SubmissionOutcome struct {
	Kind     SubmissionOutcomeKind
	ResultID identity.ResultID
	Detail   string
}

// ClaimedSubmissionFieldLimit bounds every ClaimedSubmission field the
// receipt stores: a malformed claim is recorded as evidence, never as an
// unbounded copy of arbitrary worker input.
const ClaimedSubmissionFieldLimit = 4096

// ClaimedSubmission is the as-received values of one submission that failed
// application-side parsing or bounds checks (section 7 step 1) before a
// typed ResultSubmission could be constructed: recorded with no foreign
// keys required, so a malformed retry still leaves evidence. Every field is
// truncated to ClaimedSubmissionFieldLimit bytes before recording; Detail
// names the failed check only and never echoes a secret.
type ClaimedSubmission struct {
	RunID, TaskID, AttemptID, IncarnationID string
	CommitOID                               string
	Summary                                 string
	Detail                                  string
}

// SubmissionStore holds worker-side and third-party writes: transactions
// that never carry a controller generation and must survive controller
// takeover. Each method is one internal transaction with its own contract
// (docs/plan/phase-2-design.md section 4, "Transaction authorities", and
// section 7 for SubmitResult's validation order).
type SubmissionStore interface {
	// ClaimLaunch is written by hop launch BEFORE exec: run, attempt,
	// incarnation, the expected executable's resolved absolute path, argv
	// digest, own pid, the workspace-trust seed evidence and state
	// exec_pending. It fails — and the caller must
	// not exec — when the run is stopping or stopped, the incarnation is
	// not current, or a claim for this incarnation already exists with a
	// different pid, a settled state, or a different executable or argv
	// digest. A rewrite by the same pid with the same invocation identity
	// is idempotent except that it refreshes the row's seed evidence to
	// the retry's freshly applied outcome (healing NULL rows written
	// before the seed-evidence migration), so the persisted evidence
	// always matches the plan the launcher execs with; settled history is
	// never rewritten.
	ClaimLaunch(ctx context.Context, claim LaunchClaim) error
	// SettleLaunchFailure records exec_failed on the launcher's error path.
	SettleLaunchFailure(ctx context.Context, incarnation identity.IncarnationID, reason string) error
	// ClaimCheckExec is written by hop check-exec BEFORE exec: operation id
	// and own pid (its process-group id). It fails when the operation is
	// not a pending exec-claimable operation of the current generation —
	// exactly the kinds check.run and integration.merge
	// (docs/plan/phase-3-design.md section 3): the scratch merge is
	// spawned through the same boundary so it carries the same durable
	// pre-exec group claim, while publish, reset and fence are executed
	// directly by the controller and are never claimable.
	ClaimCheckExec(ctx context.Context, op identity.OperationID, pid int) error
	// SubmitResult applies the section 7 validation order atomically,
	// including the attempt and task transitions on acceptance. submission
	// is already parsed and bounds-checked (step 1); SubmitResult performs
	// existence and agreement (step 2 — attempt belongs to task, task to
	// run) internally and returns SubmissionMalformed for disagreement.
	SubmitResult(ctx context.Context, submission ResultSubmission) (SubmissionOutcome, error)
	// RecordMalformed records a submission that failed step 1 parsing or
	// bounds checks before any typed identity could be constructed, with
	// the claimed values truncated to ClaimedSubmissionFieldLimit bytes.
	RecordMalformed(ctx context.Context, claimed ClaimedSubmission) (SubmissionOutcome, error)
	// RequestStop sets the run's monotonic stop request without a lease.
	RequestStop(ctx context.Context, run identity.RunID) error
}
