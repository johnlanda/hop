package main

import (
	"fmt"
	"path/filepath"
)

// State-root source labels reported by hop doctor.
const (
	stateRootSourceOverride = "override"
	stateRootSourceDefault  = "default"
)

// resolveStateRoot is the single state-root resolution rule every controller
// entrypoint applies: `${XDG_STATE_HOME:-$HOME/.local/state}/hop`, with
// HOP_STATE_DIR as the only override (docs/plan/phase-2-design.md
// section 4). HERDR_PLUGIN_STATE_DIR is deliberately not consulted. The
// returned root is always absolute and lexically cleaned; source labels
// where it came from (stateRootSourceOverride or stateRootSourceDefault). A
// relative HOP_STATE_DIR or XDG_STATE_HOME never silently selects a
// working-directory-dependent store: the override is refused, and a
// relative XDG_STATE_HOME is ignored in favor of $HOME per the XDG base
// directory rules.
func resolveStateRoot(getenv func(string) string) (root, source string, err error) {
	if override := getenv("HOP_STATE_DIR"); override != "" {
		if !filepath.IsAbs(override) {
			// The value is never echoed: a mistaken paste into the
			// variable must not surface in diagnostics or logs.
			return "", "", fmt.Errorf("HOP_STATE_DIR is set to a relative path; the override must be an absolute directory")
		}
		return filepath.Clean(override), stateRootSourceOverride, nil
	}
	if xdg := getenv("XDG_STATE_HOME"); filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "hop"), stateRootSourceDefault, nil
	}
	home := getenv("HOME")
	if !filepath.IsAbs(home) {
		return "", "", fmt.Errorf("cannot resolve the state root: HOP_STATE_DIR and XDG_STATE_HOME are unset and HOME is not an absolute path")
	}
	return filepath.Join(home, ".local", "state", "hop"), stateRootSourceDefault, nil
}

// requireWorkerStateRoot is the worker-context rule of the two exec-boundary
// commands and hop result submit: the absolute HOP_STATE_DIR provided at
// pane creation is REQUIRED, and a missing or relative value is a
// diagnostic failure naming the lost variable — never a fallback to the
// default resolution, which in a worker context could silently open an
// unrelated store (docs/plan/phase-2-design.md section 4).
func requireWorkerStateRoot(getenv func(string) string) (string, error) {
	root := getenv("HOP_STATE_DIR")
	switch {
	case root == "":
		return "", fmt.Errorf("HOP_STATE_DIR is not set; this command runs in a HOP-launched worker context, which provides the absolute state root, and never falls back to the default resolution")
	case !filepath.IsAbs(root):
		// The value is never echoed (exec-boundary diagnostic
		// confidentiality): only the variable is named.
		return "", fmt.Errorf("HOP_STATE_DIR is not an absolute path; the launch-provided state root is always absolute and is never resolved from anything else")
	}
	return filepath.Clean(root), nil
}
