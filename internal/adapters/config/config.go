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
// missing [check] table is distinguishable and rejected.
type fileConfig struct {
	Check   *checkSection  `toml:"check"`
	Env     envSection     `toml:"env"`
	Worker  workerSection  `toml:"worker"`
	Profile profileSection `toml:"profile"`
}

type checkSection struct {
	Command    []string `toml:"command"`
	Timeout    string   `toml:"timeout"`
	Repeatable bool     `toml:"repeatable"`
}

type envSection struct {
	Strip       []string `toml:"strip"`
	Passthrough []string `toml:"passthrough"`
}

type workerSection struct {
	Harness string `toml:"harness"`
}

type profileSection struct {
	Dir string `toml:"dir"`
}

// Load reads and validates `<repositoryRoot>/.herdr-orchestrator/
// config.toml` into the run policy `hop run` freezes. It fails on a
// missing or unreadable file, any strict-decoding violation, a missing
// [check] command, a non-positive or unparsable timeout, an unsupported
// harness, and a profile directory containing control bytes. A relative
// profile directory is resolved to an absolute path against the repository
// root here, at load, so the frozen policy never carries a
// working-directory-dependent path.
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

// decodeError renders a strict-decoding failure with the offending key
// named, so a typo in the file is directly actionable.
func decodeError(path string, err error) error {
	if strict, ok := errors.AsType[*toml.StrictMissingError](err); ok {
		keys := make([]string, 0, len(strict.Errors))
		for i := range strict.Errors {
			keys = append(keys, strings.Join(strict.Errors[i].Key(), "."))
		}
		return fmt.Errorf("%s: unknown key %s: the policy file is decoded strictly and every key must be one the design declares", path, strings.Join(keys, ", "))
	}
	if decodeErr, ok := errors.AsType[*toml.DecodeError](err); ok {
		return fmt.Errorf("%s: key %s: %w", path, strings.Join(decodeErr.Key(), "."), err)
	}
	return fmt.Errorf("decode %s: %w", path, err)
}

// policyFromFile validates the decoded file and assembles the RunPolicy
// with the design's defaults: timeout 10m, repeatable false, harness
// claude.
func policyFromFile(file *fileConfig, repositoryRoot, path string) (app.RunPolicy, error) {
	if file.Check == nil || len(file.Check.Command) == 0 {
		return app.RunPolicy{}, fmt.Errorf("%s: [check] command is required; a policy without a check cannot gate completion", path)
	}
	timeout := defaultCheckTimeout
	if file.Check.Timeout != "" {
		parsed, err := time.ParseDuration(file.Check.Timeout)
		if err != nil {
			return app.RunPolicy{}, fmt.Errorf("%s: check.timeout %q is not a duration (use forms like \"10m\" or \"90s\"): %w", path, file.Check.Timeout, err)
		}
		if parsed <= 0 {
			return app.RunPolicy{}, fmt.Errorf("%s: check.timeout %q must be positive", path, file.Check.Timeout)
		}
		timeout = parsed
	}
	harness := file.Worker.Harness
	if harness == "" {
		harness = app.HarnessClaude
	}
	switch harness {
	case app.HarnessClaude, app.HarnessCodex, app.HarnessOpencode:
	default:
		return app.RunPolicy{}, fmt.Errorf("%s: worker.harness %q is not a supported harness; supported harnesses are %q, %q and %q", path, harness, app.HarnessClaude, app.HarnessCodex, app.HarnessOpencode)
	}
	profileDir, err := resolveProfileDir(file.Profile.Dir, repositoryRoot, path)
	if err != nil {
		return app.RunPolicy{}, err
	}
	return app.RunPolicy{
		CheckArgv:       slices.Clone(file.Check.Command),
		CheckTimeout:    timeout,
		CheckRepeatable: file.Check.Repeatable,
		EnvStrip:        slices.Clone(file.Env.Strip),
		EnvPassthrough:  slices.Clone(file.Env.Passthrough),
		ProfileDir:      profileDir,
		Harness:         harness,
	}, nil
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
