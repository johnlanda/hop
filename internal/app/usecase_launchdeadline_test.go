package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// claimAfterTheNoClaimRead lands one launch claim after the round's own
// no-claim read and before the transaction that would flip the launch
// intent to reconciling: the launcher claims while the controller is
// looking the pane up by its creation label, the port call that sits
// between those two reads. The lookup itself finds nothing, so the round
// goes on to the deadline.
func claimAfterTheNoClaimRead(tc *testController, runID identity.RunID, sessionID identity.SessionID, attemptID identity.AttemptID, incarnationID identity.IncarnationID, pid int) {
	tc.Runtime.FindPaneByLabelFn = func(string) (app.PaneRef, bool, error) {
		tc.Store.LaunchClaims[incarnationID] = app.LaunchClaim{
			IncarnationID: incarnationID, RunID: runID, SessionID: sessionID, AttemptID: attemptID,
			Executable: "/usr/local/bin/claude", ArgvDigest: "d", PID: pid,
			State: app.LaunchClaimExecPending, ClaimedAt: tc.Clock.Now(),
		}
		return app.PaneRef{}, false, nil
	}
}

// unrecordPaneOpenOutcome returns a session's pane.open operation to the
// shape of a launch whose outcome was never recorded: pending, with no
// act evidence, and no binding.
func unrecordPaneOpenOutcome(t *testing.T, tc *testController, sessionID identity.SessionID) {
	t.Helper()
	found := false
	for id, op := range tc.Store.Operations { //nolint:gocritic // rangeValCopy: test helper over a small map.
		intent, ok := op.Intent.(map[string]any)
		if op.Kind != app.OpPaneOpen || !ok || intent["session_id"] != sessionID.String() {
			continue
		}
		op.State = app.OperationPending
		op.ActEvidence = nil
		op.Outcome = nil
		tc.Store.Operations[id] = op
		found = true
	}
	if !found {
		t.Fatalf("no pane.open operation for session %s", sessionID)
	}
	tc.Store.dropBinding(sessionID)
}

// requireLaunchIntentPending requires sessionID's newest launch intent
// still pending with no deadline outcome: the session's pre-binding
// principal authority is its newest PENDING launch intent, so a claimed
// launch must keep it.
func requireLaunchIntentPending(t *testing.T, tc *testController, sessionID identity.SessionID) {
	t.Helper()
	found := false
	for _, op := range tc.Store.Operations { //nolint:gocritic // rangeValCopy: test helper over a small map.
		intent, ok := op.Intent.(map[string]any)
		if op.Kind != app.OpPaneOpen || !ok || intent["session_id"] != sessionID.String() {
			continue
		}
		found = true
		if op.State != app.OperationPending {
			t.Fatalf("launch intent state = %s, want pending: a claim landed before the deadline transaction", op.State)
		}
		if op.Outcome != nil {
			t.Fatalf("launch intent outcome = %#v, want none", op.Outcome)
		}
	}
	if !found {
		t.Fatalf("no pane.open intent recorded for session %s", sessionID)
	}
}

// advancePastTheDeadline advances the clock past the launch-claim
// deadline while the controller keeps heartbeating on the design's
// interval, as a live loop does.
func advancePastTheDeadline(t *testing.T, tc *testController, handle app.RunHandle) { //nolint:gocritic // hugeParam: RunHandle is the app's opaque token, passed by value everywhere.
	t.Helper()
	for elapsed := time.Duration(0); elapsed < app.LaunchClaimDeadline+time.Minute; elapsed += 10 * time.Second {
		tc.Clock.Advance(10 * time.Second)
		if err := tc.Controller.Heartbeat(context.Background(), handle); err != nil {
			t.Fatalf("Heartbeat() error = %v", err)
		}
	}
}

// TestLaunchDeadlineYieldsToAClaimThatLanded proves the launch-claim
// deadline never retires a launch that claimed: the deadline transaction
// re-reads the claim, so a launcher that claimed after the round's own
// no-claim read keeps its intent — and with it the pre-binding principal
// authority its own verbs are validated against — instead of being
// refused by the very round that was meant to bound its silence.
func TestLaunchDeadlineYieldsToAClaimThatLanded(t *testing.T) {
	t.Run("feature-mode session corroboration", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
		sessionID, incarnationID := seedWorkerSession(t, tc, fr, taskID)
		attemptID := tc.Store.Sessions[sessionID].value.AttemptID
		tc.Runtime.InspectPaneFn = liveManagerOnly(fr, tc.Store.LaunchClaims[fr.ManagerIncarnation].PID)
		// The pane.open outcome was never recorded and no pane answers for
		// the creation label, so the round reaches the deadline with the
		// launch still unbound: the intent is the session's only principal
		// authority.
		unrecordPaneOpenOutcome(t, tc, sessionID)
		advancePastTheDeadline(t, tc, fr.Handle)
		claimAfterTheNoClaimRead(tc, fr.RunID, sessionID, attemptID, incarnationID, 4711)

		reports, err := tc.Controller.CorroborateSessionLaunches(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		for _, report := range reports {
			if report.SessionID == sessionID.String() && report.Progress == app.LaunchOverdue {
				t.Fatalf("session %s reported overdue although its claim landed", sessionID)
			}
		}
		requireLaunchIntentPending(t, tc, sessionID)
	})

	t.Run("solo corroboration", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		handle, detail := startedRun(t, tc)
		binding := detail.Binding
		if binding == nil {
			t.Fatalf("started run has no binding")
		}
		unrecordPaneOpenOutcome(t, tc, detail.SessionID)
		advancePastTheDeadline(t, tc, handle)
		claimAfterTheNoClaimRead(tc, detail.RunID, detail.SessionID, detail.AttemptID, binding.IncarnationID, 4242)

		progress, err := tc.Controller.CorroborateLaunch(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateLaunch() error = %v", err)
		}
		if progress == app.LaunchOverdue {
			t.Fatalf("progress = %s, want the deadline to yield to the claim that landed", progress)
		}
		requireLaunchIntentPending(t, tc, detail.SessionID)
	})
}
