// Package config loads one repository's run policy from
// `.herdr-orchestrator/config.toml` with strict TOML 1.0 decoding
// (github.com/pelletier/go-toml/v2): unknown keys, duplicate keys and type
// mismatches fail with the offending key named, and defaults are applied
// only for fields the design declares defaultable.
package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/johnlanda/hop/internal/app"
)

// configRelPath is the policy file's fixed location under a repository
// root.
const configRelPath = ".herdr-orchestrator/config.toml"

// defaultCheckTimeout applies when [check] timeout is absent.
const defaultCheckTimeout = 10 * time.Minute

// Source implements app.ConfigurationSource over the repository's policy
// file.
type Source struct{}

var _ app.ConfigurationSource = Source{}

// fileConfig is the policy file's wire shape; it stays inside this adapter
// and callers receive app.RunPolicy values only. Check is a pointer so a
// missing [check] table is distinguishable and rejected. The Phase 3
// sections ([workflow], [workers], [retry], [messages], [roles.*]) are
// shape-validated whenever PRESENT regardless of mode — `hop run
// --workflow feature` can override a solo-mode file (design section 10),
// so a file may carry them without setting mode = "feature" — while
// their defaults and the feature-mode requireds are applied and enforced
// only under mode = "feature", through the shared app helpers the
// override path calls too (app.ApplyWorkflowDefaults,
// app.ValidateFeaturePolicy). A file with none of them loads to the
// exact Phase 2 policy, every Phase 3 field zero.
type fileConfig struct {
	Check    *checkSection    `toml:"check"`
	Env      envSection       `toml:"env"`
	Worker   workerSection    `toml:"worker"`
	Profile  profileSection   `toml:"profile"`
	Workflow *workflowSection `toml:"workflow"`
	Workers  *workersSection  `toml:"workers"`
	Retry    *retrySection    `toml:"retry"`
	Messages *messagesSection `toml:"messages"`
	Roles    *rolesSection    `toml:"roles"`
}

type checkSection struct {
	Command []string `toml:"command"`
	// Timeout is a pointer so an absent key (defaulted) is distinguishable
	// from an explicitly supplied value, which is always validated.
	Timeout    *string `toml:"timeout"`
	Repeatable bool    `toml:"repeatable"`
}

type envSection struct {
	Strip       []string `toml:"strip"`
	Passthrough []string `toml:"passthrough"`
}

type workerSection struct {
	// Harness is a pointer so an absent key (defaulted to claude) is
	// distinguishable from an explicitly supplied value, which is always
	// validated against the supported set.
	Harness *string `toml:"harness"`
}

type profileSection struct {
	Dir string `toml:"dir"`
}

type workflowSection struct {
	// Mode is a pointer so an absent key (solo semantics, the zero-value
	// policy) is distinguishable from an explicitly supplied value, which
	// is always validated — "" is a rejection, never a silent default.
	Mode *string `toml:"mode"`
}

type workersSection struct {
	// Max is a pointer so an absent key (defaulted to 2 in feature mode)
	// is distinguishable from an explicit value, which is always
	// validated — 0 is a rejection, never "absent".
	Max *int `toml:"max"`
}

type retrySection struct {
	// MaxAttempts is a pointer for the same absent-vs-explicit reason as
	// workersSection.Max; the feature-mode default is 3.
	MaxAttempts *int `toml:"max_attempts"`
}

type messagesSection struct {
	// Both durations are pointers like checkSection.Timeout: an absent key
	// defaults (120s / 50s, feature mode), an explicit value is validated.
	AttentionAfter *string `toml:"attention_after"`
	WaitTimeout    *string `toml:"wait_timeout"`
}

type rolesSection struct {
	Manager     *roleSection         `toml:"manager"`
	Implementer *roleSection         `toml:"implementer"`
	Reviewer    *reviewerRoleSection `toml:"reviewer"`
}

type roleSection struct {
	// Instructions is a pointer so a present-but-empty value is
	// distinguishable from an absent key: a [roles.<role>] table without
	// instructions is a rejection — the table exists only to name the
	// role's instructions file.
	Instructions *string `toml:"instructions"`
}

type reviewerRoleSection struct {
	Instructions *string `toml:"instructions"`
	// Harness is a pointer so an absent key (defaulted to the worker
	// harness in feature mode) is distinguishable from an explicit value,
	// which is always validated against the supported set.
	Harness *string `toml:"harness"`
}

// Load reads and validates `<repositoryRoot>/.herdr-orchestrator/
// config.toml` into the run policy `hop run` freezes. It fails on a
// missing or unreadable file, any strict-decoding violation, a missing
// [check] command, a non-positive, unparsable or explicitly empty timeout,
// a harness outside the supported set (an explicitly empty one included;
// only an absent key defaults), and a profile directory containing control
// bytes. A relative profile directory is resolved to an absolute path
// against the repository root here, at load, so the frozen policy never
// carries a working-directory-dependent path. Diagnostics name the file,
// the key and the expected shape or set, never the supplied value.
func (Source) Load(ctx context.Context, repositoryRoot string) (app.RunPolicy, error) {
	if err := ctx.Err(); err != nil {
		return app.RunPolicy{}, err
	}
	path := filepath.Join(repositoryRoot, configRelPath)
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a fixed file name under the caller's repository root; loading it is this adapter's purpose.
	if err != nil {
		return app.RunPolicy{}, fmt.Errorf("read run policy: %w", err)
	}
	var file fileConfig
	decoder := toml.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return app.RunPolicy{}, decodeError(path, err)
	}
	return policyFromFile(&file, repositoryRoot, path)
}

// decodeError renders a strict-decoding failure with the file, position and
// offending key named and the expected shape stated, so a typo in the file
// is directly actionable. Supplied values are never echoed, and the
// decoder's own message is never rendered — it can quote document content —
// so a secret pasted into the file cannot leak through a rendered error.
func decodeError(path string, err error) error {
	if strict, ok := errors.AsType[*toml.StrictMissingError](err); ok {
		keys := make([]string, 0, len(strict.Errors))
		for i := range strict.Errors {
			keys = append(keys, strings.Join(strict.Errors[i].Key(), "."))
		}
		return fmt.Errorf("%s: unknown key %s: the policy file is decoded strictly and every key must be one the design declares", path, strings.Join(keys, ", "))
	}
	if decodeErr, ok := errors.AsType[*toml.DecodeError](err); ok {
		row, column := decodeErr.Position()
		key := strings.Join(decodeErr.Key(), ".")
		// The decoder's duplicate messages ("already defined", "already
		// exists") carry only the key, and the distinction matters to the
		// reader; the sniff is pinned by the duplicate-key table tests.
		duplicate := strings.Contains(decodeErr.Error(), "already defined") || strings.Contains(decodeErr.Error(), "already exists")
		switch {
		case key != "" && duplicate:
			return fmt.Errorf("%s:%d:%d: key %s is defined more than once", path, row, column, key)
		case key != "":
			return fmt.Errorf("%s:%d:%d: key %s must be %s", path, row, column, key, expectedShape(key))
		default:
			return fmt.Errorf("%s:%d:%d: the document is not valid TOML at this position", path, row, column)
		}
	}
	return fmt.Errorf("decode %s: %w", path, err)
}

// expectedShape names the declared shape of one policy key, so a decode
// failure can state what was expected without echoing what was supplied.
func expectedShape(key string) string {
	switch key {
	case "check.command", "env.strip", "env.passthrough":
		return "an array of strings"
	case "check.timeout", "messages.attention_after", "messages.wait_timeout":
		return "a string holding a Go duration"
	case "check.repeatable":
		return "a boolean"
	case "worker.harness", "profile.dir", "workflow.mode",
		"roles.manager.instructions", "roles.implementer.instructions",
		"roles.reviewer.instructions", "roles.reviewer.harness":
		return "a string"
	case "workers.max", "retry.max_attempts":
		return "an integer"
	}
	return "the shape the package guide's example shows"
}

// policyFromFile validates the decoded file and assembles the RunPolicy
// with the design's defaults: timeout 10m, repeatable false, harness
// claude.
func policyFromFile(file *fileConfig, repositoryRoot, path string) (app.RunPolicy, error) {
	if file.Check == nil || len(file.Check.Command) == 0 {
		return app.RunPolicy{}, fmt.Errorf("%s: [check] command is required; a policy without a check cannot gate completion", path)
	}
	// Diagnostics below name the file and key and what was expected, never
	// the supplied value or a wrapped cause that would echo it: a mistaken
	// paste of a secret into the policy file must not leak through a
	// rendered error.
	timeout := defaultCheckTimeout
	if file.Check.Timeout != nil {
		parsed, err := time.ParseDuration(*file.Check.Timeout)
		if err != nil {
			return app.RunPolicy{}, fmt.Errorf("%s: check.timeout is not a Go duration string; use forms like \"10m\" or \"90s\"", path)
		}
		if parsed <= 0 {
			return app.RunPolicy{}, fmt.Errorf("%s: check.timeout must be a positive duration", path)
		}
		timeout = parsed
	}
	harness := app.HarnessClaude
	if file.Worker.Harness != nil {
		harness = *file.Worker.Harness
	}
	switch harness {
	case app.HarnessClaude, app.HarnessCodex, app.HarnessOpencode:
	default:
		return app.RunPolicy{}, fmt.Errorf("%s: worker.harness is not a supported harness; supported harnesses are %q, %q and %q", path, app.HarnessClaude, app.HarnessCodex, app.HarnessOpencode)
	}
	profileDir, err := resolveProfileDir(file.Profile.Dir, repositoryRoot, path)
	if err != nil {
		return app.RunPolicy{}, err
	}
	policy := app.RunPolicy{
		CheckArgv:       slices.Clone(file.Check.Command),
		CheckTimeout:    timeout,
		CheckRepeatable: file.Check.Repeatable,
		EnvStrip:        slices.Clone(file.Env.Strip),
		EnvPassthrough:  slices.Clone(file.Env.Passthrough),
		ProfileDir:      profileDir,
		Harness:         harness,
	}
	if err := applyWorkflowKeys(&policy, file, repositoryRoot, path); err != nil {
		return app.RunPolicy{}, err
	}
	return policy, nil
}

// applyWorkflowKeys validates and applies the Phase 3 policy sections
// (docs/plan/phase-3-design.md section 3). Every PRESENT key is validated
// regardless of mode, so a solo-mode file carrying feature sections still
// fails on a bad value and `hop run --workflow feature` can override the
// mode later; the defaults and feature-mode requireds are applied only
// under mode = "feature", through the shared app helpers the override
// path calls too. A file with none of these sections leaves the policy's
// Phase 3 fields at their zero values — the exact Phase 2 result.
func applyWorkflowKeys(policy *app.RunPolicy, file *fileConfig, repositoryRoot, path string) error {
	if file.Workflow != nil && file.Workflow.Mode != nil {
		switch *file.Workflow.Mode {
		case app.WorkflowModeSolo, app.WorkflowModeFeature:
			policy.WorkflowMode = *file.Workflow.Mode
		default:
			return fmt.Errorf("%s: workflow.mode is not a supported mode; supported modes are %q and %q", path, app.WorkflowModeSolo, app.WorkflowModeFeature)
		}
	}
	if file.Workers != nil && file.Workers.Max != nil {
		if *file.Workers.Max < 1 {
			return fmt.Errorf("%s: workers.max must be at least 1", path)
		}
		policy.MaxWorkers = *file.Workers.Max
	}
	if file.Retry != nil && file.Retry.MaxAttempts != nil {
		if *file.Retry.MaxAttempts < 1 {
			return fmt.Errorf("%s: retry.max_attempts must be at least 1", path)
		}
		policy.RetryLimit = *file.Retry.MaxAttempts
	}
	if file.Messages != nil {
		attention, err := parseMessageDuration(file.Messages.AttentionAfter, "messages.attention_after", path)
		if err != nil {
			return err
		}
		policy.MessageAttention = attention
		wait, err := parseMessageDuration(file.Messages.WaitTimeout, "messages.wait_timeout", path)
		if err != nil {
			return err
		}
		policy.MessageWait = wait
	}
	if file.Roles != nil {
		manager, err := resolveRoleInstructions(file.Roles.Manager == nil, roleInstructions(file.Roles.Manager), "roles.manager", repositoryRoot, path)
		if err != nil {
			return err
		}
		policy.ManagerRole = manager
		implementer, err := resolveRoleInstructions(file.Roles.Implementer == nil, roleInstructions(file.Roles.Implementer), "roles.implementer", repositoryRoot, path)
		if err != nil {
			return err
		}
		policy.ImplementerRole = implementer
		if file.Roles.Reviewer != nil {
			reviewer, err := resolveRoleInstructions(false, file.Roles.Reviewer.Instructions, "roles.reviewer", repositoryRoot, path)
			if err != nil {
				return err
			}
			policy.ReviewerRole = reviewer
			if file.Roles.Reviewer.Harness != nil {
				switch *file.Roles.Reviewer.Harness {
				case app.HarnessClaude, app.HarnessCodex, app.HarnessOpencode:
					policy.ReviewerHarness = *file.Roles.Reviewer.Harness
				default:
					return fmt.Errorf("%s: roles.reviewer.harness is not a supported harness; supported harnesses are %q, %q and %q", path, app.HarnessClaude, app.HarnessCodex, app.HarnessOpencode)
				}
			}
		}
	}
	if policy.WorkflowMode != app.WorkflowModeFeature {
		return nil
	}
	app.ApplyWorkflowDefaults(policy)
	if err := app.ValidateFeaturePolicy(policy); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// roleInstructions reads a role table's instructions pointer, nil-safe.
func roleInstructions(section *roleSection) *string {
	if section == nil {
		return nil
	}
	return section.Instructions
}

// resolveRoleInstructions validates one [roles.<role>] table: an absent
// table stays "", a present table must name a non-empty instructions
// path with no control byte, and a relative path resolves against the
// repository's .herdr-orchestrator directory at load — like profile.dir,
// so the frozen policy never carries a working-directory-dependent path.
func resolveRoleInstructions(absent bool, instructions *string, key, repositoryRoot, path string) (string, error) {
	if absent {
		return "", nil
	}
	if instructions == nil || *instructions == "" {
		return "", fmt.Errorf("%s: %s.instructions is required; the table exists only to name the role's instructions file", path, key)
	}
	dir := *instructions
	if containsControlByte(dir) {
		return "", fmt.Errorf("%s: %s.instructions contains a control byte, which HOP does not permit in a role path", path, key)
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repositoryRoot, ".herdr-orchestrator", dir)
	}
	if !filepath.IsAbs(dir) {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", fmt.Errorf("%s: resolve %s.instructions to an absolute path: %w", path, key, err)
		}
		dir = abs
	}
	return filepath.Clean(dir), nil
}

// parseMessageDuration validates one [messages] duration key: absent
// stays zero (defaulted only in feature mode), an explicit value must be
// a positive Go duration.
func parseMessageDuration(value *string, key, path string) (time.Duration, error) {
	if value == nil {
		return 0, nil
	}
	parsed, err := time.ParseDuration(*value)
	if err != nil {
		return 0, fmt.Errorf("%s: %s is not a Go duration string; use forms like \"120s\" or \"2m\"", path, key)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s: %s must be a positive duration", path, key)
	}
	return parsed, nil
}

// resolveProfileDir resolves an optional profile.dir to an absolute,
// cleaned path against the repository root and rejects a value carrying a
// control byte (any byte below 0x20), matching the launch-environment
// policy the value is later frozen into. An empty value selects the
// harness's default profile and stays empty.
func resolveProfileDir(dir, repositoryRoot, path string) (string, error) {
	if dir == "" {
		return "", nil
	}
	if containsControlByte(dir) {
		return "", fmt.Errorf("%s: profile.dir contains a control byte, which HOP does not permit in a profile path", path)
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repositoryRoot, dir)
	}
	if !filepath.IsAbs(dir) {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", fmt.Errorf("%s: resolve profile.dir to an absolute path: %w", path, err)
		}
		dir = abs
	}
	return filepath.Clean(dir), nil
}

// containsControlByte reports whether s carries any byte below 0x20, the
// same contract app.EnvPolicy.Validate enforces when the policy is frozen.
func containsControlByte(s string) bool {
	for i := range len(s) {
		if s[i] < 0x20 {
			return true
		}
	}
	return false
}
