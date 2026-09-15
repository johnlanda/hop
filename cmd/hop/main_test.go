package main

import (
	"bytes"
	"errors"
	"io"
	"runtime/debug"
	"strings"
	"testing"
)

// failingWriter fails every write, standing in for a closed output stream.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("stream closed")
}

func TestRunReportsWriteFailuresThroughTheExitCode(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		stdout io.Writer
		stderr io.Writer
	}{
		{name: "usage cannot be written", args: nil, stdout: io.Discard, stderr: failingWriter{}},
		{name: "unknown command cannot be reported", args: []string{"frobnicate"}, stdout: io.Discard, stderr: failingWriter{}},
		{name: "help cannot be written", args: []string{"help"}, stdout: failingWriter{}, stderr: io.Discard},
		{name: "version cannot be written", args: []string{"version"}, stdout: failingWriter{}, stderr: io.Discard},
		{name: "version argument error cannot be reported", args: []string{"version", "x"}, stdout: io.Discard, stderr: failingWriter{}},
		{name: "version flag error cannot be reported", args: []string{"version", "-json"}, stdout: io.Discard, stderr: failingWriter{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code := run(tc.args, tc.stdout, tc.stderr)

			if code != exitFailure {
				t.Errorf("exit code = %d, want %d", code, exitFailure)
			}
		})
	}
}

func TestRun(t *testing.T) {
	cases := []struct {
		name         string
		args         []string
		wantCode     int
		wantStdout   string
		wantStderr   string
		wantNoStdout bool
	}{
		{
			name:         "no arguments prints usage to stderr",
			args:         nil,
			wantCode:     exitUsage,
			wantStderr:   "Usage: hop <command>",
			wantNoStdout: true,
		},
		{
			name:         "unknown command is reported with usage",
			args:         []string{"frobnicate"},
			wantCode:     exitUsage,
			wantStderr:   `unknown command "frobnicate"`,
			wantNoStdout: true,
		},
		{
			name:       "help prints usage to stdout",
			args:       []string{"help"},
			wantCode:   exitOK,
			wantStdout: "Usage: hop <command>",
		},
		{
			name:       "version prints the resolved version and platform",
			args:       []string{"version"},
			wantCode:   exitOK,
			wantStdout: "hop version ",
		},
		{
			name:         "version rejects positional arguments",
			args:         []string{"version", "extra"},
			wantCode:     exitUsage,
			wantStderr:   `unexpected argument "extra"`,
			wantNoStdout: true,
		},
		{
			name:         "version rejects unknown flags",
			args:         []string{"version", "-json"},
			wantCode:     exitUsage,
			wantStderr:   "flag provided but not defined: -json",
			wantNoStdout: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := run(tc.args, &stdout, &stderr)

			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d (stderr: %q)", code, tc.wantCode, stderr.String())
			}
			if tc.wantNoStdout && stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
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

func TestResolveVersion(t *testing.T) {
	cases := []struct {
		name    string
		stamped string
		info    *debug.BuildInfo
		want    string
	}{
		{
			name:    "stamped version wins over build information",
			stamped: "v1.2.3",
			info:    &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}},
			want:    "v1.2.3",
		},
		{
			name: "missing build information reports devel",
			want: devVersion,
		},
		{
			name: "module version from build information",
			info: &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}},
			want: "v0.1.0",
		},
		{
			name: "placeholder module version falls back to the revision",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"},
					{Key: "vcs.modified", Value: "false"},
				},
			},
			want: "devel+0123456789ab",
		},
		{
			name: "modified working tree is marked",
			info: &debug.BuildInfo{
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "abcdef0"},
					{Key: "vcs.modified", Value: "true"},
				},
			},
			want: "devel+abcdef0.modified",
		},
		{
			name: "no version and no revision reports devel",
			info: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			want: devVersion,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveVersion(tc.stamped, tc.info)

			if got != tc.want {
				t.Errorf("resolveVersion(%q, ...) = %q, want %q", tc.stamped, got, tc.want)
			}
		})
	}
}
