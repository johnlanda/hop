package app

// The post-merge worktree retirement operation kinds and their frozen
// exec shapes (docs/plan/phase-3-worktree-retirement.md section 7). Both
// are exec-claimable: hop check-exec claims either exactly as it claims a
// check.run or integration.merge execution, and the store resolves the
// frozen argv and spawn directory from the intent's
// worktreeRetirementExecIntent members.
const (
	// OpRetirementCheck is the detection execution: whether the run's
	// validated integration head is contained in the target branch's tip,
	// both frozen into the intent as object ids.
	OpRetirementCheck OperationKind = "retirement.check"
	// OpWorktreeRetire is one attempt worktree's removal execution, a
	// `git worktree remove` that never carries a force option.
	OpWorktreeRetire OperationKind = "worktree.retire"
)

// ExecClaimable reports whether hop check-exec may claim an operation of
// this kind: the check execution, the scratch merge and the two
// retirement executions. Every other kind is an act the controller
// performs itself (a ref move, a Herdr call) and never accepts a claim.
// The real store and the fakes decide claimability through this one
// predicate.
func (k OperationKind) ExecClaimable() bool {
	switch k {
	case OpCheckRun, OpIntegrationMerge, OpRetirementCheck, OpWorktreeRetire:
		return true
	default:
		return false
	}
}

// worktreeRetirementExecIntent is the exec-boundary part of both retirement
// intents: the argv hop check-exec runs byte for byte and the directory
// the spawn runs in. The JSON keys "argv" and "cwd" are the store's
// documented contract (LoadCheckExecutionContext reads them for both
// retirement kinds).
type worktreeRetirementExecIntent struct {
	Argv []string `json:"argv"`
	Cwd  string   `json:"cwd"`
}

// worktreeRetirementCheckArgv is the frozen detection argv: exit 0 when headOID
// is contained in targetOID (a commit contains itself), 1 when it is not,
// 128 for an unknown object (probe-pinned). Both revisions are object
// ids, never ref names, so the answer depends only on the immutable
// object graph.
func worktreeRetirementCheckArgv(git, repositoryRoot, headOID, targetOID string) []string {
	return []string{git, "-C", repositoryRoot, "merge-base", "--is-ancestor", headOID, targetOID}
}

// worktreeRetireArgv is the frozen removal argv. It never carries a force
// option, so git's own refusal of a checkout with modified, staged or
// untracked files stays in force; the `-c status.showUntrackedFiles=all`
// override keeps untracked files visible to that refusal whatever the
// repository's own configuration says (probe-pinned: a repository-local
// `no` otherwise lets the removal delete them).
func worktreeRetireArgv(git, repositoryRoot, path string) []string {
	return []string{git, "-C", repositoryRoot, "-c", "status.showUntrackedFiles=all", "worktree", "remove", path}
}
