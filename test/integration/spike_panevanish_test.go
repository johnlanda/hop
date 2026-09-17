package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/herdr"
	"github.com/johnlanda/hop/internal/app"
)

// gatedPaneScript is a command-pane process that stays alive until its
// gate file (argument 1) exists, then exits: the pane's own process, whose
// exit the test controls.
const gatedPaneScript = `while [ ! -e "$1" ]; do sleep 0.1; done`

// vanishingPane is one command pane opened through the production adapter
// and its observed process identity.
type vanishingPane struct {
	PaneID string
	Label  string
	PID    int
	Gate   string
}

// openGatedPane opens a command pane in workspaceID through the production
// Runtime.OpenWorkerPane (layout.apply) whose process is /bin/sh running
// gatedPaneScript, waits for that process to be observed, and pins the
// process relation the launch-ended row relies on: the pane's process is
// the pane's shell pid, leads its own foreground process group, is a
// session leader (portable-pty's setsid(); a session leader can never
// change its group), and is listed in its own group by the platform
// process table — observed through this suite's own listGroupMembers,
// the same `ps -A -o pid=,pgid=` source the production GroupInspector
// reads (whose parsing internal/adapters/process's
// TestGroupInspectorAgainstLiveGroupThenRetirement pins), never through
// the port under test.
func openGatedPane(t *testing.T, runtime *herdr.Runtime, server *testServer, gates, workspaceID, name string) vanishingPane {
	t.Helper()
	pane := vanishingPane{Label: "hop-spike-vanish-" + name + "-" + newSpikeUUID(t), Gate: filepath.Join(gates, name)}
	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	handle, err := runtime.OpenWorkerPane(ctx, app.WorkerPaneRequest{
		WorkspaceID: workspaceID, Cwd: server.workDir(),
		Command: []string{"/bin/sh", "-c", gatedPaneScript, "sh", pane.Gate},
		Label:   pane.Label,
	})
	cancel()
	if err != nil {
		t.Fatalf("OpenWorkerPane(%s): %v", name, err)
	}
	pane.PaneID = handle.PaneID

	var observed app.PaneProcess
	if !waitUntil(func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
		defer cancel()
		info, inspectErr := runtime.InspectPane(ctx, pane.PaneID)
		if inspectErr != nil {
			return false
		}
		observed = info
		// The member is matched by pid and its own unique gate argument, not
		// by argv[0]: macOS's /bin/sh is a shim that execs the selected shell.
		return slices.ContainsFunc(info.Foreground, func(p app.ProcessInfo) bool {
			return p.PID == info.ShellPID && (slices.Contains(p.Argv, pane.Gate) || strings.Contains(p.Cmdline, pane.Gate))
		})
	}) {
		t.Fatalf("pane %s never showed its command as the shell-pid foreground member; last observation %+v", pane.PaneID, observed)
	}
	pane.PID = observed.ShellPID
	if pane.PID <= 1 {
		t.Fatalf("pane %s shell pid = %d, want a real process", pane.PaneID, pane.PID)
	}
	if observed.ForegroundGroupID != pane.PID {
		t.Errorf("pane %s foreground group id = %d, want the command's own pid %d", pane.PaneID, observed.ForegroundGroupID, pane.PID)
	}
	if pgid, pgErr := syscall.Getpgid(pane.PID); pgErr != nil || pgid != pane.PID {
		t.Errorf("getpgid(%d) = %d, %v; want the command leading its own process group", pane.PID, pgid, pgErr)
	}
	if sid, sidErr := syscall.Getsid(pane.PID); sidErr != nil || sid != pane.PID {
		t.Errorf("getsid(%d) = %d, %v; want the command to be a session leader", pane.PID, sid, sidErr)
	}
	members := listGroupMembers(t, pane.PID)
	if !slices.ContainsFunc(members, func(m groupMember) bool { return m.PID == pane.PID }) {
		t.Errorf("group %d members = %+v, want the live command listed in its own group", pane.PID, members)
	}
	t.Logf("pane %s live: shell_pid=%d foreground_group=%d group members=%+v", pane.PaneID, observed.ShellPID, observed.ForegroundGroupID, members)
	return pane
}

// requireVanishedShapes pins what the production adapters and the raw wire
// report for a pane that no longer exists, and that its process is gone
// from its own group.
func requireVanishedShapes(t *testing.T, server *testServer, runtime *herdr.Runtime, presentation *herdr.Presentation, pane *vanishingPane) {
	t.Helper()
	call := func(method string, params any) error {
		ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
		defer cancel()
		return server.client.Call(ctx, method, params, nil)
	}
	ctx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(t.Context(), callTimeout)
	}

	// pane.process_info: code pane_not_found, message "pane not found";
	// the adapter reports app.ErrPaneNotFound.
	msg := requireAPIError(t, call("pane.process_info", map[string]any{"pane_id": pane.PaneID}), "pane_not_found")
	t.Logf("pane.process_info(%s): pane_not_found %q", pane.PaneID, msg)
	if msg != "pane not found" {
		t.Errorf("pane.process_info message = %q, want %q", msg, "pane not found")
	}
	c, cancel := ctx()
	_, err := runtime.InspectPane(c, pane.PaneID)
	cancel()
	if !errors.Is(err, app.ErrPaneNotFound) || !errors.Is(err, herdr.ErrPaneNotFound) {
		t.Errorf("InspectPane(%s) = %v, want app.ErrPaneNotFound and herdr.ErrPaneNotFound", pane.PaneID, err)
	}
	t.Logf("adapter InspectPane: %v", err)

	// The creation label no longer resolves: a successful lookup, not found.
	c, cancel = ctx()
	ref, found, err := runtime.FindPaneByLabel(c, pane.Label)
	cancel()
	if err != nil || found {
		t.Errorf("FindPaneByLabel(%s) = %+v, found=%t, err=%v; want not found, nil error", pane.Label, ref, found, err)
	}

	// pane.report_metadata: code pane_not_found, message "pane <id> not
	// found"; the adapter reports app.ErrPaneNotFound.
	wantMessage := "pane " + pane.PaneID + " not found"
	metadata := map[string]any{"pane_id": pane.PaneID, "source": app.PresentationSource, "tokens": map[string]any{app.FieldRun: "r1"}}
	msg = requireAPIError(t, call("pane.report_metadata", metadata), "pane_not_found")
	t.Logf("pane.report_metadata(%s): pane_not_found %q", pane.PaneID, msg)
	if msg != wantMessage {
		t.Errorf("pane.report_metadata message = %q, want %q", msg, wantMessage)
	}
	c, cancel = ctx()
	err = presentation.ReportMetadata(c, app.PaneMetadata{PaneID: pane.PaneID, Tokens: map[string]string{app.FieldRun: "r1"}})
	cancel()
	if !errors.Is(err, app.ErrPaneNotFound) || !errors.Is(err, herdr.ErrPaneNotFound) {
		t.Errorf("ReportMetadata(%s) = %v, want app.ErrPaneNotFound and herdr.ErrPaneNotFound", pane.PaneID, err)
	}

	// pane.close: code pane_not_found, message "pane <id> not found"; the
	// adapter reports app.ErrPaneNotFound.
	msg = requireAPIError(t, call("pane.close", map[string]any{"pane_id": pane.PaneID}), "pane_not_found")
	t.Logf("pane.close(%s): pane_not_found %q", pane.PaneID, msg)
	if msg != wantMessage {
		t.Errorf("pane.close message = %q, want %q", msg, wantMessage)
	}
	c, cancel = ctx()
	err = runtime.ClosePane(c, pane.PaneID)
	cancel()
	if !errors.Is(err, app.ErrPaneNotFound) || !errors.Is(err, herdr.ErrPaneNotFound) {
		t.Errorf("ClosePane(%s) = %v, want app.ErrPaneNotFound and herdr.ErrPaneNotFound", pane.PaneID, err)
	}

	// The command's process leaves its own group: the process table lists
	// no member with its pid.
	var members []groupMember
	if !waitUntil(func() bool {
		members = listGroupMembers(t, pane.PID)
		return !slices.ContainsFunc(members, func(m groupMember) bool { return m.PID == pane.PID })
	}) {
		t.Errorf("group %d members after the pane vanished = %+v, want no member with the pid", pane.PID, members)
	}
}

// waitPaneGone waits until InspectPane stops answering for pane.
func waitPaneGone(t *testing.T, runtime *herdr.Runtime, pane *vanishingPane) {
	t.Helper()
	var last error
	if !waitUntil(func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
		defer cancel()
		_, last = runtime.InspectPane(ctx, pane.PaneID)
		return last != nil
	}) {
		t.Fatalf("pane %s still answers after its process should have ended", pane.PaneID)
	}
}

// TestSpikeVanishedPaneShapes is the executed probe behind the launch-ended
// decision row and the presentation and close tolerance
// (docs/plan/phase-3-design.md section 4): against a real, disposable
// server and through the production Herdr adapters (herdr.Runtime,
// herdr.Presentation), it pins
//
//   - the process relation: a layout.apply command pane's process is the
//     pane's shell pid and foreground group id, a session leader leading
//     its own group, listed in that group by the process table while it
//     lives;
//   - a pane whose process exits closes by itself, and a pane a human
//     closes takes its process with it;
//   - for such a vanished pane: pane.process_info answers pane_not_found
//     "pane not found", pane.report_metadata and pane.close answer
//     pane_not_found "pane <id> not found", the adapters report each as
//     app.ErrPaneNotFound, the creation label resolves nothing, and the
//     process is gone from its own group.
func TestSpikeVanishedPaneShapes(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)
	runtime := herdr.NewRuntime(server.socketPath)
	presentation := herdr.NewPresentation(server.socketPath)
	gates := artifacts.dir(t, "gates")

	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	workspace, err := runtime.CreateWorkspace(ctx, app.WorkspaceRequest{Cwd: server.workDir(), Label: "hop-spike-vanish-" + newSpikeUUID(t)})
	cancel()
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	t.Run("the pane's process exits", func(t *testing.T) {
		pane := openGatedPane(t, runtime, server, gates, workspace.WorkspaceID, "exit")
		if err := os.WriteFile(pane.Gate, nil, 0o600); err != nil {
			t.Fatalf("open the gate: %v", err)
		}
		waitPaneGone(t, runtime, &pane)
		requireVanishedShapes(t, server, runtime, presentation, &pane)
	})

	t.Run("a human closes the pane", func(t *testing.T) {
		pane := openGatedPane(t, runtime, server, gates, workspace.WorkspaceID, "close")
		ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
		err := runtime.ClosePane(ctx, pane.PaneID)
		cancel()
		if err != nil {
			t.Fatalf("ClosePane(%s) on the live pane: %v", pane.PaneID, err)
		}
		waitPaneGone(t, runtime, &pane)
		requireVanishedShapes(t, server, runtime, presentation, &pane)
	})
}
