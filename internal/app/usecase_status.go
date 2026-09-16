package app

import (
	"context"
	"fmt"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// StatusRequest is hop status's input. An empty RunID lists every run of
// the repository; a non-empty one renders that run's full detail block.
type StatusRequest struct {
	RepositoryRoot string
	RunID          string
}

// RunSummaryView is one rendered line of `hop status` without -run.
type RunSummaryView struct {
	RunID    string
	Sequence int
	State    string
	// StopRequested mirrors the run's monotonic stop flag, so a foreground
	// controller can route to stop handling when hop stop was requested
	// elsewhere.
	StopRequested bool
	Reconciling   bool
	UpdatedAt     time.Time
	// NeedsAttention is section 7's "blocked, needs attention" condition:
	// true when at least one of the run's mailboxes is Attention. Populated
	// only by the `-run` detail render (runDetailView derives it from
	// Mailboxes); the bare listing (Status with no RunID) has no per-
	// address message data to compute it from and always leaves it false,
	// like every other field the underlying store has not populated yet.
	NeedsAttention bool
}

// TaskSummaryView is one row of RunDetailView's feature-mode task table.
type TaskSummaryView struct {
	TaskID       string
	Seq          int
	Kind         string
	State        string
	DependsOn    []string
	AttemptCount int
	WorktreePath string
}

// WorktreeView is one feature-mode worktree row as hop status -run renders
// it, from the row and its journaled retirement
// (docs/plan/phase-3-worktree-retirement.md section 6).
type WorktreeView struct {
	// Branch and Path are the row's recorded branch name and path.
	Branch string
	Path   string
	// State is the row's state: active, removed, absent or released.
	State string
	// Released is a released row's recorded reason.
	Released WorktreeReleaseReason
	// Retained is the category an active row's latest removal was refused
	// for, and EvidencePath that refusal's retained stderr.
	Retained     WorktreeRetainedCategory
	EvidencePath string
	// Removal names an active row's unfinished removal: "incomplete" or
	// "interrupted" for a settled act that did not finish, "unresolved" for
	// an act whose outcome is not recorded yet.
	Removal string
}

// IntegrationView is the run's most recently created integration, if any.
type IntegrationView struct {
	ID              string
	TaskID          string
	SourceCommitOID string
	PremergeHeadOID string
	MergeCommitOID  string
	State           string
}

// GuardShortfallView is one unmet completion guard, rendered verbatim from
// EvaluateReadiness's missing list. TaskID is "" except for
// "task-not-integrated".
type GuardShortfallView struct {
	Kind   string
	TaskID string
}

// MailboxView is one recipient address's queue condition: section 7's
// attention status surface. InFlightMessageID is "" when nothing is
// currently delivered-unacknowledged.
type MailboxView struct {
	Address           string // AddressString's canonical form: "manager", "human" or "task:<uuid>"
	InFlightMessageID string
	InFlightAge       time.Duration
	QueuedCount       int
	OldestQueuedAge   time.Duration
	AddressLive       bool
	Attention         bool
}

// PendingQuestionView is one unanswered human-addressed question.
type PendingQuestionView struct {
	MessageID string
	BodyPath  string
	Age       time.Duration
}

// WorktreeOperationView is one unresolved per-attempt worktree.create
// operation of a feature run and the human's action for it. Branch is ""
// when the intent is unreadable. Action is fixed text: never a path or a
// raw cause, which stay in the operation journal.
type WorktreeOperationView struct {
	OperationID string
	Branch      string
	State       string
	Action      string
}

// RunDetailView is the full detail block `hop status -run` renders. Every
// field the underlying store may not have populated yet renders as its
// zero value ("" or 0), never a guess.
type RunDetailView struct {
	RunSummaryView
	// Mode mirrors RunDetail.Mode: "feature", or "" for a solo run.
	Mode string
	// TargetBranch and WorktreesRetiredAt mirror RunDetail's
	// worktree-retirement target and fact ("" and nil when absent).
	TargetBranch       string
	WorktreesRetiredAt *time.Time
	// Worktrees is one line per feature-mode worktree row, oldest first;
	// nil for a solo run.
	Worktrees      []WorktreeView
	TaskState      string
	AttemptState   string
	WorktreePath   string
	BindingSummary string // "workspace/tab/pane"; "" when no current binding
	ClaimState     string // "" when no launch claim exists yet
	// SeedEvidence is the claim's recorded workspace-trust pre-seeding
	// outcome; "" when no launch claim exists yet.
	SeedEvidence   string
	PendingOps     int
	LastSubmission string // the last submission outcome's kind; "" when none
	Artifacts      []string
	// LastCheckOperation names the newest check execution; "" when none ran.
	LastCheckOperation string
	LastCheckState     string
	LastCheckUnknown   bool
	LastCheckDetail    string
	LastCheckEvidence  []string
	// LastCheckOptions spells the human's options for an unknown outcome
	// (docs/plan/phase-2-design.md sections 7-8): rerun after inspection is
	// a human decision, never an automatic one.
	LastCheckOptions string

	// Tasks is the feature-mode task table (section 3); nil for a solo run.
	Tasks []TaskSummaryView
	// LatestIntegration is the run's most recently created integration, if
	// any; nil for a solo run or a feature run with no integration
	// attempted yet.
	LatestIntegration *IntegrationView
	// GuardShortfalls is EvaluateReadiness's missing list, rendered
	// verbatim; always empty for a solo run.
	GuardShortfalls []GuardShortfallView
	// Mailboxes is section 7's per-address queue-depth/in-flight-age
	// status surface: one entry per address with a non-empty queue or an
	// unacknowledged in-flight message.
	Mailboxes []MailboxView
	// PendingQuestions is every unanswered human-addressed question,
	// oldest first.
	PendingQuestions []PendingQuestionView
	// WorktreeOperations is every unresolved per-attempt worktree.create
	// operation, oldest first, with its human action; nil for a solo run.
	WorktreeOperations []WorktreeOperationView
}

// StatusResult is Status's success value: exactly one of Runs (the -run-less
// listing) or Detail (a single run's detail) is populated.
type StatusResult struct {
	Runs   []RunSummaryView
	Detail *RunDetailView
}

// Status renders hop status's output from ReadStore, without a lease.
func (c *Controller) Status(ctx context.Context, req StatusRequest) (StatusResult, error) {
	if req.RunID == "" {
		statuses, err := c.Read.ListRuns(ctx, req.RepositoryRoot)
		if err != nil {
			return StatusResult{}, fmt.Errorf("app: list runs: %w", err)
		}
		views := make([]RunSummaryView, len(statuses))
		for i, s := range statuses {
			views[i] = runSummaryView(s)
		}
		return StatusResult{Runs: views}, nil
	}

	runID, err := identity.ParseRunID(req.RunID)
	if err != nil {
		return StatusResult{}, fmt.Errorf("app: parse run id: %w", err)
	}
	detail, err := c.Read.LoadRunStatus(ctx, runID)
	if err != nil {
		return StatusResult{}, fmt.Errorf("app: load run status: %w", err)
	}
	view := runDetailView(detail)
	return StatusResult{Detail: &view}, nil
}

func runSummaryView(s RunStatus) RunSummaryView {
	return RunSummaryView{
		RunID: s.RunID.String(), Sequence: s.Sequence, State: string(s.State),
		StopRequested: s.StopRequested, Reconciling: s.Reconciling, UpdatedAt: s.UpdatedAt,
	}
}

func runDetailView(d RunDetail) RunDetailView { //nolint:gocritic // hugeParam: RunDetail is a ReadStore DTO rendered at most once per hop status -run call.
	view := RunDetailView{
		RunSummaryView:     runSummaryView(d.RunStatus),
		Mode:               d.Mode,
		TargetBranch:       d.TargetBranch,
		WorktreesRetiredAt: d.WorktreesRetiredAt,
		TaskState:          string(d.TaskState),
		AttemptState:       string(d.AttemptState),
		WorktreePath:       d.WorktreePath,
		PendingOps:         len(d.PendingOperations),
		Worktrees:          worktreeViews(d.Worktrees, d.WorktreeRetirements),
	}
	if d.Binding != nil {
		view.BindingSummary = fmt.Sprintf("%s/%s/%s", d.Binding.WorkspaceID, d.Binding.TabID, d.Binding.PaneID)
	}
	if d.Claim != nil {
		view.ClaimState = string(d.Claim.State)
		view.SeedEvidence = d.Claim.SeedEvidence
	}
	if d.LastSubmission != nil {
		view.LastSubmission = string(d.LastSubmission.Kind)
	}
	if d.LastCheck != nil {
		view.LastCheckOperation = d.LastCheck.OperationID.String()
		view.LastCheckState = string(d.LastCheck.State)
		view.LastCheckUnknown = d.LastCheck.Unknown
		view.LastCheckDetail = d.LastCheck.Detail
		view.LastCheckEvidence = d.LastCheck.EvidencePaths
		if d.LastCheck.Unknown {
			view.LastCheckOptions = "inspect the retained evidence at the listed paths; automated retry is not available for this outcome — after inspection, start a new run for further work, or use hop stop if this run is still active"
		}
	}
	for _, a := range d.Artifacts {
		view.Artifacts = append(view.Artifacts, a.Path)
	}
	for _, t := range d.Tasks {
		dependsOn := make([]string, len(t.DependsOn))
		for i, dep := range t.DependsOn {
			dependsOn[i] = dep.String()
		}
		view.Tasks = append(view.Tasks, TaskSummaryView{
			TaskID: t.TaskID.String(), Seq: t.Seq, Kind: string(t.Kind), State: string(t.State),
			DependsOn: dependsOn, AttemptCount: t.AttemptCount, WorktreePath: t.WorktreePath,
		})
	}
	if d.LatestIntegration != nil {
		view.LatestIntegration = &IntegrationView{
			ID: d.LatestIntegration.ID.String(), TaskID: d.LatestIntegration.TaskID.String(),
			SourceCommitOID: d.LatestIntegration.SourceCommitOID, PremergeHeadOID: d.LatestIntegration.PremergeHeadOID,
			MergeCommitOID: d.LatestIntegration.MergeCommitOID, State: string(d.LatestIntegration.State),
		}
	}
	for _, s := range d.GuardShortfalls {
		gv := GuardShortfallView{Kind: string(s.Kind)}
		if s.Kind == run.ShortfallTaskNotIntegrated {
			gv.TaskID = s.TaskID.String()
		}
		view.GuardShortfalls = append(view.GuardShortfalls, gv)
	}
	for _, m := range d.Mailboxes {
		mv := MailboxView{
			Address: AddressString(m.Address), QueuedCount: m.QueuedCount, OldestQueuedAge: m.OldestQueuedAge,
			AddressLive: m.AddressLive, Attention: m.Attention,
		}
		if m.InFlight != nil {
			mv.InFlightMessageID = m.InFlight.MessageID.String()
			mv.InFlightAge = m.InFlight.Age
		}
		view.Mailboxes = append(view.Mailboxes, mv)
		if m.Attention {
			view.NeedsAttention = true
		}
	}
	for _, q := range d.PendingQuestions {
		view.PendingQuestions = append(view.PendingQuestions, PendingQuestionView{
			MessageID: q.MessageID.String(), BodyPath: q.BodyPath, Age: q.Age,
		})
	}
	if d.Mode == WorkflowModeFeature {
		for i := range d.PendingOperations {
			op := &d.PendingOperations[i]
			if op.Kind != OpWorktreeCreate {
				continue
			}
			intent, _ := decodeOperationPayload[attemptWorktreeCreateIntent](op.Intent)
			view.WorktreeOperations = append(view.WorktreeOperations, WorktreeOperationView{
				OperationID: op.ID.String(), Branch: intent.Branch, State: string(op.State),
				Action: worktreeOperationAction(op),
			})
		}
	}
	return view
}
