package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// loseSoloBinding simulates a solo launch whose pane.open outcome was
// never recorded: the binding is gone and the operation pending again. It
// returns the operation's creation label.
func loseSoloBinding(t *testing.T, tc *testController) string {
	t.Helper()
	tc.Store.Bindings = map[identity.SessionID][]run.RuntimeBinding{}
	label := ""
	for id := range tc.Store.Operations {
		if op := tc.Store.Operations[id]; op.Kind == app.OpPaneOpen {
			op.State = app.OperationPending
			op.ActEvidence = nil
			tc.Store.Operations[id] = op
			label = id.String()
		}
	}
	if label == "" {
		t.Fatal("no pane.open operation to reopen")
	}
	return label
}

// requireFakeDetailClaim asserts the fake's LoadRunStatus names session,
// a binding when wantBinding, and the claim of incarnation (none when "").
func requireFakeDetailClaim(t *testing.T, tc *testController, runID identity.RunID, session identity.SessionID, wantBinding bool, incarnation identity.IncarnationID) {
	t.Helper()
	detail, err := tc.Store.LoadRunStatus(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRunStatus() error = %v", err)
	}
	if detail.SessionID != session {
		t.Errorf("detail session = %s, want %s", detail.SessionID, session)
	}
	if (detail.Binding != nil) != wantBinding {
		t.Errorf("detail binding = %+v, want present %t", detail.Binding, wantBinding)
	}
	switch {
	case incarnation == "" && detail.Claim != nil:
		t.Errorf("detail claim = %+v, want none", detail.Claim)
	case incarnation != "" && (detail.Claim == nil || detail.Claim.IncarnationID != incarnation):
		t.Errorf("detail claim = %+v, want incarnation %s's", detail.Claim, incarnation)
	}
}

// TestFakeRunStatusResolvesTheLaunchContextClaim holds the fake's status
// read model to the real store's (internal/adapters/sqlite
// TestLoadRunStatusResolvesTheLaunchContextClaim): the session is resolved
// within the run, and the claim is the one the session launch context
// resolves — binding, else pending intent, none on a disagreement.
func TestFakeRunStatusResolvesTheLaunchContextClaim(t *testing.T) {
	t.Run("solo: a claim written before the binding", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, started := startedRun(t, tc)
		claimLaunch(t, tc, started, 4242)
		loseSoloBinding(t, tc)
		requireFakeDetailClaim(t, tc, started.RunID, started.SessionID, false, started.Binding.IncarnationID)
	})

	t.Run("solo: a binding and a differing pending intent surface no claim", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		_, started := startedRun(t, tc)
		claimLaunch(t, tc, started, 4242)
		requireFakeDetailClaim(t, tc, started.RunID, started.SessionID, true, started.Binding.IncarnationID)
		seedPendingLaunchIntent(t, tc, started.RunID, started.SessionID, identity.IncarnationID(tc.IDs.NewID()))
		requireFakeDetailClaim(t, tc, started.RunID, started.SessionID, true, "")
	})

	t.Run("feature manager: a claim written before the binding", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		delete(tc.Store.Bindings, fr.ManagerID)
		seedPendingLaunchIntent(t, tc, fr.RunID, fr.ManagerID, fr.ManagerIncarnation)
		tc.Store.LaunchClaims[fr.ManagerIncarnation] = app.LaunchClaim{
			IncarnationID: fr.ManagerIncarnation, RunID: fr.RunID, SessionID: fr.ManagerID,
			Executable: "/usr/local/bin/claude", PID: 900, State: app.LaunchClaimExecPending, ClaimedAt: tc.Clock.Now(),
		}
		requireFakeDetailClaim(t, tc, fr.RunID, fr.ManagerID, false, fr.ManagerIncarnation)
	})

	t.Run("feature manager: a binding and a differing pending intent surface no claim", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		tc.Store.LaunchClaims[fr.ManagerIncarnation] = app.LaunchClaim{
			IncarnationID: fr.ManagerIncarnation, RunID: fr.RunID, SessionID: fr.ManagerID,
			Executable: "/usr/local/bin/claude", PID: 900, State: app.LaunchClaimExeced, ClaimedAt: tc.Clock.Now(),
		}
		requireFakeDetailClaim(t, tc, fr.RunID, fr.ManagerID, true, fr.ManagerIncarnation)
		seedPendingLaunchIntent(t, tc, fr.RunID, fr.ManagerID, identity.IncarnationID(tc.IDs.NewID()))
		requireFakeDetailClaim(t, tc, fr.RunID, fr.ManagerID, true, "")
	})

	t.Run("feature: each run names its own manager", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		first := seedFeatureRun(t, tc, 2)
		second := seedFeatureRun(t, tc, 2)
		// The first manager has no placement: its run must still name it,
		// never the second run's bound manager.
		delete(tc.Store.Bindings, first.ManagerID)
		requireFakeDetailClaim(t, tc, first.RunID, first.ManagerID, false, "")
		requireFakeDetailClaim(t, tc, second.RunID, second.ManagerID, true, "")
	})
}

// TestSoloCorroborationReadsAPreBindingClaim proves the solo launch
// corroboration decides by claim state before the binding is recorded: a
// pre-binding exec_failed claim fails the run, and a pre-binding
// exec_pending claim is never mistaken for a missing one by the
// launch-claim deadline.
func TestSoloCorroborationReadsAPreBindingClaim(t *testing.T) {
	t.Run("an exec_failed claim before the binding fails the run", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, started := startedRun(t, tc)
		claimLaunch(t, tc, started, 4242)
		loseSoloBinding(t, tc)
		claim := tc.Store.LaunchClaims[started.Binding.IncarnationID]
		claim.State = app.LaunchClaimExecFailed
		claim.Error = "exec: no such file or directory"
		tc.Store.LaunchClaims[started.Binding.IncarnationID] = claim

		progress, err := tc.Controller.CorroborateLaunch(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateLaunch() error = %v", err)
		}
		if progress != app.LaunchFailed {
			t.Fatalf("progress = %s, want %s", progress, app.LaunchFailed)
		}
		if got := tc.Store.Attempts[started.AttemptID].value.State; got != run.AttemptFailed {
			t.Errorf("attempt state = %s, want failed", got)
		}
		if got := tc.Store.Runs[started.RunID].value.State; got != run.RunFailed {
			t.Errorf("run state = %s, want failed", got)
		}
		if got := tc.Store.Sessions[started.SessionID].value.State; got != run.SessionTerminated {
			t.Errorf("session state = %s, want terminated", got)
		}
	})

	t.Run("an exec_pending claim before the binding is never overdue", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, started := startedRun(t, tc)
		claimLaunch(t, tc, started, 4242)
		label := loseSoloBinding(t, tc)
		// No pane answers for the label yet: the binding stays unrecovered.
		for elapsed := time.Duration(0); elapsed < 3*time.Minute; elapsed += 10 * time.Second {
			tc.Clock.Advance(10 * time.Second)
			if err := tc.Controller.Heartbeat(context.Background(), handle); err != nil {
				t.Fatalf("Heartbeat() error = %v", err)
			}
		}

		progress, err := tc.Controller.CorroborateLaunch(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateLaunch() error = %v", err)
		}
		if progress != app.LaunchPending {
			t.Fatalf("progress = %s, want %s: a claim exists, so the claim deadline never applies", progress, app.LaunchPending)
		}
		op := tc.Store.Operations[identity.OperationID(label)]
		if op.State != app.OperationPending {
			t.Errorf("pane.open operation state = %s, want still pending", op.State)
		}
	})
}
