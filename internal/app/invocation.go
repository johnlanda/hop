package app

import (
	"errors"
	"fmt"
	"strings"
)

// Invocation describes one plugin command start as Herdr reports it through
// the plugin environment. Herdr injects these values into every startup,
// action, event and pane command of an enabled plugin.
type Invocation struct {
	PluginID     string
	ActionID     string
	Event        string
	EntrypointID string
	SocketPath   string
	BinaryPath   string
	Root         string
	ConfigDir    string
	StateDir     string
	WorkspaceID  string
	TabID        string
	PaneID       string
	ContextJSON  string
}

// invocationField binds one plugin environment variable to its Invocation
// field and the label Describe reports it under.
type invocationField struct {
	label string
	key   string
	value func(*Invocation) *string
}

// invocationFields lists every plugin environment variable in the order
// Describe reports them.
func invocationFields() []invocationField {
	return []invocationField{
		{"plugin", "HERDR_PLUGIN_ID", func(i *Invocation) *string { return &i.PluginID }},
		{"action", "HERDR_PLUGIN_ACTION_ID", func(i *Invocation) *string { return &i.ActionID }},
		{"event", "HERDR_PLUGIN_EVENT", func(i *Invocation) *string { return &i.Event }},
		{"pane entrypoint", "HERDR_PLUGIN_ENTRYPOINT_ID", func(i *Invocation) *string { return &i.EntrypointID }},
		{"socket", "HERDR_SOCKET_PATH", func(i *Invocation) *string { return &i.SocketPath }},
		{"herdr binary", "HERDR_BIN_PATH", func(i *Invocation) *string { return &i.BinaryPath }},
		{"root", "HERDR_PLUGIN_ROOT", func(i *Invocation) *string { return &i.Root }},
		{"config dir", "HERDR_PLUGIN_CONFIG_DIR", func(i *Invocation) *string { return &i.ConfigDir }},
		{"state dir", "HERDR_PLUGIN_STATE_DIR", func(i *Invocation) *string { return &i.StateDir }},
		{"workspace", "HERDR_WORKSPACE_ID", func(i *Invocation) *string { return &i.WorkspaceID }},
		{"tab", "HERDR_TAB_ID", func(i *Invocation) *string { return &i.TabID }},
		{"pane", "HERDR_PANE_ID", func(i *Invocation) *string { return &i.PaneID }},
		{"context", "HERDR_PLUGIN_CONTEXT_JSON", func(i *Invocation) *string { return &i.ContextJSON }},
	}
}

// InvocationFromEnviron reads the plugin environment out of "KEY=value"
// entries, as returned by os.Environ. Later entries win, matching how the
// operating system resolves duplicate keys.
func InvocationFromEnviron(environ []string) Invocation {
	var inv Invocation
	fields := invocationFields()
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		for _, f := range fields {
			if f.key == key {
				*f.value(&inv) = value
			}
		}
	}
	return inv
}

// Trigger names what started this invocation: the startup hook, an action,
// an event hook or a pane entrypoint.
func (inv *Invocation) Trigger() string {
	switch {
	case inv.Event == "startup":
		return "startup"
	case inv.Event != "":
		return "event " + inv.Event
	case inv.ActionID != "":
		return "action " + inv.ActionID
	case inv.EntrypointID != "":
		return "pane " + inv.EntrypointID
	}
	return "unknown"
}

// ErrNotPluginInvocation reports that the process was not started by a Herdr
// plugin command.
var ErrNotPluginInvocation = errors.New("not started by a Herdr plugin command: HERDR_PLUGIN_ID is not set; invoke through `herdr plugin action invoke`, a startup hook or a plugin pane")

// Validate reports whether the environment identifies a plugin invocation
// completely enough to act on.
func (inv *Invocation) Validate() error {
	if inv.PluginID == "" {
		return ErrNotPluginInvocation
	}
	if inv.SocketPath == "" {
		return fmt.Errorf("plugin %s was invoked without HERDR_SOCKET_PATH; the Herdr server did not inject its socket", inv.PluginID)
	}
	return nil
}

// Describe renders the invocation as deterministic "label: value" lines,
// one per plugin environment variable, with unset values marked explicitly.
func (inv *Invocation) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "trigger: %s\n", inv.Trigger())
	for _, f := range invocationFields() {
		value := *f.value(inv)
		if value == "" {
			value = "(unset)"
		}
		fmt.Fprintf(&b, "%s: %s\n", f.label, value)
	}
	return b.String()
}
