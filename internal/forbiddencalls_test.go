package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The type-aware forbidden-call rule (docs/plan/phase-3-design.md section
// 7): production code in internal/app and cmd/hop may not reference a
// method named SendText, SendKeys or Prompt declared on any type of
// internal/adapters/herdr or of internal/app (the ports), and may not
// call herdr's Client.Call at all — the raw transport escapes any
// method-name rule, so raw calls are confined to the adapter package.
// The check resolves methods through go/types over the compiler's export
// data, so chained selectors, interface and type aliases, method values
// and method expressions all resolve to the declaring package; the
// package-selector pattern used for time.Now would miss every one of
// them. It covers the production files the go tool compiles on this host
// (GoFiles); _test.go files are out of scope by design — probes and
// adapter tests reach pane.send_text through Client.Call in test files
// only, inside the adapter package.

// forbiddenCallTargets are the module-relative packages the rule covers.
func forbiddenCallTargets() []string {
	return []string{"cmd/hop", "internal/app"}
}

// forbiddenTransportMethodNames are the typed-input method names no
// covered package's production code may reference on a herdr-declared or
// app-declared type.
func forbiddenTransportMethodNames() map[string]bool {
	return map[string]bool{"SendText": true, "SendKeys": true, "Prompt": true}
}

// exportedPackage is the subset of `go list -export -deps -json` output
// the forbidden-call checker uses.
type exportedPackage struct {
	ImportPath string
	Name       string
	Dir        string
	Export     string
	Standard   bool
	GoFiles    []string
}

// listExportedPackages runs `go list -export -deps -json` for patterns in
// dir and decodes every package it prints. -export makes the go tool
// compile each package and report the file holding its export data, which
// the type checker's importer reads back.
func listExportedPackages(ctx context.Context, dir string, patterns ...string) ([]exportedPackage, error) {
	cmd := exec.CommandContext(ctx, "go", append([]string{"list", "-export", "-deps", "-json"}, patterns...)...) //nolint:gosec // G204: the executable is the fixed go tool and args are patterns chosen by this checker.
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -export %s in %s: %w\n%s", strings.Join(patterns, " "), dir, err, strings.TrimSpace(stderr.String()))
	}
	var packages []exportedPackage
	decoder := json.NewDecoder(bytes.NewReader(out))
	for decoder.More() {
		var pkg exportedPackage
		if err := decoder.Decode(&pkg); err != nil {
			return nil, fmt.Errorf("decode go list -export output: %w", err)
		}
		packages = append(packages, pkg)
	}
	return packages, nil
}

// forbiddenCallFinding is one violation of the forbidden-call rule.
type forbiddenCallFinding struct {
	pkg      string // module-relative package directory
	position string // file:line:column, module-relative
	call     string // the resolved method, e.g. (module/internal/app.Runtime).SendText
	rule     string
}

func (f forbiddenCallFinding) String() string {
	return fmt.Sprintf("%s: %s: %s: %s", f.pkg, f.position, f.call, f.rule)
}

// checkForbiddenCalls type-checks each target package's production files
// against the export data of its dependencies and reports every forbidden
// method reference. A tree that cannot be listed, parsed or type-checked
// is an error, never a pass.
func checkForbiddenCalls(ctx context.Context, root, modulePath string, targets []string) ([]forbiddenCallFinding, error) {
	patterns := make([]string, len(targets))
	for i, target := range targets {
		patterns[i] = "./" + target
	}
	listing, err := listExportedPackages(ctx, root, patterns...)
	if err != nil {
		return nil, err
	}
	exports := map[string]string{}
	byImportPath := map[string]*exportedPackage{}
	for i := range listing {
		pkg := &listing[i]
		byImportPath[pkg.ImportPath] = pkg
		if pkg.Export != "" {
			exports[pkg.ImportPath] = pkg.Export
		}
	}

	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data listed for %s", path)
		}
		return os.Open(file) //nolint:gosec // G304: the path comes from the go tool's own -export listing.
	})

	herdrPath := modulePath + "/internal/adapters/herdr"
	appPath := modulePath + "/internal/app"
	var findings []forbiddenCallFinding
	for _, target := range targets {
		importPath := modulePath + "/" + target
		pkg, ok := byImportPath[importPath]
		if !ok {
			return nil, fmt.Errorf("target package %s is not in the go list output; the rule cannot cover it", target)
		}
		targetFindings, err := checkPackageForbiddenCalls(fset, imp, pkg, target, root, herdrPath, appPath)
		if err != nil {
			return nil, err
		}
		findings = append(findings, targetFindings...)
	}
	slices.SortFunc(findings, func(a, b forbiddenCallFinding) int {
		if c := strings.Compare(a.pkg, b.pkg); c != 0 {
			return c
		}
		if c := strings.Compare(a.position, b.position); c != 0 {
			return c
		}
		return strings.Compare(a.call, b.call)
	})
	return findings, nil
}

// checkPackageForbiddenCalls parses and type-checks one target package's
// production files (the go tool's GoFiles — exactly what a build of this
// host compiles) and reports its forbidden method references.
func checkPackageForbiddenCalls(fset *token.FileSet, imp types.Importer, pkg *exportedPackage, target, root, herdrPath, appPath string) ([]forbiddenCallFinding, error) {
	var files []*ast.File
	for _, name := range pkg.GoFiles {
		full := filepath.Join(pkg.Dir, name)
		parsed, err := parser.ParseFile(fset, full, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", full, err)
		}
		files = append(files, parsed)
	}
	conf := types.Config{Importer: imp}
	info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}}
	if _, err := conf.Check(pkg.ImportPath, fset, files, info); err != nil {
		return nil, fmt.Errorf("type-check %s: %w", pkg.ImportPath, err)
	}

	forbiddenNames := forbiddenTransportMethodNames()
	var findings []forbiddenCallFinding
	for sel, selection := range info.Selections {
		if selection.Kind() != types.MethodVal && selection.Kind() != types.MethodExpr {
			continue
		}
		method, ok := selection.Obj().(*types.Func)
		if !ok || method.Pkg() == nil {
			continue
		}
		declPath := method.Pkg().Path()
		var rule string
		switch {
		case forbiddenNames[method.Name()] && (declPath == herdrPath || declPath == appPath):
			rule = "typed-input method reference is forbidden in production code; delivery is pull-only (design section 7)"
		case method.Name() == "Call" && declPath == herdrPath && receiverTypeName(method) == "Client":
			rule = "raw transport calls are confined to the herdr adapter package; production code goes through ports (design section 7)"
		default:
			continue
		}
		position := fset.Position(sel.Sel.Pos())
		rel := position.Filename
		if relative, err := filepath.Rel(root, position.Filename); err == nil {
			rel = filepath.ToSlash(relative)
		}
		findings = append(findings, forbiddenCallFinding{
			pkg:      target,
			position: fmt.Sprintf("%s:%d:%d", rel, position.Line, position.Column),
			call:     fmt.Sprintf("(%s).%s", qualifiedReceiver(method, declPath), method.Name()),
			rule:     rule,
		})
	}
	return findings, nil
}

// receiverTypeName returns the name of the named type a method's receiver
// resolves to, or "" when it has none (an interface method's receiver is
// the interface type itself, which for a declared interface is named too).
func receiverTypeName(method *types.Func) string {
	sig, ok := method.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return ""
	}
	recv := sig.Recv().Type()
	if ptr, ok := recv.(*types.Pointer); ok {
		recv = ptr.Elem()
	}
	if named, ok := recv.(*types.Named); ok {
		return named.Obj().Name()
	}
	return ""
}

// qualifiedReceiver renders a method's declaring type qualified by its
// package path, for a stable finding string.
func qualifiedReceiver(method *types.Func, declPath string) string {
	if name := receiverTypeName(method); name != "" {
		return declPath + "." + name
	}
	return declPath
}

// TestForbiddenTransportCallsRealTree runs the forbidden-call rule over
// the real internal/app and cmd/hop packages.
func TestForbiddenTransportCallsRealTree(t *testing.T) {
	root := moduleRoot(t)
	modulePath, err := goModDirective(root, "module")
	if err != nil {
		t.Fatal(err)
	}
	findings, err := checkForbiddenCalls(t.Context(), root, modulePath, forbiddenCallTargets())
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		t.Errorf("%s", finding)
	}
}

// forbiddenCallFixtureBase is the clean synthetic module: an adapter
// package with the raw Client and typed-input methods, an app package
// declaring ports (SendText declared but never referenced — the staged
// legacy member), and a composition root that uses only allowed surface.
// The adapter's own production Call sites are inside the adapter package
// and therefore allowed.
func forbiddenCallFixtureBase(goVersion string) map[string]string {
	return map[string]string{
		"go.mod": "module example.com/hop\n\ngo " + goVersion + "\n",
		"internal/adapters/herdr/herdr.go": `// Package herdr is the fixture transport adapter.
package herdr

import "context"

// Client is the raw transport.
type Client struct{}

// Call performs one raw request.
func (c *Client) Call(ctx context.Context, method string, params any) error {
	_, _, _ = ctx, method, params
	return nil
}

// Panes is a pane API over the raw client.
type Panes struct{ C *Client }

// SendText types text into a pane.
func (p Panes) SendText(ctx context.Context, pane, text string) error {
	return p.C.Call(ctx, "pane.send_text", pane+text)
}

// SendKeys types key chords into a pane.
func (p Panes) SendKeys(ctx context.Context, pane, keys string) error {
	return p.C.Call(ctx, "pane.send_keys", pane+keys)
}

// Snapshot reads a pane.
func (p Panes) Snapshot(ctx context.Context, pane string) error {
	return p.C.Call(ctx, "pane.snapshot", pane)
}
`,
		"internal/app/app.go": `// Package app is the fixture application layer.
package app

import "context"

// Runtime is the pane port; SendText is the staged legacy member,
// declared but never referenced by production code.
type Runtime interface {
	Open(ctx context.Context) error
	SendText(ctx context.Context, pane, text string) error
}

// Agent is a port carrying an agent-prompt shape.
type Agent interface {
	Prompt(ctx context.Context, text string) error
	Wait(ctx context.Context) error
}

// Controller drives the run through the ports.
type Controller struct {
	Runtime Runtime
	Agent   Agent
}

// Drive performs allowed work only.
func (c *Controller) Drive(ctx context.Context) error {
	if err := c.Runtime.Open(ctx); err != nil {
		return err
	}
	return c.Agent.Wait(ctx)
}
`,
		"cmd/hop/main.go": `// Command hop is the fixture composition root.
package main

import (
	"context"

	"example.com/hop/internal/adapters/herdr"
	"example.com/hop/internal/app"
)

func main() {
	c := &app.Controller{}
	_ = c.Drive(context.Background())
	_ = herdr.Panes{C: &herdr.Client{}}.Snapshot(context.Background(), "p")
}
`,
	}
}

// TestForbiddenTransportCallsFixtures proves the rule catches every
// documented evasion shape and stays quiet on allowed code.
func TestForbiddenTransportCallsFixtures(t *testing.T) {
	root := moduleRoot(t)
	goVersion, err := goModDirective(root, "go")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		files map[string]string
		want  []string // one substring per expected finding, in sorted-finding order; empty means clean
	}{
		{
			name: "clean fixture passes",
		},
		{
			name: "chained selector in app production code",
			files: map[string]string{
				"internal/app/bad_chain.go": `package app

import "context"

func badChain(ctx context.Context, c *Controller) error {
	return c.Runtime.SendText(ctx, "p", "t")
}
`,
			},
			want: []string{"(example.com/hop/internal/app.Runtime).SendText"},
		},
		{
			name: "interface alias in composition",
			files: map[string]string{
				"cmd/hop/bad_alias.go": `package main

import (
	"context"

	"example.com/hop/internal/app"
)

// R aliases the port; the alias resolves to the same declared interface.
type R = app.Runtime

func badAlias(ctx context.Context, r R) error { return r.SendText(ctx, "p", "t") }
`,
			},
			want: []string{"(example.com/hop/internal/app.Runtime).SendText"},
		},
		{
			name: "adapter type alias in composition",
			files: map[string]string{
				"cmd/hop/bad_adapter_alias.go": `package main

import (
	"context"

	"example.com/hop/internal/adapters/herdr"
)

// P aliases the adapter pane API.
type P = herdr.Panes

func badAdapterAlias(ctx context.Context, p P) error { return p.SendText(ctx, "p", "t") }
`,
			},
			want: []string{"(example.com/hop/internal/adapters/herdr.Panes).SendText"},
		},
		{
			name: "method value in composition",
			files: map[string]string{
				"cmd/hop/bad_value.go": `package main

import (
	"context"

	"example.com/hop/internal/app"
)

func badValue(r app.Runtime) func(context.Context, string, string) error { return r.SendText }
`,
			},
			want: []string{"(example.com/hop/internal/app.Runtime).SendText"},
		},
		{
			name: "method expression in composition",
			files: map[string]string{
				"cmd/hop/bad_expr.go": `package main

import "example.com/hop/internal/app"

var badExpr = app.Runtime.SendText
`,
			},
			want: []string{"(example.com/hop/internal/app.Runtime).SendText"},
		},
		{
			name: "raw adapter Client.Call in composition",
			files: map[string]string{
				"cmd/hop/bad_call.go": `package main

import (
	"context"

	"example.com/hop/internal/adapters/herdr"
)

func badCall(ctx context.Context, c *herdr.Client) error {
	return c.Call(ctx, "pane.send_input", nil)
}
`,
			},
			want: []string{"(example.com/hop/internal/adapters/herdr.Client).Call"},
		},
		{
			name: "Prompt on an app port from app production code",
			files: map[string]string{
				"internal/app/bad_prompt.go": `package app

import "context"

func badPrompt(ctx context.Context, c *Controller) error { return c.Agent.Prompt(ctx, "hi") }
`,
			},
			want: []string{"(example.com/hop/internal/app.Agent).Prompt"},
		},
		{
			name: "SendKeys on an adapter type from composition",
			files: map[string]string{
				"cmd/hop/bad_keys.go": `package main

import (
	"context"

	"example.com/hop/internal/adapters/herdr"
)

func badKeys(ctx context.Context, p herdr.Panes) error { return p.SendKeys(ctx, "p", "k") }
`,
			},
			want: []string{"(example.com/hop/internal/adapters/herdr.Panes).SendKeys"},
		},
		{
			name: "a same-named method on an unrelated local type is allowed",
			files: map[string]string{
				"cmd/hop/ok_local.go": `package main

import "context"

type localLogger struct{}

func (localLogger) SendText(ctx context.Context, pane, text string) error {
	_, _, _ = ctx, pane, text
	return nil
}

func okLocal(ctx context.Context) error { return localLogger{}.SendText(ctx, "p", "t") }
`,
			},
		},
		{
			name: "test files are out of the rule's scope",
			files: map[string]string{
				"cmd/hop/probe_test.go": `package main

import (
	"context"
	"testing"

	"example.com/hop/internal/adapters/herdr"
)

func TestProbe(t *testing.T) {
	c := &herdr.Client{}
	if err := c.Call(context.Background(), "pane.send_text", nil); err != nil {
		t.Fatal(err)
	}
}
`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, forbiddenCallFixtureBase(goVersion))
			if len(tc.files) > 0 {
				writeFiles(t, dir, tc.files)
			}
			findings, err := checkForbiddenCalls(t.Context(), dir, "example.com/hop", forbiddenCallTargets())
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) != len(tc.want) {
				t.Fatalf("findings = %v, want %d finding(s) %v", findings, len(tc.want), tc.want)
			}
			for i, want := range tc.want {
				if !strings.Contains(findings[i].String(), want) {
					t.Errorf("finding %d = %s, want it to contain %q", i, findings[i], want)
				}
			}
		})
	}
}
