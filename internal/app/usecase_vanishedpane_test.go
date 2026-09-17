package app_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/johnlanda/hop/internal/app"
	"github.com/johnlanda/hop/internal/domain/run"
)

// TestSchedulingPassSurvivesAVanishedPane proves the scheduling pass's
// app steps, run in the loop's order against a pane that vanished before
// its launch settled, never fail: presentation skips the vanished pane,
// corroboration settles the launch, and the next pass publishes only what
// is left.
func TestSchedulingPassSurvivesAVanishedPane(t *testing.T) {
	tc := newTestController(defaultPolicy())
	fr := seedFeatureRun(t, tc, 2)
	presenter := &runStatePresentation{store: tc.Store}
	tc.Controller.Presentation = presenter
	taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
	child := seedLaunchingChild(t, tc, fr, taskID, app.LaunchClaimExecPending)
	presenter.Vanished = map[string]bool{child.PaneID: true}
	// The worker is still alive when this pass's corroboration looks, and
	// its pane vanishes before the pass publishes.
	tc.Groups.liveLeader(childLaunchPID, "/usr/local/bin/claude")
	tc.Runtime.InspectPaneFn = vanishedPane(child.PaneID, liveManagerOnly(fr, 900))

	pass := func(round int) app.PresentationReport {
		t.Helper()
		if _, err := tc.Controller.RetireSettledSessions(context.Background(), fr.Handle); err != nil {
			t.Fatalf("round %d: RetireSettledSessions() error = %v", round, err)
		}
		if _, err := tc.Controller.CorroborateSessionLaunches(context.Background(), fr.Handle); err != nil {
			t.Fatalf("round %d: CorroborateSessionLaunches() error = %v", round, err)
		}
		report, err := tc.Controller.PublishRunPresentation(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("round %d: PublishRunPresentation() error = %v", round, err)
		}
		return report
	}

	first := pass(1)
	if !slices.Equal(first.Skipped, []string{child.PaneID}) || !slices.Equal(first.Published, []string{"pane-mgr"}) {
		t.Fatalf("round 1 presentation = %+v, want the manager published and the vanished pane skipped", first)
	}
	requireChildUnsettled(t, tc, fr.RunID, &child)

	// The process is gone by the next pass.
	tc.Groups.Processes[childLaunchPID] = nil
	second := pass(2)
	if len(second.Skipped) != 0 || !slices.Equal(second.Published, []string{"pane-mgr"}) {
		t.Fatalf("round 2 presentation = %+v, want only the manager", second)
	}
	if got := tc.Store.Tasks[taskID].value.State; got != run.TaskNeedsRework {
		t.Errorf("task state = %s, want needs-rework", got)
	}
}

// TestPublishRunPresentationToleratesOnlyNotFound proves presentation
// skips a pane the server reports as not found and still fails on any
// other error.
func TestPublishRunPresentationToleratesOnlyNotFound(t *testing.T) {
	t.Run("not found: skipped, the rest published", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		presenter := &runStatePresentation{store: tc.Store, Vanished: map[string]bool{"pane-mgr": true}}
		tc.Controller.Presentation = presenter
		taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
		w := seedSettledWorker(t, tc, fr, taskID, 4711)
		transitions := len(tc.Store.Transitions)
		operations := len(tc.Store.Operations)

		report, err := tc.Controller.PublishRunPresentation(context.Background(), fr.Handle)
		if err != nil {
			t.Fatalf("PublishRunPresentation() error = %v", err)
		}
		if !slices.Equal(report.Skipped, []string{"pane-mgr"}) || !slices.Equal(report.Published, []string{w.PaneID}) {
			t.Fatalf("report = %+v, want the manager skipped and the worker published", report)
		}
		if got := tc.Store.Sessions[fr.ManagerID].value.State; got != run.SessionActive {
			t.Errorf("manager state = %s, want active: presentation records nothing", got)
		}
		if len(tc.Store.Transitions) != transitions || len(tc.Store.Operations) != operations {
			t.Errorf("transitions %d -> %d, operations %d -> %d; want nothing recorded", transitions, len(tc.Store.Transitions), operations, len(tc.Store.Operations))
		}
	})

	t.Run("any other error is returned", func(t *testing.T) {
		tc := newTestController(defaultPolicy())
		fr := seedFeatureRun(t, tc, 2)
		presenter := &runStatePresentation{store: tc.Store, Err: errors.New("report metadata for pane pane-mgr: herdr: invalid_metadata_token: bad")}
		tc.Controller.Presentation = presenter

		if _, err := tc.Controller.PublishRunPresentation(context.Background(), fr.Handle); err == nil || errors.Is(err, app.ErrPaneNotFound) {
			t.Fatalf("PublishRunPresentation() error = %v, want the metadata error returned", err)
		}
	})
}

// TestClosePaneNotFoundReobserves proves a pane that vanishes between the
// close rule's re-inspection and the close itself is not an error: nothing
// was dispatched, and the immediate re-observation decides.
func TestClosePaneNotFoundReobserves(t *testing.T) {
	for _, tt := range []struct {
		name         string
		labelAnswers bool
		wantRetired  bool
	}{
		{name: "absence observed: retired", wantRetired: true},
		{name: "absence not yet established: awaiting", labelAnswers: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tc := newTestController(defaultPolicy())
			fr := seedFeatureRun(t, tc, 2)
			taskID := seedImplementTask(t, tc, fr.RunID, 1, "A", false, run.TaskReady)
			w := seedSettledWorker(t, tc, fr, taskID, 4711)
			rRow := tc.Store.Runs[fr.RunID]
			rRow.value = rRow.value.RequestStop(tc.Clock.Now())
			rRow.revision++

			inspections := 0
			occupant := app.PaneProcess{
				ShellPID: w.PID, ForegroundGroupID: w.PID,
				Foreground: []app.ProcessInfo{{PID: w.PID, Argv: []string{"/usr/local/bin/claude"}, Cmdline: "claude " + w.IncarnationID.String()}},
			}
			tc.Runtime.InspectPaneFn = func(id string) (app.PaneProcess, error) {
				switch id {
				case "pane-mgr":
					return liveManagerOnly(fr, 900)(id)
				case w.PaneID:
					inspections++
					if inspections == 1 {
						return occupant, nil
					}
				}
				return app.PaneProcess{}, pinnedPaneNotFound("inspect", id)
			}
			tc.Runtime.FindPaneByLabelFn = func(label string) (app.PaneRef, bool, error) {
				if tt.labelAnswers && label != "label-mgr" {
					return app.PaneRef{PaneID: w.PaneID}, true, nil
				}
				return app.PaneRef{}, false, nil
			}
			tc.Runtime.ClosePaneErr = pinnedPaneNotFound("close", w.PaneID)

			report, err := tc.Controller.DriveFeatureStop(context.Background(), fr.Handle)
			if err != nil {
				t.Fatalf("DriveFeatureStop() error = %v", err)
			}
			retired := tc.Store.Sessions[w.SessionID].value.State == run.SessionTerminated
			if retired != tt.wantRetired {
				t.Fatalf("worker retired = %t, want %t; report = %+v", retired, tt.wantRetired, report)
			}
			op := paneCloseOperationFor(t, tc, fr.RunID, w.PaneID)
			if op.ActEvidence != nil {
				t.Errorf("act evidence = %v, want no dispatch recorded for a close that found no pane", op.ActEvidence)
			}
			if !tt.wantRetired {
				want := "session " + w.SessionID.String() + ": the pane was already gone at close; awaiting observed absence"
				if !slices.Contains(report.Outstanding, want) {
					t.Errorf("outstanding = %q, want %q", report.Outstanding, want)
				}
				if got := tc.Store.Sessions[w.SessionID].value.State; got != run.SessionActive {
					t.Errorf("worker state = %s, want active: no interrupt was dispatched", got)
				}
			}
		})
	}
}
