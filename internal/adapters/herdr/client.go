// Package herdr is HOP's narrow client of the Herdr CLI and socket API. It
// speaks newline-delimited JSON over the server's local socket, correlates
// responses by request ID, maps error responses to typed errors, and probes
// the installed binary for the doctor use case. Wire shapes stay in this
// package; callers receive application values or narrow result structs.
package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
)

// maxLineBytes bounds one protocol line. Herdr responses carrying pane reads
// or plugin logs can be large, but an unbounded line would let a broken peer
// exhaust memory.
const maxLineBytes = 64 << 20

// eventBufferSize is the pushed-event channel capacity. A consumer that
// stalls longer than this backpressures the socket reader rather than
// growing without bound.
const eventBufferSize = 256

// Client issues requests against one Herdr server socket. Each call and each
// subscription uses its own connection, mirroring how the herdr CLI drives
// the socket; request IDs are still unique across the client.
type Client struct {
	socketPath string
	nextID     atomic.Uint64
}

// NewClient returns a client for the server socket at socketPath.
func NewClient(socketPath string) *Client {
	return &Client{socketPath: socketPath}
}

// request is the wire shape of one call.
type request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

// response is the wire shape of every line the server sends: a success
// response, an error response, or a pushed subscription event. Unknown
// fields are ignored so newer servers stay readable.
type response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *wireError      `json:"error"`
	Event  string          `json:"event"`
	Data   json.RawMessage `json:"data"`
}

// wireError is the error body of an error response.
type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Call sends one request and decodes the matching response's result into
// result when it is non-nil. A response with a different ID, or a frame that
// is not a response, is a ProtocolError; an error response is an APIError.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer stopOnDone(ctx, conn)()
	id := fmt.Sprintf("hop-%d", c.nextID.Add(1))
	if writeErr := writeRequest(conn, id, method, params); writeErr != nil {
		return fmt.Errorf("herdr socket %s: write %s: %w", c.socketPath, method, coalesceContextError(ctx, writeErr))
	}
	resp, err := readResponse(bufio.NewReader(conn))
	if err != nil {
		return fmt.Errorf("herdr socket %s: read %s response: %w", c.socketPath, method, coalesceContextError(ctx, err))
	}
	if err := checkResponse(resp, id); err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Result, result); err != nil {
		return &ProtocolError{Reason: fmt.Sprintf("decode %s result: %v", method, err)}
	}
	return nil
}

// EventSubscription selects one event type, optionally scoped to a pane for
// the event types Herdr requires it on.
type EventSubscription struct {
	Type   string `json:"type"`
	PaneID string `json:"pane_id,omitempty"`
}

// RawEvent is one pushed event line: the server's event name and its
// untouched payload.
type RawEvent struct {
	Name string
	Data json.RawMessage
}

// EventStream is one open events.subscribe connection. Events are delivered
// in arrival order on Events; after that channel closes, Err reports why the
// stream ended, and nil means it was closed by Close or context cancellation.
type EventStream struct {
	conn   net.Conn
	stop   func() bool
	events chan RawEvent

	mu     sync.Mutex
	closed bool
	err    error
}

// Subscribe opens a dedicated connection, registers the subscriptions and
// starts delivering pushed events after the server acknowledges the request.
// Events that arrive from acknowledgement on are buffered, so a caller can
// subscribe first, snapshot second and lose no transition in between.
func (c *Client) Subscribe(ctx context.Context, subscriptions []EventSubscription) (*EventStream, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { closeConn(conn) })
	abort := func() {
		stop()
		closeConn(conn)
	}
	id := fmt.Sprintf("hop-%d", c.nextID.Add(1))
	params := struct {
		Subscriptions []EventSubscription `json:"subscriptions"`
	}{Subscriptions: subscriptions}
	if writeErr := writeRequest(conn, id, "events.subscribe", params); writeErr != nil {
		abort()
		return nil, fmt.Errorf("herdr socket %s: write events.subscribe: %w", c.socketPath, coalesceContextError(ctx, writeErr))
	}
	reader := bufio.NewReader(conn)
	resp, err := readResponse(reader)
	if err != nil {
		abort()
		return nil, fmt.Errorf("herdr socket %s: read events.subscribe response: %w", c.socketPath, coalesceContextError(ctx, err))
	}
	if err := checkResponse(resp, id); err != nil {
		abort()
		return nil, err
	}
	stream := &EventStream{conn: conn, stop: stop, events: make(chan RawEvent, eventBufferSize)}
	go stream.read(ctx, reader)
	return stream, nil
}

// Events returns the pushed-event channel. It is closed when the stream ends.
func (s *EventStream) Events() <-chan RawEvent {
	return s.events
}

// Err reports why the stream ended. It is meaningful after Events is closed;
// nil means the stream was ended deliberately.
func (s *EventStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close ends the stream. The Events channel closes shortly after.
func (s *EventStream) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.stop()
	return s.conn.Close()
}

// read delivers pushed events until the connection ends, then records the
// reason and closes the channel. A response line on a subscription
// connection after the acknowledgement is a protocol violation.
func (s *EventStream) read(ctx context.Context, reader *bufio.Reader) {
	defer close(s.events)
	for {
		line, err := readLine(reader)
		if err != nil {
			s.finish(ctx, err)
			return
		}
		var resp response
		if err := json.Unmarshal(line, &resp); err != nil {
			s.finish(ctx, &ProtocolError{Reason: fmt.Sprintf("decode event line: %v", err)})
			return
		}
		switch {
		case resp.Event != "":
			s.events <- RawEvent{Name: resp.Event, Data: resp.Data}
		case resp.ID != "" || resp.Error != nil:
			s.finish(ctx, &ProtocolError{Reason: "unexpected response frame on a subscription connection"})
			return
		default:
			s.finish(ctx, &ProtocolError{Reason: "frame is neither a response nor an event"})
			return
		}
	}
}

// finish records why the stream ended. Errors caused by a deliberate Close
// or a canceled context are not failures.
func (s *EventStream) finish(ctx context.Context, err error) {
	closeConn(s.conn)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		s.err = nil
	case ctx.Err() != nil:
		s.err = nil
	default:
		s.err = err
	}
}

// dial opens one connection to the server socket, honoring the context for
// both the dial and, through its deadline, the following reads and writes.
func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, fmt.Errorf("herdr socket %s: %w", c.socketPath, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		if deadlineErr := conn.SetDeadline(deadline); deadlineErr != nil {
			closeConn(conn)
			return nil, fmt.Errorf("herdr socket %s: %w", c.socketPath, deadlineErr)
		}
	}
	return conn, nil
}

// stopOnDone closes conn when ctx is canceled, and returns a function that
// releases the watcher and the connection.
func stopOnDone(ctx context.Context, conn net.Conn) func() {
	stop := context.AfterFunc(ctx, func() { closeConn(conn) })
	return func() {
		stop()
		closeConn(conn)
	}
}

// closeConn releases a connection. The close result carries no signal in
// this protocol: the response line is the unit of success, and deliberate
// double closes (a context watcher racing a defer) fail by design.
func closeConn(conn net.Conn) {
	conn.Close() //nolint:errcheck,gosec // the close result carries no signal; see the function comment.
}

// coalesceContextError prefers the context's own error over the opaque
// network error produced by closing the connection on cancellation.
func coalesceContextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

// writeRequest sends one request line. A nil params still sends an empty
// object, matching the protocol examples.
func writeRequest(conn net.Conn, id, method string, params any) error {
	if params == nil {
		params = struct{}{}
	}
	line, err := json.Marshal(request{ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	_, err = conn.Write(append(line, '\n'))
	return err
}

// readResponse reads one line and decodes it as a response frame.
func readResponse(reader *bufio.Reader) (*response, error) {
	line, err := readLine(reader)
	if err != nil {
		return nil, err
	}
	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, &ProtocolError{Reason: fmt.Sprintf("decode response: %v", err)}
	}
	return &resp, nil
}

// checkResponse validates correlation and maps an error body to APIError.
// The ID is checked first so a mislabeled error cannot be attributed to the
// wrong request.
func checkResponse(resp *response, id string) error {
	if resp.ID != id {
		return &ProtocolError{Reason: fmt.Sprintf("response id %q does not match request id %q", resp.ID, id)}
	}
	if resp.Error != nil {
		return &APIError{Code: resp.Error.Code, Message: resp.Error.Message}
	}
	return nil
}

// readLine reads one newline-terminated line of at most maxLineBytes.
func readLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxLineBytes {
			return nil, &ProtocolError{Reason: fmt.Sprintf("line exceeds %d bytes", maxLineBytes)}
		}
		if err == nil {
			return line, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}
