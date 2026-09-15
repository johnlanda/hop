package app

import (
	"path/filepath"
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

// CorroborateSettlement applies the section 6 corroboration predicate, the
// ONLY corroboration rule, used identically by settlement, adoption and
// warm reattach. paneMatches reports whether the inspected pane is the
// claim's current creation binding (pane ID, or recovered by creation
// label) — a false paneMatches is always unresolved, since the predicate
// is meaningless off-target. The expected executable identity is the one
// the claim itself recorded, never a caller-selected value. markers are
// the run/attempt/incarnation or native session identifiers derived from
// durable launch/binding context; the observed argv must carry at least
// one. A still-running `hop launch` invocation is explicitly excluded: a
// paused pre-exec launcher can never satisfy the predicate even when the
// recorded executable is the HOP binary itself. Missing identity — an
// empty claim executable or an empty marker set — is unresolved, never
// settled: the predicate fails closed.
func CorroborateSettlement(paneMatches bool, pane PaneProcess, markers []string, claim LaunchClaim) LaunchSettlement { //nolint:gocritic // hugeParam: claim is an immutable snapshot read once by this pure decision function; callers pass a local value, so a pointer would only invite aliasing.
	if !paneMatches || len(pane.Foreground) == 0 {
		return SettlementUnresolved
	}
	if claim.Executable == "" {
		return SettlementUnresolved
	}
	fg := pane.Foreground[0]
	if isLauncherInvocation(fg.Argv) {
		return SettlementUnresolved
	}
	if !executableMatches(fg, claim.Executable) || FirstMarkerMatch(pane, markers) == "" {
		return SettlementUnresolved
	}
	if fg.PID == claim.PID {
		return SettlementSettled
	}
	return SettlementForkingWrapper
}

// executableMatches reports whether an observed foreground process's
// executable identity matches expected (the claim's recorded absolute
// path), against the real, platform-specific pane.process_info surface:
// argv[0] is reported verbatim (S2), so an exact match there is the
// strongest form — hop launch execs the absolute path it recorded, and
// argv[0] equals it unless the harness rewrites its own process title.
// argv0 and name are never absolute paths: on macOS, Herdr's argv0 is
// process_argv0_name, the BASENAME of argv[0] (leading dash stripped); on
// Linux, Herdr never reports argv0 (empty) and name is the kernel's
// 15-byte-truncated comm. So both are compared against expected's
// basename, never expected itself. An empty expected never matches
// (missing identity fails closed, per CorroborateSettlement). Executable
// identity is one corroboration conjunct among several (marker, pid,
// binding); it does not by itself establish occupant identity.
func executableMatches(fg ProcessInfo, expected string) bool { //nolint:gocritic // hugeParam: ProcessInfo is CorroborateSettlement's own pure-value argument shape, passed by value throughout this decision file; called once per corroboration round, never a hot loop.
	if expected == "" {
		return false
	}
	if len(fg.Argv) > 0 && fg.Argv[0] == expected {
		return true
	}
	base := filepath.Base(expected)
	if fg.Argv0 != "" && fg.Argv0 == base {
		return true
	}
	return fg.Name != "" && fg.Name == base
}

// FirstMarkerMatch returns the first non-empty marker the pane's foreground
// process argv or cmdline carries, or "" when none matches.
func FirstMarkerMatch(pane PaneProcess, markers []string) string {
	if len(pane.Foreground) == 0 {
		return ""
	}
	fg := pane.Foreground[0]
	for _, marker := range markers {
		if marker == "" {
			continue
		}
		if slices.Contains(fg.Argv, marker) || strings.Contains(fg.Cmdline, marker) {
			return marker
		}
	}
	return ""
}

// isLauncherInvocation reports whether argv is a `hop launch` invocation:
// the launch subcommand with its run/attempt flags. The corroboration
// predicate excludes it so a paused pre-exec launcher never settles a
// claim, regardless of which executable the claim recorded.
func isLauncherInvocation(argv []string) bool {
	return len(argv) >= 2 && argv[1] == "launch" && (slices.Contains(argv, "--run") || slices.Contains(argv, "--attempt"))
}

// ServerContinuityEstablished is the replaceable continuity predicate:
// both server-instance tokens are non-empty and equal. Under the
// Runtime.ServerInstance contract a non-empty token identifies both the
// configured socket and the server process behind it, so token equality
// is the whole predicate — there is no separately observed socket path.
// Anything else — either token unknown, or inequality — is NOT
// continuity: ambiguous, never absence. A deferred native restore fires
// only after a server restart, so an unchanged server process with the
// pane gone cannot have a restore pending; a restart or live handoff
// changes the identity and fails closed here.
func ServerContinuityEstablished(recorded, observed string) bool {
	return recorded != "" && recorded == observed
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

// ArgvUnavailable is the sentinel GroupProcess.Argv carries, as its sole
// element, for a group member whose argv could not be read (EPERM, or the
// process exited between listing and read). internal/adapters/process
// (task 4b) mirrors this exact literal; the classifier never treats such a
// member as proof of mismatch, only as ambiguity.
const ArgvUnavailable = "<argv unavailable>"

// ClassifyGroupRetirement classifies a ProcessGroupInspector.GroupProcesses
// listing against the expected argvs — the frozen check argv AND the
// `hop check-exec` invocation that execs it, so a paused pre-exec boundary
// still classifies as owned work. listErr is the error GroupProcesses
// returned, if any; a non-nil listErr always classifies as
// GroupInspectionFailed regardless of processes. An empty, error-free
// listing is GroupEmpty. Every group the process adapter runs (including
// every check) carries its own supervisory sleep anchor process
// (argv `<sleep path> 100000`) that outlives a crashed controller and pins
// the pgid until retirement; a listing whose only live members are that
// anchor means the check itself has already exited, so it classifies as
// GroupMatched too — the caller signals the anchor and treats the group as
// already gone, rather than waiting out the anchor's own ~27.8-hour sleep.
// Otherwise, a listing classifies as GroupMatched when any member's argv
// exactly equals one of the expected argvs; GroupInspectionFailed when no member matches
// but at least one member's argv could not be read (ArgvUnavailable is
// ambiguous, never proof of mismatch, and is never signaled on); and
// GroupMismatched only when every member's argv was read and none matches.
// Group identity is always corroborated by argv, never by pid or pgid
// alone: there is no process start time anywhere in the observable
// surface, so a recycled pgid is never trusted on its own.
func ClassifyGroupRetirement(processes []GroupProcess, listErr error, expectedArgvs [][]string) GroupRetirementOutcome {
	if listErr != nil {
		return GroupInspectionFailed
	}
	if len(processes) == 0 {
		return GroupEmpty
	}
	unavailable := false
	onlyAnchors := true
	for _, p := range processes {
		if len(p.Argv) == 1 && p.Argv[0] == ArgvUnavailable {
			unavailable = true
			onlyAnchors = false
			continue
		}
		if matchesAnyArgv(p.Argv, expectedArgvs) {
			return GroupMatched
		}
		if !isSleepAnchor(p.Argv) {
			onlyAnchors = false
		}
	}
	if onlyAnchors {
		return GroupMatched
	}
	if unavailable {
		return GroupInspectionFailed
	}
	return GroupMismatched
}

// matchesAnyArgv reports whether argv exactly equals one of the non-empty
// expected argvs.
func matchesAnyArgv(argv []string, expected [][]string) bool {
	for _, want := range expected {
		if len(want) > 0 && slices.Equal(argv, want) {
			return true
		}
	}
	return false
}

// isSleepAnchor reports whether argv is the process adapter's supervisory
// sleep anchor: a two-element argv naming a "sleep" executable (by base
// name, portable across the anchor's absolute path on different platforms)
// with "100000" as its sole argument.
func isSleepAnchor(argv []string) bool {
	return len(argv) == 2 && argv[1] == "100000" && filepath.Base(argv[0]) == "sleep"
}
