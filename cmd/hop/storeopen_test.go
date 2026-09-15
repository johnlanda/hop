package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sentinelStateRoot returns an ABSOLUTE path that passes the resolvers'
// shape checks but is a regular file, so sqlite.Open's MkdirAll fails with
// a path-bearing error carrying the complete value.
func sentinelStateRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "s3kr3t-pasted-value")
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestStoreOpenDiagnosticsNeverEchoTheRoot drives each command through the
// REAL store opener against a regular file standing in the state root's
// place: the classified diagnostic must name where the root came from and
// never the value the environment carried.
func TestStoreOpenDiagnosticsNeverEchoTheRoot(t *testing.T) {
	commands := []struct {
		name     string
		location string
		invoke   func(td *testDeps, stdout, stderr *bytes.Buffer) (int, error)
	}{
		{
			name:     "hop launch",
			location: "HOP_STATE_DIR",
			invoke: func(td *testDeps, stdout, stderr *bytes.Buffer) (int, error) {
				return runLaunch([]string{"--run", testRunID, "--attempt", testAttemptID}, stdout, stderr, td.deps)
			},
		},
		{
			name:     "hop check-exec",
			location: "HOP_STATE_DIR",
			invoke: func(td *testDeps, stdout, stderr *bytes.Buffer) (int, error) {
				return runCheckExec([]string{"--op", testOperationID, "--", "sh", "check.sh"}, stdout, stderr, td.deps)
			},
		},
		{
			name:     "hop result submit",
			location: "HOP_STATE_DIR",
			invoke: func(td *testDeps, stdout, stderr *bytes.Buffer) (int, error) {
				return runResult([]string{"submit", "--summary", "s", "--commit", testCommit}, stdout, stderr, td.deps)
			},
		},
		{
			name:     "hop status",
			location: "the resolved state root",
			invoke: func(td *testDeps, stdout, stderr *bytes.Buffer) (int, error) {
				return runStatus(nil, stdout, stderr, td.deps)
			},
		},
	}
	for _, tc := range commands {
		t.Run(tc.name, func(t *testing.T) {
			root := sentinelStateRoot(t)
			td := newTestDeps(&fakeController{}, map[string]string{
				"HOP_STATE_DIR":      root,
				"HOP_INCARNATION_ID": testIncarnationID,
				"HOME":               "/home/u",
			}, t.TempDir())
			// The real composition-root opener, so the diagnostic is
			// classified from the real sqlite/filesystem error chain.
			td.deps.openController = openController
			var stdout, stderr bytes.Buffer

			code, err := tc.invoke(td, &stdout, &stderr)
			if err != nil {
				t.Fatalf("write error: %v", err)
			}

			if code != exitFailure {
				t.Errorf("exit code = %d, want %d", code, exitFailure)
			}
			combined := stdout.String() + stderr.String()
			if strings.Contains(combined, "s3kr3t") {
				t.Errorf("output echoes the state-root value:\n%s", combined)
			}
			if !strings.Contains(stderr.String(), "cannot open the state store under "+tc.location) {
				t.Errorf("stderr = %q, want the classified diagnostic naming %s", stderr.String(), tc.location)
			}
			if !strings.Contains(stderr.String(), "not a directory") {
				t.Errorf("stderr = %q, want the not-a-directory category", stderr.String())
			}
		})
	}
}

// TestOpenControllerGitExecutableNotFound proves the real composition
// (openController) refuses before opening the store when git cannot be
// resolved on the controller's own PATH: a fixed, value-free diagnostic
// naming neither the PATH value nor the state root. This drives
// openController directly rather than through a command: every command
// that reaches it re-describes ANY openController failure through
// describeStoreOpenFailure's store-open classification (unrelated,
// pre-existing behavior this change does not touch), which would obscure
// the exact message asserted here.
func TestOpenControllerGitExecutableNotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // a directory that contains no executables at all
	_, _, err := openController(t.Context(), controllerConfig{stateRoot: t.TempDir()})
	if err == nil {
		t.Fatal("openController() succeeded despite git not being resolvable on PATH")
	}
	if err.Error() != "git executable not found on PATH" {
		t.Errorf("openController() error = %q, want the fixed, value-free git-not-found message", err.Error())
	}
}
