package app

import (
	"context"
	"time"
)

// RunPolicy is one repository's decoded `.herdr-orchestrator/config.toml`:
// the check contract and the raw environment/harness/profile choices `hop
// run` freezes into a RunSnapshot.
type RunPolicy struct {
	// CheckArgv is the [check] command; Load fails without it.
	CheckArgv []string
	// CheckTimeout is the [check] timeout; default 10m.
	CheckTimeout time.Duration
	// CheckRepeatable is [check] repeatable; default false (section 7
	// unknown-outcome rule).
	CheckRepeatable bool
	// EnvStrip lists [env] strip additions to the harness matrix.
	EnvStrip []string
	// EnvPassthrough lists [env] passthrough opt-ins.
	EnvPassthrough []string
	// ProfileDir is [profile] dir, resolved to an absolute path against the
	// repository root at load; "" selects the harness's default profile.
	ProfileDir string
	// Harness is [worker] harness; default "claude". Phase 2 launches only
	// claude.
	Harness string
}

// ConfigurationSource loads and validates one repository's policy. The
// config adapter implements it with strict TOML 1.0 decoding and unknown-key
// rejection.
type ConfigurationSource interface {
	Load(ctx context.Context, repositoryRoot string) (RunPolicy, error)
}
