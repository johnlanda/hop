package internal

import (
	"cmp"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// guideIssue is one guide-coverage, navigation or link failure.
type guideIssue struct {
	guide string // module-relative path of the AGENTS.md concerned
	link  string // link target, when the finding is link-specific
	rule  string
}

func (g guideIssue) String() string {
	return fmt.Sprintf("%s: %s: %s", g.guide, cmp.Or(g.link, "-"), g.rule)
}

// compareGuideIssues orders issues by guide, link and rule.
func compareGuideIssues(a, b guideIssue) int {
	return cmp.Or(strings.Compare(a.guide, b.guide), strings.Compare(a.link, b.link), strings.Compare(a.rule, b.rule))
}

// guideReport is the outcome of checking one tree's guides.
type guideReport struct {
	packages []string // module-relative package directories found by the source walk
	guides   []string // module-relative paths of every AGENTS.md outside excluded trees
	issues   []guideIssue
}

// guideDirectories maps every directory that must carry a guide to the
// immediate children that must carry one too: the root, every package
// directory and every grouping directory between a package and the root.
func guideDirectories(packages []string) map[string][]string {
	dirs := map[string][]string{".": nil}
	for _, pkg := range packages {
		for dir := pkg; dir != "."; dir = parentDirectory(dir) {
			dirs[dir] = nil
		}
	}
	for dir := range dirs {
		if dir != "." {
			parent := parentDirectory(dir)
			dirs[parent] = append(dirs[parent], dir)
		}
	}
	for dir := range dirs {
		slices.Sort(dirs[dir])
	}
	return dirs
}

// parentDirectory returns the parent of a slash-separated module-relative
// directory, "." for a top-level directory.
func parentDirectory(dir string) string {
	if i := strings.LastIndex(dir, "/"); i >= 0 {
		return dir[:i]
	}
	return "."
}

// guidePath returns the module-relative path of a directory's AGENTS.md.
func guidePath(dir string) string {
	if dir == "." {
		return "AGENTS.md"
	}
	return dir + "/AGENTS.md"
}

// markdownLinkPattern matches the target of an inline link or image.
var markdownLinkPattern = regexp.MustCompile(`\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)`)

// linkSchemePattern matches a link that starts with a URL scheme.
var linkSchemePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)

// markdownLinks returns the targets of every inline link in content.
func markdownLinks(content string) []string {
	var links []string
	for _, match := range markdownLinkPattern.FindAllStringSubmatch(content, -1) {
		links = append(links, match[1])
	}
	return links
}

// linkResolution is the outcome of resolving one link written in a guide.
type linkResolution struct {
	target  string // module-relative path the link names, when it is local
	skip    bool   // the link is not verified: an external URL, an in-page anchor or a path into an excluded tree
	problem string // why the link is wrong regardless of the target's existence
}

// resolveLink resolves a link written in guide to a module-relative path.
// Links into excluded trees are skipped because reference checkouts may be
// absent from a clone.
func resolveLink(guide, link string) linkResolution {
	if strings.HasPrefix(link, "#") || linkSchemePattern.MatchString(link) {
		return linkResolution{skip: true}
	}
	target, _, _ := strings.Cut(link, "#")
	if strings.HasPrefix(target, "/") {
		return linkResolution{problem: "absolute link; use a path relative to the guide"}
	}
	target = path.Join(path.Dir(guide), target)
	if target == ".." || strings.HasPrefix(target, "../") {
		return linkResolution{problem: "link leaves the repository"}
	}
	if excludedComponent(target) != "" {
		return linkResolution{target: target, skip: true}
	}
	return linkResolution{target: target}
}

// findGuides returns the module-relative path of every AGENTS.md below root
// outside the excluded trees, sorted.
func findGuides(root string) ([]string, error) {
	var guides []string
	err := filepath.WalkDir(root, func(fullPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if fullPath != root && isExcludedDirectory(entry.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Name() != "AGENTS.md" {
			return nil
		}
		rel, err := filepath.Rel(root, fullPath)
		if err != nil {
			return err
		}
		guides = append(guides, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(guides)
	return guides, nil
}

// checkGuides inventories the Go packages below root and reports every
// directory that must carry an AGENTS.md but does not, every guide that
// fails to link an immediate child guide, and every local link in any
// AGENTS.md that does not resolve.
func checkGuides(root string) (*guideReport, error) {
	tree, err := walkSources(root)
	if err != nil {
		return nil, err
	}
	guides, err := findGuides(root)
	if err != nil {
		return nil, err
	}
	contents := make(map[string]string, len(guides))
	for _, guide := range guides {
		data, err := readTreeFile(root, guide)
		if err != nil {
			return nil, err
		}
		contents[guide] = string(data)
	}
	packages := slices.Sorted(maps.Keys(tree.packages))
	var issues []guideIssue
	dirs := guideDirectories(packages)
	for _, dir := range slices.Sorted(maps.Keys(dirs)) {
		guide := guidePath(dir)
		content, ok := contents[guide]
		if !ok {
			issues = append(issues, guideIssue{guide: guide, rule: missingGuideRule(dir, tree)})
			continue
		}
		links := markdownLinks(content)
		for _, child := range dirs[dir] {
			want := guidePath(child)
			linked := slices.ContainsFunc(links, func(link string) bool { return resolveLink(guide, link).target == want })
			if !linked {
				issues = append(issues, guideIssue{guide: guide, link: want, rule: "guide does not link its immediate child guide"})
			}
		}
	}
	for _, guide := range guides {
		for _, link := range markdownLinks(contents[guide]) {
			resolved := resolveLink(guide, link)
			switch {
			case resolved.problem != "":
				issues = append(issues, guideIssue{guide: guide, link: link, rule: resolved.problem})
			case resolved.skip:
				continue
			default:
				if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(resolved.target))); err != nil {
					issues = append(issues, guideIssue{guide: guide, link: link, rule: "broken local link"})
				}
			}
		}
	}
	slices.SortFunc(issues, compareGuideIssues)
	return &guideReport{packages: packages, guides: guides, issues: slices.Compact(issues)}, nil
}

// missingGuideRule names what kind of guide a directory lacks.
func missingGuideRule(dir string, tree *sourceTree) string {
	if dir == "." {
		return "missing root guide"
	}
	if _, ok := tree.packages[dir]; ok {
		return "missing package guide"
	}
	return "missing index guide for a grouping directory"
}

// TestGuidesRealTree checks the HOP tree itself. The inventory must contain
// the packages and guides known to exist, so an enumeration that found
// nothing cannot pass.
func TestGuidesRealTree(t *testing.T) {
	root := moduleRoot(t)

	result, err := checkGuides(root)
	if err != nil {
		t.Fatalf("checking guides in %s: %v", root, err)
	}

	for _, want := range []string{"cmd/hop", "internal"} {
		if !slices.Contains(result.packages, want) {
			t.Errorf("the package inventory does not contain %s; it found %v", want, result.packages)
		}
	}
	for _, want := range []string{"AGENTS.md", "cmd/AGENTS.md", "cmd/hop/AGENTS.md", "internal/AGENTS.md"} {
		if !slices.Contains(result.guides, want) {
			t.Errorf("the guide inventory does not contain %s; it found %v", want, result.guides)
		}
	}
	for _, issue := range result.issues {
		t.Error(issue)
	}
}

func TestGuidesDirectoryMap(t *testing.T) {
	cases := []struct {
		name     string
		packages []string
		want     map[string][]string
	}{
		{
			name:     "root package alone",
			packages: []string{"."},
			want:     map[string][]string{".": nil},
		},
		{
			name:     "nested package adds every grouping ancestor",
			packages: []string{"internal/domain/run"},
			want: map[string][]string{
				".":                   {"internal"},
				"internal":            {"internal/domain"},
				"internal/domain":     {"internal/domain/run"},
				"internal/domain/run": nil,
			},
		},
		{
			name:     "siblings are sorted under one parent",
			packages: []string{"internal", "cmd/hop", "cmd/alpha"},
			want: map[string][]string{
				".":         {"cmd", "internal"},
				"cmd":       {"cmd/alpha", "cmd/hop"},
				"cmd/alpha": nil,
				"cmd/hop":   nil,
				"internal":  nil,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := guideDirectories(tc.packages)

			if !maps.EqualFunc(got, tc.want, slices.Equal) {
				t.Errorf("guideDirectories(%v) = %v, want %v", tc.packages, got, tc.want)
			}
		})
	}
}

func TestGuidesLinkResolution(t *testing.T) {
	cases := []struct {
		name  string
		guide string
		link  string
		want  linkResolution
	}{
		{name: "sibling file", guide: "cmd/hop/AGENTS.md", link: "main.go", want: linkResolution{target: "cmd/hop/main.go"}},
		{name: "parent guide with anchor", guide: "cmd/hop/AGENTS.md", link: "../AGENTS.md#purpose", want: linkResolution{target: "cmd/AGENTS.md"}},
		{name: "root guide child", guide: "AGENTS.md", link: "docs/AGENTS.md", want: linkResolution{target: "docs/AGENTS.md"}},
		{name: "in-page anchor", guide: "AGENTS.md", link: "#map", want: linkResolution{skip: true}},
		{name: "external URL", guide: "AGENTS.md", link: "https://example.com/x", want: linkResolution{skip: true}},
		{name: "mail link", guide: "AGENTS.md", link: "mailto:x@example.com", want: linkResolution{skip: true}},
		{name: "reference checkout is skipped", guide: "docs/plan/AGENTS.md", link: "../../repos/herdr/README.md", want: linkResolution{target: "repos/herdr/README.md", skip: true}},
		{name: "absolute path", guide: "AGENTS.md", link: "/Users/someone/file.md", want: linkResolution{problem: "absolute link; use a path relative to the guide"}},
		{name: "escaping the repository", guide: "cmd/AGENTS.md", link: "../../other/README.md", want: linkResolution{problem: "link leaves the repository"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveLink(tc.guide, tc.link)

			if got != tc.want {
				t.Errorf("resolveLink(%q, %q) = %+v, want %+v", tc.guide, tc.link, got, tc.want)
			}
		})
	}
}

// guideFixtureFiles is a complete synthetic tree: a root guide, a grouping
// directory with an index guide, a test-only package, leaf guides with
// resolvable links, and Go files inside excluded trees that need no guide.
func guideFixtureFiles() map[string]string {
	return map[string]string{
		"go.mod":                        "module example.com/hop\n\ngo 1.24\n",
		"AGENTS.md":                     "# root\n\n- [Commands](cmd/AGENTS.md)\n- [Internal](internal/AGENTS.md)\n- [Docs](docs/AGENTS.md)\n- [Upstream](https://example.com/upstream)\n",
		"docs/AGENTS.md":                "# docs\n\n[Root](../AGENTS.md#purpose)\n",
		"cmd/AGENTS.md":                 "# cmd\n\n| Directory | Guide |\n| --- | --- |\n| `hop/` | [cmd/hop](hop/AGENTS.md) |\n",
		"cmd/hop/main.go":               "package main\n\nfunc main() {}\n",
		"cmd/hop/AGENTS.md":             "# cmd/hop\n\n[main.go](main.go) and the [parent](../AGENTS.md).\n",
		"internal/arch_test.go":         "package internal\n",
		"internal/AGENTS.md":            "# internal\n\n[Domain](domain/AGENTS.md)\n",
		"internal/domain/AGENTS.md":     "# internal/domain\n\n[Run](run/AGENTS.md)\n",
		"internal/domain/run/run.go":    "package run\n",
		"internal/domain/run/AGENTS.md": "# internal/domain/run\n\n[run.go](run.go) and a [reference](../../../repos/ref/README.md).\n",
		"repos/ref/ref.go":              "package ref\n",
		"testdata/sample/sample.go":     "package sample\n",
		".worktrees/copy/go.mod":        "module example.com/copy\n\ngo 1.24\n",
		".worktrees/copy/main.go":       "package main\n\nfunc main() {}\n",
	}
}

func TestGuidesFixtures(t *testing.T) {
	cases := []struct {
		name       string
		files      map[string]string // overrides of guideFixtureFiles; empty content deletes
		wantIssues []string          // every issue must match one of these, and each must be matched
		wantError  string
	}{
		{
			name: "complete tree passes",
		},
		{
			name:  "package without a guide",
			files: map[string]string{"internal/domain/run/AGENTS.md": ""},
			wantIssues: []string{
				"internal/domain/run/AGENTS.md: -: missing package guide",
				"internal/domain/AGENTS.md: run/AGENTS.md: broken local link",
			},
		},
		{
			name:  "test-only package without a guide",
			files: map[string]string{"internal/AGENTS.md": ""},
			wantIssues: []string{
				"internal/AGENTS.md: -: missing package guide",
				"AGENTS.md: internal/AGENTS.md: broken local link",
			},
		},
		{
			name:  "grouping directory without an index guide",
			files: map[string]string{"internal/domain/AGENTS.md": ""},
			wantIssues: []string{
				"internal/domain/AGENTS.md: -: missing index guide for a grouping directory",
				"internal/AGENTS.md: domain/AGENTS.md: broken local link",
			},
		},
		{
			name:       "root guide not linking a child",
			files:      map[string]string{"AGENTS.md": "# root\n\n- [Commands](cmd/AGENTS.md)\n"},
			wantIssues: []string{"AGENTS.md: internal/AGENTS.md: guide does not link its immediate child guide"},
		},
		{
			name:       "index guide not linking a child",
			files:      map[string]string{"internal/domain/AGENTS.md": "# internal/domain\n\nNo map here.\n"},
			wantIssues: []string{"internal/domain/AGENTS.md: internal/domain/run/AGENTS.md: guide does not link its immediate child guide"},
		},
		{
			name:       "package guide not linking a nested package",
			files:      map[string]string{"internal/AGENTS.md": "# internal\n\nNo map here.\n"},
			wantIssues: []string{"internal/AGENTS.md: internal/domain/AGENTS.md: guide does not link its immediate child guide"},
		},
		{
			name:       "broken local link",
			files:      map[string]string{"cmd/hop/AGENTS.md": "# cmd/hop\n\n[wire.go](wire.go) and the [parent](../AGENTS.md).\n"},
			wantIssues: []string{"cmd/hop/AGENTS.md: wire.go: broken local link"},
		},
		{
			name:       "absolute link",
			files:      map[string]string{"cmd/hop/AGENTS.md": "# cmd/hop\n\n[main.go](/cmd/hop/main.go) and the [parent](../AGENTS.md).\n"},
			wantIssues: []string{"cmd/hop/AGENTS.md: /cmd/hop/main.go: absolute link"},
		},
		{
			name:       "link leaving the repository",
			files:      map[string]string{"cmd/hop/AGENTS.md": "# cmd/hop\n\n[secret](../../../outside.md) and the [parent](../AGENTS.md).\n"},
			wantIssues: []string{"cmd/hop/AGENTS.md: ../../../outside.md: link leaves the repository"},
		},
		{
			name: "tree without packages",
			files: map[string]string{
				"cmd/hop/main.go": "", "internal/arch_test.go": "", "internal/domain/run/run.go": "",
			},
			wantError: "no Go package found",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := guideFixtureFiles()
			for rel, content := range tc.files {
				if content == "" {
					delete(files, rel)
					continue
				}
				files[rel] = content
			}
			root := t.TempDir()
			writeFiles(t, root, files)

			result, err := checkGuides(root)

			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkGuides: %v", err)
			}
			rendered := make([]string, len(result.issues))
			for i, issue := range result.issues {
				rendered[i] = issue.String()
			}
			for _, fragment := range tc.wantIssues {
				if !slices.ContainsFunc(rendered, func(s string) bool { return strings.Contains(s, fragment) }) {
					t.Errorf("no issue contains %q; got:\n%s", fragment, strings.Join(rendered, "\n"))
				}
			}
			for _, s := range rendered {
				if !slices.ContainsFunc(tc.wantIssues, func(fragment string) bool { return strings.Contains(s, fragment) }) {
					t.Errorf("unexpected issue: %s", s)
				}
			}
		})
	}
}
