package herdr_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnlanda/hop/internal/adapters/herdr"
	"github.com/johnlanda/hop/internal/app"
)

// deadSocket returns a socket path with no server behind it.
func deadSocket(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "dead.sock")
}

// startFakePresentation runs a fake endpoint that answers every request with
// a fixed result and records each request. The presentation adapter cannot
// take an injected client, so its methods are exercised through the socket.
// Each Call opens its own connection, so the handler answers one request per
// connection.
func startFakePresentation(t *testing.T, result string) (presentation *herdr.Presentation, requests <-chan map[string]any) {
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
	return herdr.NewPresentation(endpoint.socketPath), got
}

func paramsOf(t *testing.T, request map[string]any) map[string]any {
	t.Helper()
	params, ok := request["params"].(map[string]any)
	if !ok {
		t.Fatalf("request has no params object: %v", request)
	}
	return params
}

func TestPresentationReportMetadata(t *testing.T) {
	presentation, got := startFakePresentation(t, `{"type":"pane_metadata"}`)

	err := presentation.ReportMetadata(testContext(t), app.PaneMetadata{
		PaneID:    "w1:p1",
		Tokens:    map[string]string{"hop_role": "manager", "hop_run": "r1"},
		Clear:     []string{"hop_task", "hop_account"},
		TTLMillis: 5000,
	})
	if err != nil {
		t.Fatalf("ReportMetadata: %v", err)
	}

	request := <-got
	if request["method"] != "pane.report_metadata" {
		t.Fatalf("method = %v, want pane.report_metadata", request["method"])
	}
	params := paramsOf(t, request)
	if params["pane_id"] != "w1:p1" {
		t.Errorf("pane_id = %v, want w1:p1", params["pane_id"])
	}
	if params["source"] != app.PresentationSource {
		t.Errorf("source = %v, want %q", params["source"], app.PresentationSource)
	}
	if params["ttl_ms"] != float64(5000) {
		t.Errorf("ttl_ms = %v, want 5000", params["ttl_ms"])
	}
	tokens, ok := params["tokens"].(map[string]any)
	if !ok || tokens["hop_role"] != "manager" {
		t.Errorf("tokens = %v, want the manager role token", params["tokens"])
	}
	// Cleared tokens are sent as JSON null so Herdr removes them. In the
	// decoded request a JSON null is a Go nil value that is still present.
	for _, cleared := range []string{"hop_task", "hop_account"} {
		value, present := tokens[cleared]
		if !present || value != nil {
			t.Errorf("cleared token %q = %v (present=%t), want a null value", cleared, value, present)
		}
	}
}

func TestPresentationSelectView(t *testing.T) {
	t.Run("run filter installs a manager-first sort", func(t *testing.T) {
		presentation, got := startFakePresentation(t, `{"type":"agent_view","active":true}`)

		err := presentation.SelectView(testContext(t), app.ViewSelection{Label: "HOP r18", Run: "r18"})
		if err != nil {
			t.Fatalf("SelectView: %v", err)
		}

		request := <-got
		if request["method"] != "agent.view.set" {
			t.Fatalf("method = %v, want agent.view.set", request["method"])
		}
		raw, err := json.Marshal(request["params"])
		if err != nil {
			t.Fatal(err)
		}
		var params struct {
			Source string `json:"source"`
			Label  string `json:"label"`
			Filter struct {
				Op    string `json:"op"`
				Field struct {
					Token string `json:"token"`
				} `json:"field"`
				Value string `json:"value"`
			} `json:"filter"`
			Sort []struct {
				Field struct {
					Token string `json:"token"`
				} `json:"field"`
				Order string `json:"order"`
			} `json:"sort"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			t.Fatal(err)
		}
		if params.Source != app.PresentationSource {
			t.Errorf("source = %q, want %q", params.Source, app.PresentationSource)
		}
		if params.Filter.Op != "eq" || params.Filter.Field.Token != app.FieldRun || params.Filter.Value != "r18" {
			t.Errorf("filter = %+v, want eq on hop_run r18", params.Filter)
		}
		if len(params.Sort) != 2 || params.Sort[0].Field.Token != app.FieldRunOrder || params.Sort[1].Field.Token != app.FieldOrder {
			t.Errorf("sort = %+v, want run order then manager-first order", params.Sort)
		}
	})

	t.Run("no run omits the filter", func(t *testing.T) {
		presentation, got := startFakePresentation(t, `{"type":"agent_view","active":true}`)

		if err := presentation.SelectView(testContext(t), app.ViewSelection{Label: "all"}); err != nil {
			t.Fatalf("SelectView: %v", err)
		}

		request := <-got
		if _, present := paramsOf(t, request)["filter"]; present {
			t.Errorf("params carry a filter for an unfiltered view: %v", request["params"])
		}
	})
}

func TestPresentationClearViewIsOwned(t *testing.T) {
	presentation, got := startFakePresentation(t, `{"type":"agent_view","active":false}`)

	if err := presentation.ClearView(testContext(t)); err != nil {
		t.Fatalf("ClearView: %v", err)
	}

	request := <-got
	if request["method"] != "agent.view.clear" {
		t.Fatalf("method = %v, want agent.view.clear", request["method"])
	}
	if source := paramsOf(t, request)["source"]; source != app.PresentationSource {
		t.Errorf("clear source = %v, want %q so another owner's view stays intact", source, app.PresentationSource)
	}
}

// startFakePresentationError runs a fake endpoint that answers every
// request with the given error code and message.
func startFakePresentationError(t *testing.T, code, message string) *herdr.Presentation {
	t.Helper()
	endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
		request := readRequestLine(t, bufio.NewReader(conn))
		if request == nil {
			return
		}
		writeLine(t, conn, fmt.Sprintf(`{"id":%q,"error":{"code":%q,"message":%q}}`, requestID(t, request), code, message))
	})
	return herdr.NewPresentation(endpoint.socketPath)
}

// TestPresentationReportMetadataNotFoundClassification proves
// ReportMetadata satisfies app.ErrPaneNotFound for Herdr's pane_not_found
// answer — "pane <id> not found", the shape spike_panevanish pins against
// a real server for a vanished pane — and only for it: every other API
// error and a transport failure satisfy neither sentinel.
func TestPresentationReportMetadataNotFoundClassification(t *testing.T) {
	cases := []struct {
		name         string
		presentation func(t *testing.T) *herdr.Presentation
		wantNotFound bool
	}{
		{
			name: "pane_not_found API error",
			presentation: func(t *testing.T) *herdr.Presentation {
				return startFakePresentationError(t, "pane_not_found", "pane w2:p2 not found")
			},
			wantNotFound: true,
		},
		{
			name: "invalid_metadata_token API error",
			presentation: func(t *testing.T) *herdr.Presentation {
				return startFakePresentationError(t, "invalid_metadata_token", "token names must be at most 32 characters")
			},
		},
		{
			name:         "transport error (dead socket)",
			presentation: func(t *testing.T) *herdr.Presentation { return herdr.NewPresentation(deadSocket(t)) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.presentation(t).ReportMetadata(testContext(t), app.PaneMetadata{PaneID: "w2:p2", Tokens: map[string]string{app.FieldRun: "r1"}})

			if err == nil {
				t.Fatalf("ReportMetadata did not error for %s", tc.name)
			}
			if got := errors.Is(err, app.ErrPaneNotFound); got != tc.wantNotFound {
				t.Errorf("errors.Is(err, app.ErrPaneNotFound) = %v, want %v (err = %v)", got, tc.wantNotFound, err)
			}
			if got := errors.Is(err, herdr.ErrPaneNotFound); got != tc.wantNotFound {
				t.Errorf("errors.Is(err, herdr.ErrPaneNotFound) = %v, want %v (err = %v)", got, tc.wantNotFound, err)
			}
			if !tc.wantNotFound {
				return
			}
			if want := "report metadata for pane w2:p2: "; !strings.HasPrefix(err.Error(), want) {
				t.Errorf("error = %q, want the pane-address context %q", err, want)
			}
		})
	}
}

func TestPresentationMapsErrors(t *testing.T) {
	presentation, _ := startFakePresentation(t, `{"type":"pane_metadata"}`)
	// A server that closes without answering surfaces as an error.
	closed := herdr.NewPresentation(deadSocket(t))

	if err := closed.ReportMetadata(testContext(t), app.PaneMetadata{PaneID: "w1:p1"}); err == nil {
		t.Error("ReportMetadata against a dead socket did not error")
	}
	// The live one still works, proving the fixture is sound.
	if err := presentation.ReportMetadata(testContext(t), app.PaneMetadata{PaneID: "w1:p1"}); err != nil {
		t.Errorf("ReportMetadata against the live fake: %v", err)
	}
}
