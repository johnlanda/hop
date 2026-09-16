package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pathLayout is TestInspectCanonicalPath's filesystem under a resolved
// temporary root:
//
//	real/wt/            a directory
//	real/dangling       -> nowhere (a dangling link)
//	link                -> real
//	a/link              -> b/sub (so a/link/.. is b, never a)
//	a/rel               -> ../b/sub (relative)
//	a/deadlink          -> gone-volume/mnt (a dangling link a path passes through)
//	a/loop              -> a/loop
//	b/sub/, b/wt/       directories
//	b/dangling          -> nowhere
//	file                a regular file
//
// a/wt does not exist, so the lexical collapse of a/link/../wt names
// nothing.
func pathLayout(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"real/wt", "a", "b/sub", "b/wt"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		"real/dangling": filepath.Join(root, "nowhere"),
		"link":          filepath.Join(root, "real"),
		"a/link":        filepath.Join(root, "b", "sub"),
		"a/rel":         "../b/sub",
		"a/deadlink":    filepath.Join(root, "gone-volume", "mnt"),
		"a/loop":        filepath.Join(root, "a", "loop"),
		"b/dangling":    filepath.Join(root, "nowhere"),
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestInspectCanonicalPath proves the retirement path seam against the
// real filesystem, resolving every spelling the way the filesystem does:
// existing paths resolve fully, `..` after a symbolic link steps out of
// the link's target (never out of the link's own parent), a missing
// checkout (one level or several) resolves through its deepest existing
// prefix so it still matches git's canonical listing, a dangling link leaf
// exists without being followed, and a relative path is refused.
func TestInspectCanonicalPath(t *testing.T) {
	root := pathLayout(t)
	at := func(rel string) string { return filepath.Join(root, rel) }
	cases := []struct {
		name, path, want string
		exists           bool
	}{
		{"existing directory", at("real/wt"), at("real/wt"), true},
		{"existing directory through a symlink", at("link/wt"), at("real/wt"), true},
		{"missing leaf through a symlink", at("link/gone"), at("real/gone"), false},
		{"several missing levels", at("link/a/b/c"), at("real/a/b/c"), false},
		{"uncleaned spelling", at("link") + "/./wt/../gone", at("real/gone"), false},
		{"dangling link leaf exists, not followed", at("real/dangling"), at("real/dangling"), true},
		{"existing checkout after a symlinked parent's ..", root + "/a/link/../wt", at("b/wt"), true},
		{"missing leaf after a symlinked parent's ..", root + "/a/link/../gone", at("b/gone"), false},
		{"several missing levels after a symlinked parent's ..", root + "/a/link/../gone/deeper", at("b/gone/deeper"), false},
		{"a relative link, then ..", root + "/a/rel/../wt", at("b/wt"), true},
		{"a dangling leaf after a symlinked parent's ..", root + "/a/link/../dangling", at("b/dangling"), true},
		{"repeated separators and a trailing dot", root + "//link//wt/.", at("real/wt"), true},
		{".. above the filesystem root", "/.." + root + "/real/wt", at("real/wt"), true},
	}
	for _, tc := range cases {
		got, exists, err := inspectCanonicalPath(tc.path)
		if err != nil || got != tc.want || exists != tc.exists {
			t.Errorf("%s: inspectCanonicalPath(%q) = %q, %v, %v; want %q, %v", tc.name, tc.path, got, exists, err, tc.want, tc.exists)
		}
		if tc.exists {
			if resolved, evalErr := filepath.EvalSymlinks(tc.path); evalErr == nil && resolved != got {
				t.Errorf("%s: canonical %q differs from the filesystem's own resolution %q", tc.name, got, resolved)
			}
		}
	}

	if _, _, err := inspectCanonicalPath("relative/path"); !errors.Is(err, errPathNotAbsolute) {
		t.Errorf("relative path error = %v, want errPathNotAbsolute", err)
	}
}

// TestInspectCanonicalPathRefusesUnresolvablePaths: a path whose
// canonical form the filesystem cannot establish is an error naming no
// path, never an absent checkout — `..` after a missing component (whose
// lexical collapse names an existing directory), a dangling link passed
// through (an unmounted volume), a symbolic link loop, and a regular file
// used as a directory.
func TestInspectCanonicalPathRefusesUnresolvablePaths(t *testing.T) {
	root := pathLayout(t)
	for name, path := range map[string]string{
		"a missing component, then ..":            root + "/b/gone/../wt",
		"a missing leaf, then ..":                 root + "/b/gone/..",
		"a missing component, then .. twice":      root + "/b/gone/x/../../wt",
		"a dangling link passed through":          root + "/a/deadlink/t1a1",
		"a dangling link, then ..":                root + "/a/deadlink/../wt",
		"a symbolic link loop":                    root + "/a/loop/t1a1",
		"a regular file used as a directory":      root + "/file/t1a1",
		"a regular file, then ..":                 root + "/file/..",
		"a missing path under a regular file":     root + "/file/gone/deeper",
		"a trailing separator on a dangling leaf": root + "/real/dangling/",
	} {
		canonical, exists, err := inspectCanonicalPath(path)
		if err == nil {
			t.Errorf("%s: inspectCanonicalPath(%q) = %q, %v; want an error", name, path, canonical, exists)
			continue
		}
		if !errors.Is(err, errPathUnresolvable) {
			t.Errorf("%s: error %v, want errPathUnresolvable", name, err)
		}
		if strings.Contains(err.Error(), root) {
			t.Errorf("%s: error %q echoes the path", name, err)
		}
	}
}
