package main

import (
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

// launchCorroborationActionLine is the exact nested action line `hop
// status -run` renders under a session reconciling because another
// process on its pane carries the launch identity: the detail block's own
// four-space indent, the shared action word, and the value-free action.
func launchCorroborationActionLine() string {
	return "\n    " + app.GrammarActionPrefix + "      " + app.GrammarSessionLaunchCorroborationAction + "\n"
}

// TestGrammarContractStatusLaunchCorroborationAction drives the session
// action line through the built binary against the real store and read
// model. The line is owed to exactly one structural shape — a reconciling
// session with a current unsuperseded placed binding whose own
// incarnation's launch claim is still exec_pending — so each state around
// that shape is rendered and checked in turn:
//
//   - launching with an exec_pending claim: no reconciliation yet, so no
//     action is owed;
//   - reconciling with that same exec_pending claim: the action renders,
//     immediately under that session's own line;
//   - reconciling with the claim SETTLED (what every resume path leaves):
//     no action, since nothing re-inspects it and the wrapper is not what
//     put it there;
//   - active after settlement: no action.
func TestGrammarContractStatusLaunchCorroborationAction(t *testing.T) {
	t.Run("launching with an unsettled claim renders no action", func(t *testing.T) {
		f := newLaunchingFeature(t, 9300)
		out := f.statusDetail(t)
		requireSessionState(t, out, f.base.ManagerID, "launching")
		if strings.Contains(out, app.GrammarSessionLaunchCorroborationAction) {
			t.Fatalf("status -run renders the launch-corroboration action for a launching session:\n%s", out)
		}
	})

	t.Run("reconciling with an unsettled claim renders the action under its session line", func(t *testing.T) {
		f := newLaunchingFeature(t, 9310)
		f.reconcile(t)
		out := f.statusDetail(t)
		requireSessionState(t, out, f.base.ManagerID, "reconciling")
		if want := launchCorroborationActionLine(); !strings.Contains(out, want) {
			t.Fatalf("status -run output missing the action line %q; got:\n%s", want, out)
		}
		// Nested under THAT session's line, not floating elsewhere in the
		// block.
		sessionLine := "\n  session " + f.base.ManagerID + ":"
		idx := strings.Index(out, sessionLine)
		if idx < 0 {
			t.Fatalf("status -run output has no session line for %s:\n%s", f.base.ManagerID, out)
		}
		rest := out[idx+1:]
		lines := strings.SplitN(rest, "\n", 3)
		if len(lines) < 2 || lines[1] != "    "+app.GrammarActionPrefix+"      "+app.GrammarSessionLaunchCorroborationAction {
			t.Fatalf("the action line does not immediately follow the session line; got:\n%s", rest)
		}
	})

	t.Run("reconciling with a settled claim renders no action", func(t *testing.T) {
		f := newLaunchingFeature(t, 9320)
		f.settle(t)
		f.reconcile(t)
		out := f.statusDetail(t)
		requireSessionState(t, out, f.base.ManagerID, "reconciling")
		if strings.Contains(out, app.GrammarSessionLaunchCorroborationAction) {
			t.Fatalf("status -run renders the launch-corroboration action for a reconciling session whose claim is already settled:\n%s", out)
		}
	})

	t.Run("active after settlement renders no action", func(t *testing.T) {
		f := newLaunchingFeature(t, 9330)
		f.settle(t)
		out := f.statusDetail(t)
		requireSessionState(t, out, f.base.ManagerID, "active")
		if strings.Contains(out, app.GrammarSessionLaunchCorroborationAction) {
			t.Fatalf("status -run renders the launch-corroboration action for an active session:\n%s", out)
		}
	})
}

// requireSessionState asserts the rendered session line reports state for
// sessionID, so each case above is pinned to the state it meant to render.
func requireSessionState(t *testing.T, out, sessionID, state string) {
	t.Helper()
	if want := "\n  session " + sessionID + ": role=manager state=" + state + " "; !strings.Contains(out, want) {
		t.Fatalf("status -run output missing %q; got:\n%s", want, out)
	}
}
