package app

import "github.com/johnlanda/hop/internal/domain/identity"

// NewRunHandleForTest exposes newRunHandle to app_test: scheduler and
// messaging scenarios seed a feature-mode run directly into the fake store
// (bypassing StartRun's Phase 2 one-task/one-attempt bootstrap) and need a
// legitimate RunHandle over the lease they seeded.
func NewRunHandleForTest(runID identity.RunID, lease Lease) RunHandle {
	return newRunHandle(runID, lease)
}

// RenderContinuationPromptForTest exposes renderContinuationPrompt to
// app_test, so scenarios scripting the argv of HOP's own cold-relaunched
// worker reproduce the pinned relaunch shape
// (`--resume <native-ref> <continuation prompt>`) from the one production
// template rather than an ad-hoc string.
func RenderContinuationPromptForTest(assignmentPath, hopPath string) string {
	return renderContinuationPrompt(assignmentPath, hopPath)
}
