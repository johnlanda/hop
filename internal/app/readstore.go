package app

import (
	"context"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// RunStatus is one line of `hop status` without `-run`: a run's identity,
// position and current condition.
type RunStatus struct {
	RunID    identity.RunID
	Sequence int
	State    run.RunState
	// StopRequested mirrors the run's monotonic stop flag: a held stop
	// request routes resume back to stop handling before any adoption or
	// new dispatch.
	StopRequested bool
	Reconciling   bool // at least one operation for the run is OperationReconciling
	UpdatedAt     time.Time
}

// TaskSummary is one row of RunDetail's feature-mode task table (section
// 3's ReadStore extension): a task's identity, kind, state, dependency
// edges and attempt count, for `hop status -run`. WorktreePath is the
// task's current attempt's worktree, "" when none exists yet — also ""
// until a later slice's migration links a Worktree row to the attempt
// that created it (Worktree is still the Phase 2 one-row-per-run shape
// today; slice 3's migration adds attempt_id and base_commit).
type TaskSummary struct {
	TaskID       identity.TaskID
	Seq          int
	Kind         run.TaskKind
	State        run.TaskState
	DependsOn    []identity.TaskID
	AttemptCount int
	WorktreePath string
}

// IntegrationSummary is one integration row `hop status` renders as "the
// integration head" (section 3's ReadStore extension): which task and
// result it merges, and its recorded object IDs and state. RunDetail
// carries the run's most recently CREATED integration, if any — reading
// exactly what is recorded; resolving what the live git ref currently
// points to is 2b's integration-operations concern, not this read model's.
type IntegrationSummary struct {
	ID              identity.IntegrationID
	TaskID          identity.TaskID
	SourceCommitOID string
	PremergeHeadOID string
	MergeCommitOID  string
	State           run.IntegrationState
}

// InFlightMessage is one delivered-but-unacknowledged message's identity
// and age (time since its most recent delivery).
type InFlightMessage struct {
	MessageID identity.MessageID
	Age       time.Duration
}

// MailboxStatus is one recipient address's queue condition: section 7's
// "idle with pending deliveries" status surface. RunDetail carries one
// entry per address with a non-empty queue or an unacknowledged in-flight
// message — an idle, caught-up address has no entry at all. InFlight is
// nil when nothing is currently delivered-unacknowledged; QueuedCount and
// OldestQueuedAge describe messages never yet delivered (OldestQueuedAge
// is the zero value when QueuedCount is 0).
type MailboxStatus struct {
	Address         run.Address
	InFlight        *InFlightMessage
	QueuedCount     int
	OldestQueuedAge time.Duration
	// AddressLive reports whether the address's own session is currently
	// non-terminal: the run's current manager session for AddressManager,
	// the address's task's current attempt's current session for
	// AddressTask, and always true for AddressHuman (which has no session
	// to go stale) — section 7's "address's session is live" qualifier on
	// the attention condition.
	AddressLive bool
	// Attention is section 7's computed condition: AddressLive and the
	// single oldest pending item at this address (the in-flight message's
	// age, or the oldest queued age when nothing is in flight) exceeds the
	// run's configured `[messages] attention_after` threshold. Computed
	// once here, against the frozen policy the read side already holds,
	// and rendered verbatim by every consumer.
	Attention bool
}

// PendingQuestion is one unanswered human-addressed question: `hop
// status`'s pending-human-questions line, naming the body path a human
// needs to read and the message id `hop answer` takes.
type PendingQuestion struct {
	MessageID identity.MessageID
	BodyPath  string
	Age       time.Duration
}

// RunDetail is the full detail block `hop status -run` renders, and the
// identities the controller use cases (stop, resume) need to load and
// mutate the run's task, attempt and session directly. Phase 2 gives every
// run exactly one task and one attempt, so TaskID and AttemptID are always
// well-defined once the run exists; SessionID is the attempt's current
// non-terminated session and is the zero value when none exists (briefly
// possible mid-reconciliation, between a lost session and its replacement).
type RunDetail struct {
	RunStatus
	// Mode is the run's frozen workflow mode ("feature"), or "" for a
	// solo run — mirrors WorkflowSnapshot.Mode so cmd/hop can dispatch
	// between the solo and feature-mode use-case pairs (Resume vs
	// ResumeFeature, DriveStop vs DriveFeatureStop, the loop's two
	// scheduling shapes) without holding a WorkflowSnapshot itself.
	Mode         string
	TaskID       identity.TaskID
	AttemptID    identity.AttemptID
	SessionID    identity.SessionID
	TaskState    run.TaskState
	AttemptState run.AttemptState
	WorktreePath string
	// StateRoot is the run snapshot's frozen absolute state root: where the
	// run's artifact directories live, needed by evidence capture.
	StateRoot         string
	Binding           *run.RuntimeBinding
	Claim             *LaunchClaim
	PendingOperations []Operation
	LastSubmission    *SubmissionOutcome
	Artifacts         []run.Artifact
	// LastCheck names the newest check execution, if any: its identity,
	// journal state and outcome, and the retained evidence artifacts — so
	// an unknown outcome stays actionable through hop status.
	LastCheck *CheckExecutionSummary

	// Tasks is the feature-mode task table (section 3's ReadStore
	// extension): every task recorded for the run, in no particular order.
	// Nil for a solo run — TaskID/TaskState above stay the Phase 2 single-
	// task fields for solo's own rendering.
	Tasks []TaskSummary
	// LatestIntegration is the run's most recently created integration
	// row, if any; nil for a solo run or a feature run with no integration
	// attempted yet.
	LatestIntegration *IntegrationSummary
	// GuardShortfalls is EvaluateReadiness's missing list, rendered
	// verbatim (section 3): always empty for a solo run, since only a
	// feature run ever assembles a GuardContext.
	GuardShortfalls []run.GuardShortfall
	// Mailboxes is section 7's per-address queue-depth/in-flight-age
	// status surface: one entry per address with a non-empty queue or an
	// unacknowledged in-flight message.
	Mailboxes []MailboxStatus
	// PendingQuestions is every unanswered human-addressed question,
	// oldest first.
	PendingQuestions []PendingQuestion
}

// CheckExecutionContext is what the check exec boundary (`hop check-exec`)
// needs, addressed by the check execution's operation ID: the frozen env
// policy, the absolute state root, the candidate checkout path and the
// frozen check argv.
type CheckExecutionContext struct {
	EnvPolicy    EnvPolicy
	StateRoot    string
	CheckoutPath string
	CheckArgv    []string
}

// CheckExecutionSummary is the read model of one check execution: what
// hop status names so a human can act on it — especially an unrepeatable
// unknown outcome, which never completes the run automatically.
type CheckExecutionSummary struct {
	OperationID identity.OperationID
	State       OperationState
	Unknown     bool
	Detail      string
	// EvidencePaths are the retained artifact paths tied to the checked
	// result (stdout, stderr) plus any pane-snapshot evidence.
	EvidencePaths []string
}

// FrozenRun is the lease-free read model of a run's frozen execution
// inputs: the immutable snapshot InitializeRun committed and the
// repository identity it belongs to. The check use case loads its argv,
// timeout, repeatability and state root from here — never from caller
// arguments — so no caller-supplied policy can drift from the freeze.
type FrozenRun struct {
	Snapshot       RunSnapshot
	RepositoryRoot string
	// Brief is the run's frozen brief text, needed to recreate the
	// assignment artifact byte-identically when its file is lost.
	Brief string
}

// ReadStore serves lease-free reads: status rendering and both exec
// boundaries load their context this way, never through a controller unit
// of work.
type ReadStore interface {
	// ListRuns lists the repository's runs. repositoryRoot is the
	// symlink-resolved absolute repository root — the design's repository
	// identity — exactly as cmd/hop's single resolver canonicalizes it;
	// composition never holds a RepositoryID, so the adapter resolves
	// repositoryRoot to its internal repository row itself (the same
	// get-or-create lookup InitializeRun uses). An unknown root returns no
	// runs, never an error: a repository with no run yet has none to list.
	ListRuns(ctx context.Context, repositoryRoot string) ([]RunStatus, error)
	LoadRunStatus(ctx context.Context, run identity.RunID) (RunDetail, error)
	// LoadFrozenRun returns the run's frozen snapshot and repository root.
	LoadFrozenRun(ctx context.Context, run identity.RunID) (FrozenRun, error)
	LoadCheckExecutionContext(ctx context.Context, op identity.OperationID) (CheckExecutionContext, error)
}
