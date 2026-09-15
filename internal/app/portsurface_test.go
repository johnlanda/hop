package app_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
)

// The port-surface allowlist (docs/plan/phase-3-design.md section 7): the
// exact method set of EVERY interface declared in internal/app, reviewed
// as a whole. A typed-input method appearing on ANY port — not just
// Runtime — fails this test, and so does any surface drift the reviewer
// has not seen: adding, renaming or removing a method anywhere in the
// package requires updating this table in the same change.

// portSurfaceAllowlist is the reviewed method set of every internal/app
// interface, method names sorted. An embedded interface would appear as
// "embed:<name>" so it cannot smuggle methods in unreviewed.
func portSurfaceAllowlist() map[string][]string {
	return map[string][]string{
		"AgentPresentation":        {"ClearView", "ReportMetadata", "SelectView"},
		"ArtifactRepository":       {"Save"},
		"ArtifactStore":            {"ReadArtifact", "WriteArtifact"},
		"AttemptIndexRepository":   {"ByTask", "Create"},
		"AttemptRepository":        {"Get", "Save"},
		"BindingRepository":        {"Create", "Current", "Save"},
		"CheckExecClaimRepository": {"Get"},
		"CheckRequestRepository":   {"Get", "Pending", "Save"},
		"Clock":                    {"Now"},
		"CommandRunner":            {"Run"},
		"ConfigurationSource":      {"Load"},
		"IDGenerator":              {"NewID"},
		"IntegrationRepository":    {"ByTask", "Create", "Current", "Get", "Save"},
		"LaunchClaimRepository":    {"Get", "Pending", "Settle"},
		"MessageRepository":        {"ByAddress", "Create", "Get"},
		"MessagingStore":           {"AckMessage", "AnswerQuestion", "FetchNextMessage", "SendMessage"},
		"Observer":                 {"Snapshot", "Subscribe"},
		"OperationRepository":      {"ByKind", "Create", "Get", "Pending", "Save"},
		"PlanStore":                {"ClosePlan", "CreateTask", "RequestRetry"},
		"Probe":                    {"Binary", "Harness", "Ping", "Schema"},
		"ProcessGroupInspector":    {"GroupProcesses", "SignalGroup"},
		"ReadStore":                {"ListRuns", "LoadCheckExecutionContext", "LoadFrozenRun", "LoadLaunchContext", "LoadRunStatus"},
		"ResultRepository":         {"Accepted"},
		"RetryRequestRepository":   {"MarkConsumed", "Pending"},
		"ReviewRepository":         {"ByAttempt", "Latest"},
		"ReviewStore":              {"SubmitReview"},
		"RunRepository":            {"Get", "Save"},
		// Runtime still carries SendText: the staged legacy member of the
		// section 7 injection invariant, declared but never referenced by
		// production code (the internal checker's forbidden-call rule
		// enforces the no-reference half). Legacy exception recorded
		// 2026-09-15 by slice 2b; slice 6's removal landing deletes the
		// port member together with this entry's "SendText", at which
		// point this test asserts its absence through the typed-input scan
		// below losing its one exception.
		"Runtime":                  {"ClosePane", "CreateWorktree", "FindPaneByLabel", "InspectPane", "OpenWorkerPane", "ReadPane", "SendText", "ServerInstance"},
		"SessionIndexRepository":   {"ByRun"},
		"SessionRepository":        {"Create", "Current", "Get", "Save"},
		"StateStore":               {"AcquireLease", "Begin", "Heartbeat", "InitializeRun", "ReleaseLease"},
		"StatusStream":             {"Close", "DrainRemaining", "Err", "Events"},
		"SubmissionStore":          {"ClaimCheckExec", "ClaimLaunch", "RecordMalformed", "RequestStop", "SettleLaunchFailure", "SubmitResult"},
		"TaskDependencyRepository": {"ByRun"},
		"TaskIndexRepository":      {"ByRun", "Create"},
		"TaskRepository":           {"Get", "Save"},
		"TransitionRepository":     {"Record"},
		"TrustSeeder":              {"SeedWorkspaceTrust"},
		"UnitOfWork":               {"Artifacts", "Attempts", "Bindings", "CheckExecClaims", "CheckRequests", "Commit", "LaunchClaims", "Operations", "Results", "Rollback", "Runs", "Sessions", "Tasks", "Transitions", "Worktrees"},
		"WorkflowReadStore":        {"LoadMessageDetail", "LoadMessagingContext", "LoadSessionLaunchContext"},
		"WorkflowRepositories":     {"AttemptIndex", "Integrations", "ManagerSession", "Messages", "RetryRequests", "Reviews", "SessionIndex", "TaskDependencies", "TaskIndex"},
		"WorkspaceRuntime":         {"CreateWorkspace", "FindWorkspaceByLabel"},
		"WorktreeRepository":       {"ByRun", "Create", "Get", "Save"},
	}
}

// typedInputMethodNames are the method names that would carry terminal
// input if they ever appeared on a port: the section 7 injection
// invariant's name set, matching the internal checker's forbidden-call
// rule plus Herdr's send_input shape.
func typedInputMethodNames() map[string]bool {
	return map[string]bool{"SendText": true, "SendKeys": true, "SendInput": true, "Prompt": true}
}

// legacyTypedInputExceptions are the staged legacy members the typed-input
// scan tolerates, as "Interface.Method". Exactly one exists: Runtime.
// SendText, recorded 2026-09-15 (slice 2b); slice 6's removal landing
// deletes the port member and this entry together, and the scan then
// proves the whole package free of typed-input method names.
func legacyTypedInputExceptions() map[string]bool {
	return map[string]bool{"Runtime.SendText": true}
}

// declaredPortSurface parses every production (non-test) file of the
// package directory and returns each declared interface's method-name
// set, embedded interfaces rendered as "embed:<type>".
func declaredPortSurface(t *testing.T) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	fset := token.NewFileSet()
	surface := map[string][]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.TYPE {
				continue
			}
			for _, spec := range genDecl.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				iface, ok := typeSpec.Type.(*ast.InterfaceType)
				if !ok {
					continue
				}
				var methods []string
				for _, field := range iface.Methods.List {
					if len(field.Names) == 0 {
						methods = append(methods, "embed:"+renderEmbeddedType(field.Type))
						continue
					}
					for _, methodName := range field.Names {
						methods = append(methods, methodName.Name)
					}
				}
				slices.Sort(methods)
				surface[typeSpec.Name.Name] = methods
			}
		}
	}
	if len(surface) == 0 {
		t.Fatal("no interface declarations found; the surface scan is broken, not the package empty")
	}
	return surface
}

// renderEmbeddedType renders an embedded interface expression for the
// surface table.
func renderEmbeddedType(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		if pkg, ok := e.X.(*ast.Ident); ok {
			return pkg.Name + "." + e.Sel.Name
		}
	}
	return "unrenderable"
}

// TestPortSurfaceAllowlist asserts the exact method set of every
// internal/app interface against the reviewed allowlist, and that no
// typed-input method name appears anywhere beyond the one dated legacy
// exception.
func TestPortSurfaceAllowlist(t *testing.T) {
	declared := declaredPortSurface(t)
	allowed := portSurfaceAllowlist()

	for name, methods := range declared {
		want, ok := allowed[name]
		if !ok {
			t.Errorf("interface %s (methods %v) is not in the reviewed port-surface allowlist; add it in the same change that declares it", name, methods)
			continue
		}
		if !slices.Equal(methods, want) {
			t.Errorf("interface %s method set = %v, allowlist has %v; surface drift needs a reviewed allowlist update", name, methods, want)
		}
	}
	for name := range allowed {
		if _, ok := declared[name]; !ok {
			t.Errorf("allowlist names interface %s, which is no longer declared; delete its entry in the same change", name)
		}
	}

	typedInput := typedInputMethodNames()
	exceptions := legacyTypedInputExceptions()
	seenExceptions := map[string]bool{}
	for name, methods := range declared {
		for _, method := range methods {
			if !typedInput[method] {
				continue
			}
			key := name + "." + method
			if exceptions[key] {
				seenExceptions[key] = true
				continue
			}
			t.Errorf("typed-input method %s violates the section 7 injection invariant: no port may carry terminal input", key)
		}
	}
	for key := range exceptions {
		if !seenExceptions[key] {
			t.Errorf("legacy exception %s is recorded but no longer declared; delete the exception entry in the same change (slice 6's removal landing)", key)
		}
	}
}
