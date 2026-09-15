package integration

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/herdr"
)

// TestSpikeAPIRequestLogObservability is a PROVISIONAL probe (numbering
// pending the design planner), requested by the Phase 3 design reviewer,
// establishing exactly what Herdr's own server request log makes observable
// -- the precondition a future injection-free real-process scenario (design
// section 11, TestRealProcessInjectionFreeDelivery) needs before it can cite
// "the server's own log shows no pane.send_text/agent.prompt request" as
// evidence.
//
// Confirmed against repos/herdr source and this test's own executed
// evidence:
//   - Location: the same directory the server's own socket lives in
//     (filepath.Dir(server.socketPath)), file "herdr-server.log"
//     (repos/herdr/src/server/headless/bootstrap.rs init_logging,
//     repos/herdr/src/session.rs data_dir/config_dir).
//   - Level: pane.send_text (changes_ui=false,
//     repos/herdr/src/api/mod.rs request_changes_ui) logs its start/complete
//     pair at DEBUG only when it SUCCEEDS (repos/herdr/src/logging.rs
//     api_request_started/api_request_completed); agent.prompt
//     (changes_ui=true, not in logging.rs's routine-method allowlist) logs
//     its start at INFO regardless of outcome. The default filter (HERDR_LOG
//     unset) is "herdr=info", so a SUCCESSFUL pane.send_text is invisible at
//     default settings while agent.prompt is always visible; a FAILED
//     pane.send_text would still surface via api_request_failed, which logs
//     at WARN unconditionally -- this probe's send_text calls all succeed,
//     so that path is not what is being pinned here.
//   - Correlation: every request-log line carries method and request_id, but
//     NEVER the pane/agent target -- an injection-free scenario can only
//     honestly claim "no pane.send_text/agent.prompt request was logged
//     during the run," never a per-pane claim. request_id is a CLIENT-chosen
//     label the server only echoes back into its own log, never a
//     server-assigned global sequence: two INDEPENDENT client connections to
//     the same server (this test's own server.client, driving readiness
//     polling and workspace.create, and a dedicated probe client) both start
//     counting from "hop-1", so the SAME id value appears in one log against
//     two UNRELATED methods (confirmed below). A log-based correlation must
//     therefore locate its own call's line by something unique to it (here,
//     method name, since nothing else in this scenario calls pane.send_text
//     or agent.prompt) and read the id back from there, never treat a
//     request_id value as a global primary key across every connection
//     touching the server.
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

	// A dedicated client for the positive control, kept separate from
	// server.client so its calls are easy to isolate by method name.
	// request_id is NOT used to locate these lines (see the file comment's
	// finding) -- it is read back FROM the located line instead.
	probe := herdr.NewClient(server.socketPath)

	// Capture-window bracketing: request-log lines never carry their own
	// request's params (see the file comment) -- a chosen "marker" value
	// like a label is NEVER visible in the log, only method and request_id
	// are. The bracket must therefore be two otherwise-unused METHODS: one
	// issued immediately before anything under test (pane.list, never
	// called elsewhere in this scenario), one immediately after
	// (tab.list). A future injection-free scenario must scope its "no
	// send_text/prompt logged" search to BETWEEN its own start and
	// completion requests' own log lines this way, never the whole
	// (potentially multi-run) log file.
	server.call(t, "pane.list", nil, &struct{}{})

	sendMarker := "hop-spike-log-probe-" + newSpikeUUID(t)
	if err := probe.Call(testContext(t), "pane.send_text", map[string]any{
		"pane_id": pane, "text": sendMarker + "\n",
	}, nil); err != nil {
		t.Fatalf("pane.send_text: %v", err)
	}
	promptErr := probe.Call(testContext(t), "agent.prompt", map[string]any{
		"target": pane, "text": "hop-spike-log-probe-prompt",
	}, nil)
	t.Logf("agent.prompt against pane %s (no live/matching agent) returned: %v; only its log visibility is under test here", pane, promptErr)

	// Cheap coverage of every other pane/agent input-adjacent method
	// (design section 7's whole "typed input" surface): none needs to
	// succeed, only to reach dispatch so its own request-log line exists.
	// agent.start uses an unrecognized kind (S5's technique) so it is
	// refused before touching any terminal, never actually starting a
	// process.
	otherInputMethods := []struct {
		method string
		params map[string]any
	}{
		{"pane.send_keys", map[string]any{"pane_id": pane, "keys": []string{"Enter"}}},
		{"pane.send_input", map[string]any{"pane_id": pane, "text": "hop-spike-log-probe-input"}},
		{"agent.send_keys", map[string]any{"target": pane, "keys": []string{"Enter"}}},
		{"agent.start", map[string]any{"name": "hop-spike-log-probe", "kind": "hop-spike-unrecognized-kind", "pane_id": pane}},
	}
	for _, call := range otherInputMethods {
		err := probe.Call(testContext(t), call.method, call.params, nil)
		t.Logf("%s (probe coverage) returned: %v; only its log visibility is under test here", call.method, err)
	}

	server.call(t, "tab.list", nil, &struct{}{})

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
	for _, call := range append([]struct {
		method string
		params map[string]any
	}{{"pane.send_text", nil}, {"agent.prompt", nil}}, otherInputMethods...) {
		idx, found := firstLineIndex(lines, `method="`+call.method+`"`)
		if !found {
			t.Errorf("herdr-server.log has no line for %s", call.method)
			continue
		}
		if idx <= startIdx || idx >= endIdx {
			t.Errorf("%s logged at line %d, want it strictly between the window-start (%d) and window-end (%d) markers", call.method, idx, startIdx, endIdx)
		}
	}

	sendTextLine, ok := lineContaining(content, `method="pane.send_text"`)
	if !ok {
		t.Fatalf("herdr-server.log has no line for pane.send_text under HERDR_LOG=herdr=debug; content:\n%s", content)
	}
	sendTextID, ok := extractRequestID(sendTextLine)
	if !ok {
		t.Fatalf("pane.send_text log line carries no request_id field: %s", sendTextLine)
	}
	if strings.Contains(sendTextLine, sendMarker) {
		t.Errorf("the logged pane.send_text line contains the sent TEXT itself (%q); expected only method/request_id metadata, never payload content: %s", sendMarker, sendTextLine)
	}
	if strings.Contains(sendTextLine, pane) {
		t.Errorf("the logged pane.send_text line names the target pane %q; expected the request log to carry no pane target, only method and request_id: %s", pane, sendTextLine)
	}

	promptLine, ok := lineContaining(content, `method="agent.prompt"`)
	if !ok {
		t.Fatalf("herdr-server.log has no line for agent.prompt; content:\n%s", content)
	}
	promptID, ok := extractRequestID(promptLine)
	if !ok {
		t.Fatalf("agent.prompt log line carries no request_id field: %s", promptLine)
	}
	t.Logf("correlated by method: pane.send_text -> request_id=%s; agent.prompt -> request_id=%s", sendTextID, promptID)
	if sendTextID == promptID {
		t.Errorf("pane.send_text and agent.prompt, two distinct calls from the SAME probe client, logged the SAME request_id %s; expected each call to get its own", sendTextID)
	}

	// The id-collision FINDING itself, made concrete: server.client (this
	// test's own driver, used for readiness polling and workspace.create
	// above) is a SEPARATE connection from probe, and both count from 1 --
	// so "hop-1" is very likely also logged here against an unrelated
	// method. This is why the lookups above locate lines by method, never by
	// a pre-guessed request_id value.
	if line, ok := lineContaining(content, `request_id="hop-1"`); ok {
		t.Logf("request_id is per-client, not global -- a second, unrelated connection's own request_id=\"hop-1\": %s", line)
	}

	// Absence, made meaningful: a SEPARATE server at Herdr's DEFAULT log
	// level (HERDR_LOG unset) proves the two methods diverge exactly as the
	// elevated run above implies -- agent.prompt is visible even without
	// elevation, a SUCCESSFUL pane.send_text is not -- using the SAME
	// detection mechanism that just found real records above, anchored to a
	// causally later, always-visible checkpoint request (never an instant
	// read or a fixed sleep) so the absence claim is not merely "nothing had
	// raced into the file yet."
	defaultArtifacts := newArtifactDir(t)
	defaultServer := prepareServer(t, defaultArtifacts)
	defaultServer.start(t)
	var defaultWorkspace workspaceCreatedResponse
	defaultServer.call(t, "workspace.create", map[string]any{"cwd": defaultServer.workDir(), "focus": true}, &defaultWorkspace)
	defaultPane := defaultWorkspace.RootPane.PaneID
	defaultServer.call(t, "pane.send_text", map[string]any{
		"pane_id": defaultPane, "text": "default-level-send-text-probe\n",
	}, nil)
	defaultPromptErr := defaultServer.client.Call(testContext(t), "agent.prompt", map[string]any{
		"target": defaultPane, "text": "default-level-agent-prompt-probe",
	}, nil)
	t.Logf("agent.prompt at the default log level returned: %v; only its log visibility is under test here", defaultPromptErr)
	var checkpoint workspaceCreatedResponse
	defaultServer.call(t, "workspace.create", map[string]any{
		"cwd": defaultServer.workDir(), "focus": false, "label": "hop-spike-log-checkpoint",
	}, &checkpoint)

	defaultLogPath := filepath.Join(filepath.Dir(defaultServer.socketPath), "herdr-server.log")
	var defaultContent string
	if !waitUntil(func() bool {
		data, err := os.ReadFile(defaultLogPath) //nolint:gosec // G304: fixed path derived from this test's own isolated socket directory.
		if err != nil {
			return false
		}
		defaultContent = string(data)
		return strings.Contains(defaultContent, checkpoint.Workspace.WorkspaceID)
	}) {
		t.Fatalf("herdr-server.log at %s never recorded the flush checkpoint workspace %s", defaultLogPath, checkpoint.Workspace.WorkspaceID)
	}
	defaultArtifacts.save(t, "herdr-server-log-default-level.txt", defaultContent)

	if strings.Contains(defaultContent, `method="pane.send_text"`) {
		t.Errorf("herdr-server.log at the DEFAULT level unexpectedly recorded pane.send_text; the assumption that a log-based injection-free proof needs an elevated level does not hold:\n%s", defaultContent)
	}
	if !strings.Contains(defaultContent, `method="agent.prompt"`) {
		t.Errorf("herdr-server.log at the DEFAULT level does not record agent.prompt; expected it visible without elevation:\n%s", defaultContent)
	}
}

// lineContaining returns the first line of content containing substr, and
// whether one was found.
func lineContaining(content, substr string) (string, bool) {
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, substr) {
			return line, true
		}
	}
	return "", false
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

// requestIDPattern matches one log line's request_id="..." field.
var requestIDPattern = regexp.MustCompile(`request_id="([^"]*)"`)

// extractRequestID reads the request_id field's value out of one log line.
func extractRequestID(line string) (string, bool) {
	match := requestIDPattern.FindStringSubmatch(line)
	if match == nil {
		return "", false
	}
	return match[1], true
}
