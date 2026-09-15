package herdr_test

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// stagedTerminalInputException is the one terminal-input method the
// allowlist currently admits: Runtime.SendText (the fixed-grammar
// launch-line fallback transport, docs/plan/phase-2-design.md section 6) is
// the adapter's own sanctioned use of a terminal-input method. Slice 6
// deletes SendText along with the app.Runtime port member and this
// exception together (docs/plan/phase-3-design.md's slice-6 work-breakdown
// row); until then, this constant names it so this file has exactly one
// place to update.
const stagedTerminalInputException = "pane.send_text"

// callAllowlist is the reviewed set of Herdr wire methods this adapter's
// production Client.Call sites may invoke (docs/plan/phase-3-design.md
// section 11, part iii of the no-injection mechanism). A function, not a
// package-level var, per this repo's gochecknoglobals lint rule.
func callAllowlist() map[string]bool {
	return map[string]bool{
		"ping":                       true, // probe.go's socket liveness check
		"pane.report_metadata":       true,
		"agent.view.set":             true,
		"agent.view.clear":           true,
		"session.snapshot":           true,
		"worktree.create":            true,
		"layout.apply":               true,
		"pane.read":                  true,
		"pane.process_info":          true,
		"pane.close":                 true,
		"workspace.create":           true,
		stagedTerminalInputException: true,
	}
}

// terminalInputMethods is Herdr's complete real input surface
// (S11-confirmed against repos/herdr/src/api/schema.rs on 0.9.0): no
// production Call site may use any of these except through the staged
// exception above. HOP delivers everything by pull
// (docs/plan/phase-3-design.md section 7); a terminal-input method reaching
// a live pane from production code is exactly the injection this rule
// exists to catch.
func terminalInputMethods() map[string]bool {
	return map[string]bool{
		"pane.send_text":  true,
		"pane.send_keys":  true,
		"pane.send_input": true,
		"agent.prompt":    true,
		"agent.send_keys": true,
		"agent.start":     true,
	}
}

// stubImporter resolves every import through real (go/importer's default gc
// export-data importer, which reads the standard library's precompiled
// export data -- no network, no module resolution) except pkgPath's own
// sibling packages by full import path, which it resolves to an empty
// stand-in package instead of erroring. Those packages' own exported types
// don't matter to this analysis: it only needs to resolve THIS package's own
// Client type and its Call method, which depend on nothing outside the
// standard library, and stubbing out an unresolvable sibling import avoids
// needing a module-aware importer (golang.org/x/tools/go/packages, which
// this stdlib-only adapter package does not depend on) just to import it.
type stubImporter struct {
	real types.Importer
	stub map[string]bool
}

func (s stubImporter) Import(path string) (*types.Package, error) {
	if s.stub[path] {
		return types.NewPackage(path, path[strings.LastIndex(path, "/")+1:]), nil
	}
	return s.real.Import(path)
}

// callSiteViolations type-checks files (one package at import path pkgPath)
// well enough to identify every reference this package makes to
// (*Client).Call BY TYPE, however it is spelled -- an ordinary
// method call, a method value assigned to a variable, or a method
// expression -- so a renamed local, an indirection through a variable, or a
// deliberately obscured call site cannot escape the way a purely
// name-and-shape-based walk could. unresolvedImports names sibling import
// paths to stub out (see stubImporter); pass nil for a self-contained
// fixture with no such imports.
//
// It reports one violation per disallowed reference: a method-expression
// reference (production code has no legitimate use for the unbound form
// when plain method-call syntax works identically), a method value that is
// not the immediate callee of a 4-argument call (the value could be invoked
// anywhere, with anything, once extracted), a direct call with the wrong
// argument count, a method argument that isn't a string literal, a
// forbidden terminal-input method, or a literal method missing from the
// reviewed allowlist. It also returns how many (*Client).Call references it
// found in total, so a caller can cross-check that number against an
// independent count rather than trusting a possibly-broken analysis that
// silently examines nothing.
func callSiteViolations(pkgPath string, unresolvedImports []string, fset *token.FileSet, files []*ast.File) (examined int, violations []string) {
	stub := make(map[string]bool, len(unresolvedImports))
	for _, path := range unresolvedImports {
		stub[path] = true
	}
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	cfg := &types.Config{
		Importer: stubImporter{real: importer.Default(), stub: stub},
		// Errors from stubbed-out sibling imports (undefined members on an
		// intentionally empty stand-in package) are expected; swallowing
		// them lets checking continue instead of aborting, and they don't
		// affect resolving this package's own Client/Call, which depends on
		// nothing from those imports.
		Error: func(error) {},
	}
	if _, err := cfg.Check(pkgPath, fset, files, info); err != nil {
		// Also expected, for the same reason the Error callback above
		// swallows individual errors: Check's own returned error just
		// summarizes that at least one occurred.
		_ = err
	}

	// directCallExprFor maps a selector to the CallExpr it is the immediate
	// callee of, for every call in the file set -- not only Call-named
	// ones -- so a (*Client).Call reference is recognized as directly
	// invoked only when IT ITSELF is that exact callee, never merely
	// present somewhere inside a call's arguments or elsewhere.
	directCallExprFor := map[*ast.SelectorExpr]*ast.CallExpr{}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					directCallExprFor[sel] = call
				}
			}
			return true
		})
	}

	allowed := callAllowlist()
	forbidden := terminalInputMethods()

	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Call" {
				return true
			}
			selection := info.Selections[sel]
			if selection == nil {
				return true // Not a method selection at all (or unresolved); not a Call reference this rule governs.
			}
			fn, ok := selection.Obj().(*types.Func)
			if !ok || fn.Name() != "Call" {
				return true
			}
			sig, ok := fn.Type().(*types.Signature)
			if !ok || sig.Recv() == nil {
				return true
			}
			recvType := sig.Recv().Type()
			if ptr, isPtr := recvType.(*types.Pointer); isPtr {
				recvType = ptr.Elem()
			}
			named, ok := recvType.(*types.Named)
			if !ok || named.Obj().Name() != "Client" || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != pkgPath {
				return true // A method named Call on some other type; out of this rule's scope.
			}

			examined++
			pos := fset.Position(sel.Pos())

			if selection.Kind() == types.MethodExpr {
				violations = append(violations, fmt.Sprintf("%s: (*Client).Call referenced as a method expression, never a direct invocation", pos))
				return true
			}

			call, direct := directCallExprFor[sel]
			if !direct {
				violations = append(violations, fmt.Sprintf("%s: Call's method value was extracted rather than invoked directly", pos))
				return true
			}
			if len(call.Args) != 4 {
				violations = append(violations, fmt.Sprintf("%s: Call invoked with %d arguments, want exactly 4", pos, len(call.Args)))
				return true
			}
			lit, ok := call.Args[1].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				violations = append(violations, fmt.Sprintf("%s: Call's method argument is not a string literal (found %T)", pos, call.Args[1]))
				return true
			}
			method, err := strconv.Unquote(lit.Value)
			if err != nil {
				violations = append(violations, fmt.Sprintf("%s: cannot unquote method literal %s: %v", pos, lit.Value, err))
				return true
			}
			if forbidden[method] && method != stagedTerminalInputException {
				violations = append(violations, fmt.Sprintf("%s: Call(%q, ...) uses a forbidden terminal-input method", pos, method))
				return true
			}
			if !allowed[method] {
				violations = append(violations, fmt.Sprintf("%s: Call(%q, ...) is not on the reviewed method allowlist", pos, method))
			}
			return true
		})
	}
	return examined, violations
}

// TestProductionCallSitesUseAllowlistedStringLiterals proves every
// production reference to (*Client).Call in this package -- resolved by
// TYPE, so a method value or method expression cannot hide from it -- is a
// direct, 4-argument invocation whose method argument is a string literal
// on the reviewed allowlist. Test files are excluded by design: probes and
// tests reach pane.send_text (and, in principle, any other wire method)
// through Client.Call directly, which is fine -- this rule governs
// production request sites only.
func TestProductionCallSitesUseAllowlistedStringLiterals(t *testing.T) {
	const pkgPath = "github.com/johnlanda/hop/internal/adapters/herdr"
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob source files: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, file)
	}

	examined, violations := callSiteViolations(pkgPath, []string{"github.com/johnlanda/hop/internal/app"}, fset, files)
	for _, v := range violations {
		t.Error(v)
	}

	// Cross-check against a plain syntactic count: every ".Call" selector in
	// this package's production files is, today, genuinely a reference to
	// (*Client).Call -- it is the only method named Call in the package.
	// Any mismatch means the type-checker silently failed to resolve a real
	// site -- exactly the silent gap this test exists to prevent -- and
	// deserves investigation rather than trusting either count blindly.
	syntactic := 0
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Call" {
				syntactic++
			}
			return true
		})
	}
	if examined != syntactic {
		t.Fatalf("type-checker resolved %d (*Client).Call references, but %d \".Call\" selectors exist syntactically", examined, syntactic)
	}
	if examined == 0 {
		t.Fatal("found no (*Client).Call references at all; the type-checking or glob is broken")
	}
}

// TestCallSiteAllowlistRuleCatchesViolations proves callSiteViolations
// itself actually catches what it claims to, against a synthetic
// self-contained package (its own Client type and Call method) rather than
// this package's real files, including the method-value escape the P1
// review finding reported: extracting Call as a value defeats a purely
// syntactic "is this a 4-arg Call(...) with a literal method" walk, but not
// this type-based one.
func TestCallSiteAllowlistRuleCatchesViolations(t *testing.T) {
	const preamble = `package fixture

type Client struct{}

func (c *Client) Call(ctx int, method string, params, result any) error { return nil }

`
	cases := []struct {
		name           string
		body           string
		wantViolations int
	}{
		{
			name:           "allowed literal passes",
			body:           `func f(c *Client, ctx int) { c.Call(ctx, "worktree.create", nil, nil) }`,
			wantViolations: 0,
		},
		{
			name:           "dynamic method argument fails",
			body:           `func f(c *Client, ctx int, method string) { c.Call(ctx, method, nil, nil) }`,
			wantViolations: 1,
		},
		{
			name:           "forbidden terminal-input method fails",
			body:           `func f(c *Client, ctx int) { c.Call(ctx, "agent.prompt", nil, nil) }`,
			wantViolations: 1,
		},
		{
			name:           "unlisted literal method fails",
			body:           `func f(c *Client, ctx int) { c.Call(ctx, "pane.frobnicate", nil, nil) }`,
			wantViolations: 1,
		},
		{
			name:           "the staged pane.send_text exception passes",
			body:           `func f(c *Client, ctx int) { c.Call(ctx, "pane.send_text", nil, nil) }`,
			wantViolations: 0,
		},
		{
			name: "a method value extracted then invoked with a dynamic method fails",
			body: `func f(c *Client, ctx int, method string) {
	call := c.Call
	call(ctx, method, nil, nil)
}`,
			wantViolations: 1,
		},
		{
			name: "a method value extracted then invoked with an otherwise-allowed literal still fails",
			body: `func f(c *Client, ctx int) {
	call := c.Call
	call(ctx, "worktree.create", nil, nil)
}`,
			wantViolations: 1,
		},
		{
			name: "a method expression reference fails even if never invoked",
			body: `func f() {
	fn := (*Client).Call
	_ = fn
}`,
			wantViolations: 1,
		},
		{
			name:           "a direct call with the wrong arity fails",
			body:           `func f(c *Client, ctx int) { c.Call(ctx) }`,
			wantViolations: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", preamble+tc.body, 0)
			if err != nil {
				t.Fatalf("parse fixture source: %v", err)
			}
			_, violations := callSiteViolations("fixture", nil, fset, []*ast.File{file})
			if len(violations) != tc.wantViolations {
				t.Errorf("violations = %v, want %d", violations, tc.wantViolations)
			}
		})
	}
}
