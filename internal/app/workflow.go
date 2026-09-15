package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// ErrWorkflowRepositoriesUnsupported reports that a UnitOfWork value does
// not implement WorkflowRepositories: the wired StateStore predates Phase 3
// feature-mode support. Every feature-mode use case that needs the
// dependency, message, review or integration repositories, or the run's
// manager session, must check for this before any side effect and fail
// closed — never a nil-interface panic, never a silent solo-mode fallback.
// Solo-mode use cases never perform this assertion at all.
var ErrWorkflowRepositoriesUnsupported = errors.New("app: unit of work does not implement WorkflowRepositories")

// ErrWorkflowReadStoreUnsupported reports that a ReadStore value does not
// implement WorkflowReadStore, for the identical reason and with the
// identical fail-closed contract as ErrWorkflowRepositoriesUnsupported.
var ErrWorkflowReadStoreUnsupported = errors.New("app: read store does not implement WorkflowReadStore")

// ErrFeatureModeUnsupported reports that a driving use case needed one of
// Controller's optional feature-mode ports (Messages, Plan, Reviews,
// Workspaces) and found it nil: this Controller was wired for solo-mode
// runs only. Checked and returned before any side effect; solo-mode use
// cases never perform this check.
var ErrFeatureModeUnsupported = errors.New("app: controller is not configured for feature-mode workflow operations")

// WorkflowRepositories is an additive capability a controller UnitOfWork
// may also implement: the Phase 3 repositories a feature-mode controller
// transaction reaches alongside the Phase 2 ones already on UnitOfWork
// (TaskDependencies for dependency release, Messages for controller info
// notices committed inside the transition transaction they report, Reviews
// and Integrations for review-task creation and integration state,
// ManagerSession for delegation and addressing).
//
// This is declared as a SEPARATE interface, never as new methods added to
// UnitOfWork itself, so the existing SQLite adapter's concrete unitOfWork
// type keeps satisfying UnitOfWork unmodified until it also implements this
// interface (slice 3 adds `var _ app.WorkflowRepositories =
// (*unitOfWork)(nil)`); a later slice may fold these getters into
// UnitOfWork proper as a non-additive flip once every implementer is
// updated together (slice 6). A caller obtains this capability by
// type-asserting the UnitOfWork value StateStore.Begin returned:
//
//	wf, ok := uow.(app.WorkflowRepositories)
//	if !ok {
//	        return fmt.Errorf("%w: ...", app.ErrWorkflowRepositoriesUnsupported)
//	}
//
// RequireWorkflowRepositories performs exactly this assertion.
type WorkflowRepositories interface {
	TaskDependencies() TaskDependencyRepository
	// TaskIndex lists a run's tasks in bulk — named apart from
	// UnitOfWork.Tasks() (a single Go type cannot expose two methods
	// named Tasks with different return types) even though the same
	// concrete unit of work implements both.
	TaskIndex() TaskIndexRepository
	// AttemptIndex lists a task's attempts in bulk and creates new ones —
	// named apart from UnitOfWork.Attempts() for the identical reason.
	AttemptIndex() AttemptIndexRepository
	// SessionIndex lists a run's sessions in bulk — named apart from
	// UnitOfWork.Sessions() for the identical reason.
	SessionIndex() SessionIndexRepository
	Messages() MessageRepository
	Reviews() ReviewRepository
	Integrations() IntegrationRepository
	// RetryRequests reads and consumes the manager's pending hop task
	// retry requests.
	RetryRequests() RetryRequestRepository
	// ManagerSession returns the run's current (non-terminal) manager
	// session — the partial-unique-index invariant makes this well
	// defined once InitializeRun has created one. ErrNotFound when none
	// exists yet.
	ManagerSession(ctx context.Context, runID identity.RunID) (run.Session, int64, error)
}

// RequireWorkflowRepositories asserts that uow also implements
// WorkflowRepositories, returning ErrWorkflowRepositoriesUnsupported
// (wrapped with context) when it does not. Every feature-mode use case
// calls this before any side effect inside its unit of work; solo-mode use
// cases never call it.
func RequireWorkflowRepositories(uow UnitOfWork, reason string) (WorkflowRepositories, error) {
	wf, ok := uow.(WorkflowRepositories)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrWorkflowRepositoriesUnsupported, reason)
	}
	return wf, nil
}

// WorkflowReadStore is an additive capability a ReadStore may also
// implement: the Phase 3 lease-free reads (session-addressed launch
// context, and the messaging CLI verbs' address/incarnation resolution).
// Declared separately from ReadStore for the identical reason
// WorkflowRepositories is declared separately from UnitOfWork — adding
// methods to ReadStore itself would break the existing SQLite adapter's
// compile before slice 3 lands. RequireWorkflowReadStore performs the
// type assertion a caller needs to reach it.
type WorkflowReadStore interface {
	// LoadSessionLaunchContext addresses launch context by session rather
	// than by attempt, covering sessions without one (the manager). Its
	// pending-intent resolution is session-keyed, matching the
	// session-keyed claim fallback (section 3/4).
	LoadSessionLaunchContext(ctx context.Context, runID identity.RunID, session identity.SessionID) (SessionLaunchContext, error)
	// LoadMessagingContext resolves session's OWN run, logical address
	// (manager, task:<id> via its attempt's task, lineage-based so a
	// successor session resolves the same address as its predecessor) and
	// current incarnation, for the message CLI verbs. Every driving
	// messaging use case compares the returned RunID against its own
	// caller-supplied one and refuses a mismatch before any side effect —
	// a session belongs to exactly one run, and this is the ONLY
	// authoritative source for which one.
	LoadMessagingContext(ctx context.Context, session identity.SessionID) (MessagingContext, error)
	// LoadMessageDetail is `hop msg show`'s entire lookup: a message's
	// immutable envelope plus its full delivery/ack history, addressed by
	// run and message id alone — read-only, no lease, and callable by any
	// of the run's sessions or the human controller-machine context
	// (section 7's grammar table explicitly distinguishes this from the
	// worker-authority MessageRepository.Get reachable only inside a
	// controller transaction through WorkflowRepositories). A message
	// belonging to a different run is reported exactly like one that does
	// not exist — ErrNotFound wrapped with context — so this lookup can
	// never leak envelope content across runs.
	LoadMessageDetail(ctx context.Context, runID identity.RunID, messageID identity.MessageID) (MessageDetail, error)
}

// MessageDetail is LoadMessageDetail's result: a message's immutable
// envelope plus its full delivery/ack history — the read-only `hop msg
// show` recovery and audit surface (section 7's grammar table: one
// `delivered: <session> <time>` line per entry in Deliveries, plus an
// `acknowledged: <time>` line when Ack is non-nil). Deliveries are
// ordered oldest first, re-serves included; Ack is nil until the message
// is acknowledged.
type MessageDetail struct {
	Message    run.Message
	Deliveries []run.Delivery
	Ack        *run.Ack
}

// RequireWorkflowReadStore asserts that read also implements
// WorkflowReadStore, returning ErrWorkflowReadStoreUnsupported (wrapped
// with context) when it does not.
func RequireWorkflowReadStore(read ReadStore, reason string) (WorkflowReadStore, error) {
	wf, ok := read.(WorkflowReadStore)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrWorkflowReadStoreUnsupported, reason)
	}
	return wf, nil
}

// TaskDependencyRepository reads a run's persisted dependency edges. Edges
// are immutable once created (PlanStore.CreateTask writes them, validated
// against ValidateAcyclic, inside its own worker-authority transaction);
// this repository is read-only because the controller only ever consumes
// edges (dependency release), never creates them.
type TaskDependencyRepository interface {
	// ByRun returns every dependency edge recorded for runID, for the
	// scheduler's dependency-release pass.
	ByRun(ctx context.Context, runID identity.RunID) ([]run.TaskDependency, error)
}

// TaskIndexRepository lists a run's tasks in bulk: the scheduler's
// dependency-release and assignment passes need every task at once rather
// than one Get by id.
type TaskIndexRepository interface {
	// ByRun returns every task recorded for runID, in no particular
	// order; callers sort as their pass requires (task seq order for
	// assignment).
	ByRun(ctx context.Context, runID identity.RunID) ([]run.Task, error)
}

// AttemptIndexRepository lists a task's attempts in bulk and creates new
// ones. Attempt creation lives here rather than on AttemptRepository
// because a controller transaction reserves a task's FIRST attempt at
// assignment time with a bare insert, unlike InitializeRun's bootstrap
// creation of a solo run's one attempt, which stays StateStore's own
// concern. A retry's successor attempt is instead reserved immediately by
// PlanStore.RequestRetry (worker-authority — the CLI grammar reports the
// attempt number in that same response); the controller's assignment
// pass only reads it back here to launch it.
type AttemptIndexRepository interface {
	// ByTask returns every attempt reserved for task, in reservation
	// order (ascending Number) — retry provenance and "does a prior
	// attempt already exist" queries.
	ByTask(ctx context.Context, task identity.TaskID) ([]run.Attempt, error)
	// Create inserts a newly reserved attempt row, returning its initial
	// revision.
	Create(ctx context.Context, a run.Attempt) (int64, error)
}

// SessionIndexRepository lists a run's sessions in bulk: the scheduler's
// bounded-concurrency slot count needs every non-manager session's state
// at once rather than one Current lookup per attempt.
type SessionIndexRepository interface {
	// ByRun returns every session recorded for runID, in no particular
	// order.
	ByRun(ctx context.Context, runID identity.RunID) ([]run.Session, error)
}

// RetryRequestState is a retry request's own small lifecycle.
type RetryRequestState string

// Retry request states.
const (
	RetryRequestPending  RetryRequestState = "pending"
	RetryRequestConsumed RetryRequestState = "consumed"
)

// RetryRequestRecord is one manager-authored `hop task retry`'s
// bookkeeping row: PlanStore.RequestRetry validates and reserves the new
// attempt IMMEDIATELY (the CLI grammar's `retry accepted t<seq> attempt
// <n>` names the attempt number in that same response), and records this
// row (state pending) as the durable fact a retry happened; the
// scheduler's assignment pass marks it consumed once it actually launches
// the reserved attempt. TaskID is the natural key: UNIQUE(task_id) WHERE
// state='pending' means at most one pending request per task.
type RetryRequestRecord struct {
	TaskID      identity.TaskID
	RequestedBy identity.SessionID
	Reason      string
	RequestID   string
	CreatedAt   time.Time
}

// RetryRequestRepository reads and consumes pending retry-request
// bookkeeping rows: PlanStore.RequestRetry is this record's only writer
// (as pending); the controller's assignment pass is its only consumer,
// marking a row consumed once it launches the attempt RequestRetry
// already reserved.
type RetryRequestRepository interface {
	// Pending returns the run's pending retry requests, oldest first.
	Pending(ctx context.Context, runID identity.RunID) ([]RetryRequestRecord, error)
	// MarkConsumed marks task's pending request consumed, recording the
	// attempt number that was launched — what an identical request-ID
	// retry against the original PlanStore.RequestRetry receipt returns
	// instead of a second pending row.
	MarkConsumed(ctx context.Context, task identity.TaskID, attemptNumber int) error
}

// MessageRepository is the controller-side view of the message journal
// (envelopes only — deliveries and acks are MessagingStore's own internal
// concern, since only a worker-authority transaction ever fetches or acks):
// controller-authored info notices are created here, inside the same
// transaction as the transition they report, and read back for
// re-address-free lineage queries (e.g. naming a task's orphaned
// obligations at mailbox closure). This repository never resolves
// Message.State (the application's reconstructed delivery/ack view);
// callers needing state go through MessagingStore.
type MessageRepository interface {
	// Create inserts one immutable message envelope, m.EnqueueSeq assigned
	// by the store as the next value for (m.RunID, m.Recipient) — the
	// durable per-address sequence — and returns the stored value.
	Create(ctx context.Context, m run.Message) (run.Message, error)
	// Get resolves one message envelope by id.
	Get(ctx context.Context, id identity.MessageID) (run.Message, error)
	// ByAddress returns every envelope recorded for one recipient address,
	// oldest enqueue sequence first.
	ByAddress(ctx context.Context, runID identity.RunID, address run.Address) ([]run.Message, error)
}

// ReviewRepository is the controller-side read of accepted review
// verdicts: writes happen only through ReviewStore.SubmitReview
// (worker-authority), never here.
type ReviewRepository interface {
	// Latest returns the run's most recently accepted review, or nil when
	// none exists — EvaluateReadiness's GuardContext.LatestReview input.
	Latest(ctx context.Context, runID identity.RunID) (*run.Review, error)
	// ByAttempt returns the accepted review for one attempt, or nil.
	ByAttempt(ctx context.Context, attempt identity.AttemptID) (*run.Review, error)
}

// IntegrationRepository loads and saves Integration rows: the controller's
// own journal of serial merge/publish/check/reset operations (section 4,
// 2b's territory to drive; declared here so its shape is frozen for every
// consumer).
type IntegrationRepository interface {
	Get(ctx context.Context, id identity.IntegrationID) (run.Integration, int64, error)
	Create(ctx context.Context, i run.Integration) (int64, error)
	Save(ctx context.Context, i run.Integration, expectedRevision int64) (int64, error)
	// Current returns the run's non-terminal integration, if any — the
	// store-enforced serial slot (partial unique index).
	Current(ctx context.Context, runID identity.RunID) (run.Integration, int64, bool, error)
	// ByTask returns every integration recorded for task, newest first:
	// provenance across a task's retries.
	ByTask(ctx context.Context, task identity.TaskID) ([]run.Integration, error)
}

// SessionLaunchContext is LoadSessionLaunchContext's result: what `hop
// launch --session` needs, addressed by session rather than by attempt so
// it covers the manager. AttemptID is the zero value for the manager.
type SessionLaunchContext struct {
	Snapshot      RunSnapshot
	Harness       run.Harness
	Session       run.Session
	AttemptID     identity.AttemptID
	IncarnationID identity.IncarnationID
	Claim         *LaunchClaim
	StopRequested bool
}

// MessagingContext is LoadMessagingContext's result: the caller session's
// OWN run (never the caller-supplied one — every driving messaging use
// case must compare this against its own parsed RunID and refuse a
// mismatch before any side effect, since a session belongs to exactly one
// run and nothing else may authorize a cross-run request), resolved
// logical address and current incarnation, lease-free.
type MessagingContext struct {
	RunID         identity.RunID
	Address       run.Address
	IncarnationID identity.IncarnationID
	StopRequested bool
}
