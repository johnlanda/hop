package integration

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/herdr"
)

// TestSpikeAPIRequestLogObservability establishes exactly what Herdr's own
// server request log makes observable -- the precondition a future
// injection-free real-process scenario (design section 11,
// TestRealProcessInjectionFreeDelivery) needs before it can cite "the
// server's own log shows no pane.send_text/agent.prompt request" as
// evidence.
//
// Confirmed against repos/herdr source and this test's own executed
// evidence:
//   - Location: the same directory the server's own socket lives in
//     (filepath.Dir(server.socketPath)), file "herdr-server.log"
//     (repos/herdr/src/server/headless/bootstrap.rs init_logging,
//     repos/herdr/src/session.rs data_dir/config_dir).
//   - Level: pane.send_text/pane.send_keys/pane.send_input
//     (changes_ui=false, repos/herdr/src/api/mod.rs request_changes_ui)
//     log their start/complete pair at DEBUG only when they succeed
//     (repos/herdr/src/logging.rs api_request_started/api_request_completed);
//     a FAILED or erroring call among these still completes via the same
//     event="api.request.complete" record, never a distinct fail event, but
//     at INFO, since api_request_completed forces INFO whenever
//     outcome != "ok". agent.prompt/agent.send_keys/agent.start
//     (changes_ui=true, not in logging.rs's routine-method allowlist) log
//     both their start and completion records at INFO regardless of
//     outcome. event="api.request.fail" (WARN, logging.rs
//     api_request_failed) is a separate event reserved for a failure
//     WRITING the response back to the client socket itself (e.g. a
//     disconnect mid-write) -- it never fires for an ordinary
//     business-logic refusal, and no case in this probe induces it. The
//     default filter (HERDR_LOG unset) is "herdr=info", so a SUCCESSFUL
//     pane.* call is invisible at default settings while every agent.*
//     call, and any FAILED or erroring call of either kind, is visible.
//   - Correlation: every request-log line carries method and request_id, but
//     NEVER the pane/agent target or any request payload -- an
//     injection-free scenario can only honestly claim "no pane.send_text/
//     agent.prompt request was logged during the run," never a per-pane
//     claim. request_id is a CLIENT-chosen label the server only echoes
//     back into its own log, never a server-assigned global sequence: two
//     INDEPENDENT client connections to the same server both start
//     counting from "hop-1", so the SAME id value can appear against two
//     UNRELATED methods in one log -- a log-based correlation must locate
//     its own call's line by something unique to it (method name, when
//     nothing else in the scenario calls that method) and read the id back
//     from there, never treat a request_id value as a global primary key.
//   - Format: plain text (tracing_subscriber's default formatter, no JSON,
//     no ANSI), one line per event with a message followed by
//     space-separated key="value"/key=value fields; captured verbatim into
//     this test's own artifacts on every run for inspection.
func TestSpikeAPIRequestLogObservability(t *testing.T) {
	artifacts := newArtifactDir(t)
	server := prepareServer(t, artifacts)
	server.extraEnv = append(server.extraEnv, "HERDR_LOG=herdr=debug")
	server.start(t)

	var workspace workspaceCreatedResponse
	server.call(t, "workspace.create", map[string]any{"cwd": server.workDir(), "focus": true}, &workspace)
	pane := workspace.RootPane.PaneID

	server.call(t, "pane.list", nil, &struct{}{}) // window-start bracket

	// Two single-call clients, each its own connection with its own
	// request-id counter starting at 1: this deterministically reproduces
	// the collision documented above -- both log request_id="hop-1"
	// against two DIFFERENT methods, distinguishable only by method, never
	// by id value.
	collisionClient := herdr.NewClient(server.socketPath)
	if err := collisionClient.Call(testContext(t), "session.snapshot", nil, nil); err != nil {
		t.Fatalf("session.snapshot (collision control): %v", err)
	}

	sendMarker := "hop-spike-log-probe-" + newSpikeUUID(t)
	inputMarker := "hop-spike-log-probe-input-" + newSpikeUUID(t)
	cases := []apiLogCase{
		{
			method: "pane.send_text", params: map[string]any{"pane_id": pane, "text": sendMarker + "\n"},
			succeeds: true, startLevel: "DEBUG", secondLevel: "DEBUG",
			forbidden: []string{pane, sendMarker},
		},
		{
			method: "pane.send_keys", params: map[string]any{"pane_id": pane, "keys": []string{"Enter"}},
			succeeds: true, startLevel: "DEBUG", secondLevel: "DEBUG",
			forbidden: []string{pane},
		},
		{
			method: "pane.send_input", params: map[string]any{"pane_id": pane, "text": inputMarker},
			succeeds: true, startLevel: "DEBUG", secondLevel: "DEBUG",
			forbidden: []string{pane, inputMarker},
		},
		{
			method: "agent.prompt", params: map[string]any{"target": pane, "text": "hop-spike-log-probe-prompt"},
			succeeds: false, startLevel: "INFO", secondLevel: "INFO",
			forbidden: []string{pane},
		},
		{
			method: "agent.send_keys", params: map[string]any{"target": pane, "keys": []string{"Enter"}},
			succeeds: false, startLevel: "INFO", secondLevel: "INFO",
			forbidden: []string{pane},
		},
		{
			method: "agent.start", params: map[string]any{"name": "hop-spike-log-probe", "kind": "hop-spike-unrecognized-kind", "pane_id": pane},
			succeeds: false, startLevel: "INFO", secondLevel: "INFO",
			forbidden: []string{pane, "hop-spike-log-probe"},
		},
	}
	for i := range cases {
		runAPILogCase(t, server, &cases[i])
	}

	server.call(t, "tab.list", nil, &struct{}{}) // window-end bracket

	logPath := filepath.Join(filepath.Dir(server.socketPath), "herdr-server.log")
	var content string
	if !waitUntil(func() bool {
		data, err := os.ReadFile(logPath) //nolint:gosec // G304: fixed path derived from this test's own isolated socket directory.
		if err != nil {
			return false
		}
		content = string(data)
		return strings.Contains(content, `method="tab.list"`)
	}) {
		t.Fatalf("herdr-server.log at %s never recorded the window-end marker (tab.list); last content:\n%s", logPath, content)
	}
	artifacts.save(t, "herdr-server-log-format.txt", content)

	lines := strings.Split(content, "\n")
	startIdx, ok := firstLineIndex(lines, `method="pane.list"`)
	if !ok {
		t.Fatalf("herdr-server.log never recorded the window-start marker (pane.list)")
	}
	endIdx, ok := firstLineIndex(lines, `method="tab.list"`)
	if !ok {
		t.Fatalf("herdr-server.log never recorded the window-end marker (tab.list)")
	}
	if endIdx <= startIdx {
		t.Fatalf("window-end line %d is not after window-start line %d; the bracket is not usable for scoping a scan", endIdx, startIdx)
	}

	snapshotLine := requireLine(t, lines, startIdx, endIdx, `request_id="hop-1"`, `method="session.snapshot"`, `event="api.request.start"`)
	sendTextStartLine := requireLine(t, lines, startIdx, endIdx, `request_id="hop-1"`, `method="pane.send_text"`, `event="api.request.start"`)
	if snapshotLine == sendTextStartLine {
		t.Fatal("the collision control and pane.send_text resolved to the same log line")
	}
	t.Logf("collision confirmed, distinguishable only by method:\n%s\n%s", snapshotLine, sendTextStartLine)

	for i := range cases {
		assertAPILogCase(t, lines, startIdx, endIdx, &cases[i])
	}

	// Absence and presence, made meaningful: a SEPARATE server at Herdr's
	// DEFAULT log level (HERDR_LOG unset) exercises the SAME six methods
	// through the SAME assertion mechanism, confirming the level split is
	// real and not an artifact of the elevated capture above -- a
	// SUCCESSFUL pane.* call produces no log line for its method at all,
	// while every agent.* call remains fully visible.
	// pane.list/tab.list are themselves DEBUG-only (changes_ui=false), so
	// they cannot bracket a capture at the default level; workspace.create
	// (already issued here for setup) and workspace.focus (issued below)
	// are changes_ui=true and non-routine, so both stay visible at INFO
	// regardless of level.
	defaultArtifacts := newArtifactDir(t)
	defaultServer := prepareServer(t, defaultArtifacts)
	defaultServer.start(t)
	var defaultWorkspace workspaceCreatedResponse
	defaultServer.call(t, "workspace.create", map[string]any{"cwd": defaultServer.workDir(), "focus": true}, &defaultWorkspace)
	defaultPane := defaultWorkspace.RootPane.PaneID

	defaultCases := []apiLogCase{
		{method: "pane.send_text", params: map[string]any{"pane_id": defaultPane, "text": "default-level-send-text\n"}, succeeds: true},
		{method: "pane.send_keys", params: map[string]any{"pane_id": defaultPane, "keys": []string{"Enter"}}, succeeds: true},
		{method: "pane.send_input", params: map[string]any{"pane_id": defaultPane, "text": "default-level-send-input"}, succeeds: true},
		{method: "agent.prompt", params: map[string]any{"target": defaultPane, "text": "default-level-prompt"}, succeeds: false, startLevel: "INFO", secondLevel: "INFO"},
		{method: "agent.send_keys", params: map[string]any{"target": defaultPane, "keys": []string{"Enter"}}, succeeds: false, startLevel: "INFO", secondLevel: "INFO"},
		{method: "agent.start", params: map[string]any{"name": "hop-spike-log-probe", "kind": "hop-spike-unrecognized-kind", "pane_id": defaultPane}, succeeds: false, startLevel: "INFO", secondLevel: "INFO"},
	}
	for i := range defaultCases {
		runAPILogCase(t, defaultServer, &defaultCases[i])
	}
	defaultServer.call(t, "workspace.focus", map[string]any{"workspace_id": defaultWorkspace.Workspace.WorkspaceID}, &struct{}{})

	defaultLogPath := filepath.Join(filepath.Dir(defaultServer.socketPath), "herdr-server.log")
	var defaultContent string
	if !waitUntil(func() bool {
		data, err := os.ReadFile(defaultLogPath) //nolint:gosec // G304: fixed path derived from this test's own isolated socket directory.
		if err != nil {
			return false
		}
		defaultContent = string(data)
		return strings.Contains(defaultContent, `method="workspace.focus"`)
	}) {
		t.Fatalf("herdr-server.log at %s never recorded the window-end marker (workspace.focus); last content:\n%s", defaultLogPath, defaultContent)
	}
	defaultArtifacts.save(t, "herdr-server-log-default-level.txt", defaultContent)

	defaultLines := strings.Split(defaultContent, "\n")
	defaultStartIdx, ok := firstLineIndex(defaultLines, `method="workspace.create"`)
	if !ok {
		t.Fatalf("default-level herdr-server.log never recorded the window-start marker (workspace.create)")
	}
	defaultEndIdx, ok := firstLineIndex(defaultLines, `method="workspace.focus"`)
	if !ok {
		t.Fatalf("default-level herdr-server.log never recorded the window-end marker (workspace.focus)")
	}
	for i := range defaultCases {
		c := &defaultCases[i]
		if c.succeeds {
			for j := defaultStartIdx + 1; j < defaultEndIdx; j++ {
				if strings.Contains(defaultLines[j], `method="`+c.method+`"`) {
					t.Errorf("default-level log unexpectedly records a successful %s: %s", c.method, defaultLines[j])
				}
			}
			continue
		}
		assertAPILogCase(t, defaultLines, defaultStartIdx, defaultEndIdx, c)
	}
}

// apiLogCase is one method's request-log expectation: the call to issue and
// (once succeeds is known) the exact start/second-record levels and any
// strings that must never appear in either of that method's own log lines.
type apiLogCase struct {
	method      string
	params      map[string]any
	succeeds    bool
	startLevel  string
	secondLevel string
	forbidden   []string
}

// runAPILogCase issues one case's call through a fresh single-call client
// (so its request_id is deterministically "hop-1" from that client's own
// counter) and fails the test if the call's outcome does not match what the
// case declares.
func runAPILogCase(t *testing.T, server *testServer, c *apiLogCase) {
	t.Helper()
	client := herdr.NewClient(server.socketPath)
	err := client.Call(testContext(t), c.method, c.params, nil)
	if c.succeeds && err != nil {
		t.Fatalf("%s: %v", c.method, err)
	}
	if !c.succeeds && err == nil {
		t.Fatalf("%s: unexpectedly succeeded, want a business-layer refusal", c.method)
	}
}

// assertAPILogCase asserts one case's exact start record and second record
// within lines[startIdx+1:endIdx], their levels, and the absence of every
// forbidden string from both lines. The second record always carries
// event="api.request.complete", whether the call succeeded (outcome="ok")
// or was refused at the business layer (outcome="error") -- a distinct
// event="api.request.fail" (WARN) record exists only for a failure writing
// the response back to the client socket itself, which no case here induces.
func assertAPILogCase(t *testing.T, lines []string, startIdx, endIdx int, c *apiLogCase) {
	t.Helper()
	startLine := requireLine(t, lines, startIdx, endIdx, `event="api.request.start"`, `method="`+c.method+`"`, `request_id="hop-1"`)
	if got := logLevel(startLine); got != c.startLevel {
		t.Errorf("%s start record level = %s, want %s: %s", c.method, got, c.startLevel, startLine)
	}
	wantOutcome := "ok"
	if !c.succeeds {
		wantOutcome = "error"
	}
	secondLine := requireLine(t, lines, startIdx, endIdx, `event="api.request.complete"`, `method="`+c.method+`"`, `request_id="hop-1"`)
	if got := logLevel(secondLine); got != c.secondLevel {
		t.Errorf("%s second record level = %s, want %s: %s", c.method, got, c.secondLevel, secondLine)
	}
	if !strings.Contains(secondLine, `outcome="`+wantOutcome+`"`) {
		t.Errorf("%s second record does not carry outcome=%q: %s", c.method, wantOutcome, secondLine)
	}
	for _, forbidden := range c.forbidden {
		if strings.Contains(startLine, forbidden) {
			t.Errorf("%s start record contains the forbidden string %q: %s", c.method, forbidden, startLine)
		}
		if strings.Contains(secondLine, forbidden) {
			t.Errorf("%s second record contains the forbidden string %q: %s", c.method, forbidden, secondLine)
		}
	}
}

// firstLineIndex returns the index of the first line in lines containing
// substr, and whether one was found.
func firstLineIndex(lines []string, substr string) (int, bool) {
	for i, line := range lines {
		if strings.Contains(line, substr) {
			return i, true
		}
	}
	return -1, false
}

// countLinesContainingAll returns how many lines in lines contain EVERY
// one of substrs. One API call logs TWO lines (its own start and
// complete/fail record), each sharing the same method="..." text, so
// asserting a method was called exactly once needs event="..." folded
// into the count too -- counting raw lines by method alone would always
// be even and never equal 1 for a method genuinely called once.
func countLinesContainingAll(lines []string, substrs ...string) int {
	n := 0
	for _, line := range lines {
		all := true
		for _, s := range substrs {
			if !strings.Contains(line, s) {
				all = false
				break
			}
		}
		if all {
			n++
		}
	}
	return n
}

// requireLine returns the line within lines[startIdx+1:endIdx] containing
// every one of substrs, failing the test unless exactly one such line
// exists.
func requireLine(t *testing.T, lines []string, startIdx, endIdx int, substrs ...string) string {
	t.Helper()
	return lines[requireLineIndex(t, lines, startIdx, endIdx, substrs...)]
}

// requireLineIndex returns the index within lines[startIdx+1:endIdx] of the
// line containing every one of substrs, failing the test unless exactly one
// such line exists. Callers that must exclude a located line from a later
// scan (by index, never by string equality, since two distinct lines can be
// byte-identical) use this instead of requireLine.
func requireLineIndex(t *testing.T, lines []string, startIdx, endIdx int, substrs ...string) int {
	t.Helper()
	foundIdx := -1
	matches := 0
	for i := startIdx + 1; i < endIdx; i++ {
		line := lines[i]
		all := true
		for _, s := range substrs {
			if !strings.Contains(line, s) {
				all = false
				break
			}
		}
		if all {
			foundIdx = i
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("expected exactly one log line containing %v within the capture window, found %d", substrs, matches)
	}
	return foundIdx
}

// logLevelPattern matches a request-log line's level token: a timestamp,
// then the level, then the tracing target and a colon.
var logLevelPattern = regexp.MustCompile(`^\S+\s+(\S+)\s+\S+:`)

// logLevel extracts one log line's level (DEBUG, INFO, WARN, ...).
func logLevel(line string) string {
	match := logLevelPattern.FindStringSubmatch(line)
	if match == nil {
		return ""
	}
	return match[1]
}
