package system

import (
	"bytes"
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

// trustSeedAttempts bounds the optimistic read-edit-publish rounds against
// an external writer of the same profile (a running Claude rewrites its
// config while it runs, and the advisory lock serializes HOP seeders
// only). Each round redoes the edit on freshly read content, so an
// exhausted budget means the file changed under every attempt.
const trustSeedAttempts = 3

// TrustSeeder implements app.TrustSeeder: a serialized, atomic
// read-modify-write of a Claude Code profile's .claude.json that sets
// projects[projectKey].hasTrustDialogAccepted = true through
// app.SeedTrustEdit. Concurrent hop launch processes serialize on an
// exclusive flock of the lock file beside the config; the write is temp
// file + rename in the config's own directory at mode 0600, so a reader —
// including a concurrently starting harness — never observes a partial
// document.
//
// The write is BEST-EFFORT against external writers of the same profile,
// which the lock cannot coordinate with (a running Claude of the profile
// rewrites the file from its own state). Each attempt re-stats and
// re-reads the config immediately before the publishing rename and redoes
// the edit on fresh content when anything changed, and a seeded success is
// returned only after a post-publish re-read verifies the key is present —
// so an external ATOMIC write landing before that pre-rename check is
// never lost, and one landing after the rename is detected and retried
// into. What is NOT guaranteed: an external write landing in the residual
// window between the pre-rename check and the rename itself is overwritten
// (the same last-writer-wins class of race Claude Code's own concurrent
// sessions of one profile have with each other). An absent or unparsable
// config is a not-seeded outcome and the
// file is not created or rewritten; every returned error carries a fixed
// category only, never the config path (which derives from environment
// values) or any file content.
type TrustSeeder struct {
	// beforePublishCheck, when non-nil, runs after an attempt computed its
	// edit and before the pre-rename freshness check — the test seam that
	// interleaves a controlled external writer deterministically inside
	// the covered window.
	beforePublishCheck func(attempt int)
}

var _ app.TrustSeeder = TrustSeeder{}

// SeedWorkspaceTrust applies one workspace-trust seed to configPath under
// the advisory lock, honoring ctx while waiting for the lock. A document
// already carrying true is a verified seeded outcome with no write at all;
// a config that keeps changing under an external writer for every attempt
// is a not-seeded outcome — the launch proceeds and the interactive
// fallback stays — never an error.
func (s TrustSeeder) SeedWorkspaceTrust(ctx context.Context, configPath, projectKey string) (app.TrustSeedOutcome, error) {
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

	for attempt := range trustSeedAttempts {
		outcome, retry, err := s.seedAttempt(configPath, projectKey, attempt)
		if err != nil {
			return app.TrustSeedOutcome{}, err
		}
		if !retry {
			return outcome, nil
		}
	}
	return app.TrustSeedOutcome{Reason: "profile config kept changing under an external writer"}, nil
}

// seedAttempt runs one optimistic read-edit-publish-verify round. retry
// reports that the config changed under this attempt (externally written
// content was left in place, nothing of it lost) and the caller should
// redo the edit on fresh content.
func (s TrustSeeder) seedAttempt(configPath, projectKey string, attempt int) (outcome app.TrustSeedOutcome, retry bool, err error) {
	content, err := os.ReadFile(configPath) //nolint:gosec // G304: the path is composed by the application from the launch's profile resolution; reading it is this port's purpose.
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return app.TrustSeedOutcome{Reason: "profile config absent"}, false, nil
	case err != nil:
		return app.TrustSeedOutcome{}, false, trustSeedFailure("read profile config", err)
	}
	before, err := os.Stat(configPath)
	if err != nil {
		return app.TrustSeedOutcome{}, false, trustSeedFailure("stat profile config", err)
	}
	edited, changed, err := app.SeedTrustEdit(content, projectKey)
	if err != nil {
		return app.TrustSeedOutcome{Reason: "profile config unparsable"}, false, nil
	}
	if !changed {
		return app.TrustSeedOutcome{Seeded: true}, false, nil
	}
	if s.beforePublishCheck != nil {
		s.beforePublishCheck(attempt)
	}
	published, err := publishTrustSeedIfUnchanged(configPath, edited, content, before)
	if err != nil {
		return app.TrustSeedOutcome{}, false, err
	}
	if !published {
		return app.TrustSeedOutcome{}, true, nil
	}
	// Verify the key actually reads back from the published file: an
	// external writer that won the residual post-rename window is detected
	// here and the next attempt redoes the edit on its content.
	final, err := os.ReadFile(configPath) //nolint:gosec // G304: the same application-composed config path, re-read for verification.
	if err != nil {
		return app.TrustSeedOutcome{}, false, trustSeedFailure("verify profile config", err)
	}
	if _, stillNeeded, verifyErr := app.SeedTrustEdit(final, projectKey); verifyErr == nil && !stillNeeded {
		return app.TrustSeedOutcome{Seeded: true}, false, nil
	}
	return app.TrustSeedOutcome{}, true, nil
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

// publishTrustSeedIfUnchanged writes content beside configPath and renames
// it into place — temp file in the same directory, mode pinned to 0600,
// fsynced before the atomic rename, directory fsynced after, the same
// publication discipline as the artifact store — but only after re-statting
// and re-reading the config immediately before the rename and confirming it
// still holds exactly the expected bytes the edit was computed from. A
// config that changed (or vanished) is left untouched and reported as not
// published, so the caller redoes the edit on fresh content; the residual
// window between this check and the rename is the documented best-effort
// remainder. Every failure path removes the temp file and returns a
// value-free category error.
func publishTrustSeedIfUnchanged(configPath string, content, expected []byte, expectedInfo os.FileInfo) (published bool, err error) {
	dir := filepath.Dir(configPath)
	tmp, err := os.CreateTemp(dir, ".hop-trust-seed-*")
	if err != nil {
		return false, trustSeedFailure("create temp profile config", err)
	}
	if fillErr := fillTemp(tmp, content); fillErr != nil {
		_ = removeTemp(tmp.Name()) //nolint:errcheck // the write failure is the reported error; a cleanup error would echo the profile path.
		return false, trustSeedFailure("write temp profile config", fillErr)
	}
	fresh, unchanged, err := configStillExpected(configPath, expected, expectedInfo)
	if err != nil {
		_ = removeTemp(tmp.Name()) //nolint:errcheck // the freshness failure is the reported error; a cleanup error would echo the profile path.
		return false, err
	}
	if !unchanged || !fresh {
		_ = removeTemp(tmp.Name()) //nolint:errcheck // nothing was published; a cleanup error would echo the profile path.
		return false, nil
	}
	if err := os.Rename(tmp.Name(), configPath); err != nil {
		_ = removeTemp(tmp.Name()) //nolint:errcheck // the publish failure is the reported error; a cleanup error would echo the profile path.
		return false, trustSeedFailure("publish profile config", err)
	}
	if err := fsyncDir(dir); err != nil {
		return false, trustSeedFailure("fsync profile config directory", err)
	}
	return true, nil
}

// configStillExpected re-stats and re-reads configPath and reports whether
// it still exists (fresh) and still holds exactly the expected bytes
// (unchanged): the stat comparison (size and mtime) is the cheap
// short-circuit, the byte comparison the authoritative check. A vanished
// config is fresh=false with no error — the caller's next attempt observes
// the absence.
func configStillExpected(configPath string, expected []byte, expectedInfo os.FileInfo) (fresh, unchanged bool, err error) {
	info, err := os.Stat(configPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, false, nil
	case err != nil:
		return false, false, trustSeedFailure("stat profile config", err)
	}
	if info.Size() != expectedInfo.Size() || !info.ModTime().Equal(expectedInfo.ModTime()) {
		return true, false, nil
	}
	current, err := os.ReadFile(configPath) //nolint:gosec // G304: the same application-composed config path, re-read for the freshness check.
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, false, nil
	case err != nil:
		return false, false, trustSeedFailure("re-read profile config", err)
	}
	return true, bytes.Equal(current, expected), nil
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
