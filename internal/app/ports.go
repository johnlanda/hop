// Package app coordinates HOP's use cases through consumer-owned ports.
// Ports are declared here, next to the services that consume them; adapters
// implement them and only the composition root or a test wires the two sides
// together. This slice carries the doctor and plugin-invocation use cases.
package app

import "context"

// BinaryInfo identifies one locally installed executable.
type BinaryInfo struct {
	// Path is the absolute path the executable resolved to.
	Path string
	// Version is the first line the executable printed for --version,
	// or "" when the version could not be read.
	Version string
}

// SchemaInfo summarizes the socket API schema bundled with a herdr binary.
type SchemaInfo struct {
	// Protocol is the schema document's protocol number.
	Protocol int
	// Methods are the request method names the schema advertises, sorted.
	Methods []string
}

// ServerInfo identifies a running herdr server reached over its socket.
type ServerInfo struct {
	// Version is the server's reported release version.
	Version string
	// Protocol is the server's reported protocol number.
	Protocol int
}

// Probe inspects the local Herdr installation for the doctor use case. Every
// method reports what it observed and returns an error when the observation
// itself could not be made, so the doctor reports facts rather than guesses.
type Probe interface {
	// Binary locates the herdr executable and reads its version.
	Binary(ctx context.Context) (BinaryInfo, error)
	// Schema reads the socket API schema bundled with the herdr binary.
	Schema(ctx context.Context) (SchemaInfo, error)
	// Ping checks the configured server socket and reads its identity.
	Ping(ctx context.Context) (ServerInfo, error)
	// Harness locates a native agent harness executable by name and reads
	// its version.
	Harness(ctx context.Context, executable string) (BinaryInfo, error)
}
