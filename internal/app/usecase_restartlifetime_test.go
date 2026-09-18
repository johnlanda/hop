package app_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/identity"
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
// paneID for label — and answering NOTHING for it once that pane has been
// closed. The second half is not a convenience: Herdr drops a pane's label
// with the pane, so a lookup that kept answering for a closed pane would
// reproduce a world that cannot occur (pinned by test/integration's
// TestSpikeVanishedPaneShapes: after a close the creation label resolves
// nothing). A fixture that got this wrong would let a close appear never to
// take effect.
func labelResolvesTo(tc *testController, label, paneID string) {
	tc.Runtime.FindPaneByLabelFn = func(asked string) (app.PaneRef, bool, error) {
		if asked != label || slices.Contains(tc.Runtime.ClosedPanes, paneID) {
			return app.PaneRef{}, false, nil
		}
		return app.PaneRef{WorkspaceID: "workspace-1", TabID: "tab-1", PaneID: paneID}, true, nil
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

// TestReconcileServerRestartIgnoresSessionsThatAreAlreadyTerminal pins the
// candidate filter, which is the OTHER half of "two sessions never identify
// the same pane" — the half the identification ladder cannot supply.
//
// A terminal session keeps its binding: a binding is superseded by a
// confirmed close, a resume's adoption or a feature cold relaunch, and by
// nothing else, so a worker the settlement machinery terminated
// (applyWorkerTermination) and a predecessor a solo cold relaunch marked
// lost (coldRelaunch) both keep a CURRENT binding naming their recorded
// pane. The lost one is the dangerous shape: a lineage SHARES its native
// session reference, so the predecessor's own restore invocation is exactly
// what its SUCCESSOR's pane runs — and a recorded pane id is not a durable
// address, so after a restart the predecessor's id can answer for that
// successor's pane. A predecessor admitted as a candidate would then
// identify its successor's pane by the harness rung and close a live agent.
func TestReconcileServerRestartIgnoresSessionsThatAreAlreadyTerminal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state run.SessionState
	}{
		{name: "a terminated worker keeps the binding nothing superseded", state: run.SessionTerminated},
		{name: "a lost predecessor shares its lineage's native reference", state: run.SessionLost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller, fr, w, binding := restartWorker(t)
			controller.Store.Sessions[w.SessionID].value.State = tc.state
			serverRestarted(controller)
			// The recorded pane id answers for a pane running THIS lineage's
			// restore invocation: the shape a successor's pane presents once
			// a reissued workspace id makes the predecessor's recorded id
			// address it.
			controller.Runtime.InspectPaneFn = func(paneID string) (app.PaneProcess, error) {
				if paneID == binding.PaneID {
					return restoredHarnessPane(w.PID+900, nativeRefFor(controller, w)), nil
				}
				return app.PaneProcess{}, pinnedPaneNotFound("inspect", paneID)
			}

			report, err := controller.Controller.ReconcileServerRestart(context.Background(), fr.Handle, restartOptions())
			if err != nil {
				t.Fatalf("ReconcileServerRestart() error = %v", err)
			}
			if len(controller.Runtime.ClosedPanes) != 0 {
				t.Fatalf("panes closed = %v, want none: a %s session is not this step's business", controller.Runtime.ClosedPanes, tc.state)
			}
			for _, entry := range append(append(append([]string{}, report.Closed...), report.Relaunched...), report.Outstanding...) {
				if strings.Contains(entry, w.SessionID.String()) {
					t.Fatalf("report names the %s session: %q", tc.state, entry)
				}
			}
			if got := controller.Store.Sessions[w.SessionID].value.State; got != tc.state {
				t.Fatalf("session state = %s, want it left %s", got, tc.state)
			}
		})
	}
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

// identifiedByLabel scripts the identification rung that speaks in BOTH
// restore windows: the session's creation label resolves to exactly its
// recorded pane id, and the pane itself answers nothing (its deferred
// restore has not fired).
func identifiedByLabel(tc *testController, binding run.RuntimeBinding) { //nolint:gocritic // hugeParam: the fixture binding is passed once per case.
	labelResolvesTo(tc, binding.CreationLabel, binding.PaneID)
	tc.Runtime.InspectPaneFn = paneAnswersNothing
}

// TestReconcileServerRestartRetires pins the disposition a held stop
// takes, which is the one that frees a run stuck stopping: an identified
// pane is closed, its
// absence is observed and the session is TERMINATED — so the slot frees and
// the run can reach stopped. Nothing is relaunched into a stopping run.
func TestReconcileServerRestartRetires(t *testing.T) {
	tc, fr, w, binding := restartWorker(t)
	requestRunStop(tc, fr.RunID)
	serverRestarted(tc)
	identifiedByLabel(tc, binding)

	report, err := tc.Controller.ReconcileServerRestart(context.Background(), fr.Handle, restartOptions())
	if err != nil {
		t.Fatalf("ReconcileServerRestart() error = %v", err)
	}
	if !slices.Contains(tc.Runtime.ClosedPanes, binding.PaneID) {
		t.Fatalf("panes closed = %v, want the identified pane %s", tc.Runtime.ClosedPanes, binding.PaneID)
	}
	if !slices.Contains(report.Closed, w.SessionID.String()) {
		t.Fatalf("report = %+v, want the session reported closed", report)
	}
	if len(report.Relaunched) != 0 {
		t.Fatalf("relaunched = %v, want nothing: a stopping run never relaunches", report.Relaunched)
	}
	if got := tc.Store.Sessions[w.SessionID].value.State; got != run.SessionTerminated {
		t.Fatalf("session state = %s, want terminated so the slot frees", got)
	}
}

// TestReconcileServerRestartRetiresWhatWasDueForRetirement pins the two
// retire rows that are not the held stop: a run carrying a durable
// terminal-failure cause, and a session whose attempt has already reached a
// retirement boundary. Both close the pane and TERMINATE the session.
// Relaunching either would start a real agent in the worktree of a run that
// has already failed, or on an attempt that is already done, for the next
// retirement round to close again.
func TestReconcileServerRestartRetiresWhatWasDueForRetirement(t *testing.T) {
	settledAttempt := func(state run.AttemptState) func(*testing.T, *testController, featureRun, workerFixture) {
		return func(t *testing.T, controller *testController, _ featureRun, w workerFixture) {
			t.Helper()
			controller.Store.Attempts[w.AttemptID].value.State = state
		}
	}
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *testController, featureRun, workerFixture)
	}{
		{
			name: "a failed task is the run's durable terminal-failure cause",
			setup: func(t *testing.T, controller *testController, fr featureRun, _ workerFixture) {
				t.Helper()
				// The run is still running: the retirement pass has not yet
				// failed it, which is exactly the window a restart lands in.
				seedImplementTask(t, controller, fr.RunID, 2, "B", false, run.TaskFailed)
			},
		},
		{name: "the attempt's outcome is completed", setup: settledAttempt(run.AttemptCompleted)},
		{name: "the attempt's outcome is failed", setup: settledAttempt(run.AttemptFailed)},
		{name: "the attempt's outcome is interrupted", setup: settledAttempt(run.AttemptInterrupted)},
		{
			name: "the attempt's result is accepted, before the attempt itself transitions",
			setup: func(t *testing.T, controller *testController, _ featureRun, w workerFixture) {
				t.Helper()
				resultID, err := identity.ParseResultID(controller.IDs.NewID())
				if err != nil {
					t.Fatalf("parse result id: %v", err)
				}
				controller.Store.Results[w.AttemptID] = run.Result{
					ID: resultID, AttemptID: w.AttemptID, CommitOID: "c",
					Accepted: true, SubmittedAt: controller.Clock.Now(),
				}
				controller.Store.Attempts[w.AttemptID].value.State = run.AttemptSubmitted
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller, fr, w, binding := restartWorker(t)
			tc.setup(t, controller, fr, w)
			serverRestarted(controller)
			identifiedByLabel(controller, binding)

			report, err := controller.Controller.ReconcileServerRestart(context.Background(), fr.Handle, restartOptions())
			if err != nil {
				t.Fatalf("ReconcileServerRestart() error = %v", err)
			}
			if !slices.Contains(controller.Runtime.ClosedPanes, binding.PaneID) {
				t.Fatalf("panes closed = %v, want the identified pane %s", controller.Runtime.ClosedPanes, binding.PaneID)
			}
			if len(report.Relaunched) != 0 {
				t.Fatalf("relaunched = %v, want nothing: this session was due for retirement", report.Relaunched)
			}
			if !slices.Contains(report.Closed, w.SessionID.String()) {
				t.Fatalf("report = %+v, want the session reported closed", report)
			}
			if got := controller.Store.Sessions[w.SessionID].value.State; got != run.SessionTerminated {
				t.Fatalf("session state = %s, want terminated", got)
			}
		})
	}
}

// TestReconcileServerRestartRelaunches pins the follow-through: a running
// feature run's settled worker, closed after a restart, is cold-relaunched
// from its RECORDED native session reference through the existing relaunch
// path — a successor session bound to the same reference, its predecessor
// marked lost with this cause's own reason, and a new pane opened.
func TestReconcileServerRestartRelaunches(t *testing.T) {
	tc, fr, w, binding := restartWorker(t)
	serverRestarted(tc)
	identifiedByLabel(tc, binding)
	priorRef := nativeRefFor(tc, w)
	if priorRef == "" {
		t.Fatal("the seeded worker has no native session reference; the relaunch branch cannot be reached")
	}

	report, err := tc.Controller.ReconcileServerRestart(context.Background(), fr.Handle, restartOptions())
	if err != nil {
		t.Fatalf("ReconcileServerRestart() error = %v", err)
	}
	if !slices.Contains(report.Relaunched, w.SessionID.String()) {
		t.Fatalf("report = %+v, want the session relaunched", report)
	}
	if got := tc.Store.Sessions[w.SessionID].value.State; got != run.SessionLost {
		t.Fatalf("predecessor session state = %s, want lost", got)
	}
	successor := successorSessionFor(t, tc, w)
	if successor.NativeSessionRef != priorRef {
		t.Fatalf("successor native reference = %q, want the predecessor's %q: the conversation continues", successor.NativeSessionRef, priorRef)
	}
	if successor.AttemptID != w.AttemptID {
		t.Fatalf("successor attempt = %s, want the SAME attempt %s", successor.AttemptID, w.AttemptID)
	}
	if successor.State != run.SessionLaunching {
		t.Fatalf("successor state = %s, want launching", successor.State)
	}
}

// successorSessionFor is the session the relaunch created for the same
// attempt: the one non-terminal session of that attempt other than prior.
func successorSessionFor(t *testing.T, tc *testController, prior workerFixture) run.Session {
	t.Helper()
	var found []run.Session
	for id, row := range tc.Store.Sessions {
		if id == prior.SessionID || row.value.AttemptID != prior.AttemptID {
			continue
		}
		if row.value.State == run.SessionTerminated || row.value.State == run.SessionLost {
			continue
		}
		found = append(found, row.value)
	}
	if len(found) != 1 {
		t.Fatalf("sessions succeeding %s on attempt %s = %d, want exactly 1", prior.SessionID, prior.AttemptID, len(found))
	}
	return found[0]
}

// TestReconcileServerRestartSettlesAnUncorroboratedLaunch pins the branch
// that must NOT relaunch: a launch the restart ended before it was ever
// corroborated. Its native session reference is PRE-ASSIGNED, so no
// transcript exists to resume and `--resume` of it would fail at the
// harness; the claim settles exec_failed under this cause's own distinct
// reason and the ordinary exec-failure consequences follow.
func TestReconcileServerRestartSettlesAnUncorroboratedLaunch(t *testing.T) {
	tc, fr, w, binding := restartWorker(t)
	// The real shape of a launch the restart ended before corroboration:
	// an exec_pending claim, with the attempt and session still LAUNCHING.
	// A running attempt would mean the launch HAD been corroborated, which
	// is a fixture that cannot occur.
	claim := tc.Store.LaunchClaims[w.IncarnationID]
	claim.State = app.LaunchClaimExecPending
	tc.Store.LaunchClaims[w.IncarnationID] = claim
	tc.Store.Attempts[w.AttemptID].value.State = run.AttemptLaunching
	tc.Store.Sessions[w.SessionID].value.State = run.SessionLaunching
	serverRestarted(tc)
	identifiedByLabel(tc, binding)

	report, err := tc.Controller.ReconcileServerRestart(context.Background(), fr.Handle, restartOptions())
	if err != nil {
		t.Fatalf("ReconcileServerRestart() error = %v", err)
	}
	if len(report.Relaunched) != 0 {
		t.Fatalf("relaunched = %v, want nothing: a harness that never started has no transcript to resume", report.Relaunched)
	}
	settled := tc.Store.LaunchClaims[w.IncarnationID]
	if settled.State != app.LaunchClaimExecFailed {
		t.Fatalf("claim state = %s, want exec_failed", settled.State)
	}
	if !strings.Contains(settled.Error, "server restart") {
		t.Fatalf("claim error = %q, want this cause's own distinct reason", settled.Error)
	}
	if got := tc.Store.Attempts[w.AttemptID].value.State; got != run.AttemptFailed {
		t.Fatalf("attempt state = %s, want failed: an exec failure is a terminal attempt outcome", got)
	}

	// The settlement must be REPORTED as this cause, not as the launcher's
	// own generic exec failure. The whole claim of this rule is that the
	// journal tells a restart from every other close, and the session's own
	// transition is where a human looks: a reason the launch-ended
	// recogniser does not know is rendered with the generic wording, which
	// silently contradicts that claim.
	reason := newestSessionTransitionReason(t, tc, w.SessionID)
	if !strings.Contains(reason, "server restart") {
		t.Errorf("the settled session's transition reads %q, want it naming the server restart as the cause", reason)
	}
	notice := controllerNoticesTo(tc, fr.RunID)
	if len(notice) == 0 {
		t.Fatal("no manager notice was committed for the settled attempt")
	}
	body := string(tc.Artifacts.files[notice[len(notice)-1].BodyPath])
	if !strings.Contains(body, "server restart") {
		t.Errorf("the manager's notice reads %q, want it naming the server restart as the cause", body)
	}
}

// TestReconcileServerRestartInterruptsAHarnessItCannotResume pins the third
// branch that does not relaunch: the session's work could continue, but cold
// resume is Claude-only and needs a recorded native reference, so the
// attempt is INTERRUPTED and the manager decides whether to retry. Nothing
// is started in the worktree, and the attempt does not silently stay open.
func TestReconcileServerRestartInterruptsAHarnessItCannotResume(t *testing.T) {
	tc, fr, w, binding := restartWorker(t)
	// No native reference: nothing to resume the conversation from.
	tc.Store.Sessions[w.SessionID].value.NativeSessionRef = ""
	serverRestarted(tc)
	identifiedByLabel(tc, binding)

	report, err := tc.Controller.ReconcileServerRestart(context.Background(), fr.Handle, restartOptions())
	if err != nil {
		t.Fatalf("ReconcileServerRestart() error = %v", err)
	}
	if !slices.Contains(tc.Runtime.ClosedPanes, binding.PaneID) {
		t.Fatalf("panes closed = %v, want the identified pane %s", tc.Runtime.ClosedPanes, binding.PaneID)
	}
	if len(report.Relaunched) != 0 {
		t.Fatalf("relaunched = %v, want nothing: there is no native reference to resume", report.Relaunched)
	}
	if !slices.Contains(report.Closed, w.SessionID.String()) {
		t.Fatalf("report = %+v, want the session reported closed", report)
	}
	if got := tc.Store.Attempts[w.AttemptID].value.State; got != run.AttemptInterrupted {
		t.Fatalf("attempt state = %s, want interrupted: the manager decides whether to retry", got)
	}
	if got := tc.Store.LaunchClaims[w.IncarnationID].State; got != app.LaunchClaimExeced {
		t.Fatalf("claim state = %s, want it left settled: this launch WAS corroborated, so it is not an exec failure", got)
	}
	notice := controllerNoticesTo(tc, fr.RunID)
	if len(notice) == 0 {
		t.Fatal("no manager notice was committed for the interrupted attempt")
	}
	body := string(tc.Artifacts.files[notice[len(notice)-1].BodyPath])
	if !strings.Contains(body, "server restarted") || !strings.Contains(body, "no cold resume") {
		t.Errorf("the manager's notice reads %q, want it naming the restart and why the attempt could not be resumed", body)
	}
	reason := newestSessionTransitionReason(t, tc, w.SessionID)
	if got := app.RestartDispositionFor(reason); got != app.RestartDispositionClosed {
		t.Errorf("the interrupted session renders %q from reason %q, want %q", got, reason, app.RestartDispositionClosed)
	}
}

// newestSessionTransitionReason is the reason of a session's most recent
// recorded transition — the same durable record the status surface reduces,
// read here to assert what a human is told about a settlement.
func newestSessionTransitionReason(t *testing.T, tc *testController, sessionID identity.SessionID) string {
	t.Helper()
	reason := ""
	for i := range tc.Store.Transitions {
		entry := &tc.Store.Transitions[i]
		if entry.EntityKind == app.EntitySession && entry.EntityID == sessionID.String() {
			reason = entry.Reason
		}
	}
	return reason
}

// TestReconcileServerRestartAndASoloRun pins this step's scope on a run
// that has no session index of its own. A solo run takes it ONLY under a
// held stop, where the whole action is to close the pane and conclude
// absence so the run can reach stopped; a running one keeps its existing
// fail-closed exit untouched, because solo cold relaunch is exclusively a
// `hop resume` recovery action and this step does not amend that.
func TestReconcileServerRestartAndASoloRun(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stop       bool
		wantClosed bool
		wantState  run.SessionState
	}{
		{name: "a held stop closes the pane and terminates the session", stop: true, wantClosed: true, wantState: run.SessionTerminated},
		{name: "a running solo run is left to its existing exit", wantState: run.SessionActive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller := newTestController(defaultPolicy())
			handle, detail := runningRun(t, controller)
			if tc.stop {
				requestRunStop(controller, detail.RunID)
			}
			binding, ok := controller.Store.currentBindingLocked(detail.SessionID)
			if !ok {
				t.Fatalf("no binding for the solo session %s", detail.SessionID)
			}
			// The recorded process is gone from its own group: a successful
			// listing that finds nothing, never an error.
			controller.Groups.Processes[detail.Claim.PID] = nil
			serverRestarted(controller)
			identifiedByLabel(controller, binding)

			report, err := controller.Controller.ReconcileServerRestart(context.Background(), handle, restartOptions())
			if err != nil {
				t.Fatalf("ReconcileServerRestart() error = %v", err)
			}
			if closed := slices.Contains(controller.Runtime.ClosedPanes, binding.PaneID); closed != tc.wantClosed {
				t.Fatalf("closed = %t (panes %v), want %t; report %+v", closed, controller.Runtime.ClosedPanes, tc.wantClosed, report)
			}
			if len(report.Relaunched) != 0 {
				t.Fatalf("relaunched = %v, want nothing: a solo run never relaunches here", report.Relaunched)
			}
			if got := controller.Store.Sessions[detail.SessionID].value.State; got != tc.wantState {
				t.Fatalf("solo session state = %s, want %s", got, tc.wantState)
			}
		})
	}
}

// TestReconcileServerRestartOrdersTheManagerLast pins the ordering the
// manager relaunch requires: every child session is reconciled BEFORE the
// manager, so a round that fails partway has not moved the run's manager
// lineage. The order is asserted from the closes themselves, not from the
// report, since the closes are what actually happened.
func TestReconcileServerRestartOrdersTheManagerLast(t *testing.T) {
	tc, fr, w, binding := restartWorker(t)
	managerBinding, ok := tc.Store.currentBindingLocked(fr.ManagerID)
	if !ok {
		t.Fatalf("no binding for the manager session %s", fr.ManagerID)
	}
	serverRestarted(tc)
	tc.Runtime.InspectPaneFn = paneAnswersNothing
	// Both panes are identified by their own creation labels, and each
	// stops answering once closed, as a real one does.
	tc.Runtime.FindPaneByLabelFn = func(asked string) (app.PaneRef, bool, error) {
		for _, known := range []run.RuntimeBinding{binding, managerBinding} {
			if asked == known.CreationLabel && !slices.Contains(tc.Runtime.ClosedPanes, known.PaneID) {
				return app.PaneRef{PaneID: known.PaneID}, true, nil
			}
		}
		return app.PaneRef{}, false, nil
	}

	if _, err := tc.Controller.ReconcileServerRestart(context.Background(), fr.Handle, restartOptions()); err != nil {
		t.Fatalf("ReconcileServerRestart() error = %v", err)
	}
	worker := slices.Index(tc.Runtime.ClosedPanes, binding.PaneID)
	manager := slices.Index(tc.Runtime.ClosedPanes, managerBinding.PaneID)
	if worker < 0 || manager < 0 {
		t.Fatalf("closed panes = %v, want both the worker's %s and the manager's %s", tc.Runtime.ClosedPanes, binding.PaneID, managerBinding.PaneID)
	}
	if manager < worker {
		t.Fatalf("closed panes = %v: the manager's pane was closed before the worker's; the manager is reconciled LAST", tc.Runtime.ClosedPanes)
	}
	_ = w
}

// TestRestartDispositionForIsValueFree pins the ONE mapping from a
// journaled reason to the status surface. Only the two reasons this rule
// writes map to anything, every other reason maps to nothing, and the
// rendered values carry no lifetime token, pid, label or path — the
// journal keeps the detail and the surface says what happened.
func TestRestartDispositionForIsValueFree(t *testing.T) {
	relaunched := app.RestartDispositionFor(app.RestartRelaunchReasonForTest)
	closed := app.RestartDispositionFor(app.RestartSessionReasonForTest)
	if relaunched != app.RestartDispositionRelaunched {
		t.Errorf("the relaunch reason maps to %q, want %q", relaunched, app.RestartDispositionRelaunched)
	}
	if closed != app.RestartDispositionClosed {
		t.Errorf("the close reason maps to %q, want %q", closed, app.RestartDispositionClosed)
	}
	// Every branch that closes a pane renders, including the two whose
	// session reason says more about the session than the close itself.
	if got := app.RestartDispositionFor(app.RestartInterruptReasonForTest); got != app.RestartDispositionClosed {
		t.Errorf("the interrupt reason maps to %q, want %q", got, app.RestartDispositionClosed)
	}
	if got := app.RestartDispositionFor(app.RestartLaunchEndedReasonForTest); got != app.RestartDispositionClosed {
		t.Errorf("the settled launch's reason maps to %q, want %q", got, app.RestartDispositionClosed)
	}
	for _, other := range []string{"", "stop: termination observed", "cold relaunch authorized", "worker exited without an accepted result", "exec_failed claim; no process"} {
		if got := app.RestartDispositionFor(other); got != "" {
			t.Errorf("RestartDispositionFor(%q) = %q, want no disposition: only this rule's own reasons map", other, got)
		}
	}
	for _, rendered := range []string{app.RestartDispositionClosed, app.RestartDispositionRelaunched} {
		for _, forbidden := range []string{"pid", "herdr-server-lifetime", "/", "pane-"} {
			if strings.Contains(rendered, forbidden) {
				t.Errorf("the rendered disposition %q carries %q; this surface is value-free", rendered, forbidden)
			}
		}
	}
}

// TestReconcileServerRestartJournalsItsOwnReasons pins that the two
// dispositions a human sees are actually REACHED by the rule, through the
// journal rather than through a constant, from EVERY branch that closes a
// pane: a status surface fed by a reason nothing writes would render
// nothing, and a branch whose reason the reduction does not recognize
// renders nothing for a session whose pane this step closed.
func TestReconcileServerRestartJournalsItsOwnReasons(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testController, featureRun, workerFixture)
		want  string
	}{
		{
			name:  "a stopping run's closed session",
			setup: func(controller *testController, fr featureRun, _ workerFixture) { requestRunStop(controller, fr.RunID) },
			want:  app.RestartDispositionClosed,
		},
		{
			name:  "a running run's relaunched session",
			setup: func(*testController, featureRun, workerFixture) {},
			want:  app.RestartDispositionRelaunched,
		},
		{
			name: "a session whose launch the restart ended before it was corroborated",
			setup: func(controller *testController, _ featureRun, w workerFixture) {
				claim := controller.Store.LaunchClaims[w.IncarnationID]
				claim.State = app.LaunchClaimExecPending
				controller.Store.LaunchClaims[w.IncarnationID] = claim
				controller.Store.Attempts[w.AttemptID].value.State = run.AttemptLaunching
				controller.Store.Sessions[w.SessionID].value.State = run.SessionLaunching
			},
			want: app.RestartDispositionClosed,
		},
		{
			name: "a session whose harness has no cold resume",
			setup: func(controller *testController, _ featureRun, w workerFixture) {
				controller.Store.Sessions[w.SessionID].value.NativeSessionRef = ""
			},
			want: app.RestartDispositionClosed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller, fr, w, binding := restartWorker(t)
			tc.setup(controller, fr, w)
			serverRestarted(controller)
			identifiedByLabel(controller, binding)

			if _, err := controller.Controller.ReconcileServerRestart(context.Background(), fr.Handle, restartOptions()); err != nil {
				t.Fatalf("ReconcileServerRestart() error = %v", err)
			}
			detail, err := controller.Controller.Status(context.Background(), app.StatusRequest{RunID: fr.RunID.String()})
			if err != nil {
				t.Fatalf("Status() error = %v", err)
			}
			var rendered string
			for _, session := range detail.Detail.Sessions {
				if session.SessionID == w.SessionID.String() {
					rendered = session.RestartDisposition
				}
			}
			if rendered != tc.want {
				t.Fatalf("the session's rendered restart disposition = %q, want %q", rendered, tc.want)
			}
		})
	}
}

// TestReconcileServerRestartJournalsItsAuthorization pins what the close
// intent records: the rung that identified the pane, and BOTH lifetime
// tokens — the placement's recorded one and the observing one it differs
// from, which together are the evidence that licensed the close. They live
// in the journal, never on any human-facing surface, and they are in the
// INTENT because the intent is the record of what a close was authorized
// against and because a later round re-drives this close from the
// persisted target rather than re-identifying it.
func TestReconcileServerRestartJournalsItsAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testController, run.RuntimeBinding, workerFixture)
		want  string
	}{
		{
			name: "identified by the creation label",
			setup: func(controller *testController, binding run.RuntimeBinding, _ workerFixture) {
				identifiedByLabel(controller, binding)
			},
			want: "creation label",
		},
		{
			name: "identified by the restored harness occupant",
			setup: func(controller *testController, binding run.RuntimeBinding, w workerFixture) {
				controller.Runtime.InspectPaneFn = func(paneID string) (app.PaneProcess, error) {
					if paneID == binding.PaneID && !slices.Contains(controller.Runtime.ClosedPanes, paneID) {
						return restoredHarnessPane(w.PID+900, nativeRefFor(controller, w)), nil
					}
					return app.PaneProcess{}, pinnedPaneNotFound("inspect", paneID)
				}
			},
			want: "restored harness occupant",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller, fr, w, binding := restartWorker(t)
			serverRestarted(controller)
			tc.setup(controller, binding, w)

			if _, err := controller.Controller.ReconcileServerRestart(context.Background(), fr.Handle, restartOptions()); err != nil {
				t.Fatalf("ReconcileServerRestart() error = %v", err)
			}
			intent := restartCloseIntentFor(t, controller, binding.PaneID)
			if intent.IdentifiedBy != tc.want {
				t.Errorf("the close intent records identification %q, want %q", intent.IdentifiedBy, tc.want)
			}
			if intent.RecordedServerInstance != binding.ServerInstance {
				t.Errorf("the close intent records the placement's lifetime %q, want %q", intent.RecordedServerInstance, binding.ServerInstance)
			}
			if intent.ServerInstance == "" || intent.ServerInstance == binding.ServerInstance {
				t.Errorf("the close intent's observing lifetime = %q, want a non-empty one DIFFERING from the placement's %q", intent.ServerInstance, binding.ServerInstance)
			}
		})
	}
}

// restartCloseIntentFor reads the single restart close this run journaled
// for paneID, decoded from the operation as a later round would read it.
func restartCloseIntentFor(t *testing.T, tc *testController, paneID string) app.PaneCloseIntentForTest {
	t.Helper()
	intent, _ := app.DecodePaneCloseIntentForTest(restartCloseOperationFor(t, tc, paneID).Intent)
	return intent
}

// restartCloseOperationFor is the ONE pane.close operation this rule
// journaled for paneID. One pane never carries two of them: a later round
// re-drives the operation it finds rather than opening a second.
func restartCloseOperationFor(t *testing.T, tc *testController, paneID string) app.Operation {
	t.Helper()
	var found []app.Operation
	for id := range tc.Store.Operations {
		op := tc.Store.Operations[id]
		intent, ok := app.DecodePaneCloseIntentForTest(op.Intent)
		if ok && intent.PaneID == paneID && intent.Reason == app.CloseReasonRestartForTest {
			found = append(found, op)
		}
	}
	if len(found) != 1 {
		t.Fatalf("restart close operations for pane %s = %d, want exactly 1", paneID, len(found))
	}
	return found[0]
}

// labelAnswersOnceThenFails scripts the creation-label lookup answering the
// recorded pane for the ladder's own lookup and then FAILING: a confirming
// read that could not be made at all, which is never absence. It is how a
// DISPATCHED close is held outstanding honestly — after a close that landed
// a real label resolves nothing (test/integration's
// TestSpikeVanishedPaneShapes pins that), so a fixture whose label kept
// answering for a closed pane would reproduce a world that cannot occur.
func labelAnswersOnceThenFails(tc *testController, binding run.RuntimeBinding) { //nolint:gocritic // hugeParam: the fixture binding is passed once per case.
	answered := 0
	tc.Runtime.FindPaneByLabelFn = func(asked string) (app.PaneRef, bool, error) {
		if asked != binding.CreationLabel {
			return app.PaneRef{}, false, nil
		}
		answered++
		if answered == 1 {
			return app.PaneRef{WorkspaceID: "workspace-1", TabID: "tab-1", PaneID: binding.PaneID}, true, nil
		}
		return app.PaneRef{}, false, errFakeLookup
	}
	tc.Runtime.InspectPaneFn = paneAnswersNothing
}

// TestReconcileServerRestartFailsTheRoundOnACloseError pins that a close
// this rule cannot carry out stops the round: the error is reported, no pane
// is closed, and nothing about the session is concluded. The alternative —
// letting the act's failure pass — would leave the journal saying a close is
// under way over a pane that still holds the agent Herdr restored into it.
func TestReconcileServerRestartFailsTheRoundOnACloseError(t *testing.T) {
	tc, fr, w, binding := restartWorker(t)
	serverRestarted(tc)
	identifiedByLabel(tc, binding)
	tc.Runtime.ClosePaneErr = errFakeLookup

	report, err := tc.Controller.ReconcileServerRestart(context.Background(), fr.Handle, restartOptions())
	if err == nil {
		t.Fatalf("ReconcileServerRestart() = %+v, want the close error", report)
	}
	if len(tc.Runtime.ClosedPanes) != 0 {
		t.Fatalf("panes closed = %v, want none: the close failed", tc.Runtime.ClosedPanes)
	}
	if len(report.Closed) != 0 || len(report.Relaunched) != 0 {
		t.Fatalf("report = %+v, want nothing concluded", report)
	}
	if got := tc.Store.Sessions[w.SessionID].value.State; got != run.SessionActive {
		t.Fatalf("session state = %s, want it untouched", got)
	}
	if evidence := restartCloseOperationFor(t, tc, binding.PaneID).ActEvidence; evidence != nil {
		t.Fatalf("the close records act evidence %v, want none: nothing was dispatched", evidence)
	}
}

// TestReconcileServerRestartRedrivesAnUnfinishedClose pins the recovery the
// journaled intent promises: a close whose ACT did not land is re-driven
// against its persisted target on the next round, and one pane never carries
// two close operations. Without it a single failed act — a transport error,
// which is exactly the state a just-restarted server can be in, or a lease
// fenced between the intent and the act — would leave the pane, and the agent
// Herdr restored into it, alive for the rest of the run while every later
// round reported the pane closed.
func TestReconcileServerRestartRedrivesAnUnfinishedClose(t *testing.T) {
	tc, fr, w, binding := restartWorker(t)
	serverRestarted(tc)
	identifiedByLabel(tc, binding)

	// Round 1: the close is authorized and journaled; the act fails.
	tc.Runtime.ClosePaneErr = errFakeLookup
	if _, err := tc.Controller.ReconcileServerRestart(context.Background(), fr.Handle, restartOptions()); err == nil {
		t.Fatal("round 1: ReconcileServerRestart() succeeded, want the close error")
	}
	opID := restartCloseOperationFor(t, tc, binding.PaneID).ID

	// Round 2: the runtime answers again, and the same operation is acted.
	tc.Runtime.ClosePaneErr = nil
	report, err := tc.Controller.ReconcileServerRestart(context.Background(), fr.Handle, restartOptions())
	if err != nil {
		t.Fatalf("round 2: ReconcileServerRestart() error = %v", err)
	}
	if !slices.Contains(tc.Runtime.ClosedPanes, binding.PaneID) {
		t.Fatalf("panes closed = %v, want the recorded pane %s re-driven", tc.Runtime.ClosedPanes, binding.PaneID)
	}
	if !slices.Contains(report.Closed, w.SessionID.String()) {
		t.Fatalf("report = %+v, want the session reported closed", report)
	}
	if got := restartCloseOperationFor(t, tc, binding.PaneID).ID; got != opID {
		t.Fatalf("the close settled as operation %s, want the journaled %s: one pane never carries two closes", got, opID)
	}
	if got := tc.Store.Sessions[w.SessionID].value.State; got != run.SessionLost {
		t.Fatalf("session state = %s, want lost: the close concluded and the session was relaunched", got)
	}
}

// TestReconcileServerRestartToleratesAPaneGoneAtTheClose pins the SECOND of
// the two not-founds. A pane that answers pane_not_found at the close went
// away between the identification and the act — and it went away as ours,
// because identification already succeeded — so the close is not an error
// and the absence observation settles it. This is the not-found the ladder's
// own refusal must never be confused with.
func TestReconcileServerRestartToleratesAPaneGoneAtTheClose(t *testing.T) {
	tc, fr, w, binding := restartWorker(t)
	serverRestarted(tc)
	tc.Runtime.InspectPaneFn = paneAnswersNothing
	// The label answers the ladder's lookup and nothing afterwards: the pane
	// vanished between being identified and being closed.
	answered := 0
	tc.Runtime.FindPaneByLabelFn = func(asked string) (app.PaneRef, bool, error) {
		if asked != binding.CreationLabel {
			return app.PaneRef{}, false, nil
		}
		answered++
		return app.PaneRef{WorkspaceID: "workspace-1", TabID: "tab-1", PaneID: binding.PaneID}, answered == 1, nil
	}
	tc.Runtime.ClosePaneErr = pinnedPaneNotFound("close", binding.PaneID)

	report, err := tc.Controller.ReconcileServerRestart(context.Background(), fr.Handle, restartOptions())
	if err != nil {
		t.Fatalf("ReconcileServerRestart() error = %v, want the close's pane_not_found tolerated", err)
	}
	if !slices.Contains(report.Closed, w.SessionID.String()) {
		t.Fatalf("report = %+v, want the session reported closed: its absence was observed", report)
	}
	if got := tc.Store.Sessions[w.SessionID].value.State; got != run.SessionLost {
		t.Fatalf("session state = %s, want lost: the close concluded and the session was relaunched", got)
	}
}

// TestReconcileServerRestartAppliesNoDispositionUntilAbsenceIsObserved pins
// that a DISPATCHED close is never termination: until the pane is observed
// absent nothing is retired, relaunched or settled, and the operation stays
// unresolved for a later round — carrying the act evidence that tells that
// later round this close was carried out.
func TestReconcileServerRestartAppliesNoDispositionUntilAbsenceIsObserved(t *testing.T) {
	tc, fr, w, binding := restartWorker(t)
	serverRestarted(tc)
	labelAnswersOnceThenFails(tc, binding)

	report, err := tc.Controller.ReconcileServerRestart(context.Background(), fr.Handle, restartOptions())
	if err != nil {
		t.Fatalf("ReconcileServerRestart() error = %v", err)
	}
	if !slices.Contains(tc.Runtime.ClosedPanes, binding.PaneID) {
		t.Fatalf("panes closed = %v, want the identified pane %s", tc.Runtime.ClosedPanes, binding.PaneID)
	}
	if len(report.Closed) != 0 || len(report.Relaunched) != 0 {
		t.Fatalf("report = %+v, want nothing concluded from a dispatch alone", report)
	}
	if got := tc.Store.Sessions[w.SessionID].value.State; got != run.SessionActive {
		t.Fatalf("session state = %s, want it untouched until absence is observed", got)
	}
	op := restartCloseOperationFor(t, tc, binding.PaneID)
	if op.State != app.OperationPending {
		t.Fatalf("the close operation is %s, want it unresolved for a later round", op.State)
	}
	if op.ActEvidence == nil {
		t.Fatal("the dispatched close records no act evidence; a later round cannot tell it was carried out")
	}
	if !slices.ContainsFunc(report.Outstanding, func(entry string) bool {
		return strings.Contains(entry, "the recorded pane was closed after the server restart, but its absence is not yet established")
	}) {
		t.Fatalf("outstanding = %q, want the dispatched close awaiting its observed absence", report.Outstanding)
	}
}

// TestReconcileServerRestartDoesNotRedriveUnderAnotherLifetime pins the
// bound on the re-drive. A recorded pane id is a durable address only WITHIN
// one server lifetime; across a restart workspace ids are reissued and the
// composed id can answer for a stranger's pane. So once the lifetime that
// authorized a close no longer serves the socket, the close is not acted
// again — and the round reports what actually happened to the pane, which is
// what the act evidence is for.
func TestReconcileServerRestartDoesNotRedriveUnderAnotherLifetime(t *testing.T) {
	for _, tc := range []struct {
		name string
		// firstAct scripts the first round's close.
		firstAct func(*testController, run.RuntimeBinding)
		// wantClosed is how many closes the pane received in round 1.
		wantClosed int
		// wantDetail is what the second round must tell the human about the
		// pane the close named.
		wantDetail string
	}{
		{
			name:       "the close landed and is awaiting its observed absence",
			firstAct:   func(*testController, run.RuntimeBinding) {},
			wantClosed: 1,
			wantDetail: "the recorded pane was closed after the server restart",
		},
		{
			name: "the close never landed",
			firstAct: func(controller *testController, _ run.RuntimeBinding) {
				controller.Runtime.ClosePaneErr = errFakeLookup
			},
			wantClosed: 0,
			wantDetail: "journaled but not yet carried out",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller, fr, _, binding := restartWorker(t)
			serverRestarted(controller)
			labelAnswersOnceThenFails(controller, binding)
			tc.firstAct(controller, binding)

			// Round 1 journals the close; its act lands or does not.
			_, err := controller.Controller.ReconcileServerRestart(context.Background(), fr.Handle, restartOptions())
			if (err != nil) != (tc.wantClosed == 0) {
				t.Fatalf("round 1: ReconcileServerRestart() error = %v", err)
			}
			if len(controller.Runtime.ClosedPanes) != tc.wantClosed {
				t.Fatalf("round 1 closed %v, want %d close(s)", controller.Runtime.ClosedPanes, tc.wantClosed)
			}

			// A third lifetime now serves the socket.
			controller.Runtime.ClosePaneErr = nil
			controller.Runtime.ServerInstanceValue = fakeServerToken(3)
			report, err := controller.Controller.ReconcileServerRestart(context.Background(), fr.Handle, restartOptions())
			if err != nil {
				t.Fatalf("round 2: ReconcileServerRestart() error = %v", err)
			}
			if len(controller.Runtime.ClosedPanes) != tc.wantClosed {
				t.Fatalf("round 2 closed %v, want the recorded pane untouched under a lifetime that did not authorize the close", controller.Runtime.ClosedPanes)
			}
			for _, want := range []string{tc.wantDetail, "the server lifetime changed again after this close was authorized"} {
				if !slices.ContainsFunc(report.Outstanding, func(entry string) bool { return strings.Contains(entry, want) }) {
					t.Fatalf("outstanding = %q, want an entry carrying %q", report.Outstanding, want)
				}
			}
		})
	}
}
