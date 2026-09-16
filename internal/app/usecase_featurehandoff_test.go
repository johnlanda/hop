package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
	"github.com/johnlanda/hop/internal/domain/run"
)

// managerLaunchPID is the pid every bootstrap manager claim records.
const managerLaunchPID = 6262

// managerLaunchFailure is the run transition reason of the manager-lineage
// exec-failure cause.
const managerLaunchFailure = "manager launch exec failed (exec_failed claim; no process) with owned-work termination observed"

// recordManagerClaim records the launch claim the bootstrap manager's
// launcher would have written for incarnation, in state.
func (f *featureStart) recordManagerClaim(incarnation identity.IncarnationID, state app.LaunchClaimState) {
	f.t.Helper()
	manager := f.manager()
	f.tc.Store.mu.Lock()
	defer f.tc.Store.mu.Unlock()
	f.tc.Store.LaunchClaims[incarnation] = app.LaunchClaim{
		IncarnationID: incarnation, RunID: manager.RunID, SessionID: manager.ID,
		Executable: "/usr/local/bin/claude", ArgvDigest: "d", PID: managerLaunchPID,
		State: state, ClaimedAt: f.tc.Clock.Now(),
	}
}

// managerOccupant answers the manager's bound pane with the claimed harness
// process carrying its incarnation marker, and every other pane as absent.
func (f *featureStart) managerOccupant(binding *run.RuntimeBinding) {
	f.tc.Runtime.InspectPaneFn = func(paneID string) (app.PaneProcess, error) {
		if paneID != binding.PaneID {
			return app.PaneProcess{}, app.ErrPaneNotFound
		}
		return app.PaneProcess{
			ShellPID: 1, ForegroundGroupID: managerLaunchPID,
			Foreground: []app.ProcessInfo{{PID: managerLaunchPID, Argv: []string{"/usr/local/bin/claude"}, Cmdline: "claude " + binding.IncarnationID.String()}},
		}, nil
	}
}

// boundManagerIncarnation returns the incarnation of the manager's
// committed binding, failing when there is none.
func (f *featureStart) boundManagerIncarnation() run.RuntimeBinding {
	f.t.Helper()
	binding, bound := f.managerBinding()
	if !bound || binding.PaneID == "" {
		f.t.Fatalf("the manager has no committed binding")
	}
	return binding
}

// requireManagerLaunchFailed proves the run failed through the
// manager-lineage disposition: the manager terminated and the run failed
// with the manager-launch cause as the transition reason.
func (f *featureStart) requireManagerLaunchFailed() {
	f.t.Helper()
	if got := f.manager().State; got != run.SessionTerminated {
		f.t.Errorf("manager state = %s, want terminated", got)
	}
	if got := f.runValue().State; got != run.RunFailed {
		f.t.Fatalf("run state = %s, want failed", got)
	}
	if reason, ok := transitionReason(f.tc, app.EntityRun, f.runID().String(), string(run.RunFailed)); !ok || reason != managerLaunchFailure {
		f.t.Errorf("run failure reason = %q (found %t), want %q", reason, ok, managerLaunchFailure)
	}
}

// TestStartFeatureRunHandsOffToLaunchCorroboration proves the launching
// hand-off between the bootstrap and the loop: StartFeatureRun leaves the
// run launching with the manager's pane open, and the loop's launching-only
// pass (runFeatureSchedulingPass calls CorroborateSessionLaunches alone
// while the run is launching) settles it — the manager's settled claim
// moves the run to running; its exec failure fails the run through the
// manager-lineage disposition.
func TestStartFeatureRunHandsOffToLaunchCorroboration(t *testing.T) {
	t.Run("a settled manager claim moves the launching run to running", func(t *testing.T) {
		f := newFeatureStart(t)
		_, handle := f.mustStart()
		binding := f.boundManagerIncarnation()
		f.recordManagerClaim(binding.IncarnationID, app.LaunchClaimExecPending)
		f.managerOccupant(&binding)

		reports, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		if len(reports) != 1 || reports[0].Role != string(run.RoleManager) || reports[0].Progress != app.LaunchSettled {
			t.Fatalf("reports = %+v, want the manager settled", reports)
		}
		if got := f.runValue().State; got != run.RunRunning {
			t.Fatalf("run state = %s, want running", got)
		}
		if got := f.manager().State; got != run.SessionActive {
			t.Errorf("manager state = %s, want active", got)
		}
		if reason, ok := transitionReason(f.tc, app.EntityRun, f.runID().String(), string(run.RunRunning)); !ok || reason != "manager launch claim settled execed" {
			t.Errorf("run running reason = %q (found %t)", reason, ok)
		}
		if claim := f.tc.Store.LaunchClaims[binding.IncarnationID]; claim.State != app.LaunchClaimExeced {
			t.Errorf("manager claim state = %s, want execed", claim.State)
		}
	})

	t.Run("a still-unsettled claim leaves the run launching", func(t *testing.T) {
		f := newFeatureStart(t)
		_, handle := f.mustStart()
		binding := f.boundManagerIncarnation()
		f.recordManagerClaim(binding.IncarnationID, app.LaunchClaimExecPending)
		f.tc.Runtime.InspectPaneFn = func(string) (app.PaneProcess, error) { return app.PaneProcess{ShellPID: 1}, nil }

		reports, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		if len(reports) != 1 || reports[0].Progress != app.LaunchPending {
			t.Fatalf("reports = %+v, want the manager pending", reports)
		}
		if got := f.runValue().State; got != run.RunLaunching {
			t.Fatalf("run state = %s, want launching", got)
		}
	})

	t.Run("an exec-failed manager claim fails the launching run with the manager-launch cause", func(t *testing.T) {
		f := newFeatureStart(t)
		_, handle := f.mustStart()
		binding := f.boundManagerIncarnation()
		f.recordManagerClaim(binding.IncarnationID, app.LaunchClaimExecFailed)

		reports, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		if len(reports) != 1 || reports[0].Role != string(run.RoleManager) || reports[0].Progress != app.LaunchFailed {
			t.Fatalf("reports = %+v, want the manager failed", reports)
		}
		f.requireManagerLaunchFailed()
	})

	t.Run("the pass's assignment defaults are the roots the bootstrap froze", func(t *testing.T) {
		f := newFeatureStart(t)
		_, handle := f.mustStart()
		binding := f.boundManagerIncarnation()
		f.recordManagerClaim(binding.IncarnationID, app.LaunchClaimExecPending)
		f.managerOccupant(&binding)
		if _, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), handle); err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}

		// The running pass's own assembly (runFeatureSchedulingPass): the
		// frozen defaults, the running binary, the live integration head.
		opts, err := f.tc.Controller.AssignmentDefaults(context.Background(), handle)
		if err != nil {
			t.Fatalf("AssignmentDefaults() error = %v", err)
		}
		if opts.RepositoryRoot != featureRequest().RepositoryRoot || opts.StateRoot != featureRequest().StateRoot {
			t.Fatalf("AssignmentDefaults() roots = %q, %q; want the request's %q, %q", opts.RepositoryRoot, opts.StateRoot, featureRequest().RepositoryRoot, featureRequest().StateRoot)
		}
		opts.HOPPath = featureHOP
		if opts.IntegrationHeadCommitOID, err = f.tc.Controller.ResolveIntegrationHead(context.Background(), handle); err != nil {
			t.Fatalf("ResolveIntegrationHead() error = %v", err)
		}
		if opts.IntegrationHeadCommitOID != f.base {
			t.Fatalf("integration head = %q, want the frozen base", opts.IntegrationHeadCommitOID)
		}
		if _, err := f.tc.Controller.AssignReadyTasks(context.Background(), handle, opts); err != nil {
			t.Fatalf("AssignReadyTasks() refused the frozen defaults: %v", err)
		}
	})
}

// TestResumeFeatureBootstrapManagerExecFailure proves an interrupted
// bootstrap whose manager launch exec failed is never left launching: the
// resume continuation hands the exec-failed manager to reconciliation, and
// the loop's next pass fails the run through the manager-lineage
// disposition — whether the pane.open outcome (and so the binding) was
// recorded or lost with the pane gone.
func TestResumeFeatureBootstrapManagerExecFailure(t *testing.T) {
	t.Run("bound: resume retires the manager and the running pass fails the run", func(t *testing.T) {
		f := newFeatureStart(t)
		f.mustStart()
		f.killController()
		binding := f.boundManagerIncarnation()
		f.recordManagerClaim(binding.IncarnationID, app.LaunchClaimExecFailed)

		result, handle, err := f.resume("controller-2")
		if err != nil {
			t.Fatalf("ResumeFeature() error = %v", err)
		}
		requireManagerRetiredOnResume(t, f, &result)

		report, err := f.tc.Controller.RetireSettledSessions(context.Background(), handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() error = %v", err)
		}
		if !report.RunFailed {
			t.Fatalf("RetireSettledSessions() = %+v, want RunFailed", report)
		}
		f.requireManagerLaunchFailed()
	})

	t.Run("unbound: the pane.open outcome was lost and the pane is gone", func(t *testing.T) {
		f := unboundExecFailedManager(t)

		result, handle, err := f.resume("controller-2")
		if err != nil {
			t.Fatalf("ResumeFeature() error = %v", err)
		}
		requireManagerRetiredOnResume(t, f, &result)
		if _, bound := f.managerBinding(); bound {
			t.Fatalf("a binding appeared for a pane that no longer exists")
		}

		report, err := f.tc.Controller.RetireSettledSessions(context.Background(), handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() error = %v", err)
		}
		if !report.RunFailed {
			t.Fatalf("RetireSettledSessions() = %+v, want RunFailed", report)
		}
		f.requireManagerLaunchFailed()
	})

	t.Run("unbound: the exec fails after resume returned the run to launching", func(t *testing.T) {
		f := newFeatureStart(t)
		f.onOpenPane = func(req app.WorkerPaneRequest) (app.PaneHandle, bool, error) {
			handle := f.openPaneLocked(&req)
			f.killController()
			return handle, true, nil
		}
		f.crash()
		f.onOpenPane = nil
		// The resume round sees the launch in flight; the pane is still there
		// but its label lookup is not answering yet.
		label, incarnation := pendingPaneIntent(t, f.tc, f.manager().ID)
		delete(f.createdPanes, label)
		result, handle, err := f.resume("controller-2")
		if err != nil {
			t.Fatalf("ResumeFeature() error = %v", err)
		}
		if result.Outcome != "resumed" || result.RunState != string(run.RunLaunching) {
			t.Fatalf("ResumeFeature() = %+v, want resumed with the run launching", result)
		}

		// The launcher then claims, fails its exec and exits: the pane closes.
		f.recordManagerClaim(incarnation, app.LaunchClaimExecFailed)
		f.tc.Runtime.InspectPaneFn = allPanesAbsent

		reports, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		if len(reports) != 1 || reports[0].Progress != app.LaunchFailed {
			t.Fatalf("reports = %+v, want the manager failed", reports)
		}
		f.requireManagerLaunchFailed()
	})

	t.Run("unbound: an exec-pending claim with no pane answering stays in flight", func(t *testing.T) {
		f := newFeatureStart(t)
		f.onOpenPane = func(req app.WorkerPaneRequest) (app.PaneHandle, bool, error) {
			handle := f.openPaneLocked(&req)
			f.killController()
			return handle, true, nil
		}
		f.crash()
		f.onOpenPane = nil
		label, incarnation := pendingPaneIntent(t, f.tc, f.manager().ID)
		delete(f.createdPanes, label)
		f.recordManagerClaim(incarnation, app.LaunchClaimExecPending)

		result, handle, err := f.resume("controller-2")
		if err != nil {
			t.Fatalf("ResumeFeature() error = %v", err)
		}
		if result.Outcome != "resumed" || result.RunState != string(run.RunLaunching) {
			t.Fatalf("ResumeFeature() = %+v, want resumed with the run launching", result)
		}
		// Heartbeat across the launch-claim deadline so the lease stays held.
		for range 6 {
			f.tc.Clock.Advance(25 * time.Second)
			if hbErr := f.tc.Controller.Heartbeat(context.Background(), handle); hbErr != nil {
				t.Fatalf("Heartbeat() error = %v", hbErr)
			}
		}
		reports, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), handle)
		if err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		// A claim exists, so the launch-claim deadline no longer applies:
		// human interaction time after a claim is unbounded.
		if len(reports) != 1 || reports[0].Progress != app.LaunchPending {
			t.Fatalf("reports = %+v, want the manager pending", reports)
		}
		if got := f.runValue().State; got != run.RunLaunching {
			t.Fatalf("run state = %s, want launching", got)
		}
		if got := f.opState(app.OpPaneOpen); got != app.OperationPending {
			t.Fatalf("pane.open state = %s, want still pending, never re-sent", got)
		}
	})
}

// unboundExecFailedManager is a bootstrap whose controller died after the
// manager's pane.open act and before its outcome, whose launcher then
// claimed, failed its exec and exited, closing the pane.
func unboundExecFailedManager(t *testing.T) *featureStart {
	t.Helper()
	f := newFeatureStart(t)
	f.onOpenPane = func(req app.WorkerPaneRequest) (app.PaneHandle, bool, error) {
		handle := f.openPaneLocked(&req)
		f.killController()
		return handle, true, nil
	}
	f.crash()
	f.onOpenPane = nil
	label, incarnation := pendingPaneIntent(t, f.tc, f.manager().ID)
	f.recordManagerClaim(incarnation, app.LaunchClaimExecFailed)
	delete(f.createdPanes, label)
	f.tc.Runtime.InspectPaneFn = allPanesAbsent
	return f
}

// requireManagerRetiredOnResume proves a resume round retired the
// exec-failed manager with no process and handed the run on.
func requireManagerRetiredOnResume(t *testing.T, f *featureStart, result *app.ResumeFeatureResult) {
	t.Helper()
	if result.Outcome != "resumed" || len(result.Blocked) != 0 {
		t.Fatalf("ResumeFeature() = %+v, want resumed", result)
	}
	if report := sessionReport(t, result, f.manager().ID.String()); report.Disposition != app.SessionRetiredNoProcess {
		t.Fatalf("manager disposition = %s, want %s", report.Disposition, app.SessionRetiredNoProcess)
	}
	if got := f.manager().State; got != run.SessionTerminated {
		t.Fatalf("manager state = %s, want terminated", got)
	}
	if n := len(f.paneCalls); n != 1 {
		t.Fatalf("pane.open ran %d times, want the one bootstrap act", n)
	}
}
