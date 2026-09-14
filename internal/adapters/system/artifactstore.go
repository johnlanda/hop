package system

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

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
func (s ArtifactStore) WriteArtifact(ctx context.Context, path string, content []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := s.establishDirDurable(dir); err != nil {
		return fmt.Errorf("create artifact directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".hop-artifact-*")
	if err != nil {
		return fmt.Errorf("create temp artifact file: %w", err)
	}
	if err := fillTemp(tmp, content); err != nil {
		return errors.Join(err, removeTemp(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return errors.Join(fmt.Errorf("publish artifact: %w", err), removeTemp(tmp.Name()))
	}
	return s.syncDirectory(dir)
}

// ReadArtifact returns the artifact's complete content.
func (ArtifactStore) ReadArtifact(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	content, err := os.ReadFile(path) //nolint:gosec // G304: the path is chosen by the application from the run's frozen artifact directories; reading it is this port's purpose.
	if err != nil {
		return nil, fmt.Errorf("read artifact: %w", err)
	}
	return content, nil
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
				return fmt.Errorf("%s exists and is not a directory", component)
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
