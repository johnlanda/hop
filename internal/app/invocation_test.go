package app_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

func TestInvocationFromEnviron(t *testing.T) {
	cases := []struct {
		name    string
		environ []string
		want    app.Invocation
	}{
		{
			name: "full action environment",
			environ: []string{
				"PATH=/usr/bin",
				"HERDR_PLUGIN_ID=hop",
				"HERDR_PLUGIN_ACTION_ID=doctor",
				"HERDR_SOCKET_PATH=/tmp/h.sock",
				"HERDR_BIN_PATH=/opt/herdr",
				"HERDR_PLUGIN_ROOT=/plug",
				"HERDR_PLUGIN_CONFIG_DIR=/cfg",
				"HERDR_PLUGIN_STATE_DIR=/state",
				"HERDR_WORKSPACE_ID=w1",
				"HERDR_TAB_ID=w1:t1",
				"HERDR_PANE_ID=w1:p1",
				"HERDR_PLUGIN_CONTEXT_JSON={\"workspace_id\":\"w1\"}",
			},
			want: app.Invocation{
				PluginID: "hop", ActionID: "doctor",
				SocketPath: "/tmp/h.sock", BinaryPath: "/opt/herdr",
				Root: "/plug", ConfigDir: "/cfg", StateDir: "/state",
				WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1",
				ContextJSON: "{\"workspace_id\":\"w1\"}",
			},
		},
		{
			name:    "empty environment",
			environ: nil,
			want:    app.Invocation{},
		},
		{
			name: "later duplicate wins and values keep equals signs",
			environ: []string{
				"HERDR_PLUGIN_ID=first",
				"HERDR_PLUGIN_ID=second",
				"HERDR_PLUGIN_CONTEXT_JSON={\"a\":\"b=c\"}",
			},
			want: app.Invocation{PluginID: "second", ContextJSON: "{\"a\":\"b=c\"}"},
		},
		{
			name:    "malformed entries are ignored",
			environ: []string{"NOEQUALS", "HERDR_PLUGIN_ID=hop"},
			want:    app.Invocation{PluginID: "hop"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := app.InvocationFromEnviron(tc.environ)

			if got != tc.want {
				t.Errorf("InvocationFromEnviron() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestInvocationTrigger(t *testing.T) {
	cases := []struct {
		name string
		inv  app.Invocation
		want string
	}{
		{name: "startup hook", inv: app.Invocation{Event: "startup"}, want: "startup"},
		{name: "event hook", inv: app.Invocation{Event: "worktree.created"}, want: "event worktree.created"},
		{name: "action", inv: app.Invocation{ActionID: "doctor"}, want: "action doctor"},
		{name: "pane entrypoint", inv: app.Invocation{EntrypointID: "doctor"}, want: "pane doctor"},
		{name: "event outranks action", inv: app.Invocation{Event: "startup", ActionID: "doctor"}, want: "startup"},
		{name: "nothing set", inv: app.Invocation{}, want: "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.inv.Trigger(); got != tc.want {
				t.Errorf("Trigger() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInvocationValidate(t *testing.T) {
	cases := []struct {
		name    string
		inv     app.Invocation
		wantErr string
	}{
		{
			name: "valid invocation",
			inv:  app.Invocation{PluginID: "hop", SocketPath: "/tmp/h.sock"},
		},
		{
			name:    "outside a plugin command",
			inv:     app.Invocation{},
			wantErr: "HERDR_PLUGIN_ID is not set",
		},
		{
			name:    "missing socket",
			inv:     app.Invocation{PluginID: "hop"},
			wantErr: "without HERDR_SOCKET_PATH",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.inv.Validate()

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
	t.Run("missing plugin id is the sentinel", func(t *testing.T) {
		var empty app.Invocation
		if !errors.Is(empty.Validate(), app.ErrNotPluginInvocation) {
			t.Fatal("Validate() on an empty invocation is not ErrNotPluginInvocation")
		}
	})
}

func TestInvocationDescribe(t *testing.T) {
	inv := app.Invocation{
		PluginID:   "hop",
		Event:      "startup",
		SocketPath: "/tmp/h.sock",
		StateDir:   "/state",
	}

	got := inv.Describe()

	for _, line := range []string{
		"trigger: startup\n",
		"plugin: hop\n",
		"action: (unset)\n",
		"socket: /tmp/h.sock\n",
		"state dir: /state\n",
		"context: (unset)\n",
	} {
		if !strings.Contains(got, line) {
			t.Errorf("Describe() lacks %q; got:\n%s", line, got)
		}
	}
	if !strings.HasPrefix(got, "trigger: ") {
		t.Errorf("Describe() must start with the trigger line; got:\n%s", got)
	}
}
