package app

import (
	"fmt"
	"slices"
	"strings"
)

// Harness identifiers of the launch-environment policy. The values match the
// configuration's `[worker] harness` field and select which profile-variable
// shape SanitizeEnvironment assigns for an alternate profile directory; the
// default strip matrix applies as a full union regardless of this choice.
const (
	HarnessClaude   = "claude"
	HarnessCodex    = "codex"
	HarnessOpencode = "opencode"
)

// EnvPolicyVersion1 names the first revision of the default strip matrix,
// assembled against Claude Code 2.1.x, Codex 0.154.x and opencode 1.18.x
// plus their official documentation. The revision a policy names is frozen
// into the run snapshot with it; a future harness version that changes the
// credential surface requires a re-verified new revision, never an edit to
// this one.
const EnvPolicyVersion1 = "hop-env-v1"

// StripMatrixV1 returns the version-1 default strip matrix: per harness
// family, the credential, provider-routing and profile-override variables
// stripped by default, exactly as the approved design records them.
// SanitizeEnvironment removes the union of every family regardless of the
// launched harness, so a stray cross-provider key never reaches any worker.
func StripMatrixV1() map[string][]string {
	return map[string][]string{
		HarnessClaude: {
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
		HarnessCodex: {
			"CODEX_HOME",
			"OPENAI_API_KEY",
			"OPENAI_BASE_URL",
		},
		HarnessOpencode: {
			"OPENCODE_CONFIG",
			"OPENCODE_CONFIG_CONTENT",
			"OPENCODE_CONFIG_DIR",
		},
	}
}

// EnvPolicy is one run's frozen, versioned launch-environment policy: the
// strip-matrix revision it was assembled under, the launched harness, the
// repository's custom strip additions and passthrough opt-ins, and the
// optional alternate profile directory. The value is frozen into the run
// snapshot at `hop run` and applied unchanged at every exec boundary of the
// run; Validate turns it into the ValidatedEnvPolicy SanitizeEnvironment
// consumes.
type EnvPolicy struct {
	// Version names the default strip-matrix revision the policy was frozen
	// with. EnvPolicyVersion1 is the only revision this build knows.
	Version string
	// Harness is the launched harness: HarnessClaude, HarnessCodex or
	// HarnessOpencode. It selects the profile-variable shape only; the
	// strip matrix applies in full regardless.
	Harness string
	// Strip lists repository-configured variable names removed in addition
	// to the default matrix (the configuration's `env.strip`).
	Strip []string
	// Passthrough lists opt-in variable names kept with their inherited
	// values even when the matrix or Strip would remove them (the
	// configuration's `env.passthrough` plus per-run `-env-passthrough`).
	Passthrough []string
	// ProfileDir is the user-prepared alternate profile directory, already
	// resolved to an absolute path by the configuration loader, or "" to
	// launch in the harness's default profile.
	ProfileDir string
}

// Validate checks the policy against the exact contract SanitizeEnvironment
// applies and returns the validated value the exec-boundary commands pass to
// it, validating once at load. It rejects a version this build has no strip
// matrix for, a harness outside the supported set, a strip or passthrough
// entry that cannot name an environment variable (empty, or containing "="),
// and a profile directory that is not an absolute path — profile paths
// arrive already resolved by the configuration loader, and a relative one
// must never select a working-directory-dependent profile.
func (p EnvPolicy) Validate() (ValidatedEnvPolicy, error) { //nolint:gocritic // hugeParam: validation reads a frozen policy value once per command start; by-value keeps it immutable and aliasing-free.
	if p.Version != EnvPolicyVersion1 {
		return ValidatedEnvPolicy{}, fmt.Errorf("environment policy version %q is not a strip-matrix revision this build knows; the only revision is %q", p.Version, EnvPolicyVersion1)
	}
	switch p.Harness {
	case HarnessClaude, HarnessCodex, HarnessOpencode:
	default:
		return ValidatedEnvPolicy{}, fmt.Errorf("harness %q has no environment policy; supported harnesses are %q, %q and %q", p.Harness, HarnessClaude, HarnessCodex, HarnessOpencode)
	}
	for _, name := range p.Strip {
		if err := validateVariableName("strip", name); err != nil {
			return ValidatedEnvPolicy{}, err
		}
	}
	for _, name := range p.Passthrough {
		if err := validateVariableName("passthrough", name); err != nil {
			return ValidatedEnvPolicy{}, err
		}
	}
	if p.ProfileDir != "" && !strings.HasPrefix(p.ProfileDir, "/") {
		return ValidatedEnvPolicy{}, fmt.Errorf("profile directory %q is not an absolute path; the configuration loader resolves profile paths before the policy is frozen", p.ProfileDir)
	}
	return ValidatedEnvPolicy{
		version:     p.Version,
		harness:     p.Harness,
		strip:       slices.Clone(p.Strip),
		passthrough: slices.Clone(p.Passthrough),
		profileDir:  p.ProfileDir,
	}, nil
}

// validateVariableName rejects a strip or passthrough entry that can never
// name an environment variable.
func validateVariableName(list, name string) error {
	if name == "" {
		return fmt.Errorf("%s entry is empty; entries name environment variables", list)
	}
	if strings.Contains(name, "=") {
		return fmt.Errorf("%s entry %q contains %q and cannot name an environment variable", list, name, "=")
	}
	return nil
}

// ValidatedEnvPolicy is an EnvPolicy that passed Validate. Its fields are
// unexported, so the only way to construct a non-zero value is Validate.
// The zero value sanitizes as the strictest policy: the full matrix union
// stripped, no passthrough, no profile assignment.
type ValidatedEnvPolicy struct {
	version     string
	harness     string
	strip       []string
	passthrough []string
	profileDir  string
}

// SanitizeEnvironment computes the environment the sanitizing launcher hands
// to the exec boundary. It is pure: environ is the complete inherited
// environment as "NAME=value" entries (the os.Environ shape) and is never
// modified; the exec-boundary commands validate the run snapshot's frozen
// policy once at load (EnvPolicy.Validate) and pass the validated value.
//
// Precedence, applied in order: every variable of the full strip-matrix
// union — regardless of the launched harness — plus the policy's custom
// strip entries is removed; a passthrough entry is exempt from both and
// keeps its inherited value (passthrough beats strip); when the policy names
// a profile directory, the launched harness's documented profile variables
// are assigned last, replacing any surviving inherited value (profile beats
// passthrough). A variable named HERDR_* or HOP_* is never stripped, custom
// strip entries included.
//
// Entries resolve by name: each splits at its first "=", an entry without
// one is a name with no value and is rendered back verbatim, and when a name
// appears more than once the last entry wins (the operating system's own
// resolution) and keeps its position. env holds the surviving entries in
// inherited order with the profile assignments appended; removed holds the
// names — never the values — of the stripped variables, in that same order.
func SanitizeEnvironment(environ []string, policy ValidatedEnvPolicy) (env, removed []string) { //nolint:gocritic // hugeParam: the design fixes a by-value policy parameter; sanitization runs once per exec boundary and the value must stay immutable.
	stripped := stripUnionV1()
	for _, name := range policy.strip {
		stripped[name] = true
	}
	passed := make(map[string]bool, len(policy.passthrough))
	for _, name := range policy.passthrough {
		passed[name] = true
	}

	// Walk environ backwards so the last entry of a duplicated name is the
	// one entry decided on; every earlier duplicate is dropped.
	decided := make(map[string]bool, len(environ))
	for i := len(environ) - 1; i >= 0; i-- {
		entry := environ[i]
		name, _, _ := strings.Cut(entry, "=")
		if decided[name] {
			continue
		}
		decided[name] = true
		switch {
		case strings.HasPrefix(name, "HERDR_") || strings.HasPrefix(name, "HOP_"):
			env = append(env, entry)
		case stripped[name] && !passed[name]:
			removed = append(removed, name)
		default:
			env = append(env, entry)
		}
	}
	slices.Reverse(env)
	slices.Reverse(removed)

	if policy.profileDir != "" {
		for _, assignment := range profileAssignments(policy.harness, policy.profileDir) {
			env = withAssignment(env, assignment)
		}
	}
	return env, removed
}

// stripUnionV1 returns the full version-1 default strip set: the union of
// every harness family's matrix row.
func stripUnionV1() map[string]bool {
	union := map[string]bool{}
	for _, family := range StripMatrixV1() {
		for _, name := range family {
			union[name] = true
		}
	}
	return union
}

// profileAssignments returns the launched harness's documented profile shape
// for an alternate profile directory, in a fixed order: CLAUDE_CONFIG_DIR
// for Claude Code, CODEX_HOME for Codex, and for opencode the full HOME plus
// XDG_*_HOME layout — never a flat directory, because an inherited XDG
// variable would otherwise split the profile and could point credentials at
// a shared directory. The directory passes through unmodified; HOP never
// prepares, inspects or writes it.
func profileAssignments(harness, dir string) []string {
	switch harness {
	case HarnessClaude:
		return []string{"CLAUDE_CONFIG_DIR=" + dir}
	case HarnessCodex:
		return []string{"CODEX_HOME=" + dir}
	case HarnessOpencode:
		return []string{
			"HOME=" + dir,
			"XDG_DATA_HOME=" + dir + "/.local/share",
			"XDG_CONFIG_HOME=" + dir + "/.config",
			"XDG_STATE_HOME=" + dir + "/.local/state",
			"XDG_CACHE_HOME=" + dir + "/.cache",
		}
	}
	return nil
}

// withAssignment appends one "NAME=value" assignment, dropping any surviving
// inherited entry of the same name so the result carries exactly one.
func withAssignment(env []string, assignment string) []string {
	name, _, _ := strings.Cut(assignment, "=")
	env = slices.DeleteFunc(env, func(entry string) bool {
		entryName, _, _ := strings.Cut(entry, "=")
		return entryName == name
	})
	return append(env, assignment)
}
