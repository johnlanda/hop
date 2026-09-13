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

func TestRunDoctorReportsWriteFailures(t *testing.T) {
	stub := writeFailingHerdrStub(t)

	_, err := runDoctor([]string{"-herdr", stub}, failingWriter{}, io.Discard, emptyGetenv)

	if err == nil {
		t.Fatal("runDoctor did not report the stdout write failure")
	}
}
