package run

import "github.com/johnlanda/hop/internal/domain/identity"

// ShortfallKind names one reason EvaluateReadiness is not satisfied.
type ShortfallKind string

// Shortfall kinds.
const (
	// ShortfallPlanOpen: the manager has not closed its plan.
	ShortfallPlanOpen ShortfallKind = "plan-open"
	// ShortfallTaskNotIntegrated: an implement task has not reached
	// integrated. GuardShortfall.TaskID names it.
	ShortfallTaskNotIntegrated ShortfallKind = "task-not-integrated"
	// ShortfallCheckMissing: no passing check receipt exists for the
	// current integration head.
	ShortfallCheckMissing ShortfallKind = "check-missing"
	// ShortfallVerdictMissing: no review verdict has been accepted at
	// all.
	ShortfallVerdictMissing ShortfallKind = "verdict-missing"
	// ShortfallVerdictRejected: the latest accepted verdict is a reject.
	ShortfallVerdictRejected ShortfallKind = "verdict-rejected"
	// ShortfallVerdictStaleSubject: the latest accepted verdict's subject
	// does not equal the current integration head — an approve bound to a
	// superseded head is harmless by construction, since staleness is
	// computed here, never stored.
	ShortfallVerdictStaleSubject ShortfallKind = "verdict-stale-subject"
)

// GuardShortfall is one reason EvaluateReadiness is not satisfied, in the
// shape RunDetail renders verbatim in status output.
type GuardShortfall struct {
	Kind   ShortfallKind
	TaskID identity.TaskID
}

// GuardContext is the complete, application-assembled evidence
// EvaluateReadiness needs: the plan flag, every implement task in the run,
// the current integration head's object IDs, whether a passing check
// receipt exists for exactly that head, and the most recently accepted
// review verdict, if any (whatever its subject or value — EvaluateReadiness
// itself compares it against the head, rather than trusting a
// pre-filtered match).
type GuardContext struct {
	PlanClosed      bool
	ImplementTasks  []Task
	HeadCommitOID   string
	HeadTreeOID     string
	HeadCheckPassed bool
	LatestReview    *Review
}

// EvaluateReadiness is the run completion guard (section 8), the ONLY path
// to Run.Complete: it takes only evidence rows — never a prose flag — and
// is a pure function over that evidence. Guards, in order: (0) the plan is
// closed; (1) every implement task is integrated; (2) a passing check
// receipt exists for the current integration head; (3) an approve verdict
// row whose subject commit and tree object IDs equal that same head. ready
// is true only when missing is empty; missing lists every unsatisfied
// guard, not just the first.
func EvaluateReadiness(ctx GuardContext) (ready bool, missing []GuardShortfall) { //nolint:gocritic // hugeParam: GuardContext is an application-assembled value passed by value throughout this package, mirroring AcceptanceContext.
	if !ctx.PlanClosed {
		missing = append(missing, GuardShortfall{Kind: ShortfallPlanOpen})
	}
	for i := range ctx.ImplementTasks {
		if ctx.ImplementTasks[i].State != TaskIntegrated {
			missing = append(missing, GuardShortfall{Kind: ShortfallTaskNotIntegrated, TaskID: ctx.ImplementTasks[i].ID})
		}
	}
	if !ctx.HeadCheckPassed {
		missing = append(missing, GuardShortfall{Kind: ShortfallCheckMissing})
	}
	switch {
	case ctx.LatestReview == nil:
		missing = append(missing, GuardShortfall{Kind: ShortfallVerdictMissing})
	case ctx.LatestReview.Verdict != VerdictApprove:
		missing = append(missing, GuardShortfall{Kind: ShortfallVerdictRejected})
	case ctx.LatestReview.SubjectCommitOID != ctx.HeadCommitOID || ctx.LatestReview.SubjectTreeOID != ctx.HeadTreeOID:
		missing = append(missing, GuardShortfall{Kind: ShortfallVerdictStaleSubject})
	}

	return len(missing) == 0, missing
}
