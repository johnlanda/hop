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

// RunDetail is the full detail block `hop status -run` renders, and the
// identities the controller use cases (stop, resume) need to load and
// mutate the run's task, attempt and session directly. Phase 2 gives every
// run exactly one task and one attempt, so TaskID and AttemptID are always
// well-defined once the run exists; SessionID is the attempt's current
// non-terminated session and is the zero value when none exists (briefly
// possible mid-reconciliation, between a lost session and its replacement).
type RunDetail struct {
	RunStatus
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
}

// LaunchContext is what the launch exec boundary (`hop launch`) needs,
// loaded without a lease. hop launch's own argv IS the pane's command
// (docs/plan/phase-2-design.md section 6): it starts the instant
// layout.apply creates the pane, which can be before the controller's
// pane.open outcome transaction — the one that records the runtime binding
// — has even committed. LoadLaunchContext is therefore satisfiable from
// the run snapshot and the recorded launch intent alone and never depends
// on a binding existing yet: IncarnationID is the incarnation the pending
// pane.open (or launch.send) operation's intent recorded, which
// HOP_INCARNATION_ID must match.
type LaunchContext struct {
	Snapshot RunSnapshot
	Harness  run.Harness
	Attempt  run.Attempt
	Session  run.Session
	// WorktreePath is the run's recorded worktree path exactly as the
	// worktree row persisted it (worktree.create's outcome; recorded
	// before any pane exists, so exposing it keeps this context free of
	// the later binding), or "" when no worktree row exists yet.
	// PrepareLaunchExec refuses a launch whose working directory does not
	// canonically resolve to it — the workspace-trust seed and the exec
	// must target the attempt's own worktree, never a foreign directory.
	WorktreePath  string
	IncarnationID identity.IncarnationID
	Claim         *LaunchClaim
	StopRequested bool
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
	LoadLaunchContext(ctx context.Context, run identity.RunID, attempt identity.AttemptID) (LaunchContext, error)
	LoadCheckExecutionContext(ctx context.Context, op identity.OperationID) (CheckExecutionContext, error)
}
