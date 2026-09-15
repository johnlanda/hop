package system_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/johnlanda/hop/internal/adapters/system"
)

const seededWorktree = "/private/var/worktrees/hop-run-1"

// writeConfig writes a fixture .claude.json and returns its path.
func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertNoTempResidue fails when a temp seed file survived in dir.
func assertNoTempResidue(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".hop-trust-seed-") {
			t.Errorf("temp seed file left behind: %s", entry.Name())
		}
	}
}

// readBack reads a test-constructed path.
func readBack(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path) //nolint:gosec // G304: a path this test constructed itself under t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func TestTrustSeederSeedsAndPreservesEverythingElse(t *testing.T) {
	dir := t.TempDir()
	config := writeConfig(t, dir, `{"numStartups":7,"projects":{"/other":{"hasTrustDialogAccepted":false}},"unknownKey":[1,2]}`)

	outcome, err := system.TrustSeeder{}.SeedWorkspaceTrust(context.Background(), config, seededWorktree)
	if err != nil {
		t.Fatalf("SeedWorkspaceTrust: %v", err)
	}
	if !outcome.Seeded {
		t.Fatalf("outcome = %+v, want seeded", outcome)
	}

	content := readBack(t, config)
	want := `{"numStartups":7,"projects":{"` + seededWorktree + `":{"hasTrustDialogAccepted":true},"/other":{"hasTrustDialogAccepted":false}},"unknownKey":[1,2]}`
	if content != want {
		t.Errorf("config:\n%s\nwant:\n%s", content, want)
	}
	info, err := os.Stat(config)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %o, want 0600", info.Mode().Perm())
	}
	assertNoTempResidue(t, dir)
	if _, err := os.Stat(config + ".hop-trust-seed.lock"); err != nil {
		t.Errorf("advisory lock file missing beside the config: %v", err)
	}
}

func TestTrustSeederOverwritesFalse(t *testing.T) {
	dir := t.TempDir()
	config := writeConfig(t, dir, `{"projects":{"`+seededWorktree+`":{"hasTrustDialogAccepted":false,"allowedTools":[]}}}`)

	outcome, err := system.TrustSeeder{}.SeedWorkspaceTrust(context.Background(), config, seededWorktree)
	if err != nil || !outcome.Seeded {
		t.Fatalf("SeedWorkspaceTrust = %+v, %v", outcome, err)
	}

	content := readBack(t, config)
	want := `{"projects":{"` + seededWorktree + `":{"hasTrustDialogAccepted":true,"allowedTools":[]}}}`
	if content != want {
		t.Errorf("config:\n%s\nwant:\n%s", content, want)
	}
}

func TestTrustSeederAlreadyTrueWritesNothing(t *testing.T) {
	dir := t.TempDir()
	original := `{ "projects" : { "` + seededWorktree + `" : { "hasTrustDialogAccepted" : true } } }`
	config := writeConfig(t, dir, original)
	before, err := os.Stat(config)
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := system.TrustSeeder{}.SeedWorkspaceTrust(context.Background(), config, seededWorktree)
	if err != nil || !outcome.Seeded {
		t.Fatalf("SeedWorkspaceTrust = %+v, %v", outcome, err)
	}

	content := readBack(t, config)
	if content != original {
		t.Errorf("an already-true document was rewritten:\n%s", content)
	}
	after, err := os.Stat(config)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("an already-true document was republished (mtime moved)")
	}
}

func TestTrustSeederAbsentConfig(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, ".claude.json")

	outcome, err := system.TrustSeeder{}.SeedWorkspaceTrust(context.Background(), config, seededWorktree)
	if err != nil {
		t.Fatalf("SeedWorkspaceTrust: %v", err)
	}
	if outcome.Seeded || outcome.Reason != "profile config absent" {
		t.Fatalf("outcome = %+v, want the absent reason", outcome)
	}
	if _, err := os.Stat(config); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an absent config was created")
	}
}

func TestTrustSeederAbsentProfileDirectory(t *testing.T) {
	config := filepath.Join(t.TempDir(), "no-such-profile", ".claude.json")

	outcome, err := system.TrustSeeder{}.SeedWorkspaceTrust(context.Background(), config, seededWorktree)
	if err != nil {
		t.Fatalf("SeedWorkspaceTrust: %v", err)
	}
	if outcome.Seeded || outcome.Reason != "profile config absent" {
		t.Fatalf("outcome = %+v, want the absent reason", outcome)
	}
	if _, err := os.Stat(filepath.Dir(config)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an absent profile directory was created")
	}
}

func TestTrustSeederMalformedConfigUntouched(t *testing.T) {
	dir := t.TempDir()
	original := `{"projects": truncated`
	config := writeConfig(t, dir, original)

	outcome, err := system.TrustSeeder{}.SeedWorkspaceTrust(context.Background(), config, seededWorktree)
	if err != nil {
		t.Fatalf("SeedWorkspaceTrust: %v", err)
	}
	if outcome.Seeded || outcome.Reason != "profile config unparsable" {
		t.Fatalf("outcome = %+v, want the unparsable reason", outcome)
	}
	content := readBack(t, config)
	if content != original {
		t.Errorf("a malformed config was rewritten:\n%s", content)
	}
	assertNoTempResidue(t, dir)
}

// TestTrustSeederResolvedSymlinkPathKey proves the key lands verbatim even
// for the macOS /var-style resolved paths: a config addressed through a
// symlinked directory is seeded at its resolved location with the exact
// caller-supplied key.
func TestTrustSeederResolvedSymlinkPathKey(t *testing.T) {
	realDir := t.TempDir()
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "profile-link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeConfig(t, realDir, `{}`)

	outcome, err := system.TrustSeeder{}.SeedWorkspaceTrust(context.Background(), filepath.Join(link, ".claude.json"), seededWorktree)
	if err != nil || !outcome.Seeded {
		t.Fatalf("SeedWorkspaceTrust = %+v, %v", outcome, err)
	}

	content := readBack(t, filepath.Join(realDir, ".claude.json"))
	var doc map[string]map[string]map[string]bool
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("seeded config is not valid JSON: %v", err)
	}
	if !doc["projects"][seededWorktree]["hasTrustDialogAccepted"] {
		t.Errorf("seeded config = %s, want the exact worktree key", content)
	}
}

// TestTrustSeederSerializesConcurrentSeeds proves the advisory lock makes
// concurrent read-modify-writes lose nothing: every goroutine's distinct
// project key survives in the final document.
func TestTrustSeederSerializesConcurrentSeeds(t *testing.T) {
	dir := t.TempDir()
	config := writeConfig(t, dir, `{"projects":{}}`)

	const writers = 8
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("/private/var/worktrees/hop-run-%d", i)
			outcome, err := system.TrustSeeder{}.SeedWorkspaceTrust(context.Background(), config, key)
			if err == nil && !outcome.Seeded {
				err = fmt.Errorf("outcome = %+v, want seeded", outcome)
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}

	content := readBack(t, config)
	var doc struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("final config is not valid JSON: %v", err)
	}
	for i := range writers {
		key := fmt.Sprintf("/private/var/worktrees/hop-run-%d", i)
		if !doc.Projects[key].HasTrustDialogAccepted {
			t.Errorf("key %s lost by a concurrent read-modify-write", key)
		}
	}
}

// TestTrustSeederBlocksOnHeldLock proves a held lock blocks the seed until
// the caller's context ends, and a released lock lets it proceed — the
// process-level serialization contract, exercised through a separate open
// of the same lock file (flock scopes to the open file description, so a
// second descriptor contends exactly as a second process would).
func TestTrustSeederBlocksOnHeldLock(t *testing.T) {
	dir := t.TempDir()
	config := writeConfig(t, dir, `{}`)
	lock, err := os.OpenFile(config+".hop-trust-seed.lock", os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: a path this test constructed itself under t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close() //nolint:errcheck // best-effort release of a test-held lock.
	if flockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); flockErr != nil {
		t.Fatal(flockErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = system.TrustSeeder{}.SeedWorkspaceTrust(ctx, config, seededWorktree)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context deadline while the lock is held", err)
	}
	content := readBack(t, config)
	if content != `{}` {
		t.Errorf("a blocked seed still wrote: %s", content)
	}

	if flockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); flockErr != nil {
		t.Fatal(flockErr)
	}
	outcome, err := system.TrustSeeder{}.SeedWorkspaceTrust(context.Background(), config, seededWorktree)
	if err != nil || !outcome.Seeded {
		t.Fatalf("SeedWorkspaceTrust after release = %+v, %v", outcome, err)
	}
}

// TestTrustSeederFailureIsValueFree proves a hard failure names a category
// only: neither the profile path nor any content appears in the error.
func TestTrustSeederFailureIsValueFree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission failures cannot be provoked")
	}
	dir := t.TempDir()
	config := writeConfig(t, dir, `{"projects":{}}`)
	// A read-only directory refuses both the temp file and the rename.
	if chmodErr := os.Chmod(dir, 0o500); chmodErr != nil { //nolint:gosec // G302: a read-only directory is the provoked failure.
		t.Fatal(chmodErr)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:errcheck,gosec // G302: best-effort restore of a directory mode so t.TempDir cleanup succeeds.

	// The lock file cannot be created either; that is already the failure.
	_, err := system.TrustSeeder{}.SeedWorkspaceTrust(context.Background(), config, seededWorktree)
	if err == nil {
		t.Fatal("a read-only profile directory did not fail the seed")
	}
	if strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), seededWorktree) {
		t.Errorf("error echoes a path: %v", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error lacks the category: %v", err)
	}
}

// TestTrustSeederWriteFailureIsValueFree provokes the failure after the
// lock: the lock file exists and is lockable, but the directory refuses
// the temp file.
func TestTrustSeederWriteFailureIsValueFree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission failures cannot be provoked")
	}
	dir := t.TempDir()
	config := writeConfig(t, dir, `{"projects":{}}`)
	lock, err := os.OpenFile(config+".hop-trust-seed.lock", os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: a path this test constructed itself under t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := lock.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if chmodErr := os.Chmod(dir, 0o500); chmodErr != nil { //nolint:gosec // G302: a read-only directory is the provoked failure.
		t.Fatal(chmodErr)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:errcheck,gosec // G302: best-effort restore of a directory mode so t.TempDir cleanup succeeds.

	_, err = system.TrustSeeder{}.SeedWorkspaceTrust(context.Background(), config, seededWorktree)
	if err == nil {
		t.Fatal("an unwritable profile directory did not fail the seed")
	}
	if strings.Contains(err.Error(), dir) {
		t.Errorf("error echoes the profile path: %v", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error lacks the category: %v", err)
	}
	if chmodErr := os.Chmod(dir, 0o700); chmodErr != nil { //nolint:gosec // G302: restoring the directory mode this test lowered.
		t.Fatal(chmodErr)
	}
	assertNoTempResidue(t, dir)
	content := readBack(t, config)
	if content != `{"projects":{}}` {
		t.Errorf("a failed publish altered the config: %s", content)
	}
}

func TestTrustSeederCanceledContext(t *testing.T) {
	dir := t.TempDir()
	config := writeConfig(t, dir, `{}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := (system.TrustSeeder{}).SeedWorkspaceTrust(ctx, config, seededWorktree); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	content := readBack(t, config)
	if content != `{}` {
		t.Errorf("a canceled seed still wrote: %s", content)
	}
}
