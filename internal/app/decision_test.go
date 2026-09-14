package app_test

import (
	"errors"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/run"
)

func TestCorroborateSettlement(t *testing.T) {
	claim := app.LaunchClaim{PID: 100, Executable: "/usr/bin/claude"}
	proc := func(pid int, argv0, name string, argv []string) app.PaneProcess {
		return app.PaneProcess{Foreground: []app.ProcessInfo{{PID: pid, Argv0: argv0, Name: name, Argv: argv}}}
	}

	tests := map[string]struct {
		paneMatches bool
		pane        app.PaneProcess
		claim       app.LaunchClaim
		markers     []string
		want        app.LaunchSettlement
	}{
		"settled: identity, marker and pid all match": {
			paneMatches: true, pane: proc(100, "/usr/bin/claude", "claude", []string{"claude", "attempt-1"}),
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementSettled,
		},
		"settled: the native session reference marker alone corroborates a resume argv": {
			paneMatches: true, pane: proc(100, "/usr/bin/claude", "claude", []string{"claude", "--resume", "native-ref-1"}),
			claim: claim, markers: []string{"attempt-1", "native-ref-1"}, want: app.SettlementSettled,
		},
		"forking wrapper: identity and marker match but pid differs": {
			paneMatches: true, pane: proc(999, "/usr/bin/claude", "claude", []string{"claude", "attempt-1"}),
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementForkingWrapper,
		},
		"unresolved: pane does not match the claim's binding": {
			paneMatches: false, pane: proc(100, "/usr/bin/claude", "claude", []string{"claude", "attempt-1"}),
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementUnresolved,
		},
		"unresolved: no foreground process observed": {
			paneMatches: true, pane: app.PaneProcess{},
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementUnresolved,
		},
		"unresolved: executable identity does not match the claim's executable": {
			// A process that execed another binary while retaining the pid
			// and marker: the claim-recorded executable refuses adoption.
			paneMatches: true, pane: proc(100, "/usr/bin/bash", "bash", []string{"bash", "attempt-1"}),
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementUnresolved,
		},
		"unresolved: marker absent from argv": {
			paneMatches: true, pane: proc(100, "/usr/bin/claude", "claude", []string{"claude"}),
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementUnresolved,
		},
		"unresolved: empty claim executable fails closed": {
			paneMatches: true, pane: proc(100, "", "", []string{"", "attempt-1"}),
			claim: app.LaunchClaim{PID: 100}, markers: []string{"attempt-1"}, want: app.SettlementUnresolved,
		},
		"unresolved: empty marker set fails closed": {
			paneMatches: true, pane: proc(100, "/usr/bin/claude", "claude", []string{"claude", "attempt-1"}),
			claim: claim, markers: nil, want: app.SettlementUnresolved,
		},
		"unresolved: paused pre-exec launcher never satisfies the predicate": {
			// argv0 is the hop binary itself, not the expected harness —
			// the claim's executable identity refuses it.
			paneMatches: true, pane: proc(100, "/usr/bin/hop", "hop", []string{"hop", "launch", "--attempt", "attempt-1"}),
			claim: claim, markers: []string{"attempt-1"}, want: app.SettlementUnresolved,
		},
		"unresolved: launcher invocation is excluded even when the claim names the hop binary": {
			paneMatches: true, pane: proc(100, "/usr/bin/hop", "hop", []string{"hop", "launch", "--run", "run-1", "--attempt", "attempt-1"}),
			claim: app.LaunchClaim{PID: 100, Executable: "/usr/bin/hop"}, markers: []string{"attempt-1"}, want: app.SettlementUnresolved,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := app.CorroborateSettlement(tc.paneMatches, tc.pane, tc.markers, tc.claim)
			if got != tc.want {
				t.Fatalf("CorroborateSettlement() = %s, want %s", got, tc.want)
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
