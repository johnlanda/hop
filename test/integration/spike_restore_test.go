package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestSpikeRestorePlainPaneLosesAdditiveEnv is part of spike item S3: on a
// normal-configuration server (session.resume_agents_on_restore defaults
// true; the test config does not disable it), a graceful restart on the same
// roots restores a pane's identity — its creation-time label round-trips —
// but the restored pane's shell does NOT carry the creation-time additive
// environment. The pane was created with an additive HOP_RUN_ID; after the
// restart the same variable reads empty in the restored shell. This is the
// executed counterpart to the audited cold-restore path building
// PaneLaunchEnv::from_extra(Vec::new()) (repos/herdr/src/persist/restore.rs).
func TestSpikeRestorePlainPaneLosesAdditiveEnv(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)

	runID := newSpikeUUID(t)
	marker := "hop-spike-restore-" + runID[:8]

	var workspace struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
	}
	server.call(t, "workspace.create", map[string]any{"cwd": server.workDir(), "focus": true}, &workspace)
	var tab struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	server.call(t, "tab.create", map[string]any{
		"workspace_id": workspace.Workspace.WorkspaceID,
		"focus":        true,
		"label":        marker,
		"env":          map[string]string{"HOP_RUN_ID": runID},
	}, &tab)
	pane := tab.RootPane.PaneID

	// Before the restart the additive variable is present.
	server.call(t, "pane.send_text", map[string]any{
		"pane_id": pane,
		"text":    "printf 'BEFORE=[%s]\\n' \"$HOP_RUN_ID\"\n",
	}, nil)
	server.waitForPaneText(t, pane, "BEFORE=["+runID+"]")
	artifacts.save(t, "snapshot-before-restart.txt", server.snapshotDump(t))

	// Graceful restart on the same roots, then a client to drive geometry.
	server.restart(t)
	client := server.attachPTYClient(t)
	defer client.close(t)

	// The pane is restored: its creation-time label is found in the fresh
	// server's snapshot, mapped to a (new) pane id.
	var restoredPane string
	if !waitUntil(func() bool {
		restoredPane = server.snapshotPaneIDForLabel(t, marker)
		return restoredPane != ""
	}) {
		t.Fatalf("no restored pane carried the creation label %q; snapshot:\n%s", marker, server.snapshotDump(t))
	}
	artifacts.save(t, "snapshot-after-restart.txt", server.snapshotDump(t))

	// The restored shell has lost the additive variable. Wait for the complete
	// output-only value AFTER=[]DONE: the echoed command line contains
	// AFTER=[%s]DONE, so that literal appears only when the shell actually ran
	// the printf and expanded an empty HOP_RUN_ID — terminal echo cannot
	// satisfy it.
	server.call(t, "pane.send_text", map[string]any{
		"pane_id": restoredPane,
		"text":    "printf 'AFTER=[%s]DONE\\n' \"$HOP_RUN_ID\"\n",
	}, nil)
	snapshot := server.waitForPaneText(t, restoredPane, "AFTER=[]DONE")
	artifacts.save(t, "restored-pane.txt", snapshot)
	if strings.Contains(snapshot, "AFTER=["+runID+"]") {
		t.Errorf("restored pane still carries the additive HOP_RUN_ID; want it absent\n%s", snapshot)
	}
}

// TestSpikeRestoreAutoRelaunchBypassesLauncher is the design-critical half of
// S3: a pane with a recorded native agent session ("herdr:claude" + a session
// id) is relaunched on restart by Herdr's own auto-restore, which runs
// `claude --resume <id>` in a fresh login shell — bypassing any HOP launcher
// and carrying none of the creation-time additive HOP_* environment. It
// asserts (a) the relaunch happens and is deferred until a client supplies
// geometry, (b) the delayed-restore window is a phantom — the snapshot lists
// the agent while pane.process_info shows no live process, (c) the exact
// resume argv (`claude --resume <sessionID>`), and (d) the relaunched
// process's environment carries HERDR_* but not the additive HOP_RUN_ID,
// correlated to the resumed pid via a complete atomic env dump.
//
// That no dedicated "restoration finished" event exists is a source-audited
// observation (see FINDINGS.md), not something one test run can prove; this
// test asserts only the positive signals above.
//
// The fixture directory is first on the pane shell's PATH, so the bare
// `claude` the resume command runs resolves to the fixture harness.
func TestSpikeRestoreAutoRelaunchBypassesLauncher(t *testing.T) {
	fixtures := buildSpikeFixtures(t)
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	// Resolve a bare `claude` to the fixture in every login shell, including
	// the one the deferred resume spawns. Both sh and zsh login shells read
	// these files from the temp HOME.
	fixtureDir := filepath.Dir(fixtures.claude)
	prepend := "PATH=\"" + fixtureDir + ":$PATH\"\nexport PATH\n"
	for _, name := range []string{".profile", ".zprofile", ".zshrc"} {
		if err := os.WriteFile(filepath.Join(server.homeDir(), name), []byte(prepend), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	server.start(t)

	runID, sessionID := newSpikeUUID(t), newSpikeUUID(t)
	var workspace struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
	}
	server.call(t, "workspace.create", map[string]any{"cwd": server.workDir(), "focus": true}, &workspace)
	var tab struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	server.call(t, "tab.create", map[string]any{
		"workspace_id": workspace.Workspace.WorkspaceID,
		"focus":        true,
		"env":          map[string]string{"HOP_RUN_ID": runID},
	}, &tab)
	pane := tab.RootPane.PaneID

	// Before ANY bare `claude` is run — this test's own send_text below, and
	// later Herdr's deferred restore, which types one into a login shell
	// this test does not control — prove that a pane login shell resolves
	// the name to the fixture. The PATH prepend above is the arrangement;
	// this is the guarantee, and it is needed because the arrangement is a
	// race it can lose silently: a macOS login shell runs path_helper,
	// which puts the directories in /etc/paths and /etc/paths.d AHEAD of
	// the inherited hermetic PATH, and a developer machine has a real
	// claude installed in one of them. Resolving to anything else stops the
	// test here, before any process is started, rather than letting Herdr
	// run the operator's real harness unsupervised in a restored pane.
	requirePaneShellResolves(t, server, artifacts, pane, "claude", fixtures.claude)

	// Launch the fixture harness so the pane is a live, detected claude agent,
	// carrying the additive HOP_RUN_ID. Wait for its env dump to be durably
	// written (SPIKE-ENV-WRITTEN follows the atomic rename) and capture the
	// initial pid so the resumed process is unambiguously a different one.
	server.call(t, "pane.send_text", map[string]any{
		"pane_id": pane,
		"text":    "exec claude --run " + runID + "\n",
	}, nil)
	server.waitForPaneText(t, pane, "SPIKE-HARNESS-STARTED name=[claude]")
	server.waitForPaneText(t, pane, "SPIKE-HOP_RUN_ID=["+runID+"]")
	server.waitForPaneText(t, pane, "SPIKE-ENV-WRITTEN")
	server.waitForAgent(t, pane, "claude")
	initialPID := foregroundClaudeProcess(server.waitForForegroundProcess(t, pane)).PID
	if _, ok := envDumpForPID(t, server.workDir(), initialPID); !ok {
		t.Fatalf("initial fixture env dump for pid %d not present after SPIKE-ENV-WRITTEN", initialPID)
	}

	// Record the native conversation session so Herdr's auto-restore will
	// build a `claude --resume <sessionID>` plan for this pane.
	server.call(t, "pane.report_agent_session", map[string]any{
		"pane_id":          pane,
		"source":           "herdr:claude",
		"agent":            "claude",
		"agent_session_id": sessionID,
	}, &struct{}{})

	artifacts.save(t, "snapshot-before-restart.txt", server.snapshotDump(t))

	// Graceful restart on the same roots. restart() waits for the old server to
	// actually exit (its shutdown save completed) before the new one starts.
	server.restart(t)

	// Delayed-restore window: after restart, before any client supplies
	// geometry, the deferred resume cannot have fired. Assert the phantom: the
	// snapshot lists the pane as a claude agent while pane.process_info shows
	// no live claude process. This is the ordering the design must guard
	// against — snapshot agent presence precedes a live process.
	var restoredPane string
	if !waitUntil(func() bool {
		p, ok := server.snapshotAgentPane(t, "claude")
		if ok {
			restoredPane = p
		}
		return ok
	}) {
		t.Fatalf("restored pane never appeared as a claude agent in the snapshot:\n%s", server.snapshotDump(t))
	}
	artifacts.save(t, "snapshot-delayed-restore-window.txt", server.snapshotDump(t))
	// Classify the phantom precisely: the snapshot listed the agent, and
	// process_info must show NO live process — either the specific no-runtime
	// API error (the deferred resume has not spawned the shell) or a successful
	// read with no live claude foreground. Any other inspection error is not
	// accepted as absence evidence and fails the test.
	info, err := server.tryProcessInfo(t, restoredPane)
	switch {
	case err == nil:
		if live := foregroundClaudeProcess(info); live != nil {
			t.Errorf("delayed-restore window is not a phantom: a live claude process (pid %d) already runs while only the snapshot agent was expected", live.PID)
		}
		artifacts.save(t, "phantom-classification.txt", "process_info ok, no live claude foreground (phantom)\n")
	case isNoRuntimeError(err):
		artifacts.save(t, "phantom-classification.txt", fmt.Sprintf("process_info no-runtime error (phantom): %v\n", err))
	default:
		t.Fatalf("unexpected process_info error during the phantom window (not the no-runtime result): %v", err)
	}

	// A client supplies geometry, which is what lets the deferred resume fire.
	client := server.attachPTYClient(t)
	defer client.close(t)

	// The relaunch happened: pane.process_info now shows a live claude process
	// with a NEW pid, started via `claude --resume <sessionID>`.
	var resumed spikeProcessDetails
	if !waitUntil(func() bool {
		info, err := server.tryProcessInfo(t, restoredPane)
		if err != nil {
			return false
		}
		live := foregroundClaudeProcess(info)
		if live == nil || live.PID == initialPID || len(live.Argv) == 0 {
			return false
		}
		resumed = *live
		return true
	}) {
		t.Fatalf("auto-restore never relaunched a new claude process; snapshot:\n%s", server.snapshotDump(t))
	}
	artifacts.save(t, "snapshot-after-relaunch.txt", server.snapshotDump(t))
	artifacts.save(t, "resumed-process.txt", renderProcessInfo(server.processInfo(t, restoredPane)))

	// Exact resume argv: exactly `<claude> --resume <sessionID>` — three
	// entries, no more — proving the launch bypassed any HOP launcher and used
	// Herdr's native resume command verbatim.
	if len(resumed.Argv) != 3 {
		t.Errorf("resumed argv = %q, want exactly 3 entries [claude --resume %s]", resumed.Argv, sessionID)
	} else {
		if base := filepath.Base(resumed.Argv[0]); base != "claude" {
			t.Errorf("resumed argv[0] basename = %q, want claude; argv=%q", base, resumed.Argv)
		}
		if resumed.Argv[1] != "--resume" || resumed.Argv[2] != sessionID {
			t.Errorf("resumed argv = %q, want [claude --resume %s]", resumed.Argv, sessionID)
		}
	}

	// The relaunched process's environment, correlated by its pid: HERDR_*
	// identity present, but the creation-time additive HOP_RUN_ID dropped.
	env := server.waitForEnvDump(t, server.workDir(), resumed.PID)
	artifacts.save(t, "resumed-environ.txt", renderEnv(env))
	if env["HERDR_PANE_ID"] == "" {
		t.Errorf("resumed process environment lacks HERDR_PANE_ID: %v", env)
	}
	if _, present := env["HOP_RUN_ID"]; present {
		t.Errorf("resumed process kept the additive HOP_RUN_ID=%q; auto-restore was expected to drop it", env["HOP_RUN_ID"])
	}
}

// renderEnv formats a parsed env map deterministically for evidence files.
func renderEnv(env map[string]string) string {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	var out strings.Builder
	for _, name := range names {
		out.WriteString(name + "=" + env[name] + "\n")
	}
	return out.String()
}

// snapshotDump renders session.snapshot's tabs and panes for evidence files.
func (s *testServer) snapshotDump(t *testing.T) string {
	t.Helper()
	var result struct {
		Snapshot struct {
			Tabs []struct {
				TabID string `json:"tab_id"`
				Label string `json:"label"`
			} `json:"tabs"`
			Panes []struct {
				PaneID      string `json:"pane_id"`
				Label       string `json:"label"`
				Agent       string `json:"agent"`
				AgentStatus string `json:"agent_status"`
			} `json:"panes"`
			Agents []struct {
				PaneID        string `json:"pane_id"`
				Agent         string `json:"agent"`
				LaunchPending bool   `json:"launch_pending"`
			} `json:"agents"`
		} `json:"snapshot"`
	}
	s.call(t, "session.snapshot", nil, &result)
	var out strings.Builder
	out.WriteString("tabs:\n")
	for _, tab := range result.Snapshot.Tabs {
		out.WriteString("  " + tab.TabID + " label=" + tab.Label + "\n")
	}
	out.WriteString("panes:\n")
	for _, pane := range result.Snapshot.Panes {
		out.WriteString("  " + pane.PaneID + " label=" + pane.Label + " agent=" + pane.Agent + " status=" + pane.AgentStatus + "\n")
	}
	out.WriteString("agents:\n")
	for _, agent := range result.Snapshot.Agents {
		fmt.Fprintf(&out, "  %s agent=%s launch_pending=%t\n", agent.PaneID, agent.Agent, agent.LaunchPending)
	}
	return out.String()
}

// snapshotPaneIDForLabel returns the pane id whose tab carries the label, or
// empty if none. It maps a restored tab (found by its stable creation label)
// to its single pane.
func (s *testServer) snapshotPaneIDForLabel(t *testing.T, label string) string {
	t.Helper()
	var result struct {
		Snapshot struct {
			Tabs []struct {
				TabID string `json:"tab_id"`
				Label string `json:"label"`
			} `json:"tabs"`
			Panes []struct {
				PaneID string `json:"pane_id"`
				TabID  string `json:"tab_id"`
				Label  string `json:"label"`
			} `json:"panes"`
		} `json:"snapshot"`
	}
	s.call(t, "session.snapshot", nil, &result)
	// A tab label or a pane label may carry the marker; check both.
	for _, pane := range result.Snapshot.Panes {
		if pane.Label == label {
			return pane.PaneID
		}
	}
	tabID := ""
	for _, tab := range result.Snapshot.Tabs {
		if tab.Label == label {
			tabID = tab.TabID
		}
	}
	if tabID == "" {
		return ""
	}
	for _, pane := range result.Snapshot.Panes {
		if pane.TabID == tabID {
			return pane.PaneID
		}
	}
	return ""
}
