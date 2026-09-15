package herdr_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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

// callSiteViolations walks file for every 4-argument `X.Call(...)`
// invocation — the exact shape of Client.Call(ctx, method, params, result)
// — and reports one message per disallowed one: a method argument that
// isn't a string literal (a dynamic method expression would escape this
// whole mechanism), a forbidden terminal-input method, or a literal method
// missing from the reviewed allowlist. It also returns how many call sites
// it examined, so a caller can tell "found nothing to check" apart from
// "found sites, all clean."
func callSiteViolations(fset *token.FileSet, file *ast.File) (examined int, violations []string) {
	allowed := callAllowlist()
	forbidden := terminalInputMethods()
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Call" || len(call.Args) != 4 {
			return true
		}
		examined++
		pos := fset.Position(call.Pos())
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
	return examined, violations
}

// TestProductionCallSitesUseAllowlistedStringLiterals proves every
// production Client.Call site in this package uses a string-literal method
// on the reviewed allowlist. Test files are excluded by design: probes and
// tests reach pane.send_text (and, in principle, any other wire method)
// through Client.Call directly, which is fine — this rule governs
// production request sites only.
func TestProductionCallSitesUseAllowlistedStringLiterals(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob source files: %v", err)
	}
	fset := token.NewFileSet()
	totalSites := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		examined, violations := callSiteViolations(fset, file)
		totalSites += examined
		for _, v := range violations {
			t.Errorf("%s: %s", name, v)
		}
	}
	if totalSites == 0 {
		t.Fatal("found no Client.Call sites in production source; the AST walk or glob is broken")
	}
}

// TestCallSiteAllowlistRuleCatchesViolations proves callSiteViolations
// itself actually catches what it claims to, against synthetic sources
// rather than this package's real files.
func TestCallSiteAllowlistRuleCatchesViolations(t *testing.T) {
	cases := []struct {
		name       string
		src        string
		wantErrors int
	}{
		{
			name:       "allowed literal passes",
			src:        `package fixture; func f(c *client) { c.Call(ctx, "worktree.create", nil, nil) }`,
			wantErrors: 0,
		},
		{
			name:       "dynamic method expression fails",
			src:        `package fixture; func f(c *client, method string) { c.Call(ctx, method, nil, nil) }`,
			wantErrors: 1,
		},
		{
			name:       "forbidden terminal-input method fails",
			src:        `package fixture; func f(c *client) { c.Call(ctx, "agent.prompt", nil, nil) }`,
			wantErrors: 1,
		},
		{
			name:       "unlisted literal method fails",
			src:        `package fixture; func f(c *client) { c.Call(ctx, "pane.frobnicate", nil, nil) }`,
			wantErrors: 1,
		},
		{
			name:       "the staged pane.send_text exception passes",
			src:        `package fixture; func f(c *client) { c.Call(ctx, "pane.send_text", nil, nil) }`,
			wantErrors: 0,
		},
		{
			name:       "a same-named Call with the wrong arity is ignored",
			src:        `package fixture; func f(c *client) { c.Call(ctx, "whatever") }`,
			wantErrors: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse fixture source: %v", err)
			}
			_, violations := callSiteViolations(fset, file)
			if len(violations) != tc.wantErrors {
				t.Errorf("violations = %v, want %d", violations, tc.wantErrors)
			}
		})
	}
}
