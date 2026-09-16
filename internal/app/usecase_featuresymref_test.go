package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/run"
)

// symrefTargets are the two symbolic shapes a pre-existing integration
// ref can take: dangling (its target absent) and pointing at an existing
// foreign branch. The fake git reproduces what the process adapter's
// real-git probe (TestGitRefSemanticsThroughRunner) pins: `symbolic-ref
// -q` reports both, and a create-only `update-ref --no-deref` refuses both
// and leaves the symref alone.
func symrefTargets(f *featureStart) map[string]func() (target, value string) {
	return map[string]func() (string, string){
		"dangling": func() (string, string) { return "refs/heads/foreign-unborn", "" },
		"existing": func() (string, string) {
			foreign := f.git.newCommit("tree-foreign", f.base)
			f.git.setRef("refs/heads/foreign", foreign)
			return "refs/heads/foreign", foreign
		},
	}
}

// assertForeignUntouched proves the symref and its target are exactly as
// seeded: never dereferenced, never created, never moved.
func assertForeignUntouched(t *testing.T, f *featureStart, target, value string) {
	t.Helper()
	if got := f.git.symref(featureRef); got != target {
		t.Fatalf("symref target = %q, want it left at %q", got, target)
	}
	if got := f.git.ref(target); got != value {
		t.Fatalf("foreign ref %s = %q, want %q (never written through the symref)", target, got, value)
	}
}

// TestFeatureBootstrapSymbolicIntegrationRef proves a symbolic
// integration ref — dangling or not — is a collision on every bootstrap
// path: refused before any side effect at start, failed when it appears
// before the CAS, failed by recovery on resume and by the stop path, and
// never dereferenced, adopted or treated as absence.
func TestFeatureBootstrapSymbolicIntegrationRef(t *testing.T) {
	for _, shape := range []string{"dangling", "existing"} {
		t.Run(shape, func(t *testing.T) {
			t.Run("start: refused before any side effect", func(t *testing.T) {
				f := newFeatureStart(t)
				target, value := symrefTargets(f)[shape]()
				f.git.setSymref(featureRef, target)
				_, _, err := f.start()
				if !errors.Is(err, app.ErrStartRefused) || !errors.Is(err, app.ErrIntegrationBranchExists) || !strings.Contains(err.Error(), featureRef) {
					t.Fatalf("StartFeatureRun() error = %v, want the refused collision naming %s", err, featureRef)
				}
				assertNothingWritten(t, f)
				assertForeignUntouched(t, f, target, value)
			})

			t.Run("start: a symref appearing before the CAS fails as a collision", func(t *testing.T) {
				f := newFeatureStart(t)
				target, value := symrefTargets(f)[shape]()
				f.onHeartbeat = func(n int) {
					if n == 1 {
						f.git.setSymref(featureRef, target)
					}
				}
				_, _, err := f.start()
				if !errors.Is(err, app.ErrIntegrationBranchExists) || errors.Is(err, app.ErrStartRefused) {
					t.Fatalf("StartFeatureRun() error = %v, want the collision after initialize", err)
				}
				op := f.onlyOp(app.OpIntegrationInit)
				var evidence struct {
					SymbolicRef bool `json:"symbolic_ref"`
				}
				decodeInto(t, op.ActEvidence, &evidence)
				if op.State != app.OperationFailed || !evidence.SymbolicRef {
					t.Fatalf("integration.init = %s (symbolic %v), want failed with the symref recorded", op.State, evidence.SymbolicRef)
				}
				assertForeignUntouched(t, f, target, value)
				assertBootstrapStoppedAfterInit(t, f)
			})

			t.Run("resume: recovery fails the pending intent, never adopting or re-acting", func(t *testing.T) {
				f := newFeatureStart(t)
				target, value := symrefTargets(f)[shape]()
				f.onHeartbeat = func(n int) {
					if n == 1 {
						f.killController()
					}
				}
				f.crash()
				f.onHeartbeat = nil
				if value != "" {
					// A symref to a branch AT the base must still never be adopted.
					f.git.setRef(target, f.base)
					value = f.base
				}
				f.git.setSymref(featureRef, target)

				result := f.mustResume("controller-2")
				assertResumeBlocked(t, &result, "inspect it before removing or renaming it")
				if state := f.opState(app.OpIntegrationInit); state != app.OperationFailed {
					t.Fatalf("integration.init state = %s, want failed", state)
				}
				if len(f.git.UpdateRefCalls) != 0 || len(f.workspaceCalls) != 0 || f.manager().State != run.SessionReserved {
					t.Fatalf("recovery acted past a symbolic ref")
				}
				assertForeignUntouched(t, f, target, value)
			})

			t.Run("stop: the pending intent fails and the run stops without writing through", func(t *testing.T) {
				f := newFeatureStart(t)
				target, value := symrefTargets(f)[shape]()
				f.onHeartbeat = func(n int) {
					if n == 1 {
						f.killController()
					}
				}
				f.crash()
				f.onHeartbeat = nil
				f.git.setSymref(featureRef, target)
				runID := f.runID()
				if err := f.tc.Controller.RequestStop(context.Background(), runID.String()); err != nil {
					t.Fatalf("RequestStop() error = %v", err)
				}
				lease, err := f.tc.Store.AcquireLease(context.Background(), runID, "controller-stop")
				if err != nil {
					t.Fatalf("AcquireLease() error = %v", err)
				}
				report, err := f.tc.Controller.DriveFeatureStop(context.Background(), app.NewRunHandleForTest(runID, lease))
				if err != nil {
					t.Fatalf("DriveFeatureStop() error = %v", err)
				}
				if !report.Terminated || f.opState(app.OpIntegrationInit) != app.OperationFailed {
					t.Fatalf("DriveFeatureStop() = %+v, init %s; want stopped with the init failed", report, f.opState(app.OpIntegrationInit))
				}
				if len(f.git.UpdateRefCalls) != 0 {
					t.Fatalf("the stop path dispatched a CAS against a symbolic ref")
				}
				assertForeignUntouched(t, f, target, value)
			})
		})
	}
}
