package integration

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// workerForegroundPID returns the run's claimed worker pid after asserting
// that pid is among paneID's observed foreground-group members. The worker
// is located by the newest launch claim's recorded pid, NEVER by listing
// index: the fixture worker (like real Claude Code) spawns an MCP stand-in
// child into its own process group, and Herdr reports the members in raw
// platform order, so ForegroundProcesses[0] is not the worker.
func (f *fixtureRun) workerForegroundPID(t *testing.T, paneID string) int {
	t.Helper()
	row := querySQLite(t, f.dbPath(), fmt.Sprintf("SELECT pid FROM launch_claims WHERE run_id = '%s' ORDER BY claimed_at DESC, rowid DESC LIMIT 1;", f.runID))
	claimPID, err := strconv.Atoi(row)
	if err != nil || claimPID <= 0 {
		t.Fatalf("launch claim pid for run %s = %q, want a positive pid", f.runID, row)
	}
	info := f.server.processInfo(t, paneID)
	for _, p := range info.ForegroundProcesses {
		if int(p.PID) == claimPID {
			return claimPID
		}
	}
	t.Fatalf("the claimed worker pid %d is not among pane %s's foreground members: %+v", claimPID, paneID, info.ForegroundProcesses)
	return 0
}

// TestRealProcessSettlementWithMCPGroupMembers is the executed probe, under
// the production transport (pane.process_info through HOP's own protocol
// client against a real herdr server), that pins the multi-member
// foreground-group shape the section 6 corroboration predicate must settle
// against — the shape a real Claude Code 2.1.270 launch produces by
// spawning its configured MCP servers (`npm exec <server>`,
// `node .../<server>`, app-installed MCP server binaries) as children in
// ITS OWN process group right after the trust check, observed live in
// TestLiveClaudeDefaultProfileRun on main 3be1748 where the claim stayed
// exec_pending for the whole bound because settlement read only
// Foreground[0], and index 0 was an MCP server.
//
// The fixture worker reproduces that shape by spawning its own long-lived
// MCP stand-in child (same process group, no HOP marker in its argv)
// before reporting ready. The behavior directive is empty, so the worker
// never submits: the run can reach "running" ONLY through the
// corroboration path settling the claim against the multi-member listing —
// the early-acceptance path is never available to mask a stuck settlement.
//
// Ordering: Herdr reports foreground members in raw platform listing
// order — macOS, unsorted proc_listpids (repos/herdr/src/platform/
// macos.rs, foreground_job); Linux, ascending pid
// (repos/herdr/src/platform/linux.rs,
// foreground_process_group_members_with) — mapped verbatim into
// foreground_processes (repos/herdr/src/app/api/panes.rs,
// handle_pane_process_info). Neither order GUARANTEES the launched
// process any index (the live probe observed an MCP server at index 0;
// on Linux a recycled lower pid can precede the leader), so the app must
// never depend on position. This scenario therefore asserts only
// position-free facts — the group is multi-member, the claimed pid is
// among the members, every non-claim member is the marker-free MCP
// stand-in, and the claim settled — and RECORDS the observed order and
// the claimed pid's index for the ledger; the deterministic position
// coverage (claim pid at index 1+, foreign-members-only,
// wrapper-with-matching-child) lives in internal/app's decision-table
// vectors, where the ordering is scripted rather than kernel-supplied.
func TestRealProcessSettlementWithMCPGroupMembers(t *testing.T) {
	fx := startFixtureRun(t, "")
	fields := fx.requireRunState(t, "running")

	claimRow := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state, pid FROM launch_claims WHERE run_id = '%s';", fx.runID))
	claimParts := strings.SplitN(claimRow, "|", 2)
	if len(claimParts) != 2 || claimParts[0] != "execed" {
		t.Fatalf("launch claim row = %q, want state \"execed\" with a pid", claimRow)
	}
	claimPID, err := strconv.Atoi(claimParts[1])
	if err != nil || claimPID <= 0 {
		t.Fatalf("launch claim pid = %q, want a positive pid", claimParts[1])
	}

	observedPath := filepath.Join(fx.stateDir, "runs", fx.runID, "artifacts", "worker-observed.txt")
	observed := readWorkerObservation(t, observedPath)
	workerPID, err := strconv.Atoi(observed.Fields["pid"])
	if err != nil || workerPID <= 0 {
		t.Fatalf("worker observation pid = %q, want a positive pid", observed.Fields["pid"])
	}
	if workerPID != claimPID {
		t.Fatalf("worker pid %d != settled claim pid %d; execve must preserve the launcher's pid", workerPID, claimPID)
	}
	standInPID, err := strconv.Atoi(observed.Fields["mcp_stand_in_pid"])
	if err != nil || standInPID <= 0 {
		t.Fatalf("worker observation mcp_stand_in_pid = %q, want a positive pid", observed.Fields["mcp_stand_in_pid"])
	}

	info := fx.server.processInfo(t, paneIDFromBinding(fields["binding"]))
	memberList := make([]string, 0, len(info.ForegroundProcesses))
	for i, p := range info.ForegroundProcesses {
		memberList = append(memberList, fmt.Sprintf("[%d] pid=%d name=%q argv0=%q argv=%q", i, p.PID, p.Name, p.Argv0, p.Argv))
	}
	// The pinned-shape evidence this scenario exists to record.
	t.Logf("foreground group pgid=%d members:\n%s", info.ForegroundProcessGroup, strings.Join(memberList, "\n"))

	if int(info.ForegroundProcessGroup) != workerPID {
		t.Errorf("foreground process group id = %d, want the worker's own pid %d (the harness is its own group leader and its MCP children join ITS group)", info.ForegroundProcessGroup, workerPID)
	}
	if len(info.ForegroundProcesses) < 2 {
		t.Fatalf("foreground group has %d member(s), want at least 2 (worker + MCP stand-in):\n%s", len(info.ForegroundProcesses), strings.Join(memberList, "\n"))
	}

	workerIdx, standInIdx := -1, -1
	for i, p := range info.ForegroundProcesses {
		switch int(p.PID) {
		case workerPID:
			workerIdx = i
		case standInPID:
			standInIdx = i
		}
	}
	if workerIdx < 0 {
		t.Fatalf("the claimed worker pid %d is not among the foreground members:\n%s", workerPID, strings.Join(memberList, "\n"))
	}
	if standInIdx < 0 {
		t.Fatalf("the MCP stand-in pid %d is not among the foreground members:\n%s", standInPID, strings.Join(memberList, "\n"))
	}
	// Recorded, not asserted: position carries no semantics, so the
	// observed index is ledger evidence only (the live probe saw an MCP
	// server at index 0; a platform change reordering the raw listing must
	// never turn this scenario red).
	t.Logf("claimed worker pid %d observed at foreground index %d of %d members (stand-in at index %d)", workerPID, workerIdx, len(info.ForegroundProcesses), standInIdx)

	worker := info.ForegroundProcesses[workerIdx]
	wantExecutable := filepath.Join(fx.server.base, "bin", "claude")
	if len(worker.Argv) == 0 || worker.Argv[0] != wantExecutable {
		t.Errorf("worker member argv[0] = %q, want the resolved fixture stub %q verbatim", worker.Argv, wantExecutable)
	}
	// Every non-claim member must be the marker-free MCP stand-in: nothing
	// else may share the worker's group, and no stand-in may carry a
	// marker that could corroborate.
	for i, member := range info.ForegroundProcesses {
		if int(member.PID) == workerPID {
			continue
		}
		if len(member.Argv) < 2 || member.Argv[1] != "fixture-mcp-stand-in" {
			t.Errorf("non-claim member [%d] argv = %q, want the MCP stand-in [<worker argv0> fixture-mcp-stand-in]", i, member.Argv)
		}
		for _, marker := range []string{fx.runID, observed.Fields["prompt_assignment_path"]} {
			if marker != "" && strings.Contains(strings.Join(member.Argv, " "), marker) {
				t.Errorf("non-claim member [%d] argv %q carries the marker %q; stand-ins must stay foreign, marker-free members", i, member.Argv, marker)
			}
		}
	}
}
