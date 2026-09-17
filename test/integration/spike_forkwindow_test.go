package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/herdr"
	"github.com/johnlanda/hop/internal/app"
)

// forkWindowLauncherScript is the pane's own command. It execs bash under
// the name the launch claim records (`exec -a <executable>`), so the pane's
// own process — the process a claim's pid names — carries the launch
// identity in argv[0] and the launch markers inside its prompt argument,
// exactly as a landed harness does.
const forkWindowLauncherScript = `exec -a "$HOP_SPIKE_EXEC" /bin/bash "$HOP_SPIKE_HOLD" --session-id "$HOP_SPIKE_REF" "$HOP_SPIKE_PROMPT"`

// forkWindowHoldScript holds a same-image child between fork and exec for
// as long as the observer wants. bash forks for the async list; the forked
// child announces itself by creating the ready file and then blocks in the
// open() of its next redirection's gate FIFO, which no writer has opened
// yet. Until a writer appears the child is still the forked image — the
// parent's argv, argv0, name and cmdline under its own pid — and has
// exec'd nothing. Opening the gate releases it into an exec of a stand-in
// under a marker-free argv of its own, in the same pid.
//
// Go cannot hold a child in this window (between fork and exec os/exec's
// child runs raw syscalls only) and cgo would add a C toolchain, so bash's
// `exec -a` (argv[0] control) and its real same-image async fork are what
// make the window both reachable and deterministic.
const forkWindowHoldScript = `{ : >"$HOP_SPIKE_READY"; read -r _ <"$HOP_SPIKE_GATE"; exec -a mcp-stand-in /bin/sleep 600; } &
wait`

// forkWindowHeldSamples is how many back-to-back inspections the probe
// takes while the child is held: one sample classifying a wrapper would be
// unremarkable, the classification HOLDING across every sample of a live
// fork window is the observation the decision rule rests on.
const forkWindowHeldSamples = 20

// forkWindowStandInArgv0 is the argv[0] the released child execs under: a
// name that carries neither the claim's executable identity nor any launch
// marker, so the released observation is a clean one.
const forkWindowStandInArgv0 = "mcp-stand-in"

// TestSpikeForkWindowSameImageChild is the executed probe behind the live
// launch corroboration's revisit rule (docs/plan/phase-3-design.md section
// 6): what a legitimate same-image child looks like to
// `pane.process_info` while it sits in its fork-before-exec window, and
// what the corroboration predicate makes of it.
//
// Under the production transport (herdr.Runtime against a disposable
// server) a command pane runs bash exec'd under the claim's own recorded
// executable name, carrying the launch markers in its argument list. That
// process forks one same-image child and holds it, unexec'd, on a gate the
// test owns. In the held phase the probe pins, for that child: a pid of
// its own, and argv, argv0, name and cmdline all equal to its parent's; a
// place in the same foreground process group, confirmed independently
// through the platform process table; and CorroborateSettlement
// classifying the observation SettlementForkingWrapper, naming that child
// as the occupant, on every sample — even though the claim's own pid is
// present in the same group and matches every conjunct itself. Releasing
// the gate execs the child into a marker-free stand-in under the SAME pid,
// and the very next observation of the same unchanged pane settles.
//
// So the wrapper classification of a live launch is not evidence about the
// launch at all: it can be a transient fact about one sample, which a
// later sample of the same pane contradicts.
func TestSpikeForkWindowSameImageChild(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)
	runtime := herdr.NewRuntime(server.socketPath)

	scripts := artifacts.dir(t, "fork-window")
	ready := filepath.Join(scripts, "ready")
	gate := filepath.Join(scripts, "gate")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatalf("create the fork-window gate fifo: %v", err)
	}
	release := forkWindowGateRelease(gate)
	// Registered before the pane exists: every failure path below releases
	// the held child rather than leaving it blocked in its own open().
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Errorf("release the held child during cleanup: %v", err)
		}
	})
	hold := filepath.Join(scripts, "hold.sh")
	writeForkWindowScript(t, hold, forkWindowHoldScript)
	launcher := filepath.Join(scripts, "launcher.sh")
	writeForkWindowScript(t, launcher, forkWindowLauncherScript)

	// The claim's recorded executable. It is never executed — argv[0] is
	// spoofed to it, and the predicate compares recorded strings — and it
	// is a plain non-executable file under this test's own artifact
	// directory, never a resolved harness on PATH.
	claimExecutable := filepath.Join(artifacts.dir(t, "fork-window-bin"), "claude")
	if err := os.WriteFile(claimExecutable, []byte("# never executed: this probe only spoofs argv[0] to this path\n"), 0o600); err != nil {
		t.Fatalf("write the claim executable stand-in: %v", err)
	}

	marker := newSpikeUUID(t)
	nativeRef := newSpikeUUID(t)
	markers := []string{marker}
	label := "hop-spike-forkwindow-" + marker[:8]

	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	workspace, err := runtime.CreateWorkspace(ctx, app.WorkspaceRequest{Cwd: server.workDir(), Label: label + "-ws"})
	cancel()
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	ctx, cancel = context.WithTimeout(t.Context(), callTimeout)
	handle, err := runtime.OpenWorkerPane(ctx, app.WorkerPaneRequest{
		WorkspaceID: workspace.WorkspaceID,
		Cwd:         server.workDir(),
		Command:     []string{"/bin/bash", launcher},
		Env: map[string]string{
			"HOP_SPIKE_EXEC":   claimExecutable,
			"HOP_SPIKE_HOLD":   hold,
			"HOP_SPIKE_READY":  ready,
			"HOP_SPIKE_GATE":   gate,
			"HOP_SPIKE_REF":    nativeRef,
			"HOP_SPIKE_PROMPT": "read the assignment for run " + marker + " and report when done",
		},
		Label: label,
	})
	cancel()
	if err != nil {
		t.Fatalf("OpenWorkerPane: %v", err)
	}

	if !waitUntil(func() bool {
		_, statErr := os.Stat(ready)
		return statErr == nil
	}) {
		t.Fatalf("the held child never announced itself; pane %s scrollback:\n%s", handle.PaneID, server.readPane(t, handle.PaneID))
	}

	// The held phase: the parent is the pane's own process (the process a
	// claim's pid names), the child is anything else in the group.
	var held app.PaneProcess
	var parent, child app.ProcessInfo
	if !waitUntil(func() bool {
		observed, inspectErr := inspectForkWindowPane(t, runtime, handle.PaneID)
		if inspectErr != nil {
			return false
		}
		p, c, ok := forkWindowMembers(&observed)
		if !ok {
			return false
		}
		held, parent, child = observed, p, c
		return true
	}) {
		t.Fatalf("pane %s never showed its own process plus one other foreground member; last observation %+v", handle.PaneID, held)
	}
	artifacts.save(t, "fork-window-held.txt", forkWindowDump(&held))
	// Logged as well as retained: a passing probe's whole product is the
	// values it observed, and a passing run keeps no artifact directory.
	t.Logf("held observation:\n%s", forkWindowDump(&held))

	claim := app.LaunchClaim{Executable: claimExecutable, PID: held.ShellPID}
	if parent.PID != held.ShellPID {
		t.Fatalf("the pane's own process is pid %d, want the shell pid %d", parent.PID, held.ShellPID)
	}
	if child.PID == parent.PID {
		t.Fatalf("the held child reports the parent's pid %d; it never forked", child.PID)
	}

	// What the held child looks like: its parent's process identity under
	// its own pid, in every field pane.process_info reports identity by.
	if !slices.Equal(child.Argv, parent.Argv) {
		t.Errorf("held child argv = %q, want the parent's %q", child.Argv, parent.Argv)
	}
	if child.Argv0 != parent.Argv0 {
		t.Errorf("held child argv0 = %q, want the parent's %q", child.Argv0, parent.Argv0)
	}
	if child.Name != parent.Name {
		t.Errorf("held child name = %q, want the parent's %q", child.Name, parent.Name)
	}
	if child.Cmdline != parent.Cmdline {
		t.Errorf("held child cmdline = %q, want the parent's %q", child.Cmdline, parent.Cmdline)
	}
	t.Logf("held child pid=%d argv0=%q name=%q cwd=%q; parent pid=%d cwd=%q", child.PID, child.Argv0, child.Name, child.Cwd, parent.PID, parent.Cwd)

	// And where it sits: the pane's own foreground group, confirmed against
	// the platform process table rather than the port under test.
	if held.ForegroundGroupID != held.ShellPID {
		t.Errorf("pane foreground group id = %d, want the pane's own process %d", held.ForegroundGroupID, held.ShellPID)
	}
	members := listGroupMembers(t, held.ShellPID)
	for _, want := range []int{parent.PID, child.PID} {
		if !slices.ContainsFunc(members, func(m groupMember) bool { return m.PID == want }) {
			t.Errorf("process group %d members = %+v, want pid %d listed", held.ShellPID, members, want)
		}
	}
	t.Logf("foreground group %d members: %+v", held.ShellPID, members)

	// The classification, on every sample while the window is held: the
	// different-pid member decides, even though the claim's own pid is
	// present in the same group and satisfies every conjunct itself.
	if !app.ClaimProcessMatches(held, markers, claim) {
		t.Fatalf("the claimed process itself does not match its own claim; the probe proves nothing about wrapper precedence. Observation:\n%s", forkWindowDump(&held))
	}
	for i := range forkWindowHeldSamples {
		observed, inspectErr := inspectForkWindowPane(t, runtime, handle.PaneID)
		if inspectErr != nil {
			t.Fatalf("held sample %d: InspectPane: %v", i, inspectErr)
		}
		settlement, occupant := app.CorroborateSettlement(true, observed, markers, claim)
		if settlement != app.SettlementForkingWrapper {
			t.Fatalf("held sample %d: CorroborateSettlement = %q, want %q while a same-image child sits in its fork window. Observation:\n%s",
				i, settlement, app.SettlementForkingWrapper, forkWindowDump(&observed))
		}
		if occupant.Occupant.PID != child.PID {
			t.Errorf("held sample %d: wrapper occupant pid = %d, want the held child %d", i, occupant.Occupant.PID, child.PID)
		}
		if occupant.Marker != marker {
			t.Errorf("held sample %d: wrapper occupant marker = %q, want %q", i, occupant.Marker, marker)
		}
		time.Sleep(pollInterval)
	}

	// Releasing the gate execs the held child, in the same pid, into a
	// stand-in carrying neither the claim's executable identity nor any
	// marker. Nothing else about the pane changes.
	if err := release(); err != nil {
		t.Fatalf("release the held child: %v", err)
	}
	var settled app.PaneProcess
	if !waitUntil(func() bool {
		observed, inspectErr := inspectForkWindowPane(t, runtime, handle.PaneID)
		if inspectErr != nil {
			return false
		}
		if !slices.ContainsFunc(observed.Foreground, func(p app.ProcessInfo) bool {
			return p.PID == child.PID && p.Argv0 == forkWindowStandInArgv0
		}) {
			return false
		}
		settled = observed
		return true
	}) {
		t.Fatalf("the released child never exec'd the stand-in under pid %d; last observation %+v", child.PID, settled)
	}
	artifacts.save(t, "fork-window-released.txt", forkWindowDump(&settled))
	t.Logf("released observation:\n%s", forkWindowDump(&settled))

	if settled.ShellPID != held.ShellPID {
		t.Errorf("pane's own process changed from %d to %d across the release", held.ShellPID, settled.ShellPID)
	}
	settlement, occupant := app.CorroborateSettlement(true, settled, markers, claim)
	if settlement != app.SettlementSettled {
		t.Fatalf("after the release CorroborateSettlement = %q, want %q. Observation:\n%s", settlement, app.SettlementSettled, forkWindowDump(&settled))
	}
	if occupant.Occupant.PID != claim.PID {
		t.Errorf("settled occupant pid = %d, want the claim's own %d", occupant.Occupant.PID, claim.PID)
	}
}

// forkWindowGateRelease returns the idempotent release of the held child:
// it opens the gate FIFO O_RDWR — which never blocks, whether or not a
// reader is still waiting — and writes the line the child's `read` is
// blocked on. Idempotent because both the test body and its cleanup call
// it, and safe on a child that already exec'd, where the write simply goes
// nowhere.
func forkWindowGateRelease(gate string) func() error {
	var (
		once sync.Once
		err  error
	)
	return func() error {
		once.Do(func() {
			var f *os.File
			f, err = os.OpenFile(gate, os.O_RDWR, 0) //nolint:gosec // G304: the gate FIFO this test created under its own artifact directory.
			if err != nil {
				return
			}
			if _, err = f.WriteString("go\n"); err != nil {
				_ = f.Close() //nolint:errcheck // the write error is the failure worth reporting.
				return
			}
			err = f.Close()
		})
		return err
	}
}

// writeForkWindowScript writes one of this probe's bash scripts under the
// test's own artifact directory.
func writeForkWindowScript(t *testing.T, path, source string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(source+"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", filepath.Base(path), err)
	}
}

// inspectForkWindowPane takes one observation through the production
// adapter under the suite's call timeout.
func inspectForkWindowPane(t *testing.T, runtime *herdr.Runtime, paneID string) (app.PaneProcess, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	defer cancel()
	return runtime.InspectPane(ctx, paneID)
}

// forkWindowMembers splits an observation into the pane's own process and
// the one other foreground member, reporting false until the group holds
// exactly those two. Members are matched by pid against the pane's own
// shell pid, never by listing position, which carries no semantics.
func forkWindowMembers(pane *app.PaneProcess) (parent, child app.ProcessInfo, ok bool) {
	if pane.ShellPID <= 1 || len(pane.Foreground) != 2 {
		return parent, child, false
	}
	for _, fg := range pane.Foreground {
		if fg.PID == pane.ShellPID {
			parent = fg
			continue
		}
		child = fg
	}
	return parent, child, parent.PID != 0 && child.PID != 0
}

// forkWindowDump renders one observation as retained evidence and as
// failure detail.
func forkWindowDump(pane *app.PaneProcess) string {
	dump := fmt.Sprintf("shell_pid=%d foreground_group=%d server_instance=%s\n", pane.ShellPID, pane.ForegroundGroupID, pane.ServerInstance)
	for _, fg := range pane.Foreground {
		dump += fmt.Sprintf("  pid=%d name=%q argv0=%q argv=%q cmdline=%q cwd=%q\n", fg.PID, fg.Name, fg.Argv0, fg.Argv, fg.Cmdline, fg.Cwd)
	}
	return dump
}
