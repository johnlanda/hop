package herdr_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"regexp"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/herdr"
	"github.com/johnlanda/hop/internal/app"
)

// startFakeRuntime runs a fake endpoint that answers every request with a
// fixed result and records each request. Like the presentation adapter,
// Runtime opens its own connection per call, so the handler answers one
// request per connection.
func startFakeRuntime(t *testing.T, result string) (runtime *herdr.Runtime, requests <-chan map[string]any) {
	t.Helper()
	got := make(chan map[string]any, 4)
	endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
		request := readRequestLine(t, bufio.NewReader(conn))
		if request == nil {
			return
		}
		got <- request
		writeLine(t, conn, fmt.Sprintf(`{"id":%q,"result":%s}`, requestID(t, request), result))
	})
	return herdr.NewRuntime(endpoint.socketPath), got
}

// startFakeRuntimeError runs a fake endpoint that answers every request
// with the given error code and message.
func startFakeRuntimeError(t *testing.T, code, message string) *herdr.Runtime {
	t.Helper()
	endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
		request := readRequestLine(t, bufio.NewReader(conn))
		if request == nil {
			return
		}
		writeLine(t, conn, fmt.Sprintf(`{"id":%q,"error":{"code":%q,"message":%q}}`, requestID(t, request), code, message))
	})
	return herdr.NewRuntime(endpoint.socketPath)
}

// assertRequestParams asserts a fake endpoint's decoded request has the
// given method and params structurally equal to wantParamsJSON: the exact
// set of keys (no extras, none missing), equal values and JSON types at
// every level, and array elements in the given order. Both sides are
// compared as decoded JSON (map[string]any / []any / scalars), so object
// key order — which carries no meaning in JSON — is not asserted, while a
// renamed, dropped, added or reordered array field fails.
func assertRequestParams(t *testing.T, request map[string]any, method, wantParamsJSON string) {
	t.Helper()
	if request["method"] != method {
		t.Fatalf("method = %v, want %s", request["method"], method)
	}
	var want any
	if err := json.Unmarshal([]byte(wantParamsJSON), &want); err != nil {
		t.Fatalf("invalid expected params fixture: %v", err)
	}
	got := request["params"]
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s params =\n%s\nwant\n%s", method, prettyJSON(t, got), prettyJSON(t, want))
	}
}

// prettyJSON renders a decoded JSON value for a readable test failure.
func prettyJSON(t *testing.T, v any) string {
	t.Helper()
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal for diff: %v", err)
	}
	return string(out)
}

func TestRuntimeCreateWorktree(t *testing.T) {
	runtime, got := startFakeRuntime(t, `{"type":"worktree_created",`+
		`"workspace":{"workspace_id":"w9"},"tab":{"tab_id":"t1"},"root_pane":{"pane_id":"w9:p1"},`+
		`"worktree":{"path":"/work/tree","branch":"feature-x","is_bare":false,"is_detached":false,`+
		`"is_prunable":false,"is_linked_worktree":true,"label":"feature-x"}}`)

	info, err := runtime.CreateWorktree(testContext(t), app.WorktreeRequest{
		RepositoryRoot: "/repo/root", Branch: "feature-x", BaseRef: "HEAD",
	})
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if info.WorkspaceID != "w9" || info.Path != "/work/tree" || info.Branch != "feature-x" {
		t.Errorf("info = %+v, want workspace w9, path /work/tree, branch feature-x", info)
	}

	assertRequestParams(t, <-got, "worktree.create", `{"cwd":"/repo/root","branch":"feature-x","base":"HEAD"}`)
}

// TestRuntimeCreateWorktreeSendsLabelWhenSet proves the S9 creation label
// round-trips onto the wire only when the caller supplies one; no landed
// call site sets it yet (label-driven callers land in slices 2b/6), so the
// unlabeled shape above must stay byte-identical.
func TestRuntimeCreateWorktreeSendsLabelWhenSet(t *testing.T) {
	runtime, got := startFakeRuntime(t, `{"type":"worktree_created",`+
		`"workspace":{"workspace_id":"w9"},"tab":{"tab_id":"t1"},"root_pane":{"pane_id":"w9:p1"},`+
		`"worktree":{"path":"/work/tree","branch":"feature-x"}}`)

	_, err := runtime.CreateWorktree(testContext(t), app.WorktreeRequest{
		RepositoryRoot: "/repo/root", Branch: "feature-x", BaseRef: "HEAD", Label: "hop-op-42",
	})
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	assertRequestParams(t, <-got, "worktree.create",
		`{"cwd":"/repo/root","branch":"feature-x","base":"HEAD","label":"hop-op-42"}`)
}

func TestRuntimeCreateWorktreeBranchOmittedIsEmpty(t *testing.T) {
	// branch is schema-optional (a detached checkout has none); its
	// absence must decode to an empty string, not a decode error.
	runtime, _ := startFakeRuntime(t, `{"type":"worktree_created","workspace":{"workspace_id":"w9"},`+
		`"worktree":{"path":"/work/tree"}}`)

	info, err := runtime.CreateWorktree(testContext(t), app.WorktreeRequest{RepositoryRoot: "/repo", Branch: "b", BaseRef: "HEAD"})
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if info.Branch != "" {
		t.Errorf("branch = %q, want empty when the response omits it", info.Branch)
	}
}

func TestRuntimeCreateWorktreeMapsAPIError(t *testing.T) {
	runtime := startFakeRuntimeError(t, "not_git_worktree", "not a git work tree")

	_, err := runtime.CreateWorktree(testContext(t), app.WorktreeRequest{RepositoryRoot: "/repo", Branch: "b", BaseRef: "HEAD"})

	var apiErr *herdr.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "not_git_worktree" {
		t.Fatalf("CreateWorktree error = %v, want an APIError not_git_worktree", err)
	}
}

func TestRuntimeCreateWorktreeMapsPartialResponse(t *testing.T) {
	cases := []struct {
		name   string
		result string
	}{
		{"wrong result type", `{"type":"something_else","workspace":{"workspace_id":"w9"},"worktree":{"path":"/x"}}`},
		{"missing workspace_id", `{"type":"worktree_created","workspace":{},"worktree":{"path":"/x"}}`},
		{"empty workspace_id", `{"type":"worktree_created","workspace":{"workspace_id":""},"worktree":{"path":"/x"}}`},
		{"missing path", `{"type":"worktree_created","workspace":{"workspace_id":"w9"},"worktree":{}}`},
		{"empty path", `{"type":"worktree_created","workspace":{"workspace_id":"w9"},"worktree":{"path":""}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runtime, _ := startFakeRuntime(t, tc.result)

			info, err := runtime.CreateWorktree(testContext(t), app.WorktreeRequest{RepositoryRoot: "/repo", Branch: "b", BaseRef: "HEAD"})

			if err == nil {
				t.Fatalf("CreateWorktree with a %s response did not error; info = %+v", tc.name, info)
			}
			var protocolErr *herdr.ProtocolError
			if !errors.As(err, &protocolErr) {
				t.Fatalf("CreateWorktree error = %v, want a ProtocolError", err)
			}
			if info != (app.WorktreeInfo{}) {
				t.Errorf("info = %+v, want the zero value on error", info)
			}
		})
	}
}

func TestRuntimeOpenWorkerPane(t *testing.T) {
	runtime, got := startFakeRuntime(t, `{"type":"layout_apply","layout":{"workspace_id":"w1","tab_id":"t2",`+
		`"zoomed":false,"focused_pane_id":"w1:p9",`+
		`"root":{"type":"pane","pane_id":"w1:p9","label":"launch-op-1","cwd":"/work/tree","command":["/abs/hop","launch"]}}}`)

	handle, err := runtime.OpenWorkerPane(testContext(t), app.WorkerPaneRequest{
		WorkspaceID: "w1",
		Cwd:         "/work/tree",
		Command:     []string{"/abs/hop", "launch", "--run", "r1"},
		Env:         map[string]string{"HOP_RUN_ID": "r1"},
		Label:       "launch-op-1",
	})
	if err != nil {
		t.Fatalf("OpenWorkerPane: %v", err)
	}
	if handle.WorkspaceID != "w1" || handle.TabID != "t2" || handle.PaneID != "w1:p9" {
		t.Errorf("handle = %+v, want workspace w1, tab t2, pane w1:p9", handle)
	}

	assertRequestParams(t, <-got, "layout.apply", `{"workspace_id":"w1","focus":false,`+
		`"root":{"type":"pane","label":"launch-op-1","cwd":"/work/tree",`+
		`"command":["/abs/hop","launch","--run","r1"],"env":{"HOP_RUN_ID":"r1"}}}`)
}

func TestRuntimeOpenWorkerPaneOmitsEmptyOptionalFields(t *testing.T) {
	runtime, got := startFakeRuntime(t, `{"type":"layout_apply","layout":{"workspace_id":"w1","tab_id":"t2",`+
		`"root":{"type":"pane","pane_id":"w1:p9"}}}`)

	_, err := runtime.OpenWorkerPane(testContext(t), app.WorkerPaneRequest{
		WorkspaceID: "w1",
		Command:     []string{"/bin/x"},
	})
	if err != nil {
		t.Fatalf("OpenWorkerPane: %v", err)
	}

	// No label, cwd or env were supplied: omitempty must drop them from the
	// wire request rather than sending empty strings/objects.
	assertRequestParams(t, <-got, "layout.apply", `{"workspace_id":"w1","focus":false,"root":{"type":"pane","command":["/bin/x"]}}`)
}

func TestRuntimeOpenWorkerPaneRequiresWorkspaceID(t *testing.T) {
	got := make(chan map[string]any, 1)
	endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
		request := readRequestLine(t, bufio.NewReader(conn))
		got <- request
	})
	runtime := herdr.NewRuntime(endpoint.socketPath)

	_, err := runtime.OpenWorkerPane(testContext(t), app.WorkerPaneRequest{Command: []string{"/bin/x"}})

	if !errors.Is(err, herdr.ErrWorkspaceIDRequired) {
		t.Fatalf("OpenWorkerPane error = %v, want ErrWorkspaceIDRequired", err)
	}
	select {
	case <-got:
		t.Error("OpenWorkerPane sent a request although WorkspaceID was empty")
	default:
	}
}

func TestRuntimeOpenWorkerPaneMapsPartialResponse(t *testing.T) {
	cases := []struct {
		name   string
		result string
	}{
		{"wrong result type", `{"type":"something_else","layout":{"workspace_id":"w1","tab_id":"t2","root":{"pane_id":"p9"}}}`},
		{"missing workspace_id", `{"type":"layout_apply","layout":{"tab_id":"t2","root":{"pane_id":"p9"}}}`},
		{"missing tab_id", `{"type":"layout_apply","layout":{"workspace_id":"w1","root":{"pane_id":"p9"}}}`},
		{"missing root pane_id", `{"type":"layout_apply","layout":{"workspace_id":"w1","tab_id":"t2","root":{}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runtime, _ := startFakeRuntime(t, tc.result)

			handle, err := runtime.OpenWorkerPane(testContext(t), app.WorkerPaneRequest{WorkspaceID: "w1", Command: []string{"/bin/x"}})

			if err == nil {
				t.Fatalf("OpenWorkerPane with a %s response did not error; handle = %+v", tc.name, handle)
			}
			var protocolErr *herdr.ProtocolError
			if !errors.As(err, &protocolErr) {
				t.Fatalf("OpenWorkerPane error = %v, want a ProtocolError", err)
			}
			if handle != (app.PaneHandle{}) {
				t.Errorf("handle = %+v, want the zero value on error", handle)
			}
		})
	}
}

func TestRuntimeFindPaneByLabel(t *testing.T) {
	t.Run("unique label found", func(t *testing.T) {
		runtime, got := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{"panes":[`+
			`{"pane_id":"w1:p1","workspace_id":"w1","tab_id":"t1","label":"other"},`+
			`{"pane_id":"w1:p2","workspace_id":"w1","tab_id":"t2","label":"launch-op-1"}]}}`)

		ref, found, err := runtime.FindPaneByLabel(testContext(t), "launch-op-1")
		if err != nil {
			t.Fatalf("FindPaneByLabel: %v", err)
		}
		if !found {
			t.Fatal("found = false, want true")
		}
		want := app.PaneRef{WorkspaceID: "w1", TabID: "t2", PaneID: "w1:p2"}
		if ref != want {
			t.Errorf("ref = %+v, want %+v", ref, want)
		}

		assertRequestParams(t, <-got, "session.snapshot", `{}`)
	})

	t.Run("no match", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{"panes":[`+
			`{"pane_id":"w1:p1","workspace_id":"w1","tab_id":"t1","label":"other"}]}}`)

		ref, found, err := runtime.FindPaneByLabel(testContext(t), "missing")
		if err != nil {
			t.Fatalf("FindPaneByLabel: %v", err)
		}
		if found {
			t.Error("found = true, want false for a label no pane carries")
		}
		if ref != (app.PaneRef{}) {
			t.Errorf("ref = %+v, want the zero value when not found", ref)
		}
	})

	t.Run("no panes at all is a legitimate not-found", func(t *testing.T) {
		// panes is schema-required on SessionSnapshot, so the legitimate
		// "no panes" state is an explicit empty array, not an absent key.
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{"panes":[]}}`)

		_, found, err := runtime.FindPaneByLabel(testContext(t), "missing")
		if err != nil {
			t.Fatalf("FindPaneByLabel: %v", err)
		}
		if found {
			t.Error("found = true, want false when the snapshot has no panes")
		}
	})

	t.Run("ambiguous label is an error", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{"panes":[`+
			`{"pane_id":"w1:p1","workspace_id":"w1","tab_id":"t1","label":"dup"},`+
			`{"pane_id":"w2:p1","workspace_id":"w2","tab_id":"t2","label":"dup"}]}}`)

		ref, found, err := runtime.FindPaneByLabel(testContext(t), "dup")

		if err == nil {
			t.Fatal("FindPaneByLabel with a label two panes carry did not error")
		}
		if found || ref != (app.PaneRef{}) {
			t.Errorf("found = %v, ref = %+v, want the zero value and false alongside the error", found, ref)
		}
	})

	t.Run("transport error propagates", func(t *testing.T) {
		runtime := herdr.NewRuntime(deadSocket(t))

		_, found, err := runtime.FindPaneByLabel(testContext(t), "any")

		if err == nil {
			t.Fatal("FindPaneByLabel against a dead socket did not error")
		}
		if found {
			t.Error("found = true against a dead socket")
		}
	})

	t.Run("wrong result type", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"something_else","snapshot":{"panes":[]}}`)

		_, found, err := runtime.FindPaneByLabel(testContext(t), "any")

		var protocolErr *herdr.ProtocolError
		if !errors.As(err, &protocolErr) {
			t.Fatalf("FindPaneByLabel error = %v, want a ProtocolError", err)
		}
		if found {
			t.Error("found = true on a result-type mismatch")
		}
	})

	t.Run("missing snapshot wrapper", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot"}`)

		_, found, err := runtime.FindPaneByLabel(testContext(t), "any")

		var protocolErr *herdr.ProtocolError
		if !errors.As(err, &protocolErr) {
			t.Fatalf("FindPaneByLabel error = %v, want a ProtocolError for a missing snapshot wrapper", err)
		}
		if found {
			t.Error("found = true although the snapshot wrapper is missing")
		}
	})

	t.Run("missing panes", func(t *testing.T) {
		// panes is schema-required on SessionSnapshot; an absent key (as
		// opposed to an explicit empty array) is a protocol violation, not
		// a legitimate "no panes" state.
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{}}`)

		ref, found, err := runtime.FindPaneByLabel(testContext(t), "any")

		var protocolErr *herdr.ProtocolError
		if !errors.As(err, &protocolErr) {
			t.Fatalf("FindPaneByLabel error = %v, want a ProtocolError for a missing panes field", err)
		}
		if found || ref != (app.PaneRef{}) {
			t.Errorf("found = %v, ref = %+v, want the zero value and false alongside the error", found, ref)
		}
	})

	t.Run("matched pane missing required ids", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{"panes":[{"label":"launch-op-1"}]}}`)

		ref, found, err := runtime.FindPaneByLabel(testContext(t), "launch-op-1")

		var protocolErr *herdr.ProtocolError
		if !errors.As(err, &protocolErr) {
			t.Fatalf("FindPaneByLabel error = %v, want a ProtocolError for a matched pane missing its ids", err)
		}
		if found || ref != (app.PaneRef{}) {
			t.Errorf("found = %v, ref = %+v, want the zero value and false alongside the error", found, ref)
		}
	})
}

func TestRuntimeReadPane(t *testing.T) {
	runtime, got := startFakeRuntime(t, `{"type":"pane_read","read":{"pane_id":"w1:p1","workspace_id":"w1",`+
		`"tab_id":"t1","source":"recent","format":"text","text":"line one\nline two\n","revision":3,"truncated":false}}`)

	text, err := runtime.ReadPane(testContext(t), "w1:p1", 50)
	if err != nil {
		t.Fatalf("ReadPane: %v", err)
	}
	if text != "line one\nline two\n" {
		t.Errorf("text = %q, want the exact scrollback text", text)
	}

	assertRequestParams(t, <-got, "pane.read", `{"pane_id":"w1:p1","source":"recent","lines":50}`)
}

func TestRuntimeReadPaneOmitsLinesWhenNotPositive(t *testing.T) {
	runtime, got := startFakeRuntime(t, `{"type":"pane_read","read":{"text":""}}`)

	text, err := runtime.ReadPane(testContext(t), "w1:p1", 0)
	if err != nil {
		t.Fatalf("ReadPane: %v", err)
	}
	if text != "" {
		t.Errorf("text = %q, want the legitimate empty scrollback text", text)
	}

	assertRequestParams(t, <-got, "pane.read", `{"pane_id":"w1:p1","source":"recent"}`)
}

func TestRuntimeReadPaneMapsPaneNotFound(t *testing.T) {
	runtime := startFakeRuntimeError(t, "pane_not_found", "pane not found")

	_, err := runtime.ReadPane(testContext(t), "w1:p1", 10)

	if !errors.Is(err, herdr.ErrPaneNotFound) {
		t.Fatalf("ReadPane error = %v, want ErrPaneNotFound", err)
	}
}

func TestRuntimeReadPaneMapsPartialResponse(t *testing.T) {
	cases := []struct {
		name   string
		result string
	}{
		{"wrong result type", `{"type":"something_else","read":{"text":"x"}}`},
		{"missing read wrapper", `{"type":"pane_read"}`},
		{"missing text", `{"type":"pane_read","read":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runtime, _ := startFakeRuntime(t, tc.result)

			text, err := runtime.ReadPane(testContext(t), "w1:p1", 10)

			if err == nil {
				t.Fatalf("ReadPane with a %s response did not error; text = %q", tc.name, text)
			}
			var protocolErr *herdr.ProtocolError
			if !errors.As(err, &protocolErr) {
				t.Fatalf("ReadPane error = %v, want a ProtocolError", err)
			}
			if text != "" {
				t.Errorf("text = %q, want empty on error", text)
			}
		})
	}
}

func TestRuntimeInspectPane(t *testing.T) {
	runtime, got := startFakeRuntime(t, `{"type":"pane_process_info","process_info":{"pane_id":"w1:p1",`+
		`"shell_pid":100,"foreground_process_group_id":150,"tty":"",`+
		`"foreground_processes":[{"pid":150,"name":"claude","argv0":"claude",`+
		`"argv":["claude","--run","r1"],"cmdline":"claude --run r1","cwd":"/work/tree"}]}}`)

	process, err := runtime.InspectPane(testContext(t), "w1:p1")
	if err != nil {
		t.Fatalf("InspectPane: %v", err)
	}
	if process.ShellPID != 100 || process.ForegroundGroupID != 150 {
		t.Errorf("process = %+v, want shell pid 100, foreground group 150", process)
	}
	if len(process.Foreground) != 1 {
		t.Fatalf("foreground = %+v, want exactly one process", process.Foreground)
	}
	fg := process.Foreground[0]
	if fg.PID != 150 || fg.Name != "claude" || fg.Argv0 != "claude" || fg.Cmdline != "claude --run r1" || fg.Cwd != "/work/tree" {
		t.Errorf("foreground process = %+v, want the decoded S2 fields", fg)
	}
	if strings.Join(fg.Argv, " ") != "claude --run r1" {
		t.Errorf("argv = %q, want the full argv vector", fg.Argv)
	}
	// The observation carries the lifetime of the server that answered it,
	// read on the request's own connection.
	wantServerLifetime(t, process.ServerInstance)

	assertRequestParams(t, <-got, "pane.process_info", `{"pane_id":"w1:p1"}`)
}

func TestRuntimeInspectPaneEmptyForeground(t *testing.T) {
	runtime, _ := startFakeRuntime(t, `{"type":"pane_process_info","process_info":{"pane_id":"w1:p1",`+
		`"shell_pid":100,"foreground_process_group_id":100,"foreground_processes":[]}}`)

	process, err := runtime.InspectPane(testContext(t), "w1:p1")
	if err != nil {
		t.Fatalf("InspectPane: %v", err)
	}
	if len(process.Foreground) != 0 {
		t.Errorf("foreground = %+v, want empty: no foreground process observed", process.Foreground)
	}
}

func TestRuntimeInspectPaneOmittedForegroundProcesses(t *testing.T) {
	// Herdr's own schema omits foreground_processes entirely when empty
	// (skip_serializing_if = "Vec::is_empty"); this must behave identically
	// to an explicit empty array, not be treated as a missing field.
	runtime, _ := startFakeRuntime(t, `{"type":"pane_process_info","process_info":{"pane_id":"w1:p1",`+
		`"shell_pid":100,"foreground_process_group_id":100}}`)

	process, err := runtime.InspectPane(testContext(t), "w1:p1")
	if err != nil {
		t.Fatalf("InspectPane: %v", err)
	}
	if len(process.Foreground) != 0 {
		t.Errorf("foreground = %+v, want empty when the field is omitted", process.Foreground)
	}
}

func TestRuntimeInspectPaneMapsPaneNotFound(t *testing.T) {
	// This is the specific classification the application layer relies on
	// to distinguish "no runtime yet" from every other InspectPane failure.
	// app.ErrPaneNotFound is the app.Runtime port's own absence contract;
	// ErrPaneNotFound is this adapter's local sentinel for the same case.
	runtime := startFakeRuntimeError(t, "pane_not_found", "pane not found")

	_, err := runtime.InspectPane(testContext(t), "w1:p1")

	if !errors.Is(err, herdr.ErrPaneNotFound) {
		t.Fatalf("InspectPane error = %v, want ErrPaneNotFound", err)
	}
	if !errors.Is(err, app.ErrPaneNotFound) {
		t.Fatalf("InspectPane error = %v, want app.ErrPaneNotFound", err)
	}
}

// TestRuntimeInspectPaneAppErrPaneNotFoundClassification proves
// errors.Is(err, app.ErrPaneNotFound) — the app.Runtime port's "positively
// does not exist" absence contract — holds for a pane_not_found API error
// and only that case: a transport failure (dead socket), an unrelated API
// error code and a protocol-level decode failure (wrong result type) must
// all produce an error that does NOT satisfy it. FindPaneByLabel's own
// not-found reporting is untouched by this: it stays (zero value, false,
// nil) with no error at all, proven by TestRuntimeFindPaneByLabel above.
func TestRuntimeInspectPaneAppErrPaneNotFoundClassification(t *testing.T) {
	cases := []struct {
		name            string
		runtime         func(t *testing.T) *herdr.Runtime
		wantAppNotFound bool
	}{
		{
			name:            "pane_not_found API error",
			runtime:         func(t *testing.T) *herdr.Runtime { return startFakeRuntimeError(t, "pane_not_found", "pane not found") },
			wantAppNotFound: true,
		},
		{
			name:            "unrelated API error",
			runtime:         func(t *testing.T) *herdr.Runtime { return startFakeRuntimeError(t, "internal_error", "boom") },
			wantAppNotFound: false,
		},
		{
			name:            "transport error (dead socket)",
			runtime:         func(t *testing.T) *herdr.Runtime { return herdr.NewRuntime(deadSocket(t)) },
			wantAppNotFound: false,
		},
		{
			name: "protocol error (wrong result type)",
			runtime: func(t *testing.T) *herdr.Runtime {
				runtime, _ := startFakeRuntime(t, `{"type":"something_else","process_info":{}}`)
				return runtime
			},
			wantAppNotFound: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runtime := tc.runtime(t)

			_, err := runtime.InspectPane(testContext(t), "w1:p1")

			if err == nil {
				t.Fatalf("InspectPane did not error for %s", tc.name)
			}
			if got := errors.Is(err, app.ErrPaneNotFound); got != tc.wantAppNotFound {
				t.Errorf("errors.Is(err, app.ErrPaneNotFound) = %v, want %v (err = %v)", got, tc.wantAppNotFound, err)
			}
		})
	}
}

func TestRuntimeInspectPaneUnknownAPIErrorIsNotPaneNotFound(t *testing.T) {
	runtime := startFakeRuntimeError(t, "internal_error", "boom")

	_, err := runtime.InspectPane(testContext(t), "w1:p1")

	if errors.Is(err, herdr.ErrPaneNotFound) {
		t.Fatalf("InspectPane error = %v, want it NOT classified as ErrPaneNotFound", err)
	}
	var apiErr *herdr.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "internal_error" {
		t.Fatalf("InspectPane error = %v, want an unwrapped APIError internal_error", err)
	}
}

func TestRuntimeInspectPaneMapsPartialResponse(t *testing.T) {
	cases := []struct {
		name   string
		result string
	}{
		{"wrong result type", `{"type":"something_else","process_info":{}}`},
		{"missing process_info wrapper", `{"type":"pane_process_info"}`},
		{
			"foreground process missing pid",
			`{"type":"pane_process_info","process_info":{"foreground_processes":[{"name":"claude"}]}}`,
		},
		{
			"foreground process missing name",
			`{"type":"pane_process_info","process_info":{"foreground_processes":[{"pid":150}]}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runtime, _ := startFakeRuntime(t, tc.result)

			process, err := runtime.InspectPane(testContext(t), "w1:p1")

			if err == nil {
				t.Fatalf("InspectPane with a %s response did not error; process = %+v", tc.name, process)
			}
			var protocolErr *herdr.ProtocolError
			if !errors.As(err, &protocolErr) {
				t.Fatalf("InspectPane error = %v, want a ProtocolError", err)
			}
			if process.ShellPID != 0 || process.ForegroundGroupID != 0 || len(process.Foreground) != 0 {
				t.Errorf("process = %+v, want the zero value on error", process)
			}
		})
	}
}

func TestRuntimeInspectPaneRejectsWrongTypedArgv(t *testing.T) {
	// argv as a string instead of an array is a JSON type mismatch that
	// the standard decoder itself rejects, before any adapter-level field
	// validation runs.
	runtime, _ := startFakeRuntime(t, `{"type":"pane_process_info","process_info":{`+
		`"foreground_processes":[{"pid":150,"name":"claude","argv":"not-an-array"}]}}`)

	_, err := runtime.InspectPane(testContext(t), "w1:p1")

	var protocolErr *herdr.ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("InspectPane error = %v, want a ProtocolError for a wrong-typed argv", err)
	}
}

func TestRuntimeClosePane(t *testing.T) {
	runtime, got := startFakeRuntime(t, `{"type":"ok"}`)

	if err := runtime.ClosePane(testContext(t), "w1:p1"); err != nil {
		t.Fatalf("ClosePane: %v", err)
	}

	assertRequestParams(t, <-got, "pane.close", `{"pane_id":"w1:p1"}`)
}

func TestRuntimeClosePaneMapsPaneNotFound(t *testing.T) {
	// pane.close answers pane_not_found with "pane <id> not found", the
	// shape spike_panevanish pins against a real server.
	runtime := startFakeRuntimeError(t, "pane_not_found", "pane w1:p1 not found")

	err := runtime.ClosePane(testContext(t), "w1:p1")

	if !errors.Is(err, herdr.ErrPaneNotFound) {
		t.Fatalf("ClosePane error = %v, want ErrPaneNotFound", err)
	}
	if !errors.Is(err, app.ErrPaneNotFound) {
		t.Fatalf("ClosePane error = %v, want app.ErrPaneNotFound", err)
	}
}

// TestRuntimeClosePaneAppErrPaneNotFoundClassification proves ClosePane
// satisfies app.ErrPaneNotFound — the port's "the server has no such
// pane" contract — for a pane_not_found API error and only that case.
func TestRuntimeClosePaneAppErrPaneNotFoundClassification(t *testing.T) {
	cases := []struct {
		name            string
		runtime         func(t *testing.T) *herdr.Runtime
		wantAppNotFound bool
	}{
		{
			name: "pane_not_found API error",
			runtime: func(t *testing.T) *herdr.Runtime {
				return startFakeRuntimeError(t, "pane_not_found", "pane w1:p1 not found")
			},
			wantAppNotFound: true,
		},
		{
			name: "confirmation_required API error",
			runtime: func(t *testing.T) *herdr.Runtime {
				return startFakeRuntimeError(t, "confirmation_required", "closing this pane would close a worktree group")
			},
		},
		{
			name:    "transport error (dead socket)",
			runtime: func(t *testing.T) *herdr.Runtime { return herdr.NewRuntime(deadSocket(t)) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.runtime(t).ClosePane(testContext(t), "w1:p1")

			if err == nil {
				t.Fatalf("ClosePane did not error for %s", tc.name)
			}
			if got := errors.Is(err, app.ErrPaneNotFound); got != tc.wantAppNotFound {
				t.Errorf("errors.Is(err, app.ErrPaneNotFound) = %v, want %v (err = %v)", got, tc.wantAppNotFound, err)
			}
			if got := errors.Is(err, herdr.ErrPaneNotFound); got != tc.wantAppNotFound {
				t.Errorf("errors.Is(err, herdr.ErrPaneNotFound) = %v, want %v (err = %v)", got, tc.wantAppNotFound, err)
			}
		})
	}
}

// wantServerLifetime is the token ServerInstance renders for a server that
// is this very test process — the fake endpoint's listener and the dialer
// are the same OS process: on darwin the lifetime format naming this
// process's pid (its start time is pinned against ps by the package's
// internal TestProcessStartTimeMatchesTheProcessTable); on every other
// platform unknown.
func wantServerLifetime(t *testing.T, token string) {
	t.Helper()
	if goruntime.GOOS != "darwin" {
		if token != "" {
			t.Errorf("token = %q, want \"\" (no lifetime identity is implemented on %s)", token, goruntime.GOOS)
		}
		return
	}
	pattern := regexp.MustCompile(fmt.Sprintf(`^herdr-server-lifetime/v1 pid=%d start=[1-9][0-9]*\.[0-9]{6}$`, os.Getpid()))
	if !pattern.MatchString(token) {
		t.Errorf("token = %q, want %s", token, pattern)
	}
}

// TestRuntimeServerInstance proves the token identifies the server
// lifetime behind the dialed socket, and is stable across calls to the
// same server.
func TestRuntimeServerInstance(t *testing.T) {
	endpoint := startFakeEndpoint(t, func(_ *testing.T, conn net.Conn) {
		holdUntilPeerCloses(conn)
	})
	runtime := herdr.NewRuntime(endpoint.socketPath)

	token, err := runtime.ServerInstance(testContext(t))
	if err != nil {
		t.Fatalf("ServerInstance: %v", err)
	}
	wantServerLifetime(t, token)

	again, err := runtime.ServerInstance(testContext(t))
	if err != nil {
		t.Fatalf("second ServerInstance: %v", err)
	}
	if again != token {
		t.Errorf("second token = %q, want %q: one server lifetime has one token", again, token)
	}
}

// TestRuntimeServerInstanceMapsDialFailure proves ServerInstance errors
// only on a genuine connection failure, never returning a fabricated token.
func TestRuntimeServerInstanceMapsDialFailure(t *testing.T) {
	runtime := herdr.NewRuntime(deadSocket(t))

	token, err := runtime.ServerInstance(testContext(t))

	if err == nil {
		t.Fatal("ServerInstance against a dead socket did not error")
	}
	if token != "" {
		t.Errorf("token = %q, want empty on a dial failure", token)
	}
}

// TestRuntimeMapsMalformedResponse proves a malformed response line
// surfaces as the Client's own ProtocolError, unchanged by this adapter,
// for a representative Runtime method.
func TestRuntimeMapsMalformedResponse(t *testing.T) {
	endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
		request := readRequestLine(t, bufio.NewReader(conn))
		if request == nil {
			return
		}
		// An id-only frame carries neither a result nor an error.
		writeLine(t, conn, fmt.Sprintf(`{"id":%q}`, requestID(t, request)))
	})
	runtime := herdr.NewRuntime(endpoint.socketPath)

	_, err := runtime.CreateWorktree(testContext(t), app.WorktreeRequest{RepositoryRoot: "/repo", Branch: "b", BaseRef: "HEAD"})

	var protocolErr *herdr.ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("CreateWorktree error = %v, want a ProtocolError for a resultless response", err)
	}
}

// TestRuntimeMapsResponseEnvelopeIDTypeMismatch proves a response whose id
// is the wrong JSON type (a number instead of a string) surfaces as the
// Client's own ProtocolError from decoding the envelope itself, before any
// adapter-level result decoding runs.
func TestRuntimeMapsResponseEnvelopeIDTypeMismatch(t *testing.T) {
	endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
		if request := readRequestLine(t, bufio.NewReader(conn)); request == nil {
			return
		}
		writeLine(t, conn, `{"id":12345,"result":{"type":"worktree_created"}}`)
	})
	runtime := herdr.NewRuntime(endpoint.socketPath)

	_, err := runtime.CreateWorktree(testContext(t), app.WorktreeRequest{RepositoryRoot: "/repo", Branch: "b", BaseRef: "HEAD"})

	var protocolErr *herdr.ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("CreateWorktree error = %v, want a ProtocolError for a non-string response id", err)
	}
}

// TestRuntimeHonorsCancellation proves every Runtime method returns
// promptly with context.Canceled when its request never gets a response
// and the caller's context is canceled, the same guarantee Client.Call
// itself provides.
func TestRuntimeHonorsCancellation(t *testing.T) {
	blocked := make(chan struct{})
	endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
		if readCancelableRequestLine(t, bufio.NewReader(conn)) == nil {
			return // The dial itself was abandoned before any request was written.
		}
		<-blocked // Never respond until the test ends.
	})
	t.Cleanup(func() { close(blocked) })
	runtime := herdr.NewRuntime(endpoint.socketPath)

	cases := []struct {
		name string
		call func(ctx context.Context) error
	}{
		{"CreateWorktree", func(ctx context.Context) error {
			_, err := runtime.CreateWorktree(ctx, app.WorktreeRequest{RepositoryRoot: "/repo", Branch: "b", BaseRef: "HEAD"})
			return err
		}},
		{"OpenWorkerPane", func(ctx context.Context) error {
			_, err := runtime.OpenWorkerPane(ctx, app.WorkerPaneRequest{WorkspaceID: "w1", Command: []string{"/bin/x"}})
			return err
		}},
		{"FindPaneByLabel", func(ctx context.Context) error {
			_, _, err := runtime.FindPaneByLabel(ctx, "label")
			return err
		}},
		{"ReadPane", func(ctx context.Context) error { _, err := runtime.ReadPane(ctx, "w1:p1", 10); return err }},
		{"InspectPane", func(ctx context.Context) error { _, err := runtime.InspectPane(ctx, "w1:p1"); return err }},
		{"ClosePane", func(ctx context.Context) error { return runtime.ClosePane(ctx, "w1:p1") }},
		{"CreateWorkspace", func(ctx context.Context) error {
			_, err := runtime.CreateWorkspace(ctx, app.WorkspaceRequest{Cwd: "/repo", Label: "op-1"})
			return err
		}},
		{"FindWorkspaceByLabel", func(ctx context.Context) error {
			_, _, err := runtime.FindWorkspaceByLabel(ctx, "label")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(testContext(t))
			done := make(chan error, 1)
			go func() { done <- tc.call(ctx) }()
			cancel()

			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("%s error = %v, want context.Canceled", tc.name, err)
				}
			case <-time.After(protocolTimeout):
				t.Fatalf("%s did not return after cancellation", tc.name)
			}
		})
	}
}
