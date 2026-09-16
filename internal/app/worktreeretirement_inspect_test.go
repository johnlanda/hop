package app_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

const (
	inspectRoot      = "/repo"
	inspectCommon    = "/repo/.git"
	inspectCanonical = "/private/var/wt/hop-r1-t1a1"
	// inspectRecorded is the non-canonical spelling Herdr records (pinned:
	// worktree.create reports the requested spelling).
	inspectRecorded = "/var/wt/hop-r1-t1a1"
	inspectBranch   = "refs/heads/hop/r1/t1a1"
	// inspectGitignoreTag is the modeled index's first entry.
	inspectGitignoreTag = "H .gitignore\x00"
	// listingBound is the capture bound of the index scan and the worktree
	// listing.
	listingBound = app.RetirementListingOutputBytesForTest
)

// inspectFixture is a test controller whose git is the fake repository with
// its linked attempt-worktree model installed.
type inspectFixture struct {
	t          *testing.T
	tc         *testController
	git        *fakeGitRepo
	base, head string
	// present are extra paths the path inspector reports as existing.
	present map[string]bool
	// failPath makes the path inspector fail for that path.
	failPath string
}

func newInspectFixture(t *testing.T) *inspectFixture {
	t.Helper()
	tc := newTestController(defaultPolicy())
	git := newFakeGitRepo(tc.Controller.GitExecutable, "/usr/local/bin/hop")
	git.setAttemptRepository(inspectRoot, inspectCommon)
	base := git.newCommit("tree-base")
	head := git.newCommit("tree-head", base)
	tc.Commands.RunHook = git.Hook
	git.setFsmonitorHook(true)
	t.Cleanup(func() {
		git.requireNoForcedAttemptRemovals(t)
		git.requireNoFsmonitorRuns(t)
	})
	return &inspectFixture{t: t, tc: tc, git: git, base: base, head: head, present: map[string]bool{inspectRoot: true, inspectCommon: true}}
}

// inspector canonicalizes /var/ to /private/var/ (the macOS symlink) and
// reports existence from the model and the fixture's extra paths.
func (f *inspectFixture) inspector() app.PathInspector {
	return func(path string) (string, bool, error) {
		if path == f.failPath {
			return "", false, errors.New("lstat " + path + ": permission denied")
		}
		canonical := path
		if rest, ok := strings.CutPrefix(path, "/var/"); ok {
			canonical = "/private/var/" + rest
		}
		if w, modeled := f.git.attemptWorktree(canonical); modeled {
			return canonical, w.Present, nil
		}
		return canonical, f.present[canonical], nil
	}
}

func (f *inspectFixture) inspect(branch string) app.CheckoutVerdictForTest {
	return app.InspectAttemptCheckoutForTest(context.Background(), f.tc.Controller, inspectRoot, inspectRecorded, branch, f.base, f.inspector())
}

// TestInspectAttemptCheckout drives the section 4 pre-act inspection
// through every decision against the fake git model that reproduces the
// executed probe: nothing it observes is ever removed, and every git read
// runs the absolute git under the retirement environment.
func TestInspectAttemptCheckout(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *inspectFixture)
		want  app.CheckoutVerdictForTest
	}{
		{
			name:  "clean, recorded in a non-canonical spelling: removable under git's canonical path",
			setup: func(f *inspectFixture) { f.addCheckout(fakeAttemptWorktree{}) },
			want:  app.CheckoutVerdictForTest{Disposition: "removable", ListedPath: inspectCanonical},
		},
		{
			name:  "ignored files alone do not block removal (approved)",
			setup: func(f *inspectFixture) { f.addCheckout(fakeAttemptWorktree{Ignored: true}) },
			want:  app.CheckoutVerdictForTest{Disposition: "removable", ListedPath: inspectCanonical},
		},
		{
			name:  "untracked file",
			setup: func(f *inspectFixture) { f.addCheckout(fakeAttemptWorktree{Untracked: true}) },
			want:  retained(app.RetainedUncommittedChanges),
		},
		{
			name: "untracked file hidden from git's own check by status.showUntrackedFiles=no",
			setup: func(f *inspectFixture) {
				f.git.setHideUntracked(true)
				f.addCheckout(fakeAttemptWorktree{Untracked: true})
			},
			want: retained(app.RetainedUncommittedChanges),
		},
		{
			name:  "modified tracked file",
			setup: func(f *inspectFixture) { f.addCheckout(fakeAttemptWorktree{Modified: true}) },
			want:  retained(app.RetainedUncommittedChanges),
		},
		{
			name:  "staged file",
			setup: func(f *inspectFixture) { f.addCheckout(fakeAttemptWorktree{Staged: true}) },
			want:  retained(app.RetainedUncommittedChanges),
		},
		{
			name:  "nested untracked repository",
			setup: func(f *inspectFixture) { f.addCheckout(fakeAttemptWorktree{NestedRepo: true}) },
			want:  retained(app.RetainedUncommittedChanges),
		},
		{
			name:  "modification hidden by assume-unchanged",
			setup: func(f *inspectFixture) { f.addCheckout(fakeAttemptWorktree{AssumeUnchanged: true}) },
			want:  retained(app.RetainedHiddenChanges),
		},
		{
			name:  "modification hidden by skip-worktree",
			setup: func(f *inspectFixture) { f.addCheckout(fakeAttemptWorktree{SkipWorktree: true}) },
			want:  retained(app.RetainedHiddenChanges),
		},
		{
			name:  "locked",
			setup: func(f *inspectFixture) { f.addCheckout(fakeAttemptWorktree{Locked: true, LockReason: "in use"}) },
			want:  retained(app.RetainedLocked),
		},
		{
			name: "locked and already gone stays locked",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{Locked: true})
				f.setPresent(false)
			},
			want: retained(app.RetainedLocked),
		},
		{
			name: "listed but gone: removable to prune the entry, outcome absent",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{})
				f.setPresent(false)
			},
			want: app.CheckoutVerdictForTest{Disposition: "removable", ListedPath: inspectCanonical, WasAbsent: true},
		},
		{
			name: "gone and no longer listed: absent",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{Unlisted: true})
				f.setPresent(false)
			},
			want: app.CheckoutVerdictForTest{Disposition: "absent"},
		},
		{
			name:  "present but not listed: released, never deleted",
			setup: func(f *inspectFixture) { f.addCheckout(fakeAttemptWorktree{Unlisted: true}) },
			want:  app.CheckoutVerdictForTest{Disposition: "released", Released: app.ReleasedNotRegistered},
		},
		{
			name: "detached HEAD: released (its commits would become unreachable)",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{})
				f.setBranch("")
			},
			want: released(app.ReleasedDetached),
		},
		{
			name: "switched to another branch: released",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{})
				f.setBranch("refs/heads/my-own-work")
			},
			want: released(app.ReleasedOtherBranch),
		},
		{
			name: "belongs to another repository: released",
			setup: func(f *inspectFixture) {
				f.present["/elsewhere/.git"] = true
				f.addCheckout(fakeAttemptWorktree{CommonDir: "/elsewhere/.git"})
			},
			want: released(app.ReleasedOtherRepository),
		},
		{
			name: "HEAD no longer descends from the recorded base: released",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{})
				f.setHead(f.git.newCommit("tree-unrelated"))
			},
			want: released(app.ReleasedBaseNotAncestor),
		},
		{
			name: "the worktree list cannot be read: retained",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{})
				f.failGit("worktree", app.CommandResult{ExitCode: 128, Stderr: []byte("fatal: not a git repository")}, nil)
			},
			want: app.CheckoutVerdictForTest{Disposition: "retained", Retained: app.RetainedInspectionFailed},
		},
		{
			name: "status cannot be run: retained",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{})
				f.failGit("status", app.CommandResult{}, errors.New("fork/exec: resource temporarily unavailable"))
			},
			want: retained(app.RetainedInspectionFailed),
		},
		{
			name: "an unparsable index listing: retained",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{})
				f.failGit("ls-files", app.CommandResult{Stdout: []byte("garbage\x00")}, nil)
			},
			want: retained(app.RetainedInspectionFailed),
		},
		{
			name: "the base check fails: retained",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{})
				f.failGit("merge-base", app.CommandResult{ExitCode: 128}, nil)
			},
			want: retained(app.RetainedInspectionFailed),
		},
		{
			name: "an assume-unchanged flag past the default capture bound, inside the index read's own bound: hidden changes",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{AssumeUnchanged: true, IndexPadding: fakeCaptureBytes - len(inspectGitignoreTag)})
			},
			want: retained(app.RetainedHiddenChanges),
		},
		{
			name: "a skip-worktree flag past the default capture bound, inside the index read's own bound: hidden changes",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{SkipWorktree: true, IndexPadding: 3 * fakeCaptureBytes})
			},
			want: retained(app.RetainedHiddenChanges),
		},
		{
			name: "an assume-unchanged flag past the index read's bound, the index cut on an entry boundary: retained",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{AssumeUnchanged: true, IndexPadding: listingBound - len(inspectGitignoreTag)})
			},
			want: retained(app.RetainedInspectionFailed),
		},
		{
			name: "a skip-worktree flag past the index read's bound, the index cut on an entry boundary: retained",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{SkipWorktree: true, IndexPadding: listingBound - len(inspectGitignoreTag)})
			},
			want: retained(app.RetainedInspectionFailed),
		},
		{
			name: "a hidden flag past the index read's bound, the index cut inside an entry: retained",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{SkipWorktree: true, IndexPadding: listingBound - len(inspectGitignoreTag) + 5})
			},
			want: retained(app.RetainedInspectionFailed),
		},
		{
			name: "a clean index past the index read's bound: retained, never removable on a prefix",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{IndexPadding: listingBound})
			},
			want: retained(app.RetainedInspectionFailed),
		},
		{
			name: "an index filling the index read's bound exactly still shows its hidden flag",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{AssumeUnchanged: true, IndexPadding: listingBound - len(inspectGitignoreTag+"h f.txt\x00H g.txt\x00")})
			},
			want: retained(app.RetainedHiddenChanges),
		},
		{
			name: "a clean index filling the index read's bound exactly: removable",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{IndexPadding: listingBound - len(inspectGitignoreTag+"H f.txt\x00H g.txt\x00")})
			},
			want: app.CheckoutVerdictForTest{Disposition: "removable", ListedPath: inspectCanonical},
		},
		{
			name: "an index read whose stderr was truncated: retained",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{})
				f.failGit("ls-files", app.CommandResult{Stdout: []byte(inspectGitignoreTag + "H f.txt\x00H g.txt\x00"), Stderr: make([]byte, listingBound+1)}, nil)
			},
			want: retained(app.RetainedInspectionFailed),
		},
		{
			name: "status output past the default capture bound, which status keeps: retained as uninspectable",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{})
				f.failGit("status", app.CommandResult{Stdout: []byte(strings.Repeat("?? new.txt\n", fakeCaptureBytes/10))}, nil)
			},
			want: retained(app.RetainedInspectionFailed),
		},
		{
			name: "a common-directory read past the default capture bound: retained, never another repository",
			setup: func(f *inspectFixture) {
				// The kept prefix names an existing other repository.
				f.present["/elsewhere/.git"] = true
				f.addCheckout(fakeAttemptWorktree{})
				f.tc.Commands.RunHook = func(ctx context.Context, cmd app.Command) (app.CommandResult, bool, error) {
					if slices.Contains(cmd.Argv, "--git-common-dir") && slices.Contains(cmd.Argv, inspectCanonical) {
						return app.CommandResult{Stdout: []byte("/elsewhere/.git\n" + strings.Repeat("\n", fakeCaptureBytes))}, true, nil
					}
					return f.git.Hook(ctx, cmd)
				}
			},
			want: retained(app.RetainedInspectionFailed),
		},
		{
			name: "a worktree list past the default capture bound, inside the listing's own bound: removable",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{})
				f.fillListingBeforeCandidate(fakeCaptureBytes)
			},
			want: app.CheckoutVerdictForTest{Disposition: "removable", ListedPath: inspectCanonical},
		},
		{
			name: "the worktree list cut on a record boundary before the candidate's record: retained, never released",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{})
				f.fillListingBeforeCandidate(listingBound)
			},
			want: app.CheckoutVerdictForTest{Disposition: "retained", Retained: app.RetainedInspectionFailed},
		},
		{
			name: "a gone checkout whose record lies past the listing's bound: retained, never absent",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{})
				f.fillListingBeforeCandidate(listingBound)
				f.mutate(func(w *fakeAttemptWorktree) { w.Present = false })
			},
			want: app.CheckoutVerdictForTest{Disposition: "retained", Retained: app.RetainedInspectionFailed},
		},
		{
			name: "a listing that fills its bound exactly, the candidate within it: removable",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{})
				f.fillListingBeforeCandidate(listingBound - f.candidateRecordBytes())
			},
			want: app.CheckoutVerdictForTest{Disposition: "removable", ListedPath: inspectCanonical},
		},
		{
			name: "the checkout path cannot be resolved: retained",
			setup: func(f *inspectFixture) {
				f.addCheckout(fakeAttemptWorktree{})
				f.failPath = inspectRecorded
			},
			want: app.CheckoutVerdictForTest{Disposition: "retained", Retained: app.RetainedInspectionFailed},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newInspectFixture(t)
			tc.setup(f)
			got := f.inspect(inspectBranch)
			got.Detail = ""
			if got != tc.want {
				t.Fatalf("verdict = %+v, want %+v", got, tc.want)
			}
			f.requireReadOnlyRetirementGit(t)
		})
	}

	t.Run("the repository root itself is never a candidate", func(t *testing.T) {
		f := newInspectFixture(t)
		got := app.InspectAttemptCheckoutForTest(context.Background(), f.tc.Controller, inspectRoot, inspectRoot, inspectBranch, f.base, f.inspector())
		if got.Disposition != "released" || got.Released != app.ReleasedRepositoryRoot {
			t.Fatalf("verdict = %+v, want released as the repository root", got)
		}
		if len(f.tc.Commands.Calls) != 0 {
			t.Fatalf("the root was inspected with git: %v", f.tc.Commands.Calls)
		}
	})

	t.Run("a missing path inspector or repository root fails closed", func(t *testing.T) {
		f := newInspectFixture(t)
		f.addCheckout(fakeAttemptWorktree{})
		if got := app.InspectAttemptCheckoutForTest(context.Background(), f.tc.Controller, inspectRoot, inspectRecorded, inspectBranch, f.base, nil); got.Retained != app.RetainedInspectionFailed {
			t.Errorf("nil inspector verdict = %+v", got)
		}
		delete(f.present, inspectRoot)
		if got := f.inspect(inspectBranch); got.Retained != app.RetainedInspectionFailed {
			t.Errorf("missing root verdict = %+v", got)
		}
	})

	t.Run("a relative git executable refuses before running anything", func(t *testing.T) {
		f := newInspectFixture(t)
		f.addCheckout(fakeAttemptWorktree{})
		f.tc.Controller.GitExecutable = "git"
		if got := f.inspect(inspectBranch); got.Retained != app.RetainedInspectionFailed {
			t.Errorf("verdict = %+v, want inspection-failed", got)
		}
		if len(f.tc.Commands.Calls) != 0 {
			t.Errorf("git ran with a relative executable: %v", f.tc.Commands.Calls)
		}
	})
}

func retained(category app.WorktreeRetainedCategory) app.CheckoutVerdictForTest {
	return app.CheckoutVerdictForTest{Disposition: "retained", Retained: category, ListedPath: inspectCanonical}
}

func released(reason app.WorktreeReleaseReason) app.CheckoutVerdictForTest {
	return app.CheckoutVerdictForTest{Disposition: "released", Released: reason, ListedPath: inspectCanonical}
}

// addCheckout models the candidate at its canonical path, on the recorded
// branch at a head descending from the base, present unless the state
// says otherwise through setPresent.
func (f *inspectFixture) addCheckout(w fakeAttemptWorktree) { //nolint:gocritic // hugeParam: test seeding takes the state by value.
	w.Branch, w.Head, w.Present = inspectBranch, f.head, true
	f.git.addAttemptWorktree(inspectCanonical, w)
}

func (f *inspectFixture) mutate(change func(w *fakeAttemptWorktree)) {
	w, ok := f.git.attemptWorktree(inspectCanonical)
	if !ok {
		panic("inspectFixture: no modeled checkout")
	}
	change(&w)
	f.git.addAttemptWorktree(inspectCanonical, w)
}

func (f *inspectFixture) setPresent(present bool) {
	f.mutate(func(w *fakeAttemptWorktree) { w.Present = present })
}

func (f *inspectFixture) setBranch(branch string) {
	f.mutate(func(w *fakeAttemptWorktree) { w.Branch = branch })
}

func (f *inspectFixture) setHead(head string) {
	f.mutate(func(w *fakeAttemptWorktree) { w.Head = head })
}

// fillListingBeforeCandidate makes the root's worktree listing hold
// exactly n bytes before the candidate's record.
func (f *inspectFixture) fillListingBeforeCandidate(n int) {
	f.t.Helper()
	f.git.fillListingBefore(f.t, inspectCanonical, n)
}

// candidateRecordBytes is the length of the candidate's record, the last
// one in the listing.
func (f *inspectFixture) candidateRecordBytes() int {
	f.t.Helper()
	return len(f.git.worktreeListing()) - f.git.listingOffset(f.t, inspectCanonical)
}

// failGit answers every git invocation of subcommand with result and err
// instead of the model.
func (f *inspectFixture) failGit(subcommand string, result app.CommandResult, err error) {
	f.tc.Commands.RunHook = func(ctx context.Context, cmd app.Command) (app.CommandResult, bool, error) {
		if slices.Contains(cmd.Argv, subcommand) {
			return result, true, err
		}
		return f.git.Hook(ctx, cmd)
	}
}

// requireReadOnlyRetirementGit asserts every command the inspection ran
// was the absolute git under exactly the retirement environment, that only
// the index scan and the worktree listing raised the capture bound, and
// that none removed anything.
func (f *inspectFixture) requireReadOnlyRetirementGit(t *testing.T) {
	t.Helper()
	wantEnv := []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	for _, call := range f.tc.Commands.Calls {
		if call.Argv[0] != f.tc.Controller.GitExecutable || !slices.Equal(call.Env, wantEnv) {
			t.Errorf("inspection ran %q with env %q, want the absolute git under the retirement environment", call.Argv, call.Env)
		}
		if slices.Contains(call.Argv, "remove") {
			t.Errorf("inspection ran a removal: %q", call.Argv)
		}
		wantBound := 0
		if slices.Contains(call.Argv, "ls-files") || slices.Contains(call.Argv, "list") {
			wantBound = listingBound
		}
		if call.MaxOutputBytes != wantBound {
			t.Errorf("inspection ran %q with output bound %d, want %d", call.Argv, call.MaxOutputBytes, wantBound)
		}
	}
	if removes, _, _ := f.git.attemptLog(); len(removes) != 0 {
		t.Errorf("inspection removed checkouts: %v", removes)
	}
}
