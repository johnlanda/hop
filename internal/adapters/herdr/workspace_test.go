package herdr_test

import (
	"errors"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/herdr"
	"github.com/johnlanda/hop/internal/app"
)

func TestRuntimeCreateWorkspace(t *testing.T) {
	runtime, got := startFakeRuntime(t, `{"type":"workspace_created",`+
		`"workspace":{"workspace_id":"w1","label":"hop-manager-1","focused":false,"tab_count":1,"pane_count":1},`+
		`"tab":{"tab_id":"t1","workspace_id":"w1"},`+
		`"root_pane":{"pane_id":"w1:p1","workspace_id":"w1","tab_id":"t1","cwd":"/repo"}}`)

	handle, err := runtime.CreateWorkspace(testContext(t), app.WorkspaceRequest{
		Cwd: "/repo", Label: "hop-manager-1", Env: map[string]string{"HOP_RUN_ID": "r1"},
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	want := app.WorkspaceHandle{WorkspaceID: "w1", TabID: "t1", PaneID: "w1:p1"}
	if handle != want {
		t.Errorf("handle = %+v, want %+v", handle, want)
	}

	assertRequestParams(t, <-got, "workspace.create",
		`{"cwd":"/repo","focus":false,"label":"hop-manager-1","env":{"HOP_RUN_ID":"r1"}}`)
}

func TestRuntimeCreateWorkspaceOmitsEmptyOptionalFields(t *testing.T) {
	runtime, got := startFakeRuntime(t, `{"type":"workspace_created",`+
		`"workspace":{"workspace_id":"w1"},"tab":{"tab_id":"t1"},"root_pane":{"pane_id":"w1:p1"}}`)

	_, err := runtime.CreateWorkspace(testContext(t), app.WorkspaceRequest{Cwd: "/repo"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	assertRequestParams(t, <-got, "workspace.create", `{"cwd":"/repo","focus":false}`)
}

func TestRuntimeCreateWorkspaceNeverRequestsFocus(t *testing.T) {
	// focus is always false regardless of what the port caller does; the
	// port carries no focus field at all, so this proves the adapter never
	// derives true from anything the caller supplies.
	runtime, got := startFakeRuntime(t, `{"type":"workspace_created",`+
		`"workspace":{"workspace_id":"w1"},"tab":{"tab_id":"t1"},"root_pane":{"pane_id":"w1:p1"}}`)

	_, err := runtime.CreateWorkspace(testContext(t), app.WorkspaceRequest{Cwd: "/repo", Label: "op-1"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	request := <-got
	params, ok := request["params"].(map[string]any)
	if !ok {
		t.Fatalf("params = %v, want an object", request["params"])
	}
	if focus, ok := params["focus"].(bool); !ok || focus {
		t.Errorf("params[focus] = %v, want false", params["focus"])
	}
}

func TestRuntimeCreateWorkspaceMapsAPIError(t *testing.T) {
	runtime := startFakeRuntimeError(t, "invalid_cwd", "cwd does not exist")

	_, err := runtime.CreateWorkspace(testContext(t), app.WorkspaceRequest{Cwd: "/nope"})

	var apiErr *herdr.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "invalid_cwd" {
		t.Fatalf("CreateWorkspace error = %v, want an APIError invalid_cwd", err)
	}
}

func TestRuntimeCreateWorkspaceMapsPartialResponse(t *testing.T) {
	cases := []struct {
		name   string
		result string
	}{
		{"wrong result type", `{"type":"something_else","workspace":{"workspace_id":"w1"},"tab":{"tab_id":"t1"},"root_pane":{"pane_id":"p1"}}`},
		{"missing workspace_id", `{"type":"workspace_created","workspace":{},"tab":{"tab_id":"t1"},"root_pane":{"pane_id":"p1"}}`},
		{"empty workspace_id", `{"type":"workspace_created","workspace":{"workspace_id":""},"tab":{"tab_id":"t1"},"root_pane":{"pane_id":"p1"}}`},
		{"missing tab_id", `{"type":"workspace_created","workspace":{"workspace_id":"w1"},"tab":{},"root_pane":{"pane_id":"p1"}}`},
		{"missing pane_id", `{"type":"workspace_created","workspace":{"workspace_id":"w1"},"tab":{"tab_id":"t1"},"root_pane":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runtime, _ := startFakeRuntime(t, tc.result)

			handle, err := runtime.CreateWorkspace(testContext(t), app.WorkspaceRequest{Cwd: "/repo"})

			if err == nil {
				t.Fatalf("CreateWorkspace with a %s response did not error; handle = %+v", tc.name, handle)
			}
			var protocolErr *herdr.ProtocolError
			if !errors.As(err, &protocolErr) {
				t.Fatalf("CreateWorkspace error = %v, want a ProtocolError", err)
			}
			if handle != (app.WorkspaceHandle{}) {
				t.Errorf("handle = %+v, want the zero value on error", handle)
			}
		})
	}
}

func TestRuntimeFindWorkspaceByLabel(t *testing.T) {
	t.Run("unique label found, descent through sole tab and pane", func(t *testing.T) {
		runtime, got := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{`+
			`"workspaces":[{"workspace_id":"w1","label":"other"},{"workspace_id":"w2","label":"hop-op-1"}],`+
			`"tabs":[{"tab_id":"t1","workspace_id":"w1"},{"tab_id":"t2","workspace_id":"w2"}],`+
			`"panes":[{"pane_id":"p1","tab_id":"t1"},{"pane_id":"p2","tab_id":"t2"}]}}`)

		ref, found, err := runtime.FindWorkspaceByLabel(testContext(t), "hop-op-1")
		if err != nil {
			t.Fatalf("FindWorkspaceByLabel: %v", err)
		}
		if !found {
			t.Fatal("found = false, want true")
		}
		want := app.WorkspaceRef{WorkspaceID: "w2", TabID: "t2", PaneID: "p2"}
		if ref != want {
			t.Errorf("ref = %+v, want %+v", ref, want)
		}

		assertRequestParams(t, <-got, "session.snapshot", `{}`)
	})

	t.Run("no match", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{`+
			`"workspaces":[{"workspace_id":"w1","label":"other"}],"tabs":[],"panes":[]}}`)

		ref, found, err := runtime.FindWorkspaceByLabel(testContext(t), "missing")
		if err != nil {
			t.Fatalf("FindWorkspaceByLabel: %v", err)
		}
		if found {
			t.Error("found = true, want false for a label no workspace carries")
		}
		if ref != (app.WorkspaceRef{}) {
			t.Errorf("ref = %+v, want the zero value when not found", ref)
		}
	})

	t.Run("empty label input is refused before any request is sent", func(t *testing.T) {
		runtime, got := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{"workspaces":[],"tabs":[],"panes":[]}}`)

		ref, found, err := runtime.FindWorkspaceByLabel(testContext(t), "")

		if err == nil {
			t.Fatal(`FindWorkspaceByLabel("") did not error`)
		}
		if found || ref != (app.WorkspaceRef{}) {
			t.Errorf("found = %v, ref = %+v, want the zero value and false alongside the error", found, ref)
		}
		select {
		case <-got:
			t.Error(`FindWorkspaceByLabel("") sent a request; want it refused before any request`)
		default:
		}
	})

	t.Run("no workspaces at all is a legitimate not-found", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{`+
			`"workspaces":[],"tabs":[],"panes":[]}}`)

		_, found, err := runtime.FindWorkspaceByLabel(testContext(t), "missing")
		if err != nil {
			t.Fatalf("FindWorkspaceByLabel: %v", err)
		}
		if found {
			t.Error("found = true, want false when the snapshot has no workspaces")
		}
	})

	t.Run("ambiguous label is an error", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{`+
			`"workspaces":[{"workspace_id":"w1","label":"dup"},{"workspace_id":"w2","label":"dup"}],`+
			`"tabs":[{"tab_id":"t1","workspace_id":"w1"},{"tab_id":"t2","workspace_id":"w2"}],`+
			`"panes":[{"pane_id":"p1","tab_id":"t1"},{"pane_id":"p2","tab_id":"t2"}]}}`)

		ref, found, err := runtime.FindWorkspaceByLabel(testContext(t), "dup")

		if err == nil {
			t.Fatal("FindWorkspaceByLabel with a label two workspaces carry did not error")
		}
		if found || ref != (app.WorkspaceRef{}) {
			t.Errorf("found = %v, ref = %+v, want the zero value and false alongside the error", found, ref)
		}
	})

	t.Run("more than one tab on the resolved workspace is an error", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{`+
			`"workspaces":[{"workspace_id":"w1","label":"hop-op-1"}],`+
			`"tabs":[{"tab_id":"t1","workspace_id":"w1"},{"tab_id":"t2","workspace_id":"w1"}],`+
			`"panes":[{"pane_id":"p1","tab_id":"t1"},{"pane_id":"p2","tab_id":"t2"}]}}`)

		ref, found, err := runtime.FindWorkspaceByLabel(testContext(t), "hop-op-1")

		if err == nil {
			t.Fatal("FindWorkspaceByLabel against a two-tab workspace did not error")
		}
		if found || ref != (app.WorkspaceRef{}) {
			t.Errorf("found = %v, ref = %+v, want the zero value and false alongside the error", found, ref)
		}
	})

	t.Run("zero tabs on the resolved workspace is an error, not a guess", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{`+
			`"workspaces":[{"workspace_id":"w1","label":"hop-op-1"}],"tabs":[],"panes":[]}}`)

		ref, found, err := runtime.FindWorkspaceByLabel(testContext(t), "hop-op-1")

		if err == nil {
			t.Fatal("FindWorkspaceByLabel against a tabless resolved workspace did not error")
		}
		if found || ref != (app.WorkspaceRef{}) {
			t.Errorf("found = %v, ref = %+v, want the zero value and false alongside the error", found, ref)
		}
	})

	t.Run("more than one pane on the resolved tab is an error", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{`+
			`"workspaces":[{"workspace_id":"w1","label":"hop-op-1"}],`+
			`"tabs":[{"tab_id":"t1","workspace_id":"w1"}],`+
			`"panes":[{"pane_id":"p1","tab_id":"t1"},{"pane_id":"p2","tab_id":"t1"}]}}`)

		ref, found, err := runtime.FindWorkspaceByLabel(testContext(t), "hop-op-1")

		if err == nil {
			t.Fatal("FindWorkspaceByLabel against a two-pane tab did not error")
		}
		if found || ref != (app.WorkspaceRef{}) {
			t.Errorf("found = %v, ref = %+v, want the zero value and false alongside the error", found, ref)
		}
	})

	t.Run("zero panes on the resolved tab is an error, not a guess", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{`+
			`"workspaces":[{"workspace_id":"w1","label":"hop-op-1"}],`+
			`"tabs":[{"tab_id":"t1","workspace_id":"w1"}],"panes":[]}}`)

		ref, found, err := runtime.FindWorkspaceByLabel(testContext(t), "hop-op-1")

		if err == nil {
			t.Fatal("FindWorkspaceByLabel against a paneless resolved tab did not error")
		}
		if found || ref != (app.WorkspaceRef{}) {
			t.Errorf("found = %v, ref = %+v, want the zero value and false alongside the error", found, ref)
		}
	})

	t.Run("transport error propagates", func(t *testing.T) {
		runtime := herdr.NewRuntime(deadSocket(t))

		_, found, err := runtime.FindWorkspaceByLabel(testContext(t), "any")

		if err == nil {
			t.Fatal("FindWorkspaceByLabel against a dead socket did not error")
		}
		if found {
			t.Error("found = true against a dead socket")
		}
	})

	t.Run("wrong result type", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"something_else","snapshot":{"workspaces":[],"tabs":[],"panes":[]}}`)

		_, found, err := runtime.FindWorkspaceByLabel(testContext(t), "any")

		var protocolErr *herdr.ProtocolError
		if !errors.As(err, &protocolErr) {
			t.Fatalf("FindWorkspaceByLabel error = %v, want a ProtocolError", err)
		}
		if found {
			t.Error("found = true on a result-type mismatch")
		}
	})

	t.Run("missing snapshot wrapper", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot"}`)

		_, found, err := runtime.FindWorkspaceByLabel(testContext(t), "any")

		var protocolErr *herdr.ProtocolError
		if !errors.As(err, &protocolErr) {
			t.Fatalf("FindWorkspaceByLabel error = %v, want a ProtocolError for a missing snapshot wrapper", err)
		}
		if found {
			t.Error("found = true although the snapshot wrapper is missing")
		}
	})

	t.Run("missing workspaces", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{"tabs":[],"panes":[]}}`)

		_, found, err := runtime.FindWorkspaceByLabel(testContext(t), "any")

		var protocolErr *herdr.ProtocolError
		if !errors.As(err, &protocolErr) {
			t.Fatalf("FindWorkspaceByLabel error = %v, want a ProtocolError for a missing workspaces field", err)
		}
		if found {
			t.Error("found = true although snapshot.workspaces is missing")
		}
	})

	t.Run("missing tabs", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{"workspaces":[],"panes":[]}}`)

		_, found, err := runtime.FindWorkspaceByLabel(testContext(t), "any")

		var protocolErr *herdr.ProtocolError
		if !errors.As(err, &protocolErr) {
			t.Fatalf("FindWorkspaceByLabel error = %v, want a ProtocolError for a missing tabs field", err)
		}
		if found {
			t.Error("found = true although snapshot.tabs is missing")
		}
	})

	t.Run("missing panes", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{"workspaces":[],"tabs":[]}}`)

		_, found, err := runtime.FindWorkspaceByLabel(testContext(t), "any")

		var protocolErr *herdr.ProtocolError
		if !errors.As(err, &protocolErr) {
			t.Fatalf("FindWorkspaceByLabel error = %v, want a ProtocolError for a missing panes field", err)
		}
		if found {
			t.Error("found = true although snapshot.panes is missing")
		}
	})

	t.Run("matched workspace missing required id", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{`+
			`"workspaces":[{"label":"hop-op-1"}],"tabs":[],"panes":[]}}`)

		ref, found, err := runtime.FindWorkspaceByLabel(testContext(t), "hop-op-1")

		var protocolErr *herdr.ProtocolError
		if !errors.As(err, &protocolErr) {
			t.Fatalf("FindWorkspaceByLabel error = %v, want a ProtocolError for a matched workspace missing its id", err)
		}
		if found || ref != (app.WorkspaceRef{}) {
			t.Errorf("found = %v, ref = %+v, want the zero value and false alongside the error", found, ref)
		}
	})

	t.Run("resolved tab missing required id", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{`+
			`"workspaces":[{"workspace_id":"w1","label":"hop-op-1"}],`+
			`"tabs":[{"workspace_id":"w1"}],"panes":[]}}`)

		ref, found, err := runtime.FindWorkspaceByLabel(testContext(t), "hop-op-1")

		var protocolErr *herdr.ProtocolError
		if !errors.As(err, &protocolErr) {
			t.Fatalf("FindWorkspaceByLabel error = %v, want a ProtocolError for a resolved tab missing its id", err)
		}
		if found || ref != (app.WorkspaceRef{}) {
			t.Errorf("found = %v, ref = %+v, want the zero value and false alongside the error", found, ref)
		}
	})

	t.Run("resolved pane missing required id", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{`+
			`"workspaces":[{"workspace_id":"w1","label":"hop-op-1"}],`+
			`"tabs":[{"tab_id":"t1","workspace_id":"w1"}],"panes":[{"tab_id":"t1"}]}}`)

		ref, found, err := runtime.FindWorkspaceByLabel(testContext(t), "hop-op-1")

		var protocolErr *herdr.ProtocolError
		if !errors.As(err, &protocolErr) {
			t.Fatalf("FindWorkspaceByLabel error = %v, want a ProtocolError for a resolved pane missing its id", err)
		}
		if found || ref != (app.WorkspaceRef{}) {
			t.Errorf("found = %v, ref = %+v, want the zero value and false alongside the error", found, ref)
		}
	})

	t.Run("missing label on a scanned workspace is an error, not a silent non-match", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{`+
			`"workspaces":[{"workspace_id":"w1"}],"tabs":[],"panes":[]}}`)

		ref, found, err := runtime.FindWorkspaceByLabel(testContext(t), "hop-op-1")

		var protocolErr *herdr.ProtocolError
		if !errors.As(err, &protocolErr) {
			t.Fatalf("FindWorkspaceByLabel error = %v, want a ProtocolError for a workspace missing its label", err)
		}
		if found || ref != (app.WorkspaceRef{}) {
			t.Errorf("found = %v, ref = %+v, want the zero value and false alongside the error", found, ref)
		}
	})

	t.Run("explicit empty label on a scanned workspace is an error, not a silent non-match", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{`+
			`"workspaces":[{"workspace_id":"w1","label":""}],"tabs":[],"panes":[]}}`)

		ref, found, err := runtime.FindWorkspaceByLabel(testContext(t), "hop-op-1")

		var protocolErr *herdr.ProtocolError
		if !errors.As(err, &protocolErr) {
			t.Fatalf("FindWorkspaceByLabel error = %v, want a ProtocolError for a workspace with an explicit empty label", err)
		}
		if found || ref != (app.WorkspaceRef{}) {
			t.Errorf("found = %v, ref = %+v, want the zero value and false alongside the error", found, ref)
		}
	})

	t.Run("missing workspace_id on a scanned tab is an error, not a silent non-match", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{`+
			`"workspaces":[{"workspace_id":"w1","label":"hop-op-1"}],`+
			`"tabs":[{"tab_id":"t1"}],"panes":[]}}`)

		ref, found, err := runtime.FindWorkspaceByLabel(testContext(t), "hop-op-1")

		var protocolErr *herdr.ProtocolError
		if !errors.As(err, &protocolErr) {
			t.Fatalf("FindWorkspaceByLabel error = %v, want a ProtocolError for a tab missing its workspace_id", err)
		}
		if found || ref != (app.WorkspaceRef{}) {
			t.Errorf("found = %v, ref = %+v, want the zero value and false alongside the error", found, ref)
		}
	})

	t.Run("missing tab_id on a scanned pane is an error, not a silent non-match", func(t *testing.T) {
		runtime, _ := startFakeRuntime(t, `{"type":"session_snapshot","snapshot":{`+
			`"workspaces":[{"workspace_id":"w1","label":"hop-op-1"}],`+
			`"tabs":[{"tab_id":"t1","workspace_id":"w1"}],"panes":[{"pane_id":"p1"}]}}`)

		ref, found, err := runtime.FindWorkspaceByLabel(testContext(t), "hop-op-1")

		var protocolErr *herdr.ProtocolError
		if !errors.As(err, &protocolErr) {
			t.Fatalf("FindWorkspaceByLabel error = %v, want a ProtocolError for a pane missing its tab_id", err)
		}
		if found || ref != (app.WorkspaceRef{}) {
			t.Errorf("found = %v, ref = %+v, want the zero value and false alongside the error", found, ref)
		}
	})
}
