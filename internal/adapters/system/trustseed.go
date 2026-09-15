package system

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/johnlanda/hop/internal/app"
)

// trustSeedLockSuffix names the advisory lock file beside a profile config:
// <config>.hop-trust-seed.lock. The lock file is created on first use and
// deliberately never removed — unlinking a lock file another process may
// just have opened would let two later lockers hold different inodes at
// once, which is exactly the race the lock exists to prevent.
const trustSeedLockSuffix = ".hop-trust-seed.lock"

// trustSeedLockPoll is the retry interval while another process holds the
// advisory lock. The lock is held only for one read-modify-write, so waits
// are short; the caller's context bounds the total wait.
const trustSeedLockPoll = 10 * time.Millisecond

// TrustSeeder implements app.TrustSeeder: a serialized, atomic
// read-modify-write of a Claude Code profile's .claude.json that sets
// projects[projectKey].hasTrustDialogAccepted = true through
// app.SeedTrustEdit. Concurrent hop launch processes serialize on an
// exclusive flock of the lock file beside the config; the write is temp
// file + rename in the config's own directory at mode 0600, so a reader —
// including a concurrently starting harness — never observes a partial
// document. An absent or unparsable config is a not-seeded outcome and the
// file is not created or rewritten; every returned error carries a fixed
// category only, never the config path (which derives from environment
// values) or any file content.
type TrustSeeder struct{}

var _ app.TrustSeeder = TrustSeeder{}

// SeedWorkspaceTrust applies one workspace-trust seed to configPath under
// the advisory lock, honoring ctx while waiting for the lock. A document
// already carrying true is a seeded outcome with no write at all.
func (TrustSeeder) SeedWorkspaceTrust(ctx context.Context, configPath, projectKey string) (app.TrustSeedOutcome, error) {
	if err := ctx.Err(); err != nil {
		return app.TrustSeedOutcome{}, err
	}
	unlock, err := acquireTrustSeedLock(ctx, configPath+trustSeedLockSuffix)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The profile directory itself does not exist, so neither does
			// the config; seeding must not create a profile.
			return app.TrustSeedOutcome{Reason: "profile config absent"}, nil
		}
		return app.TrustSeedOutcome{}, err
	}
	defer unlock()

	content, err := os.ReadFile(configPath) //nolint:gosec // G304: the path is composed by the application from the launch's profile resolution; reading it is this port's purpose.
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return app.TrustSeedOutcome{Reason: "profile config absent"}, nil
	case err != nil:
		return app.TrustSeedOutcome{}, trustSeedFailure("read profile config", err)
	}
	edited, changed, err := app.SeedTrustEdit(content, projectKey)
	if err != nil {
		return app.TrustSeedOutcome{Reason: "profile config unparsable"}, nil
	}
	if !changed {
		return app.TrustSeedOutcome{Seeded: true}, nil
	}
	if err := publishTrustSeed(configPath, edited); err != nil {
		return app.TrustSeedOutcome{}, err
	}
	return app.TrustSeedOutcome{Seeded: true}, nil
}

// acquireTrustSeedLock opens (creating if needed, mode 0600) and
// exclusively flocks the advisory lock file, polling while another holder
// exists and giving up when ctx ends. The returned unlock releases the
// flock and closes the descriptor; the file itself stays.
func acquireTrustSeedLock(ctx context.Context, lockPath string) (func(), error) {
	handle, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: the lock file sits beside the application-composed config path by construction.
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("open trust-seed lock: %w", fs.ErrNotExist)
		}
		return nil, trustSeedFailure("open trust-seed lock", err)
	}
	for {
		err := syscall.Flock(int(handle.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				// Releasing is best-effort: close drops the flock with the
				// descriptor either way, and a raw close error would carry
				// the environment-derived lock path.
				_ = syscall.Flock(int(handle.Fd()), syscall.LOCK_UN) //nolint:errcheck // close below releases the lock regardless, and the error would echo the lock path.
				_ = handle.Close()                                   //nolint:errcheck // a lock-release close failure changes nothing the caller could act on, and the error would echo the lock path.
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = handle.Close() //nolint:errcheck // the lock failure is the reported error; a close error would echo the lock path.
			return nil, trustSeedFailure("lock trust-seed lock", err)
		}
		select {
		case <-ctx.Done():
			_ = handle.Close() //nolint:errcheck // the context end is the reported error; a close error would echo the lock path.
			return nil, fmt.Errorf("wait for trust-seed lock: %w", ctx.Err())
		case <-time.After(trustSeedLockPoll):
		}
	}
}

// publishTrustSeed writes content beside configPath and renames it into
// place: temp file in the same directory, mode pinned to 0600, fsynced
// before the atomic rename, directory fsynced after — the same publication
// discipline as the artifact store. Every failure path removes the temp
// file and returns a value-free category error.
func publishTrustSeed(configPath string, content []byte) error {
	dir := filepath.Dir(configPath)
	tmp, err := os.CreateTemp(dir, ".hop-trust-seed-*")
	if err != nil {
		return trustSeedFailure("create temp profile config", err)
	}
	if err := fillTemp(tmp, content); err != nil {
		_ = removeTemp(tmp.Name()) //nolint:errcheck // the write failure is the reported error; a cleanup error would echo the profile path.
		return trustSeedFailure("write temp profile config", err)
	}
	if err := os.Rename(tmp.Name(), configPath); err != nil {
		_ = removeTemp(tmp.Name()) //nolint:errcheck // the publish failure is the reported error; a cleanup error would echo the profile path.
		return trustSeedFailure("publish profile config", err)
	}
	if err := fsyncDir(dir); err != nil {
		return trustSeedFailure("fsync profile config directory", err)
	}
	return nil
}

// trustSeedFailure renders one fixed, value-free failure line: the
// operation and the error's category established through errors.Is only.
// The raw error chain carries the profile path — composed from environment
// values — so it is never wrapped or echoed.
func trustSeedFailure(op string, err error) error {
	category := "i/o failure"
	switch {
	case errors.Is(err, fs.ErrPermission):
		category = "permission denied"
	case errors.Is(err, syscall.ENOSPC):
		category = "no space left on device"
	case errors.Is(err, syscall.EROFS):
		category = "read-only file system"
	case errors.Is(err, syscall.ENOTDIR):
		category = "a path element is not a directory"
	case errors.Is(err, syscall.EISDIR):
		category = "the config path is a directory"
	}
	return fmt.Errorf("%s: %s (the profile path is never echoed)", op, category)
}
