// Package internal holds HOP's repository-wide checks: the architecture
// checker over import boundaries and the guide checker over AGENTS.md
// coverage. It contains test files only and is never imported.
package internal

import (
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// moduleRoot returns the directory holding the go.mod of the module that
// contains the test binary's working directory.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// readTreeFile reads a file below root. Every file this package reads comes
// through here, so a read is always confined to the tree being checked.
func readTreeFile(root, rel string) ([]byte, error) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	inside, err := filepath.Rel(root, full)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%s escapes %s", rel, root)
	}
	return os.ReadFile(full) //nolint:gosec // G304: the path is confined to the inventoried tree by the check above.
}

// goModDirective returns the value of the named single-value directive in
// root's go.mod, such as "module" or "go".
func goModDirective(root, directive string) (string, error) {
	data, err := readTreeFile(root, "go.mod")
	if err != nil {
		return "", err
	}
	for line := range strings.Lines(string(data)) {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), directive+" ")
		if ok {
			return strings.Trim(strings.TrimSpace(rest), `"`), nil
		}
	}
	return "", fmt.Errorf("go.mod in %s has no %s directive", root, directive)
}

// isExcludedDirectory reports whether a directory name is left out of every
// scan: reference checkouts, vendored modules, fixture data, and hidden or
// underscore-prefixed directories such as .worktrees, which the go tool skips
// as well.
func isExcludedDirectory(name string) bool {
	switch name {
	case "repos", "vendor", "testdata":
		return true
	}
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// excludedComponent returns the first path element of a slash-separated
// module-relative path that names an excluded directory, or "" when the path
// lies inside the scanned tree.
func excludedComponent(rel string) string {
	for component := range strings.SplitSeq(rel, "/") {
		if component != "." && isExcludedDirectory(component) {
			return component
		}
	}
	return ""
}

// sourceFile is one Go file found by the source walk.
type sourceFile struct {
	path        string // module-relative path with forward slashes
	pkg         string // module-relative package directory, "." at the root
	test        bool   // a _test.go file
	generated   bool   // carries the generated-code marker comment
	constrained bool   // carries a build constraint, so a plain build may skip it
	imports     []string
	file        *ast.File
}

// sourceTree is the result of walking one module's Go source outside the
// excluded trees.
type sourceTree struct {
	root       string
	modulePath string
	packages   map[string][]sourceFile // keyed by module-relative package directory
}

// importPath returns the import path of a module-relative package directory.
func (t *sourceTree) importPath(pkg string) string {
	if pkg == "." {
		return t.modulePath
	}
	return t.modulePath + "/" + pkg
}

// relativePackage returns the module-relative directory of a first-party
// import path.
func (t *sourceTree) relativePackage(importPath string) string {
	if importPath == t.modulePath {
		return "."
	}
	return strings.TrimPrefix(importPath, t.modulePath+"/")
}

// isFirstParty reports whether an import path belongs to this module. The
// comparison is slash-aware, so a module whose path merely extends this
// module's path is not first-party.
func (t *sourceTree) isFirstParty(importPath string) bool {
	return importPath == t.modulePath || strings.HasPrefix(importPath, t.modulePath+"/")
}

// walkSources parses every Go file below root outside the excluded trees:
// tests, generated files and files under any build constraint alike, without
// regard to the host's active build configuration. It fails on a nested
// module, an unreadable entry, a file that does not parse or a tree with no
// Go package at all.
func walkSources(root string) (*sourceTree, error) {
	modulePath, err := goModDirective(root, "module")
	if err != nil {
		return nil, err
	}
	tree := &sourceTree{root: root, modulePath: modulePath, packages: map[string][]sourceFile{}}
	fset := token.NewFileSet()
	walkErr := filepath.WalkDir(root, func(fullPath string, entry fs.DirEntry, entryErr error) error {
		if entryErr != nil {
			return entryErr
		}
		rel, relErr := filepath.Rel(root, fullPath)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if rel == "." {
				return nil
			}
			if isExcludedDirectory(entry.Name()) {
				return fs.SkipDir
			}
			if _, statErr := os.Stat(filepath.Join(fullPath, "go.mod")); statErr == nil {
				return fmt.Errorf("unexpected nested module at %s: HOP code belongs to the root module", rel)
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		file, parseErr := parseSourceFile(fset, root, rel)
		if parseErr != nil {
			return parseErr
		}
		tree.packages[file.pkg] = append(tree.packages[file.pkg], *file)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	if len(tree.packages) == 0 {
		return nil, fmt.Errorf("no Go package found below %s", root)
	}
	for _, files := range tree.packages {
		slices.SortFunc(files, func(a, b sourceFile) int { return strings.Compare(a.path, b.path) })
	}
	return tree, nil
}

// parseSourceFile parses one module-relative Go file and records its imports
// and the properties that hide it from a plain build.
func parseSourceFile(fset *token.FileSet, root, rel string) (*sourceFile, error) {
	src, err := readTreeFile(root, rel)
	if err != nil {
		return nil, err
	}
	parsed, err := parser.ParseFile(fset, rel, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", rel, err)
	}
	pkg := filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel)))
	file := &sourceFile{
		path:        rel,
		pkg:         pkg,
		test:        strings.HasSuffix(rel, "_test.go"),
		generated:   ast.IsGenerated(parsed),
		constrained: hasBuildConstraint(parsed),
		file:        parsed,
	}
	for _, spec := range parsed.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, fmt.Errorf("parse %s: import path %s: %w", rel, spec.Path.Value, err)
		}
		file.imports = append(file.imports, importPath)
	}
	return file, nil
}

// hasBuildConstraint reports whether a file carries a //go:build or +build
// line before its package clause.
func hasBuildConstraint(file *ast.File) bool {
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			break
		}
		for _, comment := range group.List {
			if constraint.IsGoBuild(comment.Text) || constraint.IsPlusBuild(comment.Text) {
				return true
			}
		}
	}
	return false
}

// writeFiles creates every file of files below root, keyed by slash-separated
// relative path, creating parent directories as needed.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}
