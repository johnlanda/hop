package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
)

// The pre-act inspection of one candidate attempt worktree
// (docs/plan/phase-3-worktree-retirement.md section 4): read-only git
// observations, never journaled, deciding whether the checkout may be
// removed, is already gone, is retained for now, or is released for good.
// Every observed shape is pinned by the process-adapter probe.

// WorktreeRetainedCategory names why a candidate is kept for now; a
// retained checkout is examined again on every later pass.
type WorktreeRetainedCategory string

// The retained categories and the human action each one renders with.
const (
	RetainedUncommittedChanges WorktreeRetainedCategory = "uncommitted-changes"
	RetainedHiddenChanges      WorktreeRetainedCategory = "hidden-changes"
	RetainedLocked             WorktreeRetainedCategory = "locked"
	RetainedInterruptedRemoval WorktreeRetainedCategory = "interrupted-removal"
	RetainedRemoveRefused      WorktreeRetainedCategory = "remove-refused"
	RetainedInspectionFailed   WorktreeRetainedCategory = "inspection-failed"
)

// WorktreeReleaseReason names why HOP permanently relinquishes a checkout
// it can no longer prove is its own: a final outcome, journaled once, after
// which HOP never touches the checkout again.
type WorktreeReleaseReason string

// The release reasons.
const (
	ReleasedRepositoryRoot  WorktreeReleaseReason = "repository-root"
	ReleasedNotRegistered   WorktreeReleaseReason = "not-registered"
	ReleasedOtherBranch     WorktreeReleaseReason = "other-branch"
	ReleasedDetached        WorktreeReleaseReason = "detached"
	ReleasedOtherRepository WorktreeReleaseReason = "other-repository"
	ReleasedBaseNotAncestor WorktreeReleaseReason = "base-not-ancestor"
	ReleasedUnverified      WorktreeReleaseReason = "unverified-provenance"
)

// PathInspector is the composition seam the inspection resolves paths
// through, since this package does no filesystem access itself: it
// returns path's canonical form — the deepest existing ancestor with
// symbolic links resolved and the remainder appended — and whether path
// itself exists. Herdr records a checkout in its requested, possibly
// non-canonical spelling while git lists the canonical one (both
// probe-pinned), so every path comparison goes through it.
type PathInspector func(path string) (canonical string, exists bool, err error)

// retirementGitEnv is the complete environment every retirement git
// invocation runs with — the exact environment the process probe executed
// under: inherited global and system configuration suppressed, terminal
// prompts off, nothing else.
func retirementGitEnv() []string {
	return []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0"}
}

// errRetirementGitUnconfigured refuses a retirement git read before any
// side effect when GitExecutable is not absolute.
var errRetirementGitUnconfigured = errors.New("app: git executable is not configured as an absolute path")

// retirementGit runs one unclaimed, read-only retirement git invocation
// against dir under retirementGitEnv.
func (c *Controller) retirementGit(ctx context.Context, dir string, args ...string) (CommandResult, error) {
	if !filepath.IsAbs(c.GitExecutable) {
		return CommandResult{}, errRetirementGitUnconfigured
	}
	return c.Commands.Run(ctx, Command{Argv: append([]string{c.GitExecutable, "-C", dir}, args...), Env: retirementGitEnv()})
}

// attemptCheckout is one candidate's recorded provenance.
type attemptCheckout struct {
	RepositoryRoot string
	Path           string
	// Branch is the full recorded attempt branch ref.
	Branch  string
	BaseOID string
}

// checkoutDisposition is an inspection's decision kind.
type checkoutDisposition string

const (
	checkoutRemovable checkoutDisposition = "removable"
	checkoutAbsent    checkoutDisposition = "absent"
	checkoutReleased  checkoutDisposition = "released"
	checkoutRetained  checkoutDisposition = "retained"
)

// checkoutVerdict is one inspection's decision and its evidence.
type checkoutVerdict struct {
	Disposition checkoutDisposition
	Retained    WorktreeRetainedCategory
	Released    WorktreeReleaseReason
	// ListedPath is git's own spelling of the checkout, the path a removal
	// names; "" when git does not list it.
	ListedPath string
	// WasAbsent reports a listed checkout whose directory is already gone:
	// its removal only prunes the entry, and the outcome is absent.
	WasAbsent bool
	// Detail says which observation decided, for the journal and the
	// status line.
	Detail string
}

func retainedVerdict(category WorktreeRetainedCategory, listedPath, detail string) checkoutVerdict {
	return checkoutVerdict{Disposition: checkoutRetained, Retained: category, ListedPath: listedPath, Detail: detail}
}

func releasedVerdict(reason WorktreeReleaseReason, listedPath, detail string) checkoutVerdict {
	return checkoutVerdict{Disposition: checkoutReleased, Released: reason, ListedPath: listedPath, Detail: detail}
}

// inspectAttemptCheckout runs the section 4 observations for one
// candidate, in order, stopping at the first that decides. It never
// removes anything; only a removable verdict may lead to the claimed act.
func (c *Controller) inspectAttemptCheckout(ctx context.Context, candidate *attemptCheckout, inspect PathInspector) checkoutVerdict {
	if inspect == nil {
		return retainedVerdict(RetainedInspectionFailed, "", "no path inspector configured")
	}
	canonicalRoot, rootExists, err := inspect(candidate.RepositoryRoot)
	if err != nil || !rootExists {
		return retainedVerdict(RetainedInspectionFailed, "", "the repository root could not be resolved")
	}
	canonicalPath, pathExists, err := inspect(candidate.Path)
	if err != nil {
		return retainedVerdict(RetainedInspectionFailed, "", "the checkout path could not be resolved")
	}
	if canonicalPath == canonicalRoot {
		return releasedVerdict(ReleasedRepositoryRoot, "", "the recorded path is the repository root")
	}

	record, listed := c.listedCheckout(ctx, candidate.RepositoryRoot, canonicalPath, inspect)
	if !listed {
		return retainedVerdict(RetainedInspectionFailed, "", "the repository's worktree list could not be read")
	}
	switch {
	case record == nil && !pathExists:
		return checkoutVerdict{Disposition: checkoutAbsent, Detail: "the checkout is gone and git no longer lists it"}
	case record == nil:
		return releasedVerdict(ReleasedNotRegistered, "", "git does not list the directory as a worktree of the repository")
	case record.Detached:
		return releasedVerdict(ReleasedDetached, record.Path, "the checkout has a detached HEAD, not "+candidate.Branch)
	case record.Branch != candidate.Branch:
		return releasedVerdict(ReleasedOtherBranch, record.Path, "the checkout is on "+record.Branch+", not "+candidate.Branch)
	case record.Locked:
		return retainedVerdict(RetainedLocked, record.Path, "git lists the worktree as locked")
	case !pathExists:
		return checkoutVerdict{Disposition: checkoutRemovable, ListedPath: record.Path, WasAbsent: true, Detail: "the directory is gone; git still lists the worktree"}
	}

	rootCommon, ok := c.retirementCommonDir(ctx, candidate.RepositoryRoot, inspect)
	if !ok {
		return retainedVerdict(RetainedInspectionFailed, record.Path, "the repository's common directory could not be resolved")
	}
	checkoutCommon, ok := c.retirementCommonDir(ctx, record.Path, inspect)
	if !ok {
		return retainedVerdict(RetainedInspectionFailed, record.Path, "the checkout's common directory could not be resolved")
	}
	if checkoutCommon != rootCommon {
		return releasedVerdict(ReleasedOtherRepository, record.Path, "the checkout belongs to another repository")
	}

	ancestry, err := c.retirementGit(ctx, candidate.RepositoryRoot, "merge-base", "--is-ancestor", candidate.BaseOID, record.Head)
	if err != nil {
		return retainedVerdict(RetainedInspectionFailed, record.Path, "the checkout's base could not be checked")
	}
	switch classifyAncestry(ancestry.ExitCode) {
	case ancestryNotContained:
		return releasedVerdict(ReleasedBaseNotAncestor, record.Path, "the checkout's HEAD no longer descends from the recorded base")
	case ancestryFailed:
		return retainedVerdict(RetainedInspectionFailed, record.Path, "the checkout's base could not be checked")
	case ancestryContained:
	}

	status, err := c.retirementGit(ctx, record.Path, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || status.ExitCode != 0 {
		return retainedVerdict(RetainedInspectionFailed, record.Path, "the checkout's status could not be read")
	}
	if strings.TrimSpace(string(status.Stdout)) != "" {
		return retainedVerdict(RetainedUncommittedChanges, record.Path, "git status reports uncommitted or untracked changes")
	}
	tags, err := c.retirementGit(ctx, record.Path, "ls-files", "-v", "-z")
	if err != nil || tags.ExitCode != 0 {
		return retainedVerdict(RetainedInspectionFailed, record.Path, "the checkout's index could not be read")
	}
	hidden, err := hasHiddenIndexFlags(tags.Stdout)
	if err != nil {
		return retainedVerdict(RetainedInspectionFailed, record.Path, "the checkout's index could not be parsed")
	}
	if hidden {
		return retainedVerdict(RetainedHiddenChanges, record.Path, "index entries are marked assume-unchanged or skip-worktree")
	}
	return checkoutVerdict{Disposition: checkoutRemovable, ListedPath: record.Path, Detail: "clean, on the recorded branch, descending from the recorded base"}
}

// listedCheckout reads the root's own `worktree list --porcelain -z` and
// returns the record whose path resolves to canonicalPath, or nil when git
// lists no such checkout; ok is false when the listing could not be read
// or parsed.
func (c *Controller) listedCheckout(ctx context.Context, root, canonicalPath string, inspect PathInspector) (record *listedWorktree, ok bool) {
	listing, err := c.retirementGit(ctx, root, "worktree", "list", "--porcelain", "-z")
	if err != nil || listing.ExitCode != 0 {
		return nil, false
	}
	records, err := parseWorktreeListZ(listing.Stdout)
	if err != nil {
		return nil, false
	}
	for i := range records {
		listed, _, listErr := inspect(records[i].Path)
		if listErr == nil && listed == canonicalPath {
			return &records[i], true
		}
	}
	return nil, true
}

// observeCheckout is the post-act observation of a candidate: whether the
// root's worktree list still names it and whether its recorded path still
// exists; ok is false when either could not be observed.
func (c *Controller) observeCheckout(ctx context.Context, candidate *attemptCheckout, inspect PathInspector) (listed, exists, ok bool) {
	if inspect == nil {
		return false, false, false
	}
	canonicalPath, exists, err := inspect(candidate.Path)
	if err != nil {
		return false, false, false
	}
	record, ok := c.listedCheckout(ctx, candidate.RepositoryRoot, canonicalPath, inspect)
	if !ok {
		return false, false, false
	}
	return record != nil, exists, true
}

// retirementCommonDir resolves dir's git common directory in canonical
// form.
func (c *Controller) retirementCommonDir(ctx context.Context, dir string, inspect PathInspector) (string, bool) {
	result, err := c.retirementGit(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || result.ExitCode != 0 {
		return "", false
	}
	common := strings.TrimSpace(string(result.Stdout))
	if !filepath.IsAbs(common) {
		return "", false
	}
	canonical, exists, err := inspect(common)
	if err != nil || !exists {
		return "", false
	}
	return canonical, true
}
