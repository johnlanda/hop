package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/johnlanda/hop/internal/app"
)

// Run identities of the retirement scenarios.
const (
	retireRunA = "a0000000-0000-4000-8000-00000000000a"
	retireRunB = "b0000000-0000-4000-8000-00000000000b"
	retireRunC = "c0000000-0000-4000-8000-00000000000c"
	retireRunD = "d0000000-0000-4000-8000-00000000000d"
	retireRunE = "e0000000-0000-4000-8000-00000000000e"
	retireRunF = "f0000000-0000-4000-8000-00000000000f"
	// retirePathCanary is a path canary no retirement diagnostic may ever
	// print.
	retirePathCanary = "/Users/someone/private-checkout"
)

// scriptRetirement scripts a repository whose triage lists every
// retirement shape the commands render, returning the recorded triage
// arguments.
func scriptRetirement(t *testing.T, ctrl *fakeController) *[][2]string {
	t.Helper()
	var triaged [][2]string
	ctrl.retirementCandidates = func(root, exclude string) ([]app.RetirementCandidate, error) {
		triaged = append(triaged, [2]string{root, exclude})
		return []app.RetirementCandidate{
			{RunID: retireRunA, Sequence: 2, NeedsPass: true},
			{RunID: retireRunB, Sequence: 3, NeedsPass: true},
			{RunID: retireRunC, Sequence: 4, NeedsPass: true},
			{RunID: retireRunD, Sequence: 5, Disposition: app.RetirementHistoryMissing},
			{RunID: retireRunE, Sequence: 6, Disposition: app.RetirementNotMerged},
			{RunID: retireRunF, Sequence: 7, NeedsPass: true},
			{RunID: testRunID, Sequence: 8, NeedsPass: true},
		}, nil
	}
	ctrl.acquireForRetirement = func(runID, controllerID string) error {
		if controllerID == "" {
			t.Errorf("acquired %s without a controller id", runID)
		}
		if runID == retireRunC {
			return fmt.Errorf("app: acquire the run lease for worktree retirement: %w", app.ErrLeaseHeld)
		}
		return nil
	}
	ctrl.retireWorktrees = func(_ context.Context, runID string, opts app.RetireWorktreesOptions) (app.WorktreeRetirementReport, error) {
		if opts.HOPPath != "/opt/hop/bin/hop" {
			t.Errorf("pass hop path = %q", opts.HOPPath)
		}
		switch runID {
		case retireRunA:
			if opts.BeforeFirstRemoval == nil {
				t.Fatalf("the pass has no removal announcement")
			}
			opts.BeforeFirstRemoval()
			return app.WorktreeRetirementReport{
				RunID: retireRunA, Sequence: 2, Disposition: app.RetirementRetired, Removed: 2, Absent: 1, Released: 1,
				Worktrees: []app.WorktreeRetirementLine{
					{Branch: "hop/r2/t1a1", Path: "/wt/r2-t1a1", Outcome: app.WorktreeOutcomeRemoved},
					{Branch: "hop/r2/t2a1", Path: "/wt/r2-t2a1", Outcome: app.WorktreeOutcomeAbsent},
					{Branch: "hop/r2/t3a1", Path: "/wt/r2-t3a1", Outcome: app.WorktreeOutcomeReleased, Released: app.ReleasedOtherBranch},
				},
			}, nil
		case retireRunB:
			return app.WorktreeRetirementReport{
				RunID: retireRunB, Sequence: 3, Disposition: app.RetirementInProgress,
				Worktrees: []app.WorktreeRetirementLine{
					{Branch: "hop/r3/t4a1", Path: "/wt/r3-t4a1", Outcome: app.WorktreeOutcomeRetained, Retained: app.RetainedUncommittedChanges},
					{Branch: "hop/r3/t5a1", Path: "/wt/r3-t5a1", Outcome: app.WorktreeOutcomeRetained, Retained: app.RetainedRemoveRefused, ExitCode: 128, EvidencePath: "/state/runs/b/retirement/op/stderr"},
					{Branch: "hop/r3/t6a1", Path: "/wt/r3-t6a1", Outcome: app.WorktreeOutcomeReleased, Released: app.ReleasedNotRegistered, LeftOnDisk: true},
					{Branch: "hop/r3/t7a1", Path: "/wt/r3-t7a1", Outcome: app.WorktreeOutcomeIncomplete},
				},
			}, nil
		case retireRunF:
			return app.WorktreeRetirementReport{RunID: retireRunF, Sequence: 7, Disposition: app.RetirementBlocked}, nil
		default:
			return app.WorktreeRetirementReport{RunID: runID, Sequence: 8, Worktrees: []app.WorktreeRetirementLine{
				{Branch: "hop/r8/t1a1", Path: "/wt/r8-t1a1", Outcome: app.WorktreeOutcomeRemoved},
			}}, fmt.Errorf("sqlite: write under %s failed: %w", retirePathCanary, errors.New("disk full"))
		}
	}
	ctrl.releaseRetirement = func(string) error { return nil }
	return &triaged
}

// retirementGolden is the section 6 rendering of scriptRetirement's
// repository, in order.
func retirementGolden() []string {
	return []string{
		"r2 worktrees retired: 2 removed, 1 already absent, 1 released (removal deletes ignored files such as build output)",
		"r2 worktree hop/r2/t1a1 removed: /wt/r2-t1a1 (close its Herdr workspace if one is still open)",
		"r2 worktree hop/r2/t2a1 already absent: /wt/r2-t2a1",
		"r2 worktree hop/r2/t3a1 released (checked out on another branch): /wt/r2-t3a1; HOP will not remove it",
		"r3 worktree hop/r3/t4a1 retained (uncommitted changes): /wt/r3-t4a1",
		"  action: commit or discard the changes, then run hop status again",
		"r3 worktree hop/r3/t5a1 retained (removal refused, exit 128): /wt/r3-t5a1",
		"  action: inspect the retained evidence at /state/runs/b/retirement/op/stderr, then run hop status again",
		"r3 worktree hop/r3/t6a1 released (not a registered worktree): /wt/r3-t6a1; left on disk; no longer managed by HOP",
		"r3 worktree hop/r3/t7a1 removal incomplete: /wt/r3-t7a1; hop status will finish the removal",
		"r4 worktree retirement deferred: the run is held by another controller",
		"r5 worktree retirement skipped: the repository root no longer holds this run's history",
		"r7 worktree retirement blocked: an earlier retirement process could not be verified gone",
		"r8 worktree hop/r8/t1a1 removed: /wt/r8-t1a1 (close its Herdr workspace if one is still open)",
	}
}

// retirementCalls filters the recorded calls to the retirement protocol.
func retirementCalls(calls []string) []string {
	var out []string
	for _, call := range calls {
		if strings.HasPrefix(call, "RetirementCandidates") || strings.HasPrefix(call, "AcquireForRetirement") ||
			call == "RetireWorktrees" || call == "ReleaseRetirement" || call == "Status" {
			out = append(out, call)
		}
	}
	return out
}

// TestStatusRunsWorktreeRetirement covers hop status's pass: triage for
// the resolved repository, a lease only for runs with work, one pass and a
// release each (a lease held elsewhere deferred, a pass error reported
// value-free with the lease still released), the removal announcement
// before the output, the section 6 lines after it, exit 0 throughout, and
// no Herdr runtime.
func TestStatusRunsWorktreeRetirement(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		// render is the first rendered line.
		render string
	}{
		{"the listing", nil, "r1 " + testRunID + " running"},
		{"a run's detail", []string{"-run", testRunID}, "run r1 " + testRunID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			ctrl := &fakeController{}
			triaged := scriptRetirement(t, ctrl)
			ctrl.status = func(req app.StatusRequest) (app.StatusResult, error) {
				if req.RunID == "" {
					return app.StatusResult{Runs: []app.RunSummaryView{{RunID: testRunID, Sequence: 1, State: "running"}}}, nil
				}
				return app.StatusResult{Detail: &app.RunDetailView{RunSummaryView: app.RunSummaryView{RunID: testRunID, Sequence: 1, State: "running"}}}, nil
			}
			repo := t.TempDir()
			td := newTestDeps(ctrl, statusEnv(), repo)

			code, err := runStatus(tc.args, &stdout, &stderr, td.deps)
			if err != nil {
				t.Fatalf("write error: %v", err)
			}
			if code != exitOK {
				t.Fatalf("exit code = %d, want 0 whatever the pass reported (stderr %q)", code, stderr.String())
			}
			resolved, err := resolveRepositoryRoot(td.deps, repo)
			if err != nil {
				t.Fatal(err)
			}
			if want := [][2]string{{resolved, ""}}; !slices.Equal(*triaged, want) {
				t.Fatalf("triage = %v, want the resolved repository with nothing excluded", *triaged)
			}
			out := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
			if out[0] != "retiring worktrees of r2…" || out[1] != tc.render {
				t.Fatalf("output starts %q, want the removal announcement, then the rendering:\n%s", out[:2], stdout.String())
			}
			if tail := out[len(out)-len(retirementGolden()):]; !slices.Equal(tail, retirementGolden()) {
				t.Fatalf("pass lines =\n%s\nwant\n%s", strings.Join(tail, "\n"), strings.Join(retirementGolden(), "\n"))
			}
			if got := stderr.String(); got != "hop status: worktree retirement of r8 stopped early; the next hop status continues it\n" {
				t.Fatalf("stderr = %q, want one value-free line", got)
			}
			if strings.Contains(stdout.String()+stderr.String(), retirePathCanary) {
				t.Fatalf("a retirement diagnostic echoed a path")
			}
			wantCalls := []string{
				"RetirementCandidates",
				"AcquireForRetirement " + retireRunA, "RetireWorktrees", "ReleaseRetirement",
				"AcquireForRetirement " + retireRunB, "RetireWorktrees", "ReleaseRetirement",
				"AcquireForRetirement " + retireRunC,
				"AcquireForRetirement " + retireRunF, "RetireWorktrees", "ReleaseRetirement",
				"AcquireForRetirement " + testRunID, "RetireWorktrees", "ReleaseRetirement",
			}
			if got := retirementCalls(ctrl.recorded()); !slices.Equal(got[:len(wantCalls)], wantCalls) || !slices.Contains(got[len(wantCalls):], "Status") {
				t.Fatalf("calls = %v, want the pass %v before rendering", got, wantCalls)
			}
			for _, cfg := range td.openCalls {
				if cfg.withRuntime {
					t.Fatalf("hop status wired the Herdr runtime: %+v", cfg)
				}
			}
		})
	}

	t.Run("a triage failure is one value-free line and status still renders", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.retirementCandidates = func(string, string) ([]app.RetirementCandidate, error) {
			return nil, fmt.Errorf("sqlite: read %s: %w", retirePathCanary, errors.New("database is locked"))
		}
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return app.StatusResult{Runs: []app.RunSummaryView{{RunID: testRunID, Sequence: 1, State: "running"}}}, nil
		}
		td := newTestDeps(ctrl, statusEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer
		code, err := runStatus(nil, &stdout, &stderr, td.deps)
		if err != nil || code != exitOK {
			t.Fatalf("code = %d, err = %v", code, err)
		}
		if stderr.String() != "hop status: worktree retirement could not list this repository's runs; the next hop status retries\n" || strings.Contains(stderr.String(), retirePathCanary) {
			t.Fatalf("stderr = %q", stderr.String())
		}
		if stdout.String() != "r1 "+testRunID+" running\n" {
			t.Fatalf("stdout = %q, want the listing alone", stdout.String())
		}
	})

	t.Run("a first signal cancels the pass, which still releases, and status renders", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.retirementCandidates = func(string, string) ([]app.RetirementCandidate, error) {
			return []app.RetirementCandidate{{RunID: retireRunA, Sequence: 2, NeedsPass: true}}, nil
		}
		ctrl.acquireForRetirement = func(string, string) error { return nil }
		var td *testDeps
		ctrl.retireWorktrees = func(ctx context.Context, _ string, _ app.RetireWorktreesOptions) (app.WorktreeRetirementReport, error) {
			td.signals <- syscall.SIGINT
			<-ctx.Done()
			return app.WorktreeRetirementReport{RunID: retireRunA, Sequence: 2, Disposition: app.RetirementInProgress, Worktrees: []app.WorktreeRetirementLine{
				{Branch: "hop/r2/t1a1", Path: "/wt/r2-t1a1", Outcome: app.WorktreeOutcomeUnresolved},
			}}, nil
		}
		released := false
		ctrl.releaseRetirement = func(string) error { released = true; return nil }
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) {
			return app.StatusResult{Runs: []app.RunSummaryView{{RunID: testRunID, Sequence: 1, State: "running"}}}, nil
		}
		td = newTestDeps(ctrl, statusEnv(), t.TempDir())
		var stdout, stderr bytes.Buffer
		code, err := runStatus(nil, &stdout, &stderr, td.deps)
		if err != nil || code != exitOK || !released {
			t.Fatalf("code = %d, err = %v, released = %v", code, err, released)
		}
		want := "r1 " + testRunID + " running\nr2 worktree hop/r2/t1a1 removal unresolved: /wt/r2-t1a1; hop status will finish the removal\n"
		if stdout.String() != want {
			t.Fatalf("stdout = %q, want %q", stdout.String(), want)
		}
	})

	t.Run("an unlocatable hop binary skips the pass with one line", func(t *testing.T) {
		ctrl := &fakeController{}
		ctrl.retirementCandidates = func(string, string) ([]app.RetirementCandidate, error) {
			t.Fatal("triage ran without a hop binary")
			return nil, nil
		}
		ctrl.status = func(app.StatusRequest) (app.StatusResult, error) { return app.StatusResult{}, nil }
		td := newTestDeps(ctrl, statusEnv(), t.TempDir())
		td.deps.executable = func() (string, error) { return "", errors.New("readlink " + retirePathCanary) }
		var stdout, stderr bytes.Buffer
		code, err := runStatus(nil, &stdout, &stderr, td.deps)
		if err != nil || code != exitOK || stderr.String() != "hop status: worktree retirement skipped: the hop executable could not be located\n" {
			t.Fatalf("code = %d, err = %v, stderr = %q", code, err, stderr.String())
		}
	})
}

// TestRunAndResumeRunWorktreeRetirementFirst covers the controller
// starts: hop run passes over the repository (excluding nothing) after its
// workflow resolves and before its run starts, hop resume passes excluding
// its own resolved run before resuming it, and both print the pass lines
// before their own.
func TestRunAndResumeRunWorktreeRetirementFirst(t *testing.T) {
	onlyRemoved := func(ctrl *fakeController, triaged *[][2]string) {
		ctrl.retirementCandidates = func(root, exclude string) ([]app.RetirementCandidate, error) {
			*triaged = append(*triaged, [2]string{root, exclude})
			return []app.RetirementCandidate{{RunID: retireRunA, Sequence: 2, NeedsPass: true}}, nil
		}
		ctrl.acquireForRetirement = func(string, string) error { return nil }
		ctrl.retireWorktrees = func(context.Context, string, app.RetireWorktreesOptions) (app.WorktreeRetirementReport, error) {
			return app.WorktreeRetirementReport{RunID: retireRunA, Sequence: 2, Disposition: app.RetirementRetired, Removed: 1, Worktrees: []app.WorktreeRetirementLine{
				{Branch: "hop/r2/t1a1", Path: "/wt/r2-t1a1", Outcome: app.WorktreeOutcomeRemoved},
			}}, nil
		}
		ctrl.releaseRetirement = func(string) error { return nil }
	}
	passLines := "r2 worktrees retired: 1 removed, 0 already absent, 0 released (removal deletes ignored files such as build output)\n" +
		"r2 worktree hop/r2/t1a1 removed: /wt/r2-t1a1 (close its Herdr workspace if one is still open)\n"

	t.Run("hop run", func(t *testing.T) {
		ctrl := &fakeController{}
		var triaged [][2]string
		onlyRemoved(ctrl, &triaged)
		ctrl.startRun = func(app.StartRunRequest) (app.StartRunResult, app.RunHandle, error) {
			return app.StartRunResult{RunID: testRunID, Sequence: 3}, app.RunHandle{}, nil
		}
		ctrl.status = scriptStatus(detailStep("completed", "completed", false))
		repo := t.TempDir()
		td := newTestDeps(ctrl, map[string]string{"HOME": "/home/u"}, repo)
		var stdout, stderr bytes.Buffer
		code, err := runRun([]string{"brief"}, &stdout, &stderr, td.deps)
		if err != nil || code != exitOK {
			t.Fatalf("code = %d, err = %v, stderr = %q", code, err, stderr.String())
		}
		if !strings.HasPrefix(stdout.String(), passLines+"run r3 "+testRunID+" started\n") {
			t.Fatalf("stdout = %q, want the pass lines before the start line", stdout.String())
		}
		resolved, err := resolveRepositoryRoot(td.deps, repo)
		if err != nil {
			t.Fatal(err)
		}
		if len(triaged) != 1 || triaged[0] != [2]string{resolved, ""} {
			t.Fatalf("triage = %v", triaged)
		}
		calls := ctrl.recorded()
		if resolve, start := slices.Index(calls, "ResolveRunWorkflow"), slices.Index(calls, "StartRun"); resolve < 0 || start < 0 ||
			resolve > slices.Index(calls, "RetirementCandidates") || slices.Index(calls, "ReleaseRetirement") > start {
			t.Fatalf("calls = %v, want the pass between ResolveRunWorkflow and StartRun", calls)
		}
	})

	t.Run("a usage-refused hop run passes over nothing", func(t *testing.T) {
		ctrl := &fakeController{}
		var triaged [][2]string
		onlyRemoved(ctrl, &triaged)
		ctrl.resolveRunWorkflow = func(string, string) (string, error) {
			return "", fmt.Errorf("%w: the policy is unusable", app.ErrStartRefused)
		}
		td := newTestDeps(ctrl, map[string]string{"HOME": "/home/u"}, t.TempDir())
		var stdout, stderr bytes.Buffer
		if code, err := runRun([]string{"brief"}, &stdout, &stderr, td.deps); err != nil || code != exitUsage || len(triaged) != 0 {
			t.Fatalf("code = %d, err = %v, triage = %v", code, err, triaged)
		}
	})

	t.Run("hop resume excludes its own run", func(t *testing.T) {
		ctrl := &fakeController{}
		var triaged [][2]string
		onlyRemoved(ctrl, &triaged)
		ctrl.resume = func(app.ResumeRequest) (app.ResumeResult, app.RunHandle, error) {
			return app.ResumeResult{Outcome: app.ResumeNothingToDo, Detail: "the run is terminal"}, app.RunHandle{}, nil
		}
		ctrl.status = func(req app.StatusRequest) (app.StatusResult, error) {
			if req.RunID == "" {
				return app.StatusResult{Runs: []app.RunSummaryView{{RunID: testRunID, Sequence: 1, State: "completed"}}}, nil
			}
			return detailStep("completed", "completed", false), nil
		}
		repo := t.TempDir()
		td := newTestDeps(ctrl, map[string]string{"HOME": "/home/u"}, repo)
		var stdout, stderr bytes.Buffer
		code, err := runResume([]string{"r1"}, &stdout, &stderr, td.deps)
		if err != nil || code != exitOK {
			t.Fatalf("code = %d, err = %v, stderr = %q", code, err, stderr.String())
		}
		resolved, err := resolveRepositoryRoot(td.deps, repo)
		if err != nil {
			t.Fatal(err)
		}
		if len(triaged) != 1 || triaged[0] != [2]string{resolved, testRunID} {
			t.Fatalf("triage = %v, want the resolved run excluded", triaged)
		}
		if !strings.HasPrefix(stdout.String(), passLines+"resume nothing-to-do: ") {
			t.Fatalf("stdout = %q, want the pass lines before the resume line", stdout.String())
		}
		calls := ctrl.recorded()
		if slices.Index(calls, "ReleaseRetirement") > slices.Index(calls, "Resume") {
			t.Fatalf("calls = %v, want the pass before Resume", calls)
		}
	})
}

// TestWorktreeDetailLines pins hop status -run's per-row rendering for a
// feature run, every non-final row carrying its human action, and every
// category and reason's wording; a solo run keeps its single line.
func TestWorktreeDetailLines(t *testing.T) {
	views := []app.WorktreeView{
		{Branch: "hop/r3/t1a1", Path: "/wt/t1", State: "removed"},
		{Branch: "hop/r3/t2a1", Path: "/wt/t2", State: "absent"},
		{Branch: "hop/r3/t3a1", Path: "/wt/t3", State: "released", Released: app.ReleasedDetached},
		{Branch: "hop/r3/t4a1", Path: "/wt/t4", State: "active", Retained: app.RetainedHiddenChanges},
		{Branch: "hop/r3/t5a1", Path: "/wt/t5", State: "active", Retained: app.RetainedRemoveRefused, EvidencePath: "/state/e/stderr"},
		{Branch: "hop/r3/t6a1", Path: "/wt/t6", State: "active", Removal: "incomplete"},
		{Branch: "hop/r3/t7a1", Path: "/wt/t7", State: "active", Removal: "interrupted"},
		{Branch: "hop/r3/t8a1", Path: "/wt/t8", State: "active", Removal: "unresolved"},
		{Branch: "hop/r3/t9a1", Path: "/wt/t9", State: "active"},
	}
	want := []string{
		"  worktree:      hop/r3/t1a1 removed /wt/t1",
		"  worktree:      hop/r3/t2a1 absent /wt/t2",
		"  worktree:      hop/r3/t3a1 released (detached HEAD) /wt/t3; left on disk; no longer managed by HOP",
		"  worktree:      hop/r3/t4a1 retained (hidden changes) /wt/t4",
		"    action:      clear git update-index --no-assume-unchanged/--no-skip-worktree, then commit or discard, then run hop status again",
		"  worktree:      hop/r3/t5a1 retained (removal refused) /wt/t5",
		"    action:      inspect the retained evidence at /state/e/stderr, then run hop status again",
		"  worktree:      hop/r3/t6a1 removal incomplete /wt/t6; hop status will finish the removal",
		"  worktree:      hop/r3/t7a1 removal interrupted /wt/t7; hop status will finish the removal",
		"  worktree:      hop/r3/t8a1 removal unresolved /wt/t8; hop status will finish the removal",
		"  worktree:      hop/r3/t9a1 active /wt/t9",
	}
	if got := worktreeDetailLines(views); !slices.Equal(got, want) {
		t.Fatalf("lines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	for category, want := range map[app.WorktreeRetainedCategory][2]string{
		app.RetainedUncommittedChanges: {"uncommitted changes", "commit or discard the changes, then run hop status again"},
		app.RetainedHiddenChanges:      {"hidden changes", "clear git update-index --no-assume-unchanged/--no-skip-worktree, then commit or discard, then run hop status again"},
		app.RetainedLocked:             {"locked", "git worktree unlock, then run hop status again"},
		app.RetainedInterruptedRemoval: {"interrupted removal", "inspect; restore (git checkout -- .) or remove it yourself, then run hop status again"},
		app.RetainedRemoveRefused:      {"removal refused", "inspect the retained evidence, then run hop status again"},
		app.RetainedInspectionFailed:   {"inspection failed", "check the checkout, then run hop status again"},
	} {
		if got := [2]string{retainedLabel(category), retainedAction(category, "")}; got != want {
			t.Errorf("%s = %q, want %q", category, got, want)
		}
	}
	for reason, want := range map[app.WorktreeReleaseReason]string{
		app.ReleasedRepositoryRoot:  "the repository root",
		app.ReleasedNotRegistered:   "not a registered worktree",
		app.ReleasedOtherBranch:     "checked out on another branch",
		app.ReleasedDetached:        "detached HEAD",
		app.ReleasedOtherRepository: "belongs to another repository",
		app.ReleasedBaseNotAncestor: "no longer descends from its base",
		app.ReleasedUnverified:      "not verifiably HOP's",
	} {
		if got := releasedLabel(reason); got != want {
			t.Errorf("%s = %q, want %q", reason, got, want)
		}
	}
	for disposition, want := range map[app.WorktreeRetirementDisposition]string{
		app.RetirementCheckFailed: "r9 worktree retirement check failed: whether the run is merged could not be decided",
		app.RetirementInterrupted: "r9 worktree retirement interrupted: hop status will continue it",
	} {
		if got := dispositionLines("r9", disposition); !slices.Equal(got, []string{want}) {
			t.Errorf("%s = %q", disposition, got)
		}
	}
	for _, quiet := range []app.WorktreeRetirementDisposition{app.RetirementNotMerged, app.RetirementNothingIntegrated, app.RetirementInProgress} {
		if got := dispositionLines("r9", quiet); got != nil {
			t.Errorf("%s renders %q, want nothing", quiet, got)
		}
	}
	if got := worktreeOutcomeLines("r9", &app.WorktreeRetirementLine{Branch: "b", Path: "/p", Outcome: app.WorktreeOutcomeNotDispatched}); !slices.Equal(got, []string{"r9 worktree b not removed yet: /p; hop status will retry"}) {
		t.Errorf("not dispatched = %q", got)
	}

	featureDetail := &app.RunDetailView{
		RunSummaryView: app.RunSummaryView{RunID: testRunID, Sequence: 3, State: "completed"},
		Mode:           "feature", TargetBranch: "refs/heads/main", Worktrees: views[:1],
	}
	var out bytes.Buffer
	if _, err := renderRunDetail(&out, featureDetail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "  attempt:       (none)\n  worktree:      hop/r3/t1a1 removed /wt/t1\n  binding:") {
		t.Fatalf("feature detail:\n%s", out.String())
	}
	out.Reset()
	if _, err := renderRunDetail(&out, &app.RunDetailView{RunSummaryView: featureDetail.RunSummaryView, Mode: "feature"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "worktree:") {
		t.Fatalf("a feature run without rows renders a worktree line:\n%s", out.String())
	}
}
