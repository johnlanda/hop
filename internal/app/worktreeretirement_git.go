package app

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// Pure readers of the git observations worktree retirement decides on
// (docs/plan/phase-3-worktree-retirement.md sections 2 and 4). Every shape
// here is pinned by the executed process-adapter probe
// (internal/adapters/process/gitworktree_probe_test.go).

// listedWorktree is one `git worktree list --porcelain -z` record: the
// checkout's canonical path and HEAD, the full branch ref it has checked
// out ("" when detached or bare), and its lock and prunable attributes.
type listedWorktree struct {
	Path       string
	Head       string
	Branch     string
	Detached   bool
	Bare       bool
	Locked     bool
	LockReason string
	Prunable   bool
}

// parseWorktreeListZ parses `git worktree list --porcelain -z` output:
// every attribute is NUL-terminated and every record ends with one extra
// NUL. The first record is the main checkout, so empty output is an
// error, as is a record that does not start with its `worktree` line.
// Attributes this reader does not know are skipped: they carry nothing a
// retirement rule reads.
func parseWorktreeListZ(out []byte) ([]listedWorktree, error) {
	if len(out) == 0 {
		return nil, errors.New("worktree list is empty; a repository always lists its main checkout")
	}
	if !bytes.HasSuffix(out, []byte{0, 0}) {
		return nil, errors.New("worktree list does not end with a record terminator")
	}
	var records []listedWorktree
	for raw := range bytes.SplitSeq(bytes.TrimSuffix(out, []byte{0, 0}), []byte{0, 0}) {
		attributes := strings.Split(string(raw), "\x00")
		path, ok := strings.CutPrefix(attributes[0], "worktree ")
		if !ok || path == "" {
			return nil, fmt.Errorf("worktree list record %d does not start with its worktree line", len(records)+1)
		}
		record := listedWorktree{Path: path}
		for _, attribute := range attributes[1:] {
			name, value, _ := strings.Cut(attribute, " ")
			switch name {
			case "HEAD":
				record.Head = value
			case "branch":
				record.Branch = value
			case "detached":
				record.Detached = true
			case "bare":
				record.Bare = true
			case "locked":
				record.Locked = true
				record.LockReason = value
			case "prunable":
				record.Prunable = true
			}
		}
		records = append(records, record)
	}
	return records, nil
}

// hasHiddenIndexFlags reports whether `git ls-files -v -z` output marks any
// entry assume-unchanged (a lowercase tag) or skip-worktree (`S`): either
// flag hides a modification from `git status`, and a removal would delete
// it. Every entry is NUL-terminated (probe-pinned), so output ending
// without a terminator is an error, as is an entry that is not
// `<tag> <path>` — never a guess. The terminator does not prove the output
// complete (a cut can land on an entry boundary); completeness is the
// runner's truncation report, which retirementGit enforces.
func hasHiddenIndexFlags(out []byte) (bool, error) {
	if len(out) != 0 && out[len(out)-1] != 0 {
		return false, errors.New("ls-files output does not end with an entry terminator")
	}
	for entry := range bytes.SplitSeq(bytes.TrimSuffix(out, []byte{0}), []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		if len(entry) < 3 || entry[1] != ' ' {
			return false, errors.New("ls-files entry is not a tagged path")
		}
		tag := entry[0]
		if tag == 'S' || (tag >= 'a' && tag <= 'z') {
			return true, nil
		}
	}
	return false, nil
}

// ancestryResult is the retirement check's classified answer.
type ancestryResult string

const (
	ancestryContained    ancestryResult = "merged"
	ancestryNotContained ancestryResult = "not-merged"
	ancestryFailed       ancestryResult = "failed"
)

// classifyAncestry maps `git merge-base --is-ancestor` exit statuses: 0 is
// contained (a commit contains itself), 1 is not contained, and anything
// else (128 for an unknown object) is a failed check, never an answer.
func classifyAncestry(exitCode int) ancestryResult {
	switch exitCode {
	case 0:
		return ancestryContained
	case 1:
		return ancestryNotContained
	default:
		return ancestryFailed
	}
}

// branchRefPrefix is the namespace a retirement target must name.
const branchRefPrefix = "refs/heads/"

// errTargetUnreadable reports a `git symbolic-ref -q HEAD` failure that is
// neither a branch nor a detached HEAD.
var errTargetUnreadable = errors.New("the repository's checked-out branch could not be read")

// classifyTargetBranch maps `git symbolic-ref -q HEAD` at freeze to the
// run's retirement target: exit 0 with a `refs/heads/` name is that full
// ref; exit 1 with no output is a detached HEAD, recorded as no target;
// anything else is errTargetUnreadable.
func classifyTargetBranch(exitCode int, stdout []byte) (string, error) {
	switch exitCode {
	case 0:
		ref := strings.TrimSuffix(string(stdout), "\n")
		if !strings.HasPrefix(ref, branchRefPrefix) || len(ref) == len(branchRefPrefix) || strings.ContainsAny(ref, "\n\x00") {
			return "", errTargetUnreadable
		}
		return ref, nil
	case 1:
		if len(stdout) != 0 {
			return "", errTargetUnreadable
		}
		return "", nil
	default:
		return "", errTargetUnreadable
	}
}
