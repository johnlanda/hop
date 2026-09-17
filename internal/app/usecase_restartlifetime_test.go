package app_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/run"
)

// TestClassifyServerLifetime pins the three-valued trigger predicate. Only
// two non-empty, differing tokens are the restart signal; two non-empty
// equal ones are continuity; anything with an empty side is unknown and is
// never evidence in either direction — which is also every placement's
// verdict on a platform with no lifetime identity.
func TestClassifyServerLifetime(t *testing.T) {
	one, two := fakeServerToken(1), fakeServerToken(2)
	for _, tc := range []struct {
		name               string
		recorded, observed string
		want               app.ServerLifetimeVerdict
	}{
		{"the same lifetime still serves the socket", one, one, app.LifetimeContinuous},
		{"a different lifetime serves the socket", one, two, app.LifetimeChanged},
		{"the placement recorded no lifetime", "", one, app.LifetimeUnknown},
		{"the fresh read established no lifetime", one, "", app.LifetimeUnknown},
		{"neither side is known", "", "", app.LifetimeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := app.ClassifyServerLifetime(tc.recorded, tc.observed); got != tc.want {
				t.Fatalf("ClassifyServerLifetime(%q, %q) = %q, want %q", tc.recorded, tc.observed, got, tc.want)
			}
			// The landed continuity predicate and the new classification are
			// the same reading of the same pair, and must never disagree.
			if established := app.ServerContinuityEstablished(tc.recorded, tc.observed); established != (got(tc) == app.LifetimeContinuous) {
				t.Fatalf("ServerContinuityEstablished(%q, %q) = %t, but the verdict is %q", tc.recorded, tc.observed, established, got(tc))
			}
		})
	}
}

// got re-reads the classification inside the subtest above; it exists so
// the comparison against ServerContinuityEstablished reads as one line.
func got(tc struct {
	name               string
	recorded, observed string
	want               app.ServerLifetimeVerdict
},
) app.ServerLifetimeVerdict {
	return app.ClassifyServerLifetime(tc.recorded, tc.observed)
}

// errFakeLookup is a scripted runtime failure: an inspection or lookup
// that could not be made at all, which is never absence.
var errFakeLookup = errors.New("app_test: scripted runtime lookup failure")

// restartWorker seeds a feature run with one settled child whose pane and
// launch claim are recorded, and returns everything a restart case needs.
func restartWorker(t *testing.T) (*testController, featureRun, workerFixture, run.RuntimeBinding) {
	t.Helper()
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
	w := seedSettledWorker(t, tc, fr, taskID, 4711)
	tc.Store.Attempts[w.AttemptID].value.State = run.AttemptRunning
	tc.Store.Tasks[taskID].value.State = run.TaskActive
	binding, ok := tc.Store.currentBindingLocked(w.SessionID)
	if !ok {
		t.Fatalf("no binding for session %s", w.SessionID)
	}
	// The recorded process is gone from its own group: an empty listing for
	// its pid, which is a SUCCESSFUL observation, never an error.
	tc.Groups.Processes[w.PID] = nil
	return tc, fr, w, binding
}

// serverRestarted scripts the fresh server lifetime a restarted Herdr
// answers with: a different, non-empty token from the one every placement
// recorded.
func serverRestarted(tc *testController) {
	tc.Runtime.ServerInstanceValue = fakeServerToken(2)
}

// paneAnswersNothing is the inspection shape of a pane that answers no
// inspection at all — a restored pane whose deferred agent restore has not
// fired, and equally a pane that is gone.
func paneAnswersNothing(id string) (app.PaneProcess, error) {
	return app.PaneProcess{}, pinnedPaneNotFound("inspect", id)
}

// labelResolvesTo scripts the creation-label lookup answering exactly
// paneID for label, and nothing for any other label.
func labelResolvesTo(tc *testController, label, paneID string) {
	tc.Runtime.FindPaneByLabelFn = func(asked string) (app.PaneRef, bool, error) {
		if asked == label {
			return app.PaneRef{WorkspaceID: "workspace-1", TabID: "tab-1", PaneID: paneID}, true, nil
		}
		return app.PaneRef{}, false, nil
	}
}

// restoredHarnessPane is the pinned shape of a pane whose deferred restore
// HAS fired: its own process is the harness's native restore invocation for
// the session's native reference, under a pid HOP never recorded, and the
// recorded process is nowhere in the group. Pinned against a real server by
// test/integration's TestSpikeRestoredPaneCloseDiscardsResume, which
// observed argv ["<agent>" "--resume" "<ref>"] exactly.
func restoredHarnessPane(pid int, nativeRef string) app.PaneProcess {
	return app.PaneProcess{
		ShellPID: pid, ForegroundGroupID: pid,
		Foreground: []app.ProcessInfo{
			{PID: pid, Argv0: "claude", Name: "claude", Argv: []string{"claude", "--resume", nativeRef}},
		},
	}
}

// restartOptions is the launch parameterization a relaunch needs.
func restartOptions() app.RestartOptions {
	return app.RestartOptions{HOPPath: "/usr/local/bin/hop", StateRoot: "/state"}
}

// TestReconcileServerRestartTrigger pins WHEN the rule fires: only a
// recorded, non-empty lifetime that differs from a fresh, non-empty read.
// Continuity and unknown are the existing rules' business, and neither
// licenses a close — including on a platform that records no lifetime at
// all, where every placement reads unknown forever.
func TestReconcileServerRestartTrigger(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testController, run.RuntimeBinding)
	}{
		{
			name:  "the placement's own lifetime still serves the socket",
			setup: func(*testController, run.RuntimeBinding) {},
		},
		{
			name: "the fresh read establishes no lifetime",
			setup: func(tc *testController, _ run.RuntimeBinding) {
				tc.Runtime.ServerInstanceValue = ""
			},
		},
		{
			name: "the placement recorded no lifetime at all",
			setup: func(tc *testController, _ run.RuntimeBinding) {
				// EVERY placement, not only the worker's: the seeded run has
				// a manager too, and this is the shape of a platform that
				// records no lifetime for any of them.
				for sessionID := range tc.Store.Bindings {
					history := tc.Store.Bindings[sessionID]
					history[len(history)-1].ServerInstance = ""
				}
				serverRestarted(tc)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller, fr, w, binding := restartWorker(t)
			labelResolvesTo(controller, binding.CreationLabel, binding.PaneID)
			controller.Runtime.InspectPaneFn = paneAnswersNothing
			tc.setup(controller, binding)

			report, err := controller.Controller.ReconcileServerRestart(context.Background(), fr.Handle, restartOptions())
			if err != nil {
				t.Fatalf("ReconcileServerRestart() error = %v", err)
			}
			if len(controller.Runtime.ClosedPanes) != 0 {
				t.Fatalf("panes closed = %v, want none: the lifetime is not established as CHANGED", controller.Runtime.ClosedPanes)
			}
			if len(report.Closed)+len(report.Relaunched)+len(report.Outstanding) != 0 {
				t.Fatalf("report = %+v, want nothing at all: this rule never speaks unless the lifetime changed", report)
			}
			if got := controller.Store.Sessions[w.SessionID].value.State; got != run.SessionActive {
				t.Fatalf("session state = %s, want it untouched", got)
			}
		})
	}
}

// TestReconcileServerRestartIdentification pins the identification ladder:
// on a CHANGED lifetime a pane is closed only when it is positively
// identified as this session's own — by its creation label, or by the
// restored harness occupant running THIS session's native reference. A
// recorded pane id alone never authorizes a close, because a workspace
// closed before a restart leaves its id free and the restarted server
// reissues it, so the composed pane id can answer for somebody else's pane
// (test/integration's TestSpikeRecordedPaneIDCanAddressADifferentPane
// observed exactly that).
func TestReconcileServerRestartIdentification(t *testing.T) {
	const otherPaneID = "pane-belonging-to-someone-else"
	for _, tc := range []struct {
		name string
		// setup scripts the runtime for one identification case.
		setup func(*testController, run.RuntimeBinding, workerFixture)
		// wantClosed is whether the recorded pane is closed this round.
		wantClosed bool
		// wantDetail is a substring the outstanding reason must carry when
		// nothing is closed.
		wantDetail string
	}{
		{
			name: "the creation label resolves to the recorded pane: identified, in either restore window",
			setup: func(tc *testController, binding run.RuntimeBinding, _ workerFixture) {
				labelResolvesTo(tc, binding.CreationLabel, binding.PaneID)
				tc.Runtime.InspectPaneFn = paneAnswersNothing
			},
			wantClosed: true,
		},
		{
			name: "the label is gone but the pane holds this session's own resumed agent: identified",
			setup: func(tc *testController, binding run.RuntimeBinding, w workerFixture) {
				tc.Runtime.InspectPaneFn = func(paneID string) (app.PaneProcess, error) {
					if paneID == binding.PaneID {
						return restoredHarnessPane(w.PID+900, nativeRefFor(tc, w)), nil
					}
					return app.PaneProcess{}, pinnedPaneNotFound("inspect", paneID)
				}
			},
			wantClosed: true,
		},
		{
			name: "the label answers a DIFFERENT pane: the recorded id is not this session's",
			setup: func(tc *testController, binding run.RuntimeBinding, _ workerFixture) {
				labelResolvesTo(tc, binding.CreationLabel, otherPaneID)
				tc.Runtime.InspectPaneFn = paneAnswersNothing
			},
			wantDetail: "answers for a DIFFERENT pane",
		},
		{
			name: "the label lookup fails: absence is never assumed from an inspection error",
			setup: func(tc *testController, _ run.RuntimeBinding, _ workerFixture) {
				tc.Runtime.FindPaneByLabelFn = func(string) (app.PaneRef, bool, error) {
					return app.PaneRef{}, false, errFakeLookup
				}
				tc.Runtime.InspectPaneFn = paneAnswersNothing
			},
			wantDetail: "creation-label lookup failed",
		},
		{
			name: "the recorded id answers for a pane that is nobody's agent: the reissued-id case",
			setup: func(tc *testController, binding run.RuntimeBinding, _ workerFixture) {
				tc.Runtime.InspectPaneFn = func(paneID string) (app.PaneProcess, error) {
					if paneID == binding.PaneID {
						// A plain login shell: exactly what the reissued pane
						// id answered for in the executed probe.
						return app.PaneProcess{ShellPID: 51, ForegroundGroupID: 51, Foreground: []app.ProcessInfo{
							{PID: 51, Argv0: "-sh", Name: "sh", Argv: []string{"-sh"}},
						}}, nil
					}
					return app.PaneProcess{}, pinnedPaneNotFound("inspect", paneID)
				}
			},
			wantDetail: "answers for an occupant this session cannot claim",
		},
		{
			name: "two members carry this session's restore invocation: ambiguous, never closed",
			setup: func(tc *testController, binding run.RuntimeBinding, w workerFixture) {
				tc.Runtime.InspectPaneFn = func(paneID string) (app.PaneProcess, error) {
					if paneID != binding.PaneID {
						return app.PaneProcess{}, pinnedPaneNotFound("inspect", paneID)
					}
					ref := nativeRefFor(tc, w)
					pane := restoredHarnessPane(w.PID+900, ref)
					pane.Foreground = append(pane.Foreground, app.ProcessInfo{
						PID: w.PID + 901, Argv0: "claude", Name: "claude", Argv: []string{"claude", "--resume", ref},
					})
					return pane, nil
				}
			},
			wantDetail: "more than one member",
		},
		{
			name: "neither the label nor the id answers: a pane awaiting its restore answers neither",
			setup: func(tc *testController, _ run.RuntimeBinding, _ workerFixture) {
				tc.Runtime.InspectPaneFn = paneAnswersNothing
			},
			wantDetail: "rename it back to",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller, fr, w, binding := restartWorker(t)
			serverRestarted(controller)
			tc.setup(controller, binding, w)

			report, err := controller.Controller.ReconcileServerRestart(context.Background(), fr.Handle, restartOptions())
			if err != nil {
				t.Fatalf("ReconcileServerRestart() error = %v", err)
			}
			closed := slices.Contains(controller.Runtime.ClosedPanes, binding.PaneID)
			if closed != tc.wantClosed {
				t.Fatalf("closed = %t (panes %v), want %t; report %+v", closed, controller.Runtime.ClosedPanes, tc.wantClosed, report)
			}
			if tc.wantClosed {
				return
			}
			if !slices.ContainsFunc(report.Outstanding, func(entry string) bool { return strings.Contains(entry, tc.wantDetail) }) {
				t.Fatalf("outstanding = %q, want an entry carrying %q", report.Outstanding, tc.wantDetail)
			}
		})
	}
}

// nativeRefFor is the session's recorded native session reference, the one
// value a restored-harness occupant must carry to identify the pane.
func nativeRefFor(tc *testController, w workerFixture) string {
	return tc.Store.Sessions[w.SessionID].value.NativeSessionRef
}

// TestReconcileServerRestartProcessConjunct pins that a CHANGED lifetime
// replaces the CONTINUITY conjunct only: the recorded process must still be
// OBSERVED gone by a successful group listing, and a live or unobservable
// one concludes nothing — exactly as before this rule existed.
func TestReconcileServerRestartProcessConjunct(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(*testController, workerFixture)
		wantDetail string
	}{
		{
			name: "the recorded process is still listed in its own group",
			setup: func(tc *testController, w workerFixture) {
				tc.Groups.Processes[w.PID] = []app.GroupProcess{{PID: w.PID, Argv: []string{"claude"}}}
			},
			wantDetail: "still runs",
		},
		{
			name: "the group listing itself could not be made",
			setup: func(tc *testController, w workerFixture) {
				tc.Groups.ListErr[w.PID] = errFakeLookup
			},
			wantDetail: "listing failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller, fr, w, binding := restartWorker(t)
			serverRestarted(controller)
			labelResolvesTo(controller, binding.CreationLabel, binding.PaneID)
			controller.Runtime.InspectPaneFn = paneAnswersNothing
			tc.setup(controller, w)

			report, err := controller.Controller.ReconcileServerRestart(context.Background(), fr.Handle, restartOptions())
			if err != nil {
				t.Fatalf("ReconcileServerRestart() error = %v", err)
			}
			if len(controller.Runtime.ClosedPanes) != 0 {
				t.Fatalf("panes closed = %v, want none: the recorded process is not observed gone", controller.Runtime.ClosedPanes)
			}
			if !slices.ContainsFunc(report.Outstanding, func(entry string) bool { return strings.Contains(entry, tc.wantDetail) }) {
				t.Fatalf("outstanding = %q, want an entry carrying %q", report.Outstanding, tc.wantDetail)
			}
		})
	}
}

// TestReconcileServerRestartBracket pins the closing half of the bracket: a
// lifetime that changes AGAIN while a session is being observed concludes
// nothing, because observations spanning a restart were not all served by
// one lifetime. It is the CHANGED analog of the continuity bracket.
func TestReconcileServerRestartBracket(t *testing.T) {
	controller, fr, _, binding := restartWorker(t)
	serverRestarted(controller)
	controller.Runtime.InspectPaneFn = paneAnswersNothing
	// A third lifetime takes over DURING the identification — the label
	// lookup is the first observation the ladder makes — so the reads
	// either side of the observations no longer agree.
	controller.Runtime.FindPaneByLabelFn = func(asked string) (app.PaneRef, bool, error) {
		controller.Runtime.ServerInstanceValue = fakeServerToken(3)
		if asked == binding.CreationLabel {
			return app.PaneRef{WorkspaceID: "workspace-1", TabID: "tab-1", PaneID: binding.PaneID}, true, nil
		}
		return app.PaneRef{}, false, nil
	}

	report, err := controller.Controller.ReconcileServerRestart(context.Background(), fr.Handle, restartOptions())
	if err != nil {
		t.Fatalf("ReconcileServerRestart() error = %v", err)
	}
	if len(controller.Runtime.ClosedPanes) != 0 {
		t.Fatalf("panes closed = %v, want none: the observations spanned a restart", controller.Runtime.ClosedPanes)
	}
	if !slices.ContainsFunc(report.Outstanding, func(entry string) bool { return strings.Contains(entry, "span a restart") }) {
		t.Fatalf("outstanding = %q, want the spanning-restart reason", report.Outstanding)
	}
}
