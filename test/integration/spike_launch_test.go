package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/herdr"
	"github.com/johnlanda/hop/internal/app"
)

// TestSpikeLaunchLineDetection is spike item S1: a launch line injected into
// a fresh pane shell via pane.send_text, of the form
//
//	exec '<launch-standin>' --harness '<fixture harness>' --run <uuid> --attempt <uuid>
//
// where launch-standin execve's the fixture harness, must (a) be consumed by
// the shell — the injected line launches successfully despite the login
// shell's noisy startup files, (b) leave the exec'd process detected as an
// agent when its process name is a recognized harness name, observable through
// both agent.list and a pane.agent_status_changed event, (c) carry the
// pane-creation env map through the shell and both execs into the final
// process, and (d) leave the exec'd harness holding the pane shell's own pid
// (pid == shell_pid == foreground process group), which agent.start does not.
//
// This does not claim the line arrives strictly before rc completion; a slow
// runner could inject after the rc sleep. The claim is the weaker, sufficient
// one: injection into a fresh pane succeeds even with noisy startup. Consumed
// versus lost is distinguished by the fixture's own marker output: a consumed
// line execs the harness stand-in, which prints SPIKE-HARNESS-STARTED; a lost
// line never produces the marker and the bounded wait fails with the pane
// snapshot. The injected line is never retyped.
func TestSpikeLaunchLineDetection(t *testing.T) {
	fixtures := buildSpikeFixtures(t)

	cases := []struct {
		name string
		// shell is the SHELL every pane login shell uses.
		shell string
		// rcFiles maps home-relative startup files to contents written
		// before the server starts, to make rc startup noisy and slow, so the
		// launch is exercised against a shell that is not instantly idle. The
		// sleep is scenario construction, not a synchronization barrier; every
		// assertion still polls a bounded condition.
		rcFiles map[string]string
		// rcMarkers must all appear in the pane, proving the noisy rc files
		// really ran in this pane's shell.
		rcMarkers []string
	}{
		{
			name:  "sh login shell with quiet rc",
			shell: "/bin/sh",
		},
		{
			name:  "zsh login shell with noisy slow rc",
			shell: "/bin/zsh",
			rcFiles: map[string]string{
				".zprofile": "echo SPIKE-RC-ZPROFILE; sleep 1; echo SPIKE-RC-ZPROFILE-DONE\n",
				".zshrc":    "echo SPIKE-RC-ZSHRC\n",
			},
			rcMarkers: []string{"SPIKE-RC-ZPROFILE", "SPIKE-RC-ZPROFILE-DONE", "SPIKE-RC-ZSHRC"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireShell(t, tc.shell)
			artifacts := newArtifactDir(t)
			server := prepareServer(t, artifacts)
			server.shell = tc.shell
			for name, content := range tc.rcFiles {
				if err := os.WriteFile(filepath.Join(server.homeDir(), name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			server.start(t)

			runID, attemptID := newSpikeUUID(t), newSpikeUUID(t)
			pane := server.createSpikeWorkerPane(t, map[string]string{
				"HOP_RUN_ID":     runID,
				"HOP_ATTEMPT_ID": attemptID,
			})

			// Record what pane process inspection reports at creation time,
			// before the login shell has necessarily finished its startup
			// files. This is retained evidence, not an assertion: the exact
			// startup phase captured here is timing-dependent.
			artifacts.save(t, "process-info-at-creation.txt", renderProcessInfo(server.processInfo(t, pane)))

			// Subscribe to the pane's agent-status transitions before the
			// launch line is sent, so the detection transition cannot be
			// missed.
			observer := herdr.NewObserver(server.socketPath, pane)
			stream, err := observer.Subscribe(testContext(t))
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			defer func() {
				if closeErr := stream.Close(); closeErr != nil {
					t.Logf("close status stream: %v", closeErr)
				}
			}()

			// Inject the launch line immediately, while rc startup may still
			// be running. On ambiguity nothing is ever retyped.
			line := fmt.Sprintf("exec '%s' --harness '%s' --run %s --attempt %s\n",
				fixtures.launchStandin, fixtures.claude, runID, attemptID)
			server.call(t, "pane.send_text", map[string]any{"pane_id": pane, "text": line}, nil)

			// Consumed: the harness stand-in is running and printed its
			// markers, including the env map values carried through both
			// execs.
			server.waitForPaneText(t, pane, "SPIKE-HARNESS-STARTED name=[claude]")
			server.waitForPaneText(t, pane, "SPIKE-HOP_RUN_ID=["+runID+"]")
			server.waitForPaneText(t, pane, "SPIKE-HOP_ATTEMPT_ID=["+attemptID+"]")
			for _, marker := range tc.rcMarkers {
				server.waitForPaneText(t, pane, marker)
			}
			artifacts.save(t, "pane-after-launch.txt", server.readPane(t, pane))

			// Detected: the pane is listed as a claude agent, and at least
			// one agent-status transition for the pane was pushed on the
			// subscription opened before the launch.
			record := server.waitForAgent(t, pane, "claude")
			t.Logf("agent record after detection: %+v", record)
			event := awaitAnyStatusEvent(t, stream, pane)
			t.Logf("pane.agent_status_changed observed: pane=%s status=%s", event.PaneID, event.Status)

			// Process identity after the launch: the pane's process is the
			// exec'd stand-in under the recognized name, with the launch argv
			// visible.
			info := server.processInfo(t, pane)
			artifacts.save(t, "process-info-after-launch.txt", renderProcessInfo(info))
			process := foregroundClaudeProcess(info)
			if process == nil {
				t.Fatalf("no foreground process named claude after the launch:\n%s", renderProcessInfo(info))
			}
			if !strings.Contains(strings.Join(process.Argv, " "), "--run "+runID) {
				t.Errorf("foreground argv %q does not carry the launch identity --run %s", process.Argv, runID)
			}

			// The pid relationship the RuntimeBinding design depends on: a
			// process that execve'd inside the top-level pane shell keeps the
			// shell's own pid, so pid == shell_pid == foreground process group.
			// This is asserted, not merely logged, because the design relies on
			// it (an exec'd launcher is indistinguishable from the pane shell by
			// pid; only its argv identifies it).
			relationship := fmt.Sprintf(
				"shell=%s shell_pid=%d foreground_pgid=%d harness_pid=%d\n",
				tc.shell, info.ShellPID, info.ForegroundProcessGroup, process.PID)
			t.Log(relationship)
			artifacts.save(t, "pid-relationship.txt", relationship)
			if process.PID != info.ShellPID {
				t.Errorf("exec'd harness pid %d != shell_pid %d; an exec in the pane shell should keep the shell pid", process.PID, info.ShellPID)
			}
			if process.PID != info.ForegroundProcessGroup {
				t.Errorf("exec'd harness pid %d != foreground process group %d", process.PID, info.ForegroundProcessGroup)
			}
		})
	}
}

// TestSpikeUnrecognizedProcessNameIsNotDetected is the S1 control: the same
// exec'd launch, with the identical stand-in binary under a name outside
// Herdr's recognized harness names, is never detected as an agent. Detection
// is process-name identification, not a property of how the process was
// started.
func TestSpikeUnrecognizedProcessNameIsNotDetected(t *testing.T) {
	fixtures := buildSpikeFixtures(t)
	server := prepareServer(t, newArtifactDir(t))
	server.start(t)

	pane := server.createSpikeWorkerPane(t, nil)
	line := fmt.Sprintf("exec '%s' --harness '%s' --run %s\n",
		fixtures.launchStandin, fixtures.standinHarness, newSpikeUUID(t))
	server.call(t, "pane.send_text", map[string]any{"pane_id": pane, "text": line}, nil)

	// The process is live and the line was consumed…
	server.waitForPaneText(t, pane, "SPIKE-HARNESS-STARTED name=[standin-harness]")
	// …yet the pane never becomes an agent.
	server.assertNeverListedAsAgent(t, pane)
}

// createSpikeWorkerPane opens a fresh tab whose root pane carries the given
// additive env map and returns the pane id — the launch surface the Phase 2
// design uses for worker panes.
func (s *testServer) createSpikeWorkerPane(t *testing.T, env map[string]string) string {
	t.Helper()
	var base struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
	}
	s.call(t, "workspace.create", map[string]any{"cwd": s.workDir(), "focus": true}, &base)
	params := map[string]any{
		"workspace_id": base.Workspace.WorkspaceID,
		"focus":        true,
	}
	if len(env) > 0 {
		params["env"] = env
	}
	var tab struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	s.call(t, "tab.create", params, &tab)
	if tab.RootPane.PaneID == "" {
		t.Fatal("tab.create returned no root pane id")
	}
	return tab.RootPane.PaneID
}

// awaitAnyStatusEvent reads the stream until any status event for the pane
// arrives, with a bounded deadline.
func awaitAnyStatusEvent(t *testing.T, stream app.StatusStream, paneID string) app.StatusEvent {
	t.Helper()
	deadline := timeAfterConditionTimeout()
	for {
		select {
		case event, ok := <-stream.Events():
			if !ok {
				t.Fatalf("status stream ended before any event for %s: %v", paneID, stream.Err())
			}
			if event.PaneID == paneID {
				return event
			}
		case <-deadline:
			t.Fatalf("no agent-status event for %s before the deadline", paneID)
		}
	}
}
