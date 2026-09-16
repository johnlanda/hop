package system

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/johnlanda/hop/internal/app"
)

// ArtifactStore implements app.ArtifactStore with temp-file-then-rename
// writes under the run's artifact directories, so a reader never observes a
// partial artifact.
type ArtifactStore struct {
	// dirSyncer overrides the directory fsync for tests (recording the
	// synced directories or injecting failures); nil uses the real fsync.
	dirSyncer func(dir string) error
}

var _ app.ArtifactStore = ArtifactStore{}

// WriteArtifact durably writes content at path: every directory on the
// destination's parent chain is created if missing (0700) and fsynced —
// pre-existing directories included, because existence does not establish
// that an entry a previous, failed attempt created is durable — then the
// bytes land in a temp file inside the destination directory, the file is
// pinned to mode 0600 and then fsynced (data and mode inside one file
// sync), one atomic rename publishes it, and the destination directory is
// fsynced so the rename survives a crash after return. Every failure path
// removes the temp file. A failure before the rename leaves the
// destination exactly as it was; a failure after it (the directory fsync)
// leaves the complete new content whose durability is not yet established —
// the destination is never anything partial.
//
// Every failure is an artifactFailure: its text never carries a path.
func (s ArtifactStore) WriteArtifact(ctx context.Context, path string, content []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := s.establishDirDurable(dir); err != nil {
		return &artifactFailure{step: "create directory", err: err}
	}
	tmp, err := os.CreateTemp(dir, ".hop-artifact-*")
	if err != nil {
		return &artifactFailure{step: "create temp file", err: err}
	}
	if err := fillTemp(tmp, content); err != nil {
		return &artifactFailure{step: "write", err: errors.Join(err, removeTemp(tmp.Name()))}
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return &artifactFailure{step: "publish", err: errors.Join(err, removeTemp(tmp.Name()))}
	}
	if err := s.syncDirectory(dir); err != nil {
		return &artifactFailure{step: "sync directory", err: err}
	}
	return nil
}

// ReadArtifact returns the artifact's complete content. A failure is an
// artifactFailure: its text never carries the path.
func (ArtifactStore) ReadArtifact(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	content, err := os.ReadFile(path) //nolint:gosec // G304: the path is chosen by the application from the run's frozen artifact directories; reading it is this port's purpose.
	if err != nil {
		return nil, &artifactFailure{step: "read", err: err}
	}
	return content, nil
}

// errNotADirectory reports an existing non-directory entry on an
// artifact's parent chain.
var errNotADirectory = errors.New("a path element exists and is not a directory")

// artifactFailure is one failed artifact write or read, rendered without
// any path: an artifact lives under the operator's state root, and the
// application surfaces these errors on the CLI and in the operation
// journal. Error names the failed step and a fixed category established
// through errors.Is only; Unwrap keeps the chain, so callers still
// classify with errors.Is (fs.ErrNotExist, context cancellation).
type artifactFailure struct {
	step string
	err  error
}

func (f *artifactFailure) Error() string {
	return "artifact " + f.step + " failed: " + artifactFailureCategory(f.err) + " (the artifact path is never echoed)"
}

func (f *artifactFailure) Unwrap() error { return f.err }

// artifactFailureCategory classifies err through errors.Is only.
func artifactFailureCategory(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "a path element does not exist"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, errNotADirectory), errors.Is(err, syscall.ENOTDIR):
		return "a path element is not a directory"
	case errors.Is(err, syscall.EISDIR):
		return "the artifact path is a directory"
	case errors.Is(err, syscall.ENOSPC):
		return "no space left on device"
	case errors.Is(err, syscall.EROFS):
		return "read-only file system"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	default:
		return "i/o failure"
	}
}

// fillTemp writes the content, pins mode 0600 (CreateTemp's 0600 is
// subject to the umask) and only then fsyncs, so the file sync covers the
// final mode metadata as well as the data; the file is closed on every
// path.
func fillTemp(tmp *os.File, content []byte) error {
	if _, err := tmp.Write(content); err != nil {
		return errors.Join(fmt.Errorf("write artifact bytes: %w", err), tmp.Close())
	}
	if err := tmp.Chmod(0o600); err != nil {
		return errors.Join(fmt.Errorf("set artifact mode: %w", err), tmp.Close())
	}
	if err := tmp.Sync(); err != nil {
		return errors.Join(fmt.Errorf("fsync artifact: %w", err), tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close artifact: %w", err)
	}
	return nil
}

// establishDirDurable creates any missing directory on the chain down to
// dir (0700) and fsyncs the parent of every directory on that chain —
// pre-existing directories included. No caller state proves which entries
// an earlier, failed attempt left undurable (a directory's existence
// establishes nothing about its entry's durability), so every write
// re-establishes the whole chain: a retry after a failed parent sync
// re-syncs exactly the directory whose sync failed. dir itself receives
// its content sync after the publishing rename.
func (s ArtifactStore) establishDirDurable(dir string) error {
	for _, component := range parentChain(dir) {
		if err := os.Mkdir(component, 0o700); err != nil {
			if !errors.Is(err, fs.ErrExist) {
				return err
			}
			info, statErr := os.Stat(component)
			if statErr != nil {
				return statErr
			}
			if !info.IsDir() {
				return errNotADirectory
			}
		}
		if err := s.syncDirectory(filepath.Dir(component)); err != nil {
			return err
		}
	}
	return nil
}

// parentChain lists dir and its ancestors from the top down, excluding the
// terminal root ("/" or "."), which is not a creatable entry.
func parentChain(dir string) []string {
	var chain []string
	for current := filepath.Clean(dir); ; current = filepath.Dir(current) {
		if filepath.Dir(current) == current {
			break
		}
		chain = append(chain, current)
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// syncDirectory fsyncs one directory through the test seam or the real
// filesystem.
func (s ArtifactStore) syncDirectory(dir string) error {
	if s.dirSyncer != nil {
		return s.dirSyncer(dir)
	}
	return fsyncDir(dir)
}

// removeTemp deletes a temp file that will not be published; a file already
// gone is success.
func removeTemp(name string) error {
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove temp artifact: %w", err)
	}
	return nil
}

// fsyncDir fsyncs one directory so the entries it holds are durable.
func fsyncDir(dir string) error {
	handle, err := os.Open(dir) //nolint:gosec // G304: a directory on the application-chosen artifact path's chain, opened read-only for fsync alone.
	if err != nil {
		return fmt.Errorf("open artifact directory for fsync: %w", err)
	}
	if err := handle.Sync(); err != nil {
		return errors.Join(fmt.Errorf("fsync artifact directory: %w", err), handle.Close())
	}
	if err := handle.Close(); err != nil {
		return fmt.Errorf("close artifact directory: %w", err)
	}
	return nil
}
