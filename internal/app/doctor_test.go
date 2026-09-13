package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

// fakeProbe is a handwritten app.Probe whose observations are fixed per test
// case. A nil error field makes the corresponding method succeed.
type fakeProbe struct {
	binary     app.BinaryInfo
	binaryErr  error
	schema     app.SchemaInfo
	schemaErr  error
	server     app.ServerInfo
	pingErr    error
	harnesses  map[string]app.BinaryInfo
	harnessErr map[string]error
}

func (p *fakeProbe) Binary(context.Context) (app.BinaryInfo, error) {
	return p.binary, p.binaryErr
}

func (p *fakeProbe) Schema(context.Context) (app.SchemaInfo, error) {
	return p.schema, p.schemaErr
}

func (p *fakeProbe) Ping(context.Context) (app.ServerInfo, error) {
	return p.server, p.pingErr
}

func (p *fakeProbe) Harness(_ context.Context, executable string) (app.BinaryInfo, error) {
	if err, ok := p.harnessErr[executable]; ok {
		return app.BinaryInfo{}, err
	}
	if info, ok := p.harnesses[executable]; ok {
		return info, nil
	}
	return app.BinaryInfo{}, errors.New(executable + " is not on PATH")
}

// fullSchema returns a schema advertising every method the doctor requires.
func fullSchema() app.SchemaInfo {
	return app.SchemaInfo{Protocol: 22, Methods: []string{
		"agent.view.clear", "agent.view.set", "events.subscribe", "layout.apply",
		"pane.report_agent", "pane.report_metadata", "pane.split",
		"plugin.action.invoke", "plugin.action.list", "plugin.link", "plugin.list",
		"plugin.log.list", "plugin.pane.open", "plugin.unlink", "session.snapshot",
		"tab.create", "workspace.create", "worktree.create", "worktree.list",
		"worktree.open", "worktree.remove",
	}}
}

func healthyProbe() *fakeProbe {
	return &fakeProbe{
		binary: app.BinaryInfo{Path: "/opt/herdr", Version: "herdr 0.9.0"},
		schema: fullSchema(),
		server: app.ServerInfo{Version: "0.9.0", Protocol: 22},
		harnesses: map[string]app.BinaryInfo{
			"claude": {Path: "/bin/claude", Version: "1.0.0 (Claude Code)"},
			"codex":  {Path: "/bin/codex", Version: "codex 1.0"},
		},
	}
}

// statusOf returns the status of the named check and fails when it is absent.
func statusOf(t *testing.T, report app.Report, name string) app.Check {
	t.Helper()
	for _, c := range report.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("report has no check named %q; checks: %+v", name, report.Checks)
	return app.Check{}
}

func TestDoctorRun(t *testing.T) {
	cases := []struct {
		name        string
		probe       *fakeProbe
		socketPath  string
		wantHealthy bool
		wantStatus  map[string]app.CheckStatus
		wantDetail  map[string]string // substring per check name
	}{
		{
			name:        "healthy installation with a configured socket",
			probe:       healthyProbe(),
			socketPath:  "/tmp/herdr.sock",
			wantHealthy: true,
			wantStatus: map[string]app.CheckStatus{
				"herdr binary":           app.StatusOK,
				"socket API schema":      app.StatusOK,
				"feature plugin actions": app.StatusOK,
				"server socket":          app.StatusOK,
				"harness Claude Code":    app.StatusOK,
				"harness Codex":          app.StatusOK,
				"harness opencode":       app.StatusNotFound,
			},
			wantDetail: map[string]string{
				"herdr binary":     "herdr 0.9.0 at /opt/herdr",
				"server socket":    "herdr 0.9.0, protocol 22 at /tmp/herdr.sock",
				"harness opencode": "not on PATH",
			},
		},
		{
			name: "missing binary skips schema and features",
			probe: func() *fakeProbe {
				p := healthyProbe()
				p.binaryErr = errors.New("herdr is not on PATH")
				return p
			}(),
			wantHealthy: false,
			wantStatus: map[string]app.CheckStatus{
				"herdr binary":               app.StatusUnavailable,
				"socket API schema":          app.StatusSkipped,
				"feature plugin actions":     app.StatusSkipped,
				"feature launch environment": app.StatusSkipped,
			},
			wantDetail: map[string]string{
				"socket API schema": "not checked",
			},
		},
		{
			name: "unreadable schema skips features",
			probe: func() *fakeProbe {
				p := healthyProbe()
				p.schemaErr = errors.New("api schema exited 1")
				return p
			}(),
			wantHealthy: false,
			wantStatus: map[string]app.CheckStatus{
				"herdr binary":           app.StatusOK,
				"socket API schema":      app.StatusUnavailable,
				"feature plugin actions": app.StatusSkipped,
			},
		},
		{
			name: "missing methods are reported unsupported by name",
			probe: func() *fakeProbe {
				p := healthyProbe()
				schema := fullSchema()
				var methods []string
				for _, m := range schema.Methods {
					if m != "agent.view.set" && m != "agent.view.clear" {
						methods = append(methods, m)
					}
				}
				schema.Methods = methods
				p.schema = schema
				return p
			}(),
			wantHealthy: false,
			wantStatus: map[string]app.CheckStatus{
				"feature native agent view": app.StatusUnsupported,
				"feature plugin actions":    app.StatusOK,
			},
			wantDetail: map[string]string{
				"feature native agent view": "agent.view.set, agent.view.clear",
			},
		},
		{
			name:        "no configured socket is skipped, not failed",
			probe:       healthyProbe(),
			wantHealthy: true,
			wantStatus: map[string]app.CheckStatus{
				"server socket": app.StatusSkipped,
			},
		},
		{
			name: "configured but unreachable socket is unhealthy",
			probe: func() *fakeProbe {
				p := healthyProbe()
				p.pingErr = errors.New("connection refused")
				return p
			}(),
			socketPath:  "/tmp/gone.sock",
			wantHealthy: false,
			wantStatus: map[string]app.CheckStatus{
				"server socket": app.StatusUnavailable,
			},
			wantDetail: map[string]string{
				"server socket": "/tmp/gone.sock: connection refused",
			},
		},
		{
			name: "harness without a readable version is still found",
			probe: func() *fakeProbe {
				p := healthyProbe()
				p.harnesses["opencode"] = app.BinaryInfo{Path: "/bin/opencode"}
				return p
			}(),
			wantHealthy: true,
			wantStatus: map[string]app.CheckStatus{
				"harness opencode": app.StatusOK,
			},
			wantDetail: map[string]string{
				"harness opencode": "version unknown at /bin/opencode",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doctor := &app.Doctor{Probe: tc.probe, SocketPath: tc.socketPath}

			report := doctor.Run(t.Context())

			if got := report.Healthy(); got != tc.wantHealthy {
				t.Errorf("Healthy() = %t, want %t; checks: %+v", got, tc.wantHealthy, report.Checks)
			}
			for name, want := range tc.wantStatus {
				if check := statusOf(t, report, name); check.Status != want {
					t.Errorf("%s status = %s, want %s (detail: %s)", name, check.Status, want, check.Detail)
				}
			}
			for name, want := range tc.wantDetail {
				if check := statusOf(t, report, name); !strings.Contains(check.Detail, want) {
					t.Errorf("%s detail = %q, want it to contain %q", name, check.Detail, want)
				}
			}
		})
	}
}

func TestDoctorReportsFactsNotGuesses(t *testing.T) {
	probe := healthyProbe()
	probe.binaryErr = errors.New("herdr is not on PATH")
	doctor := &app.Doctor{Probe: probe}

	report := doctor.Run(t.Context())

	for _, c := range report.Checks {
		if strings.HasPrefix(c.Name, "feature ") && c.Status == app.StatusOK {
			t.Errorf("feature %q is reported supported although nothing was observed", c.Name)
		}
	}
	if report.Problems() != 1 {
		t.Errorf("Problems() = %d, want 1 (only the binary check failed)", report.Problems())
	}
}
