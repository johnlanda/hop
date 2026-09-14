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
	RunID       identity.RunID
	Sequence    int
	State       run.RunState
	Reconciling bool // at least one operation for the run is OperationReconciling
	UpdatedAt   time.Time
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
	TaskID            identity.TaskID
	AttemptID         identity.AttemptID
	SessionID         identity.SessionID
	TaskState         run.TaskState
	AttemptState      run.AttemptState
	WorktreePath      string
	Binding           *run.RuntimeBinding
	Claim             *LaunchClaim
	PendingOperations []Operation
	LastSubmission    *SubmissionOutcome
	Artifacts         []run.Artifact
}

// LaunchContext is what the launch exec boundary (`hop launch`) needs,
// loaded without a lease: the frozen snapshot, the session's current
// binding and incarnation, the launch-claim state and the run's stop state.
type LaunchContext struct {
	Snapshot      RunSnapshot
	Harness       run.Harness
	Attempt       run.Attempt
	Session       run.Session
	Binding       run.RuntimeBinding
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
	LoadLaunchContext(ctx context.Context, run identity.RunID, attempt identity.AttemptID) (LaunchContext, error)
	LoadCheckExecutionContext(ctx context.Context, op identity.OperationID) (CheckExecutionContext, error)
}
