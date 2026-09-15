package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSpikeMultiPaneEnvIsolation is spike item S10: three layout.apply
// command-pane launches (S6's transport) issued CONCURRENTLY into one
// workspace -- a manager-shaped identity and two worker-shaped identities,
// each with its own HOP_SESSION_ID/HOP_ROLE and creation label -- prove that
// concurrent launches cannot cross-contaminate identity: each pane's process
// observes exactly its own additive env, its own label resolves to its own
// pane, and agent detection is independent per pane. Phase 3's bounded
// worker concurrency (design section 6, up to two workers plus a manager and
// reviewer sharing one server) rests on this holding under real concurrent
// creation, not merely sequential creation that happens to look isolated.
func TestSpikeMultiPaneEnvIsolation(t *testing.T) {
	fixtures := buildSpikeFixtures(t)
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.start(t)

	var workspace workspaceCreatedResponse
	server.call(t, "workspace.create", map[string]any{"cwd": server.workDir(), "focus": true}, &workspace)

	runID := newSpikeUUID(t)
	panes := []paneEnvSpec{
		{role: "manager", sessionID: "sess-" + newSpikeUUID(t), label: "hop-spike-pane-manager"},
		{role: "implementer", sessionID: "sess-" + newSpikeUUID(t), label: "hop-spike-pane-worker-1"},
		{role: "implementer", sessionID: "sess-" + newSpikeUUID(t), label: "hop-spike-pane-worker-2"},
	}
	for i := range panes {
		panes[i].envFile = filepath.Join(artifacts.dir(t, "pane-env"), fmt.Sprintf("pane-%d.txt", i))
	}

	// Launch all three concurrently: this is the real access pattern (the
	// controller assigning multiple sessions in one scheduling pass, design
	// section 6), not three sequential requests that happen not to race.
	results := make([]<-chan paneApplyAsyncResult, len(panes))
	for i := range panes {
		results[i] = server.applyCommandPaneAsync(workspace.Workspace.WorkspaceID, fixtures.claude, runID, &panes[i])
	}
	for i, ch := range results {
		result := <-ch
		if result.err != nil {
			t.Fatalf("layout.apply for pane %d (%s): %v", i, panes[i].label, result.err)
		}
		panes[i].paneID = result.paneID
		if panes[i].paneID == "" {
			t.Fatalf("layout.apply for pane %d (%s) returned no pane id", i, panes[i].label)
		}
	}

	// Each process observes exactly its own additive env: HOP_SESSION_ID and
	// HOP_ROLE match what THIS pane requested, and none of the other panes'
	// session ids leaked into it.
	for i, spec := range panes {
		server.waitForPaneText(t, spec.paneID, "SPIKE-HARNESS-STARTED name=[claude]")
		env := server.waitForEnvDumpAtPath(t, spec.envFile)
		if got := env["HOP_SESSION_ID"]; got != spec.sessionID {
			t.Errorf("pane %d (%s) HOP_SESSION_ID = %q, want %q", i, spec.label, got, spec.sessionID)
		}
		if got := env["HOP_ROLE"]; got != spec.role {
			t.Errorf("pane %d (%s) HOP_ROLE = %q, want %q", i, spec.label, got, spec.role)
		}
		for j, other := range panes {
			if i == j {
				continue
			}
			if env["HOP_SESSION_ID"] == other.sessionID {
				t.Errorf("pane %d (%s) observed pane %d's HOP_SESSION_ID %q; concurrent launches cross-contaminated identity",
					i, spec.label, j, other.sessionID)
			}
		}
	}

	// Labels resolve to the right panes.
	for _, spec := range panes {
		got := server.snapshotPaneByLabel(t, spec.label)
		if got != spec.paneID {
			t.Errorf("session.snapshot resolves label %q to pane %q, want %q", spec.label, got, spec.paneID)
		}
	}

	// Agent detection is independent per pane: each is listed under its own
	// pane id, and exactly three agent records exist -- no merged or aliased
	// detection across the concurrently created panes.
	for _, spec := range panes {
		server.waitForAgent(t, spec.paneID, "claude")
	}
	records := server.agentRecords(t)
	if len(records) != len(panes) {
		t.Errorf("agent.list has %d records, want exactly %d (one per concurrently launched pane)", len(records), len(panes))
	}
	for i, spec := range panes {
		record, ok := records[spec.paneID]
		if !ok {
			t.Errorf("agent.list has no record for pane %d (%s)", i, spec.label)
			continue
		}
		if record.Agent != "claude" {
			t.Errorf("pane %d (%s) agent.list agent = %q, want claude", i, spec.label, record.Agent)
		}
	}
}

// paneEnvSpec is one command pane's requested identity for the S10 probe.
type paneEnvSpec struct {
	role      string
	sessionID string
	label     string
	envFile   string
	paneID    string
}

// paneApplyAsyncResult is one layout.apply call's outcome, delivered over a
// channel so several launches can be issued concurrently from a test's main
// goroutine without calling t.Fatal from another goroutine.
type paneApplyAsyncResult struct {
	paneID string
	err    error
}

// applyCommandPaneAsync issues one layout.apply command-pane creation (S6's
// transport) into workspaceID in its own goroutine, carrying spec's
// role/session identity as additive env plus its own env-dump file, and
// returns a channel with the created pane id or the call's error.
func (s *testServer) applyCommandPaneAsync(workspaceID, claudeBin, runID string, spec *paneEnvSpec) <-chan paneApplyAsyncResult {
	out := make(chan paneApplyAsyncResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		var applied workerPaneAppliedResponse
		err := s.client.Call(ctx, "layout.apply", map[string]any{
			"workspace_id": workspaceID,
			"focus":        false,
			"root": map[string]any{
				"type":    "pane",
				"label":   spec.label,
				"cwd":     s.workDir(),
				"command": []string{claudeBin, "--run", runID},
				"env": map[string]string{
					"HOP_RUN_ID":         runID,
					"HOP_SESSION_ID":     spec.sessionID,
					"HOP_ROLE":           spec.role,
					"HOP_SPIKE_ENV_FILE": spec.envFile,
				},
			},
		}, &applied)
		out <- paneApplyAsyncResult{paneID: applied.Layout.Root.PaneID, err: err}
	}()
	return out
}

// waitForEnvDumpAtPath waits until the fixture's atomic env dump at path
// exists and returns its parsed contents (the same write-temp-then-rename
// contract as envDumpForPID, addressed by an explicit path instead of a pid
// since a pane's process pid is not known ahead of the launch call here).
func (*testServer) waitForEnvDumpAtPath(t *testing.T, path string) map[string]string {
	t.Helper()
	var env map[string]string
	if !waitUntil(func() bool {
		content, err := os.ReadFile(path) //nolint:gosec // G304: the path is inside this test's own artifact directory.
		if err != nil {
			return false
		}
		env = parseEnvDump(string(content))
		return true
	}) {
		t.Fatalf("fixture env dump at %s never appeared", path)
	}
	return env
}

// parseEnvDump parses a fixture env dump's NAME=VALUE lines into a map (the
// same format envDumpForPID parses, addressed by an explicit file path here
// instead of a pid).
func parseEnvDump(content string) map[string]string {
	env := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			env[name] = value
		}
	}
	return env
}
