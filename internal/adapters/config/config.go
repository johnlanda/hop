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
	case "check.timeout":
		return "a string holding a Go duration"
	case "check.repeatable":
		return "a boolean"
	case "worker.harness", "profile.dir":
		return "a string"
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
