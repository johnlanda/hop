package main

import (
	"testing"
)

// hop view set/clear's SUCCESS shapes ("view set: <label>"/"view
// cleared") are NOT covered anywhere in this suite: viewcmd.go wires
// withRuntime: true, and internal/app/controller.go's SelectRunView/
// ClearRunView UNCONDITIONALLY call the Presentation port when the
// Controller carries one (Presenter.Focus/.Clear dial
// AgentPresentation.SelectView/.ClearView directly, with no store-only
// short-circuit) — confirmed by reading controller.go, not empirically,
// since exercising it would require actually dialing Herdr. Unlike hop
// status, there is no success path reachable without a live Herdr
// connection; both shapes are SKIPPED in this suite's coverage table for
// exactly that reason.

// TestGrammarContractViewSetUsage covers hop view set's usage-error
// branch (--run required, unexpected argument) and hop view's own
// dispatch usage (no or unknown subcommand); all validated before any
// store is opened, so no fixture is needed.
func TestGrammarContractViewSetUsage(t *testing.T) {
	dir := realDir(t)
	cases := []struct {
		name string
		args []string
	}{
		{"missing run", []string{"view", "set"}},
		{"unexpected argument", []string{"view", "set", "--run", testUUID(1), "extra"}},
		{"dispatch: no subcommand", []string{"view"}},
		{"dispatch: unknown subcommand", []string{"view", "bogus"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := execHop(t, map[string]string{}, dir, tc.args...)
			if result.ExitCode != exitUsage {
				t.Fatalf("exit = %d, want %d (usage); stdout=%q stderr=%q", result.ExitCode, exitUsage, result.Stdout, result.Stderr)
			}
		})
	}
}

// TestGrammarContractViewClearUsage covers hop view clear's usage-error
// branch (an unexpected argument); no fixture needed.
func TestGrammarContractViewClearUsage(t *testing.T) {
	dir := realDir(t)
	result := execHop(t, map[string]string{}, dir, "view", "clear", "extra")
	if result.ExitCode != exitUsage {
		t.Fatalf("exit = %d, want %d (usage); stdout=%q stderr=%q", result.ExitCode, exitUsage, result.Stdout, result.Stderr)
	}
}

// TestGrammarContractViewSetUnknownRunLabel proves hop view set's
// unknown-run-label usage refusal: resolveRunArg and runLabel both run
// (and both fail, against an empty repository listing) BEFORE
// SelectRunView is ever called, so this shape needs no fixture and never
// touches Herdr — the canary here proves it stays that way.
func TestGrammarContractViewSetUnknownRunLabel(t *testing.T) {
	dir := realDir(t)
	result := execHop(t, map[string]string{}, dir, "view", "set", "-C", dir, "--run", "r1")
	if result.ExitCode != exitUsage {
		t.Fatalf("exit = %d, want %d (usage); stdout=%q stderr=%q", result.ExitCode, exitUsage, result.Stdout, result.Stderr)
	}
}

// TestGrammarContractViewSetUnknownRunID proves hop view set's
// "run has no status detail" command failure for a well-formed but
// unknown run id: resolveRunArg passes a non-r<seq> argument through
// unchanged, runLabel's own Status lookup then fails closed, still
// entirely before SelectRunView — no fixture, no Herdr contact.
func TestGrammarContractViewSetUnknownRunID(t *testing.T) {
	dir := realDir(t)
	result := execHop(t, map[string]string{}, dir, "view", "set", "-C", dir, "--run", testUUID(1))
	if result.ExitCode != exitFailure {
		t.Fatalf("exit = %d, want %d (failure); stdout=%q stderr=%q", result.ExitCode, exitFailure, result.Stdout, result.Stderr)
	}
	if result.Stdout != "" {
		t.Errorf("unknown run id wrote to stdout: %q", result.Stdout)
	}
}
