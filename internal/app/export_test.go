package app

import (
	"context"

	"github.com/johnlanda/hop/internal/domain/identity"
)

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

// CheckoutVerdictForTest is inspectAttemptCheckout's verdict with its
// disposition rendered as a string, for app_test.
type CheckoutVerdictForTest struct {
	Disposition string
	Retained    WorktreeRetainedCategory
	Released    WorktreeReleaseReason
	ListedPath  string
	WasAbsent   bool
	Detail      string
}

// InspectAttemptCheckoutForTest exposes inspectAttemptCheckout to
// app_test, whose fake git models the linked attempt worktrees.
func InspectAttemptCheckoutForTest(ctx context.Context, c *Controller, root, path, branch, baseOID string, inspect PathInspector) CheckoutVerdictForTest {
	v := c.inspectAttemptCheckout(ctx, &attemptCheckout{RepositoryRoot: root, Path: path, Branch: branch, BaseOID: baseOID}, inspect)
	return CheckoutVerdictForTest{
		Disposition: string(v.Disposition), Retained: v.Retained, Released: v.Released,
		ListedPath: v.ListedPath, WasAbsent: v.WasAbsent, Detail: v.Detail,
	}
}
