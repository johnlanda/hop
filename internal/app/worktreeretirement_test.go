package app_test

import (
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

// TestOperationKindExecClaimable pins the exact exec-claimable set over
// every operation kind: the check execution, the scratch merge and the two
// worktree-retirement executions; every act the controller performs
// itself stays unclaimable.
func TestOperationKindExecClaimable(t *testing.T) {
	want := map[app.OperationKind]bool{
		app.OpCheckRun:           true,
		app.OpIntegrationMerge:   true,
		app.OpRetirementCheck:    true,
		app.OpWorktreeRetire:     true,
		app.OpWorktreeCreate:     false,
		app.OpPaneOpen:           false,
		app.OpLaunchSend:         false,
		app.OpPaneClose:          false,
		app.OpAbsenceAttested:    false,
		app.OpIntegrationPublish: false,
		app.OpIntegrationReset:   false,
		app.OpIntegrationFence:   false,
		app.OperationKind(""):    false,
		"worktree.remove":        false,
	}
	for kind, claimable := range want {
		if got := kind.ExecClaimable(); got != claimable {
			t.Errorf("%q.ExecClaimable() = %v, want %v", kind, got, claimable)
		}
	}
	if app.OpRetirementCheck != "retirement.check" || app.OpWorktreeRetire != "worktree.retire" {
		t.Errorf("retirement kinds = %q, %q; the persisted spellings are retirement.check and worktree.retire", app.OpRetirementCheck, app.OpWorktreeRetire)
	}
}
