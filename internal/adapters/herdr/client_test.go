package herdr_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/herdr"
)

// protocolTimeout bounds every fake-endpoint exchange. The fake answers
// immediately, so the deadline exists only to fail hung tests.
const protocolTimeout = 10 * time.Second

// fakeEndpoint is a scripted NDJSON server on a real Unix socket. Each
// accepted connection is handed to the handler on its own goroutine.
type fakeEndpoint struct {
	socketPath string
	listener   net.Listener
}

// startFakeEndpoint listens on a socket under a short temporary directory;
// t.TempDir is avoided because macOS caps Unix socket paths at 104 bytes.
func startFakeEndpoint(t *testing.T, handler func(t *testing.T, conn net.Conn)) *fakeEndpoint {
	t.Helper()
	dir, err := os.MkdirTemp("", "hop")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			t.Errorf("remove fake endpoint dir: %v", removeErr)
		}
	})
	socketPath := filepath.Join(dir, "herdr.sock")
	var config net.ListenConfig
	listener, err := config.Listen(context.Background(), "unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeQuietly(listener) })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer closeQuietly(conn)
				handler(t, conn)
			}()
		}
	}()
	return &fakeEndpoint{socketPath: socketPath, listener: listener}
}

// closeQuietly releases a test resource whose close result carries no
// signal, such as a connection the peer may already have closed.
func closeQuietly(c io.Closer) {
	c.Close() //nolint:errcheck,gosec // the close result carries no signal in these tests.
}

// requestID extracts the request's id and reports a missing one as a test
// failure at the fake endpoint.
func requestID(t *testing.T, request map[string]any) string {
	t.Helper()
	id, ok := request["id"].(string)
	if !ok {
		t.Error("fake endpoint: request has no string id")
	}
	return id
}

// drainEvents consumes a stream until it closes and returns what it carried.
func drainEvents(stream *herdr.EventStream) []herdr.RawEvent {
	var events []herdr.RawEvent
	for event := range stream.Events() {
		events = append(events, event)
	}
	return events
}

// holdUntilPeerCloses blocks until the peer closes the connection, keeping
// the fake endpoint's side open without responding.
func holdUntilPeerCloses(conn net.Conn) {
	buffer := make([]byte, 1)
	for {
		if _, err := conn.Read(buffer); err != nil {
			return
		}
	}
}

// readRequestLine decodes the next request the client sent.
func readRequestLine(t *testing.T, reader *bufio.Reader) map[string]any {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Errorf("fake endpoint: read request: %v", err)
		return nil
	}
	var request map[string]any
	if err := json.Unmarshal([]byte(line), &request); err != nil {
		t.Errorf("fake endpoint: decode request %q: %v", line, err)
		return nil
	}
	return request
}

// writeLine writes one raw protocol line.
func writeLine(t *testing.T, conn net.Conn, line string) {
	t.Helper()
	if _, err := conn.Write([]byte(line + "\n")); err != nil {
		t.Errorf("fake endpoint: write line: %v", err)
	}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), protocolTimeout)
	t.Cleanup(cancel)
	return ctx
}

func TestClientCall(t *testing.T) {
	cases := []struct {
		name string
		// respond builds the wire response for the received request id.
		respond func(id string) []string
		// check inspects the call outcome.
		check func(t *testing.T, result map[string]any, err error)
	}{
		{
			name: "matched response decodes the result",
			respond: func(id string) []string {
				return []string{fmt.Sprintf(`{"id":%q,"result":{"type":"pong","version":"0.9.0","protocol":22}}`, id)}
			},
			check: func(t *testing.T, result map[string]any, err error) {
				if err != nil {
					t.Fatalf("Call: %v", err)
				}
				if result["version"] != "0.9.0" {
					t.Errorf("result = %v, want version 0.9.0", result)
				}
			},
		},
		{
			name: "unknown optional fields are ignored",
			respond: func(id string) []string {
				return []string{fmt.Sprintf(`{"id":%q,"future_field":[1,2],"result":{"type":"pong","version":"0.9.0","later":{"x":1}}}`, id)}
			},
			check: func(t *testing.T, _ map[string]any, err error) {
				if err != nil {
					t.Fatalf("Call: %v", err)
				}
			},
		},
		{
			name: "mismatched response id is a protocol error",
			respond: func(string) []string {
				return []string{`{"id":"someone-else","result":{"type":"pong"}}`}
			},
			check: func(t *testing.T, _ map[string]any, err error) {
				var protocolErr *herdr.ProtocolError
				if !errors.As(err, &protocolErr) {
					t.Fatalf("Call error = %v, want a ProtocolError", err)
				}
				if !strings.Contains(protocolErr.Reason, "does not match") {
					t.Errorf("reason = %q, want an id mismatch", protocolErr.Reason)
				}
			},
		},
		{
			name: "error body maps to APIError with code and message",
			respond: func(id string) []string {
				return []string{fmt.Sprintf(`{"id":%q,"error":{"code":"not_found","message":"pane not found"}}`, id)}
			},
			check: func(t *testing.T, _ map[string]any, err error) {
				var apiErr *herdr.APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("Call error = %v, want an APIError", err)
				}
				if apiErr.Code != "not_found" || apiErr.Message != "pane not found" {
					t.Errorf("APIError = %+v, want not_found / pane not found", apiErr)
				}
			},
		},
		{
			name: "malformed response line is a protocol error",
			respond: func(string) []string {
				return []string{`{"id":`}
			},
			check: func(t *testing.T, _ map[string]any, err error) {
				var protocolErr *herdr.ProtocolError
				if !errors.As(err, &protocolErr) {
					t.Fatalf("Call error = %v, want a ProtocolError", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
				request := readRequestLine(t, bufio.NewReader(conn))
				if request == nil {
					return
				}
				id, ok := request["id"].(string)
				if !ok {
					t.Error("fake endpoint: request has no string id")
					return
				}
				for _, line := range tc.respond(id) {
					writeLine(t, conn, line)
				}
			})
			client := herdr.NewClient(endpoint.socketPath)
			var result map[string]any

			err := client.Call(testContext(t), "ping", nil, &result)

			tc.check(t, result, err)
		})
	}
}

func TestClientCallSendsMethodParamsAndUniqueIDs(t *testing.T) {
	type seen struct {
		id     string
		method string
		params map[string]any
	}
	requests := make(chan seen, 2)
	endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
		reader := bufio.NewReader(conn)
		request := readRequestLine(t, reader)
		if request == nil {
			return
		}
		id := requestID(t, request)
		method, ok := request["method"].(string)
		if !ok {
			t.Error("fake endpoint: request has no string method")
		}
		params, ok := request["params"].(map[string]any)
		if !ok {
			t.Error("fake endpoint: request has no params object")
		}
		requests <- seen{id: id, method: method, params: params}
		writeLine(t, conn, fmt.Sprintf(`{"id":%q,"result":{"type":"ok"}}`, id))
	})
	client := herdr.NewClient(endpoint.socketPath)
	ctx := testContext(t)

	if err := client.Call(ctx, "pane.read", map[string]string{"pane_id": "w1:p1"}, nil); err != nil {
		t.Fatalf("first Call: %v", err)
	}
	if err := client.Call(ctx, "ping", nil, nil); err != nil {
		t.Fatalf("second Call: %v", err)
	}

	first, second := <-requests, <-requests
	if first.method != "pane.read" || first.params["pane_id"] != "w1:p1" {
		t.Errorf("first request = %+v, want pane.read with pane_id w1:p1", first)
	}
	if second.method != "ping" {
		t.Errorf("second request method = %q, want ping", second.method)
	}
	if second.params == nil {
		t.Error("nil params must still be sent as an empty object")
	}
	if first.id == second.id || first.id == "" {
		t.Errorf("request ids %q and %q must be unique and non-empty", first.id, second.id)
	}
}

func TestClientCallReassemblesPartialFrames(t *testing.T) {
	endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
		request := readRequestLine(t, bufio.NewReader(conn))
		if request == nil {
			return
		}
		id := requestID(t, request)
		frame := fmt.Sprintf(`{"id":%q,"result":{"type":"pong","version":"0.9.0"}}`+"\n", id)
		for _, chunk := range []string{frame[:7], frame[7:19], frame[19:]} {
			if _, err := conn.Write([]byte(chunk)); err != nil {
				t.Errorf("fake endpoint: write chunk: %v", err)
				return
			}
		}
	})
	client := herdr.NewClient(endpoint.socketPath)
	var result struct {
		Version string `json:"version"`
	}

	if err := client.Call(testContext(t), "ping", nil, &result); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.Version != "0.9.0" {
		t.Errorf("version = %q, want 0.9.0", result.Version)
	}
}

func TestClientCallReportsDisconnects(t *testing.T) {
	endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
		request := readRequestLine(t, bufio.NewReader(conn))
		if request == nil {
			return
		}
		if _, err := conn.Write([]byte(`{"id":`)); err != nil {
			t.Errorf("fake endpoint: write partial line: %v", err)
		}
		// Returning closes the connection mid-line.
	})
	client := herdr.NewClient(endpoint.socketPath)

	err := client.Call(testContext(t), "ping", nil, nil)

	if err == nil {
		t.Fatal("Call succeeded although the server disconnected mid-response")
	}
	var protocolErr *herdr.ProtocolError
	var apiErr *herdr.APIError
	if errors.As(err, &protocolErr) || errors.As(err, &apiErr) {
		t.Fatalf("Call error = %v, want a plain transport error", err)
	}
}

func TestClientCallHonorsCancellation(t *testing.T) {
	blocked := make(chan struct{})
	endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
		_ = readRequestLine(t, bufio.NewReader(conn))
		<-blocked // Never respond until the test ends.
	})
	t.Cleanup(func() { close(blocked) })
	client := herdr.NewClient(endpoint.socketPath)
	ctx, cancel := context.WithCancel(testContext(t))
	done := make(chan error, 1)
	go func() { done <- client.Call(ctx, "ping", nil, nil) }()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Call error = %v, want context.Canceled", err)
		}
	case <-time.After(protocolTimeout):
		t.Fatal("Call did not return after cancellation")
	}
}

func TestClientSubscribe(t *testing.T) {
	t.Run("acknowledged stream delivers interleaved events in order", func(t *testing.T) {
		endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
			reader := bufio.NewReader(conn)
			request := readRequestLine(t, reader)
			if request == nil {
				return
			}
			id := requestID(t, request)
			// The acknowledgement and the first event share one write so
			// the client must split frames by newline, not by read.
			writeLine(t, conn, fmt.Sprintf(`{"id":%q,"result":{"type":"subscription_started"}}`, id)+"\n"+
				`{"event":"pane_agent_status_changed","data":{"type":"pane_agent_status_changed","pane_id":"w1:p1","workspace_id":"w1","agent_status":"working","future":true}}`)
			writeLine(t, conn, `{"event":"pane_exited","data":{"type":"pane_exited","pane_id":"w1:p1","workspace_id":"w1"}}`)
			// Second request on another connection would interleave here in
			// real use; this connection only streams events.
		})
		client := herdr.NewClient(endpoint.socketPath)

		stream, err := client.Subscribe(testContext(t), []herdr.EventSubscription{{Type: "pane.exited"}})
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer closeQuietly(stream)

		first := <-stream.Events()
		if first.Name != "pane_agent_status_changed" {
			t.Fatalf("first event = %q, want pane_agent_status_changed", first.Name)
		}
		var payload struct {
			PaneID      string `json:"pane_id"`
			AgentStatus string `json:"agent_status"`
		}
		if err := json.Unmarshal(first.Data, &payload); err != nil {
			t.Fatalf("decode event data: %v", err)
		}
		if payload.PaneID != "w1:p1" || payload.AgentStatus != "working" {
			t.Errorf("payload = %+v, want w1:p1 working", payload)
		}
		second := <-stream.Events()
		if second.Name != "pane_exited" {
			t.Fatalf("second event = %q, want pane_exited", second.Name)
		}
	})

	t.Run("subscription request is sent verbatim", func(t *testing.T) {
		got := make(chan map[string]any, 1)
		endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
			request := readRequestLine(t, bufio.NewReader(conn))
			if request == nil {
				return
			}
			got <- request
			id := requestID(t, request)
			writeLine(t, conn, fmt.Sprintf(`{"id":%q,"result":{"type":"subscription_started"}}`, id))
		})
		client := herdr.NewClient(endpoint.socketPath)

		stream, err := client.Subscribe(testContext(t), []herdr.EventSubscription{
			{Type: "pane.agent_status_changed", PaneID: "w1:p2"},
			{Type: "pane.created"},
		})
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer closeQuietly(stream)

		request := <-got
		if request["method"] != "events.subscribe" {
			t.Errorf("method = %v, want events.subscribe", request["method"])
		}
		raw, err := json.Marshal(request["params"])
		if err != nil {
			t.Fatal(err)
		}
		var params struct {
			Subscriptions []map[string]string `json:"subscriptions"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			t.Fatal(err)
		}
		want := []map[string]string{
			{"type": "pane.agent_status_changed", "pane_id": "w1:p2"},
			{"type": "pane.created"},
		}
		if len(params.Subscriptions) != len(want) {
			t.Fatalf("subscriptions = %v, want %v", params.Subscriptions, want)
		}
		for i, sub := range want {
			for key, value := range sub {
				if params.Subscriptions[i][key] != value {
					t.Errorf("subscription %d %s = %q, want %q", i, key, params.Subscriptions[i][key], value)
				}
			}
			if _, ok := params.Subscriptions[i]["pane_id"]; ok && sub["pane_id"] == "" {
				t.Errorf("subscription %d must omit pane_id when unset", i)
			}
		}
	})

	t.Run("error response fails the subscribe", func(t *testing.T) {
		endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
			request := readRequestLine(t, bufio.NewReader(conn))
			if request == nil {
				return
			}
			id := requestID(t, request)
			writeLine(t, conn, fmt.Sprintf(`{"id":%q,"error":{"code":"invalid_params","message":"bad subscription"}}`, id))
		})
		client := herdr.NewClient(endpoint.socketPath)

		_, err := client.Subscribe(testContext(t), nil)

		var apiErr *herdr.APIError
		if !errors.As(err, &apiErr) || apiErr.Code != "invalid_params" {
			t.Fatalf("Subscribe error = %v, want APIError invalid_params", err)
		}
	})

	t.Run("response frame after the acknowledgement ends the stream", func(t *testing.T) {
		endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
			request := readRequestLine(t, bufio.NewReader(conn))
			if request == nil {
				return
			}
			id := requestID(t, request)
			writeLine(t, conn, fmt.Sprintf(`{"id":%q,"result":{"type":"subscription_started"}}`, id))
			writeLine(t, conn, `{"event":"pane_created","data":{"type":"pane_created"}}`)
			writeLine(t, conn, `{"id":"stray","result":{"type":"ok"}}`)
		})
		client := herdr.NewClient(endpoint.socketPath)

		stream, err := client.Subscribe(testContext(t), nil)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer closeQuietly(stream)

		events := drainEvents(stream)
		if len(events) != 1 || events[0].Name != "pane_created" {
			t.Fatalf("events = %+v, want exactly pane_created", events)
		}
		var protocolErr *herdr.ProtocolError
		if !errors.As(stream.Err(), &protocolErr) {
			t.Fatalf("stream.Err() = %v, want a ProtocolError", stream.Err())
		}
	})

	t.Run("server disconnect surfaces as a stream error", func(t *testing.T) {
		endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
			request := readRequestLine(t, bufio.NewReader(conn))
			if request == nil {
				return
			}
			id := requestID(t, request)
			writeLine(t, conn, fmt.Sprintf(`{"id":%q,"result":{"type":"subscription_started"}}`, id))
			// Returning closes the connection.
		})
		client := herdr.NewClient(endpoint.socketPath)

		stream, err := client.Subscribe(testContext(t), nil)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}

		drainEvents(stream)
		if stream.Err() == nil {
			t.Fatal("stream.Err() = nil, want the disconnect reported")
		}
	})

	t.Run("close ends the stream without an error", func(t *testing.T) {
		endpoint := startFakeEndpoint(t, func(t *testing.T, conn net.Conn) {
			request := readRequestLine(t, bufio.NewReader(conn))
			if request == nil {
				return
			}
			id := requestID(t, request)
			writeLine(t, conn, fmt.Sprintf(`{"id":%q,"result":{"type":"subscription_started"}}`, id))
			holdUntilPeerCloses(conn)
		})
		client := herdr.NewClient(endpoint.socketPath)

		stream, err := client.Subscribe(testContext(t), nil)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		if err := stream.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		drainEvents(stream)
		if err := stream.Err(); err != nil {
			t.Fatalf("stream.Err() = %v, want nil after a deliberate close", err)
		}
	})
}
