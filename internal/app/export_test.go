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

// RetirementListingOutputBytesForTest is retirementListingOutputBytes,
// the capture bound of the index scan and the worktree listing.
const RetirementListingOutputBytesForTest = retirementListingOutputBytes

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

// DetectionForTest is one detection step's result, rendered as strings.
type DetectionForTest struct {
	State, Head, Target, Detail string
}

// DetectForRetirementForTest exposes the pass's detection step
// (recovery of unresolved checks, then detectRetirementMerge) to
// app_test, under the handle's lease.
func DetectForRetirementForTest(ctx context.Context, c *Controller, handle RunHandle, hopPath string, environ []string) (DetectionForTest, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; test bridge.
	frozen, err := c.Read.LoadFrozenRun(ctx, handle.runID)
	if err != nil {
		return DetectionForTest{}, err
	}
	spawnEnv, err := c.CheckSpawnEnvironment(ctx, handle, environ)
	if err != nil {
		return DetectionForTest{}, err
	}
	d, err := c.detectForRetirement(ctx, handle, &frozen, &retirementPassOptions{HOPPath: hopPath, SpawnEnv: spawnEnv})
	return DetectionForTest{State: string(d.State), Head: d.Head, Target: d.Target, Detail: d.Detail}, err
}

// RetirementRowForTest is one removal-step row report, rendered as
// strings.
type RetirementRowForTest struct {
	WorktreeID   string
	Branch       string
	Path         string
	Outcome      string
	Retained     WorktreeRetainedCategory
	Released     WorktreeReleaseReason
	LeftOnDisk   bool
	EvidencePath string
	ExitCode     int
	Detail       string
}

// RemoveRunWorktreesForTest exposes the pass's removal step to app_test,
// under the handle's lease.
func RemoveRunWorktreesForTest(ctx context.Context, c *Controller, handle RunHandle, hopPath string, environ []string, inspect PathInspector) ([]RetirementRowForTest, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; test bridge.
	frozen, err := c.Read.LoadFrozenRun(ctx, handle.runID)
	if err != nil {
		return nil, err
	}
	spawnEnv, err := c.CheckSpawnEnvironment(ctx, handle, environ)
	if err != nil {
		return nil, err
	}
	reports, err := c.removeRunWorktrees(ctx, handle, &frozen, &retirementPassOptions{HOPPath: hopPath, SpawnEnv: spawnEnv, InspectPath: inspect})
	out := make([]RetirementRowForTest, 0, len(reports))
	for i := range reports {
		r := &reports[i]
		out = append(out, RetirementRowForTest{
			WorktreeID: r.WorktreeID.String(), Branch: r.Branch, Path: r.Path, Outcome: string(r.Outcome),
			Retained: r.Retained, Released: r.Released, LeftOnDisk: r.LeftOnDisk,
			EvidencePath: r.EvidencePath, ExitCode: r.ExitCode, Detail: r.Detail,
		})
	}
	return out, err
}

// RecoverRetirementForTest exposes the pass's recovery step to app_test,
// under the handle's lease, returning its blocking detail.
func RecoverRetirementForTest(ctx context.Context, c *Controller, handle RunHandle, hopPath string, inspect PathInspector) (string, error) { //nolint:gocritic // hugeParam: RunHandle carries a Lease value by design; test bridge.
	frozen, err := c.Read.LoadFrozenRun(ctx, handle.runID)
	if err != nil {
		return "", err
	}
	return c.recoverRetirementOperations(ctx, handle, &frozen, &retirementPassOptions{HOPPath: hopPath, InspectPath: inspect})
}
