package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

// viewEnv is hop view's own environment: a controller command (like hop
// status/stop/resume), never a worker context, so it only needs
// HOP_STATE_DIR resolved through the controller state-root rule.
func viewEnv() map[string]string {
	return map[string]string{"HOP_STATE_DIR": "/state/root"}
}

func TestRunViewSet(t *testing.T) {
	t.Run("a run-id argument resolves the r<seq> label and selects it", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.status = scriptStatus(detailStep("running", "", false))
		var gotLabel string
		ctrl.selectRunView = func(label string) error {
			gotLabel = label
			return nil
		}
		td := newTestDeps(ctrl, viewEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runView([]string{"set", "--run", testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK || stdout.String() != "view set: r1\n" {
			t.Errorf("code = %d, stdout = %q (stderr: %s)", code, stdout.String(), stderr.String())
		}
		if gotLabel != "r1" {
			t.Errorf("SelectRunView label = %q, want %q", gotLabel, "r1")
		}
	})

	t.Run("an r<seq> label argument resolves through the run listing first", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.status = func(req app.StatusRequest) (app.StatusResult, error) {
			if req.RepositoryRoot != "" {
				// resolveRunArg's own listing lookup for the r<seq> label.
				return app.StatusResult{Runs: []app.RunSummaryView{{RunID: testRunID, Sequence: 1}}}, nil
			}
			// runLabel's own detail-based lookup, by the resolved run id.
			if req.RunID != testRunID {
				t.Errorf("runLabel resolved to run id = %q, want %q", req.RunID, testRunID)
			}
			return detailStep("running", "", false), nil
		}
		var gotLabel string
		ctrl.selectRunView = func(label string) error {
			gotLabel = label
			return nil
		}
		td := newTestDeps(ctrl, viewEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runView([]string{"set", "--run", "r1"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK || stdout.String() != "view set: r1\n" {
			t.Errorf("code = %d, stdout = %q (stderr: %s)", code, stdout.String(), stderr.String())
		}
		if gotLabel != "r1" {
			t.Errorf("SelectRunView label = %q, want %q", gotLabel, "r1")
		}
	})

	t.Run("a SelectRunView failure is a command failure", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.status = scriptStatus(detailStep("running", "", false))
		ctrl.selectRunView = func(string) error { return errors.New("boom") }
		td := newTestDeps(ctrl, viewEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runView([]string{"set", "--run", testRunID}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
	})

	t.Run("missing --run is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, viewEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runView([]string{"set"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})

	t.Run("unexpected argument is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, viewEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runView([]string{"set", "--run", testRunID, "extra"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})
}

func TestRunViewClear(t *testing.T) {
	t.Run("clears the view", func(t *testing.T) {
		ctrl := &fakeController{}
		cleared := false
		ctrl.clearRunView = func() error {
			cleared = true
			return nil
		}
		td := newTestDeps(ctrl, viewEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runView([]string{"clear"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitOK || stdout.String() != "view cleared\n" {
			t.Errorf("code = %d, stdout = %q (stderr: %s)", code, stdout.String(), stderr.String())
		}
		if !cleared {
			t.Errorf("ClearRunView was not called")
		}
	})

	t.Run("a ClearRunView failure is a command failure", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.clearRunView = func() error { return errors.New("boom") }
		td := newTestDeps(ctrl, viewEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runView([]string{"clear"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitFailure {
			t.Errorf("exit code = %d, want %d", code, exitFailure)
		}
	})

	t.Run("unexpected argument is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, viewEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runView([]string{"clear", "extra"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})
}

func TestRunViewDispatch(t *testing.T) {
	t.Run("no subcommand is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, viewEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runView(nil, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})

	t.Run("an unknown subcommand is a usage error", func(t *testing.T) {
		td := newTestDeps(&fakeController{}, viewEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer

		code, err := runView([]string{"bogus"}, &stdout, &stderr, td.deps)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}
		if code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})
}
