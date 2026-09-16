package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// ErrRevisionConflict reports that a repository Save targeted a row whose
// revision has moved since it was read: another writer committed first.
var ErrRevisionConflict = errors.New("app: revision conflict")

// ErrFenced reports that a controller unit of work could not commit because
// its lease no longer matches the run's current lease row at commit time:
// another controller took over, the lease was released, or it expired.
// Fencing the outcome commit does not fence the act itself; the operation
// decision table, not this error alone, decides what an already-performed
// act means to a superseded controller.
var ErrFenced = errors.New("app: lease fenced")

// ErrNotFound reports that a repository or read-store lookup found no row
// for the given identity.
var ErrNotFound = errors.New("app: not found")

// ErrLeaseHeld reports that AcquireLease found the run's lease row neither
// released nor expired: a live controller already holds it.
var ErrLeaseHeld = errors.New("app: lease is held")

// Lease is a controller's fenced claim on one run's exclusive write
// authority: a monotonic generation the run's lease row carries for the
// life of the row, preserved across release and reacquisition. Heartbeat
// and ReleaseLease are compare-and-swap mutations against this exact value;
// Begin binds a unit of work to it, and Commit re-reads the lease row and
// fails with ErrFenced unless it still matches and is unexpired.
type Lease struct {
	Run          identity.RunID
	ControllerID string
	Generation   int64
	ExpiresAt    time.Time
}

// RunSnapshot is a run's immutable effective configuration, frozen once by
// InitializeRun and applied unchanged at every later exec boundary: the
// check contract, the versioned environment policy (unvalidated here —
// EnvPolicy.Validate and SanitizeEnvironment are called only by the
// exec-boundary commands, never by controller use-case code), the launched
// harness, the optional alternate profile directory, the absolute state
// root and the assignment artifact's path and digest.
type RunSnapshot struct {
	CheckArgv        []string
	CheckTimeout     time.Duration
	CheckRepeatable  bool
	EnvPolicy        EnvPolicy
	Harness          string
	ProfileDir       string
	StateRoot        string // absolute
	AssignmentPath   string // absolute
	AssignmentDigest string
	// Workflow is the run's frozen feature-mode policy: the zero value
	// means solo (docs/plan/phase-3-design.md section 4, run_snapshots
	// "workflow JSON NULL means solo"). Solo-mode code never reads it.
	Workflow WorkflowSnapshot
}

// WorkflowSnapshot is a feature-mode run's frozen [workflow]/[workers]/
// [retry]/[roles]/[messages] policy plus the integration branch name
// derived from the run's sequence at freeze (docs/plan/phase-3-design.md
// sections 3, 6). The role artifact fields are the frozen COPIES'
// path/digest under the run's artifact directory — never the repository's
// own role files, which a mid-run edit must not affect (the Phase 2
// assignment-artifact snapshot-immutability rule extended to roles).
type WorkflowSnapshot struct {
	// Mode is "feature"; solo runs carry the zero WorkflowSnapshot instead
	// of a Mode of "solo", so a zero value alone means solo.
	Mode                  string
	MaxWorkers            int
	RetryLimit            int
	ManagerRolePath       string
	ManagerRoleDigest     string
	ImplementerRolePath   string
	ImplementerRoleDigest string
	ReviewerRolePath      string
	ReviewerRoleDigest    string
	ReviewerHarness       string
	MessageAttention      time.Duration
	MessageWait           time.Duration
	// IntegrationBranch is "hop/r<seq>/integration" (section 6): one
	// ref-namespace scheme, computed once at freeze from the run's
	// sequence and frozen so every later reference uses the identical
	// name. A feature-mode InitializeRun refuses a snapshot whose branch
	// does not name the sequence the store assigns (ErrRunSequenceMismatch).
	IntegrationBranch string
	// BaseCommitOID is the repository HEAD at freeze, resolved to an
	// object ID (section 6, "Integration branch and worktree bases"): the
	// commit the integration branch is created at. Frozen here, inside
	// InitializeRun's transaction, so a crash before the integration.init
	// intent commits never loses the base.
	BaseCommitOID string
}

// Feature is true when the snapshot describes a feature-mode run. A solo
// run's zero WorkflowSnapshot always reports false.
func (w WorkflowSnapshot) Feature() bool { return w.Mode == "feature" } //nolint:gocritic // hugeParam: WorkflowSnapshot is a small value type read throughout the codebase by value, mirroring RunSnapshot's own convention; called at most a few times per controller pass, never a hot loop.

// ErrFeatureRunSpecInvalid reports a feature-mode NewRunSpec that
// InitializeRun refuses before writing anything. A feature run's first
// transaction creates the run, its snapshot, the manager session and the
// first lease only (docs/plan/phase-3-design.md section 9), so a spec
// carrying a task, attempt or worktree identity — the solo bootstrap
// shape — or lacking the manager session identity is refused.
var ErrFeatureRunSpecInvalid = errors.New("app: feature-mode run spec is invalid")

// ErrRunSequenceMismatch reports a feature-mode InitializeRun whose frozen
// integration branch does not name the sequence the store assigned the
// run: the freeze predicted a sequence another run took first. Nothing is
// committed; the caller re-freezes against a fresh prediction.
var ErrRunSequenceMismatch = errors.New("app: the frozen integration branch does not name the run's assigned sequence")

// ValidateFeatureRunSpec checks the feature-mode shape of spec, shared by
// every StateStore implementation so the refusal is identical everywhere:
// the manager session identity is required, and the solo bootstrap
// identities (task, attempt, worktree) must be absent. Errors wrap
// ErrFeatureRunSpecInvalid and name fields, never values.
func ValidateFeatureRunSpec(spec *NewRunSpec) error {
	if spec.SessionID == "" {
		return fmt.Errorf("%w: the manager session identity is required", ErrFeatureRunSpecInvalid)
	}
	for _, field := range []struct {
		name string
		set  bool
	}{
		{"a task", spec.TaskID != ""},
		{"an attempt", spec.AttemptID != ""},
		{"a worktree", spec.WorktreeID != ""},
	} {
		if field.set {
			return fmt.Errorf("%w: a feature run is initialized without %s; tasks, attempts and worktrees are created by the scheduling pass", ErrFeatureRunSpecInvalid, field.name)
		}
	}
	return nil
}

// RequireIntegrationBranchForSequence checks that a feature snapshot's
// frozen integration branch names seq, the sequence the store assigned
// inside InitializeRun's transaction. Errors wrap ErrRunSequenceMismatch.
func RequireIntegrationBranchForSequence(w *WorkflowSnapshot, seq int) error {
	if w.IntegrationBranch != IntegrationBranchName(seq) {
		return fmt.Errorf("%w: the run was assigned sequence %d", ErrRunSequenceMismatch, seq)
	}
	return nil
}

// NewRunSpec is the complete, application-assembled input to InitializeRun:
// every identity, digest and frozen value the run's first transaction
// commits atomically. The application generates every identity except
// RepositoryID (get-or-create by resolved root path) through IDGenerator
// and computes every digest before this call. InitializeRun never writes
// the assignment artifact's content: it commits Snapshot.AssignmentPath and
// AssignmentDigest as the run's frozen intent, and the caller writes the
// file through ArtifactStore afterward, as a separate act — no external
// call, including a local file write, happens inside this transaction.
//
// A feature-mode spec (Snapshot.Workflow.Feature()) has a different shape
// (ValidateFeatureRunSpec): SessionID, Harness and NativeSessionRef
// describe the MANAGER session, and TaskID, AttemptID and WorktreeID are
// empty — tasks, attempts and worktrees belong to the scheduling pass.
// IncarnationID is never persisted by InitializeRun in either mode.
type NewRunSpec struct {
	// RepositoryRoot is the symlink-resolved absolute repository root — the
	// design's repository identity — canonicalized by cmd/hop's single
	// resolver. The adapter resolves it to its internal repository row
	// through the same get-or-create lookup ReadStore.ListRuns uses.
	RepositoryRoot string

	RunID         identity.RunID
	TaskID        identity.TaskID
	AttemptID     identity.AttemptID
	SessionID     identity.SessionID
	WorktreeID    identity.WorktreeID
	IncarnationID identity.IncarnationID

	Brief              string
	BriefDigest        string
	InstructionsDigest string

	Snapshot RunSnapshot

	Harness          run.Harness
	NativeSessionRef string // pre-assigned Claude --session-id UUID

	ControllerID string
	Now          time.Time
}

// RunRepository loads and saves Run values. Save fails with
// ErrRevisionConflict when the row's revision no longer matches
// expectedRevision.
type RunRepository interface {
	Get(ctx context.Context, id identity.RunID) (run.Run, int64, error)
	Save(ctx context.Context, r run.Run, expectedRevision int64) (int64, error)
}

// TaskRepository loads and saves Task values.
type TaskRepository interface {
	Get(ctx context.Context, id identity.TaskID) (run.Task, int64, error)
	Save(ctx context.Context, t run.Task, expectedRevision int64) (int64, error)
}

// AttemptRepository loads and saves Attempt values.
type AttemptRepository interface {
	Get(ctx context.Context, id identity.AttemptID) (run.Attempt, int64, error)
	Save(ctx context.Context, a run.Attempt, expectedRevision int64) (int64, error)
}

// SessionRepository loads, creates and saves Session values. Current
// returns the attempt's non-terminated session; Phase 2 has at most one at
// any instant. Create inserts a new session for a cold relaunch (a fresh
// incarnation bound to the same attempt); InitializeRun creates the first
// session directly.
type SessionRepository interface {
	Get(ctx context.Context, id identity.SessionID) (run.Session, int64, error)
	Current(ctx context.Context, attempt identity.AttemptID) (run.Session, int64, error)
	Save(ctx context.Context, s run.Session, expectedRevision int64) (int64, error)
	Create(ctx context.Context, s run.Session) (int64, error)
}

// WorktreeRepository loads, creates and saves worktree rows: a solo run's
// single unlinked worktree, or a feature run's per-attempt worktrees, each
// linked to its attempt and verified base commit (run.NewAttemptWorktree).
// ByRun serves the solo shape only. Create refuses a link naming an
// unknown attempt (ErrNotFound) or another run's attempt (ErrFenced).
// InitializeRun never creates the worktree row: worktree.create is a
// fallible external call performed afterward, so Create records it only
// once Runtime.CreateWorktree has actually succeeded.
type WorktreeRepository interface {
	Get(ctx context.Context, id identity.WorktreeID) (run.Worktree, int64, error)
	ByRun(ctx context.Context, runID identity.RunID) (run.Worktree, int64, error)
	Create(ctx context.Context, w run.Worktree) (int64, error)
	Save(ctx context.Context, w run.Worktree, expectedRevision int64) (int64, error)
}

// ResultRepository reads accepted results. Results are written only through
// SubmissionStore.SubmitResult (a worker-authority write, never a
// controller unit of work); the controller only ever reads the accepted
// result, for example to learn the commit a check must validate.
type ResultRepository interface {
	// Accepted returns the attempt's accepted result, or nil when none has
	// been accepted yet.
	Accepted(ctx context.Context, attempt identity.AttemptID) (*run.Result, error)
}

// ArtifactRepository records artifact references.
type ArtifactRepository interface {
	Save(ctx context.Context, a run.Artifact) error
}

// BindingRepository reads and appends RuntimeBinding history. Current
// returns the session's current (non-superseded) binding, when one exists.
// Bindings are append-only: Create inserts a new incarnation's row, and
// Save persists an Observe/Supersede update to an existing row.
type BindingRepository interface {
	Current(ctx context.Context, session identity.SessionID) (run.RuntimeBinding, bool, error)
	Create(ctx context.Context, b run.RuntimeBinding) error
	Save(ctx context.Context, b run.RuntimeBinding) error
}

// OperationKind names one row of the section 4 operation decision table.
type OperationKind string

// Operation kinds.
const (
	OpWorktreeCreate OperationKind = "worktree.create"
	OpPaneOpen       OperationKind = "pane.open"
	OpLaunchSend     OperationKind = "launch.send"
	OpPaneClose      OperationKind = "pane.close"
	OpCheckRun       OperationKind = "check.run"
	// OpAbsenceAttested records a hop resume --confirm-absent human
	// attestation; it is never a decision-table act, only a journal entry.
	OpAbsenceAttested OperationKind = "absence.attested"
)

// OperationState is the operation journal's own lifecycle, independent of
// the entity states an operation's outcome causes.
type OperationState string

// Operation states.
const (
	OperationPending     OperationState = "pending"
	OperationSucceeded   OperationState = "succeeded"
	OperationFailed      OperationState = "failed"
	OperationReconciling OperationState = "reconciling"
)

// Operation is one row of the application's operation journal: a
// controller-side intent/act/outcome record. Intent, ActEvidence and
// Outcome carry operation-kind-specific payload values (for example
// worktreeCreateIntent or checkRunOutcome); the store persists whatever
// value is given without interpreting it. A human attestation
// (`hop resume --confirm-absent`) is recorded as its own journal entry,
// kind OpAbsenceAttested, with the reported evidence in Outcome.
type Operation struct {
	ID          identity.OperationID
	RunID       identity.RunID
	Generation  int64
	Kind        OperationKind
	State       OperationState
	Intent      any
	ActEvidence any
	Outcome     any
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// OperationRepository records and updates the operation journal. Pending
// lists operations still open (pending or reconciling) for a run, oldest
// first — what a controller must resolve before any new act of the same
// kind (section 4). ByKind lists every operation of one kind for a run,
// newest first, regardless of state — recovery reads a settled
// operation's evidence (for example the worktree.create outcome's
// workspace) through it.
type OperationRepository interface {
	Create(ctx context.Context, op Operation) error
	Get(ctx context.Context, id identity.OperationID) (Operation, error)
	Save(ctx context.Context, op Operation) error
	Pending(ctx context.Context, runID identity.RunID) ([]Operation, error)
	ByKind(ctx context.Context, runID identity.RunID, kind OperationKind) ([]Operation, error)
}

// EntityKind names the kind of entity a Transition row describes.
type EntityKind string

// Entity kinds a Transition may describe.
const (
	EntityRun     EntityKind = "run"
	EntityTask    EntityKind = "task"
	EntityAttempt EntityKind = "attempt"
	EntitySession EntityKind = "session"
)

// Transition is one append-only evidence row recording an entity's observed
// state change. Generation is nil for a non-controller write (a worker
// transaction, or a lease operation).
type Transition struct {
	EntityKind EntityKind
	EntityID   string
	From       string
	To         string
	Reason     string
	Generation *int64
	At         time.Time
}

// TransitionRepository records transition evidence.
type TransitionRepository interface {
	Record(ctx context.Context, t Transition) error
}

// CheckRequestState is a check request's own small lifecycle, distinct from
// the check execution operation it causes.
type CheckRequestState string

// Check request states.
const (
	CheckRequestRequested CheckRequestState = "requested"
	CheckRequestClaimed   CheckRequestState = "claimed"
	CheckRequestSettled   CheckRequestState = "settled"
)

// CheckRequest is the durable record of one accepted result awaiting its
// deterministic check. SubmissionStore.SubmitResult inserts it accepted;
// the controller claims it (recording ClaimedGeneration) before acting.
type CheckRequest struct {
	ResultID          identity.ResultID
	AttemptID         identity.AttemptID
	State             CheckRequestState
	CreatedAt         time.Time
	ClaimedGeneration *int64
}

// CheckRequestRepository reads and updates check requests. Pending returns
// the run's oldest unclaimed request, if any.
type CheckRequestRepository interface {
	Get(ctx context.Context, resultID identity.ResultID) (CheckRequest, error)
	Pending(ctx context.Context, runID identity.RunID) (CheckRequest, bool, error)
	Save(ctx context.Context, cr CheckRequest) error
}

// CheckExecClaim is the durable record hop check-exec writes before it
// execs the check argv: its own pid, which IS the process-group id (it
// verifies it leads its own group first). It is read-only from the
// controller side — SubmissionStore.ClaimCheckExec is the only writer —
// and is read inside a lease-fenced unit of work, like LaunchClaims,
// because stop and resume decide process-group retirement against a
// consistent snapshot within that same transaction.
type CheckExecClaim struct {
	OperationID identity.OperationID
	PID         int
	ClaimedAt   time.Time
}

// CheckExecClaimRepository reads check-exec claims. Used by stop and
// resume to find the process group to retire when a check execution's
// controller crashed or a stop was requested while it ran.
type CheckExecClaimRepository interface {
	Get(ctx context.Context, op identity.OperationID) (CheckExecClaim, bool, error)
}

// UnitOfWork is one controller transaction, fenced by the lease Begin was
// called with. Commit re-reads the lease inside the transaction and fails
// with ErrFenced unless it still matches (run, controller, generation,
// held) and is unexpired at commit time.
type UnitOfWork interface {
	Runs() RunRepository
	Tasks() TaskRepository
	Attempts() AttemptRepository
	Sessions() SessionRepository
	Worktrees() WorktreeRepository
	Results() ResultRepository
	Artifacts() ArtifactRepository
	Bindings() BindingRepository
	LaunchClaims() LaunchClaimRepository
	CheckExecClaims() CheckExecClaimRepository
	Operations() OperationRepository
	Transitions() TransitionRepository
	CheckRequests() CheckRequestRepository
	Commit() error
	Rollback() error
}

// StateStore is the controller's authority: it is the only place a run's
// lease is created, acquired, extended or released, and the only source of
// fenced units of work. See docs/plan/phase-2-design.md section 4,
// "Transaction authorities".
type StateStore interface {
	// InitializeRun atomically creates the repository row (get-or-create by
	// resolved root path), the run with its sequence number, the frozen
	// snapshot, the task, attempt and session rows, and the initial
	// controller lease, in one transaction. This is the only way a run and
	// its first lease come into being, so there is no bootstrap cycle.
	// A feature-mode spec instead creates the run, the snapshot with its
	// workflow policy, the MANAGER session (role manager, no attempt, no
	// parent, the pre-assigned native reference) and the lease — no task,
	// attempt or worktree — refusing a malformed spec with
	// ErrFeatureRunSpecInvalid before any write and a frozen integration
	// branch naming another sequence with ErrRunSequenceMismatch inside
	// the transaction, committing nothing either way.
	InitializeRun(ctx context.Context, spec NewRunSpec) (identity.RunID, Lease, error)

	// AcquireLease claims or takes over an existing run's controller lease
	// and returns the new fencing generation. Generations are monotonic for
	// the life of the run row, preserved across release and reacquisition.
	// Acquisition succeeds only when the row is released or expired
	// (ErrLeaseHeld otherwise).
	AcquireLease(ctx context.Context, run identity.RunID, controllerID string) (Lease, error)
	// Heartbeat and ReleaseLease are compare-and-swap mutations: they
	// succeed only when the row still matches (run, controller_id,
	// generation, state = held). A stale controller can neither extend nor
	// release a successor's lease (ErrFenced). Release marks the row
	// released; it never deletes it and never touches the generation.
	Heartbeat(ctx context.Context, lease Lease) error
	ReleaseLease(ctx context.Context, lease Lease) error

	// Begin opens a controller unit of work bound to lease.
	Begin(ctx context.Context, lease Lease) (UnitOfWork, error)
}
