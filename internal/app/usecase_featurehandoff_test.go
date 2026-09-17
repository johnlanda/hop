package app_test

import (
	"context"
	"strings"
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

	t.Run("unbound: an exec-pending claim whose process lives, with no pane answering, stays in flight", func(t *testing.T) {
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
		f.tc.Groups.liveLeader(managerLaunchPID, "/usr/local/bin/claude")

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

	t.Run("unbound: an exec-pending claim whose label and process are both gone fails the run", func(t *testing.T) {
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
		f.tc.Runtime.InspectPaneFn = allPanesAbsent

		result, handle, err := f.resume("controller-2")
		if err != nil {
			t.Fatalf("ResumeFeature() error = %v", err)
		}
		mgr := sessionReport(t, &result, f.manager().ID.String())
		if mgr.Disposition != app.SessionRetiredNoProcess || mgr.Detail != launchEndedUnplacedReason {
			t.Fatalf("manager report = %+v, want %s with the label-only launch-ended reason", mgr, app.SessionRetiredNoProcess)
		}
		requireUnplacedLaunchEnded(t, f.tc, incarnation, managerLaunchPID)
		if got := f.opState(app.OpPaneOpen); got != app.OperationFailed {
			t.Fatalf("pane.open state = %s, want failed as dispatched and gone", got)
		}
		if _, bound := f.managerBinding(); bound {
			t.Fatalf("a binding appeared for a pane that no longer exists")
		}
		if result.RunState != string(run.RunLaunching) {
			t.Fatalf("resume run state = %s, want launching", result.RunState)
		}

		// The loop's launching pass reads the settled claim through the
		// resolved intent and fails the run.
		if _, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), handle); err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		f.requireManagerLaunchFailed()
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

// TestFeatureTerminalFailureResolvesIntegrationInit proves the feature
// terminal failure honors the integration.init quiescence rule the way
// stop does: the failed report waits until an unresolved init is
// resolved, and the failure path resolves it with the stop path's
// completing act. No bootstrap path launches the manager before its init
// settles, so the failing shape — a launching run whose exec-failed
// manager coexists with an unresolved init — is seeded.
func TestFeatureTerminalFailureResolvesIntegrationInit(t *testing.T) {
	seed := func(t *testing.T) (*featureStart, app.RunHandle) {
		t.Helper()
		f := newFeatureStart(t)
		f.onHeartbeat = func(n int) {
			if n == 1 {
				f.killController()
			}
		}
		f.crash()
		f.onHeartbeat = nil
		if f.opState(app.OpIntegrationInit) != app.OperationPending || len(f.git.UpdateRefCalls) != 0 {
			t.Fatalf("want the init intent pending with no act dispatched")
		}
		runID := f.runID()
		manager := f.manager()
		incarnation := identity.IncarnationID(f.tc.IDs.NewID())
		f.tc.Store.mu.Lock()
		f.tc.Store.Runs[runID].value.State = run.RunLaunching
		f.tc.Store.Sessions[manager.ID].value.State = run.SessionLaunching
		f.tc.Store.Bindings[manager.ID] = append(f.tc.Store.Bindings[manager.ID], run.NewRuntimeBinding(
			manager.ID, incarnation, "", "peer-pid:1", "workspace-m", "tab-m", "pane-m", "label-m", run.LaunchInitial, f.tc.Clock.Now()))
		f.tc.Store.mu.Unlock()
		f.recordManagerClaim(incarnation, app.LaunchClaimExecFailed)
		f.tc.Clock.Advance(leaseTTL + time.Second)
		lease, err := f.tc.Store.AcquireLease(context.Background(), runID, "controller-2")
		if err != nil {
			t.Fatalf("AcquireLease() error = %v", err)
		}
		return f, app.NewRunHandleForTest(runID, lease)
	}

	t.Run("an absent ref is completed by the same create-only CAS before the run fails", func(t *testing.T) {
		f, handle := seed(t)
		if _, err := f.tc.Controller.CorroborateSessionLaunches(context.Background(), handle); err != nil {
			t.Fatalf("CorroborateSessionLaunches() error = %v", err)
		}
		if got := f.opState(app.OpIntegrationInit); got != app.OperationSucceeded {
			t.Fatalf("integration.init state = %s, want succeeded", got)
		}
		if got := f.git.ref(featureRef); got != f.base {
			t.Fatalf("integration ref = %q, want the frozen base", got)
		}
		if n := len(f.git.UpdateRefCalls); n != 1 {
			t.Fatalf("update-ref ran %d times, want the one completing act", n)
		}
		f.requireManagerLaunchFailed()
	})

	t.Run("an unobservable ref keeps the run failing, never failed, until it can be read", func(t *testing.T) {
		f, handle := seed(t)
		f.onGit = refReadFails
		report, err := f.tc.Controller.RetireSettledSessions(context.Background(), handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() error = %v", err)
		}
		if !report.RunFailing || report.RunFailed {
			t.Fatalf("RetireSettledSessions() = %+v, want RunFailing", report)
		}
		if !strings.Contains(strings.Join(report.Outstanding, "\n"), "integration.init") {
			t.Fatalf("outstanding = %v, want the unresolved integration.init named", report.Outstanding)
		}
		if got := f.runValue().State; got != run.RunLaunching {
			t.Fatalf("run state = %s, want launching while the init is unresolved", got)
		}
		if got := f.opState(app.OpIntegrationInit); got != app.OperationReconciling {
			t.Fatalf("integration.init state = %s, want reconciling", got)
		}

		f.onGit = nil
		report, err = f.tc.Controller.RetireSettledSessions(context.Background(), handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() round 2 error = %v", err)
		}
		if !report.RunFailed {
			t.Fatalf("round 2 = %+v, want RunFailed", report)
		}
		f.requireManagerLaunchFailed()
	})

	t.Run("a colliding ref settles the init failed and the run still fails", func(t *testing.T) {
		f, handle := seed(t)
		foreign := f.git.newCommit("tree-foreign", f.base)
		f.git.setRef(featureRef, foreign)
		report, err := f.tc.Controller.RetireSettledSessions(context.Background(), handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() error = %v", err)
		}
		if !report.RunFailed {
			t.Fatalf("RetireSettledSessions() = %+v, want RunFailed", report)
		}
		if got := f.opState(app.OpIntegrationInit); got != app.OperationFailed {
			t.Fatalf("integration.init state = %s, want failed", got)
		}
		if got := f.git.ref(featureRef); got != foreign || len(f.git.UpdateRefCalls) != 0 {
			t.Fatalf("the failure path moved a colliding ref (%q, %d update-refs)", got, len(f.git.UpdateRefCalls))
		}
		f.requireManagerLaunchFailed()
	})
}

// TestFeatureTerminalFailureWaitsOnUnplacedLaunch proves the session-keyed
// unplaced-launch rule composes with the terminal failure's halting: a
// failed task's run stays failing while a child's launch is unplaced and
// unresolved, and fails once the recovered pane is observed closed.
func TestFeatureTerminalFailureWaitsOnUnplacedLaunch(t *testing.T) {
	failTask := func(u *unplacedLaunch) {
		attemptID := u.tc.Store.Sessions[u.sessionID].value.AttemptID
		taskID := u.tc.Store.Attempts[attemptID].value.TaskID
		u.tc.Store.Tasks[taskID].value.State = run.TaskFailed
	}
	retire := func(t *testing.T, u *unplacedLaunch) app.RetirementReport {
		t.Helper()
		report, err := u.tc.Controller.RetireSettledSessions(context.Background(), u.handle)
		if err != nil {
			t.Fatalf("RetireSettledSessions() error = %v", err)
		}
		return report
	}

	t.Run("found: recovered and closed before the run fails", func(t *testing.T) {
		u := unplacedWorkerWith(t, true, false)
		failTask(u)
		report := retire(t, u)
		if !report.RunFailed {
			t.Fatalf("report = %+v, want RunFailed once the recovered pane is observed closed", report)
		}
		if len(u.tc.Runtime.ClosedPanes) == 0 || u.tc.Runtime.ClosedPanes[0] != u.paneID {
			t.Fatalf("ClosedPanes = %v, want the recovered pane closed first", u.tc.Runtime.ClosedPanes)
		}
		if got := u.tc.Store.Sessions[u.sessionID].value.State; got != run.SessionTerminated {
			t.Fatalf("worker state = %s, want terminated", got)
		}
		if got := u.tc.Store.Runs[u.runID].value.State; got != run.RunFailed {
			t.Fatalf("run state = %s, want failed", got)
		}
	})

	t.Run("absent with the claimed process alive: the run stays failing, never failed", func(t *testing.T) {
		u := unplacedWorkerWith(t, true, false)
		failTask(u)
		delete(u.panes, u.label)
		u.tc.Groups.liveLeader(unplacedPID, "/usr/local/bin/claude")
		for range 3 {
			report := retire(t, u)
			if !report.RunFailing || report.RunFailed {
				t.Fatalf("report = %+v, want RunFailing while the launch is unresolved", report)
			}
			want := unplacedLiveDetail(u)
			if !strings.Contains(strings.Join(report.Outstanding, "\n"), want) {
				t.Fatalf("outstanding = %v, want %q", report.Outstanding, want)
			}
		}
		if got := u.tc.Store.Runs[u.runID].value.State; got != run.RunRunning {
			t.Fatalf("run state = %s, want running while the failure waits", got)
		}
	})

	t.Run("absent with the claimed process gone: the launch settles and the run fails", func(t *testing.T) {
		u := unplacedWorkerWith(t, true, false)
		failTask(u)
		delete(u.panes, u.label)
		report := retire(t, u)
		if !report.RunFailed {
			t.Fatalf("report = %+v, want RunFailed once the unplaced launch is observed ended", report)
		}
		requireUnplacedLaunchEnded(t, u.tc, u.incarnation, unplacedPID)
		if got := u.tc.Store.Sessions[u.sessionID].value.State; got != run.SessionTerminated {
			t.Fatalf("worker state = %s, want terminated", got)
		}
		if len(u.tc.Runtime.ClosedPanes) != 0 {
			t.Fatalf("ClosedPanes = %v, want none: no pane answers", u.tc.Runtime.ClosedPanes)
		}
	})
}
