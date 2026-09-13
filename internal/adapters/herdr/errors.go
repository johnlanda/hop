package herdr

import "fmt"

// APIError is an error response from the Herdr server, preserved with the
// code and message the server sent.
type APIError struct {
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("herdr: %s: %s", e.Code, e.Message)
}

// ProtocolError reports a line that violates the newline-delimited JSON
// protocol: a response whose ID does not match the request, a frame that is
// neither a response nor an event, or malformed JSON.
type ProtocolError struct {
	Reason string
}

func (e *ProtocolError) Error() string {
	return "herdr protocol: " + e.Reason
}
