package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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
	command := []string{"/bin/sh", "-c", gatedPaneScript, "sh", pane.Gate}
	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	handle, err := runtime.OpenWorkerPane(ctx, app.WorkerPaneRequest{
		WorkspaceID: workspaceID, Cwd: server.workDir(),
		Command: command,
		Label:   pane.Label,
	})
	cancel()
	if err != nil {
		t.Fatalf("OpenWorkerPane(%s): %v", name, err)
	}
	pane.PaneID = handle.PaneID
	// The creation label answers on the very first lookup after the
	// create returns — no wait — and for this pane alone.
	requireLabelResolves(t, runtime, &pane, "the first lookup after OpenWorkerPane returned")

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
	// The pane's own process reports exactly the argv layout.apply ran it
	// with (resume's in-flight rule compares a pre-claim launcher's argv
	// with the frozen pane.open command element by element).
	if !slices.ContainsFunc(observed.Foreground, func(p app.ProcessInfo) bool { return p.PID == pane.PID && slices.Equal(p.Argv, command) }) {
		t.Errorf("pane %s foreground = %+v, want the member at shell pid %d reporting argv %q exactly", pane.PaneID, observed.Foreground, pane.PID, command)
	}
	// The inspection is stamped with the lifetime of the server that
	// answered it, which is the lifetime ServerInstance reports.
	if want := requireServerLifetime(t, runtime, server, "while the pane lives"); observed.ServerInstance != want {
		t.Errorf("pane %s inspection stamped %q, want the answering server's %q", pane.PaneID, observed.ServerInstance, want)
	}
	members := listGroupMembers(t, pane.PID)
	if !slices.ContainsFunc(members, func(m groupMember) bool { return m.PID == pane.PID }) {
		t.Errorf("group %d members = %+v, want the live command listed in its own group", pane.PID, members)
	}
	t.Logf("pane %s live: shell_pid=%d foreground_group=%d group members=%+v", pane.PaneID, observed.ShellPID, observed.ForegroundGroupID, members)

	// While the pane lives, every lookup answers for it: sampled back to
	// back for labelContinuitySamples lookups, none misses.
	for i := range labelContinuitySamples {
		requireLabelResolves(t, runtime, &pane, fmt.Sprintf("continuity sample %d while the pane lives", i))
		time.Sleep(pollInterval)
	}
	return pane
}

// labelContinuitySamples is how many back-to-back label lookups the probe
// takes while a pane lives (about three seconds at pollInterval).
const labelContinuitySamples = 120

// requireLabelResolves asserts one FindPaneByLabel lookup, through the
// production adapter, succeeds and answers exactly pane's own id.
func requireLabelResolves(t *testing.T, runtime *herdr.Runtime, pane *vanishingPane, when string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	defer cancel()
	ref, found, err := runtime.FindPaneByLabel(ctx, pane.Label)
	if err != nil || !found || ref.PaneID != pane.PaneID {
		t.Fatalf("FindPaneByLabel(%s) at %s = %+v, found=%t, err=%v; want pane %s", pane.Label, when, ref, found, err, pane.PaneID)
	}
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
//   - label visibility: the creation label answers for the pane on the
//     first lookup after the create returns and on every back-to-back
//     lookup while the pane lives (the label-only launch-ended row reads a
//     label that stopped answering as the pane being gone);
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

// TestSpikeLabelSurvivesRestart is the restore half of the label-visibility
// probe behind the label-only launch-ended row (docs/plan/phase-3-design.md
// section 4): against a real, disposable server and through the production
// herdr.Runtime, a server restarted gracefully on the same roots answers
// the FIRST label lookup it serves with the restored pane — Herdr restores
// the persisted session before it serves any request — so a label that
// stops resolving never stems from a restore window. The pane's original
// process does not survive into the restored pane (a cold restore starts a
// fresh shell), which is exactly the shape where only the label keeps an
// unplaced launch from being read as ended. It also pins the two facts
// resume's in-flight rule reads after a restart: the restored pane keeps
// its public pane id (so "the recorded id still answers" is no evidence of
// the original occupant), and Runtime.ServerInstance is non-empty on both
// sides of the restart and differs across it (so server continuity is
// what tells a restarted server apart).
func TestSpikeLabelSurvivesRestart(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)
	runtime := herdr.NewRuntime(server.socketPath)
	gates := artifacts.dir(t, "gates")

	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	workspace, err := runtime.CreateWorkspace(ctx, app.WorkspaceRequest{Cwd: server.workDir(), Label: "hop-spike-restart-" + newSpikeUUID(t)})
	cancel()
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	pane := openGatedPane(t, runtime, server, gates, workspace.WorkspaceID, "restart")
	// The gate releases the original command should it outlive its server.
	t.Cleanup(func() {
		if gateErr := os.WriteFile(pane.Gate, nil, 0o600); gateErr != nil {
			t.Errorf("open the gate: %v", gateErr)
		}
	})

	before := requireServerLifetime(t, runtime, server, "before the restart")

	server.restart(t)

	// The first lookup after the restarted server answers ping, with no
	// wait: the restored pane already carries the creation label.
	ctx, cancel = context.WithTimeout(t.Context(), callTimeout)
	ref, found, err := runtime.FindPaneByLabel(ctx, pane.Label)
	cancel()
	if err != nil || !found {
		t.Fatalf("first FindPaneByLabel(%s) after the restart = %+v, found=%t, err=%v; want the restored pane", pane.Label, ref, found, err)
	}
	t.Logf("restored pane for label %s: %s (created as %s)", pane.Label, ref.PaneID, pane.PaneID)
	// A graceful restart keeps public pane ids: the restored pane answers
	// under the id it was created with.
	if ref.PaneID != pane.PaneID {
		t.Errorf("restored pane id = %s, want the created id %s (public pane ids survive a graceful restart)", ref.PaneID, pane.PaneID)
	}
	// The server lifetime changes across the restart, so a recorded
	// creation-time token never matches the restarted server.
	after := requireServerLifetime(t, runtime, server, "after the restart")
	if goruntime.GOOS == "darwin" && after == before {
		t.Errorf("ServerInstance after the restart = %q, the same as before; want a different server lifetime", after)
	}
	t.Logf("server instance before the restart %q, after %q", before, after)

	// The restored pane stays resolvable on every later lookup too.
	for i := range labelContinuitySamples {
		sampleCtx, sampleCancel := context.WithTimeout(t.Context(), callTimeout)
		again, againFound, againErr := runtime.FindPaneByLabel(sampleCtx, pane.Label)
		sampleCancel()
		if againErr != nil || !againFound || again.PaneID != ref.PaneID {
			t.Fatalf("FindPaneByLabel(%s) sample %d after the restart = %+v, found=%t, err=%v; want %s", pane.Label, i, again, againFound, againErr, ref.PaneID)
		}
		time.Sleep(pollInterval)
	}

	// The restored pane is a fresh process, never the original command.
	ctx, cancel = context.WithTimeout(t.Context(), callTimeout)
	restored, err := runtime.InspectPane(ctx, ref.PaneID)
	cancel()
	if err != nil {
		t.Fatalf("InspectPane(%s) on the restored pane: %v", ref.PaneID, err)
	}
	if restored.ShellPID == pane.PID {
		t.Errorf("restored pane shell pid = %d, the original command's pid; want a fresh process", restored.ShellPID)
	}
	if restored.ServerInstance != after {
		t.Errorf("restored pane inspection stamped %q, want the restarted server's %q", restored.ServerInstance, after)
	}
	var members []groupMember
	gone := waitUntil(func() bool {
		members = listGroupMembers(t, pane.PID)
		return !slices.ContainsFunc(members, func(m groupMember) bool { return m.PID == pane.PID })
	})
	t.Logf("restored pane shell_pid=%d; original command pid %d gone from its group: %t (members %+v)", restored.ShellPID, pane.PID, gone, members)
}

// lifetimeStabilitySamples is how many back-to-back ServerInstance reads
// the probe compares within one server lifetime.
const lifetimeStabilitySamples = 20

// serverLifetimePattern is the production token format on darwin:
// "herdr-server-lifetime/v1 pid=<pid> start=<sec>.<usec>".
const serverLifetimePattern = `^herdr-server-lifetime/v1 pid=([1-9][0-9]*) start=([1-9][0-9]*)\.([0-9]{6})$`

// requireServerLifetime reads the production adapter's server-lifetime
// token and pins it against the server this suite launched: on darwin it
// names the running server leader's pid and that process's start second as
// ps independently reports it, and it is identical on every back-to-back
// read within the lifetime; every other platform implements no lifetime
// identity, so the token is empty there.
func requireServerLifetime(t *testing.T, runtime *herdr.Runtime, server *testServer, when string) string {
	t.Helper()
	read := func() string {
		ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
		defer cancel()
		instance, err := runtime.ServerInstance(ctx)
		if err != nil {
			t.Fatalf("ServerInstance %s: %v", when, err)
		}
		return instance
	}
	token := read()
	if goruntime.GOOS != "darwin" {
		if token != "" {
			t.Fatalf("ServerInstance %s = %q, want \"\" (no lifetime identity is implemented on %s)", when, token, goruntime.GOOS)
		}
		return token
	}
	match := regexp.MustCompile(serverLifetimePattern).FindStringSubmatch(token)
	if match == nil {
		t.Fatalf("ServerInstance %s = %q, want %s", when, token, serverLifetimePattern)
	}
	leader := server.running[len(server.running)-1].leaderCmd.Process.Pid
	if match[1] != strconv.Itoa(leader) {
		t.Errorf("ServerInstance %s names pid %s, want the running server leader's pid %d", when, match[1], leader)
	}
	if started := psStartSecond(t, leader); match[2] != strconv.FormatInt(started, 10) {
		t.Errorf("ServerInstance %s start second = %s, ps reports %d for the server leader %d", when, match[2], started, leader)
	}
	for i := range lifetimeStabilitySamples {
		if again := read(); again != token {
			t.Fatalf("ServerInstance %s sample %d = %q, want the lifetime's one token %q", when, i, again, token)
		}
	}
	return token
}

// psStartSecond reads pid's start time, to the second, from ps's lstart
// column: an independent source to the adapter's kinfo_proc read.
func psStartSecond(t *testing.T, pid int) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // G204: the fixed ps binary; the one variable argument is a decimal pid.
	if err != nil {
		t.Fatalf("ps -o lstart= -p %d: %v", pid, err)
	}
	started, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.TrimSpace(string(out)), time.Local)
	if err != nil {
		t.Fatalf("parse ps lstart %q: %v", out, err)
	}
	return started.Unix()
}

// renamePane relabels paneID through Herdr's own pane.rename, the one live
// relabelling a human performs (it rewrites the same manual label a
// layout.apply creation label is).
func renamePane(t *testing.T, server *testServer, paneID, label string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	defer cancel()
	params := map[string]string{"pane_id": paneID, "label": label}
	if err := server.client.Call(ctx, "pane.rename", params, nil); err != nil {
		t.Fatalf("pane.rename(%s, %s): %v", paneID, label, err)
	}
}

// requireLabelAnswersNothing asserts one FindPaneByLabel lookup, through
// the production adapter, succeeds and finds no pane.
func requireLabelAnswersNothing(t *testing.T, runtime *herdr.Runtime, label, when string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	defer cancel()
	ref, found, err := runtime.FindPaneByLabel(ctx, label)
	if err != nil || found {
		t.Fatalf("FindPaneByLabel(%s) %s = %+v, found=%t, err=%v; want a successful lookup finding nothing", label, when, ref, found, err)
	}
}

// TestSpikeRenamedLabelSurvivesRestart pins the shape behind the launch-
// ended rows' server-continuity conjunct (docs/plan/phase-3-design.md
// section 4), against a real, disposable server through the production
// herdr.Runtime: a human's pane.rename replaces the creation label, so
// while the pane lives the creation label answers nothing and the new name
// answers the pane, whose original process still runs; after a graceful
// restart the MANUAL label survives — the renamed pane is restored under
// its new name, keeping its public id, with a fresh process — while the
// creation label still resolves nothing, and the server lifetime has
// changed. A label that answers nothing after a restart therefore never
// proves the pane gone; only an unchanged server lifetime does.
func TestSpikeRenamedLabelSurvivesRestart(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)
	runtime := herdr.NewRuntime(server.socketPath)
	gates := artifacts.dir(t, "gates")

	ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
	workspace, err := runtime.CreateWorkspace(ctx, app.WorkspaceRequest{Cwd: server.workDir(), Label: "hop-spike-rename-" + newSpikeUUID(t)})
	cancel()
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	pane := openGatedPane(t, runtime, server, gates, workspace.WorkspaceID, "rename")
	t.Cleanup(func() {
		if gateErr := os.WriteFile(pane.Gate, nil, 0o600); gateErr != nil {
			t.Errorf("open the gate: %v", gateErr)
		}
	})
	before := requireServerLifetime(t, runtime, server, "before the rename")

	renamed := "human-renamed-" + newSpikeUUID(t)
	renamePane(t, server, pane.PaneID, renamed)

	// While the pane lives under one server lifetime: the creation label
	// answers nothing, the new name answers the pane, and the pane keeps
	// its original process — the process conjunct still observes it.
	requireLabelAnswersNothing(t, runtime, pane.Label, "after the rename")
	requireLabelResolves(t, runtime, &vanishingPane{Label: renamed, PaneID: pane.PaneID}, "the renamed label after the rename")
	ctx, cancel = context.WithTimeout(t.Context(), callTimeout)
	live, err := runtime.InspectPane(ctx, pane.PaneID)
	cancel()
	if err != nil {
		t.Fatalf("InspectPane(%s) after the rename: %v", pane.PaneID, err)
	}
	if live.ShellPID != pane.PID || live.ServerInstance != before {
		t.Errorf("renamed pane shell pid = %d stamped %q, want the original %d under %q", live.ShellPID, live.ServerInstance, pane.PID, before)
	}
	if members := listGroupMembers(t, pane.PID); !slices.ContainsFunc(members, func(m groupMember) bool { return m.PID == pane.PID }) {
		t.Errorf("group %d members = %+v, want the renamed pane's process still listed", pane.PID, members)
	}

	server.restart(t)

	// The first lookups the restarted server serves: the manual label
	// answers the restored pane under its created id; the creation label
	// answers nothing.
	ctx, cancel = context.WithTimeout(t.Context(), callTimeout)
	ref, found, err := runtime.FindPaneByLabel(ctx, renamed)
	cancel()
	if err != nil || !found || ref.PaneID != pane.PaneID {
		t.Fatalf("FindPaneByLabel(%s) after the restart = %+v, found=%t, err=%v; want the renamed pane restored as %s", renamed, ref, found, err, pane.PaneID)
	}
	requireLabelAnswersNothing(t, runtime, pane.Label, "after the restart")
	for i := range labelContinuitySamples {
		requireLabelAnswersNothing(t, runtime, pane.Label, fmt.Sprintf("sample %d after the restart", i))
		time.Sleep(pollInterval)
	}

	after := requireServerLifetime(t, runtime, server, "after the restart")
	if goruntime.GOOS == "darwin" && after == before {
		t.Errorf("ServerInstance after the restart = %q, the same as before; want a different server lifetime", after)
	}

	ctx, cancel = context.WithTimeout(t.Context(), callTimeout)
	restored, err := runtime.InspectPane(ctx, pane.PaneID)
	cancel()
	if err != nil {
		t.Fatalf("InspectPane(%s) on the restored renamed pane: %v", pane.PaneID, err)
	}
	if restored.ShellPID == pane.PID {
		t.Errorf("restored renamed pane shell pid = %d, the original command's pid; want a fresh process", restored.ShellPID)
	}
	if restored.ServerInstance != after {
		t.Errorf("restored renamed pane inspection stamped %q, want the restarted server's %q", restored.ServerInstance, after)
	}
	gone := waitUntil(func() bool {
		return !slices.ContainsFunc(listGroupMembers(t, pane.PID), func(m groupMember) bool { return m.PID == pane.PID })
	})
	if !gone {
		t.Errorf("original command pid %d still listed in its group after the restart", pane.PID)
	}
	t.Logf("renamed pane %s restored under %q with shell pid %d (original %d gone: %t); lifetime before %q, after %q", pane.PaneID, renamed, restored.ShellPID, pane.PID, gone, before, after)
}
