package app_test

import (
	"errors"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/run"
)

// TestCorroborateSettlement exercises the executable-identity conjunct
// (executableMatches, unexported) against the real pane.process_info
// surface's platform shapes, confirmed against the Herdr source
// (repos/herdr/src/platform/{macos,linux}.rs): argv[0] is reported
// verbatim; argv0 is a BASENAME on macOS (process_argv0_name) and never
// reported at all on Linux (empty); name is the kernel's short process
// name on both. The claim always records an absolute path, so argv0/name
// are matched against its basename, never the path itself.
//
// It also exercises the member-scan rule over the real multi-member
// foreground-group shape: Claude Code 2.1.270 spawns its configured MCP
// servers into its own process group, and Herdr reports the members in
// raw platform listing order (macOS unsorted proc_listpids, Linux
// ascending pid — repos/herdr/src/platform/macos.rs foreground_job,
// linux.rs foreground_process_group_members_with), so the claimed
// process holds no particular index and index 0 was an MCP server in the
// executed live probe (TestLiveClaudeDefaultProfileRun, claude 2.1.270 on
// herdr 0.9.0). The mcpMember vectors below reproduce that pinned shape.
func TestCorroborateSettlement(t *testing.T) {
	claim := app.LaunchClaim{PID: 100, Executable: "/usr/bin/claude"}
	proc := func(pid int, argv0, name string, argv []string) app.PaneProcess {
		return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: pid, Argv0: argv0, Name: name, Argv: argv}}}
	}
	member := func(pid int, argv0, name string, argv ...string) app.ProcessInfo {
		return app.ProcessInfo{PID: pid, Argv0: argv0, Name: name, Argv: argv}
	}
	// The pinned live MCP-server member shapes: foreign executables, no
	// HOP marker anywhere in argv or cmdline.
	npmMember := member(163, "npm", "npm", "npm", "exec", "@executeautomation/playwright-mcp-server")
	nodeMember := member(230, "node", "node", "/opt/node/bin/node", "/tmp/npx/playwright-mcp-server")

	tests := map[string]struct {
		paneMatches bool
		pane        app.PaneProcess
		claim       app.LaunchClaim
		markers     []string
		want        app.LaunchSettlement
		// wantPID/wantMarker pin the returned SettlementEvidence for the
		// settled and forking-wrapper classifications; both zero means the
		// evidence must be the zero value (unresolved).
		wantPID    int
		wantMarker string
	}{
		"settled: darwin shape — argv0 and name are the claim's basename": {
			paneMatches: true, pane: proc(100, "claude", "claude", []string{"/usr/bin/claude", "attempt-1"}),
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementSettled,
			wantPID: 100, wantMarker: "attempt-1",
		},
		"settled: linux shape — argv0 empty, name alone is the claim's basename": {
			paneMatches: true, pane: proc(100, "", "claude", []string{"/usr/bin/claude", "attempt-1"}),
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementSettled,
			wantPID: 100, wantMarker: "attempt-1",
		},
		"settled: exact argv[0] match alone suffices, even with a rewritten process title": {
			paneMatches: true, pane: proc(100, "some-rewritten-title", "some-rewritten-title", []string{"/usr/bin/claude", "attempt-1"}),
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementSettled,
			wantPID: 100, wantMarker: "attempt-1",
		},
		"settled: the native session reference marker alone corroborates a resume argv": {
			paneMatches: true, pane: proc(100, "claude", "claude", []string{"/usr/bin/claude", "--resume", "native-ref-1"}),
			claim: claim, markers: []string{"attempt-1", "native-ref-1"}, want: app.SettlementSettled,
			wantPID: 100, wantMarker: "native-ref-1",
		},
		"forking wrapper: identity and marker match but pid differs": {
			paneMatches: true, pane: proc(999, "claude", "claude", []string{"/usr/bin/claude", "attempt-1"}),
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementForkingWrapper,
			wantPID: 999, wantMarker: "attempt-1",
		},
		"settled: the claimed member listed LAST behind MCP-server members (the pinned live shape)": {
			paneMatches: true,
			pane: app.PaneProcess{Foreground: []app.ProcessInfo{
				npmMember, nodeMember,
				member(100, "claude", "claude", "/usr/bin/claude", "--session-id", "native-ref-1", "attempt-1"),
			}},
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementSettled,
			wantPID: 100, wantMarker: "attempt-1",
		},
		"settled: claim pid at index 1 behind one foreign member": {
			paneMatches: true,
			pane: app.PaneProcess{Foreground: []app.ProcessInfo{
				npmMember,
				member(100, "claude", "claude", "/usr/bin/claude", "attempt-1"),
			}},
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementSettled,
			wantPID: 100, wantMarker: "attempt-1",
		},
		"unresolved: foreign members only — the claimed process is not in the group": {
			paneMatches: true,
			pane:        app.PaneProcess{Foreground: []app.ProcessInfo{npmMember, nodeMember}},
			claim:       claim, markers: []string{"attempt-1"}, want: app.SettlementUnresolved,
		},
		"unresolved: conjuncts never combine across members — identity on one, marker only on another": {
			paneMatches: true,
			pane: app.PaneProcess{Foreground: []app.ProcessInfo{
				member(100, "claude", "claude", "/usr/bin/claude"),
				member(163, "npm", "npm", "npm", "exec", "attempt-1"),
			}},
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementUnresolved,
		},
		"forking wrapper: a matching member under a different pid fails closed even when the claimed member also matches": {
			// A live wrapper that already exec'd carries the recorded
			// executable and markers in its OWN argv under the claim's pid;
			// a settled-first reading would adopt exactly the topology
			// section 6 refuses, so the wrapper classification wins.
			paneMatches: true,
			pane: app.PaneProcess{Foreground: []app.ProcessInfo{
				member(100, "claude", "claude", "/usr/bin/claude", "attempt-1"),
				member(999, "claude", "claude", "/usr/bin/claude", "attempt-1"),
			}},
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementForkingWrapper,
			wantPID: 999, wantMarker: "attempt-1",
		},
		"settled: a launcher-invocation member is skipped per member, not per pane": {
			paneMatches: true,
			pane: app.PaneProcess{Foreground: []app.ProcessInfo{
				member(99, "hop", "hop", "/usr/bin/hop", "launch", "--run", "run-1", "--attempt", "attempt-1"),
				member(100, "claude", "claude", "/usr/bin/claude", "attempt-1"),
			}},
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementSettled,
			wantPID: 100, wantMarker: "attempt-1",
		},
		"unresolved: pane does not match the claim's binding": {
			paneMatches: false, pane: proc(100, "claude", "claude", []string{"/usr/bin/claude", "attempt-1"}),
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementUnresolved,
		},
		"unresolved: no foreground process observed": {
			paneMatches: true, pane: app.PaneProcess{},
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementUnresolved,
		},
		"unresolved: a different basename never matches": {
			// A process that execed another binary while retaining the pid
			// and marker: the claim-recorded executable refuses adoption.
			paneMatches: true, pane: proc(100, "bash", "bash", []string{"/bin/bash", "attempt-1"}),
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementUnresolved,
		},
		"unresolved: marker absent from argv": {
			paneMatches: true, pane: proc(100, "claude", "claude", []string{"/usr/bin/claude"}),
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementUnresolved,
		},
		"unresolved: empty claim executable fails closed": {
			paneMatches: true, pane: proc(100, "", "", []string{"", "attempt-1"}),
			claim: app.LaunchClaim{PID: 100}, markers: []string{"attempt-1"}, want: app.SettlementUnresolved,
		},
		"unresolved: empty marker set fails closed": {
			paneMatches: true, pane: proc(100, "claude", "claude", []string{"/usr/bin/claude", "attempt-1"}),
			claim: claim, markers: nil, want: app.SettlementUnresolved,
		},
		"unresolved: paused pre-exec launcher never satisfies the predicate": {
			// argv0/name are the hop binary itself, not the expected
			// harness — the claim's executable identity refuses it.
			paneMatches: true, pane: proc(100, "hop", "hop", []string{"/usr/bin/hop", "launch", "--attempt", "attempt-1"}),
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementUnresolved,
		},
		"unresolved: launcher invocation is excluded even when the claim names the hop binary": {
			paneMatches: true, pane: proc(100, "hop", "hop", []string{"/usr/bin/hop", "launch", "--run", "run-1", "--attempt", "attempt-1"}),
			claim: app.LaunchClaim{PID: 100, Executable: "/usr/bin/hop"}, markers: []string{"attempt-1"}, want: app.SettlementUnresolved,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, evidence := app.CorroborateSettlement(tc.paneMatches, tc.pane, tc.markers, tc.claim)
			if got != tc.want {
				t.Fatalf("CorroborateSettlement() = %s, want %s", got, tc.want)
			}
			if evidence.Occupant.PID != tc.wantPID || evidence.Marker != tc.wantMarker {
				t.Fatalf("CorroborateSettlement() evidence = pid %d marker %q, want pid %d marker %q", evidence.Occupant.PID, evidence.Marker, tc.wantPID, tc.wantMarker)
			}
		})
	}
}

// TestFirstMarkerMatch pins the any-member marker predicate: positive
// evidence is accepted from ANY member of the reported foreground group,
// in listing order, since the listing order itself carries no semantics
// (macOS raw proc_listpids order, Linux ascending pid).
func TestFirstMarkerMatch(t *testing.T) {
	tests := map[string]struct {
		pane    app.PaneProcess
		markers []string
		want    string
	}{
		"found on the sole member's argv": {
			pane:    app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 1, Argv: []string{"x", "marker-1"}}}},
			markers: []string{"marker-1"}, want: "marker-1",
		},
		"found on a later member behind a foreign index 0": {
			pane: app.PaneProcess{Foreground: []app.ProcessInfo{
				{PID: 1, Argv: []string{"npm", "exec", "some-mcp-server"}},
				{PID: 2, Cmdline: "/usr/bin/claude --resume marker-1"},
			}},
			markers: []string{"marker-1"}, want: "marker-1",
		},
		"the first bearing member's first matching marker wins": {
			pane: app.PaneProcess{Foreground: []app.ProcessInfo{
				{PID: 1, Argv: []string{"x", "marker-2"}},
				{PID: 2, Argv: []string{"y", "marker-1"}},
			}},
			markers: []string{"marker-1", "marker-2"}, want: "marker-2",
		},
		"absent from every member": {
			pane: app.PaneProcess{Foreground: []app.ProcessInfo{
				{PID: 1, Argv: []string{"x"}},
				{PID: 2, Argv: []string{"y"}},
			}},
			markers: []string{"marker-1"}, want: "",
		},
		"empty markers never match": {
			pane:    app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 1, Argv: []string{"x"}}}},
			markers: []string{""}, want: "",
		},
		"no foreground members": {
			pane:    app.PaneProcess{},
			markers: []string{"marker-1"}, want: "",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := app.FirstMarkerMatch(tc.pane, tc.markers); got != tc.want {
				t.Fatalf("FirstMarkerMatch() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOccupantMatches(t *testing.T) {
	evidence := run.OccupantEvidence{Label: "label-1", ArgvMarker: "marker-1", PID: 42}

	tests := map[string]struct {
		pane app.PaneProcess
		want bool
	}{
		"matches: pid and marker both present": {
			pane: app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 42, Argv: []string{"x", "marker-1"}}}},
			want: true,
		},
		"matches via cmdline when argv split differs": {
			pane: app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 42, Cmdline: "x marker-1 y"}}},
			want: true,
		},
		"no match: pid differs": {
			pane: app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 43, Argv: []string{"marker-1"}}}},
			want: false,
		},
		"no match: marker absent": {
			pane: app.PaneProcess{Foreground: []app.ProcessInfo{{PID: 42, Argv: []string{"other"}}}},
			want: false,
		},
		"no match: no foreground process": {
			pane: app.PaneProcess{},
			want: false,
		},
		"matches: the recorded occupant listed behind its own MCP children": {
			pane: app.PaneProcess{Foreground: []app.ProcessInfo{
				{PID: 63, Argv: []string{"npm", "exec", "some-mcp-server"}},
				{PID: 42, Argv: []string{"x", "marker-1"}},
			}},
			want: true,
		},
		"no match: pid on one member, marker only on another": {
			pane: app.PaneProcess{Foreground: []app.ProcessInfo{
				{PID: 42, Argv: []string{"x"}},
				{PID: 63, Argv: []string{"y", "marker-1"}},
			}},
			want: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := app.OccupantMatches(evidence, tc.pane); got != tc.want {
				t.Fatalf("OccupantMatches() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClassifyGroupRetirement(t *testing.T) {
	expected := []string{"sh", "check.sh"}
	spawn := []string{"/usr/local/bin/hop", "check-exec", "--op", "op-1", "--", "sh", "check.sh"}
	listErr := errors.New("boom")

	tests := map[string]struct {
		processes []app.GroupProcess
		err       error
		want      app.GroupRetirementOutcome
	}{
		"inspection failed: listing itself errored": {
			processes: []app.GroupProcess{{PID: 1, Argv: expected}}, err: listErr,
			want: app.GroupInspectionFailed,
		},
		"empty: group already gone": {
			processes: nil, want: app.GroupEmpty,
		},
		"matched: a member's argv equals the expected argv": {
			processes: []app.GroupProcess{{PID: 1, Argv: []string{"sleep", "100000"}}, {PID: 2, Argv: expected}},
			want:      app.GroupMatched,
		},
		"mismatched: non-empty, every member readable, none matches": {
			processes: []app.GroupProcess{{PID: 1, Argv: []string{"sleep", "100000"}}, {PID: 2, Argv: []string{"other"}}},
			want:      app.GroupMismatched,
		},
		"anchor-only: the sole live member is the supervisory sleep anchor": {
			processes: []app.GroupProcess{{PID: 1, Argv: []string{"/usr/bin/sleep", "100000"}}},
			want:      app.GroupMatched,
		},
		"inspection failed: an unreadable member with no confirmed match or mismatch": {
			processes: []app.GroupProcess{{PID: 1, Argv: []string{"sleep", "100000"}}, {PID: 2, Argv: []string{app.ArgvUnavailable}}},
			want:      app.GroupInspectionFailed,
		},
		"matched still wins over an unreadable sibling member": {
			processes: []app.GroupProcess{{PID: 1, Argv: []string{app.ArgvUnavailable}}, {PID: 2, Argv: expected}},
			want:      app.GroupMatched,
		},
		"matched: a paused pre-exec check-exec invocation is owned work": {
			processes: []app.GroupProcess{{PID: 1, Argv: spawn}},
			want:      app.GroupMatched,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := app.ClassifyGroupRetirement(tc.processes, tc.err, [][]string{expected, spawn})
			if got != tc.want {
				t.Fatalf("ClassifyGroupRetirement() = %s, want %s", got, tc.want)
			}
		})
	}
}
