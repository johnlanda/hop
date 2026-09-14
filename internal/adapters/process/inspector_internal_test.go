package process

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeStubPS installs an executable stub standing in for ps.
func writeStubPS(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ps")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil { //nolint:gosec // G306: the stub must be executable; it lives in this test's private temporary directory.
		t.Fatal(err)
	}
	return path
}

func TestGroupProcessesListingFailuresAreErrors(t *testing.T) {
	cases := []struct {
		name        string
		stub        func(t *testing.T) string
		wantMessage string
	}{
		{
			name:        "ps exits nonzero",
			stub:        func(t *testing.T) string { t.Helper(); return writeStubPS(t, "echo boom >&2\nexit 1\n") },
			wantMessage: "boom",
		},
		{
			name:        "ps binary missing",
			stub:        func(t *testing.T) string { t.Helper(); return filepath.Join(t.TempDir(), "no-ps-here") },
			wantMessage: "list the process table",
		},
		{
			name:        "row with too few columns",
			stub:        func(t *testing.T) string { t.Helper(); return writeStubPS(t, "echo 4242\n") },
			wantMessage: "unparseable ps row",
		},
		{
			name:        "row with a non-numeric pid",
			stub:        func(t *testing.T) string { t.Helper(); return writeStubPS(t, "echo 'garbage 4242'\n") },
			wantMessage: "unparseable pid",
		},
		{
			name:        "row with a non-numeric pgid",
			stub:        func(t *testing.T) string { t.Helper(); return writeStubPS(t, "echo '17 garbage'\n") },
			wantMessage: "unparseable pgid",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inspector := GroupInspector{psPath: tc.stub(t)}

			_, err := inspector.GroupProcesses(t.Context(), 4242)

			if err == nil {
				t.Fatal("GroupProcesses succeeded, want a listing error")
			}
			if !strings.Contains(err.Error(), tc.wantMessage) {
				t.Errorf("error %q does not contain %q", err, tc.wantMessage)
			}
		})
	}
}

func TestGroupProcessesMarksUnreadableArgvUnavailable(t *testing.T) {
	// Pid 2147483646 is beyond every platform's pid range, so the exact
	// argv source must fail for it and the member must carry the explicit
	// marker instead of a guessed argv.
	inspector := GroupInspector{psPath: writeStubPS(t, "echo ' 2147483646  4242'\n")}

	members, err := inspector.GroupProcesses(t.Context(), 4242)
	if err != nil {
		t.Fatalf("GroupProcesses: %v", err)
	}

	if len(members) != 1 || members[0].PID != 2147483646 {
		t.Fatalf("members = %+v, want exactly pid 2147483646", members)
	}
	if !slices.Equal(members[0].Argv, []string{ArgvUnavailable}) {
		t.Errorf("argv = %q, want the single ArgvUnavailable marker", members[0].Argv)
	}
}

func TestGroupProcessesFiltersOtherGroups(t *testing.T) {
	inspector := GroupInspector{psPath: writeStubPS(t, "echo '10 4242'\necho '11 9999'\necho ''\n")}

	members, err := inspector.GroupProcesses(t.Context(), 9999)
	if err != nil {
		t.Fatalf("GroupProcesses: %v", err)
	}

	if len(members) != 1 || members[0].PID != 11 {
		t.Errorf("members = %+v, want exactly pid 11 of group 9999", members)
	}
}
