package internal

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// category names the architectural role of a HOP package. It decides which
// first-party categories production code may import, whether third-party and
// effectful standard packages are acceptable, and whether the package exists
// for tests alone.
type category int

const (
	// categoryComposition is a cmd package that wires adapters into the application.
	categoryComposition category = iota
	// categoryDomainShared holds identity values shared across domain modules.
	categoryDomainShared
	// categoryDomain is one domain module of pure business rules.
	categoryDomain
	// categoryApplication holds use cases and consumer-owned ports.
	categoryApplication
	// categoryDrivingAdapter translates external input into use-case calls.
	categoryDrivingAdapter
	// categoryDrivenAdapter implements application ports against an external system.
	categoryDrivenAdapter
	// categoryArchitectureTest is this package: repository-wide checks.
	categoryArchitectureTest
	// categoryIntegrationTest holds cross-adapter contract scenarios.
	categoryIntegrationTest
	// categoryTestHelper holds fakes and fixtures for one consumer's tests.
	categoryTestHelper
)

func (c category) String() string {
	switch c {
	case categoryComposition:
		return "composition"
	case categoryDomainShared:
		return "shared-domain"
	case categoryDomain:
		return "domain"
	case categoryApplication:
		return "application"
	case categoryDrivingAdapter:
		return "driving-adapter"
	case categoryDrivenAdapter:
		return "driven-adapter"
	case categoryArchitectureTest:
		return "architecture-test"
	case categoryIntegrationTest:
		return "integration-test"
	case categoryTestHelper:
		return "test-helper"
	}
	return fmt.Sprintf("category(%d)", int(c))
}

// known reports whether c is one of the declared categories.
func (c category) known() bool {
	return c >= categoryComposition && c <= categoryTestHelper
}

// testOnly reports whether packages of the category exist for tests alone and
// must stay out of every production closure.
func (c category) testOnly() bool {
	return c == categoryArchitectureTest || c == categoryIntegrationTest || c == categoryTestHelper
}

// pureStandardLibraryOnly reports whether the category may import only
// allowlisted pure standard packages.
func (c category) pureStandardLibraryOnly() bool {
	return c == categoryDomainShared || c == categoryDomain
}

// noThirdParty reports whether production code of the category takes no
// third-party dependencies.
func (c category) noThirdParty() bool {
	return c == categoryDomainShared || c == categoryDomain || c == categoryApplication
}

// mayImport reports whether production code of category c may import a
// first-party package of category dep. Dependencies point inward: domain
// modules see only shared identities, the application sees only domain
// modules, adapters see the application and domain modules but never each
// other, and only the composition root sees adapters.
func (c category) mayImport(dep category) bool {
	switch c {
	case categoryComposition:
		return dep == categoryApplication || dep == categoryDrivingAdapter || dep == categoryDrivenAdapter
	case categoryDomainShared, categoryArchitectureTest:
		return false
	case categoryDomain:
		return dep == categoryDomainShared
	case categoryApplication:
		return dep == categoryDomain || dep == categoryDomainShared
	case categoryDrivingAdapter, categoryDrivenAdapter, categoryTestHelper:
		return dep == categoryApplication || dep == categoryDomain || dep == categoryDomainShared
	case categoryIntegrationTest:
		return dep != categoryComposition && dep != categoryArchitectureTest && dep != categoryIntegrationTest
	}
	return false
}

// isPureStandardPackage reports whether a standard package is acceptable in
// domain code: value manipulation, errors and time values, never filesystem,
// network, process, environment, randomness or database access.
func isPureStandardPackage(importPath string) bool {
	switch importPath {
	case "bytes", "cmp", "errors", "fmt", "iter", "maps", "math", "math/bits", "regexp",
		"slices", "sort", "strconv", "strings", "time", "unicode", "unicode/utf8":
		return true
	}
	return false
}

// isTestStandardPackage reports whether a standard package is acceptable in
// the test files of domain code in addition to the pure set.
func isTestStandardPackage(importPath string) bool {
	switch importPath {
	case "reflect", "testing", "testing/quick":
		return true
	}
	return false
}

// forbiddenSelectors returns, per import path, the identifiers domain code
// may not reference: ambient clock and randomness sources. A nil set forbids
// every identifier of that package.
func forbiddenSelectors() map[string]map[string]bool {
	return map[string]map[string]bool{
		"time": {
			"After": true, "AfterFunc": true, "NewTicker": true, "NewTimer": true, "Now": true,
			"Since": true, "Sleep": true, "Tick": true, "Until": true,
		},
		"crypto/rand":  nil,
		"math/rand":    nil,
		"math/rand/v2": nil,
	}
}

// rule is the exact allowlist of one package. First-party entries are
// module-relative package directories; third-party entries are families that
// cover the named path and its slash-separated subpackages. The test fields
// add permissions for _test.go files only.
type rule struct {
	category       category
	firstParty     []string // packages production code may import
	standard       []string // standard packages production code may import; consulted for pure categories only
	thirdParty     []string // third-party families production code may import
	testFirstParty []string // test-helper packages only test files may import
	testStandard   []string // additional standard packages only test files may import; pure categories only
	testThirdParty []string // additional third-party families only test files may import
}

// ruleTable maps every module-relative package directory to its rule. A
// nested package needs its own entry; a parent entry grants nothing.
type ruleTable map[string]rule

// productionRules is the exact allowlist of every package in the HOP tree.
// A new package fails the architecture check until it has an entry here.
func productionRules() ruleTable {
	return ruleTable{
		"cmd/hop":  {category: categoryComposition},
		"internal": {category: categoryArchitectureTest},
	}
}

// validateRules reports every way a table contradicts the category contracts,
// so an allowlist edit cannot grant what the architecture forbids.
func validateRules(table ruleTable) []string {
	var problems []string
	report := func(pkg, format string, args ...any) {
		problems = append(problems, pkg+": "+fmt.Sprintf(format, args...))
	}
	for _, pkg := range slices.Sorted(maps.Keys(table)) {
		r := table[pkg]
		if !r.category.known() {
			report(pkg, "unknown category %s", r.category)
			continue
		}
		for _, dep := range r.firstParty {
			target, ok := table[dep]
			switch {
			case dep == pkg:
				report(pkg, "allowlists itself")
			case !ok:
				report(pkg, "allowlists %s, which has no rule", dep)
			case !r.category.mayImport(target.category):
				report(pkg, "%s code may not import %s code (%s)", r.category, target.category, dep)
			}
		}
		for _, dep := range r.testFirstParty {
			target, ok := table[dep]
			switch {
			case !ok:
				report(pkg, "test allowlist names %s, which has no rule", dep)
			case target.category != categoryTestHelper:
				report(pkg, "test files may add only test-helper packages; %s is %s", dep, target.category)
			}
		}
		if r.category.pureStandardLibraryOnly() {
			for _, imp := range r.standard {
				if !isPureStandardPackage(imp) {
					report(pkg, "effectful standard package %s is not permitted in %s code", imp, r.category)
				}
			}
			for _, imp := range r.testStandard {
				if !isPureStandardPackage(imp) && !isTestStandardPackage(imp) {
					report(pkg, "effectful standard package %s is not permitted in %s tests", imp, r.category)
				}
			}
		} else if len(r.standard) > 0 || len(r.testStandard) > 0 {
			report(pkg, "%s code may import any standard package; a standard allowlist is meaningless", r.category)
		}
		if r.category.noThirdParty() && len(r.thirdParty) > 0 {
			report(pkg, "%s code takes no third-party dependencies", r.category)
		}
		for _, family := range slices.Concat(r.thirdParty, r.testThirdParty) {
			if family == "" || strings.HasPrefix(family, "/") || strings.HasSuffix(family, "/") {
				report(pkg, "third-party family %q must be a bare import path", family)
			}
		}
	}
	return problems
}

// inFamily reports whether an import path is family itself or one of its
// subpackages. Matching is slash-aware: example.org/lib covers
// example.org/lib/sub and never example.org/library.
func inFamily(family, importPath string) bool {
	return importPath == family || strings.HasPrefix(importPath, family+"/")
}

// diagnostic is one architecture violation. Package-level findings leave file
// and imported empty.
type diagnostic struct {
	importer string // module-relative package directory
	file     string // module-relative file, when the finding is file-specific
	imported string // import path or qualified identifier, when import-specific
	rule     string
}

func (d diagnostic) String() string {
	return fmt.Sprintf("%s: %s: %s: %s", d.importer, cmp.Or(d.file, "-"), cmp.Or(d.imported, "-"), d.rule)
}

// compareDiagnostics orders diagnostics by importer, file, import and rule.
func compareDiagnostics(a, b diagnostic) int {
	return cmp.Or(
		strings.Compare(a.importer, b.importer),
		strings.Compare(a.file, b.file),
		strings.Compare(a.imported, b.imported),
		strings.Compare(a.rule, b.rule),
	)
}

// listedModule is the module metadata `go list -json` attaches to a package.
type listedModule struct {
	Path    string
	Main    bool
	Dir     string
	Replace *listedModule
}

// listedError is a load error the go tool attaches to a package listed
// with -e instead of failing the listing.
type listedError struct {
	ImportStack []string
	Pos         string
	Err         string
}

// listedPackage is the subset of `go list -json` output the checker uses.
// Dir is where the package's files physically live, which for a replaced
// module can be inside the checked tree.
type listedPackage struct {
	ImportPath string
	Dir        string
	Standard   bool
	ForTest    string
	Module     *listedModule
	Deps       []string
	Error      *listedError
	DepsErrors []listedError
}

// isBuildConstraintExclusion reports whether a load error only says that no
// file of an existing package is selected on this host. Such a package is
// not unresolved: the build matrix compiles it where its constraints apply.
func isBuildConstraintExclusion(err string) bool {
	return strings.Contains(err, "build constraints exclude all Go files")
}

// resolvedPath returns p with symbolic links evaluated, or p itself when
// they cannot be.
func resolvedPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// goList runs `go list -json` with args in dir and decodes every package it
// prints. Any listing failure, including an unresolvable import in production
// or test files, is returned as an error.
func goList(ctx context.Context, dir string, args ...string) ([]listedPackage, error) {
	cmd := exec.CommandContext(ctx, "go", append([]string{"list", "-json"}, args...)...) //nolint:gosec // G204: the executable is the fixed go tool and args are patterns chosen by this checker.
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list %s in %s: %w\n%s", strings.Join(args, " "), dir, err, strings.TrimSpace(stderr.String()))
	}
	var packages []listedPackage
	decoder := json.NewDecoder(bytes.NewReader(out))
	for decoder.More() {
		var pkg listedPackage
		if err := decoder.Decode(&pkg); err != nil {
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		packages = append(packages, pkg)
	}
	return packages, nil
}

// indexListing keys packages by import path, leaving out the per-test
// variants and generated test mains that `go list -test` adds.
func indexListing(listing []listedPackage) map[string]*listedPackage {
	index := map[string]*listedPackage{}
	for i := range listing {
		pkg := &listing[i]
		if pkg.ForTest != "" || strings.HasSuffix(pkg.ImportPath, ".test") {
			continue
		}
		index[pkg.ImportPath] = pkg
	}
	return index
}

// importKind classifies an import path.
type importKind int

const (
	importStandard importKind = iota
	importFirstParty
	importThirdParty
)

// checker evaluates one module tree against a rule table using both
// inventories: the parsed source files and the go tool's package listing.
type checker struct {
	tree         *sourceTree
	table        ruleTable
	index        map[string]*listedPackage
	resolvedRoot string // tree root with symbolic links evaluated, for comparing against go list directories
}

// excludedRoot returns the excluded tree a listed package is physically
// rooted in, such as a module replaced into repos/, or "" when the package
// lives outside the checked tree or inside its scanned part.
func (c *checker) excludedRoot(pkg *listedPackage) string {
	if pkg == nil || pkg.Dir == "" {
		return ""
	}
	rel, err := filepath.Rel(c.resolvedRoot, resolvedPath(pkg.Dir))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return excludedComponent(filepath.ToSlash(rel))
}

// report is the outcome of checking one module tree.
type report struct {
	packages    []string // module-relative package directories found by the source walk
	diagnostics []diagnostic
}

// checkTree runs both inventories over the module at root against table and
// returns every violation, sorted. It returns an error rather than
// diagnostics when the tree cannot be inventoried at all: a rule table that
// contradicts the category contracts, a nested module, unparsable source, a
// failed listing or a tree without packages.
func checkTree(ctx context.Context, root string, table ruleTable) (*report, error) {
	if problems := validateRules(table); len(problems) > 0 {
		return nil, fmt.Errorf("rule table: %s", strings.Join(problems, "; "))
	}
	tree, err := walkSources(root)
	if err != nil {
		return nil, err
	}
	listing, err := goList(ctx, root, "-deps", "-test", "./...")
	if err != nil {
		return nil, err
	}
	c := &checker{tree: tree, table: table, index: indexListing(listing), resolvedRoot: resolvedPath(root)}
	loadDiagnostics, err := c.resolveUnlistedImports(ctx)
	if err != nil {
		return nil, err
	}
	packages := slices.Sorted(maps.Keys(tree.packages))
	var diagnostics []diagnostic
	diagnostics = append(diagnostics, loadDiagnostics...)
	diagnostics = append(diagnostics, c.checkInventoryAgreement()...)
	diagnostics = append(diagnostics, c.checkRuleCoverage()...)
	for _, pkg := range packages {
		r, ok := table[pkg]
		if !ok {
			continue
		}
		diagnostics = append(diagnostics, c.checkImports(pkg, &r)...)
		if r.category.pureStandardLibraryOnly() {
			diagnostics = append(diagnostics, c.checkForbiddenReferences(pkg, r.category)...)
		}
	}
	diagnostics = append(diagnostics, c.checkProductionClosure()...)
	slices.SortFunc(diagnostics, compareDiagnostics)
	return &report{packages: packages, diagnostics: slices.Compact(diagnostics)}, nil
}

// resolveUnlistedImports classifies the imports the source walk found that
// the main listing did not reach, such as imports in build-constrained files
// the host never compiles. The listing runs with -e so that one unresolvable
// import does not hide the others; the load errors it records are returned
// as diagnostics at every file that imports the affected package.
func (c *checker) resolveUnlistedImports(ctx context.Context) ([]diagnostic, error) {
	seen := map[string]bool{}
	var unlisted []string
	for _, files := range c.tree.packages {
		for i := range files {
			for _, imp := range files[i].imports {
				if imp == "C" || seen[imp] {
					continue
				}
				seen[imp] = true
				if _, ok := c.index[imp]; !ok {
					unlisted = append(unlisted, imp)
				}
			}
		}
	}
	if len(unlisted) == 0 {
		return nil, nil
	}
	slices.Sort(unlisted)
	extra, err := goList(ctx, c.tree.root, append([]string{"-e"}, unlisted...)...)
	if err != nil {
		return nil, err
	}
	var diagnostics []diagnostic
	for i := range extra {
		pkg := &extra[i]
		if _, ok := c.index[pkg.ImportPath]; !ok {
			c.index[pkg.ImportPath] = pkg
		}
		diagnostics = append(diagnostics, c.loadDiagnostics(pkg)...)
	}
	return diagnostics, nil
}

// loadDiagnostics reports, at every file importing pkg, the load errors the
// go tool attached to it: an import it cannot resolve, or a dependency of
// that import it cannot load. A package whose files are all excluded by
// build constraints on this host is not reported; it exists, and the build
// matrix compiles it where its constraints select it.
func (c *checker) loadDiagnostics(pkg *listedPackage) []diagnostic {
	var reasons []string
	if pkg.Error != nil && !isBuildConstraintExclusion(pkg.Error.Err) {
		reasons = append(reasons, "import cannot be resolved by the go tool: "+oneLine(pkg.Error.Err))
	}
	for _, depErr := range pkg.DepsErrors {
		if !isBuildConstraintExclusion(depErr.Err) {
			reasons = append(reasons, "a dependency of the import cannot be loaded: "+oneLine(depErr.Err))
		}
	}
	if len(reasons) == 0 {
		return nil
	}
	var diagnostics []diagnostic
	for _, pkgDir := range slices.Sorted(maps.Keys(c.tree.packages)) {
		files := c.tree.packages[pkgDir]
		for i := range files {
			file := &files[i]
			if !slices.Contains(file.imports, pkg.ImportPath) {
				continue
			}
			for _, reason := range reasons {
				diagnostics = append(diagnostics, diagnostic{importer: pkgDir, file: file.path, imported: pkg.ImportPath, rule: reason + fileNature(file)})
			}
		}
	}
	return diagnostics
}

// oneLine collapses the whitespace of a multi-line go tool message.
func oneLine(message string) string {
	return strings.Join(strings.Fields(message), " ")
}

// classify decides whether an import is standard, first-party or third-party.
// Standard packages are recognized by the go tool's Standard metadata and
// first-party packages by the exact module path; everything else is
// third-party, including paths that resolve to nothing.
func (c *checker) classify(importPath string) importKind {
	if importPath == "C" {
		return importStandard
	}
	if c.tree.isFirstParty(importPath) {
		return importFirstParty
	}
	if pkg, ok := c.index[importPath]; ok && pkg.Standard {
		return importStandard
	}
	return importThirdParty
}

// checkInventoryAgreement reports packages that only one inventory sees. A
// package the go tool compiles inside an excluded tree, or one the source
// walk found that the go tool never lists, is reported by name.
func (c *checker) checkInventoryAgreement() []diagnostic {
	var diagnostics []diagnostic
	listed := map[string]bool{}
	for _, pkg := range c.index {
		if pkg.Module == nil || !pkg.Module.Main || pkg.Module.Path != c.tree.modulePath {
			continue
		}
		rel := c.tree.relativePackage(pkg.ImportPath)
		listed[rel] = true
		if component := excludedComponent(rel); component != "" {
			diagnostics = append(diagnostics, diagnostic{importer: rel, rule: fmt.Sprintf("go list compiles Go files inside the excluded tree %q; give that tree its own module or remove them", component)})
			continue
		}
		if _, ok := c.tree.packages[rel]; !ok {
			diagnostics = append(diagnostics, diagnostic{importer: rel, rule: "package is listed by go list but not found by the source walk"})
		}
	}
	for rel := range c.tree.packages {
		if !listed[rel] {
			diagnostics = append(diagnostics, diagnostic{importer: rel, rule: "package is found by the source walk but not listed by go list"})
		}
	}
	return diagnostics
}

// checkRuleCoverage reports packages without a rule and rules without a
// package, so the table is exactly the set of packages.
func (c *checker) checkRuleCoverage() []diagnostic {
	var diagnostics []diagnostic
	for pkg := range c.tree.packages {
		if _, ok := c.table[pkg]; !ok {
			diagnostics = append(diagnostics, diagnostic{importer: pkg, rule: "package has no rule in the architecture table; every package needs an exact entry and a parent's entry grants nothing"})
		}
	}
	for pkg := range c.table {
		if _, ok := c.tree.packages[pkg]; !ok {
			diagnostics = append(diagnostics, diagnostic{importer: pkg, rule: "rule names a package that does not exist in the tree"})
		}
	}
	return diagnostics
}

// checkImports evaluates every import of every file of one package against
// its rule, annotating findings in generated or build-constrained files.
func (c *checker) checkImports(pkg string, r *rule) []diagnostic {
	var diagnostics []diagnostic
	files := c.tree.packages[pkg]
	for i := range files {
		file := &files[i]
		for _, imp := range file.imports {
			var reason string
			switch c.classify(imp) {
			case importFirstParty:
				reason = c.firstPartyViolation(pkg, file, c.tree.relativePackage(imp), r)
			case importStandard:
				reason = standardViolation(imp, file, r)
			case importThirdParty:
				reason = thirdPartyViolation(imp, file, r)
			}
			if reason != "" {
				diagnostics = append(diagnostics, diagnostic{importer: pkg, file: file.path, imported: imp, rule: reason + fileNature(file)})
			}
			if tree := c.excludedRoot(c.index[imp]); tree != "" {
				diagnostics = append(diagnostics, diagnostic{importer: pkg, file: file.path, imported: imp, rule: fmt.Sprintf("imports a package rooted in the excluded tree %q", tree) + fileNature(file)})
			}
		}
	}
	return diagnostics
}

// fileNature annotates a finding with the properties that hide a file from a
// plain build of the host.
func fileNature(file *sourceFile) string {
	var notes []string
	if file.generated {
		notes = append(notes, "generated file")
	}
	if file.constrained {
		notes = append(notes, "build-constrained file")
	}
	if len(notes) == 0 {
		return ""
	}
	return " (" + strings.Join(notes, ", ") + ")"
}

// firstPartyViolation returns why file, in package pkg, may not import the
// first-party package dep, or "" when the import is allowed. A test file may
// import the package it tests and the test helpers its rule lists.
func (c *checker) firstPartyViolation(pkg string, file *sourceFile, dep string, r *rule) string {
	if dep == pkg && file.test {
		return ""
	}
	if slices.Contains(r.firstParty, dep) {
		return ""
	}
	if file.test && slices.Contains(r.testFirstParty, dep) {
		return ""
	}
	target, known := c.table[dep]
	switch {
	case !known:
		return "imports a package that has no rule in the architecture table"
	case target.category == categoryTestHelper && file.test:
		return "test helper is not in this package's test allowlist"
	case target.category.testOnly() && !file.test:
		return fmt.Sprintf("production code may not import a %s package", target.category)
	case !r.category.mayImport(target.category):
		return fmt.Sprintf("%s code may not import %s code", r.category, target.category)
	default:
		return "not in this package's exact first-party allowlist"
	}
}

// standardViolation returns why file may not import the standard package
// imp under rule r, or "" when the import is allowed. Only pure categories
// restrict the standard library.
func standardViolation(imp string, file *sourceFile, r *rule) string {
	if !r.category.pureStandardLibraryOnly() {
		return ""
	}
	if slices.Contains(r.standard, imp) {
		return ""
	}
	if file.test && slices.Contains(r.testStandard, imp) {
		return ""
	}
	if isPureStandardPackage(imp) || (file.test && isTestStandardPackage(imp)) {
		return fmt.Sprintf("standard package is not in this %s package's allowlist", r.category)
	}
	return fmt.Sprintf("effectful standard package is not permitted in %s code", r.category)
}

// thirdPartyViolation returns why file may not import the third-party package
// imp under rule r, or "" when a listed family covers it.
func thirdPartyViolation(imp string, file *sourceFile, r *rule) string {
	families := r.thirdParty
	if file.test {
		families = slices.Concat(families, r.testThirdParty)
	}
	for _, family := range families {
		if inFamily(family, imp) {
			return ""
		}
	}
	if r.category.noThirdParty() && !file.test {
		return fmt.Sprintf("%s code takes no third-party dependencies", r.category)
	}
	return "third-party package is outside this package's allowlisted families"
}

// checkForbiddenReferences reports selector references to ambient clock or
// randomness functions in the files of one pure package. The check is
// syntactic: an identifier that names an import of a watched package is taken
// to be that package.
func (c *checker) checkForbiddenReferences(pkg string, cat category) []diagnostic {
	forbidden := forbiddenSelectors()
	var diagnostics []diagnostic
	files := c.tree.packages[pkg]
	for i := range files {
		file := &files[i]
		names := map[string]string{}
		for j, spec := range file.file.Imports {
			importPath := file.imports[j]
			if _, watched := forbidden[importPath]; !watched {
				continue
			}
			name := defaultPackageName(importPath)
			if spec.Name != nil {
				name = spec.Name.Name
			}
			switch name {
			case "_":
				continue
			case ".":
				diagnostics = append(diagnostics, diagnostic{importer: pkg, file: file.path, imported: importPath, rule: fmt.Sprintf("dot import of a package with ambient clock or randomness is forbidden in %s code", cat)})
				continue
			}
			names[name] = importPath
		}
		if len(names) == 0 {
			continue
		}
		ast.Inspect(file.file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			importPath, ok := names[ident.Name]
			if !ok {
				return true
			}
			if set := forbidden[importPath]; set == nil || set[selector.Sel.Name] {
				diagnostics = append(diagnostics, diagnostic{
					importer: pkg,
					file:     file.path,
					imported: importPath + "." + selector.Sel.Name,
					rule:     fmt.Sprintf("ambient clock or randomness is forbidden in %s code; receive time and identifiers as explicit inputs", cat),
				})
			}
			return true
		})
	}
	return diagnostics
}

// defaultPackageName derives the local name an import gets without an alias:
// the last path element, or the one before a major-version suffix.
func defaultPackageName(importPath string) string {
	elements := strings.Split(importPath, "/")
	name := elements[len(elements)-1]
	if len(elements) > 1 && len(name) > 1 && name[0] == 'v' && strings.Trim(name[1:], "0123456789") == "" {
		name = elements[len(elements)-2]
	}
	return name
}

// checkProductionClosure reports production packages whose transitive
// dependency closure, as the go tool resolves it, reaches a test-only
// package, the composition root, a package without a rule or an excluded
// tree, whether the excluded tree is named by a first-party import path or
// is where a replaced third-party module physically lives.
func (c *checker) checkProductionClosure() []diagnostic {
	var diagnostics []diagnostic
	for pkg := range c.tree.packages {
		r, ok := c.table[pkg]
		if !ok || r.category.testOnly() {
			continue
		}
		listed, ok := c.index[c.tree.importPath(pkg)]
		if !ok {
			continue
		}
		for _, dep := range listed.Deps {
			if tree := c.excludedRoot(c.index[dep]); tree != "" {
				diagnostics = append(diagnostics, diagnostic{importer: pkg, imported: dep, rule: fmt.Sprintf("production closure reaches the excluded tree %q", tree)})
				continue
			}
			if !c.tree.isFirstParty(dep) {
				continue
			}
			rel := c.tree.relativePackage(dep)
			if component := excludedComponent(rel); component != "" {
				diagnostics = append(diagnostics, diagnostic{importer: pkg, imported: dep, rule: fmt.Sprintf("production closure reaches the excluded tree %q", component)})
				continue
			}
			target, known := c.table[rel]
			switch {
			case !known:
				diagnostics = append(diagnostics, diagnostic{importer: pkg, imported: dep, rule: "production closure reaches a package that has no rule in the architecture table"})
			case target.category.testOnly():
				diagnostics = append(diagnostics, diagnostic{importer: pkg, imported: dep, rule: fmt.Sprintf("production closure reaches a %s package", target.category)})
			case target.category == categoryComposition:
				diagnostics = append(diagnostics, diagnostic{importer: pkg, imported: dep, rule: "production closure reaches the composition root"})
			}
		}
	}
	return diagnostics
}

// TestArchitectureRuleTableIsValid asserts that the production rule table
// itself respects the category contracts.
func TestArchitectureRuleTableIsValid(t *testing.T) {
	if problems := validateRules(productionRules()); len(problems) > 0 {
		t.Fatalf("production rule table:\n%s", strings.Join(problems, "\n"))
	}
}

// TestArchitectureRealTree checks the HOP tree itself. The inventory must
// contain the packages known to exist, so an enumeration that found nothing
// cannot pass.
func TestArchitectureRealTree(t *testing.T) {
	root := moduleRoot(t)

	result, err := checkTree(t.Context(), root, productionRules())
	if err != nil {
		t.Fatalf("checking %s: %v", root, err)
	}

	for _, want := range []string{"cmd/hop", "internal"} {
		if !slices.Contains(result.packages, want) {
			t.Errorf("the inventory does not contain %s; it found %v", want, result.packages)
		}
	}
	for _, d := range result.diagnostics {
		t.Error(d)
	}
}

func TestArchitectureThirdPartyFamilyMatching(t *testing.T) {
	cases := []struct {
		name       string
		family     string
		importPath string
		want       bool
	}{
		{name: "family itself", family: "example.org/lib", importPath: "example.org/lib", want: true},
		{name: "subpackage", family: "example.org/lib", importPath: "example.org/lib/sub", want: true},
		{name: "nested subpackage", family: "example.org/lib", importPath: "example.org/lib/sub/deep", want: true},
		{name: "longer sibling name", family: "example.org/lib", importPath: "example.org/library", want: false},
		{name: "hyphenated sibling name", family: "example.org/lib", importPath: "example.org/lib-extra", want: false},
		{name: "parent path", family: "example.org/lib", importPath: "example.org", want: false},
		{name: "different host", family: "example.org/lib", importPath: "example.com/lib", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := inFamily(tc.family, tc.importPath)

			if got != tc.want {
				t.Errorf("inFamily(%q, %q) = %t, want %t", tc.family, tc.importPath, got, tc.want)
			}
		})
	}
}

func TestArchitectureDefaultPackageName(t *testing.T) {
	cases := []struct {
		name       string
		importPath string
		want       string
	}{
		{name: "single element", importPath: "time", want: "time"},
		{name: "nested standard package", importPath: "math/rand", want: "rand"},
		{name: "major version suffix", importPath: "math/rand/v2", want: "rand"},
		{name: "element that only starts with v", importPath: "example.org/lib/vault", want: "vault"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := defaultPackageName(tc.importPath)

			if got != tc.want {
				t.Errorf("defaultPackageName(%q) = %q, want %q", tc.importPath, got, tc.want)
			}
		})
	}
}

// fixtureModulePath is the module path of the synthetic trees. Its prefix is
// shared with example.com/hopper so that misleading prefixes are exercised.
const fixtureModulePath = "example.com/hop"

// fixtureRules is the rule table of the synthetic tree: one package of every
// category, mirroring the planned HOP layout. The workflow domain module and
// the system adapter are imported by nothing, so an outward import added to
// or toward them is a rule violation without being an import cycle.
func fixtureRules() ruleTable {
	return ruleTable{
		"cmd/fix":                  {category: categoryComposition, firstParty: []string{"internal/adapters/cli", "internal/adapters/sqlite"}},
		"internal/domain/identity": {category: categoryDomainShared, standard: []string{"strings"}},
		"internal/domain/run":      {category: categoryDomain, firstParty: []string{"internal/domain/identity"}, standard: []string{"strings", "time"}, testStandard: []string{"testing"}},
		"internal/domain/workflow": {category: categoryDomain, standard: []string{"strings"}},
		"internal/app":             {category: categoryApplication, firstParty: []string{"internal/domain/run"}, testFirstParty: []string{"internal/apptest"}},
		"internal/apptest":         {category: categoryTestHelper, firstParty: []string{"internal/app", "internal/domain/run"}},
		"internal/adapters/cli":    {category: categoryDrivingAdapter, firstParty: []string{"internal/app"}},
		"internal/adapters/sqlite": {category: categoryDrivenAdapter, firstParty: []string{"internal/app", "internal/domain/run"}, thirdParty: []string{"example.org/lib"}},
		"internal/adapters/herdr":  {category: categoryDrivenAdapter, firstParty: []string{"internal/app"}},
		"internal/adapters/system": {category: categoryDrivenAdapter},
		"test/integration":         {category: categoryIntegrationTest, firstParty: []string{"internal/adapters/sqlite", "internal/domain/run"}},
	}
}

// fixtureModuleFiles is the clean synthetic module: inward imports, a
// domain-local helper, composition wiring, an approved adapter library with a
// subpackage, an external test importing its package, a test helper used by
// an application test, and Go files inside excluded trees that no inventory
// may reach.
func fixtureModuleFiles() map[string]string {
	return map[string]string{
		"internal/domain/identity/id.go":    "package identity\n\nimport \"strings\"\n\n// RunID identifies a run.\ntype RunID string\n\n// ParseRunID trims and wraps a raw identifier.\nfunc ParseRunID(raw string) RunID { return RunID(strings.TrimSpace(raw)) }\n",
		"internal/domain/run/run.go":        "package run\n\nimport (\n\t\"time\"\n\n\t\"example.com/hop/internal/domain/identity\"\n)\n\n// Run is one orchestration run.\ntype Run struct {\n\tID        identity.RunID\n\tStartedAt time.Time\n}\n\n// Title returns the normalized title.\nfunc (r Run) Title(raw string) string { return normalize(raw) }\n",
		"internal/domain/run/helper.go":     "package run\n\nimport \"strings\"\n\nfunc normalize(s string) string { return strings.TrimSpace(s) }\n",
		"internal/domain/run/run_test.go":   "package run_test\n\nimport (\n\t\"testing\"\n\n\t\"example.com/hop/internal/domain/run\"\n)\n\nfunc TestRun(t *testing.T) {\n\tif (run.Run{}).Title(\" x \") != \"x\" {\n\t\tt.Fatal(\"title\")\n\t}\n}\n",
		"internal/domain/workflow/step.go":  "package workflow\n\nimport \"strings\"\n\n// Step is one playbook step.\ntype Step struct {\n\tName string\n}\n\n// Normalized returns the trimmed step name.\nfunc (s Step) Normalized() string { return strings.TrimSpace(s.Name) }\n",
		"internal/app/service.go":           "package app\n\nimport (\n\t\"context\"\n\n\t\"example.com/hop/internal/domain/run\"\n)\n\n// Runs stores runs.\ntype Runs interface {\n\tSave(context.Context, run.Run) error\n}\n",
		"internal/app/service_test.go":      "package app_test\n\nimport (\n\t\"testing\"\n\n\t\"example.com/hop/internal/apptest\"\n)\n\nfunc TestService(t *testing.T) {\n\tif apptest.NewRuns() == nil {\n\t\tt.Fatal(\"fake\")\n\t}\n}\n",
		"internal/apptest/fake.go":          "package apptest\n\nimport (\n\t\"context\"\n\n\t\"example.com/hop/internal/app\"\n\t\"example.com/hop/internal/domain/run\"\n)\n\n// Runs is an in-memory app.Runs.\ntype Runs struct{}\n\n// NewRuns returns an empty fake.\nfunc NewRuns() *Runs { return &Runs{} }\n\n// Save records nothing.\nfunc (*Runs) Save(context.Context, run.Run) error { return nil }\n\nvar _ app.Runs = (*Runs)(nil)\n",
		"internal/adapters/cli/cli.go":      "package cli\n\nimport \"example.com/hop/internal/app\"\n\n// Execute runs the command line against the use cases.\nfunc Execute(app.Runs) {}\n",
		"internal/adapters/sqlite/store.go": "package sqlite\n\nimport (\n\t\"context\"\n\n\t\"example.org/lib\"\n\t\"example.org/lib/sub\"\n\n\t\"example.com/hop/internal/app\"\n\t\"example.com/hop/internal/domain/run\"\n)\n\n// Store persists runs.\ntype Store struct{}\n\n// Save persists one run.\nfunc (*Store) Save(context.Context, run.Run) error {\n\tlib.Do()\n\tsub.Do()\n\treturn nil\n}\n\nvar _ app.Runs = (*Store)(nil)\n",
		"internal/adapters/herdr/client.go": "package herdr\n\nimport \"example.com/hop/internal/app\"\n\n// New builds the runtime adapter.\nfunc New(app.Runs) {}\n",
		"internal/adapters/system/clock.go": "package system\n\nimport \"time\"\n\n// Now reads the wall clock.\nfunc Now() time.Time { return time.Now() }\n",
		"cmd/fix/main.go":                   "package main\n\nimport (\n\t\"example.com/hop/internal/adapters/cli\"\n\t\"example.com/hop/internal/adapters/sqlite\"\n)\n\nfunc main() { cli.Execute(&sqlite.Store{}) }\n",
		"test/integration/store_test.go":    "package integration\n\nimport (\n\t\"testing\"\n\n\t\"example.com/hop/internal/adapters/sqlite\"\n\t\"example.com/hop/internal/domain/run\"\n)\n\nfunc TestStore(t *testing.T) {\n\tvar s sqlite.Store\n\tif err := s.Save(t.Context(), run.Run{}); err != nil {\n\t\tt.Fatal(err)\n\t}\n}\n",
		"testdata/ignored/ignored.go":       "package ignored\n\nimport _ \"os\"\n",
		".worktrees/copy/go.mod":            "module example.com/copy\n\ngo 1.24\n",
		".worktrees/copy/main.go":           "package main\n\nimport _ \"os\"\n\nfunc main() {}\n",
	}
}

// fixtureThirdPartyModules are local modules the synthetic go.mod replaces
// the third-party requirements with, so no network is needed.
func fixtureThirdPartyModules(goVersion string) map[string]string {
	return map[string]string{
		"lib/go.mod":         "module example.org/lib\n\ngo " + goVersion + "\n",
		"lib/lib.go":         "package lib\n\n// Do does nothing.\nfunc Do() {}\n",
		"lib/sub/sub.go":     "package sub\n\n// Do does nothing.\nfunc Do() {}\n",
		"library/go.mod":     "module example.org/library\n\ngo " + goVersion + "\n",
		"library/library.go": "package library\n\n// Do does nothing.\nfunc Do() {}\n",
		"hopper/go.mod":      "module example.com/hopper\n\ngo " + goVersion + "\n",
		"hopper/x/x.go":      "package x\n\n// Do does nothing.\nfunc Do() {}\n",
	}
}

// fixtureGoMod is the synthetic module's go.mod: the real tree's go directive
// and local replacements for every third-party module the fixtures import.
func fixtureGoMod(goVersion string) string {
	return "module " + fixtureModulePath + "\n\n" +
		"go " + goVersion + "\n\n" +
		"require (\n\texample.com/hopper v0.0.0\n\texample.org/lib v0.0.0\n\texample.org/library v0.0.0\n)\n\n" +
		"replace (\n\texample.com/hopper => ../thirdparty/hopper\n\texample.org/lib => ../thirdparty/lib\n\texample.org/library => ../thirdparty/library\n)\n"
}

// writeFixtureModule materializes the clean synthetic module with overrides
// applied (an empty content deletes the file) and returns the module root.
func writeFixtureModule(t *testing.T, goVersion string, overrides map[string]string) string {
	t.Helper()
	tmp := t.TempDir()
	files := fixtureModuleFiles()
	files["go.mod"] = fixtureGoMod(goVersion)
	for rel, content := range overrides {
		if content == "" {
			delete(files, rel)
			continue
		}
		files[rel] = content
	}
	root := filepath.Join(tmp, "module")
	writeFiles(t, root, files)
	writeFiles(t, filepath.Join(tmp, "thirdparty"), fixtureThirdPartyModules(goVersion))
	return root
}

// withRule returns a copy of table with one entry replaced.
func withRule(table ruleTable, pkg string, r *rule) ruleTable {
	copied := maps.Clone(table)
	copied[pkg] = *r
	return copied
}

func TestArchitectureFixtures(t *testing.T) {
	goVersion, err := goModDirective(moduleRoot(t), "go")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name            string
		files           map[string]string
		rules           func(ruleTable) ruleTable
		wantDiagnostics []string // every diagnostic must match one of these, and each must be matched
		wantError       string
	}{
		{
			name: "clean tree passes",
		},
		{
			name:            "unknown top-level package",
			files:           map[string]string{"internal/rogue/rogue.go": "package rogue\n"},
			wantDiagnostics: []string{"internal/rogue: -: -: package has no rule"},
		},
		{
			name:            "unknown nested package does not inherit its parent's rule",
			files:           map[string]string{"internal/domain/run/inner/inner.go": "package inner\n"},
			wantDiagnostics: []string{"internal/domain/run/inner: -: -: package has no rule"},
		},
		{
			name: "rule for a package that does not exist",
			rules: func(table ruleTable) ruleTable {
				return withRule(table, "internal/adapters/gone", &rule{category: categoryDrivenAdapter})
			},
			wantDiagnostics: []string{
				"internal/adapters/gone: -: -: rule names a package that does not exist",
			},
		},
		{
			name:            "domain importing the application",
			files:           map[string]string{"internal/domain/workflow/outward.go": "package workflow\n\nimport _ \"example.com/hop/internal/app\"\n"},
			wantDiagnostics: []string{"internal/domain/workflow: internal/domain/workflow/outward.go: example.com/hop/internal/app: domain code may not import application code"},
		},
		{
			name:            "domain importing an adapter",
			files:           map[string]string{"internal/domain/workflow/outward.go": "package workflow\n\nimport _ \"example.com/hop/internal/adapters/system\"\n"},
			wantDiagnostics: []string{"internal/domain/workflow: internal/domain/workflow/outward.go: example.com/hop/internal/adapters/system: domain code may not import driven-adapter code"},
		},
		{
			name:            "domain sibling import",
			files:           map[string]string{"internal/domain/workflow/outward.go": "package workflow\n\nimport _ \"example.com/hop/internal/domain/run\"\n"},
			wantDiagnostics: []string{"internal/domain/workflow: internal/domain/workflow/outward.go: example.com/hop/internal/domain/run: domain code may not import domain code"},
		},
		{
			name:            "shared domain importing a domain module",
			files:           map[string]string{"internal/domain/identity/outward.go": "package identity\n\nimport _ \"example.com/hop/internal/domain/workflow\"\n"},
			wantDiagnostics: []string{"internal/domain/identity: internal/domain/identity/outward.go: example.com/hop/internal/domain/workflow: shared-domain code may not import domain code"},
		},
		{
			name:            "application importing an adapter",
			files:           map[string]string{"internal/app/outward.go": "package app\n\nimport _ \"example.com/hop/internal/adapters/system\"\n"},
			wantDiagnostics: []string{"internal/app: internal/app/outward.go: example.com/hop/internal/adapters/system: application code may not import driven-adapter code"},
		},
		{
			name:      "outward import that closes a cycle fails the listing",
			files:     map[string]string{"internal/domain/run/outward.go": "package run\n\nimport _ \"example.com/hop/internal/app\"\n"},
			wantError: "import cycle not allowed",
		},
		{
			name:            "adapter importing a sibling adapter",
			files:           map[string]string{"internal/adapters/cli/outward.go": "package cli\n\nimport _ \"example.com/hop/internal/adapters/sqlite\"\n"},
			wantDiagnostics: []string{"internal/adapters/cli: internal/adapters/cli/outward.go: example.com/hop/internal/adapters/sqlite: driving-adapter code may not import driven-adapter code"},
		},
		{
			name:            "driven adapter importing a sibling driven adapter",
			files:           map[string]string{"internal/adapters/herdr/outward.go": "package herdr\n\nimport _ \"example.com/hop/internal/adapters/sqlite\"\n"},
			wantDiagnostics: []string{"internal/adapters/herdr: internal/adapters/herdr/outward.go: example.com/hop/internal/adapters/sqlite: driven-adapter code may not import driven-adapter code"},
		},
		{
			name:            "effectful standard package in domain code",
			files:           map[string]string{"internal/domain/run/effect.go": "package run\n\nimport _ \"os\"\n"},
			wantDiagnostics: []string{"internal/domain/run: internal/domain/run/effect.go: os: effectful standard package is not permitted in domain code"},
		},
		{
			name:            "pure standard package missing from the domain allowlist",
			files:           map[string]string{"internal/domain/run/extra.go": "package run\n\nimport _ \"regexp\"\n"},
			wantDiagnostics: []string{"internal/domain/run: internal/domain/run/extra.go: regexp: standard package is not in this domain package's allowlist"},
		},
		{
			name:            "hidden build-constrained import",
			files:           map[string]string{"internal/domain/run/hidden.go": "//go:build ignore\n\npackage run\n\nimport _ \"net/http\"\n"},
			wantDiagnostics: []string{"internal/domain/run: internal/domain/run/hidden.go: net/http: effectful standard package is not permitted in domain code (build-constrained file)"},
		},
		{
			name:            "test-only violation",
			files:           map[string]string{"internal/domain/run/outward_test.go": "package run_test\n\nimport _ \"example.com/hop/internal/adapters/sqlite\"\n"},
			wantDiagnostics: []string{"internal/domain/run: internal/domain/run/outward_test.go: example.com/hop/internal/adapters/sqlite: domain code may not import driven-adapter code"},
		},
		{
			name:            "same-package test needing an unlisted standard package",
			files:           map[string]string{"internal/domain/run/os_test.go": "package run\n\nimport _ \"os\"\n"},
			wantDiagnostics: []string{"internal/domain/run: internal/domain/run/os_test.go: os: effectful standard package is not permitted in domain code"},
		},
		{
			name:            "generated import is checked",
			files:           map[string]string{"internal/app/generated.go": "// Code generated by fixture. DO NOT EDIT.\n\npackage app\n\nimport _ \"example.com/hop/internal/adapters/system\"\n"},
			wantDiagnostics: []string{"internal/app: internal/app/generated.go: example.com/hop/internal/adapters/system: application code may not import driven-adapter code (generated file)"},
		},
		{
			name:            "misleading third-party prefix is not the allowed family",
			files:           map[string]string{"internal/adapters/sqlite/library.go": "package sqlite\n\nimport \"example.org/library\"\n\nfunc init() { library.Do() }\n"},
			wantDiagnostics: []string{"internal/adapters/sqlite: internal/adapters/sqlite/library.go: example.org/library: third-party package is outside this package's allowlisted families"},
		},
		{
			name:            "misleading first-party prefix is third-party",
			files:           map[string]string{"internal/app/hopper.go": "package app\n\nimport \"example.com/hopper/x\"\n\nfunc init() { x.Do() }\n"},
			wantDiagnostics: []string{"internal/app: internal/app/hopper.go: example.com/hopper/x: application code takes no third-party dependencies"},
		},
		{
			name:  "test helper imported by production code",
			files: map[string]string{"internal/adapters/herdr/helper.go": "package herdr\n\nimport _ \"example.com/hop/internal/apptest\"\n"},
			wantDiagnostics: []string{
				"internal/adapters/herdr: internal/adapters/herdr/helper.go: example.com/hop/internal/apptest: production code may not import a test-helper package",
				"internal/adapters/herdr: -: example.com/hop/internal/apptest: production closure reaches a test-helper package",
			},
		},
		{
			name:            "test helper not in the test allowlist",
			files:           map[string]string{"internal/adapters/cli/cli_test.go": "package cli_test\n\nimport _ \"example.com/hop/internal/apptest\"\n"},
			wantDiagnostics: []string{"internal/adapters/cli: internal/adapters/cli/cli_test.go: example.com/hop/internal/apptest: test helper is not in this package's test allowlist"},
		},
		{
			name:            "ambient clock in domain code",
			files:           map[string]string{"internal/domain/run/clock.go": "package run\n\nimport \"time\"\n\nfunc started() time.Time { return time.Now() }\n"},
			wantDiagnostics: []string{"internal/domain/run: internal/domain/run/clock.go: time.Now: ambient clock or randomness is forbidden in domain code"},
		},
		{
			name:            "ambient clock through an alias and a function value",
			files:           map[string]string{"internal/domain/run/clock.go": "package run\n\nimport clock \"time\"\n\nvar now = clock.Now\n"},
			wantDiagnostics: []string{"internal/domain/run: internal/domain/run/clock.go: time.Now: ambient clock or randomness is forbidden in domain code"},
		},
		{
			name:  "randomness in domain code",
			files: map[string]string{"internal/domain/run/rand.go": "package run\n\nimport \"math/rand/v2\"\n\nfunc pick() int { return rand.IntN(3) }\n"},
			wantDiagnostics: []string{
				"internal/domain/run: internal/domain/run/rand.go: math/rand/v2: effectful standard package is not permitted in domain code",
				"internal/domain/run: internal/domain/run/rand.go: math/rand/v2.IntN: ambient clock or randomness is forbidden in domain code",
			},
		},
		{
			name:            "Go files inside a reference checkout without a module",
			files:           map[string]string{"repos/ref/ref.go": "package ref\n"},
			wantDiagnostics: []string{`repos/ref: -: -: go list compiles Go files inside the excluded tree "repos"`},
		},
		{
			name: "approved library replaced into a reference checkout",
			files: map[string]string{
				"go.mod":               strings.ReplaceAll(fixtureGoMod(goVersion), "example.org/lib => ../thirdparty/lib", "example.org/lib => ./repos/lib"),
				"repos/lib/go.mod":     "module example.org/lib\n\ngo " + goVersion + "\n",
				"repos/lib/lib.go":     "package lib\n\n// Do does nothing.\nfunc Do() {}\n",
				"repos/lib/sub/sub.go": "package sub\n\n// Do does nothing.\nfunc Do() {}\n",
			},
			wantDiagnostics: []string{
				`internal/adapters/sqlite: internal/adapters/sqlite/store.go: example.org/lib: imports a package rooted in the excluded tree "repos"`,
				`internal/adapters/sqlite: internal/adapters/sqlite/store.go: example.org/lib/sub: imports a package rooted in the excluded tree "repos"`,
				`internal/adapters/sqlite: -: example.org/lib: production closure reaches the excluded tree "repos"`,
				`internal/adapters/sqlite: -: example.org/lib/sub: production closure reaches the excluded tree "repos"`,
				`cmd/fix: -: example.org/lib: production closure reaches the excluded tree "repos"`,
				`cmd/fix: -: example.org/lib/sub: production closure reaches the excluded tree "repos"`,
			},
		},
		{
			name:            "hidden import of a missing package inside an approved family",
			files:           map[string]string{"internal/adapters/sqlite/hidden.go": "//go:build ignore\n\npackage sqlite\n\nimport _ \"example.org/lib/missing\"\n"},
			wantDiagnostics: []string{"internal/adapters/sqlite: internal/adapters/sqlite/hidden.go: example.org/lib/missing: import cannot be resolved by the go tool: "},
		},
		{
			name:  "platform-constrained standard package is not a load error",
			files: map[string]string{"internal/adapters/sqlite/wasm.go": "//go:build js && wasm\n\npackage sqlite\n\nimport _ \"syscall/js\"\n"},
		},
		{
			name: "rule table granting an outward import",
			rules: func(table ruleTable) ruleTable {
				return withRule(table, "internal/domain/run", &rule{category: categoryDomain, firstParty: []string{"internal/app"}})
			},
			wantError: "rule table: internal/domain/run: domain code may not import application code",
		},
		{
			name: "rule table granting an effectful standard package",
			rules: func(table ruleTable) ruleTable {
				return withRule(table, "internal/domain/run", &rule{category: categoryDomain, standard: []string{"os"}})
			},
			wantError: "effectful standard package os is not permitted in domain code",
		},
		{
			name: "rule table granting a third-party family to the application",
			rules: func(table ruleTable) ruleTable {
				return withRule(table, "internal/app", &rule{category: categoryApplication, thirdParty: []string{"example.org/lib"}})
			},
			wantError: "application code takes no third-party dependencies",
		},
		{
			name:      "unexpected nested module",
			files:     map[string]string{"internal/hidden/go.mod": "module example.com/hop/internal/hidden\n\ngo " + goVersion + "\n", "internal/hidden/hidden.go": "package hidden\n"},
			wantError: "unexpected nested module at internal/hidden",
		},
		{
			name:      "malformed source",
			files:     map[string]string{"internal/app/broken.go": "package app\n\nimport (\n\t\"context\"\n"},
			wantError: "parse internal/app/broken.go",
		},
		{
			name:      "failed listing",
			files:     map[string]string{"internal/app/missing.go": "package app\n\nimport _ \"example.com/hop/internal/missing\"\n"},
			wantError: "go list -deps -test ./...",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := writeFixtureModule(t, goVersion, tc.files)
			table := fixtureRules()
			if tc.rules != nil {
				table = tc.rules(table)
			}

			result, err := checkTree(t.Context(), root, table)

			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkTree: %v", err)
			}
			assertDiagnostics(t, result.diagnostics, tc.wantDiagnostics)
		})
	}
}

// TestArchitectureCleanFixtureCompiles asserts that the clean synthetic
// module type-checks, production and test files alike, so that its passing
// the checker is evidence about a valid tree rather than about one the go
// tool merely lists.
func TestArchitectureCleanFixtureCompiles(t *testing.T) {
	goVersion, err := goModDirective(moduleRoot(t), "go")
	if err != nil {
		t.Fatal(err)
	}
	root := writeFixtureModule(t, goVersion, nil)

	cmd := exec.CommandContext(t.Context(), "go", "vet", "./...")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go vet ./... in the clean fixture: %v\n%s", err, out)
	}
}

// assertDiagnostics fails unless every expected fragment matches some
// diagnostic and every diagnostic matches some expected fragment.
func assertDiagnostics(t *testing.T, got []diagnostic, want []string) {
	t.Helper()
	rendered := make([]string, len(got))
	for i, d := range got {
		rendered[i] = d.String()
	}
	for _, fragment := range want {
		if !slices.ContainsFunc(rendered, func(s string) bool { return strings.Contains(s, fragment) }) {
			t.Errorf("no diagnostic contains %q; got:\n%s", fragment, strings.Join(rendered, "\n"))
		}
	}
	for _, s := range rendered {
		if !slices.ContainsFunc(want, func(fragment string) bool { return strings.Contains(s, fragment) }) {
			t.Errorf("unexpected diagnostic: %s", s)
		}
	}
}
