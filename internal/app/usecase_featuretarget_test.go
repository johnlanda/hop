package app_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

// isRootHeadSymbolicRef reports whether cmd is the freeze-time target read:
// `git -C <root> symbolic-ref -q HEAD`.
func isRootHeadSymbolicRef(cmd app.Command) bool {
	return slices.Equal(cmd.Argv, []string{featureGit, "-C", featureRepo, "symbolic-ref", "-q", "HEAD"})
}

// TestStartFeatureRunFreezesTargetBranch covers the worktree-retirement
// target (docs/plan/phase-3-worktree-retirement.md section 2): the branch
// checked out at the repository root is frozen into the workflow snapshot
// as its full ref, a detached HEAD freezes no target and still starts,
// and every other answer refuses the start before any side effect with a
// value-free reason.
func TestStartFeatureRunFreezesTargetBranch(t *testing.T) {
	frozenTarget := func(t *testing.T, f *featureStart) string {
		t.Helper()
		frozen, err := f.tc.Store.LoadFrozenRun(context.Background(), f.runID())
		if err != nil {
			t.Fatalf("LoadFrozenRun: %v", err)
		}
		return frozen.Snapshot.Workflow.TargetBranch
	}

	t.Run("the root's checked-out branch is frozen as its full ref", func(t *testing.T) {
		f := newFeatureStart(t)
		f.git.setSymref("HEAD", "refs/heads/main")
		var initialized string
		f.store.before = func(_ int, spec *app.NewRunSpec) { initialized = spec.Snapshot.Workflow.TargetBranch }
		f.mustStart()
		if initialized != "refs/heads/main" {
			t.Errorf("InitializeRun received TargetBranch %q, want refs/heads/main", initialized)
		}
		if got := frozenTarget(t, f); got != "refs/heads/main" {
			t.Errorf("frozen TargetBranch = %q, want refs/heads/main", got)
		}
		status, err := f.tc.Controller.Status(context.Background(), app.StatusRequest{RunID: f.runID().String()})
		if err != nil || status.Detail == nil {
			t.Fatalf("Status() = %+v, %v", status, err)
		}
		if status.Detail.TargetBranch != "refs/heads/main" || status.Detail.WorktreesRetiredAt != nil {
			t.Errorf("status detail target %q retired %v, want refs/heads/main and not retired", status.Detail.TargetBranch, status.Detail.WorktreesRetiredAt)
		}
	})

	t.Run("a nested branch name is kept whole", func(t *testing.T) {
		f := newFeatureStart(t)
		f.git.setSymref("HEAD", "refs/heads/hop/r7/integration")
		f.mustStart()
		if got := frozenTarget(t, f); got != "refs/heads/hop/r7/integration" {
			t.Errorf("frozen TargetBranch = %q, want the full nested ref", got)
		}
	})

	t.Run("a detached HEAD freezes no target and the run still starts", func(t *testing.T) {
		f := newFeatureStart(t)
		var reads int
		f.onGit = func(cmd app.Command) (app.CommandResult, bool, error) {
			if isRootHeadSymbolicRef(cmd) {
				reads++
			}
			return app.CommandResult{}, false, nil
		}
		f.mustStart()
		if reads != 1 {
			t.Errorf("symbolic-ref HEAD reads = %d, want exactly one before the freeze", reads)
		}
		if got := frozenTarget(t, f); got != "" {
			t.Errorf("frozen TargetBranch = %q, want empty for a detached HEAD", got)
		}
	})

	refusals := []struct {
		name   string
		answer func() (app.CommandResult, error)
	}{
		{"git fails", func() (app.CommandResult, error) {
			return app.CommandResult{ExitCode: 128, Stderr: []byte("fatal: not a git repository: /repo/.git")}, nil
		}},
		{"the runner fails", func() (app.CommandResult, error) {
			return app.CommandResult{}, errors.New("fork/exec /usr/bin/git: resource temporarily unavailable")
		}},
		{"HEAD names a ref outside refs/heads", func() (app.CommandResult, error) {
			return app.CommandResult{ExitCode: 0, Stdout: []byte("refs/remotes/origin/main\n")}, nil
		}},
		{"a detached answer with output", func() (app.CommandResult, error) {
			return app.CommandResult{ExitCode: 1, Stdout: []byte("/repo\n")}, nil
		}},
	}
	for _, tc := range refusals {
		t.Run("refused before any side effect: "+tc.name, func(t *testing.T) {
			f := newFeatureStart(t)
			f.onGit = func(cmd app.Command) (app.CommandResult, bool, error) {
				if isRootHeadSymbolicRef(cmd) {
					result, err := tc.answer()
					return result, true, err
				}
				return app.CommandResult{}, false, nil
			}
			_, _, err := f.start()
			if !errors.Is(err, app.ErrStartRefused) {
				t.Fatalf("StartFeatureRun() error = %v, want ErrStartRefused", err)
			}
			if !strings.Contains(err.Error(), "checked-out branch could not be read") {
				t.Errorf("refusal %q does not name the unreadable branch", err)
			}
			for _, leak := range []string{"/repo", "/usr/bin/git", "origin", "resource temporarily"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("refusal %q echoes %q", err, leak)
				}
			}
			if f.store.calls != 0 || len(f.eventLog()) != 0 {
				t.Errorf("the refusal reached InitializeRun (%d calls) or an act (%v)", f.store.calls, f.eventLog())
			}
		})
	}
}
