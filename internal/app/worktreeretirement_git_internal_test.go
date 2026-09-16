package app

import (
	"errors"
	"reflect"
	"testing"
)

// TestParseWorktreeListZ parses the record shapes the process probe
// pinned (TestGitProbeWorktreeListPorcelainZ): the main checkout first,
// then per checkout its path, HEAD, branch or detached, and the optional
// locked (with or without a reason) and prunable attributes.
func TestParseWorktreeListZ(t *testing.T) {
	out := []byte("worktree /r/repo\x00HEAD aaaa\x00branch refs/heads/main\x00\x00" +
		"worktree /r/wt-t1a1\x00HEAD bbbb\x00branch refs/heads/hop/r1/t1a1\x00\x00" +
		"worktree /r/wt-t2a1\x00HEAD bbbb\x00branch refs/heads/hop/r1/t2a1\x00locked in use\x00\x00" +
		"worktree /r/wt-t3a1\x00HEAD bbbb\x00branch refs/heads/hop/r1/t3a1\x00locked\x00\x00" +
		"worktree /r/wt-detached\x00HEAD bbbb\x00detached\x00\x00" +
		"worktree /r/wt-t4a1\x00HEAD bbbb\x00branch refs/heads/hop/r1/t4a1\x00prunable gitdir file points to non-existent location\x00\x00" +
		"worktree /r/with space\x00HEAD cccc\x00branch refs/heads/x\x00some-future-attribute v\x00\x00")
	got, err := parseWorktreeListZ(out)
	if err != nil {
		t.Fatalf("parseWorktreeListZ: %v", err)
	}
	want := []listedWorktree{
		{Path: "/r/repo", Head: "aaaa", Branch: "refs/heads/main"},
		{Path: "/r/wt-t1a1", Head: "bbbb", Branch: "refs/heads/hop/r1/t1a1"},
		{Path: "/r/wt-t2a1", Head: "bbbb", Branch: "refs/heads/hop/r1/t2a1", Locked: true, LockReason: "in use"},
		{Path: "/r/wt-t3a1", Head: "bbbb", Branch: "refs/heads/hop/r1/t3a1", Locked: true},
		{Path: "/r/wt-detached", Head: "bbbb", Detached: true},
		{Path: "/r/wt-t4a1", Head: "bbbb", Branch: "refs/heads/hop/r1/t4a1", Prunable: true},
		{Path: "/r/with space", Head: "cccc", Branch: "refs/heads/x"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsed =\n%+v\nwant\n%+v", got, want)
	}

	for name, bad := range map[string][]byte{
		"empty":                       nil,
		"no record terminator":        []byte("worktree /r/repo\x00HEAD aaaa\x00"),
		"record without its worktree": []byte("HEAD aaaa\x00branch refs/heads/main\x00\x00"),
		"empty worktree path":         []byte("worktree \x00HEAD aaaa\x00\x00"),
	} {
		if _, err := parseWorktreeListZ(bad); err == nil {
			t.Errorf("%s: parsed %q; want an error", name, bad)
		}
	}
}

// TestHasHiddenIndexFlags reads `git ls-files -v -z` tags as the probe
// pinned them: H is an ordinary tracked entry, a lowercase tag is
// assume-unchanged and S is skip-worktree.
func TestHasHiddenIndexFlags(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
		err  bool
	}{
		{"clean", "H .gitignore\x00H f.txt\x00H g.txt\x00", false, false},
		{"no entries", "", false, false},
		{"assume-unchanged", "H .gitignore\x00h f.txt\x00H g.txt\x00", true, false},
		{"skip-worktree", "H .gitignore\x00H f.txt\x00S g.txt\x00", true, false},
		{"path with spaces", "H a b.txt\x00", false, false},
		{"untagged entry", "f.txt\x00", false, true},
		{"missing separator", "Hf.txt\x00", false, true},
	}
	for _, tc := range cases {
		got, err := hasHiddenIndexFlags([]byte(tc.out))
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("%s: hasHiddenIndexFlags = %v, %v; want %v (error %v)", tc.name, got, err, tc.want, tc.err)
		}
	}
}

// TestClassifyAncestry maps the pinned `merge-base --is-ancestor` exit
// statuses.
func TestClassifyAncestry(t *testing.T) {
	for exit, want := range map[int]ancestryResult{
		0: ancestryContained, 1: ancestryNotContained, 128: ancestryFailed, 2: ancestryFailed, -1: ancestryFailed,
	} {
		if got := classifyAncestry(exit); got != want {
			t.Errorf("classifyAncestry(%d) = %q, want %q", exit, got, want)
		}
	}
}

// TestClassifyTargetBranch maps the pinned freeze-time `symbolic-ref -q
// HEAD` results: a branch is its full ref, a detached HEAD (exit 1, no
// output) is no target, and everything else is unreadable.
func TestClassifyTargetBranch(t *testing.T) {
	cases := []struct {
		name   string
		exit   int
		stdout string
		want   string
		err    bool
	}{
		{"branch", 0, "refs/heads/main\n", "refs/heads/main", false},
		{"nested branch", 0, "refs/heads/hop/r2/integration\n", "refs/heads/hop/r2/integration", false},
		{"detached", 1, "", "", false},
		{"detached with output", 1, "refs/heads/main\n", "", true},
		{"not a branch ref", 0, "refs/remotes/origin/main\n", "", true},
		{"bare prefix", 0, "refs/heads/\n", "", true},
		{"two lines", 0, "refs/heads/a\nrefs/heads/b\n", "", true},
		{"git failure", 128, "", "", true},
	}
	for _, tc := range cases {
		got, err := classifyTargetBranch(tc.exit, []byte(tc.stdout))
		if tc.err {
			if !errors.Is(err, errTargetUnreadable) {
				t.Errorf("%s: err = %v, want errTargetUnreadable", tc.name, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: classifyTargetBranch = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}
