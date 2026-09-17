package run

import "errors"

// ErrInvalidTransition reports that an entity cannot move from its current
// state through the requested transition. The section 5 state tables are
// exhaustive: a (from, to) pair the tables do not list is invalid.
var ErrInvalidTransition = errors.New("run: invalid transition")

// ErrStaleSubmission reports that a result submission targets an attempt
// that is no longer eligible to accept new content: its incarnation is not
// the session's current binding, the run is stopping or stopped, or the
// attempt is in a state that never accepts a first result.
var ErrStaleSubmission = errors.New("run: stale submission")

// ErrConflictingResult reports that a result submission's digest differs
// from the attempt's already-accepted result. The accepted result is never
// replaced; the conflicting submission is recorded and rejected.
var ErrConflictingResult = errors.New("run: conflicting result")

// ErrDuplicateResult distinguishes the idempotent case: a submission whose
// digest matches the attempt's already-accepted result. Callers treat this
// as success, not failure; it is returned as an error only so acceptance
// has one uniform (outcome, error) shape.
var ErrDuplicateResult = errors.New("run: duplicate result")

// ErrTransientNotRunning reports that a result submission arrived for an
// attempt that is launching or relaunching with a launch claim that has not
// yet settled: the submission may become valid once the claim settles, and
// the caller is expected to retry.
var ErrTransientNotRunning = errors.New("run: attempt not yet running")

// ErrDependencyCycle reports that a task dependency edge would close a
// cycle in its run's dependency graph.
var ErrDependencyCycle = errors.New("run: dependency cycle")

// ErrDependencyNotIntegrated reports that a task is not release-eligible:
// at least one prerequisite has not reached integrated.
var ErrDependencyNotIntegrated = errors.New("run: dependency not integrated")

// ErrDelegationDepth reports a one-level-delegation violation: a session
// with a parent of its own was named as another session's parent.
var ErrDelegationDepth = errors.New("run: delegation depth exceeded")

// ErrDuplicateAnswer reports an answer resubmission whose body digest
// matches the question's already-accepted answer. Callers treat this as
// success, not failure; it is returned as an error only so acceptance has
// one uniform (outcome, error) shape.
var ErrDuplicateAnswer = errors.New("run: duplicate answer")

// ErrConflictingAnswer reports an answer resubmission whose body digest
// differs from the question's already-accepted answer. The accepted answer
// is never replaced.
var ErrConflictingAnswer = errors.New("run: conflicting answer")

// ErrAnswerNotRecipient reports an answer from a principal whose logical
// address is not the question's recipient: only the addressed recipient
// may answer a question, so no session ever answers a human-addressed
// question and no session answers another address's question.
var ErrAnswerNotRecipient = errors.New("run: answerer is not the question's recipient")

// ErrStaleAck reports an ack from an incarnation that is not the acking
// session's current, non-superseded one.
var ErrStaleAck = errors.New("run: stale ack")

// ErrNotDelivered reports an ack of a message with no delivery row recorded
// for the acking session itself: a predecessor session's delivery never
// authorizes a successor's ack.
var ErrNotDelivered = errors.New("run: message not delivered to this session")

// ErrVerdictSubjectMismatch reports a review submission whose subject
// commit and tree object IDs do not equal the review task's frozen
// subject.
var ErrVerdictSubjectMismatch = errors.New("run: verdict subject mismatch")

// ErrRetryNotTerminal reports a retry request against a task whose most
// recent attempt has not reached a terminal state.
var ErrRetryNotTerminal = errors.New("run: prior attempt is not terminal")

// ErrRetryLimit reports a retry request against a task that has already
// reached its frozen retry limit.
var ErrRetryLimit = errors.New("run: retry limit reached")

// ErrRunNotAccepting reports a manager verb (CreateTask, RequestRetry,
// ClosePlan) or an ordinary message send against a run that can never
// accept one again: completed, failed, stopping or stopped, or with a stop
// request. It is final.
var ErrRunNotAccepting = errors.New("run: run is not accepting this request")

// ErrRunNotYetRunning reports a manager verb or an ordinary message send
// against a run that is not running now but can still reach running:
// created, launching, resuming or completing, with no stop request. The
// caller is expected to retry, like ErrTransientNotRunning for a result
// submission.
var ErrRunNotYetRunning = errors.New("run: run not yet running")

// ErrEmptyPlan reports a plan closure attempted with zero implement tasks
// created.
var ErrEmptyPlan = errors.New("run: plan has no implement task")

// ErrMailboxClosed reports a send or an answer addressed to a task whose
// mailbox admission has closed.
var ErrMailboxClosed = errors.New("run: mailbox is closed")

// ErrRequestConflict reports a caller-stable request ID reused with
// different request content than its original acceptance.
var ErrRequestConflict = errors.New("run: request id reused with different content")

// ErrMailboxNotClear reports a result or verdict submission whose task's
// mailbox still has a queued or delivered-unacknowledged message: section
// 5's drain-then-submit contract (`transient: undelivered messages; drain
// with hop msg next, ack, then resubmit`). Distinct from ErrMailboxClosed:
// a mailbox can be open yet not clear.
var ErrMailboxNotClear = errors.New("run: mailbox has undelivered messages")

// ErrTaskNotReleased reports a direct pending->active activation attempted
// against a task that has dependencies: a dependent task must be released
// (pending->ready, every prerequisite integrated) before it can activate.
// A task with no dependencies is exempt (the Phase 2/solo legacy path,
// where a task never carries a dependency graph and activates straight
// from pending).
var ErrTaskNotReleased = errors.New("run: task has dependencies and is not released")

// ErrDependencyEvidenceMissing reports that Release's supplied edges do
// not match the task's own HasDependencies shape: a dependent task
// (HasDependencies true) was given zero edges, or a zero-dependency task
// (HasDependencies false) was given a non-empty edge set. Distinct from
// ErrDependencyNotIntegrated, which reports a shape-consistent but still
// incomplete or unintegrated evidence set.
var ErrDependencyEvidenceMissing = errors.New("run: release evidence does not match the task's dependency shape")
