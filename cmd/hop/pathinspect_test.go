package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestInspectCanonicalPath proves the retirement path seam against the
// real filesystem: existing paths resolve fully, a missing checkout (one
// level or several) resolves through its deepest existing ancestor so it
// still matches git's canonical listing, a symbolic link spelling
// canonicalizes, a dangling link leaf exists without being followed, and
// a relative path is refused.
func TestInspectCanonicalPath(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(realDir, "wt"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(realDir, "dangling")
	if err := os.Symlink(filepath.Join(root, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, path, want string
		exists           bool
	}{
		{"existing directory", filepath.Join(realDir, "wt"), filepath.Join(realDir, "wt"), true},
		{"existing directory through a symlink", filepath.Join(link, "wt"), filepath.Join(realDir, "wt"), true},
		{"missing leaf through a symlink", filepath.Join(link, "gone"), filepath.Join(realDir, "gone"), false},
		{"several missing levels", filepath.Join(link, "a", "b", "c"), filepath.Join(realDir, "a", "b", "c"), false},
		{"uncleaned spelling", link + "/./wt/../gone", filepath.Join(realDir, "gone"), false},
		{"dangling link leaf exists, not followed", dangling, dangling, true},
	}
	for _, tc := range cases {
		got, exists, err := inspectCanonicalPath(tc.path)
		if err != nil || got != tc.want || exists != tc.exists {
			t.Errorf("%s: inspectCanonicalPath(%q) = %q, %v, %v; want %q, %v", tc.name, tc.path, got, exists, err, tc.want, tc.exists)
		}
	}

	if _, _, err := inspectCanonicalPath("relative/path"); !errors.Is(err, errPathNotAbsolute) {
		t.Errorf("relative path error = %v, want errPathNotAbsolute", err)
	}
}
