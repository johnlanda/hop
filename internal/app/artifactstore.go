package app

import "context"

// ArtifactStore performs durable local file writes and reads under the run's
// artifact directories: the assignment artifact, periodic pane snapshots and
// check stdout/stderr. The system adapter implements it with
// temp-file-then-rename semantics on every write, so a reader never
// observes a partial file. It is never called from inside a StateStore
// transaction: a use case commits the intent that names the path and digest
// first, performs the write as the external act, then records the outcome.
type ArtifactStore interface {
	WriteArtifact(ctx context.Context, path string, content []byte) error
	ReadArtifact(ctx context.Context, path string) ([]byte, error)
}
