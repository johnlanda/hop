package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/run"
)

// refReadFails makes every `rev-parse --verify <integration ref>` a runner
// failure: the ref cannot be observed at all.
func refReadFails(cmd app.Command) (app.CommandResult, bool, error) {
	argv := strings.Join(cmd.Argv, " ")
	if strings.Contains(argv, "rev-parse --verify refs/heads/") {
		return app.CommandResult{}, true, errors.New("git could not be spawned")
	}
	return app.CommandResult{}, false, nil
}

// crash runs StartFeatureRun through a scripted controller death and
// requires it to fail.
func (f *featureStart) crash() {
	f.t.Helper()
	if _, _, err := f.start(); err == nil {
		f.t.Fatalf("StartFeatureRun() succeeded despite the scripted crash")
	}
}

// assertResumedLaunching proves a continued bootstrap handed the run to
// the loop: resumed, the run launching with the manager's launch in
// flight, and exactly one of each act across the whole history.
func assertResumedLaunching(t *testing.T, f *featureStart, result *app.ResumeFeatureResult, wantUpdateRefs int) {
	t.Helper()
	if result.Outcome != "resumed" || result.RunState != string(run.RunLaunching) || len(result.Blocked) != 0 {
		t.Fatalf("ResumeFeature() = %+v, want resumed with the run launching", result)
	}
	if f.runValue().State != run.RunLaunching || f.manager().State != run.SessionLaunching {
		t.Fatalf("run %s, manager %s; want launching and launching", f.runValue().State, f.manager().State)
	}
	if got := f.git.ref(featureRef); got != f.base {
		t.Fatalf("integration ref = %q, want the frozen base", got)
	}
	if n := len(f.git.UpdateRefCalls); n != wantUpdateRefs {
		t.Fatalf("update-ref ran %d times, want %d", n, wantUpdateRefs)
	}
	if len(f.workspaceCalls) != 1 || len(f.paneCalls) != 1 {
		t.Fatalf("workspace.create ran %d times, pane.open %d; want exactly one each, never a second create", len(f.workspaceCalls), len(f.paneCalls))
	}
	if f.opState(app.OpIntegrationInit) != app.OperationSucceeded || f.opState(app.OpWorkspaceCreate) != app.OperationSucceeded {
		t.Fatalf("integration.init %s, workspace.create %s; want both succeeded", f.opState(app.OpIntegrationInit), f.opState(app.OpWorkspaceCreate))
	}
	report := sessionReport(t, result, f.manager().ID.String())
	if report.Role != string(run.RoleManager) || report.Disposition != app.SessionPending {
		t.Fatalf("manager report = %+v, want pending under the unchanged per-session predicate", report)
	}
}

// assertResumeBlocked proves a continuation round stopped at a recorded
// outcome: reconciling with the blocking detail named, nothing placed.
func assertResumeBlocked(t *testing.T, result *app.ResumeFeatureResult, detail string) {
	t.Helper()
	if result.Outcome != "reconciling" || len(result.Blocked) == 0 || !strings.Contains(strings.Join(result.Blocked, "\n"), detail) {
		t.Fatalf("ResumeFeature() = %+v, want reconciling blocked on %q", result, detail)
	}
}

// TestResumeFeatureBootstrapContinuation drives hop resume's feature
// startup continuation through every bootstrap crash point
// (docs/plan/phase-3-design.md sections 4 and 9): integration.init per its
// decision row, workspace.create per the design row (never a second
// create), and the manager's pane.open through the session-keyed label
// recovery.
func TestResumeFeatureBootstrapContinuation(t *testing.T) {
	t.Run("integration.init", func(t *testing.T) {
		t.Run("crash after InitializeRun, before the intent: a fresh intent, the manager assignment recreated", func(t *testing.T) {
			f := newFeatureStart(t)
			f.store.after = func(err error) {
				if err == nil {
					f.killController()
				}
			}
			if _, _, err := f.start(); !errors.Is(err, app.ErrFenced) {
				t.Fatalf("StartFeatureRun() error = %v, want ErrFenced after the controller died", err)
			}
			if len(f.ops(app.OpIntegrationInit)) != 0 {
				t.Fatalf("an integration.init intent was journaled before the crash")
			}
			snapshot := f.tc.Store.Snapshots[f.runID()]
			delete(f.tc.Artifacts.files, snapshot.AssignmentPath)

			result := f.mustResume("controller-2")
			assertResumedLaunching(t, f, &result, 1)
			content, ok := f.tc.Artifacts.files[snapshot.AssignmentPath]
			if !ok || digestOf(content) != snapshot.AssignmentDigest {
				t.Fatalf("the manager assignment was not recreated at its frozen digest")
			}
			recorded := 0
			for _, a := range f.tc.Store.Artifacts {
				if a.Kind == run.ArtifactAssignment && a.Path == snapshot.AssignmentPath {
					recorded++
				}
			}
			if recorded != 1 {
				t.Fatalf("assignment artifact rows = %d, want 1 (recorded by the recreation)", recorded)
			}
		})

		t.Run("crash before the intent, a ref present at the base: foreign, failed, nothing placed", func(t *testing.T) {
			f := newFeatureStart(t)
			f.store.after = func(err error) {
				if err == nil {
					f.killController()
				}
			}
			if _, _, err := f.start(); err == nil {
				t.Fatalf("StartFeatureRun() succeeded despite the dead controller")
			}
			f.git.setRef(featureRef, f.base)

			result := f.mustResume("controller-2")
			assertResumeBlocked(t, &result, "inspect it before removing or renaming it")
			if state := f.opState(app.OpIntegrationInit); state != app.OperationFailed {
				t.Fatalf("integration.init state = %s, want failed: no intent existed, so the ref is foreign even at the base", state)
			}
			if len(f.workspaceCalls) != 0 || f.manager().State != run.SessionReserved {
				t.Fatalf("the continuation placed the manager past a collision")
			}
		})

		t.Run("crash after the intent, before the act: the same operation re-acts", func(t *testing.T) {
			f := newFeatureStart(t)
			f.onHeartbeat = func(n int) {
				if n == 1 {
					f.killController()
				}
			}
			if _, _, err := f.start(); !errors.Is(err, app.ErrFenced) {
				t.Fatalf("StartFeatureRun() error = %v, want ErrFenced", err)
			}
			if len(f.git.UpdateRefCalls) != 0 || f.opState(app.OpIntegrationInit) != app.OperationPending {
				t.Fatalf("the act ran or the intent is not pending")
			}
			f.onHeartbeat = nil
			result := f.mustResume("controller-2")
			assertResumedLaunching(t, f, &result, 1)
			if n := len(f.ops(app.OpIntegrationInit)); n != 1 {
				t.Fatalf("integration.init operations = %d, want the one recovered intent", n)
			}
		})

		t.Run("takeover with the intent pending: a ref at the base is adopted, another value fails", func(t *testing.T) {
			for _, value := range []string{"base", "other"} {
				t.Run(value, func(t *testing.T) {
					f := newFeatureStart(t)
					f.onHeartbeat = func(n int) {
						if n != 1 {
							return
						}
						// B takes the lease over at A's pre-dispatch revalidation.
						f.tc.Clock.Advance(leaseTTL + time.Second)
						if _, err := f.tc.Store.AcquireLease(context.Background(), f.runID(), "controller-B"); err != nil {
							t.Errorf("AcquireLease(B) error = %v", err)
						}
					}
					if _, _, err := f.start(); !errors.Is(err, app.ErrFenced) {
						t.Fatalf("StartFeatureRun() error = %v, want ErrFenced", err)
					}
					f.onHeartbeat = nil
					existing := f.base
					if value == "other" {
						existing = f.git.newCommit("tree-foreign", f.base)
					}
					f.git.setRef(featureRef, existing)

					result := f.mustResume("controller-C")
					if value == "base" {
						assertResumedLaunching(t, f, &result, 0)
						return
					}
					assertResumeBlocked(t, &result, "inspect it before removing or renaming it")
					if state := f.opState(app.OpIntegrationInit); state != app.OperationFailed {
						t.Fatalf("integration.init state = %s, want failed", state)
					}
					if len(f.git.UpdateRefCalls) != 0 || f.git.ref(featureRef) != existing {
						t.Fatalf("recovery acted on a colliding ref")
					}
				})
			}
		})

		t.Run("crash after the act, before its outcome: the created ref is adopted, never re-created", func(t *testing.T) {
			f := newFeatureStart(t)
			f.onUpdateRef = func(cmd app.Command) (app.CommandResult, bool, error) {
				result, _, err := f.git.Hook(context.Background(), cmd)
				f.killController()
				return result, true, err
			}
			if _, _, err := f.start(); !errors.Is(err, app.ErrFenced) {
				t.Fatalf("StartFeatureRun() error = %v, want ErrFenced on the outcome commit", err)
			}
			if f.opState(app.OpIntegrationInit) != app.OperationPending || f.git.ref(featureRef) != f.base {
				t.Fatalf("want the CAS landed with the intent still pending")
			}
			f.onUpdateRef = nil
			result := f.mustResume("controller-2")
			assertResumedLaunching(t, f, &result, 1)
		})

		t.Run("takeover with the intent reconciling: the ref not observed re-acts", func(t *testing.T) {
			f := newFeatureStart(t)
			f.onUpdateRef = func(app.Command) (app.CommandResult, bool, error) {
				return app.CommandResult{}, true, errors.New("git could not be spawned")
			}
			if _, _, err := f.start(); err == nil {
				t.Fatalf("StartFeatureRun() succeeded despite the runner failure")
			}
			if f.opState(app.OpIntegrationInit) != app.OperationReconciling {
				t.Fatalf("integration.init state = %s, want reconciling", f.opState(app.OpIntegrationInit))
			}
			f.onUpdateRef = nil
			result := f.mustResume("controller-2")
			// The failed spawn never reached the fake ref store, so the one
			// recorded update-ref is the recovery's re-act.
			assertResumedLaunching(t, f, &result, 1)
		})

		t.Run("a re-act refused while the ref is still not observed stays reconciling", func(t *testing.T) {
			f := newFeatureStart(t)
			f.onHeartbeat = func(n int) {
				if n == 1 {
					f.killController()
				}
			}
			f.crash()
			f.onHeartbeat = nil
			f.onUpdateRef = func(app.Command) (app.CommandResult, bool, error) {
				return app.CommandResult{ExitCode: 128, Stderr: []byte("fatal: cannot lock ref")}, true, nil
			}
			result := f.mustResume("controller-2")
			assertResumeBlocked(t, &result, "unresolved")
			if state := f.opState(app.OpIntegrationInit); state != app.OperationReconciling {
				t.Fatalf("integration.init state = %s, want reconciling", state)
			}
		})

		t.Run("a re-act refused because the dead controller's dispatch landed first adopts it", func(t *testing.T) {
			f := newFeatureStart(t)
			f.onHeartbeat = func(n int) {
				if n == 1 {
					f.killController()
				}
			}
			f.crash()
			f.onHeartbeat = nil
			f.onUpdateRef = func(app.Command) (app.CommandResult, bool, error) {
				f.git.setRef(featureRef, f.base)
				return app.CommandResult{ExitCode: 128, Stderr: []byte("fatal: ref exists")}, true, nil
			}
			result := f.mustResume("controller-2")
			assertResumedLaunching(t, f, &result, 0)
		})

		t.Run("a read error keeps the intent reconciling until the ref can be observed", func(t *testing.T) {
			f := newFeatureStart(t)
			f.onHeartbeat = func(n int) {
				if n == 1 {
					f.killController()
				}
			}
			f.crash()
			f.onHeartbeat = nil
			f.onGit = refReadFails

			result := f.mustResume("controller-2")
			assertResumeBlocked(t, &result, "could not be observed")
			if state := f.opState(app.OpIntegrationInit); state != app.OperationReconciling {
				t.Fatalf("integration.init state = %s, want reconciling: a read error is never absence", state)
			}
			if len(f.git.UpdateRefCalls) != 0 || len(f.workspaceCalls) != 0 {
				t.Fatalf("recovery acted without observing the ref")
			}

			f.onGit = nil
			result = f.mustResume("controller-3")
			assertResumedLaunching(t, f, &result, 1)
		})

		t.Run("a failed init is re-driven by a fresh operation once the ref is gone", func(t *testing.T) {
			f := newFeatureStart(t)
			foreign := f.git.newCommit("tree-foreign", f.base)
			f.onHeartbeat = func(n int) {
				if n == 1 {
					f.git.setRef(featureRef, foreign)
				}
			}
			if _, _, err := f.start(); !errors.Is(err, app.ErrIntegrationBranchExists) {
				t.Fatalf("StartFeatureRun() error = %v, want the collision", err)
			}
			f.onHeartbeat = nil

			result := f.mustResume("controller-2")
			assertResumeBlocked(t, &result, "inspect it before removing or renaming it")
			if n := len(f.ops(app.OpIntegrationInit)); n != 2 {
				t.Fatalf("integration.init operations = %d, want the failed one and a fresh failed retry", n)
			}

			// The human inspected the ref and removed it.
			f.git.setRef(featureRef, "")
			result = f.mustResume("controller-3")
			if result.Outcome != "resumed" || result.RunState != string(run.RunLaunching) {
				t.Fatalf("ResumeFeature() = %+v, want resumed", result)
			}
			succeeded := 0
			for _, op := range f.ops(app.OpIntegrationInit) {
				if op.State == app.OperationSucceeded {
					succeeded++
				}
			}
			if succeeded != 1 || f.git.ref(featureRef) != f.base {
				t.Fatalf("succeeded integration.init operations = %d, ref %q; want one, at the base", succeeded, f.git.ref(featureRef))
			}
		})
	})

	t.Run("workspace.create", func(t *testing.T) {
		t.Run("crash after the intent, before the act: a bounded wait, then reconciling, never a create", func(t *testing.T) {
			f := newFeatureStart(t)
			f.onHeartbeat = func(n int) {
				if n == 2 {
					f.killController()
				}
			}
			if _, _, err := f.start(); !errors.Is(err, app.ErrFenced) {
				t.Fatalf("StartFeatureRun() error = %v, want ErrFenced", err)
			}
			f.onHeartbeat = nil

			result := f.mustResume("controller-2")
			assertResumeBlocked(t, &result, "may still be in flight")
			op := f.onlyOp(app.OpWorkspaceCreate)
			var wait struct {
				WaitingSince string `json:"waiting_since"`
				Deadline     string `json:"deadline"`
			}
			decodeInto(t, op.ActEvidence, &wait)
			if op.State != app.OperationPending || wait.WaitingSince == "" || wait.Deadline == "" {
				t.Fatalf("workspace.create = %s with evidence %+v, want pending with the bounded wait persisted", op.State, wait)
			}

			f.tc.Clock.Advance(2 * time.Minute)
			result = f.mustResume("controller-3")
			assertResumeBlocked(t, &result, "never re-created")
			if state := f.opState(app.OpWorkspaceCreate); state != app.OperationReconciling {
				t.Fatalf("workspace.create state = %s, want reconciling past the bounded wait", state)
			}
			if len(f.workspaceCalls) != 0 || len(f.paneCalls) != 0 || f.manager().State != run.SessionReserved {
				t.Fatalf("a workspace or pane was created for an unresolved placement")
			}
		})

		t.Run("crash after the act, before its outcome: adopted by label, the pane opens there", func(t *testing.T) {
			f := newFeatureStart(t)
			f.onCreateWorkspace = func(req app.WorkspaceRequest) (app.WorkspaceHandle, bool, error) {
				handle := f.createWorkspaceLocked(req)
				f.killController()
				return handle, true, nil
			}
			if _, _, err := f.start(); !errors.Is(err, app.ErrFenced) {
				t.Fatalf("StartFeatureRun() error = %v, want ErrFenced on the outcome commit", err)
			}
			f.onCreateWorkspace = nil
			result := f.mustResume("controller-2")
			assertResumedLaunching(t, f, &result, 1)
			var placed app.WorkspaceHandle
			decodeInto(t, f.onlyOp(app.OpWorkspaceCreate).ActEvidence, &placed)
			if f.paneCalls[0].WorkspaceID != placed.WorkspaceID || f.paneCalls[0].Cwd != featureRepo {
				t.Fatalf("the manager pane opened in %s at %s, want the adopted workspace %s at the repository root", f.paneCalls[0].WorkspaceID, f.paneCalls[0].Cwd, placed.WorkspaceID)
			}
		})

		t.Run("an ambiguous label lookup stays reconciling", func(t *testing.T) {
			f := newFeatureStart(t)
			f.onHeartbeat = func(n int) {
				if n == 2 {
					f.killController()
				}
			}
			f.crash()
			f.onHeartbeat = nil
			f.tc.Runtime.FindWorkspaceByLabelFn = func(string) (app.WorkspaceRef, bool, error) {
				return app.WorkspaceRef{}, false, errors.New("2 workspaces carry this label, want at most one")
			}
			result := f.mustResume("controller-2")
			assertResumeBlocked(t, &result, "failed or was ambiguous")
			if state := f.opState(app.OpWorkspaceCreate); state != app.OperationReconciling {
				t.Fatalf("workspace.create state = %s, want reconciling", state)
			}
			if len(f.workspaceCalls) != 0 || len(f.paneCalls) != 0 {
				t.Fatalf("an ambiguous placement created or launched something")
			}
		})

		t.Run("crash between the placement's outcome and the pane intent: the recorded workspace is used", func(t *testing.T) {
			f := newFeatureStart(t)
			killed := false
			f.tc.Store.CommitHook = func(u *fakeUnitOfWork) {
				for _, op := range u.opCreated {
					if op.Kind == app.OpPaneOpen && !killed {
						killed = true
						f.killController()
					}
				}
			}
			if _, _, err := f.start(); !errors.Is(err, app.ErrFenced) {
				t.Fatalf("StartFeatureRun() error = %v, want ErrFenced on the pane intent", err)
			}
			if f.manager().State != run.SessionReserved || len(f.ops(app.OpPaneOpen)) != 0 {
				t.Fatalf("the pane intent committed despite the crash")
			}
			result := f.mustResume("controller-2")
			assertResumedLaunching(t, f, &result, 1)
			var placed app.WorkspaceHandle
			decodeInto(t, f.onlyOp(app.OpWorkspaceCreate).ActEvidence, &placed)
			if f.paneCalls[0].WorkspaceID != placed.WorkspaceID {
				t.Fatalf("the manager pane opened in %s, want the recorded workspace %s", f.paneCalls[0].WorkspaceID, placed.WorkspaceID)
			}
		})
	})

	t.Run("pane.open", func(t *testing.T) {
		t.Run("crash after the intent, before the act: the launch stays in flight for the loop, never re-sent", func(t *testing.T) {
			f := newFeatureStart(t)
			f.onHeartbeat = func(n int) {
				if n == 3 {
					f.killController()
				}
			}
			if _, _, err := f.start(); !errors.Is(err, app.ErrFenced) {
				t.Fatalf("StartFeatureRun() error = %v, want ErrFenced", err)
			}
			f.onHeartbeat = nil
			result := f.mustResume("controller-2")
			if result.Outcome != "resumed" || result.RunState != string(run.RunLaunching) {
				t.Fatalf("ResumeFeature() = %+v, want resumed with the run launching", result)
			}
			if len(f.paneCalls) != 0 || f.opState(app.OpPaneOpen) != app.OperationPending {
				t.Fatalf("pane.open ran %d times (state %s); a pending launch is never re-sent", len(f.paneCalls), f.opState(app.OpPaneOpen))
			}
			if _, bound := f.managerBinding(); bound {
				t.Fatalf("a binding appeared without a pane")
			}
			if f.runValue().State != run.RunLaunching || f.manager().State != run.SessionLaunching {
				t.Fatalf("run %s, manager %s; want launching and launching", f.runValue().State, f.manager().State)
			}
		})

		t.Run("crash after the act, before its outcome: the pane is bound by label", func(t *testing.T) {
			f := newFeatureStart(t)
			f.onOpenPane = func(req app.WorkerPaneRequest) (app.PaneHandle, bool, error) {
				handle := f.openPaneLocked(&req)
				f.killController()
				return handle, true, nil
			}
			if _, _, err := f.start(); !errors.Is(err, app.ErrFenced) {
				t.Fatalf("StartFeatureRun() error = %v, want ErrFenced on the outcome commit", err)
			}
			f.onOpenPane = nil
			result := f.mustResume("controller-2")
			assertResumedLaunching(t, f, &result, 1)
			binding, bound := f.managerBinding()
			paneOp := f.onlyOp(app.OpPaneOpen)
			if !bound || binding.CreationLabel != paneOp.ID.String() || paneOp.State != app.OperationSucceeded {
				t.Fatalf("binding %+v, pane.open %s; want the recovered pane bound and the operation succeeded", binding, paneOp.State)
			}

			// Idempotent: another resume round acts on nothing.
			result = f.mustResume("controller-3")
			assertResumedLaunching(t, f, &result, 1)
			if n := len(f.ops(app.OpPaneOpen)); n != 1 {
				t.Fatalf("pane.open operations = %d after a repeated resume, want 1", n)
			}
		})
	})

	t.Run("a completed bootstrap is left alone", func(t *testing.T) {
		f := newFeatureStart(t)
		f.mustStart()
		f.killController()
		result := f.mustResume("controller-2")
		assertResumedLaunching(t, f, &result, 1)
		for _, kind := range []app.OperationKind{app.OpIntegrationInit, app.OpWorkspaceCreate, app.OpPaneOpen} {
			if n := len(f.ops(kind)); n != 1 {
				t.Fatalf("%s operations = %d after resuming a completed bootstrap, want 1", kind, n)
			}
		}
	})

	t.Run("hop stop takes a run whose init failed from created through stopping to stopped", func(t *testing.T) {
		f := newFeatureStart(t)
		f.onHeartbeat = func(n int) {
			if n == 1 {
				f.git.setRef(featureRef, f.base)
			}
		}
		if _, _, err := f.start(); !errors.Is(err, app.ErrIntegrationBranchExists) {
			t.Fatalf("StartFeatureRun() error = %v, want the collision", err)
		}
		f.onHeartbeat = nil
		runID := f.runID()
		if err := f.tc.Controller.RequestStop(context.Background(), runID.String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
		if f.runValue().State != run.RunStopping {
			t.Fatalf("run state = %s after the stop request, want stopping", f.runValue().State)
		}
		lease, err := f.tc.Store.AcquireLease(context.Background(), runID, "controller-stop")
		if err != nil {
			t.Fatalf("AcquireLease() error = %v", err)
		}
		report, err := f.tc.Controller.DriveFeatureStop(context.Background(), app.NewRunHandleForTest(runID, lease))
		if err != nil {
			t.Fatalf("DriveFeatureStop() error = %v", err)
		}
		if !report.Terminated || report.RunState != string(run.RunStopped) {
			t.Fatalf("DriveFeatureStop() = %+v, want stopped", report)
		}
		if f.manager().State != run.SessionTerminated {
			t.Fatalf("manager state = %s, want terminated", f.manager().State)
		}
		if got := f.git.ref(featureRef); got != f.base {
			t.Fatalf("stop touched the colliding ref: %q", got)
		}
	})

	t.Run("a stop request routes resume to stop handling before any continuation", func(t *testing.T) {
		f := newFeatureStart(t)
		f.onHeartbeat = func(n int) {
			if n == 2 {
				f.killController()
			}
		}
		f.crash()
		f.onHeartbeat = nil
		if err := f.tc.Controller.RequestStop(context.Background(), f.runID().String()); err != nil {
			t.Fatalf("RequestStop() error = %v", err)
		}
		result := f.mustResume("controller-2")
		if result.Outcome != "stop-pending" {
			t.Fatalf("ResumeFeature() outcome = %s, want stop-pending", result.Outcome)
		}
		if len(f.workspaceCalls) != 0 || len(f.tc.Runtime.FindWorkspaceLabels) != 0 {
			t.Fatalf("a stopping run's placement was recovered or acted on")
		}
	})
}
