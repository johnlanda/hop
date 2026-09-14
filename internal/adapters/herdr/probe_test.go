package herdr_test

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/herdr"
)

// writeStub writes an executable shell script that plays the part of an
// installed binary.
func writeStub(t *testing.T, dir, name, script string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil { //nolint:gosec // G306: the stub must be executable; it lives in a private test directory.
		t.Fatal(err)
	}
	return path
}

// stubSchema is a minimal schema document in the shape `herdr api schema
// --json` prints: method constants nested under the request schema.
const stubSchema = `{
  "protocol": 22,
  "schemas": {
    "request": {
      "oneOf": [
        {"properties": {"method": {"const": "ping"}}},
        {"properties": {"method": {"const": "plugin.link"}}},
        {"$defs": {"nested": {"properties": {"method": {"const": "agent.view.set"}}}}}
      ]
    }
  }
}`

func TestInstallationProbeBinary(t *testing.T) {
	dir := t.TempDir()
	stub := writeStub(t, dir, "herdr", `case "$1" in --version) echo "herdr 9.9.9-test";; esac`)
	probe := &herdr.InstallationProbe{BinaryPath: stub}

	info, err := probe.Binary(t.Context())
	if err != nil {
		t.Fatalf("Binary: %v", err)
	}
	if info.Path != stub || info.Version != "herdr 9.9.9-test" {
		t.Errorf("Binary = %+v, want path %s and version herdr 9.9.9-test", info, stub)
	}
}

func TestInstallationProbeBinaryScrubsHerdrEnvironment(t *testing.T) {
	dir := t.TempDir()
	stub := writeStub(t, dir, "herdr", `case "$1" in --version) echo "session=${HERDR_SESSION:-none}";; esac`)
	t.Setenv("HERDR_SESSION", "leaked")
	probe := &herdr.InstallationProbe{BinaryPath: stub}

	info, err := probe.Binary(t.Context())
	if err != nil {
		t.Fatalf("Binary: %v", err)
	}
	if info.Version != "session=none" {
		t.Errorf("version = %q; the probe leaked HERDR_* into the subprocess", info.Version)
	}
}

func TestInstallationProbeBinaryMissing(t *testing.T) {
	t.Run("explicit path that does not exist", func(t *testing.T) {
		probe := &herdr.InstallationProbe{BinaryPath: filepath.Join(t.TempDir(), "missing")}

		_, err := probe.Binary(t.Context())

		if err == nil {
			t.Fatal("Binary succeeded for a missing explicit path")
		}
	})
	t.Run("not on PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		probe := &herdr.InstallationProbe{}

		_, err := probe.Binary(t.Context())

		if err == nil || !strings.Contains(err.Error(), "not on PATH") {
			t.Fatalf("Binary error = %v, want a PATH lookup failure", err)
		}
	})
}

func TestInstallationProbeBinaryWithoutVersionIsStillFound(t *testing.T) {
	dir := t.TempDir()
	// A real executable that runs but exits nonzero for --version is found
	// with an unknown version, distinct from a path that cannot be executed.
	stub := writeStub(t, dir, "herdr", "exit 3")
	probe := &herdr.InstallationProbe{BinaryPath: stub}

	info, err := probe.Binary(t.Context())
	if err != nil {
		t.Fatalf("Binary: %v", err)
	}
	if info.Path != stub || info.Version != "" {
		t.Errorf("Binary = %+v, want the path with an empty version", info)
	}
}

func TestInstallationProbeBinaryRejectsNonExecutablePaths(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T) string
		wantErr string
	}{
		{
			name:    "a directory is not a binary",
			setup:   func(t *testing.T) string { return t.TempDir() },
			wantErr: "is a directory",
		},
		{
			name: "a non-executable regular file is not a binary",
			setup: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "herdr")
				if err := os.WriteFile(path, []byte("not executable"), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
			wantErr: "is not executable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := &herdr.InstallationProbe{BinaryPath: tc.setup(t)}

			_, err := probe.Binary(t.Context())

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Binary error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestInstallationProbeSchema(t *testing.T) {
	dir := t.TempDir()
	script := fmt.Sprintf(`case "$1" in
--version) echo "herdr 9.9.9-test";;
api) cat <<'SCHEMA'
%s
SCHEMA
;;
esac`, stubSchema)
	stub := writeStub(t, dir, "herdr", script)
	probe := &herdr.InstallationProbe{BinaryPath: stub}

	schema, err := probe.Schema(t.Context())
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	if schema.Protocol != 22 {
		t.Errorf("protocol = %d, want 22", schema.Protocol)
	}
	want := []string{"agent.view.set", "ping", "plugin.link"}
	if !slices.Equal(schema.Methods, want) {
		t.Errorf("methods = %v, want %v (sorted, nested constants included)", schema.Methods, want)
	}
}

func TestInstallationProbeSchemaFailures(t *testing.T) {
	cases := []struct {
		name    string
		script  string
		wantErr string
	}{
		{
			name:    "schema command fails",
			script:  `case "$1" in --version) echo herdr;; api) echo "no server" >&2; exit 1;; esac`,
			wantErr: "api schema",
		},
		{
			name:    "schema is not JSON",
			script:  `case "$1" in --version) echo herdr;; api) echo "not json";; esac`,
			wantErr: "decode schema document",
		},
		{
			name:    "schema has no request section",
			script:  `case "$1" in --version) echo herdr;; api) echo '{"protocol":22,"schemas":{}}';; esac`,
			wantErr: "no request schema",
		},
		{
			name:    "request section advertises no methods",
			script:  `case "$1" in --version) echo herdr;; api) echo '{"protocol":22,"schemas":{"request":{"oneOf":[]}}}';; esac`,
			wantErr: "no methods",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := writeStub(t, t.TempDir(), "herdr", tc.script)
			probe := &herdr.InstallationProbe{BinaryPath: stub}

			_, err := probe.Schema(t.Context())

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Schema error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestInstallationProbeHarness(t *testing.T) {
	dir := t.TempDir()
	writeStub(t, dir, "claude", `case "$1" in --version) echo "1.2.3 (Claude Code)";; esac`)
	t.Setenv("PATH", dir)
	probe := &herdr.InstallationProbe{}

	found, err := probe.Harness(t.Context(), "claude")
	if err != nil {
		t.Fatalf("Harness(claude): %v", err)
	}
	if found.Version != "1.2.3 (Claude Code)" {
		t.Errorf("claude version = %q, want 1.2.3 (Claude Code)", found.Version)
	}

	_, err = probe.Harness(t.Context(), "opencode")
	if err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("Harness(opencode) error = %v, want a PATH lookup failure", err)
	}
}

func TestInstallationProbePing(t *testing.T) {
	t.Run("no socket configured", func(t *testing.T) {
		probe := &herdr.InstallationProbe{}

		_, err := probe.Ping(t.Context())

		if err == nil || !strings.Contains(err.Error(), "no server socket configured") {
			t.Fatalf("Ping error = %v, want the unconfigured-socket explanation", err)
		}
	})
	t.Run("live socket answers", func(t *testing.T) {
		endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
			request := readRequestLine(t, bufio.NewReader(conn))
			if request == nil {
				return
			}
			if request["method"] != "ping" {
				t.Errorf("method = %v, want ping", request["method"])
			}
			id := requestID(t, request)
			writeLine(t, conn, fmt.Sprintf(`{"id":%q,"result":{"type":"pong","version":"0.9.0","protocol":22,"capabilities":{"future":true}}}`, id))
		})
		probe := &herdr.InstallationProbe{SocketPath: endpoint.socketPath}

		server, err := probe.Ping(testContext(t))
		if err != nil {
			t.Fatalf("Ping: %v", err)
		}
		if server.Version != "0.9.0" || server.Protocol != 22 {
			t.Errorf("server = %+v, want 0.9.0 protocol 22", server)
		}
	})
	t.Run("dead socket fails", func(t *testing.T) {
		probe := &herdr.InstallationProbe{SocketPath: filepath.Join(t.TempDir(), "gone.sock")}

		_, err := probe.Ping(testContext(t))

		if err == nil {
			t.Fatal("Ping succeeded against a socket that does not exist")
		}
	})
}
