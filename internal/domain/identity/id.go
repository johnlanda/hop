// Package identity holds the strongly typed identifiers shared across HOP's
// domain modules. Every identifier wraps a canonical lowercase UUID string;
// parsing is pure and never touches a clock or a random source. Identifiers
// are generated only through the application's IDGenerator port and handed
// to this package's Parse functions for validation.
package identity

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrInvalidID reports that a raw string is not a canonical lowercase UUID:
// 32 hexadecimal digits grouped 8-4-4-4-12 and separated by hyphens.
var ErrInvalidID = errors.New("identity: invalid id")

// parseUUID validates raw as a canonical lowercase UUID and returns it
// unchanged, or ErrInvalidID. Canonical form is lowercase throughout (a
// mixed- or upper-case string is rejected, not normalized) with hexadecimal
// groups of exactly 8-4-4-4-12 digits.
func parseUUID(raw string) (string, error) {
	// groupLengths are the hexadecimal digit counts of the five
	// hyphen-separated groups of a canonical UUID string.
	groupLengths := [5]int{8, 4, 4, 4, 12}

	if raw == "" || strings.ToLower(raw) != raw {
		return "", ErrInvalidID
	}
	groups := strings.Split(raw, "-")
	if len(groups) != len(groupLengths) {
		return "", ErrInvalidID
	}
	for i, group := range groups {
		if len(group) != groupLengths[i] {
			return "", ErrInvalidID
		}
		if _, err := strconv.ParseUint(group, 16, 64); err != nil {
			return "", ErrInvalidID
		}
	}
	return raw, nil
}

// RepositoryID identifies a repository: the canonical target checkout a
// run's worktrees share.
type RepositoryID string

// ParseRepositoryID validates raw as a canonical lowercase UUID and returns
// it as a RepositoryID, or ErrInvalidID.
func ParseRepositoryID(raw string) (RepositoryID, error) {
	v, err := parseUUID(raw)
	if err != nil {
		return "", fmt.Errorf("identity: parse repository id: %w", err)
	}
	return RepositoryID(v), nil
}

// String returns id's canonical UUID form.
func (id RepositoryID) String() string { return string(id) }

// RunID identifies a run: one task brief executed under an immutable
// effective configuration.
type RunID string

// ParseRunID validates raw as a canonical lowercase UUID and returns it as a
// RunID, or ErrInvalidID.
func ParseRunID(raw string) (RunID, error) {
	v, err := parseUUID(raw)
	if err != nil {
		return "", fmt.Errorf("identity: parse run id: %w", err)
	}
	return RunID(v), nil
}

// String returns id's canonical UUID form.
func (id RunID) String() string { return string(id) }

// TaskID identifies a task: a scoped unit of work with acceptance criteria
// and dependencies.
type TaskID string

// ParseTaskID validates raw as a canonical lowercase UUID and returns it as
// a TaskID, or ErrInvalidID.
func ParseTaskID(raw string) (TaskID, error) {
	v, err := parseUUID(raw)
	if err != nil {
		return "", fmt.Errorf("identity: parse task id: %w", err)
	}
	return TaskID(v), nil
}

// String returns id's canonical UUID form.
func (id TaskID) String() string { return string(id) }

// AttemptID identifies an attempt: one execution of a task.
type AttemptID string

// ParseAttemptID validates raw as a canonical lowercase UUID and returns it
// as an AttemptID, or ErrInvalidID.
func ParseAttemptID(raw string) (AttemptID, error) {
	v, err := parseUUID(raw)
	if err != nil {
		return "", fmt.Errorf("identity: parse attempt id: %w", err)
	}
	return AttemptID(v), nil
}

// String returns id's canonical UUID form.
func (id AttemptID) String() string { return string(id) }

// SessionID identifies a session: a HOP identity for a manager/worker
// harness lifecycle, including its launch reservation.
type SessionID string

// ParseSessionID validates raw as a canonical lowercase UUID and returns it
// as a SessionID, or ErrInvalidID.
func ParseSessionID(raw string) (SessionID, error) {
	v, err := parseUUID(raw)
	if err != nil {
		return "", fmt.Errorf("identity: parse session id: %w", err)
	}
	return SessionID(v), nil
}

// String returns id's canonical UUID form.
func (id SessionID) String() string { return string(id) }

// WorktreeID identifies a worktree: checkout provenance used by one or more
// sessions.
type WorktreeID string

// ParseWorktreeID validates raw as a canonical lowercase UUID and returns it
// as a WorktreeID, or ErrInvalidID.
func ParseWorktreeID(raw string) (WorktreeID, error) {
	v, err := parseUUID(raw)
	if err != nil {
		return "", fmt.Errorf("identity: parse worktree id: %w", err)
	}
	return WorktreeID(v), nil
}

// String returns id's canonical UUID form.
func (id WorktreeID) String() string { return string(id) }

// ResultID identifies a result: a submitted outcome for a particular task
// attempt.
type ResultID string

// ParseResultID validates raw as a canonical lowercase UUID and returns it
// as a ResultID, or ErrInvalidID.
func ParseResultID(raw string) (ResultID, error) {
	v, err := parseUUID(raw)
	if err != nil {
		return "", fmt.Errorf("identity: parse result id: %w", err)
	}
	return ResultID(v), nil
}

// String returns id's canonical UUID form.
func (id ResultID) String() string { return string(id) }

// ArtifactID identifies an artifact: a file reference owned by a run or a
// result.
type ArtifactID string

// ParseArtifactID validates raw as a canonical lowercase UUID and returns it
// as an ArtifactID, or ErrInvalidID.
func ParseArtifactID(raw string) (ArtifactID, error) {
	v, err := parseUUID(raw)
	if err != nil {
		return "", fmt.Errorf("identity: parse artifact id: %w", err)
	}
	return ArtifactID(v), nil
}

// String returns id's canonical UUID form.
func (id ArtifactID) String() string { return string(id) }

// OperationID identifies one row of the application's operation journal: a
// controller-side intent/act/outcome record.
type OperationID string

// ParseOperationID validates raw as a canonical lowercase UUID and returns
// it as an OperationID, or ErrInvalidID.
func ParseOperationID(raw string) (OperationID, error) {
	v, err := parseUUID(raw)
	if err != nil {
		return "", fmt.Errorf("identity: parse operation id: %w", err)
	}
	return OperationID(v), nil
}

// String returns id's canonical UUID form.
func (id OperationID) String() string { return string(id) }

// IncarnationID identifies one launch incarnation of an attempt: the unit a
// launch claim and its runtime binding are scoped to. A cold relaunch mints
// a new incarnation for the same attempt.
type IncarnationID string

// ParseIncarnationID validates raw as a canonical lowercase UUID and returns
// it as an IncarnationID, or ErrInvalidID.
func ParseIncarnationID(raw string) (IncarnationID, error) {
	v, err := parseUUID(raw)
	if err != nil {
		return "", fmt.Errorf("identity: parse incarnation id: %w", err)
	}
	return IncarnationID(v), nil
}

// String returns id's canonical UUID form.
func (id IncarnationID) String() string { return string(id) }
