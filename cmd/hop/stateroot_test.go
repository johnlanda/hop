package main

import (
	"strings"
	"testing"
)

// mapGetenv returns a getenv func over a fixed environment map.
func mapGetenv(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

func TestResolveStateRoot(t *testing.T) {
	cases := []struct {
		name       string
		env        map[string]string
		wantRoot   string
		wantSource string
		wantErr    string
	}{
		{
			name:       "HOP_STATE_DIR overrides everything",
			env:        map[string]string{"HOP_STATE_DIR": "/custom/state", "XDG_STATE_HOME": "/xdg", "HOME": "/home/u"},
			wantRoot:   "/custom/state",
			wantSource: "override",
		},
		{
			name:       "override is cleaned lexically",
			env:        map[string]string{"HOP_STATE_DIR": "/custom//state/./hop/.."},
			wantRoot:   "/custom/state",
			wantSource: "override",
		},
		{
			name:    "relative HOP_STATE_DIR is refused",
			env:     map[string]string{"HOP_STATE_DIR": "state", "HOME": "/home/u"},
			wantErr: "HOP_STATE_DIR is set to a relative path",
		},
		{
			name:       "XDG_STATE_HOME default",
			env:        map[string]string{"XDG_STATE_HOME": "/xdg/state", "HOME": "/home/u"},
			wantRoot:   "/xdg/state/hop",
			wantSource: "default",
		},
		{
			name:       "relative XDG_STATE_HOME is ignored in favor of HOME",
			env:        map[string]string{"XDG_STATE_HOME": "xdg", "HOME": "/home/u"},
			wantRoot:   "/home/u/.local/state/hop",
			wantSource: "default",
		},
		{
			name:       "HOME default",
			env:        map[string]string{"HOME": "/home/u"},
			wantRoot:   "/home/u/.local/state/hop",
			wantSource: "default",
		},
		{
			name:    "no override and no absolute HOME fails",
			env:     map[string]string{"HOME": "relative"},
			wantErr: "cannot resolve the state root",
		},
		{
			name:    "empty environment fails",
			env:     map[string]string{},
			wantErr: "cannot resolve the state root",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, source, err := resolveStateRoot(mapGetenv(tc.env))

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveStateRoot: %v", err)
			}
			if root != tc.wantRoot {
				t.Errorf("root = %q, want %q", root, tc.wantRoot)
			}
			if source != tc.wantSource {
				t.Errorf("source = %q, want %q", source, tc.wantSource)
			}
		})
	}
}

func TestStateRootDiagnosticsNeverEchoTheValue(t *testing.T) {
	// A malformed HOP_STATE_DIR can carry a pasted sensitive value; both
	// resolvers must name the variable only.
	const sentinel = "s3kr3t-pasted-value/rel"
	env := map[string]string{"HOP_STATE_DIR": sentinel, "HOME": "/home/u"}

	if _, _, err := resolveStateRoot(mapGetenv(env)); err == nil || strings.Contains(err.Error(), "s3kr3t") {
		t.Errorf("resolveStateRoot error discloses the value: %v", err)
	}
	if _, err := requireWorkerStateRoot(mapGetenv(env)); err == nil || strings.Contains(err.Error(), "s3kr3t") {
		t.Errorf("requireWorkerStateRoot error discloses the value: %v", err)
	}
}

func TestRequireWorkerStateRoot(t *testing.T) {
	cases := []struct {
		name     string
		env      map[string]string
		wantRoot string
		wantErr  string
	}{
		{
			name:     "absolute HOP_STATE_DIR is accepted",
			env:      map[string]string{"HOP_STATE_DIR": "/state/root"},
			wantRoot: "/state/root",
		},
		{
			name:    "missing HOP_STATE_DIR names the lost variable",
			env:     map[string]string{"XDG_STATE_HOME": "/xdg", "HOME": "/home/u"},
			wantErr: "HOP_STATE_DIR is not set",
		},
		{
			name:    "relative HOP_STATE_DIR is refused, never resolved",
			env:     map[string]string{"HOP_STATE_DIR": "some/dir", "HOME": "/home/u"},
			wantErr: "HOP_STATE_DIR is not an absolute path",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, err := requireWorkerStateRoot(mapGetenv(tc.env))

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("requireWorkerStateRoot: %v", err)
			}
			if root != tc.wantRoot {
				t.Errorf("root = %q, want %q", root, tc.wantRoot)
			}
		})
	}
}
