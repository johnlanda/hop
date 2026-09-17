package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/johnlanda/hop/internal/domain/identity"
)

// This file renders the Phase 3 assignment, role and protocol artifacts
// (docs/plan/phase-3-design.md section 6): the manager's brief
// assignment, the worker-protocol crib, the implementer's per-attempt
// task assignment (manager-authored instructions plus acceptance
// criteria, the frozen implementer role reference, and prior-attempt
// feedback paths on retry) and the reviewer's review assignment (the
// frozen subject, the diff scope, the reviewer role reference and the
// verdict submission instruction). Every template is pure and
// deterministic, is built from frozen run facts only — absolute paths,
// identities, digests and object IDs, never environment values — and
// quotes its protocol lines from the grammar constant set (grammar.go),
// so template, CLI and fixture can never drift apart silently
// (TestTemplatesQuoteGrammar pins the quotations).

// IntegrationBranchName is the run's integration branch,
// "hop/r<seq>/integration" (design section 6): one ref-namespace scheme,
// computed once from the run's sequence at freeze and stored in the
// WorkflowSnapshot — never a configuration key.
func IntegrationBranchName(runSeq int) string {
	return fmt.Sprintf("hop/r%d/integration", runSeq)
}

// roleArtifactPath is the deterministic path of one frozen role-artifact
// copy under the run's artifact directory.
func roleArtifactPath(stateRoot string, runID identity.RunID, role string) string {
	return filepath.Join(stateRoot, "runs", runID.String(), "artifacts", "roles", role+".md")
}

// reviewReasonsPath is the deterministic path of one accepted review's
// reasons artifact — exactly the path SubmitReviewVerdict writes
// (usecase_review.go), re-derived here so a retry assignment can name a
// prior review's reasons from its persisted identity alone.
func reviewReasonsPath(stateRoot string, runID identity.RunID, reviewID identity.ReviewID) string {
	return filepath.Join(stateRoot, "runs", runID.String(), "reviews", reviewID.String())
}

// checkEvidencePaths are the deterministic stdout/stderr retention paths
// of one check execution — exactly where ClaimAndRunCheck retains them
// (usecase_check.go), re-derived for retry-feedback references.
func checkEvidencePaths(stateRoot string, runID identity.RunID, opID identity.OperationID) (stdout, stderr string) {
	base := filepath.Join(stateRoot, "runs", runID.String(), "checks", opID.String())
	return filepath.Join(base, "stdout"), filepath.Join(base, "stderr")
}

// renderWorkerProtocolCrib renders the fixed worker-protocol reference
// (design section 6: "section 7's grammar, rendered as a fixed artifact
// at freeze"): every first line quoted from the grammar constants with
// placeholder identities, plus the enumerated refusal tokens. It takes
// no run fact at all — the crib is identical for every run — so freeze
// writes the same bytes every time and the golden test pins them.
func renderWorkerProtocolCrib() []byte {
	var b strings.Builder
	b.WriteString("# HOP worker protocol reference\n\n")
	b.WriteString("Every hop verb below prints a fixed FIRST LINE; parse only these\n")
	b.WriteString("lines. A refusal exits 1 with the first line\n")
	b.WriteString("`" + GrammarRefusalLine("<reason-token>") + "` and detail lines after it. The\n")
	b.WriteString("enumerated reason tokens are: " + strings.Join([]string{
		GrammarReasonNotFound, GrammarReasonUnauthorized, GrammarReasonMalformed,
		GrammarReasonConflicting, GrammarReasonStale, GrammarReasonNotDelivered,
		GrammarReasonRunNotAccepting, GrammarReasonMailboxClosed, GrammarReasonNotManager,
		GrammarReasonNotReviewer, GrammarReasonDependencyCycle, GrammarReasonEmptyPlan,
		GrammarReasonRetryNotTerminal, GrammarReasonRetryLimit, GrammarReasonSubjectMismatch,
	}, ", ") + ".\n\n")

	b.WriteString("## hop " + GrammarVerbResultSubmit + "\n\n")
	b.WriteString("First line: `" + GrammarResultAcceptedLine("<result-uuid>") + "` or `" + GrammarResultDuplicateLine("<result-uuid>") + "`.\n")
	b.WriteString("Retryable: `" + GrammarTransientNotRunningLine + "` — wait briefly, rerun the same command.\n")
	b.WriteString("Retryable: `" + GrammarTransientUndeliveredLine + "`.\n\n")

	b.WriteString("## hop " + GrammarVerbMsgNext + " / hop " + GrammarVerbMsgWait + "\n\n")
	b.WriteString("A served message prints three lines (optional fields appear only when set):\n\n")
	b.WriteString("    " + GrammarMessageLine("<message-uuid>", "<kind>", "<sender>", "<reply-to-uuid>", "<relay-of-uuid>", "<origin-uuid>") + "\n")
	b.WriteString("    " + GrammarBodyLine("<absolute body path>") + "\n")
	b.WriteString("    " + GrammarAckHintLine("<message-uuid>") + "\n\n")
	b.WriteString("`origin` names the ORIGINAL question behind a relayed answer; forward\n")
	b.WriteString("using only it. Empty queue: `" + GrammarMsgNoneLine + "` (next), or\n")
	b.WriteString("`" + GrammarMsgWaitNoneLine(50*time.Second) + "` (wait; the timeout varies —\n")
	b.WriteString("rerun the same command).\n\n")

	b.WriteString("## hop " + GrammarVerbMsgShow + " <message-uuid>\n\n")
	b.WriteString("Read-only envelope lookup:\n\n")
	b.WriteString("    " + GrammarMessageShowLine("<message-uuid>", "<kind>", "<sender>", "<address>", "<reply-to-uuid>", "<relay-of-uuid>", 0) + "\n")
	b.WriteString("    " + GrammarBodyLine("<absolute body path>") + "\n")
	b.WriteString("    " + GrammarDeliveredLine("<session-uuid>", time.Unix(0, 0)) + " (one line per delivery; sample time)\n")
	b.WriteString("    " + GrammarAcknowledgedLine(time.Unix(0, 0)) + " (when acknowledged; sample time)\n\n")
	b.WriteString("Unknown or foreign id: `" + GrammarRefusalLine(GrammarReasonNotFound) + "`.\n\n")

	b.WriteString("## hop " + GrammarVerbMsgAck + " <message-uuid>\n\n")
	b.WriteString("First line: `" + GrammarAckAcceptedLine("<message-uuid>") + "` or `" + GrammarAckDuplicateLine("<message-uuid>") + "`.\n\n")

	b.WriteString("## hop " + GrammarVerbMsgSend + "\n\n")
	b.WriteString("First line: `" + GrammarSentLine("<message-uuid>") + "` or `" + GrammarSendDuplicateLine("<message-uuid>") + "`.\n\n")

	b.WriteString("## hop " + GrammarVerbTaskCreate + " (manager only)\n\n")
	b.WriteString("First line: `" + GrammarTaskCreatedLine("<task-uuid>", 0) + "` or `" + GrammarTaskCreateDuplicateLine("<task-uuid>", 0) + "` (t0 stands for t<seq>).\n\n")

	b.WriteString("## hop " + GrammarVerbTaskRetry + " (manager only)\n\n")
	b.WriteString("First line: `" + GrammarRetryAcceptedLine(0, 0) + "` or `" + GrammarRetryDuplicateLine(0, 0) + "` (t0/0 stand for t<seq>/<n>).\n\n")

	b.WriteString("## hop " + GrammarVerbPlanClose + " (manager only)\n\n")
	b.WriteString("First line: `" + GrammarPlanClosedLine + "` or `" + GrammarPlanCloseDuplicateLine + "`.\n\n")

	b.WriteString("## hop " + GrammarVerbReviewSubmit + " (reviewer only)\n\n")
	b.WriteString("First line: `" + GrammarVerdictAcceptedLine("<review-uuid>") + "` or `" + GrammarVerdictDuplicateLine("<review-uuid>") + "`.\n")
	b.WriteString("Retryable: `" + GrammarTransientUndeliveredLine + "`.\n")
	return []byte(b.String())
}

// managerAssignmentFields are the values rendered into the manager's
// brief assignment artifact.
type managerAssignmentFields struct {
	RunID          string
	Brief          string
	AssignmentPath string // absolute; self-reference
	RolePath       string // absolute; the frozen manager role copy
	CribPath       string // absolute; the worker-protocol crib
	HOPPath        string // absolute
	// RepositoryRoot is the run's frozen repository root: the manager's
	// verdict-channel instruction renders it into the exact `hop status`
	// invocation the manager runs to learn a review verdict
	// (docs/plan/phase-3-design.md section 7's "the manager's verdict
	// channel"; STATUS-1).
	RepositoryRoot string
}

// posixShellQuote renders s as a single POSIX shell word: wrapped in
// single quotes, each embedded single quote closed, escaped and reopened
// ('\”), the standard POSIX technique and the only one that neutralizes
// every shell metacharacter (spaces, $(...), backticks, semicolons,
// newlines) with no exceptions. Used only for the verdict-channel
// instruction's concrete arguments (hop path, repository root, run id):
// a repository root or an installation path can legitimately contain a
// space, and this run's id and repository root are otherwise the only
// arguments in this file interpolated into shell syntax rather than a
// plain instruction sentence. Every other verb line's argument
// placeholders (<task-uuid>, <message-uuid>, ...) stay unquoted,
// exactly as before: those are literal placeholders a human or agent
// retypes, never a frozen run fact substituted in here, and they are
// pinned prompt goldens a later slice parses byte for byte.
func posixShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// renderManagerAssignment renders the manager's brief assignment
// artifact: the brief, the two companion artifacts by absolute path, and
// the manager's verb surface quoted from the grammar constants. Pure and
// deterministic like renderAssignment, whose solo shape it deliberately
// does not touch.
func renderManagerAssignment(f *managerAssignmentFields) []byte {
	hopPath := posixShellQuote(f.HOPPath)
	repositoryRoot := posixShellQuote(f.RepositoryRoot)
	runID := posixShellQuote(f.RunID)
	return fmt.Appendf(nil, `# HOP Manager Assignment

Run: %s

## Brief

%s

## Instructions

Read this file at its absolute path (%s) before doing anything else,
then your role instructions at %s and the worker protocol reference at
%s; together they are your complete instructions.

Plan the work as tasks, then close the plan:

    %s task create --title "<short title>" --file <instructions file> [--depends-on <task-uuid>]
    %s plan close

Poll your mailbox between actions and acknowledge what you handle:

    %s msg wait
    %s msg ack <message-uuid>

Answer a question with %s msg send --kind answer --reply-to
<question-uuid> --file <path>; relay a question to the human with
--kind question --to human --relay-of <original-uuid>; retry a
needs-rework task with %s task retry <task-uuid> --reason "<why>".
Every verb's first line is fixed by the protocol reference, and a
refusal's first line is refused: <reason-token> with detail after.

## Verdict channel

A review verdict's controller notice carries only the reviewer's reasons
text as its body; it never names the verdict itself. After any
controller info notice, run:

    %s status -C %s -run %s

and read its shortfall lines. A shortfall naming verdict-rejected means
the last review was rejected: read the reasons file the notice named,
then plan a fix task from it — integrating it produces a new head, which
gets its own new review task. A needs-rework task notice already carries
its own retry path (%s task retry <task-uuid> --reason "<why>"); this
channel is for verdicts, which commit no task-state notice of their own.
`,
		f.RunID, f.Brief, f.AssignmentPath, f.RolePath, f.CribPath,
		f.HOPPath, f.HOPPath, f.HOPPath, f.HOPPath, f.HOPPath, f.HOPPath,
		hopPath, repositoryRoot, runID, f.HOPPath)
}

// priorAttemptFeedback is the retry section of a task assignment: the
// durable evidence paths of the attempt being retried (design section 6
// — "the retry assignment artifact carries the prior attempt's result
// commit, check output and (where present) review reasons paths so the
// worker can recover useful work deliberately"). Every field is optional
// except Number; absent evidence renders as absent lines, never as empty
// placeholders.
type priorAttemptFeedback struct {
	Number            int
	ResultCommitOID   string
	CheckStdoutPath   string
	CheckStderrPath   string
	ReviewReasonsPath string
}

// taskAssignmentFields are the values rendered into an implementer
// attempt's assignment artifact.
type taskAssignmentFields struct {
	RunID, TaskID, AttemptID string
	TaskSeq, AttemptNumber   int
	Title                    string
	InstructionsPath         string // absolute; manager-authored instructions + acceptance criteria
	RolePath                 string // absolute; the frozen implementer role copy
	AssignmentPath           string // absolute; self-reference
	HOPPath                  string // absolute
	Prior                    *priorAttemptFeedback
}

// renderTaskAssignment renders one implementer attempt's assignment
// artifact per the section 6 role table: the manager-authored
// instructions and acceptance criteria by absolute path, the frozen
// implementer role reference, the submit instruction quoted from the
// grammar constants, and — on retry — the prior attempt's feedback
// paths.
func renderTaskAssignment(f *taskAssignmentFields) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, `# HOP Task Assignment

Run: %s
Task: %s (t%d)
Attempt: %s (attempt %d)
Title: %s

## Instructions

Read this file at its absolute path (%s) before doing anything else.
Your manager-authored instructions and acceptance criteria are at:

    %s

Your role instructions are at:

    %s

Work in this directory (your attempt's worktree) and commit your work
here. When your work is complete and committed, submit it by running:

    %s result submit --summary "<one-line summary>" --commit <commit-oid>

If that command prints a first line beginning with "transient", follow
its instruction and run the exact same command again. Any other refusal
prints refused: <reason-token> as its first line; read the detail lines
before retrying.
`,
		f.RunID, f.TaskID, f.TaskSeq, f.AttemptID, f.AttemptNumber, f.Title,
		f.AssignmentPath, f.InstructionsPath, f.RolePath, f.HOPPath)
	if f.Prior != nil {
		fmt.Fprintf(&b, "\n## Prior attempt %d (retry feedback)\n\n", f.Prior.Number)
		if f.Prior.ResultCommitOID != "" {
			fmt.Fprintf(&b, "Accepted result commit: %s\n", f.Prior.ResultCommitOID)
		}
		if f.Prior.CheckStdoutPath != "" {
			fmt.Fprintf(&b, "Check stdout: %s\n", f.Prior.CheckStdoutPath)
		}
		if f.Prior.CheckStderrPath != "" {
			fmt.Fprintf(&b, "Check stderr: %s\n", f.Prior.CheckStderrPath)
		}
		if f.Prior.ReviewReasonsPath != "" {
			fmt.Fprintf(&b, "Review reasons: %s\n", f.Prior.ReviewReasonsPath)
		}
		b.WriteString("\nRecover useful work from these deliberately; your worktree is a\nfresh branch from the current integration head.\n")
	}
	return []byte(b.String())
}

// reviewAssignmentFields are the values rendered into a reviewer
// attempt's assignment artifact.
type reviewAssignmentFields struct {
	RunID, TaskID, AttemptID string
	TaskSeq, AttemptNumber   int
	SubjectCommitOID         string
	SubjectTreeOID           string
	DiffBaseOID              string // the run's integration base: the first integration's recorded pre-merge head
	RolePath                 string // absolute; the frozen reviewer role copy
	AssignmentPath           string // absolute; self-reference
	HOPPath                  string // absolute
}

// renderReviewAssignment renders one reviewer attempt's assignment
// artifact per the section 6 role table: the frozen subject (commit and
// tree object IDs), the diff scope base..head, the reviewer role
// reference and the verdict submission instruction with the frozen
// subject embedded — the guard compares object IDs, so reviewing any
// other candidate is refused.
func renderReviewAssignment(f *reviewAssignmentFields) []byte {
	return fmt.Appendf(nil, `# HOP Review Assignment

Run: %s
Task: %s (t%d)
Attempt: %s (attempt %d)

## Subject (frozen)

Commit: %s
Tree: %s
Diff scope: %s..%s

Your worktree is checked out at the subject commit. Review exactly this
candidate: the completion guard compares object IDs, and a verdict for
any other commit is refused with refused: %s.

## Instructions

Read this file at its absolute path (%s) before doing anything else.
Your role instructions are at:

    %s

Write your reasons to a file, then submit your verdict by running:

    %s review submit --verdict <approve|reject> --subject %s --reasons-file <absolute path>

If that command prints a first line beginning with "transient", follow
its instruction and run the exact same command again.
`,
		f.RunID, f.TaskID, f.TaskSeq, f.AttemptID, f.AttemptNumber,
		f.SubjectCommitOID, f.SubjectTreeOID, f.DiffBaseOID, f.SubjectCommitOID,
		GrammarReasonSubjectMismatch,
		f.AssignmentPath, f.RolePath, f.HOPPath, f.SubjectCommitOID)
}

// WorkflowFreezeRequest is FreezeWorkflowArtifacts's input: the loaded
// policy (defaults applied and feature-validated by the caller through
// the shared helpers), the run identity and sequence, and the frozen
// state root.
type WorkflowFreezeRequest struct {
	Policy    RunPolicy
	RunID     identity.RunID
	RunSeq    int
	StateRoot string
}

// FreezeWorkflowArtifacts performs the section 3/6 role-artifact freeze
// for a feature-mode run and assembles its WorkflowSnapshot: the three
// role instruction files are read NOW from the repository paths the
// policy resolved at load, copied byte-for-byte into the run's artifact
// directory with digests — the copies are what every launch references,
// so a mid-run edit of the repository files changes nothing — and the
// worker-protocol crib is rendered from the grammar constants and
// written beside them. The integration branch name derives from the run
// sequence here, never from configuration. Reads and writes go through
// the ArtifactStore port (never inside any store transaction); the
// caller (hop run's feature freeze, slice 6) commits the returned
// snapshot with InitializeRun and creates the integration branch
// separately.
func (c *Controller) FreezeWorkflowArtifacts(ctx context.Context, req *WorkflowFreezeRequest) (WorkflowSnapshot, error) {
	if err := ValidateFeaturePolicy(&req.Policy); err != nil {
		return WorkflowSnapshot{}, err
	}
	if !filepath.IsAbs(req.StateRoot) {
		return WorkflowSnapshot{}, fmt.Errorf("app: the state root is not absolute; frozen artifacts carry only absolute paths")
	}
	snapshot := WorkflowSnapshot{
		Mode:              WorkflowModeFeature,
		MaxWorkers:        req.Policy.MaxWorkers,
		RetryLimit:        req.Policy.RetryLimit,
		ReviewerHarness:   req.Policy.ReviewerHarness,
		MessageAttention:  req.Policy.MessageAttention,
		MessageWait:       req.Policy.MessageWait,
		IntegrationBranch: IntegrationBranchName(req.RunSeq),
	}
	roles := []struct {
		name   string
		source string
		path   *string
		digest *string
	}{
		{"manager", req.Policy.ManagerRole, &snapshot.ManagerRolePath, &snapshot.ManagerRoleDigest},
		{"implementer", req.Policy.ImplementerRole, &snapshot.ImplementerRolePath, &snapshot.ImplementerRoleDigest},
		{"reviewer", req.Policy.ReviewerRole, &snapshot.ReviewerRolePath, &snapshot.ReviewerRoleDigest},
	}
	for _, role := range roles {
		content, err := c.Artifacts.ReadArtifact(ctx, role.source)
		if err != nil {
			return WorkflowSnapshot{}, fmt.Errorf("app: read the %s role instructions: %w", role.name, err)
		}
		if len(content) == 0 {
			return WorkflowSnapshot{}, fmt.Errorf("app: the %s role instructions file is empty; an empty role artifact cannot instruct a session", role.name)
		}
		frozenPath := roleArtifactPath(req.StateRoot, req.RunID, role.name)
		if err := c.Artifacts.WriteArtifact(ctx, frozenPath, content); err != nil {
			return WorkflowSnapshot{}, fmt.Errorf("app: freeze the %s role artifact: %w", role.name, err)
		}
		*role.path = frozenPath
		*role.digest = sha256Hex(content)
	}
	if err := c.Artifacts.WriteArtifact(ctx, workerProtocolCribPath(req.StateRoot, req.RunID), renderWorkerProtocolCrib()); err != nil {
		return WorkflowSnapshot{}, fmt.Errorf("app: write the worker protocol crib: %w", err)
	}
	return snapshot, nil
}
