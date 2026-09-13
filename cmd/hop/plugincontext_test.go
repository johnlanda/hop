package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestRunPluginContext(t *testing.T) {
	pluginEnviron := []string{
		"HERDR_PLUGIN_ID=hop",
		"HERDR_PLUGIN_EVENT=startup",
		"HERDR_SOCKET_PATH=/tmp/h.sock",
		"HERDR_PLUGIN_STATE_DIR=/state",
	}
	cases := []struct {
		name       string
		args       []string
		environ    []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{
			name:       "startup invocation is described on stdout",
			environ:    pluginEnviron,
			wantCode:   exitOK,
			wantStdout: "trigger: startup\nplugin: hop\n",
		},
		{
			name:       "outside a plugin command fails with guidance",
			environ:    []string{"PATH=/usr/bin"},
			wantCode:   exitFailure,
			wantStderr: "HERDR_PLUGIN_ID is not set",
		},
		{
			name:       "missing socket fails with the injection explanation",
			environ:    []string{"HERDR_PLUGIN_ID=hop"},
			wantCode:   exitFailure,
			wantStderr: "without HERDR_SOCKET_PATH",
		},
		{
			name:       "rejects positional arguments",
			args:       []string{"extra"},
			environ:    pluginEnviron,
			wantCode:   exitUsage,
			wantStderr: `unexpected argument "extra"`,
		},
		{
			name:       "rejects unknown flags",
			args:       []string{"-json"},
			environ:    pluginEnviron,
			wantCode:   exitUsage,
			wantStderr: "flag provided but not defined: -json",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code, err := runPluginContext(tc.args, &stdout, &stderr, tc.environ)
			if err != nil {
				t.Fatalf("write error: %v", err)
			}

			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d (stderr: %q)", code, tc.wantCode, stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tc.wantStdout)
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}

func TestRunPluginContextReportsWriteFailures(t *testing.T) {
	environ := []string{"HERDR_PLUGIN_ID=hop", "HERDR_SOCKET_PATH=/tmp/h.sock"}

	if _, err := runPluginContext(nil, failingWriter{}, io.Discard, environ); err == nil {
		t.Fatal("runPluginContext did not report the stdout write failure")
	}
	if _, err := runPluginContext(nil, io.Discard, failingWriter{}, nil); err == nil {
		t.Fatal("runPluginContext did not report the stderr write failure")
	}
}
