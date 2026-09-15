package config_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/config"
	"github.com/johnlanda/hop/internal/app"
)

// writePolicy materializes one repository root holding the policy file.
func writePolicy(t *testing.T, content string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".herdr-orchestrator")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLoadParsesPolicies(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    func(root string) app.RunPolicy
	}{
		{
			name: "minimal policy applies every default",
			content: "[check]\n" +
				"command = [\"sh\", \"check.sh\"]\n",
			want: func(string) app.RunPolicy {
				return app.RunPolicy{
					CheckArgv:    []string{"sh", "check.sh"},
					CheckTimeout: 10 * time.Minute,
					Harness:      app.HarnessClaude,
				}
			},
		},
		{
			name: "complete policy with comments, escapes and multiline arrays",
			content: "# repository policy\n" +
				"[check]\n" +
				"command = [\n" +
				"  \"sh\", # the interpreter\n" +
				"  \"-c\",\n" +
				"  \"make check \\\"quoted\\\"\\tafter-tab\",\n" +
				"]\n" +
				"timeout = \"90s\"\n" +
				"repeatable = true # safe to re-run\n" +
				"\n" +
				"[env]\n" +
				"strip = [\"MY_ORG_PROXY_TOKEN\"]\n" +
				"passthrough = [\n" +
				"  \"OPENAI_API_KEY\",\n" +
				"  \"OPENAI_BASE_URL\",\n" +
				"]\n" +
				"\n" +
				"[worker]\n" +
				"harness = \"codex\"\n" +
				"\n" +
				"[profile]\n" +
				"dir = \".profiles/codex-alt\"\n",
			want: func(root string) app.RunPolicy {
				return app.RunPolicy{
					CheckArgv:       []string{"sh", "-c", "make check \"quoted\"\tafter-tab"},
					CheckTimeout:    90 * time.Second,
					CheckRepeatable: true,
					EnvStrip:        []string{"MY_ORG_PROXY_TOKEN"},
					EnvPassthrough:  []string{"OPENAI_API_KEY", "OPENAI_BASE_URL"},
					ProfileDir:      filepath.Join(root, ".profiles", "codex-alt"),
					Harness:         app.HarnessCodex,
				}
			},
		},
		{
			name: "absolute profile directory passes through cleaned",
			content: "[check]\n" +
				"command = [\"sh\", \"check.sh\"]\n" +
				"[profile]\n" +
				"dir = \"/opt/profiles//claude-alt/\"\n",
			want: func(string) app.RunPolicy {
				return app.RunPolicy{
					CheckArgv:    []string{"sh", "check.sh"},
					CheckTimeout: 10 * time.Minute,
					ProfileDir:   "/opt/profiles/claude-alt",
					Harness:      app.HarnessClaude,
				}
			},
		},
		{
			name: "opencode harness accepted",
			content: "[check]\n" +
				"command = [\"sh\", \"check.sh\"]\n" +
				"[worker]\n" +
				"harness = \"opencode\"\n",
			want: func(string) app.RunPolicy {
				return app.RunPolicy{
					CheckArgv:    []string{"sh", "check.sh"},
					CheckTimeout: 10 * time.Minute,
					Harness:      app.HarnessOpencode,
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := writePolicy(t, tc.content)

			policy, err := config.Source{}.Load(t.Context(), root)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			want := tc.want(root)
			assertPolicyEqual(t, &policy, &want)
			if want.ProfileDir != "" && !filepath.IsAbs(policy.ProfileDir) {
				t.Errorf("ProfileDir %q is not absolute", policy.ProfileDir)
			}
		})
	}
}

func TestLoadRejectsInvalidPolicies(t *testing.T) {
	// secretMarker stands in for a secret mistakenly pasted into the policy
	// file; no rejection may ever echo it back through a rendered error.
	const secretMarker = "DUMMY-SECRET-MARKER"
	cases := []struct {
		name        string
		content     string
		wantMessage string
	}{
		{
			name:        "missing check table",
			content:     "[worker]\nharness = \"claude\"\n",
			wantMessage: "[check] command is required",
		},
		{
			name:        "missing check command",
			content:     "[check]\ntimeout = \"1m\"\n",
			wantMessage: "[check] command is required",
		},
		{
			name:        "empty check command",
			content:     "[check]\ncommand = []\n",
			wantMessage: "[check] command is required",
		},
		{
			name:        "unknown key is named",
			content:     "[check]\ncommand = [\"sh\", \"check.sh\"]\nrepetable = true\n",
			wantMessage: "unknown key check.repetable",
		},
		{
			name:        "duplicate key",
			content:     "[check]\ncommand = [\"a\"]\ncommand = [\"b\"]\n",
			wantMessage: "key command is defined more than once",
		},
		{
			name:        "duplicate table",
			content:     "[check]\ncommand = [\"a\"]\n[check]\ntimeout = \"1m\"\n",
			wantMessage: "key check is defined more than once",
		},
		{
			name:        "unparsable timeout is rejected without echoing it",
			content:     "[check]\ncommand = [\"a\"]\ntimeout = \"" + secretMarker + "\"\n",
			wantMessage: "check.timeout is not a Go duration",
		},
		{
			name:        "explicitly empty timeout is rejected, never defaulted",
			content:     "[check]\ncommand = [\"a\"]\ntimeout = \"\"\n",
			wantMessage: "check.timeout is not a Go duration",
		},
		{
			name:        "zero timeout",
			content:     "[check]\ncommand = [\"a\"]\ntimeout = \"0s\"\n",
			wantMessage: "check.timeout must be a positive duration",
		},
		{
			name:        "negative timeout",
			content:     "[check]\ncommand = [\"a\"]\ntimeout = \"-5m\"\n",
			wantMessage: "check.timeout must be a positive duration",
		},
		{
			name:        "timeout with the wrong type names the key and expected shape",
			content:     "[check]\ncommand = [\"a\"]\ntimeout = 600\n",
			wantMessage: "key check.timeout must be a string holding a Go duration",
		},
		{
			name:        "unsupported harness is rejected without echoing it",
			content:     "[check]\ncommand = [\"a\"]\n[worker]\nharness = \"" + secretMarker + "\"\n",
			wantMessage: "worker.harness is not a supported harness",
		},
		{
			name:        "explicitly empty harness is rejected, never defaulted",
			content:     "[check]\ncommand = [\"a\"]\n[worker]\nharness = \"\"\n",
			wantMessage: "worker.harness is not a supported harness",
		},
		{
			name:        "syntax error is positioned without echoing the document",
			content:     "[check]\ncommand = [\"a\"]\n[env]\nstrip = [" + secretMarker + "]\n",
			wantMessage: "not valid TOML at this position",
		},
		{
			name:        "profile dir with a control byte",
			content:     "[check]\ncommand = [\"a\"]\n[profile]\ndir = \"alt\\u0007profile\"\n",
			wantMessage: "control byte",
		},
		{
			name:        "profile dir with an escaped newline",
			content:     "[check]\ncommand = [\"a\"]\n[profile]\ndir = \"alt\\nprofile\"\n",
			wantMessage: "control byte",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := writePolicy(t, tc.content)

			_, err := config.Source{}.Load(t.Context(), root)

			if err == nil {
				t.Fatal("Load succeeded, want a rejection")
			}
			if !strings.Contains(err.Error(), tc.wantMessage) {
				t.Errorf("error %q does not contain %q", err, tc.wantMessage)
			}
			if strings.Contains(err.Error(), secretMarker) {
				t.Errorf("error %q echoes the supplied value; diagnostics must never render it", err)
			}
		})
	}
}

func TestLoadFailsWithoutPolicyFile(t *testing.T) {
	_, err := config.Source{}.Load(t.Context(), t.TempDir())

	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load error = %v, want it to wrap fs.ErrNotExist", err)
	}
}

func TestLoadHonorsContextCancellation(t *testing.T) {
	root := writePolicy(t, "[check]\ncommand = [\"sh\", \"check.sh\"]\n")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := config.Source{}.Load(ctx, root)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Load error = %v, want context.Canceled", err)
	}
}

// assertPolicyEqual compares every RunPolicy field with slice equality.
func assertPolicyEqual(t *testing.T, got, want *app.RunPolicy) {
	t.Helper()
	if !equalStrings(got.CheckArgv, want.CheckArgv) {
		t.Errorf("CheckArgv = %q, want %q", got.CheckArgv, want.CheckArgv)
	}
	if got.CheckTimeout != want.CheckTimeout {
		t.Errorf("CheckTimeout = %v, want %v", got.CheckTimeout, want.CheckTimeout)
	}
	if got.CheckRepeatable != want.CheckRepeatable {
		t.Errorf("CheckRepeatable = %t, want %t", got.CheckRepeatable, want.CheckRepeatable)
	}
	if !equalStrings(got.EnvStrip, want.EnvStrip) {
		t.Errorf("EnvStrip = %q, want %q", got.EnvStrip, want.EnvStrip)
	}
	if !equalStrings(got.EnvPassthrough, want.EnvPassthrough) {
		t.Errorf("EnvPassthrough = %q, want %q", got.EnvPassthrough, want.EnvPassthrough)
	}
	if got.ProfileDir != want.ProfileDir {
		t.Errorf("ProfileDir = %q, want %q", got.ProfileDir, want.ProfileDir)
	}
	if got.Harness != want.Harness {
		t.Errorf("Harness = %q, want %q", got.Harness, want.Harness)
	}
}

// equalStrings treats nil and empty as equal, matching the policy's
// list semantics.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
