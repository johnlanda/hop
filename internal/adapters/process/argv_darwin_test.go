package process

import (
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// buildProcargs2 assembles a synthetic kern.procargs2 buffer: argc, the
// executable path padded with NULs to the 8-byte boundary, the argv
// entries NUL-terminated, then the trailing tail bytes verbatim.
func buildProcargs2(argc int, path string, argv []string, tail []byte) []byte {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, uint32(argc)) //nolint:gosec // G115: test fixture; argc values are small non-negative table constants.
	region := (len(path) + 1 + procargs2PathAlign - 1) / procargs2PathAlign * procargs2PathAlign
	pathRegion := make([]byte, region)
	copy(pathRegion, path)
	buf = append(buf, pathRegion...)
	for _, arg := range argv {
		buf = append(buf, arg...)
		buf = append(buf, 0)
	}
	return append(buf, tail...)
}

func TestParseProcargs2Table(t *testing.T) {
	env := []byte("SECRET_MARKER=value\x00OTHER=x\x00")
	cases := []struct {
		name    string
		raw     []byte
		want    []string
		wantErr string
	}{
		{
			name: "plain vector",
			raw:  buildProcargs2(2, "/bin/sleep", []string{"/bin/sleep", "30"}, env),
			want: []string{"/bin/sleep", "30"},
		},
		{
			name: "empty leading argument is not padding",
			raw:  buildProcargs2(2, "/bin/sleep", []string{"", "30"}, env),
			want: []string{"", "30"},
		},
		{
			name: "empty middle and trailing arguments",
			raw:  buildProcargs2(4, "/bin/sleep", []string{"a", "", "b", ""}, env),
			want: []string{"a", "", "b", ""},
		},
		{
			name: "all arguments empty",
			raw:  buildProcargs2(2, "/bin/sleep", []string{"", ""}, env),
			want: []string{"", ""},
		},
		{
			name: "argc zero is an empty vector",
			raw:  buildProcargs2(0, "/bin/sleep", nil, env),
			want: []string{},
		},
		{
			name: "path exactly filling its region keeps an empty argv[0]",
			raw:  buildProcargs2(2, "/bin/x6", []string{"", "y"}, env), // len(path)+1 == 8: zero padding
			want: []string{"", "y"},
		},
		{
			name:    "truncated below the argc word",
			raw:     []byte{1, 0},
			wantErr: "truncated",
		},
		{
			name:    "negative argc",
			raw:     buildProcargs2(-1, "/bin/sleep", nil, nil),
			wantErr: "negative",
		},
		{
			name:    "unterminated executable path",
			raw:     append([]byte{1, 0, 0, 0}, "/bin/sleep"...),
			wantErr: "not NUL-terminated",
		},
		{
			name:    "buffer ends inside the path padding",
			raw:     append([]byte{1, 0, 0, 0}, "/bin/sleep\x00"...),
			wantErr: "ends inside the executable-path region",
		},
		{
			name:    "non-NUL byte inside the padding is never a guessed boundary",
			raw:     append([]byte{1, 0, 0, 0}, "/bin/sleep\x00x\x00\x00\x00\x00a\x00"...),
			wantErr: "padding holds a non-NUL byte",
		},
		{
			name:    "buffer ends inside an argument, never reading past",
			raw:     buildProcargs2(3, "/bin/sleep", []string{"a", "b"}, nil),
			wantErr: "ends inside argument 2 of 3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argv, err := parseProcargs2(tc.raw)

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseProcargs2: %v", err)
			}
			if !slices.Equal(argv, tc.want) {
				t.Errorf("argv = %q, want %q", argv, tc.want)
			}
			for _, arg := range argv {
				if strings.Contains(arg, "SECRET_MARKER") {
					t.Errorf("argument %q carries an environment string", arg)
				}
			}
		})
	}
}

// TestProcessArgvExactWithEmptyArguments proves the whole read path against
// real children whose argv carries empty leading, middle and trailing
// arguments, and that no environment string ever appears as an argument.
func TestProcessArgvExactWithEmptyArguments(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		argv []string
	}{
		{name: "empty argv0", argv: []string{"", "x"}},
		{name: "empty middle argument", argv: []string{exe, "", "x"}},
		{name: "two leading empties", argv: []string{"", "", "x"}},
		{name: "whitespace and trailing empty", argv: []string{exe, "with space", "with\ttab", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			child := exec.CommandContext(t.Context(), exe)
			child.Args = tc.argv
			child.Env = []string{
				"HOP_PROCESS_HELPER=sleeper",
				"HOP_HELPER_PIDFILE=" + pidFile,
				"ARGV_TEST_ENV_MARKER=must-never-appear",
			}
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if killErr := child.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
					t.Logf("kill fixture child: %v", killErr)
				}
				if waitErr := child.Wait(); waitErr != nil {
					var exitErr *exec.ExitError
					if !errors.As(waitErr, &exitErr) {
						t.Logf("reap fixture child: %v", waitErr)
					}
				}
			})
			awaitFile(t, pidFile)

			argv, err := processArgv(child.Process.Pid)
			if err != nil {
				t.Fatalf("processArgv: %v", err)
			}

			if !slices.Equal(argv, tc.argv) {
				t.Errorf("argv = %q, want the exact requested vector %q", argv, tc.argv)
			}
			for _, arg := range argv {
				if strings.Contains(arg, "ARGV_TEST_ENV_MARKER") || strings.Contains(arg, "must-never-appear") {
					t.Errorf("argument %q carries an environment string", arg)
				}
			}
		})
	}
}

// awaitFile polls for a fixture's published file with a bounded deadline.
func awaitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
