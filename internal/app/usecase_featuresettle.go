package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// taskConsequence names the task-side consequence of a terminal
// integration or check settlement (docs/plan/phase-3-design.md section
// 5): needs-rework with retry budget remaining, failed at the exhausted
// limit, interrupted under a held stop.
type taskConsequence string

const (
	taskConsequenceNeedsRework taskConsequence = "needs-rework"
	taskConsequenceFailed      taskConsequence = "failed"
	taskConsequenceInterrupted taskConsequence = "interrupted"
)

// errSettlementRetry reports that a settling transaction observed state
// that no longer matches its prepared notice (a send landed between the
// obligation snapshot and the transaction, or the predicted consequence
// changed); the caller rolls back, prepares a fresh notice and retries.
var errSettlementRetry = errors.New("app: settlement preparation is stale; retry")

// settlementNoticeRetries bounds the prepare/settle retry loop; each
// round's mismatch means a concurrent send moved the mailbox, which
// cannot repeat unboundedly under a stopping or failing task's traffic.
const settlementNoticeRetries = 5

// defaultRetryLimit mirrors the [retry] max_attempts default of 3.
const defaultRetryLimit = 3

// retryLimitFor reads the frozen retry limit, defaulting when unset.
func retryLimitFor(snapshot *RunSnapshot) int {
	if snapshot.Workflow.RetryLimit > 0 {
		return snapshot.Workflow.RetryLimit
	}
	return defaultRetryLimit
}

// decideTaskConsequence applies the section 5 rule: interrupted under a
// stop, needs-rework with budget room, failed at the exhausted limit.
func decideTaskConsequence(stopRequested bool, attemptCount, retryLimit int) taskConsequence {
	switch {
	case stopRequested:
		return taskConsequenceInterrupted
	case attemptCount < retryLimit:
		return taskConsequenceNeedsRework
	default:
		return taskConsequenceFailed
	}
}

// controllerNotice is one prepared (file-first) controller info message
// to the manager: the body written durably before the transaction that
// commits its envelope.
type controllerNotice struct {
	ID     identity.MessageID
	Path   string
	Digest string
	Bytes  int64
}

// prepareControllerNotice writes one controller notice body under the
// run's message directory — outside any transaction, per the file-first
// rule — and returns the envelope pieces.
func (c *Controller) prepareControllerNotice(ctx context.Context, handle RunHandle, stateRoot, body string) (controllerNotice, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per notice.
	id, err := identity.ParseMessageID(c.IDs.NewID())
	if err != nil {
		return controllerNotice{}, fmt.Errorf("app: generate notice message id: %w", err)
	}
	path := filepath.Join(stateRoot, "runs", handle.runID.String(), "messages", id.String())
	if err := c.Artifacts.WriteArtifact(ctx, path, []byte(body)); err != nil {
		return controllerNotice{}, fmt.Errorf("app: write controller notice body: %w", err)
	}
	return controllerNotice{ID: id, Path: path, Digest: sha256Hex(body), Bytes: int64(len(body))}, nil
}

// commitControllerNotice commits a prepared notice's envelope inside the
// caller's transaction: a controller info message to the manager,
// committed with the transition it reports — one commit, no separate
// notification step to lose.
func commitControllerNotice(ctx context.Context, wf WorkflowRepositories, runID identity.RunID, notice controllerNotice, now time.Time) error {
	_, err := wf.Messages().Create(ctx, run.NewInfo(notice.ID, runID, run.ControllerPrincipal(), run.ManagerAddress(), "", notice.Path, notice.Digest, notice.Bytes, 0, now))
	return err
}

// pendingTaskObligations reads the queued and delivered-unacknowledged
// message IDs addressed to one task, sorted — the mailbox-closure
// snapshot-equality contract's ID set (section 5). It reads ONLY
// through the caller's transaction-owned repositories: both the notice
// snapshot and the settlement's final equality check must see the
// transaction's own view, never a separate lease-free read port.
func pendingTaskObligations(ctx context.Context, wf WorkflowRepositories, runID identity.RunID, taskID identity.TaskID) ([]string, error) {
	ids, err := wf.Messages().PendingByAddress(ctx, runID, run.TaskAddress(taskID))
	if err != nil {
		return nil, err
	}
	pending := make([]string, 0, len(ids))
	for _, id := range ids {
		pending = append(pending, id.String())
	}
	slices.Sort(pending)
	return pending, nil
}

// integrationSettlement bundles one terminal integration settlement's
// inputs: the operation whose outcome settles it, the integration's
// target state, and the reason and evidence the manager notice carries.
type integrationSettlement struct {
	OpID        identity.OperationID
	OpState     OperationState
	OpOutcome   any
	TargetState run.IntegrationState
	Reason      string
	Evidence    []string
}

// settleIntegrationTerminal settles a conflicted or rolled-back
// integration: operation outcome, integration transition, the section 5
// task consequence (needs-rework / failed / interrupted, recomputed
// INSIDE the transaction so stop precedence is never stale), mailbox
// closure with the orphaned-obligations snapshot-equality contract when
// the task fails, and the controller notice to the manager — one
// committed transaction per attempt, retried with a fresh notice when a
// concurrent send moves the mailbox between preparation and settlement.
func (c *Controller) settleIntegrationTerminal(ctx context.Context, handle RunHandle, frozen *FrozenRun, integrationID identity.IntegrationID, settlement integrationSettlement) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; called once per settled integration.
	for round := 0; round < settlementNoticeRetries; round++ {
		prediction, obligations, taskSeq, err := c.predictIntegrationConsequence(ctx, handle, frozen, integrationID)
		if err != nil {
			return err
		}
		body := renderIntegrationNotice(integrationID, settlement.TargetState, prediction, taskSeq, settlement.Reason, settlement.Evidence, obligations)
		notice, err := c.prepareControllerNotice(ctx, handle, frozen.Snapshot.StateRoot, body)
		if err != nil {
			return err
		}
		err = c.applyIntegrationSettlement(ctx, handle, frozen, integrationID, &settlement, prediction, obligations, notice)
		if errors.Is(err, errSettlementRetry) {
			continue
		}
		return err
	}
	return fmt.Errorf("app: integration %s settlement kept racing concurrent sends after %d attempts", integrationID, settlementNoticeRetries)
}

// predictIntegrationConsequence reads the state the settlement's notice
// is composed from: the task consequence as currently decidable, and —
// when the task would fail — the pending-obligation snapshot. The frozen
// snapshot arrives from the caller: an immutable value already loaded
// outside any transaction, never re-read through a lease-free port
// inside one.
func (c *Controller) predictIntegrationConsequence(ctx context.Context, handle RunHandle, frozen *FrozenRun, integrationID identity.IntegrationID) (taskConsequence, []string, int, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	var (
		prediction  taskConsequence
		obligations []string
		taskSeq     int
	)
	err := c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "predict integration consequence")
		if wfErr != nil {
			return wfErr
		}
		integ, _, getErr := wf.Integrations().Get(ctx, integrationID)
		if getErr != nil {
			return getErr
		}
		r, _, runErr := uow.Runs().Get(ctx, handle.runID)
		if runErr != nil {
			return runErr
		}
		task, _, taskErr := uow.Tasks().Get(ctx, integ.TaskID)
		if taskErr != nil {
			return taskErr
		}
		taskSeq = task.Seq
		attempts, attErr := wf.AttemptIndex().ByTask(ctx, integ.TaskID)
		if attErr != nil {
			return attErr
		}
		prediction = decideTaskConsequence(r.StopRequested, len(attempts), retryLimitFor(&frozen.Snapshot))
		if prediction == taskConsequenceFailed {
			var oblErr error
			obligations, oblErr = pendingTaskObligations(ctx, wf, handle.runID, integ.TaskID)
			return oblErr
		}
		return nil
	})
	return prediction, obligations, taskSeq, err
}

// renderIntegrationNotice renders the manager notice body for one
// terminal integration settlement. The orphaned-obligation IDs appear
// only on a failing settlement, made true by the snapshot-equality
// contract.
func renderIntegrationNotice(integrationID identity.IntegrationID, target run.IntegrationState, consequence taskConsequence, taskSeq int, reason string, evidence, obligations []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "integration %s %s\n", integrationID, target)
	fmt.Fprintf(&b, "task t%d %s\n", taskSeq, consequence)
	fmt.Fprintf(&b, "reason: %s\n", reason)
	for _, path := range evidence {
		fmt.Fprintf(&b, "evidence: %s\n", path)
	}
	if consequence == taskConsequenceFailed {
		if len(obligations) == 0 {
			b.WriteString("orphaned obligations: none\n")
		} else {
			fmt.Fprintf(&b, "orphaned obligations: %s\n", strings.Join(obligations, " "))
		}
	}
	return b.String()
}

// applyIntegrationSettlement is one settlement transaction attempt.
func (c *Controller) applyIntegrationSettlement(ctx context.Context, handle RunHandle, frozen *FrozenRun, integrationID identity.IntegrationID, settlement *integrationSettlement, prediction taskConsequence, obligations []string, notice controllerNotice) error { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design.
	now := c.Clock.Now()
	return c.withUnitOfWork(ctx, handle.lease, func(uow UnitOfWork) error {
		wf, wfErr := RequireWorkflowRepositories(uow, "integration settlement")
		if wfErr != nil {
			return wfErr
		}
		if settlement.OpID != "" {
			op, getErr := uow.Operations().Get(ctx, settlement.OpID)
			if getErr != nil {
				return getErr
			}
			if op.State == OperationPending || op.State == OperationReconciling {
				op.State = settlement.OpState
				op.Outcome = settlement.OpOutcome
				op.UpdatedAt = now
				if saveErr := uow.Operations().Save(ctx, op); saveErr != nil {
					return saveErr
				}
			}
		}

		integ, rev, getErr := wf.Integrations().Get(ctx, integrationID)
		if getErr != nil {
			return getErr
		}
		if integ.State != settlement.TargetState {
			var (
				next  run.Integration
				trErr error
			)
			switch settlement.TargetState {
			case run.IntegrationConflicted:
				next, trErr = integ.Conflict(now)
			case run.IntegrationRolledBack:
				next, trErr = integ.RollBack(now)
			case run.IntegrationInterrupted:
				next, trErr = integ.Interrupt(now)
			default:
				return fmt.Errorf("app: integration settlement target %s is not terminal", settlement.TargetState)
			}
			if trErr != nil {
				return trErr
			}
			if _, saveErr := wf.Integrations().Save(ctx, next, rev); saveErr != nil {
				return saveErr
			}
		}

		r, _, runErr := uow.Runs().Get(ctx, handle.runID)
		if runErr != nil {
			return runErr
		}
		task, taskRev, taskErr := uow.Tasks().Get(ctx, integ.TaskID)
		if taskErr != nil {
			return taskErr
		}
		attempts, attErr := wf.AttemptIndex().ByTask(ctx, integ.TaskID)
		if attErr != nil {
			return attErr
		}
		consequence := decideTaskConsequence(r.StopRequested, len(attempts), retryLimitFor(&frozen.Snapshot))
		if consequence != prediction {
			return errSettlementRetry
		}

		taskFrom := task.State
		if task.State == run.TaskIntegrating {
			var (
				next  run.Task
				trErr error
			)
			switch consequence {
			case taskConsequenceNeedsRework:
				next, trErr = task.NeedsRework(now)
			case taskConsequenceInterrupted:
				next, trErr = task.Interrupt(now)
			case taskConsequenceFailed:
				next, trErr = task.Fail(now)
			}
			if trErr != nil {
				return trErr
			}
			if consequence == taskConsequenceFailed {
				// Failure closes admission in the same commit, under the
				// snapshot-equality contract: the committed notice's
				// at-closure ID set must be exactly what is pending now.
				current, oblErr := pendingTaskObligations(ctx, wf, handle.runID, integ.TaskID)
				if oblErr != nil {
					return oblErr
				}
				if !slices.Equal(current, obligations) {
					return errSettlementRetry
				}
				next = next.CloseMailbox(now)
			}
			if _, saveErr := uow.Tasks().Save(ctx, next, taskRev); saveErr != nil {
				return saveErr
			}
			if err := recordTransition(ctx, uow, EntityTask, task.ID.String(), string(taskFrom), string(next.State), settlement.Reason, gen(handle.lease.Generation), now); err != nil {
				return err
			}
		}

		return commitControllerNotice(ctx, wf, handle.runID, notice, now)
	})
}
