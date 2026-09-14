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
type ArtifactStore struct{}

var _ app.ArtifactStore = ArtifactStore{}

// WriteArtifact durably writes content at path: missing parent directories
// are created (0700) with each new entry fsynced into the directory that
// holds it, the bytes land in a temp file inside the destination
// directory, the file is pinned to mode 0600 and then fsynced (data and
// mode inside one file sync), one atomic rename publishes it, and the
// destination directory is fsynced so the rename survives a crash after
// return. Every failure path removes the temp file. A failure before the
// rename leaves the destination exactly as it was; a failure after it (the
// directory fsync) leaves the complete new content whose durability is not
// yet established — the destination is never anything partial.
func (ArtifactStore) WriteArtifact(ctx context.Context, path string, content []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := ensureDirDurable(dir); err != nil {
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
	return syncDir(dir)
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

// ensureDirDurable creates any missing directories on the chain down to
// dir (0700) and fsyncs, for each directory it creates, the parent that
// received the new entry — walking from the deepest ancestor that already
// exists — so a first write into a fresh hierarchy is durably reachable
// once WriteArtifact returns, not dependent on the filesystem flushing the
// intermediate entries on its own.
func ensureDirDurable(dir string) error {
	info, err := os.Stat(dir)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", dir)
		}
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	parent := filepath.Dir(dir)
	if parent != dir {
		if err := ensureDirDurable(parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return syncDir(parent)
}

// removeTemp deletes a temp file that will not be published; a file already
// gone is success.
func removeTemp(name string) error {
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove temp artifact: %w", err)
	}
	return nil
}

// syncDir fsyncs the directory holding a just-renamed artifact so the new
// directory entry is durable.
func syncDir(dir string) error {
	handle, err := os.Open(dir) //nolint:gosec // G304: the directory of the application-chosen artifact path, opened read-only for fsync alone.
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
