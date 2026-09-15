package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// emptyGetenv is the hermetic environment for doctor tests: no HERDR_*
// defaults, so nothing can reach a live session on the developer machine.
func emptyGetenv(string) string { return "" }

// writeFailingHerdrStub writes an executable that exists but answers nothing,
// so the binary probe finds it and the schema probe fails.
func writeFailingHerdrStub(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "herdr")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil { //nolint:gosec // G306: the stub must be executable; it lives in a private test directory.
		t.Fatal(err)
	}
	return path
}

func TestRunDoctorUsage(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{name: "rejects positional arguments", args: []string{"extra"}, wantStderr: `unexpected argument "extra"`},
		{name: "rejects unknown flags", args: []string{"-json"}, wantStderr: "flag provided but not defined: -json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code, err := runDoctor(tc.args, &stdout, &stderr, emptyGetenv)
			if err != nil {
				t.Fatalf("write error: %v", err)
			}

			if code != exitUsage {
				t.Errorf("exit code = %d, want %d", code, exitUsage)
			}
			if !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}

func TestRunDoctorUnhealthyInstallationExitsFailure(t *testing.T) {
	stub := writeFailingHerdrStub(t)
	t.Setenv("PATH", filepath.Dir(stub)) // Harness lookups must miss quickly and hermetically.
	var stdout, stderr bytes.Buffer

	code, err := runDoctor([]string{"-herdr", stub}, &stdout, &stderr, emptyGetenv)
	if err != nil {
		t.Fatalf("write error: %v", err)
	}

	if code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	out := stdout.String()
	for _, want := range []string{
		"socket API schema", "unavailable",
		"feature plugin actions", "skipped",
		"server socket", "no socket configured",
		"harness ",
		"problem(s) found",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q; got:\n%s", want, out)
		}
	}
}

func TestRunDoctorMissingBinaryDoesNotGuess(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var stdout, stderr bytes.Buffer

	code, err := runDoctor(nil, &stdout, &stderr, emptyGetenv)
	if err != nil {
		t.Fatalf("write error: %v", err)
	}

	if code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	out := stdout.String()
	if !strings.Contains(out, "install Herdr") {
		t.Errorf("stdout lacks the install advice; got:\n%s", out)
	}
	if strings.Contains(out, "ok           feature") {
		t.Errorf("a feature is reported ok although nothing was probed; got:\n%s", out)
	}
}

func TestRunDoctorStateRootLine(t *testing.T) {
	stub := writeFailingHerdrStub(t)
	t.Setenv("PATH", filepath.Dir(stub)) // Harness lookups must miss quickly and hermetically.

	t.Run("override with an existing store file", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "hop.db"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer

		if _, err := runDoctor([]string{"-herdr", stub}, &stdout, &stderr, mapGetenv(map[string]string{"HOP_STATE_DIR": root})); err != nil {
			t.Fatalf("write error: %v", err)
		}

		want := "state root: " + root + " (override; store present)"
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout lacks %q; got:\n%s", want, stdout.String())
		}
	})

	t.Run("default with no store yet", func(t *testing.T) {
		home := t.TempDir()
		var stdout, stderr bytes.Buffer

		if _, err := runDoctor([]string{"-herdr", stub}, &stdout, &stderr, mapGetenv(map[string]string{"HOME": home})); err != nil {
			t.Fatalf("write error: %v", err)
		}

		want := "state root: " + filepath.Join(home, ".local", "state", "hop") + " (default; store absent (created on first run))"
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout lacks %q; got:\n%s", want, stdout.String())
		}
	})

	t.Run("relative override is reported and unhealthy", func(t *testing.T) {
		var stdout, stderr bytes.Buffer

		code, err := runDoctor([]string{"-herdr", stub}, &stdout, &stderr, mapGetenv(map[string]string{"HOP_STATE_DIR": "rel/state"}))
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
		want := `unavailable  state root: HOP_STATE_DIR "rel/state" is not an absolute path`
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout lacks %q; got:\n%s", want, stdout.String())
		}
	})
}

func TestRunDoctorReportsWriteFailures(t *testing.T) {
	stub := writeFailingHerdrStub(t)
	// A hermetic PATH so the harness probe cannot execute a real, locally
	// installed claude/codex/opencode --version during a unit test.
	t.Setenv("PATH", t.TempDir())

	_, err := runDoctor([]string{"-herdr", stub}, failingWriter{}, io.Discard, emptyGetenv)

	if err == nil {
		t.Fatal("runDoctor did not report the stdout write failure")
	}
}

func TestRunDoctorDoesNotExecuteHarnessesFromInheritedPath(t *testing.T) {
	// A fake claude that records execution by touching a marker sits on the
	// inherited PATH, as a real harness would. The doctor unit test must run
	// on a hermetic PATH that cannot reach it, so the marker stays absent.
	// Remove the hermetic override and the fake would run and touch the
	// marker, so this asserts the override actually protects against executing
	// whatever native harness is installed on the developer's PATH.
	marker := filepath.Join(t.TempDir(), "claude-ran")
	fakeDir := t.TempDir()
	fake := "#!/bin/sh\n: > " + marker + "\necho 'claude 0.0.0-fake'\n"
	if err := os.WriteFile(filepath.Join(fakeDir, "claude"), []byte(fake), 0o755); err != nil { //nolint:gosec // G306: the fake must be executable; it lives in a private test directory.
		t.Fatal(err)
	}
	// Make the fake reachable as it would be on a developer's PATH, then apply
	// the hermetic override the doctor unit test relies on. The override is the
	// last PATH set, so it wins; a mutation that drops it leaves the fake
	// reachable.
	t.Setenv("PATH", fakeDir)
	t.Setenv("PATH", t.TempDir())
	stub := writeFailingHerdrStub(t)

	if _, err := runDoctor([]string{"-herdr", stub}, io.Discard, io.Discard, emptyGetenv); err != nil {
		t.Fatalf("runDoctor write error: %v", err)
	}

	if _, err := os.Stat(marker); err == nil {
		t.Errorf("doctor executed a harness reachable on the inherited PATH; marker %s exists", marker)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat marker: %v", err)
	}
}
