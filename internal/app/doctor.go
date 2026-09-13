package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// CheckStatus is the outcome of one doctor check.
type CheckStatus int

const (
	// StatusOK means the check ran and the capability is present.
	StatusOK CheckStatus = iota
	// StatusSkipped means the check did not run; the detail says why.
	StatusSkipped
	// StatusUnsupported means the installed Herdr does not advertise a
	// capability HOP requires.
	StatusUnsupported
	// StatusUnavailable means the check ran and failed.
	StatusUnavailable
	// StatusNotFound means an optional executable is absent. It never makes
	// a report unhealthy: HOP orchestrates with the harnesses that exist.
	StatusNotFound
)

func (s CheckStatus) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusSkipped:
		return "skipped"
	case StatusUnsupported:
		return "unsupported"
	case StatusUnavailable:
		return "unavailable"
	case StatusNotFound:
		return "not found"
	}
	return fmt.Sprintf("status(%d)", int(s))
}

// Check is one line of a doctor report.
type Check struct {
	Name   string
	Status CheckStatus
	// Detail states what was observed, never what is assumed.
	Detail string
	// Advice, when present, tells the operator how to change the outcome.
	Advice string
}

// Report is the outcome of one doctor run.
type Report struct {
	Checks []Check
}

// Healthy reports whether HOP can work against the inspected installation:
// no check is unsupported or unavailable. Skipped checks and absent optional
// harnesses do not make a report unhealthy.
func (r Report) Healthy() bool {
	for _, c := range r.Checks {
		if c.Status == StatusUnsupported || c.Status == StatusUnavailable {
			return false
		}
	}
	return true
}

// Problems counts the checks that make the report unhealthy.
func (r Report) Problems() int {
	count := 0
	for _, c := range r.Checks {
		if c.Status == StatusUnsupported || c.Status == StatusUnavailable {
			count++
		}
	}
	return count
}

// feature is one HOP capability and the socket API methods it requires.
type feature struct {
	name    string
	methods []string
}

// requiredFeatures lists the capabilities this phase relies on. A feature is
// supported only when the installed binary's schema advertises every method.
func requiredFeatures() []feature {
	return []feature{
		{name: "plugin actions", methods: []string{
			"plugin.link", "plugin.unlink", "plugin.list",
			"plugin.action.list", "plugin.action.invoke",
			"plugin.log.list", "plugin.pane.open",
		}},
		{name: "display metadata", methods: []string{"pane.report_metadata"}},
		{name: "agent state reporting", methods: []string{"pane.report_agent"}},
		{name: "native agent view", methods: []string{"agent.view.set", "agent.view.clear"}},
		{name: "event observation", methods: []string{"events.subscribe", "session.snapshot"}},
		{name: "worktrees", methods: []string{
			"worktree.create", "worktree.list", "worktree.open", "worktree.remove",
		}},
		{name: "launch environment", methods: []string{
			"workspace.create", "tab.create", "pane.split", "layout.apply",
		}},
	}
}

// harness is one native agent harness HOP knows how to name.
type harness struct {
	name       string
	executable string
}

// supportedHarnesses is the fixed initial harness coverage. Absent harnesses
// are reported as not found, never guessed at.
func supportedHarnesses() []harness {
	return []harness{
		{name: "Claude Code", executable: "claude"},
		{name: "Codex", executable: "codex"},
		{name: "opencode", executable: "opencode"},
	}
}

// Doctor reports whether the local Herdr installation and the native
// harnesses support what HOP requires.
type Doctor struct {
	Probe Probe
	// SocketPath is the server socket to ping, or "" when none is
	// configured; the socket check is then skipped with advice.
	SocketPath string
}

// Run inspects the installation and returns one check per capability. A
// capability that cannot be checked is reported as skipped or unavailable
// with the reason; it is never reported as supported.
func (d *Doctor) Run(ctx context.Context) Report {
	var checks []Check
	checks = append(checks, d.binaryChecks(ctx)...)
	checks = append(checks, d.socketCheck(ctx))
	checks = append(checks, d.harnessChecks(ctx)...)
	return Report{Checks: checks}
}

// binaryChecks covers the herdr binary, its bundled schema and the feature
// table. A missing binary skips the dependent checks rather than failing
// each of them with the same root cause.
func (d *Doctor) binaryChecks(ctx context.Context) []Check {
	var checks []Check
	binary, err := d.Probe.Binary(ctx)
	if err != nil {
		checks = append(checks, Check{
			Name:   "herdr binary",
			Status: StatusUnavailable,
			Detail: err.Error(),
			Advice: "install Herdr 0.9.0 or newer and put herdr on PATH, or set HERDR_BIN_PATH",
		})
		checks = append(checks, skipAll("not checked: the herdr binary is unavailable")...)
		return checks
	}
	checks = append(checks, Check{
		Name:   "herdr binary",
		Status: StatusOK,
		Detail: fmt.Sprintf("%s at %s", binaryDescription(binary), binary.Path),
	})
	schema, err := d.Probe.Schema(ctx)
	if err != nil {
		checks = append(checks, Check{
			Name:   "socket API schema",
			Status: StatusUnavailable,
			Detail: err.Error(),
			Advice: "upgrade Herdr; `herdr api schema --json` must print the bundled schema",
		})
		checks = append(checks, skipFeatures("not checked: the socket API schema is unavailable")...)
		return checks
	}
	checks = append(checks, Check{
		Name:   "socket API schema",
		Status: StatusOK,
		Detail: fmt.Sprintf("protocol %d, %d methods", schema.Protocol, len(schema.Methods)),
	})
	for _, f := range requiredFeatures() {
		checks = append(checks, featureCheck(f, schema.Methods))
	}
	return checks
}

// featureCheck decides one feature from the advertised method list.
func featureCheck(f feature, advertised []string) Check {
	var missing []string
	for _, method := range f.methods {
		if !slices.Contains(advertised, method) {
			missing = append(missing, method)
		}
	}
	if len(missing) > 0 {
		return Check{
			Name:   "feature " + f.name,
			Status: StatusUnsupported,
			Detail: "schema does not advertise " + strings.Join(missing, ", "),
			Advice: "upgrade Herdr; HOP keeps this feature unavailable rather than guessing",
		}
	}
	return Check{
		Name:   "feature " + f.name,
		Status: StatusOK,
		Detail: fmt.Sprintf("all %d methods advertised", len(f.methods)),
	}
}

// skipAll marks the schema check and every feature as skipped for one reason.
func skipAll(reason string) []Check {
	checks := []Check{{Name: "socket API schema", Status: StatusSkipped, Detail: reason}}
	return append(checks, skipFeatures(reason)...)
}

// skipFeatures marks every feature check as skipped for one reason.
func skipFeatures(reason string) []Check {
	var checks []Check
	for _, f := range requiredFeatures() {
		checks = append(checks, Check{Name: "feature " + f.name, Status: StatusSkipped, Detail: reason})
	}
	return checks
}

// socketCheck pings the configured server socket, or explains how to enable
// the check when no socket is configured.
func (d *Doctor) socketCheck(ctx context.Context) Check {
	if d.SocketPath == "" {
		return Check{
			Name:   "server socket",
			Status: StatusSkipped,
			Detail: "no socket configured",
			Advice: "run hop inside a Herdr pane, or set HERDR_SOCKET_PATH, to check a live server",
		}
	}
	server, err := d.Probe.Ping(ctx)
	if err != nil {
		return Check{
			Name:   "server socket",
			Status: StatusUnavailable,
			Detail: fmt.Sprintf("%s: %v", d.SocketPath, err),
			Advice: "start the Herdr session that owns this socket, or correct HERDR_SOCKET_PATH",
		}
	}
	return Check{
		Name:   "server socket",
		Status: StatusOK,
		Detail: fmt.Sprintf("herdr %s, protocol %d at %s", server.Version, server.Protocol, d.SocketPath),
	}
}

// harnessChecks reports which native harnesses exist on this machine.
func (d *Doctor) harnessChecks(ctx context.Context) []Check {
	var checks []Check
	for _, h := range supportedHarnesses() {
		binary, err := d.Probe.Harness(ctx, h.executable)
		if err != nil {
			checks = append(checks, Check{
				Name:   "harness " + h.name,
				Status: StatusNotFound,
				Detail: fmt.Sprintf("%s: %v", h.executable, err),
			})
			continue
		}
		checks = append(checks, Check{
			Name:   "harness " + h.name,
			Status: StatusOK,
			Detail: fmt.Sprintf("%s at %s", binaryDescription(binary), binary.Path),
		})
	}
	return checks
}

// binaryDescription renders what is known about an executable without
// inventing a version for one that did not report it.
func binaryDescription(b BinaryInfo) string {
	if b.Version == "" {
		return "version unknown"
	}
	return b.Version
}
