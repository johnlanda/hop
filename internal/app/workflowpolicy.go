package app

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// Workflow policy modes ([workflow] mode). An absent [workflow] table
// loads as the zero value, which means solo exactly like the explicit
// spelling (docs/plan/phase-3-design.md section 3).
const (
	WorkflowModeSolo    = "solo"
	WorkflowModeFeature = "feature"
)

// The design's feature-mode defaults (docs/plan/phase-3-design.md
// section 3), applied only where a key is absent — an explicitly supplied
// value is always validated, never silently defaulted.
const (
	// DefaultMaxWorkers is [workers] max when absent.
	DefaultMaxWorkers = 2
	// DefaultRetryLimit is [retry] max_attempts when absent.
	DefaultRetryLimit = 3
	// DefaultMessageAttention is [messages] attention_after when absent.
	DefaultMessageAttention = 120 * time.Second
	// DefaultMessageWait is [messages] wait_timeout when absent.
	DefaultMessageWait = 50 * time.Second
)

// ErrFeaturePolicyInvalid wraps every feature-mode policy refusal
// ValidateFeaturePolicy reports, so both callers — the config adapter for
// a file that sets mode = "feature", and hop run's --workflow feature
// override path (slice 6) — surface the same classified failure.
var ErrFeaturePolicyInvalid = errors.New("app: run policy cannot drive a feature-mode run")

// ApplyWorkflowDefaults fills p's ABSENT feature-mode fields with the
// design's defaults: workers 2, retry 3, attention 120s, wait 50s, and
// the reviewer harness defaulting to the worker harness. It is the one
// defaults implementation for both the config adapter (a file whose
// [workflow] mode is "feature") and the --workflow feature override path
// (slice 6), so the two can never disagree. Zero values mean absent —
// the adapter rejects explicit zeros before this runs — and a solo
// policy is never passed here, so a repository that never opts into
// feature mode keeps every Phase 3 field at its zero value.
func ApplyWorkflowDefaults(p *RunPolicy) {
	if p.MaxWorkers == 0 {
		p.MaxWorkers = DefaultMaxWorkers
	}
	if p.RetryLimit == 0 {
		p.RetryLimit = DefaultRetryLimit
	}
	if p.MessageAttention == 0 {
		p.MessageAttention = DefaultMessageAttention
	}
	if p.MessageWait == 0 {
		p.MessageWait = DefaultMessageWait
	}
	if p.ReviewerHarness == "" {
		p.ReviewerHarness = p.Harness
	}
}

// ValidateFeaturePolicy checks that p, after ApplyWorkflowDefaults, can
// drive a feature-mode run: the three role instruction paths are the
// feature-mode requireds (present and absolute — the loader resolves
// relative paths against .herdr-orchestrator/ at load, so the frozen
// policy never carries a working-directory-dependent path), the bounds
// and durations are positive, and the reviewer harness is supported.
// Shared by the config adapter and the flag-override path exactly like
// ApplyWorkflowDefaults. Errors name the key and the expected shape,
// wrap ErrFeaturePolicyInvalid, and never echo a supplied value.
func ValidateFeaturePolicy(p *RunPolicy) error {
	roles := []struct{ key, path string }{
		{"roles.manager.instructions", p.ManagerRole},
		{"roles.implementer.instructions", p.ImplementerRole},
		{"roles.reviewer.instructions", p.ReviewerRole},
	}
	for _, role := range roles {
		if role.path == "" {
			return fmt.Errorf("%w: %s is required in feature mode", ErrFeaturePolicyInvalid, role.key)
		}
		if !filepath.IsAbs(role.path) {
			return fmt.Errorf("%w: %s must be an absolute path once loaded; the loader resolves relative paths against .herdr-orchestrator", ErrFeaturePolicyInvalid, role.key)
		}
	}
	if p.MaxWorkers < 1 {
		return fmt.Errorf("%w: workers.max must be at least 1", ErrFeaturePolicyInvalid)
	}
	if p.RetryLimit < 1 {
		return fmt.Errorf("%w: retry.max_attempts must be at least 1", ErrFeaturePolicyInvalid)
	}
	if p.MessageAttention <= 0 {
		return fmt.Errorf("%w: messages.attention_after must be a positive duration", ErrFeaturePolicyInvalid)
	}
	if p.MessageWait <= 0 {
		return fmt.Errorf("%w: messages.wait_timeout must be a positive duration", ErrFeaturePolicyInvalid)
	}
	switch p.ReviewerHarness {
	case HarnessClaude, HarnessCodex, HarnessOpencode:
	default:
		return fmt.Errorf("%w: roles.reviewer.harness is not a supported harness; supported harnesses are %q, %q and %q", ErrFeaturePolicyInvalid, HarnessClaude, HarnessCodex, HarnessOpencode)
	}
	return nil
}
