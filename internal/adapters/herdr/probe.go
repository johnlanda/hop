package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/johnlanda/hop/internal/app"
)

// InstallationProbe implements app.Probe against the local machine: it runs
// executables to read versions and the bundled API schema, and pings the
// configured server socket. Subprocesses run with the inherited environment
// minus every HERDR_* variable, so a session this process was started from
// cannot influence what is probed.
type InstallationProbe struct {
	// BinaryPath, when set, is the herdr executable to probe. Empty means
	// look up "herdr" on PATH.
	BinaryPath string
	// SocketPath is the server socket Ping checks. Ping fails when empty.
	SocketPath string
}

var _ app.Probe = (*InstallationProbe)(nil)

// Binary locates the herdr executable and reads its version line.
func (p *InstallationProbe) Binary(ctx context.Context) (app.BinaryInfo, error) {
	return executableInfo(ctx, p.BinaryPath, "herdr")
}

// Harness locates a native harness executable on PATH and reads its version
// line. The executable name is the harness's canonical command.
func (*InstallationProbe) Harness(ctx context.Context, executable string) (app.BinaryInfo, error) {
	return executableInfo(ctx, "", executable)
}

// executableInfo resolves an executable, preferring the explicit path, and
// reports its path and --version line. A binary that is present but does not
// answer --version is still reported found, with an empty version.
func executableInfo(ctx context.Context, explicit, name string) (app.BinaryInfo, error) {
	path := explicit
	if path == "" {
		resolved, err := exec.LookPath(name)
		if err != nil {
			return app.BinaryInfo{}, fmt.Errorf("%s is not on PATH: %w", name, err)
		}
		path = resolved
	} else if _, err := os.Stat(path); err != nil {
		return app.BinaryInfo{}, fmt.Errorf("%s: %w", name, err)
	}
	out, err := runScrubbed(ctx, path, "--version")
	if err != nil {
		return app.BinaryInfo{Path: path}, nil
	}
	version, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return app.BinaryInfo{Path: path, Version: strings.TrimSpace(version)}, nil
}

// Schema runs `herdr api schema --json` and extracts the protocol number and
// the advertised request method names.
func (p *InstallationProbe) Schema(ctx context.Context) (app.SchemaInfo, error) {
	binary, err := p.Binary(ctx)
	if err != nil {
		return app.SchemaInfo{}, err
	}
	out, err := runScrubbed(ctx, binary.Path, "api", "schema", "--json")
	if err != nil {
		return app.SchemaInfo{}, fmt.Errorf("herdr api schema --json: %w", err)
	}
	return parseSchema(out)
}

// Ping checks the configured server socket and reads the server's identity.
func (p *InstallationProbe) Ping(ctx context.Context) (app.ServerInfo, error) {
	if p.SocketPath == "" {
		return app.ServerInfo{}, errors.New("no server socket configured")
	}
	var pong struct {
		Version  string `json:"version"`
		Protocol int    `json:"protocol"`
	}
	if err := NewClient(p.SocketPath).Call(ctx, "ping", nil, &pong); err != nil {
		return app.ServerInfo{}, err
	}
	return app.ServerInfo{Version: pong.Version, Protocol: pong.Protocol}, nil
}

// runScrubbed runs one command with the HERDR_* environment removed and
// returns its stdout. Failure output is folded into the error.
func runScrubbed(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...) //nolint:gosec // G204: path is operator-supplied configuration (an explicit flag, HERDR_BIN_PATH or a PATH lookup); probing it is this adapter's purpose.
	cmd.Env = scrubbedEnviron(os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// scrubbedEnviron removes every HERDR_* variable so probed commands cannot
// address the session this process happens to run inside.
func scrubbedEnviron(environ []string) []string {
	var scrubbed []string
	for _, entry := range environ {
		if !strings.HasPrefix(entry, "HERDR_") {
			scrubbed = append(scrubbed, entry)
		}
	}
	return scrubbed
}

// parseSchema extracts the protocol number and every request method constant
// from a schema document. The walk is structural rather than tied to the
// exact nesting, so schema layout changes inside the request section do not
// hide methods.
func parseSchema(doc []byte) (app.SchemaInfo, error) {
	var root struct {
		Protocol int                        `json:"protocol"`
		Schemas  map[string]json.RawMessage `json:"schemas"`
	}
	if err := json.Unmarshal(doc, &root); err != nil {
		return app.SchemaInfo{}, fmt.Errorf("decode schema document: %w", err)
	}
	requestSchema, ok := root.Schemas["request"]
	if !ok {
		return app.SchemaInfo{}, errors.New("schema document has no request schema")
	}
	var request any
	if err := json.Unmarshal(requestSchema, &request); err != nil {
		return app.SchemaInfo{}, fmt.Errorf("decode request schema: %w", err)
	}
	seen := map[string]bool{}
	collectMethodConsts(request, seen)
	methods := make([]string, 0, len(seen))
	for m := range seen {
		methods = append(methods, m)
	}
	slices.Sort(methods)
	if len(methods) == 0 {
		return app.SchemaInfo{}, errors.New("request schema advertises no methods")
	}
	return app.SchemaInfo{Protocol: root.Protocol, Methods: methods}, nil
}

// collectMethodConsts records every "properties.method.const" string found
// anywhere under node.
func collectMethodConsts(node any, seen map[string]bool) {
	switch v := node.(type) {
	case map[string]any:
		if properties, ok := v["properties"].(map[string]any); ok {
			if method, ok := properties["method"].(map[string]any); ok {
				if name, ok := method["const"].(string); ok {
					seen[name] = true
				}
			}
		}
		for _, child := range v {
			collectMethodConsts(child, seen)
		}
	case []any:
		for _, child := range v {
			collectMethodConsts(child, seen)
		}
	}
}
