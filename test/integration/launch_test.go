package integration

import (
	"fmt"
	"strings"
	"testing"
)

// TestRealProcessDuplicateLaunchInvocation proves design section 6's
// duplicate-launcher rule directly: a second `hop launch --run --attempt`
// invocation for an incarnation that already has a launch claim — run here
// as our own one-shot subprocess, whose own pid necessarily differs from
// the settled claim's recorded pid — is rejected by the claim's
// different-pid rule before it ever resolves an executable or execs
// anything, leaving the original worker completely undisturbed.
func TestRealProcessDuplicateLaunchInvocation(t *testing.T) {
	fx := startFixtureRun(t, "") // idling worker: settles normally once left undisturbed.
	taskID, attemptID := fx.taskAndAttemptIDs(t)

	// PrepareLaunchExec refuses on the attempt's own state before it ever
	// reaches the claim's different-pid rule ("attempt is running, not
	// launching or relaunching"), so the different-pid refusal can only be
	// observed while the attempt is still launching/relaunching — catch
	// that window as early as the claim itself exists.
	var incarnationID string
	if !waitUntil(func() bool {
		state := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state FROM attempts WHERE id = '%s';", attemptID))
		if state != "launching" && state != "relaunching" {
			return false
		}
		incarnationID = querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT incarnation_id FROM launch_claims WHERE attempt_id = '%s';", attemptID))
		return incarnationID != ""
	}) {
		t.Fatalf("attempt %s never observed launching with a recorded claim before settling", attemptID)
	}
	claimBefore := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state, pid FROM launch_claims WHERE incarnation_id = '%s';", incarnationID))

	// hop launch validates its own inherited environment against the run it
	// is asked to act on (design section 6: HOP_RUN_ID/HOP_TASK_ID/
	// HOP_ATTEMPT_ID/HOP_INCARNATION_ID must agree with the loaded launch
	// context) exactly as a real worker pane would provide them, in
	// addition to the --run/--attempt flags. This second invocation is our
	// own one-shot subprocess, whose own pid necessarily differs from the
	// claim's already-recorded pid.
	env := append(append([]string{}, fx.env...),
		"HOP_RUN_ID="+fx.runID, "HOP_TASK_ID="+taskID, "HOP_ATTEMPT_ID="+attemptID, "HOP_INCARNATION_ID="+incarnationID)
	result := runHop(t, env, fx.stateDir, "launch", "--run", fx.runID, "--attempt", attemptID)
	if result.ExitCode != 1 {
		t.Fatalf("duplicate hop launch exit=%d, want 1; stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
	if !strings.Contains(result.Stderr, "different pid") {
		t.Errorf("duplicate hop launch stderr = %q, want it to name the different-pid refusal", result.Stderr)
	}

	claimAfter := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT state, pid FROM launch_claims WHERE incarnation_id = '%s';", incarnationID))
	if claimAfter != claimBefore {
		t.Errorf("launch claim changed after the rejected duplicate invocation: before=%q after=%q, want unchanged", claimBefore, claimAfter)
	}

	// The original launch is completely undisturbed: it still settles
	// normally, and no second exec attempt ever left a trace.
	fx.requireRunState(t, "running")
	claimCount := querySQLite(t, fx.dbPath(), fmt.Sprintf("SELECT count(*) FROM launch_claims WHERE attempt_id = '%s';", attemptID))
	if claimCount != "1" {
		t.Errorf("launch_claims row count for attempt %s = %s, want 1 (the rejected duplicate never wrote a second exec attempt)", attemptID, claimCount)
	}
}
