package integration

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/herdr"
)

// TestSpikeAgentStartArgvCapability is spike item S5: agent.start cannot
// carry a wrapper argv. Its kind must be one of Herdr's recognized agent
// kinds — an arbitrary executable name is rejected — and its args are
// appended AFTER the fixed per-kind harness executable, so nothing can
// interpose a sanitizer between the shell and the harness through
// agent.start. The typed command line in the pane is the live evidence that
// argv[0] is the harness executable name resolved by the pane shell's PATH.
// (Source: start_agent composes argv as
// [interactive_agent_executable(kind)] + args —
// repos/herdr/src/app/agents.rs:198-201.)
func TestSpikeAgentStartArgvCapability(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	// agent.start types a bare executable name into the pane shell, so the pane
	// shell's PATH decides which binary runs. A macOS login shell's path_helper
	// can reorder PATH so the stub directory is not first, so the test-owned
	// .profile re-prepends it to make resolution deterministic here. That PATH
	// resolution is not fixed by agent.start is recorded as an S5 finding in
	// FINDINGS.md; this test only needs a deterministic resolution to assert
	// the composed argv.
	stubFirst := "PATH=\"" + filepath.Join(server.base, "bin") + ":$PATH\"\nexport PATH\n"
	if err := os.WriteFile(filepath.Join(server.homeDir(), ".profile"), []byte(stubFirst), 0o600); err != nil {
		t.Fatal(err)
	}
	server.start(t)

	pane := server.createSpikeWorkerPane(t, nil)

	rejections := []struct {
		name   string
		params map[string]any
		code   string
	}{
		{
			name: "an arbitrary wrapper kind is not a recognized agent kind",
			params: map[string]any{
				"name": "spike-wrapper", "kind": "hop-launch", "pane_id": pane,
				"args": []string{"--", "claude"},
			},
			code: "unsupported_agent_kind",
		},
		{
			name: "args cannot smuggle control characters",
			params: map[string]any{
				"name": "spike-control", "kind": "claude", "pane_id": pane,
				"args": []string{"bad\x01arg"},
			},
			code: "invalid_agent_argument",
		},
	}
	for _, tc := range rejections {
		t.Run(tc.name, func(t *testing.T) {
			err := server.client.Call(testContext(t), "agent.start", tc.params, &struct{}{})
			var apiErr *herdr.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("agent.start = %v, want an API error with code %s", err, tc.code)
			}
			if apiErr.Code != tc.code {
				t.Errorf("agent.start error code = %s (%s), want %s", apiErr.Code, apiErr.Message, tc.code)
			}
		})
	}

	// A recognized kind with args: the line typed into the pane shell is the
	// fixed executable for the kind followed by the args. The stub `claude`
	// on the fixture PATH answers it, proving argv[0] resolved as the
	// harness name, not any caller-chosen program. agent.start requires the
	// pane shell to be idle and the sole foreground process, so wait for that
	// before issuing it (otherwise it races the login shell's startup and
	// returns agent_pane_busy).
	server.waitForShellReady(t, pane)
	runID := newSpikeUUID(t)
	err := server.client.Call(testContext(t), "agent.start", map[string]any{
		"name": "spike-agent", "kind": "claude", "pane_id": pane,
		"args": []string{"launch", "--run", runID},
	}, &struct{}{})
	if err != nil {
		t.Fatalf("agent.start with a recognized kind: %v", err)
	}
	server.waitForPaneText(t, pane, "claude launch --run "+runID)
	server.waitForPaneText(t, pane, "claude 0.0.0-stub")
	artifacts.save(t, "agent-start-pane.txt", server.readPane(t, pane))
}
