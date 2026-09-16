package app

import (
	"strings"

	"github.com/johnlanda/hop/internal/domain/run"
)

// Candidate provenance (docs/plan/phase-3-worktree-retirement.md section
// 4, "Candidate set"): a worktree row is a retirement candidate only when
// its attempt's own succeeded worktree.create operation vouches for every
// recorded fact of the row. HOP never acts on a checkout it cannot tie to
// its own record.

// attemptBranchRefPrefix qualifies a recorded attempt branch: Herdr's
// worktree.create reports the requested short name (spike S9), git's
// `-b <name>` creates it under refs/heads/, and `worktree list` names the
// full ref.
const attemptBranchRefPrefix = "refs/heads/"

// verifyRetirementCandidate ties row to its attempt's succeeded
// worktree.create operation among creates (the run's worktree.create
// operations). Exactly one succeeded operation must carry the attempt's
// intent, and its intent and recorded outcome must agree with the row's
// attempt, branch, base commit and path, with the intent's repository root
// equal to the run's frozen root. On success it returns the candidate the
// pre-act inspection examines; otherwise ok is false and detail names the
// disagreement without echoing a path, and the row is released as
// unverified.
func verifyRetirementCandidate(row *run.Worktree, creates []Operation, repositoryRoot string) (candidate attemptCheckout, detail string, ok bool) {
	switch {
	case row.AttemptID == "":
		return attemptCheckout{}, "the worktree row names no attempt", false
	case row.BaseCommit == "":
		return attemptCheckout{}, "the worktree row names no base commit", false
	case row.Branch == "" || strings.HasPrefix(row.Branch, "refs/"):
		return attemptCheckout{}, "the worktree row's branch is not an attempt branch name", false
	case row.Path == "":
		return attemptCheckout{}, "the worktree row names no path", false
	}
	var (
		matched  *Operation
		matching int
	)
	for i := range creates {
		op := &creates[i]
		if op.Kind != OpWorktreeCreate || op.RunID != row.RunID || op.State != OperationSucceeded {
			continue
		}
		intent, decoded := decodeOperationPayload[attemptWorktreeCreateIntent](op.Intent)
		if !decoded || intent.AttemptID != row.AttemptID {
			continue
		}
		matched = op
		matching++
	}
	switch {
	case matching == 0:
		return attemptCheckout{}, "no succeeded worktree.create operation records the attempt's worktree", false
	case matching > 1:
		return attemptCheckout{}, "several succeeded worktree.create operations claim the attempt", false
	}
	intent, _ := decodeOperationPayload[attemptWorktreeCreateIntent](matched.Intent)
	outcome, decoded := recordedWorktreeCreateOutcome(matched)
	switch {
	case !decoded:
		return attemptCheckout{}, "the attempt's worktree.create outcome could not be decoded", false
	case intent.RepositoryRoot != repositoryRoot:
		return attemptCheckout{}, "the attempt's worktree was created in another repository root", false
	case intent.Branch != row.Branch || outcome.Info.Branch != row.Branch:
		return attemptCheckout{}, "the attempt's worktree.create records another branch", false
	case intent.BaseRef != row.BaseCommit || outcome.BaseCommit != row.BaseCommit:
		return attemptCheckout{}, "the attempt's worktree.create records another base commit", false
	case outcome.Info.Path != row.Path:
		return attemptCheckout{}, "the attempt's worktree.create records another path", false
	}
	return attemptCheckout{
		RepositoryRoot: repositoryRoot,
		Path:           row.Path,
		Branch:         attemptBranchRefPrefix + row.Branch,
		BaseOID:        row.BaseCommit,
	}, "", true
}

// recordedWorktreeCreateOutcome reads a succeeded worktree.create
// operation's recorded outcome: the act evidence the assignment act
// records, else the outcome a recovery records.
func recordedWorktreeCreateOutcome(op *Operation) (worktreeCreateOutcome, bool) {
	if outcome, ok := decodeOperationPayload[worktreeCreateOutcome](op.ActEvidence); ok && outcome.Info.Path != "" {
		return outcome, true
	}
	if outcome, ok := decodeOperationPayload[worktreeCreateOutcome](op.Outcome); ok && outcome.Info.Path != "" {
		return outcome, true
	}
	return worktreeCreateOutcome{}, false
}
