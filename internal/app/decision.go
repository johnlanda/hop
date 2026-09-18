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

// SettlementEvidence identifies the foreground member whose identity
// decided a settlement classification, and the marker its argv or cmdline
// carried: the values a settlement or adoption records as claim and
// binding occupant evidence. It is the zero value exactly when the
// classification is unresolved.
type SettlementEvidence struct {
	Occupant ProcessInfo
	Marker   string
}

// CorroborateSettlement applies the section 6 corroboration predicate, the
// ONLY corroboration rule, used identically by settlement, adoption and
// warm reattach. paneMatches reports whether the inspected pane is the
// claim's current creation binding (pane ID, or recovered by creation
// label) — a false paneMatches is always unresolved, since the predicate
// is meaningless off-target. The expected executable identity is the one
// the claim itself recorded, never a caller-selected value. markers are
// the run/attempt/incarnation or native session identifiers derived from
// durable launch/binding context; the observed argv must carry at least
// one. Missing identity — an empty claim executable or an empty marker
// set — is unresolved, never settled: the predicate fails closed.
//
// EVERY member of the observed foreground process group is considered,
// not only index 0: a real harness (Claude Code 2.1.270) spawns its
// configured MCP servers as children in its own process group immediately
// after the trust check, and Herdr reports group members in raw platform
// listing order (macOS: unsorted proc_listpids; Linux: ascending pid), so
// the launched process holds no particular index. Per member, a
// still-running `hop launch` invocation is skipped (a paused pre-exec
// launcher can never satisfy the predicate even when the recorded
// executable is the HOP binary itself), and executable identity, marker
// and pid are three conjuncts of the SAME member — a marker carried only
// by a foreign sibling corroborates nothing. Classification, fail-closed
// on the unsupported topology first: ANY member matching executable
// identity and marker under a pid DIFFERENT from the claim's is the
// forking-wrapper topology, even when the claimed pid also matches (a
// live wrapper that already exec'd carries the recorded executable and
// markers in its own argv, so a settled-first reading would adopt exactly
// the topology section 6 refuses); otherwise the member with the claim's
// pid matching executable and marker settles; otherwise unresolved.
func CorroborateSettlement(paneMatches bool, pane PaneProcess, markers []string, claim LaunchClaim) (LaunchSettlement, SettlementEvidence) { //nolint:gocritic // hugeParam: claim is an immutable snapshot read once by this pure decision function; callers pass a local value, so a pointer would only invite aliasing.
	if !paneMatches || len(pane.Foreground) == 0 {
		return SettlementUnresolved, SettlementEvidence{}
	}
	if claim.Executable == "" {
		return SettlementUnresolved, SettlementEvidence{}
	}
	var settled, wrapper *SettlementEvidence
	for _, fg := range pane.Foreground {
		if isLauncherInvocation(fg.Argv) {
			continue
		}
		if !executableMatches(fg, claim.Executable) {
			continue
		}
		marker := processMarkerMatch(fg, markers)
		if marker == "" {
			continue
		}
		evidence := SettlementEvidence{Occupant: fg, Marker: marker}
		switch {
		case fg.PID == claim.PID:
			if settled == nil {
				settled = &evidence
			}
		case wrapper == nil:
			wrapper = &evidence
		}
	}
	switch {
	case wrapper != nil:
		return SettlementForkingWrapper, *wrapper
	case settled != nil:
		return SettlementSettled, *settled
	default:
		return SettlementUnresolved, SettlementEvidence{}
	}
}

// ClaimProcessMatches reports whether SOME foreground member with the
// claim's pid satisfies the claim's executable identity and carries one of
// markers — the claimed process itself, observed among the members, with
// every conjunct on that SAME member and a `hop launch` invocation skipped.
// CorroborateSettlement's forking-wrapper classification does not say
// whether the claimed process was ALSO present; resume needs exactly that
// distinction, since a group holding both the matching claimed process and
// a matching different-pid process is the refused wrapper topology, while
// a matching different-pid process with no member under the claim's pid
// still satisfying these conjuncts is the restored-occupant case the
// restored-harness predicate decides. A false result observes only that no
// such member matched — not that the claim's pid is absent from the group.
// An empty claim executable never matches.
func ClaimProcessMatches(pane PaneProcess, markers []string, claim LaunchClaim) bool { //nolint:gocritic // hugeParam: claim is an immutable snapshot read once by this pure decision function, matching CorroborateSettlement's argument shape.
	if claim.Executable == "" {
		return false
	}
	for _, fg := range pane.Foreground {
		if fg.PID != claim.PID || isLauncherInvocation(fg.Argv) {
			continue
		}
		if executableMatches(fg, claim.Executable) && processMarkerMatch(fg, markers) != "" {
			return true
		}
	}
	return false
}

// RestoredHarnessOutcome classifies a pane's foreground members against a
// harness's native restore invocation for the session's durable native
// reference.
type RestoredHarnessOutcome string

// Restored-harness outcomes.
const (
	// RestoredHarnessMatched means exactly one member is the restored
	// harness: positive evidence tying that member to the run's native
	// session.
	RestoredHarnessMatched RestoredHarnessOutcome = "matched"
	// RestoredHarnessNone means no member is the restored harness.
	RestoredHarnessNone RestoredHarnessOutcome = "none"
	// RestoredHarnessAmbiguous means more than one member is the restored
	// harness: no single occupant is identified, so the caller fails closed.
	RestoredHarnessAmbiguous RestoredHarnessOutcome = "ambiguous"
	// RestoredHarnessUnsupported means no restore invocation shape is known
	// for the harness, or the native reference is empty: nothing can match.
	RestoredHarnessUnsupported RestoredHarnessOutcome = "unsupported"
)

// restoreInvocation is a harness's native restore command shape: the
// executable name Herdr's restore plan runs and the flag whose immediately
// following argument is the native session reference.
type restoreInvocation struct {
	Executable string
	ResumeFlag string
}

// restoreInvocationFor returns a harness's native restore invocation for
// retiring restored occupants: Herdr's native restore plan (repos/herdr/src/agent_resume.rs, `plan`:
// `["claude", "--resume", <id>]`, run by bare name through a login shell).
// The shape is pinned by the executed S3 probe
// (test/integration/spike_restore_test.go,
// TestSpikeRestoreAutoRelaunchBypassesLauncher: a restored process's argv
// is exactly `[claude --resume <id>]`, argv[0] basename `claude`). Only
// Claude Code has an invocation: it is the only harness HOP pre-assigns a
// native reference for and the only Phase 2 cold resume, so every other
// harness reports false and never authorizes a retirement.
func restoreInvocationFor(harness run.Harness) (restoreInvocation, bool) {
	if harness == run.HarnessClaude {
		return restoreInvocation{Executable: "claude", ResumeFlag: "--resume"}, true
	}
	return restoreInvocation{}, false
}

// MatchRestoredHarness is the restored-harness predicate: the positive
// evidence that authorizes retiring a present occupant which is not the
// claim's corroborated process. A member is the restored harness only when,
// on that SAME member, both hold:
//
//   - executable identity equals the harness's restore executable: when
//     Herdr reports argv, the basename of argv[0] (verbatim argv[0] is a
//     bare name under Herdr's restore and an absolute path under a launch);
//     when it reports none, argv0 or name, the only identity fields left;
//   - the native resume argument shape: when argv is reported, an argv
//     element equal to the resume flag immediately followed by an argv
//     element EXACTLY equal to nativeRef — never a substring, so a child
//     whose argument merely embeds the reference (a transcript path, say)
//     is not evidence. Only when Herdr reports no argv does the documented
//     fallback apply: cmdline carries `<flag> <nativeRef>` delimited by the
//     string's ends or spaces. On herdr 0.9.0 cmdline is argv joined by
//     spaces on both platforms (repos/herdr/src/platform/{macos,linux}.rs),
//     so the fallback is reachable only through a server that reports
//     cmdline without argv, which the schema permits.
//
// Exactly one matching member is RestoredHarnessMatched and is returned;
// two or more are RestoredHarnessAmbiguous, and every candidate is returned
// as the fail-closed evidence. A harness with no restore invocation, or an
// empty nativeRef, is RestoredHarnessUnsupported. Listing order carries no
// semantics and is never consulted.
func MatchRestoredHarness(pane PaneProcess, harness run.Harness, nativeRef string) (RestoredHarnessOutcome, []ProcessInfo) {
	invocation, ok := restoreInvocationFor(harness)
	if !ok || nativeRef == "" {
		return RestoredHarnessUnsupported, nil
	}
	var candidates []ProcessInfo
	for _, fg := range pane.Foreground {
		if restoredHarnessMember(fg, invocation, nativeRef) {
			candidates = append(candidates, fg)
		}
	}
	switch len(candidates) {
	case 0:
		return RestoredHarnessNone, nil
	case 1:
		return RestoredHarnessMatched, candidates
	default:
		return RestoredHarnessAmbiguous, candidates
	}
}

// restoredHarnessMember reports whether one foreground member is the
// restored harness for nativeRef under invocation (MatchRestoredHarness's
// per-member conjuncts).
func restoredHarnessMember(fg ProcessInfo, invocation restoreInvocation, nativeRef string) bool { //nolint:gocritic // hugeParam: ProcessInfo is the decision file's pure-value member shape, examined once per member per resume round, never a hot loop.
	if nativeRef == "" {
		return false
	}
	if len(fg.Argv) > 0 {
		if filepath.Base(fg.Argv[0]) != invocation.Executable {
			return false
		}
		for i := 1; i+1 < len(fg.Argv); i++ {
			if fg.Argv[i] == invocation.ResumeFlag && fg.Argv[i+1] == nativeRef {
				return true
			}
		}
		return false
	}
	if fg.Argv0 != invocation.Executable && fg.Name != invocation.Executable {
		return false
	}
	return containsDelimited(fg.Cmdline, invocation.ResumeFlag+" "+nativeRef)
}

// MatchRetirementTarget is the close-time recheck of a persisted
// positive-evidence retirement target: the whole current foreground group
// is classified again under MatchRestoredHarness with the recorded native
// reference, and the target still matches only when that classification is
// RestoredHarnessMatched AND its one candidate has the recorded pid. The
// uniqueness that authorized the retirement is therefore re-established on
// the very observation the close acts on: a second candidate appearing
// beside the recorded member is ambiguous and never closes, and a unique
// candidate under another pid is never adopted as a new target. A
// retirement records exactly one native reference, so any other marker
// count is RestoredHarnessUnsupported. The outcome and candidates are
// returned as the fail-closed evidence whenever matched is false.
func MatchRetirementTarget(pane PaneProcess, harness run.Harness, pid int, markers []string) (matched bool, outcome RestoredHarnessOutcome, candidates []ProcessInfo) {
	if len(markers) != 1 {
		return false, RestoredHarnessUnsupported, nil
	}
	outcome, candidates = MatchRestoredHarness(pane, harness, markers[0])
	matched = outcome == RestoredHarnessMatched && candidates[0].PID == pid
	return matched, outcome, candidates
}

// containsDelimited reports whether s contains needle bounded on each side
// by the start or end of s or a space.
func containsDelimited(s, needle string) bool {
	if needle == "" {
		return false
	}
	for offset := 0; offset+len(needle) <= len(s); {
		idx := strings.Index(s[offset:], needle)
		if idx < 0 {
			return false
		}
		start := offset + idx
		end := start + len(needle)
		if (start == 0 || s[start-1] == ' ') && (end == len(s) || s[end] == ' ') {
			return true
		}
		offset = start + 1
	}
	return false
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

// processMarkerMatch returns the first non-empty marker fg's own argv or
// cmdline carries, or "" when none matches. It serves the launch-marker
// conjunct of settlement and the stop close rule, where the run, attempt
// and incarnation markers legitimately sit INSIDE the larger prompt
// argument, so a cmdline substring is accepted. It is never the authority
// for retiring an occupant that is not the claim's corroborated process;
// that is MatchRestoredHarness, which matches argv elements exactly.
func processMarkerMatch(fg ProcessInfo, markers []string) string { //nolint:gocritic // hugeParam: ProcessInfo is the decision file's pure-value member shape, examined a handful of times per corroboration round, never a hot loop.
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
// pane gone has no restore pending against the session state that
// process has already persisted; a restart or live handoff changes the
// identity and fails closed here. That persisted state can itself be
// stale: Herdr saves its session snapshot on its own debounce, so a pane
// closed within that window can still be recorded as present, and the
// NEXT restart may restore it — an orphan with no HOP environment and a
// stale incarnation, unable to act through HOP (docs/plan/phase-3-design.md
// section 4's residual).
func ServerContinuityEstablished(recorded, observed string) bool {
	return recorded != "" && recorded == observed
}

// ServerLifetimeVerdict classifies a recorded server-lifetime token against
// a freshly observed one. It is the three-valued reading of the same pair
// ServerContinuityEstablished answers yes or no about: continuity is one
// verdict, a CHANGED lifetime is a POSITIVE fact of its own, and unknown is
// neither.
type ServerLifetimeVerdict string

// Server-lifetime verdicts.
const (
	// LifetimeContinuous: both tokens are non-empty and equal, so the
	// lifetime that served the placement still serves the socket.
	LifetimeContinuous ServerLifetimeVerdict = "continuous"
	// LifetimeChanged: both tokens are non-empty and differ. A socket path
	// is served by one server at a time and lifetimes are contiguous, so
	// this is positive evidence that the server behind the placement is
	// gone — which is also why the placement's own continuity can never be
	// established again.
	LifetimeChanged ServerLifetimeVerdict = "changed"
	// LifetimeUnknown: either token is empty, so the two cannot be
	// compared at all. It is never evidence, in either direction, and it is
	// the verdict for EVERY placement on a platform that implements no
	// lifetime identity.
	LifetimeUnknown ServerLifetimeVerdict = "unknown"
)

// ClassifyServerLifetime is the trigger predicate for the restart rules:
// which of the three verdicts a recorded token and a freshly observed one
// yield. Continuity licenses concluding a placed pane's absence; a CHANGED
// lifetime licenses the restart close (section 6), which is a different act
// with its own conjuncts; unknown licenses neither and keeps the fail-closed
// behavior both rules had before either existed.
func ClassifyServerLifetime(recorded, observed string) ServerLifetimeVerdict {
	switch {
	case recorded == "" || observed == "":
		return LifetimeUnknown
	case recorded == observed:
		return LifetimeContinuous
	default:
		return LifetimeChanged
	}
}

// OccupantMatches reports whether an inspected pane's foreground group
// still contains the recorded occupant: SOME member's pid equals the
// recorded pid AND that same member's argv or cmdline carries the marker
// — the claimed-process-is-among-the-members predicate. The recorded
// occupant spawns its own children (Claude Code's MCP servers) into its
// own process group, so it holds no particular index in the listing; a
// pid match on one member with the marker only on another is never a
// match. The caller is responsible for having resolved the pane by the
// evidence's label first (label identity is not itself observable from
// PaneProcess).
func OccupantMatches(evidence run.OccupantEvidence, pane PaneProcess) bool {
	for _, fg := range pane.Foreground {
		if fg.PID != evidence.PID {
			continue
		}
		if slices.Contains(fg.Argv, evidence.ArgvMarker) || strings.Contains(fg.Cmdline, evidence.ArgvMarker) {
			return true
		}
	}
	return false
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
