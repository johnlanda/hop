package app_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

// matrixVariablesV1 is the version-1 strip matrix exactly as the approved
// design tables it, keyed by harness family. The production matrix must
// equal this table member for member.
func matrixVariablesV1() map[string][]string {
	return map[string][]string{
		app.HarnessClaude: {
			"ANTHROPIC_API_KEY",
			"ANTHROPIC_AUTH_TOKEN",
			"ANTHROPIC_BASE_URL",
			"AWS_BEARER_TOKEN_BEDROCK",
			"CLAUDE_CODE_OAUTH_TOKEN",
			"CLAUDE_CODE_USE_BEDROCK",
			"CLAUDE_CODE_USE_VERTEX",
			"CLAUDE_CONFIG_DIR",
			"CLAUDE_SECURESTORAGE_CONFIG_DIR",
		},
		app.HarnessCodex: {
			"CODEX_HOME",
			"OPENAI_API_KEY",
			"OPENAI_BASE_URL",
		},
		app.HarnessOpencode: {
			"OPENCODE_CONFIG",
			"OPENCODE_CONFIG_CONTENT",
			"OPENCODE_CONFIG_DIR",
		},
	}
}

// matrixUnionV1 returns every variable of the version-1 matrix, sorted.
func matrixUnionV1() []string {
	var union []string
	for _, family := range matrixVariablesV1() {
		union = append(union, family...)
	}
	slices.Sort(union)
	return union
}

// validPolicy returns a minimal valid policy for one harness.
func validPolicy(harness string) app.EnvPolicy {
	return app.EnvPolicy{Version: app.EnvPolicyVersion1, Harness: harness}
}

// mustValidate validates a policy that the test requires to be valid.
func mustValidate(t *testing.T, policy app.EnvPolicy) app.ValidatedEnvPolicy { //nolint:gocritic // hugeParam: the helper mirrors Validate's by-value contract on the policy under test.
	t.Helper()
	validated, err := policy.Validate()
	if err != nil {
		t.Fatalf("Validate(%+v) = %v, want nil", policy, err)
	}
	return validated
}

func TestStripMatrixV1MatchesTheDesignTables(t *testing.T) {
	got := app.StripMatrixV1()
	want := matrixVariablesV1()

	for family, wantVars := range want {
		gotVars := slices.Sorted(slices.Values(got[family]))
		if !slices.Equal(gotVars, slices.Sorted(slices.Values(wantVars))) {
			t.Errorf("StripMatrixV1()[%q] = %q, want %q", family, gotVars, wantVars)
		}
	}
	for family := range got {
		if _, ok := want[family]; !ok {
			t.Errorf("StripMatrixV1() has an undesigned family %q", family)
		}
	}
}

func TestEnvPolicyValidate(t *testing.T) {
	cases := []struct {
		name    string
		policy  app.EnvPolicy
		wantErr string
	}{
		{
			name:   "default-profile claude policy",
			policy: app.EnvPolicy{Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude},
		},
		{
			name: "codex policy with an absolute profile directory",
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessCodex,
				ProfileDir: "/profiles/codex-alt",
			},
		},
		{
			name: "opencode policy with strip and passthrough entries",
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessOpencode,
				Strip:       []string{"MY_ORG_PROXY_TOKEN"},
				Passthrough: []string{"OPENAI_API_KEY"},
				ProfileDir:  "/profiles/opencode-alt",
			},
		},
		{
			name:    "empty version",
			policy:  app.EnvPolicy{Harness: app.HarnessClaude},
			wantErr: "not a strip-matrix revision this build knows",
		},
		{
			name:    "unknown version",
			policy:  app.EnvPolicy{Version: "hop-env-v2", Harness: app.HarnessClaude},
			wantErr: `"hop-env-v2" is not a strip-matrix revision`,
		},
		{
			name:    "empty harness",
			policy:  app.EnvPolicy{Version: app.EnvPolicyVersion1},
			wantErr: "has no environment policy",
		},
		{
			name:    "unknown harness",
			policy:  app.EnvPolicy{Version: app.EnvPolicyVersion1, Harness: "gemini"},
			wantErr: `harness "gemini" has no environment policy`,
		},
		{
			name: "relative profile directory",
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
				ProfileDir: "profiles/claude-alt",
			},
			wantErr: "not an absolute path",
		},
		{
			name: "dot profile directory",
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
				ProfileDir: ".",
			},
			wantErr: "not an absolute path",
		},
		{
			name: "empty strip entry",
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
				Strip: []string{""},
			},
			wantErr: "strip entry 0 is empty",
		},
		{
			name: "strip entry that is an assignment",
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
				Strip: []string{"GOOD", "FOO=bar"},
			},
			wantErr: `strip entry 1 (name "FOO") is an assignment, not a variable name`,
		},
		{
			name: "strip entry containing a NUL byte",
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
				Strip: []string{"KEY\x00"},
			},
			wantErr: "strip entry 0 contains a control byte",
		},
		{
			name: "strip entry containing a tab",
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
				Strip: []string{"KE\tY"},
			},
			wantErr: "strip entry 0 contains a control byte",
		},
		{
			name: "empty passthrough entry",
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
				Passthrough: []string{""},
			},
			wantErr: "passthrough entry 0 is empty",
		},
		{
			name: "passthrough entry that is an assignment",
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
				Passthrough: []string{"FOO=bar"},
			},
			wantErr: `passthrough entry 0 (name "FOO") is an assignment, not a variable name`,
		},
		{
			name: "passthrough entry containing a NUL byte",
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
				Passthrough: []string{"KEY\x00"},
			},
			wantErr: "passthrough entry 0 contains a control byte",
		},
		{
			name: "profile directory containing a NUL byte",
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
				ProfileDir: "/profiles/alt\x00suffix",
			},
			wantErr: "profile directory contains a control byte",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.policy.Validate()

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestSanitizeEnvironmentStripsTheFullUnionForEveryHarness(t *testing.T) {
	var environ []string
	for _, name := range matrixUnionV1() {
		environ = append(environ, name+"=secret-value")
	}
	environ = append(environ, "PATH=/usr/bin", "TERM=xterm")

	for _, harness := range []string{app.HarnessClaude, app.HarnessCodex, app.HarnessOpencode} {
		t.Run(harness, func(t *testing.T) {
			env, removed := app.SanitizeEnvironment(environ, mustValidate(t, validPolicy(harness)))

			if want := []string{"PATH=/usr/bin", "TERM=xterm"}; !slices.Equal(env, want) {
				t.Errorf("env = %q, want %q", env, want)
			}
			if want := matrixUnionV1(); !slices.Equal(removed, want) {
				t.Errorf("removed = %q, want the full union %q", removed, want)
			}
		})
	}
}

func TestSanitizeEnvironmentPrecedence(t *testing.T) {
	cases := []struct {
		name        string
		environ     []string
		policy      app.EnvPolicy
		wantEnv     []string
		wantRemoved []string
	}{
		{
			name:        "matrix variable is stripped",
			environ:     []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=secret"},
			policy:      validPolicy(app.HarnessClaude),
			wantEnv:     []string{"PATH=/usr/bin"},
			wantRemoved: []string{"ANTHROPIC_API_KEY"},
		},
		{
			name:    "custom strip entry is stripped",
			environ: []string{"MY_ORG_PROXY_TOKEN=secret", "PATH=/usr/bin"},
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
				Strip: []string{"MY_ORG_PROXY_TOKEN"},
			},
			wantEnv:     []string{"PATH=/usr/bin"},
			wantRemoved: []string{"MY_ORG_PROXY_TOKEN"},
		},
		{
			name:    "passthrough restores a matrix-stripped variable with its inherited value",
			environ: []string{"OPENAI_API_KEY=inherited", "OPENAI_BASE_URL=https://proxy"},
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
				Passthrough: []string{"OPENAI_API_KEY"},
			},
			wantEnv:     []string{"OPENAI_API_KEY=inherited"},
			wantRemoved: []string{"OPENAI_BASE_URL"},
		},
		{
			name:    "passthrough beats a custom strip entry",
			environ: []string{"MY_ORG_PROXY_TOKEN=inherited"},
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
				Strip:       []string{"MY_ORG_PROXY_TOKEN"},
				Passthrough: []string{"MY_ORG_PROXY_TOKEN"},
			},
			wantEnv:     []string{"MY_ORG_PROXY_TOKEN=inherited"},
			wantRemoved: nil,
		},
		{
			name:    "passthrough of an absent variable invents nothing",
			environ: []string{"PATH=/usr/bin"},
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
				Passthrough: []string{"OPENAI_API_KEY"},
			},
			wantEnv:     []string{"PATH=/usr/bin"},
			wantRemoved: nil,
		},
		{
			name:    "profile beats passthrough for the same variable",
			environ: []string{"CLAUDE_CONFIG_DIR=/inherited"},
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
				Passthrough: []string{"CLAUDE_CONFIG_DIR"},
				ProfileDir:  "/profiles/claude-alt",
			},
			wantEnv:     []string{"CLAUDE_CONFIG_DIR=/profiles/claude-alt"},
			wantRemoved: nil,
		},
		{
			name:        "unlisted variables pass through unchanged",
			environ:     []string{"PATH=/usr/bin", "LANG=en_US.UTF-8", "EDITOR=vi"},
			policy:      validPolicy(app.HarnessCodex),
			wantEnv:     []string{"PATH=/usr/bin", "LANG=en_US.UTF-8", "EDITOR=vi"},
			wantRemoved: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, removed := app.SanitizeEnvironment(tc.environ, mustValidate(t, tc.policy))

			if !slices.Equal(env, tc.wantEnv) {
				t.Errorf("env = %q, want %q", env, tc.wantEnv)
			}
			if !slices.Equal(removed, tc.wantRemoved) {
				t.Errorf("removed = %q, want %q", removed, tc.wantRemoved)
			}
		})
	}
}

func TestSanitizeEnvironmentProfileMapping(t *testing.T) {
	cases := []struct {
		name    string
		environ []string
		policy  app.EnvPolicy
		wantEnv []string
	}{
		{
			name:    "claude sets only CLAUDE_CONFIG_DIR",
			environ: []string{"HOME=/Users/dev", "PATH=/usr/bin"},
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
				ProfileDir: "/profiles/claude-alt",
			},
			wantEnv: []string{
				"HOME=/Users/dev",
				"PATH=/usr/bin",
				"CLAUDE_CONFIG_DIR=/profiles/claude-alt",
			},
		},
		{
			name:    "codex sets only CODEX_HOME",
			environ: []string{"HOME=/Users/dev"},
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessCodex,
				ProfileDir: "/profiles/codex-alt",
			},
			wantEnv: []string{
				"HOME=/Users/dev",
				"CODEX_HOME=/profiles/codex-alt",
			},
		},
		{
			name: "opencode sets the documented HOME plus XDG layout",
			environ: []string{
				"HOME=/Users/dev",
				"XDG_DATA_HOME=/Users/dev/.shared-data",
				"PATH=/usr/bin",
			},
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessOpencode,
				ProfileDir: "/profiles/oc-alt",
			},
			wantEnv: []string{
				"PATH=/usr/bin",
				"HOME=/profiles/oc-alt",
				"XDG_DATA_HOME=/profiles/oc-alt/.local/share",
				"XDG_CONFIG_HOME=/profiles/oc-alt/.config",
				"XDG_STATE_HOME=/profiles/oc-alt/.local/state",
				"XDG_CACHE_HOME=/profiles/oc-alt/.cache",
			},
		},
		{
			name:    "no profile directory assigns nothing",
			environ: []string{"HOME=/Users/dev", "CLAUDE_CONFIG_DIR=/inherited"},
			policy:  validPolicy(app.HarnessClaude),
			wantEnv: []string{"HOME=/Users/dev"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, _ := app.SanitizeEnvironment(tc.environ, mustValidate(t, tc.policy))

			if !slices.Equal(env, tc.wantEnv) {
				t.Errorf("env = %q, want %q", env, tc.wantEnv)
			}
		})
	}
}

func TestSanitizeEnvironmentRetainsHerdrAndHopVariables(t *testing.T) {
	environ := []string{
		"HERDR_SOCKET_PATH=/tmp/h.sock",
		"HERDR_PANE_ID=w1:p1",
		"HOP_RUN_ID=6a1c9f34-0000-4000-8000-000000000001",
		"HOP_STATE_DIR=/state/hop",
		"ANTHROPIC_API_KEY=secret",
	}
	policy := app.EnvPolicy{
		Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
		Strip: []string{"HERDR_SOCKET_PATH", "HOP_STATE_DIR"},
	}

	env, removed := app.SanitizeEnvironment(environ, mustValidate(t, policy))

	wantEnv := []string{
		"HERDR_SOCKET_PATH=/tmp/h.sock",
		"HERDR_PANE_ID=w1:p1",
		"HOP_RUN_ID=6a1c9f34-0000-4000-8000-000000000001",
		"HOP_STATE_DIR=/state/hop",
	}
	if !slices.Equal(env, wantEnv) {
		t.Errorf("env = %q, want %q: HERDR_* and HOP_* are always kept, custom strip entries included", env, wantEnv)
	}
	if want := []string{"ANTHROPIC_API_KEY"}; !slices.Equal(removed, want) {
		t.Errorf("removed = %q, want %q", removed, want)
	}
}

func TestSanitizeEnvironmentEnvironShape(t *testing.T) {
	cases := []struct {
		name        string
		environ     []string
		policy      app.EnvPolicy
		wantEnv     []string
		wantRemoved []string
	}{
		{
			name:        "later duplicate wins and keeps its position",
			environ:     []string{"FOO=first", "PATH=/usr/bin", "FOO=second"},
			policy:      validPolicy(app.HarnessClaude),
			wantEnv:     []string{"PATH=/usr/bin", "FOO=second"},
			wantRemoved: nil,
		},
		{
			name:        "duplicated stripped variable is removed once",
			environ:     []string{"OPENAI_API_KEY=a", "OPENAI_API_KEY=b", "PATH=/usr/bin"},
			policy:      validPolicy(app.HarnessClaude),
			wantEnv:     []string{"PATH=/usr/bin"},
			wantRemoved: []string{"OPENAI_API_KEY"},
		},
		{
			name:        "entry without an equals sign is a name with no value",
			environ:     []string{"NOEQUALS", "PATH=/usr/bin"},
			policy:      validPolicy(app.HarnessClaude),
			wantEnv:     []string{"NOEQUALS", "PATH=/usr/bin"},
			wantRemoved: nil,
		},
		{
			name:        "name-only entry matching the matrix is stripped",
			environ:     []string{"ANTHROPIC_API_KEY", "PATH=/usr/bin"},
			policy:      validPolicy(app.HarnessClaude),
			wantEnv:     []string{"PATH=/usr/bin"},
			wantRemoved: []string{"ANTHROPIC_API_KEY"},
		},
		{
			name:        "empty value survives",
			environ:     []string{"EMPTY=", "PATH=/usr/bin"},
			policy:      validPolicy(app.HarnessClaude),
			wantEnv:     []string{"EMPTY=", "PATH=/usr/bin"},
			wantRemoved: nil,
		},
		{
			name:        "empty environment",
			environ:     nil,
			policy:      validPolicy(app.HarnessClaude),
			wantEnv:     nil,
			wantRemoved: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, removed := app.SanitizeEnvironment(tc.environ, mustValidate(t, tc.policy))

			if !slices.Equal(env, tc.wantEnv) {
				t.Errorf("env = %q, want %q", env, tc.wantEnv)
			}
			if !slices.Equal(removed, tc.wantRemoved) {
				t.Errorf("removed = %q, want %q", removed, tc.wantRemoved)
			}
		})
	}
}

func TestSanitizeEnvironmentIsPure(t *testing.T) {
	environ := []string{"ANTHROPIC_API_KEY=secret", "PATH=/usr/bin", "FOO=1", "FOO=2"}
	original := slices.Clone(environ)
	policy := mustValidate(t, validPolicy(app.HarnessClaude))

	firstEnv, firstRemoved := app.SanitizeEnvironment(environ, policy)
	secondEnv, secondRemoved := app.SanitizeEnvironment(environ, policy)

	if !slices.Equal(environ, original) {
		t.Errorf("SanitizeEnvironment modified its input: %q, want %q", environ, original)
	}
	if !slices.Equal(firstEnv, secondEnv) || !slices.Equal(firstRemoved, secondRemoved) {
		t.Errorf("SanitizeEnvironment is not deterministic: (%q, %q) then (%q, %q)", firstEnv, firstRemoved, secondEnv, secondRemoved)
	}
}

func TestSanitizeEnvironmentRemovedCarriesNamesNotValues(t *testing.T) {
	environ := []string{
		"ANTHROPIC_API_KEY=secret-a",
		"OPENAI_API_KEY=secret-b",
		"MY_ORG_PROXY_TOKEN=secret-c",
	}
	policy := app.EnvPolicy{
		Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
		Strip: []string{"MY_ORG_PROXY_TOKEN"},
	}

	_, removed := app.SanitizeEnvironment(environ, mustValidate(t, policy))

	want := []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "MY_ORG_PROXY_TOKEN"}
	if !slices.Equal(removed, want) {
		t.Errorf("removed = %q, want %q", removed, want)
	}
	for _, name := range removed {
		if strings.Contains(name, "secret") {
			t.Errorf("removed entry %q carries a value; removed must list names only", name)
		}
	}
}

func TestEnvPolicyValidateErrorsNeverEchoValues(t *testing.T) {
	const secret = "synthetic-secret"
	cases := []struct {
		name   string
		policy app.EnvPolicy
	}{
		{
			name: "strip assignment entry",
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
				Strip: []string{"OPENAI_API_KEY=" + secret},
			},
		},
		{
			name: "passthrough assignment entry",
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
				Passthrough: []string{"OPENAI_API_KEY=" + secret},
			},
		},
		{
			name: "assignment entry with a control byte in its value",
			policy: app.EnvPolicy{
				Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
				Strip: []string{"OPENAI_API_KEY=\x00" + secret},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.policy.Validate()

			if err == nil {
				t.Fatal("Validate() = nil, want a rejection")
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("Validate() error %q echoes the entry's value; errors carry list, position and name portion only", err)
			}
		})
	}
}

func TestValidatedEnvPolicyDoesNotAliasTheCallersSlices(t *testing.T) {
	policy := app.EnvPolicy{
		Version: app.EnvPolicyVersion1, Harness: app.HarnessClaude,
		Strip:       []string{"CUSTOM"},
		Passthrough: []string{"OPENAI_API_KEY"},
	}
	validated := mustValidate(t, policy)

	policy.Strip[0] = "SAFE"
	policy.Passthrough[0] = "ANTHROPIC_API_KEY"

	environ := []string{"CUSTOM=x", "SAFE=x", "OPENAI_API_KEY=opt-in", "ANTHROPIC_API_KEY=x"}
	env, removed := app.SanitizeEnvironment(environ, validated)

	if want := []string{"SAFE=x", "OPENAI_API_KEY=opt-in"}; !slices.Equal(env, want) {
		t.Errorf("env = %q, want %q: mutating the caller's slices after Validate must not alter the validated policy", env, want)
	}
	if want := []string{"CUSTOM", "ANTHROPIC_API_KEY"}; !slices.Equal(removed, want) {
		t.Errorf("removed = %q, want %q: mutating the caller's slices after Validate must not alter the validated policy", removed, want)
	}
}

func TestSanitizeEnvironmentOutputsOwnTheirStorage(t *testing.T) {
	environ := []string{"HOME=/Users/dev", "ANTHROPIC_API_KEY=secret", "PATH=/usr/bin"}
	original := slices.Clone(environ)
	policy := mustValidate(t, app.EnvPolicy{
		Version: app.EnvPolicyVersion1, Harness: app.HarnessOpencode,
		ProfileDir: "/profiles/oc-alt",
	})
	wantEnv := []string{
		"PATH=/usr/bin",
		"HOME=/profiles/oc-alt",
		"XDG_DATA_HOME=/profiles/oc-alt/.local/share",
		"XDG_CONFIG_HOME=/profiles/oc-alt/.config",
		"XDG_STATE_HOME=/profiles/oc-alt/.local/state",
		"XDG_CACHE_HOME=/profiles/oc-alt/.cache",
	}
	wantRemoved := []string{"ANTHROPIC_API_KEY"}

	env, removed := app.SanitizeEnvironment(environ, policy)
	if !slices.Equal(env, wantEnv) || !slices.Equal(removed, wantRemoved) {
		t.Fatalf("SanitizeEnvironment() = (%q, %q), want (%q, %q)", env, removed, wantEnv, wantRemoved)
	}

	env[0] = "changed"
	removed[0] = "changed"

	if !slices.Equal(environ, original) {
		t.Errorf("mutating the outputs changed the input: %q, want %q", environ, original)
	}
	againEnv, againRemoved := app.SanitizeEnvironment(environ, policy)
	if !slices.Equal(againEnv, wantEnv) || !slices.Equal(againRemoved, wantRemoved) {
		t.Errorf("second call = (%q, %q), want (%q, %q): outputs must not share storage across calls", againEnv, againRemoved, wantEnv, wantRemoved)
	}
}

func TestSanitizeEnvironmentZeroValidatedPolicyIsStrictest(t *testing.T) {
	var environ []string
	for _, name := range matrixUnionV1() {
		environ = append(environ, name+"=secret-value")
	}
	environ = append(environ, "PATH=/usr/bin", "HOP_RUN_ID=r1", "HERDR_PANE_ID=w1:p1")

	env, removed := app.SanitizeEnvironment(environ, app.ValidatedEnvPolicy{})

	wantEnv := []string{"PATH=/usr/bin", "HOP_RUN_ID=r1", "HERDR_PANE_ID=w1:p1"}
	if !slices.Equal(env, wantEnv) {
		t.Errorf("env = %q, want %q: the zero policy strips the full union, passes nothing through and assigns no profile", env, wantEnv)
	}
	if want := matrixUnionV1(); !slices.Equal(removed, want) {
		t.Errorf("removed = %q, want the full union %q", removed, want)
	}
}
