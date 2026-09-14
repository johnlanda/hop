package integration

import (
	"sort"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/herdr"
	"github.com/johnlanda/hop/internal/app"
)

// TestRealProcessAgentPresentationAndView drives a deterministic fixture: it
// creates a manager and two worker panes, reports custom agent identity and
// state on each through the public API (no real harness), publishes HOP
// display metadata through the presentation adapter, and proves the tokens
// round-trip, that they sort manager-first, that HOP owns and clears its
// native view, and that another view owner survives HOP's owned clear.
func TestRealProcessAgentPresentationAndView(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	registryBefore := registrySnapshot(t)
	stage := stagePlugin(t)
	server.start(t)
	ctx := testContext(t)

	// The plugin must be linked and enabled for the plugin:hop view source.
	var linked struct {
		Plugin struct {
			PluginID string `json:"plugin_id"`
		} `json:"plugin"`
	}
	server.call(t, "plugin.link", map[string]any{"path": stage, "enabled": true}, &linked)
	if linked.Plugin.PluginID != "hop" {
		t.Fatalf("plugin.link = %+v, want hop", linked.Plugin)
	}

	fixture := server.createRunFixture(t)
	presentation := herdr.NewPresentation(server.socketPath)
	presenter := &app.Presenter{Presentation: presentation}

	displays := []app.AgentDisplay{
		{PaneID: fixture.manager, Run: "r1", RunSequence: 1, Role: app.RoleManager, State: "planning"},
		{PaneID: fixture.implementer, Run: "r1", RunSequence: 1, Role: app.RoleImplementer, WorkerSequence: 1, Task: "retry policy", ParentLabel: "manager-r1", Account: "claude-a", State: "implementing"},
		{PaneID: fixture.reviewer, Run: "r1", RunSequence: 1, Role: app.RoleReviewer, WorkerSequence: 2, Task: "review", ParentLabel: "manager-r1", Account: "codex-b", State: "waiting"},
	}
	for i := range displays {
		if err := presenter.Publish(ctx, &displays[i]); err != nil {
			t.Fatalf("publish metadata for %s: %v", displays[i].PaneID, err)
		}
	}

	tokensByPane := server.agentTokens(t)
	artifacts.save(t, "agent-tokens.txt", renderTokens(tokensByPane))
	if role := tokensByPane[fixture.manager]["hop_role"]; role != "manager" {
		t.Errorf("manager pane hop_role = %q, want manager (tokens did not round-trip)", role)
	}
	if task := tokensByPane[fixture.implementer]["hop_task"]; task != "retry policy" {
		t.Errorf("implementer pane hop_task = %q, want retry policy", task)
	}

	assertManagerFirst(t, tokensByPane, fixture.manager)

	// HOP installs its manager-first projection and owns it.
	if err := presenter.Focus(ctx, "r1", "HOP r1"); err != nil {
		t.Fatalf("focus view: %v", err)
	}
	view := server.viewState(t)
	if !view.Active || view.Source != app.PresentationSource {
		t.Fatalf("after focus, view = %+v, want active and owned by %s", view, app.PresentationSource)
	}

	// Another owner replaces the view; HOP's owned clear must leave it intact.
	otherOwner := "custom:other-tool"
	server.setForeignView(t, otherOwner, "Other view")
	if err := presenter.Clear(ctx); err != nil {
		t.Fatalf("owned clear: %v", err)
	}
	after := server.viewState(t)
	if !after.Active || after.Source != otherOwner {
		t.Errorf("after HOP's owned clear, view = %+v, want the other owner %q still active", after, otherOwner)
	}

	// HOP re-selects and clears its own view, which does take effect.
	if err := presenter.Focus(ctx, "r1", "HOP r1"); err != nil {
		t.Fatalf("re-focus view: %v", err)
	}
	if err := presenter.Clear(ctx); err != nil {
		t.Fatalf("owned clear of own view: %v", err)
	}
	cleared := server.viewState(t)
	if cleared.Active && cleared.Source == app.PresentationSource {
		t.Errorf("after clearing HOP's own view it is still active and owned by HOP: %+v", cleared)
	}

	assertNoRegistryLeak(t, registryBefore)
}

// runFixture holds the pane ids of one deterministic run's agents.
type runFixture struct {
	workspace   string
	manager     string
	implementer string
	reviewer    string
}

// createRunFixture builds a manager pane and two worker panes and reports a
// custom agent identity and state on each, so they are recognized agents
// with observable status without any real harness.
func (s *testServer) createRunFixture(t *testing.T) runFixture {
	t.Helper()
	var workspace struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	s.call(t, "workspace.create", map[string]any{"cwd": s.workDir(), "focus": true}, &workspace)
	manager := workspace.RootPane.PaneID
	implementer := s.splitPane(t, manager)
	reviewer := s.splitPane(t, manager)

	s.reportAgent(t, manager, "manager", "idle")
	s.reportAgent(t, implementer, "implementer", "working")
	s.reportAgent(t, reviewer, "reviewer", "working")

	return runFixture{workspace: workspace.Workspace.WorkspaceID, manager: manager, implementer: implementer, reviewer: reviewer}
}

// splitPane splits a pane and returns the new pane's id.
func (s *testServer) splitPane(t *testing.T, paneID string) string {
	t.Helper()
	var split struct {
		Pane struct {
			PaneID string `json:"pane_id"`
		} `json:"pane"`
	}
	s.call(t, "pane.split", map[string]any{"pane_id": paneID, "direction": "down", "focus": false}, &split)
	if split.Pane.PaneID == "" {
		t.Fatal("pane.split returned no pane id")
	}
	return split.Pane.PaneID
}

// reportAgent reports a custom agent identity and lifecycle state on a pane.
func (s *testServer) reportAgent(t *testing.T, paneID, agent, state string) {
	t.Helper()
	s.call(t, "pane.report_agent", map[string]any{
		"pane_id": paneID,
		"source":  "custom:hop-fixture",
		"agent":   agent,
		"state":   state,
	}, nil)
}

// agentInfo is the subset of an agent.list record the presentation assertions
// read.
type agentInfo struct {
	PaneID string            `json:"pane_id"`
	Tokens map[string]string `json:"tokens"`
}

// agentTokens returns the pane metadata tokens Herdr exposes per recognized
// agent, keyed by pane id.
func (s *testServer) agentTokens(t *testing.T) map[string]map[string]string {
	t.Helper()
	var result struct {
		Agents []agentInfo `json:"agents"`
	}
	s.call(t, "agent.list", nil, &result)
	tokens := map[string]map[string]string{}
	for _, agent := range result.Agents {
		tokens[agent.PaneID] = agent.Tokens
	}
	return tokens
}

// agentStatus returns the effective agent status Herdr reports for one pane,
// or "" when the pane is not a recognized agent.
func (s *testServer) agentStatus(t *testing.T, paneID string) app.AgentStatus {
	t.Helper()
	var result struct {
		Agents []struct {
			PaneID      string `json:"pane_id"`
			AgentStatus string `json:"agent_status"`
		} `json:"agents"`
	}
	s.call(t, "agent.list", nil, &result)
	for _, agent := range result.Agents {
		if agent.PaneID == paneID {
			return app.AgentStatus(agent.AgentStatus)
		}
	}
	return ""
}

// viewInfo is the agent view state Herdr reports.
type viewInfo struct {
	Active bool   `json:"active"`
	Source string `json:"source"`
	Label  string `json:"label"`
}

// viewState reads the current native Agents view projection by clearing with
// a source that owns nothing, which Herdr answers with the current view
// state while leaving any real owner's projection untouched.
func (s *testServer) viewState(t *testing.T) viewInfo {
	t.Helper()
	var view viewInfo
	s.call(t, "agent.view.clear", map[string]any{"source": "custom:probe-does-not-own"}, &view)
	return view
}

// setForeignView installs a view owned by a non-HOP source.
func (s *testServer) setForeignView(t *testing.T, source, label string) {
	t.Helper()
	var view viewInfo
	s.call(t, "agent.view.set", map[string]any{
		"source": source,
		"label":  label,
		"sort":   []map[string]any{{"field": "agent", "order": "asc"}},
	}, &view)
	if !view.Active || view.Source != source {
		t.Fatalf("foreign view.set = %+v, want active owned by %s", view, source)
	}
}

// workDir returns the fixture working directory inside the test roots.
func (s *testServer) workDir() string {
	return s.base + "/work"
}

// assertManagerFirst fails unless the manager pane's ordering tokens sort
// before every worker's, which is what makes the native string sort render
// the run manager-first.
func assertManagerFirst(t *testing.T, tokensByPane map[string]map[string]string, managerPane string) {
	t.Helper()
	type keyed struct {
		pane string
		key  string
	}
	var order []keyed
	for pane, tokens := range tokensByPane {
		if tokens["hop_order"] == "" {
			continue
		}
		order = append(order, keyed{pane: pane, key: tokens["hop_run_order"] + tokens["hop_order"]})
	}
	sort.Slice(order, func(i, j int) bool { return order[i].key < order[j].key })
	if len(order) == 0 {
		t.Fatal("no HOP-ordered agents found")
	}
	if order[0].pane != managerPane {
		t.Errorf("sorting by HOP tokens puts %s first, want the manager pane %s", order[0].pane, managerPane)
	}
}

// renderTokens formats a token map for evidence files.
func renderTokens(tokensByPane map[string]map[string]string) string {
	var out string
	panes := make([]string, 0, len(tokensByPane))
	for pane := range tokensByPane {
		panes = append(panes, pane)
	}
	sort.Strings(panes)
	for _, pane := range panes {
		out += pane + ": "
		tokens := tokensByPane[pane]
		names := make([]string, 0, len(tokens))
		for name := range tokens {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			out += name + "=" + tokens[name] + " "
		}
		out += "\n"
	}
	return out
}
