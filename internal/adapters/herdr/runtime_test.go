package herdr_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

	request := <-got
	if request["method"] != "worktree.create" {
		t.Fatalf("method = %v, want worktree.create", request["method"])
	}
	params := paramsOf(t, request)
	if params["cwd"] != "/repo/root" || params["branch"] != "feature-x" || params["base"] != "HEAD" {
		t.Errorf("params = %v, want cwd/branch/base from the request", params)
	}
	if len(params) != 3 {
		t.Errorf("params = %v, want exactly cwd, branch and base — worktree.create carries no env", params)
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

	request := <-got
	if request["method"] != "layout.apply" {
		t.Fatalf("method = %v, want layout.apply", request["method"])
	}
	rawParams, marshalErr := json.Marshal(request["params"])
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	var params struct {
		WorkspaceID string `json:"workspace_id"`
		Focus       bool   `json:"focus"`
		Root        struct {
			Type    string            `json:"type"`
			Label   string            `json:"label"`
			Cwd     string            `json:"cwd"`
			Command []string          `json:"command"`
			Env     map[string]string `json:"env"`
		} `json:"root"`
	}
	if err := json.Unmarshal(rawParams, &params); err != nil {
		t.Fatal(err)
	}
	if params.WorkspaceID != "w1" {
		t.Errorf("workspace_id = %q, want w1", params.WorkspaceID)
	}
	if params.Focus {
		t.Error("focus = true, want false so opening a worker pane never steals focus")
	}
	if _, hasTabID := paramsOf(t, request)["tab_id"]; hasTabID {
		t.Error("layout.apply must never carry tab_id: naming one replaces that tab instead of adding one")
	}
	if params.Root.Type != "pane" || params.Root.Label != "launch-op-1" || params.Root.Cwd != "/work/tree" {
		t.Errorf("root = %+v, want type pane, label launch-op-1, cwd /work/tree", params.Root)
	}
	if got := strings.Join(params.Root.Command, " "); got != "/abs/hop launch --run r1" {
		t.Errorf("command = %q, want the exact launch argv", got)
	}
	if params.Root.Env["HOP_RUN_ID"] != "r1" {
		t.Errorf("env = %v, want HOP_RUN_ID r1", params.Root.Env)
	}
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

		request := <-got
		if request["method"] != "session.snapshot" {
			t.Errorf("method = %v, want session.snapshot", request["method"])
		}
		if params := paramsOf(t, request); len(params) != 0 {
			t.Errorf("params = %v, want no params for session.snapshot", params)
		}
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
}

func TestRuntimeSendText(t *testing.T) {
	runtime, got := startFakeRuntime(t, `{"type":"ok"}`)
	const text = "exec '/abs/hop' launch --run r1 --attempt a1\n"

	if err := runtime.SendText(testContext(t), "w1:p1", text); err != nil {
		t.Fatalf("SendText: %v", err)
	}

	request := <-got
	if request["method"] != "pane.send_text" {
		t.Fatalf("method = %v, want pane.send_text", request["method"])
	}
	params := paramsOf(t, request)
	if params["pane_id"] != "w1:p1" || params["text"] != text {
		t.Errorf("params = %v, want the exact pane id and text", params)
	}
}

func TestRuntimeSendTextMapsPaneNotFound(t *testing.T) {
	runtime := startFakeRuntimeError(t, "pane_not_found", "pane w1:p1 not found")

	err := runtime.SendText(testContext(t), "w1:p1", "text")

	if !errors.Is(err, herdr.ErrPaneNotFound) {
		t.Fatalf("SendText error = %v, want ErrPaneNotFound", err)
	}
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

	params := paramsOf(t, <-got)
	if params["pane_id"] != "w1:p1" || params["source"] != "recent" || params["lines"] != float64(50) {
		t.Errorf("params = %v, want pane_id w1:p1, source recent, lines 50", params)
	}
}

func TestRuntimeReadPaneOmitsLinesWhenNotPositive(t *testing.T) {
	runtime, got := startFakeRuntime(t, `{"type":"pane_read","read":{"text":""}}`)

	if _, err := runtime.ReadPane(testContext(t), "w1:p1", 0); err != nil {
		t.Fatalf("ReadPane: %v", err)
	}

	params := paramsOf(t, <-got)
	if _, present := params["lines"]; present {
		t.Errorf("params = %v, want lines omitted for a non-positive line count", params)
	}
}

func TestRuntimeReadPaneMapsPaneNotFound(t *testing.T) {
	runtime := startFakeRuntimeError(t, "pane_not_found", "pane not found")

	_, err := runtime.ReadPane(testContext(t), "w1:p1", 10)

	if !errors.Is(err, herdr.ErrPaneNotFound) {
		t.Fatalf("ReadPane error = %v, want ErrPaneNotFound", err)
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

	request := <-got
	if request["method"] != "pane.process_info" {
		t.Fatalf("method = %v, want pane.process_info", request["method"])
	}
	if params := paramsOf(t, request); params["pane_id"] != "w1:p1" {
		t.Errorf("params = %v, want pane_id w1:p1", params)
	}
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

func TestRuntimeInspectPaneMapsPaneNotFound(t *testing.T) {
	// This is the specific classification the application layer relies on
	// to distinguish "no runtime yet" from every other InspectPane failure.
	runtime := startFakeRuntimeError(t, "pane_not_found", "pane not found")

	_, err := runtime.InspectPane(testContext(t), "w1:p1")

	if !errors.Is(err, herdr.ErrPaneNotFound) {
		t.Fatalf("InspectPane error = %v, want ErrPaneNotFound", err)
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

func TestRuntimeClosePane(t *testing.T) {
	runtime, got := startFakeRuntime(t, `{"type":"ok"}`)

	if err := runtime.ClosePane(testContext(t), "w1:p1"); err != nil {
		t.Fatalf("ClosePane: %v", err)
	}

	request := <-got
	if request["method"] != "pane.close" {
		t.Fatalf("method = %v, want pane.close", request["method"])
	}
	params := paramsOf(t, request)
	if len(params) != 1 || params["pane_id"] != "w1:p1" {
		t.Errorf("params = %v, want exactly pane_id w1:p1", params)
	}
}

func TestRuntimeClosePaneMapsPaneNotFound(t *testing.T) {
	runtime := startFakeRuntimeError(t, "pane_not_found", "pane not found")

	err := runtime.ClosePane(testContext(t), "w1:p1")

	if !errors.Is(err, herdr.ErrPaneNotFound) {
		t.Fatalf("ClosePane error = %v, want ErrPaneNotFound", err)
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

// TestRuntimeHonorsCancellation proves every Runtime method returns
// promptly with context.Canceled when its request never gets a response
// and the caller's context is canceled, the same guarantee Client.Call
// itself provides.
func TestRuntimeHonorsCancellation(t *testing.T) {
	blocked := make(chan struct{})
	endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
		_ = readRequestLine(t, bufio.NewReader(conn))
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
		{"SendText", func(ctx context.Context) error { return runtime.SendText(ctx, "w1:p1", "text") }},
		{"ReadPane", func(ctx context.Context) error { _, err := runtime.ReadPane(ctx, "w1:p1", 10); return err }},
		{"InspectPane", func(ctx context.Context) error { _, err := runtime.InspectPane(ctx, "w1:p1"); return err }},
		{"ClosePane", func(ctx context.Context) error { return runtime.ClosePane(ctx, "w1:p1") }},
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
