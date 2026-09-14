package app

import (
	"slices"
	"strings"
	"time"

	"github.com/johnlanda/hop/internal/domain/run"
)

// LaunchClaimDeadline bounds mechanical launcher start (from pane
// creation): if no claim row appears within it, the pane-open operation
// goes reconciling rather than being resent (docs/plan/phase-2-design.md
// section 6). Human-interaction time after a claim exists is unbounded.
const LaunchClaimDeadline = 120 * time.Second

// LaunchDeadlineExpired reports whether the mechanical launch-claim
// deadline has passed since openedAt, as of now.
func LaunchDeadlineExpired(openedAt, now time.Time) bool {
	return now.Sub(openedAt) > LaunchClaimDeadline
}

// LaunchSettlement classifies an InspectPane observation against a launch
// claim under the section 6 corroboration predicate.
type LaunchSettlement string

// Launch settlement outcomes.
const (
	// SettlementSettled means every conjunct of the predicate holds: the
	// claim may settle to execed.
	SettlementSettled LaunchSettlement = "settled"
	// SettlementForkingWrapper means the observation matches on executable
	// identity, marker and binding but differs on pid: the unsupported
	// forking-wrapper topology, which fails closed.
	SettlementForkingWrapper LaunchSettlement = "forking-wrapper"
	// SettlementUnresolved means the claim stays ambiguous: the pane does
	// not match the binding, no foreground process was observed, or
	// identity/marker do not match.
	SettlementUnresolved LaunchSettlement = "unresolved"
)

// CorroborateSettlement applies the section 6 corroboration predicate, used
// identically by settlement, adoption and warm reattach. paneMatches
// reports whether the inspected pane is the claim's current creation
// binding (pane ID, or recovered by creation label) — a false paneMatches
// is always unresolved, since the predicate is meaningless off-target.
// expectedExecutable is the resolved executable identity (name or argv0)
// the claim recorded; marker is the run/attempt/incarnation or native
// session identifier expected in the process's full argv.
func CorroborateSettlement(paneMatches bool, pane PaneProcess, expectedExecutable, marker string, claim LaunchClaim) LaunchSettlement { //nolint:gocritic // hugeParam: claim is an immutable snapshot read once by this pure decision function; callers pass a local value, so a pointer would only invite aliasing.
	if !paneMatches || len(pane.Foreground) == 0 {
		return SettlementUnresolved
	}
	fg := pane.Foreground[0]
	identityMatches := fg.Argv0 == expectedExecutable || fg.Name == expectedExecutable
	markerMatches := slices.Contains(fg.Argv, marker) || strings.Contains(fg.Cmdline, marker)
	if !identityMatches || !markerMatches {
		return SettlementUnresolved
	}
	if fg.PID == claim.PID {
		return SettlementSettled
	}
	return SettlementForkingWrapper
}

// OccupantMatches reports whether an inspected pane's foreground process
// matches recorded occupant evidence: the foreground process's argv or
// cmdline carries the marker and its pid equals the recorded pid. It is
// used by the close rule immediately before ClosePane and by warm reattach;
// the caller is responsible for having resolved the pane by the evidence's
// label first (label identity is not itself observable from PaneProcess).
func OccupantMatches(evidence run.OccupantEvidence, pane PaneProcess) bool {
	if len(pane.Foreground) == 0 {
		return false
	}
	fg := pane.Foreground[0]
	if fg.PID != evidence.PID {
		return false
	}
	return slices.Contains(fg.Argv, evidence.ArgvMarker) || strings.Contains(fg.Cmdline, evidence.ArgvMarker)
}

// GroupRetirementOutcome is one of the four typed outcomes classifying a
// process-group listing against the argv HOP expects to find there
// (docs/plan/phase-2-design.md section 7, "group-retirement rule").
type GroupRetirementOutcome string

// Group retirement outcomes.
const (
	// GroupEmpty means the group is already gone: no signal is sent.
	GroupEmpty GroupRetirementOutcome = "empty"
	// GroupMatched means at least one member's argv matches the expected
	// argv: the caller signals the group, then awaits absence.
	GroupMatched GroupRetirementOutcome = "matched"
	// GroupMismatched means the group is non-empty but no member's argv
	// matches: the caller never signals it, and the operation goes
	// reconciling with the listing as evidence.
	GroupMismatched GroupRetirementOutcome = "mismatched"
	// GroupInspectionFailed means the listing itself could not be made:
	// the caller fails closed, never signaling blindly.
	GroupInspectionFailed GroupRetirementOutcome = "inspection-failed"
)

// ClassifyGroupRetirement classifies a ProcessGroupInspector.GroupProcesses
// listing against expectedArgv. listErr is the error GroupProcesses
// returned, if any; a non-nil listErr always classifies as
// GroupInspectionFailed regardless of processes. An empty, error-free
// listing is GroupEmpty. A non-empty listing classifies as GroupMatched
// when any member's argv exactly equals expectedArgv, GroupMismatched
// otherwise. Group identity is always corroborated by argv, never by pid or
// pgid alone: there is no process start time anywhere in the observable
// surface, so a recycled pgid is never trusted on its own.
func ClassifyGroupRetirement(processes []GroupProcess, listErr error, expectedArgv []string) GroupRetirementOutcome {
	if listErr != nil {
		return GroupInspectionFailed
	}
	if len(processes) == 0 {
		return GroupEmpty
	}
	for _, p := range processes {
		if slices.Equal(p.Argv, expectedArgv) {
			return GroupMatched
		}
	}
	return GroupMismatched
}
