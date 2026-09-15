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

	// Phase 3 feature-mode additions (docs/plan/phase-3-design.md
	// section 3). Every field below is the zero value for a repository
	// that never sets [workflow] mode = "feature"; the config adapter
	// (internal/adapters/config, slice 4) is responsible for defaults and
	// feature-mode-required validation. Solo-mode code never reads them.

	// WorkflowMode is [workflow] mode: "solo" (default, and the zero
	// value) or "feature".
	WorkflowMode string
	// MaxWorkers is [workers] max: the bound on non-terminal, non-manager
	// sessions (implementer and reviewer share the bound); default 2.
	MaxWorkers int
	// RetryLimit is [retry] max_attempts: the per-task frozen retry
	// budget; default 3.
	RetryLimit int
	// ManagerRole, ImplementerRole and ReviewerRole are the
	// [roles.manager]/[roles.implementer]/[roles.reviewer] instructions
	// paths, relative to .herdr-orchestrator/; required in feature mode.
	ManagerRole     string
	ImplementerRole string
	ReviewerRole    string
	// ReviewerHarness is [roles.reviewer] harness; default [worker]
	// harness.
	ReviewerHarness string
	// MessageAttention is [messages] attention_after: the status
	// surface's "needs attention" age threshold; default 120s.
	MessageAttention time.Duration
	// MessageWait is [messages] wait_timeout: hop msg wait's default
	// bound; default 50s.
	MessageWait time.Duration
}

// ConfigurationSource loads and validates one repository's policy. The
// config adapter implements it with strict TOML 1.0 decoding and unknown-key
// rejection.
type ConfigurationSource interface {
	Load(ctx context.Context, repositoryRoot string) (RunPolicy, error)
}
